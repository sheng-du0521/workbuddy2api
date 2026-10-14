// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// egress_policy.go 出口线路失效时的处置策略（R1，v0.26.1）。
//
// ## 「不静默换出口」为什么是一条强约束
//
// 本仓在处理"线路坏掉"时已经做对了一半：**绝不把账号改写成直连**
// （宁可请求失败，也不从本机 IP 裸奔，见 egress_ops.go 的 rehomeXxx 长注释）。
//
// 但另一半此前是"自动迁移"：坏线路上的账号会被**自动**搬到另一条线路继续服务。
// 从用户视角这是"自动恢复"，很好；**从上游视角这是"这个账号的出口 IP 变了"** ——
// 而出口 IP 是上游可用于聚类的稳定特征之一。同类项目把这条写成了硬约束
// （workbuddy-manager 2026-10-12 的原话：「**出口被换在风控眼里就是特征变化**」，
// 因此"绑定的线路被删/停用/地址非法时**明确报错，不静默换出口**"）。
//
// 两种策略各有代价，所以做成**显式配置**而不是替用户选：
//
//	strict（默认）—— 不换出口。账号留在原线路（判 egressBad 排除出选号 → 不裸奔、
//	                 但也不服务），等人决定补线路还是改绑定。代价：可用性下降。
//	migrate        —— 自动搬到最空闲的可用线路（旧行为）。代价：出口变了，风控上
//	                 多一次"特征变化"；换来的是不中断。
//
// 为什么默认 strict：本仓一贯的优先级是「**宁可失败，不可裸奔/不可留下可疑特征**」
// （同 rehome 里"失败可以接受，裸奔不可以"的口径）。而且**默认值的作用是让人做决定**：
// strict 下线路一挂就立刻在面板上喊人，比"悄悄搬走了、没人知道出口变过"更符合本仓取向。
// 想回到旧行为只需把设置项改成 migrate。
package main

import "strings"

// kvEgressOnLineDown 设置键：线路失效时是否允许自动迁移账号。
const kvEgressOnLineDown = "egress_on_line_down"

// 取值（稳定的字符串，前后端与持久化共用）。
const (
	egressOnLineDownStrict  = "strict"  // 不静默换出口（默认）
	egressOnLineDownMigrate = "migrate" // 自动迁移到可用线路
)

// egressOnLineDownMode 读取当前策略；未配置或值非法一律回落 strict。
//
// 为什么非法值回落 strict 而不是 migrate：两个方向的错判代价不对称 ——
// 误判成 strict 顶多是"没自动搬、喊人处理"（可恢复）；误判成 migrate 会**在
// 无人知晓的情况下改变账号出口**（不可撤回的特征变化）。保守方向永远是可逆的那一侧。
func (a *app) egressOnLineDownMode() string {
	if a.store == nil {
		return egressOnLineDownStrict
	}
	v, _ := a.store.GetKV(kvEgressOnLineDown)
	switch strings.ToLower(strings.TrimSpace(v)) {
	case egressOnLineDownMigrate:
		return egressOnLineDownMigrate
	case egressOnLineDownStrict, "":
		return egressOnLineDownStrict
	default:
		return egressOnLineDownStrict
	}
}

// egressAllowAutoRehome 是否允许自动迁移（rehomeAccountsFromDeadProxies 的前置判断）。
func (a *app) egressAllowAutoRehome() bool {
	return a.egressOnLineDownMode() == egressOnLineDownMigrate
}

// normaliseEgressOnLineDown 校验外部写入的值。
func normaliseEgressOnLineDown(v string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case egressOnLineDownStrict:
		return egressOnLineDownStrict, true
	case egressOnLineDownMigrate:
		return egressOnLineDownMigrate, true
	default:
		return "", false
	}
}
