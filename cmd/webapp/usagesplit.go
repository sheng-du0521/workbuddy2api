// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"net/http"
	"strconv"
	"strings"
)

// 用量「归属」拆分（v0.31.20）。
//
// 为什么单开一处：报表域（`internal/api/report`）给的是「按模型 / 按天」的账，
// 它回答不了最常被问的那一问 ——
//
//	管理端：「这些消耗里多少来自平台自己的号、多少来自用户代挂？」（第 4 条）
//	用户端：「我花的钱里，多少走了自己的号（不扣余额）、多少走了号池？」（第 5 条）
//
// 两问都只需要 `usage_logs` 的三列（`scope` / `user_id` / `upstream_uid` → owner），
// 所以在查询侧聚合即可，**不去动报表域的对外契约**（那个契约被多处消费，
// 为一个新问题改形状，代价远大于收益）。
//
// ⚠️ 口径（与项目既有纪律一致）：
//   - 只统计 `status='ok'`（失败请求不该出现在"消耗"里）；
//   - 时间窗口一律 `localDayStart(days)` —— 手写 `Truncate(24h)` 会把东八区当天切掉；
//   - 管理端排除 `user_id = 0`（管理密钥通道按设计不计费，混进来会让"归属"失真）。

// usageSplitRow 一个归属桶的用量。
type usageSplitRow struct {
	Kind     string  `json:"kind"` // pool/plat 平台侧；host 代挂侧
	Requests int64   `json:"requests"`
	Credit   float64 `json:"credit"`  // 上游实扣积分（号池积分）
	Charged  float64 `json:"charged"` // 平台收费点数（微点 → 点）
}

// usageSplitSince 把 days 收敛到合法区间并换算成窗口起点。
func usageSplitSince(days int) (int, int64) {
	if days <= 0 {
		days = 14
	}
	if days > 365 {
		days = 365
	}
	return days, localDayStart(days)
}

// usageSplitDays 解析 ?days=。
func usageSplitDays(r *http.Request) int {
	n, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("days")))
	if err != nil {
		return 14
	}
	return n
}

// usageSplitQuery 跑一条「按归属分桶」的聚合，返回两个桶（缺的补 0）。
//
// 两个调用点只有 FROM/JOIN/CASE 不同，聚合口径完全一致 —— 所以用同一段扫描逻辑，
// 免得"管理端算 charged、用户端算 credit"这种口径漂移再长出来。
func (s *Store) usageSplitQuery(query string, args ...any) ([]usageSplitRow, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []usageSplitRow{}
	for rows.Next() {
		var r usageSplitRow
		var creditRaw, chargedRaw any
		if err := rows.Scan(&r.Kind, &r.Requests, &creditRaw, &chargedRaw); err != nil {
			return nil, err
		}
		// ⚠️ 按**真实列类型**取值，不假设整数（v0.31.21 真机抓到的 bug）：
		//   `upstream_credit` 在库里是 **REAL**（历史遗留，存的就是积分，如 10196.29），
		//   `charged_micro` 是 **INTEGER** 微点。
		// 此前把两者都 Scan 进 int64，SQLite 遇到 REAL 直接报
		// "converting driver.Value type float64 … to a int64" → 接口一律 500。
		// 用 `any` + 归一化，既兼容既有库，也不怕将来有人把列改成 INTEGER。
		r.Credit = valueAsFloat(creditRaw)                            // 单位：号池积分
		r.Charged = valueAsFloat(chargedRaw) / float64(MicroPerPoint) // 微点 → 点
		out = append(out, r)
	}
	return out, rows.Err()
}

// valueAsFloat 把驱动返回的数值归一成 float64（int64 / float64 / nil）。
//
// 存在的理由见上面的注释：**同一张表里两种列类型混用**是既有事实，
// 硬编码 int64 会直接 500 —— 这种错在单测里看不出来（纯函数不碰驱动），
// 只有真机（或带真实列类型的集成测试）才会暴露。
func valueAsFloat(v any) float64 {
	switch x := v.(type) {
	case int64:
		return float64(x)
	case float64:
		return x
	case nil:
		return 0
	default:
		return 0
	}
}

// UsageSplitByKeyKind 用户维度：按「号池计费 Key / 代挂自用 Key」拆用量。
//
// 判据用 **Key 的 kind**（而不是 `usage_logs.scope`）：用户看到的"我这笔走的是
// 自用还是号池"由他手里那把 Key 决定，而 scope 是服务端内部口径（两者通常一致，
// 但 Key 删掉后 join 不到时，scope 仍能给出正确归类 —— 所以 kind 取不到就回落 scope）。
func (s *Store) UsageSplitByKeyKind(userID int64, since int64) ([]usageSplitRow, error) {
	const q = `
		SELECT CASE WHEN COALESCE(k.kind,'') = 'host' THEN 'host'
		            WHEN COALESCE(u.scope,'')  = 'host' THEN 'host'
		            ELSE 'pool' END AS kind,
		       COUNT(*),
		       COALESCE(SUM(u.upstream_credit), 0),
		       COALESCE(SUM(u.charged_micro), 0)
		  FROM usage_logs u
		  LEFT JOIN api_keys k ON k.id = u.key_id
		 WHERE u.user_id = ? AND u.status = 'ok' AND u.created_at >= ?
		 GROUP BY 1`
	return s.usageSplitQuery(q, userID, since)
}

// UsageSplitByOwner 平台维度：按「平台自持号 / 用户代挂号」拆用量。
func (s *Store) UsageSplitByOwner(since int64) ([]usageSplitRow, error) {
	const q = `
		SELECT CASE WHEN COALESCE(a.owner_user_id, 0) > 0 THEN 'host' ELSE 'plat' END AS kind,
		       COUNT(*),
		       COALESCE(SUM(u.upstream_credit), 0),
		       COALESCE(SUM(u.charged_micro), 0)
		  FROM usage_logs u
		  LEFT JOIN accounts a ON a.uid = u.upstream_uid
		 WHERE u.user_id > 0 AND u.status = 'ok' AND u.created_at >= ?
		 GROUP BY 1`
	return s.usageSplitQuery(q, since)
}

// usageSplitPayload 把两桶结果整理成前端好用的形状（缺桶补 0，带合计）。
func usageSplitPayload(days int, rows []usageSplitRow) map[string]any {
	byKind := map[string]usageSplitRow{
		"pool": {Kind: "pool"},
		"host": {Kind: "host"},
		"plat": {Kind: "plat"},
	}
	for _, r := range rows {
		if _, ok := byKind[r.Kind]; ok {
			byKind[r.Kind] = r
		}
	}
	totalReq, totalCredit, totalCharged := int64(0), float64(0), float64(0)
	for _, r := range byKind {
		totalReq += r.Requests
		totalCredit += r.Credit
		totalCharged += r.Charged
	}
	return map[string]any{
		"days": days,
		"rows": []usageSplitRow{byKind["pool"], byKind["host"], byKind["plat"]},
		"pool": byKind["pool"], "host": byKind["host"], "plat": byKind["plat"],
		"total": map[string]any{
			"requests": totalReq, "credit": totalCredit, "charged": totalCharged,
		},
	}
}

// handleMyUsageSplit GET /api/me/usage-split?days=14（用户看自己的）
func (a *app) handleMyUsageSplit(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	days, since := usageSplitSince(usageSplitDays(r))
	rows, err := a.store.UsageSplitByKeyKind(u.ID, since)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, usageSplitPayload(days, rows))
}

// handleAdminUsageSplit GET /api/admin/usage-split?days=14（管理端看全平台归属）
func (a *app) handleAdminUsageSplit(w http.ResponseWriter, r *http.Request) {
	days, since := usageSplitSince(usageSplitDays(r))
	rows, err := a.store.UsageSplitByOwner(since)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, usageSplitPayload(days, rows))
}
