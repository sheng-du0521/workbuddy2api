// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// pooldash.go 号池仪表盘聚合（v0.17.3）。
//
// 为什么把聚合放到服务端而不是继续在浏览器里算：
//  1. 「实时概览」每 3 秒轮询一次。若在客户端算，就必须每 3 秒把整个账号列表
//     （18+ 个账号、几十个字段）推给浏览器 —— 传聚合结果只需十几个数字；
//  2. 指纹健康度必须用**与出站请求同一个函数**现算（upstream.AccountMachineID），
//     在浏览器里重写一份哈希算法就等于埋了一个「两边算法漂移」的雷
//     （v0.17.3 刚修掉 Go/Python 口径不一致的问题，不能又在 JS 里造第三个口径）；
//  3. 数据源与「账号池」页卡片完全同源（同一个 allAccountViews），
//     杜绝「仪表盘说 18、卡片显示 17」这类不一致。
//
// 零额外上游出站：全部读本地库 + 内存号池快照，不打任何上游接口。
package main

import (
	"net/http"
	"sort"
	"time"

	"workbuddy2api/internal/upstream"
)

// poolDashTopN 排行榜取前几名（积分 Top / 消耗 Top）。
const poolDashTopN = 8

// poolDashView 号池仪表盘的聚合结果。
type poolDashView struct {
	UpdatedAt int64 `json:"updated_at"`

	Total    int `json:"total"`
	Healthy  int `json:"healthy"`
	Cooling  int `json:"cooling"`
	Disabled int `json:"disabled"`

	CN      int `json:"cn"`
	Global  int `json:"global"`
	Hosting int `json:"hosting"`

	// 积分口径为整数（上游 credit 是整数积分；签名页展示用 fmtNum）。
	CreditSum   int64   `json:"credit_sum"`
	CreditAvg   float64 `json:"credit_avg"`
	CreditMax   int64   `json:"credit_max"`
	ServedSum   int64   `json:"served_sum"`
	ConsumedSum float64 `json:"consumed_sum"`

	// GrowthDoneAt 已执行过成长任务的账号数（不等于「已做满」）。
	GrowthDone int `json:"growth_done"`

	// Group 消耗分组分布：apply / proxy / idle / abnormal。
	Group map[string]int `json:"group"`
	// Expiry token 到期分布：expired / d7 / d30 / later / none。
	Expiry map[string]int `json:"expiry"`
	// Egress 出站形态分布：none（未指定）/ proxy / direct / error。
	Egress map[string]int `json:"egress"`
	// TaskCover 各类自动任务「有多少号开着」（配置口径，不是今天跑成了几号）。
	TaskCover map[string]int `json:"task_cover"`

	// FingerprintAnomaly 设备指纹异常账号数（v0.17.3 新增的显性告警）。
	//
	// 判据两条：uid 为空（派生不出标识）；或派生值与别的账号碰撞。这条计数一旦
	// 大于 0 就说明「多账号隔离实际失效」——而它平时是**完全静默**的：账号照常出站、
	// 上游照常 200，只是所有号在平台看来是同一台设备。故必须显性暴露。
	FingerprintAnomaly int `json:"fingerprint_anomaly"`
	// FingerprintAnomalyUIDs 异常账号的展示名（最多前 5 个，供 UI 直接列出）。
	FingerprintAnomalyUIDs []string `json:"fingerprint_anomaly_uids,omitempty"`

	// TopCredit 积分榜（降序）；TopServed 号池消耗榜（降序）。
	TopCredit []poolDashBar `json:"top_credit"`
	TopServed []poolDashBar `json:"top_served"`

	// CoolingAccounts 冷却中账号明细（v0.17.8）。
	//
	// 为什么单独列出来：健康度环形图只回答「冷却几个」，但运营真正要的是
	// 「**是哪几个、还剩多久**」—— 才能判断是「等两分钟就好」还是「这个号
	// 反复软冷却，该去看是不是 token 要过期了」。
	// 排序：剩余时间升序（最先恢复的排前），同级按展示名，保证轮询时顺序稳定。
	CoolingAccounts []poolDashCooling `json:"cooling_accounts"`
	// CoolingTruncated 冷却账号数是否超过展示上限（true 时前端提示「另有 N 个」）。
	CoolingTruncated bool `json:"cooling_truncated,omitempty"`

	// DisabledAccounts 已停用账号明细（v0.23.3）。
	//
	// 与 CoolingAccounts 并列但**分开**：两者是不同性质的状态，混在一张表里会误导 ——
	//   · 冷却：临时状态，会自己好（等 N 分钟 / 到次日 4 点），运营要的是「还剩多久」；
	//   · 停用：需要人工介入的终态（手动停用 / 登录失效累计禁用 / 被封），
	//     等多久都不会好，运营要的是「是哪几个、谁停的」。
	// 此前停用只累加数字（v.Disabled），明细完全看不到 —— 于是「可用数怎么少了 3 个」
	// 只能去账号池逐页翻。
	DisabledAccounts []poolDashDisabled `json:"disabled_accounts"`
	// DisabledTruncated 停用账号数是否超过展示上限。
	DisabledTruncated bool `json:"disabled_truncated,omitempty"`
}

// poolDashDisabled 停用账号条目。
type poolDashDisabled struct {
	// UID 一键启用/复活需要它（v0.23.4）。此前只下发展示名，于是「已停用」明细
	// 只能看不能操作 —— 运维得自己去账号池几十张卡里找那个号。
	UID    string `json:"uid"`
	Name   string `json:"name"`
	Reason string `json:"reason"` // 手动停用 / 系统禁用（原因）
	// Manual 该停用是否为**运营手动**设置（true = 人工裁决，false = 系统自动禁用）。
	// 前端据此分色：人工裁决是「我知道」，系统禁用是「可能要看看」。
	Manual bool `json:"manual"`
}

// poolDashCooling 冷却账号条目。
type poolDashCooling struct {
	Name         string `json:"name"`
	Kind         string `json:"kind"`          // hard / soft / 空
	RemainingSec int64  `json:"remaining_sec"` // 剩余冷却秒数（0 = 未知/已到期）
	Reason       string `json:"reason,omitempty"`
}

// poolDashCoolingMax 冷却明细最多列出多少个（再多就该去账号池页筛了）。
const poolDashCoolingMax = 8

// poolDashDisabledMax 停用明细上限（与冷却同口径）。
const poolDashDisabledMax = 8

// poolDashBar 排行榜条目。
type poolDashBar struct {
	Label string  `json:"label"`
	Value float64 `json:"value"`
}

// handlePoolDashboard 返回号池仪表盘聚合（实时概览页每 3 秒轮询）。
func (a *app) handlePoolDashboard(w http.ResponseWriter, r *http.Request) {
	views := a.allAccountViews()
	writeJSON(w, http.StatusOK, aggregatePoolDash(views))
}

// aggregatePoolDash 由账号视图算出全部聚合指标（纯函数，便于单测）。
func aggregatePoolDash(views []accountView) poolDashView {
	now := time.Now().Unix()
	v := poolDashView{
		UpdatedAt: now,
		Total:     len(views),
		Group:     map[string]int{},
		Expiry:    map[string]int{},
		Egress:    map[string]int{},
		TaskCover: map[string]int{},
	}
	for _, k := range accountTaskKinds {
		v.TaskCover[k] = 0
	}

	// seenMachine 用于发现派生碰撞：值 → 首个 uid。
	// 碰撞是「多账号隔离失效」的第二种形态（第一种是空 uid），比空 uid 更隐蔽。
	seenMachine := make(map[string]string, len(views))
	anomalous := make([]string, 0)
	// cooling 收集冷却账号明细（顺序无关，最后统一排序）。
	cooling := make([]poolDashCooling, 0, 4)
	// disabled 收集停用账号明细（v0.23.3）：与冷却分开，见 DisabledAccounts 注释。
	disabled := make([]poolDashDisabled, 0, 4)

	for _, av := range views {
		off := av.Disabled || av.ManualOff
		switch {
		case off:
			v.Disabled++
			// 停用原因分两类（人工 / 系统）。av.Reason 是池内记录的具体原因
			// （如 session dead、被封），有就带上，便于直接定位。
			item := poolDashDisabled{UID: av.UID, Name: av.displayName(), Manual: av.ManualOff}
			switch {
			case av.ManualOff:
				item.Reason = "手动停用"
			case av.Reason != "":
				item.Reason = "系统禁用（" + av.Reason + "）"
			default:
				item.Reason = "系统禁用"
			}
			disabled = append(disabled, item)
		case av.Cooling:
			v.Cooling++
			// 冷却明细（v0.17.8）：把「冷却 N 个」落到具体是哪些号。
			cooling = append(cooling, poolDashCooling{
				Name:         av.displayName(),
				Kind:         av.CoolKind,
				RemainingSec: av.CoolingFor,
				Reason:       av.Reason,
			})
		default:
			v.Healthy++
		}

		v.CreditSum += av.Credits
		if av.Credits > v.CreditMax {
			v.CreditMax = av.Credits
		}
		v.ServedSum += av.ServedRequests
		v.ConsumedSum += av.ConsumedCredit

		exp := av.ExpiresAt
		switch {
		case exp <= 0:
			v.Expiry["none"]++
		case exp <= now:
			v.Expiry["expired"]++
		case exp <= now+7*86400:
			v.Expiry["d7"]++
		case exp <= now+30*86400:
			v.Expiry["d30"]++
		default:
			v.Expiry["later"]++
		}

		switch {
		case av.EgressError != "":
			v.Egress["error"]++
		case av.Egress == "direct":
			v.Egress["direct"]++
		case av.Egress != "":
			v.Egress["proxy"]++
		default:
			v.Egress["none"]++
		}

		if av.Realm == "global" {
			v.Global++
		} else {
			v.CN++
		}
		if av.OwnerID > 0 {
			v.Hosting++
		}
		if av.GrowthDoneAt > 0 {
			v.GrowthDone++
		}

		v.Group[av.ConsumeGroupEffective]++

		for _, k := range av.Task.Kinds {
			if k.On {
				v.TaskCover[k.Name]++
			}
		}

		// 指纹健康度。
		mid := upstream.AccountMachineID(av.UID)
		bad := ""
		if mid == "" {
			bad = "uid 为空，派生不出设备标识"
		} else if prev, ok := seenMachine[mid]; ok && prev != av.UID {
			bad = "与其他账号派生同一设备标识"
		} else {
			seenMachine[mid] = av.UID
		}
		if bad != "" {
			v.FingerprintAnomaly++
			if len(anomalous) < 5 {
				anomalous = append(anomalous, av.displayName())
			}
		}

		v.TopCredit = append(v.TopCredit, poolDashBar{Label: av.displayName(), Value: float64(av.Credits)})
		if av.ServedRequests > 0 || av.ConsumedCredit > 0 {
			v.TopServed = append(v.TopServed, poolDashBar{Label: av.displayName(), Value: av.ConsumedCredit})
		}
	}

	if v.Total > 0 {
		v.CreditAvg = float64(v.CreditSum) / float64(v.Total)
	}
	v.FingerprintAnomalyUIDs = anomalous
	v.TopCredit = topBars(v.TopCredit, poolDashTopN)
	v.TopServed = topBars(v.TopServed, poolDashTopN)

	// 冷却明细排序：剩余时间升序（最先恢复的在前），同级按名，保证轮询稳定。
	sort.SliceStable(cooling, func(i, j int) bool {
		if cooling[i].RemainingSec != cooling[j].RemainingSec {
			return cooling[i].RemainingSec < cooling[j].RemainingSec
		}
		return cooling[i].Name < cooling[j].Name
	})
	if len(cooling) > poolDashCoolingMax {
		v.CoolingTruncated = true
		cooling = cooling[:poolDashCoolingMax]
	}
	v.CoolingAccounts = cooling

	// 停用明细排序：人工停用排前（那是运营自己的决定，往往正是他在找的），
	// 同组按名，保证 3 秒轮询时顺序稳定、不会看着乱跳。
	sort.SliceStable(disabled, func(i, j int) bool {
		if disabled[i].Manual != disabled[j].Manual {
			return disabled[i].Manual
		}
		return disabled[i].Name < disabled[j].Name
	})
	if len(disabled) > poolDashDisabledMax {
		v.DisabledTruncated = true
		disabled = disabled[:poolDashDisabledMax]
	}
	v.DisabledAccounts = disabled
	return v
}

// topBars 降序取前 n 条（并列时按标签稳定排序，避免每次轮询顺序抖动）。
func topBars(bars []poolDashBar, n int) []poolDashBar {
	sort.SliceStable(bars, func(i, j int) bool {
		if bars[i].Value != bars[j].Value {
			return bars[i].Value > bars[j].Value
		}
		return bars[i].Label < bars[j].Label
	})
	if len(bars) > n {
		bars = bars[:n]
	}
	return bars
}

// displayName 展示名（昵称优先，回退 uid 前 8 位）。
func (v accountView) displayName() string {
	if v.Nickname != "" {
		return v.Nickname
	}
	if len(v.UID) > 8 {
		return v.UID[:8]
	}
	return v.UID
}
