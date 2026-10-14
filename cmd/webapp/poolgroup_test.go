// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// hostedInPool 造一个「用户自己的、已入池」的代挂账号。
func hostedInPool(uid string, owner int64) *Account {
	return &Account{
		UID: uid, Realm: "cn", AccessToken: "a",
		OwnerUserID: owner, PoolState: "in",
	}
}

// TestPoolGroupTargetPrepEntry 规则 A：已入池账号积分跌破预备线 → 目标为预备组。
func TestPoolGroupTargetPrepEntry(t *testing.T) {
	acc := hostedInPool("u1", 7)
	// 跌破 100 → 预备组
	if target, note := poolGroupTarget(acc, prepEnterCredits-1, true); target != cgPrep {
		t.Fatalf("credits<100 must target prep, got %q", target)
	} else if !strings.HasPrefix(note, autoTagPrefix) {
		t.Fatalf("prep note must carry auto prefix, got %q", note)
	}
	// 恰好 100（不严格小于）→ 不搬
	if target, _ := poolGroupTarget(acc, prepEnterCredits, true); target != "" {
		t.Fatalf("credits==100 must not move to prep, got %q", target)
	}
	// 滞回带内（1000）→ 不变
	if target, _ := poolGroupTarget(acc, 1000, true); target != "" {
		t.Fatalf("credits in hysteresis band must not change, got %q", target)
	}
	// 已在预备组 + 仍是低积分 → 幂等，不重复写
	acc2 := hostedInPool("u2", 7)
	acc2.ConsumeGroup = cgPrep
	acc2.ConsumeGroupNote = prepGroupNote
	if target, _ := poolGroupTarget(acc2, 50, true); target != "" {
		t.Fatalf("already in prep with low credits must be idempotent, got %q", target)
	}
}

// TestPoolGroupTargetPrepReturn 规则 B：预备组账号积分恢复到回流线 → 回用户申请组。
func TestPoolGroupTargetPrepReturn(t *testing.T) {
	acc := hostedInPool("u1", 7)
	acc.ConsumeGroup = cgPrep
	acc.ConsumeGroupNote = prepGroupNote

	// 达到 2000 → 回用户申请组
	if target, note := poolGroupTarget(acc, prepReturnCredits, true); target != cgApply {
		t.Fatalf("credits>=2000 in prep must return to apply, got %q", target)
	} else if note != "" {
		t.Fatalf("return must clear note, got %q", note)
	}
	// 差一点（1999）→ 不回流（滞回带内保持预备组）
	if target, _ := poolGroupTarget(acc, prepReturnCredits-1, true); target != "" {
		t.Fatalf("credits<2000 in prep must stay, got %q", target)
	}
	// 不在预备组的账号哪怕积分很高也不动（只对预备组成员回流）
	acc2 := hostedInPool("u2", 7)
	if target, _ := poolGroupTarget(acc2, 99999, true); target != "" {
		t.Fatalf("non-prep account must not be moved by return rule, got %q", target)
	}
}

// TestPoolGroupTargetPriority 判定优先级：退出号池 / 平台自持 / 积分未知 一律不干预。
func TestPoolGroupTargetPriority(t *testing.T) {
	// 优先级 1：已退出号池（pool_state != "in"）→ 不干预（即使积分很低）
	left := &Account{UID: "u1", OwnerUserID: 7, PoolState: ""}
	if target, _ := poolGroupTarget(left, 1, true); target != "" {
		t.Fatalf("account not in pool must be untouched, got %q", target)
	}
	// 优先级 2：平台自持账号 → 不参与自动分组
	platform := &Account{UID: "u2", OwnerUserID: 0, PoolState: "in"}
	if target, _ := poolGroupTarget(platform, 1, true); target != "" {
		t.Fatalf("platform-owned account must be untouched, got %q", target)
	}
	// 优先级 3：积分未知（creditsSeenAt == 0）→ 不判定
	unknown := hostedInPool("u3", 7)
	if target, _ := poolGroupTarget(unknown, 0, false); target != "" {
		t.Fatalf("unknown credits must not trigger regroup, got %q", target)
	}
	// 对照：同样的 0 积分但**已知** → 进预备组（证明 unknown 分支确有区分作用）
	if target, _ := poolGroupTarget(unknown, 0, true); target != cgPrep {
		t.Fatalf("known 0 credits must target prep, got %q", target)
	}
}

// TestReconcilePoolGroupOne 端到端：写库 + 内存选号映射同步 + 幂等。
func TestReconcilePoolGroupOne(t *testing.T) {
	store, px, app := newTestApp(t, hostedInPool("u1", 7))
	defer store.Close()
	if err := store.SetAccountOwner("u1", 7); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAccountPoolState("u1", "in", ""); err != nil {
		t.Fatal(err)
	}
	_ = px.SyncAccounts()

	// 模拟一次权威积分读数：50（低于预备线）
	px.pool.SetCreditsDetailed("u1", 50, 0)

	app.reconcilePoolGroupOne("u1")

	acc, _ := store.GetAccount("u1")
	if effectiveConsumeGroup(acc) != cgPrep {
		t.Fatalf("account must be moved to prep, got %q", effectiveConsumeGroup(acc))
	}
	if !strings.HasPrefix(acc.ConsumeGroupNote, autoTagPrefix) {
		t.Fatalf("prep note must carry auto prefix, got %q", acc.ConsumeGroupNote)
	}
	// 内存选号映射必须同步（否则本进程内仍按旧分组选号）
	if got := px.ConsumeGroupForUID("u1"); got != cgPrep {
		t.Fatalf("in-memory group must sync to prep, got %q", got)
	}
	// 预备组不参与消耗
	if consumeGroupParticipates(px.ConsumeGroupForUID("u1")) {
		t.Fatal("prep group must not participate in platform consumption")
	}

	// 积分恢复到 2000 → 回流用户申请组
	px.pool.SetCreditsDetailed("u1", 2000, 0)
	app.poolGroups = nil // 重置节流器（同一 uid 在 1s 窗口内会被节流）
	app.reconcilePoolGroupOne("u1")
	acc2, _ := store.GetAccount("u1")
	if effectiveConsumeGroup(acc2) != cgApply {
		t.Fatalf("account must return to apply, got %q", effectiveConsumeGroup(acc2))
	}
	if acc2.ConsumeGroupNote != "" {
		t.Fatalf("return must clear note, got %q", acc2.ConsumeGroupNote)
	}
	if !consumeGroupParticipates(px.ConsumeGroupForUID("u1")) {
		t.Fatal("apply group must participate in platform consumption")
	}
}

// TestReconcilePoolGroupsThrottle 节流：同一 uid 在窗口内的第二次调用不判定。
func TestReconcilePoolGroupsThrottle(t *testing.T) {
	store, px, app := newTestApp(t, hostedInPool("u1", 7))
	defer store.Close()
	_ = px.SyncAccounts()
	px.pool.SetCreditsDetailed("u1", 50, 0)

	app.reconcilePoolGroupOne("u1")
	if got := px.ConsumeGroupForUID("u1"); got != cgPrep {
		t.Fatalf("first call must apply, got %q", got)
	}
	// 手工改回反代组，模拟「另一个操作改了分组」；节流窗口内再调不应覆盖
	_ = store.SetAccountConsumeGroup("u1", cgProxy)
	px.SetConsumeGroupInMemory("u1", cgProxy)
	app.reconcilePoolGroupOne("u1")
	if got := px.ConsumeGroupForUID("u1"); got != cgProxy {
		t.Fatalf("throttled call must be a no-op, got %q", got)
	}
	_ = store.SetAccountConsumeGroup("u1", "")
}

// TestHostPoolApplyMinCredits 入池积分门槛（2000）：不足 / 未知 / 达标 三条路径。
func TestHostPoolApplyMinCredits(t *testing.T) {
	store, px, app := newTestApp(t, &Account{UID: "u1", Realm: "cn", AccessToken: "a", OwnerUserID: 7})
	defer store.Close()
	me := mkUser(t, store, "me", 0)
	_ = store.SetAccountOwner("u1", me.ID)
	_ = px.SyncAccounts()

	apply := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/host/accounts/u1/pool",
			strings.NewReader(`{"action":"apply","note":"申请"}`))
		req.SetPathValue("uid", "u1")
		req = withUser(req, me)
		rec := httptest.NewRecorder()
		app.handleHostPool(rec, req)
		return rec
	}

	// ① 积分未知（从未拉取）→ 拒绝，提示先刷新
	if rec := apply(); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "积分") {
		t.Fatalf("unknown credits must be rejected: code=%d body=%s", rec.Code, rec.Body)
	}
	// ② 积分不足门槛 → 拒绝
	px.pool.SetCreditsDetailed("u1", poolApplyMinCredits-1, 0)
	if rec := apply(); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "积分不足") {
		t.Fatalf("credits below min must be rejected: code=%d body=%s", rec.Code, rec.Body)
	}
	// ③ 恰好达标 → 通过，状态 pending
	px.pool.SetCreditsDetailed("u1", poolApplyMinCredits, 0)
	if rec := apply(); rec.Code != http.StatusOK {
		t.Fatalf("credits == min must pass: code=%d body=%s", rec.Code, rec.Body)
	}
	acc, _ := store.GetAccount("u1")
	if acc.PoolState != "pending" {
		t.Fatalf("pool state must be pending, got %q", acc.PoolState)
	}
}

// TestAdminReviewRechecksCredits 审批放行前复核积分（申请后积分可能被消耗掉）。
func TestAdminReviewRechecksCredits(t *testing.T) {
	store, px, app := newTestApp(t, &Account{UID: "u1", Realm: "cn", AccessToken: "a",
		OwnerUserID: 7, PoolState: "pending"})
	defer store.Close()
	me := mkUser(t, store, "me", 0)
	_ = store.SetAccountOwner("u1", me.ID)
	_ = px.SyncAccounts()

	review := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/admin/host/accounts/u1/review",
			strings.NewReader(`{"action":"approve"}`))
		req.SetPathValue("uid", "u1")
		rec := httptest.NewRecorder()
		app.handleAdminHostReview(rec, req) // 管理密钥通道：无 user，不得 panic
		return rec
	}
	// 积分已掉到门槛下 → 拒绝放行
	px.pool.SetCreditsDetailed("u1", poolApplyMinCredits-1, 0)
	if rec := review(); rec.Code != http.StatusBadRequest {
		t.Fatalf("review must reject when credits below min: code=%d body=%s", rec.Code, rec.Body)
	}
	acc, _ := store.GetAccount("u1")
	if acc.PoolState == "in" {
		t.Fatal("account must not be approved")
	}
	// 积分补足 → 放行
	px.pool.SetCreditsDetailed("u1", poolApplyMinCredits, 0)
	if rec := review(); rec.Code != http.StatusOK {
		t.Fatalf("review must approve when credits ok: code=%d body=%s", rec.Code, rec.Body)
	}
	acc2, _ := store.GetAccount("u1")
	if acc2.PoolState != "in" {
		t.Fatalf("pool state must be in, got %q", acc2.PoolState)
	}
}

// TestLeavePoolClearsAutoPrep 退出号池时清掉「系统自动搬入的预备组」，
// 但**不动**管理员手动设置的分组。
func TestLeavePoolClearsAutoPrep(t *testing.T) {
	store, px, app := newTestApp(t, &Account{UID: "u1", Realm: "cn", AccessToken: "a", OwnerUserID: 7})
	defer store.Close()
	me := mkUser(t, store, "me", 0)
	_ = store.SetAccountOwner("u1", me.ID)
	_ = store.SetAccountPoolState("u1", "in", "")
	_ = px.SyncAccounts()

	leave := func(uid string) {
		req := httptest.NewRequest("POST", "/api/host/accounts/"+uid+"/pool",
			strings.NewReader(`{"action":"leave"}`))
		req.SetPathValue("uid", uid)
		req = withUser(req, me)
		app.handleHostPool(httptest.NewRecorder(), req)
	}

	// 场景一：系统自动搬入预备组 → 退出时应清空显式分组
	px.pool.SetCreditsDetailed("u1", 50, 0)
	app.reconcilePoolGroupOne("u1")
	if effectiveConsumeGroup(mustAccount(t, app, "u1")) != cgPrep {
		t.Fatal("setup: must be in prep")
	}
	leave("u1")
	acc := mustAccount(t, app, "u1")
	if acc.ConsumeGroup != "" {
		t.Fatalf("leaving pool must clear auto prep group, got %q", acc.ConsumeGroup)
	}
	if effectiveConsumeGroup(acc) != cgIdle {
		t.Fatalf("not-in-pool hosted account must infer idle, got %q", effectiveConsumeGroup(acc))
	}
}

// TestLeavePoolKeepsManualGroup 退出号池**不**应清掉管理员手动设的分组。
func TestLeavePoolKeepsManualGroup(t *testing.T) {
	store, px, app := newTestApp(t, &Account{UID: "u2", Realm: "cn", AccessToken: "a", OwnerUserID: 7})
	defer store.Close()
	me := mkUser(t, store, "me2", 0)
	_ = store.SetAccountOwner("u2", me.ID)
	_ = store.SetAccountPoolState("u2", "in", "")
	// 管理员手动设为反代组（无 auto 标记）
	_ = store.SetAccountConsumeGroup("u2", cgProxy)
	_ = px.SyncAccounts()

	req := httptest.NewRequest("POST", "/api/host/accounts/u2/pool",
		strings.NewReader(`{"action":"leave"}`))
	req.SetPathValue("uid", "u2")
	req = withUser(req, me)
	app.handleHostPool(httptest.NewRecorder(), req)

	acc := mustAccount(t, app, "u2")
	if acc.ConsumeGroup != cgProxy {
		t.Fatalf("manual group must be preserved on leave, got %q", acc.ConsumeGroup)
	}
}

// TestCreditsSeenAtPersisted 权威读数时刻随池状态持久化（重启后不必等下次签到）。
func TestCreditsSeenAtPersisted(t *testing.T) {
	store, px, _ := newTestApp(t, hostedInPool("u1", 7))
	defer store.Close()
	_ = px.SyncAccounts()

	st0, _ := px.pool.Status("u1")
	if st0.CreditsSeenAt != 0 {
		t.Fatalf("fresh account must have zero CreditsSeenAt, got %d", st0.CreditsSeenAt)
	}
	px.pool.SetCreditsDetailed("u1", 1234, 0)
	st1, _ := px.pool.Status("u1")
	if st1.CreditsSeenAt == 0 {
		t.Fatal("SetCreditsDetailed must stamp CreditsSeenAt")
	}
	if time.Since(time.Unix(st1.CreditsSeenAt, 0)) > time.Minute {
		t.Fatalf("CreditsSeenAt must be ~now, got %d", st1.CreditsSeenAt)
	}
	// NoteModelCost（实时扣减）**不应**刷新该时刻——它不是一次权威读数。
	px.pool.NoteModelCost("u1", "m", 5, 1000)
	st2, _ := px.pool.Status("u1")
	if st2.CreditsSeenAt != st1.CreditsSeenAt {
		t.Fatalf("NoteModelCost must not restamp CreditsSeenAt: %d -> %d", st1.CreditsSeenAt, st2.CreditsSeenAt)
	}
	if st2.Credits >= st1.Credits {
		t.Fatalf("NoteModelCost must deduct credits: %d -> %d", st1.Credits, st2.Credits)
	}
}
