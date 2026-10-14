// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// settings_payload_test.go 「设置」各面板保存请求的契约回归。
//
// ## 为什么需要
//
// 管理页的每个「保存」按钮发的是**部分字段**的 JSON。而 handleAdminSettings 里
// 那些可选字段过去被误嵌进 `if req.EgressMaxPerProxy != nil` 的块里，于是：
//
//	前端「反代选号 · 防封」保存 → 带 egress_max_per_proxy、不带 egress_on_line_down
//	→ `strings.TrimSpace(*req.EgressOnLineDown)` 解引用 nil → panic
//	→ net/http 断开连接，页面只看到「保存报错」，没有任何可读文案。
//
// 这个缺陷能活下来的原因很典型：**它是"部分字段缺失"才触发的**，而当时所有
// 单测都是逐个字段单独 POST（每次只带一个字段 → 守卫刚好非 nil）。所以本文件
// 的用例刻意直接抄**前端真实载荷**，而不是挑字段——载荷是唯一有权威的规格。
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// postSettings 用指定 JSON body 调 handleAdminSettings（POST 分支）。
// panic 会被显式转成 t.Fatal —— 否则用例会以一个栈回溯失败，看不出是这条契约断了。
func postSettings(t *testing.T, a *app, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/settings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	var panicked any
	func() {
		defer func() { panicked = recover() }()
		a.handleAdminSettings(rec, req)
	}()
	if panicked != nil {
		t.Fatalf("handleAdminSettings panic（这正是「保存报错」的成因）：%v\nbody=%s", panicked, body)
	}
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// TestAdminSettingsProxyPanelPayload 前端「反代选号 · 防封」保存按钮的**真实载荷**
// （cmd/webapp/ui/admin.html 的 btnSaveProxy）必须 200 且全部落库。
//
// 关键点：该载荷**不含** egress_on_line_down / update_check_enabled 等字段 ——
// 任何对这些字段的无守卫解引用都会在这里炸出来。
func TestAdminSettingsProxyPanelPayload(t *testing.T) {
	store, _, a := newTestApp(t)
	defer store.Close()

	// 与 btnSaveProxy 的 post() 参数逐字对应。
	const payload = `{
	  "proxy_groups":"用户申请,反代",
	  "proxy_accounts":"",
	  "proxy_min_gap_seconds":0,
	  "pick_mode":"weighted",
	  "egress_proxy_default":"",
	  "egress_max_per_proxy":0,
	  "egress_jitter_ms":0,
	  "pick_sink_on_5xx":"off",
	  "device_token_fallback":"off",
	  "device_token_stale_days":30,
	  "fingerprint_samples":"off"
	}`
	code, out := postSettings(t, a, payload)
	if code != http.StatusOK {
		t.Fatalf("保存返回 %d，期望 200：%v", code, out)
	}

	// 值必须真的写进去（只回 200 不算数 —— 此前"只加注释不加代码"那类补丁
	// 就是这样蒙混过关的）。
	cases := map[string]string{
		kvProxyGroups:          "用户申请,反代",
		kvEgressMaxPerProxy:    "0",
		kvEgressJitterMs:       "0",
		kvPickSinkOn5xx:        "off",
		kvDeviceTokenFallback:  "off",
		kvDeviceTokenStaleDays: "30",
		kvFingerprintSamples:   "off",
		kvProxyMinGap:          "0",
	}
	for k, want := range cases {
		got, err := store.GetKV(k)
		if err != nil {
			t.Fatalf("GetKV(%s): %v", k, err)
		}
		if strings.TrimSpace(got) != want {
			t.Errorf("kv[%s] = %q，期望 %q", k, got, want)
		}
	}
}

// TestAdminSettingsOmittedOptionalFieldsDoNotPanic 只带一个字段也不能 panic。
//
// 这是上一类缺陷的另一半：守卫必须**各自独立**。逐个字段跑一遍，
// 保证"只发该字段"时其余字段的 nil 不会被执行到。
func TestAdminSettingsOmittedOptionalFieldsDoNotPanic(t *testing.T) {
	store, _, a := newTestApp(t)
	defer store.Close()

	bodies := []string{
		`{}`,
		`{"multiplier":2}`,
		`{"egress_max_per_proxy":5}`,
		`{"egress_max_per_proxy":5,"append":0}`,
		`{"egress_on_line_down":"strict"}`,
		`{"egress_on_line_down":"migrate","egress_max_per_proxy":0}`,
		`{"device_token_fallback":"derive"}`,
		`{"device_token_stale_days":0}`,
		`{"pick_sink_on_5xx":"on"}`,
		`{"fingerprint_samples":"on"}`,
		`{"fingerprint_samples":"on","device_token_fallback":"off","pick_sink_on_5xx":"on"}`,
	}
	for _, b := range bodies {
		code, out := postSettings(t, a, b)
		if code != http.StatusOK {
			t.Errorf("body=%s 返回 %d（期望 200）：%v", b, code, out)
		}
	}
}

// TestAdminSettingsEgressMaxPerProxyStillValidates 上面修的是"缺字段"，但不能
// 顺手把"有字段时的语义"改掉：超上限的值仍应照常落库、负值钳到 0。
func TestAdminSettingsEgressMaxPerProxyStillValidates(t *testing.T) {
	store, _, a := newTestApp(t)
	defer store.Close()

	if code, _ := postSettings(t, a, `{"egress_max_per_proxy":9}`); code != http.StatusOK {
		t.Fatalf("写 9 应 200，得 %d", code)
	}
	if v, _ := store.GetKV(kvEgressMaxPerProxy); v != "9" {
		t.Errorf("egress_max_per_proxy = %q，期望 9", v)
	}
	if code, _ := postSettings(t, a, `{"egress_max_per_proxy":-3}`); code != http.StatusOK {
		t.Fatalf("写 -3 应 200（钳到 0），得 %d", code)
	}
	if v, _ := store.GetKV(kvEgressMaxPerProxy); v != "0" {
		t.Errorf("egress_max_per_proxy = %q，期望 0（负值钳零）", v)
	}
}

// TestAdminSettingsUpdatePanelPayload 前端「关于 · 版本更新提醒」保存按钮的载荷。
func TestAdminSettingsUpdatePanelPayload(t *testing.T) {
	store, _, a := newTestApp(t)
	defer store.Close()

	code, out := postSettings(t, a, `{"update_check_enabled":true,"update_manifest_url":"https://example.invalid/version.json"}`)
	if code != http.StatusOK {
		t.Fatalf("更新设置保存返回 %d，期望 200：%v", code, out)
	}
	if v, _ := store.GetKV(kvUpdateEnabled); v != "1" {
		t.Errorf("update_check_enabled = %q，期望 1", v)
	}
	if v, _ := store.GetKV(kvUpdateManifest); v != "https://example.invalid/version.json" {
		t.Errorf("update_manifest_url = %q", v)
	}
}

// TestRecovererTurnsPanicIntoReadable500 兜 panic 的中间件本身就是「保存报错」
// 这类问题的止损措施：把"连接被断开、前端只看到 Failed to fetch"变成一条可读的 500。
//
// 三条断言缺一不可：
//   - 有响应（否则前端仍只会看到网络错误）；
//   - 是 JSON 且带 error 文案（前端 toast 走的是 e.message）；
//   - **不含 panic 原文**（栈/内部变量不能外泄，与 errsafe.go 同一口径）。
func TestRecovererTurnsPanicIntoReadable500(t *testing.T) {
	h := recoverer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom-secret-internal-detail")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/admin/settings", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("panic 应转 500，得 %d", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON：%v（body=%q）", err, rec.Body.String())
	}
	if s, _ := out["error"].(string); strings.TrimSpace(s) == "" {
		t.Error("500 响应必须带可读的 error 文案")
	}
	if strings.Contains(rec.Body.String(), "boom-secret-internal-detail") {
		t.Error("panic 原文不得外泄给调用方")
	}
}

// TestRecovererPassesThroughNormalResponse 正常请求不得被中间件改动。
func TestRecovererPassesThroughNormalResponse(t *testing.T) {
	h := recoverer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != `{"ok":true}` {
		t.Errorf("正常响应被改动：code=%d body=%q", rec.Code, rec.Body.String())
	}
}
