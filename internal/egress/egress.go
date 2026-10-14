// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// Package egress 出站代理的格式校验与「真实出口 IP」探测。
//
// 为什么需要单独成包：出站代理此前只有一个自由文本输入框，保存时不校验、使用时不
// 验证，于是出现一个隐蔽的失效模式 —— 代理 URL 填错一个字符，账号就静默回落直连
// （见 cmd/webapp 的 egressClient），而界面上仍显示「已配代理」。管理员以为在做
// IP 隔离，实际上所有账号都从本机 IP 出去，防封形同虚设且毫无感知。
//
// 本包提供两道闸：
//  1. Validate —— 保存前的格式校验（协议 / 主机 / 端口），把错误挡在配置阶段；
//  2. Probe    —— 用该代理实际发一次请求，取回上游看到的**真实出口 IP**。
//
// Probe 的关键设计是区分两类失败（否则会把「探测端点不可达」误判成「代理坏了」）：
//   - 先直连访问一次探测端点。连直连都不通 → 判定为端点不可达（EndpointUnreachable），
//     结论是「无法验证」，不归咎代理；容器/受限网络下这是常见情形。
//   - 直连通、走代理不通 → 才是代理真的有问题（OK=false 且 Error 有值）。
package egress

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SupportedSchemes Go 的 http.Transport 通过 ProxyURL 能识别的代理协议。
//
// 注意不含 socks5h：Go 的 httpproxy 只认 http / https / socks5，
// 写成 socks5h 会被判为不支持的 scheme（原实现里这会直接导致静默直连）。
// 因此这里提前拒绝，并给出可操作的提示（改用 socks5）。
var SupportedSchemes = map[string]bool{
	"http":   true,
	"https":  true,
	"socks5": true,
}

// Direct 账号级出口的**显式直连**标记。
//
// 为什么需要这个哨兵值，而不是「留空即直连」：账号的出口是**两级配置**——
// 账号自身 egress_proxy 优先，为空时继承全局默认代理（egress_proxy_default）。
// 因此「留空」的真实语义是「未指定 / 跟随全局默认」，只有在全局默认也为空时
// 才恰好等于直连。一旦管理员设了全局默认代理，**留空就变成走默认代理**，
// 此时没有任何办法让某个账号单独直连 —— 这正是「支持直连」要补的缺口。
//
// 有了 Direct，「直连」成为一等状态：它显式覆盖全局默认，语义确定，不随
// 全局配置变化而漂移。（与「留空」的区别见 cmd/webapp 的 ResolveEgress 注释。）
const Direct = "direct"

// IsDirect 判断账号级出口配置是否显式要求直连。
func IsDirect(s string) bool {
	return strings.EqualFold(strings.TrimSpace(s), Direct)
}

// ValidateAccountEgress 校验**账号级**出口配置，返回规范化值。
//
// 与 Validate 的区别：这里额外接受 Direct 哨兵。空串仍然合法（= 未指定，跟随全局默认）。
// 之所以单开一个函数而不是放宽 Validate：Validate 也用于代理池录入与运行期建客户端，
// 那些场景**不允许** direct（池里存 direct 毫无意义，客户端更不可能「连到 direct」）。
func ValidateAccountEgress(raw string) (string, error) {
	if IsDirect(raw) {
		return Direct, nil
	}
	// 常见误写：想直连却写成 "none"/"null"/"-" 之类。
	// 直接拒绝并给出正确写法，比默默当成非法代理（然后账号被判出口异常）更友好。
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "none", "null", "nil", "off", "no", "false", "-", "direct://":
		return "", fmt.Errorf("如需直连请填 %q（当前填的 %q 不是合法代理，也不是直连标记）", Direct, strings.TrimSpace(raw))
	}
	return Validate(raw)
}

// Validate 校验并规范化代理 URL。空串合法（语义 = 有意直连，不配置代理）。
//
// 返回规范化后的 URL 与错误。错误信息面向管理员，需要能直接指出问题所在
// （是协议不认、还是漏了端口），否则「保存失败」三个字无法指导修复。
func Validate(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil
	}
	// 先挡一类最常见的笔误：只写了 host:port 没写协议。
	// url.Parse 会把 "1.2.3.4:8080" 解析成 scheme=1.2.3.4 这种离谱结果，
	// 若不特判，报错会变成「不支持的协议 "1.2.3.4"」，反而更难懂。
	if !strings.Contains(s, "://") {
		if _, _, err := net.SplitHostPort(s); err == nil {
			return "", fmt.Errorf("代理地址缺少协议前缀，请写成 http://%s 或 socks5://%s", s, s)
		}
		return "", errors.New("代理地址缺少协议前缀（如 http:// 或 socks5://）")
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("代理 URL 无法解析：%v", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "socks5h" {
		return "", errors.New("不支持 socks5h，请改用 socks5（由本地解析域名）")
	}
	if !SupportedSchemes[scheme] {
		return "", fmt.Errorf("不支持的代理协议 %q（支持 http / https / socks5）", u.Scheme)
	}
	if u.Hostname() == "" {
		return "", errors.New("代理 URL 缺少主机名")
	}
	port := u.Port()
	if port == "" {
		return "", fmt.Errorf("代理 URL 缺少端口（如 %s://%s:8080）", scheme, u.Hostname())
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("端口 %q 不合法（应为 1-65535）", port)
	}
	return s, nil
}

// probeEndpoints 出口 IP 探测端点候选（按「国内可达性优先」排序）。
//
// 为什么要多个候选 + 逐个尝试：这些第三方回显服务没有 SLA，随时可能被墙或下线。
// 单端点会让「端点挂了」看起来像「代理坏了」，进而导致管理员去做无意义的排查。
var probeEndpoints = []string{
	"https://myip.ipip.net/s",
	"https://api.ip.sb/ip",
	"https://api.ipify.org",
	"https://ifconfig.me/ip",
	"https://ipinfo.io/ip",
}

// ipv4Re 从任意文本响应里抠出 IPv4。各端点返回体格式不同（纯 IP / 带中文前缀 /
// JSON），统一用正则抽取比逐个写解析器更稳也更短。
var ipv4Re = regexp.MustCompile(`\b(\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3})\b`)

// ProbeResult 一次出口探测的结果。
type ProbeResult struct {
	// OK 是否成功取回出口 IP。
	OK bool
	// IP 上游看到的真实出口 IP（OK 时有效）。
	IP string
	// Error 失败原因（OK 为 false 时有效）。
	Error string
	// Endpoint 实际使用的探测端点。
	Endpoint string
	// ElapsedMs 耗时（毫秒）。
	ElapsedMs int64
	// EndpointUnreachable 直连也无法访问任何探测端点 → 无法判定代理好坏。
	// 调用方应显示「未能验证」而不是「代理不可用」。
	EndpointUnreachable bool
}

// probeTimeout 单次探测请求的超时。
//
// 取 6 秒：代理链路多一次握手，过短会误判为失败；过长会让「保存时校验」这种
// 交互路径卡住。注意这个值是**单次**请求的上限，探活的整体耗时见 Probe 的说明。
const probeTimeout = 6 * time.Second

// probeBudget 单次 Probe 的网络总预算。
//
// 为什么需要总预算：探活要先把「探测端点本身是否可达」试出来（逐个试候选端点），
// 若不做总量限制，受限网络下 5 个端点各卡满超时就是 30 秒以上——
// 管理员点一次探活要等半分钟，接口超时、体验崩坏（实测踩到过 40 秒）。
// 超预算即收敛为「无法验证」，这比继续干等更有用。
const probeBudget = 12 * time.Second

// reachableCache 记住「哪个端点是直连可达的」，避免每次探测都先直连试一轮。
//
// 缓存成功结果即可：一旦确认某端点可用，后续直接用它；若它后来失效，代理探测
// 会失败并回落到重新挑选（见 pickEndpoint）。
var reachableCache = struct {
	mu   sync.Mutex
	url  string
	at   time.Time
	life time.Duration
}{
	// life 在复合字面量里给默认值（v0.24.2，P3-15）：原先是包级 init() 赋值，
	// 于是「导入本包 = 改全局状态」—— 副作用不可见、无法在测试之间复位，
	// 也让人误以为零值可用。搬进字面量后默认值随声明走，init 整个删掉。
	life: 10 * time.Minute,
}

// Probe 通过 proxyURL 探测真实出口 IP。proxyURL 为空时探测本机直连出口。
//
// 语义：返回的 ProbeResult 描述「这次探测」的结论，不返回 error ——
// 「探测失败」本身就是一种有信息量的结果（要展示给管理员），不是异常。
func Probe(proxyURL string) ProbeResult {
	start := time.Now()
	client, err := clientFor(proxyURL)
	if err != nil {
		// 格式非法：不必浪费网络请求。
		return ProbeResult{Error: err.Error(), ElapsedMs: time.Since(start).Milliseconds()}
	}
	// 1) 先确认至少有一个端点在本机直连条件下可达，否则无法归因。
	direct := pickEndpoint(start)
	if direct == "" {
		return ProbeResult{
			EndpointUnreachable: true,
			Error:               "探测端点均不可达（本机出网受限），无法验证出口 IP",
			ElapsedMs:           time.Since(start).Milliseconds(),
		}
	}
	// 2) 直连探测时，该端点就是最终结果，无需再绕一圈。
	if proxyURL == "" {
		ip, err := fetchIP(client, direct)
		return finish(start, direct, ip, err)
	}
	// 3) 走代理探测一次。
	//
	// 刻意**不**在失败后换端点重试：pickEndpoint 已经证明该端点直连可达，
	// 那么「走代理不通」的原因只可能在代理侧；换端点既改变不了结论，
	// 又会让单次探活耗时成倍增长（每个候选端点都要等满超时）。
	ip, err := fetchIP(client, direct)
	return finish(start, direct, ip, err)
}

// finish 组装结果（统一填耗时与端点）。
func finish(start time.Time, endpoint, ip string, err error) ProbeResult {
	r := ProbeResult{Endpoint: endpoint, IP: strings.TrimSpace(ip), ElapsedMs: time.Since(start).Milliseconds()}
	if err != nil {
		r.Error = err.Error()
		return r
	}
	r.OK = true
	return r
}

// pickEndpoint 选一个直连可达的探测端点；全部不可达（或超总预算）返回空串。
//
// start 为本次 Probe 的起始时刻，用于套用 probeBudget。命中缓存则立即返回，
// 因此只有进程内第一次探活（或缓存过期后）才需要走这段探测。
func pickEndpoint(start time.Time) string {
	reachableCache.mu.Lock()
	if reachableCache.url != "" && time.Since(reachableCache.at) < reachableCache.life {
		u := reachableCache.url
		reachableCache.mu.Unlock()
		return u
	}
	reachableCache.mu.Unlock()

	direct, err := clientFor("")
	if err != nil {
		return ""
	}
	for _, ep := range probeEndpoints {
		// 预算耗尽就收手：继续试只会让调用方干等，而结论（无法验证）已经确定。
		if time.Since(start) > probeBudget {
			return ""
		}
		if _, err := fetchIP(direct, ep); err == nil {
			reachableCache.mu.Lock()
			reachableCache.url, reachableCache.at = ep, time.Now()
			reachableCache.mu.Unlock()
			return ep
		}
	}
	return ""
}

// clientFor 按代理 URL 建一个一次性 http.Client；空串 = 直连。
//
// 不复用 cmd/webapp 的缓存客户端：那个缓存按账号维度管理，这里需要的是
// 「任意 URL 都能独立探测」，耦合过去会让调用方必须持有一个 Proxy 实例。
func clientFor(proxyURL string) (*http.Client, error) {
	tr := &http.Transport{
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: probeTimeout,
		TLSHandshakeTimeout:   probeTimeout,
	}
	if strings.TrimSpace(proxyURL) != "" {
		norm, err := Validate(proxyURL)
		if err != nil {
			return nil, err
		}
		u, err := url.Parse(norm)
		if err != nil {
			return nil, err
		}
		tr.Proxy = http.ProxyURL(u)
	}
	return &http.Client{Transport: tr, Timeout: probeTimeout}, nil
}

// fetchIP 访问端点并抽取 IP。
func fetchIP(c *http.Client, endpoint string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "curl/8.0")
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("端点返回 HTTP %d", resp.StatusCode)
	}
	// 限 4KB：回显服务响应都很小，超出的必然是异常内容。
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if err != nil {
		return "", err
	}
	m := ipv4Re.FindStringSubmatch(string(body))
	if m == nil {
		return "", errors.New("端点响应中未找到 IP")
	}
	return m[1], nil
}
