// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"testing"

	"workbuddy2api/internal/scheduler"
)

// TestGrowthMarkDoneRecordsTimestamp 执行一轮后应记录时间戳（供账号卡展示
// 「上次跑于 X」），且不影响其他账号。
func TestGrowthMarkDoneRecordsTimestamp(t *testing.T) {
	store, _, _ := newTestApp(t,
		&Account{UID: "n1", Realm: "cn", AccessToken: "a"},
		&Account{UID: "n2", Realm: "cn", AccessToken: "b"},
	)
	defer store.Close()

	if err := store.MarkGrowthDone("n1"); err != nil {
		t.Fatalf("MarkGrowthDone: %v", err)
	}
	a1, err := store.GetAccount("n1")
	if err != nil || a1 == nil {
		t.Fatalf("GetAccount(n1): %v", err)
	}
	if a1.GrowthDoneAt == 0 {
		t.Fatal("n1 应记录执行时间")
	}
	a2, err := store.GetAccount("n2")
	if err != nil || a2 == nil {
		t.Fatalf("GetAccount(n2): %v", err)
	}
	if a2.GrowthDoneAt != 0 {
		t.Fatalf("n2 未被选中，不应有时间戳，得 %d", a2.GrowthDoneAt)
	}
}

// TestGrowthDoneNotWipedByReupsert 重登 / 重新同步不得抹掉成长任务时间戳。
//
// 这是 UpsertAccount 那条「ON CONFLICT 刻意不更新业务状态列」约定的回归：
// 一次 OAuth 刷新若把 growth_done_at 打回 0，账号卡上就会显示成「从未跑过」，
// 诱导管理员重复选它跑一遍。
func TestGrowthDoneNotWipedByReupsert(t *testing.T) {
	store, _, _ := newTestApp(t, &Account{UID: "k1", Realm: "cn", AccessToken: "a"})
	defer store.Close()

	if err := store.MarkGrowthDone("k1"); err != nil {
		t.Fatalf("MarkGrowthDone: %v", err)
	}
	before, _ := store.GetAccount("k1")

	// 模拟重新 OAuth 登录：同 uid 再 Upsert 一次（token 变了、业务状态不该动）。
	if err := store.UpsertAccount(&Account{UID: "k1", Realm: "cn", AccessToken: "new-token", Nickname: "改个名"}); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	acc, err := store.GetAccount("k1")
	if err != nil || acc == nil {
		t.Fatalf("GetAccount: %v", err)
	}
	if acc.GrowthDoneAt != before.GrowthDoneAt {
		t.Fatalf("重登后时间戳被改写（应保留 %d，得 %d）", before.GrowthDoneAt, acc.GrowthDoneAt)
	}
	if acc.AccessToken != "new-token" || acc.Nickname != "改个名" {
		t.Fatalf("token/昵称应更新，得 token=%q nick=%q", acc.AccessToken, acc.Nickname)
	}
}

// TestAccountsByUIDsKeepsOrderAndSkipsMissing 批量取账号：
// 顺序与入参一致（日志可对照选择顺序），缺失的静默跳过。
func TestAccountsByUIDsKeepsOrderAndSkipsMissing(t *testing.T) {
	store, _, _ := newTestApp(t,
		&Account{UID: "b", Realm: "cn", AccessToken: "1"},
		&Account{UID: "a", Realm: "cn", AccessToken: "2"},
		&Account{UID: "c", Realm: "cn", AccessToken: "3"},
	)
	defer store.Close()

	got, err := store.AccountsByUIDs([]string{"c", "nope", "a", "b"})
	if err != nil {
		t.Fatalf("AccountsByUIDs: %v", err)
	}
	want := []string{"c", "a", "b"}
	if len(got) != len(want) {
		t.Fatalf("应返回 %d 个，得 %d（%v）", len(want), len(got), accountUIDs(got))
	}
	for i, u := range want {
		if got[i].UID != u {
			t.Fatalf("第 %d 个应为 %s，得 %s（%v）", i, u, got[i].UID, accountUIDs(got))
		}
	}
	if empty, err := store.AccountsByUIDs(nil); err != nil || len(empty) != 0 {
		t.Fatalf("空入参应返回空结果，得 %v err=%v", empty, err)
	}
}

// TestGrowthProviderCredsByUID 按 uid 取凭证：顺序随入参、无 token 的滤掉、
// 字段完整映射（access token 已在读取边界解密）。
func TestGrowthProviderCredsByUID(t *testing.T) {
	store, _, _ := newTestApp(t,
		&Account{UID: "t1", Realm: "cn", AccessToken: ""},   // 只有 refresh 的形态
		&Account{UID: "t2", Realm: "cn", AccessToken: "ok"}, // 有 token
		&Account{UID: "t3", Realm: "global", AccessToken: "g", Nickname: "球"},
	)
	defer store.Close()

	g := growthProvider{store: store}
	cred, err := g.GrowthCredsByUID([]string{"t3", "t1", "t2"})
	if err != nil {
		t.Fatalf("GrowthCredsByUID: %v", err)
	}
	// t1 无 token 被滤掉，剩余顺序仍随入参（t3 在前）。
	if len(cred) != 2 || cred[0].UID != "t3" || cred[1].UID != "t2" {
		t.Fatalf("应保留 [t3 t2]，得 %v", credUIDs(cred))
	}
	if cred[0].AccessToken != "g" || cred[0].Realm != "global" || cred[0].Nickname != "球" {
		t.Fatalf("凭证字段应完整映射，得 %+v", cred[0])
	}
	if cred[1].AccessToken != "ok" {
		t.Fatalf("t2 的 token 应为 ok，得 %q", cred[1].AccessToken)
	}

	// 未选中的账号不出现在结果里（手动选号的语义：选谁跑谁）。
	only, _ := g.GrowthCredsByUID([]string{"t3"})
	if len(only) != 1 || only[0].UID != "t3" {
		t.Fatalf("只选 t3 时不应带出其他账号，得 %v", credUIDs(only))
	}
}

func accountUIDs(list []*Account) []string {
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, a.UID)
	}
	return out
}

func credUIDs(list []scheduler.GrowthCred) []string {
	out := make([]string, 0, len(list))
	for _, c := range list {
		out = append(out, c.UID)
	}
	return out
}
