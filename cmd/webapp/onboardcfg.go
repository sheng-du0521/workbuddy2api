// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 一键接入的「模型选配」（v0.31.82，需求 1）。
//
// # 为什么需要
//
// 此前一键接入把整份模型目录丢给用户自己挑：池计费 Key 看到全部模型，
// 代挂 Key 看到自己账号能跑的模型，默认值还统一填 `wb-group-work` ——
// 对代挂 Key 那是必然 404（组名展开的是号池模型，见 v0.31.81 的修复）。
//
// 运营的实际需求是："一键接入只给这几个模型"（比如只放行主力几个、
// 或者在灰度期只开一个），并且**两类 Key 分别配默认值**。
//
// # 语义
//
//	onboard_models          允许出现在接入候选里的模型（逗号分隔）；**空 = 不限制**
//	onboard_default_pool    池计费 Key 的默认模型；空 = wb-group-work（整个号池）
//	onboard_default_host    代挂自用 Key 的默认模型；空 = 留空让用户选（不能填组名）
//
// 关键：选配是**取交集**，不是替换 ——
// 配了 `glm-5.3,kimi-k2.5` 之后，一个代挂 Key 如果只能跑 glm-5.3，
// 它看到的候选就只有 glm-5.3（不会出现它跑不了的 kimi）。
// 反过来，选配**不影响** `/v1/models` 与测试对话页：那是"实际能用什么"，
// 这里管的是"接入时推荐填什么" —— 两件事必须分开，否则用户会以为模型被删了。
package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

const (
	kvOnboardModels      = "onboard_models"
	kvOnboardDefaultPool = "onboard_default_pool"
	kvOnboardDefaultHost = "onboard_default_host"
)

// onboardOptionsResp 接入弹窗要的三样东西：候选、默认值、为什么受限。
type onboardOptionsResp struct {
	Models     []string `json:"models"`
	Default    string   `json:"default_model"`
	Kind       string   `json:"kind"`
	Restricted bool     `json:"restricted"`
	Note       string   `json:"note,omitempty"`
}

// allModelIDs 平台可见的全部模型名（号池目录 ∪ 通道映射，去重排序）。
func (a *app) allModelIDs() []string {
	if !modelsCacheFresh() {
		a.refreshModelsCache()
	}
	seen := map[string]bool{}
	out := make([]string, 0, 32)
	for _, id := range cachedModelIDs() {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	if relayIDs, err := a.store.relayModelIDs(); err == nil {
		for _, id := range relayIDs {
			if id != "" && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	sort.Strings(out)
	return out
}

// onboardAllowList 后台配置的允许模型（空切片 = 不限制）。
//
// 只保留**当前仍存在**的模型：模型下线后配置里留着名字不该继续出现在候选里
// （否则用户选一个已经没了的模型，又是 404）。
func (a *app) onboardAllowList() (allow []string, configured bool) {
	raw, _ := a.store.GetKV(kvOnboardModels)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, false
	}
	all := map[string]bool{}
	for _, m := range a.allModelIDs() {
		all[m] = true
	}
	seen := map[string]bool{}
	for _, m := range splitGroups(raw) { // 同一套逗号切分（去空白、去空项）
		if all[m] && !seen[m] {
			seen[m] = true
			allow = append(allow, m)
		}
	}
	return allow, true
}

// handleOnboardOptions GET /api/onboard/options?key_id=N
//
// 给一键接入弹窗用：候选模型 + 该类 Key 的默认模型。
func (a *app) handleOnboardOptions(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	id, err := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("key_id")), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少 key_id"})
		return
	}
	// 归属校验下推 SQL（跨用户 404），与 reveal / onboard 同一纪律。
	k, err := a.store.GetAPIKey(u.ID, id)
	if err != nil || k == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "密钥不存在"})
		return
	}
	if k.Status != StatusActive {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "该密钥已停用"})
		return
	}

	// ① 该 Key 自己"能看见什么"（与 /api/keys/{id}/models 同源）。
	base := a.keyModelIDs(k)
	// ② 与后台选配取交集（选配为空 = 不限制）。
	allow, configured := a.onboardAllowList()
	models := base
	note := ""
	if configured {
		models = intersectModels(base, allow)
		note = "候选已按后台「一键接入选配」过滤"
		if len(models) == 0 {
			note = "后台只放行了 " + strings.Join(allow, "、") +
				"，但这把密钥一个都用不了 —— 直接手填模型名，或找管理员调整选配"
		}
	}
	// ③ 默认值：按 Key 类型取配置；不在候选里就回落到第一个候选。
	def := ""
	if k.Kind == KeyKindHost {
		def, _ = a.store.GetKV(kvOnboardDefaultHost)
	} else {
		def, _ = a.store.GetKV(kvOnboardDefaultPool)
		if strings.TrimSpace(def) == "" {
			def = groupVirtualModel(groupWorkID) // 池 Key 的既有默认：整个号池
		}
	}
	def = pickDefault(def, models)
	resp := onboardOptionsResp{
		Models: models, Default: def, Kind: k.Kind, Restricted: configured, Note: note,
	}
	if k.Kind == KeyKindHost {
		// 代挂 Key 的任何默认值都不能是组名（v0.31.81 的根因）。
		if strings.HasPrefix(resp.Default, groupModelPrefix) {
			resp.Default = ""
		}
		if resp.Default == "" && len(models) > 0 {
			resp.Default = models[0]
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// hasModel 判断模型名是否在列表里。
func hasModel(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// intersectModels 选配与"该 Key 自己能用什么"取交集（保持 base 的顺序）。
//
// 抽成纯函数是为了能单测：它依赖的两个全局（模型缓存、KV）在测试里很难稳定构造，
// 而这层的语义又最容易写错（配了 a,c 但 Key 只能跑 b → 必须得到空集，而不是 a,c）。
func intersectModels(base, allow []string) []string {
	set := make(map[string]bool, len(allow))
	for _, m := range allow {
		set[m] = true
	}
	out := make([]string, 0, len(allow))
	for _, m := range base {
		if set[m] {
			out = append(out, m)
		}
	}
	return out
}

// pickDefault 默认值的回落规则：配置值不在候选里 → 取第一项；配置为空 → 保持空。
func pickDefault(def string, models []string) string {
	def = strings.TrimSpace(def)
	if def == "" {
		return ""
	}
	if len(models) > 0 && !hasModel(models, def) {
		return models[0]
	}
	return def
}

// ---------------------------------------------------------------- 管理端

// handleAdminOnboardConfig GET /api/admin/onboard-config
//
// 回显配置 + 全量模型（供勾选）。all 是"平台现在有什么"，
// 与"选了哪些"分开给：前端不必自己再拼一次目录。
func (a *app) handleAdminOnboardConfig(w http.ResponseWriter, r *http.Request) {
	raw, _ := a.store.GetKV(kvOnboardModels)
	defPool, _ := a.store.GetKV(kvOnboardDefaultPool)
	defHost, _ := a.store.GetKV(kvOnboardDefaultHost)
	allow, configured := a.onboardAllowList()
	writeJSON(w, http.StatusOK, map[string]any{
		"models":       allow, // 已配置的（已剔除下线模型）
		"raw":          raw,   // 原文：让运维看见"配过但已下线"的名字
		"configured":   configured,
		"default_pool": defPool,
		"default_host": defHost,
		"all":          a.allModelIDs(),
	})
}

// handleAdminSetOnboardConfig POST /api/admin/onboard-config
//
//	{ "models": "glm-5.3,kimi-k2.5", "default_pool": "wb-group-work", "default_host": "glm-5.3" }
//
// 空字符串 = 恢复"不限制"。不做严格校验（模型可能还没被观测到），
// 但会**回显实际生效的清单** —— 运维能立刻看到"我配的名字里哪几个不存在"。
func (a *app) handleAdminSetOnboardConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Models      string `json:"models"`
		DefaultPool string `json:"default_pool"`
		DefaultHost string `json:"default_host"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	// 代挂默认值不许是组名：那会让每一把代挂 Key 接入后必然 404。
	if strings.HasPrefix(strings.TrimSpace(req.DefaultHost), groupModelPrefix) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "代挂自用的默认模型不能是模型组（组名对应号池模型，代挂 Key 会 404）",
			"code":  "host_default_cannot_be_group",
		})
		return
	}
	if err := a.store.SetKV(kvOnboardModels, strings.Join(splitGroups(req.Models), ",")); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if err := a.store.SetKV(kvOnboardDefaultPool, strings.TrimSpace(req.DefaultPool)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if err := a.store.SetKV(kvOnboardDefaultHost, strings.TrimSpace(req.DefaultHost)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	allow, configured := a.onboardAllowList()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "models": allow, "configured": configured,
		"all": a.allModelIDs(),
	})
}

// keyModelIDs 一把 Key 能看见的具体模型（不含组名），与 /api/keys/{id}/models 同源。
//
// 与 keymodels.go 的区别：那里还要拼组名给人看（下拉里要能选 wb-group-work），
// 这里只关心"具体模型"，因为接入选配配的是具体模型。
func (a *app) keyModelIDs(k *APIKey) []string {
	if k == nil {
		return nil
	}
	if !modelsCacheFresh() {
		a.refreshModelsCache()
	}
	seen := map[string]bool{}
	out := make([]string, 0, 32)
	add := func(list []string) {
		for _, id := range list {
			if id != "" && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	add(cachedModelIDs())
	if relayIDs, err := a.store.relayModelIDs(); err == nil {
		add(relayIDs)
	}
	sort.Strings(out)
	return out
}
