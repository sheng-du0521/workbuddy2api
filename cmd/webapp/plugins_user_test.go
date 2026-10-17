// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 插件「用户级启停」（v0.31.22）的回归测试。
//
// 为什么单开一个文件：这一版把插件的启停拆成了**两层**（平台级 / 用户级），
// 而分层的价值全在边界上 —— 一旦哪条边界写反，症状都是"用户点了一下，
// 要么没反应、要么把别人也关了"。所以这里逐条钉住：
//
//  1. 用户停用**只影响自己**：别人的列表与执行不受影响；
//  2. 用户**动不了平台级开关**（它归管理员）；
//  3. 平台已停用时，用户点"启用"必须**明确报错**，而不是静默写一条
//     永远不生效的偏好（否则用户以为点了、下次仍用不了、还没有任何提示）；
//  4. 停用**不只是前端效果**：执行接口必须真的拒绝（前端只是不渲染入口）；
//  5. 对外形状契约：`user_enabled` / `active` 必须是 snake_case 键
//     （本项目禁内嵌无 json tag 的结构体 —— 那是"前端读到 Go 字段名"的根源）。
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// puApp 装配：真 mux + 两个普通用户，各自挂一个自己的号、各持一条会话。
//
// 两个用户是刻意的：只造一个用户的话，"停用是否只影响自己"这条**根本测不到** ——
// 而它恰好是这版最容易写错的地方（把用户偏好写进平台级 kv 就会全平台生效）。
func puApp(t *testing.T) (store *Store, px *Proxy, a *app, mux *http.ServeMux, tokA, tokB string) {
	t.Helper()
	store, px, a, mux = plApp(t)

	mk := func(name, uid string) string {
		u, err := store.CreateUser(name, "x")
		if err != nil {
			t.Fatalf("CreateUser(%s): %v", name, err)
		}
		if err := store.UpsertAccount(&Account{
			UID: uid, Nickname: name, Realm: "cn",
			AccessToken: "tok-" + uid, RefreshToken: "rt-" + uid,
			ExpiresAt:    time.Now().Add(48 * time.Hour).Unix(),
			ConsumeGroup: "apply", PoolState: "in", OwnerUserID: u.ID,
		}); err != nil {
			t.Fatalf("UpsertAccount(%s): %v", uid, err)
		}
		tok := randomHex(32)
		if err := store.CreateSession(HashToken(tok), u.ID, sessionTTL); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		return tok
	}
	return store, px, a, mux, mk("pu-a", "pu-acct-a"), mk("pu-b", "pu-acct-b")
}

// puReq 打一次**用户会话**请求（走真 mux，因而覆盖路由注册 + userGuard）。
func puReq(t *testing.T, mux *http.ServeMux, method, target, body, cookie string) (int, map[string]any) {
	t.Helper()
	code, raw := puReqRaw(t, mux, method, target, body, cookie)
	var out map[string]any
	_ = json.Unmarshal([]byte(raw), &out)
	return code, out
}

// puReqRaw 同上，但回原始响应体 —— 形状契约只能对着**编码后的字节**验。
func puReqRaw(t *testing.T, mux *http.ServeMux, method, target, body, cookie string) (int, string) {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	r.Header.Set("Content-Type", "application/json")
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	return rec.Code, rec.Body.String()
}

// plReqRaw 管理密钥通道打一次接口，回原始响应体（形状契约要对着字节验）。
func plReqRaw(t *testing.T, mux *http.ServeMux, method, target string) (int, string) {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	r.Header.Set("X-Admin-Key", "pl-admin-key")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	return rec.Code, rec.Body.String()
}

// puFirstPlugin 取用户端列表里的第一条插件。
func puFirstPlugin(t *testing.T, mux *http.ServeMux, cookie string) map[string]any {
	t.Helper()
	code, out := puReq(t, mux, "GET", "/api/plugins", "", cookie)
	if code != http.StatusOK {
		t.Fatalf("插件列表应 200，得 %d %v", code, out)
	}
	list, _ := out["plugins"].([]any)
	if len(list) == 0 {
		t.Fatalf("插件列表为空，得 %v", list)
	}
	// 断言"领取补偿插件在列表里"，不写死总数：插件会越加越多（v0.31.31 加了健康检测）。
	if one := findPlugin(list, "claim-compensation"); one != nil {
		return one
	}
	t.Fatalf("插件列表里应包含 claim-compensation，得 %v", list)
	return nil
}

// TestPluginUserToggleIsolated 用户停用只影响自己，且动不了平台开关。
func TestPluginUserToggleIsolated(t *testing.T) {
	_, px, _, mux, tokA, tokB := puApp(t)
	plStubUp(t, px, `{"code":0,"data":{"credit":3}}`, 0)

	// 缺省：平台启用 + 用户启用 → 生效
	one := puFirstPlugin(t, mux, tokA)
	if one["enabled"] != true || one["user_enabled"] != true || one["active"] != true {
		t.Fatalf("缺省应三态皆启用，得 %v", one)
	}

	// A 在自己的控制台停用
	if code, out := puReq(t, mux, "POST", "/api/plugins/claim-compensation/enabled",
		`{"enabled":false}`, tokA); code != http.StatusOK {
		t.Fatalf("用户停用应 200，得 %d %v", code, out)
	}

	// A 看到已停用
	one = puFirstPlugin(t, mux, tokA)
	if one["user_enabled"] != false || one["active"] != false {
		t.Fatalf("A 停用后 user_enabled/active 应为 false，得 %v", one)
	}
	if one["enabled"] != true {
		t.Fatalf("用户停用**不该**改动平台级 enabled，得 %v", one["enabled"])
	}

	// B 完全不受影响 —— 这条是整版的分水岭
	one = puFirstPlugin(t, mux, tokB)
	if one["user_enabled"] != true || one["active"] != true {
		t.Fatalf("用户停用不得影响他人，得 %v", one)
	}

	// 管理端列表的平台级开关也仍是启用
	_, aout := plReq(t, mux, "GET", "/api/admin/plugins", "")
	if got := aout["plugins"].([]any)[0].(map[string]any)["enabled"]; got != true {
		t.Fatalf("管理端 enabled 应仍为 true，得 %v", got)
	}

	// A 执行被拒（停用**不是**纯前端效果）
	code, out := puReq(t, mux, "POST", "/api/plugins/claim-compensation/claim",
		`{"uids":["pu-acct-a"]}`, tokA)
	if code != http.StatusForbidden {
		t.Fatalf("用户停用后执行应 403，得 %d %v", code, out)
	}

	// B 未停用 → 不该被这道闸门拦住（上游已桩，允许它走到执行）
	code, _ = puReq(t, mux, "POST", "/api/plugins/claim-compensation/claim",
		`{"uids":["pu-acct-b"]}`, tokB)
	if code == http.StatusForbidden {
		t.Fatal("B 没有停用该插件，不该被用户级闸门拦住")
	}
}

// TestPluginUserCannotOverridePlatformOff 平台停用时，用户点"启用"必须**明确报错**。
//
// 为什么不做成"静默接受"：用户点了启用、界面显示已启用，但平台层面禁用 ——
// 于是每次执行都被拒，而用户手里没有任何线索。宁可在这里报错说清。
func TestPluginUserCannotOverridePlatformOff(t *testing.T) {
	_, _, _, mux, tokA, _ := puApp(t)

	if code, _ := plReq(t, mux, "POST", "/api/admin/plugins/claim-compensation/enabled",
		`{"enabled":false}`); code != http.StatusOK {
		t.Fatal("管理员停用平台级开关应成功")
	}

	code, out := puReq(t, mux, "POST", "/api/plugins/claim-compensation/enabled",
		`{"enabled":true}`, tokA)
	if code != http.StatusForbidden {
		t.Fatalf("平台已停用时用户点启用应 403，得 %d %v", code, out)
	}

	// 反向是允许的：用户"停用"只是写自己的偏好，与平台状态无关（幂等无害）。
	if code, _ := puReq(t, mux, "POST", "/api/plugins/claim-compensation/enabled",
		`{"enabled":false}`, tokA); code != http.StatusOK {
		t.Fatalf("平台停用时用户仍可写自己的停用偏好，得 %d", code)
	}
	if code, _ := puReq(t, mux, "GET", "/api/plugins", "", tokA); code != http.StatusOK {
		t.Fatalf("平台停用时列表仍应 200（用户要能看见「平台已停用」），得 %d", code)
	}
}

// TestPluginUserToggleUnknownAndAnonymous 未注册 id / 未登录都要被挡住。
func TestPluginUserToggleUnknownAndAnonymous(t *testing.T) {
	_, _, _, mux, tokA, _ := puApp(t)

	if code, _ := puReq(t, mux, "POST", "/api/plugins/nope/enabled", `{"enabled":false}`, tokA); code != http.StatusNotFound {
		t.Fatalf("未知插件应 404，得 %d", code)
	}
	if code, _ := puReq(t, mux, "POST", "/api/plugins/claim-compensation/enabled", `{"enabled":false}`, ""); code != http.StatusUnauthorized {
		t.Fatalf("未登录应 401，得 %d", code)
	}
	// 缺字段（不是布尔）→ 400，别把 nil 当 false 写进去。
	if code, _ := puReq(t, mux, "POST", "/api/plugins/claim-compensation/enabled", `{}`, tokA); code != http.StatusBadRequest {
		t.Fatalf("缺 enabled 字段应 400，得 %d", code)
	}
}

// TestPluginViewShape 对外形状：必须是 snake_case，且不得出现 Go 字段名。
//
// 双向断言（本项目纪律）：Go 结构体一旦漏写 json tag，前端拿到的就是
// `UserEnabled` 这种键 —— 而前端读 `user_enabled` 得到 undefined，
// 表现是"开关永远是关的"，且没有任何报错。只有对着**编码后的字节**验才拦得住。
func TestPluginViewShape(t *testing.T) {
	_, _, _, mux, tokA, _ := puApp(t)

	code, raw := puReqRaw(t, mux, "GET", "/api/plugins", "", tokA)
	if code != http.StatusOK {
		t.Fatalf("列表应 200，得 %d", code)
	}
	for _, k := range []string{`"user_enabled"`, `"active"`, `"enabled"`, `"audience"`} {
		if !strings.Contains(raw, k) {
			t.Errorf("用户端插件列表必须含键 %s：%s", k, raw)
		}
	}
	for _, bad := range []string{`"UserEnabled"`, `"Active"`, `"Audience"`, `"Builtin"`} {
		if strings.Contains(raw, bad) {
			t.Errorf("用户端插件列表不该出现 Go 字段名键 %s（漏写 json tag）：%s", bad, raw)
		}
	}

	// 管理端同样校验一次：它多一个"没有用户上下文"的口径（user_enabled 恒 true）。
	code, rawA := plReqRaw(t, mux, "GET", "/api/admin/plugins")
	if code != http.StatusOK {
		t.Fatalf("管理端插件列表（管理密钥通道）应 200，得 %d", code)
	}
	if !strings.Contains(rawA, `"user_enabled"`) {
		t.Errorf("管理端插件列表也应含 user_enabled：%s", rawA)
	}
	if strings.Contains(rawA, `"UserEnabled"`) {
		t.Errorf("管理端插件列表不该出现 Go 字段名键：%s", rawA)
	}
}
