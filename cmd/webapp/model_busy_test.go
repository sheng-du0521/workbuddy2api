// 「同 (账号, 模型) 在途请求」的软偏好测试（v0.31.7）。
//
// 背景：模型级冷却（6004）是**响应到达之后**才写入的，所以同一个 (账号, 模型) 的并发
// 请求会在冷却写下之前各自选到同一个号 —— 一次上游限额被放大成 N 次必然失败的请求
// （而且都从同一个出口 IP 出去）。
//
// 实测数据（近 7 天 4086 次请求）：同 (账号,模型) ≤10s 的重复调度有 398 对，其中含失败的
// **0** 对；67 次 6004 里也没有一次是它造成的。故处置做成**软偏好**（有别的号才换），
// 而**不是**硬互斥 —— 本文件逐条钉住"偏好生效"与"可用性绝不因此下降"两侧。
//
// 为什么偏好逻辑用**注入取号原语**的单测钉（而不是只测端到端）：池内选号是加权随机 +
// 闲置补偿 + LRU 兜底，"哪个号是自然首选"在两次请求之间并不稳定；把它写成端到端断言
// 只会得到一个偶发失败的用例。端到端只留"绝不影响可用性 / 计数不泄漏"这类确定性结论。
package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// busyModel 本文件的模型名（与别的用例隔离）。
const busyModel = "glm-busy"

// newTestProxyForPick 两个「参与消耗」分组的账号 + 已 ReloadSelection 的 Proxy ——
// 只用来测**无上游**的取号逻辑（allowsSelection / inConsumeGroup 需要真实映射）。
func newTestProxyForPick(t *testing.T) (*Store, *Proxy, *app) {
	t.Helper()
	mk := func(uid, tok string) *Account {
		return &Account{
			UID: uid, Nickname: uid, Realm: "cn",
			AccessToken: tok, RefreshToken: tok + "-r",
			ExpiresAt:    time.Now().Add(48 * time.Hour).Unix(),
			ConsumeGroup: "apply", // 平台选号只认「参与消耗」的分组
			PoolState:    "in",
		}
	}
	store, px, a := newTestApp(t, mk(coolUIDA, "tok-a"), mk(coolUIDB, "tok-b"))
	t.Cleanup(func() { store.Close() })
	px.ReloadSelection()
	return store, px, a
}

// seqPicker 按给定顺序依次返回候选（被 tried 排除后取下一个，耗尽返回 nil）——
// 模拟 pool 在被要求"再给一个"时的行为，但顺序**完全确定**。
func seqPicker(uids ...string) pickOneFn {
	return func(tried map[string]bool, model string) *auth.Auth {
		for _, u := range uids {
			if !tried[u] {
				return &auth.Auth{UID: u}
			}
		}
		return nil
	}
}

// TestModelBusySkipsBusyCandidate 有别的候选时，跳过该模型上有在途请求的号。
func TestModelBusySkipsBusyCandidate(t *testing.T) {
	_, px, _ := newTestProxyForPick(t)
	px.noteModelInFlight(coolUIDA, busyModel)
	defer px.clearModelInFlight(coolUIDA, busyModel)

	got := px.pickScopedWith(map[string]bool{}, busyModel, pickScope{},
		"", seqPicker(coolUIDA, coolUIDB))
	if got == nil || got.UID != coolUIDB {
		t.Fatalf("A 在该模型上有在途请求 → 应改选 B，got %v", got)
	}
	if n := px.modelBusySkips.Load(); n != 1 {
		t.Errorf("改选应被计数一次（留证据用），got %d", n)
	}
	if v := px.routeView(); v["model_busy_skips"].(int64) != 1 || v["model_inflight"].(int) != 1 {
		t.Errorf("/api/admin/live 的 route 块应透出计数，got %v", v)
	}
}

// TestModelBusyNeverFailsRequest 所有候选在该模型上都忙 → 照常交出去（软偏好不是互斥）。
//
// 这条比"偏好生效"更重要：**可用性不能因为一个未观测到的问题而下降**。
func TestModelBusyNeverFailsRequest(t *testing.T) {
	_, px, _ := newTestProxyForPick(t)
	px.noteModelInFlight(coolUIDA, busyModel)
	px.noteModelInFlight(coolUIDB, busyModel)
	defer func() {
		px.clearModelInFlight(coolUIDA, busyModel)
		px.clearModelInFlight(coolUIDB, busyModel)
	}()

	got := px.pickScopedWith(map[string]bool{}, busyModel, pickScope{},
		"", seqPicker(coolUIDA, coolUIDB))
	if got == nil {
		t.Fatal("全忙也必须返回一个号（返回 nil 会让上层回 503 —— 比重复调度更糟）")
	}
	if got.UID != coolUIDA {
		t.Errorf("全忙时应回落到**第一个**候选（保持原有语义），got %q", got.UID)
	}
}

// TestModelBusySingleAccountStillServed 单号池：该号在该模型上忙时仍必须被服务。
func TestModelBusySingleAccountStillServed(t *testing.T) {
	_, px, _ := newTestProxyForPick(t)
	px.noteModelInFlight(coolUIDA, busyModel)
	defer px.clearModelInFlight(coolUIDA, busyModel)

	got := px.pickScopedWith(map[string]bool{}, busyModel, pickScope{}, "", seqPicker(coolUIDA))
	if got == nil || got.UID != coolUIDA {
		t.Fatalf("单号池只有它可用，必须照用，got %v", got)
	}
}

// TestModelBusyOtherModelUnaffected 只影响**同一个模型**：别的模型照常可选该号。
func TestModelBusyOtherModelUnaffected(t *testing.T) {
	_, px, _ := newTestProxyForPick(t)
	px.noteModelInFlight(coolUIDA, busyModel)
	defer px.clearModelInFlight(coolUIDA, busyModel)

	got := px.pickScopedWith(map[string]bool{}, "glm-other", pickScope{},
		"", seqPicker(coolUIDA, coolUIDB))
	if got == nil || got.UID != coolUIDA {
		t.Fatalf("在途计数是按模型的，别的模型不该受影响，got %v", got)
	}
	if n := px.modelBusySkips.Load(); n != 0 {
		t.Errorf("别的模型不该产生改选计数，got %d", n)
	}
}

// TestModelBusySkipsBeforeBusyAccountEndToEnd 端到端：真实请求走完后在途计数归零、
// 且服务账号可被识别（不泄漏、不误伤）。
func TestModelBusySkipsBeforeBusyAccountEndToEnd(t *testing.T) {
	stub, _ := newStub()
	_, px, srv, raw := newCoolingApp(t, stub)

	body := `{"model":"` + busyModel + `","messages":[{"role":"user","content":"leak-check"}]}`
	resp := doChat(t, srv, raw, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应 200，got %d", resp.StatusCode)
	}
	served := resp.Header.Get("X-Wb-Account")
	if served == "" {
		t.Fatal("响应应透出服务账号")
	}
	time.Sleep(50 * time.Millisecond) // 等 defer 释放走完
	if px.modelBusy(served, busyModel) {
		t.Fatalf("请求结束后 %s 的 %s 在途计数应归零（泄漏会让该号被永久跳过）", served, busyModel)
	}
	if v := px.routeView(); v["model_inflight"].(int) != 0 {
		t.Errorf("在途键数应归零，got %v", v["model_inflight"])
	}

	// 失败路径同样要归还：让上游永远 429
	stub.fail429[busyModel] = 1 << 20
	resp2 := doChat(t, srv, raw, strings.Replace(body, "leak-check", "leak-check-2", 1))
	if resp2.StatusCode == http.StatusOK {
		t.Fatal("上游 429 时不该 200")
	}
	time.Sleep(50 * time.Millisecond)
	if v := px.routeView(); v["model_inflight"].(int) != 0 {
		t.Errorf("失败路径也要归还在途计数，got %v", v["model_inflight"])
	}
	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Logf("提示：失败路径状态码 = %d（不参与断言，仅记录）", resp2.StatusCode)
	}
}
