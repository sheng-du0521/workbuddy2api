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
	// groupWorkID 号池官组（平台号池）。对池计费 Key 生效。
	groupWorkID = "work"
	// groupHostID 自挂官组（号主自己的账号）。只对代挂自用 Key 生效。
	//
	// v0.31.82：把"一键接入选配"这一系列统一叫「work官组」，
	// 分类上只有两个：号池官组 / 自挂官组（不再有第三种）。
	// 之所以还要两个虚拟名而不是一个：这两种"选模型"的**依据完全不同** ——
	// 号池官组看平台账号目录，自挂官组看号主自己绑定的账号；
	// 合成一个名字会让"选出来但我跑不了"重新变成可能（v0.31.81 的 404 就是这么来的）。
	groupHostID = "host"
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

// groupMembersFor 取一个组的成员（按稳定顺序）与显示名。
//
// 返回 ok=false 表示"不是合法组"或"组已不存在/已停用"——
// 两者的前端表现一样（不列出、解析为未知模型），不需要区分。
//
// k 是"按谁的视角看"：
//   - work 官组与 Key 无关（平台号池目录）；
//   - host 官组**必须**有代挂 Key，且这把 Key 绑定了具体账号，否则成员为空
//     （我们不知道没绑账号的代挂号能跑什么，就不假装知道）。
func (p *Proxy) groupMembersFor(gid string, k *APIKey) (members []string, label string, ok bool) {
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
		return ids, "号池官组", true
	}
	if gid == groupHostID {
		return p.hostGroupMembers(k), "自挂官组", true
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

// hostGroupMembers 自挂官组的成员 = 这把代挂 Key 的**绑定账号能满足**的模型。
//
// 判据用 `AvailableUIDsForModel(m)` 与该 Key 的账号集合求交 —— 这是全仓唯一
// "某某号此刻能服务某模型"的权威来源（与选号同源），不另造一套。
//
// 未绑定具体账号（AccountUIDs 为空）时返回空：那种 Key 是"本人全部代挂号"，
// 模型面取决于哪些号在池，硬猜就会重新制造 v0.31.81 那种"选了但跑不了"。
func (p *Proxy) hostGroupMembers(k *APIKey) []string {
	if k == nil || k.Kind != KeyKindHost {
		return nil
	}
	uids := splitKVSet(k.AccountUIDs)
	if len(uids) == 0 {
		return nil
	}
	ids := cachedModelIDs()
	if len(ids) == 0 {
		ids = p.pool.ObservedModels()
	}
	out := make([]string, 0, len(ids))
	for _, m := range ids {
		for _, uid := range p.pool.AvailableUIDsForModel(m) {
			if uids[uid] {
				out = append(out, m)
				break
			}
		}
	}
	return out
}

// visibleGroups 一把 Key 能看见的「work官组」条目。
//
// v0.31.82 起**只有两类**，按 Key 类型二选一：
//   - 池计费 Key → 号池官组（wb-group-work）
//   - 代挂自用 Key → 自挂官组（wb-group-host）
//
// 通道组（ch<N>）**不再出现在这里**：它属于运营内部的上游配置，不是用户可选的"组"。
// 解析逻辑仍保留（已经授权过的 Key 照常能用），只是不再对外暴露这个分类 ——
// 用户面只有"号池官组 / 自挂官组"两种，与产品口径一致。
func (p *Proxy) visibleGroups(k *APIKey) []modelGroupInfo {
	out := make([]modelGroupInfo, 0, 2)
	if k != nil && k.Kind == KeyKindHost {
		ms, label, ok := p.groupMembersFor(groupHostID, k)
		if ok {
			out = append(out, modelGroupInfo{
				ID: groupHostID, Model: groupVirtualModel(groupHostID),
				Label: label, Kind: "host", Members: len(ms),
			})
		}
		return out
	}
	if ms, label, ok := p.groupMembersFor(groupWorkID, k); ok {
		out = append(out, modelGroupInfo{
			ID: groupWorkID, Model: groupVirtualModel(groupWorkID),
			Label: label, Kind: "work", Members: len(ms),
		})
	}
	return out
}

// groupKindMismatch 判断「组名与 Key 类型不匹配」。
//
// ServeChat 与 handleKeyOnboard **共用这一个判定**（v0.31.83）：
// 两处如果各写一套，迟早会出现"生成脚本时说不行、发请求却说行"这种自相矛盾。
// 返回空串表示没有不匹配（未知组名 / ch<N> 历史组名放行，交给展开逻辑处理）。
func groupKindMismatch(keyKind, gid string) (code, msg, hint string) {
	switch {
	case keyKind == KeyKindHost && gid == groupWorkID:
		return "host_key_cannot_use_pool_group",
			"代挂自用 Key 不能用号池官组",
			"host-billed keys run only on your own accounts, so wb-group-work would pick a pool model " +
				"your accounts do not have (upstream 404). Use wb-group-host (self-hosted group) or a " +
				"concrete model name"
	case keyKind != KeyKindHost && gid == groupHostID:
		return "pool_key_cannot_use_host_group",
			"池计费 Key 不能用自挂官组",
			"wb-group-host picks a model from your own accounts, which only host-billed keys have. " +
				"Use wb-group-work (pool group) or a concrete model name"
	}
	return "", "", ""
}

// allChannelGroups 所有 **enabled** 通道构成的组（不含 work）。
//
// v0.31.82 起**只给管理端内部使用**（例如排查"这条通道影响多少把 Key"），
// 不再出现在任何用户可见的"可选组"里 —— 用户面只有号池官组 / 自挂官组两种。
//
// 走 ListUpstreams 而不是逐条 groupMembersFor：这是管理端列表与授权用的，
// 要的是"全"，通道数量本来就是个位数；热路径（解析单个组名）才需要单条查询。
func (p *Proxy) allChannelGroups() []modelGroupInfo {
	ups, err := p.store.ListUpstreams()
	if err != nil {
		log.Printf("WARN: [group] 读取通道列表失败：%v", err)
		return nil
	}
	out := make([]modelGroupInfo, 0, len(ups))
	for _, u := range ups {
		if !u.Enabled {
			continue // 停用的通道 = 组不可见（与 groupMembers 的口径一致）
		}
		n := 0
		for _, m := range u.Models {
			if m.Enabled {
				n++
			}
		}
		out = append(out, modelGroupInfo{
			ID:      "ch" + strconv.FormatInt(u.ID, 10),
			Model:   groupVirtualModel("ch" + strconv.FormatInt(u.ID, 10)),
			Label:   "通道 · " + u.Name,
			Kind:    "channel",
			Members: n,
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
//
// k 参与展开（v0.31.82）：自挂官组的成员取决于**这把 Key 绑了哪些账号**，
// 同一份 payload 在不同 Key 下会展开出不同模型 —— 所以 Key 必须一路传进来。
func (p *Proxy) applyGroupExpansion(payload map[string]any, model string, k *APIKey) (string, bool) {
	real, label, ok := p.expandGroupModel(model, k)
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
func (p *Proxy) expandGroupModel(model string, k *APIKey) (real, label string, ok bool) {
	gid, hasCut := strings.CutPrefix(strings.TrimSpace(model), groupModelPrefix)
	if !hasCut {
		return "", "", false
	}
	members, label, ok := p.groupMembersFor(gid, k)
	if !ok || len(members) == 0 {
		// 组不存在 / 成员为 0：不展开，让它按"未知模型"走 ——
		// 客户端填错组名时，报"模型不存在"比报 500 更有指向。
		return "", "", false
	}
	// 自挂官组的成员已经按"这把 Key 的账号能不能服务"筛过一遍，
	// 这里再按可用性挑第一个（号池里第一个能跑的就是它的最优解）。
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
