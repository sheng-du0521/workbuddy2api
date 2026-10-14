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
	"time"

	"golang.org/x/crypto/bcrypt"
)

// TestValidatePasswordStrength 强度规则的逐条锁死（需求 3）。
//
// 规则（与 passwordPolicyHint 同源）：
//  1. 8~128 位；2. 至少两类字符；3. 不得与用户名相同；4. 不得与原密码相同。
func TestValidatePasswordStrength(t *testing.T) {
	cases := []struct {
		name    string
		user    string
		pw      string
		old     string
		wantErr bool
	}{
		{"太短", "alice", "abc123", "oldpass123", true},
		{"刚好 8 位且两类", "alice", "abcd1234", "oldpass123", false},
		{"纯数字", "alice", "12345678", "oldpass123", true},
		{"纯字母", "alice", "abcdefgh", "oldpass123", true},
		{"纯符号", "alice", "!@#$%^&*", "oldpass123", true},
		{"字母+数字", "alice", "pass1234", "oldpass123", false},
		{"字母+符号", "alice", "pass!!!!", "oldpass123", false},
		{"与用户名相同", "alice", "alice", "oldpass123", true},
		{"与原密码相同", "bob", "oldpass123", "oldpass123", true},
		{"超长", "bob", strings.Repeat("a1", 65), "oldpass123", true},
	}
	for _, c := range cases {
		err := validatePasswordStrength(c.user, c.pw, c.old)
		if c.wantErr && err == nil {
			t.Errorf("%s：应被拒但通过了", c.name)
		}
		if !c.wantErr && err != nil {
			t.Errorf("%s：应通过但被拒（%v）", c.name, err)
		}
	}
}

// newUserWithPassword 建一个带 bcrypt 密码的用户。
func newUserWithPassword(t *testing.T, store *Store, name, password string) *User {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	u, err := store.CreateUser(name, string(hash))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return u
}

// changePw 调一次改密码 handler。
func changePw(a *app, u *User, cookieToken, oldPw, newPw string) *httptest.ResponseRecorder {
	body := `{"old_password":` + jsonStr(oldPw) + `,"new_password":` + jsonStr(newPw) + `}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/password", strings.NewReader(body))
	if cookieToken != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookieToken})
	}
	rec := httptest.NewRecorder()
	a.handleChangePassword(rec, withUser(req, u))
	return rec
}

// TestChangePasswordFlow 改密码全链路：原密码校验 → 强度 → 落库 → 会话处理。
func TestChangePasswordFlow(t *testing.T) {
	store, px, a := newTestApp(t)
	defer store.Close()
	defer px.Close()

	u := newUserWithPassword(t, store, "alice", "oldpass123")
	// 两条会话：当前这条保命，另一条应被注销。
	curRaw, otherRaw := "cur-token-aaaa", "other-token-bbbb"
	if err := store.CreateSession(HashToken(curRaw), u.ID, time.Hour); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := store.CreateSession(HashToken(otherRaw), u.ID, time.Hour); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// 1) 原密码错 → 401，且密码未被改动。
	rec := changePw(a, u, curRaw, "wrong-old", "newpass456")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("原密码错应 401，得 %d（%s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "原密码") {
		t.Fatalf("错误信息应点明原密码，得 %s", rec.Body.String())
	}
	fresh, _ := store.GetUserByID(u.ID)
	if bcrypt.CompareHashAndPassword([]byte(fresh.PasswordHash), []byte("oldpass123")) != nil {
		t.Fatalf("原密码失败的请求不应改动密码")
	}

	// 2) 强度不足 → 400。
	if rec := changePw(a, u, curRaw, "oldpass123", "1234567"); rec.Code != http.StatusBadRequest {
		t.Fatalf("弱密码应 400，得 %d", rec.Code)
	}
	// 3) 与原密码相同 → 400。
	if rec := changePw(a, u, curRaw, "oldpass123", "oldpass123"); rec.Code != http.StatusBadRequest {
		t.Fatalf("与原密码相同应 400，得 %d", rec.Code)
	}

	// 4) 成功 → 200。
	rec = changePw(a, u, curRaw, "oldpass123", "newpass456")
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，得 %d（%s）", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["ok"] != true {
		t.Fatalf("应返回 ok=true，得 %v", resp)
	}
	if n, _ := resp["other_sessions"].(float64); int(n) != 1 {
		t.Fatalf("应注销 1 个其它会话，得 %v", resp["other_sessions"])
	}

	// 新密码可登录（哈希已更新）。
	fresh, _ = store.GetUserByID(u.ID)
	if bcrypt.CompareHashAndPassword([]byte(fresh.PasswordHash), []byte("newpass456")) != nil {
		t.Fatalf("新密码应可校验通过")
	}
	if bcrypt.CompareHashAndPassword([]byte(fresh.PasswordHash), []byte("oldpass123")) == nil {
		t.Fatalf("旧密码不应再通过")
	}
	// 当前会话保留（否则用户改完密码就被踢出，体验很差）。
	if _, ok := store.LookupSession(HashToken(curRaw)); !ok {
		t.Fatalf("当前会话应保留")
	}
	// 其它会话已注销。
	if _, ok := store.LookupSession(HashToken(otherRaw)); ok {
		t.Fatalf("其它会话应被注销")
	}
	// 响应体不得回显任何密码。
	if strings.Contains(rec.Body.String(), "newpass456") || strings.Contains(rec.Body.String(), "oldpass123") {
		t.Fatalf("响应不应回显密码：%s", rec.Body.String())
	}
}

// TestChangePasswordUnknownOldSameMessage 未登录 → 401（不泄露任何信息）。
func TestChangePasswordUnauthorized(t *testing.T) {
	store, px, a := newTestApp(t)
	defer store.Close()
	defer px.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/password",
		strings.NewReader(`{"old_password":"a","new_password":"bbbbbbbb1"}`))
	rec := httptest.NewRecorder()
	a.handleChangePassword(rec, req) // 不注入用户
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未登录应 401，得 %d", rec.Code)
	}
}

// jsonStr 把字符串编成 JSON 字面量（测试里避免手写转义）。
func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
