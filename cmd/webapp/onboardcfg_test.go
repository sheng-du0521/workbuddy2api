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
)

// ======================================================================
// 一键接入选配（v0.31.81）
//
// 语义层（取交集 / 默认值回落）单独做成纯函数来测：它依赖的两个全局
// （模型缓存、KV）在单测里要稳定构造很费劲，而这两条规则又最容易写错 ——
// "配了 a,c，但 Key 只能跑 b"必须得到**空集**，不能把 a,c 硬塞给它。
// ======================================================================

func TestIntersectModels(t *testing.T) {
	base := []string{"a", "b", "c"}
	cases := []struct {
		name  string
		allow []string
		want  []string
	}{
		{"全放行", []string{"a", "b", "c"}, []string{"a", "b", "c"}},
		{"部分放行且保持 base 顺序", []string{"c", "a"}, []string{"a", "c"}},
		{"一个都不在 base 里 → 空集（不能硬塞）", []string{"x", "y"}, []string{}},
		{"放行列表为空 → 也是空集（调用方用 configured 区分「不限制」）", nil, []string{}},
	}
	for _, c := range cases {
		got := intersectModels(base, c.allow)
		if len(got) != len(c.want) {
			t.Errorf("%s：得到 %v，期望 %v", c.name, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s：得到 %v，期望 %v", c.name, got, c.want)
				break
			}
		}
	}
}

func TestPickDefault(t *testing.T) {
	models := []string{"a", "b"}
	cases := []struct {
		name string
		def  string
		want string
	}{
		{"配置值在候选里 → 用它", "b", "b"},
		{"配置值不在候选里 → 回落第一项", "wb-group-work", "a"},
		{"没配置 → 保持空（由调用方决定默认）", "", ""},
		{"带空白也算没配置", "   ", ""},
	}
	for _, c := range cases {
		if got := pickDefault(c.def, models); got != c.want {
			t.Errorf("%s：得到 %q，期望 %q", c.name, got, c.want)
		}
	}
	// 候选为空时，配置值原样返回（让前端把它当 placeholder 显示，而不是静默清空）。
	if got := pickDefault("b", nil); got != "b" {
		t.Errorf("候选为空时应保留配置值，得到 %q", got)
	}
}

// TestAdminOnboardConfigRejectsHostGroupDefault 后台保存时拦住"代挂默认=组名"。
//
// 这是 v0.31.81 修的那个线上故障（代挂 Key 接入后一直 404）的**上游拦口**：
// 只要不让它被写进配置，用户就不会踩。
func TestAdminOnboardConfigRejectsHostGroupDefault(t *testing.T) {
	store, a, _, _ := newAuthzApp(t)
	defer store.Close()

	body := strings.NewReader(`{"models":"","default_pool":"wb-group-work","default_host":"wb-group-ch3"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/admin/onboard-config", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.handleAdminSetOnboardConfig(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("代挂默认填组名应 400，得到 %d（%s）", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["code"] != "host_default_cannot_be_group" {
		t.Errorf("code = %v，期望 host_default_cannot_be_group", out["code"])
	}
	// 被拒时不该留下半套配置。
	if v, _ := store.GetKV(kvOnboardDefaultPool); v != "" {
		t.Errorf("被拒时不该写入任何一项，default_pool 却是 %q", v)
	}
}
