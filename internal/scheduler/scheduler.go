// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// Package scheduler 定时任务：签到 / 活跃上报 / 猫猫旅行 / token keepalive / 开学季 / 夜猫子 六类独立排程。
// 签到成功后重新查余额，余额 > 0 的冷却账号自动解冻。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"math/rand"
	"sort"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/taskledger"
	"workbuddy2api/internal/upstream"
)

// Config 调度器依赖。
//
// 任务开关用「禁用」命名而非「启用」：零值 Config 即六类任务都启用（hours 回落默认），
// 与引入开关前的行为逐字一致（老调用方/老测试无需改动）。
type Config struct {
	Pool           *pool.Pool
	Upstream       *upstream.Client
	CheckinHours   []int // 默认 [9, 21]
	TravelHours    []int // 默认 [9,21]：一趟派出 + 一趟领奖闭环
	ActivityHours  []int // 默认 [10]
	KeepaliveHours []int // 默认 [22]
	CatHours       []int // 默认 [1]：夜猫子任务（迁移自系统 crontab）
	// JitterMinutes 触发时刻抖动窗口（分钟）：每类任务的触发时刻在该窗口内取一个
	// **确定性**偏移，把「所有部署都在整点同一秒打上游」摊开，对 WAF 友好。
	// 0/缺省 = 不加偏移（精确整点，与引入前逐字一致）。
	JitterMinutes int
	// ActivityReportCount 每号每次活跃上报的条数：领猫前置需 5 次对话，
	// 默认 5 条同一 conversationId 内多轮上报把 chat_5 刷满；0/缺省=1 兼容旧行为。
	ActivityReportCount int

	// ExpiringSoonWindow 快过期积分窗口：签到查余额时，把到期时间 <= now+window 的
	// 套餐余额标记为"快过期"（pool 据此优先消耗，见 entry.creditsExpiring）。
	// <=0 时禁用分桶（全部归长期，行为与引入前一致）。默认建议 7*24h。
	ExpiringSoonWindow time.Duration

	// CheckinDisabled 显式关闭签到排程（对应 config 的 schedule.checkin_enabled=false）。
	// 禁用后不再有任何签到时点。旅行不再搭签到便车（已剥离为独立排程）。
	CheckinDisabled bool
	// TravelDisabled 显式关闭猫猫旅行排程（schedule.travel_enabled=false）。
	TravelDisabled bool
	// ActivityDisabled 显式关闭活跃上报排程（schedule.activity_enabled=false）。
	ActivityDisabled bool
	// KeepaliveDisabled 显式关闭 token 保活排程（schedule.keepalive_enabled=false）。
	KeepaliveDisabled bool
	// KeepaliveJitterMs 保活每次刷新前的随机等待上限（毫秒，v0.31.29）。
	// 0 = 不等待（旧行为）。默认 800：38 个号即使都在同一小时，也不会打成同一秒。
	KeepaliveJitterMs int
	// OnTokenDead 账号 refresh 被判失效（12153）时的回调（v0.31.29）。
	//
	// 为什么需要：2026-10-18 全池 token 失效时，页面上只显示"签到：不支持"，
	// 没有任何地方告诉运维"这些号的登录已经废了"。回调让 webapp 把这件事
	// 立刻写进风控台账 + 概览告警，而不是等三个周期后被动停用。
	// nil = 不上报（测试与旧装配不受影响）。
	OnTokenDead func(uid, nickname, detail string)
	// CatDisabled 显式关闭夜猫子任务排程（schedule.cat_enabled=false）。
	CatDisabled bool

	// ---- 成长任务（v0.17.0，**纯手动**）----
	// 成长任务**不参与自动排程**：它会对账号发起真实写请求（上报 / accept / claim），
	// 且每个号的事件链要按客户端的四类指纹分别构造，风险与成本都远高于其他任务。
	// 因此由管理员在账号池里**显式选号**执行（POST /api/admin/growth/run），
	// 调度器只提供执行引擎（Growth Provider + gap 节流），不提供触发时刻。
	// 下面这些字段只服务于「手动执行这一批账号」这一场景。
	//
	// Growth 账号凭证的提供方（nil = 功能不可用）。
	Growth GrowthProvider
	// GrowthGapMin/MaxSeconds 账号之间的随机等待区间（秒，默认 30~120）。
	// 刻意不用固定值：固定间隔本身就是可被统计出来的节奏特征。
	GrowthGapMinSeconds int
	GrowthGapMaxSeconds int

	// ---- 批内任务错峰（v0.17.3）----
	// BatchStaggerMin/MaxSeconds 同一唤醒时刻派发多类任务时，任务之间的随机等待
	// 区间（秒，默认 60~150）。0/0 = 关闭（行为与引入前逐字一致）。
	//
	// 为什么需要：同一时点常派发多类任务（签到与旅行都配了 9 点），而它们遍历的是
	// **同一批账号** —— 不加间隔时一个账号会在几十秒内被签到 → 旅行 → 上报连着打好
	// 几轮，这是比「单任务高频」更明显的机器特征。
	//
	// 为什么在执行层拉开而不是给排程时刻加随机偏移：后者会把偏移量叠加到下一次的
	// 名义时刻上，逐日累积漂移，几个月后执行时刻绕时钟转一圈。
	BatchStaggerMinSeconds int
	BatchStaggerMaxSeconds int

	// RetryDelayMinutes 当日失败重试延迟（分钟）。**0 = 关闭**（缺省，行为与引入前
	// 逐字一致）。>0 时，某类任务一轮「全灭」后在该延迟后再跑一次（判据见
	// recordRun 的 AllFailed 注释），当日最多 RetryMaxPerDay 次。
	RetryDelayMinutes int
	// RetryMaxPerDay 每类任务每日最多重试次数。仅 RetryDelayMinutes>0 时生效；
	// <=0 与关闭等价（由 taskledger.PlanRetry 判为 disabled）。
	RetryMaxPerDay int
	// LedgerFile 任务执行台账落盘路径。空 = 纯内存（重启后只剩新跑过的记录）。
	// 台账是观测数据，落盘失败只记日志、不影响任务执行，故不做启动期 fail-fast。
	LedgerFile string

	// KeepaliveDays 保活阈值（天）：仅当 token 在 N 天内到期才刷新。
	// 0 = 关闭该判据（无条件刷新，与引入前逐字一致）。
	KeepaliveDays int
	// LazyRefreshHours 惰性刷新（小时）：token 剩余有效期 > N 小时则跳过一次刷新。
	// 0 = 关闭该判据。用于降低保活频次、减少对上游的无谓调用（防封）。
	LazyRefreshHours int

	// AccountTaskFilter 逐账号任务过滤（v0.11.0）：返回 false 表示该账号不参与该类任务。
	// nil = 全部账号参与全部任务（与引入前逐字一致，零值安全）。
	// task 取值为任务类名：checkin / travel / activity / keepalive / cat。
	AccountTaskFilter func(uid, task string) bool
}

// Scheduler 调度器。
type Scheduler struct {
	cfg Config

	// mu/adoptTried 领养当日失败记录：uid → 自然日（CST）。门槛未达的账号当日不再重试，
	// 避免同日多趟对上游重试轰炸；进程重启即清零（无需持久化）。
	mu         sync.Mutex
	adoptTried map[string]string

	// rewardClaimed 连登奖励按自然日（CST）领取标记：uid → 当日日期。已领/已尝试的账号
	// 当日不再重打 redeem（领取类写操作按天幂等，避免对上游重复写请求）；自然日 00:00
	// CST 重置（上游增长体系按 CST 自然日刷新，见 travelDay/cstZone）。进程重启即清零
	// （服务端幂等兜底：重启后当日重复 redeem 会拿 409 正常态，无副作用）。
	rewardClaimed map[string]string

	// checkinMu 串行化签到：定时入口与手动触发互斥，避免同一时刻重复打上游签到接口。
	// 只有签到需要「返回 ErrBusy」这种带错误的互斥语义（手动入口要把「正在跑」告诉前端），
	// 其余任务类用下面的 running 通用闸门（撞车即静默跳过 + 日志）。
	checkinMu sync.Mutex

	// runMu/running 任务级运行闸门（v0.17.3）：同一类任务同时只允许一趟在跑。
	//
	// 为什么需要（踩坑后的修正）：定时轮次与手动「立即执行」是**两条独立路径** ——
	// 定时器到点派发的同时，管理员可能正好点了同一个任务的「立即执行」。此前只有签到
	// 有互斥，其余四类会**双倍打上游**（风控面翻倍），脚本类还可能出现两个 python 子进程
	// 并发跑同一批账号。三层互斥缺一不可：入口去重只防得住「两次手动」，防不住
	// 「手动 vs 定时」。
	runMu   sync.Mutex
	running map[taskKind]bool

	// lastAuto 各类任务最近一次**自动触发**（定时/重试）的结果快照（v0.17.4）。
	// 台账只保留最近一轮，人工触发会覆盖它；这份快照让「定时跑过没」始终可查。
	lastAutoMu sync.Mutex
	lastAuto   map[taskKind]taskledger.Run

	// ledger 任务执行台账 + 当日失败重试状态（ledger.go）。恒非 nil（New 构造）；
	// 仅当直接手搓 &Scheduler{}（测试）时才为 nil——此时记台账与重试都是空操作。
	// 与 Handler.budget 的直通语义同口径：观测组件缺席不该让任务本身 panic
	// （调度 goroutine 里 panic 会带走整个进程）。
	ledger *taskledger.Store

	// nowFn 时钟注入点：默认 time.Now。夜猫子任务要按 CST 判定 23-08 窗口，
	// 窗口判定若不注入时钟就只能靠真实时间测，测试会在一天里大部分时段失真。
	nowFn func() time.Time
}

// taskAllowed 判定某账号是否参与某类任务（过滤器缺席 = 全部参与）。
// task 取任务类名（checkin/travel/activity/keepalive/cat）。
func (s *Scheduler) taskAllowed(uid, task string) bool {
	if s.cfg.AccountTaskFilter == nil {
		return true
	}
	return s.cfg.AccountTaskFilter(uid, task)
}

// now 取当前时钟（注入点；未设置回落 time.Now）。
func (s *Scheduler) now() time.Time {
	if s.nowFn != nil {
		return s.nowFn()
	}
	return time.Now()
}

// Ledger 返回任务执行台账（只读视图），供 server 的 /status 与 /metrics 渲染。
// 返回窄接口能拿到的具体类型：*taskledger.Store 结构上即满足 server 侧的
// TaskLedgerReader，无需为接线改动本包。
func (s *Scheduler) Ledger() *taskledger.Store { return s.ledger }

// 批内任务错峰的默认区间（秒）。仅在调用方显式开启（MaxSeconds>0）时生效，
// 用于补 min 缺省值；两者都为 0 表示关闭。
const defaultBatchStaggerMinSec = 60

// New 构建。
func New(cfg Config) *Scheduler {
	if len(cfg.CheckinHours) == 0 {
		cfg.CheckinHours = []int{9, 21}
	}
	if len(cfg.TravelHours) == 0 {
		cfg.TravelHours = []int{9, 21}
	}
	if len(cfg.ActivityHours) == 0 {
		cfg.ActivityHours = []int{10}
	}
	if len(cfg.KeepaliveHours) == 0 {
		cfg.KeepaliveHours = []int{22}
	}
	if len(cfg.CatHours) == 0 {
		cfg.CatHours = []int{1}
	}
	// 0/缺省 = 1 条（兼容旧行为：每号每天 1 条上报点亮连登）。
	if cfg.ActivityReportCount <= 0 {
		cfg.ActivityReportCount = 1
	}
	return &Scheduler{
		cfg:           cfg,
		adoptTried:    make(map[string]string),
		rewardClaimed: make(map[string]string),
		ledger:        taskledger.New(cfg.LedgerFile),
	}
}

// checkinRefreshSkew 签到前判定"token 是否临近过期"的时间窗口（10 分钟）。
// 长时间停机/容器长期停跑后 access token 往往已过期，不先刷新则签到必然 401 白跑。
const checkinRefreshSkew = 10 * time.Minute

// CheckinStatus 单账号签到结果状态。
type CheckinStatus string

const (
	CheckinOK      CheckinStatus = "ok"      // 签到成功
	CheckinAlready CheckinStatus = "already" // 上游判定今天已签到（幂等重复，视为正常）
	CheckinFail    CheckinStatus = "fail"    // 刷新 token / 签到 / 余额查询失败
	CheckinSkipped CheckinStatus = "skipped" // 禁用账号或无有效凭证，未参与
)

// CheckinOutcome 单账号签到结果（供手动签到回执与日志汇总）。
type CheckinOutcome struct {
	UID      string        `json:"uid"`
	Nickname string        `json:"nickname,omitempty"`
	Status   CheckinStatus `json:"status"`
	Credits  *int64        `json:"credits,omitempty"` // 签到后余额（余额查询成功才有值）
	Detail   string        `json:"detail,omitempty"`  // 失败/跳过原因（"已签到"不填）
}

// ErrBusy 已有一次签到正在执行（手动入口与定时撞车）。
var ErrBusy = errors.New("checkin already running")

// beginRun 尝试占用某类任务的运行位；false = 已有一趟在跑，调用方应放弃本轮。
//
// 语义刻意是「让路」而非「排队」：排队会让一次手动点击在上一次定时轮次结束后
// 又完整打一遍上游 —— 那正是互斥想避免的事。宁可少跑一轮，也不重复写。
func (s *Scheduler) beginRun(k taskKind) bool {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if s.running == nil {
		s.running = map[taskKind]bool{}
	}
	if s.running[k] {
		return false
	}
	s.running[k] = true
	return true
}

// endRun 释放运行位。
func (s *Scheduler) endRun(k taskKind) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	delete(s.running, k)
}

// skipIfBusy 通用的「撞车即让路」入口守卫：占用成功返回 false（调用方继续跑），
// 已有一趟在跑时记一条日志 + 台账 skipped 并返回 true（调用方直接 return）。
//
// 只记 skipped 不记 fail：撞车不是故障，是互斥按预期生效。记 fail 会让台账
// 天天报「全灭」并触发毫无意义的当日重试（与 travel「跳过≠失败」同口径）。
func (s *Scheduler) skipIfBusy(k taskKind, trigger string) bool {
	if s.beginRun(k) {
		return false
	}
	log.Printf("%s: 已有同一类任务在执行中（trigger=%s），本轮让路避免重复打上游", k, trigger)
	return true
}

// IsRunning 指定任务类此刻是否正在执行（按类名字符串判断，便于 webapp 侧调用）。
func (s *Scheduler) IsRunning(kind string) bool {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	for k, on := range s.running {
		if on && k.String() == kind {
			return true
		}
	}
	return false
}

// RunningKinds 当前正在执行的任务类名（v0.17.4）。
//
// 存在的意义是让「任务在跑」这件事**对前端可见**。此前只能靠台账里的耗时反推，
// 而台账只在**跑完**时才写一笔 —— 于是「点了没反应 / 定时没动静」成了最常见的
// 误判：其实任务正在跑（活跃上报 18 个号要 2 分多钟），只是没有任何地方告诉用户。
// 参考实现的做法也是把运行态暴露出来供调度侧与前端查询（RUNNER.is_running）。
func (s *Scheduler) RunningKinds() []string {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	out := make([]string, 0, len(s.running))
	for k, on := range s.running {
		if on {
			out = append(out, k.String())
		}
	}
	// 排序保证同一状态下输出稳定（否则前端每轮顺序跳动，看起来像在乱变）。
	sort.Strings(out)
	return out
}

// LastAutoRuns 各类任务最近一次**自动触发**（定时/重试）的结果快照（v0.17.4）。
//
// 与 Ledger().Runs() 的差别：后者只保留最近一轮、会被人工触发覆盖；这份只记自动轮次。
// 两者配合，卡片才能同时显示「本轮 · 人工」与「上次定时 01:01 · 17 成功」，
// 把「定时到底有没有在跑」讲清楚。进程重启即清空（内存态，不落盘）。
func (s *Scheduler) LastAutoRuns() map[string]taskledger.Run {
	s.lastAutoMu.Lock()
	defer s.lastAutoMu.Unlock()
	out := make(map[string]taskledger.Run, len(s.lastAuto))
	for k, v := range s.lastAuto {
		out[k.String()] = v
	}
	return out
}

// NextRuns 各类任务的下一次触发时刻（v0.17.4）。
//
// 为什么要暴露它：「定时任务到底有没有生效」这个问题，光看台账答不了 ——
// 台账只记「跑过的最近一轮」。给出「下次 09:03」之后，用户一眼就能确认调度器
// 是活的、并且知道还要等多久。成长任务不排程，故不出现在结果里。
//
// 返回的时刻已含 jitter 偏移（与真正触发时用的是同一个 nextFire 函数），
// 因此显示的时间就是实际会跑的时间，不是名义整点。
func (s *Scheduler) NextRuns() map[string]time.Time {
	now := s.now()
	jit := s.cfg.JitterMinutes
	out := make(map[string]time.Time, 5)
	add := func(k taskKind, hours []int, disabled bool) {
		if disabled {
			return
		}
		if t := nextFire(now, hours, jit); !t.IsZero() {
			out[k.String()] = t
		}
	}
	add(taskCheckin, s.cfg.CheckinHours, s.cfg.CheckinDisabled)
	add(taskTravel, s.cfg.TravelHours, s.cfg.TravelDisabled)
	add(taskActivity, s.cfg.ActivityHours, s.cfg.ActivityDisabled)
	add(taskKeepalive, s.cfg.KeepaliveHours, s.cfg.KeepaliveDisabled)
	add(taskCat, s.cfg.CatHours, s.cfg.CatDisabled)
	return out
}

// jitterOffset 返回名义时点的**确定性**偏移，落在 [0, jitterMinutes) 分钟。
//
// 为什么必须确定性（而不是每次现摇一个随机数）：
// Run 主循环每一轮都重新调用 nextWake。若偏移每次不同，某个槽位触发完再重算时，
// 新摇出的偏移仍可能落在"现在之后"——于是同一个小时被反复派发，任务重复执行。
// 用「日期 | 小时」派生后，同一槽位每次算出的偏移完全一致：触发过的时点
// 不再 After(base)，自然顺延到次日。这同时也是可测的前提。
//
// **为什么任务类不参与散列**（v0.23.3 修正，血泪教训）：
// 早先按「任务类 | 日期 | 小时」派生，本意是"不同任务错开更彻底"。但它把
// nextWake 注释里承诺的「多类任务若配到同一小时，该时刻一并执行」**打散了**——
// 签到与旅行都配 9 点时，两者算出 09:03:59 与 09:03:10 两个不同的唤醒时刻，
// 不再同批。于是先到的那个（旅行，09:03:10）先跑并阻塞 2 分钟，紧随其后的
// 签到槽位（09:03:59）就落在它的执行窗口里被永久越过（旧主循环每轮以"当前时刻"
// 起算，过去的槽位不会再被想起）→ 实测签到连续多天没有自动执行。
// 现在偏移只由「名义时点」决定：同一小时的所有任务落到同一秒、同批派发，
// 任务之间的错开交给 runBatch 的 staggerBatchGap（那才是为此设计的机制）。
//
// 散列用 FNV-1a：标准库自带、无状态、不引 math/rand 全局种子（那会让结果依赖调用
// 顺序）。语义与 internal/server 的 jitterDur（±25% 时长缩放、随机）不同，故不复用。
func jitterOffset(nominal time.Time, jitterMinutes int) time.Duration {
	if jitterMinutes <= 0 {
		return 0 // 0/负数 = 不加偏移（旧行为）
	}
	span := time.Duration(jitterMinutes) * time.Minute
	secs := int64(span / time.Second)
	if secs <= 0 {
		return 0
	}
	h := fnv.New32a()
	// 名义时点用「日期 + 小时」参与散列：同一天同一小时的偏移固定，换一天则变。
	_, _ = fmt.Fprintf(h, "%s", nominal.Format("2006-01-02T15"))
	return time.Duration(int64(h.Sum32())%secs) * time.Second
}

// nextFire 返回 now 之后最近的一个触发时刻；hours 为本地小时（0-23）。
//
// jitterMinutes > 0 时给每个候选时点加上 jitterOffset 的确定性偏移（只按名义时点
// 派生，**不含任务类** —— 同一小时的任务必须落到同一秒，见 jitterOffset 注释），
// 用于把整点齐发的负载摊开。偏移只在**定下日期之后**施加：先算
// 今天的名义时点、加偏移、若已过则改用明天的名义时点重新算偏移——否则跨日时
// 偏移会串到错误的日期上。
func nextFire(now time.Time, hours []int, jitterMinutes int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		// 先试今天，过了再试明天（最多两天足够：明天同一小时必然在 now 之后）。
		for d := 0; d < 2; d++ {
			nominal := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location()).
				AddDate(0, 0, d)
			t := nominal.Add(jitterOffset(nominal, jitterMinutes))
			if !t.After(now) {
				continue
			}
			if earliest.IsZero() || t.Before(earliest) {
				earliest = t
			}
			break
		}
	}
	return earliest
}

// taskKind 调度任务类型。
type taskKind int

const (
	taskCheckin taskKind = iota
	taskTravel
	taskActivity
	taskKeepalive
	taskCat
	taskGrowth
)

// String 返回任务类的稳定名字，用作抖动散列的种子（不要用 iota 数值：数值会随
// 枚举顺序调整而变，名字不会，且日志里可读）。
func (k taskKind) String() string {
	switch k {
	case taskCheckin:
		return "checkin"
	case taskTravel:
		return "travel"
	case taskActivity:
		return "activity"
	case taskKeepalive:
		return "keepalive"
	case taskCat:
		return "cat"
	case taskGrowth:
		return "growth"
	}
	return "unknown"
}

// nextWake 返回 now 之后最近的唤醒时刻，以及该时刻需要执行的全部任务。
// 多类任务若配到同一小时（如签到与旅行都含 9），该时刻多类任务需一并执行。
// 已显式禁用的任务不进候选（nextFire 对其零值返回零时间，nextWake 再跳过零时点）。
func (s *Scheduler) nextWake(now time.Time) (time.Time, []taskKind) {
	type slot struct {
		at   time.Time
		kind taskKind
	}
	var slots []slot
	jit := s.cfg.JitterMinutes
	if !s.cfg.CheckinDisabled {
		slots = append(slots, slot{nextFire(now, s.cfg.CheckinHours, jit), taskCheckin})
	}
	if !s.cfg.TravelDisabled {
		slots = append(slots, slot{nextFire(now, s.cfg.TravelHours, jit), taskTravel})
	}
	if !s.cfg.ActivityDisabled {
		slots = append(slots, slot{nextFire(now, s.cfg.ActivityHours, jit), taskActivity})
	}
	if !s.cfg.KeepaliveDisabled {
		slots = append(slots, slot{nextFire(now, s.cfg.KeepaliveHours, jit), taskKeepalive})
	}
	if !s.cfg.CatDisabled {
		slots = append(slots, slot{nextFire(now, s.cfg.CatHours, jit), taskCat})
	}
	// 成长任务**刻意不在这里排程**：它是纯手动任务（管理员选号执行），
	// 没有触发时刻，故不产生槽位。
	// 当日失败重试：**所有已排期的重试**（含尚未到点的）都参与「下一个唤醒时刻」
	// 的竞争——只算到点的会让未到点的重试失去唤醒源，主循环会一路睡到下一个正常
	// 时点，那次重试要么迟到、要么与正常轮次撞在一起。计划时刻可能已过（进程忙、
	// 刚启动），此时 earliest 落在过去 → Run 的 timer 立即到期，等价于「立刻补跑」。
	// 已禁用的任务不参与（用户关掉了它，不该被一次重试悄悄拉起来）。
	if s.ledger != nil {
		for name, at := range s.ledger.ArmedRetries(now) {
			k, ok := kindByName(name)
			if !ok || s.kindDisabled(k) {
				continue
			}
			slots = append(slots, slot{at, k})
		}
	}
	var earliest time.Time
	for _, sl := range slots {
		if sl.at.IsZero() {
			continue
		}
		if earliest.IsZero() || sl.at.Before(earliest) {
			earliest = sl.at
		}
	}
	if earliest.IsZero() {
		return time.Time{}, nil
	}
	// 去重：同一类任务可能同时出现在普通槽位与已排期的重试里（两者时刻碰巧相等，
	// 例如重试恰好排在某个正常整点上），不去重会让 runBatch 对同一任务并行派发
	// 两次——同一批账号被同时打两遍。
	var kinds []taskKind
	seen := make(map[taskKind]bool, len(slots))
	for _, sl := range slots {
		if sl.at.IsZero() || !sl.at.Equal(earliest) || seen[sl.kind] {
			continue
		}
		seen[sl.kind] = true
		kinds = append(kinds, sl.kind)
	}
	return earliest, kinds
}

// wakeupGraceDelay 迟到唤醒补跑的派发前网络宽限：Windows Modern Standby exit 后
// 网络栈/DNS 1-2s 才恢复（issue #152 实测 dial tcp lookup no such host 与
// Kernel-Power 507 standby exit ≤1s 重合），宽限 5s 覆盖 90%+ 唤醒场景。
// 只对迟到补跑生效（准点触发零延迟），零配置（分析报告裁定全套配置不成比例）。
// 测试可缩短（与 travelAccountDelay「测试可置 0」同口径）。
var wakeupGraceDelay = 5 * time.Second

// wakeupLateThreshold 迟到判定阈值：now 晚于槽位计划时刻超过 1s 才算迟到补跑。
// 毫秒级抖动（timer 正常触发的偏移量级）不算，避免准点触发被误宽限。
const wakeupLateThreshold = 1 * time.Second

// awaitWakeupGrace 迟到唤醒补跑派发前的网络宽限：槽位时刻已过点超过阈值
// （机器刚从睡眠唤醒）时先等满 wakeupGraceDelay 让网络栈/DNS 就绪再派发。
// 准点/阈值内抖动零延迟直接放行。ctx 取消立即返回 false（优雅停机不等宽限睡满，
// 本批放弃，下轮 nextWake 照旧从"现在"起算）。返回是否继续派发。
func awaitWakeupGrace(ctx context.Context, planned time.Time) bool {
	if late := time.Since(planned); late <= wakeupLateThreshold {
		return ctx.Err() == nil // 准点触发：零延迟放行
	}
	log.Printf("wakeup grace %s: late catch-up for slot %s", wakeupGraceDelay, planned.Format("15:04"))
	return sleepCtx(ctx, wakeupGraceDelay)
}

// Run 主循环，阻塞直到 ctx 取消。
//
// base 是「已经排到哪一刻」的水位线（v0.23.3）。用它而不是 time.Now() 作为
// nextWake 的基准，是本轮修复的核心 —— 原先每轮都以"当前时刻"起算，于是**本批
// 执行期间越过的槽位会被永久跳过**：nextWake 只返回 now 之后的最近时点，那些已过
// 的槽位既不返回、也不会在后续轮次被想起，直接顺延到下一个名义小时。
//
// 实测（当时抖动还按任务类派生）：签到与旅行同配 9 点，落成 09:03:59 / 09:03:10
// 两个唤醒时刻。先到的旅行 09:03:10 开始跑、09:05:00 结束，紧随其后的签到槽位
// 09:03:59 正好落在它的执行窗口里 —— 旧逻辑下这一轮签到就此消失，台账里 signin
// 连续多天停在很久以前的一次人工触发上。
//
// 改为「以上一批的槽位时刻为基准」后：被越过的槽位在下一轮立刻被算出，
// time.Until(next) 为负 → timer 立即到期 → 即刻补跑，不漏也不重复
// （因为基准推进到 next 之后，同一个槽位不会被返回第二次）。
func (s *Scheduler) Run(ctx context.Context) {
	base := s.now()
	for {
		next, kinds, nextBase := s.planNext(base)
		if next.IsZero() {
			// 六类任务全部禁用：不空转，只等退出信号。
			<-ctx.Done()
			return
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			// 到点任务在排程时确定（不依赖唤醒时刻的小时数），迟到唤醒也不会漏跑。
			// 迟到唤醒（睡眠跨过槽位时刻，timer 在唤醒瞬间才到期）先等网络宽限：
			// 唤醒瞬间 DNS 未就绪，零宽限派发等于把唯一一次补跑机会打在注定失败
			// 的窗口里（issue #152）；准点触发零延迟不受影响。
			if !awaitWakeupGrace(ctx, next) {
				return // ctx 取消：放弃本批，优雅退出
			}
			// 唤醒时全部并行派发：每类一个 goroutine，慢任务族（如活跃上报
			// 54 号 × 5 条 ≈ 7-8 分钟睡眠）不再阻塞同槽其他任务族；返回前
			// 等全部任务收尾（下一轮 nextWake 照旧从"现在"起算，多轮重叠
			// 的风险与串行版相同——nextWake 只挑现在之后的时点）。
			s.runBatch(ctx, kinds)
			// 基准推进到本批槽位之后（而不是"现在"）：这样本批执行期间越过的
			// 槽位在下一轮被算出来（落在过去 → timer 立即到期 → 补跑），
			// 已经派发过的这一批不会被重复返回。见 planNext 的说明。
			base = nextBase
		}
	}
}

// planNext 主循环一轮的全部决策：算下一批槽位（at）、该批要跑的任务（kinds），
// 以及**推进后的基准水位线**（nextBase）。
//
// 为什么单独提出来：Run 是无限循环、依赖真实时钟，单测跑不了；而"基准取谁"
// 正是 v0.23.3 修复的关键 —— 把它变成可测面，才能用测试钉死，避免被后人改回
// time.Now() 而无人发现（那会让被越过的槽位再次静默消失）。
//
// **调用方必须用返回值 nextBase 推进下一轮的 base**（而不是自己写 time.Now()）。
func (s *Scheduler) planNext(base time.Time) (at time.Time, kinds []taskKind, nextBase time.Time) {
	at, kinds = s.nextWake(base)
	if at.IsZero() {
		return time.Time{}, nil, base
	}
	return at, kinds, at.Add(time.Nanosecond)
}

// runBatch 并行派发一批任务（同一唤醒时刻的多类任务），等全部完成返回。
// 供 Run 主循环与测试使用；ctx 取消时由各任务内部的 sleepCtx 快速收尾。
func (s *Scheduler) runBatch(ctx context.Context, kinds []taskKind) {
	var wg sync.WaitGroup
	for i, k := range kinds {
		// 任务间错峰（v0.17.3）：同一唤醒时刻常常派发多类任务（如签到与旅行都配了 9 点），
		// 而它们遍历的是**同一批账号** —— 不加间隔时，一个账号会在几十秒内被
		// 签到 → 旅行 → 上报 连着打好几轮，这是比「单任务高频」更明显的机器特征。
		//
		// 关键是**在执行层拉开而不是给排程时刻加随机偏移**：后者会随每次调度逐日累积
		// 漂移（偏移量叠加到下一次的名义时刻上），几个月后执行时刻绕时钟转一圈。
		if i > 0 && !s.staggerBatchGap(ctx) {
			return // ctx 取消：放弃本批剩余任务，下轮 nextWake 照旧从"现在"起算
		}
		wg.Add(1)
		go func(k taskKind) {
			defer wg.Done()
			s.dispatch(ctx, k)
		}(k)
	}
	wg.Wait()
}

// staggerBatchGap 批内任务之间的随机等待；返回 false 表示 ctx 被取消。
// 未配置（<=0）时零等待，行为与引入前逐字一致。
func (s *Scheduler) staggerBatchGap(ctx context.Context) bool {
	lo, hi := s.batchStaggerMin(), s.batchStaggerMax()
	if hi <= 0 {
		return ctx.Err() == nil
	}
	d := time.Duration(lo) * time.Second
	if hi > lo {
		d = time.Duration(lo+rand.Intn(hi-lo+1)) * time.Second
	}
	log.Printf("batch stagger: 等 %s 再派发下一个任务（避免同批任务在同一账号上连打）", d.Round(time.Second))
	return sleepCtx(ctx, d)
}

func (s *Scheduler) batchStaggerMin() int {
	if v := s.cfg.BatchStaggerMinSeconds; v > 0 {
		return v
	}
	if s.cfg.BatchStaggerMaxSeconds > 0 {
		return defaultBatchStaggerMinSec
	}
	return 0
}

func (s *Scheduler) batchStaggerMax() int {
	if v := s.cfg.BatchStaggerMaxSeconds; v > 0 {
		return v
	}
	return 0
}

// dispatch 按任务类型分发到对应执行函数。脚本类（cat）失败只记 WARN、
// 不影响其余任务继续执行（与现有各任务"单账号失败不阻断遍历"同口径）。
// ctx 传导给带账号间限速的遍历（取消时立即放弃剩余账号），纯脚本类任务不感知。
//
// 本轮若是某个已到点的当日失败重试，先把触发来源判成 retry 并消费掉该重试；
// 否则就是正常排程轮次（ledger.go 的 takeRetryTrigger，判据与 nextWake 同源）。
func (s *Scheduler) dispatch(ctx context.Context, k taskKind) {
	trigger := triggerSchedule
	if s.takeRetryTrigger(k, time.Now()) {
		trigger = triggerRetry
	}
	switch k {
	case taskCheckin:
		s.runCheckin(trigger)
	case taskTravel:
		s.runTravel(ctx, trigger)
	case taskActivity:
		s.runActivity(ctx, trigger)
	case taskKeepalive:
		s.runKeepalive(trigger)
	case taskCat:
		s.runCat(trigger)
	case taskGrowth:
		// 不可达：成长任务不排程（无槽位），仅由 RunGrowthFor 手动驱动。
		log.Printf("growth: 收到自动触发，已忽略（该任务为纯手动）")
	}
}

// RunCheckinNow 立即签到的人工入口（/admin/tasks/checkin/run、一次性工具、测试）。
// 与定时轮次的差别只在台账的 trigger 标记：人工触发的失败不排当日重试
// （见 recordRun 的注释）。
//
// 需要逐账号明细（管理页要展示哪个号签到成功、额度多少）时用
// RunCheckinNowDetailed——CheckinAll 不做记账，直接调它会让手动触发的轮次
// 从台账里消失。
func (s *Scheduler) RunCheckinNow() { s.runCheckin(triggerManual) }

// RunCheckinNowDetailed 人工签到并回传逐账号结果，同时照常记台账。
//
// 与 RunCheckinNow 走同一条 runCheckin 路径（同一份记账与重试判定逻辑），
// 仅多回传明细。存在的理由：CheckinAll 是"执行"而非"执行+记账"，
// 若外部直接调它取明细，这一轮就只跑不记，管理面板看不到。
func (s *Scheduler) RunCheckinNowDetailed() ([]CheckinOutcome, error) {
	return s.runCheckin(triggerManual)
}

// runCheckin 执行一轮全量签到并记台账。
//
// 计数直接来自 CheckinAll 的逐账号结果（不另做推断）：ErrBusy（撞车）时
// out 为空 → total=0、fail=0 → 不判全灭、不排重试，这是对的——「与另一次签到
// 撞车」不是失败。
func (s *Scheduler) runCheckin(trigger string) ([]CheckinOutcome, error) {
	started := time.Now()
	out, err := s.CheckinAll()
	if err != nil {
		log.Printf("scheduled checkin skipped: %v", err)
	}
	var t runTally
	t.total = len(out)
	for _, oc := range out {
		switch oc.Status {
		case CheckinOK:
			t.ok++
		case CheckinAlready:
			t.already++
		case CheckinSkipped:
			t.skipped++
		case CheckinFail:
			t.fail++
		}
	}
	s.recordRun(taskCheckin, trigger, started, t)
	return out, err
}

// CheckinAll 全量签到：按需刷新 token → daily-checkin → 查余额 → 解冻冷却账号。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用的跳过。
// 同一时刻只允许一次签到在跑，重复调用返回 ErrBusy（防止手动触发与定时撞车重复打上游）。
//
// session dead 走 Pool.NoteSessionDead 的**连续计数**语义（与 keepalive 一致）：
// 一次刷新失败不再立即杀号，连续 sessionDeadThreshold 次才禁用，刷新成功清计数。
func (s *Scheduler) CheckinAll() ([]CheckinOutcome, error) {
	return s.checkinFiltered(nil)
}

// CheckinUIDs 只对指定账号签到（新入池账号补跑，v0.23.3）。
//
// 为什么需要它：账号在**当天触发点之后**入池（导入 / 扫码 / 代挂审批）时，当天的
// 签到轮次已经过去，要等到次日第一个触发点才会被排到 —— 最长 ~21 小时。而签到正是
// 新号拿到当日额度的那一步，没额度既不能参与平台消耗，入池门槛（积分）也拿不到
// 权威读数。这里补一次，让新号当天就进入正常状态。
//
// 与 CheckinAll 的关系：**同一条执行路径、同一把互斥锁**（checkinMu），只是把遍历
// 范围收窄到给定 uid；并发时返回 ErrBusy，与既有语义一致。
//
// **刻意不记台账**（与 CheckinAll 一致）：补跑通常只覆盖一两个新号，若记进台账的
// 「最近一轮」，就会把一次完整轮次的记录覆盖成「共 2 个」，管理员会误以为定时任务
// 这轮只跑了 2 个号。补跑的结果体现为账号卡上的「今日已签到」（该状态实时查上游）。
func (s *Scheduler) CheckinUIDs(uids []string) ([]CheckinOutcome, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	only := make(map[string]bool, len(uids))
	for _, u := range uids {
		if u != "" {
			only[u] = true
		}
	}
	if len(only) == 0 {
		return nil, nil
	}
	return s.checkinFiltered(only)
}

// checkinFiltered 签到执行本体。only 为 nil = 全量；非 nil = 只跑集合内的 uid。
func (s *Scheduler) checkinFiltered(only map[string]bool) ([]CheckinOutcome, error) {
	if !s.checkinMu.TryLock() {
		return nil, ErrBusy
	}
	defer s.checkinMu.Unlock()

	statuses := s.cfg.Pool.List()
	out := make([]CheckinOutcome, 0, len(statuses))
	var okN, alreadyN, failN, skipN int
	for _, st := range statuses {
		if only != nil && !only[st.UID] {
			continue
		}
		oc := CheckinOutcome{UID: st.UID, Nickname: st.Nickname}
		if st.Disabled {
			oc.Status, oc.Detail = CheckinSkipped, "disabled"
			skipN++
			out = append(out, oc)
			continue
		}
		if !s.taskAllowed(st.UID, taskCheckin.String()) {
			oc.Status, oc.Detail = CheckinSkipped, "task off"
			skipN++
			out = append(out, oc)
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			oc.Status, oc.Detail = CheckinSkipped, "no credentials"
			skipN++
			out = append(out, oc)
			continue
		}
		// D4 门控：realm=global 账号无签到体系/任务中心，直接跳过（不发起任何上游调用，避免风控）。
		// 经 auth.Realm() 统一判定：逃生门（global.enabled=false）下 global 账号被降级为 cn、
		// 按 CN 处理——这是 D5 逃生门的刻意语义（纯 CN 部署锁死一切 global），与引用处一致。
		if a.IsGlobal() {
			oc.Status, oc.Detail = CheckinSkipped, "global"
			skipN++
			out = append(out, oc)
			continue
		}
		// 停机跨过 token 有效期（关机过夜/容器长期停跑）时先补一次刷新，否则签到必然 401 白跑。
		if a.NeedsRefresh(checkinRefreshSkew) {
			if err := s.cfg.Upstream.RefreshToken(a); err != nil {
				log.Printf("checkin %s refresh: %v", logfmt.Label(st.UID, st.Nickname), err)
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					s.cfg.Pool.CooldownUntilTomorrow4AM(st.UID, "12153 会话失效（需重新登录）")
					if s.cfg.OnTokenDead != nil {
						s.cfg.OnTokenDead(st.UID, st.Nickname, "refresh 12153（refresh token 已失效）")
					}
					if s.cfg.Pool.NoteSessionDead(st.UID) {
						log.Printf("WARN: checkin %s: 连续 %d 次 12153 session dead — 禁用", logfmt.Label(st.UID, st.Nickname), pool.SessionDeadThreshold())
					}
				}
				// 刷新只是"提前补票"：token 若仍有效，继续照常签到（否则刷新接口抖动
				// 会让本可成功的签到被白白跳过）；真正过期才判定失败。
				if a.NeedsRefresh(0) {
					oc.Status, oc.Detail = CheckinFail, "refresh: "+err.Error()
					failN++
					out = append(out, oc)
					continue
				}
			} else {
				a.BackfillRealm() // 老 auth 空 realm → 落盘前补标识（幂等：已有不动）
				if err := a.SaveAtomic(); err != nil {
					// 刷新成功但落盘失败：重启会用旧 token，必须暴露。
					log.Printf("checkin %s save: %v", logfmt.Label(st.UID, st.Nickname), err)
				}
			}
		}
		// 签到返回错误（含"今天已签到"）也继续查余额：余额恢复即可解冻账号。
		if err := s.cfg.Upstream.DailyCheckin(a); err != nil {
			if upstream.IsAlreadyCheckin(err) {
				// "今天已签到"是幂等成功，不是错误：不填 detail，免得回执里
				// 出现一整段 400 报文、被误读成签到失败。
				oc.Status = CheckinAlready
			} else {
				oc.Status = CheckinFail
				oc.Detail = err.Error()
				log.Printf("checkin %s: %v", logfmt.Label(st.UID, st.Nickname), err)
			}
		} else {
			oc.Status = CheckinOK
		}
		// 分桶查余额：快过期窗口内的积分单独标记，pool 优先消耗（issue:积分过期）。
		// ExpiringSoonWindow<=0 时退化为纯总量（与引入前一致）。
		remain, buckets, err := s.cfg.Upstream.UserResourceDetailed(a, s.cfg.ExpiringSoonWindow)
		if err != nil {
			log.Printf("user-resource %s: %v", logfmt.Label(st.UID, st.Nickname), err)
			oc.Status = CheckinFail
			oc.Detail = joinDetail(oc.Detail, "resource: "+err.Error())
			failN++
			out = append(out, oc)
			continue
		}
		s.cfg.Pool.ReenableIfCredits(st.UID, remain)
		s.cfg.Pool.SetCreditsDetailed(st.UID, remain, buckets.Expiring)
		oc.Credits = &remain
		switch oc.Status {
		case CheckinOK:
			okN++
		case CheckinAlready:
			alreadyN++
		default:
			failN++
		}
		out = append(out, oc)
	}
	log.Printf("checkin done: total=%d ok=%d already=%d fail=%d skipped=%d",
		len(statuses), okN, alreadyN, failN, skipN)
	return out, nil
}

// joinDetail 拼接多段原因，避免后一段覆盖前一段的失败信息。
func joinDetail(existing, add string) string {
	if existing == "" {
		return add
	}
	return existing + "; " + add
}

// RunActivityNow 立即对池内所有可用账号执行对话活跃上报（无 ctx 的外部入口：
// cmd/activity 一次性触发、测试）。内部走 runActivity，取背景 ctx（不可取消，
// 语义与引入前 time.Sleep 版一致）。
func (s *Scheduler) RunActivityNow() {
	s.runActivity(context.Background(), triggerManual)
}

// RunActivityFor 与 RunActivityNow 同义，但接受外部 ctx（带超时的后台执行用）。
// 活跃上报是最慢的一类（18 号 × 5 条约 2~3 分钟），手动触发改异步后必须带超时。
func (s *Scheduler) RunActivityFor(ctx context.Context) {
	s.runActivity(ctx, triggerManual)
}

// runActivity 活跃上报遍历，随 ctx 取消立即退出。
//
// 计数口径（写台账用）：Total 为参与遍历的账号数；一个号把 N 条全发满记 OK，
// 中途报错记 Fail；禁用号与无 AccessToken 的号记 Skipped。
// 被 ctx 取消打断时把本轮标为 interrupted —— 此时计数是部分的，不判全灭。
func (s *Scheduler) runActivity(ctx context.Context, trigger string) {
	started := time.Now()
	var t runTally
	defer func() { s.recordRun(taskActivity, trigger, started, t) }()

	// 运行闸门（手动 vs 定时）——活跃上报每号 5 条，是最耗上游的一类，撞车代价最大。
	if s.skipIfBusy(taskActivity, trigger) {
		t.skipped++
		return
	}
	defer s.endRun(taskActivity)

	count := s.cfg.ActivityReportCount
	first := true
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			t.skipped++
			continue
		}
		if !s.taskAllowed(st.UID, taskActivity.String()) {
			t.skipped++
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessTokenValue() == "" {
			t.skipped++
			continue
		}
		// global 账号同样上报（PR #45 实测国际版 /v2/report 在 workbuddy.ai 上 code=0 OK，
		// 点亮连登）；realmBase 路由/头由 upstream.billingJSON/BillingHeaders 按 realm 切。
		// 单账号失败只记 WARN 不影响遍历（下方 report err → break 该号 → continue 下号）。
		if !first {
			if !sleepCtx(ctx, activityAccountDelay) {
				t.interrupted = true
				return // 优雅停机：不等限速睡满，剩余账号下轮再报
			}
		}
		first = false
		t.total++
		// N 条共用同一 conversationId（同会话），requestId 各自独立（每条一个）。
		cid := fmt.Sprintf("wb2api-%d", time.Now().UnixMilli())
		ok := 0
		for i := 1; i <= count; i++ {
			rid := fmt.Sprintf("%s-r%d", cid, i)
			if err := s.cfg.Upstream.ReportChatActivity(a, cid, rid); err != nil {
				log.Printf("activity %s: report %d/%d: %v", logfmt.Label(a.UID, a.Nickname), i, count, err)
				break // 本号上报失败：不再续发，streak 自检无意义
			}
			log.Printf("activity %s: report %d/%d ok", logfmt.Label(a.UID, a.Nickname), i, count)
			ok++
			if i < count {
				// 账号内 5 条之间间隔，避免秒发风控；取消时立即放弃本号剩余条数。
				if !sleepCtx(ctx, activityReportGap) {
					t.interrupted = true
					return
				}
			}
		}
		if ok < count {
			t.fail++
			continue // N 条未发满：streak 自检与领养均无意义，下个账号
		}
		t.ok++
		s.checkActivityStreak(a) // N 条全发满 → 回读 streak 自检（只留结论行）
		s.travelAdoptForce(a)    // 无猫账号对话量刚补满 → 立即重试领养（豁免防抖）
		s.claimGrowthRewards(a)  // 连登奖励 + 抽奖：点亮连登后按天领取（finally 语义：失败不拖累上报）
	}
}

// checkActivityStreak 上报成功后回读连登天数（只读 oracle，发现静默失败）。
// 背景：REPORT-active-map.md §2 实测「上报 200 但静默丢弃」（缺 userId 时 progress 不动），
// 上报 200 ≠ streak 计分——需要回读验证闭环。
// 异常检测口径：days==0 → warn（report OK but streak.days=0 (silent drop?)）；
// GET 失败 → warn 但不影响主流程（上报本身已成功，按天幂等，不做重试）。
// 日志每号一行、一眼可 grep：`activity %s: streak days=%d`（成功也打，方便对账）。
// 返回 true 表示「上报 OK 但 streak 可疑」（days==0 或回读失败），供测试断言。
func (s *Scheduler) checkActivityStreak(a *auth.Auth) bool {
	days, err := s.cfg.Upstream.GrowthStreak(a)
	if err != nil {
		log.Printf("WARN: activity %s: streak check failed (report OK): %v", logfmt.Label(a.UID, a.Nickname), err)
		return true
	}
	if days == 0 {
		log.Printf("WARN: activity %s: report OK but streak.days=0 (silent drop?)", logfmt.Label(a.UID, a.Nickname))
		return true
	}
	log.Printf("activity %s: streak days=%d", logfmt.Label(a.UID, a.Nickname), days)
	return false
}

// claimGrowthRewards 领取连登奖励（里程碑兑换）+ 执行连登抽奖。
// 在 runActivity 上报成功 + streak 自检之后调用：连登达标（days>=某档）才能领奖，
// 领奖送的 chances 才是抽奖次数来源，故先领奖后抽奖。
//
// 全链（panel 连登管家吸收版）：礼包/补偿领取 → 读 reward-state → 补签保连登
// （补签成功重读 state 吃恢复后的天数）→ 挑档 redeem → chances → draw。
//
// 幂等/风控语义（与现有 travel/travel 同口径：单号失败只该号 WARN，不影响其他账号）：
//   - 按天幂等：每日每号最多领一轮（rewardClaimedToday 闸；自然日 CST 重置，进程重启清零——
//     重启后当日重复 redeem 由上游 409 duplicate 正常态兜底，不刷 WARN）。
//   - 服务端正常态识别为静默跳过：redeem 409 duplicate/403 天数不足；lottery 400 无次数/未开启——
//     这些不是失败，不刷 WARN（见 upstream.IsRedeemAlreadyClaimed 等）。
//   - 领奖只领「本次新达标」的档位：状态非 claimed 且 days>=档位天数。跨档连领（14d 未领而
//     days 已到 28）是官方正常态（spa 按 byTier 逐档可兑），但每日一轮限一档，避免同日多写。
//   - 礼包/补偿是「有则领」的幂等写，业务错误静默；补签只在「昨日漏签且有卡」时触发，
//     无卡/无漏签不写。
//
// 日志每号一行可 grep：`activity %s: redeem tier=%s ...` / `activity %s: lottery ...`。
func (s *Scheduler) claimGrowthRewards(a *auth.Auth) {
	if a == nil || a.AccessTokenValue() == "" {
		return
	}
	// global 门控：连登奖励/抽奖链只服务 CN。国际版 /activity/growth/* 端点虽同构存在
	// （/tmp/analysis-global-credit.md §1.1：lottery/streak/redeem 在国际版上线），但真实
	// global 新账号 GET /activity/growth/streak 返回 500（实测 sliverkiss）——链上第一步就
	// 拿不到 days，无法挑档；且 streak 500 会每趟刷 WARN 污染日志。结论：证据不足，跳过
	// global（不发起任何领取类调用）。CN 账号无此问题（CN streak 200 days=N）。
	if a.IsGlobal() {
		return
	}
	if s.rewardClaimedToday(a.UID) {
		return // 当日已领过一轮，跳过（按天幂等）
	}
	// 0. 礼包/补偿领取（panel 连登管家口径）：每号一次 / 有则领的幂等写，
	// 业务错误静默跳过（不刷 WARN），先于 redeem——到账积分不依赖连登状态。
	s.claimGrowthBonus(a)
	state, err := s.cfg.Upstream.GrowthRewardState(a)
	if err != nil {
		log.Printf("WARN: activity %s: reward-state: %v", logfmt.Label(a.UID, a.Nickname), err)
		return
	}
	// 0.5 补签保连登（panel makeupYesterday 口径）：昨日漏签（heatmap score==0）且
	// 有补签卡 → 补昨日。放在 reward-state 之后：若补签把 streak 恢复到新档位，
	// 重读 state 让本日 redeem 直接吃到恢复后的天数（补签是保 7d/14d/28d 里程碑的关键）。
	if s.makeupYesterday(a) {
		if st2, err2 := s.cfg.Upstream.GrowthRewardState(a); err2 == nil {
			state = st2 // 补签成功 → 用恢复后的天数挑档
		}
	}
	days := state.Days()
	tier := growthEligibleTier(days, &state.Redemption)
	if tier == "" {
		// 无新达标档位：不动写接口（不刷 WARN，这是正常态——很多天没到 7d）。
		return
	}
	res, err := s.cfg.Upstream.GrowthRedeem(a, tier, "")
	switch {
	case err == nil:
		log.Printf("activity %s: redeem tier=%s ok (+%d credit, +%d energy, +%d chances)",
			logfmt.Label(a.UID, a.Nickname), tier, res.CreditGranted, res.EnergyGranted, res.ChancesGranted)
	case upstream.IsRedeemAlreadyClaimed(err) || upstream.IsRedeemNotEnoughDays(err):
		log.Printf("activity %s: redeem tier=%s skip (already claimed or days not enough)", logfmt.Label(a.UID, a.Nickname), tier)
	default:
		log.Printf("activity %s: redeem tier=%s: %v", logfmt.Label(a.UID, a.Nickname), tier, err)
	}
	// 标记当日已处理（无论 redeem 是否成功都记一次：领取类各状态当日不再重试，
	// 避免对上游重复写；成功→无需再领，失败→当日不轰炸，次日自然日重置/上游幂等兜底）。
	s.markRewardClaimed(a.UID)
	s.claimGrowthLottery(a)
}

// growthEligibleTier 按当前连登天数挑选「尚未领取且达标」的最高档位。
// 返回 "" 表示无可领档（未达标或全部已领），调用方据此跳过 redeem（正常态）。
func growthEligibleTier(days int, rs *upstream.GrowthRedemptionStatus) string {
	if rs == nil {
		return ""
	}
	// 档位按 days 升序，从高到低挑最高的已达标未领档（一次领一份，每日一轮）。
	for i := len(rs.Tiers) - 1; i >= 0; i-- {
		sp := rs.Tiers[i]
		if days >= sp.Days && !rs.Claimed(sp.Tier) {
			return sp.Tier
		}
	}
	return ""
}

// claimGrowthBonus 新手礼包 + 活动补偿领取（panel 连登管家口径）。
// 两者都是幂等写：礼包每号一次（已领业务错误静默）、补偿有则领（无则业务错误静默）。
// 只在成功到账时打日志（每号一生一次的事件，不值得每日刷行）；失败静默——
// 业务错误是常态（绝大多数号早已领过），无法与真错误可靠区分，不刷 WARN。
func (s *Scheduler) claimGrowthBonus(a *auth.Auth) {
	if credit, err := s.cfg.Upstream.ClaimGift(a); err == nil && credit > 0 {
		log.Printf("activity %s: gift ok (+%d credit)", logfmt.Label(a.UID, a.Nickname), credit)
	}
	if credit, err := s.cfg.Upstream.ClaimCompensation(a); err == nil && credit > 0 {
		log.Printf("activity %s: compensation ok (+%d credit)", logfmt.Label(a.UID, a.Nickname), credit)
	}
}

// makeupYesterday 昨日漏签且有补签卡时自动补签（保住连登连续天数，panel 口径）。
// 连续天数一断就要重攒 7 天，一张卡代价远小——有漏签 + 有卡即补。
// 判据链：heatmap 昨日格 score==0（漏签）→ streak.makeup_cards.balance>0（有卡）
// → POST makeup-cards/use {"target_date":昨日}。
// 无卡 / 无漏签 / 无该日格 / 查询失败均静默返回 false（不影响主流程）；
// 补签成功打一行日志并返回 true（调用方重读 streak 天数挑档）。
func (s *Scheduler) makeupYesterday(a *auth.Auth) bool {
	cells, err := s.cfg.Upstream.GrowthHeatmap(a)
	if err != nil {
		return false // 只读判据失败：静默（每日重试，无写风险）
	}
	yesterday := upstream.GrowthYesterdayDate(time.Now())
	score, ok := upstream.HeatmapDayScore(cells, yesterday)
	if !ok || score != 0 {
		return false // 昨日有分或无判据：无需补签
	}
	// 有漏签 → 查补签卡余额（streak 端点同一响应体）。
	st, err := s.cfg.Upstream.GrowthStreakWithCards(a)
	if err != nil || st.MakeupCards.Balance <= 0 {
		return false // 无卡或查询失败：静默（次日再判）
	}
	if err := s.cfg.Upstream.UseMakeupCard(a, yesterday); err != nil {
		log.Printf("activity %s: makeup %s: %v", logfmt.Label(a.UID, a.Nickname), yesterday, err)
		return false
	}
	log.Printf("activity %s: makeup ok %s (+streak kept)", logfmt.Label(a.UID, a.Nickname), yesterday)
	return true
}

// claimGrowthLottery 消耗连登奖励赠与的抽奖次数。仅抽 balance>0 的次数；无次数跳过
// （400 insufficient 正常态静默）；抽奖未开启（400 lottery disabled）静默。
// client_token 每次 draw 必须新键（security-relevant，见 upstream.GrowthLotteryDraw）。
func (s *Scheduler) claimGrowthLottery(a *auth.Auth) {
	chances, err := s.cfg.Upstream.GrowthLotteryChances(a)
	if err != nil {
		log.Printf("WARN: activity %s: lottery-chances: %v", logfmt.Label(a.UID, a.Nickname), err)
		return
	}
	if chances <= 0 {
		log.Printf("activity %s: lottery skip (no chances)", logfmt.Label(a.UID, a.Nickname))
		return
	}
	res, err := s.cfg.Upstream.GrowthLotteryDraw(a, "") // 每次自动新 client_token
	switch {
	case err == nil:
		log.Printf("activity %s: lottery drawn prize=%s (%s)", logfmt.Label(a.UID, a.Nickname), res.PrizeName, res.PrizeType)
	case upstream.IsLotteryNoChance(err) || upstream.IsLotteryDisabled(err):
		log.Printf("activity %s: lottery skip (no chances or disabled)", logfmt.Label(a.UID, a.Nickname))
	default:
		log.Printf("activity %s: lottery draw: %v", logfmt.Label(a.UID, a.Nickname), err)
	}
}

// rewardClaimedToday 该账号当日是否已处理过连登奖励领取（自然日 CST）。
func (s *Scheduler) rewardClaimedToday(uid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rewardClaimed[uid] == travelDay(time.Now())
}

// markRewardClaimed 记录该账号当日已处理连登奖励领取。
func (s *Scheduler) markRewardClaimed(uid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rewardClaimed[uid] = travelDay(time.Now())
}

// RunKeepaliveNow 立即对所有账号刷新 token 的人工入口（/admin/tasks/keepalive/run、测试）。
func (s *Scheduler) RunKeepaliveNow() { s.runKeepalive(triggerManual) }

// runKeepalive 对所有账号刷新 token；session 死亡的自动禁用。
// 12153 禁用走 Pool.NoteSessionDead 的**连续计数**语义：一次刷新失败不再立即杀号，
// 连续 sessionDeadThreshold 次（3 次）才禁用（P0-1：13 个 disabled 号全是历史误判）。
// 刷新成功 → ClearSessionDead 清计数（错误判定的账号有复活路径）。
//
// 计数口径：刷新成功记 OK，刷新报错记 Fail（含 12153）；禁用号与无 refresh token
// 的号记 Skipped。落盘失败不计 Fail——token 已经刷到手，重试救不了磁盘。
func (s *Scheduler) runKeepalive(trigger string) {
	started := time.Now()
	var t runTally

	// 运行闸门（手动 vs 定时）：保活会刷新 token 并写回库，撞车可能出现两个
	// 刷新流程并发写同一账号的凭证。注意本函数的 recordRun 在函数末尾（非 defer），
	// 故撞车分支必须自己记一笔，否则这次让路在台账里不可见。
	if s.skipIfBusy(taskKeepalive, trigger) {
		t.skipped++
		s.recordRun(taskKeepalive, trigger, started, t)
		return
	}
	defer s.endRun(taskKeepalive)

	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			t.skipped++
			continue
		}
		if !s.taskAllowed(st.UID, taskKeepalive.String()) {
			t.skipped++
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			t.skipped++
			continue
		}
		// 惰性刷新：token 还剩很久才过期时跳过（保活阈值/惰性刷新窗口，0=关闭该判据）。
		if s.keepaliveSkip(a) {
			t.skipped++
			continue
		}
		t.total++
		// 抖动（v0.31.29）：即使同一小时轮到多个号，也不要打成同一秒的连发。
		// 20 个号 × 0.15~0.85s ≈ 2~17 秒，仍然很快，但对上游不再是"一串并发"。
		if s.cfg.KeepaliveJitterMs > 0 {
			time.Sleep(time.Duration(rand.Intn(s.cfg.KeepaliveJitterMs)+50) * time.Millisecond)
		}
		if err := s.cfg.Upstream.RefreshToken(a); err != nil {
			t.fail++
			log.Printf("keepalive %s: %v", logfmt.Label(st.UID, st.Nickname), err)
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				// 首次即硬冷却至次日 04:00（v0.31.30）：刷新都失效了，再被选中也服务不了。
				s.cfg.Pool.CooldownUntilTomorrow4AM(st.UID, "12153 会话失效（需重新登录）")
				if s.cfg.OnTokenDead != nil {
					s.cfg.OnTokenDead(st.UID, st.Nickname, "refresh 12153（refresh token 已失效）")
				}
				if s.cfg.Pool.NoteSessionDead(st.UID) {
					log.Printf("WARN: keepalive %s: 连续 %d 次 12153 session dead — 禁用", logfmt.Label(st.UID, st.Nickname), pool.SessionDeadThreshold())
				}
			}
			continue
		}
		t.ok++
		s.cfg.Pool.ClearSessionDead(st.UID) // 刷新成功清误判计数，失败不该累计
		a.BackfillRealm()                   // 老 auth 空 realm → 落盘前补标识（幂等：已有不动）
		if err := a.SaveAtomic(); err != nil {
			log.Printf("keepalive %s save: %v", logfmt.Label(st.UID, st.Nickname), err)
		}
	}
	s.recordRun(taskKeepalive, trigger, started, t)
}

// keepaliveSkip 惰性刷新判定：token 剩余有效期还很长（超出保活阈值/惰性刷新窗口）时跳过本轮刷新，
// 减少对上游的无谓调用（防封），也让「保活」只在真正需要续期时才动。
// 两个判据都为 0（缺省）时恒返回 false —— 行为与引入前逐字一致（无条件刷新）。
// keepaliveSpreadGraceHours 分散刷新的兜底窗口：token 在这么多小时内要过期时，
// 不再等"自己那一小时"，立刻刷（分散是为了好看，不是为了把号刷废）。
const keepaliveSpreadGraceHours = 6

// keepaliveHourOf 该账号的"专属刷新小时"（0-23）。
//
// 为什么要按账号分散（v0.31.29，2026-10-18 全池掉登录换来的）：
// 原先保活每天 22 点跑一次，38 个号在**13 秒内**被逐个刷新（实测 iat 22:01:02~22:01:15，
// 同一出口 IP）—— 对上游来说这是一眼可见的批量行为，与"一个正常用户偶尔续期"完全不同。
// 把每个号固定到一天里的某一小时，上游任何时刻最多看到 1~2 次刷新。
func keepaliveHourOf(uid string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(uid))
	return int(h.Sum32() % 24)
}

// keepaliveSpreadOn 调度是否真的覆盖了全天 24 小时。
//
// 只在覆盖 24 小时时启用分散：如果仍是一天一次（KeepaliveHours=[22]），
// 分散会让绝大多数号永远轮不到自己那一小时 —— 那是把功能关掉而不是优化。
func (s *Scheduler) keepaliveSpreadOn() bool {
	seen := map[int]bool{}
	for _, h := range s.cfg.KeepaliveHours {
		if h >= 0 && h < 24 {
			seen[h] = true
		}
	}
	return len(seen) >= 24
}

func (s *Scheduler) keepaliveSkip(a *auth.Auth) bool {
	// 分散窗口（先于"还很久才过期"的判断）：不是自己那一小时就跳过，
	// 除非 token 已临近过期（见 keepaliveSpreadGraceHours）。
	if s.keepaliveSpreadOn() {
		now := s.now()
		if now.Hour() != keepaliveHourOf(a.UID) {
			exp := a.ExpiresAt
			if exp <= 0 || exp > now.Unix()+keepaliveSpreadGraceHours*3600 {
				return true
			}
		}
	}
	if s.cfg.LazyRefreshHours <= 0 && s.cfg.KeepaliveDays <= 0 {
		return false
	}
	exp := a.ExpiresAt
	if exp <= 0 {
		return false // 无过期时间信息：宁可刷，不跳过
	}
	now := s.now().Unix()
	if s.cfg.LazyRefreshHours > 0 && exp > now+int64(s.cfg.LazyRefreshHours)*3600 {
		return true
	}
	if s.cfg.KeepaliveDays > 0 && exp > now+int64(s.cfg.KeepaliveDays)*86400 {
		return true
	}
	return false
}
