// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package usage

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// host_earnings 只读聚合的独立测试（v0.31.13，P2-7 第四刀第二刀随迁）。
//
// 这三条查询原本住在 cmd/webapp/store_user.go，只能通过整个 app 间接跑到；
// 搬到本包后可以用临时 SQLite 直接钉住"哪些行算进来"，正好补上此前缺的边界。
//
// 钉住的三个口径（都是"错了不崩、只会给出错误数字"）：
//  1. **窗口 + 本地自然日**：窗口起点是 LocalDayStart，边界秒（today0-1）属于昨天；
//     分组用 (created_at+off)/86400，与窗口起点同源，两者不会错位；
//  2. **按模型聚合的次序**：收益降序 → 笔数降序（同收益时次序必须稳定，
//     否则界面每次刷新顺序都在抖）；
//  3. ⚠️ **汇总与窗口是两个口径**：HostEarningSummary 是**全时段**（没有 days 参数，
//     也没有 created_at 过滤）—— 落在窗口之外的历史行**必须**被算进 total。
//     给它"顺手加个窗口"会让用户看到"一共赚了多少"随天数参数变来变去。
const hostEarnSchema = `
CREATE TABLE host_earnings (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  account_uid   TEXT NOT NULL DEFAULT '',
  owner_user_id INTEGER NOT NULL,
  from_user_id  INTEGER NOT NULL DEFAULT 0,
  model         TEXT NOT NULL DEFAULT '',
  charged_micro INTEGER NOT NULL DEFAULT 0,
  earned_micro  INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL DEFAULT 0
);`

type earnFixture struct {
	q      *Queries
	today0 int64
	yday0  int64
}

// newEarnFixture 插一批确定性数据：
//
//	owner 1（本用例的号主）
//	  · 今天  glm-a  charged 100000  earned 10000
//	  · 今天  glm-a  charged 200000  earned 20000   ← 与上一笔同模型 → 按模型聚合应合并为 1 行 2 笔
//	  · 今天  glm-b  charged 300000  earned 5000
//	  · 昨天  glm-c  charged 400000  earned 40000   ← days=1 时不该进
//	  · 边界秒（今天零点前一秒）glm-d earned 999    ← 属于昨天，days=1 时不该进
//	  · 400 天前 glm-old earned 7000000             ← **仅用于证明汇总不设窗口**
//	owner 2（另一个号主，任何查询都不该把它的行算给 owner 1）
//	  · 今天  glm-a  earned 88888888
func newEarnFixture(t *testing.T) *earnFixture {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "he.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(hostEarnSchema); err != nil {
		t.Fatalf("建表: %v", err)
	}
	today0 := LocalDayStart(1)
	yday0 := LocalDayStart(2)

	ins := func(owner int64, model string, charged, earned, at int64) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO host_earnings
			(account_uid, owner_user_id, from_user_id, model, charged_micro, earned_micro, created_at)
			VALUES('acct',?,?,?,?,?,?)`, owner, 9, model, charged, earned, at); err != nil {
			t.Fatalf("插收益: %v", err)
		}
	}
	ins(1, "glm-a", 100000, 10000, today0+3600)
	ins(1, "glm-a", 200000, 20000, today0+7200)
	ins(1, "glm-b", 300000, 5000, today0+10800)
	ins(1, "glm-c", 400000, 40000, yday0+3600)
	ins(1, "glm-d", 0, 999, today0-1)
	ins(1, "glm-old", 0, 7000000, today0-400*86400)
	ins(2, "glm-a", 0, 88888888, today0+3600)

	return &earnFixture{q: New(db), today0: today0, yday0: yday0}
}

// TestHostEarningsDailyWindowAndLocalDay 窗口起点 + 本地自然日分桶 + 号主隔离。
func TestHostEarningsDailyWindowAndLocalDay(t *testing.T) {
	f := newEarnFixture(t)

	// days=1：只有今天那三笔（两笔 glm-a + 一笔 glm-b），边界秒与昨天都不进。
	got, err := f.q.HostEarningsDaily(1, 1)
	if err != nil {
		t.Fatalf("HostEarningsDaily(days=1): %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("days=1 应只有 1 个日桶，得 %d（%+v）", len(got), got)
	}
	row := got[0]
	if row.Requests != 3 {
		t.Fatalf("今天应有 3 笔，得 %d", row.Requests)
	}
	if row.ChargedMicro != 600000 || row.EarnedMicro != 35000 {
		t.Fatalf("今天合计应为 charged=600000 / earned=35000，得 %d / %d", row.ChargedMicro, row.EarnedMicro)
	}
	// 日桶的 key 必须是**本地日期字符串**（东八区下 today0+3600 与 today0 同日）。
	if wantDay := time.Unix(f.today0, 0).Format("2006-01-02"); row.Day != wantDay {
		t.Fatalf("日桶应为本地日期 %s，得 %s", wantDay, row.Day)
	}

	// days=2：昨天的 glm-c 与边界秒 glm-d 合成昨天的桶；仍不含 400 天前那笔。
	got2, err := f.q.HostEarningsDaily(1, 2)
	if err != nil {
		t.Fatalf("HostEarningsDaily(days=2): %v", err)
	}
	if len(got2) != 2 {
		t.Fatalf("days=2 应有 2 个日桶，得 %d（%+v）", len(got2), got2)
	}
	// 顺序按日升序（前端直接画折线，倒序会画出一条从右往左的线）。
	if got2[0].Day >= got2[1].Day {
		t.Fatalf("日桶应按日升序，得 %s → %s", got2[0].Day, got2[1].Day)
	}
	if got2[1].Requests != 3 {
		t.Fatalf("今天桶仍应是 3 笔，得 %d", got2[1].Requests)
	}
	if got2[0].Requests != 2 {
		t.Fatalf("昨天桶应含 glm-c 与边界秒 glm-d 共 2 笔，得 %d", got2[0].Requests)
	}
	if got2[0].EarnedMicro != 40999 {
		t.Fatalf("昨天桶收益应为 40000+999=40999，得 %d", got2[0].EarnedMicro)
	}

	// 号主隔离：owner 2 的 88888888 绝不能被算进 owner 1。
	for _, r := range append(append([]HostEarnDailyStat{}, got...), got2...) {
		if r.EarnedMicro > 8000000 {
			t.Fatalf("混进了别的号主的收益：%+v", r)
		}
	}
}

// TestHostEarningsByModelOrdering 按模型聚合：收益降序 → 笔数降序；窗口生效。
func TestHostEarningsByModelOrdering(t *testing.T) {
	f := newEarnFixture(t)

	got, err := f.q.HostEarningsByModel(1, 1)
	if err != nil {
		t.Fatalf("HostEarningsByModel(days=1): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("今天只有 glm-a / glm-b 两个模型，得 %d（%+v）", len(got), got)
	}
	if got[0].Model != "glm-a" || got[0].EarnedMicro != 30000 || got[0].Requests != 2 {
		t.Fatalf("glm-a 应合并为 2 笔 / 收益 30000，得 %+v", got[0])
	}
	if got[1].Model != "glm-b" || got[1].EarnedMicro != 5000 {
		t.Fatalf("次序应为收益降序（glm-a 30000 > glm-b 5000），得 %+v", got[1])
	}
	// 昨天/边界秒/400 天前的模型都不该出现。
	for _, r := range got {
		if r.Model == "glm-c" || r.Model == "glm-d" || r.Model == "glm-old" {
			t.Fatalf("窗口外的模型混进来了：%+v", r)
		}
	}
}

// TestHostEarningSummaryIsAllTime 汇总是**全时段**：窗口之外的历史行必须被算进 total。
func TestHostEarningSummaryIsAllTime(t *testing.T) {
	f := newEarnFixture(t)

	total, count, err := f.q.HostEarningSummary(1)
	if err != nil {
		t.Fatalf("HostEarningSummary: %v", err)
	}
	// owner 1 共 6 笔：10000+20000+5000+40000+999+7000000 = 7075999
	if count != 6 {
		t.Fatalf("全时段笔数应为 6（含 400 天前那笔），得 %d", count)
	}
	if total != 7075999 {
		t.Fatalf("全时段收益应为 7075999（含 400 天前的 7000000），得 %d", total)
	}

	// 对照：窗口求和明显更小 —— 两个口径**必须不同**，相等就意味着有人给汇总加了窗口。
	win, err := f.q.HostEarningsDaily(1, 1)
	if err != nil {
		t.Fatalf("HostEarningsDaily: %v", err)
	}
	var winSum int64
	for _, r := range win {
		winSum += r.EarnedMicro
	}
	if winSum == total {
		t.Fatalf("窗口求和(%d)与全时段汇总(%d)相等 —— 夹具失效或无窗口差异，本用例失去意义", winSum, total)
	}

	// 无数据的号主：0 / 0（而不是报错）。
	t2, c2, err := f.q.HostEarningSummary(999)
	if err != nil {
		t.Fatalf("HostEarningSummary(空): %v", err)
	}
	if t2 != 0 || c2 != 0 {
		t.Fatalf("无收益号主应为 0/0，得 %d/%d", t2, c2)
	}
}
