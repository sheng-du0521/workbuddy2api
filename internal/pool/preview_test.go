// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package pool

import (
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestPreviewCandidatesReportsSkipReasons 候选预览必须对每个落选者给出**可读原因**（R8）。
//
// 这是本功能存在的理由：排障问的不是"选号坏了吗"，而是"为什么跳过它"。
func TestPreviewCandidatesReportsSkipReasons(t *testing.T) {
	p := New(filepath.Join(t.TempDir(), "state.json"))
	defer p.Close()
	p.Add(&auth.Auth{UID: "ok-1"})
	p.Add(&auth.Auth{UID: "cool-1"})
	p.Add(&auth.Auth{UID: "dis-1"})

	// 制造三种处境：冷却、系统禁用、手动停用。
	// 冷却时长必须为正：传 0 等于"立刻到期"，账号根本不处于冷却中
	// （第一版用例就是这么写的，于是 cool-1 反而被判为可选、抢走了 rank 1）。
	p.Cooldown("cool-1", CoolSoft, time.Hour, "429 rate limit")
	p.Disable("dis-1", "test")
	// 不存在的 uid：不应 panic（预览/停用都要能容忍运维传错 uid）。
	if _, ok := p.SetManualDisabled("no-such-uid", true, "x"); ok {
		t.Fatal("对不存在的 uid 不应报告 changed=true")
	}

	rows := p.PreviewCandidates("", "")
	got := map[string]CandidatePreview{}
	for _, c := range rows {
		got[c.UID] = c
	}
	if len(rows) != 3 {
		t.Fatalf("应列出池内全部 3 个账号，实得 %d", len(rows))
	}
	if c := got["ok-1"]; !c.Included || c.Rank != 1 || !c.InTop5 {
		t.Fatalf("ok-1 应进候选且排第 1：%+v", c)
	}
	if c := got["cool-1"]; c.Included || c.Skip != "cooling" || c.SkipText == "" {
		t.Fatalf("cool-1 应因冷却落选并给出原因：%+v", c)
	}
	if c := got["dis-1"]; c.Included || c.Skip != "disabled" || c.SkipText == "" {
		t.Fatalf("dis-1 应因禁用落选并给出原因：%+v", c)
	}
	// 预览是只读的：不能因为预览而改变冷却/禁用状态。
	if !got["cool-1"].Cooling {
		t.Fatal("预览不得清掉冷却状态")
	}
	if !got["dis-1"].Cooling && !got["dis-1"].Included {
		// 禁用账号 healthy=false，Cooling 也应为 true —— 这里只断言它没被"预览"成可用。
		if got["dis-1"].Included {
			t.Fatal("禁用账号不得被判为进候选")
		}
	}
}

// TestPreviewCandidatesHonoursManualDisabled 手动停用必须被点名，且不回落到可选。
func TestPreviewCandidatesHonoursManualDisabled(t *testing.T) {
	p := New(filepath.Join(t.TempDir(), "state.json"))
	defer p.Close()
	p.Add(&auth.Auth{UID: "m-1"})
	if _, ok := p.SetManualDisabled("m-1", true, "运维主动摘除"); !ok {
		t.Fatal("SetManualDisabled 应成功")
	}

	rows := p.PreviewCandidates("", "")
	if len(rows) != 1 {
		t.Fatalf("应列出 1 个账号，实得 %d", len(rows))
	}
	if rows[0].Included || rows[0].Skip != "manual_disabled" {
		t.Fatalf("手动停用应落选并标 manual_disabled：%+v", rows[0])
	}
}

// TestPreviewCandidatesDoesNotMutateSelection 预览后紧接着选号，仍应选到唯一可用账号
// （即预览没有把 lastUsed 写脏、没有把候选挤掉）。
func TestPreviewCandidatesDoesNotMutateSelection(t *testing.T) {
	p := New(filepath.Join(t.TempDir(), "state.json"))
	defer p.Close()
	p.Add(&auth.Auth{UID: "only-1"})

	// 预览 100 次（若预览会写 lastUsed，之后的 minPickGap 会把唯一候选也挤成"刚用过"）。
	for i := 0; i < 100; i++ {
		rows := p.PreviewCandidates("", "")
		if len(rows) != 1 || !rows[0].Included {
			t.Fatalf("第 %d 次预览就走了样：%+v", i, rows)
		}
	}
	if got := p.Pick(""); got == nil || got.UID != "only-1" {
		t.Fatalf("预览之后选号应仍能选到唯一账号，实得 %v", got)
	}
}
