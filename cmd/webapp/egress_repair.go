// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 出口机器「一键修复」（v0.31.31）。
//
// # 为什么单开这个文件
//
// 「两台电脑、两个宽带，控制台拉起来的却是同一个出口 IP」有三种完全不同的成因，
// 而页面上它们长得一模一样（都是"两条线路、一个 IP"）：
//
//  1. **端口冲突**：两台机器的 agent 同时 poll，无锁的 load→save 让它们算出了
//     同一个隧道端口（v0.31.31 已加锁修掉）。后到的那台 `ssh -R` 失败，
//     但云端两条线路的 URL 都指向那个端口 —— 于是"两条线"= 同一台机器。
//  2. **隧道指向别处**：端口被别人占着，但本机 agent 报告"隧道已建立"，
//     而 `exit_ip` 是它自己直出的结果。云端从隧道这侧看到的 IP 才是真相。
//     两者不一致 = 这条隧道**没有落到这台机器上**。
//  3. **同一个公网出口**：两台机器确实各自跑得好好的，但它们都在同一个
//     NAT / 同一个宽带后面。这个软件改不了，只能如实说出来。
//
// 1 和 2 可以**自动修**：给这台机器换一个确定没被占用的端口，下发 start 让
// agent 重建隧道。3 只能解释（见前端文案），不要假装能修。
//
// # 边界
//
//   - 只改 `ports` 与 `cmd` 两个字段，不动账号、不动线路、不动全局默认出口；
//   - 只碰**本视角可见**的机器（用户只能修自己的机器）；
//   - 修复端口也必须落在 owner 的段内（用户密钥的 permitlisten 白名单卡着）。
package main

import (
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

// egressRepairAction 一条修复动作（给前端逐条显示）。
type egressRepairAction struct {
	Machine  string `json:"machine"`
	Hostname string `json:"hostname,omitempty"`
	// Kind 成因分类：port_conflict / exit_mismatch / port_out_of_seg。
	Kind string `json:"kind"`
	// Detail 人话说明。
	Detail string `json:"detail"`
	// FromPort / ToPort 端口迁移（0 表示没动端口）。
	FromPort int `json:"from_port,omitempty"`
	ToPort   int `json:"to_port,omitempty"`
}

// egressRepairKindText 成因 → 中文（前端也会自己映射，这里主要给日志用）。
func egressRepairKindText(kind string) string {
	switch kind {
	case "port_conflict":
		return "两台机器抢同一个隧道端口"
	case "exit_mismatch":
		return "隧道没有落到这台机器上（实测出口与自报不一致）"
	case "port_out_of_seg":
		return "隧道端口不在本账号允许的范围内"
	}
	return kind
}

// handleEgressRepair POST /api/egress/home/repair
//
// 对当前视角可见的机器做一次「端口体检 + 纠偏」：发现端口冲突 / 端口越段 /
// 隧道实测出口与 agent 自报不一致的机器，换一个新端口并下发 start。
// 幂等：没有问题的机器一个字段都不改，返回空 actions。
func (a *app) handleEgressRepair(w http.ResponseWriter, r *http.Request, scope egressScope) {
	actions, err := a.repairEgressMachines(scope)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	note := "没有发现需要修复的机器"
	if len(actions) > 0 {
		note = "已为 " + itoa(len(actions)) + " 台机器换端口并下发重建指令；" +
			"agent 下一次轮询（约 5 秒内）会按新端口重建隧道，稍后刷新即可看到新出口 IP"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"repaired": len(actions), "actions": actions, "note": note,
	})
}

// repairEgressMachines 真正的修复逻辑（可被测试直接调用）。
func (a *app) repairEgressMachines(scope egressScope) ([]egressRepairAction, error) {
	egressMachMu.Lock()
	defer egressMachMu.Unlock()

	m := a.machinesLoad()
	// 线路最近一次「从云端经隧道」探到的出口 IP —— 用来佐证隧道指向。
	proxyLastIP := map[string]string{}
	if proxies, err := a.store.ListEgressProxies(); err == nil {
		for _, p := range proxies {
			if ip := normalizeExitIP(p.LastIP); ip != "" {
				proxyLastIP[strings.TrimSpace(p.URL)] = ip
			}
		}
	}

	keys := make([]string, 0, len(m))
	for k, mm := range m {
		if mm == nil || !scope.seesMachine(mm) {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// 端口占用统计（**跨 owner 全表统计**）：sshd 的监听端口是全局的，
	// 两个 owner 拿到同一个端口一样会互相顶掉，所以不能只看本视角。
	byPort := map[int][]string{}
	for k, mm := range m {
		if mm == nil || mm.Ports.TunnelPort <= 0 {
			continue
		}
		byPort[mm.Ports.TunnelPort] = append(byPort[mm.Ports.TunnelPort], k)
	}

	actions := make([]egressRepairAction, 0, 2)
	needRepair := map[string]string{} // key → kind

	for _, k := range keys {
		mm := m[k]
		port := mm.Ports.TunnelPort
		url := egressHomeURL(port)

		// (1) 端口越段（用户机器必须在自己段内，否则 sshd 的 permitlisten 会拒）
		if mm.OwnerUserID > 0 && port > 0 && !egressMachinePortInSeg(mm) {
			needRepair[k] = "port_out_of_seg"
			continue
		}
		// (2) 端口冲突：同一个端口被多台机器登记。留下**最近上报**的那台
		//     （它大概率是真正持有监听的），其余换端口。
		if port > 0 && len(byPort[port]) > 1 {
			keeper := byPort[port][0]
			for _, other := range byPort[port][1:] {
				if om := m[other]; om != nil && om.StateAt > m[keeper].StateAt {
					keeper = other
				}
			}
			if k != keeper {
				needRepair[k] = "port_conflict"
				continue
			}
		}
		// (3) 隧道实测出口 ≠ agent 自报出口。两项都有值才判（缺值 = 不知道）。
		aIP := normalizeExitIP(mm.State.ExitIP)
		tIP := proxyLastIP[url]
		if aIP != "" && tIP != "" && aIP != tIP {
			needRepair[k] = "exit_mismatch"
		}
	}

	if len(needRepair) == 0 {
		return actions, nil
	}

	now := time.Now().Unix()
	// 按 key 排序后逐个换端口：分配函数读的是同一个 map，所以前一台换掉之后
	// 后一台不会又拿到同一个新端口。
	for _, k := range keys {
		kind, ok := needRepair[k]
		if !ok {
			continue
		}
		mm := m[k]
		old := mm.Ports.TunnelPort
		ports, ok := egressAllocPorts(m, mm.OwnerUserID)
		if !ok {
			// 段满：如实说明，不硬塞到段外（那样隧道一定建不起来）。
			log.Printf("WARN: [egress] 修复 machine=%s 失败：段 %s 已满",
				k, egressPortSegText(mm.OwnerUserID))
			actions = append(actions, egressRepairAction{
				Machine: k, Hostname: mm.Hostname, Kind: kind, FromPort: old,
				Detail: egressRepairKindText(kind) + "；但本账号的端口段已满，换不了新端口" +
					"（请停用不再使用的机器）",
			})
			continue
		}
		mm.Ports = ports
		// 端口变了 → 必须让 agent 重建（用现在的 At，agent 才会执行）。
		mm.Cmd, mm.CmdAt = "start", now
		actions = append(actions, egressRepairAction{
			Machine: k, Hostname: mm.Hostname, Kind: kind,
			FromPort: old, ToPort: ports.TunnelPort,
			Detail: egressRepairKindText(kind) + "；已从端口 " + itoa(old) +
				" 换到 " + itoa(ports.TunnelPort),
		})
		log.Printf("INFO: [egress] 修复 machine=%s（%s）：端口 %d → %d",
			k, kind, old, ports.TunnelPort)
	}
	if err := a.machinesSave(m); err != nil {
		return actions, err
	}
	return actions, nil
}
