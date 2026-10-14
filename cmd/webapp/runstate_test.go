// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import "testing"

// TestAccountRunStatePriority 运行态判定的优先级（R9 的核心口径）。
//
// 顺序不是随便定的：**先"不可自动恢复"，后"可自动恢复"**。
// 用例里刻意把多个条件同时置真，验证取的是优先级更高的那个。
func TestAccountRunStatePriority(t *testing.T) {
	cases := []struct {
		name                         string
		disabled, manualOff, cooling bool
		reason, coolKind, egressErr  string
		want                         string
	}{
		// manual_disabled 必须盖过一切：那是人的决定，等不来。
		{"手动停用盖过系统禁用", true, true, true, "余额不足", "hard_credit", "", RunStateManualDisabled},
		{"手动停用盖过风控冷却", false, true, true, "11140 request illegal", "soft_rate", "", RunStateManualDisabled},
		// 系统禁用次之。
		{"系统禁用盖过出口错误", true, false, false, "", "", "代理不可用", RunStateDisabled},
		// 出口错误排在冷却之前：这类账号**看起来完全健康**，却会从本机 IP 裸奔出去。
		{"出口错误盖过冷却", false, false, true, "429 rate limit", "soft_rate", "代理不可用", RunStateEgressError},
		// 需重登 > 额度耗尽 > 风控 > 限流 > 一般冷却。
		{"需重登盖过冷却", false, false, true, "账号故障（14017），需重登", "soft_rate", "", RunStateRelogin},
		{"会话失效算需重登", false, false, true, "Offline user session not found 12153", "soft_rate", "", RunStateRelogin},
		{"额度耗尽盖过风控", false, false, true, "积分不足（无法消耗积分）", "hard_credit", "", RunStateExhausted},
		// 风控冷却 vs 限流冷却：两者处置方向相反，必须分开（本功能的主要动机）。
		{"11140 是风控冷却", false, false, true, "11140 request illegal（累计 1/3，未超阈值仅软冷却）", "soft_rate", "", RunStateRiskCooling},
		{"WAF 403 是风控冷却", false, false, true, "waf 403 block", "soft_rate", "", RunStateRiskCooling},
		{"429 是限流冷却而非风控", false, false, true, "429 rate limit", "soft_rate", "", RunStateRateLimited},
		{"6004 是限流冷却", false, false, true, "6004 model rate limit", "soft_rate", "", RunStateRateLimited},
		{"普通冷却", false, false, true, "upstream 404", "soft_rate", "", RunStateCooling},
		{"正常可用", false, false, false, "", "", "", RunStateOnline},
	}
	for _, c := range cases {
		got := accountRunStateOf(c.disabled, c.manualOff, c.cooling, c.reason, c.coolKind, c.egressErr)
		if got != c.want {
			t.Errorf("%s：accountRunStateOf = %q，期望 %q（reason=%q kind=%q）",
				c.name, got, c.want, c.reason, c.coolKind)
		}
	}
}

// TestAccountRunStateExhaustedNeedsHardKind 硬冷却口径必须与 pool 的 CoolKind.String() 对齐。
//
// 踩过的坑：CoolKind 的 .String() 返回 "hard_credit"（不是 "hard"），
// 写成 "hard" 会让"额度耗尽"永远识别不出来 —— 这类"字面量对不上"的静默失效最难查。
func TestAccountRunStateExhaustedNeedsHardKind(t *testing.T) {
	if got := accountRunStateOf(false, false, true, "", "hard_credit", ""); got != RunStateExhausted {
		t.Fatalf("hard_credit 应判为 exhausted，实得 %q", got)
	}
	// 文案兜底：即使 kind 缺失，带"余额/积分不足"文案也要能判出来。
	if got := accountRunStateOf(false, false, true, "余额不足", "", ""); got != RunStateExhausted {
		t.Fatalf("余额文案应判为 exhausted，实得 %q", got)
	}
}
