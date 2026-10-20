// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 单模型**计费**倍率（v0.31.67，需求「按实际倍率调整计费规则」）。
//
// # 为什么要单独成文件
//
// 此前扣费只有一个数：全局 `pricing_multiplier`，所有模型一视同仁。
// 但成本从来不是一视同仁的：同样 1000 token，有的模型是别人的十倍价。
// 一刀切的倍率必然出现"贵的模型平台倒贴、便宜的模型用户被多收"。
//
// 现在：**每个模型可以有自己的倍率**，`0` 表示"不参与"（回落全局）——
//
//	chargedMicro = PointToMicro(consumeCredit × ratio(model))
//
// # 三条纪律（钱的规则）
//
//  1. **唯一口径**：谁也别自己乘。所有入口（结算 / 试算 / 后台展示）都调
//     `ratioFor(model)`，一处算错就是全局算错，好查。
//  2. **快照**：倍率在内存里是一份不可变快照，改配置时在**同一把锁**内
//     reload → 换指针（`ReloadBilling`）。绝不出现"读到一半被改了"。
//  3. **改完立刻生效**：不重启、不等缓存过期。后台 POST 返回成功时，
//     下一条请求就必须按新倍率扣 —— 否则"改完不生效"会让人反复点保存。
package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/billing"
)

const (
	// kvBillingRatioRev 倍率快照版本号（每次 ReloadBilling 自增）。
	//
	// 为什么需要它：结算发生在请求末尾，可能与"管理员改倍率"并发。
	// 有了版本号，事后对账能回答"这笔是按哪一版扣的"，而不是只能猜。
	kvBillingRatioRev = "billing_ratio_rev"

	// billingRatioMax 单模型倍率上限。挡住"手滑打成 1000"这类输入 ——
	// 那不是定价，是事故（一条请求能把用户余额扣成负数几十次）。
	billingRatioMax = 100.0
)

// billingSnapshot 一次加载得到的**不可变**倍率视图。
type billingSnapshot struct {
	// model 模型 → 倍率（只放 >0 的：0 = 不参与，放进来反而会让判断变复杂）。
	model map[string]float64
	// rev 本快照的版本号。
	rev int64
	// at 加载时刻（排障用）。
	at time.Time
}

// ratioFor 返回某模型生效的倍率，以及它来自哪里（"model" / "global"）。
//
// 调用点应当只有结算与试算两处；返回值里的 source 是给后台与日志看的，
// 让人能解释"为什么这一笔是 2.5 倍"。
func (p *Proxy) ratioFor(model string) (ratio float64, source string) {
	p.billMu.RLock()
	snap := p.billView
	p.billMu.RUnlock()
	if snap != nil {
		if r, ok := snap.model[model]; ok && r > 0 {
			return r, "model"
		}
	}
	// 全局倍率**每次实时读**（不自快照）：它只有一个 kv 键，读一次很便宜；
	// 更重要的是"改全局倍率要不要 reload"这种坑不能再犯一次 —— 实时读天然即时生效。
	// 模型级倍率则相反：它是一张表，且改它有明确的写入口（setModelBillingRatio → ReloadBilling），
	// 用快照才能保证"一笔请求里只读一版"。
	return p.store.PricingMultiplier(), "global"
}

// ReloadBilling 重新加载倍率快照并原子替换（v0.31.67）。
//
// 锁内只做"读库 + 构造新快照"，写完指针就返回 —— 不做任何网络/写库动作，
// 避免把管理员的一次保存拖成几秒。
func (p *Proxy) ReloadBilling() {
	list, err := p.store.ListModelRatios()
	if err != nil {
		log.Printf("WARN: [billing] 加载模型倍率失败，沿用旧快照：%v", err)
		return
	}
	snap := &billingSnapshot{
		model: make(map[string]float64, len(list)),
		at:    time.Now(),
	}
	for _, r := range list {
		if r.BillingRatio > 0 && r.BillingScope != "relay" {
			// 只把"号池/未标注"的倍率放进 pool 视图；relay 的倍率属于第三方通道
			// （P2 接入），现在放进来会让号池的同一模型被按通道价收费。
			snap.model[r.Model] = r.BillingRatio
		}
	}
	p.billMu.Lock()
	if p.billView != nil {
		snap.rev = p.billView.rev + 1
	} else {
		snap.rev = 1
	}
	p.billView = snap
	p.billMu.Unlock()
	_ = p.store.SetKV(kvBillingRatioRev, strconv.FormatInt(snap.rev, 10))
}

// billingRev 当前快照版本（对账/日志用）。
func (p *Proxy) billingRev() int64 {
	p.billMu.RLock()
	defer p.billMu.RUnlock()
	if p.billView == nil {
		return 0
	}
	return p.billView.rev
}

// setModelBillingRatio 写入单模型倍率并**立即生效**。
//
// 返回 (旧倍率, 新倍率, 是否真的变了)。只改库与快照，不碰历史流水 ——
// 已经扣过的钱不能被追溯修改，否则对账永远对不上。
func (p *Proxy) setModelBillingRatio(model string, ratio float64, scope string) (old float64, changed bool, err error) {
	var prev float64
	if err := p.store.db.QueryRow(
		`SELECT COALESCE(billing_ratio,0) FROM model_ratios WHERE model = ?`, model).Scan(&prev); err != nil {
		return 0, false, err
	}
	changed = math.Abs(prev-ratio) > 1e-9
	if _, err := p.store.db.Exec(
		`UPDATE model_ratios SET billing_ratio = ?, billing_scope = ?, updated_at = ?
		 WHERE model = ?`, ratio, scope, time.Now().Unix(), model); err != nil {
		return prev, false, err
	}
	p.ReloadBilling()
	return prev, changed, nil
}

// 说明：billMu / billView 是 Proxy 的字段（不是包级变量）——
// 一个进程一个 Proxy，但测试会构造多个，包级变量会让它们互相污染。

// handleAdminSetBillingRatio 设置**单模型**计费倍率（v0.31.67）。
//
//	POST /api/admin/model-ratios/billing
//	{ "model": "xxx", "billing_ratio": 2.5, "billing_scope": "" | "pool" | "relay" }
//
// 语义：
//   - ratio = 0 → 该模型**不参与**模型级倍率，回落全局（不是"免费"）；
//   - scope = relay → 先存下来但**不参与号池扣费**（等 P2 的通道计费接上）。
//
// 返回里带 effective_ratio / source / rev：让人当场看到"生效值是多少、来自哪一版"，
// 不必再去猜"到底改没改成功"。
func (a *app) handleAdminSetBillingRatio(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model        string  `json:"model"`
		BillingRatio float64 `json:"billing_ratio"`
		BillingScope string  `json:"billing_scope"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "model 不能为空"})
		return
	}
	// 倍率上下界：0 = 不参与，>0 才有意义。上限挡住"手滑打成 1000"。
	if req.BillingRatio < 0 || req.BillingRatio > billingRatioMax {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "billing_ratio 必须在 0" + "–" + strconv.FormatFloat(billingRatioMax, 'f', 0, 64) + " 之间（0 = 回落全局）",
			"code":  "bad_ratio",
		})
		return
	}
	scope := strings.TrimSpace(req.BillingScope)
	switch scope {
	case "", "pool", "relay":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "billing_scope 只能是 空 / pool / relay",
			"code":  "bad_scope",
		})
		return
	}
	old, changed, err := a.proxy.setModelBillingRatio(model, req.BillingRatio, scope)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]any{
				"error": "模型不在倍率目录里（先同步一次官方倍率）", "code": "model_not_found",
			})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	eff, src := a.proxy.ratioFor(model)
	log.Printf("INFO: [billing] 模型倍率 %s: %v -> %v（scope=%q，生效 %v 来自 %s，rev=%d）",
		model, old, req.BillingRatio, scope, eff, src, a.proxy.billingRev())
	writeJSON(w, http.StatusOK, map[string]any{
		"model": model, "old_ratio": old, "billing_ratio": req.BillingRatio,
		"billing_scope": scope, "changed": changed,
		"effective_ratio": eff, "source": src, "rev": a.proxy.billingRev(),
	})
}

// handleAdminBillingPreview 计费试算（v0.31.67）——**改之前先看效果**。
//
//	POST /api/admin/billing/preview
//	{ "model": "xxx", "credit": 1.5 }        # 直接给号池积分（与结算同口径）
//	{ "model": "xxx", "tokens": 2000 }       # 或给 token 数，用实测单价估算积分
//
// 为什么要有它：把倍率从 1 调到 3，对用户意味着什么？没有试算就只能"改了再看流水"，
// 而流水是既成事实（钱已经扣了）。这里用**与结算完全相同的函数**算，避免"预览一套、实扣一套"。
func (a *app) handleAdminBillingPreview(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model  string  `json:"model"`
		Credit float64 `json:"credit"`
		Tokens int     `json:"tokens"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "model 不能为空"})
		return
	}
	ratio, source := a.proxy.ratioFor(model)
	credit := req.Credit
	estimated := false
	if credit <= 0 && req.Tokens > 0 {
		var per1k float64
		if err := a.store.db.QueryRow(
			`SELECT COALESCE(measured_cost_1k,0) FROM model_ratios WHERE model = ?`, model).Scan(&per1k); err == nil && per1k > 0 {
			credit = per1k * float64(req.Tokens) / 1000
			estimated = true
		}
	}
	micro := billing.PointToMicro(credit * ratio)
	writeJSON(w, http.StatusOK, map[string]any{
		"model": model, "ratio": ratio, "source": source,
		"credit": credit, "credit_estimated": estimated,
		"charged_micro": micro, "charged_points": billing.MicroToPoint(micro),
		"note": "这里用的公式与结算完全相同（PointToMicro(积分 × 倍率)）",
	})
}
