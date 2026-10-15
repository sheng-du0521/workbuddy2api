// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// 实时运行态势（/api/admin/live）
//
// 与 /metrics 的分工：
//   /metrics 是给 Prometheus 抓的**累计 counter/gauge**，重启归零无所谓，
//     下游用 rate()/increase() 还原趋势；它回答「长期看量级对不对」。
//   这里是给人看的**实时仪表盘**：逐分钟时间序列 + 最近事件流 + 在途计数，
//     回答「此刻正在发生什么」。
// 口径不同（一个是单调累计、一个是滑动窗口），故独立一份内存态，不共享。
//
// 只保留近 90 分钟、按分钟分桶、进程重启即清空——这是刻意的：
// 实时面板不该被当成账本。账在 usage_logs（DB），报表页走那条路径。
// ---------------------------------------------------------------------------

// liveWindowMinutes 实时面板默认展示的窗口（分钟）。
const liveWindowMinutes = 60

// liveEvent 一条最近请求（实时事件流用）。不含 token/密钥，仅展示。
type liveEvent struct {
	At        int64   `json:"at"`
	User      string  `json:"user"`
	Model     string  `json:"model"`
	OK        bool    `json:"ok"`
	Tokens    int     `json:"tokens"`
	Charged   float64 `json:"charged"` // 点（已经过倍率换算）
	ElapsedMS int64   `json:"elapsed_ms"`
	Err       string  `json:"err,omitempty"`
	// Status 回给客户端的 HTTP 状态码（v0.30.0）。
	//
	// 为什么值得单独一个字段：失败的原因分布（余额不足 402 / 密钥停用 403 /
	// 限流 429 / 上游 503）光看文案不好聚合，而状态码是现成的分类维度；
	// 流式中断这类"响应头早已发出、改不了状态码"的情形留 0（omitempty 不出现），
	// 表示"没有可比的状态码"，而不是假装 200。
	Status int `json:"status,omitempty"`
}

// liveBucket 单分钟聚合。
type liveBucket struct {
	Req          int64
	OK           int64
	Fail         int64
	Tokens       int64
	ChargedMicro int64
}

// liveSeriesPoint 时间序列上的一点（已零填充，前端可直接画柱）。
type liveSeriesPoint struct {
	Minute  int64   `json:"minute"`
	Label   string  `json:"label"`
	Req     int64   `json:"req"`
	OK      int64   `json:"ok"`
	Fail    int64   `json:"fail"`
	Tokens  int64   `json:"tokens"`
	Charged float64 `json:"charged"`
}

// liveStats 内存态运行态势。单锁保护；写路径很短（一次请求两条：begin/done + record）。
type liveStats struct {
	mu       sync.Mutex
	buckets  map[int64]*liveBucket // 分钟序号(unix/60) → 聚合
	events   []liveEvent           // 最新在前，上限 maxEv
	inflight int64
	maxEv    int
}

func newLiveStats() *liveStats {
	return &liveStats{buckets: map[int64]*liveBucket{}, maxEv: 40}
}

// begin / done 维护在途请求计数（请求进入 +1，离开 -1）。
func (l *liveStats) begin() {
	l.mu.Lock()
	l.inflight++
	l.mu.Unlock()
}

func (l *liveStats) done() {
	l.mu.Lock()
	if l.inflight > 0 {
		l.inflight--
	}
	l.mu.Unlock()
}

// record 记一笔完成的请求（成功或失败），并顺带清理过期分钟桶。
func (l *liveStats) record(ev liveEvent) {
	if ev.At == 0 {
		ev.At = time.Now().Unix()
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	mi := ev.At / 60
	b := l.buckets[mi]
	if b == nil {
		b = &liveBucket{}
		l.buckets[mi] = b
	}
	b.Req++
	if ev.OK {
		b.OK++
	} else {
		b.Fail++
	}
	b.Tokens += int64(ev.Tokens)
	b.ChargedMicro += PointToMicro(ev.Charged)

	l.events = append([]liveEvent{ev}, l.events...)
	if len(l.events) > l.maxEv {
		l.events = l.events[:l.maxEv]
	}

	// 只留近 90 分钟：窗口 60 分钟，多留 30 分钟冗余，避免边界抖动。
	cutoff := mi - 90
	for k := range l.buckets {
		if k < cutoff {
			delete(l.buckets, k)
		}
	}
}

// snapshot 返回近 n 分钟的零填充序列 + 窗口合计 + 最近事件 + 当前在途数。
func (l *liveStats) snapshot(n int) (series []liveSeriesPoint, tot liveBucket, events []liveEvent, inflight int64) {
	if n <= 0 {
		n = liveWindowMinutes
	}
	cur := time.Now().Unix() / 60
	l.mu.Lock()
	defer l.mu.Unlock()

	series = make([]liveSeriesPoint, 0, n)
	start := cur - int64(n) + 1
	for mi := start; mi <= cur; mi++ {
		p := liveSeriesPoint{Minute: mi, Label: time.Unix(mi*60, 0).Format("15:04")}
		if b := l.buckets[mi]; b != nil {
			p.Req, p.OK, p.Fail = b.Req, b.OK, b.Fail
			p.Tokens = b.Tokens
			p.Charged = MicroToPoint(b.ChargedMicro)
			tot.Req += b.Req
			tot.OK += b.OK
			tot.Fail += b.Fail
			tot.Tokens += b.Tokens
			tot.ChargedMicro += b.ChargedMicro
		}
		series = append(series, p)
	}
	events = make([]liveEvent, len(l.events))
	copy(events, l.events)
	return series, tot, events, l.inflight
}

// liveUserName 事件流里的「谁」：租户用户名；管理密钥通道无用户实体 → admin-key。
func liveUserName(u *User) string {
	if u == nil {
		return "admin-key"
	}
	return u.Username
}

// ---------------------------------------------------------------------------
// 实时事件的「单一记录口径」（v0.30.0）
//
// ## 修的是什么
//
// 曲线与事件流此前只反映**走到上游的请求** —— 事件记录点埋在转发循环内部，
// 而大量失败在**更早**就被决定了，于是它们既不在曲线里、也不在事件流里：
//
//	apiKeyGuard（ServeChat 之前，一次都不会被记）
//	  缺密钥 / 密钥无效 / 密钥被停用 / 密钥主人不存在 / 账号被封禁 /
//	  余额不足 / 消费配额超限 / 触发限流
//	ServeChat 早段（记之前就 return 了）
//	  读请求体失败 / 请求体不是合法 JSON / 重编码失败 / 构造出站请求失败
//
// 而 reqTotal 在 ServeChat **最顶部**就 +1 —— 于是面板呈现「只有成功、没有失败」的
// 假象，且 KPI 的「累计 N 次」与「成功 + 失败」长期对不上（差额恰好是被前置拒绝的那些）。
//
// ## 修法（一个兜底出口，不自造第二套口径）
//
// 给 /v1 的转发链路套一层 liveCapture（最外层）：
//  1. 把「本请求是否已记录」的标记放进 ctx；
//  2. 转发结束后，若响应 >= 400 **且没有任何地方记过**，就在这里补记一条。
//
// 真正的记录仍由业务侧完成（ServeChat 的成功点 / 各失败点），只是改走 noteLive；
// noteLive 保证**同一请求至多记一条** —— 否则一次失败会变成曲线上的两次请求，
// 把失败率算高一倍（比"看不到失败"更难发现的错误）。
// ---------------------------------------------------------------------------

// liveNoteKey ctx 键（空结构体，零分配）。
type liveNoteKey struct{}

// liveNote 「本请求是否已记入实时面板」+ 它的结果。
//
// 用指针 + ctx 而不是在 Proxy 上开 map：标记的生命周期就是一次请求，随 ctx 一起释放；
// 用 map 还得自己回收（本仓已经吃过好几次"只增不减的 map"的亏）。
//
// ok 存在的理由：累计计数器（`wb_requests_total`）要靠它决定成败 —— 只看 HTTP 状态码
// 会判错"流式中断"（响应头早已 200，但请求实际失败了）。
type liveNote struct {
	recorded bool
	ok       bool
}

// liveNoteFrom 取标记；没有则返回 nil（调用方需判空 —— 直接调 handler 的单测就没有）。
func liveNoteFrom(r *http.Request) *liveNote {
	if r == nil {
		return nil
	}
	n, _ := r.Context().Value(liveNoteKey{}).(*liveNote)
	return n
}

// noteLive 记一条实时事件；同一请求只记第一条（见上方文件注释的最后一段）。
func (p *Proxy) noteLive(r *http.Request, ev liveEvent) {
	if n := liveNoteFrom(r); n != nil {
		if n.recorded {
			return
		}
		n.recorded, n.ok = true, ev.OK
	}
	p.live.record(ev)
}

// liveCaptureBodyMax 兜底时最多缓冲的响应体字节数（错误体只有一两百字节）。
const liveCaptureBodyMax = 2048

// liveStatusWriter 捕获响应状态码，且**仅在出错时**缓冲少量正文。
//
// 为什么连正文也捕获：writeOpenAIError 的正文里就是真实原因
// （`invalid api key` / `insufficient balance, please redeem a code` /
// `rate limit exceeded: 60 requests/minute`）—— 事件流里显示它比显示一句
// 「HTTP 402」有用得多。
//
// ⚠️ **必须实现 http.Flusher**：转发链路的 SSE 靠 `w.(http.Flusher)` 逐帧冲刷，
// 这层包装若不透传，流式响应会退化成"攒够缓冲才发"，表现是客户端长时间无输出。
func (w *liveStatusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

type liveStatusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
	buf    []byte
}

func (w *liveStatusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *liveStatusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.status, w.wrote = http.StatusOK, true
	}
	// 只在**出错**时缓冲：成功路径（尤其 SSE 流）一个字节都不多留。
	if w.status >= 400 && len(w.buf) < liveCaptureBodyMax {
		n := liveCaptureBodyMax - len(w.buf)
		if n > len(b) {
			n = len(b)
		}
		w.buf = append(w.buf, b[:n]...)
	}
	return w.ResponseWriter.Write(b)
}

// liveErrorText 从错误正文里取出最有用的那句；取不到就按状态码给一句兜底。
//
// 解析失败**不算错误**：这只是给面板加一句说明，绝不能因为一个格式变化让兜底失效。
func liveErrorText(status int, body []byte) string {
	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if len(body) > 0 && json.Unmarshal(body, &payload) == nil {
		if m := strings.TrimSpace(payload.Error.Message); m != "" {
			return m
		}
	}
	// 按状态码兜底（覆盖本仓 /v1 链路上会出现的几种拒绝）。
	switch {
	case status == http.StatusUnauthorized:
		return "鉴权失败（密钥缺失或无效）"
	case status == http.StatusPaymentRequired:
		return "余额不足或超出消费配额"
	case status == http.StatusForbidden:
		return "密钥或账号已被停用"
	case status == http.StatusTooManyRequests:
		return "触发限流"
	case status >= 500:
		return "服务内部错误"
	}
	return "请求被拒绝（HTTP " + strconv.Itoa(status) + "）"
}

// liveCapture 兜底记录层：任何"在 live 之外被拒绝"的请求也进曲线与事件流。
//
// 只挂在 /v1/chat/completions 上（面板的"请求曲线"就是这条链路的口径）；
// /v1/models 等元信息接口不计入，避免把"拉模型列表"算成业务请求。
func (a *app) liveCapture(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		n, mh, uh := &liveNote{}, &liveModelHint{}, &liveUserHint{}
		ctx := context.WithValue(r.Context(), liveNoteKey{}, n)
		ctx = context.WithValue(ctx, liveModelKey{}, mh)
		ctx = context.WithValue(ctx, liveUserKey{}, uh)
		r2 := r.WithContext(ctx)
		lw := &liveStatusWriter{ResponseWriter: w, status: http.StatusOK}
		next(lw, r2)

		// 累计计数器的**唯一**归属点（v0.30.0）：
		//   · 此前 reqTotal 记在 ServeChat 最顶部，而 reqOK/reqFail 散在各分支 ——
		//     于是「total != ok + fail」（被前置拒绝的请求只让 total +1，两类都不进）；
		//   · 判成败用 note.ok（业务侧记的那条）而不是 HTTP 状态码：流式中断时响应头
		//     早已是 200，只看状态码会把它记成成功 —— 与事件流里那条失败自相矛盾。
		failed := lw.status >= 400
		if n.recorded {
			failed = !n.ok
		}
		a.proxy.reqTotal.Add(1)
		if failed {
			a.proxy.reqFail.Add(1)
		} else {
			a.proxy.reqOK.Add(1)
		}

		if lw.status < 400 || n.recorded {
			return
		}
		// 走到这里 = 被前置环节拒绝（鉴权 / 余额 / 配额 / 限流 / 请求体）。
		// 身份优先取鉴权层**回填**的槽（见 liveUserHint）：ctx 里的 user 只有全部
		// 校验通过才会有，而"谁的余额不足"正是最该看清的。都没有时才记「未鉴权」——
		// 不能沿用 liveUserName 的 "admin-key" 兜底，那会把它误读成"运维用管理密钥发的请求"。
		who := "未鉴权"
		if uh.name != "" {
			who = uh.name
		} else if u := userFrom(r2); u != nil {
			who = u.Username
		}
		a.proxy.live.record(liveEvent{
			At: time.Now().Unix(), User: who, Model: mh.model, OK: false,
			Err:    liveErrorText(lw.status, lw.buf),
			Status: lw.status,
		})
	}
}

// liveUserHint 身份的「回填槽」（v0.30.0）。
//
// 为什么需要它：apiKeyGuard 只在**全部校验通过之后**才把用户放进 ctx，所以被它拒绝的
// 请求（密钥停用 / 账号封禁 / 余额不足 / 配额超限）在 ctx 里查不到用户 —— 面板只能显示
// "未鉴权"。而"谁的余额不足"恰恰是这些失败里最该看清的一件事。
// 于是单开一个只给实时面板用的槽：密钥主人一解析出来就回填，不影响鉴权语义
// （下游 handler 仍然只有在校验通过后才能从 ctx 取到 user）。
type liveUserHint struct{ name string }

// liveUserKey ctx 键。
type liveUserKey struct{}

// setLiveUser 回填身份（无兜底层时是空操作）。
func setLiveUser(r *http.Request, name string) {
	if r == nil || name == "" {
		return
	}
	if h, ok := r.Context().Value(liveUserKey{}).(*liveUserHint); ok {
		h.name = name
	}
}

// liveModelHint model 的「回填槽」：ServeChat 解析出 model 后写进来，兜底层读它。
//
// 为什么用共享指针而不是再解一次 body：兜底层在 next **之后**运行，那时请求体
// 早已被读完（再次读取只会拿到 EOF）。让解析方把结果放进同一个指针最省事，
// 也避免"为了一个展示字段去缓冲整个请求体"。
type liveModelHint struct{ model string }

// liveModelKey ctx 键。
type liveModelKey struct{}

// setLiveModel ServeChat 解析出 model 后回填（无兜底层时是空操作）。
func setLiveModel(r *http.Request, model string) {
	if r == nil {
		return
	}
	if h, ok := r.Context().Value(liveModelKey{}).(*liveModelHint); ok {
		h.model = model
	}
}

// modelHintFromRequest 取回填的 model；取不到返回空串（前端显示 '—'）。
func modelHintFromRequest(r *http.Request) string {
	if r == nil {
		return ""
	}
	if h, ok := r.Context().Value(liveModelKey{}).(*liveModelHint); ok {
		return h.model
	}
	return ""
}

// handleLive 实时运行态势：逐分钟序列 + 号池快照 + 告警 + 最近事件 + 计数。
func (a *app) handleLive(w http.ResponseWriter, r *http.Request) {
	series, tot, events, inflight := a.proxy.live.snapshot(liveWindowMinutes)

	pool := map[string]any{}
	for _, rl := range []struct{ name, realm string }{{"cn", "cn"}, {"global", "global"}, {"all", ""}} {
		h := a.proxy.pool.RealmHealth(rl.realm)
		pool[rl.name] = map[string]any{
			"total": h.Total, "healthy": h.Healthy, "cooling": h.Cooling,
			"disabled": h.Disabled, "breaker": h.Breaker, "degraded": h.Degraded,
			"inflight": h.InFlight,
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"now":      time.Now().Unix(),
		"uptime":   int64(time.Since(startedAt).Seconds()),
		"version":  version,
		"inflight": inflight,
		"series":   series,
		"totals": map[string]any{
			"minutes":  liveWindowMinutes,
			"requests": tot.Req, "ok": tot.OK, "fail": tot.Fail,
			"tokens": tot.Tokens, "charged": MicroToPoint(tot.ChargedMicro),
		},
		"requests": map[string]any{
			"total": a.proxy.reqTotal.Load(),
			"ok":    a.proxy.reqOK.Load(),
			"fail":  a.proxy.reqFail.Load(),
		},
		"ratelimit": map[string]any{
			"allowed": a.limiter.allowed.Load(),
			"blocked": a.limiter.blocked.Load(),
			"rpm":     a.rateLimitRPM(),
		},
		"pool":            pool,
		"alerts":          a.alertView(),
		"events":          events,
		"observed_models": a.proxy.observedModelCount(),
	})
}
