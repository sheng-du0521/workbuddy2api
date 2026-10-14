// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"testing"

	"time"

	"workbuddy2api/internal/upstream"
)

// ===========================================================================
// v0.24.6：账号级风控限制（上游明说「账号存在异常行为，已被限制使用」）
//   → 即时上报 + **立即删除**，不等累计 3 次。
//
// 关键区分：裸 11140 是可恢复抖动（走累计）；带这句 displayMsg 的 11140 是
// 上游对**账号本身**的判定（走即时）。两者的判定必须分得开 —— 分不开就会出现
// 「要么误杀一批号，要么等 3 轮白等」。
// ===========================================================================

const bodyRestricted11140 = `{"code":11140,"msg":"request illegal","requestId":"6776962f9a209c70b64166894412681a",` +
	`"displayMsg":{"en":"Your account has been restricted due to unusual activity","zh":"账号存在异常行为，已被限制使用"}}`

// TestVerdictRestrictedBeatsRequestIllegal 同样的 11140：带账号级限制文案 → 判为
// account_restricted（不可逆路径）；裸 11140 → 仍是 request_illegal（可逆路径）。
func TestVerdictRestrictedBeatsRequestIllegal(t *testing.T) {
	restricted := hcVerdictFromErr(&upstream.Error{
		Kind: upstream.ErrAccountFault, Status: 403,
		Msg:     `code=11140 msg=request illegal`,
		Display: "账号存在异常行为，已被限制使用",
	})
	if restricted.ReasonCode != hcCodeRestricted || !restricted.Definite {
		t.Fatalf("带账号级限制文案应判为 %s 且 Definite，得 code=%s definite=%v",
			hcCodeRestricted, restricted.ReasonCode, restricted.Definite)
	}
	if !containsStr([]string{restricted.Reason}, restricted.Reason) || restricted.Reason == "" {
		t.Fatal("原因文案不该为空")
	}

	bare := hcVerdictFromErr(&upstream.Error{
		Kind: upstream.ErrAccountFault, Status: 403, Msg: `code=11140 msg=request illegal`,
	})
	if bare.ReasonCode != hcCodeRequestIllegal {
		t.Fatalf("裸 11140 应仍判为 %s（可逆路径），得 %s", hcCodeRequestIllegal, bare.ReasonCode)
	}
	if bare.Definite {
		t.Fatal("裸 11140 不该是 Definite（那会带来不可逆处置）")
	}
}

// TestRestrictedImmediateDelete 账号级限制：**一次**即删（不等 3 次），并留下上报记录。
func TestRestrictedImmediateDelete(t *testing.T) {
	store, px, a := newTestApp(t, &Account{UID: "r1", Nickname: "+85200000001",
		AccessToken: "tok", ConsumeGroup: cgProxy})
	defer store.Close()
	defer px.Close()

	cause := &upstream.Error{Kind: upstream.ErrAccountFault, Status: 403,
		Msg: "code=11140 msg=request illegal", Display: "账号存在异常行为，已被限制使用"}

	acts := a.applyHealthDisposition("r1", hcCodeRestricted, cause, true)
	if !containsStr(acts, hcActDeleted) {
		t.Fatalf("账号级限制应即时删除，动作=%v", acts)
	}
	// 一次就删掉 —— 不是累计到 3 次才动。
	if acc, _ := store.GetAccount("r1"); acc != nil {
		t.Fatal("DB 行应已删除")
	}
	if _, inPool := px.pool.Status("r1"); inPool {
		t.Fatal("应从池内移除")
	}
	// 上报记录必须留下（含上游原文，便于事后核对）。
	reps, err := store.ListRiskReports(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(reps) == 0 {
		t.Fatal("应留下一条风控上报")
	}
	r := reps[0]
	if r.UID != "r1" || r.Source != "health" || r.Action != "deleted" {
		t.Fatalf("上报内容不符：%+v", r)
	}
	if !upstream.IsAccountRestricted(r.Display) {
		t.Fatalf("上报应保存上游原文，得 %q", r.Display)
	}
}

// TestRestrictedIdempotent 并发重复触发只处置一次（多个在途请求同时拿到该文案）。
func TestRestrictedIdempotent(t *testing.T) {
	store, px, a := newTestApp(t, &Account{UID: "r2", AccessToken: "tok"})
	defer store.Close()
	defer px.Close()

	a.handleRestrictedAccount("r2", "proxy", "账号存在异常行为，已被限制使用")
	// 第二次：账号已不在 → 不该重复上报、也不该报错。
	if acts := a.handleRestrictedAccount("r2", "proxy", "账号存在异常行为，已被限制使用"); len(acts) != 0 {
		t.Fatalf("重复触发不该产生动作，得 %v", acts)
	}
	reps, _ := store.ListRiskReports(10)
	if len(reps) != 1 {
		t.Fatalf("应只有 1 条上报，得 %d 条", len(reps))
	}
}

// TestRestrictedViaProxyPath 生产转发链同样即时处置（走 onAccountRestricted 回调）。
func TestRestrictedViaProxyPath(t *testing.T) {
	store, px, a := newTestApp(t, &Account{UID: "r3", AccessToken: "tok"})
	defer store.Close()
	defer px.Close()
	px.onAccountRestricted = a.handleRestrictedAccount

	// 真实转发路径上报上游错误 → 应触发即时删除。
	px.reportUpstreamError("r3", "m", 403, []byte(bodyRestricted11140), time.Time{}, upstream.ResetNone, nil)
	if acc, _ := store.GetAccount("r3"); acc != nil {
		t.Fatal("转发链上的账号级限制应即时删除")
	}
	reps, _ := store.ListRiskReports(10)
	if len(reps) != 1 || reps[0].Source != "proxy" {
		t.Fatalf("应留下 source=proxy 的上报，得 %+v", reps)
	}
}

// TestRestrictedFallsBackToDisable 未注入回调时（极简装配）退化为停用，绝不静默放过。
func TestRestrictedFallsBackToDisable(t *testing.T) {
	store, px, _ := newTestApp(t, &Account{UID: "r4", AccessToken: "tok"})
	defer store.Close()
	defer px.Close()
	px.onAccountRestricted = nil

	px.reportUpstreamError("r4", "m", 403, []byte(bodyRestricted11140), time.Time{}, upstream.ResetNone, nil)
	st, ok := px.pool.Status("r4")
	if !ok || !st.Disabled {
		t.Fatalf("未注入回调时应落为停用，状态=%+v ok=%v", st, ok)
	}
	if acc, _ := store.GetAccount("r4"); acc == nil {
		t.Fatal("未注入回调时不该删除账号（只停用）")
	}
}
