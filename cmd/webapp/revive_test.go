// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/pool"
)

// 本文件锁死 v0.23.4 修复的「启用无法恢复系统禁用账号」缺陷。
//
// 现场：健康检测对**系统禁用**的号仍会探测，探测通过就报
// 「曾被系统禁用（12153 session dead）——本次探测通过，可考虑『复活』」。
// 运维去账号池页点「启用」，结果毫无变化：账号仍被判不可服务，下次检测还是同一句
// 提示 —— 反复点也没用。
//
// 根因：池内有**两个互相独立**的停用位，恢复时必须各清各的：
//   · e.disabled        系统自动禁用（12153 / 11140 判定）→ 只有 ReviveDisabled 能清
//   · e.manualDisabled  运维手动停用                        → 只有 SetManualDisabled 能清
//
// cmd/webapp 的 handleDisable(false) 只清了后者（外加库里的 disabled 列），
// **从未调用 ReviveDisabled** —— 旧网关 internal/server/admin.go 是有这一步的，
// 重写 webapp 时漏掉了（双服务并行重写的典型后果）。

// TestEnableClearsSystemDisabled **本文件最重要的用例**：
// 对「系统禁用」的号点启用，必须清掉池内系统禁用位，账号真正回到池子。
func TestEnableClearsSystemDisabled(t *testing.T) {
	store, px, a := newTestApp(t, &Account{UID: "u1", Realm: "cn", AccessToken: "at", RefreshToken: "rt"})
	defer store.Close()
	if err := px.SyncAccounts(); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}

	// 模拟系统禁用（12153 连续触发 → pool.Disable）。
	px.pool.Disable("u1", "12153 session dead")
	if st, ok := px.pool.Status("u1"); !ok || !st.Disabled {
		t.Fatalf("前置条件不成立：账号应处于系统禁用态，got %+v ok=%v", st, ok)
	}

	// 走启用端点（管理后台账号卡的「启用」按钮打的正是这个）。
	req := httptest.NewRequest("POST", "/api/admin/accounts/u1/enable", nil)
	req.SetPathValue("uid", "u1")
	rec := httptest.NewRecorder()
	a.handleDisable(false)(rec, req)
	if rec.Code != 200 {
		t.Fatalf("启用应返回 200，got %d body=%s", rec.Code, rec.Body.String())
	}

	// 核心断言：池内系统禁用位必须被清除，否则账号永远不可服务。
	st, ok := px.pool.Status("u1")
	if !ok {
		t.Fatal("账号不在池内")
	}
	if st.Disabled {
		t.Fatalf("启用后系统禁用位应被清除（本 bug 的核心）：disabled=true reason=%q", st.DisabledReason)
	}
	if st.DisabledReason != "" {
		t.Errorf("启用后禁用原因应一并清空（否则检测还会读到旧的 12153 文案）：%q", st.DisabledReason)
	}
}

// TestEnableClearsManualDisabled 对照用例：手动停用的号点启用应清掉手动停用位。
//
// 这条**修复前就应通过** —— 它保证修复没有把原本正确的路径改坏，
// 也说明「两个位各清各的」确实需要两条独立断言（只测其中一条会漏掉另一条）。
func TestEnableClearsManualDisabled(t *testing.T) {
	store, px, a := newTestApp(t, &Account{UID: "u1", Realm: "cn", AccessToken: "at", RefreshToken: "rt"})
	defer store.Close()
	if err := px.SyncAccounts(); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}

	px.pool.SetManualDisabled("u1", true, "web ui")
	if st, _ := px.pool.Status("u1"); !st.ManualDisabled {
		t.Fatal("前置条件：应处于手动停用态")
	}

	req := httptest.NewRequest("POST", "/api/admin/accounts/u1/enable", nil)
	req.SetPathValue("uid", "u1")
	rec := httptest.NewRecorder()
	a.handleDisable(false)(rec, req)
	if rec.Code != 200 {
		t.Fatalf("启用应返回 200，got %d", rec.Code)
	}
	st, _ := px.pool.Status("u1")
	if st.ManualDisabled {
		t.Fatalf("启用后手动停用位应被清除：%+v", st)
	}
}

// TestEnableWhenAlreadyEnabledIsIdempotent 对「两个位都已清」的号重复点启用，
// 不应报错、不应产生副作用（防「重复处理」：按钮连点/脚本重放）。
func TestEnableWhenAlreadyEnabledIsIdempotent(t *testing.T) {
	store, px, a := newTestApp(t, &Account{UID: "u1", Realm: "cn", AccessToken: "at", RefreshToken: "rt"})
	defer store.Close()
	_ = px.SyncAccounts()

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest("POST", "/api/admin/accounts/u1/enable", nil)
		req.SetPathValue("uid", "u1")
		rec := httptest.NewRecorder()
		a.handleDisable(false)(rec, req)
		if rec.Code != 200 {
			t.Fatalf("第 %d 次启用应幂等返回 200，got %d", i+1, rec.Code)
		}
	}
	st, _ := px.pool.Status("u1")
	if st.Disabled || st.ManualDisabled {
		t.Fatalf("重复启用后两个停用位都应为 false：%+v", st)
	}
}

// TestEnableOnBannedAccountDoesNotFakeRevive 封号（11140）类确定性异常**不**因为
// 点一次启用就"变成正常" —— 验证修复没有把「一键启用」做成万能橡皮擦。
//
// 语义边界：启用只清**停用标记**（谁判的、人工还是系统），不改变上游对该号的实际
// 判定。若账号确实仍被封，下一次健康检测会重新判为异常。这正是我们想要的：
// 「启用」是运维表态，不是伪造健康。
func TestEnableOnBannedAccountDoesNotFakeRevive(t *testing.T) {
	store, px, a := newTestApp(t, &Account{UID: "b1", Realm: "cn", AccessToken: "at", RefreshToken: "rt"})
	defer store.Close()
	_ = px.SyncAccounts()

	px.pool.Disable("b1", "account banned by upstream (11140), re-login required")

	req := httptest.NewRequest("POST", "/api/admin/accounts/b1/enable", nil)
	req.SetPathValue("uid", "b1")
	rec := httptest.NewRecorder()
	a.handleDisable(false)(rec, req)
	if rec.Code != 200 {
		t.Fatalf("got %d", rec.Code)
	}
	// 停用位被清（这是启用的职责）……
	st, _ := px.pool.Status("b1")
	if st.Disabled {
		t.Fatal("启用应清掉停用位")
	}
	// ……但池内不应出现任何"该号已被证明健康"的伪造标记：
	// 健康与否只能由下一次真实探测决定（检测/请求）。
	// 这里断言「清启用后不残留任何禁用原因」即可 —— 是否存在真实封禁由上游说了算。
	if st.DisabledReason != "" {
		t.Errorf("清启用后不应残留禁用原因：%q", st.DisabledReason)
	}
}

// TestHealthActionForStructuredActions 判定层给前端**结构化动作**，
// 而不是让前端去匹配中文原因文案（文案会改，匹配会静默失效）。
//
//	enable —— 运维手动停用（解除即可）
//	revive —— 曾被系统禁用但探测通过（清禁用位即可复活）
//	其余（含权限异常、请求非法等需要人工排查的）一律**不给**动作：一键按钮会误导。
func TestHealthActionForStructuredActions(t *testing.T) {
	disabled := pool.Status{Disabled: true, DisabledReason: "12153 session dead"}
	cases := []struct {
		name       string
		state      string
		reasonCode string
		st         pool.Status
		want       string
	}{
		{"手动停用 → enable", hcSkipped, hcCodeManualDisabled, pool.Status{ManualDisabled: true}, hcActionEnable},
		{"曾被系统禁用但探测通过 → revive", hcDegraded, hcCodeDegraded, disabled, hcActionRevive},
		{"仅冷却（非禁用）→ 不给动作", hcDegraded, hcCodeDegraded, pool.Status{Cooling: true}, ""},
		// v0.24.3：11140 的结论是 uncertain（request illegal，非封禁），本就不给动作。
		// 若它累计超阈值被禁用、之后又探活通过，会走上面那条（degraded + Disabled）→ revive。
		{"请求非法 11140（不给定性）→ 不给动作", hcUncertain, hcCodeRequestIllegal, pool.Status{}, ""},
		{"正常 → 不给动作", hcHealthy, hcCodeOK, pool.Status{}, ""},
		{"不确定 → 不给动作", hcUncertain, hcCodeWafBlock, pool.Status{}, ""},
	}
	for _, c := range cases {
		if got := hcActionFor(c.state, c.reasonCode, c.st); got != c.want {
			t.Errorf("%s: hcActionFor=%q want %q", c.name, got, c.want)
		}
	}
}

// ---------- 检测快照失效（v0.23.4 的第二半修复）----------

// newSnap 造一个含三条结果的快照：正常 / 系统禁用（可复活）/ 手动停用（可启用）。
func newSnap(t *testing.T) *healthSnapshot {
	t.Helper()
	s := &healthSnapshot{}
	s.set([]*HealthCheckResult{
		{UID: "ok1", State: hcHealthy, StateLabel: "正常"},
		{UID: "rev1", State: hcDegraded, StateLabel: "降权中", Action: hcActionRevive},
		{UID: "man1", State: hcSkipped, StateLabel: "已跳过", Action: hcActionEnable},
	}, time.Now().Add(-time.Minute), "manual")
	return s
}

// TestHealthSnapshotDropUIDs 剔除指定账号并同步重算计数。
//
// 这是「启用后检测状态能真正恢复」的关键一半：不清掉旧条目，页面上就永远写着
// 「已跳过 / 可复活」，运维会以为启用没生效。
func TestHealthSnapshotDropUIDs(t *testing.T) {
	s := newSnap(t)

	if res, sum, _ := s.get(); len(res) != 3 || sum.Total != 3 {
		t.Fatalf("前置：应有 3 条，got %d/%d", len(res), sum.Total)
	}

	if n := s.dropUIDs([]string{"man1"}); n != 1 {
		t.Fatalf("应剔除 1 条，got %d", n)
	}
	res, sum, ok := s.get()
	if !ok {
		t.Fatal("还有 2 条有效结果，hasResult 应为 true")
	}
	if len(res) != 2 || sum.Total != 2 {
		t.Fatalf("剔除后应剩 2 条，got %d/%d", len(res), sum.Total)
	}
	for _, r := range res {
		if r.UID == "man1" {
			t.Fatal("被剔除的条目不应还在")
		}
	}
	if sum.ByState[hcSkipped] != 0 {
		t.Fatalf("skipped 计数应同步归零，got %d", sum.ByState[hcSkipped])
	}
	if sum.ByState[hcHealthy] != 1 || sum.ByState[hcDegraded] != 1 {
		t.Fatalf("其余计数应保持不变：%+v", sum.ByState)
	}
	// 元信息保留（还是同一次检测）。
	if sum.Source != "manual" {
		t.Errorf("剔除非重测，Source 应保留：%q", sum.Source)
	}
	if sum.StartedAt == 0 {
		t.Error("StartedAt 应保留")
	}
}

// TestHealthSnapshotDropUIDsIdempotentAndSafe 重复剔除 / 不存在的 uid / 空输入都安全。
func TestHealthSnapshotDropUIDsIdempotentAndSafe(t *testing.T) {
	s := newSnap(t)

	if n := s.dropUIDs([]string{"nope"}); n != 0 {
		t.Errorf("不存在的 uid 应剔除 0 条，got %d", n)
	}
	if n := s.dropUIDs(nil); n != 0 {
		t.Errorf("空输入应剔除 0 条，got %d", n)
	}
	if n := s.dropUIDs([]string{"", "  "}); n != 0 {
		t.Errorf("空白 uid 应剔除 0 条，got %d", n)
	}
	// 重复剔除同一个：第二次应为 0（已不在），且不影响其余。
	if n := s.dropUIDs([]string{"rev1"}); n != 1 {
		t.Fatalf("首次剔除应为 1，got %d", n)
	}
	if n := s.dropUIDs([]string{"rev1"}); n != 0 {
		t.Fatalf("重复剔除应为 0（幂等），got %d", n)
	}
	if res, sum, _ := s.get(); len(res) != 2 || sum.Total != 2 {
		t.Fatalf("应有 2 条，got %d/%d", len(res), sum.Total)
	}
}

// TestHealthSnapshotDropUIDsAllLeadsToNoResult 全部剔除后回到「尚无结果」，
// 而不是留下一个「总数为 0 的空结果」—— 后者会让前端显示「共 0 个」这种怪状态。
func TestHealthSnapshotDropUIDsAllLeadsToNoResult(t *testing.T) {
	s := newSnap(t)
	if n := s.dropUIDs([]string{"ok1", "rev1", "man1"}); n != 3 {
		t.Fatalf("应剔除全部 3 条，got %d", n)
	}
	if _, _, ok := s.get(); ok {
		t.Fatal("全部剔除后 hasResult 应为 false")
	}
}

// TestEnableInvalidatesHealthSnapshot 端到端：启用一个被系统禁用的号之后，
// 它在「最近一次检测」里的旧条目必须消失（否则页面继续显示「可复活」）。
func TestEnableInvalidatesHealthSnapshot(t *testing.T) {
	store, px, a := newTestApp(t, &Account{UID: "u1", Realm: "cn", AccessToken: "at", RefreshToken: "rt"})
	defer store.Close()
	_ = px.SyncAccounts()

	// 预置一份快照，其中 u1 是「曾被系统禁用、可复活」。
	healthLast.set([]*HealthCheckResult{
		{UID: "u1", State: hcDegraded, StateLabel: "降权中", Action: hcActionRevive},
		{UID: "u2", State: hcHealthy, StateLabel: "正常"},
	}, time.Now(), "manual")
	// 让 u1 真的处于系统禁用态（与快照一致）。
	px.pool.Disable("u1", "12153 session dead")
	t.Cleanup(func() {
		healthLast.set(nil, time.Now(), "")
		healthLast.hasResult = false
	})

	req := httptest.NewRequest("POST", "/api/admin/accounts/u1/enable", nil)
	req.SetPathValue("uid", "u1")
	rec := httptest.NewRecorder()
	a.handleDisable(false)(rec, req)
	if rec.Code != 200 {
		t.Fatalf("got %d", rec.Code)
	}

	res, sum, ok := healthLast.get()
	if !ok || len(res) != 1 || sum.Total != 1 {
		t.Fatalf("u1 的旧结论应被剔除，只剩 u2：got res=%d total=%d ok=%v", len(res), sum.Total, ok)
	}
	if res[0].UID != "u2" {
		t.Fatalf("剩下的应是 u2，got %q", res[0].UID)
	}
	// 响应里要能把这件事告诉前端（用于提示「旧结论已失效，建议重新检测」）。
	if !strings.Contains(rec.Body.String(), "health_result_invalidated") {
		t.Errorf("响应应包含 health_result_invalidated：%s", rec.Body.String())
	}
}
