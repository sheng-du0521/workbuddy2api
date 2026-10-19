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
// ⚠️ **必须有 json tag**（v0.31.23 补）：这个结构体直接下发给前端
// （`GET /api/admin/egress/proxies` 的 `proxies` 数组）。没有 tag 时 Go 会用
// **字段名**（`Name` / `URL` / `LastIP`）序列化，而前端读的是 `p.name` / `p.url`
// —— 于是 `(p.name || p.url).charAt(0)` 直接抛
// 「Cannot read properties of undefined」，**整块线路池渲染不出来**；
// 更坏的是异常被调用方（loadEgress）的 try/catch 吞成一句 toast，
// 页面上只留下"什么都没有"，看不出是前端 bug 还是真没数据。
// 同类结构（egressOverviewProxy / egressMachineView）一直是有 tag 的，
// 所以此前表现为"统计与探活文案正常、线路卡全空"。
type EgressProxy struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	URL    string `json:"url"`
	Region string `json:"region"`
	Note   string `json:"note"`
	// Enabled 是否启用（停用的线路不参与分配，但已分配的账号仍照原样路由）。
	Enabled bool `json:"enabled"`
	// LastOK/LastIP/LastCheck/FailStreak 最近一次探活结果。
	LastOK    bool   `json:"last_ok"`
	LastIP    string `json:"last_ip"`
	LastCheck int64  `json:"last_check"`
	// FailStreak 连续探活失败次数（成功清零）。达阈值自动停用，见 egressProbe。
	FailStreak int64 `json:"fail_streak"`
	// CreatedAt/UpdatedAt 便于人工核对「这条线路配了多久」。
	CreatedAt int64 `json:"created_at"`
	UpdatedAt int64 `json:"updated_at"`
	// OwnerUserID 这条线路「由谁接进来」的归属（v0.31.23）。
	//
	//	0  = 平台（手填 / 管理员用 agent 拉起的本机线路）
	//	>0 = 该用户在控制台「设置 → 我的出口」里配置的线路
	//
	// 为什么要单独一位而不是只看 note：多个用户各有一条线路时，
	// 管理后台要回答的是「这条线是谁的、现在被谁的号用着」——
	// note 是给人看的自由文本，判不出这个。
	//
	// ⚠️ 归属**不改变可用范围**，改变的是**自动/显式**的差别：
	// 用户提供的线路只能被管理员**显式指名**分配（见 handleEgressAssign 与
	// rehomeAccountsFromDeadProxies 的排除逻辑）—— 它是不受我们控制的第三方出口，
	// 把别人的 access_token 从它送出去是明确的错误。
	OwnerUserID int64 `json:"owner_user_id"`
	// AutoDisabled 这条线路是**被探活自动停用**的（v0.31.47）。
	//
	// 为什么必须与"人工停用"分开：自动停用是**暂时性**判断（本机断开、代理没起），
	// 人工停用是**决定**（这条线我不要了）。此前两者共用一个 Enabled 位，后果是
	// 「探活失败 → 自动停用 → 用户重新拉起代理 → 端口通了但线路仍是停用」
	// —— 恢复这件事没有任何代码负责，用户看到的是"拉起来了但全部失败"。
	//
	// 有了这一位，探活成功就能**只恢复自己停掉的**，绝不碰人工停用的线路
	// （v0.31.34 已经踩过"无条件 Enabled=true 把别人停用线路拉活"的坑）。
	AutoDisabled bool `json:"auto_disabled"`
	// Scope 这条线路「能承载哪些账号」（v0.31.50，需求二）。
	//
	//	platform      平台线路：不限制入池状态（放行所有账号，行为与历史一致）
	//	self_unpooled 用户线路：**只给线路主人的、且尚未入池的账号**用
	//
	// 为什么要落库而不是每次靠 owner+pool_state 现算：范围会随规则演进
	// （将来可能有"仅本人全部号"或"平台共享池"），把它显式写出来，
	// 判定、展示、审计读的是同一个字段，不用改三处判断。
	Scope string `json:"scope"`
	// ManualDisabled 管理员**人工停用**（v0.31.50）。
	//
	// 与 Enabled 分开的理由：Enabled=false 可能是探活自动停的（可自动恢复），
	// 而 manual_disabled 是**人的决定** —— 用户重新拉起代理不得把它拉活，
	// 管理员可以强制的范围也**不包含**它（见 egressLineAllows 第 1 条）。
	ManualDisabled bool `json:"manual_disabled"`
}

// egressProxyCols 列清单（与 scanEgressProxy 顺序严格对应）。
const egressProxyCols = `id, name, url, region, note, enabled, last_ok, last_ip,
	last_check, fail_streak, created_at, updated_at, owner_user_id, auto_disabled,
	scope, manual_disabled`

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
			updated_at  INTEGER NOT NULL DEFAULT 0,
			owner_user_id INTEGER NOT NULL DEFAULT 0,
			auto_disabled INTEGER NOT NULL DEFAULT 0,
			scope TEXT NOT NULL DEFAULT '',
			manual_disabled INTEGER NOT NULL DEFAULT 0
		)`,
		// URL 唯一：同一条线路重复录入只会让分配结果出现「分配了但其实是同一个出口」。
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_egress_proxy_url ON egress_proxies(url)`,
	}
	for _, st := range stmts {
		if _, err := s.db.Exec(st); err != nil {
			return fmt.Errorf("migrate egress_proxies: %w", err)
		}
	}
	// 增量列（v0.31.23）：老库没有 owner_user_id，补上；已有线路一律 0 = 平台。
	//
	// ⚠️ 传的是**列定义**、不是整条 ALTER —— `ensureColumn` 自己会拼
	// `ALTER TABLE <table> ADD COLUMN <ddl>`。写成整条 ALTER 会拼出
	// `ADD COLUMN ALTER TABLE …`，启动即 `near "ALTER": syntax error`、
	// **服务直接起不来**（2026-10-17 真实停机一次，见
	// TestEgressProxyOwnerMigrationOnOldDB 的说明）。
	if err := s.ensureColumn("egress_proxies", "owner_user_id",
		"owner_user_id INTEGER NOT NULL DEFAULT 0"); err != nil {
		return fmt.Errorf("migrate egress_proxies(owner): %w", err)
	}
	// 增量列（v0.31.47）：区分"探活自动停用"与"人工停用"。
	// 老库一律 0 = 人工/未知 —— 保守方向：宁可让运维点一下启用，
	// 也不要凭"没这个字段"就自动拉活一条可能是人工停掉的线路。
	if err := s.ensureColumn("egress_proxies", "auto_disabled",
		"auto_disabled INTEGER NOT NULL DEFAULT 0"); err != nil {
		return fmt.Errorf("migrate egress_proxies(auto_disabled): %w", err)
	}
	// 存量标注（v0.31.47，幂等）：加列前就已经"停用 + 连续失败达阈值"的线路，
	// 几乎只可能来自自动停用（人工停用不会恰好连续失败 3 次）。
	// 不标的话它们会永远停在停用态 —— 用户重新拉起代理也不会自动恢复，
	// 而新加的这个能力对存量线路等于没有。
	if _, err := s.db.Exec(`UPDATE egress_proxies SET auto_disabled = 1
		WHERE enabled = 0 AND fail_streak >= ?`, egressFailThreshold); err != nil {
		return fmt.Errorf("migrate egress_proxies(auto_disabled backfill): %w", err)
	}
	// 增量列（v0.31.50，需求二）：显式表达「这条线路能承载哪些账号」。
	if err := s.ensureColumn("egress_proxies", "scope",
		"scope TEXT NOT NULL DEFAULT ''"); err != nil {
		return fmt.Errorf("migrate egress_proxies(scope): %w", err)
	}
	// 人工停用位（与 Enabled/AutoDisabled 三者分离，见 EgressProxy.ManualDisabled）。
	if err := s.ensureColumn("egress_proxies", "manual_disabled",
		"manual_disabled INTEGER NOT NULL DEFAULT 0"); err != nil {
		return fmt.Errorf("migrate egress_proxies(manual_disabled): %w", err)
	}
	// 存量回填（幂等）：用户线路 → self_unpooled，其余 → platform。
	// 不回填的话，老线路的 scope 是空串，闸门会把它当"未知范围"拒绝 ——
	// 那等于一次升级把全平台的出口都停了。
	if _, err := s.db.Exec(`UPDATE egress_proxies SET scope = CASE
			WHEN owner_user_id > 0 THEN 'self_unpooled' ELSE 'platform' END
		WHERE scope IS NULL OR scope = ''`); err != nil {
		return fmt.Errorf("migrate egress_proxies(scope backfill): %w", err)
	}
	return nil
}

func (s *Store) scanEgressProxy(row interface{ Scan(dest ...any) error }) (*EgressProxy, error) {
	var p EgressProxy
	var enabled, lastOK, autoDisabled, manualDisabled int
	var scope string
	err := row.Scan(&p.ID, &p.Name, &p.URL, &p.Region, &p.Note, &enabled, &lastOK,
		&p.LastIP, &p.LastCheck, &p.FailStreak, &p.CreatedAt, &p.UpdatedAt, &p.OwnerUserID,
		&autoDisabled, &scope, &manualDisabled)
	if err != nil {
		return nil, err
	}
	p.Enabled = enabled != 0
	p.LastOK = lastOK != 0
	p.AutoDisabled = autoDisabled != 0
	p.ManualDisabled = manualDisabled != 0
	p.Scope = normEgressScope(scope, p.OwnerUserID)
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
	if p.Scope == "" {
		p.Scope = normEgressScope("", p.OwnerUserID)
	}
	_, err := s.db.Exec(`INSERT INTO egress_proxies (`+egressProxyCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			name=excluded.name, url=excluded.url, region=excluded.region, note=excluded.note,
			enabled=excluded.enabled, auto_disabled=excluded.auto_disabled,
			-- 人工停用位必须能落库（否则手动停用接口写完等于没写）；
			-- 但**编辑接口**会在进入这里之前先把旧值拷回来，见 handleEgressProxies。
			manual_disabled=excluded.manual_disabled,
			-- scope 粘性同 owner：线路一旦被登记为「平台」或「用户受限」，
			-- 后续写入不得把它改宽/改窄（改范围要显式走管理员接口）。
			scope=CASE WHEN egress_proxies.scope = '' THEN excluded.scope
				ELSE egress_proxies.scope END,
			updated_at=excluded.updated_at,
			-- ⚠️ 归属**粘性**：一旦有人认领过就不再被后续写入覆盖（v0.31.23）。
			-- 否则「管理员手工登记同一条 URL」或「另一个用户恰好用同一个出口」
			-- 会把原来的归属抹掉 —— 而归属恰恰是事后追责时唯一能查的东西。
			-- 只允许「从 0 变成非 0」这一次。
			owner_user_id=CASE WHEN egress_proxies.owner_user_id = 0
				THEN excluded.owner_user_id ELSE egress_proxies.owner_user_id END`,
		p.ID, p.Name, p.URL, p.Region, p.Note, boolToInt(p.Enabled), boolToInt(p.LastOK),
		p.LastIP, p.LastCheck, p.FailStreak, p.CreatedAt, p.UpdatedAt, p.OwnerUserID,
		boolToInt(p.AutoDisabled), p.Scope, boolToInt(p.ManualDisabled))
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
	// ProxyURL（v0.31.19）只在指定的**这一条**线路上分配（空 = 从整个线路池里挑）。
	//
	// 为什么需要它：此前分配只能选「范围」，线路由服务端自己挑 —— 于是
	// "我只想把这条本地住宅线路给某几个号"这件事**做不到**（只能先停用其它线路，
	// 那又会影响已在用它们的号）。线路卡上的「分配」按钮走的就是这个字段。
	ProxyURL string `json:"proxy_url"`
	// DryRun 只算不写，用于预览分配方案。
	DryRun bool `json:"dry_run"`
	// IncludeUserLines **已废弃**（v0.31.50）：它做的事（把用户线路纳入自动候选、
	// 且能承载任意账号）正是需求二要禁的「跨用户分发且无痕」。
	// 保留字段只为让老客户端收到一条**明确**的 400（而不是静默忽略后行为变了）。
	// 新写法：`force_scope: true` + `force_reason`（≥10 字）。
	IncludeUserLines bool `json:"include_user_lines"`
	// ForceScope 允许**强制越界**：把用户线路分给"不属于该主人"或"已入池"的账号
	// （v0.31.50，宽松版）。
	//
	// 必须同时给 ForceReason；写入前落 `egress_force_audits`。
	// ⚠️ 强制**不覆盖**线路的 enabled=false / manual_disabled —— 那是停用决定。
	ForceScope bool `json:"force_scope"`
	// ForceReason 强制越界的理由（≥10 个字，不能为空或套话）。
	ForceReason string `json:"force_reason"`
}

// egressForceReasonMin 强制越界的理由最短字数（按 rune 计）。
const egressForceReasonMin = 10

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
	// 废弃字段必须先拒（v0.31.50）：静默忽略等于"行为悄悄变了"，
	// 而管理员可能正是靠它分发了几十个号。
	if req.IncludeUserLines {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "include_user_lines 已废弃：它会不经审计地把用户线路分给任意账号。" +
				"请改用 force_scope=true + force_reason（≥10 字），系统会写强制越界审计",
			"code": "include_user_lines_deprecated",
		})
		return
	}
	if req.ForceScope {
		if n := len([]rune(strings.TrimSpace(req.ForceReason))); n < egressForceReasonMin {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": fmt.Sprintf("强制越界必须填写理由（至少 %d 个字，当前 %d 个）——"+
					"这条记录是事后追责的唯一依据", egressForceReasonMin, n),
				"code": "force_reason_required",
			})
			return
		}
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
	wantURL := strings.TrimSpace(req.ProxyURL)
	for _, p := range proxies {
		if !p.Enabled {
			continue
		}
		if wantURL != "" && strings.TrimSpace(p.URL) != wantURL {
			continue // 指定了线路 → 只用它
		}
		// 用户提供的线路（owner>0）默认不进自动候选（v0.31.23），
		// 除非管理员显式勾了 include_user_lines（v0.31.32）。
		//
		// 风险等级：那条出口不在我们控制之下（是用户自己的代理）。
		// 「自动挑一条最闲的」会把**平台账号的 access_token** 从它送出去 ——
		// 而 access_token 是 bearer 凭证，谁看到谁就能冒充那个号。
		// 显式指名（卡片点「分配」）或显式勾选，才代表管理员知情。
		if wantURL == "" && p.OwnerUserID > 0 && !req.ForceScope {
			continue
		}
		usable = append(usable, p)
	}
	if len(usable) == 0 {
		if wantURL != "" {
			// 强制越界打到一条**被停用**的线路上：必须留一条 failed 审计（v0.31.52）。
			// 否则"我明明强制了"与"一条都没改"之间没有任何可查的东西 ——
			// 而这次操作恰恰是"越权分发凭证"的尝试，失败也要留痕。
			if req.ForceScope {
				target := egressLineByURL(proxies, wantURL)
				if target != nil {
					audit := &egressForceAudit{
						ProxyID: target.ID, ProxyURL: target.URL, UIDs: targets2UIDs(targets),
						Violations: map[string]string{}, Reason: strings.TrimSpace(req.ForceReason),
						Result: "failed",
					}
					if u := userFrom(r); u != nil {
						audit.AdminID, audit.AdminName = u.ID, u.Username
					}
					if err := a.store.InsertEgressForceAudit(audit); err != nil {
						log.Printf("WARN: [egress] 写失败审计出错：%v", err)
					}
				}
			}
			// 指定线路时把话说清：是"这条不在池里/被停用"，不是"池子空了"
			// —— 两者的下一步动作完全不同（去线路池找它 vs 去加线路）。
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "指定的线路不在可用线路池里（未登记或被停用）：" + wantURL,
			})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "出口代理池为空（或全部已停用）。请先添加线路；若只想让账号直连，请改用「设为直连」模式。",
		})
		return
	}

	// ⚠️ 自动模式下按**出口 IP** 折叠候选（v0.31.26）。
	//
	// 不折的后果是"看起来分散、其实同一个 IP"：分配算法按线路做负载均衡，
	// 同机多 agent 的两条线路会被轮流使用，于是一半的号被"均衡"到同一个公网
	// IP 上，而报表显示"已分散到 2 条线路"（2026-10-17 生产现场正是如此：
	// 2 条线路 last_ip 都是 39.91.80.16，38 个号全在这一个出口上）。
	//
	// 显式指名线路（wantURL != ""）时**不折**：管理员点的就是这条 URL，
	// 悄悄换成别的线路会让他以为自己点错了。
	mergedExits := map[string]string{}
	var dedupeWarn []string
	if wantURL == "" {
		usable, mergedExits, dedupeWarn = dedupeProxiesByExit(usable, map[string]int{})
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
	warnings = append(dedupeWarn, warnings...)

	// 唯一闸门（v0.31.50，需求二 / R1）：**写入前**逐个账号复核，不合格的进 blocked。
	//
	// 为什么放在计划之后而不是之前：planner 只按"负载均衡 + 号主隔离"挑线路，
	// 不知道业务范围；而范围取决于每条线路的 scope/owner 与每个账号的归属/入池状态，
	// 是 (账号, 线路) 的二元判断 —— 事后过滤是最不容易漂移的写法。
	byUID := make(map[string]*Account, len(targets))
	for _, acc := range targets {
		byUID[acc.UID] = acc
	}
	byURL := make(map[string]*EgressProxy, len(usable))
	for _, p := range usable {
		byURL[p.URL] = p
	}
	blocked := make([]map[string]any, 0)
	violations := map[string]string{}
	keep := plan[:0]
	for _, it := range plan {
		line := egressLineByURL(usable, it.Proxy)
		ok, code, reason := egressLineAllows(line, byUID[it.UID],
			egressAllowOpts{Force: req.ForceScope})
		if !ok {
			blocked = append(blocked, map[string]any{
				"uid": it.UID, "nickname": it.Nickname, "code": code, "reason": reason,
				"proxy": it.Proxy,
			})
			continue
		}
		if strings.HasPrefix(code, "forced_") {
			violations[it.UID] = code
		}
		keep = append(keep, it)
	}
	plan = keep
	_ = byURL
	if len(blocked) > 0 {
		warnings = append(warnings, fmt.Sprintf("有 %d 个账号因出口范围规则被拦下（见 blocked）；"+
			"确需越界请用 force_scope + force_reason", len(blocked)))
	}

	if !req.DryRun {
		for _, it := range plan {
			if err := a.store.SetAccountEgress(it.UID, it.Proxy); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
		}
		// 强制越界必须落审计（v0.31.50）：谁、对哪些号、什么理由、结果如何。
		// 这条链路会**把凭证送进别人的家庭网络**，日志会被轮转，表不会。
		if len(violations) > 0 || req.ForceScope {
			audit := &egressForceAudit{
				ProxyID: "", ProxyURL: req.ProxyURL, UIDs: []string{},
				Violations: violations, Reason: strings.TrimSpace(req.ForceReason),
				Result: "applied",
			}
			if u := userFrom(r); u != nil {
				audit.AdminID, audit.AdminName = u.ID, u.Username
			}
			if len(plan) > 0 {
				if line := egressLineByURL(usable, plan[0].Proxy); line != nil {
					audit.ProxyID, audit.ProxyURL = line.ID, line.URL
				}
			}
			for uid := range violations {
				audit.UIDs = append(audit.UIDs, uid)
			}
			sort.Strings(audit.UIDs)
			if len(blocked) > 0 && len(plan) == 0 {
				audit.Result = "failed"
			}
			if err := a.store.InsertEgressForceAudit(audit); err != nil {
				log.Printf("WARN: [egress] 写强制越界审计失败（分配已生效，请人工登记）：%v", err)
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
	// 分配结果落在几个**不同出口 IP** 上（这才是风控视角的"分散程度"：
	// 分到 2 条线路但同一个 IP = 1 个出口，见 egress_exit.go）。
	exitByURL := map[string]string{}
	for _, p := range usable {
		exitByURL[p.URL] = exitIPOfProxy(p)
	}
	usedExits := map[string]bool{}
	for _, it := range plan {
		if strings.TrimSpace(it.Proxy) == "" {
			continue
		}
		ip := exitByURL[it.Proxy]
		if ip == "" {
			ip = "unknown:" + it.Proxy // 未探到出口的线路各算一个，不能互相顶替
		}
		usedExits[ip] = true
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"assigned":       len(plan),
		"proxies_used":   len(perProxy),
		"distinct_exits": len(usedExits),
		"max_per_proxy":  maxPer,
		"isolate_owner":  isolate,
		"dry_run":        req.DryRun,
		"plan":           plan,
		"per_proxy":      perProxy,
		"blocked":        blocked,
		"warnings":       warnings,
		// merged_exits 被折叠的线路 → 代表线路。前端据此解释"为什么只用了 1 条"，
		// 而不是让人怀疑分配少派了线路。
		"merged_exits": mergedExits,
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
	// Enabled 是否启用（停用的线路不参与分配；已分配的账号会被判出口异常而
	// 排除出选号，见 computeEgressBad）。
	Enabled bool `json:"enabled"`
	// AutoDisabled 是否为**探活自动停用**（v0.31.47）。
	// 前端据此区分"等它自己好"与"要人处理"，两者处置完全不同。
	AutoDisabled bool `json:"auto_disabled"`
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
	// DistinctExitIPs 实际不同的出口 IP 个数（v0.31.26）。
	//
	// 与 len(Proxies) 的区别是本文件开头那段事故的全部要点：**线路条数不是出口数**。
	// 顶部告警与页面总览都以这个数为准来回答"我现在有几个 IP 在轮换"。
	DistinctExitIPs int `json:"distinct_exit_ips"`
	// SharedExits 落在同一个出口 IP 上的线路组（仅列 >=2 条的组）。
	SharedExits []egressExitGroup `json:"shared_exits"`
	// DefaultOwnerUser 全局默认出口若指向**用户线路**，这里是该用户 id（否则 0）。
	DefaultOwnerUser int64 `json:"default_owner_user"`
	// DefaultUserOwned = DefaultOwnerUser > 0。含义很具体：
	// 所有「未单独指定出口」的账号都会从**某个用户的家庭宽带**出去 ——
	// 既是越界使用别人的带宽，也会把平台号与该用户的号绑在同一个 IP 上。
	DefaultUserOwned bool `json:"default_user_owned"`
	// OrphanAccountEgress 账号上配了出口、但线路池里找不到这条线路的条数（v0.31.33）。
	//
	// 正常恒为 0：所有写入路径都走 setAccountEgress（规范化 + 登记），启动还会对齐一次。
	// 非 0 = 有人绕过接口直接改了库，或者 URL 无法规范化 —— 两种情况都会让
	// 「账号代理」与「出口代理」的 IP 对不上，所以要在页面上报出来并可一键对齐。
	OrphanAccountEgress int `json:"orphan_account_egress"`
	// ForeignBoundAccounts 号走在他人的线路上（存量借用 / 管理员显式分配）的账号数（v0.31.34）。
	//
	// 不是"错误"，但必须知情：这些号的上游请求经过另一台机器，
	// 那台机器一关机整批就掉；而且凭证对线路主人可见。
	ForeignBoundAccounts int `json:"foreign_bound_accounts"`
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
		v.Enabled, v.AutoDisabled = p.Enabled, p.AutoDisabled
		v.LastOK, v.LastIP = p.LastOK, p.LastIP
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

	// 出口身份：按 IP 归并（见 egress_exit.go）。
	ownerOf := map[string]int64{}
	for _, p := range proxies {
		ownerOf[p.URL] = p.OwnerUserID
	}
	items := make([]exitItem, 0, len(synced))
	for _, v := range synced {
		items = append(items, exitItem{
			URL: v.URL, Name: v.Name, IP: v.LastIP, Owner: ownerOf[v.URL], OK: v.LastOK,
		})
	}
	shared, distinct := groupExits(items)
	defOwner := int64(0)
	if strings.TrimSpace(defEgress) != "" {
		defOwner = ownerOf[defEgress]
	}

	return egressOverviewView{
		Proxies:              synced,
		DirectAccountCount:   directCount,
		DirectExplicit:       directExplicit,
		DirectOwners:         owners,
		Bad:                  badRows,
		MaxPerProxy:          maxPer,
		OverCount:            overCount,
		Unassigned:           unassigned,
		DefaultEgress:        defEgress,
		DistinctExitIPs:      distinct,
		SharedExits:          shared,
		DefaultOwnerUser:     defOwner,
		DefaultUserOwned:     defOwner > 0,
		OrphanAccountEgress:  a.orphanAccountEgress(),
		ForeignBoundAccounts: a.foreignBoundAccounts(),
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
		// 绑定数与来源机器（v0.31.19）：线路池此前只报"这条线路本身"，管理员
		// 看不出「这条线是谁拉起来的、现在被几个号用着」—— 而这两个事实恰好
		// 决定了"能不能下线它 / 要不要分配"。
		//
		// 挂在 GET 的**附加键**上（而不是塞进 EgressProxy 结构）：既有形状
		// （Go 字段名序列化）被前端大量引用，改形状等于把这一页全部重写。
		urls := make([]string, 0, len(list))
		items := make([]exitItem, 0, len(list))
		exitIPs := map[string]string{}
		for _, p := range list {
			urls = append(urls, p.URL)
			items = append(items, exitItem{
				URL: p.URL, Name: p.Name, IP: p.LastIP, Owner: p.OwnerUserID, OK: p.LastOK,
			})
			exitIPs[p.URL] = exitIPOfProxy(p)
		}
		// 出口身份（v0.31.26）：**条数 ≠ 不同 IP 数**。
		// 同一出口上挂多条线路（同机多 agent、同一家宽的多个入口）时，
		// 管理员必须一眼看出来「我这里其实只有 1 个 IP」，否则"分散分配"
		// 只会把号分散到同一个 IP 上（见 egress_exit.go 文件头）。
		groups, distinct := groupExits(items)
		writeJSON(w, http.StatusOK, map[string]any{
			"proxies":            list,
			"max_per_proxy":      a.store.egressMaxPerProxy(),
			"bound":              a.egressBoundByURL(urls),
			"bound_names":        a.egressBoundNamesByURL(urls),
			"owners":             a.egressOwnerNames(list),
			"machines":           a.egressMachinesByURL(),
			"exit_ip":            exitIPs,
			"shared_exit":        sharedExitOfURLs(groups),
			"shared_exit_groups": groups,
			"distinct_exit_ips":  distinct,
		})

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
			// 人工停用位与范围**只能通过专门接口改**（v0.31.50）：
			// 编辑名字/备注/备注顺手把 manual_disabled 清掉，等于"改个名字就把
			// 管理员的停用决定撤销了" —— 这类副作用必须在接口层挡死。
			p.ManualDisabled = old.ManualDisabled
			p.Scope = old.Scope
			// 管理员显式设 enabled 就是"人的决定"（v0.31.47）：
			// 清掉自动停用标记，否则下一次探活成功会把它自动拉活 ——
			// 而管理员刚刚明明手动停用了它（v0.31.34 踩过的同类坑）。
			p.AutoDisabled = false
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
	fresh, err := a.store.GetEgressProxy(p.ID)
	if err != nil || fresh == nil {
		return res
	}
	p.FailStreak = fresh.FailStreak
	enabled, auto, action := egressEnableDecision(res.OK, fresh.Enabled, fresh.AutoDisabled, fresh.FailStreak)
	if action != "" {
		p.Enabled, p.AutoDisabled = enabled, auto
		_ = a.store.UpsertEgressProxy(p)
		switch action {
		case "disable":
			log.Printf("[egress] 线路连续 %d 次探活失败，已自动停用（恢复后会自动启用）：%s（%s）",
				fresh.FailStreak, fresh.Name, fresh.URL)
		case "enable":
			log.Printf("INFO: [egress] 线路探活恢复，已自动重新启用：%s（%s）", fresh.Name, fresh.URL)
		}
	}
	return res
}

// egressEnableDecision 根据一次探活结果决定线路的启停怎么变（纯函数，v0.31.47）。
//
// 返回 (enabled, autoDisabled, action)，action ∈ {"", "disable", "enable"}。
// 抽成纯函数是因为这是本版最关键的判定，必须能被单测钉住：
//   - 连续失败达阈值且当前启用 → 自动停用并**打标记**；
//   - 探活通过且**打了标记的停用** → 自动恢复；
//   - 人工停用（标记为 false）→ 无论探活成功与否都不动它。
//
// 第三条是 v0.31.34 的教训：能无条件 `Enabled=true` 的地方，迟早会把
// 管理员刚停用的线路拉活。
func egressEnableDecision(probeOK, enabled, autoDisabled bool, failStreak int64) (bool, bool, string) {
	if !probeOK && failStreak >= egressFailThreshold && enabled {
		return false, true, "disable"
	}
	if probeOK && !enabled && autoDisabled {
		return true, false, "enable"
	}
	return enabled, autoDisabled, ""
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
	// 探活可能刚刚**停用或恢复**了线路（v0.31.47），而选号依赖的 egressBad
	// 是 SyncAccounts 时算的 —— 不在这里重算，"线路已停用"要等到下次
	// SyncAccounts 才会反映到选号上，中间这段时间账号会被派到一个死出口。
	// 反过来，"线路已恢复"也要立刻放回候选，否则用户拉起代理后仍要等一轮。
	a.proxy.RefreshEgressBad()
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

// handleEgressLineManualDisable POST /api/admin/egress/proxies/manual-disable
//
// body {"id":"<proxy id>","disabled":true,"note":"可选：为什么停"}
//
// 为什么单独一个接口（v0.31.50，R3/R10）：`enabled` 有三个来源（管理员开关、
// 探活自动停用、用户拉起时"通了就启用"）。把"人工停用"混进 enabled 里，
// 用户重新拉起代理就会把管理员的决定覆盖掉 —— v0.31.34 已经踩过一次。
// 所以停用要走这个**只改 manual_disabled** 的入口，且必须同时落 enabled=false。
func (a *app) handleEgressLineManualDisable(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID       string `json:"id"`
		Disabled *bool  `json:"disabled"`
		Note     string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	if strings.TrimSpace(req.ID) == "" || req.Disabled == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "需要 id 与 disabled"})
		return
	}
	p, err := a.store.GetEgressProxy(req.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if p == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "代理不存在"})
		return
	}
	p.ManualDisabled = *req.Disabled
	if p.ManualDisabled {
		p.Enabled = false
		// 人工停用与"自动停用"要分开记：前者不该被探活成功自动恢复。
		p.AutoDisabled = false
	} else {
		// 解锁时必须显式启用 —— 否则"解锁了但还是停用"，用户拉起也救不回来。
		p.Enabled, p.AutoDisabled, p.FailStreak = true, false, 0
	}
	if note := strings.TrimSpace(req.Note); note != "" {
		p.Note = note
	}
	if err := a.store.UpsertEgressProxy(p); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// 线路状态是选号输入（computeEgressBad）→ 必须重建。
	_ = a.proxy.SyncAccounts()
	log.Printf("INFO: [egress] 管理员把线路 %s（%s）人工停用=%v", p.ID, p.URL, p.ManualDisabled)
	writeJSON(w, http.StatusOK, map[string]any{"proxy": p})
}

// targets2UIDs 把目标账号转成 uid 列表（审计用，保持稳定顺序）。
func targets2UIDs(targets []*Account) []string {
	out := make([]string, 0, len(targets))
	for _, acc := range targets {
		if acc != nil && strings.TrimSpace(acc.UID) != "" {
			out = append(out, acc.UID)
		}
	}
	sort.Strings(out)
	return out
}
