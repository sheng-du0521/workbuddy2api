// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// ===========================================================================
// v0.24.5：账号删除（批量接口 + 健康检测阈值自动删除）
//
// 删除**不可逆**（没有回收站），所以这里的用例重点不在"能删掉"，而在**边界**：
//   - 空输入绝不能被解释成"全删"（最危险的默认值）；
//   - 不存在 / 重复的 uid 要如实回报，不能算进 deleted；
//   - 删完必须三处一致：DB 行、池内条目、健康检测快照。
// ===========================================================================

// TestDeleteAccountsBatch 批量删除的 Store 原语。
func TestDeleteAccountsBatch(t *testing.T) {
	store, px, _ := newTestApp(t,
		&Account{UID: "d1", AccessToken: "tok"},
		&Account{UID: "d2", AccessToken: "tok"},
		&Account{UID: "d3", AccessToken: "tok"})
	defer store.Close()
	defer px.Close()

	// 空输入是**无操作**，不是"全删"。
	if n, err := store.DeleteAccounts(nil); err != nil || n != 0 {
		t.Fatalf("空列表应返回 (0,nil)，得 (%d,%v)", n, err)
	}
	if _, err := store.ListAccounts(); err != nil {
		t.Fatal(err)
	}

	// 删 2 个 + 1 个不存在的：只数真正删掉的行。
	n, err := store.DeleteAccounts([]string{"d1", "d2", "no-such-uid"})
	if err != nil {
		t.Fatalf("批量删除失败：%v", err)
	}
	if n != 2 {
		t.Fatalf("应删除 2 行（不存在的 uid 不计入），得 %d", n)
	}
	for _, uid := range []string{"d1", "d2"} {
		acc, err := store.GetAccount(uid)
		if err != nil || acc != nil {
			t.Fatalf("%s 应已删除，得 (%v,%v)", uid, acc, err)
		}
	}
	if acc, _ := store.GetAccount("d3"); acc == nil {
		t.Fatal("d3 不该被删")
	}
}

// TestBatchDeleteHandler 接口层的边界与「三处一致」。
func TestBatchDeleteHandler(t *testing.T) {
	store, px, a := newTestApp(t,
		&Account{UID: "d1", AccessToken: "tok", ConsumeGroup: cgProxy},
		&Account{UID: "d2", AccessToken: "tok", ConsumeGroup: cgProxy},
		&Account{UID: "d3", AccessToken: "tok", ConsumeGroup: cgProxy})
	defer store.Close()
	defer px.Close()
	a.key = "del-admin-key"
	mux := a.routes()

	// healthLast 是**包级全局**，多个用例共用。本用例会写它（验证"删账号同步清快照"），
	// 故必须原样还原 —— 否则残留的快照会污染后续读这个全局的用例。
	// （结构体带 RWMutex，不能整体值拷贝，故按内容复原 + 把 hasResult 翻回原值。）
	prevRes, _, prevOK := healthLast.get()
	t.Cleanup(func() {
		healthLast.set(prevRes, time.Now(), "manual")
		if !prevOK {
			healthLast.mu.Lock()
			healthLast.hasResult = false
			healthLast.mu.Unlock()
		}
	})

	post := func(body string) (*httptest.ResponseRecorder, map[string]any) {
		req := httptest.NewRequest(http.MethodPost, "/api/admin/accounts/batch-delete", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Admin-Key", "del-admin-key")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec, out
	}

	// ① 空输入 → 400（**绝不能**默认全删）。
	rec, out := post(`{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("空输入应 400，得 %d（body=%s）", rec.Code, rec.Body.String())
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "未指定") {
		t.Fatalf("错误文案应说明未指定账号，得 %q", msg)
	}
	rec, _ = post(`{"uids":["","  "]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("全空白 uid 应 400，得 %d", rec.Code)
	}

	// ② 健康快照里先放 2 条，删除后必须一并剔除（否则界面留着已删号的旧结论）。
	// 放三条：d1/d2 会被删，d3 不会 —— 用来验证是**按 uid 精确剔除**，
	// 而不是"一刀清空"（清空同样能让总数变 0，但那是错的）。
	healthLast.set([]*HealthCheckResult{{UID: "d1"}, {UID: "d3"}, {UID: "d2"}}, time.Now(), "manual")

	// ③ 删 2 个 + 1 个不存在的。
	rec, out = post(`{"uids":["d1","d2","ghost"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，得 %d（body=%s）", rec.Code, rec.Body.String())
	}
	if out["deleted"].(float64) != 2 || out["missing"].(float64) != 1 {
		t.Fatalf("应 deleted=2 missing=1，得 %v", out)
	}
	// 三处一致：DB 行 / 池内条目 / 健康快照。
	for _, uid := range []string{"d1", "d2"} {
		if acc, _ := store.GetAccount(uid); acc != nil {
			t.Fatalf("%s 的 DB 行应已删除", uid)
		}
		if _, inPool := a.proxy.pool.Status(uid); inPool {
			t.Fatalf("%s 应已从池内移除", uid)
		}
	}
	left, sum, _ := healthLast.get()
	if sum.Total != 1 {
		t.Fatalf("健康快照应只剩 1 条，得 %d", sum.Total)
	}
	if len(left) != 1 || left[0].UID != "d3" {
		t.Fatalf("剩下的应是未被删除的 d3，得 %+v", left)
	}
	if acc, _ := store.GetAccount("d3"); acc == nil {
		t.Fatal("未选中的 d3 不该被删")
	}
}

// TestRequestIllegalThresholdDeletes 11140 连续达阈值 → **删除**（v0.24.5 默认口径）。
func TestRequestIllegalThresholdDeletes(t *testing.T) {
	store, px, a := newTestApp(t, &Account{UID: "b1", AccessToken: "tok", ConsumeGroup: cgProxy})
	defer store.Close()
	defer px.Close()

	cause := &upstream.Error{Kind: upstream.ErrAccountFault, Status: 403,
		Msg: "code 11140 request illegal", Display: "账号存在异常行为，已被限制使用"}

	// 前两次：只累计 + 软冷却，账号仍在（可逆）。
	for i := 1; i <= 2; i++ {
		acts := a.applyHealthDisposition("b1", hcCodeRequestIllegal, cause, true)
		if acc, _ := store.GetAccount("b1"); acc == nil {
			t.Fatalf("第 %d 次不该删除（阈值 %d），动作=%v", i, pool.AccountFaultThreshold(), acts)
		}
	}
	// 第 3 次：达阈值 → 删除。
	acts := a.applyHealthDisposition("b1", hcCodeRequestIllegal, cause, true)
	if len(acts) == 0 || acts[len(acts)-1] != hcActDeleted {
		t.Fatalf("第 3 次应含 %s 动作，得 %v", hcActDeleted, acts)
	}
	if acc, _ := store.GetAccount("b1"); acc != nil {
		t.Fatal("达阈值后 DB 行应已删除")
	}
	if _, inPool := a.proxy.pool.Status("b1"); inPool {
		t.Fatal("达阈值后应已从池内移除")
	}
}

// TestRequestIllegalThresholdKeepsWhenDeleteOff kv `pool_delete_on_11140=0` 时退回
// 「停用 + 归档」这条**可逆**路径 —— 瞬时风控窗口会成批造成失败，必须留回退开关。
func TestRequestIllegalThresholdKeepsWhenDeleteOff(t *testing.T) {
	store, px, a := newTestApp(t, &Account{UID: "b2", AccessToken: "tok", ConsumeGroup: cgProxy})
	defer store.Close()
	defer px.Close()
	if err := store.SetKV(kvPoolDeleteOn11140, "0"); err != nil {
		t.Fatal(err)
	}

	cause := &upstream.Error{Kind: upstream.ErrAccountFault, Status: 403, Msg: "code 11140 request illegal"}
	var acts []string
	for i := 0; i < 3; i++ {
		acts = a.applyHealthDisposition("b2", hcCodeRequestIllegal, cause, true)
	}
	if containsStr(acts, hcActDeleted) {
		t.Fatalf("开关关闭时不该删除，动作=%v", acts)
	}
	if acc, _ := store.GetAccount("b2"); acc == nil {
		t.Fatal("开关关闭时账号应保留（停用而非删除）")
	}
	if st, _ := a.proxy.pool.Status("b2"); !st.Disabled {
		t.Fatalf("开关关闭时应已停用，状态=%+v", st)
	}
}

// TestRequestIllegalObservationalCallNeverDeletes 观测型调用（刷新积分 / 导入后刷余额）
// 即使 kv 开关处于"会删"的默认口径，也**绝不删除**（v0.24.8）。
//
// 这是「刷分不该顺手删号」那条边界的回归用例：这两条路径复用了检测批（同一个出站请求
// 既取回权威余额、又判定账号是否可用），但它们的语义是**观测**。修复前，点一次
// 「刷新全部积分」或「导入账号」可能连带来一批**不可逆**删除，而且删除是异步的、
// 界面上没有任何确认 —— 用户点的是"刷新余额"，得到的是"账号没了"。
func TestRequestIllegalObservationalCallNeverDeletes(t *testing.T) {
	store, px, a := newTestApp(t, &Account{UID: "b3", AccessToken: "tok", ConsumeGroup: cgProxy})
	defer store.Close()
	defer px.Close()

	// 前置：确认开关是"会删"的默认口径，否则本用例测不出东西。
	if !a.poolDeleteOn11140() {
		t.Fatal("前置失败：pool_delete_on_11140 应为默认开启（即默认可删）")
	}

	cause := &upstream.Error{Kind: upstream.ErrAccountFault, Status: 403, Msg: "code 11140 request illegal"}
	var acts []string
	for i := 0; i < 3; i++ {
		acts = a.applyHealthDisposition("b3", hcCodeRequestIllegal, cause, false) // allowDelete=false
	}
	if containsStr(acts, hcActDeleted) {
		t.Fatalf("观测型调用（allowDelete=false）绝不该删除，动作=%v", acts)
	}
	if acc, _ := store.GetAccount("b3"); acc == nil {
		t.Fatal("观测型调用后账号必须还在（只能停用，不能删）")
	}
	if st, _ := a.proxy.pool.Status("b3"); !st.Disabled {
		t.Fatalf("达阈值仍应停用（可逆处置照常执行），状态=%+v", st)
	}
}

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
