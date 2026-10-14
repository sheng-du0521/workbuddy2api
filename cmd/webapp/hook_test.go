// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"path/filepath"
	"testing"

	"workbuddy2api/internal/auth"
)

// TestCredentialHookWritesToSQLite 验证调度器刷新 token 后的落盘路径真的接到了
// SQLite：这是「token 刷新成功但没持久化」那个坑的回归测试。
//
// 场景还原：webapp 的账号来自数据库、Auth.FilePath 为空，上游刷新后调
// SaveAtomic —— 没有钩子时它直接返回 "no FilePath set"，刷新结果丢失。
func TestCredentialHookWritesToSQLite(t *testing.T) {
	defer auth.SetSaveHook(nil) // 全局钩子，测试后复位，避免污染其他用例

	store, err := OpenStore(filepath.Join(t.TempDir(), "hook.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	// 预置账号行：UpdateTokens 是 UPDATE，得有目标行才看得到效果。
	if err := store.UpsertAccount(&Account{UID: "u-hook", Nickname: "n", Realm: "cn", AccessToken: "old"}); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}

	installCredentialHook(store)

	a := &auth.Auth{UID: "u-hook", AccessToken: "new-at", RefreshToken: "new-rt", ExpiresAt: 1800000000}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("SaveAtomic 应经钩子落到 SQLite，实际报错: %v", err)
	}

	got, err := store.GetAccount("u-hook")
	if err != nil {
		t.Fatalf("GetAccount: %v", err)
	}
	if got == nil {
		t.Fatal("账号行消失")
	}
	if got.AccessToken != "new-at" || got.RefreshToken != "new-rt" || got.ExpiresAt != 1800000000 {
		t.Fatalf("凭证未写回库：access=%q refresh=%q exp=%d", got.AccessToken, got.RefreshToken, got.ExpiresAt)
	}
}

// TestCredentialHookSurfacesError 钩子必须如实抛错而不是静默吞掉：
// 「刷新成功但写库失败」若被吞，重启后会用旧 token 且毫无日志线索。
func TestCredentialHookSurfacesError(t *testing.T) {
	defer auth.SetSaveHook(nil)

	store, err := OpenStore(filepath.Join(t.TempDir(), "hook2.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()
	installCredentialHook(store)

	a := &auth.Auth{AccessToken: "at"} // UID 为空，钩子应拒绝
	if err := a.SaveAtomic(); err == nil {
		t.Fatal("UID 为空时钩子应返回错误，实际 nil")
	}
}
