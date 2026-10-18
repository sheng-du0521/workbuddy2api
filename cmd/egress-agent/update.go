// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// agent_update.go agent 自更新（v0.31.42）。
//
// ## 为什么必须做这条路
//
// 前面几版给 agent 加了不少能力（指纹、端口修复、候选探测），但**一个都到不了**
// 用户机器上：现网 agent 还停在 0.1.0，因为升级只能靠人把新 exe 拷过去替换，
// 而那个文件在被占用时根本替换不掉。于是"服务端修好了、客户端还是老的"成了常态。
//
// 现在 agent 自己检查版本、自己下载、自己换文件、自己重启。第一次仍需人工换成
// 带本功能的版本（老版本不会来问版本），**从那一次之后就不再需要人了**。
//
// ## 安全边界（都不是可选项）
//
//   - 下载地址与哈希**只从我们自己的服务端取**（`/api/agent/version`），
//     且该端点要 agent 令牌 —— 不让公网随便探测版本，也不让 agent 从第三方地址装东西。
//   - **校验 sha256 与大小**，不一致直接丢弃（否则一次中间人就能在用户机器上跑任意代码）。
//   - 换文件三步：`旧 exe → .old`、`新 exe → 旧名`、起新进程退出自己。
//     Windows 允许重命名**正在运行**的可执行文件（只是不能删），所以这三步不需要重启系统。
//   - 任一步失败 → 保留原状继续跑（绝不出现"agent 没了"）。
//   - `--no-update` 可关；配置里也能关（`"auto_update": false`）。
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// updateCheckInterval 检查版本的最小间隔。
//
// 6 小时：agent 常驻数月，一天 4 次空请求可以忽略；而"发版后几小时内自动生效"
// 对运维体验足够（不需要秒级）。
const updateCheckInterval = 6 * time.Hour

// agentUpdateInfo 服务端下发的版本信息。
type agentUpdateInfo struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
	Notes   string `json:"notes,omitempty"`
	// Disabled 服务端显式关闭自动更新（例如回滚期间）。
	Disabled bool `json:"disabled,omitempty"`
}

// maybeSelfUpdate 检查并执行一次自更新。**成功时会替换进程并退出**，不会返回。
//
// 返回值只在"没更新/更新失败"时出现：err 用于日志（绝不因为更新失败而停掉 agent）。
func (a *agent) maybeSelfUpdate() error {
	if a.noUpdate {
		return nil
	}
	info, err := a.updateInfo()
	if err != nil {
		return err
	}
	if info.Disabled || strings.TrimSpace(info.Version) == "" || info.Version == agentVersion {
		return nil
	}
	if strings.TrimSpace(info.URL) == "" || strings.TrimSpace(info.SHA256) == "" {
		return fmt.Errorf("版本信息不完整（需要 url 与 sha256）")
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("取自身路径失败：%w", err)
	}
	dir := filepath.Dir(exe)
	next := filepath.Join(dir, "agent.next"+exeSuffix())
	if err := downloadTo(a.server+info.URL, a.token, next, info.SHA256, info.Size); err != nil {
		return fmt.Errorf("下载新版本失败：%w", err)
	}
	// 换文件：先把自己改名（Windows 允许），再把新文件顶上。
	//
	// 为什么不覆盖写：正在运行的 exe 在 Windows 上被锁，覆盖会失败；
	// 而重命名只改目录项，系统不拦。
	old := exe + ".old"
	_ = os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		return fmt.Errorf("重命名旧版本失败（%w）—— 保持原状继续运行", err)
	}
	if err := os.Rename(next, exe); err != nil {
		// 回滚：把旧名字改回来，保证下一次还能被系统/用户启动。
		_ = os.Rename(old, exe)
		return fmt.Errorf("新版本就位失败（%w）—— 已回滚", err)
	}
	log.Printf("已更新到 %s（旧文件保留为 %s），正在重启…", info.Version, filepath.Base(old))
	// 起新进程：**不带**当前参数里的 --no-update 之外的东西，原样继承（见 main）。
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// 脱离当前会话：父进程退出后子进程继续跑（Windows 上 Start 即脱离）。
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动新版本失败（%w）—— 新文件已就位，下次开机/手动启动会生效", err)
	}
	os.Exit(0)
	return nil
}

// exeSuffix 本平台可执行文件后缀（测试在非 Windows 上也能跑）。
func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// updateInfo 取服务端下发的最新版本信息。
func (a *agent) updateInfo() (agentUpdateInfo, error) {
	var info agentUpdateInfo
	err := a.get("/api/agent/version", &info)
	return info, err
}

// downloadTo 下载到 dst 并校验 sha256/size；任何不一致都删文件并报错。
func downloadTo(url, token, dst, wantSHA string, wantSize int64) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Agent-Token", token)
	cl := &http.Client{Timeout: 10 * time.Minute}
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), resp.Body)
	cerr := f.Close()
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if cerr != nil {
		_ = os.Remove(tmp)
		return cerr
	}
	if wantSize > 0 && n != wantSize {
		_ = os.Remove(tmp)
		return fmt.Errorf("大小不符：期望 %d，实际 %d", wantSize, n)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, wantSHA) {
		_ = os.Remove(tmp)
		return fmt.Errorf("sha256 不符：期望 %s，实际 %s", wantSHA, got)
	}
	return os.Rename(tmp, dst)
}
