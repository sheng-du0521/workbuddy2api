// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 机器身份与端口唯一性的回归测试（v0.31.31）。
//
// 用户现场："两台电脑、两个宽带，控制台拉起来却是同一个出口 IP"。
// 三条成因里，代码能修的两条都在这份用例里钉住：
//
//	① 端口冲突 —— 两台机器的 agent 同时 poll，无锁的 load→save 让它们各自算出
//	   "第一个空闲端口"，于是两张记录拿到**同一个隧道端口**；后到的那台
//	   `ssh -R` 失败，而云端两条线路都指向先到的那台 → 同一个出口 IP。
//	② 复制安装目录 —— 第二台电脑带着同一个 `machine.id` 上来，被当成同一台机器
//	   （同一个端口、同一条线路）。指纹能识破它。
//
// 另外钉住"重装后 id 变了但机器没变"必须被认回来（否则列表里堆幽灵机器、白占端口）。
package main

import (
	"fmt"
	"sync"
	"testing"
)

// TestEgressMachineIdentityByFingerprint 指纹判定：同机合并、异机分开、复制目录不被顶掉。
func TestEgressMachineIdentityByFingerprint(t *testing.T) {
	_, a, _, _, _ := eaApp(t)
	const owner int64 = 7

	// 1) 两台不同机器各自登记 → 端口必须不同（否则隧道互相顶掉）。
	m1, created1, dup1, err := a.egressRegister("m-aaa", "pc-a", "fpaaaa", "", owner, nil)
	if err != nil || !created1 || dup1 {
		t.Fatalf("首台登记应 created=true dup=false，得 created=%v dup=%v err=%v", created1, dup1, err)
	}
	m2, created2, _, err := a.egressRegister("m-bbb", "pc-b", "fpbbbb", "", owner, nil)
	if err != nil || !created2 {
		t.Fatalf("第二台应 created=true，得 %v err=%v", created2, err)
	}
	if m1.Ports.TunnelPort == m2.Ports.TunnelPort {
		t.Fatalf("两台机器拿到了同一个隧道端口 %d —— 隧道会互相顶掉，出口 IP 必然相同",
			m1.Ports.TunnelPort)
	}

	// 2) 复制安装目录：同一个 machine id、不同指纹 → 必须是**新的一台 + 新端口**，
	//    而且不能把原来那台顶掉（顶掉 = 第一台电脑的线路静默消失）。
	m3, created3, _, err := a.egressRegister("m-aaa", "pc-c", "fpcccc", "", owner, nil)
	if err != nil || !created3 {
		t.Fatalf("复制目录应登记为新机器，得 created=%v err=%v", created3, err)
	}
	if m3.Ports.TunnelPort == m1.Ports.TunnelPort {
		t.Fatalf("复制目录的第二台拿到了与第一台相同的端口 %d", m3.Ports.TunnelPort)
	}
	orig, _, _, err := a.egressRegister("m-aaa", "pc-a", "fpaaaa", "", owner, nil)
	if err != nil {
		t.Fatalf("回查第一台失败：%v", err)
	}
	if orig.Ports.TunnelPort != m1.Ports.TunnelPort {
		t.Fatalf("第一台的端口被改动了（%d → %d）—— 复制目录不该影响原机",
			m1.Ports.TunnelPort, orig.Ports.TunnelPort)
	}
	if orig.Hostname != "pc-a" {
		t.Fatalf("第一台的 hostname 被覆盖成 %q", orig.Hostname)
	}

	// 3) 重装 / 删掉 machine.id → 新 id、同指纹：必须**认回同一台**，
	//    不能多出一台幽灵机器（否则列表里"9 台电脑"、端口一路往后涨）。
	m4, created4, dup4, err := a.egressRegister("m-zzz", "pc-b", "fpbbbb", "", owner, nil)
	if err != nil {
		t.Fatalf("同指纹不同 id 登记失败：%v", err)
	}
	if created4 {
		t.Fatal("同指纹应认回既有记录，不该新建")
	}
	if m4.ID != "m-bbb" || m4.Ports.TunnelPort != m2.Ports.TunnelPort {
		t.Fatalf("应归并到 m-bbb（端口 %d），得 id=%q 端口 %d",
			m2.Ports.TunnelPort, m4.ID, m4.Ports.TunnelPort)
	}
	if !dup4 {
		t.Fatal("同一台机器上出现第二个 agent id 应被判为重复接入")
	}
	// 重复的 id 要留痕：页面靠它说"请停掉哪一份"。
	found := false
	for _, x := range m4.AltIDs {
		if x == "m-zzz" {
			found = true
		}
	}
	if !found {
		t.Fatalf("AltIDs 应包含重复的 agent id，得 %v", m4.AltIDs)
	}
}

// TestEgressRegisterConcurrentPorts 并发登记不会分到同一个端口。
//
// 这条直接对应"两台电脑同时上线"的真实时序：两个 poll / report 交错进来。
// 修复前没有锁，两个请求各自读到同一份快照 → 同一个"第一个空闲端口"。
func TestEgressRegisterConcurrentPorts(t *testing.T) {
	_, a, _, _, _ := eaApp(t)
	const owner int64 = 9 // 段 12090-12099，正好塞得下 8 台

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_, _, _, err := a.egressRegister(
				fmt.Sprintf("m-conc-%d", i),
				fmt.Sprintf("pc-%d", i),
				fmt.Sprintf("fpconc%d", i), "",
				owner, nil)
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 台登记失败：%v", i, err)
		}
	}

	m := a.machinesLoad()
	seen := map[int]string{}
	for k, mm := range m {
		if mm == nil || mm.OwnerUserID != owner {
			continue
		}
		p := mm.Ports.TunnelPort
		if p <= 0 {
			t.Fatalf("%s 没有拿到端口", k)
		}
		if prev, dup := seen[p]; dup {
			t.Fatalf("端口 %d 被两台机器同时占用：%s 与 %s —— 并发登记丢了互斥",
				p, prev, k)
		}
		seen[p] = k
	}
	if len(seen) != n {
		t.Fatalf("应有 %d 台机器各自一个端口，实得 %d 个", n, len(seen))
	}
}

// TestEgressRepairPortConflict 修复接口：端口冲突的机器被换到新端口并下发 start。
func TestEgressRepairPortConflict(t *testing.T) {
	_, a, _, _, _ := eaApp(t)

	// 手工造两条**共用同一个端口**的记录（模拟修复前留下的脏数据）。
	m := map[string]*egressMachine{
		"u7:m-x": {
			ID: "m-x", Hostname: "pc-x", OwnerUserID: 7, Fingerprint: "fpxxxx",
			Ports: egressAgentPorts{ProxyPort: 1080, TunnelPort: 12070}, StateAt: 100,
		},
		"u7:m-y": {
			ID: "m-y", Hostname: "pc-y", OwnerUserID: 7, Fingerprint: "fpyyyy",
			Ports: egressAgentPorts{ProxyPort: 1080, TunnelPort: 12070}, StateAt: 200,
		},
	}
	if err := a.machinesSave(m); err != nil {
		t.Fatalf("machinesSave: %v", err)
	}

	actions, err := a.repairEgressMachines(egressScope{Platform: true})
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("应只修一台（最近上报的那台保留原端口），得 %d 条动作：%+v", len(actions), actions)
	}
	if actions[0].Kind != "port_conflict" {
		t.Fatalf("成因应判为 port_conflict，得 %q", actions[0].Kind)
	}
	if actions[0].Machine != "u7:m-x" {
		t.Fatalf("应移动较早上报的那台 u7:m-x，得 %q", actions[0].Machine)
	}

	after := a.machinesLoad()
	px, py := after["u7:m-x"], after["u7:m-y"]
	if px == nil || py == nil {
		t.Fatal("修复后两台机器都应还在（不能把冲突的那台删掉）")
	}
	if px.Ports.TunnelPort == py.Ports.TunnelPort {
		t.Fatalf("修复后仍然撞端口：%d", px.Ports.TunnelPort)
	}
	if px.Cmd != "start" {
		t.Fatalf("换端口后必须下发 start 让 agent 重建隧道，得 cmd=%q", px.Cmd)
	}
	if py.Cmd == "start" {
		t.Fatal("没冲突的那台不该被下发指令")
	}
	// 新端口必须落在该 owner 的段内，否则 sshd 的 permitlisten 会拒。
	owner := egressScope{UserID: 7}
	if !owner.seesMachine(px) {
		t.Fatal("修复不应改变归属")
	}
	base, span := egressPortSegOf(7)
	if px.Ports.TunnelPort < base || px.Ports.TunnelPort >= base+span {
		t.Fatalf("新端口 %d 不在段 %d-%d 内", px.Ports.TunnelPort, base, base+span-1)
	}
}
