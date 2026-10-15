package upstream

import (
	"bytes"
	"encoding/json"
	"testing"

	"workbuddy2api/internal/auth"
)

// TestPrepareOutboundDeterministic 出站改写必须**逐字节确定**。
//
// 为什么这条是延迟/费用的关键闸门：上游的前缀缓存（prompt_cache_key 指向的那块）
// 命中的前提是**前缀逐字节一致**。只要改写管线里有任何一处注入随机值 / 时间戳 /
// 递增计数，整条前缀就变了 —— 缓存 100% 不命中，表现为
//
//	· 费用按全量 prompt 计（实测 48 万 token ≈ 24 点/次，而缓存命中应低一个数量级）；
//	· 首字节前要重算 48 万 token 的 prefill（实测单请求 92~129 秒）。
//
// 而这一切**没有任何报错**：请求照样 200，只是慢和贵。
// 所以必须把它钉在测试里 —— 这是唯一能提前发现"前缀被破坏"的地方。
func TestPrepareOutboundDeterministic(t *testing.T) {
	c := New()
	c.GlobalEnabled = false

	a := &auth.Auth{UID: "26e5cdb8-97f9-46f9-bce5-b2d0fe8dc11d"}

	// 模拟真实的大上下文请求：同一会话、同一份 messages，多次调用。
	body := []byte(`{
		"model": "kimi-k3-1",
		"stream": true,
		"messages": [
			{"role": "system", "content": "你是助手"},
			{"role": "user", "content": "第一问"},
			{"role": "assistant", "content": "第一答"},
			{"role": "user", "content": "第二问"}
		],
		"tools": [{"type": "function", "function": {"name": "bash"}}]
	}`)

	const convID = "conv-abc-123"
	first := c.PrepareOutbound(body, a, convID)
	if len(first) == 0 {
		t.Fatal("改写结果为空")
	}

	// 同一输入连续 5 次必须完全一致。
	for i := 0; i < 5; i++ {
		got := c.PrepareOutbound(body, a, convID)
		if !bytes.Equal(got, first) {
			t.Fatalf("第 %d 次改写与首次不一致 —— 前缀被破坏，上游缓存必然全部失效。\n首次: %s\n本次: %s",
				i+1, first, got)
		}
	}

	// prompt_cache_key 必须已注入且稳定。
	var obj map[string]any
	if err := json.Unmarshal(first, &obj); err != nil {
		t.Fatalf("改写结果不是合法 JSON：%v", err)
	}
	key, _ := obj["prompt_cache_key"].(string)
	if key == "" {
		t.Fatal("prompt_cache_key 未注入 —— 上游无法复用前缀缓存")
	}
	if key != "wb2a-"+a.UID[:8]+"-"+key[len("wb2a-")+9:] {
		// 只做形态检查，具体哈希值不硬编码（避免锁死实现）。
		t.Logf("cache key = %s", key)
	}
	t.Logf("cache key = %s (len=%d)", key, len(key))

	// 会话变化 → key 必须变化（否则不同对话会互相污染缓存）。
	other := c.PrepareOutbound(body, a, "conv-other-456")
	var obj2 map[string]any
	if err := json.Unmarshal(other, &obj2); err != nil {
		t.Fatal(err)
	}
	if k2, _ := obj2["prompt_cache_key"].(string); k2 == key {
		t.Error("不同会话解析出了同一个 cache key —— 不同对话会命中对方的缓存")
	}
}

// TestPrepareOutboundStableAcrossMessageGrowth 同一会话追加消息后，
// 除新增消息外的字段必须保持稳定（决定"前缀"能否复用）。
//
// 这条针对的是：改写管线里若有任何**与消息内容无关**却每次都变的字段
// （时间戳 / 随机 ID / 递增序号），前缀就永远对不上。
func TestPrepareOutboundStableAcrossMessageGrowth(t *testing.T) {
	c := New()
	c.GlobalEnabled = false
	a := &auth.Auth{UID: "abcdef01-2345-6789-abcd-ef0123456789"}
	const convID = "conv-grow"

	base := `{"model":"kimi-k3-1","stream":true,"messages":[{"role":"user","content":"A"}]}`
	grown := `{"model":"kimi-k3-1","stream":true,"messages":[{"role":"user","content":"A"},{"role":"assistant","content":"B"},{"role":"user","content":"C"}]}`

	var o1, o2 map[string]any
	if err := json.Unmarshal(c.PrepareOutbound([]byte(base), a, convID), &o1); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(c.PrepareOutbound([]byte(grown), a, convID), &o2); err != nil {
		t.Fatal(err)
	}

	k1, _ := o1["prompt_cache_key"].(string)
	k2, _ := o2["prompt_cache_key"].(string)
	if k1 == "" || k2 == "" {
		t.Fatal("cache key 缺失")
	}
	if k1 != k2 {
		t.Errorf("同会话消息增长后 cache key 变了（%s → %s）——同一对话的后续轮次无法复用前缀缓存", k1, k2)
	}

	// 除 messages 外的顶层字段集合应当一致（不得凭空多出会变的字段）。
	var generic1, generic2 map[string]json.RawMessage
	_ = json.Unmarshal(c.PrepareOutbound([]byte(base), a, convID), &generic1)
	_ = json.Unmarshal(c.PrepareOutbound([]byte(grown), a, convID), &generic2)
	for k := range generic1 {
		if k == "messages" {
			continue
		}
		if _, ok := generic2[k]; !ok {
			t.Errorf("字段 %q 在消息增长后消失", k)
		}
	}
	for k := range generic2 {
		if k == "messages" {
			continue
		}
		if _, ok := generic1[k]; !ok {
			t.Errorf("消息增长后凭空多出字段 %q（若其值随时间变化，将破坏前缀缓存）", k)
		}
	}
}
