// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/taskledger"
)

// tokenFlushInterval 池内凭证回写数据库的兜底周期。
//
// 主路径其实不是这里：上游刷新 token 时会调 auth.SaveAtomic，我们注入的
// SaveHook（见 installCredentialHook）当场就把新凭证写进了 SQLite。这个循环
// 只兜住钩子覆盖不到的路径（如池内直接改写），并保证任何时刻库里都不比内存旧。
// 周期取 30s：token 有效期以小时计，这点延迟对正确性无影响，但能避免频繁写库。
const tokenFlushInterval = 30 * time.Second

// taskRunner 定时任务运行时：调度器生命周期 + 凭证回写 + 手动触发入口。
//
// 调度器可被 Reconfigure 热重建：管理员在后台改触发时刻/开关后立即生效，无需重启进程。
type taskRunner struct {
	mu        sync.Mutex
	sch       *scheduler.Scheduler
	schCancel context.CancelFunc
	proxy     *Proxy
	store     *Store
	dataDir   string
	cancel    context.CancelFunc
	started   time.Time
	// growthOK 成长任务脚本链路是否可用（python + scripts/growth_run.py）；
	// 不可用时调度器侧强制禁用该类任务（见 schedulerConfig）。
	growthOK bool
	// growth 待跑账号提供方（scheduler.GrowthProvider 实现）。
	growth growthProvider
	cfg    TaskConfig
	// 逐账号定时任务配置缓存（v0.11.0）：uid → 配置（未命中 = 参与全部任务）。
	// 由控制台/管理后台的开关更新，供调度器的 AccountTaskFilter 逐账号过滤。
	taskMu  sync.RWMutex
	taskCfg map[string]AccountTaskConfig
}

// installCredentialHook 把凭证落盘接到 SQLite。
//
// 为什么必须做这一步：调度器刷新 token 成功后会调用 auth.SaveAtomic()，而
// webapp 的账号来自数据库、没有 FilePath，原实现会直接返回 "no FilePath set"，
// 结果是「token 刷新成功但没持久化」——日志里刷一串 save 失败，重启后拿旧 token。
// 钩子把这条路径补上，且全程不落明文文件，与凭证加密落库的口径一致。
func installCredentialHook(store *Store) {
	auth.SetSaveHook(func(p auth.SavePayload) error {
		if p.UID == "" {
			return errors.New("empty uid in credential payload")
		}
		return store.UpdateTokens(p.UID, p.AccessToken, p.RefreshToken, p.ExpiresAt)
	})
}

// newTaskRunner 装配调度器。
//
// 自动排程的五类任务都是纯 Go HTTP 实现（签到 / 猫猫旅行 / 活跃上报 / token 保活 /
// 夜猫子），直接接管。
//
// 开学季排程已于 v0.27.0 **整体移除**：活动已结束，且它依赖外部 python 脚本、
// 配置项散在两处、长期是需要盯的故障源。脚本文件保留供人工执行，程序不再排程。
//
// 成长任务（internal/scheduler/growth.go）不进排程：它由管理员在账号池里选号
// 手动执行，这里只负责探测脚本链路可用性（growthOK），供 UI 决定是否给出入口。
//
// 触发时刻/开关等来自 kv 里的 TaskConfig（管理端可改，见 Reconfigure）。
// taskRunnerOnTokenDead 保活/签到前补刷新发现"refresh 已被上游作废"时的上报回调
// （v0.31.29）。由 main.go 注入（不改 newTaskRunner 签名，避免动到三处调用与测试）。
// nil = 不上报。
var taskRunnerOnTokenDead func(uid, nick, detail string)

func newTaskRunner(px *Proxy, store *Store, dataDir string) *taskRunner {
	growthOK := scheduler.GrowthScriptAvailable()
	tc := store.LoadTaskConfig()
	r := &taskRunner{proxy: px, store: store, dataDir: dataDir,
		growthOK: growthOK, growth: growthProvider{store: store}, cfg: tc,
		taskCfg: loadAccountTaskCfg(store)}
	sc := tc.schedulerConfig(px, dataDir, growthOK, r.growth, r.accountTaskFilter)
	sc.OnTokenDead = taskRunnerOnTokenDead
	r.sch = scheduler.New(sc)
	return r
}

// startScheduler 启动调度主循环（持锁调用）。
func (t *taskRunner) startScheduler() {
	ctx, cancel := context.WithCancel(context.Background())
	t.schCancel = cancel
	go t.sch.Run(ctx)
}

// Start 起调度主循环与凭证回写循环。
func (t *taskRunner) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel
	t.started = time.Now()
	t.mu.Lock()
	t.startScheduler()
	t.mu.Unlock()
	go t.flushLoop(ctx)
	// 台账清理（v0.29.0）：剪掉**已从本版本移除**的任务类别。
	//
	// 放在 Start（而不是构造函数）里：Start 才是"服务真的要开始工作"的那一刻，
	// 而首次落盘也发生在这里之后 —— 在构造里清会在纯单测（不 Start）时产生写副作用。
	// 清理结果务必打日志：静默删掉运维的历史数据，哪怕那是幽灵数据，也需要留痕。
	t.PruneLedger()
	log.Printf("定时任务已启用：%s", t.cfgSummary())
	// 脚本任务链路的**诊断行**（v0.26.9）：解释器探测过程 + 脚本根 + 成长任务可用性，
	// 一次说清。（开学季排程已于 v0.27.0 移除，不再出现在这里。）
	//
	// 此前这里只有一句"未探测到可用解释器/脚本，已禁用" —— 既不说试了哪个解释器、
	// 也不说去哪找脚本，运维拿着这句话无从下手。而本机实际原因很具体：
	// PATH 上的 `python3` 是 Microsoft Store 的存根（跑不起来），真解释器叫 `python`。
	// 现在解释器是**自动探测**的（见 scheduler.probePython），本行只负责把结论与过程摊开。
	log.Printf("脚本任务链路：%s；脚本根 %s；成长任务 %s",
		scheduler.PythonDiagnosis(), scheduler.ScriptRoot(), availText(t.growthOK))
}

// PruneLedger 从台账里删掉当前版本已不存在的任务类别（v0.29.0）。
//
// 场景：v0.27.0 把「开学季」从任务体系里整体移除，但 `task_ledger.json` 里
// 还留着它最后一轮的记录 —— 执行台账上于是持续显示一张 school 卡片，
// 点它没反应、也永远不会更新。它看起来像"这个任务还在，只是很久没跑"，
// 比"少一张卡"更容易把人引到错误的排查方向。
//
// 返回被清掉的类别（供日志）；无变化时返回空切片。
func (t *taskRunner) PruneLedger() []string {
	t.mu.Lock()
	sch := t.sch
	t.mu.Unlock()
	if sch == nil {
		return nil
	}
	dropped := sch.Ledger().DropKinds(knownLedgerKind)
	if len(dropped) > 0 {
		log.Printf("INFO: [tasks] 已从执行台账移除 %d 个当前版本不存在的任务类别：%s",
			len(dropped), strings.Join(dropped, "、"))
	}
	return dropped
}

// availText 可用性的人话文案（避免在日志里出现裸 true/false —— 那需要读者自己回忆极性）。
func availText(ok bool) string {
	if ok {
		return "可用"
	}
	return "不可用"
}

// Stop 停止调度（进程退出前调用）。
func (t *taskRunner) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.schCancel != nil {
		t.schCancel()
		t.schCancel = nil
	}
	if t.cancel != nil {
		t.cancel()
	}
}

// Reconfigure 用新配置热重建调度器（保存设置后调用，即时生效，无需重启）。
func (t *taskRunner) Reconfigure(tc TaskConfig) {
	tc = tc.normalize()
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.schCancel != nil {
		t.schCancel() // 停掉旧循环；Run(ctx) 收到取消后退出
	}
	t.cfg = tc
	sc := tc.schedulerConfig(t.proxy, t.dataDir, t.growthOK, t.growth, t.accountTaskFilter)
	sc.OnTokenDead = taskRunnerOnTokenDead
	t.sch = scheduler.New(sc)
	t.startScheduler()
	log.Printf("定时任务已重配置：%s", t.cfgSummary())
}

// Config 返回当前生效的任务配置（供管理端回显）。
func (t *taskRunner) Config() TaskConfig {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cfg
}

// cfgSummary 一段人类可读的排程摘要（日志用）。
func (t *taskRunner) cfgSummary() string {
	tc := t.cfg
	if !tc.Enabled {
		return "定时任务总开关已关闭"
	}
	sched := func(name string, hours []int, disabled bool) string {
		if disabled {
			return name + "关"
		}
		return name + intsJoin(hours) + "点"
	}
	// 成长任务不排程（手动选号执行），故不进排程摘要，只在末尾标注它是手动项。
	return strings.Join([]string{
		sched("签到", tc.CheckinHours, tc.CheckinDisabled),
		sched("旅行", tc.TravelHours, tc.TravelDisabled),
		sched("活跃", tc.ActivityHours, tc.ActivityDisabled),
		sched("保活", tc.KeepaliveHours, tc.KeepaliveDisabled),
		sched("夜猫子", tc.CatHours, tc.CatDisabled),
	}, "、") + "；成长任务：手动选号执行"
}

// itoa 小工具（避免为单处调用引入 strconv 到本文件的其他用途混淆）。
func itoa(n int) string {
	if n <= 0 {
		return "3"
	}
	return strconv.Itoa(n)
}

func intsJoin(xs []int) string {
	parts := make([]string, 0, len(xs))
	for _, x := range xs {
		parts = append(parts, strconv.Itoa(x))
	}
	return strings.Join(parts, "/")
}

// flushLoop 周期性把池内最新凭证对齐进数据库。
func (t *taskRunner) flushLoop(ctx context.Context) {
	tk := time.NewTicker(tokenFlushInterval)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			t.flushTokens()
		}
	}
}

// flushTokens 逐账号比对内存快照与库内记录，不一致才写回（避免无谓写放大）。
func (t *taskRunner) flushTokens() {
	for _, st := range t.proxy.pool.List() {
		au := t.proxy.pool.AuthByUID(st.UID)
		if au == nil {
			continue
		}
		snap := au.Snapshot() // 一次持锁取齐，杜绝逐字段直读的竞态
		if snap.AccessToken == "" {
			continue
		}
		acc, err := t.store.GetAccount(st.UID)
		if err != nil || acc == nil {
			continue
		}
		if snap.AccessToken == acc.AccessToken && snap.RefreshToken == acc.RefreshToken && snap.ExpiresAt == acc.ExpiresAt {
			continue
		}
		if err := t.store.UpdateTokens(st.UID, snap.AccessToken, snap.RefreshToken, snap.ExpiresAt); err != nil {
			log.Printf("回写凭证失败 %s: %v", st.UID, err)
		}
	}
}

// ---------------------------------------------------------------------------
// 手动触发
// ---------------------------------------------------------------------------

// runnableKinds 可手动触发的任务。
var runnableKinds = map[string]string{
	"checkin":   "每日签到（含余额查询与冷却解冻）",
	"travel":    "猫猫旅行（派出 + 领奖一趟闭环）",
	"activity":  "活跃上报（领猫前置 5 次对话）",
	"keepalive": "token 保活刷新",
	"cat":       "夜猫子（夜间对话点亮，窗口 23-08 CST）",
}

// ledgerKindGrowth 成长任务在台账里的类别名。
//
// 单独列出来是因为它**不在 runnableKinds 里**（它是手动选号执行，不属于
// "一键跑一类"），但它确实会往台账写 runs —— 若签名集合漏了它，
// 台账清理会把每一次成长任务的记录当成"已移除的类别"删掉。
const ledgerKindGrowth = "growth"

// knownLedgerKind 该类任务在当前版本是否仍然存在（台账只该保留这些类别）。
//
// 用途有两个，都在"清理历史残留"这条线上：
//  1. 启动时一次性剪掉已移除类别的台账记录（如 v0.27.0 移除的「开学季」）；
//  2. 出接口前再过滤一遍（防御：将来任何被删掉的类别都不会漏到界面）。
//
// 为什么是"存在性白名单"而不是"黑名单里写 school"：黑名单只能记住**已经**
// 被删掉的那些，下一次删任务时又要记得回来加一条 —— 而漏掉的表现正是本次这个
// 缺陷（界面上多出一张永远不会更新的幽灵卡片）。白名单天然对未来成立。
func knownLedgerKind(kind string) bool {
	if kind == ledgerKindGrowth {
		return true
	}
	_, ok := runnableKinds[kind]
	return ok
}

// ErrTaskBusy 任务正在跑（同一类任务已有执行中的一轮）。
var ErrTaskBusy = errors.New("task already running")

// manualRunTimeout 手动触发的单轮执行上限。
//
// 存在的意义是防止「一次点击留下一个永久卡死的 goroutine」：某类任务若因上游
// 挂起而永远不返回，没有上限的话它会一直占着运行位，后续所有触发都被判定
// 「撞车让路」——表现就是「定时任务再也不跑了」。30 分钟远大于任何一类任务的
// 正常耗时（最慢的活跃上报 18 号 × 5 条约 2~3 分钟）。
const manualRunTimeout = 30 * time.Minute

// RunNow 触发一类任务执行。
//
// **异步语义**（v0.17.4 从同步改为异步）：立即返回「已开始」，执行在后台推进，
// 结果进执行台账。
//
// 为什么改（此前是同步阻塞，属于实测踩坑）：
//  1. 同步时 POST 要等任务整轮跑完才返回 —— 保活 18 个号实测 6.4s，活跃上报
//     更是 2~3 分钟。前端全程只有一个「正在执行…」的静态提示，**期间看不到任何
//     进度**，这就是「点了没反应 / 反馈慢」的直接来源；
//  2. 长耗时同步请求会撞反向代理的读超时（本地直连没事，一旦挂到 tunnel/nginx
//     前面，60s 就吃 504 —— 前端收到的不是「正在跑」而是「失败」，观感更差）；
//  3. 参考同类实现也是异步 + 进度轮询（见 docs/风控策略调研.md），
//     并明确记录「同步跑会先撞 proxy_read_timeout，前端吃 504、体感是『点了没反应』」。
//
// 撞车（该类任务已在执行）时返回 ErrTaskBusy（HTTP 409）；前端按钮在 running
// 期间本就禁用，这条主要兜住「定时轮次正在跑时被手动点一下」。
func (t *taskRunner) RunNow(kind string) (map[string]any, error) {
	t.mu.Lock()
	sch := t.sch
	t.mu.Unlock()
	if sch == nil {
		return nil, fmt.Errorf("定时任务未启用")
	}
	if sch.IsRunning(kind) {
		return nil, ErrTaskBusy
	}

	// 在后台跑一轮，带超时；失败只记日志（结果口径由台账/运行态对外表达）。
	runAsync := func(name string, fn func()) {
		go func() {
			defer func() {
				// goroutine 里 panic 会带走整个进程（与 New 里 ledger 恒非 nil 同口径）。
				if r := recover(); r != nil {
					log.Printf("task %s: panic: %v", name, r)
				}
			}()
			fn()
		}()
	}

	switch kind {
	case "checkin":
		// 用 RunCheckinNowDetailed 而非 CheckinAll：后者只执行不记账，
		// 手动触发的这一轮会从台账面板里消失（明细与记账都要）。
		runAsync(kind, func() {
			if _, err := t.sch.RunCheckinNowDetailed(); err != nil && !errors.Is(err, scheduler.ErrBusy) {
				log.Printf("checkin(manual): %v", err)
			}
		})
	case "travel":
		runAsync(kind, func() {
			ctx, cancel := context.WithTimeout(context.Background(), manualRunTimeout)
			defer cancel()
			t.sch.RunTravelFor(ctx)
		})
	case "activity":
		runAsync(kind, func() {
			ctx, cancel := context.WithTimeout(context.Background(), manualRunTimeout)
			defer cancel()
			t.sch.RunActivityFor(ctx)
		})
	// 这三类**刻意不在此处套 manualRunTimeout**（v0.24.1，P3-10 复核结论）：
	// travel/activity 走的是"一次性批量请求"，必须靠外层 ctx 兜底；
	// 而这三类各自在**更内层**已经有界：
	//   · keepalive —— 逐账号调 RefreshToken，走 HTTP 客户端（Timeout 120s），
	//     整体上界 = 账号数 × 120s，不会无限挂；
	//   · cat      —— 同上，走 growth 域的上游调用。
	// 在这些入口再套一层只会让日志出现两种互相矛盾的超时来源，故不加。
	case "keepalive":
		runAsync(kind, t.sch.RunKeepaliveNow)
	case "cat":
		runAsync(kind, t.sch.RunCatNow)
	default:
		return nil, fmt.Errorf("unknown task %q", kind)
	}
	// 统一回报「已开始」：执行在后台推进，进度看 /api/admin/tasks 的 running
	// 与执行台账（前端会自动轮询，不需要用户手动刷新）。
	return map[string]any{
		"kind":   kind,
		"status": "started",
		"note":   "已在后台开始执行，进度与结果会自动出现在下方执行台账",
	}, nil
}

// RunGrowth 对**指定账号**执行一轮成长任务（手动选号，v0.17.0）。
//
// 与 RunNow 的关系：成长任务不参与自动排程，也不属于「一键跑一类」的语义
// ——它必须知道自己要跑哪些号。因此单独一个入口，而不是挤进 RunNow 的 switch。
//
// 异步执行：一批账号含号间 30~120s 随机等待，同步返回会让管理页请求超时。
// 因此这里立刻返回「已开始」，结果去执行台账看。
func (t *taskRunner) RunGrowth(uids []string) (map[string]any, error) {
	if !t.growthOK {
		// 透出**逐项诊断**而不是一句"缺 python 或脚本"（v0.31.22，真机教训）：
		// 那种说法缺了"缺哪一项""去哪里找"，运维只能猜 —— 而真实原因往往是
		// 部署时漏拷 scripts/（python 其实是好的）。
		return nil, fmt.Errorf("成长任务脚本链路不可用：%s。修法：把 scripts/ 目录放到 webapp 同级（或用 WB2A_PYTHON 指定可用的 python）", scheduler.GrowthDiagnosis())
	}
	if len(uids) == 0 {
		return nil, fmt.Errorf("请先选择要执行成长任务的账号")
	}
	t.mu.Lock()
	sch := t.sch
	t.mu.Unlock()
	if sch == nil {
		return nil, fmt.Errorf("定时任务未启用")
	}
	// 预检 + 过滤：只把「存在、未停用、有 access token」的号交给执行链路。
	//   · 停用号不打请求（disabled 是人工/系统的最终裁决，不该被一次手动执行复活）；
	//   · 无 token 的号必然失败，先滤掉避免刷一条无意义的失败日志；
	//   · 全都不可跑时直接报错，而不是返回「已开始」然后静默什么都没做 ——
	//     异步执行下用户看不到那句被吞掉的跳过日志。
	accs, err := t.store.AccountsByUIDs(uids)
	if err != nil {
		return nil, fmt.Errorf("校验选中账号失败：%w", err)
	}
	runnable := make([]string, 0, len(accs))
	skipped := 0
	for _, acc := range accs {
		if acc.Disabled || acc.AccessToken == "" {
			skipped++
			continue
		}
		runnable = append(runnable, acc.UID)
	}
	if len(runnable) == 0 {
		return nil, fmt.Errorf("选中的账号都不可执行（不存在、已停用或缺少 access token）")
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), growthRunTimeout(len(runnable)))
		defer cancel()
		sch.RunGrowthFor(ctx, runnable)
	}()
	res := map[string]any{"kind": "growth", "status": "started", "count": len(runnable),
		"note": fmt.Sprintf("已在后台开始执行 %d 个账号（逐个串行，账号之间随机等待数十秒到两分钟）",
			len(runnable))}
	if skipped > 0 {
		res["skipped"] = skipped
		res["note"] = fmt.Sprintf("已在后台开始执行 %d 个账号（另有 %d 个已停用/无凭证被跳过）",
			len(runnable), skipped)
	}
	return res, nil
}

// handleHostGrowthRun 用户侧：对**本人代挂账号**执行一轮成长任务。
//
// 权限口径与其他 /api/host/* 一致：只认 owner_user_id == 当前用户的账号，
// 非本人 uid 静默剔除（不区分「不存在」与「不属于你」，避免把 uid 变成可探测信息）。
func (a *app) handleHostGrowthRun(w http.ResponseWriter, r *http.Request) {
	if a.tasks == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "定时任务未启用"})
		return
	}
	var req struct {
		UIDs []string `json:"uids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	u := userFrom(r)
	mine, err := a.store.ListAccountsByOwner(u.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	owned := make(map[string]bool, len(mine))
	for _, acc := range mine {
		owned[acc.UID] = true
	}
	seen := make(map[string]bool, len(req.UIDs))
	uids := make([]string, 0, len(req.UIDs))
	for _, uid := range req.UIDs {
		uid = strings.TrimSpace(uid)
		if uid == "" || seen[uid] || !owned[uid] {
			continue
		}
		seen[uid] = true
		uids = append(uids, uid)
	}
	if len(uids) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请先选择要执行成长任务的账号"})
		return
	}
	res, err := a.tasks.RunGrowth(uids)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// growthRunTimeout 手动执行的上下文超时：按号数放大，避免选了大批号时中途被截断。
// 每个号两个阶段（zero + talk）各留足 6 分钟，再叠加号间等待，故按 15 分钟/号估算。
// 上下限收敛在 [30min, 6h]：太少会误伤大批次，太多等于没有超时保护。
func growthRunTimeout(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	d := time.Duration(n) * 15 * time.Minute
	if d < 30*time.Minute {
		d = 30 * time.Minute
	}
	if d > 6*time.Hour {
		d = 6 * time.Hour
	}
	return d
}

// ---------------------------------------------------------------------------
// 新入池账号补跑（v0.23.3）
// ---------------------------------------------------------------------------

// catchupMinDelay / catchupMaxDelay 新号补跑的随机延迟区间（秒）。
//
// 为什么要延迟而不是立刻跑：导入路径本身**已经**会立刻为新号跑一轮「拉余额」
// （refreshCreditsAsync，见 import_desktop.go）。补跑签到若也立刻执行，新号入池的
// 一瞬间会连打 3 个上游接口（余额 / 签到 / 余额）—— 这比定时轮次更明显的机器特征
// （定时轮次还有批内错峰）。错开一两分钟既不影响"当天有记录"，也不把动作挤在一起。
const (
	catchupMinDelay = 45
	catchupMaxDelay = 120
)

// catchupDelayFixed 测试用的固定延迟（>0 时取代随机值）。
// 与 wakeupGraceDelay「测试可缩短」、travelAccountDelay「测试可置 0」同口径。
var catchupDelayFixed time.Duration

// catchupDelay 在 [catchupMinDelay, catchupMaxDelay] 秒内随机取一个延迟。
func catchupDelay() time.Duration {
	if catchupDelayFixed > 0 {
		return catchupDelayFixed
	}
	span := catchupMaxDelay - catchupMinDelay + 1
	secs := catchupMinDelay + int(time.Now().UnixNano()%int64(span))
	return time.Duration(secs) * time.Second
}

// catchupTargets 把待补跑的 uid 收敛为「去空白、去重、有上限」的执行列表。
// 上限与「刷新余额」同口径（creditsRefreshMaxAccounts），避免一次导入几千个号时
// 在后台堆出一长串出站。
//
// 为什么要 TrimSpace：uid 来自批量导入的 JSON，手工编辑过的文件里带前导/尾随空格
// 很常见（单测就是在这一步抓到只判空串、不剪空白会让 "  " 混进列表 —— 它会静默
// 匹配不到池内账号，白跑一趟且日志里看不出问题）。
func catchupTargets(uids []string) []string {
	seen := make(map[string]bool, len(uids))
	list := make([]string, 0, len(uids))
	for _, u := range uids {
		u = strings.TrimSpace(u)
		if u == "" || seen[u] || len(list) >= creditsRefreshMaxAccounts {
			continue
		}
		seen[u] = true
		list = append(list, u)
	}
	return list
}

// catchupSkipReason 补跑的前置检查：返回非空表示**不补跑**，值是原因（进日志）。
//
// 提取成纯函数是为了可测 —— 「尊重运营配置」这条规则不该只靠集成测试覆盖：
// 总开关关闭或自动签到被单独禁用时，补跑必须闭嘴（否则等于绕过显式设置打上游）。
func (t *taskRunner) catchupSkipReason() string {
	if t == nil {
		return "定时任务未启用"
	}
	cfg := t.Config()
	if !cfg.Enabled {
		return "定时任务总开关已关闭"
	}
	if cfg.CheckinDisabled {
		return "自动签到已被禁用（尊重运营设置）"
	}
	return ""
}

// CatchupNewAccounts 为新入池账号补跑一轮签到（v0.23.3）。
//
// 触发点：Proxy.SyncAccountsReport 发现**新增**账号后，通过 onAccountsAdded 回调
// 调到这里 —— 覆盖导入 JSON / 扫码添加 / 代挂审批通过入池 / 管理端「重同步」发现
// 新号等全部入池路径（它们都走同一次 SyncAccounts）。
//
// 为什么必须有它：账号在**当天触发点之后**入池时，当天的签到轮次已经过去，最长要
// 等 ~21 小时才会被下一次排到。而签到正是新号拿到当日额度、把权威余额写回池内的
// 那一步 —— 空窗期里新号既不参与平台消耗，仪表盘上的积分也一直是 0。用户侧看到的
// 现象就是「新加入的账号没有执行定时任务」。
//
// 设计取舍：
//   - **尊重配置**：总开关关闭或「自动签到」被单独禁用时不补跑 —— 补跑不能绕过
//     运营显式关掉的东西；
//   - **不记台账**：见 scheduler.CheckinUIDs 的说明（补跑只覆盖一两个号，记进
//     「最近一轮」会把完整轮次的记录覆盖成「共 2 个」）；
//   - **异步 + 延迟**：见 catchupDelay；
//   - 失败只记日志：补跑是一次尽力而为的补偿，不该影响调用方的请求。
func (t *taskRunner) CatchupNewAccounts(uids []string) {
	if t == nil || len(uids) == 0 {
		return
	}
	if why := t.catchupSkipReason(); why != "" {
		log.Printf("新号补跑跳过：%s", why)
		return
	}
	t.mu.Lock()
	sch := t.sch
	t.mu.Unlock()
	if sch == nil {
		return
	}
	list := catchupTargets(uids)
	if len(list) == 0 {
		return
	}

	go func() {
		defer func() {
			// goroutine 里 panic 会带走整个进程（与 RunNow 的 runAsync 同口径）。
			if r := recover(); r != nil {
				log.Printf("新号补跑: panic: %v", r)
			}
		}()
		d := catchupDelay()
		log.Printf("新号补跑：%d 个账号将在 %s 后补跑一轮签到（避免入池瞬间连打多个接口）",
			len(list), d.Round(time.Second))
		time.Sleep(d)
		out, err := sch.CheckinUIDs(list)
		if err != nil {
			log.Printf("新号补跑签到失败（%d 个账号）：%v", len(list), err)
			return
		}
		var ok, already, failed int
		for _, oc := range out {
			switch oc.Status {
			case scheduler.CheckinOK:
				ok++
			case scheduler.CheckinAlready:
				already++
			case scheduler.CheckinFail:
				failed++
			}
		}
		log.Printf("新号补跑签到完成：成功 %d · 今日已签 %d · 失败 %d · 合计 %d",
			ok, already, failed, len(out))
	}()
}

// Ledger 返回任务执行台账（近期各任务结果 + 当日重试状态）。
func (t *taskRunner) Ledger() map[string]any {
	t.mu.Lock()
	sch := t.sch
	t.mu.Unlock()
	if sch == nil {
		return map[string]any{"runs": map[string]any{}, "retry_states": map[string]any{}}
	}
	l := sch.Ledger()
	// 出接口前按「当前版本仍存在」的类别过滤（v0.29.0）。启动时的 DropKinds 已经
	// 清过一次，这里是第二道闸：进程长期运行期间若发生"任务被下线"的运行时变更，
	// 界面也不会凭空冒出一张永远不会更新的幽灵卡片。
	out := map[string]any{
		"runs":          filterLedgerRuns(l.Runs()),
		"retry_states":  filterLedgerRetries(l.RetryStates()),
		"scheduler_up":  t.started.Unix(),
		"scheduler_sec": int64(time.Since(t.started).Seconds()),
	}
	// 运行态 + 下次触发时刻（v0.17.4）：让「定时有没有生效」「任务是不是正在跑」
	// 这两件事对前端可见 —— 此前只能靠台账的耗时反推，而台账只在跑完才写一笔，
	// 「点了没反应」成了最常见的误判。
	out["running"] = sch.RunningKinds()
	// 上次**自动**触发的快照：台账（runs）只保留最近一轮、会被人工触发覆盖，
	// 这份让「定时跑过没」始终可查（v0.17.4）。
	out["last_auto_runs"] = filterLedgerRuns(sch.LastAutoRuns())
	next := map[string]nextRunView{}
	now := time.Now()
	for k, at := range sch.NextRuns() {
		next[k] = nextRunView{
			At: at.Unix(),
			// Rel 相对秒数由**服务端**算好：不依赖浏览器时钟。
			// 客户端自己拿 at 减本地时间会在「用户机器时间不准 / 时区不同」时显示错乱，
			// 而这种错乱看起来就像「定时任务坏了」。
			Rel: int64(at.Sub(now).Seconds()),
		}
	}
	out["next_runs"] = next
	return out
}

// filterLedgerRuns 只保留当前版本仍存在的任务类别。
//
// 返回**新 map**而不是原地删：调用方拿到的可能是调度器内部结构的副本，
// 原地改会让"过滤"这个只读动作产生写副作用（下一处调用者会拿到已删过的视图）。
func filterLedgerRuns(m map[string]taskledger.Run) map[string]taskledger.Run {
	out := make(map[string]taskledger.Run, len(m))
	for k, v := range m {
		if knownLedgerKind(k) {
			out[k] = v
		}
	}
	return out
}

// filterLedgerRetries 同上，作用于当日重试状态。
func filterLedgerRetries(m map[string]taskledger.RetryState) map[string]taskledger.RetryState {
	out := make(map[string]taskledger.RetryState, len(m))
	for k, v := range m {
		if knownLedgerKind(k) {
			out[k] = v
		}
	}
	return out
}

// nextRunView 一类任务的下次触发时刻。
type nextRunView struct {
	// At 绝对时刻（unix 秒），已含 jitter 偏移 = 实际会跑的时间，不是名义整点。
	At int64 `json:"at"`
	// Rel 距现在还有多少秒（服务端算，避免依赖浏览器时钟）。
	Rel int64 `json:"rel"`
}

// ---------------------------------------------------------------------------
// HTTP 处理器
// ---------------------------------------------------------------------------

// handleGrowthTasks 只读排查出口：返回某账号 growth 任务列表的**原始** data 段。
// 用途：夜猫子/开学季「accept 未登记生效」这类问题时，需要看到上游真实返回
// （任务是否在列表里、accept_status 实际取值、progress 是否 null），
// 而不是被本地结构体过滤后的视图。
func (a *app) handleGrowthTasks(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if uid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing uid"})
		return
	}
	au := a.proxy.pool.AuthByUID(uid)
	if au == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "account not in pool"})
		return
	}
	raw, err := a.proxy.uc.GrowthTasksRaw(au)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, http.StatusOK, map[string]any{"uid": uid, "data": json.RawMessage(raw)})
}

// handleGrowthTaskAccept 代某账号调用 growth 任务 accept 并**原样**透出上游回包。
// 仅供运维排查「accept 未登记生效」类问题（需要看上游真实的 code/msg/results）。
// 显式 POST，避免 GET 带副作用。
func (a *app) handleGrowthTaskAccept(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	au := a.proxy.pool.AuthByUID(uid)
	if au == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "account not in pool"})
		return
	}
	var req struct {
		TaskCodes []string `json:"task_codes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.TaskCodes) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "需要 task_codes 数组"})
		return
	}
	status, err := a.proxy.uc.AcceptGrowthTaskRaw(au, req.TaskCodes)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"uid": uid, "task_codes": req.TaskCodes, "data": status})
}

// handleTasks 任务台账 + 可触发任务清单。
func (a *app) handleTasks(w http.ResponseWriter, r *http.Request) {
	if a.tasks == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	kinds := make([]map[string]string, 0, len(runnableKinds))
	for k, desc := range runnableKinds {
		kinds = append(kinds, map[string]string{"kind": k, "desc": desc})
	}
	out := a.tasks.Ledger()
	out["enabled"] = true
	out["kinds"] = kinds
	// 成长任务为**手动选号执行**，不属于「一键跑一类」，故不进 kinds；
	// 只把脚本可用性透出，供前端决定是否显示「跑成长任务」入口。
	out["growth_available"] = a.tasks.growthOK
	writeJSON(w, http.StatusOK, out)
}

// handleGrowthRun 手动对**选中的账号**执行一轮成长任务。
//
// 为什么是独立路由而不是复用 POST /api/admin/tasks/{kind}/run：成长任务的语义是
// 「跑这些号」，必须带 uid 列表；塞进「跑这一类」的通用入口会让「点了没反应
// （因为没选号）」变成难以理解的失败。
//
//	POST /api/admin/growth/run   body: {"uids": ["...", "..."]}
func (a *app) handleGrowthRun(w http.ResponseWriter, r *http.Request) {
	if a.tasks == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "定时任务未启用"})
		return
	}
	var req struct {
		UIDs []string `json:"uids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	// 去空、去重（同一 uid 出现两次会让同一个号被连跑两轮）。
	seen := make(map[string]bool, len(req.UIDs))
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
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请先选择要执行成长任务的账号"})
		return
	}
	res, err := a.tasks.RunGrowth(uids)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleTaskRun 手动触发一类任务。
func (a *app) handleTaskRun(w http.ResponseWriter, r *http.Request) {
	if a.tasks == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "tasks disabled"})
		return
	}
	kind := r.PathValue("kind")
	// 白名单：只允许 runnableKinds 里的类别。
	// 成长任务不在此列：它需要带 uid 列表，走 POST /api/admin/growth/run。
	// 开学季已于 v0.27.0 移除，不再有对应入口。
	if _, ok := runnableKinds[kind]; !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown task kind: " + kind})
		return
	}
	res, err := a.tasks.RunNow(kind)
	switch {
	case errors.Is(err, ErrTaskBusy):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "该任务正在执行中，请稍后再试"})
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
	default:
		writeJSON(w, http.StatusOK, res)
	}
}
