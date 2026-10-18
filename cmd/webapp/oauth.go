// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

const (
	// oauthTTL OAuth 会话存活时间。超时未完成的会话由 GC 清理。
	oauthTTL = 10 * time.Minute
	// pollTimeout 单次轮询的上游请求超时。
	pollTimeout = 15 * time.Second
)

// realmEndpoints 按 realm 返回上游 base 与 Origin/Referer。
// cn → copilot.tencent.com（Origin: codebuddy.cn）
// global → www.workbuddy.ai（同域）
func realmEndpoints(realm string) (base, origin string) {
	if realm == "global" {
		return "https://www.workbuddy.ai", "https://www.workbuddy.ai"
	}
	return "https://copilot.tencent.com", "https://www.codebuddy.cn"
}

// loginHeaders 登录流程的通用请求头（对齐 cmd/login 的 commonHeaders）。
func loginHeaders(origin string) func(*http.Request) {
	return loginHeadersDev(origin, "", "", "")
}

// loginHeadersDev 带设备身份族的登录请求头（v0.31.40）。
//
// 空值不注入（与运行期 `resolveDeviceToken` 的口径一致：没有就不发，别发空串 ——
// 空头会被上游当成"有头但无效"，比没有更可疑）。
func loginHeadersDev(origin, deviceToken, machineID, sessionID string) func(*http.Request) {
	return func(req *http.Request) {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.Header.Set("Origin", origin)
		req.Header.Set("Referer", origin+"/")
		req.Header.Set("User-Agent", "CLI/2.63.2 CodeBuddy/2.63.2")
		if deviceToken != "" {
			req.Header.Set("X-Device-Token", deviceToken)
		}
		if machineID != "" {
			req.Header.Set("X-Machine-ID", machineID)
		}
		if sessionID != "" {
			req.Header.Set("X-Session-ID", sessionID)
		}
		// X-Request-ID 每次请求一个（与运行期一致：它是"这次请求"的标识，不是设备身份）。
		req.Header.Set("X-Request-ID", newRandomID(16))
	}
}

// loginHeadersFor 会话级设备身份的直接封装。
func loginHeadersFor(sess *OAuthSession) func(*http.Request) {
	if sess == nil {
		return loginHeaders("")
	}
	return loginHeadersDev(sess.Origin, sess.DeviceToken, sess.MachineID, sess.SessionID)
}

// apiEnvelope 上游 {code,msg,data} 业务信封。
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// doUpstreamJSON 发起请求并按信封解包：code != 0 视为业务失败。
func doUpstreamJSON(client *http.Client, method, url string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, 0, err
	}
	if headers != nil {
		headers(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("upstream http %d", resp.StatusCode)
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, fmt.Errorf("business code=%d msg=%s", env.Code, env.Msg)
	}
	return env.Data, resp.StatusCode, nil
}

// OAuthSession 一次设备授权登录会话。
// Web 化必改项：原实现把 state 落到 os.TempDir()/wb2api-login-state.json，
// 在多用户/多并发 Web 场景会互相覆盖。这里改为进程内会话表（后续可换 Redis/DB）。
type OAuthSession struct {
	ID      string
	State   string
	Realm   string
	Base    string
	Origin  string
	AuthURL string
	// DeviceToken / MachineID / SessionID 本次登录流程使用的**设备身份**（v0.31.40）。
	//
	// 为什么登录也要带设备头：运行期每条出站请求都带 X-Machine-ID / X-Session-ID /
	// X-Device-Token（按 uid 派生），**唯独登录链路一条都不带** —— 于是上游看到的
	// 是「一个没有设备身份的客户端创建了会话，随后被设备 X 使用」。这至少是
	// 两套身份，严重时会被当成会话被盗用。
	//
	// 现在把登录时实际发出的设备身份**存进会话并落库**：创建会话的那份身份
	// 与之后使用它的那份**是同一份**（构造上成立，不靠约定）。
	DeviceToken string
	MachineID   string
	SessionID   string
	CreatedAt   time.Time
}

var (
	oauthMu       sync.Mutex
	oauthSessions = map[string]*OAuthSession{}
)

// StartOAuth 拉取设备授权 URL 并建立服务端会话。
//
// uidHint 可选：已知是哪一个号在重新登录时传进来（前端从某张账号卡发起）。
// 传了就用**该账号的稳定派生身份**，让重新登录前后的设备身份连续；
// 不传（新号首扫）就用本次会话随机生成的身份，并在登录成功时落库 ——
// 两种情况下"登录时发出的身份"与"之后使用的身份"都是同一份。
func StartOAuth(realm string, uidHint ...string) (*OAuthSession, error) {
	realm = strings.ToLower(strings.TrimSpace(realm))
	if realm == "" {
		realm = "cn"
	}
	if realm != "cn" && realm != "global" {
		return nil, fmt.Errorf("invalid realm %q (want cn|global)", realm)
	}
	base, origin := realmEndpoints(realm)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Timeout: pollTimeout, Jar: jar}

	// 设备身份必须在**发第一个请求之前**就定下来（v0.31.40）。
	//
	// 顺序不是风格问题：auth/state 就是上游"建立这次登录"的那一步，这一步没有身份，
	// 后面 sign/token 再补也补不回来 —— 上游记住的是"一个无身份客户端发起了登录"。
	devTok, devMach, devSess := oauthDeviceIdentity(uidHint)
	data, _, err := doUpstreamJSON(client, http.MethodPost,
		base+"/v2/plugin/auth/state?platform=CLI",
		loginHeadersDev(origin, devTok, devMach, devSess), bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, fmt.Errorf("auth/state: %w", err)
	}
	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
		return nil, fmt.Errorf("auth/state: missing state or authUrl")
	}

	sess := &OAuthSession{
		ID:        newRandomID(16),
		State:     st.State,
		Realm:     realm,
		Base:      base,
		Origin:    origin,
		AuthURL:   st.AuthURL,
		CreatedAt: time.Now(),
	}
	sess.DeviceToken, sess.MachineID, sess.SessionID = devTok, devMach, devSess
	oauthMu.Lock()
	oauthSessions[sess.ID] = sess
	oauthMu.Unlock()
	return sess, nil
}

// oauthDeviceIdentity 决定本次登录用哪套设备身份（v0.31.40）。
//
// 两条路，都保证"登录时发出的"与"之后使用的"是同一份：
//   - 有 uid 提示（从某张账号卡点"重新登录"）→ 用该账号的稳定派生身份，
//     重登前后身份连续（对上游来说还是那台设备，只是重新授权了一次）；
//   - 无提示（新号首扫）→ 本次会话随机生成，登录成功时随账号落库，
//     之后的运行期就用这一份。
//
// 为什么不一律用随机：同一个号每次重登都换设备身份，看起来像"这个账号在到处被登录"——
// 那正是风控最敏感的信号。
func oauthDeviceIdentity(uidHint []string) (deviceToken, machineID, sessionID string) {
	if hint := strings.TrimSpace(firstOrEmpty(uidHint)); hint != "" {
		return upstream.AccountDeviceToken(hint), upstream.AccountMachineID(hint), upstream.AccountSessionID(hint)
	}
	return newRandomID(24), newRandomID(16), newRandomID(16)
}

// firstOrEmpty 取可变参数的第一个值（空切片返回空串）。
func firstOrEmpty(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

// oauthResult 轮询结果。
type oauthResult struct {
	Status  string // "pending" | "done"
	Message string
	Account *Account
}

// PollOAuth 轮询一次登录状态；成功后写库并返回账号。
// ownerUserID = 0 表示管理员自持账号；>0 表示该用户「代挂」的账号
// （归属写库，控制台据此展示与选号；入池仍需走申请审批）。
func PollOAuth(sessionID string, store *Store, ownerUserID int64) (oauthResult, error) {
	oauthMu.Lock()
	sess, ok := oauthSessions[sessionID]
	oauthMu.Unlock()
	if !ok {
		return oauthResult{}, fmt.Errorf("unknown or expired login session")
	}
	if time.Since(sess.CreatedAt) > oauthTTL {
		oauthMu.Lock()
		delete(oauthSessions, sessionID)
		oauthMu.Unlock()
		return oauthResult{}, fmt.Errorf("login session expired")
	}

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Timeout: pollTimeout, Jar: jar}

	tokRaw, _, err := doUpstreamJSON(client, http.MethodGet,
		sess.Base+"/v2/plugin/auth/token?state="+sess.State, loginHeadersFor(sess), nil)
	if err != nil {
		// 未完成登录时上游业务码非 0（"login ing"），这是正常中间态。
		return oauthResult{Status: "pending", Message: "等待浏览器完成登录"}, nil
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
		return oauthResult{Status: "pending", Message: "等待浏览器完成登录"}, nil
	}

	// 拿 uid / nickname（带 Bearer）；失败不致命，退化成匿名账号记录。
	var acct struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	acctHeaders := func(r *http.Request) {
		loginHeadersFor(sess)(r)
		r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	}
	if raw, _, errAcct := doUpstreamJSON(client, http.MethodGet,
		sess.Base+"/v2/plugin/login/account?state="+sess.State, acctHeaders, nil); errAcct == nil {
		_ = json.Unmarshal(raw, &acct)
	}

	uid := acct.UID
	if uid == "" {
		uid = "anonymous-" + newRandomID(6)
	}

	// ---- 重新登录（v0.31.32）：同一个 uid 已存在时不能"换个人接管" ----
	//
	// 为什么必须挡（不是"顺手加个校验"）：
	//   · UpsertAccount 的 ON CONFLICT **刻意不更新 owner_user_id**（那是业务归属），
	//     于是 B 用户扫码登录 A 用户代挂的号时，**令牌会写到 A 的账号上** ——
	//     A 看到的是自己的号"突然换了会话"，B 以为自己添加成功了；
	//   · 平台号（owner=0）被普通用户扫码同理：令牌被换，归属仍是平台。
	// 两种都会造成"谁在用什么号"彻底说不清，而排查时最需要的就是这条线索。
	prev, errPrev := store.GetAccount(uid)
	if errPrev == nil && prev != nil {
		if ownerUserID > 0 && prev.OwnerUserID != ownerUserID {
			who := "平台"
			if prev.OwnerUserID > 0 {
				who = "其他用户"
			}
			return oauthResult{}, fmt.Errorf("该账号已由%s持有，不能在这里重新登录；"+
				"如需转移，请先让原持有方删除该账号", who)
		}
	}
	// 设备凭证取本次登录**实际发出的**那一份（v0.31.40）：
	// 不这么做的话，库里存的是按 uid 事后派生的值 —— 一个从未在上游出现过的设备标识，
	// 而登录时用的是另一个。导出、迁移、审计拿到的也都是那个"从没发出去过"的值。
	devToken := sess.DeviceToken
	a := &Account{
		UID:          uid,
		Nickname:     acct.Nickname,
		Realm:        auth.ResolveRealm(sess.Realm, tok.Domain),
		EnterpriseID: acct.EnterpriseID,
		Domain:       tok.Domain,
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix(),
		OwnerUserID:  ownerUserID,
		DeviceToken:  devToken,
		// 代挂新号默认未申请入池；平台自持号由 OwnerUserID==0 隐含在池。
		PoolState: "",
	}
	// 人工停用是**管理员的裁决**，重新登录不该把它冲掉（ON CONFLICT 会写
	// excluded.disabled=false）。冷却与失败计数则要清 —— 新会话意味着那些判据失效。
	if errPrev == nil && prev != nil {
		a.Disabled = prev.Disabled
	}
	if err := store.UpsertAccount(a); err != nil {
		return oauthResult{}, fmt.Errorf("save account: %w", err)
	}
	// token_dead_at 必须显式清（v0.31.32）：ON CONFLICT 的更新列里没有它，
	// 不清的话「重新登录」成功了但账号卡仍是红的 —— 用户会以为没生效，
	// 转而反复扫码（比不标红更糟）。
	_ = store.ClearTokenDead(uid)

	oauthMu.Lock()
	delete(oauthSessions, sessionID)
	oauthMu.Unlock()
	return oauthResult{Status: "done", Account: a}, nil
}

// gcOAuthSessions 清理过期会话（低频调用即可）。
func gcOAuthSessions() {
	oauthMu.Lock()
	defer oauthMu.Unlock()
	for id, s := range oauthSessions {
		if time.Since(s.CreatedAt) > oauthTTL {
			delete(oauthSessions, id)
		}
	}
}

// newRandomID 生成 n 字节的 hex 随机串。
//
// 熵源失败即 panic（v0.24.1，P3-3）：此前退化成 RFC3339Nano 时间截串，而它被用于
// **管理密钥**（main.go）、OAuth 会话 ID、匿名账号 uid —— 时间戳可枚举。
// 见 secureBytes 的说明。
func newRandomID(n int) string {
	return hex.EncodeToString(secureBytes(n))
}
