// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// ranking.go 用户消耗排行（v0.17.8）。
//
// 需求：
//   - 管理后台用量报表新增「用户消耗排行」；
//   - 用户控制台概览新增「实时用户消耗排行」。
//
// ---------------------------------------------------------------------------
// 排序依据 / 统计范围 / 条数限制（口径写死在这，改动了要同步文档）
// ---------------------------------------------------------------------------
//
//	· 排序：窗口内**消耗点数降序**（charged_micro）→ 请求数降序 → user_id 升序。
//	  不按 token 排：token 与费用不是线性关系（缓存命中 / 不同模型单价差异很大），
//	  按 token 排会出现「用了很多免费额度就登顶」的误导性榜单。
//	· 范围：与报表其余部分完全同源（同一个 days、同一个天界算法），
//	  仅含 created_at 落在窗口内的记录，且**排除管理密钥通道**（user_id = 0）。
//	· 条数：管理端一次最多 50 条（limit 参数，默认 20，超出即钳）；
//	  用户控制台一次最多 10 条，且**他人用户名一律脱敏**。
//
// ---------------------------------------------------------------------------
// 为什么用户侧要脱敏
// ---------------------------------------------------------------------------
// 排行榜天然要展示别人，但把真实用户名展示给任意登录用户，等于提供一个
// 可枚举的账号名录。故此处分层：
//   - 管理端（adminGuard）：完整用户名，供运营对接；
//   - 用户侧：`maskUsername` 脱敏 + `is_me` 标记高亮自己；分母 total_users
//     只给数量，不给名单。
package main

import (
	"net/http"
	"strconv"
	"strings"
)

// rankingLimitDefault / rankingLimitMax 管理端条数限制。
const (
	rankingLimitDefault = 20
	rankingLimitMax     = 50
	// rankingUserLimit 用户侧榜单最多展示几名（榜尾不无限长）。
	rankingUserLimit = 10
)

// maskUsername 用户名脱敏：保留首尾字符，中间固定 3 个 '*'。
// 与 maskSecret 同一思路 —— 掩码长度不随真实长度变化。
// 长度 ≤2 时只露首字符（否则等于没脱敏）。
func maskUsername(name string) string {
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

// usageRanking 组装消耗排行块（管理端与用户侧共用同一计算，只有展示口径不同）。
//
//   - userID > 0 → 「用户控制台视角」：他人用户名脱敏、短榜、附自己的名次；
//   - userID = 0 → 「管理端视角」：完整用户名、可指定 limit（上限 rankingLimitMax）。
//
// limit <= 0 表示按角色取默认（管理端 20 / 用户侧 10）。
func (a *app) usageRanking(userID int64, days, limit int) map[string]any {
	self := userID > 0
	if limit <= 0 {
		if self {
			limit = rankingUserLimit
		} else {
			limit = rankingLimitDefault
		}
	}
	if limit > rankingLimitMax {
		limit = rankingLimitMax
	}
	top, err := a.store.UsageByUser(0, days, limit)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	total, _ := a.store.CountUsageUsers(days)

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
			"charged":  MicroToPoint(st.ChargedMicro),
			"requests": st.Requests,
			"tokens":   st.Tokens,
			"is_me":    st.UserID == userID,
		}
		if self && st.UserID != userID {
			row["name"] = maskUsername(name) // 他人：脱敏，不给可枚举的账号名录
		} else {
			row["name"] = name
			row["user_id"] = st.UserID
		}
		if st.UserID == userID {
			me = map[string]any{
				"rank":        rank,
				"name":        name,
				"charged":     MicroToPoint(st.ChargedMicro),
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
		me = a.myRank(userID, days, total)
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
func (a *app) myRank(userID int64, days int, total int) map[string]any {
	all, err := a.store.UsageByUser(0, days, 0)
	if err != nil {
		return map[string]any{"rank": 0, "in_top": false, "total_users": total}
	}
	rank := 0
	for i, st := range all {
		if st.UserID == userID {
			rank = i + 1
			return map[string]any{
				"rank":        rank,
				"name":        st.Username,
				"charged":     MicroToPoint(st.ChargedMicro),
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

// handleAdminUsageRanking 管理端独立排行接口（报表页可单独刷新榜单）。
// GET /api/admin/usage/ranking?days=14&limit=20
//
// 之所以单独开一个接口而不是只塞进 /api/admin/usage：报表页的榜单要能
// 单独高频刷新（实时感），而整份报表含逐日 + 按模型聚合，扫表更重。
func (a *app) handleAdminUsageRanking(w http.ResponseWriter, r *http.Request) {
	days := clampDays(r.URL.Query().Get("days"), 14, 90)
	limit := rankingLimitDefault
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = v
	}
	writeJSON(w, http.StatusOK, a.usageRanking(0, days, limit))
}

// handleUserUsageRanking 用户侧消耗排行。
// GET /api/usage/ranking?days=14
//
// 单独接口而非复用 /api/usage：控制台概览要「实时」刷新榜单（十几秒一次），
// 而 /api/usage 还带逐日 + 按模型聚合，扫表更重 —— 高频轮询没必要拖上它们。
func (a *app) handleUserUsageRanking(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	if u == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "未登录"})
		return
	}
	days := clampDays(r.URL.Query().Get("days"), 14, 90)
	writeJSON(w, http.StatusOK, a.usageRanking(u.ID, days, 0))
}

// clampDays 解析并收敛 days（缺省 def，上限 max）。
func clampDays(raw string, def, max int) int {
	d, err := strconv.Atoi(raw)
	if err != nil || d <= 0 {
		return def
	}
	if d > max {
		return max
	}
	return d
}
