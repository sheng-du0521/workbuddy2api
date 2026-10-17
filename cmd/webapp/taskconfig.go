// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"workbuddy2api/internal/scheduler"
)

// kvTaskConfig 定时任务配置在 kv 表的键。
const kvTaskConfig = "task_config"

// TaskConfig 定时任务的管理端可配置项。落库为 kv 里的一段 JSON，
// 保存后由 taskRunner.Reconfigure 重建调度器即时生效。
//
// 「禁用」语义与 internal/scheduler.Config 对齐：零值 = 全部启用（hours 回落默认），
// 保证老配置/缺省行为与引入前逐字一致。
type TaskConfig struct {
	// Enabled 全局总开关。关闭后六类任务全部不再排程（手动「立即执行」仍可用）。
	Enabled bool `json:"enabled"`
	// JitterMinutes 触发时刻抖动窗口（分钟）：把整点同一秒打上游摊开，对 WAF 友好。
	JitterMinutes int `json:"jitter_minutes"`

	CheckinHours   []int `json:"checkin_hours"`
	TravelHours    []int `json:"travel_hours"`
	ActivityHours  []int `json:"activity_hours"`
	KeepaliveHours []int `json:"keepalive_hours"`
	CatHours       []int `json:"cat_hours"`

	CheckinDisabled   bool `json:"checkin_disabled"`
	TravelDisabled    bool `json:"travel_disabled"`
	ActivityDisabled  bool `json:"activity_disabled"`
	KeepaliveDisabled bool `json:"keepalive_disabled"`
	CatDisabled       bool `json:"cat_disabled"`

	// BatchStaggerSeconds 批内任务错峰上限（秒，v0.17.3）。同一唤醒时刻常派发
	// 多类任务，而它们遍历同一批账号 —— 任务之间先随机等一段再跑，避免一个账号
	// 在几十秒内被 签到 → 旅行 → 上报 连着打几轮（比「单任务高频」更明显的机器特征）。
	//
	// 0 = 关闭（行为与引入前逐字一致）；>0 时实际等待在 [min, 本值] 内随机，
	// min 见 batch_stagger_min_seconds（缺省 60）。
	BatchStaggerSeconds int `json:"batch_stagger_seconds"`
	// BatchStaggerMinSeconds 错峰下限（秒，缺省 60）。仅在上限 >0 时参与。
	BatchStaggerMinSeconds int `json:"batch_stagger_min_seconds"`

	// ActivityReportCount 每号每次活跃上报的条数（领猫前置需 5 次对话）。
	ActivityReportCount int `json:"activity_report_count"`
	// ExpiringSoonDays 快过期积分窗口（天）：签到查余额时把 N 天内到期的积分单独标记。
	ExpiringSoonDays int `json:"expiring_soon_days"`
	// KeepaliveDays 保活阈值（天）：仅当 token 在 N 天内到期才刷新。0 = 无条件刷新。
	KeepaliveDays int `json:"keepalive_days"`
	// LazyRefreshHours 惰性刷新（小时）：token 剩余有效期 > N 小时则跳过刷新。0 = 关闭。
	LazyRefreshHours int `json:"lazy_refresh_hours"`
}

// DefaultTaskConfig 缺省配置（与 internal/scheduler 的默认排程一致）。
func DefaultTaskConfig() TaskConfig {
	return TaskConfig{
		Enabled:       true,
		JitterMinutes: 5,
		CheckinHours:  []int{9, 21},
		TravelHours:   []int{9, 21},
		ActivityHours: []int{10},
		// 保活改为**每小时一次**（v0.31.29）：配合 scheduler 的按账号分散窗口，
		// 全池刷新从"13 秒内 38 个号"变成"每小时 1~2 个号"。
		// 2026-10-18 的教训：批量续期对上游是一眼可见的异常模式。
		KeepaliveHours:      hours24(),
		CatHours:            []int{1},
		ActivityReportCount: 5,
		ExpiringSoonDays:    7,
		// 批内任务错峰默认开启（60~150s）：同一时点多类任务遍历同一批账号，
		// 不错峰时一个号会在几十秒内被连打好几轮。
		BatchStaggerSeconds:    150,
		BatchStaggerMinSeconds: 60,
	}
}

// LoadTaskConfig 从 kv 读回配置；无记录或解析失败时返回缺省。
func (s *Store) LoadTaskConfig() TaskConfig {
	raw, _ := s.GetKV(kvTaskConfig)
	if strings.TrimSpace(raw) == "" {
		return DefaultTaskConfig()
	}
	tc := DefaultTaskConfig()
	if err := json.Unmarshal([]byte(raw), &tc); err != nil {
		return DefaultTaskConfig()
	}
	return tc.normalize()
}

// SaveTaskConfig 持久化配置。
func (s *Store) SaveTaskConfig(tc TaskConfig) error {
	b, err := json.Marshal(tc.normalize())
	if err != nil {
		return err
	}
	return s.SetKV(kvTaskConfig, string(b))
}

// normalize 兜底：把不合法的取值收敛到安全范围，避免脏配置把调度器搞崩。
func (tc TaskConfig) normalize() TaskConfig {
	d := DefaultTaskConfig()
	if tc.JitterMinutes < 0 {
		tc.JitterMinutes = 0
	}
	tc.CheckinHours = normHours(tc.CheckinHours, d.CheckinHours)
	tc.TravelHours = normHours(tc.TravelHours, d.TravelHours)
	tc.ActivityHours = normHours(tc.ActivityHours, d.ActivityHours)
	tc.KeepaliveHours = normHours(tc.KeepaliveHours, d.KeepaliveHours)
	tc.CatHours = normHours(tc.CatHours, d.CatHours)
	// 错峰上下限收敛：负值归零（= 关闭）；只配上限时下限回落默认 60。
	if tc.BatchStaggerSeconds < 0 {
		tc.BatchStaggerSeconds = 0
	}
	if tc.BatchStaggerMinSeconds < 0 {
		tc.BatchStaggerMinSeconds = 0
	}
	if tc.BatchStaggerSeconds > 0 && tc.BatchStaggerMinSeconds == 0 {
		tc.BatchStaggerMinSeconds = d.BatchStaggerMinSeconds
	}
	if tc.BatchStaggerMinSeconds > tc.BatchStaggerSeconds {
		tc.BatchStaggerMinSeconds = tc.BatchStaggerSeconds
	}
	if tc.ActivityReportCount <= 0 {
		tc.ActivityReportCount = d.ActivityReportCount
	}
	if tc.ExpiringSoonDays < 0 {
		tc.ExpiringSoonDays = 0
	}
	if tc.KeepaliveDays < 0 {
		tc.KeepaliveDays = 0
	}
	if tc.LazyRefreshHours < 0 {
		tc.LazyRefreshHours = 0
	}
	return tc
}

// normHours 过滤非法小时（0-23）、去重、排序；全空则回落默认。
func normHours(in, def []int) []int {
	seen := map[int]bool{}
	out := make([]int, 0, len(in))
	for _, h := range in {
		if h < 0 || h > 23 || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	if len(out) == 0 {
		return append([]int(nil), def...)
	}
	// 插入排序（≤24 个元素，无需引入 sort 依赖外的复杂度）
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// schedulerConfig 把管理端配置翻译成调度器依赖。
// growthOK=false 时强制丢弃成长任务的脚本提供方（nil = 手动执行不可用；
// 成长任务本身不排程）。
// accountTaskFilter 为逐账号任务过滤器（nil = 全部账号参与全部任务）。
func (tc TaskConfig) schedulerConfig(px *Proxy, dataDir string, growthOK bool, growth scheduler.GrowthProvider, accountTaskFilter func(uid, task string) bool) scheduler.Config {
	tc = tc.normalize()
	off := !tc.Enabled // 全局关闭 = 各类任务全禁
	// 成长任务不排程，但脚本链路不可用时应把 Provider 一并丢弃，
	// 让手动入口拿到「功能不可用」而不是跑到一半才失败。
	if !growthOK {
		growth = nil
	}
	return scheduler.Config{
		Pool:           px.pool,
		Upstream:       px.uc,
		CheckinHours:   tc.CheckinHours,
		TravelHours:    tc.TravelHours,
		ActivityHours:  tc.ActivityHours,
		KeepaliveHours: tc.KeepaliveHours,
		// 保活抖动（v0.31.29）：同一小时内轮到多个号时也不连发。固定常量而非配置项 ——
		// 它是"避免批量形态"的默认纪律，不需要运维去调。
		KeepaliveJitterMs:   800,
		CatHours:            tc.CatHours,
		JitterMinutes:       tc.JitterMinutes,
		ActivityReportCount: tc.ActivityReportCount,
		ExpiringSoonWindow:  time.Duration(tc.ExpiringSoonDays) * 24 * time.Hour,
		CheckinDisabled:     off || tc.CheckinDisabled,
		TravelDisabled:      off || tc.TravelDisabled,
		ActivityDisabled:    off || tc.ActivityDisabled,
		KeepaliveDisabled:   off || tc.KeepaliveDisabled,
		CatDisabled:         off || tc.CatDisabled,
		// 批内错峰（v0.17.3）：全局关闭时一并停掉（总开关语义与各类任务一致）。
		BatchStaggerMinSeconds: func() int {
			if off {
				return 0
			}
			return tc.BatchStaggerMinSeconds
		}(),
		BatchStaggerMaxSeconds: func() int {
			if off {
				return 0
			}
			return tc.BatchStaggerSeconds
		}(),
		Growth:            growth,
		KeepaliveDays:     tc.KeepaliveDays,
		LazyRefreshHours:  tc.LazyRefreshHours,
		LedgerFile:        filepath.Join(dataDir, "task_ledger.json"),
		AccountTaskFilter: accountTaskFilter,
	}
}

// hours24 返回 0..23（保活改为每小时一次时的默认时点，v0.31.29）。
func hours24() []int {
	out := make([]int, 24)
	for i := range out {
		out[i] = i
	}
	return out
}
