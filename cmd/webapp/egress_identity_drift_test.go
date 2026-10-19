// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 机器身份漂移的回归测试（v0.31.49）。
//
// 现场（2026-10-19，用户 9915）：
//   - 同一台笔记本白天在公司 10.168.1.110、晚上在家 192.168.1.19；
//   - 旧指纹把**内网 IP** 算进哈希 → 云端判成"第二台机器"，多出一条线路 + 一个新端口；
//   - 用户控制台因此出现两张同名卡片（都叫 luciozzz），他点了旧那张的「一键拉起」，
//     指令写进早已下线的记录，页面转 90 秒后只说"等待超时"。
//
// 两条不变量在这里钉住：
//  1. **老记录已离线时**，同 id + 同主机名 + 指纹变化 = 同一台机器换了网络 → 就地认回
//     （键、端口、已绑定的账号全部不变）；
//  2. **老记录仍在线时**，同 id 不同指纹 = 真的是两台机器（复制安装目录）→ 必须分叉，
//     否则两条线路指向同一台机器、出口 IP 永远相同。
package main

import (
	"strconv"
	"testing"
	"time"
)

// TestEgressIdentityDriftAdopted 换网络（老记录已离线）必须被认回同一台。
func TestEgressIdentityDriftAdopted(t *testing.T) {
	_, a, _, _, _ := eaApp(t)
	const owner int64 = 9915

	first, created, _, err := a.egressRegister("m-04621ecd3a35ee5b", "luciozzz", "fp-home", "", owner, nil)
	if err != nil || !created {
		t.Fatalf("首台登记失败：created=%v err=%v", created, err)
	}
	port := first.Ports.TunnelPort
	key := egressMachineKey(owner, "m-04621ecd3a35ee5b")

	// 人为把这条记录推到"早已离线"（真实场景是几小时后换了网络）。
	m := a.machinesLoad()
	m[key].LastPollAt = time.Now().Unix() - 3600
	m[key].StateAt = time.Now().Unix() - 3600
	if err := a.machinesSave(m); err != nil {
		t.Fatalf("machinesSave: %v", err)
	}

	// 同一个 machine id、同一个主机名，但指纹变了（旧算法含内网 IP）。
	again, created2, dup, err := a.egressRegister("m-04621ecd3a35ee5b", "luciozzz", "fp-office", "", owner, nil)
	if err != nil {
		t.Fatalf("二次登记失败：%v", err)
	}
	if created2 {
		t.Fatal("同一台机器换网络不该新建线路（会多出一个端口、页面上多一张同名卡片）")
	}
	if dup {
		t.Fatal("同一台机器不该被判成「多装了一份 agent」")
	}
	if again.Ports.TunnelPort != port {
		t.Fatalf("端口被改了（%d → %d）—— 已绑定的账号会指向一个没人监听的端口",
			port, again.Ports.TunnelPort)
	}
	if again.Fingerprint != "fp-office" {
		t.Fatalf("指纹应就地刷新为 fp-office，得 %q", again.Fingerprint)
	}
	if again.LegacyFingerprint != "fp-home" {
		t.Fatalf("旧指纹应留痕，得 %q", again.LegacyFingerprint)
	}

	// 表里必须仍然只有一条该 owner 的记录。
	m = a.machinesLoad()
	n := 0
	for _, mm := range m {
		if mm != nil && mm.OwnerUserID == owner {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("同一台机器应只有 1 条记录，实得 %d 条（页面会显示成多台电脑）", n)
	}
}

// TestEgressIdentityDriftForksWhenBothOnline 两台同时在线（复制安装目录）必须分叉。
//
// 这是上一条的**反例**：如果为了修"换网络"就把所有"同 id 不同指纹"都认回，
// 那么真正复制的第二台电脑会顶掉第一台的线路 —— 那正是 v0.31.31 花大力气修掉的
// "两台电脑却是同一个出口 IP"。
func TestEgressIdentityDriftForksWhenBothOnline(t *testing.T) {
	_, a, _, _, _ := eaApp(t)
	const owner int64 = 9916

	first, _, _, err := a.egressRegister("m-same-id", "pc-a", "fp-first", "", owner, nil)
	if err != nil {
		t.Fatalf("首台登记失败：%v", err)
	}
	// 首台刚 poll 过（在线）—— 此时来了一台同 id 的新机器。
	second, created, _, err := a.egressRegister("m-same-id", "pc-a", "fp-second", "", owner, nil)
	if err != nil {
		t.Fatalf("第二台登记失败：%v", err)
	}
	if !created {
		t.Fatal("两台都在线时必须新建线路，不能把第一台顶掉")
	}
	if second.Ports.TunnelPort == first.Ports.TunnelPort {
		t.Fatalf("两条线路拿到同一个端口 %d —— 隧道会互相顶掉", first.Ports.TunnelPort)
	}
	if second.ID != first.ID {
		t.Fatalf("分叉记录的 ID 应沿用 agent 自报的 id，得 %q", second.ID)
	}
}

// TestEgressFingerprintUpgradeByPrev agent 升级后（新算法指纹）凭 fingerprint_prev 认回老记录。
//
// 过渡期最容易被忽略的一段：算法一改，**每台机器**的指纹都变了。若没有 prev 兜底，
// 平台自己的那台也会凭空多出一条线路，而它绑着一堆账号。
func TestEgressFingerprintUpgradeByPrev(t *testing.T) {
	_, a, _, _, _ := eaApp(t)

	old, created, _, err := a.egressRegister("m-plat", "DESKTOP-X", "legacy-fp", "", 0, nil)
	if err != nil || !created {
		t.Fatalf("老记录登记失败：created=%v err=%v", created, err)
	}
	port := old.Ports.TunnelPort

	// 升级后的 agent：新指纹 + 旧指纹一起报。
	up, created2, _, err := a.egressRegister("m-plat", "DESKTOP-X", "new-fp", "legacy-fp", 0, nil)
	if err != nil {
		t.Fatalf("升级后上报失败：%v", err)
	}
	if created2 {
		t.Fatal("凭 fingerprint_prev 应认回既有记录，不该新建线路")
	}
	if up.Ports.TunnelPort != port {
		t.Fatalf("端口变了（%d → %d）", port, up.Ports.TunnelPort)
	}
	if up.Fingerprint != "new-fp" {
		t.Fatalf("记录上的指纹应升级为 new-fp，得 %q", up.Fingerprint)
	}
}

// TestEgressOfflinePullRefused 向明显离线的机器下发 start 必须**立刻**被拒绝。
//
// 用户侧的体验是"点了拉起、转 90 秒、超时"—— 和"功能坏了"没有任何区别。
// 真正的原因通常是"你选中的是另一台已下线的机器"。
func TestEgressOfflinePullRefused(t *testing.T) {
	_, a, mux, adminCookie, _ := eaApp(t)
	const owner int64 = 9917

	mm, _, _, err := a.egressRegister("m-dead", "pc-dead", "fp-dead", "", owner, nil)
	if err != nil {
		t.Fatalf("登记失败：%v", err)
	}
	_ = mm
	key := egressMachineKey(owner, "m-dead")
	// 让它离线 6 小时。
	m := a.machinesLoad()
	m[key].LastPollAt = time.Now().Unix() - 6*3600
	m[key].StateAt = m[key].LastPollAt
	if err := a.machinesSave(m); err != nil {
		t.Fatalf("machinesSave: %v", err)
	}

	code, body := eaReq(t, mux, "POST", "/api/egress/home/pull",
		`{"machine":"`+key+`"}`, adminCookie, "")
	if code != 409 {
		t.Fatalf("离线机器应返回 409（明确拒绝），得 %d body=%.200s", code, body)
	}
	d := eaJSON(t, body)
	if msg, _ := d["error"].(string); msg == "" || !contains(msg, "心跳") {
		t.Fatalf("错误信息必须说清「多久没心跳」，得 %.200s", body)
	}
}

// contains 避免为一个断言引入 strings 依赖歧义（同文件内自足）。
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestEgressLegacyStateMigratedOnce 旧单机 kv 只能被读一次。
//
// 现场（v0.31.46~48）：default 桶里那条老记录被 prune 清掉后，下一次 machinesLoad
// 又从 `egress_agent_state` 里把它**重建**出来，prune 再清、再重建 —— 每 5 秒两行
// DEBUG 日志，16 小时上万条，把真正要排查的扫码/代理日志整段淹掉。
func TestEgressLegacyStateMigratedOnce(t *testing.T) {
	_, a, _, _, _ := eaApp(t)
	now := time.Now().Unix()

	// 造一份"升级前"的单机 kv：老记录很老、无指纹，与下面登记的新机器同主机名。
	legacy := `{"at":` + strconv.FormatInt(now-2*staleMachineAge, 10) + `,"hostname":"pc-old","exit_ip":"1.2.3.4"}`
	if err := a.store.SetKV(kvEgressLegacyState, legacy); err != nil {
		t.Fatalf("SetKV: %v", err)
	}

	// 新 agent 上报：prune 会清掉重建出来的 default，并落盘。
	if _, _, _, err := a.egressRegister("m-new", "pc-old", "fp-new", "", 0, nil); err != nil {
		t.Fatalf("登记失败：%v", err)
	}

	// 再读一次：default 不能被旧 kv 复建（否则就是删建循环）。
	m := a.machinesLoad()
	if _, ok := m[egressDefaultMachine]; ok {
		t.Fatal("旧 kv 又被复建了 —— 这正是每 5 秒刷两行日志的成因")
	}
	if !a.egressLegacyMigrateNeeded() {
		return // 标记已写，符合预期
	}
	t.Fatal("迁移标记没写进 kv，下次启动还会再读一遍旧 kv")
}

// TestPruneDriftedSiblings 同 id 同主机名的旧记录：只在该机**在线**且旧记录久离线时清掉。
func TestPruneDriftedSiblings(t *testing.T) {
	now := time.Now().Unix()
	cur := &egressMachine{ID: "m-x", Hostname: "luciozzz", OwnerUserID: 9915,
		Fingerprint: "fp-new", Ports: egressAgentPorts{TunnelPort: 21151},
		StateAt: now, LastPollAt: now}
	old := &egressMachine{ID: "m-x", Hostname: "luciozzz", OwnerUserID: 9915,
		Fingerprint: "fp-old", Ports: egressAgentPorts{TunnelPort: 21150},
		StateAt: now - 2*staleMachineAge}
	alive := &egressMachine{ID: "m-x", Hostname: "luciozzz", OwnerUserID: 9915,
		Fingerprint: "fp-other", Ports: egressAgentPorts{TunnelPort: 21152},
		StateAt: now - 10}
	otherHost := &egressMachine{ID: "m-x", Hostname: "another-pc", OwnerUserID: 9915,
		Fingerprint: "fp-3", StateAt: now - 2*staleMachineAge}

	m := map[string]*egressMachine{
		"u9915:m-x~new":  cur,
		"u9915:m-x~old":  old,
		"u9915:m-x~live": alive,
		"u9915:m-x~far":  otherHost,
	}
	got := pruneDriftedSiblings(m, 9915, cur, now)
	if len(got) != 1 || got[0] != "u9915:m-x~old" {
		t.Fatalf("应只清掉 ~old，实得 %v", got)
	}
	if _, ok := m["u9915:m-x~live"]; !ok {
		t.Fatal("刚上报过的同 id 记录不能删（可能是两台交替开机的机器）")
	}
	if _, ok := m["u9915:m-x~far"]; !ok {
		t.Fatal("主机名不同的记录不能删")
	}

	// 当前机器离线时，一条都不许删。
	curOffline := *cur
	curOffline.StateAt = now - 3600
	curOffline.LastPollAt = now - 3600
	if got := pruneDriftedSiblings(m, 9915, &curOffline, now); len(got) != 0 {
		t.Fatalf("当前机器离线时不该清理，实得 %v", got)
	}
}
