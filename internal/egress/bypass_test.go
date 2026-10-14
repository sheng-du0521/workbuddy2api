// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package egress

import "testing"

func TestBypassLocalAndPrivate(t *testing.T) {
	yes := []string{
		"", "localhost", "LOCALHOST", "localhost:8780", "localhost:443",
		"127.0.0.1", "127.0.0.1:8780", "127.0.0.53", "::1", "[::1]", "[::1]:8780",
		"0.0.0.0", "10.0.0.1", "10.255.255.254", "172.16.0.1", "172.31.255.1",
		"192.168.0.1", "192.168.31.1", "192.168.1.1",
		"169.254.1.1",                   // link-local
		"100.64.0.1", "100.127.255.254", // CGNAT
		"fc00::1", "fd12:3456::1", // ULA（Go 的 IsPrivate 覆盖）
		"fe80::1", // IPv6 link-local
		"host.docker.internal", "host.docker.internal:8080",
		"gateway.docker.internal", "kubernetes.default", "metadata.google.internal",
		"nas.local", "printer.local.", "svc.internal", "box.lan", "x.home.arpa",
	}
	for _, h := range yes {
		if !Bypass(h) {
			t.Errorf("Bypass(%q) = false，应为 true（不该走代理）", h)
		}
		if ShouldProxy(h) {
			t.Errorf("ShouldProxy(%q) = true，应与 Bypass 互反", h)
		}
	}
}

func TestBypassPublicStaysProxied(t *testing.T) {
	no := []string{
		"8.8.8.8", "1.1.1.1", "223.5.5.5",
		"172.32.0.1",   // 172.16/12 之外
		"172.15.0.1",   // 同上，下界之外
		"100.63.255.1", // CGNAT 之外
		"100.128.0.1",  // CGNAT 之外
		"192.169.0.1",  // 192.168/16 之外
		"11.0.0.1",     // 10/8 之外
		"2606:4700:4700::1111",
		"api.github.com", "cdn.jsdelivr.net", "copilot.tencent.com",
		"upstream.example.org:443",
	}
	for _, h := range no {
		if Bypass(h) {
			t.Errorf("Bypass(%q) = true，应为 false（公网应走代理）", h)
		}
	}
}

// 未知主机名不得被当作内网直连 —— 否则「代理没生效」会比原问题更难查。
func TestBypassUnknownHostnameNotBypassed(t *testing.T) {
	for _, h := range []string{"not-an-ip", "some-random-host", "a.b.c"} {
		if Bypass(h) {
			t.Errorf("Bypass(%q) = true，未知主机名不该直连", h)
		}
	}
}
