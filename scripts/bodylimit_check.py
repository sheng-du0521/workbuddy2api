# -*- coding: utf-8 -*-
"""请求体上限（413）的端到端复验 —— v0.31.73 起。

为什么需要单独一个脚本：413 只会在**鉴权通过、余额够、handler 真正读 body** 时出现。
三条前置条件任何一条不满足，都会拿到别的状态码，于是"上限到底放行到多大"验不出来：

  · 不带 key        → 401（body 一个字节没读）
  · 余额 0          → 402（鉴权中间件先拦，同样没读 body）
  · 服务端提前响应   → 客户端在发送中收到连接重置（10053/ECONNRESET），拿不到状态码

所以本脚本做全三件事：注册 → **直接改库充值**（否则永远停在 402）→ 建 Key →
**用 curl 发大 body**（curl 能在发送中收到响应并给出状态码，urllib 不行）。

判据（三条，缺一不可）：
  1. 9MB → 必须走到业务层（400/404/503 之类），**不能是 413，也不能是 402**
     —— 402 意味着 body 根本没读，这条验证等于没做；
  2. 41MB（超过默认 32MB）→ 必须是 413，且 hint 里带 WB_BODY_MAX_MB；
  3. 启动日志自报的上限必须与预期一致。

用法：
    python3 scripts/bodylimit_check.py                 # 默认 dist/webapp(.exe)
    python3 scripts/bodylimit_check.py --mb-limit 64   # 用 WB_BODY_MAX_MB=64 复验
"""
import argparse
import http.cookiejar
import json
import os
import shutil
import sqlite3
import subprocess
import sys
import tempfile
import time
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import smoke as S  # noqa: E402  （复用 req / wait_health / free_port，口径与冒烟一致）

# probe_model 刻意用一个不存在的名字：读 body 发生在解析与选号之前，
# 所以能验到上限，又绝不会真的打到上游账号（不消耗额度、不产生流水）。
PROBE_MODEL = "bodylimit-probe-nonexistent"
CURL = shutil.which("curl")


def write_payload(path, mb):
    """写一个 mb 规模的合法 JSON（单个 user 消息）。"""
    head = b'{"model":"%s","messages":[{"role":"user","content":"' % PROBE_MODEL.encode()
    with open(path, "wb") as f:
        f.write(head)
        chunk = b"a" * (1 << 20)
        for _ in range(mb):
            f.write(chunk)
        f.write(b'"}]}')


def post_raw(base, key, path):
    """curl 发原始 body，返回 (code, body 前 4KB)。

    用 curl 而不是 urllib：body 超限时服务端读完上限就返回 413 并关闭连接，
    此时客户端可能**还在发**。curl 会报告响应状态码，urllib 在 Windows 上直接抛
    WinError 10053 —— 拿不到状态码，验证就变成了猜。
    """
    if not CURL:
        S.die("找不到 curl（Git for Windows 自带，PATH 里应能直接调到）")
    out = path + ".resp"
    cmd = [CURL, "-s", "--noproxy", "*", "-o", out, "-w", "%{http_code}",
           "-X", "POST", base + "/v1/chat/completions",
           "-H", "Content-Type: application/json",
           "-H", "Authorization: Bearer " + key,
           "--data-binary", "@" + path]
    r = subprocess.run(cmd, capture_output=True, text=True, timeout=300)
    code = 0
    try:
        code = int((r.stdout or "0").strip() or "0")
    except ValueError:
        code = 0
    body = b""
    if os.path.exists(out):
        body = open(out, "rb").read(4096)
    return code, body


def topup(db_path, username):
    """直接改库充值：余额 0 会在鉴权中间件就被 402 拦下，body 根本没读。

    这是**验证脚本**（跑在临时库上），不是产品能力 —— 产品里改余额必须走充值/兑换码。
    """
    c = sqlite3.connect(db_path)
    try:
        c.execute("UPDATE users SET balance_micro=? WHERE username=?", (10 ** 12, username))
        c.commit()
        got = c.execute("SELECT balance_micro FROM users WHERE username=?", (username,)).fetchone()
    finally:
        c.close()
    if not got or not got[0]:
        S.die("充值失败：%s 的余额仍为 0" % username)


def main():
    ap = argparse.ArgumentParser()
    default_exe = os.path.join("dist", "webapp.exe" if os.name == "nt" else "webapp-linux")
    ap.add_argument("--exe", default=default_exe)
    ap.add_argument("--mb-limit", type=int, default=0,
                    help="用 WB_BODY_MAX_MB 覆盖上限（0 = 用默认值）")
    a = ap.parse_args()

    exe = a.exe if os.path.isabs(a.exe) else os.path.join(S.ROOT, a.exe)
    if not os.path.exists(exe):
        S.die("找不到产物：%s（先跑 preflight.py --build-only）" % exe)

    tmp = tempfile.mkdtemp(prefix="wb2api-bodylimit-")
    port = S.free_port()
    base = "http://127.0.0.1:%d" % port
    db = os.path.join(tmp, "data", "webapp.db")
    secret = os.path.join(tmp, "data", "secret.key")
    env = {**os.environ, "TZ": "Asia/Shanghai"}
    limit_mb = a.mb_limit or 32
    if a.mb_limit:
        env["WB_BODY_MAX_MB"] = str(a.mb_limit)

    cmd = [exe, "-addr", "127.0.0.1:%d" % port, "-db", db, "-secret", secret,
           "-no-tasks", "-key", "bodylimit-admin-key"]
    out_path = os.path.join(tmp, "run.log")
    out = open(out_path, "wb")
    print("启动 %s（端口 %d，上限 %d MB）" % (os.path.basename(exe), port, limit_mb))
    proc = subprocess.Popen(cmd, stdout=out, stderr=subprocess.STDOUT, cwd=tmp, env=env)
    try:
        if not S.wait_health(base, proc):
            out.close()
            tail = open(out_path, "rb").read()[-3000:].decode("utf-8", "replace")
            S.die("15 秒内 /healthz 没起来。进程日志尾部：\n" + tail)

        # 启动日志必须自报生效上限 —— 否则"日志说 32MB、实际是 8MB"这种分裂查不出来
        out.flush()
        log = open(out_path, "rb").read().decode("utf-8", "replace")
        if ("推理请求体上限 %d MB" % limit_mb) not in log:
            S.die("启动日志没有自报上限 %d MB：\n%s" % (limit_mb, log[-1500:]))
        print("启动日志确认：上限 %d MB" % limit_mb)

        jar = http.cookiejar.CookieJar()
        opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))
        user = "bl_%d" % int(time.time())
        code, body, _ = S.req("POST", base + "/api/auth/register",
                              {"username": user, "password": "Bodylimit_pw_2026"}, opener)
        if code != 200:
            S.die("注册失败 %d：%s" % (code, body[:300].decode("utf-8", "replace")))
        code, body, _ = S.req("POST", base + "/api/auth/login",
                              {"username": user, "password": "Bodylimit_pw_2026"}, opener)
        if code != 200:
            S.die("登录失败 %d" % code)
        topup(db, user)
        print("已给 %s 充值（否则会在 402 就停下，body 根本没读）" % user)

        code, body, _ = S.req("POST", base + "/api/keys", {"name": "bodylimit"}, opener)
        if code != 200:
            S.die("建 Key 失败 %d：%s" % (code, body[:300].decode("utf-8", "replace")))
        obj = json.loads(body)
        key = (obj.get("key") or {}).get("key") if isinstance(obj.get("key"), dict) else obj.get("key")
        if not key:
            S.die("建 Key 响应里没有明文 key：%s" % body[:300])

        # ① 9MB：旧 8MB 闸门会拒，现在必须走到业务层（本地无号池 → 503；有号池 → 400/404）。
        p9 = os.path.join(tmp, "big9.json")
        write_payload(p9, 9)
        code, body = post_raw(base, key, p9)
        if code == 413:
            S.die("9MB 仍被 413 拒绝 —— 上限没有真的生效：%s" % body[:300].decode("utf-8", "replace"))
        if code == 402:
            S.die("9MB 得到 402 —— body 根本没被读，这条验证无效（检查 topup 是否生效）")
        if code not in (400, 404, 500, 502, 503):
            S.die("9MB 得到意外状态 %d：%s" % (code, body[:300].decode("utf-8", "replace")))
        print("9MB  → %d（读到 body 并进入业务层 ✓）" % code)

        # ② 超过上限：必须 413，且 hint 指出可调项
        over = limit_mb + 9
        pbig = os.path.join(tmp, "bigover.json")
        write_payload(pbig, over)
        code, body = post_raw(base, key, pbig)
        if code != 413:
            S.die("%dMB 应返回 413，实际 %d：%s" % (over, code, body[:300].decode("utf-8", "replace")))
        if b"WB_BODY_MAX_MB" not in body:
            S.die("413 的 gateway_hint 没提 WB_BODY_MAX_MB：%s" % body[:400].decode("utf-8", "replace"))
        print("%dMB → 413 ✓（hint 指向 WB_BODY_MAX_MB）" % over)

        print("BODY LIMIT OK：%d MB 放行 / 超限 413 且指引可执行" % limit_mb)
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=10)
        except Exception:
            proc.kill()
        out.close()
        if not os.environ.get("KEEP_SMOKE"):
            shutil.rmtree(tmp, ignore_errors=True)


if __name__ == "__main__":
    main()
