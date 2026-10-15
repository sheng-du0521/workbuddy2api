// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import "net/http"

// handleUserModels 会话鉴权的模型列表（供控制台「测试对话」的模型下拉）。
//
// 与 /v1/models 的区别：不消耗用户 Key 的限流桶、也不要求 Key 可用，
// 直接复用同一份上游探测缓存（5 分钟）。
//
// 为什么单独一个文件：它此前与「用量账单三类拆分」同住 insights.go，
// 但两者只是**碰巧**都在控制台概览里出现 —— 本 handler 属**模型缓存**域
// （依赖 modelsCacheFresh / cachedModelIDs / refreshModelsCache），
// 而账单域依赖的是 usage/host_earnings 只读聚合。v0.31.13 把账单域迁到
// internal/api/report 时，按"依赖什么就归到哪"把本 handler 留在了 cmd/webapp。
func (a *app) handleUserModels(w http.ResponseWriter, r *http.Request) {
	// 用自带读锁的 modelsCacheFresh：此前这里直接读 modelsCache.at/payload，
	// 完全没有加锁（同样的数据竞争，2026-10-14 随 -race 一并修），
	// 而且硬编码 5*time.Minute 会与 modelsCacheTTL 悄悄脱节。
	if !modelsCacheFresh() {
		// 触发一次刷新（与 /v1/models 共用缓存与探测逻辑）。
		a.refreshModelsCache()
	}
	// modelsCache 只存 OpenAI 形状的 payload，这里解出 id 列表返回轻量形状。
	ids := cachedModelIDs()
	writeJSON(w, http.StatusOK, map[string]any{"models": ids})
}
