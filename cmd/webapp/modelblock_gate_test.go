// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 「模型名在上游不存在」（11102）的**跨账号 fail-fast** 验收测试（v0.31.16）。
//
// 起因是一次真实事故：测速时用 `{"model":"x"}`（不存在的模型名）打了约 11 次
// 请求，因 maxAttempts=3，**32/33 个账号**被写入 6h 的 (账号,"x") 负缓存。
// 也就是说 —— **一个假模型名的请求会被放大成"犁全池"**；客户端自带重试、或
// 爬虫随手发随机模型名，可以直接把它放大成出站风暴。
//
// 本文件走 ServeChat + 真选号 + 真池状态，逐条钉住修复后的行为：
//  1. 单号 11102 **不**激活闸门（号码不够，不能凭一个号断定模型不存在）；
//  2. 两个**不同号**接连 11102 → 闸门激活，**当轮立即停止换号**（不打第 3 个号）；
//  3. 激活期内后续请求**零出站**直接拒绝（上游调用数不再增长）；
//  4. 闸门按**模型名分桶**：同池的另一个正常模型完全不受影响；
//  5. 拒绝用确定性 404（重发无用的错误不许用可重试状态码）。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// mbStub 可编程假上游：ghost 模型一律 400 + 11102，其余模型正常 200 + SSE。
// 计数按**总数**（断言出站次数）与**按模型**（断言隔离性）两个维度。
type mbStub struct {
	mu      sync.Mutex
	total   int
	byModel map[string]int
	ghost   string
}

func (s *mbStub) counts() (int, map[string]int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := map[string]int{}
	for k, v := range s.byModel {
		m[k] = v
	}
	return s.total, m
}

func (s *mbStub) modelCalls(model string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byModel[model]
}

func (s *mbStub) handler(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var req map[string]any
	_ = json.Unmarshal(raw, &req)
	model, _ := req["model"].(string)

	s.mu.Lock()
	s.total++
	if s.byModel == nil {
		s.byModel = map[string]int{}
	}
	s.byModel[model]++
	s.mu.Unlock()

	if model == s.ghost {
		// 上游对"不存在的模型名"的真实答复形态（实测原文）。
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"code":11102,"msg":"当前模型不可用，请切换模型"}`)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	frame := map[string]any{
		"id": "chatcmpl-mb", "object": "chat.completion.chunk", "model": model,
		"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"content": "hi"}, "finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15, "credit": 1,
		},
	}
	b, _ := json.Marshal(frame)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// mbNewApp 装配 n 个健康账号 + 假上游 + 一个计费 Key。
func mbNewApp(t *testing.T, stub *mbStub, n int) (*Store, *Proxy, *httptest.Server, string) {
	t.Helper()
	accts := make([]*Account, 0, n)
	for i := 0; i < n; i++ {
		uid := fmt.Sprintf("mb-acct-%04d", i)
		accts = append(accts, &Account{
			UID: uid, Nickname: uid, Realm: "cn",
			AccessToken: "tok-" + uid, RefreshToken: "rtok-" + uid,
			ExpiresAt:    time.Now().Add(48 * time.Hour).Unix(),
			ConsumeGroup: "apply", // 平台选号只认「参与消耗」的分组
			PoolState:    "in",
		})
	}
	store, px, a := newTestApp(t, accts...)
	up := httptest.NewServer(http.HandlerFunc(stub.handler))
	px.uc.HTTP = &http.Client{Transport: rewriteTransport{target: up.URL}}

	u, err := store.CreateUser("mb-payer", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := store.AddUserBalance(u.ID, 1000*MicroPerPoint); err != nil {
		t.Fatalf("AddUserBalance: %v", err)
	}
	raw := "sk-wb-modelblock-test-key"
	if _, err := store.CreateAPIKey(u.ID, "test", HashAPIKey(raw), "enc", "sk-wb", "", "", ""); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	px.ReloadSelection()
	for i := 0; i < n; i++ {
		px.pool.SetCredits(fmt.Sprintf("mb-acct-%04d", i), 5000)
	}
	// 选号确定性（否则"这次是谁答的"随机，出站次数断言无法稳定）。
	px.pool.SetRandomSource(func(int64) int64 { return 0 })

	srv := httptest.NewServer(a.apiKeyGuard(px.ServeChat))
	t.Cleanup(func() { srv.Close(); up.Close(); store.Close() })
	return store, px, srv, raw
}

func mbChat(t *testing.T, srv *httptest.Server, key, model string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model":    model,
		"stream":   false,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// TestModelBlockGateStopsRotationAfterTwoAccounts 两个不同号接连 11102 →
// **当轮立即停止换号**（不再打第 3 个号）。这是本刀最核心的断言：
// 修复前是 3 次出站（maxAttempts），修复后是 2 次。
func TestModelBlockGateStopsRotationAfterTwoAccounts(t *testing.T) {
	stub := &mbStub{ghost: "x"}
	_, px, srv, key := mbNewApp(t, stub, 4)

	code, body := mbChat(t, srv, key, "x")
	if code < 400 {
		t.Fatalf("不存在的模型必须回 4xx，得 %d body=%.200s", code, body)
	}
	if total, byModel := stub.counts(); total != 2 {
		t.Fatalf("两个不同号报 11102 后应立即停止换号：期望 2 次出站，实得 %d（byModel=%v）", total, byModel)
	}
	if !px.modelGate.Active("x") {
		t.Fatal("两个不同号接连 11102 之后闸门应已激活")
	}
}

// TestModelBlockGateActiveWindowZeroEgress 激活期内后续请求**零出站**。
// 修复前：池里还有别的号，每个请求都会继续撞 1~3 个新号（实测事故里被犁掉
// 32/33 个账号）；修复后上游调用数**一点不涨**。
func TestModelBlockGateActiveWindowZeroEgress(t *testing.T) {
	stub := &mbStub{ghost: "x"}
	_, px, srv, key := mbNewApp(t, stub, 4)

	if code, _ := mbChat(t, srv, key, "x"); code < 400 {
		t.Fatalf("首次请求应失败，得 %d", code)
	}
	before, _ := stub.counts()

	for i := 0; i < 3; i++ {
		code, body := mbChat(t, srv, key, "x")
		if code != http.StatusNotFound {
			t.Fatalf("激活期内应回确定性 404（重发无用），第 %d 次得 %d body=%.200s", i+1, code, body)
		}
		if !strings.Contains(body, "does not exist") {
			t.Fatalf("拒绝文案应说明「模型名不存在」，得 %.200s", body)
		}
	}
	if after, byModel := stub.counts(); after != before {
		t.Fatalf("激活期内必须零出站：期望仍为 %d，实得 %d（byModel=%v）", before, after, byModel)
	}
	if !px.modelGate.Active("x") {
		t.Fatal("闸门应仍在激活期")
	}
}

// TestModelBlockGateSingleAccountDoesNotActivate 号码不够时不许下结论：
// 池里只有一个号 → 单号 11102 只写它自己的负缓存，**不激活闸门**
// （凭一个号断定"模型不存在"会把"该号没有此模型"误判成"模型不存在"）。
func TestModelBlockGateSingleAccountDoesNotActivate(t *testing.T) {
	stub := &mbStub{ghost: "x"}
	_, px, srv, key := mbNewApp(t, stub, 1)

	if code, _ := mbChat(t, srv, key, "x"); code < 400 {
		t.Fatalf("单号也应付不出结果，得 %d", code)
	}
	if total, _ := stub.counts(); total != 1 {
		t.Fatalf("池里只有一个号时应只出站 1 次，实得 %d", total)
	}
	if px.modelGate.Active("x") {
		t.Fatal("单个账号的 11102 不得激活闸门（那是账号维度信息，不足以断定模型不存在）")
	}
	// 且闸门未激活 → 第二个请求仍会正常走**选号**（不是被快速失败挡掉的）。
	// 池里唯一的号已被 (账号,x) 负缓存 → 选不出号 → 503，而不是闸门的 404。
	// 这个区分正是本文件要的：两条失败路径**必须可分辨**，否则运维看到 4xx/5xx
	// 无法判断"是模型名不存在"还是"这个模型的号都用光了"。
	code2, _ := mbChat(t, srv, key, "x")
	if code2 != http.StatusServiceUnavailable {
		t.Fatalf("闸门未激活时不该走快速失败分支（应为选号失败的 503），得 %d", code2)
	}
	if total2, _ := stub.counts(); total2 != 1 {
		t.Fatalf("被负缓存挡住时不应新增出站：期望仍 1 次，实得 %d", total2)
	}
}

// TestModelBlockGateDoesNotAffectOtherModels 分桶隔离：把 `x` 打成"不存在"
// 之后，另一个正常模型必须**照常成功** —— 这是本闸门与全局单例（WAFGate）
// 最本质的区别，也是它敢放在 ServeChat 入口做快速失败的前提。
func TestModelBlockGateDoesNotAffectOtherModels(t *testing.T) {
	stub := &mbStub{ghost: "x"}
	_, px, srv, key := mbNewApp(t, stub, 4)

	mbChat(t, srv, key, "x") // 触发激活
	if !px.modelGate.Active("x") {
		t.Fatal("闸门应激活")
	}
	beforeOther := stub.modelCalls("kimi-k3-1")

	code, body := mbChat(t, srv, key, "kimi-k3-1")
	if code != http.StatusOK {
		t.Fatalf("另一个模型必须不受影响，得 %d body=%.200s", code, body)
	}
	if stub.modelCalls("kimi-k3-1") <= beforeOther {
		t.Fatal("另一个模型应真的出站调用过（不能是靠缓存/短路通过）")
	}
	if px.modelGate.Active("kimi-k3-1") {
		t.Fatal("别的模型不得被连带激活")
	}
}
