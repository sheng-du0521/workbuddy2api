// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 选号：Pick 簇（healthy 三因子加权 Top5 短名单 + 加权随机 + 全冷却兜底 + 在途占满过滤）。
package pool

import (
	"log"
	"math"
	"math/rand/v2"
	"sort"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
)

// Pick 单一选号入口（无请求级轮换、无 realm 过滤，模型感知）。
// DeptestOnly: 全库仅 pool 包测试引用；生产选号全走 PickExcludingForRealm /
// PickByUIDForModel。保留是因为测试需要无轮换/无 realm 的最小选号原语；
// 迁 export_test.go 不可行——export_test 对包外不可见，而本方法的语义文档
// （挑选策略全文）对生产簇（pick 私有实现）仍有维护参考价值。
// 挑选策略：healthy 账号中按三因子权重取前 5 名，再在 Top5 内按同一权重加权随机抽签，
// 意图是打散热点，避免永远打同一个账号。
// model 非空时启用 6004 模型级冷却豁免（healthyForModel）；空则等价账号级 healthy。
// 需要请求级轮换（tried）或分池（realm）时用 PickExcludingForRealm。
func (p *Pool) Pick(model string) *auth.Auth {
	return p.pick(nil, model, "")
}

// pick 在 healthy 候选集中按三因子权重加权随机选出账号，并记录 lastUsed（防并发撞号）。
// 候选集是 top5 近似：先按三因子权重（weightOf）降序取前 5（credits 只是权重的一个因子，
// 闲置补偿与成功率同样决定谁进短名单），再在 top5 内做防撞号过滤。
// 并发防雪崩：跳过 lastUsed 距今 < minPickGap 的账号（除非 top5 全部刚被用过，
// 此时退回最近最少使用 LRU 账号），迫使高并发请求发散，而不是全部撞同一高分账号。
// minPickGap=0（测试用）时过滤恒通过，退化为纯加权随机。
// reqModel 非空时把健康口径换成 healthyForModel（6004 模型豁免生效；PickExcluding 传 ""）。
// realm 非空时候选过滤叠加 Realm()==realm 谓词（分池选号域；PickExcluding 传 ""）。
// 注意：模型豁免进 normal 选号（候选 healthyForModel 判定），兜底则**排除**对该模型
// 正在冷却的账号（v0.31.0）—— 兜底是"全池都不可用"时的降级，它探测的是**账号级**
// 冷却能否放开；把请求发给一个"对该模型正处于 6004 冷却"的号，探测的维度是错的，
// 结果只有一种：必然失败的那一次请求 + 又一条上游风控痕迹。宁可提前 503。
func (p *Pool) pick(tried map[string]bool, reqModel, realm string) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	// 惰性清理过期的 6004 模型级冷却与成本台账（两者的 map 都不无限膨胀；
	// status 只读遍历天然跳过过期项，但内存条目必须在此真正删除）。
	for _, e := range p.byUID {
		e.pruneExpiredModelCooldowns(now)
		e.pruneExpiredModelCosts(now)
	}
	realmOK := func(e *entry) bool { return realm == "" || e.a.Realm() == realm }
	healthyOf := func(e *entry) bool { return realmOK(e) && e.healthy(now) }
	if reqModel != "" {
		healthyOf = func(e *entry) bool { return realmOK(e) && e.healthyForModel(now, reqModel) }
	}
	var cands []*entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if !healthyOf(e) {
			continue
		}
		if p.inFlightFull(e) {
			continue // 在途占满：跳过（max=0 不限时不触发）
		}
		cands = append(cands, e)
	}
	if len(cands) == 0 {
		// 全冷却兜底：无 healthy 候选时，从冷却账号里选 until 最早到期的一个
		// （熔断/冷却共用 expiry 口径，取较早截止者）。禁用的账号永不参与兜底；
		// 对**本请求模型**正在模型级冷却的账号同样排除（v0.31.0，见函数注释）。
		return p.pickEarliestExpiryLocked(tried, now, realm, reqModel)
	}
	// top5 短名单按三因子权重降序截断（而非 credits 单纯降序）：否则闲置补偿
	// 根本进不了短名单决策，低 credits 但久置的账号会永远排不进 top5。
	// maxCredits 统一用**全集口径**（tier 过滤前的全部 healthy 候选）：截断排序与
	// 抽签权重共享同一基准，两个阶段权重可比（旧实现 pickWeighted 在 eligible 子集
	// 上重取 max，全集最大 credits 号被 minPickGap 挤出后子集 max 偏小，剩余号的
	// credits 比例整体膨胀 k=全集max/子集max，credits 项相对 idle/success 被放大）。
	var maxCredits int64
	for _, e := range cands {
		if e.credits > maxCredits {
			maxCredits = e.credits
		}
	}
	// 权重与成本分层**各算一次、全程复用**（weighted 结构体定义见包级注释）：
	// weightOf 每候选一次（预计算存入 ws.w），costTier/modelCostOf 同样每候选一次
	// （存入 ws.tier/ws.cost1k）——sort 比较器与 pickWeighted 都只读缓存字段，
	// 不再现算。比较器内现算会翻成 O(n log n) 次冗余浮点/map 查找（46 账号约
	// 500 次比较），旧实现在此翻过车。
	// 成本分层：reqModel 非空时，按该模型的实测扣费把候选分层，只保留最优层。
	//   0 = 已实测免费（限免期/夜间免费的号，最强偏好）
	//   1 = 无观测（含观测过期）
	//   2 = 已实测收费
	// 为什么"无观测"排在"已实测收费"之前：新号的限免状态只能靠实测发现，
	// 若已知收费的号恒压过未知号，那台免费的号永远轮不到，也就永远学不到。
	// 为什么用硬过滤而非仅排序：pickWeighted 会在候选内加权随机，只排序的话
	// 收费号仍有机会抽中，达不到"优先免费"的语义。
	costTier := func(e *entry) (int, float64) {
		mc, ok := e.modelCostOf(reqModel, now)
		if !ok {
			return 1, 0
		}
		if mc.CostPer1k <= 0 {
			return 0, 0
		}
		return 2, mc.CostPer1k
	}
	bestTier := 2
	hasTier1 := false
	explored := false // 本次 pick 是否切了探索层（事件日志在选中号确定后打）
	for _, e := range cands {
		if ti, _ := costTier(e); ti < bestTier {
			bestTier = ti
		}
	}
	// 条件探索（issue #136 方案 a′，语义契约七条见设计报告 §2.1）：tier 0 垄断层
	// 存在（bestTier==0 且 reqModel 非空）且候选含 tier 1（冻结存在）且距上次
	// 探索 ≥ 窗口（零值 timer=从未探索→首次满足即探）时，本次 pick 生效层切
	// tier 1-only——探索=搭车改道，把一个既有真实用户请求改道给未知号（零新增
	// 上游请求；IP 维度零增量，WAF 友好）。成功 → NoteModelCost 首观测 → 毕业
	// （tier 0/2，下一轮 pick 立即生效）；失败 → 既有错误策略照常，无探测风暴。
	// hasTier1 复用本循环上方 costTier 的预计算口径（每候选一次的契约不变）——
	// 下方 ws 构建循环顺带置位，不在判定处再算一遍。timer 同锁写入：并发 pick
	// 串行进入写锁，只有一个进入者能通过窗口判定（天然防重复探索）。
	// key = realm + "\x1f" + reqModel：同模型名可跨域，探索节奏按 (域, 模型)
	// 独立；realm==""（Pick 老语义）单独成键。
	if p.costExploreInterval > 0 && bestTier == 0 && reqModel != "" {
		for _, e := range cands {
			if ti, _ := costTier(e); ti == 1 {
				hasTier1 = true
				break
			}
		}
		key := realm + "\x1f" + reqModel
		if hasTier1 && now.Sub(p.exploreLast[key]) >= p.costExploreInterval {
			p.exploreLast[key] = now
			p.costExploreEvents++
			bestTier = 1
			explored = true
		}
	}
	ws := make([]weighted, 0, len(cands))
	for _, e := range cands {
		ti, ci := costTier(e)
		if ti == bestTier {
			ws = append(ws, weighted{e: e, w: p.weightOf(e, maxCredits, now), tier: ti, cost1k: ci})
		}
	}
	// 等权重洗牌：仅当存在权重并列（epsilon 比较，防浮点微差让洗牌静默失效）且
	// 候选数超过 top5 时，才对 ws 做 Fisher-Yates 洗牌（且**不消耗 p.randInt64N
	// 注入源**，避免改变 pickWeighted 的确定性语义，见 TestPickDeterministicViaSet
	// RandomSource）。权重全等或存在并列时，按字典序截断会让 uid 靠后的账号永远
	// 进不了 top5（惊群测试 c00 集中 79/100 的根因：c05..c09 被字典序截断、LRU
	// 兜底又只在 top5 内转）。洗牌用独立的 time-seeded 源，只在截断边界制造等
	// 权重随机次序，不影响加权抽签本身的确定性。
	if len(ws) > 5 {
		eq := false
		for i := 1; i < len(ws); i++ {
			if math.Abs(ws[i].w-ws[0].w) < weightEpsilon {
				eq = true
				break
			}
		}
		if eq {
			shuf := rand.New(rand.NewPCG(uint64(now.UnixNano()), uint64(len(ws))))
			shuf.Shuffle(len(ws), func(i, j int) { ws[i], ws[j] = ws[j], ws[i] })
		}
	}
	sort.SliceStable(ws, func(i, j int) bool {
		// costTier 硬过滤后 ws 全员同层，但仍按 cost1k 升序排（tier 2 层内单价低者
		// 在前；tier 0/1 层 cost1k 恒 0，本比较退化为权重比较）——读缓存字段不现算。
		if ws[i].cost1k != ws[j].cost1k {
			return ws[i].cost1k < ws[j].cost1k // 收费层：单价低的在前
		}
		if ws[i].w != ws[j].w {
			return ws[i].w > ws[j].w
		}
		return ws[i].e.a.UID < ws[j].e.a.UID // 稳定兜底（洗牌后此项几乎不触发）
	})
	cands = cands[:0]
	for _, c := range ws {
		cands = append(cands, c.e)
	}
	// 顺序轮转（可选模式，对齐参考项目 WorkBuddy-Switch2api 的 next_uid_of）：
	// 在 tier 过滤后的候选上按「最久未用优先」取号，取代加权随机，使每个号被均匀
	// 使用（负载自然铺开、不打热点——高分号被高频命中本身就是一种可识别特征）。
	// 三条设计约束，改动前先看清：
	//   1. 放在 tier 过滤**之后** —— 轮转是"同层内均匀"，不是"放弃成本优化"；
	//      若在过滤前轮转会直接打到收费号，把"优先限免/未知"的既有收益丢掉。
	//   2. **不截断 top5** —— 轮转的意义就是覆盖全部可用号，截断会饿死窗口外的号。
	//   3. 用「最久未用」而非绝对游标 —— 见 pickRoundRobinLocked 注释：游标与
	//      上层过滤后候选集的收缩不匹配，会退化成偏斜（已实测踩坑）。
	if p.pickMode == PickModeRoundRobin {
		return p.finishPickLocked(p.pickRoundRobinLocked(cands, now), now, explored, reqModel, realm)
	}
	// candsAll 保留截断前的全候选（权重降序），供 LRU 兜底在全量范围选最旧者，
	// 避免 top5 字典序截断把等权重靠后账号饿死（惊群根因之一）。
	candsAll := cands
	if len(cands) > 5 {
		cands = cands[:5]
	}
	// 防并发撞号：在持锁内基于「上次选中时刻」过滤，但同一批并发 goroutine 会串行进入
	// 本函数（写锁），每个进入者都把 lastUsed 置为 now —— 于是同一瞬间的第 2..N 个
	// 进入者看到前一个账号 lastUsed==now（距今 0 < minPickGap），被自然挤向其他账号。
	// 关键：lastUsed 在锁内赋值，使时间窗口判定在并发下可重入（此前 Acquire 在锁外，
	// 多个 goroutine 在窗口内同时通过校验造成惊群，TestPickAntiThunderingHerd 实证）。
	// 注意：先截断后过滤（截断边界内被挤出的号不回填）——与旧实现语义严格一致。
	eligible := make([]weighted, 0, len(cands))
	for _, we := range ws[:min(len(ws), 5)] {
		if now.Sub(we.e.lastUsed) >= minPickGap {
			eligible = append(eligible, we)
		}
	}
	var e *entry
	if len(eligible) == 0 {
		// top5 全部刚被用过：LRU 兜底，在**全候选 candsAll**（非仅 top5）里选最旧者。
		// 用 usedSeq 单调序号而非 lastUsed 墙钟比较：Windows 等平台 time.Now() 精度
		// ~0.5ms，快速连续选号时所有 lastUsed 完全相等，Before 全 false 会恒选
		// candsAll[0] 导致集中。usedSeq 严格全序，与时间精度无关。
		e = candsAll[0]
		for _, c := range candsAll[1:] {
			if c.usedSeq < e.usedSeq {
				e = c
			}
		}
	} else {
		e = p.pickWeighted(eligible) // eligible 保序 = top5 降序子集，权重直接用预计算值
	}
	return p.finishPickLocked(e, now, explored, reqModel, realm)
}

// finishPickLocked 选中后的统一收尾：探索事件日志 + lastUsed/usedSeq 推进。
// 加权随机与顺序轮转两条选号路径共用，避免"某一条路径漏推进 usedSeq"这类只在
// 单条路径上犯的错——那会让 LRU 兜底与防惊群在轮转模式下静默失效。
func (p *Pool) finishPickLocked(e *entry, now time.Time, explored bool, reqModel, realm string) *auth.Auth {
	if explored {
		// 探索事件日志（§5 可观测性）：选中号此时才确定，故在选中点打出。
		// 毕业结果由相邻的既有日志闭环（免费号无日志、收费号走 NoteModelCost 常规路径）。
		log.Printf("[pool] cost explore model=%s realm=%q acct=%s window=%s",
			reqModel, realm, logfmt.Label(e.a.UID, e.a.Nickname), p.costExploreInterval)
	}
	e.lastUsed = now // 锁内即时标记：下一个进入 pick 的 goroutine 立即看到本号已用
	p.pickSeq++
	e.usedSeq = p.pickSeq // 单调序号：保证 usedSeq 严格全序（防惊群/LRU 的权威依据）
	return e.a
}

// pickRoundRobinLocked 顺序轮转（PickModeRoundRobin）：始终挑选当前「最久未用」的
// 候选（usedSeq 最小），等价于顺序游标轮转（推荐实现 WorkBuddy-Switch2api 的
// next_uid_of），但不依赖绝对游标。
//
// 为什么不用绝对游标（cursor++ 后取 cands[start%n]）——这是实测踩到的坑：
// 上层的消耗分组过滤是「取候选 → 不匹配就丢进 tried → 再取」，每丢一个候选，
// 候选集就少一个，而绝对游标仍在递增。`start % n` 的落点随 n 变化而漂移，
// 导致同一组内的"轮转"退化成伪随机偏斜：实测连发 6 次请求，**全部落在同一个
// 「用户申请」账号**上（它的下标恰好总被游标命中）。
//
// 「最久未用」天然与候选集大小无关：无论过滤掉多少个，永远取剩余里最久未用的
// 那个，组内依旧严格轮流。首次全部 usedSeq=0 时按 UID 排序决定先后，保证确定性
// （map 遍历顺序随机，不 tie-break 则每次运行结果不同）。
//
// minPickGap 照常尊重（管理员配的防撞号节流不能被模式绕过）：按最久未用序扫描，
// 返回第一个不在窗口内的；若全部都在窗口内，返回最久未用者（序列首位）。
// 调用方需已持 p.mu 写锁，且 cands 非空。
func (p *Pool) pickRoundRobinLocked(cands []*entry, now time.Time) *entry {
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].usedSeq != cands[j].usedSeq {
			return cands[i].usedSeq < cands[j].usedSeq // 最久未用优先
		}
		return cands[i].a.UID < cands[j].a.UID // 同序号（含从未使用）按 uid 定序，保证确定性
	})
	for _, e := range cands {
		if now.Sub(e.lastUsed) >= minPickGap {
			return e
		}
	}
	return cands[0] // 全部在防撞号窗口内：取最久未用者
}

// pickEarliestExpiryLocked 全冷却兜底：在非禁用的软冷却/熔断账号中选截止最早的一个，
// 并排除对该 reqModel 正在模型级冷却（6004/11102）的账号（v0.31.0）。
//
// 为什么排除：本函数选出来的号**一定**在账号级冷却中，发给它本身就是一次半开探测
// （期望"冷却已过期/上游已放开"）。但模型级冷却与账号级是**两个正交维度** —— 一个
// 号完全可能"账号级冷却刚到期"、"该模型却还有 8 小时才重置"。此时把它选出来，等于
// 用一个注定失败的请求去探测一个已经知道答案的问题（我们把答案就记在
// modelCooldowns 里）。返回 nil（→ 上层 503）比发一次必然失败的请求更诚实：
// 前者客户端立刻知道要等，后者要等一个完整的上游往返 + 换号退避。
//
// 注意它并不减少"可服务账号"：对**其他**模型正在冷却的号照常参与兜底。
// 分级：disabled 永不参与；CoolHard（余额耗尽，等签到的号）同样排除——调了必 402，浪费轮换并产生噪音日志；
// CoolSoft 与熔断号允许参与（可能已恢复，失败成本仅一轮换）。
// 被 tried 排除、在途占满的账号同样跳过（维持请求级轮换 + 租约语义）。无任何可用返回 nil。
func (p *Pool) pickEarliestExpiryLocked(tried map[string]bool, now time.Time, realm, reqModel string) *auth.Auth {
	var best *entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if realm != "" && e.a.Realm() != realm {
			continue // 域过滤：池内跨 realm 的冷却账号不参与本 realm 兜底
		}
		if e.modelCooled(now, reqModel) {
			continue // 该模型仍在模型级冷却：探测维度不对，见函数注释（v0.31.0）
		}
		if e.disabled || e.manualDisabled {
			continue // 禁用/手动停用的账号永不参与兜底
		}
		if e.coolKind == CoolHard && !e.until.IsZero() && now.Before(e.until) {
			continue // 余额耗尽号（处于有效 hard 冷却期）不参与兜底：等签到恢复，调了必 402
		}
		if p.inFlightFull(e) {
			continue
		}
		exp := e.expiry(now)
		if exp.IsZero() {
			continue
		}
		if best == nil || exp.Before(best.expiry(now)) {
			best = e
		}
	}
	if best == nil {
		return nil
	}
	log.Printf("WARN: [pool] fallback_earliest_expiry acct=%s until=%s kind=%s", logfmt.Label(best.a.UID, best.a.Nickname), best.expiry(now).Format(time.RFC3339), best.fallbackKind(now))
	best.lastUsed = time.Now()
	// 兜底同样是「选中」，必须与 pick() 正常路径、粘性命中路径（PickByUIDForModel）
	// 一样推进 usedSeq/pickSeq：否则被兜底反复选中的账号 usedSeq 恒为 0，在 pick 的
	// LRU 兜底（按 usedSeq 取最旧）眼里永远是「最旧」，刚被用过就被立刻再选——
	// 防集中/防惊群失效（entry.usedSeq 契约：每次被选中时取 p.pickSeq 自增值）。
	p.pickSeq++
	best.usedSeq = p.pickSeq
	return best.a
}

// ── 模型级半开探测选号（v0.31.6）─────────────────────────────────────────────

// PickProbeForModelRealm 半开探测选号（v0.31.6，webapp 在"常规候选已耗尽"之后调用）。
//
// 在「账号级完全健康、但对该模型正处**可探测**的模型级冷却」的账号里，选出**最早到期**
// 的那一个，并**原子地**记下本次探测时刻。返回 nil = 本次不探（没有可探的号）。
//
// ## 为什么"判定"与"记时刻"必须在同一把锁里
//
// 闸门的全部意义是「同一 (账号, 模型) 在 interval 内只放一次」。若分成两步判定，
// 并发请求会同时通过判定 → 一次探测变 N 次 → 出站强度瞬间放大 N 倍。
// 本项目最贵的一类教训就是出站强度（59 张卡同时出站 = 一次风控事件）。
//
// ## 候选口径为什么要求"账号级完全健康"
//
// 账号级冷却/熔断/降权/停用的号有各自的兜底路径（pickEarliestExpiryLocked）。
// 模型级探测要回答的是一个具体问题：「这个号的这个模型恢复了吗」—— 只有当账号本身
// 没有别的问题时，答案才只取决于模型维度。把两种病混在一起探，失败了也分不清是谁的问题。
//
// ## 与 pickEarliestExpiryLocked 的分工（刻意保留的差异）
//
// 那个函数仍然**排除**模型级冷却号（v0.31.0 的决定）。有人会问"现在有探测了，为什么不
// 一起放行"—— 因为它选出的号一定在账号级冷却中，那次发送本身就是一次面向**账号维度**的
// 探测；用"账号级刚到期"去猜"模型级也好了"是拿错误的证据下判断。两个维度两个入口。
//
// ## 已知的取舍
//
// 调用方（proxy）在拿到候选后还要过范围/消耗分组校验，若被拒会把该 UID 记入 tried 再要
// 下一个 —— 被拒的那个号已经吃掉了本次探测名额（ProbedAt 已写）。即在"探测号全部被范围
// 过滤掉"的极端配置下会浪费探测名额；代价是该号晚 interval 才再探一次，不影响正确性，
// 换取的是"闸门原子、不可能被并发绕过"这一更重要的性质。
func (p *Pool) PickProbeForModelRealm(tried map[string]bool, model, realm string) *auth.Auth {
	if model == "" {
		return nil
	}
	delay, interval, global := p.modelProbeDelay, p.modelProbeInterval, p.modelProbeGlobal
	if interval <= 0 {
		return nil // 关停（SetModelProbe 传 <=0）：回到"严格等上游墙钟"
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	var best *entry
	// 全池间隔：本模型最近**有没有探过**（不管探的是哪个号）。
	// 用"扫 ProbedAt"而不是另开一张 map 记录时刻：ProbedAt 已经在每条条目里，
	// 且随 state.json 持久化 —— 少一份状态就少一处可能漂移的地方（重启也不会失忆）。
	// 缺口背景见 defaultModelProbeGlobal 注释：没有这一层，客户端连续请求就能让
	// "一次请求探一个号"，N 个号 = N 次上游调用。
	if global > 0 {
		for _, e := range p.byUID {
			mc, ok := e.modelCooldowns[model]
			if !ok || mc.ProbedAt.IsZero() {
				continue
			}
			if now.Sub(mc.ProbedAt) < global {
				return nil
			}
		}
	}
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if realm != "" && e.a.Realm() != realm {
			continue // 域过滤：跨域号不参与本域探测
		}
		if !e.healthy(now) {
			continue // 账号级有别的病：交给账号级兜底，探测分不清根因
		}
		if !e.modelCooled(now, model) {
			continue
		}
		if ok, _ := probeGate(now, e.modelCooldowns[model], delay, interval); !ok {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		if best == nil || e.modelCooldowns[model].Until.Before(best.modelCooldowns[model].Until) {
			best = e
		}
	}
	if best == nil {
		return nil
	}
	mc := best.modelCooldowns[model]
	mc.ProbedAt = now
	best.modelCooldowns[model] = mc
	p.dirty.Store(true)
	// 与 pick() 正常路径、兜底路径同一契约：被选中必须推进 usedSeq，否则被反复选中的
	// 账号在 LRU 眼里永远"最旧"，刚探完立刻又被探（防集中失效）。
	best.lastUsed = time.Now()
	p.pickSeq++
	best.usedSeq = p.pickSeq
	log.Printf("INFO: [pool] model_probe acct=%s model=%s until=%s reason=%q",
		logfmt.Label(best.a.UID, best.a.Nickname), model, mc.Until.Format(time.RFC3339), mc.Reason)
	return best.a
}

// inFlightFull 报告账号是否已占满在途名额（max=0 不限 → 恒 false）。
// 调用方需已持 p.mu（读锁或写锁均可，本方法只读上限字段）。上限按 realm
// 分档（global 档 maxInFlightGlobal，WAF 403 修复 P1-1；未设置回落 maxInFlight）。
func (p *Pool) inFlightFull(e *entry) bool {
	limit := p.inFlightLimit(e)
	if limit <= 0 {
		return false
	}
	return e.inFlight.Load() >= int64(limit)
}

// minPickGap 防并发撞号窗口：同一账号在该窗口内不重复被选中（除非 top5 全部刚被用过）。
// 生产默认 100ms；纯加权分布测试可临时置 0 关闭防撞号。
var minPickGap = 100 * time.Millisecond

// weighted 单个候选的选号预计算结果：权重（weightOf，每候选一次）+ 成本分层
// （tier/cost1k，每候选一次）。sort 比较器与 pickWeighted 抽签都只读缓存字段，
// 任何一处现算（旧实现在比较器内重算 costTier、在 pickWeighted 内重算 weightOf）
// 都会翻成 O(n log n) 次冗余浮点/map 查找，且引入两阶段口径分裂。
type weighted struct {
	e      *entry
	w      float64
	tier   int     // costTier 结果缓存（0 免费 / 1 无观测 / 2 收费）
	cost1k float64 // CostPer1k 缓存（tier 2 排序用；tier 0/1 恒 0）
}

// expiringWeight 快过期积分占比的权重系数（三因子之一，issue:积分过期）。
// 取 8：略低于 credits 总量项（×10），足以在"快过期多"与"总量相近"的号之间拉开差距，
// 又不至于压过总量项让"总量大但快过期少"的号被完全饿死。
const expiringWeight = 8.0

// pickWeighted 三因子加权随机（claude-api selectWeightedRandom 参考口径）：
//
//		weight = credits 比例 × 10 + 快过期积分占比 × expiringWeight + idleWeight
//
//	  - credits 比例 = 该号 credits / 全集最大 credits（避免量纲爆炸；全集口径：
//	    tier 过滤前的全部 healthy 候选，与截断排序共享基准——见 weighted 预计算注释）
//	  - 快过期积分占比 = creditsExpiring/credits（×8，issue:积分过期）
//	  - idleWeight = min(距 lastUsed 小时数 × idleWeightPerHour, idleWeightMax)；从未使用给满分
//
// credits 全 0 时仍按 idle+expiring 加权（不退化均匀随机）。
// 权重为浮点，用 int64 定点（×1e6）抽签可保持确定性随机源注入（randInt64N 语义不变）。
// 随机源优先用 p.randInt64N（仅供测试注入确定性），nil 时回退 math/rand/v2 全局源。
// 候选携带调用方预计算的权重（weighted.w，maxCredits/now 均已按全集口径算好），
// 本函数**不再调用 weightOf**——单次 pick 内每个候选的权重只算一次，预计算与抽签
// 共用同一数值（旧实现在 eligible 子集上用子集 maxCredits 重算第二遍，两次口径
// 分裂：全集最大 credits 号被 minPickGap 挤出后，子集内 credits 比例整体膨胀）。
func (p *Pool) pickWeighted(cands []weighted) *entry {
	const scale = 1_000_000 // 定点放大：int64 累加权重大整数抽签
	weights := make([]int64, len(cands))
	var total int64
	for i, c := range cands {
		// 四舍五入并保底权重 ≥1：向零截断会让 w<1/scale 的低权重号权重归零，
		// 彻底失去被抽中机会（候选少时加剧选号集中，惊群测试的放大因子之一）。
		// 保底 ≥1 同时保证 total ≥ len(cands) > 0（抽签分支无需 total<=0 兜底）。
		wi := int64(c.w*scale + 0.5)
		if wi < 1 {
			wi = 1
		}
		weights[i] = wi
		total += wi
	}
	rnd := rand.Int64N
	if p.randInt64N != nil {
		rnd = p.randInt64N
	}
	r := rnd(total)
	var acc int64
	for i := range cands {
		acc += weights[i]
		if r < acc {
			return cands[i].e
		}
	}
	return cands[len(cands)-1].e
}

// weightEpsilon 等权重判定的浮点容差：权重公式微调后权重不再位级相等，
// 精确相等比较会让等权重洗牌静默失效、回到字典序截断饿死问题。
const weightEpsilon = 1e-9

// weightOf 计算单个账号的三因子权重。单次 pick 内对每个候选只调用一次
// （预计算存入 weighted.w，sort 比较器与 pickWeighted 均读缓存不重算）。
func (p *Pool) weightOf(e *entry, maxCredits int64, now time.Time) float64 {
	if p.weightOfHook != nil {
		p.weightOfHook() // DeptestOnly 观测：验证单次 pick 只算一次
	}
	if p.weightOfMaxHook != nil {
		p.weightOfMaxHook(maxCredits) // DeptestOnly 观测：验证全集口径
	}
	w := 1.0
	// 1. credits 比例 ×10（会计入 mid-credit 锚点，避免全员 0 时 credits 项为 0）。
	if maxCredits > 0 {
		w += float64(e.credits) / float64(maxCredits) * 10
	}
	// 1b. 快过期积分加成（issue:积分过期）：官方活动赠送的奖励积分按批过期，
	// 不用就作废。creditsExpiring 占总量比例越高，越应优先被消耗——把"快过期
	// 占比"作为一个独立的强权重项（×expiringWeight），让快过期积分多的号优先选。
	// 与成本分层（costTier 优先免费）正交：那是按"实测扣费"分层，这是按"过期紧迫度"。
	if e.credits > 0 && e.creditsExpiring > 0 {
		w += float64(e.creditsExpiring) / float64(e.credits) * expiringWeight
	}
	// 2. 闲置补偿。
	if e.lastUsed.IsZero() {
		w += p.idleWeightMax // 从未使用 → 满分
	} else {
		hours := now.Sub(e.lastUsed).Hours()
		idleW := hours * p.idleWeightPerHour
		if idleW > p.idleWeightMax {
			idleW = p.idleWeightMax
		}
		if idleW < 0 {
			idleW = 0 // lastUsed 在未来（时钟回拨）时钳 0
		}
		w += idleW
	}
	// （原第 3 因子「成功率 EMA ×3」已删，success-ema-review §4：双饱和计数器对
	// 真实成功率不敏感、成熟池退化为与默认值重合的常数 1.5、失败侧与熔断器
	// 100% 同源——机制名存实亡且冗余，删除后 weightOf 为三因子。）
	return w
}
