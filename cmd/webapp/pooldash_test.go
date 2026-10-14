// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// v 造一个账号视图（只填本测试关心的字段）。
func dashAcc(uid, nick string, credits int64, opts ...func(*accountView)) accountView {
	av := accountView{UID: uid, Nickname: nick, Credits: credits, Realm: "cn",
		ConsumeGroupEffective: "idle", ExpiresAt: time.Now().Unix() + 40*86400}
	for _, o := range opts {
		o(&av)
	}
	return av
}

// TestAggregatePoolDashCounts 计数口径：健康/冷却/停用互斥、分域、代挂、成长。
func TestAggregatePoolDashCounts(t *testing.T) {
	now := time.Now().Unix()
	v := aggregatePoolDash([]accountView{
		dashAcc("u1", "甲", 100),
		dashAcc("u2", "乙", 200, func(a *accountView) { a.Cooling = true }),
		dashAcc("u3", "丙", 300, func(a *accountView) { a.Disabled = true }),
		// manual_off 与 disabled 同属「停用」，不能各算一次。
		dashAcc("u4", "丁", 400, func(a *accountView) { a.ManualOff = true }),
		dashAcc("u5", "戊", 500, func(a *accountView) { a.Realm = "global"; a.OwnerID = 7 }),
		dashAcc("u6", "己", 600, func(a *accountView) { a.GrowthDoneAt = now - 100 }),
	})

	if v.Total != 6 {
		t.Fatalf("total 应为 6，得 %d", v.Total)
	}
	if v.Healthy != 3 {
		t.Fatalf("可用应为 3（u1/u5/u6），得 %d", v.Healthy)
	}
	if v.Cooling != 1 || v.Disabled != 2 {
		t.Fatalf("冷却 1 / 停用 2，得 %d/%d", v.Cooling, v.Disabled)
	}
	if v.CN != 5 || v.Global != 1 {
		t.Fatalf("国内 5 / 国际 1，得 %d/%d", v.CN, v.Global)
	}
	if v.Hosting != 1 || v.GrowthDone != 1 {
		t.Fatalf("代挂 1 / 已跑成长 1，得 %d/%d", v.Hosting, v.GrowthDone)
	}
	// 积分：合计与均值（100+200+300+400+500+600=2100，均值 350）。
	if v.CreditSum != 2100 || v.CreditAvg != 350 || v.CreditMax != 600 {
		t.Fatalf("积分 sum/avg/max 应为 2100/350/600，得 %d/%v/%d", v.CreditSum, v.CreditAvg, v.CreditMax)
	}
}

// TestAggregatePoolDashExpiryBuckets 到期分档必须**互斥且总数守恒** ——
// 分档漏项会让「7 天内到期」这类告警数字偏小，是最不容易发现的错误。
func TestAggregatePoolDashExpiryBuckets(t *testing.T) {
	now := time.Now().Unix()
	v := aggregatePoolDash([]accountView{
		dashAcc("e1", "过期", 1, func(a *accountView) { a.ExpiresAt = now - 10 }),
		dashAcc("e2", "三天后", 1, func(a *accountView) { a.ExpiresAt = now + 3*86400 }),
		dashAcc("e3", "第7天边界", 1, func(a *accountView) { a.ExpiresAt = now + 7*86400 }),   // 含在 d7
		dashAcc("e4", "第8天", 1, func(a *accountView) { a.ExpiresAt = now + 8*86400 }),     // d30
		dashAcc("e5", "第30天边界", 1, func(a *accountView) { a.ExpiresAt = now + 30*86400 }), // 含在 d30
		dashAcc("e6", "很久", 1, func(a *accountView) { a.ExpiresAt = now + 31*86400 }),     // later
		dashAcc("e7", "无期限", 1, func(a *accountView) { a.ExpiresAt = 0 }),                 // none
	})

	sum := v.Expiry["expired"] + v.Expiry["d7"] + v.Expiry["d30"] + v.Expiry["later"] + v.Expiry["none"]
	if sum != v.Total {
		t.Fatalf("到期分档应守恒（%d），得 %d：%v", v.Total, sum, v.Expiry)
	}
	if v.Expiry["expired"] != 1 || v.Expiry["d7"] != 2 || v.Expiry["d30"] != 2 ||
		v.Expiry["later"] != 1 || v.Expiry["none"] != 1 {
		t.Fatalf("分档结果不符预期：%v", v.Expiry)
	}
}

// TestAggregatePoolDashEgress 出口四态互斥（异常优先于其它三态）。
func TestAggregatePoolDashEgress(t *testing.T) {
	v := aggregatePoolDash([]accountView{
		dashAcc("g1", "未指定", 1),
		dashAcc("g2", "代理", 1, func(a *accountView) { a.Egress = "socks5://1.2.3.4:1080" }),
		dashAcc("g3", "直连", 1, func(a *accountView) { a.Egress = "direct" }),
		// 既是 direct 又有错误 → 应归 error（错误优先，否则「出口异常」会漏报）。
		dashAcc("g4", "异常", 1, func(a *accountView) { a.Egress = "direct"; a.EgressError = "代理不可达" }),
	})
	if v.Egress["none"] != 1 || v.Egress["proxy"] != 1 || v.Egress["direct"] != 1 || v.Egress["error"] != 1 {
		t.Fatalf("出口四态应各 1，得 %v", v.Egress)
	}
}

// TestAggregatePoolDashTaskCover 任务覆盖按「账号任务开关」计（关掉的不计）。
func TestAggregatePoolDashTaskCover(t *testing.T) {
	on := accountTaskState{Enabled: true, Kinds: func() []accountTaskKindView {
		out := make([]accountTaskKindView, 0, len(accountTaskKinds))
		for _, k := range accountTaskKinds {
			out = append(out, accountTaskKindView{Name: k, On: true})
		}
		return out
	}()}
	off := accountTaskState{Enabled: true, Kinds: func() []accountTaskKindView {
		out := make([]accountTaskKindView, 0, len(accountTaskKinds))
		for _, k := range accountTaskKinds {
			out = append(out, accountTaskKindView{Name: k, On: k != "checkin"})
		}
		return out
	}()}

	v := aggregatePoolDash([]accountView{
		dashAcc("t1", "全开", 1, func(a *accountView) { a.Task = on }),
		dashAcc("t2", "关签到", 1, func(a *accountView) { a.Task = off }),
	})
	if got := v.TaskCover["checkin"]; got != 1 {
		t.Fatalf("checkin 覆盖应为 1（t2 关掉了），得 %d", got)
	}
	if got := v.TaskCover["travel"]; got != 2 {
		t.Fatalf("travel 覆盖应为 2，得 %d", got)
	}
}

// TestAggregatePoolDashFingerprintAnomaly 指纹异常检测：空 uid 与派生碰撞都要报。
//
// 这是 v0.17.3 加的显性告警：两种情况都会让多个账号在平台侧是「同一台设备」，
// 而平时完全静默（账号照常出站、上游照常 200）。
func TestAggregatePoolDashFingerprintAnomaly(t *testing.T) {
	// 正常：uid 互异 → 无异常。
	clean := aggregatePoolDash([]accountView{
		dashAcc("u1", "甲", 1), dashAcc("u2", "乙", 1),
	})
	if clean.FingerprintAnomaly != 0 {
		t.Fatalf("正常账号不应报指纹异常，得 %d", clean.FingerprintAnomaly)
	}

	// 空 uid：派生不出标识。
	withEmpty := aggregatePoolDash([]accountView{
		dashAcc("u1", "甲", 1), dashAcc("", "没有 uid 的号", 1),
	})
	if withEmpty.FingerprintAnomaly != 1 {
		t.Fatalf("空 uid 应报 1 个异常，得 %d", withEmpty.FingerprintAnomaly)
	}
	if len(withEmpty.FingerprintAnomalyUIDs) != 1 || withEmpty.FingerprintAnomalyUIDs[0] != "没有 uid 的号" {
		t.Fatalf("应列出异常账号名，得 %v", withEmpty.FingerprintAnomalyUIDs)
	}

	// 两个空 uid → 都算异常（而不是「碰撞算一个」）。
	twoEmpty := aggregatePoolDash([]accountView{
		dashAcc("", "空A", 1), dashAcc("", "空B", 1),
	})
	if twoEmpty.FingerprintAnomaly != 2 {
		t.Fatalf("两个空 uid 应报 2 个异常，得 %d", twoEmpty.FingerprintAnomaly)
	}
}

// TestAggregatePoolDashTopBars 排行榜降序、上限 8 条；并列时按标签稳定排序
// （否则每 3 秒轮询一次，并列项的顺序会来回跳）。
func TestAggregatePoolDashTopBars(t *testing.T) {
	views := make([]accountView, 0, 12)
	for i := 0; i < 12; i++ {
		views = append(views, dashAcc(string(rune('a'+i)), string(rune('A'+i)), int64(i)*10))
	}
	v := aggregatePoolDash(views)
	if len(v.TopCredit) != poolDashTopN {
		t.Fatalf("积分榜应截断到 %d 条，得 %d", poolDashTopN, len(v.TopCredit))
	}
	for i := 1; i < len(v.TopCredit); i++ {
		if v.TopCredit[i-1].Value < v.TopCredit[i].Value {
			t.Fatalf("积分榜应降序，得 %v", v.TopCredit)
		}
	}
	if v.TopCredit[0].Value != 110 {
		t.Fatalf("榜首应为 110，得 %v", v.TopCredit[0].Value)
	}

	// 并列稳定：两个同分值账号，标签顺序必须确定。
	tie := aggregatePoolDash([]accountView{
		dashAcc("z", "同名B", 5), dashAcc("y", "同名A", 5),
	})
	if len(tie.TopCredit) != 2 || tie.TopCredit[0].Label > tie.TopCredit[1].Label {
		t.Fatalf("并列时应按标签升序稳定排列，得 %v", tie.TopCredit)
	}
}

// TestAggregatePoolDashEmptyPool 空号池不 panic、不产生 NaN（0 除）。
func TestAggregatePoolDashEmptyPool(t *testing.T) {
	v := aggregatePoolDash(nil)
	if v.Total != 0 || v.CreditAvg != 0 {
		t.Fatalf("空池应为 0，得 total=%d avg=%v", v.Total, v.CreditAvg)
	}
	if len(v.TaskCover) != len(accountTaskKinds) {
		t.Fatalf("任务覆盖应预置全部任务类（值为 0），得 %v", v.TaskCover)
	}
}

// TestDisplayNameFallback 展示名：昵称优先，回退 uid 前 8 位（避免榜单里出现空标签）。
func TestDisplayNameFallback(t *testing.T) {
	if got := (accountView{Nickname: "甲"}).displayName(); got != "甲" {
		t.Fatalf("有昵称应用昵称，得 %q", got)
	}
	// 注意：断言用的是 uid 的**前 8 位**，换了测试 uid 就必须同步改这里
	// （只替换完整 UUID 会漏掉这种部分引用 —— 本用例就是这样被抓出来的）。
	got := (accountView{UID: "11111111-1111-4111-8111-111111111111"}).displayName()
	if got != "11111111" || strings.Contains(got, "11111111-1111") {
		t.Fatalf("无昵称应用 uid 前 8 位，得 %q", got)
	}
	if got := (accountView{UID: "abc"}).displayName(); got != "abc" {
		t.Fatalf("短 uid 应原样返回，得 %q", got)
	}
}

// TestAggregatePoolDashCoolingAccounts 冷却明细（v0.17.8）：
// 环形图只给「冷却 N 个」，这里必须能回答「是哪几个、还剩多久」。
func TestAggregatePoolDashCoolingAccounts(t *testing.T) {
	v := aggregatePoolDash([]accountView{
		dashAcc("u1", "甲", 100),
		dashAcc("u2", "乙", 200, func(a *accountView) {
			a.Cooling = true
			a.CoolKind = "hard"
			a.CoolingFor = 90
			a.Reason = "upstream busy"
		}),
		dashAcc("u3", "丙", 300, func(a *accountView) {
			a.Cooling = true
			a.CoolKind = "soft"
			a.CoolingFor = 12
		}),
		// 停用账号即使带冷却标记也不该进冷却明细（与计数口径一致：互斥）。
		dashAcc("u4", "丁", 400, func(a *accountView) {
			a.Disabled = true
			a.Cooling = true
			a.CoolingFor = 5
		}),
	})

	if v.Cooling != 2 {
		t.Fatalf("冷却计数应为 2，得 %d", v.Cooling)
	}
	if len(v.CoolingAccounts) != 2 {
		t.Fatalf("冷却明细应有 2 条，得 %d（%+v）", len(v.CoolingAccounts), v.CoolingAccounts)
	}
	// 剩余时间升序：丙(12s) 在 乙(90s) 之前。
	if v.CoolingAccounts[0].Name != "丙" || v.CoolingAccounts[1].Name != "乙" {
		t.Fatalf("应按剩余时间升序，得 %+v", v.CoolingAccounts)
	}
	if v.CoolingAccounts[1].Kind != "hard" || v.CoolingAccounts[1].RemainingSec != 90 {
		t.Fatalf("类型/剩余时间不符：%+v", v.CoolingAccounts[1])
	}
	if v.CoolingAccounts[1].Reason != "upstream busy" {
		t.Fatalf("冷却原因应透出：%+v", v.CoolingAccounts[1])
	}
	if v.CoolingTruncated {
		t.Fatalf("2 条不该触发截断")
	}
	// 明细里不能出现停用的号。
	for _, c := range v.CoolingAccounts {
		if c.Name == "丁" {
			t.Fatalf("停用账号不应出现在冷却明细里")
		}
	}
}

// TestAggregatePoolDashCoolingTruncated 冷却明细上限 8，超出时置 truncated 标记。
func TestAggregatePoolDashCoolingTruncated(t *testing.T) {
	var views []accountView
	for i := 0; i < 12; i++ {
		views = append(views, dashAcc("cuid-"+strconv.Itoa(i), "号"+strconv.Itoa(i), 10, func(a *accountView) {
			a.Cooling = true
			a.CoolingFor = int64(30 + i)
		}))
	}
	v := aggregatePoolDash(views)
	if v.Cooling != 12 {
		t.Fatalf("计数应含全部 12 个，得 %d", v.Cooling)
	}
	if len(v.CoolingAccounts) != poolDashCoolingMax {
		t.Fatalf("明细应截到 %d 条，得 %d", poolDashCoolingMax, len(v.CoolingAccounts))
	}
	if !v.CoolingTruncated {
		t.Fatalf("超过上限应置 CoolingTruncated")
	}
}

// TestAggregatePoolDashNoCooling 无冷却时不 panic 且明细为空。
func TestAggregatePoolDashNoCooling(t *testing.T) {
	v := aggregatePoolDash([]accountView{dashAcc("u1", "甲", 100)})
	if v.Cooling != 0 || len(v.CoolingAccounts) != 0 || v.CoolingTruncated {
		t.Fatalf("无冷却时应为空：%+v", v)
	}
}

// TestAggregatePoolDashDisabledAccounts 停用明细（v0.23.3）：
// 此前停用只累加数字，运营看不到「是哪几个、为什么停」。这里锁死三件事：
//  1. 手动停用与系统禁用都进明细，且原因区分开；
//  2. 排序把**人工**停用排在前（运营找的通常是自己停的那几个）；
//  3. 停用与冷却**互斥** —— 停用号不能同时出现在冷却明细里（计数口径一致）。
func TestAggregatePoolDashDisabledAccounts(t *testing.T) {
	v := aggregatePoolDash([]accountView{
		dashAcc("u1", "甲", 100),
		// 系统禁用（带具体原因）。
		dashAcc("u2", "乙", 200, func(a *accountView) {
			a.Disabled = true
			a.Reason = "session dead"
		}),
		// 手动停用。
		dashAcc("u3", "丙", 300, func(a *accountView) { a.ManualOff = true }),
		// 系统禁用但池内没记原因。
		dashAcc("u4", "丁", 400, func(a *accountView) { a.Disabled = true }),
		// 停用 + 冷却标记：应只出现在停用明细。
		dashAcc("u5", "戊", 500, func(a *accountView) {
			a.ManualOff = true
			a.Cooling = true
			a.CoolingFor = 3
		}),
	})

	if v.Disabled != 4 {
		t.Fatalf("停用计数应为 4，得 %d", v.Disabled)
	}
	if len(v.DisabledAccounts) != 4 {
		t.Fatalf("停用明细应有 4 条，得 %d（%+v）", len(v.DisabledAccounts), v.DisabledAccounts)
	}
	if len(v.CoolingAccounts) != 0 {
		t.Fatalf("停用号不该进冷却明细，得 %+v", v.CoolingAccounts)
	}
	if v.DisabledTruncated {
		t.Fatalf("4 条不该触发截断")
	}
	// v0.23.4：明细必须带 uid —— 前端「启用 / 复活」按钮要拿它调端点。
	// 只下发展示名的话，明细就只能看不能操作（这正是修复前的状态）。
	for _, d := range v.DisabledAccounts {
		if d.UID == "" {
			t.Fatalf("停用明细缺少 uid，前端无法一键启用：%+v", d)
		}
	}
	// 人工停用（丙、戊）排在前两位，按名排序 → 丙 在 戊 之前。
	if !v.DisabledAccounts[0].Manual || !v.DisabledAccounts[1].Manual {
		t.Fatalf("人工停用应排在前，得 %+v", v.DisabledAccounts)
	}
	if v.DisabledAccounts[0].Name != "丙" || v.DisabledAccounts[1].Name != "戊" {
		t.Fatalf("人工停用组内应按名排序，得 %+v", v.DisabledAccounts)
	}
	if v.DisabledAccounts[0].Reason != "手动停用" {
		t.Fatalf("手动停用的原因应为「手动停用」，得 %q", v.DisabledAccounts[0].Reason)
	}
	// 系统禁用排在后，组内按名（Go 字符串比较 = Unicode 码点序）：
	// 丁 U+4E01 < 乙 U+4E59 → 丁 在前、乙 在后。
	if v.DisabledAccounts[2].Manual || v.DisabledAccounts[3].Manual {
		t.Fatalf("系统禁用应排在后，得 %+v", v.DisabledAccounts)
	}
	if v.DisabledAccounts[2].Name != "丁" || v.DisabledAccounts[3].Name != "乙" {
		t.Fatalf("系统禁用组内应按名排序（丁 < 乙），得 %+v", v.DisabledAccounts)
	}
	if v.DisabledAccounts[2].Reason != "系统禁用" {
		t.Fatalf("无具体原因时回落「系统禁用」，得 %q", v.DisabledAccounts[2].Reason)
	}
	if !strings.Contains(v.DisabledAccounts[3].Reason, "session dead") {
		t.Fatalf("系统禁用应带出池内原因，得 %q", v.DisabledAccounts[3].Reason)
	}
}

// TestAggregatePoolDashDisabledTruncated 停用明细上限 8，超出置 truncated。
func TestAggregatePoolDashDisabledTruncated(t *testing.T) {
	var views []accountView
	for i := 0; i < 12; i++ {
		views = append(views, dashAcc("duid-"+strconv.Itoa(i), "停"+strconv.Itoa(i), 10, func(a *accountView) {
			a.Disabled = true
		}))
	}
	v := aggregatePoolDash(views)
	if v.Disabled != 12 {
		t.Fatalf("计数应含全部 12 个，得 %d", v.Disabled)
	}
	if len(v.DisabledAccounts) != poolDashDisabledMax {
		t.Fatalf("明细应截到 %d 条，得 %d", poolDashDisabledMax, len(v.DisabledAccounts))
	}
	if !v.DisabledTruncated {
		t.Fatalf("超过上限应置 DisabledTruncated")
	}
}

// TestAggregatePoolDashNoDisabled 无停用时明细为空且不 panic。
func TestAggregatePoolDashNoDisabled(t *testing.T) {
	v := aggregatePoolDash([]accountView{dashAcc("u1", "甲", 100)})
	if v.Disabled != 0 || len(v.DisabledAccounts) != 0 || v.DisabledTruncated {
		t.Fatalf("无停用时应为空：%+v", v.DisabledAccounts)
	}
}

// ======================================================================
// v0.26.5：模型级冷却明细（用户口径「软冷却模型的时候冷却账号明细是否显示」）
// ======================================================================

// TestAggregatePoolDashModelCooling 模型级冷却必须**单独成表**且不影响账号级计数。
//
// 这是本轮用户直接问的那件事：6004 只挡一个模型，账号对其它模型照常服务 ——
// 既然行为如此，就必须**看得出来**它如此，否则"行为正确"没人能确认。
// 而且它**不能**混进「冷却中」计数：那是"整个号不可用"，两者数量级不同。
func TestAggregatePoolDashModelCooling(t *testing.T) {
	now := time.Now().Unix()
	views := []accountView{
		{UID: "u1", Nickname: "甲", Credits: 100,
			LimitedMods: 1,
			LimitedDetail: []limitedModelView{
				{Model: "glm-5.3", Until: now + 3600, ResetAt: now + 3600, Reason: "6004 [reset=body-text]"},
			}},
		// 同一账号两个模型被限 → 两条明细，但"受影响账号数"只算一个。
		{UID: "u2", Nickname: "乙", Credits: 100,
			LimitedDetail: []limitedModelView{
				{Model: "glm-5.3", Until: now + 600, Reason: "6004 [reset=none→退避]"},
				{Model: "kimi-k3-1", Until: now + 9000, ResetAt: now + 9000},
			}},
		// 账号级冷却（与模型级正交）：必须只进 Cooling，不进 ModelCooling。
		{UID: "u3", Nickname: "丙", Credits: 100, Cooling: true, CoolKind: "soft", CoolingFor: 120},
		{UID: "u4", Nickname: "丁", Credits: 100},
	}
	v := aggregatePoolDash(views)

	if len(v.ModelCooling) != 3 {
		t.Fatalf("模型级冷却应有 3 条 (账号,模型) 对，得 %d：%+v", len(v.ModelCooling), v.ModelCooling)
	}
	if v.ModelCoolingAccounts != 2 {
		t.Errorf("受影响账号数应为 2（甲、乙；同一账号多模型只算一个），得 %d", v.ModelCoolingAccounts)
	}
	// 排序：剩余时间升序（最先恢复的在前）。
	if v.ModelCooling[0].Model != "glm-5.3" || v.ModelCooling[0].Name != "乙" {
		t.Errorf("首条应为剩余最短的乙/glm-5.3，得 %+v", v.ModelCooling[0])
	}
	if got := v.ModelCooling[0].RemainingSec; got < 590 || got > 610 {
		t.Errorf("剩余秒数应约 600，得 %d", got)
	}
	if v.ModelCooling[0].Source != "local" {
		t.Errorf("无 reset_at 的条目来源应为 local，得 %q", v.ModelCooling[0].Source)
	}
	if v.ModelCooling[2].Source != "upstream" {
		t.Errorf("有 reset_at 的条目来源应为 upstream，得 %q", v.ModelCooling[2].Source)
	}
	// 不得因模型级冷却把账号算成"不健康"或"冷却中"。
	if v.Cooling != 1 {
		t.Errorf("冷却数应只含账号级那 1 个，得 %d", v.Cooling)
	}
	// 甲/乙/丁 三个都算可用：模型级冷却不吃账号可用性（只有账号级的丙进 cooling）。
	if v.Healthy != 3 {
		t.Errorf("甲/乙/丁 应仍算可用（模型级冷却不吃账号可用性），healthy=%d", v.Healthy)
	}
	if v.Disabled != 0 {
		t.Errorf("不该有停用，得 %d", v.Disabled)
	}
}

// TestAggregatePoolDashModelCoolingTruncated 超过上限时截断 + 打标记，且顺序稳定。
func TestAggregatePoolDashModelCoolingTruncated(t *testing.T) {
	now := time.Now().Unix()
	views := make([]accountView, 0, 20)
	for i := 0; i < 20; i++ {
		// 刻意用 strconv 而不是本包的 itoa：那个 helper 是给"连续失败次数"用的，
		// n<=0 会钳成 "3"（见 tasks.go），拿它拼 uid 会让 i=0 与 i=3 撞成同一个 uid。
		views = append(views, accountView{
			UID: "u" + strconv.Itoa(i), Nickname: "n" + strconv.Itoa(i),
			// 同一个剩余时长 → 必须靠 (名, 模型) 稳定排序，否则 3 秒轮询会看到顺序乱跳。
			LimitedDetail: []limitedModelView{{Model: "m", Until: now + 60}},
		})
	}
	v := aggregatePoolDash(views)
	if !v.ModelCoolingTruncated {
		t.Error("超过上限应打 truncated 标记")
	}
	if len(v.ModelCooling) != poolDashModelCoolingMax {
		t.Fatalf("应截断到 %d 条，得 %d", poolDashModelCoolingMax, len(v.ModelCooling))
	}
	if v.ModelCoolingAccounts != 20 {
		t.Errorf("受影响账号数仍应是全量 20，得 %d", v.ModelCoolingAccounts)
	}
	if v.ModelCooling[0].Name != "n0" || v.ModelCooling[1].Name != "n1" {
		t.Errorf("同级应按名升序稳定排序，得 %q, %q", v.ModelCooling[0].Name, v.ModelCooling[1].Name)
	}
}

// TestAggregatePoolDashNoModelCooling 空态：必须是**空切片**而不是 nil
// （前端直接读数组，nil 会让 JSON 里该字段变成 null 而不是 []）。
func TestAggregatePoolDashNoModelCooling(t *testing.T) {
	v := aggregatePoolDash([]accountView{{UID: "u1", Credits: 1}})
	if v.ModelCooling == nil {
		t.Error("无模型级冷却时也应回空切片（JSON 里是 [] 不是 null）")
	}
	if len(v.ModelCooling) != 0 || v.ModelCoolingAccounts != 0 || v.ModelCoolingTruncated {
		t.Errorf("空态不符：%+v", v)
	}
}

// TestAggregatePoolDashModelCoolingExpiredClamped 已到期的条目剩余时间不能是负数
// （负值会让界面显示 "-1分" 这类荒唐文案）。
func TestAggregatePoolDashModelCoolingExpiredClamped(t *testing.T) {
	now := time.Now().Unix()
	v := aggregatePoolDash([]accountView{{
		UID: "u1", Credits: 1,
		LimitedDetail: []limitedModelView{{Model: "m", Until: now - 500}},
	}})
	if len(v.ModelCooling) != 1 {
		t.Fatalf("应有 1 条，得 %d", len(v.ModelCooling))
	}
	if v.ModelCooling[0].RemainingSec != 0 {
		t.Errorf("已到期应钳到 0，得 %d", v.ModelCooling[0].RemainingSec)
	}
}
