// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// checkinStatusPathV2 与 internal/upstream/checkin_status.go 的常量同值。
// 这里写成字面量是刻意的：若上游路径被改动，本测试应当失败并提醒同步核对，
// 而不是跟着一起改（那会让测试失去告警价值）。
const checkinStatusPathV2 = "/v2/billing/meter/checkin-activity-status"

// newTaskStatusApp 造一个 app，并把上游 base 指向两个 httptest：
// billing 域（签到状态）与 chat 域（buddy info / travel status）。
// 让 queryTaskStatus 的完整链路（含路径分派与字段映射）可被离线验证。
func newTaskStatusApp(t *testing.T, bill http.HandlerFunc, chat http.HandlerFunc) (*Store, *app) {
	t.Helper()
	store, px, a := newTestApp(t, &Account{UID: "u1", Realm: "cn", AccessToken: "at", Nickname: "演示账号"})
	billSrv := httptest.NewServer(bill)
	t.Cleanup(billSrv.Close)
	chatSrv := httptest.NewServer(chat)
	t.Cleanup(chatSrv.Close)
	px.uc.BillingBaseCN = billSrv.URL
	px.uc.ChatBaseCN = chatSrv.URL
	px.uc.HTTP = billSrv.Client() // 两个 server 都是明文回环，可共用同一 client
	return store, a
}

// TestTaskStatusCheckinAndTravel 正常链路：签到 true + 有猫 + 在途。
func TestTaskStatusCheckinAndTravel(t *testing.T) {
	bill := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != checkinStatusPathV2 {
			t.Errorf("签到应走新接口，实际 %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"today_checked_in":true}}`))
	}
	chat := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/buddy/info"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"buddy":{"id":1,"name":"咪"}}}`))
		case strings.HasSuffix(r.URL.Path, "/travel/status"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"state":"traveling","daily_limit_reached":false,"record_id":7,"reward_credit":0}}`))
		default:
			t.Errorf("意外路径 %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":404}`))
		}
	}
	store, a := newTaskStatusApp(t, bill, chat)
	defer store.Close()

	acc, _ := a.store.GetAccount("u1")
	v := a.taskStatus(acc)
	if !v.Checkin.Supported || !v.Checkin.OK || !v.Checkin.CheckedIn {
		t.Fatalf("签到应为已签到，得 %+v", v.Checkin)
	}
	if !v.Travel.Supported || !v.Travel.OK || v.Travel.State != "traveling" {
		t.Fatalf("旅行应为 traveling，得 %+v", v.Travel)
	}
	if v.Cached {
		t.Fatal("首次查询不应命中缓存")
	}
	// 第二次应命中缓存（不再打上游）。
	v2 := a.taskStatus(acc)
	if !v2.Cached {
		t.Fatal("第二次查询应命中缓存")
	}
	if v2.Checkin != v.Checkin || v2.Travel != v.Travel {
		t.Fatalf("缓存内容应一致：%+v vs %+v", v2, v)
	}
}

// TestTaskStatusNoBuddy buddy 为 null → 「无猫」而不是「未旅行」，且不算错误。
func TestTaskStatusNoBuddy(t *testing.T) {
	bill := func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"today_checked_in":false}}`))
	}
	chat := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/buddy/info") {
			_, _ = w.Write([]byte(`{"code":0,"data":{"buddy":null}}`))
			return
		}
		t.Errorf("无猫时不应再查旅行状态，实际 %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
	store, a := newTaskStatusApp(t, bill, chat)
	defer store.Close()

	v := a.taskStatus(mustAccount(t, a, "u1"))
	if v.Checkin.CheckedIn {
		t.Fatal("应为未签到")
	}
	if v.Travel.State != "no-buddy" || !v.Travel.OK || v.Travel.Error != "" {
		t.Fatalf("应为无猫且无错误，得 %+v", v.Travel)
	}
}

// TestTaskStatusBuddyGate 领养门槛未达标（HTTP 400 + first_buddy）→ 等同无猫，不当错误。
func TestTaskStatusBuddyGate(t *testing.T) {
	bill := func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"today_checked_in":true}}`))
	}
	chat := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":400,"msg":"first_buddy task not completed yet"}`))
	}
	store, a := newTaskStatusApp(t, bill, chat)
	defer store.Close()

	v := a.taskStatus(mustAccount(t, a, "u1"))
	if v.Travel.State != "no-buddy" || v.Travel.Error != "" {
		t.Fatalf("门槛未达标应归为无猫，得 %+v", v.Travel)
	}
}

// TestTaskStatusCheckinFailKeepsTravel 签到查询失败不影响旅行展示（子项互不牵连）。
func TestTaskStatusCheckinFailKeepsTravel(t *testing.T) {
	bill := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":500,"msg":"boom"}`))
	}
	chat := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/buddy/info") {
			_, _ = w.Write([]byte(`{"code":0,"data":{"buddy":{"id":1}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"state":"idle","daily_limit_reached":true}}`))
	}
	store, a := newTaskStatusApp(t, bill, chat)
	defer store.Close()

	v := a.taskStatus(mustAccount(t, a, "u1"))
	if v.Checkin.OK || v.Checkin.Error == "" {
		t.Fatalf("签到应报失败并带原因，得 %+v", v.Checkin)
	}
	if !v.Travel.OK || v.Travel.State != "idle" || !v.Travel.DailyLimitReached {
		t.Fatalf("旅行应照常展示，得 %+v", v.Travel)
	}
}

// TestTaskStatusCacheTTL 缓存条目过期后应重新查询。
func TestTaskStatusCacheTTL(t *testing.T) {
	c := &taskStatusCache{}
	now := time.Now()
	v := taskStatusView{UID: "u1"}
	c.put("u1", v, now)
	if got, ok := c.get("u1", now.Add(taskStatusTTL/2)); !ok || got.UID != "u1" {
		t.Fatal("未过期应命中")
	}
	if _, ok := c.get("u1", now.Add(taskStatusTTL)); ok {
		t.Fatal("到达 TTL 应视为过期")
	}
}

func mustAccount(t *testing.T, a *app, uid string) *Account {
	t.Helper()
	acc, err := a.store.GetAccount(uid)
	if err != nil || acc == nil {
		t.Fatalf("GetAccount(%s): %v", uid, err)
	}
	return acc
}

// TestSyncReportDiff 重同步计数：首次只建基线，之后正确报新增/移出。
func TestSyncReportDiff(t *testing.T) {
	store, px, _ := newTestApp(t, &Account{UID: "u1", Realm: "cn", AccessToken: "at"})
	defer store.Close()

	r1, err := px.SyncAccountsReport()
	if err != nil {
		t.Fatalf("首次同步: %v", err)
	}
	if r1.Total != 1 || r1.InPool != 1 || r1.Added != 0 || r1.Removed != 0 {
		t.Fatalf("首次同步应只建基线（added/removed=0），得 %+v", r1)
	}

	// 新增一个 → added=1。
	if err := store.UpsertAccount(&Account{UID: "u2", Realm: "cn", AccessToken: "at"}); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	r2, err := px.SyncAccountsReport()
	if err != nil {
		t.Fatalf("二次同步: %v", err)
	}
	if r2.Added != 1 || r2.Removed != 0 || r2.InPool != 2 {
		t.Fatalf("应新增 1 条，得 %+v", r2)
	}

	// 删除一个 → removed=1。
	if err := store.DeleteAccount("u1"); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	r3, err := px.SyncAccountsReport()
	if err != nil {
		t.Fatalf("三次同步: %v", err)
	}
	if r3.Removed != 1 || r3.Added != 0 || r3.InPool != 1 {
		t.Fatalf("应移出 1 条，得 %+v", r3)
	}
}
