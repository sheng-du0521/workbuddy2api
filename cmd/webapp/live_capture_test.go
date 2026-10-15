// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ======================================================================
// v0.30.0 · 实时面板「只显示成功、看不到失败」的修复
//
// 根因：事件记录点埋在转发循环内部，而大量失败在**更早**就被决定了 ——
// apiKeyGuard（缺密钥 / 密钥无效 / 密钥停用 / 账号封禁 / 余额不足 / 配额超限 / 限流）
// 全部 return 在 ServeChat 之前，ServeChat 早段（请求体非法等）也 return 在记录之前。
// 这些请求一次都不会出现在曲线与事件流里，而 reqTotal 在 ServeChat 最顶部就 +1 ——
// 于是面板呈现「只有成功」的假象，且「累计次数」与「成功+失败」长期对不上。
//
// 修法：liveCapture 兜底层（挂在 /v1/chat/completions 最外层）+ noteLive 去重。
// ======================================================================

// liveApp 装配带真路由的 app（/v1 链路走真 mux，与生产同一条中间件链）。
func liveApp(t *testing.T) (*Store, *app, *http.ServeMux) {
	t.Helper()
	store, ap, mux := newP3App(t)
	// 每次用例结束清掉实时面板的内存态（它是包在 app.proxy 上的，用例间会串）。
	t.Cleanup(func() { *ap.proxy.live = *newLiveStats() })
	return store, ap, mux
}

// liveTotals / liveEvents 便捷读取（避免每处都写四返回值）。
func liveTotals(ap *app) (liveBucket, []liveEvent) {
	_, tot, evs, _ := ap.proxy.live.snapshot(liveWindowMinutes)
	return tot, evs
}

// postChat 打一次 /v1/chat/completions（auth 为空则不带头）。
func postChat(mux *http.ServeMux, auth, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestLiveCaptureRecordsGuardRejectionWithReason 被**鉴权层**拒绝的请求必须进面板。
//
// 这是本次缺陷的直接反例：修前这类请求只会让 reqTotal +1，曲线与事件流里什么都没有。
func TestLiveCaptureRecordsGuardRejectionWithReason(t *testing.T) {
	store, ap, mux := liveApp(t)
	defer store.Close()

	rec := postChat(mux, "", `{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无密钥应 401，got %d", rec.Code)
	}

	tot, evs := liveTotals(ap)
	if tot.Req != 1 || tot.Fail != 1 {
		t.Fatalf("该请求必须计一次失败（req=%d fail=%d ok=%d）", tot.Req, tot.Fail, tot.OK)
	}
	if len(evs) != 1 {
		t.Fatalf("事件流应有 1 条，实际 %d", len(evs))
	}
	ev := evs[0]
	if ev.OK {
		t.Errorf("应记成失败")
	}
	if ev.Status != http.StatusUnauthorized {
		t.Errorf("应带 HTTP 401，得 %d", ev.Status)
	}
	// 文案必须是**真实原因**（来自错误正文，而不是一句通用话术）——
	// 这决定了运维能否一眼分辨"密钥没带"与"密钥是错的"。
	if !strings.Contains(ev.Err, "missing api key") {
		t.Errorf("错误文案应取自响应正文，得 %q", ev.Err)
	}
	// 该请求没有任何身份，不能沿用 admin-key 兜底（那会被误读成运维用管理密钥发的）。
	if ev.User != "未鉴权" {
		t.Errorf("无身份的拒绝应记成「未鉴权」，得 %q", ev.User)
	}
}

// TestLiveCaptureRecordsInvalidKeyDisabledAndNoBalance 三类鉴权层拒绝都要进面板。
//
// 同时钉住「状态码不同 = 处置方向不同」：401（密钥不对）/ 403（密钥被停用）/
// 402（余额不足）在面板上必须能区分 —— 这是本次修复要交付的核心可读性。
func TestLiveCaptureRecordsInvalidKeyDisabledAndNoBalance(t *testing.T) {
	store, ap, mux := liveApp(t)
	defer store.Close()

	// ① 无效密钥 → 401。身份未建立，记「未鉴权」。
	if rec := postChat(mux, "not-a-real-key", `{"model":"m"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("无效密钥应 401，got %d", rec.Code)
	}
	_, evs := liveTotals(ap)
	if len(evs) != 1 || evs[0].Status != 401 {
		t.Fatalf("应记一条 401，得 %+v", evs)
	}
	if !strings.Contains(evs[0].Err, "invalid api key") {
		t.Errorf("应保留真实原因，得 %q", evs[0].Err)
	}
	if evs[0].User != "未鉴权" {
		t.Errorf("身份未建立时应记「未鉴权」，得 %q", evs[0].User)
	}

	// ② 停用的密钥 → 403。注意身份**仍是**「未鉴权」：apiKeyGuard 在"密钥状态"
	// 这一步就拒绝了，还没解析出主人 —— 这是如实的（那条错误文案本身已说明是密钥停用）。
	u, _ := store.CreateUser("live-disabled", "pw")
	plain := "sk-wb-live-disabled-key"
	k, err := store.CreateAPIKey(u.ID, "k", HashAPIKey(plain), "enc", keyPrefix(plain), "", "", "")
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if err := store.SetAPIKeyStatus(u.ID, k.ID, "disabled"); err != nil {
		t.Fatalf("SetAPIKeyStatus: %v", err)
	}
	if rec := postChat(mux, plain, `{"model":"m"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("停用密钥应 403，got %d", rec.Code)
	}
	tot, evs := liveTotals(ap)
	if tot.Fail != 2 || len(evs) != 2 {
		t.Fatalf("应有两条失败，得 fail=%d events=%d", tot.Fail, len(evs))
	}
	if evs[0].Status != http.StatusForbidden || !strings.Contains(evs[0].Err, "disabled") {
		t.Errorf("最新一条应是 403 + disabled 文案，得 %d %q", evs[0].Status, evs[0].Err)
	}

	// ③ 有效密钥但余额为 0 → 402，且**必须带上用户名**。
	//    （这条走的是 liveUserHint 回填：ctx 里的 user 只有全部校验通过才会有，
	//      而"谁的余额不足"正是这些失败里最该看清的。）
	u2, _ := store.CreateUser("live-nobalance", "pw")
	plain2 := "sk-wb-live-nobalance-key"
	if _, err := store.CreateAPIKey(u2.ID, "k2", HashAPIKey(plain2), "enc", keyPrefix(plain2), "", "", ""); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	if rec := postChat(mux, plain2, `{"model":"m"}`); rec.Code != http.StatusPaymentRequired {
		t.Fatalf("零余额应 402，got %d body=%s", rec.Code, rec.Body.String())
	}
	_, evs = liveTotals(ap)
	if evs[0].Status != http.StatusPaymentRequired {
		t.Fatalf("最新一条应是 402，得 %d", evs[0].Status)
	}
	if evs[0].User != u2.Username {
		t.Errorf("余额不足必须能看出是谁，期望 %q，得 %q", u2.Username, evs[0].User)
	}
	if !strings.Contains(evs[0].Err, "insufficient balance") {
		t.Errorf("应保留真实原因，得 %q", evs[0].Err)
	}
}

// TestLiveCaptureDoesNotDoubleCount ServeChat 自己记过的请求**不得**被兜底层再补一条。
//
// 这条比"看不到失败"更难发现：一次失败变成曲线上的两次请求，失败率直接翻倍。
func TestLiveCaptureDoesNotDoubleCount(t *testing.T) {
	store, ap, mux := liveApp(t)
	defer store.Close()

	// 管理密钥：过鉴权 → 进 ServeChat；池里没有账号 → 走"最终失败"分支自行记录。
	rec := postChat(mux, "p3-admin-key", `{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code < 400 {
		t.Fatalf("空池应失败，got %d body=%s", rec.Code, rec.Body.String())
	}
	tot, evs := liveTotals(ap)
	if len(evs) != 1 {
		t.Fatalf("必须只记一条（兜底层不得重复补记），实际 %d 条：%+v", len(evs), evs)
	}
	if tot.Req != 1 || tot.Fail != 1 {
		t.Fatalf("应恰好计 1 次失败，得 req=%d fail=%d", tot.Req, tot.Fail)
	}
	// ServeChat 记的那条带状态码（v0.30.0 起映射在记录之前完成）。
	if evs[0].Status != rec.Code {
		t.Errorf("事件状态码 %d 应与实际响应 %d 一致", evs[0].Status, rec.Code)
	}
	// model 必须被回填（兜底层在 next 之后运行、无法再解析 body；ServeChat 负责回填）。
	if len(evs) == 1 && evs[0].Model != "glm-5.3" {
		t.Errorf("应带回 model=glm-5.3，得 %q", evs[0].Model)
	}
}

// TestNoteLiveDedup 同一请求至多记一条（noteLive 的核心保证）。
func TestNoteLiveDedup(t *testing.T) {
	store, ap, _ := liveApp(t)
	defer store.Close()

	n := &liveNote{}
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req = req.WithContext(context.WithValue(req.Context(), liveNoteKey{}, n))

	ap.proxy.noteLive(req, liveEvent{OK: false, Err: "first"})
	ap.proxy.noteLive(req, liveEvent{OK: false, Err: "second"})
	_, evs := liveTotals(ap)
	if len(evs) != 1 {
		t.Fatalf("第二次应被去重，实际 %d 条", len(evs))
	}
	if evs[0].Err != "first" {
		t.Errorf("保留的应是第一条，得 %q", evs[0].Err)
	}
	// 没有 ctx 标记时（直接调 handler 的单测）退化为"照常记录"，不能被静默吞掉。
	ap.proxy.noteLive(httptest.NewRequest("POST", "/x", nil), liveEvent{OK: false, Err: "no-ctx"})
	if _, evs2 := liveTotals(ap); len(evs2) != 2 {
		t.Errorf("无 ctx 标记时应照常记录，实际 %d 条", len(evs2))
	}
}

// TestLiveStatusWriterBuffersOnlyOnError 只在出错时缓冲正文（成功路径尤其 SSE 一字节不多留）。
func TestLiveStatusWriterBuffersOnlyOnError(t *testing.T) {
	// ① 200：不缓冲
	rec := httptest.NewRecorder()
	w := &liveStatusWriter{ResponseWriter: rec, status: http.StatusOK}
	if _, err := w.Write([]byte(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	if len(w.buf) != 0 {
		t.Errorf("成功响应不该被缓冲，得 %q", w.buf)
	}
	// ② 402：缓冲并可由 liveErrorText 解出原因
	rec2 := httptest.NewRecorder()
	w2 := &liveStatusWriter{ResponseWriter: rec2}
	writeOpenAIError(w2, http.StatusPaymentRequired, "insufficient balance, please redeem a code")
	if w2.status != http.StatusPaymentRequired {
		t.Fatalf("应捕获 402，得 %d", w2.status)
	}
	if got := liveErrorText(w2.status, w2.buf); got != "insufficient balance, please redeem a code" {
		t.Errorf("应从正文解出原因，得 %q", got)
	}
	// ③ 缓冲有上限（防止一条大响应把内存吃掉）。
	rec3 := httptest.NewRecorder()
	w3 := &liveStatusWriter{ResponseWriter: rec3}
	w3.WriteHeader(http.StatusInternalServerError)
	w3.Write([]byte(strings.Repeat("x", liveCaptureBodyMax*3)))
	if len(w3.buf) > liveCaptureBodyMax {
		t.Errorf("缓冲不得超过 %d 字节，得 %d", liveCaptureBodyMax, len(w3.buf))
	}
}

// TestLiveStatusWriterKeepsFlusher SSE 逐帧冲刷依赖 http.Flusher 断言，包装层必须透传。
//
// 漏了它不会报错 —— 只会让流式响应"攒着不发"，表现是客户端长时间无输出（最难查的一类）。
func TestLiveStatusWriterKeepsFlusher(t *testing.T) {
	rec := httptest.NewRecorder()
	var w http.ResponseWriter = &liveStatusWriter{ResponseWriter: rec}
	if _, ok := w.(http.Flusher); !ok {
		t.Fatalf("liveStatusWriter 必须实现 http.Flusher（否则 SSE 退化）")
	}
	w.(http.Flusher).Flush() // 不应 panic
}

// TestLiveErrorTextFallback 正文解不出来时按状态码兜底（不能给空文案）。
func TestLiveErrorTextFallback(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{http.StatusUnauthorized, "not-json", "鉴权失败（密钥缺失或无效）"},
		{http.StatusPaymentRequired, "", "余额不足或超出消费配额"},
		{http.StatusForbidden, "", "密钥或账号已被停用"},
		{http.StatusTooManyRequests, "", "触发限流"},
		{http.StatusServiceUnavailable, "", "服务内部错误"},
		{http.StatusNotFound, "", "请求被拒绝（HTTP 404）"},
		// 正文可用时**优先**用正文里的真实原因
		{http.StatusBadRequest, `{"error":{"message":"re-encode body: boom"}}`, "re-encode body: boom"},
	}
	for _, c := range cases {
		if got := liveErrorText(c.status, []byte(c.body)); got != c.want {
			t.Errorf("status=%d body=%q → %q，期望 %q", c.status, c.body, got, c.want)
		}
	}
}

// TestLiveCaptureRecordsServeChatEarlyReturn ServeChat 早段的失败（请求体非法）也要进面板。
//
// 这条走的是兜底层（ServeChat 在解析 body 失败时直接 return，来不及自己记录）。
func TestLiveCaptureRecordsServeChatEarlyReturn(t *testing.T) {
	store, ap, mux := liveApp(t)
	defer store.Close()

	rec := postChat(mux, "p3-admin-key", `{ this is not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 JSON 应 400，got %d", rec.Code)
	}
	tot, evs := liveTotals(ap)
	if tot.Fail != 1 || len(evs) != 1 {
		t.Fatalf("非法请求体也必须计一次失败，得 fail=%d events=%d", tot.Fail, len(evs))
	}
	if evs[0].Status != http.StatusBadRequest {
		t.Errorf("应带 400，得 %d", evs[0].Status)
	}
	if !strings.Contains(evs[0].Err, "invalid json") {
		t.Errorf("应保留真实原因，得 %q", evs[0].Err)
	}
}

// TestHandleLiveExposesFailures 接口层：失败必须同时出现在 series.fail 与 totals.fail。
//
// 只测内存态是不够的 —— 前端读的是这个 JSON，字段名/层级错一位，界面就永远看不到失败。
func TestHandleLiveExposesFailures(t *testing.T) {
	store, _, mux := liveApp(t)
	defer store.Close()

	postChat(mux, "", `{"model":"m"}`) // 401
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/admin/live", nil)
	req.Header.Set("X-Admin-Key", "p3-admin-key")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("live 应 200，got %d", rec.Code)
	}
	var body struct {
		Totals struct {
			Requests int64 `json:"requests"`
			OK       int64 `json:"ok"`
			Fail     int64 `json:"fail"`
		} `json:"totals"`
		Series []struct {
			Req  int64 `json:"req"`
			OK   int64 `json:"ok"`
			Fail int64 `json:"fail"`
		} `json:"series"`
		Events []liveEvent `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析 live 响应：%v", err)
	}
	if body.Totals.Fail != 1 || body.Totals.OK != 0 {
		t.Errorf("totals 应为 ok=0 fail=1，得 ok=%d fail=%d", body.Totals.OK, body.Totals.Fail)
	}
	var sFail int64
	for _, p := range body.Series {
		sFail += p.Fail
	}
	if sFail != 1 {
		t.Errorf("series 的 fail 合计应为 1（曲线就靠它画失败段），得 %d", sFail)
	}
	if len(body.Events) != 1 || body.Events[0].OK {
		t.Errorf("events 应含那条失败，得 %+v", body.Events)
	}
}

// TestRequestCountersStayConsistent 累计计数器必须自洽：total == ok + fail。
//
// 修前是 self-inconsistent 的：reqTotal 记在 ServeChat 顶部，而 reqOK/reqFail 散在各分支 ——
// 被前置拒绝的请求只让 total +1（ok/fail 都不进），早退的请求同理。
// 这三个数同时喂「实时概览」的累计行与 /metrics 的 `wb_requests_total`，
// 对不上就等于两处都在报错数。
func TestRequestCountersStayConsistent(t *testing.T) {
	store, ap, mux := liveApp(t)
	defer store.Close()

	postChat(mux, "", `{"model":"m"}`)                           // 401 鉴权层拒绝
	postChat(mux, "p3-admin-key", `{ bad json`)                  // 400 ServeChat 早退
	postChat(mux, "p3-admin-key", `{"model":"m","messages":[]}`) // 503 空池（业务侧记录）

	total := int64(ap.proxy.reqTotal.Load())
	ok := int64(ap.proxy.reqOK.Load())
	fail := int64(ap.proxy.reqFail.Load())
	if total != 3 {
		t.Errorf("total 应为 3（三次请求都算数，包括被前置拒绝的那次），得 %d", total)
	}
	if ok != 0 {
		t.Errorf("本次没有成功请求，ok 应为 0，得 %d", ok)
	}
	if fail != 3 {
		t.Errorf("fail 应为 3，得 %d", fail)
	}
	if total != ok+fail {
		t.Errorf("必须满足 total == ok + fail，得 %d != %d + %d", total, ok, fail)
	}
	// 与窗口口径（曲线/事件流）对齐：两者的失败数应一致，否则界面两处数字互相打架。
	tot, evs := liveTotals(ap)
	if tot.Fail != fail {
		t.Errorf("窗口失败 %d 与累计失败 %d 应一致（同一批请求）", tot.Fail, fail)
	}
	if int64(len(evs)) != total {
		t.Errorf("事件数 %d 应与请求数 %d 一致（每条请求恰好一条事件）", len(evs), total)
	}
}
