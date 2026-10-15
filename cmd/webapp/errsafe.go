// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 内部错误文本脱敏：**实现已下沉 `internal/httpx`**（v0.31.12，P2-7 第四刀起手）。
//
// 本文件只剩**过渡别名**：这几个符号在 cmd/webapp 里仍有调用点（`writeJSON` 在
// main.go、`strField` 在若干 handler、`scrubErrField`/`looksInternalErr` 在测试），
// 别名让它们一行不改。
//
// ⚠️ 别名纪律：**函数与 const 别名安全**（函数值没有"改本地那份"的注入语义；
// const 是编译期常量）。**变量别名是值拷贝** —— 只有在"本包内零写入、纯只读"时
// 才可接受，且必须在注释里写明（第一刀的坑：`var c = pkg.X` 曾被当成注入点用过）。
//
// 备注（一条自查教训）：我先只数了"调用点"，漏了注释与常量引用 ——
// `scrubErrTextSafe` 其实有 **6 处引用**（含 failures.go 把它写进失败台账的 Detail），
// 所以它**必须**留别名。**数引用要用 `\bsym\b`，不要用 `sym(`。**
package main

import "workbuddy2api/internal/httpx"

var (
	scrubErrBody     = httpx.ScrubErrBody
	scrubErrField    = httpx.ScrubErrField
	looksInternalErr = httpx.LooksInternalErr
	strField         = httpx.StrField
)

// 常量别名（const 别名安全：编译期常量，无本地注入语义）。
const (
	// scrubErrTextSafe 脱敏后的统一文案
	// （failures.go 把它写进失败台账的 Detail；测试也多处对照它）。
	scrubErrTextSafe = httpx.ScrubTextSafe
)

// errScrubMarkers 诊断用的通用词清单 —— 只被 errsafe_test.go 的**注释**引用。
// 它是切片（变量别名 = 值拷贝），保留了别名仅供对照阅读；本包**没有任何写入点**，
// 所以"改本地那份不影响包内"这件事在这里无害。
var errScrubMarkers = httpx.ErrScrubMarkers
