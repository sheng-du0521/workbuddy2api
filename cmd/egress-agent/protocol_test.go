package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 审查报告 F03：旧 agent 忽略 HTTP 410 的 stop 指令。
//
// 根因：`do()` 在状态码 >=400 时先返回错误，**body 从未反序列化** ——
// 而服务端的"这台机器不再接入"恰恰是 `410 + {"action":"stop"}`。
// 于是 agent 只把 410 当成"轮询失败"重试，保活循环又把隧道拉起来，
// 形成"服务端说停、本机反复起"的拉锯。
//
// 这组用例守协议层（不碰真实 SSH/子进程）：错误体必须被解析，且状态码可被类型识别。
func TestAgentParsesStopOn410(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusGone)
		_, _ = io.WriteString(w, `{"action":"stop","code":"user_egress_removed"}`)
	}))
	defer srv.Close()

	a := &agent{server: srv.URL, token: "t"}
	var spec cmdSpec
	err := a.get("/api/agent/poll?machine=m", &spec)

	if spec.Action != "stop" {
		t.Fatalf("410 的 stop 指令必须被解析出来，得 action=%q（F03 未修复）", spec.Action)
	}
	var se *httpStatusError
	if !errors.As(err, &se) {
		t.Fatalf("应返回带状态码的错误类型，得 %v", err)
	}
	if se.Code != http.StatusGone {
		t.Fatalf("状态码应为 410，得 %d", se.Code)
	}
}

// TestAgentTerminalStatuses 401/403 同样要能被识别为终态（凭据失效 / 能力被停用）。
func TestAgentTerminalStatuses(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			_, _ = io.WriteString(w, `{"action":"stop"}`)
		}))
		a := &agent{server: srv.URL, token: "t"}
		var spec cmdSpec
		err := a.get("/api/agent/poll", &spec)
		var se *httpStatusError
		if !errors.As(err, &se) || se.Code != code {
			t.Fatalf("HTTP %d 应被识别为终态，得 %v", code, err)
		}
		srv.Close()
	}
}

// TestAgentKeepsRetryingOn5xx 临时故障不能被误判为终态。
func TestAgentKeepsRetryingOn5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `upstream down`)
	}))
	defer srv.Close()

	a := &agent{server: srv.URL, token: "t"}
	var spec cmdSpec
	err := a.get("/api/agent/poll", &spec)
	var se *httpStatusError
	if !errors.As(err, &se) {
		t.Fatalf("应返回状态码错误，得 %v", err)
	}
	if se.Code != http.StatusBadGateway {
		t.Fatalf("应保留 502，得 %d", se.Code)
	}
	if se.Code == http.StatusGone || se.Code == http.StatusUnauthorized || se.Code == http.StatusForbidden {
		t.Fatal("5xx 不应被当作终态")
	}
}

// TestAgentParsesNormalCommand 正常 200 仍要正确解析（防止修 F03 时把正常路径弄坏）。
func TestAgentParsesNormalCommand(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"action":"start","at":123,"proxy_port":1080,"tunnel_port":11080}`)
	}))
	defer srv.Close()

	a := &agent{server: srv.URL, token: "t"}
	var spec cmdSpec
	if err := a.get("/api/agent/poll", &spec); err != nil {
		t.Fatalf("get: %v", err)
	}
	if spec.Action != "start" || spec.At != 123 || spec.TunnelPort != 11080 {
		t.Fatalf("指令解析错误：%+v", spec)
	}
}
