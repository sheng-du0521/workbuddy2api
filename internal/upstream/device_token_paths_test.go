// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package upstream

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy2api/internal/auth"
)

// 本文件钉住一条纪律（v0.31.27 生产审计发现）：
// **设备凭证必须与 X-Machine-ID / X-Session-ID 在同一层注入。**
//
// 此前 X-Device-Token 只在 ChatHeaders 与 BillingHeaders 里单独注入，于是：
//
//	对话      → machine + session + device-token   ✅
//	余额/签到 → machine + session + device-token   ✅
//	模型列表  → machine + session                  ❌（fetchEnterpriseModels / fetchV3Models / globalModelsOnce）
//	refresh   → machine + session                  ❌
//
// 同一个账号在「对话」和「拉模型」上对上游呈现**两套设备身份**，而"稳定差异"
// 正是可用于聚类的特征。设备凭证审计页看不出这一点：它统计的是数据层（每号有没有
// 凭证值），不是路径层（每条出站请求带没带）。
//
// 为什么用"公共头 + 真实路径"两层断言，而不是只测 CommonHeaders：
// 只测公共函数证明不了模型列表真的走了它 —— 那正是这次漏掉的形态（三处各自
// `http.NewRequest` + 各自 `CommonHeaders`）。所以既要钉公共层，也要钉真实路径。

// dtTestClient 一个启用派生兜底的客户端（生产 kv device_token_fallback=derive 的等价物）。
func dtTestClient() *Client {
	return &Client{DeviceTokenFallback: true}
}

// TestCommonHeadersInjectsDeviceToken 公共头必须带设备凭证（与 machine/session 同层）。
func TestCommonHeadersInjectsDeviceToken(t *testing.T) {
	c := dtTestClient()
	a := &auth.Auth{UID: "u-dt-1"}
	req, _ := http.NewRequest(http.MethodGet, "http://example.invalid/x", nil)
	c.CommonHeaders(req, a)

	if got, want := req.Header.Get("X-Device-Token"), AccountDeviceToken("u-dt-1"); got != want {
		t.Fatalf("CommonHeaders 未注入 X-Device-Token：got=%q want=%q", got, want)
	}
	// 三个头都必须在，且互不相同（用途盐隔离）——缺一个都等于给上游留了"身份不一致"的口子。
	for _, h := range []string{"X-Machine-ID", "X-Session-ID", "X-Device-Token"} {
		if req.Header.Get(h) == "" {
			t.Fatalf("公共头缺 %s", h)
		}
	}
	if req.Header.Get("X-Machine-ID") == req.Header.Get("X-Device-Token") ||
		req.Header.Get("X-Session-ID") == req.Header.Get("X-Device-Token") {
		t.Fatal("设备凭证与 machine/session 撞值（用途盐失效）")
	}
}

// TestCommonHeadersOmitsDeviceTokenWhenUnavailable 没有凭证且未开兜底 → 不注入（优雅降级）。
//
// 反例必须留着：把"没凭证"写成空值头，比不带头更像伪造（官方客户端要么有值要么没有这个头）。
func TestCommonHeadersOmitsDeviceTokenWhenUnavailable(t *testing.T) {
	c := &Client{} // DeviceTokenFallback=false，且无全局/文件凭证
	req, _ := http.NewRequest(http.MethodGet, "http://example.invalid/x", nil)
	c.CommonHeaders(req, &auth.Auth{UID: "u-dt-2"})
	if _, ok := req.Header[http.CanonicalHeaderKey("X-Device-Token")]; ok {
		t.Fatalf("无凭证时不该出现该头，得 %q", req.Header.Get("X-Device-Token"))
	}
	// machine/session 仍应有值：它们是纯派生，不依赖凭证。
	if req.Header.Get("X-Machine-ID") == "" {
		t.Fatal("machine 头不该因为缺设备凭证而消失")
	}
}

// TestModelListPathCarriesDeviceToken 真实路径断言：拉模型列表必须带设备凭证。
func TestModelListPathCarriesDeviceToken(t *testing.T) {
	var got string
	var seen bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Device-Token")
		seen = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"models":[{"id":"m1"}]}}`))
	}))
	defer srv.Close()

	c := dtTestClient()
	c.ChatBaseGlobal = srv.URL
	c.HTTP = srv.Client()
	c.GlobalEnabled = true
	a := &auth.Auth{UID: "u-dt-3", AccessToken: "at", Domain: "www.workbuddy.ai"}

	if _, _, _, _, err := c.globalModelsOnce(a, "/v2/enterprises/personal/models"); err != nil {
		t.Fatalf("globalModelsOnce: %v", err)
	}
	if !seen {
		t.Fatal("未发出请求")
	}
	if want := AccountDeviceToken("u-dt-3"); got != want {
		t.Fatalf("模型列表请求没带设备凭证：got=%q want=%q —— 同号在「对话」与「拉模型」上会是两套设备身份", got, want)
	}
}

// TestRefreshPathCarriesDeviceToken refresh 也必须带（同一客户端，同一设备）。
func TestRefreshPathCarriesDeviceToken(t *testing.T) {
	c := dtTestClient()
	req, _ := http.NewRequest(http.MethodPost, "http://example.invalid/refresh", nil)
	c.RefreshHeaders(req, &auth.Auth{UID: "u-dt-4", RefreshToken: "rt"})
	if got, want := req.Header.Get("X-Device-Token"), AccountDeviceToken("u-dt-4"); got != want {
		t.Fatalf("refresh 没带设备凭证：got=%q want=%q", got, want)
	}
}
