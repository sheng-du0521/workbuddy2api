// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// 本文件锁的是 v0.26.3 对 CooldownSoftForModel 的两处**作用域**修正：
//
//	① 零值重置时间分支必须是**模型级**（此前是账号级 —— 一次没带响应头的 6004
//	   会把整个健康号挡掉，与"切模型即可用"的承诺自相矛盾）；
//	② 并发/乱序响应**只延长不缩短**已有冷却（此前是无条件覆盖，一条较早发出、
//	   重置更近的响应会把已经写入的更长冷却改短）。
//
// 两处都属于"看起来只是少等一会儿"、实际会让号在窗口内反复撞墙的那类缺陷，
// 所以单独成文件、按作用域逐条钉死。

// modelCoolUntil 读某账号某模型的独立冷却截止（台账口径，测试用）。
func modelCoolUntil(t *testing.T, p *Pool, uid, model string) (time.Time, bool) {
	t.Helper()
	st, ok := p.Status(uid)
	if !ok {
		t.Fatalf("status(%s) missing", uid)
	}
	for _, m := range st.RateLimitedModels {
		if m.Model == model {
			return m.Until, true
		}
	}
	return time.Time{}, false
}

// forceModelExpired 把某账号某模型的冷却截止改到过去（模拟"退避到期后又撞一次"）。
func forceModelExpired(t *testing.T, p *Pool, uid, model string) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		t.Fatalf("uid %s missing", uid)
	}
	mc := e.modelCooldowns[model]
	mc.Until = time.Now().Add(-time.Second)
	e.modelCooldowns[model] = mc
}

// TestModelCooldownNoResetStaysModelScoped 零值重置时间 + 真实模型名
// → 只写该模型的独立冷却，**不碰账号级 until**，其他模型仍可选。
//
// 这是本轮最关键的一条回归：旧实现走"账号级有界退避"，账号被整体挡下。
func TestModelCooldownNoResetStaysModelScoped(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetSoftRateMax(time.Hour)
	p.SetCredits("u1", 1000)
	p.SetCredits("u2", 1)
	p.SetRandomSource(func(n int64) int64 { return 0 }) // r=0 → 优先选高积分

	p.CooldownSoftForModel("u1", 600*time.Second, time.Time{}, "glm-5.3", "6004 model rate limit")

	st, _ := p.Status("u1")
	if !st.Until.IsZero() {
		t.Errorf("账号级 until = %v，应为零值（模型级冷却不得污染账号级）", st.Until)
	}
	if st.Cooling {
		t.Errorf("账号不应处于账号级冷却：%+v", st)
	}
	until, ok := modelCoolUntil(t, p, "u1", "glm-5.3")
	if !ok {
		t.Fatal("modelCooldowns[glm-5.3] 缺失：零值分支必须写模型级冷却")
	}
	if d := time.Until(until); d < 590*time.Second || d > 610*time.Second {
		t.Errorf("模型级冷却剩余 = %v, want ≈600s（base 起有界退避）", d)
	}

	// 同模型请求：跳过 u1（该模型正被限）。
	if got := p.PickExcludingForRealm(nil, "glm-5.3", ""); got == nil || got.UID != "u2" {
		t.Errorf("同模型请求应跳过被限的 u1，得 %+v", got)
	}
	// 不同模型请求：u1 可用（这正是模型级冷却的意义）。
	if got := p.PickExcludingForRealm(nil, "hy3-x", ""); got == nil || got.UID != "u1" {
		t.Errorf("不同模型请求应可用 u1，得 %+v", got)
	}
}

// TestModelCooldownNoResetBackoffEscalates 零值分支按该 (账号,模型) 连续次数翻倍，
// 且**冷却中途的兜底探测不推进**（不越堆越厚），封顶 soft_rate_max。
func TestModelCooldownNoResetBackoffEscalates(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetSoftRateMax(10 * time.Minute)

	base := 60 * time.Second
	p.CooldownSoftForModel("u1", base, time.Time{}, "m", "6004")
	if d := time.Until(mustModelUntil(t, p, "u1", "m")); d < 55*time.Second || d > 65*time.Second {
		t.Errorf("首次退避 = %v, want ≈60s", d)
	}
	// 冷却中再撞：不推进（仍是 60s 量级，不是 120s/封顶）。
	p.CooldownSoftForModel("u1", base, time.Time{}, "m", "6004")
	if d := time.Until(mustModelUntil(t, p, "u1", "m")); d > 65*time.Second {
		t.Errorf("冷却中的兜底探测不得延长冷却，得 %v", d)
	}
	// 到期后再撞：推进到 120s。
	forceModelExpired(t, p, "u1", "m")
	p.CooldownSoftForModel("u1", base, time.Time{}, "m", "6004")
	if d := time.Until(mustModelUntil(t, p, "u1", "m")); d < 115*time.Second || d > 125*time.Second {
		t.Errorf("第二次退避 = %v, want ≈120s（60s × 2）", d)
	}
	// 反复到期再撞 → 封顶 soft_rate_max（10m），不会无限增长。
	for i := 0; i < 12; i++ {
		forceModelExpired(t, p, "u1", "m")
		p.CooldownSoftForModel("u1", base, time.Time{}, "m", "6004")
	}
	if d := time.Until(mustModelUntil(t, p, "u1", "m")); d > 10*time.Minute+2*time.Second {
		t.Errorf("退避应封顶 soft_rate_max=10m，得 %v", d)
	}
}

// TestModelCooldownConcurrentNeverShortens 并发/乱序响应只延长不缩短。
//
// 场景：同一账号同模型被多个在途请求同时撞 6004，上游给的重置时刻各不相同
// （不同请求打在不同后端上）。若逐个覆盖，最后到达的那条决定结果 ——
// 一条"重置更近"的响应会把已经写入的长冷却改短，于是号在真实窗口内被反复放出。
func TestModelCooldownConcurrentNeverShortens(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetSoftRateMax(6 * time.Hour)

	// 先写入一条 3 小时后的重置。
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(3*time.Hour), "m", "6004 先到")
	// 再写入一条 1 小时后的重置（乱序到达）：不得把冷却改短。
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(1*time.Hour), "m", "6004 后到")
	d := time.Until(mustModelUntil(t, p, "u1", "m"))
	if d < 2*time.Hour+50*time.Minute {
		t.Errorf("更晚的已有冷却被改短：剩余 %v，want ≈3h", d)
	}
	// 台账 reason 必须对应**真正生效**的那条冷却，而不是最后到达的那个。
	st, _ := p.Status("u1")
	if len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Reason != "6004 先到" {
		t.Errorf("reason = %+v，want 「6004 先到」（台账须描述生效中的冷却）", st.RateLimitedModels)
	}

	// 反向：更晚的重置到达 → 正常延长。
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Hour), "m", "6004 更晚")
	if d := time.Until(mustModelUntil(t, p, "u1", "m")); d < 4*time.Hour+50*time.Minute {
		t.Errorf("更晚的重置应延长冷却，剩余 %v，want ≈5h", d)
	}
	// 且仍受 soft_rate_max 约束（6h 封顶，5h 未触顶）。
	if d := time.Until(mustModelUntil(t, p, "u1", "m")); d > 6*time.Hour {
		t.Errorf("冷却不得越过 soft_rate_max：%v", d)
	}
}

// TestModelCooldownResetBranchClearsHits 带上游权威时刻的分支把连续计数归零
// （时长来自上游，与本地退避次数无关；否则下一次"无时间信息"会接着旧次数翻倍）。
func TestModelCooldownResetBranchClearsHits(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetSoftRateMax(time.Hour)

	// 先攒两次无时间信息的退避。
	p.CooldownSoftForModel("u1", 60*time.Second, time.Time{}, "m", "6004")
	forceModelExpired(t, p, "u1", "m")
	p.CooldownSoftForModel("u1", 60*time.Second, time.Time{}, "m", "6004")
	p.mu.RLock()
	hits := p.byUID["u1"].modelCooldowns["m"].Hits
	p.mu.RUnlock()
	if hits != 2 {
		t.Fatalf("Hits = %d, want 2", hits)
	}
	// 收到带权威时刻的 6004 → Hits 归零。
	p.CooldownSoftForModel("u1", 60*time.Second, time.Now().Add(30*time.Minute), "m", "6004 带时间")
	p.mu.RLock()
	got := p.byUID["u1"].modelCooldowns["m"].Hits
	p.mu.RUnlock()
	if got != 0 {
		t.Errorf("Hits = %d, want 0（权威时刻与本地退避次数无关）", got)
	}
}

// TestModelCooldownEmptyModelDegradesNoOp 空模型名退化为账号级软冷却（不是"不冷却"）。
func TestModelCooldownEmptyModelDegradesNoOp(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetSoftRateMax(time.Hour)

	p.CooldownSoftForModel("u1", 60*time.Second, time.Time{}, "", "6004 无模型名")

	st, _ := p.Status("u1")
	if st.Until.IsZero() || !st.Cooling {
		t.Errorf("空模型名应退化为账号级冷却（宁可多挡，不可不挡）：%+v", st)
	}
	if len(st.RateLimitedModels) != 0 {
		t.Errorf("空模型名不得写模型级台账：%+v", st.RateLimitedModels)
	}
}

// TestModelCooldownEmptyUIDNoOp 空 uid 不 panic、不改任何状态。
func TestModelCooldownEmptyUIDNoOp(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftForModel("", time.Minute, time.Now().Add(time.Hour), "m", "6004")
	p.CooldownSoftForModel("u-nonexist", time.Minute, time.Now().Add(time.Hour), "m", "6004")
	st, _ := p.Status("u1")
	if !st.Until.IsZero() || len(st.RateLimitedModels) != 0 {
		t.Errorf("未知/空 uid 不得影响其它账号：%+v", st)
	}
}

// TestModelCooldownResetAtHonoredInLedger 台账里该模型的 Until 按上游重置时刻走
// （用户口径「都按照时间」），且 ResetAt 保留上游原始时刻供运维对照。
//
// v0.26.5 改断言：旧版断言 Until 被 **soft_rate_max**（账号级上限）截断。
// 现在模型级只受 modelRateMax 约束 —— 下面第二段用 SetModelRateMax 演示
// "上限确实还在，只是换成了模型级那一档"，且截断时 ResetAt 依然如实保留。
func TestModelCooldownResetAtHonoredInLedger(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetSoftRateMax(10 * time.Minute) // 账号级上限，对模型级不生效
	reset := time.Now().Add(5 * time.Hour)
	p.CooldownSoftForModel("u1", time.Minute, reset, "m", "6004")
	st, _ := p.Status("u1")
	if len(st.RateLimitedModels) != 1 {
		t.Fatalf("台账缺失：%+v", st.RateLimitedModels)
	}
	row := st.RateLimitedModels[0]
	if d := row.Until.Sub(time.Now()); d < 5*time.Hour-2*time.Second || d > 5*time.Hour+2*time.Second {
		t.Errorf("Until 应跟随上游重置时刻（~5h，不被 soft_rate_max 截断），得 %v", d)
	}
	if d := row.ResetAt.Sub(reset); d > time.Second || d < -time.Second {
		t.Errorf("ResetAt 应保留上游原始时刻 %v，得 %v", reset, row.ResetAt)
	}

	// 模型级上限生效时：Until 被截断，但 ResetAt 仍保留权威时点。
	pc := New("")
	pc.Add(&auth.Auth{UID: "u1"})
	pc.SetModelRateMax(time.Hour)
	pc.CooldownSoftForModel("u1", time.Minute, reset, "m", "6004")
	stc, _ := pc.Status("u1")
	if len(stc.RateLimitedModels) != 1 {
		t.Fatalf("台账缺失：%+v", stc.RateLimitedModels)
	}
	if d := stc.RateLimitedModels[0].Until.Sub(time.Now()); d > time.Hour+2*time.Second {
		t.Errorf("SetModelRateMax(1h) 后 Until 应被截断到 1h，得 %v", d)
	}
	if d := stc.RateLimitedModels[0].ResetAt.Sub(reset); d > time.Second || d < -time.Second {
		t.Errorf("截断时 ResetAt 仍应保留上游原始时刻 %v，得 %v", reset, stc.RateLimitedModels[0].ResetAt)
	}
}

func mustModelUntil(t *testing.T, p *Pool, uid, model string) time.Time {
	t.Helper()
	until, ok := modelCoolUntil(t, p, uid, model)
	if !ok {
		t.Fatalf("modelCooldowns[%s] 缺失", model)
	}
	return until
}
