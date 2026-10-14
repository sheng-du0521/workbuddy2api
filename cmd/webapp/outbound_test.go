// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// 本文件锁死 v0.24.0 修复的 P1-1：**webapp 转发链路此前完全绕过出站改写管线**。
//
// 缺陷（见 docs/项目全面审查报告-v0.23.3.md 的 P1-1）：
// cmd/webapp 的 ServeChat 自建请求（json.Marshal → client.Do），从不调用
// upstream 的 ChatStreamContext，因此 prepareBody 的全部改写在这条链路上**从未生效**：
//   · 黑名单设备指纹脱敏（而 proxy.go 明确设了 SanitizeFingerprints=true —— 配置欺骗）
//   · developer 角色归一 / reasoning_effort 按模型降级
//   · prompt_cache_key 注入（按账号隔离的稳定缓存键，P0 费用优化）
//
// 这类缺陷的形态是**接线缺失**，单测 prepareBody 只能证明「函数本身对」，
// 恰恰漏掉「webapp 压根没调它」。所以必须从 ServeChat 入口真跑一遍、并在
// 上游侧把**真实出站 body** 抓下来断言 —— 这正是本文件的做法。

// captureUpstream 记录上游收到的原始请求体（供断言出站正文）。
//
// 记录**全部**请求而非仅首个：多请求用例（如「客户端自带 key 不被覆盖」）需要看第二次，
// 只存首个会让人误以为第二次的结果是它 —— 这类"夹具假象"比被测缺陷更难发现。
type captureUpstream struct {
	mu     sync.Mutex
	path   string
	bodies [][]byte
}

// server 起一个假上游：记录首个请求的 body，返回单帧 SSE（含 usage，便于计费收尾）。
func (c *captureUpstream) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		if len(c.bodies) == 0 {
			c.path = r.URL.Path
		}
		c.bodies = append(c.bodies, raw)
		c.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		frame := map[string]any{
			"id": "chatcmpl-test", "object": "chat.completion.chunk", "model": "m-test",
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"content": "hi"}, "finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 20, "total_tokens": 30, "credit": 1.0},
		}
		b, _ := json.Marshal(frame)
		w.Write([]byte("data: " + string(b) + "\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
}

// capturedBody 取上游收到的**最近一次** body（副本）。
func (c *captureUpstream) capturedBody() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bodies) == 0 {
		return nil
	}
	last := c.bodies[len(c.bodies)-1]
	out := make([]byte, len(last))
	copy(out, last)
	return out
}

// capturedN 取第 n 次（0 基）上游 body 副本。
func (c *captureUpstream) capturedN(n int) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n < 0 || n >= len(c.bodies) {
		return nil
	}
	b := c.bodies[n]
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

func (c *captureUpstream) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.bodies)
}

func (c *captureUpstream) firstPath() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.path
}

// newOutboundApp 装配一个可跑 ServeChat 的 app，并把上游指向 captureUpstream。
func newOutboundApp(t *testing.T) (*Store, *Proxy, *app, *captureUpstream, *httptest.Server) {
	t.Helper()
	store, px, a := newTestApp(t, &Account{
		UID: "ob-acct", Nickname: "outbound", Realm: "cn",
		AccessToken: "tok", RefreshToken: "rtok",
		ExpiresAt:    time.Now().Add(48 * time.Hour).Unix(),
		ConsumeGroup: "apply",
		PoolState:    "in",
	})
	cap := &captureUpstream{}
	up := cap.server(t)
	t.Cleanup(up.Close)
	px.uc.HTTP = &http.Client{Transport: rewriteTransport{target: up.URL}}
	// 与生产一致：proxy 装配时开启了指纹脱敏（历史缺陷正是「开了但没生效」）。
	px.uc.SanitizeFingerprints = true

	u, err := store.CreateUser("ob-payer", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := store.AddUserBalance(u.ID, 1000*MicroPerPoint); err != nil {
		t.Fatalf("AddUserBalance: %v", err)
	}
	raw := "sk-wb-outbound-test"
	if _, err := store.CreateAPIKey(u.ID, "t", HashAPIKey(raw), "enc", "sk-wb", "", "", ""); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	px.ReloadSelection()

	srv := httptest.NewServer(a.apiKeyGuard(px.ServeChat))
	t.Cleanup(srv.Close)
	t.Setenv("WB2A_TEST_RAWKEY", raw)
	return store, px, a, cap, srv
}

// sendChat 发一次对话并读完响应（读完才能保证 handler 已收尾）。
func sendChat(t *testing.T, srv *httptest.Server, rawKey, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+rawKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp
}

// TestServeChatSanitizesOutboundBody **本文件最重要的用例**：
// 从 ServeChat 真发一次请求，断言**上游实际收到**的正文已被脱敏。
//
// 夹具用的是 cc 身份句（上游逐字拦截的指纹之一）：脱敏后应变成
// "official CLI tool for Claude."，原文不得残留。
func TestServeChatSanitizesOutboundBody(t *testing.T) {
	store, _, _, cap, srv := newOutboundApp(t)
	defer store.Close()

	// 这句话是 upstream 侧的黑名单指纹（见 internal/upstream/sanitize_test.go 的常量）。
	const fingerprint = "You are Claude Code, Anthropic's official CLI for Claude."
	body := `{"model":"m-test","stream":true,"messages":[` +
		`{"role":"system","content":"` + fingerprint + `"},` +
		`{"role":"user","content":"hello"}]}`

	resp := sendChat(t, srv, "sk-wb-outbound-test", body)
	if resp.StatusCode != 200 {
		t.Fatalf("应 200，got %d", resp.StatusCode)
	}

	got := string(cap.capturedBody())
	if len(got) == 0 {
		t.Fatal("上游没收到请求体（断言无从谈起）")
	}
	// 1) 原文指纹必须**不再出现** —— 这是「脱敏生效」的直接证据。
	if strings.Contains(got, fingerprint) {
		t.Fatalf("出站正文仍含黑名单指纹，脱敏未生效：%s", got)
	}
	// 2) 必须已被改写为替换文案（而不是整段消息被删掉）。
	if !strings.Contains(got, "official CLI tool for Claude.") {
		t.Fatalf("出站正文未出现改写后的文案：%s", got)
	}
}

// TestServeChatInjectsPromptCacheKey 出站正文应带上 prompt_cache_key
// （按账号隔离的稳定缓存键）。这条断言的另一重意义：它是**管线跑过**的
// 独立证据 —— 不依赖具体脱敏规则，规则改了也依然成立。
func TestServeChatInjectsPromptCacheKey(t *testing.T) {
	store, _, _, cap, srv := newOutboundApp(t)
	defer store.Close()

	body := `{"model":"m-test","stream":true,` +
		`"conversation_id":"conv-abc-123",` +
		`"messages":[{"role":"user","content":"hi"}]}`
	resp := sendChat(t, srv, "sk-wb-outbound-test", body)
	if resp.StatusCode != 200 {
		t.Fatalf("应 200，got %d", resp.StatusCode)
	}

	var obj map[string]any
	if err := json.Unmarshal(cap.capturedBody(), &obj); err != nil {
		t.Fatalf("出站正文不是合法 JSON：%v", err)
	}
	key, _ := obj["prompt_cache_key"].(string)
	if key == "" {
		t.Fatalf("出站正文缺少 prompt_cache_key（说明改写管线没跑）：%v", obj)
	}
	// 客户端若自带 key，注入必须让位（既有优先级契约）。
	body2 := `{"model":"m-test","stream":true,"prompt_cache_key":"client-key",` +
		`"messages":[{"role":"user","content":"hi"}]}`
	if resp2 := sendChat(t, srv, "sk-wb-outbound-test", body2); resp2.StatusCode != 200 {
		t.Fatalf("应 200，got %d", resp2.StatusCode)
	}
	if cap.count() < 2 {
		t.Fatalf("应有两次上游请求，got %d", cap.count())
	}
	var obj2 map[string]any
	if err := json.Unmarshal(cap.capturedN(1), &obj2); err != nil {
		t.Fatalf("第二次出站正文不是合法 JSON：%v", err)
	}
	if k2, _ := obj2["prompt_cache_key"].(string); k2 != "client-key" {
		t.Fatalf("客户端自带的 prompt_cache_key 不应被覆盖，got %q", k2)
	}
}

// TestPrepareOutboundPerAccount 改写按**当前账号**进行：换号必须重算。
//
// 语义：指纹段与 effort 降级桶都由账号的 realm/uid 决定，若在循环外只算一次，
// 换到另一个账号后就会带着上一个账号的指纹发出去 —— 那正是「跨账号关联」的信号。
func TestPrepareOutboundPerAccount(t *testing.T) {
	store, px, _, _, _ := newOutboundApp(t)
	defer store.Close() // 不关会让 t.TempDir 清理报「文件被占用」
	if px.uc == nil {
		t.Fatal("uc 为空")
	}
	// 直接验语义：不同 uid 得到的 prompt_cache_key 必须不同（按账号硬隔离段）。
	// authFromAccount 是包内唯一可靠的构造途径（realm 字段未导出，只能走 auth.Parse）。
	accA := authFromAccount(&Account{UID: "uid-aaa", Realm: "cn", AccessToken: "t"})
	accB := authFromAccount(&Account{UID: "uid-bbb", Realm: "cn", AccessToken: "t"})
	if accA == nil || accB == nil {
		t.Fatal("构造 auth 失败")
	}
	body := []byte(`{"model":"m-test","stream":true,"conversation_id":"c1","messages":[{"role":"user","content":"x"}]}`)

	outA := px.uc.PrepareOutbound(body, accA, "c1")
	outB := px.uc.PrepareOutbound(body, accB, "c1")

	var a1, b1 map[string]any
	_ = json.Unmarshal(outA, &a1)
	_ = json.Unmarshal(outB, &b1)
	ka, _ := a1["prompt_cache_key"].(string)
	kb, _ := b1["prompt_cache_key"].(string)
	if ka == "" || kb == "" {
		t.Fatalf("两个账号都应注入 key：%q / %q", ka, kb)
	}
	if ka == kb {
		t.Fatalf("不同账号必须得到不同的 prompt_cache_key（跨账号硬隔离），都是 %q", ka)
	}

	// nil 账号必须原样返回（不得 panic）—— 调用方在选号失败路径上可能传 nil。
	if out := px.uc.PrepareOutbound(body, nil, ""); string(out) != string(body) {
		t.Fatalf("nil 账号应原样返回")
	}
}
