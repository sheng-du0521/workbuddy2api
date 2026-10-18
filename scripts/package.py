#!/usr/bin/env python3
# WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
# 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
# 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

"""package.py 构建并打包发布件（v0.31.44）。

## 为什么需要它

在这个脚本之前，Release 里只有**一个裸的 webapp.exe**，而 README 写着
「下载对应平台的压缩包（附 SHA256SUMS.txt 可核对）」—— 承诺的三样东西
（压缩包、SHA256SUMS、Linux 版）一个都不存在，用户拿到的是一个双击就弹黑框、
不知道密钥在哪、也不知道怎么停的可执行文件。

本脚本把「发布」这件事变成一条命令，产出**自解释**的包：

```
dist/pkg/
  wb2api-<ver>-windows-amd64.zip     # exe + 启动脚本 + 快速开始
  wb2api-<ver>-linux-amd64.tar.gz    # 二进制 + 安装脚本 + 快速开始
  SHA256SUMS.txt                      # 上面两个的校验和
```

## 硬约束

- **绝不打进 data/、密钥、数据库、导出账号文件**。打包前逐个断言，
  发现可疑文件直接中止（历史教训：`dist/` 既是分发目录又是运行目录，
  直接打 dist 就等于把 secret.key 与生产库一起发出去）。
- 包内**不含**任何版本敏感的手写内容：启动脚本里的版本号由本脚本从
  `version.json` 注入，避免出现"脚本里写着 v0.31.25、程序其实是 0.31.44"。

用法：
    python scripts/package.py                 # 构建 + 打包
    python scripts/package.py --no-build      # 只用已有产物打包
    python scripts/package.py --out dist/pkg  # 指定输出目录
"""

import argparse
import hashlib
import io
import json
import os
import platform
import shutil
import subprocess
import sys
import tarfile
import zipfile

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DIST = os.path.join(ROOT, "dist")
DEPLOY = os.path.join(ROOT, "deploy")

# 包里**只允许**出现这些名字（白名单）。任何白名单之外的文件都不进包。
ALLOW_WINDOWS = {"webapp.exe", "start-webapp.bat", "快速开始.txt", "Web应用使用说明.md"}
ALLOW_LINUX = {"webapp", "install.sh", "快速开始.txt", "Web应用使用说明.md"}

# 出现任何一个就说明打包源目录被污染了 —— 直接中止，不做"跳过"。
FORBIDDEN = {
    "webapp.db", "webapp.db-wal", "webapp.db-shm", "secret.key", "admin.key",
    "pool_state.json", "task_ledger.json", "gateway.key", ".apikey",
}


def read_version():
    """版本号从 version.json 取（与 webapp 内的 const version 由测试锁住一致性）。"""
    with io.open(os.path.join(ROOT, "version.json"), encoding="utf-8") as f:
        return json.load(f)["version"]


def sh(cmd, env=None, cwd=ROOT):
    print("    $ " + " ".join(cmd))
    e = dict(os.environ)
    if env:
        e.update(env)
    r = subprocess.run(cmd, cwd=cwd, env=e)
    if r.returncode != 0:
        sys.exit("构建失败：%s" % " ".join(cmd))


def build(version):
    """构建两个平台的二进制。

    `-trimpath` 去掉了本机绝对路径（否则二进制里会带构建者的目录结构，
    对使用者没意义、对外发也不合适）；`-s -w` 去符号表，体积减半。
    """
    out = os.path.join(DIST, "pkg-build")
    os.makedirs(out, exist_ok=True)
    base = ["-trimpath", "-ldflags", "-s -w"]
    print("==> 构建 windows/amd64")
    sh(["go", "build"] + base + ["-o", os.path.join(out, "webapp.exe"), "./cmd/webapp"],
       env={"CGO_ENABLED": "0", "GOOS": "windows", "GOARCH": "amd64"})
    print("==> 构建 linux/amd64")
    sh(["go", "build"] + base + ["-o", os.path.join(out, "webapp"), "./cmd/webapp"],
       env={"CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": "amd64"})
    return out


def assert_clean(files):
    """打包前断言：白名单之外不放行，敏感文件名一律中止。"""
    for name in files:
        base = os.path.basename(name)
        if base in FORBIDDEN or base.startswith("webapp.db"):
            sys.exit("拒绝打包：发现敏感文件 %s（会把密钥/数据库发给使用者）" % base)
        if base.endswith((".pem", ".key", ".log")):
            sys.exit("拒绝打包：发现疑似凭据或日志 %s" % base)


def copy_common(dst, version):
    """放两个平台共用的文件：快速开始 + 完整使用说明。"""
    qs = os.path.join(DEPLOY, "快速开始.txt")
    if os.path.exists(qs):
        shutil.copy2(qs, os.path.join(dst, "快速开始.txt"))
    doc = os.path.join(DIST, "Web应用使用说明.md")
    if os.path.exists(doc):
        shutil.copy2(doc, os.path.join(dst, "Web应用使用说明.md"))


def make_windows(src, dst, version):
    stage = os.path.join(dst, "_win")
    shutil.rmtree(stage, ignore_errors=True)
    os.makedirs(stage, exist_ok=True)
    shutil.copy2(os.path.join(src, "webapp.exe"), os.path.join(stage, "webapp.exe"))
    bat = os.path.join(DEPLOY, "start-webapp.bat")
    if not os.path.exists(bat):
        sys.exit("缺少 deploy/start-webapp.bat（包内没有启动脚本，小白无法启动）")
    text = io.open(bat, encoding="utf-8").read()
    # 版本号由脚本注入：手写版本号一定会漂（历史脚本里还写着 v0.31.25）。
    text = text.replace("@VERSION@", version)
    io.open(os.path.join(stage, "start-webapp.bat"), "w", encoding="utf-8", newline="\r\n").write(text)
    copy_common(stage, version)

    assert_clean(os.listdir(stage))
    missing = ALLOW_WINDOWS - set(os.listdir(stage))
    if missing:
        sys.exit("windows 包缺文件：%s" % ", ".join(sorted(missing)))

    name = "wb2api-%s-windows-amd64.zip" % version
    path = os.path.join(dst, name)
    with zipfile.ZipFile(path, "w", zipfile.ZIP_DEFLATED) as z:
        for f in sorted(os.listdir(stage)):
            z.write(os.path.join(stage, f), f)
    shutil.rmtree(stage, ignore_errors=True)
    return name, path


def make_linux(src, dst, version):
    stage = os.path.join(dst, "_linux")
    shutil.rmtree(stage, ignore_errors=True)
    os.makedirs(stage, exist_ok=True)
    exe = os.path.join(stage, "webapp")
    shutil.copy2(os.path.join(src, "webapp"), exe)
    os.chmod(exe, 0o755)
    inst = os.path.join(DEPLOY, "install-linux.sh")
    if not os.path.exists(inst):
        sys.exit("缺少 deploy/install-linux.sh")
    shutil.copy2(inst, os.path.join(stage, "install.sh"))
    os.chmod(os.path.join(stage, "install.sh"), 0o755)
    copy_common(stage, version)

    assert_clean(os.listdir(stage))
    missing = ALLOW_LINUX - set(os.listdir(stage))
    if missing:
        sys.exit("linux 包缺文件：%s" % ", ".join(sorted(missing)))

    name = "wb2api-%s-linux-amd64.tar.gz" % version
    path = os.path.join(dst, name)
    # 用 tarfile 而不是调 tar 命令：Windows 上没有 GNU tar 的一致行为。
    with tarfile.open(path, "w:gz") as t:
        for f in sorted(os.listdir(stage)):
            info = t.gettarinfo(os.path.join(stage, f), arcname=f)
            info.uid = info.gid = 0
            info.uname = info.gname = "root"
            # 可执行位必须显式给（否则解压出来 ./webapp 跑不了，用户会以为包坏了）。
            info.mode = 0o755 if f in ("webapp", "install.sh") else 0o644
            with open(os.path.join(stage, f), "rb") as fh:
                t.addfile(info, fh)
    shutil.rmtree(stage, ignore_errors=True)
    return name, path


def sha256(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", default=os.path.join(DIST, "pkg"))
    ap.add_argument("--no-build", action="store_true", help="跳过构建，用 dist/pkg-build 里的已有产物")
    a = ap.parse_args()

    version = read_version()
    print("==> 打包 WorkBuddy2API v%s（%s/%s）" % (version, platform.system(), platform.machine()))
    os.makedirs(a.out, exist_ok=True)
    src = os.path.join(DIST, "pkg-build") if a.no_build else build(version)
    if a.no_build and not os.path.exists(os.path.join(src, "webapp.exe")):
        sys.exit("--no-build 但 %s 里没有产物，先跑一次不带该参数的" % src)

    print("==> 生成 windows 包")
    win_name, win_path = make_windows(src, a.out, version)
    print("    %s  %.1f MB" % (win_name, os.path.getsize(win_path) / 1048576))

    print("==> 生成 linux 包")
    lnx_name, lnx_path = make_linux(src, a.out, version)
    print("    %s  %.1f MB" % (lnx_name, os.path.getsize(lnx_path) / 1048576))

    # 校验和文件：README 承诺过它，就得真有，且格式要让 `sha256sum -c` 能直接用。
    sums = os.path.join(a.out, "SHA256SUMS.txt")
    lines = []
    for p in (win_path, lnx_path):
        lines.append("%s  %s" % (sha256(p), os.path.basename(p)))
    io.open(sums, "w", encoding="utf-8", newline="\n").write("\n".join(lines) + "\n")
    print("==> %s" % sums)
    for l in lines:
        print("    " + l)
    print("\n完成：%s" % a.out)


if __name__ == "__main__":
    main()
