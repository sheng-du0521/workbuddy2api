// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 出站口径：**统一走服务器**（v0.31.35）。
//
// ⚠️ 与 `egress_policy.go` 区分：那个文件是"线路坏掉时怎么处置"（strict/migrate），
// 本文件是"**要不用线路**"——两者是两个问题，混在一个文件里会让人以为改的是一件事。
//
// # 为什么需要这个开关
//
// 平台同时支持三种出口来源：平台线路、用户接入的住宅线路、出口代理池。
// v0.31.32~34 之间规则反复调整（可借用 → 只服务自己 → 管理员可分配），
// 每次调整都会留下存量绑定。于是需要一个**明确、可回滚、可预览**的动作：
// 把所有账号先统一交回服务器本机 IP，把住宅线路与代理池暂时放一边。
//
// # 做法（以及为什么这样最省事）
//
//	accounts.egress_proxy = ''   → 未指定，跟随全局默认
//	kv egress_proxy_default = '' → 全局默认也为空
//
// 于是 `ResolveEgress` 返回 ""，`clientFor` 走 `uc.HTTP` —— 从服务器本机 IP 直出。
// （生产无 HTTP(S)_PROXY 环境变量，`uc.HTTP` 即直连；已核对 systemd Environment 与进程 environ。）
//
// 为什么不用 `direct` 逐号写：`direct` 是"显式覆盖全局默认"的**强语义**。
// 现在要的是"暂时回到服务器"，将来恢复默认就该重新生效；写成 direct 会让后续
// 设置全局默认对这批号完全无效 —— 那时又会有人问"为什么默认不生效"。
//
// # 三条纪律
//
//  1. **可回滚**：操作前把每个号的旧值写进 kv（`egress_policy_backup`），
//     `mode=restore` 恢复（只恢复**仍存在且当前为空**的号，绝不覆盖新配置）。
//  2. **可预览**：`dry_run` 返回受影响号数与原出口分布，不写库。
//  3. **不碰线路池**：线路数据、探活、归属全不动。停用线路会触发"线路失效处置"
//     （strict 模式下绑在上面的号被判暂停服务），那不是"暂时不用"该有的副作用。
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

// kvEgressPolicyBackup 出站口径备份（JSON）。与"线路失效策略"的 kv 分开存。
const kvEgressPolicyBackup = "egress_outbound_backup"

// egressPolicyBackupRow 一个号在切换前的出口配置。
type egressPolicyBackupRow struct {
	UID   string `json:"uid"`
	Proxy string `json:"proxy"`
}

type egressPolicyBackup struct {
	At   int64                   `json:"at"`
	Rows []egressPolicyBackupRow `json:"rows"`
}

// egressPolicyView 口径快照（GET）。
type egressPolicyView struct {
	// Mode 当前口径：server（没有号绑线路）/ mixed（还有号绑着线路）。
	Mode string `json:"mode"`
	// DirectAccounts 未指定出口的号（= 跟随默认；默认为空时即从服务器本机 IP 出）。
	DirectAccounts int `json:"direct_accounts"`
	// LineAccounts 仍绑在线路上的号。
	LineAccounts int `json:"line_accounts"`
	// ExplicitDirect 显式 `direct` 的号（强语义，覆盖全局默认）。
	ExplicitDirect int `json:"explicit_direct"`
	// DefaultEgress 全局默认出口（空 = 无）。
	DefaultEgress string `json:"default_egress"`
	// BackupAt / BackupCount 上一次「统一走服务器」的时间与备份条数（0 = 没有备份）。
	BackupAt    int64 `json:"backup_at"`
	BackupCount int   `json:"backup_count"`
}

// handleEgressPolicy GET / POST /api/admin/egress/policy
func (a *app) handleEgressPolicy(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		v, err := a.egressPolicySnapshot()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, v)
		return
	}

	var req struct {
		Mode   string `json:"mode"` // server | restore
		DryRun bool   `json:"dry_run"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	mode := strings.TrimSpace(req.Mode)
	if mode == "" {
		mode = "server"
	}

	var res map[string]any
	var err error
	switch mode {
	case "server":
		res, err = a.egressPolicyToServer(req.DryRun)
	case "restore":
		res, err = a.egressPolicyRestore(req.DryRun)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "mode 只能是 server（统一走服务器）或 restore（恢复到切换前）",
		})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	res["mode"] = mode
	res["dry_run"] = req.DryRun
	writeJSON(w, http.StatusOK, res)
}

// egressPolicySnapshot 统计当前出口分布（不改任何东西）。
func (a *app) egressPolicySnapshot() (egressPolicyView, error) {
	var v egressPolicyView
	accs, err := a.store.ListAccounts()
	if err != nil {
		return v, err
	}
	for _, acc := range accs {
		cur := strings.TrimSpace(acc.EgressProxy)
		switch {
		case cur == "":
			v.DirectAccounts++
		case isDirectEgress(cur):
			v.ExplicitDirect++
		default:
			v.LineAccounts++
		}
	}
	if def, err := a.store.GetKV(kvEgressDefault); err == nil {
		v.DefaultEgress = strings.TrimSpace(def)
	}
	if raw, err := a.store.GetKV(kvEgressPolicyBackup); err == nil && strings.TrimSpace(raw) != "" {
		var b egressPolicyBackup
		if json.Unmarshal([]byte(raw), &b) == nil {
			v.BackupAt = b.At
			v.BackupCount = len(b.Rows)
		}
	}
	// 口径判断只看"还有没有号绑着线路"。显式 direct 单独报数：
	// 它虽然也不走代理，但语义是"逐号强保证"，与"跟随默认"不是一回事。
	if v.LineAccounts == 0 {
		v.Mode = "server"
	} else {
		v.Mode = "mixed"
	}
	return v, nil
}

// egressPolicyToServer 清空所有账号出口 + 清空全局默认（dry_run 只统计）。
func (a *app) egressPolicyToServer(dryRun bool) (map[string]any, error) {
	accs, err := a.store.ListAccounts()
	if err != nil {
		return nil, err
	}
	def, _ := a.store.GetKV(kvEgressDefault)
	def = strings.TrimSpace(def)

	byURL := map[string]int{}
	backup := egressPolicyBackup{At: time.Now().Unix()}
	for _, acc := range accs {
		cur := strings.TrimSpace(acc.EgressProxy)
		if cur == "" {
			continue
		}
		// 非空值全备份（含 direct）：restore 要能精确回到切换前，
		// 而不是回到"更早的某个状态"。
		backup.Rows = append(backup.Rows, egressPolicyBackupRow{UID: acc.UID, Proxy: cur})
		byURL[cur]++
	}

	res := map[string]any{
		"affected":        len(backup.Rows),
		"default_before":  def,
		"before_by_value": byURL,
		"note":            "所有账号改为「跟随默认」，全局默认清空 → 从服务器本机 IP 出站",
	}
	if dryRun {
		res["note"] = "预览：未写入任何数据"
		return res, nil
	}

	// 1) 先备份再改：中途失败也能 restore。
	if len(backup.Rows) > 0 || def != "" {
		raw, _ := json.Marshal(backup)
		if err := a.store.SetKV(kvEgressPolicyBackup, string(raw)); err != nil {
			return nil, fmt.Errorf("备份失败（未做任何修改）：%w", err)
		}
	}

	// 2) 逐号清空。刻意不用一条 UPDATE：逐号走 `SetAccountEgress` 这个统一入口，
	//    将来它若增加副作用（规范化/线路登记/审计），这条路径自动跟上。
	cleared := 0
	for _, row := range backup.Rows {
		if err := a.store.SetAccountEgress(row.UID, ""); err != nil {
			log.Printf("WARN: [egress] 清空出口失败 acct=%s: %v", row.UID, err)
			continue
		}
		cleared++
	}

	// 3) 清空全局默认。
	if def != "" {
		if err := a.store.SetKV(kvEgressDefault, ""); err != nil {
			return nil, fmt.Errorf("清空全局默认出口失败：%w", err)
		}
	}
	// 4) 运行时立即生效。
	//
	// ⚠️ 两个都要（v0.31.35 实测踩到）：`SyncAccounts` 只重建**账号级**出口映射，
	// `p.defaultEgress` 是 `ReloadSelection` 才赋值的字段。只调前者 → 数据库里
	// 默认已是空、运行时仍在用旧默认（overview 会同时显示"36 个未指定"和
	// "默认 11080"，两个数字自相矛盾）。这类"改了库没改内存"的 bug 不会报错，
	// 只会让"我明明切换了，怎么还在走住宅 IP"变成一个反复出现的疑问。
	if err := a.proxy.ApplyEgressChange(); err != nil {
		log.Printf("WARN: [egress] 出站口径变更后重建失败：%v", err)
	}

	log.Printf("INFO: [egress] 出站口径切到「统一走服务器」：清空 %d 个账号出口、全局默认 %q → 空（备份 %d 条）",
		cleared, def, len(backup.Rows))
	res["cleared"] = cleared
	res["backup_rows"] = len(backup.Rows)
	return res, nil
}

// egressPolicyRestore 从备份恢复（只恢复仍存在且当前为空的号）。
func (a *app) egressPolicyRestore(dryRun bool) (map[string]any, error) {
	raw, err := a.store.GetKV(kvEgressPolicyBackup)
	if err != nil || strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("没有可恢复的备份（还没执行过「统一走服务器」）")
	}
	var b egressPolicyBackup
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		return nil, fmt.Errorf("备份内容损坏：%w", err)
	}
	accs, err := a.store.ListAccounts()
	if err != nil {
		return nil, err
	}
	exists := map[string]bool{}
	cur := map[string]string{}
	for _, acc := range accs {
		exists[acc.UID] = true
		cur[acc.UID] = strings.TrimSpace(acc.EgressProxy)
	}

	// 只恢复到**当前为空**的号：切换后有人手工配过出口，说明那是更新的决定，
	// 不该被一次"回滚"覆盖（回滚的语义是"撤销我那次操作"，不是"回到过去"）。
	var plan []string
	skipped := 0
	for _, row := range b.Rows {
		if !exists[row.UID] || cur[row.UID] != "" {
			skipped++
			continue
		}
		plan = append(plan, row.UID)
	}
	res := map[string]any{
		"restored":       len(plan),
		"skipped":        skipped,
		"backup_at":      b.At,
		"backup_rows":    len(b.Rows),
		"note":           "只恢复「当前没有出口配置」的号；切换后手工配过的不覆盖",
		"restore_sample": sampleUIDs(plan, 5),
	}
	if dryRun {
		res["note"] = "预览：未写入任何数据"
		return res, nil
	}
	restored := 0
	for _, row := range b.Rows {
		if !exists[row.UID] || cur[row.UID] != "" {
			continue
		}
		if err := a.store.SetAccountEgress(row.UID, row.Proxy); err != nil {
			log.Printf("WARN: [egress] 恢复出口失败 acct=%s: %v", row.UID, err)
			continue
		}
		restored++
	}
	if err := a.proxy.ApplyEgressChange(); err != nil {
		log.Printf("WARN: [egress] 出站口径变更后重建失败：%v", err)
	}
	log.Printf("INFO: [egress] 出站口径从备份恢复：%d 个号，跳过 %d 条", restored, skipped)
	res["restored"] = restored
	return res, nil
}

// sampleUIDs 取前 n 个（排序后）uid，用于响应里给人看"大概动了哪些"。
func sampleUIDs(list []string, n int) []string {
	out := append([]string(nil), list...)
	sort.Strings(out)
	if len(out) > n {
		out = out[:n]
	}
	return out
}
