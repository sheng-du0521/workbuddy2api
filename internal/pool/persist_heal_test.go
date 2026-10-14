// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package pool

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// 本文件锁 v0.26.4 的**存量自愈**：旧实现把 6004（模型级限额）误写成账号级冷却，
// 那一形态必须被纠正 —— 否则升级二进制也不会让被整体挡住的健康号回来。
//
// 为什么单独立文件：这类「代码修了但磁盘上的状态还是旧形态」的缺陷，
// 只有通过"用旧形态的状态文件启动"才测得到；用内存构造的用例测不出来。

// writeLegacyState 写一份含指定账号条目的 state.json（模拟旧版本留下的文件）。
func writeLegacyState(t *testing.T, entries map[string]stateAccount) string {
	t.Helper()
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.mu.Lock()
	p.applyAccountsLocked(entries)
	p.mu.Unlock()
	p.dirty.Store(true)
	p.Flush()
	if _, err := os.Stat(fp); err != nil {
		t.Fatalf("state.json 未写出：%v", err)
	}
	return fp
}

// TestHealLegacyModelLimitAccountCooldown 旧形态（6004 却写成账号级冷却）在启动时被清除。
//
// 这是本轮用户明确提出的口径：「6004 的冷却是针对对应模型的，其他模型调用正常接」——
// 而旧形态下**所有模型一起接不了**。
func TestHealLegacyModelLimitAccountCooldown(t *testing.T) {
	future := time.Now().Add(45 * time.Minute)
	fp := writeLegacyState(t, map[string]stateAccount{
		"legacy-1": {
			Until:    future,
			CoolKind: CoolSoft,
			Reason:   "6004 model rate limit",
		},
	})

	p := New(fp)
	p.Add(&auth.Auth{UID: "legacy-1"})
	st, ok := p.Status("legacy-1")
	if !ok {
		t.Fatal("账号应在池内")
	}
	if st.Cooling || !st.Until.IsZero() {
		t.Errorf("旧形态的账号级冷却应被清除：cooling=%v until=%v", st.Cooling, st.Until)
	}
	if st.Reason != "" {
		t.Errorf("reason 应清空，得 %q", st.Reason)
	}
	if st.SoftStreak != 0 {
		t.Errorf("soft_streak 应归零，得 %d", st.SoftStreak)
	}
	// 关键：该号现在对所有模型都可用（本轮要求的落点）。
	for _, m := range []string{"glm-5.3", "hy3-x", ""} {
		if !hasUID(p.AvailableUIDsForModel(m), "legacy-1") {
			t.Errorf("纠正后该号应可用于模型 %q", m)
		}
	}
}

// TestHealKeepsCorrectModelCooldown 正确形态（模型级冷却）不得被误伤。
//
// 这条是"自愈"能不能上线的底线：一个把正常状态也清掉的迁移比不做还糟。
func TestHealKeepsCorrectModelCooldown(t *testing.T) {
	future := time.Now().Add(45 * time.Minute)
	fp := writeLegacyState(t, map[string]stateAccount{
		"ok-1": {
			ModelCooldowns: map[string]stateModelCooldown{
				"glm-5.3": {Until: future, ResetAt: future, Reason: "6004 model rate limit [reset=body-text]"},
			},
		},
	})

	p := New(fp)
	p.Add(&auth.Auth{UID: "ok-1"})
	st, _ := p.Status("ok-1")
	if len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Model != "glm-5.3" {
		t.Fatalf("模型级冷却应保留，得 %+v", st.RateLimitedModels)
	}
	if st.Cooling {
		t.Errorf("模型级冷却不该表现为账号级 cooling：%+v", st)
	}
	// 只挡该模型，其它模型可用。
	if hasUID(p.AvailableUIDsForModel("glm-5.3"), "ok-1") {
		t.Error("该模型应被挡")
	}
	for _, m := range []string{"hy3-x", ""} {
		if !hasUID(p.AvailableUIDsForModel(m), "ok-1") {
			t.Errorf("其它模型 %q 应可用", m)
		}
	}
}

// TestHealOnlyTouchesExactLegacyForm 自愈必须**精确**：只清「6004 前缀 + 账号级 + 软冷却」。
//
// 逐条列出"长得像但语义不同"的邻居，任何一条被误清都会造成真实损失。
func TestHealOnlyTouchesExactLegacyForm(t *testing.T) {
	future := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Hour)

	cases := []struct {
		name      string
		acc       stateAccount
		wantClear bool
	}{
		{"① 旧形态：6004 前缀 + 账号级软冷却 → 清", stateAccount{
			Until: future, CoolKind: CoolSoft, Reason: "6004 model rate limit"}, true},
		{"② 429 账号级限流 → 保留（不是 6004）", stateAccount{
			Until: future, CoolKind: CoolSoft, Reason: "429 rate limit [reset=header]"}, false},
		{"③ 11140 累计软冷却 → 保留", stateAccount{
			Until: future, CoolKind: CoolSoft, Reason: "11140 request illegal（累计 1/3，未超阈值仅软冷却）"}, false},
		{"④ WAF 403 → 保留", stateAccount{
			Until: future, CoolKind: CoolSoft, Reason: "waf 403 block"}, false},
		{"⑤ 余额类硬冷却（reason 恰含余额词）→ 保留", stateAccount{
			Until: future, CoolKind: CoolHard, Reason: "余额不足"}, false},
		{"⑥ 已过期的旧形态 → 无需处理（保留原样即可，不影响可用性）", stateAccount{
			Until: past, CoolKind: CoolSoft, Reason: "6004 model rate limit"}, false},
		{"⑦ 旧形态但账号同时被手动停用 → 保留（那是人的决定）", stateAccount{
			Until: future, CoolKind: CoolSoft, Reason: "6004 model rate limit",
			ManualDisabled: true, ManualReason: "运维主动摘除"}, false},
		{"⑧ 旧形态但账号同时被系统禁用 → 保留（另有病因）", stateAccount{
			Until: future, CoolKind: CoolSoft, Reason: "6004 model rate limit", Disabled: true}, false},
		{"⑨ 旧形态但已是硬冷却（coolKind 不符）→ 保留（硬冷却=余额类，语义不同）", stateAccount{
			Until: future, CoolKind: CoolHard, Reason: "6004 model rate limit"}, false},
		{"⑩ 账号故障 14017 → 保留", stateAccount{
			Until: future, CoolKind: CoolSoft, Reason: "账号故障（14017），需重登"}, false},
		{"⑪ 旧形态 + 同时已有模型级台账 → **仍清**（账号级残留照样会挡所有模型）", stateAccount{
			Until: future, CoolKind: CoolSoft, Reason: "6004 model rate limit",
			ModelCooldowns: map[string]stateModelCooldown{
				"glm-5.3": {Until: future, ResetAt: future, Reason: "6004 model rate limit [reset=body-text]"},
			}}, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fp := writeLegacyState(t, map[string]stateAccount{"u": c.acc})
			p := New(fp)
			p.Add(&auth.Auth{UID: "u"})
			// 断言读**原始 entry** 而不是 Status()：statusOf 在"不在冷却中"时会
			// 屏蔽 reason（那是给界面看的口径），用它判断"有没有被清"会得出
			// 「无冷却 + 残留 reason」也算清过的错误结论（第一版就是这么写的）。
			p.mu.RLock()
			e := p.byUID["u"]
			until, reason, streak := e.until, e.reason, e.softStreak
			p.mu.RUnlock()
			cleared := until.IsZero() && reason == "" && streak == 0
			if cleared != c.wantClear {
				t.Errorf("cleared=%v want %v（until=%v reason=%q streak=%d）",
					cleared, c.wantClear, until, reason, streak)
			}
		})
	}
}

// TestHealAppliesToSnapshotPath Redis 快照恢复走同一份 applyAccountsLocked，
// 所以自愈对它同样生效 —— 否则"换一条恢复路径启动"就又回到错误形态。
func TestHealAppliesToSnapshotPath(t *testing.T) {
	future := time.Now().Add(30 * time.Minute)
	p := New("")
	// 注意：snapshot 内嵌 stateFile，而**内嵌字段不能在复合字面量里用提升名初始化**
	// （Go 1.27 才放开；本仓的 CI 按 go.mod 用 1.26，所以必须显式写内嵌类型名）。
	p.applySnapshotLocked(snapshot{stateFile: stateFile{Accounts: map[string]stateAccount{
		"snap-1": {Until: future, CoolKind: CoolSoft, Reason: "6004 model rate limit"},
	}}})
	p.Add(&auth.Auth{UID: "snap-1"})
	st, ok := p.Status("snap-1")
	if !ok {
		t.Fatal("账号应在池内")
	}
	if st.Cooling || !st.Until.IsZero() {
		t.Errorf("快照路径也应自愈，得 cooling=%v until=%v", st.Cooling, st.Until)
	}
}

// TestHealThenRealModelLimitTakesEffectOverAccountAvailability 端到端语义：
// 纠正之后，该号对**其它模型**可用；随后若那个模型真的被限，只挡那一个模型。
func TestHealThenRealModelLimitTakesEffectOverAccountAvailability(t *testing.T) {
	future := time.Now().Add(20 * time.Minute)
	fp := writeLegacyState(t, map[string]stateAccount{
		"u1": {Until: future, CoolKind: CoolSoft, Reason: "6004 model rate limit"},
	})
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})

	// 纠正后：所有模型可用。
	if !hasUID(p.AvailableUIDsForModel("glm-5.3"), "u1") || !hasUID(p.AvailableUIDsForModel("hy3-x"), "u1") {
		t.Fatal("纠正后应对所有模型可用")
	}
	// 下一次真的撞 6004（新实现只写模型级）。
	p.SetSoftRateMax(time.Hour)
	p.CooldownSoftForModel("u1", 60*time.Second, time.Now().Add(10*time.Minute), "glm-5.3", "6004 model rate limit [reset=body-text]")

	if hasUID(p.AvailableUIDsForModel("glm-5.3"), "u1") {
		t.Error("被限的模型应不可用")
	}
	if !hasUID(p.AvailableUIDsForModel("hy3-x"), "u1") {
		t.Error("其它模型应仍然可用 —— 这正是用户要求的口径")
	}
	st, _ := p.Status("u1")
	if st.Cooling {
		t.Errorf("不该表现为账号级冷却：%+v", st)
	}
}

// hasUID 判断可用账号列表里是否含某 uid。
//
// AvailableUIDs 家族返回的是 []string（不是 set）—— 刻意不改成 map：
// 调用点大多是"按顺序遍历找第一个可用"，返回切片更贴合用法。
func hasUID(list []string, uid string) bool {
	for _, u := range list {
		if u == uid {
			return true
		}
	}
	return false
}
