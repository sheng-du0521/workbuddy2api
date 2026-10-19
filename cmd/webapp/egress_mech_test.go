// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"workbuddy2api/internal/egress"
)

// TestEgressEnableDecision 线路启停决策：自动停用要打标记，人工停用不许被拉活。
//
// 这条判定是 v0.31.47 的核心。此前"自动停用"与"人工停用"共用一个 Enabled 位，
// 于是出现一个没人收尾的状态：探活失败 → 停用 → 用户重新拉起代理 → 端口通了，
// 但线路仍是停用的，账号也不回来。用户看到的是"代理拉起来了，可全部失败"。
func TestEgressEnableDecision(t *testing.T) {
	cases := []struct {
		name       string
		probeOK    bool
		enabled    bool
		autoOff    bool
		failStreak int64
		wantEnable bool
		wantAuto   bool
		wantAction string
	}{
		{"连续失败达阈值 → 自动停用并打标记", false, true, false, egressFailThreshold, false, true, "disable"},
		{"失败但未达阈值 → 不动", false, true, false, egressFailThreshold - 1, true, false, ""},
		{"已停用再失败 → 不重复处理", false, false, true, 9, false, true, ""},
		{"自动停用 + 探活成功 → 恢复", true, false, true, 0, true, false, "enable"},
		// 关键反例：管理员手动停用的线路，探活成功也**不许**自动启用。
		// v0.31.34 踩过"无条件 Enabled=true 把别人停用线路拉活"。
		{"人工停用 + 探活成功 → 保持停用", true, false, false, 0, false, false, ""},
		{"启用中 + 探活成功 → 不动", true, true, false, 0, true, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			en, auto, action := egressEnableDecision(c.probeOK, c.enabled, c.autoOff, c.failStreak)
			if en != c.wantEnable || auto != c.wantAuto || action != c.wantAction {
				t.Fatalf("得 (enabled=%v auto=%v action=%q)，期望 (%v %v %q)",
					en, auto, action, c.wantEnable, c.wantAuto, c.wantAction)
			}
		})
	}
}

// TestRehomeWriteValue 迁移到「全局默认出口」时要写空串，保住"跟随默认"的语义。
//
// 写死 URL 的后果：那个号看起来"被显式指定到默认线路"，
// 之后用户重新拉起自己的代理时会被 apply 的"非破坏式绑定"跳过 ——
// 现场表现就是"页面显示已应用，号却仍然从平台出口出去"。
func TestRehomeWriteValue(t *testing.T) {
	def := "http://127.0.0.1:11080"
	if got := rehomeWriteValue(def, def); got != "" {
		t.Fatalf("目标即默认出口时应写空串（跟随默认），得 %q", got)
	}
	// 大小写与空格都要归一：URL 是线路身份，字符串差异会造出"两条相同线路"。
	if got := rehomeWriteValue(" HTTP://127.0.0.1:11080 ", def); got != "" {
		t.Fatalf("大小写/空格差异也应识别为同一条，得 %q", got)
	}
	if got := rehomeWriteValue("http://127.0.0.1:21150", def); got != "http://127.0.0.1:21150" {
		t.Fatalf("非默认目标应原样写回，得 %q", got)
	}
	// 默认出口为空（直连）时，任何目标都不是"默认"。
	if got := rehomeWriteValue(def, ""); got != def {
		t.Fatalf("默认出口为空时不应写空，得 %q", got)
	}
}

// TestComputeEgressBadRuntime 出口异常必须包含**运行态**，不能只看 URL 格式。
//
// 这是 2026-10-19 现场的直接成因：线路已被探活自动停用，而 egressBad 只校验格式，
// 于是绑在上面的账号照常被选中 —— 每次请求都连不上，用户看到"全部失败"。
func TestComputeEgressBadRuntime(t *testing.T) {
	const (
		lineDown  = "http://127.0.0.1:21150"
		lineFlaky = "http://127.0.0.1:21160"
		lineOK    = "http://127.0.0.1:11080"
		lineOut   = "http://1.2.3.4:8080" // 不在池里（手填）
	)
	lines := map[string]lineState{
		lineDown:  {Name: "甲的出口", Enabled: false, FailStreak: 5},
		lineFlaky: {Name: "乙的出口", Enabled: true, FailStreak: egressFailThreshold},
		lineOK:    {Name: "平台默认", Enabled: true, FailStreak: 0},
	}
	accs := []*Account{
		{UID: "u-down", EgressProxy: lineDown},
		{UID: "u-flaky", EgressProxy: lineFlaky},
		{UID: "u-ok", EgressProxy: lineOK},
		{UID: "u-out", EgressProxy: lineOut},
		{UID: "u-empty", EgressProxy: ""},
		{UID: "u-direct", EgressProxy: egress.Direct},
		{UID: "u-badfmt", EgressProxy: "://nope"},
	}
	// 默认出口正常：u-empty 跟随默认 → 可用。
	bad := computeEgressBad(accs, lineOK, lines)
	for _, uid := range []string{"u-ok", "u-out", "u-empty", "u-direct"} {
		if reason, isBad := bad[uid]; isBad {
			t.Fatalf("%s 不该被判出口异常，得 %q", uid, reason)
		}
	}
	for _, uid := range []string{"u-down", "u-flaky", "u-badfmt"} {
		if _, isBad := bad[uid]; !isBad {
			t.Fatalf("%s 应判出口异常", uid)
		}
	}
	if r := bad["u-down"]; !strings.Contains(r, "已停用") {
		t.Fatalf("停用线路的原因要写清「已停用」，得 %q", r)
	}
	if r := bad["u-flaky"]; !strings.Contains(r, "探活失败") {
		t.Fatalf("连续失败的原因要写清，得 %q", r)
	}
	// 不在池里的手填线路**不判坏**：探活只覆盖池内线路，我们不能凭空否掉它。
	if _, isBad := bad["u-out"]; isBad {
		t.Fatal("池外手填线路不应被判坏（没有依据）")
	}
}

// TestComputeEgressBadDefaultDown 默认出口不可用时，所有"跟随默认"的号都不可用。
//
// 影响面最大的一条：一个默认出口填错/挂掉，整池未单独配置的账号同时失效。
func TestComputeEgressBadDefaultDown(t *testing.T) {
	const def = "http://127.0.0.1:11080"
	lines := map[string]lineState{
		def: {Name: "平台默认", Enabled: false, FailStreak: 4},
	}
	accs := []*Account{
		{UID: "follow", EgressProxy: ""},
		{UID: "direct", EgressProxy: egress.Direct},
		{UID: "own", EgressProxy: "http://127.0.0.1:21150"},
	}
	bad := computeEgressBad(accs, def, lines)
	if _, isBad := bad["follow"]; !isBad {
		t.Fatal("跟随默认的号必须被判出口异常")
	}
	if !strings.Contains(bad["follow"], "全局默认") {
		t.Fatalf("原因应指明是全局默认出口，得 %q", bad["follow"])
	}
	if _, isBad := bad["direct"]; isBad {
		t.Fatal("显式直连不继承默认出口，不该被判坏")
	}
	if _, isBad := bad["own"]; isBad {
		t.Fatal("自绑线路（不在池里）不该因默认出口挂掉而被判坏")
	}
}

// TestApplyRefusesWhenLineUnusable 线路出不去的网时，apply 必须**一个号都不动**。
//
// 这是 v0.31.47 最重要的一条不变量。现场（2026-10-19）：用户点「一键拉起」，
// 前端在"隧道端口可连"时就自动 apply，于是该用户名下所有未指定出口的号
// 被绑到一条还没真正能用的线路上 —— 之后每条请求都失败，用户看到的是"全部挂掉"。
//
// 所以判据不能是"端口可连"（那只证明 sshd 在 listen），必须是**真实出网探测**。
func TestApplyRefusesWhenLineUnusable(t *testing.T) {
	store, _, a := newTestApp(t,
		// v0.31.50：必须是未入池号——已入池的号按规则不走个人出口。
		&Account{UID: "u-own", Nickname: "甲", OwnerUserID: 5, PoolState: ""},
		&Account{UID: "u-other", Nickname: "乙", OwnerUserID: 9, PoolState: "in"},
	)
	defer store.Close()

	a.egressVerify = func(string) (string, error) {
		return "", fmt.Errorf("%w：隧道端口可连，但经它出不了网", errEgressNotUsable)
	}
	_, err := a.applyHomeEgress(21150, egressScope{Platform: true})
	if !errors.Is(err, errEgressNotUsable) {
		t.Fatalf("应返回 errEgressNotUsable（HTTP 层据此回 409），实得 %v", err)
	}
	acc, _ := store.GetAccount("u-own")
	if strings.TrimSpace(acc.EgressProxy) != "" {
		t.Fatalf("验证失败却改动了账号出口：%q", acc.EgressProxy)
	}
	// 线路也不该被注册：留一条"没验证通过"的线路只会让页面多一条噪音。
	if list, _ := store.ListEgressProxies(); len(list) != 0 {
		t.Fatalf("验证失败却注册了线路：%d 条", len(list))
	}
}

// TestApplyWritesVerifiedExitIP 验证通过后，线路记录要立刻带上"正常 + 出口 IP"。
//
// 刚点完拉起的人最想知道的就是"我现在从哪个 IP 出去"，而定时探活要等 5 分钟。
// 把验证结果直接落库，页面不用等。
func TestApplyWritesVerifiedExitIP(t *testing.T) {
	store, _, a := newTestApp(t,
		// v0.31.50：必须是未入池号 —— 已入池的号按规则不走个人出口（需求二）。
		&Account{UID: "u-own", Nickname: "甲", OwnerUserID: 5, PoolState: ""},
	)
	defer store.Close()

	a.egressVerify = func(string) (string, error) { return "198.51.100.7", nil }
	if _, err := a.applyHomeEgress(21150, egressScope{Platform: true}); err != nil {
		t.Fatalf("apply 应成功：%v", err)
	}
	list, err := store.ListEgressProxies()
	if err != nil || len(list) != 1 {
		t.Fatalf("应注册 1 条线路，得 %d（err=%v）", len(list), err)
	}
	p := list[0]
	if !p.Enabled || !p.LastOK || p.LastIP != "198.51.100.7" {
		t.Fatalf("线路应启用且带出口 IP，得 enabled=%v last_ok=%v ip=%q", p.Enabled, p.LastOK, p.LastIP)
	}
	if p.AutoDisabled {
		t.Fatal("刚验证通过的线路不该带自动停用标记")
	}
	// v0.31.53：验证通过只说明"这条线路能用"，**不等于**"要把谁的号搬上来"。
	// 分配是管理端 assign 的事（带唯一闸门与审计），apply 碰账号就是越权。
	acc, _ := store.GetAccount("u-own")
	if strings.TrimSpace(acc.EgressProxy) != "" {
		t.Fatalf("apply 不得改账号出口，得 %q", acc.EgressProxy)
	}
}
