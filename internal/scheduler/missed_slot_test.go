// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package scheduler

import (
	"testing"
	"time"
)

// 本文件锁死 v0.23.3 修复的「槽位被本批执行吞掉」缺陷。
//
// 现场（真实生产台账）：签到与旅行都配 9 点，日志里旅行 09:03:09 启动、09:05:00
// 结束；而签到那一轮的抖动时刻是 09:03:59 —— 正落在旅行执行窗口内。旧主循环每轮
// 以 **time.Now()** 为基准调用 nextWake，而 nextWake 只返回 now 之后的最近时点，
// 于是 09:03:59 既不返回、后续轮次也不会再想起它，直接顺延到 21 点（并再次被吞）。
// 结果：台账里 checkin 连续多天停在很久以前的一次人工触发上。
//
// 修复后主循环以「上一批的槽位时刻」为基准（base），被越过的槽位仍会被算出 →
// time.Until 为负 → timer 立即到期 → 当场补跑。

// onlyCheckinActivity 只启用签到（9 点）与活跃上报（10 点），关闭其余任务，
// 便于构造「9 点这批跑太久、越过 10 点槽位」的场景。
func onlyCheckinActivity() *Scheduler {
	return New(Config{
		CheckinHours:   []int{9},
		ActivityHours:  []int{10},
		JitterMinutes:  0, // 关抖动：本文件测的是补跑语义，不是抖动派生
		TravelDisabled: true, KeepaliveDisabled: true,
		SchoolDisabled: true, CatDisabled: true,
	})
}

// TestNextWakeFromBaseReturnsOverdueSlot **本文件最重要的用例**。
//
// 同一份 nextWake，换一个基准，结论完全不同：
//   - 以「现在」（10:05）为基准 → 10:00 已过 → 顺延到次日（槽位**丢失**，就是 bug）；
//   - 以「上一批槽位」（09:00）为基准 → 10:00 落在 (base, now] 内 → 仍被算出，
//     且它早于 now，Run 的 timer 会立即到期补跑。
func TestNextWakeFromBaseReturnsOverdueSlot(t *testing.T) {
	s := onlyCheckinActivity()

	slot0900 := time.Date(2026, 9, 25, 9, 0, 0, 0, time.Local)
	nowAfter := time.Date(2026, 9, 25, 10, 5, 0, 0, time.Local) // 9 点这批跑到 10:05
	slot1000 := time.Date(2026, 9, 25, 10, 0, 0, 0, time.Local)

	// 旧口径（以"现在"为基准）：10:00 的槽位被跳过，直接顺延到次日。
	oldNext, _ := s.nextWake(nowAfter)
	if oldNext.Equal(slot1000) {
		t.Fatalf("前提不成立：以 now 为基准不该再返回已过的槽位，got %v", oldNext)
	}
	if oldNext.Day() != 26 {
		t.Fatalf("旧口径应顺延到次日（这正是槽位丢失的表现），got %v", oldNext)
	}

	// 新口径（以上一批槽位为基准）：被越过的 10:00 仍算得出，且已到期。
	base := slot0900.Add(time.Nanosecond)
	next, kinds := s.nextWake(base)
	if !next.Equal(slot1000) {
		t.Fatalf("以上一批为基准应算出被越过的 10:00 槽位，got %v", next)
	}
	if next.After(nowAfter) {
		t.Fatalf("该槽位应已到期（否则 Run 不会立即补跑）：%v 不早于 now %v", next, nowAfter)
	}
	if len(kinds) != 1 || kinds[0] != taskActivity {
		t.Fatalf("该槽位应只含 activity，got %v", kinds)
	}
}

// TestPlanNextUsesPreviousSlotAsBase 把「基准取谁」钉进测试（v0.23.3 修复的关键）。
//
// planNext 是 Run 主循环一轮的全部决策（提取成可测面就是为了这一条）。它必须
// 用**上一批的槽位时刻**推进水位线：这样本批执行期间越过的槽位在下一轮仍被算出，
// 且落在过去（Run 的 timer 立即到期 → 当场补跑）。
//
// 若有人把调用方改回 time.Now()、或把 planNext 的返回值改成 time.Now()，本用例
// 与 TestNextWakeFromBaseReturnsOverdueSlot 会一起失败 —— 这就是它存在的意义。
func TestPlanNextUsesPreviousSlotAsBase(t *testing.T) {
	s := onlyCheckinActivity()
	slot1000 := time.Date(2026, 9, 25, 10, 0, 0, 0, time.Local)
	nowAfter := time.Date(2026, 9, 25, 10, 5, 0, 0, time.Local)

	// 第 1 轮：从 9/25 0 点起排 → 命中 9 点这批。
	base := time.Date(2026, 9, 25, 0, 0, 0, 0, time.Local)
	at1, kinds1, base2 := s.planNext(base)
	if at1.Hour() != 9 || len(kinds1) != 1 || kinds1[0] != taskCheckin {
		t.Fatalf("第 1 轮应为 9 点签到，got %v %v", at1, kinds1)
	}
	// 水位线必须推进到该槽位**之后**（不是"现在"）。
	if !base2.After(at1) {
		t.Fatalf("水位线应推进到槽位之后：at=%v nextBase=%v", at1, base2)
	}

	// 第 2 轮：模拟这一批跑了 65 分钟（越过 10 点的活跃上报槽位）。
	at2, kinds2, _ := s.planNext(base2)
	if !at2.Equal(slot1000) {
		t.Fatalf("应以 base2 为基准算出被越过的 10:00 槽位，got %v", at2)
	}
	if !at2.Before(nowAfter) {
		t.Fatalf("该槽位应已到期（Run 才会立即补跑），%v 不早于 %v", at2, nowAfter)
	}
	if len(kinds2) != 1 || kinds2[0] != taskActivity {
		t.Fatalf("第 2 轮应只含 activity，got %v", kinds2)
	}
}

// TestNextWakeBaseAdvancesWithoutDuplicate 基准推进到本批槽位之后，同一个槽位
// 不会被返回第二次（补跑不能变成重复执行）。
func TestNextWakeBaseAdvancesWithoutDuplicate(t *testing.T) {
	s := New(Config{
		CheckinHours: []int{9}, TravelHours: []int{9}, // 同配 9 点：必须同批
		JitterMinutes:    0,
		ActivityDisabled: true, KeepaliveDisabled: true,
		SchoolDisabled: true, CatDisabled: true,
	})
	slot := time.Date(2026, 9, 25, 9, 0, 0, 0, time.Local)

	first, kinds := s.nextWake(slot.AddDate(0, 0, -1))
	if !first.Equal(slot) {
		t.Fatalf("应先命中 9/25 09:00，got %v", first)
	}
	if len(kinds) != 2 {
		t.Fatalf("签到与旅行同配 9 点应同批派发，got %v", kinds)
	}

	// 派发完把基准推进到该槽位之后 → 下一轮必须顺延到次日，不得重复。
	next, _ := s.nextWake(slot.Add(time.Nanosecond))
	if next.Day() != 26 {
		t.Fatalf("基准推进后应顺延到次日 9/26，got %v（同一槽位被重复派发）", next)
	}
	if next.Hour() != 9 {
		t.Fatalf("次日仍应在 9 点档，got %v", next)
	}
}
