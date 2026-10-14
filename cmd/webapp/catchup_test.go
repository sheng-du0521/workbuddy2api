// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"testing"
)

// 本文件覆盖 v0.23.3 的「新入池账号补跑」链路：
//   SyncAccountsReport 发现新增 → onAccountsAdded 钩子 → taskRunner.CatchupNewAccounts
//
// 背景（用户反馈的现象）：「定时任务新加入账号未执行」。两个成因：
//  1. 调度器侧槽位被吞导致某些任务整轮不跑（见 internal/scheduler/missed_slot_test.go）；
//  2. 账号在**当天触发点之后**入池时，当天不会再被排到（最长 ~21h）——
//     这里覆盖第 2 条：入池即补跑一轮签到。

// TestSyncReportNotifiesAddedAccounts 同步发现新增账号时必须回调钩子，
// 且「首次同步只建基线」不得触发（否则每次重启都会给全池补跑一轮，等于自造流量）。
func TestSyncReportNotifiesAddedAccounts(t *testing.T) {
	store, px, _ := newTestApp(t, &Account{UID: "u1", Realm: "cn", AccessToken: "at"})
	defer store.Close()

	var calls [][]string
	px.onAccountsAdded = func(uids []string) { calls = append(calls, uids) }

	// 首次同步：只建基线 → 不通知。
	if _, err := px.SyncAccountsReport(); err != nil {
		t.Fatalf("首次同步: %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("首次同步不该通知新增（否则重启即给全池补跑），got %v", calls)
	}

	// 新增两个账号 → 通知一次，且集合恰好是这两个。
	for _, uid := range []string{"u2", "u3"} {
		if err := store.UpsertAccount(&Account{UID: uid, Realm: "cn", AccessToken: "at"}); err != nil {
			t.Fatalf("UpsertAccount(%s): %v", uid, err)
		}
	}
	if _, err := px.SyncAccountsReport(); err != nil {
		t.Fatalf("二次同步: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("应恰好通知一次，got %d 次：%v", len(calls), calls)
	}
	got := map[string]bool{}
	for _, u := range calls[0] {
		got[u] = true
	}
	if len(calls[0]) != 2 || !got["u2"] || !got["u3"] {
		t.Fatalf("新增集合应为 {u2,u3}，got %v", calls[0])
	}

	// 只删除、无新增 → 不再通知。
	if err := store.DeleteAccount("u1"); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	if _, err := px.SyncAccountsReport(); err != nil {
		t.Fatalf("三次同步: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("无新增时不该通知，got %v", calls)
	}
}

// TestSyncReportHookNilIsSafe 钩子为 nil（-no-tasks 启动路径 / 单测不装配）时不 panic。
func TestSyncReportHookNilIsSafe(t *testing.T) {
	store, px, _ := newTestApp(t, &Account{UID: "u1", Realm: "cn", AccessToken: "at"})
	defer store.Close()

	px.onAccountsAdded = nil // 显式不装配
	px.SyncAccountsReport()  // 建基线
	if err := store.UpsertAccount(&Account{UID: "u2", Realm: "cn", AccessToken: "at"}); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	if _, err := px.SyncAccountsReport(); err != nil {
		t.Fatalf("钩子为 nil 时不该出错：%v", err)
	}
}

// TestCatchupSkipReasonRespectsConfig 补跑必须尊重运营配置。
//
// 这条规则的意义：补跑是系统自动发起的上游写入。若在「定时任务总开关关闭」或
// 「自动签到被单独禁用」时仍然补跑，就等于绕过运营显式关掉的东西 —— 而运营关掉
// 它往往正是因为风控/上游压力。所以两种情况都必须闭嘴。
func TestCatchupSkipReasonRespectsConfig(t *testing.T) {
	cases := []struct {
		name string
		t    *taskRunner
		want string // 期望的原因关键字，空 = 允许补跑
	}{
		{"nil runner", nil, "未启用"},
		{"总开关关闭", &taskRunner{cfg: TaskConfig{Enabled: false}}, "总开关"},
		{"签到被禁用", &taskRunner{cfg: TaskConfig{Enabled: true, CheckinDisabled: true}}, "签到"},
		{"正常配置", &taskRunner{cfg: TaskConfig{Enabled: true}}, ""},
	}
	for _, c := range cases {
		got := c.t.catchupSkipReason()
		if c.want == "" {
			if got != "" {
				t.Errorf("%s：应允许补跑，got 拒绝原因 %q", c.name, got)
			}
			continue
		}
		if got == "" {
			t.Errorf("%s：应拒绝补跑，got 允许", c.name)
		}
	}
}

// TestCatchupTargetsDedupesAndCaps 待补跑列表要「去空、去重、有上限」。
//
// 上限存在的原因：一次导入上千个账号时，补跑不能被无界放大成一串后台出站。
func TestCatchupTargetsDedupesAndCaps(t *testing.T) {
	got := catchupTargets([]string{"a", "", "b", "a", "  ", "c"})
	if len(got) != 3 {
		t.Fatalf("应去空去重为 3 个，got %v", got)
	}
	if got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("应保持首次出现顺序，got %v", got)
	}

	// 超过上限时截断到 creditsRefreshMaxAccounts。
	big := make([]string, 0, creditsRefreshMaxAccounts+50)
	for i := 0; i < creditsRefreshMaxAccounts+50; i++ {
		big = append(big, string(rune('A'+i%26))+string(rune('a'+i/26)))
	}
	if n := len(catchupTargets(big)); n != creditsRefreshMaxAccounts {
		t.Fatalf("应截断到 %d，got %d", creditsRefreshMaxAccounts, n)
	}
	if n := len(catchupTargets(nil)); n != 0 {
		t.Fatalf("空输入应为空列表，got %d", n)
	}
}

// TestCatchupDelayFixedOverride 固定延迟覆盖按约定生效（测试可缩短）。
func TestCatchupDelayFixedOverride(t *testing.T) {
	old := catchupDelayFixed
	defer func() { catchupDelayFixed = old }()

	catchupDelayFixed = 0
	d := catchupDelay()
	if d < catchupMinDelay*1e9 || d > catchupMaxDelay*1e9+1e9 {
		t.Fatalf("随机延迟应落在 [%ds,%ds]，got %v", catchupMinDelay, catchupMaxDelay, d)
	}

	catchupDelayFixed = 1
	if got := catchupDelay(); got != 1 {
		t.Fatalf("固定延迟覆盖应生效，got %v", got)
	}
}

// TestCatchupNewAccountsNoopOnEmpty 空 uid 列表直接返回（不起 goroutine）。
func TestCatchupNewAccountsNoopOnEmpty(t *testing.T) {
	var tr *taskRunner
	tr.CatchupNewAccounts(nil)                     // nil 接收者：不该 panic
	(&taskRunner{}).CatchupNewAccounts([]string{}) // 空列表
	(&taskRunner{cfg: TaskConfig{Enabled: true}}).CatchupNewAccounts([]string{"", ""})
}
