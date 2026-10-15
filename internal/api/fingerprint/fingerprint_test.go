// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package fingerprint

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/upstream"
)

// ======================================================================
// 本包自己的测试网（v0.31.12，P2-7 第四刀第一刀）
//
// 既有 `cmd/webapp/fingerprintrules_test.go` 走的是**真实 mux 的 HTTP 契约**
// （最强的那种证明：契约逐字节未变），但它够不到几个内部不变量：
//
//	P1 **保存失败必须不改变现有规则** —— 要断言"kv 没被写 + 内存里还是旧规则"，
//	   在 HTTP 层只能看状态码，看不到"KV 是否被动过"；
//	P2 **坏 JSON 不阻断启动、但要清掉进程内残留** —— 需要先装一份好规则，
//	   再把 kv 换成坏的，才能验证"清理过"，而 HTTP 层无法制造这个中间态；
//	P3 **删除不存在的 ID 不应该写 kv** —— 需要计数 SetKV 调用次数；
//	P4 **对外 JSON 键名契约**（全项目的硬规则：期望键必须存在 且 Go 字段名键不存在）。
//
// 这些正是"把 handler 搬进可测的包"买到的能力：一个内存 Store 就够，不需要 app/DB。
// ======================================================================

// memStore 内存 kv —— 顺带数 SetKV 被调用了几次（P3 要用）。
type memStore struct {
	data     map[string]string
	setCalls int
	failSet  error
}

func newMemStore() *memStore { return &memStore{data: map[string]string{}} }

func (m *memStore) GetKV(key string) (string, error) { return m.data[key], nil }

func (m *memStore) SetKV(key, value string) error {
	m.setCalls++
	if m.failSet != nil {
		return m.failSet
	}
	m.data[key] = value
	return nil
}

// freshAPI 每个用例一份干净状态（规则集是 upstream 的**包级全局**，用例之间必须隔离）。
func freshAPI(t *testing.T, st Store, samples SampleReader) *API {
	t.Helper()
	_ = upstream.SetFingerprintRules(nil)
	t.Cleanup(func() { _ = upstream.SetFingerprintRules(nil) })
	return New(st, samples)
}

func validRule() upstream.FingerprintRule {
	return upstream.FingerprintRule{
		Kind: "literal", Match: "UPSTREAM-FP-TOKEN", Replace: "UPSTREAM FP TOKEN",
		Note: "来自样本",
	}
}

// jsonBody 解一个响应体（测试里只看键与值）。
func jsonBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON：%q err=%v", rec.Body.String(), err)
	}
	return out
}

// P1：**保存失败必须不改变现有规则**（先校验后落盘）—— 这是本域最重要的一条纪律。
//
// 若反过来（先落盘再校验），一次失败的保存会在 kv 里留下一份非法规则，
// 重启后启动路径**只能选择吞掉它**（要么拒绝启动，要么静默按无规则跑）——
// 两种结局都比"保存失败对生产零影响"糟。
func TestSaveValidationFailureDoesNotTouchKV(t *testing.T) {
	st := newMemStore()
	x := freshAPI(t, st, nil)

	// 先装一份合法规则并落盘。
	if err := x.Save([]upstream.FingerprintRule{validRule()}); err != nil {
		t.Fatalf("合法规则不该保存失败：%v", err)
	}
	before, setCalls := st.data[KVKey], st.setCalls
	if before == "" {
		t.Fatal("前置条件：合法规则应已落 kv")
	}

	// 再尝试保存一份**非法**规则（正则无法编译）。
	// 用 "(" 这种一定编译不过的匹配串，避免依赖"过宽自检"的具体阈值。
	bad := validRule()
	bad.Match = "("
	err := x.Save([]upstream.FingerprintRule{bad})
	if err == nil {
		t.Fatal("非法规则必须保存失败（否则界面会以为存上了）")
	}
	if st.setCalls != setCalls {
		t.Errorf("校验失败不该写 kv：SetKV 调用次数 %d → %d", setCalls, st.setCalls)
	}
	if st.data[KVKey] != before {
		t.Errorf("kv 内容被改动了：\n前 %q\n后 %q", before, st.data[KVKey])
	}
	// 内存里的规则集也必须仍是原来那份（否则"保存失败却生效了"更糟）。
	got := upstream.FingerprintRules()
	if len(got) != 1 || got[0].Match != validRule().Match {
		t.Fatalf("校验失败后已安装规则应保持原样，得 %+v", got)
	}
}

// P2：坏 JSON **不阻断启动**，但必须**清掉进程内残留**（否则运维以为规则还在生效）。
func TestLoadClearsInstalledRulesOnBadJSON(t *testing.T) {
	st := newMemStore()
	x := freshAPI(t, st, nil)

	// 先装一份好规则（模拟"上一次进程装过"）。
	if err := x.Save([]upstream.FingerprintRule{validRule()}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if len(upstream.FingerprintRules()) != 1 {
		t.Fatal("前置条件：已装 1 条规则")
	}

	// kv 被手工改坏（半截写入 / 误编辑）。
	st.data[KVKey] = "{ this is not json"

	n, err := x.Load()
	if err == nil {
		t.Error("坏 JSON 应回报错误（让调用方记一条 WARN），不能静默成功")
	}
	if n != 0 {
		t.Errorf("坏 JSON 时条数应为 0，得 %d", n)
	}
	if len(upstream.FingerprintRules()) != 0 {
		t.Fatalf("坏 JSON 必须清掉进程内残留（否则运维以为规则还在生效），得 %+v",
			upstream.FingerprintRules())
	}
}

// 空 kv = 没有自定义规则 → 0 条、无错、并显式清一次（热重载会走到）。
func TestLoadEmptyKV(t *testing.T) {
	st := newMemStore()
	x := freshAPI(t, st, nil)
	// 先装一份，验证"空 kv 会清掉它"。
	_ = upstream.SetFingerprintRules([]upstream.FingerprintRule{validRule()})

	n, err := x.Load()
	if err != nil || n != 0 {
		t.Fatalf("空 kv 应 (0, nil)，得 (%d, %v)", n, err)
	}
	if len(upstream.FingerprintRules()) != 0 {
		t.Fatalf("空 kv 应清空已安装规则，得 %+v", upstream.FingerprintRules())
	}
}

// 落盘的是**归一化后**的规则（ID / AddedAt 已补齐）—— 重启前后 ID 不能变，
// 否则运维无法引用某条规则去沟通。
func TestSavePersistsNormalizedRules(t *testing.T) {
	st := newMemStore()
	x := freshAPI(t, st, nil)

	if err := x.Save([]upstream.FingerprintRule{validRule()}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	var persisted []upstream.FingerprintRule
	if err := json.Unmarshal([]byte(st.data[KVKey]), &persisted); err != nil {
		t.Fatalf("kv 内容不是合法 JSON：%v", err)
	}
	live := upstream.FingerprintRules()
	if len(persisted) != len(live) {
		t.Fatalf("落盘条数 %d != 内存条数 %d", len(persisted), len(live))
	}
	if persisted[0].ID == "" || persisted[0].ID != live[0].ID {
		t.Fatalf("落盘的 ID 必须与内存一致：persisted=%q live=%q", persisted[0].ID, live[0].ID)
	}
	if persisted[0].AddedAt == 0 || persisted[0].AddedAt != live[0].AddedAt {
		t.Fatalf("落盘的 AddedAt 必须与内存一致：%d vs %d", persisted[0].AddedAt, live[0].AddedAt)
	}
}

// P3：删除**不存在**的 ID 是幂等成功，且**不该写 kv**（空写没必要，而且会把
// "整份重写"这个动作暴露给并发保存）。
func TestHandleRuleDeleteUnknownIDSkipsSave(t *testing.T) {
	st := newMemStore()
	x := freshAPI(t, st, nil)
	if err := x.Save([]upstream.FingerprintRule{validRule()}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	setCalls := st.setCalls

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/api/admin/fingerprint-rules?id=fp-does-not-exist", nil)
	x.HandleRuleDelete(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("删不存在的 ID 应幂等成功（界面重复点不该看到报错），got %d", rec.Code)
	}
	body := jsonBody(t, rec)
	if body["changed"] != false {
		t.Errorf("changed 应为 false，得 %v", body["changed"])
	}
	if body["note"] == nil {
		t.Errorf("应给出可读说明（告诉运维「可能已被删除」）")
	}
	if st.setCalls != setCalls {
		t.Errorf("未发生删除时不该写 kv：SetKV 次数 %d → %d", setCalls, st.setCalls)
	}
}

// 缺少 id → 400（而不是当成"删 0 条"）。
func TestHandleRuleDeleteMissingID(t *testing.T) {
	x := freshAPI(t, newMemStore(), nil)
	rec := httptest.NewRecorder()
	x.HandleRuleDelete(rec, httptest.NewRequest("DELETE", "/api/admin/fingerprint-rules", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 id 应 400，得 %d", rec.Code)
	}
}

// 请求体不是 JSON → 400（不是 500：这是调用方的问题）。
func TestHandleRuleUpsertRejectsBadBody(t *testing.T) {
	x := freshAPI(t, newMemStore(), nil)
	rec := httptest.NewRecorder()
	x.HandleRuleUpsert(rec, httptest.NewRequest("POST", "/api/admin/fingerprint-rules",
		strings.NewReader("{not json")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("坏请求体应 400，得 %d", rec.Code)
	}
}

// 单条 upsert 的语义：同 (kind, match) 视为同一条（幂等），不是"再加一条"。
func TestHandleRuleUpsertIsIdempotentOnSameMatch(t *testing.T) {
	st := newMemStore()
	x := freshAPI(t, st, nil)

	post := func() int {
		rec := httptest.NewRecorder()
		x.HandleRuleUpsert(rec, httptest.NewRequest("POST", "/api/admin/fingerprint-rules",
			strings.NewReader(`{"kind":"literal","match":"UPSTREAM-FP-TOKEN","replace":"A"}`)))
		if rec.Code != http.StatusOK {
			t.Fatalf("upsert 应 200，得 %d", rec.Code)
		}
		var custom []any
		for _, r := range jsonBody(t, rec)["custom"].([]any) {
			custom = append(custom, r)
		}
		return len(custom)
	}
	if n := post(); n != 1 {
		t.Fatalf("首次 upsert 后应有 1 条，得 %d", n)
	}
	if n := post(); n != 1 {
		t.Fatalf("同 match 再次 upsert 仍应是 1 条（幂等），得 %d", n)
	}
}

// 试跑：空文本 / 超长文本都是 400（边界在 handler 层，不靠下游碰运气）。
func TestHandleRuleTestRejectsBadInput(t *testing.T) {
	x := freshAPI(t, newMemStore(), nil)
	cases := []struct {
		name, body string
	}{
		{"空文本", `{"text":""}`},
		{"超长文本", `{"text":"` + strings.Repeat("a", (64<<10)+1) + `"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			x.HandleRuleTest(rec, httptest.NewRequest("POST", "/api/admin/fingerprint-rules/test",
				strings.NewReader(c.body)))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("应 400，得 %d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

// 回放：**未注入样本读取器**时必须如实报 500（接线缺陷），而不是假装"0 条样本"。
//
// 这两种情况的处置完全不同：0 条样本是正常的（还没采样），缺接线是部署/装配问题。
// 混为一谈会让运维去查"为什么没采到样本"，而真正的原因是漏了注入。
func TestHandleRuleReplayWithoutSampleReader(t *testing.T) {
	x := freshAPI(t, newMemStore(), nil) // samples = nil
	rec := httptest.NewRecorder()
	x.HandleRuleReplay(rec, httptest.NewRequest("POST", "/api/admin/fingerprint-rules/replay", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("缺样本读取器应 500（如实报接线缺陷），得 %d", rec.Code)
	}
	if msg, _ := jsonBody(t, rec)["error"].(string); !strings.Contains(msg, "样本") {
		t.Errorf("错误文案要说清是样本读取未接线，得 %q", msg)
	}
}

// 回放：注入了读取器时，正常返回计数与分布。
func TestHandleRuleReplayWithSampleReader(t *testing.T) {
	st := newMemStore()
	x := freshAPI(t, st, func() ([]string, map[string]int, error) {
		return []string{"UPSTREAM-FP-TOKEN 出现了", "另一条样本"}, map[string]int{"banned": 2}, nil
	})
	if err := x.Save([]upstream.FingerprintRule{validRule()}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	rec := httptest.NewRecorder()
	x.HandleRuleReplay(rec, httptest.NewRequest("POST", "/api/admin/fingerprint-rules/replay", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，得 %d body=%s", rec.Code, rec.Body.String())
	}
	body := jsonBody(t, rec)
	if body["sample_count"] != float64(2) {
		t.Errorf("sample_count 应为 2，得 %v", body["sample_count"])
	}
	if body["hits"] == nil {
		t.Errorf("应返回逐规则命中数")
	}
}

// 回放：样本读取器报错 → 500 且错误原文透出（不吞）。
func TestHandleRuleReplaySampleReaderError(t *testing.T) {
	x := freshAPI(t, newMemStore(), func() ([]string, map[string]int, error) {
		return nil, nil, errors.New("disk on fire")
	})
	rec := httptest.NewRecorder()
	x.HandleRuleReplay(rec, httptest.NewRequest("POST", "/api/admin/fingerprint-rules/replay", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("应 500，得 %d", rec.Code)
	}
	if msg, _ := jsonBody(t, rec)["error"].(string); !strings.Contains(msg, "disk on fire") {
		t.Errorf("错误原文应透出，得 %q", msg)
	}
}

// P4：对外 JSON 的**键名契约**（全项目硬规则）。
//
// 期望键必须存在，**且 Go 字段名键必须不存在** —— v0.29.0 的「提现审核按钮点不动」
// 就是内嵌无 json tag 的结构体让键名变成了 Go 字段名（`Status` 而不是 `status`），
// 前端读 `undefined`、接口一直 200、日志什么都没有。
func TestRulesViewJSONKeysContract(t *testing.T) {
	x := freshAPI(t, newMemStore(), nil)
	rec := httptest.NewRecorder()
	x.HandleRules(rec, httptest.NewRequest("GET", "/api/admin/fingerprint-rules", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，得 %d", rec.Code)
	}
	body := jsonBody(t, rec)
	want := []string{"builtin", "custom", "max", "applied"}
	for _, k := range want {
		if _, ok := body[k]; !ok {
			t.Errorf("响应缺少键 %q（前端按此读取）", k)
		}
	}
	for _, k := range []string{"Builtin", "Custom", "Max", "Applied"} {
		if _, ok := body[k]; ok {
			t.Errorf("出现了 Go 字段名键 %q —— 说明结构体缺 json tag（前端会读到 undefined）", k)
		}
	}
}

// 未装配的 API（Store=nil）不得 panic。
func TestNilStoreIsSafe(t *testing.T) {
	n, err := New(nil, nil).Load()
	if n != 0 || err != nil {
		t.Fatalf("Store=nil 应安全返回 (0,nil)，得 (%d,%v)", n, err)
	}
	var nilAPI *API
	if n, err := nilAPI.Load(); n != 0 || err != nil {
		t.Fatalf("nil API 应安全返回 (0,nil)，得 (%d,%v)", n, err)
	}
}

// Store 写失败要如实回报（否则界面会以为保存成功，而重启后规则消失）。
func TestSaveReportsStoreFailure(t *testing.T) {
	st := newMemStore()
	st.failSet = errors.New("kv 只读")
	x := freshAPI(t, st, nil)
	if err := x.Save([]upstream.FingerprintRule{validRule()}); err == nil {
		t.Fatal("kv 写失败必须回报错误（否则界面以为存上了，重启后规则消失）")
	}
}
