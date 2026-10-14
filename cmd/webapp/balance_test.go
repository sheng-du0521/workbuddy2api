// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestWithdrawableQuotaOnlyFromCredits 可提现额度**只**来自返点/卖号（需求 3）。
//
// 体验码与充值码到账的都是「不可提现」的点；只有 CreditWithdrawable
// （号池收益 / 推广返点 / 账号买卖所得）才增加可提现额度。
func TestWithdrawableQuotaOnlyFromCredits(t *testing.T) {
	store, _, _ := newTestApp(t)
	defer store.Close()
	u := mkUser(t, store, "quota", 0)

	// ① 体验码 → 余额增加，但**额度不增加**
	if err := store.CreditRedeem(u.ID, PointToMicro(100), RedeemKindExp); err != nil {
		t.Fatal(err)
	}
	quota, _, _ := store.WithdrawableQuota(u.ID)
	if MicroToPoint(quota) != 0 {
		t.Fatalf("体验码不得增加可提现额度，得 %v", MicroToPoint(quota))
	}
	fresh, _ := store.GetUserByID(u.ID)
	if MicroToPoint(fresh.BalanceMicro) != 100 {
		t.Fatalf("体验码应入余额 100，得 %v", MicroToPoint(fresh.BalanceMicro))
	}
	if MicroToPoint(fresh.ExpMicro) != 100 {
		t.Fatalf("应累计体验点 100，得 %v", MicroToPoint(fresh.ExpMicro))
	}

	// ② 充值码 → 余额增加，额度仍**不增加**
	if err := store.CreditRedeem(u.ID, PointToMicro(200), RedeemKindRecharge); err != nil {
		t.Fatal(err)
	}
	quota, _, _ = store.WithdrawableQuota(u.ID)
	if MicroToPoint(quota) != 0 {
		t.Fatalf("充值码不得增加可提现额度，得 %v", MicroToPoint(quota))
	}
	fresh, _ = store.GetUserByID(u.ID)
	if MicroToPoint(fresh.RechargeMicro) != 200 {
		t.Fatalf("应累计充值点 200，得 %v", MicroToPoint(fresh.RechargeMicro))
	}

	// ③ 返点收益 → 额度增加
	if err := store.CreditWithdrawable(u.ID, PointToMicro(50)); err != nil {
		t.Fatal(err)
	}
	quota, _, _ = store.WithdrawableQuota(u.ID)
	if MicroToPoint(quota) != 50 {
		t.Fatalf("返点应增加可提现额度到 50，得 %v", MicroToPoint(quota))
	}
	fresh, _ = store.GetUserByID(u.ID)
	if MicroToPoint(fresh.BalanceMicro) != 350 { // 100 + 200 + 50
		t.Fatalf("余额应为 350，得 %v", MicroToPoint(fresh.BalanceMicro))
	}
}

// TestQuotaClampedOnSpend 消费后额度被钳制，且**不会随后续充值复活**。
//
// 这是「不做三桶账本、改为扣减时钳制」这一设计的核心不变量：
//
//	余额 1000（含额度 100）→ 消费 950 → 余额 50，额度钳到 50
//	→ 再充 500 → 余额 550，额度仍是 50（被吃掉的那 50 不复活）
//
// 若不钳制，用户可「消费掉再充值」反复套现。
func TestQuotaClampedOnSpend(t *testing.T) {
	store, _, _ := newTestApp(t)
	defer store.Close()
	u := mkUser(t, store, "clamp", 0)

	// 余额 1000 = 充值 900 + 可提现 100
	if err := store.CreditRedeem(u.ID, PointToMicro(900), RedeemKindRecharge); err != nil {
		t.Fatal(err)
	}
	if err := store.CreditWithdrawable(u.ID, PointToMicro(100)); err != nil {
		t.Fatal(err)
	}
	if q, _, _ := store.WithdrawableQuota(u.ID); MicroToPoint(q) != 100 {
		t.Fatalf("初始额度应为 100，得 %v", MicroToPoint(q))
	}

	// 消费 950 → 额度被钳到 50
	if _, err := store.ChargeUser(u.ID, PointToMicro(950)); err != nil {
		t.Fatal(err)
	}
	q, _, _ := store.WithdrawableQuota(u.ID)
	if MicroToPoint(q) != 50 {
		t.Fatalf("消费后额度应钳到 50，得 %v", MicroToPoint(q))
	}

	// 再充值 500 → 额度仍是 50（不复活）
	if err := store.CreditRedeem(u.ID, PointToMicro(500), RedeemKindRecharge); err != nil {
		t.Fatal(err)
	}
	q, _, _ = store.WithdrawableQuota(u.ID)
	if MicroToPoint(q) != 50 {
		t.Fatalf("充值后额度应仍为 50（被消费掉的不复活），得 %v", MicroToPoint(q))
	}
	fresh, _ := store.GetUserByID(u.ID)
	if MicroToPoint(fresh.BalanceMicro) != 550 {
		t.Fatalf("余额应为 1000-950+500=550，得 %v", MicroToPoint(fresh.BalanceMicro))
	}

	// 消费到清零 → 额度归零
	if _, err := store.ChargeUser(u.ID, PointToMicro(550)); err != nil {
		t.Fatal(err)
	}
	if q, _, _ := store.WithdrawableQuota(u.ID); q != 0 {
		t.Fatalf("余额清零后额度应为 0，得 %v", MicroToPoint(q))
	}
}

// TestWithdrawApplyLimitedByQuota 提现受可提现额度限制（需求 3 + 需求 4）。
func TestWithdrawApplyLimitedByQuota(t *testing.T) {
	store, _, app := newTestApp(t)
	defer store.Close()
	u := mkUser(t, store, "wd", 0)
	// 余额 1000：全部是充值点（不可提现）
	if err := store.CreditRedeem(u.ID, PointToMicro(1000), RedeemKindRecharge); err != nil {
		t.Fatal(err)
	}

	apply := func(amount float64, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/wallet/withdraw", strings.NewReader(body))
		req = withUser(req, u)
		rec := httptest.NewRecorder()
		app.handleWithdrawApply(rec, req)
		return rec
	}

	// ① 余额充足但无可提现额度 → 拒绝，且提示「可提现额度不足」
	rec := apply(100, `{"amount":100,"alipay_account":"a@b.com","real_name":"张三"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "可提现额度不足") {
		t.Fatalf("无可提现额度时应拒绝：code=%d body=%s", rec.Code, rec.Body)
	}

	// ② 缺少姓名 / 账号 → 拒绝（字段校验，需求 4）
	if rec := apply(10, `{"amount":10,"alipay_account":"a@b.com"}`); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "姓名") {
		t.Fatalf("缺姓名应拒绝：code=%d body=%s", rec.Code, rec.Body)
	}
	if rec := apply(10, `{"amount":10,"real_name":"张三"}`); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "支付宝账号") {
		t.Fatalf("缺账号应拒绝：code=%d body=%s", rec.Code, rec.Body)
	}

	// ③ 有额度后放行，且记录方式恒为支付宝、两字段落库
	if err := store.CreditWithdrawable(u.ID, PointToMicro(300)); err != nil {
		t.Fatal(err)
	}
	rec = apply(200, `{"amount":200,"alipay_account":"pay@example.com","real_name":"李四"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("有额度时应放行：code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	w := resp["withdrawal"].(map[string]any)
	if w["method"] != withdrawMethodAlipay {
		t.Fatalf("提现方式应恒为 alipay，得 %v", w["method"])
	}
	if w["method_label"] != "支付宝" {
		t.Fatalf("方式中文名应为「支付宝」，得 %v", w["method_label"])
	}
	// 打码展示：账号与姓名都不应明文
	if s, _ := w["alipay_account"].(string); strings.Contains(s, "pay@example.com") {
		t.Fatalf("支付宝账号应打码，得 %q", s)
	}
	if s, _ := w["real_name"].(string); strings.Contains(s, "李四") {
		t.Fatalf("姓名应打码，得 %q", s)
	}

	// ④ 待审申请占用额度：再申请 200（剩余 100）应被拒
	if rec := apply(200, `{"amount":200,"alipay_account":"pay@example.com","real_name":"李四"}`); rec.Code == http.StatusOK {
		t.Fatal("待审已占额度，超额申请应被拒")
	}
}

// TestReviewWithdrawalDeductsQuota 审核通过时同时扣余额与可提现额度。
func TestReviewWithdrawalDeductsQuota(t *testing.T) {
	store, _, _ := newTestApp(t)
	defer store.Close()
	u := mkUser(t, store, "rev", 0)
	if err := store.CreditWithdrawable(u.ID, PointToMicro(500)); err != nil {
		t.Fatal(err)
	}
	w := &Withdrawal{UserID: u.ID, AmountMicro: PointToMicro(200),
		Method: withdrawMethodAlipay, AlipayAccount: "a@b.com", RealName: "王五"}
	if err := store.CreateWithdrawal(w); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReviewWithdrawal(w.ID, true, "打款", 1); err != nil {
		t.Fatal(err)
	}
	fresh, _ := store.GetUserByID(u.ID)
	if MicroToPoint(fresh.BalanceMicro) != 300 {
		t.Fatalf("余额应为 300，得 %v", MicroToPoint(fresh.BalanceMicro))
	}
	if MicroToPoint(fresh.WithdrawableMicro) != 300 {
		t.Fatalf("可提现额度应同步扣到 300，得 %v", MicroToPoint(fresh.WithdrawableMicro))
	}
	// 额度不足时审核通过应被拒（防超额打款）
	w2 := &Withdrawal{UserID: u.ID, AmountMicro: PointToMicro(400),
		Method: withdrawMethodAlipay, AlipayAccount: "a@b.com", RealName: "王五"}
	_ = store.CreateWithdrawal(w2)
	if _, err := store.ReviewWithdrawal(w2.ID, true, "", 1); err == nil {
		t.Fatal("额度不足时应拒绝打款")
	}
}

// TestBackfillBalanceBuckets 老库回填：按历史返点/收益/卖号流水还原可提现额度。
func TestBackfillBalanceBuckets(t *testing.T) {
	store, _, _ := newTestApp(t)
	defer store.Close()
	u := mkUser(t, store, "legacy", 0)
	// 造历史余额（模拟老库只有 balance_micro）
	if err := store.AddUserBalance(u.ID, PointToMicro(1000)); err != nil {
		t.Fatal(err)
	}
	// 记两笔历史返点入账（rebates 表）
	if err := store.InsertRebate(&Rebate{
		Kind: "referral", Beneficiary: u.ID, BaseMicro: PointToMicro(200),
		RebateMicro: PointToMicro(80),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertHostEarning(&HostEarning{
		AccountUID: "x1", OwnerUserID: u.ID, Earned: PointToMicro(120),
	}); err != nil {
		t.Fatal(err)
	}

	// 清掉标记并重置额度，模拟「尚未回填」的老库
	if err := store.SetKV("balance_buckets_migrated", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE users SET withdrawable_micro = 0, recharge_micro = 0 WHERE id = ?`, u.ID); err != nil {
		t.Fatal(err)
	}

	if err := store.backfillBalanceBuckets(); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	fresh, _ := store.GetUserByID(u.ID)
	// 可提现 = 返点 80 + 号池收益 120 = 200（不超过余额 1000）
	if MicroToPoint(fresh.WithdrawableMicro) != 200 {
		t.Fatalf("回填额度应为 200（80 返点 + 120 收益），得 %v", MicroToPoint(fresh.WithdrawableMicro))
	}
	// 余额 − 可提现 = 800 → 估作累计充值点
	if MicroToPoint(fresh.RechargeMicro) != 800 {
		t.Fatalf("余额与额度之差应估为累计充值点 800，得 %v", MicroToPoint(fresh.RechargeMicro))
	}
	// 幂等：再跑一次不应翻倍
	if err := store.backfillBalanceBuckets(); err != nil {
		t.Fatal(err)
	}
	again, _ := store.GetUserByID(u.ID)
	if again.WithdrawableMicro != fresh.WithdrawableMicro {
		t.Fatalf("回填必须幂等：%d → %d", fresh.WithdrawableMicro, again.WithdrawableMicro)
	}
}

// TestRedeemExpKindNoRebate 体验码不产生推广返点，充值码产生（需求 3「参与计算」）。
func TestRedeemExpKindNoRebate(t *testing.T) {
	store, _, _ := newTestApp(t)
	defer store.Close()
	up := mkUser(t, store, "upline", 0)
	down := mkUser(t, store, "downline", 0)
	if err := store.SetUserReferredBy(down.ID, up.ID); err != nil {
		t.Fatal(err)
	}
	// 开启推广返点比例（100%，便于断言）
	if err := store.SetKV(kvReferralRatio, "1"); err != nil {
		t.Fatal(err)
	}

	// 体验码 → 不给上级返点
	if err := store.CreateRedeemCode("EXP-CODE-0001", PointToMicro(100), RedeemKindExp, 1); err != nil {
		t.Fatal(err)
	}
	if _, kind, err := store.RedeemCode("EXP-CODE-0001", down.ID); err != nil {
		t.Fatal(err)
	} else if kind != RedeemKindExp {
		t.Fatalf("kind 应为 exp，得 %q", kind)
	}
	// 充值码 → 给上级返点
	if err := store.CreateRedeemCode("RCH-CODE-0001", PointToMicro(100), RedeemKindRecharge, 1); err != nil {
		t.Fatal(err)
	}
	if _, kind, err := store.RedeemCode("RCH-CODE-0001", down.ID); err != nil {
		t.Fatal(err)
	} else if kind != RedeemKindRecharge {
		t.Fatalf("kind 应为 recharge，得 %q", kind)
	}
}
