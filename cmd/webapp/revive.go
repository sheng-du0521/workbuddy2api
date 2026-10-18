// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// revive.go 「探活复活」：给被判登录失效的号一条**可验证的**复活路（v0.31.39）。
//
// ## 为什么必须有这条路径
//
// v0.31.30 加了 `token_dead_at` 之后，一个号一旦被判失效，**就没有任何代码会再去
// 验证它**：健康检测不会清这个标记（它只累计 sessionDeadFails），重新登录才会，
// 而"重新登录"要求号主再扫一次码。于是判错的代价是永久的。
//
// 生产实测（2026-10-18）：38 个号里 10 个带着红牌，而它们的 `expires_at` 全在
// **一个月之后** —— access token 很可能仍然可用，只是 refresh 被上游轮换掉了。
// 上线本接口后对这批号跑了一次批量复活：**10/10 对话探活通过**，判据被证实。
//
// ⚠️ 一处必须记清的边界（否则会把危害说重）：红牌**当时并没有让它们停止服务** ——
// 12153 附带的硬冷却（次日 04:00）早已过期，池照常选中它们（实测其中 4 个当天
// 14:25~15:10 仍在正常出流量）。红牌的真实危害是另外三条：
//
//	① 界面/号主看到"登录失效"→ 误判、反复重登（而重登又会把对方的会话顶掉）；
//	② access 到期那天会**突然真失效**（refresh 已废，续不了），此前没有任何提前量；
//	③ 管理员点「启用」也清不掉这个标记 —— 它只认 `ClearTokenDead` 的三个入口。
//
// ## 口径（三条，缺一条就会变成"点一下全放回来"的不可信按钮）
//
//  1. **必须真探活**：复活依据只有一条 —— 对话通道（真正服务用户的那条）真的返回了
//     内容。余额通过、refresh 通过都不算（v0.24.3 的教训：30 个号余额 100% 通过、
//     对话 100% 被拦）。
//  2. **探活失败不做任何处置**：它已经是"被判死"的状态，再累计一次阈值毫无意义，
//     而误累计会把一个只是网络抖动的号推向停用/删除。
//  3. **复活后置 `refresh_lost`**：我们唯一掌握的证据是"它的 refresh 曾被 12153 判过"，
//     所以复活后的准确状态是**「仅剩 access」**，不是"一切正常"。等 access 到期前
//     保活刷新成功，`UpdateTokens` 会自动清掉这个橙标 —— 状态自愈，不需要人再点一次。
//
// ## 出站纪律
//
// 逐号串行 + 250~600ms 抖动（与 checkHealthMany 同节奏），并走 `a.probes` 探活总闸
// （全局并发 ≤3、同号 30s 一次）—— 复活是**批量动作**，最容易被做成一次并发风暴。
package main

import (
	"encoding/json"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"time"
)

// reviveResult 单号的复活结果（对外下发，字段名显式）。
type reviveResult struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname,omitempty"`
	// State 结果分类：revived / alive-no-mark / no-cred / probe-failed / busy / missing
	State string `json:"state"`
	Note  string `json:"note,omitempty"`
	// ProbeOK 对话探活是否通过（dry_run 时为 false 且未探活）。
	ProbeOK bool     `json:"probe_ok"`
	Actions []string `json:"actions,omitempty"`
}

// reviveCandidates 返回带「登录失效」标记的账号（批量复活的目标集合）。
//
// 只取 token_dead：`refresh_lost` 的号**还在服务**，去探活它们纯属白出站
// （每次都是一次上游调用），而且它们本来就没有需要清的牌子。
func (a *app) reviveCandidates() []string {
	dead := a.store.TokenDeadMap()
	out := make([]string, 0, len(dead))
	for _, acc := range a.mustListAccounts() {
		if _, ok := dead[acc.UID]; ok {
			out = append(out, acc.UID)
		}
	}
	return out
}

// mustListAccounts 列表读取失败时返回空切片（复活是运维动作，不该因一次读失败 500）。
func (a *app) mustListAccounts() []*Account {
	all, err := a.store.ListAccounts()
	if err != nil {
		log.Printf("WARN: [revive] 读取账号列表失败：%v", err)
		return nil
	}
	return all
}

// reviveOne 对单个号执行「探活 → 复活」。
//
// dryRun=true 时**完全不出站**：只回报"这个号是否带失效标记、有没有凭证"，
// 用来在真正动手前把影响面摆给运维看（数量 + 名单）。
func (a *app) reviveOne(uid string, dryRun bool) reviveResult {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return reviveResult{State: "missing", Note: "uid 为空"}
	}
	acc, err := a.store.GetAccount(uid)
	if err != nil || acc == nil {
		return reviveResult{UID: uid, State: "missing", Note: "账号不存在"}
	}
	res := reviveResult{UID: uid, Nickname: acc.Nickname}

	if _, isDead := a.store.TokenDeadMap()[uid]; !isDead {
		res.State = "alive-no-mark"
		res.Note = "该号没有「登录失效」标记，无需复活"
		return res
	}
	if dryRun {
		res.State = "candidate"
		res.Note = "将做一次对话探活；通过则清失效标记并置「仅剩 access」"
		return res
	}

	au := a.proxy.pool.AuthByUID(uid)
	if au == nil {
		res.State = "no-cred"
		res.Note = "池内取不到凭证（可能未同步），无法探活"
		return res
	}
	// 探活总闸：批量复活最容易变成并发风暴，这里是与余额查询共享的同一道门。
	release, gerr := a.probes.acquire(uid)
	if gerr != nil {
		res.State = "busy"
		res.Note = "探活闸门忙（同号 30s 内已探过或全局并发已满），稍后再试"
		return res
	}
	cp := a.hcChatProbe(au)
	release()
	res.ProbeOK = cp.OK
	if !cp.OK {
		// 探活失败：**只报告**。不累计 sessionDeadFails、不改标记、不冷却 ——
		// 它已经被排除在服务之外，再罚一次只会把"其实只是抖动"的号推向永久停用。
		res.State = "probe-failed"
		res.Note = "对话探活未通过：" + cp.Note
		return res
	}

	// 探活通过 = 这个号现在能干活。清标记 + 清一切可逆阻塞位。
	if err := a.store.ClearTokenDead(uid); err != nil {
		res.State = "probe-failed"
		res.Note = "探活通过但清标记失败：" + err.Error()
		return res
	}
	// 保留「仅剩 access」：我们唯一掌握的 refresh 证据是它曾被 12153 判过。
	// 如果它其实还好，下一次成功刷新会让 UpdateTokens 自动清掉这一位。
	if err := a.store.MarkRefreshLost(uid); err != nil {
		log.Printf("WARN: [revive] %s 置 refresh_lost 失败：%v", uid, err)
	} else {
		res.Actions = append(res.Actions, "refresh_lost")
	}
	if a.proxy.pool.Activate(uid) {
		res.Actions = append(res.Actions, "activate")
	}
	// 同步降到「只消费」（v0.31.44 修）。
	//
	// 与 handleSessionDead 的分档保持一致：走到这里说明这个号的 refresh 已被上游判废
	// （我们唯一的证据就是那次 12153），那它**不该继续主动刷新** —— 每刷一次都在撞
	// 一张废票，而按上游的轮换制，用废票重刷可能触发"旧票重放"判定，把对方新签发的
	// 那份也一起作废。
	//
	// 生产实测（2026-10-18）：复活后 10 个号都带 refresh_lost，但 refresh_mode 全是
	// auto —— 保活仍在每小时尝试刷新一次，日志里不断出现失败。两处口径不一致的典型。
	// 运维在号主退出本机客户端后可以点「恢复主动续期」切回 auto。
	if err := a.store.SetRefreshMode(uid, refreshModePassive); err != nil {
		log.Printf("WARN: [revive] %s 置「只消费」失败：%v", uid, err)
	} else {
		a.proxy.pool.SetPassiveRefresh(uid, true)
		res.Actions = append(res.Actions, "passive")
	}
	res.State = "revived"
	res.Note = "对话探活通过，已清失效标记；按「仅剩 access」提示在过期前重登一次即可完全恢复"
	return res
}

// handleSetRefreshMode POST /api/admin/accounts/{uid}/refresh-mode
//
// body: {"mode":"auto"|"passive"}
//
// 存在理由：检测到会话冲突后系统会自动降到 passive（只消费），但那是**保守方向**的
// 自愈。运维在"号主已经退出本机客户端"之后要能把号切回主动续期 —— 否则这个号就
// 只能靠人工重登续命。没有这个反向开关，自动降级就变成了单向棘轮。
func (a *app) handleSetRefreshMode(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimSpace(r.PathValue("uid"))
	if uid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing uid"})
		return
	}
	var req struct {
		Mode string `json:"mode"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	mode := normRefreshMode(req.Mode)
	if err := a.store.SetRefreshMode(uid, mode); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	a.proxy.pool.SetPassiveRefresh(uid, mode == refreshModePassive)
	log.Printf("INFO: [refresh] %s 刷新模式 → %s（管理员操作）", uid, mode)
	writeJSON(w, http.StatusOK, map[string]any{
		"uid": uid, "mode": mode,
		"note": "auto = 主动续期；passive = 只消费 access、不主动刷新（到期需重登）",
	})
}

// handleReviveAccount POST /api/admin/accounts/{uid}/revive
//
// 单号复活。对**已确认可服务**的号执行；不可服务的号原样返回失败原因，不改任何状态。
func (a *app) handleReviveAccount(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if strings.TrimSpace(uid) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing uid"})
		return
	}
	res := a.reviveOne(uid, false)
	log.Printf("INFO: [revive] %s → %s（%s）", uid, res.State, res.Note)
	writeJSON(w, http.StatusOK, map[string]any{"result": res})
}

// handleReviveDead POST /api/admin/accounts/revive-dead
//
// 批量复活。body: {"uids":[...],"dry_run":true}
//
// **dry_run 默认 true**：这条路径会真的打上游（每号一次对话），批量动作必须让人
// 先看到"要动几个号"，否则一次误点就是一次号池规模的出站。
func (a *app) handleReviveDead(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UIDs   []string `json:"uids"`
		DryRun *bool    `json:"dry_run"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	dry := true
	if req.DryRun != nil {
		dry = *req.DryRun
	}
	targets := req.UIDs
	if len(targets) == 0 {
		targets = a.reviveCandidates()
	}
	// 上限与体检一致：防止一次点击打出上百个上游请求。
	if len(targets) > healthCheckMaxAccounts {
		targets = targets[:healthCheckMaxAccounts]
	}
	out := make([]reviveResult, 0, len(targets))
	for i, uid := range targets {
		if !dry && i > 0 {
			// 串行 + 抖动：复活的出站强度必须低于"批量刷新"，否则它自己就是风控特征。
			time.Sleep(time.Duration(250+rand.Intn(350)) * time.Millisecond)
		}
		out = append(out, a.reviveOne(uid, dry))
	}
	sum := map[string]int{}
	for _, r := range out {
		sum[r.State]++
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"dry_run": dry,
		"count":   len(out),
		"summary": sum,
		"results": out,
		"note":    "dry_run 只列候选、不出站；真正执行时逐号做对话探活，通过才清标记",
	})
}
