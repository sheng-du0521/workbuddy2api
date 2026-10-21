// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ======================================================================
// 第三方 / 中转站通道的回归网（v0.31.68，需求 P2）
//
// 这批用例守三件事：
//  1. **分发**：号池认识这个模型 → 绝不走通道；号池不认识 → 才走通道；
//  2. **计费**：通道按 token × 配置单价折算成积分，再走同一个倍率公式；
//  3. **可见性**：停用通道 → 模型立刻从 /v1/models 消失。
//
// 第 1 条是最危险的：写错一次就是"号池流量全被推到第三方账单上"，
// 而且运行时看起来一切正常（客户端照样有回答）。
// ======================================================================

// TestValidateUpstreamURL 通道地址校验：必须挡住 SSRF。
//
// 这个校验是**服务端去请求**这个地址之前的唯一一道门。放行 127.0.0.1 或者
// 169.254.169.254 就等于给后台加了一个"内网探测 / 读云元数据"的功能。
func TestValidateUpstreamURL(t *testing.T) {
	bad := []string{
		"", "   ", "ftp://example.com", "http://127.0.0.1:8080",
		"http://localhost:8780", "http://10.0.0.5", "http://192.168.1.1",
		"http://172.16.0.1", "http://169.254.169.254/latest/meta-data",
		"http://[::1]:80", "http://0.0.0.0", "example.com", // 缺 scheme
	}
	for _, raw := range bad {
		if got, err := validateUpstreamURL(raw); err == nil {
			t.Errorf("应拒绝 %q，却通过了（归一化为 %q）", raw, got)
		}
	}
	good := map[string]string{
		"https://api.example.com/v1":    "https://api.example.com/v1",
		"https://api.example.com/v1/":   "https://api.example.com/v1",
		"http://relay.example.com:9000": "http://relay.example.com:9000",
	}
	for raw, want := range good {
		got, err := validateUpstreamURL(raw)
		if err != nil {
			t.Errorf("应接受 %q：%v", raw, err)
			continue
		}
		if got != want {
			t.Errorf("归一化 %q → %q，期望 %q", raw, got, want)
		}
	}
}

// TestUpstreamSaveMasksKey 保存后读回**不能有明文 key**。
//
// 后台是一把"能看到全部钱"的钥匙：它的会话被拿走时，明文第三方 key 就是直接损失。
// 而前端根本不需要明文（探活由服务端做）。
func TestUpstreamSaveMasksKey(t *testing.T) {
	store, _ := newHostProxy(t)
	defer store.Close()

	secret := "sk-relay-super-secret-value"
	id, err := store.SaveUpstream(&Upstream{
		Name: "relay-a", BaseURL: "https://relay.example.com/v1", APIKey: secret, Enabled: true,
	})
	if err != nil {
		t.Fatalf("SaveUpstream: %v", err)
	}
	list, err := store.ListUpstreams()
	if err != nil {
		t.Fatalf("ListUpstreams: %v", err)
	}
	if len(list) != 1 || list[0].ID != id {
		t.Fatalf("读回条数不对：%+v", list)
	}
	if list[0].KeyMasked == "" || strings.Contains(list[0].KeyMasked, "super-secret") {
		t.Fatalf("掩码不合规：%q", list[0].KeyMasked)
	}
	// 更新时不传 key = 不改（前端拿不到明文，也不可能回传它）。
	if _, err := store.SaveUpstream(&Upstream{
		ID: id, Name: "relay-a", BaseURL: "https://relay.example.com/v1", Enabled: false,
	}); err != nil {
		t.Fatalf("更新：%v", err)
	}
	var kept string
	if err := store.db.QueryRow(`SELECT api_key FROM upstreams WHERE id = ?`, id).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != secret {
		t.Fatalf("未传 key 时不应覆盖，得 %q", kept)
	}
	// 重名要挡住：否则后台会出现两条同名通道，"我改的是哪条"无法回答。
	if _, err := store.SaveUpstream(&Upstream{
		Name: "relay-a", BaseURL: "https://other.example.com", APIKey: "k", Enabled: true,
	}); err == nil {
		t.Fatal("重名通道应被拒绝")
	}
}

// fakeRelay 一个最小的 OpenAI 兼容中转站：/models + /chat/completions。
//
// hits 记录 /chat/completions 被调用次数 —— 用来证明"号池优先"时它**一次都没被调用**。
func fakeRelay(t *testing.T, promptTok, compTok int, status int, streamReply bool) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/models"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"object": "list",
				"data":   []map[string]any{{"id": "relay-only-model"}},
			})
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			atomic.AddInt32(&hits, 1)
			if status >= 400 {
				w.WriteHeader(status)
				fmt.Fprint(w, `{"error":{"message":"relay boom"}}`)
				return
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			// 断言我们确实要了 usage（否则结算拿不到 token，只能 0 费）。
			wantStream, _ := body["stream"].(bool)
			if streamReply && wantStream {
				if _, ok := body["stream_options"]; !ok {
					t.Errorf("流式请求必须带 stream_options.include_usage，实际没有")
				}
			}
			usage := map[string]any{
				"prompt_tokens": promptTok, "completion_tokens": compTok,
				"total_tokens": promptTok + compTok,
			}
			if streamReply && wantStream {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				chunk := map[string]any{
					"id": "chunk-1", "object": "chat.completion.chunk", "model": "relay-only-model",
					"choices": []any{map[string]any{
						"index": 0, "delta": map[string]any{"content": "hello from relay"}, "finish_reason": "stop",
					}},
					"usage": usage,
				}
				b, _ := json.Marshal(chunk)
				fmt.Fprintf(w, "data: %s\n\n", b)
				fmt.Fprint(w, "data: [DONE]\n\n")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "cmpl-1", "object": "chat.completion", "model": "relay-only-model",
				"choices": []any{map[string]any{
					"index": 0, "message": map[string]any{"role": "assistant", "content": "hello from relay"},
					"finish_reason": "stop",
				}},
				"usage": usage,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return srv, &hits
}

// seedRelay 直接写库造一条通道（**绕过** URL 校验：httptest 的地址是 127.0.0.1，
// 而校验刻意拒绝回环。校验本身由 TestValidateUpstreamURL 单独覆盖）。
func seedRelay(t *testing.T, store *Store, baseURL, model string, creditPer1k float64, enabled bool) (int64, int64) {
	t.Helper()
	return seedRelayNamed(t, store, "relay-test-"+model, baseURL, model, creditPer1k, enabled)
}

// seedRelayNamed 同 seedRelay，但通道名可指定（upstreams.name 有唯一索引，
// 要测"同一模型挂在多条通道上"就必须能起不同名字）。
func seedRelayNamed(t *testing.T, store *Store, name, baseURL, model string, creditPer1k float64, enabled bool) (int64, int64) {
	t.Helper()
	now := time.Now().Unix()
	en := boolToInt(enabled)
	res, err := store.db.Exec(`INSERT INTO upstreams
		(name, kind, base_url, api_key, enabled, priority, note, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?)`, name, "openai", baseURL, "sk-relay-test", en, 10, "", now, now)
	if err != nil {
		t.Fatalf("插入通道：%v", err)
	}
	uid, _ := res.LastInsertId()
	res2, err := store.db.Exec(`INSERT INTO upstream_models
		(upstream_id, model, remote_model, credit_per_1k, enabled, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?)`, uid, model, "", creditPer1k, en, now, now)
	if err != nil {
		t.Fatalf("插入模型映射：%v", err)
	}
	mid, _ := res2.LastInsertId()
	return uid, mid
}

// seedPoolModels 伪造"号池模型目录"（modelsCache）。
//
// 为什么要伪造：这一层在真实运行时来自上游 /models。用例要精确控制
// "号池认识 A、不认识 B"，用真上游做不到，而这两个分支正是要守的东西。
func seedPoolModels(ids []string) {
	modelsCache.mu.Lock()
	defer modelsCache.mu.Unlock()
	data := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		data = append(data, map[string]any{"id": id, "object": "model", "owned_by": "workbuddy"})
	}
	payload, _ := json.Marshal(map[string]any{"object": "list", "data": data})
	modelsCache.at = time.Now()
	modelsCache.ids = ids
	modelsCache.payload = payload
}

// newRelayChatApp 装配一个可跑 ServeChat 的最小 app（可选账号），带一个计费用户与 Key。
func newRelayChatApp(t *testing.T, withAccount bool) (*Store, *Proxy, *app, *httptest.Server) {
	t.Helper()
	var accts []*Account
	if withAccount {
		accts = append(accts, &Account{
			UID: "relay-pool-uid", Nickname: "pool-acct", Realm: "cn",
			AccessToken: "tok", RefreshToken: "rtok",
			ExpiresAt:    time.Now().Add(48 * time.Hour).Unix(),
			ConsumeGroup: "apply", PoolState: "in",
		})
	}
	store, px, a := newTestApp(t, accts...)
	u, err := store.CreateUser("relay-payer", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := store.AddUserBalance(u.ID, 1000*MicroPerPoint); err != nil {
		t.Fatalf("AddUserBalance: %v", err)
	}
	raw := "sk-wb-relay-test-key"
	if _, err := store.CreateAPIKey(u.ID, "test", HashAPIKey(raw), "enc", "sk-wb", "", "", ""); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	px.ReloadSelection()
	srv := httptest.NewServer(a.apiKeyGuard(px.ServeChat))
	return store, px, a, srv
}

// chatOnce 打一次对话并读完响应体（流式路径必须读完，否则断言是竞态）。
func chatOnce(t *testing.T, srv *httptest.Server, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-wb-relay-test-key")
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	blob, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, string(blob)
}

// R1：号池**不认识**这个模型 → 走通道，并按 token × 单价 × 倍率扣钱。
//
// 这是"分发 + 计费"的核心用例：两个断言都必须成立 —— 只有"转发通了"而钱算错，
// 比不通更糟（用户白用 / 平台倒贴）。
func TestRelayServesModelPoolDoesNotKnow(t *testing.T) {
	store, px, _, srv := newRelayChatApp(t, true)
	defer store.Close()
	defer srv.Close()

	up, hits := fakeRelay(t, 100, 200, 200, false)
	defer up.Close()
	// 2 积分 / 1k token → 300 token = 0.6 积分；全局倍率 1 → 600 微点。
	seedRelay(t, store, up.URL, "relay-only-model", 2.0, true)
	seedPoolModels([]string{"pool-known-model"}) // 号池目录里**没有** relay-only-model

	before := balanceOf(t, store)
	resp, body := chatOnce(t, srv, `{"model":"relay-only-model","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("应 200，得 %d：%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "hello from relay") {
		t.Fatalf("响应体应来自通道：%s", body)
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Fatalf("通道应被调用 1 次，实际 %d", n)
	}
	// 响应头透出通道名（排障时"这次是谁在答"必须一眼可见）。
	if got := resp.Header.Get("X-Wb-Upstream"); !strings.Contains(got, "relay-test") {
		t.Fatalf("应带 X-Wb-Upstream，得 %q", got)
	}
	charged := before - balanceOf(t, store)
	if charged != 600 {
		t.Fatalf("应扣 600 微点（0.6 积分 × 1 倍），实际 %d", charged)
	}
	// 流水必须是 scope=relay：混进 pool 会让"两条供给的毛利"永远查不清。
	var scope string
	var credit float64
	if err := store.db.QueryRow(`SELECT scope, upstream_credit FROM usage_logs
		WHERE model = ? ORDER BY id DESC LIMIT 1`, "relay-only-model").Scan(&scope, &credit); err != nil {
		t.Fatalf("查流水：%v", err)
	}
	if scope != "relay" {
		t.Fatalf("流水 scope 应为 relay，得 %q", scope)
	}
	if credit < 0.59 || credit > 0.61 {
		t.Fatalf("折算积分应约 0.6，得 %v", credit)
	}
	_ = px
}

// R2：号池**认识**这个模型 → 一次都不能打通道（钱花在第三方是真实成本）。
func TestRelaySkippedWhenPoolKnowsModel(t *testing.T) {
	store, _, _, srv := newRelayChatApp(t, false)
	defer store.Close()
	defer srv.Close()

	up, hits := fakeRelay(t, 10, 10, 200, false)
	defer up.Close()
	seedRelay(t, store, up.URL, "shared-model", 1.0, true)
	seedPoolModels([]string{"shared-model"}) // 号池也有这个名字 → 号池优先

	resp, body := chatOnce(t, srv, `{"model":"shared-model","messages":[{"role":"user","content":"hi"}]}`)
	if n := atomic.LoadInt32(hits); n != 0 {
		t.Fatalf("号池认识该模型时不得调用通道，实际调用 %d 次", n)
	}
	// 没有账号可用 → 走到号池的"无可用账号"失败分支（不是通道的 200）。
	if resp.StatusCode == 200 {
		t.Fatalf("不应由通道应答：%s", body)
	}
}

// R3：通道全部失败 → 502，且错误信息点明"是通道的问题"，不是"模型不存在"。
//
// 回 404 会把运维引到"去查模型名"的错误方向（模型明明配置过）。
func TestRelayAllChannelsFail(t *testing.T) {
	store, _, _, srv := newRelayChatApp(t, false)
	defer store.Close()
	defer srv.Close()

	up, _ := fakeRelay(t, 0, 0, 500, false)
	defer up.Close()
	seedRelay(t, store, up.URL, "broken-model", 1.0, true)
	seedPoolModels([]string{"pool-known-model"})

	resp, body := chatOnce(t, srv, `{"model":"broken-model","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("应 502，得 %d：%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "channel") {
		t.Fatalf("错误信息应点明通道：%s", body)
	}
}

// TestRelayFailureVerdict 通道全失败时的状态码与解释（v0.31.95）。
//
// 以前一律 502 + "check the channel config"：把**上游限流**和**模型不支持该端点**
// 都说成"你配置错了"。2026-10-21 真实踩到：图像模型（gpt-image-*）挂在通道组上，
// 用户用对话接口请求它 → 上游 400 → 用户看到 502「检查通道配置」，
// 于是去反复核对 base_url / key —— 方向完全错了。
func TestRelayFailureVerdict(t *testing.T) {
	cases := []struct {
		up     int
		status int
		kind   string
		hint   string
	}{
		{429, 429, "relay_rate_limited", "rate-limited"},
		{400, 400, "relay_rejected", "/chat/completions"},
		{404, 404, "relay_rejected", "remote name mapping"},
		{401, 502, "relay_auth", "channel-side configuration"},
		{403, 502, "relay_auth", "channel-side configuration"},
		{500, 502, "relay_unavailable", "5xx"},
		{502, 502, "relay_unavailable", "5xx"},
		{0, 502, "relay_unreachable", "timeout"},
	}
	for _, c := range cases {
		status, kind, hint := relayFailureVerdict(c.up)
		if status != c.status || kind != c.kind {
			t.Errorf("upstream=%d → (%d,%s)，期望 (%d,%s)", c.up, status, kind, c.status, c.kind)
		}
		if !strings.Contains(hint, c.hint) {
			t.Errorf("upstream=%d 的 hint 缺 %q：%s", c.up, c.hint, hint)
		}
	}
}

// TestRelayAllChannelsFailRateLimited 上游 429 → 回 429（可重试语义），并带 fail_kind。
//
// 回 502 的后果：客户端不会按"限流"退避，用户以为网关坏了。
func TestRelayAllChannelsFailRateLimited(t *testing.T) {
	store, _, _, srv := newRelayChatApp(t, false)
	defer store.Close()
	defer srv.Close()

	up, _ := fakeRelay(t, 0, 0, 429, false)
	defer up.Close()
	seedRelay(t, store, up.URL, "limited-model", 1.0, true)
	seedPoolModels([]string{"pool-known-model"})

	resp, body := chatOnce(t, srv, `{"model":"limited-model","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("上游 429 应透传 429，得 %d：%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Wb-Fail-Kind"); got != "relay_rate_limited" {
		t.Fatalf("X-Wb-Fail-Kind = %q，期望 relay_rate_limited", got)
	}
	if !strings.Contains(body, "rate-limited") {
		t.Fatalf("错误里应说明是限流：%s", body)
	}
}

// TestRelayAllChannelsFailModelUnsupported 上游 400（模型不支持对话）→ 透传 400。
func TestRelayAllChannelsFailModelUnsupported(t *testing.T) {
	store, _, _, srv := newRelayChatApp(t, false)
	defer store.Close()
	defer srv.Close()

	up, _ := fakeRelay(t, 0, 0, 400, false)
	defer up.Close()
	// ⚠️ 名字里**不能**带 image 之类标记（v0.31.98）：那种名字会在网关本地就被判成
	// 生图模型并回 400 image_model_on_chat_endpoint，这条用例要验的是
	// "上游 4xx 原样透传"，所以用一个普通名字。
	seedRelay(t, store, up.URL, "chat-rejected-model", 1.0, true)
	seedPoolModels([]string{"pool-known-model"})

	resp, body := chatOnce(t, srv, `{"model":"chat-rejected-model","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("上游 400 应透传 400（不是 502），得 %d：%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Wb-Fail-Kind"); got != "relay_rejected" {
		t.Fatalf("X-Wb-Fail-Kind = %q，期望 relay_rejected", got)
	}
	if !strings.Contains(body, "/chat/completions") {
		t.Fatalf("错误里应点明可能不支持该端点：%s", body)
	}
}

// fakeRelayPerModel 一个**按模型区分状态**的假中转站：status[model] >= 400 就失败。
//
// 为什么需要它：组内轮换的正确性必须用"第一个真失败、第二个真成功"来证明，
// 单一状态的 fakeRelay 只能测出"全失败"。
func fakeRelayPerModel(t *testing.T, status map[string]int) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/models"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []map[string]any{}})
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			atomic.AddInt32(&hits, 1)
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			m, _ := body["model"].(string)
			if code := status[m]; code >= 400 {
				w.WriteHeader(code)
				fmt.Fprintf(w, `{"error":{"message":"boom %s"}}`, m)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "cmpl-" + m, "object": "chat.completion", "model": m,
				"choices": []any{map[string]any{
					"index": 0, "message": map[string]any{"role": "assistant", "content": "hello from " + m},
					"finish_reason": "stop",
				}},
				"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return srv, &hits
}

// seedChannelWithModels 一条通道 + 多个模型映射（组内轮换要"同一条通道的多个成员"）。
func seedChannelWithModels(t *testing.T, store *Store, name, baseURL string, models ...string) int64 {
	t.Helper()
	now := time.Now().Unix()
	res, err := store.db.Exec(`INSERT INTO upstreams
		(name, kind, base_url, api_key, enabled, priority, note, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?)`, name, "openai", baseURL, "sk-relay-test", 1, 10, "", now, now)
	if err != nil {
		t.Fatalf("插入通道：%v", err)
	}
	uid, _ := res.LastInsertId()
	for _, m := range models {
		if _, err := store.db.Exec(`INSERT INTO upstream_models
			(upstream_id, model, remote_model, credit_per_1k, enabled, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?)`, uid, m, "", 1.0, 1, now, now); err != nil {
			t.Fatalf("插入模型 %s：%v", m, err)
		}
	}
	return uid
}

// timeoutNetErr 一个 net.Error 的最小实现（只用于验证"超时"判定）。
type timeoutNetErr struct{}

func (timeoutNetErr) Error() string   { return "i/o timeout" }
func (timeoutNetErr) Timeout() bool   { return true }
func (timeoutNetErr) Temporary() bool { return true }

// TestRequestMaybeProcessed 区分"可能已受理"与"确定没发出去"（v0.31.99）。
//
// 这条判据直接决定会不会重复花钱：判宽了会把可安全重试的连接失败也停掉（可用性下降），
// 判窄了会把"上游已在算"的超时当成没送达去重试（重复计费）。
func TestRequestMaybeProcessed(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"context 超时", context.DeadlineExceeded, true},
		{"包装后的 context 超时", fmt.Errorf("post: %w", context.DeadlineExceeded), true},
		{"net 超时", &net.OpError{Op: "read", Net: "tcp", Err: timeoutNetErr{}}, true},
		{"连接被拒", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, false},
		{"普通错误", errors.New("bad handshake"), false},
	}
	for _, c := range cases {
		if got := requestMaybeProcessed(c.err); got != c.want {
			t.Errorf("%s：requestMaybeProcessed = %v，期望 %v", c.name, got, c.want)
		}
	}
}

// TestRetryVerdictFor 状态码 → 继续策略（v0.31.99）。
//
// 这张表就是"钱"：504/524 若判成可重试，一次客户端请求会让上游生成两份/计费两次。
func TestRetryVerdictFor(t *testing.T) {
	cases := []struct {
		code int
		want retryVerdict
	}{
		{504, retryNone},        // 上游已受理、超时未答（Cloudflare 524 同义）
		{524, retryNone},        // 非标准码，Go 无常量
		{429, retryNextChannel}, // 限流：换通道是常规降级
		{408, retryNextChannel},
		{409, retryNextChannel},
		{401, retryNextChannel}, // key 是通道级的，另一条可能好
		{403, retryNextChannel},
		{500, retryNextChannel},
		{502, retryNextChannel},
		{503, retryNextChannel},
		{400, retryNextModel}, // 请求本身跑不通，换通道无用
		{404, retryNextModel},
		{405, retryNextModel},
		{422, retryNextModel},
	}
	for _, c := range cases {
		if got := retryVerdictFor(c.code); got != c.want {
			t.Errorf("retryVerdictFor(%d) = %v，期望 %v", c.code, got, c.want)
		}
	}
}

// TestRelayGatewayTimeoutNotRetried 上游 504 → **只打一次**，并把"别重试"写进错误。
//
// 现场依据：Cloudflare 524 / 网关 504 的语义是"上游收到请求但没在超时内算完"。
// 这时重试 = 让上游把同一件事再做一遍（视频/生图会生成两份、对话会重复计费）。
func TestRelayGatewayTimeoutNotRetried(t *testing.T) {
	store, _, _, srv := newRelayChatApp(t, false)
	defer store.Close()
	defer srv.Close()

	up, hits := fakeRelayPerModel(t, map[string]int{"slow-model": 504})
	defer up.Close()
	seedRelay(t, store, up.URL, "slow-model", 1.0, true)
	seedPoolModels([]string{"pool-known-model"})

	resp, body := chatOnce(t, srv, `{"model":"slow-model","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("上游 504 应回 502（不是透传，SDK 会对 504 自动重试），得 %d：%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Wb-Fail-Kind"); got != "relay_timeout" {
		t.Fatalf("X-Wb-Fail-Kind = %q，期望 relay_timeout", got)
	}
	if !strings.Contains(body, "does NOT retry") {
		t.Fatalf("错误里应说明网关不会重试（否则用户会自己再点一次）：%s", body)
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Fatalf("504 不该重试，实际打了 %d 次上游", n)
	}
}

// TestRelayGroupStopsOnGatewayTimeout 组内首个成员 504 → 不换下一个成员。
//
// 与 429 的差别就在这里：429 换成员是对的（限流是临时的），504 换成员是错的
// （上游已经在算了，换一个成员就变成两笔生成/两份计费）。
func TestRelayGroupStopsOnGatewayTimeout(t *testing.T) {
	store, _, _, srv := newRelayChatApp(t, false)
	defer store.Close()
	defer srv.Close()

	// 名字要保证"会 504 的那个"排在组内首位：组展开按名字排序，且优先挑"能跑"的成员。
	// 这里用 aaa-/zzz- 前缀固定顺序，否则一旦 fast-b 排前面，这条用例测的就不是 504 了
	// （第一版就踩了：fast-b 字典序在前，直接 200 通过）。
	up, hits := fakeRelayPerModel(t, map[string]int{"aaa-slow": 504})
	defer up.Close()
	uid := seedChannelWithModels(t, store, "grp-timeout", up.URL, "aaa-slow", "zzz-fast")
	seedPoolModels([]string{"pool-known-model"})

	body := fmt.Sprintf(`{"model":"wb-group-ch%d","messages":[{"role":"user","content":"hi"}]}`, uid)
	resp, out := chatOnce(t, srv, body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("组内 504 应回 502，得 %d：%s", resp.StatusCode, out)
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Fatalf("504 之后不该再试同组其他成员，实际打了 %d 次", n)
	}
}

// TestRelayGroupRetriesNextMemberOnBadRequest 组内成员 400 → 换下一个成员（不是整体放弃）。
//
// 400 是"这个模型在这条链路上跑不通"，与通道无关；组的价值正是"这批里挑一个能跑的"。
func TestRelayGroupRetriesNextMemberOnBadRequest(t *testing.T) {
	store, _, _, srv := newRelayChatApp(t, false)
	defer store.Close()
	defer srv.Close()

	up, hits := fakeRelayPerModel(t, map[string]int{"bad-c": 400})
	defer up.Close()
	uid := seedChannelWithModels(t, store, "grp-badreq", up.URL, "bad-c", "good-d")
	seedPoolModels([]string{"pool-known-model"})

	body := fmt.Sprintf(`{"model":"wb-group-ch%d","messages":[{"role":"user","content":"hi"}]}`, uid)
	resp, out := chatOnce(t, srv, body)
	if resp.StatusCode != 200 {
		t.Fatalf("组内 400 应换成员后成功，得 %d：%s", resp.StatusCode, out)
	}
	if !strings.Contains(out, "hello from good-d") {
		t.Fatalf("应由第二个成员应答：%s", out)
	}
	if n := atomic.LoadInt32(hits); n != 2 {
		t.Fatalf("应各打一次（400 + 成功），实际 %d 次", n)
	}
}

// TestRelayGroupRetriesNextMember 组名请求：首个成员失败时**自动换下一个成员**。
//
// 这是 2026-10-21 现场的直接回归：同一条通道里 gpt-5.6 系列全部 429、
// gpt-6 系列 200，而旧的展开只挑名字最小的 gpt-5.6 且不重试 ——
// 用户"用组名永远失败，手填 gpt-6-astra 却正常"。组必须能在成员之间换。
func TestRelayGroupRetriesNextMember(t *testing.T) {
	store, _, _, srv := newRelayChatApp(t, false)
	defer store.Close()
	defer srv.Close()

	up, hits := fakeRelayPerModel(t, map[string]int{"bad-model": 429})
	defer up.Close()
	uid := seedChannelWithModels(t, store, "grp-relay", up.URL, "bad-model", "good-model")
	// 号池目录**非空**但都不含这两个 → decided=true 且 known=false → 走通道。
	// （目录为空时 poolKnowsModel 返回 decided=false，会按号池处理，见 serveRelay 开头。）
	seedPoolModels([]string{"pool-known-model"})

	body := fmt.Sprintf(`{"model":"wb-group-ch%d","messages":[{"role":"user","content":"hi"}]}`, uid)
	resp, out := chatOnce(t, srv, body)
	if resp.StatusCode != 200 {
		t.Fatalf("组内应换到可用成员后成功，得 %d：%s", resp.StatusCode, out)
	}
	if !strings.Contains(out, "hello from good-model") {
		t.Fatalf("应由第二个成员应答，响应体：%s", out)
	}
	if n := atomic.LoadInt32(hits); n != 2 {
		t.Fatalf("应各打一次（首个 429 + 备选成功），实际 %d 次", n)
	}
	// 计费与流水必须落在**实际服务**的那个模型上，不能记成组名/首个成员。
	var model string
	if err := store.db.QueryRow(`SELECT model FROM usage_logs ORDER BY id DESC LIMIT 1`).Scan(&model); err != nil {
		t.Fatalf("查流水：%v", err)
	}
	if model != "good-model" {
		t.Fatalf("流水 model = %q，期望 good-model（真实服务的那个）", model)
	}
}

// R4：停用通道 → 模型立刻从号池清单外消失（relayModelIDs 为空）。
//
// 「面板说关了、实际还能看见」是本项目反复踩过的形态（缓存没作废）。
func TestRelayDisabledChannelInvisible(t *testing.T) {
	store, _, _, srv := newRelayChatApp(t, false)
	defer store.Close()
	defer srv.Close()

	up, _ := fakeRelay(t, 10, 10, 200, false)
	defer up.Close()
	uid, _ := seedRelay(t, store, up.URL, "hidden-model", 1.0, true)
	if ids, _ := store.relayModelIDs(); len(ids) != 1 || ids[0] != "hidden-model" {
		t.Fatalf("启用时应可见，得 %v", ids)
	}
	if _, err := store.db.Exec(`UPDATE upstreams SET enabled = 0 WHERE id = ?`, uid); err != nil {
		t.Fatal(err)
	}
	if ids, _ := store.relayModelIDs(); len(ids) != 0 {
		t.Fatalf("停用后不应可见，得 %v", ids)
	}
	// 模型级停用同样不可见（两种停用必须都生效，否则"关了一半"）。
	if _, err := store.db.Exec(`UPDATE upstreams SET enabled = 1 WHERE id = ?`, uid); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE upstream_models SET enabled = 0 WHERE model = 'hidden-model'`); err != nil {
		t.Fatal(err)
	}
	if ids, _ := store.relayModelIDs(); len(ids) != 0 {
		t.Fatalf("模型级停用后不应可见，得 %v", ids)
	}
}

// R5：流式通道必须带 stream_options.include_usage，并按 usage 计费。
//
// 不带的话结算拿不到 token 数 → 只能 0 费（等于白送），而客户端毫无察觉。
func TestRelayStreamingCharges(t *testing.T) {
	store, _, _, srv := newRelayChatApp(t, false)
	defer store.Close()
	defer srv.Close()

	up, _ := fakeRelay(t, 200, 300, 200, true)
	defer up.Close()
	seedRelay(t, store, up.URL, "stream-model", 1.0, true)
	seedPoolModels([]string{"pool-known-model"})

	before := balanceOf(t, store)
	resp, body := chatOnce(t, srv, `{"model":"stream-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("应 200，得 %d：%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "hello from relay") {
		t.Fatalf("流式内容应透传：%s", body)
	}
	// 500 token × 1 积分/1k × 1 倍 = 0.5 积分 = 500 微点。
	if charged := before - balanceOf(t, store); charged != 500 {
		t.Fatalf("流式应扣 500 微点，实际 %d", charged)
	}
}

// balanceOf 读用户余额（微点）。
func balanceOf(t *testing.T, store *Store) int64 {
	t.Helper()
	var bal int64
	if err := store.db.QueryRow(`SELECT balance_micro FROM users WHERE username = 'relay-payer'`).Scan(&bal); err != nil {
		t.Fatalf("读余额：%v", err)
	}
	return bal
}
