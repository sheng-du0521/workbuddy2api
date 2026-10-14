// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

// ======================================================================
// 消耗分组（v0.13.0）
//
// 目的：把「账号是否参与平台反代消耗」从隐式的来源判断，变成管理员可显式
// 设定的分组，并在管理后台一键调整。
//
// 四个内置分组：
//
//	apply    用户申请 —— 号主自助申请并审批入池的账号，**优先**被平台消耗
//	proxy    反代     —— 平台自持 / 后台与桌面导入的账号，**次级**消耗
//	prep     预备     —— 已入池但积分低于阈值（<100）的账号，系统自动搬入（v0.20.0）
//	idle     闲置     —— 暂不参与消耗的储备账号（用户代挂未入池的、管理员下架的）
//	abnormal 异常     —— 消耗过程中发现「无积分可扣 / 被封号」的账号，系统自动归档
//
// 判定链：显式 consume_group → 否则按来源推断（inferredConsumeGroup）。
// 参与消耗 = apply 或 proxy；闲置与异常**永不**参与（不设兜底，宁缺勿滥）。
// ======================================================================

const (
	// cgApply 用户申请组：优先消耗。
	cgApply = "apply"
	// cgProxy 反代组：次级消耗。
	cgProxy = "proxy"
	// cgPrep 预备组（v0.20.0）：已入池但积分 < prepEnterCredits 的账号，系统自动搬入。
	// 不参与消耗；积分恢复到 prepReturnCredits 且用户未主动退出号池时自动回流用户申请组。
	cgPrep = "prep"
	// cgIdle 闲置组：不参与消耗。
	cgIdle = "idle"
	// cgAbnormal 异常组：不参与消耗，系统自动归档。
	cgAbnormal = "abnormal"
)

// 预备组的两个阈值（v0.20.0）。
//
// 为什么入与出用**不同**的阈值（100 / 2000）：这是滞回（hysteresis）设计。
// 若进出都用同一条线，账号积分在阈值附近抖动时会来回横跳（搬进预备组 → 立刻
// 又满足回流条件 → 再搬回），每次都是一次 store 写 + 一次选号范围重算。
// 拉开成「跌破 100 才进、涨回 2000 才出」后，中间 100~2000 是一条静默带，
// 账号停在哪一侧都稳定。
const (
	// prepEnterCredits 进入预备组的积分上限：**低于**该值（严格小于）才搬入。
	prepEnterCredits = 100
	// prepReturnCredits 回流用户申请组所需的积分：**达到或超过**该值才回流。
	prepReturnCredits = 2000
	// poolApplyMinCredits 申请入池所需的账号积分下限（v0.20.0）。
	// 低于该值不让提交入池申请 —— 避免把「几乎没有额度」的号推进平台号池，
	// 既要占用审批带宽、又会在池中被反复消耗到余额见底。
	poolApplyMinCredits = 2000
	// tradeMinCredits 账号参与市场交易（上架 / 成交）所需的积分下限（v0.20.0）。
	tradeMinCredits = 4000
)

// prepGroupNote 系统自动搬入预备组时写入 ConsumeGroupNote 的固定文案。
// 带 autoTagPrefix 前缀 = 系统标记，可由回流逻辑自动清除。
const prepGroupNote = autoTagPrefix + "积分低于预备线，已移入预备组"

// autoTagPrefix 自动归档标记前缀（存入 ConsumeGroupNote）。
// 带该前缀的异常账号才会被自动回归逻辑复查；管理员手动设的异常不自动回归。
const autoTagPrefix = "[auto]"

// consumeGroupLabels 分组 → 中文名（前端与文档共用口径）。
var consumeGroupLabels = map[string]string{
	cgApply:    "用户申请",
	cgProxy:    "反代",
	cgPrep:     "预备",
	cgIdle:     "闲置",
	cgAbnormal: "异常",
}

// platformConsumeGroups 平台通用范围的选号顺序（即消耗优先级，从高到低）。
// 不在该列表中的分组（闲置 / 异常 / 未知）一律不参与平台消耗。
var platformConsumeGroups = []string{cgApply, cgProxy}

// normalizeConsumeGroup 校验并规范化分组名；非法值返回空串。
func normalizeConsumeGroup(g string) string {
	switch strings.ToLower(strings.TrimSpace(g)) {
	case cgApply:
		return cgApply
	case cgProxy:
		return cgProxy
	case cgPrep:
		return cgPrep
	case cgIdle:
		return cgIdle
	case cgAbnormal:
		return cgAbnormal
	}
	return ""
}

// inferConsumeGroup 按账号来源推断消耗分组（consume_group 为空时使用）。
//
//   - 代挂账号（owner>0）且已入池（pool_state="in"）→ 用户申请（优先消耗）
//   - 代挂账号未入池                              → 闲置（不参与消耗）
//   - 平台自持 / 后台与桌面导入（owner==0）        → 反代（次级消耗）
func inferConsumeGroup(acc *Account) string {
	if acc == nil {
		return cgIdle
	}
	if acc.OwnerUserID > 0 {
		if acc.PoolState == "in" {
			return cgApply
		}
		return cgIdle
	}
	return cgProxy
}

// effectiveConsumeGroup 取账号的**生效**消耗分组：显式值优先，否则按来源推断。
func effectiveConsumeGroup(acc *Account) string {
	if acc == nil {
		return cgIdle
	}
	if g := normalizeConsumeGroup(acc.ConsumeGroup); g != "" {
		return g
	}
	return inferConsumeGroup(acc)
}

// consumeGroupParticipates 该分组是否参与平台反代消耗。
func consumeGroupParticipates(g string) bool {
	return g == cgApply || g == cgProxy
}

// consumeGroupLabel 分组中文名；未知值原样返回（便于排查历史脏数据）。
func consumeGroupLabel(g string) string {
	if s, ok := consumeGroupLabels[g]; ok {
		return s
	}
	if g == "" {
		return "未设"
	}
	return g
}

// isAutoArchived 该账号是否为「系统自动归档」到异常组的状态（可自动回归）。
func isAutoArchived(acc *Account) bool {
	if acc == nil || effectiveConsumeGroup(acc) != cgAbnormal {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(acc.ConsumeGroupNote), autoTagPrefix)
}

// ---------------------------------------------------------------------
// 管理接口
// ---------------------------------------------------------------------

// handleSetConsumeGroup 单账号设置消耗分组。
// body: {"group":"apply|proxy|idle|abnormal|"}；空串 = 回到「按来源推断」。
func (a *app) handleSetConsumeGroup(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if uid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing uid"})
		return
	}
	var req struct {
		Group string `json:"group"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	raw := strings.TrimSpace(req.Group)
	group := ""
	if raw != "" {
		group = normalizeConsumeGroup(raw)
		if group == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "未知消耗分组（apply/proxy/prep/idle/abnormal）",
			})
			return
		}
	}
	acc, err := a.store.GetAccount(uid)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if acc == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "账号不存在"})
		return
	}
	if err := a.store.SetAccountConsumeGroup(uid, group); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// 分组直接决定选号范围，立即重算。
	_ = a.proxy.SyncAccounts()
	writeJSON(w, http.StatusOK, map[string]any{
		"uid": uid, "consume_group": group, "effective": fallbackGroup(group, acc),
	})
}

// fallbackGroup 显式分组为空时回显推断结果，便于前端即时显示。
func fallbackGroup(group string, acc *Account) string {
	if group != "" {
		return group
	}
	return inferConsumeGroup(acc)
}

// handleBatchConsumeGroup 批量设置消耗分组。
// body: {"uids":["..."],"group":"proxy","all":false}；all=true 时忽略 uids，作用于全部账号。
func (a *app) handleBatchConsumeGroup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UIDs  []string `json:"uids"`
		Group string   `json:"group"`
		All   bool     `json:"all"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	raw := strings.TrimSpace(req.Group)
	group := ""
	if raw != "" {
		group = normalizeConsumeGroup(raw)
		if group == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "未知消耗分组（apply/proxy/prep/idle/abnormal）",
			})
			return
		}
	}
	var uids []string
	if req.All {
		all, err := a.store.ListAccounts()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		for _, acc := range all {
			uids = append(uids, acc.UID)
		}
	} else {
		for _, u := range req.UIDs {
			if v := strings.TrimSpace(u); v != "" {
				uids = append(uids, v)
			}
		}
	}
	if len(uids) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "未选择任何账号"})
		return
	}
	updated, missing := 0, 0
	for _, uid := range uids {
		acc, err := a.store.GetAccount(uid)
		if err != nil || acc == nil {
			missing++
			continue
		}
		if err := a.store.SetAccountConsumeGroup(uid, group); err != nil {
			missing++
			continue
		}
		updated++
	}
	_ = a.proxy.SyncAccounts()
	writeJSON(w, http.StatusOK, map[string]any{
		"updated": updated, "missing": missing, "group": group, "group_label": consumeGroupLabel(group),
	})
}
