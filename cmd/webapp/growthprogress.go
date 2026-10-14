// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// growthprogress.go 成长任务「完成进度」查询（v0.17.1）。
//
// 为什么需要它：成长任务 v0.17.0 起改为管理员选号手动执行，但此前页面上只看得到
// 「上次执行时间」——看不出这个号到底做完了几项、差哪几项。选号执行一旦缺了进度，
// 「该跑谁」就只能凭记忆猜。本文件把上游的任务列表翻译成一眼能读的进度。
//
// 只读：本文件所有上游调用都不产生副作用（不 accept、不上报、不领奖），因此可以
// 被 UI 频繁调用；真正的执行仍走 scheduler.RunGrowthFor。
//
// 与 taskstatus.go 的关系：同一套「短 TTL 缓存 + 池内凭证优先」的口径，
// 只是查的东西不同（那里是签到/旅行，这里是 growth 任务列表）。
package main

import (
	"context"
	"encoding/json"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
)

// growthProgressTTL 进度缓存时长。
//
// 为什么比 taskStatusTTL（30s）长得多：成长任务的状态只在**执行后**才变化，
// 而执行是人手动触发的低频动作（十几分钟到几小时一次）。缓存 5 分钟既能挡掉
// 「来回切页」的重复出站，又不会让人在执行完看不到新进度——执行完前端会主动
// 刷新一次（见 handleAdminGrowthProgress 的 force 语义），不依赖缓存过期。
const growthProgressTTL = 5 * time.Minute

// growthProgressMaxBatch 单次批量查询的账号上限。
//
// 存在的意义是防封：进度查询虽然是只读 GET，但「一次请求打 18 个号」与
// 「一次请求打 200 个号」在出口特征上是两回事。超过上限的部分直接截断并告知，
// 让调用方分批（前端本来就按可见账号分批）。
const growthProgressMaxBatch = 20

// growthProgressGapMin/MaxMillis 批量查询时账号之间的等待区间（毫秒）。
//
// 只读查询可以比执行（30~120s）快得多，但**不能没有**：N 个账号在 100ms 内
// 连打上游，是最容易被统计出的关联特征。给一个亚秒级的随机间隔即可摊开。
const (
	growthProgressGapMinMillis = 250
	growthProgressGapMaxMillis = 700
)

// growthTaskMeta 单个成长任务的展示元数据。
//
// Name 的措辞对齐 scripts/task_runner.py 的 MAPPING 注释（那里是唯一的实现事实源），
// 便于日志/代码/UI 三处对得上。
type growthTaskMeta struct {
	Name string `json:"name"`
	// Phase 执行阶段：zero（不消耗额度）/ talk（需真实对话）/ night（夜猫窗口）/
	// manual（不可自动，需人工完成）。
	Phase string `json:"phase"`
	// Auto 是否属于「本功能可自动完成」的范围（= growth_run.py 的 PHASES["all"]）。
	// 进度百分比只按 Auto 的那些算：把「需真实捐款」「夜间窗口」这类算进去，
	// 会让一个已经做满自动化部分的号永远显示「未完成」。
	Auto bool `json:"auto"`
}

// growthTaskMetas 成长任务码 → 展示元数据。
//
// 覆盖上游实测下发的全部 code（含不属于自动范围的 two 个），因为进度页的价值
// 恰恰在于「把差的那几项列出来」——只列可自动的会漏掉关键信息。
var growthTaskMetas = map[string]growthTaskMeta{
	// ---- zero：行为上报，不消耗额度（新号先跑这些攒积分）----
	"create_canvas":   {Name: "设计画布", Phase: "zero", Auto: true},
	"template_5":      {Name: "模板使用 ×5", Phase: "zero", Auto: true},
	"automation_1":    {Name: "定时任务创建", Phase: "zero", Auto: true},
	"playbook_prompt": {Name: "灵感案例「做同款」", Phase: "zero", Auto: true},
	"Buddy_App":       {Name: "Buddy 应用使用", Phase: "zero", Auto: true},
	"Buddy_App_QQ":    {Name: "企鹅教师助手", Phase: "zero", Auto: true},
	"Hp_Appearance":   {Name: "主题皮肤生效", Phase: "zero", Auto: true},
	"chat_5":          {Name: "活跃对话上报 ×5", Phase: "zero", Auto: true},
	"first_buddy":     {Name: "领养第一只 Buddy", Phase: "zero", Auto: true},
	"RichMeow_Chat":   {Name: "桌面端对话链", Phase: "zero", Auto: true},
	"Library_read":    {Name: "资料库阅读", Phase: "zero", Auto: true},
	// ---- talk：需真实对话，消耗额度（用 zero 攒到的积分跑）----
	"expert_5":          {Name: "专家召唤 ×5", Phase: "talk", Auto: true},
	"Expert_team_use_3": {Name: "专家团 ×3", Phase: "talk", Auto: true},
	"skill_1":           {Name: "技能加载", Phase: "talk", Auto: true},
	"Expert_lighthouse": {Name: "轻量云专家", Phase: "talk", Auto: true},
	"Model_chat_GLM5.2": {Name: "GLM-5.2 真实对话", Phase: "talk", Auto: true},
	// ---- 不属于自动范围（进度页照常展示，但不计入完成度）----
	"black_cat":                   {Name: "夜猫子对话 ×3", Phase: "night", Auto: false},
	"wb_wechat_oa_subscribe_task": {Name: "关注公众号", Phase: "manual", Auto: false},
	"Expert_Philanthropy":         {Name: "公益捐赠", Phase: "manual", Auto: false},
}

// growthPhaseOrder 阶段展示顺序（未完成的自动项排最前，见 sortGrowthItems）。
var growthPhaseOrder = []string{"zero", "talk", "night", "manual"}

// growthTaskItem 单个任务的进度条目。
type growthTaskItem struct {
	Code    string `json:"code"`
	Name    string `json:"name"`
	Phase   string `json:"phase"`
	Status  string `json:"status"` // 上游 accept_status 原值
	Current int    `json:"current"`
	Target  int    `json:"target"`
	// Done 已领取（claimed）—— 计分完成的唯一判据。
	Done bool `json:"done"`
	// Ready 已达标但未领（completed 或 current >= target，且非 claimed）。
	Ready bool `json:"ready"`
	// Auto 是否属于本功能可自动完成的范围。
	Auto bool `json:"auto"`
}

// growthProgressView 单账号的成长任务完成进度。
type growthProgressView struct {
	UID       string `json:"uid"`
	UpdatedAt int64  `json:"updated_at"`
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
	// Total/下发的任务总数；Done 已领取数（全部任务口径）。
	Total int `json:"total"`
	Done  int `json:"done"`
	// AutoTotal/AutoDone 只统计「本功能可自动完成」的 16 项 —— 进度百分比按它算。
	AutoTotal int `json:"auto_total"`
	AutoDone  int `json:"auto_done"`
	// Ready 可自动项里「已达标未领」的数量（执行一轮 claim 即可入账）。
	Ready  int              `json:"ready"`
	Items  []growthTaskItem `json:"items"`
	Cached bool             `json:"cached"`
}

// growthProgressCache 进度的短 TTL 缓存（per 账号）。与 taskStatusCache 同构。
type growthProgressCache struct {
	mu      sync.Mutex
	entries map[string]growthProgressEntry
}

type growthProgressEntry struct {
	view growthProgressView
	at   time.Time
}

func (c *growthProgressCache) get(uid string, now time.Time) (growthProgressView, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[uid]
	if !ok || now.Sub(e.at) >= growthProgressTTL {
		return growthProgressView{}, false
	}
	return e.view, true
}

func (c *growthProgressCache) put(uid string, v growthProgressView, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]growthProgressEntry{}
	}
	c.entries[uid] = growthProgressEntry{view: v, at: now}
}

// growthProgress 取（带缓存的）进度；force=true 时忽略缓存重查。
func (a *app) growthProgress(acc *Account, force bool) growthProgressView {
	now := time.Now()
	a.initCaches()
	if !force {
		if v, ok := a.growthCache.get(acc.UID, now); ok {
			v.Cached = true
			return v
		}
	}
	v := a.queryGrowthProgress(acc)
	v.UID = acc.UID
	v.UpdatedAt = now.Unix()
	a.growthCache.put(acc.UID, v, now)
	return v
}

// queryGrowthProgress 真打上游取一次任务列表并折叠成进度。
func (a *app) queryGrowthProgress(acc *Account) growthProgressView {
	var v growthProgressView
	if a.proxy == nil || a.proxy.uc == nil {
		v.Error = "服务未就绪"
		return v
	}
	// 池内凭证优先：401 刷新出的新 token 会被池与调度器共享；池里没有（尚未同步）
	// 时回落到按库内凭证临时构造，与 taskstatus.go 同一口径。
	au := a.proxy.pool.AuthByUID(acc.UID)
	if au == nil {
		au = authFromAccount(acc)
	}
	if au == nil {
		v.Error = "账号凭证不可用"
		return v
	}
	return a.growthProgressFor(au)
}

// growthProgressFor 由一个已构造好的凭证查进度（用户侧与管理侧共用）。
func (a *app) growthProgressFor(au *auth.Auth) growthProgressView {
	var v growthProgressView
	tasks, err := a.proxy.uc.GrowthTasks(au)
	if err != nil {
		v.Error = err.Error()
		return v
	}
	v.Items = make([]growthTaskItem, 0, len(tasks))
	for _, t := range tasks {
		it := growthTaskItem{
			Code:    t.TaskCode,
			Status:  t.AcceptStatus,
			Current: t.Progress.Current,
			Target:  t.Progress.Target,
			Done:    t.AcceptStatus == "claimed",
		}
		// 已达标但未领：上游用 completed 表达，也有 current>=target 但 status
		// 尚未推进的形态（实测），两者都算「可领」。
		if !it.Done && (t.AcceptStatus == "completed" ||
			(t.Progress.Target > 0 && t.Progress.Current >= t.Progress.Target)) {
			it.Ready = true
		}
		if meta, ok := growthTaskMetas[t.TaskCode]; ok {
			it.Name = meta.Name
			it.Phase = meta.Phase
			it.Auto = meta.Auto
		} else {
			// 上游新增了任务：照常展示（用 code 当名字），但不计入自动完成度。
			it.Name = t.TaskCode
			it.Phase = "other"
		}
		if it.Auto {
			v.AutoTotal++
			if it.Done {
				v.AutoDone++
			}
			if it.Ready {
				v.Ready++
			}
		}
		if it.Done {
			v.Done++
		}
		v.Items = append(v.Items, it)
	}
	v.Total = len(v.Items)
	if v.AutoTotal == 0 {
		v.Error = "该账号未下发成长任务（可能是国际版或活动未开放）"
		return v
	}
	v.OK = true
	return v
}

// sortGrowthItems 让任务按「未完成优先 → 可领优先 → 阶段顺序」排列。
//
// 为什么未完成优先排在阶段之前：进度页的第一诉求是「这个号还差什么」。
// 若按阶段分组，一个 zero 组已全做完的号，明细开头会是一整屏「已领」，
// 真正要看的 talk 缺口被压到下面 —— 那就等于让人自己找。
// 未完成组内部再按阶段排（zero → talk → night → manual），与执行链路的顺序一致。
func sortGrowthItems(items []growthTaskItem) {
	rank := func(phase string) int {
		for i, p := range growthPhaseOrder {
			if p == phase {
				return i
			}
		}
		return len(growthPhaseOrder)
	}
	less := func(a, b growthTaskItem) bool {
		if a.Done != b.Done {
			return !a.Done // 未完成的在前
		}
		if a.Ready != b.Ready {
			return a.Ready // 都已达标未领的排更前（跑一轮就能入账）
		}
		return rank(a.Phase) < rank(b.Phase)
	}
	// 插入排序（条目数 = 任务数，十几条，无需引入 sort 依赖）。
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && less(items[j], items[j-1]); j-- {
			items[j-1], items[j] = items[j], items[j-1]
		}
	}
}

// ---------------------------------------------------------------------------
// HTTP 处理器：管理侧
// ---------------------------------------------------------------------------

// handleAdminGrowthOverview 管理后台「成长任务」页的账号清单。
//
// 刻意**不主动打上游**：一页 18 个号，若打开就打 18 个请求，光是切页就会持续
// 制造出站。这里只回账号本身的信息 + 缓存中已有的进度（没有就是 null），
// 真正的拉取由前端显式点「查询进度」触发（POST .../progress）。
func (a *app) handleAdminGrowthOverview(w http.ResponseWriter, r *http.Request) {
	list, err := a.store.ListAccounts()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	now := time.Now()
	if a.growthCache == nil {
		a.growthCache = &growthProgressCache{}
	}
	rows := make([]map[string]any, 0, len(list))
	for _, acc := range list {
		row := map[string]any{
			"uid":            acc.UID,
			"nickname":       acc.Nickname,
			"realm":          acc.Realm,
			"disabled":       acc.Disabled,
			"growth_done_at": acc.GrowthDoneAt,
			"has_token":      acc.AccessToken != "",
		}
		if acc.OwnerUserID > 0 {
			row["owner_id"] = acc.OwnerUserID
		}
		if v, ok := a.growthCache.get(acc.UID, now); ok {
			v.Cached = true
			row["progress"] = v
		}
		rows = append(rows, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": rows,
		"summary": map[string]any{
			"total": len(rows),
		},
	})
}

// handleAdminGrowthProgress 管理侧批量查询进度（body: {"uids":[...], "force":bool}）。
func (a *app) handleAdminGrowthProgress(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeGrowthProgressReq(w, r)
	if !ok {
		return
	}
	accs, err := a.store.AccountsByUIDs(req.UIDs)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, a.collectGrowthProgress(r, accs, req.Force))
}

// ---------------------------------------------------------------------------
// HTTP 处理器：用户侧（仅限本人代挂账号）
// ---------------------------------------------------------------------------

// handleHostGrowthOverview 用户控制台「成长任务」页：只看自己的代挂账号。
func (a *app) handleHostGrowthOverview(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	list, err := a.store.ListAccountsByOwner(u.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	now := time.Now()
	if a.growthCache == nil {
		a.growthCache = &growthProgressCache{}
	}
	rows := make([]map[string]any, 0, len(list))
	for _, acc := range list {
		row := map[string]any{
			"uid":            acc.UID,
			"nickname":       acc.Nickname,
			"realm":          acc.Realm,
			"disabled":       acc.Disabled,
			"growth_done_at": acc.GrowthDoneAt,
			"has_token":      acc.AccessToken != "",
			"pool_state":     acc.PoolState,
		}
		if v, ok := a.growthCache.get(acc.UID, now); ok {
			v.Cached = true
			row["progress"] = v
		}
		rows = append(rows, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": rows, "total": len(rows)})
}

// handleHostGrowthProgress 用户侧批量查询进度；越权 uid 直接剔除（不报错，
// 避免把「某个 uid 存不存在」变成可探测的信息）。
func (a *app) handleHostGrowthProgress(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeGrowthProgressReq(w, r)
	if !ok {
		return
	}
	u := userFrom(r)
	mine, err := a.store.ListAccountsByOwner(u.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	want := map[string]bool{}
	for _, uid := range req.UIDs {
		want[uid] = true
	}
	accs := make([]*Account, 0, len(req.UIDs))
	for _, acc := range mine {
		if want[acc.UID] {
			accs = append(accs, acc)
		}
	}
	if len(accs) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"views": []any{}, "count": 0})
		return
	}
	writeJSON(w, http.StatusOK, a.collectGrowthProgress(r, accs, req.Force))
}

// ---------------------------------------------------------------------------
// 共用
// ---------------------------------------------------------------------------

// growthProgressReq 批量查询请求体。
type growthProgressReq struct {
	UIDs []string `json:"uids"`
	// Force 忽略缓存重查（执行完一轮后前端会带它，确保看到新进度）。
	Force bool `json:"force"`
	// Truncated 因超出单次上限而被丢弃的账号数（>0 时前端应提示分批）。
	Truncated int `json:"-"`
}

// decodeGrowthProgressReq 解析并校验请求体。
func decodeGrowthProgressReq(w http.ResponseWriter, r *http.Request) (growthProgressReq, bool) {
	var req growthProgressReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return req, false
	}
	seen := map[string]bool{}
	uids := make([]string, 0, len(req.UIDs))
	for _, u := range req.UIDs {
		u = strings.TrimSpace(u)
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		uids = append(uids, u)
	}
	if len(uids) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请先选择账号"})
		return req, false
	}
	truncated := 0
	if len(uids) > growthProgressMaxBatch {
		truncated = len(uids) - growthProgressMaxBatch
		uids = uids[:growthProgressMaxBatch]
	}
	req.UIDs = uids
	req.Truncated = truncated
	return req, true
}

// collectGrowthProgress 串行 + 随机间隔地取一批账号的进度。
//
// 为什么串行：与执行链路同一条防封原则 —— 同一出口短时间内并发打多个账号，
// 比顺序打更容易留下关联特征。只读查询虽轻，但没必要破坏这个不变量。
func (a *app) collectGrowthProgress(r *http.Request, accs []*Account, force bool) map[string]any {
	views := make([]growthProgressView, 0, len(accs))
	ctx := r.Context()
	for i, acc := range accs {
		if ctx.Err() != nil {
			break
		}
		if i > 0 {
			// 账号之间错开一点（亚秒级随机），避免 N 个请求挤在同一毫秒。
			if !sleepJitterCtx(ctx, growthProgressGapMinMillis, growthProgressGapMaxMillis) {
				break
			}
		}
		v := a.growthProgress(acc, force)
		sortGrowthItems(v.Items)
		views = append(views, v)
	}
	out := map[string]any{"views": views, "count": len(views)}
	if len(views) < len(accs) {
		out["interrupted"] = true
	}
	return out
}

// sleepJitterCtx 在 [minMs, maxMs) 内随机等待；ctx 取消返回 false。
func sleepJitterCtx(ctx context.Context, minMs, maxMs int) bool {
	d := time.Duration(minMs) * time.Millisecond
	if maxMs > minMs {
		d = time.Duration(minMs+rand.Intn(maxMs-minMs)) * time.Millisecond
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
