// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// modelnorm_test.go 旧网关的「模型名归一化」与「流内模型级错误帧」端到端验收（v0.31.3）。
//
// 两件事都是从主产品同步过来的**口径**（此前只有主产品做）：
//  1. `GLM-5.3` 这类写法在旧网关上会被原样送出 → 上游必然 400/11102；
//  2. HTTP 200 + 流内 6004/11102 → 池侧完全不知道 → (账号,模型) 不冷却 → 下一个
//     同模型请求照样路由到它、再次撞同一个错。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// captureUpstream 假上游：除按 auth 决定响应外，把每次**出站请求体**记下来，
// 供断言"上游实际收到的 model 字段是什么"（归一化最容易在这一点上作假：
// 只改本地变量、不改出站 body，测试仍然全绿）。
type captureUpstream struct {
	mu     sync.Mutex
	bodies []string
}

func (c *captureUpstream) models() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.bodies))
	for _, b := range c.bodies {
		var m struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal([]byte(b), &m)
		out = append(out, m.Model)
	}
	return out
}

func newCapturingUpstream(t *testing.T, cap *captureUpstream, behavior func(auth string) (int, string, bool)) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Body != nil {
				if b, err := io.ReadAll(r.Body); err == nil {
					cap.mu.Lock()
					cap.bodies = append(cap.bodies, string(b))
					cap.mu.Unlock()
				}
			}
			status, body, isStream := behavior(r.Header.Get("Authorization"))
			ct := "application/json"
			if isStream {
				ct = "text/event-stream"
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{ct}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

// TestChatModelNameNormalizedOutbound 客户端写 `GLM-5.3`（已知清单里是 `glm-5.3`）
// → 出站请求体的 model 必须是规范写法。
//
// 为什么断言"出站 body"而不是本地变量：v0.31.2 的第一版就踩过这种不对称 ——
// 归一化只改本地变量、不改出站请求体，上游照样收到错写法（照样 11102），
// 而单元测试若只看本地变量会全绿。
func TestChatModelNameNormalizedOutbound(t *testing.T) {
	resetModelsCache()
	defer resetModelsCache()
	seedModelsCache([]upstream.ModelInfo{{ID: "glm-5.3"}, {ID: "kimi-k3-1"}})

	cap := &captureUpstream{}
	up := newCapturingUpstream(t, cap, func(string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})

	for _, in := range []string{"GLM-5.3", " glm-5.3 ", "GLM_5.3", "cn:GLM-5.3", "[cn:]GLM-5.3"} {
		rec := httptest.NewRecorder()
		body := `{"model":"` + in + `","stream":false,"messages":[{"role":"user","content":"hi"}]}`
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
		if rec.Code != 200 {
			t.Fatalf("model=%q code=%d body=%s", in, rec.Code, rec.Body)
		}
	}
	got := cap.models()
	if len(got) != 5 {
		t.Fatalf("应有 5 次出站，got %v", got)
	}
	for i, m := range got {
		if m != "glm-5.3" {
			t.Errorf("第 %d 次出站的 model=%q，期望 glm-5.3（归一化必须落到出站请求体上）", i+1, m)
		}
	}
}

// TestChatUnknownModelNotRewritten 清单里没有的名字**原样送出**（不猜模型）。
//
// 猜错会把"可诊断的 11102（模型不存在）"变成一次沉默的失败：上游正常服务了
// 另一个真实存在的模型，而用户永远不知道自己的写法被改过。
func TestChatUnknownModelNotRewritten(t *testing.T) {
	resetModelsCache()
	defer resetModelsCache()
	seedModelsCache([]upstream.ModelInfo{{ID: "glm-5.3"}})

	cap := &captureUpstream{}
	up := newCapturingUpstream(t, cap, func(string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","stream":false,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if got := cap.models(); len(got) != 1 || got[0] != "gpt-4o" {
		t.Errorf("未知模型名必须原样送出，got %v", got)
	}
}

// ── 流内模型级错误帧（HTTP 200 + error 帧）────────────────────────────────

// streamWithErrFrame 构造「正常帧 + 上游 error 帧 + [DONE]」的流式响应体。
//
// ⚠️ 不能复用 sseOK：它自带收尾的 `data: [DONE]`，而流透传遇到 [DONE] 就停止读取
// —— 错误帧会被放在 DONE 之后，永远读不到（本用例第一版就是这么假通过的）。
func streamWithErrFrame(frame string) string {
	const clean = "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1," +
		"\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n"
	return clean + "data: " + frame + "\n\n" + "data: [DONE]\n\n"
}

// TestChatInStream6004CoolsModel 流式：上游 200 + 流内 6004 → (账号,模型) 冷却。
//
// 断言用**行为**而不是内部字段：同模型请求必须跳过这个号（说明冷却真的写进了
// 它的模型级台账），而 6004 不写账号级 until（账号对其它模型照常服务）。
func TestChatInStream6004CoolsModel(t *testing.T) {
	seen := &captureUpstream{}
	up := newCapturingUpstream(t, seen, func(auth string) (int, string, bool) {
		if auth == "Bearer at-bad" {
			return 200, streamWithErrFrame(`{"error":{"message":"您的使用量已超出频率限制","code":"6004"}}`), true
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000) // 分数更高 → 无冷却时必然首选
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d（流内错误帧时响应头已是 200）", rec.Code)
	}

	st, _ := p.Status("bad")
	if len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Model != "glm-5.3" {
		t.Fatalf("bad 应有 glm-5.3 的模型级冷却台账，got %+v", st.RateLimitedModels)
	}
	if !st.Until.IsZero() {
		t.Errorf("6004 不该写账号级 until，got %v", st.Until)
	}
	// 行为断言：同模型跳过 bad（仍冷却），其它模型照常选中 bad。
	if got := p.PickExcludingForRealm(nil, "glm-5.3", ""); got == nil || got.UID != "good" {
		t.Errorf("同模型选号应跳过已冷却的 bad，got %+v", got)
	}
	if got := p.PickExcludingForRealm(nil, "hy3-x", ""); got == nil || got.UID != "bad" {
		t.Errorf("其它模型不应受 6004 模型级冷却影响，got %+v", got)
	}
}

// TestChatNonStreamInStream6004CoolsModel 非流式（聚合）路径同样冷却。
//
// 非流式请求读的是同一条 SSE（整条读完再聚合），此前那帧错误**根本没人看**：
// 客户端拿到的是一份空 content 的 200，而池侧什么都不知道。
func TestChatNonStreamInStream6004CoolsModel(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, streamWithErrFrame(`{"error":{"code":6004,"message":"model usage limit exceeded"}}`), true
	})
	p := testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","stream":false,"messages":[{"role":"user","content":"hi"}]}`)))

	st, _ := p.Status("a1")
	if len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Model != "glm-5.3" {
		t.Fatalf("非流式路径也应写模型级冷却，got %+v（响应码 %d）", st.RateLimitedModels, rec.Code)
	}
}

// TestChatInStream11102BlocksModel 流内 11102 → (账号,模型) 负缓存避让（与 4xx 同原语）。
func TestChatInStream11102BlocksModel(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, streamWithErrFrame(`{"error":{"code":11102,"message":"model service info not found"}}`), true
	})
	p := testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	st, _ := p.Status("a1")
	if len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Model != "glm-5.3" {
		t.Fatalf("11102 流内形态应写模型级避让，got %+v", st.RateLimitedModels)
	}
	if !st.Until.IsZero() {
		t.Errorf("11102 也不该写账号级 until，got %v", st.Until)
	}
}

// TestChatInStreamNonModelCodeNoDisposition 反例（最重要的一条）：**非模型级**码
// 在流内出现时不得写任何账号或模型级处置。
//
// 11140 是「这次请求非法」（账号级/请求级，达阈值才处置），把它当模型级会写出
// 一个错误的避让键；更糟的是这一类"按一段文本给账号定罪"正是 2026-10-13 误杀
// 30 个号的成因。这里钉住"流内热路径只处置模型级码"。
func TestChatInStreamNonModelCodeNoDisposition(t *testing.T) {
	for _, frame := range []string{
		`{"error":{"code":11140,"message":"request illegal"}}`,
		`{"error":{"code":12153,"message":"session expired"}}`,
		`{"error":{"code":11128,"message":"request illegal"}}`,
		`{"error":{"message":"no code at all"}}`,
	} {
		up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, streamWithErrFrame(frame), true })
		p := testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999})
		h := NewHandler(Config{Pool: p, Upstream: up})

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.3","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))

		st, _ := p.Status("a1")
		if len(st.RateLimitedModels) != 0 {
			t.Errorf("%s：流内非模型级码不得写模型级冷却，got %+v", frame, st.RateLimitedModels)
		}
		if !st.Until.IsZero() {
			t.Errorf("%s：流内热路径不得动账号级冷却（账号级处置有自己的链路），got until=%v", frame, st.Until)
		}
		if st.Disabled {
			t.Errorf("%s：流内一段文本不得停用账号", frame)
		}
	}
}
