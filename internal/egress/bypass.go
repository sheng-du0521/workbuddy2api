// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// bypass.go 出口代理的「直连白名单」：本机 / 内网目标不经代理。
//
// ## 为什么需要（同类项目 2026-10-12 修过同一类 bug）
//
// 一旦把出站代理设成全局默认（`egress_proxy_default`）或给账号绑了线路，
// 代理就开始接管**所有**出站请求 —— 包括健康探测、本机回调、局域网内网端点。
// 后果有两种，都很难查：
//
//  1. 内网目标被送进代理，代理自然连不上 → 表现为「本地端点不可达」，
//     而界面上线路明明显示「正常」；
//  2. 探测类请求从代理 IP 出去 → **探测结论与真实出口不一致**，
//     排障时被自己的探测结果误导。
//
// 本文件只回答一个问题：这个主机名**该不该走代理**。纯函数、无依赖、可单测。
package egress

import (
	"net"
	"strings"
)

// bypassSuffixes 明确属于内网 / 保留命名空间的后缀（RFC 6761 / 6762）。
var bypassSuffixes = []string{
	".localhost", ".local", ".internal", ".lan", ".home", ".home.arpa", ".test", ".example",
}

// bypassHosts 容器 / 云环境里的内网别名（常见写法，非 IP 故不能靠网段判定）。
var bypassHosts = map[string]bool{
	"localhost":                true,
	"host.docker.internal":     true,
	"gateway.docker.internal":  true,
	"host.containers.internal": true,
	"kubernetes.default":       true,
	"kubernetes.default.svc":   true,
	"metadata.google.internal": true,
	"instance-data":            true,
	"host.internal":            true,
}

// cgnat 运营商级 NAT 段 100.64.0.0/10：不是 RFC1918，但同样不是公网出口。
var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// Bypass 判断该主机是否**不应**走出口代理（true = 直连）。
//
// host 可带端口、IPv6 可带方括号；两者都会被归一后再判定。
// 判定不了的主机名（普通域名）返回 false —— **不**把未知一律直连，
// 否则「代理没生效」会变成一个比原问题更难查的故障。
func Bypass(host string) bool {
	h := strings.TrimSpace(host)
	if h == "" {
		// 空主机名没有可用信息；宁可不加代理，避免把畸形请求丢给代理。
		return true
	}
	// 去端口：有端口则取主机部分；无端口（SplitHostPort 报错）保持原样。
	if hostOnly, _, err := net.SplitHostPort(h); err == nil {
		h = hostOnly
	}
	h = strings.Trim(h, "[]") // IPv6 字面量 [::1] → ::1
	// 去尾点：`printer.local.` 是 FQDN 的根形式，与 `printer.local` 等价。
	low := strings.TrimRight(strings.ToLower(h), ".")

	if bypassHosts[low] {
		return true
	}
	for _, s := range bypassSuffixes {
		if strings.HasSuffix(low, s) {
			return true
		}
	}

	ip := net.ParseIP(low)
	if ip == nil {
		return false // 普通域名：交给代理
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil && cgnat.Contains(ip4) {
		return true
	}
	return false
}

// ShouldProxy 与 Bypass 互反，供调用方按语义取用（避免到处写 !Bypass）。
func ShouldProxy(host string) bool { return !Bypass(host) }
