// Command proxyscan 批量探测免费/外部 HTTP(S)/SOCKS5 代理的可用性。
//
// 为什么需要它：免费代理列表动辄几千条，但可用率通常只有个位数百分比，
// 且**失败以超时为主**（每个 5-10 秒）——串行探测一轮要几小时，完全不可用。
// 本工具做并发探测并把结论落成文件，供人工挑选后录入出口代理池。
//
// 关键设计一：判定目标是**国内**站点（默认 www.baidu.com）。
//
// 这不是随便选的。本系统的上游是 copilot.tencent.com / www.codebuddy.cn（腾讯域名），
// 因此「能访问国外站点」对业务毫无价值，反而会误导选号 —— 一个只能连国外站的代理
// 挂上去只会让上游请求全部失败。所以主判定必须是与业务同侧的国内可达性。
//
// 关键设计二：显式禁用环境代理（tr.Proxy = nil）。
//
// 本机存在 http_proxy 环境变量；若沿用 DefaultTransport，被测代理的流量会先被
// 环境代理再套一层，结论完全失真（甚至会把环境代理的出口 IP 当成候选代理的）。
package main

import (
	"bufio"
	"crypto/tls"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// probeCN 默认主判定目标：国内站点（与业务上游同侧）。
//
// 生产建议用真实上游双域名（见默认值）：只有「copilot.tencent.com 与
// www.codebuddy.cn 都可达」的代理才能承载本系统的请求 —— 实测存在只通其一的
// 代理（copilot 200 / codebuddy 000），若按单目标筛选会把这类代理录进来，
// 结果是 chat 能通、计费查询全挂，排障时极难定位。
var probeCN = "https://copilot.tencent.com,https://www.codebuddy.cn"

// probeIP 出口 IP 回显候选（尽力而为，取不到不影响可用判定）。
var probeIPList = []string{
	"https://myip.ipip.net/s",
	"https://api.ipify.org",
	"https://ifconfig.me/ip",
}

var ipRe = regexp.MustCompile(`\b(\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3})\b`)

// Result 单个代理的探测结论。
type Result struct {
	Raw     string // 原始 ip:port
	Scheme  string // http / socks5
	OK      bool   // 是否可用于本业务（国内可达）
	CNMs    int64  // 国内站点探测耗时（毫秒）
	ExitIP  string // 出口 IP（未取到为空）
	Note    string // 失败原因或补充说明
	Foreign bool   // 仅国外可达（对本业务无用，单独标注）
}

// normProxy 把 "ip:port" 或 "scheme://ip:port" 规范化成带协议的 URL。
func normProxy(raw, defaultScheme string) string {
	raw = strings.TrimSpace(raw)
	if strings.Contains(raw, "://") {
		return raw
	}
	return defaultScheme + "://" + raw
}

// newClient 建一个「只走指定代理」的客户端。
//
// tr.Proxy 必须显式设置：nil 表示不使用任何代理（含忽略环境变量），
// 这正是我们要的 —— 否则环境代理会污染被测结果。
//
// insecure 说明：免费代理里有相当比例是**中间人（MITM）**型——它们用自签证书
// 解密流量，因此必须跳过证书校验才"可用"。但生产代码（egressClient）用默认的
// 严格校验，这类代理在生产环境会直接失败。所以扫描务必显式选择：
// 宽松模式（-insecure）得到的集合只是「网络可达」的上界，严格模式才是
// 「生产真能用」的集合。这个区分能避免往池子里录一批看起来能通、实际全废的代理。
func newClient(proxyURL string, timeout time.Duration, insecure bool) (*http.Client, error) {
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, err
	}
	if u.Host == "" {
		return nil, fmt.Errorf("bad proxy url")
	}
	tr := &http.Transport{
		Proxy: http.ProxyURL(u), // 显式指定，覆盖环境变量
		DialContext: (&net.Dialer{
			Timeout:   timeout,
			KeepAlive: 10 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		DisableKeepAlives:     true,
		MaxIdleConns:          2,
	}
	if insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &http.Client{Transport: tr, Timeout: timeout}, nil
}

// probeOnce 发一次请求，返回状态码与耗时。body 只读极小片段（存在性判定足够）。
func probeOnce(c *http.Client, target string) (int, int64, []byte, error) {
	start := time.Now()
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return 0, 0, nil, err
	}
	req.Header.Set("User-Agent", "curl/8.0")
	resp, err := c.Do(req)
	ms := time.Since(start).Milliseconds()
	if err != nil {
		return 0, ms, nil, err
	}
	defer resp.Body.Close()
	buf := make([]byte, 256)
	n, _ := resp.Body.Read(buf)
	return resp.StatusCode, ms, buf[:n], nil
}

// checkProxy 探测单个代理。targets 为 AND 语义：全部可达才算可用。
func checkProxy(raw, defaultScheme string, targets []string, timeout time.Duration, fetchIP, insecure bool) Result {
	proxyURL := normProxy(raw, defaultScheme)
	r := Result{Raw: raw, Scheme: strings.SplitN(proxyURL, "://", 2)[0]}
	c, err := newClient(proxyURL, timeout, insecure)
	if err != nil {
		r.Note = "URL 非法：" + err.Error()
		return r
	}
	// 主判定：逐个目标验证，任一失败即失败（并指出是哪个目标）。
	var totalMs int64
	for _, tgt := range targets {
		code, ms, _, err := probeOnce(c, tgt)
		totalMs += ms
		if err != nil {
			r.CNMs = totalMs
			r.Note = hostOf(tgt) + " " + shortErr(err)
			return r
		}
		if code >= 500 {
			r.CNMs = totalMs
			r.Note = fmt.Sprintf("%s 返回 HTTP %d", hostOf(tgt), code)
			return r
		}
	}
	r.OK = true
	r.CNMs = totalMs

	// 附带取出口 IP（失败不回退可用结论 —— 取不到 IP 只说明回显端点不通）。
	if fetchIP {
		for _, ep := range probeIPList {
			c2, err2 := newClient(proxyURL, timeout, insecure)
			if err2 != nil {
				break
			}
			if _, _, body, err3 := probeOnce(c2, ep); err3 == nil {
				if m := ipRe.FindSubmatch(body); m != nil {
					r.ExitIP = string(m[1])
					break
				}
			}
		}
	}
	return r
}

// hostOf 取 URL 的主机名（日志里比整串 URL 好读）。
func hostOf(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	if i := strings.IndexByte(u, '/'); i >= 0 {
		u = u[:i]
	}
	return u
}

// shortErr 压缩错误信息（并发日志里保持一行可读）。
func shortErr(err error) string {
	s := err.Error()
	switch {
	case strings.Contains(s, "context deadline exceeded"),
		strings.Contains(s, "Timeout exceeded"),
		strings.Contains(s, "i/o timeout"):
		return "超时"
	case strings.Contains(s, "connection refused"):
		return "拒绝连接"
	case strings.Contains(s, "no such host"):
		return "域名解析失败"
	case strings.Contains(s, "EOF"):
		return "连接中断"
	case strings.Contains(s, "certificate"):
		return "证书错误"
	case strings.Contains(s, "socks"):
		return "SOCKS 协商失败"
	}
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

func main() {
	in := flag.String("in", "", "代理列表文件（每行 ip:port）")
	scheme := flag.String("scheme", "http", "默认协议：http / socks5")
	target := flag.String("target", probeCN, "判定目标，逗号分隔（AND 语义：全部可达才算可用）")
	conc := flag.Int("c", 200, "并发数")
	timeoutSec := flag.Int("t", 5, "单代理超时（秒）")
	fetchIP := flag.Bool("ip", true, "是否探测出口 IP")
	insecure := flag.Bool("insecure", false, "跳过 TLS 证书校验（会纳入 MITM 代理；生产代码用严格校验，故默认关闭）")
	out := flag.String("out", "", "结果输出文件（可用代理，每行 scheme://ip:port # ip 耗时）")
	jsonOut := flag.String("json", "", "完整结果输出（含失败原因，TSV）")
	flag.Parse()

	if *in == "" {
		fmt.Fprintln(os.Stderr, "用法：proxyscan -in proxies.txt [-scheme socks5] [-c 200]")
		os.Exit(2)
	}
	f, err := os.Open(*in)
	if err != nil {
		fmt.Fprintln(os.Stderr, "打开失败：", err)
		os.Exit(1)
	}
	defer f.Close()
	var cands []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		cands = append(cands, line)
	}
	if len(cands) == 0 {
		fmt.Fprintln(os.Stderr, "列表为空")
		os.Exit(1)
	}

	timeout := time.Duration(*timeoutSec) * time.Second
	var targets []string
	for _, t := range strings.Split(*target, ",") {
		if s := strings.TrimSpace(t); s != "" {
			targets = append(targets, s)
		}
	}
	if len(targets) == 0 {
		fmt.Fprintln(os.Stderr, "-target 不能为空")
		os.Exit(2)
	}
	fmt.Printf("待测 %d 个 · 并发 %d · 单代理超时 %ds · 判定目标 %d 个（AND）：%s\n",
		len(cands), *conc, *timeoutSec, len(targets), strings.Join(targets, " + "))

	results := make([]Result, len(cands))
	var done, okCount int64
	start := time.Now()
	sem := make(chan struct{}, *conc)
	var wg sync.WaitGroup
	for i, raw := range cands {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, raw string) {
			defer wg.Done()
			defer func() { <-sem }()
			r := checkProxy(raw, *scheme, targets, timeout, *fetchIP, *insecure)
			results[i] = r
			n := atomic.AddInt64(&done, 1)
			if r.OK {
				atomic.AddInt64(&okCount, 1)
			}
			if n%250 == 0 || n == int64(len(cands)) {
				fmt.Printf("  进度 %d/%d · 可用 %d · 用时 %s\n",
					n, len(cands), atomic.LoadInt64(&okCount), time.Since(start).Round(time.Second))
			}
		}(i, raw)
	}
	wg.Wait()

	var good []Result
	for _, r := range results {
		if r.OK {
			good = append(good, r)
		}
	}
	// 按国内延迟升序：延迟低的更适合承载请求。
	sort.Slice(good, func(i, j int) bool { return good[i].CNMs < good[j].CNMs })

	fmt.Printf("\n===== 结果 =====\n可用 %d / %d（%.1f%%）· 总用时 %s\n",
		len(good), len(cands), float64(len(good))*100/float64(len(cands)), time.Since(start).Round(time.Second))

	if *jsonOut != "" {
		var b strings.Builder
		b.WriteString("raw\tscheme\tok\tcn_ms\texit_ip\tnote\n")
		for _, r := range results {
			fmt.Fprintf(&b, "%s\t%s\t%v\t%d\t%s\t%s\n", r.Raw, r.Scheme, r.OK, r.CNMs, r.ExitIP, r.Note)
		}
		_ = os.WriteFile(*jsonOut, []byte(b.String()), 0o644)
		fmt.Println("完整结果 →", *jsonOut)
	}
	if *out != "" {
		var b strings.Builder
		b.WriteString("# 免费代理探测结果 · 生成于 " + time.Now().Format(time.RFC3339) + "\n")
		b.WriteString("# 判定目标（AND）：" + strings.Join(targets, " + ") + "\n")
		for _, r := range good {
			fmt.Fprintf(&b, "%s://%s\t# 出口IP=%s 国内延迟=%dms\n",
				r.Scheme, r.Raw, orDash(r.ExitIP), r.CNMs)
		}
		_ = os.WriteFile(*out, []byte(b.String()), 0o644)
		fmt.Println("可用代理 →", *out)
	}
	fmt.Println()
	for i, r := range good {
		if i >= 15 {
			fmt.Printf("  …… 其余 %d 个见输出文件\n", len(good)-15)
			break
		}
		fmt.Printf("  %-24s %-8s 出口=%-16s 国内 %dms\n", r.Raw, r.Scheme, orDash(r.ExitIP), r.CNMs)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
