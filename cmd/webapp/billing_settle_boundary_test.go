// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"strings"
	"testing"
)

// ======================================================================
// 计费结算的**边界**回归网（v0.31.10，P2-7 第三刀的前置）
//
// 方案文档写明：第三刀\"涉及钱，必须先有回归网\"。既有 billing_test.go /
// billing_basis_test.go 覆盖了主路径（预扣上限、单价不双重计倍率、分成以实收封顶），
// 本文件补的是**边界与守卫**——这些分支平时不跑，一旦跑错就是静默的钱的偏差：
//
//	E1 预扣下限：单价极小时也必须至少冻 1 微点（估成 0 = 预扣形同虚设）；
//	E2 预扣上限：kv 里的 reserve_max_point 是**点**，必须折成微点再比；
//	E3 倍率：观测单价（积分/1k）必须乘倍率；兜底单价（点/1k）不得再乘；
//	E4 0 token 的响应**不得**污染单价观测表（除零得到 +Inf 会让后续预扣算出天文数字）；
//	E5 代挂自用 Key：零扣费、不动余额、流水标 host scope 与 [host_self]；
//	E6 管理通道（无用户）：流水落 user_id=0、零扣费；
//	E7 上游没回报 credit 且无观测 → 标记 [missing_credit] 且按 0 计（不凭空扣费）；
//	E8 分成守卫：非代挂 / 未入池 / 比例为 0 → 一分不入账；
//	E9 返点不侵占平台留成（端到端，看真实入账额）；
//	E10 自荐守卫：referred_by == 自己 → 不发返点。
// ======================================================================

// reservePayload 造一个 token 数可预期的 payload：prompt 估算 6 + max_tokens 可控。
// （EstimatePromptTokens: 每条消息 4 结构 + content "abcd" 的 4/3+1=2 → 6）
func reservePayload(maxTokens int) map[string]any {
	return map[string]any{
		"messages":   []any{map[string]any{"role": "user", "content": "abcd"}},
		"max_tokens": float64(maxTokens),
	}
}

// E1：预扣下限。单价小到折算后不足 1 微点时必须抬到 1（否则预扣等于没有）。
func TestReserveMicroFloorAtProxyLevel(t *testing.T) {
	store, px := newHostProxy(t)
	defer store.Close()

	// 0.0001 积分/1k，倍率默认 1 → 0.0001 点/1k；1000 token → 0.1 微点 → 四舍五入 0。
	px.observeCost("m-floor", 0.0001)
	if got := px.estimateReserveMicro(reservePayload(994), "m-floor"); got != 1 {
		t.Fatalf("折算不足 1 微点应抬到下限 1，得 %d", got)
	}
}

// E2：上限。kv 的 reserve_max_point 单位是点，必须折成微点后再比较。
func TestReserveMicroCapFromKVIsPoints(t *testing.T) {
	store, px := newHostProxy(t)
	defer store.Close()

	if err := store.SetKV(settingReserveMax, "2"); err != nil { // 2 点 = 2000 微点
		t.Fatalf("SetKV: %v", err)
	}
	// 100 积分/1k → 100 点/1k → 1000 token → 100 点 = 100000 微点，应被压到 2000。
	px.observeCost("m-cap", 100)
	if got := px.estimateReserveMicro(reservePayload(994), "m-cap"); got != 2000 {
		t.Fatalf("上限 2 点应为 2000 微点，得 %d", got)
	}
	// 非法上限（负数）→ 视为不限。
	if err := store.SetKV(settingReserveMax, "-1"); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	if got := px.estimateReserveMicro(reservePayload(994), "m-cap"); got != 100000 {
		t.Fatalf("负数上限应视为不限，得 %d", got)
	}
}

// E3：倍率。观测单价是「积分/1k」，必须 × 倍率换成点。
func TestReserveMicroAppliesMultiplier(t *testing.T) {
	store, px := newHostProxy(t)
	defer store.Close()

	if err := store.SetKV(settingMultiplier, "3"); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	// 2 积分/1k × 3 = 6 点/1k；1000 token → 6 点 = 6000 微点。
	px.observeCost("m-mult", 2)
	if got := px.estimateReserveMicro(reservePayload(994), "m-mult"); got != 6000 {
		t.Fatalf("应预扣 6 点 = 6000 微点，得 %d", got)
	}
}

// E4：0 token 的响应不得污染单价观测表。
//
// 为什么这是**边界**：上游回报 credit 但不给 token（或解析不到 token）时，
// `credit / 0` 在 Go 里是 +Inf。一旦 +Inf 写进观测表，后续每次预扣都会算出天文数字，
// 全部用户被 402 误拒 —— 且不会报任何错。所以这里既看"没写入"，也顺手钉住"不是 Inf"。
func TestObserveCostNotPollutedByZeroToken(t *testing.T) {
	store, px := newHostProxy(t)
	defer store.Close()

	u, err := store.CreateUser("zero-token-payer", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := store.AddUserBalance(u.ID, 1000*MicroPerPoint); err != nil {
		t.Fatalf("AddUserBalance: %v", err)
	}

	// 上游给了 credit，但两个 token 计数都是 0。
	px.settleUsage(u, nil, "m-zerotok", "uid-1",
		usageInfo{Credit: 5, HasCredit: true}, nil, "ok", "", "", 1)

	px.costMu.RLock()
	per1k, seen := px.costPer1k["m-zerotok"]
	px.costMu.RUnlock()
	if seen {
		t.Fatalf("0 token 不得写入观测表（否则是除零得到的 Inf），得 %v", per1k)
	}
	// 观测表空 → 预扣仍是"无单价不预扣"，不会因 Inf 误拒用户。
	if got := px.estimateReserveMicro(reservePayload(100), "m-zerotok"); got != 0 {
		t.Fatalf("无有效观测时不应预扣，得 %d", got)
	}
}

// E5：代挂自用 Key —— 免费、不动余额、流水可辨认。
func TestSettleUsageHostSelfKeyIsFree(t *testing.T) {
	store, px := newHostProxy(t)
	defer store.Close()

	u, err := store.CreateUser("host-key-owner", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := store.AddUserBalance(u.ID, 100*MicroPerPoint); err != nil {
		t.Fatalf("AddUserBalance: %v", err)
	}
	key, err := store.CreateAPIKey(u.ID, "host", HashAPIKey("sk-host-self"), "enc", "sk-h",
		KeyKindHost, "", "h_self")
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	settled, charged := px.settleUsage(u, key, "m-hostself", "h_self",
		usageInfo{PromptTokens: 100, CompletionTokens: 50, Credit: 3, HasCredit: true},
		nil, "ok", "", "", 1)
	if !settled {
		t.Fatal("代挂自用也应算作已结算（只写流水不扣钱）")
	}
	if charged != 0 {
		t.Fatalf("代挂自用必须零扣费，得 %d", charged)
	}
	fresh, _ := store.GetUserByID(u.ID)
	if fresh.BalanceMicro != 100*MicroPerPoint {
		t.Fatalf("代挂自用不得动余额，得 %d", fresh.BalanceMicro)
	}
	logs, err := store.ListUsageLogs(u.ID, 5, 0)
	if err != nil || len(logs) == 0 {
		t.Fatalf("应写入一条流水，err=%v n=%d", err, len(logs))
	}
	if logs[0].Scope != ScopeHost {
		t.Errorf("流水 scope 应为 %q，得 %q", ScopeHost, logs[0].Scope)
	}
	if !strings.Contains(logs[0].ErrorMsg, "[host_self]") {
		t.Errorf("流水应带 [host_self] 标记便于对账，得 %q", logs[0].ErrorMsg)
	}
	if logs[0].UpstreamCredit != 3 {
		t.Errorf("号池积分消耗仍要如实记录（它是号主收益的基数），得 %v", logs[0].UpstreamCredit)
	}
}

// E6：管理通道（无用户）—— 流水落 user_id=0，零扣费。
//
// 这条链路的实际缺陷形态（v0.17.6 修过）：用管理密钥发的请求在流水里**完全不存在**，
// 于是仪表盘对自测流量视而不见，排查的人会误判"仪表盘坏了"。
func TestSettleUsageAdminChannelNoCharge(t *testing.T) {
	store, px := newHostProxy(t)
	defer store.Close()

	settled, charged := px.settleUsage(nil, nil, "m-admin-ch", "uid-1",
		usageInfo{PromptTokens: 10, CompletionTokens: 20, Credit: 7, HasCredit: true},
		nil, "ok", "", "", 1)
	if !settled || charged != 0 {
		t.Fatalf("管理通道应为 settled=true charged=0，得 settled=%v charged=%d", settled, charged)
	}
	logs, err := store.ListUsageLogs(0, 5, 0) // user_id = 0 即无归属
	if err != nil || len(logs) == 0 {
		t.Fatalf("管理通道必须留一条 user_id=0 的流水，err=%v n=%d", err, len(logs))
	}
	if logs[0].ChargedMicro != 0 {
		t.Errorf("管理通道不得收费，得 %d", logs[0].ChargedMicro)
	}
	if logs[0].UpstreamCredit != 7 {
		t.Errorf("号池消耗仍要记（号池事实与向谁收费无关），得 %v", logs[0].UpstreamCredit)
	}
	if logs[0].Scope != ScopePool {
		t.Errorf("管理通道走的是平台号池，scope 应为 %q，得 %q", ScopePool, logs[0].Scope)
	}
}

// E7：上游没回报 credit 且无任何观测 → 标记 [missing_credit] 且按 0 计。
//
// 「按 0 计」而不是「猜一个」是有意的：猜就是凭空扣用户的钱。
// 但必须留标记，否则"漏计"在报表里看不出来（对账时才发现，已经晚了几周）。
func TestSettleUsageMissingCreditMarked(t *testing.T) {
	store, px := newHostProxy(t)
	defer store.Close()

	u, err := store.CreateUser("missing-credit-payer", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := store.AddUserBalance(u.ID, 100*MicroPerPoint); err != nil {
		t.Fatalf("AddUserBalance: %v", err)
	}

	// HasCredit=false 且模型没有任何观测 → estimateCredit 返回 0。
	_, charged := px.settleUsage(u, nil, "m-nocredit", "uid-1",
		usageInfo{PromptTokens: 100, CompletionTokens: 50}, nil, "ok", "", "", 1)
	if charged != 0 {
		t.Fatalf("估不出用量时应按 0 计（不凭空扣费），得 %d", charged)
	}
	logs, err := store.ListUsageLogs(u.ID, 5, 0)
	if err != nil || len(logs) == 0 {
		t.Fatalf("应有流水，err=%v", err)
	}
	if !strings.Contains(logs[0].ErrorMsg, "[missing_credit]") {
		t.Fatalf("漏计必须留标记，得 %q", logs[0].ErrorMsg)
	}
}

// E8：分成守卫 —— 非代挂 / 未入池 / 比例为 0 时一分都不入账。
//
// 三种情形是三个不同的判断（owner<=0 / !inPool / ratio<=0），全都不该给钱。
func TestCreditHostOwnerGuards(t *testing.T) {
	cases := []struct {
		name       string
		uid        string
		ownerYes   bool   // 是否挂到号主
		poolState  string // 账号在池状态
		shareRatio string // 全局分成比例
	}{
		{"非代挂（无号主）", "g_noowner", false, "in", "0.6"},
		{"有号主但未入池", "g_outpool", true, "out", "0.6"},
		{"池状态为空串（视为未入池）", "g_empty", true, "", "0.6"},
		{"分成比例为 0", "g_zeroratio", true, "in", "0"},
		{"比例非法", "g_badratio", true, "in", "abc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store, px := newHostProxy(t)
			defer store.Close()

			owner, err := store.CreateUser("owner-"+c.uid, "x")
			if err != nil {
				t.Fatalf("CreateUser: %v", err)
			}
			acc := &Account{UID: c.uid, Realm: "cn", AccessToken: "a", PoolState: c.poolState}
			if c.ownerYes {
				acc.OwnerUserID = owner.ID
			}
			if err := store.UpsertAccount(acc); err != nil {
				t.Fatalf("UpsertAccount: %v", err)
			}
			if err := store.SetKV(kvHostShareRatio, c.shareRatio); err != nil {
				t.Fatalf("SetKV: %v", err)
			}
			px.ReloadSelection()
			_ = px.SyncAccounts()

			px.creditHostOwner(c.uid, nil, "m", 10, 20000)

			fresh, _ := store.GetUserByID(owner.ID)
			if fresh.BalanceMicro != 0 {
				t.Fatalf("该情形不应入账（%s），得 %d 微点", c.name, fresh.BalanceMicro)
			}
		})
	}
}

// E9：分成 + 返点一起吃也不超过平台实收（端到端，看真实入账额）。
//
// 这条是**钱的硬约束**：只要它破了，平台就是每笔倒贴。包级用例已覆盖公式，
// 这里覆盖"接线之后"的整链（号主 → 上级 → 余额/流水四个落点）。
func TestReferralRebateClampedToPlatformMargin(t *testing.T) {
	store, px := newHostProxy(t)
	defer store.Close()

	up, err := store.CreateUser("referrer-up", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	owner, err := store.CreateUser("referrer-owner", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := store.SetUserReferredBy(owner.ID, up.ID); err != nil {
		t.Fatalf("SetUserReferredBy: %v", err)
	}
	if err := store.UpsertAccount(&Account{
		UID: "r_in", Realm: "cn", AccessToken: "a", OwnerUserID: owner.ID, PoolState: "in",
	}); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	if err := store.SetKV(kvHostShareRatio, "0.6"); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	if err := store.SetKV(kvReferralRatio, "0.9"); err != nil { // 上级抽 90%
		t.Fatalf("SetKV: %v", err)
	}
	px.ReloadSelection()
	_ = px.SyncAccounts()

	// consumedCredit=10 → 名义分成 6 点 = 6000 微点；但平台实收只有 6500 微点
	// → 留成仅 500，返点名义 5400 必须被钳到 500。
	const charged = int64(6500)
	px.creditHostOwner("r_in", nil, "m", 10, charged)

	ownerFresh, _ := store.GetUserByID(owner.ID)
	upFresh, _ := store.GetUserByID(up.ID)
	if ownerFresh.BalanceMicro != 6000 {
		t.Fatalf("号主应得 6000 微点（6 点），得 %d", ownerFresh.BalanceMicro)
	}
	if upFresh.BalanceMicro != 500 {
		t.Fatalf("返点应被钳到平台留成 500，得 %d", upFresh.BalanceMicro)
	}
	if ownerFresh.BalanceMicro+upFresh.BalanceMicro > charged {
		t.Fatalf("分成+返点 %d 超过平台实收 %d，平台会倒贴",
			ownerFresh.BalanceMicro+upFresh.BalanceMicro, charged)
	}
}

// E10：自荐守卫 —— referred_by 指向自己时不得发返点（否则一笔收益被自己吃两次）。
//
// SetUserReferredBy 本身拒绝自荐，所以这里直接改库造出该状态 ——
// 守卫的意义正是在**脏数据**（历史遗留 / 直接改库）下也不倒贴。
func TestReferrerSelfReferralSkipped(t *testing.T) {
	store, px := newHostProxy(t)
	defer store.Close()

	owner, err := store.CreateUser("self-ref-owner", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := store.db.Exec(`UPDATE users SET referred_by = ? WHERE id = ?`,
		owner.ID, owner.ID); err != nil {
		t.Fatalf("造脏数据: %v", err)
	}
	if err := store.UpsertAccount(&Account{
		UID: "sr_in", Realm: "cn", AccessToken: "a", OwnerUserID: owner.ID, PoolState: "in",
	}); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	if err := store.SetKV(kvHostShareRatio, "0.6"); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	if err := store.SetKV(kvReferralRatio, "0.9"); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	px.ReloadSelection()
	_ = px.SyncAccounts()

	px.creditHostOwner("sr_in", nil, "m", 10, 20000)

	fresh, _ := store.GetUserByID(owner.ID)
	if fresh.BalanceMicro != 6000 {
		t.Fatalf("自荐时只应得分成 6000 微点（不得再给自己发返点），得 %d", fresh.BalanceMicro)
	}
}
