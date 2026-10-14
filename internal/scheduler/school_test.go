// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package scheduler

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// fakeScriptExec 记录命令构建参数并按需模拟执行失败，替代真实 exec 拉起 python3 子进程。
type fakeScriptExec struct {
	lastName string
	lastArgs []string
	lastDir  string
	runN     int
	err      error
}

func (f *fakeScriptExec) SetDir(dir string) { f.lastDir = dir }
func (f *fakeScriptExec) Run() error        { f.runN++; return f.err }

// installFakeExec 替换 newScriptCmd，测试结束还原。
func installFakeExec(t *testing.T) *fakeScriptExec {
	t.Helper()
	f := &fakeScriptExec{}
	orig := newScriptCmd
	newScriptCmd = func(name string, args ...string) scriptRunner {
		f.lastName, f.lastArgs = name, args
		return f
	}
	t.Cleanup(func() { newScriptCmd = orig })
	return f
}

func equalArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestNextWakeSchoolSlot 开学季任务在 school_hours（默认 12 点）处有独立时点。
func TestNextWakeSchoolSlot(t *testing.T) {
	s := New(Config{
		CheckinHours:      []int{9},
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		SchoolHours:       []int{12},
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 14, 11, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 14, 12, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（school 12:00 独立时点）", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskSchool {
		t.Errorf("kinds=%v want [school]", kinds)
	}
}

// TestNextWakeCatSlot 夜猫子任务在 cat_hours（默认 1 点）处有独立时点。
func TestNextWakeCatSlot(t *testing.T) {
	s := New(Config{
		CheckinHours:      []int{9},
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		CatHours:          []int{1},
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 14, 23, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 15, 1, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（cat 01:00 独立时点）", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskCat {
		t.Errorf("kinds=%v want [cat]", kinds)
	}
}

// TestNextWakeSchoolCatDisabled 显式禁用 school/cat 后排程只剩签到时点（互不影响）。
func TestNextWakeSchoolCatDisabled(t *testing.T) {
	s := New(Config{
		CheckinHours:      []int{21},
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		SchoolHours:       []int{12},
		CatHours:          []int{1},
		SchoolDisabled:    true,
		CatDisabled:       true,
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 14, 8, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 14, 21, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（school/cat 禁用 → 只有签到 21:00）", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskCheckin {
		t.Errorf("kinds=%v want [checkin]", kinds)
	}
}

// TestRunSchoolNowBuildsCommand RunSchoolNow 构造
// python3 scripts/school_open_day_2026.py ALL --run --yes，工作目录设为仓库根。
func TestRunSchoolNowBuildsCommand(t *testing.T) {
	// 防环境泄漏：WB2A_PYTHON 若在测试机已设置会改写 pythonCmd()，使默认值断言失败。
	t.Setenv("WB2A_PYTHON", "")
	f := installFakeExec(t)
	s := New(Config{})
	s.RunSchoolNow()
	if f.lastName != "python3" {
		t.Errorf("name=%q want python3", f.lastName)
	}
	want := []string{"scripts/school_open_day_2026.py", "ALL", "--run", "--yes"}
	if !equalArgs(f.lastArgs, want) {
		t.Errorf("args=%v want %v", f.lastArgs, want)
	}
	if f.lastDir != repoRoot() {
		t.Errorf("dir=%q want repo root %q", f.lastDir, repoRoot())
	}
	if _, err := os.Stat(filepath.Join(f.lastDir, "scripts", "school_open_day_2026.py")); err != nil {
		t.Errorf("仓库根 %q 内应有 scripts/school_open_day_2026.py: %v", f.lastDir, err)
	}
}

// TestRunCatNowGoNativeOutsideWindow 夜猫子已 Go 原生实现：不再拉起任何子进程。
// 窗口外（12:00 CST）应直接跳过，且不产生任何 exec。
func TestRunCatNowGoNativeOutsideWindow(t *testing.T) {
	f := installFakeExec(t)
	s := New(Config{})
	s.nowFn = func() time.Time {
		return time.Date(2026, 10, 9, 12, 0, 0, 0, time.FixedZone("CST", 8*3600))
	}
	s.RunCatNow()
	if f.runN != 0 {
		t.Fatalf("cat 已 Go 化，窗口外不应 exec 子进程；实际 runN=%d cmd=%s %v", f.runN, f.lastName, f.lastArgs)
	}
}

// withinNightWindow 边界：23:00 与 07:59 在窗口内，08:00 与 22:59 在窗口外。
func TestWithinNightWindow(t *testing.T) {
	zone := time.FixedZone("CST", 8*3600)
	cases := []struct {
		hour, min int
		want      bool
	}{
		{23, 0, true}, {23, 59, true}, {0, 0, true}, {7, 59, true},
		{8, 0, false}, {12, 0, false}, {22, 59, false},
	}
	for _, c := range cases {
		now := time.Date(2026, 10, 9, c.hour, c.min, 0, 0, zone)
		if got := withinNightWindow(now); got != c.want {
			t.Errorf("withinNightWindow(%02d:%02d)=%v want %v", c.hour, c.min, got, c.want)
		}
	}
}

// catStub 夜猫子链路的假上游：列表 → accept → report → 回读 → claim。
type catStub struct {
	mu        sync.Mutex
	accepted  bool
	reported  int
	claimed   int
	acceptHit int
	reportHit int
}

func (c *catStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/activity/growth/tasks":
			st := "not_accepted"
			cur := 0
			if c.accepted {
				st = "accepted"
			}
			cur = c.reported
			status := st
			if c.reported >= 3 {
				status = "completed"
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"tasks":[{"task_code":"black_cat","accept_status":"` +
				status + `","progress":{"current":` + itoa(cur) + `,"target":3}}]}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v2/activity/growth/tasks/accept":
			c.acceptHit++
			c.accepted = true
			_, _ = w.Write([]byte(`{"code":0,"data":{"results":[{"task_code":"black_cat","status":"accepted"}]}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v2/report":
			c.reportHit++
			c.reported++
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/activity/growth/tasks/black_cat/claim":
			c.claimed++
			_, _ = w.Write([]byte(`{"code":0,"data":{"credit":100,"energy":5}}`))
		default:
			http.Error(w, "not found", 404)
		}
	})
}

func itoa(n int) string { return strconv.Itoa(n) }

// TestRunCatNowInWindowGoNative 窗口内走完整 Go 原生链路：
// accept → 夜间上报 1 条（cap=1）→ 回读 → 未满则停。
func TestRunCatNowInWindowGoNative(t *testing.T) {
	stub := &catStub{}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	s.nowFn = func() time.Time {
		return time.Date(2026, 10, 9, 1, 0, 0, 0, time.FixedZone("CST", 8*3600))
	}
	s.RunCatNow()

	if stub.acceptHit != 1 {
		t.Errorf("accept 命中 %d 次 want 1", stub.acceptHit)
	}
	if stub.reportHit != 1 {
		t.Errorf("report 命中 %d 次 want 1（cap=1，窗口内最多补 1 次）", stub.reportHit)
	}
	if stub.claimed != 0 {
		t.Errorf("未达 target 不应 claim，实际 %d 次", stub.claimed)
	}
}

// TestRunCatNowClaimsWhenComplete 回读发现已满进度 → 触发 claim。
func TestRunCatNowClaimsWhenComplete(t *testing.T) {
	stub := &catStub{accepted: true, reported: 2} // 已有 2 条，本轮补 1 条即满
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	s.nowFn = func() time.Time {
		return time.Date(2026, 10, 9, 1, 0, 0, 0, time.FixedZone("CST", 8*3600))
	}
	s.RunCatNow()

	if stub.reportHit != 1 {
		t.Errorf("应补上报 1 条（cap=1），实际 %d", stub.reportHit)
	}
	if stub.claimed != 1 {
		t.Errorf("进度满应 claim 1 次，实际 %d", stub.claimed)
	}
}

// TestRunCatNowPrerequisiteNotMet 前置未满足（未领养 Buddy）→ 记为 skipped，
// 不是 fail（否则每个夜猫窗口都刷假失败，掩盖真故障）。
func TestRunCatNowPrerequisiteNotMet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/activity/growth/tasks":
			_, _ = w.Write([]byte(`{"code":0,"data":{"tasks":[{"task_code":"black_cat","accept_status":"not_accepted","progress":null}]}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v2/activity/growth/tasks/accept":
			_, _ = w.Write([]byte(`{"code":0,"data":{"results":[{"task_code":"black_cat","status":"error","message":"prerequisite not met: first_buddy"}]}}`))
		default:
			http.Error(w, "unexpected: "+r.URL.Path, 500)
		}
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	s.nowFn = func() time.Time {
		return time.Date(2026, 10, 9, 1, 0, 0, 0, time.FixedZone("CST", 8*3600))
	}
	s.RunCatNow()

	run, _ := s.Ledger().Runs()["cat"]
	if run.Skipped != 1 || run.Fail != 0 {
		t.Fatalf("前置未满足应记 skipped=1/fail=0，实际 skipped=%d fail=%d", run.Skipped, run.Fail)
	}
}

// TestRunCatNowAlreadyClaimed 已领 → 不 accept/不 report/不 claim。
func TestRunCatNowAlreadyClaimed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v2/activity/growth/tasks" {
			_, _ = w.Write([]byte(`{"code":0,"data":{"tasks":[{"task_code":"black_cat","accept_status":"claimed","progress":{"current":3,"target":3}}]}}`))
			return
		}
		http.Error(w, "should not be called: "+r.URL.Path, 500)
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	s.nowFn = func() time.Time {
		return time.Date(2026, 10, 9, 1, 0, 0, 0, time.FixedZone("CST", 8*3600))
	}
	s.RunCatNow() // 不应 panic / 不应打任何写接口（打错会由 stub 500 暴露为日志）
}

// TestDispatchSchoolCatAndFailureWarnsOnly dispatch 把 school 分发给脚本链路；
// 脚本失败只记 WARN（不 panic/不向上抛）。cat 已 Go 化，不再分发到脚本。
func TestDispatchSchoolCatAndFailureWarnsOnly(t *testing.T) {
	t.Setenv("WB2A_PYTHON", "")
	f := installFakeExec(t)
	f.err = errors.New("boom boom")
	s := New(Config{})

	var buf bytes.Buffer
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
	})

	s.dispatch(context.Background(), taskSchool)
	if f.runN != 1 || f.lastArgs[0] != "scripts/school_open_day_2026.py" {
		t.Errorf("dispatch(school) 未执行: runN=%d last=%v", f.runN, f.lastArgs)
	}
	// cat 已 Go 原生：窗口外应直接跳过，不产生子进程。
	s.nowFn = func() time.Time {
		return time.Date(2026, 10, 9, 12, 0, 0, 0, time.FixedZone("CST", 8*3600))
	}
	s.dispatch(context.Background(), taskCat)
	if f.runN != 1 {
		t.Errorf("cat 已 Go 化，不应再拉起脚本；runN=%d last=%v", f.runN, f.lastArgs)
	}
	out := buf.String()
	if !strings.Contains(out, "WARN") || !strings.Contains(out, "scripts/school_open_day_2026.py") {
		t.Errorf("school 失败未按 WARN 记录:\n%s", out)
	}
}

// TestPythonCmd WB2A_PYTHON 覆盖解释器名：缺省/空白回落 "python3"（保持
// 容器与既有测试的行为不变），显式设置时取其值（Windows 等仅有 python 的环境）。
func TestPythonCmd(t *testing.T) {
	t.Setenv("WB2A_PYTHON", "")
	if got := pythonCmd(); got != "python3" {
		t.Errorf("default pythonCmd()=%q want python3", got)
	}

	t.Setenv("WB2A_PYTHON", "   ")
	if got := pythonCmd(); got != "python3" {
		t.Errorf("blank pythonCmd()=%q want python3", got)
	}

	t.Setenv("WB2A_PYTHON", "python")
	if got := pythonCmd(); got != "python" {
		t.Errorf("override pythonCmd()=%q want python", got)
	}

	t.Setenv("WB2A_PYTHON", "  /usr/bin/python3.10  ")
	if got := pythonCmd(); got != "/usr/bin/python3.10" {
		t.Errorf("trim pythonCmd()=%q want /usr/bin/python3.10", got)
	}
}
