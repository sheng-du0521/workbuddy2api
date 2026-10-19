// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 模型冷却的用户端视图（v0.31.55）。
//
// 背景：模型级冷却（上游 6004 限额 / 11102「该后端无此模型」）在管理端已经有
// 逐账号台账（`limitedModelsOf` + 账号卡的「模型级冷却」列表），但**用户端没有**。
// 结果是用户只看到"某个模型突然一直失败"，既不知道是平台在冷却、也不知道还要等多久 ——
// 他只能反复重试，而重试恰好是最不该做的事（会让冷却反复续期）。
//
// 这里把它做成**模型维度**的结论，而不是把号池台账搬给用户：
//
//	· 不返回账号数、不返回 uid、不返回内部错误码（那是平台内部结构）；
//	· 只回答三件事：这个模型现在能不能用 / 为什么 / 多久恢复。
package main

import (
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// modelCoolingItem 一个模型的可用性结论。
type modelCoolingItem struct {
	Model string `json:"model"`
	// State 两档，判据与生产选号**同源**（不是自己另算一套）：
	//
	//	partial 还有可用账号 —— 仍能服务，但池子变小、更容易失败；
	//	full    一个可用账号都选不出来 —— 现在请求必然失败。
	//
	// 为什么用 `AvailableUIDsForModel` 判 full：它就是转发时选号用的那个判据。
	// 自己另写一套"冷却条目数 >= 账号数"会在"这个号根本不支持该模型"时给出
	// 过于乐观的 partial —— 而那正是用户最容易踩的情形。
	State string `json:"state"`
	// ReasonLabel 归一化后的原因（不暴露 6004/11102 这类内部码）。
	ReasonLabel string `json:"reason_label,omitempty"`
	// FirstResumeAt / LastResumeAt 最早 / 最晚恢复时刻（Unix 秒）。
	//
	// 两个都给是因为它们回答不同问题：partial 时用户想知道"最快多久能碰运气"，
	// full 时想知道"最晚什么时候一定能好"。只给一个必然误导一半场景。
	FirstResumeAt int64 `json:"first_resume_at,omitempty"`
	LastResumeAt  int64 `json:"last_resume_at,omitempty"`
	// RetryAfterSec 建议等待秒数（= 与 state 对应的那个恢复时刻）。
	RetryAfterSec int64 `json:"retry_after_sec"`
	// Probing 系统会对它做半开探测（6004 才允许）。
	//
	// 透出它是为了回答"我要不要一直盯着刷新"：会自动试探的模型，
	// 用户等通知即可；不试探的只能等自然到期。
	Probing bool `json:"probing,omitempty"`
}

// modelCoolingSummary 顶部汇总（一眼看懂"现在有没有事"）。
type modelCoolingSummary struct {
	CoolingModels int `json:"cooling_models"`
	FullModels    int `json:"full_models"`
	TotalModels   int `json:"total_models"`
}

type modelCoolingView struct {
	GeneratedAt int64               `json:"generated_at"`
	Models      []modelCoolingItem  `json:"models"`
	Summary     modelCoolingSummary `json:"summary"`
	// PollSec 建议前端刷新间隔：冷却是分钟级变化，5 秒一次纯属浪费。
	PollSec int `json:"poll_sec"`
}

// buildModelCoolingView 聚合全池的模型冷却。
//
// 成本：只对**有冷却记录**的模型调一次 `AvailableUIDsForModel`（O(账号)）；
// 无冷却时零遍历。全平台同一时刻冷却的模型通常 0~3 个。
func (a *app) buildModelCoolingView() modelCoolingView {
	now := time.Now()
	v := modelCoolingView{GeneratedAt: now.Unix(), PollSec: 30}

	rows := a.proxy.pool.ModelCooldowns(now)
	// total_models 取**用户可见的模型目录**（model_ratios，与「可用模型」弹窗同源），
	// 而不是 `ObservedModels()`：后者只统计"产生过成本观测"的模型（生产实测只有 3 个），
	// 会让卡片显示"共 3 个模型"—— 与用户刚在另一个弹窗里看到的 51 个直接矛盾。
	// 目录为空（首次同步前）才退回观测数，此时至少不是 0。
	v.Summary.TotalModels = len(a.proxy.pool.ObservedModels())
	if list, err := a.store.ListModelRatios(); err == nil && len(list) > 0 {
		v.Summary.TotalModels = len(list)
	}

	// 按模型聚合。用 map 收集后排序输出 —— map 遍历无序，
	// 同一份状态两次刷新顺序不同会让人以为"刚发生了什么"。
	type agg struct {
		first, last time.Time
		reasons     map[string]bool
		probing     bool
	}
	byModel := map[string]*agg{}
	for _, r := range rows {
		m := strings.TrimSpace(r.Model)
		if m == "" {
			continue
		}
		g := byModel[m]
		if g == nil {
			g = &agg{reasons: map[string]bool{}}
			byModel[m] = g
		}
		if g.first.IsZero() || r.Until.Before(g.first) {
			g.first = r.Until
		}
		if g.last.IsZero() || r.Until.After(g.last) {
			g.last = r.Until
		}
		if lbl := modelCoolingReasonLabel(r.Reason); lbl != "" {
			g.reasons[lbl] = true
		}
	}

	models := make([]string, 0, len(byModel))
	for m := range byModel {
		models = append(models, m)
	}
	sort.Strings(models)

	v.Models = make([]modelCoolingItem, 0, len(models))
	for _, m := range models {
		g := byModel[m]
		it := modelCoolingItem{Model: m, State: "partial"}
		// 与生产选号同源：能选出号 = partial，选不出 = full。
		if len(a.proxy.pool.AvailableUIDsForModel(m)) == 0 {
			it.State = "full"
		}
		if len(g.reasons) == 1 {
			for k := range g.reasons {
				it.ReasonLabel = k
			}
		} else if len(g.reasons) > 1 {
			it.ReasonLabel = "多个原因（限流 / 模型不可用）"
		}
		// 恢复时间取哪个取决于 state：partial 看最早（能碰运气），full 看最晚（一定能好）。
		resume := g.last
		if it.State != "full" {
			resume = g.first
		}
		if !resume.IsZero() {
			it.FirstResumeAt = g.first.Unix()
			it.LastResumeAt = g.last.Unix()
			sec := int64(resume.Sub(now).Seconds())
			if sec < 0 {
				sec = 0
			}
			it.RetryAfterSec = sec
		}
		v.Models = append(v.Models, it)
		if it.State == "full" {
			v.Summary.FullModels++
		}
	}
	v.Summary.CoolingModels = len(v.Models)
	return v
}

// modelCoolingReasonLabel 把运维文案归一成用户能懂的一句话。
//
// 为什么不直接透出原文：原文里带 6004 / 11102 / reset 来源后缀，是给运维排障的。
// 对用户来说，重要的是"该换模型还是该等" —— 这两类的处置完全不同。
func modelCoolingReasonLabel(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	switch {
	case s == "":
		return ""
	case strings.Contains(raw, "11102") || strings.Contains(raw, "无此模型") ||
		strings.Contains(s, "model not found") || strings.Contains(s, "no such model"):
		// 负缓存：重试无意义，只能等它过期或换模型。
		return "该模型在当前账号上不可用"
	case strings.Contains(raw, "6004") || strings.Contains(raw, "限流") ||
		strings.Contains(raw, "限额") || strings.Contains(s, "rate limit") ||
		strings.Contains(s, "too many requests"):
		return "上游限流"
	default:
		return "上游暂时不可用"
	}
}

// ---------------------------------------------------------------------------
// 缓存
// ---------------------------------------------------------------------------

// 用户端可能几十个人在同一个页面上 30 秒刷一次，而聚合成一次成本并不为零
// （每个冷却模型都要扫一遍账号表）。5 秒缓存把并发合并掉，同时不影响观感
// —— 冷却是分钟级状态，5 秒的陈旧完全看不出来。
const modelCoolingTTL = 5 * time.Second

var (
	modelCoolingMu   sync.Mutex
	modelCoolingAt   time.Time
	modelCoolingLast modelCoolingView
)

func (a *app) modelCoolingCached() modelCoolingView {
	now := time.Now()
	modelCoolingMu.Lock()
	defer modelCoolingMu.Unlock()
	if !modelCoolingAt.IsZero() && now.Sub(modelCoolingAt) < modelCoolingTTL {
		return modelCoolingLast
	}
	v := a.buildModelCoolingView()
	modelCoolingAt, modelCoolingLast = now, v
	return v
}

// handleModelCooling GET /api/models/cooling
//
// 用户可读。只返回模型级结论 —— 号池结构（几个号、谁在冷却）不出这个接口。
func (a *app) handleModelCooling(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.modelCoolingCached())
}
