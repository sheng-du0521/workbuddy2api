// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package taskledger

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 本文件是 v0.29.0「台账清掉已移除的任务类别」的回归测试。
//
// 场景：v0.27.0 把「开学季」从任务体系里整体移除，但 task_ledger.json 里还留着
// 它最后一轮的记录 —— 执行台账上于是持续显示一张 school 卡片。它看起来像
// "这个任务还在、只是很久没跑"，比少一张卡更容易把人带偏。

func newFileStore(t *testing.T, name string) (*Store, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	s := New(p)
	if s == nil {
		t.Fatal("New 返回 nil")
	}
	return s, p
}

func mkRun(kind string) Run {
	now := time.Now()
	return Run{Kind: kind, Trigger: TriggerSchedule, Started: now, Finished: now, Total: 3, OK: 3}
}

// TestDropKindsRemovesBothRunsAndRetry 两类残留都要删：runs 与 retry。
//
// 只删 runs 是不够的：retry 里的条目同样是"这个任务存在过"的证据，而且它会
// 出现在接口的 retry_states 里 —— 界面依旧能看出一个已下线的任务。
func TestDropKindsRemovesBothRunsAndRetry(t *testing.T) {
	s, _ := newFileStore(t, "led.json")
	s.Record(mkRun("checkin"))
	s.Record(mkRun("school"))
	s.PlanRetry("school", time.Now(), time.Minute, 3)
	s.PlanRetry("checkin", time.Now(), time.Minute, 3)

	dropped := s.DropKinds(func(k string) bool { return k != "school" })
	if len(dropped) != 1 || dropped[0] != "school" {
		t.Fatalf("应恰好删掉 school，得 %v", dropped)
	}
	if _, ok := s.Runs()["school"]; ok {
		t.Errorf("runs 里的 school 应已删除")
	}
	if _, ok := s.RetryStates()["school"]; ok {
		t.Errorf("retry 里的 school 应已删除")
	}
	if _, ok := s.Runs()["checkin"]; !ok {
		t.Errorf("checkin 不该被删")
	}
	if _, ok := s.RetryStates()["checkin"]; !ok {
		t.Errorf("checkin 的 retry 不该被删")
	}
}

// TestDropKindsOnlyInRetryAlsoReported retry 里独有（runs 里没有）的类别也要删并上报。
//
// 这一条容易漏：直觉上"类别是否存在"看 runs 就够了，但 retry 是独立 map，
// 一个任务可能在写 runs 之前就排过重试（或被别处清理过 runs）。
func TestDropKindsOnlyInRetryAlsoReported(t *testing.T) {
	s, _ := newFileStore(t, "led2.json")
	s.PlanRetry("school", time.Now(), time.Minute, 3)
	dropped := s.DropKinds(func(k string) bool { return false })
	if len(dropped) != 1 || dropped[0] != "school" {
		t.Fatalf("只存在于 retry 的类别也应被删并上报，得 %v", dropped)
	}
	if len(s.RetryStates()) != 0 {
		t.Errorf("retry 应已清空，得 %v", s.RetryStates())
	}
}

// TestDropKindsPersistsAcrossReload 必须**落盘**：只在内存里删的话，重启后幽灵卡片
// 会再回来（表现为"每次重启都冒出来一次"，比不删更难理解）。
func TestDropKindsPersistsAcrossReload(t *testing.T) {
	s, p := newFileStore(t, "led3.json")
	s.Record(mkRun("school"))
	s.Record(mkRun("cat"))
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("台账文件应已落盘：%v", err)
	}
	if dropped := s.DropKinds(func(k string) bool { return k != "school" }); len(dropped) != 1 {
		t.Fatalf("应删掉 1 个，得 %v", dropped)
	}

	// 重新载入（等价于进程重启）。
	again := New(p)
	if _, ok := again.Runs()["school"]; ok {
		t.Fatalf("重启后 school 又回来了 —— 说明没落盘")
	}
	if _, ok := again.Runs()["cat"]; !ok {
		t.Errorf("cat 应在重启后仍然存在")
	}
}

// TestDropKindsIdempotent 反复调用是安全的（幂等），第二次不再报告任何删除。
func TestDropKindsIdempotent(t *testing.T) {
	s, _ := newFileStore(t, "led4.json")
	s.Record(mkRun("school"))
	if got := s.DropKinds(func(string) bool { return false }); len(got) != 1 {
		t.Fatalf("第一次应删 1 个，得 %v", got)
	}
	if got := s.DropKinds(func(string) bool { return false }); len(got) != 0 {
		t.Errorf("第二次不该再报删除，得 %v", got)
	}
	// 全保留的白名单：不删任何东西。
	s.Record(mkRun("checkin"))
	if got := s.DropKinds(func(string) bool { return true }); len(got) != 0 {
		t.Errorf("全部保留时不该删任何东西，得 %v", got)
	}
}

// TestDropKindsSafeOnEmptyAndNil 空台账 / nil 回调不 panic（调用点在启动路径上）。
func TestDropKindsSafeOnEmptyAndNil(t *testing.T) {
	var nilStore *Store
	if got := nilStore.DropKinds(func(string) bool { return true }); got != nil {
		t.Errorf("nil Store 应安全返回 nil，得 %v", got)
	}
	s, _ := newFileStore(t, "led5.json")
	if got := s.DropKinds(nil); got != nil {
		t.Errorf("nil keep 应安全返回 nil（不做任何裁剪），得 %v", got)
	}
	if got := s.DropKinds(func(string) bool { return false }); len(got) != 0 {
		t.Errorf("空台账应得空结果，得 %v", got)
	}
}

// TestDropKindsMemoryOnly 纯内存台账（path 为空）不落盘也不报错。
func TestDropKindsMemoryOnly(t *testing.T) {
	s := New("")
	s.Record(mkRun("school"))
	if got := s.DropKinds(func(k string) bool { return k != "school" }); len(got) != 1 {
		t.Fatalf("内存台账也应能裁剪，得 %v", got)
	}
	if _, ok := s.Runs()["school"]; ok {
		t.Errorf("内存台账里 school 应已删除")
	}
}

// TestDropKindsResultSorted 返回顺序稳定（便于日志阅读与测试断言）。
func TestDropKindsResultSorted(t *testing.T) {
	s, _ := newFileStore(t, "led6.json")
	for _, k := range []string{"zeta", "alpha", "mid"} {
		s.Record(mkRun(k))
	}
	got := s.DropKinds(func(string) bool { return false })
	want := []string{"alpha", "mid", "zeta"}
	if len(got) != len(want) {
		t.Fatalf("应删 3 个，得 %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("返回应排序为 %v，得 %v", want, got)
		}
	}
}
