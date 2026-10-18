// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"testing"
	"time"

	"workbuddy2api/internal/upstream"
)

// 本文件钉住「请求级错误不罚号」（v0.31.41）。
//
// 缺陷形态是**同一个判断存在两份清单**：
//   - `isClientFaultKind` 决定「这类错误回不回 4xx 给客户端」；
//   - `reportUpstreamError` 的 default 分支另写一份，只要不是那 4 个具名 kind
//     就 `NoteError`（喂熔断，30min 起、封顶 6h）。
//
// 于是同一个 `ErrClient`（未细分的 4xx）在响应上被判定为"客户端过错"，
// 在账号处置上却算"账号故障"。后果：客户端发一批参数不对的请求，每个请求轮转
// 3 个账号 → 健康号被按次累计熔断 → 小号池直接进入"本地误判没有可用账号"。
//
// 三条不变量：
//  1. 请求级错误（isClientFaultKind 认定的）**不改任何账号惩罚位**；
//  2. 上游故障（5xx）**照常**累计 —— 否则就是过度放松；
//  3. 账号级信号（11140 等）**照常**处置 —— 反向回归。

// faultPolicyFixture 造一个在池账号（缓存要给够，避免余额解冻之类的分支干扰）。
func faultPolicyFixture(t *testing.T) (*Store, *Proxy) {
	t.Helper()
	store, px, _ := newTestApp(t, &Account{
		UID: "u-fault", Nickname: "甲", Realm: "cn",
		AccessToken: "at", RefreshToken: "rt",
		ExpiresAt: time.Now().Add(20 * 24 * time.Hour).Unix(),
	})
	if err := px.SyncAccounts(); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}
	return store, px
}

// penaltySnapshot 读三个与"罚号"有关的位（一次取齐，避免断言语义漂移）。
func penaltySnapshot(px *Proxy) (consecutive, breaker int, cooling bool) {
	st, _ := px.pool.Status("u-fault")
	return st.ConsecutiveFails, st.BreakerFails, st.Cooling
}

// TestUnclassified4xxDoesNotPenalizeAccount 未细分的 400 不该在账号头上记账。
//
// 用一条真实会发生的形态：上游返回 400 + 一句不含任何已知标记的 msg
// （客户端工具 schema 缺字段就是这种）。Classify 会落 ErrClient。
func TestUnclassified4xxDoesNotPenalizeAccount(t *testing.T) {
	store, px := faultPolicyFixture(t)
	defer store.Close()

	body := []byte(`{"code":400,"msg":"messages[0].tools[2].function.parameters is not a valid json schema"}`)
	kind := px.reportUpstreamError("u-fault", "deepseek-v4-flash", 400, body, time.Time{}, "", nil)
	if kind != upstream.ErrClient {
		t.Fatalf("前置：这条 body 应被分类为 ErrClient，实得 %v", kind)
	}

	consecutive, breaker, cooling := penaltySnapshot(px)
	if consecutive != 0 || breaker != 0 || cooling {
		t.Fatalf("请求级错误不该罚号：consecutive=%d breaker=%d cooling=%v", consecutive, breaker, cooling)
	}

	// 连打 5 次也不该攒出任何惩罚（真实场景就是一串坏请求连发）。
	for i := 0; i < 5; i++ {
		px.reportUpstreamError("u-fault", "deepseek-v4-flash", 400, body, time.Time{}, "", nil)
	}
	consecutive, breaker, cooling = penaltySnapshot(px)
	if consecutive != 0 || breaker != 0 || cooling {
		t.Fatalf("连发 6 次请求级错误后仍不该罚号：consecutive=%d breaker=%d cooling=%v",
			consecutive, breaker, cooling)
	}
}

// TestServerErrorStillPenalizes 上游 5xx 照常累计（防"顺手把惩罚位全关了"）。
func TestServerErrorStillPenalizes(t *testing.T) {
	store, px := faultPolicyFixture(t)
	defer store.Close()

	body := []byte(`{"code":500,"msg":"internal error"}`)
	px.reportUpstreamError("u-fault", "deepseek-v4-flash", 500, body, time.Time{}, "", nil)

	consecutive, _, _ := penaltySnapshot(px)
	if consecutive == 0 {
		t.Fatal("5xx 必须照常累计（否则上游持续故障时账号不会被降权，池子会被拖死）")
	}
}

// TestRequestIllegalStillDispositions 11140 的反向回归：账号级信号不能被这次放松带走。
func TestRequestIllegalStillDispositions(t *testing.T) {
	store, px := faultPolicyFixture(t)
	defer store.Close()

	body := []byte(`{"code":11140,"msg":"request illegal"}`)
	px.reportUpstreamError("u-fault", "deepseek-v4-flash", 400, body, time.Time{}, "", nil)

	_, _, cooling := penaltySnapshot(px)
	if !cooling {
		t.Fatal("11140 仍应做软冷却（可逆处置），本次改动只针对请求级错误")
	}
}

// TestModelBlockedStillBacksOff 11102 的反向回归：(账号,模型) 负缓存仍在。
//
// ErrModelBlocked 也在 isClientFaultKind 里，但它有**自己的** case 分支（BlockModelBackoff），
// 不该被 default 的"不罚号"吞掉 —— 这条用例就是防那个吞。
func TestModelBlockedStillBacksOff(t *testing.T) {
	store, px := faultPolicyFixture(t)
	defer store.Close()

	body := []byte(`{"code":11102,"msg":"no such model"}`)
	kind := px.reportUpstreamError("u-fault", "no-such-model", 400, body, time.Time{}, "", nil)
	if kind != upstream.ErrModelBlocked {
		t.Fatalf("前置：应分类为 ErrModelBlocked，实得 %v", kind)
	}
	st, _ := px.pool.Status("u-fault")
	if len(st.RateLimitedModels) == 0 && !st.Cooling {
		// 负缓存的表现形式是"该 (账号,模型) 被记一笔"——具体字段随实现而定，
		// 这里只要求**没有**落进"完全不处理"的路径。
		t.Fatalf("11102 必须留下避让记录（模型级），不能像请求级错误那样零动作：%+v", st)
	}
}

// Test401WithSessionDeadBodyMarksAndDegrades 401 + 会话失效证据 → 与 12153 同一处置。
//
// 为什么这条重要：同一个事件（会话失效）此前有两条纪律 —— body 里带 code=12153 时
// 标红+硬冷却+被动降级；只带 "Offline user session not found" 的 401 却只计数，
// 卡片上什么都没有，运维看到的是"号突然不能用了"。
func Test401WithSessionDeadBodyMarksAndDegrades(t *testing.T) {
	store, px := faultPolicyFixture(t)
	defer store.Close()

	body := []byte(`{"msg":"Offline user session not found"}`)
	kind := px.reportUpstreamError("u-fault", "deepseek-v4-flash", 401, body, time.Time{}, "", nil)
	if kind != upstream.ErrSessionDead {
		t.Fatalf("前置：这条 body 应分类为 ErrSessionDead，实得 %v", kind)
	}
	// fixture 的 access 还有 20 天 → 走「仅剩 access」分档。
	if _, ok := store.RefreshLostMap()["u-fault"]; !ok {
		t.Fatal("401 会话失效必须标「仅剩 access」（否则运维看不到任何提示）")
	}
	if _, ok := store.TokenDeadMap()["u-fault"]; ok {
		t.Fatal("access 仍有效时不该标真死")
	}
	if !px.pool.PassiveRefresh("u-fault") {
		t.Fatal("应同时降级为「只消费」（与 12153 路径一致）")
	}
}

// Test401WithoutEvidenceOnlyCounts 401 但没有会话失效证据（WAF/HTML 页）→ 只计数。
//
// 这条是防止把风控页判成会话失效：那会让一批健康号被标红+硬冷却，
// 正是 2026-10-13 那类误杀的形态。
func Test401WithoutEvidenceOnlyCounts(t *testing.T) {
	store, px := faultPolicyFixture(t)
	defer store.Close()

	body := []byte(`<html><head><title>401 Authorization Required</title></head></html>`)
	px.reportUpstreamError("u-fault", "deepseek-v4-flash", 401, body, time.Time{}, "", nil)

	if _, ok := store.TokenDeadMap()["u-fault"]; ok {
		t.Fatal("无证据的 401 不能标真死")
	}
	if _, ok := store.RefreshLostMap()["u-fault"]; ok {
		t.Fatal("无证据的 401 不能标「仅剩 access」")
	}
	if px.pool.PassiveRefresh("u-fault") {
		t.Fatal("无证据的 401 不能触发被动降级（那会让号不再续期）")
	}
}
