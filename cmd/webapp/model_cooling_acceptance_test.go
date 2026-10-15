// 「账号 + 模型」维度冷却与路由的**需求验收测试**（v0.31.1）。
//
// 逐条对应需求原文：
//  1. 某账号对某模型返回 6004 → 进入**该模型维度**的冷却；
//  2. 冷却期内该模型的请求**不得**再路由到该账号，由其他可用账号承接；
//  3. 冷却**仅限该模型**：同账号对其他模型的请求照常路由；
//  4. 冷却按「账号 + 模型」独立记录与判断，不同模型互不影响；
//  5. 到期后恢复正常路由（惰性清理即恢复）。
//  6. （补充）上游把 6004 放在**已 200 的流内 error 帧**里时，同样进入冷却。
//
// 全部走 ServeChat + 真选号 + 真池状态，不是只测函数。
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/pool"
)

// mcStub 按**模型**维度可编程的假上游：
//   - fail429[m] 前 n 次调用返回 HTTP 429 + {"code":6004,...}（上游最常见的 6004 形态）
//   - failInStream[m] 前 n 次调用返回 HTTP 200 + 流内 error 帧（code 6004）
//   - 其余一律 200 + 正常 SSE
type mcStub struct {
	mu           sync.Mutex
	calls        map[string]int
	fail429      map[string]int
	failInStream map[string]int
	body         string // 6004 的 JSON 体（含 resets_at）
}

func (s *mcStub) callCount(model string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[model]
}

func (s *mcStub) handler(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var req map[string]any
	_ = json.Unmarshal(raw, &req)
	model, _ := req["model"].(string)

	s.mu.Lock()
	s.calls[model]++
	n := s.calls[model]
	s.mu.Unlock()

	if s.fail429[model] >= n {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, s.body)
		return
	}
	if s.failInStream[model] >= n {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// 上游把错误放在**已经 200 的流里**：客户端看得见，池侧必须也能"看见"。
		frame := map[string]any{"error": map[string]any{
			"code": 6004, "message": "model rate limited",
		}}
		if v, ok := s.jsonResetAt(); ok {
			frame["error"].(map[string]any)["resets_at"] = v
		}
		b, _ := json.Marshal(frame)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	frame := map[string]any{
		"id": "chatcmpl-cooling", "object": "chat.completion.chunk", "model": model,
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

func (s *mcStub) jsonResetAt() (string, bool) {
	var obj map[string]any
	if json.Unmarshal([]byte(s.body), &obj) != nil {
		return "", false
	}
	v, ok := obj["resets_at"].(string)
	return v, ok
}

const (
	coolUIDA = "cool-acct-AAAA"
	coolUIDB = "cool-acct-BBBB"
)

// newCoolingApp 双账号 + 假上游 + 一个计费 Key 的端到端装配。
func newCoolingApp(t *testing.T, stub *mcStub) (*Store, *Proxy, *httptest.Server, string) {
	t.Helper()
	mk := func(uid, nick, tok string) *Account {
		return &Account{
			UID: uid, Nickname: nick, Realm: "cn",
			AccessToken: tok, RefreshToken: tok + "-r",
			ExpiresAt:    time.Now().Add(48 * time.Hour).Unix(),
			ConsumeGroup: "apply", // 平台选号只认「参与消耗」的分组
			PoolState:    "in",
		}
	}
	store, px, a := newTestApp(t, mk(coolUIDA, "acct-a", "tok-a"), mk(coolUIDB, "acct-b", "tok-b"))
	up := httptest.NewServer(http.HandlerFunc(stub.handler))
	px.uc.HTTP = &http.Client{Transport: rewriteTransport{target: up.URL}}

	u, err := store.CreateUser("cooling-payer", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := store.AddUserBalance(u.ID, 1000*MicroPerPoint); err != nil {
		t.Fatalf("AddUserBalance: %v", err)
	}
	raw := "sk-wb-cooling-test-key"
	if _, err := store.CreateAPIKey(u.ID, "test", HashAPIKey(raw), "enc", "sk-wb", "", "", ""); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	px.ReloadSelection()
	px.pool.SetCredits(coolUIDA, 5000)
	px.pool.SetCredits(coolUIDB, 5000)
	// 选号确定性（否则"这次是谁答的"随机，断言无法固定）。
	px.pool.SetRandomSource(func(n int64) int64 { return 0 })

	srv := httptest.NewServer(a.apiKeyGuard(px.ServeChat))
	t.Cleanup(func() { srv.Close(); up.Close(); store.Close() })
	return store, px, srv, raw
}

// newStub 造一个 6004 的 JSON 体（含上游重置墙钟 2 小时后）。
func newStub() (*mcStub, time.Time) {
	resetAt := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	body := fmt.Sprintf(`{"code":6004,"message":"model rate limited","resets_at":%q}`,
		resetAt.Format(time.RFC3339))
	return &mcStub{
		calls: map[string]int{}, fail429: map[string]int{},
		failInStream: map[string]int{}, body: body,
	}, resetAt
}

// modelCoolingOf 该账号在台账里对这个模型的冷却条目（无则 false）。
func modelCoolingOf(t *testing.T, px *Proxy, uid, model string) (pool.RateLimitedModel, bool) {
	t.Helper()
	st, ok := px.pool.Status(uid)
	if !ok {
		t.Fatalf("账号 %s 不在池内", uid)
	}
	for _, m := range st.RateLimitedModels {
		if m.Model == model {
			return m, true
		}
	}
	return pool.RateLimitedModel{}, false
}

// TestCooling6004RoutesToOtherAccountAndCoolsOnlyThatModel 需求 1+2+3。
func TestCooling6004RoutesToOtherAccountAndCoolsOnlyThatModel(t *testing.T) {
	stub, resetAt := newStub()
	stub.fail429["glm-cool"] = 1 // 该模型第一次调用返回 429+6004
	_, px, srv, raw := newCoolingApp(t, stub)

	// ① 第一次请求：A（或 B）撞 6004 → 换号 → 另一个号承接。
	resp := doChat(t, srv, raw, `{"model":"glm-cool","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("换号后应成功，got %d", resp.StatusCode)
	}
	served := resp.Header.Get("X-Wb-Account")
	if served != coolUIDA && served != coolUIDB {
		t.Fatalf("响应应透出服务账号，got %q", served)
	}
	if got := stub.callCount("glm-cool"); got != 2 {
		t.Fatalf("第一次请求应产生 2 次上游调用（撞 6004 后换号），got %d", got)
	}
	failedUID := coolUIDB
	if served == coolUIDB {
		failedUID = coolUIDA
	}

	// ② 撞 6004 的账号对该模型进入冷却，且**时长以上游重置墙钟为准**。
	mc, ok := modelCoolingOf(t, px, failedUID, "glm-cool")
	if !ok {
		t.Fatalf("账号 %s 的 glm-cool 应进入冷却", failedUID)
	}
	if d := mc.Until.Sub(resetAt); d < -2*time.Second || d > 2*time.Second {
		t.Errorf("冷却 Until=%v 应对齐上游重置 %v（差异 %v）", mc.Until, resetAt, d)
	}
	if !strings.Contains(mc.Reason, "6004") {
		t.Errorf("冷却原因应可读出是 6004，got %q", mc.Reason)
	}

	// ③ 冷却期内该模型**不可**路由到该账号；其他模型仍可（需求 3）。
	if got := px.pool.PickByUIDForModel(failedUID, "glm-cool"); got != nil {
		t.Errorf("冷却期内 glm-cool 不应选中 %s", failedUID)
	}
	if got := px.pool.PickByUIDForModel(failedUID, "glm-other"); got == nil {
		t.Errorf("其他模型应仍可选中 %s（冷却只限该模型）", failedUID)
	}

	// ④ 第二次同模型请求：只应打在另一个账号上（上游调用数 +1，绝不重试被冷却的号）。
	before := stub.callCount("glm-cool")
	resp2 := doChat(t, srv, raw, `{"model":"glm-cool","messages":[{"role":"user","content":"hi"}]}`)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("第二次请求应成功，got %d", resp2.StatusCode)
	}
	if got := resp2.Header.Get("X-Wb-Account"); got != served {
		t.Errorf("第二次应仍由健康账号承接，got %q want %q", got, served)
	}
	if got := stub.callCount("glm-cool") - before; got != 1 {
		t.Errorf("第二次只应有 1 次上游调用（没打到被冷却的号），got %d", got)
	}
	if _, ok := modelCoolingOf(t, px, failedUID, "glm-cool"); !ok {
		t.Error("第二次请求后冷却应仍在（未被健康号的成功清掉）")
	}
}

// TestCoolingModelsAreIndependentPerAccountAndModel 需求 4：不同模型互不影响。
func TestCoolingModelsAreIndependentPerAccountAndModel(t *testing.T) {
	stub, resetAt := newStub()
	_, px, _, _ := newCoolingApp(t, stub)

	px.pool.CooldownSoftForModel(coolUIDA, time.Minute, resetAt, "m-1", "6004 model rate limit")
	px.pool.CooldownSoftForModel(coolUIDA, time.Minute, resetAt, "m-2", "6004 model rate limit")
	px.pool.CooldownSoftForModel(coolUIDB, time.Minute, resetAt, "m-1", "6004 model rate limit")

	for _, c := range []struct {
		uid, model string
		want       bool
	}{
		{coolUIDA, "m-1", false}, {coolUIDA, "m-2", false}, {coolUIDA, "m-3", true},
		{coolUIDB, "m-1", false}, {coolUIDB, "m-2", true}, {coolUIDB, "m-3", true},
	} {
		got := px.pool.PickByUIDForModel(c.uid, c.model) != nil
		if got != c.want {
			t.Errorf("(%s, %s) 可选=%v want %v —— 冷却必须按 (账号,模型) 独立判定", c.uid, c.model, got, c.want)
		}
	}
	// 两个模型同时在冷却：各自保留（不是"后写的覆盖先写的"）。
	st, _ := px.pool.Status(coolUIDA)
	if len(st.RateLimitedModels) != 2 {
		t.Errorf("A 应同时保有 2 条模型冷却，got %d", len(st.RateLimitedModels))
	}
}

// TestCoolingExpiryReleasesAccount 需求 5：到期即恢复（惰性清理）。
//
// 注意这里的时钟依赖是**刻意的**：`CooldownSoftForModel` 拿到一个已过期的 resetAt 时，
// `cappedUntilLocked` 会把 Until 收敛成 `now+1ms`（不是直接写零值）—— 所以"到期即恢复"
// 的语义是"1ms 后恢复"，测试必须让这 1ms 过去再断言（别用零值断言，那会假通过）。
func TestCoolingExpiryReleasesAccount(t *testing.T) {
	stub, _ := newStub()
	_, px, _, _ := newCoolingApp(t, stub)

	// 上游给的时刻：一个已过去（等价于"限流窗口已结束"）、一个仍在未来。
	px.pool.CooldownSoftForModel(coolUIDA, time.Minute, time.Now().Add(-time.Minute), "m-expired", "6004")
	px.pool.CooldownSoftForModel(coolUIDA, time.Minute, time.Now().Add(time.Hour), "m-live", "6004")

	time.Sleep(20 * time.Millisecond) // 让 now+1ms 的收敛截止过去（见函数注释）

	if got := px.pool.PickByUIDForModel(coolUIDA, "m-expired"); got == nil {
		t.Error("已到期的模型冷却应立刻放行该账号")
	}
	if got := px.pool.PickByUIDForModel(coolUIDA, "m-live"); got != nil {
		t.Error("未到期的模型冷却不应放行")
	}
	// 走一次真选号（pick 的写锁路径）→ 过期条目被**真正删除**（不是每次遍历都重判过期）。
	// 断言"池里选得出号"而不是"一定选中 A"：两个号都能服务这个模型，选中谁是加权随机的
	// （断言具体 uid 会把测试绑在抽签结果上，与本需求无关）。
	if au := px.pool.PickExcludingForRealm(nil, "m-expired", ""); au == nil {
		t.Error("到期后该模型应能选出可用账号（A 的冷却不该再拦住它）")
	}
	st, _ := px.pool.Status(coolUIDA)
	for _, m := range st.RateLimitedModels {
		if m.Model == "m-expired" {
			t.Error("已过期的条目应在 pick 写锁路径被真正删除（否则内存表只增不减）")
		}
	}
}

// TestCoolingFromInStream6004Frame 需求补充：上游把 6004 放在已 200 的流内 error 帧里。
//
// 修复前：错误帧被原样透传给客户端，**池侧完全不知道** → 该 (账号,模型) 不冷却，
// 下一个同模型请求照样被路由到它。修复后与 4xx 形态共用同一套冷却原语。
func TestCoolingFromInStream6004Frame(t *testing.T) {
	stub, resetAt := newStub()
	stub.failInStream["glm-stream"] = 1
	_, px, srv, raw := newCoolingApp(t, stub)

	resp := doChat(t, srv, raw, `{"model":"glm-stream","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	served := resp.Header.Get("X-Wb-Account")
	if served != coolUIDA && served != coolUIDB {
		t.Fatalf("响应应透出服务账号，got %q", served)
	}
	if got := stub.callCount("glm-stream"); got != 1 {
		t.Fatalf("流内错误帧不触发换号（响应已开始），应只有 1 次上游调用，got %d", got)
	}

	mc, ok := modelCoolingOf(t, px, served, "glm-stream")
	if !ok {
		t.Fatalf("流内 6004 错误帧必须让 (%s, glm-stream) 进入冷却", served)
	}
	if d := mc.Until.Sub(resetAt); d < -2*time.Second || d > 2*time.Second {
		t.Errorf("流内形态的冷却时长同样应对齐上游重置 %v，got %v", resetAt, mc.Until)
	}

	// 端到端：紧接的第二次同模型请求不得再路由到该账号。
	other := coolUIDB
	if served == coolUIDB {
		other = coolUIDA
	}
	before := stub.callCount("glm-stream")
	resp2 := doChat(t, srv, raw, `{"model":"glm-stream","messages":[{"role":"user","content":"hi"}]}`)
	if got := resp2.Header.Get("X-Wb-Account"); got != other {
		t.Errorf("第二次应由另一个账号承接，got %q want %q", got, other)
	}
	if got := stub.callCount("glm-stream") - before; got != 1 {
		t.Errorf("第二次只应有 1 次上游调用，got %d", got)
	}
}
