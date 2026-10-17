// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import "testing"

// 本文件钉两件事（v0.31.30）：
//  1. `accounts.token_dead_at` 这条**增量列**在老库上真的能加上
//     （2026-10-17 就因为在 ensureColumn 里传了整条 ALTER，让服务起不来过一次）；
//  2. 失效标记的写/清/查语义 —— 它决定卡片红标什么时候出现、什么时候消失。

// TestTokenDeadColumnMigrationOnOldDB 老库（没有 token_dead_at）跑真实迁移：
// 列必须被加上、存量行必须还在、默认值必须是 0（= 正常）。
//
// 为什么必须造老库：新建库的 CREATE TABLE 里已经带了这一列，
// `ensureColumn` 会直接"已存在"返回 —— 那段迁移 SQL **根本不会执行**，
// 测试全绿也证明不了它可用（这正是上次停机的形态）。
func TestTokenDeadColumnMigrationOnOldDB(t *testing.T) {
	store, _, _ := newTestApp(t)
	defer store.Close()

	// 造老库：accounts 结构是 v0.31.29 之前的样子（无 token_dead_at）。
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

	// 列在、老数据在。
	var at int64
	if err := store.db.QueryRow(`SELECT token_dead_at FROM accounts WHERE uid='old-1'`).Scan(&at); err != nil {
		t.Fatalf("迁移后应能读到 token_dead_at：%v", err)
	}
	if at != 0 {
		t.Fatalf("存量账号的 token_dead_at 应为 0（正常），得 %d", at)
	}
	acc, err := store.GetAccount("old-1")
	if err != nil || acc == nil || acc.AccessToken != "at-old" {
		t.Fatalf("迁移不该动老数据：%+v err=%v", acc, err)
	}

	// 幂等：再跑一次不报错。
	if err := store.migrate(); err != nil {
		t.Fatalf("迁移必须幂等：%v", err)
	}
}

// TestTokenDeadMarkAndClear 标记 → 查得到；重新导入（Clear）→ 立刻消失。
//
// 「清」这条尤其重要：用户按提示重新导入后卡片必须马上不红。
// 不清的话，红标会永久留着 —— 比不标红更让人困惑（用户会以为导入没生效）。
func TestTokenDeadMarkAndClear(t *testing.T) {
	store, _, _ := newTestApp(t)
	defer store.Close()

	if err := store.UpsertAccount(&Account{UID: "u1", Nickname: "甲", Realm: "cn", AccessToken: "at"}); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	if got := store.TokenDeadMap(); len(got) != 0 {
		t.Fatalf("初始不该有失效标记，得 %+v", got)
	}

	if err := store.MarkTokenDead("u1"); err != nil {
		t.Fatalf("MarkTokenDead: %v", err)
	}
	m := store.TokenDeadMap()
	if _, ok := m["u1"]; !ok {
		t.Fatalf("标记后应出现在 TokenDeadMap，得 %+v", m)
	}

	if err := store.ClearTokenDead("u1"); err != nil {
		t.Fatalf("ClearTokenDead: %v", err)
	}
	if got := store.TokenDeadMap(); len(got) != 0 {
		t.Fatalf("清除后不该再有标记，得 %+v", got)
	}

	// UpdateTokens（刷新成功写回）也必须顺手清 —— 这是"会话真的活了"的证据。
	_ = store.MarkTokenDead("u1")
	if err := store.UpdateTokens("u1", "at-new", "rt-new", 1999999999); err != nil {
		t.Fatalf("UpdateTokens: %v", err)
	}
	if got := store.TokenDeadMap(); len(got) != 0 {
		t.Fatalf("刷新成功写回后应清除失效标记，得 %+v", got)
	}
}
