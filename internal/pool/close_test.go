// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package pool

import (
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestCloseStopsFlusher Close 应停止后台落盘 goroutine(否则每次 New 泄漏一个)。
func TestCloseStopsFlusher(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	before := runtime.NumGoroutine()
	p.Close()
	// 给 flusher goroutine 一个退出窗口。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	after := runtime.NumGoroutine()
	if after > before {
		t.Errorf("goroutine count after Close: before=%d after=%d (flusher 未退出)", before, after)
	}
}

// TestCloseIdempotent 多次 Close 不 panic(closeOnce + Flush 幂等)。
func TestCloseIdempotent(t *testing.T) {
	p := New(filepath.Join(t.TempDir(), "state.json"))
	p.Add(&auth.Auth{UID: "u1"})
	p.Close()
	p.Close() // 第二次不应 panic
}

// TestCloseNoStateFile 无 stateFp(未起 flusher)时 Close 安全。
func TestCloseNoStateFile(t *testing.T) {
	p := New("")
	p.Close() // stopCh 为 nil,仅做一次 Flush,不应 panic
}
