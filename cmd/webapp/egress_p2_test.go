// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 第二批收尾（v0.31.52）：配额 / 失败审计 / Key 绑定参数。
package main

import (
	"net/http"
	"strings"
	"testing"
)

// TestCreateKeyAccountUIDsRequiresHost 传了 account_uids 却没传 kind=host → 400。
//
// 现场（2026-10-19 端到端验证）：直接调 API 建 Key 时传了 account_uids 但没带 kind，
// 绑定被**静默丢弃**，Key 变成平台计费 Key —— 请求走平台号池、从余额扣点，
// 而调用方以为自己建的是"只用自己代挂号"的自用 Key。
func TestCreateKeyAccountUIDsRequiresHost(t *testing.T) {
	f := usApp(t)
	code, raw := eaReq(t, f.mux, "POST", "/api/keys",
		`{"name":"e2e","account_uids":["us-a-acct"]}`, f.cookieA, "")
	if code != http.StatusBadRequest || !strings.Contains(raw, "account_uids_requires_host") {
		t.Fatalf("应 400 account_uids_requires_host，得 %d %s", code, raw)
	}
	// 带上 kind=host 就应成功，且绑定生效。
	code, raw = eaReq(t, f.mux, "POST", "/api/keys",
		`{"name":"e2e2","kind":"host","account_uids":["us-a-acct"]}`, f.cookieA, "")
	if code != http.StatusOK {
		t.Fatalf("kind=host 应 200，得 %d %s", code, raw)
	}
	d := eaJSON(t, raw)
	k, _ := d["key"].(map[string]any)
	uids, _ := k["account_uids"].([]any)
	if len(uids) != 1 || uids[0] != "us-a-acct" {
		t.Fatalf("绑定应生效，得 %v", k["account_uids"])
	}
}

// TestForceOnDisabledLineWritesFailedAudit 强制越界打到停用线路 → 拒绝 + failed 审计。
//
// 这条覆盖的是"越权尝试失败也要留痕"：否则"我明明强制了"和"一个号都没改"
// 之间没有任何可查的东西。
func TestForceOnDisabledLineWritesFailedAudit(t *testing.T) {
	store, _, mux, adminCookie, _, memberID := ownerApp(t)
	url := "http://203.0.113.9:3128"
	line := scopeLine(memberID, url, false) // 停用
	line.ID = "p-off"
	if err := store.UpsertEgressProxy(line); err != nil {
		t.Fatalf("UpsertEgressProxy: %v", err)
	}

	code, raw := eaReq(t, mux, "POST", "/api/admin/egress/assign",
		`{"uids":["ow-acct"],"proxy_url":"`+url+`","force_scope":true,"force_reason":"排障演练：验证停用线路不可越界"}`,
		adminCookie, "")
	if code != http.StatusBadRequest {
		t.Fatalf("停用线路应 400，得 %d %s", code, raw)
	}
	audits, err := store.ListEgressForceAudits(5, "p-off")
	if err != nil {
		t.Fatalf("ListEgressForceAudits: %v", err)
	}
	if len(audits) != 1 || audits[0].Result != "failed" {
		t.Fatalf("应落 1 条 failed 审计，得 %+v", audits)
	}
	if audits[0].Reason == "" || len(audits[0].UIDs) != 1 {
		t.Fatalf("审计要含理由与目标号，得 %+v", audits[0])
	}
	// 账号一个字都不许改。
	if acc, _ := store.GetAccount("ow-acct"); acc != nil && strings.TrimSpace(acc.EgressProxy) != "" {
		t.Fatalf("被拒绝的强制不该改账号，得 %q", acc.EgressProxy)
	}
}

// TestEgressMachineCarryingFlag 视图要标出"哪台在承重"（挂了号的才算）。
//
// v0.31.53 起 apply 不再绑号，所以这里显式模拟**管理端的分配结果**：
// 先把号绑到线路上，再看视图是否把该机器标成 carrying。
func TestEgressMachineCarryingFlag(t *testing.T) {
	f := usApp(t)
	f.app.egressVerify = func(string) (string, error) { return "198.51.100.11", nil }
	tokP, _ := f.app.egressAgentTokenGet()
	usPoll(t, f, tokP, "", "host-plat")
	mm := f.app.machinesLoad()[egressDefaultMachine]
	if mm == nil {
		t.Fatal("平台机器应落 default 桶")
	}
	port := mm.Ports.TunnelPort
	if _, err := f.app.applyHomeEgress(port, egressScope{Platform: true}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// 管理端分配（真实链路的简化）：把两个代挂号绑到这条线路上。
	url := egressHomeURL(port)
	for _, uid := range []string{"us-a-acct", "us-b-acct"} {
		if err := f.store.SetAccountEgress(uid, url); err != nil {
			t.Fatalf("SetAccountEgress %s: %v", uid, err)
		}
	}
	_ = f.app.proxy.SyncAccounts()

	code, raw := eaReq(t, f.mux, "GET", "/api/egress/home", "", f.adminCookie, "")
	if code != http.StatusOK {
		t.Fatalf("管理员读出口状态应 200，得 %d %s", code, raw)
	}
	d := eaJSON(t, raw)
	list, _ := d["machines"].([]any)
	if len(list) == 0 {
		t.Fatal("应至少有一台平台机器")
	}
	found := false
	for _, it := range list {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if _, has := m["carrying"]; !has {
			t.Fatal("机器视图缺少 carrying 标记")
		}
		if _, has := m["bound_accounts"]; !has {
			t.Fatal("机器视图缺少 bound_accounts 计数")
		}
		if m["id"] == egressDefaultMachine {
			found = true
			if m["carrying"] != true {
				t.Fatalf("挂了 2 个号应标 carrying=true，得 %+v", m)
			}
			if n, _ := m["bound_accounts"].(float64); int(n) < 2 {
				t.Fatalf("bound_accounts 应 >= 2，得 %v", m["bound_accounts"])
			}
		}
	}
	if !found {
		t.Fatalf("视图里找不到 default 机器：%v", usKeysOf(f.app.machinesLoad()))
	}
}
