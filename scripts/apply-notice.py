#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""为 cmd/ 与 internal/ 下的全部 .go 文件补上「禁止商用倒卖」声明头。

用法：
    python scripts/apply-notice.py            # 补写（幂等，可反复跑）
    python scripts/apply-notice.py --check    # 只检查：缺声明就列出并以非零码退出

为什么声明必须**逐文件**存在：
    单看某一个 .go 文件的人（被复制走、被摘出来贴到博客/教程里）也应该看到约束，
    而不是只有克隆整个仓库、并且主动点开 NOTICE 才知道。
    所以新增文件时跑一次本脚本即可，不需要手工复制粘贴。

为什么要脚本而不是手工：
    363 个文件手工加头 = 必然漏。而且它有两条容易写错的规则（见下），
    写成脚本才能保证一致。

两条规则：
    1. **build 约束必须在文件最顶部**（`//go:build` / `// +build`），
       声明块只能插在它**之后**，否则约束会失效 —— 那会静默改变编译结果。
    2. 声明块与后续内容之间留一个空行：原文件若本就有 package 文档注释
       （紧邻 package 的注释块），不留空行会把两条注释粘成一体，
       污染 Go 的 package doc。
"""
import os
import sys

MARKER = "禁止商用倒卖"
BLOCK_LINES = [
    "// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）",
    "// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。",
    "// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。",
]
BLOCK = "\n".join(BLOCK_LINES) + "\n\n"

ROOTS = ["cmd", "internal"]
SKIP_DIRS = {"node_modules", ".git", "vendor"}


def go_files():
    for root in ROOTS:
        for dirpath, dirnames, filenames in os.walk(root):
            dirnames[:] = [d for d in dirnames if d not in SKIP_DIRS]
            for fn in sorted(filenames):
                if fn.endswith(".go"):
                    yield os.path.join(dirpath, fn)


def insert_at(lines):
    """返回声明块应插入的行号（跳过 build 约束行）。"""
    i = 0
    if lines and lines[0].startswith("//go:build"):
        i = 1
        while i < len(lines) and lines[i].startswith("// +build"):
            i += 1
        while i < len(lines) and lines[i].strip() == "":
            i += 1
    return i


def apply(path):
    """返回 True 表示做过修改。"""
    with open(path, encoding="utf-8") as f:
        src = f.read()
    # 只看文件头部，避免把正文里恰好提到该词的注释误判成"已加过"。
    if MARKER in src[:800]:
        return False

    lines = src.split("\n")
    at = insert_at(lines)
    head = "\n".join(lines[:at])
    if at > 0:
        head = head.rstrip("\n") + "\n\n"  # build 约束后必须空一行
    with open(path, "w", encoding="utf-8", newline="") as f:
        f.write(head + BLOCK + "\n".join(lines[at:]))
    return True


def main():
    check = "--check" in sys.argv
    changed, missing = [], []
    for path in go_files():
        with open(path, encoding="utf-8") as f:
            head = f.read(800)
        if MARKER in head:
            continue
        if check:
            missing.append(path)
        elif apply(path):
            changed.append(path)

    if check:
        for p in missing:
            print("缺少声明: %s" % p)
        print("检查 %d 个文件，缺少声明 %d 个" % (sum(1 for _ in go_files()), len(missing)))
        return 1 if missing else 0

    print("已为 %d 个文件补写声明" % len(changed))
    return 0


if __name__ == "__main__":
    sys.exit(main())
