// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"workbuddy2api/internal/egress"
	"workbuddy2api/internal/scheduler"
)

// handleTaskConfig 读取/保存定时任务配置。保存后热重建调度器**即时生效**（无需重启）。
func (a *app) handleTaskConfig(w http.ResponseWriter, r *http.Request) {
	if a.tasks == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "定时任务未启用（服务以 -no-tasks 参数启动）"})
		return
	}
	if r.Method == http.MethodGet {
		resp := map[string]any{
			"config": a.tasks.Config(),
			// 成长任务：只透出脚本可用性（是否能在前端给出「跑成长任务」入口）。
			// 它不排程、无待跑队列，故没有「待跑数量」这类排程态信息。
			"growth_available": a.tasks.growthOK,
		}
		// 不可用时**才**附逐项诊断（v0.31.22）：它要 spawn 一次 `python --version`
		// 来判定解释器，可用时白跑一趟没意义；不可用时这串字恰恰是唯一能自助修的信息
		// （真机教训：VPS 漏拷 scripts/ 时，页面只说"脚本链路不可用"，看不出缺哪一项）。
		if !a.tasks.growthOK {
			resp["growth_diag"] = scheduler.GrowthDiagnosis()
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	var tc TaskConfig
	if err := json.NewDecoder(r.Body).Decode(&tc); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	if err := a.store.SaveTaskConfig(tc); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	a.tasks.Reconfigure(a.store.LoadTaskConfig())
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"config": a.tasks.Config(),
	})
}

// handleAccountGroup 设置账号分组（反代选号分类用）。空串 = 取消分组。
func (a *app) handleAccountGroup(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if uid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing uid"})
		return
	}
	var req struct {
		Group string `json:"group"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	acc, err := a.store.GetAccount(uid)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if acc == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "账号不存在"})
		return
	}
	group := strings.TrimSpace(req.Group)
	if len(group) > 32 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "分组名过长（≤32 字符）"})
		return
	}
	if err := a.store.SetAccountGroup(uid, group); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	_ = a.proxy.SyncAccounts() // 重建 uid→分组 映射
	a.proxy.ReloadSelection()
	writeJSON(w, http.StatusOK, map[string]any{"uid": uid, "group": group})
}

// handleAccountEgress 设置账号专用出站代理（http/https/socks5 URL）。空串 = 走全局默认/直连。
func (a *app) handleAccountEgress(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if uid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing uid"})
		return
	}
	var req struct {
		Proxy string `json:"proxy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	acc, err := a.store.GetAccount(uid)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if acc == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "账号不存在"})
		return
	}
	p := strings.TrimSpace(req.Proxy)
	// 校验统一走 internal/egress：与运行时路由、同步期检查同一份口径。
	// 账号级校验额外接受 direct 哨兵（显式直连，覆盖全局默认代理）。
	if _, err := egress.ValidateAccountEgress(p); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if err := a.store.SetAccountEgress(uid, p); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	_ = a.proxy.SyncAccounts()
	// 显式直连不探测（没有代理可探）；否则回报真实出口 IP 便于当场核对。
	if egress.IsDirect(p) {
		writeJSON(w, http.StatusOK, map[string]any{
			"uid": uid, "proxy": p, "direct": true,
			"exit_ip": a.localExitIP(),
			"note":    "已设为直连（不使用任何代理，含绕过环境代理）",
		})
		return
	}
	probe := egress.Probe(p)
	writeJSON(w, http.StatusOK, map[string]any{
		"uid":         uid,
		"proxy":       p,
		"verified":    probe.OK,
		"exit_ip":     probe.IP,
		"probe_note":  probe.Error,
		"unreachable": probe.EndpointUnreachable,
	})
}

// localExitIP 探测本机真实出口 IP（直连时用）。
//
// 走 egress 包（Proxy=nil，真直连）：**不**用 uc.HTTP，后者可能保留环境代理，
// 那样探测到的就不是「本机直连出口」，与显式直连的实际路径也不一致。
func (a *app) localExitIP() string {
	r := egress.Probe("")
	return r.IP
}

// sanitizeCSV 规整逗号分隔配置：逐项 trim、去空、去重、重新用逗号拼接。
func sanitizeCSV(csv string) string {
	seen := map[string]bool{}
	out := make([]string, 0, 4)
	for _, part := range strings.Split(csv, ",") {
		v := strings.TrimSpace(part)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return strings.Join(out, ",")
}
