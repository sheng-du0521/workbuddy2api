// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package upstream

import (
	"net/http"
	"testing"

	"workbuddy2api/internal/auth"
)

// TestAccountDeviceTokenPerAccountDistinct 派生兜底值必须**异号互异**。
//
// 这是派生兜底能不能用的前提：一个全局常量等于给整池账号发同一张设备身份证，
// 上游一眼就能把它们聚成"同一台设备在轮着用" —— 比完全不带更容易被处理。
func TestAccountDeviceTokenPerAccountDistinct(t *testing.T) {
	a := AccountDeviceToken("uid-aaa")
	b := AccountDeviceToken("uid-bbb")
	if a == "" || b == "" {
		t.Fatal("非空 uid 必须派生非空值")
	}
	if a == b {
		t.Errorf("不同 uid 派生出同一个值：%q", a)
	}
	if AccountDeviceToken("uid-aaa") != a {
		t.Error("同 uid 派生不稳定（跨调用必须恒定）")
	}
	if AccountDeviceToken("") != "" {
		t.Error("空 uid 必须返回空串（否则全池空 uid 会撞同一个值且看起来正常）")
	}
	if !hex32Re.MatchString(a) {
		t.Errorf("形如 %q，want 32 hex", a)
	}
}

// TestAccountDeviceTokenPurposeIsolated 派生值不得与 X-Machine-ID / X-Session-ID 撞车。
//
// 三个头取同一个值本身就是"同一套模板批量生成"的迹象 —— 所以用途盐必须隔离。
func TestAccountDeviceTokenPurposeIsolated(t *testing.T) {
	uid := "u-1"
	dt := AccountDeviceToken(uid)
	if dt == AccountMachineID(uid) {
		t.Error("设备凭证兜底值与 X-Machine-ID 相同（用途盐未隔离）")
	}
	if dt == AccountSessionID(uid) {
		t.Error("设备凭证兜底值与 X-Session-ID 相同（用途盐未隔离）")
	}
}

// TestResolveDeviceTokenPrecedence 四档优先级：账号 > 全局 > 文件 > 派生。
func TestResolveDeviceTokenPrecedence(t *testing.T) {
	a := &auth.Auth{UID: "u1", DeviceToken: "acct-token"}

	// ① 账号自带（真值）最高。
	c := &Client{DeviceToken: "global-token", DeviceTokenFallback: true}
	if got := c.resolveDeviceToken(a); got != "acct-token" {
		t.Errorf("① 账号级应优先，得 %q", got)
	}
	// ② 账号为空 → 全局值。
	if got := c.resolveDeviceToken(&auth.Auth{UID: "u1"}); got != "global-token" {
		t.Errorf("② 应回落到全局值，得 %q", got)
	}
	// ③ 全局也为空 → 派生兜底（开关开）。
	c2 := &Client{DeviceTokenFallback: true}
	if got := c2.resolveDeviceToken(&auth.Auth{UID: "u1"}); got != AccountDeviceToken("u1") {
		t.Errorf("③ 应回落到派生值，得 %q", got)
	}
	// 开关关 → 不注入（保持既有行为，默认路径零变化）。
	c3 := &Client{}
	if got := c3.resolveDeviceToken(&auth.Auth{UID: "u1"}); got != "" {
		t.Errorf("④ 开关关闭时不得派生，得 %q", got)
	}
	// 无 auth（匿名）→ 即便开关打开也不派生（没有 uid 可派生）。
	if got := c2.resolveDeviceToken(nil); got != "" {
		t.Errorf("无 auth 时不得派生，得 %q", got)
	}
	// 空 uid → 不派生。
	if got := c2.resolveDeviceToken(&auth.Auth{UID: ""}); got != "" {
		t.Errorf("空 uid 不得派生，得 %q", got)
	}
}

// TestInjectDeviceTokenFallbackWritesHeader 兜底值真的会写进请求头（端到端一格）。
func TestInjectDeviceTokenFallbackWritesHeader(t *testing.T) {
	c := &Client{DeviceTokenFallback: true}
	req := mustRequest(t)
	c.injectDeviceToken(req, &auth.Auth{UID: "u-xyz"})
	if got := req.Header.Get("X-Device-Token"); got != AccountDeviceToken("u-xyz") {
		t.Errorf("X-Device-Token = %q, want 派生值", got)
	}

	// 全局值存在时优先全局，不派生。
	c2 := &Client{DeviceToken: "g", DeviceTokenFallback: true}
	req2 := mustRequest(t)
	c2.injectDeviceToken(req2, &auth.Auth{UID: "u-xyz"})
	if got := req2.Header.Get("X-Device-Token"); got != "g" {
		t.Errorf("有全局值时应用全局值，得 %q", got)
	}
}

// TestInjectDeviceTokenNilHeaderWhenAllEmpty 全空且开关关 → 不注入该头（优雅降级）。
func TestInjectDeviceTokenNilHeaderWhenAllEmpty(t *testing.T) {
	c := &Client{}
	req := mustRequest(t)
	c.injectDeviceToken(req, &auth.Auth{UID: "u1"})
	if _, ok := req.Header[http.CanonicalHeaderKey("X-Device-Token")]; ok {
		t.Error("全空时不得写入该头")
	}
}
