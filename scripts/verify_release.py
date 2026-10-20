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
import time
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


def fetch_once(url, dest=None):
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


def fetch(url, dest=None, attempts=3):
    """下载/探测，**网络类失败重试**，HTTP 4xx/5xx 不重试。

    为什么必须区分（v0.31.63 实测踩到）：发布后立刻校验时偶发
    "Remote end closed connection without response"（代理/边缘节点抖动），
    旧实现把它和"资产真的不存在"报成同一句话 —— 于是闸门会随机变红，
    而人一旦见过它误报，下次就会直接忽略它（闸门就此失效）。
    404 是确定事实（资产没发），重试只是浪费时间；网络抖动重试两次足够。
    """
    last = None
    for i in range(max(1, attempts)):
        got, err = fetch_once(url, dest)
        if err is None:
            return got, None
        last = err
        # HTTP 状态码是**服务器的确定回答**，不重试。
        if err.startswith("HTTP "):
            return None, err
        if i + 1 < attempts:
            time.sleep(1.5 * (i + 1))
    return None, "%s（已重试 %d 次）" % (last, attempts)


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
    ap.add_argument("--offline", action="store_true",
                    help="不联网：只查版本一致 / 签名 / dist 本地产物（CI 与发布前置用）")
    ap.add_argument("--manifest-only", action="store_true",
                    help="只查清单本身（版本一致 / 有签名 / assets 非空），不碰网络与 dist（每次 CI 都跑）")
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

    # --manifest-only：每次 push/PR 都能跑的**轻量闸门**（v0.31.63，F06）。
    #
    # 它抓的是"清单与代码不同步"这一类：改了 version 常量却忘了改清单、手改了 JSON
    # 导致签名失效、assets 被清空。这些在本地看起来毫无异常，却会让所有客户端
    # 收不到更新（或者收到一个指向空资产的清单）。不需要网络，也不需要先构建。
    if a.manifest_only:
        for it in assets:
            if not (it.get("sha256") or "").strip():
                die("assets 里有一条没写 sha256 —— 客户端会拒绝下载")
            if not (it.get("url") or "").strip().startswith("http"):
                die("assets 里的 url 不是 http(s)：%r" % it.get("url"))
        print("OK（仅清单）：版本/签名/assets 齐全（v%s）" % ver)
        return

    for it in assets:
        name = it.get("name") or it.get("url", "").rsplit("/", 1)[-1]
        url = it.get("url", "")
        want_sha = (it.get("sha256") or "").lower()
        want_size = int(it.get("size") or 0)
        print("检查 %s" % name)
        print("  URL %s" % url)

        # --offline：跳过一切网络检查（v0.31.63）。CI 里"资产还没上传"是**正常状态**，
        # 那时唯一要保证的是「清单声明的 sha/size 与本次构建出的字节一致」——
        # 也就是"要发的东西就是测过的东西"。上传后由发布后校验再补 URL 可达性。
        if a.offline:
            if not a.local:
                print("  ⚠ --offline 建议配合 --local 使用（否则只校验了版本与签名）")
                continue
            lp = os.path.join(ROOT, "dist", name)
            if not os.path.exists(lp):
                die("--local 但 dist/%s 不存在（先构建）" % name)
            lsha, lsize = sha256_file(lp), os.path.getsize(lp)
            if want_sha and lsha != want_sha:
                die("dist/%s 与清单 sha 不一致（清单说的不是这次构建的字节）\n  清单 %s\n  实际 %s"
                    % (name, want_sha, lsha))
            if want_size and lsize != want_size:
                die("dist/%s 与清单大小不一致：%d vs %d" % (name, lsize, want_size))
            print("  本地产物与清单一致 ✓ (%d bytes)" % lsize)
            continue

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

    if a.offline:
        print("OK（离线）：清单版本/签名/本地产物一致（v%s）" % ver)
    else:
        print("OK：清单与真实资产一致（v%s）" % ver)


if __name__ == "__main__":
    main()
