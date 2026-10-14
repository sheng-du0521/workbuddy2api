// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"os"
	"strings"

	"workbuddy2api/internal/upstream"
)

// ======================================================================
// 设备风控凭证（X-Device-Token）的来源与装配（v0.25.1）
// ======================================================================
//
// 背景（一次审计发现「全池 0/27 非空 —— 一个号都没有设备凭证」）：
//
// `internal/upstream` 的注入实现是完整的（`headers.go` 的 injectDeviceToken，
// 优先级 auth.DeviceToken > Client.DeviceToken > DeviceTokenFile），
// `authFromAccount` 也把账号的 device_token 正确传进去了 —— 但**没有任何数据流进来**：
//
//	① 账号的 device_token 全为空；
//	② 导入桌面端导出文件时 `desktopAccount` **根本没有 device_token 字段**，
//	   所以文件里即使有也会被静默丢弃（主因）；
//	③ **webapp 从未装配 Client.DeviceToken / DeviceTokenFile** ——
//	   cmd/server（旧网关）装了，webapp 漏了，于是连"全局兜底"这条退路也不存在。
//
// 结果就是该头**从来没被注入过**，而上游看到的是一台「没有设备凭证的客户端」。
//
// 本文件的职责只有两件事：把 ③ 补上、把「这个账号到底有没有设备凭证」做成**可观测**的
// （②在 import_desktop.go 里修）。
//
// ## 为什么值得修（而不是"逆向没证据就算了"）
//
// X-Device-Token 是本仓库 README §1.3 **唯一有逆向证据**的风控头（其余 X-Machine-ID /
// X-Session-ID 只是"行业对齐"）。官方客户端恒带它；一个长跑的中转站**从来不带**，
// 这本身就是稳定的客户端差异 —— 而"稳定差异"正是可用于聚类的特征。
//
// ## 来源优先级（与 upstream 内部实现逐字一致，勿在这里另立口径）
//
//	账号级 device_token  >  全局值（env WB2API_DEVICE_TOKEN > kv device_token）
//	                     >  文件（env WB2API_DEVICE_TOKEN_FILE > kv device_token_file）
//
// 刻意**不**把全局值写进日志、也不回显到接口（它是风控凭证，等同敏感串）。
const (
	kvDeviceToken     = "device_token"
	kvDeviceTokenFile = "device_token_file"
	// envDeviceToken / envDeviceTokenFile 部署级覆盖（优先级高于 kv）：
	// 容器化部署里把凭证放环境变量比落库更常见（也不进备份文件）。
	envDeviceToken     = "WB2API_DEVICE_TOKEN"
	envDeviceTokenFile = "WB2API_DEVICE_TOKEN_FILE"
)

// globalDeviceToken 全局兜底设备凭证（env > kv）。返回空串表示未配置。
//
// 挂在 Store 上而不是 app 上：装配点是 NewProxy（那里只有 store），
// 而视图侧要用的三态判断也不需要别的依赖 —— 少一层包装少一处漂移。
func (s *Store) globalDeviceToken() string {
	if v := strings.TrimSpace(os.Getenv(envDeviceToken)); v != "" {
		return v
	}
	v, _ := s.GetKV(kvDeviceToken)
	return strings.TrimSpace(v)
}

// deviceTokenFilePath 全局设备凭证文件路径（env > kv）。返回空串表示未配置。
func (s *Store) deviceTokenFilePath() string {
	if v := strings.TrimSpace(os.Getenv(envDeviceTokenFile)); v != "" {
		return v
	}
	v, _ := s.GetKV(kvDeviceTokenFile)
	return strings.TrimSpace(v)
}

// deviceTokenFileHasContent 文件是否真的能提供凭证。
//
// 为什么这里自己读一次文件（而不是复用 upstream 的 readDeviceTokenFile）：
// 只做「非空判断」这一件事，而 upstream 那个函数是包内私有、且带 5 分钟缓存。
// ⚠️ 它只用于**展示判断**，不参与出站取值 —— 出站永远由 upstream 自己的
// resolveDeviceToken 决定，两者不会因为这里的判断而分叉（最坏是展示写得保守一点）。
func deviceTokenFileHasContent(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	st, err := os.Stat(path)
	if err != nil || st.IsDir() || st.Size() == 0 {
		return false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(b)) != ""
}

// installDeviceToken 把设备凭证的全局兜底装配到出站客户端上。
//
// 在 NewProxy 里对 `uc` 调用一次即可。**必须在 upstream.New() 之后** ——
// 字面量构造会丢掉默认值（同 ChatBaseCN 那个坑）。
func (s *Store) installDeviceToken(uc *upstream.Client) {
	if s == nil || uc == nil {
		return
	}
	uc.DeviceToken = s.globalDeviceToken()
	uc.DeviceTokenFile = s.deviceTokenFilePath()
}

// deviceTokenState 该账号出站时 X-Device-Token 的来源（三态）。
//
//	"account" —— 账号自带：一号一凭证，隔离性最好
//	"global"  —— 靠全局兜底值/文件：**多号共用同一个设备凭证**，隔离性弱（但仍优于不带）
//	"none"    —— 都没有：该头不会被注入，上游看到的是一台「无设备凭证的客户端」
//
// 为什么要做成显式三态而不是一个 bool：审计时的结论天差地别 ——
// "全池都没带"（可聚类）与"全池共用一个"（也可聚类，但形态不同）需要的处置完全不同。
func (a *app) deviceTokenState(acc *Account) string {
	if acc != nil && strings.TrimSpace(acc.DeviceToken) != "" {
		return "account"
	}
	if a.store != nil && (a.store.globalDeviceToken() != "" ||
		deviceTokenFileHasContent(a.store.deviceTokenFilePath())) {
		return "global"
	}
	return "none"
}
