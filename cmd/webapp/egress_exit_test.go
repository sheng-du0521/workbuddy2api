// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"strings"
	"testing"
)

// TestNormalizeExitIP 出口 IP 的各种历史形状都要归一成同一个值。
//
// 这条用例的意义不是"字符串处理对不对"，而是**同 IP 归并是否成立**：
// 只要 `"39.91.80.16"` 与 `"39.91.80.16（…）"` 不相等，生产上就会出现
// "两条线路同 IP 却算两个出口"（2026-10-17 现场）。
func TestNormalizeExitIP(t *testing.T) {
	cases := []struct{ in, want string }{
		{"39.91.80.16", "39.91.80.16"},
		{"  39.91.80.16  ", "39.91.80.16"},
		{"39.91.80.16（Shandong Jinan China Unicom Shandong Province Network）", "39.91.80.16"},
		{"39.91.80.16（山东 济南 联通）", "39.91.80.16"},
		{"1.2.3.4:8080", "1.2.3.4"},
		{"2001:db8::1", "2001:db8::1"},
		{"[2001:db8::1]:443", "2001:db8::1"},
		{"", ""},
		{"（未探到）", ""},
		{"无法获取", ""},
	}
	for _, c := range cases {
		if got := normalizeExitIP(c.in); got != c.want {
			t.Fatalf("normalizeExitIP(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
	// 最关键的一条：同一 IP 的两种形状必须归一到相同值。
	if a, b := normalizeExitIP("39.91.80.16"), normalizeExitIP("39.91.80.16（山东 济南 联通）"); a != b {
		t.Fatalf("同 IP 两种形状归一后仍不相等：%q vs %q", a, b)
	}
}

// TestGroupExitsCounts 只按 IP 数数：两条同 IP 的线路算一个出口。
func TestGroupExitsCounts(t *testing.T) {
	items := []exitItem{
		{URL: "http://127.0.0.1:11080", Name: "家里-联通", IP: "39.91.80.16", Owner: 0, OK: true},
		{URL: "http://127.0.0.1:21160", Name: "2541 的出口", IP: "39.91.80.16（山东 济南 联通）", Owner: 9916, OK: true},
		{URL: "http://10.0.0.2:8080", Name: "机房", IP: "8.8.8.8", Owner: 0, OK: true},
		{URL: "http://10.0.0.3:8080", Name: "未探活", IP: "", Owner: 0},
	}
	groups, distinct := groupExits(items)
	if distinct != 2 {
		t.Fatalf("不同出口 IP 应为 2，得 %d", distinct)
	}
	if len(groups) != 1 {
		t.Fatalf("应只有 1 组「共用出口」，得 %d 组", len(groups))
	}
	if groups[0].ExitIP != "39.91.80.16" || len(groups[0].URLs) != 2 {
		t.Fatalf("共用组内容不对：%+v", groups[0])
	}
	// 空 IP 的线路不能被归进任何一组（未知 ≠ 相同）。
	for _, u := range groups[0].URLs {
		if u == "http://10.0.0.3:8080" {
			t.Fatalf("未探到出口的线路被错误归入共用组")
		}
	}
}

// TestSharedExitOfURLs 反向索引：每个 URL 都能看到"还有谁跟我同 IP"。
func TestSharedExitOfURLs(t *testing.T) {
	groups := []egressExitGroup{{ExitIP: "1.1.1.1", URLs: []string{"a", "b", "c"}}}
	m := sharedExitOfURLs(groups)
	if len(m["a"]) != 2 || len(m["b"]) != 2 || len(m["c"]) != 2 {
		t.Fatalf("互相索引应各 2 条，得 %+v", m)
	}
	for u, others := range m {
		for _, o := range others {
			if o == u {
				t.Fatalf("%s 的共用列表里出现了自己", u)
			}
		}
	}
}

// TestDedupeProxiesByExit 分配前折叠：同 IP 只留一条代表线路。
//
// 三个断言分别钉住三种行为：
//  1. 同 IP 的两条线路只留一条（否则"分散"是假的）；
//  2. 代表优先选**平台线路**（用户线路是别人的出口，不该成为平台流量的承载）；
//  3. 未探到 IP 的线路保持独立（不能拿"未知"当"相同"）。
func TestDedupeProxiesByExit(t *testing.T) {
	plat := &EgressProxy{URL: "http://127.0.0.1:11080", Name: "平台家宽", LastIP: "39.91.80.16", LastOK: true, OwnerUserID: 0}
	user := &EgressProxy{URL: "http://127.0.0.1:21160", Name: "用户家宽", LastIP: "39.91.80.16（山东）", LastOK: true, OwnerUserID: 9916}
	other := &EgressProxy{URL: "http://10.0.0.2:8080", Name: "机房", LastIP: "8.8.8.8", LastOK: true}
	unknown := &EgressProxy{URL: "http://10.0.0.3:8080", Name: "未探活", LastIP: ""}

	kept, merged, warn := dedupeProxiesByExit([]*EgressProxy{user, plat, other, unknown}, map[string]int{})
	if len(kept) != 3 {
		t.Fatalf("折叠后应留 3 条（同 IP 折 1 + 8.8.8.8 + 未知），得 %d", len(kept))
	}
	var hasPlat, hasUser bool
	for _, p := range kept {
		if p == plat {
			hasPlat = true
		}
		if p == user {
			hasUser = true
		}
	}
	if hasUser || !hasPlat {
		t.Fatalf("同一出口的代表应选平台线路，实际 kept 含用户线路=%v 含平台=%v", hasUser, hasPlat)
	}
	if merged[user.URL] != plat.URL {
		t.Fatalf("merged 映射应为 %s → %s，得 %q", user.URL, plat.URL, merged[user.URL])
	}
	if len(warn) != 1 {
		t.Fatalf("应给 1 条合并说明，得 %v", warn)
	}
	// 用户线路被折时文案必须点出"不会提供额外的 IP"，否则管理员会以为丢了一条线。
	if !strings.Contains(warn[0], "不会提供额外的 IP") {
		t.Fatalf("合并说明缺少关键语义：%s", warn[0])
	}
}
