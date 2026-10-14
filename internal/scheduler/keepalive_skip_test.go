// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package scheduler

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestKeepaliveSkip 惰性刷新判定：token 剩余有效期超出阈值时跳过，其余情况刷新。
func TestKeepaliveSkip(t *testing.T) {
	now := time.Now()
	nowUnix := now.Unix()

	// 缺省（两个判据都为 0）：恒不跳过 —— 与引入前逐字一致（无条件刷新）。
	s := New(Config{})
	if s.keepaliveSkip(&auth.Auth{UID: "a", ExpiresAt: nowUnix + 365*86400}) {
		t.Fatal("缺省配置不应跳过（无条件刷新）")
	}

	// 惰性刷新 6 小时：剩余 12 小时 → 跳过。
	s = New(Config{LazyRefreshHours: 6})
	if !s.keepaliveSkip(&auth.Auth{UID: "a", ExpiresAt: nowUnix + 12*3600}) {
		t.Fatal("剩余 12h > 惰性窗口 6h，应跳过")
	}
	// 剩余 3 小时 → 不跳过（进入刷新窗口）。
	if s.keepaliveSkip(&auth.Auth{UID: "a", ExpiresAt: nowUnix + 3*3600}) {
		t.Fatal("剩余 3h < 惰性窗口 6h，不应跳过")
	}

	// 保活阈值 7 天：剩余 10 天 → 跳过；剩余 3 天 → 不跳过。
	s = New(Config{KeepaliveDays: 7})
	if !s.keepaliveSkip(&auth.Auth{UID: "a", ExpiresAt: nowUnix + 10*86400}) {
		t.Fatal("剩余 10d > 阈值 7d，应跳过")
	}
	if s.keepaliveSkip(&auth.Auth{UID: "a", ExpiresAt: nowUnix + 3*86400}) {
		t.Fatal("剩余 3d < 阈值 7d，不应跳过")
	}

	// 无过期时间信息（ExpiresAt<=0）：宁可刷，不跳过。
	if s.keepaliveSkip(&auth.Auth{UID: "a", ExpiresAt: 0}) {
		t.Fatal("无过期时间信息时不应跳过")
	}
}
