// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// Package rotateguard 轮转退避与 WAF IP 级拦截 fail-fast 的共享实现。
//
// 设计动机：cmd/server 与 cmd/webapp 是同一网关的两个部署形态，二者都需要
// 「换号轮转时退避」与「WAF IP 级拦截时止损」两件事。此前这套逻辑只存在于
// internal/server（私有符号 wafIPGate / backoffAfter），cmd/webapp（对外
// /v1/chat/completions 的生产形态）缺失，导致两个实际风险：
//   - 出口 IP 被上游 WAF 频控时，webapp 会继续换号猛打，把一次 IP 级拦截
//     放大 N 倍请求量（server 侧会 fail-fast 止损）；
//   - 换号零间隔，与既有「出口抖动」防封意图不一致。
//
// 提取为独立包后两端共用，避免两份实现长期漂移。本包为**纯逻辑**（不依赖
// pool / handler / HTTP），只做三件事：退避时长计算、可取消等待、IP 级状态机。
//
// 与 internal/server 既有实现的关系：逻辑逐行等价（server 侧注释见其
// wafip.go / backoff.go）。server 侧暂保留其私有实现（改动它有独立回归风险），
// 本包供新代码（cmd/webapp）使用；后续可令 server 切换到本包以彻底收敛。
package rotateguard

import (
	"context"
	"log"
	"math/rand/v2"
	"sync"
	"time"
)

// BackoffBase 轮转退避基数（对齐官方 intl CLI computeRequestRetryDelayMs 的
// 500ms 形态）。var 而非 const：测试可置 0 跳过等待（加速轮转测试）。
var BackoffBase = 500 * time.Millisecond

// BackoffCap 轮转退避封顶（8s：轮转上限默认 3 次，实际等待序列 500ms/1s，
// 封顶只约束极端配置下的轮转上限）。
const BackoffCap = 8 * time.Second

// jitterFraction 抖动比例（±25%，对齐 intl CLI delay×(1±0.25) 形态）。
const jitterFraction = 0.25

// JitterDur 给时长施加 ±jitterFraction 的均匀抖动，返回 [d·(1-f), d·(1+f)]。
// d<=0 原样返回（零等待不抖动）。抖动目的是打散多请求同相位重试——WAF 频控
// 按密度判罚，齐步走的固定周期退避会再次聚团。
func JitterDur(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	f := 1 + (rand.Float64()*2-1)*jitterFraction
	out := time.Duration(float64(d) * f)
	if out < 0 {
		return 0
	}
	return out
}

// BackoffAfter 返回第 n 次轮转（0 基：首次失败换号前 n=0）前应等待的退避时长：
// base·2^n 封顶 BackoffCap，再施加 ±25% 抖动。base 置 0（测试）时恒 0。
// 用逐次翻倍而非位移：base 调整后无需同步维护移位上限，溢出由封顶比较兜底。
func BackoffAfter(n int) time.Duration {
	d := BackoffBase
	if d <= 0 {
		return 0
	}
	for k := 0; k < n && d < BackoffCap; k++ {
		d *= 2
		if d <= 0 { // 翻倍溢出成非正数：直接按封顶处理
			return JitterDur(BackoffCap)
		}
	}
	if d > BackoffCap {
		d = BackoffCap
	}
	return JitterDur(d)
}

// SleepCtx 可取消的等待：ctx 取消立即返回 false（客户端断连/优雅停机不必等
// 退避睡醒），等满返回 true。d<=0 立即放行并返回 ctx 是否仍有效。
func SleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// WAFWindow IP 级判定滑动窗 + 激活时长：窗内不同账号命中 WAF 403 达阈值即判
// IP 级拦截，激活同样长（到期自然解除）。var 仅供测试注入短窗（生产恒 60s）。
var WAFWindow = 60 * time.Second

// WAFThreshold 判定阈值：窗内不同 UID 数达到该值激活。取 2——「多号」的最小
// 定义：单号反复 403 永不触发（账号级偶发归软冷却管），两个不同号在 60s 内
// 接连被拦（同一出口 IP）已是 IP 级证据；阈值 2 比 3 更早止损，少放大一次轮转。
const WAFThreshold = 2

// WAFGate WAF IP 级拦截状态机（零值可用）。
type WAFGate struct {
	mu    sync.Mutex
	hits  map[string]time.Time // uid → 最近一次 WAF 403 时刻（判定窗内，惰性剪枝）
	until time.Time            // IP 级拦截激活截止；零值 = 未激活
}

// NoteWaf 记一次某账号的 WAF 403，返回记账后 IP 级拦截是否激活（调用方据此
// fail-fast 终止轮转，优先于退避）。
//   - 已激活（now < until）：不续期、不记账（窗口期内不重置——保守自然解除）→ true；
//   - 未激活：记 hits[uid]=now（同号重复命中覆盖不累计，判定口径是「不同号数」），
//     剪掉窗外的过期命中；不同 UID 数达 WAFThreshold → 激活到 now+WAFWindow
//     （打一条 WARN 供观测），清空判定窗（解除后需全新命中重新判定，不叠旧账）。
func (g *WAFGate) NoteWaf(uid string) bool {
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if now.Before(g.until) {
		return true // 激活期内新命中：不续期（自然解除语义）
	}
	if g.hits == nil {
		g.hits = map[string]time.Time{}
	}
	g.hits[uid] = now
	for u, t := range g.hits {
		if now.Sub(t) > WAFWindow {
			delete(g.hits, u)
		}
	}
	if len(g.hits) >= WAFThreshold {
		g.until = now.Add(WAFWindow)
		log.Printf("WARN: [rotateguard] waf ip-level block: %d accounts hit waf 403 within %s, rotate fail-fast until %s",
			len(g.hits), WAFWindow, g.until.Format(time.RFC3339))
		g.hits = map[string]time.Time{}
		return true
	}
	return false
}

// Active 报告 IP 级拦截是否激活（末端错误文案区分 IP 级/账号级措辞用）。
func (g *WAFGate) Active() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return time.Now().Before(g.until)
}

// Remaining 返回激活剩余时长（未激活返回 0），供可读错误文案使用。
func (g *WAFGate) Remaining() time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	if d := time.Until(g.until); d > 0 {
		return d
	}
	return 0
}
