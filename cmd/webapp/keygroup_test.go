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

// ======================================================================
// 号池计费密钥的「分组切换」（v0.31.88）
//
// 这块要钉住的不是"能改字段"，而是四个判断的顺序与语义：
//   ① 代挂 Key 没有号池分组 → 400（而不是悄悄写进一个无效值）；
//   ② 没授权给你的组 → 403（不能因为"组存在"就让你切）；
//   ③ 授权了但组不存在 → 404；组存在但没有模型 → 409（切过去等于全 404）；
//   ④ 切换后**可用模型与 /v1/models 立刻跟着变**，而余额与历史流水一分不动。
// 前三条错一个都会把用户引到错误方向（以为组被删了 / 以为切成功了）。
// ======================================================================

type groupFixture struct {
	store *Store
	px    *Proxy
	a     *app
	user  *User
	key   *APIKey
	chID  int64
}

func newGroupFixture(t *testing.T) *groupFixture {
	t.Helper()
	store, px, a := newTestApp(t, &Account{
		UID: "kg-pool", Realm: "cn", AccessToken: "at", RefreshToken: "rt",
		ConsumeGroup: "apply", PoolState: "in",
	})
	t.Cleanup(func() { store.Close() })
	withModelsCache(t, []string{"glm-5.3"})

	u, err := store.CreateUser("kg-user", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	k, err := store.CreateAPIKey(u.ID, "kg", "hash-kg", "", "sk-wb-kg", "", "", "")
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	id, err := store.SaveUpstream(&Upstream{
		Name: "kg-relay", Kind: upstreamKindOpenAI,
		BaseURL: "https://kg-relay.example.com/v1", APIKey: "sk-kg",
		Enabled: true, Priority: 100,
	})
	if err != nil {
		t.Fatalf("SaveUpstream: %v", err)
	}
	return &groupFixture{store: store, px: px, a: a, user: u, key: k, chID: id}
}

func (f *groupFixture) setGroup(t *testing.T, keyID int64, group string) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"group": group})
	req := httptest.NewRequest(http.MethodPost, "/api/keys/1/group", strings.NewReader(string(body)))
	req.SetPathValue("id", itoa(int(keyID)))
	req = withUser(req, f.user)
	rec := httptest.NewRecorder()
	f.a.handleKeyGroupSet(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func (f *groupFixture) reloadKey(t *testing.T) *APIKey {
	t.Helper()
	k, err := f.store.GetAPIKey(f.user.ID, f.key.ID)
	if err != nil || k == nil {
		t.Fatalf("GetAPIKey: %v", err)
	}
	return k
}

// TestKeyGroupSetValidation 四条拒绝路径的顺序与错误码。
func TestKeyGroupSetValidation(t *testing.T) {
	f := newGroupFixture(t)
	chGroup := "ch" + itoa(int(f.chID))

	// ① 代挂 Key：没有号池分组这回事。
	hostKey, err := f.store.CreateAPIKey(f.user.ID, "h", "hash-h", "", "sk-wb-h", KeyKindHost, "", "")
	if err != nil {
		t.Fatalf("CreateAPIKey(host): %v", err)
	}
	code, out := f.setGroup(t, hostKey.ID, "work")
	if code != http.StatusBadRequest || out["code"] != "host_key_has_no_group" {
		t.Errorf("代挂 Key 应 400 + host_key_has_no_group，得到 %d %v", code, out)
	}

	// ② **白名单模式**下不在名单里的组：403（不是 404 —— 不能让人以为组被删了）。
	// v0.31.90 新语义：scope 为空 = 默认开放，非空 = 只有这些。
	if err := f.store.SetAPIKeyGroupsByID(f.key.ID, "ch999"); err != nil {
		t.Fatalf("设白名单: %v", err)
	}
	code, out = f.setGroup(t, f.key.ID, chGroup)
	if code != http.StatusForbidden || out["code"] != "group_not_granted" {
		t.Errorf("白名单外的组应 403 + group_not_granted，得到 %d %v", code, out)
	}

	// ③ 白名单里有但组里没有模型：409（切过去每个请求都会失败）。
	if err := f.store.SetAPIKeyGroupsByID(f.key.ID, chGroup); err != nil {
		t.Fatalf("授权: %v", err)
	}
	code, out = f.setGroup(t, f.key.ID, chGroup)
	if code != http.StatusConflict || out["code"] != "group_empty" {
		t.Errorf("空组应 409 + group_empty，得到 %d %v", code, out)
	}

	// ④ 白名单里有一个不存在的组：404。
	if err := f.store.SetAPIKeyGroupsByID(f.key.ID, "ch999"); err != nil {
		t.Fatalf("授权 ch999: %v", err)
	}
	code, out = f.setGroup(t, f.key.ID, "ch999")
	if code != http.StatusNotFound || out["code"] != "group_not_found" {
		t.Errorf("不存在的组应 404 + group_not_found，得到 %d %v", code, out)
	}
}

func TestKeyGroupSwitchChangesModels(t *testing.T) {
	f := newGroupFixture(t)
	chGroup := "ch" + itoa(int(f.chID))
	if err := f.store.SetAPIKeyGroupsByID(f.key.ID, chGroup); err != nil {
		t.Fatalf("授权: %v", err)
	}
	// 给通道组一个模型。
	if _, err := f.store.SaveUpstreamModel(&UpstreamModel{
		UpstreamID: f.chID, Model: "gpt-host", RemoteModel: "gpt-host", CreditPer1k: 3, Enabled: true,
	}); err != nil {
		t.Fatalf("SaveUpstreamModel: %v", err)
	}

	// ① 切到通道组：目录只剩该组模型（号池的 glm-5.3 不再可见）。
	code, out := f.setGroup(t, f.key.ID, chGroup)
	if code != http.StatusOK {
		t.Fatalf("切到通道组应 200，得到 %d %v", code, out)
	}
	if got, _ := out["models"].(float64); got != 1 {
		t.Errorf("该组模型数 = %v，期望 1", out["models"])
	}
	k := f.reloadKey(t)
	if k.ActiveGroup != chGroup {
		t.Fatalf("active_group = %q，期望 %q", k.ActiveGroup, chGroup)
	}
	got := f.a.keyModelIDs(k)
	if len(got) != 1 || got[0] != "gpt-host" {
		t.Errorf("生效组内可用模型 = %v，期望 [gpt-host]（号池模型应被排除）", got)
	}
	// 组外模型必须被拦（ServeChat 用的就是这个判据）。
	if ms := f.px.activeGroupModels(k); ms == nil || modelInGroup(ms, "glm-5.3") {
		t.Errorf("active_group 生效时 glm-5.3 不该可用：%v", ms)
	}

	// ② 切到号池官组：只剩号池模型。
	code, out = f.setGroup(t, f.key.ID, "work")
	if code != http.StatusOK {
		t.Fatalf("切到 work 应 200，得到 %d %v", code, out)
	}
	k = f.reloadKey(t)
	if k.ActiveGroup != "work" {
		t.Fatalf("active_group = %q，期望 work", k.ActiveGroup)
	}
	if got := f.a.keyModelIDs(k); modelInGroup(got, "gpt-host") {
		t.Errorf("work 组不该包含通道模型 gpt-host：%v", got)
	} else if len(got) == 0 {
		t.Error("work 组不该是空的（号池目录里应有模型）")
	}

	// ③ 切回「不限制」：恢复老行为（号池 ∪ 通道）。
	code, _ = f.setGroup(t, f.key.ID, "")
	if code != http.StatusOK {
		t.Fatalf("切回不限制应 200，得到 %d", code)
	}
	k = f.reloadKey(t)
	if k.ActiveGroup != "" {
		t.Fatalf("active_group = %q，期望空", k.ActiveGroup)
	}
	got3 := f.a.keyModelIDs(k)
	if !modelInGroup(got3, "gpt-host") || len(got3) < 2 {
		t.Errorf("不限制时应同时包含号池与通道模型，得到 %v", got3)
	}
}

// TestKeyGroupSwitchKeepsMoney 切换不动余额、不动历史流水。
//
// 这条是需求里"说明对已有用量与余额计算的影响"的机器化版本：
// 不是靠文案承诺，而是切换前后余额与 usage_logs 行数一字不差。
func TestKeyGroupSwitchKeepsMoney(t *testing.T) {
	f := newGroupFixture(t)
	before, err := f.store.GetUserByID(f.user.ID)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	var logsBefore int
	if err := f.store.db.QueryRow(`SELECT COUNT(*) FROM usage_logs WHERE user_id = ?`, f.user.ID).
		Scan(&logsBefore); err != nil {
		t.Fatalf("统计流水: %v", err)
	}

	if code, out := f.setGroup(t, f.key.ID, "work"); code != http.StatusOK {
		t.Fatalf("切换失败：%d %v", code, out)
	} else if out["history_unchanged"] != true {
		t.Errorf("响应必须显式声明 history_unchanged，得到 %v", out["history_unchanged"])
	}

	after, err := f.store.GetUserByID(f.user.ID)
	if err != nil {
		t.Fatalf("GetUser(after): %v", err)
	}
	if after.BalanceMicro != before.BalanceMicro {
		t.Errorf("余额被切换改变了：%d → %d", before.BalanceMicro, after.BalanceMicro)
	}
	var logsAfter int
	if err := f.store.db.QueryRow(`SELECT COUNT(*) FROM usage_logs WHERE user_id = ?`, f.user.ID).
		Scan(&logsAfter); err != nil {
		t.Fatalf("统计流水(after): %v", err)
	}
	if logsAfter != logsBefore {
		t.Errorf("切换不该产生流水：%d → %d", logsBefore, logsAfter)
	}
}

// TestKeyGroupOptionsListsChoices 选项列表：不限制恒在，未授权的组不出现。
func TestKeyGroupOptionsListsChoices(t *testing.T) {
	f := newGroupFixture(t)
	chGroup := "ch" + itoa(int(f.chID))
	call := func() keyGroupResp {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/keys/1/group-options", nil)
		req.SetPathValue("id", itoa(int(f.key.ID)))
		req = withUser(req, f.user)
		rec := httptest.NewRecorder()
		f.a.handleKeyGroupOptions(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("应 200，得到 %d", rec.Code)
		}
		var out keyGroupResp
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("解析: %v", err)
		}
		return out
	}

	// ① 默认（scope 空 = 不限制）：不限制 + work + **通道组也在**（v0.31.90）。
	// 这就是用户反馈的那条："控制台只有号池组，没有通道自建的组"。
	out := call()
	if !out.Editable {
		t.Error("号池 Key 应允许切换")
	}
	if len(out.Options) < 3 {
		t.Fatalf("默认应列出 不限制 + work + 通道组，得到 %+v", out.Options)
	}
	if out.Options[0].ID != "" || out.Options[1].ID != groupWorkID {
		t.Errorf("前两项应是不限制与 work，得到 %+v", out.Options)
	}
	found := false
	for _, o := range out.Options {
		if o.ID == chGroup {
			found = true
		}
	}
	if !found {
		t.Errorf("默认就该能选通道组（不必先授权），得到 %+v", out.Options)
	}
	if out.GlobalRatio <= 0 {
		t.Errorf("global_ratio 应给出具体值（前端要显示「跟随全局 ×N」），得到 %v", out.GlobalRatio)
	}

	// ② 白名单模式：只列 work + 名单里的那个，其它通道组被排除。
	if err := f.store.SetAPIKeyGroupsByID(f.key.ID, chGroup); err != nil {
		t.Fatalf("授权: %v", err)
	}
	out = call()
	for _, o := range out.Options {
		if o.ID != "" && o.ID != groupWorkID && o.ID != chGroup {
			t.Errorf("白名单模式下不该出现 %q", o.ID)
		}
	}
}

func TestKeyGroupOptionsHostNotEditable(t *testing.T) {
	f := newGroupFixture(t)
	hostKey, err := f.store.CreateAPIKey(f.user.ID, "h2", "hash-h2", "", "sk-wb-h2", KeyKindHost, "", "")
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/keys/1/group-options", nil)
	req.SetPathValue("id", itoa(int(hostKey.ID)))
	req = withUser(req, f.user)
	rec := httptest.NewRecorder()
	f.a.handleKeyGroupOptions(rec, req)
	var out keyGroupResp
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Editable {
		t.Error("代挂 Key 不该允许切换分组")
	}
	if !strings.Contains(out.Note, "代挂") {
		t.Errorf("应说明为什么不能切：%q", out.Note)
	}
}

// TestKeyGroupRevokedImmediatelyInactive 撤销授权后，active_group 必须立刻失效。
//
// 这是"管理员撤销"能不能算数的唯一判据：groupMembersFor 只看组存不存在，
// 不看有没有授权；少了授权校验，界面显示「已失效」而请求仍按该组走。
func TestKeyGroupRevokedImmediatelyInactive(t *testing.T) {
	f := newGroupFixture(t)
	chGroup := "ch" + itoa(int(f.chID))
	if _, err := f.store.SaveUpstreamModel(&UpstreamModel{
		UpstreamID: f.chID, Model: "gpt-rev", RemoteModel: "gpt-rev", CreditPer1k: 1, Enabled: true,
	}); err != nil {
		t.Fatalf("SaveUpstreamModel: %v", err)
	}
	if err := f.store.SetAPIKeyGroupsByID(f.key.ID, chGroup); err != nil {
		t.Fatalf("授权: %v", err)
	}
	if code, out := f.setGroup(t, f.key.ID, chGroup); code != http.StatusOK {
		t.Fatalf("切到通道组应 200，得到 %d %v", code, out)
	}
	k := f.reloadKey(t)
	if ms := f.px.activeGroupModels(k); ms == nil {
		t.Fatal("在白名单里时该组应生效")
	}

	// 管理员把它**移出白名单**（scope 换成别的组）：active_group 仍是 chN（用户的选择），
	// 但必须**立刻**不再生效 —— 否则"撤销"等于没生效。
	if err := f.store.SetAPIKeyGroupsByID(f.key.ID, "ch999"); err != nil {
		t.Fatalf("移出白名单: %v", err)
	}
	k = f.reloadKey(t)
	if k.ActiveGroup != chGroup {
		t.Fatalf("改白名单不该改动 active_group：%q", k.ActiveGroup)
	}
	if ms := f.px.activeGroupModels(k); ms != nil {
		t.Errorf("移出白名单后该组必须立刻失效（按不限制处理），得到 %v", ms)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/keys/1/group-options", nil)
	req.SetPathValue("id", itoa(int(f.key.ID)))
	req = withUser(req, f.user)
	rec := httptest.NewRecorder()
	f.a.handleKeyGroupOptions(rec, req)
	var out keyGroupResp
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	for _, o := range out.Options {
		if o.ID == chGroup {
			t.Error("移出白名单后该组不该再出现在可选项里")
		}
	}
	if !strings.Contains(out.ActiveLabel, "已失效") {
		t.Errorf("当前生效组失效时必须显式提示，得到 %q", out.ActiveLabel)
	}
}

// TestKeyGroupChannelGroupDefaultOpen 通道组默认可选，不需要先授权（v0.31.90）。
//
// 这是用户反馈的核心："用户控制台密钥以及接入只有号池组，没有通道自建的组"。
// 通道组与 work 一样是平台供给，默认就该能选；要限制时管理员才把它变成白名单。
func TestKeyGroupChannelGroupDefaultOpen(t *testing.T) {
	f := newGroupFixture(t)
	chGroup := "ch" + itoa(int(f.chID))
	if _, err := f.store.SaveUpstreamModel(&UpstreamModel{
		UpstreamID: f.chID, Model: "gpt-open", RemoteModel: "gpt-open", CreditPer1k: 2, Enabled: true,
	}); err != nil {
		t.Fatalf("SaveUpstreamModel: %v", err)
	}
	// 没有调用任何 SetAPIKeyGroupsByID —— 直接切。
	if code, out := f.setGroup(t, f.key.ID, chGroup); code != http.StatusOK {
		t.Fatalf("未授权也应能切到通道组（默认开放），得到 %d %v", code, out)
	}
	k := f.reloadKey(t)
	if k.ActiveGroup != chGroup {
		t.Fatalf("active_group = %q，期望 %q", k.ActiveGroup, chGroup)
	}
	if got := f.a.keyModelIDs(k); len(got) != 1 || got[0] != "gpt-open" {
		t.Errorf("切到通道组后可用模型 = %v，期望 [gpt-open]", got)
	}
	if !f.a.groupSelectable(k, chGroup) {
		t.Error("默认开放时通道组应可选")
	}
}

// TestKeyGroupWhitelistExcludesChannelGroup 白名单模式下，未列出的通道组不可选。
func TestKeyGroupWhitelistExcludesChannelGroup(t *testing.T) {
	f := newGroupFixture(t)
	chGroup := "ch" + itoa(int(f.chID))
	if err := f.store.SetAPIKeyGroupsByID(f.key.ID, "ch999"); err != nil {
		t.Fatalf("设白名单: %v", err)
	}
	k := f.reloadKey(t)
	if f.a.groupSelectable(k, chGroup) {
		t.Error("白名单非空时，未列出的通道组不该可选")
	}
	if !f.a.groupSelectable(k, groupWorkID) {
		t.Error("work 恒可选")
	}
}
