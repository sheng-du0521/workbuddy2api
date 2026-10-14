// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"workbuddy2api/internal/egress"
)

// ======================================================================
// 导入防关联门禁（v0.25.0）
// ======================================================================
//
// 背景（用户提出的两条要求）：
//
//	① 导入前重置路由换出口 IP，防止新号与旧号被关联；
//	② 一次导入不超过 3 个账号，每次导入前重新换 IP。
//
// 两条指向同一个事实：**批量导入是一类可被上游聚类的行为**。同一出口 IP 上
// 短时间内冒出一批新号（且它们的初登、签到、首轮对话都从同一个 IP 出去），
// 是比「多个号长期共用一条宽带」更强的信号 —— 后者可能是自然用户，前者不像。
//
// 本文件把这件事从「靠运维记得去重启设备」变成**系统强制的门禁**：
//
//	解析 → 算新增数 → 超上限？拒 → 与上次导入同一个 IP？拒（或自动换）→ 才写库
//
// ## 必须说清的三条边界（别让这道门禁显得比实际更强）
//
//  1. **换 IP 这个动作本程序做不了**（没有光猫/路由的凭据）。所以它只有两种模式：
//     - 手动（默认）：门禁拒绝 → 你去重启设备 → 回来点「已重置」→ 程序**实测** IP
//     确实变了才放行。**永远不信任"我重启过了"这句话本身，只信任实测。**
//     - 命令（可选）：你在设置里填一条命令（智能插座 HTTP API / 路由器 SSH 等），
//     程序执行它并等待出口 IP 变化。凭据始终在你手里，不经过本程序。
//  2. **本机只有一个公网 IP**（典型家庭宽带拓扑：本机 → 二级路由 → 光猫持公网 IP → ISP）。
//     所以"换 IP"= 换掉**这唯一一个**出口，不是拿到多条线路；必须重启**光猫**才有效，
//     重启二级路由无效（`tracert` 第一跳是二级路由、第二跳才是光猫，公网地址在光猫上）。
//  3. **门禁只管「导入那一刻」**。它让每一批新号的初登 IP 不同，但**不能**保证这些号
//     此后的所有出站长期分属不同 IP —— 那需要真正的多出口（出口线路池），是另一件事。
//     诚实地说：这道门禁降低的是「批量导入」这一瞬间的聚类特征，不是全部关联信号。
//
// ## kv 开关（全部可在「设置」页改，也可直接改库）
//
//	import_ip_gate      "1"（默认）开启门禁；"0" 关闭
//	import_max_new      "3"（默认）单次**新增**账号数上限；"0" 不限
//	import_last_ip      上次成功导入时的出口 IP（门禁判据，程序自写）
//	import_last_at      上次成功导入时间（程序自写）
//	import_batches      最近 50 批的台账 JSON（程序自写，供事后审计）
//	ip_reset_cmd        可选的换 IP 命令；空 = 手动模式
//	import_reset_wait   等待 IP 变化的秒数上限（默认 90）

const (
	kvImportIPGate     = "import_ip_gate"
	kvImportMaxNew     = "import_max_new"
	kvImportLastIP     = "import_last_ip"
	kvImportLastAt     = "import_last_at"
	kvImportBatches    = "import_batches"
	kvIPResetCmd       = "ip_reset_cmd"
	kvImportResetWait  = "import_reset_wait"
	importMaxNewDef    = 3
	importResetWaitDef = 90
	importIPPollEvery  = 3 * time.Second
	// importBatchesKeep 台账保留批次数。50 批 × 每批几十字节，量级完全可忽略，
	// 但足够回答「这个号是哪一批、从哪个 IP 进来的」。
	importBatchesKeep = 50
	// ipResetCmdTimeout 换 IP 命令本身的执行上限（命令通常只是发一个 HTTP 请求）。
	ipResetCmdTimeout = 30 * time.Second
)

// probeExitIP 本机出口 IP 探测。
//
// 抽成变量是为了**可测**：门禁的判定全建立在"当前出口 IP 是什么"之上，
// 若测试直接调 egress.Probe，每个用例都要真连外网 —— 又慢又 flaky，
// 最后没人愿意为门禁补测试，而这恰恰是一条会拦人的路径，必须能测。
var probeExitIP = func() egress.ProbeResult { return egress.Probe("") }

// importBatchRecord 一批导入的台账记录。
type importBatchRecord struct {
	At     int64    `json:"at"`
	IP     string   `json:"ip"`     // 本批导入时的出口 IP（空 = 未能探测到）
	Source string   `json:"source"` // json / file
	New    int      `json:"new"`    // 本批新增数
	Upd    int      `json:"upd"`    // 本批更新数
	UIDs   []string `json:"uids"`
}

// importGuardVerdict 门禁判定结果。**同时用于拒绝与放行** ——
// 放行时前端也要拿到「当前 IP / 上次 IP / 新增数」来显示，而不是只看到一个 200。
type importGuardVerdict struct {
	OK         bool   `json:"ok"`
	Enabled    bool   `json:"enabled"`    // 门禁是否开启
	NewCount   int    `json:"new_count"`  // 本次将新增的账号数
	MaxNew     int    `json:"max_new"`    // 生效的上限（0 = 不限）
	ExitIP     string `json:"exit_ip"`    // 当前本机出口 IP（探测失败为空）
	LastIP     string `json:"last_ip"`    // 上次导入时的出口 IP
	LastAt     int64  `json:"last_at"`    // 上次导入时间
	IPChanged  bool   `json:"ip_changed"` // 当前 IP 是否已不同于上次导入
	Unverified bool   `json:"unverified"` // 探测失败：无法判定（见 fail-open 说明）
	NeedIP     bool   `json:"need_ip"`    // 是否需要先换 IP
	AutoReset  bool   `json:"auto_reset"` // 是否配置了自动换 IP 命令
	Reason     string `json:"reason,omitempty"`
	Hint       string `json:"hint,omitempty"`
}

// ---- kv 读取（带默认值）----

func (a *app) importIPGateOn() bool {
	v, _ := a.store.GetKV(kvImportIPGate)
	return strings.TrimSpace(v) != "0"
}

func (a *app) importMaxNew() int {
	v, _ := a.store.GetKV(kvImportMaxNew)
	v = strings.TrimSpace(v)
	if v == "" {
		return importMaxNewDef
	}
	n := 0
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n < 0 {
		return importMaxNewDef
	}
	return n
}

func (a *app) ipResetCmd() string {
	v, _ := a.store.GetKV(kvIPResetCmd)
	return strings.TrimSpace(v)
}

func (a *app) importResetWait() time.Duration {
	v, _ := a.store.GetKV(kvImportResetWait)
	v = strings.TrimSpace(v)
	if v == "" {
		return importResetWaitDef * time.Second
	}
	n := 0
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n <= 0 || n > 600 {
		return importResetWaitDef * time.Second
	}
	return time.Duration(n) * time.Second
}

// countNewAccounts 算出本次导入里**新增**（uid 不在库）的账号数。
//
// 为什么必须只算新增：更新已存在的号只是**刷新 token** —— 它不引入新的账号身份，
// 自然也不产生"新号批量出现"这一信号。若把更新也算进额度，日常「重导一遍全量备份」
// 这种无害操作会被门禁挡住，最后运维只能把门禁关掉，反而失去保护。
// （uid 重复出现在同一份 payload 里的，只算一次。）
func (a *app) countNewAccounts(list []desktopAccount) int {
	existing := map[string]bool{}
	if all, err := a.store.ListAccounts(); err == nil {
		for _, acc := range all {
			existing[acc.UID] = true
		}
	}
	seen := map[string]bool{}
	n := 0
	for _, d := range list {
		acc := d.toAccount()
		uid := strings.TrimSpace(acc.UID)
		if uid == "" || seen[uid] || existing[uid] {
			continue
		}
		// 与 importDesktopAccounts 的校验对齐：凭证全空的条目不落库，
		// 因此也不该占用额度（否则"5 条里 2 条是垃圾"会被误判成超限）。
		if strings.TrimSpace(acc.AccessToken) == "" && strings.TrimSpace(acc.RefreshToken) == "" {
			continue
		}
		seen[uid] = true
		n++
	}
	return n
}

// checkImportGuard 导入前的门禁判定（**只判定，不写任何东西**）。
//
// 判定顺序刻意如此：先看"这本来是不是就该拦住"（数量），再看"IP 是否需要换"。
// 一个 10 个新增号的导入，即使换了 IP 也不该放行 —— 先报数量错误更直接。
//
// expectNew 的用处：预检端点（/import/ip-status）手里**没有**待导入列表，但用户马上
// 就要粘一份进来，所以它必须按"会有新增"来判 —— 否则预检会说"无需换 IP"，
// 粘完又被拒（正是本文件反复强调要避免的"粘完才被拒"）。
func (a *app) checkImportGuard(list []desktopAccount, expectNew bool) importGuardVerdict {
	v := importGuardVerdict{OK: true, Enabled: a.importIPGateOn()}
	v.NewCount = a.countNewAccounts(list)
	v.MaxNew = a.importMaxNew()
	if last, _ := a.store.GetKV(kvImportLastIP); strings.TrimSpace(last) != "" {
		v.LastIP = strings.TrimSpace(last)
	}
	if at, _ := a.store.GetKV(kvImportLastAt); strings.TrimSpace(at) != "" {
		fmt.Sscanf(strings.TrimSpace(at), "%d", &v.LastAt)
	}

	// —— ① 单批新增上限 ——
	// 刻意**不**受 import_ip_gate 开关控制：「一次别超过 3 个」是独立的纪律，
	// 关掉 IP 门禁（比如换了不能重拨的宽带）不该顺带把数量限制也放掉。
	if v.MaxNew > 0 && v.NewCount > v.MaxNew {
		v.OK = false
		v.Reason = fmt.Sprintf("本次将新增 %d 个账号，超过单批上限 %d", v.NewCount, v.MaxNew)
		v.Hint = fmt.Sprintf("请拆成每批不超过 %d 个陆续导入，且每批之间换一次出口 IP（见「换 IP」）。"+
			"分批导入的意义就是让每批新号的初登 IP 不同；一次性导入 N 个是最容易被聚类的形态。", v.MaxNew)
		return v
	}

	// —— ② IP 门禁 ——
	if !a.importIPGateOn() {
		v.Hint = "IP 门禁已关闭（import_ip_gate=0）；仅数量上限仍然生效。"
		return v
	}
	probe := probeExitIP()
	v.ExitIP = probe.IP
	if !probe.OK || v.ExitIP == "" {
		// fail-open 是刻意的：导入本身**不产生任何出站**，探测失败就拒绝会让
		// 「第三方回显端点挂了」变成「导入用不了」。这里放行但如实标注"未能判定"，
		// 让运维知道这次没验上，而不是以为验过了。
		v.Unverified = true
		v.OK = true
		v.Hint = "未能探测到本机出口 IP（" + probe.Error + "），本次未校验 IP 是否已更换。"
		return v
	}
	if v.LastIP == "" {
		// 首次导入（库里还没有历史 IP）：没有可比对象，放行。
		v.IPChanged = true
		return v
	}
	v.IPChanged = v.ExitIP != v.LastIP
	if v.IPChanged {
		return v
	}
	// 本次**没有任何新增**（全是已存在账号的 token 刷新）→ 不产生新的账号身份，
	// 也就没有"新号批量出现在同一 IP 上"这一信号，不该因为 IP 相同而拦人。
	//
	// 这条不是宽容，是**精确**：门禁的目的是切断新号的关联，而不是禁止一切重复导入。
	// 缺了它，「重导一遍全量备份」会被挡住 —— 那种挫败感会直接导致运维把门禁整个关掉，
	// 保护反而消失（这比偶尔漏拦一批更糟）。
	if v.NewCount == 0 && !expectNew {
		v.Hint = "本次没有新增账号（只更新已存在的号），不产生新的账号身份，无需换 IP。"
		return v
	}

	// 与上次同一个 IP → 需要换 IP。
	v.NeedIP = true
	v.AutoReset = a.ipResetCmd() != ""
	if v.AutoReset {
		v.Hint = "出口 IP 与上次导入相同，已配置自动换 IP 命令：将执行并等待 IP 变化。"
		return v // OK 仍为 true：调用方会先执行换 IP 再复检
	}
	v.OK = false
	v.Reason = "出口 IP 与上次导入相同（" + v.ExitIP + "）"
	v.Hint = "请先重置光猫换掉出口 IP（注意：公网 IP 在光猫上，" +
		"重启二级路由无效），然后在弹窗里点「已重置，重新检测」。\n" +
		"也可以在「设置 → 导入防关联」里填一条换 IP 命令（智能插座 API / 路由器接口），之后可全自动。"
	return v
}

// resetExitIPIfNeeded 执行换 IP 并等待出口 IP 真的变化。
//
// 返回 (新 IP, 是否已变化)。**只在配置了 ip_reset_cmd 时才有动作**：
// 没有命令就返回 (before,false)，由前端引导人工重启 —— 本程序永远不会去猜
// 用户的光猫密码，也不会尝试登录他的路由设备。
func (a *app) resetExitIPIfNeeded(before string) (string, bool) {
	cmdStr := a.ipResetCmd()
	if cmdStr == "" {
		return before, false
	}
	log.Printf("INFO: [import] 执行换 IP 命令：%s", cmdStr)
	if err := a.runIPResetCmd(cmdStr); err != nil {
		// 命令失败**不静默**：它意味着"以为换了、其实没换"，后续所有判断都会失真。
		log.Printf("WARN: [import] 换 IP 命令执行失败：%v", err)
		return before, false
	}
	return a.waitExitIPChange(before, a.importResetWait())
}

// runIPResetCmd 执行换 IP 命令。
//
// 安全性说明（重要，勿放宽）：命令来自 kv，**只能由管理员写入**（设置接口走 adminGuard）。
// 也就是说它的信任级别等于管理密钥本身 —— 能改这个 kv 的人本来就能改整个网关的配置。
// 刻意**不**接受来自请求体的命令（那会让"能调导入接口"变成"能执行任意命令"）。
func (a *app) runIPResetCmd(cmdStr string) error {
	var c *exec.Cmd
	if runtime.GOOS == "windows" {
		c = exec.Command("cmd", "/C", cmdStr)
	} else {
		c = exec.Command("sh", "-c", cmdStr)
	}
	done := make(chan error, 1)
	if err := c.Start(); err != nil {
		return err
	}
	go func() { done <- c.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(ipResetCmdTimeout):
		if c.Process != nil {
			_ = c.Process.Kill()
		}
		return fmt.Errorf("命令超过 %s 未返回", ipResetCmdTimeout)
	}
}

// waitExitIPChange 轮询等待出口 IP 变为 before 之外的值。
//
// 为什么必须轮询而不是"执行完命令就当换了"：重启光猫后重拨需要时间（实测十几秒），
// 且运营商的动态 IP 池有时会分配回**同一个**地址。不看实测就会写出
// "以为换了、其实没换"的假记录 —— 那比不做门禁更糟（它给了虚假的安全感）。
func (a *app) waitExitIPChange(before string, timeout time.Duration) (string, bool) {
	deadline := time.Now().Add(timeout)
	last := before
	for time.Now().Before(deadline) {
		time.Sleep(importIPPollEvery)
		r := probeExitIP()
		if r.OK && r.IP != "" {
			last = r.IP
			if r.IP != before {
				return r.IP, true
			}
		}
	}
	return last, false
}

// recordImportBatch 落一批导入台账 + 更新门禁判据（**导入成功后**才调）。
func (a *app) recordImportBatch(source, ip string, newN, updN int, uids []string) {
	if len(uids) > 50 {
		uids = uids[:50]
	}
	rec := importBatchRecord{
		At:     time.Now().Unix(),
		IP:     ip,
		Source: source,
		New:    newN,
		Upd:    updN,
		UIDs:   uids,
	}
	var batches []importBatchRecord
	if raw, _ := a.store.GetKV(kvImportBatches); strings.TrimSpace(raw) != "" {
		_ = json.Unmarshal([]byte(raw), &batches)
	}
	batches = append([]importBatchRecord{rec}, batches...) // 新的在前
	if len(batches) > importBatchesKeep {
		batches = batches[:importBatchesKeep]
	}
	if blob, err := json.Marshal(batches); err == nil {
		_ = a.store.SetKV(kvImportBatches, string(blob))
	}
	if ip != "" {
		_ = a.store.SetKV(kvImportLastIP, ip)
	}
	_ = a.store.SetKV(kvImportLastAt, fmt.Sprintf("%d", rec.At))
	// 唯一记录"每批从哪个 IP 进来"的地方：出问题时这是第一手证据。
	log.Printf("INFO: [import] 批次台账：ip=%s source=%s 新增=%d 更新=%d uids=%d",
		ipOrDash(ip), source, newN, updN, len(uids))
}

func ipOrDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// ======================================================================
// HTTP 端点
// ======================================================================

// handleImportIPStatus 报告「现在能不能导入」：当前出口 IP、上次导入 IP、是否需换。
//
// POST /api/admin/import/ip-status —— 前端在打开导入弹窗时调用，
// 让运维在**粘贴之前**就知道需不需要先去重启设备（而不是粘完被拒）。
func (a *app) handleImportIPStatus(w http.ResponseWriter, r *http.Request) {
	// expectNew=true：预检必须按"马上会有新增"来判，否则它会说"无需换 IP"、
	// 而用户粘完却被拒（粘完才被拒正是要避免的体验）。
	v := a.checkImportGuard(nil, true)
	v.NewCount = 0
	probe := probeExitIP()
	if probe.OK {
		v.ExitIP = probe.IP
		v.IPChanged = v.LastIP == "" || probe.IP != v.LastIP
		v.Unverified = false
	} else {
		v.Unverified = true
	}
	batches := []importBatchRecord{}
	if raw, _ := a.store.GetKV(kvImportBatches); strings.TrimSpace(raw) != "" {
		_ = json.Unmarshal([]byte(raw), &batches)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"gate":        v.Enabled,
		"max_new":     a.importMaxNew(),
		"exit_ip":     v.ExitIP,
		"last_ip":     v.LastIP,
		"last_at":     v.LastAt,
		"ip_changed":  v.IPChanged,
		"unverified":  v.Unverified,
		"need_ip":     v.NeedIP,
		"auto_reset":  a.ipResetCmd() != "",
		"reset_wait":  int(a.importResetWait().Seconds()),
		"reset_cmd":   a.ipResetCmd(), // 明文回显给管理员，便于核对填对了没有
		"probe_error": probe.Error,
		"batches":     batches,
	})
}

// handleImportIPReset 换 IP：有命令则执行并等待，无命令则校验人工换 IP 的结果。
//
// POST /api/admin/import/ip-reset
// 返回 {before, after, changed} —— **changed 来自实测**，不是命令退出码。
//
// 人工模式的正确用法：运维重启完设备，再调本接口 —— 此时 before 取「上次导入 IP」，
// after 是实测的当前 IP；两者不同即视为换成功。
func (a *app) handleImportIPReset(w http.ResponseWriter, r *http.Request) {
	last, _ := a.store.GetKV(kvImportLastIP)
	last = strings.TrimSpace(last)
	beforeProbe := probeExitIP()
	before := beforeProbe.IP
	if before == "" {
		before = last
	}

	var after string
	var changed bool
	if a.ipResetCmd() != "" {
		after, changed = a.resetExitIPIfNeeded(before)
	} else {
		// 人工模式：不执行任何动作，只**重测**并把结果与上次导入 IP 比对。
		after, changed = a.waitExitIPChange(last, importIPPollEvery+1)
		if !changed && after == "" {
			after = before
		}
	}
	note := ""
	if changed {
		note = "出口 IP 已变化，可以导入。"
	} else if last == "" {
		note = "库里还没有上次导入 IP，无需换 IP 即可导入。"
		changed = true
	} else {
		note = "出口 IP 仍是 " + ipOrDash(after) + "（与上次导入相同）。" +
			"请确认重启的是光猫（公网 IP 在光猫上，二级路由无效），重拨生效通常需十几秒。"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"before":  before,
		"after":   after,
		"last_ip": last,
		"changed": changed,
		"note":    note,
	})
}
