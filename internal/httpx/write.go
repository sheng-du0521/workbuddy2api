// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package httpx

import (
	"encoding/json"
	"net/http"
)

// WriteJSON 统一的 JSON 响应出口（v0.31.12 从 cmd/webapp 的 writeJSON 下沉）。
//
// 它做三件事，顺序不可换：
//  1. 设置 `Content-Type`（必须在 WriteHeader 之前，否则头已发出）；
//  2. 写状态码；
//  3. **脱敏后**再编码 —— `err.Error()` 大多直接来自 SQLite，原文形如
//     `SQL logic error: no such table: host_earnings (1)`，回填给客户端等于
//     把内部表结构告诉外人（见 errsafe.go 的文件头）。
//
// ⚠️ 第 3 步会**就地修改** `map[string]any` 里的值（不是拷贝）。这个行为是从
// 原实现原样继承的：调用方若在写响应后还要读同一个 map，会读到已被脱敏的值。
// 搬移时保持原样（不"顺手修好"）—— 那属于独立的一刀，否则本步就不是零行为了。
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ScrubErrBody(v))
}
