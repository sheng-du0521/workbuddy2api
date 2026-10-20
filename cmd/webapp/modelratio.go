// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// modelratio.go 官方模型倍率 / 免费标识的对齐与同步（v0.31.51，需求一）。
//
// ## 为什么需要它
//
// 用户看到的"这个模型贵不贵、免不免费"此前全靠猜：`/v1/models` 只回 id，
// 而计费真正用的是 `kv model_cost_1k:<model>`（实测积分/1k）。两套口径互不相干，
// 于是"官方说限时免费、我们却在扣点"这种账，谁也说不清。
//
// ## 口径（刻意并列，不合并）
//
//	official_credits  上游模型目录的 `credits` 原文（如 "x0.05"）—— **原样展示**，不参与计费
//	official_free     上游 `tags` 里的 `badge:限时免费…` —— 只做展示与公告触发
//	measured_cost_1k  `kv model_cost_1k:<model>` 的实测值 —— **计费只认这个**
//
// 为什么不拿 credits 直接算钱：`x0.05` 是"相对基准的倍率"还是"折扣"**没有实证**。
// 把它当倍率算，一旦语义相反就是算错钱；并列展示 + 差异可见，比猜一个语义安全。
//
// ## 同步
//
// 每天最多一次（启动后 3 分钟补跑）。**不新增出站**：用的是既有的模型目录接口
// （`FetchModels`），以及本地已有的实测成本 kv。变化落 `model_ratio_diffs`，
// 免费↔付费 与 倍率变更 各写一条站内公告（每轮最多一条，避免刷屏）。
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/upstream"
)

// kvModelRatioLastSync 上次同步时刻（unix 秒）。
const kvModelRatioLastSync = "model_ratio_last_sync"

// modelRatioSyncGap 两次自动同步的最小间隔。
//
// 24 小时是需求口径（每天凌晨自动同步）；这里用 20 小时留出"服务重启抖动"的余量，
// 免得因为一次重启把它推到第二天。
const modelRatioSyncGap = 20 * time.Hour

// modelRatio 一个模型的"官方口径 + 实测口径"。
type modelRatio struct {
	Model string `json:"model"`
	Name  string `json:"name"`
	// OfficialCredits 上游 credits 原文（可能为空 = 上游没声明）。
	OfficialCredits string `json:"official_credits"`
	// OfficialBadge 上游 tags 里的 badge 文本（如"限时免费"）。
	OfficialBadge string `json:"official_badge"`
	// OfficialFree 上游是否标了免费（badge 含"免费"）。
	OfficialFree bool `json:"official_free"`
	// MeasuredCost1k 实测积分/1k（本地 kv；0 且 MeasuredKnown=false = 尚未实测）。
	MeasuredCost1k float64 `json:"measured_cost_1k"`
	// MeasuredKnown 本地**有没有**这个模型的实测记录（v0.31.52）。
	//
	// 必须与"实测为 0"分开：没有记录 ≠ 测得 0。混为一谈时，
	// 每一个还没被实测过的模型都会被标成"口径待核对"（现场实测 44/51 条误报）。
	MeasuredKnown bool `json:"measured_known"`
	// Drift 两个口径"看起来不一致"（免费但实测在扣费 / 付费但实测为 0）。
	// 只做提示，不改计费。
	Drift     string `json:"drift,omitempty"`
	UpdatedAt int64  `json:"updated_at"`

	// BillingRatio 单模型**计费**倍率（v0.31.67，需求"按实际倍率调整计费规则"）。
	//
	// 与 MeasuredCost1k 的区别必须分清：
	//   - MeasuredCost1k 是**观测**（上游扣了我们多少积分/1k），用于估预扣；
	//   - BillingRatio 是**定价**（我们要向用户收多少倍），直接乘进扣费。
	//
	// 0 = 不参与（回落全局 pricing_multiplier）。这是刻意的默认：
	// 新增模型不会因为"没人配过"而被按 0 收费（0 会让服务变成免费）。
	BillingRatio float64 `json:"billing_ratio"`
	// BillingScope 作用域："" = 未启用模型级倍率；"pool" = 号池；"relay" = 第三方通道。
	//
	// 现在只有 pool 真正参与扣费（relay 通道随 P2 一起接）。保留字段是为了
	// 让库里先能区分"这条倍率是给谁定的"，避免 P2 时再做一次数据迁移。
	BillingScope string `json:"billing_scope"`
}

// modelRatioDiff 一次被记录下来的变化（待人工复核队列）。
type modelRatioDiff struct {
	ID        int64  `json:"id"`
	Model     string `json:"model"`
	Kind      string `json:"kind"` // free_switch / credits_change / new_model / cost_drift
	Old       string `json:"old"`
	New       string `json:"new"`
	CreatedAt int64  `json:"created_at"`
}

func (s *Store) migrateModelRatio() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS model_ratios (
			model             TEXT PRIMARY KEY,
			name              TEXT NOT NULL DEFAULT '',
			official_credits  TEXT NOT NULL DEFAULT '',
			official_badge    TEXT NOT NULL DEFAULT '',
			official_free     INTEGER NOT NULL DEFAULT 0,
			measured_cost_1k  REAL NOT NULL DEFAULT 0,
			measured_known    INTEGER NOT NULL DEFAULT 0,
			updated_at        INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS model_ratio_diffs (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			model      TEXT NOT NULL DEFAULT '',
			kind       TEXT NOT NULL DEFAULT '',
			old_value  TEXT NOT NULL DEFAULT '',
			new_value  TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_model_ratio_diffs_at ON model_ratio_diffs(created_at DESC)`,
	}
	for _, st := range stmts {
		if _, err := s.db.Exec(st); err != nil {
			return fmt.Errorf("migrate model_ratios: %w", err)
		}
	}
	if err := s.ensureColumn("model_ratios", "measured_known",
		"measured_known INTEGER NOT NULL DEFAULT 0"); err != nil {
		return fmt.Errorf("migrate model_ratios(measured_known): %w", err)
	}
	// 单模型计费倍率（v0.31.67）：billing_ratio=0 表示"不参与"，回落全局倍率。
	// 用 ensureColumn 传**列定义**（不是整条 ALTER）—— 这是本项目踩过的坑。
	if err := s.ensureColumn("model_ratios", "billing_ratio",
		"billing_ratio REAL NOT NULL DEFAULT 0"); err != nil {
		return fmt.Errorf("migrate model_ratios(billing_ratio): %w", err)
	}
	if err := s.ensureColumn("model_ratios", "billing_scope",
		"billing_scope TEXT NOT NULL DEFAULT ''"); err != nil {
		return fmt.Errorf("migrate model_ratios(billing_scope): %w", err)
	}
	// 存量回填：值非 0 的一定有记录。
	if _, err := s.db.Exec(`UPDATE model_ratios SET measured_known = 1
		WHERE measured_cost_1k > 0 AND measured_known = 0`); err != nil {
		return fmt.Errorf("migrate model_ratios(backfill): %w", err)
	}
	return nil
}

// ListModelRatios 全量倍率（按 model 排序，保证界面稳定）。
func (s *Store) ListModelRatios() ([]*modelRatio, error) {
	rows, err := s.db.Query(`SELECT model, name, official_credits, official_badge,
		official_free, measured_cost_1k, measured_known, updated_at,
		COALESCE(billing_ratio,0), COALESCE(billing_scope,'') FROM model_ratios ORDER BY model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*modelRatio
	for rows.Next() {
		var r modelRatio
		var free, known int
		if err := rows.Scan(&r.Model, &r.Name, &r.OfficialCredits, &r.OfficialBadge,
			&free, &r.MeasuredCost1k, &known, &r.UpdatedAt,
			&r.BillingRatio, &r.BillingScope); err != nil {
			return nil, err
		}
		r.OfficialFree = free != 0
		r.MeasuredKnown = known != 0
		r.Drift = modelRatioDrift(r.OfficialFree, r.MeasuredCost1k, r.MeasuredKnown)
		out = append(out, &r)
	}
	return out, rows.Err()
}

// modelRatioDrift 两个口径的"看起来不一致"。
//
// 刻意保守：只有在**证据明确**时才提示 —— 免费但实测在扣点、或付费但实测为 0。
// 上游 credits 为空、或本地还没实测（0）时一律不提示（未知不等于异常）。
func modelRatioDrift(free bool, measured float64, known bool) string {
	if !known {
		return "" // 还没实测过：未知不等于异常（这一条曾把 44/51 个模型误报成"口径待核对"）
	}
	switch {
	case free && measured > 0:
		return "官方标免费，但实测在扣点 —— 计费暂按实测执行，请人工核对"
	case !free && measured == 0:
		return "官方未标免费，但实测点为 0 —— 可能仍是赠送期，计费按实测执行"
	}
	return ""
}

// PutModelRatio upsert 一条（内部用）。
func (s *Store) PutModelRatio(r *modelRatio) error {
	_, err := s.db.Exec(`INSERT INTO model_ratios
		(model, name, official_credits, official_badge, official_free, measured_cost_1k,
		 measured_known, updated_at)
		VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(model) DO UPDATE SET
			name=excluded.name, official_credits=excluded.official_credits,
			official_badge=excluded.official_badge, official_free=excluded.official_free,
			measured_cost_1k=excluded.measured_cost_1k, measured_known=excluded.measured_known,
			updated_at=excluded.updated_at`,
		r.Model, r.Name, r.OfficialCredits, r.OfficialBadge, b2i(r.OfficialFree),
		r.MeasuredCost1k, b2i(r.MeasuredKnown), r.UpdatedAt)
	return err
}

// modelRatioDiffItem 一条待写入的差异（批次事务用）。
type modelRatioDiffItem struct {
	Model, Kind, Old, New string
}

// ApplyModelRatioBatch 在**一个事务**里写入本轮全部倍率与差异（v0.31.59，审查 F10）。
//
// 修复前的写法是逐模型 Put + AddDiff：第 N 个失败时前 N-1 个已经落库，
// 差异也可能写了而状态没写 —— 函数返回 error 并打日志说"保留旧口径"，
// 实际库里已经是半新半旧。审查用注入数据库错误复现：第二个模型失败时，
// 第一个已存在、差异记录为 2 条。
//
// 现在的语义是真正的"全成功或全不写"：调用方先把完整快照算好，再一次提交。
func (s *Store) ApplyModelRatioBatch(items []*modelRatio, diffs []modelRatioDiffItem) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	for _, d := range diffs {
		if _, err := tx.Exec(`INSERT INTO model_ratio_diffs (model, kind, old_value, new_value, created_at)
			VALUES (?,?,?,?,?)`, d.Model, d.Kind, d.Old, d.New, now); err != nil {
			return err
		}
	}
	for _, r := range items {
		if _, err := tx.Exec(`INSERT INTO model_ratios
			(model, name, official_credits, official_badge, official_free, measured_cost_1k,
			 measured_known, updated_at)
			VALUES (?,?,?,?,?,?,?,?)
			ON CONFLICT(model) DO UPDATE SET
				name=excluded.name, official_credits=excluded.official_credits,
				official_badge=excluded.official_badge, official_free=excluded.official_free,
				measured_cost_1k=excluded.measured_cost_1k, measured_known=excluded.measured_known,
				updated_at=excluded.updated_at`,
			r.Model, r.Name, r.OfficialCredits, r.OfficialBadge, b2i(r.OfficialFree),
			r.MeasuredCost1k, b2i(r.MeasuredKnown), r.UpdatedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GetModelRatio 单条（找不到返回 nil）。
func (s *Store) GetModelRatio(model string) (*modelRatio, error) {
	row := s.db.QueryRow(`SELECT model, name, official_credits, official_badge,
		official_free, measured_cost_1k, measured_known, updated_at FROM model_ratios WHERE model = ?`, model)
	var r modelRatio
	var free, known int
	err := row.Scan(&r.Model, &r.Name, &r.OfficialCredits, &r.OfficialBadge, &free,
		&r.MeasuredCost1k, &known, &r.UpdatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return nil, nil
		}
		return nil, err
	}
	r.OfficialFree = free != 0
	r.MeasuredKnown = known != 0
	r.Drift = modelRatioDrift(r.OfficialFree, r.MeasuredCost1k, r.MeasuredKnown)
	return &r, nil
}

// AddModelRatioDiff 记一条变化。
func (s *Store) AddModelRatioDiff(model, kind, oldV, newV string) error {
	_, err := s.db.Exec(`INSERT INTO model_ratio_diffs (model, kind, old_value, new_value, created_at)
		VALUES (?,?,?,?,?)`, model, kind, oldV, newV, time.Now().Unix())
	return err
}

// ListModelRatioDiffs 最近的变化（人工复核队列）。
func (s *Store) ListModelRatioDiffs(limit int) ([]*modelRatioDiff, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT id, model, kind, old_value, new_value, created_at
		FROM model_ratio_diffs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*modelRatioDiff
	for rows.Next() {
		var d modelRatioDiff
		if err := rows.Scan(&d.ID, &d.Model, &d.Kind, &d.Old, &d.New, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &d)
	}
	return out, rows.Err()
}

// modelBadgeOf 从上游 tags 里取 badge 文本（`badge:限时免费:#FF0000` → "限时免费"）。
func modelBadgeOf(tags []string) string {
	for _, t := range tags {
		if strings.HasPrefix(t, "badge:") {
			body := strings.TrimPrefix(t, "badge:")
			if i := strings.LastIndex(body, ":"); i > 0 {
				body = body[:i] // 去掉颜色段
			}
			return strings.TrimSpace(body)
		}
	}
	return ""
}

// measuredCost1k 读本地实测成本（kv `model_cost_1k:<model>`）。
// 第二个返回值表示"有没有这条记录"—— 与"测得 0"是两件事。
func (a *app) measuredCost1k(model string) (float64, bool) {
	v, err := a.store.GetKV("model_cost_1k:" + strings.ToLower(strings.TrimSpace(model)))
	if err != nil || strings.TrimSpace(v) == "" {
		return 0, false
	}
	var f float64
	if _, perr := fmt.Sscanf(strings.TrimSpace(v), "%f", &f); perr != nil {
		return 0, false
	}
	return f, true
}

// errModelRatioSyncBusy 已有一轮同步在进行（v0.31.59，审查 F10）。
var errModelRatioSyncBusy = errors.New("model ratio sync already running")

// modelRatioSyncResult 一次同步的结果（接口回显 + 日志）。
type modelRatioSyncResult struct {
	Total     int      `json:"total"`
	Added     int      `json:"added"`
	Changed   int      `json:"changed"`
	FreeFlip  int      `json:"free_flip"`
	Announced bool     `json:"announced"`
	Diffs     []string `json:"diffs,omitempty"`
	Skipped   string   `json:"skipped,omitempty"`
}

// syncModelRatios 从上游模型目录同步官方口径（不新增外呼：复用 FetchModels）。
//
// 失败一律**不改库**（宁可用昨天的口径，也不要把一屏"官方未声明"写进去）。
func (a *app) syncModelRatios() (modelRatioSyncResult, error) {
	var res modelRatioSyncResult
	// 单飞（v0.31.59，审查 F10）：定时任务与管理端按钮可能同时触发。
	// 两个 run 并发会各写一半差异、各发一条公告，事后无法还原"这轮改了什么"。
	if !a.modelRatioMu.TryLock() {
		return res, errModelRatioSyncBusy
	}
	defer a.modelRatioMu.Unlock()
	// 模型目录读取做成可注入钩子（v0.31.47 的教训：需要联网的验证点不注入，
	// 单测就只能测到"函数能跑"，测不到分支）。
	fetch := func() ([]upstream.ModelInfo, error) {
		au := a.proxy.pool.Pick("")
		if au == nil {
			return nil, nil
		}
		return a.proxy.uc.FetchModels(au)
	}
	if a.modelRatioFetch != nil {
		fetch = a.modelRatioFetch
	}
	infos, err := fetch()
	if err != nil {
		return res, err
	}
	if len(infos) == 0 {
		res.Skipped = "号池里没有可用账号或上游返回空模型列表，跳过（不改库）"
		return res, nil
	}
	now := time.Now().Unix()
	var freeFlips, creditChanges, added []string
	items := make([]*modelRatio, 0, len(infos))
	diffs := make([]modelRatioDiffItem, 0, 8)
	for _, mi := range infos {
		id := strings.TrimSpace(mi.ID)
		if id == "" {
			continue
		}
		badge := modelBadgeOf(mi.Tags)
		free := strings.Contains(badge, "免费")
		cost, known := a.measuredCost1k(id)
		next := &modelRatio{
			Model: id, Name: mi.Name,
			OfficialCredits: strings.TrimSpace(mi.Credits),
			OfficialBadge:   badge, OfficialFree: free,
			MeasuredCost1k: cost, MeasuredKnown: known,
			UpdatedAt: now,
		}
		prev, gerr := a.store.GetModelRatio(id)
		if gerr != nil {
			return res, gerr
		}
		res.Total++
		switch {
		case prev == nil:
			added = append(added, id)
			diffs = append(diffs, modelRatioDiffItem{Model: id, Kind: "new_model", New: next.OfficialCredits})
		default:
			if prev.OfficialFree != next.OfficialFree {
				kind := "free_switch"
				oldV, newV := "付费", "免费"
				if !next.OfficialFree {
					oldV, newV = "免费", "付费"
				}
				diffs = append(diffs, modelRatioDiffItem{Model: id, Kind: kind, Old: oldV, New: newV})
				freeFlips = append(freeFlips, id+"（"+oldV+"→"+newV+"）")
				res.FreeFlip++
			}
			if prev.OfficialCredits != next.OfficialCredits {
				diffs = append(diffs, modelRatioDiffItem{Model: id, Kind: "credits_change",
					Old: prev.OfficialCredits, New: next.OfficialCredits})
				creditChanges = append(creditChanges, fmt.Sprintf("%s（%s → %s）",
					id, orDash(prev.OfficialCredits), orDash(next.OfficialCredits)))
				res.Changed++
			}
		}
		items = append(items, next)
	}
	// 全部算完才写：任何一步失败都不留下半批数据。
	if err := a.store.ApplyModelRatioBatch(items, diffs); err != nil {
		log.Printf("WARN: [modelratio] 批次写入失败，本轮未生效（库内保持旧口径）：%v", err)
		return res, err
	}
	res.Added = len(added)
	res.Diffs = append(freeFlips, creditChanges...)

	// 公告：只在"用户会少付/多付"的变化上发声，且每轮最多一条。
	if len(freeFlips) > 0 || len(creditChanges) > 0 {
		title := "模型计费口径变更"
		var b strings.Builder
		if len(freeFlips) > 0 {
			b.WriteString("免费状态变化：" + strings.Join(freeFlips, "、") + "\n")
		}
		if len(creditChanges) > 0 {
			b.WriteString("官方倍率变化：" + strings.Join(creditChanges, "、") + "\n")
		}
		b.WriteString("\n计费仍以实测积分/1k 为准；如与你的预期不符，请联系管理员核对。")
		if _, err := a.store.CreateAnnouncement(title, b.String(), "warn", true, false, now, 0); err != nil {
			log.Printf("WARN: [modelratio] 写公告失败（口径已更新，公告稍后可手工补）：%v", err)
		} else {
			res.Announced = true
		}
	}
	if err := a.store.SetKV(kvModelRatioLastSync, fmt.Sprintf("%d", now)); err != nil {
		log.Printf("WARN: [modelratio] 写同步时刻失败：%v", err)
	}
	log.Printf("INFO: [modelratio] 同步完成 total=%d added=%d credits_changed=%d free_flip=%d announced=%v",
		res.Total, res.Added, res.Changed, res.FreeFlip, res.Announced)
	return res, nil
}

// orDash 空值显示为 "—"（公告里别出现空括号）。
func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

// modelRatioLoop 后台同步（启动后 3 分钟补跑一次，之后每小时检查是否到期）。
//
// 为什么不用固定凌晨定时：容器/家用机器可能整夜关机，固定时刻会一直错过；
// 「上次同步超过 20 小时就补」在任何开机时间下都能收敛。
func (a *app) modelRatioLoop(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(3 * time.Minute):
	}
	for {
		if a.modelRatioDue() {
			if _, err := a.syncModelRatios(); err != nil {
				// 与手动入口撞车是正常竞争，不是故障，别刷 WARN。
				if errors.Is(err, errModelRatioSyncBusy) {
					log.Printf("INFO: [modelratio] 已有一轮同步在跑，本轮跳过")
				} else {
					log.Printf("WARN: [modelratio] 同步失败（本轮未写库，库内保持旧口径）：%v", err)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Hour):
		}
	}
}

// modelRatioDue 是否到了该同步的时候。
func (a *app) modelRatioDue() bool {
	v, err := a.store.GetKV(kvModelRatioLastSync)
	if err != nil || strings.TrimSpace(v) == "" {
		return true
	}
	var last int64
	if _, err := fmt.Sscanf(strings.TrimSpace(v), "%d", &last); err != nil {
		return true
	}
	return time.Since(time.Unix(last, 0)) >= modelRatioSyncGap
}

// handleModelRatios GET /api/models/ratios
//
// 用户侧只读：官方倍率原文 + 免费标识 + 实测积分/1k + 口径不一致提示。
func (a *app) handleModelRatios(w http.ResponseWriter, r *http.Request) {
	list, err := a.store.ListModelRatios()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	last, _ := a.store.GetKV(kvModelRatioLastSync)
	free := make([]string, 0, 4)
	for _, m := range list {
		if m.OfficialFree {
			free = append(free, m.Model)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"models": list, "free_models": free, "last_sync_at": last,
		"note": "官方倍率（credits）原样展示，不参与计费；扣点以 measured_cost_1k（实测积分/1k）为准",
	})
}

// handleAdminModelRatios GET /api/admin/model-ratios
func (a *app) handleAdminModelRatios(w http.ResponseWriter, r *http.Request) {
	list, err := a.store.ListModelRatios()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	diffs, err := a.store.ListModelRatioDiffs(egressAtoi(r.URL.Query().Get("limit"), 50))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	drift := make([]*modelRatio, 0, 2)
	for _, m := range list {
		if m.Drift != "" {
			drift = append(drift, m)
		}
	}
	last, _ := a.store.GetKV(kvModelRatioLastSync)
	writeJSON(w, http.StatusOK, map[string]any{
		"models": list, "diffs": diffs, "drift": drift, "last_sync_at": last,
	})
}

// handleAdminModelRatioSync POST /api/admin/model-ratios/sync
//
// 管理员可手动触发（比如刚拿到官方调价通知）。返回本次的变化明细。
func (a *app) handleAdminModelRatioSync(w http.ResponseWriter, r *http.Request) {
	res, err := a.syncModelRatios()
	if err != nil {
		// 单飞冲突不是"内部错误"：明确回 409 并说明"已有一轮在跑"，
		// 否则运维会看到 500 并反复点（审查 F10）。
		if errors.Is(err, errModelRatioSyncBusy) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": "已有一轮模型口径同步在进行，请稍后再试",
				"code":  "sync_in_progress",
			})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}
