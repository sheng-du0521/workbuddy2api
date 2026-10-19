// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/upstream"
)

// ======================================================================
// 用户代挂（hosting）
//
// 概念：
//   - 平台账号（owner_user_id == 0）：管理员 OAuth 添加，恒参与平台反代。
//   - 代挂账号（owner_user_id > 0）：用户自己在控制台 OAuth 添加，归属该用户。
//     用处一：号主自用 Key（kind=host）只路由到自己的代挂号，且不扣余额；
//     用处二：号主可「申请进池」→ 管理员审批 → 进池后参与平台反代，
//             按「本次消耗的号池积分 × 分成比例」折入号主余额（见 proxy.settleUsage）。
// ======================================================================

// hostAccountView 代挂账号视图（用户侧与管理侧共用）。
type hostAccountView struct {
	UID        string `json:"uid"`
	Nickname   string `json:"nickname"`
	Realm      string `json:"realm"`
	HostGroup  string `json:"host_group"`
	PoolState  string `json:"pool_state"`
	HostNote   string `json:"host_note,omitempty"`
	Disabled   bool   `json:"disabled"`
	Cooling    bool   `json:"cooling"`
	CoolingFor int64  `json:"cooling_for"`
	Credits    int64  `json:"credits"`
	// CreditsKnown 池内是否已有该账号的**权威积分读数**（v0.23.1）。
	// 前端据此区分「积分未知（需先同步一次）」与「积分不足门槛」—— 两者给用户的
	// 提示与可采取的动作完全不同：前者点开积分面板即自动同步，后者只能等积分涨上去。
	CreditsKnown bool    `json:"credits_known"`
	ExpiresAt    int64   `json:"expires_at"`
	Earned       float64 `json:"earned"`
	CreatedAt    int64   `json:"created_at"`
	// ---- 已挂载时长（v0.26.7）----
	// 与管理后台同口径（mounted.go）：从入库时刻起算的自然日数。
	// 号主最关心的就是"我这个号挂了多久"，而 CreatedAt 是原始时间戳，
	// 让前端各自算天数必然与管理侧漂移（跨零点差一天）。
	MountedAt    int64  `json:"mounted_at"`
	MountedDays  int    `json:"mounted_days"`
	MountedText  string `json:"mounted_text"`
	MountedKnown bool   `json:"mounted_known"`
	// Task 该账号的定时任务开关（v0.11.0）。
	Task accountTaskState `json:"task"`
	// 归属（管理侧展示号主；用户侧留空）。
	OwnerID   int64  `json:"owner_id,omitempty"`
	OwnerName string `json:"owner_name,omitempty"`
	// 计费口径（v0.12.0，需求 7）：该账号被消耗的号池积分 / 据此收取的点数。
	ConsumedCredit float64 `json:"consumed_credit"`
	ChargedPoints  float64 `json:"charged_points"`
	ServedRequests int64   `json:"served_requests"`
	// EarnedCNY 累计收益的金额形态（v0.17.7）：收益属「返点」，一律以钱为主展示。
	EarnedCNY float64 `json:"earned_cny"`
	// 出站代理（v0.14.0）：Egress 为实际生效值；EgressError 非空表示配置有误、
	// 该账号已被排除出平台选号（避免从本机 IP 裸奔）。
	Egress      string `json:"egress,omitempty"`
	EgressError string `json:"egress_error,omitempty"`
	// 消耗分组（v0.13.0）：管理员设定，决定该账号是否参与平台反代消耗。
	ConsumeGroup          string `json:"consume_group"`
	ConsumeGroupEffective string `json:"consume_group_effective"`
	ConsumeGroupText      string `json:"consume_group_text"`
	ConsumeGroupAuto      bool   `json:"consume_group_auto"`
	ConsumeGroupNote      string `json:"consume_group_note,omitempty"`
	// CreditPacks / CreditPacksAt 上次成功查询到的逐套餐明细与时刻（v0.24.9）。
	// 本地缓存 → 卡片渲染**零出站**即可列出「哪些包、哪天到期」；
	// 点「刷新明细」才真实出站（仍受探活总闸约束）。详见 creditcache.go。
	CreditPacks   []upstream.CreditPackage `json:"credit_packs,omitempty"`
	CreditPacksAt int64                    `json:"credit_packs_at,omitempty"`
	// CreditCachedRemain 缓存明细的可用合计（v0.25.0，排除已过期包）——
	// 卡片大数字用它，保证与明细行自洽。CreditPacksAt > 0 才有效。
	CreditCachedRemain int64 `json:"credit_cached_remain"`
	// DeviceTokenState 见 accountView 同名字段（v0.25.1 起；v0.26.3 扩为五态）：
	// account / stale / derived / global / none。
	DeviceTokenState string `json:"device_token_state"`
}

// hostView 单账号代挂视图（积分明细缓存按主键现查一次）。
func (a *app) hostView(acc *Account) hostAccountView {
	return a.hostViewP(acc, a.store.AccountCreditPacks(acc.UID))
}

// hostViewP 带明细缓存的代挂视图（列表路径用：缓存已批量载入，避免 N+1 查询）。
func (a *app) hostViewP(acc *Account, cp cachedCreditPacks) hostAccountView {
	name := acc.Nickname
	if name == "" {
		name = acc.UID
	}
	v := hostAccountView{
		UID:       acc.UID,
		Nickname:  name,
		Realm:     acc.Realm,
		HostGroup: acc.HostGroup,
		PoolState: acc.PoolState,
		HostNote:  acc.HostNote,
		Disabled:  acc.Disabled,
		ExpiresAt: acc.ExpiresAt,
		Earned:    MicroToPoint(acc.EarnedMicro),
		CreatedAt: acc.CreatedAt,
		OwnerID:   acc.OwnerUserID,
		Task:      accountTaskStateOf(parseAccountTaskCfg(acc.TaskCfg)),

		ConsumeGroup:          acc.ConsumeGroup,
		ConsumeGroupEffective: effectiveConsumeGroup(acc),
		ConsumeGroupText:      consumeGroupLabel(effectiveConsumeGroup(acc)),
		ConsumeGroupAuto:      isAutoArchived(acc),
		ConsumeGroupNote:      acc.ConsumeGroupNote,

		Egress:      a.proxy.EgressForUID(acc.UID),
		EgressError: a.proxy.EgressErrorForUID(acc.UID),
	}
	v.EarnedCNY = pointsToCNY(v.Earned, a.moneyRate())
	// 已挂载时长：与管理后台同口径（mounted.go）。
	mi := mountedOf(acc.CreatedAt, time.Now())
	v.MountedAt, v.MountedDays, v.MountedText, v.MountedKnown = mi.At, mi.Days, mi.Text, mi.Known
	if st, ok := a.proxy.pool.Status(acc.UID); ok {
		v.Credits = st.Credits
		v.CreditsKnown = st.CreditsSeenAt > 0
		v.Cooling = st.Cooling
		v.CoolingFor = st.CoolRemaining
		if v.CoolingFor < 0 {
			v.CoolingFor = 0
		}
		if st.Nickname != "" {
			v.Nickname = st.Nickname
		}
	}
	v.CreditPacks, v.CreditPacksAt = cp.Packs, cp.At
	if cp.At > 0 {
		v.CreditCachedRemain = cachedRemain(cp, time.Now().Unix())
	}
	v.DeviceTokenState = a.deviceTokenState(acc)
	return v
}

// ---------------------------------------------------------------------
// 用户侧
// ---------------------------------------------------------------------

// handleHostOAuthStart 用户起一次代挂授权（复用设备授权流程）。
func (a *app) handleHostOAuthStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Realm string `json:"realm"`
		// UID 可选：从某张账号卡点「重新登录」时带上，让登录沿用该号的设备身份
		// （重登前后身份连续，见 oauthDeviceIdentity）。只接受**本人名下**的号 ——
		// 这个参数会进入上游请求头，不能成为探测别人账号是否存在的手段。
		UID string `json:"uid"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	hint := ""
	if u := userFrom(r); u != nil && strings.TrimSpace(req.UID) != "" {
		if acc, err := a.store.GetAccount(req.UID); err == nil && acc != nil && acc.OwnerUserID == u.ID {
			hint = acc.UID
		}
	}
	sess, err := StartOAuth(req.Realm, hint)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": sess.ID,
		"auth_url":   sess.AuthURL,
		"realm":      sess.Realm,
	})
}

// handleHostOAuthPoll 轮询代挂授权，成功后账号归属当前用户。
func (a *app) handleHostOAuthPoll(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	a.pollOAuth(w, r.URL.Query().Get("sid"), u.ID)
}

// handleHostAccounts 我的代挂账号 + 代挂分组 + 收益摘要。
func (a *app) handleHostAccounts(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	list, err := a.store.ListAccountsByOwner(u.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	usageByUID, _ := a.store.UsageByAccount()
	// 积分明细缓存（v0.24.9）：批量载入，卡片零出站即可展示明细。
	creditByUID := a.store.CreditPacksByUID()
	views := make([]hostAccountView, 0, len(list))
	groups := map[string]int{}
	for _, acc := range list {
		v := a.hostViewP(acc, creditByUID[acc.UID])
		if us, ok := usageByUID[acc.UID]; ok {
			v.ConsumedCredit = us.Credit
			v.ChargedPoints = MicroToPoint(us.Charged)
			v.ServedRequests = us.Requests
		}
		views = append(views, v)
		groups[acc.HostGroup]++
	}
	totalMicro, count, _ := a.store.HostEarningSummary(u.ID)
	rate := a.moneyRate()
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": views,
		"groups":   groups,
		"rate":     rate,
		"summary": map[string]any{
			"total":      len(views),
			"in_pool":    countPool(views, "in"),
			"pending":    countPool(views, "pending"),
			"earned":     MicroToPoint(totalMicro),
			"earned_cny": microToCNY(totalMicro, rate),
			"earnings":   count,
		},
	})
}

func countPool(vs []hostAccountView, state string) int {
	n := 0
	for _, v := range vs {
		if v.PoolState == state {
			n++
		}
	}
	return n
}

// requireOwnedAccount 取本人在册的代挂账号；不存在/非本人返回 nil。
func (a *app) requireOwnedAccount(w http.ResponseWriter, uid string, userID int64) *Account {
	acc, err := a.store.GetAccount(uid)
	if err != nil || acc == nil || acc.OwnerUserID != userID {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "账号不存在或不属于你"})
		return nil
	}
	return acc
}

// handleHostSetGroup 设置代挂分组。
func (a *app) handleHostSetGroup(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	uid := r.PathValue("uid")
	if a.requireOwnedAccount(w, uid, u.ID) == nil {
		return
	}
	var req struct {
		Group string `json:"group"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	g := strings.TrimSpace(req.Group)
	if len(g) > 24 {
		g = g[:24]
	}
	if err := a.store.SetAccountHostGroup(uid, g); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"uid": uid, "group": g})
}

// poolCredits 该账号池内可用积分与「是否已有权威读数」（v0.20.0）。
//
// 第二个返回值很重要：积分为 0 既可能是「真没积分」也可能是「从没拉过」。
// 入池门槛与交易门槛都必须区分二者 —— 把「没拉过」当成「不合格」会误拒正常
// 账号；反过来把「没拉过」当成合格又会放过无额度的号。故 `!known` 一律拒绝，
// 让用户先刷一次余额再操作。
func (a *app) poolCredits(uid string) (credits int64, known bool) {
	st, ok := a.proxy.pool.Status(uid)
	if !ok {
		return 0, false
	}
	return st.Credits, st.CreditsSeenAt > 0
}

// handleHostPool 代挂账号入池动作：apply（申请）/ cancel（撤回申请）/ leave（退出号池）。
//
// v0.20.0 新增入池积分门槛：申请时账号池内积分必须 >= poolApplyMinCredits(2000)。
// 低于门槛的号不放进平台号池 —— 既占审批带宽，又会在池中被反复消耗到见底。
func (a *app) handleHostPool(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	uid := r.PathValue("uid")
	acc := a.requireOwnedAccount(w, uid, u.ID)
	if acc == nil {
		return
	}
	var req struct {
		Action string `json:"action"`
		Note   string `json:"note"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	note := strings.TrimSpace(req.Note)
	if len(note) > 200 {
		note = note[:200]
	}
	var state string
	switch strings.ToLower(strings.TrimSpace(req.Action)) {
	case "apply":
		if acc.PoolState == "in" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "该账号已在号池中"})
			return
		}
		// 入池积分门槛（v0.20.0）。
		credits, known := a.poolCredits(uid)
		if !known {
			// 提示必须指向**用户端真实存在**的入口（v0.23.1）：此前写的是
			// 「账号卡片点刷新积分」，而那是管理端的按钮，用户控制台没有 →
			// 用户被指到一个不存在的操作上，只能反复重试。
			// 现在的入口：卡片上的积分区块会自动查询并**写回**（v0.23.1），
			// 或点「查看全部」手动再查一次。
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "尚未获取到该账号的积分。请在卡片上点一次「查看全部」（积分区块会自动同步最新积分），然后再申请入池",
			})
			return
		}
		if credits < poolApplyMinCredits {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": fmt.Sprintf("账号积分不足 %d（当前 %d），暂不能申请入池",
					poolApplyMinCredits, credits),
				"credits":   credits,
				"min_apply": poolApplyMinCredits,
			})
			return
		}
		state = "pending"
	case "cancel":
		if acc.PoolState != "pending" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "当前没有待审申请"})
			return
		}
		state = ""
		note = ""
	case "leave":
		state = ""
		note = ""
	case "accept":
		// 接受管理员的入池邀请（v0.31.18）。
		//
		// 为什么需要它：入池的**发起方**可以有两边 —— 号主自己申请（apply）
		// 或管理员邀请（见 handleAdminHostReview）。被邀请时账号停在 "invited"，
		// 只有号主自己点「接受」才会真正进池。这条链路存在的意义就是
		// **不让管理员单方面把一个人的号拉进平台号池去消耗他的积分**。
		if acc.PoolState != "invited" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "当前没有待确认的入池邀请"})
			return
		}
		// 与 apply 同门槛：接受前再复核一次积分。
		if credits, known := a.poolCredits(uid); !known {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "尚未获取到该账号的积分。请在卡片上点一次「查看全部」（积分区块会自动同步最新积分），然后再确认",
			})
			return
		} else if credits < poolApplyMinCredits {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error":     fmt.Sprintf("账号积分不足 %d（当前 %d），暂不能入池", poolApplyMinCredits, credits),
				"credits":   credits,
				"min_apply": poolApplyMinCredits,
			})
			return
		}
		state = "in"
		note = "号主已接受入池邀请"
	case "decline":
		// 谢绝邀请：退回未入池，并把邀请痕迹留在 note 里
		// —— 管理端能看出“邀请过、被拒了”，而不是“从没邀请过”。
		if acc.PoolState != "invited" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "当前没有待确认的入池邀请"})
			return
		}
		state = ""
		note = "号主谢绝了入池邀请"
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "未知动作（apply/cancel/leave/accept/decline）"})
		return
	}
	if err := a.store.SetAccountPoolState(uid, state, note); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// 退出号池时清掉「系统自动搬入的预备组」（v0.20.0）：用户既然主动退池，
	// 就不该继续挂着一个自动分组 —— 清成空串后由 inferConsumeGroup 回落
	// （未入池的代挂号 → 闲置）。**只清系统搬的**：管理员手动设过的分组不动。
	if state == "" {
		if effectiveConsumeGroup(acc) == cgPrep && strings.HasPrefix(acc.ConsumeGroupNote, autoTagPrefix) {
			_ = a.store.SetAccountConsumeGroup(uid, "")
		}
	}
	// 退出号池后立即重算选号范围，避免仍被平台反代选中。
	_ = a.proxy.SyncAccounts()
	writeJSON(w, http.StatusOK, map[string]any{"uid": uid, "pool_state": state})
}

// egressHostOf 从代理 URL 里取 host:port，用于线路的「地区 / 出口」展示位。
//
// 不解析失败就回落原串：这一格是给人看的**线索**，不是判据 ——
// 为了它去拒绝一条合法代理是本末倒置。
func egressHostOf(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		return strings.TrimSpace(rawURL)
	}
	return u.Host
}

// handleHostDeleteAccount 删除本人代挂账号。
func (a *app) handleHostDeleteAccount(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	uid := r.PathValue("uid")
	if a.requireOwnedAccount(w, uid, u.ID) == nil {
		return
	}
	if err := a.store.DeleteAccount(uid); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	_ = a.proxy.SyncAccounts()
	writeJSON(w, http.StatusOK, map[string]any{"deleted": uid})
}

// handleHostResources 某代挂账号的逐套餐积分明细（供卡片展示）。
func (a *app) handleHostResources(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	uid := r.PathValue("uid")
	acc := a.requireOwnedAccount(w, uid, u.ID)
	if acc == nil {
		return
	}
	a.writeCreditDetail(w, acc)
}

// handleSystemInfo GET /api/system/info —— 控制台「设置 · 关于」要用的最小信息集。
//
// 为什么单开一个接口而不是让前端拼：**版本号只有一个来源**（`const version`），
// 前端硬编码版本号必然与二进制漂移，而"我到底跑的是哪一版"恰恰是排障第一问。
// origin 由请求推导（挂隧道 / 反代后面时，服务端自己也推不准）。
func (a *app) handleSystemInfo(w http.ResponseWriter, r *http.Request) {
	origin := requestOrigin(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"name":     "WorkBuddy2API",
		"version":  version,
		"origin":   origin,
		"base_url": origin + "/v1",
	})
}

// handleHostEarnings 我的代挂收益流水。
func (a *app) handleHostEarnings(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := a.store.ListHostEarnings(u.ID, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	rate := a.moneyRate()
	rows := make([]map[string]any, 0, len(list))
	for _, e := range list {
		rows = append(rows, map[string]any{
			"account_uid": e.AccountUID,
			"model":       e.Model,
			"charged":     MicroToPoint(e.Charged),
			"earned":      MicroToPoint(e.Earned),
			// 收益＝返点 → 金额形态（v0.17.7）：前端以 ¥ 为主展示。
			"earned_cny": microToCNY(e.Earned, rate),
			"created_at": e.CreatedAt,
		})
	}
	totalMicro, count, _ := a.store.HostEarningSummary(u.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"rows":      rows,
		"total":     MicroToPoint(totalMicro),
		"total_cny": microToCNY(totalMicro, rate),
		"count":     count,
		"rate":      rate,
	})
}

// ---------------------------------------------------------------------
// 管理侧
// ---------------------------------------------------------------------

// handleAdminHostAccounts 全部用户代挂账号 + 待审队列（管理后台统一展示代挂分组）。
func (a *app) handleAdminHostAccounts(w http.ResponseWriter, r *http.Request) {
	list, err := a.store.ListHostedAccounts()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	names := a.userNameMap()
	usageByUID, _ := a.store.UsageByAccount()
	// 积分明细缓存（v0.24.9）：批量载入，卡片零出站即可展示明细。
	creditByUID := a.store.CreditPacksByUID()
	views := make([]hostAccountView, 0, len(list))
	byOwner := map[int64][]hostAccountView{}
	pending := make([]hostAccountView, 0)
	// invited（v0.31.18）：管理员已发出邀请、等号主确认的一批。
	// 与 pending（号主申请、等管理员审批）**方向相反**，必须分开报 ——
	// 合成一个"待处理"队列会让两种动作（批准 / 催确认）混在一起。
	invited := make([]hostAccountView, 0)
	for _, acc := range list {
		v := a.hostViewP(acc, creditByUID[acc.UID])
		if us, ok := usageByUID[acc.UID]; ok {
			v.ConsumedCredit = us.Credit
			v.ChargedPoints = MicroToPoint(us.Charged)
			v.ServedRequests = us.Requests
		}
		v.OwnerName = names[acc.OwnerUserID]
		if v.OwnerName == "" {
			v.OwnerName = "用户#" + strconv.FormatInt(acc.OwnerUserID, 10)
		}
		views = append(views, v)
		byOwner[acc.OwnerUserID] = append(byOwner[acc.OwnerUserID], v)
		if acc.PoolState == "pending" {
			pending = append(pending, v)
		}
		if acc.PoolState == "invited" {
			invited = append(invited, v)
		}
	}
	// 稳定排序：按号主名升序，便于管理后台扫读。
	sort.SliceStable(views, func(i, j int) bool {
		if views[i].OwnerName != views[j].OwnerName {
			return views[i].OwnerName < views[j].OwnerName
		}
		return views[i].CreatedAt < views[j].CreatedAt
	})
	owners := make([]map[string]any, 0, len(byOwner))
	for oid, vs := range byOwner {
		name := names[oid]
		if name == "" {
			name = "用户#" + strconv.FormatInt(oid, 10)
		}
		owners = append(owners, map[string]any{
			"owner_id": oid, "owner_name": name, "count": len(vs),
		})
	}
	sort.SliceStable(owners, func(i, j int) bool {
		return strField(owners[i], "owner_name") < strField(owners[j], "owner_name")
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": views,
		"owners":   owners,
		"pending":  pending,
		"invited":  invited,
	})
}

// handleAdminHostReview 审批入池：action=approve|reject。
func (a *app) handleAdminHostReview(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	acc, err := a.store.GetAccount(uid)
	if err != nil || acc == nil || acc.OwnerUserID == 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "代挂账号不存在"})
		return
	}
	var req struct {
		Action string `json:"action"`
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	switch strings.ToLower(strings.TrimSpace(req.Action)) {
	case "approve":
		// 审批放行前复核积分门槛（v0.20.0）：申请时够、审批时可能已被消耗掉。
		if credits, known := a.poolCredits(uid); !known || credits < poolApplyMinCredits {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": fmt.Sprintf("账号当前积分不满足入池门槛（需 %d，当前 %d）",
					poolApplyMinCredits, credits),
			})
			return
		}
		// ⚠️ 语义分流（v0.31.18，用户现场驱动）：这个按钮此前对**任何**代挂号
		// 都直接落 "in"，于是管理员能把一个**号主从未同意入池**的号拉进平台号池
		// —— 号主的积分会被消耗，而界面上写的还是“批准入池”（听起来像“同意了他的申请”）。
		// 现在按**来源**分流：
		//   · 号主申请过（pool_state == "pending"）→ 这是**审批**，落 "in"；
		//   · 号主没申请过（其它值）→ 这是**邀请**，落 "invited"，等号主自己确认。
		// 号池只会选中 pool_state == "in" 的号，所以 "invited" 期间零消耗、零出站。
		if acc.PoolState == "pending" {
			if err := a.store.SetAccountPoolState(uid, "in", "已通过审核"); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
		} else {
			if err := a.store.SetAccountPoolState(uid, "invited", "管理员邀请入池，等待号主确认"); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			log.Printf("INFO: [host] 已向代挂号 %s 发出入池邀请（等待号主确认）", uid)
		}
	case "reject":
		reason := strings.TrimSpace(req.Reason)
		if reason == "" {
			reason = "未通过审核"
		}
		// 已邀请但尚未确认的号，管理员“驳回”= 撤回邀请（不是驳回申请）：
		// 落回未入池，并在 note 里留下痕迹。
		if acc.PoolState == "invited" {
			if err := a.store.SetAccountPoolState(uid, "", "管理员撤回了入池邀请"); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			break
		}
		if err := a.store.SetAccountPoolState(uid, "rejected", reason); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "未知动作（approve/reject）"})
		return
	}
	// 审批结果直接改变平台的选号范围，立即重算。
	_ = a.proxy.SyncAccounts()
	if acc, err := a.store.GetAccount(uid); err == nil && acc != nil {
		writeJSON(w, http.StatusOK, map[string]any{"uid": uid, "account": a.hostView(acc)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"uid": uid})
}

// handleAccountResources 管理侧：任意账号的逐套餐积分明细。
func (a *app) handleAccountResources(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	acc, err := a.store.GetAccount(uid)
	if err != nil || acc == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "账号不存在"})
		return
	}
	a.writeCreditDetail(w, acc)
}

// writeCreditDetail 查询并下发某账号的逐套餐积分明细。
// 上游失败不视为网关错误：以 ok=false + error 透出，前端卡片显示原因（不阻塞整页）。
//
// v0.23.1：查询成功后**顺带把权威读数写回池内**（打戳 + 按余额解冻 + 分组收敛），
// 与 creditsync / 健康检测的收尾口径逐字一致。
//
// 为什么必须写回（历史缺陷）：此接口此前**只读不写** —— 用户能在这里看到实时积分
// （比如 4000 分），但入池门槛 `poolCredits` 读的是**池内快照**，两者不同源，
// 于是出现「明明有积分却提示『尚未获取到该账号的积分』、无法入池」，而且提示让用户
// 去「账号卡片点刷新积分」—— 用户控制台根本没有那个按钮，形成**死锁**。
// 写回后：卡片一渲染就自动查一次（creditPanel 的既有行为），积分随之同步，
// 用户不需要任何额外操作即可入池。
func (a *app) writeCreditDetail(w http.ResponseWriter, acc *Account) {
	au := authFromAccount(acc)
	if au == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "账号凭证不可用"})
		return
	}
	// 探活类出站的全局闸（v0.24.3，2026-10-13 事故）：本接口**每次调用都会真实出站**
	// 查上游积分，而管理后台的账号池页曾为**每张卡片**自动调用它 —— 59 张卡 = 59 个
	// 并发、同一出口 IP、几秒内打完 → 上游风控当场返回 11140 → 30 个健康号被误判停用。
	//
	// 前端已改为「点开才查」（根因修复），这里是**服务端兜底**：即便将来又有人引入
	// 渲染即出站的 UI，也不会再形成并发风暴。
	release, gerr := a.probes.acquire(acc.UID)
	if gerr != nil {
		// 与「上游失败不视为网关错误」同口径：回 200 + ok=false，让卡片显示原因而不是报故障。
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": gerr.Error(), "throttled": true})
		return
	}
	defer release()
	packs, remain, used, size, expiringSoon, err := a.proxy.uc.ResourcePackages(au)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// —— 写回明细缓存（v0.24.9）——
	// **仅在成功路径**写：失败时保留上一次的好数据，绝不用空结果覆盖缓存。
	// 这一步是「卡片能零出站显示明细」的数据来源（见 creditcache.go）。
	a.cacheCreditPacks(acc.UID, packs)

	// —— 写回池内（v0.23.1）——
	// 顺序与 checkAccountHealth 一致：先写权威余额 + 快过期分桶，再按余额解冻，
	// 最后按新积分重算预备组（积分是预备组进出的唯一判据，刷完就该收敛）。
	a.proxy.pool.SetCreditsDetailed(acc.UID, remain, expiringSoon)
	a.proxy.pool.ReenableIfCredits(acc.UID, remain)
	a.reconcilePoolGroupOne(acc.UID)

	rows := make([]map[string]any, 0, len(packs))
	for _, p := range packs {
		rows = append(rows, map[string]any{
			"name":      p.Name,
			"remain":    p.Remain,
			"used":      p.Used,
			"size":      p.Size,
			"expire_at": p.ExpireAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"remain":        remain,
		"used":          used,
		"size":          size,
		"packs":         len(packs),
		"expiring_soon": expiringSoon,
		"resources":     rows,
		"updated_at":    time.Now().Unix(),
	})
}
