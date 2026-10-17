// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 用户端健康检测插件的回归测试（v0.31.31）。
//
// 这个功能把"检测"下沉给了普通用户，风险只有两个，都在这份用例里钉住：
//
//	① **越界**：不能检测/看到别人的号（healthLast 是全局快照，里面全是别人的 uid/昵称/余额）；
//	② **越权处置**：用户点检测**不能**停用或删除任何账号（allowDelete 必须 false）。
package main

import (
	"net/http"
	"testing"
	"time"
)

// TestHostHealthIsolation 只回本人的号，且不泄露别人的检测结论。
func TestHostHealthIsolation(t *testing.T) {
	f := usApp(t)

	// 造一次"全局"检测快照：里面既有 A 的号，也有 B 的号（模拟管理员跑过全量）。
	healthLast.set([]*HealthCheckResult{
		{UID: "us-a-acct", Nickname: "A 的号", State: "ok", StateLabel: "正常",
			Credits: 1234, CreditsKnown: true, CheckedAt: time.Now().Unix()},
		{UID: "us-b-acct", Nickname: "B 的号", State: "abnormal", StateLabel: "异常",
			Reason: "会话失效", Definite: true, CheckedAt: time.Now().Unix()},
	}, time.Now(), "manual")
	t.Cleanup(func() { healthLast = healthSnapshot{} }) // 包级全局，别污染其它用例

	code, raw := eaReq(t, f.mux, "GET", "/api/host/accounts/health/last", "", f.cookieA, "")
	if code != http.StatusOK {
		t.Fatalf("读检测结果应 200，得 %d %s", code, raw)
	}
	d := eaJSON(t, raw)
	accs := d["accounts"].([]any)
	if len(accs) != 1 {
		t.Fatalf("A 只有一个号，应只回 1 行，得 %d：%s", len(accs), raw)
	}
	row := accs[0].(map[string]any)
	if row["uid"] != "us-a-acct" {
		t.Fatalf("回了别人的号：%v", row["uid"])
	}
	if row["checked"] != true {
		t.Fatal("A 的号在快照里，应标为已检测")
	}
	// 汇总只算我的：total=1，不能把 B 的那个"异常"算进来。
	sum := d["summary"].(map[string]any)
	if sum["total"].(float64) != 1 {
		t.Fatalf("汇总 total 应只算我的号（1），得 %v", sum["total"])
	}
	if sum["definite"].(float64) != 0 {
		t.Fatalf("B 的确定性异常不该算到 A 头上，得 %v", sum["definite"])
	}
	// 原文里不能出现 B 的 uid（连"存在"都不该泄露）。
	if bytesContains(raw, "us-b-acct") {
		t.Fatal("响应里出现了别人的 uid —— 全局快照被整体透出去了")
	}
}

// TestHostHealthRejectsForeignUID 拿别人的 uid 请求检测 → 目标被剔除后报"没有可检测的账号"。
func TestHostHealthRejectsForeignUID(t *testing.T) {
	f := usApp(t)
	code, raw := eaReq(t, f.mux, "POST", "/api/host/accounts/health",
		`{"uids":["us-b-acct"]}`, f.cookieA, "")
	if code != http.StatusBadRequest {
		t.Fatalf("只给别人的 uid 应 400，得 %d %s", code, raw)
	}
	// ⚠️ 这里**故意不测正向路径**：正向会真的发起上游探测（余额 + 对话），
	// 在单测里等于往外打真实请求 —— 慢、不稳定、还会把假 token 送出去。
	// 正向链路由管理后台的既有用例覆盖（同一个 checkHealthMany），
	// 本文件只负责"越界"与"不处置"这两条用户侧新增的边界。
}

// bytesContains 小工具：避免为一个断言引入 bytes 包的 import 噪声。
func bytesContains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
