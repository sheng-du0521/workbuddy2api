// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// restrict.go 上游「账号级风控限制」信号的识别（v0.24.6）。
//
// 为什么要单列一个信号，而不只看错误码：
//
//	11140 (`request illegal`) 是一个**笼统**的码 —— 出口 IP 抖动、请求频率、
//	请求形态不合规都会触发它，这类是**可恢复**的（2026-10-13 实测：一次并发出站
//	就在一个窗口内把 30 个健康号打成 11140，单独重探即可通过）。按它做不可逆处置
//	会一次误杀一批号。
//
//	但同一批响应里，上游还会给一个**语义完全不同**的 displayMsg：
//	    {"en":"Your account has been restricted due to unusual activity",
//	     "zh":"账号存在异常行为，已被限制使用"}
//	这句话是**上游对账号本身的判定**（不是对这次请求），且经实测证实是
//	**账号级、换出口 IP 不解除**（3 个不同出口 + 显式直连的对照实验全部仍被拦）。
//
// 结论：**按 displayMsg 区分处置强度** —— 裸 11140 走「累计 + 软冷却」，
// 带账号级限制文案的 11140 走「即时上报 + 立即摘除」。判据是上游自己的原话，
// 不是我们的猜测（本函数只认措辞，不猜动机）。

package upstream

import "strings"

// restrictedPatterns 上游「账号级限制」文案家族（大小写不敏感）。
//
// 收录原则：只收**指向账号本身**的措辞。像 "request illegal" / "rate limit" /
// "too many requests" 这类指向**这次请求**的措辞**一律不收** —— 收了就会把
// 可恢复的抖动判成账号终态（那正是 2026-10-13 误杀 30 个号的成因）。
var restrictedPatterns = []string{
	// 实测到的原文（zh / en 两条一起给，取其一即可命中）
	"账号存在异常行为",
	"已被限制使用",
	"account has been restricted",
	"restricted due to unusual activity",
	// 同一家族的其他常见措辞（保守收录，均为"账号受限"语义）
	"账号已被限制",
	"账号被限制使用",
	"account is restricted",
	"account has been suspended",
	"account suspended",
	"unusual activity on your account",
}

// IsAccountRestricted 判断一段上游文案（通常是 displayMsg，也可传原始 body）
// 是否表达了「账号级限制」。
//
// 传 displayMsg 最准；传整段 body 也能命中（措辞包在 JSON 里同样是子串），
// 但**优先传 displayMsg** —— body 里可能有 requestId 之类的无关文本，
// 虽然当前不会误命中，语义上 displayMsg 才是权威来源。
//
// 空串返回 false（不编造）。
func IsAccountRestricted(text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	lower := strings.ToLower(text)
	for _, p := range restrictedPatterns {
		if strings.Contains(lower, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

// AccountRestrictedHint 账号级限制的统一说明（日志 / 上报 / 界面共用一份文案，
// 避免三处各写一遍、逐渐分叉）。
func AccountRestrictedHint() string {
	return "上游判定账号存在异常行为并已限制使用（账号级，换出口不解除）"
}
