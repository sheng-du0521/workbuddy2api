// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 用量「归属」拆分（v0.31.20）的回归。
//
// 这一处最容易出的错不是"算错"，而是**桶漏了**：SQL 的 GROUP BY 只返回有数据的
// 桶，若前端按 `d.host.requests` 直接取，缺桶时就是 `undefined` → 页面显示 `undefined 次`。
// 所以 `usageSplitPayload` 必须把桶补全；下面的用例就是钉这件事。
package main

import (
	"os"
	"strings"
	"testing"
)

func TestUsageSplitPayloadFillsMissingBuckets(t *testing.T) {
	// 只有 pool 有数据（新部署、还没人用代挂时的常见形态）。
	got := usageSplitPayload(14, []usageSplitRow{
		{Kind: "pool", Requests: 10, Credit: 5, Charged: 20},
	})
	rows, ok := got["rows"].([]usageSplitRow)
	if !ok || len(rows) != 3 {
		t.Fatalf("rows 应恒为 3 个桶（pool/host/plat），得 %#v", got["rows"])
	}
	// 缺的桶必须存在且为 0，而不是 nil/缺失。
	for _, k := range []string{"pool", "host", "plat"} {
		v, ok := got[k].(usageSplitRow)
		if !ok {
			t.Fatalf("缺少桶 %q（前端会读到 undefined）", k)
		}
		if v.Kind != k {
			t.Fatalf("桶 %q 的 kind 字段应为 %q，得 %q", k, k, v.Kind)
		}
	}
	total := got["total"].(map[string]any)
	if total["requests"].(int64) != 10 {
		t.Fatalf("合计应为 10，得 %v", total["requests"])
	}

	// 两个桶都有数据：合计必须等于两者之和（而不是某一边）。
	got2 := usageSplitPayload(14, []usageSplitRow{
		{Kind: "pool", Requests: 3, Charged: 7},
		{Kind: "host", Requests: 4, Charged: 0},
	})
	t2 := got2["total"].(map[string]any)
	if t2["requests"].(int64) != 7 {
		t.Fatalf("合计应为 7，得 %v", t2["requests"])
	}
	if t2["charged"].(float64) != 7 {
		t.Fatalf("收费合计应为 7，得 %v", t2["charged"])
	}
}

// TestUsageSplitQueriesFilterOKOnly 两条拆分 SQL 都必须只统计成功请求。
//
// 为什么用源码级断言而不是跑数据：`status='ok'` 漏掉时**功能仍然"能跑"**，
// 只是数字被失败请求抬高 —— 这种错在界面上看不出来（"消耗变多了"很容易被
// 当成"最近用得多"），只能靠把约束钉在源码上防回归。
func TestUsageSplitQueriesFilterOKOnly(t *testing.T) {
	raw, err := os.ReadFile("usagesplit.go")
	if err != nil {
		t.Fatalf("读 usagesplit.go: %v", err)
	}
	src := string(raw)
	for _, name := range []string{"UsageSplitByKeyKind", "UsageSplitByOwner"} {
		i := strings.Index(src, "func (s *Store) "+name)
		if i < 0 {
			t.Fatalf("找不到 %s", name)
		}
		body := src[i:]
		if j := strings.Index(body, "\n}"); j > 0 {
			body = body[:j]
		}
		if !strings.Contains(body, "u.status = 'ok'") {
			t.Fatalf("%s 的查询缺少 status='ok' 过滤（会把失败请求算进消耗）", name)
		}
	}
	// 管理端还必须排除管理密钥通道（user_id=0，按设计不计费）。
	i := strings.Index(src, "func (s *Store) UsageSplitByOwner")
	if i < 0 || !strings.Contains(src[i:i+900], "u.user_id > 0") {
		t.Fatal("UsageSplitByOwner 应排除 user_id=0（管理通道按设计不计费，混进来会让归属失真）")
	}
}
