// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package scheduler

import (
	"context"
	"testing"
	"time"
)

// TestBeginRunIsExclusive 运行闸门互斥：占用后同类再占失败，释放后可再占；
// 不同任务类互不影响（签到在跑不该挡旅行）。
func TestBeginRunIsExclusive(t *testing.T) {
	s := New(Config{})

	if !s.beginRun(taskCheckin) {
		t.Fatal("首次占用应成功")
	}
	if s.beginRun(taskCheckin) {
		t.Fatal("已占用时同类再占用必须失败（否则手动 vs 定时会双倍打上游）")
	}
	// 不同类不互相阻塞。
	if !s.beginRun(taskTravel) {
		t.Fatal("不同任务类不应互相阻塞")
	}

	s.endRun(taskCheckin)
	if !s.beginRun(taskCheckin) {
		t.Fatal("释放后应可再次占用")
	}
	s.endRun(taskCheckin)
	s.endRun(taskTravel)
}

// TestSkipIfBusy 撞车即让路：占用中返回 true，空闲返回 false。
func TestSkipIfBusy(t *testing.T) {
	s := New(Config{})
	if s.skipIfBusy(taskActivity, triggerSchedule) {
		t.Fatal("空闲时不应让路")
	}
	if !s.skipIfBusy(taskActivity, triggerManual) {
		t.Fatal("已被（本轮）占用时，第二次调用应让路")
	}
	s.endRun(taskActivity)
	if s.skipIfBusy(taskActivity, triggerSchedule) {
		t.Fatal("释放后不应让路")
	}
	s.endRun(taskActivity)
}

// TestManualAndScheduledAreMutuallyExclusive 本用例锁死「手动 vs 定时」这一条：
// 定时轮次正在跑时，手动入口必须让路（反之亦然）。
//
// 这是 v0.17.3 修的缺口 —— 此前只有签到有互斥，其余四类手动撞定时会双倍打上游。
func TestManualAndScheduledAreMutuallyExclusive(t *testing.T) {
	s := New(Config{})

	// 模拟定时轮次已占用。
	if !s.beginRun(taskTravel) {
		t.Fatal("定时占用应成功")
	}
	// 手动入口（RunTravelNow → runTravel）撞上时，runTravel 应立刻返回且不产生上游调用。
	// 这里直接验证守卫判据：runTravel 的第一步就是 skipIfBusy。
	if !s.skipIfBusy(taskTravel, triggerManual) {
		t.Fatal("定时在跑时手动入口必须让路")
	}
	s.endRun(taskTravel)
}

// TestBatchStaggerDisabledByDefault 默认（未配置）零等待 —— 保证行为与引入前逐字一致。
func TestBatchStaggerDisabledByDefault(t *testing.T) {
	s := New(Config{})
	start := time.Now()
	if !s.staggerBatchGap(context.Background()) {
		t.Fatal("未配置时不应返回 false")
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("未配置时不应等待，实际等了 %s", d)
	}
}

// TestBatchStaggerRange 开启后在 [min,max] 内随机等待。
func TestBatchStaggerRange(t *testing.T) {
	s := New(Config{BatchStaggerMinSeconds: 1, BatchStaggerMaxSeconds: 1})
	start := time.Now()
	if !s.staggerBatchGap(context.Background()) {
		t.Fatal("区间内等待应正常返回")
	}
	if d := time.Since(start); d < 900*time.Millisecond {
		t.Fatalf("应至少等约 1s，实际 %s", d)
	}

	// 只配 max（补 min 缺省 60）时不应退化为 0 等待。
	s2 := New(Config{BatchStaggerMaxSeconds: 1})
	if got := s2.batchStaggerMin(); got != defaultBatchStaggerMinSec {
		t.Fatalf("只配 max 时 min 应回落默认 %d，得 %d", defaultBatchStaggerMinSec, got)
	}
}

// TestBatchStaggerHonorsCancel ctx 取消时不睡满（优雅停机）。
func TestBatchStaggerHonorsCancel(t *testing.T) {
	s := New(Config{BatchStaggerMinSeconds: 30, BatchStaggerMaxSeconds: 30})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if s.staggerBatchGap(ctx) {
		t.Fatal("ctx 已取消应返回 false")
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("ctx 已取消不应等待，实际 %s", d)
	}
}
