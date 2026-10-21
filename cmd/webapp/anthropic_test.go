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
	"testing"
	"time"
)

// ======================================================================
// Anthropic 协议兼容的回归网（v0.31.70）
//
// 这层的价值全在"转换正确"：转错了不会 500，只会让客户端拿到它读不懂的形状
// （Claude Code 表现为"模型不回话"或"工具调用丢失"），线上几乎无法定位。
// ======================================================================

// A1：请求转换 —— system / messages / tools / tool_result 拆分。
func TestAnthropicToOpenAIRequest(t *testing.T) {
	in := map[string]any{
		"model":       "glm-5.3",
		"max_tokens":  1024,
		"temperature": 0.2,
		"system": []any{
			map[string]any{"type": "text", "text": "你是助手。"},
		},
		"stop_sequences": []any{"END"},
		"tools": []any{
			map[string]any{
				"name": "read_file", "description": "读文件",
				"input_schema": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}},
			},
		},
		"tool_choice": map[string]any{"type": "auto"},
		"messages": []any{
			map[string]any{"role": "user", "content": "读一下 a.txt"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": "好"},
				map[string]any{"type": "tool_use", "id": "tu_1", "name": "read_file",
					"input": map[string]any{"path": "a.txt"}},
			}},
			// tool_result 在 Anthropic 里属于 user 消息，必须被拆成独立的 role:"tool"
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "tu_1",
					"content": []any{map[string]any{"type": "text", "text": "内容A"}}},
			}},
		},
	}
	out, err := anthropicToOpenAI(in)
	if err != nil {
		t.Fatalf("转换失败：%v", err)
	}
	if out["model"] != "glm-5.3" || out["max_tokens"] != 1024 {
		t.Fatalf("基础字段没带过去：%+v", out)
	}
	if _, ok := out["stop"]; !ok {
		t.Fatal("stop_sequences 应转成 stop")
	}
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("应得到 4 条（system/user/assistant/tool），得 %d：%+v", len(msgs), msgs)
	}
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("第一条应是 system，得 %+v", msgs[0])
	}
	// assistant 的 tool_use → tool_calls
	asst := msgs[2].(map[string]any)
	tc, ok := asst["tool_calls"].([]any)
	if !ok || len(tc) != 1 {
		t.Fatalf("assistant 应带 1 个 tool_calls：%+v", asst)
	}
	call := tc[0].(map[string]any)
	if call["id"] != "tu_1" {
		t.Fatalf("tool_call id 应保留：%+v", call)
	}
	fn := call["function"].(map[string]any)
	if fn["name"] != "read_file" || !strings.Contains(fn["arguments"].(string), "a.txt") {
		t.Fatalf("function 名/参数不对：%+v", fn)
	}
	// tool_result → 独立 role:"tool" 消息
	toolMsg := msgs[3].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "tu_1" {
		t.Fatalf("tool_result 应拆成 role=tool：%+v", toolMsg)
	}
	if !strings.Contains(toolMsg["content"].(string), "内容A") {
		t.Fatalf("工具结果内容丢了：%+v", toolMsg)
	}
	// tools / tool_choice
	tools, _ := out["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools 应转成 1 个 function：%+v", out["tools"])
	}
	if out["tool_choice"] != "auto" {
		t.Fatalf("tool_choice auto 应转成 \"auto\"，得 %v", out["tool_choice"])
	}
	// 非法请求：没有 model / 没有 messages 必须明确报错（而不是把空请求发上游）
	if _, err := anthropicToOpenAI(map[string]any{"messages": []any{}}); err == nil {
		t.Error("缺 model 应报错")
	}
	if _, err := anthropicToOpenAI(map[string]any{"model": "m"}); err == nil {
		t.Error("缺 messages 应报错")
	}
}

// A2：非流式响应转换 —— 文本、tool_calls、stop_reason、token 映射。
func TestOpenAIToAnthropicMessage(t *testing.T) {
	body := []byte(`{
		"id":"chatcmpl-1","model":"glm-5.3",
		"choices":[{"message":{"content":"你好","tool_calls":[
			{"id":"call_9","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"b.txt\"}"}}
		]},"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":12,"completion_tokens":34}
	}`)
	out, err := openAIToAnthropicMessage(body, "")
	if err != nil {
		t.Fatalf("转换失败：%v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("输出不是 JSON：%v", err)
	}
	if m["type"] != "message" || m["role"] != "assistant" {
		t.Fatalf("信封不对：%+v", m)
	}
	if m["stop_reason"] != "tool_use" {
		t.Fatalf("finish_reason=tool_calls 应映射成 tool_use，得 %v", m["stop_reason"])
	}
	content, _ := m["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("应有 text + tool_use 两个块：%+v", content)
	}
	if content[0].(map[string]any)["text"] != "你好" {
		t.Fatalf("文本块不对：%+v", content[0])
	}
	tu := content[1].(map[string]any)
	if tu["type"] != "tool_use" || tu["name"] != "read_file" || tu["id"] != "call_9" {
		t.Fatalf("tool_use 块不对：%+v", tu)
	}
	if input, _ := tu["input"].(map[string]any); input["path"] != "b.txt" {
		t.Fatalf("tool input 应解析成对象：%+v", tu["input"])
	}
	u := m["usage"].(map[string]any)
	if u["input_tokens"].(float64) != 12 || u["output_tokens"].(float64) != 34 {
		t.Fatalf("usage 映射不对：%+v", u)
	}
	// stop_reason 的另外两个映射
	if anthropicStopReason("length") != "max_tokens" {
		t.Error("length 应映射 max_tokens")
	}
	if anthropicStopReason("stop") != "end_turn" {
		t.Error("stop 应映射 end_turn")
	}
}

// A3：流式适配器 —— OpenAI 帧进去，Anthropic 事件序列出来。
//
// 断言的顺序就是协议要求的状态机顺序：message_start → content_block_start →
// content_block_delta → content_block_stop → message_delta → message_stop。
// 少一个客户端会挂住，多一个 SDK 会报状态机异常。
func TestAnthropicSSEAdapter(t *testing.T) {
	rec := httptest.NewRecorder()
	ad := newAnthropicAdapter(rec, "glm-5.3", true)
	ad.WriteHeader(200)
	frames := []string{
		`data: {"id":"c1","model":"glm-5.3","choices":[{"delta":{"role":"assistant","content":"你"}}]}` + "\n",
		`data: {"id":"c1","choices":[{"delta":{"content":"好"},"finish_reason":null}]}` + "\n",
		`data: {"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"tu_1","function":{"name":"read_file","arguments":"{\"path\":"}}]}}]}` + "\n",
		`data: {"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.txt\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7,"completion_tokens":9}}` + "\n",
		"data: [DONE]\n",
	}
	for _, f := range frames {
		_, _ = ad.Write([]byte(f))
	}
	ad.finish()
	body := rec.Body.String()

	for _, want := range []string{
		"event: message_start",
		"event: content_block_start",
		`"type":"text_delta"`,
		`"type":"input_json_delta"`,
		"event: content_block_stop",
		"event: message_delta",
		`"stop_reason":"tool_use"`,
		"event: message_stop",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("输出应包含 %q\n实际：\n%s", want, body)
		}
	}
	// 顺序：message_start 必须在第一条，message_stop 必须在最后。
	if strings.Index(body, "event: message_start") > strings.Index(body, "event: content_block_start") {
		t.Error("message_start 必须早于 content_block_start")
	}
	if strings.LastIndex(body, "event: message_stop") < strings.LastIndex(body, "event: message_delta") {
		t.Error("message_stop 必须在 message_delta 之后")
	}
	// 文本增量必须真的按两段发出（不是攒成一整段 —— 那样客户端就没有流式感了）。
	if strings.Count(body, `"type":"text_delta"`) != 2 {
		t.Errorf("应有 2 个 text_delta，实际 %d", strings.Count(body, `"type":"text_delta"`))
	}
	// usage 要回填到 message_delta（客户端据此算成本）。
	if !strings.Contains(body, `"output_tokens":9`) {
		t.Errorf("message_delta 应带 output_tokens=9：\n%s", body)
	}
}

// A4：流式适配器幂等 —— [DONE] 与 finish() 都会触发收尾，不能发两遍。
func TestAnthropicSSEAdapterIdempotentStop(t *testing.T) {
	rec := httptest.NewRecorder()
	ad := newAnthropicAdapter(rec, "m", true)
	ad.WriteHeader(200)
	_, _ = ad.Write([]byte(`data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n"))
	_, _ = ad.Write([]byte("data: [DONE]\n"))
	ad.finish() // 客户端断流时也会走到这里
	body := rec.Body.String()
	if n := strings.Count(body, "event: message_stop"); n != 1 {
		t.Fatalf("message_stop 必须只出现一次，实际 %d 次：\n%s", n, body)
	}
	if n := strings.Count(body, "event: message_delta"); n != 1 {
		t.Fatalf("message_delta 必须只出现一次，实际 %d 次", n)
	}
}

// A5：错误响应转换 —— OpenAI 错误信封 → Anthropic 错误信封。
func TestAnthropicErrorEnvelope(t *testing.T) {
	rec := httptest.NewRecorder()
	ad := newAnthropicAdapter(rec, "m", false)
	ad.WriteHeader(402)
	_, _ = ad.Write([]byte(`{"error":{"message":"insufficient balance","type":"upstream_error"}}`))
	ad.finish()
	if rec.Code != 402 {
		t.Fatalf("状态码应保留 402，得 %d", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("输出不是 JSON：%v %s", err, rec.Body.String())
	}
	if out["type"] != "error" {
		t.Fatalf("顶层应是 error 信封：%+v", out)
	}
	e, _ := out["error"].(map[string]any)
	if e["message"] != "insufficient balance" {
		t.Fatalf("应保留原始 message：%+v", e)
	}
	if e["type"] == "" {
		t.Fatalf("应带错误类型：%+v", e)
	}
}

// A6：端到端 —— 真打 /v1/messages（经完整路由：鉴权 → 选号 → 转发 → 计费 → 转码）。
//
// 为什么必须有这条：A1-A5 只证明"函数级转换对"，证明不了"路由挂上了、鉴权认
// x-api-key、ServeChat 写出的字节真的流到适配器里"。v0.31.69 的 CC Switch 就是
// 栽在"配置写对了、端点根本没有"这一类问题上。
func TestAnthropicEndpointEndToEnd(t *testing.T) {
	store, px, a := newTestApp(t, &Account{
		UID: "anth-acct", Nickname: "anth", Realm: "cn",
		AccessToken: "tok", RefreshToken: "rtok",
		ExpiresAt:    time.Now().Add(48 * time.Hour).Unix(),
		ConsumeGroup: "apply", PoolState: "in",
	})
	defer store.Close()
	up := sseUpstream(t, 5, 100, 200)
	defer up.Close()
	px.uc.HTTP = &http.Client{Transport: rewriteTransport{target: up.URL}}
	px.ReloadSelection()

	u, err := store.CreateUser("anth-user", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := store.AddUserBalance(u.ID, 1000*MicroPerPoint); err != nil {
		t.Fatalf("AddUserBalance: %v", err)
	}
	raw := "sk-wb-anth-test-key"
	enc, _ := a.secret.Encrypt(raw)
	if _, err := store.CreateAPIKey(u.ID, "t", HashAPIKey(raw), enc, "sk-wb", "", "", ""); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	srv := httptest.NewServer(a.routes())
	defer srv.Close()

	body := `{"model":"m-test","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	// 关键：用 **x-api-key**（Anthropic SDK 就这么发），不是 Authorization: Bearer。
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", raw)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	blob, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("应 200，得 %d：%s", resp.StatusCode, blob)
	}
	var m map[string]any
	if err := json.Unmarshal(blob, &m); err != nil {
		t.Fatalf("响应不是 JSON：%v %s", err, blob)
	}
	if m["type"] != "message" || m["role"] != "assistant" {
		t.Fatalf("应回 Anthropic message 信封，得：%s", blob)
	}
	if _, ok := m["content"].([]any); !ok {
		t.Fatalf("应有 content 数组：%s", blob)
	}
	if _, ok := m["usage"].(map[string]any); !ok {
		t.Fatalf("应有 usage（input_tokens/output_tokens）：%s", blob)
	}
	// 计费必须照常发生（复用同一条链路的意义就在这里）。
	var logs int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM usage_logs WHERE model = 'm-test'`).Scan(&logs); err != nil {
		t.Fatal(err)
	}
	if logs == 0 {
		t.Fatal("anthropic 入口也必须写用量流水（否则等于白用）")
	}

	// 流式：必须是 Anthropic 的事件序列，而不是 OpenAI 的 data: 帧。
	sreq, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages",
		strings.NewReader(`{"model":"m-test","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	sreq.Header.Set("x-api-key", raw)
	sreq.Header.Set("Content-Type", "application/json")
	sresp, err := srv.Client().Do(sreq)
	if err != nil {
		t.Fatalf("流式请求失败：%v", err)
	}
	sblob, _ := io.ReadAll(sresp.Body)
	_ = sresp.Body.Close()
	sbody := string(sblob)
	for _, want := range []string{"event: message_start", "event: content_block_delta", "event: message_stop"} {
		if !strings.Contains(sbody, want) {
			t.Errorf("流式输出应含 %q：\n%s", want, sbody)
		}
	}

	// 鉴权：不带头一律 401（这个入口会花钱，不能有匿名口子）。
	nreq, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(body))
	nreq.Header.Set("Content-Type", "application/json")
	nresp, _ := srv.Client().Do(nreq)
	nb, _ := io.ReadAll(nresp.Body)
	_ = nresp.Body.Close()
	if nresp.StatusCode != 401 {
		t.Fatalf("未带 key 应 401，得 %d：%s", nresp.StatusCode, nb)
	}

	// 缺 model → 400（且是 Anthropic 错误信封）。
	bad, _ := json.Marshal(map[string]any{"messages": []any{}})
	breq, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(string(bad)))
	breq.Header.Set("x-api-key", raw)
	bresp, _ := srv.Client().Do(breq)
	bb, _ := io.ReadAll(bresp.Body)
	_ = bresp.Body.Close()
	if bresp.StatusCode != 400 || !strings.Contains(string(bb), `"type":"error"`) {
		t.Fatalf("缺 model 应 400 + error 信封，得 %d：%s", bresp.StatusCode, bb)
	}
}

// A7：兜底模型映射 —— Claude Code 的 claude-* 要能落到号池认识的模型上。
//
// 这条守的是"CC Switch 接入真的能用"：没有映射，配置写对了也只会得到 11102。
func TestAnthropicDefaultModelMapping(t *testing.T) {
	store, _, a := newTestApp(t)
	defer store.Close()

	// 伪造号池目录：只有一个 glm-5.3。
	seedPoolModels([]string{"glm-5.3"})

	// 未设置默认模型 → 原样返回（不做任何映射）。
	if got, from := a.anthropicResolveModel("claude-sonnet-4-5"); from != "" || got != "claude-sonnet-4-5" {
		t.Fatalf("未配置时不应映射，得 %q（from=%q）", got, from)
	}
	// 已知模型 → 永远不映射（显式配置优先）。
	if err := store.SetKV(kvAnthropicDefaultModel, "glm-5.3"); err != nil {
		t.Fatal(err)
	}
	if _, fromKnown := a.anthropicResolveModel("glm-5.3"); fromKnown != "" {
		t.Fatalf("已知模型不该被兜底覆盖，from=%q", fromKnown)
	}
	// 未知模型 → 映射到默认值，并把原名回传（供日志）。
	got, from := a.anthropicResolveModel("claude-sonnet-4-5")
	if got != "glm-5.3" || from != "claude-sonnet-4-5" {
		t.Fatalf("未知模型应映射到默认值，得 %q（from=%q）", got, from)
	}
	// 空模型名不映射（交给后面的校验去报 400）。
	if got2, from2 := a.anthropicResolveModel(""); from2 != "" || got2 != "" {
		t.Fatalf("空模型名不该映射，得 %q", got2)
	}
}

// TestAnthropicErrorCarriesHintAndHeaders 错误路径必须带上「下一步」与排障头（v0.31.87）。
//
// 两件此前在 Anthropic 路径上一直是丢的：
//
//	① OpenAI 信封里的 `gateway_hint`（该去哪充值 / 为什么没号）—— Anthropic 的
//	   error 对象只有 type/message，不拼进 message 就等于没给；
//	② `X-Wb-Fail-Kind` / `Retry-After` —— 这条错误分支只写 a.out，从不 copyHeader。
func TestAnthropicErrorCarriesHintAndHeaders(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		orig   string // 上游原话：必须原样保留（排障第一步就是看它）
		want   string // gateway_hint：必须跟在后头
	}{
		{
			name:   "503 无可用账号",
			status: http.StatusServiceUnavailable,
			body:   `{"error":{"code":503,"message":"upstream unavailable: no available account","gateway_hint":"no healthy account available in pool; retry later"}}`,
			orig:   "upstream unavailable: no available account",
			want:   "no healthy account available in pool",
		},
		{
			name:   "402 余额耗尽",
			status: http.StatusPaymentRequired,
			body:   `{"error":{"code":402,"message":"insufficient balance","gateway_hint":"redeem a code in the console"}}`,
			orig:   "insufficient balance",
			want:   "redeem a code in the console",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			ad := newAnthropicAdapter(rec, "glm-5.3", false)
			ad.Header().Set("X-Wb-Fail-Kind", "hard_credit")
			ad.Header().Set("Retry-After", "7")
			ad.WriteHeader(c.status)
			if _, err := ad.Write([]byte(c.body)); err != nil {
				t.Fatalf("Write: %v", err)
			}
			ad.finish()

			if rec.Code != c.status {
				t.Errorf("状态码 = %d，期望 %d", rec.Code, c.status)
			}
			got := rec.Body.String()
			// 信封形状必须是 Anthropic 的（OpenAI 格式会被 SDK 当未知响应）。
			if !strings.Contains(got, `"type":"error"`) || !strings.Contains(got, `"message"`) {
				t.Errorf("不是 Anthropic 错误信封：%s", got)
			}
			// 原文必须保留：hint 是**追加**，不是替换。
			if !strings.Contains(got, c.orig) {
				t.Errorf("上游原话被改写（%q 不见了）：%s", c.orig, got)
			}
			if !strings.Contains(got, c.want) {
				t.Errorf("message 里没有 gateway_hint（%q）：%s", c.want, got)
			}
			if h := rec.Header().Get("X-Wb-Fail-Kind"); h != "hard_credit" {
				t.Errorf("X-Wb-Fail-Kind = %q，期望 hard_credit（排障全靠它）", h)
			}
			if h := rec.Header().Get("Retry-After"); h != "7" {
				t.Errorf("Retry-After = %q，期望 7（客户端据此决定等多久）", h)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
				t.Errorf("Content-Type = %q，期望 application/json", ct)
			}
		})
	}
}

// TestAnthropicErrorWithoutHintUnchanged 没有 hint 时信封与旧版逐字一致。
//
// 这条防的是"为了带 hint 把普通错误的输出改了" —— 客户端可能拿旧输出做匹配。
func TestAnthropicErrorWithoutHintUnchanged(t *testing.T) {
	rec := httptest.NewRecorder()
	writeAnthropicError(rec, http.StatusBadRequest, "invalid_request_error", "bad thing")
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("不是合法 JSON：%v", err)
	}
	if got["type"] != "error" {
		t.Errorf("type = %v", got["type"])
	}
	e, _ := got["error"].(map[string]any)
	if e["type"] != "invalid_request_error" || e["message"] != "bad thing" {
		t.Errorf("错误体被改变了：%v", e)
	}
	if strings.Contains(rec.Body.String(), "\\n") {
		t.Errorf("没有 hint 时不该多出换行：%s", rec.Body.String())
	}
}
