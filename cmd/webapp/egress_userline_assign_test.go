// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 管理端「借用用户住宅 IP」的回归测试（v0.31.32）。
//
// 产品口径："用户自拉的出口 IP 给自己挂的号用；管理后台也可以借用，自由分配给
// 任意账号（账号代理 / 出口代理）。"
//
// 实现上保留了一条**知情边界**：自动分配默认不把 owner>0 的线路放进候选，
// 必须显式 `include_user_lines=true`。理由不是"不信任用户"，而是自动挑"最闲的一条"
// 会把平台账号的 access_token 随机送到某个用户家里 —— 这种事应该是管理员点过勾的。
//
// 两条断言：默认不进候选（安全）、勾选后进候选（可用）。两条缺一都不对。
package main

import (
	"net/http"
	"testing"
	"time"
)

// TestAdminAssignUserLineOptIn 自动分配：默认跳过用户线路，勾选后纳入。
func TestAdminAssignUserLineOptIn(t *testing.T) {
	store, _, mux, adminCookie, _ := eaApp(t)
	// 一个平台自持号（owner=0）+ 一条用户线路。
	acc := &Account{
		UID: "asg-plat-1", Nickname: "平台号", Realm: "cn",
		AccessToken: "t", RefreshToken: "r",
		ExpiresAt: time.Now().Add(time.Hour).Unix(), PoolState: "in",
	}
	if err := store.UpsertAccount(acc); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	u, err := store.CreateUser("asg-owner", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	userLine := "http://127.0.0.1:12070"
	if err := store.UpsertEgressProxy(&EgressProxy{
		ID: "asg-user-line", Name: "用户的住宅出口", URL: userLine,
		Enabled: true, LastOK: true, LastIP: "5.5.5.5",
		OwnerUserID: u.ID, CreatedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("UpsertEgressProxy: %v", err)
	}

	// 1) 默认（不勾）：没有可用线路 → 400，且**不能**把号写到用户线路上。
	code, raw := eaReq(t, mux, "POST", "/api/admin/egress/assign",
		`{"uids":["asg-plat-1"],"dry_run":true}`, adminCookie, "")
	if code != http.StatusBadRequest {
		t.Fatalf("默认不该把用户线路当自动候选（应 400 无可用线路），得 %d %s", code, raw)
	}
	if got, _ := store.GetAccount("asg-plat-1"); got.EgressProxy != "" {
		t.Fatalf("dry_run 不该写库，得 %q", got.EgressProxy)
	}

	// 2) 显式勾选 → 纳入候选，平台号被分到用户线路上。
	code, raw = eaReq(t, mux, "POST", "/api/admin/egress/assign",
		`{"uids":["asg-plat-1"],"include_user_lines":true}`, adminCookie, "")
	if code != http.StatusOK {
		t.Fatalf("勾选后应能分配到用户线路，得 %d %s", code, raw)
	}
	got, _ := store.GetAccount("asg-plat-1")
	if got.EgressProxy != userLine {
		t.Fatalf("平台号应被分配到用户线路 %q，得 %q", userLine, got.EgressProxy)
	}
	// 归属不能被"借用"改动：线路仍是用户的，账号仍是平台的。
	if p, _ := store.GetEgressProxy("asg-user-line"); p == nil || p.OwnerUserID != u.ID {
		t.Fatalf("借用不该改动线路归属，得 %+v", p)
	}
	if got.OwnerUserID != 0 {
		t.Fatalf("借用不该改动账号归属，得 %d", got.OwnerUserID)
	}
}
