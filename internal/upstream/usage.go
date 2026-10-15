// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// usage.go 上游 usage 帧解析的**唯一口径**（v0.26.7）。
//
// 为什么必须收敛到这一处：
//
// 上游末帧的 usage 是**计费与观测的共同源头** —— 扣费金额（credit）、
// token 用量、前缀缓存命中量（决定"为什么又慢又贵"）都来自它。
// 而本仓有**两条转发链路**（`cmd/webapp` 的站点后台 / `cmd/server` 的旧网关），
// 各自写了一份解析。两份口径的历史代价已经具体发生过：
//
//   - webapp 那条**从未解析** prompt_cache_hit/miss/write 三字段，而旧网关一直在采
//     → 于是"缓存到底有没有生效"在站点侧完全不可观测（v0.26.7 修的）；
//   - 旧网关只认 `"data: "`（带空格）前缀，webapp 两种都认
//     → 上游若发 `data:{...}`，旧网关会**整帧漏掉 usage**（包括 credit），
//     表现为"这次请求没有成本观测"，而实际上上游报了；
//   - 字段缺失的语义（缺失 ≠ 0）也靠各写各的指针判断维持，极易分叉。
//
// 收敛后：上游加字段只改本文件一处，两条链路自动同时生效。
// 这是"同步更新网关相关代码、避免遗漏"的**结构性**答案 —— 靠人记得同步一次两次
// 可以，靠人每次都记得不可能。
package upstream

import (
	"encoding/json"
	"strings"
)

// UsageFrame 上游一次响应里的用量观测（末帧 usage 的解析结果）。
//
// 字段语义刻意与上游一字不差，避免下游再解释一遍。
type UsageFrame struct {
	PromptTokens     int
	CompletionTokens int

	// Credit 本次真实扣费（号池积分）。HasCredit 区分「字段缺失」与「显式 0」——
	// 缺失 ≠ 0：把"缺观测"当"0 成本"写进账本，会让收费账号被误判为免费层。
	Credit    float64
	HasCredit bool

	// 前缀缓存三计数（上游明确回报时非零）。
	// 三者皆 0 = 上游未回报（老版本 / 非缓存模型），**不等于命中率为 0**。
	CacheHitTokens   int
	CacheMissTokens  int
	CacheWriteTokens int
}

// Total 本次请求的 token 总数（prompt + completion）。
func (u UsageFrame) Total() int { return u.PromptTokens + u.CompletionTokens }

// usageWire 上游 usage 的线格式。**唯一**的结构定义。
//
// Credit 用指针是为了保留「缺失」语义（见 UsageFrame.HasCredit）。
type usageWire struct {
	CompletionTokens int      `json:"completion_tokens"`
	PromptTokens     int      `json:"prompt_tokens"`
	Credit           *float64 `json:"credit"`

	PromptCacheHitTokens   int `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens  int `json:"prompt_cache_miss_tokens"`
	PromptCacheWriteTokens int `json:"prompt_cache_write_tokens"`
}

func (w *usageWire) frame() UsageFrame {
	f := UsageFrame{
		PromptTokens:     w.PromptTokens,
		CompletionTokens: w.CompletionTokens,
		CacheHitTokens:   w.PromptCacheHitTokens,
		CacheMissTokens:  w.PromptCacheMissTokens,
		CacheWriteTokens: w.PromptCacheWriteTokens,
	}
	if w.Credit != nil {
		f.Credit = *w.Credit
		f.HasCredit = true
	}
	return f
}

// sseUsageEnvelope 一帧 SSE 里只关心 usage。
type sseUsageEnvelope struct {
	Usage *usageWire `json:"usage"`
}

// ParseUsageObject 从非流式响应体里的 usage 对象（原始 JSON 字节）解析。
//
// raw 为空 / 非 JSON / 无 usage 时返回 (零值, false)。
// 刻意不返回 error：这是**观测**路径，畸形响应不该让请求失败。
func ParseUsageObject(raw []byte) (UsageFrame, bool) {
	if len(raw) == 0 {
		return UsageFrame{}, false
	}
	var w usageWire
	if json.Unmarshal(raw, &w) != nil {
		return UsageFrame{}, false
	}
	// 全零且无 credit 时视为"没有观测"：非流式响应里 usage 通常必有 token，
	// 全零多半是抓错了对象（比如把 choices 当 usage）。
	f := w.frame()
	if f.Total() == 0 && !f.HasCredit {
		return UsageFrame{}, false
	}
	return f, true
}

// sseDataPrefixes SSE 数据行的两种合法前缀。
//
// 两种都要认（v0.26.7）：上游及其镜像在 `data:` 后**是否有空格**并不一致，
// 只认一种的实现会整帧漏掉 usage —— 而漏掉的是**计费依据**，不会报错。
var sseDataPrefixes = []string{"data: ", "data:"}

// StripSSEDataPrefix 判断一行 SSE 是否为数据帧并剥掉前缀。
//
// 返回 (载荷, 是否数据帧)。`[DONE]` 是数据帧的一种，调用方自行判断。
// 前缀按**长优先**匹配，避免 "data: " 被 "data:" 先吃掉而留下一个前导空格
// （JSON 解码虽然容忍空白，但逐字节比较的调用方会因此判不等）。
func StripSSEDataPrefix(line string) (payload string, ok bool) {
	// 行尾先去掉 CR/LF（SSE 两种换行都要认）；行首容忍空白 —— 收敛前的
	// webapp 实现用的是 TrimSpace，若这里只做 TrimRight 会是一次**静默的
	// 能力回退**（前导空格的行又变成解析不到）。注释帧以 ":" 开头，
	// 不会因去掉前导空白而被误认成数据帧。
	trimmed := strings.TrimSpace(line)
	for _, p := range sseDataPrefixes {
		if strings.HasPrefix(trimmed, p) {
			return strings.TrimSpace(trimmed[len(p):]), true
		}
	}
	return "", false
}

// ParseUsageLine 从一行 SSE 解析 usage（含两种 data 前缀兼容）。
//
// ok=false 表示这一行不是数据帧 / 不是 JSON / 不含 usage —— 都是正常情况
// （SSE 流里绝大多数行不含 usage），调用方据此**跳过**即可，不要当错误。
func ParseUsageLine(line string) (UsageFrame, bool) {
	payload, isData := StripSSEDataPrefix(line)
	if !isData || payload == "" || payload == "[DONE]" {
		return UsageFrame{}, false
	}
	var env sseUsageEnvelope
	if json.Unmarshal([]byte(payload), &env) != nil || env.Usage == nil {
		return UsageFrame{}, false
	}
	return env.Usage.frame(), true
}
