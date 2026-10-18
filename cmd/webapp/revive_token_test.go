// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/upstream"
)

// 本文件钉住「探活复活」（v0.31.39）的三条口径 —— 每一条都对应一个很容易写反的分支：
//
//  1. **dry_run 必须零出站**：它是"先看看要动几个号"的动作。如果它也去探活，
//     这个按钮本身就成了批量出站（教训：渲染即出站 = 一次风控事件）。
//  2. **探活通过才清标记**，且复活后必须是「仅剩 access」而不是"一切正常"
//     （我们唯一掌握的证据是 refresh 曾被 12153 判过）。
//  3. **探活失败什么都不改**：它已经被排除在服务之外，再累计一次阈值只会把
//     "其实只是抖动"的号推向永久停用。
//
// 注意：本文件与 revive_test.go 不是一回事 —— 那个锁的是「启用要清系统禁用位」。

// reviveProbeServer 假上游：ok=true 返回一条正常 chat 响应；否则回 12153。
func reviveProbeServer(t *testing.T, calls *int32, ok bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":12153,"msg":"Offline user session not found"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","choices":[{"index":0,` +
			`"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
}

// reviveProbeFixture 造一个「已被判失效、但 access 还没过期」的号（生产里最常见的形态：
// refresh 被轮换掉，access 还剩几十天）。
func reviveProbeFixture(t *testing.T, calls *int32, probeOK bool) (*Store, *app, *httptest.Server) {
	t.Helper()
	store, px, a := newTestApp(t, &Account{
		UID: "u-1", Nickname: "甲", Realm: "cn",
		AccessToken: "at", RefreshToken: "rt",
		ExpiresAt: time.Now().Add(20 * 24 * time.Hour).Unix(),
	})
	srv := reviveProbeServer(t, calls, probeOK)
	px.uc = &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	if err := px.SyncAccounts(); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}
	if err := store.MarkTokenDead("u-1"); err != nil {
		t.Fatalf("MarkTokenDead: %v", err)
	}
	return store, a, srv
}

// TestReviveDryRunMakesNoRequest dry_run 只列候选，**一个上游请求都不发**。
func TestReviveDryRunMakesNoRequest(t *testing.T) {
	var calls int32
	store, a, srv := reviveProbeFixture(t, &calls, true)
	defer store.Close()
	defer srv.Close()

	res := a.reviveOne("u-1", true)
	if res.State != "candidate" {
		t.Fatalf("dry_run 应报 candidate，得 %q（note=%s）", res.State, res.Note)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("dry_run 不该出站，实际发了 %d 次请求", got)
	}
	if _, ok := store.TokenDeadMap()["u-1"]; !ok {
		t.Fatal("dry_run 不该清失效标记")
	}
}

// TestReviveProbePassClearsDeadButKeepsRefreshLost 探活通过 → 清 token_dead、置 refresh_lost。
func TestReviveProbePassClearsDeadButKeepsRefreshLost(t *testing.T) {
	var calls int32
	store, a, srv := reviveProbeFixture(t, &calls, true)
	defer store.Close()
	defer srv.Close()

	res := a.reviveOne("u-1", false)
	if res.State != "revived" {
		t.Fatalf("探活通过应复活，得 %q（note=%s）", res.State, res.Note)
	}
	if !res.ProbeOK {
		t.Fatal("ProbeOK 应为 true")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("应恰好探活 1 次，得 %d", got)
	}
	if _, ok := store.TokenDeadMap()["u-1"]; ok {
		t.Fatal("复活后不该再有 token_dead 标记")
	}
	if _, ok := store.RefreshLostMap()["u-1"]; !ok {
		t.Fatal("复活后必须置 refresh_lost —— 它的 refresh 确实被 12153 判过，不能报成一切正常")
	}
	// 池内阻塞位必须被清：否则会出现"界面已复活、选号仍跳过"。
	if st, ok := a.proxy.pool.Status("u-1"); ok {
		if st.Cooling || st.ManualDisabled || st.Disabled {
			t.Fatalf("复活后池内仍被阻塞：%+v", st)
		}
		if st.ConsecutiveFails != 0 || st.FaultFails != 0 {
			t.Fatalf("复活后失败计数应清零：consecutive=%d fault=%d", st.ConsecutiveFails, st.FaultFails)
		}
	}
}

// TestReviveProbeFailChangesNothing 探活不通 → 什么都不改（不冷却、不累计、不清标记）。
func TestReviveProbeFailChangesNothing(t *testing.T) {
	var calls int32
	store, a, srv := reviveProbeFixture(t, &calls, false)
	defer store.Close()
	defer srv.Close()

	before, _ := store.GetAccount("u-1")
	res := a.reviveOne("u-1", false)
	if res.State != "probe-failed" {
		t.Fatalf("探活失败应报 probe-failed，得 %q", res.State)
	}
	if _, ok := store.TokenDeadMap()["u-1"]; !ok {
		t.Fatal("探活失败不能清失效标记（否则等于无条件复活）")
	}
	if _, ok := store.RefreshLostMap()["u-1"]; ok {
		t.Fatal("探活失败不该置 refresh_lost")
	}
	after, _ := store.GetAccount("u-1")
	if before != nil && after != nil && before.FailCount != after.FailCount {
		t.Fatalf("探活失败不该累计失败次数：%d → %d", before.FailCount, after.FailCount)
	}
	if st, ok := a.proxy.pool.Status("u-1"); ok && st.Cooling {
		t.Fatal("探活失败不该额外冷却 —— 它已经被排除在服务之外了")
	}
}

// TestReviveUnmarkedReportsNoop 没标记的号不该被"复活"（否则按钮会变成
// "对所有号做一次探活"的批量出站入口）。
func TestReviveUnmarkedReportsNoop(t *testing.T) {
	var calls int32
	store, a, srv := reviveProbeFixture(t, &calls, true)
	defer store.Close()
	defer srv.Close()

	if err := store.ClearTokenDead("u-1"); err != nil {
		t.Fatalf("ClearTokenDead: %v", err)
	}
	res := a.reviveOne("u-1", false)
	if res.State != "alive-no-mark" {
		t.Fatalf("无标记应报 alive-no-mark，得 %q", res.State)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("无标记不该出站，实际 %d 次", got)
	}
}

// TestReviveCandidatesOnlyDead 候选只含 token_dead，不含 refresh_lost。
//
// 为什么卡这么死：refresh_lost 的号**还在服务**，把它们纳入批量复活等于对一批
// 正在干活的号各打一次对话 —— 既白烧额度，又是批量出站特征。
func TestReviveCandidatesOnlyDead(t *testing.T) {
	store, _, a := newTestApp(t,
		&Account{UID: "dead", Nickname: "死", Realm: "cn", AccessToken: "at"},
		&Account{UID: "lost", Nickname: "半死", Realm: "cn", AccessToken: "at"},
		&Account{UID: "ok", Nickname: "好", Realm: "cn", AccessToken: "at"},
	)
	defer store.Close()

	if err := store.MarkTokenDead("dead"); err != nil {
		t.Fatalf("MarkTokenDead: %v", err)
	}
	if err := store.MarkRefreshLost("lost"); err != nil {
		t.Fatalf("MarkRefreshLost: %v", err)
	}
	got := a.reviveCandidates()
	if len(got) != 1 || got[0] != "dead" {
		t.Fatalf("候选应只有 dead，得 %v", got)
	}
}
