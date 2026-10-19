// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"testing"
	"time"
)

// qrFixture 造一个"用户代挂号"场景（扫码/导入落库后的那一行）。
func qrFixture(t *testing.T) (*Store, *app, *Account) {
	t.Helper()
	acc := &Account{
		UID: "u-qr-test", Nickname: "扫码号", Realm: "cn",
		AccessToken: "at", RefreshToken: "rt",
		ExpiresAt:   time.Now().Add(24 * time.Hour).Unix(),
		OwnerUserID: 9915,
		PoolState:   "", // 落库默认：未申请
	}
	store, _, a := newTestApp(t, acc)
	return store, a, acc
}

// TestQRDoesNotAutoPoolByDefault 扫码默认**不得**入池。
//
// 这是产品规则，不是偏好：入池 = 这个号开始接平台的反代流量（消耗号主积分）。
// 闸门必须在号主「申请」+ 管理员「批准」两道之后。
//
// v0.31.37 曾把它改成默认开（误读需求），结果扫码即入池；v0.31.46 回归。
// 所以这条测试是**回归守卫** —— 谁再把默认值改回开，这里会红。
func TestQRDoesNotAutoPoolByDefault(t *testing.T) {
	store, a, acc := qrFixture(t)
	defer store.Close()

	if _, err := a.materializeQRLogin(acc); err != nil {
		t.Fatalf("materializeQRLogin: %v", err)
	}
	cur, err := store.GetAccount(acc.UID)
	if err != nil || cur == nil {
		t.Fatalf("GetAccount: %v", err)
	}
	if cur.PoolState != "" {
		t.Fatalf("扫码后默认不应入池（应停在未申请），实得 pool_state=%q", cur.PoolState)
	}
	// 但来源与设备凭证仍必须写好（这三件事互不相关）。
	if s := store.SourceMap()[acc.UID]; s != sourceQR {
		t.Fatalf("来源应记为 qr，实得 %q", s)
	}
	if cur.DeviceToken == "" {
		t.Fatal("设备凭证应显式落库")
	}
}

// TestQRAutoPoolOnlyWhenExplicitlyEnabled 只有 kv 显式开启才自动入池。
func TestQRAutoPoolOnlyWhenExplicitlyEnabled(t *testing.T) {
	store, a, acc := qrFixture(t)
	defer store.Close()

	if err := store.SetKV(qrAutoPoolKey, "1"); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	if _, err := a.materializeQRLogin(acc); err != nil {
		t.Fatalf("materializeQRLogin: %v", err)
	}
	cur, _ := store.GetAccount(acc.UID)
	if cur == nil || cur.PoolState != "in" {
		t.Fatalf("kv 显式开启后应入池，实得 %+v", cur)
	}
}

// TestQRAutoPoolRejectsFalsyValues 只有 1/true/on/yes 算开；其余（含 "0"、"false"、垃圾值）一律关。
//
// 为什么把"垃圾值"也归到关：这个开关一旦误开，号主的积分会被平台流量消耗掉 ——
// 失败方向必须是"不入池"。
func TestQRAutoPoolRejectsFalsyValues(t *testing.T) {
	for _, v := range []string{"", "0", "false", "off", "no", "随便什么", " "} {
		store, a, acc := qrFixture(t)
		if err := store.SetKV(qrAutoPoolKey, v); err != nil {
			t.Fatalf("SetKV: %v", err)
		}
		if _, err := a.materializeQRLogin(acc); err != nil {
			t.Fatalf("materializeQRLogin: %v", err)
		}
		cur, _ := store.GetAccount(acc.UID)
		if cur != nil && cur.PoolState != "" {
			t.Fatalf("kv=%q 不应开启自动入池，实得 pool_state=%q", v, cur.PoolState)
		}
		store.Close()
	}
}

// TestPruneKeepsFreshPollCreatedMachine poll 刚建出来的记录不能被当成"陈旧"删掉。
//
// 现场（2026-10-19）：poll 会经 egressRegister 创建记录但**不写 StateAt**，
// 于是 StateAt=0 → `now-0` 被判成"老于一天" → 每 5 秒 poll 建一次、prune 删一次，
// 无限循环，16 小时刷了上万条日志，把要查的扫码日志整段淹掉。
func TestPruneKeepsFreshPollCreatedMachine(t *testing.T) {
	now := time.Now().Unix()
	m := map[string]*egressMachine{
		// poll 刚创建（无指纹、无 state、CreatedAt=now）
		"default": {ID: "default", Hostname: "PC-A", CreatedAt: now},
		// 同机的新记录（有指纹、刚上报）
		"m-aaaa": {ID: "m-aaaa", Hostname: "PC-A", Fingerprint: "fp1", StateAt: now, CreatedAt: now},
	}
	if got := pruneStaleMachines(m, now, 0); len(got) != 0 {
		t.Fatalf("刚创建的 poll 记录不该被清理，实得 %v", got)
	}
}

// TestPruneKeepsRecordWithNoTimestamps 连 CreatedAt 都没有时，宁可保留。
//
// "多一条幽灵机器"的代价是页面多一行；"删掉一台真机器"的代价是那条出口线路消失。
func TestPruneKeepsRecordWithNoTimestamps(t *testing.T) {
	now := time.Now().Unix()
	m := map[string]*egressMachine{
		"weird":  {ID: "weird", Hostname: "PC-A"}, // 两个时间都是 0
		"m-aaaa": {ID: "m-aaaa", Hostname: "PC-A", Fingerprint: "fp1", StateAt: now, CreatedAt: now},
	}
	if got := pruneStaleMachines(m, now, 0); len(got) != 0 {
		t.Fatalf("无法判断年龄时应保留，实得 %v", got)
	}
}

// TestPruneStillRemovesOldFingerprintlessRecord 真正的老记录（CreatedAt 很早）仍要清。
//
// 防止上一条修得过头：升级前留下的 default（CreatedAt 是几天前）必须被清，
// 否则"多一台机器"的错觉又会回来。
func TestPruneStillRemovesOldFingerprintlessRecord(t *testing.T) {
	now := time.Now().Unix()
	m := map[string]*egressMachine{
		"m-old":  {ID: "m-old", Hostname: "PC-A", CreatedAt: now - 3*staleMachineAge},
		"m-aaaa": {ID: "m-aaaa", Hostname: "PC-A", Fingerprint: "fp1", StateAt: now, CreatedAt: now},
	}
	got := pruneStaleMachines(m, now, 0)
	if len(got) != 1 || got[0] != "m-old" {
		t.Fatalf("应清掉同机位的无指纹老记录，实得 %v", got)
	}
}

// TestPruneNeverTouchesDefaultBucket default 桶**永远不清理**。
//
// 为什么单独钉一条：default 是"没报自己是谁"的老 agent 的合法落点，而这类 agent
// 只 poll、poll 不写时间戳 —— 按时间判它一定"很老"，删掉后下一次 poll 立刻重建，
// 就是每 5 秒删一次建一次的无限循环（生产实测 16 小时刷了上万条日志）。
// 升级残留的幽灵 default 由 machineEnsure 的改名逻辑处理，不归这里。
func TestPruneNeverTouchesDefaultBucket(t *testing.T) {
	now := time.Now().Unix()
	m := map[string]*egressMachine{
		// 极老的 default，且同 hostname 有新记录 —— 依然不删。
		"default": {ID: "default", Hostname: "PC-A", CreatedAt: now - 30*staleMachineAge},
		"m-aaaa":  {ID: "m-aaaa", Hostname: "PC-A", Fingerprint: "fp1", StateAt: now, CreatedAt: now},
	}
	if got := pruneStaleMachines(m, now, 0); len(got) != 0 {
		t.Fatalf("default 桶不该被清理，实得 %v", got)
	}
	if _, ok := m["default"]; !ok {
		t.Fatal("default 记录被删了 —— 老 agent 会每 5 秒重建一次")
	}
}

// TestPruneRemovesGhostDefaultWithState 上报过状态的 default 幽灵要清，但只 poll 的不清。
//
// 两种 default 长得一样，区别在 StateAt：
//   - 老 agent 只 poll、不带 state → StateAt=0 → 按时间判一定"很老"，
//     删了立刻被下一次 poll 重建（v0.31.46 的无限循环）。绝不能碰。
//   - 升级前的旧身份会上报过 state → StateAt>0，且同机已有带指纹的新记录 →
//     它不会再上报，就是幽灵，不清就会让页面显示"两台机器"。
func TestPruneRemovesGhostDefaultWithState(t *testing.T) {
	now := time.Now().Unix()

	// 情形一：default 有 StateAt、超期、同机有活跃新记录 → 清。
	m := map[string]*egressMachine{
		"default": {ID: "default", Hostname: "PC-A", StateAt: now - 3*staleMachineAge},
		"m-new":   {ID: "m-new", Hostname: "PC-A", Fingerprint: "fp1", StateAt: now},
	}
	if got := pruneStaleMachines(m, now, 0); len(got) != 1 || got[0] != "default" {
		t.Fatalf("应清掉幽灵 default，实得 %v", got)
	}

	// 情形二：default 只有 CreatedAt（老 agent 只 poll）→ 永不清。
	m2 := map[string]*egressMachine{
		"default": {ID: "default", Hostname: "PC-A", CreatedAt: now - 30*staleMachineAge},
		"m-new":   {ID: "m-new", Hostname: "PC-A", Fingerprint: "fp1", StateAt: now},
	}
	if got := pruneStaleMachines(m2, now, 0); len(got) != 0 {
		t.Fatalf("只 poll 的 default 不该被清（会无限删建），实得 %v", got)
	}

	// 情形三：default 有 StateAt 但**没有**同机新记录 → 不清（它可能就是唯一的那台）。
	m3 := map[string]*egressMachine{
		"default": {ID: "default", Hostname: "PC-B", StateAt: now - 3*staleMachineAge},
	}
	if got := pruneStaleMachines(m3, now, 0); len(got) != 0 {
		t.Fatalf("没有替代记录时不该清，实得 %v", got)
	}
}
