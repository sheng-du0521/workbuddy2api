// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"workbuddy2api/internal/egress"
	"workbuddy2api/internal/usage"

	_ "modernc.org/sqlite"
)

// Account 一条持久化账号记录。字段与 internal/auth.Auth 对齐，
// 但存储位置从 auths/*.json 换成 SQLite（Web 化必改项之一）。
type Account struct {
	UID          string
	Nickname     string
	Realm        string
	EnterpriseID string
	Domain       string
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64
	DeviceToken  string
	// DeviceTokenAt 设备凭证的**写入时刻**（unix 秒，v0.26.3）。
	//
	// 语义严格是"这个值是什么时候被写进来的"，不是"这行记录什么时候被改过"——
	// 所以它与 UpdatedAt / OAuth 重同步解耦（见 UpsertAccount 的 ON CONFLICT 说明），
	// 只有显式写入凭证的路径（SetAccountDeviceToken）才更新它。
	// 用途：陈旧检测 —— 设备凭证会随客户端重装/升级轮换，一个两个月前抓的凭证
	// 大概率已失效，继续带着它反而更像"伪造的旧客户端"。
	DeviceTokenAt int64
	Disabled      bool
	CooldownUntil int64
	FailCount     int
	// Group 账号分组（自由文本，常见取值：反代 / 桌面端 / 闲置）。用于反代选号分类。
	// 空串 = 未分组。
	Group string
	// EgressProxy 该账号的专用出站代理（http/https/socks5 URL）。空串 = 走全局默认/直连。
	// 目的：让不同账号从不同出口 IP 打上游，降低关联封号风险。
	EgressProxy string

	// ---- 用户代挂（hosting）相关字段 ----
	// OwnerUserID 账号归属：0 = 平台/管理员自持账号（默认参与平台反代）；
	// >0 = 该用户「代挂」的账号（本人可在控制台用自用 Key 反代，也可申请进平台号池）。
	OwnerUserID int64
	// HostGroup 号主给自己的代挂账号分的组（自由文本）。仅代挂账号有意义；空串 = 未分组。
	// 与 Group 的区别：Group 是管理员的反代选号分组；HostGroup 属于号主、在管理后台统一展示。
	HostGroup string
	// PoolState 代挂账号的入池状态："" = 未申请 / "pending" = 已申请待审 /
	// "in" = 已入池（参与平台反代并可获收益）/ "rejected" = 已驳回。
	// 平台自持账号（OwnerUserID == 0）恒视为在池，不看本字段。
	PoolState string
	// HostNote 入池申请备注 / 驳回原因（管理后台与号主可见）。
	HostNote string
	// EarnedMicro 该账号累计为号主带来的收益（微点，仅展示与对账用）。
	EarnedMicro int64
	// TaskCfg 该账号的定时任务配置（JSON，v0.11.0）。
	// 空串 = 缺省（参与全部自动任务）；否则为 AccountTaskConfig 的序列化。
	// 由号主（控制台「账号代挂」）或管理员（账号池）设置，调度器据此逐账号过滤。
	TaskCfg string

	// ---- 消耗分组（v0.13.0）----
	// ConsumeGroup 决定该账号是否参与平台反代消耗：
	//   apply    用户申请（优先消耗）
	//   proxy    反代（次级消耗）
	//   idle     闲置（不参与消耗）
	//   abnormal 异常（不参与消耗，系统自动归档）
	// 空串 = 未显式设置，按来源推断（见 effectiveConsumeGroup）。
	ConsumeGroup string
	// ConsumeGroupPrev 自动归档前的原分组，用于「积分不足 / 账号故障」类异常
	// 冷却到期后自动回归；手动改组时清空。
	ConsumeGroupPrev string
	// ConsumeGroupNote 归档原因。以 autoTagPrefix 开头 = 系统自动归档（可自动回归）；
	// 手动设为异常时清空（视为管理员的最终裁决，不自动回归）。
	ConsumeGroupNote string

	// GrowthDoneAt 成长任务「最近一次执行」时间戳（v0.17.0）。0 = 从未跑过。
	// 只用于账号卡上显示「上次执行时间」，**不参与准入判断**（成长任务为手动选号
	// 执行，没有待办队列，选谁跑谁）。不做每日去重（行为事件按天幂等，重复跑无害）。
	GrowthDoneAt int64

	CreatedAt int64
	UpdatedAt int64
}

// AccountTaskConfig 单账号的定时任务配置（存 accounts.task_cfg，JSON）。
// 缺省（Enabled=false 且 Disabled 空）语义由调用方按「空串 = 全参与」处理。
type AccountTaskConfig struct {
	// Enabled 参与开关：false = 该账号不参与任何自动任务。
	Enabled bool `json:"enabled"`
	// Disabled 关闭的任务类型（checkin/travel/activity/keepalive/cat）；缺省 = 该类开启。
	Disabled []string `json:"disabled,omitempty"`
}

// CooldownRemaining 返回剩余冷却秒数（<=0 表示可用）。
func (a Account) CooldownRemaining() int64 {
	return a.CooldownUntil - time.Now().Unix()
}

// Store SQLite 存储句柄。
type Store struct {
	db  *sql.DB
	sec *SecretBox // 可选：账号 token 静态加密箱（nil = 不加密，向后兼容）

	// *usage.Queries 用量台账（usage_logs）的**只读聚合**（v0.31.9，P2-7 第二刀下沉）。
	//
	// **嵌入**而不是再加一个字段：这样 `store.UsageByModel(...)` / `store.ListUsageLogs(...)`
	// 等 33 个既有调用点**一行不改**继续编译（方法提升），本刀才能做到"零行为变化"
	// —— 由既有测试全绿证明。写入（INSERT usage_logs）仍在本包，它要参与预扣/结算事务。
	*usage.Queries
}

// SetSecret 注入加密箱，用于账号 AccessToken/RefreshToken/DeviceToken 的静态加密。
// 传 nil（-secret none 或未启用）即为明文存储的逃生门。
func (s *Store) SetSecret(sec *SecretBox) { s.sec = sec }

// encPrefix 密文标记：base64 字符集不含 ':'，故此前缀与明文/裸 base64 不会混淆，
// 可安全用于「读时判别是否密文」，实现老明文行零迁移平滑过渡。
const encPrefix = "enc1:"

// encField 写库前加密（空串与已加密的原样返回）。
func (s *Store) encField(plain string) string {
	if s.sec == nil || plain == "" || strings.HasPrefix(plain, encPrefix) {
		return plain
	}
	out, err := s.sec.Encrypt(plain)
	if err != nil {
		return plain // 加密失败宁可明文，也不丢凭证（可用性优先）
	}
	return encPrefix + out
}

// decField 读库后解密：非密文（老明文行）原样返回，解密失败也回退原文，
// 保证升级过程中既有账号不因格式变更而不可用。
func (s *Store) decField(v string) string {
	if s.sec == nil || !strings.HasPrefix(v, encPrefix) {
		return v
	}
	plain, err := s.sec.Decrypt(strings.TrimPrefix(v, encPrefix))
	if err != nil {
		return v
	}
	return plain
}

// OpenStore 打开（必要时创建）数据库并执行迁移。
//
// ## 为什么用 WAL（v0.27.0，由一次真实超时逼出来的）
//
// 默认的 rollback journal 模式下**每条语句都要单独 fsync**；而迁移有近百条
// DDL，于是每次 OpenStore 要花约 1 秒。测试包里几乎每个用例都建一个库，
// 累计把 `cmd/webapp` 整个包推到 **10 分钟超时**（实测 602s 失败）。
// 实测切到 WAL 后：建库 1080ms → **88ms**，单次写入 14ms → **0.5ms**。
// 对生产同样有益（启动与每次结算写库都变快）。
//
// ## 取舍（必须知道，别当成纯优化）
//
//   - 库旁边会多出 `webapp.db-wal` 与 `webapp.db-shm` 两个文件。
//     **备份/搬迁要么先停服**（Close 会 checkpoint 并删掉它们），**要么三个文件
//     一起复制** —— 只拷 .db 会丢掉尚未 checkpoint 的最近写入。
//   - **不要在网络盘 / 云同步目录（SMB、OneDrive、坚果云等）上运行**：WAL 依赖
//     共享内存，网络文件系统上不可靠。本应用是单机本地盘部署，符合前提。
//   - 可回退：`PRAGMA journal_mode=DELETE` 即切回（SQLite 会自动 checkpoint），
//     **无需任何数据迁移**。
func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, Queries: usage.New(db)}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	// 出口代理池（v0.14.0 防封）：独立建表，与 accounts 的增量列迁移分开，
	// 便于单测直接构造只含该表的库。
	if err := s.migrateEgressProxy(); err != nil {
		db.Close()
		return nil, err
	}
	// 站内公告（v0.17.8）：同样独立建表，便于单测构造。
	if err := s.migrateAnnouncements(); err != nil {
		db.Close()
		return nil, err
	}
	// 账号上架 / 买断（v0.19.0）：在售状态与买断台账独立建表。
	if err := s.migrateAccountSales(); err != nil {
		db.Close()
		return nil, err
	}
	// 余额分桶一次性回填（v0.21.0）：按历史返点/收益/卖号流水还原可提现额度。
	// 放在所有建表（尤其 rebates / host_earnings / account_purchases）之后。
	if err := s.backfillBalanceBuckets(); err != nil {
		db.Close()
		return nil, err
	}
	// 计费倍率变更历史（v0.27.0）：见 multiplier.go。
	if err := s.EnsureMultiplierHistory(); err != nil {
		db.Close()
		return nil, err
	}
	// 余额变动审计表（v0.27.0）：见 balanceaudit.go。
	if err := s.EnsureBalanceAudits(); err != nil {
		db.Close()
		return nil, err
	}
	// 提现冻结回填（v0.27.0）：必须排在 backfillBalanceBuckets **之后** ——
	// 后者把老库的 withdrawable 从历史流水还原出来（有 kv 守卫，只跑一次），
	// 本函数再从还原后的额度里划走冻结额。顺序反了会"先划后还原"，等于没冻。
	if err := s.backfillWithdrawFreeze(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close 关闭数据库连接。
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS accounts (
			uid            TEXT PRIMARY KEY,
			nickname       TEXT NOT NULL DEFAULT '',
			realm          TEXT NOT NULL DEFAULT 'cn',
			enterprise_id  TEXT NOT NULL DEFAULT '',
			domain         TEXT NOT NULL DEFAULT '',
			access_token   TEXT NOT NULL DEFAULT '',
			refresh_token  TEXT NOT NULL DEFAULT '',
			expires_at     INTEGER NOT NULL DEFAULT 0,
			device_token   TEXT NOT NULL DEFAULT '',
			device_token_at INTEGER NOT NULL DEFAULT 0,
			disabled       INTEGER NOT NULL DEFAULT 0,
			cooldown_until INTEGER NOT NULL DEFAULT 0,
			fail_count     INTEGER NOT NULL DEFAULT 0,
			created_at     INTEGER NOT NULL DEFAULT 0,
			updated_at     INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_accounts_realm ON accounts(realm)`,
		// 风控上报台账（v0.24.6）：上游判定账号受限时落一条，见 riskreport.go。
		migrationRiskReports,
		// 消耗失败台账（v0.26.3）：四个阶段（预扣/上游/结算/扣费）的失败各落一条，
		// 见 failures.go。与 risk_reports 的区别：risk_reports 记的是**账号级**
		// 风控事件与处置动作，本表记的是**每一次消耗失败**及其错误码/上下文。
		migrationConsumeFailures,
		`CREATE TABLE IF NOT EXISTS kv (
			k TEXT PRIMARY KEY,
			v TEXT NOT NULL DEFAULT ''
		)`,
		// 会话粘性绑定（v0.17.5）：key = 会话键，uid = 绑定账号，expires_at = 0 表示不过期。
		// 存在的意义是**跨重启保持粘性** —— 此前绑定是纯内存（redisstore.Noop），
		// 进程一重启同一场对话的下一个请求就会重新分配账号（表现为「调用过程中莫名换号」）。
		`CREATE TABLE IF NOT EXISTS sticky_binds (
			bind_key   TEXT PRIMARY KEY,
			uid        TEXT NOT NULL DEFAULT '',
			expires_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_sticky_expires ON sticky_binds(expires_at)`,
	}
	// 台账索引与建表分开追加：索引清单是切片，用展开而不是按下标逐个列 ——
	// 按下标引用（consumeFailureIndexes[0]…）在有人重排切片时会静默错位。
	stmts = append(stmts, consumeFailureIndexes...)
	for _, st := range stmts {
		if _, err := s.db.Exec(st); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	// 增量列：老库（v0.6.0 及以前）没有这两列，用 ADD COLUMN 平滑补齐；
	// 已存在时报 "duplicate column name"，忽略即可（幂等）。
	for _, col := range []string{
		`ALTER TABLE accounts ADD COLUMN account_group TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE accounts ADD COLUMN egress_proxy TEXT NOT NULL DEFAULT ''`,
		// 用户代挂（v0.9.0）：老库平滑补齐，既有平台账号自动落到 owner=0/未分组/未申请。
		`ALTER TABLE accounts ADD COLUMN owner_user_id INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE accounts ADD COLUMN host_group TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE accounts ADD COLUMN pool_state TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE accounts ADD COLUMN host_note TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE accounts ADD COLUMN earned_micro INTEGER NOT NULL DEFAULT 0`,
		// 逐账号定时任务配置（v0.11.0）：空串 = 参与全部自动任务。
		`ALTER TABLE accounts ADD COLUMN task_cfg TEXT NOT NULL DEFAULT ''`,
		// 消耗分组（v0.13.0）：空串 = 按来源推断（见 effectiveConsumeGroup）。
		`ALTER TABLE accounts ADD COLUMN consume_group TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE accounts ADD COLUMN consume_group_prev TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE accounts ADD COLUMN consume_group_note TEXT NOT NULL DEFAULT ''`,
		// 登录失效标记（v0.31.30）：refresh 12153 被确认的时刻，0 = 正常。
		// 为什么落库：卡片要整卡标红并给「重新登录」入口，这需要**逐号**状态位；
		// 风控台账是事件流，用它反推"当前还有效吗"只能靠时间窗猜。
		// 清零点精确：UpdateTokens（刷新成功写回）与导入成功（ClearTokenDead）。
		`ALTER TABLE accounts ADD COLUMN token_dead_at INTEGER NOT NULL DEFAULT 0`,
		// 成长任务最近执行时间戳（v0.17.0）：0 = 从未跑过。仅用于账号卡展示，
		// 不作准入判断（手动选号执行）；不做每日去重（行为事件本身按天幂等）。
		`ALTER TABLE accounts ADD COLUMN growth_done_at INTEGER NOT NULL DEFAULT 0`,
		// 积分明细**本地缓存**（v0.24.9）：上次成功查询到的逐套餐明细（JSON）+ 时刻。
		//
		// 为什么需要：v0.24.3 为堵住封号事故根因，把账号卡的积分区块从「渲染即出站」
		// 改成「点开才查」—— 安全了，但卡片上再也看不到明细，只能看到总额快照。
		// 现在把**上一次查询结果**落库：渲染时直接读缓存（零出站），点按钮才刷新。
		// 于是「看得到明细」与「不打并发风暴」两件事可以同时成立。
		//
		// 刻意**不进 accountCols / UpsertAccount**：它是纯展示缓存，不是账号属性。
		// 若混进 UpsertAccount，一次重新 OAuth 登录就会把它写空（同 v0.13.0 那类坑）。
		`ALTER TABLE accounts ADD COLUMN credit_packs TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE accounts ADD COLUMN credit_packs_at INTEGER NOT NULL DEFAULT 0`,
		// v0.26.3：设备凭证的写入时刻（陈旧检测用，见 devicecred.go）。
		// 为什么不复用 updated_at：账号的任何字段变动都会刷新 updated_at，
		// 用它算"凭证多少天没更新"会得出**偏年轻**的结论（改个分组就重置了），
		// 而陈旧检测的全部意义就是发现"这个凭证很久没换了"。
		`ALTER TABLE accounts ADD COLUMN device_token_at INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := s.db.Exec(col); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migrate add column: %w", err)
		}
	}
	// 租户侧表（users / api_keys / usage_logs / redeem_codes / sessions）。
	return s.migrateUsers()
}

const accountCols = `uid, nickname, realm, enterprise_id, domain, access_token,
	refresh_token, expires_at, device_token, device_token_at, disabled, cooldown_until, fail_count,
	account_group, egress_proxy, owner_user_id, host_group, pool_state, host_note,
	earned_micro, task_cfg, consume_group, consume_group_prev, consume_group_note,
	growth_done_at, created_at, updated_at`

func (s *Store) scanAccount(row interface{ Scan(dest ...any) error }) (*Account, error) {
	var a Account
	var disabled int
	err := row.Scan(
		&a.UID, &a.Nickname, &a.Realm, &a.EnterpriseID, &a.Domain,
		&a.AccessToken, &a.RefreshToken, &a.ExpiresAt, &a.DeviceToken,
		&a.DeviceTokenAt,
		&disabled, &a.CooldownUntil, &a.FailCount, &a.Group, &a.EgressProxy,
		&a.OwnerUserID, &a.HostGroup, &a.PoolState, &a.HostNote, &a.EarnedMicro, &a.TaskCfg,
		&a.ConsumeGroup, &a.ConsumeGroupPrev, &a.ConsumeGroupNote,
		&a.GrowthDoneAt,
		&a.CreatedAt, &a.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	// 落库字段可能是密文（enc1: 前缀），统一在读取边界解密回明文，
	// 使上层（池/auth/比对逻辑）始终拿到明文，无需感知存储形态。
	a.AccessToken = s.decField(a.AccessToken)
	a.RefreshToken = s.decField(a.RefreshToken)
	a.DeviceToken = s.decField(a.DeviceToken)
	a.Disabled = disabled != 0
	return &a, nil
}

// ListAccounts 返回全部账号（按创建时间升序）。
func (s *Store) ListAccounts() ([]*Account, error) {
	rows, err := s.db.Query(`SELECT ` + accountCols + ` FROM accounts ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Account
	for rows.Next() {
		a, err := s.scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetAccount 按 uid 取账号；不存在返回 (nil, nil)。
func (s *Store) GetAccount(uid string) (*Account, error) {
	row := s.db.QueryRow(`SELECT `+accountCols+` FROM accounts WHERE uid = ?`, uid)
	a, err := s.scanAccount(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return a, err
}

// UpsertAccount 插入或更新账号（按 uid 主键）。首次写入自动填 created_at。
func (s *Store) UpsertAccount(a *Account) error {
	now := time.Now().Unix()
	if a.CreatedAt == 0 {
		a.CreatedAt = now
	}
	a.UpdatedAt = now
	// ON CONFLICT 刻意**不**更新 owner_user_id / host_group / pool_state / host_note /
	// earned_micro / consume_group*：这几项是号主、管理员与系统归档的业务状态，
	// 重新 OAuth 登录或重同步绝不能把它们抹掉（否则一次刷新 token 就会把「已入池」打回
	// 「未申请」、把收益清零、把已归档的异常账号悄悄放回消耗池）。
	_, err := s.db.Exec(`INSERT INTO accounts (`+accountCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(uid) DO UPDATE SET
			nickname=excluded.nickname,
			realm=excluded.realm,
			enterprise_id=excluded.enterprise_id,
			domain=excluded.domain,
			access_token=excluded.access_token,
			refresh_token=excluded.refresh_token,
			expires_at=excluded.expires_at,
			device_token=excluded.device_token,
			-- device_token_at 刻意**不**跟随重同步刷新：它的语义是"这个凭证是什么时候换的"，
			-- 用 excluded 会让每次 OAuth 刷新都把它洗成"现在"，陈旧检测将永远显示新鲜
			-- （正是需要它报警的那种情况反而消失）。只有显式写入凭证才更新它。
			device_token_at=CASE WHEN excluded.device_token <> accounts.device_token
			                     AND excluded.device_token <> ''
			                THEN excluded.device_token_at ELSE accounts.device_token_at END,
			disabled=excluded.disabled,
			cooldown_until=excluded.cooldown_until,
			fail_count=excluded.fail_count,
			updated_at=excluded.updated_at`,
		a.UID, a.Nickname, a.Realm, a.EnterpriseID, a.Domain,
		s.encField(a.AccessToken), s.encField(a.RefreshToken), a.ExpiresAt, s.encField(a.DeviceToken),
		a.DeviceTokenAt,
		boolToInt(a.Disabled), a.CooldownUntil, a.FailCount, a.Group, a.EgressProxy,
		a.OwnerUserID, a.HostGroup, a.PoolState, a.HostNote, a.EarnedMicro, a.TaskCfg,
		a.ConsumeGroup, a.ConsumeGroupPrev, a.ConsumeGroupNote,
		a.GrowthDoneAt,
		a.CreatedAt, a.UpdatedAt,
	)
	return err
}

// SetAccountConsumeGroup 手动设置账号的消耗分组（apply/proxy/idle/abnormal）。
// 手动设置即管理员的最终裁决：一并清空自动归档留下的「原分组 / 原因」，
// 使该账号不再被自动回归逻辑改动。空串 = 回到「按来源推断」。
func (s *Store) SetAccountConsumeGroup(uid, group string) error {
	_, err := s.db.Exec(`UPDATE accounts SET consume_group = ?, consume_group_prev = '',
		consume_group_note = '', updated_at = ? WHERE uid = ?`,
		strings.TrimSpace(group), time.Now().Unix(), uid)
	return err
}

// MarkAccountAbnormal 系统自动归档：把账号置为「异常」组，并记下原分组与原因。
// prev 用于冷却到期后自动回归；note 约定以 autoTagPrefix 开头以标识「自动归档」。
func (s *Store) MarkAccountAbnormal(uid, prev, note string) error {
	_, err := s.db.Exec(`UPDATE accounts SET consume_group = ?, consume_group_prev = ?,
		consume_group_note = ?, updated_at = ? WHERE uid = ?`,
		cgAbnormal, strings.TrimSpace(prev), strings.TrimSpace(note), time.Now().Unix(), uid)
	return err
}

// RestoreAccountConsumeGroup 自动回归：把账号恢复到原分组并清空归档标记。
// group 为空时回落「按来源推断」（写空串即可）。
func (s *Store) RestoreAccountConsumeGroup(uid, group string) error {
	g := strings.TrimSpace(group)
	if g == cgAbnormal { // 防御：绝不把回归目标写成异常组自身
		g = ""
	}
	_, err := s.db.Exec(`UPDATE accounts SET consume_group = ?, consume_group_prev = '',
		consume_group_note = '', updated_at = ? WHERE uid = ?`,
		g, time.Now().Unix(), uid)
	return err
}

// SetAccountSystemGroup 系统自动设置消耗分组（v0.20.0，预备组进出用）。
//
// 与 SetAccountConsumeGroup（管理员手动设置，会清空 prev/note）刻意区分：
// 本方法保留 prev 与 note，使「这次分组变更是系统做的」可被追溯，也让管理员
// 手动设的分组能被识别为「非系统态」。
func (s *Store) SetAccountSystemGroup(uid, group, prev, note string) error {
	_, err := s.db.Exec(`UPDATE accounts SET consume_group = ?, consume_group_prev = ?,
		consume_group_note = ?, updated_at = ? WHERE uid = ?`,
		strings.TrimSpace(group), strings.TrimSpace(prev),
		strings.TrimSpace(note), time.Now().Unix(), uid)
	return err
}

// ListAbnormalAutoAccounts 列出「系统自动归档」到异常组的账号（带归档标记）。// 手动设为异常的账号（无标记）不在其中 —— 它们需人工处理，不参与自动回归。
func (s *Store) ListAbnormalAutoAccounts() ([]*Account, error) {
	rows, err := s.db.Query(`SELECT `+accountCols+` FROM accounts
		WHERE consume_group = ? AND consume_group_note LIKE ?`,
		cgAbnormal, autoTagPrefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Account
	for rows.Next() {
		a, err := s.scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetAccountGroup 设置账号分组（反代选号分类用）。空串 = 取消分组。
func (s *Store) SetAccountGroup(uid, group string) error {
	_, err := s.db.Exec(`UPDATE accounts SET account_group = ?, updated_at = ? WHERE uid = ?`,
		strings.TrimSpace(group), time.Now().Unix(), uid)
	return err
}

// SetAccountEgress 设置账号专用出站代理。空串 = 走全局默认/直连。
//
// **写库前统一规范化**（v0.31.33）：线路的身份是 URL 字符串（`egress_proxies.url`
// 是唯一键），而账号上存的是同一个串。同一个代理写成 `HTTP://a:1/` 与 `http://a:1`
// 会让"账号代理"和"出口代理"对不上号 —— 页面显示两条线、实际一个 IP。
//
// 放在这里（而不是每个调用点各写一遍）是因为这是**唯一**的落库处：
// 只要还有一条路径绕过规范化，两边就又会漂。非法的 URL 原样写入（不在这里报错）：
// 校验的责任在 `ValidateAccountEgress` / `a.setAccountEgress`，写库层不该吞掉它，
// 也不该静默改成别的值 —— 那会让"我填的"和"存下的"不一致且没人知道。
func (s *Store) SetAccountEgress(uid, proxy string) error {
	v := strings.TrimSpace(proxy)
	if canon, err := egress.Normalize(v); err == nil {
		v = canon
	}
	_, err := s.db.Exec(`UPDATE accounts SET egress_proxy = ?, updated_at = ? WHERE uid = ?`,
		v, time.Now().Unix(), uid)
	return err
}

// SetAccountOwner 设置账号归属（OAuth 建号时写入；0 = 平台自持）。
func (s *Store) SetAccountOwner(uid string, ownerUserID int64) error {
	_, err := s.db.Exec(`UPDATE accounts SET owner_user_id = ?, updated_at = ? WHERE uid = ?`,
		ownerUserID, time.Now().Unix(), uid)
	return err
}

// SetAccountHostGroup 设置号主给代挂账号分的组。空串 = 取消分组。
func (s *Store) SetAccountHostGroup(uid, group string) error {
	_, err := s.db.Exec(`UPDATE accounts SET host_group = ?, updated_at = ? WHERE uid = ?`,
		strings.TrimSpace(group), time.Now().Unix(), uid)
	return err
}

// SetAccountPoolState 设置代挂账号入池状态与备注（pending/in/rejected/""）。
func (s *Store) SetAccountPoolState(uid, state, note string) error {
	_, err := s.db.Exec(`UPDATE accounts SET pool_state = ?, host_note = ?, updated_at = ? WHERE uid = ?`,
		strings.TrimSpace(state), strings.TrimSpace(note), time.Now().Unix(), uid)
	return err
}

// AddAccountEarned 累加某账号为号主产生的收益（微点）。
func (s *Store) AddAccountEarned(uid string, micro int64) error {
	_, err := s.db.Exec(`UPDATE accounts SET earned_micro = earned_micro + ?, updated_at = ? WHERE uid = ?`,
		micro, time.Now().Unix(), uid)
	return err
}

// SetAccountTaskConfig 设置某账号的定时任务配置（JSON；空串 = 参与全部自动任务）。
func (s *Store) SetAccountTaskConfig(uid, cfg string) error {
	_, err := s.db.Exec(`UPDATE accounts SET task_cfg = ?, updated_at = ? WHERE uid = ?`,
		strings.TrimSpace(cfg), time.Now().Unix(), uid)
	return err
}

// TaskConfigMap 返回 uid → 原始 task_cfg 的映射（只取非空行）。
// 供调度器的逐账号任务过滤缓存一次性加载；缺省账号不在表里，按「全参与」处理。
func (s *Store) TaskConfigMap() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT uid, task_cfg FROM accounts WHERE task_cfg <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var uid, cfg string
		if err := rows.Scan(&uid, &cfg); err != nil {
			return nil, err
		}
		out[uid] = cfg
	}
	return out, rows.Err()
}

// ListAccountsByOwner 返回某用户代挂的全部账号（按创建时间升序）。
func (s *Store) ListAccountsByOwner(ownerUserID int64) ([]*Account, error) {
	rows, err := s.db.Query(`SELECT `+accountCols+` FROM accounts WHERE owner_user_id = ? ORDER BY created_at ASC`, ownerUserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Account
	for rows.Next() {
		a, err := s.scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListHostedAccounts 返回全部「用户代挂」账号（owner > 0），供管理后台统一展示。
func (s *Store) ListHostedAccounts() ([]*Account, error) {
	rows, err := s.db.Query(`SELECT ` + accountCols + ` FROM accounts WHERE owner_user_id > 0 ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Account
	for rows.Next() {
		a, err := s.scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListPendingHostAccounts 返回待审（pool_state = 'pending'）的代挂账号。
func (s *Store) ListPendingHostAccounts() ([]*Account, error) {
	rows, err := s.db.Query(`SELECT ` + accountCols + ` FROM accounts WHERE owner_user_id > 0 AND pool_state = 'pending' ORDER BY updated_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Account
	for rows.Next() {
		a, err := s.scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteAccount 删除账号。
//
// 注意：这是**裸删除**，只动 accounts 一行。调用方应当用 app.deleteAccountsHard ——
// 它还会同步池内条目与健康检测快照（否则界面上会留下"已删除的号"的陈旧结论）。
// 账号的**账目类关联行不级联删除**（host_earnings / account_sales / rebates /
// account_purchases）：那些是资金与分成流水，删掉会让用户账目对不上，只能留作历史。
func (s *Store) DeleteAccount(uid string) error {
	_, err := s.db.Exec(`DELETE FROM accounts WHERE uid = ?`, uid)
	return err
}

// DeleteAccounts 批量删除账号（单事务：要么全成，要么全不动）。
//
// 为什么必须事务：批量删是**不可逆**操作，中途失败留下"删了一半"的状态最难收拾
// ——运维既不知道删了哪些，也没法简单地重试（重试会重复扣减计数口径）。
// 返回实际删除的行数（不存在的 uid 不计入），便于前端如实回报。
func (s *Store) DeleteAccounts(uids []string) (int, error) {
	if len(uids) == 0 {
		return 0, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.Prepare(`DELETE FROM accounts WHERE uid = ?`)
	if err != nil {
		return 0, err
	}
	defer func() { _ = stmt.Close() }()

	deleted := 0
	for _, uid := range uids {
		res, err := stmt.Exec(uid)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			deleted++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return deleted, nil
}

// SetDisabled 设置账号停用状态。
func (s *Store) SetDisabled(uid string, disabled bool) error {
	_, err := s.db.Exec(`UPDATE accounts SET disabled = ?, updated_at = ? WHERE uid = ?`,
		boolToInt(disabled), time.Now().Unix(), uid)
	return err
}

// UpdateTokens 写回刷新后的 token。
func (s *Store) UpdateTokens(uid, accessToken, refreshToken string, expiresAt int64) error {
	// token_dead_at 一并清零（v0.31.30）：能成功刷新并写回，就证明这个号的会话活了 ——
	// 卡片上的红标必须立刻消失，否则用户重新登录后仍看到"失效"，会以为没生效。
	_, err := s.db.Exec(`UPDATE accounts SET access_token = ?, refresh_token = ?, expires_at = ?,
	        token_dead_at = 0, updated_at = ? WHERE uid = ?`,
		s.encField(accessToken), s.encField(refreshToken), expiresAt, time.Now().Unix(), uid)
	return err
}

// MarkTokenDead 标记账号登录失效（refresh 12153），供卡片整卡标红与按钮提示。
func (s *Store) MarkTokenDead(uid string) error {
	if strings.TrimSpace(uid) == "" {
		return nil
	}
	_, err := s.db.Exec(`UPDATE accounts SET token_dead_at = ? WHERE uid = ? AND token_dead_at = 0`,
		time.Now().Unix(), uid)
	return err
}

// ClearTokenDead 清除登录失效标记（重新导入成功后调用）。
//
// 为什么必须显式清：重新导入的判断是"uid 已存在 → upsert"，而 upsert 刻意不覆盖
// 业务状态列（见 import_desktop.go 顶部说明）。不清这一位，用户按提示重新导入后
// 卡片仍然是红的 —— 那比不标红更让人困惑。
func (s *Store) ClearTokenDead(uid string) error {
	if strings.TrimSpace(uid) == "" {
		return nil
	}
	_, err := s.db.Exec(`UPDATE accounts SET token_dead_at = 0 WHERE uid = ?`, uid)
	return err
}

// TokenDeadMap 返回 uid → 失效时刻（仅含已标记的号）。
//
// 单独查询而不是塞进 Account/SELECT 列清单：改 select 列要同步 scan 顺序，
// 是这个仓库最容易出错的地方之一（多一列就整片扫空）。这里只多一次轻查询。
func (s *Store) TokenDeadMap() map[string]int64 {
	out := map[string]int64{}
	rows, err := s.db.Query(`SELECT uid, token_dead_at FROM accounts WHERE token_dead_at > 0`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var uid string
		var at int64
		if rows.Scan(&uid, &at) == nil {
			out[uid] = at
		}
	}
	return out
}

// SetCooldown 给账号设置冷却到期时间戳（Unix 秒），并累计失败次数。
func (s *Store) SetCooldown(uid string, until int64) error {
	_, err := s.db.Exec(`UPDATE accounts SET cooldown_until = ?, fail_count = fail_count + 1, updated_at = ? WHERE uid = ?`,
		until, time.Now().Unix(), uid)
	return err
}

// ClearFailure 成功后清零失败计数与冷却。
func (s *Store) ClearFailure(uid string) error {
	_, err := s.db.Exec(`UPDATE accounts SET fail_count = 0, cooldown_until = 0, updated_at = ? WHERE uid = ?`,
		time.Now().Unix(), uid)
	return err
}

// SetKV / GetKV 简单键值表（存放 admin key hash 等运行时配置）。
func (s *Store) SetKV(k, v string) error {
	_, err := s.db.Exec(`INSERT INTO kv (k, v) VALUES (?, ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v`, k, v)
	return err
}

// GetKV 读取键值；不存在返回空串。
func (s *Store) GetKV(k string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT v FROM kv WHERE k = ?`, k).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

// ListKV 按前缀列出键值对（用于批量读回观测单价等）。
func (s *Store) ListKV(prefix string) (map[string]string, error) {
	rows, err := s.db.Query(`SELECT k, v FROM kv WHERE k LIKE ?`, prefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
