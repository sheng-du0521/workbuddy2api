// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// runstate.go 账号运行态（R9，v0.26.1）：把散在布尔位与自由文案里的处境收敛成一个**显式枚举**。
//
// ## 为什么需要
//
// 此前账号「为什么不在池里」只能靠三个布尔位（disabled / manual_off / cooling）加一段
// 自由文案（reason）自己拼。运维看到的典型困惑是：
//
//   - 「冷却中」既可能是 429 限流（一分钟后自己好），也可能是 11140 风控（说明出口或
//     请求形态有问题，等下去不会好）—— 两者的**处置方向相反**，却显示成同一个东西；
//   - 「额度耗尽」与「需要重新登录」都被压成 disabled/cooling，无法一眼分辨
//     「等一等 / 充点钱」与「必须人工重登」。
//
// 同类项目（AgentHub）用的是五态状态机（online / cooling / exhausted / relogin / disabled）。
// 本实现按我们自己的状态位口径扩到九态：多出来的四态不是凑数 —— `risk_cooling`
// （风控冷却，与普通限流分开）、`egress_error`（出口配置坏，最容易被忽略且会裸奔）、
// `manual_disabled`（人的决定，必须盖过系统状态）、`rate_limited`（限流，可自动恢复）
// 每一种的处置动作都不同。
//
// 这是一个**纯函数**：输入是状态位与文案，输出是枚举。改判定不必起服务。
package main

import "strings"

// 运行态枚举（供面板与告警使用；值稳定，前端按字符串匹配）。
const (
	RunStateOnline  = "online"  // 正常可用
	RunStateCooling = "cooling" // 一般冷却（自动恢复）
	// RunStateRateLimited 限流冷却（**账号级**：429 / WAF 403 等）。
	//
	// ⚠️ 注意 6004（模型级限额）**不落在这里**（v0.26.4 起）：它只写 (账号, 模型)
	// 的独立冷却表，账号级仍然健康 → 该号的 run_state 保持 online。
	// 这不是遗漏而是正确口径：run_state 回答的是"这个号整体能不能用"，
	// 而"某个模型被限"是**独立的维度**，由账号视图的 limited_models 表达
	// （见 main.go 的 limitedModelView）。两者都不可用时才该降级 run_state。
	RunStateRateLimited    = "rate_limited"
	RunStateRiskCooling    = "risk_cooling"    // 风控冷却（11140 / 11128 / WAF）—— 等下去不会好
	RunStateExhausted      = "exhausted"       // 额度/积分耗尽（等到次日或充值）
	RunStateRelogin        = "relogin"         // 需重新登录 / 会话失效（必须人工）
	RunStateEgressError    = "egress_error"    // 出口配置不可用（不修会从本机 IP 裸奔）
	RunStateDisabled       = "disabled"        // 系统判定不可用
	RunStateManualDisabled = "manual_disabled" // 运维手动停用
)

// accountRunStateOf 由状态位与自由文案推导运行态。
//
// 判定顺序（**先"不可自动恢复"，后"可自动恢复"**）：
//
//	manual_disabled > disabled > egress_error > relogin > exhausted
//	> risk_cooling > rate_limited > cooling > online
//
// 为什么 manual_disabled 排第一：那是**人的决定**，任何系统状态都不该把它盖住 ——
// 否则运维会看到「冷却中」而去等它自动恢复，实际上它永远不会回来。
// 为什么 egress_error 排在冷却类之前：出口配置坏掉的账号**看起来完全健康**
// （凭证有效、积分充足），但它一被选中就会从本机 IP 出去 —— 这是最该被立刻看见的一类。
func accountRunStateOf(disabled, manualDisabled, cooling bool, reason, coolKind string, egressErr string) string {
	switch {
	case manualDisabled:
		return RunStateManualDisabled
	case disabled:
		return RunStateDisabled
	case strings.TrimSpace(egressErr) != "":
		return RunStateEgressError
	case isReloginReason(reason):
		return RunStateRelogin
	case isExhaustedReason(reason, coolKind):
		return RunStateExhausted
	case isRiskReason(reason):
		return RunStateRiskCooling
	case isRateLimitReason(reason):
		return RunStateRateLimited
	case cooling:
		return RunStateCooling
	default:
		return RunStateOnline
	}
}

// isReloginReason 会话失效 / 试用未激活 —— 都要人工动作，等不来。
func isReloginReason(reason string) bool {
	r := strings.ToLower(reason)
	for _, k := range []string{"14017", "12153", "重登", "重新登录", "session", "trial"} {
		if strings.Contains(r, k) {
			return true
		}
	}
	return false
}

// isExhaustedReason 额度/积分耗尽 —— 要么等到次日 04:00，要么充值，不是故障。
func isExhaustedReason(reason, coolKind string) bool {
	// CoolKind.String() 的硬冷却口径是 "hard_credit"（见 pool/entry.go），不是 "hard"。
	if coolKind == "hard_credit" {
		return true
	}
	r := strings.ToLower(reason)
	for _, k := range []string{"余额", "积分不足", "额度耗尽", "402", "quota"} {
		if strings.Contains(r, k) {
			return true
		}
	}
	return false
}

// isRiskReason 风控类 —— **关键区分点**：这类冷却等下去不会好，必须先看出口与请求形态。
func isRiskReason(reason string) bool {
	r := strings.ToLower(reason)
	for _, k := range []string{"11140", "11128", "风控", "request illegal", "waf", "指纹", "非法"} {
		if strings.Contains(r, k) {
			return true
		}
	}
	return false
}

// isRateLimitReason 限流类 —— 可自动恢复，但应看软冷却退避（soft_streak）决定要不要换个出口。
//
// 这里收 "6004" 是为**账号级** reason 兜底：v0.26.3 起正常的 6004 不再写账号级 reason
// （只写模型表），所以命中它的情形只剩两种 —— 存量旧状态（由 pool 启动自愈清除）与
// 将来若有代码误把 6004 的 reason 写进账号级。保留它不会误伤：真正的模型级限额
// 根本不进这个判定（accountRunStateOf 先看 v.Cooling，模型级限额时它是 false）。
func isRateLimitReason(reason string) bool {
	r := strings.ToLower(reason)
	for _, k := range []string{"429", "6004", "rate limit", "限流", "soft"} {
		if strings.Contains(r, k) {
			return true
		}
	}
	return false
}
