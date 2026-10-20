// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// Anthropic Messages 协议兼容（v0.31.70，修 v0.31.69 的闭环缺口）。
//
// # 为什么必须做
//
// v0.31.69 的「一键接入 CC Switch」写的是 `ANTHROPIC_BASE_URL`，而 Claude Code
// 会请求 `/v1/messages`（Anthropic 协议）。当时服务端**只有 OpenAI 兼容端点**，
// 所以那条接入路径配好之后一发请求就是 404 —— 属于"看起来接好了、实际不通"，
// 而面板上不会报任何错。这一层就是补上它。
//
// 另修正一处拼写错误：`ANTHROPIC_BASE_URL` 的正确形态是 **origin（不含 /v1）**，
// 客户端自己拼 `/v1/messages`。之前写成 `origin + "/v1"` 会变成 `/v1/v1/messages`。
//
// # 怎么复用而不复制
//
// 转发、选号、通道降级、预扣、结算、错误分类 —— 全都在 `Proxy.ServeChat` 里。
// 再写一遍等于把"钱"和"风控"两套最贵的逻辑复制成两份（本项目最贵的教训）。
//
// 所以这里的做法是：
//  1. 把 Anthropic 请求体**转成 OpenAI 请求体**；
//  2. 用一个**响应适配器**（实现 http.ResponseWriter）包住真实响应，
//     把 ServeChat 写出的 OpenAI SSE/JSON **转回 Anthropic 格式**；
//  3. 中间那条链路一行都不改。
//
// 这也意味着：号池 / 第三方通道 / 计费 / 限流 / 冷却在这个入口上**自动全部生效**。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

// Anthropic 入口的请求体上限**直接引用 chatBodyMax**（不再有独立的 anthropicBodyMax）：
// v0.31.73 起 chatBodyMax 是可被 WB_BODY_MAX_MB 覆盖的包级 var，
// 另存一份副本（无论 const 还是 var 初始化）都会在环境变量生效后与它分叉，
// 变成"改了 OpenAI 侧上限、Claude Code 那条路还卡在旧值"。

// kvAnthropicDefaultModel Anthropic 入口的**兜底模型**（v0.31.70）。
//
// 为什么必须有：Claude Code 永远发 `claude-sonnet-4-5` 这类名字，而号池里的模型
// 叫 `glm-5.3` / `hy4-preview` —— 名字对不上，上游直接 11102（模型不存在）。
// 没有这一层，CC Switch 的接入仍然是"配置对了、请求全 404"。
//
// 口径：**只在本入口生效**。OpenAI 入口的模型名归一化有自己的一套（modelname），
// 在那边加兜底会把"客户端拼错模型名"静默变成一个能用的请求 —— 那是更坏的失败方向。
const kvAnthropicDefaultModel = "anthropic_default_model"

// anthropicResolveModel 给 Anthropic 请求里的模型名做兜底映射。
//
// 只替换**号池不认识**的名字：已知名字原样返回（不能让一个显式配好的模型被兜底覆盖）。
func (a *app) anthropicResolveModel(model string) (string, string) {
	def, _ := a.store.GetKV(kvAnthropicDefaultModel)
	def = strings.TrimSpace(def)
	if def == "" || model == "" {
		return model, ""
	}
	// ⚠️ 判据只能用**号池真实目录**且**严格相等**，不能用 knownModelIDs：
	// 后者把内置兜底清单也算进来、还带分隔符归一 —— 于是 `claude-sonnet-4-5`
	// 会因为和内置的 `claude-sonnet-4.5` 相似而被判成"已知"，映射就被跳过了，
	// 请求最终带着 claude-* 打到上游拿 11102（实测踩到）。
	ids := cachedModelIDs()
	if len(ids) == 0 {
		ids = a.proxy.pool.ObservedModels()
	}
	if len(ids) == 0 {
		// 没有任何依据 → 不映射（缺数据不等于"不认识"，与 relay 分发同一纪律）。
		return model, ""
	}
	for _, id := range ids {
		if id == model {
			return model, ""
		}
	}
	return def, model
}

// anthropicDefaultModelValid 校验用户填的默认模型是否"号池真的认识"（严格相等）。
func (a *app) anthropicDefaultModelValid(v string) bool {
	ids := cachedModelIDs()
	if len(ids) == 0 {
		ids = a.proxy.pool.ObservedModels()
	}
	for _, id := range ids {
		if id == v {
			return true
		}
	}
	return false
}

// handleAnthropicMessages POST /v1/messages
//
// 只做协议转换，不碰任何业务逻辑（见文件头）。
func (a *app) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	raw, err := readCappedBody(w, r, chatBodyMax)
	if err != nil {
		writeAnthropicError(w, http.StatusRequestEntityTooLarge, "invalid_request_error",
			"request body too large")
		return
	}
	var in map[string]any
	if err := json.Unmarshal(raw, &in); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error",
			"invalid json: "+err.Error())
		return
	}
	model, _ := in["model"].(string)
	// 模型名兜底（见 anthropicResolveModel 的"为什么必须有"）。
	if mapped, from := a.anthropicResolveModel(model); from != "" {
		log.Printf("INFO: [anthropic] 模型名兜底 %q → %q（该名字不在号池目录里；"+
			"默认模型来自设置 anthropic_default_model）", from, mapped)
		in["model"] = mapped
		model = mapped
	}
	wantStream, _ := in["stream"].(bool)

	payload, cerr := anthropicToOpenAI(in)
	if cerr != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", cerr.Error())
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "re-encode body: "+err.Error())
		return
	}

	// 构造一个"看起来像 OpenAI 客户端"的请求，交给同一条链路。
	// URL 用原路径即可（ServeChat 不看 r.URL）；关键是 body 与 Header 保持不变
	// —— 鉴权中间件已经把用户/Key 注入到 context 里了。
	nr := r.Clone(r.Context())
	nr.Body = io.NopCloser(bytes.NewReader(body))
	nr.ContentLength = int64(len(body))
	nr.Header = r.Header.Clone()
	nr.Header.Set("Content-Type", "application/json")

	ad := newAnthropicAdapter(w, model, wantStream)
	defer ad.finish()
	a.proxy.ServeChat(ad, nr)
}

// writeAnthropicError Anthropic 的错误信封。
//
// 形状与 OpenAI 不同（`{"type":"error","error":{"type","message"}}`），
// 客户端的 SDK 会按 type 分支处理；回 OpenAI 格式会被当成"未知响应"。
func writeAnthropicError(w http.ResponseWriter, status int, etype, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": etype, "message": msg},
	})
}

// ------------------------------------------------------------------ 请求转换

// anthropicToOpenAI 把 Anthropic Messages 请求转成 OpenAI Chat Completions 请求。
//
// 只转**语义相同**的部分；转不了的（如 thinking/computer_use 这类 Anthropic 私有块）
// 直接忽略而不是塞进上游 —— 塞进去只会被上游拒，错误还定位不到这里。
func anthropicToOpenAI(in map[string]any) (map[string]any, error) {
	out := map[string]any{}
	model, _ := in["model"].(string)
	if strings.TrimSpace(model) == "" {
		return nil, fmt.Errorf("model 不能为空")
	}
	out["model"] = model
	for _, k := range []string{"max_tokens", "temperature", "top_p"} {
		if v, ok := in[k]; ok && v != nil {
			out[k] = v
		}
	}
	// stop_sequences → stop（OpenAI 接受字符串或数组）
	if v, ok := in["stop_sequences"].([]any); ok && len(v) > 0 {
		out["stop"] = v
	}
	// stream 由调用方按客户端意愿设；这里透传（ServeChat 会自己决定向上游要流式）。
	if v, ok := in["stream"].(bool); ok {
		out["stream"] = v
	}

	msgs := make([]any, 0, 8)
	// system 可以是字符串，也可以是 [{type:"text",text}]。
	if sys := flattenAnthropicText(in["system"]); strings.TrimSpace(sys) != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": sys})
	}
	arr, _ := in["messages"].([]any)
	if len(arr) == 0 {
		return nil, fmt.Errorf("messages 不能为空")
	}
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if role == "" {
			continue
		}
		converted := anthropicContentToOpenAIMessages(role, m["content"])
		msgs = append(msgs, converted...)
	}
	out["messages"] = msgs

	// tools：input_schema → function.parameters
	if tools, ok := in["tools"].([]any); ok && len(tools) > 0 {
		ot := make([]any, 0, len(tools))
		for _, t := range tools {
			tm, ok := t.(map[string]any)
			if !ok {
				continue
			}
			name, _ := tm["name"].(string)
			if name == "" {
				continue
			}
			fn := map[string]any{"name": name}
			if d, ok := tm["description"]; ok && d != nil {
				fn["description"] = d
			}
			if s, ok := tm["input_schema"]; ok && s != nil {
				fn["parameters"] = s
			} else {
				fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			ot = append(ot, map[string]any{"type": "function", "function": fn})
		}
		if len(ot) > 0 {
			out["tools"] = ot
		}
	}
	// tool_choice：{type:"auto"|"any"|"tool", name} → OpenAI 形态
	if tc, ok := in["tool_choice"].(map[string]any); ok {
		switch tc["type"] {
		case "auto":
			out["tool_choice"] = "auto"
		case "any":
			out["tool_choice"] = "required"
		case "tool":
			if n, ok := tc["name"].(string); ok && n != "" {
				out["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": n}}
			}
		}
	}
	return out, nil
}

// flattenAnthropicText 把 system（字符串或文本块数组）压成一段纯文本。
func flattenAnthropicText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var sb strings.Builder
		for _, b := range t {
			if m, ok := b.(map[string]any); ok {
				if s, ok := m["text"].(string); ok {
					sb.WriteString(s)
				}
			}
		}
		return sb.String()
	}
	return ""
}

// anthropicContentToOpenAIMessages 一条 Anthropic 消息 → 一到多条 OpenAI 消息。
//
// **为什么可能变多条**：Anthropic 把工具结果放在 `user` 消息的 content 块里
// （`{type:"tool_result"}`），而 OpenAI 要求每个结果单独一条 `role:"tool"`。
// 不拆开的话上游会报"tool_call_id 缺失"，而报错位置离这里很远。
func anthropicContentToOpenAIMessages(role string, content any) []any {
	// 纯字符串：最常见的形态，直接对应。
	if s, ok := content.(string); ok {
		return []any{map[string]any{"role": role, "content": s}}
	}
	blocks, ok := content.([]any)
	if !ok {
		return []any{map[string]any{"role": role, "content": ""}}
	}

	var out []any
	var texts []string
	var toolCalls []any
	for _, b := range blocks {
		bm, ok := b.(map[string]any)
		if !ok {
			continue
		}
		switch bm["type"] {
		case "text":
			if s, ok := bm["text"].(string); ok && s != "" {
				texts = append(texts, s)
			}
		case "tool_use":
			// assistant 里的工具调用 → OpenAI tool_calls
			id, _ := bm["id"].(string)
			name, _ := bm["name"].(string)
			args := "{}"
			if in, ok := bm["input"]; ok && in != nil {
				if blob, err := json.Marshal(in); err == nil {
					args = string(blob)
				}
			}
			toolCalls = append(toolCalls, map[string]any{
				"id": id, "type": "function",
				"function": map[string]any{"name": name, "arguments": args},
			})
		case "tool_result":
			// user 里的工具结果 → **独立的** role:"tool" 消息
			id, _ := bm["tool_use_id"].(string)
			res := flattenAnthropicText(bm["content"])
			if res == "" {
				res = "(empty)"
			}
			out = append(out, map[string]any{
				"role": "tool", "tool_call_id": id, "content": res,
			})
		default:
			// image / thinking / 其它私有块：忽略（见函数注释）。
		}
	}
	if len(texts) > 0 || len(toolCalls) > 0 {
		m := map[string]any{"role": role, "content": strings.Join(texts, "\n")}
		if len(toolCalls) > 0 {
			m["tool_calls"] = toolCalls
			// OpenAI 要求带 tool_calls 时 content 可为空字符串，但**不能省略**字段。
			if m["content"] == "" {
				m["content"] = nil
			}
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		out = append(out, map[string]any{"role": role, "content": ""})
	}
	return out
}

// ------------------------------------------------------------------ 响应转换

// anthropicStopReason OpenAI finish_reason → Anthropic stop_reason。
func anthropicStopReason(fr string) string {
	switch fr {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		// Anthropic 没有等价值；用 end_turn 而不是编一个客户端不认识的值。
		return "end_turn"
	default:
		return "end_turn"
	}
}

// openAIToAnthropicMessage 非流式响应转换。
func openAIToAnthropicMessage(body []byte, model string) ([]byte, error) {
	var in struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content   any `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, err
	}
	text := ""
	if len(in.Choices) > 0 {
		switch c := in.Choices[0].Message.Content.(type) {
		case string:
			text = c
		case []any:
			text = flattenAnthropicText(c)
		}
	}
	content := make([]any, 0, 2)
	if text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	if len(in.Choices) > 0 {
		for _, tc := range in.Choices[0].Message.ToolCalls {
			var input any = map[string]any{}
			if strings.TrimSpace(tc.Function.Arguments) != "" {
				_ = json.Unmarshal([]byte(tc.Function.Arguments), &input)
			}
			content = append(content, map[string]any{
				"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": input,
			})
		}
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}
	fr := ""
	if len(in.Choices) > 0 {
		fr = in.Choices[0].FinishReason
	}
	id := in.ID
	if id == "" {
		id = "msg_" + randomHex(12)
	}
	if in.Model != "" {
		model = in.Model
	}
	return json.Marshal(map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": model,
		"content":     content,
		"stop_reason": anthropicStopReason(fr),
		"usage": map[string]any{
			"input_tokens":  in.Usage.PromptTokens,
			"output_tokens": in.Usage.CompletionTokens,
		},
	})
}

// ------------------------------------------------------------------ 响应适配器

// anthropicAdapter 包住真实 ResponseWriter，把 OpenAI 的字节流转成 Anthropic 格式。
//
// 它只做"出口转码"，因此对上层完全透明：ServeChat 不知道也不关心自己在跟谁说话。
type anthropicAdapter struct {
	out    http.ResponseWriter
	model  string
	stream bool

	hdr    http.Header
	status int
	wrote  bool

	// 非流式：缓冲整个 JSON 再转换（转换需要完整对象）。
	jsonBuf bytes.Buffer
	// 流式：按行切，处理完整行。
	lineBuf []byte

	// 流式状态机
	started    bool
	blockOpen  bool
	usageIn    int
	usageOut   int
	stopReason string
	sentStop   bool
	msgID      string
}

func newAnthropicAdapter(out http.ResponseWriter, model string, stream bool) *anthropicAdapter {
	return &anthropicAdapter{out: out, model: model, stream: stream, hdr: http.Header{}, status: 200}
}

func (a *anthropicAdapter) Header() http.Header { return a.hdr }

// WriteHeader 记录状态码；SSE 需要立刻把 200 与事件头写出去（否则客户端等到超时）。
func (a *anthropicAdapter) WriteHeader(code int) {
	if a.wrote {
		return
	}
	a.status = code
	a.wrote = true
	if a.stream && code < 400 {
		a.hdr.Set("Content-Type", "text/event-stream; charset=utf-8")
		a.hdr.Set("Cache-Control", "no-cache")
		a.hdr.Set("Connection", "keep-alive")
		copyHeader(a.out.Header(), a.hdr)
		a.out.WriteHeader(code)
	}
}

func (a *anthropicAdapter) Write(p []byte) (int, error) {
	if !a.wrote {
		a.WriteHeader(a.status)
	}
	if a.status >= 400 && !a.stream {
		a.jsonBuf.Write(p)
		return len(p), nil
	}
	if !a.stream {
		a.jsonBuf.Write(p)
		return len(p), nil
	}
	// 流式：切片成行。streamSSEHint 每行写一次，空行是事件分隔。
	a.lineBuf = append(a.lineBuf, p...)
	for {
		i := bytes.IndexByte(a.lineBuf, '\n')
		if i < 0 {
			break
		}
		line := string(a.lineBuf[:i])
		a.lineBuf = a.lineBuf[i+1:]
		a.handleSSELine(line)
	}
	return len(p), nil
}

func (a *anthropicAdapter) Flush() {
	if f, ok := a.out.(http.Flusher); ok {
		f.Flush()
	}
}

// finish 收尾：把缓冲的 JSON 转出去，或补齐 SSE 的结束事件。
func (a *anthropicAdapter) finish() {
	if !a.stream {
		body := a.jsonBuf.Bytes()
		if a.status >= 400 {
			// OpenAI 错误 → Anthropic 错误（客户端按 type 分支处理）
			msg := "upstream error"
			var e struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
				Message string `json:"message"`
			}
			if json.Unmarshal(body, &e) == nil {
				if e.Error.Message != "" {
					msg = e.Error.Message
				} else if e.Message != "" {
					msg = e.Message
				}
			}
			writeAnthropicError(a.out, a.status, anthropicErrType(a.status), msg)
			return
		}
		out, err := openAIToAnthropicMessage(body, a.model)
		if err != nil {
			log.Printf("WARN: [anthropic] 响应转换失败（回原始错误）：%v", err)
			writeAnthropicError(a.out, http.StatusBadGateway, "api_error",
				"failed to convert upstream response")
			return
		}
		a.hdr.Set("Content-Type", "application/json; charset=utf-8")
		copyHeader(a.out.Header(), a.hdr)
		a.out.WriteHeader(a.status)
		_, _ = a.out.Write(out)
		return
	}
	// 流式：上游可能没发 [DONE]（断流）。这里补齐，否则客户端一直等。
	a.emitStopIfNeeded()
	a.Flush()
}

// anthropicErrType HTTP 状态 → Anthropic 错误类型。
func anthropicErrType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	}
	return "api_error"
}

// handleSSELine 处理一行 OpenAI SSE，转成 Anthropic 事件。
func (a *anthropicAdapter) handleSSELine(line string) {
	if line == "" {
		// 事件分隔：把已产生的事件 flush 出去（Anthropic 也是空行分隔）。
		return
	}
	if !strings.HasPrefix(line, "data:") {
		return
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "" {
		return
	}
	if payload == "[DONE]" {
		a.emitStopIfNeeded()
		return
	}
	var chunk struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Delta struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return
	}
	if chunk.ID != "" && a.msgID == "" {
		a.msgID = chunk.ID
	}
	if chunk.Model != "" && a.model == "" {
		a.model = chunk.Model
	}
	if chunk.Usage != nil {
		a.usageIn = chunk.Usage.PromptTokens
		a.usageOut = chunk.Usage.CompletionTokens
	}
	a.emitStart()
	for _, ch := range chunk.Choices {
		// 文本增量
		if ch.Delta.Content != "" {
			a.emitTextDelta(ch.Delta.Content)
		}
		// 工具调用：参数是**分片**到达的，这里按 index 累积不划算（客户端会拼），
		// 直接每片发一个 input_json_delta —— 协议允许 partial_json 多次追加。
		for _, tc := range ch.Delta.ToolCalls {
			a.emitToolDelta(tc.Index, tc.ID, tc.Function.Name, tc.Function.Arguments)
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			a.stopReason = anthropicStopReason(*ch.FinishReason)
		}
	}
	a.Flush()
}

// event 写一个 Anthropic SSE 事件（event: + data: + 空行）。
func (a *anthropicAdapter) event(name string, obj map[string]any) {
	blob, err := json.Marshal(obj)
	if err != nil {
		return
	}
	fmt.Fprintf(a.out, "event: %s\ndata: %s\n\n", name, blob)
}

// emitStart 首个数据帧之前必须发 message_start（客户端据此建消息）。
func (a *anthropicAdapter) emitStart() {
	if a.started {
		return
	}
	a.started = true
	id := a.msgID
	if id == "" {
		id = "msg_" + randomHex(12)
	}
	a.event("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": id, "type": "message", "role": "assistant", "model": a.model,
			"content": []any{}, "stop_reason": nil,
			"usage": map[string]any{"input_tokens": a.usageIn, "output_tokens": 0},
		},
	})
}

// emitTextDelta 文本增量：必要时先开一个 text block。
func (a *anthropicAdapter) emitTextDelta(text string) {
	if !a.blockOpen {
		a.event("content_block_start", map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
		a.blockOpen = true
	}
	a.event("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
}

// emitToolDelta 工具调用：start（带 id/name）→ input_json_delta（拼参数）。
//
// Anthropic 的 tool_use 是一个独立 block（index 从 1 起，0 留给文本）。
// 这里用 index+1 对应上游的 tool_calls[i].index —— 顺序稳定，客户端按 index 归并。
func (a *anthropicAdapter) emitToolDelta(idx int, id, name, args string) {
	blockIdx := idx + 1
	if id != "" && name != "" {
		a.event("content_block_start", map[string]any{
			"type": "content_block_start", "index": blockIdx,
			"content_block": map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}},
		})
	}
	if args != "" {
		a.event("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": blockIdx,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
		})
	}
}

// emitStopIfNeeded 结束序列：content_block_stop → message_delta → message_stop。
//
// **必须幂等**：上游发 [DONE] 与 finish() 都会走到这里（少发一个事件客户端会挂住，
// 多发一个会让 SDK 报"状态机异常"）。
func (a *anthropicAdapter) emitStopIfNeeded() {
	if a.sentStop {
		return
	}
	a.sentStop = true
	if !a.started {
		a.emitStart()
	}
	if a.blockOpen {
		a.event("content_block_stop", map[string]any{
			"type": "content_block_stop", "index": 0,
		})
		a.blockOpen = false
	}
	sr := a.stopReason
	if sr == "" {
		sr = "end_turn"
	}
	a.event("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": sr},
		"usage": map[string]any{"output_tokens": a.usageOut},
	})
	a.event("message_stop", map[string]any{"type": "message_stop"})
	a.Flush()
}

// copyHeader 复制 header（避免直接赋值 map）。
func copyHeader(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Set(k, v)
		}
	}
}

// handleAnthropicCountTokens POST /v1/messages/count_tokens
//
// Claude Code 用它估算上下文占用。**我们不精确计数**（要真分词得引一个 tokenizer 依赖，
// 而本项目只有 3 个直接依赖，为此加一个不划算），给的是 ~4 字符/token 的粗估。
//
// 为什么必须实现而不是回 404：客户端拿不到计数会退化成"按字符上限猜测"，
// 表现为对话在远未到真实上限时就被截断 —— 用户会以为是我们限制了长度。
// 响应里 `note` 明确写出这是估算，避免被当成精确值用于计费核对。
func (a *app) handleAnthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	raw, err := readCappedBody(w, r, chatBodyMax)
	if err != nil {
		writeAnthropicError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "body too large")
		return
	}
	var in map[string]any
	if err := json.Unmarshal(raw, &in); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid json")
		return
	}
	chars := len(flattenAnthropicText(in["system"]))
	if arr, ok := in["messages"].([]any); ok {
		for _, item := range arr {
			if m, ok := item.(map[string]any); ok {
				chars += len(flattenAnthropicText(m["content"])) + len(flattenAnthropicText(m["text"]))
			}
		}
	}
	// 4 字符/token 是英文与代码的常见量级；中文会低估，因此保守取 3.5 再向上取整。
	est := (chars + 3) / 3
	if est < 1 {
		est = 1
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"input_tokens": est,
		"note":         "estimated (~3 chars/token), not a tokenizer count",
	})
}

// handleAdminAnthropicConfig GET/POST /api/admin/anthropic-config
//
// 只暴露一个设置项：Anthropic 入口的兜底模型。够了就不要再开字段 ——
// 每多一个旋钮，"改错了不知道"的面积就大一圈。
func (a *app) handleAdminAnthropicConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		def, _ := a.store.GetKV(kvAnthropicDefaultModel)
		known := knownModelIDs(a.proxy.pool.ObservedModels())
		writeJSON(w, http.StatusOK, map[string]any{
			"default_model": strings.TrimSpace(def),
			"known_models":  known,
			"note": "Claude Code 会发 claude-* 模型名；不在号池目录里的名字会被映射成这个默认模型。" +
				"留空 = 不做任何映射（此时非号池模型名会照常 404）。",
		})
		return
	}
	var req struct {
		DefaultModel string `json:"default_model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	v := strings.TrimSpace(req.DefaultModel)
	// 只接受"号池认识的名字"：填一个不存在的模型等于把错误藏起来
	// （请求会从一个明确的 404 变成"上游 11102"，更难查）。
	if v != "" {
		if !a.anthropicDefaultModelValid(v) {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "该模型不在号池目录里（先同步模型列表，或从下拉里选一个已知模型）",
				"code":  "unknown_model",
			})
			return
		}
	}
	if err := a.store.SetKV(kvAnthropicDefaultModel, v); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	log.Printf("INFO: [anthropic] 兜底模型设为 %q", v)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "default_model": v})
}
