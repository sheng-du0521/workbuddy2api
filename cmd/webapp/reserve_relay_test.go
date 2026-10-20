// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"net/http"
	"sync/atomic"
	"testing"
)

// ======================================================================
// 通道单价的预扣（v0.31.71，P2 收尾）
//
// 修的是"钱挡不住"：通道模型此前只能按**全局兜底单价**估预扣，
// 而兜底价是管理员随手填的一个数，通道单价高时严重低估 ——
// 余额 10 点的用户也能发起一次实际消耗 500 点的请求，结算后变负数。
// ======================================================================

// payloadForReserve 构造一个只带 max_tokens 的请求体（prompt 为空，便于手算）。
//
// ⚠️ max_tokens 必须是 **float64**：payload 来自 json.Unmarshal 到 map[string]any，
// 数字一律是 float64。传 int 时 RequestMaxTokens 的类型断言会失败 → 静默回落到
// DefaultMaxTokens(4096)，断言就会变成"期望 5000、实际 20480"（本轮实测踩到）。
func payloadForReserve(maxTokens int) map[string]any {
	return map[string]any{"messages": []any{}, "max_tokens": float64(maxTokens)}
}

// R1：通道单价 × 通道倍率 = 预扣单价。
//
// 5 credit/1k × 1 倍 = 5 点/1k；1000 token → PointToMicro(5) = 5000 微点。
func TestRelayReserveUsesChannelPrice(t *testing.T) {
	store, px := newHostProxy(t)
	defer store.Close()
	if err := store.SetKV(settingMultiplier, "1"); err != nil {
		t.Fatal(err)
	}
	// 没有兜底单价：此前这个场景的预扣直接是 0（完全不设防）。
	seedRelay(t, store, "http://relay.example.com", "relay-reserve-model", 5, true)
	px.ReloadRelayPrices()

	got := px.estimateReserveMicro(payloadForReserve(1000), "relay-reserve-model")
	if got != 5000 {
		t.Fatalf("应预扣 5000 微点（5 点/1k × 1k token），实际 %d", got)
	}

	// 通道倍率参与：设成 2 → 单价翻倍（10000）。
	// setModelBillingRatio 要求模型已在倍率目录里（它是"改已有行"，不是"建行"）。
	seedRatioModel(t, store, "relay-reserve-model", 0)
	if _, _, err := px.setModelBillingRatio("relay-reserve-model", 2, "relay"); err != nil {
		t.Fatalf("setModelBillingRatio: %v", err)
	}
	if got := px.estimateReserveMicro(payloadForReserve(1000), "relay-reserve-model"); got != 10000 {
		t.Fatalf("倍率 2 时应预扣 10000，实际 %d", got)
	}
}

// R2：停用通道 → 立刻不再按通道价预扣（回落兜底/0）。
//
// 这条守的是"改了不生效"：停用后如果快照没刷新，仍会按旧价冻结用户的钱。
func TestRelayReserveFollowsChannelState(t *testing.T) {
	store, px := newHostProxy(t)
	defer store.Close()
	if err := store.SetKV(settingMultiplier, "1"); err != nil {
		t.Fatal(err)
	}
	uid, _ := seedRelay(t, store, "http://relay.example.com", "relay-off-model", 3, true)
	px.ReloadRelayPrices()
	if got := px.estimateReserveMicro(payloadForReserve(1000), "relay-off-model"); got != 3000 {
		t.Fatalf("启用时应按 3 点/1k 预扣（3000），实际 %d", got)
	}

	if _, err := store.db.Exec(`UPDATE upstreams SET enabled = 0 WHERE id = ?`, uid); err != nil {
		t.Fatal(err)
	}
	px.ReloadRelayPrices()
	if got := px.estimateReserveMicro(payloadForReserve(1000), "relay-off-model"); got != 0 {
		t.Fatalf("停用后不应再按通道价预扣（且没配兜底 → 0），实际 %d", got)
	}
}

// R3：号池实测单价优先 —— 同名的号池模型不能被通道价覆盖。
//
// 分发规则是"号池优先"，所以号池的实测价才是这笔请求的真实成本；
// 用通道价会让号池请求的预扣凭空偏离。
func TestPoolCostBeatsRelayPrice(t *testing.T) {
	store, px := newHostProxy(t)
	defer store.Close()
	if err := store.SetKV(settingMultiplier, "1"); err != nil {
		t.Fatal(err)
	}
	// 同一个模型名：号池实测 1 积分/1k，通道配 9 credit/1k。
	px.observeCost("shared-reserve-model", 1)
	seedRelay(t, store, "http://relay.example.com", "shared-reserve-model", 9, true)
	px.ReloadRelayPrices()

	if got := px.estimateReserveMicro(payloadForReserve(1000), "shared-reserve-model"); got != 1000 {
		t.Fatalf("号池实测价应优先（1 点/1k → 1000），实际 %d", got)
	}
}

// R4：多条通道同名模型取**最高**单价（宁可高估也不低估）。
func TestRelayPriceCeilingTakesMax(t *testing.T) {
	store, px := newHostProxy(t)
	defer store.Close()
	if err := store.SetKV(settingMultiplier, "1"); err != nil {
		t.Fatal(err)
	}
	// 两条通道名不能相同（upstreams.name 有唯一索引），用不同名字。
	seedRelayNamed(t, store, "relay-a", "http://a.example.com", "multi-model", 2, true)
	seedRelayNamed(t, store, "relay-b", "http://b.example.com", "multi-model", 7, true)
	px.ReloadRelayPrices()

	// 7 点/1k → 1000 token = 7000 微点。
	if got := px.estimateReserveMicro(payloadForReserve(1000), "multi-model"); got != 7000 {
		t.Fatalf("多条通道应取最高单价（7 → 7000），实际 %d", got)
	}
}

// R5：端到端 —— 通道单价高时，余额不足的请求必须被**预扣**挡住（402），
// 而不是先发出去再结算成负数。
//
// 这条是整批改动的目的：R1-R4 证明"算得对"，这条证明"真的拦住了"。
func TestRelayReserveBlocksInsufficientBalance(t *testing.T) {
	store, px, _, srv := newRelayChatApp(t, false)
	defer store.Close()
	defer srv.Close()

	if err := store.SetKV(settingMultiplier, "1"); err != nil {
		t.Fatal(err)
	}
	up, hits := fakeRelay(t, 10, 10, 200, false)
	defer up.Close()
	// 5000 credit/1k × 1000 token = 5000 点预扣；测试用户余额是 1000 点 → 必然不足。
	seedRelay(t, store, up.URL, "expensive-model", 5000, true)
	px.ReloadRelayPrices()
	seedPoolModels([]string{"pool-known-model"})

	before := balanceOf(t, store)
	body := `{"model":"expensive-model","max_tokens":1000,"messages":[{"role":"user","content":"hi"}]}`
	resp, respBody := chatOnce(t, srv, body)
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("余额不足应 402（预扣被拒），得 %d：%s", resp.StatusCode, respBody)
	}
	// 关键：**一次都不能打到上游**（预扣的意义就是"在花钱之前拦住"）。
	if n := atomic.LoadInt32(hits); n != 0 {
		t.Fatalf("预扣被拒时不应出站，实际调用通道 %d 次", n)
	}
	// 余额不能动（没有预扣成功，也没有结算）。
	if after := balanceOf(t, store); after != before {
		t.Fatalf("被拒的请求不该改变余额：%d → %d", before, after)
	}
}
