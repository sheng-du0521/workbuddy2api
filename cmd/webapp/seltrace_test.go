// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"testing"
	"time"
)

// TestStickyTraceFlagsAccountSwitching 诊断接口必须能识别「同一场对话被多个账号服务」。
//
// 这是排查闭环的关键：证明了机制修复之后，还要有办法**持续**确认线上有没有再出现换号。
func TestStickyTraceFlagsAccountSwitching(t *testing.T) {
	store, _, a := newTestApp(t)
	defer store.Close()

	inject := func(hash, uid string, attempts int, ageSec int64) {
		if err := store.InsertUsageLog(&UsageLog{
			UserID: 1, Model: "m", Status: "ok", UpstreamUID: uid,
			StickyKeyHash: hash, Attempts: attempts,
			CreatedAt: time.Now().Unix() - ageSec,
		}); err != nil {
			t.Fatalf("InsertUsageLog: %v", err)
		}
	}

	// 会话 A：3 次请求，落到 2 个账号（应被判为换号）。
	inject("hash-aaa", "uid-1", 1, 300)
	inject("hash-aaa", "uid-1", 1, 200)
	inject("hash-aaa", "uid-2", 2, 100)
	// 会话 B：2 次请求都在同一账号（不该被判为换号）。
	inject("hash-bbb", "uid-9", 1, 150)
	inject("hash-bbb", "uid-9", 1, 50)

	// 注意：store 是独立连接，a.store 与它同一个 *sql.DB（newTestApp 传入的是同一个 store）。
	// v0.24.1（P3-9）：stickyTrace 改为返回 (map, error) —— 调用方按 error 决定状态码，
	// 不再把失败伪装成「200 + error 字段」。
	out, err := a.stickyTrace(24*60, 50)
	if err != nil {
		t.Fatalf("stickyTrace 不应报错：%v", err)
	}
	if got := out["sessions"].(int); got != 2 {
		t.Fatalf("应识别 2 个会话，得 %d", got)
	}
	if got := out["switching_sessions"].(int); got != 1 {
		t.Fatalf("应识别 1 个换号会话，得 %d", got)
	}
	rows := out["switching"].([]stickySwitchRow)
	if len(rows) != 1 {
		t.Fatalf("换号列表应 1 条，得 %d", len(rows))
	}
	if rows[0].StickyKeyHash != "hash-aaa" {
		t.Fatalf("换号会话应为 hash-aaa，得 %q", rows[0].StickyKeyHash)
	}
	if len(rows[0].Accounts) != 2 {
		t.Fatalf("该会话应关联 2 个账号，得 %v", rows[0].Accounts)
	}
	if rows[0].Requests != 3 {
		t.Fatalf("请求数应为 3，得 %d", rows[0].Requests)
	}
	if rows[0].FailoverRuns != 1 {
		t.Fatalf("失败换号回退次数应为 1，得 %d", rows[0].FailoverRuns)
	}

	// 收窄到 1 分钟窗口：只剩 hash-bbb 那条 50 秒前的记录（hash-aaa 全在窗口外），
	// 且它从不换号 —— 证明窗口过滤真的生效，而不是把历史数据当现状。
	recent, err := a.stickyTrace(1, 50)
	if err != nil {
		t.Fatalf("stickyTrace 不应报错：%v", err)
	}
	if got := recent["switching_sessions"].(int); got != 0 {
		t.Fatalf("1 分钟窗口内不应有换号会话，得 %d", got)
	}
	if got := recent["sessions"].(int); got != 1 {
		t.Fatalf("1 分钟窗口内应只剩 1 个会话，得 %d", got)
	}
}
