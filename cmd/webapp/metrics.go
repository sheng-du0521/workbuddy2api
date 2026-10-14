// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// /metrics（Prometheus 文本格式）
//
// 与 cmd/server 的关系：那边已有 internal/server/metrics_prom.go，但它的写入器
// 面向 server 的 MetricsSnapshot（模型延迟直方图等本应用没有的维度），且未导出。
// webapp 这里只暴露「号池 + 租户 + 计费 + 限流」四类运维真正会告警的指标，
// 口径直接用 pool.RealmHealth 与本地库聚合，不引入第二套统计源。
// ---------------------------------------------------------------------------

// settingAlertWebhook / settingAlertMinHealthy 告警配置（0/空 = 关闭）。
const (
	settingAlertWebhook    = "alert_webhook_url"
	settingAlertMinHealthy = "alert_min_healthy_cn"
)

// MetricsCounts 一次取齐库侧计数（/metrics 用；单连接库上合并查询减少往返）。
type MetricsCounts struct {
	Users             int64
	KeysActive        int64
	Accounts          int64
	Reservations      int64
	UsageRequests     int64
	UsageTokens       int64
	UsageChargedMicro int64
}

// MetricsCounts 汇总库侧计数。单条查询失败不致命（返回部分结果 + err），
// 调用方只在 err == nil 时导出，避免把半截数据当成真值。
func (s *Store) MetricsCounts() (MetricsCounts, error) {
	var m MetricsCounts
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&m.Users); err != nil {
		return m, err
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE status = ?`, StatusActive).Scan(&m.KeysActive); err != nil {
		return m, err
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM accounts`).Scan(&m.Accounts); err != nil {
		return m, err
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM reservations`).Scan(&m.Reservations); err != nil {
		return m, err
	}
	if err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(prompt_tokens+completion_tokens),0), COALESCE(SUM(charged_micro),0)
	                         FROM usage_logs`).
		Scan(&m.UsageRequests, &m.UsageTokens, &m.UsageChargedMicro); err != nil {
		return m, err
	}
	return m, nil
}

// promWriter 极简 Prom 文本写入器（family 头 + 样本）。
type promWriter struct {
	buf strings.Builder
}

func (w *promWriter) family(name, help, typ string) {
	w.buf.WriteString("# HELP " + name + " " + help + "\n")
	w.buf.WriteString("# TYPE " + name + " " + typ + "\n")
}

// sample 写一条样本；labels 为 "k","v" 交替。
func (w *promWriter) sample(name string, value float64, labels ...string) {
	w.buf.WriteString(name)
	if len(labels) > 0 {
		w.buf.WriteString("{")
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				w.buf.WriteString(",")
			}
			w.buf.WriteString(labels[i] + `="` + escapePromLabel(labels[i+1]) + `"`)
		}
		w.buf.WriteString("}")
	}
	w.buf.WriteString(" " + formatPromValue(value) + "\n")
}

func escapePromLabel(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return strings.ReplaceAll(s, "\n", `\n`)
}

// formatPromValue 去掉多余的尾零（Prom 文本格式接受科学计数法）。
func formatPromValue(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// alertState 告警规则的活动状态（/metrics 暴露 + 状态跃迁触发 webhook）。
type alertState struct {
	mu     sync.Mutex
	active map[string]bool
	last   time.Time
}

func newAlertState() *alertState { return &alertState{active: map[string]bool{}} }

// evaluate 评估规则集合，返回本轮各规则的激活状态。
// 规则：
//
//	min_healthy_cn — CN 健康数 < 阈值（阈值 0 = 关闭该规则）
//	all_down       — 池内总数 > 0 且健康数为 0（对外已不可服务）
func (a *app) evaluateAlerts() map[string]bool {
	now := time.Now()
	cn := a.proxy.pool.RealmHealth("cn")
	all := a.proxy.pool.RealmHealth("")

	out := map[string]bool{}
	if th := a.alertMinHealthy(); th > 0 {
		out["min_healthy_cn"] = cn.Healthy < th
	}
	out["all_down"] = all.Total > 0 && all.Healthy == 0

	// 跃迁检测 + webhook 通知（best-effort，失败只记日志不影响抓取）。
	a.alerts.mu.Lock()
	prev := a.alerts.active
	a.alerts.active = out
	a.alerts.last = now
	a.alerts.mu.Unlock()

	if url := a.alertWebhook(); url != "" {
		for rule, act := range out {
			if act && !prev[rule] {
				go a.sendAlert(url, rule, "fired")
			} else if !act && prev[rule] {
				go a.sendAlert(url, rule, "resolve")
			}
		}
	}
	return out
}

// alertView 返回当前告警状态快照（供 /api/admin/status 与管理页展示）。
func (a *app) alertView() map[string]any {
	a.alerts.mu.Lock()
	defer a.alerts.mu.Unlock()
	active := make([]string, 0, len(a.alerts.active))
	for k, v := range a.alerts.active {
		if v {
			active = append(active, k)
		}
	}
	sort.Strings(active)
	return map[string]any{
		"active":      active,
		"webhook_set": a.alertWebhook() != "",
		"min_healthy": a.alertMinHealthy(),
		"last_eval":   a.alerts.last.Unix(),
	}
}

// alertPayload webhook 请求体（对齐 internal/alert 的 Payload 结构，便于同一接收端解析）。
type alertPayload struct {
	Version  int    `json:"version"`
	Event    string `json:"event"`
	Service  string `json:"service"`
	Rule     string `json:"rule"`
	Realm    string `json:"realm,omitempty"`
	Message  string `json:"message"`
	FiredAt  int64  `json:"fired_at"`
	Snapshot struct {
		CN struct {
			Total   int `json:"total"`
			Healthy int `json:"healthy"`
			Cooling int `json:"cooling"`
			Breaker int `json:"breaker"`
		} `json:"cn"`
		All struct {
			Total   int `json:"total"`
			Healthy int `json:"healthy"`
		} `json:"all"`
	} `json:"snapshot"`
}

// sendAlert 投递一条告警（超时 5s，失败静默记日志）。
func (a *app) sendAlert(url, rule, event string) {
	cnH := a.proxy.pool.RealmHealth("cn")
	allH := a.proxy.pool.RealmHealth("")
	p := alertPayload{Version: 1, Event: event, Service: "workbuddy2api-webapp", Rule: rule, FiredAt: time.Now().Unix()}
	p.Snapshot.CN.Total, p.Snapshot.CN.Healthy, p.Snapshot.CN.Cooling, p.Snapshot.CN.Breaker =
		cnH.Total, cnH.Healthy, cnH.Cooling, cnH.Breaker
	p.Snapshot.All.Total, p.Snapshot.All.Healthy = allH.Total, allH.Healthy
	if rule == "min_healthy_cn" {
		p.Realm = "cn"
		p.Message = fmt.Sprintf("CN 健康账号数 %d 低于阈值 %d", cnH.Healthy, a.alertMinHealthy())
	} else {
		p.Message = fmt.Sprintf("号池健康账号数为 0（总数 %d），已不可服务", allH.Total)
	}
	body, _ := json.Marshal(p)
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		alertLog("webhook 投递失败 rule=%s event=%s: %v", rule, event, err)
		return
	}
	resp.Body.Close()
	alertLog("webhook 已投递 rule=%s event=%s status=%d", rule, event, resp.StatusCode)
}

func (a *app) alertWebhook() string {
	v, _ := a.store.GetKV(settingAlertWebhook)
	return strings.TrimSpace(v)
}

func (a *app) alertMinHealthy() int {
	v, _ := a.store.GetKV(settingAlertMinHealthy)
	n, _ := strconv.Atoi(strings.TrimSpace(v))
	if n < 0 {
		return 0
	}
	return n
}

// handleMetrics 渲染 Prometheus 文本格式指标。
func (a *app) handleMetrics(w http.ResponseWriter, r *http.Request) {
	var pw promWriter

	pw.family("wb_webapp_info", "Build info (always 1).", "gauge")
	pw.sample("wb_webapp_info", 1, "version", version)

	pw.family("wb_webapp_uptime_seconds", "Seconds since process start.", "gauge")
	pw.sample("wb_webapp_uptime_seconds", time.Since(startedAt).Seconds())

	// 号池：分 realm + 全池，按 state 分解（口径与 /api/admin/pool 一致）。
	pw.family("wb_pool_accounts", "Pool accounts by realm and state.", "gauge")
	for _, rl := range []struct{ label, realm string }{{"cn", "cn"}, {"global", "global"}, {"all", ""}} {
		h := a.proxy.pool.RealmHealth(rl.realm)
		pw.sample("wb_pool_accounts", float64(h.Total), "realm", rl.label, "state", "total")
		pw.sample("wb_pool_accounts", float64(h.Healthy), "realm", rl.label, "state", "healthy")
		pw.sample("wb_pool_accounts", float64(h.Cooling), "realm", rl.label, "state", "cooling")
		pw.sample("wb_pool_accounts", float64(h.Disabled), "realm", rl.label, "state", "disabled")
		pw.sample("wb_pool_accounts", float64(h.Breaker), "realm", rl.label, "state", "breaker")
		pw.sample("wb_pool_accounts", float64(h.Degraded), "realm", rl.label, "state", "degraded")
		pw.sample("wb_pool_accounts", float64(h.InFlight), "realm", rl.label, "state", "in_flight")
	}

	// 转发实时计数。
	pw.family("wb_requests_total", "Chat completion requests by outcome.", "counter")
	pw.sample("wb_requests_total", float64(a.proxy.reqTotal.Load()), "outcome", "total")
	pw.sample("wb_requests_total", float64(a.proxy.reqOK.Load()), "outcome", "ok")
	pw.sample("wb_requests_total", float64(a.proxy.reqFail.Load()), "outcome", "error")

	// 客户端维度限流。
	pw.family("wb_ratelimit_total", "Rate-limit decisions on /v1/*.", "counter")
	pw.sample("wb_ratelimit_total", float64(a.limiter.allowed.Load()), "result", "allowed")
	pw.sample("wb_ratelimit_total", float64(a.limiter.blocked.Load()), "result", "blocked")
	pw.family("wb_ratelimit_rpm", "Configured per-client requests-per-minute cap (0=off).", "gauge")
	pw.sample("wb_ratelimit_rpm", float64(a.rateLimitRPM()))

	// 库侧聚合。
	if m, err := a.store.MetricsCounts(); err == nil {
		pw.family("wb_users_total", "Registered users.", "gauge")
		pw.sample("wb_users_total", float64(m.Users))
		pw.family("wb_api_keys_active", "Active API keys.", "gauge")
		pw.sample("wb_api_keys_active", float64(m.KeysActive))
		pw.family("wb_accounts_db_total", "Accounts persisted in DB.", "gauge")
		pw.sample("wb_accounts_db_total", float64(m.Accounts))
		pw.family("wb_reservations_active", "In-flight pre-charge reservations.", "gauge")
		pw.sample("wb_reservations_active", float64(m.Reservations))
		pw.family("wb_usage_requests_total", "Settled usage log rows.", "counter")
		pw.sample("wb_usage_requests_total", float64(m.UsageRequests))
		pw.family("wb_usage_tokens_total", "Settled tokens (prompt+completion).", "counter")
		pw.sample("wb_usage_tokens_total", float64(m.UsageTokens))
		pw.family("wb_usage_charged_micro_total", "Settled charge in micro-points.", "counter")
		pw.sample("wb_usage_charged_micro_total", float64(m.UsageChargedMicro))
	}

	// 告警状态（1 = 激活）。
	alerts := a.evaluateAlerts()
	pw.family("wb_alert_active", "Alert rules currently active (1=firing).", "gauge")
	rules := make([]string, 0, len(alerts))
	for k := range alerts {
		rules = append(rules, k)
	}
	sort.Strings(rules)
	for _, k := range rules {
		pw.sample("wb_alert_active", boolToFloat(alerts[k]), "rule", k)
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(pw.buf.String()))
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// alertLog 告警投递日志（与主日志同流，便于 grep）。
func alertLog(format string, args ...any) {
	log.Printf("[alert] "+format, args...)
}
