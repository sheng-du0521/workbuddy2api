// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// TestRatioOverrideResolution 逐用户返点比例的解析顺序：覆盖优先，否则全局。
//
// 这是「对用户返点进行调整」的核心语义。三条边界必须同时成立：
//   - 未覆盖（-1）→ 用全局
//   - 覆盖为 0     → **不**回落到全局（0 是「明确不分成」，与未覆盖语义不同）
//   - 覆盖为正值   → 用覆盖值
//
// 第 2 条最容易写错（用 `if v > 0` 判断会让「设为 0%」静默失效回落到全局）。
func TestRatioOverrideResolution(t *testing.T) {
	store, px, a := newTestApp(t)
	defer store.Close()
	defer px.Close()

	// 全局比例：分成 0.6 / 推广 0.1
	if err := store.SetKV(kvHostShareRatio, "0.6"); err != nil {
		t.Fatalf("SetKV host: %v", err)
	}
	if err := store.SetKV(kvReferralRatio, "0.1"); err != nil {
		t.Fatalf("SetKV referral: %v", err)
	}
	px.ReloadSelection()

	u, err := store.CreateUser("ratio-user", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// ── 未覆盖：跟随全局 ──
	if got := px.hostRatioFor(u.ID); got != 0.6 {
		t.Fatalf("未覆盖时号池分成应为全局 0.6，得 %v", got)
	}
	if got := px.referralRatioFor(u.ID); got != 0.1 {
		t.Fatalf("未覆盖时推广返点应为全局 0.1，得 %v", got)
	}

	// ── 覆盖为 0：必须生效，不得回落全局 ──
	zero := 0.0
	if err := store.SetUserRatios(u.ID, &zero, nil); err != nil {
		t.Fatalf("SetUserRatios(0): %v", err)
	}
	if got := px.hostRatioFor(u.ID); got != 0 {
		t.Fatalf("覆盖 0 必须生效（明确不分成），得 %v —— 若用 v>0 判空会静默回落到全局", got)
	}
	if got := px.referralRatioFor(u.ID); got != 0.1 {
		t.Fatalf("未改的推广返点仍应跟随全局 0.1，得 %v", got)
	}

	// ── 覆盖为正值 ──
	half := 0.25
	if err := store.SetUserRatios(u.ID, &half, nil); err != nil {
		t.Fatalf("SetUserRatios(0.25): %v", err)
	}
	if got := px.hostRatioFor(u.ID); got != 0.25 {
		t.Fatalf("覆盖应为 0.25，得 %v", got)
	}

	// ── 清除覆盖（-1）→ 回到全局 ──
	clear := RatioNotSet
	if err := store.SetUserRatios(u.ID, &clear, &clear); err != nil {
		t.Fatalf("SetUserRatios(clear): %v", err)
	}
	if got := px.hostRatioFor(u.ID); got != 0.6 {
		t.Fatalf("清除覆盖后应回到全局 0.6，得 %v", got)
	}
	if got := px.referralRatioFor(u.ID); got != 0.1 {
		t.Fatalf("清除覆盖后应回到全局 0.1，得 %v", got)
	}
	_ = a
}

// TestRatioRangeValidated 越界比例必须被拒绝（不静默钳制）。
func TestRatioRangeValidated(t *testing.T) {
	store, px, _ := newTestApp(t)
	defer store.Close()
	defer px.Close()

	u, _ := store.CreateUser("range-user", "x")
	for _, bad := range []float64{-2, 1.5, 100} {
		v := bad
		if err := store.SetUserRatios(u.ID, &v, nil); err == nil {
			t.Fatalf("比例 %v 越界应被拒绝", bad)
		}
	}
	// 边界值 0 与 1 合法。
	for _, ok := range []float64{0, 1} {
		v := ok
		if err := store.SetUserRatios(u.ID, &v, nil); err != nil {
			t.Fatalf("比例 %v 应合法，得错 %v", ok, err)
		}
	}
}

// TestRatioOverrideAffectsHostEarning 覆盖比例必须真正改变入账金额。
//
// 这是端到端语义：光解析对不够，得让钱真的按新比例走。
// 用 creditHostOwner 直测（它是转发热路径上的入账函数）。
func TestRatioOverrideAffectsHostEarning(t *testing.T) {
	store, px, _ := newTestApp(t)
	defer store.Close()
	defer px.Close()

	// 全局分成 0.6
	if err := store.SetKV(kvHostShareRatio, "0.6"); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	if err := store.SetKV(kvHostShareRatio, "0.6"); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	px.ReloadSelection()

	owner, _ := store.CreateUser("host-owner", "x")
	acc := &Account{
		UID: "earn-uid-1", Nickname: "earn-acct", Realm: "cn",
		AccessToken: "t", RefreshToken: "r", OwnerUserID: owner.ID,
		PoolState: "in", ConsumeGroup: "apply",
	}
	if err := store.UpsertAccount(acc); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	if err := px.SyncAccounts(); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}

	// 消耗 100 积分、平台实收足够大（不作上限）。
	// 全局 0.6 → 收益 60 点 = 60000 微点。
	px.creditHostOwner("earn-uid-1", nil, "m", 100, PointToMicro(1000))
	fresh, _ := store.GetUserByID(owner.ID)
	if fresh.BalanceMicro != PointToMicro(60) {
		t.Fatalf("全局 0.6 → 应入账 60 点，得 %d 微点", fresh.BalanceMicro)
	}

	// 覆盖为 1.0 → 下一笔应全额（100 点）。
	full := 1.0
	if err := store.SetUserRatios(owner.ID, &full, nil); err != nil {
		t.Fatalf("SetUserRatios: %v", err)
	}
	px.creditHostOwner("earn-uid-1", nil, "m", 100, PointToMicro(1000))
	fresh2, _ := store.GetUserByID(owner.ID)
	if got := fresh2.BalanceMicro - fresh.BalanceMicro; got != PointToMicro(100) {
		t.Fatalf("覆盖 1.0 → 第二笔应入账 100 点，得 %d 微点（说明覆盖未生效于入账）", got)
	}

	// 覆盖为 0 → 不再入账。
	zero := 0.0
	if err := store.SetUserRatios(owner.ID, &zero, nil); err != nil {
		t.Fatalf("SetUserRatios(0): %v", err)
	}
	px.creditHostOwner("earn-uid-1", nil, "m", 100, PointToMicro(1000))
	fresh3, _ := store.GetUserByID(owner.ID)
	if fresh3.BalanceMicro != fresh2.BalanceMicro {
		t.Fatalf("覆盖 0 → 不应再入账，得 +%d 微点", fresh3.BalanceMicro-fresh2.BalanceMicro)
	}
}

// TestAdminRatioEndpointsAPI 管理端比例接口的 HTTP 契约（查看 / 编辑 / 清除）。
func TestAdminRatioEndpointsAPI(t *testing.T) {
	store, px, a := newTestApp(t)
	defer store.Close()
	defer px.Close()

	if err := store.SetKV(kvHostShareRatio, "0.5"); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	px.ReloadSelection()
	u, _ := store.CreateUser("api-user", "x")

	get := func() map[string]any {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/api/admin/users/1/ratios", nil)
		r.SetPathValue("id", strconv.FormatInt(u.ID, 10))
		a.handleAdminGetUserRatios(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET ratios 应 200，得 %d: %s", rec.Code, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("解析 GET 响应: %v", err)
		}
		return out
	}

	// 初始：跟随全局
	d := get()
	if hr := d["host_ratio"].(map[string]any); hr["is_custom"].(bool) {
		t.Fatal("初始不应为单独设定")
	} else if hr["effective"].(float64) != 0.5 {
		t.Fatalf("生效值应为全局 0.5，得 %v", hr["effective"])
	}

	post := func(body string) map[string]any {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/admin/users/1/ratios", strings.NewReader(body))
		r.SetPathValue("id", strconv.FormatInt(u.ID, 10))
		a.handleAdminSetUserRatios(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST ratios 应 200，得 %d: %s", rec.Code, rec.Body.String())
		}
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}

	// 编辑为 0.8
	d = post(`{"host_ratio":0.8}`)
	hr := d["host_ratio"].(map[string]any)
	if !hr["is_custom"].(bool) || hr["effective"].(float64) != 0.8 {
		t.Fatalf("编辑后应为单独设定 0.8，得 %v", hr)
	}
	// 响应必须带回全局值，前端才能显示「跟随全局会是多少」。
	if hr["global"].(float64) != 0.5 {
		t.Fatalf("响应应带回全局 0.5，得 %v", hr["global"])
	}

	// 越界 → 400
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/admin/users/1/ratios", strings.NewReader(`{"host_ratio":1.7}`))
	r.SetPathValue("id", strconv.FormatInt(u.ID, 10))
	a.handleAdminSetUserRatios(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("越界应 400，得 %d", rec.Code)
	}

	// 清除 → 回到全局
	d = post(`{"host_ratio":-1}`)
	hr = d["host_ratio"].(map[string]any)
	if hr["is_custom"].(bool) || hr["effective"].(float64) != 0.5 {
		t.Fatalf("清除后应跟随全局 0.5，得 %v", hr)
	}
}

// TestMoneyConversionUsesWithdrawRate 点 → 钱的折算必须走提现比例这一个口径。
//
// 存在的意义：系统里还有一个「计费倍率」（1 号池积分折多少点），与「1 点折多少元」
// 是完全不同的量。混用会让余额的「≈ ¥」按错误比例显示。
func TestMoneyConversionUsesWithdrawRate(t *testing.T) {
	store, px, a := newTestApp(t)
	defer store.Close()
	defer px.Close()

	// 提现比例 0.02 元/点；同时把计费倍率设成另一个值，验证不会被误用。
	if err := store.SetKV(kvWithdrawRate, "0.02"); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	if err := store.SetKV("pricing_multiplier", "7"); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	px.ReloadSelection()

	if got := a.moneyRate(); got != 0.02 {
		t.Fatalf("moneyRate 应为提现比例 0.02，得 %v", got)
	}
	// 100 点 → 100 × 0.02 = 2 元（而非 100 × 7）。
	if got := pointsToCNY(100, a.moneyRate()); got != 2 {
		t.Fatalf("100 点应折 2 元（提现口径），得 %v", got)
	}
	// 微点 → 金额：1500 微点 = 1.5 点 → 0.03 元。
	if got := microToCNY(1500, a.moneyRate()); got != 0.03 {
		t.Fatalf("1500 微点应折 0.03 元，得 %v", got)
	}
}
