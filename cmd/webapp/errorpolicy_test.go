// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"net/http"
	"strings"
	"testing"

	"workbuddy2api/internal/upstream"
)

// TestReportUpstreamErrorRequestLevelUnpunished v0.18.x 修复回归。
//
// 缺陷形态：reportUpstreamError 的 default 分支无条件 NoteError（喂熔断器），
// 于是「请求级错误」（内容拦截 / 请求体畸形 / 上下文超长）也记在账号头上，
// 连 3 次（= breakerThreshold 默认值）即熔断一个健康账号 30min~6h。
// 这些错误由请求内容决定、与账号健康无关（同一 body 换任何号结果相同）。
//
// 修复后按共享 upstream.Classify 分流：请求级错误零动作，账号保持可选。
// 本用例连打 3 次——修复前第 3 次即熔断、账号被摘出选号；修复后恒可选。
func TestReportUpstreamErrorRequestLevelUnpunished(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"内容拦截 400", 400, `{"code":11128,"msg":"prompt blocked by security policy"}`},
		{"请求体畸形 11101", 400, `{"code":11101,"msg":"Unmarshal chat params failed"}`},
		{"上下文超长 11115", 400, `{"code":11115,"msg":"prompt is too long"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store, px, _ := newTestApp(t, &Account{UID: "u1", Realm: "cn", AccessToken: "a"})
			defer store.Close()
			_ = px.SyncAccounts()

			for i := 0; i < 3; i++ {
				px.reportUpstreamError("u1", "m", c.status, []byte(c.body), "")
			}

			st, ok := px.pool.Status("u1")
			if !ok {
				t.Fatal("account must exist in pool")
			}
			if st.Disabled {
				t.Fatalf("request-level error must not disable account: %+v", st)
			}
			if st.Cooling {
				t.Fatalf("request-level error must not cool account: %+v", st)
			}
			if got := px.pickExcluding(map[string]bool{}, "m", pickScope{}); got == nil {
				t.Fatalf("account must stay selectable after request-level errors (st=%+v)", st)
			}
		})
	}
}

// TestReportUpstreamErrorNotFoundShortCooldown 上游 404：固定短冷却（非熔断、非禁用）。
// 修复前 404 落 default → NoteError（喂熔断器，连 3 次熔断）。
func TestReportUpstreamErrorNotFoundShortCooldown(t *testing.T) {
	store, px, _ := newTestApp(t, &Account{UID: "u1", Realm: "cn", AccessToken: "a"})
	defer store.Close()
	_ = px.SyncAccounts()

	px.reportUpstreamError("u1", "m", 404, []byte("Not Found"), "")

	st, _ := px.pool.Status("u1")
	if !st.Cooling {
		t.Fatalf("404 must short-cool the account: %+v", st)
	}
	if st.Disabled {
		t.Fatalf("404 must not disable account: %+v", st)
	}
	if !strings.Contains(st.Reason, "404") {
		t.Errorf("reason=%q want 404 marker", st.Reason)
	}
}

// TestReportUpstreamErrorModelBlockedBackoff 11102「该后端无此模型」：
// 记 (账号, 模型) 负缓存避让，账号本身不禁用。修复前落 default → 喂熔断器。
func TestReportUpstreamErrorModelBlockedBackoff(t *testing.T) {
	store, px, _ := newTestApp(t, &Account{UID: "u1", Realm: "cn", AccessToken: "a"})
	defer store.Close()
	_ = px.SyncAccounts()

	px.reportUpstreamError("u1", "m", 400, []byte(`{"code":11102,"msg":"service info not found"}`), "")

	st, _ := px.pool.Status("u1")
	if st.Disabled {
		t.Fatalf("11102 must not disable account: %+v", st)
	}
	if len(st.RateLimitedModels) == 0 {
		t.Fatalf("11102 must record (account, model) negative backoff: %+v", st)
	}
}

// ---------- v0.24.0（P2-1）：上游失败 → 客户端状态码映射 ----------

// TestUpstreamFailureStatus 失败归因到 HTTP 状态码的映射。
//
// 缺陷背景：此前**无论什么失败都回 503**。503 在 OpenAI 兼容客户端眼里是
// 「服务暂时不可用，可重试」→ 一个注定失败的坏请求会被反复自动重试，
// 每次再打最多 3 个账号（把客户端错误放大成持续上游流量，正是本项目一直
// 避免的风控特征）。4xx 被伪装成 5xx 也让排障看不出真正原因。
func TestUpstreamFailureStatus(t *testing.T) {
	cases := []struct {
		name string
		kind upstream.ErrKind
		want int
	}{
		// 请求本身的问题 → 4xx（改请求才有用，重试无意义）
		{"请求体畸形", upstream.ErrBadParams, http.StatusBadRequest},
		{"内容被拦", upstream.ErrContentBlocked, http.StatusBadRequest},
		{"上下文超长", upstream.ErrPromptTooLong, http.StatusBadRequest},
		{"图片无效", upstream.ErrImageInvalid, http.StatusBadRequest},
		{"客户端错误", upstream.ErrClient, http.StatusBadRequest},
		// 模型不存在 → 404
		{"模型不存在", upstream.ErrModelBlocked, http.StatusNotFound},
		// 限流 → 429（可重试，但带 Retry-After）
		{"上游限流", upstream.ErrSoftRate, http.StatusTooManyRequests},
		// 我方容量 / 账号级 / 上游故障 → 503（可重试）
		{"无可用账号", upstream.ErrNone, http.StatusServiceUnavailable},
		{"积分耗尽", upstream.ErrHardCredit, http.StatusServiceUnavailable},
		{"登录失效", upstream.ErrSessionDead, http.StatusServiceUnavailable},
		{"账号异常", upstream.ErrAccountFault, http.StatusServiceUnavailable},
		{"WAF 拦截", upstream.ErrWafBlock, http.StatusServiceUnavailable},
		{"上游 5xx", upstream.ErrServer, http.StatusServiceUnavailable},
	}
	for _, c := range cases {
		if got := upstreamFailureStatus(c.kind); got != c.want {
			t.Errorf("%s：status=%d want %d", c.name, got, c.want)
		}
	}
}

// TestIsClientFaultKind 客户侧归因的判定必须与状态码映射同源。
//
// 这两个函数一旦漂移，就会出现「映射说 400、优先级判断却不算客户端错误」
// 这种自相矛盾的结果 —— 故这里交叉断言一遍。
func TestIsClientFaultKind(t *testing.T) {
	clientKinds := []upstream.ErrKind{
		upstream.ErrBadParams, upstream.ErrContentBlocked,
		upstream.ErrPromptTooLong, upstream.ErrImageInvalid,
		upstream.ErrModelBlocked, upstream.ErrClient,
	}
	for _, k := range clientKinds {
		if !isClientFaultKind(k) {
			t.Errorf("%s 应判为客户端可修", k)
		}
		if got := upstreamFailureStatus(k); got >= 500 {
			t.Errorf("%s 是客户端错误，不该映射成 %d", k, got)
		}
	}
	serverKinds := []upstream.ErrKind{
		upstream.ErrServer, upstream.ErrWafBlock, upstream.ErrSessionDead,
		upstream.ErrHardCredit, upstream.ErrAccountFault, upstream.ErrNone,
	}
	for _, k := range serverKinds {
		if isClientFaultKind(k) {
			t.Errorf("%s 不该判为客户端可修（会掩盖我方容量问题）", k)
		}
		if got := upstreamFailureStatus(k); got < 500 {
			t.Errorf("%s 应映射为 5xx 可重试语义，got %d", k, got)
		}
	}
}
