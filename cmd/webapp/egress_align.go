// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 账号代理 ↔ 出口代理的**一致化**（v0.31.33）。
//
// # 问题
//
// 系统里有两处"出口"配置，它们本来是同一件事的两个视角：
//
//	· `accounts.egress_proxy`  —— 这个号用哪个代理（账号代理）
//	· `egress_proxies`        —— 有哪些代理可用（出口代理 / 线路池，带探活与出口 IP）
//
// 但账号那侧是**自由文本**：管理员在「账号代理 · 改」里可以填任意 URL，用户端也能填。
// 填进去的 URL 不进线路池，于是：
//
//  1. 这条出口**没有 last_ip** —— 页面只能显示"账号代理 = http://…"，永远说不出
//     "它实际从哪个 IP 出去"；而同一台代理若也被录进池里，两边是两份互不相认的数据；
//  2. 线路池的「N 条线路 / M 个 IP」漏掉它，分配算法也不知道它存在 ——
//     于是"IP 隔离"的账算不平；
//  3. 同一个代理写成 `http://a:1` 与 `HTTP://a:1/` 就是两个身份，各自探活、各自计数，
//     出口 IP 却是同一个。
//
// # 做法
//
// **单一事实源**：账号上的出口 URL 必须能在池里找到同一条线路。
//
//	· 所有写账号出口的路径都走 `a.setAccountEgress`：先规范化，再确保池里存在
//	  （缺则自动登记，归属按账号号主），最后才写账号；
//	· 启动时与手动「对齐」时跑 `convergeAccountEgress`：把历史数据里
//	  "只有账号有、池里没有"的 URL 补登记；把同 URL 不同写法的线路合并；
//	· 账号上的 URL 一并改写为规范写法，两边字面也一致。
//
// 这样"账号代理的 IP"与"出口代理的 IP"就是同一个字段（线路的 `last_ip`），
// 不存在两套口径。
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/egress"
)

// egressConvergeResult 一次对齐的结果（前端逐项提示）。
type egressConvergeResult struct {
	// MergedLines 同 URL 不同写法被合并掉的线路条数。
	MergedLines int `json:"merged_lines"`
	// RegisteredLines 账号上有、池里没有 → 本次补登记的线路条数。
	RegisteredLines int `json:"registered_lines"`
	// RewrittenAccounts 账号出口被改写成规范写法的条数。
	RewrittenAccounts int `json:"rewritten_accounts"`
	// Probed 本次触发探活（拿真实出口 IP）的线路条数。
	Probed int `json:"probed"`
	// Orphans 对齐后仍然"账号有、池里没有"的条数（正常应为 0）。
	//
	// 非 0 只可能是：URL 非法（规范化失败）、或写库失败 —— 两种都要人看一眼。
	Orphans int      `json:"orphans"`
	Notes   []string `json:"notes,omitempty"`
}

// egressURLKey 取 URL 的**比对键**（规范化结果；非法时退回 trim 后的原文）。
//
// 为什么所有"按 URL 查线路"的地方都要用它：规范化只在写入时做，
// 历史行可能仍是旧写法。用原文做 map key 会出现"账号上 `http://a:1`、
// 池里 `HTTP://a:1/`"这种查不到的情形 —— 查不到就显示"不在线路池"，
// 而它其实就在，只是写法不同。这类 bug 不会报错，只会让人反复怀疑数据。
func egressURLKey(raw string) string {
	v := strings.TrimSpace(raw)
	if canon, err := egress.Normalize(v); err == nil && canon != "" {
		return canon
	}
	return v
}

// ensureEgressLineRegistered 确保某个（已规范化的）URL 在线路池里存在。
//
// owner 决定归属：账号的号主是谁，这条自动登记的线路就归谁 —— 否则管理员在用户号上
// 填的自定义代理会变成"平台线路"，而用户在控制台里会看到一条不属于自己的线。
//
// 幂等：已存在就原样返回（不改名字、不改归属、不动探活结果）。
func (a *app) ensureEgressLineRegistered(canonURL string, owner int64) (*EgressProxy, bool, error) {
	url := strings.TrimSpace(canonURL)
	if url == "" || egress.IsDirect(url) {
		return nil, false, nil
	}
	list, err := a.store.ListEgressProxies()
	if err != nil {
		return nil, false, err
	}
	for _, p := range list {
		if strings.TrimSpace(p.URL) == url {
			return p, false, nil
		}
	}
	now := time.Now().Unix()
	// 名字优先带用户名：线路池里一眼能看出"这条是谁的"。
	// 归属为平台（owner=0）时才用中性名 —— 那种情形通常是管理员手工录的兜底线路。
	name := "账号出口 · " + egressHostOf(url)
	if owner > 0 {
		name = a.userNameByID(owner) + " 的出口 · " + egressHostOf(url)
	}
	p := &EgressProxy{
		ID:   newEgressProxyID(url),
		Name: name,
		URL:  url,
		// Region 先按主机填：它的用途是"给人读的第一眼线索"，
		// 真实归属地要等探活后由人补，不要凭空编。
		Region:      egressHostOf(url),
		Note:        "由账号出口自动登记（账号代理与出口代理一致化）",
		Enabled:     true,
		CreatedAt:   now,
		UpdatedAt:   now,
		OwnerUserID: owner,
	}
	if err := a.store.UpsertEgressProxy(p); err != nil {
		return nil, false, err
	}
	log.Printf("INFO: [egress] 账号出口自动登记为线路 %s（owner=%d）", url, owner)
	return p, true, nil
}

// setAccountEgress **唯一**的账号出口写入入口（v0.31.33）。
//
// 顺序是刻意的：**先登记线路、再写账号**。反过来的话，一旦登记失败，
// 账号上就留下了"池里不存在的出口"——正是这一版要消灭的状态。
//
// 返回规范化后的值（调用方用它回显/比对，避免"我填的是 A，存的是 B"）。
func (a *app) setAccountEgress(uid, raw string) (string, error) {
	norm, err := egress.ValidateAccountEgress(raw)
	if err != nil {
		return "", err
	}
	if norm != "" && !egress.IsDirect(norm) {
		acc, gerr := a.store.GetAccount(uid)
		if gerr != nil {
			return "", gerr
		}
		var owner int64
		if acc != nil {
			owner = acc.OwnerUserID
		}
		if _, _, err := a.ensureEgressLineRegistered(norm, owner); err != nil {
			return "", err
		}
	}
	if err := a.store.SetAccountEgress(uid, norm); err != nil {
		return "", err
	}
	return norm, nil
}

// convergeAccountEgress 把历史数据对齐到"账号出口 ⊆ 线路池"。
//
// probeNew=true 时会给**新登记/从未探到 IP** 的线路各探一次活（串行、带间隔），
// 让"账号代理的实际出口 IP"立刻有值；否则等定时探活（5 分钟一轮）补齐。
//
// 为什么探活要串行且有上限：探活是真实出站（经代理打第三方回显端点）。
// 一次对齐几十条线路并发打出去，既慢又容易被代理供应商限流。
func (a *app) convergeAccountEgress(probeNew bool) (egressConvergeResult, error) {
	var res egressConvergeResult

	proxies, err := a.store.ListEgressProxies()
	if err != nil {
		return res, err
	}
	accs, err := a.store.ListAccounts()
	if err != nil {
		return res, err
	}

	// ---- 1) 线路池自身去重：同 URL 不同写法合并成一条 ----
	//
	// 保留**最早创建**的那条（它的探活历史与名字更可能是人手工维护的），
	// 其余删除；指向被删线路的账号稍后在账号循环里改写到规范 URL。
	groups := map[string][]*EgressProxy{}
	order := make([]string, 0, len(proxies))
	for _, p := range proxies {
		if p == nil {
			continue
		}
		canon, cerr := egress.Normalize(p.URL)
		if cerr != nil || canon == "" {
			continue // 非法 URL：不碰（它连探活都做不了，删了反而丢线索）
		}
		if len(groups[canon]) == 0 {
			order = append(order, canon)
		}
		groups[canon] = append(groups[canon], p)
	}
	keep := map[string]*EgressProxy{}
	for _, canon := range order {
		g := groups[canon]
		// 留**创建最早**的那条：它的探活历史与名字更可能是人手工维护的。
		best := g[0]
		for _, p := range g[1:] {
			if p.CreatedAt > 0 && (best.CreatedAt == 0 || p.CreatedAt < best.CreatedAt) {
				best = p
			}
		}
		for _, p := range g {
			if p.ID == best.ID {
				continue
			}
			if err := a.store.DeleteEgressProxy(p.ID); err != nil {
				log.Printf("WARN: [egress] 合并重复线路失败 %s(%s)：%v", p.ID, p.URL, err)
				continue
			}
			res.MergedLines++
			log.Printf("INFO: [egress] 合并重复线路 %s → %s（同一条代理的另一种写法）", p.URL, canon)
		}
		// ⚠️ 留下的那条本身也要**改写成规范 URL**（v0.31.33）。
		// 只删重复、不改写法的话，池里仍然留着 `HTTP://A:1/` 这种非规范串 ——
		// 账号侧规范化后会变成 `http://a:1`，两边又对不上了（就是这一版要修的问题）。
		if strings.TrimSpace(best.URL) != canon {
			best.URL = canon
			if err := a.store.UpsertEgressProxy(best); err != nil {
				log.Printf("WARN: [egress] 线路 %s 改写为规范写法失败：%v", best.ID, err)
			}
		}
		keep[canon] = best
	}

	// ---- 2) 账号侧：规范化 + 补登记 ----
	//
	// 只处理"有出口配置"的号；`direct` 与空串不是线路，跳过。
	var newLines []*EgressProxy
	for _, acc := range accs {
		if acc == nil {
			continue
		}
		raw := strings.TrimSpace(acc.EgressProxy)
		if raw == "" || egress.IsDirect(raw) {
			continue
		}
		canon, cerr := egress.Normalize(raw)
		if cerr != nil {
			res.Notes = append(res.Notes, acc.UID+" 的出口无法规范化（"+cerr.Error()+"），保持原样")
			res.Orphans++
			continue
		}
		if _, ok := keep[canon]; !ok {
			// 账号上有、池里没有：补登记（并保证本次不重复登记）。
			p, created, rerr := a.ensureEgressLineRegistered(canon, acc.OwnerUserID)
			if rerr != nil {
				res.Notes = append(res.Notes, acc.UID+" 的出口登记失败："+rerr.Error())
				res.Orphans++
				continue
			}
			keep[canon] = p
			if created {
				res.RegisteredLines++
				newLines = append(newLines, p)
			}
		}
		if canon != raw {
			if err := a.store.SetAccountEgress(acc.UID, canon); err != nil {
				res.Notes = append(res.Notes, acc.UID+" 改写失败："+err.Error())
				continue
			}
			res.RewrittenAccounts++
		}
	}

	// ---- 3) 可选：给"还没探到 IP"的线路补一次探活 ----
	//
	// 包含两类：本次新登记的 + 池里 last_ip 为空的历史线路。
	// 后者也补，是因为"账号代理的实际出口 IP"对它们同样没有值 —— 一致化的目的是
	// **两边都能回答"从哪个 IP 出去"**，不是只把新数据修对。
	if probeNew {
		todo := make([]*EgressProxy, 0, len(newLines)+4)
		seen := map[string]bool{}
		for _, p := range newLines {
			if p != nil && !seen[p.URL] {
				seen[p.URL] = true
				todo = append(todo, p)
			}
		}
		for _, p := range proxies {
			if p == nil || p.LastIP != "" || seen[p.URL] || !p.Enabled {
				continue
			}
			seen[p.URL] = true
			todo = append(todo, p)
		}
		const probeCap = 10
		if len(todo) > probeCap {
			res.Notes = append(res.Notes,
				"还有 "+itoa(len(todo)-probeCap)+" 条线路没探活，稍后由定时探活补齐")
			todo = todo[:probeCap]
		}
		for i, p := range todo {
			if i > 0 {
				// 串行 + 间隔：对齐不是压测，别把代理供应商打到限流。
				time.Sleep(350 * time.Millisecond)
			}
			a.checkEgressProxy(p)
			res.Probed++
		}
	}

	res.Orphans = 0
	for _, acc := range accs {
		if acc == nil {
			continue
		}
		raw := strings.TrimSpace(acc.EgressProxy)
		if raw == "" || egress.IsDirect(raw) {
			continue
		}
		canon, cerr := egress.Normalize(raw)
		if cerr != nil {
			res.Orphans++
			continue
		}
		if _, ok := keep[canon]; !ok {
			res.Orphans++
		}
	}
	return res, nil
}

// handleEgressConverge POST /api/admin/egress/converge
//
// body 可选：{"probe": true} —— 是否顺带探活拿 IP（默认 true，对齐的目的就是让 IP 立刻可见）。
func (a *app) handleEgressConverge(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Probe *bool `json:"probe"`
	}
	_ = decodeOptionalJSON(r, &req)
	probe := true
	if req.Probe != nil {
		probe = *req.Probe
	}
	res, err := a.convergeAccountEgress(probe)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	note := "账号出口与线路池已对齐"
	if res.MergedLines == 0 && res.RegisteredLines == 0 && res.RewrittenAccounts == 0 {
		note = "已经对齐，无需改动"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"result": res, "note": note,
	})
}

// decodeOptionalJSON 解析**可选**的请求体（空 body / 非法 JSON 都不报错）。
//
// 为什么要这么宽容：这个接口的语义是"执行一次对齐"，body 只用来微调行为。
// 因为 body 写错就让运维无法触发对齐，是把工具变成了新的障碍。
func decodeOptionalJSON(r *http.Request, v any) error {
	if r == nil || r.Body == nil {
		return nil
	}
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return nil // 空 body 会返回 EOF，与"没写 body"同义。
	}
	return nil
}

// orphanAccountEgress 统计"账号上有、池里没有"的出口条数（概览用）。
//
// 正常情况下恒为 0（写入路径都走 setAccountEgress，启动时还会对齐一次）；
// 非 0 说明有人绕过接口直接改了库，或者规范化失败 —— 两种情况都值得在页面报出来。
func (a *app) orphanAccountEgress() int {
	proxies, err := a.store.ListEgressProxies()
	if err != nil {
		return 0
	}
	have := make(map[string]bool, len(proxies))
	for _, p := range proxies {
		if canon, cerr := egress.Normalize(p.URL); cerr == nil && canon != "" {
			have[canon] = true
		}
	}
	accs, err := a.store.ListAccounts()
	if err != nil {
		return 0
	}
	n := 0
	for _, acc := range accs {
		raw := strings.TrimSpace(acc.EgressProxy)
		if raw == "" || egress.IsDirect(raw) {
			continue
		}
		canon, cerr := egress.Normalize(raw)
		if cerr != nil || !have[canon] {
			n++
		}
	}
	return n
}
