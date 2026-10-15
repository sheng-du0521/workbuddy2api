// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 计费的**纯算术**：单位换算、用量估算、预扣额、单价观测规则、分成与返点公式。
//
// 为什么单独成包（v0.31.10，P2-7 拆包第三刀）：
//
//  1. 这一簇是**纯函数**（不碰 db、不碰 Proxy 状态、不发网络），却是全项目最不能算错的部分 ——
//     算错的后果不是崩溃，而是**账目长期静默偏移**：少扣的积分不会报错，只会让平台慢慢亏。
//     独立成包后可以用表驱动测试把每个边界钉死，不必拉起整个 app；
//  2. 此前这些算术**散落在三个文件**：单位换算在 store_user.go、预扣与估算在 billing.go、
//     分成与返点公式**内联在 proxy.go 的两个函数体里**。「同一个公式在两处各写一份」正是
//     本项目最贵的一类缺陷来源（2026-10-13 的三十号误杀就是这么来的）—— 现在只有一份；
//  3. 本包**不含**任何 IO 与账本写入：预扣/结算/退还的事务编排仍留在 cmd/webapp
//     （它要拿 Store 与选号状态），本刀只把\"钱怎么算\"抽出来。
//
// ⚠️ 口径纪律（改这里之前先读）：
//   - 分成的基数是**号池积分消耗**（consumedCredit），不是平台实收（chargedMicro）——
//     后者已乘过计费倍率，会让倍率变成号主收益的隐形缩放因子（倍率 < 1 时号主结构性亏损）；
//   - 但分成与返点都必须以**平台实收**封顶，否则「倍率 < 分成比例」时平台倒贴；
//   - 全部微点记账一律 math.Round（见 PointToMicro 的注释），不要用截断。
package billing

import (
	"math"
	"strconv"
)

// MicroPerPoint 余额单位换算：1 点 = 1000 微点。
//
// 用整数记账避免浮点累积误差；对外展示一律除以本值。
const MicroPerPoint = 1000

// PointToMicro 点数 → 微点。
//
// 必须用 math.Round 而不是 `int64(p*1000 + 0.5)`：Go 的 float→int 是向零截断，
// 对负数而言 `+0.5` 不是四舍五入 —— int64(-95000 + 0.5) = int64(-94999.5) 截断成
// -94999，每笔负向调整都会少扣 1 微点。math.Round 是「四舍五入、中点远离零」。
func PointToMicro(p float64) int64 { return int64(math.Round(p * MicroPerPoint)) }

// MicroToPoint 微点 → 点数（浮点，仅供展示）。
func MicroToPoint(m int64) float64 { return float64(m) / MicroPerPoint }

// FormatPoints 微点 → 展示字符串（去掉无意义的小数尾零）。
//
// 'f' + -1 精度 = 用最短且能精确表示的十进制（0.5 → "0.5"，1 点 → "1"，1 微点 → "0.001"）。
func FormatPoints(micro int64) string {
	return strconv.FormatFloat(MicroToPoint(micro), 'f', -1, 64)
}

// ---------- 用量估算（预扣用） ----------

// DefaultMaxTokens 客户端未声明 max_tokens 时按此上界估（保守）。
const DefaultMaxTokens = 4096

// MinReserveMicro 有单价时至少预扣 1 微点：估成 0 等于预扣形同虚设。
const MinReserveMicro = 1

// maxTokensCeiling 客户端声明值的天花板（防"填一个天文数字把余额全冻住"）。
const maxTokensCeiling = 1 << 20

// EstimatePromptTokens 粗略估算 prompt token 数（取保守上界）。
// 中文 UTF-8 每字 3 字节约 1 token，英文每 3 字节约 1 token，
// 故统一用 bytes/3 对两种语言都贴近上界。
func EstimatePromptTokens(payload map[string]any) int {
	msgs, _ := payload["messages"].([]any)
	total := 0
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		if mm == nil {
			continue
		}
		total += 4 // 每条消息的角色/分隔符结构开销
		switch c := mm["content"].(type) {
		case string:
			total += len(c)/3 + 1
		case []any:
			for _, part := range c {
				pm, _ := part.(map[string]any)
				if pm == nil {
					continue
				}
				if s, ok := pm["text"].(string); ok {
					total += len(s)/3 + 1
				}
				if _, ok := pm["image_url"]; ok {
					total += 1000 // 图片按固定量保守估
				}
			}
		}
	}
	return total
}

// RequestMaxTokens 取客户端声明的输出上限，缺省给保守默认值。
//
// 只认 JSON 解析后的 float64（json.Unmarshal 的数字类型）；字符串形式的 "4096"
// 一律按未声明处理 —— 不猜，宁可高估（缺省 4096）。
func RequestMaxTokens(payload map[string]any) int {
	for _, k := range []string{"max_completion_tokens", "max_tokens"} {
		if v, ok := payload[k].(float64); ok && v > 0 {
			if v > maxTokensCeiling {
				return maxTokensCeiling
			}
			return int(v)
		}
	}
	return DefaultMaxTokens
}

// ---------- 预扣额 ----------

// ReserveMicro 由「prompt 上界 + max_tokens 上界」与单价算出预扣额（微点）。
//
// 口径：宁可高估（低于 MinReserveMicro 抬到下限；高于 capMicro 压到上限）。
// capMicro <= 0 表示不限。
//
// per1kPoint 必须是**点 / 1k token 的终价**（调用方负责把「积分/1k 的观测单价」乘上倍率，
// 或直接用管理员按点填的兜底单价）。把两种来源的单位搞混是本包最容易被误用的地方，
// 故这里只接终价、不做来源判断。
func ReserveMicro(per1kPoint float64, tokens int, capMicro int64) int64 {
	if per1kPoint <= 0 || tokens <= 0 {
		return 0
	}
	micro := PointToMicro(per1kPoint * float64(tokens) / 1000.0)
	if micro < MinReserveMicro {
		micro = MinReserveMicro
	}
	if capMicro > 0 && micro > capMicro {
		micro = capMicro
	}
	return micro
}

// ---------- 单价观测 ----------

// Per1kCost 由「本次消耗的积分」与「本次 token 数」折出实测单价（积分 / 1k token）。
//
// 任一入参非正 → 返回 0（**不能让 0 变成除数**：tokens=0 的失败响应很容易走到这里，
// 除零在 Go 里得到 +Inf，写进观测表会让后续预扣算出天文数字）。
func Per1kCost(credit float64, tokens int) float64 {
	if credit <= 0 || tokens <= 0 {
		return 0
	}
	return credit / float64(tokens) * 1000
}

// MaxKeptCost 单价观测的**取历史最大值**规则：返回 (应写入的值, 是否应更新)。
//
// 取最大值而非平均：预扣宁高勿低，低估就意味着防不住刷爆（平均会被低价的历史拉低）。
// 只在"新值严格更大"时更新，保证同一模型的历史峰值单调不减。
//
//	seen=false（首次观测）→ 总是更新（只要 per1k > 0）；
//	per1k <= 0          → 不更新（无效观测不入表）。
func MaxKeptCost(old float64, seen bool, per1k float64) (float64, bool) {
	if per1k <= 0 {
		return old, false
	}
	if seen && old >= per1k {
		return old, false
	}
	return per1k, true
}

// EstimateCredit 用实测单价估算缺失的 credit：per1k × tokens / 1000。
// tokens <= 0 或单价非正 → 0（调用方据此决定是否标记 missing_credit）。
func EstimateCredit(per1k float64, tokens int) float64 {
	if per1k <= 0 || tokens <= 0 {
		return 0
	}
	return per1k * float64(tokens) / 1000.0
}

// ---------- 分成与返点 ----------

// HostEarn 代挂号主本次应得的收益（微点）。
//
//	earn = min(round(consumedCredit × ratio), chargedMicro)
//
// 基数刻意取**号池积分消耗**而非平台实收：后者已乘过计费倍率，会让倍率成为号主收益的
// 隐形缩放因子（倍率 0.1 时号主被扣 1 积分却只拿到 0.1×比例 点）。以积分口径解耦后，
// 号主收益只取决于自己账号被消耗了多少积分。
//
// 但**必须以平台该次实收封顶**，否则「倍率 < 分成比例」时平台倒贴。
// 结果 <= 0 → 返回 0（调用方据此跳过，避免刷零流水）。
//
// 统一 math.Round（与 PointToMicro 同口径）：截断会让小额收益长期归零 ——
// 实测 credit=0.01、分成 0.6、推广 0.1 时 10×0.6×0.1=0.6 微点，截断成 0，
// 号主的上级永远拿不到返点。
func HostEarn(consumedCredit, ratio float64, chargedMicro int64) int64 {
	if consumedCredit <= 0 || ratio <= 0 {
		return 0
	}
	earn := PointToMicro(consumedCredit * ratio)
	if earn > chargedMicro {
		earn = chargedMicro // 平台实收封顶：倍率低于分成比例时不倒贴
	}
	if earn <= 0 {
		return 0
	}
	return earn
}

// ReferralRebate 上级从号主本次收益里应得的推广返点（微点）。
//
//	rebate = clamp(round(earn × ratio), 0, chargedMicro - earn)
//
// 基数取**号主收益**而非平台收费，避免上级的返点吃掉平台全部留成：
// 平台本次留成 = charged - earn，返点从中出，故上限钳到「留成余额」。
// 推论（运维须知）：要让平台每次都留成，必须保证「计费倍率 > 代挂分成比例」。
//
// 结果 <= 0 → 返回 0（调用方据此跳过，避免刷零流水）。
func ReferralRebate(earn int64, ratio float64, chargedMicro int64) int64 {
	if earn <= 0 || ratio <= 0 {
		return 0
	}
	rebate := int64(math.Round(float64(earn) * ratio))
	if rebate <= 0 {
		return 0
	}
	if margin := chargedMicro - earn; rebate > margin {
		rebate = margin // 不侵占平台留成之外的额度
	}
	if rebate <= 0 {
		return 0
	}
	return rebate
}
