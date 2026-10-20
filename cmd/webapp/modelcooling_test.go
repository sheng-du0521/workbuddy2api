// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 用户端「模型冷却」视图的验收测试（v0.31.55）。
//
// 这条需求的难点不是"画一张卡"，而是**口径**：
//
//	· 冷却必须按模型聚合（用户问的是"这个模型还能不能用"，不是"哪个号在冷却"）；
//	· full / partial 必须与生产选号同源（`AvailableUIDsForModel`），否则页面说"能用"
//	  而转发必然失败 —— 那比不显示更糟；
//	· 不能把号池结构透给用户（uid / 账号数 / 内部错误码都不出接口）。
package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func mcAccounts() []*Account {
	return []*Account{
		{UID: "mc-a", Nickname: "甲", Realm: "cn", AccessToken: "ta", RefreshToken: "ra",
			ExpiresAt: time.Now().Add(time.Hour).Unix(), PoolState: "", ConsumeGroup: cgProxy},
		{UID: "mc-b", Nickname: "乙", Realm: "cn", AccessToken: "tb", RefreshToken: "rb",
			ExpiresAt: time.Now().Add(time.Hour).Unix(), PoolState: "", ConsumeGroup: cgProxy},
	}
}

// TestModelCoolingViewAggregates 聚合口径：无冷却 / 部分 / 全部 / 原因归一。
func TestModelCoolingViewAggregates(t *testing.T) {
	store, px, a := newTestApp(t, mcAccounts()...)
	defer store.Close()

	// ① 无冷却：一个模型都不该出现在列表里（否则页面永远在报"有冷却"）。
	v := a.buildModelCoolingView()
	if v.Summary.CoolingModels != 0 || len(v.Models) != 0 {
		t.Fatalf("无冷却时不该有模型条目，得 %+v", v)
	}

	// ② 只挡一个账号 → partial（另一个号还能服务）。
	px.pool.CooldownSoftForModel("mc-a", time.Minute, time.Now().Add(time.Minute),
		"glm-x", "6004 限额（上游重置 12:00）")
	v = a.buildModelCoolingView()
	if len(v.Models) != 1 {
		t.Fatalf("应有 1 个冷却模型，得 %d", len(v.Models))
	}
	it := v.Models[0]
	if it.Model != "glm-x" || it.State != "partial" {
		t.Fatalf("应 glm-x/partial，得 %+v", it)
	}
	if it.ReasonLabel != "上游限流" {
		t.Fatalf("6004 应归一为「上游限流」，得 %q", it.ReasonLabel)
	}
	if it.RetryAfterSec <= 0 || it.RetryAfterSec > 120 {
		t.Fatalf("剩余秒数应在 (0,120]，得 %d", it.RetryAfterSec)
	}
	if v.Summary.FullModels != 0 {
		t.Fatalf("partial 不该计入 full，得 %d", v.Summary.FullModels)
	}

	// ③ 两个账号都挡 → full（此刻请求必失败）。
	px.pool.CooldownSoftForModel("mc-b", time.Minute, time.Now().Add(time.Minute),
		"glm-x", "6004 限额")
	v = a.buildModelCoolingView()
	if len(v.Models) != 1 || v.Models[0].State != "full" {
		t.Fatalf("全部账号冷却应为 full，得 %+v", v.Models)
	}
	if v.Summary.FullModels != 1 {
		t.Fatalf("full_models 应为 1，得 %d", v.Summary.FullModels)
	}

	// ④ 11102 归一成「模型不可用」—— 与限流是**不同处置**（等没有用，得换模型）。
	px.pool.BlockModelBackoff("mc-a", "m-gone", "11102 该后端无此模型")
	v = a.buildModelCoolingView()
	var gone *modelCoolingItem
	for i := range v.Models {
		if v.Models[i].Model == "m-gone" {
			gone = &v.Models[i]
		}
	}
	if gone == nil {
		t.Fatalf("m-gone 应出现在冷却列表，得 %+v", v.Models)
	}
	if gone.ReasonLabel != "该模型在当前账号上不可用" {
		t.Fatalf("11102 应归一为「该模型在当前账号上不可用」，得 %q", gone.ReasonLabel)
	}
}

// TestModelCoolingViewHidesPoolStructure 用户端视图不得泄露号池结构。
//
// 这是产品边界而不是洁癖：uid 与账号数会暴露"平台有多少号、哪个号被限"，
// 而用户需要的只是"这个模型现在能不能用"。
func TestModelCoolingViewHidesPoolStructure(t *testing.T) {
	store, px, a := newTestApp(t, mcAccounts()...)
	defer store.Close()
	px.pool.CooldownSoftForModel("mc-a", time.Minute, time.Now().Add(time.Minute), "glm-x", "6004 限额")

	raw, err := json.Marshal(a.buildModelCoolingView())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(raw)
	for _, bad := range []string{"mc-a", "mc-b", "uid", "6004", "11102", "accounts_cooling"} {
		if strings.Contains(s, bad) {
			t.Fatalf("用户端视图不该包含 %q：%s", bad, s)
		}
	}
}

// TestModelCoolingEndpointAuth 接口：登录可读，未登录拒绝。
func TestModelCoolingEndpointAuth(t *testing.T) {
	_, a, mux, _, memberCookie := eaApp(t)
	_ = a

	if code, _ := eaReq(t, mux, "GET", "/api/models/cooling", "", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("未登录应 401，得 %d", code)
	}
	code, body := eaReq(t, mux, "GET", "/api/models/cooling", "", memberCookie, "")
	if code != http.StatusOK {
		t.Fatalf("登录用户应 200，得 %d %s", code, body)
	}
	d := eaJSON(t, body)
	if _, has := d["models"]; !has {
		t.Fatalf("响应必须含 models 字段（前端据此渲染），得 %.200s", body)
	}
	if _, has := d["summary"]; !has {
		t.Fatalf("响应必须含 summary（顶部汇总），得 %.200s", body)
	}
}

// TestCoolingRespectsSelectionScope 冷却视图必须与**真实选号规则**一致（审查 F11）。
//
// 修复前：卡片只问 pool.AvailableUIDsForModel（"池里有没有健康账号"），
// 而实际选号还要过白名单 / 消耗分组 / 出口健康。
// 于是"唯一允许的号在冷却、另一个号空闲但不在白名单"时，卡片显示 partial，
// 用户以为还能用，实际每次请求都选不出账号 —— 比不显示更糟。
func TestCoolingRespectsSelectionScope(t *testing.T) {
	store, px, a := newTestApp(t, mcAccounts()...)
	defer store.Close()

	// 白名单只放 mc-a：mc-b 健康，但不在可用范围内。
	if err := store.SetKV(kvProxyAccounts, "mc-a"); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	px.ReloadSelection()
	// mc-a 对该模型进入冷却 → 此刻一个可选账号都没有。
	px.pool.CooldownSoftForModel("mc-a", time.Minute, time.Now().Add(time.Minute),
		"glm-scope", "6004 限额（上游重置 12:00）")

	v := a.buildModelCoolingView()
	if len(v.Models) != 1 {
		t.Fatalf("应有 1 个冷却模型，得 %d", len(v.Models))
	}
	if v.Models[0].State != "full" {
		t.Fatalf("唯一允许的号在冷却、另一个不在白名单 → 应判 full，得 %q（假余量）", v.Models[0].State)
	}
	if v.Summary.FullModels != 1 {
		t.Fatalf("summary.full_models 应为 1，得 %d", v.Summary.FullModels)
	}

	// 放开白名单（mc-b 可用）→ 同一条冷却记录变成 partial。
	if err := store.SetKV(kvProxyAccounts, ""); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	px.ReloadSelection()
	v = a.buildModelCoolingView()
	if len(v.Models) != 1 || v.Models[0].State != "partial" {
		t.Fatalf("放开白名单后应回到 partial，得 %+v", v.Models)
	}
}
