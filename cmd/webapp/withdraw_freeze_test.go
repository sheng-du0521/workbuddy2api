// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"path/filepath"
	"testing"
)

// ======================================================================
// v0.27.0 · 提现冻结机制
//
// 用户报的两个现象合成一条需求：
//   「审核操作无法执行」+「发起时就要冻结，防止重复提现或超额提现」。
//
// 旧实现的毛病：待审额只是**查询时的派生减法**，不占住任何东西 ——
// 等待期间的用户消费/再申请会让审核必然失败，同一笔额度也能被申请两次。
// ======================================================================

// TestWithdrawFreezesOnApply 申请即冻结：额度减少、冻结增加、余额不变。
//
// 三者必须同时成立，缺一条都说明账目已经不一致：
//   - 余额**不该动** —— 钱还在用户账上，只是不可动用（动了就无法驳回退回）；
//   - 额度**必须减** —— 否则同一笔钱能同时被两笔申请占用；
//   - 冻结**必须加** —— 否则审核时不知道该扣哪笔。
func TestWithdrawFreezesOnApply(t *testing.T) {
	store := newWalletStore(t, "fz1.db")
	defer store.Close()
	u, _ := store.CreateUser("fz1", "x")
	if err := store.CreditWithdrawable(u.ID, 300*MicroPerPoint); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateWithdrawal(&Withdrawal{
		UserID: u.ID, AmountMicro: 100 * MicroPerPoint,
		Method: "alipay", AlipayAccount: "a@x.com", RealName: "甲",
	}); err != nil {
		t.Fatalf("CreateWithdrawal: %v", err)
	}

	got, _ := store.GetUserByID(u.ID)
	if got.BalanceMicro != 300*MicroPerPoint {
		t.Errorf("申请阶段不该动余额，应为 300 点，得 %d", got.BalanceMicro)
	}
	if got.WithdrawableMicro != 200*MicroPerPoint {
		t.Errorf("可提现额度应被冻结划走 100 点（剩 200），得 %d", got.WithdrawableMicro)
	}
	if got.FrozenMicro != 100*MicroPerPoint {
		t.Errorf("冻结额应为 100 点，得 %d", got.FrozenMicro)
	}
	// 可提现额度查询口径与桶一致（不再二次减待审）。
	q, raw, frozen, err := store.WithdrawableState(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if q != 200*MicroPerPoint || raw != 200*MicroPerPoint || frozen != 100*MicroPerPoint {
		t.Errorf("WithdrawableState = (%d,%d,%d)，期望 (200,200,100)", q, raw, frozen)
	}
}

// TestWithdrawApplyRejectsOverQuota 超额申请被拒，且**不留任何痕迹**。
//
// 事务回滚必须干净：只冻结了一半就失败的话，用户会既提不到钱又莫名少一截额度。
func TestWithdrawApplyRejectsOverQuota(t *testing.T) {
	store := newWalletStore(t, "fz2.db")
	defer store.Close()
	u, _ := store.CreateUser("fz2", "x")
	if err := store.CreditWithdrawable(u.ID, 100*MicroPerPoint); err != nil {
		t.Fatal(err)
	}
	err := store.CreateWithdrawal(&Withdrawal{
		UserID: u.ID, AmountMicro: 150 * MicroPerPoint,
		Method: "alipay", AlipayAccount: "a@x.com", RealName: "甲",
	})
	if err == nil {
		t.Fatal("超过可提现额度的申请必须被拒")
	}
	if err != ErrInsufficientWithdrawable {
		t.Errorf("应返回 ErrInsufficientWithdrawable，得 %v", err)
	}
	got, _ := store.GetUserByID(u.ID)
	if got.WithdrawableMicro != 100*MicroPerPoint || got.FrozenMicro != 0 {
		t.Errorf("失败申请不得改动任何账目：额度=%d 冻结=%d（期望 100/0）",
			got.WithdrawableMicro, got.FrozenMicro)
	}
	if rows, _ := store.ListWithdrawals(WithdrawPending, 10); len(rows) != 0 {
		t.Errorf("失败申请不该落记录，得 %d 条", len(rows))
	}
}

// TestWithdrawSecondApplyCannotReuseFrozenQuota **防止重复提现**：第一笔冻结掉的
// 额度不能被第二笔再用。
//
// 这是"超额提现"在真实使用里最常见的形态：用户连点两次「提交申请」。
func TestWithdrawSecondApplyCannotReuseFrozenQuota(t *testing.T) {
	store := newWalletStore(t, "fz3.db")
	defer store.Close()
	u, _ := store.CreateUser("fz3", "x")
	if err := store.CreditWithdrawable(u.ID, 100*MicroPerPoint); err != nil {
		t.Fatal(err)
	}
	mk := func() error {
		return store.CreateWithdrawal(&Withdrawal{
			UserID: u.ID, AmountMicro: 100 * MicroPerPoint,
			Method: "alipay", AlipayAccount: "a@x.com", RealName: "甲",
		})
	}
	if err := mk(); err != nil {
		t.Fatalf("第一笔应成功：%v", err)
	}
	if err := mk(); err != ErrInsufficientWithdrawable {
		t.Fatalf("第二笔必须因额度已被冻结而被拒，得 %v", err)
	}
	got, _ := store.GetUserByID(u.ID)
	if got.FrozenMicro != 100*MicroPerPoint {
		t.Errorf("冻结额不应翻倍（那是重复冻结），得 %d", got.FrozenMicro)
	}
	if got.WithdrawableMicro != 0 {
		t.Errorf("额度应恰好被划走一次，得 %d", got.WithdrawableMicro)
	}
}

// TestWithdrawRejectClampsRefundToBalance 驳回退款必须**钳到余额上限**。
//
// 等待期间用户可能已把余额消费到低于冻结额（ChargeUser 允许欠费），此时把额度
// 原样加回去会让 withdrawable > balance —— 那部分"复活"的额度实际已被花掉，
// 能再提一次就是凭空印钱。
func TestWithdrawRejectClampsRefundToBalance(t *testing.T) {
	store := newWalletStore(t, "fz4.db")
	defer store.Close()
	u, _ := store.CreateUser("fz4", "x")
	if err := store.CreditWithdrawable(u.ID, 100*MicroPerPoint); err != nil {
		t.Fatal(err)
	}
	w := &Withdrawal{UserID: u.ID, AmountMicro: 100 * MicroPerPoint,
		Method: "alipay", AlipayAccount: "a@x.com", RealName: "甲"}
	if err := store.CreateWithdrawal(w); err != nil {
		t.Fatal(err)
	}
	// 把余额消费到 30 点（低于冻结的 100 点）。
	if _, err := store.ChargeUser(u.ID, 70*MicroPerPoint); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReviewWithdrawal(w.ID, false, "驳回", 1); err != nil {
		t.Fatalf("驳回应成功：%v", err)
	}
	got, _ := store.GetUserByID(u.ID)
	if got.WithdrawableMicro > got.BalanceMicro {
		t.Fatalf("不变量被破坏：额度 %d > 余额 %d", got.WithdrawableMicro, got.BalanceMicro)
	}
	if got.WithdrawableMicro != 30*MicroPerPoint {
		t.Errorf("退回应被钳到余额 30 点，得 %d", got.WithdrawableMicro)
	}
	if got.FrozenMicro != 0 {
		t.Errorf("驳回后冻结额应清零，得 %d", got.FrozenMicro)
	}
}

// TestWithdrawFreezeBackfill 存量回填：老库的 pending 提现（未冻结）必须被补冻。
//
// 不补冻的两个后果都很具体：
//   - 那些额度仍留在 withdrawable 里 → 能被**再申请一次**（同一笔钱提两次）；
//   - 管理员审核老记录时 frozen 不足 → 直接报错，用户被告知"审核无法执行"。
func TestWithdrawFreezeBackfill(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bf.db")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := store.CreateUser("legacy", "x")
	if err := store.CreditWithdrawable(u.ID, 500*MicroPerPoint); err != nil {
		t.Fatal(err)
	}
	// 手工造一条"旧版本形态"的 pending：只插记录，不冻结（绕过 CreateWithdrawal）。
	if _, err := store.db.Exec(`INSERT INTO withdrawals
		(user_id, amount_micro, fee_micro, method, account_info, alipay_account, real_name, status, note, created_at)
		VALUES (?,?,0,'alipay','a@x.com / 甲','a@x.com','甲','pending','',? )`,
		u.ID, 200*MicroPerPoint, 1); err != nil {
		t.Fatal(err)
	}
	// 抹掉标记，模拟"升级到 v0.27.0 的第一次启动"。
	if err := store.SetKV(kvWithdrawFreezeMigrated, "0"); err != nil {
		t.Fatal(err)
	}
	if err := store.backfillWithdrawFreeze(); err != nil {
		t.Fatalf("回填失败：%v", err)
	}
	got, _ := store.GetUserByID(u.ID)
	if got.FrozenMicro != 200*MicroPerPoint {
		t.Fatalf("存量 pending 应被补冻 200 点，得 %d", got.FrozenMicro)
	}
	if got.WithdrawableMicro != 300*MicroPerPoint {
		t.Errorf("额度应相应扣减为 300 点，得 %d", got.WithdrawableMicro)
	}
	// 幂等：再跑一次不得重复冻结（否则每次重启都白扣一截额度）。
	if err := store.backfillWithdrawFreeze(); err != nil {
		t.Fatal(err)
	}
	got2, _ := store.GetUserByID(u.ID)
	if got2.FrozenMicro != 200*MicroPerPoint || got2.WithdrawableMicro != 300*MicroPerPoint {
		t.Errorf("重复回填不得再次扣减：冻结=%d 额度=%d", got2.FrozenMicro, got2.WithdrawableMicro)
	}
	store.Close()

	// 重新打开：kv 标记已在，不得再动账目。
	store2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	got3, _ := store2.GetUserByID(u.ID)
	if got3.FrozenMicro != 200*MicroPerPoint || got3.WithdrawableMicro != 300*MicroPerPoint {
		t.Errorf("重启后不得重复回填：冻结=%d 额度=%d", got3.FrozenMicro, got3.WithdrawableMicro)
	}
	// 补冻之后，这笔老申请就能正常审核了（这正是回填要解决的第二个后果）。
	if _, err := store2.ReviewWithdrawal(1, true, "补冻后打款", 1); err != nil {
		t.Errorf("补冻后老申请应可正常打款，却报错：%v", err)
	}
}
