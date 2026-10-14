// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// writeFileHelper 写测试用文件（避免测试里散落 os 调用细节）。
func writeFileHelper(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o600)
}

// TestParseDesktopAccountsFormats 格式兼容表驱动：批量导入的入口必须能吃下
// 用户可能粘贴的四种形态（数组 / accounts 包装 / 其它包装 / 单对象），
// 以及 Windows 记事本另存为 UTF-8 时带的 BOM。
func TestParseDesktopAccountsFormats(t *testing.T) {
	one := `{"uid":"u1","nickname":"甲","access_token":"at"}`
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"数组", `[` + one + `,{"uid":"u2","access_token":"at2"}]`, 2},
		{"accounts 包装", `{"accounts":[` + one + `]}`, 1},
		{"data 包装", `{"data":[` + one + `,{"uid":"u2","access_token":"at2"}]}`, 2},
		{"list 包装", `{"list":[` + one + `]}`, 1},
		{"items 包装", `{"items":[` + one + `]}`, 1},
		{"单对象", one, 1},
		{"空数组", `[]`, 0},
		{"前后空白", "  \n " + `[` + one + `]` + " \n ", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseDesktopAccounts([]byte(c.in))
			if err != nil {
				t.Fatalf("parseDesktopAccounts(%s): %v", c.name, err)
			}
			if len(got) != c.want {
				t.Fatalf("%s：应解析 %d 条，得 %d", c.name, c.want, len(got))
			}
		})
	}

	// BOM：直接 json.Unmarshal 会报 invalid character 'ï'，入口必须剥离。
	bom := append([]byte{0xEF, 0xBB, 0xBF}, []byte(`[`+one+`]`)...)
	if got, err := parseDesktopAccounts(bom); err != nil || len(got) != 1 {
		t.Fatalf("带 BOM 的 JSON 应能解析，得 %d 条 err=%v", len(got), err)
	}

	// 误差路径：空内容 / 非法 JSON / 顶层是数字。
	for _, bad := range []string{"", "   ", "{不是json", `"just a string"`, "123"} {
		if _, err := parseDesktopAccounts([]byte(bad)); err == nil {
			t.Errorf("非法内容 %q 应报错", bad)
		}
	}
}

// TestImportJSONRefreshOnlyAccount 只带 refresh_token 的账号应能导入
// （出站前 refreshIfNeeded 会先换出 access token），但两者都缺必须跳过。
func TestImportJSONRefreshOnlyAccount(t *testing.T) {
	store, _, a := newTestApp(t)
	defer store.Close()

	body := `[
	  {"uid":"r-only","nickname":"只有refresh","refresh_token":"rt-1","domain":"www.codebuddy.cn"},
	  {"uid":"no-cred","nickname":"无凭证"},
	  {"nickname":"连uid都没有","access_token":"at"}
	]`
	res, err := a.importDesktopJSON([]byte(body))
	if err != nil {
		t.Fatalf("importDesktopJSON: %v", err)
	}
	if res.Total != 3 || res.Imported != 1 || res.Skipped != 2 {
		t.Fatalf("计数异常（应 3 条 / 新增 1 / 跳过 2）：%+v", res)
	}
	if res.Source != "json" {
		t.Errorf("来源应标注 json，得 %q", res.Source)
	}
	acc, err := store.GetAccount("r-only")
	if err != nil || acc == nil {
		t.Fatalf("只有 refresh_token 的账号应入库: %v", err)
	}
	if acc.RefreshToken != "rt-1" || acc.AccessToken != "" {
		t.Errorf("凭证应原样保存，得 access=%q refresh=%q", acc.AccessToken, acc.RefreshToken)
	}
	if len(res.Errors) != 2 {
		t.Errorf("应给出 2 条跳过原因，得 %v", res.Errors)
	}
}

// TestImportHandlerJSONMode 走 HTTP：json 正文优先于 path（远端部署的关键，
// 服务端读不到用户本机文件），且空内容/0 条要给出可读错误。
func TestImportHandlerJSONMode(t *testing.T) {
	store, px, a := newTestApp(t)
	defer store.Close()

	call := func(body string) (int, map[string]any) {
		req := httptest.NewRequest(http.MethodPost, "/api/admin/accounts/import-desktop", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		a.handleImportDesktopAccounts(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}

	// json 优先：同时给 json 与一个必然不存在的 path，应走 json 成功。
	code, out := call(`{"json":"[{\"uid\":\"j1\",\"nickname\":\"粘贴来的\",\"access_token\":\"at\"}]","path":"Z:/definitely/missing.json"}`)
	if code != http.StatusOK {
		t.Fatalf("json 模式应成功，得 %d %v", code, out)
	}
	if out["imported"] != float64(1) || out["source"] != "json" {
		t.Fatalf("应新增 1 条且来源为 json，得 %v", out)
	}
	if err := px.SyncAccounts(); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}
	if !px.allowsSelection("j1", pickScope{}) {
		t.Error("导入的账号应参与平台反代")
	}

	// 二次导入同一条 → 计更新。
	code, out = call(`{"json":"[{\"uid\":\"j1\",\"access_token\":\"at\"}]"}`)
	if code != http.StatusOK || out["updated"] != float64(1) || out["imported"] != float64(0) {
		t.Fatalf("二次导入应为更新 1 条，得 %d %v", code, out)
	}

	// 空数组 → 明确报「0 条」，而不是假装成功。
	code, out = call(`{"json":"[]"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("0 条应 400，得 %d %v", code, out)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "0 条") {
		t.Errorf("错误信息应说明解析到 0 条，得 %q", msg)
	}

	// 非法 JSON → 400 且信息可读。
	code, out = call(`{"json":"{不是json"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("非法 JSON 应 400，得 %d", code)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "解析") {
		t.Errorf("错误信息应可读，得 %q", msg)
	}
}

// TestImportFileStillWorks 回归：原有「服务端路径」入口未被破坏。
func TestImportFileStillWorks(t *testing.T) {
	store, _, a := newTestApp(t)
	defer store.Close()

	dir := t.TempDir()
	path := dir + "/accounts.json"
	body := `[{"uid":"f1","nickname":"来自文件","access_token":"at"}]`
	if err := writeFileHelper(path, body); err != nil {
		t.Fatalf("写测试文件: %v", err)
	}
	res, err := a.importDesktopFile(path)
	if err != nil {
		t.Fatalf("importDesktopFile: %v", err)
	}
	if res.Source != "file" || res.Imported != 1 {
		t.Fatalf("文件模式应新增 1 条且来源为 file：%+v", res)
	}
	if res.Path != path {
		t.Errorf("应回填路径，得 %q", res.Path)
	}
}
