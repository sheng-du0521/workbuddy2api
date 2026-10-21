// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// ======================================================================
// 通道探活的「遗漏」（v0.31.97）
//
// 用户反馈两条，都指向同一类缺口：
//  1. 「gpt-6-sol 实际为 gpt-6.1-sol」—— 上游 /models 只说"我有这些名字"，
//     不等于"这些名字能调通"。实测上游列 gpt-6-sol 可 200，而人工填的
//     gpt-6.1-sol 直接 404（Model ... is not supported by any configured account）。
//     只探 /models 的探活对这类错误**完全无感**，要等用户发请求才炸。
//  2. 单价/倍率：上游只给一个全局 `effective_rate_multiplier`（例如 0.224），
//     没有 per-model 单价 —— 所以只能给"官方倍率 × 上游倍率"的**建议值**，
//     并把它标注成估算。
// ======================================================================

// TestProbeUpstreamDeepDetectsBrokenModel 深度探活必须把"调不通"的模型挑出来。
func TestProbeUpstreamDeepDetectsBrokenModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		m, _ := body["model"].(string)
		if m == "gpt-6.1-sol" {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":{"message":"Model \"gpt-6.1-sol\" is not supported by any configured account"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	u := Upstream{BaseURL: srv.URL, APIKey: "sk-x"}
	models := []UpstreamModel{{Model: "gpt-6-sol"}, {Model: "gpt-6.1-sol"}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	checks := probeUpstreamDeep(ctx, u, models)
	if len(checks) != 2 {
		t.Fatalf("应返回 2 条检查结果，得到 %d", len(checks))
	}
	byModel := map[string]probeCheck{}
	for _, c := range checks {
		byModel[c.Model] = c
	}
	if !byModel["gpt-6-sol"].OK {
		t.Errorf("gpt-6-sol 应判为可调用：%+v", byModel["gpt-6-sol"])
	}
	bad := byModel["gpt-6.1-sol"]
	if bad.OK || bad.Status != 404 {
		t.Errorf("gpt-6.1-sol 应判为不可调用（404），得到 %+v", bad)
	}
	if bad.Error == "" {
		t.Error("不可调用必须带上游原文（否则界面只能显示一个红点，说不清为什么）")
	}
}

// TestProbeUpstreamDeepUsesRemoteModel 深度探活要按**映射后的上游名**探测。
//
// 用户可见名 gpt-6-sol 可能映射到上游的另一个名字；探错名字会得到假阴性。
func TestProbeUpstreamDeepUsesRemoteModel(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		m, _ := body["model"].(string)
		seen = append(seen, m)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	u := Upstream{BaseURL: srv.URL, APIKey: "sk-x"}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	probeUpstreamDeep(ctx, u, []UpstreamModel{
		{Model: "alias-a", RemoteModel: "real-a"},
		{Model: "same-b"},
	})
	if len(seen) != 2 {
		t.Fatalf("应发 2 次，实际 %d", len(seen))
	}
	got := map[string]bool{seen[0]: true, seen[1]: true}
	if !got["real-a"] || !got["same-b"] {
		t.Errorf("探测用的模型名 = %v，期望 real-a 与 same-b（有映射用映射，没映射用同名）", seen)
	}
}

// TestSuggestCreditPer1k 建议单价 = 官方倍率 × 上游倍率；任一缺失就返回 0。
func TestSuggestCreditPer1k(t *testing.T) {
	cases := []struct {
		credits string
		ratio   float64
		want    float64
	}{
		{"x0.52", 0.224, 0.1165}, // 0.52 × 0.224 = 0.11648 → 四位小数
		{"X1.20", 0.224, 0.2688}, // 大写 X 也要认
		{"0.17", 1, 0.17},        // 不带 x 前缀
		{"x0.52", 0, 0},          // 没读到上游倍率 → 不给建议
		{"", 0.224, 0},           // 官方倍率缺失 → 不给建议
		{"xabc", 0.224, 0},       // 非法内容 → 不给建议
		{"x-1", 0.224, 0},        // 负数 → 不给建议
		{"x0", 0.224, 0},         // 0 倍率 → 不给建议（否则会写出"免费"）
	}
	for _, c := range cases {
		if got := suggestCreditPer1k(c.credits, c.ratio); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("suggestCreditPer1k(%q, %v) = %v，期望 %v", c.credits, c.ratio, got, c.want)
		}
	}
}

// TestStoreUpstreamRatioRoundTrip 上游倍率要能落库读回（否则刷新页面就丢了）。
func TestStoreUpstreamRatioRoundTrip(t *testing.T) {
	store, _, _ := newTestApp(t, &Account{
		UID: "ur-acct", Realm: "cn", AccessToken: "at", RefreshToken: "rt",
		ConsumeGroup: "apply", PoolState: "in",
	})
	defer store.Close()

	id, err := store.SaveUpstream(&Upstream{
		Name: "ur-relay", Kind: upstreamKindOpenAI,
		BaseURL: "https://ur.example.com/v1", APIKey: "sk-ur",
		Enabled: true, Priority: 100,
	})
	if err != nil {
		t.Fatalf("SaveUpstream: %v", err)
	}
	if err := store.SetUpstreamRatio(id, 0.224); err != nil {
		t.Fatalf("SetUpstreamRatio: %v", err)
	}
	list, err := store.ListUpstreams()
	if err != nil {
		t.Fatalf("ListUpstreams: %v", err)
	}
	var got float64
	for _, u := range list {
		if u.ID == id {
			got = u.UpstreamRatio
		}
	}
	if math.Abs(got-0.224) > 1e-9 {
		t.Fatalf("读回的上游倍率 = %v，期望 0.224", got)
	}

	// 读不到时（传 0）**不得**清掉已有值：那会把"已知成本"变回未知。
	if err := store.SetUpstreamRatio(id, 0); err != nil {
		t.Fatalf("SetUpstreamRatio(0): %v", err)
	}
	list, _ = store.ListUpstreams()
	for _, u := range list {
		if u.ID == id && math.Abs(u.UpstreamRatio-0.224) > 1e-9 {
			t.Fatalf("传 0 不该覆盖已有值，得到 %v", u.UpstreamRatio)
		}
	}
}
