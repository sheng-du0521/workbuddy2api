// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ======================================================================
// v0.29.0 · 提现审核（审核方式 + 三方一致性）
//
// 用户报的两个现象：
//  1.「后台缺少提现审核方式管理，无法进行审核操作」；
//  2.「审核通过后应默认该笔提现成功，并自动扣除对应冻结金额」。
//
// 第 1 条的**真正根因**是一个字段名事故（见下 TestAdminWithdrawalsFieldNames）：
// 列表接口内嵌了没有 json tag 的 *Withdrawal，下发的是 Go 字段名
// （`ID`/`Status`/`CreatedAt`…），而前端读 snake_case（`id`/`status`/`created_at`）。
// 于是 `x.status === 'pending'` 恒为 false —— **审核按钮根本不渲染**，
// 而接口一直返回 200、没有任何报错。
// ======================================================================

// wdApp 装配提现用例用的 app + 真 mux（走 X-Admin-Key 通道，避免依赖会话 Cookie）。
func wdApp(t *testing.T) (*Store, *app, *http.ServeMux) {
	t.Helper()
	store, ap, mux := newP3App(t)
	return store, ap, mux
}

// wdReq 打一次管理接口（真 mux + 管理密钥头）。
func wdReq(t *testing.T, mux *http.ServeMux, method, target, body string) (int, map[string]any) {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Admin-Key", "p3-admin-key")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	var out map[string]any
	_ = json.Unmarshal([]byte(rec.Body.String()), &out)
	return rec.Code, out
}

// mustKV 读一个 kv 值并断言它非空（省掉每条用例重复的 err 处理）。
func mustKV(t *testing.T, store *Store, key string) string {
	t.Helper()
	v, err := store.GetKV(key)
	if err != nil {
		t.Fatalf("GetKV(%s): %v", key, err)
	}
	return v
}

// applyWithdraw 直接打用户侧提现 handler（与生产同一条代码路径，只是跳过 userGuard）。
//
// 为什么不走真 mux：`/api/wallet/withdraw` 是 userGuard（会话/Cookie）通道，
// 而本文件的其它用例走 X-Admin-Key；混两条鉴权通道会让"用例失败"的原因
// 从业务变成鉴权，掩盖真正要测的东西。handler 本身才是被测对象。
func applyWithdraw(t *testing.T, ap *app, u *User, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/wallet/withdraw", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	ap.handleWithdrawApply(rec, withUser(req, u))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// seedWithdrawUser 造一个有钱可提的用户，并提一笔申请（走 store，绕过 HTTP 校验）。
func seedWithdrawUser(t *testing.T, store *Store, name string, creditPoints, applyPoints float64) (*User, *Withdrawal) {
	t.Helper()
	u, err := store.CreateUser(name, "pw")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := store.CreditWithdrawable(u.ID, PointToMicro(creditPoints)); err != nil {
		t.Fatalf("CreditWithdrawable: %v", err)
	}
	w := &Withdrawal{
		UserID: u.ID, AmountMicro: PointToMicro(applyPoints),
		Method: "alipay", AlipayAccount: "alice@example.com", RealName: "爱丽丝",
	}
	if err := store.CreateWithdrawal(w); err != nil {
		t.Fatalf("CreateWithdrawal: %v", err)
	}
	return u, w
}

// TestAdminWithdrawalsFieldNames 锁定管理侧提现列表的**接口契约**（本次缺陷的护栏）。
//
// 这条断言的全部价值在于"防字段名再漂移一次"：上一次是内嵌 `*Withdrawal`
// （无 json tag）导致下发 `ID`/`Status`/`CreatedAt`，前端读 `id`/`status`/`created_at`
// 全部 undefined，**审核按钮从此不再出现**，而接口始终 200。
// 所以这里必须同时断言两件事：snake_case 键**存在**，Go 字段名键**不存在**
// （只断言前者的话，"两套字段都下发"这种半修法照样通过）。
func TestAdminWithdrawalsFieldNames(t *testing.T) {
	store, _, mux := wdApp(t)
	defer store.Close()
	_, w := seedWithdrawUser(t, store, "wd-fields", 300, 100)

	code, body := wdReq(t, mux, "GET", "/api/admin/withdrawals", "")
	if code != http.StatusOK {
		t.Fatalf("列表应 200，got %d body=%v", code, body)
	}
	list, _ := body["withdrawals"].([]any)
	if len(list) != 1 {
		t.Fatalf("应有 1 条记录，实际 %d", len(list))
	}
	row, _ := list[0].(map[string]any)

	// 前端**读**的键，一个都不能少 —— 少任何一个都会表现为界面上某个字段空白，
	// 而 `status` 缺失的后果最严重（卡片状态 undefined → 审核按钮不渲染）。
	for _, k := range []string{
		"id", "user_id", "username", "amount", "fee", "cny",
		"method", "method_label", "alipay_account", "real_name", "account_info",
		"status", "status_label", "note", "reviewed_at", "reviewed_by",
		"reviewer_name", "created_at", "auto",
	} {
		if _, ok := row[k]; !ok {
			t.Errorf("响应缺少 snake_case 字段 %q（前端读的就是它）", k)
		}
	}
	// Go 字段名**不得**出现（两套并存 = 契约仍然含糊，下一个读代码的人会挑错那套）。
	for _, k := range []string{"ID", "Status", "CreatedAt", "AmountMicro", "AlipayAccount", "RealName"} {
		if _, ok := row[k]; ok {
			t.Errorf("响应不应出现 Go 字段名 %q（应只保留 snake_case 契约）", k)
		}
	}
	// 关键值必须能被前端用起来。
	if row["status"] != WithdrawPending {
		t.Errorf("status 应为 %q，得 %v", WithdrawPending, row["status"])
	}
	if row["status_label"] != "待审核" {
		t.Errorf("status_label 应为「待审核」，得 %v", row["status_label"])
	}
	if got, _ := row["id"].(float64); int64(got) != w.ID {
		t.Errorf("id 应为 %d，得 %v", w.ID, row["id"])
	}
	if got, _ := row["amount"].(float64); got != 100 {
		t.Errorf("amount 应为 100 点，得 %v", row["amount"])
	}
	if got, _ := row["created_at"].(float64); int64(got) != w.CreatedAt {
		t.Errorf("created_at 应为 %d，得 %v", w.CreatedAt, row["created_at"])
	}
	// 管理端收款信息必须**明文**（打码的话管理员没法照着打款）。
	if row["alipay_account"] != "alice@example.com" {
		t.Errorf("管理员侧支付宝账号应为明文，得 %v", row["alipay_account"])
	}
	if row["real_name"] != "爱丽丝" {
		t.Errorf("管理员侧姓名应为明文，得 %v", row["real_name"])
	}
	// 审核方式与一致性自检必须随列表下发（否则前端无从显示/告警）。
	if _, ok := body["params"].(map[string]any)["review_mode"]; !ok {
		t.Errorf("params 缺少 review_mode：%v", body["params"])
	}
	if _, ok := body["consistency"]; !ok {
		t.Errorf("响应缺少 consistency（三方一致性自检）")
	}
	if _, ok := body["status_counts"]; !ok {
		t.Errorf("响应缺少 status_counts")
	}
}

// TestReviewApproveKeepsThreeWayConsistent 通过审核 ⇒ 提现成功 + 冻结额被扣。
//
// 这是用户第二问的核心：审核状态（approved）、提现成功（钱真的出账）、
// 冻结额扣减三者必须是**同一次提交**的结果，且不变量始终成立。
func TestReviewApproveKeepsThreeWayConsistent(t *testing.T) {
	store, _, mux := wdApp(t)
	defer store.Close()
	u, w := seedWithdrawUser(t, store, "wd-approve", 300, 100)

	// 审核前：300 点余额里 100 点被冻结。
	before, _ := store.GetUserByID(u.ID)
	if before.FrozenMicro != PointToMicro(100) || before.WithdrawableMicro != PointToMicro(200) {
		t.Fatalf("审核前状态不对：frozen=%d withdrawable=%d", before.FrozenMicro, before.WithdrawableMicro)
	}

	code, body := wdReq(t, mux, "POST",
		"/api/admin/withdrawals/"+itoa64(w.ID)+"/review",
		`{"approve":true,"note":"支付宝 2026xxxx"}`)
	if code != http.StatusOK {
		t.Fatalf("通过审核应 200，got %d body=%v", code, body)
	}
	after, _ := store.GetUserByID(u.ID)
	// ① 状态 = approved（提现成功）
	rows, _ := store.ListWithdrawals(WithdrawApproved, 10)
	if len(rows) != 1 || rows[0].ID != w.ID {
		t.Fatalf("该笔应已置为 approved，实际 %+v", rows)
	}
	if rows[0].ReviewedAt <= 0 {
		t.Errorf("approved 记录必须有审核时间（否则它与从未审核无法区分）")
	}
	if rows[0].Auto {
		t.Errorf("人工审核的记录不该被标为 auto")
	}
	// ② 冻结额被扣（这是"提现成功"的账目证据）
	if after.FrozenMicro != 0 {
		t.Errorf("通过后冻结额应清零，得 %d", after.FrozenMicro)
	}
	// ③ 余额真的少了
	if after.BalanceMicro != PointToMicro(200) {
		t.Errorf("通过后余额应为 200 点（300-100），得 %d", after.BalanceMicro)
	}
	// ④ 不变量：可提现 + 冻结 ≤ 余额
	if after.WithdrawableMicro+after.FrozenMicro > after.BalanceMicro {
		t.Errorf("违反不变量：withdrawable(%d)+frozen(%d) > balance(%d)",
			after.WithdrawableMicro, after.FrozenMicro, after.BalanceMicro)
	}
	// ⑤ 全局一致性自检必须通过
	c, err := store.WithdrawConsistency()
	if err != nil {
		t.Fatalf("WithdrawConsistency: %v", err)
	}
	if c["ok"] != 1 {
		t.Errorf("审核后一致性自检应通过，得 %+v", c)
	}
	// 重复审核必须失败（否则同一笔钱会被扣两次）。
	if code, _ := wdReq(t, mux, "POST",
		"/api/admin/withdrawals/"+itoa64(w.ID)+"/review", `{"approve":true}`); code == http.StatusOK {
		t.Errorf("重复审核应被拒，却返回了 200")
	}
}

// TestReviewRejectReturnsFrozenToWithdrawable 驳回 ⇒ 冻结额原路退回可提现额度，余额不动。
func TestReviewRejectReturnsFrozenToWithdrawable(t *testing.T) {
	store, _, mux := wdApp(t)
	defer store.Close()
	u, w := seedWithdrawUser(t, store, "wd-reject", 300, 100)

	code, _ := wdReq(t, mux, "POST",
		"/api/admin/withdrawals/"+itoa64(w.ID)+"/review",
		`{"approve":false,"note":"收款信息不符"}`)
	if code != http.StatusOK {
		t.Fatalf("驳回应 200，got %d", code)
	}
	after, _ := store.GetUserByID(u.ID)
	if after.FrozenMicro != 0 {
		t.Errorf("驳回后冻结额应为 0，得 %d", after.FrozenMicro)
	}
	if after.BalanceMicro != PointToMicro(300) {
		t.Errorf("驳回后余额不应变化（300），得 %d", after.BalanceMicro)
	}
	if after.WithdrawableMicro != PointToMicro(300) {
		t.Errorf("驳回后额度应原路退回（300），得 %d", after.WithdrawableMicro)
	}
	if c, _ := store.WithdrawConsistency(); c["ok"] != 1 {
		t.Errorf("驳回后一致性自检应通过，得 %+v", c)
	}
}

// ---- 审核方式（v0.29.0） ----

// TestWithdrawReviewModeSettings 审核方式的读写与非法值拒绝。
func TestWithdrawReviewModeSettings(t *testing.T) {
	store, _, mux := wdApp(t)
	defer store.Close()

	// 默认必须是人工审核（回落方向见 withdrawReviewMode 的注释：错判代价不对称）。
	code, body := wdReq(t, mux, "GET", "/api/admin/settings", "")
	if code != http.StatusOK {
		t.Fatalf("读设置应 200，got %d", code)
	}
	if body["withdraw_review_mode"] != WithdrawReviewManual {
		t.Errorf("默认审核方式应为 manual，得 %v", body["withdraw_review_mode"])
	}

	// 改成自动 + 免审额度 500 点。
	code, body = wdReq(t, mux, "POST", "/api/admin/settings",
		`{"withdraw_review_mode":"auto","withdraw_auto_max_points":500}`)
	if code != http.StatusOK {
		t.Fatalf("保存审核方式应 200，got %d body=%v", code, body)
	}
	if got := mustKV(t, store, kvWithdrawReviewMode); got != WithdrawReviewAuto {
		t.Errorf("kv 应存入 auto，得 %q", got)
	}
	if got := mustKV(t, store, kvWithdrawAutoMaxPoints); got != "500" {
		t.Errorf("kv 应存入 500，得 %q", got)
	}

	// 非法取值必须**显式拒绝**（写入路径不做静默回落：那等于悄悄切换审核方式）。
	// 注意 "Auto " 是**合法**的（大小写 + 首尾空格会被规范化，见下一条断言），
	// 所以这里用一个真正无法归一的词 —— 否则测的只是规范化，不是"拒绝"。
	code, body = wdReq(t, mux, "POST", "/api/admin/settings", `{"withdraw_review_mode":"automatic"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("非法审核方式应 400，got %d body=%v", code, body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "manual") {
		t.Errorf("错误文案应说明合法取值，得 %q", msg)
	}
	// 大小写 + 首尾空格容错：' AUTO ' 应被规范化接受（运维手输的常见形态）。
	if code, body := wdReq(t, mux, "POST", "/api/admin/settings",
		`{"withdraw_review_mode":" AUTO "}`); code != http.StatusOK {
		t.Errorf("带空格的大写取值应被接受，got %d body=%v", code, body)
	}
	if got := mustKV(t, store, kvWithdrawReviewMode); got != WithdrawReviewAuto {
		t.Errorf("应存成规范化后的 auto，得 %q", got)
	}
	// 负数免审额度拒绝。
	if code, _ := wdReq(t, mux, "POST", "/api/admin/settings",
		`{"withdraw_auto_max_points":-1}`); code != http.StatusBadRequest {
		t.Errorf("负的免审额度应 400，got %d", code)
	}
}

// TestAutoReviewApprovesWithinLimit auto + 额度内 ⇒ 提交即通过（打款），冻结额立刻扣掉。
func TestAutoReviewApprovesWithinLimit(t *testing.T) {
	store, ap, _ := wdApp(t)
	defer store.Close()
	if err := store.SetKV(kvWithdrawReviewMode, WithdrawReviewAuto); err != nil {
		t.Fatal(err)
	}
	if err := store.SetKV(kvWithdrawAutoMaxPoints, "500"); err != nil {
		t.Fatal(err)
	}
	u, _ := store.CreateUser("wd-auto", "pw")
	if err := store.CreditWithdrawable(u.ID, PointToMicro(300)); err != nil {
		t.Fatal(err)
	}

	code, body := applyWithdraw(t, ap, u,
		`{"amount":100,"alipay_account":"bob@example.com","real_name":"鲍勃"}`)
	if code != http.StatusOK {
		t.Fatalf("自动审核下提交应 200，got %d body=%v", code, body)
	}
	if body["auto"] != true {
		t.Errorf("响应应标记 auto=true，得 %v", body["auto"])
	}
	// 该笔必须已经是 approved，且 auto=1、reviewer=0（没有人审它）。
	rows, _ := store.ListWithdrawals(WithdrawApproved, 10)
	if len(rows) != 1 {
		t.Fatalf("应已有 1 笔 approved，实际 %d", len(rows))
	}
	if !rows[0].Auto {
		t.Errorf("自动放行的记录 auto 应为 true")
	}
	if rows[0].ReviewedBy != 0 {
		t.Errorf("自动放行不该有审核人，得 %d", rows[0].ReviewedBy)
	}
	if rows[0].ReviewedAt <= 0 {
		t.Errorf("自动放行也必须写审核时间（否则它看起来像「从未审核」）")
	}
	after, _ := store.GetUserByID(u.ID)
	if after.FrozenMicro != 0 {
		t.Errorf("自动通过后冻结额应清零，得 %d", after.FrozenMicro)
	}
	if after.BalanceMicro != PointToMicro(200) {
		t.Errorf("自动通过后余额应为 200 点，得 %d", after.BalanceMicro)
	}
	if c, _ := store.WithdrawConsistency(); c["ok"] != 1 {
		t.Errorf("自动通过后一致性应通过，得 %+v", c)
	}
}

// TestAutoReviewAboveLimitStaysPending 超过免审额度 ⇒ 仍进人工队列（"小额免审、大额人工"）。
func TestAutoReviewAboveLimitStaysPending(t *testing.T) {
	store, ap, _ := wdApp(t)
	defer store.Close()
	if err := store.SetKV(kvWithdrawReviewMode, WithdrawReviewAuto); err != nil {
		t.Fatal(err)
	}
	if err := store.SetKV(kvWithdrawAutoMaxPoints, "50"); err != nil {
		t.Fatal(err)
	}
	u, _ := store.CreateUser("wd-auto-big", "pw")
	if err := store.CreditWithdrawable(u.ID, PointToMicro(300)); err != nil {
		t.Fatal(err)
	}
	code, body := applyWithdraw(t, ap, u,
		`{"amount":100,"alipay_account":"c@example.com","real_name":"丙"}`)
	if code != http.StatusOK {
		t.Fatalf("提交应 200，got %d body=%v", code, body)
	}
	if body["auto"] != false {
		t.Errorf("超过免审额度不该自动放行，body=%v", body)
	}
	pend, _ := store.ListWithdrawals(WithdrawPending, 10)
	if len(pend) != 1 {
		t.Fatalf("该笔应留在待审队列，实际 %d 条", len(pend))
	}
	if got, _ := store.GetUserByID(u.ID); got.FrozenMicro != PointToMicro(100) {
		t.Errorf("待审期间冻结额应为 100 点，得 %d", got.FrozenMicro)
	}
}

// TestWithdrawReviewModeInvalidFallsBackToManual 读取路径：kv 里是垃圾值也必须回落人工。
//
// 与写入路径"非法即拒绝"是两条**刻意不同**的纪律（见 withdrawReviewMode 注释）。
func TestWithdrawReviewModeInvalidFallsBackToManual(t *testing.T) {
	store, ap, _ := wdApp(t)
	defer store.Close()
	for _, bad := range []string{"", "AUTO2", "yes", "1"} {
		if err := store.SetKV(kvWithdrawReviewMode, bad); err != nil {
			t.Fatal(err)
		}
		if got := ap.withdrawReviewMode(); got != WithdrawReviewManual {
			t.Errorf("kv=%q 时应回落 manual，得 %q", bad, got)
		}
	}
	// 免审额度：垃圾值 → 0（不限）；合法正数 → 对应微点。
	if err := store.SetKV(kvWithdrawAutoMaxPoints, "abc"); err != nil {
		t.Fatal(err)
	}
	if got := ap.withdrawAutoMaxMicro(); got != 0 {
		t.Errorf("垃圾免审额度应得 0（不限），得 %d", got)
	}
	if err := store.SetKV(kvWithdrawAutoMaxPoints, "12.5"); err != nil {
		t.Fatal(err)
	}
	if got := ap.withdrawAutoMaxMicro(); got != PointToMicro(12.5) {
		t.Errorf("免审额度应换算为微点 %d，得 %d", PointToMicro(12.5), got)
	}
}

// TestWithdrawConsistencyDetectsMismatch 自检必须能**抓到**不一致（否则它只是个装饰）。
func TestWithdrawConsistencyDetectsMismatch(t *testing.T) {
	store, _, _ := wdApp(t)
	defer store.Close()
	u, _ := seedWithdrawUser(t, store, "wd-consist", 300, 100)

	if c, _ := store.WithdrawConsistency(); c["ok"] != 1 {
		t.Fatalf("初始状态应一致，得 %+v", c)
	}
	// 人为破坏：把冻结额直接改掉（模拟外部改库 / 历史未回填）。
	if _, err := store.db.Exec(`UPDATE users SET frozen_micro = 0 WHERE id = ?`, u.ID); err != nil {
		t.Fatal(err)
	}
	c, err := store.WithdrawConsistency()
	if err != nil {
		t.Fatalf("WithdrawConsistency: %v", err)
	}
	if c["ok"] != 0 {
		t.Errorf("冻结额被改掉后自检必须报不一致，得 %+v", c)
	}
	if c["frozen_pending_diff"] == 0 {
		t.Errorf("应给出差额，得 %+v", c)
	}
	// 顺带：该笔此时**不能**被通过（冻结额不足），且错误必须是明确的冲突而不是"打款成功"。
	_, err = store.ReviewWithdrawal(1, true, "", 1)
	if err == nil {
		t.Errorf("冻结额不足时通过必须失败（否则会把余额扣成负数还显示成功）")
	}
}
