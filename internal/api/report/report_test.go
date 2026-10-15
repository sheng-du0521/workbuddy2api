// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package report

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/httpx"
	"workbuddy2api/internal/usage"
)

// 本文件是 internal/api/report 的**独立**测试（P2-7 第四刀第二刀，v0.31.13）。
//
// 为什么值得单独写：搬迁前这些判定只能通过"拉起整个 app + 真 SQLite + HTTP mux"
// 间接跑到，于是**边界几乎没人钉** —— 而它们全是"错了不崩、只会给出错误答案"
// 的那一类：
//   · 用户侧脱敏范围（漏一个分支 = 把可枚举的账号名录给了任意登录用户）；
//   · 榜外名次（不给 = 用户看到"榜上没我"却不知道自己在第几）；
//   · limit 是否真的**下传到 SQL**（丢在中间 = 一次刷新把全站用户拉回来）；
//   · 三类拆分的 scope 与键名对应（写反 = 把"自用"和"用号池"对调）；
//   · 收益块 total/count 是**全时段**、daily/models 是**窗口**（混用即对不上）。
//
// 用假 store 而不是临时 SQLite：本包要钉的是**视图组装**（分类、脱敏、钳制、
// 求和、口径选择），不是 SQL。真库语义由 internal/usage 自己的测试钉。

// fakeStore 本域的假 store：预置返回值 + 记录调用轨迹 + 可注入失败。
type fakeStore struct {
	daily  map[string][]usage.UsageDailyStat // key = scope
	models map[string][]usage.UsageModelStat // key = scope

	earnDaily []usage.HostEarnDailyStat
	earnModel []usage.HostEarnModelStat
	earnTotal int64
	earnCount int64

	users      []usage.UsageUserStat // 按"已排好序"给出（排序属 SQL 的职责）
	totalUsers int

	fail  error    // 非 nil 时所有查询都失败（钉错误路径）
	calls []string // 调用轨迹，形如 "daily:host"
}

func (f *fakeStore) note(s string) { f.calls = append(f.calls, s) }

func (f *fakeStore) UsageDailyScoped(_ int64, _ int, scope string) ([]usage.UsageDailyStat, error) {
	f.note("daily:" + scope)
	if f.fail != nil {
		return nil, f.fail
	}
	return f.daily[scope], nil
}

func (f *fakeStore) UsageByModelScoped(_ int64, _ int, scope string) ([]usage.UsageModelStat, error) {
	f.note("models:" + scope)
	if f.fail != nil {
		return nil, f.fail
	}
	return f.models[scope], nil
}

func (f *fakeStore) HostEarningsDaily(_ int64, _ int) ([]usage.HostEarnDailyStat, error) {
	f.note("earn-daily")
	if f.fail != nil {
		return nil, f.fail
	}
	return f.earnDaily, nil
}

func (f *fakeStore) HostEarningsByModel(_ int64, _ int) ([]usage.HostEarnModelStat, error) {
	f.note("earn-models")
	if f.fail != nil {
		return nil, f.fail
	}
	return f.earnModel, nil
}

func (f *fakeStore) HostEarningSummary(_ int64) (int64, int64, error) {
	f.note("earn-summary")
	if f.fail != nil {
		return 0, 0, f.fail
	}
	return f.earnTotal, f.earnCount, nil
}

// UsageByUser 实现真正的 limit 语义 —— **这一点很关键**：接口层若不把 limit
// 下传（比如忘了传或传 0），本假实现就会把全部用户返回，测试当场变红。
func (f *fakeStore) UsageByUser(_ int64, _ int, limit int) ([]usage.UsageUserStat, error) {
	f.note(fmt.Sprintf("users:limit=%d", limit))
	if f.fail != nil {
		return nil, f.fail
	}
	if limit > 0 && limit < len(f.users) {
		return f.users[:limit], nil
	}
	return f.users, nil
}

func (f *fakeStore) CountUsageUsers(_ int) (int, error) {
	f.note("count-users")
	if f.fail != nil {
		return 0, f.fail
	}
	return f.totalUsers, nil
}

// usersN 造 n 个用户，消耗严格递减（第 i 名 charged = (n-i)*100）。
func usersN(n int) []usage.UsageUserStat {
	out := make([]usage.UsageUserStat, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, usage.UsageUserStat{
			UserID:       int64(i + 1),
			Username:     fmt.Sprintf("u%d", i+1),
			Requests:     int64(n - i),
			Tokens:       int64((n - i) * 10),
			ChargedMicro: int64((n - i) * 100),
		})
	}
	return out
}

// svc 造一个服务，身份固定为 uid（uid<=0 表示匿名）。
func svc(f *fakeStore, uid int64) Service {
	return New(f, func(*http.Request) (int64, bool) {
		if uid <= 0 {
			return 0, false
		}
		return uid, true
	})
}

// ----------------------------------------------------------------------
// 纯辅助函数（随代码一起从 cmd/webapp 迁来）
// ----------------------------------------------------------------------

// TestMaskUsername 用户名脱敏形态。
func TestMaskUsername(t *testing.T) {
	cases := map[string]string{
		"":          "匿名用户",
		"a":         "a**",
		"ab":        "a*",
		"alice":     "a***e",
		"bobbobbob": "b***b",
	}
	for in, want := range cases {
		if got := MaskUsername(in); got != want {
			t.Errorf("MaskUsername(%q) = %q, want %q", in, got, want)
		}
	}
	// 长度不可推断：不同长度输入产生等长的掩码（除首尾）。
	if len(MaskUsername("alice")) != len(MaskUsername("verylongusername")) {
		t.Fatalf("掩码长度应不随原文长度变化")
	}
}

// TestClampDays days 参数收敛（缺省 14、上限 90、非法回落）。
func TestClampDays(t *testing.T) {
	cases := []struct {
		raw  string
		def  int
		max  int
		want int
	}{
		{"", 14, 90, 14},
		{"abc", 14, 90, 14},
		{"0", 14, 90, 14},
		{"-3", 14, 90, 14},
		{"7", 14, 90, 7},
		{"90", 14, 90, 90},
		{"9999", 14, 90, 90},
	}
	for _, c := range cases {
		if got := ClampDays(c.raw, c.def, c.max); got != c.want {
			t.Errorf("ClampDays(%q) = %d, want %d", c.raw, got, c.want)
		}
	}
}

// TestBreakdownDaysParsing 账单 days 的缺省/上限/非法回落。
func TestBreakdownDaysParsing(t *testing.T) {
	for raw, want := range map[string]int{"": 14, "abc": 14, "0": 14, "-5": 14, "1": 1, "90": 90, "91": 90, "9999": 90} {
		r := httptest.NewRequest(http.MethodGet, "/api/usage/breakdown?days="+raw, nil)
		if got := BreakdownDays(r); got != want {
			t.Errorf("BreakdownDays(days=%q) = %d, want %d", raw, got, want)
		}
	}
}

// ----------------------------------------------------------------------
// 排行：两种视角
// ----------------------------------------------------------------------

// TestRankingAdminViewKeepsNamesAndIDs 管理端视角：完整用户名 + user_id、无 me 块。
func TestRankingAdminViewKeepsNamesAndIDs(t *testing.T) {
	f := &fakeStore{users: usersN(2), totalUsers: 2}
	view := svc(f, 0).Ranking(0, 14, 0)

	list, ok := view["list"].([]map[string]any)
	if !ok || len(list) != 2 {
		t.Fatalf("应 2 条，得 %v", view["list"])
	}
	if list[0]["name"] != "u1" || list[0]["user_id"] != int64(1) {
		t.Fatalf("管理端应给完整用户名与 id，得 %+v", list[0])
	}
	if list[0]["rank"] != 1 || list[1]["rank"] != 2 {
		t.Fatalf("名次应按顺序，得 %+v", list)
	}
	// 消耗微点 → 点数的换算必须发生（500000 微点 = 0.5 点）。
	if list[0]["charged"] != 0.2 {
		t.Fatalf("charged 应为点数 0.2，得 %v", list[0]["charged"])
	}
	if _, has := view["me"]; has {
		t.Fatalf("管理端不应有 me 块")
	}
	if _, has := view["masked"]; has {
		t.Fatalf("管理端不应有 masked 标记")
	}
	if view["order_by"] != "charged" {
		t.Fatalf("排序口径应显式下发（前端据此显示「按消耗」），得 %v", view["order_by"])
	}
}

// TestRankingUserViewMasksOthersAndFlagsMe 用户视角：他人脱敏且不给 id，自己原样且带 me 块。
func TestRankingUserViewMasksOthersAndFlagsMe(t *testing.T) {
	f := &fakeStore{users: usersN(3), totalUsers: 3}
	view := svc(f, 3).Ranking(3, 14, 0) // 「我」= 第 3 名（uid=3）

	if view["masked"] != true {
		t.Fatalf("用户视角应标记 masked")
	}
	list := view["list"].([]map[string]any)
	if len(list) != 3 {
		t.Fatalf("应 3 条，得 %d", len(list))
	}
	for i := 0; i < 2; i++ {
		if list[i]["name"] == fmt.Sprintf("u%d", i+1) {
			t.Fatalf("他人用户名不得原样出现：%+v", list[i])
		}
		if _, has := list[i]["user_id"]; has {
			t.Fatalf("用户视角不得暴露他人 user_id：%+v", list[i])
		}
	}
	me := list[2]
	if me["name"] != "u3" || me["is_me"] != true {
		t.Fatalf("自己那一行应原样且 is_me，得 %+v", me)
	}
	meBlock, ok := view["me"].(map[string]any)
	if !ok {
		t.Fatalf("用户视角应有 me 块，得 %T", view["me"])
	}
	if meBlock["rank"] != 3 || meBlock["in_top"] != true || meBlock["total_users"] != 3 {
		t.Fatalf("me 块不符：%+v", meBlock)
	}
}

// TestRankingMeOutsideTopUsesStoreFullList 榜外的「我」必须靠**再查一次全量**
// 才能算出名次（且这次查询不能带 limit —— 带了就永远算不出榜外名次）。
func TestRankingMeOutsideTopUsesStoreFullList(t *testing.T) {
	f := &fakeStore{users: usersN(12), totalUsers: 12}
	view := svc(f, 12).Ranking(12, 14, 0) // 我 = 第 12 名，榜只取 10 条

	list := view["list"].([]map[string]any)
	if len(list) != RankingUserLimit {
		t.Fatalf("用户侧榜应 %d 条，得 %d", RankingUserLimit, len(list))
	}
	for _, row := range list {
		if row["is_me"] == true {
			t.Fatalf("未进前 N 时榜内不该有自己：%+v", row)
		}
	}
	meBlock := view["me"].(map[string]any)
	if meBlock["rank"] != 12 || meBlock["in_top"] != false {
		t.Fatalf("榜外名次应为 12 且 in_top=false，得 %+v", meBlock)
	}
	// 第二次 UsageByUser 必须是无 limit 的（=0）那次调用，否则算不出全量名次。
	sawFull := false
	for _, c := range f.calls {
		if c == "users:limit=0" {
			sawFull = true
		}
	}
	if !sawFull {
		t.Fatalf("榜外名次必须走一次全量查询（limit=0），调用轨迹=%v", f.calls)
	}
}

// TestRankingLimitForwardedToStore limit 必须**下传到 SQL**，不能只在视图层截断。
func TestRankingLimitForwardedToStore(t *testing.T) {
	f := &fakeStore{users: usersN(30), totalUsers: 30}
	svc(f, 5).Ranking(5, 14, 0) // 用户侧缺省 10
	if got := lastUsersQuery(t, f.calls); got != "users:limit=10" {
		t.Fatalf("用户侧缺省 limit 应传 10，实际 %q（轨迹 %v）", got, f.calls)
	}
	f.calls = nil
	svc(f, 0).Ranking(0, 14, 0) // 管理端缺省 20
	if got := lastUsersQuery(t, f.calls); got != "users:limit=20" {
		t.Fatalf("管理端缺省 limit 应传 20，实际 %q（轨迹 %v）", got, f.calls)
	}
}

// lastUsersQuery 取调用轨迹里第一次 users 查询（limit 生效的那次）。
func lastUsersQuery(t *testing.T, calls []string) string {
	t.Helper()
	for _, c := range calls {
		if strings.HasPrefix(c, "users:") {
			return c
		}
	}
	t.Fatalf("没有观察到 UsageByUser 调用，轨迹=%v", calls)
	return ""
}

// TestRankingLimitClampedToMax 越界 limit 一律钳到 RankingLimitMax（防止一次刷新拉全站）。
func TestRankingLimitClampedToMax(t *testing.T) {
	f := &fakeStore{users: usersN(3), totalUsers: 3}
	view := svc(f, 0).Ranking(0, 14, 9999)
	if view["limit"] != RankingLimitMax {
		t.Fatalf("limit 应钳到 %d，得 %v", RankingLimitMax, view["limit"])
	}
	if got := lastUsersQuery(t, f.calls); got != fmt.Sprintf("users:limit=%d", RankingLimitMax) {
		t.Fatalf("钳制后的 limit 也必须下传，实际 %q", got)
	}
}

// TestRankingEmptyCollectionsAreArraysNotNil 空数据：数组不得为 null（前端会 .map 崩）。
func TestRankingEmptyCollectionsAreArraysNotNil(t *testing.T) {
	f := &fakeStore{}
	view := svc(f, 0).Ranking(0, 14, 0)
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(raw), `"list":null`) {
		t.Fatalf("空榜必须序列化成 []，不得是 null：%s", raw)
	}
	if view["total_users"] != 0 {
		t.Fatalf("分母应为 0，得 %v", view["total_users"])
	}
	// 用户视角下空榜也要有 me 块（名次 0）。
	uv := svc(f, 7).Ranking(7, 14, 0)
	me := uv["me"].(map[string]any)
	if me["rank"] != 0 || me["in_top"] != false || me["total_users"] != 0 {
		t.Fatalf("空数据 me 块应为 rank=0/in_top=false/total=0，得 %+v", me)
	}
}

// TestRankingStoreErrorSurfacesInResult store 报错时返回错误块（而不是空榜冒充"没人用"）。
func TestRankingStoreErrorSurfacesInResult(t *testing.T) {
	f := &fakeStore{fail: errors.New("sql: database is closed")}
	view := svc(f, 0).Ranking(0, 14, 0)
	if _, ok := view["error"]; !ok {
		t.Fatalf("store 出错应回 error 块，得 %+v", view)
	}
	// 错误块里**不该**有 list（否则前端会把空数组当成"确实没人用"）。
	if _, has := view["list"]; has {
		t.Fatalf("错误响应不应带 list，得 %+v", view)
	}
}

// ----------------------------------------------------------------------
// handler 行为
// ----------------------------------------------------------------------

// TestHandleUserRankingAnonymous401 匿名 → 401（越权面：排行榜会暴露他人用量）。
func TestHandleUserRankingAnonymous401(t *testing.T) {
	rec := httptest.NewRecorder()
	svc(&fakeStore{}, 0).HandleUserRanking(rec, httptest.NewRequest(http.MethodGet, "/api/usage/ranking", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("匿名应 401，得 %d", rec.Code)
	}
}

// TestHandleAdminRankingScrubsInternalError 内部错误文本必须被共享层脱敏后下发。
//
// 这条同时是**跨包纪律**的回归网：本包只管"把 err.Error() 放进 error 键"，
// 能不能给客户端看由 internal/httpx 决定 —— 所以断言要打在**响应字节**上。
func TestHandleAdminRankingScrubsInternalError(t *testing.T) {
	f := &fakeStore{fail: errors.New("sql: no such table: usage_logs")}
	rec := httptest.NewRecorder()
	svc(f, 0).HandleAdminRanking(rec, httptest.NewRequest(http.MethodGet, "/api/admin/usage/ranking", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码应保持 200（脱敏不改状态码），得 %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "no such table") || strings.Contains(body, "sql: ") {
		t.Fatalf("内部错误原文泄漏到响应：%s", body)
	}
	if !strings.Contains(body, httpx.ScrubTextSafe) {
		t.Fatalf("应回稳定脱敏文案，得 %s", body)
	}
}

// ----------------------------------------------------------------------
// 账单三类拆分
// ----------------------------------------------------------------------

// TestHandleUsageBreakdownScopeMapping **自用 / 用号池 的 scope 对应关系是契约**。
//
// 写反了不会报错：用户会看到"自用消耗"里其实是用号池的数据 —— 界面数字全对，
// 只是标签错了，人工复核极难发现。
func TestHandleUsageBreakdownScopeMapping(t *testing.T) {
	f := &fakeStore{
		daily: map[string][]usage.UsageDailyStat{
			"host": {{Day: "2026-10-15", Requests: 7, Tokens: 70, ChargedMicro: 0, UpstreamCredit: 1.5}},
			"pool": {{Day: "2026-10-15", Requests: 3, Tokens: 30, ChargedMicro: 2500, UpstreamCredit: 0.5}},
		},
		models: map[string][]usage.UsageModelStat{
			"host": {{Model: "m-a", Requests: 7, Tokens: 70}},
			"pool": {{Model: "m-b", Requests: 3, Tokens: 30, ChargedMicro: 2500}},
		},
		// 微点 → 点 = ÷ MicroPerPoint(=1000)：故 2500 微点 = 2.5 点。
		earnDaily: []usage.HostEarnDailyStat{{Day: "2026-10-15", Requests: 40, ChargedMicro: 100000, EarnedMicro: 10000}},
		earnTotal: 135800, earnCount: 903, // 全时段累计（与窗口值刻意不同）
	}
	rec := httptest.NewRecorder()
	svc(f, 1).HandleUsageBreakdown(rec, httptest.NewRequest(http.MethodGet, "/api/usage/breakdown?days=7", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", rec.Code, rec.Body.String())
	}
	var view map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("响应不是 JSON：%v", err)
	}
	if view["days"] != float64(7) {
		t.Fatalf("days 应回显 7，得 %v", view["days"])
	}

	self := view["self_use"].(map[string]any)
	pool := view["pool_use"].(map[string]any)
	// self_use 必须取到 host 那份（上游 credit 1.5、扣费 0），pool_use 取 pool 那份。
	if self["requests"] != float64(7) || self["upstream_credit"] != 1.5 || self["charged"] != float64(0) {
		t.Fatalf("self_use 应取 host scope（7 次 / credit 1.5 / 扣费 0），得 %+v", self)
	}
	if pool["requests"] != float64(3) || pool["charged"] != float64(2.5) || pool["upstream_credit"] != 0.5 {
		t.Fatalf("pool_use 应取 pool scope（3 次 / 扣费 2.5 点 / credit 0.5），得 %+v", pool)
	}
	// 关键字面必须都在（外层按"每处点数都带金额"给界面用）。
	for _, k := range []string{"self_use", "pool_use", "pool_earn"} {
		if _, ok := view[k]; !ok {
			t.Fatalf("缺块 %q", k)
		}
	}
	// scope 传参轨迹：self_use→host、pool_use→pool。
	joined := strings.Join(f.calls, ",")
	if !strings.Contains(joined, "daily:host") || !strings.Contains(joined, "daily:pool") {
		t.Fatalf("两类拆分应以 host / pool 两个 scope 各查一次，轨迹=%v", f.calls)
	}
}

// TestHostEarnBlockSummaryIsAllTimeWhileDailyIsWindowed 收益块的**双口径**。
//
// total/count 是全时段累计，daily/models 是窗口值 —— 二者不等是设计如此。
// 这条用例把两个数字刻意设成不同，谁若"顺手给 summary 加窗口"或
// "拿窗口求和冒充 total"，就会立刻变红。
func TestHostEarnBlockSummaryIsAllTimeWhileDailyIsWindowed(t *testing.T) {
	f := &fakeStore{
		earnDaily: []usage.HostEarnDailyStat{
			{Day: "2026-10-15", Requests: 40, ChargedMicro: 100000, EarnedMicro: 10000},
			{Day: "2026-10-16", Requests: 10, ChargedMicro: 20000, EarnedMicro: 2000},
		},
		earnModel: []usage.HostEarnModelStat{
			{Model: "m-a", Requests: 30, ChargedMicro: 90000, EarnedMicro: 9000},
			{Model: "m-b", Requests: 20, ChargedMicro: 30000, EarnedMicro: 3000},
		},
		earnTotal: 135800, // 全时段 135.8 点
		earnCount: 903,
	}
	rec := httptest.NewRecorder()
	svc(f, 1).HandleUsageBreakdown(rec, httptest.NewRequest(http.MethodGet, "/api/usage/breakdown", nil))
	var view map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("响应不是 JSON：%v", err)
	}
	blk := view["pool_earn"].(map[string]any)

	// 窗口值 = daily 求和（requests 40+10=50；earned 1000万+200万 微点 = 12 点）。
	if blk["requests"] != float64(50) {
		t.Fatalf("窗口请求数应为 daily 求和 50，得 %v", blk["requests"])
	}
	if blk["earned"] != 12.0 {
		t.Fatalf("窗口收益应为 daily 求和 12 点，得 %v", blk["earned"])
	}
	// 全时段值必须来自 summary（135.8 / 903 笔），**不是**窗口求和。
	if blk["total"] != 135.8 || blk["count"] != float64(903) {
		t.Fatalf("total/count 应取全时段汇总（135.8 / 903），得 %v / %v", blk["total"], blk["count"])
	}
	if blk["total"] == blk["earned"] {
		t.Fatal("两个口径被混用了：total 与 earned 不该相等（除非真相等，本例刻意不等）")
	}
	// models 是模型维度的窗口值，两块都要在。
	if list, ok := blk["models"].([]any); !ok || len(list) != 2 {
		t.Fatalf("models 应有 2 条，得 %v", blk["models"])
	}
	if list, ok := blk["daily"].([]any); !ok || len(list) != 2 {
		t.Fatalf("daily 应有 2 条，得 %v", blk["daily"])
	}
}
