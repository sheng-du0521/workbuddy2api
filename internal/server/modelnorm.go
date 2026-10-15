// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// modelnorm.go 旧网关的模型名归一化接线 + 流内模型级错误帧处置（v0.31.3）。
//
// ## 为什么旧网关也需要这两件事
//
// 此前旧网关的模型名处理只有 `resolveModel`（剥 realm 前缀，大小写敏感、不处理空白），
// 于是同一个 `GLM-5.3` 在主产品上已被折回规范名、在旧网关上依旧必然 404；更隐蔽的是
// (账号, 模型) 冷却键的**写法不同源** —— 主产品写下的 6004 冷却（键 `glm-5.3`）
// 在旧网关按 `GLM-5.3` 查不到，旧网关会把同模型请求继续发给已被限额的号。
//
// 同理，流内模型级错误帧（HTTP 200 + error 帧里的 6004/11102）此前只在主产品有处置：
// 旧网关把帧原样透传，池侧完全不知道，下一个同模型请求照样路由到它，再次撞同一个错。
//
// 算法与错误码判据都在共享包里（internal/modelname / internal/upstream），
// 这里只做**接线** —— 两个产品各写一份是 2026-10-13 三十号误杀那一类病的成因。
package server

import (
	"log"
	"time"

	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/modelname"
	"workbuddy2api/internal/upstream"
)

// knownModelNames 归一化用的已知模型清单（**全部零出站**）。
//
// 三源合并，与主产品同口径（主产品的第三源是 usage_logs 种子化，旧网关无 DB，
// 由内置兜底清单代替）：
//
//  1. /v1/models 的只读缓存快照（CN 侧，TTL 1h）—— 最全；
//  2. global 侧模型快照 —— 跨 realm 的写法同样要能归一；
//  3. internal/modelname 的内置兜底清单 —— 缓存冷/新部署时的最小集合。
//
// 为什么"绝不出站"是硬要求：这是 chat 热路径，而权威清单要一次上游往返。
// 拿不到就退化成只做去空白/剥前缀（前两层），归一化的正确性不受影响
// —— 少的只是"把大小写折回规范名"这层便利。
//
// 上限 256：清单来自上游目录与请求观测，属于外部输入，不该无界。
func (h *Handler) knownModelNames() []string {
	const maxKnown = 256
	out := make([]string, 0, 32)
	seen := map[string]struct{}{}
	add := func(m string) {
		if m == "" || len(out) >= maxKnown {
			return
		}
		if _, ok := seen[m]; ok {
			return
		}
		seen[m] = struct{}{}
		out = append(out, m)
	}
	for _, mi := range cachedModelsSnapshot() {
		add(mi.ID)
	}
	if h.cfg.Upstream != nil {
		for _, mi := range h.cfg.Upstream.GlobalModelInfosSnapshot() {
			add(mi.ID)
		}
	}
	for _, m := range modelname.FallbackModels {
		add(m)
	}
	return out
}

// normalizeRequestModel 把「已剥 realm 前缀」的裸模型名归一成上游认得的写法。
//
// 返回归一化后的裸名；known 由调用方传入（同一次请求内只算一次，避免重复合并清单）。
// 归一化说明只在真的改动时打一条 INFO —— 静默改写客户端的请求是坏事，
// 但也不该为每个正常请求写日志（与主产品同口径）。
func normalizeRequestModel(bare string, known []string) string {
	norm, how := modelname.Normalize(bare, known)
	if norm == bare {
		return bare
	}
	log.Printf("INFO: [server] 模型名归一化 %q → %q（%s）", bare, norm, how)
	return norm
}

// noteInStreamModelError 对「HTTP 200 + 流内**模型级**错误帧」补做 (账号, 模型) 冷却。
//
// 与 4xx 路径**共用同一套冷却原语**（CooldownSoftForModel / BlockModelBackoff），
// 因此"生效范围、时长、恢复逻辑"与 6004 的 4xx 形态完全一致，不存在两套口径。
//
// 刻意**只**做模型级那一件事，不跑 applyErrorPolicy 的全部分支：这条路径上响应
// 已经发出（或已读完整条流），换号重试早已无意义；账号级处罚（熔断/归档/停用）
// 需要更强的证据，不该由一段流内文本触发。
//
// 「哪些码算模型级」由 upstream.ModelLevelErrorCode 单点判定，两个产品同源。
func (h *Handler) noteInStreamModelError(uid, model, payload string) {
	if uid == "" || payload == "" {
		return
	}
	code := upstream.ErrorCode([]byte(payload))
	if !upstream.ModelLevelErrorCode(code) {
		// 非模型级错误（含解析不出的）：有自己的链路处置，流式热路径上不重复分类。
		return
	}
	switch code {
	case 6004:
		resetAt, src := upstream.ResolveResetAt(nil, []byte(payload), time.Now())
		h.cfg.Pool.CooldownSoftForModel(uid, h.cfg.SoftCooldown, resetAt, model,
			"6004 model rate limit (in-stream)"+upstream.ResetSourceSuffix(src))
	case 11102:
		h.cfg.Pool.BlockModelBackoff(uid, model, upstream.ModelBlockReason)
	}
	log.Printf("WARN: [server] 流内错误帧 acct=%s model=%s code=%d → 已按 (账号,模型) 写入冷却",
		logfmt.Label(uid, ""), model, code)
}
