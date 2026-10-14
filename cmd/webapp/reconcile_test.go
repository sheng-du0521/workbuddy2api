// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import "testing"

// 本文件钉一条涉及钱、且靠肉眼守不住的契约：**诊断进程不得改变业务状态**。
//
// 背景（2026-10-14 实测踩到）：`webapp.exe -check-update` 复用的是**完整启动路径**，
// 于是它也会跑启动期的预扣对账 —— 把 reservations 表里的在途预扣**全额退还**给用户。
//
// 那个对账本意是回收「上一个进程被强杀」留下的孤儿预扣，但它**无法区分孤儿**
// 与「另一个进程正在服务中的」预扣：真服务进程服务某请求时，那笔预扣记录同样在表里。
// 实测后果：admin 的一笔请求被判定 `[settle_failed:not found]`，该笔 9.02 点未被扣费
// （方向对用户有利、不会重复计费，但属非预期变更 —— 诊断命令不该动钱）。
//
// 契约：diagnosticOnly == true 时必须**原样不动** —— 记录还在，余额也不许变。
func TestReconcileReservationsSkippedInDiagnosticMode(t *testing.T) {
	const (
		startPoints = 100.0
		frozen      = 30.0
	)

	reservationCount := func(s *Store, id int64) int {
		t.Helper()
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM reservations WHERE id = ?`, id).Scan(&n); err != nil {
			t.Fatalf("查预扣记录失败: %v", err)
		}
		return n
	}

	t.Run("诊断模式：一行都不许动", func(t *testing.T) {
		s := newTestStore(t)
		u := mustUser(t, s, "diag", startPoints)

		r, err := s.ReserveCharge(u.ID, 0, PointToMicro(frozen), "m")
		if err != nil {
			t.Fatalf("预扣失败: %v", err)
		}
		if got := balancePoints(t, s, u.ID); got != startPoints-frozen {
			t.Fatalf("预扣后余额 = %v, want %v", got, startPoints-frozen)
		}

		reconcileReservationsOnStartup(s, true)

		if n := reservationCount(s, r.ID); n != 1 {
			t.Errorf("诊断模式删掉了不属于自己的预扣记录（残留 %d 条，want 1）", n)
		}
		if got := balancePoints(t, s, u.ID); got != startPoints-frozen {
			t.Errorf("诊断模式动了余额：%v, want %v", got, startPoints-frozen)
		}
	})

	t.Run("服务模式：回收孤儿预扣", func(t *testing.T) {
		s := newTestStore(t)
		u := mustUser(t, s, "svc", startPoints)

		r, err := s.ReserveCharge(u.ID, 0, PointToMicro(frozen), "m")
		if err != nil {
			t.Fatalf("预扣失败: %v", err)
		}

		reconcileReservationsOnStartup(s, false)

		if n := reservationCount(s, r.ID); n != 0 {
			t.Errorf("服务模式应回收孤儿预扣，实际残留 %d 条", n)
		}
		if got := balancePoints(t, s, u.ID); got != startPoints {
			t.Errorf("服务模式应全额退还，余额 = %v, want %v", got, startPoints)
		}
	})
}
