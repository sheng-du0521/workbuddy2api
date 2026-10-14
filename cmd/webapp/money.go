// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

// money.go 统一「点 / 钱 / 号池积分」三套口径（v0.17.7）。
//
// ============================================================================
// 为什么需要这个文件：系统里同时存在三种数值，此前在界面上混着叫，极易误读
// ============================================================================
//
//	① 点（平台点数）—— 用户在平台内的计价单位。余额、消费、日/月限额、提现申请额
//	   都用它。落库为微点（×MicroPerPoint=1000）以避开浮点误差。
//	② 钱（金额，元）—— 「点 × 折算比例」。折算比例就是提现页那个「元 / 点」
//	   （kvWithdrawRate，默认 0.01，即 100 点 = 1 元）。**全站只有这一个比例**，
//	   不再另设第二套，否则同一笔钱在两处会显示成不同数字。
//	③ 号池积分（上游账号的 credits）—— 上游账号被消耗的积分量，是**另一种货币**，
//	   与「点」无固定换算关系（只通过「计费倍率」间接影响收费）。
//	   它只出现在账号维度（账号卡片、号池仪表盘），**绝不能出现在用户余额位置**。
//
// 展示规则（前端按此实现，改动前先读这里）：
//
//	点       → 数字 + 「点」，≤3 位小数，千分位            fmtPoints
//	钱       → 「¥」+ 固定 2 位小数，千分位                 fmtMoney
//	号池积分 → 数字 + 「号池积分」，≤2 位小数，千分位        fmtCredit
//
// 返点（号池返点 / 推广返点）**一律以「钱」呈现**（需求明确），点数只作为
// tooltip 里的换算说明出现，不做主展示。
// ============================================================================

// moneyRate 当前统一的折算比例（元 / 点）。取提现比例，非法值回落默认。
func (a *app) moneyRate() float64 {
	rate, _ := a.withdrawParams()
	return rate
}

// pointsToCNY 点数 → 金额（元）。
//
// 唯一的换算入口：任何把「点」折成「钱」的地方都必须走这里，避免各处手写乘法
// 时把比例取成不同来源（例如误用计费倍率 pricing_multiplier —— 那是「1 号池积分
// 折多少点」，与「1 点折多少元」是完全不同的两个量）。
func pointsToCNY(points, rate float64) float64 {
	return points * rate
}

// microToCNY 微点 → 金额（元）。落库值直接折算，省掉一层中间变量。
func microToCNY(micro int64, rate float64) float64 {
	return MicroToPoint(micro) * rate
}

// ============================================================================
// 逐用户返点比例（v0.17.7）
//
// 解析顺序（唯一口径，两处调用共用）：**用户覆盖（≥0）优先，否则全局配置**。
// 覆盖值 -1 表示「未覆盖」；0 表示「明确不分成」—— 两者语义不同，不可合并。
//
// 为什么解析放在 Proxy 而不是 app：两处入账（号主分成 creditHostOwner、
// 推广返点 creditReferrerOfPool / creditReferralOnRecharge）都发生在转发热路径上，
// 而它们本来就是 Proxy 的方法，且 Proxy 已持有 *Store。
// ============================================================================

// hostRatioFor 解析某号主（用户 ID）生效的号池分成比例。
func (p *Proxy) hostRatioFor(owner int64) float64 {
	if owner > 0 {
		if u, err := p.store.GetUserByID(owner); err == nil && u.HostRatio >= 0 {
			return u.HostRatio
		}
	}
	return p.hostShareRatioNow()
}

// referralRatioFor 解析某收益人（用户 ID）生效的推广返点比例。
func (p *Proxy) referralRatioFor(userID int64) float64 {
	if userID > 0 {
		if u, err := p.store.GetUserByID(userID); err == nil && u.ReferralRatio >= 0 {
			return u.ReferralRatio
		}
	}
	return p.referralRatioNow()
}

// ratioOverrideView 逐用户返点比例的三段视图：覆盖值 / 全局值 / 生效值。
//
// 三个都返回的理由：前端必须能同时回答「现在实际按多少算」（effective）、
// 「是不是我给他单独设过」（override，-1 即未设）、以及「不设的话会是多少」（global）。
// 只回生效值会让管理员无法判断该不该点「清除覆盖」。
type ratioOverrideView struct {
	Override  float64 `json:"override"`  // -1 = 未覆盖（跟随全局）
	Global    float64 `json:"global"`    // 全局配置值
	Effective float64 `json:"effective"` // 实际生效值
	IsCustom  bool    `json:"is_custom"` // override >= 0
}

func newRatioOverrideView(u *User, pick func(*User) float64, global float64) ratioOverrideView {
	v := ratioOverrideView{Override: RatioNotSet, Global: global, Effective: global}
	if u == nil {
		return v
	}
	ov := pick(u)
	v.Override = ov
	if ov >= 0 {
		v.IsCustom = true
		v.Effective = ov
	}
	return v
}
