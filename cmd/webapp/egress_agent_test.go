// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 本地出口代理「一键拉起」的回归测试（v0.31.17）。
//
// 走**真 mux**：这一刀的价值有一半在路由与鉴权上（agent 用专用令牌、控制台用
// 会话、管理员动作再收一道），直接调 handler 测不出"路由没挂 guard"。
//
// 重点钉住四件事：
//  1. agent 端点**只认专用令牌**（拿会话/管理密钥都进不来）；
//  2. 控制台动作**只有管理员**能做（它改的是全平台出站链路）；
//  3. 「应用」是**幂等**的（重复拉起不堆线路、不重复绑）；
//  4. 隧道不通时**拒绝应用**（否则会配出一条不通的线路，比不配更坏）。
package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// eaApp 装配：真 mux + 一个管理员、一个普通用户。
func eaApp(t *testing.T) (*Store, *app, *http.ServeMux, string, string) {
	t.Helper()
	store, px, a := newTestApp(t)
	a.key = "ea-admin-key"
	a.limiter = newClientLimiter()
	a.alerts = newAlertState()
	a.dataDir = t.TempDir()
	_ = px

	admin, err := store.CreateUser("ea-admin", "x")
	if err != nil {
		t.Fatalf("CreateUser admin: %v", err)
	}
	if err := store.SetUserRole(admin.ID, RoleAdmin); err != nil {
		t.Fatalf("SetUserRole: %v", err)
	}
	member, err := store.CreateUser("ea-member", "x")
	if err != nil {
		t.Fatalf("CreateUser member: %v", err)
	}
	mux := a.routes()
	t.Cleanup(func() { store.Close() })
	return store, a, mux, sessionFor(t, store, admin), sessionFor(t, store, member)
}

// eaReq 打一次真 mux 请求。
func eaReq(t *testing.T, mux *http.ServeMux, method, path, body, cookie, agentToken string) (int, string) {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	}
	if agentToken != "" {
		req.Header.Set("X-Agent-Token", agentToken)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func eaJSON(t *testing.T, raw string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("响应不是 JSON：%v body=%.200s", err, raw)
	}
	return out
}

// TestEgressAgentTokenIsTheOnlyWay agent 端点只认专用令牌。
//
// 这条防的是"顺手用管理密钥/会话把 agent 端点也放行" —— 那等于给本机一个
// 能读全站数据的凭证。
func TestEgressAgentTokenIsTheOnlyWay(t *testing.T) {
	store, a, mux, adminCookie, _ := eaApp(t)
	tok, err := a.egressAgentTokenGet()
	if err != nil {
		t.Fatalf("egressAgentTokenGet: %v", err)
	}
	if !strings.HasPrefix(tok, "ag-") || len(tok) < 40 {
		t.Fatalf("令牌应足够长且带前缀，得 %q", tok)
	}
	_ = store

	for _, c := range []struct {
		name, cookie, token string
		want                int
	}{
		{"无令牌", "", "", http.StatusUnauthorized},
		{"错令牌", "", "ag-deadbeef", http.StatusUnauthorized},
		{"仅会话（不算）", adminCookie, "", http.StatusUnauthorized},
		{"正确令牌", "", tok, http.StatusOK},
	} {
		code, _ := eaReq(t, mux, "GET", "/api/agent/poll", "", c.cookie, c.token)
		if code != c.want {
			t.Errorf("poll [%s] 状态码 = %d，期望 %d", c.name, code, c.want)
		}
		code, _ = eaReq(t, mux, "POST", "/api/agent/report", `{}`, c.cookie, c.token)
		if code != c.want {
			t.Errorf("report [%s] 状态码 = %d，期望 %d", c.name, code, c.want)
		}
	}
}

// TestEgressAgentCmdRoundTrip 指令往返：管理员下发 → agent 取到（含默认端口）。
func TestEgressAgentCmdRoundTrip(t *testing.T) {
	_, a, mux, adminCookie, _ := eaApp(t)
	tok, _ := a.egressAgentTokenGet()

	// 未下发时 action = none（agent 因此什么都不做）。
	_, raw := eaReq(t, mux, "GET", "/api/agent/poll", "", "", tok)
	if got := eaJSON(t, raw)["action"]; got != "none" {
		t.Fatalf("未下发时应为 none，得 %v", got)
	}

	code, _ := eaReq(t, mux, "POST", "/api/egress/home/pull", "", adminCookie, "")
	if code != http.StatusOK {
		t.Fatalf("管理员下发指令应 200，得 %d", code)
	}
	_, raw = eaReq(t, mux, "GET", "/api/agent/poll", "", "", tok)
	got := eaJSON(t, raw)
	if got["action"] != "start" {
		t.Fatalf("下发后 agent 应取到 start，得 %v", got["action"])
	}
	if got["proxy_port"] != float64(1080) || got["tunnel_port"] != float64(11080) {
		t.Fatalf("端口应带上默认值（1080/11080），得 %v/%v", got["proxy_port"], got["tunnel_port"])
	}
}

// TestEgressAgentReportMakesOnline 上报后控制台显示在线 + 出口 IP。
func TestEgressAgentReportMakesOnline(t *testing.T) {
	_, a, mux, adminCookie, _ := eaApp(t)
	tok, _ := a.egressAgentTokenGet()

	body := `{"version":"test","proxy_ok":true,"tunnel_ok":true,"exit_ip":"1.2.3.4（山东 济南 联通）"}`
	if code, _ := eaReq(t, mux, "POST", "/api/agent/report", body, "", tok); code != http.StatusOK {
		t.Fatalf("上报应 200，得 %d", code)
	}
	_, raw := eaReq(t, mux, "GET", "/api/egress/home", "", adminCookie, "")
	v := eaJSON(t, raw)
	if v["agent_online"] != true {
		t.Fatalf("上报后应显示在线，得 %v", v["agent_online"])
	}
	st, _ := v["agent_state"].(map[string]any)
	if st == nil || !strings.Contains(st["exit_ip"].(string), "1.2.3.4") {
		t.Fatalf("出口 IP 应透出，得 %v", v["agent_state"])
	}
	// 有了 agent 命令（管理员才拿得到），前端才能给出"在本机跑这行"。
	if cmd, _ := v["agent_command"].(string); !strings.Contains(cmd, "-token "+tok) {
		t.Fatalf("应给出带令牌的启动命令，得 %q", cmd)
	}
}

// TestEgressHomeAccessControl 状态查询要登录；执行动作对登录用户开放，
// 但范围严格按"谁的机器 / 谁的号"切（v0.31.24）。
//
// 升级前这里是"普通用户一律 403"。现在**刻意不再断言 403** —— 那正是本版要改掉的
// 行为；取而代之的是断言"权限放行了，但范围没跟着放开"（见 egress_user_scope_test.go
// 的负向用例）。这里只钉住两端：认证边界（未登录 401）与两种视角的判据字段。
func TestEgressHomeAccessControl(t *testing.T) {
	_, a, mux, adminCookie, memberCookie := eaApp(t)

	if code, _ := eaReq(t, mux, "GET", "/api/egress/home", "", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("未登录读状态应 401，得 %d", code)
	}
	if code, _ := eaReq(t, mux, "GET", "/api/egress/home", "", memberCookie, ""); code != http.StatusOK {
		t.Fatalf("普通用户读状态应 200，得 %d", code)
	}
	// 普通用户拿到的 agent 命令必须是**他自己的令牌**（用它启动的 agent 才归属他）。
	// ⚠️ 这条是一个真实的越权面：命令若下发平台令牌，任何人都能拿它注册机器、
	// 伪造在线状态 —— 而"给我一行命令"恰恰是最不设防的功能。
	platformTok, err := a.egressAgentTokenGet()
	if err != nil {
		t.Fatalf("egressAgentTokenGet: %v", err)
	}
	_, raw := eaReq(t, mux, "GET", "/api/egress/home", "", memberCookie, "")
	v := eaJSON(t, raw)
	cmd, _ := v["agent_command"].(string)
	if cmd == "" {
		t.Fatal("普通用户应拿到 agent 命令（否则他无法把自己的机器接进来）")
	}
	if strings.Contains(cmd, platformTok) {
		t.Fatalf("普通用户的命令**不该**含平台令牌，得 %q", cmd)
	}
	if v["can_manage"] != true {
		t.Fatal("普通用户应能管理自己的出口（can_manage=true）")
	}
	if v["manage_scope"] != "self" {
		t.Fatalf("普通用户视角应为 self，得 %v", v["manage_scope"])
	}

	// 管理员：平台视角，且命令里必须仍是平台令牌（他管的是平台那台机器）。
	_, rawAdmin := eaReq(t, mux, "GET", "/api/egress/home", "", adminCookie, "")
	va := eaJSON(t, rawAdmin)
	if va["manage_scope"] != "platform" {
		t.Fatalf("管理员视角应为 platform，得 %v", va["manage_scope"])
	}
	if cmdA, _ := va["agent_command"].(string); !strings.Contains(cmdA, platformTok) {
		t.Fatalf("管理员的命令应用平台令牌，得 %q", cmdA)
	}

	// 执行动作：未登录 → 401（先过认证）；普通用户 → 不再 403，
	// 但"没有已接入的机器"是 400（引导他去跑 agent，而不是告诉他没权限）。
	if code, _ := eaReq(t, mux, "POST", "/api/egress/home/pull", "", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("未登录执行应 401，得 %d", code)
	}
	code, body := eaReq(t, mux, "POST", "/api/egress/home/pull", "", memberCookie, "")
	if code == http.StatusForbidden {
		t.Fatalf("普通用户执行出口动作不该再 403，得 %d %s", code, body)
	}
	if code != http.StatusBadRequest {
		t.Fatalf("没有已接入的机器时应 400，得 %d %s", code, body)
	}

	// 管理员：先让平台 agent 上线，再下发指令 → 200。
	if code, _ := eaReq(t, mux, "GET", "/api/agent/poll", "", "", platformTok); code != http.StatusOK {
		t.Fatalf("平台 agent poll 应 200，得 %d", code)
	}
	if code, body := eaReq(t, mux, "POST", "/api/egress/home/pull", "", adminCookie, ""); code != http.StatusOK {
		t.Fatalf("管理员 pull 应 200，得 %d %s", code, body)
	}
}

// TestApplyHomeEgressIdempotent 「应用」必须幂等，且只碰该碰的东西。
//
// 断言三件事：
//  1. 跑两次，线路池里仍**只有一条**（按 URL 认，不按名字）；
//  2. 全局默认出口与**自挂号**都被设上；
//  3. **平台号不动** —— 平台号跟随全局默认即可，给它写显式绑定会让
//     "站长换一次默认出口"变成"要逐个号改回来"。
func TestApplyHomeEgressIdempotent(t *testing.T) {
	hostAcc := &Account{
		UID: "ea-host", Nickname: "自挂号", Realm: "cn", AccessToken: "t1", RefreshToken: "r1",
		ExpiresAt: time.Now().Add(time.Hour).Unix(), OwnerUserID: 7, ConsumeGroup: "apply", PoolState: "in",
	}
	platAcc := &Account{
		UID: "ea-plat", Nickname: "平台号", Realm: "cn", AccessToken: "t2", RefreshToken: "r2",
		ExpiresAt: time.Now().Add(time.Hour).Unix(), ConsumeGroup: "apply", PoolState: "in",
	}
	store, a, _, _, _ := eaApp(t)
	for _, acc := range []*Account{hostAcc, platAcc} {
		if err := store.UpsertAccount(acc); err != nil {
			t.Fatalf("UpsertAccount: %v", err)
		}
	}

	for i := 0; i < 2; i++ {
		res, err := a.applyHomeEgress(egressBaseTunnelPort, egressScope{Platform: true}, false)
		if err != nil {
			t.Fatalf("第 %d 次 apply: %v", i+1, err)
		}
		if res.HostBound != 1 || res.HostTotal != 1 {
			t.Fatalf("第 %d 次：自挂号应 1/1，得 %d/%d", i+1, res.HostBound, res.HostTotal)
		}
	}

	proxies, err := store.ListEgressProxies()
	if err != nil {
		t.Fatalf("ListEgressProxies: %v", err)
	}
	if len(proxies) != 1 {
		t.Fatalf("重复应用后线路池应只有 1 条，得 %d", len(proxies))
	}
	// id **必须按 URL 派生**（确定性），不能是随机/时间派生。
	//
	// 这条是"幂等"的真正承重墙：只断言"两次调用后只有一条"是不够的 ——
	// 若 id 改成时间/随机派生，**同一秒内的两次调用仍会撞出同一个 id**，
	// 测试照样绿，但用户隔一分钟再点一次就会在池里堆出第二条同 URL 线路
	// （变异检验里正是这样漏过一次）。
	if proxies[0].ID != newEgressProxyID("http://127.0.0.1:11080") {
		t.Fatalf("线路 id 应按 URL 确定派生，得 %q", proxies[0].ID)
	}
	if !proxies[0].Enabled {
		t.Fatal("拉起即应启用线路（否则「通了但被停用」仍是个坑）")
	}
	def, _ := store.GetKV(kvEgressDefault)
	if def != "http://127.0.0.1:11080" {
		t.Fatalf("全局默认出口应指向本地线路，得 %q", def)
	}
	accs, _ := store.ListAccounts()
	byUID := map[string]string{}
	for _, acc := range accs {
		byUID[acc.UID] = acc.EgressProxy
	}
	// 平台视角 apply 的口径（v0.31.48）：这条线路**成为全局默认出口**时，
	// 自挂号写**空串**（= 跟随默认）而不是写死 URL。
	//
	// 两者"现在都走这条线"，但语义不同：写空算"没做过决定"，
	// 之后用户拉起自己的线路再点应用时会被正常接管；写死 URL 会被
	// 「非破坏式绑定」当成"已决定"跳过 —— 现场就是"拉起了、点了应用，号还在平台出口"。
	if byUID["ea-host"] != "" {
		t.Fatalf("平台 apply 后自挂号应为「跟随默认」（空串），得 %q", byUID["ea-host"])
	}
	if byUID["ea-plat"] != "" {
		t.Fatalf("平台号不该被写显式绑定（跟随全局默认即可），得 %q", byUID["ea-plat"])
	}
}

// TestEgressTunnelReachable 未监听的端口必须判为"不通"。
func TestEgressTunnelReachable(t *testing.T) {
	// 占一个端口再立刻释放，拿到一个几乎肯定没人监听的端口号。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	if egressTunnelReachable(port) {
		t.Fatalf("端口 %d 无监听，应判为不通", port)
	}
}

// TestEgressHomeApplyRefusedWhenTunnelDown 隧道不通时必须拒绝应用。
//
// 否则会把全局默认出口指向一条连不上的线路 —— 那比"没配"坏得多：
// 全部跟随默认的号会一起失败，而这看起来像"号全挂了"。
func TestEgressHomeApplyRefusedWhenTunnelDown(t *testing.T) {
	_, a, mux, adminCookie, _ := eaApp(t)
	// 换成一个没人监听的隧道端口。
	_ = a.store.SetKV(kvEgressAgentPorts, `{"proxy_port":1080,"tunnel_port":1}`)
	// 先让 agent 登记一次（v0.31.18 起 apply 要求"该机器已登记"，
	// 否则会先被 400「机器尚未登记」挡掉，测不到"隧道不通"这条路径）。
	tok, err := a.egressAgentTokenGet()
	if err != nil {
		t.Fatalf("egressAgentTokenGet: %v", err)
	}
	if c, raw := eaReq(t, mux, "GET", "/api/agent/poll", "", "", tok); c != http.StatusOK {
		t.Fatalf("agent poll 应 200，得 %d %s", c, raw)
	}

	code, raw := eaReq(t, mux, "POST", "/api/egress/home/apply", "", adminCookie, "")
	if code != http.StatusConflict {
		t.Fatalf("隧道不通应 409 拒绝，得 %d body=%.200s", code, raw)
	}
	if !strings.Contains(raw, "隧道") {
		t.Fatalf("拒绝文案应说明原因，得 %.200s", raw)
	}
	if def, _ := a.store.GetKV(kvEgressDefault); def != "" {
		t.Fatalf("被拒绝时不该改默认出口，得 %q", def)
	}
}
