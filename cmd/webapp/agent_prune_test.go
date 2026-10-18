// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"testing"
	"time"
)

// TestPruneStaleMachinesRemovesFingerprintlessDup 同一台机器升级后留下的无指纹老记录必须清掉。
//
// 场景（2026-10-18 实测）：0.1.0 的 agent 不带 machine id / 指纹，登记成 `default`；
// 升到 0.3.x 后带指纹，又登记成 `m-xxxx`。两条 hostname 相同、端口不同 —— 页面上
// 就是"多了一台机器"，而老那条永远不会再上报（v0.31.31 修掉的错觉又回来了）。
func TestPruneStaleMachinesRemovesFingerprintlessDup(t *testing.T) {
	now := time.Now().Unix()
	m := map[string]*egressMachine{
		// 老记录：无指纹、一天多没上报、同 hostname
		"default": {ID: "default", Hostname: "PC-A", StateAt: now - 2*staleMachineAge},
		// 新记录：有指纹、刚上报
		"m-aaaa": {ID: "m-aaaa", Hostname: "PC-A", Fingerprint: "fp1", StateAt: now},
	}
	got := pruneStaleMachines(m, now, 0)
	if len(got) != 1 || got[0] != "default" {
		t.Fatalf("应只清掉 default，实得 %v", got)
	}
	if _, ok := m["default"]; ok {
		t.Fatal("default 未被删除")
	}
	if _, ok := m["m-aaaa"]; !ok {
		t.Fatal("新记录不能被动")
	}
}

// TestPruneStaleMachinesKeepsWhenNotEnoughEvidence 证据不足时一条都不许删。
//
// 每一条都是"删了就丢真实机器"的风险，所以逐条钉住：缺哪条就保留。
func TestPruneStaleMachinesKeepsWhenNotEnoughEvidence(t *testing.T) {
	now := time.Now().Unix()

	cases := []struct {
		name string
		m    map[string]*egressMachine
		why  string
	}{
		{
			name: "无同机新记录",
			m: map[string]*egressMachine{
				"default": {ID: "default", Hostname: "PC-A", StateAt: now - 2*staleMachineAge},
			},
			why: "没有指纹记录可比对，无从判断它是幽灵还是唯一一台",
		},
		{
			name: "hostname 不同",
			m: map[string]*egressMachine{
				"default": {ID: "default", Hostname: "PC-A", StateAt: now - 2*staleMachineAge},
				"m-aaaa":  {ID: "m-aaaa", Hostname: "PC-B", Fingerprint: "fp1", StateAt: now},
			},
			why: "另一台电脑，删掉就是丢机器",
		},
		{
			name: "hostname 为空",
			m: map[string]*egressMachine{
				"default": {ID: "default", Hostname: "", StateAt: now - 2*staleMachineAge},
				"m-aaaa":  {ID: "m-aaaa", Hostname: "", Fingerprint: "fp1", StateAt: now},
			},
			why: "空 hostname 无法证明是同一台",
		},
		{
			name: "新记录也停止了上报",
			m: map[string]*egressMachine{
				"default": {ID: "default", Hostname: "PC-A", StateAt: now - 5*staleMachineAge},
				"m-aaaa":  {ID: "m-aaaa", Hostname: "PC-A", Fingerprint: "fp1", StateAt: now - 2*staleMachineAge},
			},
			why: "两台都停了，说明这台机器整体离线，不是升级留下的重复",
		},
		{
			name: "老记录还很新",
			m: map[string]*egressMachine{
				"default": {ID: "default", Hostname: "PC-A", StateAt: now - 60},
				"m-aaaa":  {ID: "m-aaaa", Hostname: "PC-A", Fingerprint: "fp1", StateAt: now},
			},
			why: "刚上报过，可能是并存的另一份 agent",
		},
		{
			name: "老记录有指纹",
			m: map[string]*egressMachine{
				"m-old":  {ID: "m-old", Hostname: "PC-A", Fingerprint: "fp2", StateAt: now - 2*staleMachineAge},
				"m-aaaa": {ID: "m-aaaa", Hostname: "PC-A", Fingerprint: "fp1", StateAt: now},
			},
			why: "有指纹就是另一台确凿的机器（v0.31.31 的红线）",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := pruneStaleMachines(c.m, now, 0)
			if len(got) != 0 {
				t.Fatalf("%s —— 不该删，实得 %v", c.why, got)
			}
		})
	}
}

// TestPruneStaleMachinesOwnerScoped 跨 owner 的同名 hostname 不能动。
//
// 同一台电脑上"平台 agent"与"用户 agent"并存是正常配置（端口不同、令牌不同）。
// 拿一个 owner 的上报去删另一个 owner 的记录，等于把别人的线路静默摘掉。
func TestPruneStaleMachinesOwnerScoped(t *testing.T) {
	now := time.Now().Unix()
	m := map[string]*egressMachine{
		// 用户 9916 的旧记录（无指纹、很老），与平台机器同 hostname
		"u9916:m-old": {ID: "m-old", Hostname: "PC-A", OwnerUserID: 9916, StateAt: now - 3*staleMachineAge},
		// 平台的新记录
		"m-aaaa": {ID: "m-aaaa", Hostname: "PC-A", Fingerprint: "fp1", StateAt: now},
	}
	if got := pruneStaleMachines(m, now, 0); len(got) != 0 {
		t.Fatalf("平台视角不该清理用户的记录，实得 %v", got)
	}
	// 用户视角：它自己没有"有指纹且活跃"的记录 → 也不该删
	if got := pruneStaleMachines(m, now, 9916); len(got) != 0 {
		t.Fatalf("用户视角缺少有指纹记录，不该清理，实得 %v", got)
	}
}
