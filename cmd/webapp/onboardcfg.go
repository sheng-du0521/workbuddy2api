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
	kvOnboardModels      = "onboard_models"       // 号池官组允许的模型（空 = 不限制）
	kvOnboardModelsHost  = "onboard_models_host"  // 自挂官组允许的模型（空 = 不限制）
	kvOnboardDefaultPool = "onboard_default_pool" // 号池官组默认模型（空 = wb-group-work）
	kvOnboardDefaultHost = "onboard_default_host" // 自挂官组默认模型（空 = wb-group-host）
)

// onboardOptionsResp 接入弹窗要的东西：候选、默认值、属于哪个官组、为什么受限。
type onboardOptionsResp struct {
	Models     []string `json:"models"`
	Default    string   `json:"default_model"`
	Kind       string   `json:"kind"`
	Restricted bool     `json:"restricted"`
	Note       string   `json:"note,omitempty"`
	// GroupName / GroupLabel（v0.31.82）：这一类对应的 work官组 虚拟名与中文名，
	// 前端据此显示「work官组 · 号池官组」而不是自己拼字符串（口径只能有一处）。
	GroupName  string `json:"group_name"`
	GroupLabel string `json:"group_label"`
	// Batch / Count（v0.31.93）：接入时会不会整组批量写进去、一共几个。
	// 前端据这句话告诉用户"点生成会接哪几个"，省掉"我以为是整组、结果只接了一个"的来回。
	Batch bool `json:"batch"`
	Count int  `json:"count"`
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

// onboardAllowList 后台为某一类官组配置的允许模型（空切片 = 不限制）。
//
// 只保留**当前仍存在**的模型：模型下线后配置里留着名字不该继续出现在候选里
// （否则用户选一个已经没了的模型，又是 404）。
//
// host 参数区分两类官组（v0.31.82）：号池官组与自挂官组各有自己的放行清单 ——
// 它们的"能跑什么"本来就不同，共用一份清单等于假装两者等价。
func (a *app) onboardAllowList(forHost bool) (allow []string, configured bool) {
	kvKey := kvOnboardModels
	if forHost {
		kvKey = kvOnboardModelsHost
	}
	raw, _ := a.store.GetKV(kvKey)
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

// workGroupPlan 一次「work官组」接入的完整计划。
//
// v0.31.83：两类官组（号池 / 自挂）**共用这一个结构、共用一段计算**，
// 差别只在 planKey() 给出的三个参数（组 id、候选基础、默认值 KV）。
// 这么做是为了让"一致"成为结构性的，而不是靠两段代码各自小心：
// 只要它们走同一个函数，默认值、候选顺序、组名位置、受限提示就不可能各写各的。
type workGroupPlan struct {
	IsHost     bool
	GroupID    string
	GroupName  string   // wb-group-work / wb-group-host
	GroupLabel string   // 号池官组 / 自挂官组
	Members    []string // 具体模型候选（不含组名）
	Default    string
	Restricted bool
	// Batch 这次接入是不是"整组接进去"（v0.31.93）。
	//
	//   · 官组：配了后台选配才批量 —— 没配时成员是整份号池目录（几十个），
	//     一键全写进客户端会让模型选择器变成一锅粥；
	//   · 通道组：**总是批量** —— 它在后台启用的模型就是运营亲手选的那批，
	//     数量本来就可控，没有理由只接一个名字进去。
	Batch bool
	Note  string
}

// planKey 两类官组的差异**只有这几个参数**，其余全部由 buildOnboardPlan 统一处理。
func (a *app) planKey(isHost bool) (gid, label, defKV string) {
	if isHost {
		return groupHostID, "自挂官组", kvOnboardDefaultHost
	}
	return groupWorkID, "号池官组", kvOnboardDefaultPool
}

// buildOnboardPlan 算出接入计划（纯计算，不写响应）。
//
// 两条路径在以下五点上由同一段代码保证一致：
//  1. 默认配置：配置优先 → 没配就用本组虚拟名（开箱即用）；
//  2. 候选构成：本类"能跑什么" ∩ 后台选配，然后**组名固定排第一**；
//  3. 可用性判断：全部来自 hostGroupMembers / keyModelIDs（与选号同源）；
//  4. 鉴权：调用方（handler）用同一个 userGuard + 同一套归属校验；
//  5. 异常/受限：同一套 note 文案结构（受限、空集、未绑定账号）。
func (a *app) buildOnboardPlan(k *APIKey) workGroupPlan {
	if k == nil {
		return workGroupPlan{}
	}
	isHost := k.Kind == KeyKindHost
	gid, label, defKV := a.planKey(isHost)
	// 生效分组（v0.31.91）：用户把 Key 切到某个通道组之后，接入必须写**那个**组名。
	//
	// 否则"我在控制台选了通道组、接进客户端却还是号池官组"—— 切组这件事对
	// 一键接入完全无效，用户看到的现象就是「选了通道组也没用」。
	// 只在组**确实存在且有成员**时才采用：切组后通道被停用的悬空值不能写进脚本。
	official := true
	if !isHost && k.ActiveGroup != "" && a.proxy != nil {
		if ms, lb, ok := a.proxy.groupMembersFor(k.ActiveGroup, k); ok && len(ms) > 0 {
			gid, label, official = k.ActiveGroup, lb, false
			defKV = "" // 后台默认值（onboard_default_pool）只对两类官组有意义
		}
	}
	plan := workGroupPlan{
		IsHost: isHost, GroupID: gid, GroupLabel: label,
		GroupName: groupVirtualModel(gid),
	}

	// ① 候选基础：本类"能跑什么"（与选号同源的判据）。
	base := a.keyModelIDs(k)
	if isHost && a.proxy != nil {
		if hm := a.proxy.hostGroupMembers(k); len(hm) > 0 {
			base = hm
		}
	}
	// ② 与后台选配取交集（选配为空 = 不限制）。
	// 选配是给两类**官组**配的；通道组的候选就是它自己的成员，不套选配
	// （否则运营为号池勾的名单会莫名其妙地把通道组裁掉一半）。
	allow, configured := a.onboardAllowList(isHost)
	if !official {
		allow, configured = nil, false
	}
	plan.Restricted = configured
	models := base
	if configured {
		models = intersectModels(base, allow)
		plan.Note = "候选已按后台「work官组」选配过滤"
		if len(models) == 0 {
			plan.Note = "后台只放行了 " + strings.Join(allow, "、") +
				"，但这把密钥一个都用不了 —— 直接手填模型名，或找管理员调整选配"
		}
	}
	plan.Members = models
	// 批量口径（v0.31.93）：见 workGroupPlan.Batch 的注释。
	// 通道组（!official）恒批量：它的成员就是"后台为该通道启用的模型"，
	// 用户点接入时期望的是把这批都写进去，而不是只写一个组名。
	plan.Batch = configured || !official

	// ③ 默认值：配置优先；没配就用本组虚拟名。
	// defKV 为空（= 生效分组是通道组）时不读 KV：那条 KV 描述的是号池官组的默认。
	def := plan.GroupName
	if defKV != "" {
		if v, _ := a.store.GetKV(defKV); strings.TrimSpace(v) != "" {
			def = strings.TrimSpace(v)
		}
	}
	// 候选列表 = 组名 + 成员；组名固定排第一（它是"我不用挑"的入口）。
	all := append([]string{plan.GroupName}, models...)
	plan.Default = pickDefault(def, all)

	// ④ 自挂官组的特有说明：没绑账号时我们确实不知道能跑什么。
	if isHost && a.proxy != nil && len(a.proxy.hostGroupMembers(k)) == 0 {
		plan.Note = "这把代挂 Key 没有绑定具体账号（或绑定的账号都不在池），平台无法判断它们能跑哪些模型；" +
			"可以手填模型名，或在「API 密钥」里给它绑定账号后再试"
	}
	return plan
}

// candidates 组名在前、具体模型在后 —— 与弹窗候选顺序完全一致。
func (p workGroupPlan) candidates() []string {
	return append([]string{p.GroupName}, p.Members...)
}

// handleOnboardOptions GET /api/onboard/options?key_id=N
//
// 给一键接入弹窗用：候选模型 + 该类 Key 的默认模型。
// 只负责"鉴权 → 取计划 → 序列化"，两类 Key 的计算全在 buildOnboardPlan 里。
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
	plan := a.buildOnboardPlan(k)
	writeJSON(w, http.StatusOK, onboardOptionsResp{
		Models:     plan.candidates(),
		Default:    plan.Default,
		Kind:       k.Kind,
		Restricted: plan.Restricted,
		Note:       plan.Note,
		GroupName:  plan.GroupName,
		GroupLabel: plan.GroupLabel,
		Batch:      plan.Batch,
		Count:      len(plan.Members),
	})
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
	rawHost, _ := a.store.GetKV(kvOnboardModelsHost)
	defPool, _ := a.store.GetKV(kvOnboardDefaultPool)
	defHost, _ := a.store.GetKV(kvOnboardDefaultHost)
	allow, configured := a.onboardAllowList(false)
	allowHost, configuredHost := a.onboardAllowList(true)
	writeJSON(w, http.StatusOK, map[string]any{
		// 号池官组
		"models":     allow, // 已配置的（已剔除下线模型）
		"raw":        raw,   // 原文：让运维看见"配过但已下线"的名字
		"configured": configured,
		// 自挂官组
		"models_host":     allowHost,
		"raw_host":        rawHost,
		"configured_host": configuredHost,
		// 两类各自的默认值
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
		ModelsHost  string `json:"models_host"`
		DefaultPool string `json:"default_pool"`
		DefaultHost string `json:"default_host"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	// 两个默认值只认各自类别的组名（v0.31.82）：
	// 自挂默认填 wb-group-work 仍会 404（组名代表号池模型），所以继续拒；
	// 但 wb-group-host 是自挂官组的正当默认值。
	if defHost := strings.TrimSpace(req.DefaultHost); strings.HasPrefix(defHost, groupModelPrefix) &&
		defHost != groupVirtualModel(groupHostID) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "自挂官组的默认模型只能是 wb-group-host（自挂官组）或具体模型名；" +
				"wb-group-work 是号池官组，代挂 Key 用它必然 404",
			"code": "host_default_cannot_be_group",
		})
		return
	}
	if defPool := strings.TrimSpace(req.DefaultPool); strings.HasPrefix(defPool, groupModelPrefix) &&
		defPool != groupVirtualModel(groupWorkID) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "号池官组的默认模型只能是 wb-group-work（号池官组）或具体模型名",
			"code":  "pool_default_cannot_be_group",
		})
		return
	}
	if err := a.store.SetKV(kvOnboardModels, strings.Join(splitGroups(req.Models), ",")); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if err := a.store.SetKV(kvOnboardModelsHost, strings.Join(splitGroups(req.ModelsHost), ",")); err != nil {
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
	allow, configured := a.onboardAllowList(false)
	allowHost, configuredHost := a.onboardAllowList(true)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"models": allow, "configured": configured,
		"models_host": allowHost, "configured_host": configuredHost,
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
	// 生效分组（v0.31.88）：非空时它说了算。
	// 这里不生效的话，接入候选/下拉会列出"切组后根本调不通"的模型，
	// 用户接完才发现 —— 那正是 v0.31.81 那类 404 的老路。
	if ms := a.activeGroupModels(k); ms != nil {
		seen := map[string]bool{}
		out := make([]string, 0, len(ms))
		for _, m := range ms {
			if m != "" && !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
		sort.Strings(out)
		return out
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
