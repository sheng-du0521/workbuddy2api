// 模型级冷却的「调度边界」回归测试（v0.31.0）。
//
// 本文件钉住三件容易被悄悄破坏的事：
//  1. (账号, 模型) 冷却不会被**无关的余额同步**清掉 —— 签到 / 健康探测 / 代挂刷新
//     余额都是 `ReenableIfCredits` 的调用者，而它是无条件调用的（含对冷却中的号）。
//  2. 全冷却兜底**不**把请求发给「对本请求模型正在模型级冷却」的账号。
//  3. 人工启用（ReviveDisabled）仍然清模型级冷却 —— 这是刻意的：
//     运维点了「启用」却因为残留模型冷却而"看着启用了、实际不能用"是本仓反复
//     踩过的坑（见 state.go ReviveDisabled 注释）。
package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// TestModelCooldownSurvivesCreditsRefresh 余额同步（签到/探活/代挂刷新）不得清模型级冷却。
//
// 缺陷现场（v0.31.0 修）：ReenableIfCredits → reviveCoolingLocked → clearCoolingLocked
// 把 modelCooldowns 一起置 nil。而 ReenableIfCredits 被
//   - 定时签到（scheduler.checkinFiltered 遍历 Pool.List()，**含冷却中的号**，只在
//     Disabled / 任务关闭时跳过）
//   - 健康检测探测成功（healthcheck 的 credits 同步）
//   - 代挂「刷新余额」（host.go）
//
// 三处**无条件**调用，于是「6004 写下的上游重置墙钟」会在下一次余额刷新时蒸发：
//
//	09:00 签到清空 → 10:00 同一模型的请求重新调度到该号 → 又撞 6004
//
// 后果：① 冷却时长被截断到"距下次签到/探活的间隔"（v0.26.5 把模型级上限提到 24h
// 的意义被抵消）；② 每次清空都放行一批必然失败的请求。
func TestModelCooldownSurvivesCreditsRefresh(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 1000)
	p.SetCredits("u2", 1)
	p.SetRandomSource(func(n int64) int64 { return 0 })

	resetAt := time.Now().Add(3 * time.Hour)
	// 账号级软冷却 + 该模型的 6004 独立冷却（顺序与生产一致：Cooldown 会清模型表）。
	p.Cooldown("u1", CoolSoft, 600*time.Second, "429")
	p.CooldownSoftForModel("u1", time.Minute, resetAt, "glm-5.3", "6004")

	// 签到：余额查得 > 0。
	p.SetCreditsDetailed("u1", 5000, 0)
	p.ReenableIfCredits("u1", 5000)

	// ① 账号级冷却必须被解冻（签到解冻的本职）。
	st, _ := p.Status("u1")
	if st.Cooling {
		t.Errorf("签到解冻后账号级冷却应已清除（until=%v kind=%v）", st.Until, st.CoolKind)
	}
	// ② 模型级冷却必须**还在**，且 Until 仍对齐上游重置墙钟。
	p.mu.RLock()
	mc, ok := p.byUID["u1"].modelCooldowns["glm-5.3"]
	p.mu.RUnlock()
	if !ok {
		t.Fatalf("签到解冻把模型级 6004 冷却清掉了 —— 冷却时长会被截断到下次签到，" +
			"期间同模型的请求会被反复调度到这个号并必然失败")
	}
	if d := mc.Until.Sub(resetAt); d < -time.Second || d > time.Second {
		t.Errorf("模型级 Until=%v 应对齐上游重置墙钟 %v", mc.Until, resetAt)
	}
	// ③ 端到端：解冻后请求同一模型必须仍避开 u1。
	if got := p.PickExcludingForRealm(nil, "glm-5.3", ""); got == nil || got.UID != "u2" {
		t.Errorf("签到解冻后 glm-5.3 请求不应回到 u1, got %+v", got)
	}
	// ④ 幂等：重复同步（每天两次签到）不得累积副作用。
	p.ReenableIfCredits("u1", 5000)
	if got := p.PickExcludingForRealm(nil, "glm-5.3", ""); got == nil || got.UID != "u2" {
		t.Errorf("重复签到解冻后仍应避开 u1, got %+v", got)
	}
	// ⑤ 但其他模型照常可用（模型级冷却只锁一个模型，不锁账号）。
	if got := p.PickExcludingForRealm(nil, "hy3-x", ""); got == nil || got.UID != "u1" {
		t.Errorf("hy3-x 请求应可选到 u1（只锁一个模型）, got %+v", got)
	}
}

// TestManualReviveStillClearsModelCooldowns 人工启用（ReviveDisabled）**仍然**清模型级冷却。
//
// 与上一条刻意不同的口径（别把两者改成一致）：
//   - 签到的余额解冻是**系统按周期无条件**做的，它不构成"这个号可用"的判断依据；
//   - 人工「启用」是运维对账号可用性的明确断言，此时若残留模型级冷却，界面显示"已启用"
//     而该模型仍不可选 —— 正是本仓那类最难查的「点了没效果、也没有任何报错」缺陷。
func TestManualReviveStillClearsModelCooldowns(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(time.Hour), "glm-5.3", "6004")
	p.Disable("u1", "test")

	if !p.ReviveDisabled("u1") {
		t.Fatal("ReviveDisabled 应返回 true")
	}
	p.mu.RLock()
	n := len(p.byUID["u1"].modelCooldowns)
	p.mu.RUnlock()
	if n != 0 {
		t.Errorf("人工启用后 modelCooldowns=%d want 0（启用是运维对可用性的明确断言）", n)
	}
}

// TestFallbackSkipsModelCooledAccount 全冷却兜底不得把请求发给「对本模型正在冷却」的账号。
//
// 兜底只会在"无任何 healthy 候选"时触发，它选出来的号**一定**在账号级冷却中 ——
// 发给它本身就是一次半开探测。但账号级与模型级是正交维度：一个号可能"账号级刚到期、
// 该模型还有 8 小时才重置"。此时把请求发过去，是在用一个注定失败的请求探测一个
// 我们已经把答案记在 modelCooled 里的问题。返回 nil（上层 503）比发一次必然失败的
// 请求更诚实。
func TestFallbackSkipsModelCooledAccount(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	// 账号级冷却（兜底候选的来源）+ 该模型的模型级冷却（要排除的维度）。
	p.Cooldown("u1", CoolSoft, 2*time.Hour, "429 rate limit")
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(3*time.Hour), "glm-5.3", "6004")

	if got := p.PickExcludingForRealm(nil, "glm-5.3", ""); got != nil {
		t.Errorf("兜底不应把 glm-5.3 请求发给对它正在冷却的 u1, got %+v", got)
	}
	// 兜底没有被"顺带关掉"：换一个模型仍应正常兜底选出 u1（它的账号级冷却仍有效）。
	if got := p.PickExcludingForRealm(nil, "hy3-x", ""); got == nil || got.UID != "u1" {
		t.Errorf("其他模型的兜底应照常可选 u1, got %+v", got)
	}
	// 空模型（如元信息类请求）不因模型维度受限，照常兜底。
	if got := p.PickExcludingForRealm(nil, "", ""); got == nil || got.UID != "u1" {
		t.Errorf("空 reqModel 应等价账号级口径（照常兜底）, got %+v", got)
	}
}

// TestObservedModelsOnlyFromSuccess 池内模型清单**只**取成功观测（(账号,模型) 成本台账），
// 刻意排除模型级冷却键。
//
// 为什么排除（v0.31.2 实测踩到）：11102 负缓存的键是**客户端传来的原样字符串** ——
// `GLM-5.3`、`gpt-4o` 这类写错的名字都在里面。把它们当"已知模型名"会让归一化
// **反向确认错误写法**：现象是 `GLM-5.3` 明明在 /v1/models 清单里却不再被折回
// `glm-5.3`（因为 `GLM-5.3` 自己成了"已知名"，归一化认为无需改动）。
// 规律：**只有"成功"可以作为命名权威**（与 modelname_seed.go 只取 status='ok' 同一条纪律）。
func TestObservedModelsFromCostAndCooldown(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	if got := p.ObservedModels(); len(got) != 0 {
		t.Fatalf("未服务过任何模型时应为空，got %v", got)
	}
	p.NoteModelCost("u1", "glm-5.3", 1, 1000)
	p.NoteModelCost("u2", "glm-5.3", 1, 1000) // 重复：应去重
	p.NoteModelCost("u2", "kimi-k3-1", 1, 1000)
	p.NoteModelCost("u1", "", 1, 1000) // 空名不入清单
	// 冷却/负缓存键：**不得**进清单（可能正是客户端写错的名字）。
	p.CooldownSoftForModel("u2", time.Minute, time.Now().Add(time.Hour), "GLM-5.3-WRONG", "6004")
	p.BlockModelBackoff("u2", "gpt-4o", upstream.ModelBlockReason)

	got := p.ObservedModels()
	if len(got) != 2 || got[0] != "glm-5.3" || got[1] != "kimi-k3-1" {
		t.Fatalf("应只含成功观测并去重排序 [glm-5.3 kimi-k3-1]，got %v", got)
	}
	for _, bad := range []string{"GLM-5.3-WRONG", "gpt-4o"} {
		for _, m := range got {
			if m == bad {
				t.Errorf("冷却/负缓存键 %q 不得进模型名清单（会反向确认错误写法）", bad)
			}
		}
	}
}
