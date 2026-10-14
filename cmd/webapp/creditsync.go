// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// creditsync.go 号池余额拉取（v0.17.8）。
//
// 解决的问题：**导入新账号后，号池仪表盘的「积分合计」不更新**。
//
// 根因：池内的 credits 是**内存快照**，只在两个时机被写入 ——
//  1. 签到（DailyCheckin + UserResourceDetailed → SetCreditsDetailed）；
//  2. 每次成功请求按上游回报的消耗实时扣减（v0.17.6 起）。
//
// 新导入的账号两头都没赶上：它没参与过签到（要等到 9/21 点的整轮），也没被
// 任何请求服务过 → 池内余额恒为 0，仪表盘自然把它算成 0，合计看起来「没变」。
// 而账号卡上的「积分面板」是**按需实时查询**的，所以卡片上有数字、仪表盘没有
// ——两边不一致，非常容易被读成「仪表盘坏了」。
//
// 修法：导入完成后**立刻**为这批新号拉一次余额（后台异步、账号间错开），
// 把它们对齐到上游权威值。仪表盘每 3 秒轮询，所以数字会在若干秒内陆续出现。
//
// 同样的思路也提供给管理员一个手动入口（POST /api/admin/credits/refresh），
// 用于「账号是在导入之外加的」「怀疑余额老化」等场景。
//
// v0.23.0 起：这条路径**同时承担健康检测** —— 拉余额用的接口就是探活接口，
// 一次出站两用（见 refreshCredits 的注释），检测结论进「最近一次检测」快照。
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"workbuddy2api/internal/pool"
)

// needRefreshCredits 该账号是否需要拉取上游积分（候选集判据，v0.20.0 抽为纯函数）。
//
// 判据（任一成立即需要刷新）：
//  1. `CreditsSeenAt == 0` —— **积分从未获取**。这是 v0.20.0 新增的判据，也是本次
//     缺陷的修复点：creditsSeenAt 是新字段，老 state.json 没有它 → 升级后全部账号
//     未打戳，而它们的 credits 又是非 0 的历史快照；旧判据 `Credits != 0 → 跳过`
//     会把它们全部排除，导致「未获取积分」永远刷不掉（用户点刷新得到 noop）。
//  2. `Credits == 0` —— 池内余额为 0，既可能是真没钱也可能是没拉过，刷一次可分辨。
//
// 停用（Disabled）/ 运维手动摘除（ManualDisabled）的号一律不刷（与选号口径一致，
// 这两种都不该再出站）。
//
// 抽成纯函数的好处：所有组合都能被单测直接覆盖，不必构造真实池状态
// （「未打戳 + 非 0 余额」这种升级态在测试里很难凭空造出来）。
func needRefreshCredits(st pool.Status) bool {
	if st.Disabled || st.ManualDisabled {
		return false
	}
	return st.CreditsSeenAt == 0 || st.Credits == 0
}

// creditsRefreshMaxAccounts 单次刷新最多处理多少个账号。
//
// 存在的意义是给「手动全量刷新」兜底：每个账号一次上游请求，账号间还要错开
// 数百毫秒，不做上限时一次点击可能打出上百个请求、跑上几分钟。
// 导入路径不受影响（它只刷刚导入的那批，通常是个位数到几十）。
const creditsRefreshMaxAccounts = 60

// refreshCredits 为给定 uid 逐个拉取上游余额并写回池内。
//
// 返回 (成功数, 失败数)。失败不重试、不中断 —— 与任务链路同一纪律：
// 连续失败通常意味着 token 失效或被风控，重试只会把失败写得更密。
//
// **v0.23.0 起本函数是 checkHealthMany 的薄封装**：拉余额用的接口
// （get-user-resource）与健康检测的探活接口**是同一个** —— 一次出站既取回权威
// 余额，又判定账号是否可用。此前两者各写一套循环（本函数与 healthcheck.go），
// 收尾动作（写余额 / 解冻 / 重算预备组）也会各写一遍，早晚分叉；合并后
// 「导入即检测」，不额外产生任何上游请求。
//
// ok = 本次探测拿到权威余额（凭证可用）；fail = 没拿到（含凭证缺失、上游报错）。
func (a *app) refreshCredits(uids []string) (ok, fail int) {
	// allowDelete=false：**观测型调用** —— 刷新积分只回报状态，绝不删除账号
	// （v0.24.8）。删除是不可逆的，不能由"刷一下余额"顺带完成。
	// 错峰在 checkHealthMany 内部完成（250~600ms/账号），此处不再重复。
	for _, r := range a.checkHealthMany(uids, false) {
		if r.CreditsKnown {
			ok++
		} else {
			fail++
			log.Printf("credits refresh %s: state=%s reason=%s", r.UID, r.State, r.Reason)
		}
	}
	return ok, fail
}

// refreshCreditsAsync 后台刷新（导入后用）。传 nil uids 表示「刷新全部候选」。
//
// 一定要异步：导入接口要立刻返回，而 N 个账号 × 数百毫秒错开 是秒级到几十秒的
// 工作量；同步做会让导入请求超时。
//
// 结果**同时写进「最近一次检测」快照**（source=import）：导入几个已失效的号时，
// 管理员在账号池页的检测面板就能直接看到是哪几个、什么原因 —— 否则这些号被检测
// 出来异常后只落一条日志，用户会觉得「我刚导入的号怎么没进去」。
func (a *app) refreshCreditsAsync(uids []string) {
	go func() {
		targets := uids
		if len(targets) == 0 {
			targets = a.creditRefreshCandidates()
		}
		if len(targets) == 0 {
			return
		}
		started := time.Now()
		// 直接走检测批（而不是 refreshCredits）—— 这样只需跑一趟就能拿到逐条结果。
		// allowDelete=false：导入后的刷余额同样是**观测型调用**，绝不删除账号
		// （v0.24.8）——"我刚导入的号怎么没了"是这条路径此前最坏的表现。
		res := a.checkHealthMany(targets, false)
		healthLast.set(res, started, "import")
		ok, fail := 0, 0
		for _, r := range res {
			if r.CreditsKnown {
				ok++
			} else {
				fail++
			}
		}
		log.Printf("credits refresh done: ok=%d fail=%d (共 %d 个)", ok, fail, len(targets))
	}()
}

// creditRefreshCandidates 需要刷新的账号集合（用于「刷新全部」）。
//
// 判据见 needRefreshCredits：**积分未获取**或**池内余额为 0** 的可用账号。
func (a *app) creditRefreshCandidates() []string {
	var out []string
	for _, st := range a.proxy.pool.List() {
		if !needRefreshCredits(st) {
			continue
		}
		if a.proxy.pool.AuthByUID(st.UID) == nil {
			continue
		}
		out = append(out, st.UID)
		if len(out) >= creditsRefreshMaxAccounts {
			break
		}
	}
	return out
}

// handleRefreshCredits 手动刷新号池余额。
// POST /api/admin/credits/refresh  body: {"uids":["..."]}（省略 / 空 = 刷新候选集）
//
// 异步受理（与定时任务的手动触发同语义）：返回「已开始 + 目标数量」，
// 结果看仪表盘 —— 余额是池内状态，本来就没有单独的「回执」需要等。
func (a *app) handleRefreshCredits(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UIDs []string `json:"uids"`
	}
	// 解析失败按「未提供 uids」处理 —— 这个接口的 body 是可选的（空 body = 刷新候选集），
	// 不该因为客户端没带 Content-Type 就直接报错。
	_ = json.NewDecoder(r.Body).Decode(&req)
	targets := req.UIDs
	if len(targets) == 0 {
		targets = a.creditRefreshCandidates()
	}
	if len(targets) > creditsRefreshMaxAccounts {
		targets = targets[:creditsRefreshMaxAccounts]
	}
	if len(targets) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "noop", "count": 0,
			"note": "没有需要刷新的账号（全部账号的积分都已获取且余额非 0）",
		})
		return
	}
	a.refreshCreditsAsync(targets)
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "started",
		"count":  len(targets),
		"note":   "已在后台开始拉取余额（账号之间错开数百毫秒），数秒后刷新页面即可看到",
	})
}

// countUnknownCredits 统计「积分未获取」（creditsSeenAt == 0）的可用账号数。
// 供管理端展示「N 个账号积分未获取，点此刷新」这类明确提示。
func (a *app) countUnknownCredits() int {
	n := 0
	for _, st := range a.proxy.pool.List() {
		if st.Disabled || st.ManualDisabled {
			continue
		}
		if st.CreditsSeenAt == 0 {
			n++
		}
	}
	return n
}
