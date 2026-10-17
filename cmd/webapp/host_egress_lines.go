// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 用户控制台「账号出口」：让用户把自己的号**逐号**分到不同的出口线路上（v0.31.31）。
//
// # 为什么必须有这一层
//
// 在此之前，用户端只有两条动作：
//
//	① 「设置 → 我的出口」填一个代理 URL，然后「应用到全部代挂号」；
//	② 代理页「一键拉起」→ 应用 = 把我名下**所有**号绑到刚拉起的那条线路。
//
// 两条都是"全量覆盖"。于是拥有两台电脑的用户会看到：
// 在 B 机上点一次拉起，A 机上的号全被搬走 —— 两台电脑最后只有一台在真正承重，
// 而他看到的"出口 IP"自然是同一个。软件没法把一台电脑变出两个公网 IP，
// 但**必须**能把不同的号分到不同的出口上，否则"两个 IP"永远只是纸面上的。
//
// 三件事：
//
//	GET  /api/host/egress/lines     我的号 + 我能用的线路 + 每个号当前实际出口
//	POST /api/host/egress/assign    逐号 / 批量指定出口（限本人号、本人线路或平台线路）
//	POST /api/host/egress/rebalance 按**不同出口 IP** 均衡（同 IP 的线路只算一个）
//
// # 边界（越权是这里唯一的高危点）
//
//   - 目标账号一律 `OwnerUserID == 我`，别人的号静默剔除（不报错、不泄露存在性）；
//   - 可选线路只有 `owner_user_id ∈ {0, 我}` 的启用线路：**平台线路可用、别人的线路不可用**
//     （别人的线路 = 别人的代理，把 access_token 从那儿送出去等于把号交给他）；
//   - 只写 `accounts.egress_proxy` 一个字段，不碰全局默认出口、不碰任何人的账号状态。
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strings"

	"workbuddy2api/internal/egress"
)

// egressLineView 用户视角的一条可用线路。
type egressLineView struct {
	URL         string `json:"url"`
	Name        string `json:"name"`
	OwnerUserID int64  `json:"owner_user_id"`
	// Mine 这条线路是**我自己的机器**接进来的（可以随便用）。
	Mine bool `json:"mine"`
	// Platform 平台线路（所有用户都可以用；不是我的，但也不是别人的私线）。
	Platform bool `json:"platform"`
	// Foreign **其他用户**接进来的线路（v0.31.32）。
	//
	// 产品上明确要求"支持走别人的"：住宅 IP 是稀缺资源，谁接进来的都能被借用。
	// 代价必须写在脸上 —— 流量经过那台机器，`access_token` 对它的主人可见。
	// 所以这个标志不是"禁用"，而是"前端必须显式标注并在切换前确认"。
	Foreign bool `json:"foreign"`
	// OwnerName 线路主人的用户名（平台线路为空）。借用时至少要知道借的是谁的。
	OwnerName string `json:"owner_name,omitempty"`
	Enabled   bool   `json:"enabled"`
	LastOK    bool   `json:"last_ok"`
	// ExitIP 归一化后的出口 IP（空 = 还没探到）。
	ExitIP string `json:"exit_ip,omitempty"`
	// BoundMine 我名下已经绑在这条线路上的号数。
	BoundMine int `json:"bound_mine"`
	// BoundTotal 这条线路上一共绑了多少号（含别人的，只给数字，不给身份）。
	BoundTotal int `json:"bound_total"`
	// Note 为什么它不可选（空 = 可选）。
	Note string `json:"note,omitempty"`
}

// hostEgressAccountView 我名下某个号当前的出口状况。
type hostEgressAccountView struct {
	UID       string `json:"uid"`
	Nickname  string `json:"nickname,omitempty"`
	PoolState string `json:"pool_state,omitempty"`
	// Configured 账号上显式配置的值：""（未指定）/ "direct" / URL。
	Configured string `json:"configured"`
	// Resolved 实际生效的线路：""（= 直连，没有默认出口）/ "direct" / URL。
	Resolved string `json:"resolved"`
	// ResolvedName 实际生效线路的名字（给人看）。
	ResolvedName string `json:"resolved_name,omitempty"`
	// ExitIP 该线路**最近一次探活**看到的出口 IP（空 = 未知）。
	//
	// ⚠️ 这是"线路的出口"，不是"这次请求的出口"：同一个号在两次请求之间可能因为
	// 探活更新而换 IP（家宽重播号）。所以文案写「线路出口」，不写「当前出口」。
	ExitIP string `json:"exit_ip,omitempty"`
	// ExitSource line / default / direct / unknown
	ExitSource string `json:"exit_source"`
	// TokenDead 该号登录已失效（12153），此时改出口没有意义。
	TokenDead bool `json:"token_dead,omitempty"`
}

// handleHostEgressLines GET /api/host/egress/lines
func (a *app) handleHostEgressLines(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	if u == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "未登录"})
		return
	}
	accs, err := a.store.ListAccountsByOwner(u.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	proxies, err := a.store.ListEgressProxies()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	def := a.defaultEgress()
	dead := a.store.TokenDeadMap()

	byURL := make(map[string]*EgressProxy, len(proxies))
	for _, p := range proxies {
		byURL[strings.TrimSpace(p.URL)] = p
	}

	// 线路视图：只给"我能用"的（平台 + 我自己的）。
	lines := make([]egressLineView, 0, len(proxies))
	mineCount := map[string]int{}
	for _, acc := range accs {
		if v := strings.TrimSpace(acc.EgressProxy); v != "" && !egress.IsDirect(v) {
			mineCount[v]++
		}
	}
	totalCount := map[string]int{}
	if all, err := a.store.ListAccounts(); err == nil {
		for _, acc := range all {
			if v := strings.TrimSpace(acc.EgressProxy); v != "" && !egress.IsDirect(v) {
				totalCount[v]++
			}
		}
	}
	for _, p := range proxies {
		url := strings.TrimSpace(p.URL)
		mine := p.OwnerUserID == u.ID
		platform := p.OwnerUserID == 0
		foreign := p.OwnerUserID > 0 && !mine
		lv := egressLineView{
			URL: url, Name: p.Name, OwnerUserID: p.OwnerUserID,
			Mine: mine, Platform: platform, Foreign: foreign,
			Enabled: p.Enabled, LastOK: p.LastOK,
			ExitIP:     normalizeExitIP(p.LastIP),
			BoundMine:  mineCount[url],
			BoundTotal: totalCount[url],
		}
		if foreign {
			lv.OwnerName = a.userNameByID(p.OwnerUserID)
		}
		// 只有"停用"是不可选的：其他用户的线路**可选**（v0.31.32 的产品决定），
		// 但前端必须标注主人并在切换前确认"流量会经过他的机器"。
		// 这里刻意不写 note —— note 的语义是"不可选的原因"。
		if !p.Enabled {
			lv.Note = "这条线路已被停用（连续探活失败或人工停用）"
		}
		lines = append(lines, lv)
	}
	sort.SliceStable(lines, func(i, j int) bool {
		// 自己的线路排前面（最常用），其次平台线路，再按 URL 稳定排序。
		if lines[i].Mine != lines[j].Mine {
			return lines[i].Mine
		}
		if lines[i].Platform != lines[j].Platform {
			return lines[i].Platform
		}
		return lines[i].URL < lines[j].URL
	})

	// 账号视图
	out := make([]hostEgressAccountView, 0, len(accs))
	for _, acc := range accs {
		out = append(out, a.hostEgressAccountOf(acc, byURL, def, dead))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Nickname != out[j].Nickname {
			return out[i].Nickname < out[j].Nickname
		}
		return out[i].UID < out[j].UID
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"lines":          lines,
		"accounts":       out,
		"default_egress": def,
		// distinct_exit_ips 我能用的线路里有几个**不同**的出口 IP —— 这是"能不能
		// 真的分散开"的唯一度量。两条线路同 IP 时它只会是 1。
		"distinct_exit_ips": countDistinctLineIPs(lines),
	})
}

// countDistinctLineIPs 可用线路里不同出口 IP 的个数（忽略未知 IP 的线路）。
func countDistinctLineIPs(lines []egressLineView) int {
	seen := map[string]bool{}
	for _, l := range lines {
		if l.Note != "" || l.ExitIP == "" {
			continue
		}
		seen[l.ExitIP] = true
	}
	return len(seen)
}

// hostEgressAccountOf 组装单个账号的出口视图（纯函数式，无副作用）。
func (a *app) hostEgressAccountOf(acc *Account, byURL map[string]*EgressProxy, def string,
	dead map[string]int64) hostEgressAccountView {
	v := hostEgressAccountView{
		UID: acc.UID, Nickname: acc.Nickname, PoolState: acc.PoolState,
		Configured: strings.TrimSpace(acc.EgressProxy),
	}
	if _, ok := dead[acc.UID]; ok {
		v.TokenDead = true
	}
	switch {
	case egress.IsDirect(v.Configured):
		v.Resolved, v.ExitSource = egress.Direct, "direct"
	case v.Configured != "":
		v.Resolved, v.ExitSource = v.Configured, "line"
	case def != "":
		v.Resolved, v.ExitSource = def, "default"
	default:
		// 未指定 + 无全局默认 = 从平台机器自己的 IP 出去。这不是"线路"，
		// 所以没有可展示的出口 IP（除非运维自己知道机房 IP）。
		v.Resolved, v.ExitSource = "", "direct"
	}
	if p := byURL[v.Resolved]; p != nil {
		v.ResolvedName = p.Name
		v.ExitIP = normalizeExitIP(p.LastIP)
	} else if v.ExitSource == "line" {
		v.ResolvedName = "（不在线路池）"
	}
	return v
}

// handleHostEgressAssign POST /api/host/egress/assign
//
// body: {"uids":["..."], "all":false, "proxy":"http://...|direct|"}
func (a *app) handleHostEgressAssign(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	if u == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "未登录"})
		return
	}
	var req struct {
		UIDs  []string `json:"uids"`
		All   bool     `json:"all"`
		Proxy string   `json:"proxy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	norm, err := egress.ValidateAccountEgress(req.Proxy)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	// 线路必须"我能用"：平台线路 or 我自己的。别人的线路一律拒（理由见文件头）。
	var line *EgressProxy
	if norm != "" && !egress.IsDirect(norm) {
		list, err := a.store.ListEgressProxies()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		for _, p := range list {
			if strings.TrimSpace(p.URL) == norm {
				line = p
				break
			}
		}
		if line == nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "这条线路不在线路池里。请先在你的电脑上「一键拉起」，或由管理员录入该线路",
			})
			return
		}
		if !line.Enabled {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "这条线路已被停用（连续探活失败或人工停用），不能指派给账号",
			})
			return
		}
		// 别人的线路**允许借用**（v0.31.32，产品决定）：住宅 IP 稀缺，谁的机器都可以承载。
		// 留痕放在 target 算完之后（见下），因为日志要带上"一共多少个号"，
		// 而那时候才知道目标集有多少 —— 排查时那个数字比什么都好用。
	}

	owned, err := a.store.ListAccountsByOwner(u.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	own := map[string]bool{}
	for _, acc := range owned {
		own[acc.UID] = true
	}
	var targets []string
	if req.All {
		for _, acc := range owned {
			targets = append(targets, acc.UID)
		}
	} else {
		for _, uid := range req.UIDs {
			uid = strings.TrimSpace(uid)
			// 越权目标**静默剔除**：报错会变成"这个 uid 存不存在"的探针。
			if uid != "" && own[uid] {
				targets = append(targets, uid)
			}
		}
	}
	if len(targets) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "没有可设置的账号（请确认勾选的账号属于你）"})
		return
	}

	// 借用他人出口的审计（v0.31.32）：出问题时"谁的号走过谁的机器"是第一条线索，
	// 所以这里用 WARN 级别 —— 它不该被常规日志淹掉。
	if line != nil && line.OwnerUserID != 0 && line.OwnerUserID != u.ID {
		log.Printf("WARN: [host] 用户 %s(#%d) 借用用户 #%d 的出口 %s 承载 %d 个号"+
			"（这些请求的 access_token 会经过对方机器）",
			u.Username, u.ID, line.OwnerUserID, strings.TrimSpace(line.URL), len(targets))
	}

	changed := 0
	for _, uid := range targets {
		if err := a.store.SetAccountEgress(uid, norm); err != nil {
			log.Printf("WARN: [host] 设置代挂出口失败 acct=%s: %v", uid, err)
			continue
		}
		changed++
	}
	_ = a.proxy.SyncAccounts()

	res := map[string]any{
		"updated": changed, "total": len(targets), "proxy": norm,
	}
	if line != nil {
		res["proxy_name"] = line.Name
		res["foreign"] = line.OwnerUserID > 0 && line.OwnerUserID != u.ID
		if line.OwnerUserID > 0 && line.OwnerUserID != u.ID {
			res["owner_name"] = a.userNameByID(line.OwnerUserID)
		}
		res["exit_ip"] = normalizeExitIP(line.LastIP)
		if res["exit_ip"] == "" {
			res["note"] = "已保存。这条线路还没有探活记录，出口 IP 稍后（最多 5 分钟）会在列表里出现"
		}
	}
	log.Printf("INFO: [host] 用户 %s 把 %d/%d 个代挂号的出口设为 %q", u.Username, changed, len(targets), norm)
	writeJSON(w, http.StatusOK, res)
}

// handleHostEgressRebalance POST /api/host/egress/rebalance
//
// 把**我名下的号**按不同出口 IP 尽量均分：每个 IP 一组，组内轮流落到该 IP 下的线路。
// 这是"两台电脑两个宽带"之后用户真正想要的一步 —— 光有两条线路不够，
// 还得让号分散上去，否则所有号仍然挤在其中一条上（看起来就是"只有一个出口"）。
func (a *app) handleHostEgressRebalance(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	if u == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "未登录"})
		return
	}
	var req struct {
		UIDs []string `json:"uids"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	owned, err := a.store.ListAccountsByOwner(u.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if len(owned) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "你还没有代挂账号"})
		return
	}
	want := map[string]bool{}
	for _, x := range req.UIDs {
		if s := strings.TrimSpace(x); s != "" {
			want[s] = true
		}
	}
	targets := make([]*Account, 0, len(owned))
	for _, acc := range owned {
		// 只动"明确选中"的号；未给名单 = 全部。
		if len(want) > 0 && !want[acc.UID] {
			continue
		}
		targets = append(targets, acc)
	}
	if len(targets) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "没有匹配的账号（请确认勾选的账号属于你）"})
		return
	}
	sort.SliceStable(targets, func(i, j int) bool { return targets[i].UID < targets[j].UID })

	proxies, err := a.store.ListEgressProxies()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// 可用线路 + 按出口 IP 归并（同一 IP 的多条线路只算一个出口）。
	// 可用线路 = 平台 + 我的 + **别人的**（v0.31.32）。别人的线路也能承载我的号，
	// 前端会在切换前提示"流量经过他的机器"，这里不再拦。
	usable := make([]*EgressProxy, 0, len(proxies))
	for _, p := range proxies {
		if !p.Enabled {
			continue
		}
		usable = append(usable, p)
	}
	groups, warns := groupEgressLinesByExit(usable, u.ID)
	// 借用别人的线路时明确说一句：这是"能用"而不是"没代价"。
	borrowed := map[int64]bool{}
	for _, g := range groups {
		if g.proxy != nil && g.proxy.OwnerUserID > 0 && g.proxy.OwnerUserID != u.ID {
			borrowed[g.proxy.OwnerUserID] = true
		}
	}
	if len(borrowed) > 0 {
		warns = append(warns, "本次分配用到了他人提供的出口：流量会经过对方的机器（凭证对它可见）")
	}
	if len(groups) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "没有可用线路：请先在自己的电脑上「一键拉起」，或联系管理员添加线路",
		})
		return
	}
	if len(groups) == 1 {
		warns = append(warns, "当前只有 1 个出口 IP —— 把所有号分上去也就是同一个 IP。"+
			"要真正分散，需要另一台电脑（另一条宽带）接入，或在管理后台加一条不同出口的线路")
	}

	// 轮流落到各组：第 i 个号 → 第 i%len(groups) 组。确定性（按 uid 排序）便于复核。
	perLine := map[string]int{}
	perExit := map[string]int{}
	changed := 0
	for i, acc := range targets {
		g := groups[i%len(groups)]
		line := g.proxy
		if err := a.store.SetAccountEgress(acc.UID, strings.TrimSpace(line.URL)); err != nil {
			log.Printf("WARN: [host] 均衡分配失败 acct=%s: %v", acc.UID, err)
			continue
		}
		changed++
		perLine[line.URL]++
		perExit[g.exitIP]++
	}
	_ = a.proxy.SyncAccounts()

	lines := make([]map[string]any, 0, len(perLine))
	for url, n := range perLine {
		name := url
		for _, p := range usable {
			if strings.TrimSpace(p.URL) == url {
				name = p.Name
				break
			}
		}
		lines = append(lines, map[string]any{"url": url, "name": name, "count": n})
	}
	sort.SliceStable(lines, func(i, j int) bool { return lines[i]["url"].(string) < lines[j]["url"].(string) })

	exits := make([]map[string]any, 0, len(perExit))
	for ip, n := range perExit {
		exits = append(exits, map[string]any{"exit_ip": ip, "count": n})
	}
	sort.SliceStable(exits, func(i, j int) bool { return exits[i]["exit_ip"].(string) < exits[j]["exit_ip"].(string) })

	log.Printf("INFO: [host] 用户 %s 按出口均衡：%d/%d 个号，%d 个出口 IP",
		u.Username, changed, len(targets), len(perExit))
	writeJSON(w, http.StatusOK, map[string]any{
		"assigned": changed, "total": len(targets),
		"lines": lines, "exits": exits,
		"distinct_exit_ips": len(perExit),
		"warnings":          warns,
	})
}

// hostExitPick 同一个出口 IP 下选出的代表线路（含该 IP）。
//
// 不复用 egress_exit.go 的 egressExitGroup：那个是**报表用**的聚合视图
// （只含 ≥2 条的组、字段全是 json），这里是**分配用**的决策结果（要有代表线路指针）。
// 两个用途混用一个结构，最后一定会为了一个改坏另一个。
type hostExitPick struct {
	exitIP string
	proxy  *EgressProxy
}

// groupEgressLinesByExit 按出口 IP 归并线路，选出每组"最该被选"的那条。
//
// 代表线路的优先级：**用户自己的 → 平台线路 → 别的**；同优先级下取探活成功的，
// 再按 URL 稳定排序。这样"我有两条线同 IP"时不会一半号分到平台线、
// 一半分到自己的线（那看起来像分散，实际是同一个 IP）。
func groupEgressLinesByExit(list []*EgressProxy, me int64) ([]hostExitPick, []string) {
	byIP := map[string][]*EgressProxy{}
	unknown := make([]*EgressProxy, 0, 1)
	for _, p := range list {
		if p == nil {
			continue
		}
		ip := normalizeExitIP(p.LastIP)
		if ip == "" {
			unknown = append(unknown, p)
			continue
		}
		byIP[ip] = append(byIP[ip], p)
	}
	ips := make([]string, 0, len(byIP))
	for ip := range byIP {
		ips = append(ips, ip)
	}
	sort.Strings(ips)

	out := make([]hostExitPick, 0, len(ips)+len(unknown))
	warns := []string{}
	for _, ip := range ips {
		g := byIP[ip]
		sort.SliceStable(g, func(i, j int) bool {
			oi, oj := ownerRank(g[i].OwnerUserID, me), ownerRank(g[j].OwnerUserID, me)
			if oi != oj {
				return oi < oj
			}
			if g[i].LastOK != g[j].LastOK {
				return g[i].LastOK
			}
			return g[i].URL < g[j].URL
		})
		out = append(out, hostExitPick{exitIP: ip, proxy: g[0]})
		if len(g) > 1 {
			warns = append(warns, "出口 "+ip+" 上有 "+itoa(len(g))+" 条线路，按 IP 只当 1 个出口用（轮流分上去不会更分散）")
		}
	}
	// 还没探到 IP 的线路排在最后：它可能可用，但不该先被当成一个"出口"。
	for _, p := range unknown {
		out = append(out, hostExitPick{exitIP: "", proxy: p})
	}
	return out, warns
}

// ownerRank 选代表线路时的优先级（**越小越优先**）。
//
// 顺序：自己的 → 平台的 → 别人的。理由不是"所有权歧视"，而是**代价**：
// 走自己的线最快（本机直连）、走平台的线最稳（运维负责）、走别人的线要经过第三方。
// 同一条 IP 有多个选择时，先选代价最小的那条。
func ownerRank(owner, me int64) int {
	switch {
	case me > 0 && owner == me:
		return 0
	case owner == 0:
		return 1
	default:
		return 2
	}
}
