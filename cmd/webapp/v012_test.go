// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestApp 造一个带库 / 池 / 加密箱的最小 app，供 v0.12.0 新接口测试使用。
func newTestApp(t *testing.T, accounts ...*Account) (*Store, *Proxy, *app) {
	t.Helper()
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "v012.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	for _, a := range accounts {
		if err := store.UpsertAccount(a); err != nil {
			t.Fatalf("UpsertAccount: %v", err)
		}
	}
	px := NewProxy(store, "")
	sec, err := LoadOrCreateSecret(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatalf("LoadOrCreateSecret: %v", err)
	}
	// 线路准入验证用桩（v0.31.47）：这些用例测的是"绑谁、绑不绑"，
	// 不是"这条线路能不能出网" —— 用真探测会让用例依赖外网。
	return store, px, &app{store: store, proxy: px, secret: sec, key: "v012-admin-key",
		egressVerify: func(string) (string, error) { return "203.0.113.9", nil }}
}

// TestUserMatchesQuery 需求 1：用户名子串（不区分大小写）/ ID 精确匹配。
func TestUserMatchesQuery(t *testing.T) {
	u := &User{ID: 42, Username: "Alice"}
	cases := []struct {
		q    string
		want bool
	}{
		{"", true},
		{"ali", true},   // 子串匹配
		{"ALICE", true}, // 大小写不敏感（handler 统一小写化后传入）
		{"alice", true},
		{"bob", false},
		{"42", true}, // ID 精确
		{"4", false}, // ID 非精确不匹配
		{"99", false},
	}
	for _, c := range cases {
		if got := userMatchesQuery(u, strings.ToLower(c.q)); got != c.want {
			t.Errorf("userMatchesQuery(%q)=%v，期望 %v", c.q, got, c.want)
		}
	}
	if userMatchesQuery(nil, "x") {
		t.Error("nil 用户不应命中")
	}
}

// TestDesktopRealmGroupMapping 桌面端 edition/group → realm/分组名映射。
func TestDesktopRealmGroupMapping(t *testing.T) {
	if r := desktopRealm("domestic", "www.codebuddy.cn"); r != "cn" {
		t.Errorf("domestic 应为 cn，得 %s", r)
	}
	if r := desktopRealm("international", "www.workbuddy.ai"); r != "global" {
		t.Errorf("international 应为 global，得 %s", r)
	}
	// edition 缺失时按 domain 推断。
	if r := desktopRealm("", "www.workbuddy.ai"); r != "global" {
		t.Errorf("按 domain 应推断为 global，得 %s", r)
	}
	if g := desktopGroup("proxy"); g != "反代" {
		t.Errorf("proxy 应映射为反代，得 %s", g)
	}
	if g := desktopGroup("desktop"); g != "桌面端" {
		t.Errorf("desktop 应映射为桌面端，得 %s", g)
	}
	if g := desktopGroup(""); g != "" {
		t.Errorf("空分组应保持空，得 %s", g)
	}
}

// TestDesktopImport 需求 4：桌面端 accounts.json 导入进号池，归属平台、参与反代。
func TestDesktopImport(t *testing.T) {
	store, px, a := newTestApp(t)
	defer store.Close()

	// 模拟桌面端导出：auth_raw 内嵌 token/domain，顶层 expiresAt 为毫秒。
	// 注意：桌面端早期版本把 auth_raw/profile_raw 存为「字符串化的 JSON」，
	// 新版本直接存「内联对象」——两种都要能吃（第 2 条用对象形态验证）。
	file := map[string]any{
		"uid":          "u-desktop-1",
		"nickname":     "演示账号",
		"domain":       "www.codebuddy.cn",
		"edition":      "domestic",
		"group":        "proxy",
		"access_token": "top-access",
		"expiresAt":    int64(1793980812013),
		"auth_raw":     `{"accessToken":"raw-access","refreshToken":"raw-refresh","domain":"www.codebuddy.cn"}`,
		"profile_raw":  `{"nickname":"演示账号(profile)","uid":"u-desktop-1"}`,
		"enterpriseId": nil,
	}
	fileObj := map[string]any{
		"uid":          "u-desktop-2",
		"nickname":     "对象形态",
		"domain":       "www.codebuddy.cn",
		"edition":      "domestic",
		"group":        "desktop",
		"access_token": "top-access-2",
		"expiresAt":    int64(1793980812013),
		"auth_raw":     map[string]any{"accessToken": "obj-access", "refreshToken": "obj-refresh", "domain": "www.codebuddy.cn"},
		"profile_raw":  map[string]any{"nickname": "对象形态(profile)", "uid": "u-desktop-2"},
		"enterpriseId": "ent-9",
	}
	path := filepath.Join(t.TempDir(), "accounts.json")
	raw, _ := json.Marshal([]any{file, fileObj})
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("写测试文件: %v", err)
	}

	res, err := a.importDesktopFile(path)
	if err != nil {
		t.Fatalf("importDesktopFile: %v", err)
	}
	if res.Total != 2 || res.Imported != 2 || res.Updated != 0 || res.Skipped != 0 {
		t.Fatalf("导入计数异常: %+v", res)
	}

	acc, err := store.GetAccount("u-desktop-1")
	if err != nil || acc == nil {
		t.Fatalf("账号未入库: %v", err)
	}
	if acc.Realm != "cn" {
		t.Errorf("realm 应为 cn，得 %s", acc.Realm)
	}
	if acc.Group != "反代" {
		t.Errorf("分组应映射为反代，得 %s", acc.Group)
	}
	if acc.AccessToken != "raw-access" || acc.RefreshToken != "raw-refresh" {
		t.Errorf("应优先采用 auth_raw 内的 token，得 access=%q refresh=%q", acc.AccessToken, acc.RefreshToken)
	}
	if acc.ExpiresAt != 1793980812 {
		t.Errorf("expiresAt 应由毫秒转秒，得 %d", acc.ExpiresAt)
	}
	if acc.OwnerUserID != 0 {
		t.Errorf("导入账号应归平台自持，得 owner=%d", acc.OwnerUserID)
	}
	// 平台范围可选（owner=0 恒参与反代）。
	_ = px.SyncAccounts()
	if !px.allowsSelection("u-desktop-1", pickScope{}) {
		t.Error("导入的账号应参与平台反代")
	}

	// 第 2 条（内联对象形态）必须同样解析成功。
	acc2, err := store.GetAccount("u-desktop-2")
	if err != nil || acc2 == nil {
		t.Fatalf("对象形态账号未入库: %v", err)
	}
	if acc2.AccessToken != "obj-access" || acc2.RefreshToken != "obj-refresh" {
		t.Errorf("对象形态应优先采用 auth_raw 内 token，得 access=%q refresh=%q", acc2.AccessToken, acc2.RefreshToken)
	}
	if acc2.Nickname != "对象形态(profile)" {
		t.Errorf("对象形态昵称应取 profile_raw，得 %q", acc2.Nickname)
	}
	if acc2.Group != "桌面端" {
		t.Errorf("desktop 分组应映射为桌面端，得 %s", acc2.Group)
	}
	if acc2.EnterpriseID != "ent-9" {
		t.Errorf("enterpriseId 应保留，得 %q", acc2.EnterpriseID)
	}

	// 再次导入 → 计为更新而非新增。
	res2, err := a.importDesktopFile(path)
	if err != nil {
		t.Fatalf("二次 importDesktopFile: %v", err)
	}
	if res2.Imported != 0 || res2.Updated != 2 {
		t.Fatalf("二次导入应为更新 2 条，得 %+v", res2)
	}
}

// TestDesktopImportHandler 需求 4：HTTP 入口（管理侧）可导入并返回计数。
func TestDesktopImportHandler(t *testing.T) {
	store, _, a := newTestApp(t)
	defer store.Close()
	path := filepath.Join(t.TempDir(), "accounts.json")
	body := `[{"uid":"u-http","nickname":"痕","domain":"www.codebuddy.cn","edition":"domestic","access_token":"tok"}]`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("写测试文件: %v", err)
	}
	// v0.24.2（P3-8）：路径导入已加**目录白名单**（默认只允许 %USERPROFILE%\.wb-switch，
	// 数据目录恒不可读）。手工装配的 app 没有白名单，故这里必须显式声明测试目录 ——
	// 这正是新契约的一部分：能读哪，是配置说了算，不是「有管理员权限就能读整个盘」。
	a.importRoots = []string{filepath.Dir(path)}
	req := httptest.NewRequest(http.MethodPost, "/api/admin/accounts/import-desktop",
		strings.NewReader(`{"path":`+jsonString(path)+`}`))
	rr := httptest.NewRecorder()
	a.handleImportDesktopAccounts(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("期望 200，得 %d body=%s", rr.Code, rr.Body.String())
	}
	var out struct {
		Imported int `json:"imported"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out.Imported != 1 {
		t.Fatalf("imported 应为 1，得 %d", out.Imported)
	}
	if acc, _ := store.GetAccount("u-http"); acc == nil {
		t.Fatal("HTTP 导入的账号未入库")
	}
}

// TestPoolPreferPoolIn 需求 6：平台范围优先命中选择「已入池」的代挂账号，平台自持号兜底。
func TestPoolPreferPoolIn(t *testing.T) {
	// v0.13.0 起该优先级由「消耗分组」表达：h_in（代挂+已入池）推断为「用户申请」组，
	// plat（平台自持）推断为「反代」组，平台选号按 用户申请 → 反代 逐组取号。
	store, px, _ := newTestApp(t,
		&Account{UID: "plat", Realm: "cn", AccessToken: "a"},                                  // 平台自持 → 反代组
		&Account{UID: "h_in", Realm: "cn", AccessToken: "a", OwnerUserID: 7, PoolState: "in"}, // 已入池代挂 → 用户申请组
	)
	defer store.Close()
	_ = px.SyncAccounts()

	// 用户申请组可用时：反复选号必须始终命中它（反代组退居次级）。
	for i := 0; i < 20; i++ {
		got := px.pickExcluding(map[string]bool{}, "deepseek", pickScope{})
		if got == nil || got.UID != "h_in" {
			t.Fatalf("第 %d 次选号应命中「用户申请」组账号 h_in，得 %v", i, got)
		}
	}

	// 停用优先组账号 → 落到次级「反代」组。
	if err := store.SetDisabled("h_in", true); err != nil {
		t.Fatalf("SetDisabled: %v", err)
	}
	_ = px.SyncAccounts()
	got := px.pickExcluding(map[string]bool{}, "deepseek", pickScope{})
	if got == nil || got.UID != "plat" {
		t.Fatalf("优先组不可用时应落到「反代」组账号 plat，得 %v", got)
	}
}

// TestCreateKeyCount 需求 2：一次生成多把 Key。
func TestCreateKeyCount(t *testing.T) {
	store, _, a := newTestApp(t)
	defer store.Close()
	u, err := store.CreateUser("multikey", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/keys",
		strings.NewReader(`{"name":"笔记本","count":3}`))
	req = req.WithContext(context.WithValue(req.Context(), ctxUserKey, u))
	rr := httptest.NewRecorder()
	a.handleCreateKey(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("期望 200，得 %d body=%s", rr.Code, rr.Body.String())
	}
	var out struct {
		Count int `json:"count"`
		Keys  []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
			Key  string `json:"key"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if out.Count != 3 || len(out.Keys) != 3 {
		t.Fatalf("应生成 3 把 Key，得 count=%d len=%d", out.Count, len(out.Keys))
	}
	seen := map[string]bool{}
	for _, k := range out.Keys {
		if k.Key == "" || seen[k.Key] {
			t.Fatalf("Key 明文应非空且互不相同：%+v", k)
		}
		seen[k.Key] = true
		if !strings.HasPrefix(k.Name, "笔记本-") {
			t.Errorf("多 Key 名称应带序号后缀，得 %q", k.Name)
		}
	}
	// 库内应确实落 3 把。
	keys, _ := store.ListAPIKeys(u.ID)
	if len(keys) != 3 {
		t.Fatalf("库内应有 3 把 Key，得 %d", len(keys))
	}
}

// jsonString 把字符串安全转成 JSON 字面量（路径含反斜杠）。
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
