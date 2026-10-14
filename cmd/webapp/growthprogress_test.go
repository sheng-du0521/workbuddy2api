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

// newGrowthProgressApp 造一个把 chat 域指向假上游的 app，并预置账号。
// 返回的 setHandler 用于在测试里安装上游响应（避免改动已启动 server 的 Config）。
func newGrowthProgressApp(t *testing.T, accounts ...*Account) (*Store, *app, func(http.HandlerFunc)) {
	t.Helper()
	if len(accounts) == 0 {
		accounts = []*Account{{UID: "u1", Realm: "cn", AccessToken: "at", Nickname: "演示账号"}}
	}
	store, px, a := newTestApp(t, accounts...)
	// Windows 下不关库会让 TempDir 清理失败（文件被占用），故在 helper 内统一收尾。
	t.Cleanup(func() { _ = store.Close() })

	var handler http.HandlerFunc
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if handler == nil {
			http.NotFound(w, r)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	px.uc.ChatBaseCN = srv.URL
	px.uc.HTTP = srv.Client()
	return store, a, func(h http.HandlerFunc) { handler = h }
}

// growthTasksBody 生成上游任务列表响应体（data 段形状）。
func growthTasksBody(items ...map[string]any) []byte {
	b, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"tasks": items}})
	return b
}

// taskItem 造一个任务条目；target<=0 时 progress 为 null（上游未 accept 的形态）。
func taskItem(code, status string, cur, target int) map[string]any {
	it := map[string]any{"task_code": code, "accept_status": status}
	if target > 0 {
		it["progress"] = map[string]any{"current": cur, "target": target}
	} else {
		it["progress"] = nil
	}
	return it
}

// serveList 安装一个「返回固定任务列表」的假上游。
func serveList(t *testing.T, set func(http.HandlerFunc), body []byte, code int) {
	t.Helper()
	set(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/activity/growth/tasks" {
			t.Errorf("进度查询应走 growth 任务列表接口，实际 %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write(body)
	})
}

func jsonReq(method, target, body string) *http.Request {
	return httptest.NewRequest(method, target, strings.NewReader(body))
}

// TestGrowthProgressFolding 折叠逻辑：claimed 计入完成、completed/达标计入可领、
// 只统计可自动项、非自动项照常列出但不进分母。
func TestGrowthProgressFolding(t *testing.T) {
	store, a, set := newGrowthProgressApp(t)
	_ = store
	serveList(t, set, growthTasksBody(
		taskItem("create_canvas", "claimed", 1, 1),            // 自动项，已领
		taskItem("template_5", "claimed", 5, 5),               // 自动项，已领
		taskItem("first_buddy", "completed", 1, 1),            // 自动项，达标未领 → Ready
		taskItem("chat_5", "in_progress", 2, 5),               // 自动项，未完成
		taskItem("skill_1", "not_accepted", 0, 0),             // 自动项，未接取
		taskItem("black_cat", "in_progress", 1, 3),            // 非自动（夜猫窗口）
		taskItem("Expert_Philanthropy", "not_accepted", 0, 0), // 非自动（需真实捐款）
	), http.StatusOK)

	acc, _ := a.store.GetAccount("u1")
	v := a.growthProgress(acc, true)

	if !v.OK {
		t.Fatalf("应查询成功，得 error=%q", v.Error)
	}
	if v.AutoTotal != 5 {
		t.Fatalf("自动项分母应为 5（black_cat/Philanthropy 不计），得 %d", v.AutoTotal)
	}
	if v.AutoDone != 2 {
		t.Fatalf("已领自动项应为 2，得 %d", v.AutoDone)
	}
	if v.Ready != 1 {
		t.Fatalf("可领（达标未领）应为 1，得 %d", v.Ready)
	}
	if v.Total != 7 || v.Done != 2 {
		t.Fatalf("全部任务应为 7 项/2 已领，得 %d/%d", v.Total, v.Done)
	}
	var cat *growthTaskItem
	for i := range v.Items {
		if v.Items[i].Code == "black_cat" {
			cat = &v.Items[i]
		}
	}
	if cat == nil {
		t.Fatal("black_cat 应出现在明细里（进度页的价值就是把差的列出来）")
	}
	if cat.Auto {
		t.Fatal("black_cat 不应被标记为可自动完成")
	}
	if cat.Name == "" || cat.Phase != "night" {
		t.Fatalf("black_cat 应有中文名与阶段，得 name=%q phase=%q", cat.Name, cat.Phase)
	}
}

// TestGrowthProgressReadyByProgress 上游存在「current 达标但状态未推进」的形态，
// 也应判为可领（否则显示成未完成，会诱导人多跑一轮上报）。
func TestGrowthProgressReadyByProgress(t *testing.T) {
	_, a, set := newGrowthProgressApp(t)
	serveList(t, set, growthTasksBody(taskItem("expert_5", "accepted", 5, 5)), http.StatusOK)

	acc, _ := a.store.GetAccount("u1")
	v := a.growthProgress(acc, true)

	if v.Ready != 1 {
		t.Fatalf("current>=target 应判为可领，得 Ready=%d", v.Ready)
	}
	if v.Items[0].Done {
		t.Fatal("未 claim 不应算已领")
	}
}

// TestGrowthProgressUnknownCode 上游新增任务：照常展示，但不计入自动完成度
// ——否则新任务会让所有账号的进度突然「永远做不满」。
func TestGrowthProgressUnknownCode(t *testing.T) {
	_, a, set := newGrowthProgressApp(t)
	serveList(t, set, growthTasksBody(
		taskItem("create_canvas", "claimed", 1, 1),
		taskItem("brand_new_task_2027", "not_accepted", 0, 1),
	), http.StatusOK)

	acc, _ := a.store.GetAccount("u1")
	v := a.growthProgress(acc, true)

	if v.AutoTotal != 1 || v.AutoDone != 1 {
		t.Fatalf("未知任务不应进自动分母，得 %d/%d", v.AutoDone, v.AutoTotal)
	}
	if v.Total != 2 {
		t.Fatalf("未知任务应计入总数，得 %d", v.Total)
	}
	var unknown *growthTaskItem
	for i := range v.Items {
		if v.Items[i].Code == "brand_new_task_2027" {
			unknown = &v.Items[i]
		}
	}
	if unknown == nil || unknown.Name != "brand_new_task_2027" || unknown.Auto {
		t.Fatalf("未知任务应以 code 为名且非自动，得 %+v", unknown)
	}
}

// TestGrowthProgressEmptyList 一个任务都没下发时给出可读原因
// （而非「0/0 = 100% 完成」这种误导性显示）。
func TestGrowthProgressEmptyList(t *testing.T) {
	_, a, set := newGrowthProgressApp(t)
	serveList(t, set, growthTasksBody(), http.StatusOK)

	acc, _ := a.store.GetAccount("u1")
	v := a.growthProgress(acc, true)

	if v.OK {
		t.Fatal("无任务时不应报成功")
	}
	if v.Error == "" {
		t.Fatal("应给出原因")
	}
}

// TestGrowthProgressUpstreamError 上游失败要如实报错，不能折叠成空进度。
func TestGrowthProgressUpstreamError(t *testing.T) {
	_, a, set := newGrowthProgressApp(t)
	serveList(t, set, []byte(`{"code":500,"msg":"boom"}`), http.StatusInternalServerError)

	acc, _ := a.store.GetAccount("u1")
	v := a.growthProgress(acc, true)

	if v.OK || v.Error == "" {
		t.Fatalf("上游 500 应报错，得 ok=%v error=%q", v.OK, v.Error)
	}
}

// TestGrowthProgressCache 缓存：不带 force 的第二次命中缓存（不再打上游）；
// force 必须真打（执行完一轮后要看到新进度）。
func TestGrowthProgressCache(t *testing.T) {
	_, a, set := newGrowthProgressApp(t)
	hits := 0
	set(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(growthTasksBody(taskItem("create_canvas", "claimed", 1, 1)))
	})

	acc, _ := a.store.GetAccount("u1")
	_ = a.growthProgress(acc, false)
	second := a.growthProgress(acc, false)

	if hits != 1 {
		t.Fatalf("第二次应命中缓存（上游只该被调 1 次），得 %d", hits)
	}
	if !second.Cached {
		t.Fatal("第二次应标记 Cached=true")
	}
	forced := a.growthProgress(acc, true)
	if forced.Cached {
		t.Fatal("force 查询不应标记为缓存命中")
	}
	if hits != 2 {
		t.Fatalf("force 应真打上游，得 %d 次", hits)
	}
}

// TestSortGrowthItems 排序：未完成整体在前（进度页第一诉求是「还差什么」），
// 未完成组内「可领」最前，组内再按阶段顺序；已完成的一律沉到末尾。
func TestSortGrowthItems(t *testing.T) {
	items := []growthTaskItem{
		{Code: "a", Phase: "zero", Done: true},               // 已完成：沉底
		{Code: "b", Phase: "talk", Done: false},              // 未完成
		{Code: "c", Phase: "zero", Done: false, Ready: true}, // 未完成且可领：最前
		{Code: "d", Phase: "talk", Done: true},               // 已完成：沉底
	}
	sortGrowthItems(items)

	got := []string{items[0].Code, items[1].Code, items[2].Code, items[3].Code}
	want := []string{"c", "b", "a", "d"} // 可领 → 未完成 → 已完成（组内按 phase）
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("排序应为 %v，得 %v", want, got)
		}
	}
}

// TestDecodeGrowthProgressReq 请求解析：去重去空保序、空列表拒绝、超上限截断。
func TestDecodeGrowthProgressReq(t *testing.T) {
	// 去重 + 去空 + 保序
	got, ok := decodeGrowthProgressReq(httptest.NewRecorder(),
		jsonReq(http.MethodPost, "/", `{"uids":["a","","a","b"]}`))
	if !ok {
		t.Fatal("应解析成功")
	}
	if len(got.UIDs) != 2 || got.UIDs[0] != "a" || got.UIDs[1] != "b" {
		t.Fatalf("应去重去空并保序，得 %v", got.UIDs)
	}

	// 空列表 → 拒绝
	if _, ok := decodeGrowthProgressReq(httptest.NewRecorder(),
		jsonReq(http.MethodPost, "/", `{"uids":[]}`)); ok {
		t.Fatal("空选择应被拒绝")
	}

	// 超上限 → 截断并记录 Truncated
	many := make([]string, 0, growthProgressMaxBatch+5)
	for i := 0; i < growthProgressMaxBatch+5; i++ {
		many = append(many, string(rune('a'+i%26))+string(rune('0'+i/26)))
	}
	body, _ := json.Marshal(map[string]any{"uids": many})
	got2, ok := decodeGrowthProgressReq(httptest.NewRecorder(),
		jsonReq(http.MethodPost, "/", string(body)))
	if !ok {
		t.Fatal("大批量应解析成功")
	}
	if len(got2.UIDs) != growthProgressMaxBatch {
		t.Fatalf("应截断到 %d，得 %d", growthProgressMaxBatch, len(got2.UIDs))
	}
	if got2.Truncated != 5 {
		t.Fatalf("Truncated 应为 5，得 %d", got2.Truncated)
	}
}

// TestHostGrowthRunOwnership 用户侧执行：非本人账号被静默剔除；
// 全部非本人时以「未选账号」拒绝（不泄漏 uid 是否存在）。
func TestHostGrowthRunOwnership(t *testing.T) {
	store, a, _ := newGrowthProgressApp(t,
		&Account{UID: "mine", Realm: "cn", AccessToken: "at", OwnerUserID: 7},
		&Account{UID: "other", Realm: "cn", AccessToken: "at", OwnerUserID: 8},
	)
	_ = store
	// tasks 置为非 nil 以越过「服务未启用」早退；growthOK=false 会在执行入口报错，
	// 本用例关心的是**归属过滤**这一步（越权 uid 必须被剔除）。
	a.tasks = &taskRunner{}

	// 只提交别人的号 → 过滤后为空 → 400「请先选择」
	w := httptest.NewRecorder()
	r := withUser(jsonReq(http.MethodPost, "/api/host/growth/run", `{"uids":["other"]}`),
		&User{ID: 7, Status: StatusActive})
	a.handleHostGrowthRun(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("越权 uid 应被剔除并拒绝，得 %d %s", w.Code, w.Body.String())
	}

	// 混入别人的号：只保留自己的那个（其余被剔除，不报错）
	w2 := httptest.NewRecorder()
	r2 := withUser(jsonReq(http.MethodPost, "/api/host/growth/run", `{"uids":["other","mine"]}`),
		&User{ID: 7, Status: StatusActive})
	a.handleHostGrowthRun(w2, r2)
	// growthOK=false → 会在 RunGrowth 处报「脚本链路不可用」，说明归属过滤已通过。
	if w2.Code != http.StatusBadRequest || !strings.Contains(w2.Body.String(), "脚本链路") {
		t.Fatalf("应通过归属过滤后停在脚本链路检查，得 %d %s", w2.Code, w2.Body.String())
	}
}

// TestHostGrowthProgressFiltersForeign 用户侧进度查询：非本人 uid 不出现在结果里。
func TestHostGrowthProgressFiltersForeign(t *testing.T) {
	_, a, _ := newGrowthProgressApp(t,
		&Account{UID: "mine", Realm: "cn", AccessToken: "at", OwnerUserID: 7},
		&Account{UID: "other", Realm: "cn", AccessToken: "at", OwnerUserID: 8},
	)
	w := httptest.NewRecorder()
	r := withUser(jsonReq(http.MethodPost, "/api/host/growth/progress", `{"uids":["other"]}`),
		&User{ID: 7, Status: StatusActive})
	a.handleHostGrowthProgress(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("应 200（空结果而非报错），得 %d %s", w.Code, w.Body.String())
	}
	var out struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析响应失败：%v", err)
	}
	if out.Count != 0 {
		t.Fatalf("别人的账号不应出现在结果里，得 count=%d", out.Count)
	}
}
