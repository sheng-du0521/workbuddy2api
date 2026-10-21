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
// 管理端「Key 组授权」的回归网（v0.31.77）
//
// 授权决定"这把 Key 能不能用某条第三方通道"，四个判断都不能错：
//  1. 代挂自用 Key 必须 400（它只走本人账号，静默改成 work 会让管理员以为授权成功了）；
//  2. 不存在的组要**丢弃并回报**，不能写进库（写了也不会生效，但界面会显示"已授权"）；
//  3. 返回的是**最终生效**的 scope，不是提交值；
//  4. 不存在的 Key 要 404（不能让 admin 接口变成"猜 id"）。
// ======================================================================

func mkGroupGrantReq(t *testing.T, a *app, keyID, scope string) (int, map[string]any) {
	t.Helper()
	body := strings.NewReader(`{"group_scope":"` + scope + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/admin/keys/"+keyID+"/groups", body)
	req.SetPathValue("id", keyID)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.handleAdminSetKeyGroups(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestAdminSetKeyGroups(t *testing.T) {
	store, a, _, _ := newAuthzApp(t)
	defer store.Close()

	owner, err := store.CreateUser("grp-owner", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	poolKey, err := store.CreateAPIKey(owner.ID, "pool-key", "h1", "", "sk-wb-p", "", "", "")
	if err != nil {
		t.Fatalf("CreateAPIKey(pool): %v", err)
	}
	hostKey, err := store.CreateAPIKey(owner.ID, "host-key", "h2", "", "sk-wb-h",
		KeyKindHost, "", "u1")
	if err != nil {
		t.Fatalf("CreateAPIKey(host): %v", err)
	}

	upID, err := store.SaveUpstream(&Upstream{
		Name: "grp-relay", Kind: upstreamKindOpenAI,
		BaseURL: "https://relay.example.com", APIKey: "sk-z", Enabled: true, Priority: 100,
	})
	if err != nil {
		t.Fatalf("SaveUpstream: %v", err)
	}
	if _, err := store.SaveUpstreamModel(&UpstreamModel{
		UpstreamID: upID, Model: "gpt-g", RemoteModel: "gpt-g", CreditPer1k: 1, Enabled: true,
	}); err != nil {
		t.Fatalf("SaveUpstreamModel: %v", err)
	}
	chID := "ch" + itoa(int(upID))

	// ① 池 Key 授权存在的组 → 200，库里生效，返回的 scope 与提交一致。
	code, out := mkGroupGrantReq(t, a, itoa(int(poolKey.ID)), chID)
	if code != http.StatusOK {
		t.Fatalf("授权应 200，得到 %d（%v）", code, out)
	}
	if out["group_scope"] != chID {
		t.Errorf("返回 scope = %v，期望 %s", out["group_scope"], chID)
	}
	if k, err := store.GetAPIKey(owner.ID, poolKey.ID); err != nil || k.GroupScope != chID {
		t.Errorf("库里 group_scope = %q（err=%v），期望 %s", k.GroupScope, err, chID)
	}

	// ② 混入不存在的组 → 丢弃并回报，库里只留有效的那一个。
	code, out = mkGroupGrantReq(t, a, itoa(int(poolKey.ID)), chID+",ch999")
	if code != http.StatusOK {
		t.Fatalf("含无效组也应 200（丢弃而不是报错），得到 %d", code)
	}
	if out["group_scope"] != chID {
		t.Errorf("返回 scope = %v，期望只留 %s", out["group_scope"], chID)
	}
	dropped, _ := out["dropped"].([]any)
	if len(dropped) != 1 || dropped[0] != "ch999" {
		t.Errorf("dropped = %v，期望 [ch999]", out["dropped"])
	}

	// ③ 代挂 Key → 400（不能静默改成 work）。
	code, out = mkGroupGrantReq(t, a, itoa(int(hostKey.ID)), chID)
	if code != http.StatusBadRequest {
		t.Fatalf("代挂 Key 授权通道组应 400，得到 %d（%v）", code, out)
	}
	if k, _ := store.GetAPIKey(owner.ID, hostKey.ID); k.GroupScope != "" {
		t.Errorf("代挂 Key 的 group_scope 不该被改动，得到 %q", k.GroupScope)
	}

	// ④ 不存在的 Key → 404。
	if code, _ := mkGroupGrantReq(t, a, "99999", chID); code != http.StatusNotFound {
		t.Errorf("不存在的 Key 应 404，得到 %d", code)
	}

	// ⑤ work 不写进 scope（它恒可见，写进去会让数据看起来像"授予过"）。
	code, out = mkGroupGrantReq(t, a, itoa(int(poolKey.ID)), "work,"+chID)
	if code != http.StatusOK || out["group_scope"] != chID {
		t.Errorf("work 应被忽略：code=%d scope=%v", code, out["group_scope"])
	}
}

// TestAdminUserKeysListsAllGroups 授权界面拿到的组清单必须是**全部可选组**。
//
// 回归 v0.31.82 的遗留：那里把 visibleGroups 收窄成"只给 work"，
// 而授权界面用的正是它 —— 结果管理员永远只看到一个可勾项，以为通道组不能授权。
func TestAdminUserKeysListsAllGroups(t *testing.T) {
	store, _, a := newTestApp(t)
	defer store.Close()
	withModelsCache(t, []string{"glm-5.3"})

	u, err := store.CreateUser("ag-list", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := store.CreateAPIKey(u.ID, "k", "hash-ag", "", "sk-wb-ag", "", "", ""); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	id, err := store.SaveUpstream(&Upstream{
		Name: "ag-relay", Kind: upstreamKindOpenAI,
		BaseURL: "https://ag-relay.example.com/v1", APIKey: "sk-ag",
		Enabled: true, Priority: 100,
	})
	if err != nil {
		t.Fatalf("SaveUpstream: %v", err)
	}
	if _, err := store.SaveUpstreamModel(&UpstreamModel{
		UpstreamID: id, Model: "gpt-ag", RemoteModel: "gpt-ag", CreditPer1k: 1, Enabled: true,
	}); err != nil {
		t.Fatalf("SaveUpstreamModel: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/admin/users/1/keys", nil)
	req.SetPathValue("id", itoa(int(u.ID)))
	rec := httptest.NewRecorder()
	a.handleAdminUserKeys(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，得到 %d（%s）", rec.Code, rec.Body.String())
	}
	var out struct {
		Groups []modelGroupInfo `json:"groups"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析: %v", err)
	}
	have := map[string]modelGroupInfo{}
	for _, g := range out.Groups {
		have[g.ID] = g
	}
	if _, ok := have[groupWorkID]; !ok {
		t.Errorf("组清单必须包含 work（恒可用），得到 %+v", out.Groups)
	}
	ch := "ch" + itoa(int(id))
	g, ok := have[ch]
	if !ok {
		t.Fatalf("组清单必须包含 enabled 的通道组 %s（否则管理员无法授权），得到 %+v", ch, out.Groups)
	}
	if g.Members != 1 {
		t.Errorf("通道组成员数 = %d，期望 1", g.Members)
	}
}
