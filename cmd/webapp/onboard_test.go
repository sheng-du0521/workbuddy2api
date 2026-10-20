// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

// ======================================================================
// 一键接入的回归网（v0.31.69，需求 P3）
//
// 这一层最容易"看起来对、实际写坏用户的配置"，所以用例分两类：
//  1. 生成物正确（脚本里真的是那些值、非法输入被拒、与手动配置同源）；
//  2. **真跑一遍**（Windows 上直接执行生成的 .bat，把 USERPROFILE 指到临时目录，
//     看它到底有没有写出一个能被解析的数组）。
//
// 第 2 类是重点：PowerShell 的 ConvertTo-Json 在**单元素数组**上会退化成对象，
// 写进去一个对象对客户端就是"模型列表解析失败" —— 而这种错生成物看起来完全正常。
// ======================================================================

// decodePSCommand 从 .bat 里取出 -EncodedCommand 的 base64 并解回脚本（UTF-16LE）。
func decodePSCommand(t *testing.T, bat string) string {
	t.Helper()
	var enc string
	for _, line := range strings.Split(bat, "\r\n") {
		if i := strings.Index(line, "-EncodedCommand "); i >= 0 {
			enc = strings.TrimSpace(line[i+len("-EncodedCommand "):])
			break
		}
	}
	if enc == "" {
		t.Fatalf(".bat 里没有 -EncodedCommand：\n%s", bat)
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatalf("base64 解码失败：%v", err)
	}
	if len(raw)%2 != 0 {
		t.Fatalf("UTF-16LE 字节数应为偶数，得 %d", len(raw))
	}
	u := make([]uint16, 0, len(raw)/2)
	for i := 0; i < len(raw); i += 2 {
		u = append(u, uint16(raw[i])|uint16(raw[i+1])<<8)
	}
	return string(utf16.Decode(u))
}

// O1：生成的脚本必须带着正确的值，且不把 key 暴露在明文命令行里。
func TestOnboardScriptCarriesPlan(t *testing.T) {
	plan := onboardPlan{
		Target:  onboardTargetWorkBuddy,
		BaseURL: "http://203.0.113.7:8780/v1",
		APIKey:  "sk-wb-abcdefghijklmnop",
		Model:   "glm-5.3",
		Name:    "WorkBuddy2API · glm-5.3",
	}
	name, bat := buildOnboardScript(plan, "windows")
	if name != "wb2api-onboard-workbuddy.bat" {
		t.Fatalf("文件名不对：%q", name)
	}
	if !strings.HasPrefix(bat, "@echo off") {
		t.Fatalf(".bat 应以 @echo off 开头")
	}
	// 明文 key 不该出现在 .bat 的命令行里（都在 base64 里）——
	// 这不是加密，只是让"随手打开看一眼"不至于把 key 直接摊在屏幕上。
	if strings.Contains(bat, "sk-wb-abcdefghijklmnop") {
		t.Fatal(".bat 不应包含明文 key")
	}
	ps := decodePSCommand(t, bat)
	for _, want := range []string{
		"203.0.113.7:8780/v1", "sk-wb-abcdefghijklmnop", "glm-5.3",
		"models.json", ".wb2api.bak", "UTF8Encoding($false)",
	} {
		if !strings.Contains(ps, want) {
			t.Errorf("脚本应包含 %q", want)
		}
	}
	// 单元素数组的坑必须显式处理（PowerShell ConvertTo-Json 会退化成对象）。
	if !strings.Contains(ps, "kept.Count -eq 1") {
		t.Error("脚本必须处理「只有一个元素时 ConvertTo-Json 退化成对象」")
	}
	// 单引号必须转义成两个，否则脚本语法直接坏掉。
	if got := psQuote("a'b"); got != "'a''b'" {
		t.Errorf("psQuote 转义不对：%s", got)
	}
	if got := shellQuote("a'b"); got != `'a'\''b'` {
		t.Errorf("shellQuote 转义不对：%s", got)
	}
}

// O2：模型名校验 —— 拒绝会让脚本/JSON 需要转义的一切。
func TestSanitizeModelName(t *testing.T) {
	bad := []string{"", "   ", strings.Repeat("a", onboardMaxModelLen+1),
		`a"b`, "a'b", `a\b`, "a`b", "a$b", "a\nb", "a\tb"}
	for _, s := range bad {
		if got, ok := sanitizeModelName(s); ok {
			t.Errorf("应拒绝 %q，却通过了（归一化为 %q）", s, got)
		}
	}
	good := []string{"glm-5.3", "gpt-4o-mini", "claude-3-5-sonnet-20241022", "  glm-5.3  "}
	for _, s := range good {
		if _, ok := sanitizeModelName(s); !ok {
			t.Errorf("应接受 %q", s)
		}
	}
	if got, _ := sanitizeModelName("  glm-5.3  "); got != "glm-5.3" {
		t.Errorf("应去掉首尾空白，得 %q", got)
	}
}

// newOnboardApp 装配一个带用户会话的 app（走真路由，含 userGuard）。
func newOnboardApp(t *testing.T) (*Store, *app, *http.ServeMux, string, int64) {
	t.Helper()
	store, _, a := newTestApp(t)
	mux := a.routes()
	u, err := store.CreateUser("onboard-user", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	raw := "sk-wb-onboard-test-key"
	enc, err := a.secret.Encrypt(raw)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	k, err := store.CreateAPIKey(u.ID, "t", HashAPIKey(raw), enc, "sk-wb", "", "", "")
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	tok := randomHex(32)
	if err := store.CreateSession(HashToken(tok), u.ID, sessionTTL); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return store, a, mux, tok, k.ID
}

// O3：接口的权限与拒绝语义。
func TestKeyOnboardEndpoint(t *testing.T) {
	store, _, mux, cookie, kid := newOnboardApp(t)
	defer store.Close()

	body := func(target, model string) string {
		b, _ := json.Marshal(map[string]any{"target": target, "model": model, "os": "windows"})
		return string(b)
	}
	do := func(id int64, target, model string) (int, string) {
		return eaReq(t, mux, "POST", fmt.Sprintf("/api/keys/%d/onboard", id), body(target, model), cookie, "")
	}

	code, resp := do(kid, "workbuddy", "glm-5.3")
	if code != 200 {
		t.Fatalf("应 200，得 %d：%s", code, resp)
	}
	var got struct {
		Filename string `json:"filename"`
		Script   string `json:"script"`
		BaseURL  string `json:"base_url"`
		Manual   struct {
			BaseURL string `json:"base_url"`
			Model   string `json:"model"`
			APIKey  string `json:"api_key"`
			JSON    string `json:"json"`
			Path    string `json:"path"`
		} `json:"manual"`
	}
	if err := json.Unmarshal([]byte(resp), &got); err != nil {
		t.Fatalf("响应不是 JSON：%v %s", err, resp)
	}
	if got.Filename == "" || got.Script == "" {
		t.Fatalf("应返回文件名与脚本：%s", resp)
	}
	if !strings.HasSuffix(got.BaseURL, "/v1") {
		t.Fatalf("base_url 应以 /v1 结尾，得 %q", got.BaseURL)
	}
	// 手动配置与脚本必须同源：两条路给出不同的值是最难解释的支持问题。
	if got.Manual.BaseURL != got.BaseURL || got.Manual.Model != "glm-5.3" || got.Manual.APIKey == "" {
		t.Fatalf("manual 与脚本不一致：%+v", got.Manual)
	}
	if !strings.Contains(got.Manual.JSON, `"useCustomProtocol": true`) {
		t.Fatalf("WorkBuddy 条目必须带 useCustomProtocol：%s", got.Manual.JSON)
	}
	if !strings.Contains(got.Manual.Path, "models.json") {
		t.Fatalf("应给出落点路径：%q", got.Manual.Path)
	}

	// CC Switch 目标：给的是 claude settings.json 的 env 形态。
	code, resp = do(kid, "ccswitch", "claude-3-5-sonnet")
	if code != 200 {
		t.Fatalf("ccswitch 应 200，得 %d：%s", code, resp)
	}
	if !strings.Contains(resp, "ANTHROPIC_BASE_URL") || !strings.Contains(resp, "ANTHROPIC_AUTH_TOKEN") {
		t.Fatalf("ccswitch 手动配置应含两个 env 键：%s", resp)
	}

	// 非法目标 / 非法模型名 / 不存在的 id → 全部被拒。
	if code, _ := do(kid, "../../../etc/passwd", "glm-5.3"); code != 400 {
		t.Errorf("非法 target 应 400，得 %d", code)
	}
	if code, _ := do(kid, "workbuddy", `a"b`); code != 400 {
		t.Errorf("含引号的模型名应 400，得 %d", code)
	}
	if code, _ := do(999999, "workbuddy", "m"); code != 404 {
		t.Errorf("不存在的 id 应 404，得 %d", code)
	}
	// 未登录一律拒绝（这个接口会回明文 key，不能有匿名口子）。
	if code, _ := eaReq(t, mux, "POST", fmt.Sprintf("/api/keys/%d/onboard", kid), body("workbuddy", "glm-5.3"), "", ""); code == 200 {
		t.Error("未登录不该拿到含明文 key 的脚本")
	}

	// 停用的 key 不许接入（接进去只会得到 401，用户会以为"接入失败"）。
	if _, err := store.db.Exec(`UPDATE api_keys SET status = 'banned' WHERE id = ?`, kid); err != nil {
		t.Fatal(err)
	}
	if code, resp := do(kid, "workbuddy", "glm-5.3"); code != 409 || !strings.Contains(resp, "停用") {
		t.Errorf("停用 key 应 409 且说明原因，得 %d %s", code, resp)
	}
}

// O4：**真跑一遍** Windows 脚本（把 USERPROFILE 指到临时目录）。
//
// 为什么必须真跑：ConvertTo-Json 对单元素数组退化成对象、PS 5.1 的 UTF8 带 BOM、
// `$list += $entry` 在单元素时的行为 —— 这三件事都能让"看起来对"的脚本写出一个
// 客户端读不了的文件，而静态断言一条都发现不了。
func TestOnboardScriptActuallyWritesArray(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("只在 Windows 上真跑 .bat（其它平台没有 powershell）")
	}
	home := t.TempDir()
	plan := onboardPlan{
		Target:  onboardTargetWorkBuddy,
		BaseURL: "http://203.0.113.7:8780/v1",
		APIKey:  "sk-wb-onboard-run-test",
		Model:   "glm-5.3",
		Name:    "WorkBuddy2API · glm-5.3",
	}
	// run 每次重写并执行：脚本结尾会 `del "%~f0"` 自删（它含明文 key，
	// 不该留在磁盘上）—— 所以不能复用同一个文件跑第二次。
	run := func(p onboardPlan) {
		t.Helper()
		_, bat := buildOnboardScript(p, "windows")
		batPath := filepath.Join(home, "onboard.bat")
		if err := os.WriteFile(batPath, []byte(bat), 0o644); err != nil {
			t.Fatal(err)
		}
		// 超时兜底：脚本里有 pause，若 stdin 行为与预期不同会挂住，不能把整包测试拖死。
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "cmd", "/c", batPath)
		// 关键：把用户目录搬到临时目录，脚本就会写到 <tmp>/.workbuddy/models.json，
		// 不碰本机真实配置。
		cmd.Env = append(os.Environ(), "USERPROFILE="+home)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("脚本执行失败：%v\n%s", err, out)
		}
	}

	modelsPath := filepath.Join(home, ".workbuddy", "models.json")

	// 第一次：目录不存在 → 必须自己建目录并写出**数组**。
	run(plan)
	raw, err := os.ReadFile(modelsPath)
	if err != nil {
		t.Fatalf("应已写出 models.json：%v", err)
	}
	if !json.Valid(raw) {
		t.Fatalf("写出的不是合法 JSON：%s", raw)
	}
	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err != nil {
		t.Fatalf("顶层必须是数组（单元素时最容易退化成对象）：%v\n%s", err, raw)
	}
	if len(arr) != 1 || arr[0]["id"] != "glm-5.3" {
		t.Fatalf("数组内容不对：%+v", arr)
	}
	if arr[0]["apiKey"] != "sk-wb-onboard-run-test" {
		t.Fatalf("apiKey 没写对：%+v", arr[0])
	}
	// 无 BOM：字节头不能是 EF BB BF（PowerShell 5.1 的 UTF8 *带* BOM）。
	if len(raw) >= 3 && raw[0] == 0xEF && raw[1] == 0xBB && raw[2] == 0xBF {
		t.Fatal("写出的文件带 UTF-8 BOM —— 多数 JSON 解析器不认，客户端会表现为「模型没出现」")
	}

	// 第二次：同一模型再跑一遍 → 必须**替换**而不是追加（否则客户端里两个同名）。
	run(plan)
	raw2, _ := os.ReadFile(modelsPath)
	var arr2 []map[string]any
	if err := json.Unmarshal(raw2, &arr2); err != nil {
		t.Fatalf("第二次写入后 JSON 坏了：%v\n%s", err, raw2)
	}
	if len(arr2) != 1 {
		t.Fatalf("重复接入应替换同 id 条目，实际 %d 条：%+v", len(arr2), arr2)
	}
	// 备份文件必须存在（用户的回滚路径）。
	if _, err := os.Stat(modelsPath + ".wb2api.bak"); err != nil {
		t.Fatalf("应生成备份文件：%v", err)
	}

	// 第三次：再加一个不同模型 → 应变成两条，且原有那条还在。
	plan2 := plan
	plan2.Model = "another-model"
	plan2.Name = "WorkBuddy2API · another-model"
	run(plan2)
	raw3, _ := os.ReadFile(modelsPath)
	var arr3 []map[string]any
	if err := json.Unmarshal(raw3, &arr3); err != nil {
		t.Fatalf("第三次写入后 JSON 坏了：%v\n%s", err, raw3)
	}
	if len(arr3) != 2 {
		t.Fatalf("应保留两条不同模型，实际 %d：%+v", len(arr3), arr3)
	}
	ids := map[string]bool{}
	for _, m := range arr3 {
		ids[fmt.Sprint(m["id"])] = true
	}
	if !ids["glm-5.3"] || !ids["another-model"] {
		t.Fatalf("两条模型都应存在：%+v", ids)
	}
}

// O5：CC Switch 目标的地址形态与真跑（v0.31.70）。
//
// 这条修的是 v0.31.69 的一个**实际不通**：当时把 `ANTHROPIC_BASE_URL` 写成
// `origin + "/v1"`，而 Anthropic 客户端自己会拼 `/v1/messages` ——
// 结果请求落到 `/v1/v1/messages`，404。配置写对了、端点不存在，面板不会报任何错。
func TestOnboardCCSwitchUsesOrigin(t *testing.T) {
	plan := onboardPlan{
		Target:  onboardTargetCCSwitch,
		BaseURL: "http://203.0.113.7:8780/v1",
		Origin:  "http://203.0.113.7:8780",
		APIKey:  "sk-wb-cc-test",
		Model:   "claude-3-5-sonnet",
		Name:    "WorkBuddy2API · claude-3-5-sonnet",
	}
	man := onboardManual(plan)
	if man["base_url"] != plan.Origin {
		t.Fatalf("CC Switch 的 base_url 必须是 origin（不带 /v1），得 %v", man["base_url"])
	}
	if !strings.Contains(man["json"].(string), plan.Origin) {
		t.Fatalf("手动 JSON 里应是 origin：%v", man["json"])
	}
	if strings.Contains(man["json"].(string), "/v1\"") {
		t.Fatalf("手动 JSON 里不该出现 /v1：%v", man["json"])
	}
	// WorkBuddy 那条路径反过来：**必须**带 /v1（它把 url 当 base 用）。
	wb := onboardManual(onboardPlan{
		Target: onboardTargetWorkBuddy, BaseURL: plan.BaseURL, Origin: plan.Origin,
		APIKey: plan.APIKey, Model: "glm-5.3", Name: "n",
	})
	if wb["base_url"] != plan.BaseURL {
		t.Fatalf("WorkBuddy 的 base_url 应带 /v1，得 %v", wb["base_url"])
	}
}

// O6：真跑 CC Switch 脚本 —— env 写进去、**其它键必须保留**。
//
// 保留键这条最容易漏：settings.json 里还有 permissions / hooks / model 等，
// 用一个"只写 env"的脚本会把用户的其它配置整个抹掉。
func TestOnboardCCSwitchScriptPreservesOtherKeys(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("只在 Windows 上真跑 .bat")
	}
	home := t.TempDir()
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	existing := `{"permissions":{"allow":["Bash(ls:*)"]},"model":"claude-3-5-sonnet"}`
	cfgPath := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(cfgPath, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	plan := onboardPlan{
		Target:  onboardTargetCCSwitch,
		BaseURL: "http://203.0.113.7:8780/v1",
		Origin:  "http://203.0.113.7:8780",
		APIKey:  "sk-wb-cc-run",
		Model:   "claude-3-5-sonnet",
		Name:    "n",
	}
	_, bat := buildOnboardScript(plan, "windows")
	batPath := filepath.Join(home, "onboard.bat")
	if err := os.WriteFile(batPath, []byte(bat), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "cmd", "/c", batPath)
	cmd.Env = append(os.Environ(), "USERPROFILE="+home)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("脚本执行失败：%v\n%s", err, out)
	}

	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("读回配置失败：%v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("写出的不是合法 JSON：%v\n%s", err, raw)
	}
	env, _ := cfg["env"].(map[string]any)
	if env["ANTHROPIC_BASE_URL"] != plan.Origin {
		t.Fatalf("base_url 没写对：%+v", cfg)
	}
	if env["ANTHROPIC_AUTH_TOKEN"] != "sk-wb-cc-run" {
		t.Fatalf("token 没写对：%+v", env)
	}
	// 关键：原有键必须还在。
	if _, ok := cfg["permissions"]; !ok {
		t.Fatalf("原有 permissions 被抹掉了：%+v", cfg)
	}
	if cfg["model"] != "claude-3-5-sonnet" {
		t.Fatalf("原有 model 丢了：%+v", cfg)
	}
	// 备份文件必须在（用户回滚路径）。
	if _, err := os.Stat(cfgPath + ".wb2api.bak"); err != nil {
		t.Fatalf("应生成备份：%v", err)
	}
}
