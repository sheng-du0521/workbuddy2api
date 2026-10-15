// gateway_hint 的可达性验收（v0.31.5）。
//
// 背景：`error.gateway_hint`（"网关视角的补充说明"）此前**只有旧网关**会下发，webapp
// 的错误响应没有这个字段 —— 这是合并两个产品时枚举能力差集**漏掉的一处**（v0.31.4 只找到
// 降级重试）。本文件把它钉住：失败路径必须带上 hint、未覆盖形态字段必须**缺席**、
// 流内 error 帧透传时也要附加，而 message 原文一个字不动。
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/upstream"
)

// hintEnvelope 错误响应解形态（含可选 gateway_hint）。
type hintEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
		// 用指针：未覆盖形态必须**字段缺席**，不能是空串（断言"不编造"）。
		GatewayHint *string `json:"gateway_hint"`
	} `json:"error"`
}

// TestHintNoHealthyAccount 池里没有可用账号 → 固定本地调度 hint（与旧网关同文案）。
func TestHintNoHealthyAccount(t *testing.T) {
	// 注意：应用里**不注册任何账号** → 选号必然失败，走末端错误路径。
	store, px, a := newTestApp(t)
	t.Cleanup(func() { _ = store.Close() })
	a.key = "hint-admin-key"
	_ = px.SyncAccounts()
	mux := a.routes()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer hint-admin-key")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	var e hintEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("响应不是 JSON：%v body=%s", err, rec.Body.String())
	}
	if e.Error.GatewayHint == nil {
		t.Fatalf("无可用账号的失败应带 gateway_hint：%s", rec.Body.String())
	}
	if *e.Error.GatewayHint != upstream.NoHealthyAccountHint() {
		t.Errorf("hint=%q want %q", *e.Error.GatewayHint, upstream.NoHealthyAccountHint())
	}
}

// TestHintOnUpstreamFailure 上游类失败的 hint 必须来自**同一张判据表**（upstream.GatewayHint）。
func TestHintOnUpstreamFailure(t *testing.T) {
	// 11102「该后端无此模型」：上游 400 + 业务码 → ErrModelBlocked。
	const body = `{"code":11102,"msg":"service info not found"}`
	store, px, a := newTestApp(t, &Account{
		UID: "hint-acct", Realm: "cn", AccessToken: "at", RefreshToken: "rt",
		ExpiresAt: time.Now().Add(time.Hour).Unix(), ConsumeGroup: "apply", PoolState: "in",
	})
	t.Cleanup(func() { _ = store.Close() })
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, body)
	}))
	defer up.Close()
	px.uc.HTTP = &http.Client{Transport: rewriteTransport{target: up.URL}}
	a.key = "hint-admin-key"
	if err := px.SyncAccounts(); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}
	mux := a.routes()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer hint-admin-key")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	var e hintEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("响应不是 JSON：%v body=%s", err, rec.Body.String())
	}
	if e.Error.GatewayHint == nil {
		t.Fatalf("上游失败应带 gateway_hint：%s", rec.Body.String())
	}
	want := upstream.GatewayHint(upstream.ErrModelBlocked, body, upstream.HintContext{})
	if *e.Error.GatewayHint != want {
		t.Errorf("hint=%q want %q（必须来自同一张判据表）", *e.Error.GatewayHint, want)
	}
	// message 原文不得被 hint 替换/包装。
	if !strings.Contains(e.Error.Message, "service info not found") {
		t.Errorf("message 应含上游原文：%q", e.Error.Message)
	}
}

// TestHintAbsentWhenUncovered 未覆盖形态 → gateway_hint **字段缺席**（不编造空串）。
func TestHintAbsentWhenUncovered(t *testing.T) {
	store, px, a := newTestApp(t)
	t.Cleanup(func() { _ = store.Close() })
	_ = px.SyncAccounts()
	a.key = "hint-admin-key"
	mux := a.routes()

	// 非法 JSON：请求级错误，不属于 hint 表覆盖的形态。
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{not json`))
	req.Header.Set("Authorization", "Bearer hint-admin-key")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("响应不是 JSON：%v body=%s", err, rec.Body.String())
	}
	errObj, _ := raw["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("响应缺 error 对象：%s", rec.Body.String())
	}
	if _, ok := errObj["gateway_hint"]; ok {
		t.Errorf("未覆盖形态不得带 gateway_hint（宁缺勿编）：%v", errObj)
	}
}

// TestHintAttachedToStreamErrorFrame 流式：上游 error 帧透传时附加 hint，原文与 [DONE] 不受影响。
func TestHintAttachedToStreamErrorFrame(t *testing.T) {
	const errFrame = `{"error":{"message":"您的使用量已超出频率限制","code":"6004","requestId":"req-rl-9"}}`
	raw := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"glm-5.3\"," +
		"\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n" +
		"data: " + errFrame + "\n\n" +
		"data: [DONE]\n\n"

	store, px, a := newTestApp(t, &Account{
		UID: "hint-acct", Realm: "cn", AccessToken: "at", RefreshToken: "rt",
		ExpiresAt: time.Now().Add(time.Hour).Unix(), ConsumeGroup: "apply", PoolState: "in",
	})
	t.Cleanup(func() { _ = store.Close() })
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, raw)
	}))
	defer up.Close()
	px.uc.HTTP = &http.Client{Transport: rewriteTransport{target: up.URL}}
	a.key = "hint-admin-key"
	if err := px.SyncAccounts(); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}
	mux := a.routes()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer hint-admin-key")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	body := rec.Body.String()
	var frame map[string]any
	for _, ln := range strings.Split(body, "\n") {
		ln = strings.TrimSpace(ln)
		if !strings.HasPrefix(ln, "data: ") || strings.TrimPrefix(ln, "data: ") == "[DONE]" {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(ln[6:]), &obj) == nil {
			if e, ok := obj["error"].(map[string]any); ok {
				frame = e
			}
		}
	}
	if frame == nil {
		t.Fatalf("error 帧缺失：%q", body)
	}
	// 原文三字段一字不动。
	if frame["message"] != "您的使用量已超出频率限制" || frame["code"] != "6004" || frame["requestId"] != "req-rl-9" {
		t.Errorf("error 帧原文被改动：%v", frame)
	}
	if frame["gateway_hint"] == nil || frame["gateway_hint"] == "" {
		t.Errorf("流内 error 帧应带 gateway_hint：%v", frame)
	}
	if !strings.Contains(body, `"content":"hi"`) || strings.Count(body, "data: [DONE]") != 1 {
		t.Errorf("干净帧/[DONE] 被破坏：%q", body)
	}
}

// TestWriteOpenAIErrorHintEmptyIsIdentical hint 为空 → 与不带 hint 的出口**逐字节一致**。
//
// 这是"不编造"的机械保证：调用方不需要记得"什么时候该传空串"，两者行为必须相同。
func TestWriteOpenAIErrorHintEmptyIsIdentical(t *testing.T) {
	a := httptest.NewRecorder()
	writeOpenAIError(a, http.StatusBadRequest, "boom")
	b := httptest.NewRecorder()
	writeOpenAIErrorHint(b, http.StatusBadRequest, "boom", "")
	if a.Body.String() != b.Body.String() {
		t.Errorf("hint 为空时必须与 writeOpenAIError 逐字节一致：\na=%s\nb=%s", a.Body.String(), b.Body.String())
	}
}
