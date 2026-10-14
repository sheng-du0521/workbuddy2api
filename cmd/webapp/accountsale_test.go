// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// mkUser 建一个测试用户并给余额（点）。
func mkUser(t *testing.T, s *Store, name string, points float64) *User {
	t.Helper()
	u, err := s.CreateUser(name, "x")
	if err != nil {
		t.Fatalf("CreateUser(%s): %v", name, err)
	}
	if points > 0 {
		if err := s.AddUserBalance(u.ID, PointToMicro(points)); err != nil {
			t.Fatalf("AddUserBalance: %v", err)
		}
	}
	return u
}

// TestSaleUnlistRestoresInferredGroup 上架前「未显式设组」的账号，下架后必须回到
// 「按来源推断」而不是留在上架时写死的「闲置」（v0.20.1 修正）。
//
// 缺陷形态：早期 unlist 用 `if sale.PrevGroup != ""` 才恢复，而 list 记的是
// effectiveConsumeGroup（推断值）。改用原始值后，prev 为空 = 未显式设组，
// 下架时必须**无条件**写回（包括写回空串）。
func TestSaleUnlistRestoresInferredGroup(t *testing.T) {
	// 平台自持、未显式设组 → 推断为「反代」
	store, px, app := newTestApp(t, &Account{UID: "u1", Realm: "cn", AccessToken: "a"})
	defer store.Close()
	px.pool.SetCreditsDetailed("u1", 5000, 0)
	if err := px.SyncAccounts(); err != nil {
		t.Fatal(err)
	}

	list := func(action string) *httptest.ResponseRecorder {
		body := `{"action":"` + action + `"}`
		if action == "list" {
			body = `{"action":"list","price":100}`
		}
		req := httptest.NewRequest("POST", "/api/admin/accounts/u1/sale", strings.NewReader(body))
		req.SetPathValue("uid", "u1")
		rec := httptest.NewRecorder()
		app.handleAdminAccountSale(rec, req) // 管理密钥通道：无 user，不得 panic
		return rec
	}

	// 上架：prev 应记录**原始值**（空串），而非推断出的 proxy。
	if rec := list("list"); rec.Code != http.StatusOK {
		t.Fatalf("list failed: code=%d body=%s", rec.Code, rec.Body)
	}
	sale, _ := store.GetAccountSale("u1")
	if sale == nil || !sale.OnSale {
		t.Fatalf("must be on sale: %+v", sale)
	}
	if sale.PrevGroup != "" {
		t.Fatalf("prev_group must record the **raw** value (empty), got %q", sale.PrevGroup)
	}
	// 挂牌期间归属闲置。
	if got := effectiveConsumeGroup(mustAccount(t, app, "u1")); got != cgIdle {
		t.Fatalf("listed account must be idle, got %q", got)
	}

	// 下架：恢复为「空 → 按来源推断 → 反代」，而不是留在 idle。
	if rec := list("unlist"); rec.Code != http.StatusOK {
		t.Fatalf("unlist failed: code=%d body=%s", rec.Code, rec.Body)
	}
	acc := mustAccount(t, app, "u1")
	if acc.ConsumeGroup != "" {
		t.Fatalf("unlist must restore raw empty group, got explicit %q", acc.ConsumeGroup)
	}
	if got := effectiveConsumeGroup(acc); got != cgProxy {
		t.Fatalf("unlisted platform account must infer proxy, got %q", got)
	}
}

// TestListableErrorRules 上架前置校验：范围与排除项逐个验证。
func TestListableErrorRules(t *testing.T) {
	base := func() *Account { return &Account{UID: "u1", Realm: "cn", AccessToken: "a"} }

	// 平台自持、健康 → 可上架
	if msg := listableError(base(), nil, 5000, true); msg != "" {
		t.Fatalf("platform-owned healthy account must be listable, got %q", msg)
	}
	// 已禁用 → 拒绝
	a := base()
	a.Disabled = true
	if msg := listableError(a, nil, 5000, true); !strings.Contains(msg, "禁用") {
		t.Fatalf("disabled must be rejected, got %q", msg)
	}
	// 异常分组 → 拒绝
	a = base()
	a.ConsumeGroup = cgAbnormal
	if msg := listableError(a, nil, 5000, true); !strings.Contains(msg, "异常") {
		t.Fatalf("abnormal group must be rejected, got %q", msg)
	}
	// 代挂但未入池 → 拒绝
	a = base()
	a.OwnerUserID = 7
	a.PoolState = ""
	if msg := listableError(a, nil, 5000, true); !strings.Contains(msg, "入池") {
		t.Fatalf("hosted not-in-pool must be rejected, got %q", msg)
	}
	// 代挂已入池 → 可上架
	a.PoolState = "in"
	if msg := listableError(a, nil, 5000, true); msg != "" {
		t.Fatalf("hosted in-pool must be listable, got %q", msg)
	}
	// 已在售 → 拒绝
	if msg := listableError(base(), &AccountSale{OnSale: true}, 5000, true); !strings.Contains(msg, "已在售") {
		t.Fatalf("already on sale must be rejected, got %q", msg)
	}
	// 冷却中 → 拒绝
	a = base()
	a.CooldownUntil = 1 << 40
	if msg := listableError(a, nil, 5000, true); !strings.Contains(msg, "冷却") {
		t.Fatalf("cooling must be rejected, got %q", msg)
	}
	// v0.20.0 交易积分门槛：低于 4000 → 拒绝
	if msg := listableError(base(), nil, tradeMinCredits-1, true); !strings.Contains(msg, "积分不足") {
		t.Fatalf("credits below tradeMinCredits must be rejected, got %q", msg)
	}
	// 恰好等于门槛 → 放行（边界含等号）
	if msg := listableError(base(), nil, tradeMinCredits, true); msg != "" {
		t.Fatalf("credits == tradeMinCredits must be allowed, got %q", msg)
	}
	// 积分未知 → 拒绝（0 既可能是「真 0」也可能是「没拉过」，交易不放行未知态）
	if msg := listableError(base(), nil, 0, false); !strings.Contains(msg, "积分") {
		t.Fatalf("unknown credits must be rejected, got %q", msg)
	}
}

// TestSaleListThenUnlistRestoresGroup 上架归闲置、下架恢复原分组。
func TestSaleListThenUnlistRestoresGroup(t *testing.T) {
	store, _, _ := newTestApp(t, &Account{UID: "u1", Realm: "cn", AccessToken: "a", ConsumeGroup: cgProxy})
	defer store.Close()

	acc, _ := store.GetAccount("u1")
	prev := effectiveConsumeGroup(acc)
	if prev != cgProxy {
		t.Fatalf("setup: want proxy, got %q", prev)
	}
	if err := store.SetAccountOnSale("u1", true, 1, "挂牌", prev, PointToMicro(100)); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAccountConsumeGroup("u1", cgIdle); err != nil {
		t.Fatal(err)
	}
	got, _ := store.GetAccount("u1")
	if effectiveConsumeGroup(got) != cgIdle {
		t.Fatalf("listed account must be idle, got %q", effectiveConsumeGroup(got))
	}
	sale, _ := store.GetAccountSale("u1")
	if sale == nil || !sale.OnSale || sale.PrevGroup != cgProxy {
		t.Fatalf("sale record wrong: %+v", sale)
	}

	// 下架 → 恢复 proxy
	if err := store.SetAccountOnSale("u1", false, 1, "", "", 0); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAccountConsumeGroup("u1", sale.PrevGroup); err != nil {
		t.Fatal(err)
	}
	got2, _ := store.GetAccount("u1")
	if effectiveConsumeGroup(got2) != cgProxy {
		t.Fatalf("unlisted account must restore proxy, got %q", effectiveConsumeGroup(got2))
	}
	sale2, _ := store.GetAccountSale("u1")
	if sale2.OnSale {
		t.Fatal("must be off sale after unlist")
	}
}

// TestBuyAccountFlowOwnershipAndSplit 买断核心：归属转移 + 资金分账 + 平台自持号全归平台。
func TestBuyAccountFlowOwnershipAndSplit(t *testing.T) {
	// 平台自持号（seller=0）
	store, _, _ := newTestApp(t, &Account{UID: "plat", Realm: "cn", AccessToken: "a"})
	defer store.Close()
	buyer := mkUser(t, store, "buyer", 1000)

	// 上架
	_ = store.SetAccountOnSale("plat", true, 1, "", cgProxy, PointToMicro(300))
	_ = store.SetAccountConsumeGroup("plat", cgIdle)

	p, err := store.BuyAccount("plat", buyer.ID)
	if err != nil {
		t.Fatalf("BuyAccount: %v", err)
	}
	if p.SellerUserID != 0 || p.PlatformMicro != p.AmountMicro || p.SellerMicro != 0 {
		t.Fatalf("platform-owned split wrong: %+v", p)
	}
	acc, _ := store.GetAccount("plat")
	if acc.OwnerUserID != buyer.ID {
		t.Fatalf("ownership must transfer to buyer, got %d", acc.OwnerUserID)
	}
	if acc.HostGroup != hostGroupBought {
		t.Fatalf("host group must be %q, got %q", hostGroupBought, acc.HostGroup)
	}
	if acc.PoolState != "" {
		t.Fatalf("pool state must be cleared, got %q", acc.PoolState)
	}
	if effectiveConsumeGroup(acc) != cgIdle {
		t.Fatalf("bought account stays idle, got %q", effectiveConsumeGroup(acc))
	}
	sale, _ := store.GetAccountSale("plat")
	if sale.OnSale {
		t.Fatal("must be off sale after purchase")
	}
	fresh, _ := store.GetUserByID(buyer.ID)
	if got := MicroToPoint(fresh.BalanceMicro); got != 700 {
		t.Fatalf("buyer balance want 700, got %v", got)
	}
}

// TestBuyAccountSplitsToHost 用户账号买断：成交金额**全额**归卖家（v0.20.0 口径）。
//
// v0.19.0 曾按号池分成比例分账；v0.20.0 改为全额归卖家 —— 成交的是「账号资产
// 本身」（含其剩余积分），与「代挂账号服务平台流量拿分成」是两件事，平台只提供
// 撮合与资金流转，不再抽成。平台自持号仍全额归平台。
func TestBuyAccountSplitsToHost(t *testing.T) {
	store, _, _ := newTestApp(t, &Account{UID: "hosted", Realm: "cn", AccessToken: "a",
		OwnerUserID: 0, PoolState: "in"})
	defer store.Close()
	host := mkUser(t, store, "host", 0)
	buyer := mkUser(t, store, "buyer2", 1000)
	// 把账号归属到 host，并设为已入池
	if err := store.SetAccountOwner("hosted", host.ID); err != nil {
		t.Fatal(err)
	}
	_ = store.SetAccountPoolState("hosted", "in", "")

	_ = store.SetAccountOnSale("hosted", true, 1, "", cgProxy, PointToMicro(1000))
	_ = store.SetAccountConsumeGroup("hosted", cgIdle)

	p, err := store.BuyAccount("hosted", buyer.ID)
	if err != nil {
		t.Fatalf("BuyAccount: %v", err)
	}
	if p.SellerUserID != host.ID {
		t.Fatalf("seller must be host %d, got %d", host.ID, p.SellerUserID)
	}
	if p.SellerMicro != p.AmountMicro || p.PlatformMicro != 0 {
		t.Fatalf("user account: amount must go **fully** to seller: %+v", p)
	}
	hu, _ := store.GetUserByID(host.ID)
	if hu.BalanceMicro != p.AmountMicro {
		t.Fatalf("host must receive full amount: want %d got %d", p.AmountMicro, hu.BalanceMicro)
	}
	bu, _ := store.GetUserByID(buyer.ID)
	if MicroToPoint(bu.BalanceMicro) != 0 {
		t.Fatalf("buyer must be charged full amount: want 0 got %v", MicroToPoint(bu.BalanceMicro))
	}
	acc, _ := store.GetAccount("hosted")
	if acc.OwnerUserID != buyer.ID {
		t.Fatalf("ownership must transfer to buyer, got %d", acc.OwnerUserID)
	}
}

// TestBuyAccountConcurrentOneWinner 并发抢单：同账号多买家同时下单，仅一个成功。
func TestBuyAccountConcurrentOneWinner(t *testing.T) {
	store, _, _ := newTestApp(t, &Account{UID: "hot", Realm: "cn", AccessToken: "a"})
	defer store.Close()
	const N = 8
	buyers := make([]*User, N)
	for i := 0; i < N; i++ {
		buyers[i] = mkUser(t, store, "b"+string(rune('a'+i)), 500)
	}
	_ = store.SetAccountOnSale("hot", true, 1, "", cgProxy, PointToMicro(100))
	_ = store.SetAccountConsumeGroup("hot", cgIdle)

	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			if _, err := store.BuyAccount("hot", id); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}(buyers[i].ID)
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("exactly one buyer must win, got %d", ok)
	}
	// 只有赢家被扣款（其余人余额不变）。
	charged := 0
	for _, b := range buyers {
		fresh, _ := store.GetUserByID(b.ID)
		if MicroToPoint(fresh.BalanceMicro) != 500 {
			charged++
		}
	}
	if charged != 1 {
		t.Fatalf("exactly one buyer must be charged, got %d", charged)
	}
}

// TestBuyAccountInsufficientBalance 余额不足：拒绝且无任何副作用（含账号仍在售）。
func TestBuyAccountInsufficientBalance(t *testing.T) {
	store, _, _ := newTestApp(t, &Account{UID: "p", Realm: "cn", AccessToken: "a"})
	defer store.Close()
	buyer := mkUser(t, store, "poor", 10)
	_ = store.SetAccountOnSale("p", true, 1, "", cgProxy, PointToMicro(999))

	_, err := store.BuyAccount("p", buyer.ID)
	if err != ErrInsufficientBalance {
		t.Fatalf("want ErrInsufficientBalance, got %v", err)
	}
	// 事务回滚：账号仍在售、归属未变、余额未变。
	sale, _ := store.GetAccountSale("p")
	if !sale.OnSale {
		t.Fatal("rollback must keep account on sale")
	}
	acc, _ := store.GetAccount("p")
	if acc.OwnerUserID != 0 {
		t.Fatal("rollback must keep ownership")
	}
	fresh, _ := store.GetUserByID(buyer.ID)
	if MicroToPoint(fresh.BalanceMicro) != 10 {
		t.Fatal("rollback must keep buyer balance")
	}
}

// TestBuyOwnAccountRejected 不能买自己的号。
func TestBuyOwnAccountRejected(t *testing.T) {
	store, _, _ := newTestApp(t, &Account{UID: "mine", Realm: "cn", AccessToken: "a"})
	defer store.Close()
	me := mkUser(t, store, "me", 1000)
	_ = store.SetAccountOwner("mine", me.ID)
	_ = store.SetAccountPoolState("mine", "in", "")
	_ = store.SetAccountOnSale("mine", true, 1, "", cgProxy, PointToMicro(100))

	if _, err := store.BuyAccount("mine", me.ID); err == nil {
		t.Fatal("buying own account must fail")
	}
}

// TestAdminAccountSaleWithoutUser adminGuard 的 X-Admin-Key（运维/脚本）通道
// **不注入 user**，此时 userFrom(r) 为 nil。handler 必须容忍 nil —— 此前直接
// 解引用 u.ID 会 panic，真机表现为「curl 无任何响应（连接被断）」。
func TestAdminAccountSaleWithoutUser(t *testing.T) {
	store, px, app := newTestApp(t, &Account{UID: "k1", Realm: "cn", AccessToken: "a", ConsumeGroup: cgProxy})
	defer store.Close()
	// 种入权威积分读数（交易门槛 4000）：未拉取过的账号会被新门槛正确拦截。
	px.pool.SetCreditsDetailed("k1", 5000, 0)

	// 上架（无 user 上下文）
	req := httptest.NewRequest("POST", "/api/admin/accounts/k1/sale", strings.NewReader(`{"action":"list","price":50}`))
	req.SetPathValue("uid", "k1")
	rec := httptest.NewRecorder()
	app.handleAdminAccountSale(rec, req) // 不得 panic
	if rec.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", rec.Code, rec.Body)
	}
	got, _ := store.GetAccount("k1")
	if effectiveConsumeGroup(got) != cgIdle {
		t.Fatalf("listed account must be idle, got %q", effectiveConsumeGroup(got))
	}

	// 下架（无 user 上下文），应恢复上架前分组
	req2 := httptest.NewRequest("POST", "/api/admin/accounts/k1/sale", strings.NewReader(`{"action":"unlist"}`))
	req2.SetPathValue("uid", "k1")
	rec2 := httptest.NewRecorder()
	app.handleAdminAccountSale(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("unlist status=%d body=%s", rec2.Code, rec2.Body)
	}
	got2, _ := store.GetAccount("k1")
	if effectiveConsumeGroup(got2) != cgProxy {
		t.Fatalf("unlisted must restore proxy, got %q", effectiveConsumeGroup(got2))
	}
}

// TestSalePriceValidation 挂牌价校验（v0.21.0 需求 2「以货币（元）为单位，并进行输入校验」）。
//
// 金额校验从「买断时买家填价」搬到了「上架时卖家定价」，覆盖：
//   - 非正数 / NaN / 无穷 → 拒绝
//   - 超过上限 → 拒绝
//   - 超过两位小数 → 拒绝
//   - 合法值（含边界）→ 通过，且元→点→微点换算自洽
func TestSalePriceValidation(t *testing.T) {
	const rate = 0.01 // 1 元 = 100 点（默认折算比例）

	// 非法输入应被拒（错误文案非空）
	bad := []struct {
		name string
		yuan float64
	}{
		{"零", 0},
		{"负数", -1},
		{"NaN", math.NaN()},
		{"正无穷", math.Inf(1)},
		{"超上限", 1e12},
		{"三位小数", 1.234},
	}
	for _, c := range bad {
		t.Run("拒绝/"+c.name, func(t *testing.T) {
			if _, msg := parseSalePriceYuan(c.yuan, rate); msg == "" {
				t.Fatalf("yuan=%v 应被拒，却通过", c.yuan)
			}
		})
	}

	// 合法输入应通过，且换算正确：金额（元） ÷ 比例 = 点数，× 1000 = 微点
	good := []struct {
		name string
		yuan float64
	}{
		{"最小两位小数", 0.01},
		{"整数", 100},
		{"两位小数", 12.34},
	}
	for _, c := range good {
		t.Run("通过/"+c.name, func(t *testing.T) {
			micro, msg := parseSalePriceYuan(c.yuan, rate)
			if msg != "" {
				t.Fatalf("yuan=%v 应通过，却报 %q", c.yuan, msg)
			}
			// 元 → 点 → 微点：0.01 元 = 1 点 = 1000 微点
			wantPoints := c.yuan / rate
			if got := MicroToPoint(micro); math.Abs(got-wantPoints) > 1e-6 {
				t.Fatalf("yuan=%v 换算得 %.6f 点，期望 %.6f", c.yuan, got, wantPoints)
			}
			// 再交给 validateSalePrice 应同样通过（两处口径一致）
			if msg := validateSalePrice(micro); msg != "" {
				t.Fatalf("validateSalePrice 与 parseSalePriceYuan 口径不一致: %q", msg)
			}
		})
	}

	// 折算比例未配置（<=0）→ 拒绝（而不是算出 0 或无穷）
	if _, msg := parseSalePriceYuan(100, 0); msg == "" {
		t.Fatal("折算比例为 0 时应拒绝")
	}
}

// TestListRejectsBadPrice 上架接口对非法挂牌价的拦截（含「不填价格」）。
func TestListRejectsBadPrice(t *testing.T) {
	store, px, app := newTestApp(t, &Account{UID: "p1", Realm: "cn", AccessToken: "a"})
	defer store.Close()
	px.pool.SetCreditsDetailed("p1", 5000, 0)

	cases := []struct{ name, body string }{
		{"不填价格", `{"action":"list"}`},
		{"价格为 0", `{"action":"list","price":0}`},
		{"价格为负", `{"action":"list","price":-1}`},
		{"三位小数", `{"action":"list","price":1.234}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/api/admin/accounts/p1/sale", strings.NewReader(c.body))
			req.SetPathValue("uid", "p1")
			rec := httptest.NewRecorder()
			app.handleAdminAccountSale(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d want 400 body=%s", rec.Code, rec.Body)
			}
			// 不得留下在售记录
			if s, _ := store.GetAccountSale("p1"); s != nil && s.OnSale {
				t.Fatal("非法价格不得留下在售状态")
			}
		})
	}
}

// TestBuyUsesListedPrice 买断必须按挂牌价成交，买家不能自行指定金额（v0.21.0）。
func TestBuyUsesListedPrice(t *testing.T) {
	store, px, app := newTestApp(t, &Account{UID: "v1", Realm: "cn", AccessToken: "a"})
	defer store.Close()
	px.pool.SetCreditsDetailed("v1", 5000, 0)
	buyer := mkUser(t, store, "vbuyer2", 200)
	_ = store.SetAccountOnSale("v1", true, 1, "", cgProxy, PointToMicro(120))
	_ = store.SetAccountConsumeGroup("v1", cgIdle)

	// 请求体里塞一个极低金额，也不应影响成交价。
	req := httptest.NewRequest("POST", "/api/sale/accounts/v1/buy", strings.NewReader(`{"amount":1}`))
	req.SetPathValue("uid", "v1")
	req = withUser(req, buyer)
	rec := httptest.NewRecorder()
	app.handleBuyAccount(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if got := resp["amount"].(float64); got != 120 {
		t.Fatalf("成交价应为挂牌价 120，得 %v", got)
	}
	// 买家按挂牌价被扣款：200 - 120 = 80（证明买家请求里的 amount=1 被忽略）。
	fresh, _ := store.GetUserByID(buyer.ID)
	if MicroToPoint(fresh.BalanceMicro) != 80 {
		t.Fatalf("买家余额应为 200-120=80，得 %v", MicroToPoint(fresh.BalanceMicro))
	}
	// 卖家（平台自持 → 入平台台账）不应有用户入账；此处仅断言台账金额为挂牌价。
	if p, err := store.ListAccountPurchases(10); err == nil {
		found := false
		for _, x := range p {
			if x.AccountUID == "v1" {
				found = true
				if MicroToPoint(x.AmountMicro) != 120 {
					t.Fatalf("台账金额应为挂牌价 120，得 %v", MicroToPoint(x.AmountMicro))
				}
			}
		}
		if !found {
			t.Fatal("应留下一条买断台账")
		}
	}
}

// TestHandleBuyAccountSuccess handler 层成功路径：返回凭证 + 账号转入买家代挂序列。
func TestHandleBuyAccountSuccess(t *testing.T) {
	store, px, app := newTestApp(t, &Account{UID: "s1", Realm: "cn", AccessToken: "a"})
	defer store.Close()
	px.pool.SetCreditsDetailed("s1", 5000, 0)
	buyer := mkUser(t, store, "sbuyer", 500)
	_ = store.SetAccountOnSale("s1", true, 1, "", cgProxy, PointToMicro(120))
	_ = store.SetAccountConsumeGroup("s1", cgIdle)

	req := httptest.NewRequest("POST", "/api/sale/accounts/s1/buy", strings.NewReader(`{"amount":120.5}`))
	req.SetPathValue("uid", "s1")
	req = withUser(req, buyer)
	rec := httptest.NewRecorder()
	app.handleBuyAccount(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["ok"] != true || resp["host_group"] != hostGroupBought {
		t.Fatalf("resp=%v", resp)
	}
	acc, _ := store.GetAccount("s1")
	if acc.OwnerUserID != buyer.ID || acc.HostGroup != hostGroupBought {
		t.Fatalf("account not moved to buyer: %+v", acc)
	}
	// 二次买断 → 409（已不在售）。
	req2 := httptest.NewRequest("POST", "/api/sale/accounts/s1/buy", strings.NewReader(`{"amount":1}`))
	req2.SetPathValue("uid", "s1")
	req2 = withUser(req2, buyer)
	rec2 := httptest.NewRecorder()
	app.handleBuyAccount(rec2, req2)
	if rec2.Code != http.StatusConflict {
		t.Fatalf("second buy must be 409, got %d", rec2.Code)
	}
}
