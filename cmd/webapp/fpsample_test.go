// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/upstream"
)

// useTempSampleFile 把样本文件指向临时目录并清空进程内去重表。
//
// 去重表是包级变量（进程内去重是有意的：同一句话反复被拦不该刷满文件），
// 所以用例之间必须显式重置，否则会互相干扰。
func useTempSampleFile(t *testing.T) string {
	t.Helper()
	old := fpSampleFile
	p := filepath.Join(t.TempDir(), "fp.jsonl")
	fpSampleFile = p
	fpSampleDedup.Lock()
	fpSampleDedup.seen = map[string]bool{}
	fpSampleDedup.Unlock()
	t.Cleanup(func() { fpSampleFile = old })
	return p
}

func TestExtractFingerprintSample(t *testing.T) {
	// 字符串形态的 system + 扁平工具名。
	sys, tools := extractFingerprintSample([]byte(`{
		"messages":[{"role":"user","content":"用户内容不该被采"},
		            {"role":"system","content":"You are Claude Code, Anthropic's official CLI"},
		            {"role":"system","content":"第二个 system 不该覆盖第一个"}],
		"tools":[{"name":"bash"},{"function":{"name":"read_file"}},{"nope":1}]
	}`))
	if !strings.HasPrefix(sys, "You are Claude Code") {
		t.Fatalf("应取第一条 system，实得 %q", sys)
	}
	if strings.Contains(sys, "用户内容") {
		t.Fatal("绝不能采到 user 消息内容（那是下游用户的隐私）")
	}
	if len(tools) != 2 || tools[0] != "bash" || tools[1] != "read_file" {
		t.Fatalf("工具名解析不对：%v", tools)
	}

	// 多段形态（数组 content）。
	sys2, _ := extractFingerprintSample([]byte(`{"messages":[{"role":"system","content":[
		{"type":"text","text":"Main branch ("},{"type":"text","text":" more"}]}]}`))
	if sys2 != "Main branch ( more" {
		t.Fatalf("多段 content 拼接不对：%q", sys2)
	}

	// 无 system / 非法 JSON / 空体 → 空样本（不 panic、不误采）。
	for _, in := range []string{`{"messages":[{"role":"user","content":"x"}]}`, `not json`, ``} {
		if s, tl := extractFingerprintSample([]byte(in)); s != "" || len(tl) != 0 {
			t.Fatalf("输入 %q 应得空样本，实得 (%q,%v)", in, s, tl)
		}
	}

	// 超长截断到 300 字符（按 rune 计）。
	long, _ := extractFingerprintSample([]byte(`{"messages":[{"role":"system","content":"` +
		strings.Repeat("字", 500) + `"}]}`))
	if len([]rune(long)) != fpSampleSystemChars {
		t.Fatalf("应截断到 %d 字符，实得 %d", fpSampleSystemChars, len([]rune(long)))
	}
}

// TestFingerprintSampleOffByDefault 默认**一个字都不落盘**。
//
// 这条是隐私承诺的闸门：本仓对外说过「请求体内容不留存」，
// 采样是本仓唯一会写请求内容的功能，因此默认必须关闭。
func TestFingerprintSampleOffByDefault(t *testing.T) {
	path := useTempSampleFile(t)
	store, px, _ := newTestApp(t, &Account{UID: "u1", AccessToken: "t"})
	defer store.Close()
	defer px.Close()

	// 不设任何 kv（= 默认 off）。
	body := []byte(`{"messages":[{"role":"system","content":"You are Claude Code"}]}`)
	px.maybeSampleFingerprint(upstream.ErrChannelDenied, "u1", "m", body, []byte(`{"code":11128}`))
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("默认关闭时不该产生样本文件（err=%v）", err)
	}
}

// TestFingerprintSampleDedupAndView 开启后落样本、同模式去重、接口可读。
func TestFingerprintSampleDedupAndView(t *testing.T) {
	path := useTempSampleFile(t)
	store, px, a := newTestApp(t, &Account{UID: "u1", AccessToken: "t"})
	defer store.Close()
	defer px.Close()
	if err := store.SetKV(kvFingerprintSamples, "on"); err != nil {
		t.Fatal(err)
	}
	if !px.fingerprintSamplesEnabled() {
		t.Fatal("设成 on 后应视为已开启")
	}

	body := []byte(`{"messages":[{"role":"system","content":"You are Claude Code, Anthropic's official CLI"}],
		"tools":[{"name":"bash"}]}`)
	// 同一模式连发 5 次：只应留 1 条（否则文件会被同一句话刷满、反而掩盖新样本）。
	for i := 0; i < 5; i++ {
		px.maybeSampleFingerprint(upstream.ErrChannelDenied, "u1", "m", body, []byte(`{"code":11128}`))
	}
	// 另一种模式（system 不同）应再留 1 条。
	other := []byte(`{"messages":[{"role":"system","content":"Main branch ("}]}`)
	px.maybeSampleFingerprint(upstream.ErrContentBlocked, "u1", "m", other, []byte(`blocked by security policy`))

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("样本文件应已创建：%v", err)
	}
	lines := 0
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(l) != "" {
			lines++
		}
	}
	if lines != 2 {
		t.Fatalf("去重后应留 2 行，实得 %d（内容：%s）", lines, string(raw))
	}

	// 无关分类不采样（这不是"记所有错误"，只记指纹/内容被拦）。
	before := lines
	px.maybeSampleFingerprint(upstream.ErrServer, "u1", "m", other, nil)
	if b2, _ := os.ReadFile(path); strings.Count(string(b2), "\n") != before {
		t.Fatal("ErrServer 不该产生样本")
	}

	// 接口：能读出来，且带上分类计数。
	rec := httptest.NewRecorder()
	a.handleFingerprintSamples(rec, httptest.NewRequest(http.MethodGet, "/api/admin/fingerprint-samples", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d：%s", rec.Code, rec.Body.String())
	}
	var v fingerprintSamplesView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("解析：%v", err)
	}
	if !v.Enabled || v.Count != 2 || len(v.Samples) != 2 {
		t.Fatalf("视图不符：enabled=%v count=%d samples=%d", v.Enabled, v.Count, len(v.Samples))
	}
	if v.Kinds["channel_denied"] != 1 || v.Kinds["content_blocked"] != 1 {
		t.Fatalf("分类计数不符：%v", v.Kinds)
	}
	// 样本里必须**只有 system 片段与工具名**，不能有 user 内容。
	for _, s := range v.Samples {
		if strings.Contains(s.System, "用户内容") {
			t.Fatal("样本里出现了 user 内容")
		}
	}
	// 空文件 / 不存在时接口回空清单而不是报错。
	_ = os.Remove(path)
	rec2 := httptest.NewRecorder()
	a.handleFingerprintSamples(rec2, httptest.NewRequest(http.MethodGet, "/api/admin/fingerprint-samples", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("文件不存在时应回 200 空清单，实得 %d", rec2.Code)
	}
}

// TestFingerprintSampleSkipsEmptySample 取不到样本时不写空行污染文件。
func TestFingerprintSampleSkipsEmptySample(t *testing.T) {
	path := useTempSampleFile(t)
	store, px, _ := newTestApp(t, &Account{UID: "u1", AccessToken: "t"})
	defer store.Close()
	defer px.Close()
	if err := store.SetKV(kvFingerprintSamples, "on"); err != nil {
		t.Fatal(err)
	}
	px.maybeSampleFingerprint(upstream.ErrChannelDenied, "u1", "m",
		[]byte(`{"messages":[{"role":"user","content":"no system here"}]}`), nil)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("没有 system 也没有 tools 时不该写文件")
	}
	_ = time.Now()
}

// TestFingerprintSamplesViewStats 视图的统计字段（体积 / 行数 / 模型 Top / 最近时刻）
// 必须准确 —— 界面的占用条与"还能装多少"直接读它们，报错的代价是运维误判"还早"。
func TestFingerprintSamplesViewStats(t *testing.T) {
	useTempSampleFile(t)
	store, px, a := newTestApp(t, &Account{UID: "u1", AccessToken: "t"})
	defer store.Close()
	defer px.Close()
	if err := a.store.SetKV(kvFingerprintSamples, "on"); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"glm-5.3","messages":[{"role":"system","content":"S-a"}],"tools":[{"name":"bash"}]}`)
	px.maybeSampleFingerprint(upstream.ErrContentBlocked, "u1", "glm-5.3", body, nil)
	// 同模型、不同内容 → 第二条。
	body2 := []byte(`{"model":"glm-5.3","messages":[{"role":"system","content":"S-b"}],"tools":[{"name":"bash"}]}`)
	px.maybeSampleFingerprint(upstream.ErrChannelDenied, "u1", "glm-5.3", body2, nil)

	rec := httptest.NewRecorder()
	a.handleFingerprintSamples(rec, httptest.NewRequest(http.MethodGet, "/api/admin/fingerprint-samples", nil))
	var v fingerprintSamplesView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.Max != fpSampleFileMaxBytes {
		t.Errorf("max = %d，期望 %d", v.Max, fpSampleFileMaxBytes)
	}
	if v.Size <= 0 || v.Lines != 2 {
		t.Errorf("size/lines 不符：size=%d lines=%d", v.Size, v.Lines)
	}
	if v.Full {
		t.Error("未到上限时 full 应为 false")
	}
	if v.LastAt <= 0 {
		t.Error("last_at 应被回填")
	}
	if len(v.Models) != 1 || v.Models[0].Name != "glm-5.3" || v.Models[0].Count != 2 {
		t.Errorf("模型分布不符：%+v", v.Models)
	}
}

// TestClearFingerprintSamples 清空：删文件 + 重置去重表（否则"清空后文件一直是空的"）。
func TestClearFingerprintSamples(t *testing.T) {
	path := useTempSampleFile(t)
	store, px, a := newTestApp(t, &Account{UID: "u1", AccessToken: "t"})
	defer store.Close()
	defer px.Close()
	if err := a.store.SetKV(kvFingerprintSamples, "on"); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"messages":[{"role":"system","content":"S"}],"tools":[{"name":"bash"}]}`)
	px.maybeSampleFingerprint(upstream.ErrContentBlocked, "u1", "m", body, nil)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("样本文件应存在：%v", err)
	}

	rec := httptest.NewRecorder()
	a.handleFingerprintSamplesClear(rec, httptest.NewRequest(http.MethodDelete, "/api/admin/fingerprint-samples", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("清空应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("文件应被删除，stat err=%v", err)
	}
	// 去重表必须一并重置：同一条样本应能重新写回。
	px.maybeSampleFingerprint(upstream.ErrContentBlocked, "u1", "m", body, nil)
	raw, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(raw)) == "" {
		t.Fatalf("清空后同一条样本应能重新写回：err=%v raw=%q", err, string(raw))
	}

	// 幂等：再清一次（文件已不存在）仍应成功。
	rec2 := httptest.NewRecorder()
	a.handleFingerprintSamplesClear(rec2, httptest.NewRequest(http.MethodDelete, "/api/admin/fingerprint-samples", nil))
	if rec2.Code != http.StatusOK {
		t.Errorf("重复清空应幂等成功，得 %d", rec2.Code)
	}
}

// TestTopFpCountsOrdering 数量相同时按名字升序 —— map 遍历无序，不排序会让界面每次刷新都跳。
func TestTopFpCountsOrdering(t *testing.T) {
	got := topFpCounts(map[string]int{"b": 2, "a": 2, "c": 5, "d": 1}, 3)
	want := []fpCount{{"c", 5}, {"a", 2}, {"b", 2}}
	if len(got) != len(want) {
		t.Fatalf("got=%+v want=%+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 项 = %+v，期望 %+v", i, got[i], want[i])
		}
	}
}
