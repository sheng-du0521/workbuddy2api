// Package modelname 模型名归一化 —— cmd/webapp（主产品）与 internal/server
// （旧网关）**共用同一份实现与同一份口径**。
//
// ## 为什么必须共用（v0.31.3）
//
// 上游对模型名**大小写敏感**、且不容忍空白与网关侧前缀（本机实测，见 docs/同类项目调研-20261016.md）：
//
//	model                    → 上游返回
//	glm-5.3                  → 200
//	GLM-5.3                  → 400 {"code":11102,"msg":"model [GLM-5.3] service info not found"}
//	" glm-5.3 "（首尾空格）   → 400 11102
//	"glm-5.3\n"（尾随换行）   → 400 11102
//	[global:]glm-5.3         → 400 11102
//	（无 model 字段）         → 400 11102 "model []"
//
// 两个产品读写的是**同一个上游账号**。同一个客户端写法若在两个产品上被解释成
// 不同的模型名，就会出现两类只有一边能修的故障：
//
//  1. 同一个必然失败的写法，在一个产品上被救回、在另一个产品上继续 404；
//  2. (账号, 模型) 维度的冷却/负缓存键不同源 —— A 产品写下的 6004 冷却，
//     B 产品看不见，于是 B 继续把同模型请求发给已被限额的号。
//
// 这与本项目最贵的一条教训同源：**同一个东西只能有一套纪律**（见 MEMORY.md 风控红线）。
//
// ## 四层（从保守到激进）
//
//  1. 去首尾空白/零宽字符（无争议，纯噪声）
//  2. 剥网关侧 realm 前缀 `cn:` / `global:` / `[cn:]` / `[global:]`
//     （前缀是本仓的路由协议，上游只认裸名）
//  3. 与已知清单**忽略大小写**精确匹配 → 用清单里的规范写法
//  4. 与已知清单**忽略大小写与分隔符**（`-`/`_`/`.`/空格/`[]()`）匹配且唯一 → 用规范写法
//
// ## 边界：只做无损归一
//
// 绝不猜模型（不做"最相近"模糊匹配后直接改）：只有在**与已知模型清单匹配**时才改写，
// 且要求匹配唯一。匹配不上就原样送出去 ——「模型不存在」是**可诊断**的错误，
// 而猜错会把它变成一次沉默的失败（且会写出错误的冷却键）。
package modelname

import (
	"log"
	"strings"
	"sync"
)

// TrimCutset 需要从模型名首尾去掉的字符。
//
// 除常规空白外刻意包含几个**看起来像空白但 TrimSpace 不处理**的：
// U+00A0 不间断空格、U+200B/U+200C/U+200D 零宽字符、U+FEFF BOM —— 它们最常见的来源是
// 「从网页/文档里复制模型名」，复制出来的字符串肉眼完全看不出区别，
// 而上游会把它当成另一个（不存在的）模型。
const TrimCutset = " \t\r\n\v\f\u00a0\u200b\u200c\u200d\ufeff"

// FallbackModels 上游模型列表取不到时使用的兜底清单（真实可用名以账号实际权限为准）。
//
// 放在本包而不是各产品里：两个产品的"最小已知集合"必须一致，否则同一个写法在
// 一个产品上能被折回规范名、在另一个上不能 —— 又回到本包开头说的口径分叉。
// 它只参与**归一化匹配**（不改写行为的判定），不参与任何出站调用。
var FallbackModels = []string{
	"deepseek-v4-flash",
	"deepseek-v4-pro",
	"glm-5.2",
	"claude-sonnet-4.5",
	"gpt-5",
}

// Normalize 把客户端传来的模型名归一成「上游认得的写法」。
//
// 返回归一化后的名字与「做了什么」的说明（说明为空 = 一字未改，供日志与台账使用）。
// known 是已知模型清单（可为空：为空时只做去空白与前缀剥离，不做大小写/分隔符归一 ——
// 清单本身来自上游 /v1/models 或成功观测，拿不到时不该凭猜改写）。
func Normalize(raw string, known []string) (string, string) {
	s := strings.Trim(raw, TrimCutset)
	var how []string
	if s != raw {
		how = append(how, "去首尾空白")
	}
	if bare, ok := StripRealmPrefix(s); ok {
		s, how = bare, append(how, "剥 realm 前缀")
	}
	// 先做第 3 层（更精确），命中就不做第 4 层 —— 避免"更宽松的规则覆盖更严格的匹配"。
	if id, n := Match(s, known, false); n == 1 {
		if id != s {
			how = append(how, "大小写归一→"+id)
		}
		s = id
	} else if id, n := Match(s, known, true); n == 1 {
		if id != s {
			how = append(how, "分隔符归一→"+id)
		}
		s = id
	}
	// 全为空白的输入（`"  "`）归一后为空：保持空串，让上游照常报"model [] 不存在"
	// —— 这里不编造任何名字。
	if s == "" {
		return "", strings.Join(how, "+")
	}
	return s, strings.Join(how, "+")
}

// StripRealmPrefix 剥掉网关侧的 realm 前缀，返回裸名与是否剥离。
//
// 与旧网关 internal/server.resolveModel 的规则**刻意保持一致**（`cn:` / `global:`，
// 前缀必须是这两个词之一）—— 两个产品对同一个客户端写法的解释不能不同；
// 额外接受方括号写法 `[cn:]` / `[global:]`（本仓文档与部分客户端用这种形态）。
func StripRealmPrefix(s string) (string, bool) {
	for _, r := range []string{"cn", "global"} {
		for _, form := range []string{r + ":", "[" + r + ":]"} {
			if strings.HasPrefix(s, form) {
				rest := strings.Trim(s[len(form):], TrimCutset)
				if rest != "" {
					return rest, true
				}
			}
		}
	}
	return s, false
}

// Match 在已知清单里找匹配，返回命中的规范写法与**命中数量**。
//
// 返回数量而不是"第一个命中"：命中不唯一时（例如清单里同时存在 `abc` 与 `ABC`）
// 调用方必须放弃改写 —— 猜哪个都可能猜错，而猜错的代价是不可诊断的失败。
func Match(s string, known []string, loose bool) (string, int) {
	if s == "" {
		return s, 0
	}
	key := LooseKey(s, loose)
	if key == "" {
		return s, 0
	}
	hit, n := "", 0
	for _, id := range known {
		if id == "" {
			continue
		}
		if LooseKey(id, loose) == key {
			hit, n = id, n+1
			if n > 1 {
				return "", n // 多个命中：调用方无从选择
			}
		}
	}
	return hit, n
}

// LooseKey 归一化比较键：loose=false 时只折叠大小写；true 时再抹掉分隔符。
func LooseKey(s string, loose bool) string {
	s = strings.ToLower(s)
	if !loose {
		return s
	}
	return strings.Map(func(r rune) rune {
		switch r {
		case '-', '_', '.', ' ', '[', ']', '(':
			return -1
		}
		return r
	}, s)
}

// IsKnown 该名字是否在已知清单里（忽略大小写与分隔符，与归一化同口径）。
//
// 同口径很重要：归一化能匹配上的名字就不该被当成"未知"，
// 否则一条"未知模型"的 WARN 会与"其实能归一化成功"自相矛盾。
func IsKnown(known []string, m string) bool {
	if m == "" {
		return true
	}
	if id, n := Match(m, known, false); n == 1 && id != "" {
		return true
	}
	if id, n := Match(m, known, true); n == 1 && id != "" {
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// 「请求的模型名不在已知清单内」的诊断（v0.31.2；v0.31.3 起两个产品共用）
// ---------------------------------------------------------------------------

// unknown 已警告过的未知模型名（每个名字只警告一次，上限 64）。
//
// 为什么值得一条 WARN：这是**客户端配置错**最直接的症状 —— 模型名不在上游认得的清单里，
// 于是每次调用都会得到上游 400/11102（webapp 回 404）。没有这条日志时，运维要在
// 台账、上游原文、客户端配置三者之间对账才能得出结论。
// 上限 + 去重是为了不让"随机乱打模型名"的客户端把日志刷爆。
var unknown struct {
	mu   sync.Mutex
	seen map[string]struct{}
}

// NoteUnknown 记一次"未知模型名"（同名只记一次）。known 只用于取样展示。
// label 是日志前缀里的产品名（"proxy" / "server"），两个产品各自的日志都能按前缀筛。
func NoteUnknown(m string, known []string, label string) {
	unknown.mu.Lock()
	defer unknown.mu.Unlock()
	if unknown.seen == nil {
		unknown.seen = map[string]struct{}{}
	}
	if _, ok := unknown.seen[m]; ok {
		return
	}
	if len(unknown.seen) >= 64 {
		return
	}
	unknown.seen[m] = struct{}{}

	// 取样：清单前 8 个（清单本身可能上百条，日志里全打没有意义）。
	sample := known
	if len(sample) > 8 {
		sample = sample[:8]
	}
	log.Printf("WARN: [%s] 模型名 %q 不在已知清单内（上游多半会报 11102「模型不存在」）—— "+
		"请核对客户端配置；已知样例：%v（清单共 %d 个，含大小写/分隔符归一后的匹配）",
		label, m, sample, len(known))
}
