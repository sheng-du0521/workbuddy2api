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
	return func(req *http.Request) {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.Header.Set("Origin", origin)
		req.Header.Set("Referer", origin+"/")
		req.Header.Set("User-Agent", "CLI/2.63.2 CodeBuddy/2.63.2")
	}
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
	ID        string
	State     string
	Realm     string
	Base      string
	Origin    string
	AuthURL   string
	CreatedAt time.Time
}

var (
	oauthMu       sync.Mutex
	oauthSessions = map[string]*OAuthSession{}
)

// StartOAuth 拉取设备授权 URL 并建立服务端会话。
func StartOAuth(realm string) (*OAuthSession, error) {
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

	data, _, err := doUpstreamJSON(client, http.MethodPost,
		base+"/v2/plugin/auth/state?platform=CLI", loginHeaders(origin), bytes.NewReader([]byte("{}")))
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
	oauthMu.Lock()
	oauthSessions[sess.ID] = sess
	oauthMu.Unlock()
	return sess, nil
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
		sess.Base+"/v2/plugin/auth/token?state="+sess.State, loginHeaders(sess.Origin), nil)
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
		loginHeaders(sess.Origin)(r)
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
		// 代挂新号默认未申请入池；平台自持号由 OwnerUserID==0 隐含在池。
		PoolState: "",
	}
	if err := store.UpsertAccount(a); err != nil {
		return oauthResult{}, fmt.Errorf("save account: %w", err)
	}

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
