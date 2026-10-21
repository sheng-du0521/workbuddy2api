"""验证「用户端请求通道组」是否真的走得通（v0.31.94）。

背景：控制台里能切到通道组（wb-group-ch<N>）只证明**目录**对了；
用户真正关心的是"我把 Key 切到这个组、客户端发请求，能不能通"。
这一步必须真打一次上游 —— 单测和目录断言都覆盖不到组展开 → relay 分发 → 计费这条链。

做法：起**临时实例 + 临时库**（不碰生产），把生产上每条 enabled 通道的配置
（base_url / api_key / 模型映射）复制进来，注册一个用户、建一把**默认参数**的密钥，
然后对每条通道组依次：

  1. 切组（POST /api/keys/{id}/group）→ 200
  2. /v1/models              → 只列该组名 + 该组成员
  3. /v1/chat/completions    → 用**组名**发一次真实请求（max_tokens=16）

判定：2xx 且有正文 = 通过；失败时打出状态码 + 错误体 + X-Wb-Fail-Kind：
  - 带 X-Wb-Fail-Kind=upstream_* → 请求已到上游（链路通，是上游对该模型/协议的意见）
  - 我们自己的 group_not_found / 404 模型不存在 → 链路有问题

用法：
    python scripts/channel_group_probe.py --channels D:/work/temp/ch.json
    python scripts/channel_group_probe.py --channels … --exe dist/webapp.exe
"""
import argparse
import http.cookiejar
import json
import os
import shutil
import socket
import sqlite3
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

JAR = http.cookiejar.CookieJar()
OP = urllib.request.build_opener(urllib.request.ProxyHandler({}),
                                 urllib.request.HTTPCookieProcessor(JAR))
RAW = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def req(method, url, obj=None, opener=None, headers=None):
    hdrs = dict(headers or {})
    data = None
    if obj is not None:
        data = json.dumps(obj).encode()
        hdrs["Content-Type"] = "application/json"
    r = urllib.request.Request(url, data=data, method=method, headers=hdrs)
    try:
        with (opener or OP).open(r, timeout=120) as resp:
            return resp.status, resp.read(), dict(resp.headers)
    except urllib.error.HTTPError as e:
        return e.code, e.read(), dict(e.headers)


def free_port():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    p = s.getsockname()[1]
    s.close()
    return p


def seed(db, channels):
    """把通道配置灌进临时库；返回 [(ch_id, ch_name, models)]。"""
    c = sqlite3.connect(db, timeout=10)
    cur = c.cursor()
    now = int(time.time())
    out = []
    for u in channels:
        cur.execute("""INSERT INTO upstreams(name, kind, base_url, api_key, enabled, priority,
                                             note, created_at, updated_at)
                       VALUES(?, 'openai', ?, ?, 1, ?, 'probe', ?, ?)""",
                    (u["name"], u["base_url"], u["api_key"], u.get("priority", 100), now, now))
        up_id = cur.lastrowid
        for m in u["models"]:
            cur.execute("""INSERT INTO upstream_models(upstream_id, model, remote_model,
                                                       credit_per_1k, enabled, created_at, updated_at)
                           VALUES(?, ?, '', 0, 1, ?, ?)""", (up_id, m, now, now))
        out.append((up_id, u["name"], list(u["models"])))
    c.commit()
    c.close()
    return out


def topup(db, username):
    c = sqlite3.connect(db, timeout=10)
    c.execute("UPDATE users SET balance_micro=? WHERE username=?", (10 ** 12, username))
    c.commit()
    c.close()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--channels", required=True, help="通道配置 JSON（[{id,name,base_url,api_key,models}]）")
    ap.add_argument("--exe", default="")
    args = ap.parse_args()

    exe = args.exe or os.path.join(ROOT, "dist", "webapp.exe" if os.name == "nt" else "webapp-linux")
    if not os.path.isabs(exe):
        exe = os.path.join(ROOT, exe)
    if not os.path.exists(exe):
        sys.exit("找不到产物：%s" % exe)
    with open(args.channels, encoding="utf-8") as f:
        channels = json.load(f)
    if not channels:
        sys.exit("通道配置为空")

    tmp = tempfile.mkdtemp(prefix="wb2api-chprobe-")
    port = free_port()
    base = "http://127.0.0.1:%d" % port
    db = os.path.join(tmp, "data", "webapp.db")
    secret = os.path.join(tmp, "data", "secret.key")
    log_path = os.path.join(tmp, "run.log")
    out = open(log_path, "wb")
    proc = subprocess.Popen(
        [exe, "-addr", "127.0.0.1:%d" % port, "-db", db, "-secret", secret,
         "-no-tasks", "-key", "chprobe-admin-key"],
        stdout=out, stderr=subprocess.STDOUT, cwd=tmp,
        env={**os.environ, "TZ": "Asia/Shanghai"})
    fails = []
    try:
        ok = False
        for _ in range(80):
            time.sleep(0.25)
            if proc.poll() is not None:
                break
            try:
                if req("GET", base + "/healthz", opener=RAW)[0] == 200:
                    ok = True
                    break
            except Exception:
                pass
        if not ok:
            out.close()
            print(open(log_path, "rb").read()[-2000:].decode("utf-8", "replace"))
            sys.exit("服务没起来")

        seeded = seed(db, channels)

        user = "chp_%d" % int(time.time())
        pw = "Chprobe_pw_2026"
        code, body, _ = req("POST", base + "/api/auth/register", {"username": user, "password": pw})
        if code != 200:
            sys.exit("注册失败 %d：%s" % (code, body[:200]))
        code, _, _ = req("POST", base + "/api/auth/login", {"username": user, "password": pw})
        if code != 200:
            sys.exit("登录失败 %d" % code)
        topup(db, user)
        code, body, _ = req("POST", base + "/api/keys", {"name": "chprobe"})
        if code != 200:
            sys.exit("建 Key 失败 %d：%s" % (code, body[:200]))
        obj = json.loads(body)
        kobj = obj.get("key")
        plain = kobj.get("key") if isinstance(kobj, dict) else kobj
        key_id = (kobj.get("id") if isinstance(kobj, dict) else None) or obj.get("id")
        if not plain or not key_id:
            sys.exit("建 Key 响应缺明文或 id：%s" % body[:300])
        auth = {"Authorization": "Bearer " + plain}

        print("密钥 id=%s（默认参数：kind/scope/active 全空）｜通道 %d 条" % (key_id, len(seeded)))
        for up_id, name, models in seeded:
            ch = "ch%d" % up_id
            vmodel = "wb-group-" + ch
            print("\n=== %s（%s）成员 %d 个：%s" % (name, ch, len(models), "、".join(models)))

            code, body, _ = req("POST", base + "/api/keys/%d/group" % key_id, {"group": ch})
            print("  切组 -> %d %s" % (code, "" if code == 200 else body[:160].decode("utf-8", "replace")))
            if code != 200:
                fails.append("%s 切组失败 %d" % (ch, code))
                continue

            code, body, _ = req("GET", base + "/v1/models", opener=RAW, headers=auth)
            ids = []
            try:
                ids = [x.get("id") for x in (json.loads(body).get("data") or []) if isinstance(x, dict)]
            except Exception:
                pass
            print("  /v1/models -> %d 条目=%s" % (code, ids))
            if vmodel not in ids:
                fails.append("%s 的 /v1/models 里没有组名 %s" % (ch, vmodel))

            code, body, hdrs = req("POST", base + "/v1/chat/completions", {
                "model": vmodel, "max_tokens": 16, "stream": False,
                "messages": [{"role": "user", "content": "ping，只回一个 pong"}],
            }, opener=RAW, headers=auth)
            txt = body.decode("utf-8", "replace")
            kind = hdrs.get("X-Wb-Fail-Kind", "")
            if code == 200:
                content = ""
                try:
                    content = (json.loads(txt).get("choices") or [{}])[0].get("message", {}).get("content", "") or ""
                except Exception:
                    pass
                print("  对话(model=%s) -> 200  首句=%r" % (vmodel, content.strip()[:60]))
            else:
                print("  对话(model=%s) -> %d  fail_kind=%s  %s" % (vmodel, code, kind or "-", txt[:180]))
                # v0.31.95 起网关会按上游状态码分类（relay_*）：
                #   relay_rate_limited / relay_rejected / relay_unavailable / relay_unreachable
                #   = 请求已经到了通道/上游，结论来自上游，不是我们拦的 → 链路通，不算失败
                # 其余（缺 fail_kind、或 group_not_found / 模型不存在）= 我们自己拦的 → 真失败
                if kind.startswith("relay_"):
                    print("   ↳ 链路已打通（上游侧结论：%s）" % kind)
                else:
                    fails.append("%s 对话失败且不像上游结论（fail_kind=%r）：%d %s"
                                 % (ch, kind, code, txt[:120]))

        print()
        if fails:
            for f in fails:
                print("[FAIL] " + f)
            sys.exit(1)
        print("OK：用户端切组后请求通道组全部走通")
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=10)
        except Exception:
            proc.kill()
        out.close()
        shutil.rmtree(tmp, ignore_errors=True)


if __name__ == "__main__":
    main()
