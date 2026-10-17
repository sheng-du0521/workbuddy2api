// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 代理规则边界（v0.31.34）。
//
// 规则一句话：**用户的线路只承载自己的号**。
//
//	· 用户端：能看到的线路 = 平台 + 自己的；他人的线路不返回、不可分配；
//	· 管理端：可以把任何线路（含用户线路）分配给任何账号 —— 这是"借用住宅 IP"的唯一入口；
//	· 出口代理 = 账号代理的同一条线路（v0.31.33 一致化），所以上面的规则对两者同时生效。
//
// 这份用例守的是"规则本身"，而不是某一次实现的细节：
// 如果以后有人觉得"顺便放开一下"很方便，这里会红，并指向为什么不能放开。
package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestHostRebalanceUsesOnlyMineAndPlatform 按出口 IP 均衡不能把号推到别人的线路上。
//
// 均衡是用户自助动作里**唯一会批量改出口**的一个。如果它的候选集比 assign 宽，
// 就会出现"单号指定被拒、一键均衡却成功"的矛盾 —— 这种不一致比放开本身更危险
// （用户会以为规则是"看情况"）。
func TestHostRebalanceUsesOnlyMineAndPlatform(t *testing.T) {
	f := usApp(t)

	platform := "http://127.0.0.1:11080"
	other := "http://127.0.0.1:12080" // B 的线路
	mine := "http://127.0.0.1:12070"  // A 的线路
	for _, p := range []*EgressProxy{
		{ID: "rb-plat", Name: "平台", URL: platform, Enabled: true, LastOK: true,
			LastIP: "10.0.0.1", CreatedAt: time.Now().Unix()},
		{ID: "rb-other", Name: "B 的出口", URL: other, Enabled: true, LastOK: true,
			LastIP: "10.0.0.2", OwnerUserID: f.userB.ID, CreatedAt: time.Now().Unix()},
	} {
		if err := f.store.UpsertEgressProxy(p); err != nil {
			t.Fatalf("UpsertEgressProxy: %v", err)
		}
	}

	// 补 3 个号凑 4 个：1 个号在 2 条线路上只会落到 1 条（轮流分配），
	// 单号场景看不出"是否真的分散"。
	for i := 0; i < 3; i++ {
		if err := f.store.UpsertAccount(&Account{
			UID: "us-a-rb" + itoa(i), Nickname: "rb" + itoa(i), Realm: "cn",
			AccessToken: "t", RefreshToken: "r",
			ExpiresAt:   time.Now().Add(time.Hour).Unix(),
			OwnerUserID: f.userA.ID, ConsumeGroup: "us", PoolState: "in",
		}); err != nil {
			t.Fatalf("UpsertAccount: %v", err)
		}
	}

	// 先只留平台 + B 的线路：均衡只能在平台线路上落地。
	code, raw := eaReq(t, f.mux, "POST", "/api/host/egress/rebalance", `{}`, f.cookieA, "")
	if code != http.StatusOK {
		t.Fatalf("均衡应 200，得 %d %s", code, raw)
	}
	accs, err := f.store.ListAccountsByOwner(f.userA.ID)
	if err != nil {
		t.Fatalf("ListAccountsByOwner: %v", err)
	}
	if len(accs) == 0 {
		t.Fatal("夹具里 A 应有账号")
	}
	for _, a := range accs {
		if strings.TrimSpace(a.EgressProxy) == other {
			t.Fatalf("均衡把 %s 推到了别人的线路上（%s）—— 用户端规则被绕过", a.UID, other)
		}
	}

	// 加上自己的线路之后，两个出口都要被用上（否则"分散"是假的）。
	if err := f.store.UpsertEgressProxy(&EgressProxy{
		ID: "rb-mine", Name: "我的", URL: mine, Enabled: true, LastOK: true,
		LastIP: "10.0.0.3", OwnerUserID: f.userA.ID, CreatedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("UpsertEgressProxy mine: %v", err)
	}
	code, raw = eaReq(t, f.mux, "POST", "/api/host/egress/rebalance", `{}`, f.cookieA, "")
	if code != http.StatusOK {
		t.Fatalf("均衡应 200，得 %d %s", code, raw)
	}
	out := eaJSON(t, raw)
	used := map[string]bool{}
	accs, _ = f.store.ListAccountsByOwner(f.userA.ID)
	for _, a := range accs {
		used[strings.TrimSpace(a.EgressProxy)] = true
		if strings.TrimSpace(a.EgressProxy) == other {
			t.Fatalf("均衡把 %s 推到了别人的线路上", a.UID)
		}
	}
	if !used[platform] || !used[mine] {
		t.Fatalf("两个可用出口都应被用上（平台 %v / 我的 %v），实际分布 %v", used[platform], used[mine], used)
	}
	if got := out["distinct_exit_ips"].(float64); got != 2 {
		t.Fatalf("应报 2 个不同出口 IP，得 %v", got)
	}
}

// TestHostEgressPluginContract 「本地出口代理」插件的契约。
//
// 三件事必须同时成立，缺一个都会让功能"看起来在、实际用不了"：
//  1. 它出现在**用户端**插件列表里，且 kind=egress（前端据此渲染独立界面，
//     没有 kind 会掉进 claim 分支画一个"开始领取"的按钮）；
//  2. 它**不出现在管理端**列表（管理端有自己的「代理」导航，重复入口只会让两处状态打架）；
//  3. 通过批量执行接口调用它会被明确拒绝（400 + 指路），而不是 501"未实现"
//     —— 后者会让运维以为服务端缺代码。
func TestHostEgressPluginContract(t *testing.T) {
	f := usApp(t)

	code, raw := eaReq(t, f.mux, "GET", "/api/plugins", "", f.cookieA, "")
	if code != http.StatusOK {
		t.Fatalf("用户端插件列表应 200，得 %d %s", code, raw)
	}
	d := eaJSON(t, raw)
	var got map[string]any
	for _, x := range d["plugins"].([]any) {
		m := x.(map[string]any)
		if m["id"] == pluginIDHostEgress {
			got = m
			break
		}
	}
	if got == nil {
		t.Fatalf("用户端插件列表应包含 %s，得 %v", pluginIDHostEgress, d["plugins"])
	}
	if got["kind"] != "egress" {
		t.Fatalf("kind 应为 egress（前端靠它进独立界面），得 %v", got["kind"])
	}

	// 管理端不应出现（Audience=user）。
	code, raw = eaReq(t, f.mux, "GET", "/api/admin/plugins", "", f.adminCookie, "")
	if code != http.StatusOK {
		t.Fatalf("管理端插件列表应 200，得 %d %s", code, raw)
	}
	for _, x := range eaJSON(t, raw)["plugins"].([]any) {
		if m := x.(map[string]any); m["id"] == pluginIDHostEgress {
			t.Fatalf("管理端不该出现 %s（它有独立的「代理」导航）", pluginIDHostEgress)
		}
	}

	// 批量执行接口：400 + 指路（不是 501）。
	code, raw = eaReq(t, f.mux, "POST", "/api/plugins/"+pluginIDHostEgress+"/claim",
		`{"all":true,"dry_run":true}`, f.cookieA, "")
	if code != http.StatusBadRequest {
		t.Fatalf("独立界面型插件被批量执行时应 400，得 %d %s", code, raw)
	}
	if !strings.Contains(string(raw), "打开") {
		t.Fatalf("400 文案应指路（在插件页点「打开」），得 %s", raw)
	}
}

// TestHostForeignBoundIsFlagged 存量越界绑定必须被标出来。
//
// 规则收紧只影响"以后能不能选"，**不会**把已经绑在别人线路上的号改回去。
// 生产上就有（v0.31.32 可借用时期留下的 10 个号）。如果不标：
// 用户看到"这个号有出口"，却找不到自己什么时候配过那条看不见的线路；
// 运维也看不出这批号正靠在别人的家宽上（对方一关机就全掉）。
func TestHostForeignBoundIsFlagged(t *testing.T) {
	f := usApp(t)
	other := "http://127.0.0.1:12090"
	if err := f.store.UpsertEgressProxy(&EgressProxy{
		ID: "fb-other", Name: "B 的出口", URL: other, Enabled: true, LastOK: true,
		LastIP: "10.9.9.9", OwnerUserID: f.userB.ID, CreatedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("UpsertEgressProxy: %v", err)
	}
	// 直接写库模拟存量越界（走接口会被 403 拦住，造不出这种数据）。
	if _, err := f.store.db.Exec(`UPDATE accounts SET egress_proxy = ? WHERE uid = ?`,
		other, "us-a-acct"); err != nil {
		t.Fatalf("seed foreign bind: %v", err)
	}

	d := heLines(t, f, f.cookieA)
	for _, x := range d["lines"].([]any) {
		if m := x.(map[string]any); m["url"] == other {
			t.Fatal("他人的线路仍出现在可见列表里")
		}
	}
	var row map[string]any
	for _, x := range d["accounts"].([]any) {
		if m := x.(map[string]any); m["uid"] == "us-a-acct" {
			row = m
			break
		}
	}
	if row == nil {
		t.Fatal("账号列表里找不到 us-a-acct")
	}
	if row["foreign_bound"] != true {
		t.Fatalf("越界绑定必须标 foreign_bound=true，得 %v", row)
	}
	if row["foreign_owner_name"] == nil || row["foreign_owner_name"] == "" {
		t.Fatalf("越界绑定必须带线路主人（用户要判断该不该改回去），得 %v", row)
	}
	if row["exit_ip"] != "10.9.9.9" {
		t.Fatalf("越界绑定仍应显示实际出口 IP，得 %v", row["exit_ip"])
	}

	// 管理端概览要能报出这类账号数（不是错误，但要知情）。
	code, raw := eaReq(t, f.mux, "GET", "/api/admin/egress/overview", "", f.adminCookie, "")
	if code != http.StatusOK {
		t.Fatalf("overview 应 200，得 %d %s", code, raw)
	}
	if got := eaJSON(t, raw)["foreign_bound_accounts"].(float64); got < 1 {
		t.Fatalf("overview 应报出至少 1 个越界绑定，得 %v", got)
	}
}
