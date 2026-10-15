// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// compat.go 旧网关（cmd/server，7863）对外面的**兼容层**（v0.31.4）。
//
// ## 为什么要有这一层
//
// 两个产品的合并计划是「webapp 吸收旧网关，然后删掉第二套实现」。但旧网关的对外面
// 被一整条 CLI / 脚本链依赖，而且是被**逐字解析**的：
//
//	cmd/acct          GET /status（账号表）· POST /admin/accounts/{uid}/{disable,enable,revive}
//	                  · POST /admin/tasks/{name}/run
//	checkin.sh        GET /status（判网关活不活，jq 读字段）
//	dev.sh / login.sh GET /status（读 accounts 数组长度）
//
// 所以这里**照旧网关的原样**实现（路径、键名、状态码、语义），而不是改成 webapp 的
// /api/admin/* 风格 —— 兼容层的价值就在于「老命令、老脚本一行不改也能用」；
// 换个形状等于把迁移成本原样转嫁给用户，那还不如不合并。
//
// ## 与 webapp 原生接口的关系
//
// 这些都是**薄封装**，状态效果一律复用既有实现（`store.SetDisabled` /
// `pool.SetManualDisabled` / `pool.ReviveDisabled` / `healthLast.dropUIDs` /
// `proxy.SyncAccounts`），绝不另写一份「停用/启用」逻辑 —— 那正是本仓最贵的一类
// 缺陷（同一件事两套实现，久了必然漂移）。唯一刻意保留的差异在 enable/revive 的
// **分离语义**上，见 handleCompatAccountEnable 的注释。
package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
)

// compatTaskNames 旧网关可手动触发的任务名（白名单顺序即 404 文案里的列举顺序）。
// 与 webapp 的 runnableKinds 同一集合 —— 两份清单必须一致：旧网关的 CLI
// （`acct task checkin`）按这五个名字调用，缺一个就是一条老命令失效。
var compatTaskNames = []string{"checkin", "activity", "keepalive", "travel", "cat"}

// compatGuard 兼容层鉴权：旧网关只有一个共享密钥（config.api_key），
// 这里等价映射到 webapp 的管理密钥 —— `X-Admin-Key` 或 `Authorization: Bearer`
// 都可以（旧脚本两种都出现过：curl -H "Authorization: Bearer $KEY"）。
// 管理员会话同样放行（人用浏览器打开 /status 也能看）。
func (a *app) compatGuard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.authorized(r) || a.isAdminSession(r) {
			next(w, r)
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": "unauthorized: 需要管理密钥（X-Admin-Key 或 Authorization: Bearer）",
		})
	}
}

// handleCompatStatus GET /status —— 旧网关的账号表 + 池计数。
//
// accounts 用的是 pool.Status 本身：**同一个 internal/pool 包**，键名与旧网关逐字一致
// （uid / realm / nickname / credits / disabled / disabled_reason / manual_disabled /
// manual_reason / cooling …）。这正是「两个产品各写一份」已经收敛过的部分 —— 池是共享的，
// 所以兼容面天然同源；这里只需要把汇总键补齐。
func (a *app) handleCompatStatus(w http.ResponseWriter, r *http.Request) {
	accounts := a.proxy.pool.List()
	total, healthy, cooling, disabled, inFlightFull := a.proxy.pool.CountsDetailed()
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":       accounts,
		"total":          total,
		"healthy":        healthy,
		"cooling":        cooling,
		"disabled":       disabled,
		"in_flight_full": inFlightFull,
		// version 是新增键（旧网关没有）——只增不改，老解析器忽略未知键。
		"version": version,
	})
}

// compatReasonFromBody 读可选 JSON 体里的 reason 字段（空体/非 JSON/无该字段 → 空串）。
// 与旧网关同口径：无体是最常见的调用形态，不能因为体格式拒绝请求。
func compatReasonFromBody(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<10))
	if err != nil || len(raw) == 0 {
		return ""
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return ""
	}
	return strings.TrimSpace(body.Reason)
}

// compatAccountState 兼容层的账号操作响应（与旧网关 adminState 同键名同语义）。
//
// 显式写全字段与 json tag：这是**对外下发**的视图，且 CLI 逐字解析
// （v0.29.0 的「提现审核按钮点不动」就是内嵌无 tag 结构体导致键名变成 Go 字段名）。
type compatAccountState struct {
	UID            string `json:"uid"`
	ManualDisabled bool   `json:"manual_disabled"`
	ManualReason   string `json:"manual_reason,omitempty"`
	// Disabled 让调用方能区分「手动摘除」与「系统判定坏了」——
	// enable 解手动位，revive 解自动位，两者的差别就在这里被看见。
	Disabled bool `json:"disabled"`
	Changed  bool `json:"changed"`
}

// handleCompatAccountDisable POST /admin/accounts/{uid}/disable —— 手动停用。
//
// 语义：把账号摘出**对话流量**，但保留在池里（状态/冷却/成本台账继续归它管，
// 签到与保活照常）。不碰冷却与熔断维度。
//
// 状态效果与 webapp 后台的「停用」按钮**完全同源**（三处一致）：
// 库列 accounts.disabled → 池内手动位 → 异常分组归档 → 检测快照失效 → 选号重算。
func (a *app) handleCompatAccountDisable(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimSpace(r.PathValue("uid"))
	if uid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "uid is required"})
		return
	}
	reason := compatReasonFromBody(r)
	if reason == "" {
		reason = "manual"
	}
	if _, ok := a.proxy.pool.Status(uid); !ok {
		// 与旧网关一致：不在池内 → 404（而不是静默 200）。
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "account not found: " + uid})
		return
	}
	was, _ := a.proxy.pool.Status(uid)
	_ = a.store.SetDisabled(uid, true)
	a.proxy.archiveAbnormal(uid, "运营停用", true)
	a.compatAfterAccountChange(uid)
	// ⚠️ 顺序要紧：`store.SetDisabled` 写的是库列，而 SyncAccounts 会把库列**同步进**
	// 池内手动位、且原因固定写成 "web ui"（见 proxy.SyncAccountsReport）。所以运维给的
	// 原因必须在同步**之后**再写一次，否则 /status 里读到的是 "web ui" ——
	// 「运维写了原因但面板显示别人的文案」正是这类"写完被覆盖"缺陷的典型现场。
	a.proxy.pool.SetManualDisabled(uid, true, reason)
	a.writeCompatAccountState(w, uid, a.compatManualChanged(uid, was.ManualDisabled))
}

// compatChanged 判定本次操作是否**真的**改变了目标位。
//
// 为什么不用 Pool 的返回值：库列同步（SyncAccountsReport）会先把库里的位写进池，
// 于是"我刚设成 true"这一步在池侧看到的往往是"本来就是 true"，返回 changed=false ——
// 而调用方（旧网关 CLI）问的是"我这次操作有没有效果"。这里改成"操作前后对比"，
// 语义才是诚实的。
func (a *app) compatManualChanged(uid string, was bool) bool {
	now, _ := a.proxy.pool.Status(uid)
	return now.ManualDisabled != was
}

// compatAutoChanged 同上，针对**系统自动禁用位**（revive 的目标位）。
func (a *app) compatAutoChanged(uid string, was bool) bool {
	now, _ := a.proxy.pool.Status(uid)
	return now.Disabled != was
}

// handleCompatAccountEnable POST /admin/accounts/{uid}/enable —— 解除**手动**停用。
//
// ⚠️ 刻意与 webapp 后台的「启用」按钮**不同**：这里只清手动位，不清系统自动禁用位。
// 为什么保留这个差异：旧网关把 enable / revive 做成两个命令，正是因为池里有
// **两个互相独立的停用位**，「运维主动摘的」与「系统判定坏了的」恢复操作不同。
// CLI 的语义契约是分开的，合并成一个动作会让 `acct enable` 在旧用户手里产生
// 与从前不同的效果（悄悄放回一个被系统判定坏掉的号）——那正是本仓明令禁止的
// 「比 NoteSuccess 更弱的证据却清更多位」。响应里的 disabled 字段会告诉调用方
// 「还被系统禁用着，需要 revive」，行为可诊断。
func (a *app) handleCompatAccountEnable(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimSpace(r.PathValue("uid"))
	if uid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "uid is required"})
		return
	}
	if _, ok := a.proxy.pool.Status(uid); !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "account not found: " + uid})
		return
	}
	was, _ := a.proxy.pool.Status(uid)
	_ = a.store.SetDisabled(uid, false) // 库列 = 手动位的持久化来源（见下）
	a.compatAfterAccountChange(uid)
	a.proxy.pool.SetManualDisabled(uid, false, "")
	a.writeCompatAccountState(w, uid, a.compatManualChanged(uid, was.ManualDisabled))
}

// handleCompatAccountRevive POST /admin/accounts/{uid}/revive —— 解除**系统自动**禁用。
//
// 清 disabled + reason + 连续 12153 计数（revive 语义），**不动手动停用位** ——
// 运维明确摘除的号不该被一次 revive 悄悄放回选号池。
func (a *app) handleCompatAccountRevive(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimSpace(r.PathValue("uid"))
	if uid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "uid is required"})
		return
	}
	if _, ok := a.proxy.pool.Status(uid); !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "account not found: " + uid})
		return
	}
	was, _ := a.proxy.pool.Status(uid)
	// ⚠️ **不碰** `accounts.disabled` 库列：那是**手动位**的持久化来源，
	// 清它等于"偷偷把运维摘掉的号放回池子"（且会在下次同步才发作，极难查）。
	// 系统自动禁用位只存在于池内（disabled + reason + 连续计数），revive 清的就是它。
	a.proxy.pool.ReviveDisabled(uid)
	a.compatAfterAccountChange(uid)
	a.writeCompatAccountState(w, uid, a.compatAutoChanged(uid, was.Disabled))
}

// compatAfterAccountChange 账号状态被改动后的两件收尾（与 webapp 后台按钮同源）：
//
//  1. 该账号的旧健康检测结论立即失效 —— 检测结果是**时点观测**，不是账号属性；
//     留着会持续显示旧结论、诱导运维反复点击（v0.23.4 的「点了启用没效果」现场）；
//  2. 重算选号（分组/停用状态直接影响选号范围）。
//
// 池内**两个停用位**因此各清各的，见 MEMORY 的「两个停用位」条目。
func (a *app) compatAfterAccountChange(uid string) {
	healthLast.dropUIDs([]string{uid})
	_ = a.proxy.SyncAccounts()
}

// writeCompatAccountState 回显操作后的双位状态（面板/脚本据此直接更新，不必再打 /status）。
func (a *app) writeCompatAccountState(w http.ResponseWriter, uid string, changed bool) {
	st, _ := a.proxy.pool.Status(uid)
	writeJSON(w, http.StatusOK, compatAccountState{
		UID:            uid,
		ManualDisabled: st.ManualDisabled,
		ManualReason:   st.ManualReason,
		Disabled:       st.Disabled,
		Changed:        changed,
	})
}

// handleCompatTaskRun POST /admin/tasks/{name}/run —— 手动触发一轮排程任务。
//
// 语义对齐旧网关：**受理即返回 202**（webapp 的 RunNow 本就是异步语义：立即返回、
// 后台推进、结果进执行台账），响应体恒为 {"task":..., "status":"started"}。
// 差别只在状态码词表：未知任务 404（附白名单）、撞车 409、未接线（-no-tasks）503。
func (a *app) handleCompatTaskRun(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if _, ok := runnableKinds[name]; !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "unknown task: " + name + "; known: " + strings.Join(compatTaskNames, ", "),
		})
		return
	}
	if a.tasks == nil {
		// 与旧网关「配置开了但没接线」同口径：回 503 而不是静默 404 ——
		// 静默 404 会让运维以为路由写错了，白查一轮。
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "task runner not wired"})
		return
	}
	if _, err := a.tasks.RunNow(name); err != nil {
		if errors.Is(err, ErrTaskBusy) {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "task already running: " + name})
			return
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"task": name, "status": "started"})
}

// handleCompatStatsReset POST /v1/stats/reset —— 清空**进程内**累计，便于观察增量。
//
// 与旧网关的差别（刻意的）：webapp 的报表数据源是 `usage_logs`（持久化审计数据），
// 不能被一次调用清空 —— 那会破坏账目与"可回溯"。能清的是实时面板的进程内累计
// （分钟桶 + 事件流 + 三个请求计数），语义仍是「便于观察增量」。
// 在途计数**不清**：它是在飞请求的真实数量，清掉只会撒谎。
func (a *app) handleCompatStatsReset(w http.ResponseWriter, r *http.Request) {
	a.proxy.live.reset()
	log.Printf("INFO: [compat] /v1/stats/reset：已清空进程内实时统计（分钟桶 + 事件流 + 请求计数；报表数据不受影响）")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":    true,
		"scope": "in-process live counters (usage_logs / 报表数据不受影响)",
	})
}
