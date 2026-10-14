// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustUser(t *testing.T, s *Store, name string, points float64) *User {
	t.Helper()
	u, err := s.CreateUser(name, "hash")
	if err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	if points > 0 {
		if err := s.AddUserBalance(u.ID, PointToMicro(points)); err != nil {
			t.Fatalf("充值失败: %v", err)
		}
	}
	fresh, err := s.GetUserByID(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	return fresh
}

func balancePoints(t *testing.T, s *Store, id int64) float64 {
	t.Helper()
	u, err := s.GetUserByID(id)
	if err != nil {
		t.Fatal(err)
	}
	return MicroToPoint(u.BalanceMicro)
}

// 预扣 → 结算，多退少补。
func TestReserveSettleAdjustsBalance(t *testing.T) {
	s := newTestStore(t)
	u := mustUser(t, s, "alice", 100)

	r, err := s.ReserveCharge(u.ID, 0, PointToMicro(30), "m")
	if err != nil {
		t.Fatalf("预扣失败: %v", err)
	}
	if got := balancePoints(t, s, u.ID); got != 70 {
		t.Fatalf("预扣后余额 = %v, 期望 70", got)
	}
	// 实际只花 10 点 → 净退 20
	if _, err := s.SettleReservation(r.ID, PointToMicro(10)); err != nil {
		t.Fatalf("结算失败: %v", err)
	}
	if got := balancePoints(t, s, u.ID); got != 90 {
		t.Fatalf("结算后余额 = %v, 期望 90", got)
	}
	// 预扣记录应已删除
	if n, _, err := s.ReconcileReservations(); err != nil || n != 0 {
		t.Fatalf("结算后不该有残留预扣: n=%d err=%v", n, err)
	}
}

// 余额不足时预扣直接拒绝，且不动余额。
func TestReserveRejectsInsufficientBalance(t *testing.T) {
	s := newTestStore(t)
	u := mustUser(t, s, "bob", 10)

	_, err := s.ReserveCharge(u.ID, 0, PointToMicro(30), "m")
	if !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("期望 ErrInsufficientBalance, 实得 %v", err)
	}
	if got := balancePoints(t, s, u.ID); got != 10 {
		t.Fatalf("被拒后余额应不变, 实得 %v", got)
	}
}

// 用户在预扣后被删除 → 返回 ErrNotFound 而不是余额不足。
func TestReserveUnknownUser(t *testing.T) {
	s := newTestStore(t)
	_, err := s.ReserveCharge(9999, 0, PointToMicro(1), "m")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("期望 ErrNotFound, 实得 %v", err)
	}
}

// 实际消耗超过预扣时允许欠费（不中断已发完的流式响应）。
func TestSettleAllowsOverdraft(t *testing.T) {
	s := newTestStore(t)
	u := mustUser(t, s, "carol", 50)

	r, err := s.ReserveCharge(u.ID, 0, PointToMicro(20), "m")
	if err != nil {
		t.Fatal(err)
	}
	// 实际花了 80 点：净变动 = 20 - 80 = -60
	if _, err := s.SettleReservation(r.ID, PointToMicro(80)); err != nil {
		t.Fatalf("结算失败: %v", err)
	}
	if got := balancePoints(t, s, u.ID); got != -30 {
		t.Fatalf("透支后余额 = %v, 期望 -30", got)
	}
}

// 释放预扣 = 全额退还。
func TestReleaseReservationRefunds(t *testing.T) {
	s := newTestStore(t)
	u := mustUser(t, s, "dave", 40)

	r, err := s.ReserveCharge(u.ID, 0, PointToMicro(25), "m")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReleaseReservation(r.ID); err != nil {
		t.Fatalf("释放失败: %v", err)
	}
	if got := balancePoints(t, s, u.ID); got != 40 {
		t.Fatalf("释放后余额 = %v, 期望 40", got)
	}
}

// 重复结算同一笔 → ErrNotFound（幂等保护，避免重复退钱）。
func TestSettleReservationIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	u := mustUser(t, s, "erin", 30)

	r, err := s.ReserveCharge(u.ID, 0, PointToMicro(10), "m")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SettleReservation(r.ID, PointToMicro(5)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SettleReservation(r.ID, PointToMicro(5)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("二次结算应返回 ErrNotFound, 实得 %v", err)
	}
	if got := balancePoints(t, s, u.ID); got != 25 {
		t.Fatalf("二次结算不该再退钱, 余额 = %v, 期望 25", got)
	}
}

// 崩溃对账：残留预扣全部退还。
func TestReconcileRefundsOrphans(t *testing.T) {
	s := newTestStore(t)
	u := mustUser(t, s, "frank", 100)

	if _, err := s.ReserveCharge(u.ID, 0, PointToMicro(30), "m1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveCharge(u.ID, 0, PointToMicro(20), "m2"); err != nil {
		t.Fatal(err)
	}
	if got := balancePoints(t, s, u.ID); got != 50 {
		t.Fatalf("两笔预扣后余额 = %v, 期望 50", got)
	}
	n, refunded, err := s.ReconcileReservations()
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if n != 2 || refunded != PointToMicro(50) {
		t.Fatalf("对账结果 n=%d refunded=%d, 期望 2 / %d", n, refunded, PointToMicro(50))
	}
	if got := balancePoints(t, s, u.ID); got != 100 {
		t.Fatalf("对账后余额 = %v, 期望 100", got)
	}
	// 幂等：再跑一次无残留
	if n, _, _ := s.ReconcileReservations(); n != 0 {
		t.Fatalf("第二次对账应为 0 笔, 实得 %d", n)
	}
}

// 消费闸统计：只算正数流水，失败/零费不计。
func TestUserUsedSince(t *testing.T) {
	s := newTestStore(t)
	u := mustUser(t, s, "grace", 0)
	now := time.Now().Unix()

	_ = s.InsertUsageLog(&UsageLog{UserID: u.ID, Model: "m", ChargedMicro: PointToMicro(7), Status: "ok", CreatedAt: now})
	_ = s.InsertUsageLog(&UsageLog{UserID: u.ID, Model: "m", ChargedMicro: 0, Status: "ok", CreatedAt: now})
	_ = s.InsertUsageLog(&UsageLog{UserID: u.ID, Model: "m", ChargedMicro: PointToMicro(3), Status: "error", CreatedAt: now})
	// 早于统计窗口的旧流水不计入
	_ = s.InsertUsageLog(&UsageLog{UserID: u.ID, Model: "m", ChargedMicro: PointToMicro(99), Status: "ok", CreatedAt: now - 7200})

	used, err := s.UserUsedSince(u.ID, now-60)
	if err != nil {
		t.Fatal(err)
	}
	if used != PointToMicro(10) {
		t.Fatalf("统计 = %d, 期望 %d（7+3）", used, PointToMicro(10))
	}
}

// 限额设置落库。
func TestSetUserLimits(t *testing.T) {
	s := newTestStore(t)
	u := mustUser(t, s, "heidi", 0)

	if err := s.SetUserLimits(u.ID, PointToMicro(50), PointToMicro(500)); err != nil {
		t.Fatalf("设置限额失败: %v", err)
	}
	got, err := s.GetUserByID(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DailyLimitMicro != PointToMicro(50) || got.MonthlyLimitMicro != PointToMicro(500) {
		t.Fatalf("限额 = %d/%d, 期望 %d/%d",
			got.DailyLimitMicro, got.MonthlyLimitMicro, PointToMicro(50), PointToMicro(500))
	}
}
