// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 版本清单（version.json）的**纯逻辑**：常量、语义化版本、清单类型、状态判定。
//
// 为什么单独成包（v0.31.7，P2-7 拆包第一刀）：这部分原来住在 cmd/webapp 这个 5 万行的
// main 包里，与"读写 kv / 发 HTTP / 生成替换脚本"这些 IO 胶水混在一起 —— 结果是
// **纯逻辑没法单独测**（要先造一个 app、一个 store、一个临时 exe 路径），而它恰恰是
// 更新功能里唯一需要严格推理的部分（版本比较、平台匹配、降级顺序）。
// 现在：纯逻辑在此，IO 留在 cmd/webapp，边界是"这个包不认识 app / store / 文件系统"。
//
// ⚠️ 本包**不做任何网络与文件操作**（拉取在 fetch.go 里，用的是调用方注入的 client；
// 下载与替换脚本仍在 cmd/webapp —— 那部分要摸 exe 路径，见 P2-7 方案的第二刀）。
package selfupdate

import (
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	KVEnabled  = "update_check_enabled"
	KVManifest = "update_manifest_url"
	KVLastAt   = "update_last_check"
	KVLastJSON = "update_last_result"
	KVSkip     = "update_skip_version"
	// KVUsed 上一次**真正拉到清单**的那个候选地址（程序自写）。
	// 存在的原因很实际：同一份清单往往有 3 个等价地址（GitHub raw / Contents API /
	// 公共镜像），而它们在不同网络下的可达性差距是"0.6 秒"与"超时"的区别。
	// 记住上一次成功的那个、下次优先试它，可以避免每次检查都先在不可达的地址上
	// 白等一个 8 秒超时。
	KVUsed = "update_manifest_used"

	// DefaultManifestURL 未配置清单地址时的默认值（v0.26.5）。
	//
	// 为什么要有默认值：本功能的整个意义是"提醒有新版本"，而"提醒"这件事
	// 一旦需要运维先手工填一个 raw URL 才会发生，实际结果就是**永远不发生**。
	// 这正是它此前在本项目线上被关掉（update_check_enabled=0、URL 为空）的原因链。
	// 默认指向本仓的 version.json —— 与程序同源，改版本时一起改，不需要额外维护。
	DefaultManifestURL = "https://raw.githubusercontent.com/sheng-du0521/workbuddy2api/main/version.json"

	// ManifestMaxBytes 清单体积上限。列表本身只有几百字节，给 64KB 足够，
	// 同时保证一个畸形/恶意响应不会把内存吃掉。
	ManifestMaxBytes = 64 << 10
	// CacheTTL 缓存有效期：12 小时内不重复出站（启动时不打、打开页面不打）。
	CacheTTL = 12 * time.Hour
	// NetTimeout 单次拉取超时。
	NetTimeout = 8 * time.Second
	// StartupDelay 启动后延迟多久做第一次检查 —— 不与启动路径争资源。
	StartupDelay = 25 * time.Second
	// PeriodicEvery 后台周期性检查间隔。
	PeriodicEvery = 12 * time.Hour
)

// HTTPClient 拉取清单用的客户端。抽成变量以便测试注入
// （否则每个用例都要真连外网 —— 又慢又 flaky，最后没人愿意为它写测试）。
var HTTPClient = &http.Client{Timeout: NetTimeout}

// updateFetchMu 保证同一时刻只有一次真实出站（单飞）。
// 包级变量即可：一个进程只有一个 app 实例，它只是防并发，不承载状态。
var updateFetchMu sync.Mutex

// ---------------------------------------------------------------- 语义化版本

// Semver 精简的 SemVer 2.0.0 表示。
type Semver struct {
	Major, Minor, Patch int
	Pre                 []string // 预发布标识符；空表示正式版
	// OK 解析是否成功（导出：调用方要能判断"这个版本号能不能比"）。
	OK bool
}

// ParseSemver 解析版本号。宽松接受 `1` / `1.2` / `v1.2.3` / `1.2.3-rc.1+build`。
//
// 为什么宽松：清单是人手写的，逼着别人写全三段只会制造无意义的失败。
// 但**宽松不等于乱认** —— 非数字段一律判为解析失败，交给调用方降级处理。
func ParseSemver(s string) Semver {
	v := Semver{}
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
		v.Pre = ids
	}
	v.Major, v.Minor, v.Patch = nums[0], nums[1], nums[2]
	v.OK = true
	return v
}

func signOf(n int) int {
	switch {
	case n > 0:
		return 1
	case n < 0:
		return -1
	default:
		return 0
	}
}

// CompareSemver 按 SemVer 2.0.0 的优先级规则比较，返回 -1 / 0 / 1。
//
// 规则要点（最容易写错的两条）：
//   - **有预发布标识的版本 < 对应的正式版**（1.0.0-rc.1 < 1.0.0）；
//   - 预发布逐段比较时，**纯数字段 < 非数字段**，且数字段按数值比较（不是字典序）。
func CompareSemver(a, b Semver) int {
	if a.Major != b.Major {
		return signOf(a.Major - b.Major)
	}
	if a.Minor != b.Minor {
		return signOf(a.Minor - b.Minor)
	}
	if a.Patch != b.Patch {
		return signOf(a.Patch - b.Patch)
	}
	if len(a.Pre) == 0 && len(b.Pre) == 0 {
		return 0
	}
	if len(a.Pre) == 0 {
		return 1
	}
	if len(b.Pre) == 0 {
		return -1
	}
	n := len(a.Pre)
	if len(b.Pre) < n {
		n = len(b.Pre)
	}
	for i := 0; i < n; i++ {
		ai, aerr := strconv.Atoi(a.Pre[i])
		bi, berr := strconv.Atoi(b.Pre[i])
		switch {
		case aerr == nil && berr == nil:
			if ai != bi {
				return signOf(ai - bi)
			}
		case aerr == nil:
			return -1 // 数字标识符优先级更低
		case berr == nil:
			return 1
		default:
			if c := strings.Compare(a.Pre[i], b.Pre[i]); c != 0 {
				return c
			}
		}
	}
	return signOf(len(a.Pre) - len(b.Pre))
}

// ---------------------------------------------------------------- 清单与状态

// Asset 一个平台产物。
type Asset struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256,omitempty"`
	Size   int64  `json:"size,omitempty"`
}

// Manifest 更新清单（version.json 的结构）。
type Manifest struct {
	Version      string  `json:"version"`
	MinSupported string  `json:"min_supported,omitempty"`
	Notes        string  `json:"notes,omitempty"`
	ReleasedAt   int64   `json:"released_at,omitempty"`
	URL          string  `json:"url,omitempty"`
	Assets       []Asset `json:"assets,omitempty"`
}

// Status 交给前端的完整状态。
//
// 刻意把「检查失败」也表达出来（Error 字段）而不是隐藏：运维需要区分
// 「已是最新」和「压根没查成功」—— 后者会让人以为项目停止维护了。
type Status struct {
	Current     string `json:"current"`    // 本机版本
	Latest      string `json:"latest"`     // 清单里的最新版本（可能为空）
	HasUpdate   bool   `json:"has_update"` // 是否有更新可用
	Severity    string `json:"severity"`   // none / patch / minor / major
	Notes       string `json:"notes,omitempty"`
	ReleaseURL  string `json:"release_url,omitempty"`
	ReleasedAt  int64  `json:"released_at,omitempty"`
	CheckedAt   int64  `json:"checked_at,omitempty"`
	ManifestURL string `json:"manifest_url"`            // 生效的清单地址（空 = 未配置）
	ManifestUse string `json:"manifest_used,omitempty"` // 实际拉成功的候选地址（多地址回退时与上面不同）
	Enabled     bool   `json:"enabled"`
	Skipped     string `json:"skipped,omitempty"`       // 已跳过的版本
	MinSupport  string `json:"min_supported,omitempty"` // 清单声明的最低支持版本
	TooOld      bool   `json:"too_old"`                 // 当前版本已低于最低支持
	Asset       *Asset `json:"asset,omitempty"`         // 匹配当前平台+架构的产物
	Error       string `json:"error,omitempty"`         // 上次检查的失败原因（空=成功）
	Stale       bool   `json:"stale"`                   // 结果来自过期缓存（离线时）

	// ---- 自动下载（v0.26.7）----
	// AutoDownload 自动下载开关的当前值（面板据此显示"自动/手动"）。
	AutoDownload bool `json:"auto_download"`
	// Download 产物下载状态。**始终非 nil**（未下载时 State="none"）——
	// 让前端无需判空就能渲染"未提供产物 / 正在下载 / 已就绪"三态。
	Download *Download `json:"download,omitempty"`
}

// Decide **纯函数**：给定当前版本、清单与用户偏好，算出要展示什么。
//
// 抽成纯函数是刻意的 —— 版本比较有一条边界特别容易写错（预发布、畸形版本号、
// 降级），而它又完全没有 IO 依赖，正好可以钉进单测。
func Decide(current string, m *Manifest, skipped string, checkedAt int64) Status {
	st := Status{Current: current, CheckedAt: checkedAt}
	if m == nil {
		return st
	}
	st.Latest = m.Version
	st.Notes = m.Notes
	st.ReleaseURL = m.URL
	st.ReleasedAt = m.ReleasedAt
	st.MinSupport = m.MinSupported
	st.Skipped = skipped

	cur, lat := ParseSemver(current), ParseSemver(m.Version)
	switch {
	case cur.OK && lat.OK:
		// 正常路径：语义化比较。
		if CompareSemver(lat, cur) > 0 {
			st.HasUpdate = true
			st.Severity = SeverityOf(cur, lat)
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
	if cur.OK && ParseSemver(m.MinSupported).OK {
		if CompareSemver(cur, ParseSemver(m.MinSupported)) < 0 {
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

// SeverityOf 新版比当前版高了哪一档。
func SeverityOf(cur, lat Semver) string {
	switch {
	case lat.Major != cur.Major:
		return "major"
	case lat.Minor != cur.Minor:
		return "minor"
	default:
		return "patch"
	}
}

// ---------------------------------------------------------------- kv 读写

// Download 产物下载状态（给前端）。
//
// 与 Manifest/Status 同属"纯数据"，故一起下沉（v0.31.7）：它出现在 Status.Download 里，
// 若留在 cmd/webapp，边界就会被一个数据字段反向依赖打破。
// 注意字段名与 JSON tag 是**对外契约**（前端读 state/version/sha256/apply_hint），
// 改它们等于改协议。
type Download struct {
	// State none / ready / downloading / done / failed
	State string `json:"state"`
	// Version 已下载（或正在下载）的版本；未下载时为空。
	Version string `json:"version,omitempty"`
	// Size 期望体积（清单声明，0 = 未知）；GotSize 实际落盘体积。
	Size    int64 `json:"size,omitempty"`
	GotSize int64 `json:"got_size,omitempty"`
	// SHA256 期望摘要（清单声明）。**必须非空**才允许下载 —— 见 downloadUpdate。
	SHA256 string `json:"sha256,omitempty"`
	// File 落盘文件名（相对 exe 目录，不暴露绝对路径。
	// 绝对路径在面板上既无用又可能在截图/日志里泄露部署目录结构）。
	File string `json:"file,omitempty"`
	// At 完成时刻；Error 失败原因。
	At    int64  `json:"at,omitempty"`
	Error string `json:"error,omitempty"`
	// ApplyHint 应用更新的操作指引（人话，直接可粘贴）。
	ApplyHint string `json:"apply_hint,omitempty"`
}
