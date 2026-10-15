// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package server

import (
	"testing"
	"time"
)

// TestNextMidnightCSTBoundaries 边界：CST 视角下当日 00:00 之后 → 次日 00:00，
// 23:59 → 次日 00:00，正午 → 次日 00:00。用固定 +08:00 偏移计算，不依赖系统时区。
func TestNextMidnightCSTBoundaries(t *testing.T) {
	cst := time.FixedZone("CST", 8*60*60)
	cases := []struct {
		name string
		now  time.Time
		// 期望 until 的 CST 时分秒恒为 00:00:00，且 strictly after now。
	}{
		{"23:59 -> next 00:00", time.Date(2026, 9, 11, 23, 59, 0, 0, cst)},
		{"00:00:01 -> next 00:00", time.Date(2026, 9, 11, 0, 0, 1, 0, cst)},
		{"12:00 -> next 00:00", time.Date(2026, 9, 11, 12, 0, 0, 0, cst)},
		{"00:00:00 exact -> next 00:00", time.Date(2026, 9, 11, 0, 0, 0, 0, cst)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nextMidnightCST(tc.now)
			if !got.After(tc.now) {
				t.Fatalf("until %v not after now %v", got, tc.now)
			}
			// CST 视角下必须落在 00:00:00。
			if g := got.In(cst); g.Hour() != 0 || g.Minute() != 0 || g.Second() != 0 {
				t.Errorf("until CST = %02d:%02d:%02d, want 00:00:00", g.Hour(), g.Minute(), g.Second())
			}
		})
	}
}

// TestNextMidnightCSTCrossMonth 跨月：月末 23:59 → 次月 1 日 00:00。
func TestNextMidnightCSTCrossMonth(t *testing.T) {
	cst := time.FixedZone("CST", 8*60*60)
	now := time.Date(2026, 1, 31, 23, 59, 0, 0, cst)
	got := nextMidnightCST(now)
	want := time.Date(2026, 2, 1, 0, 0, 0, 0, cst)
	if !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

// TestDegradeGateActiveTriggered Active() 在 Trigger 后为 true，过期后为 false。
func TestDegradeGateActiveTriggered(t *testing.T) {
	var g degradeGate
	if g.Active() {
		t.Error("fresh gate should not be active")
	}
	g.Trigger()
	if !g.Active() {
		t.Error("after Trigger should be active")
	}
}

// TestDegradeGateTriggerNoRenewal 降级期内再次 Trigger 不续期（保持最早触发点的 00:00 重置）。
func TestDegradeGateTriggerNoRenewal(t *testing.T) {
	var g degradeGate
	g.Trigger()
	// 用导出方法 Until() 读截止时刻（v0.31.4 起实现下沉到 internal/prompt，
	// 内部字段不再可触及 —— 这本身是好事：测试只能靠公开契约断言）。
	first := g.Until()
	// 再次 Trigger：仍在降级期内，until 不应改变。
	g.Trigger()
	second := g.Until()
	if !first.Equal(second) {
		t.Errorf("Trigger during active should not renew: first=%v second=%v", first, second)
	}
}
