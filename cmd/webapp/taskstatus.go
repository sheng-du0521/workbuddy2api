// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// taskstatus.go 账号卡片「任务完成情况」查询：今日签到 + 猫猫旅行状态。
//
// 为什么要单独做：自动任务此前只有「开关」（task_cfg）与任务级台账
// （internal/taskledger，按任务类计数），卡片上完全看不出「这个号今天签到没有、
// 猫出门了没有」。结果是管理员只能等 9 点 / 21 点那轮跑完去翻日志。
// 参考实现（WorkBuddy-Switch2api 的 account-card）是在卡片上直接打「已签到 /
// 未签到」徽标 + 旅行状态徽标，本文件按同一口径实现。
//
// 只读：本文件所有上游调用都不产生副作用（不签到、不派出、不领奖），
// 因此可以被 UI 频繁调用；真正的动作仍由调度器的定时任务执行。
package main

import (
	"errors"
	"net/http"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// taskStatusTTL 账号任务状态的缓存时长。
//
// 缓存的理由：卡片每次操作（改分组 / 切开关 / 重同步 / 手动刷新）都会重渲染，
// 而每个账号要打 2~3 个上游接口。签到/旅行的状态变化以小时计，30 秒的陈旧度
// 完全够用，却能挡掉绝大部分重复出站（也顺手降低被上游风控的概率）。
const taskStatusTTL = 30 * time.Second

// taskStatusCheckin 签到状态视图。
type taskStatusCheckin struct {
	// Supported 该档位是否支持签到（国际版不支持，UI 据此隐藏而非显示「未签到」）。
	Supported bool `json:"supported"`
	// CheckedIn 今日是否已签到。
	CheckedIn bool `json:"checked_in"`
	// OK 本次查询是否成功；false 时看 Error。
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// taskStatusTravel 猫猫旅行状态视图。
//
// State 取值：no-buddy（未领养，含门槛未达标）/ idle（可派出）/
// traveling（在途）/ arrived（到站待领奖）。
type taskStatusTravel struct {
	Supported         bool   `json:"supported"`
	OK                bool   `json:"ok"`
	State             string `json:"state,omitempty"`
	DailyLimitReached bool   `json:"daily_limit_reached"`
	RewardCredit      int64  `json:"reward_credit,omitempty"`
	Error             string `json:"error,omitempty"`
}

// taskStatusView 单账号任务完成情况。
type taskStatusView struct {
	UID       string            `json:"uid"`
	UpdatedAt int64             `json:"updated_at"`
	Checkin   taskStatusCheckin `json:"checkin"`
	Travel    taskStatusTravel  `json:"travel"`
	// Cached 本次是否为缓存命中（便于排查「为什么还是旧状态」）。
	Cached bool `json:"cached"`
}

// taskStatusCache 任务状态的短 TTL 缓存（per 账号）。
//
// 用互斥锁而非 sync.Map：条目数 = 账号数（几十量级），读写都是整条替换，
// 手动锁更直白；查询发生在锁外，不会把上游耗时带进临界区。
type taskStatusCache struct {
	mu      sync.Mutex
	entries map[string]taskStatusEntry
}

type taskStatusEntry struct {
	view taskStatusView
	at   time.Time
}

// get 命中未过期条目时返回副本。
func (c *taskStatusCache) get(uid string, now time.Time) (taskStatusView, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[uid]
	if !ok || now.Sub(e.at) >= taskStatusTTL {
		return taskStatusView{}, false
	}
	return e.view, true
}

// put 写入/覆盖条目。
func (c *taskStatusCache) put(uid string, v taskStatusView, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]taskStatusEntry{}
	}
	c.entries[uid] = taskStatusEntry{view: v, at: now}
}

// taskStatus 取（带缓存的）账号任务状态。
func (a *app) taskStatus(acc *Account) taskStatusView {
	now := time.Now()
	a.initCaches()
	if v, ok := a.taskCache.get(acc.UID, now); ok {
		v.Cached = true
		return v
	}
	v := a.queryTaskStatus(acc)
	v.UID = acc.UID
	v.UpdatedAt = now.Unix()
	a.taskCache.put(acc.UID, v, now)
	return v
}

// queryTaskStatus 真打上游取一次状态。任一子项失败只影响该子项（另一项照常展示）。
func (a *app) queryTaskStatus(acc *Account) taskStatusView {
	var v taskStatusView
	if a.proxy == nil || a.proxy.uc == nil {
		v.Checkin.Error = "服务未就绪"
		v.Travel.Error = "服务未就绪"
		return v
	}
	// 优先用池内凭证对象：401 时刷新出的新 token 会被池与调度器共享；
	// 池里没有（尚未同步）时回落到按库内凭证临时构造。
	au := a.proxy.pool.AuthByUID(acc.UID)
	if au == nil {
		au = authFromAccount(acc)
	}
	if au == nil {
		v.Checkin.Error = "账号凭证不可用"
		v.Travel.Error = "账号凭证不可用"
		return v
	}

	if st, err := a.proxy.uc.CheckinStatus(au); err != nil {
		v.Checkin.Error = taskStatusErrText(err)
	} else {
		v.Checkin.Supported = st.Supported
		v.Checkin.CheckedIn = st.CheckedIn
		v.Checkin.OK = true
	}

	v.Travel = a.queryTravelStatus(au)
	return v
}

// queryTravelStatus 先查有无猫再查旅行状态（与调度器 travelOne 的调用序一致）。
//
// 为什么要先查 BuddyInfo：无猫账号的 travel/status 仍会返回 idle，
// 直接展示成「未旅行」会误导（用户会以为派一次就行，实际得先领养）。
func (a *app) queryTravelStatus(au *auth.Auth) taskStatusTravel {
	var t taskStatusTravel
	// 国际版无猫猫旅行体系（与调度器 D4 门控同口径）：不发无谓请求。
	if au.IsGlobal() {
		// OK=true 是**语义必须**：前端用「先判 ok、再判 supported」的顺序渲染，
		// 国际版是"查过了，结论是不支持"，而不是"没查成"。
		// 之前这里留零值（OK=false），一旦前端把 ok 提前就会误报「查询失败」。
		t.OK = true
		return t
	}
	t.Supported = true
	buddy, err := a.proxy.uc.BuddyInfo(au)
	switch {
	case err != nil && upstream.IsBuddyTaskIncomplete(err):
		// 领养门槛未达标：等同「无猫」，不是错误。
		t.OK = true
		t.State = "no-buddy"
		return t
	case err != nil:
		t.Error = taskStatusErrText(err)
		return t
	case buddy == nil:
		t.OK = true
		t.State = "no-buddy"
		return t
	}
	ts, err := a.proxy.uc.TravelStatus(au)
	if err != nil {
		t.Error = taskStatusErrText(err)
		return t
	}
	t.OK = true
	t.State = ts.State
	t.DailyLimitReached = ts.DailyLimitReached
	t.RewardCredit = ts.RewardCredit
	return t
}

// ---------------------------------------------------------------------------
// HTTP 处理器
// ---------------------------------------------------------------------------

// handleAccountTaskStatus 管理侧：任意账号的任务完成情况。
func (a *app) handleAccountTaskStatus(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	acc, err := a.store.GetAccount(uid)
	if err != nil || acc == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "账号不存在"})
		return
	}
	writeJSON(w, http.StatusOK, a.taskStatus(acc))
}

// handleHostAccountTaskStatus 用户侧：仅限本人代挂账号。
func (a *app) handleHostAccountTaskStatus(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	acc := a.requireOwnedAccount(w, r.PathValue("uid"), u.ID)
	if acc == nil {
		return
	}
	writeJSON(w, http.StatusOK, a.taskStatus(acc))
}

// taskStatusErrText 把上游错误翻译成"下一步该做什么"（v0.31.28）。
//
// 为什么需要（2026-10-18 现场）：账号的 access/refresh token 被上游作废后，
// 签到状态与猫猫查询都返回 **HTTP 401 + openresty 的 HTML 错误页**。原样透出去
// 有两个问题：① 前端把 `supported=false`（零值）先判，于是显示成「签到：不支持」——
// 用户以为功能不支持，实际是凭证失效；② 错误正文是一坨 HTML，看不出该怎么修。
func taskStatusErrText(err error) string {
	var ue *upstream.Error
	if errors.As(err, &ue) {
		if ue.Kind == upstream.ErrSessionDead || ue.Status == http.StatusUnauthorized {
			return "账号登录已失效（access/refresh token 已被上游作废），需要重新导入该账号的 auth 文件"
		}
	}
	return err.Error()
}
