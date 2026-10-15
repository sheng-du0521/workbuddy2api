// 模型级冷却的**半开探测**端到端验收（v0.31.6）。
//
// 走完整的 ServeChat + 真选号 + 真池状态 + 假上游，逐条钉住：
//  1. 池里所有号对该模型都还在 6004 冷却、上游其实已提前解除 → 一次探测即恢复；
//  2. 上游**没有**解除 → 探测失败**不得**在间隔内重复（这是出站强度的边界，
//     也是本功能唯一可能被滥用成"打上游"的地方）；
//  3. 11102 负缓存**不**探测（重试无意义）；
//  4. 关停开关（interval=0）生效；
//  5. 有别的可用号时走常规路径，**不**浪费探测名额。
package main

import (
	"net/http"
	"testing"
	"time"
)

// probeModel 本文件的探针模型名（与别的用例隔离，避免假上游计数串味）。
const probeModel = "glm-probe"

// coolAllForModel 把两个号都置成"对该模型 6004 冷却"，A 的窗口更短（因此更早被探）。
func coolAllForModel(t *testing.T, px *Proxy) {
	t.Helper()
	now := time.Now()
	px.pool.CooldownSoftForModel(coolUIDA, time.Minute, now.Add(3*time.Hour), probeModel,
		"6004 model rate limit [reset=body-json]")
	px.pool.CooldownSoftForModel(coolUIDB, time.Minute, now.Add(6*time.Hour), probeModel,
		"6004 model rate limit [reset=body-json]")
	// 单测等不起默认的 20m：把 delay 压到 1ms，并 sleep 一小段确保**时钟已越过 Since+delay**。
	//
	// ⚠️ 不能压到 1ns：Windows 的 time.Now 粒度可达毫秒级，两次调用可能返回**同一时刻**，
	// 于是 `now.Before(Since+delay)` 成立 → 探测被判定"还没到点"。
	// 本用例在全量跑时偶发失败正是这个原因（单独跑却总是过 —— 因为单独跑时代码路径更慢）。
	// 这是**夹具**问题不是实现问题：生产默认 delay 是 20 分钟，粗粒度时钟毫无影响。
	// interval 用 1h（要验证"同号间隔内不重复探"）；global 用 1m（要验证"全池不连探"）。
	px.pool.SetModelProbe(time.Millisecond, time.Hour, time.Minute)
	time.Sleep(20 * time.Millisecond)
}

// TestModelProbeRecoversWhenUpstreamLiftedEarly 核心收益：上游提前解除 → 一次探测把
// 容量拿回来（此前只能干等到墙钟，最长 24h）。
func TestModelProbeRecoversWhenUpstreamLiftedEarly(t *testing.T) {
	stub, _ := newStub() // 不设 fail → 上游"已经恢复正常"
	_, px, srv, raw := newCoolingApp(t, stub)
	coolAllForModel(t, px)

	// ① 常规候选为空（两个号对该模型都在冷却）→ 走一次半开探测 → 上游 200
	resp := doChat(t, srv, raw, `{"model":"`+probeModel+`","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("探测应成功（上游其实已恢复），got %d", resp.StatusCode)
	}
	if got := stub.callCount(probeModel); got != 1 {
		t.Fatalf("一次客户端请求只允许一次探测（出站强度闸门），上游调用 got %d want 1", got)
	}
	served := resp.Header.Get("X-Wb-Account")
	if served != coolUIDA {
		t.Fatalf("应探测**最早到期**的那个号（A，3h < B 6h），got %q", served)
	}
	// 探测成功 = 直接证据 → 该 (账号, 模型) 的冷却被提前解除
	if _, ok := modelCoolingOf(t, px, coolUIDA, probeModel); ok {
		t.Error("探测成功后 A 对 probeModel 的冷却应被解除（否则探测毫无意义）")
	}
	// 只解除被探的那个号；B 的冷却不受影响（探测是逐 (账号, 模型) 的）
	if _, ok := modelCoolingOf(t, px, coolUIDB, probeModel); !ok {
		t.Error("B 的冷却不该被 A 的探测成功连带解除")
	}

	// ② 第二次请求走**常规**路径（A 已恢复）→ 直接成功，不需要再探
	resp2 := doChat(t, srv, raw, `{"model":"`+probeModel+`","messages":[{"role":"user","content":"hi"}]}`)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("A 恢复后第二次请求应走常规路径成功，got %d", resp2.StatusCode)
	}
	if got := stub.callCount(probeModel); got != 2 {
		t.Errorf("第二次请求应是一次常规调用（合计 2），got %d", got)
	}
}

// TestModelProbeDoesNotRepeatWithinInterval **最关键的安全断言**：
// 上游没有解除时，探测失败**不得**在 interval 内重复 —— 否则等待期变成对上游的轮番轰炸。
func TestModelProbeDoesNotRepeatWithinInterval(t *testing.T) {
	stub, _ := newStub()
	stub.fail429[probeModel] = 1 << 20 // 永远 429 + 6004：上游**没有**提前解除
	_, px, srv, raw := newCoolingApp(t, stub)
	coolAllForModel(t, px)

	body := `{"model":"` + probeModel + `","messages":[{"role":"user","content":"hi"}]}`

	// ① 第一次：探测一次 → 被拒 → 无号可用 → 非 200（503 语义）；上游恰好被调 1 次
	r1 := doChat(t, srv, raw, body)
	if r1.StatusCode == http.StatusOK {
		t.Fatal("上游仍在限流，不该有 200")
	}
	if got := stub.callCount(probeModel); got != 1 {
		t.Fatalf("第一次请求应恰好一次探测，got %d", got)
	}

	// ② 紧接着再发：池侧 interval 闸门 + 本请求级"一次探测"限制 → **零新增上游调用**
	r2 := doChat(t, srv, raw, body)
	if r2.StatusCode == http.StatusOK {
		t.Fatal("上游仍在限流，不该有 200")
	}
	if got := stub.callCount(probeModel); got != 1 {
		t.Fatalf("interval 内不得重复探测（出站强度边界），上游调用 got %d want 1", got)
	}

	// ③ 探测失败**不**解除冷却（只有成功才解除）
	for _, uid := range []string{coolUIDA, coolUIDB} {
		if _, ok := modelCoolingOf(t, px, uid, probeModel); !ok {
			t.Errorf("%s 的 6004 冷却不该因探测失败被解除", uid)
		}
	}
	// ④ 台账应留下"探过了，下次什么时候"（机制必须可见）
	row, ok := modelCoolingOf(t, px, coolUIDA, probeModel)
	if !ok {
		t.Fatal("A 的冷却条目应仍在")
	}
	if !row.Probeable || row.ProbedAt.IsZero() {
		t.Errorf("探测记录应可见（probeable/probed_at），got %+v", row)
	}
}

// TestModelProbeSkips11102NegativeCache 11102「该后端无此模型」不探测：
// 重试无意义，它的恢复路径是自己的 TTL 到期 / BlockModelClear。
func TestModelProbeSkips11102NegativeCache(t *testing.T) {
	stub, _ := newStub()
	_, px, srv, raw := newCoolingApp(t, stub)
	px.pool.BlockModelBackoff(coolUIDA, probeModel, "11102 model not found on this backend")
	px.pool.BlockModelBackoff(coolUIDB, probeModel, "11102 model not found on this backend")
	px.pool.SetModelProbe(time.Millisecond, time.Hour, time.Minute)

	resp := doChat(t, srv, raw, `{"model":"`+probeModel+`","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode == http.StatusOK {
		t.Fatal("两个号对该模型都是 11102 负缓存，不该有 200")
	}
	if got := stub.callCount(probeModel); got != 0 {
		t.Fatalf("11102 条目不得被探测（重试无意义），上游调用 got %d want 0", got)
	}
}

// TestModelProbeDisabledByZeroInterval 关停开关：interval=0 回到"严格等上游墙钟"。
func TestModelProbeDisabledByZeroInterval(t *testing.T) {
	stub, _ := newStub()
	_, px, srv, raw := newCoolingApp(t, stub)
	coolAllForModel(t, px)
	px.pool.SetModelProbe(time.Millisecond, 0, -1) // 关停

	resp := doChat(t, srv, raw, `{"model":"`+probeModel+`","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode == http.StatusOK {
		t.Fatal("关停后不该有 200（两个号都还在冷却）")
	}
	if got := stub.callCount(probeModel); got != 0 {
		t.Fatalf("关停后不得探测，上游调用 got %d want 0", got)
	}
}

// TestModelProbeNotUsedWhenHealthyAccountExists 有别的可用号时走常规路径：
// 探测名额**不该**被浪费（探测是"实在没别的办法"时的最后手段）。
func TestModelProbeNotUsedWhenHealthyAccountExists(t *testing.T) {
	stub, _ := newStub()
	_, px, srv, raw := newCoolingApp(t, stub)
	// 只有 A 对该模型冷却；B 完全可用
	px.pool.CooldownSoftForModel(coolUIDA, time.Minute, time.Now().Add(3*time.Hour), probeModel,
		"6004 model rate limit [reset=body-json]")
	px.pool.SetModelProbe(time.Millisecond, time.Hour, time.Minute)

	resp := doChat(t, srv, raw, `{"model":"`+probeModel+`","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("B 可用就该正常 200，got %d", resp.StatusCode)
	}
	if served := resp.Header.Get("X-Wb-Account"); served != coolUIDB {
		t.Fatalf("应由完全可用的 B 承接，got %q", served)
	}
	if got := stub.callCount(probeModel); got != 1 {
		t.Errorf("常规路径一次调用即可，got %d", got)
	}
	row, ok := modelCoolingOf(t, px, coolUIDA, probeModel)
	if !ok {
		t.Fatal("A 的冷却条目应仍在")
	}
	if !row.ProbedAt.IsZero() {
		t.Errorf("有可用号时不该消耗 A 的探测名额，got %+v", row)
	}
}
