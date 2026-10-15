// 上游错误码的解析与「模型级」判定 —— 两个产品（cmd/webapp / internal/server）
// 共用的单一事实来源（v0.31.3）。
//
// ## 为什么要放进 upstream 包
//
// v0.31.1 给主产品补上了「HTTP 200 + 流内 6004/11102 错误帧 → (账号,模型) 冷却」，
// 但**错误码集合与解析口径当时写在使用方**（cmd/webapp/proxy.go 的
// parseUpstreamCode / detectStreamModelErrorFrame）。旧网关要同步这条链路时，
// 只能再抄一份 —— 而本项目最贵的一条教训正是「同一个错误码在不同链路上有两套纪律」
// （2026-10-13 三十号误杀的成因）。所以先把它收敛到这里，再让两个产品都调用。
//
// ## 「模型级」的判据是唯一的
//
// 只有这两个码属于"这次请求把某个模型打到了限额 / 该后端没有这个模型"，
// 需要按 (账号, 模型) 维度避让，而账号对其它模型照常服务：
//
//	6004  模型级用量超限（上游会给重置墙钟；冷却到该时刻）
//	11102 该后端无此模型（(账号,模型) 负缓存避让）
//
// 其它码（含 11140 / 12153 / 11128）都是**账号级或请求级**，有自己的链路，
// 不在流式热路径上重复分类 —— 误把某个码加进这里等于给它配上"绕开该号的模型级豁免"，
// 而这时正确的处置可能是完全不同的东西。
package upstream

import (
	"encoding/json"
	"strconv"
	"strings"
)

// ErrorCode 从上游响应体（或 SSE error 帧 payload）里尽力解析业务错误码；
// 解析不出返回 0。
//
// 兼容三种形态：顶层 `{"code":6004}`、信封 `{"error":{"code":6004}}`，
// 以及数字以字符串给出的 `{"code":"6004"}`（上游两种都出现过）。
func ErrorCode(body []byte) int {
	if len(body) == 0 {
		return 0
	}
	var obj struct {
		Code  any `json:"code"`
		Error struct {
			Code any `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return 0
	}
	if n, ok := codeInt(obj.Code); ok {
		return n
	}
	if n, ok := codeInt(obj.Error.Code); ok {
		return n
	}
	return 0
}

// codeInt 把 JSON 解出来的 code 值转成 int（float64 / string 两种来源）。
func codeInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case string:
		if i, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
			return i, true
		}
	}
	return 0, false
}

// ModelLevelErrorCode 报告该业务码是否为「模型级」—— 需要按 (账号, 模型) 维度避让，
// 账号对其它模型仍可用。
//
// 判据清单只有下面两个码，**新增成员必须先问**：它真的是"这个模型的问题是模型级的"
// 吗？（加进来 = 该码会绕开相关账号对该模型的调度，而账号本身仍被判定为健康。）
func ModelLevelErrorCode(code int) bool {
	switch code {
	case 6004: // 模型级用量超限（上游带重置墙钟）
		return true
	case 11102: // 该后端无此模型（负缓存避让）
		return true
	}
	return false
}
