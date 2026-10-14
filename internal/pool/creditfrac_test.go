// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package pool

import (
	"testing"

	"workbuddy2api/internal/auth"
)

// creditsOf 读池内余额（测试用；生产侧走 Status）。
func creditsOf(t *testing.T, p *Pool, uid string) int64 {
	t.Helper()
	for _, st := range p.List() {
		if st.UID == uid {
			return st.Credits
		}
	}
	t.Fatalf("账号 %s 不在池内", uid)
	return 0
}

// TestCreditDeductionRoundingUnchanged 单次四舍五入口径不能被小数累积改掉。
//
// 既有语义：credit=2.5 → 扣 3（+0.5 四舍五入）。引入余数累积后这条必须仍然成立，
// 否则所有依赖它的既有测试与「签到外回写」的估计口径都会漂移。
func TestCreditDeductionRoundingUnchanged(t *testing.T) {
	p := New("")
	defer p.Close()
	p.Add(&auth.Auth{UID: "r1"})
	p.SetCredits("r1", 100)

	p.NoteModelCost("r1", "m", 2.5, 1000)
	if got := creditsOf(t, p, "r1"); got != 97 {
		t.Fatalf("2.5 应四舍五入扣 3：100 → %d", got)
	}
	// 第二次 2.5：余数 -0.5 抵回 → want=2.0 → 扣 2（两次合计正好 5，不丢不重）。
	p.NoteModelCost("r1", "m", 2.5, 1000)
	if got := creditsOf(t, p, "r1"); got != 95 {
		t.Fatalf("两次 2.5 合计应扣 5：97 → %d", got)
	}
}

// TestCreditFractionAccumulates 不足 0.5 的消耗必须能累积扣掉。
//
// 这是「号池仪表盘积分合计纹丝不动」的直接原因之一：credits 是整数，
// 每次都对单次 credit 四舍五入 → 便宜模型（实测 0.019 积分/1k token）
// 每次消耗 0.19 都被舍成 0，小数被逐次丢弃，余额**永远不会动**。
// 余数留存后累计过 0.5 就应扣 1。
func TestCreditFractionAccumulates(t *testing.T) {
	p := New("")
	defer p.Close()
	p.Add(&auth.Auth{UID: "f1"})
	p.SetCredits("f1", 100)

	// 三次 0.19 = 0.57：前两次扣 0，第三次累计到 0.57 → 扣 1。
	for i := 0; i < 2; i++ {
		p.NoteModelCost("f1", "m", 0.19, 1000)
		if got := creditsOf(t, p, "f1"); got != 100 {
			t.Fatalf("第 %d 次 0.19 不应扣（未过 0.5 阈值）：得 %d", i+1, got)
		}
	}
	p.NoteModelCost("f1", "m", 0.19, 1000)
	if got := creditsOf(t, p, "f1"); got != 99 {
		t.Fatalf("累计 0.57 应扣 1：得 %d（余数未留存则恒为 100）", got)
	}
}

// TestCreditFractionClearedOnAuthoritativeWrite 签到写入权威余额时必须清掉余数。
//
// 不清的后果：余数会跨签到存活，与权威余额之间永远差一个陈旧小数，
// 且该差值随每次签到不断累积。
//
// 判别用例的构造要点：两次消耗必须跨过四舍五入阈值（0.4 + 0.3 = 0.7 ≥ 0.5），
// 而单独 0.3 不过阈值（d = int64(0.3+0.5) = 0）。于是：
//
//	· 余数已清 → 只按 0.3 算 → 不扣
//	· 余数残留 → 按 0.7 算 → 多扣 1
func TestCreditFractionClearedOnAuthoritativeWrite(t *testing.T) {
	p := New("")
	defer p.Close()
	p.Add(&auth.Auth{UID: "f2"})

	p.SetCredits("f2", 100)
	p.NoteModelCost("f2", "m", 0.4, 1000) // 单独 0.4 不过阈值：不扣，余数 0.4
	if got := creditsOf(t, p, "f2"); got != 100 {
		t.Fatalf("0.4 不应扣（低于 0.5 阈值）：得 %d", got)
	}

	// 权威余额覆盖（签到时走 SetCreditsDetailed）→ 余数清零。
	p.SetCreditsDetailed("f2", 100, 0)

	p.NoteModelCost("f2", "m", 0.3, 1000)
	if got := creditsOf(t, p, "f2"); got != 100 {
		t.Fatalf("权威写入后余数应清零（0.4 不该累加进来）：0.3 单独不过阈值，得 %d", got)
	}
	// 清零不影响正常累积能力：再来 0.3 → 累计 0.6 → 扣 1。
	p.NoteModelCost("f2", "m", 0.3, 1000)
	if got := creditsOf(t, p, "f2"); got != 99 {
		t.Fatalf("两次 0.3 累计 0.6 应扣 1：得 %d", got)
	}
}

// TestCreditDeductionNeverGoesNegative 扣穿时必须钳 0，且余数归零。
func TestCreditDeductionNeverGoesNegative(t *testing.T) {
	p := New("")
	defer p.Close()
	p.Add(&auth.Auth{UID: "z1"})
	p.SetCredits("z1", 3)

	p.NoteModelCost("z1", "m", 100, 1000) // 远超余额
	if got := creditsOf(t, p, "z1"); got != 0 {
		t.Fatalf("应钳到 0，得 %d", got)
	}
	// 余数已归零：下次小额消耗不该被陈旧负数余数抵消。
	p.NoteModelCost("z1", "m", 0.6, 1000)
	if got := creditsOf(t, p, "z1"); got != 0 {
		t.Fatalf("余额为 0 时不应变成负数或凭空回正，得 %d", got)
	}
}
