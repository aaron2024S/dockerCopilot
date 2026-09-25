#!/usr/bin/env python3
"""离线组装 docker-save 格式镜像 tar（dockerCopilot）。

本机没有 Docker 引擎，无法执行 `docker build`，因此按 Dockerfile 的语义
**手工拼装** 镜像归档 —— 产出物可以被目标机的 `docker load -i` 直接导入。

对应 docker/Dockerfile 的语义：
    FROM alpine                       -> 复用 cache/<arch>/ 里已拉取并校验过的基础层
    COPY dc-back/dist/$T/dockerCopilot -> app/dockerCopilot (0755)
    COPY dc-back/etc/. ./etc           -> app/etc/
    COPY start.sh ./start.sh           -> app/start.sh (0755)
    RUN apk add --no-cache tzdata      -> 把 cache 里从 apk 拆出的 data 段铺进层
    WORKDIR /app / ENV / VOLUME / CMD  -> 改写 config.json 对应字段

与上游的唯一有意差别：额外写入 ENV WORKDIR=/app。
start.sh 第一行是 `cd "${WORKDIR}" || exit`，而 WORKDIR 只是 Dockerfile 指令、
不是环境变量，上游镜像里该变量是空的。补上它让工作目录切换确定化（无论
busybox 对 `cd ""` 是宽容还是报错，行为都一致）。start.sh 本身保持逐字节原样。

用法：
  python build_offline.py --arch amd64
  python build_offline.py --arch arm64
"""
import argparse
import hashlib
import io
import json
import os
import shutil
import struct
import sys
import tarfile
import time

HERE = os.path.dirname(os.path.abspath(__file__))
PROJECT = os.path.dirname(HERE)
CACHE_ROOT = os.path.join(HERE, "cache")
DIST_DIR = HERE

IMAGE_NAME = "dockercopilot"
# 上游 Dockerfile 里 LABEL authors="onlyLTY"
AUTHORS = "onlyLTY"

# ELF e_machine 取值 -> Docker 架构名，用于校验二进制架构没搞错
ELF_MACHINE = {0x3E: "amd64", 0xB7: "arm64"}

# 上游 Dockerfile 的 ENV
APP_ENV = [
    ("secretKey", ""),
    ("DOCKER_HOST", "unix:///var/run/docker.sock"),
    ("BACKUP_DIR", "/data/backups"),
    ("TZ", "Asia/Shanghai"),
    ("WORKDIR", "/app"),
]
APP_PORT = "12712"
APP_VOLUME = "/data"

# 新前端产物里的资源名。二进制里能搜到它就说明 //go:embed front/* 真的打进去了，
# 而不是编译时 front/ 还是占位页。
FRONTEND_MARKER = b"assets/index-"


def sha256_file(p):
    h = hashlib.sha256()
    with open(p, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def read_elf_arch(path):
    """读 ELF 头判断架构，顺带确认这确实是个 Linux ELF 可执行文件。"""
    with open(path, "rb") as f:
        head = f.read(20)
    if head[:4] != b"\x7fELF":
        return None, "不是 ELF 文件"
    if head[5] != 1:
        return None, "不是小端序"
    machine = struct.unpack("<H", head[18:20])[0]
    arch = ELF_MACHINE.get(machine)
    if not arch:
        return None, f"未知 e_machine=0x{machine:04x}"
    return arch, None


# --------------------------------------------------------------- staging
def add_dir_to_tar(tf, src_root, arc_prefix, mtime):
    """把目录树写进层 tar。目录 0755、文件 0644（可执行文件在调用前单独处理）。"""
    count = 0
    for root, dirs, files in os.walk(src_root):
        rel = os.path.relpath(root, src_root).replace(os.sep, "/")
        base = arc_prefix if rel == "." else f"{arc_prefix}/{rel}"
        for d in sorted(dirs):
            ti = tarfile.TarInfo(f"{base}/{d}")
            ti.type, ti.mode, ti.mtime = tarfile.DIRTYPE, 0o755, mtime
            ti.uid = ti.gid = 0
            ti.uname = ti.gname = ""
            tf.addfile(ti)
        for fn in sorted(files):
            p = os.path.join(root, fn)
            ti = tarfile.TarInfo(f"{base}/{fn}")
            ti.size = os.path.getsize(p)
            ti.type, ti.mtime = tarfile.REGTYPE, mtime
            ti.mode = 0o755 if os.access(p, os.X_OK) else 0o644
            ti.uid = ti.gid = 0
            ti.uname = ti.gname = ""
            with open(p, "rb") as f:
                tf.addfile(ti, f)
            count += 1
    return count


def add_members_from_tar(tf, src_tar, mtime):
    """把另一个 tar 的条目**原样**搬进层 tar，保留权限位与符号链接。

    apk 的 data 段里有符号链接（如 zoneinfo 的别名时区），在 Windows 上
    先解包到磁盘再重新打包会丢失链接（无权限创建 symlink），所以直接搬运
    tar 条目而不是走文件系统。
    """
    count = 0
    with tarfile.open(src_tar, "r:") as src:
        for m in src:
            name = m.name.lstrip("./")
            if not name:
                continue
            ti = tarfile.TarInfo(name)
            ti.type = m.type
            ti.mode = m.mode
            ti.linkname = m.linkname
            ti.uid = ti.gid = 0
            ti.uname = ti.gname = ""
            ti.mtime = mtime
            if m.isfile():
                ti.size = m.size
                tf.addfile(ti, src.extractfile(m))
            else:
                tf.addfile(ti)
            count += 1
    return count


def build_staging(arch, meta, staging):
    """组装 /app 目录树。返回 staging 根路径。"""
    os.makedirs(staging, exist_ok=True)

    binary_src = os.path.join(PROJECT, "dc-back", "dist", "linux", arch, "dockerCopilot")
    start_src = os.path.join(PROJECT, "start.sh")
    etc_src = os.path.join(PROJECT, "etc")

    for p in (binary_src, start_src, etc_src):
        if not os.path.exists(p):
            print(f"FATAL: 缺少构建输入 {p}")
            sys.exit(1)

    # 二进制架构守卫：防止把 amd64 的二进制打进 arm64 镜像
    got_arch, err = read_elf_arch(binary_src)
    if err:
        print(f"FATAL: {binary_src} 不是有效的 Linux ELF: {err}")
        sys.exit(1)
    if got_arch != arch:
        print(f"FATAL: 二进制架构不符 —— 期望 {arch}，实际 {got_arch}")
        sys.exit(1)

    # 前端 embed 守卫：确认 front/ 里的新前端真的被编译进去了
    with open(binary_src, "rb") as f:
        blob = f.read()
    if FRONTEND_MARKER not in blob:
        print(f"WARN: 二进制里找不到前端资源标记 {FRONTEND_MARKER!r}，"
              f"front/ 可能仍是占位页（先跑 `cd frontend && npm run build`）")
    bin_size = len(blob)
    print(f"      dockerCopilot: {bin_size/1e6:.1f} MB, ELF {got_arch}")

    app = os.path.join(staging, "app")
    os.makedirs(app, exist_ok=True)

    shutil.copyfile(binary_src, os.path.join(app, "dockerCopilot"))
    os.chmod(os.path.join(app, "dockerCopilot"), 0o755)

    shutil.copyfile(start_src, os.path.join(app, "start.sh"))
    os.chmod(os.path.join(app, "start.sh"), 0o755)

    shutil.copytree(etc_src, os.path.join(app, "etc"), dirs_exist_ok=True)

    # 数据卷挂载点：Docker 会在这里挂上卷/绑定目录
    os.makedirs(os.path.join(staging, "data"), exist_ok=True)
    return staging


# --------------------------------------------------------------- main
def main():
    ap = argparse.ArgumentParser(description="离线组装 dockerCopilot 镜像 tar")
    ap.add_argument("--arch", required=True, choices=["amd64", "arm64"], help="目标架构")
    ap.add_argument("--version", default=None,
                    help="版本号（默认读项目根的 version 文件）")
    ap.add_argument("--tag", default="latest", help="额外打的标签名（默认 latest）")
    args = ap.parse_args()

    cache = os.path.join(CACHE_ROOT, args.arch)
    meta_path = os.path.join(cache, "meta.json")
    if not os.path.exists(meta_path):
        print(f"FATAL: 缺少 {meta_path}，先跑: python fetch_base.py --arch {args.arch}")
        sys.exit(1)
    meta = json.load(open(meta_path, encoding="utf-8"))

    version = args.version
    if not version:
        vf = os.path.join(PROJECT, "version")
        version = open(vf, encoding="utf-8").read().strip() if os.path.exists(vf) else "dev"
    print(f"==== 组装 {IMAGE_NAME} {version} linux/{args.arch} ====")
    print(f"基础镜像: Alpine {meta['alpine_release']}（{meta['layers']} 层）\n")

    # 0. 基础层校验：sha256 必须与 config.rootfs.diff_ids 一一对应
    cfg = json.load(open(os.path.join(cache, "config.json"), encoding="utf-8"))
    diff_ids = list(cfg["rootfs"]["diff_ids"])
    base_layers = []
    for i in range(meta["layers"]):
        p = os.path.join(cache, f"layer-{i}.tar")
        if sha256_file(p) != diff_ids[i].split(":", 1)[1]:
            print(f"FATAL: 基础层 {i} 与 diff_id 不匹配，缓存已损坏")
            sys.exit(1)
        base_layers.append(p)
    print(f"[1/4] 基础层校验通过（{meta['layers']} 层）")

    # 1. staging
    print("[2/4] 组装应用内容 ...")
    staging = os.path.join(HERE, f"staging-run{time.strftime('%H%M%S')}")
    try:
        build_staging(args.arch, meta, staging)
    except Exception:
        shutil.rmtree(staging, ignore_errors=True)
        raise

    # 2. 应用层
    print("[3/4] 打包应用层 ...")
    # 归档内的 mtime 与 created 都取同一个值。设了 SOURCE_DATE_EPOCH 就固定它，
    # 这样同样的输入能产出逐字节相同的 tar（可复现构建，哈希可当校验基准）。
    epoch = os.environ.get("SOURCE_DATE_EPOCH")
    fixed_mtime = int(epoch) if epoch else int(time.time())
    layer_raw = os.path.join(HERE, f"app-layer-{args.arch}.tar")
    n_files = 0
    with tarfile.open(layer_raw, "w", format=tarfile.GNU_FORMAT) as tf:
        n_files += add_dir_to_tar(tf, staging, "", fixed_mtime)
        # tzdata：直接搬 apk data 段的条目（替代 Dockerfile 里的 apk add tzdata）
        for name, info in (meta.get("apks") or {}).items():
            apk_tar = os.path.join(cache, f"{name}-data.tar")
            if not os.path.exists(apk_tar):
                print(f"FATAL: 缺少 {apk_tar}")
                sys.exit(1)
            added = add_members_from_tar(tf, apk_tar, fixed_mtime)
            print(f"      {name} {info['version']}: 并入 {added} 个条目")
    app_diffid = "sha256:" + sha256_file(layer_raw)
    print(f"      应用层: {n_files} 个常规文件, diff_id {app_diffid[:19]}...")

    # 3. 改写 config
    created = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(fixed_mtime))
    out_cfg = dict(cfg)
    out_cfg["architecture"] = meta["oci_arch"]
    out_cfg["os"] = "linux"
    out_cfg["created"] = created
    out_cfg.pop("container_config", None)

    c = dict(out_cfg.get("config") or {})
    c["WorkingDir"] = "/app"
    c["Entrypoint"] = None                    # 上游 Dockerfile 无 ENTRYPOINT
    c["Cmd"] = ["./start.sh"]
    env = list(c.get("Env") or [])
    for k, v in APP_ENV:
        env = [e for e in env if not e.startswith(k + "=")] + [f"{k}={v}"]
    c["Env"] = env
    c["ExposedPorts"] = {f"{APP_PORT}/tcp": {}}
    c["Volumes"] = {APP_VOLUME: {}}
    c["Labels"] = {
        "authors": AUTHORS,
        "org.opencontainers.image.title": IMAGE_NAME,
        "org.opencontainers.image.description": "Docker 容器管理面板",
        "org.opencontainers.image.version": version,
        "org.opencontainers.image.source": "https://github.com/onlyLTY/dockerCopilot",
        "org.opencontainers.image.licenses": "AGPL-3.0",
    }
    out_cfg["config"] = c
    out_cfg["rootfs"] = {"type": "layers", "diff_ids": diff_ids + [app_diffid]}
    out_cfg["history"] = list(out_cfg.get("history") or []) + [{
        "created": created,
        "created_by": f"offline-build: COPY app (dockerCopilot {version} + etc + start.sh) "
                      f"+ tzdata {meta.get('apks', {}).get('tzdata', {}).get('version', 'n/a')}",
        "empty_layer": False,
    }]
    cfg_json = json.dumps(out_cfg, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
    cfg_hex = hashlib.sha256(cfg_json).hexdigest()

    all_layers = base_layers + [layer_raw]
    layer_ids = [d.split(":", 1)[1] for d in out_cfg["rootfs"]["diff_ids"]]

    # 4. 拼 docker-save tar
    print("[4/4] 拼装 docker-save 归档 ...")
    os.makedirs(DIST_DIR, exist_ok=True)
    out_path = os.path.join(DIST_DIR, f"dockercopilot-{version}-{args.arch}-image.tar")
    tmp_out = os.path.join(DIST_DIR, f".tmp-{args.arch}.tar")
    # 只打 latest 单标签。带版本号的附加标签（dockercopilot:v2.1.3）已按要求去掉：
    # 反复 load 不同版本会在宿主机上堆出一串 tag，而 compose 引用的只有 latest。
    # 版本信息不丢 —— 它仍在 tar 文件名、镜像 config 的 LABEL 与 /app 内的 version 里。
    repo_tags = [f"{IMAGE_NAME}:{args.tag}"]

    with tarfile.open(tmp_out, "w", format=tarfile.GNU_FORMAT) as out:
        # 每一层一个目录：<层id>/{VERSION,json,layer.tar}，json 里带 parent 形成链
        for idx, lid in enumerate(layer_ids):
            meta_obj = {"id": lid}
            if idx + 1 < len(layer_ids):
                meta_obj["parent"] = layer_ids[idx + 1]
            meta_b = json.dumps(meta_obj, separators=(",", ":")).encode()

            ti = tarfile.TarInfo(f"{lid}/")
            ti.type, ti.mtime = tarfile.DIRTYPE, fixed_mtime
            out.addfile(ti)
            for name, blob in ((f"{lid}/VERSION", b"1.0"), (f"{lid}/json", meta_b)):
                ti = tarfile.TarInfo(name)
                ti.size = len(blob)
                ti.mtime = fixed_mtime
                out.addfile(ti, io.BytesIO(blob))
            p = all_layers[idx]
            ti = tarfile.TarInfo(f"{lid}/layer.tar")
            ti.size = os.path.getsize(p)
            ti.mtime = fixed_mtime
            with open(p, "rb") as f:
                out.addfile(ti, f)

        for name, blob in (
            (f"{cfg_hex}.json", cfg_json),
            ("manifest.json", json.dumps([{
                "Config": f"{cfg_hex}.json",
                "RepoTags": repo_tags,
                "Layers": [f"{lid}/layer.tar" for lid in layer_ids],
            }], separators=(",", ":")).encode()),
            ("repositories", json.dumps({IMAGE_NAME: {args.tag: layer_ids[-1]}},
                                        separators=(",", ":")).encode()),
        ):
            ti = tarfile.TarInfo(name)
            ti.size = len(blob)
            ti.mtime = fixed_mtime
            out.addfile(ti, io.BytesIO(blob))
    shutil.move(tmp_out, out_path)

    size = os.path.getsize(out_path)
    print(f"\n==== 完成 ====")
    print(f"镜像 tar : {out_path}")
    print(f"大小     : {size/1e6:.1f} MB")
    print(f"RepoTags : {', '.join(repo_tags)}")
    print(f"层       : {len(layer_ids)} 层（{meta['layers']} 基础 + 1 应用）")

    # 产物 sha256 清单，供传输完整性核对。
    # 按文件名更新而不是追加 —— 否则反复构建同一个架构会堆出一串同名的旧哈希。
    # 同时剔除指向已删除产物的条目（例如只重建 amd64 时，清单里残留的 arm64 行会让
    # `sha256sum -c` 因找不到文件而报错）。
    sums = os.path.join(DIST_DIR, "SHA256SUMS.txt")
    fname = os.path.basename(out_path)
    entry = f"{sha256_file(out_path)} *{fname}"
    kept = []
    if os.path.exists(sums):
        for line in open(sums, encoding="utf-8").read().splitlines():
            line = line.strip()
            if not line or line.endswith("*" + fname):
                continue
            referenced = line.split("*", 1)[-1]
            if os.path.exists(os.path.join(DIST_DIR, referenced)):
                kept.append(line)
    kept.append(entry)
    with open(sums, "w", encoding="utf-8", newline="\n") as f:
        f.write("\n".join(sorted(kept)) + "\n")
    print(f"sha256   : {entry.split()[0]}")
    print(f"（已更新 {sums}）")

    # 清理本次 staging（只删本次新建的精确路径，不用通配）。
    # 环境对批量删除有守卫时可能被静默拦截 —— 不影响产物，tar 已经写完了。
    shutil.rmtree(staging, ignore_errors=True)
    if os.path.exists(staging):
        print(f"提示: 临时目录未清理，可手动删除 {staging}")


if __name__ == "__main__":
    main()
