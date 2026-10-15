// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// school.go 脚本类任务的执行基础设施。
//
// ⚠️ 命名沿革（v0.27.0）：本文件最初同时承载「开学季」与「夜猫子」两个脚本类任务。
// **开学季排程已整体移除**（活动已结束，且它依赖外部 python 脚本、长期是故障源）——
// 相关任务类、配置项、手动入口、凭证提供方全部删除；`scripts/school_open_day_2026.py`
// 文件本身保留在仓库里，供人工手动执行，但程序不再排程、不再调用它。
//
// 现在本文件只提供脚本类任务共用的执行面：
//   - 解释器探测（pythonCmd / probePython）与脚本根定位（repoRoot）；
//   - 子进程执行封装（scriptRunner / scriptCmd / runScript / runScriptTask）；
//   - 输出捕获与结论行提取。
//
// 唯一的在册使用者是**成长任务**（growth.go）。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"bytes"
	"io"
	"sync"
	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
)

// scriptRootMarkers 判定"这里就是脚本根"的标志（**任一存在**即可）。
//
// 为什么是一组而不是一个：此前只认 `school_open_day_2026.py` 一个文件 ——
// 于是**那个脚本一旦改名/缺失，成长任务会连带失效**（两者共用 repoRoot）。
// 两个功能共用的定位函数，不该把其中一个的文件当成硬依赖。
//
// 开学季排程虽已移除（v0.27.0），这条标志**仍保留**：老部署里 scripts/ 可能
// 恰好只剩这一个文件，去掉它会让原本能用的成长任务突然找不到脚本根。
var scriptRootMarkers = []string{
	filepath.Join("scripts", "school_open_day_2026.py"),
	filepath.Join("scripts", "growth_run.py"),
	filepath.Join("scripts", "task_common.py"),
}

// repoRoot 定位"脚本根"（含 scripts/ 的那一层）。
//
// 两个起点分别上溯，取先命中者：
//  1. **exe 所在目录** —— 发布包形态（scripts/ 放在 exe 旁边）时它才权威；
//  2. 当前工作目录 —— 开发/容器形态（`cd dist && ./webapp.exe` 时需要上溯到仓库根）。
//
// 为什么必须加 exe 这条链（v0.26.9）：此前只看工作目录，本机之所以"还能用"，
// 纯粹是因为 `dist/` 恰好在仓库里、上溯一级就撞见 scripts/。
// **一旦把 dist/ 单独拷出去部署，脚本任务会全部失效且原因不明显** ——
// 而"独立部署"正是发布包的目标形态。
//
// 两条链都找不到时回落工作目录（调用方会因脚本缺失打 WARN，不 panic）。
func repoRoot() string {
	if exe, err := os.Executable(); err == nil {
		if root := ascendToScriptRoot(filepath.Dir(exe)); root != "" {
			return root
		}
	}
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	if root := ascendToScriptRoot(wd); root != "" {
		return root
	}
	return wd
}

// ScriptRoot 导出脚本根路径，仅供启动日志与诊断展示（业务代码一律用 repoRoot）。
//
// 为什么值得导出一个只为打印的函数：脚本类任务的失败**几乎全部**是"找不到脚本"
// 或"解释器不对"，而这两件事在日志里此前都不可见 —— 运维只能猜。
// 把实际生效的路径打出来，问题自解释。
func ScriptRoot() string { return repoRoot() }

// ascendToScriptRoot 从 start 逐级向上找含标志脚本的目录；找不到返回 ""。
func ascendToScriptRoot(start string) string {
	dir := start
	for {
		for _, m := range scriptRootMarkers {
			if _, err := os.Stat(filepath.Join(dir, m)); err == nil {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// scriptRunner 脚本子进程的最小执行面：可被测试替换，避免测试真正拉起 python3。
type scriptRunner interface {
	SetDir(string)
	Run() error
	// SetStdin 提供子进程的标准输入。
	//
	// 存在的唯一理由：**Web 版的凭证只能走 stdin**。账号存库且加密（token 是
	// `enc1:` 前缀的密文），既没有 auths/ 目录可读，明文落盘又违背既有取舍
	// （见 growth_run.py 顶部注释）。晚于 SetDir 加它是被现实逼出来的：
	// 开学季脚本此前只认 auths/，于是 Web 版触发的任务**必然**以"无可用账号"退出。
	SetStdin(r io.Reader)
	// SetOutput 指定子进程的 stdout/stderr 去向。
	//
	// 为什么需要（v0.26.9）：此前子进程默认继承父进程输出，脚本的**汇总行**
	// （`school2026 done: accounts=27 ok=.. fail=..`）就这么飘进日志、Grep 得到
	// 但程序自己拿不到 —— 于是台账里只有"ok"两个字，看不出到底处理了几个账号、
	// 活动是不是进行期。核查定时任务时这正是最想知道的那一行。
	SetOutput(w io.Writer)
}

// scriptTimeout 单个脚本子进程的执行上限（v0.24.1，审查报告 P3-10）。
//
// 为什么必须有：此前用裸 `exec.Command`，**没有任何超时** —— 若 python 脚本卡住
// （等输入、网络死等、上游不响应），子进程永不退出，于是：
//
//	· scheduler.runScriptTask 的 endRun 永不执行，"夜猫子 / 校服"在台账里永远
//	  显示"执行中"，后续手动触发被互斥挡住；
//	· 子进程连同其 stdout/stderr 管道一直占着资源。
//
// HTTP 那条链有 120s 客户端超时兜底，唯独脚本这条完全没有 —— 这才是 P3-10
// 里真正无界的那一处。
//
// 20 分钟：脚本内部本身是「打开活动 + 领奖」这类分钟级动作，20 分钟足够；
// 真有更长的批处理需求应改配置而不是去掉超时。
const scriptTimeout = 20 * time.Minute

// scriptCmd exec.Cmd 适配器：把 exec.Cmd 的 Dir 字段包装成 SetDir 方法，
// 满足 scriptRunner 接口（exec.Cmd 本身只有字段没有方法）。
type scriptCmd struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
}

func (c *scriptCmd) SetDir(dir string) { c.cmd.Dir = dir }

func (c *scriptCmd) SetStdin(r io.Reader) { c.cmd.Stdin = r }

func (c *scriptCmd) SetOutput(w io.Writer) {
	if w == nil {
		return
	}
	c.cmd.Stdout = w
	c.cmd.Stderr = w
}

// Run 执行子进程；退出时释放 ctx（避免 withTimeout 的 cancel 泄漏，
// 这正是 go vet 的 lostcancel 检查所要求的）。
func (c *scriptCmd) Run() error {
	defer c.cancel()
	return c.cmd.Run()
}

// newScriptCmd 构建脚本子进程。包级变量便于测试注入 fake（installFakeExec 覆盖）。
// 工作目录由调用方 SetDir 显式设置仓库根。
//
// 用 CommandContext 而非 Command：超时后 Go 会 kill 子进程（连同其进程组），
// Run 返回 `signal: killed`，上层按普通失败记台账 —— 有界且可见。
var newScriptCmd = func(program string, args ...string) scriptRunner {
	ctx, cancel := context.WithTimeout(context.Background(), scriptTimeout)
	return &scriptCmd{cmd: exec.CommandContext(ctx, program, args...), cancel: cancel}
}

// pythonCandidates 候选解释器（按优先级）。**首个能真正跑起来的胜出**。
//
// 顺序的理由：Linux / 容器里叫 `python3`；Windows 官方安装器只提供 `python.exe`，
// 而 PATH 上往往**还存在** Microsoft Store 的 `python3.exe` App Execution Alias 存根
// ——`exec.LookPath` 找得到、`exec.Command` 也起得来，但一执行就退 9009。
//
// 所以**只看 LookPath 是不够的**：必须实跑一次 `--version`（见 probePython）。
// 这正是本仓此前"脚本类任务在本机统一不可用"的直接原因：默认值写死 `python3`，
// 而本机的 `python3` 恰好是那个跑不起来的存根。
var pythonCandidates = []string{"python3", "python"}

// lookPath 抽成变量以便测试注入 —— 否则每个用例都得真在 PATH 上造一个
// 叫做 python3 的可执行文件（做不到，也不该做）。与 newScriptCmd 同一套路。
var lookPath = exec.LookPath

var (
	pythonMu sync.Mutex
	// pythonProbed 用"标志 + 惰性检查"而不是 sync.Once：Once 执行过就再也
	// 不执行，测试想重新探测（或将来支持运行期改配置）会永远拿回旧值。
	// 这是本仓踩过的坑（见 initCaches 的注释），此处沿用同一纪律。
	pythonProbed   bool
	pythonResolved string
	pythonProbeLog string
)

// pythonCmd 返回执行 scripts/*.py 的解释器名。
//
// 优先级：
//  1. `WB2A_PYTHON` —— 显式指定则**原样使用、不做探测**（用户可能给绝对路径）；
//  2. 自动探测 `python3` → `python`，取首个能真正跑起来的（结果进程内缓存）。
//
// 为什么要有自动探测（v0.26.9）：此前默认值写死 `python3`，把"Windows 上要用 python"
// 这件事**推给了运维去设环境变量**。结果是：没人设 → 成长任务与开学季双双静默禁用
// （启动日志只有一句"未探测到可用解释器/脚本"，没说试过什么、为什么失败）。
// 一个能自己判断的事情不该要求人工配置 —— 而且配错了还很难看出来。
func pythonCmd() string {
	if v := strings.TrimSpace(os.Getenv("WB2A_PYTHON")); v != "" {
		return v
	}
	pythonMu.Lock()
	defer pythonMu.Unlock()
	if !pythonProbed {
		pythonProbed = true
		pythonResolved, pythonProbeLog = probePython()
	}
	if pythonResolved == "" {
		// 一个都没探到：回落默认值，让失败**暴露在原来的位置**（执行时报 9009），
		// 而不是悄悄换一个也不存在的名字。诊断信息见 PythonDiagnosis()。
		return "python3"
	}
	return pythonResolved
}

// probePython 依次实跑候选解释器，返回 (可用者, 人话探测过程)。
//
// 判据是**真的执行一次 `--version` 且退出码为 0** —— 单看 LookPath 会把
// Microsoft Store 的存根当成可用（那是本机的实际情形，也正是本函数存在的理由）。
func probePython() (string, string) {
	var notes []string
	for _, cand := range pythonCandidates {
		if _, err := lookPath(cand); err != nil {
			notes = append(notes, cand+"（不在 PATH）")
			continue
		}
		c := newScriptCmd(cand, "--version")
		if err := c.Run(); err != nil {
			notes = append(notes, cand+"（在 PATH 但无法执行："+oneLineErr(err)+"）")
			continue
		}
		note := cand + "（可用）"
		if len(notes) > 0 {
			note += "；此前已排除 " + strings.Join(notes, "、")
		}
		return cand, note
	}
	if len(notes) == 0 {
		notes = append(notes, "候选 "+strings.Join(pythonCandidates, " / ")+" 均不在 PATH")
	}
	return "", "试过 " + strings.Join(notes, "、")
}

// oneLineErr 把错误压成一行（exec 的错误偶尔带换行，日志里会串行）。
func oneLineErr(err error) string {
	if err == nil {
		return ""
	}
	return strings.Join(strings.Fields(err.Error()), " ")
}

// PythonDiagnosis 返回解释器探测过程的人话描述，供启动日志展示。
//
// 为什么要暴露过程而不是一个布尔：本机此前的现象是"日志只说没探测到，不说是谁没探到"
// —— 运维既不知道试了什么，也不知道该改哪里。把"试过 python3（存根跑不起来）、
// 用 python"写出来，问题自解释。
func PythonDiagnosis() string {
	if v := strings.TrimSpace(os.Getenv("WB2A_PYTHON")); v != "" {
		return "WB2A_PYTHON 显式指定为 " + v
	}
	pythonMu.Lock()
	defer pythonMu.Unlock()
	if !pythonProbed {
		pythonProbed = true
		pythonResolved, pythonProbeLog = probePython()
	}
	return pythonProbeLog
}

// scriptSummaryLine 提取脚本输出里的**结论行**供日志展示。
//
// 优先取含 `done:` 的行（脚本约定的汇总行，形如
// `school2026 done: accounts=33 ok=0 already=0 skipped=0 fail=0`），
// 找不到才回落最后一行非空内容。
//
// 为什么不直接取末行：脚本在 done 之后还会打一段说明（"非人工任务自动点亮并领奖…"），
// 末行就变成说明文字了 —— 而核查定时任务时想看的恰恰是那几个计数。
func scriptSummaryLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(strings.TrimRight(lines[i], "\r")); t != "" && strings.Contains(t, "done:") {
			return t
		}
	}
	return lastNonEmptyLine(s)
}

// lastNonEmptyLine 返回文本里最后一行非空内容。
//
// 脚本的**汇总行**固定在最末（`school2026 done: accounts=.. ok=.. fail=..`），
// 而它前面是几十行逐账号过程。取末行 = 取结论，且不受过程行数影响。
func lastNonEmptyLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(strings.TrimRight(lines[i], "\r")); t != "" {
			return t
		}
	}
	return ""
}

// runScript 依次执行若干脚本命令：任一命令失败只记一行 WARN，不向上抛、
// 不影响调度主循环继续跑下一个时点。单命令失败不中断后续命令。
//
// 返回成功/失败命令数与最后一条失败摘要，供台账计数（note 取最后一条：多命令
// 全挂时最有价值的是「最近一次为什么挂」，逐条细节已在日志里）。
func runScript(name, root string, commands [][]string, stdin []byte) (ok, fail int, note string) {
	for _, cmdArgs := range commands {
		c := newScriptCmd(cmdArgs[0], cmdArgs[1:]...)
		c.SetDir(root)
		if len(stdin) > 0 {
			// 每条命令都重新给一份 Reader：同一个 Reader 被第一次读完后再给
			// 第二条命令，第二条会读到 EOF（静默地"没有凭证"）。
			c.SetStdin(bytes.NewReader(stdin))
		}
		// 输出**双写**：一份照旧进日志（运维直接看得到全过程），一份留在 buffer 里
		// 供程序提取汇总行。不能只捕获 —— 那会让日志里再也看不到脚本过程；
		// 也不能只继承 —— 那程序就永远拿不到汇总。
		// 用普通 bytes.Buffer 而非带锁的：`Run()` 返回时 exec 已经等完了输出
		// 拷贝 goroutine，此处的读与写入不存在并发（写发生在 Run 期间）。
		var buf bytes.Buffer
		c.SetOutput(io.MultiWriter(os.Stdout, &buf))
		if err := c.Run(); err != nil {
			fail++
			note = fmt.Sprintf("%s: %v", cmdArgs[1], err)
			log.Printf("WARN: %s (%s): %v", name, cmdArgs[1], err)
			if tail := lastNonEmptyLine(buf.String()); tail != "" {
				// 失败时把脚本自己的**最后一行**也带上：脚本的汇总行通常直接说明了
				// 原因（"无可用账号" / "活动非进行期"），比 "exit status 1" 有用得多。
				log.Printf("WARN: %s (%s) 脚本输出末行：%s", name, cmdArgs[1], tail)
			}
			continue
		}
		ok++
		// 成功时也带上脚本的汇总行（v0.26.9）：台账里只写"ok"看不出它做了多少事
		// —— 核查定时任务时，"处理了 27 个账号 / 活动非进行期全跳过"这两个结论
		// 差别巨大，而它们都藏在这一行里。
		if tail := scriptSummaryLine(buf.String()); tail != "" {
			log.Printf("%s: ok (%s) — %s", name, cmdArgs[1], tail)
		} else {
			log.Printf("%s: ok (%s)", name, cmdArgs[1])
		}
	}
	return ok, fail, note
}

// runScriptTask 跑一组脚本命令并记台账：脚本类任务（school/cat）只有「命令级」
// 粒度——一条命令要么成功要么失败，没有账号维度的分解。
//
// 退出码非 0 即算 Fail（脚本自己会把「活动下线」「非窗口期」等正常态处理成 0 退出，
// 见 RunSchoolNow/RunCatNow 的注释），所以这里的 Fail 就是真的失败了。
func (s *Scheduler) runScriptTask(kind taskKind, trigger, name string, commands [][]string, stdin []byte) {
	started := time.Now()
	// 运行闸门：脚本类任务的代价更高——两个 python 子进程并发跑同一批账号，
	// 既是双倍上游请求，也可能并发写回同一份凭证（后写覆盖先写）。
	if s.skipIfBusy(kind, trigger) {
		s.recordRun(kind, trigger, started, runTally{skipped: 1, note: "已有同一类任务在执行中，本轮让路"})
		return
	}
	defer s.endRun(kind)

	okN, failN, note := runScript(name, repoRoot(), commands, stdin)
	s.recordRun(kind, trigger, started, runTally{
		total: okN + failN, ok: okN, fail: failN, note: note,
	})
}

// RunCatNow 立即执行夜猫子任务（人工入口）。
func (s *Scheduler) RunCatNow() { s.runCat(triggerManual) }

// catTaskCode growth 域里夜猫子的任务码（上游固定）。
const catTaskCode = "black_cat"

// errCatPrereq 夜猫子的前置条件未满足（实测上游回 prerequisite not met: first_buddy，
// 即该账号还没领养 Buddy）。这是**正常态**而非故障：领养链路由「活跃上报」任务负责
// （runActivity → travelAdoptForce），领到之后夜猫子自然可点亮。归为 skipped 而非 fail，
// 否则每个夜猫窗口都会在台账里刷一条假失败，把真故障掩盖掉。
var errCatPrereq = errors.New("cat prerequisite not met")

// withinNightWindow 夜猫窗口 23:00–08:00（CST，UTC+8）。
//
// 上游按 CST 归集夜猫进度，故这里固定 +8 偏移判定，不跟随宿主机时区——
// 否则部署在 UTC 容器里会算错窗口，整晚空跑。窗口外调用是正常态（打印 skip），
// 不是失败。
func withinNightWindow(now time.Time) bool {
	cst := now.In(time.FixedZone("CST", 8*3600))
	h := cst.Hour()
	return h >= 23 || h < 8
}

// runCat 夜猫子任务（Go 原生实现，已脱离 python 脚本）。
//
// 链路（照抄 scripts/task_runner.py 的 cat 分支，cap=1）：
//
//	查 black_cat 状态 → 未 accept 先 accept(校验 status) → 夜间对话上报 1 条
//	→ 回读进度 → 达标则 claim。
//
// 与 school 的差别：black_cat 的判据上报走 billing 域 /v2/report（与活跃上报同通道，
// 仅 mode=night + glm-5.2），不涉及小程序指纹与 school 域端点，故可完整 Go 化。
func (s *Scheduler) runCat(trigger string) {
	started := time.Now()
	var t runTally
	defer func() { s.recordRun(taskCat, trigger, started, t) }()

	if s.skipIfBusy(taskCat, trigger) {
		t.skipped++
		return
	}
	defer s.endRun(taskCat)

	if !withinNightWindow(s.now()) {
		// 窗口外：整类任务不动作，记 skipped（不是失败）。
		t.skipped++
		log.Printf("cat: 非夜猫窗口(23-08 CST)，跳过本轮")
		return
	}

	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			t.skipped++
			continue
		}
		if !s.taskAllowed(st.UID, taskCat.String()) {
			t.skipped++
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessTokenValue() == "" {
			t.skipped++
			continue
		}
		t.total++
		if err := s.catOne(a); err != nil {
			if errors.Is(err, errCatPrereq) {
				t.total--
				t.skipped++
				continue
			}
			log.Printf("cat %s: %v", logfmt.Label(a.UID, a.Nickname), err)
			t.fail++
			continue
		}
		t.ok++
	}
}

// catOne 对单账号执行一次夜猫子点亮（窗口内最多补 1 次，cap=1）。
// 返回 nil 表示「本轮处理完成」（含已领/无此任务等正常态），非 nil 表示真失败。
func (s *Scheduler) catOne(a *auth.Auth) error {
	task, err := s.cfg.Upstream.GrowthTaskStatus(a, catTaskCode)
	if err != nil {
		return fmt.Errorf("query black_cat: %w", err)
	}
	if task == nil {
		return nil // 活动期外该任务不存在——正常态
	}
	if task.AcceptStatus == "claimed" {
		return nil // 本轮已入账
	}
	// 已满进度：先领奖（不再重复上报）。
	if task.Progress.Target > 0 && task.Progress.Current >= task.Progress.Target {
		return s.cfg.Upstream.ClaimGrowthTask(a, catTaskCode)
	}

	// 未登记则先 accept，并校验回执 status（HTTP 200 不代表登记生效）。
	if !task.Accepted() {
		res, err := s.cfg.Upstream.AcceptGrowthTask(a, catTaskCode)
		if err != nil {
			return fmt.Errorf("accept black_cat: %w", err)
		}
		if res.Status != "accepted" {
			if strings.Contains(res.Message, "prerequisite") {
				log.Printf("cat %s: 前置未满足（%s），跳过本轮", logfmt.Label(a.UID, a.Nickname), res.Message)
				return errCatPrereq
			}
			return fmt.Errorf("accept 未登记生效 (status=%q msg=%q)", res.Status, res.Message)
		}
	}

	// cap=1：窗口内只补 1 条（3 条目标分 3 天完成，避免一次性刷满触发风控）。
	cid := fmt.Sprintf("wb-cat-%d", time.Now().UnixMilli())
	if err := s.cfg.Upstream.ReportNightChat(a, cid, cid+"-r1"); err != nil {
		return fmt.Errorf("report black_cat: %w", err)
	}
	log.Printf("cat %s: report ok (night, glm-5.2)", logfmt.Label(a.UID, a.Nickname))

	// 服务端事件归账有延迟，等一拍再回读（与脚本一致的 2s）。
	time.Sleep(2 * time.Second)
	t2, err := s.cfg.Upstream.GrowthTaskStatus(a, catTaskCode)
	if err != nil {
		return nil // 回读失败不判失败：上报已成功，次日窗口再判
	}
	if t2 == nil || t2.AcceptStatus == "claimed" {
		return nil
	}
	if t2.Progress.Target > 0 && t2.Progress.Current >= t2.Progress.Target {
		if err := s.cfg.Upstream.ClaimGrowthTask(a, catTaskCode); err != nil {
			return fmt.Errorf("claim black_cat: %w", err)
		}
		log.Printf("cat %s: claim black_cat ok", logfmt.Label(a.UID, a.Nickname))
		return nil
	}
	log.Printf("cat %s: 本轮未达 target（%d/%d），待下个窗口",
		logfmt.Label(a.UID, a.Nickname), t2.Progress.Current, t2.Progress.Target)
	return nil
}
