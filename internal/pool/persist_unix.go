//go:build unix

// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// persist_unix.go 平台相关：从 os.FileInfo 取属主 uid/gid（Unix）。
// Sys() 返回 nil（测试 fake）时回落 0，仅影响日志可读性。
// 非 Unix（如 Windows）由 persist_other.go 提供恒 0 回落实现。
package pool

import (
	"os"
	"syscall"
)

// statUID 返回文件属主 uid；取不到（非 Unix / 测试 fake）返回 0。
func statUID(info os.FileInfo) int {
	if s, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(s.Uid)
	}
	return 0
}

// statGID 返回文件属主 gid；取不到返回 0。
func statGID(info os.FileInfo) int {
	if s, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(s.Gid)
	}
	return 0
}
