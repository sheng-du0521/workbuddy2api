// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package pool

import (
	"path/filepath"
	"testing"

	"workbuddy2api/internal/auth"
)

// TestCostTierQueries 成本层只读查询（R6 的降档靠它划层）。
func TestCostTierQueries(t *testing.T) {
	p := New(filepath.Join(t.TempDir(), "state.json"))
	defer p.Close()
	p.Add(&auth.Auth{UID: "free-1"})
	p.Add(&auth.Auth{UID: "free-2"})
	p.Add(&auth.Auth{UID: "paid-1"})
	p.Add(&auth.Auth{UID: "unknown-1"})
	p.Add(&auth.Auth{UID: "off-1"})
	p.Disable("off-1", "test")

	// tier 记录：0 元消耗 = 已实测免费；>0 = 已实测收费。无观测 = tier 1。
	p.NoteModelCost("free-1", "m", 0, 1000)
	p.NoteModelCost("free-2", "m", 0, 1000)
	p.NoteModelCost("paid-1", "m", 0.5, 1000)

	if tier, ok := p.CostTierOf("free-1", "m"); !ok || tier != 0 {
		t.Fatalf("free-1 应为 tier 0，实得 (%d,%v)", tier, ok)
	}
	if tier, _ := p.CostTierOf("paid-1", "m"); tier != 2 {
		t.Fatalf("paid-1 应为 tier 2，实得 %d", tier)
	}
	if tier, _ := p.CostTierOf("unknown-1", "m"); tier != 1 {
		t.Fatalf("无观测应为 tier 1，实得 %d", tier)
	}
	if _, ok := p.CostTierOf("no-such-uid", "m"); ok {
		t.Fatal("不存在的账号应返回 ok=false（降档时必须能识别，否则会去划一个不存在的层）")
	}
	// 另一模型上无观测 → 归 tier 1（层是 (账号, 模型) 维度的，不是账号维度）。
	if tier, _ := p.CostTierOf("free-1", "other"); tier != 1 {
		t.Fatalf("换模型应回到 tier 1，实得 %d", tier)
	}

	free := p.UIDsInCostTier("m", 0)
	if len(free) != 2 {
		t.Fatalf("tier 0 应有 2 个，实得 %v", free)
	}
	for _, u := range free {
		if u == "off-1" {
			t.Fatal("已禁用账号不该出现在层里（会让\"已排除 N 个\"虚高）")
		}
	}
	if got := p.UIDsInCostTier("m", 2); len(got) != 1 || got[0] != "paid-1" {
		t.Fatalf("tier 2 应只有 paid-1，实得 %v", got)
	}
	// 空层返回 nil，不 panic。
	if got := p.UIDsInCostTier("m", 9); len(got) != 0 {
		t.Fatalf("不存在的层应为空，实得 %v", got)
	}
}
