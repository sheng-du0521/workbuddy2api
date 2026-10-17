// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestBuildExportRowsFields 导出行的字段映射（尤其是秒→毫秒）。
//
// 这条守的是"导出的文件能不能再导回来"：`expiresAt` 在库里是**秒**，
// 而桌面端/导入侧一律按**毫秒**解析。差 1000 倍的结果是导入后 token 立刻
// 被判过期 → 保活立刻刷新 → 反而可能触发并发刷新（2026-10-18 的事故方向）。
func TestBuildExportRowsFields(t *testing.T) {
	rows := buildExportRows([]*Account{{
		UID: "u1", Nickname: "自牧", Realm: "cn", Domain: "www.codebuddy.cn",
		AccessToken: "at-1", RefreshToken: "rt-1", ExpiresAt: 1794664871,
		DeviceToken: "dt-1", EnterpriseID: "ent-1", Group: "反代",
		PoolState: "in", OwnerUserID: 7, ConsumeGroup: "apply",
	}}, 1792260000, "wb2api/test admin")

	if len(rows) != 1 {
		t.Fatalf("应导出 1 行，得 %d", len(rows))
	}
	r := rows[0]
	if r.ExpiresAt != 1794664871000 {
		t.Fatalf("expiresAt 必须是毫秒（秒×1000），得 %d", r.ExpiresAt)
	}
	if r.AccessToken != "at-1" || r.RefreshToken != "rt-1" || r.DeviceToken != "dt-1" {
		t.Fatalf("凭证字段丢了：%+v", r)
	}
	if r.TokenType != "Bearer" {
		t.Fatalf("有 access_token 时应标 Bearer，得 %q", r.TokenType)
	}
	if r.OwnerID != 7 || r.PoolState != "in" || r.ConsumeGrp != "apply" {
		t.Fatalf("元信息丢了：%+v", r)
	}

	// 零过期时间必须保持 0（不能变成 1970-01-01），否则导入侧会把它当成
	// "有个很早以前的过期时间"而不是"没有过期信息"。
	zero := buildExportRows([]*Account{{UID: "u2", AccessToken: "at"}}, 1, "x")
	if zero[0].ExpiresAt != 0 {
		t.Fatalf("零过期时间应保持 0，得 %d", zero[0].ExpiresAt)
	}
}

// TestExportRoundTrip 导出 → 序列化 → 用导入侧解析器读回：关键字段必须一一对得上。
//
// 为什么这条最有价值：导出与导入是两段独立代码，**字段名漂移不会报错**，
// 只会静默丢字段（device_token 就这样被丢过一轮，导致全池 0/27 非空）。
// 只有把两边放在同一条断言里，漂移才会红。
func TestExportRoundTrip(t *testing.T) {
	src := &Account{
		UID: "u-round", Nickname: "烤乳鸽", Realm: "cn", Domain: "www.codebuddy.cn",
		AccessToken: "at-round", RefreshToken: "rt-round", ExpiresAt: 1794664871,
		DeviceToken: "dt-round", EnterpriseID: "ent-round", Group: "反代",
		PoolState: "in", OwnerUserID: 3, ConsumeGroup: "apply",
	}
	raw, err := json.Marshal(buildExportRows([]*Account{src}, 1792260000, "wb2api/test"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// 顶层必须是数组（导入侧第一条分支）。
	if !strings.HasPrefix(strings.TrimSpace(string(raw)), "[") {
		t.Fatalf("导出必须是顶层数组，得：%s", string(raw)[:40])
	}

	list, err := parseDesktopAccounts(raw)
	if err != nil {
		t.Fatalf("导入侧解析失败（导出格式与导入不闭环）：%v", err)
	}
	if len(list) != 1 {
		t.Fatalf("应解析出 1 条，得 %d", len(list))
	}
	got := list[0]
	if got.UID != src.UID || got.Nickname != src.Nickname {
		t.Fatalf("身份字段不一致：%+v", got)
	}
	if got.AccessToken != src.AccessToken || got.RefreshToken != src.RefreshToken {
		t.Fatalf("凭证字段不一致（导出/导入字段名漂移）：%+v", got)
	}
	if got.DeviceToken != src.DeviceToken {
		t.Fatalf("device_token 丢了（历史上就这样丢过一轮）：%+v", got)
	}
	if got.ExpiresAt != src.ExpiresAt*1000 {
		t.Fatalf("expiresAt 应为毫秒 %d，得 %d", src.ExpiresAt*1000, got.ExpiresAt)
	}
	if got.Group != src.Group {
		t.Fatalf("分组丢失：%q", got.Group)
	}
}

// TestParseUIDList 导出筛选的解析（空串 = 不筛选）。
func TestParseUIDList(t *testing.T) {
	if parseUIDList("") != nil {
		t.Fatal("空串应返回 nil（不筛选）")
	}
	if parseUIDList(" , ,") != nil {
		t.Fatal("全是空白应返回 nil")
	}
	m := parseUIDList(" a , b ,, c ")
	if len(m) != 3 || !m["a"] || !m["b"] || !m["c"] {
		t.Fatalf("解析结果不对：%+v", m)
	}
}
