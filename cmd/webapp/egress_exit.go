// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// egress_exit.go 出口身份：把「线路」与「公网出口 IP」两件事分开算。
//
// 为什么必须分开 —— 2026-10-17 的生产现场就是一个反例：
// 线路池里有 2 条线路（`127.0.0.1:11080` 平台 / `127.0.0.1:21160` 用户），
// 探活都成功，`last_ip` 都是 `39.91.80.16` —— 因为它们是**同一台机器**上的
// 两份 agent（一份 v0.1.0、一份 v0.3.0），隧道端口不同、出口是同一条家宽。
//
// 于是：
//  1. 管理员看到「2 条线路」，会以为有 2 个 IP 可用；
//  2. `planEgressAssignment` 按**线路**均衡，把几十个号"分散"到两个 URL 上，
//     实际全从同一个 IP 出去 —— 风控视角下等于没隔离，而且是**看起来做了**，
//     比不做更危险（人会据此认为这层已经安全了）。
//
// 所以「几个不同出口」必须按 **IP** 回答，不能按线路条数回答；自动分配也必须
// 按 IP 去重，把同一出口的多条线路折叠成一个候选。
//
// 这里只放纯函数（不碰 DB / 网络），便于把上面这几条规则钉死在单测里。
package main

import (
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
)

// normalizeExitIP 从各种历史形状里取出**纯 IP**，取不到返回空串。
//
// 形成过的形状（都是真实产物，不是假想）：
//
//	"39.91.80.16"                                   池探活结果 / 新版 agent
//	"39.91.80.16（Shandong Jinan China Unicom …）"   v0.1.0~v0.3.0 agent 的 exit_ip
//	"39.91.80.16（山东 济南 联通）"
//	"1.2.3.4:8080"                                  个别代理查询接口会带端口
//	""                                              未探到 / 查询接口不可达
//
// 为什么不能让这两种形状共存：字符串比较会把 `"39.91.80.16"` 与
// `"39.91.80.16（…）"` 判成两个不同出口，同 IP 归并就永远失效（这次的问题之一）。
func normalizeExitIP(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// `[2001:db8::1]:443` 这种带方括号的形式：[ 不在下面的允许字符集里，
	// 先剥括号再认，否则会在第一个字符就截断成空串。
	if strings.HasPrefix(s, "[") {
		if i := strings.Index(s, "]"); i > 1 {
			if a, err := netip.ParseAddr(s[1:i]); err == nil {
				return a.String()
			}
		}
	}
	// 先按「IP 允许出现的字符」截断：数字、点（IPv4）、冒号（IPv6/端口）、
	// 十六进制字母（IPv6）。全角括号、中文、空格都会在这里被切掉。
	cut := len(s)
	for i, r := range s {
		ok := r == '.' || r == ':' ||
			(r >= '0' && r <= '9') ||
			(r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
		if !ok {
			cut = i
			break
		}
	}
	cand := strings.TrimSuffix(strings.TrimSpace(s[:cut]), ".")
	if cand == "" {
		return ""
	}
	if a, err := netip.ParseAddr(cand); err == nil {
		return a.String()
	}
	// 带端口（`1.2.3.4:8080`）：剥掉端口再认一次。
	if host, _, err := net.SplitHostPort(cand); err == nil {
		if a, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
			return a.String()
		}
	}
	// 截断失败（尾部混了别的字符）：退化为「找第一段像 IP 的子串」。
	for i := 0; i < len(s); i++ {
		for j := len(s); j > i; j-- {
			if a, err := netip.ParseAddr(strings.Trim(s[i:j], " .")); err == nil {
				return a.String()
			}
		}
	}
	return ""
}

// exitIPOfProxy 一条池内线路的出口 IP（归一化）。
//
// 取 `LastIP`（云端从该代理**真实出站**探到的结果）而不是 agent 自报：
//   - 探活是"我们自己的证据"，agent 自报是"对端的说法"；
//   - agent 不在线时它自报的 IP 可能是几小时前的，而探活失败会把 LastIP 保留，
//     两者语义不同（见 egressProbe 的注释）。
func exitIPOfProxy(p *EgressProxy) string {
	if p == nil {
		return ""
	}
	return normalizeExitIP(p.LastIP)
}

// exitItem 参与出口聚合的最小信息（池内线路与看板条目都能映射成它）。
type exitItem struct {
	URL   string
	Name  string
	IP    string
	Owner int64
	OK    bool // 最近探活是否成功（仅用于选"代表线路"）
}

// egressExitGroup 一个公网出口 IP 以及落在它上面的线路。
type egressExitGroup struct {
	ExitIP string   `json:"exit_ip"`
	URLs   []string `json:"urls"`
	Names  []string `json:"names"`
	Owners []int64  `json:"owners"`
}

// groupExits 按出口 IP 聚合。返回：
//   - groups：**只含 2 条及以上线路**的组（真正需要提示"共用同一出口"的部分）；
//   - distinct：非空 IP 的不同取值个数（= 实际有几个出口）。
//
// 只统计能取到 IP 的线路：未探活 / 探活失败且从未成功过的线路 IP 为空，
// 把它们归成一个"未知组"会让"未知 ≠ 同一个"变成"未知 = 同一个"。
func groupExits(items []exitItem) (groups []egressExitGroup, distinct int) {
	idx := map[string]*egressExitGroup{}
	order := []string{}
	for _, it := range items {
		ip := normalizeExitIP(it.IP)
		if ip == "" {
			continue
		}
		g, ok := idx[ip]
		if !ok {
			g = &egressExitGroup{ExitIP: ip}
			idx[ip] = g
			order = append(order, ip)
			distinct++
		}
		g.URLs = append(g.URLs, it.URL)
		name := it.Name
		if name == "" {
			name = it.URL
		}
		g.Names = append(g.Names, name)
		g.Owners = append(g.Owners, it.Owner)
	}
	for _, ip := range order {
		if g := idx[ip]; len(g.URLs) > 1 {
			groups = append(groups, *g)
		}
	}
	return groups, distinct
}

// sharedExitOfURLs 把 groupExits 的结果摊平成「每个 URL → 同 IP 的其它 URL」，
// 便于线路卡直接标一行"与 X 共用出口"。
func sharedExitOfURLs(groups []egressExitGroup) map[string][]string {
	out := map[string][]string{}
	for _, g := range groups {
		for _, u := range g.URLs {
			others := make([]string, 0, len(g.URLs)-1)
			for _, v := range g.URLs {
				if v != u {
					others = append(others, v)
				}
			}
			out[u] = others
		}
	}
	return out
}

// dedupeProxiesByExit 自动分配前按出口 IP 折叠：同一个 IP 只留一条代表线路。
//
// 为什么必须折：分配算法按取到的候选线路做负载均衡，同一出口的两条线路会被
// 当成两个"出口"轮流使用 —— 于是它一边说"已分散到 2 条线路"，一边把两个号
// 送到同一个公网 IP 上。这不是均衡，是把"看起来均衡"当成了安全。
//
// 代表线路的选法（顺序即优先级，全部有理由）：
//  1. **平台线路（owner=0）优先**：用户线路是别人的出口，把平台流量的默认承载
//     放在它上面属于越界使用（与 handleEgressAssign 排除用户线路同一条原则）；
//  2. 探活成功的优先：让分配落在已知可用的线上；
//  3. 当前账号数少的优先：同等条件下继续压低单线暴露度；
//  4. URL 字典序（保证同样输入必得同样结果，便于核对与回归）。
//
// merged 返回被折叠掉的 URL → 代表 URL；warnings 是可读的一句人话。
func dedupeProxiesByExit(list []*EgressProxy, load map[string]int) (
	kept []*EgressProxy, merged map[string]string, warnings []string) {
	merged = map[string]string{}
	if len(list) == 0 {
		return nil, merged, nil
	}
	idx := map[string][]*EgressProxy{}
	order := []string{}
	for _, p := range list {
		ip := exitIPOfProxy(p)
		if ip == "" {
			// 出口未知的线路保持独立：它可能是一条真正不同的线路，只是没探到。
			// 把它折进任何一组都是拿"未知"当"相同"。
			kept = append(kept, p)
			continue
		}
		if _, ok := idx[ip]; !ok {
			order = append(order, ip)
		}
		idx[ip] = append(idx[ip], p)
	}
	// kept 先按原顺序放"未知出口"的那些，再把每组代表按 IP 首次出现顺序追加，
	// 以保证候选顺序稳定（planEgressAssignment 的确定性依赖它）。
	unknown := kept
	kept = make([]*EgressProxy, 0, len(list))
	kept = append(kept, unknown...)
	for _, ip := range order {
		group := idx[ip]
		if len(group) == 1 {
			kept = append(kept, group[0])
			continue
		}
		best := group[0]
		for _, p := range group[1:] {
			if betterExitRep(p, best, load) {
				best = p
			}
		}
		kept = append(kept, best)
		dropped := make([]string, 0, len(group)-1)
		for _, p := range group {
			if p == best {
				continue
			}
			merged[p.URL] = best.URL
			dropped = append(dropped, p.Name)
		}
		sort.Strings(dropped)
		warnings = append(warnings, fmt.Sprintf(
			"出口 %s 上有 %d 条线路（%s）实际是同一个公网 IP，已合并为「%s」参与分配 —— 它们不会提供额外的 IP。",
			ip, len(group), strings.Join(dropped, "、"), best.Name))
	}
	return kept, merged, warnings
}

// betterExitRep a 是否比 b 更适合做同一出口的代表线路（见 dedupeProxiesByExit）。
func betterExitRep(a, b *EgressProxy, load map[string]int) bool {
	if (a.OwnerUserID == 0) != (b.OwnerUserID == 0) {
		return a.OwnerUserID == 0
	}
	if a.LastOK != b.LastOK {
		return a.LastOK
	}
	if la, lb := load[a.URL], load[b.URL]; la != lb {
		return la < lb
	}
	return a.URL < b.URL
}
