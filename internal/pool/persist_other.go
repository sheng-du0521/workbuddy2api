//go:build !unix

// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// persist_other.go 非 Unix 平台（Windows 等）回落：无 POSIX uid/gid 语义，
// 恒返回 0（仅影响首错日志的属主信息可读性）。
// syscall.Stat_t 在 Windows 不存在，本文件与 persist_unix.go 由构建标签互斥。
package pool

import (
	"os"
)

// statUID 非 Unix 平台恒 0。
func statUID(info os.FileInfo) int { return 0 }

// statGID 非 Unix 平台恒 0。
func statGID(info os.FileInfo) int { return 0 }
