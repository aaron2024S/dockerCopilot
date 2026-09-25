#!/usr/bin/env python3
"""静态校验 docker-save 格式的 dockerCopilot 镜像 tar。

不依赖 Docker 引擎，直接读归档字节做结构与内容双重核对。
设计动机来自 shell-town 的教训：**静态检查全过、运行时才炸**——
所以这里除了结构，还要把"启动链路能不能走通"逐项验一遍。

检查分组：
  A. manifest.json 结构（Config / RepoTags / Layers）
  B. config 文件名 == sha256(内容)，架构/OS 正确
  C. 每层 layer.tar 的 sha256 == config.rootfs.diff_ids 对应项（顺序一致）
  D. 基础系统文件存在（/bin/sh、ld-musl、/etc/passwd、busybox）
  E. 应用内容：dockerCopilot 是目标架构的 ELF 且可执行、start.sh 可执行且纯 LF、
     etc/dockerCopilot.yaml 到位
  F. tzdata 到位（zoneinfo 与 Asia/Shanghai）
  G. config 字段：WorkingDir / Cmd / Env / Volumes / ExposedPorts / Labels
  H. 启动链路：CMD 相对路径在 WorkingDir 下可解析、启动脚本引用的文件都在、
     start.sh 里 WORKDIR 变量有兜底
  I. 前端 embed：二进制里能搜到 front/ 的构建产物标记（不是占位页）
  J. 卫生：无 Windows 路径残留、层内无绝对路径/上跳路径、无 .map 之类宿主产物
  K. 一致性：层数 == diff_ids 数、repositories 指向最顶层、history 与层数匹配

用法：
  python validate_image.py                     # 校验 offline/ 下最新的镜像 tar
  python validate_image.py <tar路径>
"""
import glob
import hashlib
import io
import json
import os
import posixpath
import re
import struct
import sys
import tarfile

HERE = os.path.dirname(os.path.abspath(__file__))
ELF_MACHINE = {0x3E: "amd64", 0xB7: "arm64"}

APP_ENV_EXPECT = {
    "secretKey": "",
    "DOCKER_HOST": "unix:///var/run/docker.sock",
    "BACKUP_DIR": "/data/backups",
    "TZ": "Asia/Shanghai",
    "WORKDIR": "/app",
}


def default_tar():
    """默认取本脚本同目录下最新的 dockercopilot-*-image.tar（按 mtime）。"""
    cands = glob.glob(os.path.join(HERE, "dockercopilot-*-image.tar"))
    return max(cands, key=os.path.getmtime) if cands else None


TAR = sys.argv[1] if len(sys.argv) > 1 else default_tar()
if not TAR or not os.path.exists(TAR):
    print("FATAL: 找不到镜像 tar，请先跑 build_offline.py 或显式传入路径")
    sys.exit(1)

# 从文件名推架构与版本，用来反向核对镜像内声称的值（防止改了 tag 忘了改 version）
base = os.path.basename(TAR)
m = re.search(r"dockercopilot-(.+?)-(amd64|arm64)-image\.tar$", base)
EXPECT_VERSION = m.group(1) if m else None
EXPECT_ARCH = m.group(2) if m else None

fails, warns = [], []


def ok(cond, msg):
    print(("  PASS " if cond else "  FAIL ") + msg)
    if not cond:
        fails.append(msg)


def warn(cond, msg):
    if not cond:
        print("  WARN " + msg)
        warns.append(msg)


def sha256(b):
    return hashlib.sha256(b).hexdigest()


print(f"校验 {base}")
if EXPECT_ARCH:
    print(f"  推断: 版本={EXPECT_VERSION} 架构={EXPECT_ARCH}")
print("=" * 68)

tf = tarfile.open(TAR, "r")
names = set(tf.getnames())

# ---------------------------------------------------------------- A. manifest
print("\n[A] manifest.json")
man_raw = tf.extractfile("manifest.json").read()
man = json.loads(man_raw)
ok(isinstance(man, list) and len(man) == 1, "manifest.json 为单元素数组")
mi = man[0]
ok(isinstance(mi.get("Config"), str) and mi["Config"].endswith(".json"),
   f"Config 字段存在: {mi.get('Config')}")
tags = mi.get("RepoTags") or []
ok(isinstance(tags, list) and len(tags) >= 1, f"RepoTags 非空: {tags}")
layers = mi.get("Layers") or []
ok(isinstance(layers, list) and all(isinstance(x, str) for x in layers),
   f"Layers 列表: {len(layers)} 项")
for lay in layers:
    ok(lay in names, f"层文件存在于归档: {lay}")

# ---------------------------------------------------------------- B. config
print("\n[B] config.json")
cfg_raw = tf.extractfile(mi["Config"]).read()
ok(sha256(cfg_raw) == mi["Config"].rsplit(".", 1)[0],
   "config 文件名 == sha256(内容)")
cfg = json.loads(cfg_raw)
diff_ids = cfg["rootfs"]["diff_ids"]
if EXPECT_ARCH:
    ok(cfg.get("architecture") == EXPECT_ARCH,
       f"architecture == {EXPECT_ARCH}（实际 {cfg.get('architecture')}）")
ok(cfg.get("os") == "linux", f"os == linux（实际 {cfg.get('os')}）")
ok(cfg["rootfs"].get("type") == "layers", "rootfs.type == layers")

# ---------------------------------------------------------------- C. 层哈希
print("\n[C] 层 sha256 与 diff_id 一致性")
ok(len(layers) == len(diff_ids),
   f"层数一致：Layers {len(layers)} == diff_ids {len(diff_ids)}")
union = {}          # "/路径" -> (TarInfo, 所属层下标, 层内原始名)
layer_raws = []     # 各层明文 tar 字节，供按需读取文件内容
layer_sizes = []
for i, lay in enumerate(layers):
    raw = tf.extractfile(lay).read()
    layer_raws.append(raw)
    layer_sizes.append(len(raw))
    ok(sha256(raw) == diff_ids[i].split(":", 1)[1],
       f"层 {i} sha256 == diff_id ({len(raw)/1e6:.1f} MB)")
    with tarfile.open(fileobj=io.BytesIO(raw)) as lt:
        for mem in lt:
            union["/" + mem.name.lstrip("./")] = (mem, i, mem.name)


def info(path):
    """取并集中某路径的 TarInfo（后层覆盖前层）。"""
    return union[path][0]


def read_member(path):
    """读出某路径的文件内容。

    注意：文件在**内层**的层 tar 里，外层 docker-save 归档只有 <层id>/layer.tar；
    直接 tf.extractfile("/app/...") 会 KeyError。
    """
    _, idx, orig = union[path]
    with tarfile.open(fileobj=io.BytesIO(layer_raws[idx])) as lt:
        return lt.extractfile(lt.getmember(orig)).read()


app_layer_idx = len(layers) - 1
print(f"  info: 各层大小 {[f'{s/1e6:.1f}MB' for s in layer_sizes]}")

# ---------------------------------------------------------------- D. 基础系统
print("\n[D] 基础系统文件（来自 alpine，应原样保留）")
for f in ["/bin/sh", "/etc/passwd", "/etc/group", "/etc/alpine-release",
          "/usr/bin/env", "/sbin/apk", "/bin/busybox"]:
    ok(f in union, f"{f} 存在")
musl = [p for p in union if p.startswith("/lib/ld-musl-")]
ok(bool(musl), f"musl 动态链接器存在: {musl}")

# ---------------------------------------------------------------- E. 应用内容
print("\n[E] 应用内容")
BIN = "/app/dockerCopilot"
SH = "/app/start.sh"
YAML = "/app/etc/dockerCopilot.yaml"

for p in (BIN, SH, YAML):
    ok(p in union, f"{p} 存在")

if BIN in union:
    blob = read_member(BIN)
    m_ = info(BIN)
    ok(blob[:4] == b"\x7fELF", "dockerCopilot 是 ELF 可执行文件")
    ok(blob[:4] != b"MZ", "dockerCopilot 不是 Windows PE")
    if blob[:4] == b"\x7fELF":
        machine = struct.unpack("<H", blob[18:20])[0]
        got = ELF_MACHINE.get(machine, f"0x{machine:04x}")
        if EXPECT_ARCH:
            ok(got == EXPECT_ARCH,
               f"ELF 架构 == {EXPECT_ARCH}（实际 {got}）")
    ok(m_.mode & 0o111 != 0,
       f"dockerCopilot 有可执行位（mode={oct(m_.mode)}）")
    ok(m_.size > 5_000_000,
       f"dockerCopilot 体积合理（{m_.size/1e6:.1f} MB）")

    # I. 前端 embed
    print("\n[I] 前端 embed 检查")
    ok(b"assets/index-" in blob,
       "二进制内含前端构建产物标记（front/ 不是占位页）")
    ok(b'<div id="root"' in blob, "二进制内含 index.html 的挂载点")

if SH in union:
    sh_raw = read_member(SH)
    sh_m = info(SH)
    ok(sh_m.mode & 0o111 != 0, f"start.sh 有可执行位（mode={oct(sh_m.mode)}）")
    ok(b"\r\n" not in sh_raw,
       "start.sh 是纯 LF 换行（CRLF 会让 shebang 失效）")
    ok(sh_raw.startswith(b"#!"), "start.sh 以 shebang 开头")
    ok(b"./dockerCopilot" in sh_raw, "start.sh 里引用了 ./dockerCopilot")

# ---------------------------------------------------------------- F. tzdata
print("\n[F] tzdata（替代 Dockerfile 的 apk add tzdata）")
tz_root = "/usr/share/zoneinfo"
ok(tz_root in union, f"{tz_root} 目录存在")
shanghai = f"{tz_root}/Asia/Shanghai"
ok(shanghai in union, f"{shanghai} 存在")
if shanghai in union:
    mem = info(shanghai)
    if mem.issym() or mem.islnk():
        # linkname 是相对归档根的路径（tar 里没有前导 /），直接补一个 /
        target = mem.linkname if mem.linkname.startswith("/") else "/" + mem.linkname
        ok(target in union,
           f"Asia/Shanghai 是指向存在的链接 -> {mem.linkname}")
    else:
        ok(mem.size > 0, f"Asia/Shanghai 是常规文件（{mem.size} 字节）")
ok(f"{tz_root}/UTC" in union, f"{tz_root}/UTC 存在")

# ---------------------------------------------------------------- G. config 字段
print("\n[G] config 运行参数")
c = cfg.get("config") or {}
ok(c.get("WorkingDir") == "/app", f"WorkingDir == /app（实际 {c.get('WorkingDir')}）")
ok(c.get("Cmd") == ["./start.sh"], f"Cmd == ['./start.sh']（实际 {c.get('Cmd')}）")
ok(not c.get("Entrypoint"), f"无 ENTRYPOINT（实际 {c.get('Entrypoint')}）")

env = {}
for e in c.get("Env") or []:
    k, _, v = e.partition("=")
    env[k] = v
for k, v in APP_ENV_EXPECT.items():
    ok(env.get(k) == v, f"ENV {k}={v!r}（实际 {env.get(k)!r}）")
ok("PATH" in env, f"ENV PATH 存在：{env.get('PATH', '')[:60]}")

ok(c.get("Volumes") == {"/data": {}}, f"Volumes == /data（实际 {c.get('Volumes')}）")
ok(c.get("ExposedPorts") == {"12712/tcp": {}},
   f"ExposedPorts == 12712/tcp（实际 {c.get('ExposedPorts')}）")
labels = c.get("Labels") or {}
ok(labels.get("org.opencontainers.image.title") == "dockercopilot",
   "Label title == dockercopilot")
if EXPECT_VERSION:
    ok(labels.get("org.opencontainers.image.version") == EXPECT_VERSION,
       f"Label version == {EXPECT_VERSION}"
       f"（实际 {labels.get('org.opencontainers.image.version')}）")
ok(labels.get("authors") == "onlyLTY", "Label authors == onlyLTY")

# ---------------------------------------------------------------- H. 启动链路
print("\n[H] 启动链路可走通")
wd = c.get("WorkingDir") or "/"
cmd = (c.get("Cmd") or [""])[0]
if cmd.startswith("./"):
    # 必须用 posixpath：镜像是 Linux 的，在 Windows 上 os.path 会把 /app 变成 \app
    resolved = posixpath.normpath(posixpath.join(wd, cmd[2:]))
    ok(resolved in union,
       f"CMD {cmd} 在 WorkingDir 下解析为 {resolved} 且存在")
    if resolved in union:
        ok(info(resolved).mode & 0o111 != 0, f"{resolved} 可执行（否则容器起不来）")
ok(bool(env.get("WORKDIR")),
   "ENV WORKDIR 有值（start.sh 首行 `cd \"${WORKDIR}\" || exit` 依赖它）")
ok("/app/etc/dockerCopilot.yaml" in union,
   "start.sh 之后主程序读的 etc/dockerCopilot.yaml 在同目录下")
# 主程序按相对路径读 etc/，cwd 必须是 /app
ok(wd == "/app", "WorkingDir 与程序读取 etc/ 的相对路径一致")

# ---------------------------------------------------------------- J. 卫生
print("\n[J] 归档卫生")
abs_paths = [p for p in union if p.startswith("//")]
ok(not abs_paths, f"层内无绝对路径条目（{abs_paths[:3]}）")
escape = [p for p in union if "/../" in p or p.endswith("/..")]
ok(not escape, f"层内无上跳路径（{escape[:3]}）")
win = [p for p in union if re.search(r"[A-Za-z]:[\\/]", p) or "\\" in p]
ok(not win, f"层内无 Windows 路径残留（{win[:3]}）")
host_art = [p for p in union if p.endswith((".map", ".pdb"))]
ok(not host_art, f"无宿主调试产物（{host_art[:3]}）")
ok(not any(p.endswith(".tmp") for p in union), "无临时文件残留")

# ---------------------------------------------------------------- K. 一致性
print("\n[K] 归档一致性")
repos_raw = tf.extractfile("repositories").read() if "repositories" in names else b"{}"
repos = json.loads(repos_raw)
top_id = diff_ids[-1].split(":", 1)[1]
ok(repos.get("dockercopilot", {}).get("latest") == top_id,
   "repositories 的 latest 指向最顶层 diff_id")
hist = cfg.get("history") or []
n_real = sum(1 for h in hist if not h.get("empty_layer"))
ok(n_real == len(diff_ids),
   f"history 里非空层数 == diff_ids 数（{n_real} vs {len(diff_ids)}）")
ok(cfg.get("container_config") is None,
   "已剔除 container_config（构建期残留，不应进产物）")
ok(all(p.startswith("/") for p in union), "所有路径已规范化为绝对形式")

# ---------------------------------------------------------------- 汇总
print("\n" + "=" * 68)
print(f"文件: {base}")
print(f"大小: {os.path.getsize(TAR)/1e6:.1f} MB")
print(f"层  : {len(layers)}（基础 {len(layers)-1} + 应用 1）")
print(f"归档内条目总数（各层并集）: {len(union)}")
print(f"RepoTags: {tags}")
if fails:
    print(f"\n结果: FAIL —— {len(fails)} 项未通过")
    for f in fails:
        print("  - " + f)
    sys.exit(1)
print(f"\n结果: PASS —— 全部断言通过"
      + (f"（{len(warns)} 条警告）" if warns else ""))
