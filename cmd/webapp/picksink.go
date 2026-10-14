// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// picksink.go 上游 5xx 时的「降档重试」（R6，v0.26.1）。
//
// ## 语义
//
// 上游 5xx 说明**这一档整体不可用**（不是某一个号坏了 —— 单个号坏了有熔断器管）。
// 于是把该档**整层划掉**再试下一档，而不是在同一层里继续换号白撞。
//
// ## ⚠️ 为什么默认关闭：本仓的「档」是成本层，不是优先级层
//
// 同类项目（multi2api 对标 new-api）的档是**优先级档**，下沉不花钱。本仓的档是
// **成本层**（0 已实测免费 / 1 无观测 / 2 已实测收费），"下沉"= **去用更贵的账号**。
// 也就是说：这个开关打开后，网关会在上游抽风时**主动多花站长的钱**把请求救回来。
//
// 这是经营决定，不是技术决定 —— 所以默认 off，由站长自己权衡：
//
//	off（默认）—— 上游 5xx 时行为与之前完全一致：同层换号 + 熔断器处置。
//	on         —— 5xx 时把当前成本层整层加入 tried，本次请求下沉到下一层继续。
//
// ## 实现为什么这么轻
//
// 不改选号内核（见 internal/pool/tier.go 的理由）：pick 本来就只在**最优可用层**内选，
// 只要把当前层划掉，它自然落到下一层。tried 是请求级状态，由请求循环持有 ——
// 划层动作放在那儿语义最正。
package main

import (
	"log"
	"strings"
)

// kvPickSinkOn5xx 设置键：on / off（默认 off）。见文件头。
const kvPickSinkOn5xx = "pick_sink_on_5xx"

// pickSinkOn5xx 是否开启「5xx 降档重试」（默认关闭）。
func (p *Proxy) pickSinkOn5xx() bool {
	if p == nil || p.store == nil {
		return false
	}
	v, _ := p.store.GetKV(kvPickSinkOn5xx)
	return strings.EqualFold(strings.TrimSpace(v), "on")
}

// sinkCostTier 把 `uid` 所在的**整个成本层**加入本次请求的 tried 集合。
//
// 返回值 = 本次新划掉的账号数（0 表示没划：未开启 / 查不到层 / 该层只有它自己之外的
// 情况都算 0）。调用方只用它做日志。
//
// 边界：
//   - uid 不在池里（刚被摘除）→ 不动作；
//   - tried 为 nil → 不动作（调用方没在做轮换，划了也没用）；
//   - 该层包含 uid 自己 → 一起划掉（它已经失败过，本来也在 tried 里）。
func (p *Proxy) sinkCostTier(uid, model string, tried map[string]bool) int {
	if tried == nil || !p.pickSinkOn5xx() {
		return 0
	}
	tier, ok := p.pool.CostTierOf(uid, model)
	if !ok {
		return 0
	}
	// 已经到底层（2 = 已实测收费）就没什么可降的了。
	if tier >= 2 {
		return 0
	}
	n := 0
	for _, peer := range p.pool.UIDsInCostTier(model, tier) {
		if !tried[peer] {
			tried[peer] = true
			n++
		}
	}
	if n > 0 {
		// 记一行：这是**花钱**的决定，必须在日志里看得见（否则事后没人说得清
		// "为什么那段时间贵号被大量使用"）。
		log.Printf("WARN: [picksink] 上游 5xx → 本次请求下沉一层：model=%s 划掉成本层 tier=%d 的 %d 个账号"+
			"（原因账号 %s）", model, tier, n, uid)
	}
	return n
}
