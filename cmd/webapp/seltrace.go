// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"net/http"
	"sort"
	"strconv"
	"time"
)

// stickySwitchRow 一个会话键（sticky_key_hash）的选号汇总。
//
// 用途：直接回答「同一场对话有没有在调用过程中换号（消耗了不同账号的积分）」。
// 判定只看一件事 —— Accounts 的长度：>1 即该会话被多个账号服务过。
type stickySwitchRow struct {
	StickyKeyHash string   `json:"sticky_key_hash"`
	Accounts      []string `json:"accounts"`
	Requests      int      `json:"requests"`
	FailoverRuns  int      `json:"failover_runs"` // attempts>1 的次数（失败换号回退）
	FirstAt       int64    `json:"first_at"`
	LastAt        int64    `json:"last_at"`
}

// stickyTrace 管理层诊断：按会话聚合最近流水，列出「被多个账号服务过」的会话。
//
// GET /api/admin/selection-trace?minutes=1440&limit=50
//
// 为什么要有它：v0.17.5 之前流水只有最终 upstream_uid，没有任何会话维度信息 ——
// 「同一场对话是否换号」只能靠请求时间聚类去猜（不可证伪）。补上 sticky_key_hash 后，
// 这个问题变成一次可复现的查询。它是排查工具，不参与任何运行时决策。
// 返回 error 而不是把错误塞进 map（v0.24.1，P3-9）：此前调用方无论成败都回 HTTP 200，
// 于是「查询失败」在客户端看来是「成功但结果里有个 error 字段」——
// 前端要额外判断，监控也统计不到失败率。现在由调用方按 error 决定状态码。
func (a *app) stickyTrace(minutes, limit int) (map[string]any, error) {
	if minutes <= 0 {
		minutes = 24 * 60
	}
	if minutes > 30*24*60 {
		minutes = 30 * 24 * 60
	}
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	since := time.Now().Add(-time.Duration(minutes) * time.Minute).Unix()

	rows, err := a.store.db.Query(
		`SELECT sticky_key_hash, upstream_uid, COUNT(*), SUM(CASE WHEN attempts > 1 THEN 1 ELSE 0 END),
		        MIN(created_at), MAX(created_at)
		 FROM usage_logs
		 WHERE sticky_key_hash <> '' AND created_at >= ?
		 GROUP BY sticky_key_hash, upstream_uid`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type agg struct {
		accounts []string
		requests int
		failover int
		first    int64
		last     int64
	}
	byKey := map[string]*agg{}
	for rows.Next() {
		var sh, uid string
		var n, failover int
		var first, last int64
		if err := rows.Scan(&sh, &uid, &n, &failover, &first, &last); err != nil {
			continue
		}
		g := byKey[sh]
		if g == nil {
			g = &agg{}
			byKey[sh] = g
		}
		if uid != "" {
			g.accounts = append(g.accounts, uid)
		}
		g.requests += n
		g.failover += failover
		if first != 0 && (g.first == 0 || first < g.first) {
			g.first = first
		}
		if last > g.last {
			g.last = last
		}
	}

	all := make([]stickySwitchRow, 0, len(byKey))
	// 非 nil 初始化：无换号会话时序列化为 []，而不是 null（客户端按数组消费，
	// null 会让「for (const x of d.switching)」这类代码直接抛错）。
	switching := []stickySwitchRow{}
	for sh, g := range byKey {
		sort.Strings(g.accounts)
		row := stickySwitchRow{
			StickyKeyHash: sh, Accounts: g.accounts, Requests: g.requests,
			FailoverRuns: g.failover, FirstAt: g.first, LastAt: g.last,
		}
		all = append(all, row)
		if len(g.accounts) > 1 {
			switching = append(switching, row)
		}
	}
	// 换号会话优先展示（按换号幅度降序），其次按请求量。
	sort.Slice(switching, func(i, j int) bool {
		if len(switching[i].Accounts) != len(switching[j].Accounts) {
			return len(switching[i].Accounts) > len(switching[j].Accounts)
		}
		return switching[i].Requests > switching[j].Requests
	})
	if len(switching) > limit {
		switching = switching[:limit]
	}
	// 全部会话按最近活跃排序（截断），供核对「没见过换号的正常会话长什么样」。
	sort.Slice(all, func(i, j int) bool { return all[i].LastAt > all[j].LastAt })
	if len(all) > limit {
		all = all[:limit]
	}

	return map[string]any{
		"window_minutes":     minutes,
		"sessions":           len(byKey),
		"switching_sessions": len(switching),
		"switching":          switching,
		"sessions_sample":    all,
		"note": "switching_accounts>1 的会话 = 同一场对话被多个号池账号服务过（积分来自不同账号）。" +
			"常见原因：a) 粘性绑定号当时不可用（冷却/模型限额/在途占满）被重分配；" +
			"b) 粘性键变化（客户端不带 conversation_id 且首条 user 消息每轮不同）；" +
			"c) v0.17.5 之前的进程重启会清空全部粘性绑定。failover_runs>0 则是单次请求内的失败换号回退。",
	}, nil
}

// handleSelectionTrace 管理端选号轨迹诊断。
func (a *app) handleSelectionTrace(w http.ResponseWriter, r *http.Request) {
	minutes, _ := strconv.Atoi(r.URL.Query().Get("minutes"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	v, err := a.stickyTrace(minutes, limit)
	if err != nil {
		// 500 而不是 200（v0.24.1，P3-9）：这是诊断接口，但"查询失败"仍是失败。
		// 具体错误由 writeJSON 的脱敏层处理（不回显 SQL 细节）。
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, v)
}
