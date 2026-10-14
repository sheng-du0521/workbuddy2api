// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// egress_pool.go 出口代理池：代理清单的持久化 + 批量分配 + 出口聚合看板。
//
// 为什么需要「池」而不是继续逐账号手填：
//  1. 同一家供应商的线路是有限的几条，逐账号手填必然出现「多人共用同一出口」；
//  2. 挂几十个号要填几十次，且没有任何约束能防止把同一号主的号配到同一出口 ——
//     同人多账号同 IP 恰是上游最强关联信号（见 docs 的防封章节）；
//  3. 无法回答「现在有几个账号从同一个 IP 出去」这个最基本的风控问题。
//
// 设计取舍：分配结果仍然写进 accounts.egress_proxy（复用既有按账号路由），
// 池只作为「可分配来源 + 探活台账」。这样运行期链路一行都不用改，
// 也保证了「手动填的代理」和「池里分配的代理」走完全相同的代码路径。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/egress"
)

// kvEgressMaxPerProxy 同一出口允许承载的账号数上限（空/0 = **不限**）。
//
// 默认给「不限」而不是某个数字：这是**选择性约束**，只有管理员明确想压低
// 单条线路的暴露度时才需要设。给个默认值 3 的副作用是「没配过」的部署会被
// 悄悄约束住（分配时报超载、看板标红），而管理员并没有下过这个决定。
const kvEgressMaxPerProxy = "egress_max_per_proxy"

// defaultMaxPerProxy 缺省上限 = 0（不限）。
const defaultMaxPerProxy = 0

// EgressProxy 池中的一个出站代理。
type EgressProxy struct {
	ID        string
	Name      string
	URL       string
	Region    string
	Note      string
	Enabled   bool
	LastOK    bool
	LastIP    string
	LastCheck int64
	// FailStreak 连续探活失败次数（成功清零）。达阈值自动停用，见 egressProbe。
	FailStreak int64
	// CreatedAt/UpdatedAt 便于人工核对「这条线路配了多久」。
	CreatedAt int64
	UpdatedAt int64
}

// egressProxyCols 列清单（与 scanEgressProxy 顺序严格对应）。
const egressProxyCols = `id, name, url, region, note, enabled, last_ok, last_ip,
	last_check, fail_streak, created_at, updated_at`

func (s *Store) migrateEgressProxy() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS egress_proxies (
			id          TEXT PRIMARY KEY,
			name        TEXT NOT NULL DEFAULT '',
			url         TEXT NOT NULL DEFAULT '',
			region      TEXT NOT NULL DEFAULT '',
			note        TEXT NOT NULL DEFAULT '',
			enabled     INTEGER NOT NULL DEFAULT 1,
			last_ok     INTEGER NOT NULL DEFAULT 0,
			last_ip     TEXT NOT NULL DEFAULT '',
			last_check  INTEGER NOT NULL DEFAULT 0,
			fail_streak INTEGER NOT NULL DEFAULT 0,
			created_at  INTEGER NOT NULL DEFAULT 0,
			updated_at  INTEGER NOT NULL DEFAULT 0
		)`,
		// URL 唯一：同一条线路重复录入只会让分配结果出现「分配了但其实是同一个出口」。
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_egress_proxy_url ON egress_proxies(url)`,
	}
	for _, st := range stmts {
		if _, err := s.db.Exec(st); err != nil {
			return fmt.Errorf("migrate egress_proxies: %w", err)
		}
	}
	return nil
}

func (s *Store) scanEgressProxy(row interface{ Scan(dest ...any) error }) (*EgressProxy, error) {
	var p EgressProxy
	var enabled, lastOK int
	err := row.Scan(&p.ID, &p.Name, &p.URL, &p.Region, &p.Note, &enabled, &lastOK,
		&p.LastIP, &p.LastCheck, &p.FailStreak, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	p.Enabled = enabled != 0
	p.LastOK = lastOK != 0
	return &p, nil
}

// ListEgressProxies 全部代理（按创建时间升序，保证分配顺序稳定可预期）。
func (s *Store) ListEgressProxies() ([]*EgressProxy, error) {
	rows, err := s.db.Query(`SELECT ` + egressProxyCols + ` FROM egress_proxies ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*EgressProxy
	for rows.Next() {
		p, err := s.scanEgressProxy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetEgressProxy 按 id 取一条。
func (s *Store) GetEgressProxy(id string) (*EgressProxy, error) {
	row := s.db.QueryRow(`SELECT `+egressProxyCols+` FROM egress_proxies WHERE id = ?`, id)
	p, err := s.scanEgressProxy(row)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return nil, nil
		}
		return nil, err
	}
	return p, nil
}

// UpsertEgressProxy 新增或更新一条代理。url 必须已通过 egress.Validate。
func (s *Store) UpsertEgressProxy(p *EgressProxy) error {
	now := time.Now().Unix()
	if p.ID == "" {
		p.ID = newRandomID(8)
	}
	if p.CreatedAt == 0 {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	_, err := s.db.Exec(`INSERT INTO egress_proxies (`+egressProxyCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			name=excluded.name, url=excluded.url, region=excluded.region, note=excluded.note,
			enabled=excluded.enabled, updated_at=excluded.updated_at`,
		p.ID, p.Name, p.URL, p.Region, p.Note, boolToInt(p.Enabled), boolToInt(p.LastOK),
		p.LastIP, p.LastCheck, p.FailStreak, p.CreatedAt, p.UpdatedAt)
	return err
}

// DeleteEgressProxy 删除代理。**不**动账号上的 egress_proxy ——
// 已分配的账号会因 URL 仍合法而继续可用（避免「删一条线路顺带弄坏一群账号」）。
// 想一并解绑请先用「解绑」把账号代理清空。
func (s *Store) DeleteEgressProxy(id string) error {
	_, err := s.db.Exec(`DELETE FROM egress_proxies WHERE id = ?`, id)
	return err
}

// RecordEgressCheck 写入一次探活结果。
// ok=false 时 fail_streak 自增；ok=true 时清零并记录真实出口 IP。
func (s *Store) RecordEgressCheck(id string, ok bool, ip, note string) error {
	now := time.Now().Unix()
	if ok {
		_, err := s.db.Exec(`UPDATE egress_proxies
			SET last_ok=1, last_ip=?, last_check=?, fail_streak=0, updated_at=? WHERE id = ?`,
			ip, now, now, id)
		return err
	}
	_, err := s.db.Exec(`UPDATE egress_proxies
		SET last_ok=0, last_check=?, fail_streak=fail_streak+1, updated_at=? WHERE id = ?`,
		now, now, id)
	return err
}

// egressMaxPerProxy 读取「同出口账号数上限」；**空 / 非法 / 0 一律表示不限**。
func (s *Store) egressMaxPerProxy() int {
	v, err := s.GetKV(kvEgressMaxPerProxy)
	if err != nil {
		return defaultMaxPerProxy
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 1 {
		return defaultMaxPerProxy // 空串 / 非数字 / 0 / 负数 → 不限
	}
	return n
}

// ---------------------------------------------------------------------------
// 批量分配
// ---------------------------------------------------------------------------

// assignRequest 分配请求。
type assignRequest struct {
	// UIDs 明确指定要分配的账号（与 ConsumeGroup/OwnerID 是「或」关系，取并集）。
	UIDs []string `json:"uids"`
	// ConsumeGroup 按生效消耗分组筛选（apply/proxy/idle/abnormal）。
	ConsumeGroup string `json:"consume_group"`
	// OwnerID 只处理某号主的账号；-1 表示不限（0 = 平台自持）。
	OwnerID *int64 `json:"owner_id"`
	// All 显式声明「全部账号」，避免空筛选被误当成「全选」。
	All bool `json:"all"`
	// MaxPerProxy 同出口账号数上限（0 = 用设置里的值）。
	MaxPerProxy int `json:"max_per_proxy"`
	// IsolateOwner 是否强制「同一号主的账号不共用出口」（默认 true）。
	IsolateOwner *bool `json:"isolate_owner"`
	// Direct 把目标账号**设为直连**（写入 direct 哨兵，覆盖全局默认代理），
	// 而不是分配代理。用于「回退到直连」这类批量操作，无需提供可用线路。
	Direct bool `json:"direct"`
	// Clear 把目标账号的出口**清空**（回到「未指定」，跟随全局默认代理）。
	// 与 Direct 的区别：Direct 是不管全局默认都直连；Clear 是交回给全局默认决定。
	Clear bool `json:"clear"`
	// DryRun 只算不写，用于预览分配方案。
	DryRun bool `json:"dry_run"`
}

// assignPlanItem 单个账号的分配结果。
type assignPlanItem struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	OwnerID  int64  `json:"owner_id"`
	// Proxy 分配到的代理 URL；空串 = 该账号被留空（有意直连）。
	Proxy string `json:"proxy"`
	// ProxyName 代理备注名（便于读）。
	ProxyName string `json:"proxy_name"`
}

// handleEgressAssign 批量把出口代理分配到账号上。
//
// 约束优先级：
//  1. 同一号主的账号不共用出口（isolate_owner，默认开）—— 同人多账号同 IP 是最强关联信号；
//  2. 同一出口承载的账号数 ≤ max_per_proxy —— 控制单条线路的暴露度。
//
// 约束冲突时的策略是**先满足 1，再尽量满足 2**，并把放宽事实写进 warnings 返回给
// 调用方（不静默降级）：号主账号数多于可用线路时，同号主隔离无法两全，
// 这时宁可让某条线路超载，也不能让同一个人的号挤在一个 IP 上。
func (a *app) handleEgressAssign(w http.ResponseWriter, r *http.Request) {
	var req assignRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	targets, err := a.assignTargets(req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if len(targets) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "没有匹配的账号"})
		return
	}

	// 直连 / 清空模式：不需要线路池，故放在「检查池是否为空」之前。
	// 顺序很重要 —— 否则「池子空时无法把账号改回直连」，把管理员锁死在一个
	// 他自己无法退出的状态里（本次回退场景正好命中）。
	if req.Direct || req.Clear {
		want := egress.Direct
		label := "直连"
		if req.Clear {
			want, label = "", "清空（跟随全局默认代理）"
		}
		plan := make([]assignPlanItem, 0, len(targets))
		for _, acc := range targets {
			plan = append(plan, assignPlanItem{
				UID: acc.UID, Nickname: acc.Nickname, OwnerID: acc.OwnerUserID,
				Proxy: want, ProxyName: label,
			})
		}
		if !req.DryRun {
			for _, it := range plan {
				if err := a.store.SetAccountEgress(it.UID, it.Proxy); err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
					return
				}
			}
			_ = a.proxy.SyncAccounts()
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"assigned": len(plan), "mode": label, "dry_run": req.DryRun,
			"plan": plan, "per_proxy": map[string]int{}, "warnings": []string{},
		})
		return
	}

	proxies, err := a.store.ListEgressProxies()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	usable := make([]*EgressProxy, 0, len(proxies))
	for _, p := range proxies {
		if p.Enabled {
			usable = append(usable, p)
		}
	}
	if len(usable) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "出口代理池为空（或全部已停用）。请先添加线路；若只想让账号直连，请改用「设为直连」模式。",
		})
		return
	}

	maxPer := req.MaxPerProxy
	if maxPer <= 0 {
		maxPer = a.store.egressMaxPerProxy()
	}
	isolate := true
	if req.IsolateOwner != nil {
		isolate = *req.IsolateOwner
	}

	plan, warnings := planEgressAssignment(targets, usable, maxPer, isolate)

	if !req.DryRun {
		for _, it := range plan {
			if err := a.store.SetAccountEgress(it.UID, it.Proxy); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
		}
		// 分配后必须重同步：egressBad 与 egressByUID 都在这条路径上重建。
		_ = a.proxy.SyncAccounts()
	}

	// 统计每条线路分到几个号，便于一眼看出是否均衡。
	perProxy := map[string]int{}
	for _, it := range plan {
		perProxy[it.Proxy]++
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"assigned":      len(plan),
		"proxies_used":  len(perProxy),
		"max_per_proxy": maxPer,
		"isolate_owner": isolate,
		"dry_run":       req.DryRun,
		"plan":          plan,
		"per_proxy":     perProxy,
		"warnings":      warnings,
	})
}

// assignTargets 按请求里的筛选条件挑出目标账号。
func (a *app) assignTargets(req assignRequest) ([]*Account, error) {
	all, err := a.store.ListAccounts()
	if err != nil {
		return nil, err
	}
	// 显式 uids 优先：给了名单就只按名单来（并集语义与筛选条件叠加）。
	if len(req.UIDs) > 0 {
		want := map[string]bool{}
		for _, u := range req.UIDs {
			if s := strings.TrimSpace(u); s != "" {
				want[s] = true
			}
		}
		out := make([]*Account, 0, len(want))
		for _, acc := range all {
			if want[acc.UID] {
				out = append(out, acc)
			}
		}
		return out, nil
	}
	if req.ConsumeGroup == "" && req.OwnerID == nil && !req.All {
		return nil, fmt.Errorf("请指定分配范围：账号列表、消耗分组、号主，或显式选择全部")
	}
	group := normalizeConsumeGroup(req.ConsumeGroup)
	out := make([]*Account, 0, len(all))
	for _, acc := range all {
		if group != "" && effectiveConsumeGroup(acc) != group {
			continue
		}
		if req.OwnerID != nil && *req.OwnerID >= 0 && acc.OwnerUserID != *req.OwnerID {
			continue
		}
		out = append(out, acc)
	}
	return out, nil
}

// planEgressAssignment 计算分配方案（纯函数，便于单测）。
//
// 算法：按号主分组后逐号主处理；每个账号挑「已承载账号数最少」的线路，
// 且（isolate 时）该号主尚未用过。首次均衡由排序保证确定性——
// 同样输入必得同样输出，便于管理员核对与回归。
func planEgressAssignment(targets []*Account, proxies []*EgressProxy, maxPer int, isolate bool) ([]assignPlanItem, []string) {
	// 目标账号按「号主的账号数降序」处理：先安排大户，避免大户的号在剩余线路里
	// 被前面零散账号挤到只能重复用同一条线路。
	byOwner := map[int64][]*Account{}
	order := []int64{}
	for _, acc := range targets {
		if len(byOwner[acc.OwnerUserID]) == 0 {
			order = append(order, acc.OwnerUserID)
		}
		byOwner[acc.OwnerUserID] = append(byOwner[acc.OwnerUserID], acc)
	}
	sort.SliceStable(order, func(i, j int) bool {
		return len(byOwner[order[i]]) > len(byOwner[order[j]])
	})

	load := map[string]int{}
	ownerUsed := map[int64]map[string]bool{}
	warnings := []string{}
	seenWarn := map[string]bool{}
	addWarn := func(s string) {
		if !seenWarn[s] {
			seenWarn[s] = true
			warnings = append(warnings, s)
		}
	}

	var plan []assignPlanItem
	for _, owner := range order {
		accs := byOwner[owner]
		if isolate && len(accs) > len(proxies) {
			addWarn(fmt.Sprintf("号主 #%d 有 %d 个账号，但可用出口只有 %d 条，无法完全隔离——已放宽为「尽量分散」。建议为该号主补充独立线路。",
				owner, len(accs), len(proxies)))
		}
		if !isolate && maxPer > 0 && len(accs) > maxPer*len(proxies) {
			addWarn(fmt.Sprintf("号主 #%d 的 %d 个账号超过全部线路容量（%d 条 × 上限 %d），部分线路会超载。",
				owner, len(accs), len(proxies), maxPer))
		}
		if ownerUsed[owner] == nil {
			ownerUsed[owner] = map[string]bool{}
		}
		for _, acc := range accs {
			pick := pickProxyFor(proxies, load, ownerUsed[owner], maxPer, isolate)
			if pick == nil {
				addWarn("可用线路不足以完成分配，部分账号被留空（保持原状）。")
				plan = append(plan, assignPlanItem{UID: acc.UID, Nickname: acc.Nickname, OwnerID: owner})
				continue
			}
			load[pick.URL]++
			ownerUsed[owner][pick.URL] = true
			plan = append(plan, assignPlanItem{
				UID: acc.UID, Nickname: acc.Nickname, OwnerID: owner,
				Proxy: pick.URL, ProxyName: pick.Name,
			})
		}
	}
	return plan, warnings
}

// pickProxyFor 为单个账号挑线路：优先负载最低者；isolate 时跳过该号主已用过的。
//
// 两级回落保证「总有号可用」：
//  1. 同时满足隔离与容量上限的最轻线路；
//  2. 只有隔离（1+）不可得时，放宽容量上限（隔离比容量更重要，见 handleEgressAssign 注释）；
//  3. 连隔离都不可得（号主账号数 > 线路数）→ 允许复用，取最轻线路。
func pickProxyFor(proxies []*EgressProxy, load map[string]int, ownerUsed map[string]bool, maxPer int, isolate bool) *EgressProxy {
	var best, bestRelaxed, bestAny *EgressProxy
	for _, p := range proxies {
		used := ownerUsed[p.URL]
		if isolate && used {
			if bestAny == nil || load[p.URL] < load[bestAny.URL] {
				bestAny = p
			}
			continue
		}
		underCap := maxPer <= 0 || load[p.URL] < maxPer
		if underCap {
			if best == nil || load[p.URL] < load[best.URL] {
				best = p
			}
			continue
		}
		if bestRelaxed == nil || load[p.URL] < load[bestRelaxed.URL] {
			bestRelaxed = p
		}
	}
	if best != nil {
		return best
	}
	if bestRelaxed != nil {
		return bestRelaxed
	}
	return bestAny
}

// ---------------------------------------------------------------------------
// 出口聚合看板
// ---------------------------------------------------------------------------

// egressOverviewProxy 看板里一条出口的聚合信息。
type egressOverviewProxy struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	URL    string `json:"url"`
	Region string `json:"region"`
	Note   string `json:"note"`
	// Enabled 是否启用（停用的线路不参与分配，但已分配的账号仍照原样路由）。
	Enabled bool `json:"enabled"`
	// LastOK/LastIP/LastCheck/FailStreak 最近一次探活结果。
	LastOK     bool   `json:"last_ok"`
	LastIP     string `json:"last_ip"`
	LastCheck  int64  `json:"last_check"`
	FailStreak int64  `json:"fail_streak"`
	// AccountCount 当前从该出口出去的账号数。
	AccountCount int `json:"account_count"`
	// OverThreshold 是否超过同出口账号数上限。
	OverThreshold bool `json:"over_threshold"`
	// Owners 使用该出口的号主列表（去重；0 = 平台自持）。
	Owners []int64 `json:"owners"`
	// OwnerConflict 是否有号主在同一出口上有多个账号（隔离被破坏的信号）。
	OwnerConflict []int64 `json:"owner_conflict"`
}

// egressOverviewView 看板整体返回。
type egressOverviewView struct {
	Proxies []egressOverviewProxy `json:"proxies"`
	// Direct 未配代理（实际从本机 IP 出去的）账号数与号主集合。
	DirectAccountCount int     `json:"direct_account_count"`
	DirectOwners       []int64 `json:"direct_owners"`
	// DirectExplicit 其中「显式设为直连」（direct 哨兵）的数量。
	// 与 DirectAccountCount 的差值 = 未指定且全局默认代理为空的账号数。
	DirectExplicit int `json:"direct_explicit"`
	// DefaultEgress 当前全局默认出口代理（空 = 未配置）。
	// 非空时，「未指定」的账号会走它而非直连 —— 看板据此区分两类直连。
	DefaultEgress string `json:"default_egress"`
	// Bad 出口异常账号（代理配置有误，已被排除出平台选号）。
	Bad []map[string]any `json:"bad"`
	// MaxPerProxy 当前上限设置。
	MaxPerProxy int `json:"max_per_proxy"`
	// OverCount 超阈值的出口条数（便于顶部告警）。
	OverCount int `json:"over_count"`
	// Unassigned 参与平台消耗但未配代理（直接暴露本机 IP）的账号数。
	Unassigned int `json:"unassigned"`
}

// handleEgressOverview 出口聚合看板。
func (a *app) handleEgressOverview(w http.ResponseWriter, r *http.Request) {
	view, err := a.egressOverview()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// egressOverview 聚合出口视角的风控信息。
func (a *app) egressOverview() (egressOverviewView, error) {
	proxies, err := a.store.ListEgressProxies()
	if err != nil {
		return egressOverviewView{}, err
	}
	all, err := a.store.ListAccounts()
	if err != nil {
		return egressOverviewView{}, err
	}
	maxPer := a.store.egressMaxPerProxy()

	idx := map[string]*egressOverviewProxy{}
	// order 保证输出顺序稳定（池中顺序优先，手填线路追加在后面），
	// 便于管理员对照与回归比较。
	order := make([]string, 0, len(proxies))
	// add 取回（或新建）某出口的聚合条目。
	//
	// 注意这里**只**用 map 持有指针，最后才生成切片：早先版本同时往 map 和切片里放，
	// 切片拿到的是值拷贝，后续回填计数改的是 map 里那一份，于是账号数永远显示 0。
	// 单一数据源是这类聚合代码唯一稳妥的写法。
	add := func(url, name string) *egressOverviewProxy {
		if v, ok := idx[url]; ok {
			return v
		}
		if name == "" {
			name = url
		}
		v := &egressOverviewProxy{URL: url, Name: name, Enabled: true}
		idx[url] = v
		order = append(order, url)
		return v
	}
	for _, p := range proxies {
		v := add(p.URL, p.Name)
		v.ID, v.Region, v.Note = p.ID, p.Region, p.Note
		v.Enabled, v.LastOK, v.LastIP = p.Enabled, p.LastOK, p.LastIP
		v.LastCheck, v.FailStreak = p.LastCheck, p.FailStreak
	}

	// 号主在某出口上的账号计数（用于识别隔离被破坏）。
	ownerPerProxy := map[string]map[int64]int{}
	directOwners := map[int64]bool{}
	directCount := 0
	directExplicit := 0
	unassigned := 0
	defEgress := a.proxy.DefaultEgress()
	for _, acc := range all {
		// 用 ResolveEgress 而非直接读 acc.EgressProxy：出口是两级配置，
		// 「留空」可能继承全局默认代理 —— 直接读字段会把这类账号误报成直连。
		resolved := acc.EgressProxy
		if egress.IsDirect(resolved) {
			resolved = egress.Direct
		} else if strings.TrimSpace(resolved) == "" {
			resolved = defEgress // 未指定 → 继承默认（默认也可能为空 = 实际直连）
		}
		if isDirectEgress(resolved) {
			directCount++
			if egress.IsDirect(acc.EgressProxy) {
				directExplicit++
			}
			directOwners[acc.OwnerUserID] = true
			// 参与平台消耗却走直连 = 直接暴露本机 IP，这是需要提示的重点。
			if consumeGroupParticipates(effectiveConsumeGroup(acc)) {
				unassigned++
			}
			continue
		}
		// 账号用的代理不在池里（手填的或池里已删）：仍要统计，否则看板会漏掉
		// 一大块实际出口，反而给出「没有账号共用出口」的错误安心感。
		v := add(resolved, "")
		v.AccountCount++
		if ownerPerProxy[resolved] == nil {
			ownerPerProxy[resolved] = map[int64]int{}
		}
		ownerPerProxy[resolved][acc.OwnerUserID]++
	}

	bad := a.proxy.EgressBadUIDs()
	badRows := make([]map[string]any, 0, len(bad))
	for _, acc := range all {
		if reason, ok := bad[acc.UID]; ok {
			badRows = append(badRows, map[string]any{
				"uid": acc.UID, "nickname": acc.Nickname,
				"owner_id": acc.OwnerUserID, "reason": reason,
			})
		}
	}
	sort.Slice(badRows, func(i, j int) bool {
		return strField(badRows[i], "uid") < strField(badRows[j], "uid")
	})

	overCount := 0
	// 从唯一数据源生成输出切片并回填 owners / conflict / over_threshold。
	synced := make([]egressOverviewProxy, 0, len(order))
	for _, url := range order {
		v := *idx[url]
		if m := ownerPerProxy[url]; m != nil {
			for oid, n := range m {
				v.Owners = append(v.Owners, oid)
				if n > 1 {
					v.OwnerConflict = append(v.OwnerConflict, oid)
				}
			}
		}
		sort.Slice(v.Owners, func(i, j int) bool { return v.Owners[i] < v.Owners[j] })
		sort.Slice(v.OwnerConflict, func(i, j int) bool { return v.OwnerConflict[i] < v.OwnerConflict[j] })
		v.OverThreshold = maxPer > 0 && v.AccountCount > maxPer
		if v.OverThreshold {
			overCount++
		}
		if v.Name == "" {
			v.Name = v.URL
		}
		synced = append(synced, v)
	}

	owners := make([]int64, 0, len(directOwners))
	for o := range directOwners {
		owners = append(owners, o)
	}
	sort.Slice(owners, func(i, j int) bool { return owners[i] < owners[j] })

	return egressOverviewView{
		Proxies:            synced,
		DirectAccountCount: directCount,
		DirectExplicit:     directExplicit,
		DirectOwners:       owners,
		Bad:                badRows,
		MaxPerProxy:        maxPer,
		OverCount:          overCount,
		Unassigned:         unassigned,
		DefaultEgress:      defEgress,
	}, nil
}

// ---------------------------------------------------------------------------
// 代理池 CRUD / 探活
// ---------------------------------------------------------------------------

// handleEgressProxies 代理池的增删改查。
//
// 路径语义（沿用本项目其它资源的习惯）：
//
//	GET    /api/admin/egress/proxies        列表
//	POST   /api/admin/egress/proxies        新增或更新（带 id 即更新）
//	DELETE /api/admin/egress/proxies/{id}   删除
//	POST   /api/admin/egress/proxies/{id}/check 单条立即探活
func (a *app) handleEgressProxies(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := a.store.ListEgressProxies()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"proxies": list, "max_per_proxy": a.store.egressMaxPerProxy()})

	case http.MethodPost:
		var req struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			URL     string `json:"url"`
			Region  string `json:"region"`
			Note    string `json:"note"`
			Enabled *bool  `json:"enabled"`
			// Check 保存后是否立即探活（默认 true，让管理员当场看到出口 IP）。
			Check *bool `json:"check"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
			return
		}
		norm, err := egress.Validate(req.URL)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if norm == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "代理地址不能为空"})
			return
		}
		p := &EgressProxy{
			ID: req.ID, Name: strings.TrimSpace(req.Name), URL: norm,
			Region: strings.TrimSpace(req.Region), Note: strings.TrimSpace(req.Note),
			Enabled: true,
		}
		if req.Enabled != nil {
			p.Enabled = *req.Enabled
		}
		if req.ID != "" {
			old, err := a.store.GetEgressProxy(req.ID)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			if old == nil {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "代理不存在"})
				return
			}
			p.CreatedAt = old.CreatedAt
			p.LastCheck, p.LastIP, p.FailStreak, p.LastOK = old.LastCheck, old.LastIP, old.FailStreak, old.LastOK
		}
		if p.Name == "" {
			p.Name = norm
		}
		if err := a.store.UpsertEgressProxy(p); err != nil {
			// URL 唯一索引冲突时给出可读提示，而不是把 SQL 错误抛给管理员。
			if strings.Contains(err.Error(), "UNIQUE") {
				writeJSON(w, http.StatusConflict, map[string]any{"error": "该代理地址已存在（同一线路无需重复录入）"})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		doCheck := req.Check == nil || *req.Check
		var probe egress.ProbeResult
		if doCheck {
			probe = a.checkEgressProxy(p)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"proxy":       p,
			"verified":    probe.OK,
			"exit_ip":     probe.IP,
			"note":        probe.Error,
			"unreachable": probe.EndpointUnreachable,
		})

	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if id == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing id"})
			return
		}
		if err := a.store.DeleteEgressProxy(id); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": id})

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
	}
}

// egressProbeConcurrency 「全部探活」的并发上限。
//
// 串行探活在每条线路都要等满超时时会线性堆积（5 条 = 30 秒以上），并发能把
// 总耗时压到接近单条。限 3 是为了不给本机网络造成突发压力 —— 探活是低频的
// 管理操作，不值得为速度冒险。
const egressProbeConcurrency = 3

// probeProxiesConcurrently 并发探活多条线路，返回与输入同序的结果。
//
// 结果切片按索引写入而非 append：并发 append 顺序不确定，而管理员对照列表时
// 顺序错乱会让人怀疑「探活结果是不是串了」。
func (a *app) probeProxiesConcurrently(list []*EgressProxy) []map[string]any {
	out := make([]map[string]any, len(list))
	sem := make(chan struct{}, egressProbeConcurrency)
	var wg sync.WaitGroup
	for i, p := range list {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, p *EgressProxy) {
			defer wg.Done()
			defer func() { <-sem }()
			res := a.checkEgressProxy(p)
			out[i] = map[string]any{
				"id": p.ID, "name": p.Name, "ok": res.OK, "exit_ip": res.IP,
				"note": res.Error, "unreachable": res.EndpointUnreachable,
			}
		}(i, p)
	}
	wg.Wait()
	return out
}

// handleEgressCheck 立即探活一条代理（或全部）。带 ?all=1 时全部探一遍。
func (a *app) handleEgressCheck(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("all") == "1" {
		list, err := a.store.ListEgressProxies()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"results": a.probeProxiesConcurrently(list)})
		return
	}
	id := r.URL.Query().Get("id")
	p, err := a.store.GetEgressProxy(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if p == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "代理不存在"})
		return
	}
	res := a.checkEgressProxy(p)
	writeJSON(w, http.StatusOK, map[string]any{
		"id": p.ID, "ok": res.OK, "exit_ip": res.IP,
		"note": res.Error, "unreachable": res.EndpointUnreachable,
		"fail_streak": p.FailStreak,
	})
}

// egressFailThreshold 连续探活失败多少次后自动停用该线路。
//
// 取 3：单次失败可能只是抖动，连续 3 次（按 5 分钟节奏即 15 分钟）基本可判定
// 线路真的挂了。自动停用是必要的——线路挂了若不摘除，分配算法会继续往上派号，
// 而不在池里的账号仍会带着这个坏代理，落到「出口异常」被排除出选号。
const egressFailThreshold = 3

// checkEgressProxy 探活一条代理并落库。
//
// 注意「探测端点不可达」不记为失败：那是本机出网受限，不是线路问题。
// 若把它算作失败，受限网络下所有线路都会被自动停用 —— 比不检测更糟。
func (a *app) checkEgressProxy(p *EgressProxy) egress.ProbeResult {
	res := egress.Probe(p.URL)
	if res.EndpointUnreachable {
		// 只更新探活时间，不动 last_ok/fail_streak（结论未知）。
		_ = a.store.RecordEgressCheck(p.ID, p.LastOK, p.LastIP, res.Error)
		return res
	}
	_ = a.store.RecordEgressCheck(p.ID, res.OK, res.IP, res.Error)
	if fresh, err := a.store.GetEgressProxy(p.ID); err == nil && fresh != nil {
		p.FailStreak = fresh.FailStreak
		if !res.OK && fresh.FailStreak >= egressFailThreshold && p.Enabled {
			p.Enabled = false
			_ = a.store.UpsertEgressProxy(p)
			log.Printf("[egress] 出口代理连续 %d 次探活失败，已自动停用：%s（%s）", fresh.FailStreak, p.Name, p.URL)
		}
	}
	return res
}

// ---------------------------------------------------------------------------
// 定时探活与请求抖动
// ---------------------------------------------------------------------------

// egressProbeInterval 定时探活的间隔。
//
// 取 5 分钟：出口 IP 变化的检测需要一定及时性（供应商换 IP 会影响风控判断），
// 但每个周期要为每条线路发一次外部请求，过于频繁既无必要也浪费流量。
const egressProbeInterval = 5 * time.Minute

// StartEgressProber 起一个后台 goroutine 周期探活出口代理（ctx 取消即退出）。
//
// 为什么不用 internal/scheduler：那是「按账号执行的业务任务」框架（签到/旅行等），
// 有自己的时段与台账口径；出口探活是「基础设施健康检查」，对象是代理而不是账号，
// 混进去会让台账语义变得含糊。独立 ticker 更简单也更好推理。
func StartEgressProber(ctx context.Context, a *app) {
	go func() {
		// 启动即先跑一轮：进程刚起时最需要知道哪条线路是通的。
		a.probeAllEgress()
		t := time.NewTicker(egressProbeInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				a.probeAllEgress()
			}
		}
	}()
}

// probeAllEgress 探活全部代理（忽略错误，探活是观测不是关键路径）。
//
// 探完之后**顺带跑一次坏线路上的账号迁移**（v0.24.6）：探活把坏线路停用了，
// 但绑在它上面的账号不会自己回来 —— 不迁移的话，"自动停用坏线路"只做了一半，
// 那些账号会一直挂在死代理上、落到「出口异常」被排除出选号。
func (a *app) probeAllEgress() {
	list, err := a.store.ListEgressProxies()
	if err != nil {
		return
	}
	a.probeProxiesConcurrently(list)
	a.rehomeAccountsFromDeadProxies()
}

// egressJitterMsNow 返回当前抖动上限（毫秒），供设置页展示。
func (p *Proxy) egressJitterMsNow() int {
	p.selMu.RLock()
	defer p.selMu.RUnlock()
	return p.egressJitterMs
}

// egressJitter 请求前的出口抖动。
//
// 目的：避免多个共用同一出口的账号在同一毫秒齐发请求 —— 「同一 IP 同时打出
// 多个账号的请求」是比「共用 IP」本身更强的关联信号。
//
// 实现上刻意保守：
//   - **只对真正走了代理出口的账号生效**：直连（含显式 `direct` 哨兵、以及全局默认
//     也为空的情况）没有「出口分散」可言，加了只是白等；
//   - 上限由 kv 控制，缺省 0 = 关闭，避免默认给所有请求增加延迟；
//   - 只对非首发的后续尝试生效，首次请求不加延迟，保住正常路径的响应速度。
//
// v0.24.8 修正：此前判据只写了 `egress == ""`，而**显式直连的账号其 `egressByUID`
// 值是 `direct` 哨兵（非空）**，于是它们被一并加了抖动 —— 与上面第一条注释
// （以及"直连不生效"的设计意图）相矛盾。现在显式判 `IsDirect`。
func (p *Proxy) egressJitter(uid string) time.Duration {
	p.selMu.RLock()
	jitterMs := p.egressJitterMs
	eg := p.egressByUID[uid] // 注意别用 egress 作变量名：会遮蔽同名的 egress 包
	if eg == "" {
		eg = p.defaultEgress
	}
	p.selMu.RUnlock()
	if jitterMs <= 0 || eg == "" || egress.IsDirect(eg) {
		return 0
	}
	// 用 uid 的后几位做确定性偏移，而非纯随机：同一出口上的不同账号会稳定地
	// 错开到不同的时间片，而不是每个请求都重新抽签（后者会偶发撞在一起）。
	return deterministicJitter(uid, jitterMs)
}

// deterministicJitter 由 uid 导出一个 [0, jitterMs) 的确定性偏移。
func deterministicJitter(uid string, jitterMs int) time.Duration {
	if jitterMs <= 0 {
		return 0
	}
	var h uint32 = 2166136261
	for i := 0; i < len(uid); i++ {
		h ^= uint32(uid[i])
		h *= 16777619
	}
	return time.Duration(h%uint32(jitterMs)) * time.Millisecond
}
