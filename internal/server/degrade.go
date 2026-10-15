// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package server

import (
	"time"

	"workbuddy2api/internal/prompt"
)

// degradeGate 降级状态机 —— **实现已下沉到 internal/prompt**（v0.31.4）。
//
// 为什么下沉：主产品（cmd/webapp）此前完全没有这条能力，而「合并两个产品」的前提是
// 能力等值；状态机本身与"用哪段降级提示词"是同一件事的两半，放在 prompt 包里
// 两个入口共用一份实现。这里保留别名，是为了让旧网关的调用点与既有测试零改动
// （旧网关按计划在合并第二步删除）。
//
// 语义见 prompt.Gate 的注释：passthrough 请求被上游内容策略拦截（400 审核文案 /
// 11128 渠道拒绝）时，换 Degraded 中性提示词重试一次，并降级到次日 00:00 CST 重置。
type degradeGate = prompt.Gate

// nextMidnightCST 旧名保留（既有单测按这个名字写）。
func nextMidnightCST(now time.Time) time.Time { return prompt.NextMidnightCST(now) }
