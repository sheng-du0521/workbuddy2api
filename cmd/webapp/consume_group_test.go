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

	"workbuddy2api/internal/pool"
)

// ---------------------------------------------------------------------
// 消耗分组（v0.13.0）：推断、选号优先级、异常归档与回归、管理接口
// ---------------------------------------------------------------------

// TestInferConsumeGroup 推断规则：
// 代挂 + 已入池 → 用户申请；代挂未入池 → 闲置；平台自持 / 后台与桌面导入 → 反代。
func TestInferConsumeGroup(t *testing.T) {
	cases := []struct {
		name string
		acc  *Account
		want string
	}{
		{"平台自持→反代", &Account{UID: "p", OwnerUserID: 0}, cgProxy},
		{"桌面导入(owner=0)→反代", &Account{UID: "d", OwnerUserID: 0, PoolState: ""}, cgProxy},
		{"代挂已入池→用户申请", &Account{UID: "a", OwnerUserID: 7, PoolState: "in"}, cgApply},
		{"代挂待审→闲置", &Account{UID: "b", OwnerUserID: 7, PoolState: "pending"}, cgIdle},
		{"代挂未申请→闲置", &Account{UID: "c", OwnerUserID: 7}, cgIdle},
		{"代挂已驳回→闲置", &Account{UID: "r", OwnerUserID: 7, PoolState: "rejected"}, cgIdle},
		{"显式值优先(平台)", &Account{UID: "e", OwnerUserID: 0, ConsumeGroup: cgIdle}, cgIdle},
		{"显式值优先(代挂入池)", &Account{UID: "f", OwnerUserID: 7, PoolState: "in", ConsumeGroup: cgAbnormal}, cgAbnormal},
		{"非法显式值回落推断", &Account{UID: "g", OwnerUserID: 0, ConsumeGroup: "bogus"}, cgProxy},
		{"大小写不敏感", &Account{UID: "h", OwnerUserID: 0, ConsumeGroup: "IDLE"}, cgIdle},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := effectiveConsumeGroup(c.acc); got != c.want {
				t.Fatalf("want %q got %q", c.want, got)
			}
		})
	}
	if effectiveConsumeGroup(nil) != cgIdle {
		t.Fatal("nil 账号应视为闲置")
	}
	if consumeGroupParticipates(cgIdle) || consumeGroupParticipates(cgAbnormal) {
		t.Fatal("闲置 / 异常不得参与消耗")
	}
	if !consumeGroupParticipates(cgApply) || !consumeGroupParticipates(cgProxy) {
		t.Fatal("用户申请 / 反代应参与消耗")
	}
}

// TestConsumeGroupPriorityAndNoFallback 平台选号只认「用户申请 / 反代」两组，
// 且按优先级逐组取号；只剩闲置 / 异常时不选任何账号（不设兜底）。
func TestConsumeGroupPriorityAndNoFallback(t *testing.T) {
	store, px, _ := newTestApp(t,
		&Account{UID: "ap", Realm: "cn", AccessToken: "a", OwnerUserID: 7, PoolState: "in"}, // 用户申请
		&Account{UID: "pl", Realm: "cn", AccessToken: "a"},                                  // 反代（平台自持）
		&Account{UID: "idle", Realm: "cn", AccessToken: "a", OwnerUserID: 7},                // 闲置
	)
	defer store.Close()
	_ = px.SyncAccounts()

	if got := px.ConsumeGroupForUID("ap"); got != cgApply {
		t.Fatalf("ap 应为用户申请组，得 %q", got)
	}
	if got := px.ConsumeGroupForUID("pl"); got != cgProxy {
		t.Fatalf("pl 应为反代组，得 %q", got)
	}
	if got := px.ConsumeGroupForUID("idle"); got != cgIdle {
		t.Fatalf("idle 应为闲置组，得 %q", got)
	}

	// 用户申请组可用 → 始终优先命中，反代组退居次级。
	for i := 0; i < 20; i++ {
		got := px.pickExcluding(map[string]bool{}, "m", pickScope{})
		if got == nil || got.UID != "ap" {
			t.Fatalf("第 %d 次应命中「用户申请」组 ap，得 %v", i, got)
		}
	}

	// 停用优先组 → 落到「反代」组。
	if err := store.SetDisabled("ap", true); err != nil {
		t.Fatalf("SetDisabled: %v", err)
	}
	_ = px.SyncAccounts()
	if got := px.pickExcluding(map[string]bool{}, "m", pickScope{}); got == nil || got.UID != "pl" {
		t.Fatalf("应落到「反代」组 pl，得 %v", got)
	}

	// 反代组也停用 → 闲置组必须不被兜底选中。
	if err := store.SetDisabled("pl", true); err != nil {
		t.Fatalf("SetDisabled: %v", err)
	}
	_ = px.SyncAccounts()
	if got := px.pickExcluding(map[string]bool{}, "m", pickScope{}); got != nil {
		t.Fatalf("闲置组不得兜底，应返回 nil，得 %v", got)
	}
}

// TestConsumeGroupHostSelfUnaffected 代挂自用范围不受消耗分组限制
// （号主用自己的 Key 打自己的号，不该被管理员的分组设置挡住）。
func TestConsumeGroupHostSelfUnaffected(t *testing.T) {
	store, px, _ := newTestApp(t,
		&Account{UID: "h1", Realm: "cn", AccessToken: "a", OwnerUserID: 7}, // 闲置组（未入池）
	)
	defer store.Close()
	_ = px.SyncAccounts()

	// 平台范围：闲置 → 不选。
	if got := px.pickExcluding(map[string]bool{}, "m", pickScope{}); got != nil {
		t.Fatalf("平台范围不应选中闲置账号，得 %v", got)
	}
	// 代挂自用范围：仍可用。
	got := px.pickExcluding(map[string]bool{}, "m", pickScope{HostUser: 7})
	if got == nil || got.UID != "h1" {
		t.Fatalf("代挂自用应能选中本人的闲置组账号，得 %v", got)
	}
}

// TestArchiveAbnormalOnUpstreamErrors 上诉两类「一次即归档」的确定性异常。
// 自动回归标记（wantAuto）按**恢复条件**区分：
//
//	余额类（14020 / 402）→ true   余额恢复即可用，冷却到期自动回归
//	运营停用            → true   解除停用即可用（见 handleDisable）
//	账号故障 14017      → false  需重登激活（**v0.23.1 从 true 修正为 false**）
//
// 14017 此前标 true 是个 bug：软冷却 60s 一到就被回归，下次请求立刻再次 14017，
// 形成「归档 ↔ 回归」循环且每次白耗一次选号（详见 docs/线上问题分析与修复-v0.23.1.md）。
//
// **哪些不在这张表里**（都不是"一次即归档"，而是走累计阈值）：
//
//	· 11140（v0.24.3）→ TestArchiveAbnormalOn11140NeedsThreshold
//	· 12153（v0.24.7）→ TestArchiveAbnormalOn12153NeedsThreshold
//
// 这两条同属一类错误：**"这次请求"被上游拒 ≠ "这个账号"坏了**，一次即摘会误杀。
func TestArchiveAbnormalOnUpstreamErrors(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		wantAuto bool
	}{
		{"积分不足 14020", 0, `{"code":14020}`, true},
		{"积分不足 402", 402, `{}`, true},
		// v0.23.1：14017 = 试用未开通 / 权限异常 → 必须重登，不是「等冷却」。
		{"账号故障 14017", 0, `{"code":14017}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store, px, _ := newTestApp(t, &Account{UID: "u1", Realm: "cn", AccessToken: "a"})
			defer store.Close()
			_ = px.SyncAccounts()
			if got := px.ConsumeGroupForUID("u1"); got != cgProxy {
				t.Fatalf("初始应为反代组，得 %q", got)
			}

			px.reportUpstreamError("u1", "m", c.status, []byte(c.body), "")

			acc, err := store.GetAccount("u1")
			if err != nil || acc == nil {
				t.Fatalf("GetAccount: %v %v", acc, err)
			}
			if got := effectiveConsumeGroup(acc); got != cgAbnormal {
				t.Fatalf("应归档到异常组，得 %q（note=%q）", got, acc.ConsumeGroupNote)
			}
			// prev 记**原始显式值**（v0.23.1 修正）：本用例的账号未显式设组（cg=""),
			// 生效分组是推断出来的「反代」。记原始空串 → 回归时按来源重新推断，
			// 避免把「未显式设置」永久固化成显式值（详见 archiveAbnormal 的注释）。
			if acc.ConsumeGroupPrev != "" {
				t.Fatalf("prev 应记原始显式值（本用例未设组 → 空串），得 %q", acc.ConsumeGroupPrev)
			}
			if got := effectiveConsumeGroup(acc); got != cgAbnormal {
				t.Fatalf("归档后生效分组应为异常，得 %q", got)
			}
			if isAutoArchived(acc) != c.wantAuto {
				t.Fatalf("自动回归标记应为 %v，得 %v（note=%q）", c.wantAuto, isAutoArchived(acc), acc.ConsumeGroupNote)
			}
			// 内存映射同步 → 选号立即不再考虑它。
			if got := px.ConsumeGroupForUID("u1"); got != cgAbnormal {
				t.Fatalf("内存分组应同步为异常，得 %q", got)
			}
			if got := px.pickExcluding(map[string]bool{}, "m", pickScope{}); got != nil {
				t.Fatalf("已归档异常的账号不应再被选中，得 %v", got)
			}
		})
	}
}

// TestArchiveAbnormalOn11140NeedsThreshold 11140「请求非法」走**阈值累计**：
// 前 N-1 次只软冷却（可逆、不归档），第 N 次才归档到异常组。
//
// v0.24.3（2026-10-13 封号事故）：旧行为是「一次即 Disable + 归档」，于是在**一个风控
// 窗口内批量误杀 30 个健康账号**（抽样 7 个重探全部通过、凭证与余额完好）。
// 11140 的真实语义是 request illegal（这次请求不合法），由出口 IP / 频率 / 头细节触发，
// **不是账号封禁** —— 见 docs/封号事件诊断与账号隔离核对-20261013.md。
func TestArchiveAbnormalOn11140NeedsThreshold(t *testing.T) {
	store, px, _ := newTestApp(t, &Account{UID: "u1", Realm: "cn", AccessToken: "a"})
	defer store.Close()
	_ = px.SyncAccounts()
	if got := px.ConsumeGroupForUID("u1"); got != cgProxy {
		t.Fatalf("初始应为反代组，得 %q", got)
	}

	threshold := pool.AccountFaultThreshold()
	for i := 1; i < threshold; i++ {
		px.reportUpstreamError("u1", "m", 0, []byte(`{"code":11140}`), "")
		acc, err := store.GetAccount("u1")
		if err != nil || acc == nil {
			t.Fatalf("GetAccount: %v %v", acc, err)
		}
		if got := effectiveConsumeGroup(acc); got == cgAbnormal {
			t.Fatalf("第 %d 次不该归档（阈值 %d）—— 一次即归档正是本次修复的缺陷", i, threshold)
		}
		st, _ := px.pool.Status("u1")
		if !st.Cooling {
			t.Fatalf("第 %d 次应软冷却（可逆、自动到期），得 %+v", i, st)
		}
		if st.Disabled {
			t.Fatalf("第 %d 次不该禁用", i)
		}
	}

	px.reportUpstreamError("u1", "m", 0, []byte(`{"code":11140}`), "")
	acc, _ := store.GetAccount("u1")
	if got := effectiveConsumeGroup(acc); got != cgAbnormal {
		t.Fatalf("第 %d 次应归档到异常组，得 %q（note=%q）", threshold, got, acc.ConsumeGroupNote)
	}
	if st, _ := px.pool.Status("u1"); !st.Disabled {
		t.Fatal("超阈值后应禁用")
	}
	// 内存映射同步 → 选号立即不再考虑它。
	if got := px.ConsumeGroupForUID("u1"); got != cgAbnormal {
		t.Fatalf("内存分组应同步为异常，得 %q", got)
	}
}

// TestArchiveAbnormalOn12153NeedsThreshold 12153（会话失效）也**必须累计才归档**
// （v0.24.7 与 11140 统一纪律）。
//
// 在此之前，生产转发链的 12153 是一次即 Disable + 归档，而同项目的健康检测侧早已
// 写成累计阈值并注明「一次 12153 可能是网络抖动 / refresh 竞态，一次就杀号会误杀」——
// 同一个错误码两条链路两套纪律，正是 2026-10-13 那类误杀的成因类别。
func TestArchiveAbnormalOn12153NeedsThreshold(t *testing.T) {
	store, px, _ := newTestApp(t, &Account{UID: "u1", Realm: "cn", AccessToken: "a"})
	defer store.Close()
	_ = px.SyncAccounts()

	body := []byte(`{"code":12153}`)
	// 前两次：只软冷却，**不归档**（保持反代组）。
	for i := 0; i < 2; i++ {
		px.reportUpstreamError("u1", "m", 0, body, "")
		acc, _ := store.GetAccount("u1")
		if got := effectiveConsumeGroup(acc); got == cgAbnormal {
			t.Fatalf("第 %d 次不该归档（阈值 %d），note=%q", i+1, pool.SessionDeadThreshold(), acc.ConsumeGroupNote)
		}
	}
	// 推到阈值：这时才归档。
	for i := 0; i <= pool.SessionDeadThreshold(); i++ {
		if st, _ := px.pool.Status("u1"); st.Disabled {
			break
		}
		px.reportUpstreamError("u1", "m", 0, body, "")
	}
	acc, _ := store.GetAccount("u1")
	if got := effectiveConsumeGroup(acc); got != cgAbnormal {
		t.Fatalf("累计达阈值应归档到异常组，得 %q（note=%q）", got, acc.ConsumeGroupNote)
	}
	if isAutoArchived(acc) {
		t.Fatal("12153 需重登，不该带自动回归标记")
	}
}

// TestArchiveAbnormalKeepsFirst 已归档账号再次报错不覆盖首次原因（保留原始 prev）。
func TestArchiveAbnormalKeepsFirst(t *testing.T) {
	store, px, _ := newTestApp(t, &Account{UID: "u1", Realm: "cn", AccessToken: "a"})
	defer store.Close()
	_ = px.SyncAccounts()

	px.reportUpstreamError("u1", "m", 402, []byte(`{}`), "")
	first, _ := store.GetAccount("u1")
	// 第二枪必须挑一个**仍是一次即归档**且**原因文案不同**的错误：
	// 14017（账号故障）满足这两点。12153 自 v0.24.7 起、11140 自 v0.24.3 起都不再
	// 一次即归档（改累计阈值），故都不适合做本用例的第二枪。
	px.reportUpstreamError("u1", "m", 0, []byte(`{"code":14017}`), "")
	second, _ := store.GetAccount("u1")

	if second.ConsumeGroupNote != first.ConsumeGroupNote {
		t.Fatalf("重复归档不应改写原因：first=%q second=%q", first.ConsumeGroupNote, second.ConsumeGroupNote)
	}
}

// TestReconcileAbnormalRestores 自动归档（积分不足类）在冷却解除后回归原分组；
// 仍在冷却中或由管理员手动设为「异常」的账号不会被自动放回。
func TestReconcileAbnormalRestores(t *testing.T) {
	store, px, _ := newTestApp(t,
		&Account{UID: "auto", Realm: "cn", AccessToken: "a"},   // 平台自持 → 反代组
		&Account{UID: "manual", Realm: "cn", AccessToken: "a"}, // 手动设为异常
		&Account{UID: "cooling", Realm: "cn", AccessToken: "a"},
	)
	defer store.Close()
	_ = px.SyncAccounts()

	// cooling：真实走一次积分不足 → 带自动标记且处于「冷却到次日 4 点」。
	px.reportUpstreamError("cooling", "m", 402, []byte(`{}`), "")
	// auto：构造「已自动归档但池中已恢复」的状态。
	if err := store.MarkAccountAbnormal("auto", cgProxy, autoTagPrefix+"积分不足（无法消耗积分）"); err != nil {
		t.Fatalf("MarkAccountAbnormal: %v", err)
	}
	// manual：管理员手动设异常（无标记）。
	if err := store.SetAccountConsumeGroup("manual", cgAbnormal); err != nil {
		t.Fatalf("SetAccountConsumeGroup: %v", err)
	}
	_ = px.SyncAccounts()

	px.reconcileAbnormalOnce()

	if acc, _ := store.GetAccount("auto"); effectiveConsumeGroup(acc) != cgProxy {
		t.Fatalf("已恢复的自动归档账号应回归反代组，得 %q", effectiveConsumeGroup(acc))
	} else if acc.ConsumeGroupNote != "" || acc.ConsumeGroupPrev != "" {
		t.Fatalf("回归后应清空归档标记，得 note=%q prev=%q", acc.ConsumeGroupNote, acc.ConsumeGroupPrev)
	}
	if acc, _ := store.GetAccount("manual"); effectiveConsumeGroup(acc) != cgAbnormal {
		t.Fatal("管理员手动设为异常的账号不应被自动回归")
	}
	if acc, _ := store.GetAccount("cooling"); effectiveConsumeGroup(acc) != cgAbnormal {
		t.Fatal("仍在冷却中的异常账号不应被自动回归")
	}

	// 回归后内存映射同步 → 该账号重新参与选号。
	if got := px.ConsumeGroupForUID("auto"); got != cgProxy {
		t.Fatalf("回归后内存分组应为反代，得 %q", got)
	}
}

// TestSetConsumeGroupHandler 单账号接口：合法值生效、空串回到推断、非法值 400。
func TestSetConsumeGroupHandler(t *testing.T) {
	store, px, ap := newTestApp(t, &Account{UID: "u1", Realm: "cn", AccessToken: "a"})
	defer store.Close()
	_ = px.SyncAccounts()

	call := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/admin/accounts/u1/consume-group", strings.NewReader(body))
		req.SetPathValue("uid", "u1")
		w := httptest.NewRecorder()
		ap.handleSetConsumeGroup(w, req)
		return w
	}

	if w := call(`{"group":"idle"}`); w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if acc, _ := store.GetAccount("u1"); effectiveConsumeGroup(acc) != cgIdle {
		t.Fatalf("显式设为闲置未生效，得 %q", effectiveConsumeGroup(acc))
	}
	if px.ConsumeGroupForUID("u1") != cgIdle {
		t.Fatal("内存映射未同步")
	}

	// 空串 = 回到「按来源推断」（平台自持 → 反代）。
	if w := call(`{"group":""}`); w.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
	if acc, _ := store.GetAccount("u1"); acc.ConsumeGroup != "" || effectiveConsumeGroup(acc) != cgProxy {
		t.Fatalf("空串应回到推断组反代，得显式=%q 生效=%q", acc.ConsumeGroup, effectiveConsumeGroup(acc))
	}

	if w := call(`{"group":"bogus"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("非法分组应 400，得 %d", w.Code)
	}
}

// TestBatchConsumeGroupHandler 批量接口：按 uid 列表 / all=true 两种口径。
func TestBatchConsumeGroupHandler(t *testing.T) {
	store, px, ap := newTestApp(t,
		&Account{UID: "u1", Realm: "cn", AccessToken: "a"},
		&Account{UID: "u2", Realm: "cn", AccessToken: "a"},
		&Account{UID: "u3", Realm: "cn", AccessToken: "a"},
	)
	defer store.Close()
	_ = px.SyncAccounts()

	call := func(body string) (int, map[string]any) {
		req := httptest.NewRequest(http.MethodPost, "/api/admin/accounts/consume-group/batch", strings.NewReader(body))
		w := httptest.NewRecorder()
		ap.handleBatchConsumeGroup(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}

	code, out := call(`{"uids":["u1","u2"],"group":"apply"}`)
	if code != http.StatusOK || out["updated"].(float64) != 2 {
		t.Fatalf("批量设置应更新 2 个，得 code=%d out=%v", code, out)
	}
	for _, uid := range []string{"u1", "u2"} {
		if acc, _ := store.GetAccount(uid); effectiveConsumeGroup(acc) != cgApply {
			t.Fatalf("%s 应为用户申请组", uid)
		}
	}
	if acc, _ := store.GetAccount("u3"); effectiveConsumeGroup(acc) != cgProxy {
		t.Fatal("未选中的 u3 不应被改动")
	}

	// all=true：作用于全部账号。
	code, out = call(`{"all":true,"group":"idle"}`)
	if code != http.StatusOK || out["updated"].(float64) != 3 {
		t.Fatalf("all=true 应更新 3 个，得 code=%d out=%v", code, out)
	}
	for _, uid := range []string{"u1", "u2", "u3"} {
		if acc, _ := store.GetAccount(uid); effectiveConsumeGroup(acc) != cgIdle {
			t.Fatalf("%s 应为闲置组", uid)
		}
	}
	// 全池闲置后平台范围应无号可选。
	if got := px.pickExcluding(map[string]bool{}, "m", pickScope{}); got != nil {
		t.Fatalf("全池闲置时不应有账号可消耗，得 %v", got)
	}

	// 未选账号 / 非法分组 → 400。
	if code, _ := call(`{"uids":[],"group":"apply"}`); code != http.StatusBadRequest {
		t.Fatalf("空 uid 列表应 400，得 %d", code)
	}
	if code, _ := call(`{"all":true,"group":"bogus"}`); code != http.StatusBadRequest {
		t.Fatalf("非法分组应 400，得 %d", code)
	}
}

// TestConsumeGroupPersistsAcrossUpsert 重新 OAuth 写入（UpsertAccount）不得冲掉
// 管理员设定的消耗分组与系统归档标记（否则一次刷新 token 就绕过异常归档）。
func TestConsumeGroupPersistsAcrossUpsert(t *testing.T) {
	store, _, _ := newTestApp(t, &Account{UID: "u1", Realm: "cn", AccessToken: "a"})
	defer store.Close()

	if err := store.MarkAccountAbnormal("u1", cgProxy, autoTagPrefix+"积分不足"); err != nil {
		t.Fatalf("MarkAccountAbnormal: %v", err)
	}
	if err := store.UpsertAccount(&Account{UID: "u1", Realm: "cn", AccessToken: "new-token", Nickname: "n"}); err != nil {
		t.Fatalf("re-Upsert: %v", err)
	}
	acc, err := store.GetAccount("u1")
	if err != nil || acc == nil {
		t.Fatalf("GetAccount: %v %v", acc, err)
	}
	if acc.ConsumeGroup != cgAbnormal || acc.ConsumeGroupPrev != cgProxy {
		t.Fatalf("消耗分组被重登冲掉：group=%q prev=%q", acc.ConsumeGroup, acc.ConsumeGroupPrev)
	}
	if acc.AccessToken != "new-token" {
		t.Fatal("凭证应被刷新（只有业务状态列受保护）")
	}
}
