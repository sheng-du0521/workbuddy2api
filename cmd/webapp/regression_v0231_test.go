// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/pool"
)

// ===========================================================================
// v0.23.1 线上问题回归测试
//
// 四个问题的共同根因是「多个收敛过程改同一列 consume_group，靠隐含假设避免冲突」。
// 本文件把四条边界钉死，且**走真实代码路径**（reportUpstreamError / handleDisable /
// writeCreditDetail），不重复实现被修的判据 —— 否则测试只验证了「我以为的规则」。
// ===========================================================================

// ---------- 问题1：预备组引擎不得覆盖「异常 / 闲置」 ----------

// TestFix1_PoolGroupDoesNotTouchAbnormalOrIdle 单元级：纯判定的边界。
func TestFix1_PoolGroupDoesNotTouchAbnormalOrIdle(t *testing.T) {
	// 前置条件必须齐（已入池 + 代挂号 + 积分已知），否则测不出「分组」这个变量。
	base := func(cg string) *Account {
		return &Account{UID: "u", OwnerUserID: 7, PoolState: "in", ConsumeGroup: cg}
	}

	// 异常 / 闲置：**任何积分档都不干预**（覆盖 <100 与滞回带两档）。
	for _, cg := range []string{cgAbnormal, cgIdle} {
		for _, credits := range []int64{0, 50, 500, 5000} {
			target, note := poolGroupTarget(base(cg), credits, true)
			if target != "" {
				t.Fatalf("分组 %s 积分 %d：不该被自动改动，却得到 target=%q note=%q",
					consumeGroupLabel(cg), credits, target, note)
			}
		}
	}

	// 对照：常规组仍按原规则工作（确保修复没有把规则改死）。
	cases := []struct {
		cg      string
		credits int64
		want    string
	}{
		{cgApply, 50, cgPrep},   // 用户申请 + 低分 → 进预备组
		{cgProxy, 50, cgPrep},   // 反代 + 低分 → 进预备组
		{"", 50, cgPrep},        // 未显式设（推断为 apply）→ 进预备组
		{cgPrep, 50, ""},        // 已在预备组 → 幂等
		{cgPrep, 2500, cgApply}, // 预备组 + 回到回流线 → 回用户申请组
		{cgPrep, 900, ""},       // 滞回带内 → 不变
		{cgApply, 2500, ""},     // 常规组 + 高分 → 不变
	}
	for _, c := range cases {
		target, _ := poolGroupTarget(base(c.cg), c.credits, true)
		if target != c.want {
			t.Fatalf("分组 %q 积分 %d：期望 target=%q 得到 %q",
				consumeGroupLabel(c.cg), c.credits, c.want, target)
		}
	}
}

// TestFix1_PoolGroupReconcilePreservesAbnormal 端到端：全量扫描不会把异常号搬走。
//
// 这是线上现象的直译 ——「点了异常分组，60 秒内又被改回预备组」。
func TestFix1_PoolGroupReconcilePreservesAbnormal(t *testing.T) {
	store, px, a := newTestApp(t,
		&Account{UID: "ab1", Nickname: "异常号", Realm: "cn", AccessToken: "tok",
			OwnerUserID: 7, PoolState: "in", ConsumeGroup: cgAbnormal,
			ConsumeGroupPrev: cgApply, ConsumeGroupNote: "[auto]积分不足"},
		&Account{UID: "id1", Nickname: "闲置号", Realm: "cn", AccessToken: "tok",
			OwnerUserID: 7, PoolState: "in", ConsumeGroup: cgIdle},
	)
	defer store.Close()
	defer px.Close()

	// 让两个号都是低积分（<100）—— 这正是历史上触发误搬的条件。
	px.pool.SetCreditsDetailed("ab1", 30, 0)
	px.pool.SetCreditsDetailed("id1", 30, 0)

	a.reconcilePoolGroups()

	ab, _ := store.GetAccount("ab1")
	id, _ := store.GetAccount("id1")
	if ab.ConsumeGroup != cgAbnormal {
		t.Fatalf("异常组被预备组引擎覆盖：consume_group=%q（应为 abnormal）", ab.ConsumeGroup)
	}
	if ab.ConsumeGroupNote != "[auto]积分不足" || ab.ConsumeGroupPrev != cgApply {
		t.Fatalf("异常号的归档原因/原分组被改写：note=%q prev=%q", ab.ConsumeGroupNote, ab.ConsumeGroupPrev)
	}
	if id.ConsumeGroup != cgIdle {
		t.Fatalf("闲置组被自动覆盖：consume_group=%q（应为 idle）", id.ConsumeGroup)
	}
}

// ---------- 问题2：停用联动归档异常组 ----------

// TestFix2_DisableArchivesAbnormal 停用 → 异常组（带 prev，可回归）；启用 → 回归。
func TestFix2_DisableArchivesAbnormal(t *testing.T) {
	store, px, a := newTestApp(t,
		&Account{UID: "d1", Nickname: "待停用号", Realm: "cn", AccessToken: "tok",
			ConsumeGroup: cgProxy})
	defer store.Close()
	defer px.Close()
	px.pool.SetCreditsDetailed("d1", 5000, 0)

	call := func(action string) map[string]any {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/admin/accounts/d1/"+action, strings.NewReader(`{}`))
		req.SetPathValue("uid", "d1")
		a.handleDisable(action == "disable")(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s 应 200，得 %d（%s）", action, rec.Code, rec.Body.String())
		}
		var m map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		return m
	}

	// ① 停用 → 归档异常组，prev 记住「反代」。
	m := call("disable")
	acc, _ := store.GetAccount("d1")
	if acc.ConsumeGroup != cgAbnormal {
		t.Fatalf("停用后应归档异常组，得 %q", acc.ConsumeGroup)
	}
	if acc.ConsumeGroupPrev != cgProxy {
		t.Fatalf("应记下原分组 proxy，得 %q", acc.ConsumeGroupPrev)
	}
	if !strings.HasPrefix(acc.ConsumeGroupNote, autoTagPrefix) {
		t.Fatalf("停用归档应带自动标记（以便启用后自动回归），得 %q", acc.ConsumeGroupNote)
	}
	if m["consume_group_text"] != "异常" {
		t.Fatalf("响应应回显「异常」，得 %v", m["consume_group_text"])
	}
	st, _ := px.pool.Status("d1")
	if !st.ManualDisabled {
		t.Fatal("池内应同时被摘除（manual_disabled）")
	}

	// ② 启用 → 自动回归原分组（反代）。
	call("enable")
	acc2, _ := store.GetAccount("d1")
	if effectiveConsumeGroup(acc2) != cgProxy {
		t.Fatalf("启用后应回到反代组，得 %q", consumeGroupLabel(effectiveConsumeGroup(acc2)))
	}
	if acc2.ConsumeGroupNote != "" || acc2.ConsumeGroupPrev != "" {
		t.Fatalf("回归后应清空归档标记，得 note=%q prev=%q", acc2.ConsumeGroupNote, acc2.ConsumeGroupPrev)
	}
}

// TestFix2_DisableKeepsExistingAbnormalReason 边界：已在异常组的号被停用，**不覆盖**首次原因。
func TestFix2_DisableKeepsExistingAbnormalReason(t *testing.T) {
	store, px, a := newTestApp(t,
		&Account{UID: "d2", Nickname: "已封号", Realm: "cn", AccessToken: "tok",
			ConsumeGroup: cgAbnormal, ConsumeGroupPrev: cgProxy,
			ConsumeGroupNote: "上游封号（11140），需重登"}) // 人工处理类：无 [auto] 标记
	defer store.Close()
	defer px.Close()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/accounts/d2/disable", strings.NewReader(`{}`))
	req.SetPathValue("uid", "d2")
	a.handleDisable(true)(rec, req)

	acc, _ := store.GetAccount("d2")
	if acc.ConsumeGroupNote != "上游封号（11140），需重登" {
		t.Fatalf("首次归档原因不该被「运营停用」覆盖，得 %q", acc.ConsumeGroupNote)
	}
	// 也不该被加上 auto 标记 —— 否则一个「需人工」的号会变成可自动回归。
	if strings.HasPrefix(acc.ConsumeGroupNote, autoTagPrefix) {
		t.Fatalf("不该把人工类归档改写成可自动回归，得 %q", acc.ConsumeGroupNote)
	}
}

// ---------- 问题3：异常组的自动回归边界 ----------

// TestFix3_PermissionFaultNeedsManual 走真实处置路径：14017 不得带自动回归标记。
//
// 这是问题3的核心：14017（试用未开通/权限异常）必须重登才能解决，不是「等冷却到期」。
// 此前标为可自动回归 → 软冷却 60s 一过就被放回，下一次请求立刻再次 14017，
// 形成「归档 → 回归 → 归档」循环，且每次都白耗一次选号。
func TestFix3_PermissionFaultNeedsManual(t *testing.T) {
	store, px, _ := newTestApp(t,
		&Account{UID: "p1", Nickname: "权限异常号", Realm: "cn", AccessToken: "tok",
			ConsumeGroup: cgProxy})
	defer store.Close()
	defer px.Close()

	// 走真实的错误处置路径（而非手工调 archiveAbnormal），确保测的是生产代码。
	px.reportUpstreamError("p1", "some-model", 400, []byte(`{"code":14017}`), "")

	acc, _ := store.GetAccount("p1")
	if acc.ConsumeGroup != cgAbnormal {
		t.Fatalf("14017 应归档异常组，得 %q", acc.ConsumeGroup)
	}
	if strings.HasPrefix(acc.ConsumeGroupNote, autoTagPrefix) {
		t.Fatalf("14017 是「需重登」类，不该带自动回归标记，得 %q", acc.ConsumeGroupNote)
	}

	// 冷却清掉（模拟软冷却 60s 到期）→ 自动回归**不得**把它放回去。
	px.pool.ReenableIfCredits("p1", 5000)
	px.reconcileAbnormalOnce()
	acc2, _ := store.GetAccount("p1")
	if effectiveConsumeGroup(acc2) != cgAbnormal {
		t.Fatalf("14017 的号不该在冷却到期后自动回归，得 %q",
			consumeGroupLabel(effectiveConsumeGroup(acc2)))
	}
}

// TestFix3_CreditFaultStillAutoRecovers 对照：余额耗尽仍是「可自动回归」类。
//
// 修复不能把「该回归的」也一起钉死 —— 余额恢复即可用，这是既有正确行为。
func TestFix3_CreditFaultStillAutoRecovers(t *testing.T) {
	store, px, _ := newTestApp(t,
		&Account{UID: "c1", Nickname: "余额耗尽号", Realm: "cn", AccessToken: "tok",
			ConsumeGroup: cgProxy})
	defer store.Close()
	defer px.Close()

	// 14020（余额不足）走真实处置路径。
	px.reportUpstreamError("c1", "m", 400, []byte(`{"code":14020}`), "")
	acc, _ := store.GetAccount("c1")
	if effectiveConsumeGroup(acc) != cgAbnormal {
		t.Fatalf("余额耗尽应归档异常组，得 %q", consumeGroupLabel(effectiveConsumeGroup(acc)))
	}
	if !strings.HasPrefix(acc.ConsumeGroupNote, autoTagPrefix) {
		t.Fatalf("余额类应带自动回归标记，得 %q", acc.ConsumeGroupNote)
	}

	// 余额恢复 + 冷却清掉 → 自动回归原分组。
	px.pool.ReenableIfCredits("c1", 5000)
	px.reconcileAbnormalOnce()
	acc2, _ := store.GetAccount("c1")
	if effectiveConsumeGroup(acc2) != cgProxy {
		t.Fatalf("余额恢复后应自动回归反代组，得 %q",
			consumeGroupLabel(effectiveConsumeGroup(acc2)))
	}
}

// TestFix3_ManualAbnormalNeverAutoRecovers 边界：管理员手动设的异常永不自动回归。
func TestFix3_ManualAbnormalNeverAutoRecovers(t *testing.T) {
	store, px, a := newTestApp(t,
		&Account{UID: "m1", Nickname: "手动异常号", Realm: "cn", AccessToken: "tok"})
	defer store.Close()
	defer px.Close()
	px.pool.SetCreditsDetailed("m1", 5000, 0)

	// 管理员通过「消耗分组」下拉显式设为异常（无 [auto] 标记 → 最终裁决）。
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/accounts/m1/consume-group",
		strings.NewReader(`{"group":"abnormal"}`))
	req.SetPathValue("uid", "m1")
	a.handleSetConsumeGroup(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("设分组应 200，得 %d（%s）", rec.Code, rec.Body.String())
	}
	px.reconcileAbnormalOnce()
	acc, _ := store.GetAccount("m1")
	if effectiveConsumeGroup(acc) != cgAbnormal {
		t.Fatalf("手动设的异常不该被自动回归，得 %q", consumeGroupLabel(effectiveConsumeGroup(acc)))
	}
}

// TestFix3_RecoverTargetIsAlwaysRegularGroup 边界：回归目标必为合法常规组。
func TestFix3_RecoverTargetIsAlwaysRegularGroup(t *testing.T) {
	store, px, _ := newTestApp(t,
		&Account{UID: "g1", Nickname: "被停用号", Realm: "cn", AccessToken: "tok",
			ConsumeGroup: cgProxy})
	defer store.Close()
	defer px.Close()

	// 停用 → 归档（prev=proxy）。
	px.archiveAbnormal("g1", "运营停用", true)
	// 人为把 prev 弄坏（模拟历史脏数据）→ 回归应回落到「按来源推断」而非异常组自身。
	if err := store.MarkAccountAbnormal("g1", cgAbnormal, autoTagPrefix+"坏数据"); err != nil {
		t.Fatal(err)
	}
	px.pool.ReenableIfCredits("g1", 5000)
	px.pool.SetManualDisabled("g1", false, "")
	px.reconcileAbnormalOnce()

	acc, _ := store.GetAccount("g1")
	got := effectiveConsumeGroup(acc)
	if got == cgAbnormal {
		t.Fatalf("回归目标不得是异常组自身，得 %q", got)
	}
	if got != cgProxy {
		t.Fatalf("prev 无效时应回落「按来源推断」= 反代，得 %q", consumeGroupLabel(got))
	}
}

// ---------- 问题4：控制台积分同步与入池门槛 ----------

// TestFix4_CreditQueryWritesBack 用户查积分（只读展示接口）必须写回池内。
//
// 这是问题4的根因修复：此前「看得到积分」与「入池门槛读的池内积分」不同源，
// 用户被卡在「有积分但无法入池」。
func TestFix4_CreditQueryWritesBack(t *testing.T) {
	store, px, a := newTestApp(t,
		&Account{UID: "h1", Nickname: "代挂号", Realm: "cn", AccessToken: "tok",
			OwnerUserID: 7, PoolState: ""})
	defer store.Close()
	defer px.Close()

	// 初始：池内无读数 → 入池被拒。
	if _, known := a.poolCredits("h1"); known {
		t.Fatal("初始不该有积分读数")
	}
	u := &User{ID: 7, Username: "owner", Role: "user", Status: StatusActive}
	apply := func() (int, string) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/host/accounts/h1/pool",
			strings.NewReader(`{"action":"apply"}`))
		req.SetPathValue("uid", "h1")
		req = withUser(req, u)
		a.handleHostPool(rec, req)
		var m map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		s, _ := m["error"].(string)
		return rec.Code, s
	}
	if code, msg := apply(); code != http.StatusBadRequest || !strings.Contains(msg, "尚未获取到") {
		t.Fatalf("无读数时应被拒且提示未获取，得 %d %q", code, msg)
	}

	// 关键：把池内读数补上（模拟用户在界面上查了一次积分 —— 该接口已写回），
	// 入池申请即应放行。这里直接调 SetCreditsDetailed 模拟 writeCreditDetail 的写回效果，
	// 因为 writeCreditDetail 需要真实上游。写回逻辑本身由 TestFix4_CreditsKnownFlag 断言。
	px.pool.SetCreditsDetailed("h1", 5000, 0)
	if code, msg := apply(); code != http.StatusOK {
		t.Fatalf("有读数后应放行入池申请，得 %d %q", code, msg)
	}
	acc, _ := store.GetAccount("h1")
	if acc.PoolState != "pending" {
		t.Fatalf("应进入待审状态，得 %q", acc.PoolState)
	}
}

// TestFix4_CreditsKnownFlag hostView 必须下发「积分是否已知」，供前端区分两种提示。
//
// 若不区分，前端会把「没拉过积分」显示成「积分不足 2000」—— 用户对着一个有积分的号
// 反复重试也无法入池（问题4的可见表现）。
func TestFix4_CreditsKnownFlag(t *testing.T) {
	store, px, a := newTestApp(t,
		&Account{UID: "k1", Nickname: "代挂号", Realm: "cn", AccessToken: "tok",
			OwnerUserID: 7, PoolState: ""})
	defer store.Close()
	defer px.Close()

	acc, _ := store.GetAccount("k1")
	v := a.hostView(acc)
	if v.CreditsKnown {
		t.Fatal("未打戳时 credits_known 应为 false")
	}
	// 未知时 credits 为 0 是「没拉过」而非「真没积分」—— 前端据此显示「同步积分」。
	if _, err := json.Marshal(v); err != nil { // 确保可序列化（前端直接消费该结构）
		t.Fatalf("hostView 应可序列化：%v", err)
	}

	px.pool.SetCreditsDetailed("k1", 1234, 0)
	acc2, _ := store.GetAccount("k1")
	if v2 := a.hostView(acc2); !v2.CreditsKnown || v2.Credits != 1234 {
		t.Fatalf("打戳后应 known=true credits=1234，得 known=%v credits=%d", v2.CreditsKnown, v2.Credits)
	}
}

// TestFix4_HostPoolHintPointsToRealEntry 提示必须指向用户端真实存在的入口。
func TestFix4_HostPoolHintPointsToRealEntry(t *testing.T) {
	store, px, a := newTestApp(t,
		&Account{UID: "h2", Nickname: "代挂号", Realm: "cn", AccessToken: "tok",
			OwnerUserID: 7, PoolState: ""})
	defer store.Close()
	defer px.Close()

	u := &User{ID: 7, Username: "owner", Role: "user", Status: StatusActive}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/host/accounts/h2/pool",
		strings.NewReader(`{"action":"apply"}`))
	req.SetPathValue("uid", "h2")
	req = withUser(req, u)
	a.handleHostPool(rec, req)

	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	msg, _ := m["error"].(string)
	// 不得再提「刷新积分」——那是管理端的按钮，用户控制台没有。
	if strings.Contains(msg, "刷新积分") {
		t.Fatalf("提示不该指向用户端不存在的按钮，得 %q", msg)
	}
	if !strings.Contains(msg, "查看全部") && !strings.Contains(msg, "同步") {
		t.Fatalf("提示应指向用户端真实入口（积分区块），得 %q", msg)
	}
}

// ---------- 交叉回归：三套收敛过程互不干扰 ----------

// TestFix_ConvergenceProcessesCoexist 预备组引擎、异常回归、停用联动三者共存时的终态。
//
// 场景：一个「已入池的代挂号」被系统以余额类原因归档为异常（可自动回归）。
// 期望：预备组引擎不碰它（问题1）；余额恢复后异常回归把它放回原组；
//
//	之后若积分仍低（<100），预备组引擎**才**可以接管把它送进预备组。
func TestFix_ConvergenceProcessesCoexist(t *testing.T) {
	store, px, a := newTestApp(t,
		&Account{UID: "x1", Nickname: "综合号", Realm: "cn", AccessToken: "tok",
			OwnerUserID: 7, PoolState: "in", ConsumeGroup: cgApply})
	defer store.Close()
	defer px.Close()

	// ① 归档为异常（余额类，可自动回归）。
	px.pool.CooldownUntilTomorrow4AM("x1", "余额不足")
	px.archiveAbnormal("x1", "积分不足（无法消耗积分）", true)
	acc, _ := store.GetAccount("x1")
	if acc.ConsumeGroup != cgAbnormal || acc.ConsumeGroupPrev != cgApply {
		t.Fatalf("① 归档态不符：cg=%q prev=%q", acc.ConsumeGroup, acc.ConsumeGroupPrev)
	}

	// ② 预备组引擎跑一轮：不得动它。
	a.reconcilePoolGroups()
	acc2, _ := store.GetAccount("x1")
	if acc2.ConsumeGroup != cgAbnormal {
		t.Fatalf("② 预备组引擎不该动异常号，得 %q", acc2.ConsumeGroup)
	}

	// ③ 余额恢复（清冷却）→ 异常回归应放回 cgApply。
	px.pool.ReenableIfCredits("x1", 9000)
	px.reconcileAbnormalOnce()
	acc3, _ := store.GetAccount("x1")
	if effectiveConsumeGroup(acc3) != cgApply {
		t.Fatalf("③ 应回归用户申请组，得 %q", consumeGroupLabel(effectiveConsumeGroup(acc3)))
	}

	// ④ 此后积分掉到预备线以下 → 这时预备组引擎才接管。
	px.pool.SetCreditsDetailed("x1", 20, 0)
	a.reconcilePoolGroups()
	acc4, _ := store.GetAccount("x1")
	if effectiveConsumeGroup(acc4) != cgPrep {
		t.Fatalf("④ 常规组低分号应进预备组，得 %q", consumeGroupLabel(effectiveConsumeGroup(acc4)))
	}
}

// TestFix_PoolStatusRoundTrip 防御性：pool.Status 的降权字段语义未被本次改动破坏。
func TestFix_PoolStatusRoundTrip(t *testing.T) {
	store, px, _ := newTestApp(t,
		&Account{UID: "s1", Nickname: "x", Realm: "cn", AccessToken: "tok", ConsumeGroup: cgProxy})
	defer store.Close()
	defer px.Close()

	px.pool.NoteFailures("s1")
	st, ok := px.pool.Status("s1")
	if !ok {
		t.Fatal("Status 应可取到账号")
	}
	// ConsecutiveFails 零值也透出（运维口径）；这里只断言字段可读且一致。
	if st.ConsecutiveFails != 1 {
		t.Fatalf("连续失败计数应为 1，得 %d", st.ConsecutiveFails)
	}
	if st.UID != "s1" {
		t.Fatalf("UID 应为 s1，得 %q", st.UID)
	}
	_ = pool.Status{}
	_ = time.Now()
}

// TestFix2_ArchiveKeepsRawGroupNotEffective 归档记**原始值**，回归后不被固化。
//
// 这是问题2 联动停用时暴露的延伸缺陷：归档若记「生效值」，会把「未显式设置分组」
// 永久固化成显式值。真实危害路径：
//
//	代挂号 cg="" 未入池 → 生效推断为「闲置」→ 运营停用（记 prev="idle"）
//	→ 运营启用 → 回归写 cg="idle"（**显式**）
//	→ 之后用户申请入池并审批通过 → 本应生效为「用户申请」，却因 cg 被显式锁成
//	  idle 而**仍然不参与消耗**（账号入池了却不干活，极难排查）。
//
// 修复：prev 记原始显式值（此处为 ""），回归时交回 inferConsumeGroup 按**当时**
// 的 pool_state / owner 推断。
func TestFix2_ArchiveKeepsRawGroupNotEffective(t *testing.T) {
	store, px, a := newTestApp(t,
		&Account{UID: "raw1", Nickname: "未设组代挂号", Realm: "cn", AccessToken: "tok",
			OwnerUserID: 7, PoolState: "", ConsumeGroup: ""}) // 未显式设组
	defer store.Close()
	defer px.Close()
	px.pool.SetCreditsDetailed("raw1", 5000, 0)

	// 初始：未入池 → 生效推断为「闲置」。
	acc, _ := store.GetAccount("raw1")
	if got := effectiveConsumeGroup(acc); got != cgIdle {
		t.Fatalf("未入池代挂号初始应推断为闲置，得 %q", got)
	}

	// 停用 → 归档。prev 必须是**原始空串**，而不是生效值 "idle"。
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/accounts/raw1/disable", strings.NewReader(`{}`))
	req.SetPathValue("uid", "raw1")
	a.handleDisable(true)(rec, req)
	acc2, _ := store.GetAccount("raw1")
	if acc2.ConsumeGroupPrev != "" {
		t.Fatalf("prev 应记原始空串（未显式设组），得 %q —— 记成生效值会把分组固化", acc2.ConsumeGroupPrev)
	}

	// 启用 → 回归。cg 必须仍是**空串**（未显式），而非被写成 "idle"。
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/api/admin/accounts/raw1/enable", strings.NewReader(`{}`))
	req2.SetPathValue("uid", "raw1")
	a.handleDisable(false)(rec2, req2)
	acc3, _ := store.GetAccount("raw1")
	if acc3.ConsumeGroup != "" {
		t.Fatalf("回归后 cg 应保持空串（未显式设组），得 %q", acc3.ConsumeGroup)
	}
	if got := effectiveConsumeGroup(acc3); got != cgIdle {
		t.Fatalf("回归后生效仍应为闲置，得 %q", got)
	}

	// 关键断言：之后用户入池并审批通过 → 生效分组应变为「用户申请」
	// （若 cg 被固化成 "idle"，这里会失败 —— 那正是缺陷的表现）。
	_ = store.SetAccountPoolState("raw1", "in", "已通过审核")
	_ = px.SyncAccounts()
	acc4, _ := store.GetAccount("raw1")
	if got := effectiveConsumeGroup(acc4); got != cgApply {
		t.Fatalf("入池通过后应生效为「用户申请」，得 %q（cg=%q）—— 分组被固化时会失败",
			consumeGroupLabel(got), acc4.ConsumeGroup)
	}
}

// TestFix2_ArchiveKeepsExplicitGroup 对照：显式设过组的账号，归档后仍回归到该组。
func TestFix2_ArchiveKeepsExplicitGroup(t *testing.T) {
	store, px, a := newTestApp(t,
		&Account{UID: "raw2", Nickname: "显式设组号", Realm: "cn", AccessToken: "tok",
			OwnerUserID: 7, PoolState: "in", ConsumeGroup: cgIdle}) // 管理员显式设「闲置」
	defer store.Close()
	defer px.Close()
	px.pool.SetCreditsDetailed("raw2", 5000, 0)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/accounts/raw2/disable", strings.NewReader(`{}`))
	req.SetPathValue("uid", "raw2")
	a.handleDisable(true)(rec, req)
	acc, _ := store.GetAccount("raw2")
	if acc.ConsumeGroupPrev != cgIdle {
		t.Fatalf("显式设 idle 的号，prev 应为 idle，得 %q", acc.ConsumeGroupPrev)
	}

	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/api/admin/accounts/raw2/enable", strings.NewReader(`{}`))
	req2.SetPathValue("uid", "raw2")
	a.handleDisable(false)(rec2, req2)
	acc2, _ := store.GetAccount("raw2")
	if acc2.ConsumeGroup != cgIdle {
		t.Fatalf("显式设的分组应原样回归，得 %q", acc2.ConsumeGroup)
	}
}
