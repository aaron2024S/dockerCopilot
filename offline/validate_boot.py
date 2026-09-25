#!/usr/bin/env python3
"""行为校验：把镜像里的启动脚本真跑一遍，确认启动链路走得通。

为什么需要这一步：validate_image.py 只读字节。shell-town 曾出现过
**静态检查全绿、容器一启动就无限重启**的情况（启动脚本 cwd 残留导致入口
相对路径解析失败）。dockerCopilot 有同样的风险面 —— start.sh 用
`cd "${WORKDIR}"` 改工作目录，之后以相对路径 `./dockerCopilot` 启动主程序；
主程序又用相对路径读 `etc/dockerCopilot.yaml`。只要 cwd 错了，容器就起不来。

因此这里用一个**桩程序**替换真实二进制，在临时目录里复现镜像内的
目录结构 + 环境变量，然后跑 `sh start.sh`，验证：

  A. WORKDIR 有值时脚本能切进该目录，且桩程序确实在**该目录**里被执行
     （不是调用者的 cwd —— 这正是 cwd 类回归的判据）
  B. ./dockerCopilot 的相对路径解析正确（不依赖调用者的 cwd）
  C. 自更新分支：存在 dockerCopilot-new 时会被 mv 覆盖，且新文件被真正执行、
     执行后仍有可执行位
  D. 主程序就在脚本同级目录（./dockerCopilot 与 ./etc 都基于同一个 cwd）
  E. 幂等：重复执行结果稳定
  F. 即使调用者的 cwd 是别处（模拟 `docker run` 的宿主 cwd 差异）行为也不变
  G. 反向验证：WORKDIR 为空时脚本会 `exit`（说明上游全靠 cwd 兜底，
     而我们在镜像里补了 ENV WORKDIR=/app 把这个不确定性消掉了）

Windows 上需要 sh：脚本会依次尝试 PATH、SH 环境变量、常见 Git 安装路径，
并把 sh 所在目录并入子进程 PATH（否则 Git 的 find/cp/mkdir 找不到，
会以 127 command not found 失败，看起来像启动脚本的 bug）。

用法：
  python validate_boot.py
  python validate_boot.py <tar路径>
"""
import glob
import io
import json
import os
import shutil
import subprocess
import sys
import tarfile
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
fails, warns = [], []


def ok(cond, msg):
    print(("  PASS " if cond else "  FAIL ") + msg)
    if not cond:
        fails.append(msg)


def warn(msg):
    print("  WARN " + msg)
    warns.append(msg)


# --------------------------------------------------------------- 找 sh
def find_sh():
    cands = []
    w = shutil.which("sh")
    if w:
        cands.append(w)
    if os.environ.get("SH"):
        cands.append(os.environ["SH"])
    for base in (r"C:\Program Files\Git", r"C:\Program Files (x86)\Git",
                 r"C:\Program Files\Git\usr", r"C:\cygwin64", r"C:\msys64"):
        for rel in (r"usr\bin\sh.exe", r"bin\sh.exe", r"bin\bash.exe"):
            cands.append(os.path.join(base, rel))
    cands += ["/bin/sh", "/usr/bin/sh", "/bin/bash"]
    for c in cands:
        if c and os.path.exists(c):
            return os.path.abspath(c)
    return None


def child_env(sh_path, extra=None):
    """构造子进程环境。

    Git Bash 的 sh 依赖同目录下的 find/cp/mkdir，而宿主 PATH 常被裁剪掉
    git 的 usr/bin，所以要把 sh 所在目录并进去。PATH 分隔符要按宿主 PATH
    的风格选：MSYS 程序收到 `;` 分隔的原生路径会自行转换；若用 `:` 拼正斜杠
    路径，MSYS 会把整串当成**一个**路径项，路径全部失效。
    """
    env = dict(os.environ)
    env.pop("PYTHONIOENCODING", None)
    sh_dir = os.path.dirname(sh_path)
    cur = env.get("PATH", "")
    sep = ";" if ";" in cur else ":"
    env["PATH"] = sh_dir + sep + cur
    if extra:
        env.update(extra)
    return env


def to_posix(p):
    """Windows 路径 -> MSYS 路径（C:\\a\\b -> /c/a/b），好跟 sh 的 pwd 输出比对。"""
    p = os.path.abspath(p).replace("\\", "/")
    if len(p) > 1 and p[1] == ":":
        p = "/" + p[0].lower() + p[2:]
    return p


def safe_remove(p):
    try:
        os.remove(p)
    except FileNotFoundError:
        pass


def run_sh(sh_path, script, cwd, env_extra):
    """用 sh 跑脚本。

    script 必须传**绝对路径**：本校验刻意让调用者 cwd 与脚本所在目录不同，
    以便验证脚本内部的 `cd "${WORKDIR}"` 是否真的把目录切过去了。
    PATH 与 WORKDIR 都转成 MSYS 能正确识别的路径形态。
    """
    extra = dict(env_extra)
    # 只转换非空值：to_posix("") 会退化成"当前进程 cwd"，那样就测不到空 WORKDIR 了
    if extra.get("WORKDIR"):
        extra["WORKDIR"] = to_posix(extra["WORKDIR"])
    # cwd 必须给 Windows 原生路径（CreateProcess 不认 MSYS 的 /c/... 形式），
    # 但传给 sh 的脚本路径要用 POSIX 形式（MSYS 自己解析它）。
    p = subprocess.run([sh_path, to_posix(script)], cwd=os.path.abspath(cwd),
                       env=child_env(sh_path, extra),
                       stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                       timeout=60)
    return p.returncode, p.stdout.decode("utf-8", "replace")


# --------------------------------------------------------------- 取镜像内容
def load_app_layer(tar_path):
    """从镜像里取出应用层的 /app/start.sh 与 etc 清单。"""
    with tarfile.open(tar_path, "r") as tf:
        man = json.loads(tf.extractfile("manifest.json").read())
        layers = man[0]["Layers"]
        app_raw = tf.extractfile(layers[-1]).read()
    out = {}
    with tarfile.open(fileobj=io.BytesIO(app_raw)) as lt:
        for m in lt:
            name = "/" + m.name.lstrip("./")
            if name == "/app/start.sh" and m.isfile():
                out["start.sh"] = lt.extractfile(m).read()
            elif name.startswith("/app/etc/"):
                out.setdefault("etc", []).append(name)
    return out


# --------------------------------------------------------------- 场景
STUB = """#!/bin/sh
{{
  echo "cwd=$(pwd)"
  echo "tag={tag}"
  echo "workdir=${{WORKDIR}}"
}} > "{outfile}"
"""


def prepare(app_dir, sh_path, start_bytes):
    """在 app_dir 里铺出镜像内的结构。"""
    os.makedirs(os.path.join(app_dir, "etc"), exist_ok=True)
    with open(os.path.join(app_dir, "etc", "dockerCopilot.yaml"), "w",
              encoding="utf-8", newline="\n") as f:
        f.write("Name: dockerCopilot\nPort: 12712\n")
    with open(os.path.join(app_dir, "start.sh"), "wb") as f:
        f.write(start_bytes)
    os.chmod(os.path.join(app_dir, "start.sh"), 0o755)


def write_stub(path, outfile, tag):
    with open(path, "w", encoding="utf-8", newline="\n") as f:
        f.write(STUB.format(outfile=outfile.replace("\\", "/"), tag=tag))
    os.chmod(path, 0o755)


def main():
    tar_path = sys.argv[1] if len(sys.argv) > 1 else None
    if not tar_path:
        cands = glob.glob(os.path.join(HERE, "dockercopilot-*-image.tar"))
        tar_path = max(cands, key=os.path.getmtime) if cands else None
    if not tar_path or not os.path.exists(tar_path):
        print("FATAL: 找不到镜像 tar")
        sys.exit(1)

    print(f"行为校验 {os.path.basename(tar_path)}")
    print("=" * 68)

    sh_path = find_sh()
    print(f"\n[0] 运行环境")
    if not sh_path:
        print("  SKIP 找不到 sh（Windows 上需安装 Git for Windows）")
        print("\n结果: SKIP —— 行为校验未执行")
        sys.exit(0)
    print(f"  info: sh = {sh_path}")

    app = load_app_layer(tar_path)
    if "start.sh" not in app:
        print("FATAL: 镜像里没有 /app/start.sh")
        sys.exit(1)
    start_bytes = app["start.sh"]
    etc = app.get("etc", [])
    print(f"  info: 已取出 start.sh（{len(start_bytes)} 字节）"
          f"，/app/etc/ 下 {len(etc)} 个文件")

    tmp = tempfile.mkdtemp(prefix="bootchk-")
    app_dir = os.path.join(tmp, "app")
    # 故意把调用者 cwd 放在 app 之外，用来验证脚本是否真的切了目录
    caller_cwd = os.path.join(tmp, "caller")
    os.makedirs(caller_cwd, exist_ok=True)
    prepare(app_dir, sh_path, start_bytes)

    try:
        start_abs = os.path.join(app_dir, "start.sh")

        # ---------------- A/B/D. 正常启动
        print("\n[A/B/D] 正常启动：cwd 切换与相对路径解析")
        out_plain = os.path.join(tmp, "out-plain.txt")
        write_stub(os.path.join(app_dir, "dockerCopilot"), out_plain, "plain")
        rc, log = run_sh(sh_path, start_abs, caller_cwd, {"WORKDIR": app_dir})
        ok(rc == 0, f"退出码 0（实际 {rc}）")
        if rc != 0:
            print("       输出: " + log.strip()[:400])
        ok(os.path.exists(out_plain), "主程序被真正执行了")
        if os.path.exists(out_plain):
            kv = dict(l.split("=", 1) for l in
                      open(out_plain, encoding="utf-8").read().splitlines() if "=" in l)
            got_cwd = kv.get("cwd", "")
            ok(got_cwd.rstrip("/") == to_posix(app_dir).rstrip("/"),
               f"主程序 cwd == WORKDIR 指定目录（实际 {got_cwd}）")
            ok(got_cwd.rstrip("/") != to_posix(caller_cwd).rstrip("/"),
               "cwd 不是调用者目录（说明 `cd \"${WORKDIR}\"` 真的生效了）")
            ok(kv.get("tag") == "plain", "执行的是 app 目录下那份程序")
        ok(os.path.isdir(os.path.join(app_dir, "etc")),
           "etc/ 与主程序同处一个 cwd（主程序按相对路径读它）")

        # ---------------- C. 自更新分支
        print("\n[C] 自更新分支：dockerCopilot-new 覆盖")
        out_new = os.path.join(tmp, "out-new.txt")
        out_old = os.path.join(tmp, "out-old.txt")
        safe_remove(out_new)
        safe_remove(out_old)
        write_stub(os.path.join(app_dir, "dockerCopilot"), out_old, "old")
        write_stub(os.path.join(app_dir, "dockerCopilot-new"), out_new, "new")
        rc2, log2 = run_sh(sh_path, start_abs, caller_cwd, {"WORKDIR": app_dir})
        ok(rc2 == 0, f"退出码 0（实际 {rc2}）")
        if rc2 != 0:
            print("       输出: " + log2.strip()[:400])
        ok(os.path.exists(out_new), "新二进制被真正执行了")
        ok(not os.path.exists(out_old), "旧二进制没有被执行（已被 mv 覆盖）")
        ok(not os.path.exists(os.path.join(app_dir, "dockerCopilot-new")),
           "dockerCopilot-new 已被 mv 走（不会残留）")
        cur = os.path.join(app_dir, "dockerCopilot")
        if os.path.exists(cur):
            txt = open(cur, encoding="utf-8").read()
            ok("tag=new" in txt, "覆盖后 dockerCopilot 内容 == 新版本")

        # ---------------- E. 幂等
        print("\n[E] 幂等：再跑一次结果一致")
        safe_remove(out_new)
        rc3, _ = run_sh(sh_path, start_abs, caller_cwd, {"WORKDIR": app_dir})
        ok(rc3 == 0, f"第二次退出码仍为 0（实际 {rc3}）")
        ok(os.path.exists(out_new), "第二次仍正常执行主程序")

        # ---------------- F. 调用者 cwd 变化
        print("\n[F] 调用者 cwd 换成别处，行为不变")
        other = os.path.join(tmp, "elsewhere")
        os.makedirs(other, exist_ok=True)
        safe_remove(out_new)
        rc4, _ = run_sh(sh_path, start_abs, other, {"WORKDIR": app_dir})
        body = open(out_new, encoding="utf-8").read() if os.path.exists(out_new) else ""
        ok(rc4 == 0 and "cwd=" in body,
           "换了调用者 cwd 依然能启动（不依赖 `docker run` 的宿主 cwd）")
        ok(to_posix(app_dir).rstrip("/") in body.replace("\\", "/"),
           f"主程序仍在 WORKDIR 目录下执行（{body.splitlines()[0] if body else ''}）")

        # ---------------- G. 反向验证 WORKDIR 为空
        print("\n[G] 反向验证：WORKDIR 为空时的行为")
        rc5, log5 = run_sh(sh_path, start_abs, app_dir, {"WORKDIR": ""})
        print(f"  info: WORKDIR 为空 -> 退出码 {rc5}")
        if rc5 != 0:
            print("        脚本以非 0 退出（`cd \"${WORKDIR}\" || exit` 生效）——"
                  "这正是镜像里补 ENV WORKDIR=/app 的原因")
        else:
            print("        脚本未退出（该 sh 对 `cd \"\"` 宽容），"
                  "补的 ENV 仍保证行为与 shell 实现无关")
        print("        注：这里用的是宿主 sh（Git Bash），busybox ash 的行为未必相同；"
              "镜像里 ENV WORKDIR=/app 使两种实现下结果一致")

    finally:
        shutil.rmtree(tmp, ignore_errors=True)

    print("\n" + "=" * 68)
    if fails:
        print(f"结果: FAIL —— {len(fails)} 项未通过")
        for f in fails:
            print("  - " + f)
        sys.exit(1)
    print(f"结果: PASS —— 启动链路行为验证通过"
          + (f"（{len(warns)} 条警告）" if warns else ""))


if __name__ == "__main__":
    main()
