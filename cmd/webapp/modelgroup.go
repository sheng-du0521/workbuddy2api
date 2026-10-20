// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 模型组（v0.31.76，需求 1-4）。
//
// # 组是什么
//
// 一个**派生的**视图：不存成员表，成员来自它依附的东西 ——
//
//	work   → 号池观测到的模型目录（与 poolKnowsModel 同源）
//	ch<N>  → 通道 N 下 enabled 的模型映射
//
// 这样"号池新增一个模型"或"通道停用一条映射"会立刻反映到组里，
// 不需要同步第二份数据 —— 两份数据一定会有对不上的那一天。
//
// # 为什么要有虚拟模型名
//
// CC Switch / Claude Code 这类客户端只能填**一个**模型名。让它们直接填 `wb-group-work`，
// 我们展开成组内一个真实模型名再转发。
//
// 关键设计：展开发生在**归一化与选号之前**，展开后 model 就是真名 ——
// 于是归一化、选号、计费倍率、台账、模型冷却全部沿用既有逻辑，
// "组"不会变成第二套计费口径（账单里永远是真实模型名）。
//
// # 组间不互相兜底
//
// 展开只在**组内**做选择；A 组全不可用不会落到 B 组，也不会落到组外的号池。
// 跨组兜底会让"我买的这个组"变成"随机被谁服务"，账单与排障都无法归因。
package main

import (
	"log"
	"strconv"
	"strings"
)

const (
	// groupModelPrefix 虚拟模型名前缀。写死这个前缀 → 永远不可能和真实模型名冲突。
	groupModelPrefix = "wb-group-"
	// groupWorkID 内置组：号池（需求 1「work 官组」）。
	groupWorkID = "work"
)

// modelGroupInfo 一个组的展示信息（下发给前端；成员数用于提示"空组"）。
type modelGroupInfo struct {
	ID      string `json:"id"`
	Model   string `json:"model"` // 客户端应填的虚拟模型名
	Label   string `json:"label"`
	Kind    string `json:"kind"` // work | channel
	Members int    `json:"members"`
}

// groupVirtualModel 组 id → 虚拟模型名。
func groupVirtualModel(id string) string { return groupModelPrefix + id }

// splitGroups 逗号分隔的组 id → 切片（空串 → 空切片）。
func splitGroups(scope string) []string { return splitUIDs(scope) }

// groupMembers 取一个组的成员（按稳定顺序）与显示名。
//
// 返回 ok=false 表示"不是合法组"或"组已不存在/已停用"——
// 两者的前端表现一样（不列出、解析为未知模型），不需要区分。
func (p *Proxy) groupMembers(gid string) (members []string, label string, ok bool) {
	gid = strings.TrimSpace(gid)
	if gid == "" {
		return nil, "", false
	}
	if gid == groupWorkID {
		ids := cachedModelIDs()
		if len(ids) == 0 {
			ids = p.pool.ObservedModels()
		}
		// 号池为空时组**仍然存在**（组是配置事实），只是成员为 0：
		// 不向外回落"内置兜底清单"，否则用户会看到一个实际不存在的模型。
		return ids, "work 官组", true
	}
	if rest, hasCut := strings.CutPrefix(gid, "ch"); hasCut {
		n, err := strconv.ParseInt(rest, 10, 64)
		if err != nil || n <= 0 {
			return nil, "", false
		}
		name, models, err := p.store.relayGroup(n)
		if err != nil || name == "" {
			return nil, "", false
		}
		return models, "通道 · " + name, true
	}
	return nil, "", false
}

// visibleGroups 一把 Key 能看见的组：work 恒在，通道组按 GroupScope 授权。
//
// 代挂自用 Key **只能**看 work：它只用本人账号，接通道组等于用平台的钱给个人用
// （与 relay 里拦 KeyKindHost 是同一条理由）。
func (p *Proxy) visibleGroups(k *APIKey) []modelGroupInfo {
	out := make([]modelGroupInfo, 0, 4)
	if ms, label, ok := p.groupMembers(groupWorkID); ok {
		out = append(out, modelGroupInfo{
			ID: groupWorkID, Model: groupVirtualModel(groupWorkID),
			Label: label, Kind: "work", Members: len(ms),
		})
	}
	if k == nil || k.Kind == KeyKindHost {
		return out
	}
	for _, gid := range splitGroups(k.GroupScope) {
		if gid == groupWorkID {
			continue // 已在内置项里
		}
		ms, label, ok := p.groupMembers(gid)
		if !ok {
			continue // 组被删/停用：忽略（不报错，见 SetAPIKeyGroups 注释）
		}
		out = append(out, modelGroupInfo{
			ID: gid, Model: groupVirtualModel(gid),
			Label: label, Kind: "channel", Members: len(ms),
		})
	}
	return out
}

// applyGroupExpansion 组名 → 真名，并**同步改写 payload**，返回 (真名, 是否展开)。
//
// 为什么单独抽出来（而不是在 ServeChat 里就地写两行）：出站请求体是从 payload
// 重新编码的，只改局部变量会让上游照样收到 `wb-group-work` → 11102。
// 这个"必须一起改"的约束在这里有单测钉住（TestApplyGroupExpansionRewritesPayload），
// 否则哪天有人把 payload["model"] = real 删掉，只有线上才会发现。
func (p *Proxy) applyGroupExpansion(payload map[string]any, model string) (string, bool) {
	real, label, ok := p.expandGroupModel(model)
	if !ok {
		return model, false
	}
	log.Printf("INFO: [group] %q → %q（%s）", model, real, label)
	payload["model"] = real
	return real, true
}

// expandGroupModel 把虚拟组名展开成一个**真实模型名**。
//
// 选谁（确定性优先，不做随机/轮询）：
//  1. 组内第一个"号池认识且有可用账号"的成员；
//  2. 否则组内第一个"有可用通道"的成员；
//  3. 都没有 → 仍返回第一个成员，让下游按它正常失败（503/404 的文案由既有链路给，
//     不在这里自造 —— 否则同一个"没号可用"会有两套错误消息）。
//
// ok=false 表示这不是组名（调用方保持原值，零影响）。
func (p *Proxy) expandGroupModel(model string) (real, label string, ok bool) {
	gid, hasCut := strings.CutPrefix(strings.TrimSpace(model), groupModelPrefix)
	if !hasCut {
		return "", "", false
	}
	members, label, ok := p.groupMembers(gid)
	if !ok || len(members) == 0 {
		// 组不存在 / 成员为 0：不展开，让它按"未知模型"走 ——
		// 客户端填错组名时，报"模型不存在"比报 500 更有指向。
		return "", "", false
	}
	for _, m := range members {
		if known, decided := p.poolKnowsModel(m); decided && known {
			if len(p.pool.AvailableUIDsForModel(m)) > 0 {
				return m, label, true
			}
		}
	}
	for _, m := range members {
		if cands, err := p.store.relayCandidates(m); err == nil && len(cands) > 0 {
			return m, label, true
		}
	}
	return members[0], label, true
}
