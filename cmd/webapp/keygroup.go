// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 用户控制台：号池计费密钥的「分组」查看与切换（v0.31.88）。
//
// # 这里说的「分组」是什么
//
// 就是模型组（`work` = 号池官组，`ch<N>` = 某条通道的组）。它同时决定两件事：
//  1. **能用哪些模型**（目录与请求范围）；
//  2. **按什么价计费** —— 倍率是按模型配的（model_ratios.billing_ratio），
//     换了组就是换了整套模型的价，所以"计费分组"与"模型组"在数据上是同一个东西。
//
// # 三个字段别混
//
//	kind          pool / host        —— 从号池还是自己账号取号（决定扣不扣余额）
//	group_scope   被授权能选哪些组    —— 管理员给的边界，用户改不了
//	active_group  现在用哪一个        —— 用户自己切，"" = 不限制（老行为）
//
// 切换只改 active_group：**授权边界不能被用户自己改**，否则等于用户给自己开权限。
//
// # 对已有用量与余额的影响
//
// 一条都不回算：usage_logs 每行在结算那一刻就把 model 与倍率写死了（快照），
// 余额只随实扣变动。切换只影响**之后**的请求 —— 这一点在切换前必须明说，
// 否则用户会以为"切过去以后按新价重算历史"。
package main

import (
	"encoding/json"
	"log"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// keyGroupOption 一个可选分组（前端单选列表的一项）。
type keyGroupOption struct {
	ID      string `json:"id"`    // "" / "work" / "ch3"
	Model   string `json:"model"` // 虚拟模型名（"" 的选项为空）
	Label   string `json:"label"`
	Members int    `json:"members"`
	// RatioMin/RatioMax 该组内**已单独配倍率**的模型的区间；两者都为 0 = 全都跟随全局。
	// 用区间而不是列表：一眼能看出"这组贵不贵"，细节留给「可用模型」里的逐个展示。
	RatioMin float64 `json:"ratio_min"`
	RatioMax float64 `json:"ratio_max"`
	// Current 是否是当前生效的那一项。
	Current bool `json:"current"`
}

// keyGroupResp GET /api/keys/{id}/group-options 的响应。
type keyGroupResp struct {
	Kind        string           `json:"kind"`
	ActiveGroup string           `json:"active_group"`
	ActiveLabel string           `json:"active_label"`
	Options     []keyGroupOption `json:"options"`
	// GlobalRatio 未单独配的模型跟随它 —— 前端要能把"跟随全局"写成一个具体数字。
	GlobalRatio float64 `json:"global_ratio"`
	// Editable host Key 不能切换（它只走本人账号，没有"分组"这回事）。
	Editable bool   `json:"editable"`
	Note     string `json:"note,omitempty"`
}

// groupRatioRange 一组模型已单独配置的倍率区间（0,0 表示全都没配）。
func (a *app) groupRatioRange(models []string) (lo, hi float64) {
	if len(models) == 0 {
		return 0, 0
	}
	list, err := a.store.ListModelRatios()
	if err != nil {
		return 0, 0 // 读不到就当"没配"，不影响分组本身
	}
	want := make(map[string]bool, len(models))
	for _, m := range models {
		want[m] = true
	}
	for _, r := range list {
		if !want[r.Model] || r.BillingRatio <= 0 {
			continue
		}
		if lo == 0 || r.BillingRatio < lo {
			lo = r.BillingRatio
		}
		if r.BillingRatio > hi {
			hi = r.BillingRatio
		}
	}
	return lo, hi
}

// handleKeyGroupOptions GET /api/keys/{id}/group-options
func (a *app) handleKeyGroupOptions(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "非法 id"})
		return
	}
	k, err := a.store.GetAPIKey(u.ID, id)
	if err != nil || k == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "密钥不存在"})
		return
	}
	resp := keyGroupResp{Kind: k.Kind, ActiveGroup: k.ActiveGroup,
		GlobalRatio: a.store.PricingMultiplier()}
	if k.Kind == KeyKindHost {
		resp.Editable = false
		resp.ActiveLabel = "不适用（自挂官组，走你自己的账号）"
		resp.Note = "代挂自用 Key 只用你绑定的账号，不按号池分组计费，因此不能切换分组。"
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.Editable = true

	// 「不限制」永远是第一项：它是存量行为（active_group 为空时的语义），
	// 也是"我全都想要"——不给这个选项，用户切到某个组之后就回不去了。
	all := a.keyModelIDsUnrestricted(k)
	lo, hi := a.groupRatioRange(all)
	resp.Options = append(resp.Options, keyGroupOption{
		ID: "", Label: "不限制（全部可用模型）", Members: len(all),
		RatioMin: lo, RatioMax: hi, Current: k.ActiveGroup == "",
	})

	// 这里刻意**不用 visibleGroups**：它在 v0.31.82 被收窄成"只给 work"，
	// 用它做切换列表就永远只有一个选项。切换要的是"**被授权**的组"——
	// 授权了才出现（没授权的用户看不到，也就不存在越权选择）。
	if a.proxy != nil {
		seenG := map[string]bool{}
		for _, gid := range append([]string{groupWorkID}, splitGroups(k.GroupScope)...) {
			if gid == "" || seenG[gid] {
				continue
			}
			seenG[gid] = true
			ms, label, ok := a.proxy.groupMembersFor(gid, k)
			if !ok {
				continue // 组已删/停用：不列（当前生效的那一个会走下面的"已失效"分支）
			}
			glo, ghi := a.groupRatioRange(ms)
			resp.Options = append(resp.Options, keyGroupOption{
				ID: gid, Model: groupVirtualModel(gid), Label: label, Members: len(ms),
				RatioMin: glo, RatioMax: ghi, Current: k.ActiveGroup == gid,
			})
		}
	}
	for _, o := range resp.Options {
		if o.Current {
			resp.ActiveLabel = o.Label
		}
	}
	if resp.ActiveLabel == "" {
		// active_group 指向了一个已不存在/已取消授权的组：显式说出来，
		// 否则界面上"当前"那一项会是空的，用户以为界面坏了。
		resp.ActiveLabel = "已失效（" + k.ActiveGroup + "）"
		resp.Note = "当前生效的组已不存在或授权被取消，请求会按「不限制」处理。请重新选择一个分组。"
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleKeyGroupSet POST /api/keys/{id}/group { "group": "" | "work" | "ch3" }
//
// 校验顺序刻意是「类型 → 授权 → 存在 → 非空」：
// 先答"这把 Key 能不能切"（类型），再答"你有没有资格用这个组"（授权），
// 最后才是"这个组现在能不能用"（存在/非空）。顺序反了会把 403 说成 404，
// 用户会以为组被删了，实际是没授权。
func (a *app) handleKeyGroupSet(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "非法 id"})
		return
	}
	var req struct {
		Group string `json:"group"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	k, err := a.store.GetAPIKey(u.ID, id)
	if err != nil || k == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "密钥不存在"})
		return
	}
	if k.Kind == KeyKindHost {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "代挂自用 Key 没有「号池分组」：它只走你绑定的账号，不按分组计费",
			"code":  "host_key_has_no_group",
		})
		return
	}
	if k.Status != StatusActive {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error": "这把密钥已停用，先启用再切换分组", "code": "key_disabled",
		})
		return
	}

	group := strings.TrimSpace(req.Group)
	if group != "" && group != groupWorkID {
		// 授权检查：work 恒可用，通道组必须在 group_scope 里。
		// 用 groupGranted 而不是就地遍历 —— 与 activeGroupModels 的失效判据必须是同一个，
		// 否则会出现"能切过去、但切完立即算失效"这种自相矛盾。
		if !groupGranted(k, group) {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error": "这个分组没有授权给你（需要管理员在后台授权后才会出现）",
				"code":  "group_not_granted",
			})
			return
		}
	}

	// 存在性 + 非空：切到一个不存在的组，之后每次请求都会失败 —— 提前拦住。
	models := a.keyModelIDsUnrestricted(k)
	label := "不限制（全部可用模型）"
	if group != "" {
		if a.proxy == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "号池未就绪，稍后再试"})
			return
		}
		ms, lb, ok := a.proxy.groupMembersFor(group, k)
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]any{
				"error": "分组不存在（可能通道已删除或已停用）", "code": "group_not_found",
			})
			return
		}
		if len(ms) == 0 {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": "这个分组现在没有任何可用模型，切过去会让请求全部失败；等它有模型再切",
				"code":  "group_empty",
			})
			return
		}
		models, label = ms, lb
	}

	ok, err := a.store.SetAPIKeyActiveGroup(u.ID, k.ID, group)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "密钥不存在"})
		return
	}
	lo, hi := a.groupRatioRange(models)
	log.Printf("INFO: [key] 用户 %d 把密钥 %d 的分组切到 %q（%d 个模型）", u.ID, k.ID, group, len(models))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "active_group": group, "active_label": label,
		"models": len(models), "ratio_min": lo, "ratio_max": hi,
		"global_ratio": a.store.PricingMultiplier(),
		// 把"不回算"写成响应的一部分：前端直接展示，不靠文案记忆。
		"history_unchanged": true,
		"note":              "切换只影响之后的请求：已有用量按当时的模型与倍率结算，余额不会被重算。",
	})
}

// keyModelIDsUnrestricted 平台层面"这把 Key 理论上能看到"的模型（不看 active_group）。
//
// 单独抽出来是因为两个用途相反：
//   - 列出候选分组时要它（不然切到某组后，别的组就从列表里消失了，回不去）；
//   - 实际分发/目录要的是**受 active_group 约束**的那一份（见 activeGroupModels）。
func (a *app) keyModelIDsUnrestricted(k *APIKey) []string {
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
	if !modelsCacheFresh() {
		a.refreshModelsCache()
	}
	add(cachedModelIDs())
	if relayIDs, err := a.store.relayModelIDs(); err == nil {
		add(relayIDs)
	}
	sort.Strings(out)
	return out
}

// activeGroupModels 该 Key 当前生效组限定的模型；nil = 不限制（走老逻辑）。
//
// **这是「切换分组」唯一的生效点**：目录、下拉、请求校验都从这里取，
// 各写一套必然出现"目录列着 A 组、请求按 B 组拦"的分裂。
func (a *app) activeGroupModels(k *APIKey) []string {
	if a.proxy == nil {
		return nil
	}
	return a.proxy.activeGroupModels(k)
}

// activeGroupModels 同上，但挂在 Proxy 上 —— ServeChat 手里只有 *Proxy，
// 而"请求该不该被这组拦住"必须与目录用同一份成员计算，不能各写一套。
func (p *Proxy) activeGroupModels(k *APIKey) []string {
	if k == nil || k.ActiveGroup == "" {
		return nil
	}
	// 授权撤销后必须**立刻**失效（v0.31.89）：groupMembersFor 只看"组存不存在"，
	// 不看"这把 Key 有没有被授权" —— 少了这一句，管理员撤销授权等于没生效：
	// 界面会显示「已失效」，请求却仍按那一组走。两个口径不一致比缺功能更糟。
	if k.ActiveGroup != groupWorkID && !groupGranted(k, k.ActiveGroup) {
		return nil
	}
	ms, _, ok := p.groupMembersFor(k.ActiveGroup, k)
	if !ok {
		// 组失效（被删/停用）：按不限制处理，与 group-options 的提示保持一致。
		// 这里**不能**返回空切片 —— 空切片会被上层当成"一个模型都没有"，直接 404 所有请求。
		return nil
	}
	return ms
}

// ratioRangeText 把倍率区间写成一句人话（前端与日志共用同一套说法）。
func ratioRangeText(lo, hi, global float64) string {
	switch {
	case lo == 0 && hi == 0:
		return "全部跟随全局 ×" + strconv.FormatFloat(global, 'f', -1, 64)
	case math.Abs(lo-hi) < 1e-9:
		return "×" + strconv.FormatFloat(lo, 'f', -1, 64)
	default:
		return "×" + strconv.FormatFloat(lo, 'f', -1, 64) + " ~ ×" + strconv.FormatFloat(hi, 'f', -1, 64)
	}
}

// modelInGroup 模型是否在这组的成员里（线性扫：组规模是几十，不值得建 map）。
func modelInGroup(models []string, model string) bool {
	for _, m := range models {
		if m == model {
			return true
		}
	}
	return false
}

// groupGranted 该 Key 是否被授权使用某个通道组（work 恒可用，由调用方另行处理）。
func groupGranted(k *APIKey, gid string) bool {
	for _, g := range splitGroups(k.GroupScope) {
		if g == gid {
			return true
		}
	}
	return false
}
