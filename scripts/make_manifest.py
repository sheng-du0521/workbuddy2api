#!/usr/bin/env python3
"""make_manifest —— 从**真实产物**生成 version.json 并签名（v0.31.63，审查 F06）。

## 为什么需要它

审查 F06 的原始问题是"清单超前于资产"：清单里写着某个版本的下载地址与 sha256，
而那个版本其实还没发布。根因不只是"忘了发"，更是**清单是手写的**：
人要先构建、再算 sha、再粘进 JSON、再签名——四步里任何一步顺序错或漏掉，
清单就开始说假话，而本地怎么看都正常。

这个脚本把四步合成一步，且**顺序不可颠倒**：

    读版本（main.go 是唯一事实源）→ 算 sha/size（真实产物）→ 写清单 → 签名

## 用法

    # 发布机标准流程（先构建，见 scripts/preflight.py）
    python3 scripts/make_manifest.py --notes-file notes.txt --key D:/_ro/update_signing.key

    # 只盖章不签名（CI 里不要用；没有私钥时不伪造）
    python3 scripts/make_manifest.py --notes "..."

改动只落在 version.json 里那几个字段；`_assets_hint` 之类的人读帮助字段原样保留。
"""
import argparse
import hashlib
import io
import json
import os
import re
import subprocess
import sys
import time

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
REPO = "sheng-du0521/workbuddy2api"


def die(msg):
    print("FAIL: " + msg, file=sys.stderr)
    sys.exit(1)


def code_version():
    """从 cmd/webapp/main.go 读 const version —— 它是版本的**唯一事实源**。

    为什么不让这个脚本收 `--version` 参数：那样就会存在"清单说 A、二进制里是 B"，
    正是 F06 的那类缺陷。参数只用来**校验**，不用来设值。
    """
    p = os.path.join(ROOT, "cmd", "webapp", "main.go")
    with io.open(p, encoding="utf-8") as f:
        m = re.search(r'const\s+version\s*=\s*"([^"]+)"', f.read())
    if not m:
        die("在 cmd/webapp/main.go 里找不到 const version")
    return m.group(1)


def sha256_file(p):
    h = hashlib.sha256()
    with open(p, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--exe", default=os.path.join("dist", "webapp.exe"),
                    help="发布资产（默认 dist/webapp.exe，即清单里 windows/amd64 那一条）")
    ap.add_argument("--os", default="windows")
    ap.add_argument("--arch", default="amd64")
    ap.add_argument("--notes", default="", help="版本说明（与 --notes-file 二选一）")
    ap.add_argument("--notes-file", default="", help="从文件读版本说明")
    ap.add_argument("--tag", default="", help="默认 v<version>")
    ap.add_argument("--key", default="", help="私钥路径；给了就签名")
    ap.add_argument("--relsign", default="", help="relsign 可执行文件；默认 dist/relsign[.exe]")
    ap.add_argument("--dry-run", action="store_true", help="只打印，不写文件")
    a = ap.parse_args()

    ver = code_version()
    exe = a.exe if os.path.isabs(a.exe) else os.path.join(ROOT, a.exe)
    if not os.path.exists(exe):
        die("产物不存在：%s（先跑 scripts/preflight.py 构建）" % exe)
    blob_size = os.path.getsize(exe)
    sha = sha256_file(exe)
    tag = a.tag or ("v" + ver)

    man_path = os.path.join(ROOT, "version.json")
    with io.open(man_path, encoding="utf-8") as f:
        man = json.load(f)

    # 版本只能与代码一致：这是本脚本存在的**核心理由**（防止"说 A 发 B"）。
    if man.get("version") and man["version"] != ver:
        print("清单原版本 %s → 改为代码版本 %s（清单只是跟随者，代码才是事实源）"
              % (man["version"], ver))
    man["version"] = ver
    man["released_at"] = int(time.time())
    man["url"] = "https://github.com/%s/releases" % REPO

    notes = a.notes
    if a.notes_file:
        with io.open(a.notes_file, encoding="utf-8") as f:
            notes = f.read().strip()
    if notes:
        man["notes"] = notes

    assets = man.get("assets") or [{}]
    assets[0] = {
        "os": a.os,
        "arch": a.arch,
        "url": "https://github.com/%s/releases/download/%s/%s" % (REPO, tag, os.path.basename(exe)),
        "sha256": sha,
        "size": blob_size,
    }
    man["assets"] = assets

    print("版本 %s  标签 %s" % (ver, tag))
    print("产物 %s  %d 字节  sha256=%s…" % (os.path.basename(exe), blob_size, sha[:16]))
    print("下载 %s" % assets[0]["url"])

    if a.dry_run:
        print("[DRY-RUN] 未写文件")
        return

    with io.open(man_path, "w", encoding="utf-8", newline="\n") as f:
        json.dump(man, f, ensure_ascii=False, indent=2)
        f.write("\n")
    print("已写入 %s" % man_path)

    if not a.key:
        print("⚠ 未提供 --key：清单**没有签名**。客户端会 fail-closed 拒绝它，"
              "所以这一步不能省（发布前必须 sign）。")
        return

    relsign = a.relsign
    if not relsign:
        for cand in ("relsign.exe", "relsign"):
            p = os.path.join(ROOT, "dist", cand)
            if os.path.exists(p):
                relsign = p
                break
    if not relsign:
        die("找不到 relsign（先跑 preflight 构建，或用 --relsign 指定）")
    # 签名必须在**写完清单之后**：签的是最终字节里的语义字段。
    code = subprocess.call([relsign, "sign", "-key", a.key, "-in", man_path])
    if code != 0:
        die("签名失败（exit=%d）" % code)
    code = subprocess.call([relsign, "verify", "-in", man_path])
    if code != 0:
        die("签完自检没通过 —— 清单不可用")
    print("签名并自检通过。")


if __name__ == "__main__":
    main()
