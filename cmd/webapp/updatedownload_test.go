package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// ======================================================================
// v0.26.7 · 更新产物自动下载
//
// 这一批用例的重心**不是"能下载"**，而是**"不该落盘的一律不落盘"**：
// 下载并落盘一个可执行文件是全项目风险最高的动作，而它的失败模式是静默的
// （落了一个坏文件，直到有人执行替换脚本才发现）。所以下面把每条拒绝理由
// 都变成一条断言。
// ======================================================================

// withTempExe 把产物落盘位置指到临时目录（否则测试会写到 go test 的二进制旁边）。
func withTempExe(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, "webapp.exe")
	if err := os.WriteFile(exe, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := updateExecutable
	updateExecutable = func() (string, error) { return exe, nil }
	t.Cleanup(func() { updateExecutable = old })
	return exe
}

// newUpdateApp 造一个只带 store 的最小 app（下载路径只用到 store 与版本号）。
func newUpdateApp(t *testing.T) *app {
	t.Helper()
	store := newTestStore(t)
	return &app{store: store, key: "k"}
}

// ---------------------------------------------------------------- 资产匹配

// TestUpdateAssetForPicksMatchingPlatform 只挑本机平台的产物。
func TestUpdateAssetForPicksMatchingPlatform(t *testing.T) {
	cur := runtime.GOOS
	arch := runtime.GOARCH
	other := "linux"
	if cur == "linux" {
		other = "windows"
	}

	assets := []updateAsset{
		{OS: other, Arch: arch, URL: "https://x/other"},
		{OS: cur, Arch: arch, URL: "https://x/mine"},
	}
	got := updateAssetFor(assets)
	if got == nil || got.URL != "https://x/mine" {
		t.Fatalf("应挑出本机平台产物，得 %+v", got)
	}

	// 只有别的平台 → nil（不能装别人的二进制）
	if got := updateAssetFor([]updateAsset{{OS: other, Arch: arch, URL: "https://x/o"}}); got != nil {
		t.Errorf("不该匹配其它平台，却得到 %+v", got)
	}
	// 缺 os/arch → 不匹配（不知道给谁的产物不能装）
	if got := updateAssetFor([]updateAsset{{URL: "https://x/n"}}); got != nil {
		t.Error("缺少 os/arch 的产物不该匹配")
	}
	// 有匹配项但没有 URL → 不匹配
	if got := updateAssetFor([]updateAsset{{OS: cur, Arch: arch}}); got != nil {
		t.Error("没有下载地址的产物不该匹配")
	}
	// 空清单
	if got := updateAssetFor(nil); got != nil {
		t.Errorf("空清单应为 nil，得 %+v", got)
	}
}

// TestUpdateAssetForAliases `win` / `x86_64` 等常见别名要认 ——
// 不认的代价是"清单明明给了产物却提示未提供"，排查起来毫无线索。
func TestUpdateAssetForAliases(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("别名规则针对 windows/amd64 常见写法，其它平台跳过")
	}
	aliases := []updateAsset{{OS: "win", Arch: runtime.GOARCH, URL: "https://x/win"}}
	if got := updateAssetFor(aliases); got == nil {
		t.Error("`win` 应被认作 windows")
	}
	if runtime.GOARCH == "amd64" {
		al := []updateAsset{{OS: runtime.GOOS, Arch: "x86_64", URL: "https://x/a"}}
		if got := updateAssetFor(al); got == nil {
			t.Error("`x86_64` 应被认作 amd64")
		}
	}
}

// ---------------------------------------------------------------- 下载的安全边界

// TestDownloadRejectsNonHTTP 只接受 http/https。
func TestDownloadRejectsNonHTTP(t *testing.T) {
	withTempExe(t)
	a := newUpdateApp(t)
	for _, u := range []string{"file:///etc/passwd", "ftp://x/a.exe", "/local/path", "javascript:alert(1)"} {
		d := a.downloadUpdate(context.Background(), updateStatus{
			Latest: "9.9.9",
			Asset:  &updateAsset{OS: runtime.GOOS, Arch: runtime.GOARCH, URL: u, SHA256: strings.Repeat("a", 64)},
		})
		if d.State != "failed" || !strings.Contains(d.Error, "http") {
			t.Errorf("地址 %q 应被拒绝，得 %+v", u, d)
		}
	}
}

// TestDownloadRejectsMissingSHA256 **没有摘要的二进制一律不落盘**。
//
// 这条挡的是"清单被换成指向任意文件"：拿不到摘要就无法判断下到的是什么，
// 而落盘的产物随后会被替换脚本捡起来执行。
func TestDownloadRejectsMissingSHA256(t *testing.T) {
	exe := withTempExe(t)
	a := newUpdateApp(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("payload"))
	}))
	defer srv.Close()

	for _, sha := range []string{"", "   ", "abc123"} { // 空 / 空白 / 长度不足
		d := a.downloadUpdate(context.Background(), updateStatus{
			Latest: "9.9.9",
			Asset:  &updateAsset{OS: runtime.GOOS, Arch: runtime.GOARCH, URL: srv.URL, SHA256: sha},
		})
		if d.State != "failed" {
			t.Errorf("sha256=%q 应被拒绝，得 %+v", sha, d)
		}
		if _, err := os.Stat(exe + updateNewSuffix); !os.IsNotExist(err) {
			t.Errorf("sha256=%q 时不该有产物落盘", sha)
		}
	}
}

// TestDownloadRejectsBadHash 摘要不匹配 → 丢弃且**不留残留**。
//
// 残留的危害很具体：替换脚本只认文件名，残留会被当成"已备好的新版本"。
func TestDownloadRejectsBadHash(t *testing.T) {
	exe := withTempExe(t)
	a := newUpdateApp(t)
	payload := []byte("real payload")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	d := a.downloadUpdate(context.Background(), updateStatus{
		Latest: "9.9.9",
		Asset: &updateAsset{OS: runtime.GOOS, Arch: runtime.GOARCH, URL: srv.URL,
			SHA256: strings.Repeat("b", 64)}, // 故意错的摘要
	})
	if d.State != "failed" || !strings.Contains(d.Error, "sha256") {
		t.Fatalf("摘要不符应失败，得 %+v", d)
	}
	if _, err := os.Stat(exe + updateNewSuffix); !os.IsNotExist(err) {
		t.Error("校验失败不该留下 .new（替换脚本会把它当成已备好的新版本）")
	}
	// 也不该留下 .tmp
	if _, err := os.Stat(exe + updateNewSuffix + ".tmp"); !os.IsNotExist(err) {
		t.Error("校验失败不该留下临时文件")
	}
}

// TestDownloadRejectsSizeMismatch 体积与清单不符 → 丢弃（半截的 exe 不能当完整产物）。
func TestDownloadRejectsSizeMismatch(t *testing.T) {
	exe := withTempExe(t)
	a := newUpdateApp(t)
	payload := []byte("0123456789")
	sum := sha256.Sum256(payload)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	d := a.downloadUpdate(context.Background(), updateStatus{
		Latest: "9.9.9",
		Asset: &updateAsset{OS: runtime.GOOS, Arch: runtime.GOARCH, URL: srv.URL,
			SHA256: hex.EncodeToString(sum[:]), Size: 999}, // 声明的体积不符
	})
	if d.State != "failed" || !strings.Contains(d.Error, "体积不符") {
		t.Fatalf("体积不符应失败，得 %+v", d)
	}
	if _, err := os.Stat(exe + updateNewSuffix); !os.IsNotExist(err) {
		t.Error("体积不符不该留下 .new")
	}
}

// TestDownloadSuccessWritesNewFile 正常路径：落盘 `.new`、State=done、带回摘要与体积。
func TestDownloadSuccessWritesNewFile(t *testing.T) {
	exe := withTempExe(t)
	a := newUpdateApp(t)
	payload := []byte("brand new binary")
	sum := sha256.Sum256(payload)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	d := a.downloadUpdate(context.Background(), updateStatus{
		Latest: "9.9.9",
		Asset: &updateAsset{OS: runtime.GOOS, Arch: runtime.GOARCH, URL: srv.URL,
			SHA256: hex.EncodeToString(sum[:]), Size: int64(len(payload))},
	})
	if d.State != "done" {
		t.Fatalf("应下载成功，得 %+v", d)
	}
	if d.GotSize != int64(len(payload)) {
		t.Errorf("落地体积 = %d，期望 %d", d.GotSize, len(payload))
	}
	if d.File != filepath.Base(exe)+updateNewSuffix {
		t.Errorf("File 应为相对文件名，得 %q", d.File)
	}
	if d.ApplyHint == "" {
		t.Error("应给出应用指引（没有它运维不知道下一步做什么）")
	}
	got, err := os.ReadFile(exe + updateNewSuffix)
	if err != nil {
		t.Fatalf("产物应已落盘：%v", err)
	}
	if string(got) != string(payload) {
		t.Error("落盘内容与下载内容不一致")
	}
	// **不动原 exe** —— 自动替换是刻意不做的。
	old, _ := os.ReadFile(exe)
	if string(old) != "old" {
		t.Error("原可执行文件不该被改动（本功能只下载、不替换）")
	}
	// 状态已持久化，供下次刷新读取。
	re := a.loadUpdateDownload()
	if re.State != "done" || re.Version != "9.9.9" {
		t.Errorf("下载状态应已持久化，得 %+v", re)
	}
}

// TestDownloadRejectsEmptyPayload 0 字节响应不能被当成产物。
func TestDownloadRejectsEmptyPayload(t *testing.T) {
	exe := withTempExe(t)
	a := newUpdateApp(t)
	empty := sha256.Sum256(nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	d := a.downloadUpdate(context.Background(), updateStatus{
		Latest: "9.9.9",
		Asset: &updateAsset{OS: runtime.GOOS, Arch: runtime.GOARCH, URL: srv.URL,
			SHA256: hex.EncodeToString(empty[:])},
	})
	if d.State != "failed" {
		t.Fatalf("空产物应失败，得 %+v", d)
	}
	if _, err := os.Stat(exe + updateNewSuffix); !os.IsNotExist(err) {
		t.Error("空产物不该落盘")
	}
}

// TestDownloadMissingAssetSaysSo 清单没给本机平台产物时要**明说**，
// 而不是静默留空（否则界面看起来像"正在准备"）。
func TestDownloadMissingAssetSaysSo(t *testing.T) {
	withTempExe(t)
	a := newUpdateApp(t)
	d := a.downloadUpdate(context.Background(), updateStatus{Latest: "9.9.9"})
	if d.State != "failed" || !strings.Contains(d.Error, runtime.GOOS) {
		t.Errorf("应明说未提供本机平台产物，得 %+v", d)
	}
	// 连最新版本都没有 → none（不是 failed，没什么可抱怨的）
	if d2 := a.downloadUpdate(context.Background(), updateStatus{}); d2.State != "none" {
		t.Errorf("无最新版本时应为 none，得 %+v", d2)
	}
}

// ---------------------------------------------------------------- 应用入口

// TestApplyWithoutArtifactRejects 没有产物时「应用更新」必须拒绝 ——
// 否则会生成一个引用不存在文件的脚本，运维执行后才发现。
func TestApplyWithoutArtifactRejects(t *testing.T) {
	withTempExe(t)
	a := newUpdateApp(t)
	rec := httptest.NewRecorder()
	a.handleAdminUpdateApply(rec, httptest.NewRequest(http.MethodPost, "/api/admin/update-apply", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("无产物应 400，得 %d：%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "已就绪") {
		t.Errorf("错误文案应说明原因，得 %s", rec.Body.String())
	}
}

// TestApplyWritesHelperScript 有产物时生成替换脚本，且脚本内容**包含备份与回滚**
// （手写这段最容易漏的就是回滚 —— 替换失败后原文件已被改名，没有回滚会让服务起不来）。
func TestApplyWritesHelperScript(t *testing.T) {
	exe := withTempExe(t)
	a := newUpdateApp(t)
	payload := []byte("new binary")
	sum := sha256.Sum256(payload)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	if d := a.downloadUpdate(context.Background(), updateStatus{
		Latest: "9.9.9",
		Asset: &updateAsset{OS: runtime.GOOS, Arch: runtime.GOARCH, URL: srv.URL,
			SHA256: hex.EncodeToString(sum[:])},
	}); d.State != "done" {
		t.Fatalf("前置下载失败：%+v", d)
	}

	rec := httptest.NewRecorder()
	a.handleAdminUpdateApply(rec, httptest.NewRequest(http.MethodPost, "/api/admin/update-apply", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if s, _ := out["script"].(string); s != updateHelperName {
		t.Errorf("应回脚本文件名 %q，得 %v", updateHelperName, out["script"])
	}
	// 不泄露绝对路径（面板会展示、会进截图）。
	if strings.Contains(rec.Body.String(), t.TempDir()) {
		t.Error("响应里不该出现绝对路径")
	}

	helper := filepath.Join(filepath.Dir(exe), updateHelperName)
	body, err := os.ReadFile(helper)
	if err != nil {
		t.Fatalf("脚本应已生成：%v", err)
	}
	text := string(body)
	for _, want := range []string{"move /y", updateNewSuffix, ".bak", "start"} {
		if !strings.Contains(text, want) {
			t.Errorf("脚本应包含 %q（备份/替换/重启缺一不可）\n%s", want, text)
		}
	}
}

// ---------------------------------------------------------------- 触发条件

// TestAttachDownloadDoesNotTriggerWithoutAsset 没有产物可下时不该起 goroutine。
func TestAttachDownloadDoesNotTriggerWithoutAsset(t *testing.T) {
	a := newUpdateApp(t)
	st := updateStatus{Current: "1.0.0", Latest: "9.9.9", HasUpdate: true}
	a.attachDownload(&st)
	if st.Download == nil {
		t.Fatal("Download 必须非 nil（前端要不判空就能渲染）")
	}
	if st.Download.State != "none" {
		t.Errorf("无产物时状态应为 none，得 %+v", st.Download)
	}
	if !st.AutoDownload {
		t.Error("自动下载默认应开启")
	}
}

// TestAttachDownloadRespectsSwitch 关掉开关时**不触发**下载，但仍回报已有状态。
func TestAttachDownloadRespectsSwitch(t *testing.T) {
	store := newTestStore(t)
	if err := store.SetKV(kvUpdateAutoDownload, "0"); err != nil {
		t.Fatal(err)
	}
	a := &app{store: store}
	st := updateStatus{Current: "1.0.0", Latest: "9.9.9", HasUpdate: true,
		Asset: &updateAsset{OS: runtime.GOOS, Arch: runtime.GOARCH, URL: "https://x/a", SHA256: strings.Repeat("a", 64)}}
	a.attachDownload(&st)
	if st.AutoDownload {
		t.Error("开关为 0 时应回报 false")
	}
	if st.Download == nil || st.Download.State == "downloading" {
		t.Errorf("关闭自动下载时不该发起下载，得 %+v", st.Download)
	}
}

// TestLoadUpdateDownloadDefaultsNone 无记录时是 none 而不是空结构
// （前端 switch 需要确定的状态值）。
func TestLoadUpdateDownloadDefaultsNone(t *testing.T) {
	a := newUpdateApp(t)
	if d := a.loadUpdateDownload(); d.State != "none" {
		t.Errorf("默认为 none，得 %+v", d)
	}
	// 脏数据（非法 JSON）也要回落 none，不能 panic。
	if err := a.store.SetKV(kvUpdateDownloadJSON, "{not json"); err != nil {
		t.Fatal(err)
	}
	if d := a.loadUpdateDownload(); d.State != "none" {
		t.Errorf("脏数据应回落 none，得 %+v", d)
	}
}

// TestUpdateHelperCarriesStartupArgs 替换脚本必须带上**当前进程的启动参数**。
//
// 这是 v0.27.0 修的一个真缺陷：脚本原先只写 `start "" "webapp.exe" %*`，而 %*
// 是**脚本自己**收到的参数（双击时为空）→ 重启后程序用默认参数启动，监听地址与
// 数据库路径都变了。表现是"替换成功但服务起不来/数据不对"，极难往这边想。
func TestUpdateHelperCarriesStartupArgs(t *testing.T) {
	exe := withTempExe(t) // 已把 updateExecutable 指向 t.TempDir 里的假 exe
	if err := os.WriteFile(exe+updateNewSuffix, []byte("new-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 模拟生产启动方式：带 -addr 与 -db 两个参数（其中一个含空格，验证引号处理）。
	oldArgs := updateArgs
	updateArgs = func() []string {
		return []string{"-addr", "127.0.0.1:8780", "-db", "./data/my db.sqlite"}
	}
	defer func() { updateArgs = oldArgs }()

	a := &app{store: newTestStore(t)}
	helper, err := a.writeUpdateHelper()
	if err != nil {
		t.Fatalf("writeUpdateHelper: %v", err)
	}
	raw, err := os.ReadFile(helper)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, want := range []string{
		"-addr", "127.0.0.1:8780", "-db",
		// 含空格的参数必须被引号包住，否则重启后会被拆成两个参数。
		`"./data/my db.sqlite"`,
		// 手工追加参数的能力要保留。
		"%*",
		// 备份 / 失败回滚这两步不能丢（脚本存在的全部意义）。
		".bak", "move /y",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("替换脚本缺少 %q：\n%s", want, body)
		}
	}
}

// TestQuoteBatArg 参数转义：含空白与元字符的要用引号包住；空串给一对空引号。
func TestQuoteBatArg(t *testing.T) {
	cases := []struct{ in, want string }{
		{"-addr", "-addr"},
		{"127.0.0.1:8780", "127.0.0.1:8780"},
		{"./data/my db.sqlite", `"./data/my db.sqlite"`},
		{"a&b", `"a&b"`},
		{"", `""`},
		// 内部双引号无法在 bat 里可靠转义 → 去掉（宁可少一个引号也不要坏掉整行）。
		{`a"b`, `"ab"`},
	}
	for _, c := range cases {
		if got := quoteBatArg(c.in); got != c.want {
			t.Errorf("quoteBatArg(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}
