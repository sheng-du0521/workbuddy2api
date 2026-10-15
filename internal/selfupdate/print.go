// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// `-check-update` 的**人类可读输出**（纯格式化，无副作用除 stdout）。
//
// 为什么放这里：它是"清单状态的呈现"而不是"应用逻辑" —— 与 CLI 入口分离后，
// 任何命令（含未来的 CLI 工具）都能复用同一份呈现口径，不会出现两套文案。
package selfupdate

import (
	"fmt"
	"time"
)

// ---------------------------------------------------------------- CLI

// PrintStatus -check-update 的输出。
//
// 输出刻意是**人类可读的多行文本**而不是 JSON：它的用途是"运维跑一下看一眼"。
// 需要机器可读的话直接请求 /api/admin/update-status（那里是 JSON）。
func PrintStatus(st Status) {
	fmt.Printf("当前版本: %s\n", st.Current)
	if !st.Enabled {
		fmt.Println("版本检查: 已关闭（设置 → 关于 / 版本更新）")
		return
	}
	if st.ManifestURL == "" {
		fmt.Println("更新源:   未配置（设置 → 关于 / 版本更新 可填清单地址）")
		return
	}
	fmt.Printf("更新源:   %s\n", st.ManifestURL)
	if st.ManifestUse != "" && st.ManifestUse != st.ManifestURL {
		fmt.Printf("实际命中: %s\n", st.ManifestUse)
	}
	if st.Error != "" {
		fmt.Printf("检查结果: 失败 —— %s\n", st.Error)
		return
	}
	fmt.Printf("最新版本: %s\n", st.Latest)
	if st.HasUpdate {
		fmt.Printf("状态:     有新版本可用（%s 级别）\n", st.Severity)
		if st.ReleaseURL != "" {
			fmt.Printf("发布页:   %s\n", st.ReleaseURL)
		}
		if st.Asset != nil && st.Asset.URL != "" {
			fmt.Printf("下载:     %s\n", st.Asset.URL)
			if st.Asset.SHA256 != "" {
				fmt.Printf("SHA-256:  %s\n", st.Asset.SHA256)
			}
		}
		if st.Notes != "" {
			fmt.Printf("更新说明: %s\n", st.Notes)
		}
		if st.Skipped == st.Latest {
			fmt.Println("注意:     该版本此前已被标记为「跳过」")
		}
	} else {
		fmt.Println("状态:     已是最新")
	}
	if st.TooOld {
		fmt.Printf("警告:     当前版本低于最低支持版本 %s，建议尽快升级\n", st.MinSupport)
	}
	if st.CheckedAt > 0 {
		fmt.Printf("检查时间: %s\n", time.Unix(st.CheckedAt, 0).Format("2006-01-02 15:04:05"))
	}
}

// ExitCode -check-update 的退出码。
//
// 刻意分三档：**巡检脚本必须能区分「有新版本」和「根本没查成功」**。
// 把检查失败当成"已是最新"（都返回 0）是这类脚本最常见的坑 ——
// 结果是更新源挂了两周都没人发现。
func ExitCode(st Status) int {
	switch {
	case st.Error != "" || !st.Enabled || st.ManifestURL == "":
		return 2
	case st.HasUpdate:
		return 1
	default:
		return 0
	}
}
