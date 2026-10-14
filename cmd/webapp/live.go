// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"net/http"
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
