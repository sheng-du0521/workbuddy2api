// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// authlog.go 鉴权/来源校验失败的留痕（v0.31.45）。
//
// ## 为什么需要它
//
// `/v1` 的几个拒绝分支此前**只回 HTTP 错误**：不写 usage_logs、也不打日志。
// 于是 2026-10-19 发生了一件没法查的事：用户报「反代 API 用 403」，
// 而服务端近 6 小时零条 401/403、usage_logs 也停在 14:08 —— 请求根本没穿过鉴权，
// 但我们连"它来过、它带的是哪个 key、为什么被拒"都不知道，只能靠猜。
//
// 同一个毛病昨天刚在任务侧修过（失败只记数量、不记原因）。这一类"拒绝不留痕"
// 的代价是**排查时间全部转嫁给运维**，而用户只能反复试。
//
// ## 边界
//
//   - 只记**元信息**：路径、状态、原因、key 前缀（`sk-wb-xxxx...yyyy` 那种，
//     取自已存储的 key_prefix，绝不打印完整 key）、客户端 IP、UA 前 60 字。
//   - **节流**：公网上的扫描器会不断打 `/v1`，无节流会把日志刷满，
//     真正的用户请求反而被淹没。同一 (IP + 原因) 默认 60 秒最多一条，
//     并记录被抑制的条数 —— 「安静」与「看不见」是两回事。
package main

import (
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// authLogWindow 同一 (IP+原因) 的日志抑制窗口。
//
// 60 秒：足够让"用户连试三次都失败"只占一两行，又不至于把一次真实故障藏掉
// （因为窗口一过就会再打一条，且每次都带抑制计数）。
const authLogWindow = 60 * time.Second

var (
	authLogMu   sync.Mutex
	authLogLast = map[string]time.Time{}
	authLogDrop = map[string]int{}
)

// logAuthReject 记录一次鉴权/来源校验拒绝（带节流）。
//
// reason 应当是**稳定的短标签**（`missing api key` / `invalid api key` /
// `api key disabled` / `account banned` / `csrf origin mismatch` / `no session`），
// 便于 grep 与计数；具体细节放 detail。
func logAuthReject(r *http.Request, status int, reason, keyPrefix, detail string) {
	if r == nil {
		return
	}
	ip := clientIPOf(r)
	k := ip + "|" + reason
	now := time.Now()

	// 环表与日志分开：日志要节流（防扫描器刷屏），环表不节流
	// （"同一秒被拒 20 次"本身就是最有价值的线索）。
	recordAuthReject(authRejectEntry{
		At: now.Unix(), Method: r.Method, Path: r.URL.Path, Status: status,
		Reason: reason, KeyPrefix: keyPrefix, IP: ip,
		UA: shortUA(r.UserAgent()), Detail: detail,
	})

	authLogMu.Lock()
	last, seen := authLogLast[k]
	if seen && now.Sub(last) < authLogWindow {
		authLogDrop[k]++
		authLogMu.Unlock()
		return
	}
	dropped := authLogDrop[k]
	authLogLast[k] = now
	delete(authLogDrop, k)
	authLogMu.Unlock()

	var b strings.Builder
	b.WriteString("WARN: [auth] ")
	b.WriteString(r.Method)
	b.WriteString(" ")
	b.WriteString(r.URL.Path)
	b.WriteString(" -> ")
	b.WriteString(itoaLog(status))
	b.WriteString(" ")
	b.WriteString(reason)
	if keyPrefix != "" {
		b.WriteString(" | key=")
		b.WriteString(keyPrefix)
	}
	b.WriteString(" | ip=")
	b.WriteString(ip)
	if ua := shortUA(r.UserAgent()); ua != "" {
		b.WriteString(" | ua=")
		b.WriteString(ua)
	}
	if detail != "" {
		b.WriteString(" | ")
		b.WriteString(detail)
	}
	if dropped > 0 {
		b.WriteString(" | 上一窗口内已抑制 ")
		b.WriteString(itoaLog(dropped))
		b.WriteString(" 条同类")
	}
	log.Print(b.String())
}

// shortUA 截断 UA：完整 UA 可能有几百字符，日志只需要足够分辨"浏览器/curl/SDK"。
func shortUA(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 60 {
		return s[:60] + "…"
	}
	return s
}

// clientIPOf 取客户端 IP（与限流同口径：优先 X-Forwarded-For 的第一跳）。
//
// 只用于日志，因此不做可信代理校验 —— 与 enforceRateLimit 不同：
// 那个值会参与限流决策，被伪造就是安全问题；日志里的值被伪造只影响排查线索。
func clientIPOf(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// maskKeyForLog 把用户送来的 key 变成可核对的指纹（前 10 + 后 4）。
//
// 为什么不用存储里的 key_prefix：用户拿着**错的** key 时，库里没有对应记录，
// 而恰恰是"他到底发了什么"最有价值（能立刻看出是旧 key、还是少了前缀）。
// 仍然不打印全文：日志会被拷来拷去，密钥不该出现在里面。
func maskKeyForLog(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if len(raw) <= 16 {
		return raw[:minInt(4, len(raw))] + "…"
	}
	return raw[:10] + "..." + raw[len(raw)-4:]
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// itoaLog 小工具：只为拼日志，避免在每个调用点 import strconv。
//
// 与 tasks.go 的 itoa 同名会被拒（那个是任务条数的特殊默认值），故单独命名。
func itoaLog(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// ---------------------------------------------------------------------------
// 最近拒绝（供管理端直接查看）
// ---------------------------------------------------------------------------
//
// 为什么除了日志还要一份内存环形表：日志要 SSH 上去 grep，而用户/运维
// 真正会看的是页面。2026-10-19 的"403 查不到"就是卡在这一步 —— 服务端连日志
// 都没有，更别说页面。现在两条路都有：日志给人 grep，环表给页面渲染。

// authRejectRingMax 环表容量。50 条足够看清"是不是同一类原因"，
// 再多只是把浏览器 DOM 撑大。
const authRejectRingMax = 50

// authRejectEntry 一条被拒请求（只含元信息，不含密钥原文）。
type authRejectEntry struct {
	At        int64  `json:"at"`
	Method    string `json:"method"`
	Path      string `json:"path"`
	Status    int    `json:"status"`
	Reason    string `json:"reason"`
	KeyPrefix string `json:"key_prefix,omitempty"`
	IP        string `json:"ip,omitempty"`
	UA        string `json:"ua,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

var (
	authRejectMu   sync.Mutex
	authRejectRing []authRejectEntry // 最新在前
)

// recordAuthReject 把一条拒绝放进环表（与日志的节流**无关**：日志要防刷屏，
// 环表只是内存里 50 条，保留全部反而更能看出"同一秒被拒了 20 次"）。
func recordAuthReject(e authRejectEntry) {
	authRejectMu.Lock()
	defer authRejectMu.Unlock()
	authRejectRing = append([]authRejectEntry{e}, authRejectRing...)
	if len(authRejectRing) > authRejectRingMax {
		authRejectRing = authRejectRing[:authRejectRingMax]
	}
}

// authRejectsSnapshot 返回当前环表副本（调用方可以安全序列化）。
func authRejectsSnapshot() []authRejectEntry {
	authRejectMu.Lock()
	defer authRejectMu.Unlock()
	out := make([]authRejectEntry, len(authRejectRing))
	copy(out, authRejectRing)
	return out
}

// resetAuthRejects 清空环表（测试用；生产没有清空入口 —— 它是滚动窗口）。
func resetAuthRejects() {
	authRejectMu.Lock()
	authRejectRing = nil
	authRejectMu.Unlock()
}

// handleAuthRejects GET /api/admin/auth-rejects
//
// 管理端「最近被拒的请求」数据源。只回**元信息**：路径 / 状态 / 原因 /
// key 指纹（前 10 + 后 4）/ IP / UA / 细节，**绝不回完整密钥**。
//
// 为什么是内存环表而不是查库：这些拒绝**不落 usage_logs**（没有账号、没有计费），
// 而"刚刚被拒了什么"正是排查时最需要看的东西 —— 落库反而要新建一张表，
// 收益只是"重启后还能看"，而重启后用户早就重试过了。
func (a *app) handleAuthRejects(w http.ResponseWriter, r *http.Request) {
	rows := authRejectsSnapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"count":  len(rows),
		"rows":   rows,
		"window": "进程内最近 " + itoaLog(authRejectRingMax) + " 条（重启清空）",
		"note": "这里只出现**被拒**的请求；'account banned' 指用户被停用，" +
			"'invalid api key' 最常见于客户端还配着已删除的旧 Key。",
	})
}
