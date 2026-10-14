// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestAnnouncementCRUD 公告基本链路：创建 → 列表 → 启停 → 编辑 → 删除。
func TestAnnouncementCRUD(t *testing.T) {
	store, px, a := newTestApp(t)
	defer store.Close()
	defer px.Close()

	if err := store.migrateAnnouncements(); err != nil {
		t.Fatalf("migrateAnnouncements: %v", err)
	}

	// 创建。
	rec := httptest.NewRecorder()
	a.handleAdminCreateAnnouncement(rec, httptest.NewRequest(http.MethodPost,
		"/api/admin/announcements",
		strings.NewReader(`{"title":"维护通知","content":"今晚 2 点维护","level":"warn","pinned":true}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("创建应 200，得 %d（%s）", rec.Code, rec.Body.String())
	}
	var created struct {
		Announcement *Announcement `json:"announcement"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created.Announcement == nil || created.Announcement.ID == 0 {
		t.Fatalf("应返回带 id 的公告，得 %s", rec.Body.String())
	}
	a1 := created.Announcement
	if a1.Level != "warn" || !a1.Pinned || !a1.Enabled {
		t.Fatalf("字段不符：%+v", a1)
	}

	// 管理端列表：含停用。
	rec = httptest.NewRecorder()
	a.handleAdminListAnnouncements(rec, httptest.NewRequest(http.MethodGet, "/api/admin/announcements", nil))
	var adminList struct {
		Announcements []*Announcement `json:"announcements"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &adminList)
	if len(adminList.Announcements) != 1 {
		t.Fatalf("管理端应看到 1 条，得 %d", len(adminList.Announcements))
	}

	// 用户侧列表：只含启用中。
	rec = httptest.NewRecorder()
	a.handleUserAnnouncements(rec, httptest.NewRequest(http.MethodGet, "/api/announcements", nil))
	var userList struct {
		Announcements []*Announcement `json:"announcements"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &userList)
	if len(userList.Announcements) != 1 {
		t.Fatalf("用户侧应看到 1 条启用公告，得 %d", len(userList.Announcements))
	}

	// 停用 → 用户侧立即消失，管理端仍在。
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/announcements/1/toggle",
		strings.NewReader(`{"enabled":false}`))
	req.SetPathValue("id", itoa(int(a1.ID)))
	a.handleAdminToggleAnnouncement(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("停用应 200，得 %d（%s）", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	a.handleUserAnnouncements(rec, httptest.NewRequest(http.MethodGet, "/api/announcements", nil))
	_ = json.Unmarshal(rec.Body.Bytes(), &userList)
	if len(userList.Announcements) != 0 {
		t.Fatalf("停用后用户侧应为空，得 %d", len(userList.Announcements))
	}
	rec = httptest.NewRecorder()
	a.handleAdminListAnnouncements(rec, httptest.NewRequest(http.MethodGet, "/api/admin/announcements", nil))
	_ = json.Unmarshal(rec.Body.Bytes(), &adminList)
	if len(adminList.Announcements) != 1 {
		t.Fatalf("停用后管理端仍应看到该条，得 %d", len(adminList.Announcements))
	}

	// 编辑（不传 pinned → 保留原值 true；这是 *bool 的意义所在）。
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/admin/announcements/1",
		strings.NewReader(`{"title":"维护通知（改）","content":"改成明晚 3 点","level":"important"}`))
	req.SetPathValue("id", itoa(int(a1.ID)))
	a.handleAdminUpdateAnnouncement(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("编辑应 200，得 %d（%s）", rec.Code, rec.Body.String())
	}
	got, err := store.GetAnnouncement(a1.ID)
	if err != nil {
		t.Fatalf("GetAnnouncement: %v", err)
	}
	if got.Title != "维护通知（改）" || got.Level != "important" {
		t.Fatalf("编辑未生效：%+v", got)
	}
	if !got.Pinned {
		t.Fatalf("未提供 pinned 时应保留原置顶状态")
	}

	// 删除。
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/api/admin/announcements/1", nil)
	req.SetPathValue("id", itoa(int(a1.ID)))
	a.handleAdminDeleteAnnouncement(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("删除应 200，得 %d", rec.Code)
	}
	if _, err := store.GetAnnouncement(a1.ID); err == nil {
		t.Fatalf("删除后不应还能取到")
	}
	// 删除不存在的 → 404。
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/api/admin/announcements/9999", nil)
	req.SetPathValue("id", "9999")
	a.handleAdminDeleteAnnouncement(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("删不存在应 404，得 %d", rec.Code)
	}
}

// TestAnnouncementValidation 内容约束：标题/正文长度与空值。
func TestAnnouncementValidation(t *testing.T) {
	store, px, a := newTestApp(t)
	defer store.Close()
	defer px.Close()
	if err := store.migrateAnnouncements(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		a.handleAdminCreateAnnouncement(rec, httptest.NewRequest(http.MethodPost,
			"/api/admin/announcements", strings.NewReader(body)))
		return rec
	}
	if rec := post(`{"title":"","content":"x"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("空标题应 400，得 %d", rec.Code)
	}
	if rec := post(`{"title":"x","content":"   "}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("空正文应 400，得 %d", rec.Code)
	}
	longTitle := strings.Repeat("题", announceMaxTitle+1)
	if rec := post(`{"title":` + jsonStr(longTitle) + `,"content":"x"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("超长标题应 400，得 %d", rec.Code)
	}
	longBody := strings.Repeat("文", announceMaxContent+1)
	if rec := post(`{"title":"t","content":` + jsonStr(longBody) + `}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("超长正文应 400，得 %d", rec.Code)
	}
	// 恰好等于上限 → 通过（边界必须放行，否则上限就成了「上限-1」）。
	okTitle := strings.Repeat("题", announceMaxTitle)
	okBody := strings.Repeat("文", announceMaxContent)
	if rec := post(`{"title":` + jsonStr(okTitle) + `,"content":` + jsonStr(okBody) + `}`); rec.Code != http.StatusOK {
		t.Fatalf("等于上限应 200，得 %d（%s）", rec.Code, rec.Body.String())
	}
	// 未知 level 回落 info（不因为字段脏值拒绝整条公告）。
	got, err := store.ListAnnouncements(false, 1)
	if err != nil || len(got) != 1 {
		t.Fatalf("应有 1 条，err=%v", err)
	}
	if got[0].Level != "info" {
		t.Fatalf("缺省级别应为 info，得 %q", got[0].Level)
	}
}

// TestAnnouncementTotalCap 总数上限：达到上限后创建被拒，且提示去删历史。
func TestAnnouncementTotalCap(t *testing.T) {
	store, px, a := newTestApp(t)
	defer store.Close()
	defer px.Close()
	if err := store.migrateAnnouncements(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for i := 0; i < announceMaxTotal; i++ {
		if _, err := store.CreateAnnouncement("t", "c", "info", true, false, 0, 0); err != nil {
			t.Fatalf("第 %d 条创建失败：%v", i+1, err)
		}
	}
	rec := httptest.NewRecorder()
	a.handleAdminCreateAnnouncement(rec, httptest.NewRequest(http.MethodPost,
		"/api/admin/announcements", strings.NewReader(`{"title":"又一条","content":"内容"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("超上限应 400，得 %d（%s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "上限") {
		t.Fatalf("错误信息应提到上限，得 %s", rec.Body.String())
	}
}

// TestAnnouncementOrdering 排序：置顶优先，其次创建时间倒序。
func TestAnnouncementOrdering(t *testing.T) {
	store, px, _ := newTestApp(t)
	defer store.Close()
	defer px.Close()
	if err := store.migrateAnnouncements(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := store.CreateAnnouncement("第一条", "c", "info", true, false, 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateAnnouncement("第二条", "c", "info", true, false, 0, 0); err != nil {
		t.Fatal(err)
	}
	// 把最早那条置顶：它应排到最前。
	first, _ := store.GetAnnouncement(1)
	if err := store.UpdateAnnouncement(first.ID, first.Title, first.Content, first.Level, true, 0, 0); err != nil {
		t.Fatalf("Update: %v", err)
	}
	list, err := store.ListAnnouncements(true, 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("应有 2 条，得 %d", len(list))
	}
	if list[0].Title != "第一条" {
		t.Fatalf("置顶的应排最前，得 %q", list[0].Title)
	}
}

// ===========================================================================
// v0.22.0：有效时限（start_at / end_at）与置顶的交互
// ===========================================================================

// TestAnnounceWindowState 状态机四态与边界（纯函数）。
//
// 时间窗是**左闭右开** [start, end)：start 时刻已生效、end 时刻已失效。
// 这两条边界是「到点就变」的全部争议所在，故逐个钉死。
func TestAnnounceWindowState(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	base := now.Unix()

	cases := []struct {
		name    string
		enabled bool
		start   int64
		end     int64
		want    string
	}{
		{"停用且无时限", false, 0, 0, annStateDisabled},
		{"停用且未到开始", false, base + 100, base + 200, annStateDisabled},
		{"停用且已过期", false, 0, base - 100, annStateDisabled},

		{"无时限", true, 0, 0, annStateActive},
		{"仅开始且已到", true, base - 10, 0, annStateActive},
		{"仅开始且未到", true, base + 10, 0, annStatePending},
		{"仅结束且未到", true, 0, base + 10, annStateActive},
		{"仅结束且已过", true, 0, base - 10, annStateExpired},

		// —— 边界：start 时刻「已生效」（左闭）——
		{"恰好等于开始时间 → 生效", true, base, base + 100, annStateActive},
		// —— 边界：end 时刻「已失效」（右开）——
		{"恰好等于结束时间 → 过期", true, base - 100, base, annStateExpired},
		{"结束时间前 1 秒 → 生效", true, base - 100, base + 1, annStateActive},

		{"窗口内", true, base - 10, base + 10, annStateActive},
		{"未来窗口", true, base + 10, base + 20, annStatePending},
		{"已过去窗口", true, base - 20, base - 10, annStateExpired},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := announceWindowState(c.enabled, c.start, c.end, now)
			if got != c.want {
				t.Fatalf("state=%q want %q (enabled=%v start=%d end=%d)",
					got, c.want, c.enabled, c.start, c.end)
			}
			// 中文标签必须与状态一一对应（前端标签直接用它）。
			if announceStateLabel(got) == "" {
				t.Fatalf("状态 %q 缺中文名", got)
			}
		})
	}
	// 标签互异性：四个状态的中文名不能撞车，否则管理端分不出状态。
	labels := map[string]bool{}
	for _, st := range []string{annStateDisabled, annStatePending, annStateActive, annStateExpired} {
		l := announceStateLabel(st)
		if labels[l] {
			t.Fatalf("状态中文名重复：%q", l)
		}
		labels[l] = true
	}
}

// TestAnnounceVisibilityParity SQL 过滤与 Go 纯函数**口径必须一致**。
//
// 这是本功能最容易长期跑偏的地方：一份判据在 SQL（WHERE）、一份在 Go
// （announceWindowState）。任一处被改动而另一处没跟上，就会出现「卡上不显示、
// 弹窗却弹」这类难查的错位。故用穷举组合把两者钉在一起。
func TestAnnounceWindowStateFilterParity(t *testing.T) {
	store, px, _ := newTestApp(t)
	defer store.Close()
	defer px.Close()
	if err := store.migrateAnnouncements(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	now := time.Now().Unix()

	// 覆盖：无时限 / 未开始 / 已过期 / 窗口内 / 停用 / 停用+过期 / 恰好边界。
	seed := []struct {
		title   string
		enabled bool
		start   int64
		end     int64
	}{
		{"no-window", true, 0, 0},
		{"pending", true, now + 3600, now + 7200},
		{"expired", true, 0, now - 3600},
		{"in-window", true, now - 3600, now + 3600},
		{"disabled", false, 0, 0},
		{"disabled-expired", false, 0, now - 10},
		// SQL 口径是「查询时刻」现算的，而种子用的是本测试开始时的 now，
		// 故边界留 **30 秒余量**：曾用 now±1 导致本用例偶发 FAIL（跨秒即翻）。
		// 「恰好等于结束时刻」这类精确边界由纯函数用例 TestAnnounceWindowState
		// 覆盖，它不依赖真实时钟。
		{"start-now", true, now - 30, 0}, // 已生效
		{"end-now", true, 0, now + 30},   // 尚未过期
		{"both-window", true, now - 30, now + 30},
	}
	for _, sd := range seed {
		if _, err := store.CreateAnnouncement(sd.title, "c", "info", sd.enabled, false, sd.start, sd.end); err != nil {
			t.Fatalf("seed %s: %v", sd.title, err)
		}
	}

	// ① SQL 口径：onlyEnabled=true（用户侧实际走的那条查询）。
	visibleBySQL, err := store.ListAnnouncements(true, 100)
	if err != nil {
		t.Fatalf("List(onlyEnabled): %v", err)
	}
	gotSQL := map[string]bool{}
	for _, a := range visibleBySQL {
		gotSQL[a.Title] = true
	}

	// ② Go 口径：取全量，用同一时刻跑纯函数。
	all, err := store.ListAnnouncements(false, 100)
	if err != nil {
		t.Fatalf("List(all): %v", err)
	}
	when := time.Now()
	wantGo := map[string]bool{}
	for _, a := range all {
		if a.Visible(when) {
			wantGo[a.Title] = true
		}
	}

	// ③ 比集合（不比分片：集合相等才说明判据一致）。
	if len(gotSQL) != len(wantGo) {
		t.Fatalf("可见集合大小不一致：SQL=%v Go=%v", sortedKeys(gotSQL), sortedKeys(wantGo))
	}
	for k := range gotSQL {
		if !wantGo[k] {
			t.Fatalf("SQL 认为可见、Go 认为不可见：%q（SQL=%v Go=%v）",
				k, sortedKeys(gotSQL), sortedKeys(wantGo))
		}
	}

	// ④ 期望的可见集合（人工断言，防止两侧「一起错」）。
	wantVisible := []string{"no-window", "in-window", "start-now", "end-now", "both-window"}
	for _, w := range wantVisible {
		if !gotSQL[w] {
			t.Fatalf("应可见却被过滤掉：%q（实际 %v）", w, sortedKeys(gotSQL))
		}
	}
	for _, hidden := range []string{"pending", "expired", "disabled", "disabled-expired"} {
		if gotSQL[hidden] {
			t.Fatalf("不应可见却出现：%q（实际 %v）", hidden, sortedKeys(gotSQL))
		}
	}
}

// sortedKeys 便于失败信息可读（map 遍历顺序随机）。
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestAnnounceWindowPinnedInteraction 时效与置顶的优先级：**时效 > 置顶**。
//
//	① 置顶 + 已过期 → 仍然不可见（置顶不能把过期公告救回来）
//	② 置顶 + 未开始 → 仍然不可见
//	③ 可见集合内 → 置顶排最前（置顶只在「能看见的」里起作用）
func TestAnnounceWindowPinnedInteraction(t *testing.T) {
	store, px, _ := newTestApp(t)
	defer store.Close()
	defer px.Close()
	if err := store.migrateAnnouncements(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	now := time.Now().Unix()

	// 三条置顶 + 一条普通，只有部分在窗内。
	mk := func(title string, pinned bool, start, end int64) {
		if _, err := store.CreateAnnouncement(title, "c", "info", true, pinned, start, end); err != nil {
			t.Fatalf("create %s: %v", title, err)
		}
	}
	mk("置顶-已过期", true, 0, now-60)
	mk("置顶-未开始", true, now+3600, now+7200)
	mk("置顶-窗内", true, 0, now+3600)
	mk("普通-窗内", false, 0, 0)

	list, err := store.ListAnnouncements(true, 50)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := make([]string, 0, len(list))
	for _, a := range list {
		got = append(got, a.Title)
	}
	if len(got) != 2 {
		t.Fatalf("只有 2 条应可见（置顶-窗内 / 普通-窗内），得 %v", got)
	}
	if got[0] != "置顶-窗内" {
		t.Fatalf("可见集合内置顶应排最前，得 %v", got)
	}
	if got[1] != "普通-窗内" {
		t.Fatalf("普通公告应排第二，得 %v", got)
	}
	// 过期/未开始的置顶公告必须彻底消失 —— 这是「时效优先」的核心断言。
	for _, bad := range []string{"置顶-已过期", "置顶-未开始"} {
		for _, g := range got {
			if g == bad {
				t.Fatalf("置顶不应突破时限：%q 出现在可见列表 %v", bad, got)
			}
		}
	}
}

// TestValidateAnnounceWindow 时限校验：非法组合逐个拒绝，合法组合放行。
func TestValidateAnnounceWindow(t *testing.T) {
	now := time.Now()
	base := now.Unix()

	bad := []struct {
		name       string
		start, end int64
	}{
		{"倒置（start > end）", base + 100, base + 50},
		{"零宽（start == end）", base + 100, base + 100},
		{"负开始", -1, base + 100},
		{"负结束", 0, -5},
		{"开始超出上限", now.AddDate(announceMaxWindowYears+1, 0, 0).Unix(), 0},
		{"结束超出上限", 0, now.AddDate(announceMaxWindowYears+1, 0, 0).Unix()},
	}
	for _, c := range bad {
		t.Run("拒绝/"+c.name, func(t *testing.T) {
			if err := validateAnnounceWindow(c.start, c.end, now); err == nil {
				t.Fatalf("start=%d end=%d 应被拒", c.start, c.end)
			}
		})
	}

	good := []struct {
		name       string
		start, end int64
	}{
		{"都不填（长期有效）", 0, 0},
		{"仅开始", base - 10, 0},
		{"仅结束", 0, base + 3600},
		{"两端都有", base - 10, base + 10},
		{"开始在过去很久（不限制过去）", -0, base + 10},
		{"结束正好在 10 年边界", 0, now.AddDate(announceMaxWindowYears, 0, 0).Unix() - 10},
	}
	for _, c := range good {
		t.Run("通过/"+c.name, func(t *testing.T) {
			if err := validateAnnounceWindow(c.start, c.end, now); err != nil {
				t.Fatalf("start=%d end=%d 应通过，得 %v", c.start, c.end, err)
			}
		})
	}
}

// TestAnnounceTimeWindowText 时间窗文本四形态。
func TestAnnounceTimeWindowText(t *testing.T) {
	ts := time.Date(2026, 10, 12, 9, 30, 0, 0, time.Local).Unix()
	if got := announceTimeWindowText(0, 0); got != "长期有效" {
		t.Fatalf("两端不限应为「长期有效」，得 %q", got)
	}
	if got := announceTimeWindowText(0, ts); !strings.Contains(got, "至") || !strings.Contains(got, "止") {
		t.Fatalf("仅结束应含「至…止」，得 %q", got)
	}
	if got := announceTimeWindowText(ts, 0); !strings.Contains(got, "自") || !strings.Contains(got, "起") {
		t.Fatalf("仅开始应含「自…起」，得 %q", got)
	}
	if got := announceTimeWindowText(ts, ts+3600); !strings.Contains(got, "~") {
		t.Fatalf("两端都应含「~」，得 %q", got)
	}
	// 时间必须按**本地时区**渲染（展示给管理员看，不是 UTC）。
	if got := announceTimeWindowText(0, ts); !strings.Contains(got, "09:30") {
		t.Fatalf("应按本地时区显示 09:30，得 %q", got)
	}
}

// TestAnnounceWindowHTTP 端到端：发布带时限的公告 → 用户侧可见性 → 管理端状态。
func TestAnnounceWindowHTTP(t *testing.T) {
	store, px, a := newTestApp(t)
	defer store.Close()
	defer px.Close()
	if err := store.migrateAnnouncements(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	now := time.Now().Unix()

	create := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		a.handleAdminCreateAnnouncement(rec, httptest.NewRequest(http.MethodPost,
			"/api/admin/announcements", strings.NewReader(body)))
		return rec
	}
	userList := func() []*Announcement {
		rec := httptest.NewRecorder()
		a.handleUserAnnouncements(rec, httptest.NewRequest(http.MethodGet, "/api/announcements", nil))
		var out struct {
			Announcements []*Announcement `json:"announcements"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out.Announcements
	}

	// ① 立即生效 + 有限期（未来结束）→ 用户可见，状态 active。
	rec := create(`{"title":"限时活动","content":"三天内有效","end_at":` + itoa64(now+3*86400) + `}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("创建应 200，得 %d（%s）", rec.Code, rec.Body.String())
	}
	var resp struct {
		Announcement *Announcement `json:"announcement"`
		State        string        `json:"state"`
		Hint         string        `json:"hint"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.State != annStateActive {
		t.Fatalf("应为 active，得 %q", resp.State)
	}
	if !strings.Contains(resp.Hint, "可见") {
		t.Fatalf("提示应说明可见性，得 %q", resp.Hint)
	}
	if len(userList()) != 1 {
		t.Fatalf("限时活动应立即可见，得 %d 条", len(userList()))
	}

	// ② 未来才开始（预告）→ 用户侧看不到，但管理端状态为 pending。
	rec = create(`{"title":"国庆预告","content":"国庆当天上线","start_at":` + itoa64(now+30*86400) + `,"end_at":` + itoa64(now+37*86400) + `}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("创建应 200，得 %d", rec.Code)
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.State != annStatePending {
		t.Fatalf("应为 pending，得 %q", resp.State)
	}
	if !strings.Contains(resp.Hint, "未到开始时间") {
		t.Fatalf("提示应说明未到开始时间，得 %q", resp.Hint)
	}
	if got := len(userList()); got != 1 {
		t.Fatalf("待生效的不该出现在用户侧，应仍为 1 条，得 %d", got)
	}

	// ③ 已经过期（结束时间在过去）→ 用户侧看不到，状态 expired。
	rec = create(`{"title":"已结束","content":"x","end_at":` + itoa64(now-60) + `}`)
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.State != annStateExpired {
		t.Fatalf("应为 expired，得 %q", resp.State)
	}
	if got := len(userList()); got != 1 {
		t.Fatalf("已过期的不该出现在用户侧，应仍为 1 条，得 %d", got)
	}

	// ④ 管理端：三条都在，且各自带状态（管理端不做时间过滤 —— 否则过期公告就
	//    从后台「消失」了，管理员没法延长它）。
	rec = httptest.NewRecorder()
	a.handleAdminListAnnouncements(rec, httptest.NewRequest(http.MethodGet, "/api/admin/announcements", nil))
	var admin struct {
		Announcements []*Announcement `json:"announcements"`
		ByState       map[string]int  `json:"by_state"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &admin)
	if len(admin.Announcements) != 3 {
		t.Fatalf("管理端应看到全部 3 条，得 %d", len(admin.Announcements))
	}
	if admin.ByState[annStateActive] != 1 || admin.ByState[annStatePending] != 1 || admin.ByState[annStateExpired] != 1 {
		t.Fatalf("状态计数不符：%v", admin.ByState)
	}
	for _, x := range admin.Announcements {
		if x.StateLabel == "" || x.TimeWindow == "" {
			t.Fatalf("管理端每条都应带状态与时间窗文本：%+v", x)
		}
	}

	// ⑤ 校验：结束早于开始 → 400，且不落库。
	before := len(admin.Announcements)
	rec = create(`{"title":"倒置","content":"x","start_at":` + itoa64(now+100) + `,"end_at":` + itoa64(now+50) + `}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("倒置时限应 400，得 %d（%s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "结束时间") {
		t.Fatalf("错误信息应提到结束时间，得 %s", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	a.handleAdminListAnnouncements(rec, httptest.NewRequest(http.MethodGet, "/api/admin/announcements", nil))
	_ = json.Unmarshal(rec.Body.Bytes(), &admin)
	if len(admin.Announcements) != before {
		t.Fatalf("校验失败不该落库，仍应为 %d 条，得 %d", before, len(admin.Announcements))
	}
}

// TestAnnounceUpdateWindowMerge 编辑时时限的「保持 / 覆盖 / 清空」三态。
//
// 用 *int64 的意义就在这里：只改正文不该把时限清掉（老前端不带这两个字段也安全），
// 而显式传 0 才是「清空这一侧」。
func TestAnnounceUpdateWindowMerge(t *testing.T) {
	store, px, a := newTestApp(t)
	defer store.Close()
	defer px.Close()
	if err := store.migrateAnnouncements(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	now := time.Now().Unix()
	end := now + 86400

	item, err := store.CreateAnnouncement("初版", "c", "info", true, false, 0, end)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	update := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/admin/announcements/"+itoa64(item.ID),
			strings.NewReader(body))
		req.SetPathValue("id", itoa64(item.ID))
		a.handleAdminUpdateAnnouncement(rec, req)
		return rec
	}

	// ① 不提供时限字段 → 保持原值（置顶同理，靠 *bool）。
	if rec := update(`{"title":"改标题","content":"改内容","level":"info"}`); rec.Code != http.StatusOK {
		t.Fatalf("编辑应 200，得 %d（%s）", rec.Code, rec.Body.String())
	}
	got, _ := store.GetAnnouncement(item.ID)
	if got.EndAt != end {
		t.Fatalf("未提供 end_at 应保持原值 %d，得 %d", end, got.EndAt)
	}
	if got.StartAt != 0 {
		t.Fatalf("未提供 start_at 应保持 0，得 %d", got.StartAt)
	}

	// ② 显式覆盖 start_at。
	if rec := update(`{"title":"改标题","content":"改内容","start_at":` + itoa64(now+60) + `}`); rec.Code != http.StatusOK {
		t.Fatalf("编辑应 200，得 %d", rec.Code)
	}
	got, _ = store.GetAnnouncement(item.ID)
	if got.StartAt != now+60 || got.EndAt != end {
		t.Fatalf("应覆盖 start 且保留 end，得 start=%d end=%d", got.StartAt, got.EndAt)
	}

	// ③ 显式传 0 → 清空该侧（改为「立即生效 / 永不过期」）。
	if rec := update(`{"title":"改标题","content":"改内容","start_at":0,"end_at":0}`); rec.Code != http.StatusOK {
		t.Fatalf("编辑应 200，得 %d", rec.Code)
	}
	got, _ = store.GetAnnouncement(item.ID)
	if got.StartAt != 0 || got.EndAt != 0 {
		t.Fatalf("显式 0 应清空时限，得 start=%d end=%d", got.StartAt, got.EndAt)
	}
	if got.State != annStateActive {
		t.Fatalf("清空时限后应为 active，得 %q", got.State)
	}

	// ④ 编辑成一个非法窗口（end <= start）→ 400，且原值不被破坏。
	if rec := update(`{"title":"改标题","content":"改内容","start_at":` + itoa64(now+100) + `,"end_at":` + itoa64(now+50) + `}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("非法窗口应 400，得 %d", rec.Code)
	}
	got, _ = store.GetAnnouncement(item.ID)
	if got.StartAt != 0 || got.EndAt != 0 {
		t.Fatalf("校验失败不该改库，得 start=%d end=%d", got.StartAt, got.EndAt)
	}
}

// itoa64 int64 → 十进制字符串（拼 JSON 请求体用；注意别用 itoa(0)，它返回 "3"）。
func itoa64(v int64) string { return strconv.FormatInt(v, 10) }
