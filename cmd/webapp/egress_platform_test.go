// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 出口能力的**边界**回归测试（v0.31.53 收窄）。
//
// v0.31.24~v0.31.52 期间，出口代理对普通用户开放：用户能在控制台拉起自己的
// 机器、把自己的号分到自己的线路上。v0.31.53 产品决定把它收敛为**平台能力** ——
// 用户端不再具备任何代理使用能力，账号出口统一由管理后台分配。
//
// 所以这个文件的断言以**负向**为主：用户拿不到入口、拿不到接口、拿不到安装包；
// 正向只有一条对照（管理员仍然可用），外加一条最容易退化的不变量：
// **"应用"不再改任何账号的出口**（分配只能走 /api/admin/egress/assign）。
package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// usFixture 测试夹具：管理员 + 两个普通用户（各带一个代挂号）。
//
// 保留这份夹具是因为还有若干个与"视角"无关的出口测试复用它（agent 令牌、
// 线路粘性、rehome 等）。用户视角的能力已删，但"两个用户各有一个号"这个
// 数据形态仍然是验证**隔离**与**分配**的好素材。
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
			OwnerUserID: spec.owner.ID, ConsumeGroup: "us", PoolState: "",
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

// usKeysOf 机器表键（只在失败信息里用；reqguard_test.go 已占用 keysOf 这个名字）。
func usKeysOf(m map[string]*egressMachine) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestUserEgressEndpointsRetired 用户端四个代理端点一律 410，且带可读出口。
//
// 为什么必须 410 而不是删路由：404 与"路径写错"无法区分，老客户端会反复重试。
// 判断 410 时必须同时看 code —— 只看状态码的话，将来某条中间件把 410 改写成
// 别的码，测试仍然绿。
func TestUserEgressEndpointsRetired(t *testing.T) {
	f := usApp(t)
	cases := []struct{ method, path string }{
		{"POST", "/api/host/egress"},
		{"GET", "/api/host/egress/lines"},
		{"POST", "/api/host/egress/assign"},
		{"POST", "/api/host/egress/rebalance"},
	}
	for _, c := range cases {
		code, body := eaReq(t, f.mux, c.method, c.path, `{}`, f.cookieA, "")
		if code != http.StatusGone {
			t.Fatalf("%s %s 应 410，得 %d %s", c.method, c.path, code, body)
		}
		d := eaJSON(t, body)
		if d["code"] != "user_egress_removed" {
			t.Fatalf("%s %s 必须带 code=user_egress_removed，得 %.200s", c.method, c.path, body)
		}
		if msg, _ := d["error"].(string); !strings.Contains(msg, "管理员") {
			t.Fatalf("%s %s 的错误文案必须指出下一步（找管理员），得 %.200s", c.method, c.path, body)
		}
	}
}

// TestUserCannotUsePlatformEgressConsole 平台出口控制台对普通用户整体关闭。
//
// 五个端点（读状态 / 拉起 / 释放 / 应用 / 体检）全部要拦。少拦一个，
// "用户端不再具备任何代理使用能力"就是一句空话 —— 何况 apply 会设全局默认出口。
func TestUserCannotUsePlatformEgressConsole(t *testing.T) {
	f := usApp(t)
	cases := []struct{ method, path, body string }{
		{"GET", "/api/egress/home", ""},
		{"POST", "/api/egress/home/pull", `{}`},
		{"POST", "/api/egress/home/release", `{}`},
		{"POST", "/api/egress/home/apply", `{}`},
		{"POST", "/api/egress/home/repair", `{}`},
		{"GET", "/api/egress/home/install", ""},
	}
	for _, c := range cases {
		code, body := eaReq(t, f.mux, c.method, c.path, c.body, f.cookieA, "")
		if code != http.StatusForbidden {
			t.Fatalf("%s %s 对普通用户应 403，得 %d %s", c.method, c.path, code, body)
		}
	}
	// 执行端点必须给出 machine-readable 的 code（前端据此显示"平台能力"，
	// 而不是把"需要管理员权限"这类通用文案四处复用）。
	code, body := eaReq(t, f.mux, "POST", "/api/egress/home/apply", `{}`, f.cookieA, "")
	if code != http.StatusForbidden || !strings.Contains(body, "platform_only") {
		t.Fatalf("执行端点应 403 platform_only，得 %d %s", code, body)
	}
}

// TestUserPluginListHasNoEgress 普通用户的插件列表里没有出口插件。
//
// 入口层面的最后一道：即使前端换了新版本、即使老页面缓存着 DOM，
// 列表里没有这一条，用户就点不到。反过来，管理员必须**还能看到**它 ——
// 那是"平台直属出口"的唯一 UI 入口。
func TestUserPluginListHasNoEgress(t *testing.T) {
	f := usApp(t)
	code, body := eaReq(t, f.mux, "GET", "/api/plugins", "", f.cookieA, "")
	if code != http.StatusOK {
		t.Fatalf("用户插件列表应 200，得 %d %s", code, body)
	}
	if strings.Contains(body, pluginIDHostEgress) {
		t.Fatalf("普通用户插件列表里不该出现 %q：%.300s", pluginIDHostEgress, body)
	}

	code, body = eaReq(t, f.mux, "GET", "/api/admin/plugins", "", f.adminCookie, "")
	if code != http.StatusOK {
		t.Fatalf("管理端插件列表应 200，得 %d %s", code, body)
	}
	if !strings.Contains(body, pluginIDHostEgress) {
		t.Fatalf("管理端必须保留平台直属出口入口：%.300s", body)
	}
}

// TestPlatformApplySetsDefaultButTouchesNoAccount "应用"只立线路，不改账号。
//
// 这是 v0.31.53 最重要的一条不变量：出口分配只有一个写入口
// （/api/admin/egress/assign）。如果 apply 仍然顺手绑号，就等于在唯一闸门之外
// 又开了一个"点一下就批量搬号"的入口 —— 没有理由、没有审计，与产品要求冲突。
func TestPlatformApplySetsDefaultButTouchesNoAccount(t *testing.T) {
	f := usApp(t)
	f.app.egressVerify = func(string) (string, error) { return "198.51.100.9", nil }

	tokP, _ := f.app.egressAgentTokenGet()
	usPoll(t, f, tokP, "", "host-plat")
	mm := f.app.machinesLoad()[egressDefaultMachine]
	if mm == nil {
		t.Fatal("平台机器应落 default 桶")
	}

	res, err := f.app.applyHomeEgress(mm.Ports.TunnelPort, egressScope{Platform: true})
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
	// 关键断言：两个代挂号的出口**一个都没变**（仍是空 = 跟随默认，
	// 而"跟随默认"本身就是正确语义，不能被写成具体 URL）。
	for _, uid := range []string{"us-a-acct", "us-b-acct"} {
		acc, err := f.store.GetAccount(uid)
		if err != nil || acc == nil {
			t.Fatalf("读账号 %s: %v", uid, err)
		}
		if got := strings.TrimSpace(acc.EgressProxy); got != "" {
			t.Fatalf("apply 不得改账号出口：%s 变成了 %q（分配只能走管理端 assign）", uid, got)
		}
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

// TestAdminApplyStillNeedsTunnel 隧道不通时管理员也不能"应用"成功。
//
// 保留这条是因为它最容易被"收口"顺手放宽：应用一条不通的线路，比不应用更坏 ——
// 它会被设成全局默认出口，于是全平台从这个连不上的地址出去。
func TestAdminApplyStillNeedsTunnel(t *testing.T) {
	f := usApp(t)
	f.app.egressVerify = func(string) (string, error) { return "198.51.100.9", nil }
	tokP, _ := f.app.egressAgentTokenGet()
	usPoll(t, f, tokP, "", "host-plat")
	mm := f.app.machinesLoad()[egressDefaultMachine]
	if mm == nil {
		t.Fatal("平台机器应落 default 桶")
	}

	// 换成一个没人监听的端口（协议里"端口开着没人听"是最常见的失败形态）。
	m := f.app.machinesLoad()
	m[egressDefaultMachine].Ports.TunnelPort = 1
	if err := f.app.machinesSave(m); err != nil {
		t.Fatalf("machinesSave: %v", err)
	}
	code, body := eaReq(t, f.mux, "POST", "/api/egress/home/apply",
		`{"machine":"`+egressDefaultMachine+`"}`, f.adminCookie, "")
	if code != http.StatusConflict {
		t.Fatalf("隧道不通应 409，得 %d %s", code, body)
	}
}

// TestUserAgentTokenCannotConnect 已安装的用户 agent 不能再接入（v0.31.53）。
//
// 这是"用户端代理整体下线"里最容易被漏掉的一条：接口封了、页面删了，
// 但**已经装在那台电脑上的 agent 还在每 5 秒 poll 一次**。不拦它会有三个后果：
//
//	· 机器表里持续出现"看得见但没有线路"的幽灵记录，永远清不完；
//	· /api/agent/config 会把受限私钥发给它（凭证外流）；
//	· 日志被刷，真正的排障信息被淹。
//
// 所以判据必须是 **owner>0 一律 410**，而不是"看它有没有机器记录"。
func TestUserAgentTokenCannotConnect(t *testing.T) {
	f := usApp(t)
	userTok, _ := f.app.egressAgentTokenFor(f.userA.ID)

	for _, path := range []string{
		"/api/agent/poll?machine=pc-a&hostname=host-a",
		"/api/agent/config",
	} {
		code, body := eaReq(t, f.mux, "GET", path, "", "", userTok)
		if code != http.StatusGone {
			t.Fatalf("用户 agent 访问 %s 应 410，得 %d %s", path, code, body)
		}
		if !strings.Contains(body, "user_egress_removed") {
			t.Fatalf("必须带 code=user_egress_removed，得 %.200s", body)
		}
	}
	// report 也拒：它会把机器状态写进表里（幽灵记录的另一个来源）。
	code, body := eaReq(t, f.mux, "POST", "/api/agent/report?machine=pc-a",
		`{"hostname":"host-a","exit_ip":"1.1.1.1"}`, "", userTok)
	if code != http.StatusGone {
		t.Fatalf("用户 agent report 应 410，得 %d %s", code, body)
	}

	// 对照：平台令牌仍然正常接入（否则就变成"把管理员也锁死"）。
	platTok, _ := f.app.egressAgentTokenGet()
	if code, body := eaReq(t, f.mux, "GET", "/api/agent/poll", "", "", platTok); code != http.StatusOK {
		t.Fatalf("平台 agent 应正常接入，得 %d %s", code, body)
	}
}

// TestUserAgentWarnThrottled 拒绝日志必须节流。
//
// 现场：9915 的 agent 没删，每 5 秒 poll 一次 → 一天 1.7 万条 WARN，
// 真正的排障信息被淹。日志是给人看的，不是给"证明我在工作"用的。
func TestUserAgentWarnThrottled(t *testing.T) {
	egressUserAgentWarnMu.Lock()
	egressUserAgentWarnAt = map[int64]int64{}
	egressUserAgentWarnMu.Unlock()

	if !shouldLogUserAgentGone(77) {
		t.Fatal("首次拒绝必须留一条日志（那是「还有残留」的证据）")
	}
	if shouldLogUserAgentGone(77) {
		t.Fatal("同一用户 5 分钟内不该重复记（会刷满日志）")
	}
	if !shouldLogUserAgentGone(78) {
		t.Fatal("不同用户之间不该互相压制")
	}
}
