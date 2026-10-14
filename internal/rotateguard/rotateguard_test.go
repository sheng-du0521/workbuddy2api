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
