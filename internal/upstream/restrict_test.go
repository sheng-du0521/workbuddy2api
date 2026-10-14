// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package upstream

import "testing"

// TestIsAccountRestricted 账号级限制信号的识别。
//
// 这个谓词决定**是否立即删除账号**，所以正例要覆盖实测原文，
// 反例更要紧 —— 把「指向这次请求」的措辞误收进来，就等于给可恢复的抖动
// 配上了不可逆处置（2026-10-13 误杀 30 个号的成因）。
func TestIsAccountRestricted(t *testing.T) {
	// 正例：2026-10-13 生产日志里实测到的原文（zh / en 各自单独出现都要命中）。
	pos := []string{
		"账号存在异常行为，已被限制使用",
		"您的账号存在异常行为，已被限制使用，请联系客服",
		"Your account has been restricted due to unusual activity",
		"account has been restricted due to unusual activity, contact support",
		"账号已被限制",
		"Account is restricted.",
		"account suspended",
		// 传整段 body 也要能命中（调用方偶尔只有 body）。
		`{"code":11140,"msg":"request illegal","displayMsg":{"en":"Your account has been restricted due to unusual activity","zh":"账号存在异常行为，已被限制使用"}}`,
	}
	// 反例：这些**都指向「这次请求」**，绝不能被判成账号级限制。
	neg := []string{
		"",
		"   ",
		"request illegal",
		`{"code":11140,"msg":"request illegal","requestId":"6776962f9a209c70b64166894412681a"}`,
		"rate limit exceeded",
		"too many requests",
		"insufficient credit",
		"session expired",
		"the request parameters were rejected by the model provider",
		"Invalid request parameters",
	}
	for _, s := range pos {
		if !IsAccountRestricted(s) {
			t.Errorf("应判为账号级限制但未命中：%q", s)
		}
	}
	for _, s := range neg {
		if IsAccountRestricted(s) {
			t.Errorf("**不该**判为账号级限制（会带来不可逆处置）：%q", s)
		}
	}
	if AccountRestrictedHint() == "" {
		t.Error("统一文案不该为空")
	}
}
