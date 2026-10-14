// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package upstream

import (
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestClientForUIDRoutesAllOutbound 注入 ClientForUID 后，**本包自身的所有出站**
// 都必须走该账号的 client（v0.24.3）。
//
// 为什么必须有这条：修之前，账号/全局出口代理**只对 webapp 的 chat 转发**生效 ——
// webapp 的转发是自建请求（自己选 client），而本包内部的出站一律走 `c.HTTP`（直连）：
// 余额查询、活跃上报、成长任务、模型列表。后果是「代理配了但大半出站没走」，
// 而且**健康检测的结论不反映账号真实的出站路径**（配了代理的号，探活仍从本机出去）。
//
// 这条断言把「每个出站点都得走 doAuth」钉住：以后新增出站忘了走 doAuth，这里会红。
func TestClientForUIDRoutesAllOutbound(t *testing.T) {
	var acctHits, defHits int32
	acctClient := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&acctHits, 1)
		return jsonResp(200, `{"code":0,"data":{}}`), nil
	})}
	defTransport := rtFunc(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&defHits, 1)
		return jsonResp(200, `{"code":0,"data":{}}`), nil
	})

	// call 打一遍本包的全部出站入口（返回错误无所谓 —— 只断言「走了哪个 client」）。
	call := func(c *Client) {
		a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", UID: "u1", Domain: "www.codebuddy.cn"}
		if rc, _, _, _ := c.ChatStream(a, []byte(`{"model":"m","messages":[]}`), "", ChatMeta{ConversationRequestID: "r"}); rc != nil {
			_ = rc.Close()
		}
		_, _, _, _, _, _ = c.ResourcePackages(a)                         // 余额 / 签到链路
		_, _ = c.FetchModels(a)                                          // 模型列表
		_, _ = c.billingJSON(a, http.MethodPost, "/x", map[string]any{}) // 活跃上报
		_, _ = c.growthJSON(a, http.MethodPost, "/x", map[string]any{})  // 成长任务
	}

	c := New()
	c.HTTP = &http.Client{Transport: defTransport}
	c.ChatHTTP = &http.Client{Transport: defTransport}

	// ① 未注入 → 全部走默认 client（保证注入是可选的，不改变既有部署行为）。
	call(c)
	if n := atomic.LoadInt32(&acctHits); n != 0 {
		t.Fatalf("未注入 ClientForUID 时不该走账号 client，得 %d 次", n)
	}
	if atomic.LoadInt32(&defHits) == 0 {
		t.Fatal("未注入时应走默认 client")
	}

	// ② 注入 → 全部走该账号的 client。
	atomic.StoreInt32(&defHits, 0)
	c.ClientForUID = func(uid string) (*http.Client, error) {
		if uid == "u1" {
			return acctClient, nil
		}
		return nil, nil
	}
	call(c)
	if n := atomic.LoadInt32(&acctHits); n < 5 {
		t.Fatalf("注入后 5 条出站入口都应走账号 client，实际只走了 %d 次"+
			"（有出站点没走 doAuth —— 见 ClientForUID 注释）", n)
	}
	if n := atomic.LoadInt32(&defHits); n != 0 {
		t.Fatalf("注入后不该再用默认 client，实际 %d 次", n)
	}

	// ③ 未命中该账号（返回 nil, nil）→ 回落默认，不能 panic 也不能静默不发。
	atomic.StoreInt32(&acctHits, 0)
	atomic.StoreInt32(&defHits, 0)
	c.ClientForUID = func(string) (*http.Client, error) { return nil, nil }
	call(c)
	if atomic.LoadInt32(&acctHits) != 0 {
		t.Fatal("ClientForUID 返回 nil 时不该走账号 client")
	}
	if atomic.LoadInt32(&defHits) == 0 {
		t.Fatal("ClientForUID 返回 nil 时应回落默认 client")
	}
}

// TestClientForUIDErrorDoesNotFallBack 出口配置不可用时**显式失败**，绝不回落直连（v0.24.8）。
//
// 这是「静默裸奔」缺口的回归用例。ClientForUID 的第二个返回值表示"这个账号配了出口、
// 但配置不可用"。旧实现只有两态（非 nil 才用），于是这类账号在**本包自身的出站**上
// （探活 / 签到 / 积分查询 / 模型列表 / 活跃上报）会静默回落到 `c.HTTP` —— 也就是
// 直连：同一个号「chat 被 egressBad 挡住、其它链路照常从本机 IP 出去」。
//
// 这与本仓最核心的一条原则直接冲突：**宁可请求失败，也不从本机 IP 出站**。
func TestClientForUIDErrorDoesNotFallBack(t *testing.T) {
	var defHits int32
	defTransport := rtFunc(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&defHits, 1)
		return jsonResp(200, `{"code":0,"data":{}}`), nil
	})
	c := New()
	c.HTTP = &http.Client{Transport: defTransport}
	c.ClientForUID = func(string) (*http.Client, error) {
		return nil, errors.New("bad egress proxy")
	}

	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/x", nil)
	resp, err := c.doAuth(req, &auth.Auth{UID: "u1"}, false)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("出口不可用时应显式报错，而不是发出去")
	}
	if !strings.Contains(err.Error(), "egress unavailable") {
		t.Fatalf("错误信息应指明是出口不可用，得 %v", err)
	}
	if atomic.LoadInt32(&defHits) != 0 {
		t.Fatal("出口不可用时**绝不能**回落默认 client —— 那等于从本机 IP 裸奔出去")
	}
}

// TestClientNoTimeout chat 链路必须脱掉 Client 级总超时，非 chat 链路保留。
//
// 理由与 webapp 的 chatClientFor 相同：长回答 / Agent 多轮 / 深度思考轻易超过 3 分钟，
// 带总超时的 client 会被网关**自己**掐断，且表现为静默的「执行到一半会话中断」。
// 传输层的 ResponseHeaderTimeout 仍然守着「连上了但迟迟不给响应头」。
func TestClientNoTimeout(t *testing.T) {
	tr := rtFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{}}`), nil
	})
	bounded := &http.Client{Transport: tr, Timeout: 180 * time.Second}

	got := clientNoTimeout(bounded)
	if got.Timeout != 0 {
		t.Fatalf("chat 链路的 client 不该带总超时，得 %v", got.Timeout)
	}
	if got.Transport == nil {
		t.Fatal("脱超时应复用同一 Transport（连接池/代理设置不能丢）")
	}
	// 已在无超时的 client 上调用应原样返回（幂等，不无谓新建）。
	zero := &http.Client{Transport: tr}
	if clientNoTimeout(zero) != zero {
		t.Fatal("Timeout 已为 0 时应原样返回，不该新建副本")
	}
	if clientNoTimeout(nil) != nil {
		t.Fatal("nil 应原样返回")
	}
	// 非 chat 链路：doAuth 必须保留总超时（有界）。
	c := New()
	c.ClientForUID = func(string) (*http.Client, error) { return bounded, nil }
	if c.HTTP == nil {
		t.Fatal("New() 应给出默认 HTTP client")
	}
	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/x", nil)
	resp, err := c.doAuth(req, &auth.Auth{UID: "u1"}, false)
	if err != nil {
		t.Fatalf("doAuth(chat=false) 应成功：%v", err)
	}
	_ = resp.Body.Close()
}
