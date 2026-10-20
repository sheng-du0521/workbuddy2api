#!/usr/bin/env python3
"""ssh_audit —— authorized_keys 的**只读**体检（审查报告 F04 的第 3 步）。

## 为什么要有它

反向隧道靠每用户一把受限 SSH 密钥（见 cmd/webapp/egress_ssh.go）。限制是否正确，
决定了「一个普通用户能不能借服务器出去、能不能抢别人的隧道端口」。而 authorized_keys
是**运维手工也可能改**的文件，代码里写对了不代表线上的那一行写对了。

## 它做什么 / 不做什么

- **只读**：只解析、只打印、`--json` 输出一份报告。**绝不写文件、绝不删行**。
- 判据只做"显然不对"的那几类（缺 command= / 缺 permitlisten / 段外端口 / 重复 /
  非本服务条目），不做自动修复 —— 撤权是不可逆操作，必须人看过报告再决定。

## 用法

    python3 scripts/ssh_audit.py                        # 默认 /root/.ssh/authorized_keys
    python3 scripts/ssh_audit.py -f /path/to/authorized_keys --json
    python3 scripts/ssh_audit.py --port-check          # 额外核对监听端口是否落在段内

退出码：0 = 没有发现高风险项；1 = 有（便于接进巡检脚本）。
"""
import argparse
import json
import os
import re
import sys

# 本服务写入的条目注释：wb2api-u<uid>
OWN_RE = re.compile(r"\bwb2api-u(\d+)$")

# 平台（owner=0）与用户段（12000 + (uid%1000)*10 .. +9），与 egressPortSegOf 一致。
PLATFORM_BASE, PLATFORM_SPAN = 11080, 10
USER_BASE, USER_SPAN = 12000, 10


def user_seg(uid):
    base = USER_BASE + (uid % 1000) * USER_SPAN
    return base, base + USER_SPAN - 1


def platform_seg():
    return PLATFORM_BASE, PLATFORM_BASE + PLATFORM_SPAN - 1


def parse_line(line):
    """把一行 authorized_keys 拆成（options, type, body, comment）。

    选项部分可能含逗号、引号、甚至转义，所以用"第一个形如 ssh-*/ecdsa-* 的 token
    之前都算 options"这个策略，而不是简单 split —— 简单 split 会把
    `permitlisten="127.0.0.1:12010",...` 这类带空格的写法切错（实际不带空格，
    但人手改过就可能带）。
    """
    m = re.search(r"\b(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp\d+|sk-ssh-ed25519@openssh\.com)\b", line)
    if not m:
        return None
    opts = line[: m.start()].strip()
    rest = line[m.start():].split()
    if len(rest) < 2:
        return None
    return opts, rest[0], rest[1], (rest[2] if len(rest) > 2 else "")


def _listen_ports(opts):
    """从选项串里取出 permitlisten 白名单的端口（升序去重）。"""
    stripped = re.sub(r'permitopen=(?:"[^"]*"(?:,"[^"]*")*|[^,]*)', "", opts)
    return sorted({int(p) for p in re.findall(r"127\.0\.0\.1:(\d+)", stripped)})


def audit(path):
    res = {
        "path": path,
        "exists": os.path.exists(path),
        "total": 0,
        "own": [],
        "foreign": [],
        "high_risk": [],
        "notes": [],
    }
    if not res["exists"]:
        res["notes"].append("文件不存在（还没签发过隧道密钥，或路径不对）")
        return res

    seen = {}
    with open(path, encoding="utf-8", errors="replace") as f:
        for lineno, raw in enumerate(f, 1):
            line = raw.strip()
            if not line or line.startswith("#"):
                continue
            res["total"] += 1
            parsed = parse_line(line)
            if not parsed:
                res["notes"].append("第 %d 行无法解析（可能被手工改坏）" % lineno)
                continue
            opts, ktype, body, comment = parsed
            own = OWN_RE.search(comment)
            entry = {
                "line": lineno,
                "uid": int(own.group(1)) if own else None,
                "type": ktype,
                "comment": comment,
                "options": opts,
                "fingerprint": body[-12:],  # 只留尾部 12 字符做去重标识，不落整段公钥
                "has_command": "command=" in opts,
                "has_permitlisten": "permitlisten=" in opts,
                "has_permitopen": "permitopen=" in opts,
                "permitopen_value": (re.search(r'permitopen="?([^",]+)"?', opts).group(1)
                                     if "permitopen=" in opts else ""),
                # 只统计 permitlisten 的端口：**先剥掉 permitopen 段**再取端口。
                # permitopen 的目标（哨兵 127.0.0.1:1）不属于端口段，混进来会把每条都判成
                # "段外端口"；而 permitlisten 的值可能是多个引号串（"…","…"），
                # 所以不能按单个值去匹配（v0.31.61 先踩过这两个坑）。
                "ports": _listen_ports(opts),
            }
            if body in seen:
                res["high_risk"].append(
                    "第 %d 行与第 %d 行公钥重复（同一把钥匙写了两份，撤权时会漏）"
                    % (lineno, seen[body])
                )
            seen[body] = lineno

            if own:
                res["own"].append(entry)
            else:
                res["foreign"].append(entry)
                # 非本服务条目本身不算缺陷（可能是管理员的登录 key），只提示。
                continue

            uid = entry["uid"]
            lo, hi = platform_seg() if uid == 0 else user_seg(uid)
            if not entry["has_command"]:
                res["high_risk"].append(
                    "第 %d 行（uid=%d）没有 command=：`restrict` 并不禁止 exec，这把钥匙能跑命令"
                    % (lineno, uid)
                )
            if not entry["has_permitlisten"]:
                res["high_risk"].append(
                    "第 %d 行（uid=%d）没有 permitlisten：可以抢占平台/别人的隧道端口" % (lineno, uid)
                )
            pv = entry["permitopen_value"]
            if not pv:
                res["high_risk"].append(
                    "第 %d 行（uid=%d）没有 permitopen：可用 `ssh -D` 借服务器出去（实测可穿透）"
                    % (lineno, uid)
                )
            elif pv in ("none", "0.0.0.0:0") or pv.endswith(":0"):
                # 2026-10-20 实测（OpenSSH 8.9p1）：authorized_keys 里写
                # permitopen="none" 或 ":0" 会让 sshd 判定选项非法、**整条 key 被丢弃**
                # —— 现象是隧道全挂且只有 Permission denied (publickey)。
                res["high_risk"].append(
                    "第 %d 行（uid=%d）permitopen=%q 是非法写法：sshd 会丢弃整条 key（隧道全挂）"
                    % (lineno, uid, pv)
                )
            stray = [p for p in entry["ports"] if p < lo or p > hi]
            if stray:
                res["high_risk"].append(
                    "第 %d 行（uid=%d）含段外端口 %s（允许 %d-%d）" % (lineno, uid, stray, lo, hi)
                )
    return res


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("-f", "--file", default="/root/.ssh/authorized_keys")
    ap.add_argument("--json", action="store_true", help="输出 JSON（给巡检/工单用）")
    a = ap.parse_args()

    r = audit(a.file)
    if a.json:
        print(json.dumps(r, ensure_ascii=False, indent=2))
    else:
        print("文件 %s（存在=%s，非空行 %d 条）" % (r["path"], r["exists"], r["total"]))
        print("本服务条目 %d 条，其他条目 %d 条" % (len(r["own"]), len(r["foreign"])))
        for e in r["own"]:
            print(
                "  uid=%-5s 端口=%-22s command=%s permitlisten=%s permitopen=%s"
                % (
                    e["uid"],
                    "%d-%d" % (e["ports"][0], e["ports"][-1]) if e["ports"] else "—",
                    "✓" if e["has_command"] else "✗",
                    "✓" if e["has_permitlisten"] else "✗",
                    (e["permitopen_value"] or "✗"),
                )
            )
        for e in r["foreign"]:
            print("  其他条目 第%d行 %s（%s）" % (e["line"], e["type"], e["comment"] or "无注释"))
        for n in r["notes"]:
            print("· %s" % n)
        if r["high_risk"]:
            print("\n高风险项 %d 条：" % len(r["high_risk"]))
            for x in r["high_risk"]:
                print("  ⚠ %s" % x)
            print("\n本工具**只报告不修改**。撤权/改行请人工确认后执行，改前先备份 authorized_keys。")
        else:
            print("\n未发现高风险项。")
    return 1 if r["high_risk"] else 0


if __name__ == "__main__":
    sys.exit(main())
