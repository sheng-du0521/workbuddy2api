// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"path/filepath"
	"testing"
)

// newReferralProxy 造一个库 + Proxy（供返点测试复用）。
func newReferralProxy(t *testing.T, name string) (*Store, *Proxy) {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	return store, NewProxy(store, "")
}

// TestInviteCodeAndReferralBinding 校验邀请码生成、查号、绑定与下级计数。
func TestInviteCodeAndReferralBinding(t *testing.T) {
	store, _ := newReferralProxy(t, "invite.db")
	defer store.Close()

	a, err := store.CreateUser("alice", "x")
	if err != nil {
		t.Fatalf("CreateUser alice: %v", err)
	}
	b, err := store.CreateUser("bob", "x")
	if err != nil {
		t.Fatalf("CreateUser bob: %v", err)
	}
	if a.InviteCode == "" || b.InviteCode == "" {
		t.Fatalf("注册应自动生成邀请码，得 a=%q b=%q", a.InviteCode, b.InviteCode)
	}
	if a.InviteCode == b.InviteCode {
		t.Fatalf("邀请码应唯一，得相同 %q", a.InviteCode)
	}

	// 按码查人（大小写不敏感）。
	if got, err := store.GetUserByInviteCode(lowerString(a.InviteCode)); err != nil || got.ID != a.ID {
		t.Fatalf("GetUserByInviteCode 应命中 alice，得 %+v err=%v", got, err)
	}
	if _, err := store.GetUserByInviteCode(""); err == nil {
		t.Fatal("空邀请码不应命中任何用户")
	}
	if _, err := store.GetUserByInviteCode("ZZZZZZZZ"); err == nil {
		t.Fatal("不存在的邀请码不应命中")
	}

	// 绑定推荐关系 + 下级计数。
	if err := store.SetUserReferredBy(b.ID, a.ID); err != nil {
		t.Fatalf("SetUserReferredBy: %v", err)
	}
	if n, _ := store.CountReferred(a.ID); n != 1 {
		t.Fatalf("alice 应有 1 个下级，得 %d", n)
	}
	freshB, _ := store.GetUserByID(b.ID)
	if freshB.ReferredBy != a.ID {
		t.Fatalf("bob 的上级应为 alice(%d)，得 %d", a.ID, freshB.ReferredBy)
	}
	// 重复绑定不应覆盖（WHERE referred_by = 0）。
	c, err := store.CreateUser("carol", "x")
	if err != nil {
		t.Fatalf("CreateUser carol: %v", err)
	}
	if err := store.SetUserReferredBy(b.ID, c.ID); err != nil {
		t.Fatalf("对已绑定用户再次绑定不应报错: %v", err)
	}
	freshB2, _ := store.GetUserByID(b.ID)
	if freshB2.ReferredBy != a.ID {
		t.Fatalf("已绑定的上级不应被覆盖，得 %d", freshB2.ReferredBy)
	}
	// 不能绑定自己。
	if err := store.SetUserReferredBy(a.ID, a.ID); err == nil {
		t.Fatal("不应允许把自己绑定为上级")
	}

	// EnsureInviteCode 对已有码幂等。
	if c, err := store.EnsureInviteCode(a.ID); err != nil || c != a.InviteCode {
		t.Fatalf("EnsureInviteCode 应返回原码，得 %q err=%v", c, err)
	}
}

// lowerString 把邀请码转小写，验证查号大小写不敏感。
func lowerString(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

// TestReferralRechargeRebate 校验下级兑换充值码后上级按比例拿返点。
func TestReferralRechargeRebate(t *testing.T) {
	store, px := newReferralProxy(t, "rechrg.db")
	defer store.Close()

	referrer, _ := store.CreateUser("referrer", "x")
	downline, _ := store.CreateUser("downline", "x")
	if err := store.SetUserReferredBy(downline.ID, referrer.ID); err != nil {
		t.Fatalf("SetUserReferredBy: %v", err)
	}
	if err := store.SetKV(kvReferralRatio, "0.1"); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	px.ReloadSelection()
	if r := px.referralRatioNow(); r != 0.1 {
		t.Fatalf("推广返点比例应为 0.1，得 %v", r)
	}

	a := &app{store: store, proxy: px}
	// handler 侧取到的是「已绑定后」的用户（referred_by 已落库），此处同样重新读一次。
	downFresh, _ := store.GetUserByID(downline.ID)
	// 下级到账 100 点 → 上级拿 10 点。
	a.creditReferralOnRecharge(downFresh, 100*MicroPerPoint)

	fresh, _ := store.GetUserByID(referrer.ID)
	if want := int64(10 * MicroPerPoint); fresh.BalanceMicro != want {
		t.Fatalf("上级应得 10 点返点，得 %d 微点", fresh.BalanceMicro)
	}
	byKind, total, _ := store.RebateSummaryByKind(referrer.ID)
	if byKind[RebateReferralRecharge] != 10*MicroPerPoint || total != 10*MicroPerPoint {
		t.Fatalf("返点台账应为 referral_recharge 10 点，得 %v total=%d", byKind, total)
	}

	// 关闭推广返点后不再入账。
	if err := store.SetKV(kvReferralRatio, "0"); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	px.ReloadSelection()
	a.creditReferralOnRecharge(downFresh, 100*MicroPerPoint)
	fresh2, _ := store.GetUserByID(referrer.ID)
	if fresh2.BalanceMicro != 10*MicroPerPoint {
		t.Fatalf("关闭后不应再入账，得 %d", fresh2.BalanceMicro)
	}
}

// TestReferralPoolRebate 校验下级号池收益按比例返给上级，且不侵占平台留成。
func TestReferralPoolRebate(t *testing.T) {
	store, px := newReferralProxy(t, "poolref.db")
	defer store.Close()

	referrer, _ := store.CreateUser("referrer", "x")
	owner, _ := store.CreateUser("owner", "x")
	if err := store.SetUserReferredBy(owner.ID, referrer.ID); err != nil {
		t.Fatalf("SetUserReferredBy: %v", err)
	}
	if err := store.UpsertAccount(&Account{UID: "h_in", Realm: "cn", AccessToken: "a",
		OwnerUserID: owner.ID, PoolState: "in"}); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	if err := store.SetKV(kvHostShareRatio, "0.6"); err != nil {
		t.Fatalf("SetKV host: %v", err)
	}
	if err := store.SetKV(kvReferralRatio, "0.1"); err != nil {
		t.Fatalf("SetKV ref: %v", err)
	}
	px.ReloadSelection()
	_ = px.SyncAccounts()

	// 平台收费 10 点 → 号主 6 点；上级再拿 6 × 0.1 = 0.6 点。
	px.creditHostOwner("h_in", &User{ID: 99}, "deepseek", 10, 10*MicroPerPoint)

	ownerFresh, _ := store.GetUserByID(owner.ID)
	if want := int64(6 * MicroPerPoint); ownerFresh.BalanceMicro != want {
		t.Fatalf("号主应得 6 点，得 %d", ownerFresh.BalanceMicro)
	}
	refFresh, _ := store.GetUserByID(referrer.ID)
	if want := int64(0.6 * MicroPerPoint); refFresh.BalanceMicro != want {
		t.Fatalf("上级应得 0.6 点（600 微点），得 %d", refFresh.BalanceMicro)
	}
	byKind, _, _ := store.RebateSummaryByKind(referrer.ID)
	if byKind[RebateReferralPool] != 600 {
		t.Fatalf("返点台账应为 referral_pool 600 微点，得 %d", byKind[RebateReferralPool])
	}
}

// TestReferralPoolRebateClampedToMargin 校验返点被钳制在平台留成内（不侵占号主分成）。
func TestReferralPoolRebateClampedToMargin(t *testing.T) {
	store, px := newReferralProxy(t, "clamp.db")
	defer store.Close()

	referrer, _ := store.CreateUser("referrer", "x")
	owner, _ := store.CreateUser("owner", "x")
	if err := store.SetUserReferredBy(owner.ID, referrer.ID); err != nil {
		t.Fatalf("SetUserReferredBy: %v", err)
	}
	if err := store.UpsertAccount(&Account{UID: "h_in", Realm: "cn", AccessToken: "a",
		OwnerUserID: owner.ID, PoolState: "in"}); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	// 极端配置：分成 0.95 + 推广 0.5 → 原始返点 0.95×0.5=0.475 点，但平台留成仅 0.05 点。
	if err := store.SetKV(kvHostShareRatio, "0.95"); err != nil {
		t.Fatalf("SetKV host: %v", err)
	}
	if err := store.SetKV(kvReferralRatio, "0.5"); err != nil {
		t.Fatalf("SetKV ref: %v", err)
	}
	px.ReloadSelection()
	_ = px.SyncAccounts()

	px.creditHostOwner("h_in", &User{ID: 99}, "deepseek", 10, 10*MicroPerPoint)

	refFresh, _ := store.GetUserByID(referrer.ID)
	if want := int64(0.5 * MicroPerPoint); refFresh.BalanceMicro != want {
		t.Fatalf("返点应被钳到平台留成 0.5 点，得 %d", refFresh.BalanceMicro)
	}
}

// TestReferralRebateRoundsSmallAmounts 校验小额返点用四舍五入而非截断。
//
// 回归：10 微点收费 × 0.6 分成 × 0.1 推广 = 0.6 微点，截断会变 0，
// 导致下级号池收益永远不给上级返点。
func TestReferralRebateRoundsSmallAmounts(t *testing.T) {
	store, px := newReferralProxy(t, "round.db")
	defer store.Close()

	referrer, _ := store.CreateUser("referrer", "x")
	owner, _ := store.CreateUser("owner", "x")
	if err := store.SetUserReferredBy(owner.ID, referrer.ID); err != nil {
		t.Fatalf("SetUserReferredBy: %v", err)
	}
	if err := store.UpsertAccount(&Account{UID: "h_in", Realm: "cn", AccessToken: "a",
		OwnerUserID: owner.ID, PoolState: "in"}); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	if err := store.SetKV(kvHostShareRatio, "0.6"); err != nil {
		t.Fatalf("SetKV host: %v", err)
	}
	if err := store.SetKV(kvReferralRatio, "0.1"); err != nil {
		t.Fatalf("SetKV ref: %v", err)
	}
	px.ReloadSelection()
	_ = px.SyncAccounts()

	// 收费仅 10 微点：号主得 round(6)=6 微点，上级得 round(0.6)=1 微点。
	px.creditHostOwner("h_in", &User{ID: 99}, "m", 0.01, 10)

	refFresh, _ := store.GetUserByID(referrer.ID)
	if refFresh.BalanceMicro != 1 {
		t.Fatalf("小额返点应四舍五入为 1 微点（而非截断为 0），得 %d", refFresh.BalanceMicro)
	}
	byKind, _, _ := store.RebateSummaryByKind(referrer.ID)
	if byKind[RebateReferralPool] != 1 {
		t.Fatalf("返点台账应为 1 微点，得 %d", byKind[RebateReferralPool])
	}
}

// TestUsageBreakdownScopes 校验三类账单的 scope 归类与聚合。
func TestUsageBreakdownScopes(t *testing.T) {
	store, _ := newReferralProxy(t, "scope.db")
	defer store.Close()
	u, _ := store.CreateUser("u", "x")

	// 两笔「使用号池」+ 一笔「自己代挂使用」。
	_ = store.InsertUsageLog(&UsageLog{UserID: u.ID, Model: "m1", PromptTokens: 10, CompletionTokens: 5,
		ChargedMicro: 3 * MicroPerPoint, Scope: ScopePool, Status: "ok"})
	_ = store.InsertUsageLog(&UsageLog{UserID: u.ID, Model: "m1", PromptTokens: 20, CompletionTokens: 10,
		ChargedMicro: 2 * MicroPerPoint, Scope: ScopePool, Status: "ok"})
	_ = store.InsertUsageLog(&UsageLog{UserID: u.ID, Model: "m2", PromptTokens: 30, CompletionTokens: 30,
		UpstreamCredit: 1.5, ChargedMicro: 0, Scope: ScopeHost, Status: "ok"})

	poolDaily, _ := store.UsageDailyScoped(u.ID, 14, ScopePool)
	if len(poolDaily) != 1 || poolDaily[0].Requests != 2 || poolDaily[0].ChargedMicro != 5*MicroPerPoint {
		t.Fatalf("用号池应为 1 天 2 请求 5 点，得 %+v", poolDaily)
	}
	hostDaily, _ := store.UsageDailyScoped(u.ID, 14, ScopeHost)
	if len(hostDaily) != 1 || hostDaily[0].Requests != 1 || hostDaily[0].ChargedMicro != 0 ||
		hostDaily[0].UpstreamCredit != 1.5 {
		t.Fatalf("自用应为 1 天 1 请求 0 扣费 1.5 credit，得 %+v", hostDaily)
	}
	// scope 为空 = 全部。
	allDaily, _ := store.UsageDailyScoped(u.ID, 14, "")
	if len(allDaily) != 1 || allDaily[0].Requests != 3 {
		t.Fatalf("全部应为 3 请求，得 %+v", allDaily)
	}
	// 默认写入（未指定 scope）应归为 pool。
	_ = store.InsertUsageLog(&UsageLog{UserID: u.ID, Model: "m3", PromptTokens: 1, CompletionTokens: 1})
	allDaily2, _ := store.UsageDailyScoped(u.ID, 14, "")
	if allDaily2[0].Requests != 4 {
		t.Fatalf("默认 scope 应计入 pool，得 %+v", allDaily2)
	}
	hostDaily2, _ := store.UsageDailyScoped(u.ID, 14, ScopeHost)
	if hostDaily2[0].Requests != 1 {
		t.Fatalf("默认 scope 不应计入 host，得 %+v", hostDaily2)
	}
}
