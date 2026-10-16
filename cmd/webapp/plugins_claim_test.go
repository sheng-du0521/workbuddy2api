// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 补偿领取的**反馈判定**（v0.31.19）。
//
// 用户现场：「插件显示无可领」—— 他要的不是"再跑一遍"，而是**到底是已领取、
// 还是当前没有这个活动**。此前两者共用一个 none 状态、一句笼统文案
// （"已领过或当前无活动"），只能来问人。
//
// 这里钉住的是纯函数（解析 + 判定），不打网络：判据本身错了，跑真机只会得到
// 一个"看起来合理"的错结论。
package main

import "testing"

func TestClaimBizReplyParse(t *testing.T) {
	// 真机原样（2026-10-16，池内账号调 claim-compensation 的答复）。
	real := `upstream client (http 400): {"code":10001,"msg":"每人限领一次，您已领取过无法重复领取","requestId":"bbb5682a-7a7a-4417-a6fc-0f66a3ede59f"}`
	code, msg := claimBizReply(real)
	if code != 10001 {
		t.Fatalf("应解析出 code=10001，得 %d", code)
	}
	if msg != "每人限领一次，您已领取过无法重复领取" {
		t.Fatalf("应解析出完整 msg，得 %q", msg)
	}

	// 尾部还有字段时**不能**整体失败：这正是 json.Unmarshal 会栽的地方
	// （"invalid character 'r' after top-level value"），Decoder 只取第一个值才对。
	tail := `{"code":0,"msg":"ok"} trailing garbage`
	if c, m := claimBizReply(tail); c != 0 || m != "ok" {
		t.Fatalf("尾部有噪声也应解出第一个 JSON 值，得 %d %q", c, m)
	}

	// 解不出来就老实返回 0/""，调用方据此退回笼统文案 —— 绝不猜。
	for _, bad := range []string{"", "network unreachable", "<html>502</html>", "upstream client (http 500): "} {
		if c, m := claimBizReply(bad); c != 0 || m != "" {
			t.Fatalf("%q 应解析失败返回零值，得 %d %q", bad, c, m)
		}
	}
}

func TestClaimAlreadyReply(t *testing.T) {
	cases := []struct {
		name string
		code int
		msg  string
		want bool
	}{
		{"实测业务码 10001", 10001, "每人限领一次，您已领取过无法重复领取", true},
		{"业务码优先于文案", 10001, "", true},
		{"文案命中「已领取」", 0, "您已领取过该补偿", true},
		{"文案命中「限领一次」", 0, "每人限领一次", true},
		{"英文文案", 0, "You have already claimed this reward", true},

		// ⚠️ 反例比正例更重要：把"当前没有活动"误判成"已领取"，会让用户
		// 以为补偿已经到账、从此不再尝试 —— 那是**沉默的错**。
		{"无活动（反例）", 0, "当前没有可领取的补偿", false},
		{"活动已结束（反例）", 0, "该活动已结束", false},
		{"无文案（反例）", 0, "", false},
		{"限流（反例）", 0, "too many requests", false},
	}
	for _, c := range cases {
		if got := claimAlreadyReply(c.code, c.msg); got != c.want {
			t.Errorf("%s：claimAlreadyReply(%d,%q)=%v，期望 %v", c.name, c.code, c.msg, got, c.want)
		}
	}
}
