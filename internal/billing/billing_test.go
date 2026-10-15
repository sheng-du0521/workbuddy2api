// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package billing

import (
	"math"
	"testing"
)

// ======================================================================
// 计费纯算术的独立测试（v0.31.10，P2-7 第三刀）
//
// 这一簇代码的特点是：**算错了不会崩，只会让账目静默偏移**。所以这里钉的不是
// "函数返回了什么"，而是几条**钱的规则**：
//
//	R1 微点记账一律 math.Round，负数也不例外（截断会每笔少扣 1 微点）；
//	R2 分成与返点**永不超过平台该次实收**（否则平台倒贴）；
//	R3 推广返点**永不侵占平台留成**（rebate ≤ charged − earn）；
//	R4 小额收益不能被舍入成 0（号主的上级会永远拿不到返点）；
//	R5 任何"用不上"的入参都返回 0，绝不返回 Inf/NaN（0 token 除零是真实入口）。
// ======================================================================

func TestPointToMicroRounding(t *testing.T) {
	cases := []struct {
		in   float64
		want int64
	}{
		{0, 0},
		{1, 1000},
		{0.001, 1},  // 1 微点
		{0.0005, 1}, // 半个微点：四舍五入到 1（不是 0）
		{0.0004, 0}, // 不到半个微点
		{2.5, 2500},
		{-0.0005, -1},    // R1：负数的中点必须远离零（+0.5 截断法会得到 0）
		{-1.2345, -1235}, // −1234.5 → −1235（远离零）；截断法会得到 −1234
		{-0.0004, 0},
	}
	for _, c := range cases {
		if got := PointToMicro(c.in); got != c.want {
			t.Errorf("PointToMicro(%v) = %d，期望 %d", c.in, got, c.want)
		}
	}
}

func TestMicroPointRoundTrip(t *testing.T) {
	for _, p := range []float64{0, 0.001, 1, 12.345, 1000, 99999.999} {
		m := PointToMicro(p)
		if back := MicroToPoint(m); math.Abs(back-p) > 0.0005 {
			t.Errorf("往返失真：%v → %d → %v", p, m, back)
		}
	}
}

func TestFormatPoints(t *testing.T) {
	cases := []struct {
		micro int64
		want  string
	}{
		{0, "0"},
		{1000, "1"},
		{1, "0.001"},
		{1500, "1.5"},
		{-1500, "-1.5"},
		{1234567, "1234.567"},
	}
	for _, c := range cases {
		if got := FormatPoints(c.micro); got != c.want {
			t.Errorf("FormatPoints(%d) = %q，期望 %q", c.micro, got, c.want)
		}
	}
}

func TestEstimatePromptTokens(t *testing.T) {
	// 空 payload / 无 messages → 0（宁可不估，也不编造）。
	if got := EstimatePromptTokens(map[string]any{}); got != 0 {
		t.Errorf("空 payload 应为 0，得 %d", got)
	}
	if got := EstimatePromptTokens(map[string]any{"messages": "not-an-array"}); got != 0 {
		t.Errorf("messages 非数组应为 0，得 %d", got)
	}

	// 每条消息 4 token 结构开销 + 内容 bytes/3 + 1。
	one := EstimatePromptTokens(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "abcd"}}, // 4 + (4/3+1)=2 → 6
	})
	if one != 6 {
		t.Errorf("单条短消息应为 6，得 %d", one)
	}

	// 多模态：text 部分按 bytes/3，图片按固定 1000 保守估。
	multi := EstimatePromptTokens(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "abc"},         // 4 + 2
			map[string]any{"type": "image_url", "image_url": "x"}, // +1000
		}}},
	})
	if multi != 1006 {
		t.Errorf("图文消息应为 1006，得 %d", multi)
	}

	// 非 map 元素与 nil 元素不能 panic（外部输入，形状不可信）。
	if got := EstimatePromptTokens(map[string]any{
		"messages": []any{"oops", nil, map[string]any{"content": []any{"bad", nil}}},
	}); got != 4 {
		t.Errorf("畸形消息应只留下 1 条结构开销（4），得 %d", got)
	}
}

func TestRequestMaxTokens(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
		want    int
	}{
		{"缺省", map[string]any{}, DefaultMaxTokens},
		{"max_tokens", map[string]any{"max_tokens": float64(512)}, 512},
		{"max_completion_tokens 优先", map[string]any{"max_completion_tokens": float64(128), "max_tokens": float64(512)}, 128},
		{"字符串形式不认（不猜）", map[string]any{"max_tokens": "512"}, DefaultMaxTokens},
		{"零与负数按未声明", map[string]any{"max_tokens": float64(0)}, DefaultMaxTokens},
		{"负数按未声明", map[string]any{"max_tokens": float64(-5)}, DefaultMaxTokens},
		{"天文数字压到天花板", map[string]any{"max_tokens": float64(1 << 30)}, 1 << 20},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := RequestMaxTokens(c.payload); got != c.want {
				t.Errorf("RequestMaxTokens = %d，期望 %d", got, c.want)
			}
		})
	}
}

func TestReserveMicro(t *testing.T) {
	cases := []struct {
		name   string
		per1k  float64
		tokens int
		cap    int64
		want   int64
	}{
		{"无单价不预扣", 0, 1000, 0, 0},
		{"负单价不预扣", -1, 1000, 0, 0},
		{"无 token 不预扣", 5, 0, 0, 0},
		// 5 点/1k × 1000 token = 5 点 = 5000 微点
		{"基本折算", 5, 1000, 0, 5000},
		// 极小额被抬到下限 1 微点（估成 0 等于预扣形同虚设）
		{"低于下限抬到 1", 0.0001, 1, 0, MinReserveMicro},
		// 上限生效
		{"触顶", 5, 1000000, 3000, 3000},
		// cap<=0 = 不限
		{"cap 为 0 不限", 5, 1000, 0, 5000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ReserveMicro(c.per1k, c.tokens, c.cap); got != c.want {
				t.Errorf("ReserveMicro = %d，期望 %d", got, c.want)
			}
		})
	}
}

// R5：0 token 与 0 积分绝不能产生 Inf/NaN。
func TestPer1kCostDivZeroGuard(t *testing.T) {
	if got := Per1kCost(1, 0); got != 0 {
		t.Errorf("tokens=0 应为 0（不是 Inf），得 %v", got)
	}
	if got := Per1kCost(0, 100); got != 0 {
		t.Errorf("credit=0 应为 0，得 %v", got)
	}
	if got := Per1kCost(-1, 100); got != 0 {
		t.Errorf("credit<0 应为 0，得 %v", got)
	}
	if got := Per1kCost(2, 1000); got != 2 {
		t.Errorf("2 积分 / 1000 token 应为 2.0（积分每 1k），得 %v", got)
	}
	if got := Per1kCost(3, 500); got != 6 {
		t.Errorf("3 积分 / 500 token 应为 6.0，得 %v", got)
	}
}

func TestMaxKeptCost(t *testing.T) {
	// 首次观测：只要 > 0 就写入
	if v, upd := MaxKeptCost(0, false, 3); !upd || v != 3 {
		t.Errorf("首次观测应写入 3，得 %v upd=%v", v, upd)
	}
	// 更大 → 更新（单调不减）
	if v, upd := MaxKeptCost(3, true, 5); !upd || v != 5 {
		t.Errorf("更大应更新为 5，得 %v upd=%v", v, upd)
	}
	// 更小 → 不更新（平均会被低价历史拉低，预扣宁高勿低）
	if v, upd := MaxKeptCost(5, true, 2); upd || v != 5 {
		t.Errorf("更小不应更新，得 %v upd=%v", v, upd)
	}
	// 相等 → 不更新
	if _, upd := MaxKeptCost(5, true, 5); upd {
		t.Error("相等不应更新")
	}
	// 无效观测（<=0）→ 不更新
	if v, upd := MaxKeptCost(5, true, 0); upd || v != 5 {
		t.Errorf("无效观测不应更新，得 %v upd=%v", v, upd)
	}
}

func TestEstimateCredit(t *testing.T) {
	if got := EstimateCredit(2, 1000); got != 2 {
		t.Errorf("应得 2，得 %v", got)
	}
	if got := EstimateCredit(2, 500); got != 1 {
		t.Errorf("应得 1，得 %v", got)
	}
	if got := EstimateCredit(0, 1000); got != 0 {
		t.Errorf("无单价应得 0，得 %v", got)
	}
	if got := EstimateCredit(2, 0); got != 0 {
		t.Errorf("无 token 应得 0，得 %v", got)
	}
}

func TestHostEarn(t *testing.T) {
	cases := []struct {
		name    string
		credit  float64
		ratio   float64
		charged int64
		want    int64
	}{
		// 10 积分 × 0.6 = 6 点 = 6000 微点，实收 20000 微点 → 不封顶
		{"基本", 10, 0.6, 20000, 6000},
		// R2：实收只有 5000 微点 → 封顶到 5000（否则平台倒贴）
		{"实收封顶", 10, 0.6, 5000, 5000},
		{"零比例", 10, 0, 20000, 0},
		{"负比例", 10, -1, 20000, 0},
		{"零消耗", 0, 0.6, 20000, 0},
		// R4：0.01 积分 × 0.6 = 0.006 点 = 6 微点（不能被舍成 0）
		{"小额不被舍成 0", 0.01, 0.6, 20000, 6},
		// 实收为 0（免费通道）→ 封顶成 0 → 返回 0（调用方跳过写流水）
		{"实收为 0", 10, 0.6, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := HostEarn(c.credit, c.ratio, c.charged)
			if got != c.want {
				t.Errorf("HostEarn(%v, %v, %d) = %d，期望 %d", c.credit, c.ratio, c.charged, got, c.want)
			}
			if c.charged > 0 && got > c.charged {
				t.Errorf("R2 违反：分成 %d > 平台实收 %d", got, c.charged)
			}
		})
	}
}

func TestReferralRebate(t *testing.T) {
	cases := []struct {
		name    string
		earn    int64
		ratio   float64
		charged int64
		want    int64
	}{
		// earn 6000、比例 0.1 → 600；实收 20000 → 留成 14000，不触钳位
		{"基本", 6000, 0.1, 20000, 600},
		// R3：实收 6500、earn 6000 → 留成仅 500 → 返点钳到 500
		{"不侵占平台留成", 6000, 0.1, 6500, 500},
		// 留成为负（平台本就不赚钱）→ 返点必须为 0，不许"负返点"去追债
		{"留成为负则不返", 6000, 0.1, 5000, 0},
		{"零收益", 0, 0.1, 20000, 0},
		{"零比例", 6000, 0, 20000, 0},
		// R4：earn=1 × 0.1 = 0.1 → round 成 0 → 返回 0（不写零流水）
		{"过小舍入为 0 则跳过", 1, 0.1, 20000, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ReferralRebate(c.earn, c.ratio, c.charged)
			if got != c.want {
				t.Errorf("ReferralRebate(%d, %v, %d) = %d，期望 %d", c.earn, c.ratio, c.charged, got, c.want)
			}
			if got > 0 && got > c.charged-c.earn {
				t.Errorf("R3 违反：返点 %d > 平台留成 %d", got, c.charged-c.earn)
			}
		})
	}
}

// TestMoneyInvariantsGrid 在参数网格上验证 R2/R3 两条**不变量**（这是本包存在的理由：
// 单个用例能钉住具体数值，但钉不住"任意参数下都不倒贴"）。
//
// 不变量：
//
//	① earn ≤ charged            —— 分成不吃到平台本金
//	② earn + rebate ≤ charged   —— 分成 + 返点一起吃也不超过实收（平台留成 ≥ 0）
func TestMoneyInvariantsGrid(t *testing.T) {
	credits := []float64{0.0001, 0.01, 0.1, 1, 3.7, 10, 100.5, 9999}
	ratios := []float64{0.05, 0.1, 0.3, 0.5, 0.6, 0.9, 1.5}
	for _, credit := range credits {
		for _, mult := range []float64{0.01, 0.1, 0.5, 1, 2, 10} {
			charged := PointToMicro(credit * mult)
			for _, hr := range ratios {
				earn := HostEarn(credit, hr, charged)
				if earn < 0 || earn > charged {
					t.Fatalf("①违反 credit=%v mult=%v ratio=%v：earn=%d charged=%d", credit, mult, hr, earn, charged)
				}
				for _, rr := range ratios {
					rebate := ReferralRebate(earn, rr, charged)
					if rebate < 0 {
						t.Fatalf("返点为负：earn=%d ratio=%v → %d", earn, rr, rebate)
					}
					if earn+rebate > charged {
						t.Fatalf("②违反 credit=%v mult=%v hr=%v rr=%v：earn=%d rebate=%d charged=%d",
							credit, mult, hr, rr, earn, rebate, charged)
					}
				}
			}
		}
	}
}

// TestPlatformNeverLosesOnEveryDayPrices 锁定运维须知的那条推论：
// 「要让平台每次都留成，必须保证计费倍率 > 代挂分成比例」。
// 换言之：倍率 > 分成比例时，留成必然 > 0（且此时返点也拿不走它）。
func TestPlatformNeverLosesOnEveryDayPrices(t *testing.T) {
	for _, credit := range []float64{0.5, 1, 7, 42, 1000} {
		for _, hr := range []float64{0.3, 0.6} {
			mult := hr + 0.001 // 略微高于分成比例
			charged := PointToMicro(credit * mult)
			earn := HostEarn(credit, hr, charged)
			margin := charged - earn
			// 留成必须为正（倍率 > 分成比例时平台不可能亏）
			if margin <= 0 {
				t.Fatalf("倍率 %v > 分成 %v，留成却为 %d（credit=%v charged=%d earn=%d）",
					mult, hr, margin, credit, charged, earn)
			}
			// 返点再怎么给也不能吃穿留成
			rebate := ReferralRebate(earn, 0.9, charged)
			if rebate > margin {
				t.Fatalf("返点 %d 吃穿了平台留成 %d", rebate, margin)
			}
		}
	}
}
