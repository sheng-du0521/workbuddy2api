// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import "testing"

// TestPickSinkOffByDefault 5xx 降档**默认关闭**：不改任何行为。
//
// 这条默认值是经营决定而非技术决定 —— 本仓的「档」是**成本层**，
// 下沉 = 去用更贵的账号。程序不该替站长默认做这个选择。
func TestPickSinkOffByDefault(t *testing.T) {
	store, px, _ := newTestApp(t,
		&Account{UID: "s-free", AccessToken: "t"},
		&Account{UID: "s-paid", AccessToken: "t"})
	defer store.Close()
	defer px.Close()

	if px.pickSinkOn5xx() {
		t.Fatal("默认应为关闭")
	}
	px.pool.NoteModelCost("s-free", "m", 0, 1000)
	px.pool.NoteModelCost("s-paid", "m", 0.5, 1000)

	tried := map[string]bool{}
	if n := px.sinkCostTier("s-free", "m", tried); n != 0 {
		t.Fatalf("关闭时不该划掉任何账号，实得 %d", n)
	}
	if len(tried) != 0 {
		t.Fatalf("关闭时 tried 应保持为空，实得 %v", tried)
	}
}

// TestPickSinkMarksWholeTier 开启后把**整层**划掉（而不是只划失败的号）。
//
// 只划失败号是没用的 —— 熔断器已经在做那件事；R6 的语义是"这一档整体不可用"。
func TestPickSinkMarksWholeTier(t *testing.T) {
	store, px, _ := newTestApp(t,
		&Account{UID: "s-free1", AccessToken: "t"},
		&Account{UID: "s-free2", AccessToken: "t"},
		&Account{UID: "s-paid", AccessToken: "t"})
	defer store.Close()
	defer px.Close()
	if err := store.SetKV(kvPickSinkOn5xx, "on"); err != nil {
		t.Fatal(err)
	}
	px.pool.NoteModelCost("s-free1", "m", 0, 1000)
	px.pool.NoteModelCost("s-free2", "m", 0, 1000)
	px.pool.NoteModelCost("s-paid", "m", 0.5, 1000)

	tried := map[string]bool{"s-free1": true}
	n := px.sinkCostTier("s-free1", "m", tried)
	if n != 1 { // 只新增 s-free2；s-free1 本来就在 tried 里
		t.Fatalf("应只新划掉 1 个（s-free2），实得 %d（tried=%v）", n, tried)
	}
	if !tried["s-free2"] {
		t.Fatalf("同层账号应被划掉：%v", tried)
	}
	if tried["s-paid"] {
		t.Fatal("不该误伤别的层（那是\"降档\"不是\"清池\"）")
	}
}

// TestPickSinkNoOpAtBottomTier 已在最底层（已实测收费）时不再下沉。
func TestPickSinkNoOpAtBottomTier(t *testing.T) {
	store, px, _ := newTestApp(t,
		&Account{UID: "s-paid1", AccessToken: "t"},
		&Account{UID: "s-paid2", AccessToken: "t"})
	defer store.Close()
	defer px.Close()
	if err := store.SetKV(kvPickSinkOn5xx, "on"); err != nil {
		t.Fatal(err)
	}
	px.pool.NoteModelCost("s-paid1", "m", 0.5, 1000)
	px.pool.NoteModelCost("s-paid2", "m", 0.5, 1000)

	tried := map[string]bool{}
	if n := px.sinkCostTier("s-paid1", "m", tried); n != 0 {
		t.Fatalf("到底层后不该再划（划掉等于本次请求必然失败），实得 %d", n)
	}
	// tried 为 nil（调用方没在做轮换）时也不动作。
	if n := px.sinkCostTier("s-paid1", "m", nil); n != 0 {
		t.Fatalf("tried 为 nil 时应零动作，实得 %d", n)
	}
}
