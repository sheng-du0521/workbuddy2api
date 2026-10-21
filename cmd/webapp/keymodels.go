// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 密钥可用的模型清单（v0.31.74，需求 2/5）。
//
// # 为什么需要它
//
// 前端有四处要回答"这把 Key 能用哪些模型"，此前各问各的：
//   - 测试对话页：拉 `/api/models`（**平台全集**，与 Key 无关）
//   - 一键接入弹窗：同样拉 `/api/models`
//
// 于是用户会选到一个自己这把 Key 根本调不通的模型，发出去才 404 ——
// 这种"能选但不能用"比没有下拉更糟。
//
// # 口径（与 /v1/models 同源，不另算一套）
//
//	池计费 Key : 号池模型目录（cachedModelIDs，与 poolKnowsModel 同源）
//	             ∪ 通道模型（store.relayModelIDs，号池没有的那些）
//	代挂 Key   : 同上，但它的可用面更窄 —— **账号级模型清单在本项目里不存在**
//	             （只有运行期观测），所以只能回答"绑定的号在不在池里"：
//	               · 有号在池 → 用池目录（它们的模型就在里面）
//	               · 绑了具体账号但一个都不在池 → 返回空 + reason，
//	                 前端据此切成"手填模型名"，而不是给一份会 404 的假清单
package main

import (
	"net/http"
	"sort"
	"strconv"
)

// keyModelsResp 对外形状（字段名显式带 json tag：下发给前端就必须有契约）。
type keyModelsResp struct {
	Models []string `json:"models"`
	Count  int      `json:"count"`
	// Kind 回显密钥类型（pool=号池计费 / host=代挂自用），前端据此换文案。
	Kind string `json:"kind"`
	// ManualAllowed 允许手填模型名。代挂 Key 绑定的号未入池时清单不可知，
	// 但用户仍然应该能测试自己的号 —— 不给清单不等于不让填。
	ManualAllowed bool `json:"manual_allowed"`
	// Reason 清单为空/受限的原因码（前端只按码换文案，不解析中文）。
	Reason string `json:"reason,omitempty"`
	Note   string `json:"note,omitempty"`
	// Groups 该 Key 能看见的模型组（v0.31.76）：含虚拟模型名与成员数。
	// 前端用它给下拉加「组」标签，也用它判断"空组"。
	Groups []modelGroupInfo `json:"groups,omitempty"`
}

// handleKeyModels GET /api/keys/{id}/models
func (a *app) handleKeyModels(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "非法 id"})
		return
	}
	// 归属校验：GetAPIKey 带 user_id 条件，跨用户直接查不到 → 404（与 reveal 同一纪律）。
	k, err := a.store.GetAPIKey(u.ID, id)
	if err != nil || k == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "密钥不存在"})
		return
	}
	if k.Status != StatusActive {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "该密钥已停用，无法列出可用模型"})
		return
	}

	resp := keyModelsResp{Kind: "pool", ManualAllowed: true}

	// 代挂自用 Key：先看它绑定的号在不在池里（决定"能不能给出清单"）。
	if k.Kind == KeyKindHost {
		resp.Kind = "host"
		uids := splitUIDs(k.AccountUIDs)
		if len(uids) > 0 {
			anyInPool := false
			if a.proxy != nil {
				for _, uid := range uids {
					if _, ok := a.proxy.pool.Status(uid); ok {
						anyInPool = true
						break
					}
				}
			}
			if !anyInPool {
				// 一个都不在池：我们确实不知道这些号能跑什么模型。
				// 返回空清单 + 允许手填，比给一份池里别人的模型诚实。
				resp.Reason = "host_accounts_not_in_pool"
				resp.Note = "该密钥绑定的账号尚未进入号池，平台还没有观测到它们的模型；可直接手填模型名测试"
				writeJSON(w, http.StatusOK, resp)
				return
			}
		}
		resp.Note = "代挂自用 Key 只走你自己的账号，这里列的是平台已观测到的模型目录；实际以调用结果为准"
	}

	// 号池模型目录：与 /v1/models 用同一个缓存，避免两处各拉一次上游。
	if !modelsCacheFresh() {
		a.refreshModelsCache()
	}
	// 模型组（v0.31.76）：控制台**总是**列出 work 官组 ——
	// 它既是"接入时的默认值"，也是让用户第一次知道"组"这个概念的入口。
	// （客户端的 /v1/models 只在 Key 绑了通道组时才列组名，避免给存量客户端加噪声。）
	if a.proxy != nil {
		resp.Groups = a.proxy.visibleGroups(k)
	}
	// 生效分组（v0.31.88）：非空时只列该组 —— 目录与请求约束必须同源。
	if ms := a.activeGroupModels(k); ms != nil {
		out2 := make([]string, 0, len(ms)+1)
		seen2 := map[string]bool{}
		for _, g := range resp.Groups {
			if g.ID == k.ActiveGroup && g.Members > 0 && !seen2[g.Model] {
				seen2[g.Model] = true
				out2 = append(out2, g.Model)
			}
		}
		for _, m := range ms {
			if m != "" && !seen2[m] {
				seen2[m] = true
				out2 = append(out2, m)
			}
		}
		resp.Models, resp.Count = out2, len(out2)
		if len(out2) == 0 {
			resp.Reason = "active_group_empty"
			resp.Note = "当前生效分组里没有可用模型；可到密钥卡上切换分组"
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	seen := map[string]bool{}
	out := make([]string, 0, 32+len(resp.Groups))
	for _, g := range resp.Groups {
		if g.Members == 0 || seen[g.Model] {
			continue // 空组不列（配了但没选模型，出现在下拉里会一选就失败）
		}
		seen[g.Model] = true
		out = append(out, g.Model)
	}
	for _, id := range cachedModelIDs() {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	// 通道模型：`/v1/models` 里号池没有的那些（relayVisibleModels 的口径）。
	// 通道模型的可见性由 enabled 决定，relayModelIDs 已经过滤过。
	// 读失败只丢通道那部分（号池清单仍可用），不把整个接口做成 500 ——
	// 这个接口是"辅助选择"的，不该因为通道表读不出来就禁用测试功能。
	if relayIDs, rerr := a.store.relayModelIDs(); rerr == nil {
		for _, id := range relayIDs {
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)

	resp.Models = out
	resp.Count = len(out)
	if len(out) == 0 {
		resp.Reason = "no_models_observed"
		resp.Note = "平台还没有观测到任何模型（号池为空或模型目录尚未建立）；可直接手填模型名测试"
	}
	writeJSON(w, http.StatusOK, resp)
}
