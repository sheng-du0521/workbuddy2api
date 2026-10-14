// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"path/filepath"
	"testing"
)

// newWalletStore 造一个库（钱包/提现测试复用）。
func newWalletStore(t *testing.T, name string) *Store {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	return store
}

// TestWithdrawApplyApproveDeductsBalance 校验提现申请→审核通过的扣余额闭环。
func TestWithdrawApplyApproveDeductsBalance(t *testing.T) {
	store := newWalletStore(t, "wd.db")
	defer store.Close()

	u, err := store.CreateUser("alice", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	// v0.21.0：提现只允许「可提现额度」（返点收益 / 账号买卖所得）。
	// AddUserBalance 是充值性入账（不可提现），故这里必须用 CreditWithdrawable。
	if err := store.CreditWithdrawable(u.ID, 500*MicroPerPoint); err != nil {
		t.Fatalf("CreditWithdrawable: %v", err)
	}

	w := &Withdrawal{UserID: u.ID, AmountMicro: 200 * MicroPerPoint, Method: "支付宝", AccountInfo: "alice@example.com"}
	if err := store.CreateWithdrawal(w); err != nil {
		t.Fatalf("CreateWithdrawal: %v", err)
	}
	if w.Status != WithdrawPending {
		t.Fatalf("新申请状态应为 pending，得 %q", w.Status)
	}
	// 待审金额应计入 PendingWithdrawMicro。
	if pend, _ := store.PendingWithdrawMicro(u.ID); pend != 200*MicroPerPoint {
		t.Fatalf("待审金额应为 200 点，得 %d 微点", pend)
	}
	// 审核通过：扣余额。
	row, err := store.ReviewWithdrawal(w.ID, true, "已打款", 999)
	if err != nil {
		t.Fatalf("ReviewWithdrawal approve: %v", err)
	}
	if row.Status != WithdrawApproved {
		t.Fatalf("通过后状态应为 approved，得 %q", row.Status)
	}
	fresh, _ := store.GetUserByID(u.ID)
	if fresh.BalanceMicro != 300*MicroPerPoint {
		t.Fatalf("扣款后余额应为 300 点，得 %d 微点", fresh.BalanceMicro)
	}
	if pend, _ := store.PendingWithdrawMicro(u.ID); pend != 0 {
		t.Fatalf("通过后待审金额应清零，得 %d", pend)
	}
	// 重复审核应被拒（已处理）。
	if _, err := store.ReviewWithdrawal(w.ID, true, "", 999); err == nil {
		t.Fatal("对已处理的提现再次审核应报错")
	}
	// 汇总：approved 累计 200。
	_, approved, _, count, _ := store.WithdrawalSummaryByUser(u.ID)
	if approved != 200*MicroPerPoint || count != 1 {
		t.Fatalf("汇总应为 approved=200 点 count=1，得 approved=%d count=%d", approved, count)
	}
}

// TestWithdrawRejectNoDeduct 校验驳回不扣余额。
func TestWithdrawRejectNoDeduct(t *testing.T) {
	store := newWalletStore(t, "wd2.db")
	defer store.Close()
	u, _ := store.CreateUser("bob", "x")
	_ = store.AddUserBalance(u.ID, 100*MicroPerPoint)
	w := &Withdrawal{UserID: u.ID, AmountMicro: 50 * MicroPerPoint, Method: "微信", AccountInfo: "bob_wx"}
	if err := store.CreateWithdrawal(w); err != nil {
		t.Fatalf("CreateWithdrawal: %v", err)
	}
	row, err := store.ReviewWithdrawal(w.ID, false, "信息不符", 1)
	if err != nil {
		t.Fatalf("ReviewWithdrawal reject: %v", err)
	}
	if row.Status != WithdrawRejected {
		t.Fatalf("驳回后状态应为 rejected，得 %q", row.Status)
	}
	fresh, _ := store.GetUserByID(u.ID)
	if fresh.BalanceMicro != 100*MicroPerPoint {
		t.Fatalf("驳回不应扣余额，应为 100 点，得 %d 微点", fresh.BalanceMicro)
	}
}

// TestWithdrawApproveInsufficientBalance 校验审核通过时余额不足会拒绝（不产生负数）。
func TestWithdrawApproveInsufficientBalance(t *testing.T) {
	store := newWalletStore(t, "wd3.db")
	defer store.Close()
	u, _ := store.CreateUser("carol", "x")
	// 先入金后提现，再把余额花掉，制造「申请时够、审核时不够」。
	_ = store.AddUserBalance(u.ID, 100*MicroPerPoint)
	w := &Withdrawal{UserID: u.ID, AmountMicro: 100 * MicroPerPoint, Method: "银行卡", AccountInfo: "6222"}
	if err := store.CreateWithdrawal(w); err != nil {
		t.Fatalf("CreateWithdrawal: %v", err)
	}
	_ = store.AddUserBalance(u.ID, -100*MicroPerPoint)
	if _, err := store.ReviewWithdrawal(w.ID, true, "", 1); err == nil {
		t.Fatal("余额不足时审核通过应报错")
	}
	fresh, _ := store.GetUserByID(u.ID)
	if fresh.BalanceMicro != 0 {
		t.Fatalf("拒绝打款后余额应保持 0，得 %d", fresh.BalanceMicro)
	}
}

// TestWalletFormatting 校验提现相关展示辅助函数（打码 / 参数回落 / uid 拆分）。
func TestWalletFormatting(t *testing.T) {
	// 收款账号打码：保留首尾各 2 位。
	if got := maskAccountInfo("6222021234567890"); got != "62************90" {
		t.Fatalf("maskAccountInfo 结果不符，得 %q", got)
	}
	if got := maskAccountInfo("1234"); got != "****" {
		t.Fatalf("短串应全打码，得 %q", got)
	}
	if got := maskAccountInfo(""); got != "" {
		t.Fatalf("空串应返回空，得 %q", got)
	}
	// 充值码打码：保留前 6 位与末 4 位。
	if got := maskRedeemCode("WB-AAAA-BBBB-CCCC"); got != "WB-AAA…CCCC" {
		t.Fatalf("maskRedeemCode 结果不符，得 %q", got)
	}
	// uid 拆分：空串 → 空切片；多余空白被丢弃。
	if got := splitUIDs("  "); len(got) != 0 {
		t.Fatalf("空串应得空切片，得 %v", got)
	}
	if got := splitUIDs(" a , , b "); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("应得 [a b]，得 %v", got)
	}
}

// TestWithdrawParamsFallback 校验提现参数缺省与非法值回落。
func TestWithdrawParamsFallback(t *testing.T) {
	store := newWalletStore(t, "param.db")
	defer store.Close()
	a := &app{store: store}

	// 未配置 → 默认比例 / 最小额。
	rate, min := a.withdrawParams()
	if rate != defaultWithdrawRate || min != defaultWithdrawMinPoints {
		t.Fatalf("缺省应为 %v / %v，得 %v / %v", defaultWithdrawRate, defaultWithdrawMinPoints, rate, min)
	}
	// 落配后生效。
	_ = store.SetKV(kvWithdrawRate, "0.02")
	_ = store.SetKV(kvWithdrawMinPoints, "250")
	if rate, min = a.withdrawParams(); rate != 0.02 || min != 250 {
		t.Fatalf("应读到 0.02 / 250，得 %v / %v", rate, min)
	}
	// 非法值（<=0 / 非数字）回落默认。
	_ = store.SetKV(kvWithdrawRate, "-1")
	_ = store.SetKV(kvWithdrawMinPoints, "abc")
	if rate, min = a.withdrawParams(); rate != defaultWithdrawRate || min != defaultWithdrawMinPoints {
		t.Fatalf("非法值应回落默认，得 %v / %v", rate, min)
	}
}

// TestRedeemHistoryByUser 校验充值记录只列本人已兑换的码。
func TestRedeemHistoryByUser(t *testing.T) {
	store := newWalletStore(t, "rd.db")
	defer store.Close()
	u, _ := store.CreateUser("dave", "x")
	other, _ := store.CreateUser("eve", "x")

	if err := store.CreateRedeemCode("WB-AAAA-0001", 10*MicroPerPoint, RedeemKindRecharge, 1); err != nil {
		t.Fatalf("CreateRedeemCode: %v", err)
	}
	if err := store.CreateRedeemCode("WB-AAAA-0002", 20*MicroPerPoint, RedeemKindExp, 1); err != nil {
		t.Fatalf("CreateRedeemCode: %v", err)
	}
	if _, _, err := store.RedeemCode("WB-AAAA-0001", u.ID); err != nil {
		t.Fatalf("RedeemCode: %v", err)
	}
	if _, _, err := store.RedeemCode("WB-AAAA-0002", other.ID); err != nil {
		t.Fatalf("RedeemCode other: %v", err)
	}
	rows, err := store.ListRedeemCodesByUser(u.ID, 50)
	if err != nil {
		t.Fatalf("ListRedeemCodesByUser: %v", err)
	}
	if len(rows) != 1 || rows[0].Code != "WB-AAAA-0001" {
		t.Fatalf("应只返回本人 1 条充值记录，得 %+v", rows)
	}
}
