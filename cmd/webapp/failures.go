// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// failures.go 「消耗失败」台账 + 错误码处置策略注册表（v0.26.3）。
//
// ## 这个文件解决什么
//
// 一次请求的「消耗」在链路上会经过四个阶段，每个阶段都可能失败：
//
//	reserve   预扣（余额不足 / 存储抖动）
//	upstream  上游拒绝（带业务码或状态码）—— 最常见，也是唯一有"处置"的一环
//	settle    预扣结算（找不到预扣记录等）
//	charge    事后扣费（无预扣的退化路径）
//
// 在 v0.26.3 之前，这些失败的去处是：一条 `usage_logs.error_msg` 文本、一行
// 日志、以及**散落在 switch 分支里的处置代码**。三个问题：
//
//  1. **问不出"最近为什么在失败"** —— error_msg 是自由文本拼出来的，
//     `LIKE '%settle_failed%'` 之外没有结构化维度，按错误码统计做不到。
//  2. **处置与语义分离** —— "6004 是模型级限额、11128 是渠道拒绝、11140 要累计
//     三次"这些知识只存在于代码注释里，运维看不到，改一处容易漏另一处。
//  3. **失败过的请求与失败的账号混在一起** —— 一个请求失败可能换号重试成功，
//     stats 上看不出来，只有翻 error_msg 才知道"这一笔其实跌跌撞撞"。
//
// 本文件把两件事分开做清楚：
//
//	**记录**：consume_failures 表，结构化落四要素（阶段 / 错误码 / 上下文 / 时间），
//	        带保留策略（默认 5000 行，超出丢最旧），管理接口可查、可按码/阶段/窗口过滤。
//	**策略**：failurePolicies() 声明式注册表 —— 每个错误码「是什么 / 罚谁 / 怎么恢复 /
//	        要不要人工」，是**唯一事实源**。界面直接展示这张表（运维据此理解处置），
//	        并由测试强制"声明了就必须有处置分支"（见 failures_test.go）。
//
// ## 扩展方式（新增一个错误码的完整步骤）
//
//  1. 在 failurePolicies() 里加一行（Key / Code / Scope / Handling / AutoRecover）；
//  2. 在 cmd/webapp/proxy.go 的 reportUpstreamError 里加对应的处置分支；
//  3. 跑 `go test ./cmd/webapp/ -run TestFailurePolicies` —— 它会按注册表逐码构造
//     合成响应、断言"处置方向与声明一致"，只做第 1 步会红。
//
// 第 3 步是这套机制的关键：没有它，注册表就只是一份会过期的文档。
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/upstream"
)

// ---------------------------------------------------------------------------
// 阶段
// ---------------------------------------------------------------------------

const (
	// FailStageReserve 预扣阶段（余额不足 / 存储抖动）。
	FailStageReserve = "reserve"
	// FailStageUpstream 上游拒绝（带业务码或状态码）。
	FailStageUpstream = "upstream"
	// FailStageSettle 预扣结算阶段。
	FailStageSettle = "settle"
	// FailStageCharge 事后扣费阶段（无预扣的退化路径）。
	FailStageCharge = "charge"
)

// ---------------------------------------------------------------------------
// 处置范围与策略注册表
// ---------------------------------------------------------------------------

// 处置范围：决定"这次错误罚谁"，也是测试断言处置方向的依据。
const (
	// ScopeRequest 请求级 —— 是"这次请求"的问题，不罚账号（但会轮转换号）。
	ScopeRequest = "request"
	// ScopeModel 模型级 —— 罚 (账号, 模型) 这一个组合，切模型即可用。
	ScopeModel = "model"
	// ScopeAccount 账号级 —— 罚整个账号（冷却 / 停用 / 摘除）。
	ScopeAccount = "account"
	// ScopeGlobal 上游全局 —— 与单个账号无关（5xx / IP 级 WAF），只做轻量降权。
	ScopeGlobal = "global"
)

// FailurePolicy 一个错误码的处置声明。
//
// 字段刻意只放**运维需要知道的东西**（是什么 / 罚谁 / 怎么恢复），不放实现细节：
// 实现细节在代码里，运维看不到也不该依赖。
type FailurePolicy struct {
	// Key 唯一标识：业务码用十进制字符串（"6004"），状态码族用 "http:429"。
	Key string `json:"key"`
	// Code 上游业务码；0 = 无业务码（按 HTTP 状态码分流）。
	Code int `json:"code,omitempty"`
	// Name 一句话说明这个码在上游是什么意思。
	Name string `json:"name"`
	// Stage 该策略作用于哪个阶段（本注册表全部为 FailStageUpstream。
	// 保留字段是为了让"预扣/结算"这类纯本地失败将来也能进同一张表）。
	Stage string `json:"stage"`
	// Kind 对应的 upstream.ErrKind（供交叉核对；空 = 无对应分类）。
	Kind string `json:"kind,omitempty"`
	// Scope 见 ScopeRequest / ScopeModel / ScopeAccount / ScopeGlobal。
	Scope string `json:"scope"`
	// Handling 处置动作（给运维看的人话）。
	Handling string `json:"handling"`
	// AutoRecover 是否会**自动恢复**。false = 必须人工介入（重登 / 补线路 / 复活）。
	// 这个字段的价值在于它把"等一会儿就好"与"等下去不会好"分开 ——
	// 界面上这两类的处置方向完全相反。
	AutoRecover bool `json:"auto_recover"`
	// Note 需要额外说明的取舍（为空则不展示）。
	Note string `json:"note,omitempty"`
}

// failurePolicies 错误码处置的**声明式唯一事实源**。
//
// 顺序即界面展示顺序：按"最严重 / 最需要人工"在前排，与运维的阅读优先级一致
// （先看必须动手的，再看会自动恢复的）。
//
// 每一条都必须有对应的处置分支，由 TestFailurePoliciesHaveDispatch 强制。
func failurePolicies() []FailurePolicy {
	return []FailurePolicy{
		{
			Key: "11128", Code: 11128, Name: "渠道 / 客户端指纹级拒绝", Stage: FailStageUpstream,
			Kind: upstream.ErrChannelDenied.String(), Scope: ScopeRequest,
			Handling:    "零动作（不冷却 / 不熔断 / 不计数），但仍轮转换号",
			AutoRecover: true,
			Note: "上游对 11128 与 11140 复用同一句 msg（request illegal），必须按 code 判、且判在 11140 之前；" +
				"只看文案会把渠道级问题读成账号级故障并错杀健康号（2026-10-13 批量误杀同类成因）。",
		},
		{
			Key: "11140", Code: 11140, Name: "请求非法（风控 / 频率 / 出口）", Stage: FailStageUpstream,
			Kind: upstream.ErrAccountFault.String(), Scope: ScopeAccount,
			Handling:    "专用 2h 软冷却 + 连续计数，满 3 次才按「删除 or 停用归档」处置",
			AutoRecover: true,
			Note: "冷却基数取 2h 而不是 60s：60s 会让 3 次阈值在几分钟内凑满，" +
				"把一个凭证与余额完好的健康号摘掉（2026-10-13 实测批量误杀 30 个号的成因）。",
		},
		{
			Key: "12153", Code: 12153, Name: "会话失效（需重新登录）", Stage: FailStageUpstream,
			Kind: upstream.ErrSessionDead.String(), Scope: ScopeAccount,
			Handling:    "连续计数，满阈值后停用并归档",
			AutoRecover: false,
			Note:        "短冷却救不活失效 session；误判为限流会让该死号留在池中反复被选中。",
		},
		{
			Key: "14017", Code: 14017, Name: "试用未开通 / 权限异常", Stage: FailStageUpstream,
			Kind: upstream.ErrAccountFault.String(), Scope: ScopeAccount,
			Handling:    "短软冷却 + 立即归档异常组（**不自动回归**）",
			AutoRecover: false,
			Note: "它不是「等一等就好」而是必须重新登录 / 重新激活；标成可自动回归会造成" +
				"「归档 ↔ 回归」循环，每次白耗一次选号（v0.23.1 修正）。",
		},
		{
			Key: "14020", Code: 14020, Name: "积分耗尽（业务码）", Stage: FailStageUpstream,
			Kind: upstream.ErrHardCredit.String(), Scope: ScopeAccount,
			Handling:    "冷却到次日 04:00（签到任务恢复）+ 归档异常组（可自动回归）",
			AutoRecover: true,
		},
		{
			Key: "http:402", Name: "积分耗尽（状态码）", Stage: FailStageUpstream,
			Kind: upstream.ErrHardCredit.String(), Scope: ScopeAccount,
			Handling:    "同 14020",
			AutoRecover: true,
		},
		{
			Key: "6004", Code: 6004, Name: "模型级用量超限", Stage: FailStageUpstream,
			Kind: upstream.ErrSoftRate.String(), Scope: ScopeModel,
			Handling:    "只冷却该 (账号, 模型)：对齐上游重置墙钟，无时间信息则有界退避",
			AutoRecover: true,
			Note: "账号健康、只是这个模型此刻被限，切模型立即可用；" +
				"**绝不写账号级冷却**（否则一次没带响应头的 6004 会把整个健康号挡掉）。",
		},
		{
			Key: "http:429", Name: "账号级限流", Stage: FailStageUpstream,
			Kind: upstream.ErrSoftRate.String(), Scope: ScopeAccount,
			Handling:    "账号级软冷却：有上游重置时刻则对齐，否则有界退避（base 起翻倍、封顶）",
			AutoRecover: true,
			Note:        "冷却中的兜底探测不再翻倍堆加 —— 这正是「全池被推到封顶」的旧成因。",
		},
		{
			Key: "11102", Code: 11102, Name: "该后端无此模型", Stage: FailStageUpstream,
			Kind: upstream.ErrModelBlocked.String(), Scope: ScopeModel,
			Handling:    "(账号, 模型) 负缓存 6h 起指数退避、封顶 24h；切模型即可用",
			AutoRecover: true,
			Note:        "只认 400/404：429 + 11102 属限流语义，不得被「模型不存在」掩盖。",
		},
		{
			Key: "http:403", Name: "WAF 拦截（无业务信封）", Stage: FailStageUpstream,
			Kind: upstream.ErrWafBlock.String(), Scope: ScopeGlobal,
			Handling:    "账号级软冷却 + IP 级 fail-fast（多号接连命中则终止轮转）",
			AutoRecover: true,
			Note: "继续换号只会把钱放大 N 倍去打同一个出口 IP，加重风控 —— " +
				"所以命中 IP 级状态后停止轮转而不是继续重试。",
		},
		{
			Key: "http:404", Name: "上游路径缺失", Stage: FailStageUpstream,
			Kind: upstream.ErrNotFound.String(), Scope: ScopeAccount,
			Handling:    "固定短冷却（不随限流退避升级）",
			AutoRecover: true,
			Note:        "偶发路径缺失不是限流信号，按限流惩罚升级会白扣账号。",
		},
		{
			Key: "http:5xx", Name: "上游服务故障", Stage: FailStageUpstream,
			Kind: upstream.ErrServer.String(), Scope: ScopeGlobal,
			Handling:    "连败降权（10min 级），不熔断",
			AutoRecover: true,
			Note: "刻意**不**按熔断（30min 起、封顶 6h）处置：上游 5xx 多为全局故障，" +
				"按熔断会把整池账号批量摘除（同类项目实测「4 小时 196 次失败熔断 13 个号」）。",
		},
		{
			Key: "consume:reserve", Name: "预扣被拒（余额不足）", Stage: FailStageReserve,
			Scope:       ScopeRequest,
			Handling:    "拒绝请求（402），不扣费、不罚账号",
			AutoRecover: true,
			Note:        "用户充值后即可用；账号与上游无关，不该受影响。",
		},
		{
			Key: "consume:settle", Name: "预扣结算失败", Stage: FailStageSettle,
			Scope:       ScopeAccount,
			Handling:    "留痕并释放预扣（不静默吞掉），该笔实际未扣费",
			AutoRecover: true,
			Note:        "方向对用户有利（少扣），但**必须留痕**：否则账目对不上时无从追查。",
		},
		{
			Key: "consume:charge", Name: "事后扣费失败", Stage: FailStageCharge,
			Scope:       ScopeAccount,
			Handling:    "留痕（响应已发出，不阻断）",
			AutoRecover: true,
		},
	}
}

// failurePolicyIndex 注册表索引（按 Key）。惰性构建一次：注册表是编译期常量，
// 运行期不会变，重复扫 15 行是纯浪费（同仓库其余注册表的既有做法）。
var failurePolicyIndex = func() map[string]FailurePolicy {
	m := map[string]FailurePolicy{}
	for _, p := range failurePolicies() {
		m[p.Key] = p
	}
	return m
}()

// policyFor 按 Key 取策略（不存在返回零值 + false）。
func policyFor(key string) (FailurePolicy, bool) {
	p, ok := failurePolicyIndex[key]
	return p, ok
}

// codeKeyOf 把「业务码 + 状态码」映射成注册表 Key。
//
// 优先业务码（更具体），无业务码时按状态码族归类。返回空串表示没有对应的声明
// —— 这不是错误：上游随时可能冒出新码，**未知码照样要被记录**（那正是台账的意义），
// 只是它在界面上会以"未声明处置"的形式出现，提示运维补一行注册表。
func codeKeyOf(code, status int) string {
	if code > 0 {
		k := strconv.Itoa(code)
		if _, ok := policyFor(k); ok {
			return k
		}
		return k // 未声明也返回业务码本身，便于按码聚合排查
	}
	switch {
	case status == http.StatusPaymentRequired:
		return "http:402"
	case status == http.StatusTooManyRequests:
		return "http:429"
	case status == http.StatusForbidden:
		return "http:403"
	case status == http.StatusNotFound:
		return "http:404"
	case status >= 500:
		return "http:5xx"
	}
	return ""
}

// resetSrcSuffix 把「冷却时长的来源」拼进 reason（可读、可 grep）。
//
// 为什么值得单独标出来：同一个 6004，"对齐到上游明示的重置时刻"与"本地估算的
// 有界退避"在排障时是完全不同的结论 —— 前者说明上游确实在限这个模型，后者
// 说明我们连上游说的时刻都没读到（可能解析失效），后者才是要查的。
func resetSrcSuffix(src upstream.ResetSource) string {
	switch src {
	case upstream.ResetBodyText:
		return " [reset=body-text]"
	case upstream.ResetBodyJSON:
		return " [reset=body-json]"
	case upstream.ResetHeader:
		return " [reset=header]"
	case upstream.ResetExpired:
		return " [reset=expired→退避]"
	default:
		return " [reset=none→退避]"
	}
}

// ---------------------------------------------------------------------------
// 台账：结构 + 落库 + 查询
// ---------------------------------------------------------------------------

// ConsumeFailure 一条消耗失败记录。
//
// 字段设计原则：**只存能从结构上聚合的维度**（阶段 / 码 / 状态 / 账号 / 模型 / 次数），
// 自由文本只留一行 detail 摘要且**必须脱敏**（复用 errsafe.go 的 scrubErrBody，
// 避免上游错误体里带出的内部细节直接落进可查的台账）。
type ConsumeFailure struct {
	ID      int64  `json:"id"`
	At      int64  `json:"at"`
	Stage   string `json:"stage"`
	Code    int    `json:"code,omitempty"`
	CodeKey string `json:"code_key,omitempty"`
	Kind    string `json:"kind,omitempty"`
	// Scope 命中的处置范围（空 = 未声明处置）。
	Scope string `json:"scope,omitempty"`
	// Policy 命中的注册表 Key（与 CodeKey 相同或为空）。
	Policy      string `json:"policy,omitempty"`
	AutoRecover bool   `json:"auto_recover"`
	Status      int    `json:"status,omitempty"`
	Model       string `json:"model,omitempty"`
	UID         string `json:"uid,omitempty"`
	UserID      int64  `json:"user_id,omitempty"`
	KeyID       int64  `json:"key_id,omitempty"`
	Attempt     int    `json:"attempt,omitempty"`
	// Source 调用来源：proxy（转发路径） / settle / charge / reserve / manual。
	Source string `json:"source"`
	Detail string `json:"detail,omitempty"`
	// ResetSource 上游重置时间的来源（仅限流类有意义；空 = 不适用）。
	ResetSource string `json:"reset_source,omitempty"`
	// ClientStatus 这条失败**若成为最终结果**，客户端会收到的 HTTP 状态码。
	//
	// 与 Status 刻意并列（不落库，读取时按 kind 现算）：Status 是**上游给我们的**
	// 状态码（例如模型不存在时上游固定回 400），ClientStatus 是**我们回客户端的**
	// （同一个例子是 404）。实测有人把前者读成后者，据此判断"网关全都回 400" ——
	// 两个数放在一起看，这个歧义就没有了。0 = 不适用（非上游阶段 / 未分类）。
	ClientStatus int `json:"client_status,omitempty"`
}

// migrationConsumeFailures consume_failures 建表语句（由 Store.migrate 调用）。
//
// 索引按**实际查询形态**建，而不是"每个字段都来一个"：
//
//	idx at   —— 按时间窗口列表（最常用）
//	idx code —— 按错误码聚合（"6004 最近有多少次"）
//	idx stage—— 按阶段过滤（"结算失败有多少"）
const migrationConsumeFailures = `CREATE TABLE IF NOT EXISTS consume_failures (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	at           INTEGER NOT NULL DEFAULT 0,
	stage        TEXT NOT NULL DEFAULT '',
	code         INTEGER NOT NULL DEFAULT 0,
	code_key     TEXT NOT NULL DEFAULT '',
	kind         TEXT NOT NULL DEFAULT '',
	scope        TEXT NOT NULL DEFAULT '',
	policy       TEXT NOT NULL DEFAULT '',
	auto_recover INTEGER NOT NULL DEFAULT 0,
	status       INTEGER NOT NULL DEFAULT 0,
	model        TEXT NOT NULL DEFAULT '',
	uid          TEXT NOT NULL DEFAULT '',
	user_id      INTEGER NOT NULL DEFAULT 0,
	key_id       INTEGER NOT NULL DEFAULT 0,
	attempt      INTEGER NOT NULL DEFAULT 0,
	source       TEXT NOT NULL DEFAULT '',
	detail       TEXT NOT NULL DEFAULT '',
	reset_source TEXT NOT NULL DEFAULT ''
)`

// consumeFailureIndexes 台账索引（与建表分开，便于既有库增量补建）。
var consumeFailureIndexes = []string{
	`CREATE INDEX IF NOT EXISTS idx_consume_failures_at ON consume_failures(at)`,
	`CREATE INDEX IF NOT EXISTS idx_consume_failures_code ON consume_failures(code_key)`,
	`CREATE INDEX IF NOT EXISTS idx_consume_failures_stage ON consume_failures(stage)`,
}

// consumeFailureRetention 台账保留行数上限（超出丢最旧）。
//
// ⚠️ 为什么必须有保留策略：这是**每次失败都写一行**的表，而失败率高的时段
// （上游抽风）正是它涨得最快的时候。仓库里 usage_logs 就是因为没有保留策略
// 而无限增长（已记为待处理技术债），不留同样的坑。
//
// 取 5000 而不是"按天"：按天在错误风暴下会瞬间爆炸，而按行数**恒定占空间**，
// 代价只是"太久以前的细节被更近的挤掉" —— 对排障够用，且行为可预测。
const consumeFailureRetention = 5000

// InsertConsumeFailure 写入一条消耗失败记录（并惰性裁旧）。
//
// 落库失败**不阻断主流程**（它本身就是错误路径的旁路记录），但会写日志 ——
// 不静默：台账静默失效比台账缺失更危险（会让人以为"最近没失败过"）。
func (s *Store) InsertConsumeFailure(f *ConsumeFailure) error {
	if s == nil || f == nil {
		return nil
	}
	if f.At == 0 {
		f.At = time.Now().Unix()
	}
	// 富化放在**唯一的写入函数**里，而不是只放在便利包装 recordConsumeFailure 上：
	// 直插（测试、将来的批量导入、巡检脚本）走同一个入口就必然得到一致的
	// code_key/scope/policy —— 否则同一张表里会出现"有的行带处置、有的行不带"，
	// 而界面的统计会静默漏掉后者（最难查的一类不一致）。
	enrichConsumeFailure(f)
	autoRecover := 0
	if f.AutoRecover {
		autoRecover = 1
	}
	res, err := s.db.Exec(
		`INSERT INTO consume_failures
		 (at, stage, code, code_key, kind, scope, policy, auto_recover,
		  status, model, uid, user_id, key_id, attempt, source, detail, reset_source)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		f.At, f.Stage, f.Code, f.CodeKey, f.Kind, f.Scope, f.Policy, autoRecover,
		f.Status, f.Model, f.UID, f.UserID, f.KeyID, f.Attempt, f.Source, f.Detail, f.ResetSource)
	if err != nil {
		return err
	}
	if id, ierr := res.LastInsertId(); ierr == nil {
		f.ID = id
	}
	s.pruneConsumeFailures()
	return nil
}

// pruneConsumeFailures 保留最近 consumeFailureRetention 行。
//
// 用「MAX(id) - N」而不是 `LIMIT ... OFFSET`：单条 DELETE 即可，不需要先查出
// 要删哪些 id；id 是自增主键，删掉的是确定的最旧 N 条。
func (s *Store) pruneConsumeFailures() {
	_, err := s.db.Exec(
		`DELETE FROM consume_failures WHERE id <= (SELECT COALESCE(MAX(id),0) FROM consume_failures) - ?`,
		consumeFailureRetention)
	if err != nil {
		// 裁剪失败不影响刚写入的那条（最多是表稍微大一点），只记日志。
		log.Printf("WARN: [failures] 台账裁剪失败：%v", err)
	}
}

// ListConsumeFailures 按条件查台账（最新在前）。
//
// 过滤参数都是**索引列**（at / code_key / stage），保持查询在错误风暴下也能秒回。
func (s *Store) ListConsumeFailures(limit int, codeKey, stage string, since int64) ([]ConsumeFailure, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT id, at, stage, code, code_key, kind, scope, policy, auto_recover,
	             status, model, uid, user_id, key_id, attempt, source, detail, reset_source
	        FROM consume_failures WHERE 1=1`
	args := []any{}
	if since > 0 {
		q += ` AND at >= ?`
		args = append(args, since)
	}
	if codeKey != "" {
		q += ` AND code_key = ?`
		args = append(args, codeKey)
	}
	if stage != "" {
		q += ` AND stage = ?`
		args = append(args, stage)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]ConsumeFailure, 0, limit)
	for rows.Next() {
		var f ConsumeFailure
		var autoRecover int
		if err := rows.Scan(&f.ID, &f.At, &f.Stage, &f.Code, &f.CodeKey, &f.Kind, &f.Scope,
			&f.Policy, &autoRecover, &f.Status, &f.Model, &f.UID, &f.UserID, &f.KeyID,
			&f.Attempt, &f.Source, &f.Detail, &f.ResetSource); err != nil {
			return nil, err
		}
		f.AutoRecover = autoRecover != 0
		f.ClientStatus = clientStatusOf(f.Stage, f.Status, f.Kind)
		out = append(out, f)
	}
	return out, rows.Err()
}

// clientStatusOf 由「阶段 + 上游状态码 + 错误分类」推出**回给客户端**的状态码。
//
// 口径与真实响应严格同源：上游阶段的映射直接复用 `upstreamFailureStatus`
// （ServeChat 末端就是用它），因此台账里这一列不会与线上行为分叉。
// 只有真正能确定的才给值，其余返回 0（面板显示"—"）—— 宁可留空也不猜，
// 猜出来的状态码会让人据此判断"客户端没收到这个码，是不是别处的问题"。
func clientStatusOf(stage string, upstreamStatus int, kind string) int {
	switch stage {
	case FailStageUpstream:
		k := upstream.ParseErrKind(kind)
		if k == upstream.ErrNone {
			return 0 // 未分类：末端会按 ErrNone 走默认分支，但那不是"这条失败"的结论
		}
		return upstreamFailureStatus(k)
	case FailStageReserve:
		// 预扣阶段唯一会回给客户端的确定性错误是余额不足（402）；
		// "预扣失败退化为事后结算"这类记录不代表客户端收到过错误码。
		if upstreamStatus == http.StatusPaymentRequired {
			return http.StatusPaymentRequired
		}
		return 0
	default:
		// settle / charge：结算与扣费失败发生在响应发出之后（或只影响账目），
		// 客户端拿不到对应状态码 —— 这正是它们与上游阶段"处置不同"的体现。
		return 0
	}
}

// ConsumeFailureStat 按 Key 聚合的一行统计。
type ConsumeFailureStat struct {
	CodeKey     string `json:"code_key"`
	Stage       string `json:"stage"`
	Count       int64  `json:"count"`
	LastAt      int64  `json:"last_at"`
	AutoRecover bool   `json:"auto_recover"`
	Scope       string `json:"scope,omitempty"`
	Handling    string `json:"handling,omitempty"`
}

// ConsumeFailureStats 按 Key 聚合统计（含未声明处置的码 —— 那正是要看见的）。
func (s *Store) ConsumeFailureStats(since int64) ([]ConsumeFailureStat, error) {
	q := `SELECT code_key, stage, COUNT(*), MAX(at)
	        FROM consume_failures WHERE at >= ?
	       GROUP BY code_key, stage ORDER BY COUNT(*) DESC, code_key`
	rows, err := s.db.Query(q, since)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]ConsumeFailureStat, 0, 16)
	for rows.Next() {
		var st ConsumeFailureStat
		if err := rows.Scan(&st.CodeKey, &st.Stage, &st.Count, &st.LastAt); err != nil {
			return nil, err
		}
		// 把注册表里的处置"贴"到统计行上：运维看统计时**同时**看到该怎么处置，
		// 不必再去翻另一张表对照（这是把策略做成声明式的直接收益）。
		if pol, ok := policyFor(st.CodeKey); ok {
			st.AutoRecover = pol.AutoRecover
			st.Scope = pol.Scope
			st.Handling = pol.Handling
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// CountConsumeFailures 台账总行数（界面用来说明"是否已满/被裁剪"）。
func (s *Store) CountConsumeFailures() int64 {
	var n int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM consume_failures`).Scan(&n); err != nil {
		return 0
	}
	return n
}

// ---------------------------------------------------------------------------
// 记录入口
// ---------------------------------------------------------------------------

// recordConsumeFailure 落一条失败记录（自动补全注册表信息与脱敏）。
//
// 挂在 Store 上而不是 app/Proxy 上：两个调用方都有 store（Proxy 有 p.store、
// app 有 a.store），少一层包装少一处漂移（同 globalDeviceToken 的理由）。
//
// 为什么在这里统一补 CodeKey/Scope/Policy/AutoRecover 而不是让调用方填：
// 调用方只该回答"发生了什么"（阶段 / 码 / 状态 / 上下文），"这意味着什么"
// 是注册表的职责。分散填写迟早出现"同一码两处 scope 不一致"。
// enrichConsumeFailure 补齐注册表派生的字段（CodeKey / Policy / Scope / AutoRecover / Kind）。
//
// 原则：调用方只回答"发生了什么"（阶段 / 码 / 状态 / 上下文），
// "这意味着什么"是注册表的职责。分散填写迟早出现"同一码两处 scope 不一致"。
func enrichConsumeFailure(f *ConsumeFailure) {
	if f == nil {
		return
	}
	if f.CodeKey == "" {
		f.CodeKey = codeKeyOf(f.Code, f.Status)
	}
	if pol, ok := policyFor(f.CodeKey); ok {
		f.Policy = pol.Key
		f.Scope = pol.Scope
		f.AutoRecover = pol.AutoRecover
		if f.Kind == "" && pol.Kind != "" {
			f.Kind = pol.Kind
		}
	}
}

func (s *Store) recordConsumeFailure(f ConsumeFailure) {
	if s == nil {
		return
	}
	if f.Source == "" {
		f.Source = "proxy"
	}
	// detail 一律脱敏 + 截断：它是**可查询**的自由文本，上游错误体里可能夹带
	// 内部细节（栈、SQL 特征串），与 writeJSON 的唯一出口同一口径（见 errsafe.go）。
	// 注意用的是 looksInternalErr（文本级判定）而不是 scrubErrBody（那个收 map）——
	// 台账里的 detail 是拼好的一行摘要，不是 JSON 信封。
	if f.Detail != "" {
		f.Detail = truncateRunes(f.Detail, 240)
		if looksInternalErr(f.Detail) {
			f.Detail = scrubErrTextSafe
		}
	}
	if err := s.InsertConsumeFailure(&f); err != nil {
		log.Printf("WARN: [failures] 落库失败 stage=%s code=%d uid=%s：%v",
			f.Stage, f.Code, f.UID, err)
	}
}

// summary 一行摘要（日志用；不含 detail 全文，避免日志被错误体淹没）。
func (f ConsumeFailure) summary() string {
	return fmt.Sprintf("stage=%s key=%s code=%d status=%d scope=%s auto=%v uid=%s model=%s",
		f.Stage, f.CodeKey, f.Code, f.Status, f.Scope, f.AutoRecover, f.UID, f.Model)
}

// ---------------------------------------------------------------------------
// 管理接口
// ---------------------------------------------------------------------------

// handleConsumeFailures 消耗失败台账：统计 + 明细 + **处置策略表**。
//
// 把策略表一起返回是刻意的：运维打开这一页要回答的是"为什么在失败、我该做什么"，
// 只给一堆记录而不给处置说明，等于把答案留在代码注释里。
//
// 参数：limit（1..500，默认 100）/ code（注册表 Key，如 6004、http:429）/ stage / hours（默认 24）。
func (a *app) handleConsumeFailures(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := atoiDefault(q.Get("limit"), 100)
	hours := atoiDefault(q.Get("hours"), 24)
	if hours <= 0 || hours > 24*30 {
		hours = 24
	}
	since := time.Now().Add(-time.Duration(hours) * time.Hour).Unix()
	codeKey := strings.TrimSpace(q.Get("code"))
	stage := strings.TrimSpace(q.Get("stage"))

	rows, err := a.store.ListConsumeFailures(limit, codeKey, stage, since)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	stats, err := a.store.ConsumeFailureStats(since)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// 注册表整体回给界面（不只是命中的那些）：界面要能说明"我们声明了哪些码"，
	// 这样"某个码没出现在统计里"才与"某个码没被声明"区分得开。
	writeJSON(w, http.StatusOK, map[string]any{
		"window_hours": hours,
		"since":        since,
		"limit":        limit,
		"filter":       map[string]any{"code": codeKey, "stage": stage},
		"total":        a.store.CountConsumeFailures(),
		"retention":    consumeFailureRetention,
		"rows":         rows,
		"stats":        stats,
		"policies":     failurePolicies(),
	})
}

// requestShape 请求的**形状**摘要（不含任何内容）。
//
// 为什么需要：排障时"失败的请求长什么样"是第一个要回答的问题，而形状
// （消息条数 / 是否流式 / 有无工具定义）足以把「上下文超长」和「普通请求被拦」
// 分开。刻意**不**落提示词原文 —— 与本仓"请求体内容不留存"的承诺一致
// （唯一例外是被拦样本回流，见 fpsample.go，且它默认关闭）。
//
// 解析失败（非 JSON / 空体）返回 "未知"，不报错：台账记录不该因为请求体畸形而失败。
func requestShape(body []byte) string {
	if len(body) == 0 {
		return "空请求体"
	}
	var obj struct {
		Stream   bool              `json:"stream"`
		Messages []json.RawMessage `json:"messages"`
		Tools    []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return fmt.Sprintf("非 JSON（%d 字节）", len(body))
	}
	parts := []string{fmt.Sprintf("messages=%d", len(obj.Messages))}
	if obj.Stream {
		parts = append(parts, "stream")
	}
	if n := len(obj.Tools); n > 0 {
		parts = append(parts, fmt.Sprintf("tools=%d", n))
	}
	return strings.Join(parts, " ")
}

// firstLine 取文本的第一行（已压缩空白），用于把多行错误体塞进一行摘要。
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(空响应体)"
	}
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = truncateRunes(s, 160)
	}
	return s
}

// atoiDefault 宽松解析查询参数（非法/缺失 → 默认值）。
//
// 刻意不报错：这些是**只读查询**的过滤参数，参数写错时给默认值比给 400 更有用
// （运维在浏览器地址栏里手改参数时不该被拦住）。
func atoiDefault(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n > 0 {
		return n
	}
	return def
}

// RecentOKModels 近期**成功**服务过的模型名（去重，最多 limit 个）。
//
// 供模型名归一化做启动种子化（见 modelname_seed.go）：它要的是一份
// "上游认得的写法"清单，而这正是 status='ok' 的语义。
// 失败行刻意排除：失败记录里的模型名可能是写法错的。
func (s *Store) RecentOKModels(limit int) ([]string, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.db.Query(
		`SELECT DISTINCT model FROM usage_logs
		 WHERE status = 'ok' AND model <> '' ORDER BY model LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
