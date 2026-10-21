// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ======================================================================
// Key 滚动消费窗口（v0.31.100）
//
// 余额只回答"一共还能花多少"，回答不了"这十分钟花得太快"。
// 三条纪律：
//  ① 三个窗口独立，命中任一即 429（**零出站、零扣费**，且带 Retry-After）；
//  ② 统计只看**已实扣**（`charged_micro`），且按滚动窗口算 —— 窗口外的历史不算；
//  ③ 0 = 不限；没配限额的 Key 不做任何查询（热路径零开销）。
// ======================================================================

// relayTestKey 取夹具里那把固定的测试 Key。
func relayTestKey(t *testing.T, store *Store) *APIKey {
	t.Helper()
	k, err := store.GetAPIKeyByHash(HashAPIKey("sk-wb-relay-test-key"))
	if err != nil || k == nil {
		t.Fatalf("取测试 Key：%v", err)
	}
	return k
}

// seedKeyUsage 写一条已实扣的流水（名字带 Key：别与 report_http_test.go 的 seedUsage 撞名）（charged 单位：积分）。
func seedKeyUsage(t *testing.T, store *Store, k *APIKey, model string, charged float64, at int64) {
	t.Helper()
	if _, err := store.db.Exec(
		`INSERT INTO usage_logs (user_id, key_id, model, charged_micro, status, scope, created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		k.UserID, k.ID, model, int64(charged*MicroPerPoint), "ok", "relay", at); err != nil {
		t.Fatalf("写流水：%v", err)
	}
}

// TestKeyWindowBlocksOverLimit 5h 窗口超限 → 429，且**一次上游都没打**。
func TestKeyWindowBlocksOverLimit(t *testing.T) {
	store, _, _, srv := newRelayChatApp(t, false)
	defer store.Close()
	defer srv.Close()

	up, hits := fakeRelayPerModel(t, map[string]int{})
	defer up.Close()
	seedRelay(t, store, up.URL, "win-model", 1.0, true)
	seedPoolModels([]string{"pool-known-model"})

	k := relayTestKey(t, store)
	if err := store.SetKeyRateLimits(k.ID, 1, 0, 0); err != nil {
		t.Fatalf("SetKeyRateLimits: %v", err)
	}
	seedKeyUsage(t, store, k, "win-model", 2, time.Now().Unix()) // 已用 2 > 上限 1

	resp, body := chatOnce(t, srv, `{"model":"win-model","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("窗口超限应回 429（不是 402 —— 这不是没钱，是花太快），得 %d：%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Wb-Fail-Kind"); got != "key_window_exceeded" {
		t.Fatalf("X-Wb-Fail-Kind = %q，期望 key_window_exceeded", got)
	}
	if ra := resp.Header.Get("Retry-After"); ra == "" || ra == "0" {
		t.Fatalf("应回 Retry-After（滚动窗口下「还要等多久」是能算出来的），得到 %q", ra)
	}
	if !strings.Contains(body, "5 小时") {
		t.Errorf("错误里应点名命中的窗口：%s", body)
	}
	if n := atomic.LoadInt32(hits); n != 0 {
		t.Fatalf("被窗口拒绝的请求不该打上游，实际 %d 次", n)
	}
}

// TestKeyWindowIsRolling 窗口外的历史流水不参与统计（滚动，不是"自然日"）。
func TestKeyWindowIsRolling(t *testing.T) {
	store, _, _, srv := newRelayChatApp(t, false)
	defer store.Close()
	defer srv.Close()

	up, hits := fakeRelayPerModel(t, map[string]int{})
	defer up.Close()
	seedRelay(t, store, up.URL, "win-model", 1.0, true)
	seedPoolModels([]string{"pool-known-model"})

	k := relayTestKey(t, store)
	// 只有 5h 限额；6 小时前花掉 5 积分 —— 已经滑出 5h 窗口，不该拦。
	if err := store.SetKeyRateLimits(k.ID, 1, 0, 0); err != nil {
		t.Fatalf("SetKeyRateLimits: %v", err)
	}
	seedKeyUsage(t, store, k, "win-model", 5, time.Now().Add(-6*time.Hour).Unix())

	resp, body := chatOnce(t, srv, `{"model":"win-model","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("6 小时前的消费不该计入 5h 窗口，得 %d：%s", resp.StatusCode, body)
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Fatalf("应正常打一次上游，实际 %d 次", n)
	}

	// 同一笔流水在 1 天窗口里就必须算上 —— 证明"窗口各自独立"。
	if err := store.SetKeyRateLimits(k.ID, 0, 1, 0); err != nil {
		t.Fatalf("SetKeyRateLimits: %v", err)
	}
	resp2, body2 := chatOnce(t, srv, `{"model":"win-model","messages":[{"role":"user","content":"hi"}]}`)
	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("6 小时前的消费应计入 1d 窗口，得 %d：%s", resp2.StatusCode, body2)
	}
}

// TestAdminSetKeyLimitsHandler 后台设置限额：正常值落库、非法值显式拒绝。
func TestAdminSetKeyLimitsHandler(t *testing.T) {
	store, _, a, srv := newRelayChatApp(t, false)
	defer store.Close()
	defer srv.Close()

	k := relayTestKey(t, store)

	do := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/admin/keys/x/limits", strings.NewReader(body))
		req.SetPathValue("id", strconv.FormatInt(k.ID, 10))
		rec := httptest.NewRecorder()
		a.handleAdminSetKeyLimits(rec, req)
		return rec
	}

	if rec := do(`{"h5":2.5,"d1":20,"d7":100}`); rec.Code != 200 {
		t.Fatalf("正常设置应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	got, err := store.GetAPIKeyByHash(HashAPIKey("sk-wb-relay-test-key"))
	if err != nil {
		t.Fatalf("重读 Key：%v", err)
	}
	if got.RL5h != 2.5 || got.RL1d != 20 || got.RL7d != 100 {
		t.Fatalf("落库值 = %v/%v/%v，期望 2.5/20/100", got.RL5h, got.RL1d, got.RL7d)
	}

	// 负数与非法值必须显式拒绝：静默写成 0 等于"悄悄解除限额"，比报错危险。
	for _, bad := range []string{`{"h5":-1}`, `{"d1":"abc"}`, `{"d7":1e9}`} {
		if rec := do(bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%s 应 400，得 %d：%s", bad, rec.Code, rec.Body.String())
		}
	}
}
