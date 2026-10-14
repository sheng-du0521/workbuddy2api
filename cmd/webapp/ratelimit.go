// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// settingRateLimitRPM 客户端维度限流：每分钟请求数上限（0 = 不限，默认）。
//
// 与 users.go 的注册/登录 IP 限速互补：那层护「入口」（防爆破/灌注册），
// 这层护「用量」（防单个 Key 把号池打穿）。默认 0 表示不限速——
// 升级后行为与旧版完全一致，管理员显式开启才生效。
const settingRateLimitRPM = "rate_limit_rpm"

// clientLimiter 客户端维度滑动窗口限流器。
//
// 维度：已鉴权的 API Key 按 Key ID 计数（换 Key 即换桶，符合「客户端」语义）；
// 管理密钥通道无 Key 实体，退化为按客户端 IP。
// 选滑动窗口而非令牌桶：口径直观（「最近 60 秒内 N 次」），且与注册限速同一实现范式，
// 运维一眼能对上日志。代价是每桶存一份时间戳切片——内网规模（几十 Key）可忽略。
type clientLimiter struct {
	mu     sync.Mutex
	hits   map[string][]time.Time
	window time.Duration
	lastGC time.Time

	allowed atomic.Uint64 // 放行总数（/metrics 用）
	blocked atomic.Uint64 // 拦截总数（/metrics 用）
}

func newClientLimiter() *clientLimiter {
	return &clientLimiter{hits: map[string][]time.Time{}, window: time.Minute}
}

// allow 记一次访问并返回是否放行与建议等待时长。
// rpm <= 0 恒放行（不限速，返回 ok=true）。
func (l *clientLimiter) allow(key string, rpm int) (ok bool, retryAfter time.Duration) {
	if rpm <= 0 {
		return true, 0
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.maybeGC(now)

	cut := now.Add(-l.window)
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= rpm {
		l.hits[key] = kept
		l.blocked.Add(1)
		// 最早一次命中滑出窗口即恢复额度。
		wait := kept[0].Add(l.window).Sub(now)
		if wait < 0 {
			wait = 0
		}
		return false, wait
	}
	l.hits[key] = append(kept, now)
	l.allowed.Add(1)
	return true, 0
}

// maybeGC 清掉完全过期的桶（低频：每分钟最多一次）。
// 不做的话，长期运行下「只用过一次就再不出现的 Key/IP」会在 map 里永久堆积。
func (l *clientLimiter) maybeGC(now time.Time) {
	if now.Sub(l.lastGC) < l.window {
		return
	}
	l.lastGC = now
	cut := now.Add(-l.window)
	for k, ts := range l.hits {
		live := false
		for _, t := range ts {
			if t.After(cut) {
				live = true
				break
			}
		}
		if !live {
			delete(l.hits, k)
		}
	}
}

// rateLimitRPM 读取当前生效的每分钟上限（0 = 不限）。
func (a *app) rateLimitRPM() int {
	v, err := a.store.GetKV(settingRateLimitRPM)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// enforceRateLimit 在请求进入上游前执行客户端维度限流。
// 超限时写 429（OpenAI 风格错误 + Retry-After）并返回 false。
func (a *app) enforceRateLimit(w http.ResponseWriter, bucket string) bool {
	rpm := a.rateLimitRPM()
	if rpm <= 0 {
		return true
	}
	ok, wait := a.limiter.allow(bucket, rpm)
	if ok {
		return true
	}
	secs := int(math.Ceil(wait.Seconds()))
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	writeOpenAIError(w, http.StatusTooManyRequests,
		"rate limit exceeded: "+strconv.Itoa(rpm)+" requests/minute, retry in "+strconv.Itoa(secs)+"s")
	return false
}
