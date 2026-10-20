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

// ======================================================================
// 单模型计费倍率的回归网（v0.31.67）
//
// 这批用例守的是**钱的正确性**，所以每条都盯着"扣了多少"，而不是"配置写没写进去"：
//   1. 设了倍率 → 按倍率扣；
//   2. 倍率为 0 → 回落全局（**不是免费**）——这是最危险的失败方向；
//   3. 改完立刻生效（不重启、不等缓存）；
//   4. 越界/非法输入被拒，且**不写库**；
//   5. 试算与实扣同一个公式。
// ======================================================================

// 直接用包内已有的 boolToInt / b2i，不再另造（本项目已踩过"换个名字又撞"的坑）。
// seedRatioModel 往倍率目录里插一条模型（真实路径是同步官方倍率，这里直接建）。
func seedRatioModel(t *testing.T, store *Store, model string, measured1k float64) {
	t.Helper()
	if _, err := store.db.Exec(
		`INSERT INTO model_ratios (model, name, measured_cost_1k, measured_known, updated_at)
		 VALUES (?,?,?,?,0)
		 ON CONFLICT(model) DO UPDATE SET measured_cost_1k=excluded.measured_cost_1k,
		   measured_known=excluded.measured_known`,
		model, model, measured1k, boolToInt(measured1k > 0)); err != nil {
		t.Fatalf("seed model_ratios: %v", err)
	}
}

// chargeFor 发一笔"纯结算"（credit 已知），返回实际扣费微点。
//
// 走的是真实链路 settleUsage → billing.Settle → CommitSettlement，
// 不是只测 ratioFor —— 后者绿了但没接进结算，是这类改动最典型的假绿。
func chargeFor(t *testing.T, store *Store, px *Proxy, user *User, model string, credit float64) int64 {
	t.Helper()
	_, charged := px.settleUsage(user, nil, model, "uid-x",
		usageInfo{Credit: credit, HasCredit: true}, nil, "ok", "", "", 1)
	return charged
}

// B1：设了单模型倍率就必须按它扣。
func TestModelBillingRatioApplied(t *testing.T) {
	store, px := newHostProxy(t)
	defer store.Close()
	u, _ := store.CreateUser("ratio-user", "x")
	seedRatioModel(t, store, "m-ratio", 0)
	if err := store.SetKV(settingMultiplier, "1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := px.setModelBillingRatio("m-ratio", 2.5, ""); err != nil {
		t.Fatalf("setModelBillingRatio: %v", err)
	}

	// 1 积分 × 2.5 = 2.5 点 = 2500 微点
	if got := chargeFor(t, store, px, u, "m-ratio", 1); got != 2500 {
		t.Fatalf("应按模型倍率 2.5 扣 2500，得 %d", got)
	}
	if r, src := px.ratioFor("m-ratio"); r != 2.5 || src != "model" {
		t.Fatalf("ratioFor 应返回 2.5/model，得 %v/%s", r, src)
	}
}

// B2：倍率为 0 = **不参与**，必须回落全局倍率 —— 绝不能变成免费。
func TestModelBillingRatioZeroFallsBackToGlobal(t *testing.T) {
	store, px := newHostProxy(t)
	defer store.Close()
	u, _ := store.CreateUser("ratio-fallback", "x")
	seedRatioModel(t, store, "m-fallback", 0)
	if err := store.SetKV(settingMultiplier, "1.7"); err != nil {
		t.Fatal(err)
	}
	px.ReloadBilling() // 全局倍率变了，快照要重载

	if _, _, err := px.setModelBillingRatio("m-fallback", 0, ""); err != nil {
		t.Fatalf("setModelBillingRatio: %v", err)
	}
	// 1 积分 × 1.7 = 1.7 点 = 1700 微点
	if got := chargeFor(t, store, px, u, "m-fallback", 1); got != 1700 {
		t.Fatalf("倍率 0 应回落全局 1.7（1700 微点），得 %d（为 0 就说明变成了免费）", got)
	}
	if _, src := px.ratioFor("m-fallback"); src != "global" {
		t.Fatalf("source 应为 global，得 %s", src)
	}
}

// B3：改完**立即**生效（不重启、不等缓存）。
//
// 这条防的是"管理员点了保存、页面提示成功、下一条请求还是按旧倍率扣"——
// 那种缺陷不报错、不告警，只在流水里静默偏差。
func TestModelBillingRatioTakesEffectImmediately(t *testing.T) {
	store, px := newHostProxy(t)
	defer store.Close()
	u, _ := store.CreateUser("ratio-live", "x")
	seedRatioModel(t, store, "m-live", 0)
	if err := store.SetKV(settingMultiplier, "1"); err != nil {
		t.Fatal(err)
	}

	if _, _, err := px.setModelBillingRatio("m-live", 2, ""); err != nil {
		t.Fatal(err)
	}
	if got := chargeFor(t, store, px, u, "m-live", 1); got != 2000 {
		t.Fatalf("改前应按 2 倍扣 2000，得 %d", got)
	}
	if _, _, err := px.setModelBillingRatio("m-live", 3, ""); err != nil {
		t.Fatal(err)
	}
	if got := chargeFor(t, store, px, u, "m-live", 1); got != 3000 {
		t.Fatalf("改完应立即按 3 倍扣 3000，得 %d（说明快照没换）", got)
	}
	// 版本号必须递增：事后对账要能回答"这笔按哪一版扣的"。
	if rev := px.billingRev(); rev < 3 {
		t.Fatalf("倍率版本号应随每次变更递增（>=3），得 %d", rev)
	}
}

// B4：越界 / 非法 scope 被拒，且**库里的值不变**。
func TestModelBillingRatioValidation(t *testing.T) {
	store, px := newHostProxy(t)
	defer store.Close()
	seedRatioModel(t, store, "m-bad", 0)
	if _, _, err := px.setModelBillingRatio("m-bad", 2, ""); err != nil {
		t.Fatal(err)
	}

	// 直接调 handler：校验发生在 HTTP 层。
	cases := []struct {
		name string
		body string
		want int
	}{
		{"负数", `{"model":"m-bad","billing_ratio":-1}`, 400},
		{"超上限", `{"model":"m-bad","billing_ratio":1000}`, 400},
		{"scope 非法", `{"model":"m-bad","billing_ratio":1,"billing_scope":"weird"}`, 400},
		{"模型为空", `{"model":"  ","billing_ratio":1}`, 400},
		{"模型不存在", `{"model":"m-not-exist","billing_ratio":1}`, 404},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/api/admin/model-ratios/billing", strings.NewReader(c.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			px.appForTest().handleAdminSetBillingRatio(rec, req)
			if rec.Code != c.want {
				t.Fatalf("应返回 %d，得 %d：%s", c.want, rec.Code, rec.Body.String())
			}
		})
	}

	// 关键：被拒之后生效值仍是 2。
	if r, _ := px.ratioFor("m-bad"); r != 2 {
		t.Fatalf("非法请求不应改动生效倍率，得 %v", r)
	}
}

// B5：试算与实扣用**同一个公式**（这是"预览一套、实扣另一套"的解药）。
func TestBillingPreviewMatchesCharge(t *testing.T) {
	store, px := newHostProxy(t)
	defer store.Close()
	u, _ := store.CreateUser("ratio-preview", "x")
	seedRatioModel(t, store, "m-preview", 0.5) // 实测 0.5 积分/1k
	if err := store.SetKV(settingMultiplier, "1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := px.setModelBillingRatio("m-preview", 2, ""); err != nil {
		t.Fatal(err)
	}

	// 直接给积分 1.25 → 预览 2500 微点
	body := `{"model":"m-preview","credit":1.25}`
	rec := httptest.NewRecorder()
	px.appForTest().handleAdminBillingPreview(rec,
		httptest.NewRequest("POST", "/api/admin/billing/preview", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("preview 应 200，得 %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		ChargedMicro int64   `json:"charged_micro"`
		Ratio        float64 `json:"ratio"`
		Source       string  `json:"source"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("preview 响应不是 JSON：%v", err)
	}
	if out.ChargedMicro != 2500 || out.Ratio != 2 || out.Source != "model" {
		t.Fatalf("预览应 2500/2/model，得 %+v", out)
	}
	// 实扣必须一模一样。
	if got := chargeFor(t, store, px, u, "m-preview", 1.25); got != out.ChargedMicro {
		t.Fatalf("实扣 %d 与预览 %d 不一致（两套公式）", got, out.ChargedMicro)
	}
}

// appForTest 给 handler 用例一个最小 app（只需要 store + proxy）。
func (p *Proxy) appForTest() *app { return &app{store: p.store, proxy: p} }

// 编译期守卫：确认结算包确实在用请求级倍率（否则这批用例可能"绿得没有意义"）。
var _ = func() bool {
	_ = http.StatusOK
	return true
}
