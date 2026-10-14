// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// growth_tasks.go growth 域「任务列表 / 接受 / 领奖」接口。
//
// 端点口径（照抄 scripts/task_common.py 实测，勿改）：
//
//	列表 : GET  {chatBase}/v2/activity/growth/tasks
//	接受 : POST {chatBase}/v2/activity/growth/tasks/accept   body {"task_codes":[...]}
//	领奖 : POST {chatBase}/activity/growth/tasks/{code}/claim  ← 无 /v2，无 body（M15 实测）
//
// 领奖路径不含 /v2 是刻意的：旧 /v2/activity/growth/tasks/reward/claim 是 404 错端点。
// 本文件只做「单个任务」的读/accept/claim 原语——批量编排（时段判定、上限、回读确认）
// 由 scheduler 负责，保持上游客户端无业务状态。
package upstream

import (
	"encoding/json"
	"net/http"

	"workbuddy2api/internal/auth"
)

const (
	growthTasksPath       = "/v2/activity/growth/tasks"
	growthAcceptTasksPath = "/v2/activity/growth/tasks/accept"
	growthClaimPrefix     = "/activity/growth/tasks/"
	growthClaimSuffix     = "/claim"
)

// GrowthTask 单个 growth 任务的状态。
// progress 在未 accept 时为 null（实测），故用指针语义的 Current/Target 兜底由调用方处理。
type GrowthTask struct {
	TaskCode     string `json:"task_code"`
	AcceptStatus string `json:"accept_status"`
	Progress     struct {
		Current int `json:"current"`
		Target  int `json:"target"`
	} `json:"progress"`
}

// accepted 判断 accept 是否已生效（accepted/completed/claimed 三态都算已登记）。
func (t *GrowthTask) Accepted() bool {
	switch t.AcceptStatus {
	case "accepted", "completed", "claimed":
		return true
	}
	return false
}

// growthTasksEnvelope 列表响应 data 段。
type growthTasksEnvelope struct {
	Tasks []GrowthTask `json:"tasks"`
}

// GrowthTasks 拉取 growth 任务列表。
func (c *Client) GrowthTasks(a *auth.Auth) ([]GrowthTask, error) {
	data, err := c.GrowthTasksRaw(a)
	if err != nil {
		return nil, err
	}
	var env growthTasksEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, err
	}
	return env.Tasks, nil
}

// GrowthTasksRaw 返回列表接口的原始 data 段（不解析），供管理端排查任务形状。
// 上游字段会增删，运维需要一个「照原样看」的出口，而不是被本地结构体过滤后的视图。
func (c *Client) GrowthTasksRaw(a *auth.Auth) (json.RawMessage, error) {
	return c.growthJSON(a, http.MethodGet, growthTasksPath, nil)
}

// GrowthTaskStatus 查询单个任务；任务不在列表中返回 (nil, nil)。
func (c *Client) GrowthTaskStatus(a *auth.Auth, code string) (*GrowthTask, error) {
	tasks, err := c.GrowthTasks(a)
	if err != nil {
		return nil, err
	}
	for i := range tasks {
		if tasks[i].TaskCode == code {
			return &tasks[i], nil
		}
	}
	return nil, nil
}

// GrowthAcceptResult accept 回执。
type GrowthAcceptResult struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

// AcceptGrowthTask 接受任务，返回上游回执（含 status 与 message）。
//
// 为什么必须回读 status 而不是只看 HTTP 200：实测存在「HTTP 200 + msg=OK 但
// status 非 accepted」的形态，此时 accept 未真正登记，后续上报全部不归账，
// 任务永远点不亮（见 scripts/task_runner.py _accept_with_verify）。
//
// message 同样必须保留：实测 black_cat 在未领养 Buddy 时返回
// status=error + message="prerequisite not met: first_buddy"——这是**正常前置未满足**，
// 不是故障；丢掉 message 就只能看到一句无信息量的「状态非 accepted」。
func (c *Client) AcceptGrowthTask(a *auth.Auth, code string) (*GrowthAcceptResult, error) {
	data, err := c.AcceptGrowthTaskRaw(a, []string{code})
	if err != nil {
		return nil, err
	}
	var env struct {
		Results []GrowthAcceptResult `json:"results"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, err
	}
	if len(env.Results) == 0 {
		return &GrowthAcceptResult{}, nil
	}
	return &env.Results[0], nil
}

// AcceptGrowthTaskRaw 返回 accept 的原始 data 段，供运维排查登记失败原因。
func (c *Client) AcceptGrowthTaskRaw(a *auth.Auth, codes []string) (json.RawMessage, error) {
	body := map[string]any{"task_codes": codes}
	return c.growthJSON(a, http.MethodPost, growthAcceptTasksPath, body)
}

// ClaimGrowthTask 领取任务奖励（无 body）。
func (c *Client) ClaimGrowthTask(a *auth.Auth, code string) error {
	path := growthClaimPrefix + code + growthClaimSuffix
	_, err := c.growthJSON(a, http.MethodPost, path, nil)
	return err
}
