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
//   - 可选线路只有 `owner_user_id ∈ {0, 我}` 的启用线路：**平台线路可用、别人的线路不可用**；
//   - 只写 `accounts.egress_proxy` 一个字段，不碰全局默认出口、不碰任何人的账号状态。
//
// # 规则变更（v0.31.34）：他人线路一律不出现在用户视角
//
// v0.31.32 曾按"住宅 IP 稀缺、谁的机器都能承载"放开过借用（列出他人线路 + 切换前确认）。
// 实际跑下来这条规则的**代价被低估了**：
//
//	① 凭证可见是架构事实而非提示问题 —— 承载方机器上的代理进程必然看到
//	   `Authorization: Bearer …`，一次"确认"弹窗挡不住任何技术风险；
//	② 责任归属说不清 —— 出了风控事件，无法区分"我的号自己跑的"与"从别人家宽跑的"；
//	③ 用户的线路本来就是为了**自己的号**拿一个住宅 IP，借给别人用只是额外风险。
//
// 所以现在：**用户的线路只承载自己的号**（账号代理与出口代理都是）。他人线路
// 在用户端**不返回、不可选、不可分配**；需要把某条线路用在别人的号上，
// 唯一路径是管理员在管理后台显式分配（那里有归属、审计与停用能力）。
//
// 字段 `Foreign` / `OwnerName` 保留但恒为空值：协议兼容，且管理端仍要区分来源。
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
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
	// Foreign **其他用户**接进来的线路 —— v0.31.34 起用户端**不再返回**这类线路，
	// 因此该字段恒 false。保留是为了：① 前端旧版本不至于因缺字段报错；
	// ② 管理端视图仍需要"这条是谁的"这一维度（那里用 OwnerUserID/OwnerName）。
	Foreign bool `json:"foreign"`
	// OwnerName 线路主人的用户名（平台线路为空）。用户端现在只会看到自己的线路，
	// 所以通常是"我自己"或空；保留字段便于管理端复用同一份结构。
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
	// RefreshLost refresh 已失效但 access 仍可用（v0.31.37）。
	//
	// 与 TokenDead 分开是必须的：前者是"这个号现在不能服务"，后者是"还能服务，
	// 但不会自动续期"。并成一个布尔会让"还有一个月可用"的号在界面上与死号长得一样 ——
	// 用户就会去重登一个其实没坏的号，或者干脆忽略掉真的警告。
	RefreshLost bool `json:"refresh_lost,omitempty"`
	// ForeignBound 这个号当前走的是**别人的线路**（v0.31.34）。
	//
	// 规则收紧后用户不能再选别人的线路，但**存量绑定还在跑**：v0.31.32 那段
	// "可借用"时期留下的数据不会自己消失。不标出来，用户会以为"这个号有出口"，
	// 而它其实正经过另一个用户的机器（凭证对对方可见）。
	//
	// 处置由用户决定（下拉改成自己的/平台线路即可），系统**不自动改** ——
	// 改出口会改变这个号的风控画像，不该由一次版本升级替人做决定。
	ForeignBound bool `json:"foreign_bound,omitempty"`
	// ForeignOwnerName 这条线路的主人（给"要不要改回去"提供判断依据）。
	ForeignOwnerName string `json:"foreign_owner_name,omitempty"`
}

// errEgressForeignLine 线路属于他人 —— 用**哨兵错误**而不是匹配文案来判状态码。
//
// 为什么（v0.31.35）：调用方需要区分 403（别人的线路，找管理员）与 400
// （不在池里 / 已停用，自己可处理）。此前用 `strings.Contains(err, "其他用户")`，
// 文案一改状态码就悄悄变成 400 —— 那种漂移没有任何测试会红，只会让前端弹错提示。
var errEgressForeignLine = fmt.Errorf("这条线路属于其他用户，你的号不能用它。" +
	"用户的线路只承载自己的账号；如果确实需要，请让管理员在「代理 → 线路池 → 分配」里指定")

// hostEgressLineFor 校验「这条线路能不能被该用户用在自己的号上」。
//
// **规则只有这一份实现**（v0.31.34 补漏）：此前 `assign` 有校验、而设置页的
// 「我的出口 → 应用到全部」没有 —— 用户在设置页手填 `http://127.0.0.1:21160`
// （别人的线路）就能把号绑上去，接口层完全绕过规则。这属于"同一个概念两处判定"，
// 迟早会漂；现在两个入口共用本函数。
//
// allowNew 区分两种动作（这就是"规则一致、动作不同"的落点）：
//
//	true  设置页「我的出口」：用户填自由文本，**允许是他自己新接的代理** ——
//	      池里没有就返回 (nil,nil)，由 `ensureUserEgressLine` / `setAccountEgress`
//	      登记成他自己名下的线路。堵掉这条等于"用户再也不能新增自己的代理"，
//	      那不是收紧规则，是把功能删了。
//	false 逐号分配下拉：只能选**池里已有**的线路。允许任意 URL 会让
//	      "随手写个地址"就多出一条线路，池子很快就没人维护得动。
//
// 返回 (nil, nil) 表示「直连 / 清空跟随默认 / 允许新建」—— 调用方按 allowNew 理解。
// 错误文案已经面向用户写好（含下一步该找谁），调用方直接 400/403 透出即可。
func (a *app) hostEgressLineFor(u *User, norm string, allowNew bool) (*EgressProxy, error) {
	if u == nil || norm == "" || egress.IsDirect(norm) {
		return nil, nil
	}
	list, err := a.store.ListEgressProxies()
	if err != nil {
		return nil, fmt.Errorf("读取线路池失败：%v", err)
	}
	want := egressURLKey(norm)
	var line *EgressProxy
	for _, p := range list {
		if egressURLKey(p.URL) == want {
			line = p
			break
		}
	}
	if line == nil {
		if allowNew {
			// 用户自己新接的代理：放行，调用方会登记为他的线路。
			return nil, nil
		}
		return nil, fmt.Errorf("这条线路不在线路池里。请先在你的电脑上「一键拉起」，或由管理员录入该线路")
	}
	// ⚠️ **先判归属，再判启用**（v0.31.35 由测试逼出来的顺序）。
	//
	// 反过来的话，用户填了一条"别人的、且已被停用"的线路会收到
	// 「这条线路已被停用」—— 他会以为是自己的线路坏了、等探活恢复就好；
	// 而真正的事实是"这条根本不是你的"。两个结论的处理方式完全不同：
	// 停用是**等**，归属是**别等了**。错误信息必须说后者。
	// 他人的线路一律拒（产品规则，不是越权兜底）：用户的线路只承载自己的号。
	if line.OwnerUserID != 0 && line.OwnerUserID != u.ID {
		return nil, errEgressForeignLine
	}
	if !line.Enabled {
		return nil, fmt.Errorf("这条线路已被停用（连续探活失败或人工停用），不能指派给账号")
	}
	return line, nil
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
	// 「仅剩 access」也一起取（v0.31.37）：用户端要能在号还没坏的时候看到预警。
	lost := a.store.RefreshLostMap()

	byURL := make(map[string]*EgressProxy, len(proxies))
	for _, p := range proxies {
		byURL[egressURLKey(p.URL)] = p
	}

	// 线路视图：只给"我能用"的（平台 + 我自己的）。
	// 他人的线路**完全不返回**（v0.31.34）：以前会列出来并标"可选/需确认"，但那条规则
	// 的实际效果是"把凭证交给第三方机器"，而弹窗确认不是技术保障。见文件头说明。
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
		// 他人的线路直接跳过（不返回、不可选）。这条判断放在最前面，
		// 是为了让"看不见"成为结构性结果，而不是靠前端不渲染。
		if p.OwnerUserID > 0 && !mine {
			continue
		}
		foreign := false
		lv := egressLineView{
			URL: url, Name: p.Name, OwnerUserID: p.OwnerUserID,
			Mine: mine, Platform: platform, Foreign: foreign,
			Enabled: p.Enabled, LastOK: p.LastOK,
			ExitIP:     normalizeExitIP(p.LastIP),
			BoundMine:  mineCount[url],
			BoundTotal: totalCount[url],
		}
		// 只有"停用"是不可选的原因（可见的线路都是自己能用的）。
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
		out = append(out, a.hostEgressAccountOf(acc, byURL, def, dead, lost))
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
	dead, lost map[string]int64) hostEgressAccountView {
	v := hostEgressAccountView{
		UID: acc.UID, Nickname: acc.Nickname, PoolState: acc.PoolState,
		Configured: strings.TrimSpace(acc.EgressProxy),
	}
	if _, ok := dead[acc.UID]; ok {
		v.TokenDead = true
	} else if _, ok := lost[acc.UID]; ok {
		// 真死优先（MarkTokenDead 已清 lost 位，这里只是防御历史行同时存在两位）。
		v.RefreshLost = true
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
	if p := byURL[egressURLKey(v.Resolved)]; p != nil {
		v.ResolvedName = p.Name
		v.ExitIP = normalizeExitIP(p.LastIP)
		// 越界绑定：号主的号指向了别人的线路（存量数据 / 管理员分配）。
		// 对用户而言这条线路是"看不见的"（lines 里已过滤），所以必须在这里说明，
		// 否则他只能看到一个出口 IP，却找不到自己什么时候配过它。
		if p.OwnerUserID > 0 && p.OwnerUserID != acc.OwnerUserID {
			v.ForeignBound = true
			v.ForeignOwnerName = a.userNameByID(p.OwnerUserID)
			if v.ForeignOwnerName == "" {
				v.ForeignOwnerName = "用户 #" + strconv.FormatInt(p.OwnerUserID, 10)
			}
		}
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

	// 线路必须"我能用"：平台线路 or 我自己的（规则与设置页共用一份实现）。
	line, lerr := a.hostEgressLineFor(u, norm, false)
	if lerr != nil {
		// 404/403 的区分对调用方有意义（"不在池里"可自救，"别人的"只能找管理员），
		// 但对外统一 400/403 两种即可：文案已经说清该做什么。
		status := http.StatusBadRequest
		if errors.Is(lerr, errEgressForeignLine) {
			status = http.StatusForbidden
		}
		writeJSON(w, status, map[string]any{"error": lerr.Error()})
		return
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
		res["mine"] = line.OwnerUserID == u.ID
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
	// 可用线路 = 平台 + 我的（v0.31.34 收紧）。他人的线路不参与均衡 ——
	// 规则是"用户的线路只承载自己的号"，均衡属于用户自助动作，必须遵守同一条规则。
	usable := make([]*EgressProxy, 0, len(proxies))
	for _, p := range proxies {
		if !p.Enabled {
			continue
		}
		if p.OwnerUserID > 0 && p.OwnerUserID != u.ID {
			continue
		}
		usable = append(usable, p)
	}
	groups, warns := groupEgressLinesByExit(usable, u.ID)
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
