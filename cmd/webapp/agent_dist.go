// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// agent_dist.go 下发 agent 产物（v0.31.42）。
//
// ## 为什么要服务端自己发
//
// agent 的自更新（cmd/egress-agent/update.go）需要两个东西：最新版本号 + 可下载的
// 二进制 + sha256。这三样都可以从 GitHub Release 拿，但：
//   - 用户机器常常连不上 GitHub（国内网络，本机也一样）；
//   - 让 agent 从第三方地址装可执行文件，等于把"代码执行"这件事外包出去。
//
// 所以二进制由**我们自己的服务端**发（就走他那条已经连通的 8780），版本清单放在
// 二进制旁边的 `agent-version.json` 里（部署时写，见部署脚本）。
//
// ## 没有清单时怎么办
//
// **整条能力关闭**（`disabled: true`），而不是猜一个版本号。猜错的后果是什么都不发生
// 却让人以为"自动更新已经启用"，而这条链路的失败是静默的（agent 只会打一行日志）。
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// agentManifestFilename 版本清单文件名（与二进制同目录）。
const agentManifestFilename = "agent-version.json"

// agentManifest 部署时写下的清单。
type agentManifest struct {
	Version string `json:"version"`
	// File 二进制文件名（同目录）。缺省 wb2api-egress-agent.exe。
	File  string `json:"file,omitempty"`
	Notes string `json:"notes,omitempty"`
}

// agentArtifact 一份可下发的产物（sha256/size 按文件缓存）。
type agentArtifact struct {
	Path   string
	SHA256 string
	Size   int64
	MTime  int64
}

var (
	agentArtifactMu    sync.Mutex
	agentArtifactCache = map[string]agentArtifact{}
)

// agentDistDir agent 产物目录。
//
// 取值顺序：`-agent-dir` 显式指定 > 可执行文件同级的 egress-agent/。
// 生产部署正是后者（/opt/wb2api/egress-agent/），本地开发则通常没有该目录 ——
// 那意味着"未启用"，由调用方按 disabled 处理。
func (a *app) agentDistDir() string {
	if a.agentDir != "" {
		return a.agentDir
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(exe), "egress-agent")
}

// agentDistArtifact 解析清单并返回可下发产物；任何一步不成立都返回 ok=false + 原因。
//
// sha256 与大小按 (路径, mtime) 缓存：几 MB 的文件每次请求都算一遍不划算，
// 而 mtime 变了必然意味着部署了新产物 —— 缓存失效条件正好对上。
func (a *app) agentDistArtifact() (agentManifest, agentArtifact, string) {
	dir := a.agentDistDir()
	if dir == "" {
		return agentManifest{}, agentArtifact{}, "未配置 agent 产物目录"
	}
	raw, err := os.ReadFile(filepath.Join(dir, agentManifestFilename))
	if err != nil {
		return agentManifest{}, agentArtifact{}, "缺少 " + agentManifestFilename + "（部署时未放置产物）"
	}
	var mf agentManifest
	if err := json.Unmarshal(raw, &mf); err != nil {
		return agentManifest{}, agentArtifact{}, "清单格式错误：" + err.Error()
	}
	if mf.Version == "" {
		return agentManifest{}, agentArtifact{}, "清单缺少 version"
	}
	name := mf.File
	if name == "" {
		name = "wb2api-egress-agent.exe"
	}
	// 防目录穿越：清单是我们自己写的，但"读清单里的路径去打开文件"这类代码
	// 一旦被人改清单就能读到任意文件。只接受纯文件名。
	if name != filepath.Base(name) || name == "." || name == ".." {
		return agentManifest{}, agentArtifact{}, "清单里的文件名不合法"
	}
	path := filepath.Join(dir, name)
	st, err := os.Stat(path)
	if err != nil {
		return agentManifest{}, agentArtifact{}, "产物不存在：" + name
	}
	agentArtifactMu.Lock()
	defer agentArtifactMu.Unlock()
	if c, ok := agentArtifactCache[path]; ok && c.MTime == st.ModTime().Unix() && c.Size == st.Size() {
		return mf, c, ""
	}
	sum, err := fileSHA256(path)
	if err != nil {
		return agentManifest{}, agentArtifact{}, "计算 sha256 失败：" + err.Error()
	}
	art := agentArtifact{Path: path, SHA256: sum, Size: st.Size(), MTime: st.ModTime().Unix()}
	agentArtifactCache[path] = art
	return mf, art, ""
}

// fileSHA256 流式计算文件哈希。
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// handleAgentVersion GET /api/agent/version
//
// agent 用它判断"要不要更新"。三件事都必须给全：版本号（比不比）、
// 相对下载地址（从哪拿）、sha256 + size（拿到之后验不验得过）。
func (a *app) handleAgentVersion(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.egressAgentAuth(r); !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid agent token"})
		return
	}
	mf, art, why := a.agentDistArtifact()
	if why != "" {
		// 不报错（agent 会把它当"暂时没有新版本"）：部署还没放产物是常态。
		writeJSON(w, http.StatusOK, map[string]any{
			"disabled": true, "reason": why,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version": mf.Version,
		"url":     "/api/agent/download",
		"sha256":  art.SHA256,
		"size":    art.Size,
		"notes":   mf.Notes,
		// checked_at 只用于排障：agent 侧不依赖它做判断。
		"checked_at": time.Now().Unix(),
	})
}

// handleAgentDownload GET /api/agent/download
//
// 直接 ServeFile（不带 Range 关闭）：几 MB 的下载，断点续传没有价值，
// 而 ServeFile 自带缓存头与大小处理，没必要自己写一遍。
func (a *app) handleAgentDownload(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.egressAgentAuth(r); !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid agent token"})
		return
	}
	_, art, why := a.agentDistArtifact()
	if why != "" {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": why})
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, filepath.Base(art.Path)))
	w.Header().Set("X-Artifact-SHA256", art.SHA256)
	http.ServeFile(w, r, art.Path)
}
