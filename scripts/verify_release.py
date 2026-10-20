#!/usr/bin/env python3
"""verify_release —— 发布清单与真实资产的**一致性闸门**（审查报告 F06）。

背景：version.json 里写着下载 URL / sha256 / size，但发布是否真的发生了
没有任何校验。审查时发现清单指向的版本在公开 Release 里不存在，latest
还停在更早的版本 —— 用户点"下载"会 404，而本地一切正常。

用法（发布前后各跑一次；失败退出码非 0，可直接接进 CI）：

    python3 scripts/verify_release.py                 # 校验远程资产
    python3 scripts/verify_release.py --local          # 额外比对本地 dist 产物
    python3 scripts/verify_release.py --skip-download  # 只查可达与 HEAD 元信息

校验内容：
  1. version.json 的 version 与本地 `cmd/webapp/main.go` 的 const version 一致；
  2. 每个 asset 的 URL 可达（HTTP 200）；
  3. 下载后 sha256 与 size 与清单**逐字节一致**；
  4. --local：清单 sha/size 与 dist 里的同名产物一致（防止"测的与发的是两份"）；
  5. --relsign：清单签名可用密码学校验通过（v0.31.60，审查 F07）。
"""
import argparse
import hashlib
import io
import json
import os
import re
import sys
import subprocess
import tempfile
import urllib.error
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def die(msg):
    print("FAIL: " + msg)
    sys.exit(1)


def local_version():
    p = os.path.join(ROOT, "cmd", "webapp", "main.go")
    with io.open(p, encoding="utf-8", errors="ignore") as f:
        for line in f:
            m = re.match(r'\s*const\s+version\s*=\s*"([^"]+)"', line)
            if m:
                return m.group(1)
    return ""


def sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def fetch(url, dest=None):
    req = urllib.request.Request(url, method="GET")
    req.add_header("User-Agent", "wb2api-release-check/1")
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            if r.status != 200:
                return None, "HTTP %d" % r.status
            if dest is None:
                r.read(1)  # 触发一次读取，确认 body 可读
                return True, None
            h = hashlib.sha256()
            size = 0
            with open(dest, "wb") as f:
                while True:
                    chunk = r.read(1 << 20)
                    if not chunk:
                        break
                    size += len(chunk)
                    h.update(chunk)
                    f.write(chunk)
            return (h.hexdigest(), size), None
    except urllib.error.HTTPError as e:
        return None, "HTTP %d" % e.code
    except Exception as e:  # noqa: BLE001 —— 网络原因千奇百怪，统一报出来即可
        return None, str(e)


def check_signature(man, relsign):
    """校验清单的独立签名（v0.31.60，审查 F07）。

    为什么发布前必须验：清单决定了客户端"下哪个文件、sha256 是多少"。
    如果它没签名（或签名与内容不符），客户端会 fail-closed 拒绝整份清单 ——
    结果不是"更新提醒不显示"，而是"用户永远收不到更新"。这种失败在本地完全看不出来
    （文件都在、URL 都通），只有等到有人发现版本一直不涨。所以必须在上传前拦住。
    """
    sig = (man.get("signature") or "").strip()
    kid = (man.get("key_id") or "").strip()
    alg = (man.get("sig_alg") or "").strip()
    if not sig:
        die("清单没有签名（缺 signature）—— 客户端会 fail-closed 拒绝整份清单。"
            "先跑：relsign sign -key <私钥> -in version.json")
    print("清单签名 %s key_id=%s" % (alg or "(未标算法)", kid or "(未标 key_id)"))
    if not relsign:
        print("  ⚠ 未提供 --relsign，只确认'有签名'，未做密码学校验")
        return
    code = subprocess.call([relsign, "verify", "-in", os.path.join(ROOT, "version.json")])
    if code != 0:
        die("清单签名校验失败（exit=%d）—— 内容与签名不符，拒绝发布" % code)
    print("  签名校验通过 ✓")

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--local", action="store_true", help="同时比对 dist 本地产物")
    ap.add_argument("--skip-download", action="store_true", help="只检查可达性，不下载校验")
    ap.add_argument("--relsign", default="", help="relsign 可执行文件路径（给了就同时校验清单签名）")
    a = ap.parse_args()

    with io.open(os.path.join(ROOT, "version.json"), encoding="utf-8") as f:
        man = json.load(f)

    ver = man.get("version", "")
    lv = local_version()
    print("清单版本 %s / 代码版本 %s" % (ver, lv))
    if ver != lv:
        die("version.json 与 main.go 的 const version 不一致（会发布一个'说 A 发 B'的版本）")

    check_signature(man, a.relsign)

    assets = man.get("assets") or []
    if not assets:
        die("version.json 没有任何 assets")

    for it in assets:
        name = it.get("name") or it.get("url", "").rsplit("/", 1)[-1]
        url = it.get("url", "")
        want_sha = (it.get("sha256") or "").lower()
        want_size = int(it.get("size") or 0)
        print("检查 %s" % name)
        print("  URL %s" % url)

        if a.skip_download:
            ok, err = fetch(url)
            if not ok:
                die("%s 不可达：%s（清单指向了未发布的资产）" % (name, err))
            print("  可达 ✓")
            continue

        with tempfile.TemporaryDirectory() as td:
            dst = os.path.join(td, name)
            got, err = fetch(url, dst)
            if err:
                die("%s 下载失败：%s（清单指向了未发布的资产？）" % (name, err))
            got_sha, got_size = got
            if want_size and got_size != want_size:
                die("%s 大小不一致：清单 %d，实际 %d" % (name, want_size, got_size))
            if want_sha and got_sha != want_sha:
                die("%s sha256 不一致：\n  清单 %s\n  实际 %s" % (name, want_sha, got_sha))
            print("  sha256/size 一致 ✓ (%d bytes)" % got_size)

        if a.local:
            lp = os.path.join(ROOT, "dist", name)
            if not os.path.exists(lp):
                die("--local 但 dist/%s 不存在" % name)
            lsha, lsize = sha256_file(lp), os.path.getsize(lp)
            if want_sha and lsha != want_sha:
                die("dist/%s 与清单 sha 不一致（测的与发的不是同一份）" % name)
            if want_size and lsize != want_size:
                die("dist/%s 与清单大小不一致：%d vs %d" % (name, lsize, want_size))
            print("  本地 dist 产物一致 ✓")

    print("OK：清单与真实资产一致（v%s）" % ver)


if __name__ == "__main__":
    main()
