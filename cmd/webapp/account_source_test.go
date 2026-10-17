// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import "testing"

// 本文件钉住 v0.31.37 的两条口径：
//  1. `accounts.source` / `accounts.refresh_lost_at` 是**增量列**，老库必须能加上；
//  2. 来源与"仅剩 access"的写读语义 —— 它们决定了「扫码 vs 导入」的失效率
//     能不能算对（这是本轮用户提的判断："掉线大多数都是扫码登录"）。
//
// 为什么这类测试必须造老库：新建库的 CREATE TABLE 已含该列，`ensureColumn`
// 走的是"已存在"分支，那段迁移 SQL 根本不执行 —— 全绿证明不了它能用
// （2026-10-17 就因此在生产上让服务起不来一次）。

// TestSourceColumnsMigrationOnOldDB 老库（无 source / refresh_lost_at）跑真实迁移。
func TestSourceColumnsMigrationOnOldDB(t *testing.T) {
	store, _, _ := newTestApp(t)
	defer store.Close()

	for _, st := range []string{
		`DROP TABLE IF EXISTS accounts`,
		`CREATE TABLE accounts (
			uid TEXT PRIMARY KEY, nickname TEXT NOT NULL DEFAULT '', realm TEXT NOT NULL DEFAULT 'cn',
			enterprise_id TEXT NOT NULL DEFAULT '', domain TEXT NOT NULL DEFAULT '',
			access_token TEXT NOT NULL DEFAULT '', refresh_token TEXT NOT NULL DEFAULT '',
			expires_at INTEGER NOT NULL DEFAULT 0, device_token TEXT NOT NULL DEFAULT '',
			device_token_at INTEGER NOT NULL DEFAULT 0, disabled INTEGER NOT NULL DEFAULT 0,
			cooldown_until INTEGER NOT NULL DEFAULT 0, fail_count INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL DEFAULT 0)`,
		`INSERT INTO accounts (uid, nickname, realm, access_token) VALUES ('old-1', '老号', 'cn', 'at-old')`,
	} {
		if _, err := store.db.Exec(st); err != nil {
			t.Fatalf("造老库失败：%v", err)
		}
	}
	if err := store.migrate(); err != nil {
		t.Fatalf("老库迁移必须成功（写错就是服务起不来）：%v", err)
	}

	var src string
	var lost int64
	if err := store.db.QueryRow(`SELECT source, refresh_lost_at FROM accounts WHERE uid='old-1'`).
		Scan(&src, &lost); err != nil {
		t.Fatalf("迁移后应能读到 source / refresh_lost_at：%v", err)
	}
	if src != "" {
		t.Fatalf("存量行来源应为空（历史/未知），得 %q", src)
	}
	if lost != 0 {
		t.Fatalf("存量行 refresh_lost_at 应为 0，得 %d", lost)
	}
	// 空来源不该进 SourceMap（否则"历史"这一格会凭空多出一堆号）。
	if _, ok := store.SourceMap()["old-1"]; ok {
		t.Fatalf("空来源不该出现在 SourceMap 里")
	}
	if err := store.migrate(); err != nil {
		t.Fatalf("迁移必须幂等：%v", err)
	}
}

// TestAccountSourceWriteOnce 来源只允许"空 → 值"，不允许互改。
//
// 为什么要卡这么死：扫码号被重新导入（很常见，掉线后就是这么救的）时，
// 若允许互改，它的来源会变成 import —— 于是"扫码号失效更多"这个结论会被
// 后续的抢救动作**自动擦掉**，越修越看不见问题。
func TestAccountSourceWriteOnce(t *testing.T) {
	store, _, _ := newTestApp(t)
	defer store.Close()

	if err := store.UpsertAccount(&Account{UID: "u1", Nickname: "甲", Realm: "cn", AccessToken: "at"}); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	if err := store.SetAccountSource("u1", sourceQR); err != nil {
		t.Fatalf("写来源：%v", err)
	}
	if got := store.SourceMap()["u1"]; got != sourceQR {
		t.Fatalf("来源应为 qr，得 %q", got)
	}
	// 再次写（导入路径）不该改写。
	if err := store.SetAccountSource("u1", sourceImport); err != nil {
		t.Fatalf("二次写来源：%v", err)
	}
	if got := store.SourceMap()["u1"]; got != sourceQR {
		t.Fatalf("来源应保持 qr（只允许空→值），得 %q", got)
	}
	// 非法值一律不写。
	if err := store.SetAccountSource("u1", "QR "); err != nil {
		t.Fatalf("非法值不该报错：%v", err)
	}
	if got := store.SourceMap()["u1"]; got != sourceQR {
		t.Fatalf("非法值不该覆盖已有来源，得 %q", got)
	}
}

// TestRefreshLostLifecycle 三态互斥：正常 → 仅剩 access → 真死 → 重登清零。
//
// 这是 v0.31.37 最核心的一条语义：**"能不能服务"与"能不能续期"是两件事**。
// 混成一个状态就会要么白扔一个月额度（把能用当死号），要么让死号继续被选中。
func TestRefreshLostLifecycle(t *testing.T) {
	store, _, _ := newTestApp(t)
	defer store.Close()

	if err := store.UpsertAccount(&Account{UID: "u1", Nickname: "甲", Realm: "cn",
		AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	// 1) refresh 丢了但 access 还能用 → 只有 refresh_lost。
	if err := store.MarkRefreshLost("u1"); err != nil {
		t.Fatalf("MarkRefreshLost: %v", err)
	}
	if _, ok := store.RefreshLostMap()["u1"]; !ok {
		t.Fatalf("应记入 refresh_lost")
	}
	if _, ok := store.TokenDeadMap()["u1"]; ok {
		t.Fatalf("仅剩 access 时不该标 token_dead（那会把还能用的号当死号）")
	}

	// 2) access 也过期 → 真死，且 lost 位必须被清（同一个号不该挂两个异常徽标）。
	if err := store.MarkTokenDead("u1"); err != nil {
		t.Fatalf("MarkTokenDead: %v", err)
	}
	if _, ok := store.TokenDeadMap()["u1"]; !ok {
		t.Fatalf("应记入 token_dead")
	}
	if _, ok := store.RefreshLostMap()["u1"]; ok {
		t.Fatalf("真死时 refresh_lost 应被清掉")
	}

	// 3) 重新登录/导入 → 两位都清。
	if err := store.ClearTokenDead("u1"); err != nil {
		t.Fatalf("ClearTokenDead: %v", err)
	}
	if len(store.TokenDeadMap()) != 0 || len(store.RefreshLostMap()) != 0 {
		t.Fatalf("重登后两个标记都该消失：dead=%v lost=%v", store.TokenDeadMap(), store.RefreshLostMap())
	}

	// 4) UpdateTokens（刷新成功写回）也清两位。
	_ = store.MarkRefreshLost("u1")
	if err := store.UpdateTokens("u1", "at2", "rt2", 9999999999); err != nil {
		t.Fatalf("UpdateTokens: %v", err)
	}
	if len(store.RefreshLostMap()) != 0 {
		t.Fatalf("刷新成功后 refresh_lost 应被清掉")
	}
}

// TestBuildSourceStats 钉住统计口径（三条，每条都能把结论做假）。
func TestBuildSourceStats(t *testing.T) {
	accs := []*Account{
		{UID: "q1", OwnerUserID: 0},                  // 平台号，恒在池
		{UID: "q2", OwnerUserID: 7, PoolState: "in"}, // 代挂已入池
		{UID: "q3", OwnerUserID: 7, PoolState: ""},   // 代挂未入池
		{UID: "i1", OwnerUserID: 0},
		{UID: "old1", OwnerUserID: 0},
		{UID: "old2", OwnerUserID: 0},
	}
	dead := map[string]int64{"q3": 1, "old1": 1}
	lost := map[string]int64{"i1": 1, "old2": 1, "q3": 1} // q3 同时有两位 → 只算 dead
	src := map[string]string{"q1": "qr", "q2": "qr", "q3": "qr", "i1": "import"}

	rows := buildSourceStats(accs, dead, lost, src)
	if len(rows) != 3 {
		t.Fatalf("应固定三行，得 %d", len(rows))
	}
	bySrc := map[string]sourceStatRow{}
	for _, r := range rows {
		bySrc[r.Source] = r
	}
	qr := bySrc["qr"]
	if qr.Total != 3 || qr.InPool != 2 || qr.Dead != 1 || qr.Lost != 0 {
		t.Fatalf("扫码口径错：%+v（分母应为全部 3 个；q3 同时死+丢只算 dead）", qr)
	}
	if got := qr.DeadRate; got < 0.33 || got > 0.34 {
		t.Fatalf("扫码失效率应约 0.333，得 %v", got)
	}
	imp := bySrc["import"]
	// import 只有一个号，且它是 refresh_lost 不是 dead —— 失效率必须是 0。
	// 若把 lost 并进 dead，这里会变成 100%，正好把"扫码更容易掉线"的结论做反。
	if imp.Dead != 0 || imp.Lost != 1 || imp.DeadRate != 0 {
		t.Fatalf("仅剩 access 不该计入失效：%+v", imp)
	}
	unk := bySrc[""]
	if unk.Total != 2 || unk.Dead != 1 || unk.Lost != 1 {
		t.Fatalf("历史行应单列且不丢弃：%+v", unk)
	}
	if unk.Label == "" || qr.Label == "" || imp.Label == "" {
		t.Fatalf("每行都必须有中文标签（前端不再自行映射）")
	}
}

// TestNormSource normSource 必须是封闭取值：拼写变体不得变成新类别。
func TestNormSource(t *testing.T) {
	cases := map[string]string{
		"qr": "qr", " QR ": "qr", "import": "import", "Import": "import",
		"": "", "manual": "", "wework": "",
	}
	for in, want := range cases {
		if got := normSource(in); got != want {
			t.Fatalf("normSource(%q)=%q，期望 %q", in, got, want)
		}
	}
}
