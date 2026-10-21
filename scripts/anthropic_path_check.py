"""验 /v1/messages（Anthropic）与 /v1/chat/completions 是否走同一条可服务链路。

背景（v0.31.87）：控制台「验证连接」对 CC Switch 目标改打 `/v1/messages`。
这个改动的前提是「那条路由在鉴权过 + 余额够时真的进业务层」——
若它返回 404/400，前端的"验证通过"就是假的。

做法：起一个**临时实例 + 临时库**（不碰生产、不碰本机配置），注册 → 改库充值 → 建 Key，
然后用同一个 Key 打两条路径，比对状态码与错误信封。本地无号池时两者都应是 503
（= 路由在、鉴权过、body 解析过、进到选号）。

用法：
    python scripts/anthropic_path_check.py            # 用 dist/ 下的当前产物
    python scripts/anthropic_path_check.py --exe dist/webapp-linux
"""
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

ROOT = r"D:/work工作区/2api"
PORT = 0


def free_port():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    p = s.getsockname()[1]
    s.close()
    return p


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
        with (opener or OP).open(r, timeout=40) as resp:
            return resp.status, resp.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()


def topup(db, user):
    c = sqlite3.connect(db, timeout=10)
    c.execute("UPDATE users SET balance_micro=? WHERE username=?", (10 ** 12, user))
    c.commit()
    c.close()


def main():
    exe = os.path.join(ROOT, "dist", "webapp.exe" if os.name == "nt" else "webapp-linux")
    if len(sys.argv) > 2 and sys.argv[1] == "--exe":
        exe = sys.argv[2] if os.path.isabs(sys.argv[2]) else os.path.join(ROOT, sys.argv[2])
    if not os.path.exists(exe):
        sys.exit("找不到产物：%s" % exe)
    tmp = tempfile.mkdtemp(prefix="wb2api-msgpath-")
    port = free_port()
    base = "http://127.0.0.1:%d" % port
    db = os.path.join(tmp, "data", "webapp.db")
    secret = os.path.join(tmp, "data", "secret.key")
    log_path = os.path.join(tmp, "run.log")
    out = open(log_path, "wb")
    proc = subprocess.Popen(
        [exe, "-addr", "127.0.0.1:%d" % port, "-db", db, "-secret", secret,
         "-no-tasks", "-key", "msgpath-admin-key"],
        stdout=out, stderr=subprocess.STDOUT, cwd=tmp,
        env={**os.environ, "TZ": "Asia/Shanghai"})
    try:
        ok = False
        for _ in range(60):
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

        user = "msg_%d" % int(time.time())
        pw = "Msgpath_pw_2026"
        code, body = req("POST", base + "/api/auth/register", {"username": user, "password": pw})
        if code != 200:
            sys.exit("注册失败 %d：%s" % (code, body[:200]))
        code, _ = req("POST", base + "/api/auth/login", {"username": user, "password": pw})
        if code != 200:
            sys.exit("登录失败 %d" % code)
        topup(db, user)
        code, body = req("POST", base + "/api/keys", {"name": "msgpath"})
        if code != 200:
            sys.exit("建 Key 失败 %d：%s" % (code, body[:200]))
        obj = json.loads(body)
        key = (obj.get("key") or {}).get("key") if isinstance(obj.get("key"), dict) else obj.get("key")
        if not key:
            sys.exit("建 Key 响应没有明文 key：%s" % body[:200])

        auth = {"Authorization": "Bearer " + key}
        # ① Anthropic 形态（Claude Code 真正发的样子）
        code_a, body_a = req("POST", base + "/v1/messages",
                             {"model": "wb-group-work", "max_tokens": 1,
                              "messages": [{"role": "user", "content": "ping"}]},
                             opener=RAW,
                             headers={**auth, "anthropic-version": "2023-06-01"})
        # ② OpenAI 形态
        code_o, body_o = req("POST", base + "/v1/chat/completions",
                             {"model": "wb-group-work", "max_tokens": 1, "stream": False,
                              "messages": [{"role": "user", "content": "ping"}]},
                             opener=RAW, headers=auth)

        print("/v1/messages          -> %d  %s" % (code_a, body_a[:160].decode("utf-8", "replace")))
        print("/v1/chat/completions  -> %d  %s" % (code_o, body_o[:160].decode("utf-8", "replace")))

        bad = []
        for name, c in (("/v1/messages", code_a), ("/v1/chat/completions", code_o)):
            if c in (401, 404):
                bad.append("%s 返回 %d（%s）" % (name, c, "鉴权没过" if c == 401 else "路由不存在"))
        if code_a != code_o:
            bad.append("两条路径结论不一致（%d vs %d）—— 前端按目标换协议会得到不同结果"
                       % (code_a, code_o))
        if bad:
            for b in bad:
                print("[FAIL] " + b)
            sys.exit(1)
        print("OK：两条路径结论一致（%d），换协议不会改变结论" % code_a)
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
