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

// TestWithdrawRejectReturnsFrozen 校验驳回：**冻结额原路退回**可提现额度，余额不变。
//
// v0.27.0：驳回不再只是"改个状态" —— 申请时划走的钱必须还回去，否则用户的钱
// 会永久卡在冻结额里（余额看着没少，却永远提不出来）。
func TestWithdrawRejectReturnsFrozen(t *testing.T) {
	store := newWalletStore(t, "wd2.db")
	defer store.Close()
	u, _ := store.CreateUser("bob", "x")
	// 入账走**可提现额度**：提现只允许动返点/卖号所得（AddUserBalance 是充值桶，
	// 不可提现，用它会让这条用例在额度校验处就失败）。
	if err := store.CreditWithdrawable(u.ID, 100*MicroPerPoint); err != nil {
		t.Fatalf("CreditWithdrawable: %v", err)
	}
	w := &Withdrawal{UserID: u.ID, AmountMicro: 50 * MicroPerPoint, Method: "alipay", AlipayAccount: "bob@x.com", RealName: "鲍勃"}
	if err := store.CreateWithdrawal(w); err != nil {
		t.Fatalf("CreateWithdrawal: %v", err)
	}
	// 申请后：额度已扣、冻结已加，余额**不变**（钱还在账上，只是不可动用）。
	before, _ := store.GetUserByID(u.ID)
	if before.FrozenMicro != 50*MicroPerPoint {
		t.Fatalf("申请后冻结额应为 50 点，得 %d", before.FrozenMicro)
	}
	if before.WithdrawableMicro != 50*MicroPerPoint {
		t.Fatalf("申请后可提现额度应剩 50 点，得 %d", before.WithdrawableMicro)
	}
	if before.BalanceMicro != 100*MicroPerPoint {
		t.Fatalf("申请阶段不动余额，应仍为 100 点，得 %d", before.BalanceMicro)
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
	if fresh.FrozenMicro != 0 {
		t.Fatalf("驳回后冻结额应清零，得 %d", fresh.FrozenMicro)
	}
	if fresh.WithdrawableMicro != 100*MicroPerPoint {
		t.Fatalf("驳回后额度应原路退回为 100 点，得 %d", fresh.WithdrawableMicro)
	}
}

// TestWithdrawApproveSucceedsDespiteSpending 冻结的核心承诺：**申请之后用户怎么消费，
// 都不影响审核通过**。
//
// 这正是用户报的「审核操作无法执行」的回归护栏。旧实现只在查询时减待审额，用户
// 一等审核期间消费就把余额打到低于待审额，审核必然失败（管理员只能驳回）。
func TestWithdrawApproveSucceedsDespiteSpending(t *testing.T) {
	store := newWalletStore(t, "wd3.db")
	defer store.Close()
	u, _ := store.CreateUser("carol", "x")
	// 100 点可提现 + 100 点充值（后者用于制造"消费"的空间）。
	if err := store.CreditWithdrawable(u.ID, 100*MicroPerPoint); err != nil {
		t.Fatal(err)
	}
	if err := store.AddUserBalance(u.ID, 100*MicroPerPoint); err != nil {
		t.Fatal(err)
	}
	w := &Withdrawal{UserID: u.ID, AmountMicro: 100 * MicroPerPoint, Method: "alipay", AlipayAccount: "c@x.com", RealName: "卡罗"}
	if err := store.CreateWithdrawal(w); err != nil {
		t.Fatalf("CreateWithdrawal: %v", err)
	}
	// 等待审核期间把余额花到只剩 100 点（刚好等于冻结额）——旧实现下这一步之后
	// 审核就会失败（balance 100 < ... 实际旧判据是 wd < amount）。
	if _, err := store.ChargeUser(u.ID, 100*MicroPerPoint); err != nil {
		t.Fatal(err)
	}
	row, err := store.ReviewWithdrawal(w.ID, true, "已打款", 999)
	if err != nil {
		t.Fatalf("冻结后审核应必然成功，却报错：%v", err)
	}
	if row.Status != WithdrawApproved {
		t.Fatalf("状态应为 approved，得 %q", row.Status)
	}
	fresh, _ := store.GetUserByID(u.ID)
	if fresh.BalanceMicro != 0 {
		t.Fatalf("打款后余额应为 0（200 - 100 消费 - 100 打款），得 %d", fresh.BalanceMicro)
	}
	if fresh.FrozenMicro != 0 {
		t.Fatalf("打款后冻结额应清零，得 %d", fresh.FrozenMicro)
	}
}

// TestWithdrawApproveFailsWhenFreezeMissing 冻结额对不上时报错而不是把余额扣成负数。
//
// 这是"数据异常"分支：老版本提交且未回填的 pending 记录，或有人直接改过库。
// 必须明确失败 —— 静默通过会让 balance 变负、账目无法对账。
func TestWithdrawApproveFailsWhenFreezeMissing(t *testing.T) {
	store := newWalletStore(t, "wd3b.db")
	defer store.Close()
	u, _ := store.CreateUser("carol2", "x")
	if err := store.CreditWithdrawable(u.ID, 100*MicroPerPoint); err != nil {
		t.Fatal(err)
	}
	w := &Withdrawal{UserID: u.ID, AmountMicro: 100 * MicroPerPoint, Method: "alipay", AlipayAccount: "c@x.com", RealName: "卡"}
	if err := store.CreateWithdrawal(w); err != nil {
		t.Fatal(err)
	}
	// 手工把冻结额清零（模拟"提交于旧版本、未回填"）。
	if _, err := store.db.Exec(`UPDATE users SET frozen_micro = 0 WHERE id = ?`, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReviewWithdrawal(w.ID, true, "", 1); err == nil {
		t.Fatal("冻结额不足时审核通过必须报错（否则余额会被扣成负数）")
	}
	fresh, _ := store.GetUserByID(u.ID)
	if fresh.BalanceMicro != 100*MicroPerPoint {
		t.Fatalf("失败后余额应保持 100 点，得 %d", fresh.BalanceMicro)
	}
	// 该申请仍是 pending，可被驳回（这也是给运维的补救路径）。
	rows, _ := store.ListWithdrawals(WithdrawPending, 10)
	if len(rows) != 1 {
		t.Fatalf("申请应仍处于待审状态以便驳回，得 %d 条", len(rows))
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
