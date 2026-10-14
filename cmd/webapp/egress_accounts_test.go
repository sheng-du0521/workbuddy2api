// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// egrAccounts 调一次账号代理列表接口，返回解析后的视图。
func egrAccounts(t *testing.T, a *app) egressAccountsView {
	t.Helper()
	rec := httptest.NewRecorder()
	a.handleEgressAccounts(rec, httptest.NewRequest(http.MethodGet, "/api/admin/egress/accounts", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d，响应 %s", rec.Code, rec.Body.String())
	}
	var v egressAccountsView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("解析响应：%v", err)
	}
	return v
}

// stateOf 取某 uid 的状态（找不到直接 Fatal）。
func stateOf(t *testing.T, v egressAccountsView, uid string) egressAccountProxy {
	t.Helper()
	for _, it := range v.Accounts {
		if it.UID == uid {
			return it
		}
	}
	t.Fatalf("响应里没有 %s（共 %d 条）", uid, len(v.Accounts))
	return egressAccountProxy{}
}

// TestEgressAccountsStates 五种状态的判定（本功能的核心口径）。
//
// 判别关键：状态取决于「配置值」与「全局默认」两件事的组合，而不是只看配置值。
// 尤其 inherit 与 direct_implicit 的区别只在有没有全局默认 ——
// 前者没暴露本机 IP，后者暴露了，风控含义不同，不能合并。
func TestEgressAccountsStates(t *testing.T) {
	store, px, a := newTestApp(t,
		&Account{UID: "a-proxy", Nickname: "指定线路", AccessToken: "t", EgressProxy: "socks5://1.1.1.1:1080"},
		&Account{UID: "a-direct", Nickname: "显式直连", AccessToken: "t", EgressProxy: "direct"},
		&Account{UID: "a-blank", Nickname: "未指定", AccessToken: "t", EgressProxy: ""},
	)
	defer store.Close()
	defer px.Close()

	// 线路池里放一条与 a-proxy 相同的 URL，用于验证「命中线路台账时带出备注名」。
	if err := store.UpsertEgressProxy(&EgressProxy{
		ID: "p1", Name: "香港 01", URL: "socks5://1.1.1.1:1080", Enabled: true,
	}); err != nil {
		t.Fatalf("UpsertEgressProxy: %v", err)
	}

	// 场景 A：没有全局默认 → 未指定的账号是「未指定 · 直连」。
	v := egrAccounts(t, a)
	if got := stateOf(t, v, "a-proxy"); got.State != egrStateProxy || got.StateLabel != "指定线路" {
		t.Fatalf("指定线路状态不符：%+v", got)
	} else if got.ProxyName != "香港 01" {
		t.Fatalf("命中线路池时应带出备注名，得 %q", got.ProxyName)
	}
	if got := stateOf(t, v, "a-direct"); got.State != egrStateDirectExplicit {
		t.Fatalf("direct 哨兵应为显式直连，得 %s", got.State)
	}
	if got := stateOf(t, v, "a-blank"); got.State != egrStateDirectImplicit {
		t.Fatalf("无全局默认时未指定应为 direct_implicit，得 %s", got.State)
	}
	if v.Summary[egrStateProxy] != 1 || v.Summary[egrStateDirectExplicit] != 1 || v.Summary[egrStateDirectImplicit] != 1 {
		t.Fatalf("汇总计数不符：%+v", v.Summary)
	}

	// 场景 B：配上全局默认 → 未指定的账号变成「继承默认」（不再算直连）。
	if err := store.SetKV(kvEgressDefault, "http://2.2.2.2:8080"); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	px.ReloadSelection()
	v = egrAccounts(t, a)
	if v.DefaultEgress != "http://2.2.2.2:8080" {
		t.Fatalf("DefaultEgress 应回传全局默认，得 %q", v.DefaultEgress)
	}
	got := stateOf(t, v, "a-blank")
	if got.State != egrStateInherit {
		t.Fatalf("有全局默认时未指定应变为 inherit，得 %s", got.State)
	}
	if got.Resolved != "http://2.2.2.2:8080" {
		t.Fatalf("实际出口应为全局默认，得 %q", got.Resolved)
	}
	if got.Configured != "" {
		t.Fatalf("配置值应保持空（与「实际出口」分开返回），得 %q", got.Configured)
	}
	// 显式直连不受全局默认影响（这正是它与「未指定」的区别）。
	if got := stateOf(t, v, "a-direct"); got.State != egrStateDirectExplicit || got.Resolved != "direct" {
		t.Fatalf("显式直连不应被全局默认盖住：%+v", got)
	}
	if v.Summary[egrStateDirectImplicit] != 0 {
		t.Fatalf("配上默认后不应再有 direct_implicit，得 %d", v.Summary[egrStateDirectImplicit])
	}
}

// TestEgressAccountsBadAndSort 配置异常优先排序 + 异常状态判定。
//
// 「异常优先」是刻意的：坏代理会让账号被排除出选号，是最需要立刻处理的一类；
// 若按 uid 排序，它会被埋在几十条里看不见。
func TestEgressAccountsBadAndSort(t *testing.T) {
	store, px, a := newTestApp(t,
		&Account{UID: "z-good-proxy", Nickname: "Z 正常的", AccessToken: "t", EgressProxy: "socks5://1.1.1.1:1080"},
		&Account{UID: "a-blank", Nickname: "A 未指定的", AccessToken: "t"},
		&Account{UID: "m-bad", Nickname: "M 坏代理的", AccessToken: "t", EgressProxy: "http://[::1]:0"},
		&Account{UID: "b-direct", Nickname: "B 直连的", AccessToken: "t", EgressProxy: "direct"},
	)
	defer store.Close()
	defer px.Close()

	// 让坏代理进入池内错误态：SyncAccounts 会校验并在失败时记录 egress 错误。
	if err := px.SyncAccounts(); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}
	v := egrAccounts(t, a)

	bad := stateOf(t, v, "m-bad")
	if bad.State != egrStateBad {
		// 若本机环境对该 URL 恰好可解析（不该发生），至少不该崩；这里直接失败以暴露口径变化。
		t.Fatalf("非法代理应为 bad 状态，得 %s（egress_error=%q）", bad.State, bad.EgressError)
	}
	if bad.EgressError == "" {
		t.Fatalf("bad 状态应带出 egress_error 原因")
	}
	// 排序：bad 必须排在最前。
	if v.Accounts[0].UID != "m-bad" {
		t.Fatalf("异常账号应排最前，实际首位是 %s（顺序 %v）",
			v.Accounts[0].UID, uidsOf(v))
	}
	// 其后应是「未指定直连」（漏配风险），再是显式直连。
	if v.Accounts[1].State != egrStateDirectImplicit {
		t.Fatalf("第二位应是 direct_implicit，得 %s（顺序 %v）", v.Accounts[1].State, uidsOf(v))
	}
	if v.Accounts[2].State != egrStateDirectExplicit {
		t.Fatalf("第三位应是 direct_explicit，得 %s（顺序 %v）", v.Accounts[2].State, uidsOf(v))
	}
	// 汇总计数守恒。
	sum := 0
	for _, n := range v.Summary {
		sum += n
	}
	if sum != len(v.Accounts) {
		t.Fatalf("汇总计数应等于账号总数：%d vs %d", sum, len(v.Accounts))
	}
}

// uidsOf 调试用：把顺序压成 uid 列表。
func uidsOf(v egressAccountsView) []string {
	out := make([]string, 0, len(v.Accounts))
	for _, a := range v.Accounts {
		out = append(out, a.UID+":"+a.State)
	}
	return out
}

// TestEgressAccountsEmpty 空池：返回空数组（不是 null），汇总为全 0。
func TestEgressAccountsEmpty(t *testing.T) {
	store, px, a := newTestApp(t)
	defer store.Close()
	defer px.Close()

	rec := httptest.NewRecorder()
	a.handleEgressAccounts(rec, httptest.NewRequest(http.MethodGet, "/api/admin/egress/accounts", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("空池应 200，得 %d", rec.Code)
	}
	body := rec.Body.String()
	// JSON 里必须是 []，不能是 null —— 前端 .length 会因 null 直接报错。
	if !containsJSON(body, `"accounts":[]`) {
		t.Fatalf("空池应回空数组而非 null：%s", body)
	}
	var v egressAccountsView
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	if len(v.Accounts) != 0 || len(v.Summary) != 0 {
		t.Fatalf("空池应无条目与计数：%+v", v)
	}
}

// containsJSON 忽略空白地做子串判断（编码器不保证空格）。
func containsJSON(body, want string) bool {
	compact := ""
	for _, r := range body {
		if r == ' ' || r == '\n' || r == '\t' {
			continue
		}
		compact += string(r)
	}
	return len(compact) >= len(want) && indexOf(compact, want) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestStateLabelOf 中文标签唯一映射（前端直接用后端返回的，避免两端各写一份）。
func TestStateLabelOf(t *testing.T) {
	want := map[string]string{
		egrStateProxy:          "指定线路",
		egrStateInherit:        "继承默认",
		egrStateDirectExplicit: "显式直连",
		egrStateDirectImplicit: "未指定 · 直连",
		egrStateBad:            "配置异常",
	}
	for state, label := range want {
		if got := stateLabelOf(state); got != label {
			t.Errorf("stateLabelOf(%q) = %q, want %q", state, got, label)
		}
	}
	if got := stateLabelOf("unknown"); got != "unknown" {
		t.Errorf("未知状态应原样返回，得 %q", got)
	}
}
