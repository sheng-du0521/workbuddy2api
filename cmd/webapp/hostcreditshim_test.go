// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// **仅测试用**：`(*Proxy).creditHostOwner` 的等价入口（v0.31.60）。
//
// 为什么需要它：生产路径上"发分成"已经不再是独立函数（它并入
// billing.Settle → Store.CommitSettlement 的同一个事务），但既有用例（E8/E9/E10、
// billing_basis、money、host）需要**单独**验证"分成/返点的资格与金额"，
// 不必每次都造一遍预扣、倍率与 token 用量。
//
// 它不是第二套实现：资格走生产的 hostShareFor，金额走生产的 billing.PlanHostCredit，
// 入账走生产的 Store.CommitSettlement —— 三者都只有一份。
// 放在 `_test.go` 里是硬约束：**生产二进制里不存在这个入口**，不会有人顺手在转发路径上再调它。
package main

import (
	"workbuddy2api/internal/billing"
	"workbuddy2api/internal/usage"
)

// creditHostOwner 只发钱、不扣费：给测试用，语义与旧生产函数一致。
func (p *Proxy) creditHostOwner(uid string, from *User, model string, consumedCredit float64, chargedMicro int64) {
	var fromID int64
	if from != nil {
		fromID = from.ID
	}
	hc, ref := billing.PlanHostCredit(p.hostShareFor(uid), uid, model, fromID, consumedCredit, chargedMicro)
	if hc == nil && ref == nil {
		return
	}
	st := billing.Settlement{
		Host:     hc,
		Referral: ref,
		Log: usage.UsageLog{
			UserID:         fromID,
			Model:          model,
			UpstreamUID:    uid,
			UpstreamCredit: consumedCredit,
			Status:         "ok",
		},
	}
	if err := p.store.CommitSettlement(st); err != nil {
		panic("creditHostOwner(test): " + err.Error())
	}
}
