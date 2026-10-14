// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 顺序游标轮转（PickModeRoundRobin）回归：均匀性、严格环序、防撞号窗口退化、
// 模式归一化、冷却跳过、成本分层不被绕过。
//
// 这些用例锁的是「轮转必须真的均匀且稳定」——候选来自 map 遍历，一旦哪天有人
// 去掉 pickRoundRobinLocked 里的排序，轮转就会退化成随机起点，而"看起来仍在
// 轮转"（每个号最终都会分到），只有严格环序断言能抓到。
package pool

import (
	"fmt"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestPickRoundRobinUniformAndOrdered 10 个号轮转 100 次：每个号恰好 10 次，
// 且严格按 UID 升序环形（第 i 次 = 排序后第 i%n 个）。
func TestPickRoundRobinUniformAndOrdered(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetPickMode(PickModeRoundRobin)
	if p.PickMode() != PickModeRoundRobin {
		t.Fatalf("模式应为 roundrobin，得 %s", p.PickMode())
	}
	const n = 10
	uids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		uid := fmt.Sprintf("u%02d", i)
		uids = append(uids, uid)
		p.Add(&auth.Auth{UID: uid})
	}
	counts := map[string]int{}
	seq := make([]string, 0, 100)
	for i := 0; i < 100; i++ {
		a := p.Pick("")
		if a == nil {
			t.Fatal("选号返回 nil")
		}
		counts[a.UID]++
		seq = append(seq, a.UID)
	}
	for _, u := range uids {
		if counts[u] != 10 {
			t.Errorf("%s 应被选 10 次，得 %d（分布 %v）", u, counts[u], counts)
		}
	}
	for i, got := range seq {
		if want := uids[i%n]; got != want {
			t.Fatalf("第 %d 次选号 = %s，want %s（游标轮转应严格环形）", i, got, want)
		}
	}
}

// TestPickRoundRobinFallsBackWhenAllInGap 防撞号窗口远大于测试间隔时，
// 从第 6 次起所有号都"刚被用过"→ 走 LRU（usedSeq 全序）兜底。
// 期望：仍严格均匀（轮转下"下一个待取者"与"最久未用者"是同一个号）。
func TestPickRoundRobinFallsBackWhenAllInGap(t *testing.T) {
	old := minPickGap
	minPickGap = time.Hour // 远超测试内的实际时间间隔
	t.Cleanup(func() { minPickGap = old })

	p := New("")
	p.SetPickMode(PickModeRoundRobin)
	const n = 5
	for i := 0; i < n; i++ {
		p.Add(&auth.Auth{UID: fmt.Sprintf("u%d", i)})
	}
	counts := map[string]int{}
	for i := 0; i < 10; i++ {
		a := p.Pick("")
		if a == nil {
			t.Fatal("选号返回 nil")
		}
		counts[a.UID]++
	}
	if len(counts) != n {
		t.Errorf("应覆盖全部 %d 个号，实得 %d（%v）", n, len(counts), counts)
	}
	for uid, c := range counts {
		if c != 2 {
			t.Errorf("%s 应被选 2 次（LRU 全序兜底仍均匀），得 %d", uid, c)
		}
	}
}

// TestPickModeNormalization 只有 roundrobin 生效，其余（空/非法/大小写变体）
// 一律回落 weighted —— 保证升级后行为与现状严格一致，新机制须显式开启。
func TestPickModeNormalization(t *testing.T) {
	p := New("")
	if p.PickMode() != PickModeWeighted {
		t.Fatalf("零值 Pool 应为 weighted，得 %s", p.PickMode())
	}
	for _, bad := range []string{"", "bogus", "Weighted", "ROUNDROBIN", "rr", "round-robin"} {
		p.SetPickMode(bad)
		if p.PickMode() != PickModeWeighted {
			t.Errorf("非法值 %q 应回落 weighted，得 %s", bad, p.PickMode())
		}
	}
	p.SetPickMode(PickModeRoundRobin)
	if p.PickMode() != PickModeRoundRobin {
		t.Fatalf("应切到 roundrobin，得 %s", p.PickMode())
	}
	// 切回 weighted 应即时生效（游标不重置，但不再被使用）。
	p.SetPickMode(PickModeWeighted)
	if p.PickMode() != PickModeWeighted {
		t.Fatalf("应切回 weighted，得 %s", p.PickMode())
	}
}

// TestPickRoundRobinSkipsCooling 冷却号不参与轮转，剩余号均分。
func TestPickRoundRobinSkipsCooling(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetPickMode(PickModeRoundRobin)
	for _, u := range []string{"a", "b", "c"} {
		p.Add(&auth.Auth{UID: u})
	}
	p.Cooldown("b", CoolHard, time.Hour, "test")
	counts := map[string]int{}
	for i := 0; i < 20; i++ {
		a := p.Pick("")
		if a == nil {
			t.Fatal("选号返回 nil")
		}
		counts[a.UID]++
	}
	if counts["b"] != 0 {
		t.Errorf("冷却号 b 不应被选，得 %d 次", counts["b"])
	}
	if counts["a"] != 10 || counts["c"] != 10 {
		t.Errorf("剩余两号应各 10 次，得 %v", counts)
	}
}

// TestPickRoundRobinKeepsCostTier 轮转不得绕过成本分层：已实测免费的号必须
// 优先于已实测收费的号（轮转是"同层内均匀"，不是"放弃成本优化"）。
func TestPickRoundRobinKeepsCostTier(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0) // 关停条件探索，锚定纯成本优先语义
	p.SetPickMode(PickModeRoundRobin)
	p.Add(&auth.Auth{UID: "free"})
	p.Add(&auth.Auth{UID: "paid"})
	// paid 积分远高于 free：若轮转绕过分层，paid 必然被选中。
	p.SetCredits("free", 1)
	p.SetCredits("paid", 1_000_000)
	p.NoteModelCost("free", "m", 0, 1000)
	p.NoteModelCost("paid", "m", 2.9, 1000)
	for i := 0; i < 30; i++ {
		a := p.PickExcludingForRealm(nil, "m", "")
		if a == nil || a.UID != "free" {
			t.Fatalf("第 %d 次选中 %v，want free（轮转不得绕过成本分层）", i, a)
		}
	}
}

// TestPickRoundRobinWithShrinkingCandidateSet 锁住实测踩到的坑：
// 上层分组过滤会「取候选 → 不匹配就丢进 tried → 再取」，候选集逐次收缩，
// 而 pick 每次返回都会推进被返回者的选取历史。
//
// 绝对游标实现（cursor++ 后取 cands[start%n]）在这里会因 start%n 随 n 漂移而
// 退化成偏斜 —— 实测连发 6 次请求全部落在同一个「用户申请」账号上。
// 「最久未用」实现必须仍然让目标组内严格轮流（A、G 各 5 次）。
func TestPickRoundRobinWithShrinkingCandidateSet(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetPickMode(PickModeRoundRobin)
	for _, u := range []string{"A", "B", "C", "D", "E", "F", "G"} {
		p.Add(&auth.Auth{UID: u})
	}
	target := map[string]bool{"A": true, "G": true} // 模拟「用户申请」组只有这两个
	counts := map[string]int{}
	for req := 0; req < 10; req++ {
		tried := map[string]bool{}
		got := ""
		for i := 0; i < 512; i++ {
			cand := p.PickExcludingForRealm(tried, "", "")
			if cand == nil {
				break
			}
			if !target[cand.UID] {
				tried[cand.UID] = true // 模拟组过滤：不匹配 → 本次试过
				continue
			}
			got = cand.UID
			break
		}
		if got == "" {
			t.Fatalf("第 %d 次未选到目标组账号", req)
		}
		counts[got]++
	}
	if counts["A"] != 5 || counts["G"] != 5 {
		t.Fatalf("组内轮转应各 5 次（候选集收缩不得导致偏斜），得 %v", counts)
	}
}

// TestPickRoundRobinNotTop5Limited 轮转覆盖**全部**可用号，不受 top5 短名单限制
// （若误把轮转分支放在 top5 截断之后，UID 靠后的号会被永久饿死）。
func TestPickRoundRobinNotTop5Limited(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetPickMode(PickModeRoundRobin)
	const n = 8 // > 5
	for i := 0; i < n; i++ {
		p.Add(&auth.Auth{UID: fmt.Sprintf("u%d", i)})
	}
	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		a := p.Pick("")
		if a == nil {
			t.Fatal("选号返回 nil")
		}
		seen[a.UID] = true
	}
	if len(seen) != n {
		t.Errorf("轮转应覆盖全部 %d 个号，实得 %d（%v）", n, len(seen), seen)
	}
}
