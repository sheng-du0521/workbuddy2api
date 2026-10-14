// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"log"
	"math"
	"net/http"
	"strconv"
)

// ======================================================================
// 用户返点（v0.10.0）
//
// 两类返点：
//   1. 号池返点（自己的账号进池赚收益）—— 由 proxy.creditHostOwner 按 host_share_ratio
//      折算入号主余额；此处只负责展示，不重复入账。
//   2. 推广返点（邀请下级）—— 上级按下级的行为拿返点，由 referral_ratio 控制，两种触发：
//        · 下级兑换充值码（rebate.kind = referral_recharge）
//        · 下级的代挂账号在号池赚到收益（rebate.kind = referral_pool）
// ======================================================================

// kvReferralRatio 推广返点比例（0~1，0 = 关闭）。见 Proxy.referralRatioNow。
const kvReferralRatio = "referral_ratio"

// handleReferral 我的返点总览：邀请码 / 邀请链接 / 下级数 / 两类返点汇总 + 流水。
//
// 展示口径（v0.17.7）：**返点一律以「钱」（元）呈现**，点数只作换算附注。
// 因此每处返点金额都同时返回 `*_cny`；`rate`（元/点）供前端显示折算比例。
func (a *app) handleReferral(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	code, err := a.store.EnsureInviteCode(u.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	referred, _ := a.store.CountReferred(u.ID)
	byKind, totalMicro, _ := a.store.RebateSummaryByKind(u.ID)
	poolTotal, poolCount, _ := a.store.HostEarningSummary(u.ID)
	rate := a.moneyRate()

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, _ := a.store.ListRebates(u.ID, limit)
	items := make([]map[string]any, 0, len(rows))
	for _, rb := range rows {
		items = append(items, map[string]any{
			"kind":       rb.Kind,
			"from_user":  rb.FromUserID,
			"account":    rb.AccountUID,
			"base":       MicroToPoint(rb.BaseMicro),
			"base_cny":   microToCNY(rb.BaseMicro, rate),
			"rebate":     MicroToPoint(rb.RebateMicro),
			"rebate_cny": microToCNY(rb.RebateMicro, rate),
			"note":       rb.Note,
			"created_at": rb.CreatedAt,
		})
	}

	// 本人生效的返点比例（v0.17.7）：覆盖面「单独设过」与「跟随全局」两种状态。
	fresh, ferr := a.store.GetUserByID(u.ID)
	if ferr != nil {
		fresh = u
	}
	poolView := newRatioOverrideView(fresh, func(x *User) float64 { return x.HostRatio },
		a.proxy.hostShareRatioNow())
	refView := newRatioOverrideView(fresh, func(x *User) float64 { return x.ReferralRatio },
		a.proxy.referralRatioNow())

	writeJSON(w, http.StatusOK, map[string]any{
		"invite_code":    code,
		"invite_link":    a.inviteLink(code),
		"referred_count": referred,
		"rate":           rate,
		"ratios": map[string]any{
			"referral": refView.Effective, // 生效值（本人）
			"pool":     poolView.Effective,
			"global": map[string]any{
				"referral": a.proxy.referralRatioNow(),
				"pool":     a.proxy.hostShareRatioNow(),
			},
			// 明细：让用户看出比例是「单独设的」还是「跟随全局」。
			"referral_detail": refView,
			"pool_detail":     poolView,
		},
		"summary": map[string]any{
			"referral_recharge":     MicroToPoint(byKind[RebateReferralRecharge]),
			"referral_recharge_cny": microToCNY(byKind[RebateReferralRecharge], rate),
			"referral_pool":         MicroToPoint(byKind[RebateReferralPool]),
			"referral_pool_cny":     microToCNY(byKind[RebateReferralPool], rate),
			"referral_total":        MicroToPoint(totalMicro),
			"referral_total_cny":    microToCNY(totalMicro, rate),
			"pool_total":            MicroToPoint(poolTotal),
			"pool_total_cny":        microToCNY(poolTotal, rate),
			"pool_count":            poolCount,
		},
		"rows": items,
	})
}

// inviteLink 生成邀请链接（相对路径，前端用 location.origin 拼绝对地址）。
func (a *app) inviteLink(code string) string {
	return "/?ref=" + code + "#register"
}

// creditReferralOnRecharge 下级兑换充值码后，给其上级按推广返点比例入账。
// 失败只记日志：返点是激励，不应影响下级的充值到账。
func (a *app) creditReferralOnRecharge(u *User, creditsMicro int64) {
	if u == nil || u.ReferredBy <= 0 || u.ReferredBy == u.ID || creditsMicro <= 0 {
		return
	}
	// 比例按**收益人（上级）**解析（v0.17.7），理由见 creditReferrerOfPool。
	ratio := a.proxy.referralRatioFor(u.ReferredBy)
	if ratio <= 0 {
		return
	}
	rebate := int64(math.Round(float64(creditsMicro) * ratio))
	if rebate <= 0 {
		return
	}
	// 下级充值产生的推广返点计**可提现额度**（v0.21.0）。
	if err := a.store.CreditWithdrawable(u.ReferredBy, rebate); err != nil {
		log.Printf("referral recharge rebate failed (referrer=%d from=%d): %v", u.ReferredBy, u.ID, err)
		return
	}
	_ = a.store.InsertRebate(&Rebate{
		Kind:        RebateReferralRecharge,
		Beneficiary: u.ReferredBy,
		FromUserID:  u.ID,
		BaseMicro:   creditsMicro,
		RebateMicro: rebate,
		Note:        "充值返点",
	})
}
