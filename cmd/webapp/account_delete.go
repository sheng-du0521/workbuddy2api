// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// account_delete.go 账号删除：单号 / 批量 / 自动（v0.24.5）。
//
// **为什么单独成文**：删除是**不可逆**操作（本项目没有账号回收站），而它有三类调用方 ——
// 单号接口、批量接口、健康检测的阈值自动删除 —— 三者的**善后动作必须完全一致**。
// 此前单号删除只做了「删 DB 行 + 池同步」两件事，健康检测快照里的旧结论会留着，
// 于是「刚删掉的号还出现在『最近一次检测结果』里」。现在统一收敛到 deleteAccountsHard。

package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"workbuddy2api/internal/upstream"
)

// kvPoolDeleteOn11140 对话探活连续失败达阈值时的动作开关。
//
//	"" / "1" → **删除账号**（v0.24.5 起的产品口径：对话通道被上游风控拦住的号
//	           已经无法服务用户，留在池里只是占名额）
//	"0"      → 退回「停用 + 归档到异常组」（可逆，便于观察上游标记是否自行解除）
//
// 为什么留这个开关：一次瞬时风控窗口（本项目 2026-10-13 已实证存在）会造成**成批**的
// 对话探活失败。删除不可逆，故必须留一条"先别删、只停用观察"的回退路径。
const kvPoolDeleteOn11140 = "pool_delete_on_11140"

// poolDeleteOn11140 读取删除开关（缺省开启）。
func (a *app) poolDeleteOn11140() bool {
	v, _ := a.store.GetKV(kvPoolDeleteOn11140)
	return strings.TrimSpace(v) != "0"
}

// retireBy11140 「11140（request illegal）连续超阈值」的**唯一处置入口**（v0.24.8）。
//
// 为什么必须收敛到一处：这个动作有多条触发链路 —— 健康检测（applyHealthDisposition）
// 与生产转发（Proxy.reportUpstreamError），而「删除还是停用」由 kv
// `pool_delete_on_11140` 决定。此前**只有健康检测读那个开关**，转发链路把动作写死成
// `RetireAccount`，于是：
//
//	· 同一个错误码，两条链路给出**不可逆程度不同**的结果（一条可能真删、一条只是停用）；
//	· 开关只覆盖其中一条，运维把 `pool_delete_on_11140=0` 当成"别删号"的保险丝，
//	  实际保护不了经转发链路触发的那些。
//
// 这正是本仓反复复发的「同一错误码多套纪律」。现在三条链路读同一个开关、走同一个函数。
//
// 返回机器可读的动作列表（健康页用），调用方直接并进自己的 acts 即可。
//
// 关于旧网关 cmd/server：那个产品**没有 DB**（配置来自 config.json），因此没有"硬删除"
// 这条路径，它的同等处置就是 `RetireAccount`（停用）——语义上等价于本开关恒为 0。
// 这是产品能力差异，不是纪律漂移。
func (a *app) retireBy11140(uid string, allowDelete bool) []string {
	const reason = "连续 11140（请求非法）超阈值，已暂停使用"
	// allowDelete=false（观测型调用：刷新积分 / 导入后刷余额）→ 强制走可逆路径。
	// 这是"刷分不该顺手删号"那条边界的落点：删除不可逆、异步、界面无确认，
	// 只允许出现在显式体检与生产转发的链路上。
	if allowDelete && a.poolDeleteOn11140() {
		if _, err := a.deleteAccountsHard([]string{uid}); err != nil {
			// 删除失败**不能静默**：账号还在池内、却报「已删除」会让运维以为干净了。
			// 退回可逆路径（停用 + 归档），并把原因写进动作列表。
			if a.proxy.pool.RetireAccount(uid, upstream.ErrAccountFault, "连续 11140 超阈值，删除失败已改为停用") {
				a.proxy.archiveAbnormal(uid, "连续 11140 超阈值，删除失败已改为停用", false)
			}
			log.Printf("WARN: [retire] account %s 达阈值删除失败（已改为停用）：%v", uid, err)
			return []string{hcActNoteFault, hcActDisabled, hcActArchived}
		}
		log.Printf("WARN: [retire] account %s 连续 11140 达阈值，已按策略删除", uid)
		return []string{hcActNoteFault, hcActDeleted}
	}
	// 开关关闭（或调用方不允许删除）：可逆处置（停用 + 归档），便于观察上游标记
	// 是否自行解除。
	if a.proxy.pool.RetireAccount(uid, upstream.ErrAccountFault, reason) {
		a.proxy.archiveAbnormal(uid, reason, false)
		return []string{hcActNoteFault, hcActDisabled, hcActArchived}
	}
	return []string{hcActNoteFault}
}

// deleteAccountsHard 删除账号并做完全部善后：DB 行 → 池内条目 → 健康检测快照。
//
// 三步缺一不可：
//   - DB 行是**唯一事实来源**，删它才算真删；
//   - 池内条目靠 SyncAccounts 重建（池自己不做增量删除），不清则选号仍可能选中已删号；
//   - 健康快照是**时点观测**不是账号属性，不清则界面持续显示已删号的旧结论。
//
// 返回实际删除的账号数（不存在的 uid 不计入）。
func (a *app) deleteAccountsHard(uids []string) (int, error) {
	if len(uids) == 0 {
		return 0, nil
	}
	n, err := a.store.DeleteAccounts(uids)
	if err != nil {
		return n, err
	}
	_ = a.proxy.SyncAccounts()
	healthLast.dropUIDs(uids)
	return n, nil
}

// handleBatchDeleteAccounts 批量删除账号。
//
// 请求体（二选一）：
//
//	{"uids": ["...","..."]}   按 uid 列表删（管理后台的勾选批量、健康页的"删失败号"）
//	{"all":  true}            删**全部**（显式全量；管理后台不提供该按钮，供脚本/DBA 用）
//
// 安全设计（删除不可逆，故刻意保守）：
//   - 两个字段都不给 / 列表为空 → 400，绝不"默认全删"（最危险的默认值）；
//   - 逐条**先查存在性**（GetAccount）再删，返回 missing 计数 —— 运维据此能看出
//     "我选的 32 个里有 2 个早已不存在"，而不是对着一个数字猜；
//   - 账目类关联行（host_earnings / account_sales / rebates / account_purchases）
//     **不级联删**，作为资金流水保留（见 Store.DeleteAccount 注释）。
func (a *app) handleBatchDeleteAccounts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UIDs []string `json:"uids"`
		All  bool     `json:"all"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	var uids []string
	if req.All {
		all, err := a.store.ListAccounts()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		for _, acc := range all {
			uids = append(uids, acc.UID)
		}
	} else {
		for _, u := range req.UIDs {
			if v := strings.TrimSpace(u); v != "" {
				uids = append(uids, v)
			}
		}
	}
	if len(uids) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "未指定要删除的账号（uids 为空且未指定 all）"})
		return
	}
	// 先做存在性核对：既能如实回报 missing，也让"删除"这个动作有一份可核对的输入清单。
	existing := make([]string, 0, len(uids))
	missing := 0
	for _, uid := range uids {
		acc, err := a.store.GetAccount(uid)
		if err != nil || acc == nil {
			missing++
			continue
		}
		existing = append(existing, uid)
	}
	deleted, err := a.deleteAccountsHard(existing)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "删除失败（已回滚，未删除任何账号）：" + err.Error()})
		return
	}
	log.Printf("WARN: [accounts] 批量删除 %d 个账号（请求 %d，其中不存在 %d）", deleted, len(uids), missing)
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted": deleted,
		"missing": missing,
		"uids":    existing,
	})
}
