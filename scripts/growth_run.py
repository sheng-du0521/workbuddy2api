#!/usr/bin/env python3
"""growth_run —— 供 Go 调度器调用的成长任务执行入口（凭证经 stdin 传入）。

与 task_runner.py 的唯一差别：**凭证不从 auths/ 目录读**，而是由 Go 侧从 SQLite
解密后经 stdin 以 JSON 传入。Web 版账号存库且加密，落盘成 auths/*.json 既多余又
增加泄露面；stdin 传递让凭证只存在于进程内存与管道中。

复用 task_runner 的全部映射逻辑（MAPPING / 四类指纹 / 逐任务事件链 / claim web 域
降级 / 幂等），本文件只做三件事：读凭证 → 按 phase 选任务子集 → 输出机器可读结果。

用法（Go 侧构造 JSON 走 stdin）：
  echo '{"uid":"..","token":"..","domain":"..","nick":"..","realm":"cn"}' \
    | python3 growth_run.py --phase zero  --yes
  ... | python3 growth_run.py --phase talk  --yes     # 需真实对话（消耗额度）
  ... | python3 growth_run.py --phase claim --yes     # 只领奖（幂等，每日增量用）
  ... | python3 growth_run.py --phase all   --yes     # 零消耗 + 对话

phase 划分（按「是否消耗账号额度」切分，这是新号编排的关键）：
  zero  仅行为上报类，不消耗 credits —— 新号先用它攒积分
  talk  需真实对话类，会消耗 credits/额度
  claim 只领奖不点亮（等价 --only-claim，服务端幂等）
  all   zero + talk

退出码：0 成功（含全部跳过）；2 凭证/参数错误；3 执行中部分失败（仍输出结果）。
结果行：stdout 最后一行形如 `GROWTH_RESULT {"ok":3,...}`，供 Go 侧解析。
"""
import argparse
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import task_runner as tr  # noqa: E402
import task_common as tc  # noqa: E402

# 零对话消耗（行为上报）：不消耗账号 credits，新号可放心先跑。
PHASE_ZERO = [
    "create_canvas",     # 设计画布创建事件组
    "template_5",        # 模板使用事件组 ×5
    "automation_1",      # 定时任务创建成功事件
    "playbook_prompt",   # 灵感案例「做同款」
    "Buddy_App",         # Buddy 应用发现→进入→授权
    "Buddy_App_QQ",      # 企鹅教师助手（与上条共用事件链）
    "Hp_Appearance",     # 主题设置 + 皮肤生效
    "chat_5",            # 对话活跃上报 ×5（是上报，不是真实对话）
    "first_buddy",       # 领养第一只 Buddy（+300c+8e）
    "RichMeow_Chat",     # 桌面端对话事件链（6 事件）
    "Library_read",      # 资料库阅读点击（web 域）
]

# 需真实对话：每条都要发真实 chat，会消耗账号 credits/额度。
# 新号必须先用 zero 攒够积分再跑它，否则额度不足中途失败会留下失败痕迹。
PHASE_TALK = [
    "expert_5",              # 专家召唤+使用 ×5
    "Expert_team_use_3",     # 专家团 ×3
    "skill_1",               # 技能加载
    "Expert_lighthouse",     # 轻量云专家（可免费领一个月轻量服务器）
    "Model_chat_GLM5.2",     # glm-5.2 真实对话 1 条
]

PHASES = {
    "zero": PHASE_ZERO,
    "talk": PHASE_TALK,
    "all": PHASE_ZERO + PHASE_TALK,
}


def emit_result(payload: dict) -> None:
    """输出机器可读结果行（Go 侧按前缀抓取；人类日志照常打到 stdout）。"""
    print("GROWTH_RESULT " + json.dumps(payload, ensure_ascii=False))


def main() -> int:
    ap = argparse.ArgumentParser(description="成长任务执行入口（凭证经 stdin）")
    ap.add_argument("--phase", default="zero", choices=["zero", "talk", "claim", "all"])
    ap.add_argument("--yes", action="store_true", help="真实执行写操作（缺省 dry-run）")
    ap.add_argument("--gap", type=float, default=1.0, help="动作间隔秒数（强制 ≥1）")
    a = ap.parse_args()
    if a.gap < 1.0:
        a.gap = 1.0  # 写操作间隔 ≥1s（防封硬约束，与 task_runner 同口径）

    raw = sys.stdin.read()
    try:
        cred = json.loads(raw)
    except Exception as e:
        print(f"ERR: 读取 stdin 凭证失败: {e}", file=sys.stderr)
        return 2
    if not isinstance(cred, dict) or not cred.get("token") or not cred.get("uid"):
        print("ERR: stdin 需要 JSON 且含 token / uid", file=sys.stderr)
        return 2

    auth = {
        "token": cred.get("token") or "",
        "uid": cred.get("uid") or "",
        "domain": cred.get("domain") or "",
        "nick": cred.get("nick") or "",
        "realm": cred.get("realm") or "cn",
        "chat_base": cred.get("chat_base") or tc.CHAT_BASE,
        "billing_base": cred.get("billing_base") or tc.BILL_BASE,
    }

    # global 账号没有 CN 任务中心：明确跳过、不发任何请求（与 task_runner 同门控）。
    if tc.auth_is_global(auth):
        emit_result({"phase": a.phase, "skipped_reason": "global realm 不适用 CN 任务",
                     "ok": 0, "already": 0, "skip": 1, "fail": 0, "credit": 0, "energy": 0})
        return 0

    only_claim = a.phase == "claim"
    # claim 也**限定在本功能覆盖的 codes 内**，这是踩过坑后的修正：
    # 不限定则会遍历全部任务，而 black_cat（要夜猫窗口 23-08）与
    # Expert_Philanthropy（需真实捐款，服务端校验回执）**永远无法达标**，
    # 于是每次 claim 都报「仍有任务待达标」→ Go 侧据此重置首轮标记 →
    # 该号每轮都被重新拉去跑一遍 zero+talk，白打几十个请求。
    # 限定后 pending 只反映「本功能负责范围内」的真实缺口，重置才有意义。
    only_codes = PHASES["all"] if only_claim else PHASES[a.phase]

    opts = argparse.Namespace(
        gap=a.gap,
        only_claim=only_claim,
        only_codes=only_codes,
        yes=a.yes,
    )

    stats = {"accounts": 0, "total": 0, "ok": 0, "already": 0,
             "skip": 0, "pending": 0, "fail": 0, "credit": 0, "energy": 0}
    mode = "REAL" if a.yes else "DRY-RUN"
    print(f"[growth_run] mode={mode} phase={a.phase} uid={auth['uid'][:8]} gap={a.gap}")
    try:
        tr.process_account(auth, opts, stats)
    except Exception as e:
        # 单账号异常不该让整批挂掉：Go 侧据此把该号记为失败并进冷却。
        print(f"[growth_run] 账号执行异常: {e}")
        emit_result({"phase": a.phase, "ok": stats["ok"], "already": stats["already"],
                     "skip": stats["skip"], "fail": stats["fail"] + 1,
                     "credit": stats["credit"], "energy": stats["energy"],
                     "error": str(e)})
        return 3

    emit_result({"phase": a.phase, "ok": stats["ok"], "already": stats["already"],
                 "skip": stats["skip"], "pending": stats["pending"], "fail": stats["fail"],
                 "credit": stats["credit"], "energy": stats["energy"]})
    return 0


if __name__ == "__main__":
    sys.exit(main())
