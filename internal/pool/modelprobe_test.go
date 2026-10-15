// 模型级冷却的**半开探测**（v0.31.6）单测。
//
// 要解决的问题：6004 的恢复时刻来自上游墙钟（封顶 24h），**上游提前解除**时我们无从
// 得知 —— 严格等墙钟意味着一个其实已经恢复的 (账号, 模型) 被白放到最晚一天。
// 探测是唯一的发现手段，而它本身会真的发上游请求，因此每一条边界都必须被钉住。
//
// 逐条覆盖：
//  1. 只有 6004（Probeable）可探；11102 负缓存不可探（重试无意义）；
//  2. 冷却已到期 / 无窗口起点（Since 零值）一律不探；
//  3. 进入冷却不足 delay 不探；剩余不足 delay 时**整个窗口内**不再探（next 为零值）；
//  4. 探测过之后 interval 内不探（出站强度上限）；
//  5. 闸门原子：选号与记时刻同一把锁 → 同一个号不会被连选两次；
//  6. 探测成功只清 Probeable 条目；11102 仍走 BlockModelClear，两条路径不混；
//  7. 三个新字段落盘/恢复往返无损；老 state.json 缺 Since 时按恢复时刻起算。
package pool

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// probeMC 造一个"典型的 6004 冷却窗口"：窗口长度 window，已经冷却了 elapsed。
func probeMC(now time.Time, window, elapsed time.Duration) modelCooldown {
	return modelCooldown{
		Until:     now.Add(window),
		Since:     now.Add(-elapsed),
		Reason:    "6004 model rate limit [reset=body-json]",
		Probeable: true,
	}
}

func TestProbeGateBoundaries(t *testing.T) {
	now := time.Now()
	const delay, interval = 20 * time.Minute, time.Hour

	// ① 典型可探：窗口 3h、已冷却 30m、从未探过
	if ok, next := probeGate(now, probeMC(now, 3*time.Hour, 30*time.Minute), delay, interval); !ok {
		t.Fatalf("过了 delay 且从未探过 → 应可探（next=%v）", next)
	}

	// ② 11102 负缓存：时间条件全满足也不探 —— 重试无意义
	mc := probeMC(now, 3*time.Hour, 30*time.Minute)
	mc.Probeable = false
	if ok, _ := probeGate(now, mc, delay, interval); ok {
		t.Error("11102 条目不得探测：上游已明说该后端没有这个模型，恢复靠 TTL 到期 / BlockModelClear")
	}

	// ③ 已到期：交给正常选号，探测纯属多余
	if ok, _ := probeGate(now, modelCooldown{Until: now.Add(-time.Minute), Since: now.Add(-time.Hour), Probeable: true}, delay, interval); ok {
		t.Error("已到期的冷却不该走探测路径")
	}

	// ④ Since 零值（v0.31.5 及更早的 state.json / 异常构造）：不探
	if ok, _ := probeGate(now, modelCooldown{Until: now.Add(3 * time.Hour), Probeable: true}, delay, interval); ok {
		t.Error("没有窗口起点就不该探：宁可少探，也不要让一次升级变成一轮对全池冷却号的探测脉冲")
	}

	// ⑤ 还没过 delay：不探，且 next 指向可探时刻
	ok, next := probeGate(now, probeMC(now, 3*time.Hour, 5*time.Minute), delay, interval)
	if ok {
		t.Error("进入冷却不足 delay 不应探测（窗口刚开时上限必然还在，白打一次上游）")
	}
	if want := now.Add(15 * time.Minute); next.Sub(want).Abs() > time.Second {
		t.Errorf("next=%v，want≈%v（应指向 Since+delay）", next, want)
	}

	// ⑥ 剩余不足 delay：本窗口内**不再探**，next 为零值（台账据此显示"没有下次"）
	if ok, next := probeGate(now, probeMC(now, 10*time.Minute, time.Hour), delay, interval); ok || !next.IsZero() {
		t.Errorf("剩余不足 delay 应判为「本窗口不探」，got ok=%v next=%v", ok, next)
	}

	// ⑦ 探过之后：interval 内不探，next 指向 ProbedAt+interval
	probed := now.Add(-10 * time.Minute)
	mc = probeMC(now, 3*time.Hour, 2*time.Hour)
	mc.ProbedAt = probed
	ok, next = probeGate(now, mc, delay, interval)
	if ok {
		t.Error("同一个号在 interval 内不得重复探测（这是出站强度的上限）")
	}
	if want := probed.Add(interval); next.Sub(want).Abs() > time.Second {
		t.Errorf("next=%v，want≈%v（应等满 interval，而不是 delay）", next, want)
	}

	// ⑧ 关停与非法参数：一律不探
	base := probeMC(now, 3*time.Hour, 2*time.Hour)
	if ok, _ := probeGate(now, base, delay, 0); ok {
		t.Error("interval=0 = 关停探测（回到严格等上游墙钟）")
	}
	if ok, _ := probeGate(now, base, 0, interval); ok {
		t.Error("delay 非法（<=0）时不应探测")
	}
}

// TestProbeGateKeepsCadenceAcrossWindowRewrite 窗口被重写（探测失败后上游又给了新的
// 重置时刻）时，节奏绑在**键**上而不是**窗口**上。
//
// 若 ProbedAt 随窗口重置，节奏会从 interval（1h）悄悄退化成 delay（20m）—— 出站强度
// 变成设计值的 3 倍，而代码看起来毫无问题。
func TestProbeGateKeepsCadenceAcrossWindowRewrite(t *testing.T) {
	now := time.Now()
	const delay, interval = 20 * time.Minute, time.Hour
	probed := now.Add(-30 * time.Minute) // 半小时前探过（已过 delay，未过 interval）
	mc := modelCooldown{
		Until:     now.Add(5 * time.Hour),
		Since:     now.Add(-2 * time.Minute), // 新窗口：刚被重写
		ProbedAt:  probed,
		Probeable: true,
	}
	ok, next := probeGate(now, mc, delay, interval)
	if ok {
		t.Fatal("新窗口 + 30 分钟前探过 → 不该立即再探（否则节奏退化成 delay）")
	}
	if want := probed.Add(interval); next.Sub(want).Abs() > time.Second {
		t.Errorf("next=%v，want≈%v（节奏应绑键：等满 interval）", next, want)
	}
}

// TestPickProbeForModelRealmPicksEarliestAndStampsAtomically 选号口径 + 原子闸门。
func TestPickProbeForModelRealmPicksEarliestAndStampsAtomically(t *testing.T) {
	now := time.Now()
	p := New("")
	p.Add(&auth.Auth{UID: "a-earliest"})
	p.Add(&auth.Auth{UID: "b-later"})
	// global=0（关闭全池间隔）：本用例要单独钉住**每号**闸门，全池间隔另有用例
	// （不能用 1ns —— Windows 的 time.Now 粒度粗，两次调用可能同戳而被误判为"刚探过"）。
	p.SetModelProbe(time.Minute, time.Hour, 0)

	seed := func(uid string, window time.Duration) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.byUID[uid].modelCooldowns = map[string]modelCooldown{
			"m1": probeMC(now, window, time.Hour),
		}
	}
	seed("a-earliest", 2*time.Hour)
	seed("b-later", 6*time.Hour)

	got := p.PickProbeForModelRealm(nil, "m1", "")
	if got == nil || got.UID != "a-earliest" {
		t.Fatalf("应选**最早到期**的可探号（它最可能已经恢复），got %v", got)
	}

	// 闸门原子性：a 已被本次探测占用 → 同一个 a 不能再被选中
	if again := p.PickProbeForModelRealm(map[string]bool{"a-earliest": true}, "m1", ""); again == nil || again.UID != "b-later" {
		t.Fatalf("a 的名额已被本次探测吃掉，第二顺位应是 b，got %v", again)
	}
	if none := p.PickProbeForModelRealm(map[string]bool{"a-earliest": true, "b-later": true}, "m1", ""); none != nil {
		t.Fatalf("两个号都探过 → 本轮不探（返回 nil，由上层回 503），got %v", none)
	}

	// 台账可见性：两次探测都在台账里留下痕迹
	st, ok := p.Status("a-earliest")
	if !ok {
		t.Fatal("账号不在池内")
	}
	row := st.RateLimitedModels[0]
	if !row.Probeable || row.ProbedAt.IsZero() || row.NextProbeAt.IsZero() {
		t.Errorf("台账应显示 probeable/probed_at/next_probe_at，got %+v", row)
	}
}

// TestPickProbeGlobalSpacing 全池间隔：**同一模型**在 global 内只允许一次探测，
// 不管要探的是哪个号。
//
// 这是实测发现的缺口补丁：只有"每号每 interval 一次"时，池里 33 个冷却号等于
// "客户端连续发 33 个请求就能打出 33 次上游调用"（同一出口 IP、几十秒内），
// 正是本项目最贵的一类事故。
func TestPickProbeGlobalSpacing(t *testing.T) {
	now := time.Now()
	p := New("")
	p.Add(&auth.Auth{UID: "a"})
	p.Add(&auth.Auth{UID: "b"})
	p.SetModelProbe(time.Minute, time.Hour, time.Hour)
	for _, uid := range []string{"a", "b"} {
		p.mu.Lock()
		p.byUID[uid].modelCooldowns = map[string]modelCooldown{"m1": probeMC(now, 3*time.Hour, time.Hour)}
		p.mu.Unlock()
	}

	if got := p.PickProbeForModelRealm(nil, "m1", ""); got == nil {
		t.Fatal("precondition：第一次应可探")
	}
	// 第二个号虽然自己没探过、也没被 tried 排除，但**全池**刚探过 → 仍然不给
	if got := p.PickProbeForModelRealm(map[string]bool{}, "m1", ""); got != nil {
		t.Fatalf("全池间隔内不得再探（否则 N 个号 = N 次连续上游调用），got %v", got)
	}
	// 另一个模型不受影响（间隔是按模型的）
	p.mu.Lock()
	p.byUID["a"].modelCooldowns["m2"] = probeMC(now, 3*time.Hour, time.Hour)
	p.mu.Unlock()
	if got := p.PickProbeForModelRealm(nil, "m2", ""); got == nil || got.UID != "a" {
		t.Fatalf("间隔是按模型计的，另一个模型应可探，got %v", got)
	}
}

// TestPickProbeForModelRealmOnlyHealthyAccounts 只有"账号级完全健康"的号才参与模型级探测。
func TestPickProbeForModelRealmOnlyHealthyAccounts(t *testing.T) {
	now := time.Now()
	p := New("")
	p.Add(&auth.Auth{UID: "healthy"})
	p.Add(&auth.Auth{UID: "acct-cooled"})
	p.Add(&auth.Auth{UID: "hard-cooled"})
	p.Add(&auth.Auth{UID: "banned-model"})
	p.SetModelProbe(time.Minute, time.Hour, time.Minute)

	for _, uid := range []string{"healthy", "acct-cooled", "banned-model"} {
		p.mu.Lock()
		p.byUID[uid].modelCooldowns = map[string]modelCooldown{"m1": probeMC(now, 3*time.Hour, time.Hour)}
		p.mu.Unlock()
	}
	// 11102 变体：同一个模型，但条目不可探
	p.mu.Lock()
	mc := p.byUID["banned-model"].modelCooldowns["m1"]
	mc.Probeable = false
	mc.Reason = "11102 model not found on this backend"
	p.byUID["banned-model"].modelCooldowns["m1"] = mc
	p.mu.Unlock()

	// 账号级软冷却：模型级探测回答不了「账号本身能不能用」，交给账号级兜底
	p.CooldownSoftRate("acct-cooled", time.Minute, time.Time{}, "429 rate limit")
	// 账号级硬冷却（余额耗尽）
	p.CooldownUntilTomorrow4AM("hard-cooled", "credits exhausted")
	p.mu.Lock()
	p.byUID["hard-cooled"].modelCooldowns = map[string]modelCooldown{"m1": probeMC(now, 3*time.Hour, time.Hour)}
	p.mu.Unlock()

	got := p.PickProbeForModelRealm(nil, "m1", "")
	if got == nil || got.UID != "healthy" {
		t.Fatalf("只有账号级健康的号可探，got %v", got)
	}
	// healthy 探掉之后，剩下的全是不可探的 → nil
	if again := p.PickProbeForModelRealm(map[string]bool{"healthy": true}, "m1", ""); again != nil {
		t.Fatalf("账号级冷却 / 硬冷却 / 11102 都不该被探，got %v", again)
	}
}

// TestProbeSucceededClearsOnlyProbeableEntries 两条清理路径各管一种载荷。
func TestProbeSucceededClearsOnlyProbeableEntries(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u"})
	p.CooldownSoftForModel("u", time.Minute, time.Now().Add(3*time.Hour), "m6004", "6004 model rate limit [reset=body-json]")

	if !p.ProbeSucceeded("u", "m6004") {
		t.Fatal("探测成功后应解除该 (账号, 模型) 的 6004 冷却")
	}
	if st, _ := p.Status("u"); len(st.RateLimitedModels) != 0 {
		t.Fatalf("解除后台账不该再有该模型，got %+v", st.RateLimitedModels)
	}
	if p.ProbeSucceeded("u", "m6004") {
		t.Error("重复解除应返回 false（已无该条目）")
	}
	if p.ProbeSucceeded("u", "") || p.ProbeSucceeded("", "m6004") {
		t.Error("空 uid / 空 model 应安全返回 false")
	}

	// 11102 条目：**不可**被探测成功清掉（它不是探测的目标）
	p.BlockModelBackoff("u", "m11102", "11102 model not found on this backend")
	if p.ProbeSucceeded("u", "m11102") {
		t.Error("11102 负缓存不该被「探测成功」清理（它不可探测；清除走 BlockModelClear）")
	}
	if st, _ := p.Status("u"); len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Model != "m11102" {
		t.Fatalf("11102 条目应仍在，got %+v", st.RateLimitedModels)
	}
	// 它自己的路径（成功 → BlockModelClear）仍然有效
	p.BlockModelClear("u", "m11102")
	if st, _ := p.Status("u"); len(st.RateLimitedModels) != 0 {
		t.Fatalf("BlockModelClear 应清掉 11102 条目，got %+v", st.RateLimitedModels)
	}
}

// TestModelProbeKillSwitch 关停开关：interval<=0 时既不放行，也不在台账里承诺"下次探测"。
func TestModelProbeKillSwitch(t *testing.T) {
	now := time.Now()
	p := New("")
	p.Add(&auth.Auth{UID: "u"})
	p.CooldownSoftForModel("u", time.Minute, now.Add(3*time.Hour), "m1", "6004 model rate limit")
	p.mu.Lock()
	p.byUID["u"].modelCooldowns["m1"] = probeMC(now, 3*time.Hour, 2*time.Hour)
	p.mu.Unlock()

	if got := p.PickProbeForModelRealm(nil, "m1", ""); got == nil {
		t.Fatal("默认配置下应可探（这是 precondition）")
	}
	p.SetModelProbe(20*time.Minute, 0, -1) // 关停
	if got := p.PickProbeForModelRealm(nil, "m1", ""); got != nil {
		t.Fatalf("关停后不得再探测，got %v", got)
	}
	if d, iv, g := p.ModelProbeConfig(); iv != 0 || d != 20*time.Minute || g != defaultModelProbeGlobal {
		t.Errorf("ModelProbeConfig 应回读注入值，got delay=%v interval=%v global=%v", d, iv, g)
	}
	if st, _ := p.Status("u"); !st.RateLimitedModels[0].NextProbeAt.IsZero() {
		t.Error("关停后台账不该再显示 next_probe_at（否则运维以为还会自动试）")
	}
}

// TestModelProbeFieldsPersistRoundTrip 三个新字段必须落盘：任何一个丢了，
// 重启都会静默改变行为（Since 丢 → 可探时刻被重置；Probeable 丢 → 功能失效；
// ProbedAt 丢 → 探测节奏被重置）。
func TestModelProbeFieldsPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	fp := dir + "/state.json"
	p := New(fp)
	p.Add(&auth.Auth{UID: "u"})
	p.CooldownSoftForModel("u", time.Minute, time.Now().Add(3*time.Hour), "m1", "6004 model rate limit")
	// 人为制造"已经探过一次"的历史
	p.mu.Lock()
	mc := p.byUID["u"].modelCooldowns["m1"]
	mc.ProbedAt = time.Now().Add(-5 * time.Minute).Truncate(time.Second)
	p.byUID["u"].modelCooldowns["m1"] = mc
	p.dirty.Store(true)
	p.mu.Unlock()
	p.Flush()

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u"})
	p2.mu.RLock()
	mc2, ok := p2.byUID["u"].modelCooldowns["m1"]
	p2.mu.RUnlock()
	if !ok {
		t.Fatal("重启后该模型的冷却条目丢失")
	}
	if !mc2.Probeable {
		t.Fatal("Probeable 未持久化 → 重启后半开探测静默失效")
	}
	if mc2.Since.IsZero() {
		t.Fatal("Since 未持久化 → 重启后可探时刻被重置")
	}
	if mc2.ProbedAt.IsZero() {
		t.Fatal("ProbedAt 未持久化 → 重启就重置探测节奏")
	}

	// 老数据（v0.31.5 及更早写的 state.json：没有 since/probed_at/probeable）：
	// 恢复时必须按**恢复时刻**起算 delay，而不是当成"已经冷却够了"（否则一次升级
	// 就变成一轮对全池冷却号的探测脉冲）。
	stripAndReload(t, fp, func(acc map[string]any) {
		mcs, _ := acc["model_cooldowns"].(map[string]any)
		for _, v := range mcs {
			one, _ := v.(map[string]any)
			delete(one, "since")
			delete(one, "probed_at")
			delete(one, "probeable")
		}
	})
	p3 := New(fp)
	p3.Add(&auth.Auth{UID: "u"})
	p3.mu.RLock()
	mc3, ok := p3.byUID["u"].modelCooldowns["m1"]
	p3.mu.RUnlock()
	if !ok {
		t.Fatal("老数据恢复失败")
	}
	if mc3.Since.IsZero() {
		t.Fatal("缺 since 的老数据应按恢复时刻补齐（否则探测永远不生效）")
	}
	if got := p3.PickProbeForModelRealm(nil, "m1", ""); got != nil {
		t.Fatalf("缺 since 的老数据刚恢复时不得立即探测（应等满 delay），got %v", got)
	}
}

// stripAndReload 就地改写 state.json：对每个账号执行 mutate，再写回。
// 用于模拟"更早版本写下的文件"（字段缺失）。
func stripAndReload(t *testing.T, fp string, mutate func(acc map[string]any)) {
	t.Helper()
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("读 state.json: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("解析 state.json: %v", err)
	}
	accounts, _ := doc["accounts"].(map[string]any)
	if len(accounts) == 0 {
		t.Fatal("state.json 里没有账号（落盘格式变了？）")
	}
	for _, v := range accounts {
		acc, _ := v.(map[string]any)
		mutate(acc)
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("序列化: %v", err)
	}
	if err := os.WriteFile(fp, out, 0o600); err != nil {
		t.Fatalf("写回 state.json: %v", err)
	}
}
