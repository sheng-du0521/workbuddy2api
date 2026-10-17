// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 用户控制台的健康检测（v0.31.31，插件 health-check 的后端）。
//
// # 与管理后台「健康检测」的关系
//
// 检测逻辑**完全复用** `checkHealthMany`，只有两条差异，都写死在这里：
//
//  1. **只检测本人名下的号** —— 目标集是"请求的 uids ∩ 我的号"，别人的 uid 静默剔除
//     （报错会变成"这个 uid 存不存在"的探针）；
//  2. **不允许任何不可逆处置** —— 一律 `allowDelete=false`。后台那次点击是运维
//     显式发起的批量体检，允许按策略摘号；用户点的是"看看我的号怎么样"，
//     不该顺手把他的号停用/删除。号是用户的资产，处置权在管理员。
//
// # 为什么要做成插件
//
// 它天然是"可独立启停、可自己决定要不要"的功能（有些用户只关心出网，不关心体检）。
// 插件层给的是导航不膨胀 + 用户自己能关。
//
// # 出站强度
//
// 检测每个号会打上游（余额 + 可选对话探活），所以：
//   - 单次上限沿用 `healthCheckMaxAccounts`；
//   - `checkHealthMany` 内部**逐号串行**并带 250~600ms 抖动（本身就为此设计）；
//   - 本文件不提供"并发/全量轰炸"的入口。
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"
)

// handleHostHealthCheck POST /api/host/accounts/health
//
// body: {"uids":["..."]}（省略 = 我的全部代挂号）
//
// 异步受理：立刻返回"已开始"，结果通过 GET /api/host/accounts/health/last 取。
// 为什么异步：一个号要打上游两次（余额 + 对话），几十个号就是分钟级；
// 同步等会让浏览器超时，而超时在用户眼里等于"点了没反应"。
func (a *app) handleHostHealthCheck(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	if u == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "未登录"})
		return
	}
	var req struct {
		UIDs []string `json:"uids"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	owned, err := a.store.ListAccountsByOwner(u.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	own := make(map[string]bool, len(owned))
	for _, acc := range owned {
		own[acc.UID] = true
	}
	var targets []string
	if len(req.UIDs) == 0 {
		for _, acc := range owned {
			targets = append(targets, acc.UID)
		}
	} else {
		for _, uid := range req.UIDs {
			uid = strings.TrimSpace(uid)
			if uid != "" && own[uid] {
				targets = append(targets, uid)
			}
		}
	}
	if len(targets) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "没有可检测的账号（请确认勾选的账号属于你）"})
		return
	}
	if len(targets) > healthCheckMaxAccounts {
		targets = targets[:healthCheckMaxAccounts]
	}

	// allowDelete=false 是这里唯一不能改的参数（用户视角不做不可逆处置）。
	go func() {
		res := a.checkHealthMany(targets, false)
		healthLast.set(res, time.Now(), "user")
		log.Printf("INFO: [health] 用户 %s 主动检测 %d 个号（只读，不做处置）", u.Username, len(targets))
	}()

	writeJSON(w, http.StatusOK, map[string]any{
		"status": "started", "count": len(targets),
		"note": "已开始检测（逐个串行，每个号约 1~3 秒）。稍后点「刷新结果」查看；" +
			"检测只读，不会停用或删除任何账号",
	})
}

// handleHostHealthLast GET /api/host/accounts/health/last
//
// 只回**我的号**的检测结论：healthLast 是全局快照（管理员那次全量检测也在里面），
// 直接把全量结果给用户等于泄露别人的 uid / 昵称 / 余额。
func (a *app) handleHostHealthLast(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	if u == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "未登录"})
		return
	}
	owned, err := a.store.ListAccountsByOwner(u.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if len(owned) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"has_result": false, "note": "你还没有代挂账号",
			"accounts": []any{},
		})
		return
	}
	// 账号清单（含昵称）由我们自己的库给出，检测结果按 uid 合并 —— 这样即使
	// 快照里没有这个号（刚加进来还没检测过），列表里也能看到"未检测"。
	// row 一行 = 一个我的号 + 它最近一次的检测结论（可能没有）。
	//
	// 不内嵌 HealthCheckResult：它自带 uid/nickname，与外层同名字段会形成
	// "浅字段遮蔽深字段"的隐式规则 —— 那种规则在 json 里能跑，但读代码的人
	// 永远要停下来算一遍。显式放 result 子对象，前端渲染也简单。
	type row struct {
		UID      string `json:"uid"`
		Nickname string `json:"nickname,omitempty"`
		// TokenDead 登录已失效（12153 落库标记），比检测结论更早可见。
		TokenDead bool `json:"token_dead,omitempty"`
		// Checked 本次快照里有没有这个号的结论。
		Checked bool `json:"checked"`
		// Result 检测结论（nil = 还没检测过）。
		Result *HealthCheckResult `json:"result,omitempty"`
	}
	res, sum, has := healthLast.get()
	byUID := make(map[string]*HealthCheckResult, len(res))
	for _, r0 := range res {
		if r0 != nil {
			byUID[r0.UID] = r0
		}
	}
	dead := a.store.TokenDeadMap()
	out := make([]row, 0, len(owned))
	myChecked, myDefinite := 0, 0
	for _, acc := range owned {
		rw := row{UID: acc.UID, Nickname: acc.Nickname}
		if _, ok := dead[acc.UID]; ok {
			rw.TokenDead = true
		}
		if r0, ok := byUID[acc.UID]; ok {
			rw.Result = r0
			rw.Checked = true
			myChecked++
			if r0.Definite {
				myDefinite++
			}
		}
		out = append(out, rw)
	}

	pick := func(k string) any {
		if sum.ByState == nil {
			return 0
		}
		return sum.ByState[k]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"has_result": has && myChecked > 0,
		"accounts":   out,
		// 汇总只算**我的号**，不能把全局快照的计数透给用户（那里面有别人的号）。
		"summary": map[string]any{
			"total": len(out), "checked": myChecked, "definite": myDefinite,
			"started_at": sum.StartedAt, "ended_at": sum.EndedAt, "source": sum.Source,
			"by_state": map[string]any{
				"ok": pick("ok"), "abnormal": pick("abnormal"), "uncertain": pick("uncertain"),
			},
		},
		"note": "检测只读：不会停用、冷却或删除任何账号。需要处置请联系管理员",
	})
}
