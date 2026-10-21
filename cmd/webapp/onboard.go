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

// onboardJSON 生成要合并进客户端的那一条（WorkBuddy 的 models.json 元素）。
//
// 字段是**实测**出来的（2026-10-20 本机只读探测）：顶层数组，每条含
// id/name/vendor/url/apiKey/useCustomProtocol/reasoning/maxXXXTokens。
// 这里只写我们真正确定的字段 —— 多写一个猜的字段，客户端可能整条不认。
func onboardWorkBuddyEntry(p onboardPlan) map[string]any {
	return map[string]any{
		"id":                p.Model,
		"name":              p.Name,
		"vendor":            "custom",
		"url":               p.BaseURL,
		"apiKey":            p.APIKey,
		"useCustomProtocol": true,
	}
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
			"powershell -NoProfile -NonInteractive -ExecutionPolicy Bypass -EncodedCommand " + enc,
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
func buildPowerShellOnboard(p onboardPlan) string {
	entry := onboardWorkBuddyEntry(p)
	entryJSON, _ := json.MarshalIndent(entry, "", "  ")
	// 用单引号字面量：JSON 里的双引号不需要转义，只有单引号要（psQuote 已处理）。
	return strings.Join([]string{
		"$ErrorActionPreference = 'Stop'",
		"$path = Join-Path $env:USERPROFILE '.workbuddy\\models.json'",
		"$id = " + psQuote(p.Model),
		"$entryJson = " + psQuote(string(entryJSON)),
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
		"$entry = $entryJson | ConvertFrom-Json",
		"$kept = @($list | Where-Object { $_.id -ne $id })",
		"$kept += $entry",
		// ConvertTo-Json 对单元素数组会退化成对象 —— 客户端读到的就不是数组了。
		// 用 @() 包一层再判断数量，是 PowerShell 里唯一可靠的写法。
		"if ($kept.Count -eq 1) { $json = '[' + ($kept[0] | ConvertTo-Json -Depth 10) + ']' }",
		"else { $json = $kept | ConvertTo-Json -Depth 10 }",
		"if ($null -eq $json -or $json.Trim().Length -eq 0) { throw '生成 JSON 失败' }",
		"[IO.File]::WriteAllText($path, $json, (New-Object Text.UTF8Encoding($false)))",
		// 校验：读回来必须能解析，且能数出条目数（写坏了要当场发现，而不是让用户去猜）。
		//
		// ⚠️ 数量必须分两步算（v0.31.80 真机测出来的坑）：
		// `@(<表达式> | ConvertFrom-Json)` 在 PS 5.1 里会把整个数组**再包一层**，
		// $check.Count 永远是 1（实测：文件里 2 条，打印「当前共 1 个模型」）。
		// 它不影响写入结果，但在"刚接入完"这个最紧张的时刻告诉用户"你的模型只剩 1 个"，
		// 会让人以为原配置被覆盖了。先赋值、再按类型取长度才是对的。
		"$check = [IO.File]::ReadAllText($path) | ConvertFrom-Json",
		"$cnt = if ($check -is [array]) { $check.Length } else { 1 }",
		"Write-Host ('已写入 ' + $path + '（当前共 ' + $cnt + ' 个模型）')",
		// 拼接必须**整体加括号**：写成 `Write-Host 'a' + 'b'` 时 PowerShell 会把 `+`
		// 当成第三个参数原样打印（实测输出 `「 + name + 」`，看起来像脚本没拼好）。
		"Write-Host ('接入完成：打开 WorkBuddy 的模型选择即可看到「' + " + psQuote(p.Name) + " + '」。')",
		"exit 0",
	}, "\n")
}

// buildPowerShellCCSwitch CC Switch / Claude Code 目标（v0.31.70）。
//
// 写的是 `~/.claude/settings.json` 的 `env` 两个键。**为什么不写 cc-switch.db**：
// 那张表是 CC Switch 自己的 SSOT（第三方 schema，它自己也会写），直接改它
// 既可能被覆盖、也可能破坏它的迁移；而 settings.json 是 Claude Code 认的格式，
// CC Switch 在 direct 模式下本身就是写这份文件。
//
// 与 WorkBuddy 版的差别只有三处：路径、要合并的是**对象字段**而不是数组、
// 以及必须保留 settings.json 里其它键（permissions / hooks …）——
// 所以用 ConvertFrom-Json → 改 env → ConvertTo-Json 整体回写，而不是拼字符串。
func buildPowerShellCCSwitch(p onboardPlan) string {
	return strings.Join([]string{
		"$ErrorActionPreference = 'Stop'",
		"$path = Join-Path $env:USERPROFILE '.claude\\settings.json'",
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
		"$json = $cfg | ConvertTo-Json -Depth 20",
		"[IO.File]::WriteAllText($path, $json, (New-Object Text.UTF8Encoding($false)))",
		// 读回校验：既要能解析，也要能读出新值（写没写进去必须当场知道）。
		"$check = [IO.File]::ReadAllText($path) | ConvertFrom-Json",
		"if ($check.env.ANTHROPIC_BASE_URL -ne " + psQuote(p.Origin) + ") { throw '写入后校验失败：base_url 不匹配' }",
		"Write-Host ('已写入 ' + $path)",
		"Write-Host ('  ANTHROPIC_BASE_URL = ' + $check.env.ANTHROPIC_BASE_URL)",
		"Write-Host '接入完成：重启 Claude Code 生效。若 CC Switch 处于 routing 模式，请在它内部再选一次供应商。'",
		"exit 0",
	}, "\n")
}

// buildShellOnboard macOS / Linux 的等价实现（用 python3 处理 JSON，没有则明确报错）。
//
// 不用 sed/awk 改 JSON：JSON 不是行式格式，"看起来改对了"的 sed 在遇到
// 单行紧凑 JSON 或转义字符时会静默写坏文件 —— 而写坏的是用户的客户端配置。
func buildShellOnboard(p onboardPlan) string {
	entry := onboardWorkBuddyEntry(p)
	entryJSON, _ := json.Marshal(entry)
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
    with open(path, "w", encoding="utf-8", newline="\n") as f:
        json.dump(cfg, f, ensure_ascii=False, indent=2)
        f.write("\n")
    check = json.load(open(path, encoding="utf-8"))
    got = (check.get("env") or {}).get("ANTHROPIC_BASE_URL")
    if got != os.environ["WB2API_BASE_URL"]:
        sys.exit("写入后校验失败：base_url 不匹配")
    print("已写入 " + path)
    print("  ANTHROPIC_BASE_URL = " + got)
else:
    entry = json.loads(os.environ["WB2API_ENTRY"])
    lst = []
    if isinstance(data, list):
        lst = data
    elif isinstance(data, dict):
        lst = [data]
    lst = [x for x in lst if not (isinstance(x, dict) and x.get("id") == entry["id"])]
    lst.append(entry)
    with open(path, "w", encoding="utf-8", newline="\n") as f:
        json.dump(lst, f, ensure_ascii=False, indent=2)
        f.write("\n")
    check = json.load(open(path, encoding="utf-8"))
    print("已写入 " + path + "（当前共 %d 个模型）" % len(check))
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
	// 客户端平台：不能只看服务端（服务端在 Linux 上跑，用户在 Windows 上用它）。
	// 前端知道自己跑在哪，所以让它传；没传时按服务端平台给一个能跑的方向。
	goos := strings.ToLower(strings.TrimSpace(req.OS))
	if goos != "windows" && goos != "unix" {
		goos = "windows" // 绝大多数用户在 Windows；脚本内容会明示怎么改成 sh
	}
	filename, script := buildOnboardScript(plan, goos)

	resp := map[string]any{
		"target": target, "model": model, "base_url": plan.BaseURL,
		"filename": filename, "script": script,
		"key_masked": maskSecret(plain),
		"manual":     onboardManual(plan),
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
		entry := onboardWorkBuddyEntry(p)
		blob, _ := json.MarshalIndent(entry, "", "  ")
		out["path"] = `%USERPROFILE%\.workbuddy\models.json` +
			`（macOS/Linux: ~/.workbuddy/models.json）`
		out["note"] = "顶层是数组：把这个对象追加进数组；已有同 id 的条目时**替换它**（不要留两条同名）。"
		out["json"] = string(blob)
	case onboardTargetCCSwitch:
		// CC Switch 自己有一张 providers 表（SQLite，第三方 SSOT），直接改它有风险：
		// schema 会变、它自己也会写。这里给的是 Claude Code 认的 settings.json 形态，
		// 也就是 CC Switch 切到 direct 模式时用的那种配置。
		blob, _ := json.MarshalIndent(map[string]any{
			"env": map[string]any{
				"ANTHROPIC_BASE_URL":   p.Origin,
				"ANTHROPIC_AUTH_TOKEN": p.APIKey,
			},
		}, "", "  ")
		out["path"] = `%USERPROFILE%\.claude\settings.json（macOS/Linux: ~/.claude/settings.json）`
		out["note"] = "把 env 这两个键合并进现有 settings.json（ANTHROPIC_BASE_URL 填**不带 /v1** 的地址：" +
			"客户端自己拼 /v1/messages）。若 CC Switch 处于 routing 模式，需在它内部再选一次供应商 —— " +
			"否则它会在下次切换时覆盖这里。"
		out["json"] = string(blob)
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
