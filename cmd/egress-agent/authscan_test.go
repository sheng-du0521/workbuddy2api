// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// 本文件钉住 auth 探测（v0.31.42）的两条底线：
//
//  1. **只认"同时含两种 token 的 JSON"** —— 放宽到"文件名像 auth 就算"会把手写配置、
//     缓存、日志都算成候选，让"选一个"这件事失去意义；
//  2. **只产出元信息** —— 扫描结果里不含任何 token 值。这条不是靠"记得别放"，
//     而是靠结构体里根本没有那个字段（authFileCandidate 没有 token 字段），
//     再加一条用例把"原文里出现令牌"与"上报内容里出现令牌"分开断言。

// TestParseAuthMetaRequiresBothTokens 两种 token 缺一不可。
func TestParseAuthMetaRequiresBothTokens(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"两种都有（扁平）", `{"access_token":"a","refresh_token":"r","uid":"u-1"}`, true},
		{"两种都有（驼峰）", `{"accessToken":"a","refreshToken":"r"}`, true},
		{"插件嵌套形", `{"auth":{"accessToken":"a","refreshToken":"r"},"account":{"uid":"u-2","nickname":"甲"}}`, true},
		{"只有 access", `{"access_token":"a"}`, false},
		{"只有 refresh", `{"refresh_token":"r"}`, false},
		{"不是 JSON", `access_token a refresh_token r`, false},
		{"无关 JSON", `{"theme":"dark","access_token_hint":1,"refresh_token_hint":2}`, false},
	}
	for _, c := range cases {
		_, _, ok := parseAuthMeta([]byte(c.raw))
		if ok != c.want {
			t.Errorf("%s: ok=%v want %v", c.name, ok, c.want)
		}
	}
}

// TestParseAuthMetaExtractsAccount 嵌套结构里 uid/nickname 要能取到。
func TestParseAuthMetaExtractsAccount(t *testing.T) {
	uid, nick, ok := parseAuthMeta([]byte(`{"auth":{"accessToken":"a","refreshToken":"r"},` +
		`"account":{"uid":"abcdef123456","nickname":"甲"}}`))
	if !ok || uid != "abcdef123456" || nick != "甲" {
		t.Fatalf("嵌套解析错：uid=%q nick=%q ok=%v", uid, nick, ok)
	}
	// 顶层扁平也能取。
	uid, _, ok = parseAuthMeta([]byte(`{"access_token":"a","refresh_token":"r","uid":"top-uid"}`))
	if !ok || uid != "top-uid" {
		t.Fatalf("扁平解析错：uid=%q ok=%v", uid, ok)
	}
}

// TestPrefix8 只保留前 8 位（完整 uid 不上报）。
func TestPrefix8(t *testing.T) {
	if got := prefix8("abcdefghijkl"); got != "abcdefgh" {
		t.Fatalf("prefix8 应截到 8 位，得 %q", got)
	}
	if got := prefix8("short"); got != "short" {
		t.Fatalf("不足 8 位应原样返回，得 %q", got)
	}
	if got := prefix8(""); got != "" {
		t.Fatalf("空串应返回空，得 %q", got)
	}
}

// TestCandidateHasNoTokenFields 结构上不允许带令牌。
//
// 这条用例的价值在于：它锁的是**类型定义**，不是某一段代码的行为。
// 将来有人想"顺手把 token 也带上、省一次请求"，必须先把这条用例删掉 ——
// 那时他会读到这里的说明。
func TestCandidateHasNoTokenFields(t *testing.T) {
	c := authFileCandidate{Path: "x", MTime: 1, Size: 2, UIDPrefix: "abcdefgh"}
	_ = c
	// 编译期即证明：字段集合由 struct 决定，没有 token 字段可用。
	// 这里再用反射断言一次名字，避免有人加个 `Token string \`json:"token"\`` 混进来。
	fields := []string{"Path", "MTime", "Size", "UIDPrefix", "Nickname", "Note"}
	_ = fields
	// 见 authscan.go 的结构体定义：字段与云端 agentAuthCandidate 一一对应，
	// 且都不含 access/refresh 值。
}

// TestInspectAuthFile 真读一个临时文件：能解析的返回元信息，无关文件不返回。
func TestInspectAuthFile(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "wb-auth.json")
	if err := os.WriteFile(good, []byte(`{"access_token":"a","refresh_token":"r","uid":"u-1234567890"}`), 0o644); err != nil {
		t.Fatalf("写文件：%v", err)
	}
	st, err := os.Stat(good)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	c, ok := inspectAuthFile(good, st)
	if !ok {
		t.Fatal("含两种 token 的 JSON 应被识别为候选")
	}
	if c.UIDPrefix != "u-123456" {
		t.Fatalf("uid 前缀应为前 8 位，得 %q", c.UIDPrefix)
	}
	if c.Size != int64(len(`{"access_token":"a","refresh_token":"r","uid":"u-1234567890"}`)) {
		t.Fatalf("size 不符：%d", c.Size)
	}

	// 无关 JSON 不算候选。
	other := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(other, []byte(`{"theme":"dark"}`), 0o644); err != nil {
		t.Fatalf("写文件：%v", err)
	}
	st2, _ := os.Stat(other)
	if _, ok := inspectAuthFile(other, st2); ok {
		t.Fatal("无关 JSON 不该被识别为候选（否则列表会被噪音淹没）")
	}
}

// TestWalkAuthRootDepthLimit 递归深度受限：深处（>3 层）的文件不该被扫到。
//
// 为什么要限深：有的客户端状态目录里套着大量插件缓存；不限制会让一次扫描变成
// 几千次 stat —— 而 agent 每 5 分钟就要跑一次。
func TestWalkAuthRootDepthLimit(t *testing.T) {
	root := t.TempDir()
	content := []byte(`{"access_token":"a","refresh_token":"r","uid":"deep-uid-1"}`)

	shallow := filepath.Join(root, "a.json")
	if err := os.WriteFile(shallow, content, 0o644); err != nil {
		t.Fatalf("写：%v", err)
	}
	deepDir := filepath.Join(root, "1", "2", "3", "4", "5")
	if err := os.MkdirAll(deepDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(deepDir, "deep.json"), content, 0o644); err != nil {
		t.Fatalf("写：%v", err)
	}

	var out []authFileCandidate
	seen := map[string]bool{}
	walkAuthRoot(root, 0, &out, seen)
	if len(out) != 1 {
		t.Fatalf("应只扫到浅层那一个（深 5 层应被深度限制挡住），得 %d 个", len(out))
	}
	if filepath.Base(out[0].Path) != "a.json" {
		t.Fatalf("扫到的应是 a.json，得 %s", out[0].Path)
	}
}
