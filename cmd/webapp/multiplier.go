// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ======================================================================
// 计费倍率的「变更历史 + 可视化 + 自动公告」（v0.27.0）
//
// 倍率（点 / 1 号池积分）是用户实际感受到的**价格**。它此前只有一个当前值，
// 改过之后：
//   - 用户只看到"这个月贵了"，不知道什么时候变的、变了多少 —— 而涨价不给理由
//     是最容易失去信任的一件事；
//   - 管理员自己也说不清"上次调是什么时候"；
//   - 涨价这种事靠人记得发公告，就会忘。
//
// 本文件解决三件事：
//  1. 每次变更落一条历史（时间 / 调整前后 / 调整人 / 备注 / 是否已公告）；
//  2. 变更后**自动**向用户侧发一条带时限的公告（临时期限见 announceTTL）；
//  3. 给两侧提供可读的变更序列（后台含调整人，用户侧不含 —— 不暴露管理员账号）。
// ======================================================================

// MultiplierChange 一次计费倍率调整。
type MultiplierChange struct {
	ID        int64   `json:"id"`
	OldValue  float64 `json:"old_value"`
	NewValue  float64 `json:"new_value"`
	AdminID   int64   `json:"admin_id"`
	AdminName string  `json:"admin_name"`
	Note      string  `json:"note"`
	// Announced 是否已自动发出用户侧公告。改成 false 时必须能看出"这次没公告"，
	// 否则用户会在不知情的情况下被涨价 —— 而那正是本功能要避免的事。
	Announced  bool  `json:"announced"`
	AnnounceID int64 `json:"announce_id"`
	CreatedAt  int64 `json:"created_at"`
}

// ChangePercent 相对变化（正 = 涨价，负 = 降价）。old 为 0 时返回 0（无比较基准）。
func (m *MultiplierChange) ChangePercent() float64 {
	if m == nil || m.OldValue == 0 {
		return 0
	}
	return (m.NewValue - m.OldValue) / m.OldValue * 100
}

// Direction 变化方向：up / down / flat。前端据此定色（红涨绿跌，与站内其余口径一致）。
func (m *MultiplierChange) Direction() string {
	if m == nil {
		return "flat"
	}
	switch {
	case m.NewValue > m.OldValue:
		return "up"
	case m.NewValue < m.OldValue:
		return "down"
	}
	return "flat"
}

const migrationMultiplierHistory = `CREATE TABLE IF NOT EXISTS multiplier_history (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	old_value   REAL    NOT NULL DEFAULT 0,
	new_value   REAL    NOT NULL DEFAULT 0,
	admin_id    INTEGER NOT NULL DEFAULT 0,
	admin_name  TEXT    NOT NULL DEFAULT '',
	note        TEXT    NOT NULL DEFAULT '',
	announced   INTEGER NOT NULL DEFAULT 0,
	announce_id INTEGER NOT NULL DEFAULT 0,
	created_at  INTEGER NOT NULL DEFAULT 0
)`

// announceTTL 倍率变更公告的默认有效期。
//
// 为什么给时限而不是长期挂着：这是一条**事件通知**，不是常驻规则说明。
// 长期挂着会让公告栏里堆满历史调价，用户对新公告的敏感度会迅速下降
// （"反正都是些没用的"），反而让真正重要的通知被忽略。
const announceTTL = 7 * 24 * time.Hour

// EnsureMultiplierHistory 建表（幂等，由 migrate 调用）。
func (s *Store) EnsureMultiplierHistory() error {
	if _, err := s.db.Exec(migrationMultiplierHistory); err != nil {
		return err
	}
	_, err := s.db.Exec(
		`CREATE INDEX IF NOT EXISTS idx_multiplier_history_created ON multiplier_history(created_at DESC)`)
	return err
}

// RecordMultiplierChange 落一条倍率变更历史，返回记录（含 ID）。
func (s *Store) RecordMultiplierChange(oldV, newV float64, adminID int64, adminName, note string) (*MultiplierChange, error) {
	now := time.Now().Unix()
	res, err := s.db.Exec(`INSERT INTO multiplier_history
		(old_value, new_value, admin_id, admin_name, note, announced, announce_id, created_at)
		VALUES (?,?,?,?,?,0,0,?)`,
		oldV, newV, adminID, strings.TrimSpace(adminName), strings.TrimSpace(note), now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &MultiplierChange{
		ID: id, OldValue: oldV, NewValue: newV,
		AdminID: adminID, AdminName: strings.TrimSpace(adminName),
		Note: strings.TrimSpace(note), CreatedAt: now,
	}, nil
}

// MarkMultiplierAnnounced 标记某条变更已发出公告。
func (s *Store) MarkMultiplierAnnounced(id, announceID int64) error {
	_, err := s.db.Exec(
		`UPDATE multiplier_history SET announced = 1, announce_id = ? WHERE id = ?`, announceID, id)
	return err
}

// ListMultiplierHistory 倒序列出倍率变更历史（最近 limit 条）。
func (s *Store) ListMultiplierHistory(limit int) ([]*MultiplierChange, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT id, old_value, new_value, admin_id, admin_name,
		note, announced, announce_id, created_at FROM multiplier_history
		ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*MultiplierChange{}
	for rows.Next() {
		var m MultiplierChange
		var announced int
		if err := rows.Scan(&m.ID, &m.OldValue, &m.NewValue, &m.AdminID, &m.AdminName,
			&m.Note, &announced, &m.AnnounceID, &m.CreatedAt); err != nil {
			return nil, err
		}
		m.Announced = announced != 0
		out = append(out, &m)
	}
	return out, rows.Err()
}

// ---------- 自动公告 ----------

// multiplierAnnounce 组织一条倍率变更公告的标题与正文。
//
// 抽成纯函数是为了可测：文案里"涨/降"的判断、百分比的取整方向、以及"没有备注时
// 不多出一行空行"这些细节，靠人眼 review 很容易漏（而它发出去就是全站可见）。
func multiplierAnnounce(oldV, newV float64, note string) (title, content, level string) {
	up := newV > oldV
	arrow := "上调"
	if !up {
		arrow = "下调"
	}
	title = fmt.Sprintf("计费倍率%s通知", arrow)

	pct := 0.0
	if oldV > 0 {
		pct = (newV - oldV) / oldV * 100
		// 小于 0.1% 的变化显示成 0.0% 会让人以为"没变"，退化为整数位描述。
		if math.Abs(pct) < 0.05 {
			pct = 0
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "计费倍率已由 %.4g 调整为 %.4g（点 / 1 号池积分）。\n", oldV, newV)
	if pct != 0 {
		fmt.Fprintf(&b, "相对变化：%s%.1f%%。\n", map[bool]string{true: "+", false: ""}[pct > 0], pct)
	}
	b.WriteString("调整后，同样的用量将按新倍率扣点；已完成结算的历史用量不受影响。")
	if n := strings.TrimSpace(note); n != "" {
		b.WriteString("\n调整说明：")
		b.WriteString(n)
	}
	// 涨价用 warn（需要留意），降价用 info（好消息不必吓人）。
	level = "info"
	if up {
		level = "warn"
	}
	return title, b.String(), level
}

// recordMultiplierChange 记一条变更历史并**自动**发公告。
//
// 顺序：先落历史（拿到 ID），再发公告，再回填 announce_id。这样即使公告失败，
// 历史里也留得下"这次没公告出去"（announced=false），后台一眼可见 ——
// 比"两条都没留"好得多。
func (a *app) recordMultiplierChange(oldV, newV float64, admin *User, note string) *MultiplierChange {
	var adminID int64
	var adminName string
	if admin != nil {
		adminID, adminName = admin.ID, admin.Username
	}
	m, err := a.store.RecordMultiplierChange(oldV, newV, adminID, adminName, note)
	if err != nil {
		log.Printf("WARN: [multiplier] 倍率变更历史写入失败：%v", err)
		return nil
	}
	log.Printf("INFO: [multiplier] 计费倍率 %g → %g（操作人 %s）", oldV, newV, adminName)
	a.announceMultiplierChange(m)
	return m
}

// announceMultiplierChange 倍率变更后**自动**发一条用户侧公告（v0.27.0）。
//
// 为什么是自动而不是"提醒管理员去发"：靠人记得就会忘，而涨价不通知是最伤信任的
// 一种做法。这里刻意**不**让公告失败影响倍率写入 —— 倍率已经生效，把接口整个
// 报错只会让管理员以为"没改成功"而重复操作；失败会打 WARN，并把
// announced=false 留在历史里（后台一眼能看出"这次没公告出去"）。
func (a *app) announceMultiplierChange(m *MultiplierChange) {
	if m == nil || a == nil || a.store == nil {
		return
	}
	title, content, level := multiplierAnnounce(m.OldValue, m.NewValue, m.Note)
	now := time.Now()
	ann, err := a.store.CreateAnnouncement(title, content, level, true, false,
		now.Unix(), now.Add(announceTTL).Unix())
	if err != nil {
		fmt.Printf("WARN: [multiplier] 倍率变更公告创建失败（倍率已生效，公告缺失）：%v\n", err)
		return
	}
	if err := a.store.MarkMultiplierAnnounced(m.ID, ann.ID); err != nil {
		fmt.Printf("WARN: [multiplier] 公告已发出但历史标记失败：%v\n", err)
		return
	}
	m.Announced, m.AnnounceID = true, ann.ID
}

// ---------- 接口 ----------

// handleAdminMultiplierHistory GET /api/admin/multiplier-history
//
// 返回当前倍率 + 变更历史。当前值一并给出，是为了让前端不必再拉一次设置接口
// （少一次往返就少一处"两个数字对不上"的可能）。
func (a *app) handleAdminMultiplierHistory(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if s := strings.TrimSpace(r.URL.Query().Get("limit")); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			limit = n
		}
	}
	rows, err := a.store.ListMultiplierHistory(limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"current": a.proxy.multiplierNow(),
		"history": rows,
		// 公告有效期的口径随数据一起给前端：界面上要说明"这条通知挂多久"。
		"announce_ttl_hours": int(announceTTL.Hours()),
	})
}

// handleMultiplier 用户侧倍率视图：GET /api/multiplier
//
// ⚠️ **不含 admin_name / admin_id**：调价是平台行为，向用户暴露具体管理员账号
// 既无必要，也会把内部人事信息带出去。用户需要知道的是"什么时候、从多少到多少"。
func (a *app) handleMultiplier(w http.ResponseWriter, r *http.Request) {
	rows, err := a.store.ListMultiplierHistory(20)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	type pubChange struct {
		OldValue  float64 `json:"old_value"`
		NewValue  float64 `json:"new_value"`
		Note      string  `json:"note"`
		CreatedAt int64   `json:"created_at"`
		Percent   float64 `json:"change_percent"`
		Direction string  `json:"direction"`
	}
	out := make([]pubChange, 0, len(rows))
	for _, m := range rows {
		out = append(out, pubChange{
			OldValue: m.OldValue, NewValue: m.NewValue,
			// 备注原样带出（它本来就是给用户看的说明）；但管理员的内部备注
			// 若含敏感信息也只能由管理员自己避免 —— 界面上的输入框会明确提示
			// "这段话会展示给用户"。
			Note:      m.Note,
			CreatedAt: m.CreatedAt,
			Percent:   m.ChangePercent(),
			Direction: m.Direction(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"current": a.proxy.multiplierNow(),
		"history": out,
	})
}

// validateMultiplier 校验倍率取值区间。
//
// 与设置接口共用同一判据（写在这里是为了让"唯一的合法区间"只有一个出处）。
func validateMultiplier(v float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return errors.New("计费倍率非法")
	}
	if v <= 0 || v > 1000 {
		return errors.New("计费倍率需在 0-1000 之间（点 / 1 号池积分）")
	}
	return nil
}

// multiplierNow 读当前倍率（未配置/非法时回落默认）。
//
// 走 store.PricingMultiplier（唯一口径）：界面显示的倍率必须与实际计费用的
// 是同一个函数 —— 否则改了价而界面还显示旧值，用户看到的就是"说好了 X 却扣了 Y"。
func (p *Proxy) multiplierNow() float64 {
	if p == nil || p.store == nil {
		return defaultMultiplier
	}
	return p.store.PricingMultiplier()
}
