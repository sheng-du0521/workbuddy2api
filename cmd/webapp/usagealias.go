// 用量统计下沉到 internal/usage（v0.31.9，P2-7 拆包第二刀）的**过渡别名**。
//
// 与 updatecheck.go 的别名块同一用途：让本包既有的调用点与测试**一行不改**。
// 与第一刀一样，**变量别名只能用于函数**（值拷贝对函数无副作用）；如果将来这些查询
// 需要测试注入点（比如注入一个假的 db 包装），必须直接用 `usage.Xxx` 而不是别名。
//
// 迁移路线（见 docs/P2-7-拆包方案.md）：本块是过渡，后续把调用点逐个迁到
// `usage.Queries` 的正式 API（直接持有 Queries 而不是靠 Store 提升），然后删掉别名。
package main

import "workbuddy2api/internal/usage"

type (
	UsageLog         = usage.UsageLog
	AccountUsageStat = usage.AccountUsageStat
	UsageUserStat    = usage.UsageUserStat
	UsageDailyStat   = usage.UsageDailyStat
	UsageModelStat   = usage.UsageModelStat
	UsagePerf        = usage.UsagePerf
)

// 本地日界 helper：口径与"按本地自然日聚合"强绑定，故随查询一起下沉
// （docs: v0.26.7 那个"东八区下窗口起点落在今天 16:00（未来）"的真实缺陷就出在这里）。
var (
	localDayStart  = usage.LocalDayStart
	localDayOffset = usage.LocalDayOffset
	// percentile 随 UsagePerf 一起下沉；perf_test.go 仍按老名字直接调它（函数别名安全）。
	percentile = usage.Percentile
)
