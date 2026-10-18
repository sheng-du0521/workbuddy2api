// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package scheduler

import (
	"errors"
	"strings"
	"testing"
)

// TestFailWithKeepsReason 失败必须留下"哪个号、什么错"。
//
// 起因（2026-10-18 生产）：cat 任务 38 个号失败 12 个，台账只有 `fail 12`，
// 要查原因只能翻 journalctl（会轮转、也不在页面上）。于是"12 个号连着失败"
// 这件事一整天没人注意到。**数量决定要不要重试，原因决定要不要处置账号。**
func TestFailWithKeepsReason(t *testing.T) {
	var tl runTally
	tl.total = 3
	tl.failWith("多福(08594b14)", errors.New("query black_cat: upstream client (http 401)"))
	tl.failWith("养猫人(265b9faf)", nil)

	if tl.fail != 2 {
		t.Fatalf("fail 计数应为 2，得 %d", tl.fail)
	}
	s := tl.failSamples()
	if len(s) != 2 {
		t.Fatalf("样本应为 2 条，得 %d：%v", len(s), s)
	}
	if !strings.Contains(s[0], "多福") || !strings.Contains(s[0], "401") {
		t.Fatalf("样本必须同时带账号与错误原文，得 %q", s[0])
	}
	if !strings.Contains(s[1], "养猫人") {
		t.Fatalf("err 为 nil 时也应留下账号，得 %q", s[1])
	}
}

// TestFailSamplesTruncated 样本有上限，且必须显式说明被截断了几条。
//
// 不截断的后果是台账 JSON 随失败数线性膨胀（全池 38 个号全挂就是 38 条长字符串，
// 每轮都写盘）；静默截断的后果是"看起来只有 6 个失败"。
func TestFailSamplesTruncated(t *testing.T) {
	var tl runTally
	for i := 0; i < runFailSampleMax+4; i++ {
		tl.failWith("u", nil)
	}
	if tl.fail != runFailSampleMax+4 {
		t.Fatalf("计数不能丢：应 %d，得 %d", runFailSampleMax+4, tl.fail)
	}
	s := tl.failSamples()
	if len(s) != runFailSampleMax+1 {
		t.Fatalf("应为 %d 条样本 + 1 条截断说明，得 %d", runFailSampleMax, len(s))
	}
	last := s[len(s)-1]
	if !strings.Contains(last, "另有 4 个") {
		t.Fatalf("截断说明要写清条数，得 %q", last)
	}
}

// TestFailSamplesEmptyStaysNil 没有失败时不产生空数组（JSON 里不该多一个 []）。
func TestFailSamplesEmptyStaysNil(t *testing.T) {
	var tl runTally
	if got := tl.failSamples(); got != nil {
		t.Fatalf("无失败时应为 nil，得 %v", got)
	}
}
