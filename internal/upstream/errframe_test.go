// 上游错误码解析与「模型级」判据的测试（v0.31.3）。
//
// 重点是**反例比正例多**：这两个判定决定"要不要给某账号的某个模型写避让"，
// 把不该算模型级的码收进来，就等于给一次可恢复的抖动配上"绕开该号"的处置
// —— 这正是 2026-10-13 三十号误杀那一类病的成因。
package upstream

import "testing"

func TestErrorCodeForms(t *testing.T) {
	cases := []struct {
		body string
		want int
	}{
		// 正例：上游出现过的三种形态
		{`{"code":6004}`, 6004},
		{`{"code":"6004"}`, 6004},
		{`{"error":{"code":6004}}`, 6004},
		{`{"error":{"code":"6004","message":"x"}}`, 6004},
		{`{ "code" :  11102 }`, 11102},
		// 反例：解析不出必须回 0（调用方据此不做任何处置）
		{``, 0},
		{`not json`, 0},
		{`{"msg":"no code here"}`, 0},
		{`{"code":null}`, 0},
		{`{"code":"abc"}`, 0},
		{`{"code":0}`, 0}, // 0 是"没有码"的哨兵，不得被当成有效码
	}
	for _, c := range cases {
		if got := ErrorCode([]byte(c.body)); got != c.want {
			t.Errorf("ErrorCode(%q) = %d，期望 %d", c.body, got, c.want)
		}
	}
}

// TestModelLevelErrorCodeOnlyModelCodes 「模型级」只认 6004 / 11102。
//
// 反例表是本用例的主体：11140（request illegal，账号级/请求级）、12153（会话失效）、
// 11128（渠道拒绝）、11133/11135（请求参数/图片）、429/500 族**一律不算模型级** ——
// 收进来会让它们绕开相关账号对该模型的调度，而正确处置（账号级冷却/摘号/降级）
// 完全在别的链路里。
func TestModelLevelErrorCodeOnlyModelCodes(t *testing.T) {
	for _, code := range []int{6004, 11102} {
		if !ModelLevelErrorCode(code) {
			t.Errorf("%d 应判为模型级", code)
		}
	}
	for _, code := range []int{
		0,    // 解析不出
		400,  // HTTP 状态码本身
		401, 403, 404, 429, 500, 502, 503,
		11115, // prompt too long（请求的问题）
		11128, // 渠道拒绝（指纹/渠道级）
		11133, // 参数无效
		11135, // 图片数据无效
		11140, // request illegal（账号级/请求级，达阈值才处置）
		12153, // 会话失效
		14017, // 上游归档类
	} {
		if ModelLevelErrorCode(code) {
			t.Errorf("%d 不该判为模型级（会绕开该号对该模型的调度，而正确处置在别的链路）", code)
		}
	}
}
