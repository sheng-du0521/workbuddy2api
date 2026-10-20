// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// ======================================================================
// 账目对账的回归网（v0.31.62，审查 F02 收尾）
//
// 对账任务的价值全在"该报的必须报"。所以每个用例都是**先造一条违规**，
// 再断言对应 code 出现；同时第一条用例证明"干净库不报任何东西" ——
// 没有这一条，后面每条"报了"都可能只是因为它本来就爱乱报。
// ======================================================================

// ledgerCodes 把发现列表压成 code 集合，便于断言。
func ledgerCodes(fs []LedgerFinding) map[string]int64 {
	m := map[string]int64{}
	for _, f := range fs {
		m[f.Code] = f.Count
	}
	return m
}

// 干净库：一条都不该报。
func TestLedgerCheckCleanStore(t *testing.T) {
	store, _ := newHostProxy(t)
	defer store.Close()

	u, err := store.CreateUser("ledger-clean", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := store.AddUserBalance(u.ID, 10*MicroPerPoint); err != nil {
		t.Fatalf("AddUserBalance: %v", err)
	}

	fs, err := store.LedgerCheck(time.Now())
	if err != nil {
		t.Fatalf("LedgerCheck: %v", err)
	}
	if len(fs) != 0 {
		t.Fatalf("干净库不该有任何发现，得 %+v", fs)
	}
}

// 可提现额度越界：能提的钱比账上多 = 最直接的资损方向。
func TestLedgerCheckWithdrawableOverflow(t *testing.T) {
	store, _ := newHostProxy(t)
	defer store.Close()

	u, err := store.CreateUser("ledger-over", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := store.db.Exec(`UPDATE users SET balance_micro = 1000, withdrawable_micro = 5000 WHERE id = ?`, u.ID); err != nil {
		t.Fatalf("造数: %v", err)
	}
	fs, err := store.LedgerCheck(time.Now())
	if err != nil {
		t.Fatalf("LedgerCheck: %v", err)
	}
	if ledgerCodes(fs)["withdrawable_overflow"] == 0 {
		t.Fatalf("应报 withdrawable_overflow，得 %+v", fs)
	}
}

// 冻结额为负 / 预扣残留超时 / 管理通道扣费：三条"不该出现"的状态。
func TestLedgerCheckStateViolations(t *testing.T) {
	store, _ := newHostProxy(t)
	defer store.Close()

	u, err := store.CreateUser("ledger-state", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := store.db.Exec(`UPDATE users SET frozen_micro = -1 WHERE id = ?`, u.ID); err != nil {
		t.Fatalf("造数(frozen): %v", err)
	}
	old := time.Now().Add(-2 * time.Hour).Unix()
	if _, err := store.db.Exec(`INSERT INTO reservations (user_id, key_id, amount_micro, model, created_at, frozen_withdrawable_micro)
		VALUES (?,?,?,?,?,?)`, u.ID, 0, 1000, "m", old, 0); err != nil {
		t.Fatalf("造数(reservation): %v", err)
	}
	if err := store.InsertUsageLog(&UsageLog{UserID: 0, Model: "m", ChargedMicro: 5, Status: "ok"}); err != nil {
		t.Fatalf("造数(admin log): %v", err)
	}

	fs, err := store.LedgerCheck(time.Now())
	if err != nil {
		t.Fatalf("LedgerCheck: %v", err)
	}
	got := ledgerCodes(fs)
	for _, code := range []string{"frozen_negative", "reservation_leak", "admin_channel_charged"} {
		if got[code] == 0 {
			t.Errorf("应报 %s，得 %+v", code, fs)
		}
	}
}

// 账号累计收益与分成台账不一致：v0.31.60 起两者同事务写入，必须逐账号相等。
func TestLedgerCheckAccountEarnedMismatch(t *testing.T) {
	store, _ := newHostProxy(t)
	defer store.Close()

	if err := store.UpsertAccount(&Account{UID: "ledger-acc", Realm: "cn", AccessToken: "a"}); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	// 只有 accounts 侧被改，台账没有对应记录 —— 典型的历史半截账。
	if _, err := store.db.Exec(`UPDATE accounts SET earned_micro = 777 WHERE uid = ?`, "ledger-acc"); err != nil {
		t.Fatalf("造数: %v", err)
	}
	fs, err := store.LedgerCheck(time.Now())
	if err != nil {
		t.Fatalf("LedgerCheck: %v", err)
	}
	if ledgerCodes(fs)["account_earned_mismatch"] == 0 {
		t.Fatalf("应报 account_earned_mismatch，得 %+v", fs)
	}
}

// 平台倒贴：同一天"分成 + 返点"超过"平台实收"。
//
// 这条是最贵的资损方向，也是最容易被人为改库破坏的约束。用例刻意**不造返点行**，
// 正好覆盖"LEFT JOIN 缺失导致整天被跳过"的那个坑（v0.31.62 写 SQL 时踩过）。
func TestLedgerCheckHostOverpay(t *testing.T) {
	store, _ := newHostProxy(t)
	defer store.Close()

	owner, err := store.CreateUser("ledger-owner", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	payer, err := store.CreateUser("ledger-payer", "x")
	if err != nil {
		t.Fatalf("CreateUser(payer): %v", err)
	}
	if err := store.UpsertAccount(&Account{UID: "ledger-pay-acc", Realm: "cn", AccessToken: "a", OwnerUserID: owner.ID, PoolState: "in"}); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	// 平台实收只有 1000，却给号主记了 5000 分成。
	if err := store.InsertUsageLog(&UsageLog{UserID: payer.ID, Model: "m", ChargedMicro: 1000, Status: "ok"}); err != nil {
		t.Fatalf("InsertUsageLog: %v", err)
	}
	if err := store.InsertHostEarning(&HostEarning{AccountUID: "ledger-pay-acc", OwnerUserID: owner.ID, Model: "m", Charged: 1000, Earned: 5000}); err != nil {
		t.Fatalf("InsertHostEarning: %v", err)
	}

	fs, err := store.LedgerCheck(time.Now())
	if err != nil {
		t.Fatalf("LedgerCheck: %v", err)
	}
	if ledgerCodes(fs)["host_overpay"] == 0 {
		t.Fatalf("应报 host_overpay（分成超过平台实收），得 %+v", fs)
	}
}

// 报告落 kv：后台接口读到的就是调度写进去的那一份。
func TestLedgerReportPersistsAndOK(t *testing.T) {
	store, a, _, _, _, _ := eaAutoApp(t)
	defer store.Close()

	rep := a.runLedgerCheck("test")
	if rep.Error != "" {
		t.Fatalf("对账不该出错：%s", rep.Error)
	}
	if !rep.OK() {
		t.Fatalf("空库应 OK，得 %+v", rep.Findings)
	}
	last, ok := a.lastLedgerCheck()
	if !ok || last.At != rep.At || last.Reason != "test" {
		t.Fatalf("结果应落 kv 并可读回，得 %+v (ok=%v)", last, ok)
	}
}

// 后台接口：默认读缓存、?run=1 立即重跑；两条都只读。
//
// 为什么要测接口而不是只测 Store：路由注册漏了/路径写错的话，Store 全绿也白搭
// （本项目已经踩过"函数写好了但没人调"的坑）。
func TestAdminLedgerCheckEndpoint(t *testing.T) {
	store, _, mux, adminCookie, _, _ := eaAutoApp(t)
	defer store.Close()

	code, body := eaReq(t, mux, "GET", "/api/admin/ledger-check?run=1", "", adminCookie, "")
	if code != 200 {
		t.Fatalf("应 200，得 %d %s", code, body)
	}
	var got struct {
		OK       bool   `json:"ok"`
		Reason   string `json:"reason"`
		Findings []struct {
			Code     string `json:"code"`
			Severity string `json:"severity"`
		} `json:"findings"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("响应不是 JSON：%v %s", err, body)
	}
	if got.Reason != "manual" {
		t.Fatalf("run=1 应标 manual，得 %q", got.Reason)
	}
	if !got.OK {
		t.Fatalf("空库应 ok=true，得 %s", body)
	}

	// 不带 run：读的是调度写下的那一份，不该再跑一遍（reason 保持不变）。
	code2, body2 := eaReq(t, mux, "GET", "/api/admin/ledger-check", "", adminCookie, "")
	if code2 != 200 || !strings.Contains(body2, `"reason":"manual"`) {
		t.Fatalf("默认应返回缓存结果（reason=manual），得 %d %s", code2, body2)
	}

	// 未登录必须被挡住（对账结果里有账号 uid 这类运营信息）。
	code3, _ := eaReq(t, mux, "GET", "/api/admin/ledger-check", "", "", "")
	if code3 == 200 {
		t.Fatal("未登录不该拿到对账结果")
	}
}
