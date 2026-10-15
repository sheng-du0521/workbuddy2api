// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
)

// ======================================================================
// 账号导入（v0.12.0 桌面端文件 / v0.15.3 通用 JSON 批量导入）
//
// 桌面版 2api（WorkBuddy-Switch2api）把设备授权登录过的账号落在
// %USERPROFILE%\.wb-switch\accounts.json（一个数组）。Web 版账号存在
// SQLite（accounts 表），两边本来不通。
//
// 两条入口，**共用同一套映射与校验**：
//   - 服务器本地路径（原 v0.12.0）：读服务端文件系统上的 accounts.json；
//   - JSON 正文（v0.15.3）：前端粘贴 / 上传 JSON 内容，**不依赖服务端文件**。
//     后者是主入口 —— 服务端常跑在远端（公网隧道），本地路径根本读不到。
//
// 无论哪条入口都做同样的事：
//   - 逐条映射为平台自持账号（owner_user_id=0 → 恒参与平台反代）；
//   - 按 uid 去重 upsert（UpsertAccount 刻意不覆盖业务状态列，
//     所以账号即使已在池中也不会被重登抹掉分组/停用状态）。
//
// 路径优先级：请求 json > 请求 ?path= > kv(desktop_accounts_path) > 默认
// %USERPROFILE%\.wb-switch\accounts.json。
// ======================================================================

// kvDesktopAccountsPath 桌面端账号文件路径的覆盖配置（空 = 用默认路径）。
const kvDesktopAccountsPath = "desktop_accounts_path"

// defaultDesktopAccountsPath 默认读取 %USERPROFILE%\.wb-switch\accounts.json。
func defaultDesktopAccountsPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".wb-switch", "accounts.json")
}

// desktopAccountsPath 解析本次导入要读取的文件路径。
func (a *app) desktopAccountsPath(override string) string {
	if p := strings.TrimSpace(override); p != "" {
		return p
	}
	if p, err := a.store.GetKV(kvDesktopAccountsPath); err == nil && strings.TrimSpace(p) != "" {
		return strings.TrimSpace(p)
	}
	return defaultDesktopAccountsPath()
}

// importRootList 汇总导入白名单目录：默认账号目录 + `-import-roots` 追加项（v0.24.2，P3-8）。
//
// 「追加」而不是「覆盖」：默认目录是账号文件的规范位置，永远该可用；
// 需要额外目录（如运维把账号文件放在 D:\imports）时再显式加。
func importRootList(extra, defaultPath string) []string {
	var out []string
	if d := strings.TrimSpace(filepath.Dir(defaultPath)); d != "" && d != "." {
		out = append(out, filepath.Clean(d))
	}
	for _, p := range strings.Split(extra, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		out = append(out, filepath.Clean(p))
	}
	return out
}

// withinDir 判定 path 是否落在 dir **之内**（不含 dir 自身）。
//
// Windows 上先转小写再比：磁盘路径大小写不敏感，而 filepath.Rel 在 Windows 的
// 分量比较是区分大小写的 —— 不归一化就会出现「白名单写 c:\Users，解析出来是
// C:\Users」导致的**误拒**。（大小写差异只会造成误拒、不会造成误放，
// 但误拒同样不可接受：功能坏掉和安全没修一样糟。）
func withinDir(path, dir string) bool {
	if runtime.GOOS == "windows" {
		path, dir = strings.ToLower(path), strings.ToLower(dir)
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	if rel == "." || rel == ".." || filepath.IsAbs(rel) {
		return false
	}
	return !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// canonPath 归一化路径：绝对化 + 尽量穿透符号链接。
//
// 为什么必须做（v0.24.2 真机发现）：同一个目录可以有多个**等价写法**——
// 实测就遇到过：仓库根同时存在「盘符路径」与「junction / 符号链接路径」两种写法，
// 于是「应用自己算出的数据目录」可能是 A 路，而请求里写的是 B 路。
// 只做前缀比较就会得出「两者无关」的结论，守卫被别名绕过（实测到过：
// data\secret.key 只被白名单兜住，没被更早的专用守卫拦下）。
//
// 解析顺序（v0.24.2 由测试逼出来的正确顺序，很关键）：
//  1. 先整体 EvalSymlinks —— 它会连**最后一段**一起解析，
//     于是「白名单目录里放一个指向机密的软链」这种逃逸才拦得住；
//  2. 整体解析失败（文件不存在）时，退化为「解析父目录 + 拼回文件名」——
//     否则非存在性路径永远归一化不了，第一次导入（账号文件尚未落盘）就会误报。
//
// 反过来写（先父目录再整体）是错的：父目录一旦解析成功就直接返回，
// 文件自身的软链从此再没机会被解开 —— 逃逸检测静默失效。
func canonPath(p string) string {
	if strings.TrimSpace(p) == "" {
		return ""
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(real)
	}
	dir, base := filepath.Split(abs)
	if real, err := filepath.EvalSymlinks(filepath.Clean(dir)); err == nil {
		return filepath.Clean(filepath.Join(real, base))
	}
	return filepath.Clean(abs)
}

// sameDirAs 两个路径是否指向**同一个目录** —— 用文件标识比较，与写法无关。
func sameDirAs(p, dir string) bool {
	fp, err1 := os.Stat(p)
	fd, err2 := os.Stat(dir)
	return err1 == nil && err2 == nil && os.SameFile(fp, fd)
}

// pathInsideDir path 是否位于 dir 之内（不含 dir 自身）。
//
// 判据**多重**，因为 Windows 上「同一个目录」可以有多种等价写法（盘符别名、junction、
// 大小写差异），而 `filepath.EvalSymlinks` 对 **junction（目录挂载点）并不保证解得开**。
// 实测本机工作区正是这种情况：EvalSymlinks 直接报
// 「not a reparse point / 系统找不到指定的路径」，于是纯路径前缀比较会得出
// 「两者无关」的错误结论 —— 守卫看起来在、实际已被静默绕过（v0.24.2 真机验证抓到）。
//
//	① 文件标识（os.SameFile）：从 path 的父目录逐级向上与 dir 比较。
//	   跨别名成立，最可靠；这是主判据。
//	② 归一化路径前缀：解得出软链时成立，也是 ① 不可用（如目录尚不存在）时的兜底。
func pathInsideDir(path, dir string) bool {
	if strings.TrimSpace(dir) == "" {
		return false
	}
	// ① 文件标识
	p := filepath.Dir(filepath.Clean(path))
	for i := 0; i < 64; i++ {
		if p == "" || p == filepath.Dir(p) {
			break
		}
		if sameDirAs(p, dir) {
			return true
		}
		p = filepath.Dir(p)
	}
	// ② 路径前缀（软链逃逸主要靠这一条：父目录不在 dir 里，但文件自身是指向 dir 的软链）
	return withinDir(canonPath(path), canonPath(dir))
}

// resolveImportPath 校验并归一化「服务端路径导入」要读的文件（v0.24.2，P3-8）。
//
// **背景**：此前 path 参数（请求体 → kv 配置 → 默认值）被**原样**交给 os.ReadFile。
// 这条接口需要管理员权限，但管理员会话一旦失窃（存储型 XSS、共用设备、日志里漏出的
// 会话 Cookie），攻击者就能读走同机的**任意文件**——其中 `data/secret.key` 是
// 用户 API Key 与号池账号 token 的**加密主密钥**：读走一次即可离线永久解密全部凭证。
// 「需要管理员」并不等于「可以读整个文件系统」，这两件事本来就该分开。
//
// **判据**（全部满足才放行）：
//  1. 路径非空；
//  2. 归一化为绝对路径，且**穿透符号链接**后仍落在白名单目录内（防 symlink 逃逸：
//     白名单目录里放一个指向 C:\Users\...\secret.key 的软链即可绕过纯前缀比较）；
//  3. 不在数据目录内 —— 即便 `-import-roots` 被配成某个父目录（如 D:\），
//     secret.key / admin.key / webapp.db 也**永远**读不到。
//
// 返回归一化后的绝对路径：调用方直接用它读文件，避免「先校验再用原始路径」的 TOCTOU 窗口。
func (a *app) resolveImportPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", errors.New("路径为空")
	}
	abs := canonPath(p)
	if abs == "" {
		return "", errors.New("路径无法解析为绝对路径")
	}

	// 数据目录守卫：先判它，错误文案更有指向性（直接说清「这里有什么」）。
	dataRoot := canonPath(a.dataDir)
	if pathInsideDir(abs, a.dataDir) {
		return "", errors.New("拒绝读取数据目录内的文件（该目录含主密钥、数据库与管理密钥）")
	}
	for _, r := range a.importRoots {
		if pathInsideDir(abs, r) {
			return abs, nil
		}
	}
	// 报错里带上**实际计算出的**几个位置：运维看到「白名单是空的」或
	// 「数据目录和我以为的不一样」时，第一反应是怀疑配置 —— 直接把值给出来，
	// 省掉一轮「改配置 → 重启 → 还是不通」的盲试。归一化后的路径尤其重要：
	// 同一个目录可以有多个等价写法，用户看到的「我明明填对了」往往就出在这里。
	return "", fmt.Errorf("路径不在允许导入的目录内（解析为：%s；白名单：%s；数据目录：%s；可用 -import-roots 追加）",
		abs, strings.Join(a.importRoots, "、"), dataRoot)
}

// desktopAccount 桌面端 accounts.json 里的一条记录（只声明我们需要用到的字段）。
// email / enterpriseId / enterpriseName 在源文件里可能是 JSON null，故用 any 兜底。
type desktopAccount struct {
	UID           string          `json:"uid"`
	Nickname      string          `json:"nickname"`
	Domain        string          `json:"domain"`
	Edition       string          `json:"edition"`
	Group         string          `json:"group"`
	AccessToken   string          `json:"access_token"`
	RefreshToken  string          `json:"refresh_token"`
	ExpiresAt     int64           `json:"expiresAt"` // 毫秒
	EnterpriseID  any             `json:"enterpriseId"`
	AuthRaw       json.RawMessage `json:"auth_raw"`    // 可能是内联对象，也可能是字符串化的 JSON
	ProfileRaw    json.RawMessage `json:"profile_raw"` // 同上
	TokenType     string          `json:"token_type"`
	CreatedAtMs   int64           `json:"createdAt"`
	RefreshedAtMs int64           `json:"refreshedAt"`
	// DeviceToken 设备风控 token（v0.25.1）。
	//
	// 此前这里**没有这个字段**，于是桌面端导出文件里的 device_token 被静默丢弃 ——
	// 这正是「全池 0/27 非空」的直接原因（另见 devicecred.go 的说明）。
	// 桌面端把它写在顶层（扁平形）与 auth_raw 内（嵌套形）两种情况都存在，
	// 故下面两处都尝试取。
	DeviceToken string `json:"device_token"`
}

// unmarshalFlexible 兼容「内联 JSON 对象」与「字符串化的 JSON」两种形态。
// 桌面端早期版本把 auth_raw / profile_raw 存为字符串，后来改存对象；
// 迁移时必须两种都能吃，否则真实文件会报
// "cannot unmarshal object into Go struct field ... of type string"。
func unmarshalFlexible(raw json.RawMessage, out any) bool {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return false
	}
	if s[0] == '"' { // 字符串：先解出字符串内容，再二次解析
		var inner string
		if json.Unmarshal(raw, &inner) != nil {
			return false
		}
		inner = strings.TrimSpace(inner)
		if inner == "" || inner == "null" {
			return false
		}
		return json.Unmarshal([]byte(inner), out) == nil
	}
	return json.Unmarshal(raw, out) == nil
}

// anyToString 把可能是 null / 数字 / 字符串的字段安全转成字符串。
func anyToString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		if t == "null" || t == "None" {
			return ""
		}
		return t
	case float64:
		return fmt.Sprintf("%.0f", t)
	case json.Number:
		return t.String()
	default:
		return fmt.Sprintf("%v", t)
	}
}

// desktopRealm 由 edition / domain 推断 realm（cn / global）。
// edition: domestic → cn，international → global；缺失时回落到 domain 推断。
func desktopRealm(edition, domain string) string {
	switch strings.ToLower(strings.TrimSpace(edition)) {
	case "domestic":
		return "cn"
	case "international":
		return "global"
	}
	return auth.ResolveRealm("", domain)
}

// desktopGroup 把桌面端分组名映射为 Web 版分组名（保留原有语义）。
// 桌面端常见取值：proxy（反代）/ desktop（桌面端）/ 空（未分组）。
func desktopGroup(g string) string {
	switch strings.ToLower(strings.TrimSpace(g)) {
	case "proxy":
		return "反代"
	case "desktop":
		return "桌面端"
	case "":
		return ""
	default:
		return g
	}
}

// normalizeExpiresSec 把 expiresAt 归一成 **Unix 秒**，同时容忍"秒"与"毫秒"两种写法。
//
// 为什么必须容忍（v0.31.4，合并两个产品的迁移通道发现的真陷阱）：
// 桌面端导出文件写的是**毫秒**（`createdAt`/`refreshedAt` 也在同一量纲），而旧网关的
// `auths/*.json` 写的是**秒**（`auth.ExpiresAt // Unix 秒`）。两者的键名完全一样，
// 于是"把 auths 目录直接导进 webapp"这条迁移路径会把秒当毫秒 → 除 1000 后得到 1970 年
// → 每个账号一落库就"已过期"，被立刻刷新 —— 一个百号部署会在几秒内打出一百次 refresh。
//
// 判据是量纲而不是年份：秒（~1.8e9）与毫秒（~1.8e12）相差三个数量级，
// 用 1e11 作分界在未来数千年内都不会误判。
func normalizeExpiresSec(v int64) int64 {
	const msCutoff = 1e11 // 秒 → 毫秒分界：秒值约 1.8e9，毫秒值约 1.8e12
	if v <= 0 {
		return 0
	}
	if v < msCutoff {
		return v // 已经是秒（auths/*.json、手写扁平文件）
	}
	return v / 1000 // 毫秒（桌面端导出）
}

// toAccount 把一条桌面端记录映射为库内 Account。
// 优先用 auth_raw（含最新 accessToken/refreshToken/domain），缺失时回落到顶层字段。
func (d desktopAccount) toAccount() *Account {
	access, refresh, domain := d.AccessToken, d.RefreshToken, d.Domain
	expiresAt := normalizeExpiresSec(d.ExpiresAt)
	// 设备凭证：顶层 device_token 优先；空的再从 auth_raw 里捞（v0.25.1）。
	deviceToken := d.DeviceToken
	if len(d.AuthRaw) > 0 {
		var ar struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			Domain       string `json:"domain"`
			ExpiresIn    int64  `json:"expiresIn"`
			// 嵌套形把设备凭证放在 auth 里（桌面端不同版本写法不一）。
			DeviceToken string `json:"deviceToken"`
		}
		if unmarshalFlexible(d.AuthRaw, &ar) {
			if ar.AccessToken != "" {
				access = ar.AccessToken
			}
			if ar.RefreshToken != "" {
				refresh = ar.RefreshToken
			}
			if ar.Domain != "" {
				domain = ar.Domain
			}
			if deviceToken == "" {
				deviceToken = ar.DeviceToken
			}
			// 顶层 expiresAt 缺失时用 expiresIn 兜底推算。
			if expiresAt == 0 && ar.ExpiresIn > 0 {
				expiresAt = time.Now().Add(time.Duration(ar.ExpiresIn) * time.Second).Unix()
			}
		}
	}
	nickname := d.Nickname
	uid := d.UID
	if len(d.ProfileRaw) > 0 {
		var pr struct {
			Nickname string `json:"nickname"`
			UID      string `json:"uid"`
		}
		if unmarshalFlexible(d.ProfileRaw, &pr) {
			if pr.Nickname != "" {
				nickname = pr.Nickname
			}
			if pr.UID != "" {
				uid = pr.UID
			}
		}
	}
	return &Account{
		UID:          uid,
		Nickname:     nickname,
		Realm:        desktopRealm(d.Edition, domain),
		EnterpriseID: anyToString(d.EnterpriseID),
		Domain:       domain,
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresAt:    expiresAt,
		// 设备凭证（v0.25.1）：不漏掉它才有 X-Device-Token 可用（见 devicecred.go）。
		DeviceToken: deviceToken,
		Group:       desktopGroup(d.Group),
		// 迁移进来的账号归平台自持（owner=0 → 恒参与平台反代），
		// 这样「把桌面端已授权账号全部迁到管理后台」后立即可被号池使用。
		OwnerUserID: 0,
		PoolState:   "",
	}
}

// desktopImportResult 导入结果（供前端提示）。
type desktopImportResult struct {
	Source    string   `json:"source"` // "json"（粘贴/上传）或 "file"（服务端路径）
	Path      string   `json:"path,omitempty"`
	Total     int      `json:"total"`
	Imported  int      `json:"imported"`
	Updated   int      `json:"updated"`
	Skipped   int      `json:"skipped"`
	Nicknames []string `json:"nicknames"`
	Errors    []string `json:"errors,omitempty"`
	// UIDs 成功写入的账号 uid（新增 + 更新）。不对外输出，仅供导入后
	// 触发一次余额拉取（见 creditsync.go）。
	UIDs []string `json:"-"`
}

// parseDesktopAccounts 解析导入正文，兼容四种常见形态（顺序即优先级）：
//
//  1. 数组：[{...}, {...}]                    ← 桌面端 accounts.json 原生形态
//  2. 带包装：{"accounts": [...]} / {"data": [...]} / {"list": [...]} / {"items": [...]}
//  3. 单对象：{...}                            ← 手动从卡片/日志里拷一条
//  4. 空数组                                    ← 合法但无内容（返回 0 条，不算错误）
//
// 另处理 UTF-8 BOM：Windows 记事本另存为 UTF-8 会带 BOM，直接 json.Unmarshal 会报
// "invalid character 'ï'"。粘贴场景很容易遇到，故在入口统一剥离。
func parseDesktopAccounts(raw []byte) ([]desktopAccount, error) {
	body := bytes.TrimSpace(bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF}))
	if len(body) == 0 {
		return nil, fmt.Errorf("内容为空")
	}
	switch body[0] {
	case '[':
		var list []desktopAccount
		if err := json.Unmarshal(body, &list); err != nil {
			return nil, fmt.Errorf("解析 JSON 数组失败：%w", err)
		}
		return list, nil
	case '{':
		// 先试探包装形态：任取一个已知键是数组就按数组解析。
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(body, &envelope); err != nil {
			return nil, fmt.Errorf("解析 JSON 对象失败：%w", err)
		}
		for _, key := range []string{"accounts", "data", "list", "items"} {
			v, ok := envelope[key]
			if !ok {
				continue
			}
			if t := bytes.TrimSpace(v); len(t) > 0 && t[0] == '[' {
				var list []desktopAccount
				if err := json.Unmarshal(t, &list); err != nil {
					return nil, fmt.Errorf("解析 %q 数组失败：%w", key, err)
				}
				return list, nil
			}
		}
		// 单对象形态：包成单元素数组。
		var one desktopAccount
		if err := json.Unmarshal(body, &one); err != nil {
			return nil, fmt.Errorf("解析 JSON 对象失败：%w", err)
		}
		return []desktopAccount{one}, nil
	default:
		return nil, fmt.Errorf("无法识别的内容格式（应以 JSON 数组 [ 或对象 { 开头）")
	}
}

// importDesktopAccounts 逐条映射并 upsert；source 仅用于结果标注来源。
func (a *app) importDesktopAccounts(list []desktopAccount, source, path string) *desktopImportResult {
	existing := map[string]bool{}
	if all, err := a.store.ListAccounts(); err == nil {
		for _, acc := range all {
			existing[acc.UID] = true
		}
	}
	res := &desktopImportResult{Source: source, Path: path, Total: len(list)}
	for i, d := range list {
		acc := d.toAccount()
		// 校验：uid 必填；凭证「access_token 或 refresh_token 至少有一个」即可 ——
		// 只有 refresh_token 时，出站前的 refreshIfNeeded 会先换出 access token
		//（NeedsRefresh 在 expiresAt<=0 时恒 true），所以不算残缺。
		// 旧版要求 access_token 必填，会把这类只带 refresh 的导出整条丢掉。
		if strings.TrimSpace(acc.UID) == "" {
			res.Skipped++
			res.Errors = append(res.Errors, fmt.Sprintf("第 %d 条缺少 uid，已跳过", i+1))
			continue
		}
		if strings.TrimSpace(acc.AccessToken) == "" && strings.TrimSpace(acc.RefreshToken) == "" {
			res.Skipped++
			res.Errors = append(res.Errors, fmt.Sprintf("%s 缺少 access_token 与 refresh_token，已跳过", acc.UID))
			continue
		}
		if err := a.store.UpsertAccount(acc); err != nil {
			res.Skipped++
			res.Errors = append(res.Errors, fmt.Sprintf("%s 写入失败：%v", acc.UID, err))
			continue
		}
		if existing[acc.UID] {
			res.Updated++
		} else {
			res.Imported++
		}
		res.UIDs = append(res.UIDs, acc.UID)
		name := acc.Nickname
		if name == "" {
			name = acc.UID
		}
		res.Nicknames = append(res.Nicknames, name)
	}
	return res
}

// importDesktopJSON 从 JSON 正文导入（前端粘贴 / 上传）。
func (a *app) importDesktopJSON(raw []byte) (*desktopImportResult, error) {
	list, err := parseDesktopAccounts(raw)
	if err != nil {
		return nil, err
	}
	return a.importDesktopAccounts(list, "json", ""), nil
}

// importDesktopFile 读取并导入服务端本地账号文件。
//
// ⚠️ 调用方**必须**先用 resolveImportPath 校验路径（v0.24.2，P3-8）：
// 白名单是 HTTP 边界上的约束，本函数只负责「读 + 解析」。
// 直接拿用户输入调本函数 = 绕过白名单。
func (a *app) importDesktopFile(path string) (*desktopImportResult, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取账号文件失败：%w", err)
	}
	list, err := parseDesktopAccounts(raw)
	if err != nil {
		return nil, fmt.Errorf("%s：%w", filepath.Base(path), err)
	}
	return a.importDesktopAccounts(list, "file", path), nil
}

// handleImportDesktopAccounts 管理侧：批量导入账号进号池。
// POST /api/admin/accounts/import-desktop
// body: {"json": "<JSON 正文>"} —— 前端粘贴/上传（主入口，不依赖服务端文件）
//
//	{"path": "<服务端路径>"} —— 读服务端文件（可选；缺省用 ~/.wb-switch/accounts.json）
//	{}                        —— 等价于 {"path": 默认路径}
//	{"force": true}           —— 应急放行：跳过防关联门禁（v0.25.0，会写 WARN 日志）
//
// v0.25.0 起本接口串了**防关联门禁**，顺序刻意是「解析 → 门禁 → 写库」：
//
//	必须先解析出待导入列表才能算「本次新增几个」，而门禁又必须在**任何写库之前**判定
//	（否则被拒时已经写进去一半，门禁就成了摆设）。见 import_guard.go。
func (a *app) handleImportDesktopAccounts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		JSON  string `json:"json"`
		Path  string `json:"path"`
		Force bool   `json:"force"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	// —— ① 解析（只解析，不写库）——
	var list []desktopAccount
	var source, path string
	if strings.TrimSpace(req.JSON) != "" {
		// JSON 正文优先：不碰文件系统，远端部署也能用。
		l, err := parseDesktopAccounts([]byte(req.JSON))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		list, source = l, "json"
	} else {
		p := a.desktopAccountsPath(req.Path)
		if p == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "无法定位账号文件，请粘贴 JSON 内容或显式提供 path"})
			return
		}
		// 白名单校验（v0.24.2，P3-8）：拒绝读数据目录 / 白名单目录之外的文件。
		safe, perr := a.resolveImportPath(p)
		if perr != nil {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "拒绝读取该路径：" + perr.Error()})
			return
		}
		raw, err := os.ReadFile(safe)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("读取账号文件失败：%v", err)})
			return
		}
		l, err := parseDesktopAccounts(raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("%s：%v", filepath.Base(safe), err)})
			return
		}
		list, source, path = l, "file", safe
	}
	if len(list) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "内容里没有任何账号记录（解析到 0 条）"})
		return
	}

	// —— ② 防关联门禁（v0.25.0）——
	guard := a.checkImportGuard(list, false)
	// 配了换 IP 命令 → 就地执行并等 IP 真的变（只有在"其它维度都通过"时才值得换：
	// 如果本来就是因为超量被拒，换 IP 也救不回来）。
	if guard.OK && guard.NeedIP && guard.AutoReset {
		after, changed := a.resetExitIPIfNeeded(guard.ExitIP)
		guard.ExitIP, guard.IPChanged = after, changed
		if changed {
			guard.NeedIP = false
			guard.Hint = "已自动换 IP：" + ipOrDash(after) + "，可以导入。"
		} else {
			guard.OK = false
			guard.Reason = "自动换 IP 未生效（出口仍为 " + ipOrDash(after) + "）"
			guard.Hint = "命令已执行但没有等到 IP 变化。请检查命令是否正确，或改用手动模式（清空 ip_reset_cmd）。"
		}
	}
	if !guard.OK {
		if !req.Force {
			// 拒绝时把 guard 一并回传：前端要拿它显示「当前 IP / 上次 IP / 怎么换」，
			// 只给一句 error 文案会让运维不知道该干什么（这类"提示指向不存在的入口"的
			// 缺陷本项目已经修过一次，见 v0.23.1）。
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error":               guard.Reason,
				"guard":               guard,
				"import_guard_failed": true,
			})
			return
		}
		// 应急放行：**必须留痕**。这是唯一一条能绕过防关联门禁的路径，
		// 事后审计时得能看出"这批号是强行导进去的"。
		log.Printf("WARN: [import] 管理员强制放行，已跳过防关联门禁：%s（新增 %d 个）", guard.Reason, guard.NewCount)
	}

	// —— ③ 写库 ——
	res := a.importDesktopAccounts(list, source, path)
	if res.Total == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "内容里没有任何账号记录（解析到 0 条）"})
		return
	}
	// 导入即入池：重算池内账号，使新账号立刻参与选号。
	if err := a.proxy.SyncAccounts(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// 台账 + 门禁判据更新（v0.25.0）：记下"这一批是从哪个 IP 进来的"。
	a.recordImportBatch(source, guard.ExitIP, res.Imported, res.Updated, res.UIDs)
	// 新号余额拉取（v0.17.8）：池内 credits 是内存快照，新号的快照恒为 0，
	// 不拉一次就会让仪表盘「积分合计」看起来没变化（而账号卡上按需查询是有数字的）。
	// 后台异步跑：导入接口不能等 N 个账号逐个出站。
	a.refreshCreditsAsync(a.importedUIDs(res))
	writeJSON(w, http.StatusOK, map[string]any{
		"source":    res.Source,
		"path":      res.Path,
		"total":     res.Total,
		"imported":  res.Imported,
		"updated":   res.Updated,
		"skipped":   res.Skipped,
		"nicknames": res.Nicknames,
		"errors":    res.Errors,
		// 让前端能明说「余额正在后台拉取」，而不是让管理员对着没变的数字猜。
		"credit_refresh": "started",
		// 本批的出口 IP 与门禁判定：让运维看到"这批号从哪进来的"，
		// 而不是导入完就再也查不到（台账也在 /api/admin/import/ip-status 里可查）。
		"guard":    guard,
		"batch_ip": guard.ExitIP,
	})
}

// importedUIDs 从导入结果里取出本次涉及的 uid（新增 + 更新都算）。
//
// 为什么更新过的也要刷：UpsertAccount 刻意不覆盖业务状态列，但 token 会被刷新
// ——token 变了意味着这个号的余额快照可能来自上一份凭证，重拉一次最稳。
func (a *app) importedUIDs(res *desktopImportResult) []string {
	if res == nil || len(res.UIDs) == 0 {
		return nil
	}
	if len(res.UIDs) > creditsRefreshMaxAccounts {
		return res.UIDs[:creditsRefreshMaxAccounts]
	}
	return res.UIDs
}
