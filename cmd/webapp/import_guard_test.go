// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"testing"
	"time"

	"workbuddy2api/internal/egress"
	"workbuddy2api/internal/upstream"
)

// ===========================================================================
// v0.25.0 导入防关联门禁
//
// 只测**纯逻辑**：出口探测走可注入的 probeExitIP（打网络会让这批用例又慢又 flaky，
// 而门禁本身是一条会拦人的路径，必须能稳定地测）。
// ===========================================================================

// withExitIP 把出口探测替换成固定返回值，测试结束自动还原。
func withExitIP(t *testing.T, ip string, ok bool) {
	t.Helper()
	old := probeExitIP
	probeExitIP = func() egress.ProbeResult {
		return egress.ProbeResult{OK: ok, IP: ip, Error: map[bool]string{true: "", false: "test: 探测失败"}[ok]}
	}
	t.Cleanup(func() { probeExitIP = old })
}

func acc(uid string) desktopAccount { return desktopAccount{UID: uid, AccessToken: "tok"} }

// TestCountNewAccountsCountsOnlyNew 只统计**新增**：更新已存在的号不占额度。
//
// 这是这条门禁最容易做错的一处 —— 若把更新也计入，日常「重导一遍全量备份」
// 就会被挡住，运维最终只能把门禁关掉，保护反而消失。
func TestCountNewAccountsCountsOnlyNew(t *testing.T) {
	store, px, a := newTestApp(t, &Account{UID: "old1", AccessToken: "tok"})
	defer store.Close()
	defer px.Close()

	list := []desktopAccount{
		acc("old1"),              // 已存在 → 更新，不计
		acc("new1"), acc("new2"), // 新增 → 计 2
		acc("new1"),                   // 同一批里的重复 → 只计一次
		{UID: "", AccessToken: "tok"}, // 缺 uid → 不落库，不计
		{UID: "new3", AccessToken: "", RefreshToken: ""}, // 凭证全空 → 不落库，不计
	}
	if got := a.countNewAccounts(list); got != 2 {
		t.Fatalf("新增数应为 2（old1 是更新、重复与无效条目都不计），得 %d", got)
	}
}

// TestImportGuardMaxNew 超出单批上限必须拒绝，且**与 IP 门禁无关**。
func TestImportGuardMaxNew(t *testing.T) {
	store, px, a := newTestApp(t, &Account{UID: "x1", AccessToken: "tok"})
	defer store.Close()
	defer px.Close()
	withExitIP(t, "1.1.1.1", true)
	_ = store.SetKV(kvImportLastIP, "1.1.1.1") // 就算 IP 没变，数量超限也该先被拒

	v := a.checkImportGuard([]desktopAccount{acc("n1"), acc("n2"), acc("n3"), acc("n4")}, false)
	if v.OK {
		t.Fatal("新增 4 个超过默认上限 3，应被拒")
	}
	if v.NewCount != 4 || v.MaxNew != 3 {
		t.Fatalf("判定信息应回传 new_count=4 max_new=3，得 %d/%d", v.NewCount, v.MaxNew)
	}
	if v.Reason == "" || v.Hint == "" {
		t.Fatal("拒绝必须同时给出原因与**可执行的**指引（只说不行等于把问题丢回给人）")
	}

	// 关掉 IP 门禁不该顺带放掉数量上限（两条是独立纪律）。
	if err := store.SetKV(kvImportIPGate, "0"); err != nil {
		t.Fatal(err)
	}
	if v2 := a.checkImportGuard([]desktopAccount{acc("n1"), acc("n2"), acc("n3"), acc("n4")}, false); v2.OK {
		t.Fatal("IP 门禁关闭后，单批上限仍应生效")
	}
}

// TestImportGuardIPGate 同 IP 拒绝 / 换 IP 放行 / 首次导入放行 / 探测失败 fail-open。
func TestImportGuardIPGate(t *testing.T) {
	store, px, a := newTestApp(t, &Account{UID: "x1", AccessToken: "tok"})
	defer store.Close()
	defer px.Close()
	list := []desktopAccount{acc("n1")}

	// ① 首次导入（库里没有上次 IP）→ 放行。
	withExitIP(t, "1.1.1.1", true)
	if v := a.checkImportGuard(list, false); !v.OK {
		t.Fatalf("首次导入应放行，得 %+v", v)
	}

	// ② 上次导入也是 1.1.1.1 → 拒绝，且标记 need_ip。
	if err := store.SetKV(kvImportLastIP, "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	v := a.checkImportGuard(list, false)
	if v.OK || !v.NeedIP {
		t.Fatalf("同一 IP 应被拒并标记 need_ip，得 ok=%v need_ip=%v", v.OK, v.NeedIP)
	}
	if v.AutoReset {
		t.Fatal("未配置换 IP 命令时不该声称能自动换")
	}

	// ③ 出口 IP 变了 → 放行。
	withExitIP(t, "2.2.2.2", true)
	if v := a.checkImportGuard(list, false); !v.OK || !v.IPChanged {
		t.Fatalf("IP 已变化应放行，得 ok=%v ip_changed=%v", v.OK, v.IPChanged)
	}

	// ④ 探测失败 → **fail-open** 但如实标注未校验。
	// 理由：导入本身不产生出站，探测失败就拒绝会让「第三方回显端点挂了」变成
	// 「导入用不了」；这道门禁防的是"无意关联"，不是"对抗攻击者"。
	withExitIP(t, "", false)
	v4 := a.checkImportGuard(list, false)
	if !v4.OK || !v4.Unverified {
		t.Fatalf("探测失败应放行但标注 unverified，得 ok=%v unverified=%v", v4.OK, v4.Unverified)
	}
}

// TestImportGuardNoNewPassesSameIP **本次没有新增**（全是已存在账号的 token 刷新）时，
// 即使 IP 与上次导入相同也必须放行。
//
// 这条是 v0.25.0 全量测试抓出来的真实缺陷：初版实现只看"IP 是否相同"，
// 于是「重导一遍全量备份」会被门禁挡住 —— 那种挫败感会直接导致运维把门禁整个关掉，
// 保护反而消失。门禁的目的是切断**新号**的关联，不是禁止一切重复导入。
func TestImportGuardNoNewPassesSameIP(t *testing.T) {
	store, px, a := newTestApp(t, &Account{UID: "exist1", AccessToken: "tok"})
	defer store.Close()
	defer px.Close()
	withExitIP(t, "1.1.1.1", true)
	_ = store.SetKV(kvImportLastIP, "1.1.1.1")

	// 全是已存在的 uid → 新增 0。
	v := a.checkImportGuard([]desktopAccount{acc("exist1")}, false)
	if !v.OK {
		t.Fatalf("无新增的导入不该被 IP 门禁拦住，得 %+v", v)
	}
	if v.NewCount != 0 {
		t.Fatalf("新增数应为 0，得 %d", v.NewCount)
	}
	if v.Hint == "" {
		t.Fatal("应明确告知本次没有新增、无需换 IP，否则运维会以为被静默放行")
	}

	// 预检（expectNew=true）必须仍判"需要换 IP"：用户马上要粘一份进来，
	// 若预检说"无需换 IP"、粘完却被拒，就是典型的"粘完才被拒"体验。
	v2 := a.checkImportGuard(nil, true)
	if v2.OK || !v2.NeedIP {
		t.Fatalf("预检应按将有新增判定为需要换 IP，得 ok=%v need_ip=%v", v2.OK, v2.NeedIP)
	}
}

// TestImportGuardGateOff 关掉门禁后：IP 维度不再拦人（数量维度仍然生效，另测）。
func TestImportGuardGateOff(t *testing.T) {
	store, px, a := newTestApp(t, &Account{UID: "x1", AccessToken: "tok"})
	defer store.Close()
	defer px.Close()
	withExitIP(t, "1.1.1.1", true)
	_ = store.SetKV(kvImportLastIP, "1.1.1.1")
	_ = store.SetKV(kvImportIPGate, "0")

	v := a.checkImportGuard([]desktopAccount{acc("n1")}, false)
	if !v.OK || v.Enabled {
		t.Fatalf("门禁关闭时应放行且 Enabled=false，得 ok=%v enabled=%v", v.OK, v.Enabled)
	}
	if v.Hint == "" {
		t.Fatal("关闭门禁时应明确告知（否则运维以为还在保护）")
	}
}

// TestImportBatchLedger 台账：新的在前、条数有上限、更新时间与判据。
func TestImportBatchLedger(t *testing.T) {
	store, px, a := newTestApp(t, &Account{UID: "x1", AccessToken: "tok"})
	defer store.Close()
	defer px.Close()

	for i := 0; i < importBatchesKeep+5; i++ {
		a.recordImportBatch("json", "9.9.9."+string(rune('0'+i%10)), 2, 1, []string{"u1"})
		time.Sleep(time.Millisecond)
	}
	batches, err := a.store.GetKV(kvImportBatches)
	if err != nil || batches == "" {
		t.Fatalf("台账应被写入，err=%v", err)
	}
	// 判据（上次导入 IP）应等于最后一次记录的值 —— 它是门禁的比较对象。
	last, _ := a.store.GetKV(kvImportLastIP)
	if last == "" {
		t.Fatal("门禁判据 import_last_ip 必须被更新")
	}
	if at, _ := a.store.GetKV(kvImportLastAt); at == "" {
		t.Fatal("import_last_at 必须被更新")
	}
}

// TestCreditAutoRefreshInterval 补新间隔的默认值/边界/垃圾输入。
//
// 它直接决定出站频率 —— 配错了就是风控事故，所以这类"默认值逻辑"必须钉住。
func TestCreditAutoRefreshInterval(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", creditRefreshSecDef}, // 未配置 → 默认 5
		{"5", 5},
		{"0", 0},                      // 显式关闭
		{"-3", 0},                     // 负数 = 关闭（不解释成"3 秒"）
		{"abc", creditRefreshSecDef},  // 垃圾输入 → 回落默认，绝不变成 0.001 秒
		{"9999", creditRefreshSecMax}, // 超上限 → 夹到上限
	}
	for _, c := range cases {
		if got := creditAutoRefreshInterval(c.in); got != c.want {
			t.Errorf("creditAutoRefreshInterval(%q)=%d want %d", c.in, got, c.want)
		}
	}
}

// TestCachedRemainExcludesExpired 缓存合计必须排除已过期的包。
func TestCachedRemainExcludesExpired(t *testing.T) {
	now := time.Now().Unix()
	cp := cachedCreditPacks{Packs: []upstream.CreditPackage{
		{Remain: 100, ExpireAt: now + 86400}, // 未过期
		{Remain: 50, ExpireAt: now - 86400},  // 已过期 → 不计
		{Remain: 30, ExpireAt: 0},            // 无到期 → 长期有效，计
		{Remain: 0, ExpireAt: now + 86400},   // 无剩余 → 计 0
	}}
	if got := cachedRemain(cp, now); got != 130 {
		t.Fatalf("可用合计应为 130（排除已过期 50），得 %d", got)
	}
}
