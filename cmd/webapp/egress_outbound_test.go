// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 出站口径（统一走服务器）与"设置页也守规则"的回归（v0.31.35）。
package main

import (
	"net/http"
	"testing"
	"time"
)

// TestEgressPolicyToServerAndRestore 统一走服务器：预览 → 执行 → 撤回。
//
// 这是**批量改生产配置**的动作，所以三条都必须钉住：
//  1. dry_run 不写库（否则"预览"就等于执行了）；
//  2. 执行后：账号出口全空、全局默认清空、备份存在；
//  3. 撤回只恢复"当前为空"的号 —— 切换后有人手工配过的不覆盖
//     （回滚的语义是"撤销我那次操作"，不是"回到过去"）。
func TestEgressPolicyToServerAndRestore(t *testing.T) {
	f := usApp(t)

	line := "http://127.0.0.1:12070"
	if err := f.store.UpsertEgressProxy(&EgressProxy{
		ID: "op-line", Name: "A 的出口", URL: line, Enabled: true, LastOK: true,
		LastIP: "1.1.1.1", OwnerUserID: f.userA.ID, CreatedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("UpsertEgressProxy: %v", err)
	}
	// A 的一个号绑线路、另一个号显式直连；再设一个全局默认。
	if err := f.store.SetAccountEgress("us-a-acct", line); err != nil {
		t.Fatalf("set line: %v", err)
	}
	if err := f.store.SetAccountEgress("us-b-acct", "direct"); err != nil {
		t.Fatalf("set direct: %v", err)
	}
	if err := f.store.SetKV(kvEgressDefault, line); err != nil {
		t.Fatalf("set default: %v", err)
	}

	// ---- 0) 快照应报 mixed ----
	code, raw := eaReq(t, f.mux, "GET", "/api/admin/egress/policy", "", f.adminCookie, "")
	if code != http.StatusOK {
		t.Fatalf("policy GET 应 200，得 %d %s", code, raw)
	}
	if got := eaJSON(t, raw); got["mode"] != "mixed" || got["line_accounts"].(float64) < 1 {
		t.Fatalf("快照应报 mixed 且至少 1 个号绑线路，得 %v", got)
	}

	// ---- 1) dry_run 不改任何东西 ----
	code, raw = eaReq(t, f.mux, "POST", "/api/admin/egress/policy",
		`{"mode":"server","dry_run":true}`, f.adminCookie, "")
	if code != http.StatusOK {
		t.Fatalf("dry_run 应 200，得 %d %s", code, raw)
	}
	if a, _ := f.store.GetAccount("us-a-acct"); a == nil || a.EgressProxy != line {
		t.Fatalf("dry_run 不得写库，账号出口被改成 %q", a.EgressProxy)
	}
	if d, _ := f.store.GetKV(kvEgressDefault); d != line {
		t.Fatalf("dry_run 不得清默认，得 %q", d)
	}
	pv := eaJSON(t, raw)
	if pv["affected"].(float64) != 2 {
		t.Fatalf("dry_run 应报 2 个受影响账号（1 线路 + 1 直连），得 %v", pv["affected"])
	}

	// ---- 2) 执行 ----
	code, raw = eaReq(t, f.mux, "POST", "/api/admin/egress/policy",
		`{"mode":"server"}`, f.adminCookie, "")
	if code != http.StatusOK {
		t.Fatalf("server 应 200，得 %d %s", code, raw)
	}
	for _, uid := range []string{"us-a-acct", "us-b-acct"} {
		acc, _ := f.store.GetAccount(uid)
		if acc == nil || acc.EgressProxy != "" {
			t.Fatalf("%s 出口应被清空，得 %q", uid, acc.EgressProxy)
		}
	}
	if d, _ := f.store.GetKV(kvEgressDefault); d != "" {
		t.Fatalf("全局默认应被清空，得 %q", d)
	}
	if b, _ := f.store.GetKV(kvEgressPolicyBackup); b == "" {
		t.Fatal("执行前必须留备份（否则不可撤回）")
	}
	// **运行时也要跟上**（v0.31.35 实测踩到）：只清库不同步内存时，
	// `p.defaultEgress` 仍是旧值，账号实际还在走原线路 ——
	// 而库里已经是 38 个"未指定"，两个口径自相矛盾。
	if f.app.proxy.defaultEgress != "" {
		t.Fatalf("运行时默认出口应被清空，得 %q（少调了 ReloadSelection？）", f.app.proxy.defaultEgress)
	}
	if got := f.app.proxy.ResolveEgress("us-a-acct"); got != "" {
		t.Fatalf("切换后 us-a-acct 的生效出口应为空（=服务器直出），得 %q", got)
	}
	// 线路池不能被改动（只清绑定，不动线路本体）。
	if pl, _ := f.store.ListEgressProxies(); len(pl) != 1 || !pl[0].Enabled {
		t.Fatalf("线路池不该被这次操作改动，得 %+v", pl)
	}

	// ---- 3) 切换后手工配一个号 → 撤回时不能覆盖它 ----
	if err := f.store.SetAccountEgress("us-b-acct", "direct"); err != nil {
		t.Fatalf("手工改: %v", err)
	}
	code, raw = eaReq(t, f.mux, "POST", "/api/admin/egress/policy",
		`{"mode":"restore"}`, f.adminCookie, "")
	if code != http.StatusOK {
		t.Fatalf("restore 应 200，得 %d %s", code, raw)
	}
	if a, _ := f.store.GetAccount("us-a-acct"); a == nil || a.EgressProxy != line {
		t.Fatalf("撤回应把线路恢复到 us-a-acct，得 %q", a.EgressProxy)
	}
	if b, _ := f.store.GetAccount("us-b-acct"); b == nil || b.EgressProxy != "direct" {
		t.Fatalf("切换后手工配过的号不该被撤回覆盖，得 %q", b.EgressProxy)
	}
	if d, _ := f.store.GetKV(kvEgressDefault); d != "" {
		t.Log("提示：restore 只恢复账号出口，不恢复全局默认（避免把全平台又指回某条线路）")
	}
}

// TestHostEgressSetRejectsForeignLine 设置页的「我的出口 → 应用到全部」也必须守规则。
//
// 这是一个**真实的绕过口**：`/api/host/egress/assign` 拦得住别人的线路，
// 但设置页那条路径此前只校验格式，用户在输入框里手填一个别人的线路 URL
// 就能把号绑上去。更糟的是 `ensureUserEgressLine` 里无条件 `Enabled = true`，
// 会把别人**已停用**的线路顺带拉活。
func TestHostEgressSetRejectsForeignLine(t *testing.T) {
	f := usApp(t)

	other := "http://127.0.0.1:12085"
	// 刻意设成"已停用"：如果代码里那句 Enabled=true 还在，它会被悄悄改回 true。
	if err := f.store.UpsertEgressProxy(&EgressProxy{
		ID: "fo-line", Name: "B 的出口", URL: other, Enabled: false, LastOK: false,
		LastIP: "9.9.9.9", OwnerUserID: f.userB.ID, CreatedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("UpsertEgressProxy: %v", err)
	}

	code, raw := eaReq(t, f.mux, "POST", "/api/host/egress",
		`{"all":true,"proxy":"`+other+`"}`, f.cookieA, "")
	if code != http.StatusForbidden {
		t.Fatalf("设置页填别人的线路必须 403，得 %d %s", code, raw)
	}
	if a, _ := f.store.GetAccount("us-a-acct"); a == nil || a.EgressProxy != "" {
		t.Fatalf("被拒绝的设置不该改动账号，得 %q", a.EgressProxy)
	}
	// 关键副作用：别人的线路必须仍是停用状态。
	pl, _ := f.store.ListEgressProxies()
	for _, p := range pl {
		if p.URL == other && p.Enabled {
			t.Fatal("别人的停用线路被这条路径重新启用了（Enabled 从 false 变 true）—— 跨归属副作用")
		}
	}

	// 正向：填平台线路仍然可用（规则只针对他人线路）。
	plat := "http://127.0.0.1:11080"
	if err := f.store.UpsertEgressProxy(&EgressProxy{
		ID: "fo-plat", Name: "平台", URL: plat, Enabled: true, LastOK: true,
		LastIP: "2.2.2.2", CreatedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("UpsertEgressProxy plat: %v", err)
	}
	code, raw = eaReq(t, f.mux, "POST", "/api/host/egress",
		`{"all":true,"proxy":"`+plat+`"}`, f.cookieA, "")
	if code != http.StatusOK {
		t.Fatalf("平台线路应可应用，得 %d %s", code, raw)
	}
	if a, _ := f.store.GetAccount("us-a-acct"); a == nil || a.EgressProxy != plat {
		t.Fatalf("平台线路未写入，得 %q", a.EgressProxy)
	}
}
