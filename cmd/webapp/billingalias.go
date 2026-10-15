// 计费纯算术下沉到 internal/billing（v0.31.10，P2-7 拆包第三刀）的**过渡别名**。
//
// 与 updatecheck.go / usagealias.go 的别名块同一用途：让本包既有的 **250+ 调用点与测试
// 一行不改**（`PointToMicro` 92 处、`MicroToPoint` 163 处、`MicroPerPoint` 多处），
// 本刀才能做到\"行为零变化\"，由既有测试全绿证明。
//
// ⚠️ 两类别名的安全边界（第一刀踩过这个坑）：
//   - **函数别名（var f = pkg.F）安全**：函数值没有\"测试改本地那份\"的注入语义；
//   - **变量别名危险**（值拷贝），所以本文件里**没有**任何数据变量别名；
//   - **常量别名（const X = pkg.X）安全**，用于 MicroPerPoint / defaultMaxTokens。
//
// 迁移路线（见 docs/P2-7-拆包方案.md）：本块是过渡。下一步把调用点逐个迁到 `billing.X`
// 的正式 API，然后删掉本文件 —— 那时改名/收敛才有意义（现在改只会放大这一刀的面积）。
package main

import "workbuddy2api/internal/billing"

const (
	MicroPerPoint = billing.MicroPerPoint

	// defaultMaxTokens 仍被既有测试直接引用（billing_test.go）；minReserveMicro 已无
	// 本包调用点（预扣下限的判定随 ReserveMicro 一起进了新包），故不再别名。
	defaultMaxTokens = billing.DefaultMaxTokens
)

var (
	// 单位换算：微点记账的三件套。全项目 250+ 处引用都走这里。
	PointToMicro = billing.PointToMicro
	MicroToPoint = billing.MicroToPoint
	formatPoints = billing.FormatPoints

	// 用量估算（预扣的上界口径）。
	estimatePromptTokens = billing.EstimatePromptTokens
	requestMaxTokens     = billing.RequestMaxTokens
)
