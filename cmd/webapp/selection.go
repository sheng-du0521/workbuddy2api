// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// selection.go 选号可观测性（R7 + R8，v0.26.1）。
//
//   - R7 GET /api/admin/selection-balance  —— 选号**均衡度**指标（最高占比 / 覆盖账号数）
//   - R8 GET /api/admin/selection-candidates —— 候选队列预览（为什么排这 / 为什么跳过）
//
// ## R7 为什么必要（而不是"看着还行就不用管"）
//
// 我们的选号权重里额度项跨度 ×10、快过期占比 ×8，而闲置补偿封顶只有个位数 ——
// 与同类项目 `multi2api` **实测退化**的公式同构（它实测 11 个号分布 10:1、
// 4 个号完全没被用到，改完才回到 9/11）。它退化是因为**池内积分悬殊**，
// 我们池内相对均衡所以没退化（实测 24h 最高占比 7.9%，均匀应 4.3%）。
//
// 也就是说：**风险真实存在，只是当前没触发**。一旦出现大额充值拉开数量级差距，
// 分布会悄悄朝"额度排序"塌过去，而这件事**现在没有任何指标能看见**。
// 本接口把它变成可监控的一行数字，阈值 15%（约 3.5 倍偏斜）先给告警。
//
// ## R8 为什么必要
//
// 排障真正常问的不是"选号坏了吗"，而是"**为什么总选它 / 为什么跳过它**"。
// 判定散在四处（健康/在途/成本分层/防撞号），此前只能读代码猜。
package main

import (
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// balanceWindowHours 默认统计窗口（小时）。24h 足够平滑又不至于把昨天的历史混进来。
const balanceWindowHours = 24

// balanceWarnSharePct 最高占比告警阈值（百分比）。
//
// 为什么是 15：账号数 27 时均匀占比约 3.7%，15% 相当于**约 4 倍偏斜**；
// 同类项目实测退化态是 50%（单号占一半）。取 15 是"明显偏斜但尚未塌成额度排序"的位置，
// 留出足够提前量，又不至于天天误报。
//
// ⚠️ 但这个阈值**单独用是错的**（本功能第一版就是，被用例抓住）：占比的上限受账号数
// 约束 —— 池里只有 4 个号时，**完全均匀**也必然每个占 25%，绝对阈值会把健康分布判成告警，
// 于是告警立刻失去意义（运维第一天就学会忽略它）。
// 因此判据是「绝对占比 **且** 相对偏斜」两个条件同时成立，见 balanceWarnMinRatio。
const balanceWarnSharePct = 15.0

// balanceWarnMinRatio 相对偏斜下限（最高占比 / 理想占比）。
//
// 2.0 = 最热账号承担的流量是最均匀情形的两倍。与绝对阈值取**与**：
//
//	27 号健康态 7.9%  / 理想 3.7% → 比值 2.1，但绝对值 7.9% < 15% → 不告警 ✓
//	4 号完全均匀  25%  / 理想 25%  → 比值 1.0 < 2.0 → 不告警 ✓（这就是被抓住的误报）
//	4 号单号独占  97%  / 理想 25%  → 比值 3.9 ≥ 2.0 且 97% ≥ 15% → 告警 ✓
//	27 号退化态   50%  / 理想 3.7% → 比值 13.5 → 告警 ✓
const balanceWarnMinRatio = 2.0

// selectionBalanceView 均衡度视图。
type selectionBalanceView struct {
	WindowHours   int             `json:"window_hours"`
	TotalRequests int64           `json:"total_requests"`
	AccountsUsed  int             `json:"accounts_used"`
	AccountsTotal int             `json:"accounts_total"`
	CoveragePct   float64         `json:"coverage_pct"`
	IdealSharePct float64         `json:"ideal_share_pct"` // 均匀分布下每个账号应占的百分比
	MaxSharePct   float64         `json:"max_share_pct"`
	SkewRatio     float64         `json:"skew_ratio"` // 最高占比 / 理想占比（1.0 = 完全均匀）
	Median        int64           `json:"median"`
	Min           int64           `json:"min"`
	Top           []balanceTopRow `json:"top"`
	Warn          bool            `json:"warn"`
	Verdict       string          `json:"verdict"`
	Advice        string          `json:"advice"`
}

type balanceTopRow struct {
	UID      string  `json:"uid"`
	Count    int64   `json:"count"`
	SharePct float64 `json:"share_pct"`
}

// selectionBalance 计算均衡度。窗口内无请求时返回零值视图并给出"样本不足"的结论。
func (a *app) selectionBalance(hours int) (selectionBalanceView, error) {
	if hours <= 0 || hours > 30*24 {
		hours = balanceWindowHours
	}
	v := selectionBalanceView{WindowHours: hours, Top: []balanceTopRow{}}
	since := time.Now().Add(-time.Duration(hours) * time.Hour).Unix()

	rows, err := a.store.db.Query(
		`SELECT upstream_uid, COUNT(*) FROM usage_logs
		 WHERE created_at >= ? AND upstream_uid IS NOT NULL AND upstream_uid <> ''
		 GROUP BY upstream_uid ORDER BY 2 DESC`, since)
	if err != nil {
		return v, err
	}
	defer rows.Close()
	var counts []int64
	for rows.Next() {
		var uid string
		var n int64
		if err := rows.Scan(&uid, &n); err != nil {
			return v, err
		}
		counts = append(counts, n)
		v.TotalRequests += n
		v.Top = append(v.Top, balanceTopRow{UID: uid, Count: n})
	}
	if err := rows.Err(); err != nil {
		return v, err
	}
	// 池内账号总数（分母口径 = 当前池规模，不是"历史用过的账号数"）。
	if list, err := a.store.ListAccounts(); err == nil {
		v.AccountsTotal = len(list)
	}
	v.AccountsUsed = len(counts)

	if v.TotalRequests == 0 || v.AccountsUsed == 0 {
		v.Verdict, v.Advice = "no-sample", "窗口内没有带账号归属的流水，样本不足，无法判断均衡度。"
		return v, nil
	}
	denom := v.AccountsUsed
	if v.AccountsTotal > denom {
		denom = v.AccountsTotal // 理想占比按"本可以轮到的账号数"算，未用到的号要计入分母
	}
	v.IdealSharePct = 100.0 / float64(denom)
	v.MaxSharePct = float64(counts[0]) * 100.0 / float64(v.TotalRequests)
	v.SkewRatio = v.MaxSharePct / v.IdealSharePct
	v.Min = counts[len(counts)-1]
	v.Median = counts[len(counts)/2]
	for i := range v.Top {
		v.Top[i].SharePct = float64(v.Top[i].Count) * 100.0 / float64(v.TotalRequests)
	}
	if len(v.Top) > 10 {
		v.Top = v.Top[:10]
	}
	if v.AccountsTotal > 0 {
		v.CoveragePct = float64(v.AccountsUsed) * 100.0 / float64(v.AccountsTotal)
	}

	// 两个条件取「与」：绝对占比高，且相对偏斜确实明显。单看任一都会误报（见常量注释）。
	v.Warn = v.MaxSharePct >= balanceWarnSharePct && v.SkewRatio >= balanceWarnMinRatio
	switch {
	case v.Warn:
		v.Verdict = "skewed"
		v.Advice = fmt.Sprintf(
			"最高占比 %.1f%%（理想 %.1f%%，约 %.1f 倍偏斜）已同时越过 %.0f%% 与 %.1f 倍两个阈值。"+
				"参考：同类项目在此形态下实测退化成「额度排序」。处置顺序建议："+
				"①先看 top 账号的 credits 是不是远超其它号（大额充值/签到补分拉开数量级）；"+
				"②再看 minPickGap 是否被调成 0、账号数是否太少；"+
				"③最后才考虑压权重里额度项的跨度。**不要**在没看 ①② 之前直接改权重。",
			v.MaxSharePct, v.IdealSharePct, v.SkewRatio, balanceWarnSharePct, balanceWarnMinRatio)
	case v.AccountsTotal > 0 && v.CoveragePct < 50:
		v.Verdict = "low-coverage"
		v.Advice = fmt.Sprintf(
			"窗口内只用到了 %d/%d 个账号（覆盖 %.0f%%）。偏斜不算高，但覆盖偏低 —— "+
				"常见原因是有相当一部分账号在冷却/禁用中，或成本分层把它们长期挡在最优档之外。"+
				"用 /api/admin/selection-candidates 可以看到每个号被跳过的具体原因。",
			v.AccountsUsed, v.AccountsTotal, v.CoveragePct)
	default:
		v.Verdict = "ok"
		v.Advice = fmt.Sprintf("分布健康：最高占比 %.1f%%，约 %.1f 倍偏斜（均匀应为 %.1f 倍）。",
			v.MaxSharePct, v.SkewRatio, 1.0)
	}
	return v, nil
}

// handleSelectionBalance GET /api/admin/selection-balance?hours=24
func (a *app) handleSelectionBalance(w http.ResponseWriter, r *http.Request) {
	hours, _ := strconv.Atoi(r.URL.Query().Get("hours"))
	v, err := a.selectionBalance(hours)
	if err != nil {
		// 500 而不是 200：诊断接口的"查询失败"仍然是失败（同 seltrace 的 P3-9 口径）。
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// handleSelectionCandidates GET /api/admin/selection-candidates?model=<模型ID>&realm=<域>
//
// 返回当前池内**每个**账号的处境：进没进候选、为什么被跳过、权重与排名。
// 纯读，不出站，不改任何状态。
func (a *app) handleSelectionCandidates(w http.ResponseWriter, r *http.Request) {
	model := r.URL.Query().Get("model")
	realm := r.URL.Query().Get("realm")
	rows := a.proxy.pool.PreviewCandidates(model, realm)
	included := 0
	for _, c := range rows {
		if c.Included {
			included++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"model":      model,
		"realm":      realm,
		"total":      len(rows),
		"included":   included,
		"candidates": rows,
		"note": "只读预览：复现的是「如果现在发一个请求会怎么排」，不是上次实际的选择。" +
			"条件探索（搭车改道探未知号）会写状态，预览不做，故不体现。",
	})
}
