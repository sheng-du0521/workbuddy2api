// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"testing"
	"time"
)

// ======================================================================
// 模型组的回归网（v0.31.76）
//
// 守四件事：
//  1. **只有组名被展开**：普通模型名必须 ok=false（否则任何模型都会被改成别人的名字）；
//  2. **组不存在就不展开**：客户端填错组名时按"未知模型"处理，比自造 503 更有指向；
//  3. **通道组按通道的 enabled 映射展开**：停用的映射不能出现在组里；
//  4. **代挂自用 Key 只能看见 work**：它只用本人账号，接通道组等于用平台的钱给个人用。
// ======================================================================

// withModelsCache 临时改写模型缓存（测试用），结束时恢复。
func withModelsCache(t *testing.T, ids []string) {
	t.Helper()
	modelsCache.mu.Lock()
	modelsCache.ids = append([]string(nil), ids...)
	modelsCache.at = time.Now()
	modelsCache.mu.Unlock()
	t.Cleanup(func() { invalidateModelsCache() })
}

// TestApplyGroupExpansionRewritesPayload 展开后**必须**同步改写 payload。
//
// 出站请求体是从 payload 重新编码的：只改局部变量，上游照样收到 `wb-group-work`
// → 11102 模型不存在。这条断言就是钉住这行赋值，防止哪天被"整理"掉。
func TestApplyGroupExpansionRewritesPayload(t *testing.T) {
	store, px, _ := newTestApp(t, &Account{
		UID: "g-acct5", Realm: "cn", AccessToken: "at", RefreshToken: "rt",
		ConsumeGroup: "apply", PoolState: "in",
	})
	defer store.Close()
	withModelsCache(t, []string{"glm-5.3"})

	payload := map[string]any{"model": "wb-group-work", "messages": []any{}}
	real, ok := px.applyGroupExpansion(payload, "wb-group-work")
	if !ok {
		t.Fatal("wb-group-work 应被展开")
	}
	if real == "wb-group-work" {
		t.Fatalf("展开结果不应还是组名：%q", real)
	}
	if payload["model"] != real {
		t.Errorf("payload[model] = %v，期望被改写成 %q（否则上游收到的是组名）", payload["model"], real)
	}

	// 普通模型名：payload 一个字节都不该动。
	p2 := map[string]any{"model": "glm-5.3"}
	if _, ok := px.applyGroupExpansion(p2, "glm-5.3"); ok {
		t.Error("普通模型名不该被展开")
	}
	if p2["model"] != "glm-5.3" {
		t.Errorf("普通模型名的 payload 被改成了 %v", p2["model"])
	}
}

// TestExpandGroupModelOnlyGroupNames 普通模型名一律不展开。
func TestExpandGroupModelOnlyGroupNames(t *testing.T) {
	store, px, _ := newTestApp(t, &Account{
		UID: "g-acct", Realm: "cn", AccessToken: "at", RefreshToken: "rt",
		ConsumeGroup: "apply", PoolState: "in",
	})
	defer store.Close()
	withModelsCache(t, []string{"glm-5.3"})

	for _, name := range []string{"glm-5.3", "wb-group-", "wb-groub-work", "claude-sonnet-4-5"} {
		if _, _, ok := px.expandGroupModel(name); ok {
			t.Errorf("%q 不该被当成模型组展开", name)
		}
	}
}

// TestExpandGroupModelWork work 官组：展开成号池目录里的一个成员。
func TestExpandGroupModelWork(t *testing.T) {
	store, px, _ := newTestApp(t, &Account{
		UID: "g-acct2", Realm: "cn", AccessToken: "at", RefreshToken: "rt",
		ConsumeGroup: "apply", PoolState: "in",
	})
	defer store.Close()
	withModelsCache(t, []string{"glm-5.3", "deepseek-v4-flash"})

	real, label, ok := px.expandGroupModel("wb-group-work")
	if !ok {
		t.Fatal("wb-group-work 应被识别为组名")
	}
	if label != "work 官组" {
		t.Errorf("label = %q，期望 work 官组", label)
	}
	if real != "glm-5.3" && real != "deepseek-v4-flash" {
		t.Errorf("展开结果 %q 不在号池目录里", real)
	}
}

// TestExpandGroupModelChannel 通道组：成员来自该通道 enabled 的映射。
func TestExpandGroupModelChannel(t *testing.T) {
	store, px, _ := newTestApp(t, &Account{
		UID: "g-acct3", Realm: "cn", AccessToken: "at", RefreshToken: "rt",
		ConsumeGroup: "apply", PoolState: "in",
	})
	defer store.Close()
	withModelsCache(t, nil)

	id, err := store.SaveUpstream(&Upstream{
		Name: "sub2-test", Kind: upstreamKindOpenAI,
		BaseURL: "https://relay.example.com", APIKey: "sk-x", Enabled: true, Priority: 100,
	})
	if err != nil {
		t.Fatalf("SaveUpstream: %v", err)
	}
	// 两条映射，其中一条停用 —— 停用的不能出现在组里。
	for _, m := range []struct {
		model   string
		enabled bool
	}{{"gpt-on", true}, {"gpt-off", false}} {
		if _, err := store.SaveUpstreamModel(&UpstreamModel{
			UpstreamID: id, Model: m.model, RemoteModel: m.model,
			CreditPer1k: 1, Enabled: m.enabled,
		}); err != nil {
			t.Fatalf("SaveUpstreamModel(%s): %v", m.model, err)
		}
	}

	gid := "ch" + itoa(int(id))
	members, label, ok := px.groupMembers(gid)
	if !ok {
		t.Fatalf("组 %s 应存在", gid)
	}
	if label != "通道 · sub2-test" {
		t.Errorf("label = %q，期望「通道 · sub2-test」", label)
	}
	if len(members) != 1 || members[0] != "gpt-on" {
		t.Errorf("成员 = %v，期望只有 gpt-on（停用的映射不进组）", members)
	}

	// 停用整条通道 → 组立刻不可见。
	if _, err := store.db.Exec(`UPDATE upstreams SET enabled=0 WHERE id=?`, id); err != nil {
		t.Fatalf("停用通道: %v", err)
	}
	if _, _, ok := px.groupMembers(gid); ok {
		t.Error("通道停用后组应不可见")
	}
}

// TestVisibleGroupsHostKeyOnlyWork 代挂自用 Key 只看得见 work，即使绑了通道组。
func TestVisibleGroupsHostKeyOnlyWork(t *testing.T) {
	store, px, _ := newTestApp(t, &Account{
		UID: "g-acct4", Realm: "cn", AccessToken: "at", RefreshToken: "rt",
		ConsumeGroup: "apply", PoolState: "in",
	})
	defer store.Close()
	withModelsCache(t, []string{"glm-5.3"})

	id, err := store.SaveUpstream(&Upstream{
		Name: "sub2-host", Kind: upstreamKindOpenAI,
		BaseURL: "https://relay2.example.com", APIKey: "sk-y", Enabled: true, Priority: 100,
	})
	if err != nil {
		t.Fatalf("SaveUpstream: %v", err)
	}
	if _, err := store.SaveUpstreamModel(&UpstreamModel{
		UpstreamID: id, Model: "gpt-host", RemoteModel: "gpt-host", CreditPer1k: 1, Enabled: true,
	}); err != nil {
		t.Fatalf("SaveUpstreamModel: %v", err)
	}

	host := &APIKey{Kind: KeyKindHost, GroupScope: "ch" + itoa(int(id))}
	groups := px.visibleGroups(host)
	if len(groups) != 1 || groups[0].ID != groupWorkID {
		t.Fatalf("代挂 Key 应只看见 work，得到 %+v", groups)
	}

	// 池计费 Key 绑了组 → 两组都在。
	pool := &APIKey{GroupScope: "ch" + itoa(int(id))}
	groups = px.visibleGroups(pool)
	if len(groups) != 2 {
		t.Fatalf("池计费 Key 应看见 work + 通道组，得到 %+v", groups)
	}
	if groups[1].Model != "wb-group-ch"+itoa(int(id)) {
		t.Errorf("虚拟模型名 = %q，期望 wb-group-ch<id>", groups[1].Model)
	}
}
