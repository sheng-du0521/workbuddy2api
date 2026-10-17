// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 用户控制台的「导入账号 JSON」（v0.31.32）。
//
// # 为什么需要它
//
// 账号登录失效（12153 / 401）之后**只能重新登录**，而在此之前用户端只有一条路：
// 扫码。扫码适合「手上就有那台机器」，不适合「我本地有一份导出的 auth JSON」，
// 也不适合「平台在海外、扫码回调走不通」。管理员那边早就有「批量导入 JSON」，
// 用户这边没有 —— 于是失效号的恢复被卡在"找管理员"这一步。
//
// 这里补上的就是用户侧的同一条路，并且**与导出严格闭环**：
// 用户可以用控制台「导出 JSON」拿到自己的号，改天原样导回来。
//
// # 边界（都是越权/串号风险，写死在这里）
//
//  1. **只写自己名下的号**：
//     · uid 不存在 → 新建，owner = 我；
//     · uid 已存在且 owner = 我 → 更新令牌（重新登录）；
//     · uid 已存在且 owner ≠ 我（平台或其他用户）→ **跳过并说明**。
//     不能像管理端那样"直接覆盖"：那等于任何用户拿一份别人的 auth 文件就能把
//     那个号的会话换掉，而归属还挂在原号主名下 —— 两个人对同一个号各有一半权限。
//  2. **不改业务状态**：入池状态 / 分组 / 出口 / 任务配置一个都不动
//     （UpsertAccount 的 ON CONFLICT 本来就不碰这些）。
//  3. **人工停用是管理员的裁决**：重新导入不解除 disabled（但会清 token_dead，
//     因为那一位的含义是"登录失效"，重新登录后它已经不成立）。
//  4. **单次上限**：一次最多 `hostImportMaxAccounts` 条。导入是逐条写库 + 解密，
//     没有理由让人一次灌上千条把服务和上游一起拖住。
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
)

// hostImportMaxAccounts 单次用户导入的账号条数上限。
//
// 取 50：用户的代挂号通常是个位数到几十个；50 足够覆盖"一次换机全量迁移"，
// 又不至于让一次请求变成批量灌库。
const hostImportMaxAccounts = 50

// hostImportSkip 一条被跳过的记录及原因（前端逐条展示）。
type hostImportSkip struct {
	UID    string `json:"uid,omitempty"`
	Reason string `json:"reason"`
}

// handleHostImportAccounts POST /api/host/accounts/import
//
// body: {"json": "<粘贴的 auth JSON>"}   —— 与导出、与管理端导入同一套解析器
func (a *app) handleHostImportAccounts(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	if u == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "未登录"})
		return
	}
	var req struct {
		JSON string `json:"json"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	if strings.TrimSpace(req.JSON) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "内容为空：请粘贴 auth JSON（导出文件的内容）"})
		return
	}

	list, err := parseDesktopAccounts([]byte(req.JSON))
	if err != nil {
		// 原样回传解析器的错误：它会说清"应以 [ 或 { 开头""解析 JSON 对象失败"这类
		// 具体原因，比"格式错误"有用得多。
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if len(list) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "内容里没有任何账号记录（解析到 0 条）"})
		return
	}
	if len(list) > hostImportMaxAccounts {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "一次最多导入 " + itoa(hostImportMaxAccounts) + " 个账号，请分批导入",
		})
		return
	}

	var imported, updated int
	var skipped []hostImportSkip
	nicknames := make([]string, 0, len(list))
	for _, d := range list {
		acc := d.toAccount()
		if strings.TrimSpace(acc.UID) == "" {
			skipped = append(skipped, hostImportSkip{Reason: "缺少 uid：桌面端导出的记录必须带账号 uid"})
			continue
		}
		if strings.TrimSpace(acc.AccessToken) == "" {
			skipped = append(skipped, hostImportSkip{UID: acc.UID, Reason: "缺少 access_token"})
			continue
		}

		prev, _ := a.store.GetAccount(acc.UID)
		if prev != nil && prev.OwnerUserID != u.ID {
			who := "平台"
			if prev.OwnerUserID > 0 {
				who = "其他用户"
			}
			skipped = append(skipped, hostImportSkip{
				UID: acc.UID,
				Reason: "该账号已由" + who + "持有，不能导入到你名下" +
					"（如需转移，请先让原持有方删除该账号）",
			})
			continue
		}

		acc.OwnerUserID = u.ID
		// 新号默认"未申请入池"（要进号池得走申请）；已存在的号这一列不会被覆盖，
		// 因为 UpsertAccount 的 ON CONFLICT 刻意不更新 pool_state。
		acc.PoolState = ""
		if prev != nil {
			// 重新导入 = 重新登录：清掉登录失效标记；但人工停用保持原样。
			acc.Disabled = prev.Disabled
			updated++
		} else {
			imported++
		}
		if err := a.store.UpsertAccount(acc); err != nil {
			skipped = append(skipped, hostImportSkip{UID: acc.UID, Reason: "写入失败：" + err.Error()})
			if prev == nil {
				imported--
			} else {
				updated--
			}
			continue
		}
		_ = a.store.ClearTokenDead(acc.UID)
		// 来源（v0.31.37）：用户端导入与管理员导入同源同口径。
		_ = a.store.SetAccountSource(acc.UID, sourceImport)
		nicknames = append(nicknames, acc.Nickname)
	}

	// 池以库为准重建：新号的凭证要立刻可用，失效号的标记也要立刻消失。
	_ = a.proxy.SyncAccounts()
	log.Printf("INFO: [host] 用户 %s 导入账号 JSON：新增 %d、更新 %d、跳过 %d",
		u.Username, imported, updated, len(skipped))

	note := "已导入 " + itoa(imported) + " 个新账号、更新 " + itoa(updated) + " 个账号的登录状态"
	if len(skipped) > 0 {
		note += "；跳过 " + itoa(len(skipped)) + " 条（原因见下）"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"imported": imported, "updated": updated,
		"skipped": len(skipped), "skips": skipped,
		"nicknames": nicknames, "note": note,
	})
}
