// 模型名归一化（v0.31.2）。
//
// ## 为什么必须做（本机实测，不是推测）
//
// 上游对模型名是**大小写敏感**、且不容忍空白与网关侧前缀的：
//
//	model                    → 上游返回
//	glm-5.3                  → 200
//	GLM-5.3                  → 400 {"code":11102,"msg":"model [GLM-5.3] service info not found"}
//	" glm-5.3 "（首尾空格）   → 400 11102
//	"glm-5.3\n"（尾随换行）   → 400 11102
//	[global:]glm-5.3         → 400 11102
//	（无 model 字段）         → 400 11102 "model []"
//
// 同类项目的公开排障文档也把「模型 ID 的大小写 / 横杠必须与平台展示名严格一致」
// 列为接入失败的第一高频原因（见 docs/同类项目调研-20261016.md）。
//
// ## 三重收益
//
//  1. **本来必然失败的调用变成正常调用**：一次写法错误此前要付出「上游一次 400 +
//     一次换号 + 最终 404」的代价，现在只改字符串就通了；
//  2. **冷却 / 负缓存按归一化后的键记录**：否则同一模型的不同写法各占一个
//     (账号, 模型) 冷却槽 —— `GLM-5.3` 撞出的 6004 拦不住随后用 `glm-5.3` 发来的请求
//     （冷却被"绕过"，同时台账的按模型统计被写法噪声污染）；
//  3. 出站请求体也带上规范写法，上游不再看到「不存在的模型」。
//
// ## 边界：只做无损归一
//
// 绝不猜模型（不做"最相近"模糊匹配后直接改）：只有在**与已知模型清单匹配**时才改写，
// 且要求匹配唯一。匹配不上就原样送出去 —— 「模型不存在」是**可诊断**的错误，
// 而猜错会把它变成一次沉默的失败（且会写出错误的冷却键）。
package main

import (
	"log"
	"sort"
	"strings"
	"sync"
)

// modelTrimCutset 需要从模型名首尾去掉的字符。
//
// 除常规空白外刻意包含几个**看起来像空白但 TrimSpace 不处理**的：
// U+00A0 不间断空格、U+200B 零宽空格、U+FEFF BOM —— 它们最常见的来源是
// 「从网页/文档里复制模型名」，复制出来的字符串肉眼完全看不出区别，
// 而上游会把它当成另一个（不存在的）模型。
const modelTrimCutset = " \t\r\n\v\f\u00a0\u200b\u200c\u200d\ufeff"

// normalizeModelName 把客户端传来的模型名归一成「上游认得的写法」。
//
// 返回归一化后的名字与「做了什么」的说明（说明为空 = 一字未改，供日志与台账使用）。
// known 是已知模型清单（可为空：为空时只做去空白与前缀剥离，不做大小写/分隔符归一 ——
// 清单本身来自上游 /v1/models，拿不到时不该凭猜改写）。
//
// 归一化按**从保守到激进**四层，任何一层失败就停在该层的结果上：
//
//  1. 去首尾空白/零宽字符（无争议，纯噪声）
//  2. 剥网关侧 realm 前缀 `cn:` / `global:` / `[cn:]` / `[global:]`
//     （前缀是**本仓的**路由协议，上游只认裸名；旧网关一直在剥，webapp 此前漏了）
//  3. 与已知清单**忽略大小写**精确匹配 → 用清单里的规范写法
//  4. 与已知清单**忽略大小写与分隔符**（`-`/`_`/`.`/空格）匹配且唯一 → 用规范写法
//     （客户端把 `glm-5.3` 写成 `glm-5-3` / `GLM_5.3`，上游就是另一个模型）
func normalizeModelName(raw string, known []string) (string, string) {
	s := strings.Trim(raw, modelTrimCutset)
	var how []string
	if s != raw {
		how = append(how, "去首尾空白")
	}
	if bare, ok := stripRealmPrefix(s); ok {
		s, how = bare, append(how, "剥 realm 前缀")
	}
	// 先做第 3 层（更精确），命中就不做第 4 层 —— 避免"更宽松的规则覆盖更严格的匹配"。
	if id, n := matchKnownModel(s, known, false); n == 1 {
		if id != s {
			how = append(how, "大小写归一→"+id)
		}
		s = id
	} else if id, n := matchKnownModel(s, known, true); n == 1 {
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

// stripRealmPrefix 剥掉网关侧的 realm 前缀，返回裸名与是否剥离。
//
// 与旧网关 internal/server/resolveModel 的规则**刻意保持一致**（`cn:` / `global:`，
// 前缀必须是这两个词之一）—— 两个产品对同一个客户端写法的解释不能不同；
// 额外接受方括号写法 `[cn:]` / `[global:]`（本仓文档与部分客户端用这种形态）。
func stripRealmPrefix(s string) (string, bool) {
	for _, r := range []string{"cn", "global"} {
		for _, form := range []string{r + ":", "[" + r + ":]"} {
			if strings.HasPrefix(s, form) {
				rest := strings.Trim(s[len(form):], modelTrimCutset)
				if rest != "" {
					return rest, true
				}
			}
		}
	}
	return s, false
}

// matchKnownModel 在已知清单里找匹配，返回命中的规范写法与**命中数量**。
//
// 返回数量而不是"第一个命中"：命中不唯一时（例如清单里同时存在 `abc` 与 `ABC`）
// 调用方必须放弃改写 —— 猜哪个都可能猜错，而猜错的代价是不可诊断的失败。
func matchKnownModel(s string, known []string, loose bool) (string, int) {
	if s == "" {
		return s, 0
	}
	key := looseKey(s, loose)
	if key == "" {
		return s, 0
	}
	hit, n := "", 0
	for _, id := range known {
		if id == "" {
			continue
		}
		if looseKey(id, loose) == key {
			hit, n = id, n+1
			if n > 1 {
				return "", n // 多个命中：调用方无从选择
			}
		}
	}
	return hit, n
}

// looseKey 归一化比较键：loose=false 时只折叠大小写；true 时再抹掉分隔符。
func looseKey(s string, loose bool) string {
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

// knownModelIDs 归一化时使用的已知模型清单（三源合并，**全部零出站**）：
//
//  1. /v1/models 的新鲜缓存（5 分钟 TTL）—— 最全；
//  2. 池内**实际服务过**的模型（observed，来自 (账号,模型) 成本台账与模型级冷却键）
//     —— 进程刚启动、缓存还没热时它就有值，且名字一定被上游认过；
//  3. 内置兜底清单 —— 池空/新部署时的最小集合。
//
// 为什么"绝不出站"是硬要求：ServeChat 是热路径，而权威清单要一次上游往返。
// 拿不到就退化成只做去空白/剥前缀（第 1、2 层），归一化的正确性不受影响
// —— 少的是"把大小写折回规范名"这一层便利。
//
// 上限 256 条：清单只用于字符串比较，但**不该无界**（模型名来自请求与服务端观测，
// 属于外部输入；一个乱打模型的客户端能把池内观测表撑大）。
func knownModelIDs(observed []string) []string {
	const maxKnown = 256
	out := make([]string, 0, 32)
	seen := map[string]struct{}{}
	add := func(m string) {
		if m == "" || len(out) >= maxKnown {
			return
		}
		if _, ok := seen[m]; ok {
			return
		}
		seen[m] = struct{}{}
		out = append(out, m)
	}
	if ids, ok := modelsCacheSnapshotIfFresh(); ok {
		for _, m := range ids {
			add(m)
		}
	}
	for _, m := range observed {
		add(m)
	}
	for _, m := range recentModelList() {
		add(m)
	}
	for _, m := range fallbackModels {
		add(m)
	}
	return out
}

// ---------------------------------------------------------------------------
// 「本进程/本库见过且**上游认过**的模型名」——归一化的第三类证据源
// ---------------------------------------------------------------------------

// recentModels 近期成功服务过的模型名（进程内 + 启动时从 usage_logs 种子化）。
//
// 为什么需要它（本机实测的教训）：模型清单缓存（/v1/models，5 分钟 TTL）在
// **进程刚启动**时是空的，而池内成本台账（pool.ObservedModels）只覆盖最近 6 小时
// 且未必落盘 —— 两者都空时，"大小写归一 / 分隔符归一"这两层会静默失效：
// 实测重启后 `GLM-5.3` 仍然 404，而 `" glm-5.3 "`（去空白层不依赖清单）已经 200。
// 于是加一条**零出站且跨重启可靠**的来源：usage_logs 里 status=ok 的模型名。
//
// 只记**成功**过的名字：失败的那次可能是写法错的（把错的写进清单，
// 下次就会把正确写法"归一"成错的 —— 恰好是最危险的方向）。
var recentModels struct {
	mu   sync.RWMutex
	list []string
	seen map[string]struct{}
}

// rememberModel 记录一个**上游认过**的模型名（幂等；上限 256，超出丢弃新值）。
func rememberModel(m string) {
	m = strings.TrimSpace(m)
	if m == "" {
		return
	}
	recentModels.mu.Lock()
	defer recentModels.mu.Unlock()
	if recentModels.seen == nil {
		recentModels.seen = map[string]struct{}{}
	}
	if _, ok := recentModels.seen[m]; ok {
		return
	}
	if len(recentModels.list) >= 256 {
		return // 只增不替换：清单是外部输入的派生集，宁可保留已确认的旧值
	}
	recentModels.seen[m] = struct{}{}
	recentModels.list = append(recentModels.list, m)
}

// recentModelList 返回近期成功模型名的副本（稳定排序，便于测试与日志）。
func recentModelList() []string {
	recentModels.mu.RLock()
	defer recentModels.mu.RUnlock()
	out := make([]string, len(recentModels.list))
	copy(out, recentModels.list)
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// 「请求的模型名不在已知清单内」的诊断（v0.31.2）
// ---------------------------------------------------------------------------

// unknownModels 已警告过的未知模型名（每个名字只警告一次，上限 64）。
//
// 为什么值得一条 WARN：这是**客户端配置错**最直接的症状 —— 模型名不在上游认得的清单里，
// 于是每次调用都会得到上游 400/11102（我们回 404）。没有这条日志时，运维要在
// 台账、上游原文、客户端配置三者之间对账才能得出结论。
// 上限 + 去重是为了不让"随机乱打模型名"的客户端把日志刷爆。
var unknownModels struct {
	mu   sync.Mutex
	seen map[string]struct{}
}

// noteUnknownModel 记一次"未知模型名"（同名只记一次）。known 只用于取样展示。
func noteUnknownModel(m string, known []string) {
	unknownModels.mu.Lock()
	defer unknownModels.mu.Unlock()
	if unknownModels.seen == nil {
		unknownModels.seen = map[string]struct{}{}
	}
	if _, ok := unknownModels.seen[m]; ok {
		return
	}
	if len(unknownModels.seen) >= 64 {
		return
	}
	unknownModels.seen[m] = struct{}{}

	// 取样：清单前 8 个（清单本身可能上百条，日志里全打没有意义）。
	sample := known
	if len(sample) > 8 {
		sample = sample[:8]
	}
	log.Printf("WARN: [proxy] 模型名 %q 不在已知清单内（上游多半会报 11102「模型不存在」）—— "+
		"请核对客户端配置；已知样例：%v（清单共 %d 个，含大小写/分隔符归一后的匹配）",
		m, sample, len(known))
}

// knownModelSet 该名字是否在已知清单里（忽略大小写与分隔符，与归一化同口径）。
//
// 同口径很重要：归一化能匹配上的名字就不该被当成"未知"，
// 否则一条"未知模型"的 WARN 会与"其实能归一化成功"自相矛盾。
func knownModelSet(known []string, m string) bool {
	if m == "" {
		return true
	}
	if id, n := matchKnownModel(m, known, false); n == 1 && id != "" {
		return true
	}
	if id, n := matchKnownModel(m, known, true); n == 1 && id != "" {
		return true
	}
	return false
}
