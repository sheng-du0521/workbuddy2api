// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

// poolgroup.go 号池分组的自动维护（v0.20.0）
//
// ============================================================================
// 两条规则（都只作用于「用户申请且已入池」的代挂账号）
// ============================================================================
//
//	规则 A（进预备组）：已入池账号积分 < prepEnterCredits(100) → 自动搬入 cgPrep
//	规则 B（回流）    ：预备组账号积分 >= prepReturnCredits(2000) 且**用户未主动
//	                    退出号池** → 自动搬回 cgApply（用户申请组）
//
// 100 与 2000 之间是一条**滞回带**：账号停在哪一侧都稳定，不会因积分在阈值附近
// 抖动而来回横跳（每次横跳 = 一次 store 写 + 一次选号范围变更）。详见
// consume_group.go 的 prepEnterCredits / prepReturnCredits 注释。
//
// ============================================================================
// 判定优先级（从高到低，任一命中即停）
// ============================================================================
//
//	1. 用户主动退出号池（pool_state != "in"）→ **完全不干预**（最高优先级）
//	   —— 需求原文：「用户未主动退出号池时」才回流；退出即视为放弃该账号的池内分组。
//	2. 平台自持账号（owner_user_id == 0）→ 不参与自动分组
//	   —— 它们的 consume_group 由管理员显式指定（默认推断为「反代」），
//	      自动规则只服务「用户申请的代挂号」这一条业务线。
//	3. 积分**未知**（pool 内 creditsSeenAt == 0）→ 不判定
//	   —— 关键边界：credits 是 0 既可能是「真没积分」也可能是「从没拉过」。
//	      把没拉过的号当没积分搬进预备组是误判，故要求一次权威读数（签到或余额刷新）。
//	4. **当前分组不是「参与消耗的常规组」→ 完全不干预**（v0.23.1 修复，最高优先于 5/6）
//	   —— cur 为 异常（cgAbnormal）或 闲置（cgIdle）时直接返回空。
//	      「异常」是系统归档态（带 prev/note，有独立的自动回归机制 reconcileAbnormal）；
//	      「闲置」是管理员显式排除。两者都是**有意为之**的状态，自动规则不该覆盖它们。
//	      历史缺陷：本引擎只排除了「已在预备组」，于是任何 credits<100 的异常号 / 闲置号
//	      都会被搬进预备组 —— 管理员点「异常分组」后分组在 60 秒内被改回，表现为
//	      「异常分组设置不生效」，也让异常号的归档原因与 prev 丢失。
//	5. 积分 < 100 → 目标 = 预备组（仅当 cur 属于 apply/proxy/prep 这三类常规组）
//	6. 积分 >= 2000 且当前在预备组 → 目标 = 用户申请组
//	7. 其余（滞回带内 / 已在目标组）→ 不变
//
// ============================================================================
// 触发时机（实时判定 + 定时兜底）
// ============================================================================
//
//	实时：积分每次变更后立即判定该账号一次
//	      · 余额刷新（creditsync.go 的 refreshCredits）
//	      · 请求成功后的实时扣减（proxy.go，经节流每秒最多一次/账号）
//	定时：后台 ticker 每 poolGroupReconcileInterval 全量扫描一次（兜底）
//	      —— 覆盖「重启后状态漂移」「实时钩子漏掉的分支」等场景。
//
// ============================================================================
// 并发与重复操作
// ============================================================================
//
//	· 判定是**幂等**的：目标分组等于当前生效分组时不写库（见 applyPoolGroupTarget）。
//	· 全量扫描与单账号实时判定可能并发；两者都只做「条件写 + 内存映射更新」，
//	  最终收敛到同一状态，无顺序依赖。
//	· 写库用 SetAccountSystemGroup（保留 prev/note），与管理员手动设置区分。
//	· 内存映射用 Proxy.SetConsumeGroupInMemory（O(1)），不做整表 SyncAccounts
//	  —— 自动分组在积分变更后高频触发，整表重建的代价与账号数成正比，不划算。

import (
	"context"
	"log"
	"sync"
	"time"
)

// poolGroupReconcileInterval 全量兜底扫描周期。
//
// 取 60 秒：账号积分的主要变动源（签到 09/21、请求消耗）都会先走实时判定，
// 全量扫描只是「防漏网」，不需要更快；而过快会把一次全表读 + 可能的写
// 变成持续的磁盘活动（本应用是单机 SQLite，写是串行的）。
const poolGroupReconcileInterval = 60 * time.Second

// poolGroupHotThrottle 实时判定的单账号节流窗口。
//
// 请求成功路径（NoteModelCost 之后）会逐请求触发判定，若不去重则每个请求都要
// 读一次库。窗口内同一账号只判一次：积分在 1 秒内的连续扣减不可能跨过 100/2000
// 两个阈值两次，因此不损失正确性。
const poolGroupHotThrottle = time.Second

// poolGroupGuard 实时判定的单账号节流器。
type poolGroupGuard struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// allow 报告该 uid 是否已过节流窗口（并记录本次时刻）。
func (g *poolGroupGuard) allow(uid string) bool {
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.last == nil {
		g.last = map[string]time.Time{}
	}
	if t, ok := g.last[uid]; ok && now.Sub(t) < poolGroupHotThrottle {
		return false
	}
	g.last[uid] = now
	// 惰性清理：避免长期运行后 map 无限增长（账号数有限，但删除过的号会残留）。
	if len(g.last) > 4096 {
		for k, t := range g.last {
			if now.Sub(t) > time.Hour {
				delete(g.last, k)
			}
		}
	}
	return true
}

// poolGroupTarget 纯判定：给定账号与积分，返回**目标生效分组**与写入用的备注。
//
// 返回目标为空串表示「本次不做任何自动调整」。
// creditsKnown 为 false 表示积分从未被权威拉取过（见优先级 3）。
func poolGroupTarget(acc *Account, credits int64, creditsKnown bool) (target, note string) {
	if acc == nil {
		return "", ""
	}
	// 优先级 1：用户未入池 / 已主动退出 → 完全不干预。
	if acc.PoolState != "in" {
		return "", ""
	}
	// 优先级 2：平台自持账号不参与自动分组。
	if acc.OwnerUserID <= 0 {
		return "", ""
	}
	// 优先级 3：积分未知 → 不判定（避免把「没拉过」当成「没积分」）。
	if !creditsKnown {
		return "", ""
	}
	cur := effectiveConsumeGroup(acc)
	// 优先级 4（v0.23.1）：遇到「有意为之」的分组一律不干预。
	//
	//	cgAbnormal 系统归档态 —— 由 archiveAbnormal 写入并带了 prev/note，
	//	          有独立的 reconcileAbnormal 负责回归。本引擎若在此动手，
	//	          会**覆盖归档原因与 prev**（不可逆的信息丢失），且与回归机制打架。
	//	cgIdle     管理员显式排除 —— 「暂不参与消耗」是人的决定，不该被自动规则推翻。
	//
	// 其余常规组（apply / proxy / prep / 空串推断）才由本引擎维护。
	if cur == cgAbnormal || cur == cgIdle {
		return "", ""
	}
	// 优先级 5：跌破预备线 → 进预备组。
	if credits < prepEnterCredits {
		if cur == cgPrep {
			return "", "" // 已在预备组：幂等，不重复写
		}
		return cgPrep, prepGroupNote
	}
	// 优先级 6：预备组账号积分恢复到回流线 → 回用户申请组。
	if cur == cgPrep && credits >= prepReturnCredits {
		return cgApply, ""
	}
	// 优先级 7：滞回带内或已在目标组 → 不变。
	return "", ""
}

// applyPoolGroupTarget 执行一次判定：需要变更时写库 + 更新内存映射。
// 返回是否发生了变更（供日志与测试断言）。
func (a *app) applyPoolGroupTarget(acc *Account, credits int64, creditsKnown bool) bool {
	target, note := poolGroupTarget(acc, credits, creditsKnown)
	if target == "" {
		return false
	}
	prev := effectiveConsumeGroup(acc)
	if err := a.store.SetAccountSystemGroup(acc.UID, target, prev, note); err != nil {
		log.Printf("WARN: [poolgroup] %s: set group %s failed: %v", acc.UID, target, err)
		return false
	}
	// 内存选号映射同步（O(1)，不做整表重建）。
	a.proxy.SetConsumeGroupInMemory(acc.UID, target)
	log.Printf("INFO: [poolgroup] %s credits=%d: %s -> %s%s",
		acc.UID, credits, consumeGroupLabel(prev), consumeGroupLabel(target),
		map[bool]string{true: " (auto)", false: ""}[note != ""])
	return true
}

// reconcilePoolGroupOne 对单个账号做一次实时判定（带节流）。
//
// 若该账号不在池内（未加载 / 已删除），静默返回 —— 自动分组只服务池内账号。
func (a *app) reconcilePoolGroupOne(uid string) {
	if uid == "" || a == nil || a.store == nil {
		return
	}
	a.initCaches()
	if !a.poolGroups.allow(uid) {
		return
	}
	acc, err := a.store.GetAccount(uid)
	if err != nil || acc == nil {
		return
	}
	st, ok := a.proxy.pool.Status(uid)
	if !ok {
		return // 不在池内：不判定
	}
	a.applyPoolGroupTarget(acc, st.Credits, st.CreditsSeenAt > 0)
}

// reconcilePoolGroups 全量兜底扫描：对全部账号做一次判定。
//
// 与「异常自动回归」（reconcileAbnormal）是两个独立的收敛过程，都只读库 + 条件写。
// 它们改的是同一列 consume_group，**必须靠显式排除来避免互相踩**：
//
//	· 本引擎（预备组）已显式跳过 cgAbnormal / cgIdle（见 poolGroupTarget 优先级 4）
//	  —— v0.23.1 修复：此前只跳过「已在预备组」，异常号会被本引擎搬走，
//	    与 reconcileAbnormal 的回归目标互相冲突。
//	· reconcileAbnormal 只处理带 autoTagPrefix 标记的账号，且要求池内已恢复可用
//	  （未停用 / 未手动停用 / 无冷却）。
func (a *app) reconcilePoolGroups() {
	if a == nil || a.store == nil {
		return
	}
	accounts, err := a.store.ListAccounts()
	if err != nil {
		log.Printf("WARN: [poolgroup] list accounts: %v", err)
		return
	}
	changed := 0
	for _, acc := range accounts {
		st, ok := a.proxy.pool.Status(acc.UID)
		if !ok {
			continue
		}
		if a.applyPoolGroupTarget(acc, st.Credits, st.CreditsSeenAt > 0) {
			changed++
		}
	}
	if changed > 0 {
		log.Printf("INFO: [poolgroup] reconcile done: %d account(s) regrouped", changed)
	}
}

// startPoolGroupReconciler 启动全量兜底扫描循环（阻塞式，随 ctx 取消退出）。
//
// 启动即先跑一轮：进程刚起时最需要把「重启前留下的分组」与当前积分对齐。
func (a *app) startPoolGroupReconciler(ctx context.Context) {
	go func() {
		a.reconcilePoolGroups()
		t := time.NewTicker(poolGroupReconcileInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				a.reconcilePoolGroups()
			}
		}
	}()
}
