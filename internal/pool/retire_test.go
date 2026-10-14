// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package pool

import (
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// TestRetireAccountGate 摘除闸门：只有"允许摘除"的错误类型能真的把账号停用。
//
// 这是本轮最重要的不变量 —— 2026-10-13 的三十号误杀，成因不是某个分支写错，
// 而是同一个错误码在不同链路上两套纪律。把闸门钉在池层之后，任何链路想摘号都必须
// 先回答"这个错误够不够格"，答不上来就只允许可逆处置。
func TestRetireAccountGate(t *testing.T) {
	// 允许摘除：会话失效 / 账号级授权配额故障。
	allowed := []struct {
		name string
		kind upstream.ErrKind
	}{
		{"12153 会话失效", upstream.ErrSessionDead},
		{"11140/14017 账号级授权配额故障", upstream.ErrAccountFault},
	}
	for _, c := range allowed {
		p := New("")
		p.Add(&auth.Auth{UID: "u1"})
		if !p.RetireAccount("u1", c.kind, "test-"+c.name) {
			t.Fatalf("%s 应允许摘除", c.name)
		}
		st, _ := p.Status("u1")
		if !st.Disabled {
			t.Fatalf("%s 应已停用: %+v", c.name, st)
		}
		if st.DisabledReason != "test-"+c.name {
			t.Errorf("%s 停用原因应原样写入，得 %q", c.name, st.DisabledReason)
		}
	}

	// 拒绝摘除：这些**指向这次请求或上游**，不指向账号终态。
	denied := []struct {
		name string
		kind upstream.ErrKind
	}{
		{"429 限流", upstream.ErrSoftRate},
		{"WAF 403", upstream.ErrWafBlock},
		{"5xx 上游故障", upstream.ErrServer},
		{"404 偶发", upstream.ErrNotFound},
		{"余额耗尽", upstream.ErrHardCredit},
		{"模型不存在", upstream.ErrModelBlocked},
		{"内容拦截", upstream.ErrContentBlocked},
		{"参数错误", upstream.ErrBadParams},
		{"上下文超限", upstream.ErrPromptTooLong},
		{"图片无效", upstream.ErrImageInvalid},
		{"未分类 4xx", upstream.ErrClient},
		{"成功", upstream.ErrNone},
	}
	for _, c := range denied {
		p := New("")
		p.Add(&auth.Auth{UID: "u1"})
		if p.RetireAccount("u1", c.kind, "should-not-happen") {
			t.Fatalf("%s **不该**允许摘除", c.name)
		}
		st, _ := p.Status("u1")
		if st.Disabled {
			t.Fatalf("%s 不该把账号停用: %+v", c.name, st)
		}
		if st.DisabledReason != "" {
			t.Errorf("%s 不该写入停用原因，得 %q", c.name, st.DisabledReason)
		}
	}

	// 不存在的 uid：闸门先判资格，合格才落到 Disable（Disable 对不存在 uid 是 no-op）。
	p := New("")
	if !p.RetireAccount("ghost", upstream.ErrAccountFault, "x") {
		t.Fatal("资格判定与存在性无关，应返回 true")
	}
}
