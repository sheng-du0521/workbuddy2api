// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 账目对账（v0.31.62，审查报告 F02 的最后一块）。
//
// # 为什么"事务都修好了"还需要对账
//
// v0.31.60 把一笔结算的四个落点放进了同一个事务，解决了"写到一半"的问题。
// 但事务只保证**单笔**一致，管不了这些：
//
//   - 历史数据：v0.31.60 之前的半截账（当时就是四次提交）已经落在库里了；
//   - 事务之外：导库、手工改库、运维脚本补数据，都可能破坏不变量；
//   - 代码演进：将来谁在某个分支又写了第二次"扣费"或第二次"发分成"；
//   - 极端崩溃：事务提交前后被强杀（预扣残留已由启动对账兜底，但要知道它发生过）。
//
// 这些情况的共同点是**没人会发现**：账偏了几点，页面上一切正常，直到用户提现时
// 才暴露，而那时已经难以回溯。对账任务的价值就是"把静默的偏移变成一条告警"。
//
// # 设计取舍
//
//   - **只读**：全部是 SELECT，不改任何数据。发现问题先报，修是人的决定
//     （自动修正会把"一次误判"直接变成"改错账"）。
//   - **不变式而不是流水比对**：不做"逐笔重算"，而是检查几条必须永远成立的关系。
//     逐笔重算需要把 30 天的计费规则都实现一遍，规则一变就得同步改两处 ——
//     那正是本项目最贵的一类缺陷。
//   - **结果落 kv**（`ledger_check_last`）：面板/接口随时能看最近一次，不必等下次调度。
//   - **不阻塞启动**：延迟 90 秒、后台跑，失败只记日志。
package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"
)

const (
	kvLedgerCheckLast = "ledger_check_last"

	// ledgerCheckStartDelay / ledgerCheckEvery 调度节奏。
	// 频率选 6 小时：对账是"发现异常"，不是"实时监控"；太频繁只会刷日志。
	ledgerCheckStartDelay = 90 * time.Second
	ledgerCheckEvery      = 6 * time.Hour

	// reservationLeakSec 预扣残留的容忍时长。
	// 正常请求的预扣存活时间是"一次上游请求"，秒级；超过 1 小时只可能是
	// 进程崩溃留下的孤儿（启动对账会清掉，但清掉也要留证据）。
	reservationLeakSec = 3600

	// hostOverpayDays 检查最近多少天的"分成+返点 ≤ 平台实收"。
	hostOverpayDays = 14
)

// LedgerFinding 一条对账发现。
//
// Severity 只有两档：
//   - high：钱的硬约束被破坏（必须马上看，通常意味着代码或数据被改坏）；
//   - warn：口径性偏差（可能是历史数据/人工操作，需要判断）。
//
// 不设 info：对账任务不输出"一切正常"的条目，正常就是空列表 —— 空列表比一百条 ok 更好读。
type LedgerFinding struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Count    int64  `json:"count"`
	Detail   string `json:"detail"`
}

// LedgerCheckReport 一次对账的完整结果（原样下发给后台）。
type LedgerCheckReport struct {
	At         int64           `json:"at"`
	Reason     string          `json:"reason"`
	DurationMS int64           `json:"duration_ms"`
	Findings   []LedgerFinding `json:"findings"`
	Error      string          `json:"error,omitempty"`
}

// OK 没有 high 级发现 = 可以认为账目处于一致状态。
func (r LedgerCheckReport) OK() bool {
	for _, f := range r.Findings {
		if f.Severity == "high" {
			return false
		}
	}
	return true
}

// LedgerCheck 跑一遍全部不变式，返回**只包含被违反项**的列表。
//
// 全部用 COUNT/SUM 而不是拉全表到内存：生产库已经有几十万行 usage_logs，
// 拉出来再算会让对账任务自己变成负载源。
func (s *Store) LedgerCheck(now time.Time) ([]LedgerFinding, error) {
	var out []LedgerFinding

	count := func(q string, args ...any) (int64, error) {
		var n int64
		err := s.db.QueryRow(q, args...).Scan(&n)
		return n, err
	}
	add := func(code, sev string, n int64, detail string) {
		if n > 0 {
			out = append(out, LedgerFinding{Code: code, Severity: sev, Count: n, Detail: detail})
		}
	}

	// ① 可提现额度不得超过余额（v0.31.59/F01 修的就是这条）。
	// 越界意味着"能提的钱比账上有的多"，是最直接的资损方向。
	if n, err := count(`SELECT COUNT(*) FROM users
		WHERE withdrawable_micro > balance_micro OR withdrawable_micro < 0`); err != nil {
		return out, err
	} else {
		add("withdrawable_overflow", "high", n, "可提现额度 > 余额，或为负")
	}

	// ② 冻结额不得为负（提现冻结是"提交即冻结"，只增不减到结算为止）。
	if n, err := count(`SELECT COUNT(*) FROM users WHERE frozen_micro < 0`); err != nil {
		return out, err
	} else {
		add("frozen_negative", "high", n, "用户冻结额为负")
	}

	// ③ 预扣残留超时：进程崩溃留下的孤儿。启动对账会退还，但"发生过"要看得见 ——
	// 频繁出现说明进程在被反复强杀，那是另一个问题。
	cutoff := now.Unix() - reservationLeakSec
	if n, err := count(`SELECT COUNT(*) FROM reservations WHERE created_at < ?`, cutoff); err != nil {
		return out, err
	} else {
		add("reservation_leak", "high", n, "预扣超过 1 小时仍未结算/释放（进程崩溃遗留）")
	}

	// ④ 管理通道（user_id=0）不该产生扣费：它无用户可扣，出现非零即代码走错了分支。
	if n, err := count(`SELECT COUNT(*) FROM usage_logs WHERE user_id = 0 AND charged_micro <> 0`); err != nil {
		return out, err
	} else {
		add("admin_channel_charged", "high", n, "管理通道流水出现非零扣费")
	}

	// ⑤ 账号累计收益 == 分成台账之和（同事务写入，必须逐账号相等）。
	// 用 warn 而不是 high：历史版本可能只写了其中一边，需要人判断是否能重建。
	if n, err := count(`SELECT COUNT(*) FROM accounts a
		WHERE a.earned_micro <> COALESCE(
			(SELECT SUM(h.earned_micro) FROM host_earnings h WHERE h.account_uid = a.uid), 0)`); err != nil {
		return out, err
	} else {
		add("account_earned_mismatch", "warn", n, "accounts.earned_micro 与 host_earnings 汇总不一致")
	}

	// ⑥ 钱的硬约束：同一天内"分成 + 返点"不得超过"平台实收"。
	// 逐日汇总而不是逐笔：逐笔需要把计费规则重算一遍（见文件头的取舍说明）。
	// 只查最近 hostOverpayDays 天：更早的数据可能来自 v0.31.60 之前的已知缺陷，
	// 把它当作"当前告警"只会训练大家忽略告警。
	since := now.AddDate(0, 0, -hostOverpayDays).Unix()
	if n, err := count(`SELECT COUNT(*) FROM (
			SELECT date(h.created_at,'unixepoch','localtime') d, SUM(h.earned_micro) e
			FROM host_earnings h WHERE h.created_at >= ? GROUP BY 1
		) x
		-- 必须 LEFT JOIN：某天可能**没有**返点或**没有**扣费流水，
		-- 用 INNER JOIN 会把这类天直接跳过 —— 恰恰是最该查的"发了钱却没收到钱"。
		LEFT JOIN (
			SELECT date(r.created_at,'unixepoch','localtime') d, SUM(r.rebate_micro) r
			FROM rebates r WHERE r.created_at >= ? GROUP BY 1
		) rr ON rr.d = x.d
		LEFT JOIN (
			SELECT date(u.created_at,'unixepoch','localtime') d, SUM(u.charged_micro) c
			FROM usage_logs u WHERE u.created_at >= ? AND u.user_id > 0 AND u.charged_micro > 0
			GROUP BY 1
		) y ON y.d = x.d
		WHERE x.e + COALESCE(rr.r, 0) > COALESCE(y.c, 0)`, since, since, since); err != nil {
		return out, err
	} else {
		add("host_overpay", "high", n, "单日「分成+返点」超过「平台实收」（平台倒贴）")
	}

	return out, nil
}

// runLedgerCheck 跑一次并把结果落到 kv（后台接口与日志共用这一份）。
//
// **不在锁里**：对账全是只读 SELECT，与写路径并发是安全的（SQLite 单连接下
// 也只是排队执行）。加锁反而会让它和结算互相阻塞。
func (a *app) runLedgerCheck(reason string) LedgerCheckReport {
	start := time.Now()
	rep := LedgerCheckReport{At: start.Unix(), Reason: reason}
	findings, err := a.store.LedgerCheck(start)
	rep.DurationMS = time.Since(start).Milliseconds()
	if err != nil {
		rep.Error = err.Error()
		log.Printf("WARN: [ledger] 对账执行失败（%s）：%v", reason, err)
	} else {
		rep.Findings = findings
	}
	if blob, jerr := json.Marshal(rep); jerr == nil {
		_ = a.store.SetKV(kvLedgerCheckLast, string(blob))
	}
	switch {
	case rep.Error != "":
		// 已经打过日志，这里不重复。
	case len(findings) == 0:
		log.Printf("INFO: [ledger] 账目对账通过（%s，%dms）", reason, rep.DurationMS)
	default:
		for _, f := range findings {
			log.Printf("WARN: [ledger] %s 发现 %d 处（%s）：%s", f.Code, f.Count, f.Severity, f.Detail)
		}
	}
	return rep
}

// lastLedgerCheck 读最近一次结果（没跑过返回空报告）。
func (a *app) lastLedgerCheck() (LedgerCheckReport, bool) {
	var rep LedgerCheckReport
	v, _ := a.store.GetKV(kvLedgerCheckLast)
	if v == "" {
		return rep, false
	}
	if err := json.Unmarshal([]byte(v), &rep); err != nil {
		return rep, false
	}
	return rep, true
}

// ledgerCheckLoop 启动后延迟一次，之后每 6 小时一次。
func (a *app) ledgerCheckLoop(ctx context.Context) {
	timer := time.NewTimer(ledgerCheckStartDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	a.runLedgerCheck("startup")
	tick := time.NewTicker(ledgerCheckEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			a.runLedgerCheck("periodic")
		}
	}
}

// handleAdminLedgerCheck 后台查询对账结果。
//
//	GET /api/admin/ledger-check          → 最近一次（没跑过就当场跑一次）
//	GET /api/admin/ledger-check?run=1    → 立即重跑（页面上的"立即对账"按钮）
//
// 为什么把"立即重跑"做成查询参数而不是 POST：它**不改变任何业务数据**
// （全 SELECT），没有副作用就不该要求 CSRF 令牌，否则前端还得为它开一条写路径。
func (a *app) handleAdminLedgerCheck(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("run") == "1"
	var rep LedgerCheckReport
	if force {
		rep = a.runLedgerCheck("manual")
	} else if last, ok := a.lastLedgerCheck(); ok {
		rep = last
	} else {
		rep = a.runLedgerCheck("first-request")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            rep.OK(),
		"at":            rep.At,
		"reason":        rep.Reason,
		"duration_ms":   rep.DurationMS,
		"findings":      rep.Findings,
		"error":         rep.Error,
		"next_check_in": strconv.FormatInt(int64(ledgerCheckEvery/time.Second), 10) + "s",
	})
}
