// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/upstream"
)

// 本文件是 v0.28.0（R4 指纹规则运营侧）的回归测试：kv 持久化 + 四个管理接口。
//
// 两条贯穿始终的纪律：
//  1. **必须清零全局规则集**：规则装在 upstream 的包级指针上，跨用例会互相污染；
//  2. **必须走真 mux**：这些接口的价值有一半在 adminGuard / CSRF / 路由名上，
//     直接调 handler 测不出"路由没注册"或"名字写错"（本仓 authz_test.go 的教训）。

// fpRulesApp 装配指纹规则用例用的 app + 真 mux，并保证用例结束后规则集归零。
func fpRulesApp(t *testing.T) (*Store, *app, *http.ServeMux) {
	t.Helper()
	store, ap, mux := newP3App(t)
	// 用例结束必须卸载规则：否则后续用例（甚至其它文件）会带着一套规则跑。
	t.Cleanup(func() { _ = upstream.SetFingerprintRules(nil) })
	if err := upstream.SetFingerprintRules(nil); err != nil {
		t.Fatalf("初始化清空规则失败：%v", err)
	}
	return store, ap, mux
}

// fpReq 打一次管理接口（走 X-Admin-Key 通道，避免依赖会话 Cookie）。
func fpReq(t *testing.T, mux *http.ServeMux, method, target, body string) (int, map[string]any) {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Admin-Key", "p3-admin-key") // newP3App 里的 a.key
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	var out map[string]any
	_ = json.Unmarshal([]byte(rec.Body.String()), &out)
	return rec.Code, out
}

// customRulesOf 从响应里取 custom 列表。
func customRulesOf(t *testing.T, body map[string]any) []any {
	t.Helper()
	raw, _ := body["custom"].([]any)
	return raw
}

// TestFingerprintRulesRouteRegistered 路由存在且需鉴权。
//
// 先钉这一条：路由名写错 / 忘注册时，下面的用例全会以一个"404 里没有 custom 字段"
// 的迷惑现象失败，而看不出根因是路由。单独一条断言把这两种失败区分开。
func TestFingerprintRulesRouteRegistered(t *testing.T) {
	store, _, mux := fpRulesApp(t)
	// ⚠️ 必须关：t.TempDir 的清理会因为「文件被占用」报错，而那是**清理失败**，
	// 不是断言失败 —— 只看 "FAIL" 两个字会误判成业务缺陷。
	defer store.Close()
	code, body := fpReq(t, mux, "GET", "/api/admin/fingerprint-rules", "")
	if code != http.StatusOK {
		t.Fatalf("GET 规则列表应 200，got %d body=%v", code, body)
	}
	if _, ok := body["builtin"]; !ok {
		t.Fatalf("响应缺少 builtin 字段：%v", body)
	}
	if _, ok := body["custom"]; !ok {
		t.Fatalf("响应缺少 custom 字段：%v", body)
	}
	if body["max"] == nil {
		t.Fatalf("响应缺少 max（界面要据此提示「还能加多少」）")
	}
	// 无鉴权必须被挡（这些接口能改变**所有**出站请求的内容，是最敏感的一类）。
	req := httptest.NewRequest("GET", "/api/admin/fingerprint-rules", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("无鉴权的请求不应通过，got %d", rec.Code)
	}
}

// TestFingerprintRulesUpsertAndDelete 增 → 读回 → 删 → 读回，且持久化落 kv。
func TestFingerprintRulesUpsertAndDelete(t *testing.T) {
	store, _, mux := fpRulesApp(t)
	defer store.Close()

	code, body := fpReq(t, mux, "POST", "/api/admin/fingerprint-rules",
		`{"kind":"literal","match":"UPSTREAM-FP-TOKEN","replace":"UPSTREAM FP TOKEN","note":"来自样本"}`)
	if code != http.StatusOK {
		t.Fatalf("新增应 200，got %d body=%v", code, body)
	}
	list := customRulesOf(t, body)
	if len(list) != 1 {
		t.Fatalf("新增后应有 1 条规则，实际 %d", len(list))
	}
	rule := list[0].(map[string]any)
	id, _ := rule["id"].(string)
	if id == "" {
		t.Fatalf("响应里的规则必须带 ID（前端据此删除）：%v", rule)
	}
	if rule["source"] != "manual" {
		t.Errorf("未指定来源时应落到 manual，实际 %v", rule["source"])
	}

	// GET 读回必须与 POST 返回一致（否则界面刷新后会"看不到刚加的那条"）。
	code, body = fpReq(t, mux, "GET", "/api/admin/fingerprint-rules", "")
	if code != http.StatusOK || len(customRulesOf(t, body)) != 1 {
		t.Fatalf("GET 应读回 1 条，code=%d body=%v", code, body)
	}

	// 持久化：kv 里必须有，且**与内存一致**（重启前后 ID 不能变）。
	raw, err := store.GetKV(kvFingerprintRules)
	if err != nil || strings.TrimSpace(raw) == "" {
		t.Fatalf("规则未落 kv：err=%v raw=%q", err, raw)
	}
	if !strings.Contains(raw, "UPSTREAM-FP-TOKEN") {
		t.Errorf("kv 内容不含新增的规则：%s", raw)
	}

	// 删除（幂等：不存在的 ID 返回成功 + changed=false，界面重复点不会看到报错）。
	code, body = fpReq(t, mux, "DELETE", "/api/admin/fingerprint-rules?id="+id, "")
	if code != http.StatusOK || body["changed"] != true {
		t.Fatalf("删除应 200 且 changed=true，code=%d body=%v", code, body)
	}
	if len(customRulesOf(t, body)) != 0 {
		t.Fatalf("删除后应为空，实际 %v", body["custom"])
	}
	code, body = fpReq(t, mux, "DELETE", "/api/admin/fingerprint-rules?id="+id, "")
	if code != http.StatusOK || body["changed"] != false {
		t.Fatalf("重复删除应 200 且 changed=false，code=%d body=%v", code, body)
	}
	if _, has := body["note"]; !has {
		t.Errorf("重复删除应给出说明（「该规则不存在」），body=%v", body)
	}
	// 缺 id → 400（不是静默成功）。
	if code, _ := fpReq(t, mux, "DELETE", "/api/admin/fingerprint-rules", ""); code != http.StatusBadRequest {
		t.Errorf("缺 id 应 400，got %d", code)
	}
}

// TestFingerprintRulesUpsertRejectsBadRule 坏规则必须 400 且**不落盘、不生效**。
//
// 这是"保存失败 ≠ 保存了一半"的证据：错误在写入之前发生。
func TestFingerprintRulesUpsertRejectsBadRule(t *testing.T) {
	store, _, mux := fpRulesApp(t)
	defer store.Close()

	code, body := fpReq(t, mux, "POST", "/api/admin/fingerprint-rules",
		`{"kind":"regex","match":".*","replace":"-","note":"过宽"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("空匹配正则应 400，got %d body=%v", code, body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "空串") {
		t.Errorf("错误文案应说明空匹配问题，实际 %q", msg)
	}
	// 不得落盘、不得生效。
	raw, _ := store.GetKV(kvFingerprintRules)
	if strings.TrimSpace(raw) != "" {
		t.Errorf("校验失败却写进了 kv：%q", raw)
	}
	if n := len(upstream.FingerprintRules()); n != 0 {
		t.Errorf("校验失败却安装了 %d 条规则", n)
	}
	// 过宽规则（命中普通文本）同样要拒。
	code, body = fpReq(t, mux, "POST", "/api/admin/fingerprint-rules",
		`{"kind":"literal","match":"func main","replace":"func-main"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("命中普通文本的规则应 400，got %d body=%v", code, body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "普通文本") {
		t.Errorf("错误文案应说明命中普通文本，实际 %q", msg)
	}
	// 请求格式错误也要有明确文案。
	if code, _ := fpReq(t, mux, "POST", "/api/admin/fingerprint-rules", "{not json"); code != http.StatusBadRequest {
		t.Errorf("坏 JSON 应 400，got %d", code)
	}
}

// TestFingerprintRulesUpsertIsIdempotentOnSameMatch 同一匹配串重复提交不产生第二条。
//
// 界面上的"保存"被点两次是常见操作，不幂等的话规则列表会堆满同一条的副本，
// 而运维会以为"有很多条不同的规则"。
func TestFingerprintRulesUpsertIsIdempotentOnSameMatch(t *testing.T) {
	store, _, mux := fpRulesApp(t)
	defer store.Close()
	for i := 0; i < 3; i++ {
		code, body := fpReq(t, mux, "POST", "/api/admin/fingerprint-rules",
			`{"kind":"literal","match":"REPEAT-SAME-MATCH","replace":"REPEAT SAME MATCH"}`)
		if code != http.StatusOK {
			t.Fatalf("第 %d 次提交应 200，got %d body=%v", i+1, code, body)
		}
		if n := len(customRulesOf(t, body)); n != 1 {
			t.Fatalf("第 %d 次提交后应仍为 1 条，实际 %d", i+1, n)
		}
	}
}

// TestFingerprintRuleTestEndpoint 试跑：保存前预演，且**不安装**候选规则。
func TestFingerprintRuleTestEndpoint(t *testing.T) {
	store, _, mux := fpRulesApp(t)
	defer store.Close()

	// 场景 1：候选集（保存前预演）。
	code, body := fpReq(t, mux, "POST", "/api/admin/fingerprint-rules/test",
		`{"text":"prefix SAMPLE-TOKEN suffix","rules":[{"kind":"literal","match":"SAMPLE-TOKEN","replace":"SAMPLE TOKEN"}]}`)
	if code != http.StatusOK {
		t.Fatalf("试跑应 200，got %d body=%v", code, body)
	}
	if body["after"] != "prefix SAMPLE TOKEN suffix" {
		t.Errorf("试跑结果不对：%v", body["after"])
	}
	if body["changed"] != true {
		t.Errorf("应报告 changed=true，body=%v", body)
	}
	if body["builtin_changed"] == true {
		t.Errorf("内置规则不该改写这段文本（否则说明语料或内置表有问题）")
	}
	// 预演不得安装规则。
	if n := len(upstream.FingerprintRules()); n != 0 {
		t.Fatalf("试跑不应安装规则，实际 %d 条", n)
	}

	// 场景 2：不带 rules → 用当前已安装规则集（此时为空，结果原样）。
	code, body = fpReq(t, mux, "POST", "/api/admin/fingerprint-rules/test",
		`{"text":"prefix SAMPLE-TOKEN suffix"}`)
	if code != http.StatusOK || body["changed"] != false {
		t.Fatalf("无已安装规则时应原样返回，code=%d body=%v", code, body)
	}

	// 场景 3：内置规则确实会改写「11128」（证明 builtin_changed 这一路真的在工作）。
	code, body = fpReq(t, mux, "POST", "/api/admin/fingerprint-rules/test", `{"text":"错误码 11128 出现了"}`)
	if code != http.StatusOK || body["builtin_changed"] != true {
		t.Fatalf("内置规则应改写含 11128 的文本，code=%d body=%v", code, body)
	}

	// 空文本 / 非法候选集必须报错（而不是给一个"什么都没变"的假结果）。
	if code, _ := fpReq(t, mux, "POST", "/api/admin/fingerprint-rules/test", `{"text":""}`); code != http.StatusBadRequest {
		t.Errorf("空文本应 400，got %d", code)
	}
	code, body = fpReq(t, mux, "POST", "/api/admin/fingerprint-rules/test",
		`{"text":"abc","rules":[{"kind":"regex","match":"a*","replace":"-"}]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("非法候选集应 400，got %d body=%v", code, body)
	}
}

// TestFingerprintRuleReplayEndpoint 回放：用真实样本验证规则有效性，并按规则给出命中数。
//
// 关键点有两个：① 回放**不安装**规则；② 未命中的规则也要出现在 hits 里（值为 0）
// ——「一条都没命中」与「没被评估」是两件事，界面必须能区分。
func TestFingerprintRuleReplayEndpoint(t *testing.T) {
	store, _, mux := fpRulesApp(t)
	defer store.Close()

	// 造样本文件：两条不同样本 + 一条重复样本（按 key 去重）。
	dir := t.TempDir()
	fp := filepath.Join(dir, "fingerprint_samples.jsonl")
	lines := []string{
		`{"at":1,"kind":"channel_denied","key":"k1","system":"hello REJECT-ME world"}`,
		`{"at":2,"kind":"account_fault","key":"k2","system":"nothing here"}`,
		`{"at":3,"kind":"channel_denied","key":"k1","system":"hello REJECT-ME world"}`,
	}
	if err := os.WriteFile(fp, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("写样本文件：%v", err)
	}
	oldFp := fpSampleFile
	fpSampleFile = fp
	t.Cleanup(func() { fpSampleFile = oldFp })

	code, body := fpReq(t, mux, "POST", "/api/admin/fingerprint-rules/replay",
		`{"rules":[{"kind":"literal","match":"REJECT-ME","replace":"REJECT ME"},{"kind":"literal","match":"NEVER-PRESENT","replace":"x"}]}`)
	if code != http.StatusOK {
		t.Fatalf("回放应 200，got %d body=%v", code, body)
	}
	if n, _ := body["sample_count"].(float64); int(n) != 2 {
		t.Fatalf("去重后应有 2 条样本，实际 %v（body=%v）", body["sample_count"], body)
	}
	kinds, _ := body["sample_kinds"].(map[string]any)
	if kinds["channel_denied"] == nil {
		t.Errorf("应报告样本分类分布（判断这批规则针对哪类拦截）：%v", kinds)
	}
	hits, _ := body["hits"].(map[string]any)
	if len(hits) != 2 {
		t.Fatalf("每条规则都应有计数（含 0），实际 %d 项：%v", len(hits), hits)
	}
	nonzero, zero := 0, 0
	for _, v := range hits {
		switch n, _ := v.(float64); int(n) {
		case 0:
			zero++
		case 1:
			nonzero++
		default:
			t.Fatalf("命中数异常：%v", hits)
		}
	}
	if nonzero != 1 || zero != 1 {
		t.Fatalf("应恰好 1 条命中 1 次、1 条命中 0 次，实际 %v", hits)
	}
	// 回放不得安装规则。
	if n := len(upstream.FingerprintRules()); n != 0 {
		t.Fatalf("回放不应安装规则，实际 %d 条", n)
	}
	// 无样本文件时应当是"空集 + 不报错"（区分"没采到样本"与"接口坏了"）。
	fpSampleFile = filepath.Join(dir, "not-exists.jsonl")
	code, body = fpReq(t, mux, "POST", "/api/admin/fingerprint-rules/replay", `{}`)
	if code != http.StatusOK {
		t.Fatalf("无样本时回放应 200，got %d body=%v", code, body)
	}
	if n, _ := body["sample_count"].(float64); int(n) != 0 {
		t.Errorf("无样本时 sample_count 应为 0，实际 %v", body["sample_count"])
	}
}

// TestLoadFingerprintRulesFromKV 启动装配：kv 里有规则就装上，且**坏数据不阻断启动**。
//
// 后半段是本用例真正想钉的：规则是增强而非前置条件，坏 JSON 只能降级、不能崩。
// 但它必须**留痕**（日志）—— 静默降级会让人以为规则仍在生效。
func TestLoadFingerprintRulesFromKV(t *testing.T) {
	store, _, _ := fpRulesApp(t)
	defer store.Close()
	ap := &app{store: store}

	// 1) 空 kv → 0 条，不报错。
	if n, err := ap.loadFingerprintRules(); err != nil || n != 0 {
		t.Fatalf("空 kv 应得 0 条且无错，got n=%d err=%v", n, err)
	}

	// 2) 合法规则 → 装上（并能在 upstream 侧观察到）。
	good, _ := json.Marshal([]upstream.FingerprintRule{
		{Kind: "literal", Match: "PERSISTED-RULE", Replace: "PERSISTED RULE"},
	})
	if err := store.SetKV(kvFingerprintRules, string(good)); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	n, err := ap.loadFingerprintRules()
	if err != nil || n != 1 {
		t.Fatalf("应加载 1 条，got n=%d err=%v", n, err)
	}
	if !upstream.HasFingerprint("x PERSISTED-RULE y") {
		t.Fatalf("加载后预检必须命中（否则规则就是「装了但没接进管线」）")
	}
	after, _ := upstream.SanitizeText("x PERSISTED-RULE y")
	if !strings.Contains(after, "PERSISTED RULE") {
		t.Errorf("加载后的规则应生效，得到 %q", after)
	}

	// 3) 坏 JSON → 降级为 0 条 + 返回 error，但**不得 panic**。
	if err := store.SetKV(kvFingerprintRules, "{ this is not json"); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	n, err = ap.loadFingerprintRules()
	if err == nil {
		t.Errorf("坏 JSON 应返回 error（供启动日志留痕）")
	}
	if n != 0 {
		t.Errorf("坏 JSON 时应按 0 条运行，got %d", n)
	}
	if b := upstream.HasFingerprint("x PERSISTED-RULE y"); b {
		t.Errorf("降级后不应残留旧规则")
	}

	// 4) 合法 JSON 但校验不过（空匹配正则）→ 同样降级为 0 条。
	if err := store.SetKV(kvFingerprintRules,
		`[{"kind":"regex","match":".*","replace":"-"}]`); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	if n, err := ap.loadFingerprintRules(); err == nil || n != 0 {
		t.Errorf("非法规则应降级为 0 条并报错，got n=%d err=%v", n, err)
	}
	if len(upstream.FingerprintRules()) != 0 {
		t.Errorf("非法规则不得留在内存里")
	}
}

// TestFingerprintRuleUpsertAccumulates 追加语义：新规则不冲掉旧规则（单条 upsert 的价值）。
func TestFingerprintRuleUpsertAccumulates(t *testing.T) {
	store, _, mux := fpRulesApp(t)
	defer store.Close()
	for _, m := range []string{"ACCUM-ONE-XX", "ACCUM-TWO-XX", "ACCUM-THREE"} {
		code, body := fpReq(t, mux, "POST", "/api/admin/fingerprint-rules",
			`{"kind":"literal","match":"`+m+`","replace":"`+m+`R"}`)
		if code != http.StatusOK {
			t.Fatalf("新增 %s 失败：%d %v", m, code, body)
		}
	}
	if n := len(upstream.FingerprintRules()); n != 3 {
		t.Fatalf("三条不同规则应共存，实际 %d", n)
	}
}
