// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/taskledger"
)

// 本文件是 v0.29.0「执行台账里 school 卡片残留」的回归测试。
//
// 两层修复都要钉住：
//  1. **启动清理**（PruneLedger → 落盘）—— 让幽灵记录从文件里消失；
//  2. **出口过滤**（filterLedgerRuns/Retries）—— 让任何来源的旧数据都进不了界面。

// TestKnownLedgerKind 存在性白名单：排程类五类 + 成长任务为"存在"，已移除的为"不存在"。
//
// 白名单（而不是"黑名单里加 school"）是本条的核心：黑名单只能记住**已经**删掉的，
// 下次删任务时又要记得回来加一条 —— 漏掉就再次出现幽灵卡片。
func TestKnownLedgerKind(t *testing.T) {
	for k := range runnableKinds {
		if !knownLedgerKind(k) {
			t.Errorf("排程类任务 %q 应被视为存在", k)
		}
	}
	// 成长任务不在一键触发清单里，但它确实会写台账 —— 漏掉它会把历史记录当残留删掉。
	if !knownLedgerKind(ledgerKindGrowth) {
		t.Errorf("成长任务（%s）不在一键清单里，但它会写台账，必须视为存在", ledgerKindGrowth)
	}
	for _, gone := range []string{"school", "", "SCHOOL", "unknown_future_task"} {
		if knownLedgerKind(gone) {
			t.Errorf("已移除 / 不存在的类别 %q 不该被视为存在", gone)
		}
	}
	// 必须**不等于** runnableKinds 的成员判定：否则将来有人把 growth 加进
	// runnableKinds（那会让"一键跑一类"多出一个不该有的入口）时这条测试不会察觉。
	if _, ok := runnableKinds[ledgerKindGrowth]; ok {
		t.Errorf("growth 不应出现在 runnableKinds（它需要带 uid 列表，走独立入口）")
	}
}

func lkRun(kind string, ok int) taskledger.Run {
	now := time.Now()
	return taskledger.Run{Kind: kind, Trigger: taskledger.TriggerSchedule,
		Started: now, Finished: now, Total: ok, OK: ok}
}

// TestFilterLedgerRunsDropsRemovedKinds 出口过滤：未知类别一律不下发。
func TestFilterLedgerRunsDropsRemovedKinds(t *testing.T) {
	in := map[string]taskledger.Run{
		"checkin": lkRun("checkin", 27),
		"school":  lkRun("school", 33),
		"growth":  lkRun("growth", 7),
		"mystery": lkRun("mystery", 1),
	}
	out := filterLedgerRuns(in)
	if len(out) != 2 {
		t.Fatalf("应只剩 checkin 与 growth，得 %v", lkKeysOf(out))
	}
	if _, ok := out["school"]; ok {
		t.Errorf("school 不该出现在下发的台账里")
	}
	// 必须是**新 map**：原地删会污染调用方（下一处调用者拿到的是已删过的视图）。
	if len(in) != 4 {
		t.Errorf("原 map 不该被修改（filter 是只读动作），得 %d 项", len(in))
	}

	rets := map[string]taskledger.RetryState{
		"checkin": {Day: "2026-10-15", Used: 1},
		"school":  {Day: "2026-10-15", Used: 2},
	}
	ro := filterLedgerRetries(rets)
	if len(ro) != 1 {
		t.Fatalf("重试状态也应过滤，得 %v", ro)
	}
	if _, ok := ro["school"]; ok {
		t.Errorf("school 的重试状态不该下发")
	}
	if len(rets) != 2 {
		t.Errorf("原 map 不该被修改")
	}
}

func lkKeysOf(m map[string]taskledger.Run) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestPruneLedgerCleansFileAndStaysIdempotent 启动清理真的会改写台账文件，且可重复调用。
//
// 这一条是"用户能不能看到 school 卡片消失"的直接证据：断言的是**磁盘上的文件**，
// 而不只是内存。
func TestPruneLedgerCleansFileAndStaysIdempotent(t *testing.T) {
	store, px, _ := newTestApp(t)
	defer store.Close()
	dataDir := t.TempDir()
	ledgerPath := filepath.Join(dataDir, "task_ledger.json")

	// 造一份"上一版留下的"台账：school + checkin。
	seed := `{"runs":{"school":{"kind":"school","trigger":"manual","started":"2026-10-15T16:52:56+08:00",` +
		`"finished":"2026-10-15T16:53:13+08:00","elapsed":"16.8s","total":1,"ok":1},` +
		`"checkin":{"kind":"checkin","trigger":"schedule","started":"2026-10-15T09:04:54+08:00",` +
		`"finished":"2026-10-15T09:05:12+08:00","elapsed":"17.5s","total":27,"already":27}},` +
		`"retry":{"school":{"day":"2026-10-15","used":2}}}`
	if err := os.WriteFile(ledgerPath, []byte(seed), 0o600); err != nil {
		t.Fatalf("写种子台账：%v", err)
	}

	tr := newTaskRunner(px, store, dataDir)
	if tr.sch == nil {
		t.Fatal("调度器未构造")
	}
	// 先确认种子确实被读进来了（否则后面的断言可能只是"本来就没有 school"）。
	if _, ok := tr.sch.Ledger().Runs()["school"]; !ok {
		t.Fatalf("种子台账里的 school 未被载入，用例前提不成立")
	}

	dropped := tr.PruneLedger()
	if len(dropped) != 1 || dropped[0] != "school" {
		t.Fatalf("应清掉 school，得 %v", dropped)
	}
	raw, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatalf("读回台账：%v", err)
	}
	if lkContains(raw, "school") {
		t.Errorf("台账文件里仍有 school —— 清理没有落盘：%s", raw)
	}
	if !lkContains(raw, "checkin") {
		t.Errorf("checkin 被误删：%s", raw)
	}
	// 幂等：再调一次不该报告任何删除（启动路径可能被重放）。
	if again := tr.PruneLedger(); len(again) != 0 {
		t.Errorf("重复清理不该再报删除，得 %v", again)
	}
}

// containsStrBytes 避免为此引入 bytes 包（只在测试里用一次）。
func lkContains(b []byte, sub string) bool {
	return len(sub) == 0 || lkIndex(b, sub) >= 0
}

func lkIndex(b []byte, sub string) int {
	n, m := len(b), len(sub)
	for i := 0; i+m <= n; i++ {
		if string(b[i:i+m]) == sub {
			return i
		}
	}
	return -1
}
