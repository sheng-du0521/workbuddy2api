package main

import (
	"testing"
	"time"

	"workbuddy2api/internal/pool"
)

// ======================================================================
// v0.26.7 · 账号「已挂载时长」的边界
//
// 这个数字同时出现在管理后台账号卡与用户控制台代挂列表两处，
// 而"多久算一天"两处各写一遍必然漂移（跨零点差一天是最典型的）。
// 下面把口径与三个边界全部钉住。
// ======================================================================

// TestMountedOfUnknownZero 挂载时刻缺失（历史行 / 异常导入）必须**不可知**，
// 而不是"0 天" —— 前端据此显示"—"。"不知道挂了多久"与"刚挂上"的下一步动作相反。
func TestMountedOfUnknownZero(t *testing.T) {
	now := time.Now()
	for _, at := range []int64{0, -1, -86400} {
		got := mountedOf(at, now)
		if got.Known {
			t.Errorf("createdAt=%d 应判为未知，却得到 %+v", at, got)
		}
		if got.Text != "" {
			t.Errorf("未知时文案应为空（前端显示 —），得 %q", got.Text)
		}
		if got.Days != 0 {
			t.Errorf("未知时天数应为 0，得 %d", got.Days)
		}
	}
}

// TestMountedOfToday 「今天挂载」必须是 0 天且文案独立 ——
// 不是"已挂载 0 天"（读起来像出了 bug）。
func TestMountedOfToday(t *testing.T) {
	now := time.Date(2026, 10, 15, 15, 0, 0, 0, time.Local)
	// 今天凌晨挂上
	at := time.Date(2026, 10, 15, 3, 0, 0, 0, time.Local).Unix()
	got := mountedOf(at, now)
	if !got.Known || got.Days != 0 {
		t.Fatalf("同一天应为 0 天，得 %+v", got)
	}
	if got.Text != "今天挂载" {
		t.Errorf("文案应为「今天挂载」，得 %q", got.Text)
	}
}

// TestMountedOfDayBoundary 自然日边界：昨天 23:59 挂上、今天 00:01 查看
// → 跨了日但不足一天，仍是 1 天（按自然日算）。
//
// 这条正是两处各写一遍最容易不一致的地方：若一处用 `now.Sub(at) >= 24h`，
// 这里会得到 0 天，与另一处的 1 天冲突。
func TestMountedOfDayBoundary(t *testing.T) {
	at := time.Date(2026, 10, 14, 23, 59, 0, 0, time.Local).Unix()
	now := time.Date(2026, 10, 15, 0, 1, 0, 0, time.Local)

	got := mountedOf(at, now)
	if got.Days != 1 {
		t.Errorf("跨一个自然日应为 1 天，得 %d（若这里得到 0，说明用了 24h 秒差而不是自然日）", got.Days)
	}
	if got.Text != "已挂载 1 天" {
		t.Errorf("文案应为「已挂载 1 天」，得 %q", got.Text)
	}
}

// TestMountedOfMultiDay 多日累计（含跨月的真实场景）。
func TestMountedOfMultiDay(t *testing.T) {
	at := time.Date(2026, 10, 1, 9, 30, 0, 0, time.Local).Unix()
	now := time.Date(2026, 10, 15, 15, 0, 0, 0, time.Local)
	if got := mountedOf(at, now); got.Days != 14 {
		t.Errorf("10-01 → 10-15 应为 14 天，得 %d（%q）", got.Days, got.Text)
	}
	// 跨月：9-29 → 10-15 = 16 天
	at2 := time.Date(2026, 9, 29, 9, 0, 0, 0, time.Local).Unix()
	if got := mountedOf(at2, now); got.Days != 16 {
		t.Errorf("09-29 → 10-15 应为 16 天，得 %d", got.Days)
	}
}

// TestMountedOfFutureClampedToZero 未来时间（时钟回拨 / 跨机导入）必须钳到 0 天，
// **不能出现负数** —— "已挂载 -3 天"是荒唐文案，且会成为排查时的新疑点。
func TestMountedOfFutureClampedToZero(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.Local)
	at := now.Add(72 * time.Hour).Unix()
	got := mountedOf(at, now)
	if !got.Known {
		t.Fatal("有时刻即视为可知")
	}
	if got.Days != 0 {
		t.Errorf("未来时刻天数应钳到 0，得 %d", got.Days)
	}
	if got.Text != "今天挂载" {
		t.Errorf("未来时刻文案应回落「今天挂载」，得 %q", got.Text)
	}
	if got.At != at {
		t.Errorf("原始时刻应原样保留（前端要展示日期），得 %d vs %d", got.At, at)
	}
}

// TestMountedOfTextNeverUsesBareZero 「0 天」这种写法不该出现（会像 bug），
// 且文案里的天数必须与 Days 一致 —— 这钉的是 itoa(n<=0)→"3" 那个包内陷阱。
func TestMountedOfTextNeverUsesBareZero(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.Local)
	// 构造 N = 0..3 天，逐个检查文案与天数自洽。
	for _, wantDays := range []int{0, 1, 2, 3} {
		at := time.Date(2026, 10, 15-wantDays, 12, 0, 0, 0, time.Local).Unix()
		got := mountedOf(at, now)
		if got.Days != wantDays {
			t.Fatalf("want days=%d got %d", wantDays, got.Days)
		}
		if wantDays == 0 {
			if got.Text != "今天挂载" {
				t.Errorf("days=0 文案应为「今天挂载」，得 %q（若是「已挂载 3 天」，说明踩了包内 itoa 陷阱）", got.Text)
			}
			continue
		}
		want := "已挂载 " + string(rune('0'+wantDays)) + " 天"
		if got.Text != want {
			t.Errorf("days=%d 文案应为 %q，得 %q", wantDays, want, got.Text)
		}
	}
}

// TestMountedViewFieldsExposed 两个视图都要把挂载信息透出给前端。
//
// 只做后端计算而不透出 = 白算；这里钉住字段真的出现在 JSON 里。
func TestMountedViewFieldsExposed(t *testing.T) {
	store, px, a := newTestApp(t, &Account{UID: "u1", AccessToken: "t", Nickname: "甲"})
	defer store.Close()
	defer px.Close()

	acc := &Account{UID: "u1", Nickname: "甲", AccessToken: "t",
		CreatedAt: time.Now().AddDate(0, 0, -7).Unix()}
	v := a.toViewP(pool.Status{UID: "u1", Nickname: "甲"}, acc, cachedCreditPacks{})
	if v.MountedAt <= 0 {
		t.Error("accountView 应透出 mounted_at")
	}
	if v.MountedDays != 7 {
		t.Errorf("挂载 7 天应算出 7，得 %d（%q）", v.MountedDays, v.MountedText)
	}
	if v.MountedText == "" {
		t.Error("accountView 应透出 mounted_text")
	}

	hv := a.hostView(&Account{
		UID: "u1", Nickname: "甲", AccessToken: "t",
		CreatedAt: time.Now().AddDate(0, 0, -30).Unix(),
	})
	if hv.MountedDays != 30 {
		t.Errorf("hostAccountView 挂载 30 天应算出 30，得 %d", hv.MountedDays)
	}
	if hv.MountedText == "" || hv.MountedAt <= 0 {
		t.Errorf("hostAccountView 字段缺失：%+v", hv)
	}
}

// storeClose 关掉 newTestApp 建出来的库（它不像 newTestStore 自带 cleanup）。
