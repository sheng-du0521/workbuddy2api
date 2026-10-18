// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本文件钉住 agent 产物下发（v0.31.42）的三条边界。
//
// 为什么这三条尤其要测：这条链路**在用户机器上执行代码**。任何一处"宽容处理"
// （没清单也给个版本号、清单里的路径照读、哈希不符也继续）都会把一次失误
// 变成一次远程代码执行。

// writeAgentFixture 造一份产物 + 清单，返回 (目录, 内容, sha256)。
func writeAgentFixture(t *testing.T, version string, manifest bool) (string, []byte, string) {
	t.Helper()
	dir := t.TempDir()
	payload := []byte("FAKE-AGENT-BINARY-" + version)
	sum := sha256.Sum256(payload)
	sha := hex.EncodeToString(sum[:])
	if err := os.WriteFile(filepath.Join(dir, "wb2api-egress-agent.exe"), payload, 0o644); err != nil {
		t.Fatalf("写产物：%v", err)
	}
	if manifest {
		raw, _ := json.Marshal(map[string]any{"version": version, "notes": "test"})
		if err := os.WriteFile(filepath.Join(dir, agentManifestFilename), raw, 0o644); err != nil {
			t.Fatalf("写清单：%v", err)
		}
	}
	return dir, payload, sha
}

// TestAgentVersionRequiresToken 版本端点只认 agent 令牌（不公开探测版本）。
func TestAgentVersionRequiresToken(t *testing.T) {
	_, a, mux, _, _ := eaApp(t)
	dir, _, _ := writeAgentFixture(t, "9.9.9", true)
	a.agentDir = dir

	if code, _ := eaReq(t, mux, "GET", "/api/agent/version", "", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("无令牌应 401，得 %d", code)
	}
	if code, _ := eaReq(t, mux, "GET", "/api/agent/download", "", "", "ag-wrong"); code != http.StatusUnauthorized {
		t.Fatalf("错令牌应 401，得 %d", code)
	}
}

// TestAgentVersionDisabledWithoutManifest 没有清单 → 明确 disabled，**不猜版本**。
//
// 猜一个版本号的后果是：什么都不发生，但所有人以为自动更新已经启用 ——
// 而这条链路的失败是静默的（agent 只会打一行日志）。
func TestAgentVersionDisabledWithoutManifest(t *testing.T) {
	_, a, mux, _, _ := eaApp(t)
	dir, _, _ := writeAgentFixture(t, "9.9.9", false)
	a.agentDir = dir
	tok, err := a.egressAgentTokenGet()
	if err != nil {
		t.Fatalf("token: %v", err)
	}

	code, body := eaReq(t, mux, "GET", "/api/agent/version", "", "", tok)
	if code != http.StatusOK {
		t.Fatalf("状态码 %d body=%s", code, body)
	}
	m := eaJSON(t, body)
	if m["disabled"] != true {
		t.Fatalf("缺清单时必须 disabled=true，得 %v", m)
	}
	if m["version"] != nil {
		t.Fatalf("缺清单时不该给出任何版本号，得 %v", m["version"])
	}
}

// TestAgentVersionAndDownload 正常路径：版本、sha256、size 与实际文件一致，下载能拿到原文。
func TestAgentVersionAndDownload(t *testing.T) {
	_, a, mux, _, _ := eaApp(t)
	dir, payload, sha := writeAgentFixture(t, "0.3.3", true)
	a.agentDir = dir
	tok, err := a.egressAgentTokenGet()
	if err != nil {
		t.Fatalf("token: %v", err)
	}

	code, body := eaReq(t, mux, "GET", "/api/agent/version", "", "", tok)
	if code != http.StatusOK {
		t.Fatalf("状态码 %d body=%s", code, body)
	}
	m := eaJSON(t, body)
	if m["version"] != "0.3.3" {
		t.Fatalf("版本应为 0.3.3，得 %v", m["version"])
	}
	if m["sha256"] != sha {
		t.Fatalf("sha256 不符：%v want %s", m["sha256"], sha)
	}
	if int64(m["size"].(float64)) != int64(len(payload)) {
		t.Fatalf("size 不符：%v want %d", m["size"], len(payload))
	}
	if m["url"] != "/api/agent/download" {
		t.Fatalf("下载地址应为相对路径 /api/agent/download，得 %v", m["url"])
	}

	if code, got := eaReq(t, mux, "GET", "/api/agent/download", "", "", tok); code != http.StatusOK || got != string(payload) {
		t.Fatalf("下载内容不符（code=%d size=%d）", code, len(got))
	}
}

// TestAgentManifestRejectsTraversal 清单里的文件名必须是纯文件名。
//
// 「读清单里的路径去打开文件」这类代码，一旦清单被改就能读到任意文件。
// 这里钉住的是：`../` 一律拒绝，且**明确 disabled** 而不是静默失败。
func TestAgentManifestRejectsTraversal(t *testing.T) {
	_, a, mux, _, _ := eaApp(t)
	dir := t.TempDir()
	raw, _ := json.Marshal(map[string]any{"version": "9.9.9", "file": "../../etc/passwd"})
	if err := os.WriteFile(filepath.Join(dir, agentManifestFilename), raw, 0o644); err != nil {
		t.Fatalf("写清单：%v", err)
	}
	a.agentDir = dir
	tok, _ := a.egressAgentTokenGet()

	code, body := eaReq(t, mux, "GET", "/api/agent/version", "", "", tok)
	if code != http.StatusOK {
		t.Fatalf("状态码 %d body=%s", code, body)
	}
	m := eaJSON(t, body)
	if m["disabled"] != true {
		t.Fatalf("穿越路径必须被拒（disabled=true），得 %v", m)
	}
	if m["version"] != nil {
		t.Fatalf("被拒时不该下发版本号，得 %v", m["version"])
	}
	// 下载端点同样不能把它端出去。
	if code, _ := eaReq(t, mux, "GET", "/api/agent/download", "", "", tok); code != http.StatusNotFound {
		t.Fatalf("穿越路径的下载应 404，得 %d", code)
	}
}

// TestAgentArtifactShaCacheInvalidatedByMtime 换了产物（mtime 变）→ 哈希必须重算。
//
// 缓存键用 (路径, mtime, size)：这一条防的是"部署了新二进制但还在发旧哈希"，
// 那样全体 agent 的自动更新会一直校验失败（而且日志里只会说 sha 不符）。
func TestAgentArtifactShaCacheInvalidatedByMtime(t *testing.T) {
	_, a, _, _, _ := eaApp(t)
	dir, _, sha1 := writeAgentFixture(t, "0.3.3", true)
	a.agentDir = dir

	_, art1, why := a.agentDistArtifact()
	if why != "" || art1.SHA256 != sha1 {
		t.Fatalf("首次解析应拿到 sha1（why=%s）", why)
	}

	// 改写产物（大小不同 → 缓存必然失效；这里显式等待 mtime 精度）。
	newPayload := []byte("FAKE-AGENT-BINARY-0.3.4-longer")
	if err := os.WriteFile(filepath.Join(dir, "wb2api-egress-agent.exe"), newPayload, 0o644); err != nil {
		t.Fatalf("改写产物：%v", err)
	}
	_, art2, why := a.agentDistArtifact()
	if why != "" {
		t.Fatalf("改写后仍应可解析：%s", why)
	}
	sum := sha256.Sum256(newPayload)
	want := hex.EncodeToString(sum[:])
	if art2.SHA256 != want {
		t.Fatalf("产物变了必须重算哈希：得 %s want %s", art2.SHA256, want)
	}
	if art2.SHA256 == art1.SHA256 {
		t.Fatal("哈希没变说明缓存失效条件写错了（会把旧哈希发给 agent）")
	}
	if !strings.HasSuffix(art2.Path, "wb2api-egress-agent.exe") {
		t.Fatalf("产物路径异常：%s", art2.Path)
	}
}
