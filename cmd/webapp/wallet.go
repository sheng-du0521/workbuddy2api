// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ======================================================================
// 充值 / 提现（v0.11.0）
//
// 控制台「充值提现」页：
//   · 充值 = 兑换码兑换（沿用既有路径）+ 本人充值记录；
//   · 提现 = 发起申请 → 管理员审核 →（通过时）扣余额并记打款。
//
// 记账语义：点为站内内部单位，非真实货币；提现仅落台账与打款记录，
// 不接真实支付通道。兑换比例（元/点）与最小提现额均后台可配。
// ======================================================================

const (
	// kvWithdrawRate 提现兑换比例：元 / 点（默认 0.01，即 100 点 = 1 元）。
	kvWithdrawRate = "withdraw_rate"
	// kvWithdrawMinPoints 最小提现额（点，默认 100）。
	kvWithdrawMinPoints = "withdraw_min_points"
)

// 提现默认参数。
const (
	defaultWithdrawRate      = 0.01
	defaultWithdrawMinPoints = 100.0
)

// withdrawParams 读取提现参数（比例与最小额），非法值回落默认。
func (a *app) withdrawParams() (rate, minPoints float64) {
	rate, minPoints = defaultWithdrawRate, defaultWithdrawMinPoints
	if v, _ := a.store.GetKV(kvWithdrawRate); strings.TrimSpace(v) != "" {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f > 0 {
			rate = f
		}
	}
	if v, _ := a.store.GetKV(kvWithdrawMinPoints); strings.TrimSpace(v) != "" {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f > 0 {
			minPoints = f
		}
	}
	return
}

// maskAccountInfo 收款账号打码（仅保留首尾，避免列表里明文暴露）。
func maskAccountInfo(s string) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= 4 {
		if len(r) == 0 {
			return ""
		}
		return strings.Repeat("*", len(r))
	}
	return string(r[:2]) + strings.Repeat("*", len(r)-4) + string(r[len(r)-2:])
}

// withdrawView 提现记录对外视图（v0.21.0：仅支付宝，字段为「账号」+「姓名」）。
//
// 展示策略：用户侧对支付宝账号打码（列表里不明文暴露）；姓名只留姓（首字）+ 星号。
// 管理员侧的明细视图另有明文（见 adminWithdrawRow），用于实际打款。
func withdrawView(w *Withdrawal, rate float64) map[string]any {
	acct, name := withdrawalPayee(w)
	return map[string]any{
		"id":             w.ID,
		"user_id":        w.UserID,
		"amount":         MicroToPoint(w.AmountMicro),
		"fee":            MicroToPoint(w.FeeMicro),
		"cny":            MicroToPoint(w.AmountMicro) * rate,
		"method":         normalizeWithdrawMethod(w.Method),
		"method_label":   withdrawMethodLabel(w.Method),
		"alipay_account": maskAccountInfo(acct),
		"real_name":      maskRealName(name),
		"account_info":   maskAccountInfo(acct), // 兼容旧字段名
		"status":         w.Status,
		"note":           w.Note,
		"reviewed_at":    w.ReviewedAt,
		"created_at":     w.CreatedAt,
	}
}

// withdrawMethodAlipay 唯一支持的提现方式（v0.21.0：仅支付宝）。
const withdrawMethodAlipay = "alipay"

// normalizeWithdrawMethod 规范化提现方式。历史数据里可能存着「微信/银行卡」等
// 自由文本 —— 一律回落到支付宝（本版本只有这一种方式），避免前端出现未知选项。
func normalizeWithdrawMethod(m string) string {
	return withdrawMethodAlipay
}

// withdrawMethodLabel 提现方式的中文名（恒「支付宝」）。
func withdrawMethodLabel(string) string { return "支付宝" }

// withdrawalPayee 取提现记录的收款方（账号, 姓名）。
// 优先用结构化字段；老数据只有 account_info（自由文本）→ 原样当账号展示。
func withdrawalPayee(w *Withdrawal) (account, name string) {
	if strings.TrimSpace(w.AlipayAccount) != "" || strings.TrimSpace(w.RealName) != "" {
		return w.AlipayAccount, w.RealName
	}
	return w.AccountInfo, ""
}

// maskRealName 姓名打码：保留首字（中文习惯），其余星号。
func maskRealName(name string) string {
	r := []rune(strings.TrimSpace(name))
	if len(r) == 0 {
		return ""
	}
	if len(r) == 1 {
		return string(r)
	}
	return string(r[0]) + strings.Repeat("*", len(r)-1)
}

// handleWallet 控制台「充值提现」页数据：余额 / 参数 / 充值记录 / 提现记录与汇总。
func (a *app) handleWallet(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	rate, minPoints := a.withdrawParams()
	fresh, err := a.store.GetUserByID(u.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	redeems, err := a.store.ListRedeemCodesByUser(u.ID, 100)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	rechargeRows := make([]map[string]any, 0, len(redeems))
	for _, c := range redeems {
		rechargeRows = append(rechargeRows, map[string]any{
			"code":    maskRedeemCode(c.Code),
			"credits": MicroToPoint(c.CreditsMicro),
			"kind":    normalizeRedeemKind(c.Kind),
			"kind_label": map[string]string{
				RedeemKindExp:      "体验",
				RedeemKindRecharge: "充值",
			}[normalizeRedeemKind(c.Kind)],
			// 可提现标记：两类兑换码都不可提现（前端据此提示）。
			"withdrawable": false,
			"added_at":     c.UsedAt,
		})
	}
	withdrawals, err := a.store.ListWithdrawalsByUser(u.ID, 100)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	wdRows := make([]map[string]any, 0, len(withdrawals))
	for _, x := range withdrawals {
		wdRows = append(wdRows, withdrawView(x, rate))
	}
	pending, approved, rejected, count, err := a.store.WithdrawalSummaryByUser(u.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	pendingApply, _ := a.store.PendingWithdrawMicro(u.ID)
	// 可提现额度 = min(额度桶, 余额) − 待审申请（v0.21.0，见 store.WithdrawableQuota）。
	quota, rawQuota, _ := a.store.WithdrawableQuota(u.ID)
	// 汇总各口径的金额形态（v0.17.7）：提现页是「点 / 钱」并存的主要界面，
	// 每处点数都必须带上金额，避免用户在两套口径间自己心算。
	writeJSON(w, http.StatusOK, map[string]any{
		"balance":     MicroToPoint(fresh.BalanceMicro),
		"balance_cny": microToCNY(fresh.BalanceMicro, rate),
		// 分桶（v0.21.0）：两类累计点数（不可提现）+ 可提现额度。
		"exp_points":      MicroToPoint(fresh.ExpMicro),
		"recharge_points": MicroToPoint(fresh.RechargeMicro),
		"params": map[string]any{
			"rate":       rate,
			"min_points": minPoints,
			"min_cny":    pointsToCNY(minPoints, rate),
			"rate_hint":  "1 元 ≈ " + strconv.FormatFloat(1/rate, 'f', 0, 64) + " 点",
		},
		"recharges":   rechargeRows,
		"withdrawals": wdRows,
		"summary": map[string]any{
			"pending":           MicroToPoint(pending),
			"pending_cny":       microToCNY(pending, rate),
			"approved":          MicroToPoint(approved),
			"approved_cny":      microToCNY(approved, rate),
			"rejected":          MicroToPoint(rejected),
			"rejected_cny":      microToCNY(rejected, rate),
			"count":             count,
			"pending_apply":     MicroToPoint(pendingApply),
			"pending_apply_cny": microToCNY(pendingApply, rate),
			// 可提现额度（v0.21.0）：仅返点收益 + 账号买卖所得；已扣待审申请。
			"withdrawable":     MicroToPoint(quota),
			"withdrawable_cny": microToCNY(quota, rate),
			// 额度原值（未扣待审），供界面解释「为什么可提比额度小」。
			"withdrawable_raw": MicroToPoint(rawQuota),
		},
	})
}

// maskRedeemCode 充值码打码（保留前缀与末段，便于本人识别）。
func maskRedeemCode(code string) string {
	code = strings.TrimSpace(code)
	if len(code) <= 8 {
		return code
	}
	return code[:6] + "…" + code[len(code)-4:]
}

// handleWithdrawApply 用户提交提现申请。
func (a *app) handleWithdrawApply(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	var req struct {
		Amount float64 `json:"amount"` // 点数
		// AlipayAccount / RealName 支付宝账号与姓名（v0.21.0）。
		// 兼容入参：account_info（旧字段名）与 target（前端历史别名）。
		AlipayAccount string `json:"alipay_account"`
		RealName      string `json:"real_name"`
		AccountInfo   string `json:"account_info"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	acct := strings.TrimSpace(req.AlipayAccount)
	if acct == "" {
		acct = strings.TrimSpace(req.AccountInfo) // 兼容旧字段名
	}
	name := strings.TrimSpace(req.RealName)
	if acct == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请填写支付宝账号"})
		return
	}
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请填写姓名"})
		return
	}
	// 字段长度校验：防止超长串灌库 / 展示错乱。
	if len([]rune(acct)) > 100 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "支付宝账号过长（最多 100 字）"})
		return
	}
	if len([]rune(name)) > 32 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "姓名过长（最多 32 字）"})
		return
	}
	if req.Amount <= 0 || math.IsNaN(req.Amount) || math.IsInf(req.Amount, 0) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "提现点数需大于 0"})
		return
	}
	rate, minPoints := a.withdrawParams()
	if req.Amount < minPoints {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("最小提现 %.0f 点", minPoints)})
		return
	}
	amountMicro := PointToMicro(req.Amount)
	// 可提现额度 = min(可提现额度桶, 余额) − 待审申请（v0.21.0）。
	// 只有返点收益与账号买卖所得进入该额度，体验点 / 充值点不可提现。
	quota, rawQuota, err := a.store.WithdrawableQuota(u.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if quota < amountMicro {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": fmt.Sprintf("可提现额度不足：可提 %.2f 点（仅返点收益与账号买卖所得可提现），当前申请 %.2f 点",
				MicroToPoint(quota), req.Amount),
			"withdrawable": MicroToPoint(quota),
			"raw":          MicroToPoint(rawQuota),
		})
		return
	}
	row := &Withdrawal{
		UserID:        u.ID,
		AmountMicro:   amountMicro,
		Method:        withdrawMethodAlipay,
		AlipayAccount: acct,
		RealName:      name,
		AccountInfo:   acct + " / " + name, // 兼容字段：保留可读组合串
	}
	if err := a.store.CreateWithdrawal(row); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"withdrawal": withdrawView(row, rate),
		"message":    "提现申请已提交，等待管理员审核",
	})
}

// ---------------------------------------------------------------------
// 管理侧
// ---------------------------------------------------------------------

// adminWithdrawRow 管理侧提现记录（含号主信息，收款账号明文供打款）。
type adminWithdrawRow struct {
	*Withdrawal
	Username    string  `json:"username"`
	AccountInfo string  `json:"account_info"`
	CNY         float64 `json:"cny"`
}

// handleAdminWithdrawals 管理侧提现列表（可按状态过滤）。
func (a *app) handleAdminWithdrawals(w http.ResponseWriter, r *http.Request) {
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	rate, minPoints := a.withdrawParams()
	rows, err := a.store.ListWithdrawals(status, 200)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	users, _ := a.store.ListUsers()
	nameOf := map[int64]string{}
	for _, u := range users {
		nameOf[u.ID] = u.Username
	}
	out := make([]adminWithdrawRow, 0, len(rows))
	for _, x := range rows {
		out = append(out, adminWithdrawRow{
			Withdrawal:  x,
			Username:    nameOf[x.UserID],
			AccountInfo: x.AccountInfo,
			CNY:         MicroToPoint(x.AmountMicro) * rate,
		})
	}
	// 待审计数（前端角标用）。
	pendingList, _ := a.store.ListWithdrawals(WithdrawPending, 500)
	writeJSON(w, http.StatusOK, map[string]any{
		"withdrawals":   out,
		"pending_count": len(pendingList),
		"params":        map[string]any{"rate": rate, "min_points": minPoints},
	})
}

// handleAdminWithdrawReview 管理员审核提现（approve=true 扣余额打款 / false 驳回）。
func (a *app) handleAdminWithdrawReview(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "非法 id"})
		return
	}
	var req struct {
		Approve bool   `json:"approve"`
		Note    string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	admin := userFrom(r)
	var reviewerID int64
	if admin != nil {
		reviewerID = admin.ID
	}
	row, err := a.store.ReviewWithdrawal(id, req.Approve, strings.TrimSpace(req.Note), reviewerID)
	if err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "提现申请不存在"})
		case errors.Is(err, ErrInsufficientWithdraw):
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "用户余额不足，无法打款（可先驳回）"})
		default:
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		}
		return
	}
	rate, _ := a.withdrawParams()
	writeJSON(w, http.StatusOK, map[string]any{
		"withdrawal":  withdrawView(row, rate),
		"reviewed_at": time.Now().Unix(),
	})
}
