// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// ======================================================================
// 批量一键接入（v0.31.85）
//
// 需求：一次接入「该组里被选中的全部模型」，逐条执行、逐条报结果，
// 单个失败不中断其余，最后给汇总。
//
// 关键事实：**脚本跑在用户机器上**，服务端看不到执行过程。所以这里的断言分两层：
//   - 生成的脚本里必须真的有"逐条循环 + 逐条结果 + 汇总 + 失败时非 0 退出"；
//   - 接口层必须把整组清单交出去（而不是只给一个模型）。
// 真实执行结果由 `scripts/onboard_check.py` 的批量场景在隔离 HOME 里真跑来验。
// ======================================================================

func batchPlan() onboardPlan {
	return onboardPlan{
		Target:  onboardTargetWorkBuddy,
		BaseURL: "http://127.0.0.1:1/v1",
		Origin:  "http://127.0.0.1:1",
		APIKey:  "sk-wb-batch-test",
		Model:   "m-one",
		Models:  []string{"m-one", "m-two", "m-three"},
	}
}

// TestOnboardBatchPowerShell 批量 .bat 的 PowerShell 主体必须逐条循环并给汇总。
func TestOnboardBatchPowerShell(t *testing.T) {
	ps := buildPowerShellOnboard(batchPlan())

	if n := strings.Count(ps, `"id": `); n != 3 {
		t.Errorf("脚本里有 %d 条 entry，期望 3 条", n)
	}
	for _, want := range []string{
		"$entries = @(",                                // 一个数组，不是单条
		"foreach ($json in $entries)",                  // 逐条处理
		"$i + '/' + $entries.Count",                    // [序号/总数] 进度
		"接入完成：成功 ",                                     // 汇总
		"失败明细：",                                        // 失败原因分组
		"if ($fail.Count -gt 0) { exit 1 }",            // 有失败时非 0 退出
		"$fail += ($who + '：' + $_.Exception.Message)", // 单条失败只记录，不 throw 出去
	} {
		if !strings.Contains(ps, want) {
			t.Errorf("脚本缺少 %q —— 批量/逐条反馈不完整", want)
		}
	}
	// 单条失败必须被 try/catch 吃掉，否则第一条失败就会中断整个循环。
	if !strings.Contains(ps, "  try {") || !strings.Contains(ps, "  } catch {") {
		t.Error("批量循环里必须有 try/catch：单个失败不能中断其余")
	}
	// 写盘只能有一次（逐条写会在中途失败时留下半截配置）。
	if n := strings.Count(ps, "WriteAllText("); n != 1 {
		t.Errorf("WriteAllText 出现 %d 次，期望 1 次（合并完只写一次）", n)
	}
}

// TestOnboardBatchShell 批量 .sh 的 python 主体同样要逐条 + 汇总。
func TestOnboardBatchShell(t *testing.T) {
	sh := buildShellOnboard(batchPlan())
	for _, want := range []string{
		"WB2API_ENTRIES",                        // 数组入参
		"for i, entry in enumerate(entries, 1)", // 逐条
		"[%d/%d] ok   %s",
		"接入完成：成功 %d / %d，失败 %d",
		"失败明细：",
		"sys.exit(1)",
	} {
		if !strings.Contains(sh, want) {
			t.Errorf("shell 脚本缺少 %q", want)
		}
	}
	if n := strings.Count(sh, `json.dump(lst`); n != 1 {
		t.Errorf("json.dump 出现 %d 次，期望 1 次（合并完只写一次）", n)
	}
}

// TestOnboardBatchModels 清单去重、限长、保序。
func TestOnboardBatchModels(t *testing.T) {
	// 去重 + 去空白 + 保序
	got, trunc := onboardBatchModels([]string{" b ", "a", "b", "", "a"})
	if trunc != 0 {
		t.Errorf("不该截断，truncated=%d", trunc)
	}
	if len(got) != 2 || got[0] != "b" || got[1] != "a" {
		t.Errorf("清单 = %v，期望 [b a]", got)
	}
	// 空成员 → nil（调用方据此退回单条）
	if m, _ := onboardBatchModels(nil); len(m) != 0 {
		t.Errorf("空成员应返回空，得到 %v", m)
	}
	// 超上限截断，且报告被截掉多少
	big := make([]string, onboardBatchLimit+3)
	for i := range big {
		big[i] = "m-" + strconv.Itoa(i) // 不用 itoa：它对 n<=0 返回 "3"（tasks.go 的历史 hack）
	}
	m, trunc := onboardBatchModels(big)
	want := len(big) - onboardBatchLimit
	if len(m) != onboardBatchLimit || trunc != want {
		t.Errorf("超限时应保留 %d 条、报告截掉 %d 条，得到 %d / %d（传入 %d 条）",
			onboardBatchLimit, want, len(m), trunc, len(big))
	}
}

// TestOnboardBatchResponse 接口层：batch=false 时只接一个；不传时默认批量语义。
//
// 用一把**有密文**的 Key 走完整 handler（真实的模型清单依赖缓存，测试环境里可能为空，
// 所以这里只断言"清单字段存在且自洽"，不硬编码具体模型）。
func TestOnboardBatchResponse(t *testing.T) {
	f := newParityFixture(t)
	plain := GenerateAPIKey()
	enc, err := f.a.secret.Encrypt(plain)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	key, err := f.store.CreateAPIKey(f.owner.ID, "batch", HashAPIKey(plain), enc,
		keyPrefix(plain), "", "", "")
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	call := func(body string) (int, map[string]any) {
		req := httptest.NewRequest(http.MethodPost, "/api/keys/1/onboard", strings.NewReader(body))
		req.SetPathValue("id", itoa(int(key.ID)))
		req = withUser(req, f.owner)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		f.a.handleKeyOnboard(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	// batch=false：只接指定的那一个。
	code, out := call(`{"target":"workbuddy","model":"wb-group-work","os":"unix","batch":false}`)
	if code != http.StatusOK {
		t.Fatalf("batch=false 应 200，得到 %d（%v）", code, out)
	}
	models, _ := out["models"].([]any)
	if len(models) != 1 || models[0] != "wb-group-work" {
		t.Errorf("batch=false 的清单 = %v，期望只有 wb-group-work", models)
	}
	if out["batch"] != false {
		t.Errorf("batch = %v，期望 false", out["batch"])
	}

	// 不传 batch（默认批量）：清单至少包含第一条，且 model 字段与清单首项一致。
	code, out = call(`{"target":"workbuddy","model":"wb-group-work","os":"unix"}`)
	if code != http.StatusOK {
		t.Fatalf("默认批量应 200，得到 %d", code)
	}
	models, _ = out["models"].([]any)
	if len(models) == 0 {
		t.Fatal("清单不能为空")
	}
	if out["model"] != models[0] {
		t.Errorf("model=%v 与清单首项 %v 不一致（兼容字段必须对齐）", out["model"], models[0])
	}
	if int(out["count"].(float64)) != len(models) {
		t.Errorf("count=%v 与清单长度 %d 不一致", out["count"], len(models))
	}
	// 批量模式下脚本必须真的是数组（不然前端说"3 个"而脚本只写 1 个）。
	script, _ := out["script"].(string)
	if strings.Contains(script, "WB2API_ENTRY=") && !strings.Contains(script, "WB2API_ENTRIES=") {
		t.Error("批量模式生成的 sh 脚本缺少 WB2API_ENTRIES")
	}
}

// TestBatchModelsFromOnlyWhenRestricted 只有配了选配才批量。
//
// 没配选配时成员是整份模型目录（几十个），一键全写进客户端会让模型选择器变成一锅粥；
// 那种情况必须退回单条（组名）。
func TestBatchModelsFromOnlyWhenRestricted(t *testing.T) {
	// 未配选配 → 不批量
	if m, _ := batchModelsFrom(workGroupPlan{Restricted: false, Members: []string{"a", "b", "c"}}); len(m) != 0 {
		t.Errorf("未配选配时不该批量，得到 %v", m)
	}
	// 配了选配 → 批量
	m, _ := batchModelsFrom(workGroupPlan{Restricted: true, Members: []string{"a", "b", "c"}})
	if len(m) != 3 {
		t.Errorf("配了选配应批量接入 3 个，得到 %v", m)
	}
	// 配了但成员为空（一个都跑不了）→ 不批量，交给单条兜底
	if m, _ := batchModelsFrom(workGroupPlan{Restricted: true}); len(m) != 0 {
		t.Errorf("成员为空时不该批量，得到 %v", m)
	}
}

// TestOnboardBatchRunReal 真跑一次批量脚本（仅 Windows）：验证"单个失败不中断其余"。
//
// 为什么必须在 Go 测试里真跑：关键逻辑在脚本里（每条独立 try/catch + 一次性写盘 + 汇总），
// 单测只能验字符串存在，验不出"第一条失败后第二条还写不写"。
// 用 runtime.GOOS 守卫，非 Windows 环境自动跳过。
func TestOnboardBatchRunReal(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("批量脚本是 .bat 脚本，只在 Windows 上真跑")
	}
	home := t.TempDir()
	dest := filepath.Join(home, ".workbuddy", "models.json")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// 原有配置里放一条别的模型，验证"不会被我们清掉"。
	if err := os.WriteFile(dest, []byte(`[{"id":"keep-me","name":"保留","url":"http://x/v1"}]`), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// 第 2 条故意给空 id → 脚本里必须抓住并继续，而不是中断整个循环。
	plan := onboardPlan{
		Target: onboardTargetWorkBuddy, BaseURL: "http://127.0.0.1:1/v1",
		Origin: "http://127.0.0.1:1", APIKey: "sk-wb-real", Model: "m-good-1",
		Models: []string{"m-good-1", "", "m-good-3"},
	}
	_, script := buildOnboardScript(plan, "windows")
	bat := filepath.Join(home, "run.bat")
	if err := os.WriteFile(bat, []byte(script), 0o644); err != nil {
		t.Fatalf("写脚本: %v", err)
	}

	cmd := exec.Command("cmd", "/c", bat, "<", "nul")
	cmd.Env = append(os.Environ(), "USERPROFILE="+home, "WB_NO_PAUSE=1")
	out, err := cmd.CombinedOutput()
	text := decodeConsole(out)
	t.Logf("脚本输出：\n%s", text)

	// 有失败 → 退出码非 0（用户据此知道"不是全成功"）。
	if err == nil {
		t.Error("有失败条目时应非 0 退出")
	}
	if !strings.Contains(text, "[2/3] FAIL") {
		t.Errorf("缺少第 2 条的失败行，实际输出：%s", text)
	}
	if !strings.Contains(text, "[3/3] ok") {
		t.Error("第 2 条失败后，第 3 条必须继续执行（单个失败不能中断其余）")
	}
	if !strings.Contains(text, "接入完成：成功 2 / 3，失败 1") {
		t.Errorf("汇总行不对：%s", text)
	}

	// 写入结果：原条目保留 + 2 条成功的新条目（失败那条不进文件）。
	raw, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("读回配置: %v", err)
	}
	var list []map[string]any
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("配置不是合法 JSON 数组：%v\n%s", err, raw)
	}
	ids := map[string]bool{}
	for _, e := range list {
		if id, ok := e["id"].(string); ok {
			ids[id] = true
		}
	}
	for _, want := range []string{"keep-me", "m-good-1", "m-good-3"} {
		if !ids[want] {
			t.Errorf("配置里缺少 %q，实际 %v", want, ids)
		}
	}
	if len(list) != 3 {
		t.Errorf("配置里应有 3 条（1 原有 + 2 成功），实际 %d 条：%v", len(list), ids)
	}
	// 备份必须存在。
	bak, err := os.ReadFile(dest + ".wb2api.bak")
	if err != nil {
		t.Fatalf("备份不存在：%v", err)
	}
	if !strings.Contains(string(bak), "keep-me") {
		t.Errorf("备份内容不对：%s", bak)
	}
}

// decodeConsole 控制台输出是本地代码页（中文 Windows 为 GBK），
// 而脚本里 chcp 65001 之后写的是 UTF-8 —— 两种编码都可能出现，
// 这里按 UTF-8 优先、失败则原样返回（断言只依赖其中的 ASCII 与稳定中文片段）。
func decodeConsole(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	return string(b)
}
