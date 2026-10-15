// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package scheduler

import (
	"testing"
	"time"

	"workbuddy2api/internal/taskledger"
)

// TestNextRunsCoversEnabledKinds 启用的任务类都应有下次触发时刻，禁用的不出现。
func TestNextRunsCoversEnabledKinds(t *testing.T) {
	s := New(Config{
		CheckinHours:   []int{9, 21},
		TravelHours:    []int{9, 21},
		KeepaliveHours: []int{22},
		CatHours:       []int{1},
		// 显式禁用两类，验证它们不出现在结果里。
		ActivityDisabled: true,
	})
	got := s.NextRuns()

	for _, k := range []taskKind{taskCheckin, taskTravel, taskKeepalive, taskCat} {
		if _, ok := got[k.String()]; !ok {
			t.Errorf("启用中的 %s 应有下次触发时刻，得 %v", k, keysOf(got))
		}
	}
	// 成长任务不排程：绝不能出现（否则前端会显示一个永远不触发的「下次时刻」）。
	if _, ok := got[taskGrowth.String()]; ok {
		t.Errorf("成长任务不排程，不应有下次触发时刻，得 %v", keysOf(got))
	}
	for _, k := range []taskKind{taskActivity} {
		if _, ok := got[k.String()]; ok {
			t.Errorf("已禁用的 %s 不应有下次触发时刻，得 %v", k, keysOf(got))
		}
	}
}

// TestNextRunsIsFuture 下次触发时刻必须**在未来** —— 这是「定时有效」的关键承诺：
// 若返回了过去时刻，前端会算出负数剩余时间，显示成「0 秒后」这种明显错误。
func TestNextRunsIsFuture(t *testing.T) {
	base := time.Date(2026, 10, 11, 8, 30, 0, 0, time.Local)
	s := New(Config{CheckinHours: []int{9}, JitterMinutes: 5})
	s.nowFn = func() time.Time { return base }

	at, ok := s.NextRuns()[taskCheckin.String()]
	if !ok {
		t.Fatal("签到应有下次触发时刻")
	}
	if !at.After(base) {
		t.Fatalf("下次触发必须晚于 now（%s），得 %s", base.Format("15:04:05"), at.Format("15:04:05"))
	}
	// 含 jitter 偏移，故可能略晚于 9:00，但不应跨过 9:00+5min。
	if at.Hour() != 9 || at.Minute() > 5 {
		t.Fatalf("应落在 09:00~09:05，得 %s", at.Format("15:04:05"))
	}
}

// TestNextRunsJitterWrapped 已过时点应回落到明天（不是返回今天那个已过去的时刻）。
func TestNextRunsJitterWrapped(t *testing.T) {
	// 当天 23:30：9 点早已过去 → 应排到明天 9 点。
	base := time.Date(2026, 10, 11, 23, 30, 0, 0, time.Local)
	s := New(Config{CheckinHours: []int{9}})
	s.nowFn = func() time.Time { return base }

	at := s.NextRuns()[taskCheckin.String()]
	if at.Day() != 12 || at.Hour() != 9 {
		t.Fatalf("23:30 时下次签到应为次日 09:00，得 %s", at.Format("2006-01-02 15:04"))
	}
}

// TestNextRunsDeterministic 同一 now 下多次调用结果一致 —— 否则前端每次轮询
// 看到的「下次时刻」都在跳，反而让人怀疑调度器有问题。
func TestNextRunsDeterministic(t *testing.T) {
	base := time.Date(2026, 10, 11, 8, 30, 0, 0, time.Local)
	s := New(Config{CheckinHours: []int{9, 21}, JitterMinutes: 5})
	s.nowFn = func() time.Time { return base }

	first := s.NextRuns()[taskCheckin.String()]
	for i := 0; i < 5; i++ {
		if got := s.NextRuns()[taskCheckin.String()]; !got.Equal(first) {
			t.Fatalf("多次调用结果应一致：%s vs %s", first.Format("15:04:05"), got.Format("15:04:05"))
		}
	}
}

// TestRunningKindsEmptyAndSorted 运行态：默认空、占用后可读到且有序。
func TestRunningKindsEmptyAndSorted(t *testing.T) {
	s := New(Config{})
	if got := s.RunningKinds(); len(got) != 0 {
		t.Fatalf("初始不应有运行中任务，得 %v", got)
	}

	s.beginRun(taskTravel)
	s.beginRun(taskCheckin)
	defer s.endRun(taskCheckin)
	defer s.endRun(taskTravel)

	got := s.RunningKinds()
	if len(got) != 2 {
		t.Fatalf("应有 2 个运行中任务，得 %v", got)
	}
	// 排序保证同一状态下输出稳定（否则前端顺序每轮跳动）。
	if got[0] != "checkin" || got[1] != "travel" {
		t.Fatalf("运行中任务应按名字有序，得 %v", got)
	}

	s.endRun(taskTravel)
	if got := s.RunningKinds(); len(got) != 1 || got[0] != "checkin" {
		t.Fatalf("释放后应只剩 checkin，得 %v", got)
	}
}

// TestRunningKindsNotAffectedByOtherKinds 不同任务类互不干扰（签到在跑不该让旅行显示为运行中）。
func TestRunningKindsNotAffectedByOtherKinds(t *testing.T) {
	s := New(Config{})
	s.beginRun(taskCheckin)
	defer s.endRun(taskCheckin)
	for _, k := range s.RunningKinds() {
		if k == taskTravel.String() {
			t.Fatal("旅行未被占用，不应出现在运行中列表")
		}
	}
}

// TestLastAutoRunsKeepsAutoNotManual 核心断言：**手动触发不得覆盖「上次自动触发」快照**。
//
// 这正是「定时到底跑没跑」看不出来的根源 —— 台账按任务类只留最近一轮，管理员点一次
// 「立即执行」，定时那一轮就从卡片上消失了。该快照只记非人工触发，把这条信息保住。
func TestLastAutoRunsKeepsAutoNotManual(t *testing.T) {
	s := New(Config{})

	// 先记一轮定时。
	s.recordRun(taskCheckin, triggerSchedule, time.Now(), runTally{total: 18, ok: 18})
	auto := s.LastAutoRuns()["checkin"]
	if auto.Trigger != triggerSchedule || auto.OK != 18 {
		t.Fatalf("应记下定时轮次，得 %+v", auto)
	}

	// 再记一轮人工 —— 不得覆盖上面的定时快照。
	s.recordRun(taskCheckin, triggerManual, time.Now(), runTally{total: 18, ok: 3})
	after := s.LastAutoRuns()["checkin"]
	if after.Trigger != triggerSchedule {
		t.Fatalf("人工触发不得覆盖自动快照（否则「定时跑过没」再次不可查），得 trigger=%q", after.Trigger)
	}
	if after.OK != 18 {
		t.Fatalf("自动快照的结果应保持为定时那轮（ok=18），得 ok=%d", after.OK)
	}

	// 重试触发算自动：应更新快照。
	s.recordRun(taskCheckin, triggerRetry, time.Now(), runTally{total: 18, ok: 5, fail: 13})
	if got := s.LastAutoRuns()["checkin"].Trigger; got != triggerRetry {
		t.Fatalf("自动重试应更新快照，得 trigger=%q", got)
	}
}

// TestLastAutoRunsIsolatedPerKind 各类任务互不干扰；未跑过的类不出现。
func TestLastAutoRunsIsolatedPerKind(t *testing.T) {
	s := New(Config{})
	s.recordRun(taskTravel, triggerSchedule, time.Now(), runTally{total: 5, ok: 5})

	got := s.LastAutoRuns()
	if len(got) != 1 {
		t.Fatalf("只跑过 travel，应只有 1 条，得 %v", got)
	}
	if _, ok := got["checkin"]; ok {
		t.Fatal("未跑过的 checkin 不应出现")
	}
	// 返回的是副本：外部改动不影响内部。
	got["travel"] = taskledger.Run{}
	if s.LastAutoRuns()["travel"].OK != 5 {
		t.Fatal("LastAutoRuns 应返回副本，外部改动不得影响内部状态")
	}
}

func keysOf(m map[string]time.Time) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
