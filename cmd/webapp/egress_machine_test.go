// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 出口代理「多机并存」的回归测试（v0.31.18）。
//
// 用户现场：在**第二台电脑**上跑 agent 时，v0.31.17 的三处全局单例同时撞车 ——
//
//	① 指令单例 → "我在 A 机点的拉起，被 B 机执行了"（页面看不出这件事）；
//	② 状态单例 → 两台每 5 秒互相覆盖，页面上的 hostname / 出口 IP 随机跳；
//	③ 端口单例 → 两台都 `ssh -R 11080`，**后到的那台必然失败**
//	   （`remote port forwarding failed`），而错误只在该机 agent 日志里。
//
// 本用例钉住修好后的三件事：端口自动错开、指令定向、状态分桶。
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestEgressMachineMultiHost 两台机器并存：端口错开 + 指令定向 + 状态分桶。
func TestEgressMachineMultiHost(t *testing.T) {
	store, a, _, _, _ := eaApp(t)
	tok, err := a.egressAgentTokenGet()
	if err != nil {
		t.Fatalf("egressAgentTokenGet: %v", err)
	}
	_ = store

	poll := func(machine, host string) egressAgentCmd {
		t.Helper()
		u := "/api/agent/poll"
		var q []string
		if machine != "" {
			q = append(q, "machine="+machine)
		}
		if host != "" {
			q = append(q, "hostname="+host)
		}
		if len(q) > 0 {
			u += "?" + strings.Join(q, "&")
		}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, u, nil)
		req.Header.Set("X-Agent-Token", tok)
		a.handleEgressAgentPoll(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("poll(%q) 应 200，得 %d %s", machine, rec.Code, rec.Body.String())
		}
		var c egressAgentCmd
		if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil {
			t.Fatalf("poll 响应不是合法 JSON: %v", err)
		}
		return c
	}
	report := func(machine string, st egressAgentState) {
		t.Helper()
		raw, _ := json.Marshal(st)
		rec := httptest.NewRecorder()
		u := "/api/agent/report"
		if machine != "" {
			u += "?machine=" + machine
		}
		req := httptest.NewRequest(http.MethodPost, u, strings.NewReader(string(raw)))
		req.Header.Set("X-Agent-Token", tok)
		a.handleEgressAgentReport(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("report(%q) 应 200，得 %d %s", machine, rec.Code, rec.Body.String())
		}
	}
	findMachine := func(v egressHomeView, id string) *egressMachineView {
		for i := range v.Machines {
			if v.Machines[i].ID == id {
				return &v.Machines[i]
			}
		}
		return nil
	}

	// ---- 1) 老 agent（不带 machine）占 default 桶，端口保持 11080 ----
	c1 := poll("", "")
	if c1.TunnelPort != egressBaseTunnelPort {
		t.Fatalf("老 agent（default 桶）应保持 %d，得 %d —— 升级不该改变已跑着的这台",
			egressBaseTunnelPort, c1.TunnelPort)
	}
	// ---- 2) 第二台机器必须拿到**不同**端口 ----
	c2 := poll("mid-2", "pc-2")
	if c2.TunnelPort == c1.TunnelPort {
		t.Fatalf("两台机器的隧道端口必须错开，否则后到的那台 ssh -R 必然失败（都得 %d）", c2.TunnelPort)
	}
	if c2.TunnelPort != egressBaseTunnelPort+1 {
		t.Fatalf("第二台应拿到 %d，得 %d", egressBaseTunnelPort+1, c2.TunnelPort)
	}

	// ---- 3) 指令必须定向：给 mid-2 的 start 不能被 default 桶取走 ----
	m := a.machinesLoad()
	if mm, ok := m["mid-2"]; !ok {
		t.Fatal("poll 之后 mid-2 应已登记")
	} else {
		mm.Cmd, mm.CmdAt = "start", 1234567
	}
	if err := a.machinesSave(m); err != nil {
		t.Fatalf("machinesSave: %v", err)
	}
	if got := poll("mid-2", "pc-2").Action; got != "start" {
		t.Fatalf("mid-2 应取到自己的 start 指令，得 %q", got)
	}
	if got := poll("", "").Action; got != "none" {
		t.Fatalf("指令必须定向：给 mid-2 的 start 不该被老 agent 取走，得 %q", got)
	}

	// ---- 4) 状态分桶：两台各报各的，互不覆盖 ----
	report("", egressAgentState{Hostname: "pc-old", ProxyOK: true, TunnelOK: true, ExitIP: "1.1.1.1"})
	report("mid-2", egressAgentState{Hostname: "pc-2", ProxyOK: true, TunnelOK: true, ExitIP: "2.2.2.2"})

	v := a.buildEgressHomeView(httptest.NewRequest(http.MethodGet, "/api/egress/home", nil),
		egressScope{Platform: true})
	if len(v.Machines) != 2 {
		t.Fatalf("应看到 2 台机器，得 %d: %+v", len(v.Machines), v.Machines)
	}
	m1, m2 := findMachine(v, egressDefaultMachine), findMachine(v, "mid-2")
	if m1 == nil || m2 == nil {
		t.Fatalf("两台机器都该在列表里，得 %+v", v.Machines)
	}
	if m1.ExitIP != "1.1.1.1" || m2.ExitIP != "2.2.2.2" {
		t.Fatalf("状态被互相覆盖了：default=%q mid-2=%q", m1.ExitIP, m2.ExitIP)
	}
	if m1.Hostname != "pc-old" || m2.Hostname != "pc-2" {
		t.Fatalf("hostname 串了：default=%q mid-2=%q", m1.Hostname, m2.Hostname)
	}
	// 两台各自的线路 URL 必须不同（否则"第二台接进来"等于没接）。
	if m1.ProxyURL == m2.ProxyURL {
		t.Fatalf("不同机器的线路 URL 不该相同，得 %q", m1.ProxyURL)
	}
	if !strings.HasSuffix(m2.ProxyURL, fmt.Sprintf(":%d", c2.TunnelPort)) {
		t.Fatalf("mid-2 的线路 URL 应指向它自己的隧道端口，得 %q", m2.ProxyURL)
	}

	// ---- 5) 同指纹 = 同一台机器（v0.31.26）：必须点出来，别让人以为有两条线 ----
	//
	// 现场原型：本机先后跑了 v0.1.0（占 default 桶）与 v0.3.0（用户令牌），
	// 两个 key、两个端口、两条线路，出口都是 39.91.80.16。
	report("", egressAgentState{Hostname: "DESKTOP-AQO1QE4", ExitIP: "39.91.80.16（山东 济南 联通）", Fingerprint: "fp-same"})
	report("mid-2", egressAgentState{Hostname: "DESKTOP-AQO1QE4", ExitIP: "39.91.80.16", Fingerprint: "fp-same"})
	v2 := a.buildEgressHomeView(httptest.NewRequest(http.MethodGet, "/api/egress/home", nil), egressScope{Platform: true})
	x1, x2 := findMachine(v2, egressDefaultMachine), findMachine(v2, "mid-2")
	if len(x1.SameMachineOf) != 1 || x1.SameMachineOf[0] != "mid-2" {
		t.Fatalf("default 应指出与自己同指纹的 mid-2，得 %v", x1.SameMachineOf)
	}
	if len(x2.SameMachineOf) != 1 || x2.SameMachineOf[0] != egressDefaultMachine {
		t.Fatalf("mid-2 应反过来指出 default，得 %v", x2.SameMachineOf)
	}
	// 同指纹的也同出口，但不要再重复报一次 SameExitOf（一个事实说一遍）。
	if len(x1.SameExitOf) != 0 {
		t.Fatalf("同指纹机器不该再进 SameExitOf，得 %v", x1.SameExitOf)
	}

	// 复合串与纯 IP 必须被认成同一个出口（老 agent 报的就是复合串）。
	byURL := a.egressMachinesByURL()
	if got := byURL[x1.ProxyURL]["exit_ip"]; got != "39.91.80.16" {
		t.Fatalf("机器出口 IP 应归一化为纯 IP，得 %v", got)
	}

	// ---- 5b) 老 agent（无指纹）也必须能取线路视图 ----
	//
	// 这条是**线上 500 的回归**：`egressMachinesByURL` 里按指纹分组后
	// `make([]string, 0, len(group)-1)`，无指纹时 group 为 nil → cap=-1 → panic，
	// 整个代理页 500。此前用例都设了指纹，所以从未走到这条路径。
	// 必须把**存量记录上的指纹清空**才算真的走到那条路径：
	// 上报处理只在指纹非空时覆盖（老 agent 不上报时保留旧值），
	// 所以单发一条无指纹的 report 是模拟不出"从没上报过指纹"的机器的。
	mn := a.machinesLoad()
	for _, x := range mn {
		x.Fingerprint = ""
	}
	if err := a.machinesSave(mn); err != nil {
		t.Fatalf("machinesSave: %v", err)
	}
	report("", egressAgentState{Hostname: "legacy", ExitIP: "1.2.3.4"})
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("无指纹的老 agent 不该让线路视图 panic：%v", r)
			}
		}()
		out := a.egressMachinesByURL()
		if len(out) == 0 {
			t.Fatal("无指纹时也应返回机器视图")
		}
	}()

	// ---- 6) 两台不同机器、出口相同：不是配置错误，但"没有新 IP"必须说清 ----
	report("", egressAgentState{Hostname: "pc-a", ExitIP: "8.8.8.8", Fingerprint: "fp-a"})
	report("mid-2", egressAgentState{Hostname: "pc-b", ExitIP: "8.8.8.8", Fingerprint: "fp-b"})
	v3 := a.buildEgressHomeView(httptest.NewRequest(http.MethodGet, "/api/egress/home", nil), egressScope{Platform: true})
	y1 := findMachine(v3, egressDefaultMachine)
	if len(y1.SameMachineOf) != 0 {
		t.Fatalf("不同指纹不该算同一台机器，得 %v", y1.SameMachineOf)
	}
	if len(y1.SameExitOf) != 1 || y1.SameExitOf[0] != "mid-2" {
		t.Fatalf("应指出 mid-2 与自己同出口，得 %v", y1.SameExitOf)
	}
}

// TestEgressHomeApplyTargetsMachine 应用动作只影响**被指定的那台**机器的线路。
//
// 这条防的是："在 B 机页面上点应用，结果把 A 机的线路设成了全局默认" ——
// 多机之后这是最容易发生的越界（默认出口是全局唯一的，选错机器就选错了链路）。
func TestEgressHomeApplyTargetsMachine(t *testing.T) {
	store, a, mux, adminCookie, _ := eaApp(t)

	// 造两台已登记的机器（不跑真实隧道，直接写机器表）。
	m := map[string]*egressMachine{
		egressDefaultMachine: {ID: egressDefaultMachine, Hostname: "pc-old",
			Ports: egressAgentPorts{ProxyPort: 1080, TunnelPort: 11080}, StateAt: 100},
		"mid-2": {ID: "mid-2", Hostname: "pc-2",
			Ports: egressAgentPorts{ProxyPort: 1080, TunnelPort: 11081}, StateAt: 200},
	}
	if err := a.machinesSave(m); err != nil {
		t.Fatalf("machinesSave: %v", err)
	}
	// 起一个真的监听在 11081 上，让 egressTunnelReachable 判为"通"。
	ln, err := net.Listen("tcp", "127.0.0.1:11081")
	if err != nil {
		t.Skipf("11081 被占用，跳过本用例: %v", err)
	}
	defer ln.Close()

	// 必须走**真 mux**：应用动作挂的是 adminGuard + 会话级二层校验，
	// 直接调 handler 会 403（而这正是这条用例想验证的鉴权面之一）。
	code, raw := eaReq(t, mux, "POST", "/api/egress/home/apply", `{"machine":"mid-2"}`, adminCookie, "")
	if code != http.StatusOK {
		t.Fatalf("应用 mid-2 应成功，得 %d %s", code, raw)
	}
	def, _ := store.GetKV(kvEgressDefault)
	if def != "http://127.0.0.1:11081" {
		t.Fatalf("默认出口应指向**被指定**的 mid-2 线路，得 %q", def)
	}
	proxies, _ := store.ListEgressProxies()
	if len(proxies) != 1 || proxies[0].URL != "http://127.0.0.1:11081" {
		t.Fatalf("只该注册 mid-2 那一条线路，得 %+v", proxies)
	}
}

// TestEgressAssignToSpecificProxy 指定线路的批量分配（v0.31.19）。
//
// 线路卡右侧的「分配」按钮走的就是这条路：只把**这一条**线路发给目标账号，
// 不动其它线路。此前分配只能选「范围」、线路由服务端从整个池子里挑 ——
// 于是"我只想把这条本地住宅线路给某几个号"做不到，只能先去停用其它线路，
// 而那又会牵动正在用它们的账号。
func TestEgressAssignToSpecificProxy(t *testing.T) {
	store, _, mux, adminCookie, _ := eaApp(t)
	for _, p := range []*EgressProxy{
		{ID: "p1", Name: "线路1", URL: "http://127.0.0.1:11080", Enabled: true},
		{ID: "p2", Name: "线路2", URL: "http://127.0.0.1:1088", Enabled: true},
	} {
		if err := store.UpsertEgressProxy(p); err != nil {
			t.Fatalf("UpsertEgressProxy: %v", err)
		}
	}
	for _, uid := range []string{"ea-spec-1", "ea-spec-2"} {
		if err := store.UpsertAccount(&Account{UID: uid, Nickname: uid, Realm: "cn", AccessToken: "t"}); err != nil {
			t.Fatalf("UpsertAccount: %v", err)
		}
	}

	code, raw := eaReq(t, mux, "POST", "/api/admin/egress/assign",
		`{"uids":["ea-spec-1","ea-spec-2"],"proxy_url":"http://127.0.0.1:11080","dry_run":false}`,
		adminCookie, "")
	if code != http.StatusOK {
		t.Fatalf("指定线路分配应成功，得 %d %s", code, raw)
	}
	accs, err := store.ListAccounts()
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	for _, acc := range accs {
		if acc.EgressProxy != "http://127.0.0.1:11080" {
			t.Fatalf("账号 %s 应绑到**指定**线路，得 %q（说明分配挑了别的线路）", acc.UID, acc.EgressProxy)
		}
	}

	// 指定一条不在池里 / 被停用的线路 → 400，且措辞要指向"这条线路"。
	// （笼统回"池子空了"会把运维引到错误的排查方向：去加线路，而不是去查这条。）
	code2, raw2 := eaReq(t, mux, "POST", "/api/admin/egress/assign",
		`{"uids":["ea-spec-1"],"proxy_url":"http://10.0.0.9:3128","dry_run":true}`,
		adminCookie, "")
	if code2 != http.StatusBadRequest || !strings.Contains(raw2, "指定的线路") {
		t.Fatalf("指定不可用线路应 400 且说明是【指定的线路】，得 %d %s", code2, raw2)
	}
}
