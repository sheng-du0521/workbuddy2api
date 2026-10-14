// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// TestMaskSecretShape 掩码形态：保留前缀与最后 4 位，中间固定 8 个 •。
//
// 这条锁的是「掩码长度不随真实密钥长度变化」——否则掩码位数本身就是长度泄露。
func TestMaskSecretShape(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"空值", "", "sk-wb-••••••••"},
		{"标准密钥", "sk-wb-abcdefghij1234", "sk-wb-••••••••1234"},
		{"长密钥掩码长度不变", "sk-wb-" + strings.Repeat("x", 60) + "9876", "sk-wb-••••••••9876"},
		{"过短整体掩码", "abc", "••••••••"},
	}
	for _, c := range cases {
		if got := maskSecret(c.in); got != c.want {
			t.Errorf("%s: maskSecret(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
	// 掩码里绝不能出现连续的真实字符段（尾 4 位是刻意保留的识别位）。
	m := maskSecret("sk-wb-abcdefghij1234")
	if strings.Contains(m, "abcdefghij") {
		t.Fatalf("掩码泄露了中段真实字符：%q", m)
	}
}

// TestMaskSecretNeverLeaksMiddle 任意长度输入下，掩码都不含原文中间段。
func TestMaskSecretNeverLeaksMiddle(t *testing.T) {
	for _, n := range []int{0, 1, 5, 12, 20, 43} {
		plain := "sk-wb-" + strings.Repeat("Q", n)
		m := maskSecret(plain)
		// 中段（去掉前缀与尾 4 位后）不应出现在掩码里。
		if n > 0 && strings.Contains(m, strings.Repeat("Q", n)) {
			t.Fatalf("n=%d 掩码泄露原文：%q", n, m)
		}
	}
}

// TestListKeysHidesPlaintext 列表接口不得返回明文（需求 1 的核心断言）。
func TestListKeysHidesPlaintext(t *testing.T) {
	store, px, a := newTestApp(t)
	defer store.Close()
	defer px.Close()

	u, err := store.CreateUser("alice", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	plain := GenerateAPIKey()
	enc, err := a.secret.Encrypt(plain)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	k, err := store.CreateAPIKey(u.ID, "k1", HashAPIKey(plain), enc, keyPrefix(plain), "", "", "")
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	rec := httptest.NewRecorder()
	a.handleListKeys(rec, withUser(httptest.NewRequest(http.MethodGet, "/api/keys", nil), u))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d，响应 %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, plain) {
		t.Fatalf("列表响应泄露了完整密钥！body=%s", body)
	}
	// 也不能以 "key" 字段形式夹带（旧实现就是 item["key"] = plain）。
	var resp struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应：%v", err)
	}
	if len(resp.Keys) != 1 {
		t.Fatalf("应有 1 把密钥，得 %d", len(resp.Keys))
	}
	item := resp.Keys[0]
	if _, has := item["key"]; has {
		t.Fatalf("列表项不应含 key 字段（明文），得 %v", item)
	}
	masked, _ := item["masked"].(string)
	if !strings.Contains(masked, "••") {
		t.Fatalf("masked 应为掩码形态，得 %q", masked)
	}
	if masked == plain {
		t.Fatalf("masked 不应等于明文")
	}
	if masked != maskSecret(plain) {
		t.Fatalf("masked 应为 maskSecret(plain)，得 %q，期望 %q", masked, maskSecret(plain))
	}
	if item["revealable"] != true {
		t.Fatalf("有密文时 revealable 应为 true，得 %v", item["revealable"])
	}
	if int64(item["id"].(float64)) != k.ID {
		t.Fatalf("id 不匹配")
	}
}

// TestRevealKeyOwnerOnly 显式查看：本人可取回明文；他人（含不存在的 id）一律 404。
func TestRevealKeyOwnerOnly(t *testing.T) {
	store, px, a := newTestApp(t)
	defer store.Close()
	defer px.Close()

	alice, _ := store.CreateUser("alice", "hash")
	bob, _ := store.CreateUser("bob", "hash")
	plain := GenerateAPIKey()
	enc, _ := a.secret.Encrypt(plain)
	k, err := store.CreateAPIKey(alice.ID, "k1", HashAPIKey(plain), enc, keyPrefix(plain), "", "", "")
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	reveal := func(u *User, id string) *httptest.ResponseRecorder {
		req := withUser(httptest.NewRequest(http.MethodGet, "/api/keys/"+id+"/reveal", nil), u)
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		a.handleRevealKey(rec, req)
		return rec
	}
	idStr := strconv.FormatInt(k.ID, 10)

	// 本人：拿到明文。
	rec := reveal(alice, idStr)
	if rec.Code != http.StatusOK {
		t.Fatalf("本人查看应 200，得 %d（%s）", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["key"] != plain {
		t.Fatalf("应返回明文，得 %v", out["key"])
	}

	// 他人：404，且响应体里不能出现明文。
	rec = reveal(bob, idStr)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("跨用户查看应 404，得 %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), plain) {
		t.Fatalf("跨用户响应泄露明文")
	}

	// 非法 id：400。
	rec = reveal(alice, "abc")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 id 应 400，得 %d", rec.Code)
	}
}

// TestRevealKeyWithoutCipher 无密文（如以 -secret none 启动时创建的 key）→ 409，明确告知而不是返回空。
func TestRevealKeyWithoutCipher(t *testing.T) {
	store, px, a := newTestApp(t)
	defer store.Close()
	defer px.Close()

	u, _ := store.CreateUser("alice", "hash")
	plain := GenerateAPIKey()
	k, err := store.CreateAPIKey(u.ID, "k1", HashAPIKey(plain), "", keyPrefix(plain), "", "", "")
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	req := withUser(httptest.NewRequest(http.MethodGet, "/api/keys/1/reveal", nil), u)
	req.SetPathValue("id", strconv.FormatInt(k.ID, 10))
	rec := httptest.NewRecorder()
	a.handleRevealKey(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("无密文应 409，得 %d（%s）", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), plain) {
		t.Fatalf("响应不应含明文")
	}

	// 列表里 revealable 也应为 false（前端据此禁用按钮）。
	rec = httptest.NewRecorder()
	a.handleListKeys(rec, withUser(httptest.NewRequest(http.MethodGet, "/api/keys", nil), u))
	var resp struct {
		Keys []map[string]any `json:"keys"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Keys[0]["revealable"] != false {
		t.Fatalf("无密文时 revealable 应为 false，得 %v", resp.Keys[0]["revealable"])
	}
}
