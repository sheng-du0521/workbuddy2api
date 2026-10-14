// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 探活总闸（v0.24.3）
//
// 背景：2026-10-13 封号事故。管理后台账号池页曾为**每张卡片**自动出站查积分
// （creditPanel → /api/admin/accounts/{uid}/resources → writeCreditDetail），
// 59 张卡 = 59 个并发出站、同一个出口 IP、几秒内打完 → 上游风控返回 11140 →
// 30 个健康账号被误判停用。
//
// 前端已改为「点开才查」（根因修复）；本闸是**服务端兜底**：
// 即便将来又有人引入「渲染即出站」的 UI，也不会再形成并发风暴。
// ---------------------------------------------------------------------------

// TestProbeGateLimitsInflight 全局并发上限：第 probeMaxInflight+1 个被拒。
func TestProbeGateLimitsInflight(t *testing.T) {
	g := &probeGate{}
	var releases []func()
	// 用不同的 uid，避免撞上「单账号最小间隔」那条规则（本用例只测并发上限）。
	for i := 0; i < probeMaxInflight; i++ {
		rel, err := g.acquire("uid-" + string(rune('a'+i)))
		if err != nil {
			t.Fatalf("第 %d 个并发获取应成功，得 err=%v", i+1, err)
		}
		releases = append(releases, rel)
	}
	if _, err := g.acquire("uid-z"); err == nil {
		t.Fatal("超过全局并发上限应被拒（不排队 —— 排队只是把风暴推迟）")
	}
	// 释放一个后应能再获取一个。
	releases[0]()
	if _, err := g.acquire("uid-z"); err != nil {
		t.Fatalf("释放后应可再获取，得 err=%v", err)
	}
}

// TestProbeGateMinIntervalPerAccount 同一账号最小间隔：窗口内重复探活被拒。
func TestProbeGateMinIntervalPerAccount(t *testing.T) {
	g := &probeGate{}
	rel, err := g.acquire("same")
	if err != nil {
		t.Fatalf("首次应成功：%v", err)
	}
	rel()

	if _, err := g.acquire("same"); err == nil {
		t.Fatal("同一账号在最小间隔内重复探活应被拒（重复探活没有新增信息量）")
	}
	// 不同账号不受影响（间隔是**按账号**的，不是全局节流）。
	if rel2, err := g.acquire("other"); err != nil {
		t.Fatalf("不同账号应可正常获取：%v", err)
	} else {
		rel2()
	}
}

// TestProbeGateGC 过期条目被回收（map 不能只增不减）。
func TestProbeGateGC(t *testing.T) {
	g := &probeGate{}
	rel, _ := g.acquire("stale")
	rel()
	// 直接推进时间不可行（用真实时钟），改为断言 gc 在「未到期」时不误删。
	g.gc(time.Now())
	if _, err := g.acquire("stale"); err == nil {
		t.Fatal("未过期条目不该被 gc 删掉（否则最小间隔形同虚设）")
	}
	// 构造一条过期记录：手动把时刻拨回过去。
	g.mu.Lock()
	g.last["old"] = time.Now().Add(-2 * probeMinInterval)
	g.mu.Unlock()
	g.gc(time.Now())
	g.mu.Lock()
	_, still := g.last["old"]
	g.mu.Unlock()
	if still {
		t.Fatal("过期条目应被 gc 回收")
	}
}

// TestProbeGateReleaseIdempotent release 被重复调用不得把计数减成负数。
//
// 为什么值得单测：真实调用方是 `defer release()`，但 handler 里也可能提前
// 显式调用一次（如提前返回分支）—— 重复 release 若把 inflight 减穿，
// 并发上限就被悄悄放开了（且表现为「闸好像没生效」，极难排查）。
func TestProbeGateReleaseIdempotent(t *testing.T) {
	g := &probeGate{}
	rel, err := g.acquire("u")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	rel()
	rel()
	rel()
	g.mu.Lock()
	n := g.inflight
	g.mu.Unlock()
	if n != 0 {
		t.Fatalf("重复 release 后 inflight=%d，期望 0", n)
	}
}

// TestHealthChatProbeModelSelection 探活模型的选取顺序：kv 指定 > 内置兜底首个。
func TestHealthChatProbeModelSelection(t *testing.T) {
	store, _, a := newTestApp(t)
	defer store.Close()

	// 无 kv 配置 → 取内置兜底列表首个（非空）。
	if got := a.hcChatProbeModel(); got == "" {
		t.Fatal("无配置时应回落到内置兜底模型，不得为空")
	}
	// kv 指定 → 优先。
	if err := store.SetKV(kvHealthProbeModel, "my-cheap-model"); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	if got := a.hcChatProbeModel(); got != "my-cheap-model" {
		t.Fatalf("kv 指定的模型应优先，得 %q", got)
	}
}

// TestHealthChatProbeEnabledDefaultOn 对话探活**默认开启**（用户要求）。
//
// 默认开启是刻意的：事故的盲区正是「只探余额」，把它做成选配等于默认保留盲区。
func TestHealthChatProbeEnabledDefaultOn(t *testing.T) {
	store, _, a := newTestApp(t)
	defer store.Close()

	if !a.hcChatProbeEnabled() {
		t.Fatal("默认应开启对话探活")
	}
	if err := store.SetKV(kvHealthProbeChat, "0"); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	if a.hcChatProbeEnabled() {
		t.Fatal("kv 写 0 后应关闭")
	}
}

// TestHealthChatProbeSkipsWithoutAuth 无凭证时跳过而不是 panic / 误判失败。
func TestHealthChatProbeSkipsWithoutAuth(t *testing.T) {
	store, _, a := newTestApp(t)
	defer store.Close()

	out := a.hcChatProbe(nil)
	if out.OK {
		t.Fatal("无凭证不应判为通过")
	}
	if out.Err != nil {
		t.Fatal("跳过不应产生「已分类错误」——否则会被误当成账号故障去处置")
	}
	if out.Note == "" {
		t.Fatal("跳过要给出可读原因")
	}
}
