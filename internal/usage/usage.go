// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 台账（usage_logs 用量 + host_earnings 代挂收益）的**只读聚合**：
// 明细查询、按账号/用户/模型/日的统计、性能分布、号主收益按日/按模型。
//
// 为什么单独成包（v0.31.9，P2-7 拆包第二刀；v0.31.13 并入 host_earnings 聚合）：
//  1. 这一簇在 `Store`（169 个方法）里是**唯一全部只读、且只依赖 db 的聚合面** ——
//     实测 10 个方法里 9 个只碰 `s.db`，第 10 个只调本簇内的另一个方法；
//  2. 聚合 SQL 是"错了不会崩、只会给出错误数字"的那类代码，最需要**独立测试**
//     （独立包 = 可以用一个临时 SQLite 建表插数、断言聚合结果，不必拉起整个 app）；
//  3. 名字**刻意保留原样**（`usage.Log` / `usage.UsageByModel` 这种 stutter 是取舍）：
//     本刀的目标是零调用点改动 —— cmd/webapp 把 `*Queries` **嵌入** `Store`，
//     方法名一致 → 33 个既有调用点一行不改，由既有测试证明行为零变化。
//     重命名留作后续独立的一刀（改名字只需改调用点，不必碰逻辑）。
//
// 边界：本包**只读**。写用量台账（INSERT usage_logs）仍留在 cmd/webapp 的 Store 里 ——
// 写入要参与预扣/结算事务，和这里的报表读取不是一件事。
package usage

import (
	"database/sql"
	"sort"
	"strings"
	"time"
)

// Queries 只读聚合查询集合。构造后即可用（无状态、可并发）。
type Queries struct{ db *sql.DB }

// New 绑定一个数据库句柄。传 nil 会让所有查询在调用时 panic（明确失败优于静默空结果）。
func New(db *sql.DB) *Queries { return &Queries{db: db} }

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

// ListUsageLogs 某用户最近流水（分页）。
func (uq *Queries) ListUsageLogs(userID int64, limit, offset int) ([]*UsageLog, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := uq.db.Query(
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
func (uq *Queries) UsageLogsBySticky(hash string, limit int) ([]*UsageLog, error) {
	if strings.TrimSpace(hash) == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := uq.db.Query(
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
func (uq *Queries) UserUsageSummary(userID int64) (reqs int64, tokens int64, chargedMicro int64, err error) {
	err = uq.db.QueryRow(
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
func (uq *Queries) UsageByAccount() (map[string]AccountUsageStat, error) {
	rows, err := uq.db.Query(`SELECT upstream_uid,
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
func (uq *Queries) UsageByUser(userID int64, days, limit int) ([]UsageUserStat, error) {
	if days <= 0 || days > 365 {
		days = 14
	}
	since := LocalDayStart(days)
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
	rows, err := uq.db.Query(q, args...)
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
func (uq *Queries) CountUsageUsers(days int) (int, error) {
	if days <= 0 || days > 365 {
		days = 14
	}
	since := LocalDayStart(days)
	var n int
	err := uq.db.QueryRow(
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

// UsageDailyScoped 同上，但可限定 scope（"" = 全部；ScopePool / ScopeHost 单类）。
// 控制台「用量账单」据此把消费拆成「自用 / 用号池 / 进号池收益」三类。
func (uq *Queries) UsageDailyScoped(userID int64, days int, scope string) ([]UsageDailyStat, error) {
	if days <= 0 || days > 365 {
		days = 14
	}
	off := LocalDayOffset()
	since := LocalDayStart(days)
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
	rows, err := uq.db.Query(query, args...)
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
func (uq *Queries) UsageByModel(userID int64, days int) ([]UsageModelStat, error) {
	return uq.UsageByModelScoped(userID, days, "")
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
//
// usagePerfMaxSample 分位数计算的**采样上限**（v0.31.59，审查 F13）。
//
// 原实现把窗口内（最长 365 天）所有 latency 读进 slice 再排序：几十万行时
// 既占内存又长期占用唯一的数据库连接（SetMaxOpenConns(1)），计费写入只能排队。
// 5 万条足以让 p50/p95 稳定到毫秒级，再多的边际收益远小于它对主链路的影响。
const usagePerfMaxSample = 50000

func (uq *Queries) UsagePerf(userID int64, days int) (UsagePerf, error) {
	var p UsagePerf
	if days <= 0 || days > 365 {
		days = 14
	}
	since := LocalDayStart(days)

	where := ` WHERE created_at >= ?`
	args := []any{since}
	if userID > 0 {
		where += ` AND user_id = ?`
		args = append(args, userID)
	}

	// 1) 耗时聚合
	var avg sql.NullFloat64
	var maxMS sql.NullInt64
	if err := uq.db.QueryRow(
		`SELECT COUNT(*), AVG(latency_ms), MAX(latency_ms) FROM usage_logs`+
			where+` AND latency_ms > 0`, args...).
		Scan(&p.LatencySampled, &avg, &maxMS); err != nil {
		return p, err
	}
	if avg.Valid {
		p.AvgLatencyMS = avg.Float64
	}
	p.MaxLatencyMS = maxMS.Int64

	// 2) 分位：**有界采样**（v0.31.59，审查 F13）。
	//
	// 取最近 usagePerfMaxSample 条（ORDER BY id DESC），再在内存里排序算分位。
	// 为什么不取"最小的 N 条"（ORDER BY latency ASC LIMIT N）：那会把长尾全部裁掉，
	// p95 会系统性偏低 —— 而长尾正是这张报表要回答的问题。
	sampleArgs := make([]any, 0, len(args)+1)
	sampleArgs = append(sampleArgs, args...)
	sampleArgs = append(sampleArgs, usagePerfMaxSample)
	if p.LatencySampled > 0 {
		rows, err := uq.db.Query(
			`SELECT latency_ms FROM usage_logs`+where+` AND latency_ms > 0
			 ORDER BY id DESC LIMIT ?`, sampleArgs...)
		if err != nil {
			return p, err
		}
		capHint := p.LatencySampled
		if capHint > usagePerfMaxSample {
			capHint = usagePerfMaxSample
		}
		vals := make([]int64, 0, capHint)
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
		sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
		p.P50LatencyMS = Percentile(vals, 0.50)
		p.P95LatencyMS = Percentile(vals, 0.95)
	}

	// 3) 缓存聚合（只算上游回报过的行）
	if err := uq.db.QueryRow(
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

// UsageByModelScoped 同上，可限定 scope。
func (uq *Queries) UsageByModelScoped(userID int64, days int, scope string) ([]UsageModelStat, error) {
	if days <= 0 || days > 365 {
		days = 14
	}
	since := LocalDayStart(days)
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
	rows, err := uq.db.Query(query, args...)
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

// ---------- 代挂收益聚合（host_earnings，号池返点） ----------
//
// 与 usage_logs 聚合同属「报表读取侧」：只读、只碰 uq.db、错了不会崩只会给出错误数字。
// v0.31.13 从 cmd/webapp 的 Store 搬到这里，理由与第二刀相同 —— 独立包能用临时 SQLite
// 直接钉住"哪些行该被算进去"（含「汇总口径是全时段、按日/按模型才有窗口」这个易错点）。
//
// 边界：仍然**只读**。写 host_earnings（号主分成入账）留在 cmd/webapp 的结算路径里 ——
// 写入要参与预扣/结算事务，与报表读取不是一件事。

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

// HostEarningsDaily 某号主的号池收益按本地自然日聚合近 days 天（按日升序，便于直接画折线）。
func (uq *Queries) HostEarningsDaily(ownerUserID int64, days int) ([]HostEarnDailyStat, error) {
	if days <= 0 || days > 365 {
		days = 14
	}
	off := LocalDayOffset()
	since := LocalDayStart(days)
	rows, err := uq.db.Query(
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
func (uq *Queries) HostEarningsByModel(ownerUserID int64, days int) ([]HostEarnModelStat, error) {
	if days <= 0 || days > 365 {
		days = 14
	}
	since := LocalDayStart(days)
	rows, err := uq.db.Query(
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

// HostEarningSummary 某号主的收益汇总：总入账微点 + 笔数。
//
// ⚠️ 口径与上面两个**不同**：本方法是**全时段**累计（没有 days 参数、不带 created_at 过滤）。
// 「进号池」块的 total/count 给的就是这个累计值，而 daily/models 是窗口值 —— 二者不等是
// 设计如此（用户看"一共赚了多少"时不该被 days 参数影响）。**别顺手给它加窗口**。
func (uq *Queries) HostEarningSummary(ownerUserID int64) (totalMicro int64, count int64, err error) {
	err = uq.db.QueryRow(`SELECT COALESCE(SUM(earned_micro),0), COUNT(*) FROM host_earnings WHERE owner_user_id = ?`,
		ownerUserID).Scan(&totalMicro, &count)
	return
}

// ---------- 本地日界与分位 ----------

// LocalDayOffset 本地时区偏移（秒，东为正）。
// 用它把 UTC 秒换算成「本地自然日」序号，避免依赖 SQLite 的 localtime 修饰符
// （modernc 驱动是否带 tzdata 取决于构建，不赌）。
func LocalDayOffset() int64 {
	_, off := time.Now().Zone()
	return int64(off)
}

// LocalDayStart 返回近 days 天窗口的**最早本地日界**（unix 秒）。
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
func LocalDayStart(days int) int64 {
	if days < 1 {
		days = 1
	}
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	return start.AddDate(0, 0, -(days - 1)).Unix()
}

// Percentile 取已排序切片的 q 分位值（聚合统计用；空切片返回 0）。
func Percentile(sorted []int64, quantile float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)) * quantile)
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
