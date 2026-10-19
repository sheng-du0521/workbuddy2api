// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// egress_scope.go 「这条出口能给哪些账号用」的**唯一闸门**（v0.31.50，需求二）。
//
// ## 规则（唯一事实）
//
// 一条**用户线路**（owner_user_id > 0）能承载某账号，当且仅当：
//
//	account.owner_user_id == line.owner_user_id   // 只能给自己的号
//	account.pool_state   != "in"                  // 已入池的号走平台反代，不许走个人出口
//	line.enabled && !line.manual_disabled         // 被停用的线路不承载任何流量
//
// 平台线路（scope=platform）不限制入池状态，行为与历史一致。
//
// ## 为什么必须是"唯一闸门"
//
// 本仓最贵的教训是"同一个错误码/同一件事存在两套纪律"（11140 误杀 30 个号那次）。
// 出口资格有四个入口：用户自助 apply、管理员批量分配、单账号改出口、反代选号（读路径）。
// 每处各写一遍判断，迟早漂移 —— 而漂移的后果是把某个人的 access_token 从**别人的
// 家庭宽带**送出去，事后只能靠日志猜。
//
// ## 宽松版（2026-10-19 用户拍板）
//
// 管理员可对「归属」「入池」两条**强制越界**，但必须带理由并落审计表；
// `manual_disabled` 与 `enabled=false` **永远不可强制** —— 那是"人的决定"，
// 强制是"越权分发"，两者混在一个开关里，停用随时会被一次批量分配掀翻。
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/egress"
)

// 线路范围取值。
const (
	// egressScopePlatform 平台线路：不限入池状态（历史行为）。
	egressScopePlatform = "platform"
	// egressScopeSelfUnpooled 用户线路：只给线路主人的、未入池的账号。
	egressScopeSelfUnpooled = "self_unpooled"
	// egressScopeOrphan 线路主人已注销：既不放给任何人，也不再自动启用。
	egressScopeOrphan = "orphan"
)

// 判定码（前端与测试断言用 code，**不要靠文案匹配**）。
const (
	egressCodeOK            = "ok"
	egressCodeNoLine        = "no_line"
	egressCodeLineDisabled  = "line_disabled"
	egressCodePlatformScope = "platform_scope"
	egressCodeForeign       = "foreign_account"
	egressCodePooled        = "account_pooled"
	// 强制越界后仍"允许"，但 code 保留越界事实，供调用方写审计。
	egressCodeForcedForeign = "forced_foreign"
	egressCodeForcedPooled  = "forced_pooled"
)

// normEgressScope 归一化线路范围；空值按归属推断（存量老库）。
func normEgressScope(raw string, owner int64) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case egressScopePlatform:
		return egressScopePlatform
	case egressScopeSelfUnpooled:
		return egressScopeSelfUnpooled
	case egressScopeOrphan:
		return egressScopeOrphan
	}
	if owner > 0 {
		return egressScopeSelfUnpooled
	}
	return egressScopePlatform
}

// egressScopeLabel 范围的中文说明（给界面用，不参与判定）。
func egressScopeLabel(scope string) string {
	switch scope {
	case egressScopeSelfUnpooled:
		return "仅本人未入池号可用"
	case egressScopeOrphan:
		return "归属已注销（停用）"
	default:
		return "平台线路（不限入池）"
	}
}

// egressAllowOpts 闸门选项。零值 = 严格校验（用户自助路径永远用零值）。
type egressAllowOpts struct {
	// Force 管理员强制越界（只放开「归属」与「入池」两条）。
	Force bool
}

// egressScopeAllows 只判「范围」（归属 + 入池），不管线路是否停用。
//
// 读路径（反代选号）用它做兜底：停用的线路**仍然按原样路由**已有账号，
// 这是既有纪律（见 EgressProxy.Enabled 的说明），不能借这次收口把它改掉。
func egressScopeAllows(line *EgressProxy, acc *Account, opts egressAllowOpts) (bool, string, string) {
	if line == nil || acc == nil {
		return true, egressCodeNoLine, ""
	}
	if normEgressScope(line.Scope, line.OwnerUserID) == egressScopePlatform {
		return true, egressCodePlatformScope, ""
	}
	if acc.OwnerUserID != line.OwnerUserID {
		if opts.Force {
			return true, egressCodeForcedForeign, fmt.Sprintf(
				"该出口属于用户 #%d，账号属于用户 #%d（已强制越界）", line.OwnerUserID, acc.OwnerUserID)
		}
		return false, egressCodeForeign, fmt.Sprintf(
			"该出口属于用户 #%d，只能给其本人的账号使用", line.OwnerUserID)
	}
	if strings.EqualFold(strings.TrimSpace(acc.PoolState), "in") {
		if opts.Force {
			return true, egressCodeForcedPooled,
				"该账号已入池（参与平台反代），使用个人出口会把平台流量转到个人宽带（已强制越界）"
		}
		return false, egressCodePooled,
			"该账号已入池，由平台统一调度，不能使用个人出口"
	}
	return true, egressCodeOK, ""
}

// egressLineAllows **唯一闸门**：范围 + 线路可用性（停用/人工停用）。
//
// 所有写入口都必须过它：用户 apply、管理员 assign、单账号改出口。
// 返回 (ok, code, reason)；Force 只影响「归属/入池」，不影响第 1 条。
func egressLineAllows(line *EgressProxy, acc *Account, opts egressAllowOpts) (bool, string, string) {
	if line != nil && (line.ManualDisabled || !line.Enabled) {
		// ⚠️ 这一条 Force 也不能越过：停用是人的决定，越权分发是另一件事。
		reason := "这条出口已被停用"
		if line.ManualDisabled {
			reason = "这条出口已被管理员停用（需管理员先解锁）"
		}
		return false, egressCodeLineDisabled, reason
	}
	return egressScopeAllows(line, acc, opts)
}

// ---------------------------------------------------------------------------
// 强制越界审计（egress_force_audits）
// ---------------------------------------------------------------------------

// egressForceAudit 一次「强制越界」的完整台账。
//
// 为什么不用日志代替：这条链路会**把凭证送进别人的家庭网络**，追责时要回答
// "谁、在什么时候、用什么理由、把哪几个号送过去了" —— 这是可查询的表，
// 不是一行会被轮转掉的日志。
type egressForceAudit struct {
	ID        int64    `json:"id"`
	AdminID   int64    `json:"admin_id"`
	AdminName string   `json:"admin_name"`
	ProxyID   string   `json:"proxy_id"`
	ProxyURL  string   `json:"proxy_url"`
	UIDs      []string `json:"uids"`
	// Violations uid → code（foreign_account / account_pooled）。
	Violations map[string]string `json:"violations"`
	Reason     string            `json:"reason"`
	Result     string            `json:"result"` // applied / partial / failed
	CreatedAt  int64             `json:"created_at"`
}

func (s *Store) migrateEgressForceAudit() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS egress_force_audits (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			admin_id    INTEGER NOT NULL DEFAULT 0,
			admin_name  TEXT NOT NULL DEFAULT '',
			proxy_id    TEXT NOT NULL DEFAULT '',
			proxy_url   TEXT NOT NULL DEFAULT '',
			uids        TEXT NOT NULL DEFAULT '[]',
			violations  TEXT NOT NULL DEFAULT '{}',
			reason      TEXT NOT NULL DEFAULT '',
			result      TEXT NOT NULL DEFAULT '',
			created_at  INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_egress_force_at ON egress_force_audits(created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_egress_force_proxy ON egress_force_audits(proxy_id)`,
	}
	for _, st := range stmts {
		if _, err := s.db.Exec(st); err != nil {
			return fmt.Errorf("migrate egress_force_audits: %w", err)
		}
	}
	return nil
}

// InsertEgressForceAudit 落一条审计（uuid 列表与 violations 一次性写入）。
func (s *Store) InsertEgressForceAudit(a *egressForceAudit) error {
	if a == nil {
		return nil
	}
	if a.CreatedAt == 0 {
		a.CreatedAt = time.Now().Unix()
	}
	uids, _ := json.Marshal(a.UIDs)
	viol, _ := json.Marshal(a.Violations)
	if len(uids) == 0 {
		uids = []byte("[]")
	}
	if len(viol) == 0 {
		viol = []byte("{}")
	}
	res, err := s.db.Exec(`INSERT INTO egress_force_audits
		(admin_id, admin_name, proxy_id, proxy_url, uids, violations, reason, result, created_at)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		a.AdminID, a.AdminName, a.ProxyID, a.ProxyURL, string(uids), string(viol),
		a.Reason, a.Result, a.CreatedAt)
	if err != nil {
		return err
	}
	a.ID, _ = res.LastInsertId()
	return nil
}

// ListEgressForceAudits 最近的强制越界记录（可按线路过滤）。
func (s *Store) ListEgressForceAudits(limit int, proxyID string) ([]*egressForceAudit, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	q := `SELECT id, admin_id, admin_name, proxy_id, proxy_url, uids, violations,
		reason, result, created_at FROM egress_force_audits`
	args := []any{}
	if strings.TrimSpace(proxyID) != "" {
		q += ` WHERE proxy_id = ?`
		args = append(args, strings.TrimSpace(proxyID))
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*egressForceAudit
	for rows.Next() {
		var a egressForceAudit
		var uids, viol string
		if err := rows.Scan(&a.ID, &a.AdminID, &a.AdminName, &a.ProxyID, &a.ProxyURL,
			&uids, &viol, &a.Reason, &a.Result, &a.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(uids), &a.UIDs)
		_ = json.Unmarshal([]byte(viol), &a.Violations)
		out = append(out, &a)
	}
	return out, rows.Err()
}

// handleEgressForceAudits GET /api/admin/egress/force-audits?proxy_id=&limit=
func (a *app) handleEgressForceAudits(w http.ResponseWriter, r *http.Request) {
	list, err := a.store.ListEgressForceAudits(egressAtoi(r.URL.Query().Get("limit"), 20),
		r.URL.Query().Get("proxy_id"))
	if list == nil {
		list = []*egressForceAudit{} // 空列表要是 []，前端才不用为 null 写分支
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"audits": list})
}

// ---------------------------------------------------------------------------
// 越界存量（compliance）
// ---------------------------------------------------------------------------

// egressViolation 一条"现有配置违反范围规则"的记录。
type egressViolation struct {
	UID       string `json:"uid"`
	Nickname  string `json:"nickname"`
	OwnerID   int64  `json:"owner_id"`
	PoolState string `json:"pool_state"`
	ProxyID   string `json:"proxy_id"`
	ProxyURL  string `json:"proxy_url"`
	LineOwner int64  `json:"line_owner"`
	Code      string `json:"code"`
	Reason    string `json:"reason"`
}

// egressCompliance 扫描存量越界。
//
// 为什么只报不改：自动迁移会**直接断流量**（一批号突然换出口，上游可能立刻风控）。
// 正确姿势是让人看见清单，然后显式点"改回默认"（dry_run 先预览）。
func (a *app) egressCompliance() ([]egressViolation, error) {
	proxies, err := a.store.ListEgressProxies()
	if err != nil {
		return nil, err
	}
	byURL := map[string]*EgressProxy{}
	for _, p := range procsNonNil(proxies) {
		canon, cerr := egress.Normalize(p.URL)
		if cerr != nil {
			continue
		}
		byURL[canon] = p
	}
	accs, err := a.store.ListAccounts()
	if err != nil {
		return nil, err
	}
	out := make([]egressViolation, 0)
	for _, acc := range accs {
		if acc == nil {
			continue
		}
		raw := strings.TrimSpace(acc.EgressProxy)
		if raw == "" || egress.IsDirect(raw) {
			continue
		}
		canon, cerr := egress.Normalize(raw)
		if cerr != nil {
			continue
		}
		line := byURL[canon]
		if line == nil {
			continue
		}
		if ok, code, reason := egressScopeAllows(line, acc, egressAllowOpts{}); ok {
			continue
		} else {
			out = append(out, egressViolation{
				UID: acc.UID, Nickname: acc.Nickname, OwnerID: acc.OwnerUserID,
				PoolState: acc.PoolState, ProxyID: line.ID, ProxyURL: line.URL,
				LineOwner: line.OwnerUserID, Code: code, Reason: reason,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ProxyID != out[j].ProxyID {
			return out[i].ProxyID < out[j].ProxyID
		}
		return out[i].UID < out[j].UID
	})
	return out, nil
}

// handleEgressCompliance GET /api/admin/egress/compliance
func (a *app) handleEgressCompliance(w http.ResponseWriter, r *http.Request) {
	list, err := a.egressCompliance()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	byCode := map[string]int{}
	for _, v := range list {
		byCode[v.Code]++
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total": len(list), "by_code": byCode, "violations": list,
		"hint": "这些账号正从别人的/不该用的线路出去。修正方式：POST /api/admin/egress/assign " +
			`{"clear":true,"dry_run":true} 先预览，再去掉 dry_run 执行。`,
	})
}

// procsNonNil 过滤 nil 元素（列表里偶有 nil，直接索引会 panic）。
func procsNonNil(in []*EgressProxy) []*EgressProxy {
	out := make([]*EgressProxy, 0, len(in))
	for _, p := range in {
		if p != nil {
			out = append(out, p)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 读路径兜底（R12）
// ---------------------------------------------------------------------------

// egressAtoi 解析整数，失败用默认值（本文件自足，避免与其它包的 helper 撞名）。
func egressAtoi(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return def
}

// egressScopeFilter 在**反代选号**构建 uid→出口 映射时复核范围（v0.31.50）。
//
// 为什么写入口都收口了还要这一步：数据会被很多路径改（迁移脚本、手工 SQL、
// 历史版本写下的配置）。规则如果只在写入口生效，一次脏数据就绕过全部纪律。
// 这里静默把违规映射降级为"跟随全局默认"，并留一条 WARN 台账；
// 真正的修正仍走 compliance 清单（不自动改账号，避免批量断流）。
func egressScopeFilter(raw map[string]string, proxies []*EgressProxy, accs []*Account) (map[string]string, int) {
	if len(raw) == 0 {
		return raw, 0
	}
	byURL := map[string]*EgressProxy{}
	for _, p := range procsNonNil(proxies) {
		canon, err := egress.Normalize(p.URL)
		if err != nil {
			continue
		}
		byURL[canon] = p
	}
	byUID := make(map[string]*Account, len(accs))
	for _, acc := range accs {
		if acc != nil {
			byUID[acc.UID] = acc
		}
	}
	dropped := 0
	for uid, u := range raw {
		canon, err := egress.Normalize(u)
		if err != nil {
			continue
		}
		line := byURL[canon]
		if line == nil {
			continue
		}
		if ok, code, reason := egressScopeAllows(line, byUID[uid], egressAllowOpts{}); !ok {
			delete(raw, uid)
			dropped++
			log.Printf("WARN: [egress] scope violation uid=%s line=%s(%s) code=%s —— %s；已降级为跟随默认出口",
				uid, line.ID, line.URL, code, reason)
		}
	}
	return raw, dropped
}

// egressLineByURL 按规范化 URL 找线路（找不到返回 nil）。
func egressLineByURL(proxies []*EgressProxy, raw string) *EgressProxy {
	canon, err := egress.Normalize(raw)
	if err != nil || canon == "" {
		return nil
	}
	for _, p := range procsNonNil(proxies) {
		if c, e := egress.Normalize(p.URL); e == nil && c == canon {
			return p
		}
	}
	return nil
}

// egressScopeError 被出口范围规则拒绝（带 code，便于前端与测试断言）。
//
// 用类型而不是哨兵：规则有多个 code（line_disabled / foreign_account /
// account_pooled），而调用方需要把 code 原样回给前端做 i18n。
type egressScopeError struct {
	Code string
	Msg  string
}

func (e *egressScopeError) Error() string { return e.Msg }
