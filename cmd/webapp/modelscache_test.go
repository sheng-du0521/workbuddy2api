// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 本文件锁死 v0.24.0 修复的 modelsCache 数据竞争与缓存击穿。
//
// 缺陷：modelsCache 原是无锁包级 struct，多请求 goroutine 同时读写（cachedModelIDs 读 /
// refreshModelsCache 写）。/v1/models 又恰是客户端接入的第一个探测请求 —— 多客户端
// 同时接入就会撞上；同时每个并发请求各自打上游（惊群）。
//
// 注：本环境无 gcc，`go test -race` 跑不起来（见 docs/项目全面审查报告 P1-3）。
// 因此这里不依赖 -race，而是断言**可观测行为**：并发刷新只应触发**一轮**上游探测。
// 修复前 N=8 个并发会打出 8 轮；修复后应为 1 轮（2 次探测），差异是数量级的。

// clearModelsCache 清空包级缓存（global）。
//
// 注意**不要**在这里注册 t.Cleanup —— 早先写成 resetModelsCache 自清理，
// 而 cleanup 又调用 resetModelsCache → 无限注册 cleanup 导致测试挂死。
// 清理逻辑（本函数）与注册逻辑（下方 resetModelsCache）必须分开。
func clearModelsCache() {
	modelsCache.mu.Lock()
	modelsCache.at = time.Time{}
	modelsCache.payload = nil
	modelsCache.ids = nil
	modelsCache.mu.Unlock()
}

// resetModelsCache 用例前清空缓存，并在用例后复位（避免污染其它测试）。
func resetModelsCache(t *testing.T) {
	t.Helper()
	clearModelsCache()
	t.Cleanup(clearModelsCache)
}

// TestModelsCacheSingleFlight 并发刷冷缓存 → 上游只被探测**一轮**（单飞）。
func TestModelsCacheSingleFlight(t *testing.T) {
	resetModelsCache(t)

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		// 稍微慢一点，让并发请求真的有重叠窗口（否则单飞与惊群难以区分）。
		time.Sleep(60 * time.Millisecond)
		_, _ = w.Write([]byte(`{"code":0,"data":[]}`))
	}))
	defer srv.Close()

	store, px, a := newTestApp(t, &Account{UID: "u1", Realm: "cn", AccessToken: "at"})
	defer store.Close()
	px.uc.ChatBaseCN = srv.URL
	px.uc.BillingBaseCN = srv.URL
	px.uc.HTTP = srv.Client()
	if err := px.SyncAccounts(); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}

	const N = 8
	var wg sync.WaitGroup
	got := make([][]string, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i] = a.refreshModelsCache()
		}(i)
	}
	wg.Wait()

	// FetchModels 内部并发探测两个端点（企业版 + v3），故一轮 = 2 次请求。
	// 修复前：N 个并发各自一轮 = 16 次；修复后：1 轮 = 2 次。
	// 用 <= 4 留一点实现余量（若上游探测端点增减，阈值仍能区分数量级）。
	if n := calls.Load(); n > 4 {
		t.Fatalf("并发刷新应只打一轮上游（≈2 次），实际 %d 次 —— 单飞失效（缓存击穿）", n)
	}
	if n := calls.Load(); n == 0 {
		t.Fatal("从未探测上游：说明走了缓存，但本用例刚清空缓存，应至少探测一轮")
	}

	// 所有并发调用者都必须拿到**一致且非空**的结果（不能有人拿到 nil）。
	for i, ids := range got {
		if len(ids) == 0 {
			t.Fatalf("第 %d 个并发调用者拿到空列表", i)
		}
		if len(ids) != len(got[0]) {
			t.Fatalf("第 %d 个结果长度 %d 与首个 %d 不一致", i, len(ids), len(got[0]))
		}
	}
}

// TestModelsCacheHitAvoidsUpstream 命中缓存时不再打上游（TTL 内）。
func TestModelsCacheHitAvoidsUpstream(t *testing.T) {
	resetModelsCache(t)

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"code":0,"data":[]}`))
	}))
	defer srv.Close()

	store, px, a := newTestApp(t, &Account{UID: "u1", Realm: "cn", AccessToken: "at"})
	defer store.Close()
	px.uc.ChatBaseCN = srv.URL
	px.uc.BillingBaseCN = srv.URL
	px.uc.HTTP = srv.Client()
	_ = px.SyncAccounts()

	a.refreshModelsCache()
	first := calls.Load()
	if first == 0 {
		t.Fatal("首次应探测上游")
	}
	// 再连续刷 5 次：TTL 内应全部命中缓存，上游计数不变。
	for i := 0; i < 5; i++ {
		a.refreshModelsCache()
	}
	if got := calls.Load(); got != first {
		t.Fatalf("TTL 内不应再打上游：首次 %d，之后变 %d", first, got)
	}
}

// TestModelsCacheConcurrentHandlers 并发走 handleModels 处理器：不得 panic，
// 且所有响应体一致（验证读写路径在并发下都拿到**副本**而非共享底层数组）。
func TestModelsCacheConcurrentHandlers(t *testing.T) {
	resetModelsCache(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":[]}`))
	}))
	defer srv.Close()

	store, px, a := newTestApp(t, &Account{UID: "u1", Realm: "cn", AccessToken: "at"})
	defer store.Close()
	px.uc.ChatBaseCN = srv.URL
	px.uc.BillingBaseCN = srv.URL
	px.uc.HTTP = srv.Client()
	_ = px.SyncAccounts()

	const N = 16
	bodies := make([]string, N)
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			a.handleModels(rec, httptest.NewRequest("GET", "/v1/models", nil))
			bodies[i] = rec.Body.String()
		}(i)
	}
	wg.Wait()

	if bodies[0] == "" {
		t.Fatal("响应体为空")
	}
	for i, b := range bodies {
		if b != bodies[0] {
			t.Fatalf("第 %d 个响应体与首个不一致（并发下读写共享数据）：%q vs %q", i, b, bodies[0])
		}
	}
}

// TestCachedModelIDsReturnsCopy 返回的必须是副本 —— 调用方改它不能污染内部缓存。
//
// 这条防的是「修了锁但忘了防别名」：把内部切片直接交出去，等价于把无锁共享
// 换了个形式继续存在。
func TestCachedModelIDsReturnsCopy(t *testing.T) {
	resetModelsCache(t)
	modelsCache.mu.Lock()
	modelsCache.ids = []string{"m1", "m2"}
	modelsCache.payload = []byte(`{"object":"list","data":[]}`)
	modelsCache.at = time.Now()
	modelsCache.mu.Unlock()

	got := cachedModelIDs()
	if len(got) != 2 {
		t.Fatalf("应返回 2 个 id，got %v", got)
	}
	got[0] = "MUTATED"
	if again := cachedModelIDs(); again[0] != "m1" {
		t.Fatalf("内部缓存被调用方改坏：%v", again)
	}

	p := modelsPayload()
	if len(p) == 0 {
		t.Fatal("payload 不应为空")
	}
	p[0] = 'X'
	if again := modelsPayload(); again[0] == 'X' {
		t.Fatal("payload 返回了共享底层数组")
	}
}

// ---------- app 内部缓存的惰性初始化（v0.24.0）----------

// TestAppInitCachesConcurrent 并发首次访问只应建一份，且各调用者看到同一实例。
//
// 这修的是原先的裸 `if a.x == nil { a.x = ... }`：多个请求 goroutine 同时写
// 同一指针字段（数据竞争），还可能各建一份、后写覆盖先写（缓存丢失）。
func TestAppInitCachesConcurrent(t *testing.T) {
	a := &app{}
	const N = 24
	got := make([]*poolGroupGuard, N)
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a.initCaches()
			got[i] = a.poolGroups
		}(i)
	}
	wg.Wait()
	for i := 1; i < N; i++ {
		if got[i] == nil {
			t.Fatalf("第 %d 个调用者拿到 nil", i)
		}
		if got[i] != got[0] {
			t.Fatalf("第 %d 个调用者拿到不同实例（各建一份 → 缓存失效）", i)
		}
	}
	if a.taskCache == nil || a.growthCache == nil {
		t.Fatal("三个缓存都应被初始化")
	}
}

// TestAppInitCachesResettable 置回 nil 后必须能**重新**初始化。
//
// 这是与 sync.Once 的关键差别，也是本项目真实需要的语义：
// poolgroup_test.go 用 `app.poolGroups = nil` 重置节流窗口来测「第二次判定」，
// 而 Once 执行过就再也不执行 —— 字段置 nil 后就再也建不起来（那正是本用例
// 被加进来的原因：改用 Once 后 TestReconcilePoolGroupOne 立刻失败）。
func TestAppInitCachesResettable(t *testing.T) {
	a := &app{}
	a.initCaches()
	first := a.poolGroups

	a.poolGroups = nil
	a.initCaches()
	if a.poolGroups == nil {
		t.Fatal("置 nil 后应能重新初始化（sync.Once 做不到这一点）")
	}
	if a.poolGroups == first {
		t.Fatal("应是一个新实例（重置的语义就是丢掉旧的节流状态）")
	}
	// taskCache / growthCache 未被置 nil，应保持原实例（不做无谓重建）。
	if a.taskCache == nil || a.growthCache == nil {
		t.Fatal("未置 nil 的缓存不该被丢掉")
	}
}
