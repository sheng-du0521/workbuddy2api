// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package rotateguard

import (
	"context"
	"testing"
	"time"
)

// withWindow 注入短 IP 级判定窗（生产恒 60s；测试用短窗加速过期验证）。
func withWindow(t *testing.T, d time.Duration) {
	t.Helper()
	prev := WAFWindow
	WAFWindow = d
	t.Cleanup(func() { WAFWindow = prev })
}

// withBase 注入退避基数（生产恒 500ms；测试置 0 跳过等待）。
func withBase(t *testing.T, base time.Duration) {
	t.Helper()
	prev := BackoffBase
	BackoffBase = base
	t.Cleanup(func() { BackoffBase = prev })
}

// TestWAFGateMultiAccountTriggers 状态机单测：窗内两个不同号命中 → 激活；
// 单号反复命中（任意多次）不触发；激活期内 NoteWaf 恒 true（不续期路径）。
func TestWAFGateMultiAccountTriggers(t *testing.T) {
	withWindow(t, time.Minute)
	var g WAFGate
	for n := 0; n < 10; n++ {
		if g.NoteWaf("u1") {
			t.Fatalf("single account repeated hits must never trigger, iter=%d", n)
		}
	}
	if g.Active() {
		t.Fatal("single account must not activate")
	}
	if !g.NoteWaf("u2") {
		t.Fatalf("threshold crossing call must activate and return true")
	}
	if !g.Active() {
		t.Fatal("two distinct accounts within window must activate")
	}
	if !g.NoteWaf("u3") {
		t.Fatal("hits during active window must report active")
	}
}

// TestWAFGateWindowExpiry 窗口过期自然解除 + 激活不续期。
func TestWAFGateWindowExpiry(t *testing.T) {
	withWindow(t, 150*time.Millisecond)
	var g WAFGate
	g.NoteWaf("u1")
	g.NoteWaf("u2")
	if !g.Active() {
		t.Fatal("must activate")
	}
	if !g.NoteWaf("u3") {
		t.Fatal("mid-window hit must report active")
	}
	time.Sleep(200 * time.Millisecond)
	if g.Active() {
		t.Fatal("gate must deactivate after window expiry")
	}
	if g.NoteWaf("u4") {
		t.Fatal("post-expiry single hit must not re-activate (hits cleared on activation)")
	}
	if g.Active() {
		t.Fatal("post-expiry single hit must leave gate inactive")
	}
}

// TestWAFGateRemaining 激活剩余时长语义：未激活 0；激活期 >0 且 ≤ 窗长。
func TestWAFGateRemaining(t *testing.T) {
	withWindow(t, time.Minute)
	var g WAFGate
	if d := g.Remaining(); d != 0 {
		t.Fatalf("inactive gate remaining=%v want 0", d)
	}
	g.NoteWaf("u1")
	g.NoteWaf("u2")
	if d := g.Remaining(); d <= 0 || d > time.Minute {
		t.Fatalf("active gate remaining=%v want in (0,1m]", d)
	}
}

// TestBackoffAfterBounds 轮转退避次数与抖动界：n 次失败后 = base·2^n 封顶 8s，抖动 ±25%。
func TestBackoffAfterBounds(t *testing.T) {
	withBase(t, 500*time.Millisecond)
	cases := []struct {
		n       int
		wantMin time.Duration
		wantMax time.Duration
	}{
		{0, 350 * time.Millisecond, 650 * time.Millisecond},
		{1, 750 * time.Millisecond, 1500 * time.Millisecond},
		{2, 1500 * time.Millisecond, 3000 * time.Millisecond},
		{3, 3000 * time.Millisecond, 6000 * time.Millisecond},
		{4, 6000 * time.Millisecond, 10000 * time.Millisecond},
		{20, 6000 * time.Millisecond, 10000 * time.Millisecond},
	}
	for _, c := range cases {
		for i := 0; i < 200; i++ {
			got := BackoffAfter(c.n)
			if got < c.wantMin || got > c.wantMax {
				t.Fatalf("BackoffAfter(%d)=%v want in [%v,%v]", c.n, got, c.wantMin, c.wantMax)
			}
		}
	}
}

// TestBackoffAfterZeroBase 测试态（base=0）恒零：轮转测试不等退避。
func TestBackoffAfterZeroBase(t *testing.T) {
	withBase(t, 0)
	for n := 0; n < 5; n++ {
		if got := BackoffAfter(n); got != 0 {
			t.Fatalf("BackoffAfter(%d)=%v want 0 (test mode)", n, got)
		}
	}
}

// TestJitterDurZero d<=0 原样返回（零等待不抖动）。
func TestJitterDurZero(t *testing.T) {
	if got := JitterDur(0); got != 0 {
		t.Fatalf("JitterDur(0)=%v want 0", got)
	}
	if got := JitterDur(-time.Second); got != -time.Second {
		t.Fatalf("JitterDur(-1s)=%v want -1s", got)
	}
}

// TestJitterDurBounds 抖动界：JitterDur(d) ∈ [0.75d, 1.25d]。
func TestJitterDurBounds(t *testing.T) {
	d := 400 * time.Millisecond
	for i := 0; i < 500; i++ {
		got := JitterDur(d)
		if got < 300*time.Millisecond || got > 500*time.Millisecond {
			t.Fatalf("JitterDur(%v)=%v want in [300ms,500ms]", d, got)
		}
	}
}

// TestSleepCtxCancel ctx 取消立即返回 false，等满返回 true，d<=0 立即放行。
func TestSleepCtxCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if SleepCtx(ctx, 5*time.Second) {
		t.Fatal("cancelled ctx must return false")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("cancel must be immediate, elapsed=%v", elapsed)
	}
	if !SleepCtx(context.Background(), 0) {
		t.Fatal("d<=0 must pass immediately")
	}
	if !SleepCtx(context.Background(), 20*time.Millisecond) {
		t.Fatal("full wait must return true")
	}
}

// withModelWindow 注入短「模型名不存在」判定窗（生产恒 60s）。
func withModelWindow(t *testing.T, d time.Duration) {
	t.Helper()
	prev := ModelBlockWindow
	ModelBlockWindow = d
	t.Cleanup(func() { ModelBlockWindow = prev })
}

// TestModelBlockGateMultiAccountTriggers 单号反复命中永不触发；第二个**不同号**
// 命中即激活；激活期内恒 true（不续期路径，同 WAFGate）。
func TestModelBlockGateMultiAccountTriggers(t *testing.T) {
	withModelWindow(t, time.Minute)
	var g ModelBlockGate
	for n := 0; n < 10; n++ {
		if g.NoteBlocked("ghost-model", "u1") {
			t.Fatalf("single account repeated hits must never trigger, iter=%d", n)
		}
	}
	if g.Active("ghost-model") {
		t.Fatal("single account must not activate")
	}
	if !g.NoteBlocked("ghost-model", "u2") {
		t.Fatal("threshold crossing call must activate and return true")
	}
	if !g.Active("ghost-model") {
		t.Fatal("two distinct accounts within window must activate")
	}
	if !g.NoteBlocked("ghost-model", "u3") || !g.NoteBlocked("ghost-model", "u4") {
		t.Fatal("hits during active window must report active")
	}
}

// TestModelBlockGatePerModelIsolation 分桶正确性：一个模型名被判定不存在，
// **不得**牵连其它模型。这是本闸门与 WAFGate（全局单例）最本质的区别 ——
// 一个打错的模型名不该让真模型也被快速失败。
func TestModelBlockGatePerModelIsolation(t *testing.T) {
	withModelWindow(t, time.Minute)
	var g ModelBlockGate
	g.NoteBlocked("ghost-a", "u1")
	if !g.NoteBlocked("ghost-a", "u2") {
		t.Fatal("ghost-a should activate")
	}
	// 另一个模型名必须完全不受影响：第一个号命中就该返回 false。
	if g.NoteBlocked("real-model", "u1") {
		t.Fatal("a different model must not inherit the active state")
	}
	if g.Active("real-model") {
		t.Fatal("real-model must stay inactive")
	}
	// 空 model / uid 一律不参与判定（防御性：空模型名绝不能被激活）。
	if g.NoteBlocked("", "u1") || g.NoteBlocked("m", "") {
		t.Fatal("empty model/uid must never activate")
	}
	if g.Active("") {
		t.Fatal("empty model must never be active")
	}
}

// TestModelBlockGateExpiryAndReArm 过期自然解除；解除后需**重新攒够不同号**
// 才再激活（激活时清窗：不叠旧账，否则一次偶然的双号命中会永久挡住该模型）。
func TestModelBlockGateExpiryAndReArm(t *testing.T) {
	withModelWindow(t, 120*time.Millisecond)
	var g ModelBlockGate
	g.NoteBlocked("ghost", "u1")
	if !g.NoteBlocked("ghost", "u2") {
		t.Fatal("should activate")
	}
	if !g.Active("ghost") {
		t.Fatal("active right after crossing")
	}
	time.Sleep(180 * time.Millisecond)
	if g.Active("ghost") {
		t.Fatal("must expire naturally")
	}
	if g.NoteBlocked("ghost", "u1") {
		t.Fatal("after expiry a single hit must not re-arm immediately")
	}
	if !g.NoteBlocked("ghost", "u2") {
		t.Fatal("re-arm requires a fresh distinct account")
	}
}

// TestModelBlockGateWindowPrune 窗外命中不参与判定：u1 命中后等过窗，再来 u2 时
// u1 已被剪掉 → 单个有效命中不触发（口径恒为「窗内不同号数」）。
func TestModelBlockGateWindowPrune(t *testing.T) {
	withModelWindow(t, 100*time.Millisecond)
	var g ModelBlockGate
	g.NoteBlocked("ghost", "u1")
	time.Sleep(150 * time.Millisecond)
	if g.NoteBlocked("ghost", "u2") {
		t.Fatal("stale hit outside the window must not count towards threshold")
	}
}
