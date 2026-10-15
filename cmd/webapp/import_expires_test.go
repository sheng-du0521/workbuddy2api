// 迁移通道的量纲陷阱回归（v0.31.4）。
//
// 背景：合并两个产品的第一步要把旧网关的账号搬进 webapp，而两边的账号文件里
// `expiresAt` 的**量纲不同、键名相同**：
//
//	桌面端 accounts.json  → 毫秒（同文件里 createdAt / refreshedAt 也是毫秒）
//	旧网关 auths/*.json    → 秒（auth.ExpiresAt // Unix 秒）
//
// 把秒当毫秒除以 1000 → 1970 年 → 每个账号一落库就"已过期"，被立刻刷新。
// 百号部署会在几秒内打出一百次 refresh（对上游是一次明显的批量行为）。
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalizeExpiresSec(t *testing.T) {
	const sec = int64(1792087385) // 2026-10-16 前后（秒）
	cases := []struct {
		name string
		in   int64
		want int64
	}{
		{"秒（auths/*.json）", sec, sec},
		{"毫秒（桌面端导出）", sec * 1000, sec},
		{"零值", 0, 0},
		{"负值（脏数据）", -1, 0},
		{"分界下沿（仍算秒）", 1e11 - 1, 1e11 - 1},
		{"分界上沿（算毫秒）", 1e11 + 1000, (1e11 + 1000) / 1000},
	}
	for _, c := range cases {
		if got := normalizeExpiresSec(c.in); got != c.want {
			t.Errorf("%s: normalizeExpiresSec(%d)=%d want %d", c.name, c.in, got, c.want)
		}
	}
}

// TestImportAuthsStyleSecondsKeepsExpiry 端到端：导入 auths 风格的 JSON（秒）
// 后，库里的到期时刻必须是原值（而不是被当成毫秒而塌到 1970）。
func TestImportAuthsStyleSecondsKeepsExpiry(t *testing.T) {
	store, px, a := newTestApp(t)
	t.Cleanup(func() { _ = store.Close() })
	a.key = "import-admin-key"
	mux := a.routes()

	const expiresSec = int64(1792087385)
	// 旧网关 auths/*.json 的扁平形态（单对象；uid/access_token/refresh_token/expiresAt 同键名）。
	one := map[string]any{
		"uid": "auths-style-0001", "nickname": "旧网关号",
		"access_token": "at-1", "refresh_token": "rt-1",
		"expiresAt": expiresSec,
	}
	inner, _ := json.Marshal(one)
	body, _ := json.Marshal(map[string]any{"json": string(inner), "force": true})

	req := httptest.NewRequest(http.MethodPost, "/api/admin/accounts/import-desktop", strings.NewReader(string(body)))
	req.Header.Set("X-Admin-Key", "import-admin-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("导入失败 code=%d body=%s", rec.Code, rec.Body.String())
	}

	acc, err := store.GetAccount("auths-style-0001")
	if err != nil || acc == nil {
		t.Fatalf("账号未入库：%v", err)
	}
	if acc.ExpiresAt != expiresSec {
		t.Errorf("expiresAt=%d want %d（秒被当成毫秒会让账号一落库就判过期，触发批量 refresh）",
			acc.ExpiresAt, expiresSec)
	}
	if acc.AccessToken == "" || acc.RefreshToken == "" {
		t.Error("token 未入库")
	}
	// 落池后应可被选号（凭证齐、未停用、未冷却）。
	px.pool.SetCredits("auths-style-0001", 1000)
	if st, ok := px.pool.Status("auths-style-0001"); !ok || st.Disabled || st.ManualDisabled {
		t.Errorf("导入的账号应可用：ok=%v st=%+v", ok, st)
	}
}
