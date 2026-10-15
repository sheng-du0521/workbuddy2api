package upstream

import "testing"

// ======================================================================
// v0.26.7 · 上游 usage 解析的唯一口径
//
// 这批用例钉的是**两条转发链路共用一份解析**这件事本身。收敛之前各写各的，
// 已经产生的真实差异有两处（见 usage.go 顶部注释）：webapp 漏采缓存三字段、
// 旧网关只认带空格的 `data: ` 前缀。前者让"缓存有没有生效"不可观测，
// 后者会让整帧 usage（含计费依据 credit）被静默丢掉。
// ======================================================================

// TestParseUsageLineAcceptsBothDataPrefixes 两种 data 前缀都必须认。
//
// 这是两条链路**实际存在过的分歧**：旧网关只认 `"data: "`。上游（或其镜像）
// 若发 `data:{"usage":...}`，旧网关会整帧跳过 —— 而漏掉的正是计费依据。
func TestParseUsageLineAcceptsBothDataPrefixes(t *testing.T) {
	body := `{"usage":{"prompt_tokens":100,"completion_tokens":7,"credit":0.25,` +
		`"prompt_cache_hit_tokens":90,"prompt_cache_miss_tokens":10,"prompt_cache_write_tokens":80}}`

	for _, line := range []string{"data: " + body, "data:" + body} {
		f, ok := ParseUsageLine(line)
		if !ok {
			t.Fatalf("前缀 %q 未被识别（这会让整帧 usage 被静默丢弃）", line[:6])
		}
		if f.PromptTokens != 100 || f.CompletionTokens != 7 {
			t.Errorf("token 解析不符：%+v", f)
		}
		if !f.HasCredit || f.Credit != 0.25 {
			t.Errorf("credit 解析不符：%+v", f)
		}
		if f.CacheHitTokens != 90 || f.CacheMissTokens != 10 || f.CacheWriteTokens != 80 {
			t.Errorf("缓存三计数解析不符：%+v", f)
		}
	}
}

// TestParseUsageLineCreditMissingVsZero 「缺失」与「显式 0」必须可分 ——
// 缺失≠0：把缺观测当 0 成本写进账本，会让收费账号被误判为免费层。
func TestParseUsageLineCreditMissingVsZero(t *testing.T) {
	miss, ok := ParseUsageLine(`data: {"usage":{"prompt_tokens":10,"completion_tokens":1}}`)
	if !ok {
		t.Fatal("应解析出 usage")
	}
	if miss.HasCredit {
		t.Error("字段缺失时 HasCredit 必须为 false")
	}
	if miss.Credit != 0 {
		t.Errorf("缺失时 Credit 应为 0，得 %v", miss.Credit)
	}

	zero, ok := ParseUsageLine(`data: {"usage":{"prompt_tokens":10,"completion_tokens":1,"credit":0}}`)
	if !ok {
		t.Fatal("应解析出 usage")
	}
	if !zero.HasCredit {
		t.Error("显式 credit:0 必须 HasCredit=true（这是合法的免费观测）")
	}
}

// TestParseUsageLineNonUsageLines 流里绝大多数行不含 usage —— 必须 ok=false
// 而不是把它当错误或当零值观测。零值观测会被计费与统计当成"真的用了 0"。
func TestParseUsageLineNonUsageLines(t *testing.T) {
	cases := []string{
		"",                     // 空行（SSE 分隔符）
		"data: [DONE]",         // 结束帧
		"data:[DONE]",          // 无空格形态的结束帧
		`data: {"choices":[]}`, // 普通增量帧
		`: keep-alive`,         // SSE 注释帧
		`data: {bad json`,      // 畸形 JSON
		`event: message`,       // 事件名行
		`data: {"usage":null}`, // usage 显式为 null
	}
	for _, line := range cases {
		if f, ok := ParseUsageLine(line); ok {
			t.Errorf("行 %q 不该解析出 usage，却得到 %+v", line, f)
		}
	}
}

// TestStripSSEDataPrefix 前缀剥离：长前缀优先，且剥完不留前导空格。
func TestStripSSEDataPrefix(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{`data: {"a":1}`, `{"a":1}`, true},
		{`data:{"a":1}`, `{"a":1}`, true},
		{"data: {\"a\":1}\r\n", `{"a":1}`, true}, // CRLF 行尾
		{"data: {\"a\":1}\n", `{"a":1}`, true},
		{"data:", "", true}, // 只有前缀
		{"data: [DONE]", "[DONE]", true},
		{": comment", "", false}, // 注释帧不是数据帧
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := StripSSEDataPrefix(c.in)
		if ok != c.wantOK || got != c.want {
			t.Errorf("StripSSEDataPrefix(%q) = (%q, %v)，期望 (%q, %v)", c.in, got, ok, c.want, c.wantOK)
		}
	}
}

// TestParseUsageObject 非流式路径与流式**同一口径**。
func TestParseUsageObject(t *testing.T) {
	f, ok := ParseUsageObject([]byte(`{"prompt_tokens":1000,"completion_tokens":50,` +
		`"prompt_cache_hit_tokens":900,"prompt_cache_miss_tokens":100,` +
		`"prompt_cache_write_tokens":880,"credit":1.5}`))
	if !ok {
		t.Fatal("应解析成功")
	}
	if f.PromptTokens != 1000 || f.CompletionTokens != 50 {
		t.Errorf("token 不符：%+v", f)
	}
	if f.CacheHitTokens != 900 || f.CacheMissTokens != 100 || f.CacheWriteTokens != 880 {
		t.Errorf("缓存不符：%+v", f)
	}
	if !f.HasCredit || f.Credit != 1.5 {
		t.Errorf("credit 不符：%+v", f)
	}
	if f.Total() != 1050 {
		t.Errorf("Total = %d，期望 1050", f.Total())
	}

	// 空 / 畸形 / 抓错对象：一律 ok=false，不 panic、不返回假观测。
	for _, raw := range []string{"", `{`, `null`, `{}`, `{"choices":[]}`, `[]`} {
		if _, ok := ParseUsageObject([]byte(raw)); ok {
			t.Errorf("输入 %q 不该产生 usage 观测", raw)
		}
	}
	// 只有 credit（无 token）仍算有效观测 —— 计费依据在这儿。
	if f, ok := ParseUsageObject([]byte(`{"credit":0}`)); !ok || !f.HasCredit {
		t.Errorf("仅 credit 的响应应被采信：ok=%v frame=%+v", ok, f)
	}
}

// TestUsageFrameTotal 合计口径：prompt + completion。
func TestUsageFrameTotal(t *testing.T) {
	if got := (UsageFrame{PromptTokens: 3, CompletionTokens: 4}).Total(); got != 7 {
		t.Errorf("Total = %d，期望 7", got)
	}
	if got := (UsageFrame{}).Total(); got != 0 {
		t.Errorf("零值 Total = %d，期望 0", got)
	}
}
