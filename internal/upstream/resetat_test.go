// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package upstream

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// resetAtNow 测试基准时刻（固定值，不用 time.Now：本文件测的是纯函数，
// 固定 now 才能对"UTC+8 解释"这类时区断言做逐字比较）。
var resetAtNow = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

// TestResolveResetAtBodyTextUTC8 body 文案里的无后缀墙钟必须按 UTC+8 解释。
//
// 这是本文件最重要的一条：按 UTC 解释会整体偏 8 小时（偏早 → 冷却几乎立刻
// 到期、窗口内反复撞墙；偏晚 → 白扣一个号半天）。
func TestResolveResetAtBodyTextUTC8(t *testing.T) {
	body := []byte(`{"code":6004,"msg":"该模型将在 2026-10-15 21:30:00 重置","data":null}`)
	got, src := ResolveResetAt(hdr(), body, resetAtNow)
	if src != ResetBodyText {
		t.Fatalf("来源 = %q, want %q", src, ResetBodyText)
	}
	// 21:30 UTC+8 == 13:30 UTC。
	want := time.Date(2026, 10, 15, 13, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("解析结果 = %v, want %v（UTC+8 口径）", got.UTC(), want)
	}
}

// TestResolveResetAtBodyTextEN 英文文案 `reset at <t>` 同样命中（上游双语形态）。
func TestResolveResetAtBodyTextEN(t *testing.T) {
	body := []byte(`{"code":6004,"msg":"model quota exceeded, reset at 2026-10-15 21:30:00"}`)
	got, src := ResolveResetAt(hdr(), body, resetAtNow)
	if src != ResetBodyText {
		t.Fatalf("来源 = %q, want body-text", src)
	}
	if want := time.Date(2026, 10, 15, 13, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("= %v, want %v", got.UTC(), want)
	}
}

// TestResolveResetAtBodyTextWinsOverHeader body 文案优先于响应头。
//
// 优先级不是"谁更保守"而是"谁更权威"：头只说明"多久后可以再试"，
// 文案是上游直接写出的恢复时刻。
func TestResolveResetAtBodyTextWinsOverHeader(t *testing.T) {
	body := []byte(`{"code":6004,"msg":"将在 2026-10-15 21:30:00 重置"}`)
	got, src := ResolveResetAt(hdr("Retry-After", "3600"), body, resetAtNow)
	if src != ResetBodyText {
		t.Fatalf("来源 = %q, want body-text（文案优先于头）", src)
	}
	if want := time.Date(2026, 10, 15, 13, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("= %v, want %v", got.UTC(), want)
	}
}

// TestResolveResetAtJSONNumberEpoch 结构化字段发 epoch 秒 → 直接换算（时区无关）。
func TestResolveResetAtJSONNumberEpoch(t *testing.T) {
	want := time.Date(2026, 10, 15, 14, 0, 0, 0, time.UTC)
	body := []byte(`{"code":6004,"data":{"resets_at":` + itoa(want.Unix()) + `}}`)
	got, src := ResolveResetAt(hdr(), body, resetAtNow)
	if src != ResetBodyJSON {
		t.Fatalf("来源 = %q, want body-json", src)
	}
	if !got.Equal(want) {
		t.Errorf("= %v, want %v", got.UTC(), want)
	}
}

// TestResolveResetAtJSONEpochMillis 13 位毫秒口径不能被当成秒
// （当成秒会算出五万年后到期 = 永久摘除该模型）。
func TestResolveResetAtJSONEpochMillis(t *testing.T) {
	want := time.Date(2026, 10, 15, 14, 0, 0, 0, time.UTC)
	body := []byte(`{"data":{"reset_at":` + itoa(want.UnixMilli()) + `}}`)
	got, src := ResolveResetAt(hdr(), body, resetAtNow)
	if src != ResetBodyJSON {
		t.Fatalf("来源 = %q, want body-json", src)
	}
	if !got.Equal(want) {
		t.Errorf("= %v, want %v（毫秒必须除 1000）", got.UTC(), want)
	}
}

// TestResolveResetAtJSONRFC3339HonoursOffset 带偏移的 RFC3339 必须尊重自带偏移，
// 不能按 UTC+8 二次解释。
func TestResolveResetAtJSONRFC3339HonoursOffset(t *testing.T) {
	// +08:00 的 22:00 == 14:00 UTC（基准 now = 12:00 UTC，必须晚于它才不算过期）。
	body := []byte(`{"data":{"reset_time":"2026-10-15T22:00:00+08:00"}}`)
	got, _ := ResolveResetAt(hdr(), body, resetAtNow)
	if want := time.Date(2026, 10, 15, 14, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("= %v, want %v", got.UTC(), want)
	}
	// Z 后缀同理（= UTC 时刻本身）。
	body2 := []byte(`{"data":{"reset_time":"2026-10-15T20:00:00Z"}}`)
	got2, _ := ResolveResetAt(hdr(), body2, resetAtNow)
	if want := time.Date(2026, 10, 15, 20, 0, 0, 0, time.UTC); !got2.Equal(want) {
		t.Errorf("Z 形态 = %v, want %v", got2.UTC(), want)
	}
}

// TestResolveResetAtJSONRelativeSeconds `resets_in` 是"多久以后"，加 now。
func TestResolveResetAtJSONRelativeSeconds(t *testing.T) {
	body := []byte(`{"data":{"resets_in":1800}}`)
	got, src := ResolveResetAt(hdr(), body, resetAtNow)
	if src != ResetBodyJSON {
		t.Fatalf("来源 = %q, want body-json", src)
	}
	if want := resetAtNow.Add(30 * time.Minute); !got.Equal(want) {
		t.Errorf("= %v, want %v", got.UTC(), want)
	}
}

// TestResolveResetAtSmallNumberIsRelative 小数字按相对秒解释，不能当成 1970 年的 epoch。
func TestResolveResetAtSmallNumberIsRelative(t *testing.T) {
	body := []byte(`{"data":{"reset_at":3600}}`)
	got, _ := ResolveResetAt(hdr(), body, resetAtNow)
	if want := resetAtNow.Add(time.Hour); !got.Equal(want) {
		t.Errorf("= %v, want %v（小数值按相对秒）", got.UTC(), want)
	}
}

// TestResolveResetAtRelativeKeyRejectsWallClock reset_in 族给墙钟属于上游自相矛盾 → 丢弃。
func TestResolveResetAtRelativeKeyRejectsWallClock(t *testing.T) {
	body := []byte(`{"data":{"resets_in":"2026-10-15 21:00:00"}}`)
	got, src := ResolveResetAt(hdr(), body, resetAtNow)
	if src != ResetNone || !got.IsZero() {
		t.Errorf("相对键给墙钟应被丢弃，得 (%v, %q)", got, src)
	}
}

// TestResolveResetAtHeaderFallback 三级里只有头可用时用头（并标注来源）。
func TestResolveResetAtHeaderFallback(t *testing.T) {
	got, src := ResolveResetAt(hdr("Retry-After", "600"), []byte(`{"code":429}`), resetAtNow)
	if src != ResetHeader {
		t.Fatalf("来源 = %q, want header", src)
	}
	if want := resetAtNow.Add(10 * time.Minute); !got.Equal(want) {
		t.Errorf("= %v, want %v", got.UTC(), want)
	}
}

// TestResolveResetAtHeaderFamily x-ratelimit-reset 是 epoch 时刻而非时长。
func TestResolveResetAtHeaderFamily(t *testing.T) {
	// x-ratelimit-reset 的换算要用**真实时钟**（parseRetryNumber 内部走
	// time.Until）：生产环境里调用方传的 now 就是真实时钟，两者一致；
	// 这里显式用 time.Now() 而不是固定基准，并留 2 秒容差（跨秒边界）。
	realNow := time.Now()
	epoch := realNow.Add(45 * time.Minute).Unix()
	got, src := ResolveResetAt(hdr("X-Ratelimit-Reset", itoa(epoch)), nil, realNow)
	if src != ResetHeader {
		t.Fatalf("来源 = %q, want header", src)
	}
	if diff := got.Unix() - epoch; diff > 2 || diff < -2 {
		t.Errorf("= %v, want ≈epoch %d（差 %ds）", got.Unix(), epoch, diff)
	}
	// Retry-After-Ms 是毫秒时长。
	got2, src2 := ResolveResetAt(hdr("Retry-After-Ms", "90000"), nil, resetAtNow)
	if src2 != ResetHeader || !got2.Equal(resetAtNow.Add(90*time.Second)) {
		t.Errorf("Retry-After-Ms = (%v, %q), want +90s", got2, src2)
	}
}

// TestResolveResetAtMissing 三级全无 → ResetNone + 零值（调用方必须回退有界退避）。
func TestResolveResetAtMissing(t *testing.T) {
	got, src := ResolveResetAt(hdr(), []byte(`{"code":429,"msg":"too many requests"}`), resetAtNow)
	if src != ResetNone {
		t.Errorf("来源 = %q, want none", src)
	}
	if !got.IsZero() {
		t.Errorf("缺失时必须是零值，得 %v", got)
	}
}

// TestResolveResetAtExpired 已过期的重置时间判为 expired + 零值。
//
// 这是刻意的**不**选"立刻恢复"：立刻恢复会让同一秒内的并发重试再吃一次
// 同样的错误（热循环）。判为"没有时间信息"后回落到秒级有界退避即可。
func TestResolveResetAtExpired(t *testing.T) {
	for name, body := range map[string][]byte{
		"文案已过期":     []byte(`{"msg":"将在 2026-10-15 11:00:00 重置"}`),
		"epoch 已过期": []byte(`{"data":{"resets_at":` + itoa(resetAtNow.Add(-time.Hour).Unix()) + `}}`),
	} {
		got, src := ResolveResetAt(hdr(), body, resetAtNow)
		if src != ResetExpired {
			t.Errorf("%s：来源 = %q, want expired", name, src)
		}
		if !got.IsZero() {
			t.Errorf("%s：过期时必须返回零值，得 %v", name, got)
		}
	}
	// 恰好等于 now 也算过期（边界：不可用于冷却）。
	got, src := ResolveResetAt(hdr(), []byte(`{"data":{"resets_at":`+itoa(resetAtNow.Unix())+`}}`), resetAtNow)
	if src != ResetExpired || !got.IsZero() {
		t.Errorf("t == now 应判过期，得 (%v, %q)", got, src)
	}
}

// TestResolveResetAtExpiredBodyFallsThroughToHeader body 文案过期不应挡住可用的响应头。
//
// 注意：这一条锁的是**当前实现的一个刻意取舍** —— 第 1 级解析成功但已过期时
// 直接返回 expired，不再尝试第 3 级。理由：上游在同一响应里给出互相矛盾的两份
// 时间信息说明我们对其语义的理解已经不可靠，此时任何一份都不该被采信。
func TestResolveResetAtExpiredBodyFallsThroughToHeader(t *testing.T) {
	body := []byte(`{"msg":"将在 2026-10-15 11:00:00 重置"}`)
	got, src := ResolveResetAt(hdr("Retry-After", "600"), body, resetAtNow)
	if src != ResetExpired || !got.IsZero() {
		t.Errorf("过期文案 + 可用头 = (%v, %q)，want (零值, expired)", got, src)
	}
}

// TestResolveResetAtIgnoresUnrelatedExpiryFields 只认 reset 语义的键：
// expires_at（套餐到期）不得被读成限流恢复时间。
func TestResolveResetAtIgnoresUnrelatedExpiryFields(t *testing.T) {
	body := []byte(`{"data":{"expires_at":` + itoa(resetAtNow.Add(72*time.Hour).Unix()) +
		`,"expire_time":"2026-11-01 00:00:00"}}`)
	got, src := ResolveResetAt(hdr(), body, resetAtNow)
	if src != ResetNone || !got.IsZero() {
		t.Errorf("expires_at 不应被采信，得 (%v, %q)", got, src)
	}
}

// TestResolveResetAtNestedEnvelope 字段在 data 一层嵌套里同样能取到。
func TestResolveResetAtNestedEnvelope(t *testing.T) {
	body := []byte(`{"error":{"code":6004,"data":{"resetTime":` + itoa(resetAtNow.Add(2*time.Hour).Unix()) + `}}}`)
	got, src := ResolveResetAt(hdr(), body, resetAtNow)
	if src != ResetBodyJSON {
		t.Fatalf("来源 = %q, want body-json", src)
	}
	if want := resetAtNow.Add(2 * time.Hour); !got.Equal(want) {
		t.Errorf("= %v, want %v", got.UTC(), want)
	}
}

// TestResolveResetAtGarbageBody 非 JSON / 空体 / HTML 拦截页 → 不 panic、返回 none。
func TestResolveResetAtGarbageBody(t *testing.T) {
	for _, body := range [][]byte{
		nil, []byte(""), []byte("<html>Access Denied</html>"),
		[]byte(`{"data":`), []byte(`[]`), []byte(`"raw string"`),
		[]byte(`{"data":{"resets_at":"not-a-time"}}`),
		[]byte(`{"data":{"resets_at":-5}}`),
		[]byte(`{"data":{"resets_at":1e9}}`), // 1e9 秒 ≈ 2001 年，已过期 → expired
	} {
		got, src := ResolveResetAt(hdr(), body, resetAtNow)
		if !got.IsZero() {
			t.Errorf("body=%q 应返回零值，得 %v", body, got)
		}
		if src != ResetNone && src != ResetExpired {
			t.Errorf("body=%q 来源 = %q, want none/expired", body, src)
		}
	}
}

// TestResolveResetAtDeterministic 同一份 body 多次解析结果恒定
// （map 遍历无序，字段扫描必须有序化，否则会出现"偶发取不到"）。
func TestResolveResetAtDeterministic(t *testing.T) {
	body := []byte(`{"data":{"resets_at":` + itoa(resetAtNow.Add(time.Hour).Unix()) +
		`},"extra":{"resetAt":` + itoa(resetAtNow.Add(9*time.Hour).Unix()) + `}}`)
	first, src := ResolveResetAt(hdr(), body, resetAtNow)
	if src == ResetNone || first.IsZero() {
		t.Fatalf("应解析成功，得 (%v, %q)", first, src)
	}
	for i := 0; i < 50; i++ {
		got, _ := ResolveResetAt(hdr(), body, resetAtNow)
		if !got.Equal(first) {
			t.Fatalf("第 %d 次结果漂移：%v vs %v", i, got, first)
		}
	}
}

// TestResetSourceStrings 来源标识是对外可见的台账字段，改动即破坏可读性 → 锁死。
func TestResetSourceStrings(t *testing.T) {
	for _, tc := range []struct {
		src  ResetSource
		want string
	}{
		{ResetNone, "none"}, {ResetBodyText, "body-text"}, {ResetBodyJSON, "body-json"},
		{ResetHeader, "header"}, {ResetExpired, "expired"},
	} {
		if string(tc.src) != tc.want {
			t.Errorf("ResetSource = %q, want %q", tc.src, tc.want)
		}
	}
}

// TestResolveResetAtJSONNumberType json.Number 形态（UseNumber 解码器）同样可解析。
func TestResolveResetAtJSONNumberType(t *testing.T) {
	dec := json.NewDecoder(strings.NewReader(`{"data":{"resets_at":` +
		itoa(resetAtNow.Add(time.Hour).Unix()) + `}}`))
	dec.UseNumber()
	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		t.Fatalf("decode: %v", err)
	}
	sub := root["data"].(map[string]any)
	if t2, ok := parseResetValue(sub["resets_at"], resetAtNow, false); !ok {
		t.Error("json.Number 形态应可解析")
	} else if want := resetAtNow.Add(time.Hour); !t2.Equal(want) {
		t.Errorf("= %v, want %v", t2.UTC(), want)
	}
}
