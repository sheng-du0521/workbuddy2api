// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"testing"
	"time"

	"workbuddy2api/internal/scheduler"
)

// newAsyncRunner 造一个只含调度器骨架的 taskRunner（无真实上游）。
// 后台任务会快速失败，但本文件的断言目标是「受理语义」而非执行结果。
func newAsyncRunner(t *testing.T) *taskRunner {
	t.Helper()
	store, _, _ := newTestApp(t, &Account{UID: "u1", Realm: "cn", AccessToken: "at"})
	t.Cleanup(func() { _ = store.Close() })
	return &taskRunner{sch: scheduler.New(scheduler.Config{}), store: store, cfg: DefaultTaskConfig()}
}

// TestRunNowIsAsync RunNow 必须是**异步**的：调用立刻返回「已开始」，
// 而不是等整轮跑完再返回。
//
// 这是 v0.17.4 从同步改异步的回归锚点。此前同步阻塞导致：
//   - 保活 18 个号实测 6.4s、活跃上报 2~3 分钟，前端全程只有静态提示，看不到进度；
//   - 挂到反代/隧道后被 60s 读超时打断，前端收到的不是「正在跑」而是失败。
func TestRunNowIsAsync(t *testing.T) {
	tr := newAsyncRunner(t)

	for _, kind := range []string{"keepalive", "cat", "travel", "activity", "checkin"} {
		res, err := tr.RunNow(kind)
		if err != nil {
			// 允许「已在执行中」——多个任务共享同一测试调度器时会撞车，
			// 这不是本用例要验的东西。
			if err == ErrTaskBusy {
				continue
			}
			t.Fatalf("%s 空闲时应立即受理，得 error=%v", kind, err)
		}
		if got := res["status"]; got != "started" {
			t.Fatalf("%s 应返回 status=started（异步受理），得 %v", kind, got)
		}
		if res["kind"] != kind {
			t.Fatalf("%s 应回传 kind，得 %v", kind, res)
		}
	}
}

// TestRunNowUnknownKind 未知任务类仍报错（不能被异步化顺手吞掉）。
func TestRunNowUnknownKind(t *testing.T) {
	tr := newAsyncRunner(t)
	if _, err := tr.RunNow("nope"); err == nil {
		t.Fatal("未知任务类应报错")
	}
}

// TestRunNowSchoolUnavailable school 脚本链路不可用时拒绝（不静默受理）。
func TestRunNowSchoolUnavailable(t *testing.T) {
	tr := newAsyncRunner(t)
	tr.schoolOK = false
	if _, err := tr.RunNow("school"); err == nil {
		t.Fatal("school 链路不可用时应报错")
	}
}

// TestLedgerExposesRunningAndNextRuns 台账接口必须透出运行态与下次触发时刻 ——
// 这是「定时有没有生效」「任务是不是正在跑」对前端可见的唯一来源。
func TestLedgerExposesRunningAndNextRuns(t *testing.T) {
	tr := newAsyncRunner(t)
	tr.started = time.Now()

	out := tr.Ledger()
	if _, ok := out["running"]; !ok {
		t.Fatalf("Ledger 应含 running 字段，得 %v", keysOfAny(out))
	}
	nx, ok := out["next_runs"].(map[string]nextRunView)
	if !ok {
		t.Fatalf("Ledger 应含 next_runs（map[string]nextRunView），得 %T", out["next_runs"])
	}
	// 默认配置下六类自动任务都启用 → 都应有下次时刻。
	if len(nx) != 6 {
		t.Fatalf("默认应给出 6 类任务的下次时刻，得 %d：%v", len(nx), nx)
	}
	if _, ok := nx["growth"]; ok {
		t.Fatal("成长任务不排程，不应有下次时刻")
	}
	for k, v := range nx {
		if v.Rel <= 0 {
			t.Fatalf("%s 的剩余秒数应为正（前端据此显示「X 后」），得 %d", k, v.Rel)
		}
		if v.At == 0 {
			t.Fatalf("%s 应给出绝对时刻", k)
		}
	}
}

// TestLedgerNilSchedulerSafe 调度器缺席（部分启动路径）时不 panic。
func TestLedgerNilSchedulerSafe(t *testing.T) {
	tr := &taskRunner{}
	out := tr.Ledger()
	if out == nil {
		t.Fatal("应返回空结果而非 nil")
	}
}

func keysOfAny(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
