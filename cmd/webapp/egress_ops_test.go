// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ===========================================================================
// v0.24.6 出口运维：坏线路上的账号自动迁移 + 候选地址批量导入
//
// 只测**纯逻辑与边界**（不打网络）：探活本身依赖外网，放在单测里会又慢又 flaky。
// ===========================================================================

func addProxy(t *testing.T, s *Store, id, url string, enabled, lastOK bool, streak int64) {
	t.Helper()
	if err := s.UpsertEgressProxy(&EgressProxy{
		ID: id, Name: id, URL: url, Enabled: enabled, LastOK: lastOK, FailStreak: streak,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestRehomeAccountsFromDeadProxies 坏线路（停用 / 连续失败）上的账号要迁到可用线路上。
func TestRehomeAccountsFromDeadProxies(t *testing.T) {
	store, px, a := newTestApp(t,
		&Account{UID: "e1", AccessToken: "tok"},
		&Account{UID: "e2", AccessToken: "tok"},
		&Account{UID: "e3", AccessToken: "tok"})
	defer store.Close()
	defer px.Close()

	dead := "http://10.0.0.1:8080"
	alive := "http://10.0.0.2:8080"
	addProxy(t, store, "p-dead", dead, true, false, egressFailThreshold) // 连续失败达阈值
	addProxy(t, store, "p-alive", alive, true, true, 0)

	for _, uid := range []string{"e1", "e2", "e3"} {
		if err := store.SetAccountEgress(uid, dead); err != nil {
			t.Fatal(err)
		}
	}
	// 手工停用的线路同样算「坏」（否则"我停用了它、账号还挂在上面"没人收尾）。
	manuallyOff := "http://10.0.0.3:8080"
	addProxy(t, store, "p-off", manuallyOff, false, true, 0)
	if err := store.SetAccountEgress("e3", manuallyOff); err != nil {
		t.Fatal(err)
	}

	rep := a.rehomeAccountsFromDeadProxies()
	if rep.Moved != 3 || rep.Kept != 0 {
		t.Fatalf("应迁移 3 个账号且无需保留原状，得 moved=%d kept=%d warn=%v",
			rep.Moved, rep.Kept, rep.Warnings)
	}
	if len(rep.DeadProxies) != 2 {
		t.Fatalf("应识别 2 条坏线路，得 %v", rep.DeadProxies)
	}
	for _, uid := range []string{"e1", "e2", "e3"} {
		acc, _ := store.GetAccount(uid)
		if acc == nil || strings.TrimSpace(acc.EgressProxy) != alive {
			t.Fatalf("%s 应迁到可用线路，得 %q", uid, acc.EgressProxy)
		}
	}
}

// TestRehomeRespectsCapacity 迁移要遵守「单条线路承载上限」。
func TestRehomeRespectsCapacity(t *testing.T) {
	store, px, a := newTestApp(t,
		&Account{UID: "c1", AccessToken: "tok"},
		&Account{UID: "c2", AccessToken: "tok"},
		&Account{UID: "c3", AccessToken: "tok"})
	defer store.Close()
	defer px.Close()
	if err := store.SetKV(kvEgressMaxPerProxy, "2"); err != nil {
		t.Fatal(err)
	}
	dead := "http://10.0.0.1:8080"
	alive := "http://10.0.0.2:8080"
	addProxy(t, store, "p-dead", dead, false, false, 0)
	addProxy(t, store, "p-alive", alive, true, true, 0)
	for _, uid := range []string{"c1", "c2", "c3"} {
		_ = store.SetAccountEgress(uid, dead)
	}

	rep := a.rehomeAccountsFromDeadProxies()
	// 上限 2：两个进 alive，一个无处可去 → **保留原状**（v0.24.8 起不再退化直连）。
	if rep.Moved != 2 || rep.Kept != 1 {
		t.Fatalf("应 moved=2 kept=1，得 moved=%d kept=%d", rep.Moved, rep.Kept)
	}
	if len(rep.Warnings) == 0 {
		t.Fatal("有账号无处可去时必须给出 warning（不静默降级）")
	}
}

// TestRehomeNoHealthyProxyKeepsOriginal 没有任何可用线路时：**保留原状 + 告警**，
// **绝不**退化为直连（v0.24.8 语义反转 —— 旧用例断言的是写 `direct` 哨兵）。
//
// 这是与选号侧设计原则对齐的关键用例：proxy.go 的 allowsSelection 写明
// 「宁可这个请求失败，也不让账号从本机 IP 裸奔出去」。旧实现在这条路径上写 `direct`，
// 把账号改成从本机 IP 出站，与那条原则正面矛盾 —— 线路全挂时，全部账号会在一个
// 探活周期内被静默改写，防封整体失效，痕迹只有一行日志。
//
// 保留原状不会裸奔：账号仍绑在坏线路上 → 判 egressBad → 排除出选号 → 请求失败，
// 但出口仍是那条坏线路。「失败」可以接受，「裸奔」不可以。
func TestRehomeNoHealthyProxyKeepsOriginal(t *testing.T) {
	store, px, a := newTestApp(t, &Account{UID: "d1", AccessToken: "tok"})
	defer store.Close()
	defer px.Close()
	dead := "http://10.0.0.1:8080"
	addProxy(t, store, "p-dead", dead, true, false, egressFailThreshold)
	_ = store.SetAccountEgress("d1", dead)

	rep := a.rehomeAccountsFromDeadProxies()
	if rep.Kept != 1 || rep.Moved != 0 {
		t.Fatalf("应保留原状 1 个，得 moved=%d kept=%d", rep.Moved, rep.Kept)
	}
	acc, _ := store.GetAccount("d1")
	if acc.EgressProxy != dead {
		t.Fatalf("应保留原线路 %q（绝不写 direct 让账号裸奔），得 %q", dead, acc.EgressProxy)
	}
	if len(rep.Warnings) == 0 {
		t.Fatal("保留原状时必须给出 warning，不能静默")
	}
}

// TestRehomeRespectsOwnerIsolation 迁移要维持「同一号主的账号不共用出口」（v0.24.8）。
//
// 分配阶段把 isolate_owner 当**第一约束**（甚至为此放宽容量上限），而迁移阶段此前
// 完全不看号主，只按负载升序挑线路 —— 于是"修出口"这个动作自己把同一号主的多个号
// 搬回同一条线路，重新制造出最强的关联信号（风控看板上表现为 OwnerConflict 标红），
// 而且每 5 分钟探活一轮就可能重复发生。
func TestRehomeRespectsOwnerIsolation(t *testing.T) {
	store, px, a := newTestApp(t,
		&Account{UID: "o1", AccessToken: "tok", OwnerUserID: 7},
		&Account{UID: "o2", AccessToken: "tok", OwnerUserID: 7})
	defer store.Close()
	defer px.Close()

	dead := "http://10.0.0.1:8080"
	alive1 := "http://10.0.0.2:8080"
	alive2 := "http://10.0.0.3:8080"
	addProxy(t, store, "p-dead", dead, false, false, 0)
	addProxy(t, store, "p-a1", alive1, true, true, 0)
	addProxy(t, store, "p-a2", alive2, true, true, 0)
	_ = store.SetAccountEgress("o1", dead)
	_ = store.SetAccountEgress("o2", dead)

	rep := a.rehomeAccountsFromDeadProxies()
	if rep.Moved != 2 {
		t.Fatalf("两个号都该迁走，得 moved=%d kept=%d", rep.Moved, rep.Kept)
	}
	a1, _ := store.GetAccount("o1")
	a2, _ := store.GetAccount("o2")
	if a1.EgressProxy == a2.EgressProxy {
		t.Fatalf("同一号主的两个号不该共用出口（isolate_owner），都得 %q", a1.EgressProxy)
	}
}

// TestRehomeOnlyTouchesEgress 迁移只改 egress_proxy 一个字段，其它状态一概不动。
func TestRehomeOnlyTouchesEgress(t *testing.T) {
	store, px, a := newTestApp(t, &Account{UID: "f1", AccessToken: "tok", ConsumeGroup: cgProxy})
	defer store.Close()
	defer px.Close()
	dead := "http://10.0.0.1:8080"
	addProxy(t, store, "p-dead", dead, false, false, 0)
	addProxy(t, store, "p-alive", "http://10.0.0.2:8080", true, true, 0)
	_ = store.SetAccountEgress("f1", dead)

	before, _ := store.GetAccount("f1")
	a.rehomeAccountsFromDeadProxies()
	after, _ := store.GetAccount("f1")
	if before.ConsumeGroup != after.ConsumeGroup || before.Disabled != after.Disabled ||
		before.Group != after.Group {
		t.Fatalf("迁移不该改动其它字段：before=%+v after=%+v", before, after)
	}
}

// TestFlattenCandidates 候选地址的解析：JSON 数组 / 一整段文本 / 注释行 / 去空。
func TestFlattenCandidates(t *testing.T) {
	got := flattenCandidates([]any{"http://1.1.1.1:80", "http://2.2.2.2:81\nhttp://3.3.3.3:82"})
	if len(got) != 3 {
		t.Fatalf("应解析出 3 条，得 %v", got)
	}
	text := flattenCandidates("# 注释\nhttp://a:1,http://b:2\n\n  http://c:3  ")
	if len(text) != 3 || text[0] != "http://a:1" || text[2] != "http://c:3" {
		t.Fatalf("文本形态解析不符：%v", text)
	}
	if len(flattenCandidates(123)) != 0 {
		t.Fatal("非字符串/数组应返回空")
	}
	if got := egressHostLabel("http://1.2.3.4:8080/path?a=1"); got != "1.2.3.4:8080" {
		t.Fatalf("host label 不符：%q", got)
	}
}

// TestEgressCandidatesRejectsInvalid 候选地址全非法 → 400（不落库、不探活）。
func TestEgressCandidatesRejectsInvalid(t *testing.T) {
	store, px, a := newTestApp(t, &Account{UID: "g1", AccessToken: "tok"})
	defer store.Close()
	defer px.Close()
	a.key = "cand-key"
	mux := a.routes()

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/admin/egress/candidates", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Admin-Key", "cand-key")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	if rec := post(`{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("空候选应 400，得 %d", rec.Code)
	}
	rec := post(`{"urls":["不是地址","direct"]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("全非法应 400，得 %d（body=%s）", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if inv, _ := out["invalid"].([]any); len(inv) != 2 {
		t.Fatalf("应回报 2 条非法，得 %v", out["invalid"])
	}
}
