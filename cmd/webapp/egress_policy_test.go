// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import "testing"

// TestEgressOnLineDownDefaultsToStrict 默认必须是 strict（不静默换出口）。
//
// 这条默认值本身就是产品决定，值得用测试钉住：它决定"线路挂了以后，
// 是程序自己悄悄把账号换到别的出口，还是停下来喊人"。
func TestEgressOnLineDownDefaultsToStrict(t *testing.T) {
	store, px, a := newTestApp(t, &Account{UID: "d-1", AccessToken: "t"})
	defer store.Close()
	defer px.Close()

	if got := a.egressOnLineDownMode(); got != egressOnLineDownStrict {
		t.Fatalf("默认策略应为 strict（不静默换出口），实得 %q", got)
	}
	if a.egressAllowAutoRehome() {
		t.Fatal("默认下不应允许自动迁移")
	}
}

// TestEgressOnLineDownInvalidFallsBackToStrict 非法值一律回落 strict。
//
// 错判代价不对称：误判成 strict 只是"没自动搬、喊人处理"（可恢复）；
// 误判成 migrate 会在无人知晓时改掉账号出口（不可撤回的特征变化）。
// 保守方向必须选**可逆**的那一侧。
func TestEgressOnLineDownInvalidFallsBackToStrict(t *testing.T) {
	store, px, a := newTestApp(t, &Account{UID: "d-2", AccessToken: "t"})
	defer store.Close()
	defer px.Close()

	for _, bad := range []string{"MIGRATE ", "auto", "1", "strict ", "yes"} {
		m, ok := normaliseEgressOnLineDown(bad)
		if ok && m != egressOnLineDownMigrate && m != egressOnLineDownStrict {
			t.Fatalf("normalise(%q) 返回了非法值 %q", bad, m)
		}
	}
	// 大小写与空白应被容忍。
	if m, ok := normaliseEgressOnLineDown("  Migrate  "); !ok || m != egressOnLineDownMigrate {
		t.Fatalf("Migrate 应被接受并归一为 migrate，实得 (%q,%v)", m, ok)
	}
	// 库里存了脏值 → 读出来必须回落 strict。
	if err := store.SetKV(kvEgressOnLineDown, "whatever"); err != nil {
		t.Fatal(err)
	}
	if got := a.egressOnLineDownMode(); got != egressOnLineDownStrict {
		t.Fatalf("脏值应回落 strict，实得 %q", got)
	}
}

// TestRehomeStrictBlocksSilentExitChange strict 下坏线路上的账号**刻意不迁移**。
//
// 这是本策略的全部意义：宁可不可服务，也不让账号的出口 IP 在无人知晓时变化。
// 同时必须保证它**没有偷偷改成直连**（那会从本机 IP 裸奔，是更严重的问题）。
func TestRehomeStrictBlocksSilentExitChange(t *testing.T) {
	store, px, a := newTestApp(t,
		&Account{UID: "s-1", AccessToken: "t"},
		&Account{UID: "s-2", AccessToken: "t"})
	defer store.Close()
	defer px.Close()
	// 默认即 strict，无需设置 —— 这里刻意不设，验证"开箱就是安全的"。

	dead := "http://10.0.0.1:8080"
	alive := "http://10.0.0.2:8080"
	addProxy(t, store, "p-dead", dead, true, false, egressFailThreshold)
	addProxy(t, store, "p-alive", alive, true, true, 0)
	for _, uid := range []string{"s-1", "s-2"} {
		if err := store.SetAccountEgress(uid, dead); err != nil {
			t.Fatal(err)
		}
	}

	rep := a.rehomeAccountsFromDeadProxies()
	if rep.Mode != egressOnLineDownStrict {
		t.Fatalf("报告里的 mode 应为 strict，实得 %q", rep.Mode)
	}
	if rep.Moved != 0 {
		t.Fatalf("strict 下不该迁移任何账号，实得 moved=%d", rep.Moved)
	}
	if rep.Blocked != 2 {
		t.Fatalf("应有 2 个账号被策略拦下，实得 %d", rep.Blocked)
	}
	if len(rep.Warnings) == 0 {
		t.Fatal("必须给出人话告警，否则运维不知道发生了什么")
	}
	// 关键：绑定没被改动 —— 既没换线路，也没被改成直连。
	for _, uid := range []string{"s-1", "s-2"} {
		acc, err := store.GetAccount(uid)
		if err != nil || acc == nil {
			t.Fatalf("GetAccount(%s): %v", uid, err)
		}
		if acc.EgressProxy != dead {
			t.Fatalf("%s 的绑定被改动了：%q（strict 下必须保持原线路，"+
				"且绝不能变成直连）", uid, acc.EgressProxy)
		}
	}
}

// TestRehomeMigrateStillWorks migrate 模式下迁移算法照常（改设置即可回到旧行为）。
func TestRehomeMigrateStillWorks(t *testing.T) {
	store, px, a := newTestApp(t, &Account{UID: "m-1", AccessToken: "t"})
	defer store.Close()
	defer px.Close()
	setEgressMode(t, store, egressOnLineDownMigrate)

	dead := "http://10.0.0.1:8080"
	alive := "http://10.0.0.2:8080"
	addProxy(t, store, "p-dead", dead, true, false, egressFailThreshold)
	addProxy(t, store, "p-alive", alive, true, true, 0)
	if err := store.SetAccountEgress("m-1", dead); err != nil {
		t.Fatal(err)
	}

	rep := a.rehomeAccountsFromDeadProxies()
	if rep.Mode != egressOnLineDownMigrate {
		t.Fatalf("mode 应为 migrate，实得 %q", rep.Mode)
	}
	if rep.Moved != 1 || rep.Blocked != 0 {
		t.Fatalf("migrate 下应迁走 1 个、拦截 0 个，实得 moved=%d blocked=%d", rep.Moved, rep.Blocked)
	}
	acc, _ := store.GetAccount("m-1")
	if acc.EgressProxy != alive {
		t.Fatalf("账号应被迁到活线路，实得 %q", acc.EgressProxy)
	}
}
