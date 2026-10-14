// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import "testing"

func newTestProxy(t *testing.T) (*Proxy, *Store) {
	t.Helper()
	s := newTestStore(t)
	return &Proxy{store: s, costPer1k: map[string]float64{}}, s
}

// 微点换算必须精确覆盖负数。
//
// 这里踩过坑：写成 `int64(p*1000 + 0.5)` 时，Go 的 float→int 向零截断会把
// -95 点算成 -94999 微点（因为 -94999.5 截断为 -94999），每笔负向调整都少扣 1 微点。
// 管理员「扣 95 点」后余额会显示 5.001 而不是 5。
func TestPointToMicroPrecision(t *testing.T) {
	cases := []struct {
		points float64
		want   int64
	}{
		{100, 100000},
		{-95, -95000},
		{-1, -1000},
		{0, 0},
		{-0.5, -500},
		{1.5, 1500},
		{-0.001, -1},
		{0.001, 1},
	}
	for _, c := range cases {
		if got := PointToMicro(c.points); got != c.want {
			t.Errorf("PointToMicro(%v) = %d, 期望 %d", c.points, got, c.want)
		}
	}
	if got := MicroToPoint(PointToMicro(-95)); got != -95 {
		t.Errorf("往返 -95 得到 %v, 期望 -95", got)
	}
}

// 无任何单价数据 → 不预扣（退化为事后结算，绝不误拒用户）。
func TestEstimateReserveNoPriceNoReserve(t *testing.T) {
	p, _ := newTestProxy(t)
	payload := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	if got := p.estimateReserveMicro(payload, "m"); got != 0 {
		t.Fatalf("无单价时应返回 0, 实得 %d", got)
	}
}

// 有兜底单价 → 预扣 > 0，且随 max_tokens 增长。
func TestEstimateReserveScalesWithMaxTokens(t *testing.T) {
	p, s := newTestProxy(t)
	if err := s.SetKV(settingReserveRate, "1"); err != nil { // 1 点 / 1k token
		t.Fatal(err)
	}
	base := func(mt int) map[string]any {
		return map[string]any{
			"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
			"max_tokens": float64(mt),
		}
	}
	small := p.estimateReserveMicro(base(1000), "m")
	large := p.estimateReserveMicro(base(10000), "m")
	if small <= 0 || large <= small {
		t.Fatalf("预扣应随 max_tokens 增长: small=%d large=%d", small, large)
	}
}

// 预扣上限生效。
func TestEstimateReserveCap(t *testing.T) {
	p, s := newTestProxy(t)
	_ = s.SetKV(settingReserveRate, "100")
	_ = s.SetKV(settingReserveMax, "5")
	payload := map[string]any{
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
		"max_tokens": float64(100000),
	}
	if got := p.estimateReserveMicro(payload, "m"); got != PointToMicro(5) {
		t.Fatalf("应被上限截断为 5 点, 实得 %v 点", MicroToPoint(got))
	}
}

// 观测单价取历史最大值，并持久化到 kv（重启后能读回）。
func TestObserveCostKeepsMaxAndPersists(t *testing.T) {
	p, s := newTestProxy(t)
	p.observeCost("m", 2)
	p.observeCost("m", 1) // 更小 → 不覆盖
	p.costMu.RLock()
	got := p.costPer1k["m"]
	p.costMu.RUnlock()
	if got != 2 {
		t.Fatalf("应保留最大值 2, 实得 %v", got)
	}
	// 模拟重启：新 Proxy 从库读回
	p2 := &Proxy{store: s, costPer1k: map[string]float64{}}
	p2.loadObservedCosts()
	p2.costMu.RLock()
	got2 := p2.costPer1k["m"]
	p2.costMu.RUnlock()
	if got2 != 2 {
		t.Fatalf("重启后应读回 2, 实得 %v", got2)
	}
}

// token 估算：多模态与缺省 max_tokens 都要能处理。
func TestEstimatePromptTokensAndMaxTokens(t *testing.T) {
	payload := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "abcdef"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "看图"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "http://x/y.png"}},
			}},
		},
	}
	if got := estimatePromptTokens(payload); got <= 0 {
		t.Fatalf("prompt 估算应 > 0, 实得 %d", got)
	}
	if requestMaxTokens(payload) != defaultMaxTokens {
		t.Fatalf("缺省 max_tokens 应为 %d", defaultMaxTokens)
	}
	payload["max_tokens"] = float64(123)
	if requestMaxTokens(payload) != 123 {
		t.Fatal("应取客户端声明的 max_tokens=123")
	}
	// max_completion_tokens 优先
	payload["max_completion_tokens"] = float64(456)
	if requestMaxTokens(payload) != 456 {
		t.Fatal("max_completion_tokens 应优先于 max_tokens")
	}
}

// observedModelCount 是设置页判断「预扣是否已自动生效」的信号。
func TestObservedModelCount(t *testing.T) {
	p, _ := newTestProxy(t)
	if p.observedModelCount() != 0 {
		t.Fatal("初始应为 0")
	}
	p.observeCost("a", 1)
	p.observeCost("b", 1)
	if p.observedModelCount() != 2 {
		t.Fatalf("应观测到 2 个模型, 实得 %d", p.observedModelCount())
	}
}

// 观测单价参与预扣：有实测数据后即使没配兜底单价也应预扣。
func TestObservedCostDrivesReserve(t *testing.T) {
	p, _ := newTestProxy(t)
	payload := map[string]any{
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
		"max_tokens": float64(1000),
	}
	if got := p.estimateReserveMicro(payload, "m"); got != 0 {
		t.Fatalf("未观测到单价时不该预扣, 实得 %d", got)
	}
	p.observeCost("m", 1) // 1 credit / 1k token
	if got := p.estimateReserveMicro(payload, "m"); got <= 0 {
		t.Fatalf("观测到单价后应预扣, 实得 %d", got)
	}
}
