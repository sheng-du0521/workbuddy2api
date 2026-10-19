// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 路由兜底语义的回归测试（v0.31.49）。
//
// 现场（2026-10-19）：用户把客户端 base_url 填成裸域名 `http://47.103.194.103:8780`，
// SDK 自己补 `/chat/completions`，服务端回 `405 Method Not Allowed
// (target: http://47.103.194.103:8780)` —— 看的人只会以为网关"不支持这个接口"，
// 真实原因是路径少了一段 /v1，而 405 是 `GET /` 兜底替 404 背了锅。
package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// routeApp 起一个只关心路由的最小 app（不需要登录态）。
func routeApp(t *testing.T) (*Store, *app, *http.ServeMux) {
	t.Helper()
	store, _, a := newTestApp(t)
	mux := a.routes()
	t.Cleanup(func() { store.Close() })
	return store, a, mux
}

// TestRootPostIs404Not405 未注册路径的 POST 必须回 404 + 接入提示，而不是 405。
func TestRootPostIs404Not405(t *testing.T) {
	_, _, mux := routeApp(t)
	req := httptest.NewRequest("POST", "/chat/completions2", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("未注册路径应回 404，得 %d body=%.200s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "/v1/chat/completions") {
		t.Fatalf("404 必须给出正确的接入路径，得 %.300s", rec.Body.String())
	}
}

// TestRootGetStillServesUI GET / 仍然要出控制台首页（不能被兜底改动误伤）。
func TestRootGetStillServesUI(t *testing.T) {
	_, _, mux := routeApp(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 200 {
		t.Fatalf("GET / 应回 200（控制台首页），得 %d", rec.Code)
	}
	if !strings.Contains(strings.ToLower(rec.Body.String()), "<html") {
		t.Fatal("GET / 返回的不是 HTML 页面")
	}
}

// TestChatCompletionsAliasWithoutV1 不带 /v1 的别名必须存在（且仍然要鉴权）。
//
// 只断言"不是 404"：无 key 时应当是 401，这说明路由通了、鉴权在位。
func TestChatCompletionsAliasWithoutV1(t *testing.T) {
	_, _, mux := routeApp(t)
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/chat/completions", `{"model":"x","messages":[]}`},
		{"GET", "/models", ""},
	} {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code == 404 || rec.Code == 405 {
			t.Fatalf("%s %s 必须被路由接住（别名），得 %d body=%.200s",
				c.method, c.path, rec.Code, rec.Body.String())
		}
	}
}
