// 降级状态机的测试（v0.31.4：实现从 internal/server 下沉到本包，两个产品共用）。
package prompt

import (
	"testing"
	"time"
)

// TestNextMidnightCSTBoundaries 边界：23:59 → 次日 00:00；00:00 → 次日 00:00。
func TestNextMidnightCSTBoundaries(t *testing.T) {
	cst := time.FixedZone("CST", 8*60*60)
	cases := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{"23:59 当天", time.Date(2026, 10, 16, 23, 59, 0, 0, cst), time.Date(2026, 10, 17, 0, 0, 0, 0, cst)},
		{"00:00 整点", time.Date(2026, 10, 16, 0, 0, 0, 0, cst), time.Date(2026, 10, 17, 0, 0, 0, 0, cst)},
		{"12:00 正午", time.Date(2026, 10, 16, 12, 0, 0, 0, cst), time.Date(2026, 10, 17, 0, 0, 0, 0, cst)},
		// UTC 输入：UTC 16:30 = CST 次日 00:30 → 已过当天零点，取**再下一天**的零点。
		{"UTC 输入（跨日界）", time.Date(2026, 10, 16, 16, 30, 0, 0, time.UTC), time.Date(2026, 10, 18, 0, 0, 0, 0, cst)},
		// UTC 输入：UTC 08:00 = CST 16:00 → 当天零点已过 → 次日零点。
		{"UTC 输入（同日内）", time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC), time.Date(2026, 10, 17, 0, 0, 0, 0, cst)},
	}
	for _, c := range cases {
		got := NextMidnightCST(c.now)
		if !got.Equal(c.want) {
			t.Errorf("%s: NextMidnightCST=%v want %v", c.name, got, c.want)
		}
		if !got.After(c.now) {
			t.Errorf("%s: 结果必须晚于 now（got %v now %v）", c.name, got, c.now)
		}
	}
}

// TestGateTriggerAndActive 降级触发后 Active 为真、Until 落在未来。
func TestGateTriggerAndActive(t *testing.T) {
	var g Gate
	if g.Active() {
		t.Fatal("初始状态不该处于降级期")
	}
	if !g.Until().IsZero() {
		t.Errorf("未触发时 Until 应为零值，got %v", g.Until())
	}
	g.Trigger()
	if !g.Active() {
		t.Fatal("触发后应处于降级期")
	}
	if u := g.Until(); u.IsZero() || !u.After(time.Now()) {
		t.Errorf("Until 应落在未来，got %v", u)
	}
	if n := g.Triggers(); n != 1 {
		t.Errorf("Triggers=%d want 1", n)
	}
}

// TestGateTriggerDoesNotExtend 降级期内重复触发**不续期**（保持最早触发点的 00:00 语义）。
//
// 若续期，一个持续报错的客户端会把降级状态无限延长 —— 而 00:00 重置的意义正是
// 「每天让真实的上游行为重新决定一次是否需要降级」。
func TestGateTriggerDoesNotExtend(t *testing.T) {
	var g Gate
	g.Trigger()
	first := g.Until()
	g.Trigger()
	g.Trigger()
	if got := g.Until(); !got.Equal(first) {
		t.Errorf("重复触发不得续期：%v → %v", first, got)
	}
	if n := g.Triggers(); n != 3 {
		t.Errorf("Triggers 应如实累计（面板要看触发频率），got %d", n)
	}
}

// TestGateExpiry IsActiveAt 到期即失效（纯函数判定，便于用固定时刻断言）。
func TestGateExpiry(t *testing.T) {
	g := &Gate{until: time.Now().Add(time.Minute)}
	if !g.ActiveAt(time.Now()) {
		t.Fatal("未到期应为 Active")
	}
	if g.ActiveAt(time.Now().Add(2 * time.Minute)) {
		t.Error("已过期不应为 Active")
	}
}
