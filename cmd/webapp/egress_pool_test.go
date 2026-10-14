// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/egress"
)

// decodeJSON 解析测试响应体（失败即致命，避免后续断言用到零值而误判）。
func decodeJSON(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("响应不是合法 JSON：%v（%s）", err, string(raw))
	}
	return out
}

// mkOwnerAccounts 造 n 个属于 owner 的账号。
func mkOwnerAccounts(owner int64, n int, prefixUID string) []*Account {
	out := make([]*Account, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, &Account{
			UID:         prefixUID + "-" + string(rune('a'+i)),
			Nickname:    prefixUID + string(rune('a'+i)),
			OwnerUserID: owner,
		})
	}
	return out
}

// mkProxies 造 n 条可用线路（URL 各自不同）。
func mkProxies(n int) []*EgressProxy {
	out := make([]*EgressProxy, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, &EgressProxy{
			ID:      string(rune('p' + i)),
			Name:    "线路" + string(rune('1'+i)),
			URL:     "http://10.0.0." + string(rune('1'+i)) + ":8080",
			Enabled: true,
		})
	}
	return out
}

// TestPlanAssignIsolateOwner 核心约束：同一号主的账号不得共用出口。
func TestPlanAssignIsolateOwner(t *testing.T) {
	targets := mkOwnerAccounts(7, 2, "u")
	proxies := mkProxies(2)
	plan, warns := planEgressAssignment(targets, proxies, 10, true)

	if len(plan) != 2 {
		t.Fatalf("应分配 2 个账号，得 %d", len(plan))
	}
	if plan[0].Proxy == "" || plan[1].Proxy == "" {
		t.Fatalf("两个账号都应分到线路，得 %+v", plan)
	}
	if plan[0].Proxy == plan[1].Proxy {
		t.Fatalf("同一号主的两个账号不应共用出口，都分到了 %s", plan[0].Proxy)
	}
	if len(warns) != 0 {
		t.Fatalf("线路够用不应有 warning，得 %v", warns)
	}
}

// TestPlanAssignMaxPerProxy 容量上限：线路够多时应严格遵守同出口账号数上限。
func TestPlanAssignMaxPerProxy(t *testing.T) {
	// 三个不同号主（isolate 不构成约束），一条线路，上限 3。
	targets := append(append(mkOwnerAccounts(1, 1, "a"), mkOwnerAccounts(2, 1, "b")...), mkOwnerAccounts(3, 1, "c")...)
	plan, _ := planEgressAssignment(targets, mkProxies(1), 3, true)

	per := map[string]int{}
	for _, it := range plan {
		per[it.Proxy]++
	}
	if per["http://10.0.0.1:8080"] != 3 {
		t.Fatalf("3 个账号应落在同一条线路且不超上限 3，得 %v", per)
	}

	// 再加一个号主（第 4 个账号）→ 必须超载，因为只有一条线路。
	targets = append(targets, mkOwnerAccounts(4, 1, "d")...)
	plan2, _ := planEgressAssignment(targets, mkProxies(1), 3, true)
	if len(plan2) != 4 {
		t.Fatalf("所有账号都应分到线路（宁可超载也不能留空），得 %d", len(plan2))
	}
	for _, it := range plan2 {
		if it.Proxy == "" {
			t.Fatalf("不应有账号被留空，得 %+v", it)
		}
	}
}

// TestPlanAssignOwnerOverCapacity 号主账号数多于线路数：
// 隔离优先于容量 —— 应放宽容量并给出明确 warning，而不是让同号主挤在一个出口。
func TestPlanAssignOwnerOverCapacity(t *testing.T) {
	targets := mkOwnerAccounts(7, 4, "u")
	proxies := mkProxies(2)
	plan, warns := planEgressAssignment(targets, proxies, 3, true)

	if len(warns) == 0 {
		t.Fatal("号主账号数超过线路数时应给出 warning（不静默降级）")
	}
	if !strings.Contains(warns[0], "无法完全隔离") {
		t.Fatalf("warning 应说明隔离无法两全，得 %v", warns)
	}
	// 应尽量分散：两条线路各 2 个。
	per := map[string]int{}
	for _, it := range plan {
		per[it.Proxy]++
	}
	if len(per) != 2 {
		t.Fatalf("应使用全部 2 条线路，得 %v", per)
	}
	for url, n := range per {
		if n != 2 {
			t.Fatalf("应均衡分散为 2/2，得 %s=%d（%v）", url, n, per)
		}
	}
}

// TestPlanAssignDeterministic 同输入必得同输出：管理员要能核对方案，
// 也便于回归比较。
func TestPlanAssignDeterministic(t *testing.T) {
	targets := append(mkOwnerAccounts(1, 2, "a"), mkOwnerAccounts(2, 3, "b")...)
	proxies := mkProxies(3)
	p1, _ := planEgressAssignment(targets, proxies, 2, true)
	p2, _ := planEgressAssignment(targets, proxies, 2, true)
	if len(p1) != len(p2) {
		t.Fatalf("长度应一致：%d vs %d", len(p1), len(p2))
	}
	for i := range p1 {
		if p1[i] != p2[i] {
			t.Fatalf("第 %d 项不一致：%+v vs %+v", i, p1[i], p2[i])
		}
	}
}

// TestPlanAssignDisableIsolate 关闭隔离时应允许同号主共用出口。
func TestPlanAssignDisableIsolate(t *testing.T) {
	targets := mkOwnerAccounts(7, 2, "u")
	// 上限设 5 且只有一条线路：不隔离时会都落在同一条。
	plan, _ := planEgressAssignment(targets, mkProxies(1), 5, false)
	if len(plan) != 2 || plan[0].Proxy != plan[1].Proxy {
		t.Fatalf("关闭隔离时应可共用出口，得 %+v", plan)
	}
}

// TestEgressProxyStoreCRUD 池的持久化与唯一约束。
func TestEgressProxyStoreCRUD(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "egress.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	p := &EgressProxy{Name: "香港 01", URL: "http://1.2.3.4:8080", Region: "HK", Enabled: true}
	if err := store.UpsertEgressProxy(p); err != nil {
		t.Fatalf("UpsertEgressProxy: %v", err)
	}
	if p.ID == "" {
		t.Fatal("应自动生成 ID")
	}

	list, err := store.ListEgressProxies()
	if err != nil || len(list) != 1 {
		t.Fatalf("应有 1 条，得 %d（%v）", len(list), err)
	}
	if list[0].Name != "香港 01" || !list[0].Enabled {
		t.Fatalf("字段不一致：%+v", list[0])
	}

	// 同 URL 再录一次 → 唯一索引必须拦住（否则分配算法会把「同一个出口」当成两条）。
	dup := &EgressProxy{Name: "重复", URL: "http://1.2.3.4:8080", Enabled: true}
	if err := store.UpsertEgressProxy(dup); err == nil {
		t.Fatal("重复 URL 应被唯一索引拒绝")
	}

	// 更新（同 ID）应成功且不新增。
	p.Name = "香港 01 改名"
	if err := store.UpsertEgressProxy(p); err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	list, _ = store.ListEgressProxies()
	if len(list) != 1 || list[0].Name != "香港 01 改名" {
		t.Fatalf("更新后应仍为 1 条且改名生效：%+v", list)
	}

	// 探活结果：失败累计 streak，成功清零并记 IP。
	if err := store.RecordEgressCheck(p.ID, false, "", "boom"); err != nil {
		t.Fatalf("RecordEgressCheck: %v", err)
	}
	got, _ := store.GetEgressProxy(p.ID)
	if got.FailStreak != 1 || got.LastOK {
		t.Fatalf("失败应记 streak=1 且 last_ok=false，得 %+v", got)
	}
	if err := store.RecordEgressCheck(p.ID, true, "203.0.113.9", ""); err != nil {
		t.Fatalf("RecordEgressCheck: %v", err)
	}
	got, _ = store.GetEgressProxy(p.ID)
	if got.FailStreak != 0 || !got.LastOK || got.LastIP != "203.0.113.9" {
		t.Fatalf("成功应清零并记 IP，得 %+v", got)
	}

	if err := store.DeleteEgressProxy(p.ID); err != nil {
		t.Fatalf("DeleteEgressProxy: %v", err)
	}
	if list, _ := store.ListEgressProxies(); len(list) != 0 {
		t.Fatalf("删除后应为空，得 %d", len(list))
	}
}

// TestEgressBadFromSync 同步期校验：绕过接口直接写库的非法代理也要被发现。
//
// 这是「静默直连」的第二道闸 —— 不管代理值从哪来（接口、直接改库、历史脏数据），
// 只要进池就必须被校验，否则运行期就会带着坏代理出去。
func TestEgressBadFromSync(t *testing.T) {
	store, px, _ := newTestApp(t,
		&Account{UID: "good", Realm: "cn", AccessToken: "a", EgressProxy: "http://1.2.3.4:8080"},
		&Account{UID: "bad", Realm: "cn", AccessToken: "a", EgressProxy: "socks5h://1.2.3.4:1080"},
		&Account{UID: "direct", Realm: "cn", AccessToken: "a"}, // 有意直连，合法
	)
	defer store.Close()
	if err := px.SyncAccounts(); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}

	if got := px.EgressErrorForUID("good"); got != "" {
		t.Fatalf("合法代理不应报错，得 %q", got)
	}
	if got := px.EgressErrorForUID("bad"); !strings.Contains(got, "socks5h") {
		t.Fatalf("非法代理应报出原因，得 %q", got)
	}
	if got := px.EgressErrorForUID("direct"); got != "" {
		t.Fatalf("空代理是「有意直连」，不应报错，得 %q", got)
	}
	bad := px.EgressBadUIDs()
	if len(bad) != 1 {
		t.Fatalf("应只有 1 个出口异常账号，得 %v", bad)
	}
}

// TestEgressBadExcludedFromPlatformSelection 出口异常账号必须被排除出平台选号。
//
// 这是需求「宁可不跑，也不裸奔直连」的直接落地：如果只是标记而不排除，
// 账号照样会从本机 IP 出去，标记就毫无意义。
func TestEgressBadExcludedFromPlatformSelection(t *testing.T) {
	store, px, _ := newTestApp(t,
		&Account{UID: "bad", Realm: "cn", AccessToken: "a", EgressProxy: "not-a-url"},
	)
	defer store.Close()
	if err := px.SyncAccounts(); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}
	if px.allowsSelection("bad", pickScope{}) {
		t.Fatal("出口异常账号不应参与平台选号")
	}
	if au := px.pickExcluding(map[string]bool{}, "", pickScope{}); au != nil {
		t.Fatalf("平台应选不出任何账号，得 %v", au)
	}
	// 代挂自用（HostUser>0）的判定走另一分支，与 egressBad 无关：
	// 号主用自己的号时，管理员的代理配置不该把他挡在门外。
	px.ownerByUID["bad"] = 5
	if !px.allowsSelection("bad", pickScope{HostUser: 5}) {
		t.Fatal("代挂自用不应被出口异常影响（风险由号主自担，不该被管理员配置连累）")
	}
}

// TestGlobalDefaultEgressBadAffectsInheritors 全局默认代理填错：
// 所有继承它的账号都应被标记 —— 影响面比单个账号大得多，最容易全池踩中。
func TestGlobalDefaultEgressBadAffectsInheritors(t *testing.T) {
	store, px, _ := newTestApp(t,
		&Account{UID: "a1", Realm: "cn", AccessToken: "a"}, // 继承默认
		&Account{UID: "a2", Realm: "cn", AccessToken: "a", EgressProxy: "http://9.9.9.9:8080"},
	)
	defer store.Close()
	if err := store.SetKV(kvEgressDefault, "socks5h://1.2.3.4:1080"); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	px.ReloadSelection()
	if err := px.SyncAccounts(); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}
	if got := px.EgressErrorForUID("a1"); !strings.Contains(got, "全局默认出口代理") {
		t.Fatalf("继承默认代理的账号应被标记，得 %q", got)
	}
	if got := px.EgressErrorForUID("a2"); got != "" {
		t.Fatalf("自配合法代理的账号不应被牵连，得 %q", got)
	}
}

// TestEgressOverviewAggregation 看板聚合：账号数、号主分布、超阈值、直连与异常清单。
func TestEgressOverviewAggregation(t *testing.T) {
	store, px, a := newTestApp(t,
		&Account{UID: "p1", Realm: "cn", AccessToken: "a", OwnerUserID: 1, EgressProxy: "http://1.1.1.1:8080", ConsumeGroup: cgProxy},
		&Account{UID: "p2", Realm: "cn", AccessToken: "a", OwnerUserID: 2, EgressProxy: "http://1.1.1.1:8080", ConsumeGroup: cgProxy},
		&Account{UID: "p3", Realm: "cn", AccessToken: "a", OwnerUserID: 3, EgressProxy: "http://1.1.1.1:8080", ConsumeGroup: cgProxy},
		&Account{UID: "d1", Realm: "cn", AccessToken: "a", ConsumeGroup: cgProxy}, // 直连且参与消耗
		&Account{UID: "b1", Realm: "cn", AccessToken: "a", EgressProxy: "bad", ConsumeGroup: cgProxy},
	)
	defer store.Close()
	if err := px.SyncAccounts(); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}
	// 上限 2 → 3 个账号共用一条线路应判超阈值。
	if err := store.SetKV(kvEgressMaxPerProxy, "2"); err != nil {
		t.Fatalf("SetKV: %v", err)
	}

	view, err := a.egressOverview()
	if err != nil {
		t.Fatalf("egressOverview: %v", err)
	}
	var shared *egressOverviewProxy
	for i := range view.Proxies {
		if view.Proxies[i].URL == "http://1.1.1.1:8080" {
			shared = &view.Proxies[i]
		}
	}
	if shared == nil {
		t.Fatalf("应统计出手填但不在池里的线路，得 %+v", view.Proxies)
	}
	if shared.AccountCount != 3 {
		t.Fatalf("共用该出口的账号应为 3，得 %d", shared.AccountCount)
	}
	if !shared.OverThreshold {
		t.Fatal("3 > 上限 2，应判为超阈值")
	}
	if len(shared.Owners) != 3 {
		t.Fatalf("应有 3 个不同号主，得 %v", shared.Owners)
	}
	if view.Unassigned != 1 {
		t.Fatalf("应有 1 个参与消耗却走直连的账号，得 %d", view.Unassigned)
	}
	if len(view.Bad) != 1 || view.Bad[0]["uid"] != "b1" {
		t.Fatalf("异常清单应含 b1，得 %v", view.Bad)
	}
	if view.OverCount != 1 {
		t.Fatalf("超阈值线路数应为 1，得 %d", view.OverCount)
	}
}

// TestEgressAssignHandler 分配接口端到端：写库 + 重同步 + 计数回传。
func TestEgressAssignHandler(t *testing.T) {
	store, px, a := newTestApp(t,
		&Account{UID: "u1", Realm: "cn", AccessToken: "a", OwnerUserID: 5, ConsumeGroup: cgProxy},
		&Account{UID: "u2", Realm: "cn", AccessToken: "a", OwnerUserID: 5, ConsumeGroup: cgProxy},
	)
	defer store.Close()
	if err := px.SyncAccounts(); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}
	for i := 0; i < 2; i++ {
		p := &EgressProxy{Name: "线路", URL: "http://10.0.0." + string(rune('1'+i)) + ":8080", Enabled: true}
		if err := store.UpsertEgressProxy(p); err != nil {
			t.Fatalf("UpsertEgressProxy: %v", err)
		}
	}

	body := `{"consume_group":"proxy","max_per_proxy":2}`
	req := httptest.NewRequest(http.MethodPost, "/api/admin/egress/assign", strings.NewReader(body))
	rec := httptest.NewRecorder()
	a.handleEgressAssign(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	resp := decodeJSON(t, rec.Body.Bytes())
	if resp["assigned"].(float64) != 2 {
		t.Fatalf("应分配 2 个账号，得 %v", resp["assigned"])
	}

	// 落库校验：两个同号主账号必须落在不同出口。
	a1, _ := store.GetAccount("u1")
	a2, _ := store.GetAccount("u2")
	if a1.EgressProxy == "" || a2.EgressProxy == "" {
		t.Fatalf("代理未写入库：%q / %q", a1.EgressProxy, a2.EgressProxy)
	}
	if a1.EgressProxy == a2.EgressProxy {
		t.Fatalf("同号主的账号不应落到同一出口，两者都是 %s", a1.EgressProxy)
	}
	// 重同步后不应再判为出口异常。
	if bad := px.EgressBadUIDs(); len(bad) != 0 {
		t.Fatalf("合法代理不应产生出口异常，得 %v", bad)
	}
}

// TestEgressAssignHandlerEmptyPool 池为空时必须给出可操作提示，而不是静默什么都不做。
func TestEgressAssignHandlerEmptyPool(t *testing.T) {
	store, _, a := newTestApp(t, &Account{UID: "u1", Realm: "cn", AccessToken: "a"})
	defer store.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/egress/assign", strings.NewReader(`{"all":true}`))
	rec := httptest.NewRecorder()
	a.handleEgressAssign(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("应 400，得 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "出口代理池为空") {
		t.Fatalf("应说明原因，得 %s", rec.Body.String())
	}
}

// TestEgressAssignNoScope 不指定范围时不能默默全选（避免误把全池账号改掉）。
func TestEgressAssignNoScope(t *testing.T) {
	store, _, a := newTestApp(t, &Account{UID: "u1", Realm: "cn", AccessToken: "a"})
	defer store.Close()
	p := &EgressProxy{Name: "线路", URL: "http://10.0.0.1:8080", Enabled: true}
	_ = store.UpsertEgressProxy(p)
	req := httptest.NewRequest(http.MethodPost, "/api/admin/egress/assign", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	a.handleEgressAssign(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("空筛选应 400，得 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "请指定分配范围") {
		t.Fatalf("应提示指定范围，得 %s", rec.Body.String())
	}
}

// TestEgressProxyHandlerValidate 代理池接口：非法 URL 必须被拒（保存阶段就挡住）。
func TestEgressProxyHandlerValidate(t *testing.T) {
	store, _, a := newTestApp(t)
	defer store.Close()

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/admin/egress/proxies", strings.NewReader(body))
		rec := httptest.NewRecorder()
		a.handleEgressProxies(rec, req)
		return rec
	}
	if rec := post(`{"url":"socks5h://1.2.3.4:1080"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("socks5h 应被拒，得 %d", rec.Code)
	}
	if rec := post(`{"url":"1.2.3.4:8080"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("缺协议应被拒，得 %d", rec.Code)
	}
	if rec := post(`{"url":""}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("空 URL 应被拒，得 %d", rec.Code)
	}
	// 合法但关闭探活（check=false），避免测试里打外网。
	rec := post(`{"url":"http://10.1.2.3:8080","name":"测试线路","check":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("合法应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	list, _ := store.ListEgressProxies()
	if len(list) != 1 || list[0].Name != "测试线路" {
		t.Fatalf("应落库 1 条，得 %+v", list)
	}
	// 重复 URL → 409。
	if rec := post(`{"url":"http://10.1.2.3:8080","name":"再录一次","check":false}`); rec.Code != http.StatusConflict {
		t.Fatalf("重复 URL 应 409，得 %d", rec.Code)
	}
}

// TestDeterministicJitter 抖动必须确定性：同一账号每次得到同一偏移，
// 不同账号尽量错开（用 uid 派生而非纯随机，避免每请求重新抽签偶发撞车）。
func TestDeterministicJitter(t *testing.T) {
	a := deterministicJitter("uid-aaa", 200)
	b := deterministicJitter("uid-aaa", 200)
	if a != b {
		t.Fatalf("同一 uid 应得到同一偏移：%v vs %v", a, b)
	}
	if a < 0 || a >= 200*1e6 {
		t.Fatalf("偏移应在 [0,200ms)，得 %v", a)
	}
	if deterministicJitter("uid-aaa", 0) != 0 {
		t.Fatal("上限 0 应返回 0（关闭）")
	}
	// 不同 uid 应大概率落在不同档位（不强断言相等，只确认不是恒定值）。
	distinct := map[int64]bool{}
	for _, u := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		distinct[int64(deterministicJitter(u, 1000))] = true
	}
	if len(distinct) < 3 {
		t.Fatalf("8 个 uid 的偏移过于集中（%d 种），抖动形同虚设", len(distinct))
	}
}

// TestEgressJitterOnlyWhenProxySet 未配代理时不应引入任何延迟。
func TestEgressJitterOnlyWhenProxySet(t *testing.T) {
	px := newSelectionProxy()
	px.egressJitterMs = 200
	if d := px.egressJitter("uid-1"); d != 0 {
		t.Fatalf("未配代理不应抖动，得 %v", d)
	}
	px.egressByUID["uid-1"] = "http://1.2.3.4:8080"
	if d := px.egressJitter("uid-1"); d == 0 {
		t.Fatal("配了代理且上限非 0 时应产生非零抖动")
	}
}

// TestEgressValidateUsedEverywhere 防御性回归：
// internal/egress.Validate 必须拒绝 Go 不支持的 scheme（socks5h）与缺端口的地址。
// 这条守住的是「保存通过但运行期失效」这一类问题的最低门槛。
func TestEgressValidateUsedEverywhere(t *testing.T) {
	for _, s := range []string{"socks5h://h:1", "http://h", "http://:1"} {
		if _, err := egress.Validate(s); err == nil {
			t.Fatalf("%q 应被拒绝", s)
		}
	}
}
