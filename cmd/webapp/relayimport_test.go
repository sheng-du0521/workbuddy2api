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
// 通道接入：探活结果一键入库 + 单模型倍率（v0.31.88）
//
// 这块的核心不是"能写进去"，而是**幂等与边界**：
//   - 同一批模型导两次，第二次不能把人工调过的单价/倍率冲掉；
//   - 已经在系统模型列表里的模型，不能被导入把官方倍率信息清空；
//   - 非法行只拒那一行，别把整批拖死（运营一次勾几十个，一条坏数据不该全废）。
// ======================================================================

func seedChannel(t *testing.T, store *Store) int64 {
	t.Helper()
	id, err := store.SaveUpstream(&Upstream{
		Name: "relay-import", Kind: upstreamKindOpenAI,
		BaseURL: "https://relay-import.example.com/v1", APIKey: "sk-imp",
		Enabled: true, Priority: 100,
	})
	if err != nil {
		t.Fatalf("SaveUpstream: %v", err)
	}
	return id
}

// TestImportUpstreamModelsWritesBothLists 两个列表都要写：通道映射 + 系统模型列表。
//
// 只写前者 = 能用但运营看不见（也就没法定价）；只写后者 = 看得见但跑不了。
func TestImportUpstreamModelsWritesBothLists(t *testing.T) {
	store, _, _ := newTestApp(t)
	defer store.Close()
	id := seedChannel(t, store)

	res, err := store.ImportUpstreamModels(id, []relayImportItem{
		{Model: "gpt-5.5", CreditPer1k: 12},
		{Model: "kimi-k2.5", CreditPer1k: 3, BillingRatio: 1.5},
	})
	if err != nil {
		t.Fatalf("ImportUpstreamModels: %v", err)
	}
	if len(res.Added) != 2 || len(res.Invalid) != 0 {
		t.Fatalf("应新增 2 条、无非法，得到 %+v", res)
	}
	// ① 通道映射
	ups, err := store.ListUpstreams()
	if err != nil {
		t.Fatalf("ListUpstreams: %v", err)
	}
	var got []string
	var price float64
	for _, u := range ups {
		for _, m := range u.Models {
			got = append(got, m.Model)
			if m.Model == "gpt-5.5" {
				price = m.CreditPer1k
			}
		}
	}
	if len(got) != 2 {
		t.Errorf("通道映射应有 2 条，实际 %v", got)
	}
	if price != 12 {
		t.Errorf("gpt-5.5 单价 = %v，期望 12", price)
	}

	// ② 系统模型列表
	ratios, err := store.ListModelRatios()
	if err != nil {
		t.Fatalf("ListModelRatios: %v", err)
	}
	have := map[string]*modelRatio{}
	for _, r := range ratios {
		have[r.Model] = r
	}
	if have["gpt-5.5"] == nil || have["kimi-k2.5"] == nil {
		t.Fatalf("两个模型都应进入系统模型列表，实际 %v", have)
	}
	if got := have["kimi-k2.5"].BillingRatio; got != 1.5 {
		t.Errorf("显式倍率应写成 1.5，得到 %v", got)
	}
	if got := have["kimi-k2.5"].BillingScope; got != "relay" {
		t.Errorf("通道模型的倍率 scope 必须是 relay（写成 pool 会连带号池同名模型），得到 %q", got)
	}
	if got := have["gpt-5.5"].BillingRatio; got != 0 {
		t.Errorf("未给倍率应保持 0（= 跟随全局，不是免费），得到 %v", got)
	}
}

// TestImportUpstreamModelsIdempotent 重复导入：跳过、不覆盖人工调过的价。
func TestImportUpstreamModelsIdempotent(t *testing.T) {
	store, _, _ := newTestApp(t)
	defer store.Close()
	id := seedChannel(t, store)

	if _, err := store.ImportUpstreamModels(id, []relayImportItem{
		{Model: "m-a", CreditPer1k: 10},
	}); err != nil {
		t.Fatalf("第一次导入: %v", err)
	}
	// 运营手动调价到 25。
	ups, _ := store.ListUpstreams()
	var mid int64
	for _, u := range ups {
		for _, m := range u.Models {
			mid = m.ID
		}
	}
	if _, err := store.SaveUpstreamModel(&UpstreamModel{
		ID: mid, UpstreamID: id, Model: "m-a", CreditPer1k: 25, Enabled: true,
	}); err != nil {
		t.Fatalf("调价: %v", err)
	}

	// 再点一次探活导入（单价传 0，模拟"没填价"）。
	res, err := store.ImportUpstreamModels(id, []relayImportItem{{Model: "m-a"}})
	if err != nil {
		t.Fatalf("第二次导入: %v", err)
	}
	if len(res.Added) != 0 || len(res.Skipped) != 1 {
		t.Errorf("重复导入应全部跳过，得到 %+v", res)
	}
	if len(res.RatiosAdded) != 0 {
		t.Errorf("模型已在目录里，不该重复计入 ratios_added：%v", res.RatiosAdded)
	}
	ups, _ = store.ListUpstreams()
	for _, u := range ups {
		for _, m := range u.Models {
			if m.Model == "m-a" && m.CreditPer1k != 25 {
				t.Errorf("人工调过的单价被覆盖了：%v", m.CreditPer1k)
			}
		}
	}
}

// TestImportUpstreamModelsKeepsOfficialRatios 已存在的模型不能被导入清空官方信息。
//
// 这是 PutModelRatio（upsert）与这里的 DO NOTHING 的关键差别：
// 前者会拿"本次传的空值"覆盖 official_credits / measured_cost_1k。
func TestImportUpstreamModelsKeepsOfficialRatios(t *testing.T) {
	store, _, _ := newTestApp(t)
	defer store.Close()
	id := seedChannel(t, store)

	// 模拟官方同步已经写过这个模型。
	if _, err := store.db.Exec(`INSERT INTO model_ratios
		(model, name, official_credits, official_badge, official_free, measured_cost_1k, measured_known, updated_at)
		VALUES ('glm-5.3','GLM 5.3','1200 credits','免费',1,4.2,1,1)`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := store.ImportUpstreamModels(id, []relayImportItem{{Model: "glm-5.3", CreditPer1k: 8}}); err != nil {
		t.Fatalf("导入: %v", err)
	}
	ratios, _ := store.ListModelRatios()
	for _, r := range ratios {
		if r.Model != "glm-5.3" {
			continue
		}
		if r.OfficialCredits != "1200 credits" || !r.OfficialFree {
			t.Errorf("官方信息被导入冲掉了：%+v", r)
		}
		if r.MeasuredCost1k != 4.2 || !r.MeasuredKnown {
			t.Errorf("实测值被导入冲掉了：%+v", r)
		}
	}
}

// TestImportUpstreamModelsRejectsBadRows 非法行只拒那一行，其余照常导入。
func TestImportUpstreamModelsRejectsBadRows(t *testing.T) {
	store, _, _ := newTestApp(t)
	defer store.Close()
	id := seedChannel(t, store)

	res, err := store.ImportUpstreamModels(id, []relayImportItem{
		{Model: "ok-1", CreditPer1k: 1},
		{Model: "  ", CreditPer1k: 1},                     // 空名
		{Model: "bad-price", CreditPer1k: -1},             // 负价
		{Model: "bad-ratio", BillingRatio: 101},           // 倍率越界
		{Model: strings.Repeat("x", 121), CreditPer1k: 1}, // 超长
	})
	if err != nil {
		t.Fatalf("导入不该因为个别坏行失败：%v", err)
	}
	if len(res.Added) != 1 || res.Added[0] != "ok-1" {
		t.Errorf("应只导入 ok-1，得到 %+v", res.Added)
	}
	if len(res.Invalid) != 4 {
		t.Errorf("应拒 4 行，得到 %d：%v", len(res.Invalid), res.Invalid)
	}
}

// TestImportUpstreamModelsGuards 通道 id 与批量上限。
func TestImportUpstreamModelsGuards(t *testing.T) {
	store, _, _ := newTestApp(t)
	defer store.Close()
	if _, err := store.ImportUpstreamModels(0, []relayImportItem{{Model: "x"}}); err == nil {
		t.Error("没有通道 id 应报错")
	}
	id := seedChannel(t, store)
	if _, err := store.ImportUpstreamModels(id, nil); err == nil {
		t.Error("空清单应报错")
	}
	many := make([]relayImportItem, 201)
	for i := range many {
		many[i] = relayImportItem{Model: "m" + itoa(i+1)}
	}
	if _, err := store.ImportUpstreamModels(id, many); err == nil {
		t.Error("超过 200 条应报错（防一次把整站目录塞进来）")
	}
}

// TestAdminUpstreamImportEndpoint 接口层：通道不存在 → 404；全被拒 → 400 且不假装成功。
func TestAdminUpstreamImportEndpoint(t *testing.T) {
	store, _, a := newTestApp(t)
	defer store.Close()
	id := seedChannel(t, store)

	call := func(body string) (int, map[string]any) {
		req := httptest.NewRequest(http.MethodPost, "/api/admin/upstreams/models/import", strings.NewReader(body))
		rec := httptest.NewRecorder()
		a.handleAdminUpstreamImport(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	code, out := call(`{"id":99999,"models":[{"model":"m"}]}`)
	if code != http.StatusNotFound {
		t.Errorf("不存在的通道应 404，得到 %d（%v）", code, out)
	}

	code, out = call(`{"id":` + itoa(int(id)) + `,"models":[{"model":"","credit_per_1k":1}]}`)
	if code != http.StatusBadRequest || out["code"] != "nothing_imported" {
		t.Errorf("全被拒时应 400 + nothing_imported，得到 %d %v", code, out)
	}

	code, out = call(`{"id":` + itoa(int(id)) + `,"models":[{"model":"m-ok","credit_per_1k":2,"billing_ratio":1.2}]}`)
	if code != http.StatusOK {
		t.Fatalf("正常导入应 200，得到 %d（%v）", code, out)
	}
	if n, _ := out["added"].([]any); len(n) != 1 {
		t.Errorf("应回报 1 条新增，得到 %v", out["added"])
	}
	if n, _ := out["ratios_set"].([]any); len(n) != 1 {
		t.Errorf("应回报 1 条倍率写入，得到 %v", out["ratios_set"])
	}
}

// TestAdminUpstreamsExposesBillingRatio 通道列表要把「计费倍率」带出来（前端那一列）。
func TestAdminUpstreamsExposesBillingRatio(t *testing.T) {
	store, px, a := newTestApp(t)
	defer store.Close()
	id := seedChannel(t, store)
	if _, err := store.ImportUpstreamModels(id, []relayImportItem{
		{Model: "m-rate", CreditPer1k: 5, BillingRatio: 2.5},
	}); err != nil {
		t.Fatalf("导入: %v", err)
	}
	px.ReloadBilling()

	req := httptest.NewRequest(http.MethodGet, "/api/admin/upstreams", nil)
	rec := httptest.NewRecorder()
	a.handleAdminUpstreams(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("列表应 200，得到 %d", rec.Code)
	}
	var out struct {
		Upstreams []struct {
			Models []struct {
				Model        string  `json:"model"`
				BillingRatio float64 `json:"billing_ratio"`
			} `json:"models"`
		} `json:"upstreams"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析: %v", err)
	}
	found := false
	for _, u := range out.Upstreams {
		for _, m := range u.Models {
			if m.Model == "m-rate" {
				found = true
				if m.BillingRatio != 2.5 {
					t.Errorf("billing_ratio = %v，期望 2.5", m.BillingRatio)
				}
			}
		}
	}
	if !found {
		t.Error("列表里没找到刚导入的模型")
	}
}
