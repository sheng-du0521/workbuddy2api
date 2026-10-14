//go:build !windows && !unix

// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// term_other.go 其余平台（js/wasm、plan9 等）：既无 ANSI 控制台语义，也无
// 统一的窗口尺寸查询手段，故恒返回"未知"。
//
// 不知尺寸时渲染方按 -width 显式值或默认值排版，watch 也会因尺寸不可知而
// 保持单次输出，不会去尝试原地覆盖（那在尺寸未知时才真的会错位）。
// solaris/aix 虽支持 ANSI 但同样拿不到尺寸，另见 term_ansi_nosize.go。
package main

// enableVT 在无 ANSI 语义的平台恒返回 false：调用方据此回落为逐帧滚动输出，
// 而非发送无人解释的转义序列。
func enableVT() bool { return false }

// termSize 在无窗口尺寸语义的平台恒返回 0, 0（表示"未知"）。
func termSize() (cols, rows int) { return 0, 0 }
