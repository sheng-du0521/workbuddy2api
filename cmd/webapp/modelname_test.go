// 模型名归一化的**证据源**测试（v0.31.2；算法用例已随实现迁到 internal/modelname）。
//
// 本文件只测本产品特有的部分：已知模型清单从哪来（三源合并、种子化只取成功、有界）。
package main

import (
	"fmt"
	"testing"
	"time"
)

// seedModelsCache 临时把模型清单缓存置为新鲜（模拟"后台刚拉过 /v1/models"）。
// 返回恢复函数 —— 缓存是包级共享状态，用完必须还原，否则会串到别的用例。
func seedModelsCache(t *testing.T, ids []string) func() {
	t.Helper()
	modelsCache.mu.Lock()
	oldAt, oldPayload, oldIDs := modelsCache.at, modelsCache.payload, modelsCache.ids
	modelsCache.at = time.Now()
	modelsCache.payload = []byte(`{"object":"list","data":[]}`)
	modelsCache.ids = ids
	modelsCache.mu.Unlock()
	return func() {
		modelsCache.mu.Lock()
		modelsCache.at, modelsCache.payload, modelsCache.ids = oldAt, oldPayload, oldIDs
		modelsCache.mu.Unlock()
	}
}

// hasStr 小工具（避免与 account_delete_test.go 里的同名函数冲突）。
func hasStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestKnownModelIDsMergesThreeSources 已知清单三源合并，且**绝不出站**。
//
// 三源：/v1/models 新鲜缓存 → 池内实测模型（observed）→ 内置兜底。
// 关键点是第二源：进程刚启动时缓存还没热（要么没人调 /v1/models，要么管理员没开控制台），
// 而池内实测模型随 pool_state.json 一起恢复 —— 没有它，重启后"大小写归一"这一层
// 会静默退化成"只能用内置兜底清单匹配"，而兜底清单里没有生产在用的模型。
func TestKnownModelIDsMergesThreeSources(t *testing.T) {
	// ① 缓存新鲜：缓存内容排在最前，observed 与兜底补在后（去重）。
	restore := seedModelsCache(t, []string{"glm-5.3", "kimi-k3-1"})
	ids := knownModelIDs([]string{"kimi-k3-1", "deepseek-v4-flash"})
	restore()
	if len(ids) < 3 {
		t.Fatalf("应合并三源，got %v", ids)
	}
	if ids[0] != "glm-5.3" || ids[1] != "kimi-k3-1" {
		t.Errorf("缓存内容应在最前且去重，got %v", ids)
	}
	if !hasStr(ids, "deepseek-v4-flash") {
		t.Errorf("observed 里的模型应被并入，got %v", ids)
	}
	// ② 缓存过期：observed 仍在（这正是重启后的常态）。
	modelsCache.mu.Lock()
	modelsCache.at = time.Time{}
	modelsCache.payload = nil
	modelsCache.ids = nil
	modelsCache.mu.Unlock()
	ids = knownModelIDs([]string{"glm-5.3"})
	if ids[0] != "glm-5.3" {
		t.Errorf("缓存过期时 observed 必须顶上，got %v", ids)
	}
	// ③ 有上限：外部输入不能把清单撑成无界（池内观测的模型名来自请求）。
	big := make([]string, 0, 400)
	for i := 0; i < 400; i++ {
		big = append(big, fmt.Sprintf("m-%d", i))
	}
	if got := knownModelIDs(big); len(got) > 256 {
		t.Errorf("已知清单应有上限（256），got %d", len(got))
	}
}

// TestClientStatusOf 台账「回客户端」状态码的映射 —— 与线上末端同一函数。
//
// 这一列存在的意义：上游对「模型不存在」固定回 400，而我们回客户端的是 404；
// 不区分就会把"某模型写错"读成"网关整站 400"。
func TestClientStatusOf(t *testing.T) {
	cases := []struct {
		stage  string
		status int
		kind   string
		want   int
	}{
		{FailStageUpstream, 400, "model_blocked", 404},   // 上游 400 → 客户端 404
		{FailStageUpstream, 400, "client", 400},          // 上游 400 → 客户端 400
		{FailStageUpstream, 400, "bad_params", 400},      //
		{FailStageUpstream, 400, "content_blocked", 400}, //
		{FailStageUpstream, 400, "prompt_too_long", 400}, //
		{FailStageUpstream, 400, "channel_denied", 400},  // 11128 渠道拒绝（零罚账号但仍回 400）
		{FailStageUpstream, 429, "soft_rate", 429},       //
		{FailStageUpstream, 500, "server", 503},          //
		{FailStageUpstream, 401, "session_dead", 503},    //
		{FailStageUpstream, 400, "", 0},                  // 未分类：不猜
		{FailStageReserve, 402, "", 402},                 // 预扣余额不足
		{FailStageReserve, 0, "", 0},                     // 预扣失败但退化为事后结算：客户端没收到错
		{FailStageSettle, 0, "", 0},                      // 结算发生在响应之后
		{FailStageCharge, 0, "client", 0},                // 扣费失败只影响账目
	}
	for _, c := range cases {
		if got := clientStatusOf(c.stage, c.status, c.kind); got != c.want {
			t.Errorf("clientStatusOf(%q,%d,%q) = %d，期望 %d", c.stage, c.status, c.kind, got, c.want)
		}
	}
}

// TestSeedRecentModelsOnlyTakesOK 启动种子化只收**成功**过的模型名。
//
// 失败行里的模型名可能正是写错的写法 —— 若把它写进清单，归一化会把
// **正确**写法折成错的（最危险的方向）。这条断言就是钉这个方向。
func TestSeedRecentModelsOnlyTakesOK(t *testing.T) {
	// 还原全局清单（它是进程级的，不能串到别的用例）。
	recentModels.mu.Lock()
	oldList, oldSeen := recentModels.list, recentModels.seen
	recentModels.list, recentModels.seen = nil, nil
	recentModels.mu.Unlock()
	defer func() {
		recentModels.mu.Lock()
		recentModels.list, recentModels.seen = oldList, oldSeen
		recentModels.mu.Unlock()
	}()

	store, _, _ := newTestApp(t)
	defer store.Close()
	now := time.Now().Unix()
	ins := `INSERT INTO usage_logs (user_id, key_id, model, status, created_at) VALUES (0, 0, ?, ?, ?)`
	for _, row := range []struct{ model, status string }{
		{"glm-5.3", "ok"},
		{"Kimi-K3-1", "ok"},       // 大小写写法也成功过 → 也要收（它证明上游接受这个名字？）
		{"glm5.3", "interrupted"}, // 失败行：不得进清单
		{"", "ok"},                // 空名：不得进清单
	} {
		if _, err := store.db.Exec(ins, row.model, row.status, now); err != nil {
			t.Fatalf("写入 usage_logs 失败: %v", err)
		}
	}

	seedRecentModels(store)
	got := recentModelList()
	if !hasStr(got, "glm-5.3") {
		t.Errorf("成功过的模型名应入清单，got %v", got)
	}
	if !hasStr(got, "Kimi-K3-1") {
		t.Errorf("成功过的大小写写法也应入清单（它是上游真认过的名字），got %v", got)
	}
	if hasStr(got, "glm5.3") {
		t.Errorf("失败行里的模型名不得入清单（会把正确写法折成错的），got %v", got)
	}
	if hasStr(got, "") {
		t.Errorf("空名不得入清单，got %v", got)
	}
	// 幂等：再种子化一次不产生重复。
	seedRecentModels(store)
	if n := len(recentModelList()); n != 2 {
		t.Errorf("重复种子化应幂等（2 项），got %d: %v", n, recentModelList())
	}
}

// TestRememberModelBounded 清单有上限（模型名来自请求，属于外部输入）。
func TestRememberModelBounded(t *testing.T) {
	recentModels.mu.Lock()
	oldList, oldSeen := recentModels.list, recentModels.seen
	recentModels.list, recentModels.seen = nil, nil
	recentModels.mu.Unlock()
	defer func() {
		recentModels.mu.Lock()
		recentModels.list, recentModels.seen = oldList, oldSeen
		recentModels.mu.Unlock()
	}()

	for i := 0; i < 400; i++ {
		rememberModel(fmt.Sprintf("m-%d", i))
	}
	if n := len(recentModelList()); n != 256 {
		t.Errorf("应有上限 256，got %d", n)
	}
	// 空串/纯空白不入清单（TrimSpace 后为空）。
	rememberModel("   ")
	rememberModel("")
	if n := len(recentModelList()); n != 256 {
		t.Errorf("空名不应入清单，got %d", n)
	}
}
