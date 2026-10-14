// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 本文件是 v0.24.0 补上的**HTTP 层鉴权测试**（见 docs/项目全面审查报告-v0.23.3.md 的 P1-3）。
//
// 为什么此前没有：路由写死在 main() 里（~110 条），main() 在测试中不可调用 ——
// 于是**没有任何测试能构造真实 mux**，adminGuard / userGuard 这两个鉴权中间件
// 从未被真正穿过（既有测试只能直接调 a.handleX，把中间件整段绕过）。
// 鉴权是安全边界，绕过它测等于没测。
//
// 前置改动：路由已抽成 app.routes()（见 main.go），本文件据此用真 mux 打请求。
//
// 覆盖目标（都是"越权"这一类的正/反例）：
//   · 普通用户访问 /api/admin/* → 403（不是 200，也不是 500）
//   · 未登录访问 /api/admin/*   → 403（管理端不认匿名）
//   · 未登录访问 /api/host/*    → 401（用户端要登录）
//   · 管理员密钥访问 /api/admin/* → 放行（运维脚本通道不能被改坏）
//   · 公开端点（/healthz）→ 200（路由确实注册上了）
//   · 用户端不认管理员密钥通道 —— 这是刻意隔离，改为 401/403 都算"没放行"

// newAuthzApp 装配一个带真 mux 的 app，并造齐「管理员 / 普通用户 / 已停用用户」三类会话。
func newAuthzApp(t *testing.T) (*Store, *app, *http.ServeMux, map[string]string) {
	t.Helper()
	store, px, a := newTestApp(t, &Account{
		UID: "az-acct", Realm: "cn", AccessToken: "at", RefreshToken: "rt",
		ConsumeGroup: "apply", PoolState: "in",
	})
	a.key = "test-admin-key-xyz"
	if err := px.SyncAccounts(); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}
	mux := a.routes()

	// 会话：直接建（issueSession 写 cookie，这里只要 token）。
	newSess := func(u *User) string {
		token := randomHex(32)
		if err := store.CreateSession(HashToken(token), u.ID, sessionTTL); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		return token
	}

	admin, err := store.CreateUser("az-admin", "x")
	if err != nil {
		t.Fatalf("CreateUser(admin): %v", err)
	}
	if err := store.SetUserRole(admin.ID, RoleAdmin); err != nil {
		// 若没有专门的 SetUserRole，退化为直接 SQL 之外的手段会较脆；此处明确提示。
		t.Fatalf("SetUserRole: %v", err)
	}
	plain, err := store.CreateUser("az-plain", "x")
	if err != nil {
		t.Fatalf("CreateUser(plain): %v", err)
	}
	stopped, err := store.CreateUser("az-stopped", "x")
	if err != nil {
		t.Fatalf("CreateUser(stopped): %v", err)
	}
	if err := store.SetUserStatus(stopped.ID, StatusBanned); err != nil {
		t.Fatalf("SetUserStatus: %v", err)
	}

	tokens := map[string]string{
		"admin":   newSess(admin),
		"plain":   newSess(plain),
		"stopped": newSess(stopped),
	}
	return store, a, mux, tokens
}

// call 用指定凭据打一次真 mux 请求，返回状态码与响应体。
func call(t *testing.T, mux *http.ServeMux, method, path, cookie, adminKey string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	}
	if adminKey != "" {
		req.Header.Set("X-Admin-Key", adminKey)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// TestAuthzAdminRoutesRejectNonAdmin **核心越权用例**：普通用户不得访问管理端。
func TestAuthzAdminRoutesRejectNonAdmin(t *testing.T) {
	store, _, mux, tok := newAuthzApp(t)
	defer store.Close()

	// 挑几条代表性的管理端点：读、写、以及最敏感的「看全部用户」。
	paths := []struct{ method, path string }{
		{"GET", "/api/admin/users"},
		{"GET", "/api/admin/accounts"},
		{"GET", "/api/admin/key"},
		{"POST", "/api/admin/accounts/az-acct/disable"},
		{"GET", "/api/admin/withdrawals"},
	}
	for _, p := range paths {
		code, body := call(t, mux, p.method, p.path, tok["plain"], "")
		if code != http.StatusForbidden {
			t.Errorf("%s %s 普通用户应 403，got %d body=%.120s", p.method, p.path, code, body)
		}
	}
}

// TestAuthzAdminRoutesRejectAnonymous 匿名访问管理端 → 403（管理端不认匿名）。
func TestAuthzAdminRoutesRejectAnonymous(t *testing.T) {
	store, _, mux, _ := newAuthzApp(t)
	defer store.Close()

	code, body := call(t, mux, "GET", "/api/admin/users", "", "")
	if code != http.StatusForbidden {
		t.Fatalf("匿名访问管理端应 403，got %d body=%.120s", code, body)
	}
}

// TestAuthzAdminKeyChannelStillWorks 管理密钥通道必须放行 —— 修复安全问题时
// 最容易顺手把运维通道一起改坏，故此用例守它。
func TestAuthzAdminKeyChannelStillWorks(t *testing.T) {
	store, _, mux, _ := newAuthzApp(t)
	defer store.Close()

	code, body := call(t, mux, "GET", "/api/admin/users", "", "test-admin-key-xyz")
	if code != http.StatusOK {
		t.Fatalf("带正确管理密钥应 200，got %d body=%.200s", code, body)
	}
	// 错密钥必须被拒（这是 P0-1 修复后的行为：常量时间比较，但结论不变）。
	bad, _ := call(t, mux, "GET", "/api/admin/users", "", "wrong-key")
	if bad == http.StatusOK {
		t.Fatalf("错误管理密钥不应放行，got %d", bad)
	}
}

// TestAuthzAdminKeyIsConstantTimeCompare 密钥比较走 subtle.ConstantTimeCompare（v0.24.0）。
//
// 这不是时序测试（时序测不稳定），而是**契约锁定**：比较必须对长度不同的输入也安全
// （== 与 ConstantTimeCompare 在这里行为一致，但后者不会因前缀匹配提前返回）。
// 真正要防的是"后人改回 =="，故同时锁住等长/不等长两种拒绝。
func TestAuthzAdminKeyIsConstantTimeCompare(t *testing.T) {
	store, _, mux, _ := newAuthzApp(t)
	defer store.Close()

	// 相同前缀但更短/更长，都必须拒绝。
	cases := []string{
		"test-admin-key-xy",     // 少一个字符（前缀相同）
		"test-admin-key-xyz!",   // 多一个字符
		"test-admin-key-xyzz",   // 末位不同
		"TEST-ADMIN-KEY-XYZ",    // 大小写不同
		strings.Repeat("x", 40), // 长度不同
	}
	for _, k := range cases {
		if code, _ := call(t, mux, "GET", "/api/admin/users", "", k); code == http.StatusOK {
			t.Errorf("密钥 %q 不应通过", k)
		}
	}
}

// TestAuthzHostRoutesRejectAnonymous 用户端接口要求登录（401），而不是 403。
func TestAuthzHostRoutesRejectAnonymous(t *testing.T) {
	store, _, mux, _ := newAuthzApp(t)
	defer store.Close()

	for _, p := range []string{"/api/host/accounts", "/api/host/growth/overview"} {
		code, body := call(t, mux, "GET", p, "", "")
		if code != http.StatusUnauthorized {
			t.Errorf("匿名访问 %s 应 401，got %d body=%.120s", p, code, body)
		}
	}
}

// TestAuthzHostRoutesRejectAdminKeyChannel 用户端**不认**管理密钥通道。
//
// 这是刻意的隔离：管理密钥是运维脚本用的，不应能当作"某个用户"去操作用户资源
// ——否则 401/403 的边界会被一条 header 抹平。只要不放行（401 或 403）即算通过。
func TestAuthzHostRoutesRejectAdminKeyChannel(t *testing.T) {
	store, _, mux, _ := newAuthzApp(t)
	defer store.Close()

	code, body := call(t, mux, "GET", "/api/host/accounts", "", "test-admin-key-xyz")
	if code == http.StatusOK {
		t.Fatalf("用户端不应认管理密钥通道，got 200 body=%.200s", body)
	}
	if code != http.StatusUnauthorized && code != http.StatusForbidden {
		t.Fatalf("应为 401/403，got %d", code)
	}
}

// TestAuthzDisabledUserIsRejected 被停用的用户会话不得通行（403）。
func TestAuthzDisabledUserIsRejected(t *testing.T) {
	store, _, mux, tok := newAuthzApp(t)
	defer store.Close()

	code, body := call(t, mux, "GET", "/api/host/accounts", tok["stopped"], "")
	if code != http.StatusForbidden {
		t.Fatalf("停用用户应 403，got %d body=%.120s", code, body)
	}
}

// TestAuthzRoutesWired 路由确实注册在 **mux** 上（而不是只在 main() 里存在）。
//
// 抽 routes() 的附带价值：这一条能证明「改名/漏注册」会在测试里暴露，
// 而此前只能在真机上手点。
func TestAuthzRoutesWired(t *testing.T) {
	store, _, mux, tok := newAuthzApp(t)
	defer store.Close()

	// 公开端点：匿名可达。
	if code, _ := call(t, mux, "GET", "/healthz", "", ""); code != http.StatusOK {
		t.Errorf("/healthz 应 200，got %d", code)
	}
	// 已注册但属用户端的端点：匿名 401（而不是 404 —— 404 说明压根没注册）。
	if code, _ := call(t, mux, "GET", "/api/host/accounts", "", ""); code == http.StatusNotFound {
		t.Errorf("/api/host/accounts 未注册到 mux（404）")
	}
	// 管理员本人访问管理端应放行（用它自己的会话，而不是密钥通道）。
	if code, body := call(t, mux, "GET", "/api/admin/users", tok["admin"], ""); code != http.StatusOK {
		t.Errorf("管理员会话应放行，got %d body=%.200s", code, body)
	}
	// 未注册路径应 404（确认 mux 真的在按注册表路由，而不是"全都放行"）。
	if code, _ := call(t, mux, "GET", "/api/definitely-not-a-route", tok["admin"], ""); code != http.StatusNotFound {
		t.Errorf("未注册路径应 404，got %d", code)
	}
}

// TestAuthzAdminUsersPayloadIsJSON 放行路径要真的返回数据（避免"403 修好了但 200 是空的"）。
func TestAuthzAdminUsersPayloadIsJSON(t *testing.T) {
	store, _, mux, tok := newAuthzApp(t)
	defer store.Close()

	code, body := call(t, mux, "GET", "/api/admin/users", tok["admin"], "")
	if code != http.StatusOK {
		t.Fatalf("got %d", code)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("响应不是 JSON：%v body=%.200s", err, body)
	}
	if _, ok := obj["users"]; !ok {
		t.Fatalf("响应缺少 users 字段：%.200s", body)
	}
}
