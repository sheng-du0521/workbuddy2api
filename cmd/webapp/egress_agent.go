// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 本地出口代理的「一键拉起」（v0.31.17）+ 多机支持（v0.31.18）。
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
//
// # 通道设计：指令 / 状态，两端各做自己会做的事
//
//	[控制台]   点「一键拉起」→ 给**选中的那台机器**写一条 start 指令
//	[本机 agent] 轮询 poll（带自己的 machine id）→ 收到属于本机的 start → 起代理 + 反向隧道 → report
//	[云端]     隧道端口可连 + agent 在线 → **自动应用**（注册 / 设默认 / 绑自挂号）
//
// **为什么用轮询，而不是"服务端推"**：本机在家庭 NAT 后面，云端**无法主动连它**；
// 反向隧道只解决"云端能访问本机的服务"，**不解决**"云端能让本机执行命令"。
// 轮询是唯一既不需要在本机开入站端口、也不需要在本机跑 SSH 服务端的做法
// —— agent 全程**只出不进**。
//
// # 为什么必须按机器分（v0.31.18，用户现场驱动）
//
// v0.31.17 的指令 / 状态 / 隧道端口**都是全局单例**，这在一台机器上没问题，
// 用户说"我还有另一台电脑"时立刻三处同时撞车：
//
//	① 指令单例 → 在 A 机点了「拉起」，被 B 机的 agent 抢先取走并执行（"我点的没反应，它自己动了"）；
//	② 状态单例 → 两台机器每 5 秒互相覆盖上报，页面显示的 hostname / 出口 IP 随机跳；
//	③ 端口单例 → 两台都 `ssh -R 11080`，**后到的那台必然失败**（remote port forwarding failed），
//	   而失败信息只出现在该机的 agent 日志里，云端页面看不出"其实是端口被占了"。
//
// 现在：指令、状态、端口**全部按 machine id 分桶**；端口从 11080 起自动分配
// （第一台仍是 11080，与升级前完全一致）。**向后兼容**：老 agent 不带 machine 参数时
// 归入 "default" 桶，行为不变（它拿到的一直是 11080）。
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
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	// kvEgressAgentToken agent 的认证令牌（`X-Agent-Token`）。
	kvEgressAgentToken = "egress_agent_token"
	// kvEgressAgentMachines 所有已知机器的指令与状态（JSON：map[machineID]egressMachine）。
	//
	// 这是 v0.31.18 的**唯一事实来源**：指令与状态同存一处，
	// 免得"机器 A 的指令配机器 B 的状态"这种半截状态出现。
	kvEgressAgentMachines = "egress_agent_machines"
	// kvEgressLegacyState / kvEgressLegacyCmd v0.31.17 的单机 kv，**只读兼容**：
	// 升级时把里面那台机器认成 "default"，让正在跑的 agent 立刻出现在列表里
	// （否则升级瞬间页面会显示"一台机器都没有"，而它的隧道其实还好着）。
	kvEgressLegacyState = "egress_agent_state"
	kvEgressLegacyCmd   = "egress_agent_cmd"
	// kvEgressAgentPorts 端口**默认值**（用户机可覆盖；正常情况永远不用动）。
	kvEgressAgentPorts = "egress_agent_ports"

	// egressDefaultMachine 老 agent（不带 machine 参数）的归属桶。
	// 名字刻意叫 default 而不是 "legacy"：它就是"没报自己是谁的那台"。
	egressDefaultMachine = "default"
	// egressBaseTunnelPort / egressBaseProxyPort 端口分配起点。
	egressBaseTunnelPort = 11080
	egressBaseProxyPort  = 1080

	// egressAgentOnlineSec 多久没上报就算离线。
	//
	// agent 每 5 秒上报一次，这个值是它的 4 倍：容忍一次网络抖动 + 一次 gc 停顿，
	// 但**不允许**"进程没了却显示在线"超过 20 秒 —— 状态页显示"在线"却拉不起代理，
	// 比显示"离线"更坏。
	egressAgentOnlineSec = 20
	// egressAgentPollSec **仅用于文案**（告诉用户 agent 多久心跳一次）。
	egressAgentPollSec = 5
)

// egressAgentPorts 一台机器上的一对端口。
type egressAgentPorts struct {
	// ProxyPort 本机代理监听端口（agent 起代理用）。
	ProxyPort int `json:"proxy_port"`
	// TunnelPort 云端隧道端口（agent 建反向隧道用；云端据此访问本机代理）。
	TunnelPort int `json:"tunnel_port"`
}

func (p egressAgentPorts) withDefaults() egressAgentPorts {
	if p.ProxyPort == 0 {
		p.ProxyPort = egressBaseProxyPort
	}
	if p.TunnelPort == 0 {
		p.TunnelPort = egressBaseTunnelPort
	}
	return p
}

// egressAgentPortsOf 读端口**默认值**（kv 无值 = 内置默认）。只给 default 桶用。
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

// egressMachine 一台跑过 agent 的机器（指令 + 状态 + 端口）。
type egressMachine struct {
	ID       string           `json:"id"`
	Hostname string           `json:"hostname,omitempty"`
	Ports    egressAgentPorts `json:"ports"`
	// Cmd / CmdAt 待执行（或最近一条）指令。空串 = none。
	Cmd   string `json:"cmd,omitempty"`
	CmdAt int64  `json:"cmd_at,omitempty"`
	// State / StateAt 最近一次上报。
	State   egressAgentState `json:"state,omitempty"`
	StateAt int64            `json:"state_at,omitempty"`
	// CreatedAt 首次出现时刻（用于列表稳定排序 + "这台什么时候接入的"）。
	CreatedAt int64 `json:"created_at,omitempty"`
}

// online 该机器最近是否上报过。
func (m *egressMachine) online(now int64) bool {
	return m != nil && m.StateAt > 0 && now-m.StateAt < egressAgentOnlineSec
}

// machinesLoad 读取机器表（含 v0.31.17 单机 kv 的只读迁移）。
func (a *app) machinesLoad() map[string]*egressMachine {
	out := map[string]*egressMachine{}
	if v, err := a.store.GetKV(kvEgressAgentMachines); err == nil && strings.TrimSpace(v) != "" {
		var raw map[string]*egressMachine
		if json.Unmarshal([]byte(v), &raw) == nil && raw != nil {
			out = raw
		}
	}
	// 旧单机格式迁移（只在 default 桶缺失时做一次）。
	if _, ok := out[egressDefaultMachine]; !ok {
		if v, err := a.store.GetKV(kvEgressLegacyState); err == nil && strings.TrimSpace(v) != "" {
			var st egressAgentState
			if json.Unmarshal([]byte(v), &st) == nil && st.At > 0 {
				mm := &egressMachine{
					ID: egressDefaultMachine, Hostname: st.Hostname,
					Ports: a.egressAgentPortsOf(), State: st, StateAt: st.At, CreatedAt: st.At,
				}
				if cv, err := a.store.GetKV(kvEgressLegacyCmd); err == nil && strings.TrimSpace(cv) != "" {
					var cmd egressAgentCmd
					if json.Unmarshal([]byte(cv), &cmd) == nil {
						mm.Cmd, mm.CmdAt = cmd.Action, cmd.At
					}
				}
				out[egressDefaultMachine] = mm
			}
		}
	}
	return out
}

// machinesSave 落盘机器表。
func (a *app) machinesSave(m map[string]*egressMachine) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return a.store.SetKV(kvEgressAgentMachines, string(raw))
}

// machineEnsure 取一台机器；不存在则登记并**分配端口**。返回值 created 表示本次新建。
func (a *app) machineEnsure(m map[string]*egressMachine, id, hostname string) (mm *egressMachine, created bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		id = egressDefaultMachine
	}
	if id == egressDefaultMachine {
		// default 桶恒用全局默认端口（= 升级前行为，老 agent 不受影响）。
		hostname = "" // default 的 hostname 只由上报填充，避免被 query 里的空值覆盖
	}
	if exist, ok := m[id]; ok {
		if strings.TrimSpace(hostname) != "" {
			exist.Hostname = hostname
		}
		return exist, false
	}
	// 幽灵机器防护（v0.31.18）：老 agent（default 桶）升级到带 machine id 的新 agent 后，
	// hostname 不变但 id 有了。若不处理，列表里会多出一台"离线机器"占着 11080，
	// 而真正在跑的那台落在 11081 上 —— 用户看到的是“我明明只有一台电脑”。
	// 故：hostname 相同时，把 default 桶**改名**为新 id（端口、状态、指令全部继承，11080 不变）。
	if hn := strings.TrimSpace(hostname); hn != "" && id != egressDefaultMachine {
		if old, ok := m[egressDefaultMachine]; ok && old.Hostname != "" && strings.EqualFold(old.Hostname, hn) {
			delete(m, egressDefaultMachine)
			old.ID = id
			old.Hostname = hn
			m[id] = old
			log.Printf("INFO: [egress] default 桶已改名为 machine=%s（hostname %s 相同，端口保持 %d）",
				id, hn, old.Ports.TunnelPort)
			return old, true
		}
	}
	var ports egressAgentPorts
	if id == egressDefaultMachine {
		ports = a.egressAgentPortsOf()
	} else {
		ports = egressAllocPorts(m)
	}
	mm = &egressMachine{ID: id, Hostname: strings.TrimSpace(hostname), Ports: ports, CreatedAt: time.Now().Unix()}
	m[id] = mm
	return mm, true
}

// egressAllocPorts 给新机器分配一对端口：隧道端口从 11080 起找**第一个未被占用**的。
//
// 为什么必须避开已占用的：两台机器都 `ssh -R 11080` 时，后到的那台会失败
// （`remote port forwarding failed for listen port 11080`），而这条错误**只出现在
// 本机 agent 的日志里** —— 云端页面上只会看到"隧道未连通"，运维无从判断是
// 网络问题还是端口被占了。错开端口让两台机器可以**并存**。
func egressAllocPorts(m map[string]*egressMachine) egressAgentPorts {
	used := map[int]bool{}
	for _, v := range m {
		if v != nil && v.Ports.TunnelPort > 0 {
			used[v.Ports.TunnelPort] = true
		}
	}
	p := egressBaseTunnelPort
	for used[p] {
		p++
	}
	return egressAgentPorts{ProxyPort: egressBaseProxyPort, TunnelPort: p}
}

// ---------------------------------------------------------------------------
// 令牌
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// poll / report
// ---------------------------------------------------------------------------

// handleEgressAgentPoll GET /api/agent/poll?machine=<id>&hostname=<h>
//
// agent 取指令的入口。**每次都返回当前指令**（而不是"取走即清"）：
// agent 可能重启，清空式会丢掉"该拉起"的事实；幂等交给 agent 用 At 判断
// （同一条指令只执行一次）。
//
// machine 参数可省（老 agent 不带）→ 归入 "default" 桶，行为与 v0.31.17 一致。
func (a *app) handleEgressAgentPoll(w http.ResponseWriter, r *http.Request) {
	if !a.egressAgentAuth(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid agent token"})
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("machine"))
	host := strings.TrimSpace(r.URL.Query().Get("hostname"))
	m := a.machinesLoad()
	mm, created := a.machineEnsure(m, id, host)
	if created {
		_ = a.machinesSave(m)
		log.Printf("INFO: [egress] 新 agent 已登记 machine=%s host=%q tunnel_port=%d", mm.ID, mm.Hostname, mm.Ports.TunnelPort)
	}
	cmd := egressAgentCmd{Action: "none", ProxyPort: mm.Ports.ProxyPort, TunnelPort: mm.Ports.TunnelPort}
	if mm.Cmd != "" {
		cmd.Action, cmd.At = mm.Cmd, mm.CmdAt
	}
	writeJSON(w, http.StatusOK, cmd)
}

// handleEgressAgentReport POST /api/agent/report?machine=<id>
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
	m := a.machinesLoad()
	mm, _ := a.machineEnsure(m, r.URL.Query().Get("machine"), st.Hostname)
	mm.State, mm.StateAt = st, st.At
	if strings.TrimSpace(st.Hostname) != "" {
		mm.Hostname = strings.TrimSpace(st.Hostname)
	}
	if err := a.machinesSave(m); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "machine": mm.ID})
}

// egressTunnelReachable 云端自己确认一次"隧道真的通了"。
//
// **不信任 agent 的自我报告**：agent 说"隧道建好了"与"云端真能连上"是两件事
// （中间隔着一整条公网）。这里只做一次 TCP 连接 —— 便宜、且对"端口开着没人听"
// 这种最常见的失败形态足够敏感。
func egressTunnelReachable(port int) bool {
	if port <= 0 {
		return false
	}
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

// ---------------------------------------------------------------------------
// 视图
// ---------------------------------------------------------------------------

// egressMachineView 控制台「代理」页里的一台机器。
type egressMachineView struct {
	ID       string `json:"id"`
	Hostname string `json:"hostname,omitempty"`
	// Online agent 最近是否上报过。
	Online bool `json:"online"`
	// Tunnel 云端到该机代理端口的 TCP 是否可连（**不采信 agent 自报**）。
	Tunnel bool `json:"tunnel_reachable"`
	// Ready 一步到位的判据：agent 在线 + 隧道可连。
	Ready  bool   `json:"ready"`
	ExitIP string `json:"exit_ip,omitempty"`
	// ProxyOK / TunnelOK agent 自报的两个子进程状态（与上面的 Tunnel 分开报：
	// "agent 说建好了" 和 "云端真连得上" 是两件事，合并会把排障信息抹掉）。
	ProxyOK      bool   `json:"proxy_ok"`
	TunnelOK     bool   `json:"tunnel_ok"`
	AgentVersion string `json:"agent_version,omitempty"`
	Message      string `json:"message,omitempty"`
	ProxyPort    int    `json:"proxy_port"`
	TunnelPort   int    `json:"tunnel_port"`
	ProxyURL     string `json:"proxy_url"`
	// ProxyID 该机器的出口线路在线路池里的 id（未注册为空）。
	ProxyID string `json:"proxy_id,omitempty"`
	// Applied 该线路是否已设为全局默认（= 配置已应用）。
	Applied bool `json:"applied"`
	// HostBound / HostTotal 自挂号里已绑定本线路的数量。报"已绑/总数"而不是一个布尔：
	// **部分绑定**是最容易被忽略的中间状态（新挂了一个号、或换了线路）。
	HostBound int `json:"host_bound"`
	HostTotal int `json:"host_total"`
	// CurrentCmd 当前待执行指令（none / start / stop）。
	CurrentCmd string `json:"current_cmd,omitempty"`
	LastAt     int64  `json:"last_at"`
	// IsDefault 是否是不带 machine 参数的老 agent（default 桶）。
	IsDefault bool `json:"is_default"`
}

// egressHomeView 控制台「代理」页的状态载荷。
type egressHomeView struct {
	// Machines 所有已知机器（v0.31.18）。前端据此渲染"目标机器"选择器。
	Machines []egressMachineView `json:"machines"`
	// ---- 汇总字段：取**最近上报的那台**（兼容 v0.31.17 的字段名，便于前端渐进迁移）----
	AgentOnline   bool             `json:"agent_online"`
	AgentState    egressAgentState `json:"agent_state"`
	Ports         egressAgentPorts `json:"ports"`
	Tunnel        bool             `json:"tunnel_reachable"`
	Applied       bool             `json:"applied"`
	ProxyID       string           `json:"proxy_id,omitempty"`
	DefaultEgress string           `json:"default_egress"`
	HostBound     int              `json:"host_bound"`
	HostTotal     int              `json:"host_total"`
	AppliedAt     int64            `json:"applied_at"`
	Ready         bool             `json:"ready"`
	// ActiveMachine 上面那批汇总字段属于哪台机器（无则空）。
	ActiveMachine string `json:"active_machine,omitempty"`
	CanManage     bool   `json:"can_manage"`
	PollSec       int    `json:"poll_sec"`
	// AgentCommand 在本机启动 agent 的命令（一键复制用）。
	AgentCommand string `json:"agent_command,omitempty"`
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

// egressAdminOnly 管理员动作的入口校验（**二层防线**）。
//
// 路由本身已挂 adminGuard（管理员会话 或 X-Admin-Key 运维通道）。这里再判一次
// **会话级**管理员，是为了收紧"运维密钥"这条通道在这几个端点上的语义：
// 拉起/停止本机代理会**实质改变全平台出站链路**，希望执行者是一个具体的、
// 可追责的管理员会话而不是一把共享密钥。
func (a *app) egressAdminOnly(w http.ResponseWriter, r *http.Request) bool {
	if a.isAdminSession(r) || a.authorized(r) {
		return true // 管理员会话，或 X-Admin-Key 运维通道
	}
	writeJSON(w, http.StatusForbidden, map[string]any{
		"error": "该操作会改变全平台的出站链路（全局默认出口 + 自挂号绑定），仅管理员可执行",
	})
	return false
}

// defaultEgress 当前全局默认出口。
func (a *app) defaultEgress() string {
	if kv, err := a.store.GetKV(kvEgressDefault); err == nil {
		return strings.TrimSpace(kv)
	}
	return ""
}

// egressHostBinding 统计自挂号对若干线路 URL 的绑定数量。
//
// 一次遍历算出所有机器需要的计数（每台机器一条 URL），避免 N 台机器扫 N 遍账号表。
func (a *app) egressHostBinding(urls []string) (bound map[string]int, total int) {
	bound = map[string]int{}
	for _, u := range urls {
		bound[u] = 0
	}
	accs, err := a.store.ListAccounts()
	if err != nil {
		return bound, 0
	}
	for _, acc := range accs {
		if acc.OwnerUserID <= 0 {
			continue // 只统计自挂号（平台号跟随全局默认即可）
		}
		total++
		if u := strings.TrimSpace(acc.EgressProxy); u != "" {
			if _, ok := bound[u]; ok {
				bound[u]++
			}
		}
	}
	return bound, total
}

// buildEgressHomeView 组装状态（只读，不做任何写操作）。
func (a *app) buildEgressHomeView(r *http.Request, canManage bool) egressHomeView {
	now := time.Now().Unix()
	m := a.machinesLoad()
	def := a.defaultEgress()
	v := egressHomeView{PollSec: egressAgentPollSec, CanManage: canManage, DefaultEgress: def}

	// 线路池：按 URL 认线路（名字是给人看的、可改，URL 才是身份）。
	proxyIDByURL := map[string]string{}
	if proxies, err := a.store.ListEgressProxies(); err == nil {
		for _, p := range proxies {
			proxyIDByURL[strings.TrimSpace(p.URL)] = p.ID
		}
	}

	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	urls := make([]string, 0, len(ids))
	for _, id := range ids {
		urls = append(urls, egressHomeURL(m[id].Ports.TunnelPort))
	}
	bound, hostTotal := a.egressHostBinding(urls)

	for _, id := range ids {
		mm := m[id]
		url := egressHomeURL(mm.Ports.TunnelPort)
		mv := egressMachineView{
			ID: mm.ID, Hostname: mm.Hostname,
			Online:  mm.online(now),
			ProxyOK: mm.State.ProxyOK, TunnelOK: mm.State.TunnelOK,
			AgentVersion: mm.State.Version, Message: mm.State.Message,
			ExitIP:    mm.State.ExitIP,
			ProxyPort: mm.Ports.ProxyPort, TunnelPort: mm.Ports.TunnelPort,
			ProxyURL:  url,
			ProxyID:   proxyIDByURL[url],
			Applied:   def == url && url != "",
			HostBound: bound[url], HostTotal: hostTotal,
			CurrentCmd: mm.Cmd,
			LastAt:     mm.StateAt,
			IsDefault:  mm.ID == egressDefaultMachine,
		}
		mv.Tunnel = egressTunnelReachable(mm.Ports.TunnelPort)
		mv.Ready = mv.Online && mv.Tunnel
		v.Machines = append(v.Machines, mv)
	}
	// 稳定排序：default 在最前（它就是"当前这台"最常见的情形），其余按首次接入时间。
	sort.SliceStable(v.Machines, func(i, j int) bool {
		a, b := v.Machines[i], v.Machines[j]
		if a.IsDefault != b.IsDefault {
			return a.IsDefault
		}
		if a.LastAt != b.LastAt {
			return a.LastAt > b.LastAt
		}
		return a.ID < b.ID
	})

	// 汇总字段 = 最近上报的那台（有在线的优先）。
	var active *egressMachineView
	for i := range v.Machines {
		mv := &v.Machines[i]
		if active == nil {
			active = mv
			continue
		}
		if mv.LastAt > active.LastAt {
			active = mv
		}
	}
	if active != nil {
		v.ActiveMachine = active.ID
		v.AgentOnline = active.Online
		v.AgentState = egressAgentState{
			At: active.LastAt, Version: active.AgentVersion, Hostname: active.Hostname,
			ProxyOK: active.ProxyOK, TunnelOK: active.TunnelOK,
			ExitIP: active.ExitIP, Message: active.Message,
		}
		v.Ports = egressAgentPorts{ProxyPort: active.ProxyPort, TunnelPort: active.TunnelPort}
		v.Tunnel = active.Tunnel
		v.Applied = active.Applied
		v.ProxyID = active.ProxyID
		v.HostBound, v.HostTotal = active.HostBound, active.HostTotal
		v.Ready = active.Ready
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

// handleEgressHome GET /api/egress/home（登录可见，只读）
func (a *app) handleEgressHome(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	if u == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "未登录"})
		return
	}
	writeJSON(w, http.StatusOK, a.buildEgressHomeView(r, u.IsAdmin()))
}

// egressPickMachine 从请求体里取目标机器；缺省取"最近上报的那台"。
//
// 为什么要能指定：不指定就只能"谁最近说过话就命令谁"，两台机器并存时
// 用户在 A 机页面上点一下，可能恰好发给了 B 机 —— 而用户**看不出**这件事。
func (a *app) egressPickMachine(r *http.Request) string {
	var body struct {
		Machine string `json:"machine"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if id := strings.TrimSpace(body.Machine); id != "" {
		return id
	}
	m := a.machinesLoad()
	best, bestAt := "", int64(-1)
	for id, mm := range m {
		if mm.StateAt > bestAt {
			best, bestAt = id, mm.StateAt
		}
	}
	return best
}

// handleEgressHomePull POST /api/egress/home/pull body {"machine":"<id>"}（仅管理员）
//
// 下发 start 指令。**不等结果就返回**：真正的进度由前端轮询 GET 得到
// —— 一个 HTTP 请求里等 30 秒会同时考验反向代理超时、浏览器超时和用户耐心，
// 而这件事实在没什么需要"等"的（agent 是独立进程，会自己完成）。
func (a *app) handleEgressHomePull(w http.ResponseWriter, r *http.Request) {
	if !a.egressAdminOnly(w, r) {
		return
	}
	id := a.egressPickMachine(r)
	m := a.machinesLoad()
	mm, _ := a.machineEnsure(m, id, "")
	mm.Cmd, mm.CmdAt = "start", time.Now().Unix()
	if err := a.machinesSave(m); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	log.Printf("INFO: [egress] 已向 machine=%s 下发 start 指令（等待 agent 拉起本机出口代理）", mm.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"issued": true, "action": "start", "machine": mm.ID,
		"view": a.buildEgressHomeView(r, true),
	})
}

// handleEgressHomeRelease POST /api/egress/home/release body {"machine":"<id>"}（仅管理员）
//
// 下发 stop。**刻意不自动清掉已应用的出口配置**：停代理只是"不再从家宽出"，
// 而"账号该走哪条线"是另一个决定（可能已经有人手工调过）。要改回去，
// 用管理后台的「代理」页显式操作 —— 一键停代理顺手改几十个账号的出口，
// 是那种"点一下才发现改错了"的操作。
func (a *app) handleEgressHomeRelease(w http.ResponseWriter, r *http.Request) {
	if !a.egressAdminOnly(w, r) {
		return
	}
	id := a.egressPickMachine(r)
	m := a.machinesLoad()
	mm, _ := a.machineEnsure(m, id, "")
	mm.Cmd, mm.CmdAt = "stop", time.Now().Unix()
	if err := a.machinesSave(m); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"issued": true, "action": "stop", "machine": mm.ID})
}

// handleEgressHomeApply POST /api/egress/home/apply body {"machine":"<id>"}（仅管理员）
//
// 显式应用。前端在"就绪"后调用它（而不是让 GET 顺手写）——
// 读接口带副作用是排障时最讨厌的东西之一。
func (a *app) handleEgressHomeApply(w http.ResponseWriter, r *http.Request) {
	if !a.egressAdminOnly(w, r) {
		return
	}
	id := a.egressPickMachine(r)
	m := a.machinesLoad()
	mm, ok := m[id]
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "该机器尚未登记（本机 agent 还没连上来过）",
		})
		return
	}
	if !egressTunnelReachable(mm.Ports.TunnelPort) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": fmt.Sprintf("隧道尚未就绪（云端连不上本机代理端口 %d），请先完成「一键拉起」", mm.Ports.TunnelPort),
		})
		return
	}
	res, err := a.applyHomeEgress(mm.Ports.TunnelPort)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"applied": res, "machine": mm.ID, "view": a.buildEgressHomeView(r, true),
	})
}

// egressHomeApplyResult 应用结果（逐项如实回报）。
type egressHomeApplyResult struct {
	ProxyID   string `json:"proxy_id"`
	ProxyURL  string `json:"proxy_url"`
	DefaultOK bool   `json:"default_ok"`
	HostBound int    `json:"host_bound"`
	HostTotal int    `json:"host_total"`
	Probed    bool   `json:"probed"`
	ExitIP    string `json:"exit_ip,omitempty"`
}

// applyHomeEgress 把某台机器的本地出口线路**应用到位**（幂等，可反复调用）。
//
// 三件事，缺一不可：
//  1. **注册**线路（按 URL upsert，重复调用不产生第二条）；
//  2. **设为全局默认出口** —— 平台号（owner_user_id == 0）与所有"跟随默认"的
//     账号因此自动走本地出口，不必逐个绑定；
//  3. **显式绑定自挂号** —— 这一条是刻意的：自挂号的出口不该随"全局默认"
//     漂移（站长换一次默认出口就把别人的号也换了链路）。
//
// ⚠️ 多机注意：**第 2 步是全局唯一的**，所以"最后应用的那台机器"决定默认出口。
// 这是有意为之 —— 保留"一个全局默认"这个简单模型，而不是让每台机器各自为政
// （那会让"我的号到底从哪出去"变成一个需要逐台推演的问题）。
// 要让某些号走另一台机器的线路，用管理后台的「批量分配」显式绑定。
func (a *app) applyHomeEgress(tunnelPort int) (egressHomeApplyResult, error) {
	url := egressHomeURL(tunnelPort)
	var res egressHomeApplyResult
	res.ProxyURL = url

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
	log.Printf("INFO: [egress] 本地出口已应用 proxy=%s url=%s host_bound=%d/%d",
		hit.ID, url, res.HostBound, res.HostTotal)
	return res, nil
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
