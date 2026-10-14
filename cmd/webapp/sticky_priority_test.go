// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"testing"
	"time"

	"workbuddy2api/internal/pool"
)

// TestStickyPriorityGuard 锁住实测缺陷：粘性会话分配账号时会绕过分组优先级。
//
// session.Router 未命中时按 key 哈希从「池内可用集」直接分配账号，而该可用集
// **不含消耗分组过滤** —— 于是新会话可能被绑到「反代」组账号，handleChat 直接复用
// 该绑定、不经过 pickExcluding，导致「用户申请优先」被绕过。
// 实测现象：apply 组有 2 个可用账号时，8 次请求仍有 3 次落到 proxy 组（全部 ok）。
//
// 守卫语义：平台范围下绑定号必须属于「当前最高优先级的可用分组」。
func TestStickyPriorityGuard(t *testing.T) {
	store, px, _ := newTestApp(t,
		&Account{UID: "p1", Realm: "cn", AccessToken: "a"},
		&Account{UID: "p2", Realm: "cn", AccessToken: "a"},
		&Account{UID: "a1", Realm: "cn", AccessToken: "a", OwnerUserID: 10, PoolState: "in"},
		&Account{UID: "a2", Realm: "cn", AccessToken: "a", OwnerUserID: 10, PoolState: "in"},
	)
	defer store.Close()
	if err := px.SyncAccounts(); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}

	plat := pickScope{}
	if got := px.pickPriorityGroupForModel("m", plat); got != "apply" {
		t.Fatalf("apply 组有可用账号时最高优先级分组应为 apply，得 %q", got)
	}
	if px.stickyPriorityOK("p1", "m", plat) {
		t.Error("apply 可用时，粘性绑定到 proxy 账号必须被拒（否则绕过优先级）")
	}
	if !px.stickyPriorityOK("a1", "m", plat) {
		t.Error("粘性绑定到 apply 账号应放行")
	}

	// apply 组全部冷却 → 优先级降到 proxy，此时 proxy 绑定应放行（托底语义）。
	px.pool.Cooldown("a1", pool.CoolHard, time.Hour, "test")
	px.pool.Cooldown("a2", pool.CoolHard, time.Hour, "test")
	if got := px.pickPriorityGroupForModel("m", plat); got != "proxy" {
		t.Fatalf("apply 不可用时最高优先级应为 proxy，得 %q", got)
	}
	if !px.stickyPriorityOK("p1", "m", plat) {
		t.Error("apply 不可用时，proxy 绑定应作为托底放行")
	}

	// 代挂自用范围不受分组优先级管辖（否则号主自己的粘性会被打散）。
	self := pickScope{HostUser: 10, AccountUIDs: map[string]bool{"a1": true}}
	px.pool.Cooldown("a1", pool.CoolSoft, time.Hour, "reset")
	if !px.stickyPriorityOK("p1", "m", self) {
		t.Error("代挂自用范围不应受消耗分组优先级限制")
	}
}
