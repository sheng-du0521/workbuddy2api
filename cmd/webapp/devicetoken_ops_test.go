// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// devTokAudit 调一次设备凭证审计接口。
func devTokAudit(t *testing.T, a *app) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	a.handleDeviceTokenAudit(rec, httptest.NewRequest(http.MethodGet, "/api/admin/device-tokens", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("审计状态码 %d：%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析审计响应：%v", err)
	}
	return out
}

// postDevTok 调一次单号写入接口，返回原始响应体（供"原文不得回显"断言）。
func postDevTok(t *testing.T, a *app, uid, token string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"token": token})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/accounts/"+uid+"/device-token", bytes.NewReader(body))
	req.SetPathValue("uid", uid)
	rec := httptest.NewRecorder()
	a.handleAccountDeviceToken(rec, req)
	return rec.Code, rec.Body.String()
}

// TestDeviceTokenAuditThreeStates 三态计数与结论文案（本功能的核心口径）。
func TestDeviceTokenAuditThreeStates(t *testing.T) {
	t.Setenv(envDeviceToken, "") // 排除环境变量干扰，避免测试机恰好设了该变量
	store, px, a := newTestApp(t,
		&Account{UID: "dt-has", Nickname: "有凭证", AccessToken: "t", DeviceToken: "tok-A"},
		&Account{UID: "dt-none", Nickname: "没凭证", AccessToken: "t"},
	)
	defer store.Close()
	defer px.Close()

	out := devTokAudit(t, a)
	bs, _ := out["by_state"].(map[string]any)
	if bs["account"].(float64) != 1 || bs["none"].(float64) != 1 || bs["global"].(float64) != 0 {
		t.Fatalf("三态计数不符：%v", bs)
	}
	if out["verdict"] != "partially-shared" && out["verdict"] != "ok" {
		t.Fatalf("verdict 异常：%v", out["verdict"])
	}
}

// TestDeviceTokenAuditDetectsShared —— 「一个凭证被多个号共用」必须被点名。
//
// 这是本功能真正的价值点：全池共用一个凭证 = 27 个账号声称自己是同一台设备，
// 比"全池都不带"更糟（后者只是一类差异，前者是可以一次性聚合出整池的强关联特征）。
func TestDeviceTokenAuditDetectsShared(t *testing.T) {
	t.Setenv(envDeviceToken, "")
	const same = "shared-token-xyz"
	store, px, a := newTestApp(t,
		&Account{UID: "sh-1", Nickname: "甲", AccessToken: "t", DeviceToken: same},
		&Account{UID: "sh-2", Nickname: "乙", AccessToken: "t", DeviceToken: same},
		&Account{UID: "sh-3", Nickname: "丙", AccessToken: "t", DeviceToken: "other"},
	)
	defer store.Close()
	defer px.Close()

	out := devTokAudit(t, a)
	shared, _ := out["shared"].([]any)
	if len(shared) != 1 {
		t.Fatalf("应恰好有 1 个共用分组，实得 %d：%v", len(shared), shared)
	}
	g := shared[0].(map[string]any)
	if g["count"].(float64) != 2 || g["kind"] != "account" {
		t.Fatalf("共用分组内容不符：%v", g)
	}
	if out["verdict"] != "partially-shared" {
		t.Fatalf("verdict 应为 partially-shared，实得 %v", out["verdict"])
	}
}

// TestDeviceTokenAuditGlobalOnlyIsWorseThanNone —— 全局兜底独占时给出更强的结论。
func TestDeviceTokenAuditGlobalOnlyIsWorseThanNone(t *testing.T) {
	t.Setenv(envDeviceToken, "global-token-abc")
	store, px, a := newTestApp(t,
		&Account{UID: "g-1", Nickname: "甲", AccessToken: "t"},
		&Account{UID: "g-2", Nickname: "乙", AccessToken: "t"},
	)
	defer store.Close()
	defer px.Close()

	out := devTokAudit(t, a)
	if out["verdict"] != "shared-global" {
		t.Fatalf("verdict 应为 shared-global，实得 %v（by_state=%v）", out["verdict"], out["by_state"])
	}
	shared, _ := out["shared"].([]any)
	if len(shared) != 1 || shared[0].(map[string]any)["kind"] != "global" {
		t.Fatalf("应有一个 global 共用分组：%v", shared)
	}
}

// TestDeviceTokenNeverEchoed —— 凭证原文绝不出现在任何响应里（含错误路径）。
func TestDeviceTokenNeverEchoed(t *testing.T) {
	t.Setenv(envDeviceToken, "")
	const secret = "SUPER-SECRET-DEVICE-TOKEN-0123456789"
	store, px, a := newTestApp(t,
		&Account{UID: "echo-1", Nickname: "回显测试", AccessToken: "t"},
	)
	defer store.Close()
	defer px.Close()

	code, body := postDevTok(t, a, "echo-1", secret)
	if code != http.StatusOK {
		t.Fatalf("写入失败 %d：%s", code, body)
	}
	if strings.Contains(body, secret) {
		t.Fatalf("响应回显了凭证原文：%s", body)
	}
	if !strings.Contains(body, "\"token_set\":true") {
		t.Fatalf("应回 token_set=true：%s", body)
	}
	// 审计接口同样不得回显。
	rec := httptest.NewRecorder()
	a.handleDeviceTokenAudit(rec, httptest.NewRequest(http.MethodGet, "/api/admin/device-tokens", nil))
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("审计接口回显了凭证原文")
	}
	// 清除后应回到 none。
	if code, _ := postDevTok(t, a, "echo-1", ""); code != http.StatusOK {
		t.Fatalf("清除失败：%d", code)
	}
	if got := devTokAudit(t, a)["by_state"].(map[string]any)["none"].(float64); got != 1 {
		t.Fatalf("清除后应为 none，实得 none=%v", got)
	}
}

// TestDeviceTokenRejectsOversize —— 明显粘错内容要挡住（>1KB，与出站读取口径一致）。
func TestDeviceTokenRejectsOversize(t *testing.T) {
	t.Setenv(envDeviceToken, "")
	store, px, a := newTestApp(t, &Account{UID: "big-1", Nickname: "大", AccessToken: "t"})
	defer store.Close()
	defer px.Close()

	code, body := postDevTok(t, a, "big-1", strings.Repeat("x", 2000))
	if code != http.StatusBadRequest {
		t.Fatalf("超长应 400，实得 %d：%s", code, body)
	}
	if code, _ := postDevTok(t, a, "nope", "tok"); code != http.StatusNotFound {
		t.Fatalf("不存在的账号应 404，实得 %d", code)
	}
}

// TestDeviceTokenBulkImport —— 批量粘贴：选择符支持 uid / uid 前 8 位 / 昵称，逐行容错。
func TestDeviceTokenBulkImport(t *testing.T) {
	t.Setenv(envDeviceToken, "")
	store, px, a := newTestApp(t,
		&Account{UID: "aaaaaaaa-1111-4111-8111-111111111111", Nickname: "甲号", AccessToken: "t"},
		&Account{UID: "bbbbbbbb-2222-4222-8222-222222222222", Nickname: "乙号", AccessToken: "t"},
	)
	defer store.Close()
	defer px.Close()

	text := strings.Join([]string{
		"# 注释行忽略",
		"",
		"aaaaaaaa-1111-4111-8111-111111111111 tok-full-uid",
		"bbbbbbbb tok-by-prefix",
		"乙号 tok-by-nickname",
		"ghost tok-not-found",
		"onlyone",
	}, "\n")
	body, _ := json.Marshal(map[string]string{"text": text})
	rec := httptest.NewRecorder()
	a.handleDeviceTokenBulk(rec, httptest.NewRequest(http.MethodPost, "/api/admin/device-tokens/bulk", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("批量状态码 %d：%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Applied int `json:"applied"`
		Total   int `json:"total"`
		Results []struct {
			Input string `json:"input"`
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析：%v", err)
	}
	// 注释/空行不算行 → 共 5 行；成功 3（全 uid / 前缀 / 昵称），失败 2（找不到 / 格式不对）
	if out.Total != 5 || out.Applied != 3 {
		t.Fatalf("批量结果不符：total=%d applied=%d（results=%v）", out.Total, out.Applied, out.Results)
	}
	for _, r := range out.Results {
		if !r.OK && r.Error == "" {
			t.Fatalf("失败行必须给出原因：%+v", r)
		}
		if strings.Contains(r.Input, "tok") {
			t.Fatalf("回显里不该出现凭证片段：%+v", r)
		}
	}
	// 三号各自独立 → 无共用分组。
	bs := devTokAudit(t, a)["by_state"].(map[string]any)
	if bs["account"].(float64) != 2 {
		t.Fatalf("应有两个账号各持独立凭证，实得 %v", bs)
	}
}
