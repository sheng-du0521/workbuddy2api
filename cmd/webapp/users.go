// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// sessionCookie 会话 cookie 名。
const sessionCookie = "wb_session"

// sessionTTL 会话有效期。
const sessionTTL = 7 * 24 * time.Hour

type ctxKey string

// ctxUserKey 请求上下文里的 *User。
const ctxUserKey ctxKey = "wb_user"

// userFrom 从上下文取当前登录用户（未登录返回 nil）。
func userFrom(r *http.Request) *User {
	u, _ := r.Context().Value(ctxUserKey).(*User)
	return u
}

// withUser 把用户塞进上下文。
func withUser(r *http.Request, u *User) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), ctxUserKey, u))
}

// bcryptCost 密码哈希强度（v0.24.1，P3-4）：从库默认的 10 提到 12。
//
// 代价：单次哈希 CPU 约 4 倍（10→12 即 2^2）。登录/注册本身有限流兜底，
// 这点开销换的是「库泄露后离线爆破成本翻两番」。
// 兼容性：bcrypt 的 cost 写在哈希串里，**旧 hash 照常可验证**，无需迁移数据；
// 用户下次改密码时自然升级到新 cost。
const bcryptCost = 12

// ---------- 注册限速 ----------

// rateLimiter 极简 IP 限速（滑动窗口）。
// 内网部署也用得上：开放注册若日后被转发到公网，这层是唯一的第一道闸。
// lastGC / maybeGC（v0.24.1，P3-5）：此前 hits 只增不减 —— 每个来访 IP 都会留下
// 一个桶，而注册桶窗口是**一小时**，于是「只来过一次的 IP」会永久驻留。
// 公网隧道场景下这等于把访问者数量当内存用（无界增长）。
type rateLimiter struct {
	mu     sync.Mutex
	hits   map[string][]time.Time
	limit  int
	window time.Duration
	lastGC time.Time
}

// maybeGC 清掉完全过期的桶（低频：每 window 最多一次）。调用方需持锁。
func (l *rateLimiter) maybeGC(now time.Time) {
	if now.Sub(l.lastGC) < l.window {
		return
	}
	l.lastGC = now
	cut := now.Add(-l.window)
	for k, ts := range l.hits {
		live := false
		for _, t := range ts {
			if t.After(cut) {
				live = true
				break
			}
		}
		if !live {
			delete(l.hits, k)
		}
	}
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{hits: map[string][]time.Time{}, limit: limit, window: window}
}

// allow 记一次访问并返回是否放行。
func (l *rateLimiter) allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.maybeGC(now)
	cut := now.Add(-l.window)
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.limit {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, now)
	return true
}

// ---------- 校验 ----------

var usernameRe = regexp.MustCompile(`^[A-Za-z0-9_]{3,32}$`)

// validateCredentials 用户名与密码强度校验。
// 开放注册下这是必须的：用户名限字符集防注入与钓鱼同形字，密码设下限防弱口令。
func validateCredentials(username, password string) error {
	if !usernameRe.MatchString(username) {
		return errors.New("用户名需为 3-32 位字母、数字或下划线")
	}
	if len(password) < 8 {
		return errors.New("密码至少 8 位")
	}
	if len(password) > 128 {
		return errors.New("密码过长")
	}
	return nil
}

// ---------- 密码强度 ----------

// passwordPolicyHint 强度规则的**人话版**（前端提示与该规则共用同一份文案，
// 避免「提示说一套、后端校验另一套」）。改规则时这里与前端的提示要一起改。
const passwordPolicyHint = "至少 8 位、至多 128 位，且需包含字母与数字（或其它符号）中的至少两类"

// validatePasswordStrength 改密码时的强度校验（v0.17.8）。
//
// 明确规则（逐条可测，不用模糊的「太弱」）：
//  1. 长度 8 ~ 128 字节（与注册口径一致，避免新老规则打架）；
//  2. 至少包含**两类**字符：小写字母 / 大写字母 / 数字 / 其它符号；
//     —— 只挡「纯数字」「纯字母」「纯符号」这三类最容易被撞的形态，
//     不强制四类混合（那会把强密码拒掉，反而逼用户写便签）；
//  3. 不得与用户名相同（忽略大小写）—— 账号+密码同形是最常见的弱口令；
//  4. 不得与旧密码相同（由调用方传入，可为空表示不校验）。
//
// 返回 nil 表示通过；否则返回可直接展示给用户的中文原因。
func validatePasswordStrength(username, password, oldPassword string) error {
	n := len(password)
	if n < 8 {
		return fmt.Errorf("新密码至少 8 位（当前 %d 位）", n)
	}
	if n > 128 {
		return errors.New("新密码过长（最多 128 位）")
	}
	if strings.EqualFold(strings.TrimSpace(username), password) {
		return errors.New("新密码不能与用户名相同")
	}
	if oldPassword != "" && password == oldPassword {
		return errors.New("新密码不能与原密码相同")
	}
	var low, up, digit, other bool
	for _, r := range password {
		switch {
		case r >= 'a' && r <= 'z':
			low = true
		case r >= 'A' && r <= 'Z':
			up = true
		case r >= '0' && r <= '9':
			digit = true
		default:
			other = true
		}
	}
	classes := 0
	for _, b := range []bool{low, up, digit, other} {
		if b {
			classes++
		}
	}
	if classes < 2 {
		return fmt.Errorf("新密码太简单，需%s", passwordPolicyHint)
	}
	return nil
}

// handleChangePassword 登录用户修改自己的密码。
// POST /api/auth/password  body: {"old_password":"...","new_password":"..."}
//
// 校验顺序（刻意从「身份」到「强度」，避免用强度报错探测他人密码）：
//  1. 原密码正确 —— 错则一律 401「原密码不正确」，不区分其它原因；
//  2. 新密码强度（含「不得与原密码相同」）；
//  3. 通过后写新哈希，并注销**其它**会话（保留当前这条）。
//
// 日志：全程不打印密码、不打印哈希，也不回显它们。
func (a *app) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	if u == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "未登录"})
		return
	}
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	// 1) 原密码：必须与库中哈希匹配。
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(req.OldPassword)) != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "原密码不正确"})
		return
	}
	// 2) 新密码强度。
	if err := validatePasswordStrength(u.Username, req.NewPassword, req.OldPassword); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcryptCost)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "密码处理失败"})
		return
	}
	if err := a.store.SetUserPassword(u.ID, string(hash)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// 3) 注销其它会话（保留当前这条，避免把自己踢下线）。
	other := int64(0)
	if c, cerr := r.Cookie(sessionCookie); cerr == nil && c.Value != "" {
		if n, derr := a.store.DeleteSessionsExcept(u.ID, HashToken(c.Value)); derr == nil {
			other = n
		}
	}
	msg := "密码已更新"
	if other > 0 {
		msg = fmt.Sprintf("密码已更新，并已注销其它 %d 个登录会话", other)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"other_sessions": other,
		"message":        msg,
		"policy":         passwordPolicyHint,
	})
}

// ---------- Handlers ----------

// bootstrapOK 判定本次注册是否被允许获得**管理员**角色（v0.24.2，P3-12）。
//
// 背景：此前「首个注册用户自动成为管理员」（见旧 Store.CreateUserWithWeChat）。
// 库为空 + 注册开放时，任何人抢先注册即拿到后台；而生产形态是公网隧道先起、
// 运维后注册 —— 中间那段窗口足以被扫站的脚本撞上。这不是理论风险：
// 只要有人定期扫新上线的 IP/域名，就能稳定抢到管理员。
//
// 现在的判据：**必须出示管理密钥**（请求体 bootstrap 字段，或 X-Admin-Key 头）。
// 复用既有凭证而不是再造一个文件：data/admin.key 本来就在首次启动时生成并打印，
// 运维手里已经有它；再引入一个 bootstrap.key 只会多一处要同步的密钥。
//
// 比较用 ConstantTimeCompare（与 authorized 同口径）。
// 例外：未配置管理密钥（a.key == ""，测试/嵌入式）时返回 true —— 与 authorized()
// 的既有约定保持一致，避免把测试全部打挂。
func (a *app) bootstrapOK(r *http.Request, provided string) bool {
	if a.key == "" {
		return true
	}
	provided = strings.TrimSpace(provided)
	if provided == "" {
		provided = strings.TrimSpace(r.Header.Get("X-Admin-Key"))
	}
	if provided == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(a.key)) == 1
}

// handleRegister 注册。
//
// 角色（v0.24.2，P3-12）：默认建**普通用户**；仅当「库里还没有任何管理员」
// 且本次请求出示了正确的管理密钥时，才把该账号提为管理员。这样：
//   - 抢注者拿不到管理员（没有密钥，最多建个普通号）；
//   - 抢注者无法把初始化锁死（判定看的是「有没有管理员」而不是「有没有用户」）；
//   - 运维侧仍是同一条路径：首次部署时填一次管理密钥即可。
func (a *app) handleRegister(w http.ResponseWriter, r *http.Request) {
	if !a.registerLimiter.allow(a.clientIP(r)) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "注册过于频繁，请稍后再试"})
		return
	}
	if off, _ := a.store.GetKV("register_enabled"); off == "0" {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "注册已关闭，请联系管理员"})
		return
	}
	var req struct {
		Username  string `json:"username"`
		Password  string `json:"password"`
		Ref       string `json:"ref"`       // 邀请码（可选）：绑定推荐关系，供上级拿推广返点
		WeChat    string `json:"wechat"`    // 微信号（可选，v0.21.0）
		Bootstrap string `json:"bootstrap"` // 管理引导密钥（可选，仅首次部署建管理员时需要）
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if err := validateCredentials(req.Username, req.Password); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	// 注册也要过**强度**校验（v0.24.1，P3-4）。
	//
	// 背景：注册此前只查长度（len >= 8），于是 "aaaaaaaa" 可以注册；而改密码路径
	// 早就有 validatePasswordStrength（v0.17.8）—— 同一份强度要求两个入口各写一半，
	// **开放注册的口子反而是最松的那个**。现在两条路径共用同一函数与同一份提示文案
	// （passwordPolicyHint），不会再分叉。oldPassword 传空：注册没有旧密码。
	if err := validatePasswordStrength(req.Username, req.Password, ""); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	wechat := strings.TrimSpace(req.WeChat)
	// 微信号做长度与非空校验（选填）：过长直接截断反而会让用户以为填成功，故报错。
	if len([]rune(wechat)) > 64 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "微信号过长（最多 64 字）"})
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcryptCost)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "密码处理失败"})
		return
	}
	// 先判角色再建号：HasAdmin 里若查询出错按「已有管理员」处理（见其注释），
	// 拒绝提权只会让运维重试一次，把管理员给错人却是不可逆的。
	role := RoleUser
	if !a.store.HasAdmin() && a.bootstrapOK(r, req.Bootstrap) {
		role = RoleAdmin
	}
	u, err := a.store.CreateUserAs(req.Username, string(hash), wechat, role)
	if err != nil {
		if errors.Is(err, ErrUserExists) {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "用户名已被占用"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// 邀请码绑定：无效邀请码不阻断注册（静默忽略），仅记一次成功绑定。
	if ref := strings.TrimSpace(req.Ref); ref != "" {
		if up, err := a.store.GetUserByInviteCode(ref); err == nil && up != nil && up.ID != u.ID {
			_ = a.store.SetUserReferredBy(u.ID, up.ID)
		}
	}
	a.issueSession(w, r, u)
	writeJSON(w, http.StatusOK, map[string]any{"user": userView(u), "first_admin": u.IsAdmin()})
}

// handleLogin 用户控制台登录（任意账号）。
func (a *app) handleLogin(w http.ResponseWriter, r *http.Request) {
	a.loginWith(w, r, false)
}

// handleAdminLogin 管理后台独立登录：仅管理员账号可通过。
// 与用户登录分成两个面板（v0.11.0），但共用同一账号体系与会话表。
func (a *app) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	a.loginWith(w, r, true)
}

// loginWith 登录公共实现。requireAdmin=true 时非管理员一律拒绝（不建会话）。
func (a *app) loginWith(w http.ResponseWriter, r *http.Request, requireAdmin bool) {
	if !a.loginLimiter.allow(a.clientIP(r)) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "尝试过于频繁，请稍后再试"})
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	u, err := a.store.GetUserByUsername(strings.TrimSpace(req.Username))
	if err != nil {
		// 不区分「用户不存在」与「密码错」，避免账号枚举。
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "用户名或密码错误"})
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(req.Password)) != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "用户名或密码错误"})
		return
	}
	if u.Status != StatusActive {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "账号已被停用"})
		return
	}
	if requireAdmin && !u.IsAdmin() {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "该账号无管理员权限"})
		return
	}
	a.issueSession(w, r, u)
	writeJSON(w, http.StatusOK, map[string]any{"user": userView(u)})
}

// issueSession 生成会话令牌并下发 HttpOnly cookie。
//
// 需要 r 是为了判定是否该带 Secure（v0.24.1，P3-2）—— 见 cookieSecure：
// 纯 HTTP 本机访问不能带（带了浏览器不回传、登录直接失效），
// 隧道 / 反代（https）必须带（否则会话 Cookie 会随明文请求发出、可被中间人抓取）。
func (a *app) issueSession(w http.ResponseWriter, r *http.Request, u *User) {
	token := randomHex(32)
	if err := a.store.CreateSession(HashToken(token), u.ID, sessionTTL); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "会话创建失败"})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   a.cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL / time.Second),
	})
}

// handleLogout 登出。
func (a *app) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		a.store.DeleteSession(HashToken(c.Value))
	}
	// 清除 Cookie 时属性要与下发时**一致**（v0.24.1，P3-2）：
	// 浏览器按 (name, domain, path) 匹配删除；属性不一致时部分实现会留下一个
	// 「同名但不带 Secure」的残留 Cookie —— 那正是要避免的形态。
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   a.cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleMe 当前登录用户信息。
// 注意：本 handler 未挂 userGuard（前端靠 401 判断未登录），
// 因此必须自己解析会话 cookie，不能依赖上下文注入。
func (a *app) handleMe(w http.ResponseWriter, r *http.Request) {
	u := a.currentUser(r)
	if u == nil {
		// 留痕（v0.31.45）：管理后台报 401 时，最需要分清的是
		// "根本没带 cookie"（换地址访问/清过缓存）还是"cookie 在但会话无效"
		// （过期/被别的会话注销）—— 处置完全不同。
		logAuthReject(r, http.StatusUnauthorized, sessionRejectReason(r), "", "")
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "未登录"})
		return
	}
	v := userView(u)
	// 消费闸进度：让用户自己看得见「今天已用 / 上限」，而不是撞墙才知道。
	dayUsed, monthUsed := a.quotaUsage(u)
	v["daily_limit"] = MicroToPoint(u.DailyLimitMicro)
	v["daily_used"] = MicroToPoint(dayUsed)
	v["monthly_limit"] = MicroToPoint(u.MonthlyLimitMicro)
	v["monthly_used"] = MicroToPoint(monthUsed)
	writeJSON(w, http.StatusOK, map[string]any{"user": v})
}

// ---------- 中间件 ----------

// userGuard 要求已登录（cookie 会话）；把 *User 注入上下文。
func (a *app) userGuard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := a.currentUser(r)
		if u == nil {
			logAuthReject(r, http.StatusUnauthorized, sessionRejectReason(r), "", "")
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "未登录"})
			return
		}
		if u.Status != StatusActive {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "账号已被停用"})
			return
		}
		next(w, withUser(r, u))
	}
}

// adminGuard 要求管理员：会话是管理员，或带对的管理密钥（保留命令行运维通道）。
func (a *app) adminGuard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.authorized(r) { // X-Admin-Key 通道（运维/脚本）
			next(w, r)
			return
		}
		u := a.currentUser(r)
		if u == nil || !u.IsAdmin() {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "需要管理员权限"})
			return
		}
		if u.Status != StatusActive {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "账号已被停用"})
			return
		}
		next(w, withUser(r, u))
	}
}

// isAdminSession 当前请求是否携带**有效的活跃管理员会话**（v0.24.2，P3-11）。
//
// 抽出来的理由：handleHealth 需要「是不是管理员」这个判断，但它不是一条路由守卫，
// 用不了 adminGuard。若在两处各写一遍 u != nil && u.IsAdmin() && u.Status == ...，
// 迟早会出现「加了第三个条件只改了一处」的分叉。
// （adminGuard 仍保留自己的分支：它需要把 *User 注入上下文，不只是判真假。）
func (a *app) isAdminSession(r *http.Request) bool {
	u := a.currentUser(r)
	return u != nil && u.IsAdmin() && u.Status == StatusActive
}

// metricsGuard 给 /metrics 加访问控制（v0.24.2，P3-11）。
//
// 背景：该端点此前是**完全公开**的。它确实不含凭证/uid/会话键，但聚合了
// 用户总数、API Key 数、用量与号池规模 —— 生产形态经公网隧道暴露时，
// 等于给扫描器一份「这台机器有多少用户、多少可用账号」的现成清单。
//
// 默认复用 adminGuard：管理密钥（X-Admin-Key / Bearer）与管理员会话都放行，
// 于是运维脚本与浏览器直接打开都能用。Prometheus 抓取时把密钥配进
// headers 即可（`X-Admin-Key: <data/admin.key 内容>`）。
//
// `-metrics-public` 是给「抓取端就在本机、且不想配密钥」的场景留的逃生门；
// 公网/隧道部署不要开（开=回到修复前的状态）。
//
// 注意 flag 是**按请求**判定的，不在注册路由时判定：routes() 只在启动时调用一次，
// 若在那里就二选一返回，运行期改 metricsPublic 将毫无效果（测试与热配置都指望它生效）。
func (a *app) metricsGuard(next http.HandlerFunc) http.HandlerFunc {
	guarded := a.adminGuard(next)
	return func(w http.ResponseWriter, r *http.Request) {
		if a.metricsPublic {
			next(w, r)
			return
		}
		guarded(w, r)
	}
}

// currentUser 解析会话 cookie → 用户。
func (a *app) currentUser(r *http.Request) *User {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	uid, ok := a.store.LookupSession(HashToken(c.Value))
	if !ok {
		return nil
	}
	u, err := a.store.GetUserByID(uid)
	if err != nil {
		return nil
	}
	return u
}

// sessionRejectReason 区分 401 的两种成因（日志用）。
//
// "没带 cookie" 与 "cookie 无效" 在浏览器里表现一样（都跳登录页），
// 但前者几乎总是**换了访问地址/端口**（cookie 按 host 隔离），
// 后者是会话过期或被他处注销。没有这条区分，排查只能靠猜。
func sessionRejectReason(r *http.Request) string {
	if _, err := r.Cookie(sessionCookie); err != nil {
		return "no session cookie"
	}
	return "session invalid or expired"
}

// userView 用户对外视图（绝不含密码哈希）。
func userView(u *User) map[string]any {
	return map[string]any{
		"id":         u.ID,
		"username":   u.Username,
		"role":       u.Role,
		"status":     u.Status,
		"balance":    MicroToPoint(u.BalanceMicro),
		"is_admin":   u.IsAdmin(),
		"created_at": u.CreatedAt,
		"wechat":     u.WeChat,
		// 余额分桶（v0.21.0）：可提现额度 + 两类累计点数。
		"withdrawable":    MicroToPoint(u.WithdrawableMicro),
		"exp_points":      MicroToPoint(u.ExpMicro),
		"recharge_points": MicroToPoint(u.RechargeMicro),
		"invite_code":     u.InviteCode,
	}
}
