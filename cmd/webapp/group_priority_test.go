// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"testing"
)

// TestPlatformGroupPriorityWeighted 验证加权随机模式（默认模式）下
// 「用户申请」组优先、「反代」组托底的分组优先级。
//
// 真机实测曾出现：apply 组有 2 个健康账号可用，8 次请求仍有 3 次落到 proxy 组，
// 且全部 status=ok（不是失败回退）——即 proxy 被当作首选。本用例把该场景固化。
func TestPlatformGroupPriorityWeighted(t *testing.T) {
	store, px, _ := newTestApp(t,
		&Account{UID: "p1", Realm: "cn", AccessToken: "a"},
		&Account{UID: "p2", Realm: "cn", AccessToken: "a"},
		&Account{UID: "p3", Realm: "cn", AccessToken: "a"},
		&Account{UID: "p4", Realm: "cn", AccessToken: "a"},
		&Account{UID: "p5", Realm: "cn", AccessToken: "a"},
		&Account{UID: "a1", Realm: "cn", AccessToken: "a", OwnerUserID: 10, PoolState: "in"},
		&Account{UID: "a2", Realm: "cn", AccessToken: "a", OwnerUserID: 10, PoolState: "in"},
	)
	defer store.Close()

	if err := store.SetKV(kvPickMode, "weighted"); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	px.ReloadSelection()
	if err := px.SyncAccounts(); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}
	if got := px.pool.PickMode(); got != "weighted" {
		t.Fatalf("模式应为 weighted，得 %s", got)
	}

	const rounds = 40
	byGroup := map[string]int{}
	for i := 0; i < rounds; i++ {
		au := px.pickExcluding(map[string]bool{}, "m", pickScope{})
		if au == nil {
			t.Fatalf("第 %d 次选号返回 nil", i)
		}
		byGroup[px.ConsumeGroupForUID(au.UID)]++
	}
	t.Logf("分布 = %v", byGroup)

	if byGroup["proxy"] != 0 {
		t.Fatalf("apply 组有可用账号时不得选中 proxy（分组优先级被破坏），得 %v", byGroup)
	}
	if byGroup["apply"] != rounds {
		t.Fatalf("应全部命中 apply，得 %v", byGroup)
	}
}
