package scheduler

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ======================================================================
// v0.26.9 · 脚本链路：解释器探测 + 脚本根定位
//
// 这一批来自本机的真实故障：**成长任务与开学季双双静默禁用**，启动日志只有
// 一句"未探测到可用解释器/脚本"。实际原因很具体 ——
//   · `pythonCmd()` 默认写死 `python3`；
//   · 而 Windows 的 PATH 上 `python3` 往往只是 Microsoft Store 的
//     App Execution Alias **存根**：LookPath 找得到、exec 也起得来，一跑就退 9009；
//   · 真解释器叫 `python`（本机实测：python3 不可用、python 为 3.12.10）。
//
// 所以下面把两件事钉住：**探测必须实跑而不是只看 LookPath**；
// **脚本根不能只认一个标志文件**（否则改个名就带走另一个功能）。
// ======================================================================

// withFakePython 注入可控的 LookPath 与子进程行为，并重置探测缓存。
// probe 决定某个候选"能否跑起来"。
func withFakePython(t *testing.T, inPath map[string]bool, canRun map[string]bool) {
	t.Helper()
	oldLook, oldCmd, oldCands := lookPath, newScriptCmd, pythonCandidates
	t.Cleanup(func() {
		lookPath, newScriptCmd, pythonCandidates = oldLook, oldCmd, oldCands
		resetPythonProbe()
	})
	resetPythonProbe()

	lookPath = func(file string) (string, error) {
		if inPath[file] {
			return "/fake/" + file, nil
		}
		return "", errors.New("executable file not found in $PATH")
	}
	newScriptCmd = func(program string, _ ...string) scriptRunner {
		return &fakeRunner{err: func() error {
			if canRun[program] {
				return nil
			}
			return errors.New("exit status 9009")
		}()}
	}
	pythonCandidates = []string{"python3", "python"}
}

// resetPythonProbe 清掉探测缓存（用 mutex + 标志而不是 Once 的原因就在这里）。
func resetPythonProbe() {
	pythonMu.Lock()
	defer pythonMu.Unlock()
	pythonProbed = false
	pythonResolved = ""
	pythonProbeLog = ""
}

type fakeRunner struct {
	dir   string
	err   error
	stdin string
}

func (f *fakeRunner) SetDir(d string) { f.dir = d }
func (f *fakeRunner) SetStdin(r io.Reader) {
	if r == nil {
		return
	}
	b, _ := io.ReadAll(r)
	f.stdin = string(b)
}
func (f *fakeRunner) SetOutput(w io.Writer) {}
func (f *fakeRunner) Run() error            { return f.err }

// TestProbePythonPrefersRunnableCandidate 「存在但跑不起来」的候选必须被跳过。
//
// 这是本故障的核心判据：只看 LookPath 会选中 Store 存根（它确实"在 PATH 上"），
// 于是任务被判定为不可用 —— 而真正能用的 `python` 就在旁边。
func TestProbePythonPrefersRunnableCandidate(t *testing.T) {
	// python3 在 PATH 但跑不起来（Store 存根）；python 可用。
	withFakePython(t, map[string]bool{"python3": true, "python": true},
		map[string]bool{"python": true})

	if got := pythonCmd(); got != "python" {
		t.Fatalf("应跳过跑不起来的 python3、选中 python，得 %q", got)
	}
	diag := PythonDiagnosis()
	if !strings.Contains(diag, "python") {
		t.Errorf("诊断应提到选中的解释器，得 %q", diag)
	}
	if !strings.Contains(diag, "python3") {
		t.Errorf("诊断应说明 python3 被排除过（否则读者不知道试了什么），得 %q", diag)
	}
}

// TestProbePythonFirstRunnableWins 第一个候选能跑就用第一个（Linux/容器形态，
// 不该因为探测而改变既有行为）。
func TestProbePythonFirstRunnableWins(t *testing.T) {
	withFakePython(t, map[string]bool{"python3": true, "python": true},
		map[string]bool{"python3": true, "python": true})
	if got := pythonCmd(); got != "python3" {
		t.Fatalf("python3 可用时应选它（保持 Linux/容器零变更），得 %q", got)
	}
}

// TestProbePythonNoneAvailableFallsBackToDefault 全不可用时回落默认值，
// 让失败暴露在原来的位置（执行时报 9009），并给出"试过什么"的诊断。
func TestProbePythonNoneAvailableFallsBackToDefault(t *testing.T) {
	withFakePython(t, map[string]bool{}, map[string]bool{})
	if got := pythonCmd(); got != "python3" {
		t.Fatalf("全不可用时应回落默认值，得 %q", got)
	}
	diag := PythonDiagnosis()
	if !strings.Contains(diag, "不在 PATH") {
		t.Errorf("诊断应说明候选不在 PATH，得 %q", diag)
	}
}

// TestProbePythonNotInPathReported 候选不在 PATH 与"在 PATH 但跑不起来"
// 必须是**两种不同的说明** —— 它们的修法完全不同（装解释器 vs 改解释器名）。
func TestProbePythonNotInPathReported(t *testing.T) {
	withFakePython(t, map[string]bool{"python3": true}, map[string]bool{})
	diag := PythonDiagnosis()
	if !strings.Contains(diag, "无法执行") {
		t.Errorf("在 PATH 但跑不起来应说明为「无法执行」，得 %q", diag)
	}
	if !strings.Contains(diag, "python（不在 PATH）") {
		t.Errorf("python 确实不在 PATH，应如实说明，得 %q", diag)
	}
}

// TestPythonCmdExplicitEnvWinsAndSkipsProbe WB2A_PYTHON 显式指定时**原样使用**，
// 不做探测 —— 用户可能给绝对路径，探测它没有意义（也可能探测失败而误判为不可用）。
func TestPythonCmdExplicitEnvWinsAndSkipsProbe(t *testing.T) {
	withFakePython(t, map[string]bool{}, map[string]bool{}) // 全不可用
	t.Setenv("WB2A_PYTHON", `D:\Python312\python.exe`)

	if got := pythonCmd(); got != `D:\Python312\python.exe` {
		t.Fatalf("显式指定应原样返回，得 %q", got)
	}
	if diag := PythonDiagnosis(); !strings.Contains(diag, "WB2A_PYTHON") {
		t.Errorf("诊断应说明来源是显式指定，得 %q", diag)
	}
}

// TestProbePythonCached 探测结果缓存：同一进程内不重复实跑
// （每次调用都拉起子进程会让可用性检查变成开销）。
func TestProbePythonCached(t *testing.T) {
	calls := 0
	oldCmd, oldLook, oldCands := newScriptCmd, lookPath, pythonCandidates
	t.Cleanup(func() {
		newScriptCmd, lookPath, pythonCandidates = oldCmd, oldLook, oldCands
		resetPythonProbe()
	})
	resetPythonProbe()
	lookPath = func(file string) (string, error) { return "/fake/" + file, nil }
	newScriptCmd = func(string, ...string) scriptRunner {
		calls++
		return &fakeRunner{}
	}
	pythonCandidates = []string{"python3"}

	for i := 0; i < 5; i++ {
		_ = pythonCmd()
	}
	if calls != 1 {
		t.Errorf("探测应只实跑一次，实际 %d 次", calls)
	}
}

// ---------------------------------------------------------------- 脚本根定位

// TestAscendToScriptRootAcceptsAnyMarker 三个标志文件**任一**存在即认。
//
// 这条钉的是耦合缺陷：此前只认 school_open_day_2026.py，一旦它改名/缺失，
// **成长任务会连带失效**（两者共用定位函数）。
func TestAscendToScriptRootAcceptsAnyMarker(t *testing.T) {
	for _, marker := range scriptRootMarkers {
		dir := t.TempDir()
		full := filepath.Join(dir, marker)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("# stub"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := ascendToScriptRoot(dir); got != dir {
			t.Errorf("标志 %s 单独存在时应被认出，得 %q", marker, got)
		}
	}
}

// TestAscendToScriptRootWalksUp 从深层子目录要能上溯到脚本根。
func TestAscendToScriptRootWalksUp(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, scriptRootMarkers[1])
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("# stub"), 0o644); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := ascendToScriptRoot(deep); got != root {
		t.Errorf("应从 %s 上溯到 %s，得 %q", deep, root, got)
	}
}

// TestAscendToScriptRootNoMarkerReturnsEmpty 找不到时返回空串
// （让调用方有机会换一条起点链，而不是悄悄返回一个错的目录）。
func TestAscendToScriptRootNoMarkerReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	if got := ascendToScriptRoot(dir); got != "" {
		t.Errorf("无标志应返回空串，得 %q", got)
	}
}

// TestRepoRootFindsRepoInDevelopment 开发形态：从 internal/scheduler 上溯应命中仓库根
// （本仓确实有 scripts/，所以这条在当前工作目录下应当成立）。
func TestRepoRootFindsRepoInDevelopment(t *testing.T) {
	root := repoRoot()
	if _, err := os.Stat(filepath.Join(root, "scripts")); err != nil {
		t.Fatalf("repoRoot 返回 %q，其下没有 scripts/ —— 脚本类任务会全部失效", root)
	}
}
