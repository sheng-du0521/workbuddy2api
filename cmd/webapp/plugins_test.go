// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 插件中心（v0.31.16）的回归测试。
//
// 走**真 mux**：本功能的价值有一半在路由名（`/api/admin/plugins/{id}/claim`）
// 与鉴权中间件上，直接调 handler 测不出"路由没注册"。这是 authz_test.go 的老教训。
//
// 重点钉住四件事：
//  1. 停用后再执行**必须被拒**（停用不是纯前端效果）；
//  2. 出站走的是一次一个账号 —— 多传 uid 要报错，不许在服务端悄悄批量；
//  3. 「没有可领」是**正常态**（none）而不是失败（error）—— 否则运维每次点
//     都会看到一片红，进而忽略真正的失败；
//  4. 同一账号 30s 内不许重复出站（pluginGate，2026-10-13 事故的直接产物）。
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// plApp 装配：真 mux + 假上游可注入。
func plApp(t *testing.T, accs ...*Account) (*Store, *Proxy, *app, *http.ServeMux) {
	t.Helper()
	store, px, a := newTestApp(t, accs...)
	a.key = "pl-admin-key"
	a.registerLimiter = newRateLimiter(100, time.Hour)
	a.limiter = newClientLimiter()
	a.alerts = newAlertState()
	a.dataDir = t.TempDir()
	mux := a.routes()
	t.Cleanup(func() { store.Close() })
	return store, px, a, mux
}

// plReq 打一次管理接口（走 X-Admin-Key 通道，避免依赖会话 Cookie）。
func plReq(t *testing.T, mux *http.ServeMux, method, target, body string) (int, map[string]any) {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Admin-Key", "pl-admin-key")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	var out map[string]any
	_ = json.Unmarshal([]byte(rec.Body.String()), &out)
	return rec.Code, out
}

// plStub 假上游：按调用序号决定回什么（模拟"有补偿 / 没有补偿 / 网络挂"）。
type plStub struct {
	mu    sync.Mutex
	n     int
	body  string
	code  int
	paths []string
}

func (s *plStub) handler(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.n++
	s.paths = append(s.paths, r.URL.Path)
	body, code := s.body, s.code
	s.mu.Unlock()
	if code == 0 {
		code = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(body))
}

func (s *plStub) calls() (int, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n, append([]string(nil), s.paths...)
}

func plAcct(uid, nick, realm string) *Account {
	return &Account{
		UID: uid, Nickname: nick, Realm: realm,
		AccessToken: "tok-" + uid, RefreshToken: "rtok-" + uid,
		ExpiresAt:    time.Now().Add(48 * time.Hour).Unix(),
		ConsumeGroup: "apply", PoolState: "in",
	}
}

func plStubUp(t *testing.T, px *Proxy, body string, code int) *plStub {
	t.Helper()
	st := &plStub{body: body, code: code}
	up := httptest.NewServer(http.HandlerFunc(st.handler))
	px.uc.HTTP = &http.Client{Transport: rewriteTransport{target: up.URL}}
	t.Cleanup(up.Close)
	return st
}

// TestPluginListAndToggle 列表形状 + 停用**真的**拦住执行。
func TestPluginListAndToggle(t *testing.T) {
	_, _, _, mux := plApp(t, plAcct("pl-a", "A", "cn"))

	code, out := plReq(t, mux, "GET", "/api/admin/plugins", "")
	if code != http.StatusOK {
		t.Fatalf("列表应 200，得 %d", code)
	}
	list, _ := out["plugins"].([]any)
	// ⚠️ 断言"包含"而不是"数量恰好等于 N"（v0.31.31）：插件层存在的意义就是
	// 持续加插件，把数量写死会让每加一个插件都红一次，然后有人图省事把断言删掉。
	// 这里真正要守的是两件事：目标插件在列表里、且缺省为启用。
	first := findPlugin(list, "claim-compensation")
	if first == nil {
		t.Fatalf("插件列表里应包含 claim-compensation，得 %v", list)
	}
	if first["enabled"] != true {
		t.Fatalf("缺省应为启用，得 %v", first["enabled"])
	}

	// 停用
	code, out = plReq(t, mux, "POST", "/api/admin/plugins/claim-compensation/enabled", `{"enabled":false}`)
	if code != http.StatusOK || out["enabled"] != false {
		t.Fatalf("停用应 200 且回 enabled=false，得 %d %v", code, out)
	}
	_, out = plReq(t, mux, "GET", "/api/admin/plugins", "")
	list, _ = out["plugins"].([]any)
	if list[0].(map[string]any)["enabled"] != false {
		t.Fatal("停用后列表应显示 false")
	}

	// 停用后执行必须被拒（**不是**只在前端隐藏入口）
	code, out = plReq(t, mux, "POST", "/api/admin/plugins/claim-compensation/claim", `{"uids":["pl-a"]}`)
	if code != http.StatusForbidden {
		t.Fatalf("停用后执行应 403，得 %d %v", code, out)
	}

	// 恢复
	if code, _ := plReq(t, mux, "POST", "/api/admin/plugins/claim-compensation/enabled", `{"enabled":true}`); code != http.StatusOK {
		t.Fatalf("启用应 200，得 %d", code)
	}
	_, out = plReq(t, mux, "GET", "/api/admin/plugins", "")
	list, _ = out["plugins"].([]any)
	if list[0].(map[string]any)["enabled"] != true {
		t.Fatal("重新启用后列表应显示 true")
	}
}

// TestPluginToggleUnknownID 未注册的插件 id 不许写 kv（否则 kv 会被垃圾键撑大）。
func TestPluginToggleUnknownID(t *testing.T) {
	_, _, _, mux := plApp(t)
	if code, _ := plReq(t, mux, "POST", "/api/admin/plugins/nope/enabled", `{"enabled":false}`); code != http.StatusNotFound {
		t.Fatalf("未知插件应 404，得 %d", code)
	}
	if code, _ := plReq(t, mux, "POST", "/api/admin/plugins/nope/claim", `{"uids":["x"]}`); code != http.StatusNotFound {
		t.Fatalf("未知插件执行应 404，得 %d", code)
	}
}

// TestPluginClaimCompensationOK 到账路径：正确解析 credit，且只出站一次。
func TestPluginClaimCompensationOK(t *testing.T) {
	_, px, _, mux := plApp(t, plAcct("pl-a", "甲", "cn"))
	st := plStubUp(t, px, `{"code":0,"msg":"ok","data":{"credit":88}}`, 0)

	code, out := plReq(t, mux, "POST", "/api/admin/plugins/claim-compensation/claim", `{"uids":["pl-a"]}`)
	if code != http.StatusOK {
		t.Fatalf("应 200，得 %d %v", code, out)
	}
	if out["status"] != claimOK {
		t.Fatalf("应到账，得 %v（message=%v）", out["status"], out["message"])
	}
	if n, _ := st.calls(); n != 1 {
		t.Fatalf("单号执行应恰好出站 1 次，得 %d", n)
	}
}

// TestPluginClaimNormalStateIsNotError 「没有可领」是正常态，不是失败。
// 这条直接决定运维看到的是"一片红"还是"一列灰色正常"。
func TestPluginClaimNormalStateIsNotError(t *testing.T) {
	_, px, _, mux := plApp(t, plAcct("pl-a", "甲", "cn"))
	plStubUp(t, px, `{"code":40001,"msg":"没有可领取的补偿"}`, 0) // 真实形态：HTTP 200 + code!=0

	code, out := plReq(t, mux, "POST", "/api/admin/plugins/claim-compensation/claim", `{"uids":["pl-a"]}`)
	if code != http.StatusOK {
		t.Fatalf("业务拒绝仍应 200（接口本身成功），得 %d", code)
	}
	if out["status"] != claimNone {
		t.Fatalf("上游 4xx 业务拒绝应归为 none，得 %v", out["status"])
	}
}

// TestPluginClaimUpstreamFailureIsError 真失败必须如实报红（网络层错误）。
func TestPluginClaimUpstreamFailureIsError(t *testing.T) {
	_, px, _, mux := plApp(t, plAcct("pl-a", "甲", "cn"))
	plStubUp(t, px, `{"msg":"boom"}`, http.StatusInternalServerError)

	_, out := plReq(t, mux, "POST", "/api/admin/plugins/claim-compensation/claim", `{"uids":["pl-a"]}`)
	if out["status"] != claimError {
		t.Fatalf("上游 5xx 应归为 error，得 %v", out["status"])
	}
}

// TestPluginClaimSkipsGlobal 国际版账号不盲试（scheduler 已有同判据）。
func TestPluginClaimSkipsGlobal(t *testing.T) {
	_, px, _, mux := plApp(t, plAcct("pl-g", "国际号", "global"))
	st := plStubUp(t, px, `{"code":0,"data":{"credit":1}}`, 0)

	_, out := plReq(t, mux, "POST", "/api/admin/plugins/claim-compensation/claim", `{"uids":["pl-g"]}`)
	if out["status"] != claimSkip {
		t.Fatalf("global 应跳过，得 %v", out["status"])
	}
	if n, _ := st.calls(); n != 0 {
		t.Fatalf("跳过必须**零出站**，得 %d", n)
	}
}

// TestPluginClaimRejectsBatch 服务端不做批量（批量由前端串行循环完成）。
//
// 这条是**出站纪律的结构性保证**：如果能在一个 HTTP 请求里塞 N 个账号，
// 就会出现"一次调用打 N 次上游"的形态 —— 那正是 2026-10-13 事故的形状。
func TestPluginClaimRejectsBatch(t *testing.T) {
	_, px, _, mux := plApp(t, plAcct("pl-a", "甲", "cn"), plAcct("pl-b", "乙", "cn"))
	st := plStubUp(t, px, `{"code":0,"data":{"credit":1}}`, 0)

	code, out := plReq(t, mux, "POST", "/api/admin/plugins/claim-compensation/claim",
		`{"uids":["pl-a","pl-b"]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("多号应 400（要求前端串行），得 %d %v", code, out)
	}
	if n, _ := st.calls(); n != 0 {
		t.Fatalf("被拒的请求不许出站，得 %d", n)
	}
}

// TestPluginClaimThrottledSameAccount 同一账号 30s 内第二次必须被闸门拦下。
func TestPluginClaimThrottledSameAccount(t *testing.T) {
	_, px, _, mux := plApp(t, plAcct("pl-a", "甲", "cn"))
	st := plStubUp(t, px, `{"code":0,"data":{"credit":5}}`, 0)

	if _, out := plReq(t, mux, "POST", "/api/admin/plugins/claim-compensation/claim", `{"uids":["pl-a"]}`); out["status"] != claimOK {
		t.Fatalf("首次应成功，得 %v", out)
	}
	_, out := plReq(t, mux, "POST", "/api/admin/plugins/claim-compensation/claim", `{"uids":["pl-a"]}`)
	if out["status"] != claimError {
		t.Fatalf("30s 内重复领取应被闸门拦下并如实报错，得 %v", out["status"])
	}
	if msg, _ := out["message"].(string); !strings.Contains(msg, "频繁") {
		t.Fatalf("拦下时应说明原因，得 %q", msg)
	}
	if n, _ := st.calls(); n != 1 {
		t.Fatalf("被闸门拦下的请求不许出站，得 %d", n)
	}
}

// TestPluginClaimDryRun 预览不出站（前端"先看看有几个号"用）。
func TestPluginClaimDryRun(t *testing.T) {
	_, px, _, mux := plApp(t, plAcct("pl-a", "甲", "cn"), plAcct("pl-b", "乙", "cn"))
	st := plStubUp(t, px, `{"code":0,"data":{"credit":1}}`, 0)

	code, out := plReq(t, mux, "POST", "/api/admin/plugins/claim-compensation/claim",
		`{"all":true,"dry_run":true}`)
	if code != http.StatusOK {
		t.Fatalf("dry_run 应 200，得 %d", code)
	}
	if out["total"] != float64(2) {
		t.Fatalf("dry_run 应列出 2 个号，得 %v", out["total"])
	}
	if n, _ := st.calls(); n != 0 {
		t.Fatalf("dry_run 必须零出站，得 %d", n)
	}
}

// TestPluginUserListRequiresLogin 用户侧路由存在且需要登录。
func TestPluginUserListRequiresLogin(t *testing.T) {
	_, _, _, mux := plApp(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/plugins", nil))
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Fatalf("未登录访问 /api/plugins 应 401/403，得 %d", rec.Code)
	}
}

// TestPluginResolveTargetsOwnerScope 用户侧**按号主收窄**，且越权目标被静默剔除
// （不报错 —— 否则接口就成了"探测别人 uid 是否存在"的探针）。
func TestPluginResolveTargetsOwnerScope(t *testing.T) {
	mine := plAcct("pl-mine", "我的", "cn")
	mine.OwnerUserID = 7
	other := plAcct("pl-other", "别人的", "cn")
	other.OwnerUserID = 8
	_, _, a, _ := plApp(t, mine, other)

	got, err := a.resolveClaimTargets(claimRequest{UIDs: []string{"pl-mine", "pl-other"}}, 7)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(got) != 1 || got[0].UID != "pl-mine" {
		t.Fatalf("用户 7 只应拿到自己的号，得 %+v", got)
	}
	// 管理端（ownerScope<0）应拿到全部
	all, _ := a.resolveClaimTargets(claimRequest{All: true}, -1)
	if len(all) != 2 {
		t.Fatalf("管理端应拿到全部 2 个，得 %d", len(all))
	}
	// 空筛选且未声明 All → 一个都不处理（避免"误当全选"）
	none, _ := a.resolveClaimTargets(claimRequest{}, -1)
	if len(none) != 0 {
		t.Fatalf("空筛选不该被当成全选，得 %d", len(none))
	}
}

// TestPluginClaimNoCreditExposesRawData credit==0 时必须把**上游原文**透出。
//
// 这条钉的是一个排障能力：`credit == 0` 有两种成因（真的无可领 / 我们猜错了
// 字段名），从 credit 上完全一样。没有原文时，运维看到永远是"无可领"，
// 无法判断插件到底有没有在工作 —— 而这两者的处置完全相反。
func TestPluginClaimNoCreditExposesRawData(t *testing.T) {
	_, px, _, mux := plApp(t, plAcct("pl-a", "甲", "cn"))
	// 刻意用一个「有内容但没有 credit 字段」的回执来模拟"字段名猜错"的情形。
	plStubUp(t, px, `{"code":0,"data":{"points":123,"note":"已领取"}}`, 0)

	_, out := plReq(t, mux, "POST", "/api/admin/plugins/claim-compensation/claim", `{"uids":["pl-a"]}`)
	if out["status"] != claimNone {
		t.Fatalf("无 credit 字段应归 none，得 %v", out["status"])
	}
	d, _ := out["detail"].(string)
	if !strings.Contains(d, "points") {
		t.Fatalf("必须透出上游原文（区分「无可领」与「字段名猜错」的唯一事实），得 %q", d)
	}
}

// findPlugin 在插件列表里按 id 找一条（找不到返回 nil）。
func findPlugin(list []any, id string) map[string]any {
	for _, x := range list {
		if m, ok := x.(map[string]any); ok && m["id"] == id {
			return m
		}
	}
	return nil
}
