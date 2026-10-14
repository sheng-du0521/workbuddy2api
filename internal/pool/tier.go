// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// tier.go 成本层的**只读查询**（R6，v0.26.1）。
//
// ## 干什么用
//
// R6「retry 降档」的语义是：上游 5xx 说明**这一档整体不可用**，下一次尝试应当换一档。
// 同类项目（multi2api 对标 new-api）的做法是在 pick 内部加 `tierOffset` 参数。
//
// 本仓**没有**动 pick 的内核，理由有两条：
//
//  1. pick 是选号唯一的入口，内部已经有「成本层硬过滤 + Top5 短名单 + 防撞号窗口 +
//     条件探索」四层判定。为了一个**默认关闭**的策略去改它的入参签名，
//     收益与风险不成比例。
//  2. 语义上也不需要：pick 本来就会**只在最优可用层内选**。只要把当前这一层的账号
//     加进请求级的 `tried` 集合，pick 就会**自然**落到下一层 —— 效果与改内核等价，
//     而 tryed 的维护本来就是上层（请求循环）的职责。
//
// 所以本文件只提供两个只读查询，供上层做"把整层划掉"这个动作。
//
// ## ⚠️ 为什么这个策略默认关闭
//
// 本仓的「档」是**成本层**（0 已实测免费 / 1 无观测 / 2 已实测收费），不是优先级层。
// 因此"下沉一档"= **去用更贵的账号**。这在"宁可失败"与"宁可花钱"之间选了后者，
// 是**站长的经营决定**，不该由程序替他默认做（同类项目那次改动没这个问题，
// 因为它的档是优先级档，下沉不花钱）。
package pool

import "time"

// CostTierOf 该账号在指定模型上的成本层。第二个返回值 false = 池里没有这个账号。
//
// 语义与选号内的 costTier 完全一致（复用 preview.go 的 previewTier，避免两处口径漂移）：
//
//	0 = 已实测免费（限免期 / 夜间免费的号，最强偏好）
//	1 = 无观测（含观测过期）—— 新号的限免状态只能靠实测发现，故排在"已收费"之前
//	2 = 已实测收费
func (p *Pool) CostTierOf(uid, model string) (int, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return 0, false
	}
	t, _ := p.previewTier(e, model, time.Now())
	return t, true
}

// UIDsInCostTier 当前池中该模型上处于指定成本层的账号 UID（跳过 disabled / manualDisabled）。
//
// 跳过两个停用位是刻意的：它们本来就不可能被选中，把它们塞进 tried 只会让
// "本次请求已排除 N 个账号"这个数字虚高，掩盖真实情况。
func (p *Pool) UIDsInCostTier(model string, tier int) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	var out []string
	for uid, e := range p.byUID {
		if e.disabled || e.manualDisabled {
			continue
		}
		if t, _ := p.previewTier(e, model, now); t == tier {
			out = append(out, uid)
		}
	}
	return out
}
