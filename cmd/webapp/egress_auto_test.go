// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 「一键安装 + 自动拉起」的回归测试（v0.31.25）。
//
// 这一版解决的问题是"用户点一下，剩下全自动"，而它背后是三件互相牵连的事：
//
//	① 信任根：用户机器凭什么能建到云端的隧道（不能用部署用的 root 私钥）；
//	② 自动传递：服务地址 / 令牌 / 密钥怎么到用户机器上（不能靠用户复制粘贴）；
//	③ 并发与失败：连点、多标签、装到一半失败时页面与后端各自怎么表现。
//
// 所以断言的重点不在"happy path 能跑通"，而在**约束**：
// 密钥被限制到什么程度、安装包里有没有不该有的东西、段满了会不会静默出错、
// 重复点击会不会把隧道拆了又建。
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// eaAutoApp 在 eaApp 基础上加一个普通用户，并把 authorized_keys 指到临时目录。
//
// ⚠️ 必须重定向 authorized_keys：这个功能会**真的写文件**，测试若不隔离，
// 就会往开发者本机真实的 ~/.ssh/authorized_keys 里塞一把密钥 —— 那是留后门。
func eaAutoApp(t *testing.T) (*Store, *app, *http.ServeMux, string, string, *User) {
	t.Helper()
	store, a, mux, adminCookie, _ := eaApp(t)
	u, err := store.CreateUser("auto-u", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	a.authKeysPath = filepath.Join(t.TempDir(), "authorized_keys")
	egressDedupReset()
	t.Cleanup(egressDedupReset)
	return store, a, mux, adminCookie, sessionFor(t, store, u), u
}

// egressDedupReset 清掉包级的去抖/单飞状态（测试之间会互相污染）。
func egressDedupReset() {
	egressDedupMu.Lock()
	egressPullAtMap = map[string]int64{}
	egressApplyAtMap = map[string]int64{}
	egressDedupMu.Unlock()
}

// hasSSHKeygen 本机是否有 ssh-keygen（没有则跳过依赖它的用例）。
func hasSSHKeygen(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("本机没有 ssh-keygen，跳过")
	}
}

// TestEgressPortSeg 端口分区：平台与用户各在自己的段内，互不重叠。
//
// 这不是"让数字好看"——用户的 SSH 密钥带 `permitlisten=` 白名单（= 它的段），
// 把用户机器分配到段外端口会让隧道**被 sshd 拒绝**，而现象只是"连不上"。
func TestEgressPortSeg(t *testing.T) {
	// ① 段划分：平台 11080-11089；用户 12000+(uid%1000)*10 .. +9。
	if base, span := egressPortSegOf(0); base != 11080 || span != 10 {
		t.Fatalf("平台段应为 11080/10，得 %d/%d", base, span)
	}
	if base, span := egressPortSegOf(7); base != 12070 || span != 10 {
		t.Fatalf("uid=7 的段应为 12070/10，得 %d/%d", base, span)
	}
	// ② 两个用户的段必须不相交（否则互相能抢对方的 permitlisten 端口）。
	pa := map[int]bool{}
	b, sp := egressPortSegOf(3)
	for i := 0; i < sp; i++ {
		pa[b+i] = true
	}
	b2, sp2 := egressPortSegOf(4)
	for i := 0; i < sp2; i++ {
		if pa[b2+i] {
			t.Fatalf("uid=3 与 uid=4 的段重叠于端口 %d", b2+i)
		}
	}

	// ③ 分配：同一 owner 连续两台不撞，且都落在段内。
	m := map[string]*egressMachine{}
	for i := 0; i < 3; i++ {
		ports, ok := egressAllocPorts(m, 7)
		if !ok {
			t.Fatalf("第 %d 台应能分配", i+1)
		}
		if ports.TunnelPort < 12070 || ports.TunnelPort > 12079 {
			t.Fatalf("用户机器端口应落在自己段内，得 %d", ports.TunnelPort)
		}
		m[egressMachineKey(7, string(rune('a'+i)))] = &egressMachine{OwnerUserID: 7, Ports: ports}
	}
	// ④ 平台分配走 1108x 段，与用户段不重叠。
	pp, ok := egressAllocPorts(m, 0)
	if !ok || pp.TunnelPort != 11080 {
		t.Fatalf("平台第一台应为 11080，得 %+v ok=%v", pp, ok)
	}

	// ⑤ 段满（10 台）→ 明确失败，而不是溢出到段外。
	full := map[string]*egressMachine{}
	for i := 0; i < egressUserPortSpan; i++ {
		ports, ok := egressAllocPorts(full, 7)
		if !ok {
			t.Fatalf("第 %d 台应能分配（段内有 %d 个）", i+1, egressUserPortSpan)
		}
		full[egressMachineKey(7, "m"+string(rune('a'+i)))] = &egressMachine{OwnerUserID: 7, Ports: ports}
	}
	if _, ok := egressAllocPorts(full, 7); ok {
		t.Fatal("段满后不该再分配成功（否则隧道会被 permitlisten 拒绝，而现象只是连不通）")
	}
}

// TestEgressAuthKeyRestrictions ⚠️ 本版最关键的安全断言。
//
// 用户拿到的 SSH 密钥必须是**受限**的：不能开 shell，且 `permitlisten` 只允许
// 它自己段内的端口 —— 否则一个用户就能监听平台的 11080，接住本应发给平台的流量
// （那些请求里带着 access_token）。
func TestEgressAuthKeyRestrictions(t *testing.T) {
	hasSSHKeygen(t)
	_, a, _, _, _, u := eaAutoApp(t)

	pem, err := a.egressSSHKeyEnsure(u.ID)
	if err != nil {
		t.Fatalf("egressSSHKeyEnsure: %v", err)
	}
	if !strings.Contains(pem, "PRIVATE KEY") {
		t.Fatalf("应返回私钥 PEM，得 %q", pem[:atMost(60, len(pem))])
	}

	raw, err := os.ReadFile(a.authKeysPath)
	if err != nil {
		t.Fatalf("读 authorized_keys: %v", err)
	}
	lines := []string{}
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, strings.TrimSpace(l))
		}
	}
	if len(lines) != 1 {
		t.Fatalf("应恰好写 1 行，得 %d：%v", len(lines), lines)
	}
	line := lines[0]
	for _, want := range []string{"restrict", "port-forwarding", "permitlisten=", "ssh-ed25519"} {
		if !strings.Contains(line, want) {
			t.Errorf("限制行应包含 %q，得 %q", want, line)
		}
	}
	// ⚠️ `command=` 是**必需项**（实测：`restrict` 不禁 exec，少了它就是一把 root
	// shell 的钥匙）。断言它存在且被顶替成无害命令，避免后人"清理"掉它。
	if !strings.Contains(line, `command="/bin/true"`) {
		t.Errorf("限制行必须顶替 exec（command=\"/bin/true\"）—— 只靠 restrict 不禁 shell：%q", line)
	}
	base, _ := egressPortSegOf(u.ID)
	if !strings.Contains(line, `permitlisten="127.0.0.1:`+strconv.Itoa(base)+`"`) {
		t.Errorf("应白名单自己的段首端口 %d，得 %q", base, line)
	}
	// **负向**：绝不能包含平台端口或别的段。
	if strings.Contains(line, "127.0.0.1:11080") {
		t.Errorf("用户密钥**不该**允许平台隧道端口 11080：%q", line)
	}
	if !strings.HasSuffix(line, " "+itoaComment(u.ID)) {
		t.Errorf("条目注释应为 %q（幂等替换的锚），得 %q", itoaComment(u.ID), line)
	}
	if strings.Contains(line, "no-port-forwarding") {
		t.Error("`restrict` 后面必须显式放回 port-forwarding，否则隧道建不起来")
	}

	// 幂等：再取一次不该新增行（重新生成会让正在跑的 agent 立刻失去隧道）。
	if _, err := a.egressSSHKeyEnsure(u.ID); err != nil {
		t.Fatalf("第二次 ensure: %v", err)
	}
	raw2, _ := os.ReadFile(a.authKeysPath)
	if n := strings.Count(string(raw2), "ssh-ed25519"); n != 1 {
		t.Fatalf("重复 ensure 不该新增条目，得 %d 条", n)
	}
}

// itoaComment authorized_keys 条目的注释（与实现同源，故意不抽公共函数：
// 测试自己拼一遍，才能在实现改了前缀时立刻发现）。
func itoaComment(uid int64) string {
	return "wb2api-u" + strconv.FormatInt(uid, 10)
}

// TestEgressInstallScriptHasNoSecretMaterial ⚠️ 安装脚本里**不能有密钥材料**。
//
// 安装包会进浏览器下载目录（常被长期留存），而私钥一旦落在那里就是一份散落的
// 凭证。正确做法是脚本只带服务地址与令牌，密钥由 agent 首次启动时自举取回。
func TestEgressInstallScriptHasNoSecretMaterial(t *testing.T) {
	_, a, _, adminCookie, _, u := eaAutoApp(t)
	_ = adminCookie

	req := httptest.NewRequest(http.MethodGet, "http://cloud.example:8780/api/egress/home/install", nil)
	// v0.31.53：安装包只面向平台机器（用户端自助代理已下线）。
	script, err := a.egressInstallScript(req, egressScope{Platform: true})
	if err != nil {
		t.Fatalf("egressInstallScript: %v", err)
	}
	tok, _ := a.egressAgentTokenFor(0)
	for _, want := range []string{
		"http://cloud.example:8780", // 服务地址自动带上
		tok,                         // 他自己的令牌
		"-config egress.json",       // 配置文件方式启动（避免命令行转义）
		`X-Agent-Token: %TOKEN%`,    // 下载 agent 二进制要带令牌
		"api/egress/agent/bundle",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("安装脚本应包含 %q", want)
		}
	}
	// 用户端下线后还多一条：脚本里**不该**出现任何"发给某个用户"的痕迹。
	if userTok, _ := a.egressAgentTokenFor(u.ID); strings.Contains(script, userTok) {
		t.Error("安装脚本不该携带用户令牌（用户端代理已下线）")
	}
	for _, bad := range []string{"PRIVATE KEY", "BEGIN OPENSSH", "permitlisten"} {
		if strings.Contains(script, bad) {
			t.Errorf("安装脚本**不该**包含密钥材料（%q）：一旦落在下载目录就是散落的凭证", bad)
		}
	}
	// 脚本内不能出现裸的未转义换行错乱：每条命令都应以 CRLF 结尾（cmd 要求）。
	if strings.Contains(script, "\n") && !strings.Contains(script, "\r\n") {
		t.Error("批处理必须是 CRLF 行尾，否则 cmd 解析会出问题")
	}

	// 平台视角：不生成用户密钥，改为提示放部署密钥。
	ps, err := a.egressInstallScript(req, egressScope{Platform: true})
	if err != nil {
		t.Fatalf("平台视角生成脚本: %v", err)
	}
	if !strings.Contains(ps, "-key ai.pem") {
		t.Error("平台视角的启动行应显式指向部署密钥 ai.pem")
	}
	if strings.Contains(ps, "PRIVATE KEY") {
		t.Error("平台脚本同样不该含密钥材料")
	}
}

// TestEgressAgentConfigBootstrap 自举接口的形状与边界。
func TestEgressAgentConfigBootstrap(t *testing.T) {
	hasSSHKeygen(t)
	_, a, _, _, _, u := eaAutoApp(t)
	req := httptest.NewRequest(http.MethodGet, "http://cloud.example:8780/api/agent/config", nil)

	cfg, err := a.egressAgentConfigFor(req, u.ID)
	if err != nil {
		t.Fatalf("egressAgentConfigFor: %v", err)
	}
	if cfg.PrivateKey == "" {
		t.Fatal("用户视角应下发私钥（否则 agent 自举后仍建不起隧道）")
	}
	if cfg.Server != "http://cloud.example:8780" {
		t.Errorf("server 应取请求的 origin，得 %q", cfg.Server)
	}
	if !strings.Contains(cfg.Note, "permitlisten") && !strings.Contains(cfg.Note, "只允许转发") {
		t.Errorf("note 该说明密钥是受限的，得 %q", cfg.Note)
	}

	// 平台视角：**不下发私钥** —— 管理员本机沿用部署密钥，多发一把只是多一个要管的。
	pc, err := a.egressAgentConfigFor(req, 0)
	if err != nil {
		t.Fatalf("平台视角: %v", err)
	}
	if pc.PrivateKey != "" {
		t.Error("平台视角不该下发用户私钥")
	}
	// 形状契约（对外 JSON 必须是 snake_case，双向断言）。
	body, _ := pc.JSON()
	for _, k := range []string{`"server"`, `"token"`, `"user"`} {
		if !strings.Contains(string(body), k) {
			t.Errorf("config JSON 应含 %s：%s", k, body)
		}
	}
	for _, bad := range []string{`"Server"`, `"PrivateKey"`, `"Token"`} {
		if strings.Contains(string(body), bad) {
			t.Errorf("config JSON 不该出现 Go 字段名键 %s：%s", bad, body)
		}
	}
}

// TestEgressAgentBundleMissingFile 交付文件缺失时要**点名缺什么、放哪儿**。
//
// 这是最常见的部署遗漏（只更新了 webapp，忘了带 egress-agent/ 目录）。
// 回一个 404 空响应会让用户看到"下载失败"[ERROR]，而管理员完全不知道要做什么。
func TestEgressAgentBundleMissingFile(t *testing.T) {
	_, a, mux, _, _, u := eaAutoApp(t)
	t.Setenv("WB2API_AGENT_DIR", t.TempDir()) // 空目录 = 模拟未随部署提供

	tok, _ := a.egressAgentTokenFor(u.ID)
	code, raw := eaReq(t, mux, "GET", "/api/egress/agent/bundle?part=agent", "", "", tok)
	if code != http.StatusNotFound {
		t.Fatalf("文件缺失应 404，得 %d %s", code, raw)
	}
	if !strings.Contains(raw, "egress-agent") {
		t.Fatalf("报错应点名目录 egress-agent/，得 %s", raw)
	}
	// 未带令牌 → 401（不能变成公开的 15MB 下载位）。
	if code, _ := eaReq(t, mux, "GET", "/api/egress/agent/bundle?part=agent", "", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("无令牌应 401，得 %d", code)
	}
}

// TestEgressPullDedup 连点不该让 agent 把隧道拆了又建。
//
// 重复下发会**刷新指令的 At**，而 agent 用 At 判断"执行过没有" —— At 一变它就
// stopAll 再重建一次。连点三下的表现是"点了反而更慢、还一闪一闪"。
func TestEgressPullDedup(t *testing.T) {
	f := usApp(t)
	// v0.31.53：pull 只剩平台视角，用平台机器（default 桶）+ 管理员会话。
	tokP, _ := f.app.egressAgentTokenGet()
	usPoll(t, f, tokP, "", "host-plat")
	key := egressDefaultMachine

	if code, body := eaReq(t, f.mux, "POST", "/api/egress/home/pull",
		`{"machine":"`+key+`"}`, f.adminCookie, ""); code != http.StatusOK {
		t.Fatalf("第一次 pull 应 200，得 %d %s", code, body)
	}
	at1 := f.app.machinesLoad()[key].CmdAt

	code, raw := eaReq(t, f.mux, "POST", "/api/egress/home/pull",
		`{"machine":"`+key+`"}`, f.adminCookie, "")
	if code != http.StatusOK {
		t.Fatalf("去抖期内重复 pull 应仍 200（幂等语义），得 %d %s", code, raw)
	}
	if !strings.Contains(raw, `"deduped":true`) {
		t.Fatalf("应标记为已合并，得 %s", raw)
	}
	if at2 := f.app.machinesLoad()[key].CmdAt; at2 != at1 {
		t.Fatalf("去抖期内不该刷新指令 At（否则 agent 会重建隧道）：%d → %d", at1, at2)
	}
	// 另一台机器不受影响（去抖是按机器而不是全局）。
	// v0.31.53：出口只剩平台机器，第二台的键就是 agent 自报的 id（owner=0）。
	usPoll(t, f, tokP, "plat-b", "host-plat2")
	codeB, rawB := eaReq(t, f.mux, "POST", "/api/egress/home/pull",
		`{"machine":"plat-b"}`, f.adminCookie, "")
	if codeB != http.StatusOK || strings.Contains(rawB, `"deduped":true`) {
		t.Fatalf("另一台机器不该被去抖，得 %d %s", codeB, rawB)
	}
}

// TestEgressApplySingleFlight 应用动作必须单飞。
//
// 一次应用要写几十个账号的出口；两个请求同时跑会互相覆盖结果，
// 而用户看到的只是"绑定 N 个"里的 N 跳来跳去。
func TestEgressApplySingleFlight(t *testing.T) {
	release, busy := egressApplyBegin("m1")
	if busy {
		t.Fatal("第一次应用不该 busy")
	}
	if _, busy2 := egressApplyBegin("m1"); !busy2 {
		t.Fatal("同一台机器并发第二次应用应被判忙")
	}
	if _, busyOther := egressApplyBegin("m2"); busyOther {
		t.Fatal("别的机器不该被牵连")
	}
	release()
	if _, busy3 := egressApplyBegin("m1"); busy3 {
		t.Fatal("释放后应能立刻再次应用（否则应用完还要干等）")
	}
}

// TestEgressApplyBusyReturns409 忙碌时接口要回 409 + 人话，而不是静默排队。
func TestEgressApplyBusyReturns409(t *testing.T) {
	f := usApp(t)
	tokP, _ := f.app.egressAgentTokenGet()
	usPoll(t, f, tokP, "", "host-plat")
	key := egressDefaultMachine
	// 手动占住"正在应用"。
	release, busy := egressApplyBegin(key)
	if busy {
		t.Fatal("占位失败")
	}
	defer release()

	code, raw := eaReq(t, f.mux, "POST", "/api/egress/home/apply",
		`{"machine":"`+key+`"}`, f.adminCookie, "")
	if code != http.StatusConflict {
		t.Fatalf("忙碌时应 409，得 %d %s", code, raw)
	}
	if !strings.Contains(raw, "正在应用中") {
		t.Fatalf("应给出人话提示，得 %s", raw)
	}
}

// TestEgressPollSegmentFullRejects 段满时 poll 明确报错（而不是给个 0 端口）。
func TestEgressPollSegmentFullRejects(t *testing.T) {
	f := usApp(t)
	// v0.31.53：用户 agent 已不能接入，段满改用**平台段**验证（同一套分配逻辑）。
	tokP, _ := f.app.egressAgentTokenGet()

	// 先把平台段占满（10 台）。
	m := f.app.machinesLoad()
	base, span := egressPortSegOf(0)
	for i := 0; i < span; i++ {
		m[egressMachineKey(0, "fill"+strconv.Itoa(i))] = &egressMachine{
			ID: "fill" + strconv.Itoa(i), OwnerUserID: 0,
			Ports: egressAgentPorts{ProxyPort: 1080, TunnelPort: base + i},
		}
	}
	if err := f.app.machinesSave(m); err != nil {
		t.Fatalf("machinesSave: %v", err)
	}
	// 第 11 台（新 machine id）→ 409。
	code, raw := eaReq(t, f.mux, "GET", "/api/agent/poll?machine=overflow&hostname=h", "", "", tokP)
	if code != http.StatusConflict {
		t.Fatalf("段满应 409，得 %d %s", code, raw)
	}
	if !strings.Contains(raw, "端口已用完") {
		t.Fatalf("报错应说清是端口用完，得 %s", raw)
	}
}

// TestEgressDedupStateIsolatable 去抖状态可重置（测试隔离用，同时也是文档）。
func TestEgressDedupStateIsolatable(t *testing.T) {
	egressPullDeduped("k")
	if !egressPullDeduped("k") {
		t.Fatal("同键应被去抖")
	}
	egressDedupReset()
	if egressPullDeduped("k") {
		t.Fatal("重置后不该再去抖")
	}
	var cfg struct {
		Note string `json:"note"`
	}
	_ = json.Unmarshal([]byte(`{"note":"x"}`), &cfg)
}

// atMost 小工具（Go 1.21 起有内建 min；显式写一个避免与内建重名）。
func atMost(a, b int) int {
	if a < b {
		return a
	}
	return b
}
