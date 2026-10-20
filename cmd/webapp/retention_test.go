// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"strings"
	"testing"
	"time"
)

// ======================================================================
// 用量流水保留策略的回归网（v0.31.66）
//
// 删数据是**不可逆**的，所以这批用例的重点不是"能删掉"，而是"**不会多删**"：
//   - 边界那一行（正好等于 cutoff）必须活着；
//   - 保留策略为 0/负数时必须一行都不动；
//   - 配置写错时必须退回默认值，而不是被当成"永久"或被当成"全删"。
// ======================================================================

// seedLog 插一条指定时间的流水（其余字段无所谓）。
func seedLog(t *testing.T, store *Store, at int64) {
	t.Helper()
	if _, err := store.db.Exec(`INSERT INTO usage_logs (user_id, model, charged_micro, status, created_at)
		VALUES (0, 'm-retention', 0, 'ok', ?)`, at); err != nil {
		t.Fatalf("插入流水失败：%v", err)
	}
}

func countLogs(t *testing.T, store *Store) int64 {
	t.Helper()
	var n int64
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM usage_logs`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// P1：只删早于 cutoff 的行，边界那一行必须保留。
func TestPruneUsageLogsKeepsBoundary(t *testing.T) {
	store, _ := newHostProxy(t)
	defer store.Close()

	now := time.Now().Unix()
	seedLog(t, store, now-100*24*3600) // 老
	seedLog(t, store, now-30*24*3600)  // 新
	seedLog(t, store, now-10)          // 刚写入

	cutoff := now - 60*24*3600
	n, err := store.PruneUsageLogs(cutoff, 1) // batch=1 顺带验证"多批也能删干净"
	if err != nil {
		t.Fatalf("PruneUsageLogs: %v", err)
	}
	if n != 1 {
		t.Fatalf("应只删 1 行，得 %d", n)
	}
	if left := countLogs(t, store); left != 2 {
		t.Fatalf("应剩 2 行，得 %d", left)
	}

	// 边界：created_at 正好等于 cutoff 的行**不算过期**（< 而不是 <=）。
	seedLog(t, store, cutoff)
	if n, err := store.PruneUsageLogs(cutoff, 10); err != nil || n != 0 {
		t.Fatalf("边界行不该被删：deleted=%d err=%v", n, err)
	}
	if left := countLogs(t, store); left != 3 {
		t.Fatalf("边界行应保留，得 %d 行", left)
	}
}

// P2：保留策略为 0 / 负数 = 永久保留，一行都不许动。
func TestPruneUsageLogsDisabledByPolicy(t *testing.T) {
	for _, v := range []string{"0", "-1"} {
		t.Run("retention="+v, func(t *testing.T) {
			store, a, _, _, _, _ := eaAutoApp(t)
			defer store.Close()
			if err := store.SetKV(kvUsageRetentionDays, v); err != nil {
				t.Fatalf("SetKV: %v", err)
			}
			seedLog(t, store, time.Now().Unix()-3650*24*3600) // 十年前
			before := countLogs(t, store)
			rep := a.runUsagePrune("test")
			if !rep.Skipped || rep.Deleted != 0 {
				t.Fatalf("永久保留时不得删除，得 %+v", rep)
			}
			if after := countLogs(t, store); after != before {
				t.Fatalf("行数被动了：%d -> %d", before, after)
			}
		})
	}
}

// P3：按策略清理时，报告要如实（删了多少、cutoff、保留天数），并落 kv。
func TestRunUsagePruneReportsAndPersists(t *testing.T) {
	store, a, _, _, _, _ := eaAutoApp(t)
	defer store.Close()

	if err := store.SetKV(kvUsageRetentionDays, "30"); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	now := time.Now().Unix()
	seedLog(t, store, now-100*24*3600)
	seedLog(t, store, now-40*24*3600)
	seedLog(t, store, now-3600)

	rep := a.runUsagePrune("test")
	if rep.Error != "" || rep.Deleted != 2 {
		t.Fatalf("应删 2 行且无错，得 %+v", rep)
	}
	if rep.Retention != 30 || rep.Cutoff >= now-29*24*3600 {
		t.Fatalf("cutoff 应为 30 天前，得 retention=%d cutoff=%d", rep.Retention, rep.Cutoff)
	}
	last, ok := a.lastPruneReport()
	if !ok || last.Deleted != 2 || last.At != rep.At {
		t.Fatalf("结果应落 kv 可读回，得 %+v (ok=%v)", last, ok)
	}
	if left := countLogs(t, store); left != 1 {
		t.Fatalf("应剩 1 行，得 %d", left)
	}
}

// P4：配置写错 → 退回默认值（180 天），**不能**被当成永久、也不能当成 0。
//
// 这条针对的是"一个人手滑把 `180` 写成 `180天`"：静默变成"永久保留"会让人以为
// 清理在跑；静默变成"删全部"更糟。两个方向都必须有确定行为。
func TestRetentionDaysFallsBackOnGarbage(t *testing.T) {
	store, a, _, _, _, _ := eaAutoApp(t)
	defer store.Close()

	if err := store.SetKV(kvUsageRetentionDays, "180天"); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	if got := a.usageRetentionDays(); got != defaultUsageRetentionDays {
		t.Fatalf("非法配置应退回默认 %d，得 %d", defaultUsageRetentionDays, got)
	}
	if err := store.SetKV(kvUsageRetentionDays, ""); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	if got := a.usageRetentionDays(); got != defaultUsageRetentionDays {
		t.Fatalf("未设置应走默认 %d，得 %d", defaultUsageRetentionDays, got)
	}
}

// P5：后台接口给的是真实占用（行数/页数/可回收字节），不是猜测。
func TestAdminDataUsageSnapshot(t *testing.T) {
	store, a, mux, adminCookie, _, _ := eaAutoApp(t)
	defer store.Close()

	now := time.Now().Unix()
	// 一条超过默认保留期（180 天），一条是刚刚的 —— 判据必须能区分两者。
	seedLog(t, store, now-400*24*3600)
	seedLog(t, store, now-10)

	code, body := eaReq(t, mux, "GET", "/api/admin/data-usage", "", adminCookie, "")
	if code != 200 {
		t.Fatalf("应 200，得 %d %s", code, body)
	}
	if !strings.Contains(body, `"usage_logs":2`) {
		t.Fatalf("快照应报告真实行数 2，得 %s", body)
	}
	if !strings.Contains(body, `"retention_days":`) {
		t.Fatalf("快照应带生效的保留天数，得 %s", body)
	}

	// ?prune=1 立即清理一次（默认 180 天 → 只删 100 天前那条）。
	code2, body2 := eaReq(t, mux, "GET", "/api/admin/data-usage?prune=1", "", adminCookie, "")
	if code2 != 200 || !strings.Contains(body2, `"deleted":1`) {
		t.Fatalf("应清理 1 行，得 %d %s", code2, body2)
	}

	// 未登录必须被挡住（数据体积与行数属于运营信息）。
	if code3, _ := eaReq(t, mux, "GET", "/api/admin/data-usage", "", "", ""); code3 == 200 {
		t.Fatal("未登录不该拿到数据快照")
	}
	_ = a
}
