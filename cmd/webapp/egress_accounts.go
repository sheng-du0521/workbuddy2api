// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// egress_accounts.go 「账号代理」视图（v0.17.9）。
//
// ---------------------------------------------------------------------------
// 为什么把「代理」拆成两件事
// ---------------------------------------------------------------------------
// 本项目的代理配置本来是**两层**的，但此前界面只显性暴露了其中一层：
//
//	出口代理（基础设施层）：线路池 —— 「有哪些 IP 可以出去」。维护 URL、探活、
//	                        容量上限、批量分配策略，对象是**线路**。
//	账号代理（绑定层）    ：逐账号 —— 「这个号实际从哪出去」。三态：未指定 /
//	                        显式直连 / 指定线路；对象是**账号**。
//
// 原先账号代理这一层只有两种入口：账号池卡片上的状态标签（只读）与「出口代理」
// 页底部的批量分配（只能批量）。要单个改某个号，得回账号池翻卡片 —— 而卡片上的
// 「代理 / 直连」按钮早在 v0.15.0 就撤掉了，界面上却还写着「点卡片上的代理按钮」，
// 属于**说明与实际不符的过时信息**。本文件与配套前端页面一起把这件事理顺：
// 线路池归「出口代理」，逐账号归「账号代理」，两者同一个页面、同一套口径。
//
// ---------------------------------------------------------------------------
// 状态口径（与服务端选号、账号池卡片标签三方一致，改一处要同步三处）
// ---------------------------------------------------------------------------
//
//	proxy            指定了代理 URL（实际走它）
//	inherit          未指定，但配了「全局默认出口代理」→ 实际走默认那条
//	direct_explicit  配置为 direct 哨兵 → 强制不加代理（含绕过环境变量代理）
//	direct_implicit  未指定且无全局默认 → 实际从本机 IP 直连（多为漏配）
//	bad              配了代理但不可用（EgressError 非空）→ 已被排除出平台选号
//
// 注意 inherit 与 direct_implicit 的区别只在「有没有全局默认」：
// 前者**没暴露本机 IP**，后者**暴露了**。风控含义不同，不能合并展示。
package main

import (
	"net/http"
	"sort"
	"strings"

	"workbuddy2api/internal/egress"
)

// 账号代理状态常量（前端按同一套字符串分支渲染）。
const (
	egrStateProxy          = "proxy"
	egrStateInherit        = "inherit"
	egrStateDirectExplicit = "direct_explicit"
	egrStateDirectImplicit = "direct_implicit"
	egrStateBad            = "bad"
)

// egressAccountProxy 单个账号的代理视图。
type egressAccountProxy struct {
	UID       string `json:"uid"`
	Nickname  string `json:"nickname"`
	Realm     string `json:"realm"`
	OwnerID   int64  `json:"owner_id"`
	OwnerName string `json:"owner_name,omitempty"`

	// Configured 账号级**配置值**原样（"" = 未指定；"direct" = 显式直连；否则代理 URL）。
	// 与 Resolved 分开返回：管理员需要看到「我设的是什么」与「实际走了什么」两件事。
	Configured string `json:"configured"`
	// Resolved 实际生效的出口（已把「未指定 → 继承全局默认」算进去）。
	Resolved string `json:"resolved"`
	// State 见文件头状态口径表。
	State string `json:"state"`
	// StateLabel 中文状态名（前端直接用，避免两端各写一份映射）。
	StateLabel string `json:"state_label"`

	// ProxyID/ProxyName 当 Resolved 命中线路池中的某条线路时的台账信息（供显示备注名）。
	ProxyID   string `json:"proxy_id,omitempty"`
	ProxyName string `json:"proxy_name,omitempty"`

	// EgressError 配置异常原因（非空 = 状态为 bad，且该号已被排除出平台选号）。
	EgressError string `json:"egress_error,omitempty"`

	Disabled bool `json:"disabled"`
	Cooling  bool `json:"cooling"`
}

// egressAccountsView 账号代理聚合返回。
type egressAccountsView struct {
	Accounts []egressAccountProxy `json:"accounts"`
	// Summary 各状态计数（键与 State 同名），供前端画汇总与做筛选。
	Summary map[string]int `json:"summary"`
	// DefaultEgress 当前全局默认出口代理（空 = 未配置）。
	// 前端据此解释「未指定」的账号到底是继承还是直连。
	DefaultEgress string `json:"default_egress"`
	// MaxPerProxy 同出口账号数上限（本页只用于展示，分配在「出口代理」区）。
	MaxPerProxy int `json:"max_per_proxy"`
}

// stateLabelOf 状态 → 中文名（唯一映射源，前端不重复实现）。
func stateLabelOf(state string) string {
	switch state {
	case egrStateProxy:
		return "指定线路"
	case egrStateInherit:
		return "继承默认"
	case egrStateDirectExplicit:
		return "显式直连"
	case egrStateDirectImplicit:
		return "未指定 · 直连"
	case egrStateBad:
		return "配置异常"
	default:
		return state
	}
}

// handleEgressAccounts 管理侧「账号代理」列表。
// GET /api/admin/egress/accounts
//
// 只读本地库 + 内存号池快照，**不向上游出站**（探活是显式动作，见
// POST /api/admin/accounts/{uid}/egress）。
func (a *app) handleEgressAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := a.store.ListAccounts()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	proxies, err := a.store.ListEgressProxies()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	names := a.userNameMap()
	def := a.proxy.DefaultEgress()

	// URL → 线路台账（取备注名与 id，便于「指定线路」显示成人读的名字）。
	byURL := map[string]*EgressProxy{}
	for _, p := range proxies {
		// 用规范化键（v0.31.33）：历史行可能是旧写法，原样比会"查不到"。
		byURL[egressURLKey(p.URL)] = p
	}

	out := egressAccountsView{
		Accounts:      make([]egressAccountProxy, 0, len(accounts)),
		Summary:       map[string]int{},
		DefaultEgress: def,
		MaxPerProxy:   a.store.egressMaxPerProxy(),
	}
	for _, acc := range accounts {
		configured := strings.TrimSpace(acc.EgressProxy)
		resolved := configured
		switch {
		case egress.IsDirect(configured):
			resolved = egress.Direct
		case resolved == "":
			resolved = def // 未指定 → 继承全局默认（默认也可能为空 = 实际直连）
		}

		item := egressAccountProxy{
			UID:        acc.UID,
			Nickname:   acc.Nickname,
			Realm:      acc.Realm,
			OwnerID:    acc.OwnerUserID,
			Configured: configured,
			Resolved:   resolved,
			Disabled:   acc.Disabled,
			Cooling:    false, // 由号池快照补齐（库里的 cooldown 与运行态可能不同）
		}
		if acc.OwnerUserID > 0 {
			item.OwnerName = names[acc.OwnerUserID]
		}
		if st, ok := a.proxy.pool.Status(acc.UID); ok {
			item.Cooling = st.Cooling
		}
		item.EgressError = a.proxy.EgressErrorForUID(acc.UID)

		// 状态判定：异常优先（配了代理但不可用最需要被看见）。
		switch {
		case item.EgressError != "":
			item.State = egrStateBad
		case egress.IsDirect(configured):
			item.State = egrStateDirectExplicit
		case configured != "":
			item.State = egrStateProxy
		case def != "":
			item.State = egrStateInherit
		default:
			item.State = egrStateDirectImplicit
		}
		if p, ok := byURL[egressURLKey(resolved)]; ok {
			item.ProxyID, item.ProxyName = p.ID, p.Name
		}
		item.StateLabel = stateLabelOf(item.State)

		out.Summary[item.State]++
		out.Accounts = append(out.Accounts, item)
	}

	// 排序：异常最先 → 未指定直连（漏配风险）→ 显式直连 → 继承 → 指定线路；
	// 同级按昵称/uid 稳定排序，保证每次刷新顺序不抖动（否则列表看着像在跳）。
	rank := map[string]int{
		egrStateBad: 0, egrStateDirectImplicit: 1, egrStateDirectExplicit: 2,
		egrStateInherit: 3, egrStateProxy: 4,
	}
	sort.SliceStable(out.Accounts, func(i, j int) bool {
		ri, rj := rank[out.Accounts[i].State], rank[out.Accounts[j].State]
		if ri != rj {
			return ri < rj
		}
		ni, nj := displayNameOf(out.Accounts[i]), displayNameOf(out.Accounts[j])
		if ni != nj {
			return ni < nj
		}
		return out.Accounts[i].UID < out.Accounts[j].UID
	})
	writeJSON(w, http.StatusOK, out)
}

// displayNameOf 展示名（昵称优先，回退 uid 前 8 位）。
func displayNameOf(a egressAccountProxy) string {
	if a.Nickname != "" {
		return a.Nickname
	}
	if len(a.UID) > 8 {
		return a.UID[:8]
	}
	return a.UID
}
