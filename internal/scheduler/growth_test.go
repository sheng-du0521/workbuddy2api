// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package scheduler

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fakeGrowthProvider 记录调用并按 uid 返回预置凭证（模拟真实 provider 的顺序语义）。
type fakeGrowthProvider struct {
	creds []GrowthCred
	done  []string
	err   error
}

func (f *fakeGrowthProvider) GrowthCredsByUID(uids []string) ([]GrowthCred, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]GrowthCred, 0, len(uids))
	for _, u := range uids {
		for _, c := range f.creds {
			if c.UID == u {
				out = append(out, c)
			}
		}
	}
	return out, nil
}

func (f *fakeGrowthProvider) MarkGrowthDone(uid string) error {
	f.done = append(f.done, uid)
	return nil
}

// withGrowthStubs 替换脚本可用性检查与进程执行，并返回调用记录。
func withGrowthStubs(t *testing.T, scriptOut map[string]string) *[]string {
	t.Helper()
	oldAvail := growthScriptAvailableHook
	oldExec := growthExecHook
	growthScriptAvailableHook = func() bool { return true }
	var calls []string
	growthExecHook = func(_ context.Context, payload []byte, phase string) (string, error) {
		calls = append(calls, phase)
		out, ok := scriptOut[phase]
		if !ok {
			out = `GROWTH_RESULT {"phase":"` + phase + `","ok":0,"already":10,"skip":0,"pending":0,"fail":0,"credit":0,"energy":0}`
		}
		return out, nil
	}
	t.Cleanup(func() {
		growthScriptAvailableHook = oldAvail
		growthExecHook = oldExec
	})
	return &calls
}

// newGrowthScheduler 造一个「其余任务全禁、只留 growth 执行引擎」的最小调度器。
// 注意：成长任务本来就不排程，这里禁掉其他任务只是让 nextWake 保持安静。
func newGrowthScheduler(p GrowthProvider) *Scheduler {
	return New(Config{
		Growth:            p,
		CheckinDisabled:   true,
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		SchoolDisabled:    true,
		CatDisabled:       true,
	})
}

func TestParseGrowthResult(t *testing.T) {
	// 正常：取最后一条（脚本可能在重试后打印多条）。
	out := "log line\nGROWTH_RESULT {\"phase\":\"zero\",\"ok\":3,\"already\":7,\"pending\":1,\"fail\":0,\"credit\":900}\nnoise\n" +
		"GROWTH_RESULT {\"phase\":\"zero\",\"ok\":4,\"already\":6,\"pending\":1,\"fail\":0,\"credit\":1200}"
	r := parseGrowthResult(out, "zero")
	if !r.ParsedOK || r.OK != 4 || r.Already != 6 || r.Credit != 1200 {
		t.Fatalf("应取最后一条结果行，得 %+v", r)
	}
	// 缺失结果行：ParsedOK=false（调用方据此判「未取到结果」）。
	if r2 := parseGrowthResult("no result here", "zero"); r2.ParsedOK {
		t.Fatal("无结果行时 ParsedOK 应为 false")
	}
	// 结果行是坏 JSON：同样视为未取到，而不是把零值当成功。
	if r3 := parseGrowthResult("GROWTH_RESULT {bad json", "zero"); r3.ParsedOK {
		t.Fatal("坏 JSON 应视为未解析成功")
	}
	// global 跳过形态。
	r4 := parseGrowthResult(`GROWTH_RESULT {"skipped_reason":"global realm 不适用 CN 任务"}`, "zero")
	if r4.SkippedNote == "" {
		t.Fatal("应解析出跳过原因")
	}
}

func TestGrowthGapDurationRange(t *testing.T) {
	s := New(Config{GrowthGapMinSeconds: 5, GrowthGapMaxSeconds: 8})
	for i := 0; i < 50; i++ {
		d := s.growthGapDuration()
		if d < 5*time.Second || d > 8*time.Second {
			t.Fatalf("间隔应落在 [5s,8s]，得 %s", d)
		}
	}
	// max<=min 退化为固定值（测试与「就想固定」的场景）。
	s2 := New(Config{GrowthGapMinSeconds: 3, GrowthGapMaxSeconds: 3})
	if d := s2.growthGapDuration(); d != 3*time.Second {
		t.Fatalf("max==min 应为固定 3s，得 %s", d)
	}
	// 未配置走默认（30~120）。
	s3 := New(Config{})
	d := s3.growthGapDuration()
	if d < 30*time.Second || d > 120*time.Second {
		t.Fatalf("默认间隔应在 [30s,120s]，得 %s", d)
	}
}

// TestGrowthNeverScheduled 成长任务**不得**出现在自动排程里（v0.17.0 起为纯手动）。
// 这条是本次改动的核心约束：任何回归（重新加回槽位）都会被这里挡下。
func TestGrowthNeverScheduled(t *testing.T) {
	s := New(Config{
		CheckinHours: []int{9},
		Growth:       &fakeGrowthProvider{creds: []GrowthCred{{UID: "u1", AccessToken: "at"}}},
	})
	now := time.Date(2026, 1, 1, 8, 0, 0, 0, time.Local)
	next, kinds := s.nextWake(now)
	if next.IsZero() {
		t.Fatal("应排到 9 点的签到槽位")
	}
	for _, k := range kinds {
		if k == taskGrowth {
			t.Fatalf("成长任务不应出现在排程结果里，得 %v", kinds)
		}
	}
}

// TestGrowthPhaseOrderZeroBeforeTalk 「先零消耗、后对话」是既定口径，
// 顺序写反会让新号在额度不足时直接跑对话类而失败。
func TestGrowthPhaseOrderZeroBeforeTalk(t *testing.T) {
	calls := withGrowthStubs(t, nil)
	p := &fakeGrowthProvider{creds: []GrowthCred{{UID: "u1", Nickname: "甲", AccessToken: "at"}}}
	s := newGrowthScheduler(p)

	s.RunGrowthFor(context.Background(), []string{"u1"})

	if len(*calls) != 2 || (*calls)[0] != "zero" || (*calls)[1] != "talk" {
		t.Fatalf("阶段顺序应为 zero→talk，得 %v", *calls)
	}
	if len(p.done) != 1 || p.done[0] != "u1" {
		t.Fatalf("成功后应记录执行时间，得 %v", p.done)
	}
}

// TestGrowthRunsEverySelectedUID 选中的号全部执行（手动语义：选了几个跑几个）。
func TestGrowthRunsEverySelectedUID(t *testing.T) {
	calls := withGrowthStubs(t, nil)
	creds := make([]GrowthCred, 0, 5)
	uids := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		uid := fmt.Sprintf("u%d", i)
		creds = append(creds, GrowthCred{UID: uid, AccessToken: "at"})
		uids = append(uids, uid)
	}
	p := &fakeGrowthProvider{creds: creds}
	s := newGrowthScheduler(p)
	// 号间等待压到 1s（cfg 零值会被判为「未配置」而回落默认 30s，故不能写 0）。
	s.cfg.GrowthGapMinSeconds, s.cfg.GrowthGapMaxSeconds = 1, 1

	s.RunGrowthFor(context.Background(), uids)

	// 5 个号 × 2 阶段 = 10 次调用。
	if len(*calls) != 10 {
		t.Fatalf("5 个号应产生 10 次阶段调用，得 %d（%v）", len(*calls), *calls)
	}
	if len(p.done) != 5 {
		t.Fatalf("应记录 5 个号，得 %v", p.done)
	}
}

// TestGrowthEmptySelectionIsNoop 没选号时不发任何请求（前端也会拦，后端仍需自守）。
func TestGrowthEmptySelectionIsNoop(t *testing.T) {
	calls := withGrowthStubs(t, nil)
	p := &fakeGrowthProvider{creds: []GrowthCred{{UID: "u1", AccessToken: "at"}}}
	s := newGrowthScheduler(p)

	ok, fail := s.RunGrowthFor(context.Background(), nil)

	if len(*calls) != 0 {
		t.Fatalf("空选择不应发起任何阶段调用，得 %v", *calls)
	}
	if ok != 0 || fail != 0 {
		t.Fatalf("空选择应返回 0,0，得 %d,%d", ok, fail)
	}
}

// TestGrowthZeroFailSkipsTalk zero 失败时不得继续 talk ——
// 否则新号在没攒到积分的情况下被推去做消耗额度的对话类。
func TestGrowthZeroFailSkipsTalk(t *testing.T) {
	calls := withGrowthStubs(t, map[string]string{
		"zero": `GROWTH_RESULT {"phase":"zero","ok":0,"fail":1,"error":"list_tasks 失败"}`,
	})
	p := &fakeGrowthProvider{creds: []GrowthCred{{UID: "u1", AccessToken: "at"}}}
	s := newGrowthScheduler(p)

	ok, fail := s.RunGrowthFor(context.Background(), []string{"u1"})

	if len(*calls) != 1 || (*calls)[0] != "zero" {
		t.Fatalf("zero 失败后不应继续 talk，得 %v", *calls)
	}
	if len(p.done) != 0 {
		t.Fatalf("失败不应记录执行时间（由管理员决定是否再选一次），得 %v", p.done)
	}
	if ok != 0 || fail != 1 {
		t.Fatalf("应返回 0,1，得 %d,%d", ok, fail)
	}
}

// TestGrowthTalkFailStillCountsSuccess talk 失败但 zero 已入账：仍记为成功并记录时间，
// 避免 UI 上显示成「没跑过」而诱导管理员重复跑一遍 zero。
func TestGrowthTalkFailStillCountsSuccess(t *testing.T) {
	withGrowthStubs(t, map[string]string{
		"talk": `GROWTH_RESULT {"phase":"talk","ok":0,"fail":1,"error":"额度不足"}`,
	})
	p := &fakeGrowthProvider{creds: []GrowthCred{{UID: "u1", AccessToken: "at"}}}
	s := newGrowthScheduler(p)

	ok, _ := s.RunGrowthFor(context.Background(), []string{"u1"})

	if len(p.done) != 1 {
		t.Fatalf("talk 失败但 zero 已入账时应记录执行时间，得 %v", p.done)
	}
	if ok != 1 {
		t.Fatalf("应计 1 个成功，得 %d", ok)
	}
}

// TestGrowthNilProviderSkips Provider 未注入时不 panic、不发请求。
func TestGrowthNilProviderSkips(t *testing.T) {
	withGrowthStubs(t, nil)
	s := newGrowthScheduler(nil)
	ok, fail := s.RunGrowthFor(context.Background(), []string{"u1"})
	if ok != 0 || fail != 0 {
		t.Fatalf("Provider 为空应安静跳过，得 %d,%d", ok, fail)
	}
}

// TestGrowthGlobalAccountSkipIsNotFailure 脚本返回 skipped_reason（global 账号）
// 视为成功而非失败 —— 否则 global 号每次都往台账刷一条假失败。
func TestGrowthGlobalAccountSkipIsNotFailure(t *testing.T) {
	calls := withGrowthStubs(t, map[string]string{
		"zero": `GROWTH_RESULT {"skipped_reason":"global realm 不适用 CN 任务"}`,
		"talk": `GROWTH_RESULT {"skipped_reason":"global realm 不适用 CN 任务"}`,
	})
	p := &fakeGrowthProvider{creds: []GrowthCred{{UID: "g1", Realm: "global", AccessToken: "at"}}}
	s := newGrowthScheduler(p)

	ok, _ := s.RunGrowthFor(context.Background(), []string{"g1"})

	if len(*calls) != 2 {
		t.Fatalf("global 号应照常走两个阶段（由脚本判定跳过），得 %v", *calls)
	}
	if len(p.done) != 1 || ok != 1 {
		t.Fatalf("global 跳过应视为成功并记录，得 done=%v ok=%d", p.done, ok)
	}
}

func TestGrowthTailAndTrim(t *testing.T) {
	if got := tailString("abcdef", 3); got != "…def" {
		t.Fatalf("tailString 应保留末尾，得 %q", got)
	}
	if got := tailString("abc", 10); got != "abc" {
		t.Fatalf("短串应原样返回，得 %q", got)
	}
	long := strings.Repeat("x", 700)
	if got := trimNote(long); len(got) > 610 {
		t.Fatalf("trimNote 应截断，得 %d 字符", len(got))
	}
}
