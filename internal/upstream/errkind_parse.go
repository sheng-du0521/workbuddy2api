// ErrKind 的字符串反向映射（v0.31.2）。
//
// 为什么需要：`consume_failures.kind` 落库的是 `ErrKind.String()` 的结果，
// 而"这条失败最终会让客户端收到什么状态码"要用 `upstreamFailureStatus(ErrKind)`
// 才能算出来。台账面板要把两者并排显示（**上游给我们的** 400 与 **我们回客户端的**
// 404/400/503 不是一回事 —— 实测有人把前者读成后者，进而误判"网关全都回 400"）。
package upstream

import "strings"

// ParseErrKind 把 String() 的输出解析回枚举（认不出来返回 ErrNone）。
//
// 实现刻意是"遍历枚举 + 比较 String()"，而不是再维护一张字符串表：
// 两张表迟早分叉，而分叉的表现是"台账里的 kind 认不出来、面板上状态码一列空着"，
// 属于最不该出现的一类静默缺陷。
//
// 边界由 errKindLast 给出；新增枚举时**必须**同步它（errkind_parse_test.go 会在
// 有人忘记时失败 —— 见该测试对"哨兵之后仍为空串"的断言）。
func ParseErrKind(s string) ErrKind {
	s = strings.TrimSpace(s)
	if s == "" {
		return ErrNone
	}
	for _, k := range allErrKinds {
		if k.String() == s {
			return k
		}
	}
	return ErrNone
}

// allErrKinds 全部枚举（顺序即声明顺序）。
//
// 它有**两个**用途，缺一不可：① ParseErrKind 的遍历源；② 与 errKindLast 互相校验
// （errkind_parse_test.go 断言 `len(allErrKinds) == int(errKindLast)+1`）——
// 新增枚举时若只改一处，测试会失败，从而避免"新码在台账里认不出来、面板状态码空着"
// 这类静默缺陷。
var allErrKinds = []ErrKind{
	ErrNone, ErrHardCredit, ErrSoftRate, ErrSessionDead, ErrNotFound, ErrServer,
	ErrContentBlocked, ErrBadParams, ErrAccountFault, ErrModelBlocked, ErrWafBlock,
	ErrPromptTooLong, ErrImageInvalid, ErrClient, ErrChannelDenied,
}

// errKindLast 当前枚举的最后一个值。
//
// 与 allErrKinds 尾部必须一致（测试守着）——
// 不能靠"探测下一个值的 String() 是否为空"来判边界：String() 有 default 分支
// 返回 "none"，探测法恒为假（实测）。
const errKindLast = ErrChannelDenied
