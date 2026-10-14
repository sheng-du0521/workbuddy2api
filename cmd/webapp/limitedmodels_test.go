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
)

// 本文件锁 v0.26.4 的**模型级限额可见性**。
//
// 用户口径：「6004 的冷却是针对对应模型的，其他模型调用正常接」。
// 行为正确还不够 —— 界面必须能**看出**到底是哪个模型被限、何时恢复，
// 否则运维无法回答"这个号现在能不能接某个模型"（只给「限模×1」等于没回答）。

// TestLimitedModelsOfSortedAndBounded 视图行按模型名排序、带上时间与原因。
func TestLimitedModelsOfSortedAndBounded(t *testing.T) {
	now := time.Now()
	rows := []pool.RateLimitedModel{
		{Model: "c-model", Until: now.Add(time.Hour), ResetAt: now.Add(2 * time.Hour), Reason: "6004 model rate limit [reset=body-text]"},
		{Model: "a-model", Until: now.Add(30 * time.Minute), ResetAt: now.Add(30 * time.Minute), Reason: "6004 model rate limit [reset=body-text]"},
		{Model: "b-model", Until: now.Add(45 * time.Minute), Reason: "6004 model rate limit [reset=none→退避]"},
	}

	got := limitedModelsOf(rows)
	if len(got) != 3 {
		t.Fatalf("行数 = %d, want 3", len(got))
	}
	// 排序确定：map 遍历无序，不排序会让同一份状态每次刷新顺序都变。
	for i, want := range []string{"a-model", "b-model", "c-model"} {
		if got[i].Model != want {
			t.Errorf("第 %d 行 = %q, want %q（应按模型名排序）", i, got[i].Model, want)
		}
	}
	if got[0].Until == 0 || got[0].ResetAt == 0 {
		t.Errorf("时间字段应带上：%+v", got[0])
	}
	if !strings.Contains(got[0].Reason, "body-text") {
		t.Errorf("reason 应保留（它说明时长是上游给的还是本地估的）：%q", got[0].Reason)
	}
	// b-model 上游没给时间 → ResetAt 留零（界面据此显示"本地估"）。
	if got[1].ResetAt != 0 || got[1].Until == 0 {
		t.Errorf("无上游重置时刻时 ResetAt 应为 0：%+v", got[1])
	}
}

// TestLimitedModelsOfCapsAtEight 超量截断，且空输入返回 nil。
func TestLimitedModelsOfCapsAtEight(t *testing.T) {
	now := time.Now()
	rows := make([]pool.RateLimitedModel, 0, 12)
	for i := 0; i < 12; i++ {
		rows = append(rows, pool.RateLimitedModel{
			Model: string(rune('a'+i)) + "-model", Until: now.Add(time.Hour),
		})
	}
	if got := limitedModelsOf(rows); len(got) != 8 {
		t.Errorf("应截断到 8 个，得 %d", len(got))
	}
	// 空输入 → nil（omitempty 才不输出空数组）。
	if got := limitedModelsOf(nil); got != nil {
		t.Errorf("空输入应返回 nil，得 %+v", got)
	}
}

// accountRowJSON 账号列表响应里我们关心的那几个字段。
type accountRowJSON struct {
	UID             string `json:"uid"`
	Cooling         bool   `json:"cooling"`
	RateLimitedMods int    `json:"rate_limited_models"`
	LimitedModels   []struct {
		Model   string `json:"model"`
		Until   int64  `json:"until"`
		ResetAt int64  `json:"reset_at"`
		Reason  string `json:"reason"`
	} `json:"limited_models"`
}

// TestAccountViewExposesLimitedModels 账号列表接口必须把「被限的具体模型」带出去。
//
// 这是端到端的那一环：池里有正确状态、视图不带出去，界面照样看不见。
func TestAccountViewExposesLimitedModels(t *testing.T) {
	store, px, a := newTestApp(t, &Account{UID: "m1", Nickname: "限模号", AccessToken: "at"})
	defer store.Close()
	defer px.Close()
	_ = px.SyncAccounts()

	// 造一个模型级限额（新实现的口径：只写模型表，不写账号级 until）。
	px.pool.CooldownSoftForModel("m1", time.Minute, time.Now().Add(30*time.Minute),
		"glm-5.3", "6004 model rate limit [reset=body-text]")

	req := httptest.NewRequest(http.MethodGet, "/api/admin/accounts", nil)
	w := httptest.NewRecorder()
	a.handleListAccounts(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("HTTP %d：%s", w.Code, w.Body.String())
	}
	var out struct {
		Accounts []accountRowJSON `json:"accounts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析响应失败：%v", err)
	}
	var got *accountRowJSON
	for i := range out.Accounts {
		if out.Accounts[i].UID == "m1" {
			got = &out.Accounts[i]
			break
		}
	}
	if got == nil {
		t.Fatal("响应里没有 m1")
	}
	// 账号级不得冷却（这就是「其他模型正常接」的判据）。
	if got.Cooling {
		t.Errorf("模型级限额不得表现为账号级冷却：%+v", got)
	}
	if got.RateLimitedMods != 1 {
		t.Errorf("rate_limited_models = %d, want 1", got.RateLimitedMods)
	}
	if len(got.LimitedModels) != 1 || got.LimitedModels[0].Model != "glm-5.3" {
		t.Fatalf("limited_models 未透出具体模型：%+v", got.LimitedModels)
	}
	if got.LimitedModels[0].ResetAt == 0 || got.LimitedModels[0].Until == 0 {
		t.Errorf("应带上截止与上游重置时刻：%+v", got.LimitedModels[0])
	}
	if !strings.Contains(got.LimitedModels[0].Reason, "reset=body-text") {
		t.Errorf("reason 应带上重置来源：%q", got.LimitedModels[0].Reason)
	}
}
