// internal/usage 的独立测试（v0.31.9，P2-7 拆包第二刀）。
//
// 为什么这些用例值得单独写：聚合 SQL 是"**错了不会崩、只会给出错误数字**"的那类代码 ——
// 在 cmd/webapp 里它们只能通过整个 app + 真实调度才能间接跑到，所以边界几乎没人钉。
// 独立成包后可以用一个临时 SQLite 建表插数，把"哪些行该被算进去"逐条断言。
//
// 钉住的口径（都是历史上真出过问题或极易写错的地方）：
//  1. **本地日界**：窗口起点是"本地零点 × days"，不是 `Truncate(24h)+offset`（v0.26.7 的真实缺陷：
//     东八区下窗口起点落在今天 16:00（未来）→ 当天 00:00~16:00 的数据全被漏掉）；
//  2. 边界秒：`today0 - 1` 属于昨天，不算进 days=1；
//  3. `user_id > 0`：系统行（user_id=0）不进用户维度聚合，但进全站维度；
//  4. `upstream_uid <> ”`：没上报账号的行不进按账号聚合；
//  5. scope 过滤（"" = 全部 / pool / host）；
//  6. 分页与排序（按 id 倒序 = 最近优先）；
//  7. 缓存计数只统计"上游明确回报过"的行（hit+miss > 0）。
package usage

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// 表结构**镜像** cmd/webapp 的真实 schema（DDL 取自线上库的 sqlite_master）。
// 只保留本包查询用到的列；若 app 侧改了列名，cmd/webapp 的既有测试会先炸，
// 这里再跟着改（本文件不承担"发现 schema 漂移"的职责，只钉聚合口径）。
const testSchema = `
CREATE TABLE users (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  username TEXT NOT NULL UNIQUE
);
CREATE TABLE usage_logs (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id           INTEGER NOT NULL,
  key_id            INTEGER NOT NULL DEFAULT 0,
  model             TEXT NOT NULL DEFAULT '',
  prompt_tokens     INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  upstream_credit   REAL NOT NULL DEFAULT 0,
  charged_micro     INTEGER NOT NULL DEFAULT 0,
  upstream_uid      TEXT NOT NULL DEFAULT '',
  status            TEXT NOT NULL DEFAULT 'ok',
  error_msg         TEXT NOT NULL DEFAULT '',
  created_at        INTEGER NOT NULL DEFAULT 0,
  scope             TEXT NOT NULL DEFAULT 'pool',
  sticky_key_hash   TEXT NOT NULL DEFAULT '',
  attempts          INTEGER NOT NULL DEFAULT 0,
  cache_hit_tokens  INTEGER NOT NULL DEFAULT 0,
  cache_miss_tokens INTEGER NOT NULL DEFAULT 0,
  cache_write_tokens INTEGER NOT NULL DEFAULT 0,
  latency_ms        INTEGER NOT NULL DEFAULT 0
);`

// dbFixture 一批**确定性**的流水 + 用户，配合下面的断言使用。
type dbFixture struct {
	q      *Queries
	today0 int64 // 今天本地零点
	yday0  int64 // 昨天本地零点
}

func newFixture(t *testing.T) *dbFixture {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "u.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(testSchema); err != nil {
		t.Fatalf("建表: %v", err)
	}
	for _, u := range []struct {
		id   int64
		name string
	}{{1, "alice"}, {2, "bob"}, {3, "carol"}} {
		if _, err := db.Exec(`INSERT INTO users(id, username) VALUES(?,?)`, u.id, u.name); err != nil {
			t.Fatalf("插用户: %v", err)
		}
	}

	today0 := LocalDayStart(1)
	yday0 := LocalDayStart(2)
	// 一笔流水的全部维度（用 name 参数化，便于按语义读用例）。
	type row struct {
		user      int64
		model     string
		scope     string
		uid       string
		at        int64
		pt, ct    int
		charged   int64
		credit    float64
		latency   int64
		hit, miss int
		write     int
		sticky    string
		attempts  int
	}
	rows := []row{
		// 今天：user1 / glm-a / pool / acct-1，缓存有回报
		{1, "glm-a", "pool", "acct-1", today0 + 3600, 10, 5, 100, 1.0, 100, 3, 7, 1, "hash-1", 1},
		// 昨天：user1 / glm-b / pool / acct-2
		{1, "glm-b", "pool", "acct-2", yday0 + 43200, 20, 10, 200, 2.0, 300, 0, 0, 0, "", 2},
		// 今天：user2 / glm-a / host / acct-1
		{2, "glm-a", "host", "acct-1", today0 + 7200, 30, 15, 300, 3.0, 500, 5, 0, 0, "", 1},
		// 边界秒：今天零点**前一秒** → 属于昨天，不该进 days=1
		{1, "glm-a", "pool", "acct-1", today0 - 1, 1, 1, 999, 0, 10, 0, 0, 0, "", 1},
		// 系统行：user_id = 0 → 用户维度聚合必须排除它，但全站维度要算
		{0, "glm-a", "pool", "", today0 + 60, 1, 1, 7, 0, 10, 0, 0, 0, "", 1},
		// 今天：user2 / glm-a / host / **无账号上报**（uid 空）→ 不进按账号聚合
		{2, "glm-a", "host", "", today0 + 1800, 2, 2, 50, 0.5, 500, 0, 0, 0, "", 1},
		// 昨天：user3 / glm-a / pool / acct-2（用于区分 days=1 与 days=2 的用户数）
		{3, "glm-a", "pool", "acct-2", yday0 + 3600, 4, 0, 400, 0, 50, 0, 0, 0, "", 1},
	}
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO usage_logs
			(user_id, model, scope, upstream_uid, created_at, prompt_tokens, completion_tokens,
			 charged_micro, upstream_credit, latency_ms, cache_hit_tokens, cache_miss_tokens,
			 cache_write_tokens, sticky_key_hash, attempts)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			r.user, r.model, r.scope, r.uid, r.at, r.pt, r.ct, r.charged, r.credit,
			r.latency, r.hit, r.miss, r.write, r.sticky, r.attempts); err != nil {
			t.Fatalf("插流水: %v", err)
		}
	}
	return &dbFixture{q: New(db), today0: today0, yday0: yday0}
}

// TestLocalDayStartIsLocalMidnight 窗口起点必须是**本地零点**，且绝不在未来。
//
// 这是 v0.26.7 修过的真实缺陷：写成 `Truncate(24h) + offset` 时，东八区得到的是
// 「本地日界 + 2×offset」= 今天 16:00（未来）→ 当天 00:00~16:00 的用量全被漏掉。
func TestLocalDayStartIsLocalMidnight(t *testing.T) {
	now := time.Now()
	want := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
	if got := LocalDayStart(1); got != want {
		t.Fatalf("LocalDayStart(1) = %d，期望今天本地零点 %d", got, want)
	}
	if got := LocalDayStart(1); got > now.Unix() {
		t.Fatalf("窗口起点落在未来（%d > now）：当天数据会被整段漏掉", got)
	}
	// days<1 视为 1（防御：调用方传 0/负数不该得到"未来起点"或"空窗口"）
	if got := LocalDayStart(0); got != want {
		t.Errorf("LocalDayStart(0) = %d，应等价于 1 天", got)
	}
	// 相邻窗口正好差一天（本地日界，跨夏令时也由 time.Date 处理）
	d1, d2 := LocalDayStart(1), LocalDayStart(2)
	if y := time.Unix(d1, 0).AddDate(0, 0, -1); y.Unix() != d2 {
		t.Errorf("LocalDayStart(2)=%d 应等于 LocalDayStart(1) 的前一天 %d", d2, y.Unix())
	}
	// LocalDayOffset 是当前时区相对 UTC 的秒偏移（UTC 下为 0；东八区 28800）。
	// **只记录不断言**：CI/容器时区可能不同，钉死数值只会得到环境相关的偶发失败。
	_, off := time.Now().Zone()
	if got := LocalDayOffset(); got != int64(off) {
		t.Errorf("LocalDayOffset()=%d 与 time.Now().Zone() 的偏移 %d 不一致", got, off)
	}
}

func TestPercentileBoundaries(t *testing.T) {
	if got := Percentile(nil, 0.5); got != 0 {
		t.Errorf("空切片应返回 0，got %d", got)
	}
	one := []int64{42}
	for _, q := range []float64{0, 0.5, 0.99, 1} {
		if got := Percentile(one, q); got != 42 {
			t.Errorf("单元素 q=%v 应为 42，got %d", q, got)
		}
	}
	vals := []int64{10, 20, 30, 40}
	if got := Percentile(vals, 0); got != 10 {
		t.Errorf("q=0 应取最小，got %d", got)
	}
	if got := Percentile(vals, 1); got != 40 {
		t.Errorf("q=1 应取最大，got %d", got)
	}
	// q 超界不得越界读（实现里会钳到 len-1）
	if got := Percentile(vals, 2); got != 40 {
		t.Errorf("q>1 应钳到最大，got %d", got)
	}
}

func TestListUsageLogsPagingAndMapping(t *testing.T) {
	f := newFixture(t)
	rows, err := f.q.ListUsageLogs(1, 10, 0)
	if err != nil {
		t.Fatalf("ListUsageLogs: %v", err)
	}
	// user1 有 3 行（今天、昨天、边界秒那行）
	if len(rows) != 3 {
		t.Fatalf("user1 应有 3 行，got %d", len(rows))
	}
	// 按 id 倒序 = 最近插入优先
	for i := 1; i < len(rows); i++ {
		if rows[i-1].ID <= rows[i].ID {
			t.Errorf("应按 id 倒序，位置 %d/%d 违反", i-1, i)
		}
	}
	// 字段映射（列顺序写错是最容易犯的错，这里逐字段钉一遍）
	top := rows[0]
	if top.ChargedMicro != 999 || top.LatencyMS != 10 || top.UserID != 1 {
		t.Errorf("字段映射不对：%+v", top)
	}
	if top.PromptTokens != 1 || top.CompletionTokens != 1 || top.Attempts != 1 {
		t.Errorf("token/attempts 映射不对：%+v", top)
	}
	// 分页：第二页从 offset=1 起，拿到的是中间那一行
	p2, err := f.q.ListUsageLogs(1, 1, 1)
	if err != nil || len(p2) != 1 {
		t.Fatalf("分页第二页 got %v err %v", p2, err)
	}
	if p2[0].ID != rows[1].ID {
		t.Errorf("offset=1 应返回第二行（id=%d），got %d", rows[1].ID, p2[0].ID)
	}
	// 缓存三计数要如实带出（它是"为什么又慢又贵"的唯一直接证据）：
	// rows[0] 是边界秒那行（上游没回报缓存 → 全 0）；rows[2] 是今天那行（3/7/1）。
	if got := rows[0].CacheHitTokens + rows[0].CacheMissTokens + rows[0].CacheWriteTokens; got != 0 {
		t.Errorf("无缓存回报的行三计数应全 0，got %d", got)
	}
	if rows[2].CacheHitTokens != 3 || rows[2].CacheMissTokens != 7 || rows[2].CacheWriteTokens != 1 {
		t.Errorf("缓存三计数映射不对：%+v", rows[2])
	}
}

func TestUserUsageSummaryIsAllTime(t *testing.T) {
	f := newFixture(t)
	reqs, tokens, charged, err := f.q.UserUsageSummary(1)
	if err != nil {
		t.Fatalf("UserUsageSummary: %v", err)
	}
	// user1 的三行：100+200+999（**不含时间窗口过滤**，这是"总额"口径）
	if reqs != 3 || tokens != 47 || charged != 1299 {
		t.Errorf("got reqs=%d tokens=%d charged=%d，期望 3/47/1299", reqs, tokens, charged)
	}
	// 不存在的用户：全零且无错
	if r, tk, c, err := f.q.UserUsageSummary(99); err != nil || r != 0 || tk != 0 || c != 0 {
		t.Errorf("不存在的用户应全零无错，got %d/%d/%d %v", r, tk, c, err)
	}
}

func TestUsageByAccountExcludesEmptyUID(t *testing.T) {
	f := newFixture(t)
	got, err := f.q.UsageByAccount()
	if err != nil {
		t.Fatalf("UsageByAccount: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应有两个账号（空 upstream_uid 的行必须排除），got %d: %+v", len(got), got)
	}
	a1 := got["acct-1"]
	// acct-1：今天 2 行 + 边界秒 1 行（按账号聚合**不带时间窗口**）
	if a1.Requests != 3 || a1.Tokens != 62 || a1.Charged != 1399 {
		t.Errorf("acct-1 got %+v，期望 3/62/1399", a1)
	}
	a2 := got["acct-2"]
	if a2.Requests != 2 || a2.Charged != 600 {
		t.Errorf("acct-2 got %+v，期望 2/600", a2)
	}
	if _, ok := got[""]; ok {
		t.Error("空 upstream_uid 不该成为 key")
	}
}

func TestUsageByModelWindowAndScope(t *testing.T) {
	f := newFixture(t)
	// days=1：只有今天 → 只剩 glm-a（457 = 100+300+7+50）
	one, err := f.q.UsageByModelScoped(0, 1, "")
	if err != nil {
		t.Fatalf("UsageByModelScoped: %v", err)
	}
	if len(one) != 1 || one[0].Model != "glm-a" || one[0].ChargedMicro != 457 || one[0].Requests != 4 {
		t.Fatalf("days=1 期望 glm-a/457/4，got %+v", one)
	}
	// days=2：窗口起点回到昨天零点 → 补上"昨天"的三行：glm-b(200)、边界秒的 glm-a(999)、
	// carol 的 glm-a(400)；glm-a 合计 457+999+400=1856（6 次），仍按消耗倒序排第一。
	two, err := f.q.UsageByModelScoped(0, 2, "")
	if err != nil || len(two) != 2 {
		t.Fatalf("days=2 got %+v err %v", two, err)
	}
	if two[0].Model != "glm-a" || two[0].ChargedMicro != 1856 || two[0].Requests != 6 {
		t.Errorf("days=2 的 glm-a 应为 1856/6，got %+v", two[0])
	}
	if two[1].Model != "glm-b" || two[1].ChargedMicro != 200 {
		t.Errorf("days=2 的 glm-b 应为 200，got %+v", two[1])
	}
	// scope=host：只有 host 那两行（300+50）
	host, err := f.q.UsageByModelScoped(0, 2, "host")
	if err != nil || len(host) != 1 || host[0].ChargedMicro != 350 || host[0].Requests != 2 {
		t.Errorf("scope=host got %+v err %v", host, err)
	}
	// UsageByModel 是 scope="" 的薄封装，口径必须一致
	viaModel, err := f.q.UsageByModel(0, 2)
	if err != nil || len(viaModel) != len(two) {
		t.Fatalf("UsageByModel 与 UsageByModelScoped(\"\") 口径不一致：%+v vs %+v", viaModel, two)
	}
	// 限定用户：只有 user1 的 glm-a（今天 100）
	byUser, err := f.q.UsageByModel(1, 1)
	if err != nil || len(byUser) != 1 || byUser[0].ChargedMicro != 100 {
		t.Errorf("userID=1 got %+v err %v", byUser, err)
	}
}

func TestUsageDailyScopedLocalDayBuckets(t *testing.T) {
	f := newFixture(t)
	got, err := f.q.UsageDailyScoped(0, 2, "")
	if err != nil {
		t.Fatalf("UsageDailyScoped: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应有两个自然日桶，got %+v", got)
	}
	now := time.Now()
	wantToday := now.Format("2006-01-02")
	wantYday := now.AddDate(0, 0, -1).Format("2006-01-02")
	if got[0].Day != wantYday || got[1].Day != wantToday {
		t.Fatalf("桶应按**本地自然日**且升序：got %q,%q 期望 %q,%q",
			got[0].Day, got[1].Day, wantYday, wantToday)
	}
	// 昨天桶 = glm-b(200) + **边界秒那行(999)** + carol(400) = 1599 / 3 次 ——
	// "今天零点前一秒"必须落在昨天桶，这是日界口径最容易错的一格。
	// 今天桶 = 100+300+7+50（**含**系统行 user_id=0：全站维度不过滤用户）。
	if got[0].ChargedMicro != 1599 || got[0].Requests != 3 {
		t.Errorf("昨天桶 got %+v，期望 1599/3", got[0])
	}
	if got[1].ChargedMicro != 457 || got[1].Requests != 4 {
		t.Errorf("今天桶 got %+v，期望 457/4（含系统行、不含边界秒那行 999）", got[1])
	}
	// days=1 + scope=host：只剩今天一个桶（昨天那两行都是 pool）
	h, err := f.q.UsageDailyScoped(0, 1, "host")
	if err != nil || len(h) != 1 || h[0].ChargedMicro != 350 {
		t.Errorf("days=1 scope=host got %+v err %v", h, err)
	}
}

func TestUsageByUserJoinsUsername(t *testing.T) {
	f := newFixture(t)
	got, err := f.q.UsageByUser(0, 1, 10)
	if err != nil {
		t.Fatalf("UsageByUser: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("今天只有 user1/user2（user_id=0 的行必须排除），got %+v", got)
	}
	// 按消耗倒序：user2(350) 在 user1(100) 之前；用户名来自 users 表
	if got[0].UserID != 2 || got[0].Username != "bob" || got[0].ChargedMicro != 350 {
		t.Errorf("首位应为 bob/350，got %+v", got[0])
	}
	if got[1].Username != "alice" || got[1].Tokens != 15 {
		t.Errorf("次位应为 alice/15，got %+v", got[1])
	}
	// days=2 时 carol 出现（她只在昨天有量）—— 这条同时证明 days 真的生效了
	all, err := f.q.UsageByUser(0, 2, 10)
	if err != nil || len(all) != 3 {
		t.Fatalf("days=2 应含 carol，got %+v err %v", all, err)
	}
	// limit 生效
	lim, err := f.q.UsageByUser(0, 2, 1)
	if err != nil || len(lim) != 1 {
		t.Errorf("limit=1 应只回 1 条，got %+v err %v", lim, err)
	}
}

func TestCountUsageUsersWindow(t *testing.T) {
	f := newFixture(t)
	one, err := f.q.CountUsageUsers(1)
	if err != nil || one != 2 {
		t.Fatalf("days=1 去重用户应为 2（alice/bob），got %d err %v", one, err)
	}
	two, err := f.q.CountUsageUsers(2)
	if err != nil || two != 3 {
		t.Fatalf("days=2 去重用户应为 3（+carol），got %d err %v", two, err)
	}
}

func TestUsagePerfLatencyAndCache(t *testing.T) {
	f := newFixture(t)
	got, err := f.q.UsagePerf(0, 1)
	if err != nil {
		t.Fatalf("UsagePerf: %v", err)
	}
	// 今天的行：4 行有 latency>0（100/500/10/500）→ 采样 4、最大 500
	if got.LatencySampled != 4 {
		t.Errorf("LatencySampled got %d want 4", got.LatencySampled)
	}
	if got.MaxLatencyMS != 500 {
		t.Errorf("MaxLatencyMS got %d want 500", got.MaxLatencyMS)
	}
	if got.AvgLatencyMS < 270 || got.AvgLatencyMS > 285 {
		t.Errorf("AvgLatencyMS got %v want ≈277.5", got.AvgLatencyMS)
	}
	if got.P50LatencyMS < 10 || got.P50LatencyMS > 500 || got.P95LatencyMS != 500 {
		t.Errorf("分位值越界或不单调：%+v", got)
	}
	// 缓存：**两行**"上游明确回报过"（3+7 与 5+0）→ 采样 2、hit 8、miss 7、写 1；
	// 命中率 = 8/(8+7) ≈ 0.5333。第三行（miss=0 且 hit=0）不计入采样 —— 那是"没回报"，
	// 不是"没命中"，两者混起来会让命中率被稀释。
	if got.CacheSampled != 2 || got.CacheHitTokens != 8 || got.CacheMissTokens != 7 || got.CacheWriteTokens != 1 {
		t.Errorf("缓存计数 got %+v，期望采样 2 / 8 / 7 / 1", got)
	}
	if got.CacheHitRate < 0.53 || got.CacheHitRate > 0.54 {
		t.Errorf("命中率 got %v want ≈0.5333", got.CacheHitRate)
	}
}

func TestUsageLogsBySticky(t *testing.T) {
	f := newFixture(t)
	got, err := f.q.UsageLogsBySticky("hash-1", 10)
	if err != nil {
		t.Fatalf("UsageLogsBySticky: %v", err)
	}
	if len(got) != 1 || got[0].Model != "glm-a" || got[0].StickyKeyHash != "hash-1" {
		t.Fatalf("按粘性键查会话应只回 1 行，got %+v", got)
	}
	if none, err := f.q.UsageLogsBySticky("nope", 10); err != nil || len(none) != 0 {
		t.Errorf("未知粘性键应回空，got %v err %v", none, err)
	}
}
