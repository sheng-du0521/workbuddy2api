// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import "testing"

// ======================================================================
// 「通道影响面」的回归网（v0.31.78）
//
// 这条统计的数字会直接决定运维"敢不敢停这条通道"，所以最怕的是**少算**：
// 用 `LIKE '%ch3%'` 会把 ch13 / ch30 一起算进来（多算，尚可忍受），
// 但用 `LIKE '%ch3,%'` 漏掉末尾那个（少算，会让人以为只影响 3 把）。
// 这里用两端补逗号的写法，两个方向的边界都要钉住。
// ======================================================================

func TestCountKeysByGroupExact(t *testing.T) {
	store, _, _, _ := newAuthzApp(t)
	defer store.Close()

	owner, err := store.CreateUser("ck-owner", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	mk := func(name, scope string) {
		t.Helper()
		if _, err := store.CreateAPIKey(owner.ID, name, "h-"+name, "", "sk-"+name, "", "", ""); err != nil {
			t.Fatalf("CreateAPIKey(%s): %v", name, err)
		}
		keys, err := store.ListAPIKeys(owner.ID)
		if err != nil || len(keys) == 0 {
			t.Fatalf("ListAPIKeys: %v", err)
		}
		var id int64
		for _, k := range keys {
			if k.Name == name {
				id = k.ID
			}
		}
		if err := store.SetAPIKeyGroupsByID(id, scope); err != nil {
			t.Fatalf("SetAPIKeyGroupsByID(%s): %v", name, err)
		}
	}

	mk("k-ch3", "ch3")
	mk("k-ch3-ch13", "ch3,ch13") // 同时含 ch3 与 ch13
	mk("k-ch13", "ch13")         // 不能被 ch3 的统计算进去
	mk("k-ch30", "ch30")         // 同上
	mk("k-none", "")

	cases := map[string]int{"ch3": 2, "ch13": 2, "ch30": 1, "ch1": 0}
	for gid, want := range cases {
		got, err := store.CountKeysByGroup(gid)
		if err != nil {
			t.Fatalf("CountKeysByGroup(%s): %v", gid, err)
		}
		if got != want {
			t.Errorf("CountKeysByGroup(%s) = %d，期望 %d（前缀相似不能被算成同一个组）", gid, got, want)
		}
	}
}
