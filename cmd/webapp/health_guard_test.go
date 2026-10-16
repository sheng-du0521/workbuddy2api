// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 健康检测的两条护栏（v0.31.20，来自一次"代挂号 vs 平台号 机制一致性"审查）。
//
// 审查结论：三套机制（模型级冷却 / 账号级软冷却 / 风控摘号）在池层与 upstream 层
// **都没有 owner 分支**，代挂号与平台号走的是同一套代码与阈值 —— 这是好消息。
// 但审查同时发现两处**链路间**漂移，本文件把它们钉住：
//
//	① 同一个 11140，转发链用 2h 软冷却（accountFaultCooldown），健康检测链却用 60s
//	   （softCooldown）→ 体检几分钟内重复触发 3 次就能凑满阈值摘掉健康号，
//	   正是"同一错误码两套纪律"那条历史教训的形态；
//	② 未入池的代挂号（owner>0 且 pool_state != "in"）**不参与平台流量**，却照样被
//	   探活并按阈值停用、甚至不可逆删除 —— 对用户资产的越权处置，且与
//	   "入池要号主同意"（invited 态）直接冲突。
package main

import (
	"testing"
	"time"
)

func hasAct(acts []string, want string) bool {
	for _, x := range acts {
		if x == want {
			return true
		}
	}
	return false
}

// TestHealth11140UsesAccountFaultCooldown 体检链路的 11140 软冷却必须与转发链同源（≥1h）。
//
// 这条断言用**时长**而不是"用了哪个常量"来钉：常量改名/换实现都不该让它变绿。
// 60s 口径会让它失败 —— 那正是要防的回归。
func TestHealth11140UsesAccountFaultCooldown(t *testing.T) {
	store, px, a := newTestApp(t,
		&Account{UID: "hc-plat", Realm: "cn", AccessToken: "t"},
	)
	defer store.Close()
	_ = px.SyncAccounts()

	acts := a.applyHealthDisposition("hc-plat", hcCodeRequestIllegal, nil, false)
	if !hasAct(acts, hcActCooled) {
		t.Fatalf("11140 未达阈值应写入软冷却，得 %v", acts)
	}
	st, ok := px.pool.Status("hc-plat")
	if !ok {
		t.Fatal("取不到池状态")
	}
	if st.Until.Before(time.Now().Add(time.Hour)) {
		t.Fatalf("体检链路的 11140 软冷却应 ≥1h（与转发链同源 2h），得 %s —— "+
			"60s 口径下一个风控窗口内 3 次白撞就能摘掉健康号", st.Until)
	}
}

// TestHealthGuardSkipsUnpooledHosted 未入池的代挂号：只报告，不处置。
func TestHealthGuardSkipsUnpooledHosted(t *testing.T) {
	store, px, a := newTestApp(t,
		// 用户挂了但没入池（四种未入池形态取两种代表）
		&Account{UID: "h-none", Realm: "cn", AccessToken: "t", OwnerUserID: 7, PoolState: ""},
		&Account{UID: "h-invited", Realm: "cn", AccessToken: "t", OwnerUserID: 7, PoolState: "invited"},
		// 对照组：已入池的代挂号 / 平台自持号
		&Account{UID: "h-in", Realm: "cn", AccessToken: "t", OwnerUserID: 7, PoolState: "in"},
		&Account{UID: "plat", Realm: "cn", AccessToken: "t"},
	)
	defer store.Close()
	_ = px.SyncAccounts()

	// 三种「会摘号」的错误码，对未入池代挂号一律只报告。
	for _, uid := range []string{"h-none", "h-invited"} {
		for _, code := range []string{hcCodeSessionDead, hcCodeRequestIllegal, hcCodeRestricted} {
			acts := a.applyHealthDisposition(uid, code, nil, true /* allowDelete：最激进的档 */)
			if len(acts) != 1 || acts[0] != hcActSkipUnpooledHost {
				t.Fatalf("%s/%s 应只报告（%s），得 %v", uid, code, hcActSkipUnpooledHost, acts)
			}
		}
		acc, err := store.GetAccount(uid)
		if err != nil || acc == nil {
			t.Fatalf("GetAccount(%s): %v", uid, err)
		}
		if acc.Disabled {
			t.Fatalf("%s 是未入池代挂号，不该被停用", uid)
		}
		if acc.PoolState != "" && acc.PoolState != "invited" {
			t.Fatalf("%s 的入池状态被改动了：%q", uid, acc.PoolState)
		}
	}

	// 已入池的代挂号**不受**护栏影响（否则平台就没有可用的代挂号了）。
	acts := a.applyHealthDisposition("h-in", hcCodeRequestIllegal, nil, false)
	if len(acts) == 1 && acts[0] == hcActSkipUnpooledHost {
		t.Fatal("已入池的代挂号不该被整体跳过（护栏只针对未入池）")
	}
	if !hasAct(acts, hcActCooled) && !hasAct(acts, hcActNoteFault) {
		t.Fatalf("已入池代挂号应按正常纪律记一次，得 %v", acts)
	}
}

// TestHealthCandidatesExcludeUnpooledHosted 全量检测的候选集不含未入池代挂号。
//
// 与上面那条护栏是**两层**：护栏防"被显式选中检测后遭处置"，
// 候选集防"批量检测时白白出站"（每号一次上游调用，纯浪费 + 多一次风控暴露）。
func TestHealthCandidatesExcludeUnpooledHosted(t *testing.T) {
	store, px, a := newTestApp(t,
		&Account{UID: "h-none", Realm: "cn", AccessToken: "t", OwnerUserID: 7, PoolState: ""},
		&Account{UID: "h-in", Realm: "cn", AccessToken: "t", OwnerUserID: 7, PoolState: "in"},
		&Account{UID: "plat", Realm: "cn", AccessToken: "t"},
	)
	defer store.Close()
	_ = px.SyncAccounts()

	got := map[string]bool{}
	for _, uid := range a.healthCheckCandidates() {
		got[uid] = true
	}
	if got["h-none"] {
		t.Fatal("未入池的代挂号不该进全量检测候选（白出站 + 越权处置）")
	}
	if !got["h-in"] {
		t.Fatal("已入池的代挂号应进候选")
	}
	if !got["plat"] {
		t.Fatal("平台自持号应进候选")
	}
}
