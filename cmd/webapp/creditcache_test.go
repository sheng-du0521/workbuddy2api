// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"strings"
	"testing"
	"unicode/utf8"

	"workbuddy2api/internal/upstream"
)

// ===========================================================================
// v0.24.9 积分明细缓存
//
// 为什么值得单测：这份缓存的**唯一**存在理由，是让账号卡在不产生任何出站的前提下
// 重新显示明细（2026-10-13 封号事故后卡片只剩一个总额快照）。所以这里守两条：
//  1. 缓存能正确往返，且**只在成功路径**被写入（空结果绝不能覆盖好数据）；
//  2. 排序/截断口径稳定 —— 卡片与「查看全部」弹窗共用同一份行数据。
// ===========================================================================

// TestCreditPacksRoundTrip 写入 → 读出必须逐字段一致。
func TestCreditPacksRoundTrip(t *testing.T) {
	store, px, _ := newTestApp(t, &Account{UID: "c1", AccessToken: "tok"})
	defer store.Close()
	defer px.Close()

	packs := []upstream.CreditPackage{
		{Name: "体验版", Remain: 300, Used: 200, Size: 500, ExpireAt: 1_800_000_000},
		{Name: "拉新包", Remain: 1500, Used: 0, Size: 1500, ExpireAt: 1_900_000_000},
	}
	blob := encodeCreditPacks(packs, 1_700_000_000)
	if blob == "" {
		t.Fatal("encodeCreditPacks 不该返回空串（有 2 行输入）")
	}
	if err := store.SetAccountCreditPacks("c1", blob, 1_700_000_000); err != nil {
		t.Fatalf("写缓存失败：%v", err)
	}

	got := store.AccountCreditPacks("c1")
	if got.At != 1_700_000_000 {
		t.Fatalf("缓存时刻应为 1700000000，得 %d", got.At)
	}
	if len(got.Packs) != 2 {
		t.Fatalf("应有 2 行，得 %d", len(got.Packs))
	}
	// 到期升序：体验版（1.8e9）在前。
	if got.Packs[0].Name != "体验版" || got.Packs[1].Name != "拉新包" {
		t.Fatalf("应按到期升序，得 %s / %s", got.Packs[0].Name, got.Packs[1].Name)
	}
	if got.Packs[0].Remain != 300 || got.Packs[1].Size != 1500 {
		t.Fatalf("字段未正确往返：%+v", got.Packs)
	}
}

// TestCreditPacksEmptyNeverOverwrites 空结果**不得**覆盖已有缓存 —— 这是最关键的一条：
// 上游偶发返回空列表时，卡片不能因此从「有明细」退化成「没有明细」。
func TestCreditPacksEmptyNeverOverwrites(t *testing.T) {
	store, px, _ := newTestApp(t, &Account{UID: "c2", AccessToken: "tok"})
	defer store.Close()
	defer px.Close()

	good := encodeCreditPacks([]upstream.CreditPackage{{Name: "包A", Remain: 10, Size: 10}}, 111)
	if err := store.SetAccountCreditPacks("c2", good, 111); err != nil {
		t.Fatal(err)
	}

	// 空输入：encode 返回空串 → SetAccountCreditPacks 应当**什么都不做**。
	if blob := encodeCreditPacks(nil, 222); blob != "" {
		t.Fatalf("空输入应返回空串，得 %q", blob)
	}
	if err := store.SetAccountCreditPacks("c2", "", 222); err != nil {
		t.Fatalf("空写入应静默跳过，却报错：%v", err)
	}

	got := store.AccountCreditPacks("c2")
	if len(got.Packs) != 1 || got.Packs[0].Name != "包A" {
		t.Fatalf("旧缓存被空结果覆盖了：%+v", got)
	}
	if got.At != 111 {
		t.Fatalf("时刻不该被空写入改动，得 %d", got.At)
	}
}

// TestCreditPacksCaps 行数上限与名称长度上限：给存储一个上界，且截断按到期升序
// （先丢「最晚到期」的 —— 那最不可能是运维要找的）。
func TestCreditPacksCaps(t *testing.T) {
	store, px, _ := newTestApp(t, &Account{UID: "c3", AccessToken: "tok"})
	defer store.Close()
	defer px.Close()

	longName := strings.Repeat("长", creditPackNameMax+20)
	packs := make([]upstream.CreditPackage, 0, creditPacksCacheMax+10)
	for i := 0; i < creditPacksCacheMax+10; i++ {
		packs = append(packs, upstream.CreditPackage{
			Name: longName, Remain: 1, Size: 1, ExpireAt: int64(1_000_000 + i),
		})
	}
	blob := encodeCreditPacks(packs, 1)
	if err := store.SetAccountCreditPacks("c3", blob, 1); err != nil {
		t.Fatal(err)
	}
	got := store.AccountCreditPacks("c3")
	if len(got.Packs) != creditPacksCacheMax {
		t.Fatalf("应截断到 %d 行，得 %d", creditPacksCacheMax, len(got.Packs))
	}
	// 保留的是**最早到期**的那批：最小值仍是 1_000_000。
	if got.Packs[0].ExpireAt != 1_000_000 {
		t.Fatalf("截断应保留最早到期的行，得首行到期 %d", got.Packs[0].ExpireAt)
	}
	// 按**字符**而非字节计数：按字节截会把汉字劈成半个（非法 UTF-8），
	// JSON 编码时又被补成 U+FFFD —— 长度反而变长，且末尾是乱码方块。
	if n := utf8.RuneCountInString(got.Packs[0].Name); n > creditPackNameMax {
		t.Fatalf("名称应被截到 %d 字符，得 %d", creditPackNameMax, n)
	}
	if !utf8.ValidString(got.Packs[0].Name) {
		t.Fatalf("截断后必须是合法 UTF-8，得 %q", got.Packs[0].Name)
	}
}

// TestWarmupCreditCacheSkipsMissing 预热对「库里没有的账号」必须**跳过且不出站**。
//
// 这条守的是批量预热的边界：它是管理端一键动作，输入可能来自已删除的勾选残留
// （前端勾选状态与库会短暂不一致）。不能因为一个不存在的 uid 就去打上游，
// 更不能把整批中断 —— 逐条如实回报、其余继续。
func TestWarmupCreditCacheSkipsMissing(t *testing.T) {
	store, px, a := newTestApp(t, &Account{UID: "w1", AccessToken: "tok"})
	defer store.Close()
	defer px.Close()

	rep := a.warmupCreditCache([]string{"不存在-uid", "不存在-uid-2"})
	if rep.Total != 2 || rep.Skipped != 2 || rep.OK != 0 || rep.Failed != 0 {
		t.Fatalf("两个不存在的账号应全部 skipped，得 %+v", rep)
	}
	for _, r := range rep.Rows {
		if r.Error == "" {
			t.Fatalf("跳过必须带原因，得 %+v", r)
		}
	}
	// 空输入：不 panic、不出站、返回空报告。
	if empty := a.warmupCreditCache(nil); empty.Total != 0 || len(empty.Rows) != 0 {
		t.Fatalf("空输入应返回空报告，得 %+v", empty)
	}
}

// TestCreditPacksByUIDSkipsEmpty + 坏 JSON 退化为「无缓存」：
// 缓存是纯展示增强，坏数据只能让它自己失效，绝不能把账号卡渲染弄崩。
func TestCreditPacksByUIDSkipsEmpty(t *testing.T) {
	store, px, _ := newTestApp(t,
		&Account{UID: "c4", AccessToken: "tok"},
		&Account{UID: "c5", AccessToken: "tok"})
	defer store.Close()
	defer px.Close()

	if err := store.SetAccountCreditPacks("c4",
		encodeCreditPacks([]upstream.CreditPackage{{Name: "包", Remain: 5, Size: 5}}, 7), 7); err != nil {
		t.Fatal(err)
	}
	// c5 塞一个坏 JSON：应被当作「无缓存」而不是报错。
	if _, err := store.db.Exec(`UPDATE accounts SET credit_packs = ?, credit_packs_at = 9 WHERE uid = ?`,
		"{不是合法 JSON", "c5"); err != nil {
		t.Fatal(err)
	}

	all := store.CreditPacksByUID()
	if _, ok := all["c4"]; !ok {
		t.Fatal("c4 的缓存应被读出")
	}
	if cp, ok := all["c5"]; !ok || len(cp.Packs) != 0 {
		t.Fatalf("c5 的坏 JSON 应退化为无明细，得 %+v (present=%v)", cp, ok)
	}
}

// TestCreditPacksSurviveUpsertAccount 重新 OAuth 登录（UpsertAccount）**不能**把缓存写空。
//
// 这正是刻意把 credit_packs 排除在 accountCols / UpsertAccount 之外的原因 ——
// 混进去的话，一次刷新 token 就会连带清掉展示数据（同 v0.13.0 那类坑）。
func TestCreditPacksSurviveUpsertAccount(t *testing.T) {
	store, px, _ := newTestApp(t, &Account{UID: "c6", AccessToken: "tok"})
	defer store.Close()
	defer px.Close()

	if err := store.SetAccountCreditPacks("c6",
		encodeCreditPacks([]upstream.CreditPackage{{Name: "包", Remain: 5, Size: 5}}, 42), 42); err != nil {
		t.Fatal(err)
	}
	// 模拟重新登录：带上同样的 uid 再 Upsert 一次。
	if err := store.UpsertAccount(&Account{UID: "c6", AccessToken: "tok-new", Nickname: "改名了"}); err != nil {
		t.Fatal(err)
	}
	got := store.AccountCreditPacks("c6")
	if len(got.Packs) != 1 || got.At != 42 {
		t.Fatalf("重新登录后缓存不该被清掉：%+v", got)
	}
}
