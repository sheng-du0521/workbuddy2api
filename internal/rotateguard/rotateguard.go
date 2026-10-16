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

// ModelBlockWindow 「模型名在上游不存在」（11102）的判定滑动窗与激活时长。
// 按模型名分桶，故没有 WAFGate 那样的全局单例状态。var 仅供测试注入短窗
// （生产恒 60s，与 WAFWindow 同量级："短时间内换个号再确认一次"的窗口）。
var ModelBlockWindow = 60 * time.Second

// ModelBlockThreshold 判定阈值：窗内**不同账号**数达到该值即判定「上游没有这个
// 模型名」。取 2 —— 「多号」的最小定义。
//
// 为什么必须跨账号：单账号的 11102 只说明"这个号没有该模型"（账号维度，由
// (账号,模型) 负缓存自己管）；**两个不同号**在窗内接连报 11102，才排除掉
// "个别号权限缺失"，指向"模型名本身不存在"（模型维度）。
//
// 阈值 2 不会误伤"真模型但只有部分号有"：选号侧已按 (账号,模型) 负缓存过滤
// （healthyForModel），上一轮撞到的号 6h 内不会再被选中 —— 能接连撞到两个报
// 11102 的号，只可能是"池里对该模型可用的号已耗尽"；此时继续轮转只是把剩余
// 账号挨个犁一遍（每个都白得一条 6h 负缓存）。
const ModelBlockThreshold = 2

// ModelBlockGate 「模型名上游不存在」的跨账号 fail-fast 状态机（零值可用）。
//
// 与 WAFGate 结构同源，但**按模型名分桶**，且激活只作用于该模型 —— 一个打错的
// 模型名激活闸门之后，别的模型完全不受影响；反过来，一个模型名被判定不存在，
// 也不会牵连任何账号（刻意的：11102 指向**模型名**，不指向账号）。
//
// 为什么需要它（2026-10-16 实测教训）：测速时用 `{"model":"x"}`（不存在的模型名）
// 打了约 11 次请求，因 maxAttempts=3，**32/33 个账号**被写入了 6h 的 (账号,"x")
// 负缓存 —— 也就是说**一个假模型名的请求会被放大成"犁全池"**。客户端自带重试、
// 或爬虫随手发随机模型名，可以直接把它放大成出站风暴（本项目最贵的一类事故）。
type ModelBlockGate struct {
	mu  sync.Mutex
	hit map[string]map[string]time.Time // model → (uid → 窗内最近命中时刻)
	act map[string]time.Time            // model → 激活截止（缺席 = 未激活）
}

// NoteBlocked 记一次「账号 uid 对该模型报 11102」，返回该**模型名**当前是否已被
// 判定为「上游不存在」（调用方据此立即终止轮转，不再换号）。
//   - 已激活（now < act[model]）：不续期、不记账 → true（自然解除语义，同 WAFGate）；
//   - 未激活：记 hit[model][uid]=now（同号重复命中**覆盖不累计** —— 判定口径是
//     「不同号数」），惰性剪掉窗外的过期命中；不同 UID 数达 ModelBlockThreshold
//     → 激活到 now+ModelBlockWindow（打一条 WARN 供观测），并清空该模型的判定窗
//     （解除后需全新命中重新判定，不叠旧账）。
func (g *ModelBlockGate) NoteBlocked(model, uid string) bool {
	if model == "" || uid == "" {
		return false
	}
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if until, ok := g.act[model]; ok {
		if now.Before(until) {
			return true // 激活期内新命中：不续期（自然解除语义）
		}
		delete(g.act, model) // 已过期：清掉，避免 map 随"见过的模型名"无限增长
	}
	if g.hit == nil {
		g.hit = map[string]map[string]time.Time{}
	}
	h := g.hit[model]
	if h == nil {
		h = map[string]time.Time{}
		g.hit[model] = h
	}
	h[uid] = now
	for u, t := range h {
		if now.Sub(t) > ModelBlockWindow {
			delete(h, u)
		}
	}
	if len(h) == 0 {
		// 只在窗口被注入成 0/负（测试极端值）时可达：连刚写入的命中都被剪掉了。
		delete(g.hit, model)
		return false
	}
	if len(h) >= ModelBlockThreshold {
		if g.act == nil {
			g.act = map[string]time.Time{}
		}
		g.act[model] = now.Add(ModelBlockWindow)
		log.Printf("WARN: [rotateguard] model %q rejected by %d accounts within %s: upstream reports this model does not exist, rotate fail-fast until %s",
			model, len(h), ModelBlockWindow, g.act[model].Format(time.RFC3339))
		delete(g.hit, model)
		return true
	}
	return false
}

// Active 该模型当前是否处于「判定为不存在」的激活期（观测/测试用）。
func (g *ModelBlockGate) Active(model string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	until, ok := g.act[model]
	return ok && time.Now().Before(until)
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
