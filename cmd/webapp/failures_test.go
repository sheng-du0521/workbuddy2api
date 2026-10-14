// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/upstream"
)

// 本文件锁两件事：
//
//	① **声明即处置** —— failurePolicies() 里声明的每个码都必须真有处置分支，
//	   且处置方向与声明的作用域一致（这是把注册表从"会过期的文档"变成"机制"的关键）；
//	② 台账的结构化与保留行为（每次失败一条、脱敏、裁剪、按码/阶段/窗口过滤）。

// TestFailurePoliciesKeysUnique 注册表 Key 必须唯一 —— 否则 policyFor 会静默取到
// 不确定的那一条（map 构建时后写覆盖先写，且顺序不确定）。
func TestFailurePoliciesKeysUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range failurePolicies() {
		if p.Key == "" {
			t.Errorf("策略缺少 Key：%+v", p)
			continue
		}
		if seen[p.Key] {
			t.Errorf("Key 重复：%s", p.Key)
		}
		seen[p.Key] = true
	}
}

// TestFailurePoliciesFieldsComplete 每行都必须填齐运维需要的最小信息集。
//
// 刻意逐字段断言而不是只查 Key：这些字段正是界面直接展示的内容，
// 缺一个就会在页面上留下空白（而空白比错误更难被发现）。
func TestFailurePoliciesFieldsComplete(t *testing.T) {
	validScope := map[string]bool{
		ScopeRequest: true, ScopeModel: true, ScopeAccount: true, ScopeGlobal: true,
	}
	validStage := map[string]bool{
		FailStageReserve: true, FailStageUpstream: true, FailStageSettle: true, FailStageCharge: true,
	}
	for _, p := range failurePolicies() {
		if p.Name == "" {
			t.Errorf("%s 缺 Name", p.Key)
		}
		if p.Handling == "" {
			t.Errorf("%s 缺 Handling（界面靠它告诉运维该做什么）", p.Key)
		}
		if !validScope[p.Scope] {
			t.Errorf("%s 的 Scope=%q 非法", p.Key, p.Scope)
		}
		if !validStage[p.Stage] {
			t.Errorf("%s 的 Stage=%q 非法", p.Key, p.Stage)
		}
	}
}

// TestFailurePoliciesHaveDispatch 注册表里每个**上游业务码**都必须有真实的处置分支，
// 且处置方向与声明的 Scope 一致。
//
// 这是本机制的核心守卫：新增一个码时只加注册表而不加处置分支，这里会红。
// 断言口径（按 Scope 决定"什么算真的处置了"）：
//
//	request → 账号必须**毫发无伤**（不冷却 / 不熔断 / 不停用）
//	model   → 必须有该模型的独立冷却，且**账号级 until 保持零值**
//	account → 账号级冷却 或 停用（二者其一）
//	global  → 允许连败降权，但账号不得被停用（5xx 是上游故障不是账号问题）
func TestFailurePoliciesHaveDispatch(t *testing.T) {
	// 每个声明码的合成响应：status 取该码在真实链路上出现的状态码
	// （429/403 的限定条件写在各分支注释里，这里取最常见形态）。
	type probe struct {
		status int
		body   string
	}
	probes := map[string]probe{
		"11128":    {http.StatusBadRequest, `{"code":11128,"msg":"request illegal"}`},
		"11140":    {http.StatusForbidden, `{"code":11140,"msg":"request illegal"}`},
		"12153":    {http.StatusUnauthorized, `{"code":12153,"msg":"session invalid"}`},
		"14017":    {http.StatusBadRequest, `{"code":14017,"msg":"trial not activated"}`},
		"14020":    {http.StatusOK, `{"code":14020,"msg":"insufficient credits"}`},
		"6004":     {http.StatusTooManyRequests, `{"code":6004,"msg":"model limit"}`},
		"11102":    {http.StatusBadRequest, `{"code":11102,"msg":"service info not found"}`},
		"http:402": {http.StatusPaymentRequired, `{}`},
		"http:429": {http.StatusTooManyRequests, `{"msg":"too many requests"}`},
		"http:403": {http.StatusForbidden, `<html>Access Denied</html>`},
		"http:404": {http.StatusNotFound, `Not Found`},
		"http:5xx": {http.StatusBadGateway, `{"msg":"bad gateway"}`},
	}

	for _, pol := range failurePolicies() {
		if pol.Stage != FailStageUpstream {
			continue // 预扣/结算/扣费由本地路径驱动，见 TestConsumeFailureReserveStage
		}
		pr, ok := probes[pol.Key]
		if !ok {
			t.Fatalf("策略 %s 没有对应的测试探针 —— 新增码时必须同时补探针与处置断言", pol.Key)
		}
		t.Run(pol.Key, func(t *testing.T) {
			store, px, _ := newTestApp(t, &Account{UID: "u1", Realm: "cn", AccessToken: "a"})
			defer store.Close()
			_ = px.SyncAccounts()

			reset := time.Now().Add(30 * time.Minute)
			kind := px.reportUpstreamError("u1", "m", pr.status, []byte(pr.body),
				reset, upstream.ResetBodyText, nil)
			_ = kind

			before, _ := px.pool.Status("u1")
			acc, _ := store.GetAccount("u1")

			switch pol.Scope {
			case ScopeRequest:
				// 请求级：不能动账号。11128 属此列。
				if before.Cooling || before.Disabled || len(before.RateLimitedModels) > 0 {
					t.Errorf("请求级策略 %s 动了账号：%+v", pol.Key, before)
				}
			case ScopeModel:
				// 模型级：必须只挡该模型，账号级 until 保持零值。
				if !before.Until.IsZero() {
					t.Errorf("模型级策略 %s 污染了账号级 until=%v", pol.Key, before.Until)
				}
				found := false
				for _, m := range before.RateLimitedModels {
					if m.Model == "m" {
						found = true
					}
				}
				if !found {
					t.Errorf("模型级策略 %s 没有记录 (账号, 模型) 冷却：%+v", pol.Key, before)
				}
			case ScopeAccount:
				if !before.Cooling && !before.Disabled && (acc == nil || effDisabled(acc)) == false {
					t.Errorf("账号级策略 %s 没有产生任何账号级处置：%+v", pol.Key, before)
				}
			case ScopeGlobal:
				if before.Disabled {
					t.Errorf("全局策略 %s 不得停用账号（上游故障不是账号问题）：%+v", pol.Key, before)
				}
			}

			// 无论哪条分支：台账必须有一条对应记录（这正是"每次失败都记"的守卫）。
			rows, err := store.ListConsumeFailures(10, "", "", 0)
			if err != nil {
				t.Fatalf("ListConsumeFailures: %v", err)
			}
			if len(rows) != 1 {
				t.Fatalf("应恰好记 1 条失败，得 %d 条 %+v", len(rows), rows)
			}
			got := rows[0]
			if got.Stage != FailStageUpstream {
				t.Errorf("stage = %q, want %q", got.Stage, FailStageUpstream)
			}
			if got.CodeKey != pol.Key {
				t.Errorf("code_key = %q, want %q", got.CodeKey, pol.Key)
			}
			if got.Scope != pol.Scope {
				t.Errorf("scope = %q, want %q（台账必须带上注册表的处置范围）", got.Scope, pol.Scope)
			}
			if got.AutoRecover != pol.AutoRecover {
				t.Errorf("auto_recover = %v, want %v", got.AutoRecover, pol.AutoRecover)
			}
			if got.ResetSource != string(upstream.ResetBodyText) {
				t.Errorf("reset_source = %q, want body-text", got.ResetSource)
			}
			if got.Detail == "" {
				t.Error("detail 不应为空（排障要能看到上游说了什么）")
			}
		})
	}
}

// effDisabled 账号是否处于停用态（库列口径）。
func effDisabled(a *Account) bool { return a != nil && a.Disabled }

// TestConsumeFailureEveryUpstreamErrorRecorded 「每次」失败都记一条，不合并、不抽样。
func TestConsumeFailureEveryUpstreamErrorRecorded(t *testing.T) {
	store, px, _ := newTestApp(t, &Account{UID: "u1", Realm: "cn", AccessToken: "a"})
	defer store.Close()
	_ = px.SyncAccounts()

	for i := 0; i < 5; i++ {
		px.reportUpstreamError("u1", "m", http.StatusBadRequest,
			[]byte(`{"code":11101,"msg":"bad request"}`), time.Time{}, upstream.ResetNone, nil)
	}
	n := store.CountConsumeFailures()
	if n != 5 {
		t.Errorf("台账行数 = %d, want 5（每次失败一行）", n)
	}
}

// TestConsumeFailureUnknownCodeRecorded 未声明处置的码**照样记录** ——
// 那正是台账存在的意义（上游随时会冒出新码），只是它在界面上没有处置说明。
func TestConsumeFailureUnknownCodeRecorded(t *testing.T) {
	store, px, _ := newTestApp(t, &Account{UID: "u1", Realm: "cn", AccessToken: "a"})
	defer store.Close()
	_ = px.SyncAccounts()

	px.reportUpstreamError("u1", "m", http.StatusBadRequest,
		[]byte(`{"code":99999,"msg":"brand new error"}`), time.Time{}, upstream.ResetNone, nil)

	rows, err := store.ListConsumeFailures(10, "", "", 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("应有 1 条记录，得 %d 条（err=%v）", len(rows), err)
	}
	if rows[0].CodeKey != "99999" {
		t.Errorf("code_key = %q, want 99999（未知码按业务码本身聚合）", rows[0].CodeKey)
	}
	if rows[0].Scope != "" || rows[0].Policy != "" {
		t.Errorf("未知码不该有处置范围/策略：scope=%q policy=%q", rows[0].Scope, rows[0].Policy)
	}
}

// TestConsumeFailureDetailScrubbedAndTruncated detail 是**可查询**文本，
// 必须脱敏内部错误特征串、且截断到人眼可读长度。
func TestConsumeFailureDetailScrubbedAndTruncated(t *testing.T) {
	store, _, _ := newTestApp(t)
	defer store.Close()

	store.recordConsumeFailure(ConsumeFailure{
		Stage: FailStageSettle, UID: "u1",
		Detail: "预扣结算失败：SQL logic error: no such table: reservations (1)",
	})
	rows, _ := store.ListConsumeFailures(1, "", "", 0)
	if len(rows) != 1 {
		t.Fatalf("应有 1 条，得 %d", len(rows))
	}
	if strings.Contains(rows[0].Detail, "no such table") {
		t.Errorf("detail 泄漏了内部错误特征串：%q", rows[0].Detail)
	}

	// 超长：截断（不落整段上游错误体）。
	long := strings.Repeat("错误", 400)
	store.recordConsumeFailure(ConsumeFailure{Stage: FailStageCharge, UID: "u2", Detail: long})
	rows2, _ := store.ListConsumeFailures(1, "", "", 0)
	if n := len([]rune(rows2[0].Detail)); n > 241 {
		t.Errorf("detail 未截断：%d 个字符", n)
	}
}

// TestConsumeFailureRetentionCap 保留策略：超出上限丢最旧，且总行数恒定。
//
// 这是**必须**有的：这是每次失败都写一行的表，而失败率最高的时段正是它涨得最快
// 的时候（仓库里 usage_logs 就是因为没有保留策略而无限增长，已记为待处理技术债）。
func TestConsumeFailureRetentionCap(t *testing.T) {
	store, _, _ := newTestApp(t)
	defer store.Close()

	// 只写 cap+7 条（而不是写几万条）：断言的是"裁剪发生且总量收敛"，
	// 不需要用真实规模去压（那会让单测跑几十秒）。
	for i := 0; i < consumeFailureRetention+7; i++ {
		store.recordConsumeFailure(ConsumeFailure{
			Stage: FailStageUpstream, Code: 11140, UID: "u1",
			Detail: "第 " + itoa(i) + " 条",
		})
	}
	if n := store.CountConsumeFailures(); n > consumeFailureRetention {
		t.Errorf("台账行数 = %d，超过上限 %d（保留策略未生效）", n, consumeFailureRetention)
	}
	// 最旧的必须已被删掉（保证是"丢最旧"而不是"丢最新"）。
	rows, _ := store.ListConsumeFailures(1, "", "", 0)
	if len(rows) != 1 || !strings.Contains(rows[0].Detail, itoa(consumeFailureRetention+6)) {
		t.Errorf("保留的应是最新一条，得 %+v", rows)
	}
}

// TestConsumeFailureFilters 按码 / 阶段 / 窗口过滤（都是索引列）。
func TestConsumeFailureFilters(t *testing.T) {
	store, _, _ := newTestApp(t)
	defer store.Close()

	store.recordConsumeFailure(ConsumeFailure{Stage: FailStageUpstream, Code: 6004, UID: "a"})
	store.recordConsumeFailure(ConsumeFailure{Stage: FailStageUpstream, Code: 11140, UID: "b"})
	store.recordConsumeFailure(ConsumeFailure{Stage: FailStageSettle, UID: "c"})
	// 一条"昨天"的记录，用于验证窗口过滤。
	old := &ConsumeFailure{Stage: FailStageUpstream, Code: 6004, UID: "d",
		At: time.Now().Add(-48 * time.Hour).Unix()}
	if err := store.InsertConsumeFailure(old); err != nil {
		t.Fatal(err)
	}

	all, _ := store.ListConsumeFailures(50, "", "", 0)
	if len(all) != 4 {
		t.Errorf("不设窗口应得 4 条，得 %d", len(all))
	}
	byCode, _ := store.ListConsumeFailures(50, "6004", "", 0)
	if len(byCode) != 2 {
		t.Errorf("按码 6004 过滤应得 2 条，得 %d", len(byCode))
	}
	byStage, _ := store.ListConsumeFailures(50, "", FailStageSettle, 0)
	if len(byStage) != 1 {
		t.Errorf("按阶段 settle 过滤应得 1 条，得 %d", len(byStage))
	}
	since := time.Now().Add(-24 * time.Hour).Unix()
	byWin, _ := store.ListConsumeFailures(50, "", "", since)
	if len(byWin) != 3 {
		t.Errorf("近 24h 应得 3 条，得 %d", len(byWin))
	}

	// 统计：必须带上注册表里的处置说明（界面据此同时显示"几次"与"该怎么办"）。
	stats, err := store.ConsumeFailureStats(since)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 3 {
		t.Fatalf("统计应有 3 组，得 %d：%+v", len(stats), stats)
	}
	for _, st := range stats {
		if st.CodeKey == "6004" {
			if st.Scope != ScopeModel || st.Handling == "" {
				t.Errorf("6004 统计未带处置说明：%+v", st)
			}
			if st.Count != 1 {
				t.Errorf("6004 近 24h 计数 = %d, want 1（昨天那条不算）", st.Count)
			}
		}
	}
}

// TestConsumeFailureStatsCountsDescending 统计按次数降序 —— 界面第一眼看到的是
// "最常失败的是什么"。
func TestConsumeFailureStatsCountsDescending(t *testing.T) {
	store, _, _ := newTestApp(t)
	defer store.Close()
	for i := 0; i < 3; i++ {
		store.recordConsumeFailure(ConsumeFailure{Stage: FailStageUpstream, Code: 11140})
	}
	store.recordConsumeFailure(ConsumeFailure{Stage: FailStageUpstream, Code: 6004})
	stats, _ := store.ConsumeFailureStats(0)
	if len(stats) != 2 || stats[0].CodeKey != "11140" {
		t.Errorf("应按次数降序，得 %+v", stats)
	}
}

// TestCodeKeyOf 业务码优先于状态码；无业务码时按状态码族归类。
func TestCodeKeyOf(t *testing.T) {
	for _, tc := range []struct {
		code, status int
		want         string
	}{
		{6004, http.StatusTooManyRequests, "6004"},
		{11140, http.StatusForbidden, "11140"},
		{0, http.StatusTooManyRequests, "http:429"},
		{0, http.StatusPaymentRequired, "http:402"},
		{0, http.StatusForbidden, "http:403"},
		{0, http.StatusNotFound, "http:404"},
		{0, http.StatusBadGateway, "http:5xx"},
		{0, http.StatusBadRequest, ""},
		{0, http.StatusOK, ""},
	} {
		if got := codeKeyOf(tc.code, tc.status); got != tc.want {
			t.Errorf("codeKeyOf(%d,%d) = %q, want %q", tc.code, tc.status, got, tc.want)
		}
	}
}

// TestResetSrcSuffix 来源后缀可读且可 grep（台账/日志里靠它区分
// "上游说的时长"与"本地估的时长"）。
func TestResetSrcSuffix(t *testing.T) {
	for _, tc := range []struct {
		src  upstream.ResetSource
		want string
	}{
		{upstream.ResetBodyText, "body-text"},
		{upstream.ResetBodyJSON, "body-json"},
		{upstream.ResetHeader, "header"},
		{upstream.ResetExpired, "expired"},
		{upstream.ResetNone, "none"},
	} {
		got := resetSrcSuffix(tc.src)
		if !strings.Contains(got, tc.want) {
			t.Errorf("resetSrcSuffix(%q) = %q, 应含 %q", tc.src, got, tc.want)
		}
	}
}

// TestRequestShapeNoContent requestShape 只报形状、不落内容。
func TestRequestShapeNoContent(t *testing.T) {
	body := []byte(`{"stream":true,"messages":[{"role":"user","content":"我的银行卡号是 6222..."},{"role":"user","content":"x"}],"tools":[{},{}]}`)
	got := requestShape(body)
	if !strings.Contains(got, "messages=2") || !strings.Contains(got, "stream") || !strings.Contains(got, "tools=2") {
		t.Errorf("形状摘要不完整：%q", got)
	}
	if strings.Contains(got, "银行卡") {
		t.Errorf("形状摘要不得包含请求内容：%q", got)
	}
	if got := requestShape(nil); got == "" {
		t.Error("空请求体应给出可读摘要")
	}
	if got := requestShape([]byte("<html>")); !strings.Contains(got, "非 JSON") {
		t.Errorf("非 JSON 应显式说明：%q", got)
	}
}

// TestHandleConsumeFailuresEndpoint 管理接口返回统计 + 明细 + **策略表**。
func TestHandleConsumeFailuresEndpoint(t *testing.T) {
	store, px, app := newTestApp(t, &Account{UID: "u1", Realm: "cn", AccessToken: "a"})
	defer store.Close()
	_ = px.SyncAccounts()
	px.reportUpstreamError("u1", "m", http.StatusForbidden,
		[]byte(`{"code":11140,"msg":"request illegal"}`), time.Time{}, upstream.ResetNone, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/consume-failures?hours=24", nil)
	w := httptest.NewRecorder()
	app.handleConsumeFailures(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("HTTP %d，body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{"policies", "stats", "retention", "11140", "auto_recover"} {
		if !strings.Contains(body, want) {
			t.Errorf("响应缺 %q：%s", want, w.Body.String()[:200])
		}
	}
	// 策略表是**完整**回显的（不只是命中的那些）：界面要能说明"我们声明了哪些码"，
	// 这样"某码没出现在统计里"才能与"某码没被声明"区分开。
	if n := strings.Count(body, `"key":"`); n < len(failurePolicies()) {
		t.Errorf("策略表回显不完整：%d 条 < %d", n, len(failurePolicies()))
	}
}

// TestHandleConsumeFailuresBadParamsTolerated 只读查询的参数写错时给默认值、
// 不报 400（运维在地址栏手改参数不该被拦住）。
func TestHandleConsumeFailuresBadParamsTolerated(t *testing.T) {
	store, _, app := newTestApp(t)
	defer store.Close()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/consume-failures?limit=abc&hours=-5", nil)
	w := httptest.NewRecorder()
	app.handleConsumeFailures(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("非法参数应回落默认值，得 HTTP %d", w.Code)
	}
}
