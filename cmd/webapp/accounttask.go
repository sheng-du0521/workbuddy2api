// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
)

// ======================================================================
// 逐账号定时任务配置（v0.11.0）
//
// 用户控制台「账号代挂」页可为每个代挂账号开关自动任务（并支持统一设置），
// 管理后台「账号池」页亦可在每个账号上独立开关。调度器通过 AccountTaskFilter
// 逐账号过滤（见 internal/scheduler.Config.AccountTaskFilter）。
//
// 存储：accounts.task_cfg（JSON；空串 = 参与全部自动任务）。
// ======================================================================

// accountTaskKinds 任务类名（与 internal/scheduler taskKind.String() 对齐）。
//
// 不含 growth：成长任务**不参与自动排程**（v0.17.0 起改为管理员选号手动执行），
// 因此「这个号参不参与自动跑」这个开关对它没有意义 —— 留在列表里只会让管理员
// 以为关掉它就不跑，实际上真正决定跑不跑的是「有没有被选中」。
// 存量配置里残留的 growth 会在 normalize 时被静默丢弃，无需手工清理。
var accountTaskKinds = []string{"checkin", "travel", "activity", "keepalive", "cat"}

// accountTaskKindLabel 中文标签（前端下拉/校验复用）。
var accountTaskKindLabel = map[string]string{
	"checkin":   "自动签到",
	"travel":    "猫猫旅行",
	"activity":  "活跃上报",
	"keepalive": "登录保活",
	"cat":       "夜猫子",
}

// validAccountTaskKind 是否为合法任务类名。
func validAccountTaskKind(k string) bool {
	for _, x := range accountTaskKinds {
		if x == k {
			return true
		}
	}
	return false
}

// parseAccountTaskCfg 解析账号 task_cfg；空串 / 非法 = 缺省（参与全部任务）。
func parseAccountTaskCfg(raw string) AccountTaskConfig {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return AccountTaskConfig{Enabled: true}
	}
	var c AccountTaskConfig
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return AccountTaskConfig{Enabled: true}
	}
	return normalizeAccountTaskCfg(c)
}

// normalizeAccountTaskCfg 收敛：Disabled 只保留合法任务名、去重、排序。
func normalizeAccountTaskCfg(c AccountTaskConfig) AccountTaskConfig {
	seen := map[string]bool{}
	out := AccountTaskConfig{Enabled: c.Enabled}
	for _, k := range c.Disabled {
		k = strings.TrimSpace(k)
		if !validAccountTaskKind(k) || seen[k] {
			continue
		}
		seen[k] = true
		out.Disabled = append(out.Disabled, k)
	}
	sort.Strings(out.Disabled)
	return out
}

// marshalAccountTaskCfg 序列化；缺省（参与全部）存空串，保持库干净。
func marshalAccountTaskCfg(c AccountTaskConfig) string {
	c = normalizeAccountTaskCfg(c)
	if c.Enabled && len(c.Disabled) == 0 {
		return ""
	}
	b, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return string(b)
}

// accountTaskKindView 单个任务类型的对外视图。
type accountTaskKindView struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	On    bool   `json:"on"`
}

// accountTaskState 账号任务配置的对外视图。
type accountTaskState struct {
	Enabled  bool                  `json:"enabled"`
	Disabled []string              `json:"disabled"`
	Kinds    []accountTaskKindView `json:"kinds"`
}

// accountTaskStateOf 由配置算出对外视图（含每个任务类型的开关态）。
func accountTaskStateOf(c AccountTaskConfig) accountTaskState {
	c = normalizeAccountTaskCfg(c)
	off := map[string]bool{}
	for _, k := range c.Disabled {
		off[k] = true
	}
	st := accountTaskState{Enabled: c.Enabled, Disabled: c.Disabled}
	for _, k := range accountTaskKinds {
		st.Kinds = append(st.Kinds, accountTaskKindView{
			Name:  k,
			Label: accountTaskKindLabel[k],
			On:    c.Enabled && !off[k],
		})
	}
	return st
}

// loadAccountTaskCfg 全量加载 uid → 配置（启动时一次）。
func loadAccountTaskCfg(store *Store) map[string]AccountTaskConfig {
	m := map[string]AccountTaskConfig{}
	raw, err := store.TaskConfigMap()
	if err != nil {
		return m
	}
	for uid, s := range raw {
		m[uid] = parseAccountTaskCfg(s)
	}
	return m
}

// accountTaskFilter 调度器过滤器：逐账号判定是否参与某类任务（未配置 = 全参与）。
func (t *taskRunner) accountTaskFilter(uid, task string) bool {
	if t == nil {
		return true
	}
	t.taskMu.RLock()
	c, ok := t.taskCfg[uid]
	t.taskMu.RUnlock()
	if !ok {
		return true
	}
	if !c.Enabled {
		return false
	}
	for _, d := range c.Disabled {
		if d == task {
			return false
		}
	}
	return true
}

// setAccountTask 更新某账号任务配置（落库 + 更新内存缓存）。
func (t *taskRunner) setAccountTask(uid string, c AccountTaskConfig) error {
	c = normalizeAccountTaskCfg(c)
	if err := t.store.SetAccountTaskConfig(uid, marshalAccountTaskCfg(c)); err != nil {
		return err
	}
	t.taskMu.Lock()
	if c.Enabled && len(c.Disabled) == 0 {
		delete(t.taskCfg, uid)
	} else {
		t.taskCfg[uid] = c
	}
	t.taskMu.Unlock()
	return nil
}

// applyAccountTask 落库 + 尽力更新缓存（a.tasks 为 nil 时只落库）。
func (a *app) applyAccountTask(uid string, c AccountTaskConfig) error {
	c = normalizeAccountTaskCfg(c)
	raw := marshalAccountTaskCfg(c)
	if err := a.store.SetAccountTaskConfig(uid, raw); err != nil {
		return err
	}
	if a.tasks != nil {
		a.tasks.taskMu.Lock()
		if raw == "" {
			delete(a.tasks.taskCfg, uid)
		} else {
			a.tasks.taskCfg[uid] = c
		}
		a.tasks.taskMu.Unlock()
	}
	return nil
}

// decodeAccountTaskReq 解析 {enabled, disabled[]}；enabled 缺省 = true。
func decodeAccountTaskReq(r *http.Request) (AccountTaskConfig, error) {
	var req struct {
		Enabled  *bool    `json:"enabled"`
		Disabled []string `json:"disabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return AccountTaskConfig{}, err
	}
	c := AccountTaskConfig{Enabled: true, Disabled: req.Disabled}
	if req.Enabled != nil {
		c.Enabled = *req.Enabled
	}
	return normalizeAccountTaskCfg(c), nil
}

// ---------------------------------------------------------------------
// 用户侧：单个代挂账号 / 统一设置
// ---------------------------------------------------------------------

// handleHostSetTask 设置本人某个代挂账号的定时任务配置。
func (a *app) handleHostSetTask(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	uid := r.PathValue("uid")
	if a.requireOwnedAccount(w, uid, u.ID) == nil {
		return
	}
	c, err := decodeAccountTaskReq(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	if err := a.applyAccountTask(uid, c); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"uid": uid, "task": accountTaskStateOf(c)})
}

// handleHostSetTaskAll 统一设置本人「全部代挂账号」的定时任务配置。
func (a *app) handleHostSetTaskAll(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	c, err := decodeAccountTaskReq(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	list, err := a.store.ListAccountsByOwner(u.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	for _, acc := range list {
		if err := a.applyAccountTask(acc.UID, c); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": len(list), "task": accountTaskStateOf(c)})
}

// ---------------------------------------------------------------------
// 管理侧：任意账号
// ---------------------------------------------------------------------

// handleAdminSetTask 管理员对任意账号独立开关自动任务。
func (a *app) handleAdminSetTask(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if acc, err := a.store.GetAccount(uid); err != nil || acc == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "账号不存在"})
		return
	}
	c, err := decodeAccountTaskReq(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	if err := a.applyAccountTask(uid, c); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"uid": uid, "task": accountTaskStateOf(c)})
}
