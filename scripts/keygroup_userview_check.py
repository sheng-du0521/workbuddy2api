"""验证「用户面能选用通道组」这一整条链（v0.31.92）。

背景：用户报「生成密钥后依然无法选用通道组」。根因是用户面的「组」曾有两套口径 ——
切换弹窗用 selectableGroupIDs（含 ch<N>），而模型清单 / /v1/models / 一键接入
还在用旧口径（只有 work / host）。单测能覆盖函数，但**接口真实 JSON 的形态**
（models 里到底有没有 wb-group-ch<N>、one-click 的默认组名是什么）只有真跑才看得见。

做法：起一个**临时实例 + 临时库**（不碰生产、不碰本机配置），直接往库里插一条
enabled 通道与一个模型映射，再注册普通用户、建一把**默认参数**的密钥（kind/scope/
active 全空 —— 正是"生成密钥后"的状态），然后逐条断言：

  1. /api/keys/{id}/group-options  选项里出现 ch1（能切）
  2. /v1/models                    新 Key 也列出 wb-group-ch1 与 wb-group-work
  3. /api/keys/{id}/models         models 里出现 wb-group-ch1（下拉里能选）
  4. /api/onboard/options          未切组时组名是 wb-group-work
  5. 切到 ch1 后                    /v1/models 只剩该组；接入包组名变成 wb-group-ch1

用法：
    python scripts/keygroup_userview_check.py            # 用 dist/ 下当前产物
    python scripts/keygroup_userview_check.py --exe dist/webapp.exe
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
        with (opener or OP).open(r, timeout=40) as resp:
            return resp.status, resp.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()


def free_port():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    p = s.getsockname()[1]
    s.close()
    return p


def seed_channel(db, name, models):
    """插一条 enabled 通道 + 若干模型映射（绕开探活，纯数据）。"""
    c = sqlite3.connect(db, timeout=10)
    cur = c.cursor()
    now = int(time.time())
    cur.execute("""INSERT INTO upstreams(name, kind, base_url, api_key, enabled, priority,
                                        note, created_at, updated_at)
                   VALUES(?, 'openai', 'https://relay.example.com/v1', 'sk-x', 1, 100, 'userview', ?, ?)""",
                (name, now, now))
    up_id = cur.lastrowid
    for m in models:
        cur.execute("""INSERT INTO upstream_models(upstream_id, model, remote_model,
                                                   credit_per_1k, enabled, created_at, updated_at)
                       VALUES(?, ?, ?, 1, 1, ?, ?)""", (up_id, m, m, now, now))
    c.commit()
    c.close()
    return up_id


def models_of(body):
    try:
        d = json.loads(body)
    except Exception:
        return []
    out = []
    for x in (d.get("data") or []):
        if isinstance(x, dict) and x.get("id"):
            out.append(x["id"])
    return out


def main():
    exe = os.path.join(ROOT, "dist", "webapp.exe" if os.name == "nt" else "webapp-linux")
    if len(sys.argv) > 2 and sys.argv[1] == "--exe":
        exe = sys.argv[2] if os.path.isabs(sys.argv[2]) else os.path.join(ROOT, sys.argv[2])
    if not os.path.exists(exe):
        sys.exit("找不到产物：%s" % exe)

    tmp = tempfile.mkdtemp(prefix="wb2api-userview-")
    port = free_port()
    base = "http://127.0.0.1:%d" % port
    db = os.path.join(tmp, "data", "webapp.db")
    secret = os.path.join(tmp, "data", "secret.key")
    log_path = os.path.join(tmp, "run.log")
    out = open(log_path, "wb")
    proc = subprocess.Popen(
        [exe, "-addr", "127.0.0.1:%d" % port, "-db", db, "-secret", secret,
         "-no-tasks", "-key", "userview-admin-key"],
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

        ch_id = seed_channel(db, "userview-relay", ["relay-only-model", "relay-second"])

        user = "uv_%d" % int(time.time())
        pw = "Userview_pw_2026"
        code, body = req("POST", base + "/api/auth/register", {"username": user, "password": pw})
        if code != 200:
            sys.exit("注册失败 %d：%s" % (code, body[:200]))
        code, _ = req("POST", base + "/api/auth/login", {"username": user, "password": pw})
        if code != 200:
            sys.exit("登录失败 %d" % code)

        # 刻意**不带 kind / 不带任何分组参数** —— 这就是"生成密钥后"的真实状态。
        code, body = req("POST", base + "/api/keys", {"name": "userview"})
        if code != 200:
            sys.exit("建 Key 失败 %d：%s" % (code, body[:200]))
        obj = json.loads(body)
        keyobj = obj.get("key")
        plain = keyobj.get("key") if isinstance(keyobj, dict) else keyobj
        key_id = (keyobj.get("id") if isinstance(keyobj, dict) else None) or obj.get("id")
        if not plain or not key_id:
            sys.exit("建 Key 响应缺明文或 id：%s" % body[:300])
        print("已建密钥 id=%s（kind/group_scope/active_group 全空）" % key_id)

        chg = "ch%d" % ch_id
        vmodel = "wb-group-" + chg

        # ① 切换弹窗的候选
        code, body = req("GET", base + "/api/keys/%d/group-options" % key_id)
        opt_ids = []
        if code == 200:
            opt_ids = [o.get("id") for o in (json.loads(body).get("options") or [])]
        print("① group-options -> %d  选项=%s" % (code, opt_ids))
        if chg not in opt_ids:
            fails.append("group-options 里没有 %s（用户切不过去）" % chg)

        # ② 客户端 /v1/models（新 Key，两字段全空 —— 曾经的死区）
        code, body = req("GET", base + "/v1/models", opener=RAW,
                         headers={"Authorization": "Bearer " + plain})
        ids = models_of(body)
        print("② /v1/models -> %d  含组名=%s" % (code, [i for i in ids if i.startswith("wb-group-")]))
        if vmodel not in ids:
            fails.append("/v1/models 里没有 %s（客户端选不到通道组）" % vmodel)
        if "wb-group-work" not in ids:
            fails.append("/v1/models 里没有 wb-group-work（新 Key 连官组都没了）")

        # ③ 控制台模型清单（测试对话下拉 / 接入弹窗的组标签）
        code, body = req("GET", base + "/api/keys/%d/models" % key_id)
        km = json.loads(body) if code == 200 else {}
        print("③ /api/keys/{id}/models -> %d  groups=%s" %
              (code, [g.get("id") for g in (km.get("groups") or [])]))
        if vmodel not in (km.get("models") or []):
            fails.append("密钥模型清单里没有 %s（下拉里选不到）" % vmodel)

        # ④ 未切组时，一键接入写官组
        code, body = req("GET", base + "/api/onboard/options?key_id=%d" % key_id)
        plan = json.loads(body) if code == 200 else {}
        print("④ onboard(未切组) -> %d  group_name=%s" % (code, plan.get("group_name")))
        if plan.get("group_name") != "wb-group-work":
            fails.append("未切组时接入组名应为 wb-group-work，得到 %r" % plan.get("group_name"))

        # ⑤ 切到通道组
        code, body = req("POST", base + "/api/keys/%d/group" % key_id, {"group": chg})
        print("⑤ 切到 %s -> %d  %s" % (chg, code, body[:140].decode("utf-8", "replace")))
        if code != 200:
            fails.append("切到 %s 失败：%d" % (chg, code))
        else:
            code, body = req("GET", base + "/v1/models", opener=RAW,
                             headers={"Authorization": "Bearer " + plain})
            ids = models_of(body)
            print("   /v1/models -> %d  条目=%s" % (code, ids))
            if vmodel not in ids or "relay-only-model" not in ids:
                fails.append("切组后 /v1/models 没有该组名或成员（%s）" % ids)
            if "wb-group-work" in ids:
                fails.append("切组后 /v1/models 仍列出 wb-group-work（切组没生效）")

            code, body = req("GET", base + "/api/onboard/options?key_id=%d" % key_id)
            plan2 = json.loads(body) if code == 200 else {}
            print("   onboard(已切组) -> %d  group_name=%s  batch=%s  count=%s" %
                  (code, plan2.get("group_name"), plan2.get("batch"), plan2.get("count")))
            if plan2.get("group_name") != vmodel:
                fails.append("切组后接入组名应为 %s，得到 %r（切组对一键接入无效）"
                             % (vmodel, plan2.get("group_name")))
            if not plan2.get("batch"):
                fails.append("通道组的接入应是批量（batch=false 只会接进去一个组名）")

            # ⑥ 生成接入包：必须把这组的模型**全部**写进去（v0.31.93）
            code, body = req("POST", base + "/api/keys/%d/onboard" % key_id,
                             {"target": "workbuddy", "os": "windows"})
            pkg = json.loads(body) if code == 200 else {}
            got = pkg.get("models") or []
            print("⑥ onboard 生成 -> %d  count=%s  models=%s" %
                  (code, pkg.get("count"), got))
            want = {"relay-only-model", "relay-second"}
            if code != 200:
                fails.append("生成接入包失败：%d" % code)
            elif set(got) != want:
                fails.append("接入包模型清单 = %s，期望该通道选用的全部 %s"
                             % (sorted(got), sorted(want)))
            elif not (pkg.get("script") or "").strip():
                fails.append("接入包没有脚本正文")

            # ⑦ CC Switch 同一套规则（"能接几个"不该看目标）
            code, body = req("POST", base + "/api/keys/%d/onboard" % key_id,
                             {"target": "ccswitch", "os": "unix"})
            pkg2 = json.loads(body) if code == 200 else {}
            got2 = pkg2.get("models") or []
            print("⑦ onboard(ccswitch) -> %d  count=%s  models=%s" %
                  (code, pkg2.get("count"), got2))
            if code != 200:
                fails.append("CC Switch 接入包生成失败：%d" % code)
            elif set(got2) != want:
                fails.append("CC Switch 接入包清单 = %s，期望 %s（两个目标口径应一致）"
                             % (sorted(got2), sorted(want)))

        if fails:
            for f in fails:
                print("[FAIL] " + f)
            sys.exit(1)
        print("OK：新密钥能切、能选、能接入通道组（%s）" % vmodel)
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
