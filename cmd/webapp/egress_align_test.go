// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 账号代理 ↔ 出口代理「一致化」的回归测试（v0.31.33）。
//
// 要守住的就一件事：**账号上出现的出口 URL，一定能在线路池里找到同一条线路**。
// 否则账号那侧没有任何地方能回答"这个号实际从哪个 IP 出去"，而线路池的
// "N 条线路 / M 个 IP" 也漏掉它 —— 两边各说各话，正是这条需求要消灭的状态。
//
// 三条断言：
//
//	① 写入即登记（管理端单号改出口 → 池里立刻有这条线，归属 = 号主）；
//	② 规范化（`HTTP://A:1/` 与 `http://a:1` 是同一条；账号上存规范写法）；
//	③ 对齐能修历史数据（池外出口补登记、同 URL 不同写法合并、孤儿清零）。
package main

import (
	"testing"
	"time"
)

// eaAccount 造一个账号（简化字段）。
func eaAccount(t *testing.T, s *Store, uid string, owner int64) {
	t.Helper()
	acc := &Account{
		UID: uid, Nickname: uid, Realm: "cn",
		AccessToken: "t", RefreshToken: "r",
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
		OwnerUserID: owner, PoolState: "",
	}
	if err := s.UpsertAccount(acc); err != nil {
		t.Fatalf("UpsertAccount %s: %v", uid, err)
	}
}

// TestSetAccountEgressRegistersLine 写入账号出口时必须同时登记进线路池。
func TestSetAccountEgressRegistersLine(t *testing.T) {
	store, _, a := newTestApp(t)
	defer store.Close()
	eaAccount(t, store, "al-1", 7)

	// 池里没有这条线；管理员直接填自定义代理。
	norm, err := a.setAccountEgress("al-1", "HTTP://Proxy.Example.COM:3128/")
	if err != nil {
		t.Fatalf("setAccountEgress: %v", err)
	}
	if norm != "http://proxy.example.com:3128" {
		t.Fatalf("应返回规范化值，得 %q", norm)
	}
	acc, _ := store.GetAccount("al-1")
	if acc.EgressProxy != norm {
		t.Fatalf("账号应存规范写法 %q，得 %q", norm, acc.EgressProxy)
	}
	// 池里必须出现同一条（规范 URL 精确匹配）。
	list, err := store.ListEgressProxies()
	if err != nil {
		t.Fatalf("ListEgressProxies: %v", err)
	}
	var hit *EgressProxy
	for _, p := range list {
		if p.URL == norm {
			hit = p
		}
	}
	if hit == nil {
		t.Fatalf("账号出口未登记进线路池，池内只有 %d 条", len(list))
	}
	if hit.OwnerUserID != 7 {
		t.Fatalf("自动登记的线路应归该账号号主（7），得 %d", hit.OwnerUserID)
	}

	// 幂等：再写一次不会多出一条。
	if _, err := a.setAccountEgress("al-1", norm); err != nil {
		t.Fatalf("重复写入: %v", err)
	}
	list2, _ := store.ListEgressProxies()
	if len(list2) != len(list) {
		t.Fatalf("重复写入不该新增线路：%d → %d", len(list), len(list2))
	}

	// 直连 / 空串不是线路，不该往池里塞东西。
	if _, err := a.setAccountEgress("al-1", "direct"); err != nil {
		t.Fatalf("设为直连: %v", err)
	}
	if _, err := a.setAccountEgress("al-1", ""); err != nil {
		t.Fatalf("清空: %v", err)
	}
	list3, _ := store.ListEgressProxies()
	if len(list3) != len(list2) {
		t.Fatalf("direct/空串不该登记线路：%d → %d", len(list2), len(list3))
	}
}

// TestConvergeAccountEgress 历史脏数据一次性对齐：合并重复 + 补登记 + 改写账号。
func TestConvergeAccountEgress(t *testing.T) {
	store, _, a := newTestApp(t)
	defer store.Close()

	// 同一个代理的两种写法 —— 事故原型：各探活、各计数，出口 IP 却是同一个。
	dupA := &EgressProxy{ID: "dup-a", Name: "A 写法", URL: "HTTP://1.2.3.4:8080/",
		Enabled: true, CreatedAt: 100}
	dupB := &EgressProxy{ID: "dup-b", Name: "B 写法", URL: "http://1.2.3.4:8080",
		Enabled: true, CreatedAt: 200}
	for _, p := range []*EgressProxy{dupA, dupB} {
		if err := store.UpsertEgressProxy(p); err != nil {
			t.Fatalf("UpsertEgressProxy %s: %v", p.ID, err)
		}
	}
	// 一个账号绑在"被合并掉"的写法上；另一个账号的出口根本不在池里。
	eaAccount(t, store, "cv-1", 0)
	eaAccount(t, store, "cv-2", 3)
	// ⚠️ 必须**直接写库**模拟"规范化之前留下的历史数据"：
	// `SetAccountEgress` 现在会在写库前规范化，用它造数据是造不出脏数据的
	// （测试通过 = 测了个寂寞）。
	if _, err := store.db.Exec(`UPDATE accounts SET egress_proxy=? WHERE uid=?`,
		"HTTP://1.2.3.4:8080/", "cv-1"); err != nil {
		t.Fatalf("seed cv-1: %v", err)
	}
	if err := store.SetAccountEgress("cv-2", "http://9.9.9.9:3123"); err != nil {
		t.Fatalf("SetAccountEgress cv-2: %v", err)
	}

	if n := a.orphanAccountEgress(); n != 1 {
		t.Fatalf("对齐前应有 1 条池外出口（cv-2），得 %d", n)
	}

	res, err := a.convergeAccountEgress(false) // 不探活：单测不打外网
	if err != nil {
		t.Fatalf("converge: %v", err)
	}
	if res.MergedLines != 1 {
		t.Fatalf("应合并 1 条重复线路，得 %+v", res)
	}
	if res.RegisteredLines != 1 {
		t.Fatalf("应补登记 1 条（cv-2 的出口），得 %+v", res)
	}
	if res.RewrittenAccounts != 1 {
		t.Fatalf("应改写 1 个账号（cv-1 的大小写/尾斜杠），得 %+v", res)
	}
	if res.Orphans != 0 {
		t.Fatalf("对齐后不该还有池外出口，得 %+v", res)
	}

	// 池里只剩一条 1.2.3.4 的线路（保留创建更早的那条），且 cv-2 的线路也在。
	list, _ := store.ListEgressProxies()
	byURL := map[string]*EgressProxy{}
	for _, p := range list {
		byURL[p.URL] = p
	}
	if _, ok := byURL["http://1.2.3.4:8080"]; !ok {
		t.Fatalf("应留下规范写法的线路，得 %v", byURL)
	}
	if _, ok := byURL["HTTP://1.2.3.4:8080/"]; ok {
		t.Fatal("非规范写法的重复线路应被删除")
	}
	if _, ok := byURL["http://9.9.9.9:3123"]; !ok {
		t.Fatal("cv-2 的出口应被补登记进池")
	}
	// 账号侧全部是规范写法。
	if acc, _ := store.GetAccount("cv-1"); acc.EgressProxy != "http://1.2.3.4:8080" {
		t.Fatalf("账号应改写为规范写法，得 %q", acc.EgressProxy)
	}
	if acc, _ := store.GetAccount("cv-2"); acc.EgressProxy != "http://9.9.9.9:3123" {
		t.Fatalf("cv-2 出口不该被改坏，得 %q", acc.EgressProxy)
	}
	// 幂等：再跑一次什么都不该改。
	res2, err := a.convergeAccountEgress(false)
	if err != nil {
		t.Fatalf("converge 第二次: %v", err)
	}
	if res2.MergedLines+res2.RegisteredLines+res2.RewrittenAccounts != 0 {
		t.Fatalf("第二次对齐应无改动，得 %+v", res2)
	}
	if n := a.orphanAccountEgress(); n != 0 {
		t.Fatalf("对齐后孤儿应为 0，得 %d", n)
	}
}
