// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 本地出口代理的「一键拉起」（v0.31.17）。
//
// # 要解决的问题
//
// 住宅出口（家里的宽带）比机房 IP 对上游友好得多；但它的建立过程分两半，
// 而两半在不同的机器上：
//
//	本机（用户的电脑）：跑本地代理进程 + 一条到云端的反向隧道；
//	云端（本服务）：把这条线路**接进出口池**（注册线路 / 设为默认 / 绑到自挂号）。
//
// 只做前半 = "隧道通了但没人用"；只做后半 = "配了一条不通的线路"。
// 此前这两半全靠手工（本机双击 bat、云端在管理后台逐项填），正是这里要消灭的。
//
// # 通道设计：指令 / 状态，两端各做自己会做的事
//
//	[控制台]   点「一键拉起」→ 写一条 start 指令（kv）
//	[本机 agent] 轮询 poll → 收到 start → 起代理 + 反向隧道 → report 状态
//	[云端]     隧道端口可连 + agent 在线 → **自动应用**（注册 / 设默认 / 绑自挂号）
//
// **为什么用轮询，而不是"服务端推"**：本机在家庭 NAT 后面，云端**无法主动连它**；
// 反向隧道只解决"云端能访问本机的服务"，**不解决**"云端能让本机执行命令"。
// 轮询是唯一既不需要在本机开入站端口、也不需要在本机跑 SSH 服务端的做法
// —— agent 全程**只出不进**。
//
// ⚠️ **权限边界（刻意的）**：状态查询对登录用户开放；但「拉起 / 停止 /
// 应用出口」会**改变全平台的出站链路**（全局默认出口 + 自挂号绑定），
// 因此**只有管理员**能执行。普通用户看到的是只读状态 + 一句说明。
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"log"
)

const (
	// kvEgressAgentToken agent 的认证令牌（`X-Agent-Token`）。
	kvEgressAgentToken = "egress_agent_token"
	// kvEgressAgentCmd 下发给 agent 的指令（JSON，见 egressAgentCmd）。
	kvEgressAgentCmd = "egress_agent_cmd"
	// kvEgressAgentState agent 最近一次上报的状态（JSON）。
	kvEgressAgentState = "egress_agent_state"
	// kvEgressAgentPorts 端口配置（本机代理端口 / 隧道端口）。
	kvEgressAgentPorts = "egress_agent_ports"

	// egressAgentOnlineSec 多久没上报就算离线。
	//
	// agent 每 egressAgentPollSec 秒上报一次，这个值是它的 4 倍：容忍一次
	// 网络抖动 + 一次 gc 停顿，但**不允许**"进程没了却显示在线"超过 20 秒
	// —— 状态页显示"在线"却拉不起代理，比显示"离线"更坏。
	egressAgentOnlineSec = 20
	// egressAgentPollSec **仅用于文案**（告诉用户 agent 多久心跳一次）。
	// 真实节奏由 agent 决定：服务端不假设它，只按上报时间判定在线。
	egressAgentPollSec = 5
)

// egressAgentPorts 端口配置。给默认值，正常情况下用户永远不需要看见它们
// （"无需手动填写"是需求原文）。
type egressAgentPorts struct {
	// ProxyPort 本机代理监听端口（agent 起代理用）。
	ProxyPort int `json:"proxy_port"`
	// TunnelPort 云端隧道端口（agent 建反向隧道用；云端据此访问本机代理）。
	TunnelPort int `json:"tunnel_port"`
}

func (p egressAgentPorts) withDefaults() egressAgentPorts {
	if p.ProxyPort == 0 {
		p.ProxyPort = 1080
	}
	if p.TunnelPort == 0 {
		p.TunnelPort = 11080
	}
	return p
}

// egressAgentPortsOf 读端口配置（kv 无值 = 用默认）。
func (a *app) egressAgentPortsOf() egressAgentPorts {
	var p egressAgentPorts
	if v, err := a.store.GetKV(kvEgressAgentPorts); err == nil && strings.TrimSpace(v) != "" {
		_ = json.Unmarshal([]byte(v), &p)
	}
	return p.withDefaults()
}

// egressAgentCmd 下发给 agent 的指令。
type egressAgentCmd struct {
	// Action start / stop / none。
	Action string `json:"action"`
	// At 指令写入时刻（unix 秒）。agent 用它做幂等：同一条指令只执行一次。
	At int64 `json:"at"`
	// ProxyPort / TunnelPort 端口（agent 不必记住，跟着指令走）。
	ProxyPort  int `json:"proxy_port"`
	TunnelPort int `json:"tunnel_port"`
}

// egressAgentState agent 上报的状态。
type egressAgentState struct {
	At       int64  `json:"at"`
	Version  string `json:"version,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	// ProxyOK 本机代理进程是否在跑。
	ProxyOK bool `json:"proxy_ok"`
	// TunnelOK 反向隧道是否已建立（agent 自己的判断）。
	TunnelOK bool `json:"tunnel_ok"`
	// ExitIP agent **通过该代理**查到的出口 IP（这是最有说服力的一行：
	// 它证明"流量确实从家里出去了"，而不是"进程起来了"）。
	ExitIP string `json:"exit_ip,omitempty"`
	// Message 最近一条人话状态（错误原文也放这里）。
	Message string `json:"message,omitempty"`
}

// egressAgentTokenGet 取（必要时生成）agent 令牌。
//
// 令牌是**随机 32 字节**、只存在服务端 kv 里；它能做的只有 poll / report
// 两件事（见路由），拿不到任何账号数据 —— 所以即便泄露，影响面是
// "有人能替我拉起本机代理"，而不是"有人能读我的号"。
func (a *app) egressAgentTokenGet() (string, error) {
	if v, err := a.store.GetKV(kvEgressAgentToken); err == nil {
		if t := strings.TrimSpace(v); t != "" {
			return t, nil
		}
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	t := "ag-" + hex.EncodeToString(buf)
	if err := a.store.SetKV(kvEgressAgentToken, t); err != nil {
		return "", err
	}
	return t, nil
}

// egressAgentAuth agent 请求的令牌校验。
func (a *app) egressAgentAuth(r *http.Request) bool {
	want, err := a.egressAgentTokenGet()
	if err != nil || want == "" {
		return false
	}
	got := strings.TrimSpace(r.Header.Get("X-Agent-Token"))
	// 定长比较（长度相同才比内容），避免把令牌长度也变成可观测信号。
	if len(got) != len(want) {
		return false
	}
	var diff byte
	for i := 0; i < len(got); i++ {
		diff |= got[i] ^ want[i]
	}
	return diff == 0
}

// handleEgressAgentPoll GET /api/agent/poll
//
// agent 取指令的入口。**每次都返回当前指令**（而不是"取走即清"）：
// agent 可能重启，清空式会丢掉"该拉起"的事实；幂等交给 agent 用 At 判断
// （同一条指令只执行一次）。
func (a *app) handleEgressAgentPoll(w http.ResponseWriter, r *http.Request) {
	if !a.egressAgentAuth(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid agent token"})
		return
	}
	ports := a.egressAgentPortsOf()
	cmd := egressAgentCmd{Action: "none", ProxyPort: ports.ProxyPort, TunnelPort: ports.TunnelPort}
	if v, err := a.store.GetKV(kvEgressAgentCmd); err == nil && strings.TrimSpace(v) != "" {
		var stored egressAgentCmd
		if json.Unmarshal([]byte(v), &stored) == nil && stored.Action != "" {
			cmd = stored
			// 端口以**服务端当前配置**为准（指令里的可能是旧的）。
			cmd.ProxyPort, cmd.TunnelPort = ports.ProxyPort, ports.TunnelPort
		}
	}
	writeJSON(w, http.StatusOK, cmd)
}

// handleEgressAgentReport POST /api/agent/report
func (a *app) handleEgressAgentReport(w http.ResponseWriter, r *http.Request) {
	if !a.egressAgentAuth(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid agent token"})
		return
	}
	var st egressAgentState
	if err := json.NewDecoder(r.Body).Decode(&st); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	st.At = time.Now().Unix()
	raw, _ := json.Marshal(st)
	if err := a.store.SetKV(kvEgressAgentState, string(raw)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// egressAgentStateOf 读最近一次上报（含在线判定）。
func (a *app) egressAgentStateOf() (egressAgentState, bool) {
	var st egressAgentState
	v, err := a.store.GetKV(kvEgressAgentState)
	if err != nil || strings.TrimSpace(v) == "" {
		return st, false
	}
	if json.Unmarshal([]byte(v), &st) != nil {
		return st, false
	}
	return st, time.Now().Unix()-st.At < egressAgentOnlineSec
}

// egressTunnelReachable 云端自己确认一次"隧道真的通了"。
//
// **不信任 agent 的自我报告**：agent 说"隧道建好了"与"云端真能连上"是两件事
// （中间隔着一整条公网）。这里只做一次 TCP 连接 —— 便宜、且对"端口开着没人听"
// 这种最常见的失败形态足够敏感。
func egressTunnelReachable(port int) bool {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 3*time.Second)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// egressHomeURL 本地出口线路的 URL（云端视角）。
func egressHomeURL(port int) string {
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

// egressHomeView 控制台「代理」页的状态载荷。
type egressHomeView struct {
	// AgentOnline agent 最近是否上报过。
	AgentOnline bool             `json:"agent_online"`
	AgentState  egressAgentState `json:"agent_state"`
	Ports       egressAgentPorts `json:"ports"`
	Tunnel      bool             `json:"tunnel_reachable"`
	// Applied 该出口线路是否已注册并已应用（设为默认 + 绑自挂号）。
	Applied bool `json:"applied"`
	// ProxyID 线路池里的线路 id（未注册为空）。
	ProxyID string `json:"proxy_id,omitempty"`
	// DefaultEgress 当前全局默认出口。
	DefaultEgress string `json:"default_egress"`
	// HostBound / HostTotal 自挂号里已绑定本线路的数量。
	//
	// 报"已绑/总数"而不是一个布尔：**部分绑定**是最容易被忽略的中间状态
	// （比如新挂了一个号、或换了线路），只报布尔会把它显示成"没配"或"配好了"。
	HostBound int `json:"host_bound"`
	HostTotal int `json:"host_total"`
	// AppliedAt 最近一次应用的时刻（unix 秒，0 = 从未）。
	AppliedAt int64 `json:"applied_at"`
	// Ready 一步到位的判据：agent 在线 + 隧道可连。
	Ready bool `json:"ready"`
	// CanManage 当前登录者是否有权执行（仅管理员）。
	CanManage bool `json:"can_manage"`
	// PollSec 心跳间隔（文案用）。
	PollSec int `json:"poll_sec"`
	// AgentCommand 在本机启动 agent 的命令（一键复制用）。
	AgentCommand string `json:"agent_command,omitempty"`
}

// egressAdminOnly 管理员动作的入口校验（**二层防线**）。
//
// 路由本身已挂 adminGuard（管理员会话 或 X-Admin-Key 运维通道）。这里再判一次
// **会话级**管理员，是为了收紧"运维密钥"这条通道在这几个端点上的语义：
// 拉起/停止本机代理会**实质改变全平台出站链路**，希望执行者是一个具体的、
// 可追责的管理员会话而不是一把共享密钥。
//
// 为什么这些动作必须是管理员：它们改的是**全平台的出站链路**（全局默认出口 +
// 自挂号绑定），一个普通用户点一下就会把所有人的号换到另一条网络上。
// 状态查询则相反 —— 只读、无副作用，对登录用户开放，让他知道"现在出口在哪"。
func (a *app) egressAdminOnly(w http.ResponseWriter, r *http.Request) bool {
	if a.isAdminSession(r) || a.authorized(r) {
		return true // 管理员会话，或 X-Admin-Key 运维通道
	}
	writeJSON(w, http.StatusForbidden, map[string]any{
		"error": "该操作会改变全平台的出站链路（全局默认出口 + 自挂号绑定），仅管理员可执行",
	})
	return false
}

// buildEgressHomeView 组装状态（只读，不做任何写操作）。
func (a *app) buildEgressHomeView(r *http.Request, canManage bool) egressHomeView {
	ports := a.egressAgentPortsOf()
	st, online := a.egressAgentStateOf()
	v := egressHomeView{
		AgentOnline: online,
		AgentState:  st,
		Ports:       ports,
		Tunnel:      egressTunnelReachable(ports.TunnelPort),
		PollSec:     egressAgentPollSec,
		CanManage:   canManage,
	}
	if kv, err := a.store.GetKV(kvEgressDefault); err == nil {
		v.DefaultEgress = strings.TrimSpace(kv)
	}
	v.Ready = online && v.Tunnel

	// 线路池里有没有这条（按 URL 认，不按名字 —— 名字是给人看的、可改）。
	want := egressHomeURL(ports.TunnelPort)
	if proxies, err := a.store.ListEgressProxies(); err == nil {
		for _, p := range proxies {
			if strings.TrimSpace(p.URL) == want {
				v.ProxyID = p.ID
				break
			}
		}
	}
	v.Applied = v.ProxyID != "" && v.DefaultEgress == want

	// 自挂号的绑定情况。
	if accs, err := a.store.ListAccounts(); err == nil {
		for _, acc := range accs {
			if acc.OwnerUserID <= 0 {
				continue // 只统计自挂号（平台号跟随全局默认即可）
			}
			v.HostTotal++
			if strings.TrimSpace(acc.EgressProxy) == want {
				v.HostBound++
			}
		}
	}
	if kv, err := a.store.GetKV(kvEgressAppliedAt); err == nil {
		if n, err2 := parseI64(kv); err2 == nil {
			v.AppliedAt = n
		}
	}
	// 命令只给有权限的人看（它是能拉起整机代理的凭证）。
	if canManage {
		if tok, err := a.egressAgentTokenGet(); err == nil {
			v.AgentCommand = fmt.Sprintf(
				"wb2api-egress-agent.exe -server %s -token %s", requestOrigin(r), tok)
		}
	}
	return v
}

// kvEgressAppliedAt 最近一次"应用"的时刻（仅用于展示）。
const kvEgressAppliedAt = "egress_applied_at"

// parseI64 宽松解析整数（kv 里可能是空串 / 带空白）。
func parseI64(s string) (int64, error) {
	var n int64
	_, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &n)
	return n, err
}

// requestOrigin 由请求推导出对外基址（给 agent 命令用）。
//
// 优先用请求自带的 Host：**它一定是客户端真的能访问到的地址**（而我们无法
// 从服务端可靠推断 —— 可能挂在隧道后面、可能在反代后面）。X-Forwarded-Proto
// 只影响 scheme 选择。
func requestOrigin(r *http.Request) string {
	if r == nil {
		return ""
	}
	host := strings.TrimSpace(r.Host)
	if host == "" {
		return ""
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https") {
		scheme = "https"
	}
	return scheme + "://" + host
}

// egressHomeApplyResult 应用结果（逐项如实回报）。
type egressHomeApplyResult struct {
	ProxyID   string `json:"proxy_id"`
	DefaultOK bool   `json:"default_ok"`
	HostBound int    `json:"host_bound"`
	HostTotal int    `json:"host_total"`
	Probed    bool   `json:"probed"`
	ExitIP    string `json:"exit_ip,omitempty"`
}

// applyHomeEgress 把本地出口线路**应用到位**（幂等，可反复调用）。
//
// 三件事，缺一不可：
//  1. **注册**线路（按 URL upsert，重复调用不产生第二条）；
//  2. **设为全局默认出口** —— 平台号（owner_user_id == 0）与所有"跟随默认"的
//     账号因此自动走本地出口，不必逐个绑定；
//  3. **显式绑定自挂号** —— 这一条是刻意的：自挂号的出口不该随"全局默认"
//     漂移（站长换一次默认出口就把别人的号也换了链路）。显式绑定让"我的号
//     走我家宽带"这件事**写在账号上**，看得见、也改得动。
//
// 不做的事：不动平台号已有的显式绑定（`egress_proxy` 非空的），也不动冷却/
// 熔断/分组 —— 本函数**只碰出口相关的两个字段**。
func (a *app) applyHomeEgress() (egressHomeApplyResult, error) {
	ports := a.egressAgentPortsOf()
	url := egressHomeURL(ports.TunnelPort)
	var res egressHomeApplyResult

	// 1) 注册（或复用）线路。
	existing, err := a.store.ListEgressProxies()
	if err != nil {
		return res, err
	}
	var hit *EgressProxy
	for _, p := range existing {
		if strings.TrimSpace(p.URL) == url {
			hit = p
			break
		}
	}
	now := time.Now().Unix()
	if hit == nil {
		hit = &EgressProxy{
			ID:        newEgressProxyID(url),
			Name:      "本地住宅出口",
			URL:       url,
			Region:    "本机所在网络",
			Note:      "由「一键拉起」自动注册（本机代理 + 反向隧道）",
			Enabled:   true,
			CreatedAt: now,
		}
	}
	hit.UpdatedAt = now
	hit.Enabled = true // 拉起即启用（否则"通了但被停用"仍是个坑）
	if err := a.store.UpsertEgressProxy(hit); err != nil {
		return res, err
	}
	res.ProxyID = hit.ID

	// 2) 全局默认出口。
	if err := a.store.SetKV(kvEgressDefault, url); err != nil {
		return res, err
	}
	res.DefaultOK = true

	// 3) 显式绑定自挂号（只改 egress_proxy 一个字段）。
	accs, err := a.store.ListAccounts()
	if err != nil {
		return res, err
	}
	for _, acc := range accs {
		if acc.OwnerUserID <= 0 {
			continue
		}
		res.HostTotal++
		if strings.TrimSpace(acc.EgressProxy) == url {
			res.HostBound++
			continue
		}
		if err := a.store.SetAccountEgress(acc.UID, url); err != nil {
			// 单个号失败不回滚整体：出口是一个"尽量覆盖"的配置，
			// 回滚会让已经生效的部分也退回去（更难诊断）。如实计数即可。
			log.Printf("WARN: [egress] 绑定自挂号出口失败 acct=%s: %v", acc.UID, err)
			continue
		}
		res.HostBound++
	}
	_ = a.store.SetKV(kvEgressAppliedAt, fmt.Sprintf("%d", now))
	// 池以库为准重建（否则选号仍按旧出口）。
	_ = a.proxy.SyncAccounts()
	log.Printf("INFO: [egress] 本地出口已应用 proxy=%s default=%s host_bound=%d/%d",
		hit.ID, url, res.HostBound, res.HostTotal)
	return res, nil
}

// handleEgressHome GET /api/egress/home（登录可见，只读）
func (a *app) handleEgressHome(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	if u == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "未登录"})
		return
	}
	writeJSON(w, http.StatusOK, a.buildEgressHomeView(r, u.IsAdmin()))
}

// handleEgressHomePull POST /api/egress/home/pull（仅管理员）
//
// 下发 start 指令。**不等结果就返回**：真正的进度由前端轮询 GET 得到
// —— 一个 HTTP 请求里等 30 秒会同时考验反向代理超时、浏览器超时和用户耐心，
// 而这件事实在没什么需要"等"的（agent 是独立进程，会自己完成）。
func (a *app) handleEgressHomePull(w http.ResponseWriter, r *http.Request) {
	if !a.egressAdminOnly(w, r) {
		return
	}
	ports := a.egressAgentPortsOf()
	cmd := egressAgentCmd{
		Action: "start", At: time.Now().Unix(),
		ProxyPort: ports.ProxyPort, TunnelPort: ports.TunnelPort,
	}
	raw, _ := json.Marshal(cmd)
	if err := a.store.SetKV(kvEgressAgentCmd, string(raw)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	log.Printf("INFO: [egress] 已下发 start 指令（等待 agent 拉起本机出口代理）")
	writeJSON(w, http.StatusOK, map[string]any{
		"issued": true, "action": "start",
		"view": a.buildEgressHomeView(r, true),
	})
}

// handleEgressHomeRelease POST /api/egress/home/release（仅管理员）
//
// 下发 stop。**刻意不自动清掉已应用的出口配置**：停代理只是"不再从家宽出"，
// 而"账号该走哪条线"是另一个决定（可能已经有人手工调过）。要改回去，
// 用管理后台的「代理」页显式操作 —— 一键停代理顺手改几十个账号的出口，
// 是那种"点一下才发现改错了"的操作。
func (a *app) handleEgressHomeRelease(w http.ResponseWriter, r *http.Request) {
	if !a.egressAdminOnly(w, r) {
		return
	}
	cmd := egressAgentCmd{Action: "stop", At: time.Now().Unix()}
	raw, _ := json.Marshal(cmd)
	if err := a.store.SetKV(kvEgressAgentCmd, string(raw)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"issued": true, "action": "stop"})
}

// handleEgressHomeApply POST /api/egress/home/apply（仅管理员）
//
// 显式应用。前端在"就绪"后调用它（而不是让 GET 顺手写）——
// 读接口带副作用是排障时最讨厌的东西之一。
func (a *app) handleEgressHomeApply(w http.ResponseWriter, r *http.Request) {
	if !a.egressAdminOnly(w, r) {
		return
	}
	ports := a.egressAgentPortsOf()
	if !egressTunnelReachable(ports.TunnelPort) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "隧道尚未就绪（云端连不上本机代理端口），请先完成「一键拉起」",
		})
		return
	}
	res, err := a.applyHomeEgress()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"applied": res, "view": a.buildEgressHomeView(r, true),
	})
}

// newEgressProxyID 线路 id（与既有线路池同风格：稳定短 id，按 url 派生）。
//
// 按 URL 派生而不是随机：同一条线路被重复注册时**得到同一个 id**，
// 于是"重新拉起"不会在池里堆出一串同 URL 的历史线路。
func newEgressProxyID(url string) string {
	sum := 0
	for i := 0; i < len(url); i++ {
		sum = sum*31 + int(url[i])
	}
	return fmt.Sprintf("home%08x", uint32(sum))
}
