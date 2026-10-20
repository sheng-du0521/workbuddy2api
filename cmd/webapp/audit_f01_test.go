package main

import "testing"

// 审查报告 F01：预扣释放不恢复可提现额度。
//
// 现象（修复前）：余额 100、可提现 100 → 预扣 80 → 全额释放 →
// 余额回到 100，但可提现永久停在 20。用户什么都没消费，却少了一截可提现额度。
//
// 根因：预扣用 `withdrawable = MIN(withdrawable, MAX(balance-amount,0))` 压低额度，
// 但没有记录"压了多少"；释放只加回 balance，withdrawable 不还原。
// 修法：reservations 记录 frozen_withdrawable_micro，结算/释放时按实际消耗还原。
func TestReserveRefundPreservesWithdrawable(t *testing.T) {
	store, _, _ := newTestApp(t)
	defer store.Close()
	u, err := store.CreateUser("f01-user", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	setBuckets := func(balance, withdrawable int64) {
		t.Helper()
		if _, err := store.db.Exec(
			`UPDATE users SET balance_micro = ?, exp_micro = 0, recharge_micro = ?, withdrawable_micro = ?, frozen_micro = 0
			 WHERE id = ?`, balance, balance-withdrawable, withdrawable, u.ID); err != nil {
			t.Fatalf("设置分桶: %v", err)
		}
	}
	buckets := func() (balance, withdrawable int64) {
		t.Helper()
		if err := store.db.QueryRow(
			`SELECT balance_micro, withdrawable_micro FROM users WHERE id = ?`, u.ID).
			Scan(&balance, &withdrawable); err != nil {
			t.Fatalf("读分桶: %v", err)
		}
		return
	}

	// ---- 场景 1：全额释放（请求失败 / 取消）----
	setBuckets(100, 100)
	rv, err := store.ReserveCharge(u.ID, 0, 80, "m1")
	if err != nil {
		t.Fatalf("ReserveCharge: %v", err)
	}
	if rv.FrozenWithdrawableMicro != 80 {
		t.Fatalf("预扣应冻结 80 可提现，得 %d", rv.FrozenWithdrawableMicro)
	}
	if bal, wd := buckets(); bal != 20 || wd != 20 {
		t.Fatalf("预扣后应为 20/20，得 %d/%d", bal, wd)
	}
	if _, err := store.ReleaseReservation(rv.ID); err != nil {
		t.Fatalf("ReleaseReservation: %v", err)
	}
	if bal, wd := buckets(); bal != 100 || wd != 100 {
		t.Fatalf("全额释放后应恢复 100/100，得 %d/%d（F01 未修复）", bal, wd)
	}

	// ---- 场景 2：部分消费（预扣 80、实扣 30）----
	setBuckets(100, 100)
	rv, err = store.ReserveCharge(u.ID, 0, 80, "m2")
	if err != nil {
		t.Fatalf("ReserveCharge: %v", err)
	}
	if _, err := store.SettleReservation(rv.ID, 30); err != nil {
		t.Fatalf("SettleReservation: %v", err)
	}
	if bal, wd := buckets(); bal != 70 || wd != 70 {
		t.Fatalf("实扣 30 后应为 70/70（可提现净减 30），得 %d/%d", bal, wd)
	}

	// ---- 场景 3：混合分桶（余额 100 里只有 50 可提现）----
	setBuckets(100, 50)
	rv, err = store.ReserveCharge(u.ID, 0, 80, "m3")
	if err != nil {
		t.Fatalf("ReserveCharge: %v", err)
	}
	// 余额 100-80=20，可提现被压到 20 → 冻结 30（不是 50）。
	if rv.FrozenWithdrawableMicro != 30 {
		t.Fatalf("混合桶预扣应冻结 30（可提现 50 被压到新余额 20），得 %d", rv.FrozenWithdrawableMicro)
	}
	if _, err := store.ReleaseReservation(rv.ID); err != nil {
		t.Fatalf("ReleaseReservation: %v", err)
	}
	if bal, wd := buckets(); bal != 100 || wd != 50 {
		t.Fatalf("混合桶全额释放应回到 100/50，得 %d/%d", bal, wd)
	}

	// ---- 场景 4：实际消耗超过冻结额（不应把可提现加回去）----
	setBuckets(100, 20)
	rv, err = store.ReserveCharge(u.ID, 0, 50, "m4") // 新余额 50 ≥ 可提现 20 → 未压低，冻结 0
	if err != nil {
		t.Fatalf("ReserveCharge: %v", err)
	}
	if rv.FrozenWithdrawableMicro != 0 {
		t.Fatalf("可提现低于新余额时不该冻结额度，得 %d", rv.FrozenWithdrawableMicro)
	}
	if _, err := store.SettleReservation(rv.ID, 40); err != nil {
		t.Fatalf("SettleReservation: %v", err)
	}
	// balance = 100-50+10 = 60；可提现没有被压低过，保持 20（且 20 ≤ 60）。
	if bal, wd := buckets(); bal != 60 || wd != 20 {
		t.Fatalf("期望 60/20，得 %d/%d", bal, wd)
	}
}

// TestWithdrawableInvariant 任何操作后都满足 0 ≤ withdrawable ≤ balance。
func TestWithdrawableInvariant(t *testing.T) {
	store, _, _ := newTestApp(t)
	defer store.Close()
	u, _ := store.CreateUser("f01-inv", "x")
	if _, err := store.db.Exec(
		`UPDATE users SET balance_micro = 100, exp_micro = 0, recharge_micro = 0,
		 withdrawable_micro = 100, frozen_micro = 0 WHERE id = ?`, u.ID); err != nil {
		t.Fatalf("设置分桶: %v", err)
	}
	for i := 0; i < 20; i++ {
		rv, err := store.ReserveCharge(u.ID, 0, 30, "m")
		if err != nil {
			continue
		}
		if i%2 == 0 {
			_, _ = store.ReleaseReservation(rv.ID)
		} else {
			_, _ = store.SettleReservation(rv.ID, 10)
		}
		var bal, wd int64
		if err := store.db.QueryRow(
			`SELECT balance_micro, withdrawable_micro FROM users WHERE id = ?`, u.ID).
			Scan(&bal, &wd); err != nil {
			t.Fatalf("读分桶: %v", err)
		}
		if wd < 0 || wd > bal {
			t.Fatalf("第 %d 轮违反 0 ≤ withdrawable(%d) ≤ balance(%d)", i, wd, bal)
		}
	}
}
