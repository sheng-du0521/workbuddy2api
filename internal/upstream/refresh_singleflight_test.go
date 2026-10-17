// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package upstream

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// 本文件钉住一条纪律（v0.31.28，2026-10-18 全池 38 个号掉登录换来的）：
// **同一个账号的 refresh 请求，任何时刻只能有一个在途。**
//
// 为什么这不是"优化"而是硬要求：上游的 refresh 是轮换制且会撤销旧凭证。
// 两个并发请求带同一个 refresh_token 时，第一个成功并作废旧 token，第二个收到
// `12153: refresh token is invalid`；部分实现还会把"旧 refresh 被重复使用"判为
// 重放，连带作废刚签发的新 token —— 账号就此彻底掉登录（access 也 invalid_token）。
//
// 本仓库有四处独立刷新入口（签到前补刷新 / 保活 / 401 重试 / chat 前 refreshIfNeeded），
// 此前没有任何互斥；旧注释还以「实测并发双刷新两个新 token 都有效」为它背书 ——
// 那次实测只验证了"两条都能用"，没验证"旧 token 会不会被撤销"，结论已被推翻。

// refreshTestServer 一个最小的 refresh 端点：返回新 token，并统计被调用次数。
func refreshTestServer(t *testing.T, calls *int32, fail bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		// 模拟上游 I/O 延迟，让并发请求真的重叠（否则单飞根本没被考验）。
		time.Sleep(3 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		if fail {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":12153,"msg":"refresh token is invalid"}`))
			return
		}
		// doJSON 解析的是 {code,data} 信封，data 段才是 token。
		_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"at-new","refreshToken":"rt-new","expiresIn":3600}}`))
	}))
}

// TestRefreshTokenSingleFlight 10 个并发刷新 → 上游只应收到 1 次请求，且全部成功。
//
// 断言三件事：
//  1. 请求次数 == 1（多一次就足以让账号掉登录）；
//  2. 所有调用都返回 nil（等待者复用同一次结果，不该被判失败）；
//  3. 等待者读到的 token 已经是刷新后的新值（不能被"空结果"覆盖回旧值）。
func TestRefreshTokenSingleFlight(t *testing.T) {
	var calls int32
	srv := refreshTestServer(t, &calls, false)
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	a := &auth.Auth{UID: "u-sf", AccessToken: "at-old", RefreshToken: "rt-old"}

	const n = 10
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = c.RefreshToken(a)
		}(i)
	}
	close(start)
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("并发刷新应只发 1 次请求，实际 %d 次 —— 第二次会带同一个旧 refresh_token，必然 12153 并可能作废新 token", got)
	}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 个等待者不该失败：%v", i, err)
		}
	}
	if got := a.AccessTokenValue(); got != "at-new" {
		t.Fatalf("刷新后的 access token 应为 at-new，得 %q", got)
	}
	if got := a.RefreshTokenValue(); got != "rt-new" {
		t.Fatalf("刷新后的 refresh token 应为 rt-new，得 %q", got)
	}
}

// TestRefreshTokenNotCachedAcrossCalls 单飞只合并"并发"，不能把结果缓存下来：
// 顺序调用两次必须真的发两次请求（否则 token 到期后永远刷不动）。
func TestRefreshTokenNotCachedAcrossCalls(t *testing.T) {
	var calls int32
	srv := refreshTestServer(t, &calls, false)
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	a := &auth.Auth{UID: "u-seq", AccessToken: "at-old", RefreshToken: "rt-old"}

	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("第一次刷新：%v", err)
	}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("第二次刷新：%v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("顺序调用应发 2 次请求，得 %d —— 单飞缓存了结果会让过期 token 永远刷不动", got)
	}
}

// TestRefreshTokenSingleFlightSharesError 刷新失败时，等待者拿到**同一个**错误，
// 并且下一次调用能重新尝试（失败不该把账号永久锁死）。
func TestRefreshTokenSingleFlightSharesError(t *testing.T) {
	var calls int32
	srv := refreshTestServer(t, &calls, true)
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	a := &auth.Auth{UID: "u-fail", AccessToken: "at-old", RefreshToken: "rt-old"}

	const n = 6
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = c.RefreshToken(a)
		}(i)
	}
	close(start)
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("失败也应只发 1 次，得 %d 次", got)
	}
	for i, err := range errs {
		if err == nil {
			t.Fatalf("第 %d 个等待者应拿到失败结果", i)
		}
	}
	// 失败不能把单飞登记表卡死：再调一次应重新发请求。
	_ = c.RefreshToken(a)
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("失败后应能重新尝试，累计请求数应为 2，得 %d", got)
	}
}
