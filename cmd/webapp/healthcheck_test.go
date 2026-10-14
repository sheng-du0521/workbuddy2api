// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// ===========================================================================
// v0.23.0：号池账号健康检测
//
// 测试分层原则：**探测本身依赖网络**，故把全部判定逻辑抽成纯函数并在此穷举覆盖；
// 网络那一层只留「本地前置不出站」与「处置动作」两类可离线验证的用例。
// ===========================================================================

// TestHealthPrecheck 第一级：本地前置判定（不出站）。
//
// 这一级的作用是「本地能定论的别浪费一次上游请求」，且**跳过**必须只留给
// 运维的明确意图（手动停用）—— 被系统误判禁用的号仍要探测，这是本模块的核心
// 设计取舍之一，故专门断言 Disabled 不会被跳过。
func TestHealthPrecheck(t *testing.T) {
	acc := &Account{UID: "u1", AccessToken: "tok"}

	t.Run("手动停用→跳过", func(t *testing.T) {
		st := pool.Status{UID: "u1", ManualDisabled: true, ManualReason: "维护中"}
		v := hcPrecheck(acc, st, true)
		if !v.Done || v.State != hcSkipped || v.ReasonCode != hcCodeManualDisabled {
			t.Fatalf("手动停用应跳过，得 %+v", v)
		}
		if !strings.Contains(v.Reason, "维护中") {
			t.Fatalf("原因应带上运维备注，得 %q", v.Reason)
		}
	})

	t.Run("凭证缺失→异常", func(t *testing.T) {
		noTok := &Account{UID: "u1"}
		v := hcPrecheck(noTok, pool.Status{UID: "u1"}, true)
		if !v.Done || v.State != hcAbnormal || v.ReasonCode != hcCodeAuthMissing {
			t.Fatalf("凭证缺失应判异常，得 %+v", v)
		}
	})

	t.Run("凭证为空串（仅空格）也判异常", func(t *testing.T) {
		blank := &Account{UID: "u1", AccessToken: "   "}
		v := hcPrecheck(blank, pool.Status{UID: "u1"}, true)
		if !v.Done || v.State != hcAbnormal {
			t.Fatalf("空白凭证应判异常，得 %+v", v)
		}
	})

	t.Run("账号不在池内", func(t *testing.T) {
		v := hcPrecheck(nil, pool.Status{}, false)
		if !v.Done || v.State != hcAbnormal {
			t.Fatalf("不在池内应判异常，得 %+v", v)
		}
	})

	t.Run("系统禁用但仍要探测（不跳过）", func(t *testing.T) {
		st := pool.Status{UID: "u1", Disabled: true, DisabledReason: "12153 session dead"}
		v := hcPrecheck(acc, st, true)
		if v.Done {
			t.Fatalf("系统禁用的号必须继续探测（可能被误判），不该定论：%+v", v)
		}
	})

	t.Run("正常账号继续探测", func(t *testing.T) {
		v := hcPrecheck(acc, pool.Status{UID: "u1"}, true)
		if v.Done {
			t.Fatalf("正常账号应交由探测定论，得 %+v", v)
		}
	})
}

// TestHealthVerdictMatrix 第二级：探测错误 → 判定的**完整映射矩阵**。
//
// 这是本模块最核心的一张表：它同时定义「归因给谁」与「能不能自动处置」。
// 断言点有二：state 正确 + Definite（确定性）正确 —— 后者是自动处置的唯一闸门，
// 一旦把某个「可能是出口/上游问题」的 Kind 误标为确定，探测抖动就会改生产账号状态。
func TestHealthVerdictMatrix(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantState string
		wantCode  string
		wantDef   bool
	}{
		// —— 确定性异常：证据指向账号本身 ——
		{"登录失效 12153", &upstream.Error{Kind: upstream.ErrSessionDead, Status: 401},
			hcAbnormal, hcCodeSessionDead, true},
		{"试用未开通 14017", &upstream.Error{Kind: upstream.ErrAccountFault, Status: 400, Msg: "code 14017 trial not activated"},
			hcAbnormal, hcCodeAccountFault, true},
		{"余额耗尽", &upstream.Error{Kind: upstream.ErrHardCredit, Status: 402},
			hcAbnormal, hcCodeNoCredits, true},

		// —— 不确定：可能是出口 / 上游 / 临时状态，一律不处置 ——
		// 11140 **从"确定性封号"移到这里**（v0.24.3，2026-10-13 封号事故）：
		// 它的语义是 request illegal（这次请求不合法），由出口 IP / 频率 / 头细节触发，
		// **不是账号封禁**。旧断言期望 abnormal/banned/true，那正是把 bug 规格化了 ——
		// 一次风控窗口因此把 30 个健康账号批量停用（凭证与余额完好、重探即通过）。
		{"请求非法 11140（不给定性，但会软冷却 + 累计阈值）",
			&upstream.Error{Kind: upstream.ErrAccountFault, Status: 400, Msg: "code 11140 request illegal"},
			hcUncertain, hcCodeRequestIllegal, false},
		{"WAF 403", &upstream.Error{Kind: upstream.ErrWafBlock, Status: 403},
			hcUncertain, hcCodeWafBlock, false},
		{"429 限流", &upstream.Error{Kind: upstream.ErrSoftRate, Status: 429},
			hcUncertain, hcCodeRateLimited, false},
		{"404 偶发", &upstream.Error{Kind: upstream.ErrNotFound, Status: 404},
			hcUncertain, hcCodeUpstream404, false},
		{"5xx 上游故障", &upstream.Error{Kind: upstream.ErrServer, Status: 503},
			hcUncertain, hcCodeUpstream5xx, false},
		{"其他 4xx（未分类）", &upstream.Error{Kind: upstream.ErrClient, Status: 400},
			hcUncertain, hcCodeUnclassified, false},
		{"内容拦截（与账号无关）", &upstream.Error{Kind: upstream.ErrContentBlocked, Status: 400},
			hcUncertain, hcCodeUnclassified, false},
		{"传输错误（非信封）", errors.New("dial tcp: i/o timeout"),
			hcUncertain, hcCodeTransport, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := hcVerdictFromErr(c.err)
			if v.State != c.wantState || v.ReasonCode != c.wantCode || v.Definite != c.wantDef {
				t.Fatalf("得 state=%s code=%s definite=%v，期望 %s/%s/%v（reason=%q）",
					v.State, v.ReasonCode, v.Definite, c.wantState, c.wantCode, c.wantDef, v.Reason)
			}
			if v.Reason == "" {
				t.Fatal("每种判定都必须带可读原因")
			}
			// 不确定性判定一律不允许自动处置 —— 这是「不误杀」的底线。
			if v.State == hcUncertain && v.Definite {
				t.Fatal("uncertain 绝不可是 definite")
			}
		})
	}

	// nil 错误 = 探测成功，不该被本函数当作结论（由第三级判定接手）。
	if v := hcVerdictFromErr(nil); v.State != "" {
		t.Fatalf("nil 错误不该产生判定，得 %+v", v)
	}
}

// TestHealthVerdictFromProbeOK 第三级：探测成功后的本地补充判定与**优先级**。
//
// 关键断言是「余额为 0」优先于「降权痕迹」：一个既没余额又有熔断记录的号，
// 结论应是「受限（无额度）」而非含糊的「降权中」—— 前者才是运维首先要处理的。
func TestHealthVerdictFromProbeOK(t *testing.T) {
	clean := pool.Status{UID: "u1"}

	t.Run("干净账号→正常", func(t *testing.T) {
		v := hcVerdictFromProbeOK(clean, 5000)
		if v.State != hcHealthy || v.ReasonCode != hcCodeOK {
			t.Fatalf("应判正常，得 %+v", v)
		}
	})

	t.Run("余额为 0→异常（受限）", func(t *testing.T) {
		v := hcVerdictFromProbeOK(clean, 0)
		if v.State != hcAbnormal || v.ReasonCode != hcCodeZeroRemain || !v.Definite {
			t.Fatalf("余额 0 应判确定性异常，得 %+v", v)
		}
	})

	t.Run("负余额也判受限", func(t *testing.T) {
		if v := hcVerdictFromProbeOK(clean, -1); v.State != hcAbnormal {
			t.Fatalf("负余额应判异常，得 %+v", v)
		}
	})

	t.Run("优先级：余额 0 胜过降权痕迹", func(t *testing.T) {
		st := pool.Status{UID: "u1", Cooling: true, CoolRemaining: 100, Disabled: true, DisabledReason: "x"}
		v := hcVerdictFromProbeOK(st, 0)
		if v.ReasonCode != hcCodeZeroRemain {
			t.Fatalf("余额 0 应优先于降权痕迹，得 %+v", v)
		}
	})

	// —— 降权中：凭证是好的，但当前不能被用来服务请求 ——
	degraded := []struct {
		name string
		st   pool.Status
	}{
		{"曾被系统禁用", pool.Status{Disabled: true, DisabledReason: "12153 session dead"}},
		{"冷却中", pool.Status{Cooling: true, CoolRemaining: 42}},
		{"熔断中", pool.Status{BreakerUntil: time.Now().Add(5 * time.Minute)}},
		{"连败降权中", pool.Status{DegradeUntil: time.Now().Add(3 * time.Minute)}},
		{"模型级限流", pool.Status{RateLimitedModels: []pool.RateLimitedModel{{Model: "x"}}}},
	}
	for _, c := range degraded {
		t.Run("降权/"+c.name, func(t *testing.T) {
			v := hcVerdictFromProbeOK(c.st, 5000)
			if v.State != hcDegraded {
				t.Fatalf("应判降权中，得 %+v", v)
			}
			if !strings.Contains(v.Reason, "不参与选号") {
				t.Fatalf("原因应说明「当前不参与选号」，得 %q", v.Reason)
			}
		})
	}

	// 已过期的降权时刻不该再算降权（时间敏感字段必须与 now 比较，不能被零值/旧值误判）。
	t.Run("降权已过期→正常", func(t *testing.T) {
		st := pool.Status{
			BreakerUntil: time.Now().Add(-time.Minute),
			DegradeUntil: time.Now().Add(-time.Minute),
			Cooling:      false,
		}
		if v := hcVerdictFromProbeOK(st, 5000); v.State != hcHealthy {
			t.Fatalf("已过期的降权不该影响结论，得 %+v", v)
		}
	})

	// 被禁用但探测通过时，原因里必须提示「可考虑复活」——否则运维看不出这个号其实能救。
	t.Run("禁用但探测通过→提示可复活", func(t *testing.T) {
		st := pool.Status{Disabled: true, DisabledReason: "历史误判"}
		v := hcVerdictFromProbeOK(st, 100)
		if !strings.Contains(v.Reason, "复活") {
			t.Fatalf("应提示可复活，得 %q", v.Reason)
		}
	})
}

// TestHealthStateRank 展示排序：严重的必须排在前面。
func TestHealthStateRank(t *testing.T) {
	order := []string{hcAbnormal, hcUncertain, hcDegraded, hcSkipped, hcHealthy}
	for i := 0; i < len(order)-1; i++ {
		if hcStateRank(order[i]) >= hcStateRank(order[i+1]) {
			t.Fatalf("%s 应排在 %s 之前（rank %d vs %d）",
				order[i], order[i+1], hcStateRank(order[i]), hcStateRank(order[i+1]))
		}
	}
	// 每个状态都必须有中文名（前端直接用 state_label 渲染）。
	for _, s := range order {
		if l := hcStateLabel(s); l == "" || l == s {
			t.Fatalf("状态 %q 缺中文名", s)
		}
	}
}

// TestCheckAccountHealthLocalOnly 端到端：本地前置能定论的路径**不发上游请求**。
//
// 「不发请求」的验证方式：这两个账号的凭证/状态使流程在探测前就返回，
// 故 CreditsKnown 必为 false 且 LatencyMs 极短（没有网络等待）。
func TestCheckAccountHealthLocalOnly(t *testing.T) {
	store, px, a := newTestApp(t,
		&Account{UID: "man", Nickname: "手动停用号", AccessToken: "tok"},
		&Account{UID: "notok", Nickname: "无凭证号"},
	)
	defer store.Close()
	defer px.Close()

	// 把 man 设为手动停用。
	if _, changed := px.pool.SetManualDisabled("man", true, "运维摘除"); !changed {
		t.Fatal("SetManualDisabled 应生效")
	}

	t.Run("手动停用→skipped", func(t *testing.T) {
		r := a.checkAccountHealth("man", true)
		if r.State != hcSkipped || r.ReasonCode != hcCodeManualDisabled {
			t.Fatalf("得 %+v", r)
		}
		if r.StateLabel != "已跳过" {
			t.Fatalf("state_label 应为「已跳过」，得 %q", r.StateLabel)
		}
		if r.CreditsKnown {
			t.Fatal("跳过的账号不该有余额读数")
		}
		if len(r.Actions) != 0 {
			t.Fatalf("跳过的账号不该有任何处置动作，得 %v", r.Actions)
		}
	})

	t.Run("无凭证→异常且不出站", func(t *testing.T) {
		r := a.checkAccountHealth("notok", true)
		if r.State != hcAbnormal || r.ReasonCode != hcCodeAuthMissing {
			t.Fatalf("得 %+v", r)
		}
		if r.CreditsKnown {
			t.Fatal("无凭证的账号不该拿到余额")
		}
		// 没有网络等待 → 延迟应在毫秒级（给 1s 宽容，避免 CI 抖动误报）。
		if r.LatencyMs > 1000 {
			t.Fatalf("本地判定的延迟应很短，得 %dms", r.LatencyMs)
		}
	})

	t.Run("不存在的账号→异常", func(t *testing.T) {
		r := a.checkAccountHealth("ghost", true)
		if r.State != hcAbnormal {
			t.Fatalf("得 %+v", r)
		}
	})
}

// TestHealthDispositionSessionDead 自动处置：登录失效走**累计阈值**，不一次杀号。
//
// 与既有链路同纪律（pool.NoteSessionDead）：一次 12153 可能是网络抖动或 refresh
// 竞态，一次就禁用会误杀健康账号。故第 1、2 次只记不裁，第 3 次才禁用。
func TestHealthDispositionSessionDead(t *testing.T) {
	store, px, a := newTestApp(t,
		&Account{UID: "sd", Nickname: "会话失效号", AccessToken: "tok"})
	defer store.Close()
	defer px.Close()

	cause := &upstream.Error{Kind: upstream.ErrSessionDead, Status: 401, Msg: "12153 offline session"}

	// 前 sessionDeadThreshold-1 次：只记，不禁用。
	for i := 1; i < sessionDeadThresholdForTest; i++ {
		acts := a.applyHealthDisposition("sd", hcCodeSessionDead, cause, true)
		st, _ := px.pool.Status("sd")
		if st.Disabled {
			t.Fatalf("第 %d 次不该禁用（阈值 %d）", i, sessionDeadThresholdForTest)
		}
		if len(acts) == 0 || acts[0] != hcActNoteDead {
			t.Fatalf("第 %d 次应只记录，得 %v", i, acts)
		}
	}
	// 第 threshold 次：禁用。
	acts := a.applyHealthDisposition("sd", hcCodeSessionDead, cause, true)
	st, _ := px.pool.Status("sd")
	if !st.Disabled {
		t.Fatalf("第 %d 次应禁用，动作=%v", sessionDeadThresholdForTest, acts)
	}
	foundDisabled := false
	for _, x := range acts {
		if x == hcActDisabled {
			foundDisabled = true
		}
	}
	if !foundDisabled {
		t.Fatalf("动作列表应含 disabled，得 %v", acts)
	}
}

// TestHealthDispositionDeterministic 其余确定性异常的处置映射。
func TestHealthDispositionDeterministic(t *testing.T) {
	t.Run("请求非法 11140→软冷却+累计，连续超阈值才删除", func(t *testing.T) {
		store, px, a := newTestApp(t, &Account{UID: "b1", AccessToken: "tok", ConsumeGroup: cgProxy})
		defer store.Close()
		defer px.Close()
		cause := &upstream.Error{Kind: upstream.ErrAccountFault, Status: 400, Msg: "11140"}
		threshold := pool.AccountFaultThreshold()

		// 前 threshold-1 次：**只软冷却**，不动归属、不禁用（可逆）——
		// 这正是 2026-10-13 事故的修复点：旧实现对第 1 次就 Disable + 归档，
		// 于是一个风控窗口把 30 个健康账号批量摘掉。
		for i := 1; i < threshold; i++ {
			acts := a.applyHealthDisposition("b1", hcCodeRequestIllegal, cause, true)
			st, _ := px.pool.Status("b1")
			if st.Disabled {
				t.Fatalf("第 %d 次不该禁用（阈值 %d），动作=%v", i, threshold, acts)
			}
			if !st.Cooling {
				t.Fatalf("第 %d 次应软冷却，动作=%v", i, acts)
			}
			noted := false
			for _, x := range acts {
				if x == hcActNoteFault {
					noted = true
				}
			}
			if !noted {
				t.Fatalf("第 %d 次应记一次「请求非法」，动作=%v", i, acts)
			}
			if acc, _ := store.GetAccount("b1"); effectiveConsumeGroup(acc) == cgAbnormal {
				t.Fatalf("第 %d 次不该归档（尚未超阈值）", i)
			}
		}

		// 第 threshold 次：**删除**（v0.24.5 起，默认动作从「停用 + 归档」改为删除；
		// 理由：对话通道被 11140 拦住的号已无法服务用户，留着只占名额。见
		// docs/账号删除-v0.24.5.md。「只停用不删」的可逆路径由 kv
		// pool_delete_on_11140=0 保留，另有 TestRequestIllegalThresholdKeepsWhenDeleteOff 覆盖）。
		acts := a.applyHealthDisposition("b1", hcCodeRequestIllegal, cause, true)
		if !containsStr(acts, hcActDeleted) {
			t.Fatalf("第 %d 次应删除，动作=%v", threshold, acts)
		}
		if acc, _ := store.GetAccount("b1"); acc != nil {
			t.Fatal("达阈值后 DB 行应已删除")
		}
		if _, inPool := px.pool.Status("b1"); inPool {
			t.Fatal("达阈值后应已从池内移除")
		}
	})

	t.Run("余额耗尽→冷却到次日4点+归档", func(t *testing.T) {
		store, px, a := newTestApp(t, &Account{UID: "c1", AccessToken: "tok", ConsumeGroup: cgProxy})
		defer store.Close()
		defer px.Close()
		a.applyHealthDisposition("c1", hcCodeNoCredits,
			&upstream.Error{Kind: upstream.ErrHardCredit, Status: 402}, true)
		st, _ := px.pool.Status("c1")
		if !st.Cooling {
			t.Fatal("余额耗尽应进入冷却")
		}
		acc, _ := store.GetAccount("c1")
		if effectiveConsumeGroup(acc) != cgAbnormal {
			t.Fatalf("应归档到异常分组，得 %s", effectiveConsumeGroup(acc))
		}
		// 归档原因须带自动标记（可自动回归）。
		if !strings.HasPrefix(acc.ConsumeGroupNote, autoTagPrefix) {
			t.Fatalf("积分类异常应带自动标签以便回归，得 %q", acc.ConsumeGroupNote)
		}
	})

	t.Run("不确定的错误不做任何处置", func(t *testing.T) {
		store, px, a := newTestApp(t, &Account{UID: "w1", AccessToken: "tok", ConsumeGroup: cgProxy})
		defer store.Close()
		defer px.Close()
		// 直接对「不确定」类原因码调用处置 → 不该有任何动作（防御性断言：
		// 即使调用方误传，处置层也不该动手）。
		for _, code := range []string{hcCodeWafBlock, hcCodeRateLimited, hcCodeUpstream5xx, hcCodeTransport} {
			if acts := a.applyHealthDisposition("w1", code, nil, true); len(acts) != 0 {
				t.Fatalf("%s 不该产生处置动作，得 %v", code, acts)
			}
		}
		st, _ := px.pool.Status("w1")
		if st.Disabled || st.Cooling {
			t.Fatal("不确定结论不得改变账号状态")
		}
	})
}

// TestHealthSnapshot 批量结果快照的读写与汇总。
func TestHealthSnapshot(t *testing.T) {
	var snap healthSnapshot
	if _, _, ok := snap.get(); ok {
		t.Fatal("初始应无结果")
	}
	res := []*HealthCheckResult{
		{UID: "a", State: hcAbnormal, Definite: true},
		{UID: "b", State: hcUncertain},
		{UID: "c", State: hcHealthy},
		{UID: "d", State: hcHealthy},
	}
	snap.set(res, time.Now(), "manual")
	got, sum, ok := snap.get()
	if !ok || len(got) != 4 {
		t.Fatalf("读取失败：ok=%v n=%d", ok, len(got))
	}
	if sum.Total != 4 || sum.ByState[hcHealthy] != 2 || sum.Definite != 1 || sum.Uncertain != 1 {
		t.Fatalf("汇总不符：%+v", sum)
	}
	if sum.Source != "manual" {
		t.Fatalf("source 应透传，得 %q", sum.Source)
	}
	if sum.StartedAt == 0 || sum.EndedAt == 0 {
		t.Fatalf("起止时间应被填入：%+v", sum)
	}
	// 快照返回的是副本：改动读到的切片不该影响内部状态。
	got[0] = &HealthCheckResult{UID: "mutated"}
	if again, _, _ := snap.get(); again[0].UID != "a" {
		t.Fatal("快照必须返回副本，不能被调用方改写")
	}
}

// TestHealthHTTP 端到端：三个接口的请求/响应契约。
func TestHealthHTTP(t *testing.T) {
	store, px, a := newTestApp(t,
		&Account{UID: "h1", Nickname: "本地可判号"},
		&Account{UID: "h2", Nickname: "手动停用号", AccessToken: "tok"},
	)
	defer store.Close()
	defer px.Close()
	px.pool.SetManualDisabled("h2", true, "运维摘除")

	t.Run("单账号 404", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/admin/accounts/ghost/health", nil)
		req.SetPathValue("uid", "ghost")
		a.handleCheckAccountHealth(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("不存在应 404，得 %d（%s）", rec.Code, rec.Body.String())
		}
	})

	t.Run("单账号 200 + 返回形态", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/admin/accounts/h1/health", nil)
		req.SetPathValue("uid", "h1")
		a.handleCheckAccountHealth(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("应 200，得 %d（%s）", rec.Code, rec.Body.String())
		}
		var out struct {
			Result *HealthCheckResult `json:"result"`
			OK     bool               `json:"ok"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("解析失败：%v", err)
		}
		if out.Result == nil {
			t.Fatal("必须返回 result")
		}
		// 返回形式契约：五态字段齐全、原因非空、中文标签可用。
		if out.Result.State == "" || out.Result.StateLabel == "" || out.Result.Reason == "" {
			t.Fatalf("结果字段不完整：%+v", out.Result)
		}
	})

	t.Run("批量受理（异步）", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/admin/accounts/health",
			strings.NewReader(`{"uids":["h2"]}`))
		a.handleCheckHealthBatch(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("应 200，得 %d（%s）", rec.Code, rec.Body.String())
		}
		var out struct {
			Status string `json:"status"`
			Count  int    `json:"count"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if out.Status != "started" || out.Count != 1 {
			t.Fatalf("应为异步受理，得 %+v", out)
		}
	})

	t.Run("批量：无候选时 noop", func(t *testing.T) {
		// 给一个只含「不在池」uid 的请求 → 目标数为 1，仍会 started；
		// 空池 + 空 body 才是 noop。这里构造空 body + 池里只剩手动停用号。
		rec := httptest.NewRecorder()
		// h1 无凭证 → 不在候选（凭证取不到且未禁用）；h2 手动停用 → 不在候选。
		req := httptest.NewRequest(http.MethodPost, "/api/admin/accounts/health", nil)
		a.handleCheckHealthBatch(rec, req)
		var out struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if out.Status != "noop" {
			t.Fatalf("无候选应 noop，得 %s", rec.Body.String())
		}
	})

	t.Run("取最近结果", func(t *testing.T) {
		// 先直接写快照（不依赖异步完成），再读。
		healthLast.set([]*HealthCheckResult{{UID: "h1", State: hcHealthy, StateLabel: "正常"}},
			time.Now(), "manual")
		rec := httptest.NewRecorder()
		a.handleHealthLast(rec, httptest.NewRequest(http.MethodGet, "/api/admin/accounts/health/last", nil))
		var out struct {
			HasResult bool                 `json:"has_result"`
			Results   []*HealthCheckResult `json:"results"`
			Summary   HealthCheckSummary   `json:"summary"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if !out.HasResult || len(out.Results) != 1 || out.Summary.Total != 1 {
			t.Fatalf("取结果失败：%s", rec.Body.String())
		}
	})
}

// TestHealthCandidatesExcludesManualDisabled 全量候选集的范围：
// 排除运维手动停用，但**保留**系统禁用的号（需要被检测翻案）。
func TestHealthCandidatesExcludesManualDisabled(t *testing.T) {
	store, px, a := newTestApp(t,
		&Account{UID: "c-manual", AccessToken: "tok"},
		&Account{UID: "c-disabled", AccessToken: "tok"},
		&Account{UID: "c-normal", AccessToken: "tok"},
	)
	defer store.Close()
	defer px.Close()
	px.pool.SetManualDisabled("c-manual", true, "x")
	px.pool.Disable("c-disabled", "12153 session dead")

	got := map[string]bool{}
	for _, uid := range a.healthCheckCandidates() {
		got[uid] = true
	}
	if got["c-manual"] {
		t.Fatal("手动停用的号不该进候选（探测它没有意义）")
	}
	if !got["c-disabled"] {
		t.Fatal("系统禁用的号必须进候选 —— 被误判的号正是最需要检测的")
	}
	if !got["c-normal"] {
		t.Fatal("正常号应在候选内")
	}
}

// sessionDeadThresholdForTest 与 pool 的私有阈值保持一致（跨包不可读，故此处镜像）。
// 有 TestHealthDispositionSessionDead 断言行为，故镜像值若漂移会被该用例抓到。
const sessionDeadThresholdForTest = 3

// ===========================================================================
// v0.24.3（A 方案对照实验的产物）：上游提示透出 + 停用号 reason 不被覆盖
// ===========================================================================

// TestUpstreamNoteOf 上游错误信封里的 displayMsg 必须能取到，取不到要给空串。
func TestUpstreamNoteOf(t *testing.T) {
	ue := &upstream.Error{Kind: upstream.ErrAccountFault, Status: 403,
		Msg: "code 11140 request illegal", Display: "账号存在异常行为，已限制使用"}
	if got := upstreamNoteOf(ue); got != "账号存在异常行为，已限制使用" {
		t.Fatalf("应取到 displayMsg，得 %q", got)
	}
	if got := upstreamNoteOf(errors.New("transport boom")); got != "" {
		t.Fatalf("非上游错误应返回空串，得 %q", got)
	}
	if got := upstreamNoteOf(nil); got != "" {
		t.Fatalf("nil 应返回空串，得 %q", got)
	}
	// 11140 的 reason 要带上上游提示从句；无提示时该从句整体消失（不留空尾巴）。
	if s := upstreamNoteSuffix("上游说这个号异常"); !strings.Contains(s, "上游说这个号异常") {
		t.Fatalf("suffix 应包含原文，得 %q", s)
	}
	if s := upstreamNoteSuffix(""); s != "" {
		t.Fatalf("空提示应返回空串，得 %q", s)
	}
}

// TestRequestIllegalDoesNotClobberDisabledReason 已停用的号再被判 11140 时：
// 只该累计计数，**不该**被「未超阈值仅软冷却」的文案覆盖 reason。
//
// 2026-10-13 生产实测到该误导：两个号在池内是「已停用」，账号卡却显示
// 「累计 1/3，未超阈值仅软冷却」—— 运维会读成「还在用、只是被降温」。根因是
// pool.Cooldown 无条件写 e.reason。停用位与冷却位是两件事，不能共用一个文案。
func TestRequestIllegalDoesNotClobberDisabledReason(t *testing.T) {
	store, px, a := newTestApp(t, &Account{UID: "b1", AccessToken: "tok", ConsumeGroup: cgProxy})
	defer store.Close()
	defer px.Close()

	// 因**别的原因**停用（模拟 11140 之外的既有停用位）。
	px.pool.Disable("b1", "账号已被上游封禁（人工确认）")
	before, _ := px.pool.Status("b1")
	if !before.Disabled {
		t.Fatalf("前置：账号应已停用")
	}

	acts := a.applyHealthDisposition("b1", hcCodeRequestIllegal,
		&upstream.Error{Kind: upstream.ErrAccountFault, Status: 403,
			Msg: "code 11140 request illegal", Display: "账号存在异常行为"}, true)

	after, _ := px.pool.Status("b1")
	if !after.Disabled {
		t.Fatalf("停用位不该被本次处置清除")
	}
	if after.Reason != before.Reason {
		t.Fatalf("已停用号的 reason 被覆盖了：before=%q after=%q", before.Reason, after.Reason)
	}
	if len(acts) != 1 || acts[0] != hcActNoteFault {
		t.Fatalf("应只记一次累计（不写冷却），得 %v", acts)
	}
}

// TestShouldClearFault 11140 累计的清零口径（v0.24.3 修正）。
//
// 曾经的写法是「余额通道通过就清零」，实测被证伪：存在「余额 100% 通过、对话 100%
// 11140」的号。若那样清零，计数会「清一次→记一次」永远停在 1/3 —— 既不摘号，
// 用户请求又持续失败。故这里的表把正确口径钉死：开了探活就以对话通道为准。
func TestShouldClearFault(t *testing.T) {
	cases := []struct {
		name                 string
		probeEnabled, probed bool
		ok                   bool
		want                 bool
	}{
		{"开探活 + 对话通过 → 清", true, true, true, true},
		{"开探活 + 对话失败 → **不清**（余额通也不清）", true, true, false, false},
		{"开探活 + 没做探活 → 不清（证据不足，宁可挂着）", true, false, false, false},
		{"未开探活 → 退化为余额通过即清", false, false, false, true},
	}
	for _, c := range cases {
		if got := shouldClearFault(c.probeEnabled, c.probed, c.ok); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
