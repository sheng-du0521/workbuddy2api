// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"encoding/json"
	"testing"
	"time"
)

// ======================================================================
// 「用户面能选哪些组」必须只有一个口径（v0.31.91）
//
// 用户反馈：「模型选用问题虽然已经处理，但生成密钥后依然无法选用通道组」。
//
// 根因是**两套口径分裂**：
//   - 切换分组弹窗用 selectableGroupIDs（含 ch<N>）→ 能切过去；
//   - 模型清单（keymodels.go）与 /v1/models（modelsPayloadFor）用 visibleGroups
//     （只有 work / host）→ 下拉里没有 wb-group-ch<N> 这个名字可选。
//   再加上 handleModels 的定制判据还停在 `group_scope 非空`（v0.31.90 之后
//   管理员不再逐 Key 授权 → 恒为空），新建的 Key 直接拿全局缓存原文，
//   连 wb-group-work 都没有。
//
// 这组用例把三件事分别钉住：
//   ① userGroups = work + 该 Key 可切换的通道组（与 selectableGroupIDs 同源）；
//   ② /api/keys/{id}/models 的 models 里真的出现 wb-group-ch<N>；
//   ③ /v1/models 对新 Key（active_group 与 group_scope 都为空）也列出通道组名，
//      而"没有通道组"的部署仍保持原样（不定制）。
// ======================================================================

func (f *groupFixture) addChannelModel(t *testing.T, model string) {
	t.Helper()
	if _, err := f.store.SaveUpstreamModel(&UpstreamModel{
		UpstreamID: f.chID, Model: model, RemoteModel: model, CreditPer1k: 1, Enabled: true,
	}); err != nil {
		t.Fatalf("SaveUpstreamModel: %v", err)
	}
}

// setModelCatalogPayload 给 modelsPayloadFor 准备一份可解析的缓存体。
//
// 不能走 refreshModelsCache：那条慢路径会真的向上游 fetch（单测里必超时）。
func setModelCatalogPayload(ids ...string) {
	data := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		data = append(data, map[string]any{"id": id, "object": "model", "owned_by": "workbuddy"})
	}
	blob, _ := json.Marshal(map[string]any{"object": "list", "data": data})
	modelsCache.mu.Lock()
	modelsCache.payload = blob
	modelsCache.ids = append([]string(nil), ids...)
	modelsCache.at = time.Now()
	modelsCache.mu.Unlock()
}

// TestUserGroupsCoversChannelGroups 用户可见组 = work + 可切换的通道组。
func TestUserGroupsCoversChannelGroups(t *testing.T) {
	f := newGroupFixture(t)
	f.addChannelModel(t, "ch-only-model")
	ch := "ch" + itoa(int(f.chID))

	var hasWork, hasCh bool
	for _, g := range f.px.userGroups(f.key) {
		switch g.ID {
		case groupWorkID:
			hasWork = true
			if g.Kind != "work" {
				t.Errorf("work 组 kind=%q，期望 work", g.Kind)
			}
		case ch:
			hasCh = true
			if g.Kind != "channel" {
				t.Errorf("通道组 kind=%q，期望 channel", g.Kind)
			}
			if g.Members != 1 {
				t.Errorf("通道组成员数 = %d，期望 1", g.Members)
			}
			if g.Model != groupVirtualModel(ch) {
				t.Errorf("通道组虚拟名 = %q，期望 %q", g.Model, groupVirtualModel(ch))
			}
			if g.Label == "" {
				t.Error("通道组缺 label（前端会显示空白项）")
			}
		}
	}
	if !hasWork {
		t.Error("userGroups 缺少号池官组")
	}
	if !hasCh {
		t.Errorf("userGroups 缺少通道组 %s：用户能切过去却选不到它", ch)
	}
}

// TestUserGroupsHostKeyOnlyHost 代挂 Key 仍然只有自挂官组（不能被通道组污染）。
func TestUserGroupsHostKeyOnlyHost(t *testing.T) {
	f := newGroupFixture(t)
	hostKey, err := f.store.CreateAPIKey(f.user.ID, "h", "hash-h2", "", "sk-wb-h2", KeyKindHost, "", "")
	if err != nil {
		t.Fatalf("CreateAPIKey(host): %v", err)
	}
	gs := f.px.userGroups(hostKey)
	if len(gs) != 1 || gs[0].ID != groupHostID {
		t.Fatalf("代挂 Key 的可见组 = %v，期望只有 %s", gs, groupHostID)
	}
}

// TestKeyModelsListsChannelGroupName 控制台模型清单里必须出现 wb-group-ch<N>。
func TestKeyModelsListsChannelGroupName(t *testing.T) {
	f := newGroupFixture(t)
	f.addChannelModel(t, "ch-only-model")
	ch := "ch" + itoa(int(f.chID))

	code, out := keyModelsCall(t, f.a, f.user, itoa(int(f.key.ID)))
	if code != 200 {
		t.Fatalf("code=%d", code)
	}
	var groupHit bool
	for _, g := range out.Groups {
		if g.ID == ch {
			groupHit = true
		}
	}
	if !groupHit {
		t.Fatalf("models 响应的 groups 里没有 %s（现有 %v）", ch, out.Groups)
	}
	if !hasModel(out.Models, groupVirtualModel(ch)) {
		t.Errorf("models 里没有组名 %q —— 下拉里就选不到它（现有 %v）",
			groupVirtualModel(ch), out.Models)
	}
	if !hasModel(out.Models, "ch-only-model") {
		t.Errorf("models 里没有通道成员模型（现有 %v）", out.Models)
	}
}

// TestModelsCatalogShowsChannelGroupForNewKey 新建 Key（两字段全空）的 /v1/models 也要有组名。
func TestModelsCatalogShowsChannelGroupForNewKey(t *testing.T) {
	f := newGroupFixture(t)
	f.addChannelModel(t, "ch-only-model")
	ch := "ch" + itoa(int(f.chID))
	setModelCatalogPayload("glm-5.3")

	k := f.reloadKey(t)
	if k.GroupScope != "" || k.ActiveGroup != "" {
		t.Fatalf("前提不成立：新 Key 应为 scope/active 全空，得到 %q/%q", k.GroupScope, k.ActiveGroup)
	}
	if !f.a.keyNeedsGroupCatalog(k) {
		t.Fatal("有通道组可选的池 Key 应走定制路径（否则客户端里根本看不到组名）")
	}
	blob := f.a.modelsPayloadFor(k)
	if blob == nil {
		t.Fatal("modelsPayloadFor 返回 nil")
	}
	var doc struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(blob, &doc); err != nil {
		t.Fatalf("不是合法 JSON：%v", err)
	}
	ids := map[string]bool{}
	for _, d := range doc.Data {
		if id, _ := d["id"].(string); id != "" {
			ids[id] = true
		}
	}
	if !ids[groupVirtualModel(ch)] {
		t.Errorf("目录里没有 %q（现有 %v）", groupVirtualModel(ch), doc.Data)
	}
	if !ids[groupVirtualModel(groupWorkID)] {
		t.Errorf("目录里没有 %q", groupVirtualModel(groupWorkID))
	}
}

// TestModelsCatalogUnchangedWithoutChannelGroup 没有通道组的部署保持原样（不定制）。
func TestModelsCatalogUnchangedWithoutChannelGroup(t *testing.T) {
	f := newGroupFixture(t)
	ups, err := f.store.ListUpstreams()
	if err != nil {
		t.Fatalf("ListUpstreams: %v", err)
	}
	for i := range ups {
		if ups[i].ID == f.chID {
			ups[i].Enabled = false
			if _, err := f.store.SaveUpstream(&ups[i]); err != nil {
				t.Fatalf("停用通道: %v", err)
			}
		}
	}
	k := f.reloadKey(t)
	if f.a.keyNeedsGroupCatalog(k) {
		t.Error("没有可选通道组时不该定制 /v1/models（存量响应应逐字节不变）")
	}
}

// TestOnboardPlanWritesActiveChannelGroup 切到通道组后，一键接入必须写这个组名。
func TestOnboardPlanWritesActiveChannelGroup(t *testing.T) {
	f := newGroupFixture(t)
	f.addChannelModel(t, "ch-only-model")
	ch := "ch" + itoa(int(f.chID))
	if _, err := f.store.SetAPIKeyActiveGroup(f.user.ID, f.key.ID, ch); err != nil {
		t.Fatalf("SetAPIKeyActiveGroup: %v", err)
	}
	k := f.reloadKey(t)
	plan := f.a.buildOnboardPlan(k)
	if plan.GroupName != groupVirtualModel(ch) {
		t.Errorf("接入包组名 = %q，期望 %q（否则切组对一键接入无效）",
			plan.GroupName, groupVirtualModel(ch))
	}
	if !hasModel(plan.Members, "ch-only-model") {
		t.Errorf("接入候选里没有通道成员模型（现有 %v）", plan.Members)
	}
	if plan.Restricted {
		t.Error("通道组不该套用后台的官组选配（Restricted 应为 false）")
	}
	if plan.Default == "" || !hasModel(plan.candidates(), plan.Default) {
		t.Errorf("默认值 %q 不在候选里（%v）", plan.Default, plan.candidates())
	}
}
