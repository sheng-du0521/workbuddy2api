// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// devicetoken_ops.go 每号设备凭证（X-Device-Token）的**写入通道** + 「共用一个即关联」审计（R2，v0.26.1）。
//
// ## 为什么需要这一层
//
// `accounts.device_token` 列一直存在，`authFromAccount` 也一直把它传给出站实现 ——
// 但**从来没有任何接口能把它写进去**。结果是这条最有逆向证据的风控头长期空置
// （审计实测「全池 0/27 非空」），上游看到的是一台「没有设备凭证的客户端」。
//
// ## 为什么不做「按 uid 派生一个假 token」兜底
//
// 一度考虑过用 `deriveAccountStableID`（md5(purpose:uid)）给每号编一个兜底值。
// 否决了，原因有两条，任何一条都足够：
//
//  1. **X-Device-Token 是上游 SDK 下发的凭证，不是客户端自编的设备 ID**。
//     官方客户端由 Turing Shield SDK 取得该值（本包 device_token.go 的注释已记录：
//     容器内无 SDK，只能由宿主把桌面端落盘的 token 喂进来）。自编一个格式对不上、
//     也没被上游签发过的串，很可能直接被判异常 —— **比不带更糟**。
//  2. 同类项目（Zhengyuuuui）三轮抓包 + 二进制层验证的结论也是「必须注入**真实**值」，
//     并且明确要求「**一设备一账号，勿共用**」。
//
// 所以本文件的定位是：**把真实值录进来的通道 + 共用检测**，而不是伪造。
//
// ## 「共用一个」为什么比「什么都不带」更糟
//
// 全池都不带头 = 一台「没有设备凭证的客户端」（一类可聚类特征）。
// 全池共用一个头 = 27 个账号全部声称自己是**同一台设备**（另一类、且**更强**的关联
// 特征 —— 上游只要按该头聚合就能一次性看到整池）。两者形态不同，处置也不同：
// 前者需要往「每号独立」补齐，后者需要**立刻拆开**。
package main

import (
	"workbuddy2api/internal/upstream"

	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"
)

// deviceTokenFP 设备凭证的短指纹（sha256 前 8 字节 hex）。
//
// 只用于**审计比对**（判断两个账号是不是同一个凭证），绝不回显原文 ——
// 它是风控凭证，等同于敏感串（与全局值的处理口径一致：只回 *_set 布尔）。
func deviceTokenFP(token string) string {
	t := strings.TrimSpace(token)
	if t == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:8])
}

// SetAccountDeviceToken 写入账号级设备凭证。空串 = 清除（回落到全局兜底 / 不注入）。
//
// 单独成方法而不是复用某个通用 update：`accounts.device_token` 是**风控凭证**，
// 写入口要显式、可审计，不能混在一堆可随意覆盖的字段里。
func (s *Store) SetAccountDeviceToken(uid, token string) error {
	now := time.Now().Unix()
	// device_token_at 只在**真的写入了凭证**时刷新；清除（空串）时归零，
	// 表示"没有凭证"，从而不会被陈旧检测误判为"很久以前录的凭证"。
	at := now
	tok := strings.TrimSpace(token)
	if tok == "" {
		at = 0
	}
	_, err := s.db.Exec(
		`UPDATE accounts SET device_token = ?, device_token_at = ?, updated_at = ? WHERE uid = ?`,
		tok, at, now, uid)
	return err
}

// deviceTokenRow 审计视图里的一行（绝不含 token 原文）。
type deviceTokenRow struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	// State 五态：account / stale / derived / global / none（见 devicecred.go）。
	State string `json:"state"`
	// FP 凭证指纹（account / stale 时非空；derived 时也给指纹 —— 它虽是合成值，
	// 但"各号指纹互不相同"这一点必须能被验证，否则派生兜底等于没做隔离）。
	FP string `json:"fp,omitempty"`
	// TokenLen 长度可用于判断"填了但明显不对"。
	TokenLen int `json:"token_len"`
	// AgeDays 凭证距今天数（stale 时非零；account 时也给出，供运维判断"快到期了"）。
	AgeDays int `json:"age_days,omitempty"`
}

// handleAccountDeviceToken POST /api/admin/accounts/{uid}/device-token
//
// body: {"token":"<凭证原文>"}；传空串 = 清除。
// 返回**不含 token 原文**，只回指纹与长度，便于核对是否写对。
func (a *app) handleAccountDeviceToken(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if uid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing uid"})
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
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
	tok := strings.TrimSpace(req.Token)
	// 与 upstream readDeviceTokenFile 的上限同口径（>1KB 视为非 token 内容）。
	if len(tok) > 1024 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "设备凭证过长（>1024 字节）—— 疑似粘错了内容（token 通常几百字节）"})
		return
	}
	if err := a.store.SetAccountDeviceToken(uid, tok); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// 出站取值由 authFromAccount 每次请求现读库，但还要过 Proxy 的账号同步，
	// 这里同步一次，保证「刚写的凭证下一条请求就生效」。
	a.proxy.ReloadSelection()
	// 写完回读一次，让返回值就是**库里真实生效**的口径（而不是本次请求的入参）。
	fresh, _ := a.store.GetAccount(uid)
	if fresh == nil {
		fresh = acc
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"uid":       uid,
		"token_set": tok != "",
		"token_len": len(tok),
		"fp":        deviceTokenFP(tok),
		"state":     a.deviceTokenState(fresh),
	})
}

// handleDeviceTokenAudit GET /api/admin/device-tokens
//
// 把「这台网关对外呈现成几台设备」做成一个可读结论：五态计数 + 共用分组 + 结论文案。
// （五态 = 真凭证 / 真凭证·陈旧 / 派生兜底 / 靠全局兜底 / 无凭证，见 devicecred.go）
func (a *app) handleDeviceTokenAudit(w http.ResponseWriter, r *http.Request) {
	accs, err := a.store.ListAccounts()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	var rows []deviceTokenRow
	byState := map[string]int{"account": 0, "stale": 0, "derived": 0, "global": 0, "none": 0}
	groups := map[string][]string{} // fp → uids（含全局兜底那一组，用固定键）
	for _, acc := range accs {
		st := a.deviceTokenState(acc)
		byState[st]++
		row := deviceTokenRow{UID: acc.UID, Nickname: acc.Nickname, State: st}
		switch st {
		case "account", "stale":
			row.FP = deviceTokenFP(acc.DeviceToken)
			row.TokenLen = len(strings.TrimSpace(acc.DeviceToken))
			if acc.DeviceTokenAt > 0 {
				row.AgeDays = int(time.Since(time.Unix(acc.DeviceTokenAt, 0)).Hours() / 24)
			}
			groups[row.FP] = append(groups[row.FP], acc.UID)
		case "derived":
			// 派生值也纳入指纹分组：它是"按 uid 派生"的，若实现出错（例如用途盐
			// 写成了常量）会让全池派生出同一个值 —— 那正是本审计最该抓的形态。
			row.FP = deviceTokenFP(upstream.AccountDeviceToken(acc.UID))
			row.TokenLen = len(row.FP)
			groups[row.FP] = append(groups[row.FP], acc.UID)
		case "global":
			// 全局兜底：所有落在这一态的账号**共用同一个凭证**，天然是一组。
			groups["\x00global"] = append(groups["\x00global"], acc.UID)
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].State < rows[j].State })

	// 共用分组（>1 个账号同一个凭证）—— 这是本审计真正要抓的东西。
	type sharedGroup struct {
		FP    string   `json:"fp"`
		Kind  string   `json:"kind"` // account / global
		Count int      `json:"count"`
		UIDs  []string `json:"uids"`
	}
	var shared []sharedGroup
	for fp, uids := range groups {
		if len(uids) <= 1 {
			continue
		}
		if fp == "\x00global" {
			shared = append(shared, sharedGroup{FP: "", Kind: "global", Count: len(uids), UIDs: uids})
			continue
		}
		shared = append(shared, sharedGroup{FP: fp, Kind: "account", Count: len(uids), UIDs: uids})
	}
	sort.Slice(shared, func(i, j int) bool { return shared[i].Count > shared[j].Count })

	total := len(accs)
	// 结论文案（给运维直接看的，不用自己推）。
	verdict := "ok"
	advice := ""
	switch {
	case total == 0:
		verdict, advice = "empty", "池内没有账号。"
	case byState["none"] == total:
		verdict = "none"
		advice = "全池都不带 X-Device-Token：上游看到的是一台「没有设备凭证的客户端」。" +
			"这是一类稳定差异 —— 建议给每号各录一份真凭证；若暂时拿不到，" +
			"可打开「派生兜底」（设置里 device_token_fallback=derive）先让每号有一个互异值，" +
			"至少消掉「全池同一形态」这个特征。"
	case byState["global"] > 0 && byState["account"] == 0:
		verdict = "shared-global"
		advice = "全池共用同一个全局兜底凭证：等于 **27 个账号声称自己是同一台设备**，" +
			"比不带更容易被按该头聚合。处置：把全局兜底清掉，改为逐号录入。"
	case byState["stale"] > 0:
		verdict = "has-stale"
		advice = "有 " + itoa(byState["stale"]) + " 个账号的凭证已超过 " + itoa(a.deviceTokenStaleDays()) +
			" 天未更新。陈旧 ≠ 失效，所以**没有**自动替换（用合成值顶替真值是确定性的降级）；" +
			"但如果这些号的请求开始被上游按客户端特征挡下，优先重新采集这几号的凭证。"
	case byState["derived"] > 0 && byState["account"]+byState["stale"]+byState["global"] == 0:
		verdict = "derived-only"
		advice = "全池用的是**按 uid 派生的合成值**（不是上游签发的真凭证）：各号互异、" +
			"不会被按单值聚类，但值本身上游从未签发过。这是「有总比没有好」的兜底；" +
			"拿到真凭证后应逐号覆盖（覆盖后该号自动转为 account 态）。"
	case len(shared) > 0:
		verdict = "partially-shared"
		advice = "存在共用同一凭证的账号分组，建议拆开（一设备一账号）。"
	default:
		advice = "每号一份独立凭证，隔离性最好。"
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"total":        total,
		"by_state":     byState,
		"shared":       shared,
		"rows":         rows,
		"verdict":      verdict,
		"advice":       advice,
		"distinct_fps": len(groups),
		// 回显生效中的两个开关：界面要能解释"为什么是这几档"，
		// 否则运维看到 derived 态会以为程序擅自造了凭证。
		"fallback_on": a.store.deviceTokenFallbackOn(),
		"stale_days":  a.deviceTokenStaleDays(),
	})
}

// handleDeviceTokenBulk POST /api/admin/device-tokens/bulk
//
// body: {"text":"<每行一条：账号选择符 <空白> 凭证>"}
// 账号选择符可用 **完整 uid / uid 前 8 位 / 昵称**（三者都唯一即可）。
// 以 `#` 开头的行与空行忽略。逐行返回结果，**一行失败不影响其它行**（运维一次贴几十行时不能全废）。
func (a *app) handleDeviceTokenBulk(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	accs, err := a.store.ListAccounts()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// 选择符 → uid 的解析表（uid 全量 + 前 8 位 + 昵称；重名/前缀撞车一律视为歧义）。
	bySel := map[string]string{}
	ambiguous := map[string]bool{}
	add := func(sel, uid string) {
		if sel == "" {
			return
		}
		if prev, ok := bySel[sel]; ok && prev != uid {
			ambiguous[sel] = true
			return
		}
		bySel[sel] = uid
	}
	for _, acc := range accs {
		add(acc.UID, acc.UID)
		if len(acc.UID) >= 8 {
			add(acc.UID[:8], acc.UID)
		}
		add(acc.Nickname, acc.UID)
	}

	type lineResult struct {
		Line    int    `json:"line"`
		Input   string `json:"input"`
		UID     string `json:"uid,omitempty"`
		OK      bool   `json:"ok"`
		Error   string `json:"error,omitempty"`
		TokenFP string `json:"fp,omitempty"`
	}
	var results []lineResult
	applied := 0
	lines := strings.Split(req.Text, "\n")
	for i, raw := range lines {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		res := lineResult{Line: i + 1, Input: truncateForEcho(line)}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			res.Error = "格式应为「账号标识 凭证」（用空白分隔）"
			results = append(results, res)
			continue
		}
		sel := fields[0]
		tok := strings.Join(fields[1:], " ")
		if ambiguous[sel] {
			res.Error = "账号标识有歧义（多个账号匹配），请改用完整 uid"
			results = append(results, res)
			continue
		}
		uid, ok := bySel[sel]
		if !ok {
			res.Error = "找不到账号：" + sel
			results = append(results, res)
			continue
		}
		if len(tok) > 1024 {
			res.Error = "凭证过长（>1024 字节）"
			results = append(results, res)
			continue
		}
		if err := a.store.SetAccountDeviceToken(uid, tok); err != nil {
			res.Error = err.Error()
			results = append(results, res)
			continue
		}
		res.UID, res.OK, res.TokenFP = uid, true, deviceTokenFP(tok)
		applied++
		results = append(results, res)
	}
	if applied > 0 {
		a.proxy.ReloadSelection()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"applied": applied,
		"total":   len(results),
		"results": results,
	})
}

// truncateForEcho 把回显行截断 —— 不允许凭证原文出现在响应里（含错误路径）。
func truncateForEcho(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	if len(f) == 1 {
		return truncateRunes(f[0], 16)
	}
	return truncateRunes(f[0], 16) + " …(凭证已隐藏)"
}
