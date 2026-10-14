// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// preview.go 选号「候选队列预览」（R8，v0.26.1）：把一次 pick 的判定过程**只读地**复现出来。
//
// ## 为什么需要
//
// 排障时最常见的问题不是"选号坏了"，而是"**为什么总选它 / 为什么跳过它**"。
// 这个问题此前只能靠读代码 + 猜：pick 的判定分散在 healthy / inFlightFull / 成本分层 /
// minPickGap 四处，日志只记最终结果，不记落选原因。同类项目（agent2api）把
// 「候选队列预览 + 每个账号为什么排这 / 为什么跳过」做成了面板能力，实测对运维价值很高。
//
// ## 契约（重要）
//
//   - **只读**：本函数不写 lastUsed、不动探索 timer、不改任何状态位。
//     因此它复现的是"如果现在发一个请求，会怎么排"，而不是"上次是怎么排的"。
//   - 与 pick 的判定顺序保持一致；**唯一的偏差**是 `costExplore` 的条件探索
//     （那会写状态，预览不做），已在下方注明。
//   - 输出按权重降序（落选者按 UID 排在其后），便于直接贴进工单。
package pool

import (
	"fmt"
	"sort"
	"time"
)

// CandidatePreview 一个账号在本次选号里的处境。
type CandidatePreview struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname,omitempty"`
	Realm    string `json:"realm,omitempty"`

	Included bool   `json:"included"`            // 是否进入候选（进候选才有 weight/rank）
	Skip     string `json:"skip,omitempty"`      // 落选原因（机器可读短码）
	SkipText string `json:"skip_text,omitempty"` // 落选原因（人话）

	Weight float64 `json:"weight,omitempty"`
	Rank   int     `json:"rank,omitempty"` // 权重排名（1 起，仅 Included）
	InTop5 bool    `json:"in_top5"`        // 是否进了 Top5 短名单（抽签范围）

	Credits int64 `json:"credits"`
	// CostTier 成本分层：0 已实测免费 / 1 无观测 / 2 已实测收费。-1 = 未参与计算。
	CostTier int `json:"cost_tier"`

	Cooling            bool  `json:"cooling"`
	CoolUntil          int64 `json:"cool_until,omitempty"`           // Unix 秒
	ModelCooldownUntil int64 `json:"model_cooldown_until,omitempty"` // 该模型上的冷却截止
	BreakerUntil       int64 `json:"breaker_until,omitempty"`
	InFlight           int   `json:"in_flight"`
	FaultFails         int   `json:"fault_fails"` // 连续 11140 计数（快判死的号要能被看见）
	RecentPick         bool  `json:"recent_pick"` // 距上次被选中 < minPickGap（防撞号窗口内）
}

// PreviewCandidates 复现一次选号的候选排序与落选原因。reqModel 为空 = 不看模型级豁免。
func (p *Pool) PreviewCandidates(reqModel, realm string) []CandidatePreview {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()

	uids := make([]string, 0, len(p.byUID))
	for uid := range p.byUID {
		uids = append(uids, uid)
	}
	sort.Strings(uids) // 稳定输出：同权重时顺序可复现

	out := make([]CandidatePreview, 0, len(uids))
	// 第一遍：硬过滤，判定"能不能进候选"。
	for _, uid := range uids {
		e := p.byUID[uid]
		c := CandidatePreview{UID: uid, CostTier: -1, Credits: e.credits,
			InFlight: int(e.inFlight.Load()), FaultFails: e.faultFails}
		if e.a != nil {
			c.Nickname, c.Realm = e.a.Nickname, e.a.Realm()
		}
		c.CoolUntil = unixOrZero(e.until)
		c.BreakerUntil = unixOrZero(e.breakerUntil)
		c.RecentPick = !e.lastUsed.IsZero() && now.Sub(e.lastUsed) < minPickGap
		if reqModel != "" {
			if mc, ok := e.modelCooldowns[reqModel]; ok && mc.Until.After(now) {
				c.ModelCooldownUntil = mc.Until.Unix()
			}
		}
		switch {
		case e.disabled:
			c.Skip, c.SkipText = "disabled", "系统判定不可用（"+e.reason+"）"
		case e.manualDisabled:
			c.Skip, c.SkipText = "manual_disabled", "运维手动停用"
		case realm != "" && (e.a == nil || e.a.Realm() != realm):
			c.Skip, c.SkipText = "realm", "不属于本次选号域（"+realm+"）"
		case reqModel != "" && !e.healthyForModel(now, reqModel):
			if e.healthy(now) {
				c.Skip, c.SkipText = "model_cooling", "仅该模型被限流（其它模型仍可用）"
			} else {
				c.Skip, c.SkipText = "cooling", "账号级冷却中（至 "+fmtUnixSec(c.CoolUntil)+"）"
			}
		case !e.healthy(now):
			c.Skip, c.SkipText = "cooling", "账号级冷却中（至 "+fmtUnixSec(c.CoolUntil)+"）"
		case p.inFlightFull(e):
			c.Skip, c.SkipText = "in_flight_full", "在途请求已占满该账号名额"
		default:
			c.Included = true
		}
		c.Cooling = !e.healthy(now)
		out = append(out, c)
	}

	// 第二遍：成本分层硬过滤 + 权重（与 pick 同口径：maxCredits 用全集口径）。
	var maxCredits int64
	for i := range out {
		if out[i].Included && p.byUID[out[i].UID].credits > maxCredits {
			maxCredits = p.byUID[out[i].UID].credits
		}
	}
	bestTier, any := 2, false
	for i := range out {
		if !out[i].Included {
			continue
		}
		tier, _ := p.previewTier(p.byUID[out[i].UID], reqModel, now)
		out[i].CostTier = tier
		if !any || tier < bestTier {
			bestTier, any = tier, true
		}
	}
	if any {
		for i := range out {
			if out[i].Included && out[i].CostTier != bestTier {
				out[i].Included = false
				out[i].Skip, out[i].SkipText = "cost_tier", fmt.Sprintf(
					"存在更优成本层（本档 tier=%d，可用最优档 tier=%d）", out[i].CostTier, bestTier)
			}
		}
	}

	// 第三遍：权重与排名（只对进候选者算，避免对落选者白算一遍）。
	ws := make([]*CandidatePreview, 0, len(out))
	for i := range out {
		if out[i].Included {
			ws = append(ws, &out[i])
			out[i].Weight = p.weightOf(p.byUID[out[i].UID], maxCredits, now)
		}
	}
	sort.SliceStable(ws, func(i, j int) bool { return ws[i].Weight > ws[j].Weight })
	for i, c := range ws {
		c.Rank = i + 1
		c.InTop5 = i < 5
	}

	// 第四遍：防撞号窗口。**刻意与 pick 的"全被挤出则退回 LRU"对齐**：
	// 若所有候选都在窗口内，就不标记跳过（否则预览会说"没人可选"，而实际会照选）。
	anyOutside := false
	for _, c := range ws {
		if !c.RecentPick {
			anyOutside = true
			break
		}
	}
	if anyOutside {
		for _, c := range ws {
			if c.RecentPick {
				c.Included = false
				c.Skip, c.SkipText = "recent_pick",
					"距上次被选中不足 "+minPickGap.String()+"（防并发撞号，稍后会自然可用）"
			}
		}
	}

	// 排序：进候选者按 rank，落选者按 UID 排后面。
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Included != b.Included {
			return a.Included
		}
		if a.Included {
			return a.Rank < b.Rank
		}
		if a.Skip != b.Skip {
			return a.Skip < b.Skip
		}
		return a.UID < b.UID
	})
	return out
}

// previewTier 与 pick 内 costTier 同口径（0 免费 / 1 无观测 / 2 已实测收费）。
func (p *Pool) previewTier(e *entry, reqModel string, now time.Time) (int, float64) {
	mc, ok := e.modelCostOf(reqModel, now)
	if !ok {
		return 1, 0
	}
	if mc.CostPer1k <= 0 {
		return 0, 0
	}
	return 2, mc.CostPer1k
}

// fmtUnixSec Unix 秒 → 本地可读时刻（0 → "未定"）。仅用于人话文案。
func fmtUnixSec(sec int64) string {
	if sec <= 0 {
		return "未定"
	}
	return time.Unix(sec, 0).Format("15:04:05")
}
