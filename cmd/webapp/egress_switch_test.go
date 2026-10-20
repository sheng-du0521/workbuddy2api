package main

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// 这组用例守的是「管理员切换号池出口时，普通用户仍能正常使用号池」（v0.31.58）。
//
// 两条不变量：
//  1. **切换必须立即、自洽地生效**：库里的默认出口改了，内存里的默认出口、
//     账号映射、出口异常表必须一起改 —— 任何"只改一半"都会让普通用户要么走旧出口，
//     要么被按旧默认误判成"出口异常"而整片 503。
//  2. **切换失败必须不改变现状**：不健康线路不能被分配、写库失败要回滚，
//     保证"要么全切、要么不动"。

// TestApplyReloadsDefaultImmediately apply 之后默认出口必须**立即**在进程内生效。
//
// 修复前的 bug：applyHomeEgress 写完 kv 只调 SyncAccounts()，而 SyncAccounts 里的
// `defEgress := p.defaultEgress` 读的是**内存旧值**。于是：
//   - ResolveEgress 仍返回旧默认 → 空出口账号继续走老路径（切换"没生效"）；
//   - egressBad 按旧默认评估 → 旧默认坏了就全池判坏（普通用户整片 503）。
func TestApplyReloadsDefaultImmediately(t *testing.T) {
	f := usApp(t)
	f.app.egressVerify = func(string) (string, error) { return "198.51.100.7", nil }
	tokP, err := f.app.egressAgentTokenGet()
	if err != nil {
		t.Fatalf("egressAgentTokenGet: %v", err)
	}
	usPoll(t, f, tokP, "", "plat-switch")

	mm := f.app.machinesLoad()[egressDefaultMachine]
	if mm == nil {
		t.Fatal("平台机器未登记")
	}
	url := egressHomeURL(mm.Ports.TunnelPort)

	// 切换前默认是空（= 直连）。
	if got := f.app.proxy.DefaultEgress(); got != "" {
		t.Fatalf("切换前默认应为空，得 %q", got)
	}

	res, err := f.app.applyHomeEgress(mm.Ports.TunnelPort, egressScope{Platform: true})
	if err != nil {
		t.Fatalf("applyHomeEgress: %v", err)
	}
	if !res.DefaultOK {
		t.Fatal("平台线路应用后应设置全局默认出口")
	}
	if got := f.app.proxy.DefaultEgress(); got != url {
		t.Fatalf("apply 后默认出口应立即生效：得 %q，期望 %q（只改库不改内存 = 切换不生效）", got, url)
	}
	// 空出口账号（普通用户的自挂号）解析结果必须指向新线路。
	for _, uid := range []string{"us-a-acct", "us-b-acct"} {
		if got := f.app.proxy.ResolveEgress(uid); got != url {
			t.Fatalf("%s 应解析到新默认出口 %q，得 %q", uid, url, got)
		}
	}
	// 影响面必须被统计出来（前端据此二次确认）。
	if res.Followers < 2 {
		t.Fatalf("应统计到跟随默认的账号（>=2），得 followers=%d", res.Followers)
	}
	if res.FollowerHosts < 2 {
		t.Fatalf("应统计到跟随默认的代挂号（>=2），得 follower_hosts=%d", res.FollowerHosts)
	}
}

// TestDefaultEgressBadUsesNormalizedURL 默认出口的线路状态必须用**规范化 URL** 匹配。
//
// 修复前的实现是 `lines[strings.TrimSpace(defEgress)]`，而 lines 的键是
// `egress.Validate` 规范化后的值（scheme 小写、去尾斜杠）。管理员把默认写成
// `HTTP://host:port/` 这类等价形式时查不到线路 → 线路已停用也不会传播到账号
// → 这些号照常被选中、从一个坏出口发请求。
func TestDefaultEgressBadUsesNormalizedURL(t *testing.T) {
	f := usApp(t)
	port := 21180
	url := egressHomeURL(port) // http://127.0.0.1:21180

	if err := f.store.UpsertEgressProxy(&EgressProxy{
		ID: "p-norm", Name: "规范化测试", URL: url,
		Enabled: false, // 已停用：跟随它的账号必须被判出口异常
	}); err != nil {
		t.Fatalf("UpsertEgressProxy: %v", err)
	}

	// 刻意写成等价但**非规范化**的形式。
	weird := "HTTP://127.0.0.1:21180/"
	if err := f.store.SetKV(kvEgressDefault, weird); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	if err := f.app.proxy.ApplyEgressChange(); err != nil {
		t.Fatalf("ApplyEgressChange: %v", err)
	}

	reason := f.app.proxy.EgressErrorForUID("us-a-acct")
	if reason == "" {
		t.Fatal("默认线路已停用，跟随默认的账号必须被判出口异常（否则会从坏出口发请求）")
	}
	if !strings.Contains(reason, "停用") {
		t.Fatalf("原因应指向线路停用，得 %q", reason)
	}
}

// TestAssignRejectsUnhealthyLine 连续探活失败的线路不能被分配。
//
// 绑上去的号会在下一次重建时被判出口异常、立刻退出平台选号 ——
// 管理员看到"分配成功"，普通用户看到整片 503。
func TestAssignRejectsUnhealthyLine(t *testing.T) {
	f := usApp(t)
	url := "http://127.0.0.1:21190"
	if err := f.store.UpsertEgressProxy(&EgressProxy{
		ID: "p-unhealthy", Name: "坏线", URL: url,
		Enabled: true, FailStreak: egressFailThreshold,
	}); err != nil {
		t.Fatalf("UpsertEgressProxy: %v", err)
	}

	code, raw := eaReq(t, f.mux, "POST", "/api/admin/egress/assign",
		`{"uids":["us-a-acct"],"proxy_url":"`+url+`"}`, f.adminCookie, "")
	if code != http.StatusConflict {
		t.Fatalf("分配不健康线路应 409，得 %d %s", code, raw)
	}
	if !strings.Contains(raw, "line_unhealthy") {
		t.Fatalf("应回 code=line_unhealthy，得 %.240s", raw)
	}
	acc, err := f.store.GetAccount("us-a-acct")
	if err != nil || acc == nil {
		t.Fatalf("GetAccount: %v", err)
	}
	if strings.TrimSpace(acc.EgressProxy) == url {
		t.Fatal("被拒绝的分配不得写库（否则账号已被绑到坏线路，只是回了错误）")
	}
}

// TestConcurrentEgressSwitchKeepsConsistent 并发切换后，内存与库必须一致且自洽。
//
// 不加 cfgMu 时，两个切换可能交错成"em 是新映射、bad 按旧默认算"，
// 对外表现为部分账号莫名被判出口异常 / 走错出口。
func TestConcurrentEgressSwitchKeepsConsistent(t *testing.T) {
	f := usApp(t)
	urls := []string{"http://127.0.0.1:21200", "http://127.0.0.1:21201"}
	for i, u := range urls {
		if err := f.store.UpsertEgressProxy(&EgressProxy{
			ID: "p-conc-" + string(rune('a'+i)), Name: "并发线", URL: u, Enabled: true,
		}); err != nil {
			t.Fatalf("UpsertEgressProxy: %v", err)
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = f.store.SetKV(kvEgressDefault, urls[i%2])
			if err := f.app.proxy.ApplyEgressChange(); err != nil {
				t.Errorf("ApplyEgressChange: %v", err)
			}
		}(i)
	}
	wg.Wait()

	kv, err := f.store.GetKV(kvEgressDefault)
	if err != nil {
		t.Fatalf("GetKV: %v", err)
	}
	want := strings.TrimSpace(kv)
	if got := f.app.proxy.DefaultEgress(); got != want {
		t.Fatalf("并发切换后内存默认出口 %q ≠ 库 %q", got, want)
	}
	// 两条线路都是 enabled/fail_streak=0，不应有任何账号被判出口异常。
	if bad := f.app.proxy.EgressBadUIDs(); len(bad) != 0 {
		t.Fatalf("健康线路不该产生出口异常，得 %v", bad)
	}
	for _, uid := range []string{"us-a-acct", "us-b-acct"} {
		if got := f.app.proxy.ResolveEgress(uid); got != want {
			t.Fatalf("%s 的出口 %q ≠ 默认 %q（映射与默认不一致）", uid, got, want)
		}
	}
}

// TestApplyVerifyFailureLeavesDefaultUntouched 切换失败必须**零改动**。
//
// 目标线路连不上时，apply 要在写任何配置之前就拒绝：默认出口保持原值、
// 线路不会被启用、账号映射不变。否则一次失败的切换会把全池推向一个坏出口。
func TestApplyVerifyFailureLeavesDefaultUntouched(t *testing.T) {
	f := usApp(t)
	const prev = "http://127.0.0.1:29999"
	if err := f.store.SetKV(kvEgressDefault, prev); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	if err := f.app.proxy.ApplyEgressChange(); err != nil {
		t.Fatalf("ApplyEgressChange: %v", err)
	}

	f.app.egressVerify = func(string) (string, error) { return "", errEgressNotUsable }
	tokP, _ := f.app.egressAgentTokenGet()
	usPoll(t, f, tokP, "", "plat-fail")
	mm := f.app.machinesLoad()[egressDefaultMachine]
	if mm == nil {
		t.Fatal("平台机器未登记")
	}

	if _, err := f.app.applyHomeEgress(mm.Ports.TunnelPort, egressScope{Platform: true}); !errors.Is(err, errEgressNotUsable) {
		t.Fatalf("应返回 errEgressNotUsable，得 %v", err)
	}
	if got := f.app.proxy.DefaultEgress(); got != prev {
		t.Fatalf("切换失败后默认出口必须保持 %q，得 %q", prev, got)
	}
	// 线路也不该被启用（apply 在验证之后才登记/启用）。
	if p := f.app.egressProxyByURL(egressHomeURL(mm.Ports.TunnelPort)); p != nil && p.Enabled {
		t.Fatal("验证失败的线路不应被启用")
	}
}
