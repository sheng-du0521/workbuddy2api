// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// 本文件收拢「请求来源与来源可信度」相关的三件事（v0.24.1，对应审查报告 P3-2/5/6）：
//
//	clientIP       —— 取客户端**真实** IP（隧道下不能只看 RemoteAddr）
//	cookieSecure   —— 会话 Cookie 何时该带 Secure
//	csrfGuard      —— 阻止跨站发起的写请求
//
// 三者的共同前提是「判断对端可不可信」，所以放在一起：判据只有一处，
// 才不会出现「取 IP 时信了转发头、判 Cookie 时又没信」这类自相矛盾。

// ipSet 一组网段（用于「可信对端」判定）。nil / 空 = 只信 loopback。
type ipSet struct {
	nets []*net.IPNet
}

// parseIPSet 解析逗号分隔的 CIDR 列表；单个 IP 会按 /32（或 /128）处理。
// 传空串返回 nil（调用方语义：只信 loopback）。
func parseIPSet(spec string) (*ipSet, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	set := &ipSet{}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.Contains(part, "/") {
			// 裸 IP：按主机位全掩码处理。
			ip := net.ParseIP(part)
			if ip == nil {
				return nil, &badCIDRError{part}
			}
			if ip.To4() != nil {
				part += "/32"
			} else {
				part += "/128"
			}
		}
		_, n, err := net.ParseCIDR(part)
		if err != nil {
			return nil, &badCIDRError{part}
		}
		set.nets = append(set.nets, n)
	}
	return set, nil
}

type badCIDRError struct{ spec string }

func (e *badCIDRError) Error() string { return "非法网段: " + e.spec }

// contains 判定 IP 是否落在集合内。
func (s *ipSet) contains(ip net.IP) bool {
	if s == nil || ip == nil {
		return false
	}
	for _, n := range s.nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// hostOnly 从 "host:port" 中取 host；无端口时原样返回。
func hostOnly(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

// isLoopbackIP loopback 判定（127.0.0.0/8 与 ::1）。
func isLoopbackIP(ip net.IP) bool {
	return ip != nil && ip.IsLoopback()
}

// isPrivateIP 判定「非公网可达」地址：私有段、loopback、link-local、ULA、未指定。
//
// 用途是**反向**的：在 X-Forwarded-For 里从右往左找「第一个公网地址」——
// 私有地址多半是链路中间的代理自己加的，不是真正的客户端。
func isPrivateIP(ip net.IP) bool {
	if ip == nil {
		return true // 解析失败一律当私有（不可用）处理，宁保守不放宽
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return true
	}
	// IPv6 唯一本地地址 fc00::/7（IsPrivate 只覆盖 IPv4 私有段与 IPv6 ULA 的部分实现，
	// 这里显式兜一层，避免不同 Go 版本行为差异）。
	if ip.To4() == nil {
		if len(ip) == net.IPv6len && (ip[0]&0xfe) == 0xfc {
			return true
		}
	}
	return false
}

// trustedProxy 判定对端是否可信（可信才采信它带来的转发头）。
//
// **默认只信 loopback**：本项目生产形态是 cloudflared 同机隧道（对端恒为 127.0.0.1），
// 以及「nginx/其他反代跑在本机」的常见部署。只有本机进程才能以 loopback 身份连进来，
// 所以这个默认值是安全的 —— 公网来源无论伪造什么头都只会用自己的 RemoteAddr 计数。
//
// 若前置了非同机代理，用 `-trusted-proxies` 显式加网段。
// **不要**填 0.0.0.0/0：那等于允许任何来源伪造 IP，限速与审计就都失效了。
func (a *app) trustedProxy(peer string) bool {
	ip := net.ParseIP(hostOnly(peer))
	if ip == nil {
		return false
	}
	if isLoopbackIP(ip) {
		return true
	}
	return a.trustedProxies.contains(ip)
}

// clientIP 取客户端真实 IP。
//
// 背景（v0.24.1 修的真实可用性缺陷，审查报告 P3-5）：此前只看 RemoteAddr。
// 隧道下**所有公网用户的对端都是 127.0.0.1** —— 于是：
//   - 全站共享一个登录限流桶：一个人连错 30 次密码，所有人 15 分钟登不上；
//   - 注册桶同理（10 次/小时），一个人就能把全站注册关掉；
//   - API 维度限流（按 IP 那条）也退化成全局桶。
//
// 取值优先级（仅当对端可信时才会读这些头）：
//
//  1. CF-Connecting-IP —— Cloudflare 会**覆写**此头，客户端无法伪造；
//  2. X-Forwarded-For 中**最右一个公网地址** —— 代理链是追加语义，客户端自己塞在
//     左侧的假值不会赢过代理追加的真实值；
//  3. X-Real-IP（同样是公网地址才采信）；
//  4. 对端地址。
//
// 绝不回落到空串：空串会让**所有**请求并进同一个桶，正是本次要修的那个问题。
func (a *app) clientIP(r *http.Request) string {
	peer := hostOnly(r.RemoteAddr)
	if peer == "" {
		peer = r.RemoteAddr
	}
	if !a.trustedProxy(peer) {
		return peer
	}
	if ip := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); net.ParseIP(ip) != nil {
		return ip
	}
	if ip := rightmostPublicIP(r.Header.Get("X-Forwarded-For")); ip != "" {
		return ip
	}
	if ip := strings.TrimSpace(r.Header.Get("X-Real-IP")); ip != "" {
		if parsed := net.ParseIP(ip); parsed != nil && !isPrivateIP(parsed) {
			return ip
		}
	}
	return peer
}

// rightmostPublicIP 从 X-Forwarded-For 里取最右侧的公网地址（无则空串）。
func rightmostPublicIP(xff string) string {
	parts := strings.Split(xff, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		v := strings.TrimSpace(parts[i])
		if v == "" {
			continue
		}
		ip := net.ParseIP(v)
		if ip == nil {
			continue
		}
		if !isPrivateIP(ip) {
			return ip.String()
		}
	}
	return ""
}

// ---------- Cookie 安全属性 ----------

// cookieSecure 判断本次响应里的会话 Cookie 是否该带 Secure（v0.24.1，P3-2）。
//
// 为什么不能无条件加 Secure：本项目默认跑在**纯 HTTP 的本机 8780** 上，
// 无条件加会让本地登录直接失效（浏览器不回传 Secure Cookie），
// 这是「修了安全问题、功能坏了」的典型误伤。
//
// 为什么必须有条件加：经隧道（https）访问时，不带 Secure 的会话 Cookie 会随
// 明文请求一起发出，可被中间人抓取并直接冒用会话。
//
// 判据（可信度从高到低）：
//  1. `-secure-cookies` 显式开启 → 恒 true（确认全链路 https 时用它，最稳）；
//  2. 请求本身是 TLS（r.TLS != nil）→ true；
//  3. 对端可信且 X-Forwarded-Proto=https（隧道 / 同机反代场景）→ true；
//  4. 其余 → false。
func (a *app) cookieSecure(r *http.Request) bool {
	if a.secureCookies {
		return true
	}
	if r.TLS != nil {
		return true
	}
	if a.trustedProxy(hostOnly(r.RemoteAddr)) {
		return strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
	}
	return false
}

// ---------- CSRF ----------

// csrfGuard 阻止「跨站发起的写请求」（v0.24.1，P3-6）。
//
// 背景：会话是 Cookie（浏览器自动携带），此前只靠 `SameSite=Lax` 兜底。
// Lax 能挡住多数跨站 POST，但**挡不住**同站子域、也挡不住某些浏览器/老旧客户端的
// 行为差异；而这里的写操作包含「改密码、提现、买卖账号、发兑换码」——
// 一次 CSRF 成功就是直接的资金/账号损失，值得独立一道闸。
//
// 判据（Origin/Referer 校验，业界标准的无 token CSRF 防护）：
//
//	· 用**请求头鉴权**的通道直接放行 —— `X-Admin-Key` 与 `Authorization: Bearer`
//	  不是浏览器自动携带的凭据，跨站请求无法伪造（要带就得先过 CORS 预检），
//	  不存在 CSRF 面。放行它们也保证了运维脚本 / API 客户端不受影响。
//	· 其余（Cookie 会话 + 未鉴权）请求：GET/HEAD/OPTIONS 是安全方法，放行。
//	· 写方法：若带 `Origin` 或 `Referer`，其 host 必须与 `Host` 一致；
//	  两者都没有时**放行** —— 浏览器在跨站写请求上必带 Origin，
//	  所以「两个都没有」实际是非浏览器客户端（curl/脚本），拦它只会误伤。
//
// 这一判据的已知边界（如实写在文档里）：不防「同源内的 XSS」——
// XSS 能在同源下发任意请求，Origin 校验对它无效；那条防线是输出转义。
func (a *app) csrfGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if csrfSafeRequest(r) {
			next.ServeHTTP(w, r)
			return
		}
		origin := strings.TrimSpace(r.Header.Get("Origin"))
		ref := strings.TrimSpace(r.Header.Get("Referer"))
		if origin == "" && ref == "" {
			// 非浏览器客户端：无来源信息可校验，按放行处理（见函数注释）。
			next.ServeHTTP(w, r)
			return
		}
		src := origin
		if src == "" {
			src = ref
		}
		if !sameHostAsRequest(src, r.Host) {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error": "请求来源校验失败（可能的跨站请求）",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// csrfSafeRequest 该请求是否天然不受 CSRF 影响（安全方法 or 头鉴权）。
func csrfSafeRequest(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	if strings.TrimSpace(r.Header.Get("X-Admin-Key")) != "" {
		return true
	}
	if strings.HasPrefix(strings.TrimSpace(r.Header.Get("Authorization")), "Bearer ") {
		return true
	}
	return false
}

// sameHostAsRequest 判定来源 URL 的 host 是否与请求 Host 一致（含端口）。
//
// 同时接受 http / https 两种 scheme：隧道下浏览器看到的是 https，
// 而应用自己（r.Host）只有域名 —— scheme 不参与比较，只比 host:port。
func sameHostAsRequest(src, reqHost string) bool {
	u, err := url.Parse(src)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, reqHost)
}
