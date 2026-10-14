// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// 本文件覆盖 v0.24.1 的 P3-1（内部错误文本脱敏，见 docs/审查问题整改-v0.24.1.md）。
//
// 脱敏挂在**唯一出口** writeJSON 上，风险是「改错一处影响所有响应」，
// 所以这里的用例重点在**不该动的东西一个都别动**，以及**不许 panic**。

// TestScrubErrBodyDoesNotPanicOnUncomparableValues **本组最重要的回归**。
//
// 背景：脱敏层最初写成 `if nv := scrubErrField(k, val); nv != val` ——
// 用 `!=` 比较 `any`，而 **map / slice / func 是不可比较类型**，比较即 panic。
// 公告列表的响应体里带一个 `map[string]int` 计数，于是线上接口会 500，
// 而且不是"报个错"，是整个 handler 崩掉（`TestAnnouncementCRUD` 当场炸）。
//
// 这条用例把「响应体里可以有任意类型」钉死：只要它不 panic，就算过了。
func TestScrubErrBodyDoesNotPanicOnUncomparableValues(t *testing.T) {
	bodies := []any{
		map[string]any{"counts": map[string]int{"a": 1}},              // 触发原 panic 的形态
		map[string]any{"counts": map[string]string{"a": "b"}},         // map[string]string
		map[string]any{"list": []any{map[string]any{"x": 1}}},         // []any 里的 map
		map[string]any{"fn": func() {}, "ch": make(chan int, 1)},      // func / chan
		map[string]any{"err": map[string]any{"message": "余额不足"}},      // error 下嵌 map
		map[string]any{"nested": []map[string]any{{"error": "余额不足"}}}, // []map（非 []any）
		map[string]any{"num": 3, "flt": 1.5, "b": true, "nil": nil},   // 各种标量
		[]any{map[string]any{"error": "x"}},                           // 顶层是 slice
		"just a string",                                               // 顶层不是 map
		nil,                                                           // nil
	}
	for i, b := range bodies {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("第 %d 个响应体让 scrubErrBody panic：%v\n"+
						"（不可比较类型不能用 != 比较 —— 这正是本用例要拦住的回归）", i, r)
				}
			}()
			_ = scrubErrBody(b)
		}()
	}
}

// TestScrubErrBodyReplacesInternalText 内部错误文本被替换，且原文不再出现。
func TestScrubErrBodyReplacesInternalText(t *testing.T) {
	cases := []string{
		"SQL logic error: no such table: host_earnings (1)",
		"constraint failed: UNIQUE constraint failed: users.username (2067)",
		"no such column: foo",
		"database is locked",
		"sql: Scan error on column",
		"panic: runtime error: nil pointer dereference",
		"goroutine 1 [running]:",
		"open /x/y.go: permission denied",
	}
	for _, raw := range cases {
		body := map[string]any{"error": raw}
		got := scrubErrBody(body)
		s, _ := got.(map[string]any)["error"].(string)
		if s != scrubErrTextSafe {
			t.Errorf("内部错误文本应被替换为稳定文案，got %q（原始 %q）", s, raw)
		}
		if strings.Contains(s, "table") || strings.Contains(s, "column") ||
			strings.Contains(s, "constraint") {
			t.Errorf("脱敏后仍含内部结构名：%q", s)
		}
	}
}

// TestScrubErrBodyKeepsBusinessText 业务文案必须**原样放行**。
//
// 这条是脱敏能安全上线的关键：判据只认「内部实现一定会带、业务文案一定不会带」的
// 特征串（见 errsafe.go 的 errScrubMarkers）。若把通用词收进去，这里会立刻红。
func TestScrubErrBodyKeepsBusinessText(t *testing.T) {
	business := []string{
		"余额不足",
		"用户名已被占用",
		"密码至少 8 位",
		"请求格式错误",
		"账号不存在或不属于当前用户",
		"提现金额不能超过可提现额度",
		"上游限流，请稍后重试",
		"模型不存在或无权限",
	}
	for _, msg := range business {
		body := map[string]any{"error": msg}
		got := scrubErrBody(body)
		if s, _ := got.(map[string]any)["error"].(string); s != msg {
			t.Errorf("业务文案被误改：%q → %q", msg, s)
		}
	}
}

// TestScrubErrFieldKeyMatching 字段名匹配规则（含 error_* / *_error 变体）。
func TestScrubErrFieldKeyMatching(t *testing.T) {
	raw := "no such table: x"
	shouldMatch := []string{"error", "err", "Error", "ERR", "message", "detail", "reason", "error_detail", "db_error"}
	for _, k := range shouldMatch {
		if s, ok := scrubErrField(k, raw); !ok || s != scrubErrTextSafe {
			t.Errorf("键 %q 应命中脱敏，got (%q, %v)", k, s, ok)
		}
	}
	shouldNotMatch := []string{"name", "uid", "count", "data", "notes", "errorish"}
	for _, k := range shouldNotMatch {
		if _, ok := scrubErrField(k, raw); ok {
			t.Errorf("键 %q 不该被当作错误字段", k)
		}
	}
	// 值不是字符串 → 一律不处理（且不许 panic）。
	if _, ok := scrubErrField("error", map[string]int{"a": 1}); ok {
		t.Error("非字符串值不该被判为已脱敏")
	}
	if _, ok := scrubErrField("error", nil); ok {
		t.Error("nil 值不该被判为已脱敏")
	}
}

// TestScrubErrBodyNestedOneLevel 一层嵌套（{"error": {...}} 与 [{...}]）也要覆盖。
func TestScrubErrBodyNestedOneLevel(t *testing.T) {
	body := map[string]any{
		"error": map[string]any{"message": "no such table: secret"},
		"items": []any{
			map[string]any{"uid": "u1", "error": "constraint failed: UNIQUE x"},
			map[string]any{"uid": "u2", "error": "余额不足"}, // 业务文案，不该动
		},
	}
	got := scrubErrBody(body).(map[string]any)

	inner := got["error"].(map[string]any)
	if s, _ := inner["message"].(string); s != scrubErrTextSafe {
		t.Errorf("嵌套 error.message 应被脱敏，got %q", s)
	}
	items := got["items"].([]any)
	if s, _ := items[0].(map[string]any)["error"].(string); s != scrubErrTextSafe {
		t.Errorf("批量项里的 error 应被脱敏，got %q", s)
	}
	if s, _ := items[1].(map[string]any)["error"].(string); s != "余额不足" {
		t.Errorf("批量项里的业务文案不该被动，got %q", s)
	}
}

// TestWriteJSONScrubsAndKeepsValidJSON 端到端：writeJSON 之后响应体仍是合法 JSON。
//
// 脱敏是**原地改 map**，所以顺带确认序列化没被搞坏（这是"挂在唯一出口"的代价：
// 一旦写错，所有接口的响应都坏）。
func TestWriteJSONScrubsAndKeepsValidJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, 500, map[string]any{
		"error":  "SQL logic error: no such table: t (1)",
		"counts": map[string]int{"a": 1},
		"ok":     false,
	})
	if rec.Code != 500 {
		t.Fatalf("状态码不该被脱敏层改动，got %d", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应体应是合法 JSON：%v（body=%s）", err, rec.Body.String())
	}
	if s, _ := out["error"].(string); s != scrubErrTextSafe {
		t.Errorf("error 应被脱敏，got %q", s)
	}
	if _, ok := out["counts"].(map[string]any); !ok {
		t.Error("非错误字段应原样保留")
	}
}
