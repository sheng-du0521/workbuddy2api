// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// 用量报表域的**契约层**测试（P2-7 拆包第四刀第二刀，v0.31.13）。
//
// 本文件原为 ranking_test.go。该域的实现在 v0.31.13 迁到了 internal/api/report，
// 于是这里做了一件比"改个包名"更重要的事：**把断言从"调 app 内部函数"改成
// "走真实 mux 打 HTTP"**。
//
// 为什么升级而不是加一层转发函数保住旧写法：
//   · 本仓的「零行为变化」最强证据形态就是**契约层测试** —— 走真 mux 等于同时
//     证明路由、鉴权中间件、状态码、JSON 键名/键值逐字节未变；
//   · 调 `a.usageRanking(...)` 只能证明"这个函数还在"，证明不了接口没变；
//   · 转发函数本身是负债：它把重复的入口留在原地，还多一层跳转（本仓的接线
//     手段优先级：嵌入+方法提升 > 直接改调用点 > 类型别名 > 函数别名）。
//
// 代价（如实记录）：本文件不能再声称"一行未改"。作为补偿，新增了此前**完全没有
// Go 测试**的账单域契约用例（TestUsageBreakdownHTTP*），并用 JSON 往返断言
// 键名契约（比直接调内部函数更接近真实缺陷面 —— v0.29.0 的提现按钮不渲染
// 就是键名契约破损，而当时的单测是直接调内部函数的，没抓到）。

// seedUsage 写一条窗口内的用量流水。
func seedUsage(t *testing.T, store *Store, userID int64, chargedMicro int64, reqs int) {
	t.Helper()
	if err := store.InsertUsageLog(&UsageLog{
		UserID: userID, KeyID: 1, Model: "m", PromptTokens: 10, CompletionTokens: 10,
		ChargedMicro: chargedMicro, UpstreamUID: "u", Scope: ScopePool, Status: "ok",
		CreatedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("InsertUsageLog: %v", err)
	}
	_ = reqs
}

// sessionFor 造一个真会话，返回可放进 wb_session cookie 的 token。
func sessionFor(t *testing.T, store *Store, u *User) string {
	t.Helper()
	tok := randomHex(32)
	if err := store.CreateSession(HashToken(tok), u.ID, sessionTTL); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return tok
}

// reportGET 打一次真 mux 请求并解出 JSON；非 200 即失败。
//
// ⚠️ 断言 JSON 而不是 Go map：键名契约（snake_case）只有经过编码一趟才能被验证，
// 而"对外视图内嵌无 json tag 的结构体"正是本仓出过的真缺陷（v0.29.0）。
func reportGET(t *testing.T, mux *http.ServeMux, path, cookie, adminKey string) map[string]any {
	t.Helper()
	body, code := reportRaw(t, mux, path, cookie, adminKey)
	if code != http.StatusOK {
		t.Fatalf("GET %s 状态码 = %d，body=%.200s", path, code, body)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("GET %s 响应不是 JSON：%v body=%.200s", path, err, body)
	}
	return out
}

// reportRaw 打一次真 mux 请求，返回原始 body 与状态码（用于断言非 200 的场景）。
func reportRaw(t *testing.T, mux *http.ServeMux, path, cookie, adminKey string) (string, int) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	}
	if adminKey != "" {
		req.Header.Set("X-Admin-Key", adminKey)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Body.String(), rec.Code
}

// listOf 取 JSON 里的 list 数组（经 JSON 往返后是 []any，逐项再断言是对象）。
func listOf(t *testing.T, view map[string]any) []map[string]any {
	t.Helper()
	raw, ok := view["list"].([]any)
	if !ok {
		t.Fatalf("list 应为数组，得 %T（%v）", view["list"], view["list"])
	}
	out := make([]map[string]any, 0, len(raw))
	for i, it := range raw {
		m, ok := it.(map[string]any)
		if !ok {
			t.Fatalf("list[%d] 应为对象，得 %T", i, it)
		}
		out = append(out, m)
	}
	return out
}

// ----------------------------------------------------------------------
// store 层：排序与排除口径（不经 HTTP，钉的是聚合 SQL 的"哪些行算进来"）
// ----------------------------------------------------------------------

// TestUsageByUserOrdering 排序依据：消耗降序 → 请求数降序 → user_id 升序。
func TestUsageByUserOrdering(t *testing.T) {
	store, px, _ := newTestApp(t)
	defer store.Close()
	defer px.Close()

	alice, _ := store.CreateUser("alice", "h")
	bob, _ := store.CreateUser("bob", "h")
	carol, _ := store.CreateUser("carol", "h")

	seedUsage(t, store, alice.ID, 1000, 1)
	seedUsage(t, store, bob.ID, 5000, 1)
	seedUsage(t, store, carol.ID, 1000, 1) // 与 alice 同分 → 按 user_id 升序，alice 在前

	rows, err := store.UsageByUser(0, 14, 0)
	if err != nil {
		t.Fatalf("UsageByUser: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("应有 3 个用户，得 %d", len(rows))
	}
	want := []int64{bob.ID, alice.ID, carol.ID}
	for i, w := range want {
		if rows[i].UserID != w {
			t.Fatalf("第 %d 名应为 user=%d，得 %d（完整顺序 %+v）", i+1, w, rows[i].UserID, rows)
		}
	}
	if rows[0].Username != "bob" || rows[0].ChargedMicro != 5000 {
		t.Fatalf("明细不符：%+v", rows[0])
	}
}

// TestUsageByUserExcludesAdminChannel 管理密钥通道（user_id=0）不进榜。
//
// v0.17.6 起管理通道也写台账（费用恒 0），若不排除就会凭空多出一行「0 号用户」，
// 并把 total_users 抬高一格。
func TestUsageByUserExcludesAdminChannel(t *testing.T) {
	store, px, a := newTestApp(t)
	defer store.Close()
	defer px.Close()

	alice, _ := store.CreateUser("alice", "h")
	seedUsage(t, store, alice.ID, 1234, 1)
	seedUsage(t, store, 0, 0, 1) // 管理通道

	rows, err := store.UsageByUser(0, 14, 0)
	if err != nil {
		t.Fatalf("UsageByUser: %v", err)
	}
	if len(rows) != 1 || rows[0].UserID != alice.ID {
		t.Fatalf("应只有 alice 一个用户，得 %+v", rows)
	}
	total, err := store.CountUsageUsers(14)
	if err != nil {
		t.Fatalf("CountUsageUsers: %v", err)
	}
	if total != 1 {
		t.Fatalf("分母应为 1（不含 user_id=0），得 %d", total)
	}

	// 管理端视角（契约层）：完整用户名 + user_id。
	admin := reportGET(t, a.routes(), "/api/admin/usage/ranking?days=14", "", a.key)
	list := listOf(t, admin)
	if len(list) != 1 {
		t.Fatalf("管理端榜单应 1 条，得 %d", len(list))
	}
	row := list[0]
	if row["name"] != "alice" || row["user_id"] != float64(alice.ID) {
		t.Fatalf("管理端应给完整用户名与 id，得 %+v", row)
	}
	if _, hasMe := admin["me"]; hasMe {
		t.Fatalf("管理端视角不应有 me 块")
	}
	if _, hasID := row["user_id"]; !hasID {
		t.Fatalf("管理端视图应带 user_id")
	}
}

// ----------------------------------------------------------------------
// 契约层：排行两种视角
// ----------------------------------------------------------------------

// TestUsageRankingSelfMasked 用户视角：他人用户名脱敏、自己原样、附自己的名次。
func TestUsageRankingSelfMasked(t *testing.T) {
	store, px, a := newTestApp(t)
	defer store.Close()
	defer px.Close()

	alice, _ := store.CreateUser("alice", "h")
	bob, _ := store.CreateUser("bobbobbob", "h")
	seedUsage(t, store, alice.ID, 900, 1)
	seedUsage(t, store, bob.ID, 100, 1)

	tok := sessionFor(t, store, bob)
	view := reportGET(t, a.routes(), "/api/usage/ranking?days=14", tok, "")
	if view["masked"] != true {
		t.Fatalf("用户视角应标记 masked")
	}
	list := listOf(t, view)
	if len(list) != 2 {
		t.Fatalf("应 2 条，得 %d", len(list))
	}
	// 第一名是 alice（900），对 bob 而言应脱敏 —— 掩码形态固定为 首字+3星+末字。
	first := list[0]
	if first["name"] == "alice" {
		t.Fatalf("他人用户名不应原样出现，得 %v", first["name"])
	}
	if first["name"] != "a***e" {
		t.Fatalf("他人用户名应为脱敏形态 a***e，得 %v", first["name"])
	}
	if _, hasID := first["user_id"]; hasID {
		t.Fatalf("用户视角不应暴露他人 user_id")
	}
	// 自己那一行原样且 is_me。
	me := list[1]
	if me["name"] != "bobbobbob" || me["is_me"] != true {
		t.Fatalf("自己那一行应原样且标记 is_me，得 %+v", me)
	}
	meBlock, ok := view["me"].(map[string]any)
	if !ok {
		t.Fatalf("用户视角应有 me 块，得 %T", view["me"])
	}
	if meBlock["rank"] != float64(2) || meBlock["in_top"] != true {
		t.Fatalf("me 块名次应为 2 且 in_top，得 %+v", meBlock)
	}
	if meBlock["total_users"] != float64(2) {
		t.Fatalf("分母应为 2，得 %v", meBlock["total_users"])
	}
	// 未登录不得看榜（越权面：排行榜会暴露他人用量）。
	if body, code := reportRaw(t, a.routes(), "/api/usage/ranking?days=14", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("匿名访问排行应 401，got %d body=%.120s", code, body)
	}
}

// TestUsageRankingMeOutsideTop 未进前 N 名时仍要给出自己的名次（否则用户只看到「榜上没我」）。
func TestUsageRankingMeOutsideTop(t *testing.T) {
	store, px, a := newTestApp(t)
	defer store.Close()
	defer px.Close()

	// 12 个用户各消耗递增：把「我」放最后，榜只取前 10（rankingUserLimit 的契约值）。
	var me *User
	for i := 0; i < 12; i++ {
		u, err := store.CreateUser("rankuser"+strconv.Itoa(i), "h")
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		seedUsage(t, store, u.ID, int64(1000-i*10), 1)
		me = u // 最后一个 = 消耗最低 = 第 12 名
	}
	tok := sessionFor(t, store, me)
	view := reportGET(t, a.routes(), "/api/usage/ranking?days=14", tok, "")
	list := listOf(t, view)
	if len(list) != 10 {
		t.Fatalf("用户侧榜应限制为 10 条，得 %d", len(list))
	}
	meBlock, ok := view["me"].(map[string]any)
	if !ok {
		t.Fatalf("应有 me 块，得 %+v", view)
	}
	if meBlock["rank"] != float64(12) {
		t.Fatalf("me 名次应为 12，得 %v", meBlock["rank"])
	}
	if meBlock["in_top"] != false {
		t.Fatalf("未进前 10 时 in_top 应为 false，得 %v", meBlock["in_top"])
	}
	for _, row := range list {
		if row["is_me"] == true {
			t.Fatalf("未进前 N 时榜单里不该有自己的行")
		}
	}
}

// TestUsageRankingEmpty 空数据不崩、分母为 0、me 有名次 0，且数组不得为 null。
func TestUsageRankingEmpty(t *testing.T) {
	store, px, a := newTestApp(t)
	defer store.Close()
	defer px.Close()
	u, _ := store.CreateUser("alice", "h")

	tok := sessionFor(t, store, u)
	view := reportGET(t, a.routes(), "/api/usage/ranking?days=14", tok, "")
	if list := listOf(t, view); len(list) != 0 {
		t.Fatalf("无数据时榜单应为空，得 %d", len(list))
	}
	if view["total_users"] != float64(0) {
		t.Fatalf("分母应为 0，得 %v", view["total_users"])
	}
	meBlock := view["me"].(map[string]any)
	if meBlock["rank"] != float64(0) || meBlock["in_top"] != false {
		t.Fatalf("无数据时名次应为 0，得 %+v", meBlock)
	}
	admin := reportGET(t, a.routes(), "/api/admin/usage/ranking?days=14", "", a.key)
	if admin["list"] == nil {
		t.Fatalf("管理端空榜也应返回空数组而非 null")
	}
}

// TestUsageRankingLimitClamped 管理端 limit 的钳制口径：非法/越界都不得越权放大扫表量。
func TestUsageRankingLimitClamped(t *testing.T) {
	store, px, a := newTestApp(t)
	defer store.Close()
	defer px.Close()
	u, _ := store.CreateUser("alice", "h")
	seedUsage(t, store, u.ID, 100, 1)

	mux := a.routes()
	for raw, want := range map[string]float64{
		"":    20, // 缺省
		"abc": 20, // 非法 → 缺省
		"0":   20, // 非正 → 缺省
		"7":   7,
		"999": 50, // 超上限 → 钳到 50
	} {
		view := reportGET(t, mux, "/api/admin/usage/ranking?days=14&limit="+raw, "", a.key)
		if view["limit"] != want {
			t.Errorf("limit=%q 应回 %v，得 %v", raw, want, view["limit"])
		}
	}
	// 用户侧不看 limit 参数：恒为自身上限（否则用户能拉一份长榜）。
	view := reportGET(t, mux, "/api/usage/ranking?days=14&limit=999", sessionFor(t, store, u), "")
	if view["limit"] != float64(10) {
		t.Errorf("用户侧 limit 应恒为 10，得 %v", view["limit"])
	}
}

// ----------------------------------------------------------------------
// 契约层：用量账单三类拆分（本域此前在 Go 侧完全没有测试）
// ----------------------------------------------------------------------

// TestUsageBreakdownHTTP 三类拆分的键名与口径契约。
func TestUsageBreakdownHTTP(t *testing.T) {
	store, px, a := newTestApp(t)
	defer store.Close()
	defer px.Close()
	u, _ := store.CreateUser("alice", "h")
	seedUsage(t, store, u.ID, 5000, 1)

	tok := sessionFor(t, store, u)
	// days 钳制：缺省 14、上限 90、非法回落（与排行的 90 上限同口径）。
	for raw, want := range map[string]float64{"": 14, "abc": 14, "0": 14, "7": 7, "999": 90} {
		view := reportGET(t, a.routes(), "/api/usage/breakdown?days="+raw, tok, "")
		if view["days"] != want {
			t.Errorf("breakdown days=%q 应回 %v，得 %v", raw, want, view["days"])
		}
	}

	view := reportGET(t, a.routes(), "/api/usage/breakdown?days=7", tok, "")
	for _, k := range []string{"days", "self_use", "pool_use", "pool_earn"} {
		if _, ok := view[k]; !ok {
			t.Fatalf("响应缺键 %q：%v", k, view)
		}
	}
	// 消费侧两类的键名契约（前端逐字段读，缺一个就是"界面少一块"）。
	for _, k := range []string{"self_use", "pool_use"} {
		blk, ok := view[k].(map[string]any)
		if !ok {
			t.Fatalf("%s 应为对象，得 %T", k, view[k])
		}
		for _, f := range []string{"requests", "tokens", "charged", "upstream_credit", "daily", "models"} {
			if _, ok := blk[f]; !ok {
				t.Fatalf("%s 缺字段 %q：%v", k, f, blk)
			}
		}
	}
	// 收益侧另有一套键（有 earned/total/count，没有 tokens）。
	blk := view["pool_earn"].(map[string]any)
	for _, f := range []string{"requests", "charged", "earned", "total", "count", "daily", "models"} {
		if _, ok := blk[f]; !ok {
			t.Fatalf("pool_earn 缺字段 %q：%v", f, blk)
		}
	}
	// 未登录不得读账单。
	if body, code := reportRaw(t, a.routes(), "/api/usage/breakdown", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("匿名访问账单应 401，got %d body=%.120s", code, body)
	}
}
