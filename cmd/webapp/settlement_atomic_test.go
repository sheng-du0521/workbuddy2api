// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"testing"
)

// ======================================================================
// F02 的事务化回归网（v0.31.60）
//
// 审查报告用一个临时 SQLite trigger 证明了旧实现会留下三种**半截账**：
//
//	① 扣了钱、流水写失败  → 用户账上少了钱，报表里这笔消费不存在；
//	② 扣了钱、分成写失败  → 号主该拿的钱没有到账，且无从补；
//	③ 分成成功、返点失败  → 上级该拿的钱没有到账。
//
// 这几种在"正常"路径上永远不会出现 —— 只有磁盘满、库被锁、约束冲突时才暴露，
// 而那时正是最不能靠人肉对账的时候。所以必须用 trigger 主动制造失败来钉住。
//
// 判据统一为一句：**要么四个落点全在，要么一个都没有**。
// ======================================================================

// failTrigger 在指定表上装一个"必定失败"的 BEFORE INSERT 触发器。
func failTrigger(t *testing.T, store *Store, table string) {
	t.Helper()
	// 表名来自测试常量，不含外部输入；用字符串拼接是因为 SQLite 不允许参数化对象名。
	if _, err := store.db.Exec(`CREATE TRIGGER t_fail_` + table + ` BEFORE INSERT ON ` + table +
		` BEGIN SELECT RAISE(ABORT, 'injected-failure'); END`); err != nil {
		t.Fatalf("装触发器失败（%s）：%v", table, err)
	}
}

// newSettlementFixture 造一套"扣费 + 分成 + 返点"齐全的场景：
// 消费者有余额、号主的代挂账号已入池、号主还有上级推广人。
type settlementFixture struct {
	store   *Store
	px      *Proxy
	payer   *User
	owner   *User
	up      *User
	account string
}

func newSettlementFixture(t *testing.T) *settlementFixture {
	t.Helper()
	store, px := newHostProxy(t)
	f := &settlementFixture{store: store, px: px, account: "atomic_in"}

	payer, err := store.CreateUser("atomic-payer", "x")
	if err != nil {
		t.Fatalf("CreateUser(payer): %v", err)
	}
	if err := store.AddUserBalance(payer.ID, 100*MicroPerPoint); err != nil {
		t.Fatalf("AddUserBalance: %v", err)
	}
	owner, err := store.CreateUser("atomic-owner", "x")
	if err != nil {
		t.Fatalf("CreateUser(owner): %v", err)
	}
	up, err := store.CreateUser("atomic-up", "x")
	if err != nil {
		t.Fatalf("CreateUser(up): %v", err)
	}
	if err := store.SetUserReferredBy(owner.ID, up.ID); err != nil {
		t.Fatalf("SetUserReferredBy: %v", err)
	}
	if err := store.UpsertAccount(&Account{
		UID: f.account, Realm: "cn", AccessToken: "a", OwnerUserID: owner.ID, PoolState: "in",
	}); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	if err := store.SetKV(kvHostShareRatio, "0.6"); err != nil {
		t.Fatalf("SetKV(host share): %v", err)
	}
	if err := store.SetKV(kvReferralRatio, "0.2"); err != nil {
		t.Fatalf("SetKV(referral): %v", err)
	}
	px.ReloadSelection()
	_ = px.SyncAccounts()
	f.payer, f.owner, f.up = payer, owner, up
	return f
}

// bal 读某个用户的（总余额, 可提现额度）。
func (f *settlementFixture) bal(t *testing.T, id int64) (int64, int64) {
	t.Helper()
	u, err := f.store.GetUserByID(id)
	if err != nil || u == nil {
		t.Fatalf("GetUserByID(%d): %v", id, err)
	}
	return u.BalanceMicro, u.WithdrawableMicro
}

// count 数一张表里满足条件的行（table 只接受测试内常量）。
func (f *settlementFixture) count(t *testing.T, table, where string, args ...any) int {
	t.Helper()
	q := `SELECT COUNT(*) FROM ` + table
	if where != "" {
		q += ` WHERE ` + where
	}
	var n int
	if err := f.store.db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("count(%s): %v", table, err)
	}
	return n
}

// F02-A：无预扣 → 事后扣费；流水写失败时必须**连扣费一起回滚**。
//
// 这是最容易"看起来成功"的一条：旧实现先扣余额（提交），再写流水（失败只打日志），
// 于是用户被扣了钱而报表里查不到这笔消费 —— 客诉时无法自证。
func TestSettlementRollsBackChargeWhenLogFails(t *testing.T) {
	f := newSettlementFixture(t)
	defer f.store.Close()

	before, _ := f.bal(t, f.payer.ID)
	failTrigger(t, f.store, "usage_logs")

	settled, charged := f.px.settleUsage(f.payer, nil, "m-atomic", f.account,
		usageInfo{PromptTokens: 100, CompletionTokens: 50, Credit: 2.5, HasCredit: true},
		nil, "ok", "", "", 1)

	if settled {
		t.Fatal("流水写不进去时不得报 settled=true（会掩盖半截账）")
	}
	if charged != 0 {
		t.Fatalf("整笔回滚后 charged 必须为 0，得 %d", charged)
	}
	if after, _ := f.bal(t, f.payer.ID); after != before {
		t.Fatalf("扣费必须随事务回滚：前 %d，后 %d", before, after)
	}
	if n := f.count(t, "host_earnings", ""); n != 0 {
		t.Fatalf("整笔回滚不得留下分成记录，得 %d 条", n)
	}
	if ob, _ := f.bal(t, f.owner.ID); ob != 0 {
		t.Fatalf("整笔回滚不得给号主入账，得 %d", ob)
	}
}

// F02-B：有预扣 → 分成写失败时，预扣**不得被核销**。
//
// 核销了就等于"钱从余额里彻底消失"：预扣行没了、实扣也没记上，
// 用户既没买到服务也拿不回钱。必须回滚成"预扣仍在"，交给上层释放。
func TestSettlementKeepsReservationWhenHostCreditFails(t *testing.T) {
	f := newSettlementFixture(t)
	defer f.store.Close()

	resv, err := f.store.ReserveCharge(f.payer.ID, 0, 5000, "m-atomic")
	if err != nil {
		t.Fatalf("ReserveCharge: %v", err)
	}
	afterReserve, _ := f.bal(t, f.payer.ID)
	failTrigger(t, f.store, "host_earnings")

	settled, charged := f.px.settleUsage(f.payer, nil, "m-atomic", f.account,
		usageInfo{PromptTokens: 100, CompletionTokens: 50, Credit: 2.5, HasCredit: true},
		resv, "ok", "", "", 1)

	if settled || charged != 0 {
		t.Fatalf("分成写失败必须整笔放弃（settled=%v charged=%d）", settled, charged)
	}
	if n := f.count(t, "reservations", "id = ?", resv.ID); n != 1 {
		t.Fatalf("预扣行必须还在（交由上层释放），实际 %d 条", n)
	}
	if after, _ := f.bal(t, f.payer.ID); after != afterReserve {
		t.Fatalf("余额不得变动：预扣后 %d，失败后 %d", afterReserve, after)
	}
	// 失败也要留痕：流水必须以零费形态存在，error_msg 带标记。
	logs, err := f.store.ListUsageLogs(f.payer.ID, 5, 0)
	if err != nil || len(logs) != 1 {
		t.Fatalf("失败必须补一条降级流水，得 %d 条（err=%v）", len(logs), err)
	}
	if logs[0].ChargedMicro != 0 {
		t.Fatalf("降级流水不得写名义扣费（会虚增平台收入），得 %d", logs[0].ChargedMicro)
	}
}

// F02-C：正常路径 —— 扣费 / 流水 / 分成 / 返点四个落点必须在**一次提交**里全在。
//
// 这条防的是"事务化之后有人把某一项挪回事务外"：那样 F02-A/B 仍然会绿
// （因为没有失败可复现），但原子性已经悄悄丢了。
func TestSettlementCommitsAllFourLedgersTogether(t *testing.T) {
	f := newSettlementFixture(t)
	defer f.store.Close()

	settled, charged := f.px.settleUsage(f.payer, nil, "m-atomic", f.account,
		usageInfo{PromptTokens: 100, CompletionTokens: 50, Credit: 2.5, HasCredit: true},
		nil, "ok", "", "", 1)
	if !settled || charged <= 0 {
		t.Fatalf("正常路径应结算成功（settled=%v charged=%d）", settled, charged)
	}

	// ① 消费者余额减少 ② 流水一条 ③ 号主收益 + 可提现 ④ 上级返点 + 可提现
	if n := f.count(t, "usage_logs", "user_id = ?", f.payer.ID); n != 1 {
		t.Fatalf("应有 1 条流水，得 %d", n)
	}
	hostBal, hostWith := f.bal(t, f.owner.ID)
	if hostBal <= 0 || hostWith != hostBal {
		t.Fatalf("号主收益应入总额与可提现（同额）：bal=%d with=%d", hostBal, hostWith)
	}
	upBal, upWith := f.bal(t, f.up.ID)
	if upBal <= 0 || upWith != upBal {
		t.Fatalf("上级返点应入总额与可提现：bal=%d with=%d", upBal, upWith)
	}
	if hostBal+upBal > charged {
		t.Fatalf("分成+返点 %d 超过平台实收 %d（平台倒贴）", hostBal+upBal, charged)
	}
	if n := f.count(t, "accounts", "uid = ? AND earned_micro = ?", f.account, hostBal); n != 1 {
		t.Fatalf("账号累计收益应与号主入账一致（%d）", hostBal)
	}
}
