package main

import (
	"database/sql"
	"fmt"
	"log"

	"workbuddy2api/internal/billing"
)

// 滚动消费窗口（v0.31.100）。
//
// # 为什么需要它
//
// 余额只回答"一共还能花多少"，回答不了"这十分钟花得太快"。一把 Key 泄漏或被脚本
// 刷起来，可以在余额耗尽前把整月额度烧完 —— 事后只能人工停用，且无法区分
// "被滥用"与"业务量真的上来了"。上游（Sub2API）的 Key 默认带 5h / 1d / 7d 三个
// 滚动窗口，正是这个用途；我们此前只有总量。
//
// # 口径
//
//   - 单位是**积分**（管理员填的是人话），比较时才换算成微点（billing.MicroPerPoint）；
//   - 0 = 不限（默认值），三个窗口相互独立，命中任意一个即拒绝；
//   - 统计来源是 `usage_logs.charged_micro`（**已实扣**），不是"预扣"也不是"名义金额" ——
//     与结算同源，不会出现"窗口说超了但余额没动"。
//
// # 为什么在鉴权中间件里查
//
// 放在代理链路最前面：拒绝是**零出站、零扣费**的，用户拿到的是 429 + 明确的窗口名，
// 而不是"上游限流"这种查不出所以然的错误。
//
// 性能：只有配置了限额的 Key 才会查（默认全 0 → 不查）。查询走
// `idx_usage_key_time(key_id, created_at)`，一次聚合把三个窗口算完。
const (
	keyWindow5h = int64(5 * 3600)
	keyWindow1d = int64(24 * 3600)
	keyWindow7d = int64(7 * 24 * 3600)
)

// keyWindow 一个滚动窗口的限制。
type keyWindow struct {
	// Label 给用户看的窗口名（"5 小时" / "1 天" / "7 天"）。
	Label string
	// Seconds 窗口长度（秒）。
	Seconds int64
	// Limit 上限（积分，>0 才生效）。
	Limit float64
}

// windowLimits 该 Key 配了限制的窗口（全 0 时返回 nil = 不必做任何查询）。
func (k *APIKey) windowLimits() []keyWindow {
	if k == nil {
		return nil
	}
	all := []keyWindow{
		{Label: "5 小时", Seconds: keyWindow5h, Limit: k.RL5h},
		{Label: "1 天", Seconds: keyWindow1d, Limit: k.RL1d},
		{Label: "7 天", Seconds: keyWindow7d, Limit: k.RL7d},
	}
	var out []keyWindow
	for _, w := range all {
		if w.Limit > 0 {
			out = append(out, w)
		}
	}
	return out
}

// windowUsage 三个窗口各自"已实扣多少微点"。
type windowUsage struct {
	W          keyWindow
	UsedMicro  int64
	LimitMicro int64
}

// Exceeded 是否已经用满（>= 即拒绝：上限是"最多花这么多"，花到等于上限就该停）。
func (u windowUsage) Exceeded() bool { return u.UsedMicro >= u.LimitMicro }

// checkKeyWindows 判断这把 Key 是否已超过任一滚动窗口。
//
// 返回 (命中的窗口, 建议的重试等待秒数, 已用微点, 是否超限)。
// 未配置任何窗口时直接返回 false（零查询）。
func (s *Store) checkKeyWindows(k *APIKey, now int64) (windowUsage, int64, bool, error) {
	wins := k.windowLimits()
	if len(wins) == 0 {
		return windowUsage{}, 0, false, nil
	}
	// 取最早的一个窗口作为 SQL 下界，三个窗口用 CASE 在一次聚合里分别累计。
	shortest := wins[0]
	for _, w := range wins {
		if w.Seconds < shortest.Seconds {
			shortest = w
		}
	}
	_ = shortest
	var row struct {
		used5h, used1d, used7d int64
	}
	row.used5h, row.used1d, row.used7d = 0, 0, 0

	// 用参数化的 CASE 一次算完：每个窗口一行 SUM(CASE WHEN ...)，条件里带自己的下界。
	var u5, u1, u7 sql.NullInt64
	err := s.db.QueryRow(`
		SELECT
			COALESCE(SUM(CASE WHEN created_at >= ? THEN charged_micro ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN created_at >= ? THEN charged_micro ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN created_at >= ? THEN charged_micro ELSE 0 END), 0)
		FROM usage_logs
		WHERE key_id = ? AND created_at >= ?`,
		now-keyWindow5h, now-keyWindow1d, now-keyWindow7d,
		k.ID, now-keyWindow7d).Scan(&u5, &u1, &u7)
	if err != nil {
		return windowUsage{}, 0, false, fmt.Errorf("查 Key 窗口用量：%w", err)
	}
	used := map[int64]int64{keyWindow5h: u5.Int64, keyWindow1d: u1.Int64, keyWindow7d: u7.Int64}

	for _, w := range wins {
		got := windowUsage{
			W:          w,
			UsedMicro:  used[w.Seconds],
			LimitMicro: billing.PointToMicro(w.Limit),
		}
		if !got.Exceeded() {
			continue
		}
		// 超限：算出"最快多久能恢复" —— 窗口内最早那笔实扣滑出窗口的时刻。
		// 这比固定回 Retry-After=窗口长度 准得多：用户已经等了 4 小时的话，
		// 告诉他"再等 5 小时"是错的。
		retry := w.Seconds
		var minAt sql.NullInt64
		if err := s.db.QueryRow(
			`SELECT MIN(created_at) FROM usage_logs WHERE key_id = ? AND created_at >= ?`,
			k.ID, now-w.Seconds).Scan(&minAt); err == nil && minAt.Valid {
			if left := minAt.Int64 + w.Seconds - now; left > 0 && left < retry {
				retry = left
			}
		}
		if retry < 1 {
			retry = 1
		}
		log.Printf("INFO: [keywindow] 拒绝 key=%d 窗口=%s 已用=%.3f 上限=%.3f 重试=%ds",
			k.ID, w.Label, billing.MicroToPoint(got.UsedMicro), w.Limit, retry)
		return got, retry, true, nil
	}
	return windowUsage{}, 0, false, nil
}

// SetKeyRateLimits 管理员设置某把 Key 的三个滚动窗口（积分，0 = 不限）。
//
// 不做"小于已用就不许设"的限制：运营常常就是想把上限压到当前用量以下来**立即止血**，
// 下一次请求就会被 429 —— 这正是预期行为。
func (s *Store) SetKeyRateLimits(keyID int64, h5, d1, d7 float64) error {
	res, err := s.db.Exec(
		`UPDATE api_keys SET rl_5h = ?, rl_1d = ?, rl_7d = ? WHERE id = ?`, h5, d1, d7, keyID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// WindowUsageForAdmin 给后台列表用：返回该 Key 当前三个窗口的已用/上限（积分）。
//
// 后台需要看到"现在用了多少"，否则填限额只能靠猜。
func (s *Store) WindowUsageForAdmin(k *APIKey, now int64) []map[string]any {
	out := make([]map[string]any, 0, 3)
	for _, w := range []keyWindow{
		{Label: "5 小时", Seconds: keyWindow5h, Limit: k.RL5h},
		{Label: "1 天", Seconds: keyWindow1d, Limit: k.RL1d},
		{Label: "7 天", Seconds: keyWindow7d, Limit: k.RL7d},
	} {
		var micro sql.NullInt64
		_ = s.db.QueryRow(
			`SELECT COALESCE(SUM(charged_micro), 0) FROM usage_logs WHERE key_id = ? AND created_at >= ?`,
			k.ID, now-w.Seconds).Scan(&micro)
		out = append(out, map[string]any{
			"label": w.Label,
			"limit": w.Limit,
			"used":  billing.MicroToPoint(micro.Int64),
		})
	}
	return out
}
