// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// ======================================================================
// 通道"看得见"的回归网（v0.31.72，sub2api 接入调研的落地项）
//
// 守两件事：
//  1. **统计口径**：只算 scope='relay'、只算时间窗内；0 费占比算错会让人
//     误判"通道在漏钱"或反之（两种误判都会导致错误的停用/调价决定）。
//  2. **上游倍率探测是尽力而为**：读到要正确解析，读不到必须安静地放弃 ——
//     它绝不能把"通道可用"的结论翻成"通道坏了"。这一点比解析本身更重要：
//     非 sub2 站点天生没有这个端点，判错就是把绝大多数正常通道标成故障。
// ======================================================================

func insUsageRow(t *testing.T, s *Store, model, scope, status string, chargedMicro int64, at int64) {
	t.Helper()
	_, err := s.db.Exec(`INSERT INTO usage_logs (user_id, key_id, model, charged_micro, scope, status, created_at)
		VALUES (1, 0, ?, ?, ?, ?, ?)`, model, chargedMicro, scope, status, at)
	if err != nil {
		t.Fatalf("插入流水失败: %v", err)
	}
}

// TestRelayModelStats 统计口径：只算通道、只算窗口内、0 费占比正确。
func TestRelayModelStats(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().Unix()
	old := now - 30*24*3600 // 窗口外

	// 通道：3 次调用，其中 2 次 0 费（1 次 no_usage、1 次单价 0）。
	insUsageRow(t, s, "m-relay", "relay", "ok", 1200, now)
	insUsageRow(t, s, "m-relay", "relay", "no_usage", 0, now)
	insUsageRow(t, s, "m-relay", "relay", "ok", 0, now)
	// 干扰项：号池流水（绝不能计入）、窗口外流水。
	insUsageRow(t, s, "m-relay", "pool", "ok", 9999, now)
	insUsageRow(t, s, "m-relay", "relay", "ok", 500, old)
	// 另一个通道模型。
	insUsageRow(t, s, "m-other", "relay", "ok", 800, now)

	since := time.Now().Add(-7 * 24 * time.Hour).Unix()
	stats, err := s.relayModelStats(since)
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	got := stats["m-relay"]
	if got == nil {
		t.Fatal("m-relay 应有统计")
	}
	if got.Total != 3 {
		t.Errorf("total = %d，期望 3（号池流水与窗口外流水都不该计入）", got.Total)
	}
	if got.NoUsage != 1 {
		t.Errorf("no_usage = %d，期望 1", got.NoUsage)
	}
	if got.ZeroFee != 2 {
		t.Errorf("zero_fee = %d，期望 2（no_usage + 单价 0 都算白送）", got.ZeroFee)
	}
	if math.Abs(got.LeakRate-2.0/3.0) > 1e-9 {
		t.Errorf("leak_rate = %v，期望 %.6f", got.LeakRate, 2.0/3.0)
	}
	if o := stats["m-other"]; o == nil || o.Total != 1 || o.LeakRate != 0 {
		t.Errorf("m-other 统计不对: %+v", o)
	}

	// 空窗口：不该出现任何键（前端据此显示"—"，而不是 0 次 0 费）。
	empty, err := s.relayModelStats(now + 3600)
	if err != nil {
		t.Fatalf("空窗口统计失败: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("窗口内无数据时应返回空 map，得到 %d 项", len(empty))
	}
}

// TestProbeUpstreamBillingSub2API 两种 base_url 填法都能读到上游倍率。
func TestProbeUpstreamBillingSub2API(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sub2api/billing" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"sub2api.key_billing","schema_version":1,"billing_scope":"token",` +
			`"group_rate_multiplier":1.2,"resolved_rate_multiplier":1.2,` +
			`"peak_rate_enabled":true,"effective_rate_multiplier":1.5,"observed_at":"2026-10-21T00:00:00Z"}`))
	}))
	defer srv.Close()

	for _, base := range []string{srv.URL, srv.URL + "/v1", srv.URL + "/v1/"} {
		rate, note, ok := probeUpstreamBilling(context.Background(), base, "sk-test")
		if !ok {
			t.Fatalf("base=%q 应读到上游计费信息", base)
		}
		if math.Abs(rate-1.5) > 1e-9 {
			t.Errorf("base=%q rate = %v，期望 1.5（effective_rate_multiplier，含高峰）", base, rate)
		}
		if note == "" {
			t.Errorf("base=%q 说明文案不应为空", base)
		}
	}
	if hits == 0 {
		t.Error("没打到达标请求（说明候选路径全错）")
	}
}

// TestProbeUpstreamBillingNonSub2 非 sub2 站点必须**安静地**放弃。
//
// 这是本组用例里最重要的一条：如果这里返回 ok 或 panic，探活会把
// 每一个正常的中转站都标成"上游计费异常"。
func TestProbeUpstreamBillingNonSub2(t *testing.T) {
	cases := []struct {
		name string
		code int
		body string
	}{
		{"404", http.StatusNotFound, `{"error":"not found"}`},
		{"200 但不是 sub2", http.StatusOK, `{"object":"list","data":[]}`},
		{"200 非 JSON", http.StatusOK, `<html>hello</html>`},
		{"401", http.StatusUnauthorized, `{"error":"unauthorized"}`},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.code)
			_, _ = w.Write([]byte(c.body))
		}))
		_, _, ok := probeUpstreamBilling(context.Background(), srv.URL+"/v1", "sk-x")
		srv.Close()
		if ok {
			t.Errorf("%s：不该判定为读到上游计费信息", c.name)
		}
	}
}
