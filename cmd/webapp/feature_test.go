// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"path/filepath"
	"testing"
	"time"
)

// ---------- 客户端维度限流 ----------

func TestClientLimiterAllowsUpToRPM(t *testing.T) {
	l := newClientLimiter()
	for i := 0; i < 3; i++ {
		if ok, _ := l.allow("key:1", 3); !ok {
			t.Fatalf("第 %d 次应放行", i+1)
		}
	}
	ok, wait := l.allow("key:1", 3)
	if ok {
		t.Fatal("超过 rpm 应被拦截")
	}
	if wait <= 0 || wait > time.Minute {
		t.Fatalf("retry-after 应在 (0, 60s]，实际 %v", wait)
	}
	if l.blocked.Load() != 1 {
		t.Fatalf("blocked=%d want 1", l.blocked.Load())
	}
	if l.allowed.Load() != 3 {
		t.Fatalf("allowed=%d want 3", l.allowed.Load())
	}
}

func TestClientLimiterZeroMeansUnlimited(t *testing.T) {
	l := newClientLimiter()
	for i := 0; i < 1000; i++ {
		if ok, _ := l.allow("key:1", 0); !ok {
			t.Fatal("rpm=0 应恒放行")
		}
	}
}

// 桶按 key 隔离：一个 Key 打满不影响另一个。
func TestClientLimiterPerKeyIsolation(t *testing.T) {
	l := newClientLimiter()
	for i := 0; i < 2; i++ {
		l.allow("key:1", 2)
	}
	if ok, _ := l.allow("key:1", 2); ok {
		t.Fatal("key:1 应已打满")
	}
	if ok, _ := l.allow("key:2", 2); !ok {
		t.Fatal("key:2 不应受 key:1 影响")
	}
}

// 窗口滑出后额度恢复（把窗口内的时间戳通过 GC 语义间接验证：
// 用极小窗口 + 手动构造过期时间戳不可行，这里用「窗口为 1 分钟」+ 直接改内部状态模拟）。
func TestClientLimiterWindowSlides(t *testing.T) {
	l := newClientLimiter()
	l.window = 50 * time.Millisecond
	if ok, _ := l.allow("k", 1); !ok {
		t.Fatal("首次应放行")
	}
	if ok, _ := l.allow("k", 1); ok {
		t.Fatal("窗口内第二次应拦截")
	}
	time.Sleep(60 * time.Millisecond)
	if ok, _ := l.allow("k", 1); !ok {
		t.Fatal("窗口滑出后应恢复")
	}
}

// ---------- 用量报表聚合 ----------

func TestUsageDailyAndModelAggregation(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "report.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	now := time.Now().Unix()
	seed := func(model string, pt, ct int, chargedMicro int64, at int64) {
		if _, err := store.db.Exec(
			`INSERT INTO usage_logs (user_id, model, prompt_tokens, completion_tokens, charged_micro, created_at)
			 VALUES (1, ?, ?, ?, ?, ?)`, model, pt, ct, chargedMicro, at); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	seed("glm-5.2", 10, 20, 2000, now)
	seed("glm-5.2", 5, 5, 1000, now)
	seed("deepseek-v4-flash", 8, 2, 0, now-86400) // 昨天

	daily, err := store.UsageDaily(0, 7)
	if err != nil {
		t.Fatalf("UsageDaily: %v", err)
	}
	if len(daily) != 2 {
		t.Fatalf("应有 2 个自然日桶，实际 %d: %+v", len(daily), daily)
	}
	// 升序：较早的在前
	var totalReq, totalTokens, totalCharged int64
	for _, d := range daily {
		totalReq += d.Requests
		totalTokens += d.Tokens
		totalCharged += d.ChargedMicro
	}
	if totalReq != 3 || totalTokens != 50 || totalCharged != 3000 {
		t.Fatalf("合计 req=%d tokens=%d charged=%d want 3/50/3000", totalReq, totalTokens, totalCharged)
	}

	models, err := store.UsageByModel(0, 7)
	if err != nil {
		t.Fatalf("UsageByModel: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("应有 2 个模型桶，实际 %d", len(models))
	}
	// 按已扣降序 → glm 在前
	if models[0].Model != "glm-5.2" || models[0].Requests != 2 || models[0].ChargedMicro != 3000 {
		t.Fatalf("模型聚合错误: %+v", models[0])
	}
}

// 按用户过滤：userID>0 时只看该用户。
func TestUsageReportUserScoped(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "report2.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()
	now := time.Now().Unix()
	for _, uid := range []int{1, 2} {
		if _, err := store.db.Exec(
			`INSERT INTO usage_logs (user_id, model, prompt_tokens, completion_tokens, charged_micro, created_at)
			 VALUES (?, 'm', 1, 1, 100, ?)`, uid, now); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	daily, _ := store.UsageDaily(1, 7)
	var reqs int64
	for _, d := range daily {
		reqs += d.Requests
	}
	if reqs != 1 {
		t.Fatalf("用户 1 只应有 1 条，实际 %d", reqs)
	}
}

// ---------- 指标计数 ----------

func TestMetricsCounts(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	// 预置：1 账号、1 用户、1 条用量
	if err := store.UpsertAccount(&Account{UID: "u1", Realm: "cn", AccessToken: "at"}); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	if _, err := store.CreateUser("alice", "bcrypt-hash-placeholder"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := store.db.Exec(
		`INSERT INTO usage_logs (user_id, model, prompt_tokens, completion_tokens, charged_micro, created_at)
		 VALUES (1, 'm', 3, 4, 500, ?)`, time.Now().Unix()); err != nil {
		t.Fatalf("seed usage: %v", err)
	}

	m, err := store.MetricsCounts()
	if err != nil {
		t.Fatalf("MetricsCounts: %v", err)
	}
	if m.Users != 1 || m.Accounts != 1 {
		t.Fatalf("users=%d accounts=%d want 1/1", m.Users, m.Accounts)
	}
	if m.UsageRequests != 1 || m.UsageTokens != 7 || m.UsageChargedMicro != 500 {
		t.Fatalf("usage req=%d tokens=%d charged=%d want 1/7/500",
			m.UsageRequests, m.UsageTokens, m.UsageChargedMicro)
	}
}

// ---------- Prom 文本写入器 ----------

func TestPromWriterFormat(t *testing.T) {
	var w promWriter
	w.family("wb_test_total", "help text.", "counter")
	w.sample("wb_test_total", 42)
	w.sample("wb_test_total", 7, "state", "cooling")

	got := w.buf.String()
	want := "# HELP wb_test_total help text.\n" +
		"# TYPE wb_test_total counter\n" +
		"wb_test_total 42\n" +
		"wb_test_total{state=\"cooling\"} 7\n"
	if got != want {
		t.Fatalf("prom 输出不符:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// 标签转义：引号与反斜杠必须转义，否则抓取端解析失败。
func TestPromLabelEscaping(t *testing.T) {
	var w promWriter
	w.sample("m", 1, "k", `a"b\c`)
	if got := w.buf.String(); got != `m{k="a\"b\\c"} 1`+"\n" {
		t.Fatalf("转义错误: %q", got)
	}
}
