// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ======================================================================
// 请求体上限的回归网（v0.31.73）
//
// 起因：线上 413 —— 客户端 deepseek-harness 发来 8.4~23.4MB 的请求体，
// 全部卡在 8MB 闸门上，一次都没触达上游（日志证据见 proxy.go chatBodyMaxDefault 注释）。
//
// 这批用例守三件事：
//  1. **上限按路径分**：/v1 放宽，其余（含未鉴权注册/登录）保持 16MB 兜底 ——
//     放宽推理不等于放宽注册接口；
//  2. **只有一个口径**：两层 MaxBytesReader 嵌套时数字必须一致，否则同一个 413
//     会报出两个不同的上限，排障时无法判断是谁在拦；
//  3. **可运行期调整**：WB_BODY_MAX_MB 合法则生效、非法/越界必须退回默认并 WARN，
//     绝不能因为配置写错让服务起不来。
// ======================================================================

// TestBodyLimitForPath 路径分流：/v1 用推理上限，其余用通用兜底。
func TestBodyLimitForPath(t *testing.T) {
	old := chatBodyMax
	defer func() { chatBodyMax = old }()
	chatBodyMax = 32 << 20

	cases := []struct {
		path string
		want int64
		why  string
	}{
		{"/v1/chat/completions", chatBodyMax, "OpenAI 推理入口"},
		{"/v1/messages", chatBodyMax, "Anthropic 推理入口"},
		{"/v1/models", chatBodyMax, "同前缀（GET 无 body，但口径要一致）"},
		{"/api/auth/register", maxRequestBody, "未鉴权接口必须留在小上限"},
		{"/api/admin/accounts/import", maxRequestBody, "导入接口靠 16MB 兜底"},
		{"/healthz", maxRequestBody, "其余路径"},
		{"/v1", maxRequestBody, "只有 /v1 本身（无尾斜杠）不属于推理前缀"},
	}
	for _, c := range cases {
		if got := bodyLimitFor(c.path); got != c.want {
			t.Errorf("bodyLimitFor(%q) = %d，期望 %d（%s）", c.path, got, c.want, c.why)
		}
	}
}

// TestLimitRequestBodyByPath 真中间件 + 真读体：验证"放行"与"仍然拦"两侧。
//
// 临时把 chatBodyMax 压到 12MB（>旧的 8MB 闸门，<16MB 通用兜底），
// 这样"9MB 放行"能证明旧闸门确实被抬开，同时不必分配几十 MB。
func TestLimitRequestBodyByPath(t *testing.T) {
	old := chatBodyMax
	defer func() { chatBodyMax = old }()
	chatBodyMax = 12 << 20

	// 只读 body：读到错误 = 413（MaxBytesReader 的行为），否则 200。
	h := limitRequestBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	do := func(path string, size int) int {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(bytes.Repeat([]byte("a"), size)))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	// 旧的 8MB 闸门卡的正是这一段：9MB 在 /v1 上必须放行。
	if code := do("/v1/chat/completions", 9<<20); code != http.StatusOK {
		t.Errorf("/v1 上 9MB 应放行（旧 8MB 闸门时代的真实故障），得到 %d", code)
	}
	// 超过新上限仍然要拦（放宽 ≠ 无上限）。
	if code := do("/v1/chat/completions", 13<<20); code != http.StatusRequestEntityTooLarge {
		t.Errorf("/v1 上 13MB（超过 12MB 上限）应 413，得到 %d", code)
	}
	// 非 /v1 路径不受 chatBodyMax 影响：9MB < 16MB 兜底 → 放行；
	// 17MB > 16MB → 仍然是 413（放宽推理不等于放宽导入/注册接口）。
	if code := do("/api/admin/accounts/import", 9<<20); code != http.StatusOK {
		t.Errorf("非 /v1 路径 9MB 应放行（16MB 兜底），得到 %d", code)
	}
	if code := do("/api/admin/accounts/import", 17<<20); code != http.StatusRequestEntityTooLarge {
		t.Errorf("非 /v1 路径 17MB 应 413（16MB 兜底），得到 %d", code)
	}
}

// TestApplyBodyLimitEnv 环境变量：合法生效、非法/越界退回默认。
func TestApplyBodyLimitEnv(t *testing.T) {
	old := chatBodyMax
	defer func() { chatBodyMax = old }()

	reset := func() { chatBodyMax = chatBodyMaxDefault }

	// 未设置：保持默认。
	reset()
	t.Setenv(bodyLimitEnvKey, "")
	applyBodyLimitEnv()
	if chatBodyMax != chatBodyMaxDefault {
		t.Errorf("未设置时应保持默认 %d，得到 %d", chatBodyMaxDefault, chatBodyMax)
	}

	// 合法值生效。
	reset()
	t.Setenv(bodyLimitEnvKey, "64")
	applyBodyLimitEnv()
	if chatBodyMax != 64<<20 {
		t.Errorf("64 应生效为 %d，得到 %d", int64(64<<20), chatBodyMax)
	}

	// 非法与越界：一律退回默认（不能让配置写错把服务拖垮）。
	for _, raw := range []string{"abc", "-1", "0", "2", "999", "64.5"} {
		reset()
		t.Setenv(bodyLimitEnvKey, raw)
		applyBodyLimitEnv()
		if chatBodyMax != chatBodyMaxDefault {
			t.Errorf("%s=%q 应退回默认 %d，得到 %d", bodyLimitEnvKey, raw, chatBodyMaxDefault, chatBodyMax)
		}
	}
}

// TestTooLargeHintMentionsEnv 413 的 gateway_hint 必须给出**可执行**的下一步。
//
// 只说 "split or shrink the payload" 不够：agent 客户端的长上下文请求没法拆，
// 运维需要知道"这是网关侧参数、可以调、调的范围是多少"。
func TestTooLargeHintMentionsEnv(t *testing.T) {
	px := &Proxy{}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	px.reportBodyReadFailure(rec, req, &http.MaxBytesError{Limit: chatBodyMax}, 1234, 2*time.Second)

	body := rec.Body.String()
	for _, want := range []string{bodyLimitEnvKey, "exceeds the gateway limit", "no upstream account was used"} {
		if !strings.Contains(body, want) {
			t.Errorf("413 响应应包含 %q，实际：%s", want, body)
		}
	}
}
