// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package prompt

import (
	"sync"
	"time"
)

// Gate 降级提示词的状态机（v0.31.4 起由两个产品共用；此前只存在于旧网关
// internal/server/degrade.go）。
//
// ## 它解决什么
//
// 客户端（Claude Code / Codex 等）在 system 里注入固定模板句，上游内容审核按逐字
// 匹配误杀合法流量（issue #36/PR39 的 11128，以及 400 content_blocked）。首发命中时
// 请求已经失败一次，此刻换**极简中性**的 system 提示词（Degraded）**同请求内重试一次**，
// 大多数情况下就能通过 —— 因为被拦的是那段模板的指纹，不是用户内容。
//
// 而且这类拦截往往**不是一次性**的：同一客户端的后续请求会带同一段模板，于是降级
// 必须有一个时间窗（否则每个请求都要先撞一次 400 才降级）。窗口取「到次日 00:00 CST」
// —— 与上游风控窗口的日界对齐，也和本仓既有冷却口径（签到 04:00 / 次日）同一套时间语言。
//
// ## 定位边界（别把它当对抗框架）
//
// 误报处理：只换掉「可能触发检查的 system 内容」，不改变用户指令的合法性语义。
// 用户内容本身触发审核时，降级重试第二次仍会被拦 —— 那条路径由调用方回内容防火墙错误，
// 不在这里。
//
// ## 状态：进程内存、重启清零
//
// 重启后第一次命中会重新触发一次（代价 = 一个请求多一次上游往返），可接受；
// 持久化它的收益不足以抵消「状态文件与真实上游行为可能不同步」的风险。
type Gate struct {
	mu    sync.Mutex
	until time.Time
	// triggers 触发次数（进程内累计，供面板展示"今天降级过几次"）。
	triggers int64
}

// Active 当前是否处于降级期（now < until）。
func (g *Gate) Active() bool {
	return g.ActiveAt(time.Now())
}

// ActiveAt 与 Active 同义，但时刻由调用方给出（便于单测与"同一时刻多处判定"）。
func (g *Gate) ActiveAt(now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return now.Before(g.until)
}

// Trigger 触发降级，直到次日 00:00 CST（Asia/Shanghai）。
// 已在降级期内则**不续期**（保持最早触发点的 00:00 重置语义）。
func (g *Gate) Trigger() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !time.Now().Before(g.until) {
		g.until = NextMidnightCST(time.Now())
	}
	g.triggers++
}

// Until 返回降级截止时刻（零值 = 未降级/已过期），供面板与日志展示。
func (g *Gate) Until() time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !time.Now().Before(g.until) {
		return time.Time{}
	}
	return g.until
}

// Triggers 触发次数（进程内累计）。
func (g *Gate) Triggers() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.triggers
}

// NextMidnightCST 返回 now 之后最近的 Asia/Shanghai 00:00 时刻（纯函数，方便单测）。
//
// 边界语义：
//   - 23:59 → 次日 00:00（几秒后）
//   - 00:00 → 次日 00:00（刚过 00:00，下个零点是次日）
//
// 用固定 +08:00 偏移计算，避免依赖系统时区配置（容器/宿主机时区不确定）。
func NextMidnightCST(now time.Time) time.Time {
	cst := time.FixedZone("CST", 8*60*60)
	// 把 now 转到 CST 视角取当天 00:00，再加一天；若该 00:00 <= now 则再加一天。
	y, m, d := now.In(cst).Date()
	midnight := time.Date(y, m, d, 0, 0, 0, 0, cst)
	for !midnight.After(now) {
		midnight = midnight.Add(24 * time.Hour)
	}
	return midnight
}
