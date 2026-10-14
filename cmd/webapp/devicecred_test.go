// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/upstream"
)

// ===========================================================================
// v0.25.1 设备风控凭证（X-Device-Token）
//
// 这批用例守的是「凭证到底从哪儿来」这条链 —— 它此前整条断着：
// 导入时字段被丢弃、webapp 也没装全局兜底，于是该头从来没被注入过。
// 而"从来没注入"是个**静默失败**：没有任何报错，只有审计才看得出来。
// ===========================================================================

// TestDeviceTokenStateThreeStates 三态必须分明（v0.26.3 起在派生兜底关闭时即此三态）。
//
// 为什么不是 bool：审计结论天差地别 —— 「全池都没带」（可聚类）与
// 「全池共用一个」（也可聚类，形态不同）需要的处置完全不同。
func TestDeviceTokenStateThreeStates(t *testing.T) {
	store, px, a := newTestApp(t, &Account{UID: "d1", AccessToken: "tok"})
	defer store.Close()
	defer px.Close()
	t.Setenv(envDeviceToken, "")
	t.Setenv(envDeviceTokenFile, "")

	// ① 都没有 → none
	if got := a.deviceTokenState(&Account{UID: "x"}); got != "none" {
		t.Fatalf("无任何来源应为 none，得 %q", got)
	}
	// ② 全局 kv 有值 → global
	if err := store.SetKV(kvDeviceToken, "global-tok"); err != nil {
		t.Fatal(err)
	}
	if got := a.deviceTokenState(&Account{UID: "x"}); got != "global" {
		t.Fatalf("全局兜底应为 global，得 %q", got)
	}
	// ③ 账号自带优先于全局 → account
	if got := a.deviceTokenState(&Account{UID: "x", DeviceToken: "own-tok"}); got != "account" {
		t.Fatalf("账号自带应优先为 account，得 %q", got)
	}
}

// TestDeviceTokenEnvBeatsKV env 优先级高于 kv（容器化部署把凭证放环境变量）。
func TestDeviceTokenEnvBeatsKV(t *testing.T) {
	store, px, a := newTestApp(t, &Account{UID: "d2", AccessToken: "tok"})
	defer store.Close()
	defer px.Close()

	if err := store.SetKV(kvDeviceToken, "kv-tok"); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envDeviceToken, "env-tok")
	if got := store.globalDeviceToken(); got != "env-tok" {
		t.Fatalf("env 应覆盖 kv，得 %q", got)
	}
	if got := a.deviceTokenState(&Account{UID: "x"}); got != "global" {
		t.Fatalf("应为 global，得 %q", got)
	}
}

// TestDeviceTokenFileHasContent 文件必须**真的非空**才算提供了凭证。
//
// 这条的意义：把"配了路径但文件是空的/路径写错"与"确实配好了"区分开。
// 混为一谈的话，运维会以为凭证在生效，而实际上该头从未被注入。
func TestDeviceTokenFileHasContent(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ok := filepath.Join(dir, "ok")
	if err := os.WriteFile(ok, []byte("tok-123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		path string
		want bool
	}{
		{"", false},
		{filepath.Join(dir, "nope"), false}, // 不存在
		{dir, false},                        // 是目录
		{empty, false},                      // 只有空白
		{ok, true},
	}
	for _, c := range cases {
		if got := deviceTokenFileHasContent(c.path); got != c.want {
			t.Errorf("deviceTokenFileHasContent(%q)=%v want %v", c.path, got, c.want)
		}
	}
}

// TestDeviceTokenFileCountsAsGlobal 只配文件（没配全局值）也应算 global。
func TestDeviceTokenFileCountsAsGlobal(t *testing.T) {
	store, px, a := newTestApp(t, &Account{UID: "d3", AccessToken: "tok"})
	defer store.Close()
	defer px.Close()
	t.Setenv(envDeviceToken, "")
	t.Setenv(envDeviceTokenFile, "")

	dir := t.TempDir()
	f := filepath.Join(dir, "dt")
	if err := os.WriteFile(f, []byte("file-tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.SetKV(kvDeviceTokenFile, f); err != nil {
		t.Fatal(err)
	}
	if got := a.deviceTokenState(&Account{UID: "x"}); got != "global" {
		t.Fatalf("只配文件应为 global，得 %q", got)
	}
	// 文件被清空后又应回到 none（凭证是"内容"而不是"路径"）。
	if err := os.WriteFile(f, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := a.deviceTokenState(&Account{UID: "x"}); got != "none" {
		t.Fatalf("文件为空应回到 none，得 %q", got)
	}
}

// TestInstallDeviceToken 装配真的把值装到出站客户端上。
//
// 这是"全池 0/27"的**第二层根因**：cmd/server 一直装、webapp 漏装。
// 只测 deviceTokenState（展示）不测这里，漏装这个缺陷还会复现。
func TestInstallDeviceToken(t *testing.T) {
	store, px, _ := newTestApp(t, &Account{UID: "d4", AccessToken: "tok"})
	defer store.Close()
	defer px.Close()

	if err := store.SetKV(kvDeviceToken, "installed-tok"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetKV(kvDeviceTokenFile, "/tmp/whatever"); err != nil {
		t.Fatal(err)
	}
	uc := upstream.New()
	store.installDeviceToken(uc)
	if uc.DeviceToken != "installed-tok" {
		t.Fatalf("全局凭证未装配，得 %q", uc.DeviceToken)
	}
	if uc.DeviceTokenFile != "/tmp/whatever" {
		t.Fatalf("凭证文件路径未装配，得 %q", uc.DeviceTokenFile)
	}
	// nil 安全性：装配点在 NewProxy，不该因为 store/client 为 nil 就 panic。
	store.installDeviceToken(nil)
	var nilStore *Store
	nilStore.installDeviceToken(uc)
}

// TestDesktopImportKeepsDeviceToken **导入不能再丢 device_token**（第一层根因）。
//
// 顶层扁平形与 auth_raw 嵌套形都要能取到 —— 桌面端不同版本写法不一，
// 只认一种就会让另一半用户的凭证照样丢。
func TestDesktopImportKeepsDeviceToken(t *testing.T) {
	// ① 顶层 device_token
	d1 := desktopAccount{UID: "u1", AccessToken: "at", DeviceToken: "tok-top"}
	if got := d1.toAccount().DeviceToken; got != "tok-top" {
		t.Fatalf("顶层 device_token 应被保留，得 %q", got)
	}
	// ② auth_raw 内嵌 deviceToken
	d2 := desktopAccount{
		UID: "u2", AccessToken: "at",
		AuthRaw: []byte(`{"accessToken":"at2","deviceToken":"tok-nested"}`),
	}
	a2 := d2.toAccount()
	if a2.DeviceToken != "tok-nested" {
		t.Fatalf("auth_raw 内嵌 deviceToken 应被保留，得 %q", a2.DeviceToken)
	}
	if a2.AccessToken != "at2" {
		t.Fatalf("auth_raw 的 accessToken 仍应生效，得 %q", a2.AccessToken)
	}
	// ③ 顶层优先于嵌套
	d3 := desktopAccount{
		UID: "u3", AccessToken: "at", DeviceToken: "tok-top",
		AuthRaw: []byte(`{"deviceToken":"tok-nested"}`),
	}
	if got := d3.toAccount().DeviceToken; got != "tok-top" {
		t.Fatalf("顶层应优先，得 %q", got)
	}
	// ④ 都没有 → 空（不能凭空造值）
	if got := (desktopAccount{UID: "u4", AccessToken: "at"}).toAccount().DeviceToken; got != "" {
		t.Fatalf("无来源应为空，得 %q", got)
	}
}

// TestDeviceTokenWiredIntoAuthAuth 账号的 device_token 必须传到 auth.Auth。
//
// 这条补齐链路最后一环：库里存了、但 authFromAccount 不传的话，
// 出站照样不注入 —— 三个环节缺一不可（存 · 传 · 注入）。
func TestDeviceTokenWiredIntoAuthAuth(t *testing.T) {
	au := authFromAccount(&Account{UID: "u9", AccessToken: "at", DeviceToken: "dt-9"})
	if au == nil {
		t.Fatal("authFromAccount 不应返回 nil")
	}
	if au.DeviceToken != "dt-9" {
		t.Fatalf("device_token 未传到 auth.Auth，得 %q", au.DeviceToken)
	}
}

// ===========================================================================
// v0.26.3 派生兜底 + 陈旧检测（五态）
//
// 三态扩成五态的原因：审计结论比"有没有带"更细 ——
//   derived = 各号一个合成值（不可按单值聚类，但值本身非上游签发）
//   stale   = 真值但很可能已随客户端轮换失效（真值 vs 现在值，取舍不同）
// 这两种都不是"none"，也不该被并进 "account" 里悄悄混过去。
// ===========================================================================

// TestDeviceTokenDerivedStateFallbackOffIsNone 兜底开关**默认关闭** ——
// 三个空档全空时仍是 none（既有行为零变化）。
func TestDeviceTokenDerivedStateFallbackOffIsNone(t *testing.T) {
	store, px, a := newTestApp(t)
	defer store.Close()
	defer px.Close()
	t.Setenv(envDeviceToken, "")
	t.Setenv(envDeviceTokenFile, "")

	if store.deviceTokenFallbackOn() {
		t.Fatal("派生兜底默认必须是关闭的（合成值不该默认发给上游）")
	}
	if got := a.deviceTokenState(&Account{UID: "x"}); got != "none" {
		t.Fatalf("默认应为 none，得 %q", got)
	}
}

// TestDeviceTokenDerivedStateWhenEnabled 开关打开且有 uid → derived。
func TestDeviceTokenDerivedStateWhenEnabled(t *testing.T) {
	store, px, a := newTestApp(t)
	defer store.Close()
	defer px.Close()
	t.Setenv(envDeviceToken, "")
	t.Setenv(envDeviceTokenFile, "")

	if err := store.SetKV(kvDeviceTokenFallback, "derive"); err != nil {
		t.Fatal(err)
	}
	if !store.deviceTokenFallbackOn() {
		t.Fatal("开关未生效")
	}
	if got := a.deviceTokenState(&Account{UID: "x"}); got != "derived" {
		t.Fatalf("应为 derived，得 %q", got)
	}
	// 真值仍然优先于派生值：账号 / 全局 / 文件任一存在就不落到 derived。
	if got := a.deviceTokenState(&Account{UID: "x", DeviceToken: "real"}); got != "account" {
		t.Fatalf("账号真值应优先，得 %q", got)
	}
	if err := store.SetKV(kvDeviceToken, "g"); err != nil {
		t.Fatal(err)
	}
	if got := a.deviceTokenState(&Account{UID: "x"}); got != "global" {
		t.Fatalf("全局真值应优先于派生，得 %q", got)
	}
}

// TestInstallDeviceTokenWiresFallback 开关必须真装到出站客户端上。
//
// 只改 kv 不装配 = 界面显示 derived、实际出站仍不带头 —— 又一个静默失败。
func TestInstallDeviceTokenWiresFallback(t *testing.T) {
	store, _, _ := newTestApp(t)
	defer store.Close()

	uc := upstream.New()
	if err := store.SetKV(kvDeviceTokenFallback, "derive"); err != nil {
		t.Fatal(err)
	}
	store.installDeviceToken(uc)
	if !uc.DeviceTokenFallback {
		t.Fatal("派生兜底开关未装配到 upstream.Client")
	}
	if err := store.SetKV(kvDeviceTokenFallback, "off"); err != nil {
		t.Fatal(err)
	}
	store.installDeviceToken(uc)
	if uc.DeviceTokenFallback {
		t.Fatal("关闭后仍残留开启状态")
	}
}

// TestDeviceTokenStaleDetection 陈旧阈值：超过阈值才算陈旧，且未刷新时刻视为未知（不报警）。
func TestDeviceTokenStaleDetection(t *testing.T) {
	store, px, a := newTestApp(t)
	defer store.Close()
	defer px.Close()
	t.Setenv(envDeviceToken, "")
	t.Setenv(envDeviceTokenFile, "")

	now := time.Now().Unix()
	cases := []struct {
		name string
		at   int64
		want bool
	}{
		{"刚写入", now, false},
		{"29 天前（阈值内）", now - 29*86400, false},
		{"31 天前（超阈值）", now - 31*86400, true},
		{"时刻未知（未记录）", 0, false}, // 老库遗留：不报警（避免升级后全池告警）
	}
	for _, c := range cases {
		if got := a.deviceTokenStale(&Account{UID: "x", DeviceToken: "t", DeviceTokenAt: c.at}); got != c.want {
			t.Errorf("%s：stale=%v want %v", c.name, got, c.want)
		}
	}
	// 阈值为 0 = 关闭检测。
	if err := store.SetKV(kvDeviceTokenStaleDays, "0"); err != nil {
		t.Fatal(err)
	}
	if a.deviceTokenStale(&Account{UID: "x", DeviceToken: "t", DeviceTokenAt: now - 400*86400}) {
		t.Error("阈值为 0 时应关闭检测")
	}
}

// TestDeviceTokenStaleDaysDefaultAndBounds 阈值的默认值与越界回落。
func TestDeviceTokenStaleDaysDefaultAndBounds(t *testing.T) {
	store, _, a := newTestApp(t)
	defer store.Close()

	if got := a.deviceTokenStaleDays(); got != defaultDeviceTokenStaleDays {
		t.Errorf("默认阈值 = %d, want %d", got, defaultDeviceTokenStaleDays)
	}
	for _, tc := range []struct {
		set  string
		want int
	}{
		{"7", 7},
		{"abc", defaultDeviceTokenStaleDays}, // 非法 → 默认
		{"-1", defaultDeviceTokenStaleDays},  // 负数 → 默认
		{"99999", defaultDeviceTokenStaleDays},
		{"0", 0}, // 0 是合法值（关闭检测）
	} {
		if err := store.SetKV(kvDeviceTokenStaleDays, tc.set); err != nil {
			t.Fatal(err)
		}
		if got := a.deviceTokenStaleDays(); got != tc.want {
			t.Errorf("阈值 %q → %d, want %d", tc.set, got, tc.want)
		}
	}
}

// TestSetDeviceTokenRecordsTimestamp 写入凭证必须刷新时刻；清除必须归零。
//
// 归零这一半同样重要：清除后若仍留着旧时刻，"没有凭证"会被算成"很久前录的凭证"，
// 于是该账号在审计里显示成 stale 而不是 none —— 状态互相矛盾。
func TestSetDeviceTokenRecordsTimestamp(t *testing.T) {
	store, px, _ := newTestApp(t, &Account{UID: "u1", AccessToken: "at"})
	defer store.Close()
	defer px.Close()

	before := time.Now().Unix()
	if err := store.SetAccountDeviceToken("u1", "  tok-1  "); err != nil {
		t.Fatal(err)
	}
	acc, err := store.GetAccount("u1")
	if err != nil || acc == nil {
		t.Fatalf("GetAccount: %v %v", acc, err)
	}
	if acc.DeviceToken != "tok-1" {
		t.Errorf("应 trim 后写入，得 %q", acc.DeviceToken)
	}
	if acc.DeviceTokenAt < before || acc.DeviceTokenAt > time.Now().Unix() {
		t.Errorf("写入时刻 = %d, want ≈%d", acc.DeviceTokenAt, before)
	}

	if err := store.SetAccountDeviceToken("u1", ""); err != nil {
		t.Fatal(err)
	}
	acc2, _ := store.GetAccount("u1")
	if acc2.DeviceTokenAt != 0 {
		t.Errorf("清除后时刻应归零，得 %d", acc2.DeviceTokenAt)
	}
}

// TestUpsertAccountKeepsDeviceTokenAt OAuth 重同步不得把凭证时刻洗成"现在"。
//
// 用 excluded.device_token_at 会让每次刷新 token 都重置该时刻，
// 于是"多久没换凭证"这个指标**永远显示新鲜** —— 正是需要它报警的场景反而消失。
func TestUpsertAccountKeepsDeviceTokenAt(t *testing.T) {
	store, px, _ := newTestApp(t, &Account{UID: "u1", AccessToken: "at"})
	defer store.Close()
	defer px.Close()

	old := time.Now().Add(-100 * 24 * time.Hour).Unix()
	if err := store.SetAccountDeviceToken("u1", "tok-old"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE accounts SET device_token_at = ? WHERE uid = 'u1'`, old); err != nil {
		t.Fatal(err)
	}

	// 模拟 OAuth 重同步：token 相同、但走一次 UpsertAccount。
	acc, _ := store.GetAccount("u1")
	acc.AccessToken = "at2"
	if err := store.UpsertAccount(acc); err != nil {
		t.Fatal(err)
	}
	after, _ := store.GetAccount("u1")
	if after.DeviceTokenAt != old {
		t.Errorf("重同步把凭证时刻洗成了 %d，want 保持 %d", after.DeviceTokenAt, old)
	}

	// 但如果**凭证本身变了**（重新采集），时刻应更新为传入值。
	acc2, _ := store.GetAccount("u1")
	acc2.DeviceToken = "tok-new"
	acc2.DeviceTokenAt = time.Now().Unix()
	if err := store.UpsertAccount(acc2); err != nil {
		t.Fatal(err)
	}
	after2, _ := store.GetAccount("u1")
	if after2.DeviceTokenAt == old {
		t.Error("凭证变化时时刻应随之更新")
	}
}
