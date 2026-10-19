// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 出口范围闸门的验收测试（v0.31.50，需求二 / R1·R2·R3·R12）。
//
// 这一组用例对应方案文档《三项需求设计与实施方案》2.8 的验收条款；
// 反例（"不该放行的必须拦"）比正例更重要 —— 这条链路的错误方向是把某个人的
// access_token 送进别人的家庭宽带，事后只能靠这张审计表追责。
package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// scopeLine 造一条线路（默认启用）。
func scopeLine(owner int64, url string, enabled bool) *EgressProxy {
	return &EgressProxy{ID: "pl-" + url, Name: url, URL: url, Enabled: enabled,
		OwnerUserID: owner, LastOK: true, LastIP: "1.2.3.4", CreatedAt: time.Now().Unix()}
}

// TestEgressLineAllowsRules 唯一闸门的判定顺序与 force 边界。
func TestEgressLineAllowsRules(t *testing.T) {
	userLine := scopeLine(7, "http://127.0.0.1:12070", true)
	platLine := scopeLine(0, "http://127.0.0.1:11080", true)
	offLine := scopeLine(7, "http://127.0.0.1:12071", false)
	manual := scopeLine(7, "http://127.0.0.1:12072", false)
	manual.ManualDisabled = true

	ownUnpooled := &Account{UID: "a1", OwnerUserID: 7, PoolState: ""}
	ownPooled := &Account{UID: "a2", OwnerUserID: 7, PoolState: "in"}
	foreign := &Account{UID: "a3", OwnerUserID: 9, PoolState: ""}

	cases := []struct {
		name  string
		line  *EgressProxy
		acc   *Account
		force bool
		ok    bool
		code  string
	}{
		{"本人未入池 → 放行", userLine, ownUnpooled, false, true, egressCodeOK},
		{"本人已入池 → 拦", userLine, ownPooled, false, false, egressCodePooled},
		{"别人的号 → 拦", userLine, foreign, false, false, egressCodeForeign},
		{"平台线路不限入池", platLine, ownPooled, false, true, egressCodePlatformScope},
		{"平台线路给任何人", platLine, foreign, false, true, egressCodePlatformScope},
		{"停用线路一律拦", offLine, ownUnpooled, false, false, egressCodeLineDisabled},
		{"force 不能越过停用", offLine, ownUnpooled, true, false, egressCodeLineDisabled},
		{"force 不能越过人工停用", manual, ownUnpooled, true, false, egressCodeLineDisabled},
		{"force 放开归属", userLine, foreign, true, true, egressCodeForcedForeign},
		{"force 放开入池", userLine, ownPooled, true, true, egressCodeForcedPooled},
	}
	for _, c := range cases {
		ok, code, _ := egressLineAllows(c.line, c.acc, egressAllowOpts{Force: c.force})
		if ok != c.ok || code != c.code {
			t.Fatalf("%s：得 ok=%v code=%s，期望 ok=%v code=%s", c.name, ok, code, c.ok, c.code)
		}
	}
}

// TestAssignBlocksThenForceWithAudit 管理端：默认拦下跨用户/已入池，force 必须带理由并落审计（R1）。
func TestAssignBlocksThenForceWithAudit(t *testing.T) {
	store, _, mux, adminCookie, _, memberID := ownerApp(t)

	other, err := store.CreateUser("scope-other", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := store.UpsertAccount(&Account{UID: "other-acct", Nickname: "外人", Realm: "cn",
		AccessToken: "t", RefreshToken: "r", OwnerUserID: other.ID, PoolState: ""}); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	const userURL = "http://203.0.113.7:3128"
	if err := store.UpsertEgressProxy(scopeLine(memberID, userURL, true)); err != nil {
		t.Fatalf("UpsertEgressProxy: %v", err)
	}

	// ① 不带 force → 拦下，账号不动。
	body := `{"uids":["other-acct"],"proxy_url":"` + userURL + `"}`
	code, raw := eaReq(t, mux, "POST", "/api/admin/egress/assign", body, adminCookie, "")
	if code != http.StatusOK {
		t.Fatalf("分配应 200（blocked 在响应里），得 %d %s", code, raw)
	}
	d := eaJSON(t, raw)
	blocked, _ := d["blocked"].([]any)
	if len(blocked) != 1 {
		t.Fatalf("应拦下 1 个账号，得 %v", d["blocked"])
	}
	if b, _ := blocked[0].(map[string]any); b["code"] != egressCodeForeign {
		t.Fatalf("拦截码应为 foreign_account，得 %v", blocked[0])
	}
	if acc, _ := store.GetAccount("other-acct"); acc != nil && strings.TrimSpace(acc.EgressProxy) != "" {
		t.Fatalf("被拦下的账号不该被写：%q", acc.EgressProxy)
	}

	// ② force 但理由太短 → 400，且一个号都没改。
	code, raw = eaReq(t, mux, "POST", "/api/admin/egress/assign",
		`{"uids":["other-acct"],"proxy_url":"`+userURL+`","force_scope":true,"force_reason":"测试"}`, adminCookie, "")
	if code != http.StatusBadRequest || !strings.Contains(raw, "force_reason_required") {
		t.Fatalf("理由不足应 400 force_reason_required，得 %d %s", code, raw)
	}
	if acc, _ := store.GetAccount("other-acct"); acc != nil && acc.EgressProxy != "" {
		t.Fatalf("被拒绝的请求不该写库：%q", acc.EgressProxy)
	}

	// ③ force + 合规理由 → 写入 + 审计 1 条。
	const reason = "跨用户排障：该出口是测试环境共用线路，已获号主确认"
	code, raw = eaReq(t, mux, "POST", "/api/admin/egress/assign",
		`{"uids":["other-acct"],"proxy_url":"`+userURL+`","force_scope":true,"force_reason":"`+reason+`"}`,
		adminCookie, "")
	if code != http.StatusOK {
		t.Fatalf("强制越界应 200，得 %d %s", code, raw)
	}
	if acc, _ := store.GetAccount("other-acct"); acc == nil || acc.EgressProxy != userURL {
		t.Fatalf("强制越界后应写入，得 %+v", acc)
	}
	audits, err := store.ListEgressForceAudits(10, "")
	if err != nil {
		t.Fatalf("ListEgressForceAudits: %v", err)
	}
	if len(audits) != 1 {
		t.Fatalf("应落 1 条审计，得 %d", len(audits))
	}
	a := audits[0]
	if a.Reason != reason || a.Result != "applied" || len(a.UIDs) != 1 || a.UIDs[0] != "other-acct" {
		t.Fatalf("审计内容不完整：%+v", a)
	}
	if a.Violations["other-acct"] != egressCodeForcedForeign {
		t.Fatalf("审计应记录违规码 forced_foreign，得 %v", a.Violations)
	}
	if a.AdminName == "" || a.AdminID == 0 {
		t.Fatalf("审计必须记下是谁做的：%+v", a)
	}

	// ④ 废弃字段必须被明确拒绝（而不是静默忽略）。
	code, raw = eaReq(t, mux, "POST", "/api/admin/egress/assign",
		`{"uids":["other-acct"],"include_user_lines":true}`, adminCookie, "")
	if code != http.StatusBadRequest || !strings.Contains(raw, "include_user_lines_deprecated") {
		t.Fatalf("include_user_lines 应 400，得 %d %s", code, raw)
	}
}

// TestSetAccountEgressRejectsPooled 单账号改出口也要过闸门（拦截点 3）。
func TestSetAccountEgressRejectsPooled(t *testing.T) {
	store, _, a := newTestApp(t)
	defer store.Close()
	eaAccount(t, store, "sc-1", 7)
	const userURL = "http://127.0.0.1:12075"
	if err := store.UpsertEgressProxy(scopeLine(7, userURL, true)); err != nil {
		t.Fatalf("UpsertEgressProxy: %v", err)
	}
	if err := store.SetAccountPoolState("sc-1", "in", ""); err != nil {
		t.Fatalf("SetAccountPoolState: %v", err)
	}

	_, err := a.setAccountEgress("sc-1", userURL)
	if err == nil {
		t.Fatal("已入池的号不许改到个人出口")
	}
	var se *egressScopeError
	if !errors.As(err, &se) || se.Code != egressCodePooled {
		t.Fatalf("应返回 account_pooled，得 %v", err)
	}
	if acc, _ := store.GetAccount("sc-1"); acc != nil && acc.EgressProxy != "" {
		t.Fatalf("被拒绝的写入不该落到账号上：%q", acc.EgressProxy)
	}
}

// TestEgressReadPathDropsViolation 读路径兜底（R12）：脏数据不会真的被拿去出站。
func TestEgressReadPathDropsViolation(t *testing.T) {
	userLine := scopeLine(7, "http://127.0.0.1:12070", true)
	accs := []*Account{
		{UID: "dirty", OwnerUserID: 7, PoolState: "in"}, // 已入池却绑在个人线路上
		{UID: "clean", OwnerUserID: 7, PoolState: ""},
	}
	raw := map[string]string{
		"dirty": strings.ToUpper(userLine.URL), // 写法不同也要能认出来
		"clean": userLine.URL,
	}
	out, dropped := egressScopeFilter(raw, []*EgressProxy{userLine}, accs)
	if dropped != 1 {
		t.Fatalf("应降级 1 条越界映射，得 %d", dropped)
	}
	if _, ok := out["dirty"]; ok {
		t.Fatal("越界映射必须被删掉（降级为跟随默认出口）")
	}
	if _, ok := out["clean"]; !ok {
		t.Fatal("合规映射不能被误删")
	}
}

// TestEgressComplianceListsViolation 越界存量必须可见（R11 的 P0 部分）。
func TestEgressComplianceListsViolation(t *testing.T) {
	store, a, mux, adminCookie, _, memberID := ownerApp(t)
	const userURL = "http://203.0.113.8:3128"
	if err := store.UpsertEgressProxy(scopeLine(memberID, userURL, true)); err != nil {
		t.Fatalf("UpsertEgressProxy: %v", err)
	}
	// 绕过闸门直接写库，模拟历史脏数据。
	if err := store.SetAccountEgress("ow-acct", userURL); err != nil {
		t.Fatalf("SetAccountEgress: %v", err)
	}
	if err := store.SetAccountPoolState("ow-acct", "in", ""); err != nil {
		t.Fatalf("SetAccountPoolState: %v", err)
	}

	code, raw := eaReq(t, mux, "GET", "/api/admin/egress/compliance", "", adminCookie, "")
	if code != http.StatusOK {
		t.Fatalf("compliance 应 200，得 %d %s", code, raw)
	}
	d := eaJSON(t, raw)
	if n, _ := d["total"].(float64); n < 1 {
		t.Fatalf("应报出至少 1 条越界，得 %v", d["total"])
	}
	list, _ := d["violations"].([]any)
	found := false
	for _, it := range list {
		if m, _ := it.(map[string]any); m["uid"] == "ow-acct" && m["code"] == egressCodePooled {
			found = true
		}
	}
	if !found {
		t.Fatalf("越界清单应包含 ow-acct/account_pooled，得 %v", list)
	}
	_ = a
}

// TestManualDisableBlocksApplyAndSurvivesEdit 人工停用：拉不活、编辑也不丢（R3/R10）。
//
// v0.31.53 起 apply 只剩平台视角（用户端能力已下线），但这条不变量仍然成立：
// **人工停用是管理员的决定**，任何"拉起/应用"都不能把它翻过来。
func TestManualDisableBlocksApplyAndSurvivesEdit(t *testing.T) {
	store, a, mux, adminCookie, memberCookie, memberID := ownerApp(t)
	// 必须用**该用户端口段内**的地址：applyHomeEgress 是按隧道端口拼 URL 的，
	// 拿一个任意外部代理地址去测，会走到"另一条线路"，永远测不到人工停用这条。
	base, _ := egressPortSegOf(memberID)
	userURL := egressHomeURL(base)
	line := scopeLine(memberID, userURL, true)
	line.ID = "p-manual"
	if err := store.UpsertEgressProxy(line); err != nil {
		t.Fatalf("UpsertEgressProxy: %v", err)
	}

	// 管理员人工停用。
	code, raw := eaReq(t, mux, "POST", "/api/admin/egress/proxies/manual-disable",
		`{"id":"p-manual","disabled":true,"note":"线路主人要求下线"}`, adminCookie, "")
	if code != http.StatusOK {
		t.Fatalf("人工停用应 200，得 %d %s", code, raw)
	}

	// 重新拉起（apply）→ 409 语义的错误，且线路仍停用。
	a.egressVerify = func(string) (string, error) { return "198.51.100.13", nil }
	_, err := a.applyHomeEgress(base, egressScope{Platform: true})
	if !errors.Is(err, errEgressLineManuallyDisabled) {
		t.Fatalf("人工停用的线路应拒绝启用，得 %v", err)
	}
	got, _ := store.GetEgressProxy("p-manual")
	if got == nil || got.Enabled || !got.ManualDisabled {
		t.Fatalf("线路必须保持停用+人工标记，得 %+v", got)
	}

	// 编辑名字（管理员改备注）不得把人工停用位清掉。
	code, raw = eaReq(t, mux, "POST", "/api/admin/egress/proxies",
		`{"id":"p-manual","name":"改个名","url":"`+userURL+`","enabled":true,"check":false}`,
		adminCookie, "")
	if code != http.StatusOK {
		t.Fatalf("编辑线路应 200，得 %d %s", code, raw)
	}
	got, _ = store.GetEgressProxy("p-manual")
	if got == nil || !got.ManualDisabled {
		t.Fatalf("编辑不得撤销人工停用，得 %+v", got)
	}
	_ = memberCookie
}

// usTunnelPortFor 从线路 URL 反推隧道端口（apply 用端口拼 URL，必须一致）。
func usTunnelPortFor(t *testing.T, url string) int {
	t.Helper()
	u := strings.TrimPrefix(url, "http://127.0.0.1:")
	n := 0
	for _, c := range u {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	if n == 0 {
		t.Fatalf("无法从 %q 解析端口", url)
	}
	return n
}
