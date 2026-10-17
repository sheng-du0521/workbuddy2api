// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDefaultEgressGuardRejectsUserLine 全局默认出口不能指向用户线路。
//
// 这条护栏守的是"唯一不经逐个指定就生效"的那个口子：一旦默认指向某个用户在
// 控制台拉起的线路，全平台未指定出口的号都会从这个人的家庭宽带走，并与他的号
// 落到同一个公网 IP 上（2026-10-17 生产现场：38 个号全在 39.91.80.16）。
func TestDefaultEgressGuardRejectsUserLine(t *testing.T) {
	store, _, a := newTestApp(t)
	defer store.Close()

	userURL := "http://127.0.0.1:21160"
	platURL := "http://127.0.0.1:11080"
	for _, p := range []*EgressProxy{
		{ID: "u1", Name: "2541601142 的出口", URL: userURL, Enabled: true, OwnerUserID: 9916},
		{ID: "p1", Name: "家里-联通住宅IP", URL: platURL, Enabled: true, OwnerUserID: 0},
	} {
		if err := store.UpsertEgressProxy(p); err != nil {
			t.Fatalf("UpsertEgressProxy: %v", err)
		}
	}

	if msg := a.defaultEgressGuard(userURL); msg == "" {
		t.Fatal("用户线路必须被拒绝作为全局默认出口")
	} else if !strings.Contains(msg, "9916") || !strings.Contains(msg, "分配") {
		t.Fatalf("拒绝原因应点名用户并给出正确做法（显式分配），得 %q", msg)
	}
	if msg := a.defaultEgressGuard(platURL); msg != "" {
		t.Fatalf("平台线路应放行，得 %q", msg)
	}
	if msg := a.defaultEgressGuard(""); msg != "" {
		t.Fatalf("空（直连）应放行，得 %q", msg)
	}
	// 不在池里的手填地址按平台处理：否则管理员无法把自己刚填的商业代理设为默认。
	if msg := a.defaultEgressGuard("http://1.2.3.4:8080"); msg != "" {
		t.Fatalf("池外手填地址应放行，得 %q", msg)
	}
}

// TestDefaultRepair 一键修复：历史配置里的"默认指向用户线路"能被改回平台线路。
func TestDefaultRepair(t *testing.T) {
	store, _, a := newTestApp(t)
	defer store.Close()
	userURL := "http://127.0.0.1:21160"
	platURL := "http://127.0.0.1:11080"
	for _, p := range []*EgressProxy{
		{ID: "u1", Name: "用户出口", URL: userURL, Enabled: true, OwnerUserID: 9916},
		{ID: "p1", Name: "平台出口", URL: platURL, Enabled: true, OwnerUserID: 0, LastOK: true, LastIP: "39.91.80.16"},
	} {
		if err := store.UpsertEgressProxy(p); err != nil {
			t.Fatalf("UpsertEgressProxy: %v", err)
		}
	}
	_ = store.SetKV(kvEgressDefault, userURL)

	repair := func(mode string) *httptest.ResponseRecorder {
		body := strings.NewReader(`{"mode":"` + mode + `"}`)
		req := httptest.NewRequest(http.MethodPost, "/api/admin/egress/default/repair", body)
		w := httptest.NewRecorder()
		a.handleEgressDefaultRepair(w, req)
		return w
	}

	w := repair("platform")
	if w.Code != http.StatusOK {
		t.Fatalf("修复应成功，得 %d：%s", w.Code, w.Body.String())
	}
	if got, _ := store.GetKV(kvEgressDefault); got != platURL {
		t.Fatalf("默认出口应改回平台线路 %s，得 %q", platURL, got)
	}

	// mode=none → 清空（改回直连）。这是"池里只有用户线路"时唯一安全的出口。
	if w := repair("none"); w.Code != http.StatusOK {
		t.Fatalf("清空应成功，得 %d：%s", w.Code, w.Body.String())
	}
	if got, _ := store.GetKV(kvEgressDefault); got != "" {
		t.Fatalf("默认出口应被清空，得 %q", got)
	}

	// 非法 mode 必须拒绝，而不是当成 platform 静默执行 ——
	// 一次误触发就把全平台的出口改了，代价太大。
	if w := repair("whatever"); w.Code != http.StatusBadRequest {
		t.Fatalf("非法 mode 应 400，得 %d", w.Code)
	}
}
