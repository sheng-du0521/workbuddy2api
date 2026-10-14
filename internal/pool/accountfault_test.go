// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package pool

import (
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// faultFailsOf 曝露 entry.faultFails 供测试断言（包内私有 helper）。
func (p *Pool) faultFailsOf(uid string) (int, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return 0, false
	}
	return e.faultFails, true
}

// ---------------------------------------------------------------------------
// 连续 11140（request illegal）累计（v0.24.3）
//
// 背景：2026-10-13 封号事故。11140 的真实语义是 **request illegal（这次请求不合法）**，
// 由出口 IP / 频率 / 头细节触发，**不是账号封禁**。旧实现一次即 Disable 并写
// reason「account banned by upstream」，于是**一个风控窗口批量误杀 30 个健康账号**
// （抽样 7 个重探全部通过、凭证与余额完好）。
//
// 本组用例锁住新纪律：**累计到阈值才判死，中间只做可逆处置**。
// ---------------------------------------------------------------------------

// TestNoteAccountFaultThreshold 前 N-1 次不禁用、第 N 次才禁用。
func TestNoteAccountFaultThreshold(t *testing.T) {
	dir := t.TempDir()
	p := New(filepath.Join(dir, "state.json"))
	p.Add(&auth.Auth{UID: "u1"})

	for i := 1; i < accountFaultThreshold; i++ {
		fails, disabled := p.NoteAccountFault("u1")
		if disabled {
			t.Fatalf("第 %d 次不该禁用（阈值 %d）—— 一次即杀号正是本次修复的缺陷", i, accountFaultThreshold)
		}
		if fails != i {
			t.Fatalf("第 %d 次累计值=%d，期望 %d", i, fails, i)
		}
		if st, _ := p.Status("u1"); st.Disabled {
			t.Fatalf("第 %d 次账号不该 disabled", i)
		}
	}

	fails, disabled := p.NoteAccountFault("u1")
	if !disabled {
		t.Fatalf("第 %d 次应达阈值并禁用", accountFaultThreshold)
	}
	if fails != accountFaultThreshold {
		t.Fatalf("达阈值时返回的累计值=%d，期望 %d", fails, accountFaultThreshold)
	}
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Fatal("达阈值后应 disabled")
	}
	// reason 必须**如实**：不能再写 "account banned"（它会误导运维去重登，
	// 而重登解决不了请求形态问题）。
	if st.DisabledReason != accountFaultReason {
		t.Fatalf("禁用原因=%q，期望 %q", st.DisabledReason, accountFaultReason)
	}
	// 达阈值后计数归零（下次累计从 0 起，与 NoteSessionDead 同口径）。
	if n, _ := p.faultFailsOf("u1"); n != 0 {
		t.Fatalf("达阈值后计数应归零，得 %d", n)
	}
}

// TestClearAccountFault 成功（探活通过 / refresh / chat 成功）清计数。
func TestClearAccountFault(t *testing.T) {
	dir := t.TempDir()
	p := New(filepath.Join(dir, "state.json"))
	p.Add(&auth.Auth{UID: "u1"})

	p.NoteAccountFault("u1")
	if n, _ := p.faultFailsOf("u1"); n != 1 {
		t.Fatalf("应有 1 次累计，得 %d", n)
	}
	if !p.ClearAccountFault("u1") {
		t.Fatal("非零计数应被清掉并返回 true")
	}
	if n, _ := p.faultFailsOf("u1"); n != 0 {
		t.Fatalf("清后应为 0，得 %d", n)
	}
	// 幂等：本来就干净时返回 false（供调用方区分「真的清了」与「本来就干净」）。
	if p.ClearAccountFault("u1") {
		t.Fatal("本来就干净时应返回 false")
	}
}

// TestReviveDisabledClearsFault 手工复活要一并清 11140 计数。
//
// 否则会出现极隐蔽的形态：运维把号复活了，但累计计数还挂着 2/3，
// 下一次偶发抖动就凑满阈值把号重新摘掉 —— 「点了复活，过一会儿又死了」。
func TestReviveDisabledClearsFault(t *testing.T) {
	dir := t.TempDir()
	p := New(filepath.Join(dir, "state.json"))
	p.Add(&auth.Auth{UID: "u1"})

	p.NoteAccountFault("u1")
	p.NoteAccountFault("u1")
	p.Disable("u1", "test")

	if !p.ReviveDisabled("u1") {
		t.Fatal("应能复活")
	}
	if n, _ := p.faultFailsOf("u1"); n != 0 {
		t.Fatalf("复活后计数应清零，得 %d", n)
	}
}

// blockersOf 曝露熔断 / 降权截止位供测试断言（包内私有 helper，与 faultFailsOf 同风格）。
func (p *Pool) blockersOf(uid string) (breaker, degrade time.Time, fails, consecutive int, ok bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, found := p.byUID[uid]
	if !found {
		return
	}
	return e.breakerUntil, e.degradeUntil, e.fails, e.consecutiveFails, true
}

// TestReviveDisabledClearsBlockers 人工复活要一并清空**残余阻塞位**（v0.24.8）。
//
// 为什么必须有这条：选号判定（`entry.healthy`）对「任一未到期截止」都返回 false。
// 于是管理员点了「启用」之后，若该号还有未到期的熔断 / 降权截止，它在池内**依然
// 不可选**，而界面上已经显示"已启用" —— 表现为「点了启用毫无效果，也没有任何报错」。
// 这与本仓那对「两个停用位」属于同一类最难查的缺陷（都是"状态位清了、但实际行为
// 没变"），所以必须用测试钉住：**revive 之后这些截止位必须一起消失**。
func TestReviveDisabledClearsBlockers(t *testing.T) {
	dir := t.TempDir()
	p := New(filepath.Join(dir, "state.json"))
	p.Add(&auth.Auth{UID: "u1"})

	// 造出熔断（连续 5xx 达阈值）与连败降权（连续失败达阈值）。
	for i := 0; i < p.breakerThreshold; i++ {
		p.NoteError("u1")
	}
	for i := 0; i < p.degradeThreshold; i++ {
		p.NoteFailures("u1")
	}
	p.Disable("u1", "test")

	breaker, degrade, _, _, _ := p.blockersOf("u1")
	if breaker.IsZero() && degrade.IsZero() {
		t.Fatal("前置失败：没造出熔断/降权截止位，本用例测不出东西")
	}

	if !p.ReviveDisabled("u1") {
		t.Fatal("应能复活")
	}
	breaker, degrade, fails, consecutive, _ := p.blockersOf("u1")
	if !breaker.IsZero() {
		t.Fatalf("复活后熔断截止位应清空，得 %v（否则界面上『已启用』而池内仍不可选）", breaker)
	}
	if !degrade.IsZero() {
		t.Fatalf("复活后降权截止位应清空，得 %v", degrade)
	}
	if fails != 0 || consecutive != 0 {
		t.Fatalf("复活后连败计数应归零，得 fails=%d consecutive=%d", fails, consecutive)
	}
}

// TestFaultFailsPersistRoundTrip 累计计数持久化：重启不丢进度（同 sessionDeadFails 理由）。
func TestFaultFailsPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteAccountFault("u1")
	p.NoteAccountFault("u1") // =2（未达阈值 3）
	p.Flush()

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	n, ok := p2.faultFailsOf("u1")
	if !ok {
		t.Fatal("重启后账号缺失")
	}
	if n != 2 {
		t.Fatalf("恢复后 faultFails=%d want 2（连续计数未持久化）", n)
	}
	// 从恢复值继续累计：下一次即达阈值。
	if _, disabled := p2.NoteAccountFault("u1"); !disabled {
		t.Fatal("恢复计数=2 后第 3 次应达阈值禁用")
	}
}

// TestNoteSuccessClearsFault 一次成功请求就清 11140 计数（v0.24.8 修复）。
//
// 修复前的形态：NoteSuccess 清了 fails / retryCount / breakerUntil / softStreak /
// sessionDeadFails / consecutiveFails / degradeUntil 共 7 项，**唯独漏了 faultFails**
// —— 而 ClearAccountFault 的注释一直声称「chat 成功（NoteSuccess）」会清它。
// 后果是 11140 计数只增不减（唯一生产清零点只剩健康检测探活，而检测不是周期性的），
// 于是一个号在某个风控窗口吃到 2 次之后，**此后长期正常服务也清不掉**，
// 某天一次瞬态 11140 就凑满阈值被停用 / 删除 —— 跨天累积的过度处置。
func TestNoteSuccessClearsFault(t *testing.T) {
	dir := t.TempDir()
	p := New(filepath.Join(dir, "state.json"))
	p.Add(&auth.Auth{UID: "u1"})

	p.NoteAccountFault("u1")
	p.NoteAccountFault("u1")
	if n, _ := p.faultFailsOf("u1"); n != 2 {
		t.Fatalf("前置：应有 2 次累计，得 %d", n)
	}

	p.NoteSuccess("u1")
	if n, _ := p.faultFailsOf("u1"); n != 0 {
		t.Fatalf("成功请求后 11140 计数应清零（否则跨天累积误杀），得 %d", n)
	}

	// 清零后重新累计不应带着旧账：第 3 次（=清零后的第 3 次）才该达阈值。
	p.NoteAccountFault("u1")
	if _, disabled := p.NoteAccountFault("u1"); disabled {
		t.Fatal("清零后仅累计 2 次就禁用 —— 说明旧计数没有被真正清掉")
	}
}
