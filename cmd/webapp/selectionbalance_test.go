// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// seedUsage 往流水里塞 n 条归属于 uid 的记录。
func seedBalanceUsage(t *testing.T, a *app, uid string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := a.store.InsertUsageLog(&UsageLog{
			UserID: 1, Model: "m", UpstreamUID: uid, Status: "ok",
		}); err != nil {
			t.Fatalf("InsertUsageLog: %v", err)
		}
	}
}

// TestSelectionBalanceMath 均衡度的数学口径（R7 的核心：把"看着还行"变成一行数字）。
//
// 关键口径两条，都用例锁住：
//   - 理想占比的分母是**池内账号总数**（没被用到的号也要占分母），否则"只用了一个号"
//     反而会算出理想占比 100%、偏斜 1.0 倍的荒谬结论；
//   - Warn 只看最高占比是否越阈值。
func TestSelectionBalanceMath(t *testing.T) {
	store, px, a := newTestApp(t,
		&Account{UID: "b-1", Nickname: "甲", AccessToken: "t"},
		&Account{UID: "b-2", Nickname: "乙", AccessToken: "t"},
		&Account{UID: "b-3", Nickname: "丙", AccessToken: "t"},
		&Account{UID: "b-4", Nickname: "丁", AccessToken: "t"},
	)
	defer store.Close()
	defer px.Close()

	// 100 条请求几乎全落在 b-1 上（最坏情形：单号独占）。
	seedBalanceUsage(t, a, "b-1", 100)
	// 另加 3 条分散在 b-2/b-3/b-4，让它们出现在"用过"的集合里。
	seedBalanceUsage(t, a, "b-2", 1)
	seedBalanceUsage(t, a, "b-3", 1)
	seedBalanceUsage(t, a, "b-4", 1)

	rec := httptest.NewRecorder()
	a.handleSelectionBalance(rec, httptest.NewRequest(http.MethodGet, "/api/admin/selection-balance?hours=24", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d：%s", rec.Code, rec.Body.String())
	}
	v, err := a.selectionBalance(24)
	if err != nil {
		t.Fatalf("selectionBalance: %v", err)
	}
	if v.TotalRequests != 103 {
		t.Fatalf("总请求应为 103，实得 %d", v.TotalRequests)
	}
	if v.AccountsUsed != 4 || v.AccountsTotal != 4 {
		t.Fatalf("覆盖口径不符：used=%d total=%d", v.AccountsUsed, v.AccountsTotal)
	}
	if v.IdealSharePct < 24.9 || v.IdealSharePct > 25.1 {
		t.Fatalf("理想占比应为 25%%（4 个号），实得 %.2f", v.IdealSharePct)
	}
	if !v.Warn || v.Verdict != "skewed" {
		t.Fatalf("单号独占 97%% 必须告警，实得 warn=%v verdict=%s", v.Warn, v.Verdict)
	}
	if len(v.Top) == 0 || v.Top[0].UID != "b-1" || v.Top[0].Count != 100 {
		t.Fatalf("top 列表不对：%+v", v.Top)
	}
	if v.Top[0].SharePct < 97 || v.Top[0].SharePct > 98 {
		t.Fatalf("top1 占比应约 97%%，实得 %.2f", v.Top[0].SharePct)
	}
}

// TestSelectionBalanceHealthyIsNotWarned 均匀分布不该告警（否则运维会忽略告警）。
func TestSelectionBalanceHealthyIsNotWarned(t *testing.T) {
	store, px, a := newTestApp(t,
		&Account{UID: "h-1", AccessToken: "t"},
		&Account{UID: "h-2", AccessToken: "t"},
		&Account{UID: "h-3", AccessToken: "t"},
		&Account{UID: "h-4", AccessToken: "t"},
	)
	defer store.Close()
	defer px.Close()

	for _, uid := range []string{"h-1", "h-2", "h-3", "h-4"} {
		seedBalanceUsage(t, a, uid, 10)
	}
	v, err := a.selectionBalance(24)
	if err != nil {
		t.Fatalf("selectionBalance: %v", err)
	}
	if v.Warn {
		t.Fatalf("完全均匀（各 25%%）不该告警：%+v", v)
	}
	if v.Verdict != "ok" {
		t.Fatalf("应判定 ok，实得 %s（advice=%s）", v.Verdict, v.Advice)
	}
}

// TestSelectionBalanceNoSample 窗口内无流水时要明确说"样本不足"，而不是给个 0% 让人误读。
func TestSelectionBalanceNoSample(t *testing.T) {
	store, px, a := newTestApp(t, &Account{UID: "n-1", AccessToken: "t"})
	defer store.Close()
	defer px.Close()

	v, err := a.selectionBalance(24)
	if err != nil {
		t.Fatalf("selectionBalance: %v", err)
	}
	if v.Verdict != "no-sample" || v.TotalRequests != 0 || v.Warn {
		t.Fatalf("无样本时应明确报 no-sample 且不告警：%+v", v)
	}
	if v.Advice == "" {
		t.Fatal("无样本时也要给出人话说明")
	}
}
