// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package auth

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestSaveAtomicWithoutFileUsesHook 无 FilePath 的凭证（数据库部署）应由注入的
// 落盘钩子接管，且钩子拿到的是完整快照。
func TestSaveAtomicWithoutFileUsesHook(t *testing.T) {
	defer SetSaveHook(nil)

	var (
		mu    sync.Mutex
		got   []SavePayload
		calls int
	)
	SetSaveHook(func(p SavePayload) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		got = append(got, p)
		return nil
	})

	a := &Auth{
		AccessToken:  "at-1",
		RefreshToken: "rt-1",
		ExpiresAt:    1700000000,
		Domain:       "copilot.tencent.com",
		realm:        "cn",
		UID:          "u-1",
		Nickname:     "nick",
		EnterpriseID: "ent-1",
		DeviceToken:  "dev-1",
	}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("hook 调用次数 = %d, want 1", calls)
	}
	p := got[0]
	if p.UID != "u-1" || p.AccessToken != "at-1" || p.RefreshToken != "rt-1" ||
		p.ExpiresAt != 1700000000 || p.Realm != "cn" || p.Domain != "copilot.tencent.com" ||
		p.DeviceToken != "dev-1" || p.Nickname != "nick" || p.EnterpriseID != "ent-1" {
		t.Fatalf("快照字段不完整: %+v", p)
	}
}

// TestSaveAtomicNoFileNoHook 钩子缺席时必须保持引入钩子前的行为：
// 返回 no FilePath 错误（而不是静默成功）。
func TestSaveAtomicNoFileNoHook(t *testing.T) {
	SetSaveHook(nil)
	a := &Auth{AccessToken: "at", UID: "u"}
	if err := a.SaveAtomic(); err == nil {
		t.Fatal("无 FilePath 且无钩子时应报错，实际返回 nil")
	}
}

// TestSaveAtomicWithFileSkipsHook 有来源文件时走原路径写文件，钩子不得被调用——
// 否则同一份凭证会被写两次（一次文件一次数据库），产生双写源。
func TestSaveAtomicWithFileSkipsHook(t *testing.T) {
	defer SetSaveHook(nil)
	called := false
	SetSaveHook(func(SavePayload) error { called = true; return nil })

	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy.json")
	a := &Auth{AccessToken: "at", RefreshToken: "rt", UID: "u", FilePath: fp}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}
	if called {
		t.Fatal("有 FilePath 时不应调用落盘钩子")
	}
	if b, err := os.ReadFile(fp); err != nil || len(b) == 0 {
		t.Fatalf("文件未写入: err=%v len=%d", err, len(b))
	}
}

// TestSaveAtomicEmptyTokenStillRefused 空 accessToken 的拒绝早于钩子分派，
// 避免把空凭证覆盖进数据库。
func TestSaveAtomicEmptyTokenStillRefused(t *testing.T) {
	defer SetSaveHook(nil)
	called := false
	SetSaveHook(func(SavePayload) error { called = true; return nil })

	a := &Auth{UID: "u"} // AccessToken 为空
	if err := a.SaveAtomic(); err == nil {
		t.Fatal("空 accessToken 应被拒绝")
	}
	if called {
		t.Fatal("空 accessToken 不应触达落盘钩子")
	}
}

// TestSnapshotIsOpaqueCopy 快照必须是值拷贝：调用方改它不得影响原对象。
func TestSnapshotIsOpaqueCopy(t *testing.T) {
	a := &Auth{AccessToken: "at", RefreshToken: "rt", UID: "u", realm: "cn"}
	snap := a.Snapshot()
	snap.AccessToken = "tampered"
	if a.AccessToken != "at" {
		t.Fatal("快照改动污染了原 Auth")
	}
	var nilAuth *Auth
	if got := nilAuth.Snapshot(); got != (SavePayload{}) {
		t.Fatalf("nil Auth 快照应为零值，实际 %+v", got)
	}
}
