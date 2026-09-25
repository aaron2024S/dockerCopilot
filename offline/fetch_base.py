#!/usr/bin/env python3
"""离线拉取基础镜像层与 tzdata，供 build_offline.py 组装镜像使用。

本机没有 Docker 引擎，因此不调用 docker 命令，而是直接走两条公开 HTTP 渠道：

  1. 基础镜像：registry v2 API（经国内镜像站）匿名拉取 library/alpine
     manifest index -> 选 linux/<arch> 子 manifest -> config blob + 各层 blob
     逐层校验 sha256(gzip blob) == manifest digest 且 sha256(明文) == diff_ids[i]
     401 时按 WWW-Authenticate 走标准 Bearer token 流程（匿名 token 即可）
  2. tzdata：上游 Dockerfile 有 `apk add --no-cache tzdata`，离线组装无法执行 apk。
     改为下载 apk 包、拆出其中的 data.tar.gz 直接铺进应用层。
     版本从基础层里的 /etc/alpine-release 推导，保证与基础镜像同源。

产物缓存到 offline/cache/<arch>/，之后重建镜像全程离线。

用法：
  python fetch_base.py --arch amd64
  python fetch_base.py --arch arm64
"""
import argparse
import base64
import gzip
import hashlib
import io
import json
import os
import re
import sys
import tarfile
import urllib.error
import urllib.request
import zlib

HERE = os.path.dirname(os.path.abspath(__file__))
CACHE_ROOT = os.path.join(HERE, "cache")

# registry v2 镜像站：按顺序尝试，第一个成功的就用。
# dockerproxy.net 放在最后 —— 它不要求鉴权，但按 digest 取子 manifest 会 500。
REGISTRY_MIRRORS = [
    "https://docker.m.daocloud.io",
    "https://hub.rat.dev",
    "https://docker.1ms.run",
    "https://dockerproxy.net",
]

# Alpine 软件源（apk）：优先国内镜像，失败再回源站
APK_MIRRORS = [
    "https://mirrors.tuna.tsinghua.edu.cn/alpine",
    "https://mirrors.aliyun.com/alpine",
    "https://dl-cdn.alpinelinux.org/alpine",
]

REPO = "library/alpine"

# Docker 平台名 -> (OCI 架构名, Alpine apk 仓库目录名)
ARCH_MAP = {
    "amd64": ("amd64", "x86_64"),
    "arm64": ("arm64", "aarch64"),
}

ACCEPT = ", ".join([
    "application/vnd.docker.distribution.manifest.v2+json",
    "application/vnd.docker.distribution.manifest.list.v2+json",
    "application/vnd.oci.image.manifest.v1+json",
    "application/vnd.oci.image.index.v1+json",
])
UA = {"User-Agent": "docker/25.0.5 offline-builder"}

# 上游 Dockerfile 里 apk add 的包
APK_WANTED = ["tzdata"]


def sha256_bytes(b):
    return hashlib.sha256(b).hexdigest()


def sha256_file(p):
    h = hashlib.sha256()
    with open(p, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


# --------------------------------------------------------------- 传输
def raw_get(url, headers, timeout=60, retries=2):
    last = None
    for _ in range(retries + 1):
        try:
            req = urllib.request.Request(url, headers=headers)
            with urllib.request.urlopen(req, timeout=timeout) as r:
                return r.read()
        except urllib.error.HTTPError:
            raise
        except Exception as e:  # noqa: BLE001
            last = e
    raise last


def registry_get(mirror, path, accept, timeout=60):
    """registry v2 匿名拉取，自动处理 Bearer 鉴权。

    registry 的标准流程是：首次请求返回 401 并在 WWW-Authenticate 头里给出
    realm/service/scope，客户端拿这些换一个匿名 token，再带上重放原请求。
    """
    url = mirror + path
    h = {"Accept": accept, **UA}
    try:
        return raw_get(url, h, timeout)
    except urllib.error.HTTPError as e:
        if e.code != 401:
            raise
        www = e.headers.get("WWW-Authenticate", "") or ""
        m = re.search(r'realm="([^"]+)"', www)
        if not m:
            raise RuntimeError(f"401 但未提供 realm: {www[:150]}")
        q = []
        for key in ("service", "scope"):
            mm = re.search(rf'{key}="([^"]+)"', www)
            if mm:
                q.append(f"{key}={mm.group(1)}")
        tok_raw = raw_get(m.group(1) + ("?" + "&".join(q) if q else ""), UA)
        tok = json.loads(tok_raw)
        token = tok.get("token") or tok.get("access_token")
        if not token:
            raise RuntimeError("token 响应缺少 token 字段")
        return raw_get(url, {**h, "Authorization": f"Bearer {token}"}, timeout)


def try_registry(path, timeout=60):
    """依次尝试各镜像站，返回 (bytes, 命中镜像)。"""
    errs = []
    for m in REGISTRY_MIRRORS:
        try:
            return registry_get(m, path, ACCEPT, timeout), m
        except Exception as e:  # noqa: BLE001
            errs.append(f"{m}: {type(e).__name__}: {e}")
    raise RuntimeError("所有镜像站均失败:\n  " + "\n  ".join(errs))


def try_urls(urls, timeout=60):
    errs = []
    for u in urls:
        try:
            return raw_get(u, UA, timeout), u
        except Exception as e:  # noqa: BLE001
            errs.append(f"{u}: {type(e).__name__}: {e}")
    raise RuntimeError("所有候选均失败:\n  " + "\n  ".join(errs))


# ----------------------------------------------------------- 基础镜像
def decompress_layer(blob, hint=""):
    """层 blob -> 明文 tar 字节。支持 gzip / zstd / 未压缩。"""
    if blob[:2] == b"\x1f\x8b":
        return gzip.decompress(blob)
    if blob[:4] == b"\x28\xb5\x2f\xfd":
        try:
            import zstandard  # noqa: PLC0415
        except ImportError:
            print(f"FATAL: 层 {hint} 是 zstd 压缩，需先安装 zstandard")
            sys.exit(1)
        return zstandard.ZstdDecompressor().decompress(blob, max_output_size=1 << 31)
    if len(blob) > 262 and blob[257:262] == b"ustar":
        return blob
    print(f"FATAL: 层 {hint} 压缩格式无法识别: {blob[:8].hex()}")
    sys.exit(1)


def fetch_base(arch, tag, cache):
    os.makedirs(cache, exist_ok=True)
    oci_arch, alpine_arch = ARCH_MAP[arch]

    print(f"[1/4] 拉取 {REPO}:{tag} 的 manifest index ...")
    index_raw, used = try_registry(f"/v2/{REPO}/manifests/{tag}")
    index = json.loads(index_raw)
    print(f"      来自 {used}")

    sub_digest = None
    if "manifests" in index:
        for e in index["manifests"]:
            p = e.get("platform") or {}
            if p.get("os") == "linux" and p.get("architecture") == oci_arch:
                sub_digest = e["digest"]
                break
        if not sub_digest:
            print(f"FATAL: index 中没有 linux/{oci_arch} 条目")
            sys.exit(1)
        print(f"      linux/{oci_arch} -> {sub_digest[:24]}...")
    else:
        print("      已是单架构 manifest")

    print(f"[2/4] 取 linux/{oci_arch} manifest ...")
    if sub_digest:
        raw, _ = try_registry(f"/v2/{REPO}/manifests/{sub_digest}")
        got = "sha256:" + sha256_bytes(raw)
        if got != sub_digest:
            print(f"FATAL: manifest digest 不匹配\n  got    {got}\n  expect {sub_digest}")
            sys.exit(1)
        manifest = json.loads(raw)
    else:
        manifest = index

    cfg_digest = manifest["config"]["digest"]
    cfg_raw, _ = try_registry(f"/v2/{REPO}/blobs/{cfg_digest}", timeout=120)
    if sha256_bytes(cfg_raw) != cfg_digest.split(":", 1)[1]:
        print("FATAL: config blob sha256 不匹配")
        sys.exit(1)
    cfg = json.loads(cfg_raw)
    diff_ids = cfg["rootfs"]["diff_ids"]
    layers = manifest["layers"]
    if len(layers) != len(diff_ids):
        print(f"FATAL: 层数 {len(layers)} 与 diff_ids {len(diff_ids)} 不一致")
        sys.exit(1)
    print(f"      config OK，{len(layers)} 层")
    with open(os.path.join(cache, "config.json"), "wb") as f:
        f.write(cfg_raw)

    print(f"[3/4] 下载并双重校验 {len(layers)} 个层 ...")
    for i, lay in enumerate(layers):
        dg = lay["digest"]
        blob_path = os.path.join(cache, f"blob-{i}.gz")
        if os.path.exists(blob_path):
            raw = open(blob_path, "rb").read()
            print(f"      层 {i}: 命中缓存")
        else:
            print(f"      层 {i}: 下载中 ({lay.get('size', 0)/1e6:.1f} MB) ...")
            raw, _ = try_registry(f"/v2/{REPO}/blobs/{dg}", timeout=900)
        if sha256_bytes(raw) != dg.split(":", 1)[1]:
            print(f"FATAL: 层 {i} blob sha256 与 manifest digest 不匹配")
            sys.exit(1)
        with open(blob_path, "wb") as f:
            f.write(raw)

        plain = decompress_layer(raw, hint=str(i))
        expect = diff_ids[i].split(":", 1)[1]
        if sha256_bytes(plain) != expect:
            print(f"FATAL: 层 {i} 明文 sha256 与 diff_id 不匹配")
            sys.exit(1)
        with open(os.path.join(cache, f"layer-{i}.tar"), "wb") as f:
            f.write(plain)
        print(f"      层 {i}: {len(plain)/1e6:.1f} MB，双重校验通过")

    print("[4/4] 读取基础镜像版本 ...")
    alpine_release = None
    for i in range(len(layers)):
        with tarfile.open(os.path.join(cache, f"layer-{i}.tar"), "r") as t:
            for m in t.getmembers():
                if m.name.lstrip("./") == "etc/alpine-release" and m.isfile():
                    alpine_release = t.extractfile(m).read().decode().strip()
                    break
        if alpine_release:
            break
    if not alpine_release:
        print("FATAL: 基础层里找不到 /etc/alpine-release")
        sys.exit(1)
    branch = "v" + ".".join(alpine_release.split(".")[:2])
    print(f"      Alpine {alpine_release}（软件源分支 {branch}）")

    meta = {
        "arch": arch,
        "oci_arch": oci_arch,
        "alpine_arch": alpine_arch,
        "tag": tag,
        "alpine_release": alpine_release,
        "apk_branch": branch,
        "layers": len(layers),
        "diff_ids": diff_ids,
        "layer_digests": [l["digest"] for l in layers],
        "registry_mirror": used,
    }
    with open(os.path.join(cache, "meta.json"), "w", encoding="utf-8") as f:
        json.dump(meta, f, indent=1)
    return meta


# --------------------------------------------------------------- apk
def apk_segments(buf):
    """把 apk 拆成段落列表：{start, end, raw, plain, kind, names}。

    apk 由**连续拼接**的 gzip 流构成，顺序固定：
      可选签名段(.SIGN.*) -> control 段(.PKGINFO) -> data 段(真正的文件树)

    两个坑：
      1. 标准 gzip 库只读第一个成员，必须用 zlib.decompressobj 逐个拆，
         靠 unused_data 反推下一个成员的起点。
      2. 签名段本身也是合法 tar，不能只凭"能解析成 tar"判断哪段是 data，
         否则会把 2KB 的签名当成文件树。
    """
    segs, pos, n = [], 0, len(buf)
    while pos < n:
        idx = buf.find(b"\x1f\x8b\x08", pos)
        if idx < 0:
            break
        d = zlib.decompressobj(31)
        try:
            plain = d.decompress(buf[idx:]) + d.flush()
        except zlib.error:
            pos = idx + 3
            continue
        end = n - len(d.unused_data)
        try:
            with tarfile.open(fileobj=io.BytesIO(plain), mode="r:") as t:
                names = t.getnames()
        except tarfile.TarError:
            names = []
        if any(x.startswith(".SIGN.") for x in names):
            kind = "signature"
        elif ".PKGINFO" in names:
            kind = "control"
        elif names:
            kind = "data"
        else:
            kind = None
        segs.append({"start": idx, "end": end, "raw": buf[idx:end],
                     "plain": plain, "kind": kind, "names": names})
        if not d.unused_data:
            break
        pos = end
    return segs


def parse_apkindex(raw):
    """解析 APKINDEX.tar.gz -> {包名: {字段: 值}}。"""
    with tarfile.open(fileobj=io.BytesIO(gzip.decompress(raw)), mode="r:") as t:
        member = next(m for m in t.getmembers() if m.name == "APKINDEX")
        text = t.extractfile(member).read().decode("utf-8", "replace")
    out, cur = {}, {}
    for line in text.splitlines() + [""]:
        if not line:
            if cur.get("P"):
                out[cur["P"]] = cur
            cur = {}
            continue
        k, _, v = line.partition(":")
        cur[k] = v
    return out


def fetch_apks(meta, cache):
    branch, apk_arch = meta["apk_branch"], meta["alpine_arch"]
    print(f"      解析 {branch}/main/{apk_arch} 的 APKINDEX ...")
    index_raw, src = try_urls([f"{m}/{branch}/main/{apk_arch}/APKINDEX.tar.gz"
                               for m in APK_MIRRORS], timeout=120)
    index = parse_apkindex(index_raw)
    print(f"      来自 {src}，共 {len(index)} 个包")

    resolved = {}
    for name in APK_WANTED:
        ent = index.get(name)
        if not ent:
            print(f"FATAL: APKINDEX 里没有 {name}")
            sys.exit(1)
        ver, size = ent["V"], int(ent["S"])
        filename = f"{name}-{ver}.apk"
        out_apk = os.path.join(cache, filename)
        if os.path.exists(out_apk) and os.path.getsize(out_apk) == size:
            apk_bytes = open(out_apk, "rb").read()
            print(f"      {filename}: 命中缓存")
        else:
            apk_bytes, _ = try_urls([f"{m}/{branch}/main/{apk_arch}/{filename}"
                                     for m in APK_MIRRORS], timeout=600)
            if len(apk_bytes) != size:
                print(f"FATAL: {filename} 大小与 APKINDEX 不符 "
                      f"({len(apk_bytes)} != {size})")
                sys.exit(1)
            with open(out_apk, "wb") as f:
                f.write(apk_bytes)
            print(f"      {filename}: 已下载 ({len(apk_bytes)/1e6:.2f} MB)")

        # APKINDEX 的 C: 字段（Q1+base64）是 control 段**原始压缩字节**的 sha1，
        # 这是实测确认的口径 —— 不是解压后的明文，也不是整个 apk 文件。
        segs = apk_segments(apk_bytes)
        ctrl = next((s for s in segs if s["kind"] == "control"), None)
        data_segs = [s for s in segs if s["kind"] == "data"]
        if not ctrl or not data_segs:
            print(f"FATAL: {filename} 段落结构异常: "
                  f"{[s['kind'] for s in segs]}")
            sys.exit(1)
        cks = ent.get("C", "")
        if cks.startswith("Q1"):
            got = base64.b64encode(hashlib.sha1(ctrl["raw"]).digest()).decode()
            if got != cks[2:]:
                print(f"FATAL: {filename} control 段校验和不符\n"
                      f"  got  {got}\n  want {cks[2:]}")
                sys.exit(1)

        # data 段是最后（也是最大）的一段
        data = max(data_segs, key=lambda s: len(s["plain"]))["plain"]
        with tarfile.open(fileobj=io.BytesIO(data), mode="r:") as t:
            dnames = set(t.getnames())
        if "usr/share/zoneinfo/Asia/Shanghai" not in dnames:
            print(f"FATAL: {filename} 的 data 段缺少 "
                  f"usr/share/zoneinfo/Asia/Shanghai")
            sys.exit(1)

        with open(os.path.join(cache, f"{name}-data.tar"), "wb") as f:
            f.write(data)
        resolved[name] = {"version": ver, "apk_size": len(apk_bytes),
                          "data_tar_size": len(data),
                          "data_sha256": sha256_bytes(data),
                          "files": len(dnames)}
        print(f"      {filename}: data 段 {len(data)/1e6:.2f} MB / "
              f"{len(dnames)} 个条目，校验和通过")

    meta["apks"] = resolved
    with open(os.path.join(cache, "meta.json"), "w", encoding="utf-8") as f:
        json.dump(meta, f, indent=1)
    return meta


# --------------------------------------------------------------- main
def main():
    ap = argparse.ArgumentParser(description="离线拉取基础镜像层与 tzdata")
    ap.add_argument("--arch", required=True, choices=sorted(ARCH_MAP), help="目标架构")
    ap.add_argument("--tag", default="latest", help="基础镜像 tag（默认 latest）")
    args = ap.parse_args()

    cache = os.path.join(CACHE_ROOT, args.arch)
    print(f"==== 拉取基础镜像 linux/{args.arch} -> {cache} ====\n")
    meta = fetch_base(args.arch, args.tag, cache)
    meta = fetch_apks(meta, cache)
    with open(os.path.join(cache, "meta.json"), "w", encoding="utf-8") as f:
        json.dump(meta, f, indent=1)

    print("\n==== 完成 ====")
    print(f"Alpine   : {meta['alpine_release']}")
    print(f"基础层数 : {meta['layers']}")
    for k, v in meta["apks"].items():
        print(f"{k:<9}: {v['version']}")
    print(f"缓存目录 : {cache}")
    print(f"\n下一步: python build_offline.py --arch {args.arch}")


if __name__ == "__main__":
    main()
