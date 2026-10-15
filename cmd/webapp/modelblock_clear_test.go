// 成功路径对 (账号, 模型) 负缓存的清理口径（v0.31.0）。
//
// 背景：`BlockModelBackoff`（11102「该后端无此模型」）写的是 modelCooldowns 条目，
// 其**设计好的**提前解除路径是「该 (账号, 模型) 实测又通了」→ BlockModelClear。
// 旧网关一直有这一步（internal/server/handler.go），webapp 侧历史上缺 —— 当时被
// 「签到解冻会清空整张模型冷却表」这个副作用掩盖着。v0.31.0 修掉该副作用后，
// 这条恢复路径必须在本侧真的存在，否则 11102 条目只能等 TTL（6h 起、封顶 24h）。
package main

import (
	"testing"
	"time"

	"workbuddy2api/internal/upstream"
)

// hasModelCooling 该账号在 /status 台账里是否还有指定模型的冷却条目。
func hasModelCooling(t *testing.T, px *Proxy, uid, model string) bool {
	t.Helper()
	st, ok := px.pool.Status(uid)
	if !ok {
		t.Fatalf("账号 %s 不在池内", uid)
	}
	for _, m := range st.RateLimitedModels {
		if m.Model == model {
			return true
		}
	}
	return false
}

// TestNoteChatSuccessClearsModelBlockEntry 成功记账只清 11102 负缓存、不清 6004 独立冷却、
// 且不波及其他模型。
func TestNoteChatSuccessClearsModelBlockEntry(t *testing.T) {
	const uid = "mbc-uid-0001"
	store, px, _, srv, up := newChatApp(t, uid, 5000)
	defer store.Close()
	defer srv.Close()
	defer up.Close()

	// ① 11102 负缓存 → 一次成功必须解除（否则该模型在该号上白等 6~24h）。
	px.pool.BlockModelBackoff(uid, "m-blocked", upstream.ModelBlockReason)
	if !hasModelCooling(t, px, uid, "m-blocked") {
		t.Fatal("前置条件不成立：11102 条目应已写入")
	}
	px.noteChatSuccess(uid, "m-blocked")
	if hasModelCooling(t, px, uid, "m-blocked") {
		t.Error("成功一次应清掉该 (账号, 模型) 的 11102 负缓存（BlockModelClear 未被调用）")
	}

	// ② 6004 独立冷却**不得**被一次成功抹掉：它对齐的是上游给的重置墙钟，
	//    「这个模型刚才成功过」并不等于「限流窗口已结束」。
	resetAt := time.Now().Add(2 * time.Hour)
	px.pool.CooldownSoftForModel(uid, time.Minute, resetAt, "m-6004", "6004 model rate limit")
	px.noteChatSuccess(uid, "m-6004")
	if !hasModelCooling(t, px, uid, "m-6004") {
		t.Error("成功不得清除 6004 模型级冷却（只清 11102 前缀条目）")
	}

	// ③ 只清命中的那个模型，其他模型的 11102 条目不受影响。
	px.pool.BlockModelBackoff(uid, "m-other", upstream.ModelBlockReason)
	px.noteChatSuccess(uid, "m-6004")
	if !hasModelCooling(t, px, uid, "m-other") {
		t.Error("清命中的模型不得波及其他模型的 11102 条目")
	}

	// ④ NoteSuccess 仍然照常（清熔断/连败等运行态阻塞位）。
	px.pool.NoteError(uid)
	px.noteChatSuccess(uid, "m-6004")
	st, _ := px.pool.Status(uid)
	if st.ConsecutiveFails != 0 || st.BreakerFails != 0 {
		t.Errorf("成功记账仍应清失败累计：consecutive=%d breaker=%d", st.ConsecutiveFails, st.BreakerFails)
	}
}

// TestModelCooldownScopeAcrossCreditsSync 端到端钉住「余额同步不会让冷却失效」。
//
// 这是本次核查结论里最关键的一条可执行证据：签到/探活/代挂刷新都走
// ReenableIfCredits，它过去会把整张模型冷却表清空 —— 于是「冷却中的 (账号, 模型)」
// 会在下一次余额同步之后重新被调度，产生一批必然失败的请求。
func TestModelCooldownScopeAcrossCreditsSync(t *testing.T) {
	const uid = "mbc-uid-0002"
	store, px, _, srv, up := newChatApp(t, uid, 5000)
	defer store.Close()
	defer srv.Close()
	defer up.Close()

	px.pool.CooldownSoftForModel(uid, time.Minute, time.Now().Add(3*time.Hour), "m-cool", "6004")
	// 模拟定时签到 / 健康探测 / 代挂刷新余额的收尾（三处调用完全一致）。
	px.pool.SetCreditsDetailed(uid, 9000, 0)
	px.pool.ReenableIfCredits(uid, 9000)

	if !hasModelCooling(t, px, uid, "m-cool") {
		t.Fatal("余额同步（ReenableIfCredits）不得清除模型级冷却")
	}
	// 该模型此刻不可选：单账号池下选号应为"选不到"而不是"选到它再失败"。
	if got := px.pool.PickExcludingForRealm(nil, "m-cool", ""); got != nil {
		t.Errorf("模型冷却期内不该选中该账号的该模型, got %v", got.UID)
	}
	// 其他模型不受影响。
	if got := px.pool.PickExcludingForRealm(nil, "m-fine", ""); got == nil {
		t.Error("其他模型应仍可选（模型级冷却只锁一个模型）")
	}
}
