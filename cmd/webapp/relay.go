// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 通道转发与计费（v0.31.68，需求 P2）。
//
// # 它是什么
//
// 号池之外的**第二条供给**：第三方模型 API / 中转站，标准 OpenAI 兼容协议。
// 分发规则是「merge，号池优先」——
//   - 号池有该模型的可用号 → 走号池（原路径，一行不改）；
//   - 号池没有 → 落到通道（本文件）。
//
// # 三条刻意的决定
//
//  1. **号池优先用"同源判定"**：`SelectableUIDsForModel` 与生产选号同一个函数。
//     用别的口径（比如"号池目录里有这个模型名"）会出现"面板说有、选不到、又不走通道"
//     的双重落空 —— 那种故障最难查，因为两边都"看起来正常"。
//
//  2. **不对请求做 PrepareOutbound**：那套改写（指纹 / 角色归一 / efforts 降级 /
//     prompt_cache_key）全是给 workbuddy 上游准备的。对第三方照搬只会被拒或降智 ——
//     这是"复用得好"与"复用错了"的分界线。
//
//  3. **计费复用同一条结算链**：通道也把用量折算成"积分"（tokens/1000 × 配置单价），
//     然后走 `billing.Settle` 的同一个公式 `PointToMicro(积分 × 倍率)`。
//     不另写一套扣费 —— 钱只能有一个口径（审查 F02 的教训）。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/billing"
)

const (
	// relayMaxAttempts 一次请求最多试几条通道。
	// 与号池的 maxAttempts 同一个理由：试遍全表会把一次客户端请求放大成 N 次出站。
	relayMaxAttempts = 3

	// relayChatPath OpenAI 兼容对话端点（相对 base_url）。
	relayChatPath = "/chat/completions"
)

// relayClientStream 流式用的客户端：**不设总超时**。
//
// 设了总超时就会掐断长回答（与 chatClientFor 的 why 完全一样）。
// 客户端断开时 r.Context() 取消，连接随之释放，不会泄漏。
var relayClientStream = &http.Client{Transport: http.DefaultTransport}

// relayClientJSON 非流式用的客户端：必须有总超时。
// 非流式是一次"请求-应答"，上游挂死时没有别的兜底，会一直占着 goroutine。
var relayClientJSON = &http.Client{Timeout: 180 * time.Second, Transport: http.DefaultTransport}

// serveRelay 尝试用通道处理本次请求。
//
// 返回 (handled, settled)：
//   - handled=false 表示"不该走通道"（号池有号 / 代挂 Key / 没有候选），
//     此时**没有写过响应**，调用方应继续走号池路径；
//   - handled=true 表示响应已写（成功或失败都算），调用方直接 return；
//   - settled 告知调用方"钱已结清"，据此决定是否释放预扣（未结清则由 defer 退）。
func (p *Proxy) serveRelay(w http.ResponseWriter, r *http.Request, payload map[string]any,
	model string, u *User, key *APIKey, resv *Reservation, stickyHash string,
	start time.Time, wantStream bool) (handled, settled bool) {

	// 代挂自用 Key 不能走通道：它买的是"号主自己的账号"，通道是平台的钱买上游。
	// 放过去等于用平台成本给某个号主免费用（且不产生任何分成）。
	if key != nil && key.Kind == KeyKindHost {
		return false, false
	}
	// 号池优先：**号池认识这个模型名**就完全不走通道。
	//
	// 这里不能用 `SelectableUIDsForModel` 非空来判（最初就是这么写的，是错的）：
	// 池内可用性只看 (账号,模型) 冷却，不认识"这个账号到底支不支持这个模型" ——
	// 于是任何模型名都会返回"有号"，relay 永远走不到（看起来像"通道配了不用"）。
	//
	// 正确判据是"号池的模型目录里有没有这个名字"，与 /v1/models 的可见性规则**同源**
	// （见 relayVisibleModels：号池有就不追加通道条目）。
	if known, decided := p.poolKnowsModel(model); !decided || known {
		return false, false
	}
	cands, err := p.store.relayCandidates(model)
	if err != nil {
		log.Printf("WARN: [relay] 读取通道配置失败（本次仍走号池路径）：%v", err)
		return false, false
	}
	if len(cands) == 0 {
		return false, false
	}

	ratio, ratioSrc := p.ratioForRelay(model)
	var lastErr string
	// upstreamStatus 最后一次**上游 HTTP 状态码**（0 = 没拿到响应，网络错误/超时）。
	// 全失败时它是"该回什么状态、该怎么解释"的唯一依据（v0.31.95）。
	upstreamStatus := 0
	tried := 0
	for _, v := range cands {
		if tried >= relayMaxAttempts {
			break
		}
		tried++
		resp, reqErr := p.relayRequest(r.Context(), payload, v, wantStream)
		if reqErr != nil {
			lastErr = v.UpstreamName + "：" + reqErr.Error()
			continue
		}
		if resp.StatusCode >= 400 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<10))
			resp.Body.Close()
			upstreamStatus = resp.StatusCode
			lastErr = fmt.Sprintf("%s 返回 %d：%s", v.UpstreamName, resp.StatusCode,
				truncateStr(strings.TrimSpace(string(body)), 200))
			// 4xx 也不在这里判"这条通道坏了"：401 是 key 错（要人修配置），
			// 429 是限流（换下一条是合理的）—— 都不该改任何持久状态。
			continue
		}

		// 响应头透出实际服务的通道（与 X-Wb-Account 同语义）：
		// 排障时"这次是谁在答"必须一眼可见，否则号池/通道两条链路分不清。
		w.Header().Set("X-Wb-Upstream", v.UpstreamName)
		var ui usageInfo
		var ferr error
		if wantStream {
			ui, _, ferr = streamSSEHint(w, resp, nil)
		} else {
			ui, ferr = relayForwardJSON(w, resp)
		}
		resp.Body.Close()

		if ferr != nil {
			log.Printf("WARN: [relay] upstream stream interrupted channel=%s model=%s: %v",
				v.UpstreamName, model, ferr)
			// 与号池同口径：中断也要留一条流水（0 费），否则"执行到一半断了"查不到。
			p.settleUsageScoped(u, key, model, "", usageInfo{LatencyMS: time.Since(start).Milliseconds()},
				resv, "interrupted", "relay stream interrupted: "+ferr.Error(), stickyHash, tried, billing.ScopeRelay)
			return true, false
		}

		// 折算积分：通道按 token 计价，平台内部一律用"积分"这一个单位。
		// 这样结算公式、流水字段、倍率语义都与号池完全一致（不需要第二套账）。
		tokens := ui.Total()
		ui.Credit = float64(tokens) / 1000.0 * v.CreditPer1k
		ui.HasCredit = true // 这是**我们自己折算**的，不是上游回报；见下方注释
		ui.LatencyMS = time.Since(start).Milliseconds()

		status := "ok"
		errMsg := ""
		if tokens == 0 {
			// 上游没回报 usage → 无法计费。**不能**按"估算"扣钱：通道的估算没有基准
			// （对方单价是按 token 的，而 token 数正是缺的那个）。记流水留痕、收 0 费、
			// 打 WARN 让人去查（多数是没支持 stream_options.include_usage）。
			status = "no_usage"
			errMsg = "upstream did not return usage; charged 0"
			log.Printf("WARN: [relay] 上游未返回 usage（本次 0 费）channel=%s model=%s；"+
				"检查该中转站是否支持 stream_options.include_usage", v.UpstreamName, model)
		}
		ok, charged := p.settleUsageScoped(u, key, model, "", ui, resv, status, errMsg,
			stickyHash, tried, billing.ScopeRelay)
		log.Printf("INFO: [relay] channel=%s model=%s→%s tokens=%d credit=%.4f ratio=%.3f(%s) charged=%.4f点",
			v.UpstreamName, model, v.RemoteModel, tokens, ui.Credit, ratio, ratioSrc, MicroToPoint(charged))
		p.noteLive(r, liveEvent{
			At: time.Now().Unix(), User: liveUserName(u), Model: model, OK: true,
			Tokens: tokens, Charged: MicroToPoint(charged),
			Status: http.StatusOK, ElapsedMS: time.Since(start).Milliseconds(),
		})
		return true, ok
	}

	// 全部通道都失败：按**最后一次上游状态码**给出可诊断的结论（v0.31.95）。
	//
	// 以前一律 502 + "check the channel config"：上游 429 限流、
	// 400「该模型不支持 chat/completions」全被说成"你配置错了"，
	// 用户只会去反复核对 base_url / key（2026-10-21 实测：图像模型走对话就是这个形态）。
	// 不回 404：模型是配置过的（只是通道不可用），说"没有这个模型"会把运维引到
	// "去查模型名"这个错误方向。
	status, failKind, hint := relayFailureVerdict(upstreamStatus)
	w.Header().Set("X-Wb-Fail-Kind", failKind)
	log.Printf("WARN: [relay] 所有通道均失败 model=%s（%d 条尝试，upstream_status=%d → %d %s）：%s",
		model, tried, upstreamStatus, status, failKind, lastErr)
	p.noteLive(r, liveEvent{
		At: time.Now().Unix(), User: liveUserName(u), Model: model, OK: false,
		Err: "通道不可用：" + lastErr, Status: status,
		ElapsedMS: time.Since(start).Milliseconds(),
	})
	writeOpenAIErrorHint(w, status, "upstream channel error: "+lastErr, hint)
	return true, false
}

// relayFailureVerdict 通道全失败时该回什么状态、给什么解释（纯函数，便于单测）。
//
// 输入只有"最后一次拿到的上游 HTTP 状态码"（0 = 没拿到响应）。三类失败必须分开说：
//
//	401/403  通道的 key 无效/无权限 —— 确实是配置问题，但错在**通道侧**，不是用户请求
//	429      限流 —— 可重试，用户什么都不用改（SDK 对 429 会退避重试）
//	其它 4xx 请求本身跑不通 —— 典型是"图像模型走 chat/completions"或模型名映射不对
//	5xx / 0  上游不可用 / 连不上
func relayFailureVerdict(upstreamStatus int) (status int, kind, hint string) {
	switch {
	case upstreamStatus == http.StatusUnauthorized || upstreamStatus == http.StatusForbidden:
		return http.StatusBadGateway, "relay_auth",
			"the channel rejected our API key (401/403): this is a channel-side configuration " +
				"problem, not your request. An admin needs to update that channel's key/permissions"
	case upstreamStatus == http.StatusTooManyRequests:
		return http.StatusTooManyRequests, "relay_rate_limited",
			"the upstream rate-limited this channel (429): nothing to fix on your side, retry later; " +
				"if it keeps happening, lower that channel's priority or add another channel"
	case upstreamStatus >= 400 && upstreamStatus < 500:
		return upstreamStatus, "relay_rejected",
			"the upstream rejected this request (not a gateway failure): typically the model is not " +
				"served on /chat/completions (e.g. an image model) or its remote name mapping is wrong"
	case upstreamStatus == 0:
		return http.StatusBadGateway, "relay_unreachable",
			"cannot reach the upstream (timeout / network): check the channel base_url and the " +
				"server's egress"
	default:
		return http.StatusBadGateway, "relay_unavailable",
			"the upstream is temporarily unavailable (5xx): retry later; if it persists check that " +
				"channel's upstream status"
	}
}

// relayRequest 构造并发出一次通道请求（**不写响应**，失败可以换下一条）。
func (p *Proxy) relayRequest(ctx context.Context, payload map[string]any,
	v UpstreamModelView, wantStream bool) (*http.Response, error) {

	// 复制一份再改：payload 是调用方的 map，改它会污染"后面可能还要走号池"的分支。
	// （虽然当前判定下不会两条路径都走，但这种"共享 map 就地改"的隐患不值得留。）
	body := make(map[string]any, len(payload)+1)
	for k, val := range payload {
		body[k] = val
	}
	// 用户可见名 → 上游真实名（中转站常用自己的命名，如 gpt-4o-mini-relay）。
	body["model"] = v.RemoteModel
	body["stream"] = wantStream
	if wantStream {
		// 流式必须显式要 usage：否则结算拿不到 token 数（只能 0 费，等于白送）。
		// 这是 OpenAI 的 stream_options 扩展，主流中转站都支持；不支持时
		// 会走上面的 no_usage 分支（留痕 + 0 费，不会算错钱）。
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	blob, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(v.BaseURL, "/") + relayChatPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(blob))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+v.APIKey)
	req.Header.Set("Accept", "application/json")
	if wantStream {
		req.Header.Set("Accept", "text/event-stream")
		return relayClientStream.Do(req)
	}
	return relayClientJSON.Do(req)
}

// relayForwardJSON 非流式：读完整响应体、取出 usage、原样写回客户端。
//
// **为什么原样写回**：客户端看到的内容不该因为"经过通道"而变样（字段顺序、
// 思考字段、供应商私有字段都可能有用）。我们只旁路读一次 usage。
func relayForwardJSON(w http.ResponseWriter, resp *http.Response) (usageInfo, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return usageInfo{}, err
	}
	var ui usageInfo
	var probe struct {
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &probe) == nil && probe.Usage != nil {
		ui.PromptTokens = probe.Usage.PromptTokens
		ui.CompletionTokens = probe.Usage.CompletionTokens
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	return ui, nil
}

// poolKnowsModel 号池是否认识这个模型名。
//
// 返回 (known, decided)：
//   - decided=false 表示**没有任何依据**（模型缓存与池内观测都为空，如进程刚起、
//     账号一个都没同步）。此时必须按"号池认识"处理 —— 数据缺失不等于"号池没有"，
//     否则一次重启就会把全部流量推到第三方通道（真金白银，且没人会立刻发现）。
func (p *Proxy) poolKnowsModel(model string) (known, decided bool) {
	ids := cachedModelIDs() // 只含号池：通道模型在 relayIDs 里，混进来会自我死锁
	if len(ids) == 0 {
		ids = p.pool.ObservedModels()
	}
	if len(ids) == 0 {
		return true, false
	}
	for _, id := range ids {
		if id == model {
			return true, true
		}
	}
	return false, true
}
