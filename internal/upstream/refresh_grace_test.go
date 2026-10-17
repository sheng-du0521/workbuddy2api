// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package upstream

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// 本文件钉住 v0.31.37 对刷新闸门的两处修正：
//
//  1. **按 refresh token 单飞**（而不是按 *auth.Auth 指针）。
//     旧实现的前提是"同一账号只有一份 Auth 对象"，而生产里健康检测/任务会各自
//     构造快照 —— 两份对象 = 两把互不可见的闸门 = 同一张 refresh token 被双花。
//
//  2. **60s 宽限**：单飞只挡并发。"前后脚"的串行调用（第二次还拿着已作废的旧 RT）
//     会真的再打一次上游，拿 12153，并把一个只是"续期票被作废"的号甩给死号处置。
//
// 两条都指向同一个上游事实：refresh 是**轮换制且撤销旧凭证**。

// resetRefreshCaches 清空进程级刷新闸门状态（仅供测试）。
func resetRefreshCaches() {
	refreshFlights.Range(func(k, _ any) bool { refreshFlights.Delete(k); return true })
	refreshGraceCache.Range(func(k, _ any) bool { refreshGraceCache.Delete(k); return true })
}

// TestRefreshSingleFlightKeysByRefreshToken 两份 Auth、同一个 RT → 只发 1 次请求，
// 且**两份都要拿到新凭据**。
//
// 后半句是关键：旧实现只把 error 传给等待者，等待者手里那份对象仍是旧 token，
// 它下一次出站就是 12153 —— 而错误日志会指向"上游拒绝"，完全看不出是本地的锅。
func TestRefreshSingleFlightKeysByRefreshToken(t *testing.T) {
	var calls int32
	srv := refreshTestServer(t, &calls, false)
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	// 两份"同一个号"的凭据对象（模拟池内 Auth 与任务侧快照）。
	a1 := &auth.Auth{UID: "u-dup", AccessToken: "at-old", RefreshToken: "rt-dup"}
	a2 := &auth.Auth{UID: "u-dup", AccessToken: "at-old", RefreshToken: "rt-dup"}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	start := make(chan struct{})
	for i, a := range []*auth.Auth{a1, a2} {
		wg.Add(1)
		go func(i int, a *auth.Auth) {
			defer wg.Done()
			<-start
			errs[i] = c.RefreshToken(a)
		}(i, a)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 份凭据不该失败：%v", i, err)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("同一张 RT 只能打一次上游，实际 %d 次 —— 第二次必然 12153 并可能作废新 token", got)
	}
	for i, a := range []*auth.Auth{a1, a2} {
		if a.AccessTokenValue() != "at-new" || a.RefreshTokenValue() != "rt-new" {
			t.Fatalf("第 %d 份凭据必须也被写成新值（否则它会拿作废的旧 token 继续跑）：at=%q rt=%q",
				i, a.AccessTokenValue(), a.RefreshTokenValue())
		}
	}
}

// TestRefreshGraceReusesResultWithin60s 「前后脚」的第二次刷新在 60s 内复用上次结果，
// 不再消耗那张已经作废的旧 RT。
func TestRefreshGraceReusesResultWithin60s(t *testing.T) {
	var calls int32
	srv := refreshTestServer(t, &calls, false)
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	first := &auth.Auth{UID: "u-grace", AccessToken: "at-old", RefreshToken: "rt-grace"}
	if err := c.RefreshToken(first); err != nil {
		t.Fatalf("第一次刷新：%v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("第一次应发 1 次，得 %d", got)
	}

	// 第二个调用方是"还没来得及同步"的旧快照：它手里仍是作废的 rt-grace。
	stale := &auth.Auth{UID: "u-grace", AccessToken: "at-old", RefreshToken: "rt-grace"}
	if err := c.RefreshToken(stale); err != nil {
		t.Fatalf("宽限期内不该失败（旧 RT 已被撤销，再打上游就是 12153）：%v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("60s 宽限内不该再打上游，累计 %d 次", got)
	}
	if stale.AccessTokenValue() != "at-new" || stale.RefreshTokenValue() != "rt-new" {
		t.Fatalf("宽限回放必须把结果写回旧快照：at=%q rt=%q", stale.AccessTokenValue(), stale.RefreshTokenValue())
	}
}

// TestRefreshKeyDiffersPerToken 单飞键必须按 token 值区分：
// 不同账号的 RT 不能共用一个键（否则会互相串结果，比双花更糟）。
func TestRefreshKeyDiffersPerToken(t *testing.T) {
	if refreshKey("c1", "u1", "rt-a") == refreshKey("c1", "u1", "rt-b") {
		t.Fatalf("不同 RT 必须得到不同键")
	}
	if refreshKey("c1", "u1", "rt-a") != refreshKey("c1", "u1", "rt-a") {
		t.Fatalf("同一个 RT 必须得到同一个键（否则单飞形同虚设）")
	}
	// 键不能是 RT 原文（会进内存/日志排查路径）。
	if refreshKey("c1", "u1", "secret-rt") == "secret-rt" {
		t.Fatalf("键必须是派生值，不能是原文")
	}
}

// TestRefreshGraceExpires 宽限必须过期：超过 60s 后同一张旧 RT 要重新走真实请求
// （否则"永远回放"会让 token 到期后刷不动）。
//
// 用直接改缓存时间的方式模拟过期，避免 sleep 60s 的测试。
func TestRefreshGraceExpires(t *testing.T) {
	var calls int32
	srv := refreshTestServer(t, &calls, false)
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	a := &auth.Auth{UID: "u-exp", AccessToken: "at-old", RefreshToken: "rt-exp"}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("第一次刷新：%v", err)
	}
	// 把缓存里的时间拨回 61s 前。
	key := refreshKey(fmt.Sprintf("%p", c), "u-exp", "rt-exp")
	if v, ok := refreshGraceCache.Load(key); ok {
		if f, _ := v.(*refreshFlight); f != nil {
			f.at = time.Now().Add(-61 * time.Second)
		}
	}
	stale := &auth.Auth{UID: "u-exp", AccessToken: "at-old", RefreshToken: "rt-exp"}
	// 打上游会返回 200（测试服务器不看 RT），所以这里只断言"确实又发了一次"。
	_ = c.RefreshToken(stale)
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("宽限过期后应重新请求上游，累计 %d 次", got)
	}
}
