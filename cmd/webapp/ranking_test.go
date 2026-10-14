// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"strconv"
	"testing"
	"time"
)

// seedUsage 写一条窗口内的用量流水。
func seedUsage(t *testing.T, store *Store, userID int64, chargedMicro int64, reqs int) {
	t.Helper()
	if err := store.InsertUsageLog(&UsageLog{
		UserID: userID, KeyID: 1, Model: "m", PromptTokens: 10, CompletionTokens: 10,
		ChargedMicro: chargedMicro, UpstreamUID: "u", Scope: ScopePool, Status: "ok",
		CreatedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("InsertUsageLog: %v", err)
	}
	_ = reqs
}

// TestUsageByUserOrdering 排序依据：消耗降序 → 请求数降序 → user_id 升序。
func TestUsageByUserOrdering(t *testing.T) {
	store, px, _ := newTestApp(t)
	defer store.Close()
	defer px.Close()

	alice, _ := store.CreateUser("alice", "h")
	bob, _ := store.CreateUser("bob", "h")
	carol, _ := store.CreateUser("carol", "h")

	seedUsage(t, store, alice.ID, 1000, 1)
	seedUsage(t, store, bob.ID, 5000, 1)
	seedUsage(t, store, carol.ID, 1000, 1) // 与 alice 同分 → 按 user_id 升序，alice 在前

	rows, err := store.UsageByUser(0, 14, 0)
	if err != nil {
		t.Fatalf("UsageByUser: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("应有 3 个用户，得 %d", len(rows))
	}
	want := []int64{bob.ID, alice.ID, carol.ID}
	for i, w := range want {
		if rows[i].UserID != w {
			t.Fatalf("第 %d 名应为 user=%d，得 %d（完整顺序 %+v）", i+1, w, rows[i].UserID, rows)
		}
	}
	if rows[0].Username != "bob" || rows[0].ChargedMicro != 5000 {
		t.Fatalf("明细不符：%+v", rows[0])
	}
}

// TestUsageByUserExcludesAdminChannel 管理密钥通道（user_id=0）不进榜。
//
// v0.17.6 起管理通道也写台账（费用恒 0），若不排除就会凭空多出一行「0 号用户」，
// 并把 total_users 抬高一格。
func TestUsageByUserExcludesAdminChannel(t *testing.T) {
	store, px, a := newTestApp(t)
	defer store.Close()
	defer px.Close()

	alice, _ := store.CreateUser("alice", "h")
	seedUsage(t, store, alice.ID, 1234, 1)
	seedUsage(t, store, 0, 0, 1) // 管理通道

	rows, err := store.UsageByUser(0, 14, 0)
	if err != nil {
		t.Fatalf("UsageByUser: %v", err)
	}
	if len(rows) != 1 || rows[0].UserID != alice.ID {
		t.Fatalf("应只有 alice 一个用户，得 %+v", rows)
	}
	total, err := store.CountUsageUsers(14)
	if err != nil {
		t.Fatalf("CountUsageUsers: %v", err)
	}
	if total != 1 {
		t.Fatalf("分母应为 1（不含 user_id=0），得 %d", total)
	}

	// 管理端视角：完整用户名 + user_id。
	admin := a.usageRanking(0, 14, 0)
	list := admin["list"].([]map[string]any)
	if len(list) != 1 {
		t.Fatalf("管理端榜单应 1 条，得 %d", len(list))
	}
	row := list[0]
	if row["name"] != "alice" || row["user_id"] != alice.ID {
		t.Fatalf("管理端应给完整用户名与 id，得 %+v", row)
	}
	if _, hasMe := admin["me"]; hasMe {
		t.Fatalf("管理端视角不应有 me 块")
	}
}

// TestUsageRankingSelfMasked 用户视角：他人用户名脱敏、自己原样、附自己的名次。
func TestUsageRankingSelfMasked(t *testing.T) {
	store, px, a := newTestApp(t)
	defer store.Close()
	defer px.Close()

	alice, _ := store.CreateUser("alice", "h")
	bob, _ := store.CreateUser("bobbobbob", "h")
	seedUsage(t, store, alice.ID, 900, 1)
	seedUsage(t, store, bob.ID, 100, 1)

	view := a.usageRanking(bob.ID, 14, 0)
	if view["masked"] != true {
		t.Fatalf("用户视角应标记 masked")
	}
	list := view["list"].([]map[string]any)
	if len(list) != 2 {
		t.Fatalf("应 2 条，得 %d", len(list))
	}
	// 第一名是 alice（900），对 bob 而言应脱敏。
	first := list[0]
	if first["name"] == "alice" {
		t.Fatalf("他人用户名不应原样出现，得 %v", first["name"])
	}
	if first["name"] != maskUsername("alice") {
		t.Fatalf("他人用户名应为脱敏形态，得 %v，期望 %v", first["name"], maskUsername("alice"))
	}
	if _, hasID := first["user_id"]; hasID {
		t.Fatalf("用户视角不应暴露他人 user_id")
	}
	// 自己那一行原样且 is_me。
	me := list[1]
	if me["name"] != "bobbobbob" || me["is_me"] != true {
		t.Fatalf("自己那一行应原样且标记 is_me，得 %+v", me)
	}
	meBlock := view["me"].(map[string]any)
	if meBlock["rank"] != 2 || meBlock["in_top"] != true {
		t.Fatalf("me 块名次应为 2 且 in_top，得 %+v", meBlock)
	}
	if meBlock["total_users"] != 2 {
		t.Fatalf("分母应为 2，得 %v", meBlock["total_users"])
	}
}

// TestUsageRankingMeOutsideTop 未进前 N 名时仍要给出自己的名次（否则用户只看到「榜上没我」）。
func TestUsageRankingMeOutsideTop(t *testing.T) {
	store, px, a := newTestApp(t)
	defer store.Close()
	defer px.Close()

	// 12 个用户各消耗递增：把「我」放最后，榜只取前 10。
	var me *User
	for i := 0; i < 12; i++ {
		u, err := store.CreateUser("rankuser"+strconv.Itoa(i), "h")
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		seedUsage(t, store, u.ID, int64(1000-i*10), 1)
		me = u // 最后一个 = 消耗最低 = 第 12 名
	}
	view := a.usageRanking(me.ID, 14, 0)
	list := view["list"].([]map[string]any)
	if len(list) != rankingUserLimit {
		t.Fatalf("用户侧榜应限制为 %d 条，得 %d", rankingUserLimit, len(list))
	}
	meBlock, ok := view["me"].(map[string]any)
	if !ok {
		t.Fatalf("应有 me 块，得 %+v", view)
	}
	if meBlock["rank"] != 12 {
		t.Fatalf("me 名次应为 12，得 %v", meBlock["rank"])
	}
	if meBlock["in_top"] != false {
		t.Fatalf("未进前 %d 时 in_top 应为 false，得 %v", rankingUserLimit, meBlock["in_top"])
	}
	for _, row := range list {
		if row["is_me"] == true {
			t.Fatalf("未进前 N 时榜单里不该有自己的行")
		}
	}
}

// TestUsageRankingEmpty 空数据不崩、分母为 0、me 有名次 0。
func TestUsageRankingEmpty(t *testing.T) {
	store, px, a := newTestApp(t)
	defer store.Close()
	defer px.Close()
	u, _ := store.CreateUser("alice", "h")

	view := a.usageRanking(u.ID, 14, 0)
	if list := view["list"].([]map[string]any); len(list) != 0 {
		t.Fatalf("无数据时榜单应为空，得 %d", len(list))
	}
	if view["total_users"] != 0 {
		t.Fatalf("分母应为 0，得 %v", view["total_users"])
	}
	meBlock := view["me"].(map[string]any)
	if meBlock["rank"] != 0 || meBlock["in_top"] != false {
		t.Fatalf("无数据时名次应为 0，得 %+v", meBlock)
	}
	admin := a.usageRanking(0, 14, 0)
	if admin["list"] == nil {
		t.Fatalf("管理端空榜也应返回空数组而非 null")
	}
}

// TestMaskUsername 用户名脱敏形态。
func TestMaskUsername(t *testing.T) {
	cases := map[string]string{
		"":          "匿名用户",
		"a":         "a**",
		"ab":        "a*",
		"alice":     "a***e",
		"bobbobbob": "b***b",
	}
	for in, want := range cases {
		if got := maskUsername(in); got != want {
			t.Errorf("maskUsername(%q) = %q, want %q", in, got, want)
		}
	}
	// 长度不可推断：不同长度输入产生等长的掩码（除首尾）。
	if len(maskUsername("alice")) != len(maskUsername("verylongusername")) {
		t.Fatalf("掩码长度应不随原文长度变化")
	}
}

// TestClampDays days 参数收敛（缺省 14、上限 90、非法回落）。
func TestClampDays(t *testing.T) {
	cases := []struct {
		raw  string
		def  int
		max  int
		want int
	}{
		{"", 14, 90, 14},
		{"abc", 14, 90, 14},
		{"0", 14, 90, 14},
		{"-3", 14, 90, 14},
		{"7", 14, 90, 7},
		{"90", 14, 90, 90},
		{"9999", 14, 90, 90},
	}
	for _, c := range cases {
		if got := clampDays(c.raw, c.def, c.max); got != c.want {
			t.Errorf("clampDays(%q) = %d, want %d", c.raw, got, c.want)
		}
	}
}
