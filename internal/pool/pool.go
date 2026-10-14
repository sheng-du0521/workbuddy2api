// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// Pool 账号池核心：结构定义、构造（New/Set* 注入）、在途租约（Acquire/Release）
// 与账号增删（Add/SyncToDir/upsertLocked）。选号/冷却/状态/持久化见同包其他文件。
package pool

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/auth"
)

type Pool struct {
	mu      sync.RWMutex
	byUID   map[string]*entry
	stateFp string
	dirty   atomic.Bool // 内存有变更待落盘
	// store 池状态快照镜像（redisstore.Store）；nil = 无需镜像（未配置 Redis / Noop 之外也可能 nil）。
	// SaveState/LoadState 经它接线，与本地 state.json 并存作启动恢复备份。
	store StoreSnapshotter
	// 熔断器调优（SetBreaker 注入；默认值见 defaultBreaker*）。
	breakerThreshold   int
	breakerCooldown    time.Duration
	breakerCooldownMax time.Duration
	// softRateMax 软冷却指数退避的封顶（SetSoftRateMax 注入；默认 defaultSoftRateMax）。
	softRateMax time.Duration
	// modelRateMax **模型级**冷却对齐上游重置墙钟时的封顶（SetModelRateMax 注入；
	// 默认 defaultModelRateMax）。与 softRateMax 分开的原因见 cooldown.go 的
	// CooldownSoftForModel 注释：账号级封顶要防"白扣一个健康号"，而模型级只挡
	// 一个模型 —— 上限压到 2h 会让「上游明说 5h 后恢复」在 2h 后白白重试一次。
	modelRateMax time.Duration
	// costExploreInterval costTier 条件探索窗口（issue #136 方案 a′，SetCostExploreInterval
	// 注入；默认 defaultCostExploreInterval 30m）。tier 0 垄断 + tier 1 存在且距上次
	// 探索 ≥ 窗口时，本次 pick 生效层切 tier 1-only（探索=搭车改道，零新增上游请求）。
	// 0 = 关停（完全回到现状行为）。
	costExploreInterval time.Duration
	// exploreLast 各 (realm, 模型) 的上次探索时刻，键 = realm + "\x1f" + model。
	// 运行态（不持久化，同 lastUsed/usedSeq 口径）：重启归零 → 每个仍冻结的
	// (域, 模型) 多至 1 次即时重探；已毕业号经 ModelCosts 恢复 tier，学费不重付。
	// 只在探索事件时写入（tier 1 枯竭期间停走，陈旧无害）；不做对称清理。
	exploreLast map[string]time.Time
	// costExploreEvents 累计探索事件数（/status 透出；pick 写锁内 ++，无需 atomic）。
	costExploreEvents int64
	// degradeThreshold / degradeCooldown / degradeCooldownMax 连败降权参数
	// （SetDegrade 注入；默认值见 defaultDegrade*，issue #114）。
	degradeThreshold   int
	degradeCooldown    time.Duration
	degradeCooldownMax time.Duration
	// 三因子加权调优（SetWeights 注入；默认值见 defaultIdle*）。
	idleWeightPerHour float64
	idleWeightMax     float64
	// maxInFlight 单账号最大在途请求数；0 = 不限（租约关闭）。
	maxInFlight int
	// maxInFlightGlobal global 域单账号在途上限分档（WAF 403 修复 P1-1：global 域
	// WAF 风控更紧，压低并发）；0 = 未设置，回落 maxInFlight（不分档，零回归）。
	maxInFlightGlobal int
	// randInt64N 仅供测试注入确定性随机源；nil 时用 math/rand/v2 全局源。
	// 生产代码不应设置此字段。
	randInt64N func(n int64) int64
	// persistFails 本地 state.json 连续落盘失败计数（仅 saveLocked 在持锁下读写，无需 atomic）。
	// 用于落盘失败的日志节流：首败/每 N 次提醒/恢复各打一条，避免磁盘满时刷屏。
	persistFails int
	// weightOfHook / weightOfMaxHook 仅供测试观测（DeptestOnly）：分别统计 weightOf
	// 被调次数与收到的 maxCredits 口径，验证「单次 pick 只算一次 + 全集口径」的重构
	// 契约（TestWeightOfCalledOncePerPick / TestWeightOfMaxCreditsPassedVerbatim）。
	// 生产恒 nil，零开销（nil 函数调用分支预测友好）。
	weightOfHook    func()
	weightOfMaxHook func(maxCredits int64)
	// pickSeq 选号单调序号源：仅 pick 在持 p.mu 写锁时自增并赋给 entry.usedSeq，
	// 无需 atomic。见 entry.usedSeq 注释（解决 Windows 时钟精度导致的 LRU 失效）。
	pickSeq uint64
	// pickMode 选号模式：PickModeWeighted（缺省，三因子加权随机）或
	// PickModeRoundRobin（顺序轮转）。SetPickMode 注入，由 kv proxy_pick_mode 驱动。
	// 空串按缺省（weighted）处理，使零值 Pool 行为与现状一致。
	// 轮转实现见 pickRoundRobinLocked：按 usedSeq 取「最久未用」，不维护绝对游标
	// （游标与过滤后候选集收缩不匹配会导致偏斜，已实测踩坑）。
	pickMode string
	// stopCh 关闭信号：Close 关闭它使 startFlusher 的后台 goroutine 退出。
	// nil = 未启动 flusher（stateFp 为空时 New 不起 flusher）。
	stopCh chan struct{}
	// closeOnce 保证 Close 幂等（多次调用不重复 close channel）。
	closeOnce sync.Once
}

// New 构建池；stateFp 非空时尝试加载旧状态，并启动后台周期性落盘 goroutine。
func New(stateFp string) *Pool {
	p := &Pool{
		byUID:              map[string]*entry{},
		stateFp:            stateFp,
		breakerThreshold:   defaultBreakerThreshold,
		breakerCooldown:    defaultBreakerCooldown,
		breakerCooldownMax: defaultBreakerCooldownMax,
		idleWeightPerHour:  defaultIdleWeightPerHour,
		idleWeightMax:      defaultIdleWeightMax,
		degradeThreshold:   defaultDegradeThreshold,
		degradeCooldown:    defaultDegradeCooldown,
		degradeCooldownMax: defaultDegradeCooldownMax,
		// 探索缺省 30m：tier 0 垄断下的 tier 1 探索窗口（issue #136）。用户经
		// config 显式 "0" 关停（SetCostExploreInterval(0)）。
		costExploreInterval: defaultCostExploreInterval,
		exploreLast:         map[string]time.Time{},
	}
	if stateFp != "" {
		p.load()
		p.startFlusher()
	}
	return p
}

// SetBreaker 注入熔断器参数（main 从 config 解析后调用）。非正值保留原值（用默认）。
func (p *Pool) SetBreaker(threshold int, cooldown, cooldownMax time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if threshold > 0 {
		p.breakerThreshold = threshold
	}
	if cooldown > 0 {
		p.breakerCooldown = cooldown
	}
	if cooldownMax > 0 {
		p.breakerCooldownMax = cooldownMax
	}
}

// SetSoftRateMax 注入软冷却指数退避的封顶时长（main 从 config 解析后调用）。
// 非正值保留原值（用默认 2h），风格同 SetBreaker。
func (p *Pool) SetSoftRateMax(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d > 0 {
		p.softRateMax = d
	}
}

// SetModelRateMax 注入**模型级**冷却对齐上游重置墙钟时的封顶（默认 defaultModelRateMax）。
// 非正值保留原值，风格同 SetSoftRateMax。
//
// 与 SetSoftRateMax 分开是刻意的：账号级封顶（2h）的职责是"别把健康号白扣太久"；
// 模型级冷却只挡一个模型、其它模型照常服务，把上限也压到 2h 只会让
// 「上游明说 5h 后恢复」在 2h 后被白试一次（一次失败请求 + 一次风控痕迹）。
// 见 cooldown.go 的 CooldownSoftForModel。
func (p *Pool) SetModelRateMax(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d > 0 {
		p.modelRateMax = d
	}
}

// SetCostExploreInterval 注入 costTier 条件探索窗口（main 从 config 解析后调用，
// issue #136）。0 = 关停（完全回到现状行为）；正值覆盖默认 30m。
// 注意：与 SetSoftRateMax「非正值保留默认」不同，0 在这里是**合法值**（关停开关，
// 与 config 的 "0" 关停语义对齐）——不设 0 语义就无法关停探索。
func (p *Pool) SetCostExploreInterval(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d < 0 {
		return // 负值非法，保留现值
	}
	p.costExploreInterval = d
}

// CostExploreStatus 透出探索台账（/status 用）：累计探索事件数 + 各 (域, 模型)
// 的最近探索时刻（键内 \x1f 分隔符输出为 "|"，与 model_costs 行对照即可读出
// 「探索→毕业」全链路）。RLock 只读遍历；map 大小受「服务过的 (域, 模型)」集合
// 约束（与 modelCost 同界，天然有界）。
func (p *Pool) CostExploreStatus() (events int64, last map[string]time.Time) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	last = make(map[string]time.Time, len(p.exploreLast))
	for k, ts := range p.exploreLast {
		// 键 realm+"\x1f"+model → 输出 "|"（JSON 安全可读；\x1f 不可打印）。
		last[strings.ReplaceAll(k, "\x1f", "|")] = ts
	}
	return p.costExploreEvents, last
}

// SetDegrade 注入连败降权参数（main 从 config 解析后调用，issue #114）。
// 非正值保留原值（用默认，见 defaultDegrade*），风格同 SetBreaker/SetSoftRateMax。
func (p *Pool) SetDegrade(threshold int, cooldown, cooldownMax time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if threshold > 0 {
		p.degradeThreshold = threshold
	}
	if cooldown > 0 {
		p.degradeCooldown = cooldown
	}
	if cooldownMax > 0 {
		p.degradeCooldownMax = cooldownMax
	}
}

// SetWeights 注入三因子加权的闲置补偿参数。非正值保留原值（用默认）。
func (p *Pool) SetWeights(idlePerHour, idleMax float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if idlePerHour > 0 {
		p.idleWeightPerHour = idlePerHour
	}
	if idleMax > 0 {
		p.idleWeightMax = idleMax
	}
}

// SetMaxInFlight 注入单账号最大在途请求数；0 = 不限。负值保留原值。
func (p *Pool) SetMaxInFlight(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n >= 0 {
		p.maxInFlight = n
	}
}

// SetMaxInFlightGlobal 注入 global 域单账号在途上限（WAF 403 修复 P1-1 分档）；
// 0 = 未设置，global 账号回落 maxInFlight（不分档）。负值保留原值。
func (p *Pool) SetMaxInFlightGlobal(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n >= 0 {
		p.maxInFlightGlobal = n
	}
}

// inFlightLimit 报告账号的生效在途上限（global 分档优先，回落 maxInFlight）；
// 0 = 不限。调用方需已持 p.mu（或快照过 limit，见 Acquire 注释）。
func (p *Pool) inFlightLimit(e *entry) int {
	if p.maxInFlightGlobal > 0 && e.a.Realm() == "global" {
		return p.maxInFlightGlobal
	}
	return p.maxInFlight
}

// 选号模式（kv `proxy_pick_mode`）。缺省/非法值一律回落 PickModeWeighted，
// 保证升级后行为与现状严格一致（新机制需显式开启）。
const (
	// PickModeWeighted 三因子加权随机（缺省）：credits/快过期/idle 加权抽签 +
	// top5 短名单 + minPickGap 防并发撞号。偏向高分号，适合"积分最大化"。
	PickModeWeighted = "weighted"
	// PickModeRoundRobin 顺序游标轮转：候选按 UID 稳定排序后环形顺序取号，
	// 使每个号被均匀使用（负载天然铺开、不打热点）。对齐参考实现
	// WorkBuddy-Switch2api 的 next_uid_of（cursor.fetch_add + (start+i)%len）。
	// 语义差异：参考实现是"纯轮转"，本实现在 tier 硬过滤后轮转（优先限免/未知号，
	// 不打收费号），因为放弃成本分层会直接把"优先免费"的既有收益丢掉。
	PickModeRoundRobin = "roundrobin"
)

// SetPickMode 设置选号模式（main/ReloadSelection 从 kv 解析后调用）。
// 仅 PickModeRoundRobin 生效，其余（含空串/非法值）一律回落 PickModeWeighted。
// 无内部状态需重置：轮转按 usedSeq（最久未用）取号，切换模式不改变已积累的选取历史。
func (p *Pool) SetPickMode(mode string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if mode != PickModeRoundRobin {
		mode = PickModeWeighted
	}
	p.pickMode = mode
}

// PickMode 返回当前生效的选号模式（空串归一为 PickModeWeighted，便于 UI/摘要直接展示）。
func (p *Pool) PickMode() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.pickMode != PickModeRoundRobin {
		return PickModeWeighted
	}
	return p.pickMode
}

// SetStore 注入池状态快照镜像（redisstore.Store）。nil 表示不镜像（纯本地恢复）。
// 必须在 SyncToDir 之前调用，使"择新恢复"发生在账号对齐之前。
func (p *Pool) SetStore(s StoreSnapshotter) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.store = s
}

// Acquire 为 uid 占一个在途名额（会话粘性命中后调用）；池上限内返回 true。
// 名额用 entry.inFlight 原子自增，满额返回 false。上限按账号 realm 分档
// （global 档 maxInFlightGlobal，P1-1；未设置回落 maxInFlight）。
func (p *Pool) Acquire(uid string) bool {
	p.mu.RLock()
	e, ok := p.byUID[uid]
	if !ok {
		p.mu.RUnlock()
		return false
	}
	limit := p.inFlightLimit(e)
	p.mu.RUnlock()
	if limit <= 0 {
		// 不限：计数仍累加（供状态观测），但永不拒绝。
		e.inFlight.Add(1)
		return true
	}
	for {
		cur := e.inFlight.Load()
		if cur >= int64(limit) {
			return false
		}
		if e.inFlight.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

// Release 释放一个在途名额。幂等减到 0 为止（防重复释放扣成负数）。
func (p *Pool) Release(uid string) {
	p.mu.RLock()
	e, ok := p.byUID[uid]
	p.mu.RUnlock()
	if !ok {
		return
	}
	for {
		cur := e.inFlight.Load()
		if cur <= 0 {
			return
		}
		if e.inFlight.CompareAndSwap(cur, cur-1) {
			return
		}
	}
}

// SetRandomSource 仅供测试注入确定性随机源；生产代码不应调用。
// 注入源取 n∈[0,n) 后，pickWeighted 的抽签结果完全可预测。
func (p *Pool) SetRandomSource(fn func(n int64) int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.randInt64N = fn
}

// Add 加入账号；已存在则保留原状态、更新凭证（upsert 单账号）。
func (p *Pool) Add(a *auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.upsertLocked(a)
}

// SyncToDir 用最新扫描结果对齐池：新账号加入、消失的账号剔除（状态保留）。
// 剔除结果持久化回 state.json，避免已删账号在下次启动时被 load() 复活。
func (p *Pool) SyncToDir(auths []*auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := make(map[string]bool, len(auths))
	for _, a := range auths {
		seen[a.UID] = true
		p.upsertLocked(a)
	}
	changed := false
	for uid := range p.byUID {
		if !seen[uid] {
			delete(p.byUID, uid)
			changed = true
		}
	}
	if changed {
		p.saveLocked()
	}
}

// upsertLocked 更新或插入单个账号；已存在则只换凭证、保留 credits/cooling 状态。
// 调用方必须已持有 p.mu；Add 与 SyncToDir 共用此 upsert 逻辑。
func (p *Pool) upsertLocked(a *auth.Auth) {
	if e, ok := p.byUID[a.UID]; ok {
		e.a = a // 保留 credits/cooling 状态
		return
	}
	p.byUID[a.UID] = &entry{a: a}
}
