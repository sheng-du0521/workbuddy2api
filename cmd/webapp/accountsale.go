// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

// accountsale.go 账号上架 / 买断（v0.19.0）。
//
// ============================================================================
// 业务定位
// ============================================================================
//
//	上架（管理员）：把「可售」账号挂牌到用户市场的在售列表，挂牌期间归属**闲置**
//	   分组（不参与平台反代消耗），避免「挂出来卖又同时被平台拿去跑」。
//	买断（用户）：用户对在售账号出价买断；成交后账号所有权转移给买家、进入买家的
//	   「代挂序列」，买家用自用 Key（kind=host）即可反代，不自动参与平台公共池。
//
//	归属流转（三段）：
//
//	  阶段        OwnerUserID   ConsumeGroup   PoolState   在售
//	  上架前       不变          不变           不变         —
//	  上架后       不变          **闲置**        不变         ✅
//	  买断后       **买家**      **闲置**        **清空**     ❌
//
//	  · 上架只改分组（→ 闲置），不动归属 —— 卖不掉可以下架原样收回。
//	  · 买断同时改归属与清 PoolState：账号退出平台号池，只服务买家的自用 Key。
//	    ConsumeGroup 保持闲置（不参与平台消耗）——若买家想让平台也用它，需另行
//	    走「申请进池 → 管理员审批」的既有代挂流程。
//
// ============================================================================
// 数据字段
// ============================================================================
//
//	account_sales（在售状态，uid 主键，与 accounts 一对一）
//	  uid / on_sale / listed_at / listed_by / sale_note / prev_group
//	  sold_at / sold_to
//
//	account_purchases（买断台账，逐笔）
//	  id / account_uid / buyer_user_id / seller_user_id(0=平台自持)
//	  amount_micro / seller_micro / platform_micro / created_at
//
//	刻意**不改 accounts 表结构**：上架/买断状态独立成表，老库零迁移风险，
//	也不影响 accountCols 的全部既有查询。
//
// ============================================================================
// 资金流向（买断）
// ============================================================================
//
//	买家余额 −amount ；卖家（原号主）余额 + amount×分成比例 ；余数归平台。
//	平台自持号（seller=0）→ 全额归平台（不产生用户入账，仅台账记录 platform_micro）。
//	分成比例沿用既有的号池分成（proxy.hostRatioFor），不新设第二套比例。
//
// ============================================================================
// 并发与失败边界
// ============================================================================
//
//	并发抢单：买断第一步即以 `UPDATE ... WHERE uid=? AND on_sale=1` 原子占位，
//	  仅一行能成功；失败者得「已被买断 / 已下架」。库层 MaxOpenConns(1) 使事务
//	  天然串行，双重保险。
//	金额校验：> 0 且 ≤ maxSalePricePoints（防溢出 / 防手滑多打零）。
//	余额不足：条件更新（balance_micro >= amount）判 0 行 → 明确提示，不产生欠费。
//	账号状态异常：买断前重校验（禁用 / 异常分组 / 已冷却）→ 自动下架并拒绝。
//	整笔事务：占位 → 扣款 → 分账 → 改归属 → 记台账，任一步失败整体回滚。

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// maxSalePricePoints 单笔买断金额上限（点）。防手滑多打零与 int64 溢出。
const maxSalePricePoints = 1e9

// hostGroupBought 买断账号在买家「代挂序列」中的默认分组名。
const hostGroupBought = "买断"

// salePriceMinMicro 挂牌价下限（> 0 即可，这里用 0.01 元对应的最小步进）。
// salePriceDecimals 允许的小数位（元）：2 位（分）。
const salePriceDecimals = 2

// microStepForDecimals 返回 n 位小数在「微点」下的最小步进。
// 1 点 = MicroPerPoint(1000) 微点，故 2 位小数 → 10 微点。
func microStepForDecimals(n int) int64 {
	step := int64(MicroPerPoint)
	for i := 0; i < n; i++ {
		step /= 10
	}
	if step < 1 {
		step = 1
	}
	return step
}

// validateSalePrice 校验挂牌价（微点）。返回空串表示通过，否则为可读的中文原因。
//
// 校验项（需求 2「以货币（元）为单位，并进行输入校验」）：
//   - 必须 > 0
//   - 不超过 maxSalePricePoints 点（防手滑多打零 / 防溢出）
//   - 最多 2 位小数（对应「元」的两位小数；输入以元为单位，前端换算成微点）
func validateSalePrice(priceMicro int64) string {
	if priceMicro <= 0 {
		return "请填写大于 0 的挂牌价"
	}
	if priceMicro > int64(maxSalePricePoints)*MicroPerPoint {
		return fmt.Sprintf("挂牌价超出上限（%.0f 点）", maxSalePricePoints)
	}
	if step := microStepForDecimals(salePriceDecimals); priceMicro%step != 0 {
		return "挂牌价最多保留两位小数"
	}
	return ""
}

// errAlreadySold 并发抢单 / 已被下架：条件更新占位失败。
var errAlreadySold = errors.New("account is no longer on sale")

// ---------------------------------------------------------------------------
// 表结构
// ---------------------------------------------------------------------------

// AccountSale 账号在售状态。
type AccountSale struct {
	UID      string
	OnSale   bool
	ListedAt int64
	ListedBy int64  // 操作人 user id（管理员或卖家本人）
	SaleNote string // 挂牌备注（可选）
	// SalePriceMicro 挂牌价（微点，v0.21.0）。上架时由卖家/管理员设定，
	// **买断必须按此价成交**（买家不再自行填价，见 BuyAccount）。
	SalePriceMicro int64
	PrevGroup      string // 上架前的消耗分组，下架时恢复
	SoldAt         int64
	SoldTo         int64
}

// AccountPurchase 买断台账行。
type AccountPurchase struct {
	ID            int64
	AccountUID    string
	BuyerUserID   int64
	SellerUserID  int64 // 0 = 平台自持
	AmountMicro   int64
	SellerMicro   int64
	PlatformMicro int64
	CreatedAt     int64
}

// migrateAccountSales 建在售状态表与买断台账表（幂等，独立建表便于单测）。
func (s *Store) migrateAccountSales() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS account_sales (
			uid        TEXT PRIMARY KEY,
			on_sale    INTEGER NOT NULL DEFAULT 0,
			listed_at  INTEGER NOT NULL DEFAULT 0,
			listed_by  INTEGER NOT NULL DEFAULT 0,
			sale_note  TEXT NOT NULL DEFAULT '',
			sale_price_micro INTEGER NOT NULL DEFAULT 0,
			prev_group TEXT NOT NULL DEFAULT '',
			sold_at    INTEGER NOT NULL DEFAULT 0,
			sold_to    INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_account_sales_on ON account_sales(on_sale)`,
		`CREATE TABLE IF NOT EXISTS account_purchases (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			account_uid    TEXT NOT NULL DEFAULT '',
			buyer_user_id  INTEGER NOT NULL,
			seller_user_id INTEGER NOT NULL DEFAULT 0,
			amount_micro   INTEGER NOT NULL,
			seller_micro   INTEGER NOT NULL DEFAULT 0,
			platform_micro INTEGER NOT NULL DEFAULT 0,
			created_at     INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_purchases_buyer ON account_purchases(buyer_user_id, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_purchases_uid ON account_purchases(account_uid)`,
	}
	for _, st := range stmts {
		if _, err := s.db.Exec(st); err != nil {
			return fmt.Errorf("migrate account_sales: %w", err)
		}
	}
	// 老库增量列（v0.21.0）：挂牌价。
	if err := s.ensureColumn("account_sales", "sale_price_micro",
		"sale_price_micro INTEGER NOT NULL DEFAULT 0"); err != nil {
		return fmt.Errorf("migrate account_sales column sale_price_micro: %w", err)
	}
	return nil
}

// GetAccountSale 取账号的在售状态；无记录返回 nil（= 从未上架）。
func (s *Store) GetAccountSale(uid string) (*AccountSale, error) {
	row := s.db.QueryRow(`SELECT uid, on_sale, listed_at, listed_by, sale_note,
		sale_price_micro, prev_group, sold_at, sold_to FROM account_sales WHERE uid = ?`, uid)
	var x AccountSale
	var on int
	err := row.Scan(&x.UID, &on, &x.ListedAt, &x.ListedBy, &x.SaleNote,
		&x.SalePriceMicro, &x.PrevGroup, &x.SoldAt, &x.SoldTo)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return nil, nil
		}
		return nil, err
	}
	x.OnSale = on != 0
	return &x, nil
}

// ListAccountSales 全部在售记录（含已下架的历史行），按上架时间倒序。
func (s *Store) ListAccountSales() (map[string]*AccountSale, error) {
	rows, err := s.db.Query(`SELECT uid, on_sale, listed_at, listed_by, sale_note,
		sale_price_micro, prev_group, sold_at, sold_to FROM account_sales`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*AccountSale{}
	for rows.Next() {
		var x AccountSale
		var on int
		if err := rows.Scan(&x.UID, &on, &x.ListedAt, &x.ListedBy, &x.SaleNote,
			&x.SalePriceMicro, &x.PrevGroup, &x.SoldAt, &x.SoldTo); err != nil {
			return nil, err
		}
		x.OnSale = on != 0
		out[x.UID] = &x
	}
	return out, rows.Err()
}

// SetAccountOnSale 上架 / 下架。
//   - on=true：置在售、记录上架前的分组（供下架恢复）、备注。
//   - on=false：清在售、恢复 prev_group（若为空则保持不变）。
func (s *Store) SetAccountOnSale(uid string, on bool, operatorID int64, note, prevGroup string, priceMicro int64) error {
	now := time.Now().Unix()
	flag := 0
	if on {
		flag = 1
	}
	if !on {
		priceMicro = 0 // 下架清价，避免残留价格被误读
	}
	_, err := s.db.Exec(`INSERT INTO account_sales (uid, on_sale, listed_at, listed_by, sale_note, sale_price_micro, prev_group, sold_at, sold_to)
		VALUES (?,?,?,?,?,?,?,0,0)
		ON CONFLICT(uid) DO UPDATE SET
			on_sale=excluded.on_sale,
			listed_at=excluded.listed_at,
			listed_by=excluded.listed_by,
			sale_note=excluded.sale_note,
			sale_price_micro=excluded.sale_price_micro,
			prev_group=excluded.prev_group,
			sold_at=0,
			sold_to=0`,
		uid, flag, now, operatorID, note, priceMicro, prevGroup)
	return err
}

// BuyAccount 买断事务：原子占位 → 扣买家余额 → 全额入卖家 → 转移归属 → 记台账。
//
// 分账口径（v0.20.0 定稿，取代 v0.19.0 的按比例分账）：
//   - 平台自持号（seller_user_id == 0）→ **全额归平台**（platform_micro = amount）
//   - 用户账号（seller > 0）           → **全额归卖家**（seller_micro = amount）
//
// 为什么改成全额归卖家：成交的是**账号这个资产本身**（含其全部剩余积分），
// 与「代挂账号服务平台流量拿分成」是两件事 —— 后者平台提供了算力/流量，
// 前者平台只提供撮合与资金流转，再抽成于理不合（需求方确认口径）。
//
// 返回成交后的台账行；失败时不产生任何副作用（整体回滚）。
func (s *Store) BuyAccount(uid string, buyerID int64) (*AccountPurchase, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().Unix()

	// 1) 原子占位：只有在售时才能抢到（并发抢单在此决出唯一赢家）。
	//    同时取出**挂牌价** —— 成交价以挂单为准，不由买家自定（v0.21.0）。
	var amountMicro int64
	if err := tx.QueryRow(`SELECT sale_price_micro FROM account_sales WHERE uid = ? AND on_sale = 1`, uid).
		Scan(&amountMicro); err != nil {
		if err == sql.ErrNoRows {
			return nil, errAlreadySold
		}
		return nil, err
	}
	if amountMicro <= 0 {
		return nil, errors.New("account has no sale price")
	}
	res, err := tx.Exec(`UPDATE account_sales SET on_sale=0, sold_at=?, sold_to=? WHERE uid=? AND on_sale=1`,
		now, buyerID, uid)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, errAlreadySold
	}

	// 2) 读账号当前归属，校验「不能买自己的号」。
	var sellerID int64
	if err := tx.QueryRow(`SELECT owner_user_id FROM accounts WHERE uid = ?`, uid).Scan(&sellerID); err != nil {
		return nil, fmt.Errorf("account not found")
	}
	if sellerID == buyerID {
		return nil, errors.New("cannot buy your own account")
	}

	// 3) 扣买家余额（条件更新：余额不足则 0 行 → 不产生欠费）。
	res, err = tx.Exec(`UPDATE users SET balance_micro = balance_micro - ?, updated_at = ?
		WHERE id = ? AND balance_micro >= ?`, amountMicro, now, buyerID, amountMicro)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrInsufficientBalance
	}

	// 4) 分账：平台自持 → 全额归平台；用户账号 → 全额归卖家。
	// 卖家所得计入**可提现额度**（v0.21.0：账号买卖金额是允许提现的两个来源之一）。
	var sellerMicro, platformMicro int64
	if sellerID > 0 {
		sellerMicro = amountMicro
		if _, err := tx.Exec(`UPDATE users SET balance_micro = balance_micro + ?,
			withdrawable_micro = withdrawable_micro + ?, updated_at = ? WHERE id = ?`,
			sellerMicro, sellerMicro, now, sellerID); err != nil {
			return nil, err
		}
	} else {
		platformMicro = amountMicro
	}

	// 5) 归属转移：买家持有、进代挂序列、退出平台号池、保持闲置分组。
	if _, err := tx.Exec(`UPDATE accounts SET owner_user_id=?, host_group=?, pool_state='',
		consume_group=?, consume_group_prev='', consume_group_note='', updated_at=? WHERE uid=?`,
		buyerID, hostGroupBought, cgIdle, now, uid); err != nil {
		return nil, err
	}

	// 6) 台账。
	p := &AccountPurchase{
		AccountUID: uid, BuyerUserID: buyerID, SellerUserID: sellerID,
		AmountMicro: amountMicro, SellerMicro: sellerMicro, PlatformMicro: platformMicro,
		CreatedAt: now,
	}
	res, err = tx.Exec(`INSERT INTO account_purchases
		(account_uid, buyer_user_id, seller_user_id, amount_micro, seller_micro, platform_micro, created_at)
		VALUES (?,?,?,?,?,?,?)`,
		uid, buyerID, sellerID, amountMicro, sellerMicro, platformMicro, now)
	if err != nil {
		return nil, err
	}
	p.ID, _ = res.LastInsertId()

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return p, nil
}

// ListAccountPurchases 全部买断台账（倒序），管理端用。
func (s *Store) ListAccountPurchases(limit int) ([]*AccountPurchase, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.db.Query(`SELECT id, account_uid, buyer_user_id, seller_user_id,
		amount_micro, seller_micro, platform_micro, created_at
		FROM account_purchases ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPurchases(rows)
}

// ListPurchasesByBuyer 某买家的买断记录（用户侧）。
func (s *Store) ListPurchasesByBuyer(buyerID int64, limit int) ([]*AccountPurchase, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT id, account_uid, buyer_user_id, seller_user_id,
		amount_micro, seller_micro, platform_micro, created_at
		FROM account_purchases WHERE buyer_user_id = ? ORDER BY created_at DESC, id DESC LIMIT ?`,
		buyerID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPurchases(rows)
}

func scanPurchases(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}) ([]*AccountPurchase, error) {
	var out []*AccountPurchase
	for rows.Next() {
		var p AccountPurchase
		if err := rows.Scan(&p.ID, &p.AccountUID, &p.BuyerUserID, &p.SellerUserID,
			&p.AmountMicro, &p.SellerMicro, &p.PlatformMicro, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &p)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// 上架前置校验与视图
// ---------------------------------------------------------------------------

// listableError 判定账号是否可上架；可上架返回 ""（空串），否则返回拒绝原因。
//
// 可上架范围（v0.19.0 定稿，v0.20.0 加积分门槛）：
//   - 平台自持账号（owner=0）—— 管理员上架；或
//   - 已入池的代挂账号（owner>0 且 pool_state="in"）—— 管理员可上架；
//   - 用户自己上架时额外限定「必须是本人的账号」（由调用方先行校验归属）。
//
// 排除：积分未知 / 积分不足 tradeMinCredits(4000) / 已禁用 / 冷却中 / 异常分组 / 已在售。
//
// creditsKnown 为 false 表示积分从未被权威拉取过（credits==0 无法区分「真 0」与「未知」）；
// 交易涉及真金白银，未知即拒绝（让用户先刷一次余额再来）。
func listableError(a *Account, sale *AccountSale, credits int64, creditsKnown bool) string {
	if a == nil {
		return "账号不存在"
	}
	if sale != nil && sale.OnSale {
		return "该账号已在售"
	}
	if a.Disabled {
		return "账号已禁用，不可上架"
	}
	if a.CooldownRemaining() > 0 {
		return "账号正在冷却中，暂不可上架"
	}
	if effectiveConsumeGroup(a) == cgAbnormal {
		return "账号处于异常分组，不可上架（请先移出异常）"
	}
	if a.OwnerUserID != 0 && a.PoolState != "in" {
		return "代挂账号需先入池（管理员审批通过）后才可上架"
	}
	// 交易积分门槛（v0.20.0）：账号积分必须达到 tradeMinCredits。
	if !creditsKnown {
		return "尚未获取到该账号的积分，暂不可上架（请先在账号卡片点「刷新积分」）"
	}
	if credits < tradeMinCredits {
		return fmt.Sprintf("账号积分不足 %d（当前 %d），不允许参与交易", tradeMinCredits, credits)
	}
	return ""
}

// saleView 在售账号视图（用户市场列表；不含任何凭证）。
func (a *app) saleView(acc *Account, sale *AccountSale) map[string]any {
	st, _ := a.proxy.pool.Status(acc.UID)
	ownerName := ""
	if acc.OwnerUserID != 0 {
		if u, err := a.store.GetUserByID(acc.OwnerUserID); err == nil && u != nil {
			ownerName = u.Username
		}
	}
	v := map[string]any{
		"uid":       acc.UID,
		"nickname":  acc.Nickname,
		"realm":     acc.Realm,
		"credits":   st.Credits,
		"disabled":  acc.Disabled,
		"cooling":   acc.CooldownRemaining() > 0,
		"owner_id":  acc.OwnerUserID,
		"listed_at": sale.ListedAt,
		"sale_note": sale.SaleNote,
		"group":     consumeGroupLabel(effectiveConsumeGroup(acc)),
		// 挂牌价（v0.21.0）：以「点」与「元」两种形态下发，前端卡片直接展示。
		"price":       MicroToPoint(sale.SalePriceMicro),
		"price_micro": sale.SalePriceMicro,
	}
	if ownerName != "" {
		v["owner_name"] = ownerName
	}
	if acc.ExpiresAt > 0 {
		v["expires_at"] = acc.ExpiresAt
	}
	return v
}

// ---------------------------------------------------------------------------
// 管理端 handler
// ---------------------------------------------------------------------------

// parseSalePrice app 层便捷包装：注入当前折算比例。
func (a *app) parseSalePrice(yuan float64) (int64, string) {
	return parseSalePriceYuan(yuan, a.moneyRate())
}

// handleAdminAccountSale 管理员上架 / 下架指定账号。
//
//	POST /api/admin/accounts/{uid}/sale  {"action":"list"|"unlist","note":"..."}
//
// 上架成功：该账号 ConsumeGroup → 闲置（记录上架前分组，供下架恢复）。
func (a *app) handleAdminAccountSale(w http.ResponseWriter, r *http.Request) {
	// 注意：adminGuard 的 X-Admin-Key（运维/脚本）通道**不注入 user**，
	// 此时 userFrom(r) 为 nil —— 只取 ID 用于审计，绝不能直接解引用。
	adminID := int64(0)
	if u := userFrom(r); u != nil {
		adminID = u.ID
	}
	uid := r.PathValue("uid")
	var req struct {
		Action string `json:"action"`
		Note   string `json:"note"`
		// Price 挂牌价（**元**，需求 2）。前端以元输入，服务端换算为微点存储。
		Price float64 `json:"price"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	note := strings.TrimSpace(req.Note)
	if len(note) > 200 {
		note = note[:200]
	}
	priceMicro, perr := a.parseSalePrice(req.Price)
	if perr != "" {
		// 下架不需要价格；仅在上架动作上报错。
		if strings.EqualFold(strings.TrimSpace(req.Action), "list") {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": perr})
			return
		}
	}
	acc, err := a.store.GetAccount(uid)
	if err != nil || acc == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "账号不存在"})
		return
	}
	sale, err := a.store.GetAccountSale(uid)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	credits, creditsKnown := a.poolCredits(uid)
	switch strings.ToLower(strings.TrimSpace(req.Action)) {
	case "list":
		if code, msg := a.listAccountForSale(acc, sale, credits, creditsKnown, adminID, note, priceMicro); msg != "" {
			writeJSON(w, code, map[string]any{"error": msg})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"uid": uid, "on_sale": true, "consume_group": cgIdle,
			"consume_group_text": consumeGroupLabel(cgIdle),
		})

	case "unlist":
		if code, msg := a.unlistAccount(acc, sale, adminID); msg != "" {
			writeJSON(w, code, map[string]any{"error": msg})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"uid": uid, "on_sale": false})

	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "未知动作（list/unlist）"})
	}
}

// listAccountForSale 执行上架（管理端与用户端共用）：校验 → 置在售 → 分组改闲置。
// 返回 (HTTP 状态码, 错误文案)；错误文案为空表示成功。
//
// 挂牌期间归属闲置是刻意的：避免「挂出来卖、又同时被平台拿去跑」——
// 一旦被平台消耗，账号积分会掉，买家看到的挂牌信息与成交时不一致。
func (a *app) listAccountForSale(acc *Account, sale *AccountSale, credits int64, creditsKnown bool, operatorID int64, note string, priceMicro int64) (int, string) {
	if msg := listableError(acc, sale, credits, creditsKnown); msg != "" {
		return http.StatusBadRequest, msg
	}
	if msg := validateSalePrice(priceMicro); msg != "" {
		return http.StatusBadRequest, msg
	}
	// prev 记录**原始显式值**（可为空），不是 effectiveConsumeGroup 的推断结果。
	//
	// 为什么必须用原始值（v0.20.1）：若记推断值，一个「未显式设组」（空串 → 推断为
	// 「反代」）的账号下架后会被写成**显式** proxy —— 行为等效，但丢掉了「未设置」
	// 这一信息（此后它不再随来源变化，例如账号转成代挂号时不会自动变成「闲置」）。
	// 记原始值则下架恢复空串 = 回到「按来源推断」，语义无损。
	prev := acc.ConsumeGroup
	if err := a.store.SetAccountOnSale(acc.UID, true, operatorID, note, prev, priceMicro); err != nil {
		return http.StatusInternalServerError, err.Error()
	}
	if err := a.store.SetAccountConsumeGroup(acc.UID, cgIdle); err != nil {
		return http.StatusInternalServerError, err.Error()
	}
	_ = a.proxy.SyncAccounts()
	log.Printf("INFO: [sale] account %s listed (operator=%d credits=%d price=%.2f点 prev_group=%q effective_before=%s)",
		acc.UID, operatorID, credits, MicroToPoint(priceMicro), prev, consumeGroupLabel(effectiveConsumeGroup(acc)))
	return http.StatusOK, ""
}

// unlistAccount 执行下架：清在售 + 恢复上架前分组，并记审计。
func (a *app) unlistAccount(acc *Account, sale *AccountSale, operatorID int64) (int, string) {
	if sale == nil || !sale.OnSale {
		return http.StatusBadRequest, "该账号当前不在售"
	}
	if err := a.store.SetAccountOnSale(acc.UID, false, operatorID, "", "", 0); err != nil {
		return http.StatusInternalServerError, err.Error()
	}
	// **无条件**恢复为 sale.PrevGroup：空串也是合法目标（= 回到「按来源推断」）。
	// 早期写法是 `if sale.PrevGroup != ""`，会把「上架前未显式设组」的账号
	// 永久留在上架时写死的「闲置」上（v0.20.1 修正）。
	if err := a.store.SetAccountConsumeGroup(acc.UID, sale.PrevGroup); err != nil {
		return http.StatusInternalServerError, err.Error()
	}
	_ = a.proxy.SyncAccounts()
	// 日志里的恢复后分组：prev 为空表示「回到按来源推断」，此时用当前账号推断一次展示。
	if after, err := a.store.GetAccount(acc.UID); err == nil && after != nil {
		log.Printf("INFO: [sale] account %s unlisted (operator=%d restore_group=%q → %s)",
			acc.UID, operatorID, sale.PrevGroup, consumeGroupLabel(effectiveConsumeGroup(after)))
	}
	return http.StatusOK, ""
}

// handleAdminSales 管理端账号市场台账：在售列表 + 买断记录。
//
//	GET /api/admin/sales
func (a *app) handleAdminSales(w http.ResponseWriter, r *http.Request) {
	accounts, err := a.store.ListAccounts()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	sales, err := a.store.ListAccountSales()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	onSale := make([]map[string]any, 0)
	// 可上架候选（供管理员一键挂牌）：符合范围且当前未在售。
	listable := make([]map[string]any, 0)
	byUID := map[string]*Account{}
	for _, acc := range accounts {
		byUID[acc.UID] = acc
	}
	for _, acc := range accounts {
		s := sales[acc.UID]
		if s != nil && s.OnSale {
			onSale = append(onSale, a.saleView(acc, s))
			continue
		}
		candCredits, candKnown := a.poolCredits(acc.UID)
		if msg := listableError(acc, s, candCredits, candKnown); msg == "" {
			ownerName := ""
			if acc.OwnerUserID != 0 {
				if ou, err := a.store.GetUserByID(acc.OwnerUserID); err == nil && ou != nil {
					ownerName = ou.Username
				}
			}
			row := map[string]any{
				"uid": acc.UID, "nickname": acc.Nickname, "realm": acc.Realm,
				"owner_id": acc.OwnerUserID, "pool_state": acc.PoolState,
				"group": consumeGroupLabel(effectiveConsumeGroup(acc)),
			}
			if ownerName != "" {
				row["owner_name"] = ownerName
			}
			listable = append(listable, row)
		}
	}
	sort.Slice(onSale, func(i, j int) bool {
		return toInt64(onSale[i]["listed_at"]) > toInt64(onSale[j]["listed_at"])
	})

	purchases, err := a.store.ListAccountPurchases(200)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	prows := make([]map[string]any, 0, len(purchases))
	for _, p := range purchases {
		row := map[string]any{
			"id": p.ID, "uid": p.AccountUID,
			"buyer_id": p.BuyerUserID, "seller_id": p.SellerUserID,
			"amount":       MicroToPoint(p.AmountMicro),
			"amount_cny":   round2(pointsToCNY(MicroToPoint(p.AmountMicro), a.moneyRate())),
			"seller_share": MicroToPoint(p.SellerMicro),
			"created_at":   p.CreatedAt,
		}
		if acc := byUID[p.AccountUID]; acc != nil {
			row["nickname"] = acc.Nickname
		}
		if bu, err := a.store.GetUserByID(p.BuyerUserID); err == nil && bu != nil {
			row["buyer_name"] = bu.Username
		}
		if p.SellerUserID != 0 {
			if su, err := a.store.GetUserByID(p.SellerUserID); err == nil && su != nil {
				row["seller_name"] = su.Username
			}
		} else {
			row["seller_name"] = "平台"
		}
		prows = append(prows, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"on_sale":   onSale,
		"listable":  listable,
		"purchases": prows,
		// unknown_credits：积分「未获取」的可用账号数（v0.20.0）。
		// 这些号会被上架校验拦下，管理端据此给出「N 个账号积分未获取，点此刷新」的明确入口
		// —— 否则管理员只会反复看到「未获取积分」却不知道要做什么。
		"unknown_credits": a.countUnknownCredits(),
		"min_trade":       tradeMinCredits,
	})
}

// ---------------------------------------------------------------------------
// 用户端 handler
// ---------------------------------------------------------------------------

// handleSaleList 用户市场：在售账号列表（脱敏，不含凭证）。
//
//	GET /api/sale/accounts
func (a *app) handleSaleList(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	accounts, err := a.store.ListAccounts()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	sales, err := a.store.ListAccountSales()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	out := make([]map[string]any, 0)
	owned := make([]map[string]any, 0) // 本人可上架的账号（v0.20.0）
	for _, acc := range accounts {
		s := sales[acc.UID]
		if acc.OwnerUserID == u.ID {
			// 自己的号不出现在自己的市场里（也不能买自己的）；但要作为
			// 「我拥有的、可上架的账号」列出，供控制台上架。
			credits, known := a.poolCredits(acc.UID)
			row := map[string]any{
				"uid":           acc.UID,
				"nickname":      acc.Nickname,
				"realm":         acc.Realm,
				"credits":       credits,
				"credits_known": known,
				"on_sale":       s != nil && s.OnSale,
				"price":         salePriceOf(s),
				"pool_state":    acc.PoolState,
				"group":         consumeGroupLabel(effectiveConsumeGroup(acc)),
			}
			if !known {
				row["blocked"] = "尚未获取到积分，请先刷新积分"
			} else if credits < tradeMinCredits {
				row["blocked"] = fmt.Sprintf("积分不足 %d，不可上架", tradeMinCredits)
			} else if s != nil && s.OnSale {
				// 已在售：不 blocked，前端展示「下架」。
			} else if acc.Disabled {
				row["blocked"] = "账号已禁用"
			}
			owned = append(owned, row)
			continue
		}
		if s == nil || !s.OnSale {
			continue
		}
		v := a.saleView(acc, s)
		v["mine"] = false
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		return toInt64(out[i]["listed_at"]) > toInt64(out[j]["listed_at"])
	})

	// 本人买断记录。
	mine, _ := a.store.ListPurchasesByBuyer(u.ID, 50)
	mrows := make([]map[string]any, 0, len(mine))
	for _, p := range mine {
		mrows = append(mrows, map[string]any{
			"uid": p.AccountUID, "amount": MicroToPoint(p.AmountMicro), "created_at": p.CreatedAt,
		})
	}
	fresh, _ := a.store.GetUserByID(u.ID)
	bal := float64(0)
	if fresh != nil {
		bal = MicroToPoint(fresh.BalanceMicro)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":  out,
		"owned":     owned, // 本人可上架的账号（v0.20.0）
		"purchases": mrows,
		"balance":   bal,
		"rate":      a.moneyRate(),
		// 交易门槛常量下发给前端，避免前端硬编码（改阈值只需改后端一处）。
		"min_trade_credits": tradeMinCredits,
	})
}

// buyRequest 买断请求体（v0.21.0 起**不再携带金额** —— 成交价以挂单为准）。
type buyRequest struct{}

// handleUserAccountSale 用户上架 / 下架**自己的**账号（v0.20.0）。
//
//	POST /api/sale/accounts/{uid}/list  {"action":"list"|"unlist","note":"..."}
//
// 与管理员上架的区别只在两点：① 必须先校验归属（只能是自己的号）；
// ② 成交金额全额归卖家（平台自持号才归平台，见 BuyAccount 注释）。
func (a *app) handleUserAccountSale(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	if u == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "未登录"})
		return
	}
	uid := r.PathValue("uid")
	acc := a.requireOwnedAccount(w, uid, u.ID)
	if acc == nil {
		return
	}
	var req struct {
		Action string  `json:"action"`
		Note   string  `json:"note"`
		Price  float64 `json:"price"` // 挂牌价（元）
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	note := strings.TrimSpace(req.Note)
	if len(note) > 200 {
		note = note[:200]
	}
	priceMicro, perr := a.parseSalePrice(req.Price)
	if perr != "" && strings.EqualFold(strings.TrimSpace(req.Action), "list") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": perr})
		return
	}
	sale, err := a.store.GetAccountSale(uid)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	credits, creditsKnown := a.poolCredits(uid)

	switch strings.ToLower(strings.TrimSpace(req.Action)) {
	case "list":
		if code, msg := a.listAccountForSale(acc, sale, credits, creditsKnown, u.ID, note, priceMicro); msg != "" {
			writeJSON(w, code, map[string]any{"error": msg})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"uid": uid, "on_sale": true, "consume_group": cgIdle,
			"consume_group_text": consumeGroupLabel(cgIdle),
		})
	case "unlist":
		if code, msg := a.unlistAccount(acc, sale, u.ID); msg != "" {
			writeJSON(w, code, map[string]any{"error": msg})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"uid": uid, "on_sale": false})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "未知动作（list/unlist）"})
	}
}

// handleBuyAccount 用户对在售账号发起买断。
//
//	POST /api/sale/accounts/{uid}/buy  {"amount": 123.45}
//
// 交互为「一次提交即成交」：金额随请求给定，服务端在同一事务内完成
// 「校验 → 扣款 → 分账 → 转移归属 → 记账」，成功即返回买断凭证。
func (a *app) handleBuyAccount(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	uid := r.PathValue("uid")
	// body 可为空：成交价取自挂单（v0.21.0），不接受买家自带金额。
	_ = json.NewDecoder(r.Body).Decode(&buyRequest{})

	acc, err := a.store.GetAccount(uid)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if acc == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "账号不存在"})
		return
	}
	sale, err := a.store.GetAccountSale(uid)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if sale == nil || !sale.OnSale {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "该账号已下架或已被买断"})
		return
	}
	if acc.OwnerUserID == u.ID {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "不能买断自己的账号"})
		return
	}
	// 账号状态异常（上架后可能被禁用/归档）：自动下架并拒绝，避免买到坏号。
	if acc.Disabled || effectiveConsumeGroup(acc) == cgAbnormal {
		_ = a.store.SetAccountOnSale(uid, false, 0, "", "", 0)
		_ = a.proxy.SyncAccounts()
		writeJSON(w, http.StatusConflict, map[string]any{"error": "该账号状态异常，已自动下架"})
		return
	}
	// 成交前二次校验交易积分门槛（v0.20.0）：上架时达标不等于成交时仍达标 ——
	// 账号可能在上架期间被平台消耗掉大量积分。不达标则**自动下架**并拒绝，
	// 避免买家买到与挂牌描述不符（积分已空）的号。
	if credits, known := a.poolCredits(uid); !known || credits < tradeMinCredits {
		_ = a.store.SetAccountOnSale(uid, false, 0, "", "", 0)
		_ = a.proxy.SyncAccounts()
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": fmt.Sprintf("该账号积分已不满足交易门槛（需 %d，当前 %d），已自动下架",
				tradeMinCredits, credits),
			"credits": credits,
		})
		return
	}

	p, err := a.store.BuyAccount(uid, u.ID)
	if err != nil {
		switch {
		case errors.Is(err, errAlreadySold):
			writeJSON(w, http.StatusConflict, map[string]any{"error": "手慢了，该账号已被买断或下架"})
		case errors.Is(err, ErrInsufficientBalance):
			need := 0.0
			if sal, serr := a.store.GetAccountSale(uid); serr == nil && sal != nil {
				need = MicroToPoint(sal.SalePriceMicro)
			}
			fresh, _ := a.store.GetUserByID(u.ID)
			bal := float64(0)
			if fresh != nil {
				bal = MicroToPoint(fresh.BalanceMicro)
			}
			writeJSON(w, http.StatusPaymentRequired, map[string]any{
				"error":   fmt.Sprintf("余额不足：需 %.2f 点，当前 %.2f 点", need, bal),
				"balance": bal,
				"need":    need,
			})
		case strings.Contains(err.Error(), "your own"):
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "不能买断自己的账号"})
		default:
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		}
		return
	}

	// 买断后立即重算选号范围：账号已退出平台号池，不应再被公共反代选中。
	_ = a.proxy.SyncAccounts()
	log.Printf("INFO: [sale] account %s bought by user %d for %.2f points (seller=%d share=%.2f)",
		uid, u.ID, MicroToPoint(p.AmountMicro), p.SellerUserID, MicroToPoint(p.SellerMicro))

	fresh, _ := a.store.GetUserByID(u.ID)
	bal := float64(0)
	if fresh != nil {
		bal = MicroToPoint(fresh.BalanceMicro)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"uid":        uid,
		"amount":     MicroToPoint(p.AmountMicro),
		"balance":    bal,
		"host_group": hostGroupBought,
		"message":    "买断成功，账号已进入你的代挂序列，可用自用 Key 反代",
	})
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// parseSalePrice 把「元」为单位的挂牌价换算成微点，并做基本合法性检查。
//
// 为什么输入是「元」而存储是「微点」：需求 2 要求以货币（元）为输入单位；
// 而站内所有金额（余额、结算、台账）统一用微点存储（1 点 = 1000 微点，
// 1 元 = 1/折算比例 点）。这里用**折算比例**做桥梁，保证与余额同一口径。
//
// 返回 (priceMicro, 错误文案)。错误文案为空表示成功。
// 注意：折算比例由调用方注入（moneyRate），本函数只做算术与边界检查。
func parseSalePriceYuan(yuan float64, rate float64) (int64, string) {
	if math.IsNaN(yuan) || math.IsInf(yuan, 0) {
		return 0, "挂牌价格式不正确"
	}
	if yuan <= 0 {
		return 0, "请填写大于 0 的挂牌价"
	}
	if rate <= 0 {
		return 0, "折算比例未配置，请联系管理员"
	}
	// 小数位校验必须做在**元**域：1.234 元 = 123.4 点 = 123400 微点，
	// 在微点域是整除的（微点精度 0.001 点 = 0.00001 元），微点检查拦不住它。
	// 需求以「元」为输入单位、限两位小数，故这里按 yuan×100 是否为整数判定。
	if step := math.Pow10(salePriceDecimals); math.Abs(yuan*step-math.Round(yuan*step)) > 1e-6 {
		return 0, fmt.Sprintf("挂牌价最多保留 %d 位小数", salePriceDecimals)
	}
	points := yuan / rate // 元 → 点
	if points > maxSalePricePoints {
		return 0, fmt.Sprintf("挂牌价超出上限（%.0f 元）", maxSalePricePoints*rate)
	}
	micro := PointToMicro(points)
	if msg := validateSalePrice(micro); msg != "" {
		return 0, msg
	}
	return micro, ""
}

// salePriceOf 取在售记录的挂牌价（点）；无记录/未在售返回 0。
func salePriceOf(s *AccountSale) float64 {
	if s == nil || !s.OnSale {
		return 0
	}
	return MicroToPoint(s.SalePriceMicro)
}

// round2 保留两位小数（金额展示）。
func round2(f float64) float64 { return math.Round(f*100) / 100 }

// toInt64 视图 map 取 int64 字段（排序用）。
func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	case string:
		x, _ := strconv.ParseInt(n, 10, 64)
		return x
	}
	return 0
}
