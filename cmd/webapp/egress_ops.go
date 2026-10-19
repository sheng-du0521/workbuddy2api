// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// egress_ops.go 出口代理的运维动作：失效自动迁移 + 候选批量探活导入（v0.24.6）。
//
// 这两件事的共同点是「让出口池自己保持可用」，都属于**可用性**工程：
//
//	1. 一条线路被探活判定为坏（连续失败达阈值已自动停用）之后，**绑在它上面的账号
//	   并不会自己回来** —— 它们仍然带着这个坏 URL，落到「出口异常」被排除出选号。
//	   自动迁移就是把它们搬到还活着的线路上，否则"自动停用坏线路"只做了一半。
//	2. 候选导入让运维把一批候选地址一次探完、只把通得过的收进池子，避免
//	   "手工一条条录、录完发现一半不通"。
//
// 关于「全自动采集公开免费代理」：**没有实现**，理由记在 handleEgressCandidates 的注释里
// 与 docs 中（一句话：把带账号凭证的请求交给不可信的第三方主机，是把自己的 token
// 送出去；且公开代理可用率实测约 1%，挂掉时会让整条链路不可用）。

package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"

	"workbuddy2api/internal/egress"
)

// handleEgressRecover 手工触发一次「坏线路上的账号自动迁移」。
// 定时探活每轮也会自动跑一次（见 probeAllEgress）。
func (a *app) handleEgressRecover(w http.ResponseWriter, r *http.Request) {
	rep := a.rehomeAccountsFromDeadProxies()
	writeJSON(w, http.StatusOK, rep)
}

// rehomeReport 一次自动迁移的结果（同时用于日志与接口返回）。
type rehomeReport struct {
	DeadProxies []string `json:"dead_proxies"` // 判定为坏、被迁空的线路
	Moved       int      `json:"moved"`        // 迁到其它可用线路的账号数
	// Kept 因**无可用线路**而保留原状的账号数（v0.24.8）。
	//
	// 它取代了旧字段 `to_direct`：旧实现在这种情形下把账号**改写成直连**，
	// 与本仓「宁可请求失败也不从本机 IP 裸奔」的原则正面矛盾，故已删除该行为。
	// 保留原状的账号会被判 egressBad 排除出选号 —— 不可服务，但出口仍是一条
	// 已知的坏线路，而不是本机 IP。
	Kept int `json:"kept"`
	// Blocked（R1，v0.26.1）严格模式（"不静默换出口"）下**被拦下、刻意不迁移**的账号数。
	//
	// 为什么单列而不是并进 Kept：两者的原因完全不同 ——
	//   Kept    = 想迁但**没有可用线路**（客观约束）；
	//   Blocked = **策略不允许换出口**（主观决定，改设置即可继续）。
	// 合在一起运维会以为"线路不够"，实际只需改一个开关。
	Blocked  int      `json:"blocked"`
	Mode     string   `json:"mode"` // 本次实际生效的策略：strict / migrate
	Warnings []string `json:"warnings,omitempty"`
}

// rehomeAccountsFromDeadProxies 把绑在**已停用/连续失败**线路上的账号迁到可用线路上。
//
// 判定「坏」的口径与自动停用一致：`Enabled == false`（停用即不再分配）**或**
// `FailStreak >= egressFailThreshold`。用两条是因为：手工停用的线路也该迁走
// （否则"我停用了它，账号还挂在上面"这个状态没人收尾）。
//
// 迁移目标选择（v0.24.8 修正口径）：在 `Enabled` 且 `FailStreak < 阈值` 的线路中
// 取**当前承载账号数最少**的一条，并**优先选该号主尚未占用**的（与分配阶段的首要
// 约束 isolate_owner 一致），同时不超过 `max_per_proxy` 上限（0 = 不限）。
//
// 注意判据是 `Enabled && FailStreak < 阈值`，**不看 `LastOK`** —— 此前注释写成
// 「`Enabled && LastOK`」与实现不符（`LastOK` 只反映最近一次探活结果，而"连续失败
// 达阈值"才是停用依据；刚建好还没探过的线路 LastOK 为 false，但它可用）。
//
// **没有可用线路时的处置是「保留原状 + 告警」，绝不退化为直连**（v0.24.8）：
//
//	旧实现在这里写 `egress.Direct`，理由是"可用性优先"。但它与本仓最关键的一条
//	防封原则正面矛盾 —— 选号侧写得很清楚：「宁可这个请求失败，也不让账号从本机
//	IP 裸奔出去」。后果是线路全挂时，全部账号在一个探活周期内被**静默改写成直连**，
//	防封链路整体失效，而唯一的痕迹只有一行日志。
//
//	保留原状则不会裸奔：账号仍绑在坏线路上 → 判 `egressBad` → **排除出选号** →
//	请求失败，但出口始终是那条坏线路而不是本机 IP。运维补上线路后下一轮探活自然
//	会把它迁走。**「失败」可以接受，「裸奔」不可以。**
//
// 刻意不做的两件事：
//   - **不**去池外找新线路（不自动采集 / 不自动启用已停用线路）—— 换哪条线路是
//     运维的决定，程序只保证"不吊死在那条已知坏掉的上"；
//   - **不**删账号的任何状态位 —— 迁移只改 egress_proxy 一个字段，其余（冷却 /
//     停用 / 分组）一概不动，避免"修出口"顺手改了别的东西。
//
// rehomeWriteValue 决定迁移时**写到账号上的值**（v0.31.47，纯函数）。
//
// 目标就是全局默认出口时写空串（= 跟随默认），而不是把默认出口的 URL 写死。
// 二者的区别在"下次重新应用本机出口"时才显形：
//   - 写空 → 仍算"没做过决定"，用户重新拉起自己的代理时会被正常接管；
//   - 写 URL → 看起来像"显式指定到这条线"，重新应用时被当成已决定而跳过，
//     于是用户重拉代理后号再也绑不回来（现场真实发生）。
func rehomeWriteValue(target, defEgress string) string {
	if strings.EqualFold(strings.TrimSpace(target), strings.TrimSpace(defEgress)) {
		return ""
	}
	return target
}

func (a *app) rehomeAccountsFromDeadProxies() rehomeReport {
	rep := rehomeReport{Mode: a.egressOnLineDownMode()}
	proxies, err := a.store.ListEgressProxies()
	if err != nil {
		rep.Warnings = append(rep.Warnings, "读取线路池失败："+err.Error())
		return rep
	}
	accs, err := a.store.ListAccounts()
	if err != nil {
		rep.Warnings = append(rep.Warnings, "读取账号失败："+err.Error())
		return rep
	}

	dead := map[string]bool{}    // URL → 坏
	load := map[string]int{}     // URL → 当前承载账号数
	healthy := map[string]bool{} // URL → 可用
	for _, p := range proxies {
		bad := !p.Enabled || p.FailStreak >= egressFailThreshold
		if bad {
			dead[p.URL] = true
			continue
		}
		// ⚠️ 用户提供的线路（owner>0）**不当迁移落点**（v0.31.23）。
		//
		// 自动迁移是"把坏线路上的号搬到最闲的好线路上"—— 如果用户线路在候选里，
		// 别人的号会被**自动**搬到一条我们控制不了的第三方出口上，
		// 而那些号带着 access_token（bearer 凭证）。这不是配置问题，是泄露。
		// 要让它承载谁的号，只能由管理员在卡片上**显式分配**。
		if p.OwnerUserID > 0 {
			continue
		}
		healthy[p.URL] = true
	}
	// 号主在某条健康线路上的占用：迁移时必须维持「同号主的账号不共用出口」
	// （v0.24.8 —— 分配阶段把它当第一约束，迁移阶段此前完全没看，于是迁移自己
	// 又把同人多号塞回同一条线路上，制造出最强的那条关联信号）。
	ownerUsed := map[int64]map[string]bool{}
	for _, acc := range accs {
		u := strings.TrimSpace(acc.EgressProxy)
		if u == "" || !healthy[u] {
			continue
		}
		load[u]++
		if ownerUsed[acc.OwnerUserID] == nil {
			ownerUsed[acc.OwnerUserID] = map[string]bool{}
		}
		ownerUsed[acc.OwnerUserID][u] = true
	}

	maxPer := a.store.egressMaxPerProxy()
	// 候选目标按当前负载升序，保证分散；同负载时按 URL 稳定排序，
	// 使同一批账号的迁移结果可复现（不然每次跑的落点都不一样，难核对）。
	candidates := make([]string, 0, len(healthy))
	for u := range healthy {
		candidates = append(candidates, u)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if load[candidates[i]] != load[candidates[j]] {
			return load[candidates[i]] < load[candidates[j]]
		}
		return candidates[i] < candidates[j]
	})

	// pickTarget 为该号主挑一条线路：优先「该号主未占用且未满」（满足隔离 + 容量），
	// 其次「未满但已被该号主占用」，都没有则返回空串（→ 保留原状）。
	pickTarget := func(owner int64) string {
		used := ownerUsed[owner]
		relaxed := ""
		for _, u := range candidates {
			if maxPer > 0 && load[u] >= maxPer {
				continue
			}
			if !used[u] {
				return u // 满足隔离且未满：最优，直接取（candidates 已按负载升序）
			}
			if relaxed == "" {
				relaxed = u
			}
		}
		return relaxed
	}

	for _, acc := range accs {
		cur := strings.TrimSpace(acc.EgressProxy)
		if cur == "" || !dead[cur] {
			continue
		}
		// 严格模式（R1，v0.26.1 默认）：**不静默换出口**。
		//
		// 账号留在原线路 → 判 egressBad → 排除出选号 → 不服务，也不裸奔。
		// 代价是可用性下降，换来的是"出口 IP 不会被程序在无人知晓时改掉"。
		if !a.egressAllowAutoRehome() {
			rep.Blocked++
			continue
		}
		target := pickTarget(acc.OwnerUserID)
		if target == "" {
			// 无可用线路（或全部满载）：**保留原状**，绝不退化为直连（v0.24.8）。
			//
			// 旧实现在这里写 `egress.Direct`（"可用性优先"），但它与选号侧的设计原则
			// 正面矛盾 ——「宁可这个请求失败，也不让账号从本机 IP 裸奔出去」。后果是
			// 线路全挂时全部账号在一个探活周期内被静默改写成直连，防封链路整体失效，
			// 而痕迹只有一行日志。
			//
			// 保留原状不会裸奔：账号仍绑在坏线路上 → 判 egressBad → 排除出选号 →
			// 请求失败，但出口仍是那条坏线路。**「失败」可以接受，「裸奔」不可以。**
			rep.Kept++
			continue
		}
		// 写到账号上的值（v0.31.47）：目标若就是**全局默认出口**，写空串而不是 URL。
		//
		// 为什么：写 URL 会把"这个号跟随默认"变成"这个号被显式指定到默认线路"，
		// 而后者在「重新应用本机出口」时会被当成"用户已做过决定"跳过 ——
		// 于是用户重新拉起自己的代理后，这些号**再也绑不回来**。
		// 现场：用户换了一条线路，旧线路挂过一次把号迁到默认，之后重拉代理，
		// 页面显示"已应用"，号却仍然从平台出口出去。
		//
		// 写空串保持"跟随默认"语义：默认变它就跟着变，重新应用时也能被正常接管。
		writeTo := rehomeWriteValue(target, a.defaultEgress())
		if err := a.store.SetAccountEgress(acc.UID, writeTo); err != nil {
			rep.Warnings = append(rep.Warnings, "迁移 "+acc.UID+" 失败："+err.Error())
			continue
		}
		load[target]++
		if ownerUsed[acc.OwnerUserID] == nil {
			ownerUsed[acc.OwnerUserID] = map[string]bool{}
		}
		ownerUsed[acc.OwnerUserID][target] = true
		rep.Moved++
	}
	for u := range dead {
		rep.DeadProxies = append(rep.DeadProxies, u)
	}
	sort.Strings(rep.DeadProxies)

	if rep.Moved+rep.Kept+rep.Blocked > 0 {
		_ = a.proxy.SyncAccounts()
		log.Printf("WARN: [egress] 出口线路失效处置（mode=%s）：坏线路 %d 条，迁移 %d 个账号，"+
			"保留原状 %d 个，策略拦截 %d 个",
			rep.Mode, len(rep.DeadProxies), rep.Moved, rep.Kept, rep.Blocked)
	}
	if rep.Blocked > 0 {
		rep.Warnings = append(rep.Warnings,
			"当前策略为 **strict（不静默换出口）**：坏线路上的 "+itoa(rep.Blocked)+
				" 个账号**刻意没有迁移** —— 换出口在上游眼里就是特征变化。"+
				"它们仍留在原线路（不会从本机 IP 裸奔）但在修复前不可服务。"+
				"处置二选一：①补一条可用线路后把账号手动改绑；②把设置项 `egress_on_line_down` "+
				"改成 `migrate` 允许自动迁移。")
	}
	if rep.Kept > 0 {
		rep.Warnings = append(rep.Warnings,
			"有账号因**无可用线路**而保留原状：它们不会从本机 IP 裸奔（这条原则优先于可用性），"+
				"但在补充线路前不可服务。请尽快补充可用出口")
	}
	return rep
}

// handleEgressCandidates 批量**探活**候选代理，只把通得过的收进池子。
//
// 请求体：{"urls": ["http://1.2.3.4:8080", ...], "region": "cn", "note": "..."}
// 其中 urls 也接受整段文本（按行/逗号/空白切分），方便直接从网上粘一批候选过来。
//
// 为什么是「粘一批 → 探活 → 只收通的」而不是「自动定期去公开列表里抓」：
//
//	① **凭证安全**：这些出站请求里带的是账号 access_token。公开免费代理是**不可信
//	   的第三方主机**，让它们经手账号凭证 = 把 token 交给陌生人。摘要里再写多少
//	   "只用于功能验证"都改变不了这一点。要用就用自己的出口（自建 VPS / 商业线路）。
//	② **可用率**：实测公开列表 2956 条里只有 38 条可用（1.3%），且随时会挂。
//	   把它们自动绑到生产账号上，等于给每个账号装一个随机定时炸弹。
//	③ **可核对**：人工批准过的线路，出问题时能回答"这条是谁加的、什么时候加的"。
//
// 所以这里提供的是**工效**（一次探完、只收通的、自动去重），而不是**全自动轮换**。
func (a *app) handleEgressCandidates(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URLs   any    `json:"urls"` // []string 或一整段文本
		Region string `json:"region"`
		Note   string `json:"note"`
		Import *bool  `json:"import"` // 缺省 true：通得过的直接入池
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	raw := flattenCandidates(req.URLs)
	if len(raw) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "未提供候选地址"})
		return
	}
	// 校验 + 去重（同一地址重复贴进来只探一次）。
	seen := map[string]bool{}
	urls := make([]string, 0, len(raw))
	invalid := []string{}
	for _, u := range raw {
		norm, err := egress.Validate(u)
		if err != nil || norm == "" || egress.IsDirect(norm) {
			invalid = append(invalid, u)
			continue
		}
		if seen[norm] {
			continue
		}
		seen[norm] = true
		urls = append(urls, norm)
	}
	if len(urls) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "候选地址全部非法", "invalid": invalid})
		return
	}

	// 并发探测（上限 16）：候选可能有几百条，串行太慢；但也不能无限并发
	// —— 探测本身是出网行为，别把本机连接数打满。
	const parallel = 16
	type candResult struct {
		URL     string `json:"url"`
		OK      bool   `json:"ok"`
		ExitIP  string `json:"exit_ip,omitempty"`
		Note    string `json:"note,omitempty"`
		Unknown bool   `json:"unknown,omitempty"` // 探测端点不可达 → 结论未知
	}
	results := make([]candResult, len(urls))
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i, u := range urls {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, u string) {
			defer wg.Done()
			defer func() { <-sem }()
			pr := egress.Probe(u)
			results[i] = candResult{URL: u, OK: pr.OK, ExitIP: pr.IP,
				Note: pr.Error, Unknown: pr.EndpointUnreachable}
		}(i, u)
	}
	wg.Wait()

	// 已存在的地址不重复录入（探活仍然照做 —— 运维可能就想确认老线路还活着）。
	existing := map[string]bool{}
	if list, err := a.store.ListEgressProxies(); err == nil {
		for _, p := range list {
			existing[p.URL] = true
		}
	}
	doImport := req.Import == nil || *req.Import
	imported, skipped := 0, 0
	if doImport {
		for _, c := range results {
			if !c.OK || existing[c.URL] {
				skipped++
				continue
			}
			p := &EgressProxy{
				ID: newRandomID(8), Name: egressHostLabel(c.URL),
				URL: c.URL, Region: strings.TrimSpace(req.Region),
				Note: strings.TrimSpace(req.Note), Enabled: true,
			}
			if p.Name == "" {
				p.Name = c.URL
			}
			if err := a.store.UpsertEgressProxy(p); err != nil {
				skipped++
				continue
			}
			existing[c.URL] = true
			imported++
		}
	}
	okCount := 0
	for _, c := range results {
		if c.OK {
			okCount++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tested": len(urls), "ok": okCount, "imported": imported,
		"skipped": skipped, "invalid": invalid,
		"results": results, "dry_run": !doImport,
	})
}

// flattenCandidates 把 urls 字段收敛成字符串列表：既接受 JSON 数组，也接受一整段文本。
func flattenCandidates(v any) []string {
	split := func(s string) []string {
		out := []string{}
		// 先按行切、**先剥注释**，再分词。
		// 顺序不能反：反了的话 `# 注释` 会被空白切分成 "#" 与 "注释" 两个 token，
		// 前者被 HasPrefix 滤掉、后者留下来被当成一个候选地址（测试抓到过这个 bug）。
		for _, line := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
			if i := strings.Index(line, "#"); i >= 0 {
				line = line[:i]
			}
			for _, tok := range strings.FieldsFunc(line, func(r rune) bool {
				return r == ',' || r == ' ' || r == '\t' || r == ';'
			}) {
				if t := strings.TrimSpace(tok); t != "" {
					out = append(out, t)
				}
			}
		}
		return out
	}
	switch x := v.(type) {
	case string:
		return split(x)
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, split(s)...)
			}
		}
		return out
	default:
		return nil
	}
}

// egressHostLabel 从代理 URL 取一个便于阅读的名字（如 1.2.3.4:8080）。
func egressHostLabel(u string) string {
	s := u
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
