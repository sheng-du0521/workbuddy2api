// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package upstream

import (
	"crypto/md5"
	"encoding/hex"
	"testing"

	"workbuddy2api/internal/auth"
)

// 注：32 位 hex 的格式断言复用 headers_ids_test.go 的 hex32Re（同包已声明），
// 本文件不再重复定义 —— 故这里只留 wantDerived。

// wantDerived 独立算一遍期望值：直接构造 md5("<purpose>:<uid>")。
// 刻意不复用被测函数，避免「实现改了测试跟着改」的假绿。
func wantDerived(uid, purpose string) string {
	sum := md5.Sum([]byte(purpose + ":" + uid))
	return hex.EncodeToString(sum[:])
}

// TestDeriveAccountStableIDStable 同 uid + 同用途两次派生 → 恒同值（跨重启稳定语义：
// 固定盐纯派生，无随机源）。
func TestDeriveAccountStableIDStable(t *testing.T) {
	first := deriveAccountStableID("u1", "machine")
	second := deriveAccountStableID("u1", "machine")
	if first != second {
		t.Errorf("deriveAccountStableID(u1, machine) unstable: %q vs %q", first, second)
	}
}

// TestDeriveAccountStableIDDistinct 不同 uid → 不同值（账号间互异，防多号关联）。
func TestDeriveAccountStableIDDistinct(t *testing.T) {
	if deriveAccountStableID("u1", "machine") == deriveAccountStableID("u2", "machine") {
		t.Errorf("different uids derive same machine ID")
	}
}

// TestDeriveAccountStableIDPurposeIsolation 同 uid + "machine" vs "session" → 不同值
// （用途盐隔离：设备标识与账号固定会话标识不可碰撞）。
func TestDeriveAccountStableIDPurposeIsolation(t *testing.T) {
	if deriveAccountStableID("u1", "machine") == deriveAccountStableID("u1", "session") {
		t.Errorf("machine/session purposes derive same ID")
	}
}

// TestDeriveAccountStableIDFormat 输出恒为 32 hex（md5 hexdigest）。
func TestDeriveAccountStableIDFormat(t *testing.T) {
	for _, purpose := range []string{"machine", "session"} {
		got := deriveAccountStableID("uid-abc", purpose)
		if !hex32Re.MatchString(got) {
			t.Errorf("deriveAccountStableID(%q, %q) = %q, want 32 hex", "uid-abc", purpose, got)
		}
	}
}

// TestDeriveAccountStableIDMatchesPython 派生值与独立算出的 md5("<purpose>:<uid>")
// 逐字一致 —— 这是 v0.17.3 「统一口径」的核心断言。
//
// 为什么必须锁死：同一账号经「Go 请求头」与「Python 事件体」两条通道出站，若两边
// 派生值不同，上游看到的同一个号就是**两台设备**。这条断言用独立实现（wantDerived）
// 交叉验证，任何一方单方面改算法都会在这里红。
func TestDeriveAccountStableIDMatchesPython(t *testing.T) {
	for _, tc := range []struct{ uid, purpose string }{
		{"u1", "machine"},
		{"11111111-1111-4111-8111-111111111111", "machine"},
		{"11111111-1111-4111-8111-111111111111", "session"},
	} {
		if got, want := deriveAccountStableID(tc.uid, tc.purpose), wantDerived(tc.uid, tc.purpose); got != want {
			t.Errorf("deriveAccountStableID(%q, %q) = %q, want %q（应与 Python md5(salt:uid) 一致）",
				tc.uid, tc.purpose, got, want)
		}
	}
}

// TestDeriveAccountStableIDEmptyUIDReturnsEmpty 空 uid 返回空串而非固定哈希。
//
// 这一点是刻意的：若按 md5("machine:") 派生，所有空 uid 账号会得到**同一个**看似
// 正常的哈希 —— 多账号隔离实际失效却毫无痕迹。返回空串让「未注入」成为可见状态，
// 配合 UI 的「指纹异常」计数形成显性告警。
func TestDeriveAccountStableIDEmptyUIDReturnsEmpty(t *testing.T) {
	if got := deriveAccountStableID("", "machine"); got != "" {
		t.Errorf("空 uid 应返回空串（显性失败），得 %q", got)
	}
	if got := AccountMachineID(""); got != "" {
		t.Errorf("AccountMachineID(空) 应返回空串，得 %q", got)
	}
	// 导出访问器与内部派生必须同源（UI 显示值 = 真实出站值）。
	if got, want := AccountMachineID("u7"), deriveAccountStableID("u7", "machine"); got != want {
		t.Errorf("AccountMachineID 与 deriveAccountStableID 不同源：%q vs %q", got, want)
	}
	if got, want := AccountSessionID("u7"), deriveAccountStableID("u7", "session"); got != want {
		t.Errorf("AccountSessionID 与 deriveAccountStableID 不同源：%q vs %q", got, want)
	}
}

// TestCommonHeadersInjectsMachineSessionID CommonHeaders 后 req 携带 X-Machine-ID 与
// X-Session-ID，且两头互异、值与直接派生一致（账号级稳定头族全出站路径覆盖）。
func TestCommonHeadersInjectsMachineSessionID(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	req := mustRequest(t)
	c := &Client{}
	c.CommonHeaders(req, a)

	machine := req.Header.Get("X-Machine-ID")
	session := req.Header.Get("X-Session-ID")
	if !hex32Re.MatchString(machine) {
		t.Errorf("X-Machine-ID = %q, want 32 hex", machine)
	}
	if !hex32Re.MatchString(session) {
		t.Errorf("X-Session-ID = %q, want 32 hex", session)
	}
	if machine != deriveAccountStableID("u1", "machine") {
		t.Errorf("X-Machine-ID = %q, want deriveAccountStableID(u1, machine)", machine)
	}
	if session != deriveAccountStableID("u1", "session") {
		t.Errorf("X-Session-ID = %q, want deriveAccountStableID(u1, session)", session)
	}
	if machine == session {
		t.Errorf("X-Machine-ID == X-Session-ID (must differ by purpose salt)")
	}
}

// TestCommonHeadersEmptyUIDSkipsInjection uid 为空（匿名请求）→ 不注入两头且不 panic。
func TestCommonHeadersEmptyUIDSkipsInjection(t *testing.T) {
	req := mustRequest(t)
	c := &Client{}
	c.CommonHeaders(req, nil)

	if got := req.Header.Get("X-Machine-ID"); got != "" {
		t.Errorf("X-Machine-ID = %q, want empty (no auth)", got)
	}
	if got := req.Header.Get("X-Session-ID"); got != "" {
		t.Errorf("X-Session-ID = %q, want empty (no auth)", got)
	}
	req2 := mustRequest(t)
	c.CommonHeaders(req2, &auth.Auth{AccessToken: "at", UID: ""})
	if got := req2.Header.Get("X-Machine-ID"); got != "" {
		t.Errorf("X-Machine-ID = %q, want empty (empty uid)", got)
	}
	if got := req2.Header.Get("X-Session-ID"); got != "" {
		t.Errorf("X-Session-ID = %q, want empty (empty uid)", got)
	}
}

// TestChatHeadersCarriesMachineSessionID ChatHeaders 走 CommonHeaders 之上叠加 →
// 两头天然继承（chat/billing/refresh 全覆盖路径的回归锚点）。
func TestChatHeadersCarriesMachineSessionID(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u9"}
	req := mustRequest(t)
	c := &Client{}
	c.ChatHeaders(req, a, "", ChatMeta{})

	if got := req.Header.Get("X-Machine-ID"); got != deriveAccountStableID("u9", "machine") {
		t.Errorf("X-Machine-ID = %q, want deriveAccountStableID(u9, machine)", got)
	}
	if got := req.Header.Get("X-Session-ID"); got != deriveAccountStableID("u9", "session") {
		t.Errorf("X-Session-ID = %q, want deriveAccountStableID(u9, session)", got)
	}
}
