// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// resetAuthLog 清空节流状态（进程级 map，用例之间会互相污染）。
func resetAuthLog() {
	authLogMu.Lock()
	authLogLast = map[string]time.Time{}
	authLogDrop = map[string]int{}
	authLogMu.Unlock()
}

// TestMaskKeyForLogNeverPrintsWholeKey 日志里绝不能出现完整密钥。
//
// 这是硬约束而不是"尽量"：日志会被拷进工单、贴进聊天、进 journalctl 归档。
// 只要有一条完整 key 落进去，就等于密钥泄露。
func TestMaskKeyForLogNeverPrintsWholeKey(t *testing.T) {
	full := "sk-wb-A5FaHZ0123456789abcdefP9ak"
	got := maskKeyForLog(full)
	if got == full {
		t.Fatal("掩码后仍是完整密钥")
	}
	if !strings.HasPrefix(got, "sk-wb-A5Fa") {
		t.Fatalf("应保留前 10 位便于核对，得 %q", got)
	}
	if !strings.HasSuffix(got, "P9ak") {
		t.Fatalf("应保留后 4 位便于核对，得 %q", got)
	}
	if strings.Contains(got, "0123456789abcdef") {
		t.Fatalf("中段必须被截掉，得 %q", got)
	}
	if maskKeyForLog("") != "" {
		t.Fatal("空串应返回空串")
	}
	if s := maskKeyForLog("abc"); s == "abc" {
		t.Fatalf("短 key 不能原样输出，得 %q", s)
	}
}

// TestLogAuthRejectWritesReasonAndKey 拒绝日志必须同时有原因与 key 指纹。
//
// 起因（2026-10-19）：用户报「反代 403」，服务端零日志，连"请求来过没有"都不知道。
func TestLogAuthRejectWritesReasonAndKey(t *testing.T) {
	resetAuthLog()
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.RemoteAddr = "203.0.113.9:5555"
	r.Header.Set("User-Agent", "curl/8.4.0")

	cap := captureLog(t)
	logAuthReject(r, 403, "api key disabled", maskKeyForLog("sk-wb-XXXXsecretXXXX"), "key id=10029")
	out := cap.String()
	for _, want := range []string{"[auth]", "/v1/chat/completions", "403", "api key disabled", "203.0.113.9", "curl/8.4.0", "key id=10029"} {
		if !strings.Contains(out, want) {
			t.Fatalf("日志缺少 %q：\n%s", want, out)
		}
	}
	if strings.Contains(out, "secret") {
		t.Fatalf("日志泄露了密钥中段：\n%s", out)
	}
}

// TestLogAuthRejectThrottles 同 IP 同原因在窗口内只打一条，窗口过后恢复并带抑制计数。
//
// 不节流的后果：公网扫描器不断打 /v1，日志被刷满，真正的用户故障被淹没。
func TestLogAuthRejectThrottles(t *testing.T) {
	resetAuthLog()
	r := httptest.NewRequest("GET", "/v1/models", nil)
	r.RemoteAddr = "198.51.100.7:1111"

	cap := captureLog(t)
	for i := 0; i < 5; i++ {
		logAuthReject(r, 401, "missing api key", "", "")
	}
	out := cap.String()
	lines := 0
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.Contains(l, "[auth]") {
			lines++
		}
	}
	if lines != 1 {
		t.Fatalf("同一窗口内应只有 1 行，实得 %d 行：\n%s", lines, out)
	}

	// 模拟"窗口已过、期间抑制了 4 条"：下一次必须重新打，并说明抑制了几条。
	authLogMu.Lock()
	authLogLast["198.51.100.7|missing api key"] = time.Now().Add(-2 * authLogWindow)
	authLogDrop["198.51.100.7|missing api key"] = 4
	authLogMu.Unlock()

	cap2 := captureLog(t)
	logAuthReject(r, 401, "missing api key", "", "")
	out2 := cap2.String()
	if !strings.Contains(out2, "[auth]") {
		t.Fatalf("窗口过后应恢复打印，得 %q", out2)
	}
	if !strings.Contains(out2, "已抑制 4 条") {
		t.Fatalf("恢复打印时必须带抑制计数，得 %q", out2)
	}
}

// TestSessionRejectReasonDistinguishes 401 要能区分"没带 cookie"与"cookie 无效"。
func TestSessionRejectReasonDistinguishes(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/me", nil)
	if got := sessionRejectReason(r); got != "no session cookie" {
		t.Fatalf("没 cookie 时应报 no session cookie，得 %q", got)
	}
	r2 := httptest.NewRequest("GET", "/api/me", nil)
	r2.AddCookie(&http.Cookie{Name: sessionCookie, Value: "deadbeef"})
	if got := sessionRejectReason(r2); got != "session invalid or expired" {
		t.Fatalf("带无效 cookie 时应报 invalid，得 %q", got)
	}
}
