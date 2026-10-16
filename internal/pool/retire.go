// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// retire.go 「把账号从池子里摘掉」的唯一闸门（v0.24.7）。
//
// ## 为什么需要闸门
//
// 2026-10-13 的三十号误杀，成因**不是某个分支写错了**，而是**同一个错误码在不同链路上
// 有两套纪律**：健康检测侧早已写明「一次 12153 可能是网络抖动 / refresh 竞态，一次就杀号
// 会误杀」并走累计阈值，而生产转发链路、旧网关、乃至旧网关**同一个文件里的 refresh 路径
// 与 applyErrorPolicy**，都是"一次即摘"。哪条链路先被改，就只有那条对 —— 这种漂移会
// 持续制造同类事故。
//
// 所以把"能不能摘"做成**池层的一道门**：所有摘除都必须经过它，先过
// upstream.MayRetireAccount 的资格判定；不合格的类型一律不摘，只记日志并返回 false，
// 调用方应退化为**可逆**处置（软冷却 / 计数）。
//
// ## 为什么放在 pool 层
//
// 两个可执行产品（`cmd/webapp` 与旧网关 `internal/server`）共用 `internal/pool`，
// 放在这里两个产品才有**同一道门**；放在任一产品内都会立刻重现"只有那条链路是对的"。

package pool

import (
	"log"

	"workbuddy2api/internal/upstream"
)

// RetireAccount 摘除账号（停用）的唯一入口。
//
// kind 传该错误的上游分类（upstream.ErrKind）。返回 true = 已停用；
// false = 未摘（该类型不足以作为摘除依据，已记 WARN 日志）。
//
// ⚠️ 已知暗道（**两条**，不止一条）：达阈值时自身就会 `disableLocked`、不经过这道门的计数原语是
//
//	`NoteAccountFault`（11140 计数，state.go）
//	`NoteSessionDead`（12153 计数，state.go —— 2026-10-16 审查补记，此前只记了前者）
//
// 也就是说调用它们的前提必须是"这个错误类型允许摘除"。当前两个 kind
// （ErrAccountFault / ErrSessionDead）都满足 `MayRetireAccount`，故暂无可观测漂移；
// **但暗道客观存在**：将来若给别的错误类型接这两个计数原语，要么先过
// `upstream.MayRetireAccount`，要么用不自动 disable 的计数变体 —— 否则会出现"绕过闸门"。
//
// 调用方约定：拿到 false 时**必须**退化为可逆处置。不要绕过本方法直接调 Disable ——
// 那样这道门就白设了（`Disable` 仍保留给非错误驱动的场景：运维手工停用、账号级
// 明示限制等由上层语义保证的场合）。
func (p *Pool) RetireAccount(uid string, kind upstream.ErrKind, reason string) bool {
	if !upstream.MayRetireAccount(kind) {
		log.Printf("WARN: [retire] 拒绝摘除 %s：%s 不足以作为摘除依据（%s）",
			uid, kind, upstream.RetireEligibleHint())
		return false
	}
	p.Disable(uid, reason)
	return true
}
