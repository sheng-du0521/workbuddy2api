// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"strconv"
	"testing"

	"workbuddy2api/internal/pool"
)

// TestCreditRefreshCandidates 候选集口径（v0.17.8 起，v0.20.0 扩为两类）：
// 需要刷新的号 = 「积分未获取」或「池内余额为 0」且可用；停用的不出站。
//
// 注：`SetCredits`/`SetCreditsDetailed` 现在都会打戳 creditsSeenAt，因此本用例里
// u2（SetCredits 500）落在「已打戳 + 非 0」→ 仍不是候选。**判据的组合覆盖**
// 见 TestNeedRefreshCredits —— 「未打戳 + 非 0」这种升级态无法在此凭空构造。
func TestCreditRefreshCandidates(t *testing.T) {
	store, px, a := newTestApp(t,
		&Account{UID: "u1", Nickname: "甲", AccessToken: "tok1"},
		&Account{UID: "u2", Nickname: "乙", AccessToken: "tok2"},
		&Account{UID: "u3", Nickname: "丙", AccessToken: "tok3", Disabled: true},
	)
	defer store.Close()
	defer px.Close()

	// u1 未打戳（从未拉取）→ 候选；u2 已打戳且非 0 → 不是候选；u3 停用 → 不是候选。
	px.pool.SetCredits("u2", 500)

	got := a.creditRefreshCandidates()
	if len(got) != 1 || got[0] != "u1" {
		t.Fatalf("候选应只有 u1，得 %v", got)
	}
}

// TestNeedRefreshCredits 候选判据的组合覆盖（v0.20.0 核心修复）。
//
// 用户报告：账号池点「上架」提示「未获取积分」，点了刷新也没用。
// 根因是旧候选集只挑「余额为 0」的号，而升级后老号的 credits 都是非 0 的历史
// 快照、creditsSeenAt 却为 0 → 被全部排除 → 永远刷不到。
// 本用例锁定判据：**未打戳（CreditsSeenAt == 0）就必须刷，无论余额多少**。
func TestNeedRefreshCredits(t *testing.T) {
	cases := []struct {
		name string
		st   pool.Status
		want bool
	}{
		{"未打戳+非0余额（升级后的老号，本次缺陷核心）", pool.Status{Credits: 5000}, true},
		{"未打戳+0余额（从未拉取过）", pool.Status{Credits: 0}, true},
		{"已打戳+非0余额（刚对齐过，无需再打上游）", pool.Status{Credits: 5000, CreditsSeenAt: 1}, false},
		{"已打戳+0余额（真没钱，仍刷一次以确认）", pool.Status{Credits: 0, CreditsSeenAt: 1}, true},
		{"停用账号一律不刷（未打戳）", pool.Status{Disabled: true}, false},
		{"手动摘除账号一律不刷（未打戳）", pool.Status{ManualDisabled: true}, false},
		{"停用账号一律不刷（已打戳+0）", pool.Status{Disabled: true, Credits: 0, CreditsSeenAt: 1}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := needRefreshCredits(c.st); got != c.want {
				t.Fatalf("needRefreshCredits(%+v)=%v want %v", c.st, got, c.want)
			}
		})
	}
}

// TestImportedUIDsCap 导入后的刷新目标列表受上限约束（避免一次点击打出上百个上游请求）。
func TestImportedUIDsCap(t *testing.T) {
	store, px, a := newTestApp(t)
	defer store.Close()
	defer px.Close()

	var res desktopImportResult
	for i := 0; i < creditsRefreshMaxAccounts+5; i++ {
		res.UIDs = append(res.UIDs, "uid-"+strconv.Itoa(i))
	}
	got := a.importedUIDs(&res)
	if len(got) != creditsRefreshMaxAccounts {
		t.Fatalf("应截到 %d 个，得 %d", creditsRefreshMaxAccounts, len(got))
	}
	// nil / 空结果不 panic。
	if a.importedUIDs(nil) != nil {
		t.Fatalf("nil 结果应返回 nil")
	}
	if a.importedUIDs(&desktopImportResult{}) != nil {
		t.Fatalf("空 UIDs 应返回 nil")
	}
}
