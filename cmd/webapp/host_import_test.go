// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 用户端「导入账号 JSON」的回归测试（v0.31.32）。
//
// 这条路径把"重新登录"下放给了用户，风险只有三类，全部在这里钉住：
//
//	① **串号**：导入别人的 auth 文件不能把那个号变成我的（归属仍是原号主）；
//	② **越权改业务状态**：重新导入不能把入池状态/分组/出口改掉（那是号主与管理员
//	   的决定，不是一次登录能顺带改的）；
//	③ **导入不生效**：重新导入必须清掉 `token_dead_at`，否则页面一直红着，
//	   用户会以为没成功而反复导入。
package main

import (
	"net/http"
	"testing"
	"time"
)

// hostImportPayload 组一段与导出闭环的 JSON（字段名对齐 desktopAccount）。
func hostImportPayload(uid, nick, at, rt string, expMs int64) string {
	return `[{"uid":"` + uid + `","nickname":"` + nick + `","domain":"www.codebuddy.cn",` +
		`"access_token":"` + at + `","refresh_token":"` + rt + `",` +
		`"expiresAt":` + itoa(int(expMs)) + `}]`
}

// TestHostImportOwnAccounts 新号入库 / 同号重导=更新 / 失效标记被清。
func TestHostImportOwnAccounts(t *testing.T) {
	f := usApp(t)
	exp := time.Now().Add(48*time.Hour).Unix() * 1000

	// 1) 新号：直接加到我名下，且默认"未申请入池"。
	code, raw := eaReq(t, f.mux, "POST", "/api/host/accounts/import",
		`{"json":`+jsonQuote(hostImportPayload("imp-1", "导入号", "at-1", "rt-1", exp))+`}`,
		f.cookieA, "")
	if code != http.StatusOK {
		t.Fatalf("导入应 200，得 %d %s", code, raw)
	}
	d := eaJSON(t, raw)
	if d["imported"].(float64) != 1 || d["updated"].(float64) != 0 {
		t.Fatalf("应 imported=1 updated=0，得 %v", d)
	}
	acc, _ := f.store.GetAccount("imp-1")
	if acc == nil || acc.OwnerUserID != f.userA.ID {
		t.Fatalf("新号应归属 A，得 %+v", acc)
	}
	if acc.PoolState != "" {
		t.Fatalf("新号默认应为未申请入池，得 %q", acc.PoolState)
	}

	// 2) 同一个号再导一次 = 重新登录：更新凭证 + 清掉失效标记 + **不动业务状态**。
	if err := f.store.SetAccountEgress("imp-1", "http://127.0.0.1:12070"); err != nil {
		t.Fatalf("SetAccountEgress: %v", err)
	}
	if err := f.store.MarkTokenDead("imp-1"); err != nil {
		t.Fatalf("MarkTokenDead: %v", err)
	}
	dead := f.store.TokenDeadMap()
	if _, ok := dead["imp-1"]; !ok {
		t.Fatal("前置：应已标记为登录失效")
	}
	code, raw = eaReq(t, f.mux, "POST", "/api/host/accounts/import",
		`{"json":`+jsonQuote(hostImportPayload("imp-1", "改了昵称", "at-2", "rt-2", exp))+`}`,
		f.cookieA, "")
	if code != http.StatusOK {
		t.Fatalf("重复导入应 200，得 %d %s", code, raw)
	}
	d = eaJSON(t, raw)
	if d["imported"].(float64) != 0 || d["updated"].(float64) != 1 {
		t.Fatalf("重复导入应 imported=0 updated=1，得 %v", d)
	}
	acc, _ = f.store.GetAccount("imp-1")
	if acc.AccessToken != "at-2" || acc.Nickname != "改了昵称" {
		t.Fatalf("凭证/昵称未更新，得 %+v", acc)
	}
	if acc.EgressProxy != "http://127.0.0.1:12070" {
		t.Fatalf("重新导入不该改掉出口配置，得 %q", acc.EgressProxy)
	}
	if acc.OwnerUserID != f.userA.ID {
		t.Fatalf("归属不该变，得 %d", acc.OwnerUserID)
	}
	if _, still := f.store.TokenDeadMap()["imp-1"]; still {
		t.Fatal("重新登录后必须清掉 token_dead_at，否则卡片一直红着（用户会以为没生效）")
	}
}

// TestHostImportRejectsForeignUID 导入别人的号：跳过并说明原因，绝不动原号。
func TestHostImportRejectsForeignUID(t *testing.T) {
	f := usApp(t)
	exp := time.Now().Add(48*time.Hour).Unix() * 1000

	// us-b-acct 属于 B；A 拿一份它的"auth 文件"来导。
	before, _ := f.store.GetAccount("us-b-acct")
	code, raw := eaReq(t, f.mux, "POST", "/api/host/accounts/import",
		`{"json":`+jsonQuote(hostImportPayload("us-b-acct", "B 的号", "at-hijack", "rt-hijack", exp))+`}`,
		f.cookieA, "")
	if code != http.StatusOK {
		// 允许 200 + skipped（不算请求错误：这是一份"部分成功"的导入结果）。
		t.Fatalf("应 200 并逐条给出跳过原因，得 %d %s", code, raw)
	}
	d := eaJSON(t, raw)
	if d["imported"].(float64) != 0 || d["updated"].(float64) != 0 {
		t.Fatalf("别人的号不该被导入/更新，得 %v", d)
	}
	if d["skipped"].(float64) != 1 {
		t.Fatalf("应记 1 条跳过，得 %v", d)
	}
	skips := d["skips"].([]any)
	reason, _ := skips[0].(map[string]any)["reason"].(string)
	if reason == "" {
		t.Fatal("跳过必须带原因（否则用户不知道该怎么办）")
	}
	after, _ := f.store.GetAccount("us-b-acct")
	if after.AccessToken != before.AccessToken {
		t.Fatalf("别人的号被改写了令牌：%q → %q（这是串号）", before.AccessToken, after.AccessToken)
	}
	if after.OwnerUserID != f.userB.ID {
		t.Fatalf("归属被改动：%d", after.OwnerUserID)
	}

	// 平台号（owner=0）同理：普通用户不能把它导到自己名下。
	plat := &Account{UID: "plat-1", Nickname: "平台号", Realm: "cn",
		AccessToken: "p-at", RefreshToken: "p-rt",
		ExpiresAt: time.Now().Add(time.Hour).Unix(), PoolState: "in"}
	if err := f.store.UpsertAccount(plat); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	code, raw = eaReq(t, f.mux, "POST", "/api/host/accounts/import",
		`{"json":`+jsonQuote(hostImportPayload("plat-1", "平台号", "x", "y", exp))+`}`,
		f.cookieA, "")
	if code != http.StatusOK {
		t.Fatalf("应 200，得 %d %s", code, raw)
	}
	if d := eaJSON(t, raw); d["skipped"].(float64) != 1 {
		t.Fatalf("平台号也必须被跳过，得 %v", d)
	}
	if a, _ := f.store.GetAccount("plat-1"); a.OwnerUserID != 0 {
		t.Fatalf("平台号归属被改成 %d", a.OwnerUserID)
	}
}

// jsonQuote 把一段 JSON 文本包成 JSON 字符串字面量（用于拼请求体）。
func jsonQuote(s string) string {
	out := make([]byte, 0, len(s)+8)
	out = append(out, '"')
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			out = append(out, '\\', '"')
		case '\\':
			out = append(out, '\\', '\\')
		case '\n':
			out = append(out, '\\', 'n')
		case '\r':
			out = append(out, '\\', 'r')
		case '\t':
			out = append(out, '\\', 't')
		default:
			out = append(out, s[i])
		}
	}
	out = append(out, '"')
	return string(out)
}
