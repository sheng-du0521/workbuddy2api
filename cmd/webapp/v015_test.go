// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newKeyForZeroBalanceUser 造一个「余额为 0 的普通用户 + 一把有效 Key」。
// 用于验证余额闸的放行边界。
func newKeyForZeroBalanceUser(t *testing.T, store *Store, username string) string {
	t.Helper()
	u, err := store.CreateUser(username, "pw")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	raw := "sk-wb-test-" + username
	if _, err := store.CreateAPIKey(u.ID, "测试", HashAPIKey(raw), raw, "sk-wb", "", "", ""); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	return raw
}

// okHandler 记录是否被放行。
func okHandler(hit *bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		*hit = true
		w.WriteHeader(http.StatusOK)
	}
}

// TestModelsExemptFromBalance 核心回归：/v1/models 不受余额闸拦。
//
// 为什么这条最重要：几乎所有 OpenAI 兼容客户端在「添加服务商」时**第一步就是拉
// /v1/models**。此前该接口与 chat 共用同一道余额闸，余额为 0 时返回 402，
// 客户端直接报「连接失败」—— 用户看到连接错误、完全看不到真正原因，
// 于是反复检查 Base URL 与密钥。实测在公网正是此症状。
func TestModelsExemptFromBalance(t *testing.T) {
	store, _, a := newTestApp(t)
	defer store.Close()
	raw := newKeyForZeroBalanceUser(t, store, "pooruser")

	// ① /v1/models：余额 0 也必须放行。
	var hit bool
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	a.apiKeyGuardNoBalance(okHandler(&hit))(rec, req)
	if !hit || rec.Code != http.StatusOK {
		t.Fatalf("/v1/models 应免余额放行，得 code=%d hit=%v body=%s", rec.Code, hit, rec.Body.String())
	}

	// ② chat：余额 0 必须拦住（豁免只给元信息接口，不能扩散到计费接口）。
	hit = false
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req2.Header.Set("Authorization", "Bearer "+raw)
	a.apiKeyGuard(okHandler(&hit))(rec2, req2)
	if hit {
		t.Fatal("余额 0 时 chat 必须被拦")
	}
	if rec2.Code != http.StatusPaymentRequired {
		t.Fatalf("应 402，得 %d", rec2.Code)
	}
	if !strings.Contains(rec2.Body.String(), "insufficient balance") {
		t.Fatalf("应给出余额不足提示，得 %s", rec2.Body.String())
	}
}

// TestModelsStillRequiresValidKey 豁免余额 ≠ 豁免鉴权：无效 Key 仍必须 401。
func TestModelsStillRequiresValidKey(t *testing.T) {
	store, _, a := newTestApp(t)
	defer store.Close()

	var hit bool
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-wb-not-exist")
	a.apiKeyGuardNoBalance(okHandler(&hit))(rec, req)
	if hit {
		t.Fatal("无效 Key 不应放行")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("应 401，得 %d", rec.Code)
	}
}

// TestModelsStillRequiresKey 无 Key 仍 401（不能因为免余额就放开）。
func TestModelsStillRequiresKey(t *testing.T) {
	store, _, a := newTestApp(t)
	defer store.Close()
	var hit bool
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	a.apiKeyGuardNoBalance(okHandler(&hit))(rec, req)
	if hit || rec.Code != http.StatusUnauthorized {
		t.Fatalf("无 Key 应 401，得 code=%d hit=%v", rec.Code, hit)
	}
}

// TestMaxPerProxyBlankMeansUnlimited 「同一出口账号数」留空 = 不限。
//
// 回归护栏：此前缺省是 3，会让「没配过」的部署被悄悄约束住 ——
// 分配时报超载、看板标红，而管理员从没下过这个决定。
func TestMaxPerProxyBlankMeansUnlimited(t *testing.T) {
	store, _, a := newTestApp(t)
	defer store.Close()

	cases := []struct {
		kv   string
		want int
	}{
		{"", 0},    // 留空 → 不限
		{"  ", 0},  // 空白 → 不限
		{"0", 0},   // 显式 0 → 不限
		{"-1", 0},  // 负数 → 不限
		{"abc", 0}, // 非法 → 不限
		{"2", 2},
		{"5", 5},
	}
	for _, c := range cases {
		if err := store.SetKV(kvEgressMaxPerProxy, c.kv); err != nil {
			t.Fatalf("SetKV: %v", err)
		}
		if got := store.egressMaxPerProxy(); got != c.want {
			t.Errorf("egressMaxPerProxy(kv=%q) = %d，期望 %d", c.kv, got, c.want)
		}
	}

	// 不限时看板不得标超阈值（哪怕一条线路上挤了很多账号）。
	for i := 0; i < 5; i++ {
		acc := &Account{UID: "s" + string(rune('a'+i)), Realm: "cn", AccessToken: "a",
			EgressProxy: "http://10.0.0.1:8080", ConsumeGroup: cgProxy}
		if err := store.UpsertAccount(acc); err != nil {
			t.Fatalf("UpsertAccount: %v", err)
		}
	}
	if err := store.SetKV(kvEgressMaxPerProxy, ""); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	if err := a.proxy.SyncAccounts(); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}
	v, err := a.egressOverview()
	if err != nil {
		t.Fatalf("egressOverview: %v", err)
	}
	if v.MaxPerProxy != 0 {
		t.Fatalf("看板应回显上限 0（不限），得 %d", v.MaxPerProxy)
	}
	if v.OverCount != 0 {
		t.Fatalf("不限时不应有超额线路，得 %d", v.OverCount)
	}
	for _, p := range v.Proxies {
		if p.URL == "http://10.0.0.1:8080" && p.OverThreshold {
			t.Fatal("不限时该线路不应标超阈值")
		}
	}
}
