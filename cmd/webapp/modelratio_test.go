// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 模型倍率同步的回归测试（v0.31.51，需求一）。
package main

import (
	"errors"
	"testing"

	"workbuddy2api/internal/upstream"
)

// TestModelBadgeOf 上游 tag 的 badge 解析（带颜色 / 不带颜色 / 没有）。
func TestModelBadgeOf(t *testing.T) {
	cases := []struct {
		tags []string
		want string
	}{
		{[]string{"craft", "badge:限时免费:#FF0000"}, "限时免费"},
		{[]string{"badge:新模型"}, "新模型"},
		{[]string{"craft", "vision"}, ""},
		{nil, ""},
	}
	for _, c := range cases {
		if got := modelBadgeOf(c.tags); got != c.want {
			t.Fatalf("badge(%v)=%q，期望 %q", c.tags, got, c.want)
		}
	}
}

// TestModelRatioDrift 口径不一致的判定：只在证据明确时提示。
func TestModelRatioDrift(t *testing.T) {
	if s := modelRatioDrift(true, 0, false); s != "" {
		t.Fatalf("未实测不该报 drift（未知≠异常），得 %q", s)
	}
	if s := modelRatioDrift(true, 0, true); s != "" {
		t.Fatalf("免费且实测 0 不该报 drift，得 %q", s)
	}
	if s := modelRatioDrift(true, 0.5, true); s == "" {
		t.Fatal("免费但实测在扣点必须提示")
	}
	if s := modelRatioDrift(false, 0, true); s == "" {
		t.Fatal("付费但实测为 0 必须提示")
	}
	if s := modelRatioDrift(false, 0.5, true); s != "" {
		t.Fatalf("一致的口径不该报 drift，得 %q", s)
	}
}

// TestSyncModelRatios 首次落库、二次变更记差异 + 发公告、失败不改库。
func TestSyncModelRatios(t *testing.T) {
	store, _, a := newTestApp(t)
	defer store.Close()

	batch := []upstream.ModelInfo{
		{ID: "hy3", Name: "HY3", Credits: "x0.05", Tags: []string{"badge:限时免费:#FF0000"}},
		{ID: "glm-5.3", Name: "GLM", Credits: "x1"},
	}
	a.modelRatioFetch = func() ([]upstream.ModelInfo, error) { return batch, nil }
	// 本地实测成本（计费口径）。
	if err := store.SetKV("model_cost_1k:hy3", "0.02"); err != nil {
		t.Fatalf("SetKV: %v", err)
	}

	res, err := a.syncModelRatios()
	if err != nil {
		t.Fatalf("首次同步: %v", err)
	}
	if res.Total != 2 || res.Added != 2 {
		t.Fatalf("首次应落 2 条，得 total=%d added=%d", res.Total, res.Added)
	}
	r, _ := store.GetModelRatio("hy3")
	if r == nil || !r.OfficialFree || r.OfficialCredits != "x0.05" {
		t.Fatalf("hy3 应记为免费 + x0.05，得 %+v", r)
	}
	if r.MeasuredCost1k != 0.02 {
		t.Fatalf("实测成本应取 kv，得 %v", r.MeasuredCost1k)
	}
	if r.Drift == "" {
		t.Fatal("官方标免费但实测在扣点 → 必须提示 drift")
	}

	// 第二轮：hy3 取消免费 + 倍率变化；glm 不变。
	batch = []upstream.ModelInfo{
		{ID: "hy3", Name: "HY3", Credits: "x0.1", Tags: []string{}},
		{ID: "glm-5.3", Name: "GLM", Credits: "x1"},
	}
	res2, err := a.syncModelRatios()
	if err != nil {
		t.Fatalf("二次同步: %v", err)
	}
	if res2.FreeFlip != 1 || res2.Changed != 1 {
		t.Fatalf("应记 1 次免费状态变化 + 1 次倍率变化，得 %+v", res2)
	}
	if !res2.Announced {
		t.Fatal("免费转付费必须发公告（用户会少付/多付）")
	}
	diffs, _ := store.ListModelRatioDiffs(10)
	if len(diffs) < 3 { // 2 条 new_model + free_switch + credits_change
		t.Fatalf("差异台账应有 ≥3 条，得 %d", len(diffs))
	}
	ann, _ := store.ListAnnouncements(false, 5)
	if len(ann) == 0 {
		t.Fatal("公告表里应有 1 条计费口径变更")
	}

	// 第三轮：上游报错 → 一个字都不许改。
	before, _ := store.GetModelRatio("hy3")
	a.modelRatioFetch = func() ([]upstream.ModelInfo, error) {
		return nil, errors.New("upstream down")
	}
	if _, err := a.syncModelRatios(); err == nil {
		t.Fatal("上游失败应返回错误")
	}
	after, _ := store.GetModelRatio("hy3")
	if before.UpdatedAt != after.UpdatedAt || before.OfficialCredits != after.OfficialCredits {
		t.Fatalf("同步失败却改了库：before=%+v after=%+v", before, after)
	}

	// 第四轮：空列表 → 跳过且不改库（宁可用旧口径）。
	a.modelRatioFetch = func() ([]upstream.ModelInfo, error) { return nil, nil }
	res4, err := a.syncModelRatios()
	if err != nil || res4.Skipped == "" {
		t.Fatalf("空列表应跳过并说明原因，得 %+v err=%v", res4, err)
	}
}

// TestModelRatioRoutes 三个端点存在且要鉴权。
func TestModelRatioRoutes(t *testing.T) {
	_, _, mux, _, memberCookie := eaApp(t)
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/models/ratios"},
		{"GET", "/api/admin/model-ratios"},
		{"POST", "/api/admin/model-ratios/sync"},
	} {
		if code, _ := eaReq(t, mux, c.method, c.path, "", "", ""); code != 401 && code != 403 {
			t.Fatalf("%s %s 未鉴权应 401/403，得 %d", c.method, c.path, code)
		}
	}
	// 普通用户可读倍率（只读），但不能碰管理端。
	if code, _ := eaReq(t, mux, "GET", "/api/models/ratios", "", memberCookie, ""); code != 200 {
		t.Fatalf("普通用户应能读 /api/models/ratios，得 %d", code)
	}
	if code, _ := eaReq(t, mux, "GET", "/api/admin/model-ratios", "", memberCookie, ""); code != 403 {
		t.Fatalf("普通用户不该读管理端口径，得 %d", code)
	}
}
