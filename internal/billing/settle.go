// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 结算（预扣对账 / 扣费 / 写流水）的**编排**。
//
// 为什么单独成文件（v0.31.11，P2-7 第三刀下半 3b）：
//
//	上半（v0.31.10）把"钱怎么算"收成了纯函数，但它们仍然**内联在转发主循环同一个文件里**——
//	要回答"这一笔钱到底怎么记的"，还是得读懂转发流程。本步把**记账的顺序与分支**也搬出来：
//	预扣对账 → 扣费/退还 → 写流水 → （交给调用方触发分成）。
//
// 边界（刻意的）：
//   - 本文件**不碰 Proxy 状态、不碰选号、不发网络**；所有 IO 通过 Ledger 接口注入；
//   - 分成/返点**不在这里**：它需要"这个号归谁、是否已入池"这类选号侧状态，
//     留在 cmd/webapp（公式本身早已在 billing.HostEarn / ReferralRebate）。
//     本文件只通过 Result.HostCreditDue 告诉调用方"该给号主分成了"，以及分成需要的两个数。
//   - 返回值 Result 带回**可能被追加过标记**的 ErrMsg：流水里的 error_msg 是事后对账的唯一线索
//     （[missing_credit] / [host_self] / [settle_failed:…] 这些标记不能在搬移中丢掉）。
package billing

import (
	"log"
	"strings"

	"workbuddy2api/internal/usage"
)

// 流水 scope 与失败阶段。**值与 cmd/webapp 的同名常量必须一致** ——
// 那边是别名（`= billing.X`），所以只有这一份定义。
//
// 为什么放在这里：结算是唯一写这些值的地方；两处各写一份字符串字面量，
// 迟早会出现"一边写 pool 一边查 platform"这种查不到数据的排查地狱。
const (
	ScopePool = "pool" // 走平台号池（平台计费 Key）
	ScopeHost = "host" // 走号主自己的代挂账号（代挂自用 Key，免费）

	// KeyKindHost 代挂自用 Key 的类型标识（与 ScopeHost 同值但语义不同：
	// 一个是"这把 Key 是什么"，一个是"这条流水算哪类消费"，不要互相替换）。
	KeyKindHost = "host"

	StageSettle = "settle"
	StageCharge = "charge"
)

// Usage 一次请求的上游用量（预扣 / 结算 / 流水共用的唯一结构）。
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	Credit           float64
	HasCredit        bool

	// 前缀缓存观测（v0.26.7）：上游在末帧 usage 里**明确回报**的三个计数。
	//
	// 为什么必须采（这是一次真实排查逼出来的）：上游对同一前缀有缓存，
	// 命中时预处理几乎免费、未命中则要全量重算 —— 实测本机 48 万 token 的请求
	// 单次耗时 92~129 秒、计 24~25 点，但按缓存命中口径该低一个数量级。
	// 旧网关（internal/server/logging.go，已于 v0.31.5 删除）一直在采这三个字段并有命中率统计，
	// **只有 webapp 这条链路把它们丢了** —— 于是"缓存到底有没有生效"根本无从判断，
	// 只能对着耗时和费用猜。观测缺口本身就是缺陷。
	//
	// 口径：hit + miss 才是本次 prompt 的全量（write 是本次**新写入**缓存的量）。
	// 三者都为 0 = 上游没返回（老版本 / 非缓存模型），**不等于命中率为 0**。
	CacheHitTokens   int
	CacheMissTokens  int
	CacheWriteTokens int

	// LatencyMS 本次请求的端到端耗时（毫秒），由转发层在结算前填入。
	//
	// 为什么不从上游取：上游只给处理时长，用户感知的是"从发出到收完"的整体，
	// 含排队、换号重试、反代回程。此前它只存在于 90 分钟的内存态实时面板里，
	// 进程一重启就没，无法做"最近一天 p95 是多少"这类判断。
	LatencyMS int64
}

// Total 总 token 数。
func (u Usage) Total() int { return u.PromptTokens + u.CompletionTokens }

// Ledger 结算需要的**账本写入面**。由 cmd/webapp 的 Store 适配实现。
//
// 只列真正用到的 5 个方法：接口越窄，测试就越好造假对象，也越不容易把
// "结算"偷偷扩大成"什么都干"。
type Ledger interface {
	PricingMultiplier() float64
	SettleReservation(id int64, chargedMicro int64) (int64, error)
	ChargeUser(userID int64, micro int64) (int64, error)
	InsertUsageLog(*usage.UsageLog) error
	// RecordConsumeFailure 记一条**账目类**失败（结算/扣费失败）。
	// 用散字段而不是结构体：失败台账的结构体属于展示/查询层，不该被结算反向依赖。
	RecordConsumeFailure(stage, source, model, uid string, userID, keyID int64, attempt int, detail string)
}

// Estimator 估算缺失 credit 的钩子（单价来源与观测表都在调用方）。
// nil = 不估算（等价于"无单价"）。
type Estimator func(upstreamUID, model string, tokens int) float64

// CostObserver 记一次实测单价（含持久化）。nil = 不记。
type CostObserver func(model string, per1k float64)

// Settler 结算器：把 Request 落成账。零值不可用（Ledger 必填）。
type Settler struct {
	Ledger      Ledger
	Estimate    Estimator
	ObserveCost CostObserver
}

// Request 一次结算的输入。
//
// 用**显式布尔**而不是"看某个字段是否为零值"来表达"有没有"：
// `UserID == 0` 恰好也等于管理通道，但那是巧合（真实用户 ID 从 1 起），
// 靠巧合的判断方式在将来引入"系统用户 ID=0"时就会静默走错分支。
type Request struct {
	// HasUser=false 表示管理密钥通道（运维 / 后台直连）：不经用户计费，
	// 但**必须进号池台账**（见 Settle 内的注释：这是 v0.17.6 修过的真实缺陷）。
	HasUser bool

	UserID int64
	KeyID  int64
	// KeyKind = KeyKindHost 时本次免费（不扣余额、不走预扣），仅记零费用流水。
	KeyKind string

	Model       string
	UpstreamUID string
	Usage       Usage

	// HasReservation=true 表示这笔请求已经预扣过（ReservationID 必填）：
	// 结算走"退还冻结额 + 扣实际消耗"（净额即多退少补），而不是直接扣余额。
	HasReservation bool
	ReservationID  int64

	Status     string // "ok" / 失败分类文案
	ErrMsg     string
	StickyHash string
	Attempts   int
}

// Result 一次结算的输出。
type Result struct {
	// Settled 表示预扣**已被处理**。false = 结算异常，调用方必须走释放路径把钱退回，
	// 否则预扣会悬空（v0.26.2 那笔孤儿预扣就是这么来的）。
	Settled bool
	// ChargedMicro 本次真正记账的扣费（微点），供实时态势展示。
	ChargedMicro int64
	// ErrMsg 可能被追加了标记（[missing_credit] / [host_self] / [settle_failed:…] /
	// [charge_failed:…]）。调用方必须把它写进流水 —— 这些标记是事后对账的唯一线索。
	ErrMsg string
	// Scope 流水的消费归类（ScopePool / ScopeHost）。
	Scope string
	// ConsumedCredit 本次号池积分消耗（上游 direct 口径，估不出时按 0）。
	// 分成/返点以它为基数（不是平台实收），故必须回传。
	ConsumedCredit float64
	// HostCreditDue=true 表示调用方应当触发"号主分成"（本次非免费、且有积分消耗）。
	// 判定放在这里是因为它依赖 hostSelf 与 consumedCredit，两者都在本函数内确定；
	// 但**执行**留给调用方 —— 它需要选号侧状态（号主是谁 / 是否已入池）。
	HostCreditDue bool
}

// Settle 结算一次请求：算钱 → 与预扣对账 → 写流水。
//
// 计费口径（v0.12.0，需求 7 明确）：**唯一基准 = 本次被消耗的号池账号积分 × 倍率**。
//
//	用户扣费（点）= 号池积分消耗 × 计费倍率（倍率由管理员在「设置 → 计费」配置，单位：点 / 1 积分）
//
// 上游没回报 credit 时（老版本 / 异常响应）才用池里已学的「账号+模型」实测单价 × token 数估算，
// 仍估不出则按 0 计并标记 missing_credit 便于对账 —— **不猜**：猜就是凭空扣用户的钱。
func (s Settler) Settle(req Request) Result {
	if !req.HasUser {
		return s.settleAdminChannel(req)
	}

	res := Result{ErrMsg: req.ErrMsg, Scope: ScopePool}
	mult := s.Ledger.PricingMultiplier()

	// consumedCredit = 号池账号本次被消耗的积分（上游 direct 口径）。
	consumedCredit := req.Usage.Credit
	if !req.Usage.HasCredit {
		// 只对成功响应做回落估算：失败响应没有真实用量（token=0），
		// 强行估就是凭空扣费。失败一律按 0 计。
		if req.Status == "ok" && s.Estimate != nil {
			consumedCredit = s.Estimate(req.UpstreamUID, req.Model, req.Usage.Total())
		} else {
			consumedCredit = 0
		}
		if consumedCredit <= 0 && req.Status == "ok" {
			res.ErrMsg = appendMark(res.ErrMsg, "[missing_credit]")
		}
	}
	// 记录实测单价（积分 / 1k token），供后续请求的预扣估算使用
	// （调用方取历史最大值并持久化 —— 那两步在 Costs / kv，不在这里）。
	if per1k := Per1kCost(consumedCredit, req.Usage.Total()); per1k > 0 && s.ObserveCost != nil {
		s.ObserveCost(req.Model, per1k)
	}
	// 扣费 = 号池积分消耗 × 倍率。倍率即「1 号池积分折多少点」。
	chargedMicro := PointToMicro(consumedCredit * mult)

	// 代挂自用 Key：免费（不扣余额、不走预扣），仅记一条零费用流水便于观测/对账。
	hostSelf := req.KeyKind == KeyKindHost
	if hostSelf {
		chargedMicro = 0
		res.Scope = ScopeHost
		res.ErrMsg = appendMark(res.ErrMsg, "[host_self]")
	}

	res.Settled = true
	// chargeCommitted 本次扣费是否**真的提交到了账本**（v0.31.59，审查 F02）。
	//
	// 关键不变量：**只有已提交的实扣才能驱动分成**。
	// 修复前的行为：SettleReservation 失败时虽然 Settled=false，但函数末尾仍然把
	// 名义 chargedMicro 写回 Result，并把 HostCreditDue 置为 true —— 调用方据此
	// 给号主发了可提现收益，而消费者的钱其实一分没扣（预扣随后被释放）。
	// 审查用临时 SQLite trigger 让 reservation 删除失败复现了这条路径：
	// settled=false、reportedCharged=5、号主余额 +3。
	chargeCommitted := hostSelf // 代挂自用：零费用，视为"无需扣费"的已提交状态
	if !hostSelf {
		switch {
		case req.HasReservation:
			// 预扣-结算：退还冻结额、扣实际消耗，净额即多退少补。
			if _, err := s.Ledger.SettleReservation(req.ReservationID, chargedMicro); err != nil {
				res.ErrMsg = appendMark(res.ErrMsg, "[settle_failed:"+err.Error()+"]")
				res.Settled = false // 交给调用方 defer 释放，避免预扣悬空
				// 结算失败是**账目类**失败：响应已经发出，钱的方向对用户有利，
				// 但必须留痕，否则"账对不上"时无从追查（v0.26.2 那笔孤儿预扣
				// 就是靠 usage_logs 的 error_msg 才定位到的；结构化之后不必再翻文本）。
				s.recordFailure(StageSettle, "settle", req, "预扣结算失败："+err.Error())
			} else {
				chargeCommitted = true
			}
		case chargedMicro > 0:
			// 无预扣（池内还没有单价数据）→ 退化为事后结算。
			if _, err := s.Ledger.ChargeUser(req.UserID, chargedMicro); err != nil {
				// 扣费失败不阻断响应（响应已发出），但要留痕。
				// ⚠️ 必须同时把 Settled 置 false：没有预扣可释放，"未结算"的语义就是
				// 这笔钱没有扣成功。此前这里只加标记、Settled 保持 true，调用方据此
				// 认为账已两清并发分成 —— 属于同一条缺陷的第二个入口。
				res.ErrMsg = appendMark(res.ErrMsg, "[charge_failed:"+err.Error()+"]")
				res.Settled = false
				s.recordFailure(StageCharge, "charge", req, "事后扣费失败："+err.Error())
			} else {
				chargeCommitted = true
			}
		default:
			// chargedMicro == 0：没有需要扣的钱（上游缺 credit / 纯缓存命中），
			// 视为已提交（0 元扣费），分成仍按 consumedCredit 判断。
			chargeCommitted = true
		}
	}

	// 流水是事后对账的唯一依据：写失败必须留日志，不能 `_ =` 静默丢弃（审查 F02）。
	if err := s.Ledger.InsertUsageLog(&usage.UsageLog{
		UserID:           req.UserID,
		KeyID:            req.KeyID,
		Model:            req.Model,
		PromptTokens:     req.Usage.PromptTokens,
		CompletionTokens: req.Usage.CompletionTokens,
		UpstreamCredit:   consumedCredit,
		ChargedMicro:     chargedMicro,
		UpstreamUID:      req.UpstreamUID,
		Scope:            res.Scope,
		Status:           req.Status,
		ErrorMsg:         res.ErrMsg,
		StickyKeyHash:    req.StickyHash,
		Attempts:         req.Attempts,
		CacheHitTokens:   req.Usage.CacheHitTokens,
		CacheMissTokens:  req.Usage.CacheMissTokens,
		CacheWriteTokens: req.Usage.CacheWriteTokens,
		LatencyMS:        req.Usage.LatencyMS,
	}); err != nil {
		log.Printf("WARN: [billing] 写用量流水失败（本次 charged=%d acct=%s）：%v", chargedMicro, req.UpstreamUID, err)
	}

	// 只回传**已提交**的实扣。未提交时为 0：既不让实时态势显示"扣了 5 点"，
	// 也不让分成按一个不存在的收入去发钱。
	if chargeCommitted {
		res.ChargedMicro = chargedMicro
	} else {
		res.ChargedMicro = 0
	}
	res.ConsumedCredit = consumedCredit
	// 代挂分成：本次若由某号主的「已入池」代挂账号服务平台流量，
	// 按「号池积分消耗 × 分成比例」折入号主余额（以本次实收封顶）—— 由调用方执行。
	// 判定必须同时满足：非免费 + 有积分消耗 + **扣费已提交**。
	// 只有已提交的实扣才构成"平台收入"，也才谈得上分成（审查 F02）。
	res.HostCreditDue = !hostSelf && consumedCredit > 0 && chargeCommitted
	return res
}

// settleAdminChannel 管理密钥通道（无用户可扣）。
//
// 此前这里在调用方是"直接 return" —— 于是用管理密钥发的每一次请求在 usage_logs 里都不存在。
// 后果很具体：号池仪表盘的「累计服务 / 号池消耗 Top」对自己人的自测流量**视而不见**，
// 排查的人一边发请求一边看仪表盘纹丝不动，很容易误判「仪表盘坏了」。
// 「哪个号服务了这次请求、消耗了多少号池积分」是号池事实，与向谁收费无关，故照记。
// user_id 记 0：用户侧账单（按 user_id 过滤）天然看不到它，只有全站报表会统计。
//
// 与主路径的区别只有两点：不扣任何用户余额（无用户可扣）、不做预扣对账（管理通道本就不预扣）。
// 其余（consumedCredit 口径、单价观测、流水字段）与计费路径**完全一致** —— 台账要能横向比较。
func (s Settler) settleAdminChannel(req Request) Result {
	res := Result{ErrMsg: req.ErrMsg, Scope: ScopePool, Settled: true}

	consumedCredit := req.Usage.Credit
	if !req.Usage.HasCredit {
		if req.Status == "ok" && s.Estimate != nil {
			consumedCredit = s.Estimate(req.UpstreamUID, req.Model, req.Usage.Total())
		} else {
			consumedCredit = 0
		}
	}
	// 单价观测与计费路径同口径（只认真实用量），否则管理通道的请求白白浪费一次学习机会。
	if per1k := Per1kCost(consumedCredit, req.Usage.Total()); per1k > 0 && s.ObserveCost != nil {
		s.ObserveCost(req.Model, per1k)
	}
	_ = s.Ledger.InsertUsageLog(&usage.UsageLog{
		UserID:           0, // 0 = 无归属（运维通道）；用户侧按 user_id 过滤，天然隔离
		KeyID:            req.KeyID,
		Model:            req.Model,
		PromptTokens:     req.Usage.PromptTokens,
		CompletionTokens: req.Usage.CompletionTokens,
		UpstreamCredit:   consumedCredit,
		ChargedMicro:     0, // 管理通道不收费
		UpstreamUID:      req.UpstreamUID,
		Scope:            ScopePool,
		Status:           req.Status,
		ErrorMsg:         res.ErrMsg,
		StickyKeyHash:    req.StickyHash,
		Attempts:         req.Attempts,
		CacheHitTokens:   req.Usage.CacheHitTokens,
		CacheMissTokens:  req.Usage.CacheMissTokens,
		CacheWriteTokens: req.Usage.CacheWriteTokens,
		LatencyMS:        req.Usage.LatencyMS,
	})
	res.ConsumedCredit = consumedCredit
	return res
}

func (s Settler) recordFailure(stage, source string, req Request, detail string) {
	s.Ledger.RecordConsumeFailure(stage, source, req.Model, req.UpstreamUID,
		req.UserID, req.KeyID, req.Attempts, detail)
}

// appendMark 追加一个流水标记（与原有写法逐字节等价：`TrimSpace(s + " [x]")`）。
// 抽成函数只为了让"多个标记按什么顺序追加"在一处可见 —— 那个顺序是运维读流水的依据。
func appendMark(s, mark string) string { return strings.TrimSpace(s + " " + mark) }
