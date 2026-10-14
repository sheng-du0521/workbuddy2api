// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"path/filepath"
	"testing"
)

// newHostedProxy 建一个「已入池代挂账号 + 号主」的最小环境。
func newHostedProxy(t *testing.T, ratio, mult string) (*Store, *Proxy, int64) {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "hosted.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	owner, err := store.CreateUser("owner-"+ratio+"-"+mult, "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := store.UpsertAccount(&Account{
		UID: "h_in", Realm: "cn", AccessToken: "a",
		OwnerUserID: owner.ID, PoolState: "in",
	}); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	if err := store.SetKV(kvHostShareRatio, ratio); err != nil {
		t.Fatalf("SetKV ratio: %v", err)
	}
	if err := store.SetKV(settingMultiplier, mult); err != nil {
		t.Fatalf("SetKV mult: %v", err)
	}
	px := NewProxy(store, "")
	px.ReloadSelection()
	_ = px.SyncAccounts()
	return store, px, owner.ID
}

// ======================================================================
// 计费口径审计回归（v0.12.0 修复 A / B / C 三处漂移）
//
// 统一口径：扣费 = 本次消耗的「号池账号积分」× 计费倍率。
//   A. 上游不回报 credit 时的回落估算必须真的能估出来（此前是死代码）。
//   B. 兜底单价是「点 / 千 token」终价，不得再乘倍率（否则双重计倍率）。
//   C. 代挂分成基数是「号池积分消耗」而非「已乘倍率的收费」，并以实收封顶。
// ======================================================================

// TestEstimateCreditFallsBackToObserved 覆盖 A。
//
// 历史 bug：estimateCredit 只读 pool.ModelCost，而 cmd/webapp 不经过
// internal/server，pool.NoteModelCost 从不被调用 → 该账本恒空 → 回落估算恒 0，
// 上游漏报 credit 时直接按 0 计费（纯漏计）。
func TestEstimateCreditFallsBackToObserved(t *testing.T) {
	store, px := newHostProxy(t)
	defer store.Close()

	// pool 侧无任何观测（模拟 webapp 运行时真实状态）。
	if _, ok := px.pool.ModelCost("uid-1", "m"); ok {
		t.Fatal("前置条件：pool 成本账本应为空")
	}
	// 本进程观测到「积分 / 1k token」= 2.0（settleUsage 的 observeCost 写入路径）。
	px.observeCost("m", 2.0)

	if got := px.estimateCredit("uid-1", "m", 1000); got != 2.0 {
		t.Fatalf("1000 token 应按 2.0 积分估算，得 %v", got)
	}
	if got := px.estimateCredit("uid-1", "m", 500); got != 1.0 {
		t.Fatalf("500 token 应按 1.0 积分估算，得 %v", got)
	}
	// 无观测的模型仍应返回 0（不瞎估）。
	if got := px.estimateCredit("uid-1", "unknown-model", 1000); got != 0 {
		t.Fatalf("未观测模型应返回 0，得 %v", got)
	}
	// 非法入参。
	if got := px.estimateCredit("uid-1", "m", 0); got != 0 {
		t.Fatalf("token=0 应返回 0，得 %v", got)
	}
}

// TestReserveUnitPriceNoDoubleMultiplier 覆盖 B。
//
// 历史 bug：兜底单价在设置页标注为「点 / 千 token」（终价），代码却再乘一次倍率。
func TestReserveUnitPriceNoDoubleMultiplier(t *testing.T) {
	store, px := newHostProxy(t)
	defer store.Close()

	// 倍率 3.0，兜底单价 5 点/千 token（终价）。
	if err := store.SetKV(settingMultiplier, "3"); err != nil {
		t.Fatalf("SetKV multiplier: %v", err)
	}
	if err := store.SetKV(settingReserveRate, "5"); err != nil {
		t.Fatalf("SetKV reserve: %v", err)
	}
	if got, ok := px.reserveUnitPrice("m"); !ok || got != 5 {
		t.Fatalf("兜底单价应原样返回 5（不再乘倍率），得 %v ok=%v", got, ok)
	}

	// 观测单价是「积分 / 1k」，必须乘倍率换成点：2.0 × 3 = 6.0。
	px.observeCost("m2", 2.0)
	if got, ok := px.reserveUnitPrice("m2"); !ok || got != 6.0 {
		t.Fatalf("观测单价应换算为 2.0 × 3 = 6.0，得 %v ok=%v", got, ok)
	}
}

// TestHostShareBasisIsPoolCredit 覆盖 C。
//
// 分成基数 = 号池积分消耗，倍率不再缩放号主收益；但以平台实收封顶。
func TestHostShareBasisIsPoolCredit(t *testing.T) {
	cases := []struct {
		name         string
		mult         string
		credit       float64 // 本次消耗的号池积分
		wantEarnedPt float64 // 期望号主入账（点）
	}{
		// 倍率 2：charged = 20 点，分成 = 10 × 0.6 = 6 点（未被封顶）。
		{"倍率高于分成比例", "2", 10, 6},
		// 倍率 1：charged = 10 点，分成 6 点（未被封顶）。
		{"倍率等于 1", "1", 10, 6},
		// 倍率 0.5：charged = 5 点，分成名义 6 点 → 被实收封顶为 5 点。
		{"倍率低于分成比例应封顶", "0.5", 10, 5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store, px, ownerID := newHostedProxy(t, "0.6", c.mult)
			defer store.Close()

			mult := store.PricingMultiplier()
			charged := PointToMicro(c.credit * mult)
			px.creditHostOwner("h_in", &User{ID: 99}, "m", c.credit, charged)

			fresh, _ := store.GetUserByID(ownerID)
			if want := PointToMicro(c.wantEarnedPt); fresh.BalanceMicro != want {
				t.Fatalf("号主应得 %v 点（%d 微点），得 %d", c.wantEarnedPt, want, fresh.BalanceMicro)
			}
			// 分成绝不超过平台实收。
			if fresh.BalanceMicro > charged {
				t.Fatalf("分成 %d 超过平台实收 %d，平台会倒贴", fresh.BalanceMicro, charged)
			}
		})
	}
}

// TestHostShareUnaffectedByLowMultiplier 锁定 C 的核心诉求：
// 只要没触到实收封顶，号主收益只取决于被消耗的积分，与倍率无关。
func TestHostShareUnaffectedByLowMultiplier(t *testing.T) {
	earn := func(mult string) int64 {
		store, px, ownerID := newHostedProxy(t, "0.5", mult)
		defer store.Close()
		m := store.PricingMultiplier()
		// 每次固定消耗 100 积分，实收按倍率折算。
		px.creditHostOwner("h_in", &User{ID: 99}, "m", 100, PointToMicro(100*m))
		fresh, _ := store.GetUserByID(ownerID)
		return fresh.BalanceMicro
	}
	// 100 积分 × 0.5 = 50 点。倍率 2 / 10 都高于分成比例 0.5，不触顶 → 恒 50 点。
	a, b := earn("2"), earn("10")
	if a != b || a != 50*MicroPerPoint {
		t.Fatalf("未触顶时号主分成应恒为 50 点且不随倍率变化，得 倍率2→%d 倍率10→%d", a, b)
	}
	// 触顶场景：倍率 0.1 < 分成比例 0.5 → 名义 50 点被压到实收 10 点（平台不倒贴）。
	// 这是设计行为，也是「想有留成就要保证 倍率 > 分成比例」的由来。
	if capped := earn("0.1"); capped != PointToMicro(10.0) {
		t.Fatalf("倍率低于分成比例时应被实收封顶为 10 点，得 %d", capped)
	}
}

// TestSettleUsageBasisIsConsumedCredit 端到端锁定：扣费 = 号池积分 × 倍率。
func TestSettleUsageBasisIsConsumedCredit(t *testing.T) {
	store, px := newHostProxy(t)
	defer store.Close()

	u, err := store.CreateUser("payer", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := store.AddUserBalance(u.ID, 1000*MicroPerPoint); err != nil {
		t.Fatalf("AddUserBalance: %v", err)
	}
	if err := store.SetKV(settingMultiplier, "0.25"); err != nil {
		t.Fatalf("SetKV mult: %v", err)
	}

	key := &APIKey{ID: 1, UserID: u.ID}
	ui := usageInfo{PromptTokens: 800, CompletionTokens: 200, Credit: 8, HasCredit: true}
	settled, charged := px.settleUsage(u, key, "m", "uid-x", ui, nil, "ok", "", "", 1)
	if !settled {
		t.Fatal("应结算成功")
	}
	// 8 积分 × 0.25 = 2 点 = 2000 微点。
	if want := PointToMicro(2.0); charged != want {
		t.Fatalf("扣费应为 8 × 0.25 = 2 点（%d 微点），得 %d", want, charged)
	}
	fresh, _ := store.GetUserByID(u.ID)
	if balance := 1000*MicroPerPoint - charged; fresh.BalanceMicro != balance {
		t.Fatalf("余额应被扣到 %d，得 %d", balance, fresh.BalanceMicro)
	}
}

// TestSettleUsageSkipsEstimateOnFailure 锁定：失败响应不得凭空估算 credit 扣费。
func TestSettleUsageSkipsEstimateOnFailure(t *testing.T) {
	store, px := newHostProxy(t)
	defer store.Close()

	u, _ := store.CreateUser("payer2", "x")
	_ = store.AddUserBalance(u.ID, 100*MicroPerPoint)
	px.observeCost("m", 2.0) // 有观测单价，旧实现会对失败响应也估一笔

	key := &APIKey{ID: 2, UserID: u.ID}
	// 上游未回报 credit 且状态为失败 → 必须计 0。
	ui := usageInfo{PromptTokens: 900, CompletionTokens: 0, HasCredit: false}
	if _, charged := px.settleUsage(u, key, "m", "uid-y", ui, nil, "error", "boom", "", 1); charged != 0 {
		t.Fatalf("失败响应不应计费，得 %d", charged)
	}
	fresh, _ := store.GetUserByID(u.ID)
	if fresh.BalanceMicro != 100*MicroPerPoint {
		t.Fatalf("失败请求不应扣余额，得 %d", fresh.BalanceMicro)
	}
}
