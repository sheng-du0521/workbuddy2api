// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package scheduler

import (
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// 本文件钉住「只消费」模式对保活的约束（v0.31.40）。
//
// 保活本身就是**主动刷新**，而主动刷新就是会话冲突的源头：上游 refresh 是轮换制，
// 我们刷成功就会把号主本机那份打掉，刷失败则白撞一次已作废的票。
// 所以一旦某号被判定存在冲突（passive），保活必须**一次上游请求都不打** ——
// 不是"少打"，是"完全不打"。
//
// 放在 scheduler 包而不是 webapp：这里是唯一能直接驱动 RunKeepaliveNow 的地方，
// 用 webapp 那边的公共入口测反而要绕一圈（且容易测成"看起来跑了"）。

// TestKeepaliveSkipsPassiveAccount passive 号的保活零出站；auto 号照常刷新。
func TestKeepaliveSkipsPassiveAccount(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	// 两个号：一个 auto、一个 passive。ExpiresAt 都设成"马上过期"，
	// 否则惰性阈值会把两个都跳过 —— 那样这条用例就什么都没证明。
	autoAuth := &auth.Auth{UID: "auto1", AccessToken: "old", RefreshToken: "rt-a", ExpiresAt: 1}
	passiveAuth := &auth.Auth{UID: "pass1", AccessToken: "old", RefreshToken: "rt-p", ExpiresAt: 1}
	p.Add(autoAuth)
	p.Add(passiveAuth)
	if _, changed := p.SetPassiveRefresh("pass1", true); !changed {
		t.Fatal("前置条件：SetPassiveRefresh 应报告状态变化")
	}

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	s.RunKeepaliveNow()

	// 只应刷新 auto1 一次。
	if got := f.refreshCalls.Load(); got != 1 {
		t.Fatalf("passive 号的保活不该出站：refreshCalls=%d want 1", got)
	}
	if autoAuth.AccessToken != "new" {
		t.Errorf("auto 号应被刷新：%s", autoAuth.AccessToken)
	}
	if passiveAuth.AccessToken != "old" {
		t.Errorf("passive 号不该被刷新（token 变了说明打了上游）：%s", passiveAuth.AccessToken)
	}
}

// TestPassiveAccountStillSchedulable 只消费 ≠ 停用：它必须仍在选号池里。
//
// 这条是防"顺手把 passive 当成 disabled"的：一旦混用，一个只是不续期的号会被
// 摘出服务，等于把冲突的代价放大成停服。
func TestPassiveAccountStillSchedulable(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "pass1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1})
	p.SetPassiveRefresh("pass1", true)

	st, ok := p.Status("pass1")
	if !ok {
		t.Fatal("账号应在池内")
	}
	if st.Disabled || st.ManualDisabled || st.Cooling {
		t.Fatalf("passive 不该带来任何停用/冷却语义：%+v", st)
	}
	if !st.PassiveRefresh {
		t.Fatal("Status 必须透出 passive_refresh（前端据此显示徽标与反向开关）")
	}
}
