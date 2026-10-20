// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 一笔结算的**单事务落地**（v0.31.60，审查报告 F02 的收尾）。
//
// 为什么单独成文件：钱的四个落点（用户余额 / 用量流水 / 号主分成 / 上级返点）
// 此前是四条独立语句、四次提交，任何一次失败都会留下"账只记了一半"的中间态。
// 审查用 SQLite trigger 复现过三种半截账：
//
//	① 扣费成功、流水写失败 → 用户被扣钱但报表里这笔消费不存在；
//	② 扣费成功、分成写失败 → 号主少收钱，且没有任何记录能事后补；
//	③ 分成成功、返点写失败 → 上级少收钱（同上）。
//
// 事务化之后这三种都不存在：要么四个落点全在，要么一条都没写、整笔返回错误
// （由 billing 侧改成"钱没动 + 补一条零费流水留痕"）。
package main

import (
	"database/sql"

	"workbuddy2api/internal/billing"
	"workbuddy2api/internal/usage"
)

// CommitSettlement 实现 billing.Ledger：同事务落地一笔结算的全部写入。
//
// 纪律：**本函数不做任何判断**（该不该扣、该不该发钱一律由 internal/billing 决定）。
// 它只负责"一起提交或一起放弃"——判断留在这里就会变成两处口径。
func (s *Store) CommitSettlement(st billing.Settlement) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	switch {
	case st.HasReservation:
		// 预扣对账：退还冻结额、扣实际消耗（净额即多退少补）。
		if _, err := settleReservationTx(tx, st.ReservationID, st.ChargedMicro); err != nil {
			return err
		}
	case st.ChargedMicro > 0:
		// 无预扣（池内还没有单价数据）→ 退化为事后结算。
		if _, err := chargeUserTx(tx, st.UserID, st.ChargedMicro); err != nil {
			return err
		}
	}

	// 流水：使用与独立写入同一段 SQL（insertUsageLogSQL），避免两处字段漂移。
	log := st.Log
	if _, err := tx.Exec(insertUsageLogSQL, usageLogArgs(&log)...); err != nil {
		return err
	}

	if h := st.Host; h != nil {
		// 分成计**可提现额度**（v0.21.0：号池收益可提现）。
		if err := creditWithdrawableTx(tx, h.OwnerUserID, h.EarnedMicro); err != nil {
			return err
		}
		if err := addAccountEarnedTx(tx, h.AccountUID, h.EarnedMicro); err != nil {
			return err
		}
		if err := insertHostEarningTx(tx, &HostEarning{
			AccountUID:  h.AccountUID,
			OwnerUserID: h.OwnerUserID,
			FromUserID:  h.FromUserID,
			Model:       h.Model,
			Charged:     h.ChargedMicro,
			Earned:      h.EarnedMicro,
		}); err != nil {
			return err
		}
	}

	if r := st.Referral; r != nil {
		if err := creditWithdrawableTx(tx, r.Beneficiary, r.RebateMicro); err != nil {
			return err
		}
		if err := insertRebateTx(tx, &Rebate{
			Kind:        RebateReferralPool,
			Beneficiary: r.Beneficiary,
			FromUserID:  r.FromUserID,
			AccountUID:  r.AccountUID,
			BaseMicro:   r.BaseMicro,
			RebateMicro: r.RebateMicro,
			Note:        r.Note,
		}); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// 编译期断言：失败台账/流水所需的接口形态没被改坏。
var _ interface {
	CommitSettlement(billing.Settlement) error
	InsertUsageLog(*usage.UsageLog) error
} = (*Store)(nil)

// 防御：execer 必须同时被 DB 与 Tx 满足（store_user.go 定义）。
var (
	_ execer = (*sql.DB)(nil)
	_ execer = (*sql.Tx)(nil)
)
