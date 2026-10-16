// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// growth.go 成长任务（v0.17.0）：**纯手动选号执行**。
//
// 设计变更（v0.17.0）：v0.16.0 曾把成长任务做成第 7 类定时任务（导入后延迟到
// 低峰自动跑首轮 + 每日自动补领）。实际运行一段时间后收敛为纯手动，理由三条：
//  1. 成长任务对账号发起的是**真实写请求**（上报 / accept / claim），且每个号要按
//     四类客户端指纹分别构造事件链（CLI / 桌面 / web / mp），风险与成本都显著高于
//     签到这类轻量任务 —— 这种量级的动作不该由定时器自作主张。
//  2. 「哪些号该跑」本质是运营判断（刚导入的？存量的？某个号疑似漏任务？），
//     交给 growth_done_at 猜不如管理员在账号池里直接勾选来得准。
//  3. 自动重跑有隐性成本：claim 会对每个已领任务各发一条幂等请求，自动轮次意味着
//     持续的无意义请求量 —— 在防封上是明确的负面。
//
// 现在只保留执行引擎：管理员选号 → 逐个跑 zero → talk（号间随机间隔）。
//
// 为什么是「脚本 + Go 编排」而不是纯 Go 实现：成长任务共 18 个，每个都要构造
// 特定客户端指纹的事件链，且「上报 200 ≠ 计分」——必须回读进度确认达标再 claim。
// 这套逻辑已在 scripts/task_runner.py 沉淀并实测（M1-M15），Go 侧重写一遍等于把
// 已踩过的坑重踩一次。与 school/cat 的差别：school 一条命令跑全部账号，本任务
// 需要**逐账号传凭证**（凭证明文只在 webapp 内存里，不落盘），故走 stdin。
//
// 两阶段编排（先零消耗、后对话）：
//
//	zero  行为上报类（11 个），**不消耗账号 credits** → 先用它攒积分
//	talk  需真实对话类（5 个），消耗 credits/额度 → 用 zero 攒到的积分去跑
//
// 防封是这套编排的一等约束，不是附带考虑：
//  1. 单号内严格串行 —— 一个号的任务逐个跑，绝不并发（脚本内 --gap ≥1s）
//  2. 账号间随机间隔 30~120s —— 固定间隔本身就是可被统计出的节奏特征
//  3. 选号即批次 —— 由管理员决定这批跑几个；不设自动队列，也就没有「一次涌入」问题
//  4. 只跑未完成的 —— 「已完成默认跳过」由服务端幂等保证（claimed → 不发请求）
//  5. 失败不重试 —— 某号失败即跳过，不重试刷请求
package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// GrowthCred 一个待跑账号的凭证（由 webapp 侧从加密库解密后提供）。
type GrowthCred struct {
	UID         string
	Nickname    string
	Realm       string
	Domain      string
	AccessToken string
}

// GrowthProvider 账号凭证的提供方（webapp 实现；nil = 功能不可用）。
//
// 只暴露两个能力：按 uid 取明文凭证（供脚本调用）+ 记录执行时间。
// 「哪些号该跑」由管理员在 UI 上决定，调度器不持有待办队列。
type GrowthProvider interface {
	// GrowthCredsByUID 按 uid 取凭证；入参顺序即执行顺序，缺失/无 token 的跳过。
	GrowthCredsByUID(uids []string) ([]GrowthCred, error)
	// MarkGrowthDone 记录该号成长任务的最近一次执行时间。
	MarkGrowthDone(uid string) error
}

// 默认节流参数（Config 未显式配置时生效）。
const (
	defaultGrowthGapMinSec = 30
	defaultGrowthGapMaxSec = 120
	// growthPerAccountTimeout 单个账号单个阶段的超时。
	// zero 阶段 11 个任务各 ≥1s + 网络往返，正常在 30s 内；talk 含真实对话更久。
	// 给到 6 分钟是留足慢链路余量，同时保证一个卡死的号不会拖住整批。
	growthPerAccountTimeout = 6 * time.Minute
	// growthLogTailMax 回传日志的截断长度（失败时留证据，成功时不刷屏）。
	growthLogTailMax = 1200
)

// growthResult growth_run.py 的 GROWTH_RESULT 行解析结果。
type growthResult struct {
	Phase        string `json:"phase"`
	OK           int    `json:"ok"`
	Already      int    `json:"already"`
	Skip         int    `json:"skip"`
	Pending      int    `json:"pending"`
	Fail         int    `json:"fail"`
	Credit       int    `json:"credit"`
	Energy       int    `json:"energy"`
	Error        string `json:"error"`
	SkippedNote  string `json:"skipped_reason"`
	RawLog       string `json:"-"`
	ParsedOK     bool   `json:"-"`
	ProcessError string `json:"-"`
}

// growthScriptAvailableHook 可替换（测试注入）：默认检查脚本存在 + 解释器能真跑。
var growthScriptAvailableHook = func() bool {
	root := repoRoot()
	if _, err := os.Stat(filepath.Join(root, "scripts", "growth_run.py")); err != nil {
		return false
	}
	c := newScriptCmd(pythonCmd(), "--version")
	c.SetDir(root)
	return c.Run() == nil
}

// GrowthScriptAvailable 报告成长任务脚本链路是否可用。
// 与 SchoolAvailable 同两手准备：脚本存在 + 解释器能真跑（Windows 上 PATH 里
// 常有 Microsoft Store 的 python3.exe 存根，LookPath 找得到却跑不了）。
func GrowthScriptAvailable() bool { return growthScriptAvailableHook() }

// GrowthDiagnosis 逐项诊断成长任务脚本链路（脚本根 / 脚本存在性 / 解释器）。
//
// 为什么需要它（v0.31.22，真机踩过）：原错误只说「缺少 python 或 scripts/growth_run.py」，
// 既不说**缺哪一项**，也不说它**去哪里找的** —— 运维只能猜。实际情形是 VPS 部署时
// 漏拷了 `scripts/` 目录，而 python3 明明是好的；那句含糊的文案把排查方向引向了
// 「装 python」，白走一圈。
//
// 判据与 growthScriptAvailableHook 完全一致（同一处 root、同一处脚本路径、
// 同一个 pythonCmd），只是把每一项的中间结论说出来。
func GrowthDiagnosis() string {
	root := repoRoot()
	script := filepath.Join(root, "scripts", "growth_run.py")
	scriptTxt := "存在"
	if _, err := os.Stat(script); err != nil {
		scriptTxt = "缺失"
	}
	py := pythonCmd()
	c := newScriptCmd(py, "--version")
	c.SetDir(root)
	pyTxt := "可用"
	if c.Run() != nil {
		pyTxt = "不可用"
	}
	diag := fmt.Sprintf("脚本根 %s；scripts/growth_run.py %s；解释器 %s %s", root, scriptTxt, py, pyTxt)
	if pyTxt != "可用" {
		// 只有在解释器真的跑不起来时才附上探测过程 —— 平时它是一串噪音。
		diag += "（" + PythonDiagnosis() + "）"
	}
	return diag
}

// RunGrowthFor 手动执行入口：对指定账号逐个跑一轮成长任务（zero → talk）。
//
// 同步执行，调用方（HTTP handler）应放进 goroutine 异步跑并在入口立即返回：
// 一批账号含号间 30~120s 等待，同步等待会让管理页请求超时。
// 返回 (成功号数, 失败号数)；ctx 取消时提前收尾。
func (s *Scheduler) RunGrowthFor(ctx context.Context, uids []string) (int, int) {
	started := time.Now()
	var t runTally
	defer func() { s.recordRun(taskGrowth, triggerManual, started, t) }()

	// 运行闸门：手动执行也可能被连点两次（或前后两次点击间隔很短），
	// 而成长任务每号要发十几个写请求 —— 撞车即让路，代价远小于重跑一遍。
	if s.skipIfBusy(taskGrowth, triggerManual) {
		t.skipped++
		return 0, 0
	}
	defer s.endRun(taskGrowth)

	if s.cfg.Growth == nil {
		t.skipped++
		return 0, 0
	}
	if !GrowthScriptAvailable() {
		t.note = "growth_run.py 或 python 不可用，已跳过"
		t.skipped++
		log.Printf("growth: %s", t.note)
		return 0, 0
	}
	if len(uids) == 0 {
		t.skipped++
		return 0, 0
	}

	creds, err := s.cfg.Growth.GrowthCredsByUID(uids)
	if err != nil {
		t.fail++
		t.note = "拉取账号凭证失败：" + err.Error()
		log.Printf("growth: %s", t.note)
		return 0, 0
	}
	if len(creds) == 0 {
		t.skipped++
		t.note = "选中的账号没有可执行凭证（不存在或无 access token）"
		log.Printf("growth: %s", t.note)
		return 0, 0
	}

	t.total = len(creds)
	log.Printf("growth: 手动执行 %d 个账号（号间 %d~%ds）",
		len(creds), s.growthGapMin(), s.growthGapMax())

	fails := 0
	for i, cred := range creds {
		if ctx.Err() != nil {
			t.interrupted = true
			break
		}
		if i > 0 {
			// 账号间随机等待：固定间隔会被统计出节奏，随机化才是有效的节流。
			d := s.growthGapDuration()
			log.Printf("growth: 等待 %s 后处理下一个账号", d.Round(time.Second))
			if !s.sleepCtx(ctx, d) {
				t.interrupted = true
				break
			}
		}
		if s.runGrowthForAccount(ctx, cred, &t) {
			t.ok++
			if err := s.cfg.Growth.MarkGrowthDone(cred.UID); err != nil {
				// 标记写失败不影响已完成的收益，只影响 UI 上的「上次执行时间」。
				log.Printf("growth: 记录 %s 执行时间失败：%v", logfmtUID(cred), err)
			}
		} else {
			t.fail++
			fails++
			// 失败不重试本号：连续失败往往意味着账号本身有问题（token 失效 /
			// 被风控），立刻重试只会把失败写得更密。由管理员决定是否再选一次。
		}
	}
	return t.ok, fails
}

// runGrowthForAccount 跑单个账号：zero →（成功才）talk。返回是否整体成功。
//
// 「先零消耗、后对话」不只是省钱：新号 credits 通常为 0，直接跑对话类会因额度
// 不足失败并留下失败痕迹；先用行为上报类攒到积分，再跑对话类才跑得动。
func (s *Scheduler) runGrowthForAccount(ctx context.Context, cred GrowthCred, t *runTally) bool {
	zr, err := s.execGrowthPhase(ctx, cred, "zero")
	t.already += zr.Already
	if err != nil {
		t.note = trimNote(fmt.Sprintf("%s zero 失败：%v | %s", logfmtUID(cred), err, zr.RawLog))
		log.Printf("growth: %s", t.note)
		return false
	}
	log.Printf("growth: %s zero 完成 ok=%d already=%d pending=%d credit=+%d",
		logfmtUID(cred), zr.OK, zr.Already, zr.Pending, zr.Credit)

	tr, terr := s.execGrowthPhase(ctx, cred, "talk")
	t.already += tr.Already
	if terr != nil {
		// 对话类失败不算整号失败：zero 的收益已入账，只要标记了执行时间就不会
		// 被误认为「没跑过」；损失只是对话类那部分，管理员可再选一次补跑（幂等）。
		t.note = trimNote(fmt.Sprintf("%s talk 失败（zero 已入账）：%v | %s", logfmtUID(cred), terr, tr.RawLog))
		log.Printf("growth: %s", t.note)
		return true
	}
	log.Printf("growth: %s talk 完成 ok=%d already=%d credit=+%d energy=+%d",
		logfmtUID(cred), tr.OK, tr.Already, tr.Credit, tr.Energy)
	return true
}

// growthExecHook 起进程跑一个阶段并回传合并后的输出。
// 抽成包级变量是为了让测试能替换掉真实 exec（避免单测真的拉起 python 打上游）。
var growthExecHook = func(ctx context.Context, payload []byte, phase string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, growthPerAccountTimeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, pythonCmd(), "scripts/growth_run.py", "--phase", phase, "--yes")
	cmd.Dir = repoRoot()
	cmd.Stdin = bytes.NewReader(payload)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// execGrowthPhase 跑一个阶段：python growth_run.py --phase X --yes，凭证经 stdin。
func (s *Scheduler) execGrowthPhase(ctx context.Context, cred GrowthCred, phase string) (growthResult, error) {
	payload, err := json.Marshal(map[string]any{
		"uid":    cred.UID,
		"token":  cred.AccessToken,
		"domain": cred.Domain,
		"nick":   cred.Nickname,
		"realm":  cred.Realm,
	})
	if err != nil {
		return growthResult{}, err
	}

	out, runErr := growthExecHook(ctx, payload, phase)
	res := parseGrowthResult(out, phase)
	res.RawLog = tailString(out, growthLogTailMax)

	if runErr != nil {
		return res, fmt.Errorf("脚本退出异常：%w", runErr)
	}
	if !res.ParsedOK {
		return res, fmt.Errorf("未取到结果行")
	}
	if res.SkippedNote != "" {
		// global 账号没有 CN 任务中心：脚本已明确跳过，属正常态而非失败。
		return res, nil
	}
	if res.Error != "" {
		return res, fmt.Errorf("%s", res.Error)
	}
	return res, nil
}

// parseGrowthResult 从脚本输出里抓 GROWTH_RESULT 行（最后一条优先）。
func parseGrowthResult(out, phase string) growthResult {
	res := growthResult{Phase: phase}
	idx := strings.LastIndex(out, "GROWTH_RESULT ")
	if idx < 0 {
		return res
	}
	line := out[idx+len("GROWTH_RESULT "):]
	if nl := strings.IndexByte(line, '\n'); nl >= 0 {
		line = line[:nl]
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &res); err != nil {
		return growthResult{Phase: phase}
	}
	res.ParsedOK = true
	return res
}

// growthGapDuration 账号间等待时长：在 [min,max] 内随机取。
// max<=min 时退化为固定 min（便于测试与「就想用固定值」的场景）。
func (s *Scheduler) growthGapDuration() time.Duration {
	lo, hi := s.growthGapMin(), s.growthGapMax()
	if hi <= lo {
		return time.Duration(lo) * time.Second
	}
	return time.Duration(lo+rand.Intn(hi-lo+1)) * time.Second
}

func (s *Scheduler) growthGapMin() int {
	if v := s.cfg.GrowthGapMinSeconds; v > 0 {
		return v
	}
	return defaultGrowthGapMinSec
}

func (s *Scheduler) growthGapMax() int {
	if v := s.cfg.GrowthGapMaxSeconds; v > 0 {
		return v
	}
	return defaultGrowthGapMaxSec
}

// sleepCtx 可被取消的等待；返回 false 表示被取消。
func (s *Scheduler) sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// logfmtUID 日志里标识账号（昵称优先、回退 uid 前 8 位）。
func logfmtUID(c GrowthCred) string {
	if c.Nickname != "" {
		return c.Nickname + "(" + shortUID(c.UID) + ")"
	}
	return shortUID(c.UID)
}

func shortUID(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

// tailString 截取字符串尾部（失败日志留证据；超长时保留末尾更接近错误点）。
func tailString(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return "…" + s[len(s)-max:]
}

// trimNote 台账备注截断（台账是给人和 UI 看的，过长会淹没关键信息）。
func trimNote(s string) string {
	const max = 600
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
