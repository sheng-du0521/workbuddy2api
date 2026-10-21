// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 管理端「Key 可见模型组」授权（v0.31.77，需求 1-4 的最后一屏）。
//
// 为什么单独两个接口而不是塞进用户详情：
//   - 用户详情已经很长（用量/返点/提现/代挂账号），再塞密钥列表会让它变成"什么都有"的接口，
//     而后台这两件事的操作频率差别很大（看详情很频繁，改授权很少）；
//   - 授权是**写**操作，应该能被单独审计与单独失败，不能因为详情里某个统计查询报错而一起失败。
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
)

// handleAdminUserKeys GET /api/admin/users/{id}/keys
//
// 返回该用户的密钥（**只给掩码**，与用户侧列表同一纪律：明文只在显式 reveal 时出现）。
func (a *app) handleAdminUserKeys(w http.ResponseWriter, r *http.Request) {
	uid, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || uid <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "非法用户 id"})
		return
	}
	if u, err := a.store.GetUserByID(uid); err != nil || u == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "用户不存在"})
		return
	}
	keys, err := a.store.ListAPIKeys(uid)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, map[string]any{
			"id":          k.ID,
			"name":        k.Name,
			"masked":      k.KeyPrefix + "…",
			"kind":        k.Kind,
			"status":      k.Status,
			"group_scope": k.GroupScope,
			// allowed_groups：这把 Key **可以**被授权哪些组。
			// 代挂 Key 只有 work —— 它只用本人账号，接通道组等于用平台的钱给个人用。
			"allowed_groups": a.allowedGroupsFor(k),
		})
	}
	// 组清单随接口下发（前端据此渲染勾选项，不自己拼 id）。
	//
	// ⚠️ 这里**不能用 visibleGroups**：v0.31.82 起它收窄成"只给 work"，
	// 用它渲染授权界面就永远只有一个可勾项 —— 管理员会以为"通道组不能授权"。
	// 授权界面要的是**全部可选组**（work + 所有 enabled 通道组）。
	var groups []modelGroupInfo
	if a.proxy != nil {
		n := 0
		if ms, _, ok := a.proxy.groupMembersFor(groupWorkID, nil); ok {
			n = len(ms)
		}
		groups = append(groups, modelGroupInfo{
			ID: groupWorkID, Model: groupVirtualModel(groupWorkID),
			Label: "号池官组", Kind: "work", Members: n,
		})
		groups = append(groups, a.proxy.allChannelGroups()...)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"keys": out, "groups": groups, "count": len(out),
	})
}

// allowedGroupsFor 该 Key 可被授权的组 id 列表。
func (a *app) allowedGroupsFor(k *APIKey) []string {
	if k == nil {
		return nil
	}
	if k.Kind == KeyKindHost {
		return []string{groupWorkID}
	}
	out := []string{groupWorkID}
	if a.proxy != nil {
		for _, g := range a.proxy.allChannelGroups() {
			out = append(out, g.ID)
		}
	}
	return out
}

// handleAdminSetKeyGroups POST /api/admin/keys/{id}/groups  {group_scope:"ch3,ch4"}
//
// 语义：
//   - 只接受**已存在**的通道组；不存在的 id 被丢弃（并在返回里说明），不报错 ——
//     组可能是刚被删的，让管理员因为一个过期勾选而整屏重填不值得；
//   - 代挂自用 Key 显式拒绝（400），不静默改成 work —— 静默改会让管理员以为授权成功了；
//   - 返回值给出**最终生效**的 scope，前端用它回显（不是回显用户提交值）。
func (a *app) handleAdminSetKeyGroups(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "非法 Key id"})
		return
	}
	var req struct {
		GroupScope string `json:"group_scope"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	k, err := a.store.GetAPIKeyAnyOwner(id)
	if err != nil || k == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "密钥不存在"})
		return
	}
	if k.Kind == KeyKindHost {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "代挂自用 Key 只能用 work 官组（它只走号主本人账号，不能接入第三方通道组）",
		})
		return
	}

	// 过滤：只保留存在的通道组（work 不写进 scope —— 它恒可见，写进去只会让数据看起来像"授权过"）。
	valid := map[string]bool{}
	if a.proxy != nil {
		for _, g := range a.proxy.allChannelGroups() {
			valid[g.ID] = true
		}
	}
	kept := make([]string, 0, 4)
	dropped := make([]string, 0, 4)
	seen := map[string]bool{}
	for _, gid := range splitGroups(req.GroupScope) {
		if gid == groupWorkID || seen[gid] {
			continue
		}
		seen[gid] = true
		if !valid[gid] {
			dropped = append(dropped, gid)
			continue
		}
		kept = append(kept, gid)
	}
	scope := strings.Join(kept, ",")
	if err := a.store.SetAPIKeyGroupsByID(id, scope); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// 授权是"谁能用哪条通道"的决定，必须有痕：谁授权、授权到哪把 Key、最终生效什么。
	log.Printf("INFO: [admin] 设置 Key 组授权 id=%d name=%q scope=%q dropped=%v",
		id, k.Name, scope, dropped)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "id": id, "group_scope": scope, "dropped": dropped,
	})
}
