// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package billing

import (
	"errors"
	"strings"
	"testing"

	"workbuddy2api/internal/usage"
)

// ======================================================================
// 结算**编排**的独立测试（v0.31.11，P2-7 第三刀下半 3b）
//
// 这一步的收益就在这里：编排过去住在转发主循环同一个文件里，要测它必须先有
// app + Store + 临时库 + 假上游。现在只要一个假 Ledger —— 于是那些"平时不跑、
// 跑错就静默错账"的分支可以被逐条钉死：
//
//	O1 预扣对账 vs 事后扣费：有预扣就不该再直接扣余额（否则**收两次钱**）；
//	O2 无预扣且扣费为 0 → 两个写入口都不许被碰（空写会污染账本）；
//	O3 结算失败必须回报 Settled=false（否则预扣悬空，钱被冻住不还）；
//	O4 扣费失败不阻断（响应已发出），但必须留痕；
//	O5 代挂自用：零扣费、零账本写、流水标 host；
//	O6 管理通道：user_id=0、零扣费、**不标 missing_credit**（它不是漏计，是本来就不收）；
//	O7 失败响应绝不估算（估算 = 凭空扣费）；
//	O8 单价观测只在"有真实用量"时发生（0 token 观测会写进 Inf）；
//	O9 流水标记的**追加顺序**（运维读流水的依据）。
// ======================================================================

type failCall struct {
	Stage, Source, Model, UID string
	UserID, KeyID             int64
	Attempt                   int
	Detail                    string
}

// fakeLedger 记录所有账本写入，并可按需注入错误。
type fakeLedger struct {
	mult      float64
	settleErr error
	chargeErr error

	// commits 记录每次 CommitSettlement 的**完整计划**（v0.31.60）：
	// 现在钱的四个落点是一次提交，用例必须能断言"这一笔计划里到底有什么"，
	// 而不是分别去看扣费/settle/日志三个切片。
	commits []Settlement
	// settles/charges 由 CommitSettlement 按计划回填，保持既有断言写法可用。
	settles  []struct{ ID, Micro int64 }
	charges  []struct{ User, Micro int64 }
	logs     []*usage.UsageLog
	failures []failCall
}

func (f *fakeLedger) PricingMultiplier() float64 { return f.mult }

// CommitSettlement 模拟"一个事务"：出错时**什么都不记**（连流水也不记），
// 这正是事务化要表达的语义 —— 旧假账本会先记 settle 再记 charge，掩盖了半截账。
func (f *fakeLedger) CommitSettlement(st Settlement) error {
	if st.HasReservation && f.settleErr != nil {
		return f.settleErr
	}
	if !st.HasReservation && st.ChargedMicro > 0 && f.chargeErr != nil {
		return f.chargeErr
	}
	f.commits = append(f.commits, st)
	if st.HasReservation {
		f.settles = append(f.settles, struct{ ID, Micro int64 }{st.ReservationID, st.ChargedMicro})
	} else if st.ChargedMicro > 0 {
		f.charges = append(f.charges, struct{ User, Micro int64 }{st.UserID, st.ChargedMicro})
	}
	log := st.Log
	f.logs = append(f.logs, &log)
	return nil
}

func (f *fakeLedger) InsertUsageLog(e *usage.UsageLog) error {
	f.logs = append(f.logs, e)
	return nil
}

func (f *fakeLedger) RecordConsumeFailure(stage, source, model, uid string,
	userID, keyID int64, attempt int, detail string) {
	f.failures = append(f.failures, failCall{stage, source, model, uid, userID, keyID, attempt, detail})
}

func (f *fakeLedger) lastLog(t *testing.T) *usage.UsageLog {
	t.Helper()
	if len(f.logs) == 0 {
		t.Fatal("应当写入一条流水")
	}
	return f.logs[len(f.logs)-1]
}

// cfg 一次结算的常用输入（测试里只改关心的字段）。
func okReq() Request {
	return Request{
		HasUser: true, UserID: 7, KeyID: 3,
		Model: "glm-5.3", UpstreamUID: "acct-1",
		Usage:  Usage{PromptTokens: 100, CompletionTokens: 50, Credit: 2.5, HasCredit: true},
		Status: "ok", Attempts: 1,
	}
}

// O1：有预扣 → 走预扣对账（退还冻结额 + 扣实际消耗），**不得**再直接扣余额。
func TestSettleWithReservationDoesNotChargeBalance(t *testing.T) {
	f := &fakeLedger{mult: 2}
	res := (Settler{Ledger: f}).Settle(func() Request {
		r := okReq()
		r.HasReservation, r.ReservationID = true, 42
		return r
	}())

	if !res.Settled {
		t.Fatal("settled 应为 true")
	}
	if res.ChargedMicro != 5000 { // 2.5 积分 × 倍率 2 = 5 点 = 5000 微点
		t.Fatalf("chargedMicro = %d，期望 5000", res.ChargedMicro)
	}
	if len(f.settles) != 1 || f.settles[0].ID != 42 || f.settles[0].Micro != 5000 {
		t.Fatalf("应对 42 号预扣做结算（5000），得 %+v", f.settles)
	}
	if len(f.charges) != 0 {
		t.Fatalf("有预扣时不得再直接扣余额（会收两次钱），得 %+v", f.charges)
	}
	// 本用例没给 Host 意图 → 这笔不产生分成（due=false），但消费积分与 scope 必须正确。
	if res.HostCreditDue || res.ConsumedCredit != 2.5 || res.Scope != ScopePool {
		t.Fatalf("结果字段不对：due=%v credit=%v scope=%q", res.HostCreditDue, res.ConsumedCredit, res.Scope)
	}
	if len(f.commits) != 1 || f.commits[0].Host != nil {
		t.Fatalf("无号主意图时不得生成分成入账，得 %+v", f.commits)
	}
}

// O2：无预扣 → 事后扣费；而**无预扣且扣费为 0**（失败请求 / 免费）→ 两个写入口都不许被碰。
func TestSettleWithoutReservationChargesBalanceOrNothing(t *testing.T) {
	t.Run("有扣费则事后扣", func(t *testing.T) {
		f := &fakeLedger{mult: 2}
		res := (Settler{Ledger: f}).Settle(okReq())
		if len(f.charges) != 1 || f.charges[0].User != 7 || f.charges[0].Micro != 5000 {
			t.Fatalf("应事后扣 7 号 5000，得 %+v", f.charges)
		}
		if len(f.settles) != 0 {
			t.Fatalf("无预扣不该有预扣结算，得 %+v", f.settles)
		}
		if res.ChargedMicro != 5000 {
			t.Fatalf("chargedMicro = %d，期望 5000", res.ChargedMicro)
		}
	})
	t.Run("扣费为 0 则两个入口都不碰", func(t *testing.T) {
		f := &fakeLedger{mult: 1}
		r := okReq()
		r.Status = "6004 model rate limit"
		r.Usage = Usage{} // 失败响应没有用量
		res := (Settler{Ledger: f}).Settle(r)
		if res.ChargedMicro != 0 || res.Settled != true {
			t.Fatalf("失败请求应零扣费且 settled=true，得 charged=%d settled=%v", res.ChargedMicro, res.Settled)
		}
		if len(f.charges) != 0 || len(f.settles) != 0 {
			t.Fatalf("零扣费不该产生任何账本金额写（空写污染账本），得 charges=%+v settles=%+v", f.charges, f.settles)
		}
		if got := f.lastLog(t); got.Status != r.Status || got.ChargedMicro != 0 || got.UpstreamCredit != 0 {
			t.Fatalf("失败流水不对：status=%q charged=%d credit=%v", got.Status, got.ChargedMicro, got.UpstreamCredit)
		}
	})
}

// O3 + O4：结算失败必须回报 Settled=false（否则预扣悬空）；扣费失败不阻断但要留痕。
func TestSettleFailureReporting(t *testing.T) {
	t.Run("预扣结算失败 → settled=false + 台账", func(t *testing.T) {
		f := &fakeLedger{mult: 1, settleErr: errors.New("db locked")}
		r := okReq()
		r.HasReservation, r.ReservationID = true, 9
		res := (Settler{Ledger: f}).Settle(r)

		if res.Settled {
			t.Fatal("结算失败必须回报 settled=false，否则调用方不会释放预扣 → 钱被冻住")
		}
		if !strings.Contains(res.ErrMsg, "[settle_failed:db locked]") {
			t.Fatalf("流水标记缺失：%q", res.ErrMsg)
		}
		if len(f.failures) != 1 || f.failures[0].Stage != StageSettle || f.failures[0].Source != "settle" {
			t.Fatalf("应记一条 settle 阶段失败台账，得 %+v", f.failures)
		}
		// v0.31.59（审查 F02）：结算失败 = 钱没扣成功 → 不得回报实扣、不得触发分成。
		// 只把 Settled 置 false 是不够的：调用方按 HostCreditDue 发钱，
		// 名义金额一旦回传，号主就会拿到一笔"用户从未付出"的可提现收益。
		if res.HostCreditDue {
			t.Fatal("结算失败仍标记 HostCreditDue=true —— 会给号主发钱，而用户一分未扣")
		}
		if res.ChargedMicro != 0 {
			t.Fatalf("结算失败不应回报实扣，得 %d", res.ChargedMicro)
		}
		// 失败也必须落流水（否则"账对不上"时无从追查）
		if got := f.lastLog(t); !strings.Contains(got.ErrorMsg, "settle_failed") {
			t.Fatalf("流水应带失败标记，得 %q", got.ErrorMsg)
		}
	})
	t.Run("事后扣费失败 → 不阻断但留痕", func(t *testing.T) {
		f := &fakeLedger{mult: 1, chargeErr: errors.New("disk full")}
		res := (Settler{Ledger: f}).Settle(okReq())

		// v0.31.59（审查 F02）语义修正：无预扣时"未结算"就等于"这笔钱没扣成功"。
		// 此前这里保持 Settled=true，调用方据此认为账已两清并发分成 ——
		// 与"预扣结算失败仍分成"是同一条缺陷的第二个入口。
		if res.Settled {
			t.Fatal("无预扣且扣费失败时必须回报 settled=false（钱没扣成功）")
		}
		if res.HostCreditDue {
			t.Fatal("扣费失败不得触发分成")
		}
		if res.ChargedMicro != 0 {
			t.Fatalf("扣费失败不应回报实扣，得 %d", res.ChargedMicro)
		}
		if !strings.Contains(res.ErrMsg, "[charge_failed:disk full]") {
			t.Fatalf("流水标记缺失：%q", res.ErrMsg)
		}
		if len(f.failures) != 1 || f.failures[0].Stage != StageCharge {
			t.Fatalf("应记一条 charge 阶段失败台账，得 %+v", f.failures)
		}
	})
}

// O5：代挂自用 Key —— 零扣费、零账本写、流水标 host、不触发分成。
func TestSettleHostSelfKeyIsFree(t *testing.T) {
	f := &fakeLedger{mult: 1}
	r := okReq()
	r.KeyKind = KeyKindHost
	res := (Settler{Ledger: f}).Settle(r)

	if res.ChargedMicro != 0 || res.Scope != ScopeHost {
		t.Fatalf("charged=%d scope=%q，期望 0 / %q", res.ChargedMicro, res.Scope, ScopeHost)
	}
	if len(f.charges) != 0 || len(f.settles) != 0 {
		t.Fatalf("代挂自用不得动账本金额，得 %+v %+v", f.charges, f.settles)
	}
	if !strings.Contains(res.ErrMsg, "[host_self]") {
		t.Fatalf("流水应带 [host_self] 标记，得 %q", res.ErrMsg)
	}
	if res.HostCreditDue {
		t.Fatal("代挂自用不该给号主分成（自己用自己的号）")
	}
	got := f.lastLog(t)
	if got.Scope != ScopeHost || got.ChargedMicro != 0 || got.UpstreamCredit != 2.5 {
		t.Fatalf("流水不对：scope=%q charged=%d credit=%v（号池消耗仍要如实记，它是分成基数）",
			got.Scope, got.ChargedMicro, got.UpstreamCredit)
	}
}

// O6：管理通道 —— user_id=0、零扣费、scope=pool、**不标 missing_credit**。
//
// 不标 missing_credit 是关键：它不是"漏计"，而是"本就不收费"。给它打上漏计标记
// 会让对账的人去追一笔根本不存在的账。
func TestSettleAdminChannel(t *testing.T) {
	f := &fakeLedger{mult: 1}
	r := okReq()
	r.HasUser = false
	r.UserID = 0
	r.Usage = Usage{PromptTokens: 10, CompletionTokens: 20} // 没有 credit
	res := (Settler{Ledger: f, Estimate: func(string, string, int) float64 { return 9 }}).Settle(r)

	if !res.Settled || res.ChargedMicro != 0 {
		t.Fatalf("管理通道应 settled=true charged=0，得 %v / %d", res.Settled, res.ChargedMicro)
	}
	if len(f.charges) != 0 || len(f.settles) != 0 {
		t.Fatalf("管理通道不得动账本金额，得 %+v %+v", f.charges, f.settles)
	}
	if strings.Contains(res.ErrMsg, "missing_credit") {
		t.Fatalf("管理通道不收费，不该标漏计：%q", res.ErrMsg)
	}
	got := f.lastLog(t)
	if got.UserID != 0 || got.ChargedMicro != 0 || got.Scope != ScopePool {
		t.Fatalf("流水不对：user=%d charged=%d scope=%q", got.UserID, got.ChargedMicro, got.Scope)
	}
	// 号池消耗仍按估算口径记（它是号池事实，与向谁收费无关）
	if got.UpstreamCredit != 9 {
		t.Fatalf("号池消耗应走估算（9），得 %v", got.UpstreamCredit)
	}
	if res.HostCreditDue {
		t.Fatal("管理通道不触发分成")
	}
}

// O7：失败响应绝不估算 —— 估算就是凭空扣费。
func TestSettleNeverEstimatesOnFailure(t *testing.T) {
	called := 0
	f := &fakeLedger{mult: 5}
	r := okReq()
	r.Status = "12153 session expired"
	r.Usage = Usage{PromptTokens: 0, CompletionTokens: 0, HasCredit: false}
	res := (Settler{Ledger: f, Estimate: func(string, string, int) float64 { called++; return 100 }}).Settle(r)

	if called != 0 {
		t.Fatalf("失败响应不得调用估算，被调用 %d 次", called)
	}
	if res.ConsumedCredit != 0 || res.ChargedMicro != 0 {
		t.Fatalf("失败应零计费，得 credit=%v charged=%d", res.ConsumedCredit, res.ChargedMicro)
	}
	if strings.Contains(res.ErrMsg, "missing_credit") {
		t.Fatalf("失败请求没有用量是正常的，不该标漏计：%q", res.ErrMsg)
	}
}

// O8：单价观测只在"有真实用量"时发生；0 token 绝不观测（那是 Inf 的入口）。
func TestSettleObservesCostOnlyWithRealUsage(t *testing.T) {
	observed := []float64{}
	f := &fakeLedger{mult: 1}
	s := Settler{Ledger: f, ObserveCost: func(model string, per1k float64) {
		observed = append(observed, per1k)
	}}

	// 正常：credit 2.5、150 token → 2.5/150*1000 ≈ 16.667
	s.Settle(okReq())
	if len(observed) != 1 {
		t.Fatalf("应观测一次，得 %v", observed)
	}
	if observed[0] < 16.66 || observed[0] > 16.67 {
		t.Fatalf("观测值应为 2.5/150*1000≈16.667，得 %v", observed[0])
	}

	// 有 credit 但没有 token → 不许观测
	observed = nil
	r := okReq()
	r.Usage = Usage{Credit: 5, HasCredit: true}
	s.Settle(r)
	if len(observed) != 0 {
		t.Fatalf("0 token 不得观测（除零得 Inf 会让后续预扣算出天文数字），得 %v", observed)
	}

	// 有 token 但没有 credit → 同样不许观测（没有单价可推）
	observed = nil
	r = okReq()
	r.Usage = Usage{PromptTokens: 10, CompletionTokens: 10}
	s.Settle(r)
	if len(observed) != 0 {
		t.Fatalf("无 credit 不得观测，得 %v", observed)
	}
}

// O9：流水标记的**追加顺序**（运维读流水的依据，顺序变了可读性就变了）。
func TestSettleMarkOrder(t *testing.T) {
	f := &fakeLedger{mult: 1}
	r := okReq()
	r.Usage = Usage{PromptTokens: 10, CompletionTokens: 10} // 无 credit → 会标 missing_credit
	r.KeyKind = KeyKindHost                                 // 再标 host_self
	res := (Settler{Ledger: f, Estimate: func(string, string, int) float64 { return 0 }}).Settle(r)

	want := "[missing_credit] [host_self]"
	if res.ErrMsg != want {
		t.Fatalf("标记顺序应为 %q，得 %q", want, res.ErrMsg)
	}
}

// 反例：Estimate / ObserveCost 为 nil 时不得 panic（它们是可选注入点）。
func TestSettleNilHooksAreSafe(t *testing.T) {
	f := &fakeLedger{mult: 1}
	r := okReq()
	r.Usage = Usage{PromptTokens: 5, CompletionTokens: 5} // 无 credit → 会走估算分支
	res := (Settler{Ledger: f}).Settle(r)                 // 两个钩子都没注入
	if res.ChargedMicro != 0 || !res.Settled {
		t.Fatalf("无钩子时应按 0 计且正常结算，得 charged=%d settled=%v", res.ChargedMicro, res.Settled)
	}
	// 管理通道同样不能 panic
	r.HasUser = false
	if res2 := (Settler{Ledger: f}).Settle(r); !res2.Settled {
		t.Fatal("管理通道 + 无钩子应正常结算")
	}
}
