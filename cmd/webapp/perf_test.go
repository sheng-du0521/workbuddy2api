package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ======================================================================
// v0.26.7 采集补全：端到端耗时 + 上游前缀缓存命中量
//
// 背景（真实排查出来的）：用户报"延迟有点高"。查下来的事实链是
//   · 后台接口全部 1~29ms —— 后台不慢；
//   · 小请求（27 号探活）中位 1375ms —— 网关转发本身不慢；
//   · 同一时段的大请求（48 万 token）单次 92~129 秒 —— 与大 prompt 严格成正比；
//   · 计费 24~25 点/次，而按缓存命中口径该低一个数量级。
// 也就是说瓶颈在"上游重新处理 48 万 token 前缀"，而**这一层此前完全不可观测**：
// webapp 把上游明确回报的缓存三计数丢掉了（旧网关 internal/server 一直在采），
// 耗时也只存在 90 分钟的内存态面板里。下面这些用例把补上的采集钉住。
// ======================================================================

// TestParseUsageLineCapturesCacheTokens 流式路径必须解析上游的缓存三计数。
//
// 字段名与旧网关（internal/server/logging.go）**一字不差**：两套转发实现的
// 观测口径必须是同一份，否则两个面板的"命中率"不可比。
func TestParseUsageLineCapturesCacheTokens(t *testing.T) {
	line := `data: {"usage":{"prompt_tokens":481276,"completion_tokens":602,` +
		`"prompt_cache_hit_tokens":470000,"prompt_cache_miss_tokens":11276,` +
		`"prompt_cache_write_tokens":11000,"credit":25.22}}`

	var ui usageInfo
	parseUsageLine(line, &ui)

	if ui.PromptTokens != 481276 || ui.CompletionTokens != 602 {
		t.Fatalf("token 解析回归：%+v", ui)
	}
	if ui.CacheHitTokens != 470000 {
		t.Errorf("CacheHitTokens = %d，期望 470000", ui.CacheHitTokens)
	}
	if ui.CacheMissTokens != 11276 {
		t.Errorf("CacheMissTokens = %d，期望 11276", ui.CacheMissTokens)
	}
	if ui.CacheWriteTokens != 11000 {
		t.Errorf("CacheWriteTokens = %d，期望 11000", ui.CacheWriteTokens)
	}
	if !ui.HasCredit || ui.Credit != 25.22 {
		t.Errorf("credit 回归：%+v", ui)
	}
}

// TestParseUsageLineAbsentCacheFields 上游不返回缓存字段时必须留 0 ——
// 0 的含义是"未采集"，不是"命中 0 次"。两者在统计上必须能分开（见 UsagePerf）。
func TestParseUsageLineAbsentCacheFields(t *testing.T) {
	var ui usageInfo
	parseUsageLine(`data: {"usage":{"prompt_tokens":100,"completion_tokens":5}}`, &ui)
	if ui.CacheHitTokens != 0 || ui.CacheMissTokens != 0 || ui.CacheWriteTokens != 0 {
		t.Errorf("缺字段时应全 0，得 %+v", ui)
	}
	if ui.PromptTokens != 100 {
		t.Errorf("其余字段仍应解析：%+v", ui)
	}
}

// TestParseUsageJSONCapturesCacheTokens 非流式路径与流式**同一口径**。
// 两条链路各写一遍解析是"两套转发实现"的历史遗留，漏一条就会让某类客户端的
// 观测凭空消失。
func TestParseUsageJSONCapturesCacheTokens(t *testing.T) {
	raw := json.RawMessage(`{"prompt_tokens":1000,"completion_tokens":50,` +
		`"prompt_cache_hit_tokens":900,"prompt_cache_miss_tokens":100,` +
		`"prompt_cache_write_tokens":80,"credit":1.5}`)
	ui := parseUsageJSON(raw)
	if ui.CacheHitTokens != 900 || ui.CacheMissTokens != 100 || ui.CacheWriteTokens != 80 {
		t.Errorf("非流式缓存字段解析不符：%+v", ui)
	}
	if ui.LatencyMS != 0 {
		t.Errorf("解析层不该填耗时（那是转发层的职责），得 %d", ui.LatencyMS)
	}
}

// TestUsageLogPersistsLatencyAndCache 落库必须真的带上新字段，
// 且读回来一致 —— 只改结构体不改 SQL 是很常见的一类"改了但没生效"。
func TestUsageLogPersistsLatencyAndCache(t *testing.T) {
	store := newTestStore(t) // 内部已注册 t.Cleanup 关闭，勿再 defer

	u, err := store.CreateUser("perf-user", "hash")
	if err != nil {
		t.Fatal(err)
	}
	want := &UsageLog{
		UserID: u.ID, Model: "kimi-k3-1",
		PromptTokens: 481276, CompletionTokens: 602,
		UpstreamCredit: 25.22, ChargedMicro: 25220,
		UpstreamUID: "acct-1", Status: "ok",
		CacheHitTokens:   470000,
		CacheMissTokens:  11276,
		CacheWriteTokens: 11000,
		LatencyMS:        126481,
	}
	if err := store.InsertUsageLog(want); err != nil {
		t.Fatal(err)
	}

	got, err := store.ListUsageLogs(u.ID, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("应有 1 条，得 %d", len(got))
	}
	g := got[0]
	if g.LatencyMS != 126481 {
		t.Errorf("LatencyMS 未落库：%d", g.LatencyMS)
	}
	if g.CacheHitTokens != 470000 || g.CacheMissTokens != 11276 || g.CacheWriteTokens != 11000 {
		t.Errorf("缓存三计数未落库：hit=%d miss=%d write=%d",
			g.CacheHitTokens, g.CacheMissTokens, g.CacheWriteTokens)
	}
}

// TestUsagePerfAggregates 汇总口径：
//   - 平均值/分位**只在有观测的行上算**（历史行的 0 不能拉低它）；
//   - 命中率按 **token 加权**，不是按请求数。
func TestUsagePerfAggregates(t *testing.T) {
	store := newTestStore(t) // 内部已注册 t.Cleanup 关闭，勿再 defer
	u, err := store.CreateUser("perf2", "hash")
	if err != nil {
		t.Fatal(err)
	}

	seed := func(latency int64, hit, miss int) {
		if err := store.InsertUsageLog(&UsageLog{
			UserID: u.ID, Model: "m", PromptTokens: hit + miss, Status: "ok",
			CacheHitTokens: hit, CacheMissTokens: miss, LatencyMS: latency,
		}); err != nil {
			t.Fatal(err)
		}
	}
	seed(1000, 0, 0)       // 无缓存观测
	seed(100000, 900, 100) // 大请求，命中率 90%
	seed(2000, 0, 0)       // 无缓存观测
	seed(3000, 50, 50)     // 小请求，命中率 50%
	// 历史行：两个指标都是 0（模拟 v0.26.7 之前的库）
	if err := store.InsertUsageLog(&UsageLog{UserID: u.ID, Model: "m", Status: "ok"}); err != nil {
		t.Fatal(err)
	}

	p, err := store.UsagePerf(u.ID, 14)
	if err != nil {
		t.Fatal(err)
	}

	// 4 条有耗时观测（1000/100000/2000/3000）；历史行不计入分母。
	if p.LatencySampled != 4 {
		t.Errorf("耗时样本数应为 4（历史 0 值行不计入），得 %d", p.LatencySampled)
	}
	if want := (1000.0 + 100000 + 2000 + 3000) / 4; p.AvgLatencyMS != want {
		t.Errorf("平均耗时 = %v，期望 %v", p.AvgLatencyMS, want)
	}
	if p.MaxLatencyMS != 100000 {
		t.Errorf("最大耗时 = %d，期望 100000", p.MaxLatencyMS)
	}
	// 升序：1000, 2000, 3000, 100000 → p50 = idx 2 = 3000；p95 = idx 3 = 100000
	if p.P50LatencyMS != 3000 {
		t.Errorf("P50 = %d，期望 3000", p.P50LatencyMS)
	}
	if p.P95LatencyMS != 100000 {
		t.Errorf("P95 = %d，期望 100000", p.P95LatencyMS)
	}

	// 缓存：只有 2 条有观测（900/100 与 50/50）。
	if p.CacheSampled != 2 {
		t.Errorf("缓存样本数应为 2，得 %d", p.CacheSampled)
	}
	if p.CacheHitTokens != 950 || p.CacheMissTokens != 150 {
		t.Errorf("缓存合计不符：hit=%d miss=%d", p.CacheHitTokens, p.CacheMissTokens)
	}
	// token 加权：950/1100 ≈ 0.8636 —— 若按请求数算是 1/2 = 0.5，差别巨大。
	if want := 950.0 / 1100.0; p.CacheHitRate < want-1e-9 || p.CacheHitRate > want+1e-9 {
		t.Errorf("命中率 = %v，期望 token 加权的 %v（按请求数算会是 0.5）", p.CacheHitRate, want)
	}
}

// TestUsagePerfEmpty 空库：不得除零、不得报错，样本数全 0。
// 前端要能区分"命中率 0%"与"还没有观测"。
func TestUsagePerfEmpty(t *testing.T) {
	store := newTestStore(t) // 内部已注册 t.Cleanup 关闭，勿再 defer
	p, err := store.UsagePerf(0, 14)
	if err != nil {
		t.Fatal(err)
	}
	if p.LatencySampled != 0 || p.CacheSampled != 0 {
		t.Errorf("空库样本数应为 0：%+v", p)
	}
	if p.AvgLatencyMS != 0 || p.CacheHitRate != 0 {
		t.Errorf("空库指标应为 0：%+v", p)
	}
}

// TestPercentileNearestRank 分位取最近秩，且边界不越界。
func TestPercentileNearestRank(t *testing.T) {
	vals := []int64{1000, 2000, 3000, 100000}
	cases := []struct {
		q    float64
		want int64
	}{
		{0.0, 1000},
		{0.5, 3000},
		{0.95, 100000},
		{1.0, 100000}, // 不越界
	}
	for _, c := range cases {
		if got := percentile(vals, c.q); got != c.want {
			t.Errorf("percentile(%v) = %d，期望 %d", c.q, got, c.want)
		}
	}
	if percentile(nil, 0.5) != 0 {
		t.Error("空切片应返回 0")
	}
}

// TestReportIncludesPerf 报表接口必须把 perf 透出（采了看不见等于没采）。
func TestReportIncludesPerf(t *testing.T) {
	store, px, a := newTestApp(t, &Account{UID: "u1", AccessToken: "t"})
	defer store.Close()
	defer px.Close()

	req := httptest.NewRequest(http.MethodGet, "/api/admin/usage?days=7", nil)
	req.Header.Set("X-Admin-Key", a.key)
	rec := httptest.NewRecorder()
	a.handleAdminUsage(rec, req)
	if rec.Code != 200 {
		t.Fatalf("报表应 200，得 %d：%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	perf, ok := out["perf"].(map[string]any)
	if !ok {
		t.Fatalf("报表应包含 perf 块，得 %v", out)
	}
	for _, k := range []string{"latency_sampled", "avg_latency_ms", "p50_latency_ms",
		"p95_latency_ms", "max_latency_ms", "cache_sampled", "cache_hit_rate"} {
		if _, ok := perf[k]; !ok {
			t.Errorf("perf 缺字段 %q", k)
		}
	}
	// 口径必须随响应一起给出，避免前端各自解读。
	if s, _ := perf["cache_note"].(string); !strings.Contains(s, "token") {
		t.Errorf("cache_note 应说明 token 加权口径，得 %q", s)
	}
}

// ======================================================================
// v0.26.7 · 报表日界缺陷（真机排查时顺带发现的）
//
// 缺陷：`since` 曾写成
//
//	time.Now().AddDate(0, 0, -(days-1)).Truncate(24*time.Hour).Unix() + off
//
// Truncate 按 **UTC 零点**切，再 `+off` 得到的是「本地日界 + 2×off」——
// 东八区下即**本地今天 16:00**（未来）。后果：**当天 00:00~16:00 的流水
// 全部落在窗口外**，报表在每天前 16 小时看不到"今天"。
// 实测就是在本地 15:05 发现刚落库的两条流水在 days=1 窗口里查不到。
// ======================================================================

// TestLocalDayStartNotInFuture 窗口起点**绝不能在未来** —— 这是本缺陷的最小判据。
func TestLocalDayStartNotInFuture(t *testing.T) {
	now := time.Now()
	got := localDayStart(1)
	if got > now.Unix() {
		t.Fatalf("窗口起点落在未来：got=%d（%s） now=%d（%s）—— 会漏掉当天的全部数据",
			got, time.Unix(got, 0).Format(time.RFC3339),
			now.Unix(), now.Format(time.RFC3339))
	}
	if got == now.Unix() {
		t.Fatal("窗口起点恰好等于当前时刻：只剩同一秒的数据，几乎等价于全部漏掉")
	}
}

// TestLocalDayStartIsLocalMidnight days=1 必须正好是**本地今天的零点**。
func TestLocalDayStartIsLocalMidnight(t *testing.T) {
	now := time.Now()
	want := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
	if got := localDayStart(1); got != want {
		t.Errorf("localDayStart(1) = %d（%s），期望本地零点 %d（%s）",
			got, time.Unix(got, 0).Format(time.RFC3339),
			want, time.Unix(want, 0).Format(time.RFC3339))
	}
}

// TestLocalDayStartWindowDays 窗口长度：days 每加 1 就往前推一个自然日。
func TestLocalDayStartWindowDays(t *testing.T) {
	d1, d2, d14 := localDayStart(1), localDayStart(2), localDayStart(14)
	if !(d14 < d2 && d2 < d1) {
		t.Errorf("窗口应随 days 递减：d14=%d d2=%d d1=%d", d14, d2, d1)
	}
	// 自然日长度。中国无夏令时，恒定 86400；这里断言"差分是 86400 的整数倍"
	// 而不是死等 86400，避免在夏令时地区误报。
	if diff := d1 - d2; diff < 82800 || diff > 90000 {
		t.Errorf("相邻窗口应相差约一天，得 %d 秒", diff)
	}
	if d1 <= 0 || d14 <= 0 {
		t.Error("窗口起点不应为非正数（时区计算错误）")
	}
	// 非法 days 钳到 1（与各统计函数入口的钳制一致，不得算出更晚的起点）。
	if got := localDayStart(0); got != d1 {
		t.Errorf("localDayStart(0) 应钳到 days=1，得 %d vs %d", got, d1)
	}
}

// TestUsagePerfSeesTodaysRows 端到端复现：**刚落库的流水必须能被 days=1 查到**。
//
// 这是用户实际看到的现象（报表"今天"空白）的最小复现。只测 localDayStart 的
// 数值不够 —— 真正要钉的是"采集 → 落库 → 按窗口查询"这条链走通。
func TestUsagePerfSeesTodaysRows(t *testing.T) {
	store := newTestStore(t)
	u, err := store.CreateUser("today", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InsertUsageLog(&UsageLog{
		UserID: u.ID, Model: "m", PromptTokens: 1000, Status: "ok",
		CacheHitTokens: 900, CacheMissTokens: 100, LatencyMS: 1234,
	}); err != nil {
		t.Fatal(err)
	}

	p, err := store.UsagePerf(u.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if p.LatencySampled != 1 || p.CacheSampled != 1 {
		t.Fatalf("刚落库的流水必须落在 days=1 窗口内：%+v", p)
	}
	if p.MaxLatencyMS != 1234 {
		t.Errorf("耗时未取到：%d", p.MaxLatencyMS)
	}

	// 同样的窗口口径下，逐日聚合也要能看到今天。
	daily, err := store.UsageDaily(u.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(daily) != 1 || daily[0].Requests != 1 {
		t.Fatalf("UsageDaily(days=1) 应含今天这 1 条，得 %+v", daily)
	}
	today := time.Now().Format("2006-01-02")
	if daily[0].Day != today {
		t.Errorf("日期分组应为本地今天 %s，得 %s", today, daily[0].Day)
	}
}
