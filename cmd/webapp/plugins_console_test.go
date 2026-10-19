package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

// 这组用例守的是「管理员在**用户控制台**里能拉起代理」这条产品需求（v0.31.57）。
//
// 背景：v0.31.53 把出口收敛成平台能力时，顺手把插件的 Audience 改成 admin，
// 但 `/api/plugins`（控制台用的接口）仍然无条件按 user 视角过滤 ——
// 于是**管理员登录控制台也看不到那个插件**。入口在注册表里存在、在界面上不存在，
// 用户报的正是"控制台里没有拉起代理的插件"。
//
// 所以这里同时守两头：
//   - 管理员：能看到、能开关（平台级）、能拉起；
//   - 普通用户：看不到、调接口 403。
//
// 只守一头是不够的：把 audience 放宽成 both 能让管理员看到，
// 但普通用户也会看到 —— 那是把已经下线的能力又开回去了。

// pluginIDsOf 把列表响应转成 id → 条目，便于断言。
func pluginIDsOf(t *testing.T, raw string) map[string]map[string]any {
	t.Helper()
	var d struct {
		Plugins []map[string]any `json:"plugins"`
	}
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatalf("解析插件列表失败：%v body=%.200s", err, raw)
	}
	out := make(map[string]map[string]any, len(d.Plugins))
	for _, p := range d.Plugins {
		if id, _ := p["id"].(string); id != "" {
			out[id] = p
		}
	}
	return out
}

// TestAdminConsoleSeesPlatformPlugin 管理员在控制台能看到「平台直属出口」。
func TestAdminConsoleSeesPlatformPlugin(t *testing.T) {
	_, _, mux, adminCookie, memberCookie := eaApp(t)

	// 管理员：必须出现，且 scope=platform（启停是平台级）。
	code, raw := eaReq(t, mux, "GET", "/api/plugins", "", adminCookie, "")
	if code != http.StatusOK {
		t.Fatalf("管理员读插件列表应 200，得 %d %s", code, raw)
	}
	adm := pluginIDsOf(t, raw)
	p, ok := adm[pluginIDHostEgress]
	if !ok {
		t.Fatalf("管理员在控制台看不到「平台直属出口」：%v", pluginKeys(adm))
	}
	if p["scope"] != "platform" {
		t.Fatalf("平台插件的 scope 必须是 platform，得 %v", p["scope"])
	}
	if p["kind"] != "egress" {
		t.Fatalf("kind 应为 egress，得 %v", p["kind"])
	}
	if p["active"] != true {
		t.Fatalf("默认应启用，得 active=%v", p["active"])
	}
	// 控制台里也必须有 user 插件（不能因为加了 admin 就把 user 挤掉）。
	if _, ok := adm["health-check"]; !ok {
		t.Fatalf("管理员控制台的插件列表丢了普通插件：%v", pluginKeys(adm))
	}

	// 普通用户：**不能**出现 —— 出口能力已下线，放宽可见范围不等于开回去。
	code, raw = eaReq(t, mux, "GET", "/api/plugins", "", memberCookie, "")
	if code != http.StatusOK {
		t.Fatalf("普通用户读插件列表应 200，得 %d %s", code, raw)
	}
	if _, ok := pluginIDsOf(t, raw)[pluginIDHostEgress]; ok {
		t.Fatal("普通用户不该看到平台出口插件（用户端代理已下线）")
	}
}

// TestAdminCanTogglePlatformPluginFromConsole 管理员在控制台开关平台插件 = 平台级。
func TestAdminCanTogglePlatformPluginFromConsole(t *testing.T) {
	store, a, mux, adminCookie, memberCookie := eaApp(t)

	// 管理员关掉 → 写的是**平台级** kv，不是自己的用户偏好。
	code, raw := eaReq(t, mux, "POST", "/api/plugins/"+pluginIDHostEgress+"/enabled",
		`{"enabled":false}`, adminCookie, "")
	if code != http.StatusOK {
		t.Fatalf("管理员应能关闭平台插件，得 %d %s", code, raw)
	}
	if a.pluginEnabled(pluginIDHostEgress) {
		t.Fatal("关闭后平台级开关仍为启用 —— 写错了键（写到用户偏好里就等于没生效）")
	}
	if v, err := store.GetKV(pluginUserOffKey(1, pluginIDHostEgress)); err == nil && v == "1" {
		t.Fatal("平台插件不该写进某个用户的偏好（管理员 id 恰好是 1，这里是防串键）")
	}

	// 普通用户不能动平台开关：直接调接口也要被拒。
	code, raw = eaReq(t, mux, "POST", "/api/plugins/"+pluginIDHostEgress+"/enabled",
		`{"enabled":true}`, memberCookie, "")
	if code != http.StatusForbidden {
		t.Fatalf("普通用户启停平台插件应 403，得 %d %s", code, raw)
	}
	if a.pluginEnabled(pluginIDHostEgress) {
		t.Fatal("普通用户被拒后平台开关不该变化（403 不应产生写入）")
	}

	// 管理员再开回来。
	code, raw = eaReq(t, mux, "POST", "/api/plugins/"+pluginIDHostEgress+"/enabled",
		`{"enabled":true}`, adminCookie, "")
	if code != http.StatusOK {
		t.Fatalf("管理员应能启用平台插件，得 %d %s", code, raw)
	}
	if !a.pluginEnabled(pluginIDHostEgress) {
		t.Fatal("启用后平台级开关仍为停用")
	}
}

// TestEgressBlockedWhenPluginDisabled 停用插件后，出口端点**真的**拒绝执行。
//
// 这是"可用性校验逻辑"的核心：此前停用只让按钮变灰，绕过界面直接调接口
// 仍然能把隧道拉起来 —— 那个开关是假的。守卫必须落在服务端。
func TestEgressBlockedWhenPluginDisabled(t *testing.T) {
	_, a, mux, adminCookie, _ := eaApp(t)
	if err := a.setPluginEnabled(pluginIDHostEgress, false); err != nil {
		t.Fatalf("setPluginEnabled: %v", err)
	}

	cases := []struct {
		method, path string
		body         string
		want         int
	}{
		{"GET", "/api/egress/home", "", http.StatusForbidden},
		{"POST", "/api/egress/home/pull", `{}`, http.StatusForbidden},
		{"POST", "/api/egress/home/apply", `{}`, http.StatusForbidden},
		{"POST", "/api/egress/home/release", `{}`, http.StatusForbidden},
		{"POST", "/api/egress/home/repair", `{}`, http.StatusForbidden},
		{"GET", "/api/egress/home/install", "", http.StatusForbidden},
	}
	for _, c := range cases {
		code, raw := eaReq(t, mux, c.method, c.path, c.body, adminCookie, "")
		if code != c.want {
			t.Fatalf("%s %s 停用插件时应 %d，得 %d %s", c.method, c.path, c.want, code, raw)
		}
		if wantCode := "plugin_disabled"; !contains(raw, wantCode) {
			t.Fatalf("%s %s 应回 code=%s，得 %.200s", c.method, c.path, wantCode, raw)
		}
	}

	// 重新启用 → 状态恢复可读（200）。这条同时证明 gate 不是永久拒。
	if err := a.setPluginEnabled(pluginIDHostEgress, true); err != nil {
		t.Fatalf("setPluginEnabled: %v", err)
	}
	if code, raw := eaReq(t, mux, "GET", "/api/egress/home", "", adminCookie, ""); code != http.StatusOK {
		t.Fatalf("启用后读状态应 200，得 %d %s", code, raw)
	}
}

// pluginKeys 只为报错信息里能看到"到底拿到了哪几个插件"。
func pluginKeys(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
