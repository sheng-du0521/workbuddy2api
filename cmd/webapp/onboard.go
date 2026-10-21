// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 一键接入（v0.31.69，需求 P3）：把"某一把 Key + 本站地址 + 某模型"写进客户端配置。
//
// # 为什么不是"服务端替用户写文件"
//
// 浏览器不能写本地文件，这是架构前提。可选的三条路：
//  1. 让本机 agent 代写 —— **普通用户没有 agent**：v0.31.53 用户端代理已整体下线，
//     `/api/agent/poll` 对 owner>0 直接回 410。剩下的只有平台机那一份 agent。
//  2. 让用户自己在应用里改 —— 那叫"手动配置"，不叫一键。
//  3. **生成一个一次性脚本**，用户下载后双击，脚本自己备份+合并+校验。
//
// 选 3。它的好处恰好是 1 的缺点：
//
//	· 不需要常驻进程，不需要给它"能写任意路径"的权限（脚本只碰一个固定文件）；
//	· 没有在线依赖（agent 离线也能用）；
//	· 用户可以自己打开看一眼再运行 —— 一次性动作天然比常驻授权更容易被审视。
//
// 代价：脚本里必然含**明文 Key**（与"复制 Key"等价）。所以：
//
//	· 文件名带前缀，便于识别与清理；
//	· 脚本运行完会提示删除自己；
//	· 接口与 handleRevealKey 同一套纪律：归属校验下推 SQL（跨用户 404）、不写日志、
//	  错误信息笼统 —— 泄露"某 id 存在"本身就是信息。
package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf16"
)

// 接入目标。写成白名单而不是"任意字符串"：target 决定脚本往哪个路径写，
// 放任意值等于让调用方指定写入路径（那就是个文件写入漏洞）。
const (
	onboardTargetWorkBuddy = "workbuddy"
	onboardTargetCCSwitch  = "ccswitch"
)

// onboardPlan 一次接入要写进客户端的东西（脚本与被测逻辑共用同一份结构）。
type onboardPlan struct {
	Target string
	// BaseURL 带 /v1 的地址（**WorkBuddy 的 models.json 要这个形态**）。
	BaseURL string
	// Origin 不带 /v1 的地址（**Anthropic 客户端要这个**：它自己拼 /v1/messages，
	// 写成 origin+/v1 会变成 /v1/v1/messages —— v0.31.69 就是这个错）。
	Origin string
	APIKey string
	Model  string
	Name   string // 展示名（WorkBuddy 的 name 字段）
	// Models 批量接入的模型清单（v0.31.85）。
	//
	// 「一键接入」接入的是**这一组里被选中的全部模型**，不是其中一个。
	// 清单由 buildOnboardPlan 的成员算出（后台选配 ∩ 这把 Key 能跑的），
	// 单个模型的成败在**脚本里逐条记录**（脚本跑在用户机器上，服务端看不到过程）。
	// 为空时退回单条（Model），即"只接一个模型/只接组名"的老行为。
	Models []string
}

// onboardBatchLimit 一次批量接入的模型条数上限。
//
// 不是怕慢，是怕把用户的 models.json 塞成一份"模型大全"：没配选配时成员可能是
// 整个号池目录（几十个），一键全写进去会让客户端模型选择器变得没法用。
// 超限时截取前 N 个，并在响应里说明（前端会显示"已截取"）。
const onboardBatchLimit = 20

// batchModelsFrom 批量模式下的清单（v0.31.93 改判据：看 plan.Batch）。
//
// 口径由 buildOnboardPlan 决定，这里只负责"要不要批量 + 限长"：
//
//	官组 + 配了选配 → 批量（成员就是后台勾选的那批）
//	官组 + 没配选配 → 单条（组名）：成员是整份号池目录，几十个会把客户端塞爆
//	通道组          → 批量（成员就是后台为该通道启用的那批模型）
//
// 为什么通道组不再要求"配了选配"：后台选配（onboard_models）是给两类官组配的，
// 通道组的"选用模型"本来就在通道自己的映射表里。上一版把它当成"没选配"，
// 结果用户点接入只得到一个组名 —— 与他"把这批模型都接进去"的预期不符。
func batchModelsFrom(pl workGroupPlan) (models []string, truncated int) {
	if !pl.Batch {
		return nil, 0
	}
	return onboardBatchModels(pl.Members)
}

// onboardBatchModels 算出这次要批量接入的模型清单（去重、保序、限长）。
//
// members 为空（没配选配 / 这把 Key 一个都跑不了）→ 返回 nil，调用方退回单条模式。
func onboardBatchModels(members []string) (models []string, truncated int) {
	seen := map[string]bool{}
	for _, m := range members {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		models = append(models, m)
	}
	if len(models) > onboardBatchLimit {
		truncated = len(models) - onboardBatchLimit
		models = models[:onboardBatchLimit]
	}
	return models, truncated
}

// onboardMaxModelLen 模型名的长度上限。
//
// 不只是"输入友好"：模型名会被拼进生成的脚本与 JSON。设上限是第一道闸，
// 真正的注入防护是下面 jsonEscape / 脚本里不使用字符串拼接（见 buildOnboardScript）。
const onboardMaxModelLen = 120

// validOnboardTarget 目标白名单。
func validOnboardTarget(t string) bool {
	return t == onboardTargetWorkBuddy || t == onboardTargetCCSwitch
}

// sanitizeModelName 校验并归一化模型名。
//
// 拒绝控制字符与引号类字符：它们会让生成的 JSON/脚本需要转义，
// 而"需要转义的地方"正是注入最容易漏的地方。模型名合法字符集本来就窄。
func sanitizeModelName(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > onboardMaxModelLen {
		return "", false
	}
	for _, r := range s {
		if r < 0x20 || r == '"' || r == '\'' || r == '\\' || r == '`' || r == '$' {
			return "", false
		}
	}
	return s, true
}

// onboardWorkBuddyEntry 生成要合并进客户端的那一条（WorkBuddy 的 models.json 元素）。
//
// 字段是**实测**出来的（2026-10-20 本机只读探测）：顶层数组，每条含
// id/name/vendor/url/apiKey/useCustomProtocol/reasoning/maxXXXTokens。
// 这里只写我们真正确定的字段 —— 多写一个猜的字段，客户端可能整条不认。
//
// ⚠️ url 必须是**完整端点**（v0.31.84 实证修正）：
// useCustomProtocol=true 的语义是"按我给的这个地址原样发请求"，客户端**不会**再拼
// `/chat/completions`。此前这里写的是 base（`…/v1`），于是 WorkBuddy 直接 POST 到
// `/v1`，我们没这个路由 → 客户端报 `internal error 404`。
// 现场日志：`[route] 404 POST /v1 ua="WorkBuddy/5.8.3 …"`。
func onboardWorkBuddyEntry(p onboardPlan, model string) map[string]any {
	name := p.Name
	if model != p.Model {
		// 批量模式下每条用自己的模型名做展示名；单条模式保持 p.Name（可能与模型名不同）。
		name = "WorkBuddy2API · " + model
	}
	return map[string]any{
		"id":                model,
		"name":              name,
		"vendor":            "custom",
		"url":               p.BaseURL + "/chat/completions",
		"apiKey":            p.APIKey,
		"useCustomProtocol": true,
	}
}

// onboardEntryModels 本次要写的模型清单（批量优先，否则单条）。
func onboardEntryModels(p onboardPlan) []string {
	if len(p.Models) > 0 {
		return p.Models
	}
	return []string{p.Model}
}

// buildOnboardScript 生成一次性接入脚本（Windows 用 .bat，其它平台用 .sh）。
//
// ## 为什么 .bat 里嵌 PowerShell 的 base64（-EncodedCommand）
//
// 直接在 .bat 里写 PowerShell 要面对 cmd 与 ps 两套转义规则（引号、^、% 、$ 全打架），
// 而模型名与 Key 是外部输入 —— 那就是一个注入面。`-EncodedCommand` 收 base64(UTF-16LE)，
// 传输层只剩 [A-Za-z0-9+/=]，**没有任何可被解析的元字符**，脚本内容因此与输入完全解耦。
//
// ## 为什么写入要用 UTF-8 无 BOM
//
// PowerShell 5.1 的 `Out-File -Encoding UTF8` 会写 BOM，而多数 JSON 解析器不认 BOM
// 开头的对象（WorkBuddy 读失败会表现为"模型没出现"，而不是报错 —— 最难查的一类）。
// 所以统一走 `[IO.File]::WriteAllText` + `UTF8Encoding($false)`。
func buildOnboardScript(p onboardPlan, goos string) (filename, content string) {
	if goos == "windows" {
		// 两个目标写的是不同文件、不同结构（数组 vs 对象的 env 字段），
		// 但壳（编码/备份/校验/自删）完全一致 —— 所以只有 payload 分支。
		ps := buildPowerShellOnboard(p)
		if p.Target == onboardTargetCCSwitch {
			ps = buildPowerShellCCSwitch(p)
		}
		enc := encodePSCommand(ps)
		bat := strings.Join([]string{
			"@echo off",
			"setlocal",
			"chcp 65001 >nul",
			"rem WorkBuddy2API one-click onboarding (" + p.Target + ")",
			// ⚠️ cmd 层（.bat 本体）的文本**一律 ASCII**：文件是 UTF-8 无 BOM，
			// 而 cmd 会在 chcp 之前按本地代码页解析既有行 —— 中文会被切在
			// 某个字节处，实测报 `'ey，运行后请删除。' is not recognized`。
			// 中文提示交给 PowerShell（那时代码页已是 65001）。
			"rem This file contains a PLAINTEXT API key. Delete it after running.",
			// `-OutputFormat Text`：显式声明 stdout 为纯文本（部分宿主默认会把对象
			// 序列化成 XML）。注意 stderr 里那条 `#< CLIXML` 是 PowerShell 在被
			// 管道/文件重定向时写信息流的固有行为 —— 双击运行（真控制台）不会出现，
			// 所以它不是缺陷，也别指望这个开关能消掉它。
			"powershell -NoProfile -NonInteractive -OutputFormat Text -ExecutionPolicy Bypass -EncodedCommand " + enc,
			"set RC=%ERRORLEVEL%",
			"echo.",
			"if not \"%RC%\"==\"0\" echo [FAILED] exit %RC% - see the message above.",
			// pause 可跳过（v0.31.80）：双击运行时它当然要停住让人看结果，
			// 但自动化/CI 里没人按键 —— 设 WB_NO_PAUSE=1 就直通。
			"if not defined WB_NO_PAUSE pause",
			// 自删只在**成功时**做（v0.31.80）：失败还删掉脚本，用户既看不到原文
			// 也没法重试，只能回控制台重新生成一遍。失败时留着文件，并提示路径。
			"if not \"%RC%\"==\"0\" (echo [KEPT] script left at %~f0 for retry & exit /b %RC%)",
			// 自删：`del %~f0` 直接跟在后面会报 "The batch file cannot be found."
			// （cmd 还持有文件句柄）。`(goto) 2>nul` 先让 cmd 跳出当前批处理、
			// 释放句柄，删除才会成功。
			"(goto) 2>nul & del \"%~f0\" >nul 2>&1",
			"exit /b %RC%",
		}, "\r\n") + "\r\n"
		return "wb2api-onboard-" + p.Target + ".bat", bat
	}
	sh := buildShellOnboard(p)
	return "wb2api-onboard-" + p.Target + ".sh", sh
}

// encodePSCommand 把脚本编成 `-EncodedCommand` 要的 base64（UTF-16LE）。
func encodePSCommand(script string) string {
	u := utf16.Encode([]rune(script))
	buf := make([]byte, 0, len(u)*2)
	for _, v := range u {
		buf = append(buf, byte(v), byte(v>>8))
	}
	return base64.StdEncoding.EncodeToString(buf)
}

// psQuote 单引号字符串（PowerShell 里最安全的字面量形式：只有 ' 需要写成 ”）。
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// buildPowerShellOnboard WorkBuddy 目标的 PowerShell 主体。
//
// 合并语义（三条都必须对，否则用户会看到"改了没生效"）：
//  1. 文件不存在 → 建目录、写数组；
//  2. 已存在 → **按 id 去重替换**（不是追加）—— 追加会让客户端里出现两个同名模型；
//  3. 原文件先备份为 `models.json.wb2api.bak`，且写完后**读回来校验**（JSON 必须能解析）。
//
// v0.31.85 起支持**批量**：一次写入组内所有被选中的模型，且逐条报告结果。
// 两条纪律：
//   - **单个条目的失败不中断其余**（每个 entry 独立 try/catch）；
//   - **写盘只做一次**（全部合并完再写）—— 逐条写会在中途失败时留下一份半截配置，
//     而用户看到的"成功 3/5"与实际文件内容就对不上了。
func buildPowerShellOnboard(p onboardPlan) string {
	models := onboardEntryModels(p)
	parts := make([]string, 0, len(models))
	for _, m := range models {
		entryJSON, _ := json.MarshalIndent(onboardWorkBuddyEntry(p, m), "", "  ")
		// 用单引号字面量：JSON 里的双引号不需要转义，只有单引号要（psQuote 已处理）。
		parts = append(parts, "  "+psQuote(string(entryJSON)))
	}
	entriesLiteral := "@(\n" + strings.Join(parts, ",\n") + "\n)"

	return strings.Join([]string{
		"$ErrorActionPreference = 'Stop'",
		"$path = Join-Path $env:USERPROFILE '.workbuddy\\models.json'",
		"$entries = " + entriesLiteral,
		"$dir = Split-Path -Parent $path",
		"if (-not (Test-Path $dir)) { New-Item -ItemType Directory -Force -Path $dir | Out-Null }",
		"$list = @()",
		"if (Test-Path $path) {",
		"  $raw = [IO.File]::ReadAllText($path)",
		"  if ($raw.Trim().Length -gt 0) {",
		"    $parsed = $raw | ConvertFrom-Json",
		"    if ($null -ne $parsed) { $list = @($parsed) }",
		"  }",
		"  Copy-Item -LiteralPath $path -Destination ($path + '.wb2api.bak') -Force",
		"  Write-Host ('已备份原配置 -> ' + $path + '.wb2api.bak')",
		"}",
		"$okIds = @()",
		"$fail = @()",
		"$i = 0",
		"foreach ($json in $entries) {",
		"  $i++",
		"  $who = '(第 ' + $i + ' 条)'",
		"  try {",
		"    $obj = $json | ConvertFrom-Json",
		"    if ($null -eq $obj -or [string]::IsNullOrWhiteSpace($obj.id)) { throw '条目缺少 id' }",
		"    $who = $obj.id",
		"    $list = @($list | Where-Object { $_.id -ne $obj.id })",
		"    $list += $obj",
		"    $okIds += $obj.id",
		"    Write-Host ('[' + $i + '/' + $entries.Count + '] ok   ' + $obj.id)",
		"  } catch {",
		"    $fail += ($who + '：' + $_.Exception.Message)",
		"    Write-Host ('[' + $i + '/' + $entries.Count + '] FAIL ' + $who + '：' + $_.Exception.Message)",
		"  }",
		"}",
		// 一次性写回：$list 已包含"原有 - 被替换的 + 本次成功的"。
		"if ($okIds.Count -gt 0) {",
		"  $kept = @($list)",
		// ConvertTo-Json 对单元素数组会退化成对象 —— 客户端读到的就不是数组了。
		"  if ($kept.Count -eq 1) { $json = '[' + ($kept[0] | ConvertTo-Json -Depth 10) + ']' }",
		"  else { $json = $kept | ConvertTo-Json -Depth 10 }",
		"  if ($null -eq $json -or $json.Trim().Length -eq 0) { throw '生成 JSON 失败' }",
		"  [IO.File]::WriteAllText($path, $json, (New-Object Text.UTF8Encoding($false)))",
		"}",
		// 校验：读回来必须能解析，且能数出条目数（写坏了要当场发现，而不是让用户去猜）。
		//
		// ⚠️ 数量必须分两步算（v0.31.80 真机测出来的坑）：
		// `@(<表达式> | ConvertFrom-Json)` 在 PS 5.1 里会把整个数组**再包一层**，
		// $check.Count 永远是 1（实测：文件里 2 条，打印「当前共 1 个模型」）。
		"$check = [IO.File]::ReadAllText($path) | ConvertFrom-Json",
		"$cnt = if ($check -is [array]) { $check.Length } else { 1 }",
		"Write-Host '===================='",
		// 拼接必须**整体加括号**：写成 `Write-Host 'a' + 'b'` 时 PowerShell 会把 `+`
		// 当成第三个参数原样打印（实测输出 `「 + name + 」`，看起来像脚本没拼好）。
		"Write-Host ('接入完成：成功 ' + $okIds.Count + ' / ' + $entries.Count + '，失败 ' + $fail.Count)",
		"if ($fail.Count -gt 0) {",
		"  Write-Host '失败明细：'",
		"  foreach ($f in $fail) { Write-Host ('  - ' + $f) }",
		"}",
		"Write-Host ('配置文件：' + $path + '（当前共 ' + $cnt + ' 个模型）')",
		"if ($okIds.Count -gt 0) { Write-Host ('打开 WorkBuddy 的模型选择即可看到 ' + $okIds.Count + ' 个「WorkBuddy2API」条目。') }",
		"if ($fail.Count -gt 0) { exit 1 }",
		"exit 0",
	}, "\n")
}

// ccSwitchSlots Claude Code 认的模型槽位（顺序即分配顺序）。
//
// 为什么是这五个：`ANTHROPIC_MODEL` 是主模型；`ANTHROPIC_DEFAULT_SONNET_MODEL`
// / `_OPUS_` / `_HAIKU_` 是 Claude Code 里 `/model sonnet|opus|haiku` 的别名落点；
// `ANTHROPIC_SMALL_FAST_MODEL` 是后台快任务。批量接入按这个顺序占位，
// 用户重启后在 `/model` 里就能逐个切到它们。
//
// 超出五个的模型**不占槽位，但仍算接入成功**：Claude Code 支持
// `/model <任意模型名>`，请求会把该名字原样透传（我们按名选号）。
// 这一点必须在汇总里显式写出来，不能含糊成"全都配好了"。
var ccSwitchSlots = []string{
	"ANTHROPIC_MODEL",
	"ANTHROPIC_DEFAULT_SONNET_MODEL",
	"ANTHROPIC_DEFAULT_OPUS_MODEL",
	"ANTHROPIC_DEFAULT_HAIKU_MODEL",
	"ANTHROPIC_SMALL_FAST_MODEL",
}

// ccSwitchSlotOf 第 i 个模型落在哪个槽位（越界返回 ""）。
func ccSwitchSlotOf(i int) string {
	if i < 0 || i >= len(ccSwitchSlots) {
		return ""
	}
	return ccSwitchSlots[i]
}

// ccSwitchManifestName 我们自己的接入清单文件名（落在 ~/.claude/）。
//
// 刻意**不写** CC Switch 的 providers 表：那是它的第三方 SSOT（schema 由它自己
// 迁移，它也会写）。清单只是给人看/给脚本重跑对照，写坏了不影响任何客户端。
const ccSwitchManifestName = "wb2api-models.json"

// ccSwitchEntries 本次要写进 env 的模型条目（批量优先，否则单条）。
func ccSwitchEntries(p onboardPlan) []map[string]any {
	models := onboardEntryModels(p)
	out := make([]map[string]any, 0, len(models))
	for _, m := range models {
		out = append(out, map[string]any{"id": m})
	}
	return out
}

// buildPowerShellCCSwitch CC Switch / Claude Code 目标（v0.31.70，v0.31.86 批量）。
//
// 写的是 `~/.claude/settings.json` 的 `env`。**为什么不写 cc-switch.db**：
// 那张表是 CC Switch 自己的 SSOT（第三方 schema，它自己也会写），直接改它
// 既可能被覆盖、也可能破坏它的迁移；而 settings.json 是 Claude Code 认的格式，
// CC Switch 在 direct 模式下本身就是写这份文件。
//
// 批量的语义与 WorkBuddy 不同，必须说清：
//   - env 里"一个模型一个槽"最多五个（见 ccSwitchSlots），所以前五个占槽位；
//   - 其余模型仍逐个"接入"（校验 + 记进清单）—— 在 Claude Code 里用
//     `/model <名字>` 直接指定即可，不需要占槽位；
//   - 逐条 try/catch：单个失败只记进 $fail 并继续，**绝不中断**后面的模型；
//   - settings.json 只在全部跑完后**写一次**（逐条写会在中途失败时留下半截配置）。
func buildPowerShellCCSwitch(p onboardPlan) string {
	blob, _ := json.Marshal(ccSwitchEntries(p))
	slotList := "'" + strings.Join(ccSwitchSlots, "', '") + "'"
	return strings.Join([]string{
		"$ErrorActionPreference = 'Stop'",
		"$path = Join-Path $env:USERPROFILE '.claude\\settings.json'",
		"$manifest = Join-Path $env:USERPROFILE '.claude\\" + ccSwitchManifestName + "'",
		"$dir = Split-Path -Parent $path",
		"if (-not (Test-Path $dir)) { New-Item -ItemType Directory -Force -Path $dir | Out-Null }",
		"$cfg = $null",
		"if (Test-Path $path) {",
		"  $raw = [IO.File]::ReadAllText($path)",
		"  if ($raw.Trim().Length -gt 0) { $cfg = $raw | ConvertFrom-Json }",
		"  Copy-Item -LiteralPath $path -Destination ($path + '.wb2api.bak') -Force",
		"  Write-Host ('已备份原配置 -> ' + $path + '.wb2api.bak')",
		"}",
		"if ($null -eq $cfg) { $cfg = New-Object psobject }",
		// env 必须存在且是对象；若它被写成字符串（手改过），明确失败而不是覆盖。
		"if (-not ($cfg.PSObject.Properties.Name -contains 'env')) {",
		"  $cfg | Add-Member -NotePropertyName env -NotePropertyValue (New-Object psobject) -Force",
		"} elseif ($cfg.env -is [string]) {",
		"  throw '现有 settings.json 里的 env 是字符串而不是对象，为避免写坏已中止（请先手工改正）'",
		"}",
		"$cfg.env | Add-Member -NotePropertyName ANTHROPIC_BASE_URL -NotePropertyValue " + psQuote(p.Origin) + " -Force",
		"$cfg.env | Add-Member -NotePropertyName ANTHROPIC_AUTH_TOKEN -NotePropertyValue " + psQuote(p.APIKey) + " -Force",
		// 再设一次 base_url/token 之后才处理模型：即使模型条全部失败，
		// 一个能用的 base+token 配置也已经落盘（不会是"什么都没写"）。
		"$slots = @(" + slotList + ")",
		"$entriesRaw = " + psQuote(string(blob)),
		// @(... | ConvertFrom-Json) 在 PS 5.1 里会再包一层（v0.31.80 踩过），
		// 所以先赋值、再用 @() 统一成数组。
		"$parsed = $entriesRaw | ConvertFrom-Json",
		"$list = @($parsed)",
		"$ok = @()",
		"$fail = @()",
		"$plan = @()",
		"$i = 0",
		"foreach ($e in $list) {",
		"  $i = $i + 1",
		"  $mid = ''",
		"  try {",
		"    $mid = [string]$e.id",
		"    if ([string]::IsNullOrWhiteSpace($mid)) { throw '条目缺少 id' }",
		"    foreach ($p0 in $plan) { if ($p0.model -eq $mid) { throw '同一次接入里出现重复模型' } }",
		"    $slot = ''",
		"    if ($plan.Count -lt $slots.Count) { $slot = $slots[$plan.Count] }",
		"    if ($slot -ne '') { $cfg.env | Add-Member -NotePropertyName $slot -NotePropertyValue $mid -Force }",
		"    $plan += (New-Object psobject -Property @{ model = $mid; slot = $slot })",
		"    $ok += $mid",
		"    $where = if ($slot -ne '') { '-> ' + $slot } else { '(槽位已满，Claude Code 里用 /model ' + $mid + ' 直接指定)' }",
		"    Write-Host ('[' + $i + '/' + $list.Count + '] ok   ' + $mid + ' ' + $where)",
		"  } catch {",
		"    $who = if ($mid) { $mid } else { '(第 ' + $i + ' 条)' }",
		"    $fail += ($who + '：' + $_.Exception.Message)",
		"    Write-Host ('[' + $i + '/' + $list.Count + '] FAIL ' + $fail[-1])",
		"  }",
		"}",
		"if ($ok.Count -gt 0) {",
		"  $json = $cfg | ConvertTo-Json -Depth 20",
		"  [IO.File]::WriteAllText($path, $json, (New-Object Text.UTF8Encoding($false)))",
		"  $check = [IO.File]::ReadAllText($path) | ConvertFrom-Json",
		"  if ($check.env.ANTHROPIC_BASE_URL -ne " + psQuote(p.Origin) + ") { throw '写入后校验失败：base_url 不匹配' }",
		"  $mf = $plan | ConvertTo-Json -Depth 10",
		"  if ($plan.Count -eq 1) { $mf = '[' + ($plan[0] | ConvertTo-Json -Depth 10) + ']' }",
		"  [IO.File]::WriteAllText($manifest, $mf, (New-Object Text.UTF8Encoding($false)))",
		"  Write-Host '===================='",
		"  Write-Host ('接入完成：成功 ' + $ok.Count + ' / ' + $list.Count + '，失败 ' + $fail.Count)",
		"  if ($fail.Count -gt 0) {",
		"    Write-Host '失败明细：'",
		"    foreach ($f in $fail) { Write-Host ('  - ' + $f) }",
		"  }",
		"  Write-Host ('  ANTHROPIC_BASE_URL = ' + $check.env.ANTHROPIC_BASE_URL)",
		"  Write-Host '模型槽位（重启 Claude Code 后 /model 可用）：'",
		"  foreach ($p1 in $plan) {",
		"    $line = '  ' + $p1.model",
		"    if ($p1.slot -ne '') { $line = $line + ' -> ' + $p1.slot } else { $line = $line + ' -> /model 直接指定' }",
		"    Write-Host $line",
		"  }",
		"  Write-Host ('清单文件：' + $manifest)",
		"} else {",
		"  Write-Host '===================='",
		"  Write-Host ('接入完成：成功 0 / ' + $list.Count + '，全部失败（原配置未改动）')",
		"  foreach ($f in $fail) { Write-Host ('  - ' + $f) }",
		"}",
		"if ($fail.Count -gt 0) { exit 1 }",
		"exit 0",
	}, "\n")
}

// buildShellOnboard macOS / Linux 的等价实现（用 python3 处理 JSON，没有则明确报错）。
//
// 不用 sed/awk 改 JSON：JSON 不是行式格式，"看起来改对了"的 sed 在遇到
// 单行紧凑 JSON 或转义字符时会静默写坏文件 —— 而写坏的是用户的客户端配置。
func buildShellOnboard(p onboardPlan) string {
	entryModels := onboardEntryModels(p)
	// 单条（兼容旧字段 WB2API_ENTRY：ccswitch 分支也只认一条 env）。
	entryJSON, _ := json.Marshal(onboardWorkBuddyEntry(p, entryModels[0]))
	// 两个目标共用一个 python 主体（按 WB2API_TARGET 分支）：
	// 差别只有"写数组元素"与"合并对象字段"，而备份/校验/UTF-8 语义完全一样。
	py := `import json, os, shutil, sys
target = os.environ.get("WB2API_TARGET", "workbuddy")
home = os.path.expanduser("~")
if target == "ccswitch":
    path = os.path.join(home, ".claude", "settings.json")
else:
    path = os.path.join(home, ".workbuddy", "models.json")
os.makedirs(os.path.dirname(path), exist_ok=True)
data = None
if os.path.exists(path):
    raw = open(path, encoding="utf-8").read().strip()
    if raw:
        data = json.loads(raw)
    shutil.copy2(path, path + ".wb2api.bak")
    print("已备份原配置 -> " + path + ".wb2api.bak")
if target == "ccswitch":
    cfg = data if isinstance(data, dict) else {}
    env = cfg.get("env")
    if isinstance(env, str):
        sys.exit("settings.json 里的 env 是字符串而不是对象，为避免写坏已中止")
    if not isinstance(env, dict):
        env = {}
    env["ANTHROPIC_BASE_URL"] = os.environ["WB2API_BASE_URL"]
    env["ANTHROPIC_AUTH_TOKEN"] = os.environ["WB2API_API_KEY"]
    cfg["env"] = env
    slots = ["ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL",
             "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL",
             "ANTHROPIC_SMALL_FAST_MODEL"]
    manifest = os.path.join(home, ".claude", "wb2api-models.json")
    try:
        entries = json.loads(os.environ.get("WB2API_ENTRIES") or "[]")
    except Exception:
        entries = []
    if not isinstance(entries, list) or not entries:
        entries = [json.loads(os.environ["WB2API_ENTRY"])]
    ok, fail, plan = [], [], []
    for i, entry in enumerate(entries, 1):
        mid = ""
        try:
            mid = str((entry or {}).get("id") or "")
            if not mid:
                raise ValueError("条目缺少 id")
            if any(x["model"] == mid for x in plan):
                raise ValueError("同一次接入里出现重复模型")
            slot = slots[len(plan)] if len(plan) < len(slots) else ""
            if slot:
                env[slot] = mid
            plan.append({"model": mid, "slot": slot})
            ok.append(mid)
            where = ("-> " + slot) if slot else ("(槽位已满，Claude Code 里用 /model %s 直接指定)" % mid)
            print("[%d/%d] ok   %s %s" % (i, len(entries), mid, where))
        except Exception as ex:
            fail.append("%s：%s" % (mid or ("(第 %d 条)" % i), ex))
            print("[%d/%d] FAIL %s" % (i, len(entries), fail[-1]))
    if ok:
        with open(path, "w", encoding="utf-8", newline="\n") as f:
            json.dump(cfg, f, ensure_ascii=False, indent=2)
            f.write("\n")
        check = json.load(open(path, encoding="utf-8"))
        got = (check.get("env") or {}).get("ANTHROPIC_BASE_URL")
        if got != os.environ["WB2API_BASE_URL"]:
            sys.exit("写入后校验失败：base_url 不匹配")
        with open(manifest, "w", encoding="utf-8", newline="\n") as f:
            json.dump(plan, f, ensure_ascii=False, indent=2)
            f.write("\n")
        print("=" * 20)
        print("接入完成：成功 %d / %d，失败 %d" % (len(ok), len(entries), len(fail)))
        if fail:
            print("失败明细：")
            for f1 in fail:
                print("  - " + f1)
        print("  ANTHROPIC_BASE_URL = " + got)
        print("模型槽位（重启 Claude Code 后 /model 可用）：")
        for x in plan:
            print("  " + x["model"] + (" -> " + x["slot"] if x["slot"] else " -> /model 直接指定"))
        print("清单文件：" + manifest)
    else:
        print("=" * 20)
        print("接入完成：成功 0 / %d，全部失败（原配置未改动）" % len(entries))
        for f1 in fail:
            print("  - " + f1)
    if fail:
        sys.exit(1)
else:
    # v0.31.85：批量。entries 是数组；旧调用方只给单条 WB2API_ENTRY 也能跑。
    try:
        entries = json.loads(os.environ.get("WB2API_ENTRIES") or "[]")
    except Exception:
        entries = []
    if not isinstance(entries, list) or not entries:
        entries = [json.loads(os.environ["WB2API_ENTRY"])]
    lst = []
    if isinstance(data, list):
        lst = data
    elif isinstance(data, dict):
        lst = [data]
    ok, fail = [], []
    for i, entry in enumerate(entries, 1):
        mid = (entry or {}).get("id") or ""
        try:
            if not mid:
                raise ValueError("条目缺少 id")
            # 按 id 去重替换：追加会让客户端里出现两个同名模型。
            lst = [x for x in lst if not (isinstance(x, dict) and x.get("id") == mid)]
            lst.append(entry)
            ok.append(mid)
            print("[%d/%d] ok   %s" % (i, len(entries), mid))
        except Exception as ex:
            fail.append("%s：%s" % (mid or ("(第 %d 条)" % i), ex))
            print("[%d/%d] FAIL %s" % (i, len(entries), fail[-1]))
    # 写盘只在最后做一次：逐条写会在中途失败时留下半截配置。
    if ok:
        with open(path, "w", encoding="utf-8", newline="\n") as f:
            json.dump(lst, f, ensure_ascii=False, indent=2)
            f.write("\n")
        check = json.load(open(path, encoding="utf-8"))
        print("=" * 20)
        print("接入完成：成功 %d / %d，失败 %d" % (len(ok), len(entries), len(fail)))
        if fail:
            print("失败明细：")
            for f1 in fail:
                print("  - " + f1)
        print("配置文件：" + path + "（当前共 %d 个模型）" % len(check))
        print("打开 WorkBuddy 的模型选择即可看到 %d 个「WorkBuddy2API」条目。" % len(ok))
    else:
        print("=" * 20)
        print("接入完成：成功 0 / %d，全部失败（原配置未改动）" % len(entries))
        for f1 in fail:
            print("  - " + f1)
    if fail:
        sys.exit(1)
`
	baseURL := p.BaseURL
	if p.Target == onboardTargetCCSwitch {
		baseURL = p.Origin
	}
	// 收尾提示必须**按目标**给（v0.31.80 真机发现）：此前两句写死，
	// ccswitch 用户会看到"打开 WorkBuddy 的模型选择"——去翻一个他根本没配的东西。
	verifyHint := "  WorkBuddy：打开模型选择，应看到「" + p.Name + "」。"
	if p.Target == onboardTargetCCSwitch {
		verifyHint = "  重启 Claude Code 后 /status 应显示新的 base_url（" + baseURL + "）。"
	}
	// 批量：把整组模型编成一个 JSON 数组传进去（单条仍保留 WB2API_ENTRY，兼容旧逻辑）。
	var entryArr []json.RawMessage
	for _, m := range entryModels {
		b, _ := json.Marshal(onboardWorkBuddyEntry(p, m))
		entryArr = append(entryArr, b)
	}
	entriesJSON := "[]"
	if len(entryArr) > 0 {
		if b, err := json.Marshal(entryArr); err == nil {
			entriesJSON = string(b)
		}
	}
	return strings.Join([]string{
		"#!/usr/bin/env bash",
		"# WorkBuddy2API 一键接入脚本（" + p.Target + "）。本文件含明文 API Key，运行后请删除。",
		"set -euo pipefail",
		"if ! command -v python3 >/dev/null 2>&1; then",
		"  echo '需要 python3 来处理 JSON（不会用 sed 硬改 —— 那会把配置写坏）。'",
		"  echo 'macOS: xcode-select --install   /   Debian: apt install python3'",
		"  exit 2",
		"fi",
		"export WB2API_TARGET=" + shellQuote(p.Target),
		"export WB2API_BASE_URL=" + shellQuote(baseURL),
		"export WB2API_API_KEY=" + shellQuote(p.APIKey),
		"export WB2API_ENTRY=" + shellQuote(string(entryJSON)),
		"export WB2API_ENTRIES=" + shellQuote(entriesJSON),
		"python3 - <<'WB2API_PY'",
		py + "WB2API_PY",
		"echo '接入完成：'",
		"echo " + shellQuote(verifyHint),
		"rm -- \"$0\" 2>/dev/null || true",
	}, "\n") + "\n"
}

// shellQuote 单引号包裹（POSIX sh 里唯一没有展开风险的形式）。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ------------------------------------------------------------------ 接口

// handleKeyOnboard POST /api/keys/{id}/onboard
//
//	{ "target": "workbuddy" | "ccswitch", "model": "glm-5.3" }
//
// 返回脚本与手动配置片段。**不落库、无状态**：这是一次"生成物"，
// 不需要任务表，也就不存在"任务挂在库里等人清"的问题。
func (a *app) handleKeyOnboard(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "非法 id"})
		return
	}
	var req struct {
		Target string `json:"target"`
		Model  string `json:"model"`
		OS     string `json:"os"` // 可选：windows / unix（默认按服务端平台猜，前端可以覆盖）
		// Batch 批量接入（v0.31.85）：把整组被选中的模型一次接进去。缺省 true。
		// 用指针是为了区分"没传"（=true，默认批量）与"显式 false"（只接一个）。
		Batch *bool `json:"batch"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	target := strings.TrimSpace(req.Target)
	if !validOnboardTarget(target) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "不支持的接入目标", "code": "bad_target",
		})
		return
	}
	// 批量默认开启：这个功能的默认行为就是"把整组接进去"。
	batch := true
	if req.Batch != nil {
		batch = *req.Batch
	}
	// 归属校验下推 SQL：跨用户查直接 404，不泄露存在性（与 reveal 同一纪律）。
	k, err := a.store.GetAPIKey(u.ID, id)
	if err != nil || k == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "密钥不存在"})
		return
	}
	// 停用的 Key 不许接入：让它进客户端只会得到 401，用户会以为是"接入失败"。
	if k.Status != StatusActive {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "这把密钥已停用，先启用再接入（停用的密钥调用会被拒）", "code": "key_disabled",
		})
		return
	}
	// 没传模型 = 用「work官组」的默认模型（v0.31.83）。
	//
	// 这是"默认会配置的模型"真正的落点：不只是弹窗替你填好，**接口层也认这个默认**。
	// 调用方（脚本/第三方）不传 model 也能接入，且拿到的一定是这类 Key 的正确组名
	// （号池官组 / 自挂官组），不会因为"没填"而失败或落到错误的组。
	if strings.TrimSpace(req.Model) == "" {
		if def := a.buildOnboardPlan(k).Default; def != "" {
			req.Model = def
		}
	}
	model, ok := sanitizeModelName(req.Model)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "模型名不合法（不能为空、不能含引号/控制字符，长度 ≤120）",
			"code":  "bad_model",
		})
		return
	}
	// 「work官组」只有两类，且必须与 Key 类型匹配（v0.31.82）。
	// 判定走 groupKindMismatch —— 与 ServeChat 那一处**共用同一个函数**（v0.31.83），
	// 保证"能不能接入"和"能不能调用"用同一把尺子。
	if strings.HasPrefix(strings.TrimSpace(model), groupModelPrefix) {
		gid := strings.TrimPrefix(strings.TrimSpace(model), groupModelPrefix)
		if code, msg, hint := groupKindMismatch(k.Kind, gid); code != "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": msg + "（" + model + "）。" + hint,
				"code":  code,
			})
			return
		}
	}
	plain, derr := a.secret.Decrypt(k.KeyEnc)
	if derr != nil || plain == "" {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "这把密钥没有可回显的密文（创建于更早版本或加密密钥已更换），请重新生成",
			"code":  "no_cipher",
		})
		return
	}

	origin := requestOrigin(r)
	plan := onboardPlan{
		Target:  target,
		BaseURL: origin + "/v1",
		Origin:  origin,
		APIKey:  plain,
		Model:   model,
		Name:    "WorkBuddy2API · " + model,
	}
	// 批量接入（v0.31.85，v0.31.86 扩到 CC Switch）：把**这一组里被选中的
	// 全部模型**一次接进去，两个目标同一套规则 —— "能接几个模型"不该看目标，
	// 用户看到的应当是同一个行为。
	//
	// 两个目标的落地方式不同（**这是实现细节，不是产品差异**）：
	//   WorkBuddy  → models.json 数组，一个模型一条，天然无上限；
	//   CC Switch  → settings.json 的 env，前五个模型占槽位
	//                （MODEL / SONNET / OPUS / HAIKU / SMALL_FAST），
	//                其余模型仍接入成功，在 `/model <名字>` 里直接可用。
	//
	// ⚠️ 要不要批量由 plan.Batch 决定（v0.31.93）：官组只在配了选配时批量；
	// 通道组**总是批量**（它后台启用的那几个模型就是用户要接的清单）。
	// 没配选配的官组仍退回单条（组名），也就是"让平台替我挑一个"。
	batchTruncated := 0
	if batch {
		if m, truncated := batchModelsFrom(a.buildOnboardPlan(k)); len(m) > 0 {
			plan.Models = m
			plan.Model = m[0]
			batchTruncated = truncated
		}
	}
	// 客户端平台：不能只看服务端（服务端在 Linux 上跑，用户在 Windows 上用它）。
	// 前端知道自己跑在哪，所以让它传；没传时按服务端平台给一个能跑的方向。
	goos := strings.ToLower(strings.TrimSpace(req.OS))
	if goos != "windows" && goos != "unix" {
		goos = "windows" // 绝大多数用户在 Windows；脚本内容会明示怎么改成 sh
	}
	filename, script := buildOnboardScript(plan, goos)

	entryModels := onboardEntryModels(plan)
	resp := map[string]any{
		"target": target, "model": plan.Model, "base_url": plan.BaseURL,
		"filename": filename, "script": script,
		"key_masked": maskSecret(plain),
		"manual":     onboardManual(plan),
		// 批量信息（v0.31.85）：前端据此显示"将接入 N 个模型"与清单。
		// 真实成败由**脚本在用户机器上**逐条打印并汇总 —— 服务端看不到那个过程，
		// 所以这里只给"计划接入的清单"，不伪造结果。
		"models":    entryModels,
		"count":     len(entryModels),
		"batch":     len(plan.Models) > 1,
		"truncated": batchTruncated,
	}
	writeJSON(w, http.StatusOK, resp)
}

// onboardManual 手动配置信息（给不愿/不能运行脚本的人）。
//
// 与脚本走**同一份 plan**：两条路必须给出同样的值，否则会出现
// "脚本能通、手动配不通"这种最难解释的支持问题。
func onboardManual(p onboardPlan) map[string]any {
	// base_url 按目标给**对应形态**：OpenAI 兼容要 /v1，Anthropic 要 origin。
	// 给错的形态不会报错，只会 404 —— 那是用户最难自查的一类问题。
	bu := p.BaseURL
	if p.Target == onboardTargetCCSwitch {
		bu = p.Origin
	}
	out := map[string]any{
		"base_url": bu,
		"api_key":  p.APIKey,
		"model":    p.Model,
	}
	switch p.Target {
	case onboardTargetWorkBuddy:
		// 手动兜底同样给**批量**：一次复制一个数组进去，而不是让人逐条粘贴。
		models := onboardEntryModels(p)
		entries := make([]map[string]any, 0, len(models))
		for _, m := range models {
			entries = append(entries, onboardWorkBuddyEntry(p, m))
		}
		blob, _ := json.MarshalIndent(entries, "", "  ")
		out["path"] = `%USERPROFILE%\.workbuddy\models.json` +
			`（macOS/Linux: ~/.workbuddy/models.json）`
		out["note"] = "顶层是数组：把下面这些对象合并进数组；已有同 id 的条目时**替换它**（不要留两条同名）。" +
			"共 " + itoa(len(entries)) + " 条。"
		out["json"] = string(blob)
		out["count"] = len(entries)
	case onboardTargetCCSwitch:
		// CC Switch 自己有一张 providers 表（SQLite，第三方 SSOT），直接改它有风险：
		// schema 会变、它自己也会写。这里给的是 Claude Code 认的 settings.json 形态，
		// 也就是 CC Switch 切到 direct 模式时用的那种配置。
		//
		// 批量时和脚本**同一套槽位**（前五个模型占 MODEL/SONNET/OPUS/HAIKU/SMALL_FAST），
		// 超出五个的写在 models 字段里让人自己 `/model <名字>` —— 手动路径不能比脚本少。
		models := onboardEntryModels(p)
		env := map[string]any{
			"ANTHROPIC_BASE_URL":   p.Origin,
			"ANTHROPIC_AUTH_TOKEN": p.APIKey,
		}
		extra := make([]string, 0, len(models))
		for i, m := range models {
			if slot := ccSwitchSlotOf(i); slot != "" {
				env[slot] = m
				continue
			}
			extra = append(extra, m)
		}
		blob, _ := json.MarshalIndent(map[string]any{"env": env}, "", "  ")
		out["path"] = `%USERPROFILE%\.claude\settings.json（macOS/Linux: ~/.claude/settings.json）`
		out["note"] = "把 env 合并进现有 settings.json（ANTHROPIC_BASE_URL 填**不带 /v1** 的地址：" +
			"客户端自己拼 /v1/messages）。共接入 " + itoa(len(models)) + " 个模型：前 " +
			itoa(minInt(len(models), len(ccSwitchSlots))) + " 个占槽位（/model 里按别名切换）" +
			func() string {
				if len(extra) == 0 {
					return "。"
				}
				return "，其余 " + itoa(len(extra)) + " 个用 /model <名字> 直接指定。"
			}() +
			"若 CC Switch 处于 routing 模式，需在它内部再选一次供应商 —— 否则它会在下次切换时覆盖这里。"
		out["json"] = string(blob)
		out["models"] = models
		out["count"] = len(models)
		if len(extra) > 0 {
			out["extra_models"] = extra
		}
	}
	return out
}

// ------------------------------------------------------------------ 能力探测

// handleOnboardCaps GET /api/onboard/caps —— 前端据此决定显示哪些接入目标。
//
// 走 userGuard 而不是完全公开：这页只在控制台里用，公开出去只是多一处
// "能探测部署形态"的面。目标列表本身写在前端也行，但**能力清单归服务端**
// 才不会出现"前端多写一个后端不支持的目标"。
func (a *app) handleOnboardCaps(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.onboardCaps())
}

// onboardCaps 前端据此决定显示哪些目标（现在两个都稳定，留钩子给后续 anthropic 原生）。
func (a *app) onboardCaps() map[string]any {
	return map[string]any{
		"targets": []map[string]any{
			{"id": onboardTargetWorkBuddy, "label": "WorkBuddy", "desc": "写入 ~/.workbuddy/models.json（按 id 去重替换）"},
			{"id": onboardTargetCCSwitch, "label": "CC Switch", "desc": "写入 ~/.claude/settings.json 的 env（direct 模式）"},
		},
	}
}
