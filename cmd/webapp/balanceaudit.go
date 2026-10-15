// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ======================================================================
// 余额变动审计（v0.27.0）
//
// 背景：管理后台的「加余额 / 扣余额」此前只是 `balance += delta` ——
// **谁改的、改了多少、改前改后各是多少，一处都没记**。钱的事没有凭据，
// 事后对不上账就只能互相猜。
//
// 设计取舍：
//
//  1. **只审计"人工调整"**（管理后台按钮）。计费结算、返点入账、充值兑换这些
//     都有各自的流水表（usage_logs / rebates / host_earnings / redeem_codes），
//     再往这里写一份只会让"账目对不上时该看哪张表"变得更难判断。
//
//  2. **成功与失败都记**。失败记录同样有价值：它证明"有人试过但被拒绝"，
//     这正是排查"我明明点了却没生效"时最需要的那条信息。
//
//  3. **记 admin_name 快照**，不只记 admin_id。管理员账号改名或删除后，
//     只留一个 id 会让历史记录失去可读性（审计的全部意义就是事后能读懂）。
//
//  4. 变动值用**微点**原样存（不换算），避免任何换算精度问题；
//     展示层再做点数换算，与项目其余部分的「分」一致。
// ======================================================================

// BalanceAudit 一条余额人工调整记录。
type BalanceAudit struct {
	ID         int64  `json:"id"`
	UserID     int64  `json:"user_id"`
	Username   string `json:"username"`
	AdminID    int64  `json:"admin_id"`
	AdminName  string `json:"admin_name"`
	DeltaMicro int64  `json:"delta_micro"`
	// BeforeMicro / AfterMicro 是**操作前后**的余额快照。记快照而不是让读的人
	// 自己累加，是因为累加依赖"中间没有任何其它变动"这个前提 —— 而它不成立
	//（同一秒可能有计费结算在跑）。
	BeforeMicro int64  `json:"before_micro"`
	AfterMicro  int64  `json:"after_micro"`
	Reason      string `json:"reason"`
	Result      string `json:"result"` // ok / rejected
	Error       string `json:"error,omitempty"`
	CreatedAt   int64  `json:"created_at"`
}

// 审计结果取值。
const (
	auditOK       = "ok"
	auditRejected = "rejected"
)

const migrationBalanceAudits = `CREATE TABLE IF NOT EXISTS balance_audits (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id      INTEGER NOT NULL DEFAULT 0,
	username     TEXT    NOT NULL DEFAULT '',
	admin_id     INTEGER NOT NULL DEFAULT 0,
	admin_name   TEXT    NOT NULL DEFAULT '',
	delta_micro  INTEGER NOT NULL DEFAULT 0,
	before_micro INTEGER NOT NULL DEFAULT 0,
	after_micro  INTEGER NOT NULL DEFAULT 0,
	reason       TEXT    NOT NULL DEFAULT '',
	result       TEXT    NOT NULL DEFAULT '',
	error        TEXT    NOT NULL DEFAULT '',
	created_at   INTEGER NOT NULL DEFAULT 0
)`

// EnsureBalanceAudits 建表 + 索引（由 migrate 调用，幂等）。
func (s *Store) EnsureBalanceAudits() error {
	if _, err := s.db.Exec(migrationBalanceAudits); err != nil {
		return err
	}
	// 两个索引对应两种查法：按用户翻历史、按时间倒序看最近动作。
	if _, err := s.db.Exec(
		`CREATE INDEX IF NOT EXISTS idx_balance_audits_user ON balance_audits(user_id, created_at DESC)`); err != nil {
		return err
	}
	_, err := s.db.Exec(
		`CREATE INDEX IF NOT EXISTS idx_balance_audits_created ON balance_audits(created_at DESC)`)
	return err
}

// RecordBalanceAudit 落一条审计。
//
// **故意不返回错误给业务路径**：审计写失败不该让已经成功的余额调整"回滚显示成失败"
// （钱确实动了，却告诉管理员没动，比丢一条审计更糟）。失败只打日志 —— 但必须打，
// 静默丢审计就等于没审计。
func (s *Store) RecordBalanceAudit(a *BalanceAudit) {
	if a == nil {
		return
	}
	if a.CreatedAt == 0 {
		a.CreatedAt = time.Now().Unix()
	}
	if a.Result == "" {
		a.Result = auditOK
	}
	if _, err := s.db.Exec(`INSERT INTO balance_audits
		(user_id, username, admin_id, admin_name, delta_micro, before_micro, after_micro,
		 reason, result, error, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		a.UserID, a.Username, a.AdminID, a.AdminName, a.DeltaMicro,
		a.BeforeMicro, a.AfterMicro, a.Reason, a.Result, a.Error, a.CreatedAt); err != nil {
		log.Printf("WARN: [audit] 余额变动审计写入失败（账目已变更，审计缺失）: %v", err)
	}
}

// ListBalanceAudits 倒序列出审计记录；userID>0 时只看该用户。
func (s *Store) ListBalanceAudits(userID int64, limit int) ([]*BalanceAudit, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT id, user_id, username, admin_id, admin_name, delta_micro,
		before_micro, after_micro, reason, result, error, created_at FROM balance_audits`
	var args []any
	if userID > 0 {
		q += ` WHERE user_id = ?`
		args = append(args, userID)
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*BalanceAudit{}
	for rows.Next() {
		var a BalanceAudit
		if err := rows.Scan(&a.ID, &a.UserID, &a.Username, &a.AdminID, &a.AdminName,
			&a.DeltaMicro, &a.BeforeMicro, &a.AfterMicro, &a.Reason, &a.Result,
			&a.Error, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &a)
	}
	return out, rows.Err()
}

// CountBalanceAudits 审计总条数（列表页角标/上限检查用）。
func (s *Store) CountBalanceAudits() (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM balance_audits`).Scan(&n)
	return n, err
}

// handleAdminBalanceAudits GET /api/admin/balance-audits[?user_id=&limit=]
//
// 用 user_id 参数支持"只看某个人"：排查纠纷时几乎总是从一个具体的人开始，
// 而不是从全站流水里翻。
func (a *app) handleAdminBalanceAudits(w http.ResponseWriter, r *http.Request) {
	var userID int64
	if s := strings.TrimSpace(r.URL.Query().Get("user_id")); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "user_id 非法"})
			return
		}
		userID = n
	}
	limit := 100
	if s := strings.TrimSpace(r.URL.Query().Get("limit")); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			limit = n
		}
	}
	rows, err := a.store.ListBalanceAudits(userID, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	rate, _ := a.withdrawParams()
	writeJSON(w, http.StatusOK, map[string]any{
		"audits": rows,
		// 金额形态一并给出（点 / 元）：这一页是"钱的凭据"，两套口径都要能对得上。
		"rate": rate,
	})
}
