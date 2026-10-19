// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 出口线路「归属」（v0.31.23）的回归测试。
//
// 背景：线路池此前只有"有哪些 URL"，看不出「这条线是谁接进来的、现在谁的号在用它」。
// 多个用户各自配了自己的出口之后，管理后台需要把**每一台不同出口的机主**
// 映射成一条**可独立分配**的线路 —— 归属就是这条映射的键。
//
// 本文件钉住四件事：
//  1. 归属能持久化，且**粘性**（后写的 0 不得抹掉先认领的）；
//  2. 用户在控制台配的出口**自动入池**，且归属如实落到该用户；
//  3. ⚠️ **安全边界**：用户提供的线路**不进自动选线**（自动挑一条最闲的会把
//     别人的 access_token 从我们控制不了的第三方出口送出去）；
//  4. ⚠️ 同一条边界也管**自动迁移**（线路挂了不把号自动搬到用户线路上）。
package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// ownerApp 装配：管理员 + 普通用户各一条会话，并给普通用户挂一个代挂号。
func ownerApp(t *testing.T) (store *Store, a *app, mux *http.ServeMux, adminCookie, memberCookie string, memberID int64) {
	t.Helper()
	store, a, mux, adminCookie, memberCookie = eaApp(t)
	users, err := store.ListUsers()
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	for _, u := range users {
		if u.Username == "ea-member" {
			memberID = u.ID
		}
	}
	if memberID == 0 {
		t.Fatal("装配失败：找不到 ea-member")
	}
	if err := store.UpsertAccount(&Account{
		UID: "ow-acct", Nickname: "甲", Realm: "cn",
		AccessToken: "tok", RefreshToken: "rtok",
		ConsumeGroup: "apply", PoolState: "", OwnerUserID: memberID,
	}); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	return store, a, mux, adminCookie, memberCookie, memberID
}

// findProxy 按 URL 找一条线路（找不到返回 nil）。
func findProxy(t *testing.T, store *Store, url string) *EgressProxy {
	t.Helper()
	list, err := store.ListEgressProxies()
	if err != nil {
		t.Fatalf("ListEgressProxies: %v", err)
	}
	for _, p := range list {
		if strings.TrimSpace(p.URL) == url {
			return p
		}
	}
	return nil
}

// TestEgressOwnerStickyAndRoundTrip 归属持久化 + 粘性（0 不得覆盖已认领的）。
func TestEgressOwnerStickyAndRoundTrip(t *testing.T) {
	store, _, _, _, _, _ := ownerApp(t)

	p := &EgressProxy{ID: "ow-1", Name: "甲", URL: "http://203.0.113.9:3128", Enabled: true, OwnerUserID: 7}
	if err := store.UpsertEgressProxy(p); err != nil {
		t.Fatalf("UpsertEgressProxy: %v", err)
	}
	got, err := store.GetEgressProxy("ow-1")
	if err != nil || got == nil {
		t.Fatalf("GetEgressProxy: %v %v", got, err)
	}
	if got.OwnerUserID != 7 {
		t.Fatalf("归属应持久化为 7，得 %d（列没进 scan/cols？）", got.OwnerUserID)
	}

	// 再写一次，owner 给 0（模拟"管理员手工登记同一条 URL"）。
	// 若这里变成 0，等于事后追责时线索被抹掉。
	p2 := &EgressProxy{ID: "ow-1", Name: "改过名", URL: "http://203.0.113.9:3128", Enabled: true, OwnerUserID: 0}
	if err := store.UpsertEgressProxy(p2); err != nil {
		t.Fatalf("UpsertEgressProxy(2): %v", err)
	}
	got, _ = store.GetEgressProxy("ow-1")
	if got.OwnerUserID != 7 {
		t.Fatalf("归属必须粘性（0 不得覆盖 7），得 %d", got.OwnerUserID)
	}
	if got.Name != "改过名" {
		t.Fatalf("其余字段仍应被更新，得 name=%q", got.Name)
	}
}

// TestEgressAssignAutoPickSkipsUserLines ⚠️ 安全边界：自动选线不碰用户线路。
//
// 为什么用例要写成"**池里只有用户线路**"这种极端形态：如果同时摆一条平台线路，
// 规划器按 created_at / 负载排序**恰好**会选中平台线路 —— 那样即便把排除逻辑删掉，
// 用例照样绿（没有牙齿）。只留用户线路，才能让"它有没有被当成候选"这件事显形。
func TestEgressAssignAutoPickSkipsUserLines(t *testing.T) {
	store, _, mux, adminCookie, _, memberID := ownerApp(t)

	const userURL = "http://203.0.113.7:3128"
	if err := store.UpsertEgressProxy(&EgressProxy{
		ID: "p-user", Name: "用户线路", URL: userURL, Enabled: true, OwnerUserID: memberID,
	}); err != nil {
		t.Fatalf("UpsertEgressProxy: %v", err)
	}
	if err := store.SetAccountEgress("ow-acct", ""); err != nil {
		t.Fatalf("SetAccountEgress: %v", err)
	}

	// ① 池里**只有**一条用户线路 → 自动分配必须**明确拒绝**，且不动账号。
	code, raw := eaReq(t, mux, "POST", "/api/admin/egress/assign",
		`{"all":true,"dry_run":false}`, adminCookie, "")
	if code != http.StatusBadRequest {
		t.Fatalf("只剩用户线路时自动分配应 400（无可用候选），得 %d %s", code, raw)
	}
	acc, err := store.GetAccount("ow-acct")
	if err != nil || acc == nil {
		t.Fatalf("GetAccount: %v %v", acc, err)
	}
	if got := strings.TrimSpace(acc.EgressProxy); got != "" {
		t.Fatalf("账号不该被自动分配到用户线路上，得 %q —— "+
			"那会把它的 access_token 从我们控制不了的第三方出口送出去", got)
	}

	// ② 补一条平台线路 → 自动分配落到平台线路上（对照：证明 ①不是"分配整个坏了"）。
	const platURL = "http://10.0.0.1:8080"
	if err := store.UpsertEgressProxy(&EgressProxy{
		ID: "p-plat", Name: "平台线路", URL: platURL, Enabled: true,
	}); err != nil {
		t.Fatalf("UpsertEgressProxy(plat): %v", err)
	}
	if code, raw := eaReq(t, mux, "POST", "/api/admin/egress/assign",
		`{"all":true,"dry_run":false}`, adminCookie, ""); code != http.StatusOK {
		t.Fatalf("自动分配应 200，得 %d %s", code, raw)
	}
	acc, _ = store.GetAccount("ow-acct")
	if got := strings.TrimSpace(acc.EgressProxy); got != platURL {
		t.Fatalf("自动分配应落在平台线路 %s，得 %q", platURL, got)
	}

	// ③ 显式指名则**允许**（管理员在卡片上点「分配」就是这条路径）。
	if code, raw := eaReq(t, mux, "POST", "/api/admin/egress/assign",
		`{"all":true,"proxy_url":"`+userURL+`","dry_run":false}`, adminCookie, ""); code != http.StatusOK {
		t.Fatalf("显式指名用户线路应 200，得 %d %s", code, raw)
	}
	acc, _ = store.GetAccount("ow-acct")
	if got := strings.TrimSpace(acc.EgressProxy); got != userURL {
		t.Fatalf("显式指名后应落到该线路 %s，得 %q", userURL, got)
	}
}

// accs2 重新读一次账号（分配后有写入，必须重取）。
func accs2(t *testing.T, store *Store) []*Account {
	t.Helper()
	list, err := store.ListAccounts()
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	return list
}

// TestRehomeSkipsUserLines ⚠️ 同一条边界也管**自动迁移**。
//
// 同样刻意让"健康的"只剩用户线路：此时正确行为是**保留原状**（号仍绑在坏线路上，
// 被选号排除 → 不服务也不裸奔），而不是"反正好线路上有位置就搬过去"。
// 若把用户线路当落点，这条会立刻失败。
func TestRehomeSkipsUserLines(t *testing.T) {
	store, a, _, _, _, memberID := ownerApp(t)

	const badURL, userURL = "http://10.0.0.2:8080", "http://203.0.113.7:3128"
	for _, p := range []*EgressProxy{
		{ID: "h-bad", Name: "平台坏线", URL: badURL, Enabled: true, FailStreak: egressFailThreshold},
		{ID: "h-user", Name: "用户线路", URL: userURL, Enabled: true, OwnerUserID: memberID},
	} {
		if err := store.UpsertEgressProxy(p); err != nil {
			t.Fatalf("UpsertEgressProxy: %v", err)
		}
	}
	if err := store.SetAccountEgress("ow-acct", badURL); err != nil {
		t.Fatalf("SetAccountEgress: %v", err)
	}
	// 打开自动迁移（默认是 strict：不静默换出口）。
	if err := store.SetKV(kvEgressOnLineDown, egressOnLineDownMigrate); err != nil {
		t.Fatalf("SetKV: %v", err)
	}

	a.rehomeAccountsFromDeadProxies()

	acc, err := store.GetAccount("ow-acct")
	if err != nil || acc == nil {
		t.Fatalf("GetAccount: %v %v", acc, err)
	}
	if got := strings.TrimSpace(acc.EgressProxy); got != badURL {
		t.Fatalf("无平台可用线路时应**保留原状**（%s），得 %q —— "+
			"自动迁移把号搬到用户线路上就是泄露", badURL, got)
	}

	// 补一条平台好线路 → 这时才应该迁（对照：证明上面不是因为"迁移整个没跑"）。
	const okURL = "http://10.0.0.1:8080"
	if err := store.UpsertEgressProxy(&EgressProxy{
		ID: "h-ok", Name: "平台好线", URL: okURL, Enabled: true,
	}); err != nil {
		t.Fatalf("UpsertEgressProxy(ok): %v", err)
	}
	a.rehomeAccountsFromDeadProxies()
	acc, _ = store.GetAccount("ow-acct")
	if got := strings.TrimSpace(acc.EgressProxy); got != okURL {
		t.Fatalf("有平台好线路时应迁到 %s，得 %q", okURL, got)
	}
}

// TestEgressProxyOwnerMigrationOnOldDB ⚠️ 这条是**用一次线上停机换来的**。
//
// 背景（2026-10-17，真实事故）：`migrateEgressProxy` 里 `ensureColumn` 的参数我写成了
// **整条 `ALTER TABLE …`**，而该 helper 自己会拼 `ALTER TABLE <table> ADD COLUMN ` ——
// 于是拼成了 `ADD COLUMN ALTER TABLE …` → 启动即 `near "ALTER": syntax error`，
// **服务起不来**（systemd 反复重启），停机约 1 分钟后回滚才恢复。
//
// 为什么当时没被本文件前面的用例拦住：那些用例跑在**新建库**上，而建表语句里
// 已经带了 `owner_user_id` → `ensureColumn` 直接"已存在"返回，**那段有 bug 的
// SQL 根本没被执行**。也就是说：**增量列迁移只在老库上跑，而测试只测了新库。**
//
// 这条用例刻意造一个**老库**（不带该列）再调真实迁移，把那条路径覆盖上。
func TestEgressProxyOwnerMigrationOnOldDB(t *testing.T) {
	store, _, _, _, _, _ := ownerApp(t)

	// 造老库：结构与 v0.31.22 一致（**没有** owner_user_id），并塞一条存量线路。
	for _, st := range []string{
		`DROP TABLE IF EXISTS egress_proxies`,
		`CREATE TABLE egress_proxies (
			id TEXT PRIMARY KEY, name TEXT NOT NULL DEFAULT '', url TEXT NOT NULL DEFAULT '',
			region TEXT NOT NULL DEFAULT '', note TEXT NOT NULL DEFAULT '',
			enabled INTEGER NOT NULL DEFAULT 1, last_ok INTEGER NOT NULL DEFAULT 0,
			last_ip TEXT NOT NULL DEFAULT '', last_check INTEGER NOT NULL DEFAULT 0,
			fail_streak INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL DEFAULT 0)`,
		`INSERT INTO egress_proxies (id, name, url, region, enabled)
			VALUES ('old-1', '老线路', 'http://10.9.9.9:8080', '旧机房', 1)`,
	} {
		if _, err := store.db.Exec(st); err != nil {
			t.Fatalf("造老库失败：%v", err)
		}
	}

	// 跑真实迁移：这条 SQL 写错就会在这里报错 —— 正是线上那次的形态。
	if err := store.migrateEgressProxy(); err != nil {
		t.Fatalf("老库迁移必须成功（线上就是因为这里报错导致服务起不来）：%v", err)
	}

	// 迁移后：列在、老数据在、归属默认 0（存量线路都是平台手工登记）。
	p, err := store.GetEgressProxy("old-1")
	if err != nil || p == nil {
		t.Fatalf("迁移后应仍能读到老线路：%v %v", p, err)
	}
	if p.OwnerUserID != 0 {
		t.Fatalf("存量线路的归属应默认 0（平台），得 %d", p.OwnerUserID)
	}
	if p.Name != "老线路" || p.Region != "旧机房" {
		t.Fatalf("迁移不得改动存量字段，得 name=%q region=%q", p.Name, p.Region)
	}

	// 再跑一次必须幂等（服务每次启动都会跑迁移）。
	if err := store.migrateEgressProxy(); err != nil {
		t.Fatalf("迁移必须幂等（每次启动都会跑）：%v", err)
	}
}

// TestEgressProxiesShape 线路池下发的键名必须是 snake_case。
//
// 这条同样是**用一次"整块页面全空"换来的**（v0.31.23）：`EgressProxy` 此前没有
// json tag，Go 用**字段名**（`Name` / `URL` / `LastIP`）序列化，而前端读的是
// `p.name` / `p.url` —— 于是 `(p.name || p.url).charAt(0)` 抛
// 「Cannot read properties of undefined」，**线路卡一张都渲染不出来**；
// 而异常被 `loadEgress` 的 try/catch 吞成一句 toast，页面上只剩空白，
// 既看不出是前端 bug、也看不出是真没数据。
//
// 双向断言（本仓纪律）：前端要读的 snake_case 键**必须存在**，
// Go 字段名键**必须不存在**去。
func TestEgressProxiesShape(t *testing.T) {
	store, _, mux, adminCookie, _, _ := ownerApp(t)
	if err := store.UpsertEgressProxy(&EgressProxy{
		ID: "sh-1", Name: "线路甲", URL: "http://10.1.1.1:8080",
		Region: "某地", Note: "备注", Enabled: true, LastIP: "9.9.9.9", LastOK: true,
	}); err != nil {
		t.Fatalf("UpsertEgressProxy: %v", err)
	}

	code, raw := eaReq(t, mux, "GET", "/api/admin/egress/proxies", "", adminCookie, "")
	if code != http.StatusOK {
		t.Fatalf("线路池接口应 200，得 %d %s", code, raw)
	}
	for _, k := range []string{
		`"name"`, `"url"`, `"region"`, `"note"`, `"enabled"`,
		`"last_ok"`, `"last_ip"`, `"last_check"`, `"fail_streak"`,
		`"created_at"`, `"updated_at"`, `"owner_user_id"`,
	} {
		if !strings.Contains(raw, k) {
			t.Errorf("线路池响应必须含 %s（前端按这个键渲染）：%s", k, raw)
		}
	}
	for _, bad := range []string{
		`"Name"`, `"URL"`, `"Region"`, `"LastIP"`, `"LastOK"`,
		`"LastCheck"`, `"FailStreak"`, `"OwnerUserID"`, `"CreatedAt"`,
	} {
		if strings.Contains(raw, bad) {
			t.Errorf("线路池响应不该出现 Go 字段名键 %s（说明漏了 json tag）：%s", bad, raw)
		}
	}

	// 再按**结构**取一次：把前端真正会读的字段逐个点出来，
	// 免得出现"断言了一套键、前端读的是另一套"这种两边都绿的错位。
	var env struct {
		Proxies []map[string]any `json:"proxies"`
	}
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("解析线路池响应失败：%v", err)
	}
	if len(env.Proxies) != 1 {
		t.Fatalf("应回 1 条线路，得 %d", len(env.Proxies))
	}
	for _, k := range []string{"id", "name", "url", "region", "note", "enabled", "last_ip", "owner_user_id"} {
		if _, ok := env.Proxies[0][k]; !ok {
			t.Errorf("前端会读 proxies[].%s，但响应里没有这个键", k)
		}
	}
}
