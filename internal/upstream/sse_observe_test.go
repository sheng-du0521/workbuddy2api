// 流透传的「错误帧观察者」测试（v0.31.3）。
//
// 观察者是池侧处置的入口：上游把错误放进已经 200 的流里时，使用方需要拿到那帧
// 原文才能对 (账号, 模型) 写冷却。三条纪律必须钉住：
//  1. 上游 error 帧 → 回调（原文，帧写出之前）；
//  2. **本地**空流兜底帧 → 不回调（它不是上游说的话）；
//  3. 回调 panic 不得击穿流透传（补充功能不拖垮主路径）。
package upstream

import (
	"net/http/httptest"
	"strings"
	"testing"
)

const observeCleanFrame = `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}`
const observeErrFrame = `{"error":{"message":"您的使用量已超出频率限制","code":"6004","requestId":"r1"}}`

// TestStreamObserveCallsForUpstreamErrorFrame 上游 error 帧必须回调，且原文完整。
func TestStreamObserveCallsForUpstreamErrorFrame(t *testing.T) {
	raw := "data: " + observeCleanFrame + "\n\n" +
		"data: " + observeErrFrame + "\n\n" +
		"data: [DONE]\n\n"
	var got []string
	rec := httptest.NewRecorder()
	if err := StreamObserve(rec, strings.NewReader(raw), nil, func(p string) { got = append(got, p) }); err != nil {
		t.Fatalf("StreamObserve err=%v", err)
	}
	if len(got) != 1 {
		t.Fatalf("应恰好回调一次（只有一帧 error），got %d: %v", len(got), got)
	}
	if got[0] != observeErrFrame {
		t.Errorf("回调必须是帧原文（含尾部字段，判据依赖它），got %q", got[0])
	}
	// 透传本身不受影响：干净帧 + error 帧 + 恰好一个 [DONE]。
	body := rec.Body.String()
	if !strings.Contains(body, `"content":"hi"`) || !strings.Contains(body, `"code":"6004"`) {
		t.Errorf("帧透传被观察者破坏：%q", body)
	}
	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Errorf("[DONE] 应恰好一个，got %d", n)
	}
}

// TestStreamObserveSkipsLocalEmptyStreamFrame 本地空流兜底帧不回调。
//
// 那一帧是**网关自己造的**（code 是字符串 "upstream_parse"），把它喂给池侧处置
// 等于"因为上游什么都没说而罚某个账号"，方向完全错。
func TestStreamObserveSkipsLocalEmptyStreamFrame(t *testing.T) {
	calls := 0
	rec := httptest.NewRecorder()
	err := StreamObserve(rec, strings.NewReader(""), nil, func(string) { calls++ })
	if !IsEmptyStreamError(err) {
		t.Fatalf("空流应返回 errEmptyStream，got %v", err)
	}
	if calls != 0 {
		t.Errorf("本地兜底帧不得触发观察者，got %d 次", calls)
	}
	if !strings.Contains(rec.Body.String(), "empty upstream stream") {
		t.Errorf("空流兜底帧应照常写出：%q", rec.Body.String())
	}
}

// TestStreamObservePanicIsolated 观察者 panic 不得击穿透传。
func TestStreamObservePanicIsolated(t *testing.T) {
	raw := "data: " + observeCleanFrame + "\n\n" +
		"data: " + observeErrFrame + "\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("观察者 panic 泄漏到了调用方：%v", r)
			}
		}()
		if err := StreamObserve(rec, strings.NewReader(raw), nil, func(string) { panic("boom") }); err != nil {
			t.Fatalf("StreamObserve err=%v", err)
		}
	}()
	if !strings.Contains(rec.Body.String(), `"content":"hi"`) {
		t.Errorf("panic 后透传不完整：%q", rec.Body.String())
	}
}

// TestAggregateObserveCallsForErrorFrame 非流式（聚合）路径同样能看到 error 帧。
func TestAggregateObserveCallsForErrorFrame(t *testing.T) {
	raw := "data: " + observeErrFrame + "\n\n" + "data: [DONE]\n\n"
	var got []string
	if _, err := AggregateObserve(strings.NewReader(raw), func(p string) { got = append(got, p) }); err != nil {
		t.Fatalf("AggregateObserve err=%v", err)
	}
	if len(got) != 1 || got[0] != observeErrFrame {
		t.Fatalf("聚合路径应回调一次且为原文，got %v", got)
	}
	// 计数口径不变：error 帧仍是"有效数据帧"，不该被误判成空流。
	if _, err := AggregateObserve(strings.NewReader(raw), nil); err != nil {
		t.Errorf("error 帧存在时不该报空流：%v", err)
	}
}

// TestAggregateNilObserverUnchanged nil 观察者与 Aggregate 行为一致（零开销回归锚）。
func TestAggregateNilObserverUnchanged(t *testing.T) {
	raw := "data: " + observeCleanFrame + "\n\n" + "data: [DONE]\n\n"
	a, errA := Aggregate(strings.NewReader(raw))
	b, errB := AggregateObserve(strings.NewReader(raw), nil)
	if (errA == nil) != (errB == nil) {
		t.Fatalf("错误不一致：%v vs %v", errA, errB)
	}
	if a["id"] != b["id"] {
		t.Errorf("结果不一致：%v vs %v", a, b)
	}
}
