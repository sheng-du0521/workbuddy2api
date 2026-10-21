# -*- coding: utf-8 -*-
"""一键接入的真机复验（v0.31.80）。

为什么需要它：一键接入的产物是**脚本**，而脚本能跑通只有真跑才知道 ——
单测只能验"生成的字符串里有某段文本"，验不了「PowerShell 里 @() 会不会多包一层」
「失败时脚本还在不在」这类只有执行才暴露的问题（v0.31.80 三个问题全是这么发现的）。

三个场景（全部在隔离 HOME 里跑，不碰本机真实配置）：
  1. workbuddy · 成功：合并写入、同名替换、其它条目保留、备份、自删、数量提示正确；
  2. workbuddy · 失败（原配置是坏 JSON）：非 0 退出、脚本保留、坏配置不被覆盖、WB_NO_PAUSE 生效；
  3. ccswitch：BASE_URL 是 origin（不带 /v1）、只改 env 两项、其它字段保留、备份、自删。

用法：python3 scripts/onboard_check.py [--keep]
依赖：本机可执行 dist/webapp.exe；场景 3 需要 bash（Windows 用 Git Bash）。
"""
import http.cookiejar
import json
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
EXE = os.path.join(ROOT, "dist", "webapp.exe")
WORK = os.path.join(tempfile.gettempdir(), "wb2api-onboard-check")
KEEP = "--keep" in sys.argv


def free_port():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    p = s.getsockname()[1]
    s.close()
    return p


def start_server(port, home):
    db = os.path.join(home, "data", "webapp.db")
    os.makedirs(os.path.dirname(db), exist_ok=True)
    p = subprocess.Popen([EXE, "-addr", "127.0.0.1:%d" % port, "-db", db,
                          "-secret", os.path.join(home, "data", "secret.key"), "-no-tasks"],
                         cwd=home, stdout=subprocess.DEVNULL, stderr=subprocess.STDOUT)
    base = "http://127.0.0.1:%d" % port
    op = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()),
                                     urllib.request.ProxyHandler({}))
    for _ in range(60):
        try:
            with op.open(base + "/healthz", timeout=2) as r:
                if r.status == 200:
                    break
        except Exception:
            pass
        time.sleep(0.3)
    return p, base, op


def call(op, base, path, body=None, method="GET"):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(base + path, data=data, method=method)
    if data:
        req.add_header("Content-Type", "application/json")
    try:
        with op.open(req, timeout=30) as r:
            return r.status, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()


def gen_scripts(base, op, outdir):
    user = "oc_%d" % int(time.time() * 1000 % 10 ** 9)
    call(op, base, "/api/auth/register", {"username": user, "password": "Oc_pw_20261234"}, "POST")
    call(op, base, "/api/auth/login", {"username": user, "password": "Oc_pw_20261234"}, "POST")
    st, b = call(op, base, "/api/keys", {"name": "接入复验"}, "POST")
    if st != 200:
        raise SystemExit("建密钥失败：%s %s" % (st, b[:200]))
    kid = json.loads(b)["key"]["id"]
    out = {}
    for target, model, ostag, name in (("workbuddy", "glm-5.3", "windows", "wb.bat"),
                                       ("ccswitch", "wb-group-work", "unix", "cc.sh")):
        st, b = call(op, base, "/api/keys/%d/onboard" % kid,
                     {"target": target, "model": model, "os": ostag}, "POST")
        if st != 200:
            raise SystemExit("生成 %s 失败：%s %s" % (target, st, b[:200]))
        d = json.loads(b)
        p = os.path.join(outdir, name)
        with open(p, "w", encoding="utf-8", newline=("\r\n" if ostag == "windows" else "\n")) as f:
            f.write(d.get("script", ""))
        out[target] = p
    return out


def run_bat(bat, home, env_extra=None):
    env = dict(os.environ)
    env["USERPROFILE"] = home
    env.update(env_extra or {})
    return subprocess.run('"%s" < nul' % bat, shell=True, env=env,
                          capture_output=True, timeout=180)


def find_bash():
    for c in ("bash", r"D:/work/workbuddy-data/.workbuddy/binaries/PortableGit/versions/1.3.0/bin/bash.exe"):
        if c.startswith("D:") and os.path.exists(c):
            return c
    return "bash"


def run_sh(sh, home):
    os.makedirs(os.path.join(home, "bin"), exist_ok=True)
    wrapper = os.path.join(home, "bin", "python3")
    with open(wrapper, "w", encoding="utf-8", newline="\n") as f:
        f.write('#!/bin/sh\nexec "%s" "$@"\n' % sys.executable.replace("\\", "/"))
    os.chmod(wrapper, 0o755)
    env = dict(os.environ)
    # 路径一律转正斜杠：bash 会把反斜杠当转义，`rm -- "$0"` 就删不掉自己
    # （第一版忘了转，出现「脚本已自删」假失败 —— 产品没问题，是测试传参的锅）。
    env["HOME"] = home.replace("\\", "/")
    env["USERPROFILE"] = home
    env["PATH"] = os.path.join(home, "bin").replace("\\", "/") + ":" + env.get("PATH", "").replace("\\", "/")
    return subprocess.run([find_bash(), sh.replace("\\", "/")], env=env,
                          capture_output=True, timeout=180)


class Report:
    def __init__(self):
        self.fail = 0

    def check(self, name, cond, extra=""):
        print(("  [ok] " if cond else "  [FAIL] ") + name + ("  " + extra if extra else ""))
        if not cond:
            self.fail += 1


def scenario_workbuddy_ok(rep, bat, home):
    print("\n=== 1. workbuddy · 成功 ===")
    dest = os.path.join(home, ".workbuddy", "models.json")
    os.makedirs(os.path.dirname(dest), exist_ok=True)
    origin = [{"id": "other-model", "name": "保留我", "url": "http://x/v1"},
              {"id": "glm-5.3", "name": "旧条目-应被替换", "url": "http://old/v1"}]
    json.dump(origin, open(dest, "w", encoding="utf-8"), ensure_ascii=False)
    work = os.path.join(home, "run.bat")
    shutil.copyfile(bat, work)
    r = run_bat(work, home)
    out = r.stdout.decode("utf-8", "replace")
    print("  " + out.strip().replace("\n", "\n  ")[:500])
    rep.check("退出码 0", r.returncode == 0, "rc=%d" % r.returncode)
    rep.check("提示的数量与实际一致", "当前共 2 个模型" in out, "")
    data = json.load(open(dest, encoding="utf-8-sig"))
    ids = [d.get("id") for d in data]
    rep.check("写入后是数组", isinstance(data, list))
    rep.check("同名条目被替换（不重复）", ids.count("glm-5.3") == 1, str(ids))
    rep.check("其它条目保留", "other-model" in ids, str(ids))
    new = next(d for d in data if d.get("id") == "glm-5.3")
    rep.check("用了新 url", "old" not in str(new.get("url")), str(new.get("url")))
    rep.check("apiKey 已写入", str(new.get("apiKey", "")).startswith("sk-wb-"))
    bak = dest + ".wb2api.bak"
    rep.check("备份存在", os.path.exists(bak))
    if os.path.exists(bak):
        rep.check("备份=原内容", json.load(open(bak, encoding="utf-8-sig")) == origin)
    rep.check("脚本已自删", not os.path.exists(work))


def scenario_workbuddy_fail(rep, bat, home):
    print("\n=== 2. workbuddy · 失败（原配置是坏 JSON） ===")
    dest = os.path.join(home, ".workbuddy", "models.json")
    os.makedirs(os.path.dirname(dest), exist_ok=True)
    bad = '[{"id":"broken",'
    open(dest, "w", encoding="utf-8").write(bad)
    work = os.path.join(home, "run_fail.bat")
    shutil.copyfile(bat, work)
    r = run_bat(work, home, {"WB_NO_PAUSE": "1"})
    out = r.stdout.decode("utf-8", "replace")
    print("  " + out.strip().replace("\n", "\n  ")[:400])
    rep.check("退出码非 0", r.returncode != 0, "rc=%d" % r.returncode)
    rep.check("有 [FAILED] 提示", "[FAILED]" in out)
    rep.check("提示脚本被保留", "[KEPT]" in out)
    rep.check("脚本仍在（可重试）", os.path.exists(work))
    rep.check("坏配置没被覆盖", open(dest, encoding="utf-8").read() == bad)
    rep.check("WB_NO_PAUSE 生效（无 pause）", "Press any key" not in out)


def scenario_ccswitch(rep, sh, home):
    print("\n=== 3. ccswitch ===")
    dest = os.path.join(home, ".claude", "settings.json")
    os.makedirs(os.path.dirname(dest), exist_ok=True)
    origin = {"permissions": {"allow": ["Bash(ls)"]},
              "env": {"ANTHROPIC_MODEL": "old-model", "OTHER": "keep-me"}}
    json.dump(origin, open(dest, "w", encoding="utf-8"), ensure_ascii=False, indent=2)
    work = os.path.join(home, "run.sh")
    shutil.copyfile(sh, work)
    r = run_sh(work, home)
    out = r.stdout.decode("utf-8", "replace")
    print("  " + out.strip().replace("\n", "\n  ")[:400])
    rep.check("退出码 0", r.returncode == 0, "rc=%d" % r.returncode)
    cur = json.load(open(dest, encoding="utf-8"))
    e = cur.get("env") or {}
    base = e.get("ANTHROPIC_BASE_URL", "")
    rep.check("BASE_URL 是 origin（不带 /v1）", "/v1" not in base, base)
    rep.check("AUTH_TOKEN 已写入", str(e.get("ANTHROPIC_AUTH_TOKEN", "")).startswith("sk-wb-"))
    rep.check("已有 env 键保留", e.get("ANTHROPIC_MODEL") == "old-model" and e.get("OTHER") == "keep-me")
    rep.check("其他顶层字段保留", cur.get("permissions", {}).get("allow") == ["Bash(ls)"])
    rep.check("备份存在", os.path.exists(dest + ".wb2api.bak"))
    rep.check("脚本已自删", not os.path.exists(work))


def main():
    if not os.path.exists(EXE):
        raise SystemExit("找不到 %s（先 preflight --build-only）" % EXE)
    shutil.rmtree(WORK, ignore_errors=True)
    os.makedirs(WORK, exist_ok=True)
    port = free_port()
    srv_home = os.path.join(WORK, "srv")
    os.makedirs(srv_home, exist_ok=True)
    proc, base, op = start_server(port, srv_home)
    rep = Report()
    try:
        scripts = gen_scripts(base, op, WORK)
        scenario_workbuddy_ok(rep, scripts["workbuddy"], os.path.join(WORK, "home1"))
        scenario_workbuddy_fail(rep, scripts["workbuddy"], os.path.join(WORK, "home2"))
        scenario_ccswitch(rep, scripts["ccswitch"], os.path.join(WORK, "home3"))
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=10)
        except Exception:
            proc.kill()
        if not KEEP:
            shutil.rmtree(WORK, ignore_errors=True)
        else:
            print("\n保留工作目录：%s" % WORK)
    print("\nRESULT %s（失败 %d 项）" % ("PASS" if rep.fail == 0 else "FAIL", rep.fail))
    sys.exit(0 if rep.fail == 0 else 1)


if __name__ == "__main__":
    main()
