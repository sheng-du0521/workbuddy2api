// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// ===========================================================================
// 健康检测的「对话探活」（v0.24.3）
// ===========================================================================
//
// **为什么加这个**（用户要求 + 2026-10-13 封号事故的教训）：
//
// 本包此前的立场是**刻意不做** chat 探测 —— 文件头写明的理由是「真发 chat 会烧积分，
// 且高频检测本身就是风控特征」。这个取舍在 2026-10-13 被证伪了：
// 30 个账号在**余额探活全部通过**的情况下，实际已被上游风控判定为「请求非法(11140)」——
// 余额接口（billing 域）健康 ≠ chat 通道可用。只探余额会给出一个**虚假的安心结论**，
// 而事故中最难的部分恰恰是「号看起来是好的」。
//
// 所以现在：**默认走一遍真实对话流程**，把「能不能干活」也纳入判定。
// 代价（如实记录）：每个账号多一次出站、并消耗极少量积分（max_tokens=1）。
// 收益：能暴露「凭证有效但对话通道被风控/不可用」这一整类状态。
//
// 成本与风险控制（三件都必须成立，否则探测本身会变成新的风控特征）：
//  1. 走**最便宜的模型**（kv `health_probe_model` 可指定；缺省取内置兜底列表首个）；
//  2. 请求体极小：单轮、内容 "hi"、max_tokens=1、非流式（一次读完即判定）；
//  3. 计入**探活总闸**（probeGate）：与余额查询共享全局并发上限与单账号最小间隔。
//
// 刻意**不**记账：这是平台侧自检，不是用户请求 —— 不写 usage_logs、不扣用户点数。
// ===========================================================================

const (
	// kvHealthProbeChat 对话探活开关：kv 值 "0" = 关闭；缺省（无记录）= **开启**。
	kvHealthProbeChat = "health_probe_chat"
	// kvHealthProbeModel 对话探活使用的模型（缺省取 fallbackModels 首个）。
	kvHealthProbeModel = "health_probe_model"
)

// hcChatProbeTimeout 单次对话探活的超时。
//
// 取 20s：一次 max_tokens=1 的对话在正常网络下是亚秒级，20s 只用于兜住
// 「上游卡住」—— 探活必须**有界**，否则一个卡住的号会把整批检测拖到超时。
const hcChatProbeTimeout = 20 * time.Second

// hcChatProbeEnabled 对话探活是否启用（**默认开启**）。
//
// 默认开启是刻意的：事故的盲区正是「只探余额」，把它做成选配等于默认保留盲区。
// 关掉只在「积分极度紧张」或「怀疑探活本身触发风控」时用（kv 写 "0"）。
func (a *app) hcChatProbeEnabled() bool {
	v, _ := a.store.GetKV(kvHealthProbeChat)
	return strings.TrimSpace(v) != "0"
}

// hcChatProbeModel 选定探活模型：kv 指定 > 内置兜底列表首个。
//
// 刻意**不**去拉 /v1/models 来挑：那是一次额外的出站请求，而探活的目的恰恰是
// 「少打扰上游」。fallbackModels 里的名字是内置的、已知可用的形态。
func (a *app) hcChatProbeModel() string {
	if v, _ := a.store.GetKV(kvHealthProbeModel); strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if len(fallbackModels) > 0 {
		return fallbackModels[0]
	}
	return ""
}

// hcChatProbeResult 对话探活结果。
type hcChatProbeResult struct {
	OK        bool
	Model     string
	LatencyMs int64
	// Note 给人看的说明（失败原因 / 跳过原因）。
	Note string
	// Err 已分类的上游错误（*upstream.Error）—— 供 hcVerdictFromErr 复用同一套判定，
	// 刻意不在这里另写一份分类逻辑（「同一件事两个实现」是本项目反复踩过的坑）。
	Err error
}

// hcChatProbe 发一次**极小**的真实对话请求，验证 chat 通道能否正常返回。
//
// 返回的 Err 是**已分类**的（ChatStreamContext 内部完成分类，见其注释），
// 调用方直接交给 hcVerdictFromErr 即可，不必二次分类。
func (a *app) hcChatProbe(au *auth.Auth) hcChatProbeResult {
	var out hcChatProbeResult
	if au == nil {
		out.Note = "凭证不可用，跳过对话探活"
		return out
	}
	model := a.hcChatProbeModel()
	out.Model = model
	if model == "" {
		out.Note = "未取到可用模型，跳过对话探活"
		return out
	}

	body, err := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		"max_tokens": 1,
		"stream":     false,
	})
	if err != nil {
		out.Note = "构造探活请求失败：" + err.Error()
		return out
	}

	ctx, cancel := context.WithTimeout(context.Background(), hcChatProbeTimeout)
	defer cancel()

	// 会话头族（v0.24.3 实测得出，**不是可选装饰**）：
	// 真实客户端的 chat 请求总是带 X-Conversation-ID / X-Conversation-Request-ID /
	// X-Trace-ID（值由 body 里的会话字段提取）。探活没有入站请求，最初三个都留空 ——
	// 实测这种「无会话标识的 chat 请求」被上游直接判 11140 request illegal，
	// 于是探活会把**健康账号也报成失败**（假阳性），比不做探活更糟。
	// 现在按 uid 稳定派生（不用随机值：随机值会让每次探活都像"新会话"，
	// 而这里要模拟的是「同一账号的常规客户端」）。
	meta := upstream.ChatMeta{ConversationRequestID: newRandomID(12)}
	if au.UID != "" {
		meta.ConversationID = upstream.AccountSessionID(au.UID)
		meta.ConversationRequestID = upstream.AccountSessionID(au.UID)
		meta.TraceID = upstream.AccountMachineID(au.UID)
	}

	start := time.Now()
	rc, status, respBody, cerr := a.proxy.uc.ChatStreamContext(ctx, au, body, "", meta)
	out.LatencyMs = time.Since(start).Milliseconds()
	if rc != nil {
		_ = rc.Close()
	}

	if cerr != nil {
		out.Err = cerr
		out.Note = "对话请求失败：" + cerr.Error()
		return out
	}
	// 防御：ChatStreamContext 在 ≥400 时应返回已分类 err；若它判定为 ErrNone
	// （注释说「理论上不存在」），这里按状态码自行包一层，绝不让失败被读成成功。
	if status >= 400 {
		out.Err = &upstream.Error{
			Kind:   upstream.Classify(status, string(respBody)),
			Status: status,
			Msg:    truncateForReason(string(respBody)),
		}
		out.Note = "对话请求返回 HTTP " + itoa(status)
		return out
	}
	out.OK = true
	return out
}

// ===========================================================================
// 探活总闸（probeGate）
// ===========================================================================
//
// **为什么必须有**（2026-10-13 事故的直接产物）：
// 管理后台的账号池页会为**每一张账号卡片**发一次出站查积分请求
// （admin.html 的 creditPanel → /api/admin/accounts/{uid}/resources →
// writeCreditDetail → 上游）。59 张卡片 = 59 个**并发**出站、同一个出口 IP、
// 几秒内打完 —— 上游风控当场返回 11140，30 个号被误判停用。
//
// 这是「用户打开页面看一眼」就能触发的风控事件，所以必须在**服务端**也兜一道：
// 出站请求不能无界并发，同一账号也不该被反复探。
//
// 职责边界：本闸只管**探活类**出站（查积分 / 健康检测 / 对话探活），
// **不含**用户业务请求（/v1/* 反代）—— 后者有自己的一套限流与在途名额语义。
type probeGate struct {
	mu       sync.Mutex
	inflight int
	last     map[string]time.Time
}

const (
	// probeMaxInflight 同时进行的探活出站上限。
	// 取 3：小到不会形成「几十个并发打同一出口」的特征，大到手动刷新几行也不会串行等待。
	probeMaxInflight = 3
	// probeMinInterval 同一账号两次探活的最小间隔。
	// 取 30s：比软冷却（60s）短，够挡住「前端卡片反复渲染 / 页面来回切换」造成的重复出站，
	// 又不会让运维点一次「刷新」被无谓拒绝（正常手动操作远慢于 30s 一次）。
	probeMinInterval = 30 * time.Second
)

// errProbeThrottled 被探活闸拦下的错误（调用方据此回一个「请稍候」而不是报故障）。
var errProbeThrottled = errors.New("探活过于频繁：同一账号 30 秒内只查一次（或当前并发探活已满），请稍候重试")

// acquire 申请一次探活出站许可。成功返回 release（必须 defer 调用）。
//
// 两条拒绝条件（都会返回 errProbeThrottled，不做排队）：
//   - 全局在途已满 —— 排队只会把并发风暴推迟，不如直接拒绝让前端退避；
//   - 同一账号在 probeMinInterval 内刚探过 —— 重复探活对同一个号没有新增信息量。
func (g *probeGate) acquire(uid string) (func(), error) {
	// nil receiver = 未装配闸（单元测试 / 极简装配）：不限制，直接放行。
	//
	// 用 nil receiver 而不是在各调用点判 `a.probes != nil`：闸是**可选基础设施**，
	// 生产必装配（见 newApp），而"忘了判 nil"会 panic 在探活路径上 —— 那正是用户
	// 点一下页面就会走到的路径。把安全性放在类型自己身上，调用方就不必记住这件事。
	if g == nil {
		return func() {}, nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.inflight >= probeMaxInflight {
		return nil, errProbeThrottled
	}
	if uid != "" && g.last != nil {
		if at, ok := g.last[uid]; ok && time.Since(at) < probeMinInterval {
			return nil, errProbeThrottled
		}
	}
	if g.last == nil {
		g.last = make(map[string]time.Time)
	}
	if uid != "" {
		g.last[uid] = time.Now()
	}
	g.inflight++
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.inflight--
			g.mu.Unlock()
		})
	}, nil
}

// gc 回收过期的「上次探活时刻」条目，避免 map 只增不减。
//
// 与 rateLimiter 的 maybeGC 同一理由：长期运行的服务里，任何「按 key 记录时刻」
// 的结构都必须有回收路径（每个被探过的 uid 都会留一条，号池会有增删）。
func (g *probeGate) gc(now time.Time) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for k, at := range g.last {
		if now.Sub(at) >= probeMinInterval {
			delete(g.last, k)
		}
	}
}
