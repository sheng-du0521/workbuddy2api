// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/upstream"
)

// 本文件钉住「会话冲突自动降级」（v0.31.40）的三条口径。
//
// 背景：上游 refresh 是轮换制，同一个号在号主本机客户端上也登着时，两边共用一条链 ——
// 谁先刷谁把对方打掉。**本机那一半我们控制不了**，代码能消除的只有我方这一半：
// 一旦确认冲突就停止主动刷新（只消费 access）。
//
// 三条都必须钉住，否则"自动降级"会变成"自动把号弄死"：
//  1. 降级只发生在**证据充分**时（refresh 已废 + access 仍有效）；
//  2. 降级后**不再主动刷新**（保活/签到前补刷新都跳过），但**照常服务**；
//  3. 必须有**反向开关**（否则自动降级是单向棘轮，只能靠人工重登续命）。

// TestNormRefreshMode 归一：只认 passive，其余一律 auto。
//
// 默认 auto 是刻意的：passive = 到期即断、必须人工重登。把它做成默认等于让所有号
// 30 天后集体失效 —— 那是把冲突换成更大的问题。
func TestNormRefreshMode(t *testing.T) {
	cases := map[string]string{
		"passive": refreshModePassive, " PASSIVE ": refreshModePassive,
		"auto": refreshModeAuto, "": refreshModeAuto, "whatever": refreshModeAuto,
	}
	for in, want := range cases {
		if got := normRefreshMode(in); got != want {
			t.Fatalf("normRefreshMode(%q)=%q want %q", in, got, want)
		}
	}
}

// TestRefreshModeColumnMigrationOnOldDB 老库（无 refresh_mode 列）跑真实迁移。
func TestRefreshModeColumnMigrationOnOldDB(t *testing.T) {
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
		t.Fatalf("老库迁移必须成功：%v", err)
	}
	var mode string
	if err := store.db.QueryRow(`SELECT refresh_mode FROM accounts WHERE uid='old-1'`).Scan(&mode); err != nil {
		t.Fatalf("迁移后应能读到 refresh_mode：%v", err)
	}
	if mode != "" {
		t.Fatalf("存量行 refresh_mode 应为空（= auto），得 %q", mode)
	}
	if store.RefreshModeMap()["old-1"] {
		t.Fatal("空值不该被当成 passive（否则所有老号都会停止续期）")
	}
}

// TestRefreshModeSwitchAndPoolMirror 写库 + 池内镜像必须同步。
//
// 只写库会造出"界面显示已降级、保活照常刷"的半个动作（本仓反复踩过的那类坑）。
func TestRefreshModeSwitchAndPoolMirror(t *testing.T) {
	store, px, a := newTestApp(t, &Account{
		UID: "u1", Nickname: "甲", Realm: "cn", AccessToken: "at", RefreshToken: "rt",
		ExpiresAt: time.Now().Add(20 * 24 * time.Hour).Unix(),
	})
	defer store.Close()
	if err := px.SyncAccounts(); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}
	if px.pool.PassiveRefresh("u1") {
		t.Fatal("默认应为 auto（主动续期）")
	}

	if err := store.SetRefreshMode("u1", "passive"); err != nil {
		t.Fatalf("SetRefreshMode: %v", err)
	}
	// DB 是源：SyncAccounts 之后池内必须跟着变（这就是"三处纪律一致"的落地方式）。
	if err := px.SyncAccounts(); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}
	if !px.pool.PassiveRefresh("u1") {
		t.Fatal("切 passive 后池内标记必须为 true（否则保活会照常刷）")
	}
	if st, ok := px.pool.Status("u1"); !ok || !st.PassiveRefresh {
		t.Fatalf("Status 必须透出 passive_refresh：%+v", st)
	}

	// 反向：切回 auto。
	if err := store.SetRefreshMode("u1", "auto"); err != nil {
		t.Fatalf("SetRefreshMode: %v", err)
	}
	_ = px.SyncAccounts()
	if px.pool.PassiveRefresh("u1") {
		t.Fatal("切回 auto 后池内标记必须清掉（否则这个号永远刷不动）")
	}
	_ = a
}

// TestConflictDegradeSetsPassive 冲突证据（refresh 已废 + access 仍有效）→ 自动降级。
//
// 这条把"冲突"与"降级"焊在一起：否则运维只能靠人肉判断"这个号是不是在别处也在用"。
func TestConflictDegradeSetsPassive(t *testing.T) {
	store, px, a := newTestApp(t, &Account{
		UID: "u1", Nickname: "甲", Realm: "cn",
		AccessToken: "at", RefreshToken: "rt",
		ExpiresAt: time.Now().Add(20 * 24 * time.Hour).Unix(),
	})
	defer store.Close()
	if err := px.SyncAccounts(); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}
	if px.pool.PassiveRefresh("u1") {
		t.Fatal("前置：默认不该是 passive")
	}

	a.proxy.handleSessionDead("u1")

	if _, ok := store.RefreshLostMap()["u1"]; !ok {
		t.Fatal("应置 refresh_lost（access 仍有效）")
	}
	if _, ok := store.TokenDeadMap()["u1"]; ok {
		t.Fatal("access 仍有效时不该标 token_dead")
	}
	if !store.RefreshModeMap()["u1"] || !px.pool.PassiveRefresh("u1") {
		t.Fatal("检测到冲突后必须自动降级为 passive（库 + 池都要）")
	}
	// 降级不是停用：账号必须还在池里、还能被选中。
	if st, ok := px.pool.Status("u1"); !ok || st.Disabled || st.ManualDisabled {
		t.Fatalf("降级不是停用，账号状态不该被摘除：%+v", st)
	}
}

// TestConflictDegradeNotWhenAccessExpired access 也过期时走真死路径，**不降级**。
//
// 为什么不降级：那个号已经不能服务了，passive 毫无意义；更重要的是别把两种状态
// 混在一起 —— 以后看到 passive 就分不清"还能用"还是"已经死了"。
func TestConflictDegradeNotWhenAccessExpired(t *testing.T) {
	store, px, a := newTestApp(t, &Account{
		UID: "u1", Nickname: "甲", Realm: "cn",
		AccessToken: "at", RefreshToken: "rt",
		ExpiresAt: time.Now().Add(-time.Hour).Unix(),
	})
	defer store.Close()
	_ = px.SyncAccounts()

	a.proxy.handleSessionDead("u1")

	if _, ok := store.TokenDeadMap()["u1"]; !ok {
		t.Fatal("access 已过期应走真死路径（token_dead）")
	}
	if store.RefreshModeMap()["u1"] {
		t.Fatal("真死的号不该被标成 passive（两种状态必须能区分）")
	}
}

// TestOAuthDeviceIdentityConsistency 登录身份：有 uid 提示时与运行期同源，无提示时随机。
//
// 为什么钉住：登录链路此前**一个设备头都不带**，而运行期每条请求都带 —— 上游看到的
// 是"无身份客户端创建的会话被设备 X 使用"。这条用例保证"创建者"与"使用者"同源。
func TestOAuthDeviceIdentityConsistency(t *testing.T) {
	// 无提示：随机，且三次互不相同（不能退化成常量）。
	d1, m1, s1 := oauthDeviceIdentity(nil)
	d2, _, _ := oauthDeviceIdentity(nil)
	if d1 == "" || m1 == "" || s1 == "" {
		t.Fatal("无提示时应生成非空身份")
	}
	if d1 == d2 {
		t.Fatal("无提示时每次会话必须是新身份（复用会被当成同一设备）")
	}
	// 有提示：与该账号的运行期派生身份完全一致。
	uid := "u-fixed"
	d, m, ss := oauthDeviceIdentity([]string{uid})
	if strings.TrimSpace(d) == "" {
		t.Fatal("有提示时应给出该账号的设备凭证")
	}
	if m != upstream.AccountMachineID(uid) || ss != upstream.AccountSessionID(uid) {
		t.Fatalf("有提示时必须与运行期派生身份同源：m=%q session=%q", m, ss)
	}
}
