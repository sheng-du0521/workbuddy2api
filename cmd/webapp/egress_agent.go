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
// ⚠️ **权限边界（v0.31.24 起按「谁的机器 / 谁的号」切）**：
//
//	平台视角（管理员会话 / X-Admin-Key）→ 看全部机器；应用出口时注册线路 +
//	**设全局默认出口** + 绑**全部**自挂号；
//	用户视角（普通登录用户）→ 只看得到**自己名下的机器**；应用出口时注册线路
//	（带上归属）+ **只绑自己名下的号**，**绝不碰全局默认出口**。
//
// 为什么用户侧必须能操作：出口是"我的号从哪个 IP 出去"，这件事本来就该由号主
// 自己决定；此前只能由管理员代劳，等于把每个用户的出站链路都挂在一个人的手工操作上。
//
// 为什么用户侧**不能**设全局默认出口：那是平台级决定 —— 它同时决定所有平台号与
// 所有"跟随默认"的账号从哪出去。用户提供的代理是不可信第三方（我们无法担保其
// 稳定性，且理论上能看到经过它的流量特征），只应承载它主人的号。
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/egress"
)

const (
	// kvEgressAgentToken agent 的认证令牌（`X-Agent-Token`）。
	kvEgressAgentToken = "egress_agent_token"
	// kvEgressAgentTokenUserFmt 按用户签发的 agent 令牌（v0.31.24）。
	//
	// `%d` 是该用户的 id。用户令牌与平台令牌**互不通用**：用户令牌解析出的 uid
	// 决定"这台机器归谁"，也正是视图过滤与 apply 范围的依据。
	kvEgressAgentTokenUserFmt = "egress_agent_token_u%d"

	// kvEgressAgentMachines 所有已知机器的指令与状态（JSON：map[machineKey]egressMachine）。
	//
	// 这是 v0.31.18 的**唯一事实来源**：指令与状态同存一处，
	// 免得"机器 A 的指令配机器 B 的状态"这种半截状态出现。
	kvEgressAgentMachines = "egress_agent_machines"
	// kvEgressLegacyState / kvEgressLegacyCmd v0.31.17 的单机 kv，**只读兼容**：
	// 升级时把里面那台机器认成 "default"，让正在跑的 agent 立刻出现在列表里
	// （否则升级瞬间页面会显示"一台机器都没有"，而它的隧道其实还好着）。
	kvEgressLegacyState = "egress_agent_state"
	kvEgressLegacyCmd   = "egress_agent_cmd"
	// kvEgressLegacyMigrated 旧单机 kv **已迁移**的标记（v0.31.49）。
	//
	// 有它才能保证"旧 kv 只读一次"：否则 default 桶被清掉后，下一次 load 又把它
	// 从旧 kv 重建出来，而 prune 又会按"无指纹 + 很老"清掉 —— 删建循环每 5 秒
	// 刷两行日志，把真正要查的扫码/代理日志整段淹掉（现场 16 小时上万条）。
	kvEgressLegacyMigrated = "egress_legacy_migrated"
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
	//
	// ⚠️ 历史上这里存的是 `"39.91.80.16（Shandong Jinan …）"` **复合串**（v0.3.0 及以前）。
	// 比较出口是否相同必须走 `normalizeExitIP`（见 egress_exit.go）—— 直接比字符串
	// 会把同一个 IP 的两种形状判成两个出口，同 IP 归并就完全失效。
	ExitIP string `json:"exit_ip,omitempty"`
	// ExitIPNote 出口 IP 的归属地/运营商（v0.31.26 起 agent 分开上报）。
	ExitIPNote string `json:"exit_ip_note,omitempty"`
	// Fingerprint 机器指纹（v0.31.26）：hostname + 主网卡 MAC + 首选内网 IP 的哈希。
	//
	// 它回答的是这台机器**能不能被 agent 自报的 id 骗过**的问题：machine id 是
	// agent 自己生成、随参数上行的（用户把整个目录拷到另一台电脑就会重复），
	// 而指纹取的是这台机器的物理特征 —— 两台机器同一个指纹，基本只能是同一台机器。
	Fingerprint string `json:"fingerprint,omitempty"`
	// FingerprintPrev 指纹算法**上一版**的取值（v0.31.49，升级过渡期用）。
	//
	// 旧算法把内网 IP 算进哈希 → 同一台笔记本换个网络就换了身份。新算法只用
	// hostname + 主网卡 MAC。升级时 agent 同时报两个值，服务端才能把老记录
	// 认回来（否则每台机器升级后都会多出一条幽灵线路 + 一个新端口）。
	FingerprintPrev string `json:"fingerprint_prev,omitempty"`
	// LocalIP 本机首选内网地址（人眼核对"这是哪台机器"用，不参与判定）。
	LocalIP string `json:"local_ip,omitempty"`
	// Message 最近一条人话状态（错误原文也放这里）。
	Message string `json:"message,omitempty"`
	// AuthCandidates 这台机器上**探测到的**客户端登录态文件（v0.31.42）。
	//
	// 为什么需要它：号主本机客户端与我们共用同一条 refresh 轮换链，谁先刷谁把对方
	// 打掉。要彻底消除这个冲突只有一条路 —— **让本机成为唯一刷新者，服务器只读**。
	// 而服务器看不到对方机器上的文件，只能由 agent 去扫、把"有哪些候选"报上来。
	//
	// 边界（刻意）：这里**只有元信息**（路径 / 修改时间 / 大小 / 账号 uid 前几位），
	// **不含任何令牌**。要不要跟随某个文件，是运维看过列表之后显式选的；
	// 上报令牌是"跟随"动作的结果，不是扫描的副作用。
	AuthCandidates []agentAuthCandidate `json:"auth_candidates,omitempty"`
}

// agentAuthCandidate agent 在本机探测到的一个候选登录态文件。
//
// 字段全部是"能不泄露凭证地回答这是哪个号"的最小集合：路径用于人在自己电脑上找到它，
// UIDPrefix 用于和云端的账号对上号，其余用于判断"这个文件多久没动过"。
type agentAuthCandidate struct {
	Path string `json:"path"`
	// MTime 文件修改时间（unix 秒）。它几乎等于"这个客户端的令牌最后一次换新是在什么时候"。
	MTime int64 `json:"mtime"`
	Size  int64 `json:"size"`
	// UIDPrefix 账号 uid 的前 8 位（没有则空）。**只放前缀**：完整 uid 是账号标识，
	// 列表要发给浏览器；前缀足够人工对上号，也足够在库里做匹配提示。
	UIDPrefix string `json:"uid_prefix,omitempty"`
	Nickname  string `json:"nickname,omitempty"`
	// Kind 这份文件是什么（v0.3.4）：exported（我们自己导出的账号备份）/ client（客户端登录态）。
	//
	// 分开放是因为**用途相反**：exported 可以拿来重新导入；client 是我们正在
	// 想办法不要跟它抢轮换链的那一份，绝不能当备份用。
	Kind string `json:"kind,omitempty"`
	// Note 探测结论：ok / 无令牌 / 解析失败 / 太大 等。
	Note string `json:"note,omitempty"`
}

// egressMachine 一台跑过 agent 的机器（指令 + 状态 + 端口）。
//
// ⚠️ 机器表以 **machineKey**（见 `egressMachineKey`）为键，而 `ID` 存的是
// **agent 自报的原始 id**：两者在用户视角下不相同（`u12:abc` vs `abc`）。
// 页面上的 `id` 一律用 key（它是这个表里的唯一句柄），agent 那边只用原始 id。
type egressMachine struct {
	ID       string `json:"id"`
	Hostname string `json:"hostname,omitempty"`
	// OwnerUserID 认领这台机器的用户（0 = 平台：管理员 / 运维 / 老 agent）。
	//
	// 归属**由认领它的 agent 令牌决定**，且是**粘性**的 —— 机器一旦被某个用户
	// 认领，别人就不能再操作它（视图里看不到、pull/apply 也拒），
	// 与线路池 `EgressProxy.OwnerUserID` 同一条原则：归属是事后唯一的追责线索。
	OwnerUserID int64 `json:"owner_user_id,omitempty"`
	// Fingerprint 最近一次上报的机器指纹（v0.31.26）。
	// 与 agent 自报的 machine id 并存：id 用来分桶，指纹用来识破"同一个 id/同一台机器"。
	Fingerprint string `json:"fingerprint,omitempty"`
	// AltIDs 同一台机器上**另外几份 agent** 自报的 id（v0.31.31）。
	//
	// 指纹相同 ⇒ 同一台物理机器。同一台机器跑两份 agent 时，两份会抢同一个
	// 隧道端口，后到的那份 `ssh -R` 必然失败 —— 现象是"我又接了一台，但出口 IP
	// 没变"。把多出来的 id 记下来，页面才能说清"请停掉哪一份"。
	AltIDs []string         `json:"alt_ids,omitempty"`
	Ports  egressAgentPorts `json:"ports"`
	// Cmd / CmdAt 待执行（或最近一条）指令。空串 = none。
	Cmd   string `json:"cmd,omitempty"`
	CmdAt int64  `json:"cmd_at,omitempty"`
	// LegacyFingerprint 指纹算法上一版的取值（v0.31.49）。
	//
	// 旧算法把**内网 IP** 也算进哈希里，于是同一台笔记本换个网络（DHCP 变了 IP、
	// 换 Wi-Fi、开热点）就会被判成"另一台机器"，凭空多出一条线路 + 一个端口。
	// 升级时 agent 会同时上报新旧两个指纹，服务端据此把老记录认回来，
	// 并把旧值留在这里，供再下次核对（老 agent 也可能只报旧值）。
	LegacyFingerprint string `json:"legacy_fingerprint,omitempty"`
	// State / StateAt 最近一次上报。
	State   egressAgentState `json:"state,omitempty"`
	StateAt int64            `json:"state_at,omitempty"`
	// LastPollAt 最近一次 poll 的时刻（v0.31.49）。
	//
	// poll 与 report 是两条独立心跳：agent 刚装好、代理还没起来时只会 poll，
	// 此时 StateAt 一直是 0 —— 页面把一台**活着**的机器显示成"离线"，
	// 用户就去点另一台早已关机的卡片，"拉起"永远等不到响应。
	LastPollAt int64 `json:"last_poll_at,omitempty"`
	// CreatedAt 首次出现时刻（用于列表稳定排序 + "这台什么时候接入的"）。
	CreatedAt int64 `json:"created_at,omitempty"`
}

// sameHostname 两个主机名是否指向同一台机器（大小写无关，空值一律不成立）。
//
// 刻意不做模糊匹配：主机名相同只是**辅证**，真正决定身份的是指纹；
// 这里只在"同一个 machine id + 同一个主机名 + 原记录已离线"这种组合下
// 才用来判断"是同一台机器换了网络"，而不是"另一台电脑"。
func sameHostname(a, b string) bool {
	x, y := strings.ToLower(strings.TrimSpace(a)), strings.ToLower(strings.TrimSpace(b))
	return x != "" && x == y
}

// egressMachineKey 机器表的键：owner + agent 自报的 machine id。
//
//	owner <= 0 → "abc" / "default"（平台：与升级前完全一致，老 agent 不受影响）
//	owner >  0 → "u12:abc"       （用户：自己的命名空间）
//
// 为什么要带 owner 前缀（而不是只在表里记一个 owner 字段就够）：不带前缀时，
// 用户只要**猜到或复用一个别人的 machine id**，就能把对方的桶顶掉（或者被对方
// 顶掉）—— 而 machine id 是 agent 自己生成、随参数上行的，构造成本为零。
// 加了前缀，"别人的 id" 落到自己的命名空间里，物理上碰不到对方。
func egressMachineKey(owner int64, id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		id = egressDefaultMachine
	}
	if owner > 0 {
		return fmt.Sprintf("u%d:%s", owner, id)
	}
	return id
}

// online 该机器最近是否上报过（或至少还在 poll）。
//
// v0.31.49：判据从"只看 StateAt"改成 max(StateAt, LastPollAt)。
// 只看上报时刻会把"agent 活着、代理还没起来"的机器显示成离线 —— 而拉起代理
// 恰恰发生在这个阶段，用户据此去点另一台真离线的机器，就永远等不到结果。
func (m *egressMachine) online(now int64) bool {
	if m == nil {
		return false
	}
	seen := m.StateAt
	if m.LastPollAt > seen {
		seen = m.LastPollAt
	}
	return seen > 0 && now-seen < egressAgentOnlineSec
}

// lastSeenAt 这台机器最后一次"说过话"的时刻（上报或 poll，取较大者）。
func (m *egressMachine) lastSeenAt() int64 {
	if m == nil {
		return 0
	}
	if m.LastPollAt > m.StateAt {
		return m.LastPollAt
	}
	return m.StateAt
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
	// 旧单机格式迁移（只做**一次**，v0.31.49 加标记）。
	//
	// 旧写法只判"default 桶缺失就迁移"，而迁移结果会被 pruneStaleMachines 当作
	// 幽灵清掉（default 桶 StateAt>0 + 无指纹 + 同机已有新记录）—— 下一次 load
	// 又把它从旧 kv 里重建出来，于是每 5 秒删一次、建一次，日志被刷满上万行。
	// 迁移过一次就写标记：**这份 kv 只值得读一次**。
	if _, ok := out[egressDefaultMachine]; !ok && len(out) == 0 && a.egressLegacyMigrateNeeded() {
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
				a.markEgressLegacyMigrated()
			}
		}
	}
	return out
}

// egressLegacyMigrateNeeded 旧单机 kv 是否还需要迁移一次。
func (a *app) egressLegacyMigrateNeeded() bool {
	v, err := a.store.GetKV(kvEgressLegacyMigrated)
	return err != nil || strings.TrimSpace(v) == ""
}

// markEgressLegacyMigrated 记下"旧 kv 已经读过了"，保证它只读一次。
func (a *app) markEgressLegacyMigrated() {
	if err := a.store.SetKV(kvEgressLegacyMigrated, "1"); err != nil {
		log.Printf("WARN: [egress] 写迁移标记失败（下次启动会再读一次旧 kv）：%v", err)
	}
}

// machinesSave 落盘机器表。
func (a *app) machinesSave(m map[string]*egressMachine) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return a.store.SetKV(kvEgressAgentMachines, string(raw))
}

// egressMachMu 机器表的**读-改-写**互斥锁（v0.31.31）。
//
// 为什么必须有（这是"两台电脑却是同一个出口 IP"的根因之一）：
// poll / report 的处理是典型的 load → modify → save（整张表一次性 JSON 覆盖）。
// 两台机器的 agent 同时 poll 时，两个请求会**各自读到同一份快照**、都算出
// "第一个空闲端口是 X"、然后各自整表写回 —— 结果两张记录拿到**同一个隧道端口**。
// sshd 只允许一个 `-R` 监听同端口，后到的那台 `ssh -R` 直接失败（日志里是
// `remote port forwarding failed`），而云端看到的是"两条线路都通"：
// 两条线路的 URL 都是 `http://127.0.0.1:X`，探测自然得到**同一个出口 IP**。
//
// 锁的粒度是"整个 load→save"，因为落盘是整表覆盖 —— 拆细了等于没锁。
// 出站探测（egressTunnelReachable / egress.Probe）**不在锁内**：那是网络 IO，
// 持锁做网络会让一个慢探测卡住所有 agent 的上报。
var egressMachMu sync.Mutex

// staleMachineAge 无指纹的陈旧机器记录保留多久（v0.31.43）。
//
// 1 天：agent 的 report 间隔是 5 秒，一条记录一整天没有任何上报，
// 只可能是"这台机器已经换身份/不再装了"，留着的唯一效果是让页面显示幽灵机器。
const staleMachineAge = 24 * 3600

// pruneStaleMachines 清掉"同一台机器升级后留下的老记录"（v0.31.43）。
//
// 症状（2026-10-18 实测）：0.1.0 的 agent 不带 machine id / 指纹，登记成 `default`；
// 升到 0.3.x 后带指纹，又登记成 `m-xxxx`。两条记录 hostname 相同、端口不同，
// 页面上就是"多了一台机器" —— 正是 v0.31.31 花大力气修掉的那个错觉又回来了，
// 而老那条永远不会再上报。
//
// 判据（**四条全中**才删，任一不满足都留着）：
//  1. 记录**没有指纹** —— 有指纹的绝不能凭 hostname 推测身份（v0.31.31 的教训）；
//  2. 同一 owner 下存在**有指纹且最近上报过**的记录；
//  3. 两者 hostname 相同且非空；
//  4. 老记录超过 staleMachineAge 没有上报。
//
// 为什么限制在"同 owner"：跨 owner 的 hostname 相同是正常的（同一台电脑上
// 平台 agent 与用户 agent 并存），删掉别人的记录不是我们该做的清理。
// machineLastSeen 这条记录最近一次已知活动的时刻。
//
// **不能直接用 StateAt**（v0.31.46 修，现场踩到）：poll 也会经 egressRegister
// 创建记录，而 poll 不带 state → StateAt 恒为 0。用 0 去算 `now-StateAt` 会得出
// "老于 1 天"，于是刚建出来的记录立刻被判为陈旧：每 5 秒 poll 建一次、prune 删一次，
// 无限循环，并且每轮打两行日志。生产实测 16 小时刷了上万条，
// 把真正要查的扫码日志整段淹掉（那次排查就是被它挡住才绕了一圈）。
//
// 语义：StateAt（上报时刻）优先，没有则用 CreatedAt（登记时刻）；两者都为 0
// 表示"无法判断年龄"，由调用方按**保留**处理。
func machineLastSeen(v *egressMachine) int64 {
	if v == nil {
		return 0
	}
	if v.StateAt > 0 {
		return v.StateAt
	}
	return v.CreatedAt
}

func pruneStaleMachines(m map[string]*egressMachine, now int64, owner int64) []string {
	fresh := map[string]bool{}
	for _, v := range m {
		if v == nil || v.OwnerUserID != owner {
			continue
		}
		if strings.TrimSpace(v.Fingerprint) == "" {
			continue
		}
		if seen := machineLastSeen(v); seen > 0 && now-seen > staleMachineAge {
			continue
		}
		if h := strings.ToLower(strings.TrimSpace(v.Hostname)); h != "" {
			fresh[h] = true
		}
	}
	if len(fresh) == 0 {
		return nil
	}
	var pruned []string
	for k, v := range m {
		if v == nil || v.OwnerUserID != owner {
			continue
		}
		// **default 桶一律不碰**（v0.31.46 修，现场踩到）。
		//
		// default = "没报自己是谁的那台"，它是**老 agent 的合法落点**：这类 agent
		// 只 poll、不带 machine id，而 poll **不写任何时间戳** —— 于是它的记录永远
		// 看起来是"很久没活动"，被这里删掉后下一次 poll 立刻重建：
		// 每 5 秒删一次建一次，16 小时刷了上万条日志（还把这轮排查要看的日志淹了）。
		//
		// 而"升级后留下的幽灵 default"**不需要这里处理** —— machineEnsure 里
		// 已有一段专门的逻辑：新 agent 带 machine id 登记时，若 hostname 与 default
		// 相同，会把 default 改名接管（v0.31.18 幽灵机器防护）。两处都做，等于
		// 一边删一边建。
		if k == egressDefaultMachine {
			// default 桶的两种情形要分开（v0.31.48）：
			//   · StateAt == 0：**只 poll、不上报状态**的老 agent —— 它每 5 秒建一次、
			//     按时间判一定"很老"，删了就被下一次 poll 重建（v0.31.46 的无限循环）。
			//     一律不碰。
			//   · StateAt > 0：**曾经上报过状态**的 default = 升级前的旧身份。
			//     若同一台机器已有带指纹的活跃记录，它就是幽灵，按下面的通用判据清掉
			//     （它不会再上报，留着只会让页面显示"两台机器"）。
			if v.StateAt == 0 {
				continue
			}
		}
		if strings.TrimSpace(v.Fingerprint) != "" {
			continue
		}
		h := strings.ToLower(strings.TrimSpace(v.Hostname))
		if h == "" || !fresh[h] {
			continue
		}
		seen := machineLastSeen(v)
		if seen == 0 {
			// 连登记时间都没有 —— 无法判断它是不是"升级留下的老记录"，
			// 宁可留着（多一条幽灵机器比删掉一台真机器便宜）。
			continue
		}
		if now-seen <= staleMachineAge {
			continue
		}
		log.Printf("DEBUG: [egress] 清理陈旧机器 %s（last_seen=%d now=%d host=%s）",
			k, seen, now, v.Hostname)
		delete(m, k)
		pruned = append(pruned, k)
	}
	return pruned
}

// pruneDriftedSiblings 清掉同一台机器"换网络"留下的旧记录（v0.31.49）。
//
// 判据（三条全中才删）：
//  1. 同 owner + 同 machine id + 同主机名 —— 只是键不同（分叉键带指纹后缀）；
//  2. **当前这台在线**（否则可能是两台交替开机的机器，删错就没了一条线路）；
//  3. 旧记录超过 staleMachineAge 没有任何心跳。
//
// 为什么需要它：v0.3.6 及以前的指纹含内网 IP，同一台笔记本换一次网络就分叉出一条
// 新记录。修好指纹之后，这些**历史**记录不会自己消失 —— 它们永远不会再上报，
// 只会让页面一直显示"两台同名电脑"。
func pruneDriftedSiblings(m map[string]*egressMachine, owner int64, cur *egressMachine, now int64) []string {
	if cur == nil || !cur.online(now) || strings.TrimSpace(cur.Hostname) == "" {
		return nil
	}
	var out []string
	for k, v := range m {
		if v == nil || v == cur || v.OwnerUserID != owner {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(v.ID), strings.TrimSpace(cur.ID)) {
			continue
		}
		if !sameHostname(v.Hostname, cur.Hostname) {
			continue
		}
		if seen := v.lastSeenAt(); seen > 0 && now-seen <= staleMachineAge {
			continue
		}
		log.Printf("INFO: [egress] 清理同机旧记录 %s（machine=%s 主机名 %s 相同，"+
			"已 %d 小时无心跳 —— 指纹修复前换网络留下的）",
			k, cur.ID, cur.Hostname, (now-v.lastSeenAt())/3600)
		delete(m, k)
		out = append(out, k)
	}
	return out
}

// machineEnsure 取一台机器；不存在则登记并**分配端口**。返回值 created 表示本次新建。
//
// owner 来自 agent 令牌（见 egressAgentAuth）：0 = 平台，>0 = 该用户。
//
// fp = 机器指纹（v0.31.31，可空）。它改变了"这台是谁"的判定方式：
//
//	· 同 owner + 同指纹就已存在 → **就是那台机器**（无论它这次报的 id 是什么）。
//	  用户重装系统 / 删掉 machine.id 之后会拿到一个新 id，若按 id 分桶就会多出
//	  一台"幽灵机器"，还占一个新端口 —— 指纹能把它认回来。
//	· 目标键已存在，但记录上的指纹**非空且不同** → 说明有**另一台机器**在复用
//	  这个 id（最常见：把整份安装目录连同 machine.id 一起拷到第二台电脑）。
//	  此时不能共用端口（共用 = 两条线路指向同一台机器 = 同一个出口 IP），
//	  必须分出一个新键 + 新端口。
func (a *app) machineEnsure(m map[string]*egressMachine, id, hostname string, owner int64, fp, fpPrev string) (mm *egressMachine, created bool) {
	rawID := strings.TrimSpace(id)
	if rawID == "" {
		rawID = egressDefaultMachine
	}
	// default 桶 = "没报自己是谁的那台"，它是**平台**的（老 agent 不带 machine 参数）。
	// 用户令牌永远不会落到这里：它的 rawID 为空时键是 `u12:default`，与 default 分开。
	isDefaultBucket := owner <= 0 && rawID == egressDefaultMachine
	if isDefaultBucket {
		// default 的 hostname 只由上报填充，避免被 query 里的空值覆盖。
		hostname = ""
	}
	key := egressMachineKey(owner, rawID)
	fp = strings.TrimSpace(fp)
	fpPrev = strings.TrimSpace(fpPrev)
	// 本次上报自报的身份集合：新算法指纹 + 旧算法指纹（升级过渡期两者并存）。
	incomingFP := map[string]bool{}
	if fp != "" {
		incomingFP[fp] = true
	}
	if fpPrev != "" {
		incomingFP[fpPrev] = true
	}

	// ---- 指纹优先（v0.31.31）----
	if len(incomingFP) > 0 {
		// (a) 同 owner + 指纹命中（新/旧算法任一）= 同一台机器：认回既有记录。
		for k, v := range m {
			if v == nil || v.OwnerUserID != owner {
				continue
			}
			recFP := strings.TrimSpace(v.Fingerprint)
			if recFP == "" || !incomingFP[recFP] {
				continue
			}
			// 指纹算法升级（v0.31.49）：旧算法把内网 IP 算进哈希，换网络就会变。
			// agent 会同时上报新旧两个值，这里把记录刷成新值，旧值留痕。
			if fp != "" && recFP != fp {
				v.LegacyFingerprint = recFP
				v.Fingerprint = fp
				log.Printf("INFO: [egress] 机器 %s 指纹算法升级（%s → %s），线路与端口保持不变",
					k, recFP, fp)
			}
			if k != key {
				// 同一个 agent 换了 id（重装/清目录），或历史升级路径留下的重复记录：
				// 把状态挪到既有记录上，并清掉这个**冗余**记录。
				//
				// 为什么要删而不是留着：一条机器记录 = 一条线路 = 一个出口端口。
				// 留着它，页面上就又多出一条"线路"，而它指向的是同一台机器 ——
				// 这正是"两台电脑却是同一个 IP"的错觉来源。
				// 只删"指纹为空或相同"的记录；指纹不同的说明是另一台机器占着这个 id，
				// 那种情形下面 (b) 会单独处理，不能动。
				if dup, ok := m[key]; ok && dup != nil &&
					(strings.TrimSpace(dup.Fingerprint) == "" || incomingFP[strings.TrimSpace(dup.Fingerprint)]) {
					delete(m, key)
					log.Printf("INFO: [egress] 指纹 %s 已登记为 %s，本次上报 id=%q 归并到既有记录（清理冗余记录 %s）",
						fp, k, rawID, key)
				} else {
					log.Printf("INFO: [egress] 指纹 %s 已登记为 %s，本次上报 id=%q 归并到既有记录",
						fp, k, rawID)
				}
			}
			if strings.TrimSpace(hostname) != "" {
				v.Hostname = hostname
			}
			if strings.TrimSpace(v.ID) == "" {
				v.ID = rawID
			}
			if rawID != "" && !strings.EqualFold(rawID, v.ID) {
				seen := false
				for _, x := range v.AltIDs {
					if strings.EqualFold(x, rawID) {
						seen = true
						break
					}
				}
				if !seen {
					v.AltIDs = append(v.AltIDs, rawID)
				}
			}
			return v, false
		}
		// (b) 身份漂移（v0.31.49）：同一个 machine id、同一个主机名，指纹却变了，
		// 而老记录**已经不在线** —— 这不是"另一台电脑"，而是同一台电脑换了网络。
		//
		// 现场就是这个坑：笔记本白天在公司（10.168.1.110）、晚上在家（192.168.1.19），
		// 旧指纹把内网 IP 算进去 → 服务端当成第二台机器，多出一条线路 + 一个新端口。
		// 用户在页面上看到两张一模一样的卡片，点了**旧那张**的「一键拉起」，
		// 指令写进一台早已下线的记录里，等 90 秒超时 —— 报告的就是"无法拉起代理"。
		//
		// 只在"老记录不在线"时认回：两台机器**同时**跑才需要各占一条线路，
		// 这时保持分叉（指纹不同 + 都在线 ⇒ 真的是两台）。
		if exist, ok := m[key]; ok && exist != nil && fp != "" {
			existFP := strings.TrimSpace(exist.Fingerprint)
			if existFP != "" && !incomingFP[existFP] &&
				sameHostname(exist.Hostname, hostname) && !exist.online(time.Now().Unix()) {
				if fpPrev != "" {
					exist.LegacyFingerprint = fpPrev
				} else {
					exist.LegacyFingerprint = existFP
				}
				exist.Fingerprint = fp
				log.Printf("INFO: [egress] machine=%s 指纹变化（%s → %s）且原记录已离线，"+
					"按「同一台机器换了网络」就地认回（线路与端口保持 %d）",
					key, existFP, fp, exist.Ports.TunnelPort)
				// 落到下面的公共分支：更新 hostname 后返回既有记录。
			} else if existFP != "" && !incomingFP[existFP] {
				// 该键已被**在线**的另一台机器占用（指纹不同）→ 复制安装目录的经典症状。
				base := key + "~" + fp
				if len(fp) > 6 {
					base = key + "~" + fp[:6]
				}
				alt := base
				for i := 2; ; i++ {
					if _, taken := m[alt]; !taken {
						break
					}
					alt = fmt.Sprintf("%s#%d", base, i)
				}
				log.Printf("WARN: [egress] machine id %q 被两台在线机器复用（指纹 %s ≠ %s）—— "+
					"第二台已改用独立键 %s 与独立端口；请确认是否复制了安装目录",
					rawID, fp, existFP, alt)
				key = alt
			}
		}
	}

	if exist, ok := m[key]; ok {
		if strings.TrimSpace(hostname) != "" {
			exist.Hostname = hostname
		}
		if fp != "" && strings.TrimSpace(exist.Fingerprint) == "" {
			exist.Fingerprint = fp // 老记录补指纹（上面 (b) 已排除"指纹不同"的情形）
		}
		return exist, false
	}
	// 幽灵机器防护（v0.31.18）：老 agent（default 桶）升级到带 machine id 的新 agent 后，
	// hostname 不变但 id 有了。若不处理，列表里会多出一台"离线机器"占着 11080，
	// 而真正在跑的那台落在 11081 上 —— 用户看到的是“我明明只有一台电脑”。
	// ⚠️ **只在平台视角生效**（v0.31.24）：这条判断的依据是 hostname 相同，而
	// 家用 Windows 的默认主机名（DESKTOP-xxxx）撞车并不罕见 —— 若让用户视角也参与，
	// 一个普通用户就能把平台那台改名带走。
	if owner <= 0 && !isDefaultBucket {
		if hn := strings.TrimSpace(hostname); hn != "" {
			if old, ok := m[egressDefaultMachine]; ok && old.Hostname != "" && strings.EqualFold(old.Hostname, hn) {
				delete(m, egressDefaultMachine)
				old.ID = rawID
				old.Hostname = hn
				m[key] = old
				log.Printf("INFO: [egress] default 桶已改名为 machine=%s（hostname %s 相同，端口保持 %d）",
					rawID, hn, old.Ports.TunnelPort)
				return old, true
			}
		}
	}
	var ports egressAgentPorts
	if isDefaultBucket {
		// default 桶恒用全局默认端口（= 升级前行为，老 agent 不受影响）。
		ports = a.egressAgentPortsOf()
	} else {
		var ok bool
		ports, ok = egressAllocPorts(m, owner)
		if !ok {
			// 段满：**如实返回 TunnelPort=0**，让调用方报明确错误 ——
			// 溢出到段外只会让隧道静默建不起来（permitlisten 会拒）。
			log.Printf("WARN: [egress] owner=%d 的隧道端口段 %s 已满，无法为新机器分配端口",
				owner, egressPortSegText(owner))
			mm = &egressMachine{
				ID: rawID, Hostname: strings.TrimSpace(hostname),
				OwnerUserID: owner, CreatedAt: time.Now().Unix(),
				Fingerprint: fp,
			}
			m[key] = mm
			return mm, true
		}
	}
	mm = &egressMachine{
		ID: rawID, Hostname: strings.TrimSpace(hostname), Ports: ports,
		OwnerUserID: owner, CreatedAt: time.Now().Unix(),
		// 指纹**在新建时就落库**（v0.31.31）：只在 report 里补的话，先到的 poll
		// 会留下一条"有 id 无指纹"的记录，于是复制目录的第二台在第一次 poll 时
		// 根本判不出来（要等下一次 report 才纠正，那一小段时间它用的是别人的端口）。
		Fingerprint: fp,
	}
	m[key] = mm
	return mm, true
}

// egressAllocPorts 给新机器分配一对端口：在**该 owner 的端口段内**找第一个未被占用的。
//
// 为什么要避开已占用的：两台机器都 `ssh -R 11080` 时，后到的那台会失败
// （`remote port forwarding failed for listen port 11080`），而这条错误**只出现在
// 本机 agent 的日志里** —— 云端页面上只会看到"隧道未连通"，运维无从判断是
// 网络问题还是端口被占了。错开端口让两台机器可以**并存**。
//
// ⚠️ 为什么必须按 owner 分段（v0.31.25）：用户的 SSH 密钥带
// `permitlisten=` 白名单（见 egress_ssh.go），白名单就是**它的段**。
// 若把用户机器分配到段外端口，隧道会被 sshd 拒绝，而现象只是"连不通"。
// 分段还顺带保证了"用户拿不到平台的隧道端口"，即无法接住本应发给平台的流量。
//
// 段满（每用户 10 台机器）时返回 ok=false，由调用方明确报错 ——
// 悄悄溢出到段外只会让隧道建不起来。
func egressAllocPorts(m map[string]*egressMachine, owner int64) (egressAgentPorts, bool) {
	base, span := egressPortSegOf(owner)
	used := map[int]bool{}
	for _, v := range m {
		if v != nil && v.Ports.TunnelPort > 0 {
			used[v.Ports.TunnelPort] = true
		}
	}
	for p := base; p < base+span; p++ {
		if !used[p] {
			return egressAgentPorts{ProxyPort: egressBaseProxyPort, TunnelPort: p}, true
		}
	}
	return egressAgentPorts{}, false
}

// egressMachinePortInSeg 该机器的隧道端口是否落在 owner 的段内。
func egressMachinePortInSeg(mm *egressMachine) bool {
	if mm == nil || mm.Ports.TunnelPort <= 0 {
		return false
	}
	base, span := egressPortSegOf(mm.OwnerUserID)
	return mm.Ports.TunnelPort >= base && mm.Ports.TunnelPort < base+span
}

// egressMigratePortsToSeg 把**端口不在自己段内**的用户机器迁回段内。
//
// 为什么需要：v0.31.24 的机器端口是全局分配的（用户机器可能拿到 11081），
// 而 v0.31.25 给用户密钥加了 `permitlisten=` 白名单（只允许 12xxx 段）——
// 那台老机器再用旧端口建隧道会被 sshd 拒绝。迁移是幂等的：端口已在段内就什么都不做。
//
// 返回发生迁移的机器数（调用方打日志）。**只在 agent 主动联系时做**（poll/report），
// 不在只读的视图里做 —— 读接口带写副作用是排障时最讨厌的东西。
func (a *app) egressMigratePortsToSeg(m map[string]*egressMachine, owner int64) int {
	if owner <= 0 {
		return 0
	}
	moved := 0
	for _, mm := range m {
		if mm == nil || mm.OwnerUserID != owner || egressMachinePortInSeg(mm) {
			continue
		}
		old := mm.Ports.TunnelPort
		ports, ok := egressAllocPorts(m, owner)
		if !ok {
			log.Printf("WARN: [egress] machine=%s 端口迁移失败：段 %s 已满", mm.ID, egressPortSegText(owner))
			continue
		}
		mm.Ports = ports
		// 端口变了 → 让 agent 重建（清掉旧指令的 At，agent 才会重新执行 start）。
		mm.Cmd, mm.CmdAt = "start", time.Now().Unix()
		moved++
		log.Printf("INFO: [egress] machine=%s 端口 %d → %d（迁入本账号段 %s）",
			mm.ID, old, ports.TunnelPort, egressPortSegText(owner))
	}
	return moved
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

// egressAgentTokenFor 取（必要时生成）某个 owner 的 agent 令牌。
//
//	uid <= 0 → 平台令牌（kv egress_agent_token，前缀 "ag-"）
//	uid >  0 → 该用户自己的令牌（kv egress_agent_token_u<uid>，前缀 "ag<uid>-"）
//
// 为什么按用户分：令牌是"能往这台云端投喂指令与状态"的凭证。全平台共用一把时，
// 任何拿到它的人都能**抢注 machine id、伪造别人的在线状态、替别人拉起/停止代理**。
// 令牌本身只授权 poll/report（读不到任何账号数据），但"能冒充别人的机器"已经
// 越过了用户之间的边界 —— 而划这条边界正是为了不让 D 类问题（见包注释）扩散。
func (a *app) egressAgentTokenFor(uid int64) (string, error) {
	key, prefix := kvEgressAgentToken, "ag-"
	if uid > 0 {
		key = fmt.Sprintf(kvEgressAgentTokenUserFmt, uid)
		prefix = fmt.Sprintf("ag%d-", uid)
	}
	if v, err := a.store.GetKV(key); err == nil {
		if t := strings.TrimSpace(v); t != "" {
			return t, nil
		}
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	t := prefix + hex.EncodeToString(buf)
	if err := a.store.SetKV(key, t); err != nil {
		return "", err
	}
	return t, nil
}

// egressAgentTokenPeek 只读某 owner 的令牌（**不生成**）。
//
// 校验路径必须用它的原因：若用 egressAgentTokenFor，一个伪造的 uid 会让服务端
// **顺手创建一把没人用的令牌**（写放大 + 无意义的 kv 增长）。
func (a *app) egressAgentTokenPeek(uid int64) string {
	key := kvEgressAgentToken
	if uid > 0 {
		key = fmt.Sprintf(kvEgressAgentTokenUserFmt, uid)
	}
	v, _ := a.store.GetKV(key)
	return strings.TrimSpace(v)
}

// egressAgentTokenParse 从用户令牌里解析出 uid（形如 "ag12-<hex>"）。
//
// 平台令牌（"ag-<hex>"）在第 3 个字符就是 '-'，解析器自然拒绝 ——
// 两者的前缀形态互不重叠，不需要额外判别分支。
func egressAgentTokenParse(tok string) (int64, bool) {
	if !strings.HasPrefix(tok, "ag") {
		return 0, false
	}
	rest := tok[2:]
	i := strings.IndexByte(rest, '-')
	if i <= 0 {
		return 0, false
	}
	uid, err := strconv.ParseInt(rest[:i], 10, 64)
	if err != nil || uid <= 0 {
		return 0, false
	}
	return uid, true
}

// tokensEqual 定长比较（长度相同才比内容），避免把令牌长度也变成可观测信号。
func tokensEqual(got, want string) bool {
	if want == "" || len(got) != len(want) {
		return false
	}
	var diff byte
	for i := 0; i < len(got); i++ {
		diff |= got[i] ^ want[i]
	}
	return diff == 0
}

// egressAgentAuth agent 请求的令牌校验，返回**这台机器归谁**（0 = 平台）。
//
// 两条通道：平台令牌（管理员 / 运维 / 老 agent）与用户令牌（每用户一把）。
// 顺序上先试平台令牌再解析用户令牌 —— 平台令牌不含 uid 段，解析器本来也会拒绝它，
// 这里显式排前面只是为了读起来更直白。
func (a *app) egressAgentAuth(r *http.Request) (int64, bool) {
	got := strings.TrimSpace(r.Header.Get("X-Agent-Token"))
	if got == "" {
		return 0, false
	}
	if want, err := a.egressAgentTokenGet(); err == nil && tokensEqual(got, want) {
		return 0, true
	}
	if uid, ok := egressAgentTokenParse(got); ok {
		if tokensEqual(got, a.egressAgentTokenPeek(uid)) {
			return uid, true
		}
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// poll / report
// ---------------------------------------------------------------------------

// egressRegister 登记/更新一台机器（**唯一入口**，全程持锁）。
//
// 为什么把它单独抽出来（v0.31.31）：poll 与 report 都是
// `machinesLoad → machineEnsure → machinesSave` 的读-改-写，而这个"表"是
// **整张 JSON 一次性覆盖**。两处各写一遍临界区，只要有一处漏锁，就会回到
// "两台机器抢同一个端口"的老毛病上；抽成一个函数后，锁只在一个地方存在，
// 新加的入口（比如将来的心跳）也只能走它。
//
// owner 来自 agent 令牌；st != nil 表示这次带状态上报（report），否则只是取指令（poll）。
// dupAgent = 这次上报的 machine id 与**该指纹名下已登记的那台**不一致 ——
// 即同一台机器上跑了多份 agent。
func (a *app) egressRegister(id, hostname, fp, fpPrev string, owner int64, st *egressAgentState) (mm *egressMachine, created, dupAgent bool, err error) {
	egressMachMu.Lock()
	defer egressMachMu.Unlock()

	m := a.machinesLoad()
	mm, created = a.machineEnsure(m, id, hostname, owner, fp, fpPrev)
	// 老机器端口可能不在本账号段内（v0.31.24 全局分配 → v0.31.25 分段），迁回来。
	moved := a.egressMigratePortsToSeg(m, owner)
	// 顺手清掉同一台机器升级后留下的无指纹老记录（v0.31.43）。
	// 放在这里而不是单独定时任务：机器表只在 agent 上报时才有新事实，
	// 清理跟着上报走，就不需要再引入一个后台循环去猜"什么时候该清"。
	pruned := pruneStaleMachines(m, time.Now().Unix(), owner)
	// 连带清掉"换网络留下的同机旧记录"（v0.31.49）：同 owner + 同 machine id +
	// 同主机名，且离线超过一天。指纹漂移修复之前，同一台笔记本每换一次网络
	// 就会多出一条这样的记录，页面上就是两个同名卡片 —— 用户分不清该点哪个。
	pruned = append(pruned, pruneDriftedSiblings(m, owner, mm, time.Now().Unix())...)
	for _, k := range pruned {
		log.Printf("INFO: [egress] 清理陈旧机器记录 %s（无指纹且已有同机新记录）", k)
	}

	if st != nil {
		mm.State, mm.StateAt = *st, st.At
		if strings.TrimSpace(st.Hostname) != "" {
			mm.Hostname = strings.TrimSpace(st.Hostname)
		}
		if s := strings.TrimSpace(st.Fingerprint); s != "" {
			mm.Fingerprint = s
		}
		if s := strings.TrimSpace(st.FingerprintPrev); s != "" && s != mm.Fingerprint {
			mm.LegacyFingerprint = s
		}
	}
	// 每次 poll / report 都记一次心跳（v0.31.49）：只看 StateAt 时，"agent 活着、
	// 代理还没起来"的机器会被判离线 —— 而那正是用户点「拉起」的时刻。
	now := time.Now().Unix()
	if now-mm.LastPollAt >= 1 {
		mm.LastPollAt = now
	}

	rawID := strings.TrimSpace(id)
	if rawID == "" {
		rawID = egressDefaultMachine
	}
	// 只在"确实带着 id"时判重复：老 agent 不带 machine 参数（rawID=default），
	// 它与普通记录本来就不同键，不能算"多装了一份"。
	dupAgent = strings.TrimSpace(id) != "" && mm.ID != "" && !strings.EqualFold(rawID, mm.ID)

	// LastPollAt 也要落盘：只在 poll 时它才变，而 poll 的其余字段都不变，
	// 若沿用"created/moved/pruned/st"这套条件，心跳会永远写不进去。
	if created || moved > 0 || len(pruned) > 0 || st != nil || mm.LastPollAt == now {
		if e := a.machinesSave(m); e != nil {
			return mm, created, dupAgent, e
		}
	}
	return mm, created, dupAgent, nil
}

// handleEgressAgentPoll GET /api/agent/poll?machine=<id>&hostname=<h>
//
// agent 取指令的入口。**每次都返回当前指令**（而不是"取走即清"）：
// agent 可能重启，清空式会丢掉"该拉起"的事实；幂等交给 agent 用 At 判断
// （同一条指令只执行一次）。
//
// machine 参数可省（老 agent 不带）→ 归入 "default" 桶，行为与 v0.31.17 一致。
func (a *app) handleEgressAgentPoll(w http.ResponseWriter, r *http.Request) {
	owner, ok := a.egressAgentAuth(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid agent token"})
		return
	}
	// v0.31.53：用户端代理已下线 —— 用户身份的 agent 一律拒之门外。
	// 否则这些机器会每 5 秒写一次机器表，页面上"看得见、没线路"，永远清不完。
	if owner > 0 {
		if shouldLogUserAgentGone(owner) {
			log.Printf("WARN: [egress] 拒绝用户 agent 接入（owner=#%d machine=%q）：用户端代理已下线。"+
				"请让该用户删除本机安装目录 %s（同类日志 5 分钟内只记一条）",
				owner, r.URL.Query().Get("machine"), `%LOCALAPPDATA%\wb2api-egress`)
		}
		writeUserEgressGone(w)
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("machine"))
	host := strings.TrimSpace(r.URL.Query().Get("hostname"))
	fp := strings.TrimSpace(r.URL.Query().Get("fingerprint"))
	fpPrev := strings.TrimSpace(r.URL.Query().Get("fingerprint_prev"))
	mm, created, dupAgent, err := a.egressRegister(id, host, fp, fpPrev, owner, nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// 同一台机器上的**非主 agent**：直接让它停（判据见 egressRegister）。
	if dupAgent {
		log.Printf("WARN: [egress] machine id %q 与 %q 同指纹（同一台机器跑了两份 agent）—— 已让前者停止",
			id, mm.ID)
		writeJSON(w, http.StatusOK, map[string]any{
			"action": "stop",
			"note": "同一台机器上已有一份 agent 在运行（指纹相同）；这一份请直接关闭，" +
				"否则两份会抢同一个隧道端口，出口 IP 也不会多出来",
			"proxy_port": mm.Ports.ProxyPort, "tunnel_port": mm.Ports.TunnelPort,
		})
		return
	}
	if created {
		log.Printf("INFO: [egress] 新 agent 已登记 machine=%s owner=%d host=%q tunnel_port=%d",
			mm.ID, owner, mm.Hostname, mm.Ports.TunnelPort)
	}
	if mm.Ports.TunnelPort <= 0 {
		// 段满：明确告诉 agent 与用户，而不是给一个 0 端口让它去建隧道。
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": fmt.Sprintf("本账号的隧道端口已用完（%s，最多 %d 台机器）"+
				"——请先停用不再使用的机器，或联系管理员扩容",
				egressPortSegText(owner), egressUserPortSpan),
			"retryable": false,
		})
		return
	}
	cmd := egressAgentCmd{Action: "none", ProxyPort: mm.Ports.ProxyPort, TunnelPort: mm.Ports.TunnelPort}
	if mm.Cmd != "" {
		cmd.Action, cmd.At = mm.Cmd, mm.CmdAt
	}
	writeJSON(w, http.StatusOK, cmd)
}

// handleEgressAgentReport POST /api/agent/report?machine=<id>
func (a *app) handleEgressAgentReport(w http.ResponseWriter, r *http.Request) {
	owner, ok := a.egressAgentAuth(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid agent token"})
		return
	}
	if owner > 0 {
		writeUserEgressGone(w)
		return
	}
	var st egressAgentState
	if err := json.NewDecoder(r.Body).Decode(&st); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	st.At = time.Now().Unix()
	mm, _, _, err := a.egressRegister(r.URL.Query().Get("machine"), st.Hostname,
		st.Fingerprint, st.FingerprintPrev, owner, &st)
	if err != nil {
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
	// BoundAccounts 这条线路当前**承载了几个账号**（含平台号与自挂号）。
	//
	// v0.31.53：它取代了原来的 host_bound/host_total —— 那两个只数自挂号，
	// 而用户端代理下线后，平台视角下自挂号恒为 0，拿它判"承重"永远显示待命。
	// 承重的定义应是"这条出口上真的挂着要出去的号"，不分号是谁的。
	BoundAccounts int `json:"bound_accounts"`
	// Carrying 是否正在承重（BoundAccounts > 0）。
	//
	// 两台机器都连着时，只有挂了号的那台在真正出流量；不标出来，
	// 运维会以为两台都在工作（"两个 IP 却是同一个出口"的错觉来源）。
	Carrying bool `json:"carrying"`
	// AuthCandidates 这台机器上探测到的客户端登录态候选（v0.31.42）。
	//
	// 只有元信息（路径 / 修改时间 / uid 前 8 位）。用途是回答"这个号的本机
	// 客户端登录态在哪" —— 那是"让本机成为唯一刷新者"这条路的第一步。
	AuthCandidates []agentAuthCandidate `json:"auth_candidates,omitempty"`
	// CurrentCmd 当前待执行指令（none / start / stop）。
	CurrentCmd string `json:"current_cmd,omitempty"`
	LastAt     int64  `json:"last_at"`
	// IsDefault 是否是不带 machine 参数的老 agent（default 桶）。
	IsDefault bool `json:"is_default"`
	// Fingerprint 机器指纹（v0.31.26）：同指纹 = **同一台机器**跑了两份 agent。
	Fingerprint string `json:"fingerprint,omitempty"`
	// SameMachineOf 与本机同指纹的其它机器 key —— 非空即"重复接入"，
	// 这类"两条线路"必然同一个出口，还会互相抢隧道端口。
	SameMachineOf []string `json:"same_machine_of,omitempty"`
	// AltIDs 同一台机器上另外几份 agent 自报的 id（v0.31.31）。
	// 非空 = 一台电脑装了多份 agent，它们抢同一个隧道端口，多出来的出口并不存在。
	AltIDs []string `json:"alt_ids,omitempty"`
	// TunnelExitIP **云端从隧道这一侧实测**的出口 IP（v0.31.31）。
	//
	// 与 ExitIP（agent 自报，走本机代理直出）是两条不同的链路：
	//   · ExitIP      = "这台机器如果自己上网，看到的是什么 IP"
	//   · TunnelExitIP = "平台通过这条隧道出去，实际从哪个 IP 出口"
	// 两者不一致，说明**隧道没有落到这台机器上**（端口被另一台占了 / 转发没建起来），
	// 而页面会同时显示两条"线路"，看起来像"两个出口"。
	TunnelExitIP string `json:"tunnel_exit_ip,omitempty"`
	// ExitMismatch 上面两者都有值且不相等 → 隧道指向的不是这台机器。
	ExitMismatch bool `json:"exit_mismatch,omitempty"`
	// SameExitOf 出口 IP 相同、但指纹不同的其它机器 key。
	// 它不是配置错误（两台机器同一条宽带 / 同一 NAT / 双线负载均衡），
	// 但**不会带来新 IP** —— 后台必须把它说出来，否则"我又接了一台"是错觉。
	SameExitOf []string `json:"same_exit_of,omitempty"`
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
	// BoundAccounts / Carrying 汇总取"最近上报那台"（见下方 ActiveMachine）。
	BoundAccounts int   `json:"bound_accounts"`
	Carrying      bool  `json:"carrying"`
	AppliedAt     int64 `json:"applied_at"`
	Ready         bool  `json:"ready"`
	// ActiveMachine 上面那批汇总字段属于哪台机器（无则空）。
	ActiveMachine string `json:"active_machine,omitempty"`
	// CanManage 当前视角能否执行操作。v0.31.24 起**两种视角都为 true** ——
	// 区别不在"能不能操作"，而在"能操作谁"（见 ManageScope）。
	CanManage bool `json:"can_manage"`
	// ManageScope 当前视角的操作范围（v0.31.24）：
	//
	//	"platform" 平台视角（管理员 / 运维密钥）—— 全部机器；应用出口时会设全局默认；
	//	"self"     用户视角 —— 只有自己的机器；应用出口只绑自己名下的号。
	//
	// 前端据此切换文案：对用户说「全局默认出口」是误导（他没有这个开关）。
	ManageScope string `json:"manage_scope"`
	PollSec     int    `json:"poll_sec"`
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

// ---------------------------------------------------------------------------
// 并发防护（v0.31.25）
//
// 「一键拉起」是一个**会被连点**的按钮，而它背后是几十上百个账号的写入。
// 前端禁用按钮只能防住同一个页面，防不住多标签页 / 手抖双击 / 脚本重放，
// 所以后端必须自己兜底。
// ---------------------------------------------------------------------------
var (
	egressDedupMu sync.Mutex
	// egressPullAtMap machineKey → 上次下发 start 的时刻。
	egressPullAtMap = map[string]int64{}
	// egressApplyAtMap machineKey → 开始应用的时刻（应用完成即清）。
	egressApplyAtMap = map[string]int64{}
)

const (
	// egressPullDedupSec 重复下发指令的去抖窗口。
	//
	// 为什么值得去抖：重复下发会**刷新指令的 At**，而 agent 用 At 判断"这条指令
	// 我执行过没有" —— At 一变它就会 stopAll 再重建一次代理与隧道。
	// 连点三下 = 隧道被拆建三次，表现是"点了反而更慢、还一闪一闪"。
	egressPullDedupSec = 3
	// egressApplyGuardSec 应用动作的忙碌判定窗口（超时自动放行，防残留）。
	//
	// 用时间戳而不是 bool：若某次应用因异常路径提前返回而没清标志，
	// 一个 bool 会让这台机器**永久卡在"正在应用中"**，只能重启服务。
	egressApplyGuardSec = 120
)

// egressPullDeduped 判断本次 start 下发是否在去抖窗口内（是则不该重复写指令）。
func egressPullDeduped(key string) bool {
	now := time.Now().Unix()
	egressDedupMu.Lock()
	defer egressDedupMu.Unlock()
	if at, ok := egressPullAtMap[key]; ok && now-at < egressPullDedupSec {
		return true
	}
	egressPullAtMap[key] = now
	return false
}

// egressApplyBegin 尝试开始一次应用；返回的函数用于结束时释放。
//
// busy=true 表示同一台机器已有一次应用在进行（或刚开始不到 guard 窗口）。
func egressApplyBegin(key string) (release func(), busy bool) {
	now := time.Now().Unix()
	egressDedupMu.Lock()
	if at, ok := egressApplyAtMap[key]; ok && now-at < egressApplyGuardSec {
		egressDedupMu.Unlock()
		return func() {}, true
	}
	egressApplyAtMap[key] = now
	egressDedupMu.Unlock()
	return func() {
		egressDedupMu.Lock()
		delete(egressApplyAtMap, key)
		egressDedupMu.Unlock()
	}, false
}

// egressScope 谁在执行这次出口动作。
//
// v0.31.53 之后**只有一个合法取值**：Platform=true（管理员会话 / X-Admin-Key）。
// 用户端代理已整体下线，那个"UserID>0 → 只看自己机器、只绑自己的号"的视角
// 连同它的实现一起删掉了。
//
// 字段保留而不是直接删掉理由：视图、日志、agent 令牌派生都以 egressScope 为参数，
// 一次性拆掉会制造一大片与本次需求无关的改动面。但语义已经收紧到 **fail-closed**：
// 非平台视角在 seesMachine 处看不到任何机器，而不是"按 owner 收窄"——
// 万一将来有人误传 Platform=false，他拿到的是"什么都看不到"，不是一个
// 半开的后门。
type egressScope struct {
	// UserID 操作者（仅用于审计与日志，**不再参与范围收窄**）。
	UserID int64
	// Platform 是否平台视角。v0.31.53 起必须为 true。
	Platform bool
}

// kind 视角标识（v0.31.53 起只会是 "platform"；非平台返回 "invalid"）。
//
// 前端据此决定文案。返回 "invalid" 而不是硬编码 "platform"，
// 是为了让误用**看得见**（页面上会显示出异常值，而不是悄悄按平台渲染）。
func (s egressScope) kind() string {
	if s.Platform {
		return "platform"
	}
	return "invalid"
}

// hostOwner 出口线路与自挂号的归属：平台恒为 0（不按人收窄）。
func (s egressScope) hostOwner() int64 { return 0 }

// seesMachine 该视角能否看见（并操作）这台机器。
//
// fail-closed：只有平台视角能看见机器。用户视角已下线，
// 保留 "return false" 是为了让任何误用都表现为"看不到"而不是"全看得见"。
func (s egressScope) seesMachine(mm *egressMachine) bool {
	return mm != nil && s.Platform
}

func (s egressScope) label() string {
	if s.Platform {
		return "platform"
	}
	return "invalid"
}

// egressPlatformGuard 出口执行端点的入口守卫（v0.31.53 收紧）。
//
// v0.31.24 起这个位置叫 egressActorGuard，语义是"普通用户也能过，只是只能碰
// 自己的机器"。v0.31.53 产品决定把出口收敛为平台能力 —— 用户端不再有代理入口
// 与接口权限，于是这里**只放行平台身份**：
//
//   - 运维密钥（X-Admin-Key）：脚本化运维通道，语义上等同平台视角；
//   - 管理员会话：平台视角（可设全局默认出口、操作任意平台机器）；
//   - 普通用户：403 `platform_only`（不是 401 —— 他是登录着的，只是没这个权限）。
//
// 为什么不复用 adminGuard：后续 handler 需要拿到 **egressScope**（Platform 标志），
// 而 adminGuard 只注入 *User。少了这个标志，下游会退回到"按 owner 收窄"的老路径，
// 那正是这次要删掉的东西。
func (a *app) egressPlatformGuard(next func(http.ResponseWriter, *http.Request, egressScope)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.authorized(r) { // X-Admin-Key 通道（运维 / 脚本）
			// v0.31.57：运维通道同样受平台开关约束 —— 停用插件的语义是
			// "这项能力现在不提供"，不是"只对浏览器不提供"。
			if a.egressPluginGate(w) {
				return
			}
			next(w, r, egressScope{Platform: true})
			return
		}
		u := a.currentUser(r)
		if u == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "未登录"})
			return
		}
		if !u.IsAdmin() {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error": "出口代理是平台能力，仅管理员可用；用户端代理已下线",
				"code":  "platform_only",
			})
			return
		}
		if u.Status != StatusActive {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "账号已被停用"})
			return
		}
		if a.egressPluginGate(w) {
			return
		}
		next(w, withUser(r, u), egressScope{UserID: u.ID, Platform: true})
	}
}

// egressPluginGate 插件停用时**真的**拒绝执行（v0.31.57）。
//
// 为什么必须有这一层：此前「停用插件」只做到"列表里标灰 + 打开按钮禁用"，
// 而 egress 系列是独立界面，不经过 handlePluginClaim，于是停用之后
// 直接调 /api/egress/home/pull 仍然能把隧道拉起来 —— 那个开关是假的。
//
// 口径与 handlePluginClaim 一致：**平台停用 = 一律不可用**。
// 返回 true 表示已经写过响应，调用方直接 return。
func (a *app) egressPluginGate(w http.ResponseWriter) bool {
	if a.pluginEnabled(pluginIDHostEgress) {
		return false
	}
	writeJSON(w, http.StatusForbidden, map[string]any{
		"error":  "「平台直属出口」插件已被停用：请先在「插件」页启用后再操作",
		"code":   "plugin_disabled",
		"plugin": pluginIDHostEgress,
		"hint":   "管理员可在用户控制台「插件」页（或管理后台「插件」页）重新启用；停用期间已建立的隧道不受影响，但不能拉起/应用/修复",
	})
	return true
}

// egressMachineFor 取一台机器并按视角校验归属。
//
// ⚠️ 拒绝时**不区分**"这台不存在"与"这台不是你的"：分开说等于给了一个
// 能枚举别人 machine id 的探针（"这个 id 存在 → 是别人的机器"）。
func (a *app) egressMachineFor(m map[string]*egressMachine, id string, scope egressScope) (*egressMachine, bool) {
	mm, ok := m[strings.TrimSpace(id)]
	if !ok || !scope.seesMachine(mm) {
		return nil, false
	}
	return mm, true
}

// defaultEgress 当前全局默认出口。
func (a *app) defaultEgress() string {
	if kv, err := a.store.GetKV(kvEgressDefault); err == nil {
		return strings.TrimSpace(kv)
	}
	return ""
}

// buildEgressHomeView 组装状态（只读，不做任何写操作）。
//
// scope（v0.31.24）：平台视角看全部机器、并显示全局默认出口；
// 用户视角**只列自己的机器**，且不给全局默认出口（那个开关不属于他）。
func (a *app) buildEgressHomeView(r *http.Request, scope egressScope) egressHomeView {
	now := time.Now().Unix()
	m := a.machinesLoad()
	def := a.defaultEgress()
	v := egressHomeView{
		PollSec: egressAgentPollSec, CanManage: true, ManageScope: scope.kind(),
	}
	if scope.Platform {
		v.DefaultEgress = def
	}

	// 线路池：按 URL 认线路（名字是给人看的、可改，URL 才是身份）。
	proxyIDByURL := map[string]string{}
	// proxyLastIP = 该线路**最近一次探活（从云端经隧道发出）**看到的出口 IP。
	// 这个值不采信 agent 自报，因此可以用来校验"隧道到底通到哪台机器"。
	proxyLastIP := map[string]string{}
	// boundByURL 每个 URL 上挂了几个号（v0.31.52，R6）：用户开两台电脑时，
	// 只有"真的有号挂上去的那台"在承重，另一台只是连着 —— 页面必须说清是哪台，
	// 否则用户会以为两台都在工作，而实际出口始终只有一台。
	boundByURL := map[string]int{}
	if accs, err := a.store.ListAccounts(); err == nil {
		for _, acc := range accs {
			if acc == nil {
				continue
			}
			raw := strings.TrimSpace(acc.EgressProxy)
			if raw == "" || egress.IsDirect(raw) {
				continue
			}
			if c, cerr := egress.Normalize(raw); cerr == nil {
				boundByURL[c]++
			}
		}
	}
	if proxies, err := a.store.ListEgressProxies(); err == nil {
		for _, p := range proxies {
			u := egressURLKey(p.URL)
			proxyIDByURL[u] = p.ID
			if ip := normalizeExitIP(p.LastIP); ip != "" {
				proxyLastIP[u] = ip
			}
		}
	}

	// ⚠️ 列表用**机器表的键**（而非 mm.ID）作为页面句柄：用户视角下键是
	// `u12:abc`，两个不同 owner 的机器各自独立命名空间，前端回传什么我们就查什么。
	keys := make([]string, 0, len(m))
	for key, mm := range m {
		if !scope.seesMachine(mm) {
			continue
		}
		keys = append(keys, key)
	}
	urls := make([]string, 0, len(keys))
	for _, key := range keys {
		urls = append(urls, egressHomeURL(m[key].Ports.TunnelPort))
	}
	// v0.31.53：承重统计改为**全部账号**。原来只数自挂号（scope.hostOwner()），
	// 在平台视角下恒为 0 —— 那会让"承重机"永远显示待命，比不显示更误导。
	boundAll := a.egressBoundByURL(urls)

	// 同指纹 / 同出口分组（v0.31.26）：见 egressMachineView 两个字段的注释。
	fpGroup := map[string][]string{}
	exitGroup := map[string][]string{}
	for _, k := range keys {
		if fp := strings.TrimSpace(m[k].Fingerprint); fp != "" {
			fpGroup[fp] = append(fpGroup[fp], k)
		}
		if ip := normalizeExitIP(m[k].State.ExitIP); ip != "" {
			exitGroup[ip] = append(exitGroup[ip], k)
		}
	}

	for _, key := range keys {
		mm := m[key]
		url := egressHomeURL(mm.Ports.TunnelPort)
		// Applied 只有一个口径（v0.31.53）：这条线路就是当前的全局默认出口。
		// 用户视角那个"我名下号是否全部绑上"的判据随用户端一起下线。
		applied := def == url && url != ""
		mv := egressMachineView{
			ID: key, Hostname: mm.Hostname,
			Online:  mm.online(now),
			ProxyOK: mm.State.ProxyOK, TunnelOK: mm.State.TunnelOK,
			AgentVersion: mm.State.Version, Message: mm.State.Message,
			ExitIP:    mm.State.ExitIP,
			ProxyPort: mm.Ports.ProxyPort, TunnelPort: mm.Ports.TunnelPort,
			ProxyURL:       url,
			ProxyID:        proxyIDByURL[url],
			Applied:        applied,
			BoundAccounts:  boundAll[url],
			Carrying:       boundAll[url] > 0,
			AuthCandidates: append([]agentAuthCandidate(nil), mm.State.AuthCandidates...),
			CurrentCmd:     mm.Cmd,
			LastAt:         mm.StateAt,
			IsDefault:      key == egressDefaultMachine,
			Fingerprint:    strings.TrimSpace(mm.Fingerprint),
			AltIDs:         append([]string(nil), mm.AltIDs...),
			TunnelExitIP:   proxyLastIP[url],
		}
		// 两条链路对不上 = 隧道没落到这台机器上（v0.31.31）。
		// 只在两边都有值时才判：任一侧缺值（线路还没探过 / agent 还没探到）
		// 属于"不知道"，不能当成"不一致"去报警。
		if aIP := normalizeExitIP(mm.State.ExitIP); aIP != "" && mv.TunnelExitIP != "" {
			mv.ExitMismatch = aIP != mv.TunnelExitIP
		}
		if fp := strings.TrimSpace(mm.Fingerprint); fp != "" {
			for _, k := range fpGroup[fp] {
				if k != key {
					mv.SameMachineOf = append(mv.SameMachineOf, k)
				}
			}
		}
		if ip := normalizeExitIP(mm.State.ExitIP); ip != "" {
			for _, k := range exitGroup[ip] {
				if k == key {
					continue
				}
				// 同指纹的已经由 SameMachineOf 表达了，不重复报两次。
				if fp := strings.TrimSpace(mm.Fingerprint); fp != "" && strings.TrimSpace(m[k].Fingerprint) == fp {
					continue
				}
				mv.SameExitOf = append(mv.SameExitOf, k)
			}
		}
		sort.Strings(mv.SameMachineOf)
		sort.Strings(mv.SameExitOf)
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
	// 空表也要给**空数组**而不是 null：前端 `(v.machines || [])` 虽能兜住，
	// 但 null 会让"没有任何 agent 连上来"和"接口没返回这个字段"长得一样。
	if v.Machines == nil {
		v.Machines = []egressMachineView{}
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
		v.BoundAccounts, v.Carrying = active.BoundAccounts, active.Carrying
		v.Ready = active.Ready
	}
	if scope.Platform {
		if kv, err := a.store.GetKV(kvEgressAppliedAt); err == nil {
			if n, err2 := parseI64(kv); err2 == nil {
				v.AppliedAt = n
			}
		}
	}
	// 命令用**当前视角自己的令牌**：用户拿到的命令里必须是他的那把
	// （用它启动的 agent 上报的机器才会归属他自己），而管理员的仍是平台令牌。
	if tok, err := a.egressAgentTokenFor(scope.hostOwner()); err == nil {
		v.AgentCommand = fmt.Sprintf(
			"wb2api-egress-agent.exe -server %s -token %s", requestOrigin(r), tok)
	}
	return v
}

// handleEgressHome GET /api/egress/home（登录可见，只读）
func (a *app) handleEgressHome(w http.ResponseWriter, r *http.Request) {
	if a.egressPluginGate(w) {
		return
	}
	// v0.31.53：这个视图只剩平台视角（路由挂 adminGuard，普通用户进不来）。
	//
	// 运维密钥（X-Admin-Key）通道没有会话 → userFrom 为 nil，UserID 保持 0；
	// Platform 恒为 true，`scope.hostOwner()` 因此恒返回 0（平台机器）。
	scope := egressScope{Platform: true}
	if u := userFrom(r); u != nil {
		scope.UserID = u.ID
	}
	writeJSON(w, http.StatusOK, a.buildEgressHomeView(r, scope))
}

// egressBoundByURL 每条线路 URL 被多少个账号绑定（**含平台号与自挂号**）。
//
// v0.31.53 起它是**唯一**的承重口径：出口代理只剩平台能力，
// "我的号绑了几个"这个问题（以及为它服务的按 owner 统计）随用户端一起下线。
func (a *app) egressBoundByURL(urls []string) map[string]int {
	out := map[string]int{}
	for _, u := range urls {
		out[u] = 0
	}
	accs, err := a.store.ListAccounts()
	if err != nil {
		return out
	}
	for _, acc := range accs {
		if u := strings.TrimSpace(acc.EgressProxy); u != "" {
			if _, ok := out[u]; ok {
				out[u]++
			}
		}
	}
	return out
}

// egressOwnerNames 线路 URL → **声明者**用户名（v0.31.23）。
//
// 「谁把它接进来的」与「现在谁的号在用它」是两件事，管理后台两个都要看：
// 前者用于追责/联系（这条线是某个用户给的），后者用于判断下线它会影响谁。
// 这里算前者（`EgressProxy.OwnerUserID`）；后者见 egressBoundNamesByURL。
//
// 用户已删时回落成 "用户#<id>"：宁可显示一个可追溯的 id，也不要显示空 ——
// 空会让"平台手填"与"用户已注销"长得一样，而这两件事的处理完全不同。
func (a *app) egressOwnerNames(list []*EgressProxy) map[string]string {
	out := map[string]string{}
	need := map[int64]bool{}
	for _, p := range list {
		if p.OwnerUserID > 0 {
			need[p.OwnerUserID] = true
		}
	}
	if len(need) == 0 {
		return out
	}
	users, err := a.store.ListUsers()
	if err != nil {
		log.Printf("WARN: [egress] 读取用户名失败（归属将显示为用户#id）：%v", err)
	}
	nameOf := map[int64]string{}
	for _, u := range users {
		nameOf[u.ID] = u.Username
	}
	for _, p := range list {
		if p.OwnerUserID <= 0 {
			continue
		}
		if n := nameOf[p.OwnerUserID]; n != "" {
			out[strings.TrimSpace(p.URL)] = n
		} else {
			out[strings.TrimSpace(p.URL)] = fmt.Sprintf("用户#%d", p.OwnerUserID)
		}
	}
	return out
}

// egressBoundNamesByURL 线路 URL → **正在用它**的账号归属用户名列表（去重、稳定排序）。
//
// 与 egressBoundByURL 的分工：那个给"几个号"，这个给"哪几个人的号"。
// 「一条用户提供的线路上挂着别人的号」是最需要被一眼看见的状态（见
// handleEgressAssign 的排除逻辑 —— 正常路径下不该出现，出现即意味着人工显式分配过）。
func (a *app) egressBoundNamesByURL(urls []string) map[string][]string {
	out := map[string][]string{}
	want := map[string]bool{}
	for _, u := range urls {
		want[u] = true
		out[u] = []string{}
	}
	accs, err := a.store.ListAccounts()
	if err != nil {
		return out
	}
	ids := map[int64]bool{}
	for _, acc := range accs {
		if u := strings.TrimSpace(acc.EgressProxy); u != "" && want[u] && acc.OwnerUserID > 0 {
			ids[acc.OwnerUserID] = true
		}
	}
	if len(ids) == 0 {
		return out
	}
	nameOf := map[int64]string{}
	if users, err := a.store.ListUsers(); err == nil {
		for _, u := range users {
			nameOf[u.ID] = u.Username
		}
	}
	for _, acc := range accs {
		u := strings.TrimSpace(acc.EgressProxy)
		if u == "" || !want[u] || acc.OwnerUserID <= 0 {
			continue
		}
		n := nameOf[acc.OwnerUserID]
		if n == "" {
			n = fmt.Sprintf("用户#%d", acc.OwnerUserID)
		}
		dup := false
		for _, x := range out[u] {
			if x == n {
				dup = true
				break
			}
		}
		if !dup {
			out[u] = append(out[u], n)
		}
	}
	for u := range out {
		sort.Strings(out[u])
	}
	return out
}

// egressMachinesByURL 把"哪些线路是本机 agent 拉起来的"按线路 URL 索引出来。
//
// 管理后台的线路池需要这一行：一条 `http://127.0.0.1:11080` 在手填线路里毫无
// 特殊之处，但它的**失效语义完全不同** —— 隧道断 / agent 离线，整条线路一起失效，
// 而商业代理不会有"机器"这个概念。不标出来，运维只会看到"探活失败"。
func (a *app) egressMachinesByURL() map[string]map[string]any {
	out := map[string]map[string]any{}
	now := time.Now().Unix()
	all := a.machinesLoad()

	// 同指纹 / 同出口 IP 的分组（v0.31.26）。
	//
	// 这两个分组回答的是两个不同的问题，别混：
	//   - 同指纹 = **同一台机器**跑了两份 agent（拷贝了安装目录 / 老 agent 占了 default 桶）。
	//     这类"两条线路"必然同一个 IP，而且互相抢隧道端口。
	//   - 同出口 IP、指纹不同 = 两台不同机器，但从同一条宽带/同一个 NAT 出去
	//     （双线负载均衡、公司出口、同户多机）。这不是配置错误，但**不会带来新 IP**。
	// 隧道实测出口（v0.31.31）：线路池里 last_ip 是**云端经该 URL 的隧道探到的**，
	// 与 agent 自报的 exit_ip 是两条不同链路。两者不一致 = 这条隧道没落到那台机器。
	proxyLastIP := map[string]string{}
	if proxies, err := a.store.ListEgressProxies(); err == nil {
		for _, p := range proxies {
			if ip := normalizeExitIP(p.LastIP); ip != "" {
				proxyLastIP[strings.TrimSpace(p.URL)] = ip
			}
		}
	}
	// 承重机判定（v0.31.52，R6）：每个出口 URL 上挂了几个号。
	boundByURL := map[string]int{}
	if accs, err := a.store.ListAccounts(); err == nil {
		for _, acc := range accs {
			if acc == nil {
				continue
			}
			raw := strings.TrimSpace(acc.EgressProxy)
			if raw == "" || egress.IsDirect(raw) {
				continue
			}
			if c, cerr := egress.Normalize(raw); cerr == nil {
				boundByURL[c]++
			}
		}
	}

	byFP := map[string][]string{}
	byExit := map[string][]string{}
	for key, mm := range all {
		if fp := strings.TrimSpace(mm.Fingerprint); fp != "" {
			byFP[fp] = append(byFP[fp], key)
		}
		if ip := normalizeExitIP(mm.State.ExitIP); ip != "" {
			byExit[ip] = append(byExit[ip], key)
		}
	}
	others := func(group []string, self string) []string {
		// ⚠️ 必须先判空（v0.31.26 线上 500 的成因）：老 agent 不上报指纹，
		// `byFP[""]` 就是 nil，而 `make([]string, 0, len-1)` 在 len=0 时
		// **cap = -1 → panic（makeslice: cap out of range）**，
		// 结果是整页线路池 500 —— 且本地测试全绿，因为用例里都设了指纹。
		if len(group) <= 1 {
			return nil
		}
		list := make([]string, 0, len(group)-1)
		for _, k := range group {
			if k != self {
				list = append(list, k)
			}
		}
		sort.Strings(list)
		return list
	}

	for key, mm := range all {
		url := egressHomeURL(mm.Ports.TunnelPort)
		fp := strings.TrimSpace(mm.Fingerprint)
		ip := normalizeExitIP(mm.State.ExitIP)
		tunnelIP := proxyLastIP[url]
		mismatch := false
		if ip != "" && tunnelIP != "" && ip != tunnelIP {
			mismatch = true
		}
		out[url] = map[string]any{
			"id":               mm.ID,
			"key":              key,
			"hostname":         mm.Hostname,
			"owner_user_id":    mm.OwnerUserID,
			"online":           mm.online(now),
			"tunnel_reachable": egressTunnelReachable(mm.Ports.TunnelPort),
			// exit_ip 归一化（用于比较/分组），exit_ip_raw 保留原文（用于人眼核对地区）。
			"exit_ip":      ip,
			"exit_ip_raw":  mm.State.ExitIP,
			"exit_ip_note": mm.State.ExitIPNote,
			"fingerprint":  fp,
			"local_ip":     mm.State.LocalIP,
			// duplicate_fingerprint 与 self 同指纹的**其它**机器 key。
			"duplicate_fingerprint": others(byFP[fp], key),
			// same_exit 出口 IP 相同、但指纹不同的机器 key（两台机器同一个出口）。
			"same_exit": others(byExit[ip], key),
			"last_at":   mm.StateAt,
			// last_poll_at / last_seen_at / offline_sec（v0.31.49）：页面据此说清
			// "它到底是关机了，还是只是还没上报"。同主机名的两条记录摆在一起时，
			// 这一列是用户唯一能分辨"该点哪张卡"的依据。
			"last_poll_at": mm.LastPollAt,
			"last_seen_at": mm.lastSeenAt(),
			"offline_sec": func() int64 {
				if seen := mm.lastSeenAt(); seen > 0 && now > seen {
					return now - seen
				}
				return 0
			}(),
			"is_default": mm.ID == egressDefaultMachine,
			// tunnel_exit_ip 云端经隧道实测的出口；exit_mismatch 两者不一致。
			"tunnel_exit_ip": tunnelIP,
			"exit_mismatch":  mismatch,
			// alt_ids 同一台机器上另外几份 agent 自报的 id（一台电脑装了多份）。
			"alt_ids": append([]string(nil), mm.AltIDs...),
			"port":    mm.Ports.TunnelPort,
			// 承重机判定（v0.31.52）：这条线路当前挂着几个号。
			// carrying=true 才是"真正在承重"的那台；另一台即便在线也只是待命。
			"bound_accounts": boundByURL[egressURLKey(url)],
			"carrying":       boundByURL[egressURLKey(url)] > 0,
		}
	}
	return out
}

// egressPickMachine 从请求体里取目标机器；缺省取"最近上报的那台"。
//
// 为什么要能指定：不指定就只能"谁最近说过话就命令谁"，两台机器并存时
// 用户在 A 机页面上点一下，可能恰好发给了 B 机 —— 而用户**看不出**这件事。
func (a *app) egressPickMachine(r *http.Request, m map[string]*egressMachine, scope egressScope) string {
	var body struct {
		Machine string `json:"machine"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if id := strings.TrimSpace(body.Machine); id != "" {
		return id
	}
	// 缺省取"最近上报的那台" —— **只在该视角可见的机器里挑**（v0.31.24）：
	// 否则管理员的机器会成为某个用户"没说指定哪台"时的默认目标。
	best, bestAt := "", int64(-1)
	for key, mm := range m {
		if !scope.seesMachine(mm) {
			continue
		}
		if mm.StateAt > bestAt {
			best, bestAt = key, mm.StateAt
		}
	}
	return best
}

// egressOfflineGraceSec 下发 start 前容忍的"沉默"时长（v0.31.49）。
//
// 2 分钟 ≈ 24 个心跳：足够过滤网络抖动、系统休眠唤醒，又能挡住"这台其实早就关了"。
const egressOfflineGraceSec = 120

// egressOfflineHint 目标机器是否已经不在了；在则返回空串。
//
// 为什么要在下发前判（而不是让前端等超时）：agent 是**拉取式**的，指令只是写进
// 一张表，机器不在就永远没人取。用户看到的"转 90 秒然后超时"和"拉起坏了"没有区别，
// 而真实原因往往是"你选中的是另一台已离线的机器"（同一台电脑换网络会留下两条记录）。
func egressOfflineHint(mm *egressMachine, now int64) (string, int64) {
	if mm == nil {
		return "", 0
	}
	seen := mm.lastSeenAt()
	if seen <= 0 {
		// 从未上报也未 poll 过 —— 没有证据说它离线，放行（宁可让前端等）。
		return "", 0
	}
	off := now - seen
	if off <= egressOfflineGraceSec {
		return "", 0
	}
	h := strings.TrimSpace(mm.Hostname)
	if h == "" {
		h = mm.ID
	}
	return fmt.Sprintf("「%s」已经 %s 没有任何心跳（agent 未运行，或那台电脑没联网）——"+
		"请在**那台**电脑上启动 agent 后再点「一键拉起」", h, egressAgoText(off)), off
}

// egressAgoText 把秒数说成人话（只到"分/小时/天"，不追求精确）。
func egressAgoText(sec int64) string {
	if sec < 60 {
		return fmt.Sprintf("%d 秒", sec)
	}
	mins := sec / 60
	if mins < 60 {
		return fmt.Sprintf("%d 分钟", mins)
	}
	h, m := mins/60, mins%60
	if h < 24 {
		if m == 0 {
			return fmt.Sprintf("%d 小时", h)
		}
		return fmt.Sprintf("%d 小时 %d 分", h, m)
	}
	return fmt.Sprintf("%d 天", h/24)
}

// handleEgressHomePull POST /api/egress/home/pull body {"machine":"<key>"}
//
// 下发 start 指令。**不等结果就返回**：真正的进度由前端轮询 GET 得到
// —— 一个 HTTP 请求里等 30 秒会同时考验反向代理超时、浏览器超时和用户耐心，
// 而这件事实在没什么需要"等"的（agent 是独立进程，会自己完成）。
//
// v0.31.24：普通用户可执行，但目标只能是**自己的机器**；**不再凭空登记机器**
// —— 一台永远不上线的机器只会让列表变脏，而"没有可选机器"的正确指引是
// "先去目标电脑把 agent 跑起来"（页面下方就有那行命令）。
func (a *app) handleEgressHomePull(w http.ResponseWriter, r *http.Request, scope egressScope) {
	m := a.machinesLoad()
	id := a.egressPickMachine(r, m, scope)
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "还没有已接入的机器：请先在目标电脑上运行 agent 命令（本页下方有）",
		})
		return
	}
	mm, ok := a.egressMachineFor(m, id, scope)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "该机器尚未登记，或不属于你"})
		return
	}
	// 目标机器明显已经不在线 → 立刻说清楚，不要让前端干等 90 秒（v0.31.49）。
	//
	// 现场：用户在同一台笔记本上换了网络，服务端因此多出一条"同主机名"的旧记录；
	// 他在下拉框里选中旧那条点「一键拉起」，指令写进一台早已下线的记录，
	// 页面转 90 秒后只给一句"等待超时" —— 看起来就是"拉起功能坏了"。
	// 判定用 120 秒的宽限（约 24 个心跳），只为挡住"真的关机/agent 没跑"，
	// 网络抖动（几秒）仍然照常下发。
	if msg, off := egressOfflineHint(mm, time.Now().Unix()); msg != "" {
		log.Printf("INFO: [egress] 拒绝向离线机器 machine=%s 下发 start（离线 %ds）", mm.ID, off)
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":       msg,
			"offline_sec": off,
			"machine":     mm.ID,
			"hint":        "如果这台就是你正在用的电脑，说明它换了网络、被登记成了两条线路；请选“最近在线”的那条，或直接在目标电脑上重新运行 agent",
		})
		return
	}
	// 去抖（v0.31.25）：连点不该让 agent 把隧道拆了又建。
	key := egressMachineKey(scope.hostOwner(), mm.ID)
	if egressPullDeduped(key) {
		log.Printf("INFO: [egress] machine=%s 在去抖窗口内重复下发 start，已合并（不刷新指令）", mm.ID)
		writeJSON(w, http.StatusOK, map[string]any{
			"issued": true, "action": "start", "machine": mm.ID, "deduped": true,
			"view": a.buildEgressHomeView(r, scope),
		})
		return
	}
	mm.Cmd, mm.CmdAt = "start", time.Now().Unix()
	if err := a.machinesSave(m); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	log.Printf("INFO: [egress] 已向 machine=%s（scope=%s）下发 start 指令（等待 agent 拉起本机出口代理）",
		mm.ID, scope.label())
	writeJSON(w, http.StatusOK, map[string]any{
		"issued": true, "action": "start", "machine": mm.ID,
		"view": a.buildEgressHomeView(r, scope),
	})
}

// handleEgressHomeRelease POST /api/egress/home/release body {"machine":"<key>"}
//
// 下发 stop。**刻意不自动清掉已应用的出口配置**：停代理只是"不再从家宽出"，
// 而"账号该走哪条线"是另一个决定（可能已经有人手工调过）。要改回去，
// 用管理后台的「代理」页显式操作 —— 一键停代理顺手改几十个账号的出口，
// 是那种"点一下才发现改错了"的操作。
func (a *app) handleEgressHomeRelease(w http.ResponseWriter, r *http.Request, scope egressScope) {
	m := a.machinesLoad()
	id := a.egressPickMachine(r, m, scope)
	mm, ok := a.egressMachineFor(m, id, scope)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "该机器尚未登记，或不属于你"})
		return
	}
	mm.Cmd, mm.CmdAt = "stop", time.Now().Unix()
	if err := a.machinesSave(m); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"issued": true, "action": "stop", "machine": mm.ID})
}

// handleEgressHomeApply POST /api/egress/home/apply body {"machine":"<key>"}
//
// 显式应用。前端在"就绪"后调用它（而不是让 GET 顺手写）——
// 读接口带副作用是排障时最讨厌的东西之一。
//
// v0.31.24：普通用户可执行，但范围严格是"他自己的机器 + 他自己名下的号"。
func (a *app) handleEgressHomeApply(w http.ResponseWriter, r *http.Request, scope egressScope) {
	m := a.machinesLoad()
	id := a.egressPickMachine(r, m, scope)
	mm, ok := a.egressMachineFor(m, id, scope)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "该机器尚未登记（本机 agent 还没连上来过），或不属于你",
		})
		return
	}
	// 单飞（v0.31.25）：应用会写几十个账号的出口，两个请求同时跑会互相覆盖结果，
	// 而用户看到的只是"绑定 N 个"里的 N 跳来跳去。
	//
	// ⚠️ **放在隧道检查之前**：若是先判隧道，连点两次的第二下会得到
	// "隧道尚未就绪" —— 那句话把人引去查网络，而真实原因是"上一次还在跑"。
	// 报错要指向真正的原因，顺序就得让更本质的状态先说话。
	applyKey := egressMachineKey(scope.hostOwner(), mm.ID)
	release, busy := egressApplyBegin(applyKey)
	if busy {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "该机器的配置正在应用中，请稍候几秒再试", "busy": true,
		})
		return
	}
	defer release()
	if !egressTunnelReachable(mm.Ports.TunnelPort) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": fmt.Sprintf("隧道尚未就绪（云端连不上本机代理端口 %d），请先完成「一键拉起」", mm.Ports.TunnelPort),
		})
		return
	}
	// v0.31.53：不再接受 rebind_all —— "应用"不再改任何账号的出口。
	// 老客户端仍可能带这个字段，JSON 解码会忽略未知字段（不报错），
	// 但也不会产生任何绑定行为：分配统一走管理后台。
	res, err := a.applyHomeEgress(mm.Ports.TunnelPort, scope)
	if err != nil {
		// 线路不通 → 409 而不是 500：这是"当前状态不允许"，不是服务故障。
		// 文案直接给出下一步（用户要的是"为什么刚拉起就全挂"，不是 HTTP 语义）。
		if errors.Is(err, errEgressLineManuallyDisabled) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": "这条出口已被管理员停用：可以确认它能出网，但不会启用、也不会绑定任何账号。" +
					"如需恢复，请让管理员在「代理」页解锁后再点「一键拉起」",
				"line_manually_disabled": true,
			})
			return
		}
		if errors.Is(err, errEgressNotUsable) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": err.Error() + "。未改动任何账号的出口。" +
					"请检查本机 agent 窗口：代理进程是否在跑、是否被系统代理劫持（VPN/Clash），" +
					"然后重新点「一键拉起」",
				"unusable": true,
			})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"applied": res, "machine": mm.ID, "view": a.buildEgressHomeView(r, scope),
	})
}

// egressHomeApplyResult 应用结果（逐项如实回报）。
//
// v0.31.53 起这里**不再有"绑定了几个号"的字段** —— 出口分配已收敛到管理后台，
// 一键拉起只负责把平台线路立起来（注册 + 设默认）。保留一堆恒为 0 的字段会让
// 前端/脚本以为"应用时会顺带绑号"，那正是这次要删掉的旧逻辑。
type egressHomeApplyResult struct {
	ProxyID   string `json:"proxy_id"`
	ProxyURL  string `json:"proxy_url"`
	DefaultOK bool   `json:"default_ok"`
	Probed    bool   `json:"probed"`
	ExitIP    string `json:"exit_ip,omitempty"`
	// Warnings 本次**刻意没做**的事（v0.31.35）。例如：这条 URL 已被某个用户认领，
	// 平台视角就不再把它设成全局默认。
	//
	// 为什么不用 error：线路注册本身是成功的（隧道确实通了），
	// 只是"不能拿它当平台出口"—— 用错误会让调用方以为整件事失败。
	Warnings []string `json:"warnings,omitempty"`
}

// errEgressNotUsable 隧道端口可连、但线路实际出不了网。
//
// 用哨兵错误而不是匹配错误文案：本仓踩过「状态码跟着文案改」的坑
// （改一句话，403 变 400）。调用方用 errors.Is 判，文案随便改。
var errEgressNotUsable = errors.New("egress line not usable")

// errEgressLineManuallyDisabled 线路被管理员**人工停用**（v0.31.50）。
//
// 语义是"这个决定属于管理员"，不是"线路坏了"：用户重新拉起代理不得把它拉活，
// 管理员的强制越界也不放开这一条（见 egressLineAllows 第 1 条）。
var errEgressLineManuallyDisabled = errors.New("egress line manually disabled")

// notUsableError 带**用户可读文案**的「线路不可用」错误。
//
// 为什么不直接 fmt.Errorf("%w：...", errEgressNotUsable)：`%w` 会把哨兵的
// Error() 原文拼在最前面，于是用户在前端看到的是
// "egress line not usable：隧道端口可连……" —— 一句英文技术话混在中文提示里。
// 这个类型对 errors.Is(errEgressNotUsable) 回答"是"，但自己的文案只有中文。
type notUsableError struct{ msg string }

func (e *notUsableError) Error() string { return e.msg }

// Is 让 errors.Is(err, errEgressNotUsable) 成立，同时不影响 Error() 的文案。
func (e *notUsableError) Is(target error) bool { return target == errEgressNotUsable }

// verifyLineUsable 经这条线路做一次**真实出网**验证，并把结果落库。
//
// 为什么必须有这一步（v0.31.47，2026-10-19 现场）：
//
//	「一键拉起」的就绪判据是 `agent 在线 + 隧道端口可连`。但端口可连只证明
//	**sshd 在 listen**，不证明隧道那头的 wbproxy 真能把请求转出去 ——
//	代理进程可能还没 listen、可能走了系统代理、也可能上游根本不通。
//	于是"拉起 → 立刻自动应用 → 把该用户所有号绑到这条线"会把一批**健康账号**
//	绑到一条**出不去的线路**上：用户看到的就是"刚拉起，全部失败"。
//
// 判据用真实请求而不是 TCP 探测：这正是 `egress.Probe` 的口径（走代理抓出口 IP）。
func (a *app) verifyLineUsable(url string) (string, error) {
	pr := egress.Probe(url)
	if pr.EndpointUnreachable {
		return "", &notUsableError{msg: "服务端自身出网受限，无法验证这条线路的出口（" + pr.Error + "）"}
	}
	// 落库：页面立刻能看到出口 IP / 状态，不必等下一轮 5 分钟定时探活。
	// 这里只用 RecordEgressCheck（不改 enabled），启停仍由探活那一套统一负责。
	if list, err := a.store.ListEgressProxies(); err == nil {
		for _, p := range list {
			if strings.EqualFold(strings.TrimSpace(p.URL), strings.TrimSpace(url)) {
				_ = a.store.RecordEgressCheck(p.ID, pr.OK, pr.IP, pr.Error)
				break
			}
		}
	}
	if !pr.OK {
		return "", &notUsableError{msg: "隧道端口可连，但经它出不了网（" + strings.TrimSpace(pr.Error) + "）"}
	}
	return pr.IP, nil
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
//
// ⚠️ scope（v0.31.24）：**用户视角不做第 2 步**，且第 1、3 步都收窄到自己。
// 理由：全局默认出口是平台级决定（它同时决定所有平台号与所有"跟随默认"的账号
// 从哪出去），而用户提供的代理是不可信第三方 —— 它只应承载它主人的号。
// 用户拉起的线路仍会进线路池（带 owner），管理员据此可显式分配给别人。
func (a *app) applyHomeEgress(tunnelPort int, scope egressScope) (egressHomeApplyResult, error) {
	url := egressHomeURL(tunnelPort)
	var res egressHomeApplyResult
	res.ProxyURL = url

	// 0) **先验证这条线路真能出网，再谈绑定**（v0.31.47）。
	//
	// 放在这里而不是只放调用方：这是"绑号"这件事唯一的入口，
	// 验证写在入口里才是结构性的（以后新增调用点不会忘）。失败时**一个账号都不动**。
	verify := a.verifyLineUsable
	if a.egressVerify != nil {
		verify = a.egressVerify
	}
	ip, err := verify(url)
	if err != nil {
		return res, err
	}
	res.Probed, res.ExitIP = true, ip

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
	// v0.31.53：每用户线路配额随用户端代理一起下线（唯一入口已是管理端）。
	// 这里不再判配额 —— 平台机器数量由运维控制，用一条"按用户计数"的规则去管它
	// 只会给出误导性的报错。
	if hit == nil {
		name, region := "本地住宅出口", "本机所在网络"
		hit = &EgressProxy{
			ID:        newEgressProxyID(url),
			Name:      name,
			URL:       url,
			Region:    region,
			Note:      "由「一键拉起」自动注册（本机代理 + 反向隧道）",
			Enabled:   true,
			CreatedAt: now,
		}
	}
	hit.UpdatedAt = now
	// 拉起即启用 —— **但不许把管理员的"人工停用"拉活**（v0.31.50，R3）。
	// 这个坑 v0.31.34 踩过一次（无条件 Enabled=true 覆盖别人停用），当时只修了
	// 用户线路的登记路径；这里是同一个决定的另一个入口。
	if hit.ManualDisabled {
		res.Warnings = append(res.Warnings, "这条出口已被管理员停用：已确认能出网，但不会启用、也不会绑定任何账号（需管理员先在后台解锁）")
		log.Printf("WARN: [egress] 拒绝启用人工停用的线路 %s（owner=#%d scope=%s）",
			url, hit.OwnerUserID, scope.label())
		if err := a.store.UpsertEgressProxy(hit); err != nil {
			return res, err
		}
		res.ProxyID = hit.ID
		return res, errEgressLineManuallyDisabled
	}
	hit.Enabled = true // 拉起即启用（否则"通了但被停用"仍是个坑）
	// 刚做过真实出网验证，结果直接写进线路记录（v0.31.47）：
	// 否则页面会显示"未探活"，而运维刚点完"拉起"最想看到的就是出口 IP。
	// 同时清掉 AutoDisabled —— 这条线刚刚被证明能用，没有理由还留着自动停用标记。
	hit.LastOK, hit.LastIP, hit.FailStreak = true, ip, 0
	hit.AutoDisabled = false
	if err := a.store.UpsertEgressProxy(hit); err != nil {
		return res, err
	}
	res.ProxyID = hit.ID

	// 归属守卫（v0.31.35 / v0.31.53 收窄）：平台视角拉到一条**已被用户认领**的 URL 时，
	// 绝不能再把它当平台出口用 —— 否则"全局默认 = 某个用户的家庭宽带"。
	// 这种情形只可能来自端口/URL 撞车（用户段与平台段本该不同），
	// 但代码不能依赖"本该"，否则一次端口迁移就会触发。
	userOwnedLine := hit.OwnerUserID > 0
	if userOwnedLine {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"这条线路（%s）已由用户 #%d 认领：已登记，但**不会**设为全局默认出口。"+
				"平台出口请用平台自己的机器（端口 11080-11089）", url, hit.OwnerUserID))
		log.Printf("WARN: [egress] 平台视角命中用户线路 %s（owner=#%d）：跳过默认出口",
			url, hit.OwnerUserID)
	}

	// 2) 全局默认出口（只有平台自己的线路才配得上）
	if !userOwnedLine {
		if err := a.store.SetKV(kvEgressDefault, url); err != nil {
			return res, err
		}
		res.DefaultOK = true
	}

	// 3) v0.31.53：**不再绑定任何账号**。
	//
	// 出口分配已统一收敛到管理后台（POST /api/admin/egress/assign，带唯一闸门与
	// 强制越界审计）；"一键拉起 / 应用"只负责把**平台线路本身**立起来
	// （注册线路 + 设全局默认出口）。
	//
	// 为什么删掉而不是留着：自动绑号是**隐式批量改出口** —— 点一下可能把几十个号
	// 从一条线路搬到另一条，页面上只显示"绑定成功 N 个"，既没有理由也没有台账。
	// 它也是唯一闸门（egressLineAllows）之外的额外写入点，规则同时存在于两处
	// 就一定会漂移（v0.31.41 的血泪教训）。
	_ = a.store.SetKV(kvEgressAppliedAt, fmt.Sprintf("%d", now))
	// 池以库为准重建：全局默认出口刚可能变化，选号映射要跟着变。
	_ = a.proxy.SyncAccounts()
	log.Printf("INFO: [egress] 平台线路已应用 proxy=%s url=%s default=%v（不改任何账号出口）",
		hit.ID, url, res.DefaultOK)
	return res, nil
}

// userNameByID 用户名（缺失时退回 "用户#<id>"）—— 仅用于给人读的线路名/日志。
func (a *app) userNameByID(uid int64) string {
	if uid <= 0 {
		return "平台"
	}
	if u, err := a.store.GetUserByID(uid); err == nil && u != nil {
		if n := strings.TrimSpace(u.Username); n != "" {
			return n
		}
	}
	return fmt.Sprintf("用户#%d", uid)
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
