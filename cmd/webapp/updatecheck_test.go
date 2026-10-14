// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// withTestClient 把出站客户端换成**显式禁代理**的实例。
//
// 必须禁代理：宿主环境可能注入 http_proxy（指向本机代理端口），虽然 Go 对 loopback
// 默认不走代理，但显式关掉能让"测试为什么连不上"这类问题少一个变量。
func withTestClient(t *testing.T) {
	t.Helper()
	old := updateHTTPClient
	updateHTTPClient = &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{Proxy: nil},
	}
	t.Cleanup(func() { updateHTTPClient = old })
}

func newUpdateTestApp(t *testing.T) *app {
	t.Helper()
	st, err := OpenStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &app{store: st}
}

// ---------------------------------------------------------------- parseSemver

func TestParseSemver(t *testing.T) {
	cases := []struct {
		in    string
		ok    bool
		major int
		minor int
		patch int
		pre   string
	}{
		{"1.2.3", true, 1, 2, 3, ""},
		{"v1.2.3", true, 1, 2, 3, ""},
		{"V1.2.3", true, 1, 2, 3, ""},
		{"1.2", true, 1, 2, 0, ""},
		{"1", true, 1, 0, 0, ""},
		{" 1.2.3 ", true, 1, 2, 3, ""},
		{"1.2.3+build.7", true, 1, 2, 3, ""},
		{"1.2.3-rc.1", true, 1, 2, 3, "rc.1"},
		{"1.2.3-rc.1+build", true, 1, 2, 3, "rc.1"},
		// 畸形：一律 ok=false，交给调用方降级（不猜、不报错）
		{"", false, 0, 0, 0, ""},
		{"dev", false, 0, 0, 0, ""},
		{"2026.10.13.1", false, 0, 0, 0, ""},
		{"1.2.x", false, 0, 0, 0, ""},
		{"-1.2.3", false, 0, 0, 0, ""},
		{"1.2.3-", false, 0, 0, 0, ""},
	}
	for _, c := range cases {
		got := parseSemver(c.in)
		if got.ok != c.ok {
			t.Errorf("parseSemver(%q).ok = %v, want %v", c.in, got.ok, c.ok)
			continue
		}
		if !c.ok {
			continue
		}
		if got.major != c.major || got.minor != c.minor || got.patch != c.patch {
			t.Errorf("parseSemver(%q) = %d.%d.%d, want %d.%d.%d",
				c.in, got.major, got.minor, got.patch, c.major, c.minor, c.patch)
		}
		if wantPre := c.pre; wantPre != "" && len(got.pre) == 0 {
			t.Errorf("parseSemver(%q) 应解析出预发布标识 %q", c.in, wantPre)
		}
	}
}

// TestCompareSemverSpecChain 用 SemVer 2.0.0 规范里给出的**官方示例链**做黄金测试。
//
// 这条链专门覆盖两个最容易写错的点：
//   - 预发布版本**低于**同号正式版（1.0.0-rc.1 < 1.0.0）；
//   - 预发布逐段比较时数字段按**数值**而非字典序（beta.2 < beta.11，字典序会判反）。
func TestCompareSemverSpecChain(t *testing.T) {
	chain := []string{
		"1.0.0-alpha",
		"1.0.0-alpha.1",
		"1.0.0-alpha.beta",
		"1.0.0-beta",
		"1.0.0-beta.2",
		"1.0.0-beta.11",
		"1.0.0-rc.1",
		"1.0.0",
	}
	for i := 0; i+1 < len(chain); i++ {
		a, b := parseSemver(chain[i]), parseSemver(chain[i+1])
		if !a.ok || !b.ok {
			t.Fatalf("解析失败: %q / %q", chain[i], chain[i+1])
		}
		if got := compareSemver(a, b); got != -1 {
			t.Errorf("compareSemver(%s, %s) = %d, want -1（规范规定前者更低）", chain[i], chain[i+1], got)
		}
		if got := compareSemver(b, a); got != 1 {
			t.Errorf("compareSemver(%s, %s) = %d, want 1", chain[i+1], chain[i], got)
		}
		if got := compareSemver(a, a); got != 0 {
			t.Errorf("compareSemver(%s, %s) = %d, want 0", chain[i], chain[i], got)
		}
	}
}

func TestCompareSemverNumericNotLexical(t *testing.T) {
	// 0.9.0 < 0.10.0 —— 字符串比较会判反，这是"不要用字符串比版本"的经典反例。
	if compareSemver(parseSemver("0.9.0"), parseSemver("0.10.0")) != -1 {
		t.Fatal("0.9.0 应低于 0.10.0")
	}
	if compareSemver(parseSemver("0.25.1"), parseSemver("0.25.10")) != -1 {
		t.Fatal("0.25.1 应低于 0.25.10")
	}
	// build metadata 不参与比较。
	if compareSemver(parseSemver("1.2.3+a"), parseSemver("1.2.3+b")) != 0 {
		t.Fatal("build metadata 不应参与优先级比较")
	}
}

// ---------------------------------------------------------------- decideUpdate

func TestDecideUpdateBasic(t *testing.T) {
	m := &updateManifest{Version: "0.26.0", Notes: "修了些东西", URL: "https://example.com/rel"}

	st := decideUpdate("0.25.1", m, "", 1000)
	if !st.HasUpdate {
		t.Fatal("0.25.1 → 0.26.0 应报有更新")
	}
	if st.Severity != "minor" {
		t.Fatalf("severity = %q, want minor", st.Severity)
	}
	if st.Latest != "0.26.0" || st.Notes == "" || st.ReleaseURL == "" {
		t.Fatalf("字段透传不完整: %+v", st)
	}

	// 同版本 → 无更新
	if st := decideUpdate("0.26.0", m, "", 1000); st.HasUpdate {
		t.Fatal("同版本不应报更新")
	}
	// major
	if st := decideUpdate("0.25.1", &updateManifest{Version: "1.0.0"}, "", 1000); st.Severity != "major" {
		t.Fatalf("severity = %q, want major", st.Severity)
	}
	// patch
	if st := decideUpdate("0.25.1", &updateManifest{Version: "0.25.2"}, "", 1000); st.Severity != "patch" {
		t.Fatalf("severity = %q, want patch", st.Severity)
	}
}

// TestDecideUpdateNoDowngradePrompt 当前版本比清单更新（开发版/预发布用户）
// **绝不能**提示"更新" —— 那会诱导用户降级。
func TestDecideUpdateNoDowngradePrompt(t *testing.T) {
	m := &updateManifest{Version: "0.25.0"}
	st := decideUpdate("0.26.0-dev", m, "", 1000)
	if st.HasUpdate {
		t.Fatalf("当前版本更新时不该提示更新，得 %+v", st)
	}
	// 预发布也算"比同号正式版低"，但比 0.24.x 高
	if st := decideUpdate("0.26.0", &updateManifest{Version: "0.26.0"}, "", 1000); st.HasUpdate {
		t.Fatal("同号正式版不该提示")
	}
}

func TestDecideUpdateSkip(t *testing.T) {
	m := &updateManifest{Version: "0.26.0"}
	st := decideUpdate("0.25.1", m, "0.26.0", 1000)
	if st.HasUpdate {
		t.Fatal("已跳过的版本不该再提示")
	}
	if st.Latest != "0.26.0" {
		t.Fatal("跳过只是不提示，最新版本号仍应透出（否则用户以为检查坏了）")
	}
	// 下一个版本仍应提示
	st2 := decideUpdate("0.25.1", &updateManifest{Version: "0.27.0"}, "0.26.0", 1000)
	if !st2.HasUpdate {
		t.Fatal("跳过只对指定版本生效，新版本应继续提示")
	}
}

// TestDecideUpdateMalformedVersion 版本号不符合 SemVer 时退化为"只判是否相等"。
// 关键：**不能猜大小** —— 猜反方向就成了"降级提醒"。
func TestDecideUpdateMalformedVersion(t *testing.T) {
	// 注意：`2026.10.13` 是**合法** SemVer（三段数字），走正常比较路径。
	// 真正的畸形要用四段或非数字，例如 "2026.10.13.1" / "dev"。
	st := decideUpdate("2026.10.13.1", &updateManifest{Version: "2026.10.13.2"}, "", 1000)
	if !st.HasUpdate {
		t.Fatal("畸形版本号不同时应报有更新（降级为不等判断）")
	}
	if st.Severity != "unknown" {
		t.Fatalf("severity = %q, want unknown", st.Severity)
	}
	// 三段数字式版本号走正常比较：2026.9.1 < 2026.10.13（数值比较，不是字典序）
	if st := decideUpdate("2026.9.1", &updateManifest{Version: "2026.10.13"}, "", 1000); !st.HasUpdate {
		t.Fatal("日期式版本号应按数值比较出有更新")
	}
	// 完全相同 → 不报
	if st := decideUpdate("dev", &updateManifest{Version: "dev"}, "", 1000); st.HasUpdate {
		t.Fatal("畸形但相同不应报更新")
	}
	// 带 v 前缀应视为相同
	if st := decideUpdate("v2026.10.13", &updateManifest{Version: "2026.10.13"}, "", 1000); st.HasUpdate {
		t.Fatal("仅 v 前缀差异不应报更新")
	}
}

func TestDecideUpdateTooOldAndAsset(t *testing.T) {
	// 诱饵产物的平台必须**与当前平台不同**：实现取「首个匹配」，
	// 若诱饵写成 linux/amd64，在 linux/amd64 的 CI runner 上两条都命中、
	// 选中诱饵 → 断言必挂（2026-10-14 CI 抓到；本用例此前只在 Windows 上跑过）。
	decoyOS, decoyArch := "linux", "amd64"
	if runtime.GOOS == "linux" && runtime.GOARCH == "amd64" {
		decoyOS, decoyArch = "windows", "amd64"
	}
	if decoyOS == runtime.GOOS && decoyArch == runtime.GOARCH {
		t.Fatalf("诱饵平台 %s/%s 与当前平台相同，本用例失去意义", decoyOS, decoyArch)
	}
	m := &updateManifest{
		Version:      "0.30.0",
		MinSupported: "0.26.0",
		Assets: []updateAsset{
			{OS: decoyOS, Arch: decoyArch, URL: "https://example.com/decoy"},
			{OS: runtime.GOOS, Arch: runtime.GOARCH, URL: "https://example.com/mine", SHA256: "abc"},
		},
	}
	st := decideUpdate("0.25.1", m, "", 1000)
	if !st.TooOld {
		t.Fatal("当前版本低于 min_supported 时 TooOld 应为 true")
	}
	if st.Asset == nil || st.Asset.URL != "https://example.com/mine" {
		t.Fatalf("应选中当前平台的产物，得 %+v", st.Asset)
	}

	// 版本够新时不标 TooOld
	st2 := decideUpdate("0.27.0", m, "", 1000)
	if st2.TooOld {
		t.Fatal("版本满足 min_supported 时不该标 TooOld")
	}
}

func TestDecideUpdateNilManifest(t *testing.T) {
	st := decideUpdate("0.25.1", nil, "", 0)
	if st.HasUpdate || st.Current != "0.25.1" {
		t.Fatalf("nil 清单应只回填当前版本，得 %+v", st)
	}
}

// ---------------------------------------------------------------- fetchUpdateManifest

func TestFetchUpdateManifest(t *testing.T) {
	withTestClient(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/ok.json", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			t.Error("请求应带 User-Agent")
		}
		_, _ = w.Write([]byte(`{"version":"0.26.0","notes":"n","assets":[{"os":"windows","arch":"amd64","url":"u"}]}`))
	})
	mux.HandleFunc("/bad.json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{not json`))
	})
	mux.HandleFunc("/noversion.json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"notes":"忘了写版本号"}`))
	})
	mux.HandleFunc("/err.json", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusForbidden)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	m, used, err := fetchUpdateManifest(context.Background(), srv.URL+"/ok.json", "0.25.1", "")
	if err != nil || m == nil || m.Version != "0.26.0" || len(m.Assets) != 1 {
		t.Fatalf("正常清单解析失败: m=%+v err=%v", m, err)
	}
	if used != srv.URL+"/ok.json" {
		t.Errorf("used=%q，期望命中的候选地址 %q", used, srv.URL+"/ok.json")
	}
	if _, _, err := fetchUpdateManifest(context.Background(), srv.URL+"/bad.json", "0.25.1", ""); err == nil {
		t.Fatal("坏 JSON 应返回错误")
	}
	if _, _, err := fetchUpdateManifest(context.Background(), srv.URL+"/noversion.json", "0.25.1", ""); err == nil {
		t.Fatal("缺 version 字段应返回错误")
	}
	// 403 必须把状态码带进错误里，且错误汇总要能看出是哪个 host 失败的。
	_, _, err = fetchUpdateManifest(context.Background(), srv.URL+"/err.json", "0.25.1", "")
	if err == nil {
		t.Fatal("403 应返回错误（带状态码，便于判断是不是被 CDN 拦了）")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("错误信息应含状态码 403，得 %v", err)
	}
	if _, _, err := fetchUpdateManifest(context.Background(), "", "0.25.1", ""); err == nil {
		t.Fatal("空地址应返回错误")
	}
}

// ---------------------------------------------------------------- v0.26.5 候选回退

// TestUpdateManifestCandidates GitHub raw 地址应派生出 API / CDN / 反代三个等价候选；
// 非 raw 地址（自建 CDN、内网服务）**不得**被塞进第三方站点。
func TestUpdateManifestCandidates(t *testing.T) {
	got := updateManifestCandidates("https://raw.githubusercontent.com/o/r/main/dist/version.json")
	want := []string{
		"https://raw.githubusercontent.com/o/r/main/dist/version.json",
		"https://api.github.com/repos/o/r/contents/dist/version.json?ref=main",
		"https://cdn.jsdelivr.net/gh/o/r@main/dist/version.json",
		"https://ghproxy.net/https://raw.githubusercontent.com/o/r/main/dist/version.json",
	}
	if len(got) != len(want) {
		t.Fatalf("候选数 = %d，期望 %d：%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("候选[%d] = %q，期望 %q", i, got[i], want[i])
		}
	}
	// 非 raw：只有它自己，不引入任何第三方。
	if c := updateManifestCandidates("https://cdn.example.com/version.json"); len(c) != 1 {
		t.Errorf("非 raw 地址不该派生候选，得 %v", c)
	}
	if c := updateManifestCandidates(""); len(c) != 0 {
		t.Errorf("空地址应无候选，得 %v", c)
	}
	// 段数不足（缺 branch/path）不是合法 raw 形态 → 不派生。
	if c := updateManifestCandidates("https://raw.githubusercontent.com/o/r/main"); len(c) != 1 {
		t.Errorf("缺 path 的 raw 地址不该派生候选，得 %v", c)
	}
}

// TestSplitRawGitHubURL 解析形态（含多段 path 与 http 老形态）。
func TestSplitRawGitHubURL(t *testing.T) {
	cases := []struct {
		in                        string
		owner, repo, branch, path string
		ok                        bool
	}{
		{"https://raw.githubusercontent.com/o/r/main/version.json", "o", "r", "main", "version.json", true},
		{"https://raw.githubusercontent.com/o/r/main/a/b/c.json", "o", "r", "main", "a/b/c.json", true},
		{"http://raw.githubusercontent.com/o/r/main/v.json", "o", "r", "main", "v.json", true},
		{"https://raw.githubusercontent.com/o/r/main", "", "", "", "", false},
		{"https://example.com/v.json", "", "", "", "", false},
	}
	for _, c := range cases {
		o, r, b, p, ok := splitRawGitHubURL(c.in)
		if ok != c.ok || o != c.owner || r != c.repo || b != c.branch || p != c.path {
			t.Errorf("%s → (%q,%q,%q,%q,%v)，期望 (%q,%q,%q,%q,%v)",
				c.in, o, r, b, p, ok, c.owner, c.repo, c.branch, c.path, c.ok)
		}
	}
}

// TestParseUpdateManifestBodyGitHubContentsAPI Contents API 的 base64 信封必须能解析 ——
// 它正是 raw 不可达时那条回退路径的返回形态。
//
// body 用 json.Marshal 构造而不是手拼字符串：真实响应里 content 的折行是
// **转义过的**（`\n` 两个字符），手拼一个真换行会让 JSON 本身非法 ——
// 那样测的就不是"解析口径"而是"我这个字符串写错了"。
func TestParseUpdateManifestBodyGitHubContentsAPI(t *testing.T) {
	inner := `{"version":"9.9.9","notes":"来自 API","url":"https://example.com/rel"}`
	enc := base64.StdEncoding.EncodeToString([]byte(inner))
	var folded strings.Builder
	for i := 0; i < len(enc); i += 60 {
		end := i + 60
		if end > len(enc) {
			end = len(enc)
		}
		folded.WriteString(enc[i:end])
		folded.WriteByte(0x0a) // 真实 GitHub 响应每 60 字符折一行
	}
	body, err := json.Marshal(map[string]any{
		"name": "version.json", "encoding": "base64", "content": folded.String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	m, err := parseUpdateManifestBody(body)
	if err != nil {
		t.Fatalf("Contents API 信封解析失败: %v", err)
	}
	if m.Version != "9.9.9" || m.Notes != "来自 API" {
		t.Errorf("解析结果 = %+v", m)
	}
	// 裸清单仍走原路径。
	if m2, err := parseUpdateManifestBody([]byte(inner)); err != nil || m2.Version != "9.9.9" {
		t.Errorf("裸清单解析回归: m=%+v err=%v", m2, err)
	}
}

// TestUpdateManifestURLDefaults 未配置时回落默认地址；配置了就用配置的。
func TestUpdateManifestURLDefaults(t *testing.T) {
	store, _, a := newTestApp(t)
	defer store.Close()

	if got := a.updateManifestURL(); got != defaultUpdateManifestURL {
		t.Errorf("未配置时应回落默认地址，得 %q", got)
	}
	if err := store.SetKV(kvUpdateManifest, "https://self.example/version.json"); err != nil {
		t.Fatal(err)
	}
	if got := a.updateManifestURL(); got != "https://self.example/version.json" {
		t.Errorf("显式配置应优先，得 %q", got)
	}
	// 清空（写空格）→ 回落默认：运维"清空"的意图是回到默认，不是"永不检查"。
	if err := store.SetKV(kvUpdateManifest, "   "); err != nil {
		t.Fatal(err)
	}
	if got := a.updateManifestURL(); got != defaultUpdateManifestURL {
		t.Errorf("空白配置应回落默认地址，得 %q", got)
	}
}

// TestOrderUpdateCandidates 上次命中的候选排到最前；
// **不在候选里的值必须被忽略**（否则一个陈旧 kv 值就能让程序去访问任意 URL）。
func TestOrderUpdateCandidates(t *testing.T) {
	cands := updateManifestCandidates("https://raw.githubusercontent.com/o/r/main/version.json")
	if len(cands) != 4 {
		t.Fatalf("候选数 = %d，期望 4", len(cands))
	}
	// 取第 3 个（jsdelivr）作为"上次成功"，它应被提到最前，其余保持相对顺序。
	last := cands[2]
	got := orderUpdateCandidates(cands, last)
	if got[0] != last {
		t.Errorf("首项 = %q，期望 %q", got[0], last)
	}
	if len(got) != len(cands) {
		t.Fatalf("候选数量不得变化：%v", got)
	}
	seen := map[string]bool{}
	for _, c := range got {
		if seen[c] {
			t.Errorf("候选重复：%v", got)
		}
		seen[c] = true
	}
	// 已在首位 → 原样。
	if c := orderUpdateCandidates(cands, cands[0]); c[0] != cands[0] {
		t.Error("已在首位时不该变动")
	}
	// 陌生值 → 原样返回（不注入）。
	out := orderUpdateCandidates(cands, "https://evil.example/version.json")
	if len(out) != len(cands) || out[0] != cands[0] {
		t.Errorf("未命中的 lastUsed 不得进入候选：%v", out)
	}
	// 单候选 / 空 lastUsed → 原样。
	if out := orderUpdateCandidates(cands[:1], last); len(out) != 1 {
		t.Error("单候选不该变动")
	}
	if out := orderUpdateCandidates(cands, ""); out[0] != cands[0] {
		t.Error("空 lastUsed 不该变动")
	}
}

// ---------------------------------------------------------------- checkUpdate

// TestCheckUpdateUsesDefaultManifestURLAndReportsFailure 未显式配置清单地址时
// 走默认地址（v0.26.5：默认值让"提醒"这件事不需要先手工配置就成立）。
//
// 出站被替换成必定失败的 transport，因此本用例**不发任何真实请求**：
// 断言的是"它确实去试了默认地址 + 全部失败时给出可读原因 + 不谎报有更新"。
func TestCheckUpdateUsesDefaultManifestURLAndReportsFailure(t *testing.T) {
	old := updateHTTPClient
	updateHTTPClient = &http.Client{
		Timeout:   time.Second,
		Transport: failTransport{},
	}
	t.Cleanup(func() { updateHTTPClient = old })

	a := newUpdateTestApp(t)
	st := a.checkUpdate(context.Background(), true)
	if st.Current != version {
		t.Fatalf("应回填当前版本，得 %q", st.Current)
	}
	if st.ManifestURL != defaultUpdateManifestURL {
		t.Fatalf("生效地址 = %q，期望默认 %q", st.ManifestURL, defaultUpdateManifestURL)
	}
	if st.Error == "" {
		t.Fatal("全部候选失败时应给出明确提示，而不是静默返回空结果")
	}
	if !strings.Contains(st.Error, "所有候选地址均失败") {
		t.Errorf("错误应说明是候选回退全失败，得 %q", st.Error)
	}
	// 失败原因要能区分"哪个站挂了"：错误里必须出现至少两个候选的 host。
	if !strings.Contains(st.Error, "raw.githubusercontent.com") || !strings.Contains(st.Error, "api.github.com") {
		t.Errorf("错误应逐个候选说明原因，得 %q", st.Error)
	}
	if st.HasUpdate {
		t.Fatal("检查失败时不该报有更新")
	}
}

// failTransport 让所有出站请求立即失败（不碰网络，测试确定性）。
type failTransport struct{}

func (failTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errNetworkDownForTest
}

var errNetworkDownForTest = errors.New("test: network disabled")

func TestCheckUpdateDisabled(t *testing.T) {
	a := newUpdateTestApp(t)
	if err := a.store.SetKV(kvUpdateEnabled, "0"); err != nil {
		t.Fatal(err)
	}
	st := a.checkUpdate(context.Background(), true)
	if st.Enabled {
		t.Fatal("开关关闭时 Enabled 应为 false")
	}
	if st.Current != version {
		t.Fatal("关闭检查不等于没有版本信息 —— 本机版本仍应显示")
	}
}

// TestCheckUpdateCacheAndSilentFailure 钉住两条核心约束：
//  1. 缓存新鲜时**完全不出站**（打开页面不会因为检查更新变慢）；
//  2. 出站失败时**保留上一次的好结果**，只标 error ——
//     否则"暂时网络不通"看起来像"项目已停止维护"。
func TestCheckUpdateCacheAndSilentFailure(t *testing.T) {
	withTestClient(t)
	a := newUpdateTestApp(t)

	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_ = json.NewEncoder(w).Encode(updateManifest{Version: "9.9.9", Notes: "新"})
	}))
	defer srv.Close()

	if err := a.store.SetKV(kvUpdateManifest, srv.URL); err != nil {
		t.Fatal(err)
	}

	st := a.checkUpdate(context.Background(), false)
	if !st.HasUpdate || st.Latest != "9.9.9" {
		t.Fatalf("首次检查应发现新版本，得 %+v", st)
	}
	if hits != 1 {
		t.Fatalf("首次检查应恰好出站 1 次，实际 %d", hits)
	}

	// 第二次：缓存新鲜 → 不该再出站，但结果必须一样。
	st2 := a.checkUpdate(context.Background(), false)
	if hits != 1 {
		t.Fatalf("缓存新鲜时不应再次出站，实际出站 %d 次", hits)
	}
	if !st2.HasUpdate || st2.Latest != "9.9.9" {
		t.Fatalf("缓存命中应返回同样的结论，得 %+v", st2)
	}

	// force=true 绕过缓存：只有用户**明确点了「立即检查」**才再打一次网络。
	// 注意要在服务器还活着时验证 —— 关了服务器就发不出请求，hits 自然不会涨。
	_ = a.checkUpdate(context.Background(), true)
	if hits != 2 {
		t.Fatalf("force=true 应绕过缓存真实出站（期望 2 次，实际 %d）", hits)
	}

	// 关掉服务器模拟断网：**保留上次的好结果**并标 stale + 带出 error。
	srv.Close()
	st3 := a.checkUpdate(context.Background(), true)
	if st3.Error == "" {
		t.Fatal("出站失败时应把 error 带出来")
	}
	if !st3.Stale || st3.Latest != "9.9.9" {
		t.Fatalf("失败时应回退到上次的好结果并标记 stale，得 %+v", st3)
	}
}

func TestUpdateCacheFreshUsesRelativeInterval(t *testing.T) {
	a := newUpdateTestApp(t)
	now := time.Now()
	if a.updateCacheFresh(now) {
		t.Fatal("没有检查记录时应视为缓存不新鲜")
	}
	_ = a.store.SetKV(kvUpdateLastAt, "1791900000")
	// 写入一个"很久以前"的时间戳（该值远早于 2026-10-14）→ 已过期
	if a.updateCacheFresh(now) {
		t.Fatal("陈旧时间戳应判为过期")
	}
	_ = a.store.SetKV(kvUpdateLastAt, "")
	if a.updateCacheFresh(now) {
		t.Fatal("空值应判为过期")
	}
}

func TestUpdateExitCode(t *testing.T) {
	cases := []struct {
		st   updateStatus
		want int
	}{
		{updateStatus{Current: "1", Enabled: true, ManifestURL: "u", Latest: "1"}, 0},
		{updateStatus{Current: "1", Enabled: true, ManifestURL: "u", Latest: "2", HasUpdate: true}, 1},
		{updateStatus{Current: "1", Enabled: true, ManifestURL: "u", Error: "boom"}, 2},
		{updateStatus{Current: "1", Enabled: false}, 2},
		{updateStatus{Current: "1", Enabled: true, ManifestURL: ""}, 2},
	}
	for i, c := range cases {
		if got := updateExitCode(c.st); got != c.want {
			t.Errorf("case %d: updateExitCode = %d, want %d（检查失败必须与『已最新』区分开）", i, got, c.want)
		}
	}
}
