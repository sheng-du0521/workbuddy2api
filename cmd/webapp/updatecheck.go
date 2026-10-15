// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"workbuddy2api/internal/selfupdate"
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
//
// ── 过渡期别名（v0.31.7）──────────────────────────────────────────────────────
//
// 清单解析 / 语义化版本 / 候选推导 / 状态判定这些**纯逻辑**已下沉到 internal/selfupdate
// （P2-7 拆包第一刀）。本块只做**名字映射**，让本包既有的调用点与测试**一行不改** ——
// 迁移分两步走：先建立边界（本步，零行为变化），再把调用点逐个迁到 selfupdate 的
// 正式 API 并删掉这些别名（见 docs/P2-7-拆包方案.md）。
//
// ⚠️ 函数/常量走别名没问题；**变量不行**（`var x = pkg.X` 是值拷贝，测试改本地那份
// 不会影响包内使用），所以 HTTPClient 是**直接引用包内变量**，测试也直接改它。
type (
	semver         = selfupdate.Semver
	updateAsset    = selfupdate.Asset
	updateManifest = selfupdate.Manifest
	updateStatus   = selfupdate.Status
)

const (
	kvUpdateEnabled          = selfupdate.KVEnabled
	kvUpdateManifest         = selfupdate.KVManifest
	kvUpdateLastAt           = selfupdate.KVLastAt
	kvUpdateLastJSON         = selfupdate.KVLastJSON
	kvUpdateSkip             = selfupdate.KVSkip
	kvUpdateUsed             = selfupdate.KVUsed
	updateNetTimeout         = selfupdate.NetTimeout
	updateStartupDelay       = selfupdate.StartupDelay
	updatePeriodicEvery      = selfupdate.PeriodicEvery
	updateCacheTTL           = selfupdate.CacheTTL
	updateManifestMaxBytes   = selfupdate.ManifestMaxBytes
	defaultUpdateManifestURL = selfupdate.DefaultManifestURL
)

var (
	parseSemver              = selfupdate.ParseSemver
	compareSemver            = selfupdate.CompareSemver
	decideUpdate             = selfupdate.Decide
	severityOf               = selfupdate.SeverityOf
	updateManifestCandidates = selfupdate.Candidates
	splitRawGitHubURL        = selfupdate.SplitRawGitHubURL
	parseUpdateManifestBody  = selfupdate.ParseBody
	fetchUpdateManifest      = selfupdate.Fetch
	orderUpdateCandidates    = selfupdate.OrderCandidates
	shortHost                = selfupdate.ShortHost
	printUpdateStatus        = selfupdate.PrintStatus
	updateExitCode           = selfupdate.ExitCode
)

// ⚠️ 这里**刻意没有** updateHTTPClient 这个名字：曾经有过一个
// `var updateHTTPClient = selfupdate.HTTPClient` 的"别名"，但变量别名是**值拷贝** ——
// 测试改它只改本地那份，包内 fetch 仍用 selfupdate.HTTPClient（表现为"注入了假 client
// 却真的去连了外网"，静默失效）。要注入就写 `selfupdate.HTTPClient = ...`，
// 或者干脆让这个名字不存在（误用者直接编译失败，比静默失效好）。

// updateFetchMu 保证同一时刻只有一次真实出站（单飞）。
//
// 刻意**留在本包**：它是调用方的策略（"整个应用同一时刻只打一次清单"），而不是
// 清单库的能力 —— 库只管"怎么取一份清单"，要不要串行化由使用方决定。
var updateFetchMu sync.Mutex

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
