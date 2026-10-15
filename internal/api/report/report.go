// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 用量报表域（P2-7 拆包「第四刀第二刀」，v0.31.13）：
// **用量账单三类拆分** + **消耗排行** 两个只读报表域。
//
// 为什么是这一簇（选簇判据 = 先量化再动刀）：
//
//	· 它只依赖**只读聚合**，且这些聚合已经在 internal/usage 里（v0.31.9 第二刀 +
//	  v0.31.13 并入 host_earnings 聚合）；
//	· 它是本轮**依赖宽度最窄**的一簇：7 个 store 方法、**0 个 cmd/webapp 自有类型**
//	  —— 因此本刀**不需要任何类型别名**（迁移成本最低的一种形态）；
//	· 与本仓其他 handler 不同，这两个域的**判定**（脱敏范围、榜外名次、条数钳制、
//	  三类拆分口径）过去只能通过"拉起整个 app + 真库"间接跑到，边界几乎没人钉。
//
// 边界纪律（与 internal/httpx 的分工）：
//
//	· 本包 = **本域的领域逻辑 + 本域的 HTTP 视图组装**（带业务判断的写出留在这里）；
//	· 横切关注点（WriteJSON / 内部错误文本脱敏）走 internal/httpx；
//	· 路由注册仍在 cmd/webapp（应用级装配，不是领域逻辑）；
//	· 身份解析由调用方**注入**（UserIDOf）—— 本包不认 cookie / 会话表 / 用户角色。
package report

import (
	"net/http"
	"strconv"
	"strings"

	"workbuddy2api/internal/billing"
	"workbuddy2api/internal/httpx"
	"workbuddy2api/internal/usage"
)

// Store 本域需要的**最小** store 能力面（全部只读）。
//
// 刻意逐个列出 7 个方法：窄接口是"这一域到底依赖什么"的**可执行文档** ——
// 将来给 Store 加方法不会意外扩大本域的可达面；删/改签名会在这里立刻编译失败。
type Store interface {
	// 消费侧（usage_logs）按 scope 拆分的逐日 / 按模型聚合。
	UsageDailyScoped(userID int64, days int, scope string) ([]usage.UsageDailyStat, error)
	UsageByModelScoped(userID int64, days int, scope string) ([]usage.UsageModelStat, error)
	// 收益侧（host_earnings）：窗口聚合 + 全时段汇总。
	HostEarningsDaily(ownerUserID int64, days int) ([]usage.HostEarnDailyStat, error)
	HostEarningsByModel(ownerUserID int64, days int) ([]usage.HostEarnModelStat, error)
	HostEarningSummary(ownerUserID int64) (totalMicro int64, count int64, err error)
	// 排行侧（usage_logs）按用户聚合与分母。
	UsageByUser(userID int64, days, limit int) ([]usage.UsageUserStat, error)
	CountUsageUsers(days int) (int, error)
}

// UserIDOf 从请求解析当前会话用户 ID。
//
// ⚠️ 返回**显式 ok 布尔**，不用"ID == 0"表示匿名：身份的有无是事实，
// 不该靠"某个字段恰好是零值"来推断（真实用户 ID 从 1 起只是巧合，不是契约）。
//
// 由调用方注入的原因：身份认证是**应用级装配**（本仓会话走 cookie + 会话表），
// 不是报表领域知识。注入后本包只认"一个整数 + 一个布尔"。
type UserIDOf func(*http.Request) (id int64, ok bool)

// Service 报表域服务。
//
// **值接收者 + 调用方每次现构造**：它只持有两个不可变引用、无内部状态 ——
// 于是不存在"某条构造路径忘了初始化某个字段 → nil panic"这类缺陷。
type Service struct {
	store    Store
	userIDOf UserIDOf
}

// New 构造服务。userIDOf 传 nil 时按"永远匿名"处理（用户侧接口会如实回 401）。
func New(store Store, userIDOf UserIDOf) Service {
	if userIDOf == nil {
		userIDOf = func(*http.Request) (int64, bool) { return 0, false }
	}
	return Service{store: store, userIDOf: userIDOf}
}

// 排行条数限制（口径写在 rankDoc 里，改动要同步文档）。
const (
	// RankingLimitDefault 管理端默认条数。
	RankingLimitDefault = 20
	// RankingLimitMax 管理端条数上限（超出即钳）。
	RankingLimitMax = 50
	// RankingUserLimit 用户侧榜单最多展示几名（榜尾不无限长）。
	RankingUserLimit = 10
)

// MaskUsername 用户名脱敏：保留首尾字符，中间固定 3 个 '*'。
//
// 与管理密钥掩码同一思路 —— **掩码长度不随真实长度变化**（否则长度本身成了信息）。
// 长度 ≤2 时只露首字符（否则等于没脱敏）。
func MaskUsername(name string) string {
	r := []rune(strings.TrimSpace(name))
	switch {
	case len(r) == 0:
		return "匿名用户"
	case len(r) == 1:
		return string(r[0]) + "**"
	case len(r) == 2:
		return string(r[0]) + "*"
	default:
		return string(r[0]) + "***" + string(r[len(r)-1])
	}
}

// ClampDays 解析并收敛 days（缺省 def，上限 max）。
func ClampDays(raw string, def, max int) int {
	d, err := strconv.Atoi(raw)
	if err != nil || d <= 0 {
		return def
	}
	if d > max {
		return max
	}
	return d
}

// BreakdownDays 解析并钳制「用量账单三类拆分」的 days 参数（缺省 14，上限 90）。
func BreakdownDays(r *http.Request) int {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 {
		days = 14
	}
	if days > 90 {
		days = 90
	}
	return days
}

// ----------------------------------------------------------------------
// 消耗排行
// ----------------------------------------------------------------------
//
// 排序依据 / 统计范围 / 条数限制（口径写死在这，改动了要同步文档）：
//
//	· 排序：窗口内**消耗点数降序**（charged_micro）→ 请求数降序 → user_id 升序。
//	  不按 token 排：token 与费用不是线性关系（缓存命中 / 不同模型单价差异很大），
//	  按 token 排会出现「用了很多免费额度就登顶」的误导性榜单。
//	· 范围：与报表其余部分完全同源（同一个 days、同一个天界算法），
//	  仅含 created_at 落在窗口内的记录，且**排除管理密钥通道**（user_id = 0）。
//	· 条数：管理端一次最多 50 条（limit 参数，默认 20，超出即钳）；
//	  用户控制台一次最多 10 条，且**他人用户名一律脱敏**。
//
// 为什么用户侧要脱敏：排行榜天然要展示别人，但把真实用户名展示给任意登录用户，
// 等于提供一个可枚举的账号名录。故此处分层：
//   - 管理端（adminGuard）：完整用户名，供运营对接；
//   - 用户侧：MaskUsername 脱敏 + is_me 标记高亮自己；分母 total_users 只给数量。

// Ranking 组装消耗排行块（管理端与用户侧共用同一计算，只有展示口径不同）。
//
//   - userID > 0 → 「用户控制台视角」：他人用户名脱敏、短榜、附自己的名次；
//   - userID = 0 → 「管理端视角」：完整用户名、可指定 limit（上限 RankingLimitMax）。
//
// limit <= 0 表示按角色取默认（管理端 20 / 用户侧 10）。
func (s Service) Ranking(userID int64, days, limit int) map[string]any {
	self := userID > 0
	if limit <= 0 {
		if self {
			limit = RankingUserLimit
		} else {
			limit = RankingLimitDefault
		}
	}
	if limit > RankingLimitMax {
		limit = RankingLimitMax
	}
	top, err := s.store.UsageByUser(0, days, limit)
	if err != nil {
		// 注意：错误**不改变 HTTP 状态码**（仍是 200），文案由 httpx 侧脱敏后回填。
		// 保持原样是为了零行为变化；这正是「横切关注点只放一处」的价值 ——
		// 本包不必知道内部错误文本能不能给客户端看。
		return map[string]any{"error": err.Error()}
	}
	total, _ := s.store.CountUsageUsers(days)

	list := make([]map[string]any, 0, len(top))
	var me map[string]any
	for i, st := range top {
		rank := i + 1
		name := st.Username
		if name == "" {
			name = "用户 #" + strconv.FormatInt(st.UserID, 10)
		}
		row := map[string]any{
			"rank":     rank,
			"charged":  billing.MicroToPoint(st.ChargedMicro),
			"requests": st.Requests,
			"tokens":   st.Tokens,
			"is_me":    st.UserID == userID,
		}
		if self && st.UserID != userID {
			row["name"] = MaskUsername(name) // 他人：脱敏，不给可枚举的账号名录
		} else {
			row["name"] = name
			row["user_id"] = st.UserID
		}
		if st.UserID == userID {
			me = map[string]any{
				"rank":        rank,
				"name":        name,
				"charged":     billing.MicroToPoint(st.ChargedMicro),
				"requests":    st.Requests,
				"tokens":      st.Tokens,
				"in_top":      true,
				"total_users": total,
			}
		}
		list = append(list, row)
	}
	// 自己不在榜内（未进 top N，或窗口内没有消耗）：单独查一次自己的名次，
	// 否则用户会看到「榜上没有我」而不知道自己的位置。
	if self && me == nil {
		me = s.myRank(userID, days, total)
	}
	out := map[string]any{
		"days":        days,
		"order_by":    "charged",
		"total_users": total,
		"limit":       limit,
		"list":        list,
	}
	if self {
		out["masked"] = true
		out["me"] = me
	}
	return out
}

// myRank 查某用户在当前窗口的名次（不在前 N 名时用）。
//
// 实现方式：取全部有消耗的用户合计值，再数「比我多」的人 —— 数据量是
// 「窗口内有消耗的用户数」，运营量级（几十~几百）下完全够用；
// 用 SQL 窗口函数（ROW_NUMBER）当然更快，但本项目走的是 modernc sqlite
// 最小可用集，少引入一个方言依赖更稳。
func (s Service) myRank(userID int64, days int, total int) map[string]any {
	all, err := s.store.UsageByUser(0, days, 0)
	if err != nil {
		return map[string]any{"rank": 0, "in_top": false, "total_users": total}
	}
	for i, st := range all {
		if st.UserID == userID {
			return map[string]any{
				"rank":        i + 1,
				"name":        st.Username,
				"charged":     billing.MicroToPoint(st.ChargedMicro),
				"requests":    st.Requests,
				"tokens":      st.Tokens,
				"in_top":      false,
				"total_users": total,
			}
		}
	}
	return map[string]any{
		"rank":        0,
		"name":        "",
		"charged":     0,
		"requests":    0,
		"tokens":      0,
		"in_top":      false,
		"total_users": total,
	}
}

// HandleAdminRanking 管理端独立排行接口（报表页可单独刷新榜单）。
// GET /api/admin/usage/ranking?days=14&limit=20
//
// 之所以单独开一个接口而不是只塞进 /api/admin/usage：报表页的榜单要能
// 单独高频刷新（实时感），而整份报表含逐日 + 按模型聚合，扫表更重。
func (s Service) HandleAdminRanking(w http.ResponseWriter, r *http.Request) {
	days := ClampDays(r.URL.Query().Get("days"), 14, 90)
	limit := RankingLimitDefault
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = v
	}
	httpx.WriteJSON(w, http.StatusOK, s.Ranking(0, days, limit))
}

// HandleUserRanking 用户侧消耗排行。
// GET /api/usage/ranking?days=14
//
// 单独接口而非复用 /api/usage：控制台概览要「实时」刷新榜单（十几秒一次），
// 而 /api/usage 还带逐日 + 按模型聚合，扫表更重 —— 高频轮询没必要拖上它们。
func (s Service) HandleUserRanking(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.userIDOf(r)
	if !ok {
		httpx.WriteJSON(w, http.StatusUnauthorized, map[string]any{"error": "未登录"})
		return
	}
	days := ClampDays(r.URL.Query().Get("days"), 14, 90)
	httpx.WriteJSON(w, http.StatusOK, s.Ranking(uid, days, 0))
}

// ----------------------------------------------------------------------
// 用量账单 · 三类拆分（v0.10.0）
// ----------------------------------------------------------------------
//
// 控制台的用量账单按数据来源拆成三类：
//  1. self_use  自己代挂使用：用「代挂自用 Key」走自己的代挂账号，免费但真实消耗
//     （度量 = 请求数 / tokens / 上游 credit，扣费恒为 0）。
//  2. pool_use  使用号池：用「平台计费 Key」走平台号池，按用量扣余额。
//  3. pool_earn 进入号池：自己的代挂账号在池中承接平台流量，平台按分成折算为号主余额。
//     前两者来自 usage_logs（按 scope 拆分），第三者来自 host_earnings。

// HandleUsageBreakdown 本人用量账单的三类拆分。
func (s Service) HandleUsageBreakdown(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.userIDOf(r)
	if !ok {
		httpx.WriteJSON(w, http.StatusUnauthorized, map[string]any{"error": "未登录"})
		return
	}
	days := BreakdownDays(r)

	resp := map[string]any{"days": days}
	// ⚠️ 两类的 scope 与键名的对应关系是**契约**：self_use 必须配 ScopeHost、
	// pool_use 必须配 ScopePool。写反了不会报错，只会把用户的"自用"与"用号池"
	// 对调 —— 一个有单测钉住的易错点。
	resp["self_use"] = s.scopeBlock(uid, days, billing.ScopeHost)
	resp["pool_use"] = s.scopeBlock(uid, days, billing.ScopePool)
	resp["pool_earn"] = s.hostEarnBlock(uid, days)
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// scopeBlock 组装「消费侧」一类（请求数 / token / 扣费 / 上游 credit + 按日 + 按模型）。
func (s Service) scopeBlock(userID int64, days int, scope string) map[string]any {
	daily, _ := s.store.UsageDailyScoped(userID, days, scope)
	models, _ := s.store.UsageByModelScoped(userID, days, scope)

	var reqs, tokens, charged int64
	var credit float64
	dailyItems := make([]map[string]any, 0, len(daily))
	for _, d := range daily {
		reqs += d.Requests
		tokens += d.Tokens
		charged += d.ChargedMicro
		credit += d.UpstreamCredit
		dailyItems = append(dailyItems, map[string]any{
			"day": d.Day, "requests": d.Requests, "tokens": d.Tokens,
			"charged": billing.MicroToPoint(d.ChargedMicro), "upstream_credit": d.UpstreamCredit,
		})
	}
	modelItems := make([]map[string]any, 0, len(models))
	for _, m := range models {
		modelItems = append(modelItems, map[string]any{
			"model": m.Model, "requests": m.Requests, "tokens": m.Tokens,
			"charged": billing.MicroToPoint(m.ChargedMicro), "upstream_credit": m.UpstreamCredit,
		})
	}
	return map[string]any{
		"requests":        reqs,
		"tokens":          tokens,
		"charged":         billing.MicroToPoint(charged),
		"upstream_credit": credit,
		"daily":           dailyItems,
		"models":          modelItems,
	}
}

// hostEarnBlock 组装「进入号池」一类：自己的代挂账号服务了多少平台流量、赚了多少返点。
//
// ⚠️ 两个口径必须分开看：total/count 是**全时段累计**（HostEarningSummary），
// daily/models 是**近 days 天窗口**。二者不等是设计如此 —— 界面上"一共赚了多少"
// 不该被 days 参数影响。给 summary 加窗口是错，给 daily 去窗口也是错。
func (s Service) hostEarnBlock(userID int64, days int) map[string]any {
	daily, _ := s.store.HostEarningsDaily(userID, days)
	models, _ := s.store.HostEarningsByModel(userID, days)

	var reqs, charged, earned int64
	dailyItems := make([]map[string]any, 0, len(daily))
	for _, d := range daily {
		reqs += d.Requests
		charged += d.ChargedMicro
		earned += d.EarnedMicro
		dailyItems = append(dailyItems, map[string]any{
			"day": d.Day, "requests": d.Requests,
			"charged": billing.MicroToPoint(d.ChargedMicro), "earned": billing.MicroToPoint(d.EarnedMicro),
		})
	}
	modelItems := make([]map[string]any, 0, len(models))
	for _, m := range models {
		modelItems = append(modelItems, map[string]any{
			"model": m.Model, "requests": m.Requests,
			"charged": billing.MicroToPoint(m.ChargedMicro), "earned": billing.MicroToPoint(m.EarnedMicro),
		})
	}
	totalMicro, count, _ := s.store.HostEarningSummary(userID)
	return map[string]any{
		"requests": reqs,
		"charged":  billing.MicroToPoint(charged),
		"earned":   billing.MicroToPoint(earned),
		"total":    billing.MicroToPoint(totalMicro),
		"count":    count,
		"daily":    dailyItems,
		"models":   modelItems,
	}
}
