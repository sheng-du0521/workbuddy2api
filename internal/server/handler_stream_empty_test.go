// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// TestChatStreamEmptyUpstreamLogged502 RED：上游 200 + 空流（0 有效帧）时，
// StreamHint 写 error 帧 + [DONE] 兜底（wire 200），但日志状态必须收敛为 502
// 观测——此前 `_ =` 吞错把失败流记成 200 假成功（排障被误导）。
func TestChatStreamEmptyUpstreamLogged502(t *testing.T) {
	withChatLog(t)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, "", true // 空 body，非 SSE
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	out := captureStdout(t, func() {
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	})
	if rec.Code != 200 {
		t.Fatalf("wire code=%d want 200 (headers already sent before empty-stream detected)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "empty upstream stream") {
		t.Errorf("client should receive empty-stream error frame: %s", rec.Body)
	}
	if !strings.Contains(out, "| 502 |") {
		t.Errorf("log row status must be 502 (empty stream is upstream failure), got:\n%s", out)
	}
}
