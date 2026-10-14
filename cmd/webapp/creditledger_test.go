// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// rewriteTransport 把出站请求改指向本地 httptest 上游。
//
// 为什么必须做全链路（而不是直接单测 pool.NoteModelCost）：
// 本次缺陷的形态是**接线缺失** —— `pool.NoteModelCost` 在 internal/server 里被调用、
// 在 cmd/webapp 里从来没人调。单测 pool 只能证明「函数本身对」，
// 恰恰漏掉「webapp 压根没调它」这一层。所以这里从 ServeChat 入口真跑一遍。
type rewriteTransport struct{ target string }

func (t rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u, err := http.NewRequestWithContext(req.Context(), req.Method, t.target+req.URL.Path, req.Body)
	if err != nil {
		return nil, err
	}
	u.Header = req.Header.Clone()
	return http.DefaultTransport.RoundTrip(u)
}

// sseUpstream 返回一个吐单帧 SSE（带 usage.credit）的假上游。
func sseUpstream(t *testing.T, credit float64, promptTok, compTok int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		frame := map[string]any{
			"id": "chatcmpl-test", "object": "chat.completion.chunk", "model": "m-test",
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"content": "hi"}, "finish_reason": "stop",
			}},
			"usage": map[string]any{
				"prompt_tokens": promptTok, "completion_tokens": compTok,
				"total_tokens": promptTok + compTok, "credit": credit,
			},
		}
		b, _ := json.Marshal(frame)
		fmt.Fprintf(w, "data: %s\n\n", b)
		fmt.Fprint(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
}

// poolCredits 读池内该账号的余额（pool 未导出单账号读入口，走 List 扫描）。
func poolCredits(t *testing.T, px *Proxy, uid string) int64 {
	t.Helper()
	for _, st := range px.pool.List() {
		if st.UID == uid {
			return st.Credits
		}
	}
	t.Fatalf("账号 %s 不在池内", uid)
	return 0
}

// newChatApp 装配一个可跑 ServeChat 的 app：一个健康账号 + 一个计费用户与 Key。
func newChatApp(t *testing.T, uid string, credits int64) (*Store, *Proxy, *app, *httptest.Server, *httptest.Server) {
	t.Helper()
	store, px, a := newTestApp(t, &Account{
		UID: uid, Nickname: "chat-acct", Realm: "cn",
		AccessToken: "tok", RefreshToken: "rtok",
		ExpiresAt:    time.Now().Add(48 * time.Hour).Unix(),
		ConsumeGroup: "apply", // 平台选号只认「参与消耗」的分组
		PoolState:    "in",
	})
	up := sseUpstream(t, 5, 100, 200)
	px.uc.HTTP = &http.Client{Transport: rewriteTransport{target: up.URL}}

	u, err := store.CreateUser("chat-payer", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := store.AddUserBalance(u.ID, 1000*MicroPerPoint); err != nil {
		t.Fatalf("AddUserBalance: %v", err)
	}
	raw := "sk-wb-chat-test-key"
	if _, err := store.CreateAPIKey(u.ID, "test", HashAPIKey(raw), "enc", "sk-wb", "", "", ""); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	px.ReloadSelection()
	// 账号积分不在 accounts 表里（表结构无该列）—— 池内 credits 由签到写回或持久快照恢复。
	// 测试里直接种入，才能观察「消耗是否被扣回」。
	px.pool.SetCredits(uid, credits)

	srv := httptest.NewServer(a.apiKeyGuard(px.ServeChat))
	return store, px, a, srv, up
}

// doChat 发一次对话请求，并**读完响应体**再返回。
//
// 为什么必须读完：流式路径在 streamSSE 开头就 WriteHeader(200)，`Do` 拿到响应头即返回 ——
// 此时 handler 还在读上游流，usage 尚未解析、池内余额尚未回写。不读完就断言是竞态
// （实测：非流式通过、流式偶发「没扣」）。读完 body 保证 handler 已收尾。
func doChat(t *testing.T, srv *httptest.Server, rawKey, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+rawKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("读响应体: %v", err)
	}
	_ = resp.Body.Close()
	return resp
}

// TestChatSuccessFeedsPoolCreditLedger 一次成功的对话必须把「消耗」回写到池内余额。
//
// 这是「号池仪表盘实时数据不更新」的根治性回归测试。修复前 `pool.NoteModelCost`
// 在 cmd/webapp 链路**从未被调用**，后果有两个：
//  1. 池内 credits 只在签到（9 点 / 21 点）刷新 → 仪表盘的「积分合计 / 平均积分 /
//     积分 Top 8」在两次签到之间（最长 12 小时）整块冻结，用户看到的就是「不实时」；
//  2. 池内 (账号, 模型) 成本观测恒空 → 选号的成本分层永远停在「无观测」。
//
// 断言直接看池内余额（数据源头），而不是看接口返回的聚合。
func TestChatSuccessFeedsPoolCreditLedger(t *testing.T) {
	const uid = "chat-uid-0001"
	store, px, _, srv, up := newChatApp(t, uid, 5000)
	defer store.Close()
	defer srv.Close()
	defer up.Close()
	defer px.Close()

	before := poolCredits(t, px, uid)
	if before != 5000 {
		t.Fatalf("初始余额应为 5000，得 %d", before)
	}
	_ = before

	resp := doChat(t, srv, "sk-wb-chat-test-key",
		`{"model":"m-test","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应 200，得 %d", resp.StatusCode)
	}

	after := poolCredits(t, px, uid)
	if after >= before {
		t.Fatalf("上游回报 credit=5，池内余额应被扣减：前 %d → 后 %d（未扣 = NoteModelCost 没被调用）",
			before, after)
	}
	if before-after != 5 {
		t.Fatalf("应恰好扣 5（credit 为整数），实际扣 %d", before-after)
	}
	// 成本账本同时必须被写入（(账号,模型) 粒度观测），否则选号成本分层恒为「无观测」。
	if per1k, ok := px.pool.ModelCost(uid, "m-test"); !ok {
		t.Fatal("池内 (账号,模型) 成本观测未被写入（该号永远进不了成本分层）")
	} else if per1k <= 0 {
		t.Fatalf("credit=5 / 300 tokens → per1k 应 > 0，得 %v", per1k)
	}
}

// TestStreamSuccessFeedsPoolCreditLedger 流式路径同样必须回写（两条路径都要覆盖）。
func TestStreamSuccessFeedsPoolCreditLedger(t *testing.T) {
	const uid = "chat-uid-0002"
	store, px, _, srv, up := newChatApp(t, uid, 5000)
	defer store.Close()
	defer srv.Close()
	defer up.Close()
	defer px.Close()

	resp := doChat(t, srv, "sk-wb-chat-test-key",
		`{"model":"m-test","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应 200，得 %d", resp.StatusCode)
	}
	if got := poolCredits(t, px, uid); got != 4995 {
		t.Fatalf("流式路径也应扣 5：前 5000 → 后 %d", got)
	}
}

// TestAdminChannelUsageFeedsPoolLedger 管理密钥通道的请求也必须进号池台账。
//
// 修复前 settleUsage 在 u == nil 时直接 return，管理密钥发的请求在 usage_logs 里
// **完全不存在** —— 运维一边用管理密钥自测、一边看仪表盘，会看到「累计服务 / 号池消耗
// Top」纹丝不动，从而误判「仪表盘坏了」。这条锁定它必须进台账（user_id=0，零费用）。
func TestAdminChannelUsageFeedsPoolLedger(t *testing.T) {
	const uid = "chat-uid-0003"
	store, px, a, srv, up := newChatApp(t, uid, 5000)
	defer store.Close()
	defer srv.Close()
	defer up.Close()
	defer px.Close()

	// 号池侧聚合（仪表盘「累计服务」的数据源）在请求前的基数。
	before, err := store.UsageByAccount()
	if err != nil {
		t.Fatalf("UsageByAccount: %v", err)
	}
	beforeReqs := before[uid].Requests

	// 用**管理密钥**（a.key）请求：走 apiKeyGuard 的管理通道 → u == nil。
	resp := doChat(t, srv, a.key,
		`{"model":"m-test","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应 200，得 %d", resp.StatusCode)
	}

	after, err := store.UsageByAccount()
	if err != nil {
		t.Fatalf("UsageByAccount: %v", err)
	}
	if after[uid].Requests != beforeReqs+1 {
		t.Fatalf("管理密钥流量必须计入号池台账：%d → %d（修复前恒不变）",
			beforeReqs, after[uid].Requests)
	}
	// 管理通道不收费：该行 charged_micro 必须为 0（否则等于向 0 号用户凭空扣费）。
	var charged int64
	if err := store.db.QueryRow(
		`SELECT COALESCE(SUM(charged_micro),0) FROM usage_logs WHERE user_id = 0 AND upstream_uid = ?`,
		uid).Scan(&charged); err != nil {
		t.Fatalf("查流水: %v", err)
	}
	if charged != 0 {
		t.Fatalf("管理通道不应产生任何扣费，得 %d 微点", charged)
	}
}
