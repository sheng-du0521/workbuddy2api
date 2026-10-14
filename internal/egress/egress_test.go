// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package egress

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestValidate 逐类非法输入都要被挡下，且错误信息要能指导修复。
func TestValidate(t *testing.T) {
	ok := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"   ", ""},
		{"http://1.2.3.4:8080", "http://1.2.3.4:8080"},
		{"https://user:pass@proxy.example.com:3128", "https://user:pass@proxy.example.com:3128"},
		{"socks5://127.0.0.1:1080", "socks5://127.0.0.1:1080"},
		{"  http://1.2.3.4:8080  ", "http://1.2.3.4:8080"}, // 首尾空白被裁剪
		{"HTTP://1.2.3.4:8080", "HTTP://1.2.3.4:8080"},     // 协议大小写不敏感
	}
	for _, c := range ok {
		got, err := Validate(c.in)
		if err != nil {
			t.Errorf("Validate(%q) 应通过，得错误 %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("Validate(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}

	bad := []struct {
		in      string
		mustSay string
	}{
		{"socks5h://1.2.3.4:1080", "socks5h"},
		{"ftp://1.2.3.4:21", "不支持的代理协议"},
		{"http://1.2.3.4", "缺少端口"},
		{"http://1.2.3.4:99999", "端口"},
		{"http://:8080", "主机名"},
		{"1.2.3.4:8080", "缺少协议前缀"},
		{"just-a-string", "缺少协议前缀"},
	}
	for _, c := range bad {
		_, err := Validate(c.in)
		if err == nil {
			t.Errorf("Validate(%q) 应被拒绝", c.in)
			continue
		}
		if !strings.Contains(err.Error(), c.mustSay) {
			t.Errorf("Validate(%q) 错误信息 %q 应包含 %q", c.in, err.Error(), c.mustSay)
		}
	}
}

// TestFetchIPExtract 各端点返回格式不同，统一按 IPv4 正则抽取。
func TestFetchIPExtract(t *testing.T) {
	cases := []struct {
		name string
		body string
		code int
		want string
		fail bool
	}{
		{"纯 IP", "203.0.113.7\n", 200, "203.0.113.7", false},
		{"带中文前缀（ipip.net）", "当前 IP：203.0.113.7  来自于：中国 广东 深圳 电信\n", 200, "203.0.113.7", false},
		{"JSON（ipify 风格）", `{"ip":"203.0.113.7"}`, 200, "203.0.113.7", false},
		{"IPv6 优先但含 v4", "203.0.113.7", 200, "203.0.113.7", false},
		{"无 IP", "hello world", 200, "", true},
		{"非 200", "203.0.113.7", 503, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.code)
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()
			cl, err := clientFor("")
			if err != nil {
				t.Fatalf("clientFor: %v", err)
			}
			got, err := fetchIP(cl, srv.URL)
			if c.fail {
				if err == nil {
					t.Fatalf("应失败，得 %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("fetchIP: %v", err)
			}
			if got != c.want {
				t.Fatalf("抽出 %q，期望 %q", got, c.want)
			}
		})
	}
}

// TestProbeRejectsInvalidURL 格式非法时不应浪费网络请求，直接返回错误。
func TestProbeRejectsInvalidURL(t *testing.T) {
	r := Probe("socks5h://1.2.3.4:1080")
	if r.OK {
		t.Fatal("非法 URL 不应探测成功")
	}
	if !strings.Contains(r.Error, "socks5h") {
		t.Fatalf("应指出 socks5h 不受支持，得 %q", r.Error)
	}
	if r.Endpoint != "" {
		t.Fatalf("未发起请求时不应有端点，得 %q", r.Endpoint)
	}
}

// TestProbeBoundedLatency 探活必须有耗时上界。
//
// 回归用例：此前代理探测失败后会遍历全部候选端点逐个重试，实测单次探活耗时
// 40 秒 —— 管理员点一次「探活」要等半分钟，接口还容易被前端超时切断。
// 现在 pickEndpoint 已证明端点直连可达，失败即归因代理，单次上限 ≈ 6s；
// 即便端点全部不可达，也受 probeBudget(12s) 约束。
func TestProbeBoundedLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("涉及真实网络等待，-short 下跳过")
	}
	// 192.0.2.0/24 是 RFC 5737 保留的测试网段，必定不可达。
	start := time.Now()
	r := Probe("http://192.0.2.1:8080")
	elapsed := time.Since(start)

	if elapsed > 20*time.Second {
		t.Fatalf("探活耗时 %v 超出上界（预期 ≤ 12s 预算 + 余量）", elapsed)
	}
	if r.OK {
		t.Fatal("不可达代理不应探活成功")
	}
	if r.Error == "" {
		t.Fatal("失败必须带原因，否则管理员无从排查")
	}
}

// TestProbeInvalidURLIsFast 格式非法时不应发起任何网络请求（快速失败）。
func TestProbeInvalidURLIsFast(t *testing.T) {
	start := time.Now()
	r := Probe("http://192.0.2.1") // 缺端口
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("格式校验失败应立即返回，实际耗时 %v", elapsed)
	}
	if r.OK || !strings.Contains(r.Error, "端口") {
		t.Fatalf("应指出缺少端口，得 ok=%v err=%q", r.OK, r.Error)
	}
}

// TestIsDirect 直连哨兵的识别（大小写 / 空白容忍，且不误判普通代理）。
func TestIsDirect(t *testing.T) {
	yes := []string{"direct", "DIRECT", " Direct ", "DiReCt"}
	for _, s := range yes {
		if !IsDirect(s) {
			t.Errorf("IsDirect(%q) 应为 true", s)
		}
	}
	no := []string{"", "http://1.2.3.4:8080", "socks5://1.2.3.4:1080", "directt", "direct://1.2.3.4"}
	for _, s := range no {
		if IsDirect(s) {
			t.Errorf("IsDirect(%q) 应为 false", s)
		}
	}
}

// TestValidateAccountEgressAcceptsDirect 账号级校验接受 direct，但池级 Validate 不接受。
//
// 这个区分很重要：代理池里存 "direct" 毫无意义（池是「线路清单」），
// 若放宽 Validate，invalid 的池条目会一路流到建客户端时才炸。
func TestValidateAccountEgressAcceptsDirect(t *testing.T) {
	if got, err := ValidateAccountEgress("direct"); err != nil || got != Direct {
		t.Fatalf("账号级应接受 direct，得 %q/%v", got, err)
	}
	if _, err := Validate("direct"); err == nil {
		t.Fatal("池级 Validate 不应接受 direct（它不是代理 URL）")
	}
}

// TestValidateAccountEgressRejectsLookalikes 「想直连但写错」要给可用提示。
func TestValidateAccountEgressRejectsLookalikes(t *testing.T) {
	for _, s := range []string{"none", "off", "nil", "null", "-"} {
		_, err := ValidateAccountEgress(s)
		if err == nil {
			t.Errorf("ValidateAccountEgress(%q) 应被拒绝", s)
			continue
		}
		if !strings.Contains(err.Error(), Direct) {
			t.Errorf("错误提示应引导使用 %q，得 %q", Direct, err.Error())
		}
	}
}

// TestClientForUsesProxy 走代理的 client 必须真的配置了 Proxy，
// 否则「配了代理却直连」这个 bug 会以另一种形式复现。
func TestClientForUsesProxy(t *testing.T) {
	c, err := clientFor("http://127.0.0.1:18080")
	if err != nil {
		t.Fatalf("clientFor: %v", err)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatal("Transport 类型不符")
	}
	if tr.Proxy == nil {
		t.Fatal("配置了代理 URL 时 Transport.Proxy 不应为 nil")
	}
	req, _ := http.NewRequest(http.MethodGet, "http://example.com", nil)
	u, err := tr.Proxy(req)
	if err != nil {
		t.Fatalf("解析代理失败: %v", err)
	}
	if u == nil || u.Host != "127.0.0.1:18080" {
		t.Fatalf("代理地址应为 127.0.0.1:18080，得 %v", u)
	}
}

// TestClientForDirect 空代理 → 直连（Proxy 为 nil，走环境变量兜底）。
func TestClientForDirect(t *testing.T) {
	c, err := clientFor("")
	if err != nil {
		t.Fatalf("clientFor: %v", err)
	}
	tr := c.Transport.(*http.Transport)
	if tr.Proxy != nil {
		t.Fatal("空代理 URL 时不应设置 Proxy")
	}
}

// TestReachableCacheDefaultLife 探活端点缓存的默认 TTL 必须在**零值之外**就绪。
//
// v0.24.2（P3-15）：此前这个默认值由包级 `func init() { reachableCache.life = 10*time.Minute }`
// 赋值 —— 「导入本包即改全局状态」，且生命值藏在 init 里对读者不可见。
// 现在写进复合字面量。本用例锁住「默认值不再依赖 init」：
// 忘了带默认值的话，life 为 0，缓存恒视为过期，每次探活都重新试一轮候选端点。
func TestReachableCacheDefaultLife(t *testing.T) {
	if reachableCache.life <= 0 {
		t.Fatalf("reachableCache.life = %v，必须由字面量给出默认值（不再靠 init）", reachableCache.life)
	}
}
