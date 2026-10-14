// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"testing"
	"time"

	"workbuddy2api/internal/session"
)

// TestStickyBindingSurvivesRestart 粘性绑定必须跨进程重启保持。
//
// 这条锁的是「调用过程中换号」的一个**可复现**来源：此前 Store 是 redisstore.Noop，
// 粘性纯内存 —— 重启后同一场对话的下一个请求在可用集里重新哈希分配，
// 前后两半由两个不同账号服务（消耗两个账号的积分）。
// 现在落到 sticky_binds 表，重启后用 LoadFromStore 读回。
//
// 测试刻意走「两个 Router 实例 + 两次 store 构造」来模拟重启，而不是只看同一进程内
// 内存命中 —— 只有真正落库又读回，才算证明了持久化闭环。
func TestStickyBindingSurvivesRestart(t *testing.T) {
	store, _, _ := newTestApp(t)
	defer store.Close()

	const wantUID = "uid-b"
	avail := []string{"uid-a", "uid-b"}
	newRouter := func(s *stickyStore) *session.Router {
		return session.New(session.Config{
			TTL:               time.Hour,
			GCInterval:        time.Hour, // 不启 GC，避免干扰
			Store:             s,
			Available:         func() []string { return avail },
			AvailableForModel: func(string) []string { return avail },
		})
	}

	// ── 第一次运行：绑定 conv-1 → uid-b，然后「退出进程」。 ──
	s1 := newStickyStore(store.db)
	r1 := newRouter(s1)
	r1.Bind("conv-1", wantUID)
	if got, ok := r1.ResolveForModel("conv-1", "m"); !ok || got != wantUID {
		t.Fatalf("同进程内应命中绑定，得 (%q, %v)", got, ok)
	}
	// Close 要排空异步队列 —— 否则刚写的绑定可能还没落库就被「重启」。
	if err := s1.Close(); err != nil {
		t.Fatalf("close stickyStore: %v", err)
	}
	r1.StopGC()

	// ── 第二次运行（模拟重启）：内存是空的，靠 LoadFromStore 恢复。 ──
	s2 := newStickyStore(store.db)
	defer s2.Close()
	r2 := newRouter(s2)
	r2.LoadFromStore()

	got, ok := r2.ResolveForModel("conv-1", "m")
	if !ok {
		t.Fatal("重启后应能从持久层恢复出绑定")
	}
	if got != wantUID {
		t.Fatalf("重启后粘性应恢复为 %q，得 %q（说明绑定没落库，重启即换号）", wantUID, got)
	}
}

// TestStickyStoreDelBindPersists 解绑也必须落库：否则重启后「已失效的绑定」会复活，
// 把会话又钉回一个当初被判定不可用的账号。
func TestStickyStoreDelBindPersists(t *testing.T) {
	store, _, _ := newTestApp(t)
	defer store.Close()

	s1 := newStickyStore(store.db)
	s1.SetBind("conv-x", "uid-a", time.Hour)
	s1.DelBind("conv-x")
	if err := s1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2 := newStickyStore(store.db)
	defer s2.Close()
	if binds := s2.LoadBinds(); len(binds) != 0 {
		t.Fatalf("解绑后不应残留绑定，得 %v", binds)
	}
}

// TestStickyStoreExpiredBindNotLoaded 过期绑定不该在冷启动时被恢复：
// 否则一个半小时前的对话会被钉回旧账号（上游侧上下文早已不可用）。
func TestStickyStoreExpiredBindNotLoaded(t *testing.T) {
	store, _, _ := newTestApp(t)
	defer store.Close()

	s1 := newStickyStore(store.db)
	s1.SetBind("conv-old", "uid-a", 50*time.Millisecond)
	time.Sleep(80 * time.Millisecond)
	if err := s1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2 := newStickyStore(store.db)
	defer s2.Close()
	if binds := s2.LoadBinds(); len(binds) != 0 {
		t.Fatalf("过期绑定不应被恢复，得 %v", binds)
	}
	// 顺带清理：过期行必须被删掉，否则长期运行会缓慢堆积（内存 GC 管不到
	// 「过期前进程就退出」的那些行）。
	var n int
	if err := store.db.QueryRow(
		`SELECT COUNT(*) FROM sticky_binds WHERE bind_key = 'conv-old'`).Scan(&n); err != nil {
		t.Fatalf("查库: %v", err)
	}
	if n != 0 {
		t.Fatalf("过期绑定行应被清理，残留 %d 行", n)
	}
}
