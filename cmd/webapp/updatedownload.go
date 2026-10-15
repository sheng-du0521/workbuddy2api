// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// updatedownload.go 更新产物的**自动下载**与「应用更新」入口（v0.26.7）。
//
// 与 updatecheck.go 的分工：
//   - updatecheck.go：发现新版本（拉清单、比版本、缓存、提醒）；
//   - 本文件：把清单里声明的产物**下载下来并校验**，但**绝不自动替换正在运行的 exe**。
//
// 为什么不自动替换（这是刻意的安全边界，不是没做）：
//
//	① 正在运行的 exe 在 Windows 上无法被覆盖，任何"自动替换"最终都要靠外部进程
//	   在退出后动手 —— 那意味着一次**无人值守的进程重启**，会打断在途请求；
//	② 自动替换等于把"上游仓库/镜像被篡改"直接变成"本机执行任意代码"。下载并校验
//	   sha256 只能保证"与清单声明一致"，而清单本身与二进制来自同一处 —— 不能自证。
//	   所以最后一步必须由人确认。
//
// 因此本模块做到：下载 → 校验 sha256 → 落盘为 `<exe>.new` → 生成替换脚本。
// 替换动作由运维在维护窗口执行（或调用 /api/admin/update-apply 拿到指引）。
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	// kvUpdateAutoDownload 是否在发现新版本后自动下载产物（默认开）。
	//
	// 默认开：产物只落盘、不替换，风险极低，而"发现新版本时已经在本地备好了"
	// 是它全部的价值 —— 需要更新时不用再等一次下载。
	kvUpdateAutoDownload = "update_auto_download"
	// kvUpdateDownloadJSON 最近一次下载结果的 JSON（供面板展示）。
	kvUpdateDownloadJSON = "update_download_result"

	// updateDownloadMaxBytes 产物体积上限。本程序 exe 约 14~20MB，给 200MB 足够，
	// 同时保证一个畸形/恶意的 Content-Length 不会把磁盘写满。
	updateDownloadMaxBytes = 200 << 20
	// updateDownloadTimeout 单次下载的总超时。
	updateDownloadTimeout = 5 * time.Minute

	// updateNewSuffix 落盘后缀：`webapp.exe` → `webapp.exe.new`。
	//
	// 用固定后缀而不是带版本号：替换脚本要能**无条件**找到它，
	// 而带版本号需要在生成脚本时再插值一次（多一处可能不一致的地方）。
	updateNewSuffix = ".new"
	// updateHelperName 生成的替换脚本名。
	updateHelperName = "apply-update.bat"
)

// updateDownload 下载状态（给前端）。
type updateDownload struct {
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

// updateAutoDownloadEnabled 读自动下载开关（默认**开**）。
func (a *app) updateAutoDownloadEnabled() bool {
	v, _ := a.store.GetKV(kvUpdateAutoDownload)
	v = strings.TrimSpace(v)
	if v == "" {
		return true // 缺省开
	}
	return v == "1" || strings.EqualFold(v, "on") || strings.EqualFold(v, "true")
}

// updateAssetFor 从清单里挑出匹配**当前平台与架构**的产物。
//
// 匹配规则：os/arch 都非空且与本机一致。清单里缺 os/arch 的条目**不匹配**
// （不知道给谁的产物不能装）；本机 runtime.GOOS 为 "windows" 时清单写 "win"
// 也认（两种写法在实际项目里都常见，认它不需要额外代价）。
func updateAssetFor(assets []updateAsset) *updateAsset {
	wantOS := runtime.GOOS
	wantArch := runtime.GOARCH
	for i := range assets {
		a := assets[i]
		osName := strings.ToLower(strings.TrimSpace(a.OS))
		arch := strings.ToLower(strings.TrimSpace(a.Arch))
		if osName == "" || arch == "" {
			continue
		}
		osOK := osName == wantOS || (wantOS == "windows" && osName == "win")
		archOK := arch == wantArch || (wantArch == "amd64" && arch == "x86_64") ||
			(wantArch == "arm64" && arch == "aarch64")
		if osOK && archOK && strings.TrimSpace(a.URL) != "" {
			return &assets[i]
		}
	}
	return nil
}

// updateArgs 返回当前进程的启动参数（不含 argv[0]）。抽成变量以便测试注入：
// 用例无法真的重启 go test 进程，但必须能验证"参数被写进了脚本"。
var updateArgs = func() []string { return os.Args[1:] }

// updateExecutable 返回当前可执行文件路径。抽成变量以便测试注入 ——
// 否则每个用例都会把 `.new` 写到 `go test` 的临时二进制旁边（跑完留一地垃圾，
// 且用例之间互相干扰）。与 updateHTTPClient 同一套路。
var updateExecutable = os.Executable

// updateNewFile 返回 `<exe>.new` 的绝对路径（与 exe 同目录，保证同在 C 盘/同分区
// —— 跨分区的替换不是原子操作）。
func updateNewFile() (string, error) {
	exe, err := updateExecutable()
	if err != nil {
		return "", err
	}
	return exe + updateNewSuffix, nil
}

// updateHelperPath 返回替换脚本的绝对路径（与 exe 同目录）。
func updateHelperPath() (string, error) {
	exe, err := updateExecutable()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(exe), updateHelperName), nil
}

// loadUpdateDownload 读上次的下载结果（解析失败视为空）。
func (a *app) loadUpdateDownload() updateDownload {
	v, _ := a.store.GetKV(kvUpdateDownloadJSON)
	if strings.TrimSpace(v) == "" {
		return updateDownload{State: "none"}
	}
	var d updateDownload
	if err := json.Unmarshal([]byte(v), &d); err != nil || d.State == "" {
		return updateDownload{State: "none"}
	}
	return d
}

func (a *app) saveUpdateDownload(d updateDownload) {
	if blob, err := json.Marshal(d); err == nil {
		_ = a.store.SetKV(kvUpdateDownloadJSON, string(blob))
	}
}

// updateDownloadMu 保证同一时刻只有一次下载（与清单拉取的单飞同理：
// 多个页面同时打开不该把同一个 20MB 的产物下 5 份）。
var updateDownloadMu sync.Mutex

// updateTriggerMu / updateTriggeringFor 防"每次页面刷新都起一个下载 goroutine"。
//
// 与 updateDownloadMu 是两件事：后者保证**只有一个在真的下**，前者保证
// **只有一个 goroutine 被起出来**。少了它，一个开着自动刷新的面板会持续
// 堆积等锁的 goroutine（每个拿到锁后立刻发现已下好而返回 —— 结果是白起）。
var (
	updateTriggerMu     sync.Mutex
	updateTriggeringFor string // 正在下载的版本；空 = 没有
)

// updateNewFileExists 判断 `.new` 产物是否还在（可能被应用或人工清理）。
func updateNewFileExists() bool {
	p, err := updateNewFile()
	if err != nil {
		return false
	}
	fi, err := os.Stat(p)
	return err == nil && fi.Size() > 0
}

// downloadUpdate 下载并校验更新产物。
//
// 返回的 updateDownload 无论成功失败都可直接给前端展示 —— 失败时 State=failed
// 且 Error 有说明，**不清空已有的成功结果**（与 checkUpdate 保留上次好清单同理）。
//
// 校验纪律（三条都不可放宽）：
//  1. 只接受 http/https（file:// 之类会被 url.Parse 放行，必须显式拒）；
//  2. 清单**必须**给出 sha256 —— 拿不到摘要的二进制一律不下、不落盘。
//     这一条挡的是"清单被换成指向任意文件"：没有摘要就无法判断下到的是什么；
//  3. 下载到临时文件、校验通过后才 rename 成 `.new`。绝不先落最终路径再校验
//     —— 那会出现"校验失败的残留文件被替换脚本捡起来"。
func (a *app) downloadUpdate(ctx context.Context, st updateStatus) updateDownload {
	prev := a.loadUpdateDownload()

	asset := st.Asset
	if asset == nil {
		// 清单已给最新版本但没给本机平台的产物：明确说出来，别让界面以为是"正在准备"。
		if st.Latest != "" {
			return updateDownload{State: "failed", Version: st.Latest,
				Error: "清单未提供本机平台（" + runtime.GOOS + "/" + runtime.GOARCH + "）的下载产物"}
		}
		return updateDownload{State: "none"}
	}

	u, err := url.Parse(strings.TrimSpace(asset.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return updateDownload{State: "failed", Version: st.Latest,
			Error: "下载地址非法（只接受 http/https）"}
	}
	sha := strings.ToLower(strings.TrimSpace(asset.SHA256))
	if sha == "" {
		return updateDownload{State: "failed", Version: st.Latest,
			Error: "清单未提供 sha256，拒绝下载（无法校验的二进制不落盘）"}
	}
	if len(sha) != 64 {
		return updateDownload{State: "failed", Version: st.Latest,
			Error: "sha256 长度非法（期望 64 位十六进制）"}
	}
	if asset.Size > updateDownloadMaxBytes {
		return updateDownload{State: "failed", Version: st.Latest,
			Error: fmt.Sprintf("清单声明的体积 %d 超过上限 %d", asset.Size, updateDownloadMaxBytes)}
	}

	updateDownloadMu.Lock()
	defer updateDownloadMu.Unlock()

	// 已经下好同一个版本就不重复下（面板每次刷新都会调一次）。
	if prev.State == "done" && prev.Version == st.Latest && prev.SHA256 == sha {
		if p, err := updateNewFile(); err == nil {
			if fi, err := os.Stat(p); err == nil && fi.Size() > 0 {
				return prev
			}
		}
		// 文件不在了（被清理/被应用）→ 继续往下重新下载。
	}

	d := updateDownload{State: "downloading", Version: st.Latest,
		Size: asset.Size, SHA256: sha}
	a.saveUpdateDownload(d) // 先落"进行中"，避免前台刷新看到旧的成功态

	ctx, cancel := context.WithTimeout(ctx, updateDownloadTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.URL, nil)
	if err != nil {
		return a.failDownload(d, "构造请求失败："+err.Error())
	}
	req.Header.Set("User-Agent", "workbuddy2api/"+version)
	resp, err := updateHTTPClient.Do(req)
	if err != nil {
		return a.failDownload(d, "下载失败："+err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return a.failDownload(d, fmt.Sprintf("下载失败：HTTP %d", resp.StatusCode))
	}

	newPath, err := updateNewFile()
	if err != nil {
		return a.failDownload(d, "定位落盘路径失败："+err.Error())
	}
	tmpPath := newPath + ".tmp"
	tmp, err := os.Create(tmpPath)
	if err != nil {
		return a.failDownload(d, "创建临时文件失败："+err.Error())
	}

	// io.LimitReader 多读 1 字节：用来**发现**超限（等于上限说明可能还有更多）。
	// 直接按上限截断会让"下了一半的 exe"被当成完整产物。
	limited := io.LimitReader(resp.Body, updateDownloadMaxBytes+1)
	hasher := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(tmp, hasher), limited)
	closeErr := tmp.Close()

	if copyErr != nil {
		_ = os.Remove(tmpPath)
		return a.failDownload(d, "写出失败："+copyErr.Error())
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		return a.failDownload(d, "关闭文件失败："+closeErr.Error())
	}
	if n > updateDownloadMaxBytes {
		_ = os.Remove(tmpPath)
		return a.failDownload(d, fmt.Sprintf("产物超过上限 %d 字节，已丢弃", updateDownloadMaxBytes))
	}
	if n == 0 {
		_ = os.Remove(tmpPath)
		return a.failDownload(d, "产物为空（0 字节）")
	}
	if asset.Size > 0 && n != asset.Size {
		_ = os.Remove(tmpPath)
		return a.failDownload(d, fmt.Sprintf("体积不符：清单声明 %d，实际 %d", asset.Size, n))
	}

	got := hex.EncodeToString(hasher.Sum(nil))
	if got != sha {
		_ = os.Remove(tmpPath)
		return a.failDownload(d, "sha256 校验不通过（产物与清单声明不一致，已丢弃）")
	}

	// 校验通过才落到最终路径。先删旧的 `.new`（重命名在 Windows 上不覆盖已存在文件）。
	_ = os.Remove(newPath)
	if err := os.Rename(tmpPath, newPath); err != nil {
		_ = os.Remove(tmpPath)
		return a.failDownload(d, "落盘失败："+err.Error())
	}

	done := updateDownload{
		State: "done", Version: st.Latest,
		Size: asset.Size, GotSize: n, SHA256: sha,
		File: filepath.Base(newPath), At: time.Now().Unix(),
	}
	done.ApplyHint = a.updateApplyHint()
	a.saveUpdateDownload(done)
	log.Printf("INFO: [update] 已下载 %s 产物（%d 字节，sha256 校验通过），待应用：%s",
		st.Latest, n, filepath.Base(newPath))
	return done
}

// failDownload 统一收尾失败：**保留上一次的成功结果**，只把失败标出来。
//
// 为什么要保留：下载失败（网络抖动）不该让界面从"已备好新版本"退回"什么都没有"
// —— 那会让运维以为本地没有可用产物，转而去手工下载。
func (a *app) failDownload(d updateDownload, msg string) updateDownload {
	log.Printf("WARN: [update] 产物下载失败：%s", msg)
	d.State, d.Error = "failed", msg
	a.saveUpdateDownload(d)
	return d
}

// updateApplyHint 「应用更新」的操作指引（人话，可直接粘贴）。
//
// 不直接给出自动替换，理由见文件头。这里给的是**确定性**的两条命令：
// 先停服务（运维已知自己的启动方式），再执行生成的脚本。
func (a *app) updateApplyHint() string {
	exe, err := updateExecutable()
	if err != nil {
		return ""
	}
	helper, err := updateHelperPath()
	if err != nil {
		return ""
	}
	return "1) 停止本程序；2) 运行 " + helper + "（它会把 " + filepath.Base(exe) + updateNewSuffix +
		" 替换为 " + filepath.Base(exe) + " 并按原参数重新启动）。脚本在产物校验通过后生成。"
}

// handleAdminUpdateApply 「应用更新」端点（POST /api/admin/update-apply）。
//
// 只做两件事：**校验产物确实已下载并校验通过** → 生成替换脚本 → 回操作指引。
// 刻意**不**自动替换、不自动重启，理由见本文件头部。
//
// 为什么还要生成脚本而不是只给一段说明：替换要"先退进程再覆盖"，手写这段
// 容易漏掉备份与失败回滚；脚本把"备份 → 覆盖 → 失败回滚 → 重启"四步固定下来。
func (a *app) handleAdminUpdateApply(w http.ResponseWriter, r *http.Request) {
	d := a.loadUpdateDownload()
	if d.State != "done" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":    "当前没有已就绪的更新产物（先触发一次「检测更新」并等待下载完成）",
			"download": d,
		})
		return
	}
	if !updateNewFileExists() {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":    "产物文件已不在（可能已被应用或人工清理），请重新检测更新",
			"download": d,
		})
		return
	}
	helper, err := a.writeUpdateHelper()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	d.ApplyHint = a.updateApplyHint()
	a.saveUpdateDownload(d)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		// 回相对文件名而不是绝对路径：面板要展示、会进截图与日志，
		// 绝对路径会泄露部署目录结构。
		"script":     filepath.Base(helper),
		"version":    d.Version,
		"apply_hint": d.ApplyHint,
		"download":   d,
	})
}

// writeUpdateHelper 生成替换脚本（幂等）。
//
// 脚本内容刻意**不做任何猜测**：只做"备份旧 exe → 用 .new 覆盖 → 重启"三步，
// 且每一步失败都停下来并打印原因。重启用的参数从脚本内联的当前启动参数来
// —— 运维若是用 bat/服务方式启动，按提示改一行即可（不试图自动识别启动方式：
// 猜错会以"更新后服务起不来"的形式出现，代价远大于省下的那一行手工编辑）。
func (a *app) writeUpdateHelper() (string, error) {
	helper, err := updateHelperPath()
	if err != nil {
		return "", err
	}
	newPath, err := updateNewFile()
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(newPath); err != nil {
		return "", fmt.Errorf("尚未下载到可应用的产物（%s）", filepath.Base(newPath))
	}
	exe, err := updateExecutable()
	if err != nil {
		return "", err
	}
	base := filepath.Base(exe)
	// **把当前进程的启动参数原样写进重启行**（v0.27.0 修的真缺陷）。
	//
	// 此前这里只有 `start "" "webapp.exe" %*`，而 `%*` 是**脚本自己**收到的参数
	// ——双击运行时它是空的。于是重启后程序用默认参数启动：监听地址变回默认、
	// 数据库指向默认路径，表现是"替换成功、服务却起不来/数据不对"，
	// 极易被误判成端口占用或防火墙。
	//
	// 注意 updateApplyHint 早就写着"按原参数重新启动" —— 文案与实现不一致本身
	// 就是最难发现的一类缺陷：它把排查方向直接引偏。
	restart := "start \"\" \"" + base + "\""
	if args := updateArgs(); len(args) > 0 {
		quoted := make([]string, 0, len(args))
		for _, a := range args {
			quoted = append(quoted, quoteBatArg(a))
		}
		restart += " " + strings.Join(quoted, " ")
	}
	// 末尾保留 %*：手工在命令行追加参数（如临时换端口）时仍然生效。
	restart += " %*"

	content := "@echo off\r\n" +
		// chcp 必须在**任何中文输出之前**：否则默认代码页（GBK）下中文注释会被
		// 显示成乱码 —— 运维打开脚本第一眼就是一团乱码，会直接怀疑脚本坏了。
		"chcp 65001 >nul\r\n" +
		"rem WorkBuddy2API 更新替换脚本（由程序生成，可自行修改）\r\n" +
		"rem 用法：先停止本程序，再运行本脚本。\r\n" +
		"cd /d \"%~dp0\"\r\n" +
		"if not exist \"" + base + updateNewSuffix + "\" (\r\n" +
		"  echo [错误] 未找到 " + base + updateNewSuffix + "，请先在后台\"检测更新\"触发下载。\r\n" +
		"  pause & exit /b 1\r\n" +
		")\r\n" +
		"if exist \"" + base + ".bak\" del /q \"" + base + ".bak\"\r\n" +
		"move /y \"" + base + "\" \"" + base + ".bak\" >nul || (\r\n" +
		"  echo [错误] 无法重命名 " + base + "（请确认程序已完全退出）。\r\n" +
		"  pause & exit /b 1\r\n" +
		")\r\n" +
		"move /y \"" + base + updateNewSuffix + "\" \"" + base + "\" >nul || (\r\n" +
		"  echo [错误] 替换失败，已把原文件改回 " + base + ".bak。\r\n" +
		"  move /y \"" + base + ".bak\" \"" + base + "\" >nul\r\n" +
		"  pause & exit /b 1\r\n" +
		")\r\n" +
		"echo 替换完成，正在启动...\r\n" +
		"rem 下面这行由程序按**当前启动参数**生成，重启后监听地址与数据库路径与更新前一致。\r\n" +
		restart + "\r\n" +
		// **刻意不写 `timeout /t 2`**（v0.27.0 实测发现）：它在非交互环境里
		//（任务计划程序、CI、脚本被其它程序调用）会等待输入而**永久挂住**，
		// 脚本永远不退出、调用方一直等。start 本身是异步的，脚本退出不影响
		// 新进程 —— 那段等待纯属多余。
		"\r\n"
	if err := os.WriteFile(helper, []byte(content), 0o644); err != nil {
		return "", err
	}
	return helper, nil
}

// quoteBatArg 把单个启动参数转成 bat 里安全的形态。
//
// 规则（够用且不过度）：含空白或 cmd 元字符时用双引号包裹；内部的双引号本身
// 无法在 bat 中可靠转义，改为**去掉**并在日志里留痕 —— 静默改写参数会让"重启后
// 行为不一致"变成一桩无头案，所以宁可少一个引号也不要坏掉整行。
func quoteBatArg(a string) string {
	if a == "" {
		return `""`
	}
	if !strings.ContainsAny(a, " \t\"&|<>^()%!") {
		return a
	}
	if strings.Contains(a, `"`) {
		log.Printf("WARN: [update] 启动参数含双引号，已在替换脚本中移除：%s", a)
		a = strings.ReplaceAll(a, `"`, "")
	}
	return `"` + a + `"`
}
