// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"bytes"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// 本文件覆盖 v0.24.1 的 P3 安全整改（见 docs/项目全面审查报告-v0.23.3.md 的 P3 清单）：
//
//	P3-3  熵源故障时**拒绝**生成凭据（此前静默降级为时间戳派生）
//	P3-5  限流按客户端**真实** IP（隧道下此前全站共享一个桶）+ 桶回收
//	P3-2  会话 Cookie 的 Secure 属性（https 下加、纯 HTTP 本地不加）
//	P3-6  跨站写请求（Origin/Referer 校验）
//	P3-4  注册密码强度（与改密同一套规则）+ bcrypt cost

// ---------- P3-3 熵源故障不降级 ----------

// withFailingEntropy 临时把熵源换成「恒失败」，并在用例结束还原。
//
// 这是 secureBytes 那个「panic 而不是降级」契约**可被测试**的前提：
// 契约若无法注入故障，就无法被守住 —— 后人改回「失败就用时间戳」时不会有任何测试报警。
func withFailingEntropy(t *testing.T) {
	t.Helper()
	old := randRead
	randRead = func([]byte) (int, error) { return 0, errors.New("entropy unavailable (test)") }
	t.Cleanup(func() { randRead = old })
}

// TestSecureBytesRefusesOnEntropyFailure 熵源故障时必须 panic，**绝不**返回弱随机值。
//
// 缺陷背景（P3-3）：此前会话 token / API Key / 兑换码 / 邀请码 / 消息 ID 在
// crypto/rand 失败时会回落到时间戳派生 —— 不报错、不告警，静默产出可枚举的凭据。
// 会话 token 可枚举 = 账号可被直接接管。
func TestSecureBytesRefusesOnEntropyFailure(t *testing.T) {
	withFailingEntropy(t)

	cases := []struct {
		name string
		gen  func() string
	}{
		{"secureBytes", func() string { return string(secureBytes(16)) }},
		{"randomHex(会话令牌)", func() string { return randomHex(32) }},
		{"newRandomID(管理密钥/uid)", func() string { return newRandomID(24) }},
		{"GenerateAPIKey", func() string { return GenerateAPIKey() }},
		{"generateRedeemCode", func() string { return generateRedeemCode() }},
		{"randByte(邀请码)", func() string { return string([]byte{randByte()}) }},
	}
	for _, c := range cases {
		func() {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("%s：熵源故障时必须 panic（拒绝生成凭据），却正常返回了值 —— "+
						"这正是 P3-3 要消除的静默降级", c.name)
				}
			}()
			got := c.gen()
			// 走到这里说明没 panic；把值打出来便于定位「降级成了什么」。
			t.Logf("%s 返回了 %q（本不该有返回值）", c.name, got)
		}()
	}
}

// TestSecureBytesNormal 正常熵源下的基本性质：长度正确、两次不同、是十六进制可解析的。
func TestSecureBytesNormal(t *testing.T) {
	b := secureBytes(32)
	if len(b) != 32 {
		t.Fatalf("长度应为 32，得 %d", len(b))
	}
	if string(b) == string(secureBytes(32)) {
		t.Fatal("两次调用返回相同字节（熵源被写成常量？）")
	}
	h := randomHex(16)
	if len(h) != 32 {
		t.Fatalf("randomHex(16) 应为 32 个 hex 字符，得 %d", len(h))
	}
	if _, err := hex.DecodeString(h); err != nil {
		t.Fatalf("randomHex 输出不是合法 hex：%v", err)
	}
	if !strings.HasPrefix(GenerateAPIKey(), "sk-wb-") {
		t.Fatal("API Key 前缀不对")
	}
	if !strings.HasPrefix(generateRedeemCode(), "WB-") {
		t.Fatal("兑换码前缀不对")
	}
}

// TestRandomHexNotTimeDerived 熵源正常时，连续生成的值不应呈现「时间递增」形态 ——
// 这是对 P3-3 旧实现（"%x" % UnixNano()）的**正面反例**。
//
// 时间戳派生的特征很明确：同一毫秒内的两次调用会得到相同值，且高位字节长期不变。
// 真随机则几乎必然不同。断言不做统计检验，只挡住「明显退化回旧实现」。
func TestRandomHexNotTimeDerived(t *testing.T) {
	a, b := randomHex(8), randomHex(8)
	if a == b {
		t.Fatalf("连续两次 randomHex 相同（%q）—— 疑似退化成时间戳派生", a)
	}
	// 旧实现的值一定以时间戳的高位 hex 开头（同一天内前几位恒定）。
	prefix := randomHex(4)[:2]
	same := 0
	for i := 0; i < 32; i++ {
		if randomHex(4)[:2] == prefix {
			same++
		}
	}
	if same > 24 {
		t.Errorf("32 次采样中首字节有 %d 次相同 —— 分布可疑（时间戳派生特征）", same)
	}
}

// ---------- P3-5 客户端真实 IP ----------

// TestClientIP 取值优先级与「不可信对端忽略转发头」。
func TestClientIP(t *testing.T) {
	cases := []struct {
		name   string
		peer   string
		hdrs   map[string]string
		spec   string // -trusted-proxies
		want   string
		reason string
	}{
		{
			name: "公网直连：转发头一律忽略",
			peer: "203.0.113.9:5555",
			hdrs: map[string]string{
				"CF-Connecting-IP": "1.2.3.4",
				"X-Forwarded-For":  "1.2.3.4",
				"X-Real-IP":        "1.2.3.4",
			},
			want:   "203.0.113.9",
			reason: "对端不可信时伪造 XFF 必须无效，否则限速可被绕过",
		},
		{
			name: "隧道（loopback 对端）：优先 CF-Connecting-IP",
			peer: "127.0.0.1:4433",
			hdrs: map[string]string{
				"CF-Connecting-IP": "198.51.100.7",
				"X-Forwarded-For":  "198.51.100.9",
			},
			want:   "198.51.100.7",
			reason: "CF-Connecting-IP 由 Cloudflare 覆写，客户端无法伪造",
		},
		{
			name:   "隧道：无 CFCIP 时取 XFF 最右的公网地址",
			peer:   "127.0.0.1:4433",
			hdrs:   map[string]string{"X-Forwarded-For": "10.0.0.5, 198.51.100.7, 127.0.0.1"},
			want:   "198.51.100.7",
			reason: "代理链是追加语义；取最右公网地址可绕开客户端自己塞在左侧的假值",
		},
		{
			name:   "隧道：客户端伪造在左侧的假值不会赢",
			peer:   "127.0.0.1:4433",
			hdrs:   map[string]string{"X-Forwarded-For": "1.1.1.1, 198.51.100.7"},
			want:   "198.51.100.7",
			reason: "最右侧才是代理追加的真实值",
		},
		{
			name:   "隧道：XFF 全是私有地址时回落对端",
			peer:   "127.0.0.1:4433",
			hdrs:   map[string]string{"X-Forwarded-For": "10.1.2.3, 192.168.1.1"},
			want:   "127.0.0.1",
			reason: "宁可并回对端，也不把伪造的私有地址当客户端（那等于开了任意分桶的口子）",
		},
		{
			name:   "隧道：X-Real-IP 仅公网地址才采信",
			peer:   "127.0.0.1:4433",
			hdrs:   map[string]string{"X-Real-IP": "192.168.9.9"},
			want:   "127.0.0.1",
			reason: "私有地址不采信",
		},
		{
			name: "隧道：X-Real-IP 公网地址可用",
			peer: "127.0.0.1:4433",
			hdrs: map[string]string{"X-Real-IP": "198.51.100.7"},
			want: "198.51.100.7",
		},
		{
			name: "显式可信网段内的对端也采信转发头",
			peer: "10.9.8.7:5555",
			hdrs: map[string]string{"X-Forwarded-For": "198.51.100.7"},
			spec: "10.0.0.0/8",
			want: "198.51.100.7",
		},
		{
			name:   "无端口形态的 RemoteAddr 也能取到",
			peer:   "127.0.0.1",
			want:   "127.0.0.1",
			reason: "SplitHostPort 失败时不能回落到空串（空串会把所有请求并进一个桶）",
		},
	}
	for _, c := range cases {
		set, err := parseIPSet(c.spec)
		if err != nil {
			t.Fatalf("%s: parseIPSet: %v", c.name, err)
		}
		a := &app{trustedProxies: set}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = c.peer
		for k, v := range c.hdrs {
			r.Header.Set(k, v)
		}
		if got := a.clientIP(r); got != c.want {
			t.Errorf("%s: clientIP=%q want %q（%s）", c.name, got, c.want, c.reason)
		}
	}
}

// TestParseIPSet 网段解析：裸 IP、CIDR、IPv6、空、非法。
func TestParseIPSet(t *testing.T) {
	if s, err := parseIPSet(""); err != nil || s != nil {
		t.Fatalf("空串应返回 (nil,nil)，得 (%v,%v)", s, err)
	}
	s, err := parseIPSet("10.0.0.0/8, 192.0.2.5 , ::1")
	if err != nil {
		t.Fatalf("合法列表解析失败：%v", err)
	}
	if len(s.nets) != 3 {
		t.Fatalf("应有 3 段，得 %d", len(s.nets))
	}
	if !s.contains(parseIPOrNil("10.1.2.3")) {
		t.Error("10.1.2.3 应命中 10.0.0.0/8")
	}
	if !s.contains(parseIPOrNil("192.0.2.5")) {
		t.Error("裸 IP 应按 /32 处理")
	}
	if s.contains(parseIPOrNil("192.0.2.6")) {
		t.Error("192.0.2.6 不该命中")
	}
	if _, err := parseIPSet("not-a-cidr"); err == nil {
		t.Error("非法网段应报错（否则会被静默忽略、可信边界与预期不符）")
	}
	// nil 集合是合法的（= 只信 loopback），contains 必须 nil-safe。
	var nilSet *ipSet
	if nilSet.contains(parseIPOrNil("10.0.0.1")) {
		t.Error("nil 集合不应命中任何地址")
	}
}

func parseIPOrNil(s string) net.IP { return net.ParseIP(s) }

// TestRateLimiterGCRemovesExpiredBuckets 过期桶要被回收（此前只增不减）。
func TestRateLimiterGCRemovesExpiredBuckets(t *testing.T) {
	l := newRateLimiter(5, 50*time.Millisecond)
	if !l.allow("1.1.1.1") {
		t.Fatal("首次应放行")
	}
	if len(l.hits) != 1 {
		t.Fatalf("应有 1 个桶，得 %d", len(l.hits))
	}
	// 让桶过期，然后触发下一次 allow（带 GC）。
	time.Sleep(60 * time.Millisecond)
	if !l.allow("2.2.2.2") {
		t.Fatal("第二个 IP 应放行")
	}
	if _, ok := l.hits["1.1.1.1"]; ok {
		t.Errorf("过期桶应被回收，当前桶：%v（长期运行会无界增长）", keysOf(l.hits))
	}
	if _, ok := l.hits["2.2.2.2"]; !ok {
		t.Error("活跃桶不该被误删")
	}
}

func keysOf(m map[string][]time.Time) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ---------- P3-2 Cookie Secure ----------

// TestCookieSecure Cookie 安全属性的判定矩阵。
func TestCookieSecure(t *testing.T) {
	cases := []struct {
		name     string
		force    bool
		peer     string
		proto    string
		tls      bool
		want     bool
		whyShort string
	}{
		{name: "显式开启 → 恒 true", force: true, peer: "203.0.113.9:1", want: true},
		{name: "本机 HTTP 直连 → false", peer: "127.0.0.1:1234", want: false,
			whyShort: "无条件加 Secure 会让本地登录直接失效"},
		{name: "隧道声明 https → true", peer: "127.0.0.1:1234", proto: "https", want: true,
			whyShort: "否则会话 Cookie 会随明文请求发出、可被中间人抓取"},
		{name: "隧道声明 HTTPS（大小写）→ true", peer: "127.0.0.1:1234", proto: "HTTPS", want: true},
		{name: "隧道声明 http → false", peer: "127.0.0.1:1234", proto: "http", want: false},
		{name: "不可信对端声明 https → false", peer: "203.0.113.9:1", proto: "https", want: false,
			whyShort: "转发头只在对端可信时才采信"},
		{name: "TLS 直连 → true", peer: "203.0.113.9:1", tls: true, want: true},
	}
	for _, c := range cases {
		a := &app{secureCookies: c.force}
		r := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
		r.RemoteAddr = c.peer
		if c.proto != "" {
			r.Header.Set("X-Forwarded-Proto", c.proto)
		}
		if c.tls {
			r.TLS = &tls.ConnectionState{}
		}
		if got := a.cookieSecure(r); got != c.want {
			t.Errorf("%s: cookieSecure=%v want %v（%s）", c.name, got, c.want, c.whyShort)
		}
	}
}

// ---------- P3-6 CSRF ----------

// TestCsrfGuard 跨站写请求的判据矩阵（打真中间件 + 真 mux）。
func TestCsrfGuard(t *testing.T) {
	store, a, mux, tokens := newAuthzApp(t)
	defer store.Close() // 不关会让 t.TempDir 清理报「文件被占用」
	guard := a.csrfGuard(mux)
	h := securityHeaders(limitRequestBody(guard))

	do := func(method, path, origin, referer, adminKey string) int {
		req := httptest.NewRequest(method, path, nil)
		req.Host = "example.test"
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: tokens["plain"]})
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if referer != "" {
			req.Header.Set("Referer", referer)
		}
		if adminKey != "" {
			req.Header.Set("X-Admin-Key", adminKey)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := do(http.MethodGet, "/api/auth/me", "https://evil.test", "", ""); code != http.StatusOK {
		t.Errorf("GET 不该被 CSRF 校验拦（安全方法），got %d", code)
	}
	if code := do(http.MethodPost, "/api/auth/logout", "https://evil.test", "", ""); code != http.StatusForbidden {
		t.Errorf("跨站 POST（Origin 不匹配）应 403，got %d", code)
	}
	if code := do(http.MethodPost, "/api/auth/logout", "http://example.test", "", ""); code == http.StatusForbidden {
		t.Errorf("同源 POST 不该被拦，got %d", code)
	}
	if code := do(http.MethodPost, "/api/auth/logout", "", "http://example.test/some/page", ""); code == http.StatusForbidden {
		t.Errorf("Referer 同源应放行（Origin 缺失时的兜底），got %d", code)
	}
	if code := do(http.MethodPost, "/api/auth/logout", "", "https://evil.test/x", ""); code != http.StatusForbidden {
		t.Errorf("Referer 跨站应 403，got %d", code)
	}
	if code := do(http.MethodPost, "/api/auth/logout", "", "", ""); code == http.StatusForbidden {
		t.Errorf("无 Origin/Referer 应放行（非浏览器客户端；浏览器跨站必带 Origin），got %d", code)
	}
	if code := do(http.MethodPost, "/api/auth/logout", "https://evil.test", "", "test-admin-key-xyz"); code == http.StatusForbidden {
		t.Errorf("X-Admin-Key 头鉴权通道不该被 CSRF 拦（无环境凭据、无法被跨站伪造），got %d", code)
	}
}

// TestSameHostAsRequest host 比较的细节。
func TestSameHostAsRequest(t *testing.T) {
	cases := []struct {
		src, host string
		want      bool
	}{
		{"https://example.test", "example.test", true},
		{"http://example.test", "example.test", true},
		{"https://EXAMPLE.test", "example.test", true},
		{"https://example.test:443", "example.test", false}, // 端口不同即不同源（保守）
		{"https://example.test", "example.test:8080", false},
		{"https://evil.test", "example.test", false},
		{"not a url", "example.test", false},
		{"", "example.test", false},
	}
	for _, c := range cases {
		if got := sameHostAsRequest(c.src, c.host); got != c.want {
			t.Errorf("sameHostAsRequest(%q,%q)=%v want %v", c.src, c.host, got, c.want)
		}
	}
}

// ---------- P3-4 注册密码强度 ----------

// TestRegisterRejectsWeakPassword 注册必须过与改密同一套强度规则。
//
// 缺陷背景：注册此前只查长度，`aaaaaaaa` 能注册；而改密早就有强度校验 ——
// 两个入口各写一半，**开放注册那个口子反而最松**。
func TestRegisterRejectsWeakPassword(t *testing.T) {
	store, a, mux, _ := newAuthzApp(t)
	defer store.Close()
	// 限流器由 main() 装配，newTestApp 不建（避免影响其它用例的调用次数）。
	// 这里显式补上：**刻意不加 nil 兜底** —— 生产里它恒非 nil，
	// 若哪天漏装配，宁可当场 panic（500）也不要静默「不限流」。
	a.registerLimiter = newRateLimiter(1000, time.Hour)
	a.loginLimiter = newRateLimiter(1000, time.Hour)

	// 注册接口吃 JSON（不是表单）。用错格式会让**每个**用例都返回
	// "请求格式错误" 的 400 —— 断言照样"通过"，但通过的理由是错的。
	// 所以下面除了状态码，还断言错误**内容**确实指向密码强度。
	post := func(username, password string) (int, string) {
		body, _ := json.Marshal(map[string]string{"username": username, "password": password})
		req := httptest.NewRequest(http.MethodPost, "/api/auth/register", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	weak := []struct{ name, user, pass, wantContains string }{
		{"纯小写字母长度够但单类", "weak1", "aaaaaaaa", "密码"},
		{"纯数字", "weak2", "12345678", "密码"},
		{"纯符号", "weak3", "!!!!!!!!", "密码"},
		// 用户名=密码：这条**只**能被"同名"规则挡住（它是三类字符混合、长度也够），
		// 是专门用来验证那条规则真的生效的用例。
		{"用户名即密码", "Abcdefg1", "Abcdefg1", "用户名"},
		{"过短", "weak4", "a1", "密码"},
	}
	for _, c := range weak {
		code, body := post(c.user, c.pass)
		if code != http.StatusBadRequest {
			t.Errorf("%s（%s/%s）：应 400，got %d %s", c.name, c.user, c.pass, code, body)
			continue
		}
		// 关键：确认拒绝的理由是**强度**，而不是「请求格式错误」之类的旁因。
		if strings.Contains(body, "请求格式错误") {
			t.Errorf("%s：请求格式有误（用例写错了，不是被测代码的问题）", c.name)
			continue
		}
		if c.wantContains != "" && !strings.Contains(body, c.wantContains) {
			t.Errorf("%s：错误信息应提到 %q，got %s", c.name, c.wantContains, body)
		}
	}

	// 合理强度的密码仍可注册（不能把门堵死）。
	code, body := post("okuser1", "Str0ngPassw0rd!")
	if code != http.StatusOK && code != http.StatusCreated {
		t.Errorf("合理密码应注册成功，got %d %s", code, body)
	}
	_ = a
}

// TestValidatePasswordStrengthRules 强度规则本身（与改密路径共用，改一处即两处生效）。
func TestValidatePasswordStrengthRules(t *testing.T) {
	if err := validatePasswordStrength("bob", "Str0ngPassw0rd", ""); err != nil {
		t.Errorf("合规密码不该被拒：%v", err)
	}
	if err := validatePasswordStrength("bob", "aaaaaaaa", ""); err == nil {
		t.Error("纯小写 8 位应被拒")
	}
	if err := validatePasswordStrength("bob", "bob", ""); err == nil {
		t.Error("过短应被拒")
	}
	if err := validatePasswordStrength("Bob", "bob", ""); err == nil {
		t.Error("与用户名相同（忽略大小写）应被拒")
	}
}

// TestBcryptCostRaised 注册/改密使用提升后的 cost，且旧 hash（cost=10）仍可验证。
func TestBcryptCostRaised(t *testing.T) {
	if bcryptCost <= 10 {
		t.Fatalf("bcryptCost 应高于库默认 10，得 %d", bcryptCost)
	}
	// 旧 hash 兼容性：以旧 cost 生成的 hash 必须仍能通过比较（否则老用户登不上）。
	oldHash, err := bcrypt.GenerateFromPassword([]byte("legacy-pass"), 10)
	if err != nil {
		t.Fatalf("生成旧 hash 失败：%v", err)
	}
	if err := bcrypt.CompareHashAndPassword(oldHash, []byte("legacy-pass")); err != nil {
		t.Fatalf("旧 cost 的 hash 必须仍可验证（否则等于强制全量改密）：%v", err)
	}
}
