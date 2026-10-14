// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"net/http"
	"strconv"
	"time"
)

// ======================================================================
// 用户侧「用量账单 · 三类拆分」与模型列表（v0.10.0）
//
// 控制台的用量账单按数据来源拆成三类：
//   1. self_use  自己代挂使用：用「代挂自用 Key」走自己的代挂账号，免费但真实消耗
//                （度量 = 请求数 / tokens / 上游 credit，扣费恒为 0）。
//   2. pool_use  使用号池：用「平台计费 Key」走平台号池，按用量扣余额。
//   3. pool_earn 进入号池：自己的代挂账号在池中承接平台流量，平台按分成折算为号主余额。
//                前两者来自 usage_logs（按 scope 拆分），第三者来自 host_earnings。
// ======================================================================

// breakdownDays 解析并钳制 days 参数（缺省 14，上限 90）。
func breakdownDays(r *http.Request) int {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 {
		days = 14
	}
	if days > 90 {
		days = 90
	}
	return days
}

// handleUsageBreakdown 本人用量账单的三类拆分。
func (a *app) handleUsageBreakdown(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	days := breakdownDays(r)

	resp := map[string]any{"days": days}
	resp["self_use"] = a.scopeBlock(u.ID, days, ScopeHost)
	resp["pool_use"] = a.scopeBlock(u.ID, days, ScopePool)
	resp["pool_earn"] = a.hostEarnBlock(u.ID, days)
	writeJSON(w, http.StatusOK, resp)
}

// scopeBlock 组装「消费侧」一类（请求数 / token / 扣费 / 上游 credit + 按日 + 按模型）。
func (a *app) scopeBlock(userID int64, days int, scope string) map[string]any {
	daily, _ := a.store.UsageDailyScoped(userID, days, scope)
	models, _ := a.store.UsageByModelScoped(userID, days, scope)

	var reqs, tokens, charged int64
	var credit float64
	dailyItems := make([]map[string]any, 0, len(daily))
	for _, d := range daily {
		reqs += d.Requests
		tokens += d.Tokens
		charged += d.ChargedMicro
		credit += d.UpstreamCredit
		dailyItems = append(dailyItems, map[string]any{
			"day": d.Day, "requests": d.Requests, "tokens": d.Tokens,
			"charged": MicroToPoint(d.ChargedMicro), "upstream_credit": d.UpstreamCredit,
		})
	}
	modelItems := make([]map[string]any, 0, len(models))
	for _, m := range models {
		modelItems = append(modelItems, map[string]any{
			"model": m.Model, "requests": m.Requests, "tokens": m.Tokens,
			"charged": MicroToPoint(m.ChargedMicro), "upstream_credit": m.UpstreamCredit,
		})
	}
	return map[string]any{
		"requests":        reqs,
		"tokens":          tokens,
		"charged":         MicroToPoint(charged),
		"upstream_credit": credit,
		"daily":           dailyItems,
		"models":          modelItems,
	}
}

// hostEarnBlock 组装「进入号池」一类：自己的代挂账号服务了多少平台流量、赚了多少返点。
func (a *app) hostEarnBlock(userID int64, days int) map[string]any {
	daily, _ := a.store.HostEarningsDaily(userID, days)
	models, _ := a.store.HostEarningsByModel(userID, days)

	var reqs, charged, earned int64
	dailyItems := make([]map[string]any, 0, len(daily))
	for _, d := range daily {
		reqs += d.Requests
		charged += d.ChargedMicro
		earned += d.EarnedMicro
		dailyItems = append(dailyItems, map[string]any{
			"day": d.Day, "requests": d.Requests,
			"charged": MicroToPoint(d.ChargedMicro), "earned": MicroToPoint(d.EarnedMicro),
		})
	}
	modelItems := make([]map[string]any, 0, len(models))
	for _, m := range models {
		modelItems = append(modelItems, map[string]any{
			"model": m.Model, "requests": m.Requests,
			"charged": MicroToPoint(m.ChargedMicro), "earned": MicroToPoint(m.EarnedMicro),
		})
	}
	totalMicro, count, _ := a.store.HostEarningSummary(userID)
	return map[string]any{
		"requests": reqs,
		"charged":  MicroToPoint(charged),
		"earned":   MicroToPoint(earned),
		"total":    MicroToPoint(totalMicro),
		"count":    count,
		"daily":    dailyItems,
		"models":   modelItems,
	}
}

// handleUserModels 会话鉴权的模型列表（供控制台「测试对话」的模型下拉）。
//
// 与 /v1/models 的区别：不消耗用户 Key 的限流桶、也不要求 Key 可用，
// 直接复用同一份上游探测缓存（5 分钟）。
func (a *app) handleUserModels(w http.ResponseWriter, r *http.Request) {
	if time.Since(modelsCache.at) >= 5*time.Minute || len(modelsCache.payload) == 0 {
		// 触发一次刷新（与 /v1/models 共用缓存与探测逻辑）。
		a.refreshModelsCache()
	}
	// modelsCache 只存 OpenAI 形状的 payload，这里解出 id 列表返回轻量形状。
	ids := cachedModelIDs()
	writeJSON(w, http.StatusOK, map[string]any{"models": ids})
}
