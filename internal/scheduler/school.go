// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// school.go 开学季任务与夜猫子任务的脚本类排程：从系统 crontab 迁入 Go scheduler。
//
// 背景：school（12:00）与 cat（01:00 夜猫窗口）原由系统 crontab 调
// scripts/school_open_day_cron.sh 执行——依赖外部系统 cron、容器重建可能丢失、
// 不在 config 里配置。迁入后成为第五、第六类任务，时点由 schedule.school_hours /
// schedule.cat_hours 配置，school_open_day_cron.sh 保留为手动触发入口。
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

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
)

// repoRoot 定位仓库根（容器内 /app、宿主 /root/workbuddy2api）。
// 策略：从当前工作目录逐级向上找 scripts/school_open_day_2026.py，
// 找不到回落 os.Getwd()（此时 Run 会因脚本缺失打 WARN，不 panic）。
// 注意：Go scheduler 在 cmd/server 内以工作目录启动（容器 WORKDIR /app），
// 若进程以别的工作目录拉起（如 systemd/裸 binary），上溯穷尽后仍以
// os.Getwd() 兜底，把缺失暴露成 WARN 而非静默。
func repoRoot() string {
	start, err := os.Getwd()
	if err != nil {
		return "."
	}
	dir := start
	for {
		if _, err := os.Stat(filepath.Join(dir, "scripts", "school_open_day_2026.py")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return start
		}
		dir = parent
	}
}

// scriptRunner 脚本子进程的最小执行面：可被测试替换，避免测试真正拉起 python3。
type scriptRunner interface {
	SetDir(string)
	Run() error
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

// pythonCmd 返回执行 scripts/*.py 的解释器名。
//
// 默认 "python3"，与容器/Linux 现状完全一致，行为零变更；WB2A_PYTHON
// 显式指定时优先，供解释器不叫 python3 的环境使用（命名对齐仓库 Go 侧
// WB2A_* env 约定，如 WB2A_AUTH_DIR / WB2A_LISTEN）。
//
// 需要该开关的原因：Windows 官方安装器只提供 python.exe，且 PATH 上常存在
// Microsoft Store 的 python3.exe App Execution Alias 存根——exec.Command 能找到
// 它却无法真正执行，脚本类任务统一报 `exit status 9009`。
// 设 WB2A_PYTHON=python 即可绕过。
func pythonCmd() string {
	if v := strings.TrimSpace(os.Getenv("WB2A_PYTHON")); v != "" {
		return v
	}
	return "python3"
}

// runScript 依次执行若干脚本命令：任一命令失败只记一行 WARN，不向上抛、
// 不影响调度主循环继续跑下一个时点。单命令失败不中断后续命令。
//
// 返回成功/失败命令数与最后一条失败摘要，供台账计数（note 取最后一条：多命令
// 全挂时最有价值的是「最近一次为什么挂」，逐条细节已在日志里）。
func runScript(name, root string, commands [][]string) (ok, fail int, note string) {
	for _, cmdArgs := range commands {
		c := newScriptCmd(cmdArgs[0], cmdArgs[1:]...)
		c.SetDir(root)
		if err := c.Run(); err != nil {
			fail++
			note = fmt.Sprintf("%s: %v", cmdArgs[1], err)
			log.Printf("WARN: %s (%s): %v", name, cmdArgs[1], err)
			continue
		}
		ok++
		log.Printf("%s: ok (%s)", name, cmdArgs[1])
	}
	return ok, fail, note
}

// runScriptTask 跑一组脚本命令并记台账：脚本类任务（school/cat）只有「命令级」
// 粒度——一条命令要么成功要么失败，没有账号维度的分解。
//
// 退出码非 0 即算 Fail（脚本自己会把「活动下线」「非窗口期」等正常态处理成 0 退出，
// 见 RunSchoolNow/RunCatNow 的注释），所以这里的 Fail 就是真的失败了。
func (s *Scheduler) runScriptTask(kind taskKind, trigger, name string, commands [][]string) {
	started := time.Now()
	// 运行闸门：脚本类任务的代价更高——两个 python 子进程并发跑同一批账号，
	// 既是双倍上游请求，也可能并发写回同一份凭证（后写覆盖先写）。
	if s.skipIfBusy(kind, trigger) {
		s.recordRun(kind, trigger, started, runTally{skipped: 1, note: "已有同一类任务在执行中，本轮让路"})
		return
	}
	defer s.endRun(kind)

	okN, failN, note := runScript(name, repoRoot(), commands)
	s.recordRun(kind, trigger, started, runTally{
		total: okN + failN, ok: okN, fail: failN, note: note,
	})
}

// RunSchoolNow 立即执行开学季任务（人工入口）。
func (s *Scheduler) RunSchoolNow() { s.runSchool(triggerManual) }

// SchoolAvailable 报告开学季脚本链路当前是否可用。
//
// 两个条件缺一不可：
//  1. scripts/school_open_day_2026.py 存在（发行目录不一定带 scripts/）；
//  2. python 解释器能真正执行（Windows 上 PATH 常存在 Microsoft Store 的
//     python3.exe 存根——LookPath 能找到却跑不了，故必须实跑一次 --version）。
//
// 装配方据此决定 SchoolDisabled，把「装了但缺依赖」从「每轮刷失败日志」变成
// 「启动时一句明确说明」，故障不再被掩护。
func SchoolAvailable() bool {
	root := repoRoot()
	if _, err := os.Stat(filepath.Join(root, "scripts", "school_open_day_2026.py")); err != nil {
		return false
	}
	c := newScriptCmd(pythonCmd(), "--version")
	c.SetDir(root)
	return c.Run() == nil
}

// runSchool 执行开学季任务：school_open_day_2026.py ALL --run --yes。
// 全量跑任务点亮 + 领奖 + 自动抽空抽奖余额。活动下线（in_period=false）时脚本
// 各段全量跳过、正常退出，不视为失败。失败只记 WARN。
func (s *Scheduler) runSchool(trigger string) {
	s.runScriptTask(taskSchool, trigger, "school", [][]string{
		{pythonCmd(), "scripts/school_open_day_2026.py", "ALL", "--run", "--yes"},
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
