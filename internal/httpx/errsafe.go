// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 共享 HTTP 层的两件事：**响应写出**与**内部错误文本脱敏**。
//
// 为什么单独成包（v0.31.12，P2-7 第四刀起手）：
//  1. 这两件事是**每个** HTTP handler 都要用的横切关注点（`WriteJSON` 全项目 552 处调用），
//     而 `internal/api/*` 若各自复制一份，脱敏策略就会出现多个版本 ——
//     本项目最贵的一类缺陷就是"同一件事两处实现"；
//  2. 它们**零业务依赖**（只 import log/strings/net/http/encoding/json），
//     天然是可共享层 —— 搬进来不需要任何接口设计。
//
// ⚠️ 纪律：本包**只放"对所有 handler 都一样"的东西**。任何带业务判断的写出
// （比如"余额不足要回 402"）都必须留在业务包里；否则这个包会变成第二个上帝对象。
package httpx

import (
	"log"
	"strings"
)

// 本文件处理审查报告 P3-1：**内部错误文本直接回填给客户端**。
//
// 问题：全项目有 150+ 处 `writeJSON(w, code, map[string]any{"error": err.Error()})`，
// 而 err 大多直接来自 SQLite —— modernc-sqlite 的错误长这样：
//
//	SQL logic error: no such table: host_earnings (1)
//	constraint failed: UNIQUE constraint failed: users.username (2067)
//
// 于是多租户用户在报错里直接看到**表名、列名、约束名**。对管理员无所谓，
// 对普通用户就是信息泄露 + 观感问题（"这破站连错误信息都这么脏"）。
//
// 为什么不在 154 个调用点逐个改：那是纯机械的大改动，收益与风险不成比例
// （每个点都要判断"这个错误该不该给用户看"，很容易改错）。
// 改在**唯一的出口** writeJSON 上：一处收敛、可单测、和调用点解耦。
//
// 设计要点：
//   - **只脱敏，不改状态码**：状态码由各 handler 决定（那是它们的业务判断），
//     这里只管文本。混在一起改会让这个改动从"安全加固"变成"行为重构"。
//   - **只认内部错误的特征串**，认不出的一律原样放行 —— 业务错误是手写中文
//     （"余额不足"、"用户名已被占用"），不会命中特征串，因此不受影响。
//   - **原文写日志**：脱敏只是不给客户端看，运维仍然要能看到真实原因。
//     日志是唯一保留原文的地方（也便于未来做错误率统计）。

// ErrScrubMarkers 判决用特征串：命中任意一个即认为是「内部实现细节」。
//
// 取串原则：**只取内部实现一定会带、而业务文案一定不会带的**。
// 例如 "database is locked"、"no such table" 是 SQLite 的固定措辞；
// "goroutine" + ".go:" 是 panic 栈的形态。刻意**不**收 "error"、"failed"
// 这类通用词 —— 那会把业务文案一起误杀。
var ErrScrubMarkers = []string{
	// SQLite / database-sql 层
	"SQL logic error",
	"SQLITE_",
	"constraint failed",
	"UNIQUE constraint",
	"no such table",
	"no such column",
	"no such index",
	"database is locked",
	"database table is locked",
	"sql: ",
	"modernc.org/sqlite",
	// Go 运行时 / 栈
	"goroutine ",
	"runtime error",
	"nil pointer",
	"invalid memory address",
	"index out of range",
	".go:",
}

// ScrubTextSafe 稳定的脱敏文案（不再回显任何内部细节）。
const ScrubTextSafe = "内部错误，请稍后重试（详情见服务端日志）"

// LooksInternalErr 判定该文本是否泄漏内部实现细节。
func LooksInternalErr(s string) bool {
	for _, m := range ErrScrubMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// ScrubErrField 判断该键值是否应当脱敏；返回替换后的文本与「是否替换」。
//
// ⚠️ **返回值刻意是 (string, bool) 而不是 any**（v0.24.1 修，血的教训）：
// 最初写成 `func ScrubErrField(key string, val any) any`，调用处用
// `if nv := ScrubErrField(k, val); nv != val` 判断"有没有变" —— 而 `any` 用 `!=`
// 比较**不可比较类型**（map / slice / func）会直接 **panic**。
// 公告列表的响应体里恰好带一个 `map[string]int` 计数，于是
// `TestAnnouncementCRUD` 当场炸（`comparing uncomparable type map[string]int`）。
//
// 现在只可能"替换成一段字符串"，所以：
//
//	· 非字符串值（map/slice/数字/结构体…）一律视为"无需处理"，连类型都不用比；
//	· 调用方靠 bool 判断，永不比较 any。
func ScrubErrField(key string, val any) (string, bool) {
	if !IsErrFieldName(key) {
		return "", false
	}
	s, ok := val.(string)
	if !ok || s == "" {
		return "", false
	}
	if !LooksInternalErr(s) {
		return "", false // 业务文案原样放行
	}
	// 原文进日志（唯一保留处）；客户端只看到稳定文案。
	log.Printf("已脱敏内部错误文本（原样回填会泄漏表名/栈）：%s", s)
	return ScrubTextSafe, true
}

// IsErrFieldName 错误字段名判定。
//
// 覆盖 "error" / "message" / "detail" / "reason" / "err" 等本项目实际用到的键，
// 以及它们的 `error_xxx` 变体（如 error_detail）。
func IsErrFieldName(key string) bool {
	k := strings.ToLower(key)
	if k == "err" || k == "error" || k == "detail" || k == "reason" || k == "message" {
		return true
	}
	return strings.HasPrefix(k, "error_") || strings.HasSuffix(k, "_error")
}

// ScrubErrBody 把响应体里的内部错误文本替换掉（原地改 map，返回同一个对象）。
//
// 只看 `map[string]any` 及其**内一层**（map 与 []any 里的逐项 map）——
// 不做任意深度反射遍历：那会让行为难以预测，而实际数据形态就这么浅。
func ScrubErrBody(v any) any {
	switch body := v.(type) {
	case map[string]any:
		for k, val := range body {
			if nv, ok := ScrubErrField(k, val); ok {
				body[k] = nv
				continue
			}
			// 一层嵌套：如 {"error": {"message": ...}}
			switch inner := val.(type) {
			case map[string]any:
				for ik, iv := range inner {
					if nv, ok := ScrubErrField(ik, iv); ok {
						inner[ik] = nv
					}
				}
			case []any:
				// 批量操作的逐项结果（如 [{"uid":..,"error":..}]）。
				for _, item := range inner {
					if m, ok := item.(map[string]any); ok {
						for ik, iv := range m {
							if nv, ok := ScrubErrField(ik, iv); ok {
								m[ik] = nv
							}
						}
					}
				}
			}
		}
	}
	return v
}

// StrField 从 map 里安全取字符串字段（缺失或类型不符时返回空串）。
//
// 用途：排序比较器里的类型断言（审查报告 P3-14）。那些地方"当前键恒存在、
// 值恒为 string"，所以断言不会炸 —— 但只要上游构造改一次（如某天字段变成 int
// 或干脆不填），`m["x"].(string)` 就会 panic：而排序里的 panic 会带走整个请求
// （若发生在后台 goroutine 里则直接带走进程）。
//
// 刻意**不**返回 (string, bool)：排序只需要一个稳定、可比较的值，
// 缺失时用空串排在最前即可 —— 让调用方处理"取不到"只会把简单问题复杂化。
func StrField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}
