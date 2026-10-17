// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 出口「权限下沉」的回归测试（v0.31.24）。
//
// 背景：三个执行端点（pull / release / apply）此前挂 adminGuard —— 普通用户
// 在控制台点「一键拉起」只会得到 403，出口这件事只能由管理员代劳。v0.31.24 把它
// 放开给登录用户，风险也随之集中在一句话上：**放开了权限，范围会不会一起放开**。
//
// 所以这个文件里**绝大多数断言是负向的**：看不到别人的机器、动不了别人的机器、
// 改不了全局默认出口、碰不到别人的号。正向断言只是给出"它确实能用"的对照。
package main

import (
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// usFixture 测试夹具：管理员 + 两个普通用户（各带一个代挂号）。
type usFixture struct {
	store       *Store
	app         *app
	mux         *http.ServeMux
	adminCookie string
	cookieA     string
	cookieB     string
	userA       *User
	userB       *User
}

func usApp(t *testing.T) usFixture {
	t.Helper()
	store, a, mux, adminCookie, _ := eaApp(t)
	ua, err := store.CreateUser("us-a", "x")
	if err != nil {
		t.Fatalf("CreateUser A: %v", err)
	}
	ub, err := store.CreateUser("us-b", "x")
	if err != nil {
		t.Fatalf("CreateUser B: %v", err)
	}
	for _, spec := range []struct {
		uid   string
		owner *User
	}{{"us-a-acct", ua}, {"us-b-acct", ub}} {
		acc := &Account{
			UID: spec.uid, Nickname: spec.uid, Realm: "cn",
			AccessToken: "t", RefreshToken: "r",
			ExpiresAt:   time.Now().Add(time.Hour).Unix(),
			OwnerUserID: spec.owner.ID, ConsumeGroup: "us", PoolState: "in",
		}
		if err := store.UpsertAccount(acc); err != nil {
			t.Fatalf("UpsertAccount %s: %v", spec.uid, err)
		}
	}
	return usFixture{
		store: store, app: a, mux: mux, adminCookie: adminCookie,
		cookieA: sessionFor(t, store, ua), cookieB: sessionFor(t, store, ub),
		userA: ua, userB: ub,
	}
}

// usPoll 让某个令牌拿着 machine id 走一次 poll（= 一台机器接入云端）。
func usPoll(t *testing.T, f usFixture, tok, machine, host string) {
	t.Helper()
	code, body := eaReq(t, f.mux, "GET",
		"/api/agent/poll?machine="+machine+"&hostname="+host, "", "", tok)
	if code != http.StatusOK {
		t.Fatalf("poll(machine=%s) 应 200，得 %d %s", machine, code, body)
	}
}

// machineIDs 取某个视角下可见的机器 id 集合。
func usVisibleIDs(t *testing.T, f usFixture, cookie string) map[string]bool {
	t.Helper()
	code, raw := eaReq(t, f.mux, "GET", "/api/egress/home", "", cookie, "")
	if code != http.StatusOK {
		t.Fatalf("读出口状态应 200，得 %d %s", code, raw)
	}
	out := map[string]bool{}
	for _, m := range (eaJSON(t, raw)["machines"]).([]any) {
		if mm, ok := m.(map[string]any); ok {
			out[mm["id"].(string)] = true
		}
	}
	return out
}

// TestUserMachineOwnershipNamespace 机器桶按 owner 命名空间隔离。
//
// ⚠️ 这条是本版最容易被忽略的防线：机器 id 是 agent 自己生成、随查询参数上行的
// —— 构造成本为零。若机器表只用 machine id 作键，用户只要**猜到或复用一个别人的
// id**，就能把自己的 agent 顶到对方的桶里（或反过来把对方顶掉）。
// 带上 owner 前缀后，"别人的 id" 落在自己的命名空间内，物理上碰不到对方。
func TestUserMachineOwnershipNamespace(t *testing.T) {
	f := usApp(t)
	tokA, err := f.app.egressAgentTokenFor(f.userA.ID)
	if err != nil {
		t.Fatalf("token A: %v", err)
	}
	tokB, _ := f.app.egressAgentTokenFor(f.userB.ID)
	if tokA == tokB {
		t.Fatal("两个用户必须拿到不同的令牌（否则根本分不清机器归谁）")
	}

	// 两个用户**故意用同一个 machine id**（模拟"复用别人的 id"）。
	usPoll(t, f, tokA, "same-id", "pc-a")
	usPoll(t, f, tokB, "same-id", "pc-b")

	m := f.app.machinesLoad()
	keyA := egressMachineKey(f.userA.ID, "same-id")
	keyB := egressMachineKey(f.userB.ID, "same-id")
	if keyA == keyB {
		t.Fatalf("两个用户的机器键不该相同：%q", keyA)
	}
	ma, okA := m[keyA]
	mb, okB := m[keyB]
	if !okA || !okB {
		t.Fatalf("两台机器都应各自登记，得 A=%v B=%v（表内键=%v）", okA, okB, usKeysOf(m))
	}
	if ma.Hostname != "pc-a" || mb.Hostname != "pc-b" {
		t.Fatalf("同 id 的两台机器被串了：A=%q B=%q", ma.Hostname, mb.Hostname)
	}
	if ma.OwnerUserID != f.userA.ID || mb.OwnerUserID != f.userB.ID {
		t.Fatalf("归属写错：A=%d(期望 %d) B=%d(期望 %d)",
			ma.OwnerUserID, f.userA.ID, mb.OwnerUserID, f.userB.ID)
	}
	if !strings.HasPrefix(keyA, "u") || !strings.Contains(keyA, ":") {
		t.Fatalf("用户机器的键应带 owner 前缀，得 %q", keyA)
	}

	// 用户令牌**绝不允许**落到平台 default 桶：它的 rawID 为空时键是 u<uid>:default。
	usPoll(t, f, tokA, "", "")
	if _, ok := f.app.machinesLoad()[egressDefaultMachine]; ok {
		t.Fatal("用户令牌不带 machine 时不该占用平台 default 桶（那会把管理员的机器顶掉）")
	}
	if _, ok := f.app.machinesLoad()[egressMachineKey(f.userA.ID, "")]; !ok {
		t.Fatal("用户令牌不带 machine 时应落到自己的 u<uid>:default")
	}
}

// TestUserSeesOnlyOwnMachines 视图按 owner 过滤：用户看不到别人的机器。
//
// 这不是"界面整洁"问题：机器项里带着 hostname / 出口 IP / 隧道端口 ——
// 一个用户看到别人的机器，等于看到别人家里那台电脑的出口 IP 与在线规律。
func TestUserSeesOnlyOwnMachines(t *testing.T) {
	f := usApp(t)
	tokA, _ := f.app.egressAgentTokenFor(f.userA.ID)
	tokB, _ := f.app.egressAgentTokenFor(f.userB.ID)
	platformTok, _ := f.app.egressAgentTokenGet()

	usPoll(t, f, tokA, "pc-a", "host-a")
	usPoll(t, f, tokB, "pc-b", "host-b")
	usPoll(t, f, platformTok, "", "host-plat") // 平台那台（default 桶）

	seenA := usVisibleIDs(t, f, f.cookieA)
	seenB := usVisibleIDs(t, f, f.cookieB)
	seenAdmin := usVisibleIDs(t, f, f.adminCookie)

	if seenA[egressMachineKey(f.userB.ID, "pc-b")] || seenA[egressDefaultMachine] {
		t.Fatalf("用户 A 不该看到别人/平台的机器，得 %v", seenA)
	}
	if !seenA[egressMachineKey(f.userA.ID, "pc-a")] {
		t.Fatalf("用户 A 应看到自己的机器，得 %v", seenA)
	}
	if seenB[egressMachineKey(f.userA.ID, "pc-a")] {
		t.Fatalf("用户 B 不该看到 A 的机器，得 %v", seenB)
	}
	if len(seenAdmin) != 3 {
		t.Fatalf("管理员应看到全部 3 台，得 %v", seenAdmin)
	}
	// manage_scope 是前端文案唯一的判据（用户视角不能说"全局默认出口"）。
	_, raw := eaReq(t, f.mux, "GET", "/api/egress/home", "", f.cookieA, "")
	if got := eaJSON(t, raw)["manage_scope"]; got != "self" {
		t.Fatalf("用户视角 manage_scope 应为 self，得 %v", got)
	}
	// 用户视角**不下发**全局默认出口：那个开关不属于他，显示出来只会误导。
	if got, _ := eaJSON(t, raw)["default_egress"].(string); got != "" {
		t.Fatalf("用户视角不该看到全局默认出口，得 %q", got)
	}
}

// TestUserCannotPullOthersMachine 用户动不了别人的机器。
//
// ⚠️ 断言的是 **404 + 同一句文案**：把"这台不存在"与"这台不是你的"分开说，
// 等于给出一个能枚举别人 machine id 的探针（"这个 id 报的是后者 → 它存在"）。
func TestUserCannotPullOthersMachine(t *testing.T) {
	f := usApp(t)
	tokA, _ := f.app.egressAgentTokenFor(f.userA.ID)
	usPoll(t, f, tokA, "pc-a", "host-a")
	keyA := egressMachineKey(f.userA.ID, "pc-a")

	// ① B 对 A 的机器下发指令 → 拒绝。
	code, body := eaReq(t, f.mux, "POST", "/api/egress/home/pull",
		`{"machine":"`+keyA+`"}`, f.cookieB, "")
	if code != http.StatusNotFound {
		t.Fatalf("B 操作 A 的机器应 404，得 %d %s", code, body)
	}
	if !strings.Contains(body, "不属于你") {
		t.Fatalf("拒绝文案应把\"不存在\"与\"不是你的\"合并成一句，得 %s", body)
	}
	// ② B 的 apply 同样被拒。
	if code, body := eaReq(t, f.mux, "POST", "/api/egress/home/apply",
		`{"machine":"`+keyA+`"}`, f.cookieB, ""); code != http.StatusBadRequest {
		t.Fatalf("B 应用 A 的机器应 400，得 %d %s", code, body)
	}
	// ③ B 自己的机器不存在 → "尚未登记"（不是 403：权限已经放行了）。
	if code, body := eaReq(t, f.mux, "POST", "/api/egress/home/pull",
		`{"machine":"`+keyA+`"}`, f.cookieB, ""); code == http.StatusForbidden {
		t.Fatalf("用户执行出口动作不该再 403（v0.31.24 已下沉），得 %s", body)
	}

	// ④ A 操作自己的机器 → 成功，且**B 的 agent 取不到**这条指令（定向）。
	if code, body := eaReq(t, f.mux, "POST", "/api/egress/home/pull",
		`{"machine":"`+keyA+`"}`, f.cookieA, ""); code != http.StatusOK {
		t.Fatalf("A 操作自己的机器应 200，得 %d %s", code, body)
	}
	_, pollA := eaReq(t, f.mux, "GET", "/api/agent/poll?machine=pc-a", "", "", tokA)
	if got := eaJSON(t, pollA)["action"]; got != "start" {
		t.Fatalf("A 的 agent 应取到 start，得 %v", got)
	}
	tokB, _ := f.app.egressAgentTokenFor(f.userB.ID)
	_, pollB := eaReq(t, f.mux, "GET", "/api/agent/poll?machine=pc-a", "", "", tokB)
	if got := eaJSON(t, pollB)["action"]; got != "none" {
		t.Fatalf("B 的 agent（即便报同一个 machine id）不该取到 A 的指令，得 %v", got)
	}
}

// TestUserApplyDoesNotSetGlobalDefault ⚠️ 本版的核心边界。
//
// 用户 apply 时**不设全局默认出口**，且**只绑自己名下的号**。
// 理由不是"配置问题"而是越权：全局默认出口同时决定所有平台号与所有"跟随默认"的
// 账号从哪出去 —— 用户提供的代理是我们控制不了的第三方，只应承载它主人的号。
func TestUserApplyDoesNotSetGlobalDefault(t *testing.T) {
	f := usApp(t)
	const platURL = "http://10.9.9.9:8080"
	if err := f.store.SetKV(kvEgressDefault, platURL); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	tokA, _ := f.app.egressAgentTokenFor(f.userA.ID)
	usPoll(t, f, tokA, "pc-a", "host-a")
	keyA := egressMachineKey(f.userA.ID, "pc-a")
	mm := f.app.machinesLoad()[keyA]

	res, err := f.app.applyHomeEgress(mm.Ports.TunnelPort, egressScope{UserID: f.userA.ID}, false)
	if err != nil {
		t.Fatalf("用户 apply: %v", err)
	}
	if res.HostBound != 1 || res.HostTotal != 1 {
		t.Fatalf("用户 apply 应只覆盖他自己的 1 个代挂号，得 %d/%d", res.HostBound, res.HostTotal)
	}
	if res.DefaultOK {
		t.Fatal("用户 apply **绝不能**报告已设全局默认出口")
	}

	// ① 全局默认出口必须原样不动。
	if def, _ := f.store.GetKV(kvEgressDefault); def != platURL {
		t.Fatalf("用户 apply 改动了全局默认出口：期望 %q，得 %q", platURL, def)
	}
	// ② 只有自己的号被绑，别人的号一个都不碰。
	url := egressHomeURL(mm.Ports.TunnelPort)
	accs, _ := f.store.ListAccounts()
	for _, acc := range accs {
		switch acc.UID {
		case "us-a-acct":
			if acc.EgressProxy != url {
				t.Fatalf("A 自己的号应被绑到 %q，得 %q", url, acc.EgressProxy)
			}
		case "us-b-acct":
			if strings.TrimSpace(acc.EgressProxy) != "" {
				t.Fatalf("**别人的号被改了**：us-b-acct 的出口变成 %q", acc.EgressProxy)
			}
		}
	}
	// ③ 线路归属必须是 A（管理后台靠它区分"这条线是谁接进来的"）。
	var hit *EgressProxy
	if proxies, err := f.store.ListEgressProxies(); err == nil {
		for _, p := range proxies {
			if strings.TrimSpace(p.URL) == url {
				hit = p
			}
		}
	}
	if hit == nil {
		t.Fatal("用户拉起的线路应登记进线路池（否则管理员看不到它，也就无从分配）")
	}
	if hit.OwnerUserID != f.userA.ID {
		t.Fatalf("线路归属应为用户 A(%d)，得 %d", f.userA.ID, hit.OwnerUserID)
	}
	if !strings.Contains(hit.Name, f.userA.Username) {
		t.Fatalf("线路名应带上用户名（管理后台一眼看出是谁接的），得 %q", hit.Name)
	}
	// ④ 用户视角的 applied 判据 = "我的号全绑上了"，与"是不是全局默认"无关。
	_, raw := eaReq(t, f.mux, "GET", "/api/egress/home", "", f.cookieA, "")
	for _, m := range (eaJSON(t, raw)["machines"]).([]any) {
		if mv, ok := m.(map[string]any); ok && mv["id"] == keyA {
			if mv["applied"] != true {
				t.Fatalf("自己的号已全绑上，applied 应为 true，得 %v", mv["applied"])
			}
			if mv["host_total"] != float64(1) {
				t.Fatalf("host_total 应只数他自己的号（1），得 %v", mv["host_total"])
			}
		}
	}
}

// TestPlatformApplyStillSetsGlobalDefault 平台视角行为保持（回归护栏）。
//
// 权限下沉最容易的副作用是"顺手把管理员那条路也改了"。这条钉住升级前语义：
// 管理员 apply 仍设全局默认出口、仍绑全部自挂号。
func TestPlatformApplyStillSetsGlobalDefault(t *testing.T) {
	f := usApp(t)
	tokP, _ := f.app.egressAgentTokenGet()
	usPoll(t, f, tokP, "", "host-plat")
	mm := f.app.machinesLoad()[egressDefaultMachine]
	if mm == nil {
		t.Fatal("平台机器应落 default 桶")
	}

	res, err := f.app.applyHomeEgress(mm.Ports.TunnelPort, egressScope{Platform: true}, false)
	if err != nil {
		t.Fatalf("管理员 apply: %v", err)
	}
	url := egressHomeURL(mm.Ports.TunnelPort)
	if !res.DefaultOK {
		t.Fatal("管理员 apply 应设全局默认出口")
	}
	if def, _ := f.store.GetKV(kvEgressDefault); def != url {
		t.Fatalf("全局默认出口应指向 %q，得 %q", url, def)
	}
	if res.HostBound != 2 || res.HostTotal != 2 {
		t.Fatalf("管理员 apply 应绑全部 2 个自挂号，得 %d/%d", res.HostBound, res.HostTotal)
	}
	// 平台拉起的线路**不带归属**（owner=0），线路池据此区分"平台登记"与"用户提供的"。
	var hit *EgressProxy
	if proxies, err := f.store.ListEgressProxies(); err == nil {
		for _, p := range proxies {
			if strings.TrimSpace(p.URL) == url {
				hit = p
			}
		}
	}
	if hit == nil || hit.OwnerUserID != 0 {
		t.Fatalf("平台线路的归属应为 0，得 %+v", hit)
	}
}

// TestUserAgentTokenCannotBeForged 令牌不可伪造、不可跨身份复用。
func TestUserAgentTokenCannotBeForged(t *testing.T) {
	f := usApp(t)
	real, _ := f.app.egressAgentTokenFor(f.userB.ID)

	// ① 形如用户令牌但内容不对 → 401。
	if code, _ := eaReq(t, f.mux, "GET", "/api/agent/poll", "", "", "ag999-"+strings.Repeat("0", 64)); code != http.StatusUnauthorized {
		t.Fatalf("伪造令牌应 401，得 %d", code)
	}
	// ② 把真实令牌截断/改写 → 401。
	if code, _ := eaReq(t, f.mux, "GET", "/api/agent/poll", "", "", real[:len(real)-1]); code != http.StatusUnauthorized {
		t.Fatalf("篡改令牌应 401，得 %d", code)
	}
	// ③ 平台令牌**不是**用户令牌：它落的机器 owner=0（default 命名空间），
	//    不会因为"某个用户先注册了这台机器"就被抢走。
	platformTok, _ := f.app.egressAgentTokenGet()
	usPoll(t, f, platformTok, "shared-name", "host-plat")
	m := f.app.machinesLoad()
	if _, ok := m["shared-name"]; !ok {
		t.Fatalf("平台令牌应落平台命名空间，得键 %v", usKeysOf(m))
	}
	if _, ok := m[egressMachineKey(f.userA.ID, "shared-name")]; ok {
		t.Fatal("平台令牌不该被算进用户 A 的命名空间")
	}
}

// TestUserEgressNeedsTunnel 隧道不通时拒绝应用（用户视角同样受这条约束）。
//
// 保留这条是因为它最容易被"放宽权限"顺手放宽：应用一条不通的线路，
// 比不应用更坏 —— 号会被绑到一个永远连不上的出口。
func TestUserEgressNeedsTunnel(t *testing.T) {
	f := usApp(t)
	tokA, _ := f.app.egressAgentTokenFor(f.userA.ID)
	usPoll(t, f, tokA, "pc-a", "host-a")
	keyA := egressMachineKey(f.userA.ID, "pc-a")

	// 换成一个没人监听的端口（协议里"端口开着没人听"是最常见的失败形态）。
	m := f.app.machinesLoad()
	m[keyA].Ports.TunnelPort = 1
	if err := f.app.machinesSave(m); err != nil {
		t.Fatalf("machinesSave: %v", err)
	}
	code, body := eaReq(t, f.mux, "POST", "/api/egress/home/apply",
		`{"machine":"`+keyA+`"}`, f.cookieA, "")
	if code != http.StatusConflict {
		t.Fatalf("隧道不通应 409，得 %d %s", code, body)
	}

	// 起一个真的监听 → 同一次调用应当放行（对照：证明上面不是因为别的错）。
	ln, err := net.Listen("tcp", "127.0.0.1:11091")
	if err != nil {
		t.Skipf("11091 被占用，跳过本用例: %v", err)
	}
	defer ln.Close()
	m = f.app.machinesLoad()
	m[keyA].Ports.TunnelPort = 11091
	if err := f.app.machinesSave(m); err != nil {
		t.Fatalf("machinesSave: %v", err)
	}
	if code, body := eaReq(t, f.mux, "POST", "/api/egress/home/apply",
		`{"machine":"`+keyA+`"}`, f.cookieA, ""); code != http.StatusOK {
		t.Fatalf("隧道连通后应 200，得 %d %s", code, body)
	}
}

// usKeysOf 机器表键（只在失败信息里用；reqguard_test.go 已占用 keysOf 这个名字）。
func usKeysOf(m map[string]*egressMachine) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
