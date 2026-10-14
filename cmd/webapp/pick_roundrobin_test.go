// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"testing"
)

// TestPlatformPickRoundRobinAcrossGroups 端到端复现真机现象：5 个「反代」组账号
// + 2 个「用户申请」组账号，roundrobin 模式下连续选号，用户申请组内应严格轮流。
//
// 真机实测曾出现「连发 6 次全部落在同一个用户申请账号」，而分组优先级本身是正确的
// （apply 组优先）。本用例把该场景固化，锁住选号在「组过滤 + 候选集收缩」下仍均匀。
func TestPlatformPickRoundRobinAcrossGroups(t *testing.T) {
	store, px, _ := newTestApp(t,
		&Account{UID: "p1", Realm: "cn", AccessToken: "a"},
		&Account{UID: "p2", Realm: "cn", AccessToken: "a"},
		&Account{UID: "p3", Realm: "cn", AccessToken: "a"},
		&Account{UID: "p4", Realm: "cn", AccessToken: "a"},
		&Account{UID: "p5", Realm: "cn", AccessToken: "a"},
		// 代挂 + 已入池 = 用户申请组（优先消耗）。
		&Account{UID: "a1", Realm: "cn", AccessToken: "a", OwnerUserID: 10, PoolState: "in"},
		&Account{UID: "a2", Realm: "cn", AccessToken: "a", OwnerUserID: 10, PoolState: "in"},
	)
	defer store.Close()

	if err := store.SetKV(kvPickMode, "roundrobin"); err != nil {
		t.Fatalf("SetKV pick_mode: %v", err)
	}
	px.ReloadSelection()
	if err := px.SyncAccounts(); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}
	t.Logf("pool pick mode = %s", px.pool.PickMode())
	for _, uid := range []string{"p1", "a1", "a2"} {
		t.Logf("consume group %s -> %s", uid, px.ConsumeGroupForUID(uid))
	}

	const rounds = 10
	counts := map[string]int{}
	for i := 0; i < rounds; i++ {
		au := px.pickExcluding(map[string]bool{}, "m", pickScope{})
		if au == nil {
			t.Fatalf("第 %d 次选号返回 nil", i)
		}
		counts[au.UID]++
	}
	t.Logf("分布 = %v", counts)

	if counts["a1"] != rounds/2 || counts["a2"] != rounds/2 {
		t.Fatalf("用户申请组内应各 %d 次（优先组内严格轮流），得 %v", rounds/2, counts)
	}
	if counts["p1"]+counts["p2"]+counts["p3"]+counts["p4"]+counts["p5"] != 0 {
		t.Fatalf("反代组不应被选中（用户申请组非空时应优先），得 %v", counts)
	}
}
