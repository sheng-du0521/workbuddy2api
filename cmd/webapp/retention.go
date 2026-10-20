// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 用量流水的**保留策略**（v0.31.66，长期技术债"usage_logs 无保留策略"）。
//
// # 为什么必须有
//
// `usage_logs` 是**每次请求一行**的表：本项目一天几百到几千行，看起来很小；
// 但它是 SQLite（单文件、单连接），行数上去之后：
//
//   - 报表查询（按日/按模型聚合）越来越慢，还占着唯一那条连接；
//   - 库文件只增不减 —— 删了行也不会自动缩小（需要 VACUUM/增量回收）；
//   - 备份体积跟着涨（README 里的 Online Backup 指引会把这个成本放大）。
//
// 而**绝大多数用途只需要最近几十天**：账单页按日聚合、分位报表看最近 5 万条、
// 对账看六条不变式。三年前某一次请求的 prompt_tokens 没有第二个读者。
//
// # 取舍（刻意保守）
//
//   - 默认 **180 天**，而不是"30 天"这种激进值：删数据是不可逆的，宁可晚删；
//   - 设为 `0` 或负数 = **永久保留**（有些部署就是要留全量做审计）；
//   - **分批删**（每批 2000 行）：一次性 `DELETE ... WHERE created_at < ?` 在几十万行上
//     会长时间持有写锁，把正在结算的请求堵住 —— 那是"清理把线上搞挂"的经典形态；
//   - 每次运行把结果落 kv（删了多少、删到哪一天），后台接口可查 —— 静默的清理比不清理更危险；
//   - **不自动 VACUUM**：VACUUM 需要整库重写（期间锁死），必须由运维在低峰手动做。
//     这里只把"该做了"的事实摆到接口上（`reclaimable_bytes`）。
package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// kvUsageRetentionDays 保留天数（字符串）。未设置走默认；"0"/负数 = 永久保留。
	kvUsageRetentionDays = "usage_log_retention_days"
	// kvUsagePruneLast 上次清理结果（JSON，程序自写）。
	kvUsagePruneLast = "usage_log_prune_last"

	// defaultUsageRetentionDays 默认保留 180 天。
	//
	// 为什么不是 30：删数据不可逆。180 天覆盖"上一个季度对账 + 一次年度复核"，
	// 而规模上 180 天的流水仍然只是几万行 —— 真正需要担心的是三五年。
	defaultUsageRetentionDays = 180

	// usagePruneBatch 每批删除行数。小批量 = 短事务 = 不堵结算。
	usagePruneBatch = 2000
	// usagePruneStartDelay 启动后延迟执行（不与启动期迁移/对账抢连接）。
	usagePruneStartDelay = 2 * time.Minute
	// usagePruneEvery 每天一次。清理不需要更频繁。
	usagePruneEvery = 24 * time.Hour
)

// UsagePruneReport 一次清理的结果（落 kv 并原样下发给后台）。
type UsagePruneReport struct {
	At        int64  `json:"at"`
	Reason    string `json:"reason"`
	Retention int    `json:"retention_days"` // 生效的保留天数（<=0 表示永久）
	Cutoff    int64  `json:"cutoff"`         // 删除 created_at < cutoff 的记录
	Deleted   int64  `json:"deleted"`
	Skipped   bool   `json:"skipped"` // true = 保留策略为"永久"，什么都没删
	Error     string `json:"error,omitempty"`
	Duration  int64  `json:"duration_ms"`
}

// usageRetentionDays 生效的保留天数：未设置走默认，0/负数 = 永久保留。
func (a *app) usageRetentionDays() int {
	v, err := a.store.GetKV(kvUsageRetentionDays)
	if err != nil || strings.TrimSpace(v) == "" {
		return defaultUsageRetentionDays
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		// 写错了**不能**当成 0（=永久保留），也不能当成默认值悄悄删数据 ——
		// 两个方向都会让人意外。退回默认并留一条日志，让人知道配置没生效。
		log.Printf("WARN: [retention] usage_log_retention_days=%q 不是整数，按默认 %d 天处理",
			v, defaultUsageRetentionDays)
		return defaultUsageRetentionDays
	}
	return n
}

// PruneUsageLogs 分批删除 created_at < before 的用量流水，返回删除行数。
//
// 分批的意义见文件头：**长事务会把唯一那条连接占住**，而后台还要结算。
// 每批之间让出连接（Exec 结束即释放），于是清理再久也不会让请求卡住。
func (s *Store) PruneUsageLogs(before int64, batch int) (int64, error) {
	if before <= 0 {
		return 0, nil
	}
	if batch <= 0 || batch > 20000 {
		batch = usagePruneBatch
	}
	var total int64
	for {
		res, err := s.db.Exec(
			`DELETE FROM usage_logs WHERE id IN
			 (SELECT id FROM usage_logs WHERE created_at < ? ORDER BY id LIMIT ?)`,
			before, batch)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
		if n < int64(batch) {
			return total, nil // 删完了
		}
	}
}

// runUsagePrune 执行一次清理并落 kv。
func (a *app) runUsagePrune(reason string) UsagePruneReport {
	start := time.Now()
	rep := UsagePruneReport{At: start.Unix(), Reason: reason, Retention: a.usageRetentionDays()}
	if rep.Retention <= 0 {
		rep.Skipped = true
		rep.Duration = time.Since(start).Milliseconds()
		log.Printf("INFO: [retention] 保留策略为永久（retention_days=%d），跳过清理", rep.Retention)
		a.savePruneReport(rep)
		return rep
	}
	rep.Cutoff = start.AddDate(0, 0, -rep.Retention).Unix()
	n, err := a.store.PruneUsageLogs(rep.Cutoff, usagePruneBatch)
	rep.Deleted = n
	rep.Duration = time.Since(start).Milliseconds()
	if err != nil {
		rep.Error = err.Error()
		log.Printf("WARN: [retention] 清理失败（已删 %d 行）：%v", n, err)
	} else if n > 0 {
		log.Printf("INFO: [retention] 已清理 %d 行用量流水（早于 %s，保留 %d 天，%dms）",
			n, time.Unix(rep.Cutoff, 0).Format("2006-01-02"), rep.Retention, rep.Duration)
	}
	a.savePruneReport(rep)
	return rep
}

func (a *app) savePruneReport(rep UsagePruneReport) {
	if blob, err := json.Marshal(rep); err == nil {
		_ = a.store.SetKV(kvUsagePruneLast, string(blob))
	}
}

func (a *app) lastPruneReport() (UsagePruneReport, bool) {
	var rep UsagePruneReport
	v, _ := a.store.GetKV(kvUsagePruneLast)
	if strings.TrimSpace(v) == "" {
		return rep, false
	}
	if err := json.Unmarshal([]byte(v), &rep); err != nil {
		return rep, false
	}
	return rep, true
}

// pruneLoop 启动后延迟一次，之后每 24 小时一次。
func (a *app) pruneLoop(ctx context.Context) {
	timer := time.NewTimer(usagePruneStartDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	a.runUsagePrune("startup")
	tick := time.NewTicker(usagePruneEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			a.runUsagePrune("periodic")
		}
	}
}

// dataUsageSnapshot 数据体积/行数快照（后台展示用）。
//
// 只读：`PRAGMA page_count/page_size` 比 os.Stat 更能反映**真实占用**
// （WAL 模式下主文件不缩，但页数会说明"能 reclaim 多少"）。
type dataUsageSnapshot struct {
	DBPath          string `json:"db_path"`
	FileBytes       int64  `json:"file_bytes"`
	WALBytes        int64  `json:"wal_bytes"`
	PageCount       int64  `json:"page_count"`
	PageSize        int64  `json:"page_size"`
	FreePages       int64  `json:"free_pages"`
	ReclaimableByte int64  `json:"reclaimable_bytes"`
	UsageLogs       int64  `json:"usage_logs"`
	OldestLog       int64  `json:"oldest_log"`
	NewestLog       int64  `json:"newest_log"`
	RetentionDays   int    `json:"retention_days"`
	LastPrune       any    `json:"last_prune,omitempty"`
}

func (a *app) dataUsage() dataUsageSnapshot {
	s := dataUsageSnapshot{RetentionDays: a.usageRetentionDays()}
	if a.dataDir != "" {
		s.DBPath = filepath.Join(a.dataDir, "webapp.db")
		if st, err := os.Stat(s.DBPath); err == nil {
			s.FileBytes = st.Size()
		}
		if st, err := os.Stat(s.DBPath + "-wal"); err == nil {
			s.WALBytes = st.Size()
		}
	}
	_ = a.store.db.QueryRow(`PRAGMA page_count`).Scan(&s.PageCount)
	_ = a.store.db.QueryRow(`PRAGMA page_size`).Scan(&s.PageSize)
	_ = a.store.db.QueryRow(`PRAGMA freelist_count`).Scan(&s.FreePages)
	s.ReclaimableByte = s.FreePages * s.PageSize
	_ = a.store.db.QueryRow(`SELECT COUNT(*), COALESCE(MIN(created_at),0), COALESCE(MAX(created_at),0)
		FROM usage_logs`).Scan(&s.UsageLogs, &s.OldestLog, &s.NewestLog)
	if rep, ok := a.lastPruneReport(); ok {
		s.LastPrune = rep
	}
	return s
}

// handleAdminDataUsage 后台查询数据体积与清理结果。
//
//	GET /api/admin/data-usage           → 快照（只读）
//	GET /api/admin/data-usage?prune=1   → 立即清理一次（按当前保留策略）
//
// `?prune=1` 是有副作用的，但它**只删自己过期的流水**、且保留策略由管理员显式配置；
// 这里不做 CSRF 之外的额外限制（与"立即对账"同样的取舍：先看得到，才好判断）。
func (a *app) handleAdminDataUsage(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{"snapshot": a.dataUsage()}
	if r.URL.Query().Get("prune") == "1" {
		resp["prune"] = a.runUsagePrune("manual")
		resp["snapshot"] = a.dataUsage() // 清完再取一次，让页面直接看到变化
	}
	writeJSON(w, http.StatusOK, resp)
}
