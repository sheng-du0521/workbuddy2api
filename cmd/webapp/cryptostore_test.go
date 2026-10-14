// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// newTestBox 造一个临时主密钥的加密箱（测试用）。
func newTestBox(t *testing.T) *SecretBox {
	t.Helper()
	box, err := LoadOrCreateSecret(filepath.Join(t.TempDir(), "secret.key"))
	if err != nil {
		t.Fatalf("LoadOrCreateSecret: %v", err)
	}
	if box == nil {
		t.Fatal("加密箱为空")
	}
	return box
}

// rawToken 直读库内裸列（绕过解密边界），用于确认「落盘确实是密文」。
func rawToken(t *testing.T, store *Store, uid, col string) string {
	t.Helper()
	var v string
	if err := store.db.QueryRow(`SELECT `+col+` FROM accounts WHERE uid = ?`, uid).Scan(&v); err != nil {
		t.Fatalf("raw select %s: %v", col, err)
	}
	return v
}

// TestAccountTokenEncryptedAtRest 核心断言：账号 token 落盘为密文，读取回明文。
func TestAccountTokenEncryptedAtRest(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "enc.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()
	store.SetSecret(newTestBox(t))

	const plainAT = "ey-plain-access-token"
	const plainRT = "ey-plain-refresh-token"
	if err := store.UpsertAccount(&Account{UID: "u1", Realm: "cn", AccessToken: plainAT, RefreshToken: plainRT, DeviceToken: "dt-1"}); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}

	atRaw := rawToken(t, store, "u1", "access_token")
	if atRaw == plainAT || !strings.HasPrefix(atRaw, encPrefix) {
		t.Fatalf("access_token 未加密落盘: %q", atRaw)
	}
	if rtRaw := rawToken(t, store, "u1", "refresh_token"); rtRaw == plainRT || !strings.HasPrefix(rtRaw, encPrefix) {
		t.Fatalf("refresh_token 未加密落盘: %q", rtRaw)
	}
	if dtRaw := rawToken(t, store, "u1", "device_token"); dtRaw == "dt-1" || !strings.HasPrefix(dtRaw, encPrefix) {
		t.Fatalf("device_token 未加密落盘: %q", dtRaw)
	}

	// 读取边界必须解密回明文，上层无感。
	got, err := store.GetAccount("u1")
	if err != nil || got == nil {
		t.Fatalf("GetAccount: %v", err)
	}
	if got.AccessToken != plainAT || got.RefreshToken != plainRT || got.DeviceToken != "dt-1" {
		t.Fatalf("读取未解密回明文: at=%q rt=%q dt=%q", got.AccessToken, got.RefreshToken, got.DeviceToken)
	}
}

// TestAccountTokenLegacyPlaintextPassthrough 升级路径：老明文行不改就已可用。
func TestAccountTokenLegacyPlaintextPassthrough(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()
	store.SetSecret(newTestBox(t))

	// 模拟 v0.5 之前写入的明文行（直接 SQL，不经加密边界）。
	_, err = store.db.Exec(`INSERT INTO accounts (uid, realm, access_token, refresh_token, created_at, updated_at)
		VALUES ('u-old','cn','legacy-at','legacy-rt',0,0)`)
	if err != nil {
		t.Fatalf("insert legacy: %v", err)
	}

	got, err := store.GetAccount("u-old")
	if err != nil || got == nil {
		t.Fatalf("GetAccount: %v", err)
	}
	if got.AccessToken != "legacy-at" || got.RefreshToken != "legacy-rt" {
		t.Fatalf("老明文行应原样透传，实际 at=%q rt=%q", got.AccessToken, got.RefreshToken)
	}

	// 老行在下一次写回（token 刷新/保活）后应转为密文。
	if err := store.UpdateTokens("u-old", "new-at", "new-rt", 1800000000); err != nil {
		t.Fatalf("UpdateTokens: %v", err)
	}
	if raw := rawToken(t, store, "u-old", "access_token"); raw != "" && !strings.HasPrefix(raw, encPrefix) {
		t.Fatalf("写回后仍未加密: %q", raw)
	}
	if got, _ := store.GetAccount("u-old"); got.AccessToken != "new-at" {
		t.Fatalf("写回后读取应得明文，实际 %q", got.AccessToken)
	}
}

// TestAccountTokenNoSecret：未启用加密时行为与旧版一致（明文落盘）。
func TestAccountTokenNoSecret(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "nosecret.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()
	// 不调用 SetSecret（等价 -secret none）。

	if err := store.UpsertAccount(&Account{UID: "u1", Realm: "cn", AccessToken: "plain-at"}); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	if raw := rawToken(t, store, "u1", "access_token"); raw != "plain-at" {
		t.Fatalf("未启用加密时应明文落盘，实际 %q", raw)
	}
}

// TestAccountTokenCiphertextNotReencrypted：已是密文再走一次写入不应二次加密（幂等）。
func TestAccountTokenCiphertextNotReencrypted(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "idem.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()
	box := newTestBox(t)
	store.SetSecret(box)

	// 手工造一个合法密文，模拟「读出的值被原样写回」的路径。
	enc, err := box.Encrypt("secret-at")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	prefixed := encPrefix + enc
	got := store.encField(prefixed)
	if got != prefixed {
		t.Fatalf("已加密值被二次加密: %q", got)
	}
	if plain := store.decField(prefixed); plain != "secret-at" {
		t.Fatalf("解密失败: %q", plain)
	}
}
