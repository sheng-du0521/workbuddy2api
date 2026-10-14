// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/egress"
	"workbuddy2api/internal/pool"
)

// ctxAPIKeyKey 请求上下文里的 *APIKey。
const ctxAPIKeyKey ctxKey = "wb_apikey"

// apiKeyFrom 取上下文里的 API Key。
func apiKeyFrom(r *http.Request) *APIKey {
	k, _ := r.Context().Value(ctxAPIKeyKey).(*APIKey)
	return k
}

// ---------- 定价 ----------

// 定价设置键名。
const (
	// settingMultiplier 计费倍率：点 / 号池积分（v0.12.0 起口径名统一为「号池积分」，
	// 数值语义不变 —— 上游 usage.credit 即账号被实扣的积分数）。
	settingMultiplier      = "pricing_multiplier"
	settingRegisterEnabled = "register_enabled" // 1 开 0 关
)

// 默认倍率：消耗 1 号池积分 = 扣 1 点。
const defaultMultiplier = 1.0

// ---------- 计费结算 ----------

// usageInfo 一次请求的上游用量。
type usageInfo struct {
	PromptTokens     int
	CompletionTokens int
	Credit           float64
	HasCredit        bool
}

// Total 总 token 数。
func (u usageInfo) Total() int { return u.PromptTokens + u.CompletionTokens }

// ---------- 预扣估算 ----------

// 预扣相关设置键。
const (
	settingReserveRate = "reserve_rate_per_1k" // 兜底单价：点 / 1k token（0 = 不预扣）
	settingReserveMax  = "reserve_max_point"   // 单笔预扣上限（点，0 = 不限）
)

// 预扣估算参数。
const (
	defaultMaxTokens = 4096 // 客户端未声明 max_tokens 时按此上界估
	minReserveMicro  = 1    // 有单价时至少预扣 1 微点，避免估成 0 形同虚设
	// 观测单价的持久化前缀（跨重启保留，否则重启后预扣能力归零）。
	kvModelCostPrefix = "model_cost_1k:"
)

// estimateReserveMicro 估算一次请求的预扣额（微点）。
//
// 口径：按「prompt 上界 + max_tokens 上界」× 单价 × 倍率，宁可高估。
// 单价来源优先级：
//  1. 本进程观测到的该模型实测单价（settleUsage 每次成功调用后取历史最大值）
//  2. 管理员配置的兜底单价 reserve_rate_per_1k
//
// 两者都没有（新部署、池内还没跑过一轮对话）→ 返回 0，退化为事后结算，
// 不做预扣。这是有意为之：宁可不保护，也不能拿瞎猜的单价去误拒用户。
func (p *Proxy) estimateReserveMicro(payload map[string]any, model string) int64 {
	per1kPoint, ok := p.reserveUnitPrice(model)
	if !ok || per1kPoint <= 0 {
		return 0
	}
	tokens := estimatePromptTokens(payload) + requestMaxTokens(payload)
	if tokens <= 0 {
		return 0
	}
	micro := PointToMicro(per1kPoint * float64(tokens) / 1000.0)
	if micro < minReserveMicro {
		micro = minReserveMicro
	}
	if cap := p.reserveCapMicro(); cap > 0 && micro > cap {
		micro = cap
	}
	return micro
}

// reserveUnitPrice 取该模型用于预扣的单价，单位统一为「点 / 1k token」。
//
// 两个来源的原始单位不同，务必区分（v0.12.0 修复了这里的历史 bug）：
//  1. 观测单价 costPer1k 存的是「**积分** / 1k token」（observeCost 只除 token，
//     不乘倍率），必须 × mult 才是点；
//  2. 兜底单价 reserve_rate_per_1k 是管理员在设置页按「**点** / 千 token」填的
//     （标签原文如此），本身就是终价，**不能再乘倍率** —— 否则双重计倍率：
//     倍率 0.1 时冻结额被缩小 10 倍（保护形同虚设），倍率 10 时放大 10 倍
//     （余额略低即被 402 误拒）。
func (p *Proxy) reserveUnitPrice(model string) (float64, bool) {
	mult := p.store.PricingMultiplier()
	p.costMu.RLock()
	per1k, ok := p.costPer1k[model]
	p.costMu.RUnlock()
	if ok && per1k > 0 {
		return per1k * mult, true // 积分/1k → 点/1k
	}
	if v, err := p.store.GetKV(settingReserveRate); err == nil && strings.TrimSpace(v) != "" {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f > 0 {
			return f, true // 已是点/1k，直接用
		}
	}
	return 0, false
}

// reserveCapMicro 单笔预扣上限（微点），0 = 不限。
func (p *Proxy) reserveCapMicro() int64 {
	v, err := p.store.GetKV(settingReserveMax)
	if err != nil || strings.TrimSpace(v) == "" {
		return 0
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || f < 0 {
		return 0
	}
	return PointToMicro(f)
}

// observeCost 记录一次实测单价（credit / 1k token），取历史最大值。
//
// 取最大值而非平均：预扣宁高勿低，低估就意味着防不住刷爆。
// 同时落 kv 表，使重启后预扣能力不丢。
func (p *Proxy) observeCost(model string, per1k float64) {
	if model == "" || per1k <= 0 {
		return
	}
	p.costMu.Lock()
	old, seen := p.costPer1k[model]
	if seen && old >= per1k {
		p.costMu.Unlock()
		return
	}
	p.costPer1k[model] = per1k
	p.costMu.Unlock()
	_ = p.store.SetKV(kvModelCostPrefix+model, strconv.FormatFloat(per1k, 'f', 6, 64))
}

// loadObservedCosts 启动时把持久化的实测单价读回内存。
func (p *Proxy) loadObservedCosts() {
	rows, err := p.store.ListKV(kvModelCostPrefix)
	if err != nil {
		return
	}
	p.costMu.Lock()
	defer p.costMu.Unlock()
	for k, v := range rows {
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil || f <= 0 {
			continue
		}
		p.costPer1k[strings.TrimPrefix(k, kvModelCostPrefix)] = f
	}
}

// observedModelCount 已观测到实测单价的模型数。
// 为 0 表示预扣尚未生效（退化为事后结算），管理员可在设置页看到这个信号。
func (p *Proxy) observedModelCount() int {
	p.costMu.RLock()
	defer p.costMu.RUnlock()
	return len(p.costPer1k)
}

// estimatePromptTokens 粗略估算 prompt token 数（取保守上界）。
// 中文 UTF-8 每字 3 字节约 1 token，英文每 3 字节约 1 token，
// 故统一用 bytes/3 对两种语言都贴近上界。
func estimatePromptTokens(payload map[string]any) int {
	msgs, _ := payload["messages"].([]any)
	total := 0
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		if mm == nil {
			continue
		}
		total += 4 // 每条消息的角色/分隔符结构开销
		switch c := mm["content"].(type) {
		case string:
			total += len(c)/3 + 1
		case []any:
			for _, part := range c {
				pm, _ := part.(map[string]any)
				if pm == nil {
					continue
				}
				if s, ok := pm["text"].(string); ok {
					total += len(s)/3 + 1
				}
				if _, ok := pm["image_url"]; ok {
					total += 1000 // 图片按固定量保守估
				}
			}
		}
	}
	return total
}

// requestMaxTokens 取客户端声明的输出上限，缺省给保守默认值。
func requestMaxTokens(payload map[string]any) int {
	for _, k := range []string{"max_completion_tokens", "max_tokens"} {
		if v, ok := payload[k].(float64); ok && v > 0 {
			if v > 1<<20 {
				return 1 << 20
			}
			return int(v)
		}
	}
	return defaultMaxTokens
}

// ---------- 消费闸 ----------

// quotaExceeded 检查用户消费闸，超限返回给用户看的提示；未超返回空串。
//
// 消费闸是「防跑飞」的第二道闸（第一道是余额本身）：
// 余额可能一次充很多，但日/月上限能把单日突发消费按住。
func (a *app) quotaExceeded(u *User) string {
	if u == nil {
		return ""
	}
	now := time.Now()
	if u.DailyLimitMicro > 0 {
		start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
		if used, err := a.store.UserUsedSince(u.ID, start); err == nil && used >= u.DailyLimitMicro {
			return fmt.Sprintf("daily quota exceeded (%s/%s points used today)",
				formatPoints(used), formatPoints(u.DailyLimitMicro))
		}
	}
	if u.MonthlyLimitMicro > 0 {
		start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).Unix()
		if used, err := a.store.UserUsedSince(u.ID, start); err == nil && used >= u.MonthlyLimitMicro {
			return fmt.Sprintf("monthly quota exceeded (%s/%s points used this month)",
				formatPoints(used), formatPoints(u.MonthlyLimitMicro))
		}
	}
	return ""
}

// quotaUsage 返回用户当前的日 / 月已用量（微点），供前端展示。
func (a *app) quotaUsage(u *User) (dayUsed, monthUsed int64) {
	if u == nil {
		return 0, 0
	}
	now := time.Now()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).Unix()
	dayUsed, _ = a.store.UserUsedSince(u.ID, dayStart)
	monthUsed, _ = a.store.UserUsedSince(u.ID, monthStart)
	return dayUsed, monthUsed
}

// formatPoints 微点 → 展示字符串（去掉无意义的小数尾零）。
func formatPoints(micro int64) string {
	return strconv.FormatFloat(MicroToPoint(micro), 'f', -1, 64)
}

// ---------- API Key 中间件 ----------

// apiKeyGuard /v1/* 的鉴权：用户 API Key → 用户 → 余额检查。
// 取代原「全局单 key」模型（多租户核心改造）。
func (a *app) apiKeyGuard(next http.HandlerFunc) http.HandlerFunc {
	return a.apiKeyGuardOpt(next, true)
}

// apiKeyGuardNoBalance 同 apiKeyGuard，但**跳过余额与消费闸校验**。
//
// 专用于 /v1/models 这类「只读元信息、不产生消费」的接口。
// 为什么必须豁免：绝大多数 OpenAI 兼容客户端在「添加服务商」时**第一件事就是拉
// /v1/models**。若这里因余额不足返回 402，客户端会直接判定「连接失败」，
// 用户看到的是连接错误、完全看不到真正原因（余额不足），于是反复检查
// Base URL 与密钥 —— 实测踩到过这个坑（本系统测试密钥在公网正是此症状）。
// 元信息接口不收费，豁免它不带来任何计费风险。
func (a *app) apiKeyGuardNoBalance(next http.HandlerFunc) http.HandlerFunc {
	return a.apiKeyGuardOpt(next, false)
}

func (a *app) apiKeyGuardOpt(next http.HandlerFunc, requireBalance bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw := bearerToken(r)
		if raw == "" {
			writeOpenAIError(w, http.StatusUnauthorized, "missing api key")
			return
		}
		// 管理密钥通道：运维/后台调试直连，不经过用户计费
		// （settleUsage 遇到 user == nil 会直接跳过）。无 Key 实体，限流按客户端 IP。
		if a.key != "" && subtle.ConstantTimeCompare([]byte(raw), []byte(a.key)) == 1 {
			if !a.enforceRateLimit(w, "ip:"+a.clientIP(r)) {
				return
			}
			next(w, r)
			return
		}
		key, err := a.store.GetAPIKeyByHash(HashAPIKey(raw))
		if err != nil {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid api key")
			return
		}
		if key.Status != StatusActive {
			writeOpenAIError(w, http.StatusForbidden, "api key disabled")
			return
		}
		u, err := a.store.GetUserByID(key.UserID)
		if err != nil {
			writeOpenAIError(w, http.StatusUnauthorized, "api key owner not found")
			return
		}
		if u.Status != StatusActive {
			writeOpenAIError(w, http.StatusForbidden, "account banned")
			return
		}
		// 代挂自用 Key（kind=host）：只用号主自己的代挂账号、不扣余额，
		// 因此跳过余额与消费闸（仍保留用户状态与限流校验）。
		// requireBalance=false 的接口（如 /v1/models）同样跳过。
		if requireBalance && key.Kind != KeyKindHost {
			if u.BalanceMicro <= 0 {
				// 余额耗尽：硬拦截。允许上一笔把余额打成负数（在途请求不中断），
				// 但负余额状态下不再放行新请求。
				writeOpenAIError(w, http.StatusPaymentRequired, "insufficient balance, please redeem a code")
				return
			}
			// 消费闸：余额可能一次充很多，日/月上限能把单日突发消费按住。
			if msg := a.quotaExceeded(u); msg != "" {
				writeOpenAIError(w, http.StatusPaymentRequired, msg)
				return
			}
		}
		// 客户端维度限流：按 API Key 计数（换 Key 即换桶）。
		// 放在 Key/用户/余额全部校验通过之后、写上游之前——拒绝的请求不占用上游配额。
		if !a.enforceRateLimit(w, "key:"+strconv.FormatInt(key.ID, 10)) {
			return
		}
		a.store.TouchAPIKey(key.ID)
		ctx := context.WithValue(r.Context(), ctxUserKey, u)
		ctx = context.WithValue(ctx, ctxAPIKeyKey, key)
		next(w, r.WithContext(ctx))
	}
}

// bearerToken 取 Bearer token（兼容 Authorization 与 x-api-key）。
func bearerToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	if h := r.Header.Get("x-api-key"); h != "" {
		return strings.TrimSpace(h)
	}
	return ""
}

// ---------- 用户侧 API Key 管理 ----------

// handleListKeys 列出本人的 key。
//
// ⚠️ 安全口径（v0.17.8）：**列表不再返回明文**。此前这里会解密并把完整
// key 塞进每一次列表响应 —— 意味着它必然出现在页面初始渲染结果、浏览器内存
// 与任何抓包/日志里；一个只读接口不该携带长期有效的凭证。
// 现在只回展示用掩码（maskSecret），完整值改由 handleRevealKey **显式**单取。
func (a *app) handleListKeys(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	keys, err := a.store.ListAPIKeys(u.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		// 掩码优先按能解出的明文算（尾 4 位才是用户认得出的那几位）；
		// 解不出（老库无 key_enc / 换过 -secret）则退化为按 prefix 打码，绝不回退成明文。
		masked := ""
		if plain, derr := a.secret.Decrypt(k.KeyEnc); derr == nil && plain != "" {
			masked = maskSecret(plain)
		} else {
			masked = maskKey(k.KeyPrefix)
		}
		out = append(out, map[string]any{
			"id":           k.ID,
			"name":         k.Name,
			"prefix":       maskKey(k.KeyPrefix),
			"masked":       masked,
			"status":       k.Status,
			"kind":         k.Kind,
			"host_group":   k.HostGroup,
			"account_uids": splitUIDs(k.AccountUIDs),
			"last_used_at": k.LastUsedAt,
			"created_at":   k.CreatedAt,
			// revealable：能否「显式查看」（有密文且启用了加密才可解）。前端据此决定
			// 按钮是否可用，避免点开才发现是老库无密文 / 服务端以 -secret none 启动。
			"revealable": a.secret != nil && strings.TrimSpace(k.KeyEnc) != "",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

// handleRevealKey 显式取回某把 key 的完整明文（本人限定、单把）。
// GET /api/keys/{id}/reveal
//
// 设计约束（对应需求 1）：
//   - 只在用户主动点击时返回，绝不参与列表 / 初始渲染；
//   - 归属校验下推到 SQL（跨用户查直接 404，不泄露存在性）；
//   - **绝不记录**：本函数与调用链上没有任何日志语句，也不回填到其他响应字段；
//   - 错误信息保持笼统（「密钥不存在或不可查看」），不区分「不存在 / 无密文 / 解密失败」，
//     避免把服务端内部状态当作探测信号返回。
func (a *app) handleRevealKey(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "非法 id"})
		return
	}
	k, err := a.store.GetAPIKey(u.ID, id)
	if err != nil || k == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "密钥不存在"})
		return
	}
	plain, err := a.secret.Decrypt(k.KeyEnc)
	if err != nil || plain == "" {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "这把密钥没有可回显的密文（创建于更早的版本或加密密钥已更换），请重新生成",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":     k.ID,
		"key":    plain,
		"masked": maskSecret(plain),
	})
}

// splitUIDs 逗号分隔 uid → 切片（空串 → 空切片）。
func splitUIDs(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return []string{}
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// handleCreateKey 新建 key，返回明文（此后仍可在列表回显）。
// v0.12.0（需求 2）：支持 count 一次生成多把（1-20），返回 keys 数组；
// 同时保留 key 字段（= 第一把）兼容旧前端。
func (a *app) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	var req struct {
		Name        string   `json:"name"`
		Kind        string   `json:"kind"`
		HostGroup   string   `json:"host_group"`
		AccountUIDs []string `json:"account_uids"` // 代挂自用 Key 绑定的账号集合（可选）
		Count       int      `json:"count"`        // 生成数量（缺省 1，上限 20）
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	count := req.Count
	if count <= 0 {
		count = 1
	}
	if count > 20 {
		count = 20
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		req.Name = "default"
	}
	if len(req.Name) > 32 {
		req.Name = req.Name[:32]
	}
	// kind 仅认 "host"（代挂自用）；其余一律归为平台计费 Key，避免脏值绕过计费。
	kind := ""
	if strings.TrimSpace(req.Kind) == KeyKindHost {
		kind = KeyKindHost
	}
	// 绑定账号集合：仅代挂自用 Key 有意义；只接受本人代挂账号（防越权绑定他人账号）。
	accountUIDs := ""
	if kind == KeyKindHost && len(req.AccountUIDs) > 0 {
		owned := map[string]bool{}
		if list, err := a.store.ListAccountsByOwner(u.ID); err == nil {
			for _, acc := range list {
				owned[acc.UID] = true
			}
		}
		keep := make([]string, 0, len(req.AccountUIDs))
		for _, uid := range req.AccountUIDs {
			if uid = strings.TrimSpace(uid); uid != "" && owned[uid] {
				keep = append(keep, uid)
			}
		}
		accountUIDs = strings.Join(keep, ",")
	}
	hostGroup := strings.TrimSpace(req.HostGroup)
	items := make([]map[string]any, 0, count)
	for i := 0; i < count; i++ {
		name := req.Name
		if count > 1 {
			suffix := "-" + strconv.Itoa(i+1)
			base := req.Name
			if len(base)+len(suffix) > 32 {
				base = base[:32-len(suffix)]
			}
			name = base + suffix
		}
		plain := GenerateAPIKey()
		enc, err := a.secret.Encrypt(plain)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "密钥加密失败"})
			return
		}
		k, err := a.store.CreateAPIKey(u.ID, name, HashAPIKey(plain), enc, keyPrefix(plain), kind, hostGroup, accountUIDs)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		items = append(items, map[string]any{
			"id": k.ID, "name": k.Name, "key": plain, "status": k.Status,
			"kind": k.Kind, "host_group": k.HostGroup, "account_uids": splitUIDs(k.AccountUIDs),
			"created_at": k.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"key":   items[0],
		"keys":  items,
		"count": len(items),
	})
}

// handleDeleteKey 删除本人 key。
func (a *app) handleDeleteKey(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "非法 id"})
		return
	}
	if err := a.store.DeleteAPIKey(u.ID, id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "密钥不存在"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// handleToggleKey 启用 / 禁用本人 key。
func (a *app) handleToggleKey(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := userFrom(r)
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "非法 id"})
			return
		}
		status := StatusBanned
		if enabled {
			status = StatusActive
		}
		if err := a.store.SetAPIKeyStatus(u.ID, id, status); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "密钥不存在"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": status})
	}
}

// handleSetKeyAccounts 更新某代挂自用 Key 绑定的账号集合（req 7：Key 生成照反代页）。
func (a *app) handleSetKeyAccounts(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "非法 id"})
		return
	}
	var req struct {
		AccountUIDs []string `json:"account_uids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	owned := map[string]bool{}
	if list, err := a.store.ListAccountsByOwner(u.ID); err == nil {
		for _, acc := range list {
			owned[acc.UID] = true
		}
	}
	keep := make([]string, 0, len(req.AccountUIDs))
	for _, uid := range req.AccountUIDs {
		if uid = strings.TrimSpace(uid); uid != "" && owned[uid] {
			keep = append(keep, uid)
		}
	}
	if err := a.store.SetAPIKeyAccounts(u.ID, id, strings.Join(keep, ",")); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "密钥不存在"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "account_uids": keep})
}

// ---------- 用户侧账单与兑换 ----------

// handleUsage 本人的用量流水与汇总。
func (a *app) handleUsage(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	logs, err := a.store.ListUsageLogs(u.ID, limit, offset)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	reqs, tokens, charged, err := a.store.UserUsageSummary(u.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	items := make([]map[string]any, 0, len(logs))
	for _, l := range logs {
		items = append(items, map[string]any{
			"id": l.ID, "model": l.Model,
			"prompt_tokens": l.PromptTokens, "completion_tokens": l.CompletionTokens,
			"credit": l.UpstreamCredit, "charged": MicroToPoint(l.ChargedMicro),
			"status": l.Status, "error": l.ErrorMsg, "created_at": l.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"summary": map[string]any{
			"requests": reqs, "tokens": tokens,
			"charged":     MicroToPoint(charged),
			"charged_cny": microToCNY(charged, a.moneyRate()),
			"balance":     MicroToPoint(u.BalanceMicro),
			// 余额的金额形态（v0.17.7）：概览页要同时呈现「点」与「钱」两种形式。
			"balance_cny": microToCNY(u.BalanceMicro, a.moneyRate()),
			"multiplier":  a.store.PricingMultiplier(),
			"rate":        a.moneyRate(),
		},
		"logs": items,
	})
}

// handleUsageReport 本人用量报表（近 N 天，按日 + 按模型聚合）。
func (a *app) handleUsageReport(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	a.writeUsageReport(w, r, u.ID)
}

// handleAdminUsage 全站用量报表（userID=0 → 不限用户）。
func (a *app) handleAdminUsage(w http.ResponseWriter, r *http.Request) {
	a.writeUsageReport(w, r, 0)
}

// writeUsageReport 报表公共实现：userID<=0 表示全站。
// days 缺省 14，上限 90（再长前端也画不下，且扫表成本随天数线性上升）。
func (a *app) writeUsageReport(w http.ResponseWriter, r *http.Request, userID int64) {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 {
		days = 14
	}
	if days > 90 {
		days = 90
	}
	daily, err := a.store.UsageDaily(userID, days)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	models, err := a.store.UsageByModel(userID, days)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	var reqs, tokens, charged int64
	dailyItems := make([]map[string]any, 0, len(daily))
	for _, d := range daily {
		reqs += d.Requests
		tokens += d.Tokens
		charged += d.ChargedMicro
		dailyItems = append(dailyItems, map[string]any{
			"day": d.Day, "requests": d.Requests, "tokens": d.Tokens,
			"charged": MicroToPoint(d.ChargedMicro),
		})
	}
	modelItems := make([]map[string]any, 0, len(models))
	for _, m := range models {
		modelItems = append(modelItems, map[string]any{
			"model": m.Model, "requests": m.Requests, "tokens": m.Tokens,
			"charged": MicroToPoint(m.ChargedMicro),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"days":   days,
		"daily":  dailyItems,
		"models": modelItems,
		"totals": map[string]any{
			"requests": reqs, "tokens": tokens, "charged": MicroToPoint(charged),
		},
	})
}

// handleRedeem 用户兑换充值码。
func (a *app) handleRedeem(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	var req struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	code := strings.ToUpper(strings.TrimSpace(req.Code))
	if code == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请输入兑换码"})
		return
	}
	credits, kind, err := a.store.RedeemCode(code, u.ID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	// 推广返点：**仅充值码**参与返点计算；体验码不产生返点（v0.21.0 需求）。
	// 这是「体验点不参与计算、充值点可参与计算」的落地点。
	if kind == RedeemKindRecharge {
		a.creditReferralOnRecharge(u, credits)
	}
	fresh, _ := a.store.GetUserByID(u.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"added":   MicroToPoint(credits),
		"kind":    kind,
		"balance": MicroToPoint(fresh.BalanceMicro),
		"note": map[string]string{
			RedeemKindExp:      "体验点已到账（可消耗、不可提现、不产生返点）",
			RedeemKindRecharge: "充值点已到账（可消耗、不可提现）",
		}[kind],
	})
}

// ---------- 管理员 ----------

// userMatchesQuery 判定用户是否命中搜索词 q（已小写化）。
// 命中条件：用户名包含 q（子串、不区分大小写）或用户 ID 精确等于 q。
func userMatchesQuery(u *User, q string) bool {
	if u == nil {
		return false
	}
	if q == "" {
		return true
	}
	if strings.Contains(strings.ToLower(u.Username), q) {
		return true
	}
	return strconv.FormatInt(u.ID, 10) == q
}

// withdrawableOf 用户可提现额度（列表用简化版：min(额度桶, 余额)）。
//
// 与 store.WithdrawableQuota 的差别：这**不扣待审申请**（列表里逐个查待审会让
// 一页用户产生 N 次聚合查询）；精确额度（含待审扣减）在用户详情接口里给出。
// 列表用途是「快速判断能不能提」，用上界即可。
func withdrawableOf(u *User) int64 {
	if u == nil {
		return 0
	}
	wd := u.WithdrawableMicro
	if wd > u.BalanceMicro {
		wd = u.BalanceMicro
	}
	if wd < 0 {
		return 0
	}
	return wd
}

// handleAdminUserDetail 管理后台「用户详情」（v0.21.0，需求 6）。
//
//	GET /api/admin/users/{id}
//
// 一屏给出运维/客服需要的全部用户画像：身份与联系方式、余额分桶（可提现额度）、
// 消费与用量、返点与号池收益、代挂账号、提现记录、兑换记录、邀请关系。
// 只读接口，不产生任何副作用。
func (a *app) handleAdminUserDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "非法用户 id"})
		return
	}
	u, err := a.store.GetUserByID(id)
	if err != nil || u == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "用户不存在"})
		return
	}
	rate := a.moneyRate()

	// 用量 / 消费
	reqs, tokens, chargedMicro, _ := a.store.UserUsageSummary(u.ID)
	// 返点与号池收益
	hostMicro, hostCount, _ := a.store.HostEarningSummary(u.ID)
	// 提现
	pendingW, approvedW, rejectedW, wdCount, _ := a.store.WithdrawalSummaryByUser(u.ID)
	quota, rawQuota, _ := a.store.WithdrawableQuota(u.ID)
	// 代挂账号
	var accRows []map[string]any
	accCount, accInPool := 0, 0
	if accs, err := a.store.ListAccountsByOwner(u.ID); err == nil {
		for _, acc := range accs {
			accCount++
			if acc.PoolState == "in" {
				accInPool++
			}
			row := map[string]any{
				"uid": acc.UID, "nickname": acc.Nickname, "realm": acc.Realm,
				"pool_state": acc.PoolState, "host_group": acc.HostGroup,
				"consume_group": consumeGroupLabel(effectiveConsumeGroup(acc)),
				"disabled":      acc.Disabled,
			}
			if st, ok := a.proxy.pool.Status(acc.UID); ok {
				row["credits"] = st.Credits
				row["cooling"] = st.Cooling
			}
			accRows = append(accRows, row)
		}
	}
	// 兑换记录（本用户兑换过的码）
	redeems, _ := a.store.ListRedeemCodesByUser(u.ID, 100)
	redeemRows := make([]map[string]any, 0, len(redeems))
	for _, c := range redeems {
		redeemRows = append(redeemRows, map[string]any{
			"code":       maskRedeemCode(c.Code),
			"credits":    MicroToPoint(c.CreditsMicro),
			"kind":       normalizeRedeemKind(c.Kind),
			"used_at":    c.UsedAt,
			"created_at": c.CreatedAt,
		})
	}
	// 邀请关系：上级（推荐人）
	parentName := ""
	if u.ReferredBy != 0 {
		if p, err := a.store.GetUserByID(u.ReferredBy); err == nil && p != nil {
			parentName = p.Username
		}
	}
	// 下级数量
	childCount := 0
	if all, err := a.store.ListUsers(); err == nil {
		for _, x := range all {
			if x.ReferredBy == u.ID {
				childCount++
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id": u.ID, "username": u.Username, "role": u.Role, "status": u.Status,
		"wechat":      u.WeChat,
		"invite_code": u.InviteCode,
		"created_at":  u.CreatedAt, "updated_at": u.UpdatedAt,
		"referred_by":      u.ReferredBy,
		"referred_by_name": parentName,
		"invitees":         childCount,
		// 余额分桶（v0.21.0）
		"balance":          MicroToPoint(u.BalanceMicro),
		"balance_cny":      microToCNY(u.BalanceMicro, rate),
		"withdrawable":     MicroToPoint(quota),
		"withdrawable_cny": microToCNY(quota, rate),
		"withdrawable_raw": MicroToPoint(rawQuota),
		"exp_points":       MicroToPoint(u.ExpMicro),
		"recharge_points":  MicroToPoint(u.RechargeMicro),
		// 限额
		"daily_limit":   MicroToPoint(u.DailyLimitMicro),
		"monthly_limit": MicroToPoint(u.MonthlyLimitMicro),
		// 比例（-1 = 跟随全局）
		"host_ratio":     u.HostRatio,
		"referral_ratio": u.ReferralRatio,
		// 用量
		"usage": map[string]any{
			"requests": reqs, "tokens": tokens,
			"charged": MicroToPoint(chargedMicro), "charged_cny": microToCNY(chargedMicro, rate),
		},
		// 收益与提现
		"earnings": map[string]any{
			"host_total": MicroToPoint(hostMicro), "host_count": hostCount,
			"host_total_cny": microToCNY(hostMicro, rate),
		},
		"withdrawals": map[string]any{
			"pending": MicroToPoint(pendingW), "approved": MicroToPoint(approvedW),
			"rejected": MicroToPoint(rejectedW), "count": wdCount,
		},
		// 代挂账号
		"accounts": accRows,
		"account_summary": map[string]any{
			"total": accCount, "in_pool": accInPool, "idle": accCount - accInPool,
		},
		"redeems": redeemRows,
	})
}

// handleAdminUsers 用户列表。支持 ?q= 关键字搜索（v0.12.0，需求 1）：
// 匹配用户名（不区分大小写子串）或用户 ID（精确）。空 q = 全部。
func (a *app) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	users, err := a.store.ListUsers()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	match := func(u *User) bool { return userMatchesQuery(u, query) }
	// 统一口径（v0.17.7）：折算比例与全局返点比例只取一次，逐行复用。
	// 逐行各自读一遍会让同一页出现两种数值（管理员在别处改过比例的瞬间）。
	rate := a.moneyRate()
	hostGlobal := a.proxy.hostShareRatioNow()
	refGlobal := a.proxy.referralRatioNow()
	// 代挂分组：管理后台用户卡片统一展示「该用户代挂了多少账号、都是什么分组」。
	type hostAgg struct {
		Count  int
		Groups map[string]int
		Earned int64
	}
	hostByOwner := map[int64]*hostAgg{}
	if hosted, err := a.store.ListHostedAccounts(); err == nil {
		for _, acc := range hosted {
			ag := hostByOwner[acc.OwnerUserID]
			if ag == nil {
				ag = &hostAgg{Groups: map[string]int{}}
				hostByOwner[acc.OwnerUserID] = ag
			}
			ag.Count++
			ag.Groups[acc.HostGroup]++
			ag.Earned += acc.EarnedMicro
		}
	}
	out := make([]map[string]any, 0, len(users))
	for _, u := range users {
		if !match(u) {
			continue
		}
		reqs, tokens, charged, _ := a.store.UserUsageSummary(u.ID)
		// 日 / 月已用量：让管理端卡片能画出限额进度条（不然只有上限看不见用量）。
		dayUsed, monthUsed := a.quotaUsage(u)
		row := map[string]any{
			"id": u.ID, "username": u.Username, "role": u.Role, "status": u.Status,
			"balance": MicroToPoint(u.BalanceMicro), "created_at": u.CreatedAt,
			// 余额的金额形态（v0.17.7）：前端「点」为主 + 「≈ ¥」为副，避免两套口径。
			"balance_cny": microToCNY(u.BalanceMicro, rate),
			// 微信号（v0.21.0）：注册时选填，卡片上以标签展示。
			"wechat": u.WeChat,
			// 余额分桶（v0.21.0）：可提现额度（min(额度,余额)−待审）+ 两类累计点数。
			// 卡片用于快速判断「这人的钱能不能提」，详情弹窗有完整口径。
			"withdrawable":    MicroToPoint(withdrawableOf(u)),
			"exp_points":      MicroToPoint(u.ExpMicro),
			"recharge_points": MicroToPoint(u.RechargeMicro),
			"requests":        reqs, "tokens": tokens, "charged": MicroToPoint(charged),
			"charged_cny":   microToCNY(charged, rate),
			"daily_limit":   MicroToPoint(u.DailyLimitMicro),
			"monthly_limit": MicroToPoint(u.MonthlyLimitMicro),
			"daily_used":    MicroToPoint(dayUsed),
			"monthly_used":  MicroToPoint(monthUsed),
			// 逐用户返点比例（v0.17.7）：override/global/effective 三段，供卡片查看与编辑。
			"host_ratio":     newRatioOverrideView(u, func(x *User) float64 { return x.HostRatio }, hostGlobal),
			"referral_ratio": newRatioOverrideView(u, func(x *User) float64 { return x.ReferralRatio }, refGlobal),
		}
		if ag := hostByOwner[u.ID]; ag != nil {
			groups := make([]map[string]any, 0, len(ag.Groups))
			for g, n := range ag.Groups {
				groups = append(groups, map[string]any{"group": g, "count": n})
			}
			sort.SliceStable(groups, func(i, j int) bool {
				return strField(groups[i], "group") < strField(groups[j], "group")
			})
			row["host_count"] = ag.Count
			row["host_groups"] = groups
			row["host_earned"] = MicroToPoint(ag.Earned)
			// 代挂收益是「钱」（返点）→ 金额形态为主展示（v0.17.7）。
			row["host_earned_cny"] = microToCNY(ag.Earned, rate)
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"users":   out,
		"total":   len(users),
		"matched": len(out),
		"q":       query,
		// 全页统一口径（v0.17.7）：折算比例 + 全局返点比例，前端不必再各自取一次。
		"rate": rate,
		"ratios": map[string]any{
			"host":     hostGlobal,
			"referral": refGlobal,
		},
	})
}

// handleAdminSetBalance 调整某用户余额（正负均可，单位：点）。
func (a *app) handleAdminSetBalance(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "非法 id"})
		return
	}
	var req struct {
		Delta float64 `json:"delta"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Delta == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请提供 non-zero delta（单位：点）"})
		return
	}
	if err := a.store.AddUserBalance(id, PointToMicro(req.Delta)); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "用户不存在"})
		return
	}
	u, _ := a.store.GetUserByID(id)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "balance": MicroToPoint(u.BalanceMicro)})
}

// handleAdminSetLimits 设置某用户的日 / 月消费闸（单位：点，0 = 不限）。
func (a *app) handleAdminSetLimits(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "非法 id"})
		return
	}
	var req struct {
		Daily   *float64 `json:"daily"`
		Monthly *float64 `json:"monthly"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	if req.Daily == nil && req.Monthly == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请提供 daily 或 monthly（单位：点，0=不限）"})
		return
	}
	u, err := a.store.GetUserByID(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "用户不存在"})
		return
	}
	daily, monthly := u.DailyLimitMicro, u.MonthlyLimitMicro
	if req.Daily != nil {
		if *req.Daily < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "daily 不能为负数"})
			return
		}
		daily = PointToMicro(*req.Daily)
	}
	if req.Monthly != nil {
		if *req.Monthly < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "monthly 不能为负数"})
			return
		}
		monthly = PointToMicro(*req.Monthly)
	}
	if err := a.store.SetUserLimits(id, daily, monthly); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	dayUsed, monthUsed := a.quotaUsage(u)
	writeJSON(w, http.StatusOK, map[string]any{
		"id":            id,
		"daily_limit":   MicroToPoint(daily),
		"daily_used":    MicroToPoint(dayUsed),
		"monthly_limit": MicroToPoint(monthly),
		"monthly_used":  MicroToPoint(monthUsed),
	})
}

// handleAdminSetUserStatus 封禁 / 解封用户。
func (a *app) handleAdminSetUserStatus(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "非法 id"})
		return
	}
	var req struct {
		Status string `json:"status"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Status != StatusActive && req.Status != StatusBanned {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "status 只能是 active / banned"})
		return
	}
	// 不允许把自己封掉（免得管理员把自己锁在门外）。
	if me := userFrom(r); me != nil && me.ID == id && req.Status == StatusBanned {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "不能停用自己"})
		return
	}
	if err := a.store.SetUserStatus(id, req.Status); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": req.Status})
}

// handleAdminGenCodes 批量生成兑换码。
func (a *app) handleAdminGenCodes(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Count  int     `json:"count"`
		Points float64 `json:"points"`
		// Kind 兑换码类型（v0.21.0）："exp" 体验 / "" 或 "recharge" 充值（默认）。
		Kind string `json:"kind"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	if req.Count <= 0 || req.Count > 200 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "count 需在 1-200 之间"})
		return
	}
	if req.Points <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "points 必须大于 0"})
		return
	}
	var createdBy int64
	if me := userFrom(r); me != nil {
		createdBy = me.ID
	}
	micro := PointToMicro(req.Points)
	kind := normalizeRedeemKind(req.Kind)
	codes := make([]string, 0, req.Count)
	for i := 0; i < req.Count; i++ {
		code := generateRedeemCode()
		if err := a.store.CreateRedeemCode(code, micro, kind, createdBy); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		codes = append(codes, code)
	}
	writeJSON(w, http.StatusOK, map[string]any{"codes": codes, "points": req.Points, "kind": kind})
}

// handleAdminListCodes 兑换码列表。
func (a *app) handleAdminListCodes(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	codes, err := a.store.ListRedeemCodes(limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	out := make([]map[string]any, 0, len(codes))
	for _, c := range codes {
		out = append(out, map[string]any{
			"code": c.Code, "points": MicroToPoint(c.CreditsMicro),
			"kind": normalizeRedeemKind(c.Kind),
			"kind_label": map[string]string{
				RedeemKindExp:      "体验",
				RedeemKindRecharge: "充值",
			}[normalizeRedeemKind(c.Kind)],
			"used": c.UsedBy != 0, "used_by": c.UsedBy, "used_at": c.UsedAt,
			"created_at": c.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"codes": out})
}

// handleAdminSettings 读取 / 写入全局设置（定价倍率、注册开关、预扣参数）。
func (a *app) handleAdminSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, a.settingsView())
		return
	}
	var req struct {
		Multiplier      *float64 `json:"multiplier"`
		RegisterEnabled *bool    `json:"register_enabled"`
		ReserveRate     *float64 `json:"reserve_rate"`      // 兜底单价：点 / 1k token
		ReserveMax      *float64 `json:"reserve_max"`       // 单笔预扣上限（点，0=不限）
		RateLimitRPM    *int     `json:"rate_limit_rpm"`    // 客户端维度限流（次/分钟，0=不限）
		AlertWebhook    *string  `json:"alert_webhook"`     // 告警 webhook（空=不投递）
		AlertMinHealthy *int     `json:"alert_min_healthy"` // CN 健康数告警阈值（0=关闭）

		// 用户代挂：代挂账号在池中服务平台流量后按此比例把收费折入号主余额。
		HostShareRatio *float64 `json:"host_share_ratio"` // 分成比例（0~1，0=关闭）

		// 推广返点：下级兑换充值码 / 下级号池收益时，按此比例给上级返点（0~1，0=关闭）。
		ReferralRatio *float64 `json:"referral_ratio"`

		// 提现（v0.11.0）：兑换比例（元/点）与最小提现额（点）。
		WithdrawRate      *float64 `json:"withdraw_rate"`
		WithdrawMinPoints *float64 `json:"withdraw_min_points"`

		// 反代选号（分组 + 白名单）与防封（节流间隔 / 默认出站代理）。
		ProxyAccounts      *string `json:"proxy_accounts"`        // 反代白名单 uid（逗号分隔，空=全部）
		ProxyGroups        *string `json:"proxy_groups"`          // 反代分组白名单（逗号分隔，空=全部分组）
		ProxyMinGapSeconds *int    `json:"proxy_min_gap_seconds"` // 同账号最小复用间隔（秒，0=不限）
		EgressProxyDefault *string `json:"egress_proxy_default"`  // 全局默认出站代理（空=直连）
		// 出口代理池参数（v0.14.0 防封）。
		EgressMaxPerProxy *int `json:"egress_max_per_proxy"` // 同出口账号数上限（**0 = 不限**，见 defaultMaxPerProxy）
		EgressJitterMs    *int `json:"egress_jitter_ms"`     // 请求前抖动上限（毫秒，0=关闭）
		// EgressOnLineDown 线路失效时的处置（R1，v0.26.1）：
		//   strict（默认）= 不静默换出口（账号留在原线路、排除选号、等人处理）
		//   migrate       = 自动迁移到可用线路（旧行为）
		// 见 egress_policy.go —— 为什么默认 strict 写在那里。
		EgressOnLineDown *string `json:"egress_on_line_down"`
		// FingerprintSamples 指纹被拦样本回流（R4，v0.26.1）：on / off，**默认 off**。
		// 之所以默认关：它是本仓唯一会把请求内容（system 原文片段）写到磁盘的功能，
		// 与"请求体不留存"的既有承诺冲突，必须由运维显式开启。见 fpsample.go 文件头。
		FingerprintSamples *string `json:"fingerprint_samples"`
		// PickSinkOn5xx 上游 5xx 时是否降档重试（R6，v0.26.1）：on / off，**默认 off**。
		// 本仓的「档」是**成本层**，下沉 = 去用更贵的账号 → 默认不替站长做这个决定。
		PickSinkOn5xx *string `json:"pick_sink_on_5xx"`
		// DeviceTokenFallback 设备凭证派生兜底（v0.26.3）：off（默认）/ derive。
		// derive = 前三档（账号 / 全局 / 文件）全空时，按 uid 稳定派生一个互异值。
		// 见 devicecred.go 与 upstream/headers.go 的 resolveDeviceToken。
		DeviceTokenFallback *string `json:"device_token_fallback"`
		// DeviceTokenStaleDays 设备凭证陈旧阈值（天，0=关闭检测，默认 30）。
		DeviceTokenStaleDays *int `json:"device_token_stale_days"`
		// 选号模式（v0.15.1）：weighted（缺省，三因子加权随机）|
		// roundrobin（顺序游标轮转，负载均匀铺开，对齐参考实现）。
		PickMode *string `json:"pick_mode"`

		// ---- 导入防关联（v0.25.0，见 import_guard.go）----
		// ImportIPGate 导入前是否要求「出口 IP 与上次导入不同」。
		ImportIPGate   *bool   `json:"import_ip_gate"`
		ImportMaxNew   *int    `json:"import_max_new"`    // 单次新增账号数上限（0 = 不限）
		IPResetCmd     *string `json:"ip_reset_cmd"`      // 可选的换 IP 命令（空 = 手动模式）
		ImportResetSec *int    `json:"import_reset_wait"` // 等待 IP 变化的秒数上限

		// ---- 账号卡积分补新（v0.25.0）----
		// CreditAutoSec 页面可见时逐个补新的间隔秒（0 = 关闭自动补新，只留手动按钮）。
		CreditAutoSec *int `json:"credit_auto_refresh_sec"`

		// ---- 设备风控凭证兜底（v0.25.1，见 devicecred.go）----
		// DeviceToken 全局兜底设备凭证（账号没自带 device_token 时用它）。
		// 传空串 = 清除。**不会被回显**（只回 device_token_set），因为它是风控凭证。
		DeviceToken *string `json:"device_token"`
		// DeviceTokenFile 存放设备凭证的文件路径（与桌面端共用状态文件的部署方式）。
		DeviceTokenFile *string `json:"device_token_file"`

		// ---- 版本更新提醒（v0.26.0，见 updatecheck.go）----
		// UpdateCheckEnabled 是否开启版本检查。内网/私有化部署应当关掉 ——
		// 它会主动向外部地址出站，这在隔离网络里既无用又多余。
		UpdateCheckEnabled *bool `json:"update_check_enabled"`
		// UpdateManifestURL 更新清单（version.json）地址。原样回显：它是配置不是密钥。
		UpdateManifestURL *string `json:"update_manifest_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	setFloat := func(key string, v float64) bool {
		if err := a.store.SetKV(key, strconv.FormatFloat(v, 'f', -1, 64)); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return false
		}
		return true
	}
	if req.Multiplier != nil {
		if *req.Multiplier <= 0 || *req.Multiplier > 1000 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "计费倍率需在 0-1000 之间（点 / 1 号池积分）"})
			return
		}
		if !setFloat(settingMultiplier, *req.Multiplier) {
			return
		}
	}
	if req.RegisterEnabled != nil {
		v := "0"
		if *req.RegisterEnabled {
			v = "1"
		}
		if err := a.store.SetKV(settingRegisterEnabled, v); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	if req.ReserveRate != nil {
		if *req.ReserveRate < 0 || *req.ReserveRate > 100000 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "预扣单价需在 0-100000 之间（点/千 token）"})
			return
		}
		if !setFloat(settingReserveRate, *req.ReserveRate) {
			return
		}
	}
	if req.ReserveMax != nil {
		if *req.ReserveMax < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "预扣上限不能为负数"})
			return
		}
		if !setFloat(settingReserveMax, *req.ReserveMax) {
			return
		}
	}
	if req.RateLimitRPM != nil {
		if *req.RateLimitRPM < 0 || *req.RateLimitRPM > 1000000 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "限流需在 0-1000000 次/分钟之间（0=不限）"})
			return
		}
		if err := a.store.SetKV(settingRateLimitRPM, strconv.Itoa(*req.RateLimitRPM)); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	if req.AlertWebhook != nil {
		u := strings.TrimSpace(*req.AlertWebhook)
		if u != "" && !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "webhook 必须是 http(s):// 开头"})
			return
		}
		if err := a.store.SetKV(settingAlertWebhook, u); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	if req.AlertMinHealthy != nil {
		if *req.AlertMinHealthy < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "告警阈值不能为负"})
			return
		}
		if err := a.store.SetKV(settingAlertMinHealthy, strconv.Itoa(*req.AlertMinHealthy)); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	// 反代选号 + 防封 + 代挂分成配置：任一项变更后重载选号参数。
	reloadSel := false
	if req.HostShareRatio != nil {
		v := *req.HostShareRatio
		if v < 0 || v > 1 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "代挂分成比例需在 0~1 之间"})
			return
		}
		if err := a.store.SetKV(kvHostShareRatio, strconv.FormatFloat(v, 'f', 4, 64)); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		reloadSel = true
	}
	if req.ReferralRatio != nil {
		v := *req.ReferralRatio
		if v < 0 || v > 1 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "推广返点比例需在 0~1 之间"})
			return
		}
		if err := a.store.SetKV(kvReferralRatio, strconv.FormatFloat(v, 'f', 4, 64)); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		reloadSel = true
	}
	// 提现参数（v0.11.0）：兑换比例与最小提现额，不影响选号，无需 reloadSel。
	if req.WithdrawRate != nil {
		v := *req.WithdrawRate
		if v <= 0 || v > 1000 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "提现兑换比例需在 0~1000 之间（元/点）"})
			return
		}
		if err := a.store.SetKV(kvWithdrawRate, strconv.FormatFloat(v, 'f', -1, 64)); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	if req.WithdrawMinPoints != nil {
		v := *req.WithdrawMinPoints
		if v < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "最小提现额不能为负"})
			return
		}
		if err := a.store.SetKV(kvWithdrawMinPoints, strconv.FormatFloat(v, 'f', -1, 64)); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	if req.ProxyAccounts != nil {
		if err := a.store.SetKV(kvProxyAccounts, sanitizeCSV(*req.ProxyAccounts)); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		reloadSel = true
	}
	if req.ProxyGroups != nil {
		if err := a.store.SetKV(kvProxyGroups, sanitizeCSV(*req.ProxyGroups)); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		reloadSel = true
	}
	if req.ProxyMinGapSeconds != nil {
		if *req.ProxyMinGapSeconds < 0 || *req.ProxyMinGapSeconds > 86400 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "最小复用间隔需在 0-86400 秒之间"})
			return
		}
		if err := a.store.SetKV(kvProxyMinGap, strconv.Itoa(*req.ProxyMinGapSeconds)); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		reloadSel = true
	}
	if req.EgressProxyDefault != nil {
		p := strings.TrimSpace(*req.EgressProxyDefault)
		// 与账号级代理同一份校验（internal/egress）。
		// 默认代理影响面更大：填错会让**所有**未单独配置的账号同时失去出口隔离。
		if _, err := egress.Validate(p); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "全局默认出口代理：" + err.Error()})
			return
		}
		if err := a.store.SetKV(kvEgressDefault, p); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		reloadSel = true
	}
	if req.EgressJitterMs != nil {
		// 抖动：0 = 关闭；上限 5000ms（再大就不是「抖动」而是人为故障了）。
		v := *req.EgressJitterMs
		if v < 0 {
			v = 0
		}
		if v > 5000 {
			v = 5000
		}
		if err := a.store.SetKV(kvEgressJitterMs, strconv.Itoa(v)); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		reloadSel = true
	}
	if req.EgressMaxPerProxy != nil {
		v := *req.EgressMaxPerProxy
		if v < 0 {
			v = 0
		}
		if v := strings.TrimSpace(*req.EgressOnLineDown); v != "" {
			m, ok := normaliseEgressOnLineDown(v)
			if !ok {
				writeJSON(w, http.StatusBadRequest, map[string]any{
					"error": "egress_on_line_down 只能是 strict 或 migrate"})
				return
			}
			if err := a.store.SetKV(kvEgressOnLineDown, m); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
		}
		if v := strings.ToLower(strings.TrimSpace(*req.DeviceTokenFallback)); v != "" {
			if v != "off" && v != "derive" {
				writeJSON(w, http.StatusBadRequest, map[string]any{
					"error": "device_token_fallback 只能是 off 或 derive"})
				return
			}
			if err := a.store.SetKV(kvDeviceTokenFallback, v); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			// 改开关要立刻生效：出站取值装配在 upstream.Client 上，不是每次请求现读 kv。
			if a.proxy != nil && a.proxy.uc != nil {
				a.proxy.uc.DeviceTokenFallback = v == "derive"
			}
		}
		if v := req.DeviceTokenStaleDays; v != nil {
			n := *v
			if n < 0 || n > 3650 {
				writeJSON(w, http.StatusBadRequest, map[string]any{
					"error": "device_token_stale_days 需在 0..3650 之间（0 = 关闭检测）"})
				return
			}
			if err := a.store.SetKV(kvDeviceTokenStaleDays, strconv.Itoa(n)); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
		}
		if v := strings.ToLower(strings.TrimSpace(*req.PickSinkOn5xx)); v != "" {
			if v != "on" && v != "off" {
				writeJSON(w, http.StatusBadRequest, map[string]any{
					"error": "pick_sink_on_5xx 只能是 on 或 off"})
				return
			}
			if err := a.store.SetKV(kvPickSinkOn5xx, v); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
		}
		if v := strings.ToLower(strings.TrimSpace(*req.FingerprintSamples)); v != "" {
			if v != "on" && v != "off" {
				writeJSON(w, http.StatusBadRequest, map[string]any{
					"error": "fingerprint_samples 只能是 on 或 off"})
				return
			}
			if err := a.store.SetKV(kvFingerprintSamples, v); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
		}
	}
	if req.EgressMaxPerProxy != nil {
		v := *req.EgressMaxPerProxy
		if v < 0 {
			v = 0
		}
		if err := a.store.SetKV(kvEgressMaxPerProxy, strconv.Itoa(v)); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	// 导入防关联（v0.25.0）：
	// 换成 IP 命令是**能执行本地命令**的配置项，只允许从这里写（adminGuard 之后），
	// **刻意不接受**来自导入接口请求体的命令 —— 否则"能调导入接口"就变成"能执行任意命令"。
	if req.ImportIPGate != nil {
		v := "0"
		if *req.ImportIPGate {
			v = "1"
		}
		if err := a.store.SetKV(kvImportIPGate, v); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	if req.ImportMaxNew != nil {
		v := *req.ImportMaxNew
		if v < 0 {
			v = 0
		}
		if err := a.store.SetKV(kvImportMaxNew, strconv.Itoa(v)); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	if req.IPResetCmd != nil {
		cmdStr := strings.TrimSpace(*req.IPResetCmd)
		if len(cmdStr) > 500 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "换 IP 命令过长（上限 500 字符）"})
			return
		}
		if err := a.store.SetKV(kvIPResetCmd, cmdStr); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		if cmdStr != "" {
			log.Printf("INFO: [import] 已配置换 IP 命令（长度 %d，内容不落日志以免带出凭据）", len(cmdStr))
		}
	}
	if req.ImportResetSec != nil {
		v := *req.ImportResetSec
		if v < 10 {
			v = importResetWaitDef
		}
		if v > 600 {
			v = 600
		}
		if err := a.store.SetKV(kvImportResetWait, strconv.Itoa(v)); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	// 设备风控凭证兜底（v0.25.1）：允许传空串以**清除**。
	// 注意 env 优先级高于 kv（WB2API_DEVICE_TOKEN）—— 若由 env 提供，
	// 这里的修改不会生效，GET 会回 device_token_from_env 让界面能说明这一点。
	if req.DeviceToken != nil {
		v := strings.TrimSpace(*req.DeviceToken)
		if len(v) > 4096 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "设备凭证过长（上限 4096 字符）"})
			return
		}
		if err := a.store.SetKV(kvDeviceToken, v); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		// 只记长度，不记内容：凭证不该进日志。
		log.Printf("INFO: [device] 全局设备凭证已更新（长度 %d）", len(v))
	}
	if req.DeviceTokenFile != nil {
		v := strings.TrimSpace(*req.DeviceTokenFile)
		if len(v) > 512 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "文件路径过长（上限 512 字符）"})
			return
		}
		if err := a.store.SetKV(kvDeviceTokenFile, v); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	// 版本更新提醒（v0.26.0）。
	if req.UpdateCheckEnabled != nil {
		v := "0"
		if *req.UpdateCheckEnabled {
			v = "1"
		}
		if err := a.store.SetKV(kvUpdateEnabled, v); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	if req.UpdateManifestURL != nil {
		v := strings.TrimSpace(*req.UpdateManifestURL)
		// **只接受 http(s)**：这是程序会主动出站请求的地址。放开 scheme 等于
		// 把这个配置项变成"读任意本地文件/打内网"的原语（file://、gopher://
		// 之类），而它本来只是个更新源地址。
		if v != "" && !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://") {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "更新清单地址必须是 http(s) URL"})
			return
		}
		if len(v) > 512 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "更新清单地址过长（上限 512 字符）"})
			return
		}
		if err := a.store.SetKV(kvUpdateManifest, v); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	if req.CreditAutoSec != nil {
		v := *req.CreditAutoSec
		if v < 0 {
			v = 0
		}
		if v > creditRefreshSecMax {
			v = creditRefreshSecMax
		}
		if err := a.store.SetKV(kvCreditAutoRefreshSec, strconv.Itoa(v)); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	if req.PickMode != nil {
		mode := strings.TrimSpace(*req.PickMode)
		// 显式传值必须合法：静默回落会让"填了没生效"变成难查的问题
		//（前端回读虽能看到实际值，但不如直接拒绝明确）。
		if mode != pool.PickModeWeighted && mode != pool.PickModeRoundRobin {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "选号模式仅支持 weighted（加权随机）或 roundrobin（顺序轮转）",
			})
			return
		}
		if err := a.store.SetKV(kvPickMode, mode); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		reloadSel = true
	}
	if reloadSel {
		a.proxy.ReloadSelection()
	}
	writeJSON(w, http.StatusOK, a.settingsView())
}

// settingsView 全局设置的统一返回体。
func (a *app) settingsView() map[string]any {
	reg, _ := a.store.GetKV(settingRegisterEnabled)
	rate, _ := a.store.GetKV(settingReserveRate)
	capv, _ := a.store.GetKV(settingReserveMax)
	fRate, _ := strconv.ParseFloat(strings.TrimSpace(rate), 64)
	fCap, _ := strconv.ParseFloat(strings.TrimSpace(capv), 64)
	out := map[string]any{
		"multiplier": a.store.PricingMultiplier(),
		// billing_basis 明确计费基准，供前端展示口径（v0.12.0，需求 7）。
		"billing_basis":    "pool_credit",
		"register_enabled": reg != "0",
		"reserve_rate":     fRate,
		"reserve_max":      fCap,
		// 客户端维度限流（每分钟请求数，0 = 不限）。
		"rate_limit_rpm": a.rateLimitRPM(),
		// 告警（webhook URL 空 = 不投递；min_healthy 0 = 关闭该规则）。
		"alert_webhook":     a.alertWebhook(),
		"alert_min_healthy": a.alertMinHealthy(),
		// 预扣是否已自动生效：观测到单价的模型数 > 0 即开始冻结额度。
		"observed_models": a.proxy.observedModelCount(),
		// 代挂分成比例（0 = 关闭）。
		"host_share_ratio": a.proxy.hostShareRatioNow(),
		// 推广返点比例（0 = 关闭）。
		"referral_ratio": a.proxy.referralRatioNow(),
	}
	// 提现参数（兑换比例 元/点 + 最小提现额 点）。
	wRate, wMin := a.withdrawParams()
	out["withdraw_rate"] = wRate
	out["withdraw_min_points"] = wMin
	// 反代选号（分组/白名单）+ 防封（节流间隔/默认出站代理）摘要。
	for k, v := range a.proxy.SelectionSummary() {
		out[k] = v
	}
	// 出口代理池参数（v0.14.0 防封）。
	out["egress_max_per_proxy"] = a.store.egressMaxPerProxy()
	out["egress_jitter_ms"] = a.proxy.egressJitterMsNow()
	// 线路失效处置策略（R1）：回显当前生效值，strict = 不静默换出口。
	out["egress_on_line_down"] = a.egressOnLineDownMode()
	// 样本回流开关（R4）：默认 off，回显让界面能说明"当前没在收集"。
	smp, _ := a.store.GetKV(kvFingerprintSamples)
	if !strings.EqualFold(strings.TrimSpace(smp), "on") {
		smp = "off"
	}
	out["fingerprint_samples"] = smp
	// 5xx 降档开关（R6）：默认 off。打开 = 允许用更贵的账号换可用性。
	sink, _ := a.store.GetKV(kvPickSinkOn5xx)
	if !strings.EqualFold(strings.TrimSpace(sink), "on") {
		sink = "off"
	}
	out["pick_sink_on_5xx"] = sink
	// 设备凭证的两项设置（v0.26.3）：回显让界面能说明当前处于哪一档。
	dtFb, _ := a.store.GetKV(kvDeviceTokenFallback)
	if !strings.EqualFold(strings.TrimSpace(dtFb), "derive") {
		dtFb = "off"
	}
	out["device_token_fallback"] = dtFb
	out["device_token_stale_days"] = a.deviceTokenStaleDays()
	// 导入防关联 + 积分补新（v0.25.0）。换 IP 命令**原样回显**给管理员：
	// 它是配置项不是密钥，回显才能核对填对没有（命令里可能带插座 token，
	// 但能看到它的人本来就有管理密钥）。
	out["import_ip_gate"] = a.importIPGateOn()
	out["import_max_new"] = a.importMaxNew()
	out["ip_reset_cmd"] = a.ipResetCmd()
	out["import_reset_wait"] = int(a.importResetWait().Seconds())
	out["credit_auto_refresh_sec"] = a.creditAutoRefreshSec()
	// 设备风控凭证（v0.25.1）：**只回"是否已配置"，绝不回显明文** ——
	// 它是风控凭证，等同敏感串；设置页也不需要看到原值才能改。
	out["device_token_set"] = a.store.globalDeviceToken() != ""
	out["device_token_from_env"] = strings.TrimSpace(os.Getenv(envDeviceToken)) != ""
	out["device_token_file"] = a.store.deviceTokenFilePath()
	out["device_token_file_ok"] = deviceTokenFileHasContent(a.store.deviceTokenFilePath())
	// 版本更新提醒（v0.26.0）：清单地址原样回显（配置项，不是密钥）；
	// 被跳过的版本也回显，否则管理员会以为"检查坏了"。
	out["update_check_enabled"] = a.updateCheckEnabled()
	out["update_manifest_url"] = a.updateManifestURL()
	out["update_skip_version"] = a.updateSkipVersion()
	return out
}

// gcSessions 定期清理过期会话。
func (a *app) gcSessions() { a.store.GCSessions() }

// startGC 后台 GC（会话 + OAuth 暂存）+ 告警周期评估。
func (a *app) startGC() {
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for range t.C {
			a.gcSessions()
			gcOAuthSessions()
			// 探活闸的「上次探活时刻」也要回收（v0.24.3）：每个被探过的 uid 留一条，
			// 号池会有增删，不回收就是又一个只增不减的 map。
			a.probes.gc(time.Now())
		}
	}()
	// 告警评估独立 30s 周期：不依赖 /metrics 被抓取，
	// 否则「没人抓指标 = 不告警」——恰恰是最需要告警的场景。
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for range t.C {
			a.evaluateAlerts()
		}
	}()
}

// ======================================================================
// 逐用户返点比例（v0.17.7）
//
// 需求：管理员可对单个用户「查看与编辑」返点比例，改完即时生效于该用户后续入账。
// 两类比例各自独立：
//   · host_ratio      号池分成比例 —— 该用户的代挂账号在池中服务时，他拿多少
//   · referral_ratio  推广返点比例 —— 他的下级充值 / 赚收益时，他从上级身份拿多少
// 两者的解析口径一致：**用户覆盖（≥0）优先，否则全局配置**（见 money.go）。
//
// 注意语义边界（刻意区分，不可合并）：
//   -1（或 null）→ 清除覆盖，回到「跟随全局」
//    0          → 明确设为 0%，即该用户不参与该类返点
// 编辑只影响**之后**的入账；历史流水与已入账余额不会追溯重算 —— 这是刻意的：
// 追溯重算要动用户余额与历史流水两处账，风险远高于收益，故不做（页面已写明）。
// ======================================================================

// ratioEditRequest 逐用户返点比例的编辑请求。
//
// 用 *float64 而不是 float64：必须区分「字段未提供（不改）」与「显式传 0」。
// 用 0 值 float64 会把「只改号池比例」的请求误解成「顺便把推广比例清零」。
type ratioEditRequest struct {
	HostRatio     *float64 `json:"host_ratio"`
	ReferralRatio *float64 `json:"referral_ratio"`
}

// handleAdminGetUserRatios 查看某用户当前的返点比例三段视图。
// GET /api/admin/users/{id}/ratios
func (a *app) handleAdminGetUserRatios(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "非法 id"})
		return
	}
	u, err := a.store.GetUserByID(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "用户不存在"})
		return
	}
	hostGlobal := a.proxy.hostShareRatioNow()
	refGlobal := a.proxy.referralRatioNow()
	writeJSON(w, http.StatusOK, map[string]any{
		"id":       u.ID,
		"username": u.Username,
		"host_ratio": newRatioOverrideView(u, func(x *User) float64 { return x.HostRatio },
			hostGlobal),
		"referral_ratio": newRatioOverrideView(u, func(x *User) float64 { return x.ReferralRatio },
			refGlobal),
		"rate": a.moneyRate(),
	})
}

// handleAdminSetUserRatios 编辑某用户的返点比例（含清除覆盖）。
// POST /api/admin/users/{id}/ratios  body: {"host_ratio":0.5|null, "referral_ratio":-1|null}
//
// 返回更新后的三段视图（含 effective），使前端无需二次请求即可刷新展示 ——
// 需求要求「调整结果实时反映到返点金额的展示中」，回传生效值是最省往返的做法。
func (a *app) handleAdminSetUserRatios(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "非法 id"})
		return
	}
	var req ratioEditRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	if req.HostRatio == nil && req.ReferralRatio == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "请提供 host_ratio 或 referral_ratio（0~1，-1 或 null 表示跟随全局）"})
		return
	}
	if err := a.store.SetUserRatios(id, req.HostRatio, req.ReferralRatio); err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, ErrNotFound) {
			code = http.StatusNotFound
		} else if strings.Contains(err.Error(), "must be in") {
			code = http.StatusBadRequest
		}
		writeJSON(w, code, map[string]any{"error": err.Error()})
		return
	}
	u, err := a.store.GetUserByID(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "用户不存在"})
		return
	}
	hostGlobal := a.proxy.hostShareRatioNow()
	refGlobal := a.proxy.referralRatioNow()
	writeJSON(w, http.StatusOK, map[string]any{
		"id":       u.ID,
		"username": u.Username,
		"host_ratio": newRatioOverrideView(u, func(x *User) float64 { return x.HostRatio },
			hostGlobal),
		"referral_ratio": newRatioOverrideView(u, func(x *User) float64 { return x.ReferralRatio },
			refGlobal),
		"rate": a.moneyRate(),
	})
}
