// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// settle_nocharge_test.go 「失败不计费」的契约。
//
// ## 用户口径（v0.26.5）
//
// 「对应模型报错、设定重置时间的，都按照时间 —— 对应模型**不消耗**进入软冷却，
//
//	其他模型可正常调用。」
//
// "不消耗"有三层含义，本文件覆盖前两层（第三层在 pool 侧）：
//
//  1. **用户侧**：预扣必须全额退回（用户没被服务，不能收钱）；
//  2. **号池侧**：没有真实用量 → 不能借"估算"之名凭空扣一次积分；
//  3. **账号侧**：冷却状态照常记录（由 pool 的用例覆盖）。
//
// 为什么值得单独钉：settleUsage 里有一处 `if status == "ok"` 的估算分支 ——
// 一旦有人把"失败"也送进估算，失败就会变成**静默扣费**，而账单上看不出异常
// （金额小、次数少），只能靠对账才发现。
package main

import (
	"testing"
	"time"

	"workbuddy2api/internal/upstream"
)

// TestSettleUsageUpstreamFailureChargesNothing 上游 6004（模型级限额）导致请求失败时：
// 预扣全额退回、计费为 0、写出一条 status 非 ok 的流水。
func TestSettleUsageUpstreamFailureChargesNothing(t *testing.T) {
	store, px, _ := newTestApp(t)
	defer store.Close()
	u := mustUser(t, store, "dave", 100)

	resv, err := store.ReserveCharge(u.ID, 0, PointToMicro(30), "glm-5.3")
	if err != nil {
		t.Fatalf("预扣失败：%v", err)
	}
	if got := balancePoints(t, store, u.ID); got != 70 {
		t.Fatalf("预扣后余额 = %v，期望 70", got)
	}

	// status 非 "ok" + usage 为空（上游错误响应没有 usage）：这是 6004 的真实形态。
	settled, charged := px.settleUsage(u, nil, "glm-5.3", "acct1", usageInfo{}, resv,
		"6004 model rate limit", `upstream 429: {"code":6004}`, "", 1)

	if !settled {
		t.Error("settled 应为 true（预扣已由结算路径处理，不能留给 defer 重复释放）")
	}
	if charged != 0 {
		t.Errorf("chargedMicro = %d，期望 0（失败不得计费）", charged)
	}
	if got := balancePoints(t, store, u.ID); got != 100 {
		t.Errorf("结算后余额 = %v，期望 100（预扣必须全额退回）", got)
	}
	// 预扣记录必须清干净：残留会让下一次启动的 ReconcileReservations 把它当孤儿处理。
	if n, _, err := store.ReconcileReservations(); err != nil || n != 0 {
		t.Errorf("失败结算后不该有残留预扣：n=%d err=%v", n, err)
	}
	// 流水必须留痕（status 非 ok），否则"被上游挡了"这件事在账单里查不到。
	logs, err := store.ListUsageLogs(u.ID, 10, 0)
	if err != nil {
		t.Fatalf("ListUsageLogs: %v", err)
	}
	if len(logs) == 0 {
		t.Fatal("失败请求也应落一条流水（0 费用），便于事后核对")
	}
	if logs[0].Status == "ok" {
		t.Errorf("流水 status = %q，失败请求不该记成 ok", logs[0].Status)
	}
	if logs[0].ChargedMicro != 0 || logs[0].UpstreamCredit != 0 {
		t.Errorf("流水费用应为 0：charged=%d credit=%v", logs[0].ChargedMicro, logs[0].UpstreamCredit)
	}
}

// TestDetailIsRecordedForConsumeLedger 失败请求必须进消耗失败台账（failures.go），
// 且带上下文（模型/uid/阶段）—— 这是"为什么这个号在这个模型上不可用"的唯一数据源。
func TestDetailIsRecordedForConsumeLedger(t *testing.T) {
	store, px, _ := newTestApp(t)
	defer store.Close()

	px.reportUpstreamError("acct1", "glm-5.3", 429, []byte(`{"code":6004,"msg":"model limit"}`),
		time.Time{}, upstream.ResetNone, []byte(`{"model":"glm-5.3","messages":[]}`))

	rows, err := store.ListConsumeFailures(50, "6004", "", 0)
	if err != nil {
		t.Fatalf("ListConsumeFailures: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("6004 失败应落 1 条台账，得 %d", len(rows))
	}
	if rows[0].Model != "glm-5.3" || rows[0].UID != "acct1" {
		t.Errorf("台账上下文缺失：model=%q uid=%q", rows[0].Model, rows[0].UID)
	}
	if rows[0].Stage != FailStageUpstream {
		t.Errorf("stage = %q，期望 %q", rows[0].Stage, FailStageUpstream)
	}
}
