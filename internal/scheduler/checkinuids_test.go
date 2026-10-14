// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package scheduler

import (
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// 本文件覆盖 v0.23.3 新增的「定向签到」入口 —— 新入池账号补跑的执行侧。
//
// 为什么要它：账号在当天签到触发点之后入池时，要等到次日才会被排到（最长 ~21h）。
// CheckinUIDs 让上层能只给这批新号补一次签到，而不把全池再打一遍。

// TestCheckinUIDsOnlyRunsGivenAccounts 定向签到只覆盖给定 uid。
//
// 关键在「只」：若实现写成"过滤失败就退化成全量"，一次新号补跑会把整池账号全部
// 再打一遍上游 —— 那正是本项目一直在避免的风控行为，而且从返回结果里看不出来
// （数量对不上，但没人会去数）。所以这里逐个 uid 断言。
func TestCheckinUIDsOnlyRunsGivenAccounts(t *testing.T) {
	stub := &checkinStub{checkinBody: `{"code":0,"msg":"ok","data":{}}`, resourceRemain: 100}
	srv := stub.server()
	defer srv.Close()

	p := pool.New("")
	for _, uid := range []string{"a", "b", "c"} {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	}
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	out, err := s.CheckinUIDs([]string{"b"})
	if err != nil {
		t.Fatalf("CheckinUIDs: %v", err)
	}
	if len(out) != 1 || out[0].UID != "b" {
		t.Fatalf("应只覆盖 b 一个账号，got %+v", out)
	}
	if out[0].Status != CheckinOK {
		t.Fatalf("b 应签到成功，got %q（%s）", out[0].Status, out[0].Detail)
	}
}

// TestCheckinUIDsEmptyIsNoop 空列表必须是 no-op，**不能**退化成"全量签到"。
//
// 这条是本函数的语义边界：nil/空 与「全量」必须区分开 —— CheckinAll 才是全量。
// 若实现里写成 `if only == nil { 全量 }` 而在空切片时构造出空 map，就会变成
// 「一个都不跑」；反之若把空切片当成 nil 传给底层，就会变成「全跑」。两种都是
// 静默的行为差异，只有测试能钉住。
func TestCheckinUIDsEmptyIsNoop(t *testing.T) {
	stub := &checkinStub{checkinBody: `{"code":0,"msg":"ok","data":{}}`, resourceRemain: 100}
	srv := stub.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "a", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	for _, in := range [][]string{nil, {}, {"", ""}} {
		out, err := s.CheckinUIDs(in)
		if err != nil || out != nil {
			t.Fatalf("空输入 %v 应为 no-op，got out=%+v err=%v", in, out, err)
		}
	}
	if got := stub.refreshCalls.Load(); got != 0 {
		t.Fatalf("no-op 不应触发任何刷新，got %d", got)
	}
}

// TestCheckinUIDsUnknownUIDProducesNothing 不在池内的 uid 不产生记录（也不报错）。
func TestCheckinUIDsUnknownUIDProducesNothing(t *testing.T) {
	stub := &checkinStub{checkinBody: `{"code":0,"msg":"ok","data":{}}`, resourceRemain: 100}
	srv := stub.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "a", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	out, err := s.CheckinUIDs([]string{"nope"})
	if err != nil {
		t.Fatalf("不该报错（未知 uid 是正常情况）：%v", err)
	}
	if len(out) != 0 {
		t.Fatalf("未知 uid 不该产生记录，got %+v", out)
	}
}

// TestCheckinUIDsNotRecordedInLedger 补跑**不写台账**。
//
// 这是刻意的：补跑通常只覆盖一两个新号，若写进台账的「最近一轮」，一次完整轮次的
// 记录就会被覆盖成「共 1 个」—— 管理员看到会以为定时任务这轮只跑了 1 个号，
// 反而制造出「定时任务坏了」的假象。
func TestCheckinUIDsNotRecordedInLedger(t *testing.T) {
	stub := &checkinStub{checkinBody: `{"code":0,"msg":"ok","data":{}}`, resourceRemain: 100}
	srv := stub.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "a", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	if _, err := s.CheckinUIDs([]string{"a"}); err != nil {
		t.Fatalf("CheckinUIDs: %v", err)
	}
	if runs := s.Ledger().Runs(); len(runs) != 0 {
		t.Fatalf("补跑不该进台账，got %v", runs)
	}
	if last := s.LastAutoRuns(); len(last) != 0 {
		t.Fatalf("补跑不该写「上次自动轮次」快照，got %v", last)
	}
}
