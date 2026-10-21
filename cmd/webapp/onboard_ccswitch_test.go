// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// ======================================================================
// CC Switch 批量一键接入（v0.31.86）
//
// 需求：CC Switch 也要「一次接完整组模型」，逐条执行、逐条报结果、
// 单个失败不中断其余、最后给汇总。
//
// CC Switch 与 WorkBuddy 的落地方式**不同**（这是事实，不是偷懒）：
// settings.json 的 env 里一个模型一个槽（MODEL/SONNET/OPUS/HAIKU/SMALL_FAST），
// 只有 5 个。所以：
//   - 前 5 个模型各占一个槽位（`/model sonnet|opus|haiku` 能切到）；
//   - 其余模型仍算接入成功，用 `/model <名字>` 直接指定（Claude Code 会把名字原样透传）；
//   - 任何一条失败只记进失败列表，**不阻断**后面的模型。
//
// 与 WorkBuddy 版一样：脚本跑在用户机器上，服务端看不到执行过程 ——
// 所以除了字符串断言，必须有真跑（.bat 与 .sh 各一条）。
// ======================================================================

func ccBatchPlan() onboardPlan {
	return onboardPlan{
		Target:  onboardTargetCCSwitch,
		BaseURL: "http://127.0.0.1:1/v1",
		Origin:  "http://127.0.0.1:1",
		APIKey:  "sk-wb-cc-batch",
		Model:   "m-one",
		Models:  []string{"m-one", "m-two", "m-three"},
	}
}

// TestCCSwitchSlotOf 槽位顺序固定：主模型 → sonnet → opus → haiku → small_fast，越界为空。
func TestCCSwitchSlotOf(t *testing.T) {
	want := []string{
		"ANTHROPIC_MODEL",
		"ANTHROPIC_DEFAULT_SONNET_MODEL",
		"ANTHROPIC_DEFAULT_OPUS_MODEL",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL",
		"ANTHROPIC_SMALL_FAST_MODEL",
	}
	for i, w := range want {
		if got := ccSwitchSlotOf(i); got != w {
			t.Errorf("第 %d 个槽位 = %q，期望 %q", i, got, w)
		}
	}
	if len(want) != len(ccSwitchSlots) {
		t.Errorf("槽位数量 %d，期望 %d", len(ccSwitchSlots), len(want))
	}
	for _, i := range []int{-1, len(want), 99} {
		if got := ccSwitchSlotOf(i); got != "" {
			t.Errorf("越界索引 %d 应返回空槽位，得到 %q", i, got)
		}
	}
}

// TestOnboardCCSwitchBatchPowerShell 批量 .bat 的 PowerShell 主体：逐条 + 汇总 + 只写一次盘。
func TestOnboardCCSwitchBatchPowerShell(t *testing.T) {
	ps := buildPowerShellCCSwitch(ccBatchPlan())

	for _, want := range []string{
		"$list = @($parsed)",                           // 统一成数组（PS 5.1 的 @() 坑）
		"foreach ($e in $list)",                        // 逐条处理
		"$i + '/' + $list.Count",                       // [序号/总数] 进度
		"接入完成：成功 ",                                     // 汇总
		"失败明细：",                                        // 失败原因
		"if ($fail.Count -gt 0) { exit 1 }",            // 有失败 → 非 0
		"$fail += ($who + '：' + $_.Exception.Message)", // 单条失败只记录
		"ANTHROPIC_DEFAULT_SONNET_MODEL",               // 槽位
		`"m-two"`,                                      // 三个模型都进了清单（entries 是 JSON 字面量）
		`"m-three"`,
	} {
		if !strings.Contains(ps, want) {
			t.Errorf("脚本缺少 %q —— 批量/逐条反馈不完整", want)
		}
	}
	// 失败必须被 try/catch 吃掉；否则第一条失败就中断整个循环。
	if !strings.Contains(ps, "  try {") || !strings.Contains(ps, "  } catch {") {
		t.Error("批量循环里必须有 try/catch：单个失败不能中断其余")
	}
	// 写盘两次 = settings.json + 清单文件（不是逐条写）。
	if n := strings.Count(ps, "WriteAllText("); n != 2 {
		t.Errorf("WriteAllText 出现 %d 次，期望 2 次（配置文件 + 清单，各写一次）", n)
	}
	// 备份与环境校验不能因为批量被弄丢。
	if !strings.Contains(ps, ".wb2api.bak") {
		t.Error("批量路径也必须先备份原配置")
	}
	if !strings.Contains(ps, "写入后校验失败：base_url 不匹配") {
		t.Error("批量路径也必须读回校验")
	}
}

// TestOnboardCCSwitchBatchShell sh 版必须有同一套逐条逻辑（macOS/Linux 是主路径）。
func TestOnboardCCSwitchBatchShell(t *testing.T) {
	sh := buildShellOnboard(ccBatchPlan())

	for _, want := range []string{
		"WB2API_ENTRIES=",
		"for i, entry in enumerate(entries, 1)",
		"[%d/%d] ok",
		"[%d/%d] FAIL",
		"接入完成：成功 %d / %d",
		"失败明细：",
		"sys.exit(1)",
		"ANTHROPIC_DEFAULT_SONNET_MODEL",
		"wb2api-models.json",
	} {
		if !strings.Contains(sh, want) {
			t.Errorf("sh 脚本缺少 %q", want)
		}
	}
	// 逐条 try/except：失败只 append，不 raise。
	if !strings.Contains(sh, "        except Exception as ex:") {
		t.Error("sh 版必须逐条 except（单个失败不能中断其余）")
	}
	// 写配置只一次（循环外），否则中途失败会留半截。
	if n := strings.Count(sh, `json.dump(cfg, f`); n != 1 {
		t.Errorf("json.dump(cfg) 出现 %d 次，期望 1 次", n)
	}
}

// TestOnboardCCSwitchManualMultiModels 手动兜底不能比脚本少：
// 5 个槽位全给，超出部分单列出来让人 `/model <名字>`。
func TestOnboardCCSwitchManualMultiModels(t *testing.T) {
	p := ccBatchPlan()
	p.Models = []string{"a", "b", "c", "d", "e", "f", "g"}
	m := onboardManual(p)

	raw, _ := m["json"].(string)
	var got struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("manual JSON 解析失败：%v\n%s", err, raw)
	}
	for i, slot := range ccSwitchSlots {
		want := p.Models[i]
		if got.Env[slot] != want {
			t.Errorf("%s = %q，期望 %q", slot, got.Env[slot], want)
		}
	}
	if got.Env["ANTHROPIC_BASE_URL"] != p.Origin {
		t.Errorf("base_url = %q，期望 origin %q（带 /v1 会变成 /v1/v1/messages）",
			got.Env["ANTHROPIC_BASE_URL"], p.Origin)
	}
	if got.Env["ANTHROPIC_AUTH_TOKEN"] != p.APIKey {
		t.Error("token 没写进 env")
	}
	extra, _ := m["extra_models"].([]string)
	if len(extra) != 2 || extra[0] != "f" || extra[1] != "g" {
		t.Errorf("超出槽位的模型 = %v，期望 [f g]", extra)
	}
	if m["count"] != 7 {
		t.Errorf("count = %v，期望 7", m["count"])
	}
	note, _ := m["note"].(string)
	if !strings.Contains(note, "5 个") || !strings.Contains(note, "/model") {
		t.Errorf("note 必须说清槽位与 /model 用法：%s", note)
	}
}

// seedCCSwitchHome 造一个已有 settings.json 的家目录（含无关的权限与 env 键）。
func seedCCSwitchHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	seed := `{"permissions":{"allow":["Bash(ls)"]},"env":{"ANTHROPIC_MODEL":"old-model","KEEP_ME":"yes"}}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(seed), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return home
}

// assertCCSwitchResult 检查真跑后的配置、清单、备份（两个平台共用）。
func assertCCSwitchResult(t *testing.T, home, out string, exitErr error) {
	t.Helper()
	if exitErr == nil {
		t.Error("有失败条目时应非 0 退出")
	}
	if !strings.Contains(out, "[1/3] ok") {
		t.Errorf("缺少第 1 条成功行：%s", out)
	}
	if !strings.Contains(out, "[2/3] FAIL") {
		t.Errorf("缺少第 2 条失败行：%s", out)
	}
	// 这一条是本需求的核心：第 2 条必须被记进失败并**继续**处理第 3 条。
	if !strings.Contains(out, "[3/3] ok") {
		t.Errorf("第 2 条失败后必须继续处理第 3 条：%s", out)
	}
	if !strings.Contains(out, "接入完成：成功 2 / 3，失败 1") {
		t.Errorf("汇总行不对：%s", out)
	}

	dir := filepath.Join(home, ".claude")
	raw, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatalf("读 settings.json: %v", err)
	}
	var cfg struct {
		Permissions map[string]any `json:"permissions"`
		Env         map[string]any `json:"env"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("settings.json 不是合法 JSON：%v\n%s", err, raw)
	}
	if cfg.Env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:1" {
		t.Errorf("ANTHROPIC_BASE_URL = %v，期望 origin（不带 /v1）", cfg.Env["ANTHROPIC_BASE_URL"])
	}
	if cfg.Env["ANTHROPIC_AUTH_TOKEN"] != "sk-wb-cc-batch" {
		t.Error("token 没写进 env")
	}
	// 槽位按成功顺序分配：第 1 条 → MODEL，第 3 条 → SONNET（失败的第 2 条不占槽）。
	if cfg.Env["ANTHROPIC_MODEL"] != "m-one" {
		t.Errorf("ANTHROPIC_MODEL = %v，期望 m-one", cfg.Env["ANTHROPIC_MODEL"])
	}
	if cfg.Env["ANTHROPIC_DEFAULT_SONNET_MODEL"] != "m-three" {
		t.Errorf("ANTHROPIC_DEFAULT_SONNET_MODEL = %v，期望 m-three（失败项不该占槽位）",
			cfg.Env["ANTHROPIC_DEFAULT_SONNET_MODEL"])
	}
	// 无关配置必须原样保留（写坏用户其它设置是最不可接受的副作用）。
	if cfg.Env["KEEP_ME"] != "yes" {
		t.Error("原有 env 键 KEEP_ME 被丢掉了")
	}
	if cfg.Permissions == nil || cfg.Permissions["allow"] == nil {
		t.Error("原有 permissions 被丢掉了")
	}

	// 备份必须存在且是原内容。
	bak, err := os.ReadFile(filepath.Join(dir, "settings.json.wb2api.bak"))
	if err != nil {
		t.Fatalf("备份不存在：%v", err)
	}
	if !strings.Contains(string(bak), "old-model") {
		t.Errorf("备份内容不对：%s", bak)
	}

	// 清单：只有成功的两个模型，且槽位与配置一致。
	mraw, err := os.ReadFile(filepath.Join(dir, ccSwitchManifestName))
	if err != nil {
		t.Fatalf("清单文件不存在：%v", err)
	}
	var plan []struct {
		Model string `json:"model"`
		Slot  string `json:"slot"`
	}
	if err := json.Unmarshal(mraw, &plan); err != nil {
		t.Fatalf("清单不是合法 JSON：%v\n%s", err, mraw)
	}
	if len(plan) != 2 {
		t.Fatalf("清单应有 2 条（失败项不进清单），实际 %d：%v", len(plan), plan)
	}
	if plan[0].Model != "m-one" || plan[0].Slot != "ANTHROPIC_MODEL" {
		t.Errorf("清单第 1 条 = %+v", plan[0])
	}
	if plan[1].Model != "m-three" || plan[1].Slot != "ANTHROPIC_DEFAULT_SONNET_MODEL" {
		t.Errorf("清单第 2 条 = %+v", plan[1])
	}
}

// TestOnboardCCSwitchBatchRunReal 真跑 .bat（仅 Windows）。
func TestOnboardCCSwitchBatchRunReal(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("批量脚本是 .bat + PowerShell，只在 Windows 上真跑")
	}
	home := seedCCSwitchHome(t)
	plan := ccBatchPlan()
	// 第 2 条故意空 id → 必须 FAIL 且继续。
	plan.Models = []string{"m-one", "", "m-three"}

	_, script := buildOnboardScript(plan, "windows")
	bat := filepath.Join(home, "run.bat")
	if err := os.WriteFile(bat, []byte(script), 0o644); err != nil {
		t.Fatalf("写脚本: %v", err)
	}
	cmd := exec.Command("cmd", "/c", bat)
	cmd.Env = append(os.Environ(), "USERPROFILE="+home, "WB_NO_PAUSE=1")
	out, err := cmd.CombinedOutput()
	t.Logf("脚本输出：\n%s", string(out))

	assertCCSwitchResult(t, home, string(out), err)
}

// TestOnboardCCSwitchBatchRunRealShell 真跑 .sh（Git Bash / macOS / Linux）。
//
// 为什么不只验 .bat：macOS 用户走的是 .sh，而这套逻辑完全独立（python 主体）。
// 只验 Windows 等于把一半用户当"应该没事"。
func TestOnboardCCSwitchBatchRunRealShell(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("没有 bash，跳过 .sh 真跑")
	}
	py := pythonExeForTest(t)
	if py == "" {
		t.Skip("没有可用的 python，跳过 .sh 真跑")
	}
	home := seedCCSwitchHome(t)
	// python3 wrapper：bash 里 command -v python3 要找得到（脚本第一句就检查）。
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	wrapper := filepath.Join(bin, "python3")
	body := "#!/bin/sh\nexec \"" + filepath.ToSlash(py) + "\" \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(body), 0o755); err != nil {
		t.Fatalf("写 wrapper: %v", err)
	}
	_ = os.Chmod(wrapper, 0o755)

	plan := ccBatchPlan()
	plan.Models = []string{"m-one", "", "m-three"}
	_, script := buildOnboardScript(plan, "unix")
	shPath := filepath.Join(home, "run.sh")
	if err := os.WriteFile(shPath, []byte(script), 0o755); err != nil {
		t.Fatalf("写 sh: %v", err)
	}

	cmd := exec.Command(bash, filepath.ToSlash(shPath))
	env := append([]string{}, os.Environ()...)
	for _, kv := range []string{
		"HOME=" + filepath.ToSlash(home),
		"USERPROFILE=" + home,
		"PATH=" + filepath.ToSlash(bin) + ":" + filepath.ToSlash(os.Getenv("PATH")),
	} {
		env = append(env, kv)
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	t.Logf("脚本输出：\n%s", string(out))

	assertCCSwitchResult(t, home, string(out), err)
}

// pythonExeForTest 找一个可用的 python 解释器路径（给 sh 的 python3 wrapper 用）。
func pythonExeForTest(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"python3", "python"} {
		p, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		out, err := exec.Command(p, "-c", "import sys;print(sys.executable)").Output()
		if err != nil {
			continue
		}
		if v := strings.TrimSpace(string(out)); v != "" {
			return v
		}
	}
	return ""
}
