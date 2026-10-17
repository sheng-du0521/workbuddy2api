// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// accountexport.go 账号导出（管理端全量 / 用户端仅自己的）。
//
// 为什么需要导出：2026-10-18 全池 token 被上游作废后，恢复路径只有"重新导入"——
// 而此前的库存只进不出，一次作废就意味着要重新去每台机器上导出 auth 文件。
// 有了导出，「备份 / 迁移 / 换机 / 误删恢复」都变成一次下载。
//
// ## 导出格式必须能"再导入"（闭环）
//
// 字段名与 `import_desktop.go` 的 `desktopAccount` 逐字对齐（`access_token` /
// `refresh_token` / `expiresAt` 毫秒 / `device_token` / `enterpriseId` …），
// 顶层是一个**数组** —— `parseDesktopAccounts` 第一条分支就吃数组。
// 这样"导出 → 导入"不需要任何转换脚本，也不会因为字段名漂移而静默丢字段
// （2026-10 之前 device_token 就是这样被丢的）。
//
// ## 安全边界（必须显式写在代码里）
//
// 导出的文件里是 **access/refresh token 原文明文** —— 拿到它就等于拿到账号。
// 所以：
//   - 管理端走 adminGuard（管理密钥或管理员会话）；
//   - 用户端只导出 `owner_user_id = 自己` 的号，绝不做"按 uid 查任意号"；
//   - 前端下载前必须二次确认并提示"该文件等同账号钥匙，请勿外传"；
//   - 不提供匿名/免鉴权入口（不要图方便加一个 `?download=1` 的公开路径）。
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// exportedAccount 导出的一行。字段名对齐导入侧 desktopAccount，保证闭环。
type exportedAccount struct {
	UID          string `json:"uid"`
	Nickname     string `json:"nickname"`
	Domain       string `json:"domain,omitempty"`
	Edition      string `json:"edition,omitempty"`
	Group        string `json:"group,omitempty"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	// ExpiresAt 毫秒（与桌面端/导入侧一致）。库里存的是秒，导出时 ×1000。
	ExpiresAt    int64  `json:"expiresAt,omitempty"`
	EnterpriseID string `json:"enterpriseId,omitempty"`
	TokenType    string `json:"token_type,omitempty"`
	DeviceToken  string `json:"device_token,omitempty"`

	// Source 来源（v0.31.37）：qr / import / 空（历史）。
	// 导入侧不认这个字段（会被忽略），但只要它是"导出→导入"同一套结构里的一项，
	// 备份文件就自带"这个号当初怎么进来的"，复盘时不必再去翻登录日志。
	Source string `json:"source,omitempty"`

	// ---- 以下为元信息：导入侧不存在这些字段，会被忽略（不参与闭环）----
	// 放它们是为了让备份文件**可读**（谁、哪个分组、什么时候导的）。
	Realm      string `json:"realm,omitempty"`
	PoolState  string `json:"pool_state,omitempty"`
	OwnerID    int64  `json:"owner_user_id,omitempty"`
	ConsumeGrp string `json:"consume_group,omitempty"`
	ExportedAt int64  `json:"exported_at"`
	PluginTag  string `json:"_exported_by,omitempty"`
}

// buildExportRows 把账号换算成导出行（纯函数，便于单测字段映射）。
// source 可传 nil（= 不带来源），避免所有既有调用点被迫改签名。
func buildExportRows(accs []*Account, now int64, tag string) []exportedAccount {
	return buildExportRowsWithSource(accs, now, tag, nil)
}

// buildExportRowsWithSource 带来源的导出行（v0.31.37）。
func buildExportRowsWithSource(accs []*Account, now int64, tag string, src map[string]string) []exportedAccount {
	out := make([]exportedAccount, 0, len(accs))
	for _, acc := range accs {
		if acc == nil {
			continue
		}
		row := exportedAccount{
			UID:          acc.UID,
			Nickname:     acc.Nickname,
			Domain:       acc.Domain,
			Edition:      acc.Realm,
			Group:        acc.Group,
			AccessToken:  acc.AccessToken,
			RefreshToken: acc.RefreshToken,
			EnterpriseID: acc.EnterpriseID,
			DeviceToken:  acc.DeviceToken,
			Realm:        acc.Realm,
			PoolState:    acc.PoolState,
			OwnerID:      acc.OwnerUserID,
			ConsumeGrp:   acc.ConsumeGroup,
			Source:       src[acc.UID],
			ExportedAt:   now,
			PluginTag:    tag,
		}
		// 秒 → 毫秒。零值保持零（导入侧会当成"无过期信息"，而不是 1970）。
		if acc.ExpiresAt > 0 {
			row.ExpiresAt = acc.ExpiresAt * 1000
		}
		if acc.AccessToken != "" {
			row.TokenType = "Bearer"
		}
		out = append(out, row)
	}
	return out
}

// handleExportAccounts GET /api/admin/accounts/export[?uids=a,b,c]
//
// 不带 uids = 导出全部；带 uids = 只导出这些（前端"导出勾选"）。
// 不存在的 uid 静默跳过（导出是**读**操作，为一条不存在的 uid 报错会让整批白跑）。
func (a *app) handleExportAccounts(w http.ResponseWriter, r *http.Request) {
	all, err := a.store.ListAccounts()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	want := parseUIDList(r.URL.Query().Get("uids"))
	accs := make([]*Account, 0, len(all))
	for _, acc := range all {
		if want != nil && !want[acc.UID] {
			continue
		}
		accs = append(accs, acc)
	}
	a.writeExport(w, accs, "admin")
}

// handleExportHostAccounts GET /api/host/accounts/export
//
// 用户只能导出**自己名下**的号。这里刻意不支持 uids 参数：
// 多一个筛选维度就多一处越权可能，而"导出我的全部"已经覆盖真实需求。
func (a *app) handleExportHostAccounts(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	if u == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "未登录"})
		return
	}
	all, err := a.store.ListAccounts()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	accs := make([]*Account, 0, len(all))
	for _, acc := range all {
		if acc.OwnerUserID == u.ID {
			accs = append(accs, acc)
		}
	}
	a.writeExport(w, accs, "host")
}

// writeExport 统一落盘响应。
func (a *app) writeExport(w http.ResponseWriter, accs []*Account, tag string) {
	// 来源一次批量取（N+1 查询在这里完全没必要，且导出动辄几十个号）。
	rows := buildExportRowsWithSource(accs, time.Now().Unix(), "wb2api/"+version+" "+tag, a.store.SourceMap())
	// 顶层数组（不是 {accounts:[…]}）：导入侧的第一条分支就吃数组，
	// 且桌面端原生导出也是数组形态 —— 少一层包装就少一处"格式对不上"。
	raw, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	name := fmt.Sprintf("wb2api-accounts-%s.json", time.Now().Format("20060102-150405"))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	// 含凭证，禁止任何中间层缓存。
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(raw)
}

// parseUIDList 解析 `uids=a,b,c`。空串返回 nil（= 不筛选）。
func parseUIDList(s string) map[string]bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	out := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		if u := strings.TrimSpace(part); u != "" {
			out[u] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
