// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ======================================================================
// 版本更新提醒（v0.26.0）
// ======================================================================
//
// 目标很窄：**只提醒，不自动更新。**
//
// 为什么刻意不做自动替换 exe：
//  1. Windows 上正在运行的 .exe 文件被锁定，无法原地覆盖。要做就得写
//     "改名 → 写新文件 → 重启后自清理" 这套把戏，而本项目的启动方式是
//     一个前台 bat + 一个后台进程，没有服务托管者来帮你重启。
//  2. 本机 `schtasks` 已被安全策略列入黑名单（实测：注册计划任务被拦），
//     没有可靠的"重启后完成替换"的时机。
//  3. 更新通道是供应链攻击最肥的目标 —— 一次绕过就能给所有用户装后门。
//     只做提醒的话，这个攻击面根本不存在。
//
// 所以这里的职责边界是：**发现新版本 → 在管理页显示出来 → 给出下载指引**，
// 其余交给运维手动执行。
//
// ## 数据源
//
// 一个静态清单 JSON（`update_manifest_url`，默认指向上游仓库 raw 的 version.json）。
// 选静态清单而不是 GitHub Releases API 的理由：
//   - 未认证的 Releases API 限 60 次/小时/IP，且该端点**不返回 prerelease**；
//   - 静态文件可以放 CDN，能带任意字段，而且**出问题时可一键撤下**
//     （把清单回退即可全量收回提示，不必发新版本）。
//
// ## 清单格式（version.json）
//
//	{
//	  "version": "0.26.0",            // 必填，SemVer
//	  "min_supported": "0.24.0",      // 可选：低于此版本的实例会被标记为需尽快升级
//	  "notes": "修了什么…",            // 可选：更新说明（纯文本，前端转义后展示）
//	  "released_at": 1791900000,      // 可选：Unix 秒
//	  "url": "https://github.com/.../releases/tag/v0.26.0",
//	  "assets": [                     // 可选：按平台给下载地址
//	    {"os":"windows","arch":"amd64","url":"https://…/webapp.exe","sha256":"…","size":14157584}
//	  ]
//	}
//
// ## kv 开关（设置页可改，也可直接改库）
//
//	update_check_enabled  "1"（默认）开启；"0" 关闭（内网/私有化部署应当关掉）
//	update_manifest_url   清单地址；空 = 未配置，界面提示去填
//	update_last_check     上次检查时间戳（程序自写）
//	update_last_result    上次成功拉到的清单 JSON（程序自写，兼作离线缓存）
//	update_skip_version   用户点了"跳过此版本"的版本号（程序自写）
//
// ## 设计约束（对应"边界情况"清单）
//
//   - **检查失败必须静默**：离线 / DNS 失败 / 代理拦截 / 超时 / 限流，一律不报错、
//     不影响任何主功能，只把 error 记在状态里给自己看。
//   - **不拖慢任何请求**：状态接口优先返回缓存；缓存新鲜时根本不出站。
//   - **用单调的相对间隔判断"该不该再查"**，不用绝对时间 —— 用户机器时钟不准时，
//     绝对时间比较会得出荒谬结论。
//   - **降级检测**：当前版本比清单更新（开发版/预发布用户）时**不提示**"更新"。
//   - **版本号畸形**：SemVer 解析失败时退化为"只判是否相等"，绝不因此报错。

const (
	kvUpdateEnabled  = "update_check_enabled"
	kvUpdateManifest = "update_manifest_url"
	kvUpdateLastAt   = "update_last_check"
	kvUpdateLastJSON = "update_last_result"
	kvUpdateSkip     = "update_skip_version"
	// kvUpdateUsed 上一次**真正拉到清单**的那个候选地址（程序自写）。
	// 存在的原因很实际：同一份清单往往有 3 个等价地址（GitHub raw / Contents API /
	// 公共镜像），而它们在不同网络下的可达性差距是"0.6 秒"与"超时"的区别。
	// 记住上一次成功的那个、下次优先试它，可以避免每次检查都先在不可达的地址上
	// 白等一个 8 秒超时。
	kvUpdateUsed = "update_manifest_used"

	// defaultUpdateManifestURL 未配置清单地址时的默认值（v0.26.5）。
	//
	// 为什么要有默认值：本功能的整个意义是"提醒有新版本"，而"提醒"这件事
	// 一旦需要运维先手工填一个 raw URL 才会发生，实际结果就是**永远不发生**。
	// 这正是它此前在本项目线上被关掉（update_check_enabled=0、URL 为空）的原因链。
	// 默认指向本仓的 version.json —— 与程序同源，改版本时一起改，不需要额外维护。
	defaultUpdateManifestURL = "https://raw.githubusercontent.com/sheng-du0521/workbuddy2api/main/version.json"

	// updateManifestMaxBytes 清单体积上限。列表本身只有几百字节，给 64KB 足够，
	// 同时保证一个畸形/恶意响应不会把内存吃掉。
	updateManifestMaxBytes = 64 << 10
	// updateCacheTTL 缓存有效期：12 小时内不重复出站（启动时不打、打开页面不打）。
	updateCacheTTL = 12 * time.Hour
	// updateNetTimeout 单次拉取超时。
	updateNetTimeout = 8 * time.Second
	// updateStartupDelay 启动后延迟多久做第一次检查 —— 不与启动路径争资源。
	updateStartupDelay = 25 * time.Second
	// updatePeriodicEvery 后台周期性检查间隔。
	updatePeriodicEvery = 12 * time.Hour
)

// updateHTTPClient 拉取清单用的客户端。抽成变量以便测试注入
// （否则每个用例都要真连外网 —— 又慢又 flaky，最后没人愿意为它写测试）。
var updateHTTPClient = &http.Client{Timeout: updateNetTimeout}

// updateFetchMu 保证同一时刻只有一次真实出站（单飞）。
// 包级变量即可：一个进程只有一个 app 实例，它只是防并发，不承载状态。
var updateFetchMu sync.Mutex

// ---------------------------------------------------------------- 语义化版本

// semver 精简的 SemVer 2.0.0 表示。
type semver struct {
	major, minor, patch int
	pre                 []string // 预发布标识符；空表示正式版
	ok                  bool     // 解析是否成功
}

// parseSemver 解析版本号。宽松接受 `1` / `1.2` / `v1.2.3` / `1.2.3-rc.1+build`。
//
// 为什么宽松：清单是人手写的，逼着别人写全三段只会制造无意义的失败。
// 但**宽松不等于乱认** —— 非数字段一律判为解析失败，交给调用方降级处理。
func parseSemver(s string) semver {
	v := semver{}
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	s = strings.TrimPrefix(s, "V")
	if s == "" {
		return v
	}
	// 去掉 +build 元数据（SemVer 规定它不参与优先级比较）
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	var pre string
	if i := strings.IndexByte(s, '-'); i >= 0 {
		pre = s[i+1:]
		s = s[:i]
		// "1.2.3-" 这种尾随横线是畸形的。不特判的话 pre 为空字符串，
		// 会被下面 `if pre != ""` 静默跳过、当成 "1.2.3" —— 把一个写错的
		// 版本号当成合法版本，正是"更新提醒"最不该犯的错误。
		if pre == "" {
			return v
		}
	}
	parts := strings.Split(s, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return v
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < 0 {
			return v
		}
		nums[i] = n
	}
	if pre != "" {
		ids := strings.Split(pre, ".")
		for _, id := range ids {
			if id == "" {
				return v
			}
		}
		v.pre = ids
	}
	v.major, v.minor, v.patch = nums[0], nums[1], nums[2]
	v.ok = true
	return v
}

func sign(n int) int {
	switch {
	case n > 0:
		return 1
	case n < 0:
		return -1
	default:
		return 0
	}
}

// compareSemver 按 SemVer 2.0.0 的优先级规则比较，返回 -1 / 0 / 1。
//
// 规则要点（最容易写错的两条）：
//   - **有预发布标识的版本 < 对应的正式版**（1.0.0-rc.1 < 1.0.0）；
//   - 预发布逐段比较时，**纯数字段 < 非数字段**，且数字段按数值比较（不是字典序）。
func compareSemver(a, b semver) int {
	if a.major != b.major {
		return sign(a.major - b.major)
	}
	if a.minor != b.minor {
		return sign(a.minor - b.minor)
	}
	if a.patch != b.patch {
		return sign(a.patch - b.patch)
	}
	if len(a.pre) == 0 && len(b.pre) == 0 {
		return 0
	}
	if len(a.pre) == 0 {
		return 1
	}
	if len(b.pre) == 0 {
		return -1
	}
	n := len(a.pre)
	if len(b.pre) < n {
		n = len(b.pre)
	}
	for i := 0; i < n; i++ {
		ai, aerr := strconv.Atoi(a.pre[i])
		bi, berr := strconv.Atoi(b.pre[i])
		switch {
		case aerr == nil && berr == nil:
			if ai != bi {
				return sign(ai - bi)
			}
		case aerr == nil:
			return -1 // 数字标识符优先级更低
		case berr == nil:
			return 1
		default:
			if c := strings.Compare(a.pre[i], b.pre[i]); c != 0 {
				return c
			}
		}
	}
	return sign(len(a.pre) - len(b.pre))
}

// ---------------------------------------------------------------- 清单与状态

// updateAsset 一个平台产物。
type updateAsset struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256,omitempty"`
	Size   int64  `json:"size,omitempty"`
}

// updateManifest 更新清单（version.json 的结构）。
type updateManifest struct {
	Version      string        `json:"version"`
	MinSupported string        `json:"min_supported,omitempty"`
	Notes        string        `json:"notes,omitempty"`
	ReleasedAt   int64         `json:"released_at,omitempty"`
	URL          string        `json:"url,omitempty"`
	Assets       []updateAsset `json:"assets,omitempty"`
}

// updateStatus 交给前端的完整状态。
//
// 刻意把「检查失败」也表达出来（Error 字段）而不是隐藏：运维需要区分
// 「已是最新」和「压根没查成功」—— 后者会让人以为项目停止维护了。
type updateStatus struct {
	Current     string       `json:"current"`    // 本机版本
	Latest      string       `json:"latest"`     // 清单里的最新版本（可能为空）
	HasUpdate   bool         `json:"has_update"` // 是否有更新可用
	Severity    string       `json:"severity"`   // none / patch / minor / major
	Notes       string       `json:"notes,omitempty"`
	ReleaseURL  string       `json:"release_url,omitempty"`
	ReleasedAt  int64        `json:"released_at,omitempty"`
	CheckedAt   int64        `json:"checked_at,omitempty"`
	ManifestURL string       `json:"manifest_url"`            // 生效的清单地址（空 = 未配置）
	ManifestUse string       `json:"manifest_used,omitempty"` // 实际拉成功的候选地址（多地址回退时与上面不同）
	Enabled     bool         `json:"enabled"`
	Skipped     string       `json:"skipped,omitempty"`       // 已跳过的版本
	MinSupport  string       `json:"min_supported,omitempty"` // 清单声明的最低支持版本
	TooOld      bool         `json:"too_old"`                 // 当前版本已低于最低支持
	Asset       *updateAsset `json:"asset,omitempty"`         // 匹配当前平台+架构的产物
	Error       string       `json:"error,omitempty"`         // 上次检查的失败原因（空=成功）
	Stale       bool         `json:"stale"`                   // 结果来自过期缓存（离线时）

	// ---- 自动下载（v0.26.7）----
	// AutoDownload 自动下载开关的当前值（面板据此显示"自动/手动"）。
	AutoDownload bool `json:"auto_download"`
	// Download 产物下载状态。**始终非 nil**（未下载时 State="none"）——
	// 让前端无需判空就能渲染"未提供产物 / 正在下载 / 已就绪"三态。
	Download *updateDownload `json:"download,omitempty"`
}

// decideUpdate **纯函数**：给定当前版本、清单与用户偏好，算出要展示什么。
//
// 抽成纯函数是刻意的 —— 版本比较有一条边界特别容易写错（预发布、畸形版本号、
// 降级），而它又完全没有 IO 依赖，正好可以钉进单测。
func decideUpdate(current string, m *updateManifest, skipped string, checkedAt int64) updateStatus {
	st := updateStatus{Current: current, CheckedAt: checkedAt}
	if m == nil {
		return st
	}
	st.Latest = m.Version
	st.Notes = m.Notes
	st.ReleaseURL = m.URL
	st.ReleasedAt = m.ReleasedAt
	st.MinSupport = m.MinSupported
	st.Skipped = skipped

	cur, lat := parseSemver(current), parseSemver(m.Version)
	switch {
	case cur.ok && lat.ok:
		// 正常路径：语义化比较。
		if compareSemver(lat, cur) > 0 {
			st.HasUpdate = true
			st.Severity = severityOf(cur, lat)
		}
	case current != "" && m.Version != "":
		// 版本号畸形（如 "2026.10.13"、"dev"）：退化为**只判是否相等**。
		// 不报错、不猜大小 —— 猜错方向会让"更新提醒"变成"降级提醒"。
		if strings.TrimPrefix(strings.TrimPrefix(current, "v"), "V") !=
			strings.TrimPrefix(strings.TrimPrefix(m.Version, "v"), "V") {
			st.HasUpdate = true
			st.Severity = "unknown"
		}
	}

	// 用户点过"跳过此版本"：不再提示，但仍把最新版本号显示出来
	// （静静消失会让人怀疑检查坏了）。
	if st.HasUpdate && skipped != "" && skipped == m.Version {
		st.HasUpdate = false
	}
	if !st.HasUpdate {
		st.Severity = "none"
	}

	// 当前版本低于清单声明的最低支持版本 → 标记为需要尽快升级。
	if cur.ok && parseSemver(m.MinSupported).ok {
		if compareSemver(cur, parseSemver(m.MinSupported)) < 0 {
			st.TooOld = true
		}
	}

	// 匹配当前平台的产物（找不到也没关系，用 ReleaseURL 兜底）。
	for i := range m.Assets {
		a := m.Assets[i]
		if strings.EqualFold(a.OS, runtime.GOOS) && strings.EqualFold(a.Arch, runtime.GOARCH) {
			st.Asset = &m.Assets[i]
			break
		}
	}
	return st
}

// severityOf 新版比当前版高了哪一档。
func severityOf(cur, lat semver) string {
	switch {
	case lat.major != cur.major:
		return "major"
	case lat.minor != cur.minor:
		return "minor"
	default:
		return "patch"
	}
}

// ---------------------------------------------------------------- kv 读写

func (a *app) updateCheckEnabled() bool {
	v, _ := a.store.GetKV(kvUpdateEnabled)
	return strings.TrimSpace(v) != "0"
}

// updateManifestURL 生效的清单地址：显式配置优先，未配置时回落默认（见
// defaultUpdateManifestURL 的注释）。**返回的一定是非空值**，除非默认值被显式清空。
func (a *app) updateManifestURL() string {
	if v, _ := a.store.GetKV(kvUpdateManifest); strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return defaultUpdateManifestURL
}

// updateManifestUsed 上一次成功拉到清单的候选地址（空 = 还没成功过）。
func (a *app) updateManifestUsed() string {
	v, _ := a.store.GetKV(kvUpdateUsed)
	return strings.TrimSpace(v)
}

func (a *app) updateSkipVersion() string {
	v, _ := a.store.GetKV(kvUpdateSkip)
	return strings.TrimSpace(v)
}

// updateCacheFresh 缓存是否还在有效期内。
//
// 用**相对间隔**（now - lastAt < TTL）而不是绝对时间点判断，且 lastAt 由本机
// 自己写 —— 系统时钟被改动时最坏结果是多查一次，不会卡死。
func (a *app) updateCacheFresh(now time.Time) bool {
	v, _ := a.store.GetKV(kvUpdateLastAt)
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || n <= 0 {
		return false
	}
	return now.Sub(time.Unix(n, 0)) < updateCacheTTL
}

// ---------------------------------------------------------------- 出站拉取

// updateManifestCandidates 列出一个地址的**等价候选**，按建议尝试顺序返回。
//
// ## 为什么需要回退（v0.26.5）
//
// `raw.githubusercontent.com` 在部分网络下**直接不可达**（本项目实测：本机连不通，
// 12 秒超时；而 `api.github.com` 0.6 秒返回）。于是"检查更新"在这类环境里恒失败，
// 提醒功能等于不存在 —— 而它恰恰是私有化部署最需要的那个功能（用户不会天天来翻仓库）。
//
// 候选的来源是**从 raw 地址推导**，不是硬编码一个第三方域名列表：
//
//	https://raw.githubusercontent.com/OWNER/REPO/BRANCH/PATH
//	  → https://api.github.com/repos/OWNER/REPO/contents/PATH?ref=BRANCH   （GitHub 官方 API，可解析 base64）
//	  → https://cdn.jsdelivr.net/gh/OWNER/REPO@BRANCH/PATH                （公共 CDN 镜像）
//	  → https://ghproxy.net/https://raw.githubusercontent.com/...          （反代）
//
// 非 raw 形态的地址（自建 CDN、内网文件服务）**不做推导**：那种情况下"多试几个
// 第三方站点"没有任何依据，只会把出站面扩大。此时候选就是它自己。
//
// ⚠️ 安全边界：回退只改**传输路径**，不改**校验口径** —— 清单里给出的下载地址与
// SHA-256 照旧原样展示给运维，程序从不自动下载替换 exe（见文件头"为什么不做自动更新"）。
// 也就是说镜像被投毒的影响面是"提示里的说明文字"，不是"被自动装上的二进制"。
func updateManifestCandidates(primary string) []string {
	p := strings.TrimSpace(primary)
	if p == "" {
		return nil
	}
	out := []string{p}
	owner, repo, branch, path, ok := splitRawGitHubURL(p)
	if !ok {
		return out
	}
	out = append(out,
		fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s?ref=%s", owner, repo, path, branch),
		fmt.Sprintf("https://cdn.jsdelivr.net/gh/%s/%s@%s/%s", owner, repo, branch, path),
		"https://ghproxy.net/https://raw.githubusercontent.com/"+owner+"/"+repo+"/"+branch+"/"+path,
	)
	return out
}

// splitRawGitHubURL 解析 raw.githubusercontent.com/OWNER/REPO/BRANCH/PATH... 形态。
// 路径可以有多段（如 dist/version.json），故只切前三段、其余整体作为 path。
func splitRawGitHubURL(u string) (owner, repo, branch, path string, ok bool) {
	const prefix = "https://raw.githubusercontent.com/"
	rest, found := strings.CutPrefix(u, prefix)
	if !found {
		// 也接受 http:// 形态（老配置里可能有）。
		rest, found = strings.CutPrefix(u, "http://raw.githubusercontent.com/")
		if !found {
			return "", "", "", "", false
		}
	}
	rest = strings.TrimPrefix(rest, "/")
	parts := strings.SplitN(rest, "/", 4)
	if len(parts) < 4 {
		return "", "", "", "", false
	}
	owner, repo, branch, path = parts[0], parts[1], parts[2], parts[3]
	if owner == "" || repo == "" || branch == "" || path == "" {
		return "", "", "", "", false
	}
	return owner, repo, branch, path, true
}

// parseUpdateManifestBody 把响应体解析成清单，**同时接受两种封装**：
//
//  1. 裸清单 JSON（version.json 本体，raw / CDN 镜像返回的就是它）；
//  2. GitHub Contents API 信封：{name,path,size,content:"<base64>",encoding:"base64",...}。
//
// 第 2 种必须支持 —— 它正是 raw 不可达时的那条回退路径。不支持的话"回退"
// 只会拿到一个没有 version 字段的 JSON 然后报"清单缺少 version 字段"，
// 看起来像清单写错了，实际是解析口径没跟上。
func parseUpdateManifestBody(body []byte) (*updateManifest, error) {
	var m updateManifest
	if err := json.Unmarshal(body, &m); err == nil && strings.TrimSpace(m.Version) != "" {
		return &m, nil
	}
	var env struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("清单解析失败: %w", err)
	}
	if strings.EqualFold(strings.TrimSpace(env.Encoding), "base64") && strings.TrimSpace(env.Content) != "" {
		// Contents API 的 content 是**折行**的 base64（每 60 字符一个 \n）。
		raw, err := base64.StdEncoding.DecodeString(stripSpace(env.Content))
		if err != nil {
			return nil, fmt.Errorf("清单 base64 解码失败: %w", err)
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("清单解析失败（Contents API 内容）: %w", err)
		}
	}
	if strings.TrimSpace(m.Version) == "" {
		return nil, fmt.Errorf("清单缺少 version 字段")
	}
	return &m, nil
}

// stripSpace 去掉字符串里所有空白（base64 折行、缩进、\r）。
func stripSpace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r':
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// fetchManifestOnce 在**一个**候选地址上拉一次清单。失败返回 error 由调用方决定是否换地址。
func fetchManifestOnce(ctx context.Context, url, curVersion string) (*updateManifest, error) {
	if url == "" {
		return nil, fmt.Errorf("未配置更新清单地址")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// 带 UA 是礼貌也是实践：GitHub raw / CDN / 反代都可能拒绝空 UA 的客户端。
	req.Header.Set("User-Agent", "workbuddy2api/"+curVersion)
	req.Header.Set("Accept", "application/json")

	resp, err := updateHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 403/429 常见于被限流；这里不做重试（下次周期再试即可），
		// 把状态码如实带回，便于运维判断是不是被 CDN 拦了。
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, updateManifestMaxBytes))
	if err != nil {
		return nil, err
	}
	return parseUpdateManifestBody(body)
}

// fetchUpdateManifest 依候选顺序尝试，返回第一个成功的清单与其实际地址。
//
// 每个候选各自受 ctx 约束（ctx 的预算由调用方给足，见 handleAdminUpdateStatus）；
// **逐个试**而不是并发打，是因为候选之间有明确的主次（canonical 优先），
// 并发只会让我们对外多打几个包。
//
// 全部失败时 error 汇总每个候选的失败原因 —— 排障时一眼能看出"是全都超时
// （网络不通）"还是"只有 raw 超时而 API 报 404（路径写错了）"。
func fetchUpdateManifest(ctx context.Context, primary, curVersion, lastUsed string) (*updateManifest, string, error) {
	cands := updateManifestCandidates(primary)
	if len(cands) == 0 {
		return nil, "", fmt.Errorf("未配置更新清单地址")
	}
	// 上次成功的候选提到最前：避免每次都在已知不可达的地址上白等一个超时。
	cands = orderUpdateCandidates(cands, lastUsed)
	errs := make([]string, 0, len(cands))
	for _, c := range cands {
		m, err := fetchManifestOnce(ctx, c, curVersion)
		if err == nil {
			return m, c, nil
		}
		errs = append(errs, shortHost(c)+": "+err.Error())
		if ctx.Err() != nil {
			break // 预算用尽：剩下的候选不必再试（免得把"超时"记成"全部不可达"）
		}
	}
	return nil, "", fmt.Errorf("所有候选地址均失败（%s）", strings.Join(errs, "；"))
}

// orderUpdateCandidates 把上次命中的候选提到最前（抽成纯函数以便钉进测试：
// "优先试上次成功的地址"这条规则看不见摸不着，只靠端到端很难断言）。
//
// lastUsed 不在候选里（运维改了清单地址 / 候选推导规则变了）时**原样返回** ——
// 不把外部输入塞进候选列表，否则一个陈旧的 kv 值就能让程序去访问任意 URL。
func orderUpdateCandidates(cands []string, lastUsed string) []string {
	if lastUsed == "" || len(cands) < 2 {
		return cands
	}
	for i, c := range cands {
		if c == lastUsed {
			if i == 0 {
				return cands
			}
			out := make([]string, 0, len(cands))
			out = append(out, lastUsed)
			out = append(out, cands[:i]...)
			out = append(out, cands[i+1:]...)
			return out
		}
	}
	return cands
}

// shortHost 把地址压成 host+路径，用于错误汇总（完整 URL 太长，且反代形态会嵌套）。
func shortHost(u string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	if i := strings.IndexByte(s, '/'); i > 0 {
		return s[:i]
	}
	return s
}

// checkUpdate 执行一次检查（带缓存与单飞），返回给前端的状态。
//
// force=true 时忽略缓存（用户手动点了"立即检查"）。
func (a *app) checkUpdate(ctx context.Context, force bool) updateStatus {
	now := time.Now()
	st := updateStatus{
		Current:     version,
		Enabled:     a.updateCheckEnabled(),
		ManifestURL: a.updateManifestURL(),
		Skipped:     a.updateSkipVersion(),
	}
	if !st.Enabled {
		// 关闭时也要把本机版本显示出来 —— "已关闭检查"不等于"没有版本信息"。
		a.attachDownload(&st)
		return st
	}
	if st.ManifestURL == "" {
		st.Error = "未配置更新清单地址（设置 → 关于 / 版本更新 可填）"
		a.attachDownload(&st)
		return st
	}
	if !force && a.updateCacheFresh(now) {
		if m := a.cachedManifest(); m != nil {
			out := decideUpdate(version, m, st.Skipped, a.updateLastCheckAt())
			out.Enabled, out.ManifestURL = true, st.ManifestURL
			out.ManifestUse = a.updateManifestUsed()
			a.attachDownload(&out)
			return out
		}
	}

	// 单飞：并发的页面请求不该打出多份网络请求。
	updateFetchMu.Lock()
	defer updateFetchMu.Unlock()
	if !force && a.updateCacheFresh(time.Now()) {
		if m := a.cachedManifest(); m != nil {
			out := decideUpdate(version, m, st.Skipped, a.updateLastCheckAt())
			out.Enabled, out.ManifestURL = true, st.ManifestURL
			out.ManifestUse = a.updateManifestUsed()
			a.attachDownload(&out)
			return out
		}
	}

	m, used, err := fetchUpdateManifest(ctx, st.ManifestURL, version, a.updateManifestUsed())
	if err != nil {
		// 失败：**保留上一次的好结果**（如果有），只把 error 标出来。
		// 覆盖成空会让"暂时网络不通"看起来像"项目已停止维护"。
		st.Error = err.Error()
		if m2 := a.cachedManifest(); m2 != nil {
			out := decideUpdate(version, m2, st.Skipped, a.updateLastCheckAt())
			out.Enabled, out.ManifestURL, out.Error = true, st.ManifestURL, st.Error
			out.ManifestUse = a.updateManifestUsed()
			out.Stale = true
			a.attachDownload(&out)
			return out
		}
		a.attachDownload(&st)
		return st
	}

	// 成功：写缓存（含时间戳）与**命中的候选地址**（下次优先试它）。
	if blob, err := json.Marshal(m); err == nil {
		_ = a.store.SetKV(kvUpdateLastJSON, string(blob))
		_ = a.store.SetKV(kvUpdateLastAt, strconv.FormatInt(now.Unix(), 10))
		_ = a.store.SetKV(kvUpdateUsed, used)
	}
	out := decideUpdate(version, m, st.Skipped, now.Unix())
	out.Enabled, out.ManifestURL = true, st.ManifestURL
	out.ManifestUse = used
	// 自动下载（v0.26.7）：发现新版本且清单给了本机平台的产物时**异步**下载。
	// 必须异步 —— 这里处在 HTTP 请求路径上，而一次 20MB 的下载远超本次请求
	// 的预算（候选数 × 8s）。状态通过 Download 字段随每次刷新回报。
	a.attachDownload(&out)
	return out
}

// attachDownload 把下载状态挂到更新状态上，并在需要时**触发一次**异步下载。
//
// 触发条件（全部满足才下）：
//   - 有更新、且清单提供了本机平台的产物（没有产物就没什么可下）；
//   - 自动下载开关打开（关掉时只展示已有结果，不发起新的下载）；
//   - 该版本尚未下好、也没有正在下的（避免每次页面刷新都起一个 goroutine）。
func (a *app) attachDownload(st *updateStatus) {
	st.AutoDownload = a.updateAutoDownloadEnabled()
	d := a.loadUpdateDownload()

	if !st.AutoDownload || !st.HasUpdate || st.Asset == nil {
		st.Download = &d
		return
	}
	// 已下好同一版本且文件仍在 → 直接展示。
	if d.State == "done" && d.Version == st.Latest && updateNewFileExists() {
		st.Download = &d
		return
	}
	// 已在下载同一版本 → 只回报状态。
	updateTriggerMu.Lock()
	if updateTriggeringFor == st.Latest {
		updateTriggerMu.Unlock()
		d.State, d.Version = "downloading", st.Latest
		st.Download = &d
		return
	}
	updateTriggeringFor = st.Latest
	updateTriggerMu.Unlock()

	snapshot := *st
	go func() {
		defer func() {
			updateTriggerMu.Lock()
			updateTriggeringFor = ""
			updateTriggerMu.Unlock()
		}()
		// 用 Background 而不是请求的 ctx：响应一结束请求 ctx 就会被取消，
		// 那样下载永远走不完（表现为状态卡在 downloading）。
		ctx, cancel := context.WithTimeout(context.Background(), updateDownloadTimeout)
		defer cancel()
		a.downloadUpdate(ctx, snapshot)
	}()

	d.State, d.Version = "downloading", st.Latest
	st.Download = &d
}

// cachedManifest 读出上次成功拉到的清单（兼作离线缓存）。解析失败视为无缓存。
func (a *app) cachedManifest() *updateManifest {
	v, _ := a.store.GetKV(kvUpdateLastJSON)
	if strings.TrimSpace(v) == "" {
		return nil
	}
	var m updateManifest
	if err := json.Unmarshal([]byte(v), &m); err != nil {
		return nil
	}
	return &m
}

func (a *app) updateLastCheckAt() int64 {
	v, _ := a.store.GetKV(kvUpdateLastAt)
	n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	return n
}

// ---------------------------------------------------------------- 后台轮询

// updateLoop 后台周期性检查：启动后延迟一次，之后每 12 小时一次。
//
// 为什么不在启动路径上同步做：启动时要做的事已经够多，而"查版本"纯属锦上添花，
// 让它去和初始化抢资源是本末倒置。延迟 25 秒是个经验值 —— 足够让启动流程跑完。
func (a *app) updateLoop(ctx context.Context) {
	timer := time.NewTimer(updateStartupDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	// 首次：只有在缓存不新鲜时才真的出站，并**静默**处理失败。
	if a.updateCheckEnabled() && a.updateManifestURL() != "" && !a.updateCacheFresh(time.Now()) {
		st := a.checkUpdate(ctx, false)
		if st.Error != "" {
			log.Printf("INFO: [update] 版本检查未成功（不影响服务）：%s", st.Error)
		} else if st.HasUpdate {
			log.Printf("INFO: [update] 发现新版本 %s（当前 %s，%s）", st.Latest, st.Current, st.Severity)
		}
	}
	tick := time.NewTicker(updatePeriodicEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if !a.updateCheckEnabled() || a.updateManifestURL() == "" {
				continue
			}
			if st := a.checkUpdate(ctx, false); st.HasUpdate {
				log.Printf("INFO: [update] 发现新版本 %s（当前 %s）", st.Latest, st.Current)
			}
		}
	}
}

// ---------------------------------------------------------------- HTTP 端点

// handleAdminUpdateStatus 管理侧：读当前更新状态（GET /api/admin/update-status）。
//
// 默认走缓存；缓存新鲜时**完全不出站**，所以打开页面不会因为"检查更新"而变慢。
func (a *app) handleAdminUpdateStatus(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("force") == "1"
	// 预算要能容纳**多个候选**（raw 超时后还要试 API / CDN），否则回退逻辑
	// 会在第一个候选的超时上把预算耗尽、永远走不到第二个。
	// 用候选数而不是硬编码 3：推导规则变了这里自动跟着变。
	budget := time.Duration(len(updateManifestCandidates(a.updateManifestURL())))*updateNetTimeout + 2*time.Second
	if budget < updateNetTimeout+2*time.Second {
		budget = updateNetTimeout + 2*time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), budget)
	defer cancel()
	writeJSON(w, http.StatusOK, a.checkUpdate(ctx, force))
}

// handleAdminUpdateSkip 管理侧：跳过某个版本（POST /api/admin/update-skip）。
//
// 与"关闭检查"是**两个不同的动作**，刻意分开：
//   - 跳过：只对某个版本生效，下一个版本还会提醒；
//   - 关闭：彻底不再检查。
//
// 混成一个按钮会导致"用户只想忽略这一版，结果再也收不到提醒"。
func (a *app) handleAdminUpdateSkip(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Version string `json:"version"`
		Clear   bool   `json:"clear"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req)
	v := strings.TrimSpace(req.Version)
	if req.Clear {
		v = ""
	}
	if err := a.store.SetKV(kvUpdateSkip, v); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "skipped": v})
}

// ---------------------------------------------------------------- CLI

// printUpdateStatus -check-update 的输出。
//
// 输出刻意是**人类可读的多行文本**而不是 JSON：它的用途是"运维跑一下看一眼"。
// 需要机器可读的话直接请求 /api/admin/update-status（那里是 JSON）。
func printUpdateStatus(st updateStatus) {
	fmt.Printf("当前版本: %s\n", st.Current)
	if !st.Enabled {
		fmt.Println("版本检查: 已关闭（设置 → 关于 / 版本更新）")
		return
	}
	if st.ManifestURL == "" {
		fmt.Println("更新源:   未配置（设置 → 关于 / 版本更新 可填清单地址）")
		return
	}
	fmt.Printf("更新源:   %s\n", st.ManifestURL)
	if st.ManifestUse != "" && st.ManifestUse != st.ManifestURL {
		fmt.Printf("实际命中: %s\n", st.ManifestUse)
	}
	if st.Error != "" {
		fmt.Printf("检查结果: 失败 —— %s\n", st.Error)
		return
	}
	fmt.Printf("最新版本: %s\n", st.Latest)
	if st.HasUpdate {
		fmt.Printf("状态:     有新版本可用（%s 级别）\n", st.Severity)
		if st.ReleaseURL != "" {
			fmt.Printf("发布页:   %s\n", st.ReleaseURL)
		}
		if st.Asset != nil && st.Asset.URL != "" {
			fmt.Printf("下载:     %s\n", st.Asset.URL)
			if st.Asset.SHA256 != "" {
				fmt.Printf("SHA-256:  %s\n", st.Asset.SHA256)
			}
		}
		if st.Notes != "" {
			fmt.Printf("更新说明: %s\n", st.Notes)
		}
		if st.Skipped == st.Latest {
			fmt.Println("注意:     该版本此前已被标记为「跳过」")
		}
	} else {
		fmt.Println("状态:     已是最新")
	}
	if st.TooOld {
		fmt.Printf("警告:     当前版本低于最低支持版本 %s，建议尽快升级\n", st.MinSupport)
	}
	if st.CheckedAt > 0 {
		fmt.Printf("检查时间: %s\n", time.Unix(st.CheckedAt, 0).Format("2006-01-02 15:04:05"))
	}
}

// updateExitCode -check-update 的退出码。
//
// 刻意分三档：**巡检脚本必须能区分「有新版本」和「根本没查成功」**。
// 把检查失败当成"已是最新"（都返回 0）是这类脚本最常见的坑 ——
// 结果是更新源挂了两周都没人发现。
func updateExitCode(st updateStatus) int {
	switch {
	case st.Error != "" || !st.Enabled || st.ManifestURL == "":
		return 2
	case st.HasUpdate:
		return 1
	default:
		return 0
	}
}
