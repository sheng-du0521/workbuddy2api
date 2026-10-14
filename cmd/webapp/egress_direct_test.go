// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/egress"
)

// TestValidateAccountEgress 账号级校验：接受 direct 与空，拒绝非法代理与常见误写。
func TestValidateAccountEgress(t *testing.T) {
	ok := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"   ", ""},
		{"direct", egress.Direct},
		{"DIRECT", egress.Direct},   // 大小写不敏感
		{" direct ", egress.Direct}, // 前后空白容忍
		{"http://1.2.3.4:8080", "http://1.2.3.4:8080"},
		{"socks5://1.2.3.4:1080", "socks5://1.2.3.4:1080"},
	}
	for _, c := range ok {
		got, err := egress.ValidateAccountEgress(c.in)
		if err != nil {
			t.Errorf("ValidateAccountEgress(%q) 应通过，得 %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ValidateAccountEgress(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}

	// 想直连却写成别的字符串 → 必须拒绝并提示正确写法。
	// 若默默当成非法代理，账号会被判「出口异常」而不可用 —— 用户完全摸不着头脑。
	for _, s := range []string{"none", "null", "off", "-", "direct://"} {
		_, err := egress.ValidateAccountEgress(s)
		if err == nil {
			t.Errorf("ValidateAccountEgress(%q) 应被拒绝", s)
			continue
		}
		if !strings.Contains(err.Error(), "direct") {
			t.Errorf("ValidateAccountEgress(%q) 的提示应引导使用 direct，得 %q", s, err.Error())
		}
	}
	// 真非法代理仍按原样报错。
	if _, err := egress.ValidateAccountEgress("socks5h://1.2.3.4:1080"); err == nil {
		t.Error("socks5h 应被拒绝")
	}
}

// TestDirectNotInheritGlobalDefault 直连的核心语义：**覆盖**全局默认代理。
//
// 这是「支持直连」要解决的原始问题 —— 没有这个哨兵时，一旦管理员设了全局默认
// 代理，任何账号都无法单独直连（留空只会继承默认）。
func TestDirectNotInheritGlobalDefault(t *testing.T) {
	store, px, _ := newTestApp(t,
		&Account{UID: "d", Realm: "cn", AccessToken: "a", EgressProxy: egress.Direct},
		&Account{UID: "inherit", Realm: "cn", AccessToken: "a"},
	)
	defer store.Close()
	if err := store.SetKV(kvEgressDefault, "http://10.0.0.9:8080"); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	px.ReloadSelection()
	if err := px.SyncAccounts(); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}

	if got := px.ResolveEgress("d"); got != egress.Direct {
		t.Fatalf("显式直连的账号应解析为 direct（不被默认代理覆盖），得 %q", got)
	}
	if got := px.ResolveEgress("inherit"); got != "http://10.0.0.9:8080" {
		t.Fatalf("未指定账号应继承全局默认，得 %q", got)
	}
	// 直连账号用真直连客户端（≠ 共享客户端，后者保留环境代理）。
	cd, err := px.clientFor("d")
	if err != nil {
		t.Fatalf("直连不应报错: %v", err)
	}
	if cd == px.uc.HTTP {
		t.Fatal("显式直连应使用专用直连客户端（Proxy=nil），而不是保留环境代理的共享客户端")
	}
	tr, ok := cd.Transport.(*http.Transport)
	if !ok {
		t.Fatal("Transport 类型不符")
	}
	if tr.Proxy != nil {
		t.Fatal("直连客户端的 Proxy 必须为 nil（既不走环境代理也不走 URL 代理）")
	}
	// 继承默认的账号应走该代理的客户端。
	ci, err := px.clientFor("inherit")
	if err != nil {
		t.Fatalf("继承默认不应报错: %v", err)
	}
	if ci == px.uc.HTTP || ci == cd {
		t.Fatal("继承默认的账号应使用该代理的专用客户端")
	}
}

// TestDirectNotBadEgress 直连不能被判为「出口异常」。
//
// 回归护栏：校验循环若忘了跳过 direct，会把直连账号标成非法代理
// （因为 "direct" 不是合法 URL），进而把它排除出平台选号 —— 直连反而不可用。
func TestDirectNotBadEgress(t *testing.T) {
	store, px, _ := newTestApp(t,
		&Account{UID: "d", Realm: "cn", AccessToken: "a", EgressProxy: egress.Direct},
	)
	defer store.Close()
	if err := px.SyncAccounts(); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}
	if got := px.EgressErrorForUID("d"); got != "" {
		t.Fatalf("直连不应被判出口异常，得 %q", got)
	}
	if bad := px.EgressBadUIDs(); len(bad) != 0 {
		t.Fatalf("不应有出口异常账号，得 %v", bad)
	}
	// 必须仍能参与平台选号（直连是合法状态，不是错误状态）。
	if !px.allowsSelection("d", pickScope{}) {
		t.Fatal("直连账号应可参与平台选号")
	}
}

// TestDirectImmuneToBadGlobalDefault 全局默认代理配错时，直连账号不受牵连。
func TestDirectImmuneToBadGlobalDefault(t *testing.T) {
	store, px, _ := newTestApp(t,
		&Account{UID: "d", Realm: "cn", AccessToken: "a", EgressProxy: egress.Direct},
		&Account{UID: "inherit", Realm: "cn", AccessToken: "a"},
	)
	defer store.Close()
	_ = store.SetKV(kvEgressDefault, "socks5h://1.2.3.4:1080") // 非法
	px.ReloadSelection()
	if err := px.SyncAccounts(); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}
	if got := px.EgressErrorForUID("d"); got != "" {
		t.Fatalf("直连账号不应被全局默认的错误牵连，得 %q", got)
	}
	if got := px.EgressErrorForUID("inherit"); !strings.Contains(got, "全局默认出口代理") {
		t.Fatalf("继承者应被标记，得 %q", got)
	}
}

// TestAssignDirectMode 批量设直连：不依赖线路池（池空也能用），写库为 direct 哨兵。
//
// 「不依赖线路池」是刻意的顺序设计：先前把「池为空」检查放在前面，会导致
// 池子空时无法把账号改回直连 —— 管理员被锁在一个自己退不出的状态里。
func TestAssignDirectMode(t *testing.T) {
	store, px, a := newTestApp(t,
		&Account{UID: "u1", Realm: "cn", AccessToken: "a", EgressProxy: "http://1.2.3.4:8080"},
		&Account{UID: "u2", Realm: "cn", AccessToken: "a", EgressProxy: "http://5.6.7.8:8080"},
	)
	defer store.Close()
	_ = px.SyncAccounts()

	// 池故意为空。
	req := httptest.NewRequest(http.MethodPost, "/api/admin/egress/assign", strings.NewReader(`{"all":true,"direct":true}`))
	rec := httptest.NewRecorder()
	a.handleEgressAssign(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("池空时设直连应成功（200），得 %d：%s", rec.Code, rec.Body.String())
	}
	resp := decodeJSON(t, rec.Body.Bytes())
	if resp["assigned"].(float64) != 2 {
		t.Fatalf("应设 2 个账号，得 %v", resp["assigned"])
	}
	for _, uid := range []string{"u1", "u2"} {
		acc, _ := store.GetAccount(uid)
		if !egress.IsDirect(acc.EgressProxy) {
			t.Fatalf("%s 应被写为 direct，得 %q", uid, acc.EgressProxy)
		}
		if got := px.EgressErrorForUID(uid); got != "" {
			t.Fatalf("%s 不应是出口异常，得 %q", uid, got)
		}
	}
}

// TestAssignClearMode 清空模式：写空串（回到「未指定」）。
func TestAssignClearMode(t *testing.T) {
	store, _, a := newTestApp(t,
		&Account{UID: "u1", Realm: "cn", AccessToken: "a", EgressProxy: egress.Direct},
	)
	defer store.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/egress/assign", strings.NewReader(`{"all":true,"clear":true}`))
	rec := httptest.NewRecorder()
	a.handleEgressAssign(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	acc, _ := store.GetAccount("u1")
	if acc.EgressProxy != "" {
		t.Fatalf("清空后应为空串，得 %q", acc.EgressProxy)
	}
}

// TestAssignProxyModeStillNeedsPool 分配代理模式仍需线路池（避免误以为「什么都不做也算成功」）。
func TestAssignProxyModeStillNeedsPool(t *testing.T) {
	store, _, a := newTestApp(t, &Account{UID: "u1", Realm: "cn", AccessToken: "a"})
	defer store.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/egress/assign", strings.NewReader(`{"all":true}`))
	rec := httptest.NewRecorder()
	a.handleEgressAssign(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("池空时分配代理应 400，得 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "设为直连") {
		t.Fatalf("错误提示应引导到「设为直连」，得 %s", rec.Body.String())
	}
}

// TestOverviewDistinguishesDirectKinds 看板区分「显式直连」与「未指定」（含默认代理在时的继承）。
func TestOverviewDistinguishesDirectKinds(t *testing.T) {
	store, px, a := newTestApp(t,
		&Account{UID: "exp", Realm: "cn", AccessToken: "a", EgressProxy: egress.Direct, ConsumeGroup: cgProxy},
		&Account{UID: "unspec", Realm: "cn", AccessToken: "a", ConsumeGroup: cgProxy},
	)
	defer store.Close()
	_ = px.SyncAccounts()

	// 无全局默认：两者都实际直连。
	v, err := a.egressOverview()
	if err != nil {
		t.Fatalf("egressOverview: %v", err)
	}
	if v.DirectAccountCount != 2 || v.DirectExplicit != 1 {
		t.Fatalf("应 2 个直连（其中 1 个显式），得 count=%d explicit=%d", v.DirectAccountCount, v.DirectExplicit)
	}
	if v.Unassigned != 2 {
		t.Fatalf("两个都参与消耗且直连，裸奔数应为 2，得 %d", v.Unassigned)
	}

	// 设了全局默认后：未指定的账号不再算直连（改走默认代理），只剩显式的那个。
	if err := store.SetKV(kvEgressDefault, "http://10.1.1.1:8080"); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	px.ReloadSelection()
	v2, err := a.egressOverview()
	if err != nil {
		t.Fatalf("egressOverview: %v", err)
	}
	if v2.DirectAccountCount != 1 || v2.DirectExplicit != 1 {
		t.Fatalf("有默认代理后应只剩 1 个直连（显式），得 count=%d explicit=%d", v2.DirectAccountCount, v2.DirectExplicit)
	}
	if v2.Unassigned != 1 {
		t.Fatalf("裸奔数应降为 1，得 %d", v2.Unassigned)
	}
	if v2.DefaultEgress != "http://10.1.1.1:8080" {
		t.Fatalf("应在看板回显全局默认代理，得 %q", v2.DefaultEgress)
	}
	// 未指定账号应归到默认代理这一条出口下。
	var found bool
	for _, p := range v2.Proxies {
		if p.URL == "http://10.1.1.1:8080" && p.AccountCount == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("未指定账号应统计到默认代理出口下，得 %+v", v2.Proxies)
	}
}

// TestSetEgressHandlerDirect 单账号接口接受 direct 并回本机出口 IP。
func TestSetEgressHandlerDirect(t *testing.T) {
	store, px, a := newTestApp(t, &Account{UID: "u1", Realm: "cn", AccessToken: "a", EgressProxy: "http://1.2.3.4:8080"})
	defer store.Close()
	_ = px.SyncAccounts()

	req := httptest.NewRequest(http.MethodPost, "/api/admin/accounts/u1/egress", strings.NewReader(`{"proxy":"direct"}`))
	req.SetPathValue("uid", "u1")
	rec := httptest.NewRecorder()
	a.handleAccountEgress(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	resp := decodeJSON(t, rec.Body.Bytes())
	if resp["direct"] != true {
		t.Fatalf("响应应标明 direct=true，得 %v", resp)
	}
	acc, _ := store.GetAccount("u1")
	if !egress.IsDirect(acc.EgressProxy) {
		t.Fatalf("应写入 direct，得 %q", acc.EgressProxy)
	}
	if got := px.EgressErrorForUID("u1"); got != "" {
		t.Fatalf("直连不应是出口异常，得 %q", got)
	}
}
