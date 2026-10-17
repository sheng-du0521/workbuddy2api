// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 用户端「账号出口」的回归测试（v0.31.31）。
//
// 用户现场："账号代理无法分配" + "两台电脑两个 IP，拉起来却是同一个出口"。
// 这份用例钉住四件事：
//
//	① 用户能把**自己的**号分到**自己或平台**的线路上（正向，最常用）；
//	② 用户**不能**把号指到别人的线路上（负向：出口会把 access_token 送给第三方）；
//	③ 用户**不能**通过 uid 参数改别人的号（静默剔除）；
//	④ 「一键拉起 → 应用」不再改绑已显式指定到其他线路的号（非破坏式）。
package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// heLines 调 GET /api/host/egress/lines。
func heLines(t *testing.T, f usFixture, cookie string) map[string]any {
	t.Helper()
	code, raw := eaReq(t, f.mux, "GET", "/api/host/egress/lines", "", cookie, "")
	if code != http.StatusOK {
		t.Fatalf("读账号出口应 200，得 %d %s", code, raw)
	}
	return eaJSON(t, raw)
}

// TestHostEgressLinesAndAssign 线路可见性 + 逐号分配 + 越权拦截。
func TestHostEgressLinesAndAssign(t *testing.T) {
	f := usApp(t)

	mine := "http://127.0.0.1:12070"     // A 自己的线路
	platform := "http://127.0.0.1:11080" // 平台线路
	others := "http://127.0.0.1:12080"   // B 的线路（A 不该能用）
	for _, p := range []*EgressProxy{
		{ID: "p-mine", Name: "A 自己的出口", URL: mine, Enabled: true, LastOK: true,
			LastIP: "1.1.1.1", OwnerUserID: f.userA.ID, CreatedAt: time.Now().Unix()},
		{ID: "p-plat", Name: "平台线路", URL: platform, Enabled: true, LastOK: true,
			LastIP: "2.2.2.2", CreatedAt: time.Now().Unix()},
		{ID: "p-other", Name: "B 的出口", URL: others, Enabled: true, LastOK: true,
			LastIP: "3.3.3.3", OwnerUserID: f.userB.ID, CreatedAt: time.Now().Unix()},
	} {
		if err := f.store.UpsertEgressProxy(p); err != nil {
			t.Fatalf("UpsertEgressProxy %s: %v", p.URL, err)
		}
	}

	d := heLines(t, f, f.cookieA)
	lines := d["lines"].([]any)
	// v0.31.34 规则收紧：**他人的线路不返回**（以前会列出来并允许借用）。
	//
	// 为什么改：那条规则的实际效果是"把 access_token 交给第三方机器"，
	// 一次确认弹窗不是技术保障；而且出了风控事件无法区分"我的号自己跑的"
	// 与"从别人家宽跑的"。用户线路只承载自己的号；要借用只能由管理员在后台显式分配。
	if len(lines) != 2 {
		t.Fatalf("线路列表应只含平台+自己的 2 条（他人的不返回），得 %d：%v", len(lines), lines)
	}
	noteOf := map[string]string{}
	seen := map[string]bool{}
	for _, x := range lines {
		m := x.(map[string]any)
		u := m["url"].(string)
		seen[u] = true
		noteOf[u], _ = m["note"].(string)
		if m["foreign"] == true {
			t.Fatalf("用户端不该再返回 foreign 线路：%v", m)
		}
	}
	if !seen[mine] || !seen[platform] {
		t.Fatalf("平台线路与自己的线路必须可见，得 %v", seen)
	}
	if seen[others] {
		t.Fatal("他人的线路不能出现在用户视角（v0.31.34 起结构性过滤）")
	}
	if noteOf[mine] != "" || noteOf[platform] != "" {
		t.Fatalf("启用中的线路都应可选，得 note=%q/%q", noteOf[mine], noteOf[platform])
	}
	if got := d["distinct_exit_ips"].(float64); got != 2 {
		t.Fatalf("可见线路有两个出口 IP（1.1.1.1 / 2.2.2.2），得 %v", got)
	}

	// 正向：把自己的号指到自己的线路
	code, raw := eaReq(t, f.mux, "POST", "/api/host/egress/assign",
		`{"uids":["us-a-acct"],"proxy":"`+mine+`"}`, f.cookieA, "")
	if code != http.StatusOK {
		t.Fatalf("分配给自己的线路应 200，得 %d %s", code, raw)
	}
	if d := eaJSON(t, raw); d["updated"].(float64) != 1 {
		t.Fatalf("应更新 1 个号，得 %v", d["updated"])
	}
	acc, _ := f.store.GetAccount("us-a-acct")
	if acc == nil || acc.EgressProxy != mine {
		t.Fatalf("账号出口未写入，得 %+v", acc)
	}

	// 负向（v0.31.34）：别人的线路 → 403。
	//
	// 这不是"越权兜底"而是**产品规则**，所以文案必须指路（"让管理员在后台分配"），
	// 写成"无权限"会让人以为是权限 bug。这里同时断言**账号没被改动** ——
	// 一条只报错但已经写库的接口，比不报错更糟。
	code, raw = eaReq(t, f.mux, "POST", "/api/host/egress/assign",
		`{"uids":["us-a-acct"],"proxy":"`+others+`"}`, f.cookieA, "")
	if code != http.StatusForbidden {
		t.Fatalf("指向他人的线路必须 403，得 %d %s", code, raw)
	}
	if !strings.Contains(string(raw), "管理员") {
		t.Fatalf("403 文案要说清下一步找管理员，得 %s", raw)
	}
	if a, _ := f.store.GetAccount("us-a-acct"); a == nil || a.EgressProxy != mine {
		t.Fatalf("被拒绝的分配不该改动账号，得 %+v", a)
	}

	// 负向：拿别人的 uid 改别人的号 → 目标被静默剔除，最终"没有可设置的账号"
	code, _ = eaReq(t, f.mux, "POST", "/api/host/egress/assign",
		`{"uids":["us-b-acct"],"proxy":"`+platform+`"}`, f.cookieA, "")
	if code != http.StatusBadRequest {
		t.Fatalf("改别人的号应 400（目标被剔除），得 %d", code)
	}
	if b, _ := f.store.GetAccount("us-b-acct"); b != nil && b.EgressProxy != "" {
		t.Fatalf("B 的号被改动了：%q", b.EgressProxy)
	}

	// 负向：不在线路池里的 URL → 400（不能拿任意代理当线路用）
	code, _ = eaReq(t, f.mux, "POST", "/api/host/egress/assign",
		`{"uids":["us-a-acct"],"proxy":"http://9.9.9.9:8080"}`, f.cookieA, "")
	if code != http.StatusBadRequest {
		t.Fatalf("未登记线路应 400，得 %d", code)
	}
}

// TestHostEgressRebalanceByExitIP 按出口 IP 均衡：不同的 IP 都要用上。
//
// 这才是"两台电脑两个宽带"真正落地的一步 —— 两条线路都通，但号全挤在一条上，
// 出口仍然只有一个。
func TestHostEgressRebalanceByExitIP(t *testing.T) {
	f := usApp(t)
	// A 名下再加 3 个号，凑 4 个便于看分布
	for i := 0; i < 3; i++ {
		acc := &Account{
			UID: "us-a-x" + itoa(i), Nickname: "x" + itoa(i), Realm: "cn",
			AccessToken: "t", RefreshToken: "r",
			ExpiresAt:   time.Now().Add(time.Hour).Unix(),
			OwnerUserID: f.userA.ID, ConsumeGroup: "us", PoolState: "in",
		}
		if err := f.store.UpsertAccount(acc); err != nil {
			t.Fatalf("UpsertAccount: %v", err)
		}
	}
	l1 := "http://127.0.0.1:12070"
	l2 := "http://127.0.0.1:12071"
	for _, p := range []*EgressProxy{
		{ID: "r1", Name: "家里", URL: l1, Enabled: true, LastOK: true,
			LastIP: "1.1.1.1", OwnerUserID: f.userA.ID},
		{ID: "r2", Name: "公司", URL: l2, Enabled: true, LastOK: true,
			LastIP: "2.2.2.2", OwnerUserID: f.userA.ID},
		// 与 r1 同一个出口 IP 的第三条线路：不能被当成"第三个出口"
		{ID: "r3", Name: "家里备用", URL: "http://127.0.0.1:12072", Enabled: true,
			LastOK: true, LastIP: "1.1.1.1", OwnerUserID: f.userA.ID},
	} {
		if err := f.store.UpsertEgressProxy(p); err != nil {
			t.Fatalf("UpsertEgressProxy: %v", err)
		}
	}

	code, raw := eaReq(t, f.mux, "POST", "/api/host/egress/rebalance", `{}`, f.cookieA, "")
	if code != http.StatusOK {
		t.Fatalf("均衡应 200，得 %d %s", code, raw)
	}
	d := eaJSON(t, raw)
	if got := d["distinct_exit_ips"].(float64); got != 2 {
		t.Fatalf("应只用 2 个不同出口 IP（第三条与第一条同 IP 应被折掉），得 %v", got)
	}
	if got := d["assigned"].(float64); got != 4 {
		t.Fatalf("4 个号都应被分配，得 %v", got)
	}

	// 逐号核对：两个 IP 上都要有号，且没有号被分到"同 IP 的备用线路"上。
	used := map[string]int{}
	accs, _ := f.store.ListAccountsByOwner(f.userA.ID)
	for _, acc := range accs {
		if acc.EgressProxy == "" {
			t.Fatalf("账号 %s 没有被分配出口", acc.UID)
		}
		used[acc.EgressProxy]++
	}
	if used[l1] != 2 || used[l2] != 2 {
		t.Fatalf("应各 2 个号，得 %v", used)
	}
	if used["http://127.0.0.1:12072"] != 0 {
		t.Fatalf("同 IP 的备用线路不该被选中，得 %v", used)
	}

	// 未给名单时只动自己的号：B 的号必须原样。
	if b, _ := f.store.GetAccount("us-b-acct"); b != nil && b.EgressProxy != "" {
		t.Fatalf("均衡不该碰别人的号，得 %q", b.EgressProxy)
	}
}

// TestApplyHomeEgressNonDestructive 一键拉起的「应用」不再搬走已显式指定出口的号。
//
// 旧行为（v0.31.30 及以前）：应用 = 把我名下所有号改绑到这条线路。
// 于是"在第二台电脑上点一次拉起"，第一台电脑上的号全被静默搬走 ——
// 两台机器最后只有一台在承重，用户看到的自然是同一个出口 IP。
func TestApplyHomeEgressNonDestructive(t *testing.T) {
	f := usApp(t)
	// A 名下两个号：一个已显式绑到"旧线路"，一个还没指定。
	oldLine := "http://127.0.0.1:12070"
	if err := f.store.SetAccountEgress("us-a-acct", oldLine); err != nil {
		t.Fatalf("SetAccountEgress: %v", err)
	}
	acc2 := &Account{
		UID: "us-a-new", Nickname: "新号", Realm: "cn",
		AccessToken: "t", RefreshToken: "r",
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
		OwnerUserID: f.userA.ID, ConsumeGroup: "us", PoolState: "in",
	}
	if err := f.store.UpsertAccount(acc2); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}

	// 新机器（另一条宽带）的隧道端口
	const tunnelPort = 12075
	res, err := f.app.applyHomeEgress(tunnelPort, egressScope{UserID: f.userA.ID}, false)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.HostBound != 1 || res.Skipped != 1 {
		t.Fatalf("应只绑 1 个、跳过 1 个（已显式指定），得 bound=%d skipped=%d", res.HostBound, res.Skipped)
	}
	if a1, _ := f.store.GetAccount("us-a-acct"); a1 == nil || a1.EgressProxy != oldLine {
		t.Fatalf("已显式指定出口的号被搬走了：%+v", a1)
	}
	if a2, _ := f.store.GetAccount("us-a-new"); a2 == nil || a2.EgressProxy != egressHomeURL(tunnelPort) {
		t.Fatalf("未指定的号应被绑到新线路，得 %+v", a2)
	}

	// 显式要求整体改绑时才覆盖（旧行为，由按钮文案承担告知义务）。
	res2, err := f.app.applyHomeEgress(tunnelPort, egressScope{UserID: f.userA.ID}, true)
	if err != nil {
		t.Fatalf("apply(rebindAll): %v", err)
	}
	if res2.Skipped != 0 || res2.HostBound != 2 {
		t.Fatalf("rebind_all 应改绑全部，得 bound=%d skipped=%d", res2.HostBound, res2.Skipped)
	}
	if a1, _ := f.store.GetAccount("us-a-acct"); a1 == nil || a1.EgressProxy != egressHomeURL(tunnelPort) {
		t.Fatalf("rebind_all 后应绑到新线路，得 %+v", a1)
	}
}
