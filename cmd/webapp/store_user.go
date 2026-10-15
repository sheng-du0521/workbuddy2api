// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"time"
)

// 余额单位：微点（micro point）。1 点 = 1000 微点。
// 用整数记账避免浮点累积误差；对外展示一律除以 MicroPerPoint。
const MicroPerPoint = 1000

// PointToMicro 点数 → 微点。
//
// 必须用 math.Round 而不是 `int64(p*1000 + 0.5)`：Go 的 float→int 是向零截断，
// 对负数而言 `+0.5` 不是四舍五入 —— int64(-95000 + 0.5) = int64(-94999.5) 截断成
// -94999，每笔负向调整都会少扣 1 微点。math.Round 是「四舍五入、中点远离零」。
func PointToMicro(p float64) int64 { return int64(math.Round(p * MicroPerPoint)) }

// MicroToPoint 微点 → 点数（浮点，仅供展示）。
func MicroToPoint(m int64) float64 { return float64(m) / MicroPerPoint }

// 用户角色与状态。
const (
	RoleUser  = "user"
	RoleAdmin = "admin"

	StatusActive = "active"
	StatusBanned = "banned"
)

// User 一个租户账号。
type User struct {
	ID           int64
	Username     string
	PasswordHash string
	Role         string
	Status       string
	BalanceMicro int64
	// 消费闸（0 = 不限）。按自然日 / 自然月统计已结算流水，超限拒绝新请求。
	// 这是「防跑飞」的第二道闸，第一道是余额本身。
	DailyLimitMicro   int64
	MonthlyLimitMicro int64
	// InviteCode 本人的推广邀请码（注册即可分享给别人）；ReferredBy 本人的上级（0 = 无）。
	// 上级在「下级兑换充值码」「下级号池收益」时按推广返点比例获得返点。
	InviteCode string
	ReferredBy int64
	// WeChat 微信号（v0.21.0）：注册时选填，供管理员联系用户。
	WeChat string

	// FrozenMicro 已冻结金额（微点，v0.27.0）：提现申请提交时从**可提现额度**转入，
	// 审核通过时随余额一并扣除、驳回时原路退回可提现额度。
	//
	// 为什么要冻结（而不是像 v0.21.0 那样只在查询时减去待审额）：
	// 「待审额」是**派生值**，不占住任何东西 —— 用户在等审核期间继续消费或再提一笔，
	// 余额就会被打到低于待审额，审核那一刻必然失败（`ErrInsufficientWithdraw`），
	// 表现为管理员点「通过并打款」被拒、只能驳回。冻结把这件事变成**不可能发生**：
	// 钱在提交那一刻就离开了可提现额度，审核时只可能成功或由管理员主动驳回。
	//
	// 不变量（v0.27.0 起）：withdrawable_micro + frozen_micro ≤ balance_micro，
	// 且 frozen_micro == Σ(status='pending' 的申请额)。
	FrozenMicro int64

	// ---- 余额分桶（v0.21.0）----
	//
	// 为什么分桶：需求要求「体验点不可提现、充值点不可提现、**仅返点收益与账号买卖金额
	// 可提现**」。单一 balance_micro 无法表达「哪些钱能提」，故拆成三桶，且恒满足：
	//
	//	balance_micro == exp_micro + recharge_micro + withdrawable_micro
	//
	// （允许同时为负：结算路径允许欠费，见 ChargeUser。）
	//
	//	ExpMicro          体验点 —— 体验兑换码到账。可消耗；不可提现；**不参与返点计算**
	//	                  （用它充值不给上级产生推广返点）。
	//	RechargeMicro     充值点 —— 充值兑换码 / 管理员手动调整到账。可消耗；不可提现；
	//	                  参与返点计算（兑换充值码会给上级产生推广返点）。
	//	WithdrawableMicro 可提现余额 —— **仅**返点收益（号池收益 + 推广返点）与账号买卖所得
	//	                  入账。可消耗，也可提现。
	//
	// 扣减优先级：体验 → 充值 → 可提现（对用户最有利：先花掉不可提现的部分）。
	ExpMicro          int64
	RechargeMicro     int64
	WithdrawableMicro int64
	// HostRatio / ReferralRatio 逐用户返点比例覆盖（v0.17.7）。
	//
	//   -1 = 未覆盖，跟随全局配置（host_share_ratio / referral_ratio）；
	//   [0,1] = 该用户专属比例（0 = 明确不分成，与「未覆盖」语义不同）。
	//
	// 为什么用 -1 哨兵而不是加一个 has_override 布尔列：SQLite 的 ADD COLUMN 只能带
	// 常量默认值，用 -1 一次到位；且 REAL 列天然容纳哨兵，不必再引入第二列的同步问题。
	// 语义分界必须保持清晰：-1（未覆盖）≠ 0（明确为零），前端据此区分「跟随全局」与「已设 0%」。
	HostRatio     float64
	ReferralRatio float64
	CreatedAt     int64
	UpdatedAt     int64
}

// Reservation 一笔在途预扣。
//
// 为什么要落库：预扣是「先扣后算」，进程如果中途崩溃，这笔钱就悬空了。
// 落库后启动时可对账退还，保证不会因为崩溃把用户余额吃掉。
type Reservation struct {
	ID          int64
	UserID      int64
	KeyID       int64
	AmountMicro int64
	Model       string
	CreatedAt   int64
}

// ErrInsufficientBalance 预扣时余额不足。
var ErrInsufficientBalance = errors.New("insufficient balance")

// IsAdmin 是否管理员。
func (u *User) IsAdmin() bool { return u.Role == RoleAdmin }

// Key 类型："" = 平台计费 Key（走兑换余额）；KeyKindHost = 代挂自用 Key。
const (
	KeyKindHost = "host"
)

// APIKey 用户申请的一把调用密钥。
// 明文经 AES-GCM 加密存 KeyEnc（可回显），KeyHash 用于登录时定位（唯一索引）。
type APIKey struct {
	ID        int64
	UserID    int64
	Name      string
	KeyHash   string
	KeyEnc    string
	KeyPrefix string
	Status    string
	// Kind "" = 平台计费；KeyKindHost = 代挂自用（只用本人代挂账号，不扣余额）。
	Kind string
	// HostGroup 代挂自用 Key 限定的代挂分组（空 = 全部本人代挂账号）。
	HostGroup string
	// AccountUIDs 代挂自用 Key 限定的具体账号集合（逗号分隔 uid；空 = 不按账号限定）。
	// v0.11.0：Key 生成照「API 反代」页，可逐账号勾选参与反代的账号。
	AccountUIDs string
	LastUsedAt  int64
	CreatedAt   int64
}

// HostEarning 一笔「代挂账号在池中为号主赚到」的收益流水（对账用）。
//
// 记账口径：平台按 charged_micro 向调用方收费，其中 earned_micro 按分成比例
// 折算入号主余额。earned < charged 的差额即平台留成。
type HostEarning struct {
	ID          int64
	AccountUID  string
	OwnerUserID int64
	FromUserID  int64
	Model       string
	Charged     int64 // 平台向调用方收取的微点
	Earned      int64 // 折算给号主的微点
	CreatedAt   int64
}

// 用量流水的 scope：区分「用平台号池」与「用自己的代挂账号自用」。
// 控制台用量账单据此把消费拆成三类（自用 / 用号池 / 进号池的收益）。
const (
	ScopePool = "pool" // 走平台号池（平台计费 Key）
	ScopeHost = "host" // 走号主自己的代挂账号（代挂自用 Key，免费）
)

// UsageLog 一笔用量流水。
type UsageLog struct {
	ID               int64
	UserID           int64
	KeyID            int64
	Model            string
	PromptTokens     int
	CompletionTokens int
	UpstreamCredit   float64
	ChargedMicro     int64
	UpstreamUID      string
	Scope            string
	Status           string
	ErrorMsg         string
	// StickyKeyHash 会话粘性键的 sha256 前 12 hex（v0.17.5）。空 = 请求无会话标识。
	// 用途：把「同一场对话的多轮」在流水里串起来，回答「有没有在调用过程中换号」。
	StickyKeyHash string
	// Attempts 本次请求实际尝试的账号数（v0.17.5）。1 = 首选成功；>1 = 中途换过号，
	// 说明那一轮发生过失败回退（冷却/4xx/传输错误）。
	Attempts int
	// 上游前缀缓存的三个计数（v0.26.7）。
	//
	// 为什么落库而不是只放内存：命中率是回答"为什么又慢又贵"的**唯一直接证据** ——
	// 48 万 token 的请求若命中缓存，预处理几乎免费；若未命中则每次全量重算
	// （实测 92~129 秒 / 24~25 点）。只放实时面板的话重启即丢，无法做趋势判断。
	CacheHitTokens   int
	CacheMissTokens  int
	CacheWriteTokens int
	// LatencyMS 端到端耗时（毫秒，v0.26.7）。同理由"内存态 90 分钟"落到账上。
	LatencyMS int64
	CreatedAt int64
}

// 返点类型。
const (
	// RebateReferralRecharge 推广返点 · 下级兑换充值码时给上级的返点。
	RebateReferralRecharge = "referral_recharge"
	// RebateReferralPool 推广返点 · 下级的代挂账号在号池赚到收益时，再给上级的返点。
	RebateReferralPool = "referral_pool"
)

// Rebate 一笔返点台账。
//
// BaseMicro 是计返基数：充值返点用「下级本次到账点数」，
// 下级号池返点用「下级本次号池收益点数」。RebateMicro 是按比例算出的实际返点。
type Rebate struct {
	ID          int64
	Kind        string
	Beneficiary int64 // 拿返点的人（上级）
	FromUserID  int64 // 下级
	AccountUID  string
	BaseMicro   int64
	RebateMicro int64
	Note        string
	CreatedAt   int64
}

// RedeemCode 充值兑换码。
type RedeemCode struct {
	Code         string
	CreditsMicro int64
	// Kind 兑换码类型（v0.21.0）：
	//   "exp"      体验码 —— 到账记入**体验点**桶；可消耗、不可提现、**不产生推广返点**
	//   "recharge" 充值码 —— 到账记入**充值点**桶；可消耗、不可提现、**产生推广返点**
	// 老库无此列 → 默认 "recharge"（保守：老码按充值码对待）。
	Kind      string
	CreatedBy int64
	UsedBy    int64
	UsedAt    int64
	CreatedAt int64
}

// RedeemKindExp / RedeemKindRecharge 兑换码类型取值。
const (
	RedeemKindExp      = "exp"
	RedeemKindRecharge = "recharge"
)

// normalizeRedeemKind 校验并规范化兑换码类型；空串按充值码（默认，兼容老数据）。
func normalizeRedeemKind(k string) string {
	if strings.ToLower(strings.TrimSpace(k)) == RedeemKindExp {
		return RedeemKindExp
	}
	return RedeemKindRecharge
}

// migrateUsers 建租户相关表（幂等）。
func (s *Store) migrateUsers() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			username      TEXT NOT NULL UNIQUE,
			password_hash TEXT NOT NULL,
			role          TEXT NOT NULL DEFAULT 'user',
			status        TEXT NOT NULL DEFAULT 'active',
			balance_micro INTEGER NOT NULL DEFAULT 0,
			invite_code   TEXT NOT NULL DEFAULT '',
			referred_by   INTEGER NOT NULL DEFAULT 0,
			created_at    INTEGER NOT NULL DEFAULT 0,
			updated_at    INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS api_keys (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id      INTEGER NOT NULL,
			name         TEXT NOT NULL DEFAULT '',
			key_hash     TEXT NOT NULL UNIQUE,
			key_enc      TEXT NOT NULL DEFAULT '',
			key_prefix   TEXT NOT NULL DEFAULT '',
			status       TEXT NOT NULL DEFAULT 'active',
			kind         TEXT NOT NULL DEFAULT '',
			host_group   TEXT NOT NULL DEFAULT '',
			account_uids TEXT NOT NULL DEFAULT '',
			last_used_at INTEGER NOT NULL DEFAULT 0,
			created_at   INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_api_keys_user ON api_keys(user_id)`,
		`CREATE TABLE IF NOT EXISTS usage_logs (
			id                INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id           INTEGER NOT NULL,
			key_id            INTEGER NOT NULL DEFAULT 0,
			model             TEXT NOT NULL DEFAULT '',
			prompt_tokens     INTEGER NOT NULL DEFAULT 0,
			completion_tokens INTEGER NOT NULL DEFAULT 0,
			upstream_credit   REAL NOT NULL DEFAULT 0,
			charged_micro     INTEGER NOT NULL DEFAULT 0,
			upstream_uid      TEXT NOT NULL DEFAULT '',
			scope             TEXT NOT NULL DEFAULT 'pool',
			status            TEXT NOT NULL DEFAULT 'ok',
			error_msg         TEXT NOT NULL DEFAULT '',
			created_at        INTEGER NOT NULL DEFAULT 0,
			sticky_key_hash   TEXT NOT NULL DEFAULT '',
			attempts          INTEGER NOT NULL DEFAULT 0,
			cache_hit_tokens   INTEGER NOT NULL DEFAULT 0,
			cache_miss_tokens  INTEGER NOT NULL DEFAULT 0,
			cache_write_tokens INTEGER NOT NULL DEFAULT 0,
			latency_ms         INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_user_time ON usage_logs(user_id, created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS redeem_codes (
			code          TEXT PRIMARY KEY,
			credits_micro INTEGER NOT NULL,
			kind          TEXT NOT NULL DEFAULT 'recharge',
			created_by    INTEGER NOT NULL DEFAULT 0,
			used_by       INTEGER NOT NULL DEFAULT 0,
			used_at       INTEGER NOT NULL DEFAULT 0,
			created_at    INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			token_hash TEXT PRIMARY KEY,
			user_id    INTEGER NOT NULL,
			expires_at INTEGER NOT NULL,
			created_at INTEGER NOT NULL DEFAULT 0
		)`,
		// 在途预扣：进程崩溃后靠它对账退还，避免吞掉用户余额。
		`CREATE TABLE IF NOT EXISTS reservations (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id      INTEGER NOT NULL,
			key_id       INTEGER NOT NULL DEFAULT 0,
			amount_micro INTEGER NOT NULL,
			model        TEXT NOT NULL DEFAULT '',
			created_at   INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_reservations_user ON reservations(user_id)`,
		// 代挂收益台账：每一笔「代挂账号服务平台流量」的分成入账一行，供号主对账。
		`CREATE TABLE IF NOT EXISTS host_earnings (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			account_uid   TEXT NOT NULL DEFAULT '',
			owner_user_id INTEGER NOT NULL,
			from_user_id  INTEGER NOT NULL DEFAULT 0,
			model         TEXT NOT NULL DEFAULT '',
			charged_micro INTEGER NOT NULL DEFAULT 0,
			earned_micro  INTEGER NOT NULL DEFAULT 0,
			created_at    INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_host_earnings_owner ON host_earnings(owner_user_id, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_accounts_owner ON accounts(owner_user_id)`,
		// 返点台账：推广返点（下级充值 / 下级号池收益）逐笔入账，供上级对账。
		`CREATE TABLE IF NOT EXISTS rebates (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			kind         TEXT NOT NULL DEFAULT '',
			beneficiary  INTEGER NOT NULL,
			from_user_id INTEGER NOT NULL DEFAULT 0,
			account_uid  TEXT NOT NULL DEFAULT '',
			base_micro   INTEGER NOT NULL DEFAULT 0,
			rebate_micro INTEGER NOT NULL DEFAULT 0,
			note         TEXT NOT NULL DEFAULT '',
			created_at   INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_rebates_beneficiary ON rebates(beneficiary, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_rebates_from ON rebates(from_user_id)`,
		// 提现台账（v0.11.0）：用户发起申请 → 管理员审核 →（通过时）扣余额并记打款。
		// 记账语义：点为内部单位，提现仅做台账与打款记录，不接真实支付通道。
		`CREATE TABLE IF NOT EXISTS withdrawals (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id        INTEGER NOT NULL,
			amount_micro   INTEGER NOT NULL,
			fee_micro      INTEGER NOT NULL DEFAULT 0,
			method         TEXT NOT NULL DEFAULT 'alipay',
			account_info   TEXT NOT NULL DEFAULT '',
			alipay_account TEXT NOT NULL DEFAULT '',
			real_name      TEXT NOT NULL DEFAULT '',
			status         TEXT NOT NULL DEFAULT 'pending',
			note         TEXT NOT NULL DEFAULT '',
			reviewed_by  INTEGER NOT NULL DEFAULT 0,
			reviewed_at  INTEGER NOT NULL DEFAULT 0,
			created_at   INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_withdrawals_user ON withdrawals(user_id, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_withdrawals_status ON withdrawals(status, created_at DESC)`,
	}
	for _, st := range stmts {
		if _, err := s.db.Exec(st); err != nil {
			return fmt.Errorf("migrate users: %w", err)
		}
	}
	// users 表的增量列（老库升级用，ADD COLUMN 不幂等所以先探测）。
	for _, c := range []struct{ col, ddl string }{
		{"daily_limit_micro", "daily_limit_micro INTEGER NOT NULL DEFAULT 0"},
		{"monthly_limit_micro", "monthly_limit_micro INTEGER NOT NULL DEFAULT 0"},
		{"invite_code", "invite_code TEXT NOT NULL DEFAULT ''"},
		{"referred_by", "referred_by INTEGER NOT NULL DEFAULT 0"},
		// 逐用户返点比例覆盖（v0.17.7）：-1 = 未覆盖（跟随全局），[0,1] = 专属比例。
		{"host_ratio", "host_ratio REAL NOT NULL DEFAULT -1"},
		{"referral_ratio", "referral_ratio REAL NOT NULL DEFAULT -1"},
		// 微信号（v0.21.0）：注册时选填。
		{"wechat", "wechat TEXT NOT NULL DEFAULT ''"},
		// 余额分桶（v0.21.0）：见 User 结构注释。老库全 0，由 backfillBalanceBuckets 回填。
		{"exp_micro", "exp_micro INTEGER NOT NULL DEFAULT 0"},
		{"recharge_micro", "recharge_micro INTEGER NOT NULL DEFAULT 0"},
		{"withdrawable_micro", "withdrawable_micro INTEGER NOT NULL DEFAULT 0"},
		// 提现冻结（v0.27.0）：见 User.FrozenMicro。老库全 0，由
		// backfillWithdrawFreeze 把**已存在的待审申请**补冻（否则老 pending 记录
		// 的可提现额度仍暴露在 withdrawable 里，能被重复申请）。
		{"frozen_micro", "frozen_micro INTEGER NOT NULL DEFAULT 0"},
	} {
		if err := s.ensureColumn("users", c.col, c.ddl); err != nil {
			return fmt.Errorf("migrate users column %s: %w", c.col, err)
		}
	}
	// usage_logs 的 scope 增量列（v0.10.0 三类账单）。
	if err := s.ensureColumn("usage_logs", "scope", "scope TEXT NOT NULL DEFAULT 'pool'"); err != nil {
		return fmt.Errorf("migrate usage_logs column scope: %w", err)
	}
	// 选号轨迹增量列（v0.17.5）：把「同一场对话的多轮」在流水里串起来，
	// 用于判定「调用过程中是否换号」。老库平滑补齐，历史行留空/0 即可。
	if err := s.ensureColumn("usage_logs", "sticky_key_hash", "sticky_key_hash TEXT NOT NULL DEFAULT ''"); err != nil {
		return fmt.Errorf("migrate usage_logs column sticky_key_hash: %w", err)
	}
	if err := s.ensureColumn("usage_logs", "attempts", "attempts INTEGER NOT NULL DEFAULT 0"); err != nil {
		return fmt.Errorf("migrate usage_logs column attempts: %w", err)
	}
	// 缓存命中与耗时增量列（v0.26.7）。老库平滑补齐：历史行留 0。
	//
	// 0 = "未采集"而非"命中 0 次" —— 历史行本就无从考据，当成 0 会让命中率被
	// 严重低估。统计侧据此只在**有观测**的行上算（hit+miss > 0）。
	for _, c := range []struct{ col, ddl string }{
		{"cache_hit_tokens", "cache_hit_tokens INTEGER NOT NULL DEFAULT 0"},
		{"cache_miss_tokens", "cache_miss_tokens INTEGER NOT NULL DEFAULT 0"},
		{"cache_write_tokens", "cache_write_tokens INTEGER NOT NULL DEFAULT 0"},
		{"latency_ms", "latency_ms INTEGER NOT NULL DEFAULT 0"},
	} {
		if err := s.ensureColumn("usage_logs", c.col, c.ddl); err != nil {
			return fmt.Errorf("migrate usage_logs column %s: %w", c.col, err)
		}
	}
	// scope 索引必须在列的 ensureColumn 之后建（老库建表语句不会补列）。
	// usage_logs 的补充索引（v0.24.0，P2-6）。
	//
	// 为什么补：这三个查询条件此前**都没有可用索引**，于是每次全表扫描：
	//   · created_at   —— UsageByUser / CountUsageUsers（管理端报表、用户排行）
	//   · upstream_uid —— UsageByAccount（**号池仪表盘每 3 秒调用一次**）
	//   · sticky_key_hash —— 选号轨迹按会话聚合
	// 现有 idx_usage_user_time 的前导列是 user_id，纯 created_at 条件用不上它。
	//
	// 当前数据量小（生产库 usage_logs 仅千行级），影响可忽略；但 usage_logs 是
	// 唯一无界增长的表，且仪表盘是 3 秒轮询 —— 随规模增长会变成"每次轮询一次全表扫"。
	// 补索引是廉价的预防（写入放大小到可忽略，本表是纯追加）。
	for _, ddl := range []string{
		`CREATE INDEX IF NOT EXISTS idx_usage_created ON usage_logs(created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_upstream ON usage_logs(upstream_uid)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_sticky ON usage_logs(sticky_key_hash)`,
	} {
		if _, err := s.db.Exec(ddl); err != nil {
			return fmt.Errorf("migrate usage_logs index: %w", err)
		}
	}
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_usage_user_scope ON usage_logs(user_id, scope, created_at DESC)`); err != nil {
		return fmt.Errorf("migrate usage_logs scope index: %w", err)
	}
	// 邀请码唯一索引（部分索引：空串不参与，避免老库一堆空值互撞）。
	if _, err := s.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_users_invite ON users(invite_code) WHERE invite_code <> ''`); err != nil {
		return fmt.Errorf("migrate users invite index: %w", err)
	}
	// redeem_codes.kind（v0.21.0）：体验 / 充值两类兑换码。
	if err := s.ensureColumn("redeem_codes", "kind", "kind TEXT NOT NULL DEFAULT 'recharge'"); err != nil {
		return fmt.Errorf("migrate redeem_codes column kind: %w", err)
	}
	// withdrawals 的支付宝结构化字段（v0.21.0）：account_info 保留（旧行回显用）。
	for _, c := range []struct{ col, ddl string }{
		{"alipay_account", "alipay_account TEXT NOT NULL DEFAULT ''"},
		{"real_name", "real_name TEXT NOT NULL DEFAULT ''"},
	} {
		if err := s.ensureColumn("withdrawals", c.col, c.ddl); err != nil {
			return fmt.Errorf("migrate withdrawals column %s: %w", c.col, err)
		}
	}
	// api_keys 表的增量列（v0.9.0 代挂 Key）。
	for _, c := range []struct{ col, ddl string }{
		{"kind", "kind TEXT NOT NULL DEFAULT ''"},
		{"host_group", "host_group TEXT NOT NULL DEFAULT ''"},
		{"account_uids", "account_uids TEXT NOT NULL DEFAULT ''"},
	} {
		if err := s.ensureColumn("api_keys", c.col, c.ddl); err != nil {
			return fmt.Errorf("migrate api_keys column %s: %w", c.col, err)
		}
	}
	return nil
}

// InsertHostEarning 记一笔代挂分成入账。
func (s *Store) InsertHostEarning(e *HostEarning) error {
	if e.CreatedAt == 0 {
		e.CreatedAt = time.Now().Unix()
	}
	_, err := s.db.Exec(`INSERT INTO host_earnings
		(account_uid, owner_user_id, from_user_id, model, charged_micro, earned_micro, created_at)
		VALUES (?,?,?,?,?,?,?)`,
		e.AccountUID, e.OwnerUserID, e.FromUserID, e.Model, e.Charged, e.Earned, e.CreatedAt)
	return err
}

// ListHostEarnings 某号主的收益流水（倒序）。
func (s *Store) ListHostEarnings(ownerUserID int64, limit int) ([]*HostEarning, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT id, account_uid, owner_user_id, from_user_id, model,
		charged_micro, earned_micro, created_at
		FROM host_earnings WHERE owner_user_id = ? ORDER BY created_at DESC, id DESC LIMIT ?`,
		ownerUserID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*HostEarning
	for rows.Next() {
		var e HostEarning
		if err := rows.Scan(&e.ID, &e.AccountUID, &e.OwnerUserID, &e.FromUserID, &e.Model,
			&e.Charged, &e.Earned, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

// HostEarningSummary 某号主的收益汇总：总入账微点 + 笔数。
func (s *Store) HostEarningSummary(ownerUserID int64) (totalMicro int64, count int64, err error) {
	err = s.db.QueryRow(`SELECT COALESCE(SUM(earned_micro),0), COUNT(*) FROM host_earnings WHERE owner_user_id = ?`,
		ownerUserID).Scan(&totalMicro, &count)
	return
}

// ensureColumn 表缺少某列时补上（SQLite 的 ADD COLUMN 无 IF NOT EXISTS，需先探测）。
func (s *Store) ensureColumn(table, column, ddl string) error {
	rows, err := s.db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var (
			cid, notNull, pk int
			name, ctype      string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == column {
			found = true
			break
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = s.db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + ddl)
	return err
}

// ---------- 用户 ----------

// CreateUser 新建用户（普通角色）。
func (s *Store) CreateUser(username, passwordHash string) (*User, error) {
	return s.CreateUserWithWeChat(username, passwordHash, "")
}

// CreateUserWithWeChat 建用户并可一并写入微信号（v0.21.0，注册页选填）。
//
// v0.24.2（P3-12）起**不再**把首个用户自动提为管理员 —— 角色由调用方
// 通过 CreateUserAs 显式指定。原因：库为空 + 开放注册 = 任何人抢先注册即成管理员，
// 而公网隧道往往先于运维本人就绪（详见 handleRegister 的说明）。
func (s *Store) CreateUserWithWeChat(username, passwordHash, wechat string) (*User, error) {
	return s.CreateUserAs(username, passwordHash, wechat, RoleUser)
}

// CreateUserAs 建用户并**显式指定角色**（v0.24.2，P3-12）。
//
// 为什么要把角色提到参数位：此前「首个用户 = 管理员」的判定藏在建用户的 SQL 路径里，
// 既看不见也无法在调用点审计。现在建库只负责建库，**谁配当管理员是调用方的决定**
// （handleRegister 用 HasAdmin + 引导密钥判定）。
func (s *Store) CreateUserAs(username, passwordHash, wechat, role string) (*User, error) {
	if role != RoleAdmin && role != RoleUser {
		role = RoleUser
	}
	now := time.Now().Unix()
	for attempt := 0; attempt < 8; attempt++ {
		res, err := s.db.Exec(
			`INSERT INTO users (username, password_hash, role, status, balance_micro, invite_code, referred_by, wechat, created_at, updated_at)
			 VALUES (?,?,?,?,0,?,0,?,?,?)`,
			username, passwordHash, role, StatusActive, genInviteCode(), strings.TrimSpace(wechat), now, now,
		)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				if strings.Contains(err.Error(), "username") {
					return nil, ErrUserExists
				}
				continue // 邀请码撞了，换一个重试
			}
			return nil, err
		}
		id, _ := res.LastInsertId()
		return s.GetUserByID(id)
	}
	return nil, errors.New("生成邀请码失败，请重试")
}

// HasAdmin 是否已存在管理员账号（v0.24.2，P3-12）。
//
// 判定条件是「有没有管理员」而不是「有没有用户」：否则抢注者只要先建一个普通账号，
// 就能把合法的首次初始化永久锁死（n 永远 > 0，真正的主人再也提不了权）。
// 错误按「保守」处理：查询失败返回 true（视为已有管理员，不发放提权），
// 宁可多输一次引导密钥，也不要因为一次库抖动把管理员给错人。
func (s *Store) HasAdmin() bool {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE role = ?`, RoleAdmin).Scan(&n); err != nil {
		return true
	}
	return n > 0
}

// genInviteCode 生成 8 位邀请码（去掉了 0/O/1/I 等易混淆字符）。
func genInviteCode() string {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 8)
	for i := range b {
		b[i] = alphabet[randByte()%byte(len(alphabet))]
	}
	return string(b)
}

// randByte 取一个随机字节。
//
// 熵源失败即 panic（v0.24.1，P3-3）：此前退化成时间戳低位，而调用方是
// genInviteCode（邀请码）。邀请码可预测 → 可枚举他人邀请码骗返点。见 secureBytes。
func randByte() byte {
	return secureBytes(1)[0]
}

// ErrUserExists 用户名已被占用。
var ErrUserExists = errors.New("username already exists")

// ErrNotFound 目标不存在。
var ErrNotFound = errors.New("not found")

const userCols = `id, username, password_hash, role, status, balance_micro,
	daily_limit_micro, monthly_limit_micro, invite_code, referred_by,
	host_ratio, referral_ratio, wechat,
	exp_micro, recharge_micro, withdrawable_micro, frozen_micro, created_at, updated_at`

func scanUser(row interface{ Scan(dest ...any) error }) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Status,
		&u.BalanceMicro, &u.DailyLimitMicro, &u.MonthlyLimitMicro,
		&u.InviteCode, &u.ReferredBy,
		&u.HostRatio, &u.ReferralRatio, &u.WeChat,
		&u.ExpMicro, &u.RechargeMicro, &u.WithdrawableMicro, &u.FrozenMicro,
		&u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// GetUserByID 按主键取用户。
func (s *Store) GetUserByID(id int64) (*User, error) {
	u, err := scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM users WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return u, err
}

// GetUserByUsername 按用户名取用户。
func (s *Store) GetUserByUsername(name string) (*User, error) {
	u, err := scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM users WHERE username = ?`, name))
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return u, err
}

// CountUsers 用户总数（统计/展示用）。
//
// 注：v0.24.2（P3-12）起**不再**用于判定首个管理员 —— 那个判定已换成 HasAdmin
// （判定「有没有管理员」而非「有没有用户」，否则抢注者用一个小号就能锁死初始化）。
func (s *Store) CountUsers() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// EnsureInviteCode 返回本人的邀请码；老账号（迁移后为空）就地补发一个。
func (s *Store) EnsureInviteCode(userID int64) (string, error) {
	u, err := s.GetUserByID(userID)
	if err != nil {
		return "", err
	}
	if u.InviteCode != "" {
		return u.InviteCode, nil
	}
	for i := 0; i < 8; i++ {
		code := genInviteCode()
		res, err := s.db.Exec(`UPDATE users SET invite_code = ?, updated_at = ? WHERE id = ? AND invite_code = ''`,
			code, time.Now().Unix(), userID)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				continue
			}
			return "", err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return code, nil
		}
		// 已被并发写好：直接读回。
		if u2, err := s.GetUserByID(userID); err == nil && u2.InviteCode != "" {
			return u2.InviteCode, nil
		}
	}
	return "", errors.New("生成邀请码失败")
}

// GetUserByInviteCode 按邀请码找上级（大小写不敏感；空码恒不匹配）。
func (s *Store) GetUserByInviteCode(code string) (*User, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" {
		return nil, ErrNotFound
	}
	u, err := scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM users WHERE invite_code = ?`, code))
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return u, err
}

// SetUserReferredBy 绑定推荐关系（仅当目标用户尚未绑定、且不是自己时生效）。
func (s *Store) SetUserReferredBy(userID, referrerID int64) error {
	if userID <= 0 || referrerID <= 0 || userID == referrerID {
		return errors.New("invalid referral")
	}
	_, err := s.db.Exec(`UPDATE users SET referred_by = ?, updated_at = ? WHERE id = ? AND referred_by = 0`,
		referrerID, time.Now().Unix(), userID)
	return err
}

// RatioNotSet 逐用户返点比例「未覆盖」哨兵值：表示跟随全局配置。
// 与 0 严格区分 —— 0 是「明确设为 0%（不分成）」。
const RatioNotSet = -1.0

// SetUserRatios 设置某用户的返点比例覆盖（v0.17.7）。
// 传 nil 表示该字段不动；传 RatioNotSet 表示清除覆盖（回到跟随全局）。
// 取值落在 [0,1]，越界返回错误（不静默钳制 —— 让调用方立刻发现口径错误）。
func (s *Store) SetUserRatios(id int64, host, referral *float64) error {
	if host == nil && referral == nil {
		return errors.New("no ratio provided")
	}
	sets := make([]string, 0, 2)
	args := make([]any, 0, 3)
	for _, f := range []struct {
		col string
		v   *float64
	}{{"host_ratio", host}, {"referral_ratio", referral}} {
		if f.v == nil {
			continue
		}
		v := *f.v
		if v != RatioNotSet && (v < 0 || v > 1) {
			return errors.New(f.col + " must be in [0,1] or -1 to clear")
		}
		sets = append(sets, f.col+" = ?")
		args = append(args, v)
	}
	sets = append(sets, "updated_at = ?")
	args = append(args, time.Now().Unix(), id)
	res, err := s.db.Exec(`UPDATE users SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CountReferred 统计某用户名下直接下级数。
func (s *Store) CountReferred(referrerID int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE referred_by = ?`, referrerID).Scan(&n)
	return n, err
}

// ListUsers 全部用户（不含密码哈希的展示由上层脱敏）。
func (s *Store) ListUsers() ([]*User, error) {
	rows, err := s.db.Query(`SELECT ` + userCols + ` FROM users ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetUserStatus 封禁 / 解封。
func (s *Store) SetUserStatus(id int64, status string) error {
	_, err := s.db.Exec(`UPDATE users SET status = ?, updated_at = ? WHERE id = ?`,
		status, time.Now().Unix(), id)
	return err
}

// SetUserRole 调整角色。
func (s *Store) SetUserRole(id int64, role string) error {
	_, err := s.db.Exec(`UPDATE users SET role = ?, updated_at = ? WHERE id = ?`,
		role, time.Now().Unix(), id)
	return err
}

// SetUserPassword 更新密码哈希（bcrypt）。
// 出于安全，这里**不做任何日志**：连「用户 X 改了密码」也不写明文相关字段。
func (s *Store) SetUserPassword(id int64, hash string) error {
	res, err := s.db.Exec(`UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`,
		hash, time.Now().Unix(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// AddUserBalance 增减余额（deltaMicro 可为负）。余额不允许被手动改到负数以下
// （计费扣减走 ChargeUser，那条路径允许欠费）。
//
// v0.21.0：正数计入**累计充值点**（管理员手动调整属充值性质，不可提现）；
// 负数同时把可提现额度钳到不超过扣减后的余额（与 ChargeUser 同一不变量）。
func (s *Store) AddUserBalance(id int64, deltaMicro int64) error {
	var res sql.Result
	var err error
	if deltaMicro >= 0 {
		res, err = s.db.Exec(
			`UPDATE users SET balance_micro = balance_micro + ?,
				recharge_micro = recharge_micro + ?, updated_at = ? WHERE id = ?`,
			deltaMicro, deltaMicro, time.Now().Unix(), id)
	} else {
		res, err = s.db.Exec(
			`UPDATE users SET balance_micro = balance_micro + ?, `+clampWithdrawableSQL+`, updated_at = ?
			 WHERE id = ?`,
			deltaMicro, -deltaMicro, time.Now().Unix(), id)
	}
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// 余额分桶与可提现额度（v0.21.0）
// ---------------------------------------------------------------------------
//
// 需求：体验点与充值点**都不可提现**，**仅**返点收益与账号买卖金额可提现。
//
// ## 为什么不做「三桶各自记余额」
//
// 三桶独立记账需要一条完整的账本（每次扣减记哪个桶、退款退回哪个桶），而退款
// 路径（预扣退还 / 结算多退少补）跨进程与跨重启，一旦退回错桶就会**凭空制造
// 可提现额度**（体验点变可提现 = 直接漏钱）。收益与复杂度不成比例。
//
// ## 采用的模型：可提现额度独立记账 + 扣减时钳制
//
//	withdrawable_micro  可提现额度（真实额度，只有返点 / 卖号入账增加）
//	exp_micro           累计体验点（**累计统计**，不参与额度计算）
//	recharge_micro      累计充值点（**累计统计**，不参与额度计算）
//	balance_micro       总可用余额（唯一可消费来源，全部既有计费逻辑不变）
//
// 关键不变量：**withdrawable_micro + frozen_micro ≤ balance_micro**
//（可提现额度与冻结额之和不得超过总余额）。
//
// 每次**扣减余额**时把额度钳到「扣减后的余额」：MIN(withdrawable, MAX(balance-delta,0))。
// 每次**退回冻结额**时同样钳制：MIN(withdrawable+amt, MAX(balance,0))。
//
//	可提现额度 = min(withdrawable_micro, balance_micro)
//
// ⚠️ v0.27.0 起**不再**在查询时减「待审申请额」：申请提交那一刻就把它从
// withdrawable_micro 转进 frozen_micro（见 CreateWithdrawal）。「减去待审额」是
// 派生算法，不占住任何东西，用户在审核期间一消费就会让审核必然失败
//（这正是用户报的「审核操作无法执行」）。
//
// 为什么钳制就够（不用记扣了哪个桶）：体验点与充值点**同为不可提现**，二者之间的
// 花费顺序对「能提多少」毫无影响 —— 只有「余额被打到低于可提现额度」时，超出的
// 那部分额度才应当作已被消费掉。钳制正好表达这件事：
//
//	余额 1000（含可提现 100）→ 消费 950 → 余额 50，额度钳到 50（正确：吃掉了 50 额度）
//	→ 之后充值 500 → 余额 550，额度仍 50（正确：被吃掉的那 50 不会复活）
//
// 若不钳制，额度会随余额回升「复活」，用户可反复套现 —— 这正是必须钳制的原因。

// CreditWithdrawable 入账**可提现额度**：返点收益（号池收益 + 推广返点）与
// 账号买卖所得走这里。总余额与可提现额度同步增加。
func (s *Store) CreditWithdrawable(id int64, micro int64) error {
	if micro <= 0 {
		return nil
	}
	res, err := s.db.Exec(
		`UPDATE users SET balance_micro = balance_micro + ?,
			withdrawable_micro = withdrawable_micro + ?, updated_at = ?
		 WHERE id = ?`,
		micro, micro, time.Now().Unix(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CreditRedeem 兑换码入账：总余额增加，并按类型累计（**不计入可提现额度** ——
// 两类兑换码都不可提现）。kind："exp" 记累计体验点，其余记累计充值点。
func (s *Store) CreditRedeem(id int64, micro int64, kind string) error {
	if micro <= 0 {
		return nil
	}
	col := "recharge_micro"
	if normalizeRedeemKind(kind) == RedeemKindExp {
		col = "exp_micro"
	}
	res, err := s.db.Exec(
		`UPDATE users SET balance_micro = balance_micro + ?, `+col+` = `+col+` + ?, updated_at = ?
		 WHERE id = ?`,
		micro, micro, time.Now().Unix(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// clampWithdrawableSQL 供扣减语句复用的钳制表达式：
// 把可提现额度压到不超过「扣减后的余额」，且不为负。
const clampWithdrawableSQL = `withdrawable_micro = MIN(withdrawable_micro, MAX(balance_micro - ?, 0))`

// ChargeUser 计费扣款：允许扣成负数（欠费），用于已在途的请求结算。
// 同时把可提现额度钳到不超过扣减后的余额（见上方不变量说明）。
// 返回扣减后的余额。
func (s *Store) ChargeUser(id int64, micro int64) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`UPDATE users SET balance_micro = balance_micro - ?, `+clampWithdrawableSQL+`, updated_at = ?
		 WHERE id = ?`,
		micro, micro, time.Now().Unix(), id); err != nil {
		return 0, err
	}
	var bal int64
	if err := tx.QueryRow(`SELECT balance_micro FROM users WHERE id = ?`, id).Scan(&bal); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return bal, nil
}

// WithdrawableQuota 返回用户**当前可提现额度**与其原值。
//
// v0.27.0 起两个返回值通常相等：待审申请在提交时就已经被扣进 frozen_micro，
// 不再需要用「减去待审额」的方式在查询时派生（那个派生值不占住任何东西，
// 用户一消费就让审核必然失败）。保留两个返回值是为了兼容既有调用与展示。
//
// 额度 = min(withdrawable_micro, balance_micro)：上限钳到余额是防御历史脏数据
// 或外部直改库；负值归零。
func (s *Store) WithdrawableQuota(userID int64) (int64, int64, error) {
	q, r, _, err := s.WithdrawableState(userID)
	return q, r, err
}

// WithdrawableState 返回（可提现额度, 额度原值, 已冻结额）。三个值一次读回，
// 避免调用方为拿冻结额再查一次（多一次查询就多一个"两次读到不一致"的窗口）。
func (s *Store) WithdrawableState(userID int64) (quotaMicro, rawMicro, frozenMicro int64, err error) {
	var bal, wd, fr int64
	if err := s.db.QueryRow(
		`SELECT balance_micro, withdrawable_micro, frozen_micro FROM users WHERE id = ?`, userID).
		Scan(&bal, &wd, &fr); err != nil {
		if err == sql.ErrNoRows {
			return 0, 0, 0, ErrNotFound
		}
		return 0, 0, 0, err
	}
	rawMicro = wd
	if rawMicro < 0 {
		rawMicro = 0
	}
	quotaMicro = rawMicro
	if quotaMicro > bal {
		quotaMicro = bal // 防御：历史脏数据/外部直改库导致的越界
	}
	if quotaMicro < 0 {
		quotaMicro = 0
	}
	return quotaMicro, rawMicro, fr, nil
}

// kvWithdrawFreezeMigrated 提现冻结回填的一次性标记（v0.27.0）。
const kvWithdrawFreezeMigrated = "migrate_withdraw_freeze_v1"

// backfillWithdrawFreeze 把**在途的待审申请**补冻（v0.27.0 一次性迁移）。
//
// 为什么必须回填：改冻结前，待审提现只是"查询时减去"，钱仍留在 withdrawable_micro
// 里。不补冻的话，那些老 pending 记录对应的额度**可以被再申请一次** —— 同一笔钱
// 提两次。而当管理员审核老记录时，`frozen_micro >= amount` 又会不成立，直接报错。
// 两头都坏，所以必须在升级当刻把存量对齐。
//
// 为什么用 kv 标记而不是"每次启动按盘面自愈"：金额类迁移必须**可预期、可审计**。
// 每次启动重算会让运维无法判断"这笔钱到底被动过没有"，而这类判断一旦出错就是钱。
// 迁移只跑一次，跑过的结论写死在 kv 里；日志留一行"补冻了 N 笔"作为凭据。
//
// 额度不足时**只划到 0**（MAX(…,0)）而不是失败：那说明该额度在等待期间已被消费
// 掉（旧口径下这是允许的），差额会以日志告警暴露，由人工决定是否驳回该申请。
func (s *Store) backfillWithdrawFreeze() error {
	if v, err := s.GetKV(kvWithdrawFreezeMigrated); err == nil && v == "1" {
		return nil
	}
	rows, err := s.db.Query(
		`SELECT user_id, COALESCE(SUM(amount_micro),0) FROM withdrawals
		 WHERE status = ? GROUP BY user_id`, WithdrawPending)
	if err != nil {
		return err
	}
	type pend struct {
		uid int64
		amt int64
	}
	var list []pend
	for rows.Next() {
		var x pend
		if err := rows.Scan(&x.uid, &x.amt); err != nil {
			rows.Close()
			return err
		}
		if x.amt > 0 {
			list = append(list, x)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	var done, short int
	for _, x := range list {
		// 先看够不够，够不着就记一笔告警（不静默）。
		var wd, bal int64
		if err := tx.QueryRow(`SELECT withdrawable_micro, balance_micro FROM users WHERE id = ?`, x.uid).
			Scan(&wd, &bal); err != nil {
			if err == sql.ErrNoRows {
				continue // 用户已删，跳过
			}
			return err
		}
		if wd < x.amt {
			short++
			log.Printf("WARN: [withdraw] 存量补冻额度不足：user=%d 待审 %d 微点，可提现额度仅 %d 微点"+
				"（该额度可能已在旧口径下被消费；请人工复核该申请是否需要驳回）", x.uid, x.amt, wd)
		}
		if _, err := tx.Exec(`UPDATE users SET withdrawable_micro = MAX(withdrawable_micro - ?, 0),
			frozen_micro = frozen_micro + ?, updated_at = ? WHERE id = ?`,
			x.amt, x.amt, now, x.uid); err != nil {
			return err
		}
		done++
	}
	if _, err := tx.Exec(`INSERT INTO kv (k, v) VALUES (?, '1')
		ON CONFLICT(k) DO UPDATE SET v = '1'`, kvWithdrawFreezeMigrated); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if done > 0 {
		log.Printf("INFO: [withdraw] 已把 %d 笔在途待审提现补冻（共 %d 笔额度不足，见上方 WARN）"+
			"—— 这些金额已从可提现额度转入冻结额，审核时不再可能因余额变动而失败", done, short)
	}
	return nil
}

// backfillBalanceBuckets 一次性回填可提现额度（v0.21.0）。
//
// 老库只有 balance_micro，没有分桶概念。若把历史余额全部当成「可提现」，等于
// 凭空给所有用户发提现额度；若全当成「不可提现」，则老用户**已经赚到的返点与
// 卖号款会被吞掉**。故按历史流水**真实还原**：
//
//	可提现 = min( 推广返点合计 + 号池收益合计 + 卖号所得合计 , 当前余额 )
//
// 上限取余额是因为用户可能已经把这些钱花掉了（花掉的不该再可提现）。
//
// 累计充值点用「余额 − 可提现」估算（历史余额几乎全部来自兑换码与管理员调整，
// 都是不可提现的充值性质）；累计体验点无法追溯，保持 0。
//
// 幂等：以 kv 标记 balance_buckets_migrated 保证只跑一次（否则会重复累加）。
func (s *Store) backfillBalanceBuckets() error {
	if v, _ := s.GetKV("balance_buckets_migrated"); v == "1" {
		return nil
	}
	rows, err := s.db.Query(`
		SELECT u.id, u.balance_micro,
			COALESCE((SELECT SUM(rebate_micro) FROM rebates WHERE beneficiary = u.id), 0),
			COALESCE((SELECT SUM(earned_micro) FROM host_earnings WHERE owner_user_id = u.id), 0),
			COALESCE((SELECT SUM(seller_micro) FROM account_purchases WHERE seller_user_id = u.id), 0)
		FROM users u`)
	if err != nil {
		return fmt.Errorf("backfill buckets query: %w", err)
	}
	type row struct{ id, bal, wd int64 }
	var all []row
	for rows.Next() {
		var id, bal, reb, earn, sold int64
		if err := rows.Scan(&id, &bal, &reb, &earn, &sold); err != nil {
			rows.Close()
			return fmt.Errorf("backfill buckets scan: %w", err)
		}
		wd := reb + earn + sold
		if wd > bal {
			wd = bal
		}
		if wd < 0 {
			wd = 0
		}
		all = append(all, row{id, bal, wd})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range all {
		recharge := r.bal - r.wd
		if recharge < 0 {
			recharge = 0
		}
		if _, err := s.db.Exec(
			`UPDATE users SET withdrawable_micro = ?, recharge_micro = ?, updated_at = ? WHERE id = ?`,
			r.wd, recharge, time.Now().Unix(), r.id); err != nil {
			return fmt.Errorf("backfill buckets update %d: %w", r.id, err)
		}
	}
	if err := s.SetKV("balance_buckets_migrated", "1"); err != nil {
		return err
	}
	log.Printf("余额分桶回填完成：%d 个用户（可提现额度按历史返点/收益/卖号流水还原）", len(all))
	return nil
}

// SetUserWeChat 更新用户微信号（空串 = 清除）。
func (s *Store) SetUserWeChat(id int64, wechat string) error {
	res, err := s.db.Exec(`UPDATE users SET wechat = ?, updated_at = ? WHERE id = ?`,
		strings.TrimSpace(wechat), time.Now().Unix(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------- 预扣 / 结算 / 消费闸 ----------

// ReserveCharge 预扣：仅当余额充足时原子扣减并登记一笔在途预扣。
//
// 这是「先扣后算」的第一步。为什么必须预扣：流式响应时长不可控，
// 若等拿到 usage 才扣费，用户余额 10 点也能发起一次实际消耗 500 点的请求
// （max_tokens 给到上限），结算后直接欠费 490 —— 刷爆。
// 预扣按 max_tokens 上界冻结，钱不够就当场拒（402），从源头堵住。
func (s *Store) ReserveCharge(userID, keyID int64, amountMicro int64, model string) (*Reservation, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(
		`UPDATE users SET balance_micro = balance_micro - ?, `+clampWithdrawableSQL+`, updated_at = ?
		 WHERE id = ? AND balance_micro >= ?`,
		amountMicro, amountMicro, time.Now().Unix(), userID, amountMicro)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// 0 行受影响有两种可能：用户不存在，或余额不足。要区分开，
		// 否则会拿「余额不足」去回应一个已删除的用户。
		var exists int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM users WHERE id = ?`, userID).Scan(&exists); err != nil {
			return nil, err
		}
		if exists == 0 {
			return nil, ErrNotFound
		}
		return nil, ErrInsufficientBalance
	}
	now := time.Now().Unix()
	ins, err := tx.Exec(
		`INSERT INTO reservations (user_id, key_id, amount_micro, model, created_at)
		 VALUES (?,?,?,?,?)`,
		userID, keyID, amountMicro, model, now)
	if err != nil {
		return nil, err
	}
	id, _ := ins.LastInsertId()
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Reservation{
		ID: id, UserID: userID, KeyID: keyID,
		AmountMicro: amountMicro, Model: model, CreatedAt: now,
	}, nil
}

// SettleReservation 结算：退还预扣、扣实际，净额 = 预扣 - 实际。
//
// 实际超过预扣时净额为负，继续从余额扣 —— 允许欠费是为了不中断已经吐完的
// 流式响应（钱可以事后补，字吐错了没得补）。下一笔新请求会被 402 挡住。
//
// 记录不存在返回 ErrNotFound（说明已被结算或释放，调用方应忽略）。
func (s *Store) SettleReservation(id int64, actualMicro int64) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var userID, amount int64
	err = tx.QueryRow(`SELECT user_id, amount_micro FROM reservations WHERE id = ?`, id).
		Scan(&userID, &amount)
	if err == sql.ErrNoRows {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`DELETE FROM reservations WHERE id = ?`, id); err != nil {
		return 0, err
	}
	net := amount - actualMicro
	if _, err := tx.Exec(
		`UPDATE users SET balance_micro = balance_micro + ?, updated_at = ? WHERE id = ?`,
		net, time.Now().Unix(), userID); err != nil {
		return 0, err
	}
	var bal int64
	if err := tx.QueryRow(`SELECT balance_micro FROM users WHERE id = ?`, userID).Scan(&bal); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return bal, nil
}

// ReleaseReservation 全额退还一笔预扣（请求失败 / 无可用账号 / 崩溃对账）。
func (s *Store) ReleaseReservation(id int64) (int64, error) {
	return s.SettleReservation(id, 0)
}

// ReconcileReservations 启动对账：退还全部残留在表里的预扣。
//
// 能留下的只有「上次进程在途时被强杀」的记录，正常路径结算时已 delete。
// 退还方向对用户有利，且不会重复计费（结算先 delete 再改余额，天然幂等）。
func (s *Store) ReconcileReservations() (count int, refundedMicro int64, err error) {
	rows, err := s.db.Query(`SELECT id, amount_micro FROM reservations ORDER BY id ASC`)
	if err != nil {
		return 0, 0, err
	}
	type item struct{ id, amount int64 }
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.amount); err != nil {
			rows.Close()
			return 0, 0, err
		}
		items = append(items, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	for _, it := range items {
		if _, err := s.ReleaseReservation(it.id); err != nil {
			return count, refundedMicro, err
		}
		count++
		refundedMicro += it.amount
	}
	return count, refundedMicro, nil
}

// UserUsedSince 某时刻以来该用户的实际消费（微点），用于消费闸判定。
// 只统计正数流水：失败请求与零费请求不计入。
func (s *Store) UserUsedSince(userID int64, since int64) (int64, error) {
	var total sql.NullInt64
	err := s.db.QueryRow(
		`SELECT SUM(charged_micro) FROM usage_logs
		 WHERE user_id = ? AND created_at >= ? AND charged_micro > 0`,
		userID, since).Scan(&total)
	if err != nil {
		return 0, err
	}
	return total.Int64, nil
}

// SetUserLimits 设置用户消费闸（微点，0 = 不限）。
func (s *Store) SetUserLimits(userID int64, dailyMicro, monthlyMicro int64) error {
	res, err := s.db.Exec(
		`UPDATE users SET daily_limit_micro = ?, monthly_limit_micro = ?, updated_at = ? WHERE id = ?`,
		dailyMicro, monthlyMicro, time.Now().Unix(), userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------- API Key ----------

// CreateAPIKey 落库一把 key（hash + 密文 + 前缀）。
//
// kind："" = 平台计费 Key（走兑换余额结算）；KeyKindHost = 代挂自用 Key
// （只用本人在池/代挂账号，不扣余额）。hostGroup 仅代挂 Key 有意义：
// 限定用哪个代挂分组的账号，空串 = 全部本人代挂账号。
func (s *Store) CreateAPIKey(userID int64, name, keyHash, keyEnc, prefix, kind, hostGroup, accountUIDs string) (*APIKey, error) {
	now := time.Now().Unix()
	res, err := s.db.Exec(
		`INSERT INTO api_keys (user_id, name, key_hash, key_enc, key_prefix, status, kind, host_group, account_uids, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		userID, name, keyHash, keyEnc, prefix, StatusActive, kind, hostGroup, accountUIDs, now,
	)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &APIKey{ID: id, UserID: userID, Name: name, KeyHash: keyHash,
		KeyEnc: keyEnc, KeyPrefix: prefix, Status: StatusActive,
		Kind: kind, HostGroup: hostGroup, AccountUIDs: accountUIDs, CreatedAt: now}, nil
}

const keyCols = `id, user_id, name, key_hash, key_enc, key_prefix, status, last_used_at, kind, host_group, account_uids, created_at`

func scanAPIKey(row interface{ Scan(dest ...any) error }) (*APIKey, error) {
	var k APIKey
	err := row.Scan(&k.ID, &k.UserID, &k.Name, &k.KeyHash, &k.KeyEnc,
		&k.KeyPrefix, &k.Status, &k.LastUsedAt, &k.Kind, &k.HostGroup, &k.AccountUIDs, &k.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &k, nil
}

// GetAPIKeyByHash 按 hash 查 key（鉴权主路径）。
func (s *Store) GetAPIKeyByHash(hash string) (*APIKey, error) {
	k, err := scanAPIKey(s.db.QueryRow(`SELECT `+keyCols+` FROM api_keys WHERE key_hash = ?`, hash))
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return k, err
}

// GetAPIKey 按 (userID, id) 取单把 key（含 key_enc，供「显式查看」解密后一次性回显）。
// 归属校验放在 SQL 条件里：跨用户查别人的 key 直接 ErrNotFound，不给存在性信息。
func (s *Store) GetAPIKey(userID, id int64) (*APIKey, error) {
	k, err := scanAPIKey(s.db.QueryRow(
		`SELECT `+keyCols+` FROM api_keys WHERE id = ? AND user_id = ?`, id, userID))
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return k, err
}

// ListAPIKeys 某用户的全部 key。
func (s *Store) ListAPIKeys(userID int64) ([]*APIKey, error) {
	rows, err := s.db.Query(`SELECT `+keyCols+` FROM api_keys WHERE user_id = ? ORDER BY id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// SetAPIKeyStatus 启用 / 禁用某 key。
func (s *Store) SetAPIKeyStatus(userID, id int64, status string) error {
	res, err := s.db.Exec(`UPDATE api_keys SET status = ? WHERE id = ? AND user_id = ?`,
		status, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetAPIKeyAccounts 更新某代挂自用 Key 绑定的账号集合（逗号分隔 uid）。
func (s *Store) SetAPIKeyAccounts(userID, id int64, accountUIDs string) error {
	res, err := s.db.Exec(`UPDATE api_keys SET account_uids = ? WHERE id = ? AND user_id = ?`,
		strings.TrimSpace(accountUIDs), id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteAPIKey 删除某 key。
func (s *Store) DeleteAPIKey(userID, id int64) error {
	res, err := s.db.Exec(`DELETE FROM api_keys WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchAPIKey 记录 key 最近使用时间（失败不致命，忽略错误）。
func (s *Store) TouchAPIKey(id int64) {
	_, _ = s.db.Exec(`UPDATE api_keys SET last_used_at = ? WHERE id = ?`, time.Now().Unix(), id)
}

// ---------- 用量流水 ----------

// InsertUsageLog 写一笔流水。
func (s *Store) InsertUsageLog(l *UsageLog) error {
	if l.CreatedAt == 0 {
		l.CreatedAt = time.Now().Unix()
	}
	if l.Scope == "" {
		l.Scope = ScopePool
	}
	_, err := s.db.Exec(
		`INSERT INTO usage_logs (user_id, key_id, model, prompt_tokens, completion_tokens,
		 upstream_credit, charged_micro, upstream_uid, scope, status, error_msg, created_at,
		 sticky_key_hash, attempts, cache_hit_tokens, cache_miss_tokens, cache_write_tokens, latency_ms)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		l.UserID, l.KeyID, l.Model, l.PromptTokens, l.CompletionTokens,
		l.UpstreamCredit, l.ChargedMicro, l.UpstreamUID, l.Scope, l.Status, l.ErrorMsg, l.CreatedAt,
		l.StickyKeyHash, l.Attempts,
		l.CacheHitTokens, l.CacheMissTokens, l.CacheWriteTokens, l.LatencyMS,
	)
	return err
}

// ListUsageLogs 某用户最近流水（分页）。
func (s *Store) ListUsageLogs(userID int64, limit, offset int) ([]*UsageLog, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.Query(
		`SELECT id, user_id, key_id, model, prompt_tokens, completion_tokens,
		        upstream_credit, charged_micro, upstream_uid, scope, status, error_msg, created_at,
		        sticky_key_hash, attempts,
		        cache_hit_tokens, cache_miss_tokens, cache_write_tokens, latency_ms
		 FROM usage_logs WHERE user_id = ? ORDER BY id DESC LIMIT ? OFFSET ?`,
		userID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*UsageLog
	for rows.Next() {
		var l UsageLog
		if err := rows.Scan(&l.ID, &l.UserID, &l.KeyID, &l.Model, &l.PromptTokens,
			&l.CompletionTokens, &l.UpstreamCredit, &l.ChargedMicro, &l.UpstreamUID,
			&l.Scope, &l.Status, &l.ErrorMsg, &l.CreatedAt,
			&l.StickyKeyHash, &l.Attempts,
			&l.CacheHitTokens, &l.CacheMissTokens, &l.CacheWriteTokens, &l.LatencyMS); err != nil {
			return nil, err
		}
		out = append(out, &l)
	}
	return out, rows.Err()
}

// UsageLogsBySticky 按会话粘性键哈希取最近流水（排查「同一场对话是否换号」）。
// 空 hash 返回空集（不查「无会话标识」的历史行——那些本来就不该被当成同一场对话）。
// 供离线排查用：一个 sticky_key_hash 下若出现多个 upstream_uid，即为换号证据。
func (s *Store) UsageLogsBySticky(hash string, limit int) ([]*UsageLog, error) {
	if strings.TrimSpace(hash) == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(
		`SELECT id, user_id, key_id, model, prompt_tokens, completion_tokens,
		        upstream_credit, charged_micro, upstream_uid, scope, status, error_msg, created_at,
		        sticky_key_hash, attempts,
		        cache_hit_tokens, cache_miss_tokens, cache_write_tokens, latency_ms
		 FROM usage_logs WHERE sticky_key_hash = ? ORDER BY id DESC LIMIT ?`,
		hash, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*UsageLog
	for rows.Next() {
		var l UsageLog
		if err := rows.Scan(&l.ID, &l.UserID, &l.KeyID, &l.Model, &l.PromptTokens,
			&l.CompletionTokens, &l.UpstreamCredit, &l.ChargedMicro, &l.UpstreamUID,
			&l.Scope, &l.Status, &l.ErrorMsg, &l.CreatedAt,
			&l.StickyKeyHash, &l.Attempts,
			&l.CacheHitTokens, &l.CacheMissTokens, &l.CacheWriteTokens, &l.LatencyMS); err != nil {
			return nil, err
		}
		out = append(out, &l)
	}
	return out, rows.Err()
}

// UserUsageSummary 用户累计用量（请求数 / token / 已扣微点）。
func (s *Store) UserUsageSummary(userID int64) (reqs int64, tokens int64, chargedMicro int64, err error) {
	err = s.db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(prompt_tokens + completion_tokens),0), COALESCE(SUM(charged_micro),0)
		 FROM usage_logs WHERE user_id = ?`, userID).Scan(&reqs, &tokens, &chargedMicro)
	return
}

// AccountUsageStat 某账号作为 serving 账号（即真正承接请求的号池账号）的累计用量。
// v0.12.0（需求 6/7）：让管理后台能看到「每个号池账号被消耗了多少积分」，
// 与计费口径（扣费 = 号池积分消耗 × 倍率）一一对应。
type AccountUsageStat struct {
	Requests int64
	Tokens   int64
	Credit   float64 // 该账号被实扣的号池积分合计
	Charged  int64   // 平台据此向调用方收取的点数合计（微点）
}

// UsageByAccount 按 serving 账号（usage_logs.upstream_uid）聚合累计用量。
func (s *Store) UsageByAccount() (map[string]AccountUsageStat, error) {
	rows, err := s.db.Query(`SELECT upstream_uid,
		COUNT(*), COALESCE(SUM(prompt_tokens + completion_tokens),0),
		COALESCE(SUM(upstream_credit),0), COALESCE(SUM(charged_micro),0)
		FROM usage_logs WHERE upstream_uid <> '' GROUP BY upstream_uid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]AccountUsageStat{}
	for rows.Next() {
		var uid string
		var st AccountUsageStat
		if err := rows.Scan(&uid, &st.Requests, &st.Tokens, &st.Credit, &st.Charged); err != nil {
			return nil, err
		}
		out[uid] = st
	}
	return out, rows.Err()
}

// ---------- 用量报表 ----------

// UsageUserStat 单用户消耗聚合（消耗排行用）。
type UsageUserStat struct {
	UserID         int64
	Username       string
	Requests       int64
	Tokens         int64
	ChargedMicro   int64
	UpstreamCredit float64
}

// UsageByUser 按用户聚合窗口内消耗（v0.17.8，消耗排行）。
//
// 口径：
//   - 统计范围：usage_logs 中 created_at >= 窗口起点 的记录，与报表其余部分同口径
//     （同一天界算法、同样按 scope 不限）；
//   - **刻意排除 user_id = 0**：那是管理密钥通道（v0.17.6 起也写台账，费用恒为 0）。
//     把它算进来会凭空多出一行「0 号用户」，还会把 total_users 抬高一格；
//   - 排序依据：**消耗点数（charged_micro）降序** → 请求数降序 → user_id 升序。
//     末两级是为了让同分用户的次序稳定（否则每次刷新顺序抖动，看着像在跳）；
//   - limit：>0 时截断（接口层已钳到 ≤50）。
func (s *Store) UsageByUser(userID int64, days, limit int) ([]UsageUserStat, error) {
	if days <= 0 || days > 365 {
		days = 14
	}
	since := localDayStart(days)
	q := `SELECT l.user_id, COALESCE(u.username,''), COUNT(*),
	             COALESCE(SUM(l.prompt_tokens + l.completion_tokens),0),
	             COALESCE(SUM(l.charged_micro),0), COALESCE(SUM(l.upstream_credit),0)
	      FROM usage_logs l LEFT JOIN users u ON u.id = l.user_id
	      WHERE l.created_at >= ? AND l.user_id > 0`
	args := []any{since}
	if userID > 0 {
		q += ` AND l.user_id = ?`
		args = append(args, userID)
	}
	q += ` GROUP BY l.user_id ORDER BY COALESCE(SUM(l.charged_micro),0) DESC,
	       COUNT(*) DESC, l.user_id ASC`
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageUserStat
	for rows.Next() {
		var st UsageUserStat
		if err := rows.Scan(&st.UserID, &st.Username, &st.Requests, &st.Tokens,
			&st.ChargedMicro, &st.UpstreamCredit); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// CountUsageUsers 窗口内有消耗记录的用户数（排行分母）。
func (s *Store) CountUsageUsers(days int) (int, error) {
	if days <= 0 || days > 365 {
		days = 14
	}
	since := localDayStart(days)
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(DISTINCT user_id) FROM usage_logs WHERE created_at >= ? AND user_id > 0`,
		since).Scan(&n)
	return n, err
}

// UsageDailyStat 单日聚合。
type UsageDailyStat struct {
	Day            string
	Requests       int64
	Tokens         int64
	ChargedMicro   int64
	UpstreamCredit float64
}

// UsageModelStat 单模型聚合。
type UsageModelStat struct {
	Model          string
	Requests       int64
	Tokens         int64
	ChargedMicro   int64
	UpstreamCredit float64
}

// localDayOffset 本地时区偏移（秒，东为正）。
// 用它把 UTC 秒换算成「本地自然日」序号，避免依赖 SQLite 的 localtime 修饰符
// （modernc 驱动是否带 tzdata 取决于构建，不赌）。
func localDayOffset() int64 {
	_, off := time.Now().Zone()
	return int64(off)
}

// localDayStart 返回近 days 天窗口的**最早本地日界**（unix 秒）。
//
// ⚠️ 修复记录（v0.26.7，真实缺陷）：此前 7 处一律写成
//
//	time.Now().AddDate(0, 0, -(days-1)).Truncate(24*time.Hour).Unix() + off
//
// 而 `Truncate(24h)` 是按 **UTC 零点**切的，再 `+off` 得到的是「本地日界 + 2×off」
// —— 东八区下即 **本地今天 16:00**（在未来）。后果很具体：**当天 00:00~16:00
// 产生的流水全部落在窗口之外**，报表在每天前 16 小时里看不到"今天"的数据。
// 实测：本地 15:05 刚落库的两条流水，`days=1` 窗口查不到（perf 样本数为 0）。
//
// 正确做法：用 time.Date 按**本地时区**构造当天零点（与分组口径
// `(created_at+off)/86400` 同源），再往前推 days-1 天。
//
// 抽成单点函数的意义：这个边界是 7 处报表查询的公共前提，散写必然再次漂移
// （本次就是一处写错、复制了 7 份）。
func localDayStart(days int) int64 {
	if days < 1 {
		days = 1
	}
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	return start.AddDate(0, 0, -(days - 1)).Unix()
}

// UsageDaily 按本地自然日聚合近 days 天的用量；userID <= 0 表示全站。
// 返回按日期升序（便于前端直接画折线）。
func (s *Store) UsageDaily(userID int64, days int) ([]UsageDailyStat, error) {
	return s.UsageDailyScoped(userID, days, "")
}

// UsageDailyScoped 同上，但可限定 scope（"" = 全部；ScopePool / ScopeHost 单类）。
// 控制台「用量账单」据此把消费拆成「自用 / 用号池 / 进号池收益」三类。
func (s *Store) UsageDailyScoped(userID int64, days int, scope string) ([]UsageDailyStat, error) {
	if days <= 0 || days > 365 {
		days = 14
	}
	off := localDayOffset()
	since := localDayStart(days)
	// since 是**精确**的本地日界（localDayStart，见其注释）；分组口径
	// (created_at+off)/86400 与它同源，两者不会错位。
	query := `SELECT (created_at + ?) / 86400 AS d,
	                 COUNT(*), COALESCE(SUM(prompt_tokens + completion_tokens),0), COALESCE(SUM(charged_micro),0),
	                 COALESCE(SUM(upstream_credit),0)
	          FROM usage_logs WHERE created_at >= ?`
	args := []any{off, since}
	if userID > 0 {
		query += ` AND user_id = ?`
		args = append(args, userID)
	}
	if scope != "" {
		query += ` AND scope = ?`
		args = append(args, scope)
	}
	query += ` GROUP BY d ORDER BY d ASC`
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageDailyStat
	for rows.Next() {
		var dayIdx int64
		var st UsageDailyStat
		if err := rows.Scan(&dayIdx, &st.Requests, &st.Tokens, &st.ChargedMicro, &st.UpstreamCredit); err != nil {
			return nil, err
		}
		st.Day = time.Unix(dayIdx*86400-off, 0).Format("2006-01-02")
		out = append(out, st)
	}
	return out, rows.Err()
}

// UsageByModel 按模型聚合近 days 天用量（按已扣微点降序）；userID <= 0 表示全站。
func (s *Store) UsageByModel(userID int64, days int) ([]UsageModelStat, error) {
	return s.UsageByModelScoped(userID, days, "")
}

// UsagePerf 性能与缓存观测汇总（v0.26.7）。
//
// 为什么单开一个结构：这是回答"为什么又慢又贵"的两个直接指标 ——
// **端到端耗时**（用户感知）与**前缀缓存命中率**（决定预处理成本）。
// 此前前者只活在 90 分钟的内存态实时面板里，后者**根本没采**，
// 于是"延迟高"只能靠肉眼看单个事件、无法归因。
type UsagePerf struct {
	// 耗时（仅统计有观测的行：latency_ms > 0）
	LatencySampled int64   `json:"latency_sampled"`
	AvgLatencyMS   float64 `json:"avg_latency_ms"`
	P50LatencyMS   int64   `json:"p50_latency_ms"`
	P95LatencyMS   int64   `json:"p95_latency_ms"`
	MaxLatencyMS   int64   `json:"max_latency_ms"`

	// 前缀缓存（仅统计上游**明确回报**过的行：hit+miss > 0）
	CacheSampled     int64   `json:"cache_sampled"`
	CacheHitTokens   int64   `json:"cache_hit_tokens"`
	CacheMissTokens  int64   `json:"cache_miss_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	CacheHitRate     float64 `json:"cache_hit_rate"`
}

// UsagePerf 汇总近 days 天的耗时与缓存指标；userID<=0 表示全站。
//
// 口径说明（重要，别把"没采到"当成"是 0"）：
//   - 历史行（v0.26.7 之前）两个指标都是 0，**计入 sampled 会把平均值拉低**。
//     所以分母只算"有观测"的行，并把 sampled 一并返回，让调用方知道样本量。
//   - 命中率用 token 加权（hit / (hit+miss)），不是按请求数 —— 缓存省的是
//     token 预处理成本，按请求数算会让一个 48 万 token 的请求和一次 100 token
//     的探活等权，完全没有意义。
func (s *Store) UsagePerf(userID int64, days int) (UsagePerf, error) {
	var p UsagePerf
	if days <= 0 || days > 365 {
		days = 14
	}
	since := localDayStart(days)

	where := ` WHERE created_at >= ?`
	args := []any{since}
	if userID > 0 {
		where += ` AND user_id = ?`
		args = append(args, userID)
	}

	// 1) 耗时聚合
	var avg sql.NullFloat64
	var maxMS sql.NullInt64
	if err := s.db.QueryRow(
		`SELECT COUNT(*), AVG(latency_ms), MAX(latency_ms) FROM usage_logs`+
			where+` AND latency_ms > 0`, args...).
		Scan(&p.LatencySampled, &avg, &maxMS); err != nil {
		return p, err
	}
	if avg.Valid {
		p.AvgLatencyMS = avg.Float64
	}
	p.MaxLatencyMS = maxMS.Int64

	// 2) 分位（行数在报告窗口内可控；直接取有序切片算，避免方言依赖）
	if p.LatencySampled > 0 {
		rows, err := s.db.Query(
			`SELECT latency_ms FROM usage_logs`+where+` AND latency_ms > 0 ORDER BY latency_ms`, args...)
		if err != nil {
			return p, err
		}
		vals := make([]int64, 0, p.LatencySampled)
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				rows.Close()
				return p, err
			}
			vals = append(vals, v)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return p, err
		}
		p.P50LatencyMS = percentile(vals, 0.50)
		p.P95LatencyMS = percentile(vals, 0.95)
	}

	// 3) 缓存聚合（只算上游回报过的行）
	if err := s.db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(cache_hit_tokens),0), COALESCE(SUM(cache_miss_tokens),0),
		        COALESCE(SUM(cache_write_tokens),0)
		 FROM usage_logs`+where+` AND (cache_hit_tokens + cache_miss_tokens) > 0`, args...).
		Scan(&p.CacheSampled, &p.CacheHitTokens, &p.CacheMissTokens, &p.CacheWriteTokens); err != nil {
		return p, err
	}
	if denom := p.CacheHitTokens + p.CacheMissTokens; denom > 0 {
		p.CacheHitRate = float64(p.CacheHitTokens) / float64(denom)
	}
	return p, nil
}

// percentile 取升序切片的分位值（最近秩法）。空切片返回 0。
//
// 用最近秩而不是插值：耗时分布是长尾的，"950ms 与 1200ms 之间插出 1075ms"
// 这种精度对排障没有价值，反而让数字看起来比实际更可信。
func percentile(sorted []int64, q float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)) * q)
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// UsageByModelScoped 同上，可限定 scope。
func (s *Store) UsageByModelScoped(userID int64, days int, scope string) ([]UsageModelStat, error) {
	if days <= 0 || days > 365 {
		days = 14
	}
	since := localDayStart(days)
	query := `SELECT model, COUNT(*), COALESCE(SUM(prompt_tokens + completion_tokens),0), COALESCE(SUM(charged_micro),0),
	                 COALESCE(SUM(upstream_credit),0)
	          FROM usage_logs WHERE created_at >= ?`
	args := []any{since}
	if userID > 0 {
		query += ` AND user_id = ?`
		args = append(args, userID)
	}
	if scope != "" {
		query += ` AND scope = ?`
		args = append(args, scope)
	}
	query += ` GROUP BY model ORDER BY COALESCE(SUM(charged_micro),0) DESC, COUNT(*) DESC`
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageModelStat
	for rows.Next() {
		var st UsageModelStat
		if err := rows.Scan(&st.Model, &st.Requests, &st.Tokens, &st.ChargedMicro, &st.UpstreamCredit); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// ---------- 代挂收益聚合（号池返点） ----------

// HostEarnDailyStat 号池返点按日聚合。
type HostEarnDailyStat struct {
	Day          string
	Requests     int64
	ChargedMicro int64
	EarnedMicro  int64
}

// HostEarnModelStat 号池返点按模型聚合。
type HostEarnModelStat struct {
	Model        string
	Requests     int64
	ChargedMicro int64
	EarnedMicro  int64
}

// HostEarningsDaily 某号主的号池收益按本地自然日聚合近 days 天。
func (s *Store) HostEarningsDaily(ownerUserID int64, days int) ([]HostEarnDailyStat, error) {
	if days <= 0 || days > 365 {
		days = 14
	}
	off := localDayOffset()
	since := localDayStart(days)
	rows, err := s.db.Query(
		`SELECT (created_at + ?) / 86400 AS d, COUNT(*),
		        COALESCE(SUM(charged_micro),0), COALESCE(SUM(earned_micro),0)
		 FROM host_earnings WHERE owner_user_id = ? AND created_at >= ?
		 GROUP BY d ORDER BY d ASC`, off, ownerUserID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HostEarnDailyStat
	for rows.Next() {
		var dayIdx int64
		var st HostEarnDailyStat
		if err := rows.Scan(&dayIdx, &st.Requests, &st.ChargedMicro, &st.EarnedMicro); err != nil {
			return nil, err
		}
		st.Day = time.Unix(dayIdx*86400-off, 0).Format("2006-01-02")
		out = append(out, st)
	}
	return out, rows.Err()
}

// HostEarningsByModel 某号主的号池收益按模型聚合近 days 天（按收益降序）。
func (s *Store) HostEarningsByModel(ownerUserID int64, days int) ([]HostEarnModelStat, error) {
	if days <= 0 || days > 365 {
		days = 14
	}
	since := localDayStart(days)
	rows, err := s.db.Query(
		`SELECT model, COUNT(*), COALESCE(SUM(charged_micro),0), COALESCE(SUM(earned_micro),0)
		 FROM host_earnings WHERE owner_user_id = ? AND created_at >= ?
		 GROUP BY model ORDER BY COALESCE(SUM(earned_micro),0) DESC, COUNT(*) DESC`, ownerUserID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HostEarnModelStat
	for rows.Next() {
		var st HostEarnModelStat
		if err := rows.Scan(&st.Model, &st.Requests, &st.ChargedMicro, &st.EarnedMicro); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// ---------- 返点台账 ----------

// InsertRebate 记一笔返点。
func (s *Store) InsertRebate(r *Rebate) error {
	if r.CreatedAt == 0 {
		r.CreatedAt = time.Now().Unix()
	}
	res, err := s.db.Exec(`INSERT INTO rebates
		(kind, beneficiary, from_user_id, account_uid, base_micro, rebate_micro, note, created_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		r.Kind, r.Beneficiary, r.FromUserID, r.AccountUID, r.BaseMicro, r.RebateMicro, r.Note, r.CreatedAt)
	if err != nil {
		return err
	}
	r.ID, _ = res.LastInsertId()
	return nil
}

// ListRebates 某受益人的返点流水（倒序）。
func (s *Store) ListRebates(beneficiary int64, limit int) ([]*Rebate, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT id, kind, beneficiary, from_user_id, account_uid,
		base_micro, rebate_micro, note, created_at
		FROM rebates WHERE beneficiary = ? ORDER BY created_at DESC, id DESC LIMIT ?`, beneficiary, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Rebate
	for rows.Next() {
		var r Rebate
		if err := rows.Scan(&r.ID, &r.Kind, &r.Beneficiary, &r.FromUserID, &r.AccountUID,
			&r.BaseMicro, &r.RebateMicro, &r.Note, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// RebateSummaryByKind 某受益人各类型返点的累计值（kind → 微点）+ 总计。
func (s *Store) RebateSummaryByKind(beneficiary int64) (map[string]int64, int64, error) {
	rows, err := s.db.Query(`SELECT kind, COALESCE(SUM(rebate_micro),0) FROM rebates WHERE beneficiary = ? GROUP BY kind`, beneficiary)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := map[string]int64{}
	var total int64
	for rows.Next() {
		var kind string
		var sum int64
		if err := rows.Scan(&kind, &sum); err != nil {
			return nil, 0, err
		}
		out[kind] = sum
		total += sum
	}
	return out, total, rows.Err()
}

// ---------- 兑换码 ----------

// CreateRedeemCode 生成一张兑换码。
func (s *Store) CreateRedeemCode(code string, creditsMicro int64, kind string, createdBy int64) error {
	_, err := s.db.Exec(
		`INSERT INTO redeem_codes (code, credits_micro, kind, created_by, created_at) VALUES (?,?,?,?,?)`,
		code, creditsMicro, normalizeRedeemKind(kind), createdBy, time.Now().Unix())
	return err
}

// RedeemCode 兑换：事务内校验未使用 → 标记已用 → 按类型入账。
// 返回 (到账微点, 兑换码类型)。类型供调用方决定是否计推广返点（体验码不计）。
func (s *Store) RedeemCode(code string, userID int64) (int64, string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, "", err
	}
	defer tx.Rollback()

	var credits, usedBy int64
	var kind string
	err = tx.QueryRow(`SELECT credits_micro, used_by, kind FROM redeem_codes WHERE code = ?`, code).
		Scan(&credits, &usedBy, &kind)
	if err == sql.ErrNoRows {
		return 0, "", errors.New("兑换码不存在")
	}
	if err != nil {
		return 0, "", err
	}
	if usedBy != 0 {
		return 0, "", errors.New("兑换码已被使用")
	}
	now := time.Now().Unix()
	// 原子占用：并发同码只允许一个成功（条件 used_by = 0）。
	res, err := tx.Exec(`UPDATE redeem_codes SET used_by = ?, used_at = ? WHERE code = ? AND used_by = 0`,
		userID, now, code)
	if err != nil {
		return 0, "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, "", errors.New("兑换码已被使用")
	}
	// 入账：总余额增加；按类型累计到「体验点 / 充值点」（两者都**不可提现**）。
	col := "recharge_micro"
	if normalizeRedeemKind(kind) == RedeemKindExp {
		col = "exp_micro"
	}
	if _, err := tx.Exec(`UPDATE users SET balance_micro = balance_micro + ?, `+col+` = `+col+` + ?, updated_at = ? WHERE id = ?`,
		credits, credits, now, userID); err != nil {
		return 0, "", err
	}
	if err := tx.Commit(); err != nil {
		return 0, "", err
	}
	return credits, normalizeRedeemKind(kind), nil
}

// ListRedeemCodes 最近生成的兑换码。
func (s *Store) ListRedeemCodes(limit int) ([]*RedeemCode, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(
		`SELECT code, credits_micro, kind, created_by, used_by, used_at, created_at
		 FROM redeem_codes ORDER BY created_at DESC, code ASC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*RedeemCode
	for rows.Next() {
		var c RedeemCode
		if err := rows.Scan(&c.Code, &c.CreditsMicro, &c.Kind, &c.CreatedBy, &c.UsedBy,
			&c.UsedAt, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// ---------- 提现 ----------

// 提现状态。
const (
	WithdrawPending  = "pending"  // 已提交，待管理员审核
	WithdrawApproved = "approved" // 审核通过并已扣余额（打款）
	WithdrawRejected = "rejected" // 已驳回（不扣余额）
)

// Withdrawal 一笔提现申请（记账语义：点为内部单位，非真实货币）。
type Withdrawal struct {
	ID          int64
	UserID      int64
	AmountMicro int64
	FeeMicro    int64
	// Method 收款方式（v0.21.0 起**仅支持支付宝**，恒为 "alipay"）。
	Method string
	// AccountInfo 兼容旧数据：历史行把「账号/姓名」自由文本存在这里。
	// 新写入一律用 AlipayAccount + RealName 两个结构化字段。
	AccountInfo string
	// AlipayAccount 支付宝账号；RealName 姓名（v0.21.0 结构化字段）。
	AlipayAccount string
	RealName      string
	Status        string
	Note          string // 审核备注 / 驳回原因
	ReviewedBy    int64
	ReviewedAt    int64
	CreatedAt     int64
}

// ErrInsufficientWithdrawable 可提现额度不足，无法受理提现申请。
//
// 与 ErrInsufficientWithdraw（审核时失败）刻意分开：两者对用户意味着完全不同的
// 下一步 —— 前者是"再赚一点或改小金额"，后者是"数据异常，请找管理员"。
var ErrInsufficientWithdrawable = errors.New("insufficient withdrawable quota")

// CreateWithdrawal 提交一笔提现申请：**事务内先把金额冻结**，再落申请记录。
//
// ## 为什么必须冻结（v0.27.0 修的是用户报的「审核操作无法执行」）
//
// 旧实现（v0.21.0）只在**查询时**把待审额从可提现额度里减掉 —— 那是派生值，
// 不占住任何东西。于是用户在等审核期间正常消费（或再提一笔）就会把余额打到低于
// 待审额，审核那一刻 `balance < amount` 必然成立 → 管理员点「通过并打款」被拒、
// 只能驳回。用户看到的就是"审核按钮点了没反应/报错"。
//
// 冻结把这件事变成不可能：钱在**提交那一刻**就离开可提现额度并计入 frozen_micro，
// 审核时只可能成功，或由管理员主动驳回（驳回原路退回）。
//
// ## 校验与扣减必须在**同一条 UPDATE** 里
//
// 语句带 `AND withdrawable_micro >= ?`：先查后写之间存在窗口，两笔并发申请会双双
// 读到"够"然后各扣一次（同一笔钱提两次）。合并成一条条件 UPDATE 后，先到者扣完，
// 后到者条件不成立 → RowsAffected==0 → 报额度不足。这就是"防止重复提现/超额提现"
// 的全部机制，不依赖任何应用层锁。
func (s *Store) CreateWithdrawal(w *Withdrawal) error {
	if w.CreatedAt == 0 {
		w.CreatedAt = time.Now().Unix()
	}
	if w.Status == "" {
		w.Status = WithdrawPending
	}
	if w.AmountMicro <= 0 {
		return errors.New("withdrawal amount must be positive")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`UPDATE users SET withdrawable_micro = withdrawable_micro - ?,
		frozen_micro = frozen_micro + ?, updated_at = ?
		WHERE id = ? AND withdrawable_micro >= ?`,
		w.AmountMicro, w.AmountMicro, time.Now().Unix(), w.UserID, w.AmountMicro)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// 区分「用户不存在」与「额度不足」：把两者混成一句会让用户以为是自己填错了。
		var exists int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM users WHERE id = ?`, w.UserID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return ErrNotFound
		}
		return ErrInsufficientWithdrawable
	}

	res2, err := tx.Exec(`INSERT INTO withdrawals
		(user_id, amount_micro, fee_micro, method, account_info, alipay_account, real_name, status, note, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		w.UserID, w.AmountMicro, w.FeeMicro, w.Method, w.AccountInfo,
		w.AlipayAccount, w.RealName, w.Status, w.Note, w.CreatedAt)
	if err != nil {
		return err
	}
	id, err := res2.LastInsertId()
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	w.ID = id
	return nil
}

// ListWithdrawalsByUser 某用户的提现流水（倒序）。
func (s *Store) ListWithdrawalsByUser(userID int64, limit int) ([]*Withdrawal, error) {
	return s.listWithdrawals(`WHERE user_id = ?`, []any{userID}, limit)
}

// ListWithdrawals 全部提现（可按状态过滤，"" = 全部；倒序）。
func (s *Store) ListWithdrawals(status string, limit int) ([]*Withdrawal, error) {
	if strings.TrimSpace(status) == "" {
		return s.listWithdrawals("", nil, limit)
	}
	return s.listWithdrawals(`WHERE status = ?`, []any{status}, limit)
}

func (s *Store) listWithdrawals(where string, args []any, limit int) ([]*Withdrawal, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT id, user_id, amount_micro, fee_micro, method, account_info,
		alipay_account, real_name, status, note,
		reviewed_by, reviewed_at, created_at FROM withdrawals ` + where +
		` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Withdrawal
	for rows.Next() {
		var w Withdrawal
		if err := rows.Scan(&w.ID, &w.UserID, &w.AmountMicro, &w.FeeMicro, &w.Method, &w.AccountInfo,
			&w.AlipayAccount, &w.RealName,
			&w.Status, &w.Note, &w.ReviewedBy, &w.ReviewedAt, &w.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &w)
	}
	return out, rows.Err()
}

// PendingWithdrawMicro 某用户待审中的提现总额（微点），用于防止重复申请超额。
func (s *Store) PendingWithdrawMicro(userID int64) (int64, error) {
	var total sql.NullInt64
	err := s.db.QueryRow(
		`SELECT SUM(amount_micro) FROM withdrawals WHERE user_id = ? AND status = ?`,
		userID, WithdrawPending).Scan(&total)
	if err != nil {
		return 0, err
	}
	return total.Int64, nil
}

// ErrInsufficientWithdraw 审核时**冻结额**不足（数据异常）。
//
// v0.27.0 起它不再是"用户余额不够"（那件事被冻结机制消除了），而是：
// 这条申请在提交时没有冻结过（v0.27.0 之前的历史 pending 记录未被回填），
// 或有人直接改过库。恢复方式：驳回该申请，或人工把 frozen_micro 补上。
var ErrInsufficientWithdraw = errors.New("insufficient frozen amount for withdrawal")

// ReviewWithdrawal 管理员审核提现。
//
//	通过 → 从**冻结额**出账（balance -= amount, frozen -= amount）并置 approved；
//	驳回 → 冻结额**原路退回**可提现额度（frozen -= amount, withdrawable += amount）
//	       并置 rejected。
//
// 仅允许对 pending 记录操作：重复审核会让同一笔钱被扣两次（或退两次）。
//
// 为什么通过时不再检查"用户余额够不够"（v0.27.0 的行为变更）：钱在**申请提交
// 那一刻**就已经从可提现额度划进冻结额，等待期间用户的消费动不了它 —— 所以
// "余额不足导致审核失败"这件事从根上不可能发生（那正是用户报的故障）。
// 现在唯一可能失败的是冻结额本身对不上（历史未回填的记录 / 外部改库），
// 那种情况必须明确报错而不是把余额扣成负数还显示"打款成功"。
func (s *Store) ReviewWithdrawal(id int64, approve bool, note string, reviewerID int64) (*Withdrawal, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var w Withdrawal
	err = tx.QueryRow(`SELECT id, user_id, amount_micro, fee_micro, method, account_info,
		alipay_account, real_name, status, note,
		reviewed_by, reviewed_at, created_at FROM withdrawals WHERE id = ?`, id).
		Scan(&w.ID, &w.UserID, &w.AmountMicro, &w.FeeMicro, &w.Method, &w.AccountInfo,
			&w.AlipayAccount, &w.RealName,
			&w.Status, &w.Note, &w.ReviewedBy, &w.ReviewedAt, &w.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if w.Status != WithdrawPending {
		return nil, errors.New("该提现申请已处理")
	}
	now := time.Now().Unix()
	if approve {
		// 打款：从**冻结额**出账（v0.27.0）。额度早在申请时就已扣走，这里只做
		// "钱真正离开账户"这一步。
		//
		// `AND frozen_micro >= ?` 是最后一道闸：老数据（v0.27.0 之前提交、未冻结）
		// 或外部直改库会让冻结额不足，此时必须**明确报错**而不是把余额扣成负数
		// 还显示"打款成功"。
		res, err := tx.Exec(`UPDATE users SET balance_micro = balance_micro - ?,
			frozen_micro = frozen_micro - ?,
			withdrawable_micro = MIN(withdrawable_micro, MAX(balance_micro - ?, 0)),
			updated_at = ? WHERE id = ? AND frozen_micro >= ?`,
			w.AmountMicro, w.AmountMicro, w.AmountMicro, now, w.UserID, w.AmountMicro)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, ErrInsufficientWithdraw
		}
		w.Status = WithdrawApproved
	} else {
		// 驳回：冻结额**原路退回**可提现额度。
		//
		// 退回时同样要钳制（MIN(…, MAX(balance,0))）：等待期间用户可能已把余额
		// 消费到低于该额度，直接加回去会让 withdrawable > balance（违反不变量，
		// 且那部分"复活"的额度实际已被花掉，能再提一次就是凭空印钱）。
		res, err := tx.Exec(`UPDATE users SET frozen_micro = frozen_micro - ?,
			withdrawable_micro = MIN(withdrawable_micro + ?, MAX(balance_micro, 0)),
			updated_at = ? WHERE id = ? AND frozen_micro >= ?`,
			w.AmountMicro, w.AmountMicro, now, w.UserID, w.AmountMicro)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, ErrInsufficientWithdraw
		}
		w.Status = WithdrawRejected
	}
	if _, err := tx.Exec(`UPDATE withdrawals SET status = ?, note = ?, reviewed_by = ?, reviewed_at = ? WHERE id = ?`,
		w.Status, strings.TrimSpace(note), reviewerID, now, id); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	w.Note = strings.TrimSpace(note)
	w.ReviewedBy = reviewerID
	w.ReviewedAt = now
	return &w, nil
}

// WithdrawalSummaryByUser 某用户各状态提现汇总（微点）。
func (s *Store) WithdrawalSummaryByUser(userID int64) (pendingMicro, approvedMicro, rejectedMicro int64, count int64, err error) {
	rows, err := s.db.Query(
		`SELECT status, COALESCE(SUM(amount_micro),0), COUNT(*) FROM withdrawals WHERE user_id = ? GROUP BY status`,
		userID)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var st string
		var sum, n int64
		if err = rows.Scan(&st, &sum, &n); err != nil {
			return
		}
		count += n
		switch st {
		case WithdrawPending:
			pendingMicro = sum
		case WithdrawApproved:
			approvedMicro = sum
		case WithdrawRejected:
			rejectedMicro = sum
		}
	}
	err = rows.Err()
	return
}

// ListRedeemCodesByUser 某用户已兑换的充值记录（倒序，供控制台「充值提现」页展示）。
func (s *Store) ListRedeemCodesByUser(userID int64, limit int) ([]*RedeemCode, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.Query(
		`SELECT code, credits_micro, kind, created_by, used_by, used_at, created_at
		 FROM redeem_codes WHERE used_by = ? ORDER BY used_at DESC, code ASC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*RedeemCode
	for rows.Next() {
		var c RedeemCode
		if err := rows.Scan(&c.Code, &c.CreditsMicro, &c.Kind, &c.CreatedBy, &c.UsedBy,
			&c.UsedAt, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// ---------- 会话 ----------

// CreateSession 建会话（存 token 的 sha256）。
func (s *Store) CreateSession(tokenHash string, userID int64, ttl time.Duration) error {
	now := time.Now().Unix()
	_, err := s.db.Exec(
		`INSERT INTO sessions (token_hash, user_id, expires_at, created_at) VALUES (?,?,?,?)`,
		tokenHash, userID, time.Now().Add(ttl).Unix(), now)
	return err
}

// LookupSession 校验会话并返回用户 ID。
func (s *Store) LookupSession(tokenHash string) (int64, bool) {
	var uid, exp int64
	err := s.db.QueryRow(`SELECT user_id, expires_at FROM sessions WHERE token_hash = ?`, tokenHash).
		Scan(&uid, &exp)
	if err != nil || exp < time.Now().Unix() {
		return 0, false
	}
	return uid, true
}

// DeleteSession 登出。
func (s *Store) DeleteSession(tokenHash string) {
	_, _ = s.db.Exec(`DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
}

// DeleteSessionsExcept 注销该用户的**其它**会话，保留 keepHash 这一条（改密码时调用）。
//
// 为什么不连当前会话一起删：用户刚改完密码就被踢出登录，体验很差且容易让人以为
// 「改密码把账号弄坏了」。只踢其它会话即可达成目的 —— 密码泄露场景下，攻击者
// 手里那批旧会话会立即失效，而本人当前这条得以延续。
func (s *Store) DeleteSessionsExcept(userID int64, keepHash string) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM sessions WHERE user_id = ? AND token_hash <> ?`, userID, keepHash)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// GC 过期会话。
func (s *Store) GCSessions() {
	_, _ = s.db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, time.Now().Unix())
}

// PricingMultiplier 读取定价倍率（点 / 上游 credit）；非法值回落默认 1.0。
func (s *Store) PricingMultiplier() float64 {
	v, err := s.GetKV(settingMultiplier)
	if err != nil || strings.TrimSpace(v) == "" {
		return defaultMultiplier
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || f <= 0 {
		return defaultMultiplier
	}
	return f
}

// randomHex 生成 n 字节随机数的十六进制串。
//
// 熵源失败即 panic（v0.24.1，P3-3）：此前会**退化成 "%x" % UnixNano()** ——
// 而本函数的头号调用方是**会话令牌**（users.go 的 issueSession）：
// 可枚举的时间戳做会话 token，等于账号可被直接接管。见 secureBytes 的说明。
func randomHex(n int) string {
	return fmt.Sprintf("%x", secureBytes(n))
}
