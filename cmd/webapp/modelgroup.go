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
	"context"
	"log"
	"net/http"
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

// groupKindOf 组 id → 分类标签（前端与日志用）。
func groupKindOf(gid string) string {
	switch gid {
	case groupWorkID:
		return "work"
	case groupHostID:
		return "host"
	}
	return "channel"
}

// userGroups 用户面「看得见、且选得动」的模型组 —— **唯一口径**（v0.31.91）。
//
// # 为什么必须再加一层
//
// v0.31.82 把用户面收窄成两类官组（visibleGroups 只回 work / host）；
// v0.31.90 又因为用户投诉"只有号池组"，给控制台开了「切到通道组」——
// 但**只有切换弹窗改了口径**（selectableGroupIDs 含 ch<N>），
// 模型清单（keymodels.go）与 /v1/models（main.go modelsPayloadFor）还在用 visibleGroups。
//
// 结果就是用户报的那句「生成密钥后依然无法选用通道组」：
// 切换弹窗里能切到 ch1，但模型下拉里根本没有 `wb-group-ch1` 这一项 ——
// 能切、却选不到，两套口径分裂的典型症状。
//
// # 口径
//
//	代挂自用 Key → 自挂官组（host）
//	池计费 Key   → 号池官组（work）+ 该 Key **可切换**的通道组（ch<N>）
//
// 内部直接复用 selectableGroupIDs，所以"列出来的"与"切得动的"永远是同一批，
// 不可能再出现"列表里有、点了 403"。
func (p *Proxy) userGroups(k *APIKey) []modelGroupInfo {
	if p == nil || k == nil {
		return nil
	}
	// ⚠️ 对话面只列**能对话**的成员（v0.31.98）：
	// 生图模型（gpt-image-* 等）挂在这里，用户选了必然被上游 400 —— 那是
	// "我们给了它一个用不了的名字"，不是用户选错。过滤后成员为 0 的组**不列**
	// （只挂生图模型的通道因此不再作为对话分组出现），与"空组不列"同一口径。
	if k.Kind == KeyKindHost {
		ms, label, ok := p.groupMembersFor(groupHostID, k)
		if !ok {
			return nil
		}
		// 只过滤**非空**的成员表：代挂 Key 还没绑定账号时成员本来就是 0，
		// 那个 0 不是"全被生图模型占了"，不能因此把自挂官组藏掉（否则用户
		// 生成一把代挂 Key 后连组名都看不见，又回到"选不到组"的老问题）。
		if len(ms) > 0 {
			ms = chatCapableModels(ms)
			if len(ms) == 0 {
				return nil
			}
		}
		return []modelGroupInfo{{
			ID: groupHostID, Model: groupVirtualModel(groupHostID),
			Label: label, Kind: groupKindOf(groupHostID), Members: len(ms),
		}}
	}
	out := make([]modelGroupInfo, 0, 4)
	for _, gid := range p.selectableGroupIDs(k) {
		ms, label, ok := p.groupMembersFor(gid, k)
		if !ok {
			continue // 组已删 / 通道已停用：不列（与 groupSelectable 的口径一致）
		}
		// 同上：只有"原本有成员、过滤后没了"才判定为不可用对话分组。
		// 原本就空（通道刚建、还没加映射）交给上层既有口径（成员 0 由调用方处理）。
		if len(ms) > 0 {
			ms = chatCapableModels(ms)
			if len(ms) == 0 {
				continue // 只剩生图模型 = 这条通道不能对话，不是一个可用的对话分组
			}
		}
		out = append(out, modelGroupInfo{
			ID: gid, Model: groupVirtualModel(gid),
			Label: label, Kind: groupKindOf(gid), Members: len(ms),
		})
	}
	return out
}

// 注：v0.31.91 删除了 visibleGroups（"只回官组"的那一版）。
//
// 删的理由不是它写得不对，而是**留着就一定会被再次误用**：用户面已经有
// 「切换分组」（selectableGroupIDs，含通道组）之后，任何只回 work/host 的
// 用户可见清单都会造成"能切过去、却选不到这个名字"—— 这正是
// 「生成密钥后依然无法选用通道组」的根因。用户面的组统一走 userGroups。

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
func (p *Proxy) applyGroupExpansion(payload map[string]any, model string, k *APIKey) (string, []string, bool) {
	models, label, ok := p.expandGroupModels(model, k)
	if !ok {
		return model, nil, false
	}
	log.Printf("INFO: [group] %q → %q（%s，同组备选 %d 个）", model, models[0], label, len(models)-1)
	payload["model"] = models[0]
	return models[0], models, true
}

// ctxGroupModelsKey 本次请求"组展开出来的候选模型"（首个已写进 payload）。
//
// 为什么用 context 而不是改 serveRelay 的签名：签名已经 11 个参数，再加一个
// "可能与请求无关、只在组名场景有值"的切片，会让每个调用点都要想一遍。
// 挂在请求上语义也更准 —— 它本来就是**这一次请求**的上下文。
type ctxGroupModelsKey struct{}

// withGroupModels 把组候选挂到请求上（只挂一次，后续 relay 读取）。
func withGroupModels(r *http.Request, models []string) *http.Request {
	if len(models) < 2 {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), ctxGroupModelsKey{}, models))
}

// groupModelsFrom 取本次请求的组候选（没有就返回 nil = 不是组名请求）。
func groupModelsFrom(r *http.Request) []string {
	if r == nil {
		return nil
	}
	if v, ok := r.Context().Value(ctxGroupModelsKey{}).([]string); ok {
		return v
	}
	return nil
}

// expandGroupModels 把虚拟组名展开成**一组**真实模型名（首个是首选，其余是备选）。
//
// 为什么返回一组而不是一个（v0.31.96）：组的意义是"这一组里随便挑一个能跑的"，
// 而上游的可用性是**按模型**的。2026-10-21 的真实现场：同一条通道里
// gpt-5.6/5.6-sol/5.6-terra 全部 429、gpt-6-astra/gpt-6-sol 正常 200，
// 而展开固定挑名字最小的 gpt-5.6 —— 于是"用组名永远失败，手填 gpt-6-astra 却能用"。
// 只挑一个的写法把"组"退化成了"别名"，用户看到的就是"组不能用"。
//
// 首选顺序（确定性优先，不做随机/轮询）：
//  1. 组内第一个"号池认识且有可用账号"的成员；
//  2. 否则组内第一个"有可用通道"的成员；
//  3. 都没有 → 仍返回第一个成员，让下游按它正常失败（503/404 的文案由既有链路给，
//     不在这里自造 —— 否则同一个"没号可用"会有两套错误消息）。
//
// 备选（后面的元素）按组内原顺序排列，由 relay 在首个失败时逐个重试。
//
// ok=false 表示这不是组名（调用方保持原值，零影响）。
func (p *Proxy) expandGroupModels(model string, k *APIKey) (models []string, label string, ok bool) {
	gid, hasCut := strings.CutPrefix(strings.TrimSpace(model), groupModelPrefix)
	if !hasCut {
		return nil, "", false
	}
	members, label, ok := p.groupMembersFor(gid, k)
	if !ok || len(members) == 0 {
		// 组不存在 / 成员为 0：不展开，让它按"未知模型"走 ——
		// 客户端填错组名时，报"模型不存在"比报 500 更有指向。
		return nil, "", false
	}
	// 对话面只展开**能对话**的成员（v0.31.98）：组里混着生图模型时，
	// 旧行为会把 gpt-image-* 也当候选 → 轮到它必 400，而错误文案说的是
	// "上游拒绝"，看起来像网关坏了。生图模型的入口是图像端点，不是组名。
	members = chatCapableModels(members)
	if len(members) == 0 {
		return nil, "", false
	}
	// 首选：号池里第一个能跑的成员（自挂官组的成员已按"这把 Key 的账号"筛过）。
	first := ""
	for _, m := range members {
		if known, decided := p.poolKnowsModel(m); decided && known {
			if len(p.pool.AvailableUIDsForModel(m)) > 0 {
				first = m
				break
			}
		}
	}
	if first == "" {
		for _, m := range members {
			if cands, err := p.store.relayCandidates(m); err == nil && len(cands) > 0 {
				first = m
				break
			}
		}
	}
	if first == "" {
		first = members[0]
	}
	out := make([]string, 0, len(members))
	out = append(out, first)
	for _, m := range members {
		if m != first {
			out = append(out, m)
		}
	}
	return out, label, true
}
