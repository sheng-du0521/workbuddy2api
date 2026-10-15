// 结算编排（internal/billing）与本地 Store 之间的**适配层**（v0.31.11，P2-7 第三刀下半 3b）。
//
// 为什么需要它：`internal/billing` 不能依赖 `cmd/webapp`（包方向是单向的），
// 所以它把"要往账本写什么"声明成 `billing.Ledger` 接口，由这里用 Store 实现。
//
// 适配层的纪律：**只做转发，不做判断**。任何"该不该写"的逻辑都必须在新包里
// （否则同一件事又变成两处实现 —— 本项目最贵的一类缺陷）。
package main

import (
	"workbuddy2api/internal/billing"
	"workbuddy2api/internal/usage"
)

// storeLedger 把 Store 适配成 billing.Ledger。
type storeLedger struct{ s *Store }

func (l storeLedger) PricingMultiplier() float64 { return l.s.PricingMultiplier() }

func (l storeLedger) SettleReservation(id int64, chargedMicro int64) (int64, error) {
	return l.s.SettleReservation(id, chargedMicro)
}

func (l storeLedger) ChargeUser(userID int64, micro int64) (int64, error) {
	return l.s.ChargeUser(userID, micro)
}

func (l storeLedger) InsertUsageLog(e *usage.UsageLog) error {
	return l.s.InsertUsageLog(e)
}

// RecordConsumeFailure 组装失败台账结构体。
//
// 为什么接口用散字段而不是直接传 ConsumeFailure：失败台账的结构体带 json tag，
// 属于**查询/展示层**（后台页面直接读它），不该被结算逻辑反向依赖。
// 散字段还让新包对"账目失败要记哪些维度"这件事保持无知 —— 它只知道 stage/source/detail。
func (l storeLedger) RecordConsumeFailure(stage, source, model, uid string,
	userID, keyID int64, attempt int, detail string) {
	l.s.recordConsumeFailure(ConsumeFailure{
		Stage: stage, Source: source, Model: model, UID: uid,
		UserID: userID, KeyID: keyID, Attempt: attempt, Detail: detail,
	})
}

// billingSettler 构造结算器。
//
// **每次现构造**（一个 3 字段结构体，栈上分配，开销可忽略），不缓存到 Proxy 字段上：
// 那样就不存在"某个构造路径忘了初始化 → 结算时 nil panic"的失败模式
// （测试里有用 `&Proxy{...}` 字面量直接构造的，走不到 NewProxy）。
func (p *Proxy) billingSettler() billing.Settler {
	return billing.Settler{
		Ledger:      storeLedger{p.store},
		Estimate:    p.estimateCredit, // 单价两级回落（pool 观测 + 本进程观测）留在本包
		ObserveCost: p.observeCost,    // 含 kv 持久化，属于 IO
	}
}
