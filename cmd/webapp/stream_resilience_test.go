// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// brokenBody 模拟「上游流读到一半断掉」：把 script 吐完，再返回读错误。
//
// 为什么需要它：真实断流的三种形态（网关侧 http.Client 超时、上游掐断、
// 网络抖动）在单测里无法稳定复现，但它们对读取方都是同一个语义 ——
// 先拿到若干字节，随后的 Read 返回非 EOF 错误。bufio.Scanner 会把这个错误
// 记进 Err()，正是本次修复要检查的信号。
type brokenBody struct {
	r      *strings.Reader
	broken bool
}

func (b *brokenBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err == io.EOF && !b.broken {
		b.broken = true
		return n, errors.New("connection reset by peer")
	}
	return n, err
}

func (b *brokenBody) Close() error { return nil }

// TestChatClientForDropsTimeoutOnStream 流式转发必须脱掉 http.Client 的总超时。
//
// http.Client.Timeout 的语义包含「读取响应体」，180s 硬超时会把长回答
// （Agent 多轮工具调用 / 深度思考，轻易超过 3 分钟）从网关这一侧掐断——
// 这正是「执行到一半会话中断」的一个根因。非流式（服务端聚合）仍要保留上界，
// 否则一个卡死的上游会长期占住请求。
func TestChatClientForDropsTimeoutOnStream(t *testing.T) {
	store, px, _ := newTestApp(t)
	defer store.Close() // Windows：不关库会让 t.TempDir 清理报「文件被占用」
	defer px.Close()

	plain, err := px.chatClientFor("uid-unknown", false)
	if err != nil {
		t.Fatalf("非流式取客户端失败: %v", err)
	}
	if plain.Timeout != 180*time.Second {
		t.Fatalf("非流式应保留 180s 上界，得 %v", plain.Timeout)
	}

	stream, err := px.chatClientFor("uid-unknown", true)
	if err != nil {
		t.Fatalf("流式取客户端失败: %v", err)
	}
	if stream.Timeout != 0 {
		t.Fatalf("流式必须无总超时（180s 会掐断长流），得 %v", stream.Timeout)
	}
	// 必须复用同一 Transport：脱掉的只是 Client 级时限，出口代理 / 连接池设置不能丢。
	if stream.Transport != plain.Transport {
		t.Fatal("流式客户端必须复用同一 Transport（否则丢失代理与连接池配置）")
	}
	// 非流式仍应是原对象（不做无谓复制）。
	plain2, _ := px.chatClientFor("uid-unknown", false)
	if plain2 != plain {
		t.Fatal("非流式路径不应重建客户端")
	}
}

// TestStreamSSESynthesizesDone 上游漏发 [DONE] 时必须兜底补写。
//
// OpenAI 兼容客户端普遍以 [DONE]（或 finish_reason）判定「回答结束」，
// 缺失时客户端会一直挂着等 —— 用户侧看到的就是「会话中断」。
func TestStreamSSESynthesizesDone(t *testing.T) {
	body := io.NopCloser(strings.NewReader("data: {\"choices\":[]}\n\n"))
	rec := httptest.NewRecorder()
	_, _, err := streamSSE(rec, &http.Response{Body: body})
	if err != nil {
		t.Fatalf("正常读完不应报错: %v", err)
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("上游漏发 [DONE] 时必须兜底补写，实际输出: %q", rec.Body.String())
	}
}

// TestStreamSSEDoneNotDuplicated 上游自带 [DONE] 时不得重复写。
func TestStreamSSEDoneNotDuplicated(t *testing.T) {
	body := io.NopCloser(strings.NewReader("data: {\"choices\":[]}\n\ndata: [DONE]\n\n"))
	rec := httptest.NewRecorder()
	if _, _, err := streamSSE(rec, &http.Response{Body: body}); err != nil {
		t.Fatalf("正常读完不应报错: %v", err)
	}
	if n := strings.Count(rec.Body.String(), "[DONE]"); n != 1 {
		t.Fatalf("[DONE] 应恰好出现 1 次，得 %d：%q", n, rec.Body.String())
	}
}

// TestStreamSSEInterruptedStillWritesDone 断流时：既要暴露错误，也要补 [DONE]。
//
// 这条锁的是「中断不再静默」：返回值带错误（供上层记日志/实时事件/失败流水），
// 同时客户端仍能正常收尾。
func TestStreamSSEInterruptedStillWritesDone(t *testing.T) {
	body := &brokenBody{r: strings.NewReader("data: {\"choices\":[]}\n\n")}
	rec := httptest.NewRecorder()
	_, _, err := streamSSE(rec, &http.Response{Body: body})
	if err == nil {
		t.Fatal("上游断流必须返回错误（此前被静默吞掉，失败流被记成成功）")
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("断流时仍须补 [DONE] 让客户端收尾，实际输出: %q", rec.Body.String())
	}
}

// TestAggregateSSEInterruptedReturnsBadGateway 非流式聚合路径中断必须报错且不吐半截内容。
//
// 聚合路径不逐帧发给客户端，而是读完整流再写 JSON —— 中途断掉时，旧实现会把
// 半截 content 当作**完整回答**以 200 发出，客户端完全察觉不到后半段丢了。
func TestAggregateSSEInterruptedReturnsBadGateway(t *testing.T) {
	body := &brokenBody{r: strings.NewReader(
		"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"前半段\"}}]}\n\n")}
	rec := httptest.NewRecorder()
	ui, _, err := aggregateSSE(rec, &http.Response{Body: body})
	if err == nil {
		t.Fatal("上游中断必须返回错误，否则残缺回答被当成完整响应")
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("中断应回 502，得 %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "前半段") {
		t.Fatalf("中断时不得把残缺内容发出去，实际: %q", rec.Body.String())
	}
	if ui.Total() != 0 {
		t.Fatalf("中断的响应不该产生用量，得 %d", ui.Total())
	}
}

// TestUsageLogStoresSelectionTrace 选号轨迹（粘性哈希 + 尝试次数）必须落库并可读回。
//
// 这是本次排查补的观测盲区：此前流水只有最终 upstream_uid，无法回答
// 「同一场对话的多轮是否落到不同账号」。有了 sticky_key_hash，同一场的多轮可被串起来；
// attempts>1 则直接说明那一轮发生过失败换号。
func TestUsageLogStoresSelectionTrace(t *testing.T) {
	store, _, _ := newTestApp(t)
	defer store.Close()

	if err := store.InsertUsageLog(&UsageLog{
		UserID: 1, KeyID: 2, Model: "m", Status: "ok",
		StickyKeyHash: "abc123def456", Attempts: 2,
	}); err != nil {
		t.Fatalf("InsertUsageLog: %v", err)
	}
	if err := store.InsertUsageLog(&UsageLog{
		UserID: 1, KeyID: 2, Model: "m", Status: "ok",
		StickyKeyHash: "abc123def456", Attempts: 1,
	}); err != nil {
		t.Fatalf("InsertUsageLog: %v", err)
	}

	rows, err := store.UsageLogsBySticky("abc123def456", 10)
	if err != nil {
		t.Fatalf("UsageLogsBySticky: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("同一粘性键应查到 2 条，得 %d", len(rows))
	}
	for _, r := range rows {
		if r.StickyKeyHash != "abc123def456" {
			t.Fatalf("粘性哈希未落库: %q", r.StickyKeyHash)
		}
		if r.Attempts != 1 && r.Attempts != 2 {
			t.Fatalf("尝试次数未落库: %d", r.Attempts)
		}
	}

	// 空 hash 不查历史（那些本来就不是同一场对话），返回空集而非全表。
	empty, err := store.UsageLogsBySticky("", 10)
	if err != nil {
		t.Fatalf("UsageLogsBySticky(空): %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("空哈希应返回空集，得 %d 条", len(empty))
	}
	// 列表读取路径也要带上新列（否则管理端/账单页看不到轨迹）。
	list, err := store.ListUsageLogs(1, 10, 0)
	if err != nil {
		t.Fatalf("ListUsageLogs: %v", err)
	}
	if len(list) != 2 || list[0].StickyKeyHash != "abc123def456" {
		t.Fatalf("ListUsageLogs 未带回选号轨迹: %+v", list)
	}
}

// TestStreamSSEReportsModelErrorFrame 流内模型级错误帧必须被识别并回传给调用方。
//
// 这是 (账号, 模型) 冷却的关键上游形态：错误放在**已经 200 的流**里，
// 客户端看得见但池侧此前完全不知道 —— 于是该 (账号,模型) 不冷却，
// 下一个同模型请求照样被路由过去，再次撞同一个错。
func TestStreamSSEReportsModelErrorFrame(t *testing.T) {
	frame := `data: {"error":{"code":6004,"message":"model rate limited","resets_at":"2030-01-01T00:00:00Z"}}` + "\n\n"
	rec := httptest.NewRecorder()
	_, me, err := streamSSE(rec, &http.Response{Body: io.NopCloser(strings.NewReader(frame))})
	if err != nil {
		t.Fatalf("读流不应报错: %v", err)
	}
	if me.Code != 6004 {
		t.Fatalf("应识别出模型级错误帧 code=6004，got %d（原文 %q）", me.Code, string(me.Raw))
	}
	if !strings.Contains(string(me.Raw), "resets_at") {
		t.Errorf("应保留该帧原文（供重置时刻解析），got %q", string(me.Raw))
	}
	// 错误帧仍必须**透传**给客户端（不能因为识别了就吞掉）。
	if !strings.Contains(rec.Body.String(), `"code":6004`) {
		t.Errorf("错误帧应原样透传，实际输出 %q", rec.Body.String())
	}
}

// TestStreamSSEIgnoresNonModelErrorFrame 非模型级错误帧不得被误当成模型级信号。
//
// 只认 6004 / 11102：其他 code（内容审核、参数错误…）有自己的处置链路，
// 在流式热路径上重复分类只会制造两套口径。
func TestStreamSSEIgnoresNonModelErrorFrame(t *testing.T) {
	for _, line := range []string{
		`data: {"error":{"code":11140,"message":"request illegal"}}`,
		`data: {"error":{"code":12153,"message":"session dead"}}`,
		`data: {"choices":[{"delta":{"content":"no error keyword"}}]}`,
		`data: {"choices":[{"delta":{"content":"mentions error but no code"}}]}`,
		`data: [DONE]`,
		`: comment line`,
	} {
		if me := detectStreamModelErrorFrame(line); me.Code != 0 {
			t.Errorf("%q 不应被识别为模型级错误帧，got code=%d", line, me.Code)
		}
	}
	// 11102（该后端无此模型）同样是**模型级**，必须认。
	if me := detectStreamModelErrorFrame(`data: {"error":{"code":11102}}`); me.Code != 11102 {
		t.Errorf("11102 是模型级信号，应被识别，got %d", me.Code)
	}
}
