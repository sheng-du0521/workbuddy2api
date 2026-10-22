//go:build !windows

// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// tray_other.go 非 Windows 的托盘空实现。
//
// Linux/macOS 上的常驻进程本就由 systemd / launchd 托管（`systemctl stop`、
// `launchctl unload` 才是那里的"右键退出"），没有"右下角"这个位置。
// Windows 实现见 tray_windows.go。
//
// 这里刻意**不打日志**：一键部署脚本在两类系统上跑同一条命令，
// 每启动一次就多一句"本平台不支持托盘"，只会淹没真正有用的启动信息。
package main

// startTray 非 Windows 下无动作（保持与 tray_windows.go 相同的签名）。
func startTray(opts trayOptions) {}

// stopTray 非 Windows 下无动作。
func stopTray() {}
