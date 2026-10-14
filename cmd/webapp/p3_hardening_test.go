// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 本文件是 v0.24.2 收尾 P3 剩余项的回归测试（对应 docs/审查问题整改-v0.24.1.md 第十一节）：
//
//	P3-8  管理员「服务端路径导入」的文件读取白名单
//	P3-11 /metrics 需鉴权、/healthz 保持公开但只回最小信息
//	P3-12 首个注册用户不再自动成为管理员（需显式引导密钥）
//
// P3-13（前端 helper 下沉）与 P3-15（init 副作用）分别在 tools/uitest.mjs 与
// internal/egress 的测试里覆盖 —— 那两个点在 Go 侧没有可观察面。

// newP3App 装配 p3 用例用的 app：真 store + 真 mux + 已知管理密钥 + 独立数据目录。
//
// 注意 limiter / alerts 必须一起装：/metrics 会读它们，缺一个就是空指针 panic
// （这正是「用真 mux 打真路由」的价值 —— 直接调 handler 的老测试碰不到这条路径）。
func newP3App(t *testing.T) (*Store, *app, *http.ServeMux) {
	t.Helper()
	store, _, a := newTestApp(t)
	a.key = "p3-admin-key"
	a.registerLimiter = newRateLimiter(100, time.Hour)
	a.limiter = newClientLimiter()
	a.alerts = newAlertState()
	a.dataDir = t.TempDir()
	return store, a, a.routes()
}

// registerWith 打一次 /api/auth/register（真 mux），返回状态码与解析后的响应。
func registerWith(t *testing.T, mux *http.ServeMux, username, password, bootstrap string) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"username": username, "password": password, "bootstrap": bootstrap,
	})
	req := jsonReq("POST", "/api/auth/register", string(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	var out map[string]any
	_ = json.Unmarshal([]byte(rec.Body.String()), &out)
	return rec.Code, out
}

// ---------- P3-12：首个用户不再自动成管理员 ----------

// TestRegisterFirstUserNotAdminWithoutBootstrap 抢注者拿不到管理员。
//
// 这是 P3-12 的核心反例：库为空 + 开放注册时，任何先到的人都会成为管理员 ——
// 而生产形态是「隧道先起、运维后注册」，中间那段窗口足以被扫站脚本撞上。
func TestRegisterFirstUserNotAdminWithoutBootstrap(t *testing.T) {
	store, _, mux := newP3App(t)
	defer store.Close()

	code, body := registerWith(t, mux, "squatter", "Squ@tter-2026x", "")
	if code != http.StatusOK {
		t.Fatalf("注册应成功（只是不给管理员），got %d body=%v", code, body)
	}
	if body["first_admin"] != false {
		t.Errorf("未出示引导密钥时 first_admin 必须为 false，got %v", body["first_admin"])
	}
	if store.HasAdmin() {
		t.Error("未出示引导密钥的注册不得产生管理员账号")
	}
}

// TestRegisterBootstrapCreatesAdmin 出示正确引导密钥 → 建出管理员。
func TestRegisterBootstrapCreatesAdmin(t *testing.T) {
	store, a, mux := newP3App(t)
	defer store.Close()

	code, body := registerWith(t, mux, "owner", "0wner-Pass-2026", a.key)
	if code != http.StatusOK {
		t.Fatalf("注册应成功，got %d body=%v", code, body)
	}
	if body["first_admin"] != true {
		t.Errorf("出示正确引导密钥后 first_admin 应为 true，got %v", body["first_admin"])
	}
	if !store.HasAdmin() {
		t.Error("出示正确引导密钥后应存在管理员账号")
	}
}

// TestRegisterWrongBootstrapDoesNotLockOut 抢注者**无法把初始化锁死**。
//
// 判据看的是「有没有管理员」而不是「有没有用户」：否则抢注者只要先建一个小号，
// 合法的主人在提供密钥时也会因为 n>0 而再也提不了权 —— 这是修复本身最容易踩的坑。
func TestRegisterWrongBootstrapDoesNotLockOut(t *testing.T) {
	store, a, mux := newP3App(t)
	defer store.Close()

	// 1) 抢注者：错误密钥
	if code, body := registerWith(t, mux, "squatter", "Squ@tter-2026x", "guess-wrong-key"); code != http.StatusOK {
		t.Fatalf("抢注注册应成功，got %d body=%v", code, body)
	}
	if store.HasAdmin() {
		t.Fatal("错误引导密钥不得产生管理员")
	}
	// 2) 真正的主人随后注册：仍能拿到管理员（初始化没有被锁死）
	code, body := registerWith(t, mux, "owner", "0wner-Pass-2026", a.key)
	if code != http.StatusOK || body["first_admin"] != true {
		t.Fatalf("抢注后合法初始化仍应成功，got %d body=%v", code, body)
	}
	if !store.HasAdmin() {
		t.Error("合法初始化后应存在管理员")
	}
	// 3) 已有管理员后再出示密钥 → 不再颁发第二个
	code, body = registerWith(t, mux, "second", "Sec0nd-Pass-2026", a.key)
	if code != http.StatusOK || body["first_admin"] != false {
		t.Errorf("已有管理员时不得再颁发管理员，got %d body=%v", code, body)
	}
}

// TestCreateUserAsDefaultsToUser 建库层不再隐式提权（角色是调用方的决定）。
func TestCreateUserAsDefaultsToUser(t *testing.T) {
	store, _, _ := newP3App(t)
	defer store.Close()

	u, err := store.CreateUser("plain-first", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if u.IsAdmin() {
		t.Error("CreateUser 不得再把首个用户提为管理员（角色应由调用方经 CreateUserAs 指定）")
	}
	// 非法角色值必须回落到普通用户，不能因为打了个错字就造出一个管理员。
	u2, err := store.CreateUserAs("typo-role", "x", "", "root")
	if err != nil {
		t.Fatalf("CreateUserAs: %v", err)
	}
	if u2.IsAdmin() {
		t.Error("未知角色值必须回落为普通用户")
	}
}

// ---------- P3-11：/metrics 鉴权 + /healthz 最小化 ----------

func TestMetricsRequiresAdminAuth(t *testing.T) {
	store, a, mux := newP3App(t)
	defer store.Close()

	if code, body := call(t, mux, "GET", "/metrics", "", ""); code != http.StatusForbidden {
		t.Errorf("匿名 /metrics 应 403，got %d body=%.120s", code, body)
	}
	if code, body := call(t, mux, "GET", "/metrics", "", a.key); code != http.StatusOK {
		t.Errorf("带管理密钥的 /metrics 应 200，got %d body=%.120s", code, body)
	}
	// -metrics-public 逃生门：开启后回到公开（给「抓取端就在本机」的场景）。
	a.metricsPublic = true
	if code, _ := call(t, mux, "GET", "/metrics", "", ""); code != http.StatusOK {
		t.Errorf("-metrics-public 开启后 /metrics 应 200，got %d", code)
	}
}

func TestHealthzPublicButMinimal(t *testing.T) {
	store, a, mux := newP3App(t)
	defer store.Close()

	code, body := call(t, mux, "GET", "/healthz", "", "")
	if code != http.StatusOK {
		t.Fatalf("/healthz 必须公开可用（探活要靠它），got %d", code)
	}
	if !strings.Contains(body, `"status":"ok"`) {
		t.Errorf("/healthz 应回 status=ok，got %.120s", body)
	}
	// 未鉴权不得泄漏版本号：它是「这台机器在用哪个已知有洞的版本」的指路牌。
	if strings.Contains(body, "version") {
		t.Errorf("未鉴权 /healthz 不得包含 version，got %.160s", body)
	}

	// 已鉴权则附带版本与时间（运维对版本/时钟要用）。
	_, body2 := call(t, mux, "GET", "/healthz", "", a.key)
	if !strings.Contains(body2, "version") || !strings.Contains(body2, version) {
		t.Errorf("带管理密钥的 /healthz 应附带 version，got %.160s", body2)
	}
}

// ---------- P3-8：路径导入白名单 ----------

func TestResolveImportPathWhitelist(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	roots := filepath.Join(dir, "roots")
	for _, d := range []string{dataDir, roots} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", d, err)
		}
	}
	allowed := filepath.Join(roots, "accounts.json")
	if err := os.WriteFile(allowed, []byte("[]"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	secret := filepath.Join(dataDir, "secret.key")
	if err := os.WriteFile(secret, []byte("k"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	a := &app{dataDir: dataDir, importRoots: []string{roots}}

	// 正例：白名单目录内的文件放行，并返回归一化绝对路径。
	got, err := a.resolveImportPath(allowed)
	if err != nil {
		t.Fatalf("白名单内路径应放行，got err=%v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("应返回绝对路径，got %q", got)
	}

	// 反例 1：数据目录内的 secret.key（管理员会话失窃即等于交出主密钥）。
	if _, err := a.resolveImportPath(secret); err == nil {
		t.Error("数据目录内的文件必须被拒绝")
	}
	// 反例 2：白名单之外。
	if _, err := a.resolveImportPath(filepath.Join(dir, "outside.json")); err == nil {
		t.Error("白名单之外的路径必须被拒绝")
	}
	// 反例 3：用 .. 从白名单目录里爬出去（前缀比较的经典绕过）。
	if _, err := a.resolveImportPath(filepath.Join(roots, "..", "escape.json")); err == nil {
		t.Error(".. 逃逸必须被拒绝")
	}
	// 反例 4：空路径。
	if _, err := a.resolveImportPath("   "); err == nil {
		t.Error("空路径必须被拒绝")
	}
}

// TestResolveImportPathRejectsSymlinkEscape 白名单目录里放一个指向机密的软链，
// 纯前缀比较会放行 —— 必须靠 EvalSymlinks 穿透后再判。
//
// Windows 上创建符号链接需要特权/开发者模式，失败则跳过（不算缺陷）。
func TestResolveImportPathRejectsSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	roots := filepath.Join(dir, "roots")
	for _, d := range []string{dataDir, roots} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}
	secret := filepath.Join(dataDir, "secret.key")
	if err := os.WriteFile(secret, []byte("k"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	link := filepath.Join(roots, "innocent.json")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("本机无法创建符号链接（%v），跳过软链逃逸用例", err)
	}

	a := &app{dataDir: dataDir, importRoots: []string{roots}}
	if _, err := a.resolveImportPath(link); err == nil {
		t.Error("指向数据目录的软链必须被拒绝（EvalSymlinks 未生效）")
	}
}

// TestResolveImportPathRejectsDataDirViaSymlinkAlias 同一个数据目录换个**别名**写法
// 也必须被拦下（v0.24.2 真机发现：本地工作区本身就是符号链接，应用算出的数据目录
// 与请求里写的是两个不同盘符的同一条路径，纯前缀比较会得出「无关」的结论）。
//
// Windows 上创建符号链接需要特权/开发者模式，失败则跳过。
func TestResolveImportPathRejectsDataDirViaSymlinkAlias(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "realdata")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "secret.key"), []byte("k"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ok.json"), []byte("[]"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(dataDir, alias); err != nil {
		t.Skipf("本机无法创建符号链接（%v），跳过别名用例", err)
	}

	a := &app{dataDir: dataDir, importRoots: []string{dir}}
	if _, err := a.resolveImportPath(filepath.Join(alias, "secret.key")); err == nil {
		t.Error("经符号链接别名访问数据目录内的文件必须被拒绝")
	}
	// 对照：白名单目录里的普通文件仍要放行 —— 防别名不能变成误杀。
	if _, err := a.resolveImportPath(filepath.Join(dir, "ok.json")); err != nil {
		t.Errorf("白名单内的普通文件应放行，got err=%v", err)
	}
}

// TestCanonPathHandlesMissingFile 归一化要能处理「文件还不存在」：
// 否则第一次导入（账号文件尚未落盘）会得到一条指向错误方向的报错。
func TestCanonPathHandlesMissingFile(t *testing.T) {
	dir := t.TempDir()
	got := canonPath(filepath.Join(dir, "missing.json"))
	want := filepath.Join(canonPath(dir), "missing.json")
	if !strings.EqualFold(got, want) {
		t.Errorf("canonPath(不存在文件) = %q，期望 %q", got, want)
	}
	if canonPath("   ") != "" {
		t.Error("空白路径应归一化为空串")
	}
}

// TestPathInsideDirAcrossSpellings 同一个目录的**不同写法**必须判定为「在里面」。
//
// 这是 v0.24.2 真机验证抓到的真实缺陷：本机工作区同时存在 C:\ 与 D:\ 两种等价写法，
// 而 filepath.EvalSymlinks 对 junction 解不开 —— 纯路径前缀比较会得出「两者无关」，
// 于是守卫静默失效。现在主判据换成文件标识（os.SameFile），跨写法成立。
func TestPathInsideDirAcrossSpellings(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	f := filepath.Join(sub, "a.json")
	if err := os.WriteFile(f, []byte("[]"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// 目录自身的写法差异：多余分隔符、冗余 . 段、绕一圈的 .. 段。
	if !pathInsideDir(f, sub+string(filepath.Separator)) {
		t.Errorf("带尾部分隔符的同一目录应判定为包含")
	}
	if !pathInsideDir(f, filepath.Join(dir, ".", "sub")) {
		t.Errorf("带冗余 . 段的同一目录应判定为包含")
	}
	if !pathInsideDir(filepath.Join(sub, "..", "sub", "a.json"), sub) {
		t.Errorf("路径含 .. 段但归一化后仍在目录内，应判定为包含")
	}
	// 目录自身不算「在里面」；父目录不算。
	if pathInsideDir(sub, sub) {
		t.Error("目录自身不应判定为「在目录内」")
	}
	if pathInsideDir(dir, sub) {
		t.Error("父目录不应判定为「在子目录内」")
	}
	if pathInsideDir(f, dir) == false {
		// 反向确认：文件确实在 dir 里（上面 sub 是 dir 的子目录）——
		// 这条只是防止「全判 false」也能让上面几条通过。
		t.Error("文件应在 dir 之内（子目录属于父目录范围）")
	}
}

// ---------- 附带：with..in 判定的边界 ----------

func TestWithinDir(t *testing.T) {
	base := t.TempDir()
	sub := filepath.Join(base, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if !withinDir(filepath.Join(sub, "a.json"), sub) {
		t.Error("子目录内的文件应判为「在目录内」")
	}
	if withinDir(sub, sub) {
		t.Error("目录自身不算「在目录内」（避免把白名单目录本身当文件读）")
	}
	if withinDir(base, sub) {
		t.Error("父目录不得判为在子目录内")
	}
	if withinDir(filepath.Join(sub, "..", "b.json"), sub) {
		t.Error(".. 逃逸不得判为在目录内")
	}
}
