// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// checkin_status.go 只读查询「今日是否已签到」。
//
// 为什么需要单独的查询接口：调度器的签到任务只在到点（9 点 / 21 点）跑一次，
// 结果只落在任务级台账（internal/taskledger，按任务类而非按账号）里。于是
// 「这个号今天到底签没签到」在账号卡片上无从显示——只能等下一次跑完看日志。
// 本文件给出逐账号的只读状态查询，供卡片直接展示（对齐参考实现 get_checkin_status）。
//
// 与 DailyCheckin 的关系：后者有副作用（真的去签到）；本文件只读，任何情况下
// 都不会触发签到，因此可以放心地被 UI 频繁调用。
package upstream

import (
	"encoding/json"
	"errors"
	"net/http"

	"workbuddy2api/internal/auth"
)

// 签到状态查询路径（实测）。新接口优先，失败回退旧接口：
// 上游在不同档位/灰度下会只保留其中一个，双路径保证卡片不因单点变更而瞎。
const (
	checkinActivityStatusPath = "/v2/billing/meter/checkin-activity-status"
	checkinStatusPath         = "/v2/billing/meter/checkin-status"
)

// CheckinState 单账号「今日签到」状态。
type CheckinState struct {
	// Supported 该档位是否支持签到。国际版没有签到接口（后端也会短路），
	// UI 据此隐藏签到相关展示，避免显示成「未签到」误导用户。
	Supported bool
	// CheckedIn 今日是否已签到。Supported 为 false 时无意义。
	CheckedIn bool
}

// CheckinStatus 查询账号今日签到状态。只读，不产生任何副作用。
//
// 失败语义：两条路径都失败时返回 error（调用方按「查询失败」展示），
// 不把网络抖动伪装成「未签到」——那会让用户以为需要补签而重复操作。
func (c *Client) CheckinStatus(a *auth.Auth) (*CheckinState, error) {
	if a == nil {
		return nil, errors.New("nil auth")
	}
	// 国际版：上游无签到能力，直接短路（不发无谓请求）。
	if c.globalOn(a) {
		return &CheckinState{Supported: false}, nil
	}
	st := &CheckinState{Supported: true}
	if err := c.fetchCheckinStatus(a, checkinActivityStatusPath, st); err != nil {
		if err2 := c.fetchCheckinStatus(a, checkinStatusPath, st); err2 != nil {
			// 先返回新路径的错误（更可能是真实原因），旧路径的失败只用于确认兜底也无效。
			return nil, err
		}
	}
	return st, nil
}

// fetchCheckinStatus 打一条签到状态路径并把 data 段解析进 out。
//
// 401 时刷新一次 token 重试（对齐参考实现的 checkin_request）：状态查询通常发生在
// 用户打开控制台时，而长时间停机后 access token 往往已过期，不重试会稳定显示
// 「查询失败」。刷新成功由 auth.SaveAtomic 的钩子写回数据库（见 installCredentialHook）。
func (c *Client) fetchCheckinStatus(a *auth.Auth, path string, out *CheckinState) error {
	data, err := c.billingJSON(a, http.MethodPost, path, map[string]any{})
	if err != nil {
		var ue *Error
		if errors.As(err, &ue) && ue.Status == http.StatusUnauthorized && a.RefreshToken != "" {
			if rerr := c.RefreshToken(a); rerr == nil {
				_ = a.SaveAtomic()
				data, err = c.billingJSON(a, http.MethodPost, path, map[string]any{})
			}
		}
		if err != nil {
			return err
		}
	}
	if len(data) == 0 {
		return nil
	}
	// 字段名两种形态都收：上游不同路径分别用 snake_case / camelCase。
	var resp struct {
		Snake *bool `json:"today_checked_in"`
		Camel *bool `json:"todayCheckedIn"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return err
	}
	switch {
	case resp.Snake != nil:
		out.CheckedIn = *resp.Snake
	case resp.Camel != nil:
		out.CheckedIn = *resp.Camel
	}
	return nil
}
