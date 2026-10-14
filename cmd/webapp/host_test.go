// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"path/filepath"
	"testing"
)

// newHostProxy 造一个带库的 Proxy：账号池 + 代挂归属映射均由库同步而来。
func newHostProxy(t *testing.T, accounts ...*Account) (*Store, *Proxy) {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "host.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	for _, a := range accounts {
		if err := store.UpsertAccount(a); err != nil {
			t.Fatalf("UpsertAccount: %v", err)
		}
	}
	px := NewProxy(store, "")
	return store, px
}

// TestHostScopeSelection 校验平台通用 / 代挂自用两套选号范围互不串号。
func TestHostScopeSelection(t *testing.T) {
	store, px := newHostProxy(t,
		&Account{UID: "plat", Realm: "cn", AccessToken: "a"},                                     // 平台自持
		&Account{UID: "h_in", Realm: "cn", AccessToken: "a", OwnerUserID: 7, PoolState: "in"},    // 用户7，已入池
		&Account{UID: "h_new", Realm: "cn", AccessToken: "a", OwnerUserID: 7, PoolState: ""},     // 用户7，未入池
		&Account{UID: "h_other", Realm: "cn", AccessToken: "a", OwnerUserID: 8, PoolState: "in"}, // 用户8
	)
	defer store.Close()
	_ = px.SyncAccounts()

	// 平台通用：平台自持 + 已入池代挂号；未入池代挂号排除。
	if !px.allowsSelection("plat", pickScope{}) {
		t.Fatal("平台自持账号应在平台范围")
	}
	if !px.allowsSelection("h_in", pickScope{}) {
		t.Fatal("已入池代挂账号应在平台范围")
	}
	if px.allowsSelection("h_new", pickScope{}) {
		t.Fatal("未入池代挂账号不应参与平台反代")
	}

	// 代挂自用（用户7）：只有自己的账号；入池与否都可用。
	sc := pickScope{HostUser: 7}
	if !px.allowsSelection("h_in", sc) || !px.allowsSelection("h_new", sc) {
		t.Fatal("用户7 的两个代挂账号都应在自用范围")
	}
	if px.allowsSelection("plat", sc) || px.allowsSelection("h_other", sc) {
		t.Fatal("平台账号与他人账号不应出现在自用范围")
	}

	// 平台白名单不影响代挂自用（否则号主会被管理员分组设置挡住）。
	px.selMu.Lock()
	px.allowGroups = map[string]bool{"不存在的组": true}
	px.selMu.Unlock()
	if !px.allowsSelection("h_in", sc) {
		t.Fatal("代挂自用不应受平台分组白名单约束")
	}
	if px.allowsSelection("plat", pickScope{}) {
		t.Fatal("平台白名单应排除平台账号")
	}

	// 按代挂分组过滤。
	if err := store.SetAccountHostGroup("h_in", "主力"); err != nil {
		t.Fatalf("SetAccountHostGroup: %v", err)
	}
	_ = px.SyncAccounts()
	if !px.allowsSelection("h_in", pickScope{HostUser: 7, HostGroup: "主力"}) {
		t.Fatal("指定分组应含该组账号")
	}
	if px.allowsSelection("h_new", pickScope{HostUser: 7, HostGroup: "主力"}) {
		t.Fatal("指定分组应排除未分组账号")
	}
}

// TestHostKeyAccountBinding 校验代挂自用 Key 的账号绑定集合参与选号过滤（req 7）。
func TestHostKeyAccountBinding(t *testing.T) {
	store, px := newHostProxy(t,
		&Account{UID: "h_a", Realm: "cn", AccessToken: "a", OwnerUserID: 7},
		&Account{UID: "h_b", Realm: "cn", AccessToken: "a", OwnerUserID: 7},
		&Account{UID: "h_c", Realm: "cn", AccessToken: "a", OwnerUserID: 7},
	)
	defer store.Close()
	_ = px.SyncAccounts()

	// 未绑定（nil）= 全部本人账号可选。
	all := pickScope{HostUser: 7}
	for _, uid := range []string{"h_a", "h_b", "h_c"} {
		if !px.allowsSelection(uid, all) {
			t.Fatalf("未绑定时 %s 应可选", uid)
		}
	}
	// 绑定到 h_a / h_c → h_b 被排除。
	bound := pickScope{HostUser: 7, AccountUIDs: splitKVSet("h_a,h_c")}
	if !px.allowsSelection("h_a", bound) || !px.allowsSelection("h_c", bound) {
		t.Fatal("绑定集合内的账号应可选")
	}
	if px.allowsSelection("h_b", bound) {
		t.Fatal("绑定集合外的账号不应可选")
	}
	// 空串 → nil，等价未绑定。
	if got := splitKVSet(""); got != nil {
		t.Fatalf("空串应得 nil（不过滤），得 %v", got)
	}
}

// TestHostCreditsOwnerShare 校验分成入账：平台收取的点数按比例折入号主余额。
func TestHostCreditsOwnerShare(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "share.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()
	u, err := store.CreateUser("owner7", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := store.UpsertAccount(&Account{UID: "h_in", Realm: "cn", AccessToken: "a", OwnerUserID: u.ID, PoolState: "in"}); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	if err := store.UpsertAccount(&Account{UID: "h_new", Realm: "cn", AccessToken: "a", OwnerUserID: u.ID, PoolState: ""}); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	px := NewProxy(store, "")
	_ = px.SyncAccounts()

	if err := store.SetKV(kvHostShareRatio, "0.6"); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	px.ReloadSelection()
	if r := px.hostShareRatioNow(); r != 0.6 {
		t.Fatalf("分成比例应为 0.6，得 %v", r)
	}

	// 入池账号服务一次、平台收费 5 点 → 号主得 3 点。
	px.creditHostOwner("h_in", &User{ID: 99}, "deepseek", 5, 5*MicroPerPoint)

	fresh, _ := store.GetUserByID(u.ID)
	if want := int64(3 * MicroPerPoint); fresh.BalanceMicro != want {
		t.Fatalf("号主余额应为 %d 微点，得 %d", want, fresh.BalanceMicro)
	}
	total, count, err := store.HostEarningSummary(u.ID)
	if err != nil || count != 1 || total != 3*MicroPerPoint {
		t.Fatalf("收益台账应为 1 笔 3 点，得 count=%d total=%d err=%v", count, total, err)
	}
	acc, _ := store.GetAccount("h_in")
	if acc.EarnedMicro != 3*MicroPerPoint {
		t.Fatalf("账号累计收益应为 3 点，得 %d", acc.EarnedMicro)
	}

	// 未入池账号不产生分成。
	px.creditHostOwner("h_new", &User{ID: 99}, "deepseek", 5, 5*MicroPerPoint)
	fresh2, _ := store.GetUserByID(u.ID)
	if fresh2.BalanceMicro != 3*MicroPerPoint {
		t.Fatalf("未入池账号不应产生分成，余额仍应为 3 点，得 %d", fresh2.BalanceMicro)
	}

	// 关闭分成（0）后不再入账。
	if err := store.SetKV(kvHostShareRatio, "0"); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	px.ReloadSelection()
	px.creditHostOwner("h_in", &User{ID: 99}, "deepseek", 5, 5*MicroPerPoint)
	fresh3, _ := store.GetUserByID(u.ID)
	if fresh3.BalanceMicro != 3*MicroPerPoint {
		t.Fatalf("分成关闭后不应入账，得 %d", fresh3.BalanceMicro)
	}
}

// TestHostKeySkipsCharge 校验代挂自用 Key 结算时不扣余额、不产分成。
func TestHostKeySkipsCharge(t *testing.T) {
	store, px := newHostProxy(t,
		&Account{UID: "h_in", Realm: "cn", AccessToken: "a", OwnerUserID: 7, PoolState: "in"},
	)
	defer store.Close()
	u, err := store.CreateUser("u7", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := store.AddUserBalance(u.ID, 100*MicroPerPoint); err != nil {
		t.Fatalf("AddUserBalance: %v", err)
	}
	if err := store.SetAccountOwner("h_in", u.ID); err != nil {
		t.Fatalf("SetAccountOwner: %v", err)
	}
	if err := store.SetKV(kvHostShareRatio, "0.6"); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	px.ReloadSelection()
	_ = px.SyncAccounts()

	key := &APIKey{ID: 1, UserID: u.ID, Kind: KeyKindHost}
	ui := usageInfo{Credit: 5, HasCredit: true}
	settled, charged := px.settleUsage(u, key, "deepseek", "h_in", ui, nil, "ok", "", "", 1)
	if !settled || charged != 0 {
		t.Fatalf("代挂自用应零费用，得 settled=%v charged=%d", settled, charged)
	}
	fresh, _ := store.GetUserByID(u.ID)
	if fresh.BalanceMicro != 100*MicroPerPoint {
		t.Fatalf("代挂自用不应扣余额，得 %d", fresh.BalanceMicro)
	}
	// 自用不产生分成（否则等于自己给自己返现）。
	total, count, _ := store.HostEarningSummary(u.ID)
	if count != 0 || total != 0 {
		t.Fatalf("代挂自用不应产生分成，得 count=%d total=%d", count, total)
	}
}
