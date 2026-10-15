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

// adminReq 造一个带管理密钥 + 已登录管理员的请求。
//
// 为什么要塞 User 进 context：handler 用 `userFrom(r)` 取"操作人"来写审计，
// 不注入的话 admin_id/admin_name 会是空 —— 那恰好是审计最该记的两个字段，
// 测试就会"通过但什么都没验证"。
func adminReq(method, target, body string, admin *User) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	r.Header.Set("Content-Type", "application/json")
	if admin != nil {
		r = withUser(r, admin)
	}
	return r
}

// ======================================================================
// v0.27.0 · 余额人工调整 + 审计
// ======================================================================

// TestAdminSetBalanceWritesAudit 每次余额调整都必须留下完整凭据。
//
// 用户报的是"余额增减功能失效"，而修复的同时必须解决更根本的问题：
// 这个功能此前**改完什么都不记** —— 钱动了却没有凭据，事后对不上账只能互相猜。
func TestAdminSetBalanceWritesAudit(t *testing.T) {
	store, px, a := newTestApp(t, &Account{UID: "u1", AccessToken: "t"})
	defer store.Close()
	defer px.Close()

	u, err := store.CreateUser("victim", "x")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddUserBalance(u.ID, 100*MicroPerPoint); err != nil {
		t.Fatal(err)
	}
	admin := &User{ID: 42, Username: "op-alpha"}

	req := adminReq(http.MethodPost, "/api/admin/users/"+itoa64(u.ID)+"/balance",
		`{"delta":50,"reason":"客服补偿"}`, admin)
	req.SetPathValue("id", itoa64(u.ID))
	rec := httptest.NewRecorder()
	a.handleAdminSetBalance(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	// 回显"改前 / 改后"：只有"已调整"的提示无法让人确认到底成没成。
	if got["before"] != float64(100) || got["after"] != float64(150) {
		t.Errorf("应回显变动前后 100→150，得 %v→%v", got["before"], got["after"])
	}

	rows, err := store.ListBalanceAudits(u.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("应落 1 条审计，得 %d", len(rows))
	}
	r := rows[0]
	if r.AdminID != 42 || r.AdminName != "op-alpha" {
		t.Errorf("必须记录操作人：admin_id=%d admin_name=%q", r.AdminID, r.AdminName)
	}
	if r.DeltaMicro != 50*MicroPerPoint {
		t.Errorf("变动值应为 +50 点，得 %d 微点", r.DeltaMicro)
	}
	if r.BeforeMicro != 100*MicroPerPoint || r.AfterMicro != 150*MicroPerPoint {
		t.Errorf("快照应为 100→150 点，得 %d→%d", r.BeforeMicro, r.AfterMicro)
	}
	if r.Reason != "客服补偿" || r.Result != auditOK {
		t.Errorf("原因/结果不符：reason=%q result=%q", r.Reason, r.Result)
	}
	if r.CreatedAt <= 0 {
		t.Error("必须记录操作时间")
	}
}

// TestAdminSetBalanceAuditsFailure 失败也要留痕。
//
// "我明明点了却没生效"这类追问，答案往往就在失败记录里（用户不存在 / 余额被
// 其它操作改动）。只记成功的审计在排障时等于没有。
func TestAdminSetBalanceAuditsFailure(t *testing.T) {
	store, px, a := newTestApp(t, &Account{UID: "u1", AccessToken: "t"})
	defer store.Close()
	defer px.Close()

	admin := &User{ID: 7, Username: "op-beta"}
	req := adminReq(http.MethodPost, "/api/admin/users/99999/balance", `{"delta":10}`, admin)
	req.SetPathValue("id", "99999")
	rec := httptest.NewRecorder()
	a.handleAdminSetBalance(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的用户应 404，得 %d", rec.Code)
	}

	rows, err := store.ListBalanceAudits(99999, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("失败操作也应留 1 条审计，得 %d", len(rows))
	}
	if rows[0].Result != auditRejected || rows[0].Error == "" {
		t.Errorf("失败审计必须标明结果与原因：%+v", rows[0])
	}
}

// TestAdminSetBalanceRejectsNonFinite delta 为 NaN/Inf 必须在动账前挡下。
//
// 它们能通过 `delta != 0` 判断，却会把 PointToMicro 的结果变成一个荒谬的整数
// —— 直接污染余额且事后难以解释。
func TestAdminSetBalanceRejectsNonFinite(t *testing.T) {
	store, px, a := newTestApp(t, &Account{UID: "u1", AccessToken: "t"})
	defer store.Close()
	defer px.Close()
	u, _ := store.CreateUser("nb", "x")
	_ = store.AddUserBalance(u.ID, 10*MicroPerPoint)

	for _, body := range []string{`{"delta":0}`, `{"delta":1e400}`, `{"delta":-1e400}`} {
		req := adminReq(http.MethodPost, "/x/balance", body, &User{ID: 1, Username: "op"})
		req.SetPathValue("id", itoa64(u.ID))
		rec := httptest.NewRecorder()
		a.handleAdminSetBalance(rec, req)
		if rec.Code == http.StatusOK {
			t.Errorf("body=%s 不该被接受", body)
		}
	}
	fresh, _ := store.GetUserByID(u.ID)
	if fresh.BalanceMicro != 10*MicroPerPoint {
		t.Errorf("被拒的请求不得改动余额，得 %d", fresh.BalanceMicro)
	}
}

// ======================================================================
// v0.27.0 · 计费倍率历史 + 自动公告
// ======================================================================

// TestMultiplierChangeRecordedAndAnnounced 改倍率 → 记历史 + **自动**发公告。
//
// 用户的要求是"调整后自动同步并向用户控制台发送临时公告"。靠人记得发就会忘，
// 而涨价不通知是最伤信任的一种做法，所以这件事必须是写入路径的一部分。
func TestMultiplierChangeRecordedAndAnnounced(t *testing.T) {
	store, px, a := newTestApp(t, &Account{UID: "u1", AccessToken: "t"})
	defer store.Close()
	defer px.Close()

	if got := store.PricingMultiplier(); got != defaultMultiplier {
		t.Fatalf("初始倍率应回落到默认 %v，得 %v", defaultMultiplier, got)
	}
	admin := &User{ID: 3, Username: "pricer"}
	req := adminReq(http.MethodPost, "/api/admin/settings",
		`{"multiplier":2.5,"multiplier_note":"算力成本上涨"}`, admin)
	rec := httptest.NewRecorder()
	a.handleAdminSettings(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if got := store.PricingMultiplier(); got != 2.5 {
		t.Fatalf("倍率应为 2.5，得 %v", got)
	}

	hist, err := store.ListMultiplierHistory(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 {
		t.Fatalf("应记 1 条变更历史，得 %d", len(hist))
	}
	h := hist[0]
	if h.OldValue != defaultMultiplier || h.NewValue != 2.5 {
		t.Errorf("调整前后应为 %v→2.5，得 %v→%v", defaultMultiplier, h.OldValue, h.NewValue)
	}
	if h.AdminName != "pricer" || h.AdminID != 3 {
		t.Errorf("必须记录调整人：%+v", h)
	}
	if h.CreatedAt <= 0 {
		t.Error("必须记录调整时间")
	}
	if !h.Announced || h.AnnounceID <= 0 {
		t.Fatalf("必须已自动发出公告：%+v", h)
	}
	if d := h.Direction(); d != "up" {
		t.Errorf("涨价方向应为 up，得 %q", d)
	}
	if p := h.ChangePercent(); p < 149 || p > 151 {
		t.Errorf("涨幅应约 +150%%，得 %.1f", p)
	}

	// 公告必须真的对用户可见，且带时限（临时公告）。
	ann, err := store.GetAnnouncement(h.AnnounceID)
	if err != nil {
		t.Fatalf("公告应存在：%v", err)
	}
	if !ann.Enabled || ann.EndAt <= ann.StartAt {
		t.Errorf("公告应是启用中且有明确时限的临时公告：%+v", ann)
	}
	if !strings.Contains(ann.Title, "上调") {
		t.Errorf("标题应说明方向，得 %q", ann.Title)
	}
	if !strings.Contains(ann.Content, "算力成本上涨") {
		t.Errorf("公告正文应带上调价说明，得 %q", ann.Content)
	}
	if ann.Level != "warn" {
		t.Errorf("涨价公告应为 warn 级别，得 %q", ann.Level)
	}
	// 用户侧可见性：必须能被 /api/announcements 那条 SQL 口径取到。
	list, err := store.ListAnnouncements(true, 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, x := range list {
		if x.ID == ann.ID {
			found = true
		}
	}
	if !found {
		t.Error("自动公告必须出现在用户侧可见列表里")
	}
}

// TestMultiplierUnchangedRecordsNothing 值没变就不该留记录。
//
// "保存设置"这个动作前端会带着当前值一起提交（回显后再保存），把它也记一笔会让
// 历史里塞满 1→1 的空记录 —— 而这张表存在的理由恰恰是回答"上次什么时候调的"。
func TestMultiplierUnchangedRecordsNothing(t *testing.T) {
	store, px, a := newTestApp(t, &Account{UID: "u1", AccessToken: "t"})
	defer store.Close()
	defer px.Close()

	req := adminReq(http.MethodPost, "/api/admin/settings",
		`{"multiplier":1}`, &User{ID: 1, Username: "op"})
	rec := httptest.NewRecorder()
	a.handleAdminSettings(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	hist, _ := store.ListMultiplierHistory(10)
	if len(hist) != 0 {
		t.Fatalf("倍率未变化不应留记录，得 %d 条", len(hist))
	}
}

// TestMultiplierDecreaseUsesInfoLevel 降价公告用 info：好消息不必吓人。
func TestMultiplierDecreaseUsesInfoLevel(t *testing.T) {
	title, content, level := multiplierAnnounce(2, 1, "")
	if !strings.Contains(title, "下调") {
		t.Errorf("标题应说明下调，得 %q", title)
	}
	if level != "info" {
		t.Errorf("降价应为 info 级别，得 %q", level)
	}
	if !strings.Contains(content, "-50.0%") {
		t.Errorf("正文应给出降幅，得 %q", content)
	}
	// 无备注时不得多出一行空的"调整说明："。
	if strings.Contains(content, "调整说明") {
		t.Errorf("无备注不该出现说明行，得 %q", content)
	}
}

// TestMultiplierHistoryAPIExposesAdminButUserAPIDoesNot 两侧口径隔离。
//
// 后台需要知道"谁调的"（追责与协作），用户只需要知道"什么时候、从多少到多少"
// —— 向用户暴露管理员账号既无必要，也会把内部人事信息带出去。
func TestMultiplierHistoryAPIExposesAdminButUserAPIDoesNot(t *testing.T) {
	store, px, a := newTestApp(t, &Account{UID: "u1", AccessToken: "t"})
	defer store.Close()
	defer px.Close()

	if _, err := a.store.RecordMultiplierChange(1, 3, 9, "boss", "调价"); err != nil {
		t.Fatal(err)
	}

	// 后台
	rec := httptest.NewRecorder()
	a.handleAdminMultiplierHistory(rec, httptest.NewRequest(http.MethodGet, "/api/admin/multiplier-history", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("后台接口 code=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "boss") {
		t.Error("后台历史必须含调整人")
	}
	if !strings.Contains(rec.Body.String(), `"current"`) {
		t.Error("后台历史应同时给出当前倍率（少一次往返）")
	}

	// 用户侧
	rec2 := httptest.NewRecorder()
	a.handleMultiplier(rec2, httptest.NewRequest(http.MethodGet, "/api/multiplier", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("用户侧接口 code=%d", rec2.Code)
	}
	body := rec2.Body.String()
	if strings.Contains(body, "boss") || strings.Contains(body, "admin_name") {
		t.Errorf("用户侧不得暴露管理员信息，得：%s", body)
	}
	if !strings.Contains(body, `"direction"`) {
		t.Error("用户侧应给出方向（涨/降），供趋势展示")
	}
	if !strings.Contains(body, "调价") {
		t.Error("用户侧应保留备注（它本来就是给用户看的说明）")
	}
}

// TestMultiplierSettingsRejectsOutOfRange 非法倍率被拒且不留痕。
func TestMultiplierSettingsRejectsOutOfRange(t *testing.T) {
	store, px, a := newTestApp(t, &Account{UID: "u1", AccessToken: "t"})
	defer store.Close()
	defer px.Close()

	for _, body := range []string{`{"multiplier":0}`, `{"multiplier":-3}`, `{"multiplier":1001}`} {
		req := adminReq(http.MethodPost, "/api/admin/settings", body, &User{ID: 1, Username: "op"})
		rec := httptest.NewRecorder()
		a.handleAdminSettings(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body=%s 应被拒（400），得 %d", body, rec.Code)
		}
	}
	if hist, _ := store.ListMultiplierHistory(10); len(hist) != 0 {
		t.Errorf("被拒的提交不该留历史，得 %d 条", len(hist))
	}
	if store.PricingMultiplier() != defaultMultiplier {
		t.Error("被拒的提交不该改动倍率")
	}
}
