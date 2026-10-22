// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// tray.go 托盘的**平台无关**契约（放在无 build tag 的文件里，
// 这样 tray_other.go 与 tray_windows.go 共用同一套签名，
// 不会出现"Windows 编译得过、Linux 编译不过"的割裂）。
//
// 实现：
//   - Windows：tray_windows.go（通知区域图标 + 右键菜单）
//   - 其他：tray_other.go（空实现）
package main

// trayOptions 托盘的启动参数。
//
// onExit 是**唯一的退出通道**：它必须触发 main 里那条优雅停机路径（与 SIGTERM
// 同源），而不是 os.Exit —— 后者会跳过所有 defer（凭证回写 / 池状态落盘 /
// 定时任务停止），留下一个"看起来关了、其实状态没存"的烂摊子。
type trayOptions struct {
	addr    string // 监听地址，用于「打开控制台」与菜单上的展示
	version string // 版本号，用于「关于」
	dataDir string // 数据目录，用于「打开数据目录」
	onExit  func() // 退出回调；必须触发优雅停机
}
