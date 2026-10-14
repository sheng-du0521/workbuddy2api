// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// modelratemax_test.go 模型级冷却上限（model_rate_max）的契约。
//
// ## 用户口径（v0.26.5）
//
// 「对应模型报错、设定重置时间的，都按照时间 —— 对应模型不消耗进入软冷却，
//
//	其他模型可正常调用。」
//
// 三件事各自对应一条断言：
//
//	按照时间       → Until 跟随上游重置墙钟，不再被**账号级** soft_rate_max（2h）截断
//	不消耗         → 冷却只动冷却状态，不碰余额/积分（本文件只覆盖"不写账号级 until"，
//	                计费侧的"失败不计费"由 cmd/webapp 的预扣释放用例覆盖）
//	其他模型正常   → 触发模型的兄弟模型必须仍可选（healthyForModel 的既有语义）
package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestModelRateMaxDefaultIs24h 未注入时模型级上限默认 24h：
// 5h 的重置时刻应被**完整尊重**（旧行为会在 2h 处截断）。
func TestModelRateMaxDefaultIs24h(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	reset := time.Now().Add(5 * time.Hour)
	p.CooldownSoftForModel("u1", time.Minute, reset, "glm-5.3", "6004 model rate limit")

	until := mustModelUntil(t, p, "u1", "glm-5.3")
	if d := time.Until(until); d < 5*time.Hour-time.Second || d > 5*time.Hour+time.Second {
		t.Errorf("Until 剩余 %v，期望 ~5h（默认 model_rate_max=24h 不该截断）", d)
	}
	// 账号级 until 必须仍为零值：6004 不得污染账号级冷却。
	if st, _ := p.Status("u1"); !st.Until.IsZero() {
		t.Errorf("Status.Until=%v 应为零值（6004 只写模型级）", st.Until)
	}
}

// TestModelRateMaxCapsAbsurdResetAt 荒谬/被误读出来的遥远时刻仍被封顶：
// 不封顶等于永久放弃该 (账号, 模型)，这是必须保留的护栏。
func TestModelRateMaxCapsAbsurdResetAt(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	reset := time.Now().Add(400 * 24 * time.Hour) // 一年后（典型的时区/格式误读形态）
	p.CooldownSoftForModel("u1", time.Minute, reset, "glm-5.3", "6004 model rate limit")

	until := mustModelUntil(t, p, "u1", "glm-5.3")
	if d := time.Until(until); d > defaultModelRateMax+time.Second {
		t.Errorf("Until 剩余 %v，应被默认上限 %v 封顶", d, defaultModelRateMax)
	}
}

// TestModelRateMaxDoesNotAffectAccountLevel 账号级路径的 2h 封顶不受本次改动影响。
//
// 这是最重要的一条"防误伤"断言：模型级放宽上限是因为只挡一个模型没代价，
// 而账号级一旦放宽，上游明说的按天窗口会真的把一个健康号白扣一天。
func TestModelRateMaxDoesNotAffectAccountLevel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	reset := time.Now().Add(5 * time.Hour)
	p.CooldownSoftRate("u1", time.Minute, reset, "429 rate limit")

	st, _ := p.Status("u1")
	if st.Until.IsZero() {
		t.Fatal("账号级软冷却应写 until")
	}
	if d := time.Until(st.Until); d > defaultSoftRateMax+time.Second {
		t.Errorf("账号级 until 剩余 %v，应仍被 soft_rate_max=%v 封顶（不许跟着放宽）",
			d, defaultSoftRateMax)
	}
}

// TestModelRateMaxLeavesSiblingModelsUsable 「其他模型可正常调用」的显式断言。
func TestModelRateMaxLeavesSiblingModelsUsable(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	reset := time.Now().Add(5 * time.Hour)
	p.CooldownSoftForModel("u1", time.Minute, reset, "glm-5.3", "6004 model rate limit")

	now := time.Now()
	_ = now
	// PickExcludingForRealm(nil, model, "") 是选号主入口对模型维度的暴露点，
	// reqModel 非空时健康口径即 healthyForModel。
	if a := p.PickExcludingForRealm(nil, "glm-5.3", ""); a != nil {
		t.Errorf("触发模型 glm-5.3 在冷却期内不应被选中，却选中了 %s", a.UID)
	}
	if a := p.PickExcludingForRealm(nil, "deepseek-v4-pro", ""); a == nil {
		t.Error("兄弟模型 deepseek-v4-pro 必须仍可选中（模型级隔离）")
	} else if a.UID != "u1" {
		t.Errorf("应选中唯一账号 u1，得 %s", a.UID)
	}
}
