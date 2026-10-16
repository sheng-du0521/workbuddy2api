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

// 邀请入池语义（v0.31.18）。
//
// 背景（用户现场）：号主在控制台挂了号、**没有点「申请入池」**，
// 管理后台却显示/能落成「已批准入池」。根因是管理侧的 approve 对**任何**
// 代挂号都直接落 "in"，于是管理员可以单方面把一个号主从未同意的号拉进
// 平台号池去消耗他的积分，而界面文案写的是"批准入池"（听起来像"同意了他的申请"）。
//
// 本组用例钉住的行为：
//
//	未申请 + 管理员 approve → invited（**邀请**，等号主确认），且 invited 期间不参与平台反代；
//	已申请(pending) + 管理员 approve → in（审批路径不变）；
//	号主 accept → in；号主 decline → 空（退回未入池）；
//	非 invited 状态下 accept/decline 一律 400（否则等于给了一条绕过邀请的暗道）。
func TestInviteToPoolSemantics(t *testing.T) {
	store, px, a := newTestApp(t,
		&Account{UID: "h_new", Realm: "cn", AccessToken: "a", OwnerUserID: 7, PoolState: ""},
	)
	defer store.Close()
	_ = px.SyncAccounts()
	// 积分读数写回（模拟用户在卡片上查过一次积分）——accept 与 approve 都要过门槛。
	px.pool.SetCreditsDetailed("h_new", 5000, 0)

	owner := &User{ID: 7, Username: "owner", Role: "user", Status: StatusActive}

	review := func(action string) (int, map[string]any) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/admin/host/accounts/h_new/review",
			strings.NewReader(`{"action":"`+action+`"}`))
		req.SetPathValue("uid", "h_new")
		a.handleAdminHostReview(rec, req)
		var m map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		return rec.Code, m
	}
	poolAct := func(action string) (int, map[string]any) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/host/accounts/h_new/pool",
			strings.NewReader(`{"action":"`+action+`"}`))
		req.SetPathValue("uid", "h_new")
		req = withUser(req, owner)
		a.handleHostPool(rec, req)
		var m map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		return rec.Code, m
	}
	state := func() string {
		acc, err := store.GetAccount("h_new")
		if err != nil || acc == nil {
			t.Fatalf("GetAccount: %v", err)
		}
		return acc.PoolState
	}

	// ---- 0) 起点：未申请 ----
	if got := state(); got != "" {
		t.Fatalf("起点应为未入池空态，得 %q", got)
	}

	// ---- 1) 未被邀请时 accept 必须被拒（否则存在绕过邀请的暗道）----
	if code, m := poolAct("accept"); code != http.StatusBadRequest {
		t.Fatalf("未被邀请时 accept 应 400，得 %d %v", code, m)
	}

	// ---- 2) 管理员对未申请账号 approve → **邀请**，不是入池 ----
	if code, m := review("approve"); code != http.StatusOK {
		t.Fatalf("邀请应成功，得 %d %v", code, m)
	}
	if got := state(); got != "invited" {
		t.Fatalf("未申请时 approve 必须落 invited（邀请）；直接落 in 等于绕过号主意愿。得 %q", got)
	}
	// 最关键的一条：invited 期间**不得**参与平台反代 —— 否则"邀请"和"入池"没区别。
	if px.allowsSelection("h_new", pickScope{}) {
		t.Fatal("invited 状态不该被平台选号（否则邀请就已经等于入池了）")
	}

	// ---- 3) 号主谢绝 → 退回未入池（且被选号范围继续排除）----
	if code, m := poolAct("decline"); code != http.StatusOK {
		t.Fatalf("谢绝应成功，得 %d %v", code, m)
	}
	if got := state(); got != "" {
		t.Fatalf("谢绝后应回到未入池空态，得 %q", got)
	}

	// ---- 4) 再邀请一次 → 号主接受 → 真正入池 ----
	if code, _ := review("approve"); code != http.StatusOK {
		t.Fatalf("二次邀请应成功，得 %d", code)
	}
	if code, m := poolAct("accept"); code != http.StatusOK {
		t.Fatalf("接受邀请应成功，得 %d %v", code, m)
	}
	if got := state(); got != "in" {
		t.Fatalf("接受后应入池，得 %q", got)
	}
	if !px.allowsSelection("h_new", pickScope{}) {
		t.Fatal("入池后应参与平台选号")
	}
}

// TestApprovePendingStillEntersPool 审批路径未被邀请语义破坏。
//
// 号主自己申请（pending）后，管理员 approve 仍应**直接入池** ——
// 邀请分流只针对"号主没申请过"的情形。若这条也变成 invited，
// 申请入池的号主会卡在一个自己不肯点的按钮上（他明明已经同意了）。
func TestApprovePendingStillEntersPool(t *testing.T) {
	store, px, a := newTestApp(t,
		&Account{UID: "h_p", Realm: "cn", AccessToken: "a", OwnerUserID: 9, PoolState: "pending"},
	)
	defer store.Close()
	_ = px.SyncAccounts()
	px.pool.SetCreditsDetailed("h_p", 5000, 0)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/host/accounts/h_p/review",
		strings.NewReader(`{"action":"approve"}`))
	req.SetPathValue("uid", "h_p")
	a.handleAdminHostReview(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("审批应成功，得 %d %s", rec.Code, rec.Body.String())
	}
	acc, err := store.GetAccount("h_p")
	if err != nil || acc == nil {
		t.Fatalf("GetAccount: %v", err)
	}
	if acc.PoolState != "in" {
		t.Fatalf("号主已申请的号被审批后应直接入池，得 %q", acc.PoolState)
	}
}

// TestRevokeInviteByAdmin 管理员撤回邀请 = 落回未入池（不是驳回申请）。
func TestRevokeInviteByAdmin(t *testing.T) {
	store, px, a := newTestApp(t,
		&Account{UID: "h_i", Realm: "cn", AccessToken: "a", OwnerUserID: 11, PoolState: "invited"},
	)
	defer store.Close()
	_ = px.SyncAccounts()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/host/accounts/h_i/review",
		strings.NewReader(`{"action":"reject","reason":"分组不符"}`))
	req.SetPathValue("uid", "h_i")
	a.handleAdminHostReview(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("撤回邀请应成功，得 %d %s", rec.Code, rec.Body.String())
	}
	acc, err := store.GetAccount("h_i")
	if err != nil || acc == nil {
		t.Fatalf("GetAccount: %v", err)
	}
	// 撤回邀请落回空态（而不是 rejected）——"被撤回的邀请"与"被驳回的申请"是两件事，
	// 前者号主还能再被邀请，后者需要重新申请。
	if acc.PoolState != "" {
		t.Fatalf("撤回邀请应落回未入池空态，得 %q", acc.PoolState)
	}
	if !strings.Contains(acc.HostNote, "撤回") {
		t.Fatalf("撤回痕迹应留在备注里，得 %q", acc.HostNote)
	}
}
