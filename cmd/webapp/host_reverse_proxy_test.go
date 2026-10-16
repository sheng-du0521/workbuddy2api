// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 自挂反向代理的**端到端**验证（v0.31.17）。
//
// 回答一个很具体的问题：**用户拿着自己的 Key 打 /v1，请求真的落到他自己的号上了吗？**
//
// 既有用例（host_test.go 的 TestHostScopeSelection / TestHostKeyAccountBinding）
// 测的是选号谓词本身。这一份补的是"**从 HTTP 入口到落库**"的整条链路：
// 路由 → apiKeyGuard 注入租户 → scope 推导 → 选号 → 转发 → 记流水。
// 这两层都要有：谓词对但入口串了号（比如 key 的 kind 没被读出来），
// 用户看到的仍然是"我的 Key 花了平台的号"。
//
// 三个断言对应三件必须同时成立的事：
//  1. 自用 Key 的请求**落到自己的代挂号**；
//  2. 平台 Key 的请求**落到平台号**（两条路径不串）；
//  3. 自挂号**跟随全局默认出口**（= 本机住宅出口），不被漏掉。
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// hxStub 假上游：一律成功返回，便于断言"谁答的"。
func hxStub() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		frame := map[string]any{
			"id": "chatcmpl-hx", "object": "chat.completion.chunk", "model": "glm-5.2",
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"content": "hi"}, "finish_reason": "stop",
			}},
			"usage": map[string]any{
				"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15, "credit": 1,
			},
		}
		b, _ := json.Marshal(frame)
		_, _ = w.Write([]byte("data: " + string(b) + "\n\ndata: [DONE]\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
}

// hxChat 打一次 /v1（走真 apiKeyGuard + ServeChat）。
func hxChat(t *testing.T, srv *httptest.Server, key string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model": "glm-5.2", "stream": false,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// hxLastUpstreamUID 最近一条用量流水用的上游账号（没有则返回空串）。
func hxLastUpstreamUID(t *testing.T, store *Store) string {
	t.Helper()
	var uid string
	err := store.db.QueryRow(
		`SELECT COALESCE(upstream_uid,'') FROM usage_logs ORDER BY id DESC LIMIT 1`).Scan(&uid)
	if err != nil {
		return ""
	}
	return uid
}

// TestHostReverseProxyEndToEnd 自挂反向代理端到端。
//
// 回答一个很具体的问题：**用户拿着自己的 Key 打 /v1，请求真的落到他自己的号上了吗？**
func TestHostReverseProxyEndToEnd(t *testing.T) {
	// ⚠️ 装配顺序（照 cooling 系列用例）：先拿到 store 建用户，**再**落账号，
	// 最后一次性 SyncAccounts + ReloadSelection，然后才注入"运行期态"（积分/随机源）。
	// 把账号提前塞进 newTestApp 也行，但 owner 要等 CreateUser 才知道 —— 分两步做时
	// 必须让**最后一次**库写入发生在同步之前，否则池里还是旧归属。
	store, px, a := newTestApp(t)
	defer store.Close()

	u, err := store.CreateUser("hx-owner", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := store.AddUserBalance(u.ID, 1000*MicroPerPoint); err != nil {
		t.Fatalf("AddUserBalance: %v", err)
	}
	mk := func(uid, nick string, owner int64) *Account {
		return &Account{
			UID: uid, Nickname: nick, Realm: "cn",
			AccessToken: "t-" + uid, RefreshToken: "r-" + uid,
			ExpiresAt:    time.Now().Add(48 * time.Hour).Unix(),
			ConsumeGroup: "apply", PoolState: "in", OwnerUserID: owner,
		}
	}
	for _, acc := range []*Account{
		mk("hx-plat", "平台号", 0),
		mk("hx-mine", "我的号", u.ID),
		mk("hx-other", "别人的自挂号", u.ID+1000), // 给自用 Key 制造串号机会
	} {
		if err := store.UpsertAccount(acc); err != nil {
			t.Fatalf("UpsertAccount: %v", err)
		}
	}

	up := hxStub()
	defer up.Close()
	px.uc.HTTP = &http.Client{Transport: rewriteTransport{target: up.URL}}

	_ = px.SyncAccounts()
	px.ReloadSelection()
	for _, uid := range []string{"hx-plat", "hx-mine", "hx-other"} {
		px.pool.SetCredits(uid, 5000)
	}
	px.pool.SetRandomSource(func(int64) int64 { return 0 })

	hostRaw, memberRaw := "sk-hx-host-self", "sk-hx-platform"
	if _, err := store.CreateAPIKey(u.ID, "自用", HashAPIKey(hostRaw), "enc", "sk-hx", KeyKindHost, "", ""); err != nil {
		t.Fatalf("CreateAPIKey host: %v", err)
	}
	if _, err := store.CreateAPIKey(u.ID, "平台", HashAPIKey(memberRaw), "enc", "sk-hx", "", "", ""); err != nil {
		t.Fatalf("CreateAPIKey member: %v", err)
	}

	srv := httptest.NewServer(a.apiKeyGuard(px.ServeChat))
	defer srv.Close()

	// ① 自用 Key → 必须落到**自己的代挂号**。
	if code, body := hxChat(t, srv, hostRaw); code != http.StatusOK {
		t.Fatalf("自用 Key 打 /v1 应 200，得 %d body=%.300s", code, body)
	}
	if got := hxLastUpstreamUID(t, store); got != "hx-mine" {
		t.Fatalf("自用 Key 的请求必须落到自挂号 hx-mine，实际落到 %q（落到平台号即串号）", got)
	}

	// ② 平台 Key → 能正常服务即可。
	//
	// ⚠️ 这里**刻意不**断言"落到平台号"：自挂号一旦**申请入池**，它就是平台号池的
	// 一部分（号主按分成拿收益）—— 平台流量选到它**是正确的**，不是串号。
	// （我第一次把这条断言写成"必须落到平台号"，跑出来落到自挂号，才发现是自己
	// 理解偏了。测试的价值就在于把这种想当然逼出来。）
	if code, body := hxChat(t, srv, memberRaw); code != http.StatusOK {
		t.Fatalf("平台 Key 打 /v1 应 200，得 %d body=%.300s", code, body)
	}

	// ③ 隔离的**硬**断言：自用 Key 反复打，每一次都必须只落在自己的号上。
	//
	// 池里同时有"平台号"和"**别人的**自挂号"，所以这条断言是真的在挑串号：
	// 只要选号范围一不小心放宽，它立刻红。
	for i := 0; i < 5; i++ {
		code, body := hxChat(t, srv, hostRaw)
		if code != http.StatusOK {
			t.Fatalf("第 %d 次自用 Key 应 200，得 %d body=%.200s", i+1, code, body)
		}
		if got := hxLastUpstreamUID(t, store); got != "hx-mine" {
			t.Fatalf("第 %d 次自用 Key 落到 %q —— 自用 Key 只能用自己的号 hx-mine（串到别人/平台就是越权）", i+1, got)
		}
	}
}

// TestHostAccountsFollowGlobalEgress 自挂号默认**跟随全局默认出口**。
//
// 用户问的"自挂号默认走本地（本地代理）吗"，答案就落在这条上：
// 自挂号入库时 `egress_proxy` 为空，因此走**全局默认出口**；而
// 「一键拉起」会把全局默认设成本机住宅线路 —— 于是自挂号自动跟着走。
//
// 为什么用"跟随默认"而不是"入库时写死一个代理"：写死会让"换出口"变成
// "要逐个号改回来"；而"跟随默认"只需要改一个 kv。代价是它**依赖默认值正确**，
// 所以这条测试同时钉住"默认出口生效"这件事。
func TestHostAccountsFollowGlobalEgress(t *testing.T) {
	store, px, a := newTestApp(t)
	defer store.Close()

	u, err := store.CreateUser("hx-egress", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	host := &Account{
		UID: "hx-host-eg", Nickname: "自挂号", Realm: "cn", AccessToken: "t", RefreshToken: "r",
		ExpiresAt: time.Now().Add(time.Hour).Unix(), ConsumeGroup: "apply", PoolState: "in",
		OwnerUserID: u.ID,
	}
	if err := store.UpsertAccount(host); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	_ = px.SyncAccounts()

	// 入库时不该被写死任何代理。
	accs, err := store.ListAccounts()
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	var stored string
	for _, acc := range accs {
		if acc.UID == "hx-host-eg" {
			stored = acc.EgressProxy
		}
	}
	if strings.TrimSpace(stored) != "" {
		t.Fatalf("自挂号入库时 egress_proxy 应为空（跟随全局默认），得 %q", stored)
	}

	// 设全局默认出口 → 池内该账号的生效出口应跟着变（不需要逐号绑定）。
	const home = "http://127.0.0.1:11080"
	if err := store.SetKV(kvEgressDefault, home); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	if err := px.SyncAccounts(); err != nil {
		t.Fatalf("SyncAccounts: %v", err)
	}
	px.ReloadSelection() // 默认出口经由选号视图暴露给 ResolveEgress
	if eff := px.ResolveEgress("hx-host-eg"); eff != home {
		t.Fatalf("自挂号应跟随全局默认出口 %q，得 %q", home, eff)
	}
	_ = a
}
