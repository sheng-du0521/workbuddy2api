// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// announce.go 站内公告（v0.17.8）。
//
// 需求：用户控制台弹窗公告 + 概览页公告卡片；管理后台可创建 / 编辑 / 启用停用 / 删除。
//
// ---------------------------------------------------------------------------
// 展示时机与触发条件（明确定义，避免「什么时候会弹」说不清）
// ---------------------------------------------------------------------------
//
//	· 弹窗：用户控制台**每次页面加载**拉一次启用中的公告，对「本地未记录过」
//	  的公告逐条弹出；已读 id 记在 localStorage（wb_ann_seen），因此**同一条
//	  公告只弹一次**，刷新 / 重新登录都不会重复打扰。清空浏览器数据后会再弹一次
//	  —— 这是刻意取舍：公告是「送达」语义，浏览器侧已读即视为送达。
//	· 概览卡片：**始终**展示启用中的公告（与已读状态无关），置顶的排最前、
//	  至多 20 条；这样用户想回看时随时能找到。
//	· 触发条件：仅 enabled=1 **且落在有效时限内**的公告对外可见；停用 / 到期 /
//	  未开始都会立即从两侧（卡片 + 弹窗）消失。
//	· 排序：pinned（置顶）优先，其次 created_at 倒序。
//
// ---------------------------------------------------------------------------
// 有效时限与置顶的交互（v0.22.0，优先级规则的单一定义处）
// ---------------------------------------------------------------------------
//
// 两个设置**互不干涉、优先级明确**：
//
//	① 有效时限（start_at / end_at）—— 决定「**能不能被看到**」（硬门槛）
//	② 置顶（pinned）              —— 决定「可见时**排多前**」（软排序）
//
// **时效 > 置顶**：过期或未开始的公告一律不可见，无论是否置顶；置顶只在「当前
// 可见集合」内部把它排到最前。置顶**不带**自己的时限，也不需要 —— 想让它下线
// 就改时限或停用，一个开关管到底，避免两套时间互相打架。
//
// 状态机（纯函数 announceWindowState，四态互斥）：
//
//	enabled=0                        → 已停用（不看时间窗）
//	enabled=1 且 now < start_at       → 待生效（两侧都不显示）
//	enabled=1 且 now ≥ end_at         → 已过期（两侧都不显示）
//	其余（在窗内 / 未设时限）          → 生效中（可见）
//
// 时间窗是**左闭右开** [start_at, end_at)：start_at 时刻生效、end_at 时刻失效。
// start_at=0 表示「立即生效」，end_at=0 表示「永不过期」（两者都 0 = 长期有效）。
//
// 「到期仅隐藏、不改数据」：到期**不**回写 enabled=0。原因是发布者的本意是
// 「启用这条公告，只是在某个时间段内有效」，把 enabled 改掉会丢失这个意图，
// 事后也无法区分「手动停用」与「到期失效」。故到期只是一个**查询期过滤** ——
// 延长时限或清空结束时间即可原样恢复，无需重新启用。
//
// ---------------------------------------------------------------------------
// 内容约束（超限直接 400，不做静默截断 —— 静默截断会让管理员以为写进去了）
// ---------------------------------------------------------------------------
//
//	· 标题：trim 后 1~60 字符；
//	· 正文：trim 后 1~1000 字符；
//	· level：info / warn / important 三选一（影响卡片配色，unknown 回落 info）；
//	· 总量：最多 announceMaxTotal 条，超出拒绝创建（列表接口要一次回全量，
//	  不设上限等于让一个后台接口的响应体无界增长）。
package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	announceMaxTitle   = 60
	announceMaxContent = 1000
	announceMaxTotal   = 100
	// announceUserLimit 用户侧一次最多返回多少条（概览卡片宽度与注意力都有限）。
	announceUserLimit = 20
)

// announceMaxWindowYears 有效时限的**上限**（v0.22.0）。
//
// 为什么设上限：时间以 Unix 秒提交，手滑把「天」当「秒」或多数几位零，会得到
// 一个几百亿秒后的结束时间 —— 那本质上等于「设了个永不过期的公告」，而管理员
// 以为自己设了时限。10 年足够覆盖任何真实运营场景，超出即报错而非静默接受。
const announceMaxWindowYears = 10

// 公告的**时间窗状态**（v0.22.0，四态互斥，由 announceWindowState 派生）。
const (
	annStateDisabled = "disabled" // 已停用（enabled=0）
	annStatePending  = "pending"  // 待生效（未到开始时间）
	annStateActive   = "active"   // 生效中（在有效期内，或未设时限）
	annStateExpired  = "expired"  // 已过期（已过结束时间）
)

// announceStateLabel 状态的中文名（管理端标签、前端提示共用）。
func announceStateLabel(s string) string {
	switch s {
	case annStateDisabled:
		return "已停用"
	case annStatePending:
		return "待生效"
	case annStateExpired:
		return "已过期"
	default:
		return "生效中"
	}
}

// announceWindowState 公告时间窗状态机（纯函数，便于单测）。
//
// 优先级：**停用 > 未开始 > 已过期 > 生效**。时间窗为左闭右开 [startAt, endAt)：
// startAt 时刻生效、endAt 时刻失效；0 表示该侧不限（立即生效 / 永不过期）。
//
// 与 SQL 过滤口径必须一致（见 ListAnnouncements 的 WHERE 子句）：
//
//	可见 ⇔ enabled=1 ∧ (start_at=0 ∨ start_at≤now) ∧ (end_at=0 ∨ end_at>now)
//
// 两处改一处必改另一处 —— 有单测（TestAnnounceWindowStateFilterParity）锁定一致性。
func announceWindowState(enabled bool, startAt, endAt int64, now time.Time) string {
	if !enabled {
		return annStateDisabled
	}
	t := now.Unix()
	if startAt > 0 && t < startAt {
		return annStatePending
	}
	if endAt > 0 && t >= endAt {
		return annStateExpired
	}
	return annStateActive
}

// fmtUnixMinute Unix 秒 → 「2006-01-02 15:04」（**本地时区**）。
// 展示用，故跟管理员/用户所在时区一致，而不是 UTC。
func fmtUnixMinute(ts int64) string {
	return time.Unix(ts, 0).Format("2006-01-02 15:04")
}

// announceTimeWindowText 时间窗的可读文本。
//
//	两端都不限 → 长期有效
//	仅开始      → 自 <时间> 起
//	仅结束      → 至 <时间> 止
//	两端都有    → <开始> ~ <结束>
func announceTimeWindowText(startAt, endAt int64) string {
	switch {
	case startAt == 0 && endAt == 0:
		return "长期有效"
	case startAt == 0:
		return "至 " + fmtUnixMinute(endAt) + " 止"
	case endAt == 0:
		return "自 " + fmtUnixMinute(startAt) + " 起"
	default:
		return fmtUnixMinute(startAt) + " ~ " + fmtUnixMinute(endAt)
	}
}

// validateAnnounceWindow 有效时限的合法性校验（v0.22.0）。
//
//	· 非负（负数会被 SQLite 当普通整数存进来，但语义无意义）
//	· 两端都填时 start < end —— 否则是个零宽/倒置的窗口，永远看不到
//	· 两端都不得超过 now + announceMaxWindowYears（防手滑把秒当毫秒）
//
// 只填一端是合法的：仅有结束 = 「立即生效、到点失效」；仅有开始 = 「到点生效、
// 永不过期」。两者都不填 = 长期有效。
func validateAnnounceWindow(startAt, endAt int64, now time.Time) error {
	if startAt < 0 || endAt < 0 {
		return errors.New("有效时限不能为负数")
	}
	maxTs := now.AddDate(announceMaxWindowYears, 0, 0).Unix()
	if startAt > maxTs || endAt > maxTs {
		return errors.New("有效时限超出上限（最远 " + strconv.Itoa(announceMaxWindowYears) + " 年）")
	}
	if startAt > 0 && endAt > 0 && startAt >= endAt {
		return errors.New("结束时间必须晚于开始时间")
	}
	return nil
}

// announceLevels 允许的级别（顺序即前端选择器顺序）。
var announceLevels = []string{"info", "warn", "important"}

func normalizeAnnounceLevel(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	for _, l := range announceLevels {
		if v == l {
			return v
		}
	}
	return "info"
}

// Announcement 一条公告。
type Announcement struct {
	ID      int64  `json:"id"`
	Title   string `json:"title"`
	Content string `json:"content"`
	Level   string `json:"level"`
	Enabled bool   `json:"enabled"`
	Pinned  bool   `json:"pinned"`
	// StartAt / EndAt 有效时限（v0.22.0，Unix 秒；**0 = 该侧不限**）。
	// 时间窗左闭右开 [StartAt, EndAt)：到 EndAt 即失效（不进用户侧列表、不弹窗）。
	StartAt   int64 `json:"start_at"`
	EndAt     int64 `json:"end_at"`
	CreatedAt int64 `json:"created_at"`
	UpdatedAt int64 `json:"updated_at"`

	// 以下三个是**派生视图字段**（不落库，由 applyState 按查询时刻计算）：
	//   state       四态之一（disabled/pending/active/expired），供前端做标签与灰显
	//   state_label 中文名（已停用 / 待生效 / 生效中 / 已过期）
	//   time_window 可读的时间窗文本（长期有效 / 至 … 止 / … ~ …）
	State      string `json:"state"`
	StateLabel string `json:"state_label"`
	TimeWindow string `json:"time_window"`
}

// applyState 按给定时刻派生展示字段。所有查询出口都调它，保证「拿到的」与
// 「库里存的」口径一致 —— 否则前端得自己算时间窗，两处逻辑迟早跑偏。
func (a *Announcement) applyState(now time.Time) *Announcement {
	a.State = announceWindowState(a.Enabled, a.StartAt, a.EndAt, now)
	a.StateLabel = announceStateLabel(a.State)
	a.TimeWindow = announceTimeWindowText(a.StartAt, a.EndAt)
	return a
}

// Visible 当前是否对用户可见（等价于 state == active）。
// 是 SQL 过滤的 Go 侧镜像，供单测做「双口径一致性」校验。
func (a *Announcement) Visible(now time.Time) bool {
	return announceWindowState(a.Enabled, a.StartAt, a.EndAt, now) == annStateActive
}

// ---------- Store ----------

// migrateAnnouncements 建表（幂等）。
func (s *Store) migrateAnnouncements() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS announcements (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			title      TEXT NOT NULL DEFAULT '',
			content    TEXT NOT NULL DEFAULT '',
			level      TEXT NOT NULL DEFAULT 'info',
			enabled    INTEGER NOT NULL DEFAULT 1,
			pinned     INTEGER NOT NULL DEFAULT 0,
			start_at   INTEGER NOT NULL DEFAULT 0,
			end_at     INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_announce_enabled ON announcements(enabled, pinned, created_at)`,
	}
	for _, st := range stmts {
		if _, err := s.db.Exec(st); err != nil {
			return err
		}
	}
	// 老库增量列（v0.22.0）：有效时限。默认 0 = 不限，故升级后所有历史公告
	// 都是「长期有效」，行为与升级前完全一致（不会突然集体消失）。
	for _, c := range []struct{ col, ddl string }{
		{"start_at", "start_at INTEGER NOT NULL DEFAULT 0"},
		{"end_at", "end_at INTEGER NOT NULL DEFAULT 0"},
	} {
		if err := s.ensureColumn("announcements", c.col, c.ddl); err != nil {
			return fmt.Errorf("migrate announcements column %s: %w", c.col, err)
		}
	}
	return nil
}

const announceCols = `id, title, content, level, enabled, pinned, start_at, end_at, created_at, updated_at`

func scanAnnouncement(row interface{ Scan(dest ...any) error }) (*Announcement, error) {
	var a Announcement
	var enabled, pinned int
	if err := row.Scan(&a.ID, &a.Title, &a.Content, &a.Level, &enabled, &pinned,
		&a.StartAt, &a.EndAt, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return nil, err
	}
	a.Enabled = enabled != 0
	a.Pinned = pinned != 0
	return &a, nil
}

// ListAnnouncements 列出公告。onlyEnabled=true 时只回启用中的（用户侧口径）。
// 排序：置顶优先，其次创建时间倒序（id 倒序，避免同秒创建时顺序抖动）。
func (s *Store) ListAnnouncements(onlyEnabled bool, limit int) ([]*Announcement, error) {
	if limit <= 0 {
		limit = announceUserLimit
	}
	now := time.Now()
	q := `SELECT ` + announceCols + ` FROM announcements`
	args := []any{}
	if onlyEnabled {
		// 用户侧口径（v0.22.0）：只回「当前可见」的 —— 启用中 + 已到开始时间 +
		// 未到结束时间。左闭右开，与 announceWindowState 完全同口径
		// （改这里必同步改那个纯函数，有 TestAnnounceWindowStateFilterParity 兜底）。
		//
		// 过滤放在 SQL 而不是 Go 侧：既省掉把过期公告读出来再丢掉的开销，也保证
		// 「limit 截断」是在**可见集合**上做的 —— 否则 100 条历史公告里前 20 条
		// 恰好全过期，用户侧就会拿到一个空列表。
		q += ` WHERE enabled = 1 AND (start_at = 0 OR start_at <= ?) AND (end_at = 0 OR end_at > ?)`
		args = append(args, now.Unix(), now.Unix())
	}
	// 排序：置顶优先（**仅影响无时限约束的相对次序**），其次 id 倒序（同秒创建不抖动）。
	q += ` ORDER BY pinned DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Announcement{}
	for rows.Next() {
		a, err := scanAnnouncement(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a.applyState(now))
	}
	return out, rows.Err()
}

// CountAnnouncements 公告总数（创建前做上限检查）。
func (s *Store) CountAnnouncements() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM announcements`).Scan(&n)
	return n, err
}

// CreateAnnouncement 新建公告，返回落库后的实体（含 id）。
func (s *Store) CreateAnnouncement(title, content, level string, enabled, pinned bool, startAt, endAt int64) (*Announcement, error) {
	now := time.Now().Unix()
	res, err := s.db.Exec(
		`INSERT INTO announcements (title, content, level, enabled, pinned, start_at, end_at, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		title, content, level, b2i(enabled), b2i(pinned), startAt, endAt, now, now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.GetAnnouncement(id)
}

// GetAnnouncement 按 id 取一条。
func (s *Store) GetAnnouncement(id int64) (*Announcement, error) {
	a, err := scanAnnouncement(s.db.QueryRow(`SELECT `+announceCols+` FROM announcements WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return a.applyState(time.Now()), nil
}

// UpdateAnnouncement 全量更新一条（标题 / 正文 / 级别 / 置顶）。
// enabled 单独走 SetAnnouncementEnabled —— 列表里的启停开关是高频轻操作，
// 不该把整条内容一起提交（否则并发编辑会互相覆盖）。
func (s *Store) UpdateAnnouncement(id int64, title, content, level string, pinned bool, startAt, endAt int64) error {
	res, err := s.db.Exec(
		`UPDATE announcements SET title = ?, content = ?, level = ?, pinned = ?,
			start_at = ?, end_at = ?, updated_at = ?
		 WHERE id = ?`,
		title, content, level, b2i(pinned), startAt, endAt, time.Now().Unix(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetAnnouncementEnabled 启用 / 停用。
func (s *Store) SetAnnouncementEnabled(id int64, enabled bool) error {
	res, err := s.db.Exec(`UPDATE announcements SET enabled = ?, updated_at = ? WHERE id = ?`,
		b2i(enabled), time.Now().Unix(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteAnnouncement 删除。
func (s *Store) DeleteAnnouncement(id int64) error {
	res, err := s.db.Exec(`DELETE FROM announcements WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// b2i bool → 0/1（SQLite 无布尔类型）。
func b2i(v bool) int {
	if v {
		return 1
	}
	return 0
}

// ---------- 校验 ----------

// validateAnnouncement 内容约束校验。返回中文原因供前端直接展示。
func validateAnnouncement(title, content string) error {
	if title == "" {
		return errors.New("标题不能为空")
	}
	if len([]rune(title)) > announceMaxTitle {
		return errors.New("标题最多 " + strconv.Itoa(announceMaxTitle) + " 个字")
	}
	if content == "" {
		return errors.New("正文不能为空")
	}
	if len([]rune(content)) > announceMaxContent {
		return errors.New("正文最多 " + strconv.Itoa(announceMaxContent) + " 个字")
	}
	return nil
}

// ---------- Handlers ----------

// handleUserAnnouncements 用户侧公告列表（仅启用中）。
// GET /api/announcements
func (a *app) handleUserAnnouncements(w http.ResponseWriter, r *http.Request) {
	list, err := a.store.ListAnnouncements(true, announceUserLimit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"announcements": list,
		"count":         len(list),
		// 前端可把旧 id 一并传上来做「已读」判定；这里只给当前可见集合。
		//
		// 注：本接口**只回当前可见**（生效中）的公告，故每条 state 恒为 active、
		// 过期的与未开始的都不会出现在这里 —— 弹窗因此天然不会弹到过期公告。
		"limits": map[string]any{
			"title":   announceMaxTitle,
			"content": announceMaxContent,
		},
	})
}

// handleAdminListAnnouncements 管理侧全量列表（含停用）。
// GET /api/admin/announcements
func (a *app) handleAdminListAnnouncements(w http.ResponseWriter, r *http.Request) {
	list, err := a.store.ListAnnouncements(false, announceMaxTotal)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// 状态计数（v0.22.0）：管理端摘要行直接展示「生效 / 待生效 / 已过期 / 已停用」，
	// 不必让管理员逐条看时间推状态。
	byState := map[string]int{}
	for _, a := range list {
		byState[a.State]++
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"announcements": list,
		"count":         len(list),
		"by_state":      byState,
		"limits": map[string]any{
			"title":   announceMaxTitle,
			"content": announceMaxContent,
			"total":   announceMaxTotal,
		},
	})
}

// announceRequest 创建 / 编辑的请求体。
//
// 三个可选字段都用**指针**，目的是区分「未提供」与「显式设为 0/false」：
//
//	· Pinned  —— 编辑时只改标题不该把置顶清掉
//	· StartAt —— 未提供 = 保持原值；显式 0 = 清空（改为「立即生效」）
//	· EndAt   —— 未提供 = 保持原值；显式 0 = 清空（改为「永不过期」）
//
// 时间值是 **Unix 秒**（前端由 datetime-local 换算后提交）。指针语义让「只改正文」
// 这类部分更新不会误伤时限 —— 老前端（不带这两个字段）编辑也安全。
type announceRequest struct {
	Title   string `json:"title"`
	Content string `json:"content"`
	Level   string `json:"level"`
	Pinned  *bool  `json:"pinned"`
	StartAt *int64 `json:"start_at"`
	EndAt   *int64 `json:"end_at"`
}

// resolveAnnounceWindow 把请求里的可时限字段与「当前值」合并，得到最终时限。
//
//	provided == nil  → 用 cur（保持原值）
//	provided != nil  → 用 *provided（显式覆盖，0 表示清空该侧）
//
// 复用同一段逻辑，创建（cur 全 0）与编辑共用，避免两处各写一遍合并规则。
func resolveAnnounceWindow(curStart, curEnd int64, req announceRequest) (int64, int64) {
	startAt, endAt := curStart, curEnd
	if req.StartAt != nil {
		startAt = *req.StartAt
	}
	if req.EndAt != nil {
		endAt = *req.EndAt
	}
	return startAt, endAt
}

// announceVisibilityHint 按状态给一句「用户侧现在能不能看到」的提示文案，
// 供管理端 toast 直接在发布/保存后告知，而不是让管理员自己推时间。
func announceVisibilityHint(state string) string {
	switch state {
	case annStateActive:
		return "用户侧立即可见"
	case annStatePending:
		return "已发布，但未到开始时间 —— 用户侧暂时不可见"
	case annStateExpired:
		return "已保存，但结束时间已过 —— 用户侧不可见"
	case annStateDisabled:
		return "已保存（当前为停用状态，用户侧不可见）"
	default:
		return ""
	}
}

// handleAdminCreateAnnouncement 新建公告。
// POST /api/admin/announcements
func (a *app) handleAdminCreateAnnouncement(w http.ResponseWriter, r *http.Request) {
	var req announceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	title := strings.TrimSpace(req.Title)
	content := strings.TrimSpace(req.Content)
	if err := validateAnnouncement(title, content); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	n, err := a.store.CountAnnouncements()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if n >= announceMaxTotal {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "公告总数已达上限（" + strconv.Itoa(announceMaxTotal) + " 条），请先删除历史公告"})
		return
	}
	pinned := req.Pinned != nil && *req.Pinned
	startAt, endAt := resolveAnnounceWindow(0, 0, req)
	if err := validateAnnounceWindow(startAt, endAt, time.Now()); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	item, err := a.store.CreateAnnouncement(title, content, normalizeAnnounceLevel(req.Level), true, pinned, startAt, endAt)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"announcement": item,
		// 状态提示：让管理员知道这条现在是否真的能被用户看到（v0.22.0）。
		"state":       item.State,
		"state_label": item.StateLabel,
		"hint":        announceVisibilityHint(item.State),
	})
}

// handleAdminUpdateAnnouncement 编辑公告（内容 / 级别 / 置顶）。
// POST /api/admin/announcements/{id}
func (a *app) handleAdminUpdateAnnouncement(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "非法 id"})
		return
	}
	cur, err := a.store.GetAnnouncement(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "公告不存在"})
		return
	}
	var req announceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	title := strings.TrimSpace(req.Title)
	content := strings.TrimSpace(req.Content)
	if err := validateAnnouncement(title, content); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	pinned := cur.Pinned
	if req.Pinned != nil {
		pinned = *req.Pinned
	}
	// 时限合并：未提供则保持原值（见 resolveAnnounceWindow 注释）。
	startAt, endAt := resolveAnnounceWindow(cur.StartAt, cur.EndAt, req)
	if err := validateAnnounceWindow(startAt, endAt, time.Now()); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if err := a.store.UpdateAnnouncement(id, title, content, normalizeAnnounceLevel(req.Level), pinned, startAt, endAt); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	item, err := a.store.GetAnnouncement(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"announcement": item,
		"state":        item.State,
		"state_label":  item.StateLabel,
		"hint":         announceVisibilityHint(item.State),
	})
}

// handleAdminToggleAnnouncement 启用 / 停用。
// POST /api/admin/announcements/{id}/toggle  body: {"enabled":true}
func (a *app) handleAdminToggleAnnouncement(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "非法 id"})
		return
	}
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Enabled == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少 enabled 字段"})
		return
	}
	if err := a.store.SetAnnouncementEnabled(id, *req.Enabled); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "公告不存在"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "enabled": *req.Enabled})
}

// handleAdminDeleteAnnouncement 删除。
// DELETE /api/admin/announcements/{id}
func (a *app) handleAdminDeleteAnnouncement(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "非法 id"})
		return
	}
	if err := a.store.DeleteAnnouncement(id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "公告不存在"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}
