// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
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

	m, err := fetchUpdateManifest(context.Background(), srv.URL+"/ok.json", "0.25.1")
	if err != nil || m == nil || m.Version != "0.26.0" || len(m.Assets) != 1 {
		t.Fatalf("正常清单解析失败: m=%+v err=%v", m, err)
	}
	if _, err := fetchUpdateManifest(context.Background(), srv.URL+"/bad.json", "0.25.1"); err == nil {
		t.Fatal("坏 JSON 应返回错误")
	}
	if _, err := fetchUpdateManifest(context.Background(), srv.URL+"/noversion.json", "0.25.1"); err == nil {
		t.Fatal("缺 version 字段应返回错误")
	}
	if _, err := fetchUpdateManifest(context.Background(), srv.URL+"/err.json", "0.25.1"); err == nil {
		t.Fatal("403 应返回错误（带状态码，便于判断是不是被 CDN 拦了）")
	}
	if _, err := fetchUpdateManifest(context.Background(), "", "0.25.1"); err == nil {
		t.Fatal("空地址应返回错误")
	}
}

// ---------------------------------------------------------------- checkUpdate

func TestCheckUpdateNotConfigured(t *testing.T) {
	a := newUpdateTestApp(t)
	st := a.checkUpdate(context.Background(), true)
	if st.Current != version {
		t.Fatalf("应回填当前版本，得 %q", st.Current)
	}
	if st.Error == "" {
		t.Fatal("未配置清单地址时应给出明确提示，而不是静默返回空结果")
	}
	if st.HasUpdate {
		t.Fatal("未配置时不该报有更新")
	}
}

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
