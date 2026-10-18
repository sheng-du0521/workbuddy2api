// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// authscan.go 在本机探测「客户端登录态文件」（v0.31.42）。
//
// ## 为什么由 agent 来扫
//
// 号主本机客户端与我们共用同一条 refresh 轮换链 —— 谁先刷新谁把对方打掉。
// 彻底消除这个冲突只有一条路：**让本机成为唯一刷新者，服务器只读**。
// 而服务器看不到对方机器上的文件，只能让跑在那台机器上的 agent 去找。
//
// ## 边界（这条比功能本身更重要）
//
// 本文件**只产出元信息**：路径、修改时间、大小、账号 uid 前 8 位、结论。
// **绝不把 access/refresh token 放进上报内容**。理由是：
//   - 扫描是"自动、周期、无人确认"的动作 —— 自动上传凭证等于把用户机器上的
//     所有登录态默默搬进云库，即使那台机器是本人的，也不该在没被问过时发生；
//   - 真正的"跟随"必须是运维看过列表后**显式选一个文件**，那时才需要读令牌。
//
// 所以这里的职责只有一件事：把"这台机器上疑似有哪些登录态"说清楚，
// 让人能做出选择。
package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// authScanMaxFiles 最多上报几个候选（再多对"选一个"没有帮助，只是噪音）。
	authScanMaxFiles = 8
	// authScanMaxSize 超过这个大小的文件不读（正常登录态是几百字节到几 KB）。
	authScanMaxSize = 256 << 10
	// authScanMaxDepth 目录递归深度上限（客户端状态目录通常不超过 3 层）。
	authScanMaxDepth = 3
)

// authFileCandidate 扫描结果（与云端 agentAuthCandidate 同构，字段名一致）。
type authFileCandidate struct {
	Path      string `json:"path"`
	MTime     int64  `json:"mtime"`
	Size      int64  `json:"size"`
	UIDPrefix string `json:"uid_prefix,omitempty"`
	Nickname  string `json:"nickname,omitempty"`
	Note      string `json:"note,omitempty"`
}

// authScanRoots 常见登录态目录（存在才扫）。
//
// 为什么用"枚举 + 存在性过滤"而不是猜单一路径：这类客户端在不同版本里把状态放到
// 不同位置（AppData\Roaming / Local、.xxx 家目录），而我们**不能只猜一个** ——
// 猜错的结果是"探测不到"，让人以为这个方案不可用。
func authScanRoots() []string {
	var roots []string
	if dir, err := os.UserConfigDir(); err == nil { // Windows: %APPDATA%
		for _, n := range []string{"WorkBuddy", "CodeBuddy", "workbuddy", "codebuddy"} {
			roots = append(roots, filepath.Join(dir, n))
		}
	}
	if dir, err := os.UserCacheDir(); err == nil { // Windows: %LOCALAPPDATA%
		for _, n := range []string{"WorkBuddy", "CodeBuddy", "workbuddy", "codebuddy"} {
			roots = append(roots, filepath.Join(dir, n))
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, n := range []string{".workbuddy", ".codebuddy", ".config/workbuddy", ".config/codebuddy"} {
			roots = append(roots, filepath.Join(home, n))
		}
	}
	// agent 自己的目录也扫一遍：有些部署会把 auth 文件放在这里由 agent 代管。
	if exe, err := os.Executable(); err == nil {
		roots = append(roots, filepath.Dir(exe))
	}
	out := roots[:0]
	for _, r := range roots {
		if st, err := os.Stat(r); err == nil && st.IsDir() {
			out = append(out, r)
		}
	}
	return out
}

// scanAuthFiles 扫一遍候选目录，返回"像登录态"的文件元信息。
//
// 判定"像登录态"的口径（宽松但在最后一层收紧）：
//  1. 文件名或内容里出现 access_token/accessToken 与 refresh_token/refreshToken；
//  2. 能解析出 JSON 对象（不要求结构完全固定 —— 各版本外层键不同）。
//
// 刻意**不**用文件名硬匹配（`auth.json`）：版本之间会改名，而"含两种 token 的 JSON"
// 是这类文件的本质特征。
func scanAuthFiles() []authFileCandidate {
	var out []authFileCandidate
	seen := map[string]bool{}
	for _, root := range authScanRoots() {
		walkAuthRoot(root, 0, &out, seen)
		if len(out) >= authScanMaxFiles {
			break
		}
	}
	// 按修改时间倒序：最近改过的最可能是"当前在用"的那份。
	sort.SliceStable(out, func(i, j int) bool { return out[i].MTime > out[j].MTime })
	if len(out) > authScanMaxFiles {
		out = out[:authScanMaxFiles]
	}
	return out
}

// walkAuthRoot 递归扫描（深度受限）。
func walkAuthRoot(dir string, depth int, out *[]authFileCandidate, seen map[string]bool) {
	if depth > authScanMaxDepth || len(*out) >= authScanMaxFiles {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if len(*out) >= authScanMaxFiles {
			return
		}
		full := filepath.Join(dir, e.Name())
		if e.IsDir() {
			// 跳过明显无关节省时间（缓存/日志/插件目录里不会有登录态）。
			low := strings.ToLower(e.Name())
			if low == "cache" || low == "logs" || low == "node_modules" || low == ".git" {
				continue
			}
			walkAuthRoot(full, depth+1, out, seen)
			continue
		}
		if !strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
			continue
		}
		real, err := filepath.Abs(full)
		if err != nil || seen[real] {
			continue
		}
		info, err := e.Info()
		if err != nil || info.Size() > authScanMaxSize {
			continue
		}
		if c, ok := inspectAuthFile(real, info); ok {
			seen[real] = true
			*out = append(*out, c)
		}
	}
}

// inspectAuthFile 读一个文件并判断它是不是登录态；是则返回元信息。
func inspectAuthFile(path string, info os.FileInfo) (authFileCandidate, bool) {
	f, err := os.Open(path)
	if err != nil {
		return authFileCandidate{}, false
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, authScanMaxSize))
	if err != nil {
		return authFileCandidate{}, false
	}
	uid, nick, ok := parseAuthMeta(raw)
	if !ok {
		return authFileCandidate{}, false
	}
	return authFileCandidate{
		Path:      path,
		MTime:     info.ModTime().Unix(),
		Size:      info.Size(),
		UIDPrefix: prefix8(uid),
		Nickname:  nick,
		Note:      "ok",
	}, true
}

// parseAuthMeta 从 JSON 里取 uid / nickname，并判定"是否同时含两种 token"。
//
// 结构各版本不同（插件 OAuth 是 {auth:{...},account:{...}}，手写的是扁平），
// 所以按出现位置宽松找：优先 account.uid，其次顶层 uid。
func parseAuthMeta(raw []byte) (uid, nickname string, ok bool) {
	low := strings.ToLower(string(raw))
	// 必须匹配**带引号的键名**，不能用裸子串。
	//
	// 反例（真实存在）：`{"access_token_hint":1}` —— 裸子串匹配会把它当成登录态，
	// 于是配置/缓存的 JSON 混进候选列表，"选一个"这件事就失去意义了。
	hasKey := func(keys ...string) bool {
		for _, k := range keys {
			if strings.Contains(low, `"`+k+`"`) {
				return true
			}
		}
		return false
	}
	if !(hasKey("access_token", "accesstoken") && hasKey("refresh_token", "refreshtoken")) {
		return "", "", false
	}
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		return "", "", false
	}
	if acc, ok2 := doc["account"].(map[string]any); ok2 {
		uid = strAny(acc["uid"])
		nickname = strAny(acc["nickname"])
	}
	if uid == "" {
		uid = strAny(doc["uid"])
	}
	if nickname == "" {
		nickname = strAny(doc["nickname"])
	}
	// uid 为空也算候选：有些版本只存令牌，uid 由服务端在刷新时返回。
	return uid, nickname, true
}

// strAny 取 JSON 值的字符串形态（只处理字符串；数字/map 一律返回空）。
//
// 刻意不用 fmt.Sprint 兜底：JSON 里的数字会被格式化成 float（`123` → `123`），
// 而 uid 这类标识混进浮点表示后与云端比对会莫名不等 —— 宁可空着。
func strAny(v any) string {
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

// prefix8 取 uid 前 8 位（不足则原样）。
func prefix8(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8]
}
