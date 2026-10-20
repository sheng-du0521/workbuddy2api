package main

import (
	"errors"
	"testing"

	"workbuddy2api/internal/upstream"
)

// 审查报告 F10：模型同步批次部分提交、并发入口未单飞。
//
// 修复前的行为：逐模型 PutModelRatio + AddModelRatioDiff。第 N 个失败时，
// 前 N-1 个已经落库、差异也可能写了 —— 函数返回 error 并打日志说"保留旧口径"，
// 而库里已经是半新半旧。两个入口（定时 / 管理端按钮）同时触发还会各写一半。
//
// 修复后：先算完整快照 → 一个事务提交；同一时刻只允许一轮同步。
func TestModelSyncBatchIsAtomic(t *testing.T) {
	store, _, a := newTestApp(t)
	defer store.Close()

	a.modelRatioFetch = func() ([]upstream.ModelInfo, error) {
		return []upstream.ModelInfo{
			{ID: "f10-m1", Name: "模型一", Credits: "x0.10"},
			{ID: "f10-m2", Name: "模型二", Credits: "x0.20"},
		}, nil
	}

	// 让第二个模型的写入必然失败：整批必须回滚。
	if _, err := store.db.Exec(`CREATE TRIGGER f10_fail_m2 BEFORE INSERT ON model_ratios
		WHEN NEW.model = 'f10-m2' BEGIN SELECT RAISE(ABORT, 'boom'); END;`); err != nil {
		t.Fatalf("建触发器: %v", err)
	}

	if _, err := a.syncModelRatios(); err == nil {
		t.Fatal("第二个模型写入失败时整轮应报错")
	}
	var n int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM model_ratios WHERE model IN ('f10-m1','f10-m2')`).Scan(&n); err != nil {
		t.Fatalf("查询: %v", err)
	}
	if n != 0 {
		t.Fatalf("整批必须回滚，库里却留下了 %d 条（半批提交）", n)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM model_ratio_diffs WHERE model IN ('f10-m1','f10-m2')`).Scan(&n); err != nil {
		t.Fatalf("查询差异: %v", err)
	}
	if n != 0 {
		t.Fatalf("失败时不该留下差异记录，得 %d 条", n)
	}

	// 故障移除后重跑：两条都应写入，且各有一条 new_model 差异。
	if _, err := store.db.Exec(`DROP TRIGGER f10_fail_m2`); err != nil {
		t.Fatalf("删触发器: %v", err)
	}
	res, err := a.syncModelRatios()
	if err != nil {
		t.Fatalf("重跑: %v", err)
	}
	if res.Total != 2 || res.Added != 2 {
		t.Fatalf("应同步 2 个新模型，得 total=%d added=%d", res.Total, res.Added)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM model_ratios WHERE model IN ('f10-m1','f10-m2')`).Scan(&n); err != nil {
		t.Fatalf("查询: %v", err)
	}
	if n != 2 {
		t.Fatalf("重跑后应有 2 条，得 %d", n)
	}
}

// TestModelSyncSingleFlight 两个入口同时触发时，第二个必须被明确拒绝。
func TestModelSyncSingleFlight(t *testing.T) {
	store, _, a := newTestApp(t)
	defer store.Close()

	// 模拟"已有一轮在跑"：持有同一把锁。
	a.modelRatioMu.Lock()
	_, err := a.syncModelRatios()
	a.modelRatioMu.Unlock()

	if !errors.Is(err, errModelRatioSyncBusy) {
		t.Fatalf("并发第二轮应返回 errModelRatioSyncBusy，得 %v", err)
	}
}
