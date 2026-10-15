// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package scheduler

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
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
	// lastStdin 记录子进程收到的标准输入（v0.26.9：开学季凭证必须走这里）。
	lastStdin string
	// out Run 时写进 output 的内容（模拟脚本的 stdout）。
	out    string
	output io.Writer
}

func (f *fakeScriptExec) SetDir(dir string) { f.lastDir = dir }
func (f *fakeScriptExec) SetStdin(r io.Reader) {
	if r == nil {
		f.lastStdin = ""
		return
	}
	b, _ := io.ReadAll(r)
	f.lastStdin = string(b)
}
func (f *fakeScriptExec) SetOutput(w io.Writer) { f.output = w }
func (f *fakeScriptExec) Run() error {
	f.runN++
	if f.output != nil && f.out != "" {
		_, _ = io.WriteString(f.output, f.out)
	}
	return f.err
}

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

// TestNextWakeCatDisabled 显式禁用夜猫子后排程只剩签到时点（互不影响）。
func TestNextWakeCatDisabled(t *testing.T) {
	s := New(Config{
		CheckinHours:      []int{21},
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		CatHours:          []int{1},
		CatDisabled:       true,
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 14, 8, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 14, 21, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（cat 禁用 → 只有签到 21:00）", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskCheckin {
		t.Errorf("kinds=%v want [checkin]", kinds)
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

// TestScriptTaskFailureWarnsOnly 脚本链路失败只记 WARN（不 panic、不向上抛）。
//
// v0.27.0 起不再用 dispatch 驱动脚本任务（开学季移除后已无排程内的脚本类任务），
// 改为直接调用 runScriptTask —— 被验证的是"脚本失败怎么记账"这件事本身。
func TestScriptTaskFailureWarnsOnly(t *testing.T) {
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

	s.runScriptTask(taskGrowth, triggerManual, "growth", [][]string{
		{"python", "scripts/growth_run.py", "--phase", "zero"},
	}, []byte("[]"))
	if f.runN != 1 || f.lastArgs[0] != "scripts/growth_run.py" {
		t.Errorf("脚本未执行: runN=%d last=%v", f.runN, f.lastArgs)
	}
	out := buf.String()
	if !strings.Contains(out, "WARN") || !strings.Contains(out, "scripts/growth_run.py") {
		t.Errorf("脚本失败未按 WARN 记录:\n%s", out)
	}
}

// TestDispatchCatNeverSpawnsScript cat 已 Go 原生实现：dispatch 它**不得**拉起任何
// 子进程（窗口外应直接跳过）。这条守的是"Go 化"这个决定本身。
func TestDispatchCatNeverSpawnsScript(t *testing.T) {
	t.Setenv("WB2A_PYTHON", "")
	f := installFakeExec(t)
	s := New(Config{})
	s.nowFn = func() time.Time {
		return time.Date(2026, 10, 9, 12, 0, 0, 0, time.FixedZone("CST", 8*3600))
	}
	s.dispatch(context.Background(), taskCat)
	if f.runN != 0 {
		t.Errorf("cat 已 Go 化，不应拉起脚本；runN=%d last=%v", f.runN, f.lastArgs)
	}
}

// TestPythonCmd pythonCmd 的取值优先级。
//
// ⚠️ 行为变更（v0.26.9）：缺省**不再恒定返回 "python3"**，而是自动探测
// （`python3` → `python`，取首个能真正跑起来的）。旧断言把"Windows 上要用 python"
// 推给运维去设 `WB2A_PYTHON`，结果是没人设 → 脚本类任务静默禁用
// （本机的 `python3` 是 Microsoft Store 存根，跑不起来）。详见 scriptchain_test.go。
//
// 显式指定与 trim 的语义**不变**，原样保留。
func TestPythonCmd(t *testing.T) {
	t.Run("探测到可用解释器时用它", func(t *testing.T) {
		// python3 在 PATH 但跑不起来（Store 存根），python 可用。
		withFakePython(t, map[string]bool{"python3": true, "python": true},
			map[string]bool{"python": true})
		t.Setenv("WB2A_PYTHON", "")
		if got := pythonCmd(); got != "python" {
			t.Errorf("缺省应取探测结果 python，得 %q", got)
		}
	})

	t.Run("探测不到时回落 python3", func(t *testing.T) {
		withFakePython(t, map[string]bool{}, map[string]bool{})
		t.Setenv("WB2A_PYTHON", "")
		if got := pythonCmd(); got != "python3" {
			t.Errorf("探测不到应回落 python3（让失败暴露在原处），得 %q", got)
		}
		t.Setenv("WB2A_PYTHON", "   ")
		if got := pythonCmd(); got != "python3" {
			t.Errorf("空白 WB2A_PYTHON 视同未设置，得 %q", got)
		}
	})

	t.Setenv("WB2A_PYTHON", "python")
	if got := pythonCmd(); got != "python" {
		t.Errorf("override pythonCmd()=%q want python", got)
	}

	t.Setenv("WB2A_PYTHON", "  /usr/bin/python3.10  ")
	if got := pythonCmd(); got != "/usr/bin/python3.10" {
		t.Errorf("trim pythonCmd()=%q want /usr/bin/python3.10", got)
	}
}

// ======================================================================
// v0.26.9 · 脚本输出的可见性
//
// 此前脚本的子进程输出直接继承父进程，脚本自己的**汇总行**
// （`school2026 done: accounts=27 ok=.. fail=..`）只是"碰巧飘进日志"——
// 程序自己拿不到，于是台账里只有 "ok" 两个字：看不出处理了几个账号、
// 活动是不是进行期。而这两个结论恰恰是核查定时任务时最想知道的。
// ======================================================================

// TestRunScriptLogsSummaryLineOnSuccess 成功时日志必须带上脚本的汇总行。
func TestRunScriptLogsSummaryLineOnSuccess(t *testing.T) {
	f := installFakeExec(t)
	f.out = "逐账号过程…\n[school2026] xxx 活动非进行期\n" +
		"school2026 done: accounts=27 ok=0 already=0 skipped=27 pending=0 fail=0\n"

	var buf bytes.Buffer
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(os.Stderr); log.SetFlags(log.LstdFlags) })

	s := New(Config{})
	s.runScriptTask(taskGrowth, triggerManual, "growth", [][]string{
		{"python", "scripts/growth_run.py", "--phase", "zero"},
	}, []byte("[]"))

	if !strings.Contains(buf.String(), "school2026 done") {
		t.Errorf("日志应包含脚本汇总行，得：\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "skipped=27") {
		t.Errorf("汇总行的关键计数应可见（否则等于没记），得：\n%s", buf.String())
	}
}

// TestRunScriptLogsScriptOutputOnFailure 失败时除了 exec 的错误，
// 还要带上脚本自己的末行 —— "无可用账号"比 "exit status 1" 有用得多。
func TestRunScriptLogsScriptOutputOnFailure(t *testing.T) {
	f := installFakeExec(t)
	f.err = errors.New("exit status 1")
	f.out = "mode=RUN\nERR: 无可用账号（检查 auths/ 或 --token）\n"

	var buf bytes.Buffer
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(os.Stderr); log.SetFlags(log.LstdFlags) })

	s := New(Config{})
	s.runScriptTask(taskGrowth, triggerManual, "growth", [][]string{
		{"python", "scripts/growth_run.py", "--phase", "zero"},
	}, []byte("[]"))

	out := buf.String()
	if !strings.Contains(out, "无可用账号") {
		t.Errorf("失败日志应带回脚本末行（它是真正的失败原因），得：\n%s", out)
	}
	if !strings.Contains(out, "exit status 1") {
		t.Errorf("仍应保留 exec 的错误，得：\n%s", out)
	}
}

// TestScriptSummaryLinePrefersDoneLine 结论行优先取含 `done:` 的那行。
//
// 真实场景（本机实测）：脚本在 done 之后还会打一段"说明"，末行因此变成说明文字
// —— 那样日志里就看不到 `accounts=33 fail=0` 这个核查时最想要的结论。
func TestScriptSummaryLinePrefersDoneLine(t *testing.T) {
	out := "逐账号过程…\n" +
		"school2026 done: accounts=33 ok=0 already=0 skipped=0 fail=0\n" +
		"\n" +
		"[school2026] 说明：非人工任务自动点亮并领奖…\n"
	got := scriptSummaryLine(out)
	if !strings.Contains(got, "accounts=33") {
		t.Errorf("应取含 done: 的结论行，得 %q", got)
	}
	// 没有 done: 行时回落末行。
	got2 := scriptSummaryLine("a\nERR: 无可用账号\n")
	if !strings.Contains(got2, "无可用账号") {
		t.Errorf("无 done: 行时应回落末行，得 %q", got2)
	}
}

// TestLastNonEmptyLine 取末行要跳过空行与 CR（脚本在 Windows 上输出 \r\n）。
func TestLastNonEmptyLine(t *testing.T) {
	cases := []struct{ in, want string }{
		{"a\nb\n", "b"},
		{"a\nb\n\n\n", "b"},
		{"a\r\nb\r\n", "b"},
		{"  \n only \n  \n", "only"},
		{"", ""},
		{"\n\n", ""},
		{"单行", "单行"},
	}
	for _, c := range cases {
		if got := lastNonEmptyLine(c.in); got != c.want {
			t.Errorf("lastNonEmptyLine(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}
