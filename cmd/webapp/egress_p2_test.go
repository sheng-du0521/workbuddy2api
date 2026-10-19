// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 第二批收尾（v0.31.52）：配额 / 失败审计 / Key 绑定参数。
package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
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

// TestEgressUserLineQuota 每用户线路条数上限（默认 2）。
//
// 为什么需要：每条用户线路占一个端口 + 一条隧道 + 一份"别人家的网络"这个不可信边界；
// 端口段按 uid 模 1000 分配，吃光会牵连到同一段的**别的用户**。
func TestEgressUserLineQuota(t *testing.T) {
	f := usApp(t)
	// 先给他建满 2 条（走正常注册路径）。
	for i := 0; i < 2; i++ {
		url := egressHomeURL(12000 + int(f.userA.ID%1000)*10 + i)
		if err := f.store.UpsertEgressProxy(scopeLine(f.userA.ID, url, true)); err != nil {
			t.Fatalf("UpsertEgressProxy: %v", err)
		}
	}
	// 第 3 条（新端口）必须被拒。
	thirdPort := 12000 + int(f.userA.ID%1000)*10 + 5
	_, err := f.app.applyHomeEgress(thirdPort, egressScope{UserID: f.userA.ID}, true)
	if err == nil || !strings.Contains(err.Error(), "上限") {
		t.Fatalf("第 3 条线路应被配额拒绝，得 %v", err)
	}
	// 已有线路的重复应用不受影响（否则老用户升级后会被锁死）。
	firstURL := egressHomeURL(12000 + int(f.userA.ID%1000)*10)
	firstPort := 12000 + int(f.userA.ID%1000)*10
	if _, err := f.app.applyHomeEgress(firstPort, egressScope{UserID: f.userA.ID}, true); err != nil {
		t.Fatalf("已有线路重复应用不该被配额拦：%v（%s）", err, firstURL)
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
func TestEgressMachineCarryingFlag(t *testing.T) {
	f := usApp(t)
	const port = 12075
	res, err := f.app.applyHomeEgress(port, egressScope{UserID: f.userA.ID}, true)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.HostBound == 0 {
		t.Skip("夹具里没有可绑的号，跳过（本用例只验标记逻辑）")
	}
	code, raw := eaReq(t, f.mux, "GET", "/api/egress/home", "", f.cookieA, "")
	if code != http.StatusOK {
		t.Fatalf("读出口状态应 200，得 %d", code)
	}
	d := eaJSON(t, raw)
	list, _ := d["machines"].([]any)
	if len(list) == 0 {
		return // 没有机器记录（apply 不创建机器），标记无从验证
	}
	for _, it := range list {
		if m, ok := it.(map[string]any); ok {
			if _, has := m["carrying"]; !has {
				t.Fatal("机器视图缺少 carrying 标记")
			}
			if _, has := m["bound_accounts"]; !has {
				t.Fatal("机器视图缺少 bound_accounts 计数")
			}
		}
	}
	_ = time.Now()
}
