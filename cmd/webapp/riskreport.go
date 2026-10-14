// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// riskreport.go 上游风控事件的「上报」台账 + 账号级限制的即时处置（v0.24.6）。
//
// 需求（用户原话）：「账号存在异常行为，已被限制使用」涉及此类异常**直接上报并且直接删除**，
// 不需要三次等待。
//
// 为什么这条要与「裸 11140」分开处置：
//
//	裸 11140 (`request illegal`) 是可恢复的抖动（并发 / 频率 / 出口 IP），
//	按它做不可逆处置会像 2026-10-13 那样一次误杀一批健康号 → 所以走「累计 3 次」。
//	而 displayMsg 里的「账号存在异常行为，已被限制使用」是**上游对账号本身的判定**，
//	且经实测证实换出口 IP 不解除 → 此时累计没有意义，等 3 次只是白等两轮。
//
// 「上报」在这里落地为三件事（都不依赖外部服务）：
//  1. 落库 `risk_reports`（uid / 文案 / 来源 / 动作 / 时间）—— 可查、可统计、可追溯；
//  2. WARN 日志（`[risk]` 前缀，便于 grep 与日志系统采集）；
//  3. 管理后台「健康检测」页的近期风控上报清单。

package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"workbuddy2api/internal/upstream"
)

// riskReport 一条风控上报记录。
type riskReport struct {
	ID       int64  `json:"id"`
	UID      string `json:"uid"`
	Nickname string `json:"nickname,omitempty"`
	Code     string `json:"code,omitempty"`    // 上游业务码（如 11140）
	Display  string `json:"display,omitempty"` // 上游原文（displayMsg）
	Source   string `json:"source"`            // health / proxy / manual
	Action   string `json:"action"`            // deleted / disabled / observed
	At       int64  `json:"at"`
}

// migrationRiskReports risk_reports 建表语句（由 Store.migrate 调用）。
const migrationRiskReports = `CREATE TABLE IF NOT EXISTS risk_reports (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	uid        TEXT NOT NULL,
	nickname   TEXT NOT NULL DEFAULT '',
	code       TEXT NOT NULL DEFAULT '',
	display    TEXT NOT NULL DEFAULT '',
	source     TEXT NOT NULL DEFAULT '',
	action     TEXT NOT NULL DEFAULT '',
	at         INTEGER NOT NULL DEFAULT 0
)`

// AddRiskReport 写入一条风控上报。
func (s *Store) AddRiskReport(r riskReport) error {
	if r.At == 0 {
		r.At = time.Now().Unix()
	}
	_, err := s.db.Exec(
		`INSERT INTO risk_reports (uid, nickname, code, display, source, action, at) VALUES (?,?,?,?,?,?,?)`,
		r.UID, r.Nickname, r.Code, r.Display, r.Source, r.Action, r.At)
	return err
}

// ListRiskReports 近期风控上报（最新在前，上限 limit）。
func (s *Store) ListRiskReports(limit int) ([]riskReport, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.Query(
		`SELECT id, uid, nickname, code, display, source, action, at
		   FROM risk_reports ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]riskReport, 0, limit)
	for rows.Next() {
		var r riskReport
		if err := rows.Scan(&r.ID, &r.UID, &r.Nickname, &r.Code, &r.Display, &r.Source, &r.Action, &r.At); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// reportRisk 上报一次风控事件：落库 + WARN 日志。
//
// 刻意**不**往 liveStats 里塞（那是"用户请求"的指标与事件流）：把风控事件混进
// 请求统计会污染每分钟的 Req/Fail 与成功率 —— 那些数字是要看的，不能为了"顺手
// 显示"而失真。上报有自己的台账与列表。
//
// 落库失败不阻断主流程（删除账号才是主流程），但会明确写日志 —— 不静默。
func (a *app) reportRisk(uid, nickname, code, display, source, action string) {
	r := riskReport{UID: uid, Nickname: nickname, Code: code, Display: display,
		Source: source, Action: action}
	if err := a.store.AddRiskReport(r); err != nil {
		log.Printf("WARN: [risk] 上报落库失败（uid=%s source=%s action=%s）：%v", uid, source, action, err)
	}
	log.Printf("WARN: [risk] uid=%s nick=%s code=%s source=%s action=%s display=%q",
		uid, nickname, code, source, action, display)
}

// handleRestrictedAccount 对「上游判定账号受限」的账号做**即时**处置：上报 + 立即删除。
//
// 与 11140 累计路径的区别：这里是上游对账号本身的判定，等累计没有意义。
//
// 幂等：并发场景下同一账号可能被多次触发（多个在途请求同时拿到该文案）。
// 先查存在性，账号已不在则直接返回 —— 既不重复删除，也不重复上报。
//
// 删除失败**不静默**：退化为「停用 + 归档」并如实记录 action=delete_failed，
// 否则界面会显示"已删除"而账号其实还在池里。
func (a *app) handleRestrictedAccount(uid, source, display string) []string {
	acc, err := a.store.GetAccount(uid)
	if err != nil || acc == nil {
		return nil // 已经处理过了（并发重复触发）
	}
	nick := ""
	if acc != nil {
		nick = acc.Nickname
	}
	if _, derr := a.deleteAccountsHard([]string{uid}); derr != nil {
		a.reportRisk(uid, nick, "11140", display, source, "delete_failed")
		a.proxy.pool.RetireAccount(uid, upstream.ErrAccountFault, upstream.AccountRestrictedHint())
		a.proxy.archiveAbnormal(uid, upstream.AccountRestrictedHint(), false)
		log.Printf("WARN: [risk] uid=%s 上游判定受限但删除失败，已改为停用：%v", uid, derr)
		return []string{hcActDisabled, hcActArchived}
	}
	a.reportRisk(uid, nick, "11140", display, source, "deleted")
	return []string{hcActDeleted}
}

// handleRiskReports GET /api/admin/risk/reports —— 近期风控上报。
func (a *app) handleRiskReports(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	list, err := a.store.ListRiskReports(limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reports": list, "total": len(list)})
}

// riskReportJSON 供测试/调试把一条上报转成 JSON（保证字段名与接口一致）。
func riskReportJSON(r riskReport) string {
	b, _ := json.Marshal(r)
	return string(b)
}
