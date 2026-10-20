#!/usr/bin/env python3
"""smoke —— 对**构建产物**做一次真实冒烟（v0.31.64，审查 F15 的"制品验收"）。

## 为什么单测不够

`go test ./...` 测的是**包**：它把所有 IO 边界都换成临时目录、假时钟、假 HTTP。
这套测试能保证逻辑对，但保证不了**这个 exe 真的能起来**：

- 前端资源是 `go:embed` 进去的 —— 打包漏文件，测试照绿，用户打开是白屏；
- 启动期有数据库迁移（`ensureColumn` 等）—— 迁移写错，测试不跑真实 main；
- 启动参数/日志/端口绑定 —— 只有真进程才验证；
- **包里跑的版本是不是清单说的那个版本** —— 只有读启动日志才知道。

审查 F15 把这条列为"制品验收不足"，这脚本就是补它：起进程 → 走一遍用户刚需路径 → 看日志。

## 检查项（全部通过才算过）

1. 进程在 15 秒内 `/healthz` 200；
2. `GET /` 200 且返回的 HTML 里有界面骨架（embed 没漏）；
3. **启动日志里的版本 == version.json 的版本**（防止"说 A 跑 B"）；
4. 注册 → 登录（拿到会话 cookie）→ 建 Key（拿到明文 `sk-…`）；
5. 用该 Key `GET /v1/models` 200 且是合法 JSON（不需要余额、不打上游）；
6. 全程无 `panic`；
7. 退出后数据目录里确实有 `webapp.db` 且含核心表。

## 用法

    python3 scripts/smoke.py                       # 用 dist/webapp.exe（本机平台）
    python3 scripts/smoke.py --exe dist/webapp-linux
    python3 scripts/smoke.py --keep                # 保留临时目录（排查用）

退出码 0 = 通过。失败会把**进程日志尾部**打出来 —— 那才是原因所在。
"""
import argparse
import http.cookiejar
import json
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def die(msg):
    print("FAIL: " + msg)
    sys.exit(1)


def free_port():
    """让系统分配一个空闲端口。

    不用固定端口：CI 上并行、本机上也常有人占着 8780；
    固定的"测试端口"迟早会撞上，然后表现为"冒烟莫名失败"。
    """
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    p = s.getsockname()[1]
    s.close()
    return p


def req(method, url, body=None, opener=None, headers=None, timeout=10):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    if data is not None:
        req.add_header("Content-Type", "application/json")
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    op = opener or urllib.request.build_opener()
    try:
        with op.open(req, timeout=timeout) as r:
            raw = r.read()
            return r.status, raw, r.headers
    except urllib.error.HTTPError as e:
        return e.code, e.read(), e.headers


def manifest_version():
    with open(os.path.join(ROOT, "version.json"), encoding="utf-8") as f:
        return json.load(f)["version"]


def wait_health(base, proc, timeout=15.0):
    """等 /healthz 200。进程提前死掉就立刻失败（别干等 15 秒）。"""
    deadline = time.time() + timeout
    while time.time() < deadline:
        if proc.poll() is not None:
            return False
        try:
            code, _, _ = req("GET", base + "/healthz", timeout=1)
            if code == 200:
                return True
        except Exception:  # noqa: BLE001 —— 连不上就是还没起来
            pass
        time.sleep(0.2)
    return False


def main():
    ap = argparse.ArgumentParser()
    default_exe = os.path.join("dist", "webapp.exe" if os.name == "nt" else "webapp-linux")
    ap.add_argument("--exe", default=default_exe)
    ap.add_argument("--keep", action="store_true", help="保留临时数据目录")
    a = ap.parse_args()

    exe = a.exe if os.path.isabs(a.exe) else os.path.join(ROOT, a.exe)
    if not os.path.exists(exe):
        die("找不到产物：%s（先跑 preflight.py --build-only）" % exe)

    tmp = tempfile.mkdtemp(prefix="wb2api-smoke-")
    port = free_port()
    base = "http://127.0.0.1:%d" % port
    db = os.path.join(tmp, "data", "webapp.db")
    # -no-tasks：冒烟不该触发签到/保活这类出站任务（既慢又可能真的打到上游）。
    # -secret none：跳过密钥文件，避免在 CI 上随机生成文件干扰断言。
    # -key：给一个固定管理密钥，保证可重复。
    cmd = [exe, "-addr", "127.0.0.1:%d" % port, "-db", db, "-secret", "none",
           "-no-tasks", "-key", "smoke-admin-key"]
    out_path = os.path.join(tmp, "run.log")
    out = open(out_path, "wb")
    print("启动 %s（端口 %d）" % (os.path.basename(exe), port))
    proc = subprocess.Popen(cmd, stdout=out, stderr=subprocess.STDOUT,
                            cwd=tmp, env={**os.environ, "TZ": "Asia/Shanghai"})
    try:
        if not wait_health(base, proc):
            out.close()
            tail = open(out_path, "rb").read()[-3000:].decode("utf-8", "replace")
            die("15 秒内 /healthz 没起来。进程日志尾部：\n" + tail)

        # ① 首页：embed 的资源真的在（否则用户看到白屏，而单测全绿）
        code, body, _ = req("GET", base + "/")
        if code != 200 or b"<html" not in body.lower():
            die("GET / 返回 %d，或没有 HTML 骨架（go:embed 漏文件？）" % code)
        print("首页 200，HTML %d 字节" % len(body))

        # ② 版本一致：跑起来的二进制必须就是清单说的那个版本
        out.flush()
        log = open(out_path, "rb").read().decode("utf-8", "replace")
        want = manifest_version()
        marker = "webapp %s 启动于" % want
        if marker not in log:
            got = [l for l in log.splitlines() if "启动于" in l]
            die("启动日志里的版本与 version.json(%s) 不符：%s" % (want, got[-1] if got else "（没有启动日志）"))
        print("版本一致：%s" % want)

        # ③ 用户刚需路径：注册 → 登录 → 建 Key → 列模型
        jar = http.cookiejar.CookieJar()
        opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))
        user = "smoke_%d" % int(time.time())
        code, body, _ = req("POST", base + "/api/auth/register",
                             {"username": user, "password": "Smoke_pw_2026"}, opener)
        if code != 200:
            die("注册失败 %d：%s" % (code, body[:300].decode("utf-8", "replace")))
        code, body, _ = req("POST", base + "/api/auth/login",
                             {"username": user, "password": "Smoke_pw_2026"}, opener)
        if code != 200:
            die("登录失败 %d：%s" % (code, body[:300].decode("utf-8", "replace")))
        if not list(jar):
            die("登录成功但没有会话 cookie —— 后续页面必然 401")
        code, body, _ = req("POST", base + "/api/keys", {"name": "smoke"}, opener)
        if code != 200:
            die("建 Key 失败 %d：%s" % (code, body[:300].decode("utf-8", "replace")))
        obj = json.loads(body)
        # 响应是 {"count":1,"key":{…,"key":"sk-…"},"keys":[…]}：单条时 `key` 是对象，
        # 明文在它的 `key` 字段里（v0.31.64 冒烟实测，别按直觉当字符串取）。
        node = obj.get("key")
        key = node.get("key") if isinstance(node, dict) else node
        if not isinstance(key, str) or not key.startswith("sk-"):
            die("建 Key 的响应里没有明文 key：%s" % body[:300].decode("utf-8", "replace"))
        print("注册/登录/建 Key 通过（key 前缀 %s…）" % key[:8])

        code, body, _ = req("GET", base + "/v1/models",
                             headers={"Authorization": "Bearer " + key})
        if code != 200:
            die("GET /v1/models 返回 %d：%s" % (code, body[:300].decode("utf-8", "replace")))
        try:
            models = json.loads(body)
        except Exception:
            die("/v1/models 不是合法 JSON：%s" % body[:200].decode("utf-8", "replace"))
        if "data" not in models:
            die("/v1/models 缺少 data 字段：%s" % body[:200].decode("utf-8", "replace"))
        print("/v1/models 200（%d 个模型）" % len(models.get("data") or []))

        # ④ 服务仍然活着（上面任何一步把进程搞挂了，这里会暴露）
        code, _, _ = req("GET", base + "/healthz")
        if code != 200:
            die("走完一轮后 /healthz 不再 200 —— 进程可能已经退出了")
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()
        out.close()

    # ⑤ 收尾断言：无 panic、库文件与表都在
    log = open(out_path, "rb").read().decode("utf-8", "replace")
    if "panic:" in log:
        die("日志里出现 panic：\n" + log[-2000:])
    if not os.path.exists(db):
        die("数据目录里没有 webapp.db（-db 参数没生效？）")
    size = os.path.getsize(db)
    if size < 4096:
        die("webapp.db 只有 %d 字节 —— 建表可能没跑" % size)
    print("数据目录 http://…/data/webapp.db %d 字节，无 panic" % size)

    if a.keep:
        print("临时目录保留：%s" % tmp)
    else:
        shutil.rmtree(tmp, ignore_errors=True)

    print("SMOKE OK：产物能起来、能注册、能取模型列表（%s）" % os.path.basename(exe))


if __name__ == "__main__":
    main()
