// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// 本文件锁死 v0.31.14 修的两件事（起因：线上出现 `HTTP 400 read body: unexpected EOF`）。
//
// ## 一、这条 400 的成因在**发送侧**，网关的责任只有「如实回报 + 留下现场」
//
// Go 的 `unexpected EOF` 只有一种成因：**声明的长度与实际收到的字节不一致**
// （Content-Length 写大了 / chunked 尾块没到 / 连接中途断）。可能是客户端库的 bug、
// 反向代理或隧道截断、本机 http_proxy 吃掉请求体 —— 网关无从判断是哪一段网络，
// 既不能代客户端重发，也不该替它猜。
//
// 但**此前这条分支一个字节的痕迹都不留**：/api/admin/live 只能看到"有一次 400 失败"，
// 台账里没有（没有用量）、日志里没有（没打日志）。于是"客户端自己截断"与
// "中间层吃掉请求体"在本机无法区分 —— 这正是本轮要修的**真实缺口**。
//
// 状态码取舍（v0.31.15）：截断回 **408（可重试）**，超限回 **413**，都不是 400 ——
// 400 会让标准客户端放弃重试，而"传输被截断"重发一次通常就好了。
//
// ## 二、顺带修掉一个会把根因说反的缺陷：8MB 以上被静默截断
//
// 旧实现是 `io.ReadAll(io.LimitReader(r.Body, 8<<20))`。`io.LimitReader` 到上限后
// 返回 **EOF 而不是错误** —— 于是 8MB 以上的请求体被静默截断，再交给 json.Unmarshal，
// 客户端看到的是 `invalid json: unexpected end of JSON input`：
// 一条把「请求太大」说成「JSON 写坏了」的消息。换成 MaxBytesReader → 413 说清上限。

// logCapture 捕获 log 输出（handler 里的取证行必须真的被写出来，不能只测纯函数）。
//
// 用互斥包一层：被测 app 可能还有别的 goroutine 在写日志，直接拿 bytes.Buffer
// 会在 -race 下报数据竞争。
type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *logCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// captureLog 把标准 logger 重定向到内存，返回捕获器与还原函数。
func captureLog(t *testing.T) *logCapture {
	t.Helper()
	c := &logCapture{}
	prev := log.Writer()
	log.SetOutput(c)
	t.Cleanup(func() { log.SetOutput(prev) })
	return c
}

// parseOpenAIError 解出 /v1 错误响应（error.message / error.gateway_hint）。
func parseOpenAIError(t *testing.T, rec *httptest.ResponseRecorder) (msg, hint string) {
	t.Helper()
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Hint    string `json:"gateway_hint"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("错误响应不是 JSON：%v body=%.200s", err, rec.Body.String())
	}
	return payload.Error.Message, payload.Error.Hint
}

// ----------------------------------------------------------------------
// 一、取证行与状态码映射（直接打函数，能覆盖 8MB 那条分支而不必真发 8MB）
// ----------------------------------------------------------------------

// TestBodyReadEvidenceFields 取证片段的字段口径：缺一不可，且空值有稳定占位。
//
// 这几个字段合起来才回答得了"谁的请求、声明多大、实际到了多少、传了多久、什么客户端"——
// 少了 content_length 与已读这一对，就分不清「客户端根本没发」与「发到一半被截断」。
func TestBodyReadEvidenceFields(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.ContentLength = 4096
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	req.Header.Set("User-Agent", "openai-python/1.40")

	got := bodyReadEvidence(req, 37, 1500*time.Millisecond)
	for _, want := range []string{
		"peer=127.0.0.1:54321", "xff=203.0.113.7",
		"content_length=4096", "已读=37", "耗时=1.5s", `ua="openai-python/1.40"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("取证片段缺 %q：%s", want, got)
		}
	}

	// 隧道场景：RemoteAddr 恒是回环、没有 xff、UA 为空 → 必须有占位符，
	// 不能留下一个空字段让人以为"没采集到"与"采集到空"是同一件事。
	bare := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	bare.Header.Del("User-Agent")
	got2 := bodyReadEvidence(bare, 0, 0)
	if !strings.Contains(got2, "xff=-") || !strings.Contains(got2, `ua="-"`) {
		t.Errorf("空值应有 '-' 占位：%s", got2)
	}
	if !strings.Contains(got2, "耗时=0s") {
		t.Errorf("0 耗时不应被舍入成别的值：%s", got2)
	}
}

// TestReadCappedBodyOversizeReturnsMaxBytesError 超限必须是**可判定类型**的错误。
//
// 这条同时钉住"不再静默截断"：旧实现（io.LimitReader）在这个输入下返回的是
// **nil 错误 + 被砍短的字节**，调用方随后只会得到一句 JSON 解析失败。
// 用 16 字节的小上限测，避免为了覆盖这一分支真去构造 8MB 请求体。
func TestReadCappedBodyOversizeReturnsMaxBytesError(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(strings.Repeat("a", 32)))
	rec := httptest.NewRecorder()

	raw, err := readCappedBody(rec, req, 16)
	if err == nil {
		t.Fatalf("超限必须报错（旧实现的静默截断正是这里返回 nil），得 raw=%d 字节", len(raw))
	}
	var tooLarge *http.MaxBytesError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("超限应返回 *http.MaxBytesError，得 %T：%v", err, err)
	}
	if tooLarge.Limit != 16 {
		t.Fatalf("Limit 应为 16，得 %d", tooLarge.Limit)
	}
	// 已读字节数必须是**到上限为止**的真实量（取证要用它区分截断位置）。
	if len(raw) != 16 {
		t.Fatalf("超限时应回读到上限为止的 16 字节，得 %d", len(raw))
	}
}

// TestReadCappedBodyKeepsPartialOnTruncation 读错时要保留**已读字节数**。
//
// 这是 old→new 的关键差异：io.ReadAll 报错时调用方拿不到长度，
// 于是"0 字节就断"与"7.9MB 才断"在日志里长得一模一样。
func TestReadCappedBodyKeepsPartialOnTruncation(t *testing.T) {
	body := io.MultiReader(strings.NewReader("{\"model\":\"m"), errReader{err: io.ErrUnexpectedEOF})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	rec := httptest.NewRecorder()

	raw, err := readCappedBody(rec, req, 1<<20)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("应透出 io.ErrUnexpectedEOF，得 %v", err)
	}
	if string(raw) != `{"model":"m` {
		t.Fatalf("已读内容应被保留，得 %q", raw)
	}
}

// errReader 读完即返回指定错误（模拟连接在传输尾部断掉）。
type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// TestReportBodyReadFailureOversizeIs413 超限 → 413（不是 400）+ 说清上限 + 有 hint。
//
// 为什么状态码必须是 413：400 的语义是"你的请求不合法"，会把客户端引向
// 检查自己的 JSON 序列化；413 才直接指向"请求太大"这个真实动作（减量/分段）。
func TestReportBodyReadFailureOversizeIs413(t *testing.T) {
	store, px, _ := newTestApp(t)
	defer store.Close() // 不关会在 Windows 上占住 t.TempDir() 里的库文件，清理阶段报错
	logs := captureLog(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.RemoteAddr = "127.0.0.1:5555"
	req.ContentLength = 9 << 20
	rec := httptest.NewRecorder()
	px.reportBodyReadFailure(rec, req, &http.MaxBytesError{Limit: chatBodyMax}, 1234, 2*time.Second)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限应回 413，得 %d", rec.Code)
	}
	msg, hint := parseOpenAIError(t, rec)
	if !strings.Contains(msg, "request body too large") || !strings.Contains(msg, fmt.Sprintf("%d", chatBodyMax)) {
		t.Fatalf("message 应说清上限（%d），得 %q", chatBodyMax, msg)
	}
	if !strings.Contains(hint, fmt.Sprintf("exceeds the gateway limit of %d bytes", chatBodyMax)) {
		t.Fatalf("hint 应指向「请求体超限」，得 %q", hint)
	}
	lg := logs.String()
	if !strings.Contains(lg, "请求体超限") || !strings.Contains(lg, "已读=1234") {
		t.Fatalf("超限必须留下带证据的日志，得 %q", lg)
	}
}

// TestReportBodyReadFailureTruncatedIsRetryable408 传输中断 → **408** + 原文 message + 有 hint + 有日志。
//
// ⚠️ 这条用例钉的是**状态码的语义**（v0.31.15）：截断是**瞬时传输故障**，不是"请求不合法"。
// 400 会让 OpenAI 兼容 SDK **不重试**（它们的重试名单是 408/409/429/5xx），于是本该
// 自动重发的故障变成用户每次都得手动重发的报错。所以这里必须是 408。
//
// message 里必须**逐字保留** Go 的原错（"read body: unexpected EOF"）：
// 用户报障时贴的就是这一句，它也是搜索引擎里唯一能查到成因的字符串，不能改写。
func TestReportBodyReadFailureTruncatedIsRetryable408(t *testing.T) {
	store, px, _ := newTestApp(t)
	defer store.Close()
	logs := captureLog(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.RemoteAddr = "192.0.2.9:40000"
	req.Header.Set("X-Forwarded-For", "198.51.100.3")
	req.ContentLength = 200
	rec := httptest.NewRecorder()
	px.reportBodyReadFailure(rec, req, io.ErrUnexpectedEOF, 118, 300*time.Millisecond)

	if rec.Code != http.StatusRequestTimeout {
		t.Fatalf("传输中断应回 408（可重试），得 %d", rec.Code)
	}
	if rec.Code == http.StatusBadRequest {
		t.Fatalf("不得回 400：标准 SDK 不对 400 重试，会把可自愈的故障变成用户可见的报错")
	}
	msg, hint := parseOpenAIError(t, rec)
	if msg != "read body: unexpected EOF" {
		t.Fatalf("message 应逐字保留 Go 原错，得 %q", msg)
	}
	// hint 要说清"成因在发送侧 + 没有触达上游账号 + 可以重发"。
	// "没有触达上游账号"是运维最关心的（据此断定不必去查账号池）；
	// "可以重发"是给客户端/用户看的下一步动作。
	for _, want := range []string{"Content-Length", "truncated", "no upstream account was used", "re-sending the same request"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint 缺 %q：%s", want, hint)
		}
	}
	lg := logs.String()
	for _, want := range []string{"请求体读取失败", "unexpected EOF", "content_length=200", "已读=118", "xff=198.51.100.3"} {
		if !strings.Contains(lg, want) {
			t.Errorf("取证日志缺 %q：%s", want, lg)
		}
	}
}

// ----------------------------------------------------------------------
// 二、端到端：用原始 TCP 造一次**真实的**截断请求体
// ----------------------------------------------------------------------

// TestChatTruncatedBodyEndToEnd 复现用户报的那条错误，并证明现场被记了下来。
//
// 为什么不用 http.Client 造这个输入：Go 的 Transport 会按 Body 的真实长度设置
// Content-Length，**造不出"声明 200 字节只发 40 字节"**这种形态（那正是线上故障的形态）。
// 只能自己开一个 TCP 连接，按 HTTP/1.1 手工写头与半截 body，再半关连接。
func TestChatTruncatedBodyEndToEnd(t *testing.T) {
	store, _, _, _, srv := newOutboundApp(t)
	defer store.Close()
	logs := captureLog(t)

	addr := strings.TrimPrefix(srv.URL, "http://")
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("连接测试服务器失败：%v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	// 声明 200 字节，实际只发 40 字节 —— 与线上故障同形态。
	head := "POST /v1/chat/completions HTTP/1.1\r\n" +
		"Host: " + addr + "\r\n" +
		"Authorization: Bearer sk-wb-outbound-test\r\n" +
		"Content-Type: application/json\r\n" +
		"User-Agent: truncating-client/0.1\r\n" +
		"Content-Length: 200\r\n" +
		"Connection: close\r\n\r\n"
	partial := `{"model":"m-test","messages":[{"role":"user"`
	if _, err := conn.Write([]byte(head + partial)); err != nil {
		t.Fatalf("写请求失败：%v", err)
	}
	// 半关写方向：服务端会看到「body 还没读满就 EOF」→ unexpected EOF。
	if tcp, ok := conn.(*net.TCPConn); ok {
		if err := tcp.CloseWrite(); err != nil {
			t.Fatalf("CloseWrite 失败：%v", err)
		}
	} else {
		t.Fatalf("测试连接不是 *net.TCPConn，无法半关")
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("读响应失败：%v（服务端可能没回响应就关了连接）", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	// 408 而不是 400：这是可重试的传输故障（见 reportBodyReadFailure 注释）。
	if resp.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("截断的请求体应回 408（可重试），得 %d body=%.200s", resp.StatusCode, raw)
	}
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Hint    string `json:"gateway_hint"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("响应不是 JSON：%v body=%.200s", err, raw)
	}
	if !strings.Contains(payload.Error.Message, "read body:") {
		t.Fatalf("message 应带 read body 前缀，得 %q", payload.Error.Message)
	}
	if !strings.Contains(payload.Error.Message, "unexpected EOF") {
		t.Fatalf("message 应含 unexpected EOF，得 %q", payload.Error.Message)
	}
	if payload.Error.Hint == "" {
		t.Fatalf("这条错误必须带 gateway_hint（否则客户端只能看到一句无指向的 400）")
	}
	// 本轮的核心收益：**日志里必须有现场**（含声明长度与客户端标识）。
	lg := logs.String()
	for _, want := range []string{"请求体读取失败", "content_length=200", "ua=\"truncating-client/0.1\""} {
		if !strings.Contains(lg, want) {
			t.Errorf("取证日志缺 %q：\n%s", want, lg)
		}
	}
}

// TestChatCompleteNonJSONBodyStaysInvalidJSON 回归护栏：**读体成功但不是 JSON**
// 仍必须回 `invalid json`，不能被上面的改动吞成"读体失败"。
//
// 这两类错误的处置完全相反（一个是"改你的 body 编码"，一个是"网络截断，重发即可"），
// 混在一起就等于把客户端引向错误的排查方向。
func TestChatCompleteNonJSONBodyStaysInvalidJSON(t *testing.T) {
	store, _, _, _, srv := newOutboundApp(t)
	defer store.Close()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m-test",`)) // 合法长度、非法 JSON
	req.Header.Set("Authorization", "Bearer sk-wb-outbound-test")
	req.Header.Set("Content-Type", "application/json")
	// 走真实链路（apiKeyGuard + ServeChat）。
	srv.Config.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 JSON 应回 400，得 %d body=%.200s", rec.Code, rec.Body.String())
	}
	msg, _ := parseOpenAIError(t, rec)
	if !strings.HasPrefix(msg, "invalid json:") {
		t.Fatalf("读体成功时必须是 invalid json（不是 read body），得 %q", msg)
	}
}
