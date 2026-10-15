// 旧网关兼容层的端到端验收（v0.31.4）。
//
// 这些端点的形状不是"我们的设计"，而是**别人已经在逐字解析的契约**：
// cmd/acct（账号表 / 停用恢复复活 / 跑任务）、checkin.sh、dev.sh、login.sh。
// 所以断言必须钉在**键名与状态码**上，而不是"接口返回 200 就算过"。
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/upstream"
)

// newCompatApp 一个账号 + 真 mux 的装配（管理密钥固定）。
func newCompatApp(t *testing.T, uid string) (*Store, *Proxy, *app, *http.ServeMux) {
	t.Helper()
	store, px, a := newTestApp(t, &Account{
		UID: uid, Realm: "cn", Nickname: "compat-nick",
		AccessToken: "at", RefreshToken: "rt",
		ConsumeGroup: "apply", PoolState: "in",
	})
	// 用到 store 必须显式 Close：否则 t.TempDir 清理时 SQLite 文件仍被占用
	//（本仓既有纪律，测试报的是 "file is being used by another process"）。
	t.Cleanup(func() { _ = store.Close() })
	a.key = "compat-admin-key"
	if err := px.SyncAccounts(); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}
	px.pool.SetCredits(uid, 1234)
	return store, px, a, a.routes()
}

func compatDo(t *testing.T, mux *http.ServeMux, method, path, key, body string) (int, map[string]any) {
	t.Helper()
	var rdr *strings.Reader = strings.NewReader(body)
	req := httptest.NewRequest(method, path, rdr)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// TestCompatStatusContract GET /status 的键名契约（cmd/acct 与 checkin.sh 逐字解析）。
//
// 双向断言：snake_case 键**必须存在**，且 Go 字段名键**必须不存在** ——
// 只断言前者的话，"两套都下发"的半修法照样通过（v0.29.0 提现按钮那次就是）。
func TestCompatStatusContract(t *testing.T) {
	const uid = "cp-acct-0001"
	_, _, _, mux := newCompatApp(t, uid)

	code, out := compatDo(t, mux, http.MethodGet, "/status", "compat-admin-key", "")
	if code != http.StatusOK {
		t.Fatalf("code=%d want 200", code)
	}
	for _, k := range []string{"accounts", "total", "healthy", "cooling", "disabled", "in_flight_full"} {
		if _, ok := out[k]; !ok {
			t.Errorf("/status 缺键 %q（旧版 CLI/脚本依赖它）：%v", k, out)
		}
	}
	arr, ok := out["accounts"].([]any)
	if !ok || len(arr) != 1 {
		t.Fatalf("accounts 应为 1 条数组，got %v", out["accounts"])
	}
	acc := arr[0].(map[string]any)
	for _, k := range []string{"uid", "realm", "nickname", "credits", "disabled", "manual_disabled", "cooling"} {
		if _, ok := acc[k]; !ok {
			t.Errorf("accounts[] 缺键 %q（acct 用 json:\"%s\" 解析）：%v", k, k, acc)
		}
	}
	if acc["uid"] != uid {
		t.Errorf("uid=%v want %s", acc["uid"], uid)
	}
	if acc["manual_disabled"] != false {
		t.Errorf("manual_disabled 应为 false（零值也必须下发，运维口径）")
	}
	// 负向：Go 字段名键不得出现（内嵌无 json tag 结构体的典型症状）。
	for _, bad := range []string{"UID", "Nickname", "ManualDisabled", "DisabledReason"} {
		if _, ok := acc[bad]; ok {
			t.Errorf("accounts[] 不该出现 Go 字段名键 %q：%v", bad, acc)
		}
	}
}

// TestCompatGuardRequiresAdminKey 兼容层鉴权：无密钥/错密钥一律 401。
func TestCompatGuardRequiresAdminKey(t *testing.T) {
	_, _, _, mux := newCompatApp(t, "cp-acct-0002")
	cases := []struct {
		name, key, path, method string
	}{
		{"status 无密钥", "", "/status", http.MethodGet},
		{"status 错密钥", "wrong", "/status", http.MethodGet},
		{"disable 无密钥", "", "/admin/accounts/cp-acct-0002/disable", http.MethodPost},
		{"tasks 无密钥", "", "/admin/tasks/checkin/run", http.MethodPost},
		{"stats reset 无密钥", "", "/v1/stats/reset", http.MethodPost},
	}
	for _, c := range cases {
		code, _ := compatDo(t, mux, c.method, c.path, c.key, "")
		if code != http.StatusUnauthorized {
			t.Errorf("%s: code=%d want 401", c.name, code)
		}
	}
	// X-Admin-Key 通道同样可用（脚本两种写法都出现过）。
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.Header.Set("X-Admin-Key", "compat-admin-key")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("X-Admin-Key 通道应放行，code=%d", rec.Code)
	}
}

// TestCompatDisableEnableReviveBits 停用 / 启用 / 复活各自清**该清的位**。
//
// 这是本项目最难查的一类缺陷（「点了启用毫无效果但没报错」）的接口面：
// 池内有两个互相独立的停用位，enable 解手动位、revive 解系统位，绝不能互相代劳。
func TestCompatDisableEnableReviveBits(t *testing.T) {
	const uid = "cp-acct-0003"
	_, px, _, mux := newCompatApp(t, uid)

	// ① 手动停用（带原因）
	code, out := compatDo(t, mux, http.MethodPost, "/admin/accounts/"+uid+"/disable", "compat-admin-key", `{"reason":"维护中"}`)
	if code != http.StatusOK {
		t.Fatalf("disable code=%d body=%v", code, out)
	}
	if out["manual_disabled"] != true || out["manual_reason"] != "维护中" {
		t.Errorf("手动停用位/原因未生效：%v", out)
	}
	if out["changed"] != true {
		t.Errorf("首次停用 changed 应为 true：%v", out)
	}
	st, _ := px.pool.Status(uid)
	if !st.ManualDisabled || st.ManualReason != "维护中" {
		t.Errorf("池侧手动位未生效：%+v", st)
	}

	// ② 让系统也把它禁用（自动位）——走唯一闸门（12153 是合格 kind）。
	if !px.pool.RetireAccount(uid, upstream.ErrSessionDead, "test: 模拟系统自动禁用") {
		t.Fatal("RetireAccount 被拒（用例前提不成立）")
	}
	st, _ = px.pool.Status(uid)
	if !st.Disabled {
		t.Fatal("自动禁用位未置位（用例前提不成立）")
	}

	// ③ enable 只清手动位：自动位仍在 → 调用方能从 disabled 字段看到"还需 revive"
	code, out = compatDo(t, mux, http.MethodPost, "/admin/accounts/"+uid+"/enable", "compat-admin-key", "")
	if code != http.StatusOK {
		t.Fatalf("enable code=%d body=%v", code, out)
	}
	if out["manual_disabled"] != false {
		t.Errorf("enable 应清手动位：%v", out)
	}
	if out["disabled"] != true {
		t.Errorf("enable **不得**清系统自动位（那需要 revive）：%v", out)
	}

	// ④ revive 清自动位
	code, out = compatDo(t, mux, http.MethodPost, "/admin/accounts/"+uid+"/revive", "compat-admin-key", "")
	if code != http.StatusOK {
		t.Fatalf("revive code=%d body=%v", code, out)
	}
	if out["disabled"] != false {
		t.Errorf("revive 应清自动位：%v", out)
	}
	if out["manual_disabled"] != false {
		t.Errorf("revive **不得**动手动位：%v", out)
	}

	// ⑤ 不存在的账号 → 404（不是静默 200）
	code, _ = compatDo(t, mux, http.MethodPost, "/admin/accounts/nope-uid/disable", "compat-admin-key", "")
	if code != http.StatusNotFound {
		t.Errorf("未知账号 code=%d want 404", code)
	}
}

// TestCompatTaskRunCodes 任务触发的状态码词表 + 白名单文案。
func TestCompatTaskRunCodes(t *testing.T) {
	_, _, a, mux := newCompatApp(t, "cp-acct-0004")

	// 未知任务 → 404，且文案必须列出白名单（脚本排错靠它）。
	code, out := compatDo(t, mux, http.MethodPost, "/admin/tasks/nope/run", "compat-admin-key", "")
	if code != http.StatusNotFound {
		t.Fatalf("未知任务 code=%d want 404", code)
	}
	msg, _ := out["error"].(string)
	for _, name := range compatTaskNames {
		if !strings.Contains(msg, name) {
			t.Errorf("404 文案应列出白名单任务 %q：%q", name, msg)
		}
	}

	// 未接线（-no-tasks）→ 503，绝不伪装成"已开始"。
	a.tasks = nil
	code, out = compatDo(t, mux, http.MethodPost, "/admin/tasks/checkin/run", "compat-admin-key", "")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("未接线 code=%d body=%v want 503", code, out)
	}

	// 接到真调度器 → 202 + {"task":..,"status":"started"}（与旧网关逐字一致）。
	a.tasks = newTaskRunner(a.proxy, a.store, t.TempDir())
	code, out = compatDo(t, mux, http.MethodPost, "/admin/tasks/keepalive/run", "compat-admin-key", "")
	if code != http.StatusAccepted {
		t.Fatalf("已接线 code=%d body=%v want 202", code, out)
	}
	if out["task"] != "keepalive" || out["status"] != "started" {
		t.Errorf("响应体形状应与旧网关一致：%v", out)
	}
}

// TestCompatTaskNamesMatchRunnableKinds 两份任务清单必须同集合（防漂移）。
//
// compatTaskNames 决定"老命令还能用哪些名字"，runnableKinds 决定"哪些真能跑"。
// 两者一旦分叉，就会出现「404 文案里列着它，调用却说不认识」这类只有用户能发现的怪事。
func TestCompatTaskNamesMatchRunnableKinds(t *testing.T) {
	if len(compatTaskNames) != len(runnableKinds) {
		t.Fatalf("任务清单长度不一致：compat=%v runnable=%v", compatTaskNames, runnableKinds)
	}
	for _, name := range compatTaskNames {
		if _, ok := runnableKinds[name]; !ok {
			t.Errorf("compat 白名单里的 %q 在 runnableKinds 里不存在", name)
		}
	}
}

// TestCompatStatsResetClearsLiveOnly 统计重置只清进程内实时累计，不动报表数据。
func TestCompatStatsResetClearsLiveOnly(t *testing.T) {
	_, px, _, mux := newCompatApp(t, "cp-acct-0005")
	px.live.record(liveEvent{At: 1, Model: "glm-5.3", OK: true})
	if _, _, ev, _ := px.live.snapshot(liveWindowMinutes); len(ev) == 0 {
		t.Fatal("用例前提：record 后事件流应非空")
	}
	code, out := compatDo(t, mux, http.MethodPost, "/v1/stats/reset", "compat-admin-key", "")
	if code != http.StatusOK {
		t.Fatalf("code=%d want 200", code)
	}
	if out["ok"] != true {
		t.Errorf("响应应为 {ok:true}：%v", out)
	}
	if _, _, ev, _ := px.live.snapshot(liveWindowMinutes); len(ev) != 0 {
		t.Errorf("事件流应被清空，got %d 条", len(ev))
	}
}

// TestCompatStatusRealmTotalsFields /status 的账号数组在停用后如实反映两个位。
func TestCompatStatusRealmTotalsFields(t *testing.T) {
	const uid = "cp-acct-0006"
	_, px, _, mux := newCompatApp(t, uid)
	px.pool.SetManualDisabled(uid, true, "运维摘除")
	code, out := compatDo(t, mux, http.MethodGet, "/status", "compat-admin-key", "")
	if code != http.StatusOK {
		t.Fatalf("code=%d", code)
	}
	arr := out["accounts"].([]any)
	acc := arr[0].(map[string]any)
	if acc["manual_disabled"] != true {
		t.Errorf("停用后 manual_disabled 应为 true：%v", acc)
	}
	if acc["manual_reason"] != "运维摘除" {
		t.Errorf("manual_reason 应透出：%v", acc)
	}
	if fmt.Sprint(out["total"]) != "1" {
		t.Errorf("total=%v want 1", out["total"])
	}
}
