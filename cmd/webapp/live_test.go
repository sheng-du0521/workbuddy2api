// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"testing"
	"time"
)

// TestLiveSnapshotZeroFilled 序列必须按分钟零填充：没请求的那一分钟也要有点，
// 否则前端把「空闲」误读成「时间轴跳变」。
func TestLiveSnapshotZeroFilled(t *testing.T) {
	l := newLiveStats()
	now := time.Now().Unix()
	l.record(liveEvent{At: now, Model: "m", OK: true, Tokens: 100, Charged: 0.05})
	l.record(liveEvent{At: now, Model: "m", OK: false, Err: "boom"})

	series, tot, events, inflight := l.snapshot(60)
	if len(series) != 60 {
		t.Fatalf("序列长度 = %d，期望 60", len(series))
	}
	if inflight != 0 {
		t.Fatalf("在途 = %d，期望 0", inflight)
	}
	// 最后一分钟必然是本分钟，承载刚记的两条。
	last := series[len(series)-1]
	if last.Req != 2 || last.OK != 1 || last.Fail != 1 {
		t.Fatalf("当分钟桶 = %+v，期望 req=2 ok=1 fail=1", last)
	}
	if last.Tokens != 100 {
		t.Fatalf("当分钟 tokens = %d，期望 100", last.Tokens)
	}
	// 其余分钟必须为零值（不凭空造数）。
	if series[0].Req != 0 {
		t.Fatalf("历史分钟应为 0，实得 %d", series[0].Req)
	}
	if tot.Req != 2 || tot.Fail != 1 {
		t.Fatalf("窗口合计 = %+v", tot)
	}
	if len(events) != 2 || events[0].OK {
		t.Fatalf("事件流最新在前且首条应为失败，实得 %+v", events)
	}
}

// TestLivePrunesOldBuckets 只保留近 90 分钟，更早的桶要清掉，避免内存无界增长。
func TestLivePrunesOldBuckets(t *testing.T) {
	l := newLiveStats()
	old := time.Now().Unix() - 5*3600 // 5 小时前
	l.record(liveEvent{At: old, Model: "m", OK: true})
	l.record(liveEvent{At: time.Now().Unix(), Model: "m", OK: true})

	l.mu.Lock()
	n := len(l.buckets)
	l.mu.Unlock()
	if n != 1 {
		t.Fatalf("过期桶未清理：buckets=%d，期望 1", n)
	}
	_, tot, _, _ := l.snapshot(60)
	if tot.Req != 1 {
		t.Fatalf("窗口内合计 = %d，期望 1（旧桶不应计入）", tot.Req)
	}
}

// TestLiveInflight 在途计数随 begin/done 增减，且不会掉到负数。
func TestLiveInflight(t *testing.T) {
	l := newLiveStats()
	l.begin()
	l.begin()
	if got := l.inflightCount(); got != 2 {
		t.Fatalf("在途 = %d，期望 2", got)
	}
	l.done()
	l.done()
	l.done() // 多减一次：应钳在 0
	if got := l.inflightCount(); got != 0 {
		t.Fatalf("在途 = %d，期望被钳在 0", got)
	}
}

// TestLiveEventCapRing 事件流封顶（默认 40），最旧的被挤出。
func TestLiveEventCapRing(t *testing.T) {
	l := newLiveStats()
	now := time.Now().Unix()
	for i := 0; i < 60; i++ {
		l.record(liveEvent{At: now, Model: "m", OK: true, Tokens: i})
	}
	_, _, events, _ := l.snapshot(60)
	if len(events) != l.maxEv {
		t.Fatalf("事件流长度 = %d，期望封顶 %d", len(events), l.maxEv)
	}
	if events[0].Tokens != 59 {
		t.Fatalf("最新事件应为最后一条（Tokens=59），实得 %d", events[0].Tokens)
	}
}

func (l *liveStats) inflightCount() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inflight
}
