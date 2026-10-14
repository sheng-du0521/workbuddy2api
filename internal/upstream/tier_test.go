// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package upstream

import "testing"

// TestMayRetireAccount 「够不够作为摘除账号的依据」的判定。
//
// 这条判定决定**能不能停用/删除一个号**，所以负例比正例重要 ——
// 把不该收的类型收进来，就等于给可恢复的抖动配上不可逆处置
// （2026-10-13 批量误杀 30 个号的成因类别）。
func TestMayRetireAccount(t *testing.T) {
	// 正例：指向账号自身状态、且有终态语义。
	yes := []ErrKind{ErrSessionDead, ErrAccountFault}
	// 负例：全部**不指向账号终态** —— 逐个说明理由（这些理由必须与处置分支一致）。
	no := map[ErrKind]string{
		ErrNone:           "成功，显然",
		ErrHardCredit:     "余额耗尽（可签到恢复）——账号没坏，只做冷却",
		ErrSoftRate:       "429 限流：usage-driven，等窗口过去即可",
		ErrNotFound:       "404 路径/前端偶发，与账号无关",
		ErrServer:         "5xx 上游故障，与账号无关",
		ErrContentBlocked: "内容策略，请求的问题",
		ErrBadParams:      "请求体不合法，请求的问题",
		ErrModelBlocked:   "该后端无此模型，切模型即可",
		ErrWafBlock:       "WAF 拦截指向出口/IP 而非账号",
		ErrPromptTooLong:  "上下文超限：同一 body 换任何号都超限",
		ErrImageInvalid:   "图片数据无效，请求的问题",
		ErrClient:         "未分类 4xx，不足以定罪",
	}
	for _, k := range yes {
		if !MayRetireAccount(k) {
			t.Errorf("%s 应可作为摘除依据", k)
		}
	}
	for k, why := range no {
		if MayRetireAccount(k) {
			t.Errorf("%s 不该作为摘除依据（%s）", k, why)
		}
	}
	if RetireEligibleHint() == "" {
		t.Error("统一说明不该为空")
	}
}
