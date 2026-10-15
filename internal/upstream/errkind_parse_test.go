// ErrKind 字符串反向映射的测试（v0.31.2）。
package upstream

import "testing"

// TestParseErrKindRoundTrip 每个枚举都要能被 String() → ParseErrKind 往返回来。
//
// 这是"台账里的 kind 字符串还能算出状态码"的前提：面板要把
// 「上游给我们什么」与「我们回客户端什么」并排显示，靠的就是这个往返。
func TestParseErrKindRoundTrip(t *testing.T) {
	for k := ErrNone; k <= errKindLast; k++ {
		s := k.String()
		if s == "" {
			t.Fatalf("枚举 %d 的 String() 为空 —— 反向映射无从谈起", int(k))
		}
		if got := ParseErrKind(s); got != k {
			t.Errorf("ParseErrKind(%q) = %d，期望 %d", s, int(got), int(k))
		}
	}
}

// TestErrKindTableIsComplete 枚举清单与边界常量必须互相一致。
//
// 判据不能是"探测下一个值的 String() 是否为空"—— String() 有 default 分支返回
// "none"，探测法恒为假（第一版就这么写的，被测试当场抓住）。
// 改为两条可判定的断言：
//
//	· len(allErrKinds) == int(errKindLast)+1  —— 新增枚举却忘记同步任一处即失败；
//	· 每个枚举的 String() 非空且互不重复      —— 重复会让 ParseErrKind 只能认回第一个。
func TestErrKindTableIsComplete(t *testing.T) {
	if len(allErrKinds) != int(errKindLast)+1 {
		t.Fatalf("allErrKinds 有 %d 项，errKindLast=%d —— 两者必须一致（新增枚举时同步）",
			len(allErrKinds), int(errKindLast))
	}
	seen := map[string]ErrKind{}
	for i, k := range allErrKinds {
		if int(k) != i {
			// ErrNone=0 起连续递增：声明顺序应与数值一致，否则 allErrKinds 顺序即错
			t.Fatalf("allErrKinds[%d]=%d —— 顺序应与枚举数值一致", i, int(k))
		}
		s := k.String()
		if s == "" {
			t.Fatalf("枚举 %d 的 String() 为空", int(k))
		}
		if prev, dup := seen[s]; dup && prev != ErrNone {
			t.Fatalf("String() 重复：%q 同时属于 %d 与 %d —— ParseErrKind 只能认回第一个",
				s, int(prev), int(k))
		}
		seen[s] = k
	}
}

// TestParseErrKindUnknown 认不出来的字符串回 ErrNone（调用方据此显示"—"而不是乱猜一个码）。
func TestParseErrKindUnknown(t *testing.T) {
	for _, s := range []string{"", "  ", "not_a_kind", "SOFT_RATE", "6004"} {
		if got := ParseErrKind(s); got != ErrNone {
			t.Errorf("ParseErrKind(%q) = %d，期望 ErrNone", s, int(got))
		}
	}
	// 容错：首尾空白不影响解析（台账里的值来自 Go 字符串，正常不会有空白，
	// 但接口回传/人工拼装可能出现）。
	if got := ParseErrKind(" soft_rate "); got != ErrSoftRate {
		t.Errorf("带空白的 kind 应能解析，got %d", int(got))
	}
}
