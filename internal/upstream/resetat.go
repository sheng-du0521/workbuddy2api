// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// resetat.go 上游「重置时间」的统一解析入口（三级来源，一个实现，两个产品共用）。
//
// ## 为什么需要这个文件
//
// 限流类错误（6004 模型级限额 / 429 / WAF 403）的上游响应会**明说**恢复时刻，
// 而「冷却多久」这件事有两个口径：上游给的时间（权威）与本地猜的（有界退避）。
// 本地猜出来的值必然不准：猜短了会在窗口内反复撞墙（每次都是一次失败请求 +
// 一次风控痕迹），猜长了会把健康号白扣几小时。
//
// 历史上这件事有两份实现，且**能力不对等**：
//
//	cmd/server（旧网关）  → 读 body 文案 + Retry-After 头        ✅ 较完整
//	cmd/webapp（主产品）  → 只读 Retry-After 头（且只认整数字符串） ❌ 弱
//
// 后果是主产品上一条**无 Retry-After 头、只把重置时间写在 body 里**的 6004，
// 会被当成「没有时间信息的普通软冷却」处理（见 pool.CooldownSoftForModel 的
// 零值分支）—— 那是**账号级**冷却，于是「6004 只冷却该模型、切模型即可用」
// 这个承诺在真实流量里根本不成立。本文件把两份实现收敛成一份。
//
// ## 三级来源与优先级（顺序即权威性）
//
//  1. **body 文案**（`ResetBodyText`）—— 上游在人话 msg 里直接写的恢复时刻，
//     例如 `将在 2026-10-15 10:00:00 重置`。这是上游自己写给用户看的，最权威。
//  2. **body 字段**（`ResetBodyJSON`）—— 结构化字段（`data.resets_at` 等）。
//     上游族目前并不保证发它，但一旦发了它比文案更精确（无歧义、可带时区）。
//     与文案的先后是刻意的：文案是**已知存在**的形态，字段是**可能存在**的形态，
//     而字段解析要扫全 body，代价更高 —— 把便宜的权威来源放前面。
//  3. **响应头**（`ResetHeader`）—— `Retry-After` / `Retry-After-Ms` /
//     `X-Ratelimit-Reset`（见 ParseRetryAfter）。语义是「多久后可以再试」，
//     属于兜底信息：它不带绝对时刻，精度比前两者低。
//
// 三级全不可用 → `ResetNone`：调用方回落到本地有界退避，**绝不臆造时间**
// （臆造一个"看起来正常"的时间会让排障时无法区分"上游说的"与"我们猜的"）。
//
// ## 时区与格式（这是最容易错的地方）
//
// 上游文案里的墙钟是**上游本地时区 UTC+8**（与 softRateResetLoc 同一口径，见
// client.go 的说明）。解析规则：
//
//	形态                              处理
//	--------------------------------  --------------------------------------------
//	2026-10-15 10:00:00（无后缀）      按 UTC+8 解释（上游文案形态）
//	2026-10-15 10:00:00 UTC+8（有后缀）  剥掉后缀后同上一行（上游偶尔带后缀）
//	2026-10-15T10:00:00+08:00          RFC3339：**按自带偏移**解析（最精确）
//	2026-10-15T10:00:00Z               同上（UTC 时刻）
//	1791972000 / "1791972000"           epoch 秒 → 绝对时刻（时区无关）
//	1791972000000                       epoch 毫秒（>=1e12）→ 绝对时刻
//	3600 / "3600"（< 1e8）              相对秒数 → now + 3600s
//
// **把「无后缀墙钟」当成 UTC 解析会整体偏 8 小时** —— 偏早 8 小时等于冷却
// 几乎立刻到期（窗口内反复撞墙），偏晚 8 小时等于白扣一个号半天。两种错都
// 比"没有时间信息"更糟，所以这里显式固定 UTC+8 而不是交给 time.Parse。
//
// ## 边界情况（三种，均有明确策略）
//
//	缺失（ResetNone）      调用方回落到本地有界退避（base 起指数放大、封顶
//	                       soft_rate_max）。不延长、不缩短。
//	已过期（ResetExpired） 解析到了但时刻不在未来（上游文案过期 / 本地时钟偏移）。
//	                       **按"没有时间信息"处理**，回落到有界退避 —— 而不是
//	                       "立刻恢复"。理由：立刻恢复会让同一秒内的并发重试
//	                       立刻再吃一次同样的错误（热循环），而有界退避最短也是
//	                       秒级，足以让频控窗口滑过；代价只是偶尔多等几十秒。
//	并发                   多次并发触发由 pool 侧收敛（见 pool.CooldownSoftForModel）：
//	                       取「更晚的截止」，绝不因为并发响应乱序而缩短已有冷却。
package upstream

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ResetSource 重置时间的来源（用于日志与台账，让排障时一眼看出"这个时长哪来的"）。
type ResetSource string

const (
	// ResetNone 上游没有给出任何重置信息 → 调用方回落本地有界退避。
	ResetNone ResetSource = "none"
	// ResetBodyText 来自 body 里「将在 … 重置」的人话文案（最权威）。
	ResetBodyText ResetSource = "body-text"
	// ResetBodyJSON 来自 body 的结构化字段（data.resets_at 等）。
	ResetBodyJSON ResetSource = "body-json"
	// ResetHeader 来自 Retry-After / Retry-After-Ms / X-Ratelimit-Reset 响应头（兜底）。
	ResetHeader ResetSource = "header"
	// ResetExpired 解析到了时间，但该时刻不在未来 → 视同"没有时间信息"（见文件头「已过期」）。
	ResetExpired ResetSource = "expired"
)

// resetAtFieldKeys 结构化字段的候选键（snake / camel 双形态，与仓库其余
// 解析器「snake 优先于 camel」的既定顺序一致，见 session.ExtractKey）。
//
// 只收 reset 语义的键，刻意**不收** expires_at / expire_time 一类：
// 那些词在上游其它业务（包月周期、token 有效期）上大量出现，收进来会把
// 「套餐到期」误读成「限流恢复」。宁缺毋滥。
var resetAtFieldKeys = []string{
	"resets_at", "reset_at", "reset_time",
	"resetsAt", "resetAt", "resetTime",
}

// resetInFieldKeys 相对秒数的候选键（"多久以后"而不是"什么时刻"）。
// 同样只收 reset 语义：retry_after 因为是 HTTP 头的同名形态而收，
// wait / sleep / delay 一类并发/调度语义的词一律不收。
var resetInFieldKeys = []string{
	"resets_in", "reset_in", "reset_after", "retry_after",
	"resetsIn", "resetIn", "resetAfter", "retryAfter",
}

// resetJSONScanDepth 结构化字段的最大下钻层数（含顶层，即覆盖
// root / root.data / root.error.data 三级）。
//
// 取值 3 而不是不限深度：上游信封是 {code,msg,data} 与 {error:{code,msg,data}}
// 两种固定形态，三层足够且确定性可控；再深就是畸形体（偶尔夹带整段 HTML
// 拦截页），继续下钻是纯浪费。
const resetJSONScanDepth = 3

// ResolveResetAt 解析上游给出的重置墙钟（三级来源，见文件头）。
//
// 返回 (时刻, 来源)：来源为 ResetNone / ResetExpired 时**时刻为零值**，
// 调用方必须回落到本地有界退避（不要拿零值时刻去做 now.Add 之类运算）。
//
// now 由调用方传入而不是内部取 time.Now()：一是可测（本函数是纯函数），
// 二是同一次错误处置里所有时间判断共用同一个"现在"，避免参数漂移。
func ResolveResetAt(h http.Header, body []byte, now time.Time) (time.Time, ResetSource) {
	wait, _ := parseRetryAfterHeader(h, resetHeaderSanity)
	return ResolveResetAtWith(body, wait, now)
}

// ResolveResetAtWith 与 ResolveResetAt 同源，但响应头那一级由调用方以
// 「已解析好的等待时长」传入。
//
// 存在的理由：本仓旧网关（internal/server）收到的上游响应已经被
// upstream.Client 解析成 Error 信封，头信息只剩 Error.RetryAfter 这一个
// 时长 —— 它拿不到原始的 http.Header。让两个产品共用同一份**优先级与
// 边界策略**（文案 > 字段 > 头 > 无）比让各自维护一份更安全：这份策略
// 的每一处偏差都会变成"同一个上游错误在两个产品上冷却时长不同"。
//
// 与 ResolveResetAt 的一处**已知差异**（如实记录）：旧网关的 headerWait 来自
// Error.RetryAfter，它带 2h 防呆上限；因此"上游明示 5 小时后再试"在旧网关上
// 会被丢弃、回落有界退避，而主产品按 7 天上限接受后再由 pool 截断到
// soft_rate_max。方向是旧网关更保守（更短），不会造成裸奔或热循环。
func ResolveResetAtWith(body []byte, headerWait time.Duration, now time.Time) (time.Time, ResetSource) {
	// 第 1 级：body 文案（最权威，且最便宜）。
	if t, ok := ParseRateReset(string(body)); ok {
		return usableResetAt(t, now, ResetBodyText)
	}
	// 第 2 级：body 结构化字段。
	if t, ok := parseResetAtJSON(body, now); ok {
		return usableResetAt(t, now, ResetBodyJSON)
	}
	// 第 3 级：响应头（兜底）。上限的职责是拒绝荒谬值，截断由 pool 统一负责。
	if headerWait > 0 && headerWait <= resetHeaderSanity {
		return usableResetAt(now.Add(headerWait), now, ResetHeader)
	}
	return time.Time{}, ResetNone
}

// ResetSourceSuffix 把重置来源渲染成「冷却原因」里可读的后缀（供日志与台账）。
//
// 收敛到 upstream 包的理由：这段文案此前在主产品（cmd/webapp 的 resetSrcSuffix）
// 与旧网关（applyErrorPolicy 内联判断）各写一份，而它写进的是 (账号,模型) 冷却的
// **reason 字段** —— 两份文案漂移会让"同一个上游错误在两个产品上的冷却原因不同名"，
// 排障时看起来像两种不同的故障。与 ResolveResetAtWith 共用同一份优先级是同一件事。
func ResetSourceSuffix(src ResetSource) string {
	switch src {
	case ResetBodyText:
		return " [reset=body-text]"
	case ResetBodyJSON:
		return " [reset=body-json]"
	case ResetHeader:
		return " [reset=header]"
	case ResetExpired:
		return " [reset=expired→退避]"
	default:
		return " [reset=none→退避]"
	}
}

// usableResetAt 把「解析到的时刻」收敛成对外契约：不在未来的一律判为过期。
//
// 抽出来是因为三级来源都走这一关（否则每级各写一遍 now.Before 判断，
// 迟早有一级漏掉，而漏掉的那一级就是热循环的来源）。
func usableResetAt(t, now time.Time, src ResetSource) (time.Time, ResetSource) {
	if t.IsZero() || !t.After(now) {
		return time.Time{}, ResetExpired
	}
	return t, src
}

// parseResetAtJSON 从 body 的结构化字段里取重置时间。
//
// 返回的时刻已是绝对时刻（epoch 直接换算；墙钟按 UTC+8 解释；相对秒数已加 now）。
func parseResetAtJSON(body []byte, now time.Time) (time.Time, bool) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return time.Time{}, false
	}
	for _, m := range scanEnvelopes(root, resetJSONScanDepth) {
		for _, k := range resetAtFieldKeys {
			v, ok := m[k]
			if !ok {
				continue
			}
			if t, ok := parseResetValue(v, now, false); ok {
				return t, true
			}
		}
		for _, k := range resetInFieldKeys {
			v, ok := m[k]
			if !ok {
				continue
			}
			if t, ok := parseResetValue(v, now, true); ok {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

// scanEnvelopes 产出「顶层 + 逐层下钻」的信封对象，供字段扫描。
//
// 为什么不是递归：上游信封固定是 {code,msg,data} / {error:{code,msg,data}} 形态，
// 且错误体偶尔夹带整段 HTML 拦截页。有界广度优先既覆盖真实形态，又不会在
// 畸形/巨型 body 上失控。
//
// 顺序必须**确定**（含子对象的键名排序）：map 遍历无序，不排序会让同一份 body
// 每次解析出不同的键 —— 表现是"偶发取不到重置时间"，最难查的一类缺陷。
func scanEnvelopes(root map[string]any, depth int) []map[string]any {
	out := make([]map[string]any, 0, 4)
	level := []map[string]any{root}
	for d := 0; d < depth && len(level) > 0; d++ {
		out = append(out, level...)
		next := make([]map[string]any, 0, 4)
		for _, m := range level {
			next = append(next, childEnvelopes(m)...)
		}
		level = next
	}
	return out
}

// childEnvelopes 一个信封对象的直接子对象，按**确定顺序**返回：
// data / error / result 先（上游信封的既有三处），其余子对象按其键名升序补足。
func childEnvelopes(m map[string]any) []map[string]any {
	out := make([]map[string]any, 0, 3)
	seen := map[string]bool{}
	for _, k := range []string{"data", "error", "result"} {
		if v, ok := m[k].(map[string]any); ok {
			out = append(out, v)
			seen[k] = true
		}
	}
	rest := make([]string, 0, len(m))
	for k, v := range m {
		if _, ok := v.(map[string]any); ok && !seen[k] {
			rest = append(rest, k)
		}
	}
	sortStrings(rest)
	for _, k := range rest {
		out = append(out, m[k].(map[string]any))
	}
	return out
}

// parseResetValue 单个字段值 → 绝对时刻。
//
// relative 表示该键的语义是「相对秒数」（reset_in 族）；此时数字一律按
// now+n 秒解释，字符串只接受纯数字（`"3600"`），不接受墙钟 —— 键名说
// 的是"多久以后"，给个墙钟属于上游自相矛盾，宁缺毋滥地丢弃。
func parseResetValue(v any, now time.Time, relative bool) (time.Time, bool) {
	switch x := v.(type) {
	case float64:
		return numericResetAt(x, now, relative)
	case json.Number:
		if f, err := x.Float64(); err == nil {
			return numericResetAt(f, now, relative)
		}
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return time.Time{}, false
		}
		// 纯数字字符串：与数字同口径（上游常把时间戳序列化成长整型字符串，
		// 以免在 JS 侧被浮点截断）。
		if isAllDigits(s) {
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				return numericResetAt(float64(n), now, relative)
			}
		}
		if relative {
			return time.Time{}, false
		}
		return parseResetWallClock(s)
	}
	return time.Time{}, false
}

// epochSecThreshold / epochMsThreshold 数字形态的语义分界。
//
// 判定依据是**量级**而不是位数：
//
//	< 1e8（≈1973 年）   相对秒数（"多久以后"）
//	< 1e12              绝对 epoch 秒（1e8 ≈ 1973、1e12 ≈ 2001 年 9 月）
//	>= 1e12             绝对 epoch 毫秒
//
// 1e8/1e12 两个界既避开"把 3600 秒当成 1970 年"的荒谬，也避开"把毫秒当秒"
// （那会算出一个 5 万年后才到期的冷却，等于永久摘除）。
const (
	epochSecThreshold = 1e8
	epochMsThreshold  = 1e12
)

// resetHeaderSanity 响应头形态的等待时长上限。
//
// 取值 7 天：它的职责只是拒绝"明显不是时长"的荒谬值（例如把序号/HTTP-Date
// 误当数字），而不是截断 —— 截断由 pool 的软冷却封顶统一负责。为什么不能沿用
// ParseRetryAfter 的 2h：x-ratelimit-reset 是**绝对时刻**，与 2h 无关，
// 一处 5 小时后的真实重置被丢掉就会退化成 60 秒冷却（窗口内反复撞墙）。
const resetHeaderSanity = 7 * 24 * time.Hour

// numericResetAt 数字形态 → 绝对时刻。
func numericResetAt(n float64, now time.Time, relative bool) (time.Time, bool) {
	if n <= 0 {
		return time.Time{}, false
	}
	if relative || n < epochSecThreshold {
		// 相对秒数。上限 1e8 秒（≈3.17 年）纯属防呆：真出现这种值说明上游
		// 发的东西不是我们理解的那个语义，宁可丢弃回退有界退避。
		if n >= epochSecThreshold {
			return time.Time{}, false
		}
		return now.Add(time.Duration(n * float64(time.Second))), true
	}
	if n >= epochMsThreshold {
		return time.UnixMilli(int64(n)), true
	}
	return time.Unix(int64(n), 0), true
}

// parseResetWallClock 字符串墙钟 → 绝对时刻（时区规则见文件头）。
func parseResetWallClock(s string) (time.Time, bool) {
	// 先试 RFC3339（自带偏移/Z，最精确）。上游若发这种形态，就必须尊重它
	// 自带的偏移，不能按 UTC+8 二次解释。
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	// 再试「无偏移但有 T 分隔」的 ISO 形态（2026-10-15T10:00:00）：上游若只给
	// 本地墙钟而漏了偏移，与文案同口径按 UTC+8 解释。
	if t, err := time.ParseInLocation("2006-01-02T15:04:05", s, softRateResetLoc); err == nil {
		return t, true
	}
	// 最后是文案形态：剥掉可选的 " UTC+8" 后缀后按 UTC+8 解释。
	ts := strings.TrimSuffix(strings.TrimSpace(s), " UTC+8")
	if t, err := time.ParseInLocation(softRateTimeLayout, ts, softRateResetLoc); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// sortStrings 极简有序化（避免为一个 4 元素的切片引入 sort 依赖）。
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
