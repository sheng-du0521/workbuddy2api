#!/usr/bin/env python3
"""preflight —— 发布前/CI 的**同一条**检查链（v0.31.63，审查 F06）。

## 为什么要有它

审查 F06 的原话是"清单超前于资产、发布未依赖 CI"。根因不是"CI 配置写错了"，
而是**发布前的检查散落在人的记忆里**：这次记得跑 gofmt 与签名校验，下次换个时间就漏。
只要检查没有一处可执行的实现，CI 和"人在发布机上敲的几条命令"就一定会漂移 ——
它们各自跑各自的子集，谁也不知道少了哪一步。

所以这里把顺序固化成一份可执行清单（顺序本身就是知识）：

    格式 → 静态检查 → 单测 → 前端语法/交互 → 双平台构建 → relsign 构建
         → 清单一致性（版本一致 + 签名 + 本地产物 sha/size）

`.github/workflows/ci.yml` 与发布机都只调它，**没有第二条路径**。

## 用法

    # 发版的标准顺序（**顺序本身是知识**，别打乱）：
    #   1. 改 cmd/webapp/main.go 的 const version
    #   2. preflight.py --build-only          只构建（此时清单还没跟上，跑测试必然红）
    #   3. make_manifest.py --key <私钥>      用**刚构建的字节**盖清单章并签名
    #   4. preflight.py --expect-manifest     全量检查 + 要求清单 sha == 本次构建字节

    python3 scripts/preflight.py                # 全量（清单已盖章时）
    python3 scripts/preflight.py --fast         # 跳过 go test（改文档/前端时用）
    python3 scripts/preflight.py --build-only   # 只构建

退出码 0 = 可以发；非 0 = 第几步挂了，看上面最后一行。
"""
import argparse
import os
import shutil
import subprocess
import sys
import time

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DIST = os.path.join(ROOT, "dist")


def run(step, cmd, env=None, allow_fail=False):
    """跑一步，失败立刻给出**它自己的输出**（不要让人再去翻日志）。"""
    t0 = time.time()
    print("\n=== %s ===" % step)
    print("$ %s" % " ".join(cmd))
    e = dict(os.environ)
    if env:
        e.update(env)
    p = subprocess.run(cmd, cwd=ROOT, env=e, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                       text=True, encoding="utf-8", errors="replace")
    out = (p.stdout or "").strip()
    if out:
        # 只回显尾部：go test 全量输出几百行，真正有用的是最后几行。
        lines = out.splitlines()
        print("\n".join(lines[-25:]))
    print("  -> exit=%d  %.1fs" % (p.returncode, time.time() - t0))
    if p.returncode != 0 and not allow_fail:
        print("\nFAIL：这一步没过，后面的不用跑了。")
        sys.exit(p.returncode)
    return p.returncode


def which(name):
    p = shutil.which(name)
    if not p:
        print("缺少可执行文件：%s（发布机需要它；CI 由 setup-go / setup-node 提供）" % name)
        sys.exit(2)
    return p


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--fast", action="store_true", help="跳过 go test")
    ap.add_argument("--build-only", action="store_true", help="只构建，不跑任何检查")
    ap.add_argument("--no-node", action="store_true", help="跳过前端检查（没装 node 时）")
    ap.add_argument("--expect-manifest", action="store_true",
                    help="要求清单声明的 sha/size 就是本次构建的字节（发布机在 make_manifest 之后用）")
    a = ap.parse_args()

    go = which("go")
    # gofmt 与 go 同目录（`go fmt -l` 不存在，`go fmt` 会**改写文件** ——
    # 检查步骤里改写文件等于"检查结果不落地"，所以必须用 gofmt 本体）。
    gofmt = os.path.join(os.path.dirname(go), "gofmt" + (".exe" if os.name == "nt" else ""))
    if not os.path.exists(gofmt):
        print("找不到 gofmt（应在 go 同目录）：%s" % gofmt)
        sys.exit(2)
    os.makedirs(DIST, exist_ok=True)
    build_env = {"CGO_ENABLED": "0"}

    if not a.build_only:
        # ① gofmt：只报不写。CI 上写文件等于"检查结果不落地"，改了也白改。
        run("gofmt（应为空输出）", [gofmt, "-l", "./cmd", "./internal"])
        # ② go vet：类型/无用代码/明显错误，比测试更早暴露问题。
        run("go vet", [go, "vet", "./..."])
        # ③ 单测：全量，不 -short（本项目没有依赖外网的稳定用例）。
        if not a.fast:
            run("go test（全量，-count=1 防缓存）", [go, "test", "./...", "-count=1"])

    # ④ 前端：语法 + 关键交互。node 不在时给**明确**提示而不是静默跳过 ——
    #    "没检查"和"检查通过"在发布决策里是两件事。
    if not a.build_only and not a.no_node:
        if shutil.which("node"):
            ui = ["admin.html", "dashboard.html", "index.html"]
            run("前端语法（jscheck）", ["node", "tools/jscheck.mjs"] +
                [os.path.join("cmd", "webapp", "ui", f) for f in ui])
            run("前端交互（uitest）", ["node", "tools/uitest.mjs"])
        else:
            print("\n⚠ 没有 node，跳过前端检查（--no-node 可显式声明这一点）")

    # ⑤ 双平台构建：**两个都要**。只构建 Windows 的话，部署到 Linux 时才会发现编译问题，
    #    而那时已经在发布窗口里了。
    # ⚠️ `-buildvcs=false` 不是为了省事，而是让构建**可重复**（v0.31.63 实测）：
    # 默认会嵌入 VCS 标记，同一份源码在不同 commit/不同机器上构建出**不同字节**，
    # 于是"清单 sha == 本次构建字节"这条闸门永远为假 —— 那它就不是闸门。
    # 关掉之后同一份源码 + 同一 Go 版本 → 逐字节相同（本机两次构建已验）。
    # 代价：二进制里没有 commit 标记；来源由**签名清单**背书（sha256 是清单签过名的字段）。
    run("构建 linux/amd64", [go, "build", "-trimpath", "-buildvcs=false", "-ldflags", "-s -w",
                          "-o", os.path.join("dist", "webapp-linux"), "./cmd/webapp"],
        env={**build_env, "GOOS": "linux", "GOARCH": "amd64"})
    run("构建 windows/amd64（发布资产）", [go, "build", "-trimpath", "-buildvcs=false", "-ldflags", "-s -w",
                                "-o", os.path.join("dist", "webapp.exe"), "./cmd/webapp"],
        env={**build_env, "GOOS": "windows", "GOARCH": "amd64"})
    relsign_name = "relsign.exe" if os.name == "nt" else "relsign"
    # relsign 用**本机**平台构建（GOOS/GOARCH 不覆盖），发布机是 Windows、CI 是 Linux。
    run("构建 relsign", [go, "build", "-o", os.path.join("dist", relsign_name), "./cmd/relsign"],
        env=build_env)

    # ⑥ 清单一致性（版本一致 + 签名可用）。**sha 比对是否算失败，取决于你是谁**：
    #
    #    - 发布机：先 make_manifest 盖章（用本次构建的 sha 写清单），再跑本脚本 → 必须一致；
    #    - CI：从 tag 重新构建，虽然已用 -buildvcs=false 追求可重复，但 Go 版本/工具链
    #      差异仍可能让字节不同 —— 那不是"资产错了"，而是"CI 的构建不是发布的那个构建"。
    #      所以默认只**警告**；要当闸门用就显式 `--expect-manifest`。
    #
    # 真正防"清单超前于资产"的闸门在发布之后：`verify_release.py`（联网，下载已发布的
    # 资产逐字节比对）。那一步 CI 也跑（见 .github/workflows/release.yml）。
    cmd = [sys.executable, os.path.join("scripts", "verify_release.py"),
           "--local", "--offline", "--relsign", os.path.join("dist", relsign_name)]
    rc = run("清单一致性（离线）", cmd, allow_fail=not a.expect_manifest)
    if rc != 0:
        print("\n⚠ 清单声明的 sha 与本次构建不同。若刚 make_manifest 过，说明盖章没生效；")
        print("  若这是 CI 从 tag 重建，属于工具链差异 —— 以发布后的联网校验为准。")
        print("  要把它当闸门：python3 scripts/preflight.py --expect-manifest")

    print("\nPREFLIGHT OK：可以发布（下一步：push2 → release → verify_release 全项）")


if __name__ == "__main__":
    main()
