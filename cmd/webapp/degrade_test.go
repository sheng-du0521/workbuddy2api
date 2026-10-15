// 提示词降级重试的端到端验收（v0.31.4）。
//
// 背景：这条能力此前**只存在于旧网关**（internal/server），而"合并两个产品"的前提是
// 能力等值 —— 本文件把旧网关的行为逐条钉在 webapp 上：
//
//  1. 首遇内容拦截（400 审核文案 / 11128 渠道拒绝）→ 触发降级 + **同请求内重试一次**；
//  2. 重试时出站 system 换成 prompt.Degraded（中性极简），用户消息逐字不动；
//  3. 降级状态保持到次日 00:00 CST：窗口内后续请求**直达**降级提示词（不先撞 400）；
//  4. 只重试**一次**：第二次仍被拦 → 回 400，不再放大请求量；
//  5. 反例：请求级错误（参数错）**不**触发降级（降级是针对"system 指纹被误杀"的处置）。
//
// 断言全部看**上游实际收到的请求体**，不看本地变量 —— 只看本地变量会漏掉
// "改写了变量却没改写出站 body"这类静默缺陷（v0.31.2 踩过）。
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

	"workbuddy2api/internal/prompt"
)

// degradeStub 可编程假上游：按调用序返回「拦截 N 次后放行」，并记录每次出站请求体。
type degradeStub struct {
	mu       sync.Mutex
	bodies   []string
	blockFor int    // 前 blockFor 次调用返回拦截
	status   int    // 拦截时用哪个 HTTP 状态（400 内容拦截 / 403 渠道拒绝）
	body     string // 拦截时返回的 body
}

func (s *degradeStub) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bodies)
}

func (s *degradeStub) handler(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.bodies = append(s.bodies, string(raw))
	n := len(s.bodies)
	s.mu.Unlock()

	if n <= s.blockFor {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(s.status)
		_, _ = io.WriteString(w, s.body)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	frame, _ := json.Marshal(map[string]any{
		"id": "chatcmpl-degrade", "object": "chat.completion.chunk", "model": "glm-5.3",
		"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"content": "ok"}, "finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 2, "total_tokens": 7, "credit": 1},
	})
	_, _ = w.Write(append(append([]byte("data: "), frame...), []byte("\n\ndata: [DONE]\n\n")...))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// systemOf 取出站请求体里第一条 system 消息的内容（无则空串）。
func systemOf(t *testing.T, body string) string {
	t.Helper()
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("出站体不是 JSON: %v body=%s", err, body)
	}
	for _, m := range req.Messages {
		if m.Role == "system" {
			return m.Content
		}
	}
	return ""
}

// newDegradeApp 单账号 + 假上游 + 一条可用 Key 的端到端装配（选号确定性）。
func newDegradeApp(t *testing.T, stub *degradeStub) (*Store, *Proxy, *httptest.Server, string) {
	t.Helper()
	store, px, a := newTestApp(t, &Account{
		UID: "dg-acct-AAAA", Nickname: "dg", Realm: "cn",
		AccessToken: "tok", RefreshToken: "tok-r",
		ExpiresAt:    time.Now().Add(48 * time.Hour).Unix(),
		ConsumeGroup: "apply",
		PoolState:    "in",
	})
	up := httptest.NewServer(http.HandlerFunc(stub.handler))
	px.uc.HTTP = &http.Client{Transport: rewriteTransport{target: up.URL}}
	u, err := store.CreateUser("degrade-payer", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := store.AddUserBalance(u.ID, 1000*MicroPerPoint); err != nil {
		t.Fatalf("AddUserBalance: %v", err)
	}
	raw := "sk-wb-degrade-test-key"
	if _, err := store.CreateAPIKey(u.ID, "test", HashAPIKey(raw), "enc", "sk-wb", "", "", ""); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	px.ReloadSelection()
	px.pool.SetCredits("dg-acct-AAAA", 5000)
	px.pool.SetRandomSource(func(n int64) int64 { return 0 })
	srv := httptest.NewServer(a.apiKeyGuard(px.ServeChat))
	t.Cleanup(func() { srv.Close(); up.Close(); store.Close() })
	return store, px, srv, raw
}

// degradeCall 发一次对话请求（带客户端 system 模板，模拟 Claude Code / Codex 形态）。
func degradeCall(t *testing.T, srv *httptest.Server, key string) (*http.Response, string) {
	t.Helper()
	body := `{"model":"glm-5.3","stream":false,"messages":[` +
		`{"role":"system","content":"You are Claude Code, Anthropic's official CLI for Claude."},` +
		`{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

const blockedBody400 = `{"code":11133,"msg":"blocked by security policy","requestId":"r1"}`

// TestContentBlockedTriggersDegradedRetry 首遇内容拦截 → 降级重试一次 → 200，
// 且重试那次出站的 system 已是 Degraded 中性提示词。
func TestContentBlockedTriggersDegradedRetry(t *testing.T) {
	stub := &degradeStub{blockFor: 1, status: http.StatusBadRequest, body: blockedBody400}
	_, px, srv, key := newDegradeApp(t, stub)

	resp, _ := degradeCall(t, srv, key)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("code=%d want 200（降级重试后应成功）", resp.StatusCode)
	}
	if n := stub.calls(); n != 2 {
		t.Fatalf("上游调用应为 2 次（首次被拦 + 降级重试），got %d", n)
	}
	if got := systemOf(t, stub.bodies[0]); !strings.Contains(got, "Claude Code") {
		t.Errorf("首次出站应带客户端 system 原文，got %q", got)
	}
	if got := systemOf(t, stub.bodies[1]); got != prompt.Degraded {
		t.Errorf("降级重试的出站 system 必须是 Degraded 中性提示词，got %q", got)
	}
	// 用户消息逐字不动（降级只换 system，不改用户指令语义）。
	if !strings.Contains(stub.bodies[1], `"hi"`) {
		t.Errorf("降级重试不得改动用户消息：%s", stub.bodies[1])
	}
	// 内容拦截不是账号问题 → 不罚账号。
	st, _ := px.pool.Status("dg-acct-AAAA")
	if st.Disabled || !st.Until.IsZero() {
		t.Errorf("内容拦截不得处置账号：disabled=%v until=%v", st.Disabled, st.Until)
	}
	// 降级状态已进入窗口。
	if !px.degrade.Active() {
		t.Error("触发后应处于降级期")
	}
}

// TestDegradedWindowShortCircuits 降级窗口内后续请求**直达**降级提示词（不先撞 400）。
func TestDegradedWindowShortCircuits(t *testing.T) {
	stub := &degradeStub{blockFor: 1, status: http.StatusBadRequest, body: blockedBody400}
	_, px, srv, key := newDegradeApp(t, stub)

	if resp, _ := degradeCall(t, srv, key); resp.StatusCode != http.StatusOK {
		t.Fatalf("首次请求应成功（降级重试），code=%d", resp.StatusCode)
	}
	before := stub.calls()
	resp, _ := degradeCall(t, srv, key)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("降级期内的第二次请求应成功，code=%d", resp.StatusCode)
	}
	if n := stub.calls() - before; n != 1 {
		t.Fatalf("降级期内应**直达**（只 1 次上游调用），got %d", n)
	}
	if got := systemOf(t, stub.bodies[len(stub.bodies)-1]); got != prompt.Degraded {
		t.Errorf("直达请求的 system 应为 Degraded，got %q", got)
	}
	if !px.degrade.Active() {
		t.Error("降级窗口应保持到次日 00:00")
	}
}

// TestContentBlockedRetriesOnlyOnce 持续被拦 → 只重试一次，绝不放大请求量。
func TestContentBlockedRetriesOnlyOnce(t *testing.T) {
	stub := &degradeStub{blockFor: 99, status: http.StatusBadRequest, body: blockedBody400}
	_, _, srv, key := newDegradeApp(t, stub)

	resp, body := degradeCall(t, srv, key)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("code=%d want 400（第二次仍被拦 → 回请求级错误）", resp.StatusCode)
	}
	if n := stub.calls(); n != 2 {
		t.Fatalf("只允许降级重试一次：上游调用应为 2 次，got %d（放大请求量会加重风控）", n)
	}
	if !strings.Contains(body, "upstream unavailable") && !strings.Contains(body, "error") {
		t.Errorf("应回错误体，got %s", body)
	}
}

// TestChannelDeniedTriggersDegradedRetry 11128（渠道/指纹拒绝）走**同一条**降级通道。
//
// 二者在处置上完全同类：都是「上游按指纹拒绝」，而 Degraded 中性提示词正是为
// 「换掉可能触发检查的 system 内容」而存在。分开处置就会出现"同一个 11128，
// 只因为 code 有没有被识别，一条路重试一条路直接失败"。
func TestChannelDeniedTriggersDegradedRetry(t *testing.T) {
	stub := &degradeStub{
		blockFor: 1, status: http.StatusForbidden,
		body: `{"code":11128,"msg":"illegal api invocation"}`,
	}
	_, px, srv, key := newDegradeApp(t, stub)

	resp, _ := degradeCall(t, srv, key)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("code=%d want 200（11128 降级重试后应成功）", resp.StatusCode)
	}
	if n := stub.calls(); n != 2 {
		t.Fatalf("11128 也应触发一次降级重试，上游调用 got %d", n)
	}
	if got := systemOf(t, stub.bodies[1]); got != prompt.Degraded {
		t.Errorf("11128 降级重试的 system 应为 Degraded，got %q", got)
	}
	if !px.degrade.Active() {
		t.Error("11128 也应触发降级窗口")
	}
}

// TestBadParamsDoesNotDegrade 反例：请求级错误（参数错）**不**触发降级。
//
// 降级是给「system 指纹被误杀」用的处置；把它套到参数错、上下文超长这类错误上，
// 只会让同一个坏请求多打一次上游，还会把降级窗口无故打开（影响全站 system）。
func TestBadParamsDoesNotDegrade(t *testing.T) {
	stub := &degradeStub{
		blockFor: 99, status: http.StatusBadRequest,
		body: `{"code":11101,"msg":"Unmarshal chat params failed: invalid character"}`,
	}
	_, px, srv, key := newDegradeApp(t, stub)

	resp, _ := degradeCall(t, srv, key)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("code=%d want 400", resp.StatusCode)
	}
	if n := stub.calls(); n != 1 {
		t.Errorf("参数错不该重试，上游调用应为 1 次，got %d", n)
	}
	if px.degrade.Active() {
		t.Error("参数错不得打开降级窗口（会影响全站 system 提示词）")
	}
}

// TestPromptDegradeViewExposed 降级状态必须在可观测面里出现。
//
// 「进程级、影响全站请求」的状态如果只在日志里，运维无法回答"为什么回答风格变了"。
func TestPromptDegradeViewExposed(t *testing.T) {
	_, px, _, _ := newDegradeApp(t, &degradeStub{blockFor: 0, status: 400, body: blockedBody400})
	v := px.promptDegradeView()
	if v["degraded"] != false {
		t.Errorf("未降级时 degraded 应为 false，got %v", v["degraded"])
	}
	// 键名必须是 snake_case 的字符面（对外视图禁内嵌无 json tag 的结构体）。
	for _, k := range []string{"degraded", "triggers", "until"} {
		if _, ok := v[k]; !ok {
			t.Errorf("视图缺键 %q：%v", k, v)
		}
	}
	px.degrade.Trigger()
	v = px.promptDegradeView()
	if v["degraded"] != true {
		t.Errorf("触发后 degraded 应为 true，got %v", v["degraded"])
	}
	if u, _ := v["until"].(int64); u <= time.Now().Unix() {
		t.Errorf("until 应为未来时刻，got %v", v["until"])
	}
}
