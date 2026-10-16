// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// healthcheck.go 号池账号健康检测（v0.23.0）。
//
// 需求：明确「账号是否正常」的判定 —— 覆盖被封禁 / 受限 / 异常 / 登录失效 /
// 权限不足等状态，说明触发时机、判定优先级、返回形式，以及检测通过前后的流程。
//
// ===========================================================================
// 一、探测深度：**轻量探活**（一次幂等上游请求，不消耗积分）
// ===========================================================================
//
// 探测调用 `get-user-resource`（billing/meter 域，即「拉余额」接口）。选它的理由：
//
//	· **幂等且不消耗积分** —— 与真实 chat 请求不同，不会因为「检测」本身烧掉额度；
//	· **覆盖面广** —— 一次请求即能验出：token 是否还有效（12153 登录失效）、
//	  账号是否被封（11140）/ 被 WAF 拦（403）、权限或试用是否开通（14017）、
//	  余额是否耗尽（402/14020）；
//	· **与既有链路同源** —— creditsync.go 早已用它做「拉余额」，故本次把
//	  「探测」与「刷新余额」**合并为同一次出站**（见 refreshCredits），
//	  导入新号后的自动检测不会额外产生请求。
//
// **明确的能力边界**：本检测只能证明「账号可用」（凭证 + billing 通道健康），
// **不能**证明「chat 推理通道健康」。后者由池内既有的熔断器（连续 5xx 信号）
// 覆盖 —— 两套机制正交，刻意不重复：真发 chat 探测会消耗积分，且高频检测本身
// 就是一个风控特征。
//
// ===========================================================================
// 二、需要检测的账号状态范围（本地前置，不出站）
// ===========================================================================
//
// 判定前先看本地状态，两类立即定论、**不消耗上游请求**：
//
//	· 手动停用（ManualDisabled）→ skipped。这是运维的明确意图，账号被刻意摘除，
//	  探测它没有意义（还会打扰运维）。
//	· 凭证不可用（无 AccessToken）→ abnormal(auth_missing)。本地就能定论，
//	  发出去也只会失败。
//
// 反之，**系统自动禁用（Disabled）的账号仍要探测** —— 这是刻意的：池内注释明确记录
// 「13 个 disabled 号全部 refresh 成功，是历史误判的受害者」，12153 会被网络抖动
// 临时触发。若不探测，被误判的号永远躺在禁用里没人发现。探测通过 → 判 degraded 并
// 提示「可考虑复活」，把决定权交回运维（本检测不自动复活，见下）。
//
// ===========================================================================
// 三、判定优先级（三级漏斗，从高到低）
// ===========================================================================
//
//	第一级 · 本地前置（不出站）
//	  ① ManualDisabled            → skipped  （运维手动停用）
//	  ② 凭证不可用                 → abnormal （原因 auth_missing）
//
//	第二级 · 探测结论（出站，按 upstream.Classify 的 ErrKind 映射）
//	  ③ Kind 决定异常类型与「确定性」（见下表）
//
//	第三级 · 本地补充（仅在第一/二级未定论、即探测成功时）
//	  ④ 余额 == 0                  → abnormal （受限：无额度可服务）
//	  ⑤ 有降权痕迹（冷却/熔断/连降权/曾被禁用）→ degraded（凭证正常，当前不可服务）
//	  ⑥ 其余                       → healthy  （正常）
//
// **为什么「探测成功」后还要看本地痕迹**：探测证明的是「凭证与 billing 通道可用」，
// 而熔断/降权记录的是「chat 通道连续失败」。两者独立 —— 一个正在熔断的号，拉余额
// 会成功，但它此刻**不能被用来服务请求**。若只看探测就判「正常」，运维会以为它
// 在正常工作。故降权中一律判 degraded（可用但有伤），而非 healthy。
//
// ===========================================================================
// 四、判定结果（五态）与自动处置矩阵
// ===========================================================================
//
// 五态：healthy 正常 / degraded 降权中 / abnormal 异常 / uncertain 不确定 / skipped 已跳过
//
// 「确定性」是自动处置的**唯一闸门**：只有「证据指向账号本身」的结论才自动处置；
// 一切可能由出口、上游或临时状况引起的结果一律判 uncertain 且**只报告不处置**
// （避免一次探测的偶发抖动就改生产账号状态）。
//
//	ErrKind             结论         确定性  自动处置（复用既有原语）
//	────────────────────────────────────────────────────────────────────
//	ErrNone             见第三级     —       清会话失效计数（账号被证明未死）
//	ErrSessionDead      登录失效     ✅      NoteSessionDead（**累计阈值**后禁用）
//	ErrAccountFault     封号/权限    ✅      11140 → Disable + 归档异常
//	                                        （14017 → 软冷却 + 归档异常，可自动回归）
//	ErrHardCredit       余额耗尽     ✅      硬冷却到次日 4:00 + 归档异常（可自动回归）
//	ErrWafBlock         出口被拦     ❌      只报告（是出口 IP 的问题，不是账号）
//	ErrSoftRate         限流中       ❌      只报告（临时状态，自然恢复）
//	ErrNotFound         上游偶发     ❌      只报告（404 与账号无关）
//	ErrServer           上游故障     ❌      只报告（全局故障，不罚账号）
//	ErrClient / 其他    未分类       ❌      只报告（信息不足，不擅自判罪）
//	传输错误（超时/DNS）不确定       ❌      只报告（网络问题）
//
// 处置动作**全部复用既有原语**（pool.Disable / Cooldown / archiveAbnormal /
// NoteSessionDead），与 proxy.go 的 reportUpstreamError 是同一套处置语义 —— 本模块
// 不新造处置路径。两处若需调整应同步（有 TestHealthVerdictMatrix 锁定分类口径）。
//
// 12153 登录失效刻意**不**直接 Disable，而是走 NoteSessionDead 的累计阈值 ——
// 与既有链路同纪律：一次 12153 可能是网络抖动 / refresh 竞态，一次就杀号会误杀。
//
// ===========================================================================
// 五、触发时机与调用场景
// ===========================================================================
//
//	① 手动 · 单账号   POST /api/admin/accounts/{uid}/health  → **同步**返回该号结果
//	   （1 次上游请求，秒级；运维在账号卡片上点「检测」）
//	② 手动 · 批量     POST /api/admin/accounts/health          → **异步**受理
//	   body {"uids":[...]} 省略/空 = 全量候选；结果进内存缓存，用 ③ 取回。
//	   为什么异步：N 个账号 × 每个一次上游请求 × 账号间错峰 250~600ms，同步必超时
//	   （与 creditsync 的手动刷新同一理由、同一范式）。
//	③ 取批量结果      GET  /api/admin/accounts/health/last     → 最近一次结果 + 汇总
//	④ 导入新号后自动  复用 refreshCredits 的异步批（**合并同一次出站**，不额外请求）
//
// **刻意不做周期自动巡检**：持续性的周期性出站流量（账号数 × 频率）本身就是一个
// 显著特征，收益（早点发现悄悄失效的号）不值这个暴露面。手动 + 导入时点已覆盖
// 「新号上线」与「怀疑有问题」两个真正需要检测的场景。
//
// ===========================================================================
// 六、检测通过前后的后续流程
// ===========================================================================
//
//	通过前（异常）：
//	  确定性异常 → 立即自动处置（见上表），账号随即退出选号（禁用/冷却/归档），
//	  不需要运维再点一次；结果里 reason/action 会写明「做了什么」。
//	  不确定异常 → 仅报告。运维据 reason 自行判断（出口问题该换 IP、上游故障该等），
//	  不做任何状态变更。
//
//	通过后（正常 / 降权中）：
//	  ① 清会话失效计数（ClearSessionDead）—— 探测成功即证明账号未死，与既有纪律
//	     「被证明未死的任何时刻都清计数」一致；
//	  ② 刷新池内权威余额 + 快过期分桶（SetCreditsDetailed）+ 按余额解冻
//	     （ReenableIfCredits）+ 即时重算预备组（reconcilePoolGroupOne）——
//	     与 creditsync 的收尾口径逐字一致；
//	  ③ 不动分组、不自动复活、不自动解除运维停用。检测是「体检」，不是「治疗」：
//	     诊断结论交给人，处置只做确定性、可逆、与既有链路同语义的那部分。
package main

import (
	"encoding/json"
	"errors"
	"log"
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// ---------- 五态 ----------

// 判定状态。healthy 与 abnormal 是需求要求的「正常 / 异常」主标识，
// degraded 与 uncertain 是必要的细化（可用但有伤 / 无法定论）。
const (
	hcHealthy   = "healthy"   // 正常
	hcDegraded  = "degraded"  // 降权中（凭证正常，当前不参与选号）
	hcAbnormal  = "abnormal"  // 异常（确定性，已/应处置）
	hcUncertain = "uncertain" // 不确定（探测失败，无法定论，不处置）
	hcSkipped   = "skipped"   // 已跳过（运维手动停用）

	// 结构化「可执行动作」（v0.23.4）：给前端一个**机器可读**的入口，
	// 而不是让它去匹配中文原因文案 —— 文案会随迭代改，匹配会静默失效
	// （本项目已吃过这类亏：前端按字符串判断状态，后端改文案后判断恒假）。
	//
	//	enable —— 运维手动停用：解除即可恢复
	//	revive —— 曾被系统禁用但本次探测通过：清系统禁用位即可复活
	//
	// 其余状态（含封号 / 权限异常等**需要重新登录**的）一律不给动作：
	// 一键按钮解决不了它们，给了只会误导运维以为点一下就好。
	hcActionEnable = "enable"
	hcActionRevive = "revive"
)

// hcStateLabel 状态的中文名（前端直接展示）。
func hcStateLabel(s string) string {
	switch s {
	case hcHealthy:
		return "正常"
	case hcDegraded:
		return "降权中"
	case hcAbnormal:
		return "异常"
	case hcUncertain:
		return "不确定"
	case hcSkipped:
		return "已跳过"
	default:
		return s
	}
}

// hcStateRank 排序权重：越严重的排越前（异常 > 不确定 > 降权 > 跳过 > 正常）。
// 运维打开检测结果时最该先看到「有问题的」，而不是被一排「正常」淹没。
func hcStateRank(s string) int {
	switch s {
	case hcAbnormal:
		return 0
	case hcUncertain:
		return 1
	case hcDegraded:
		return 2
	case hcSkipped:
		return 3
	default:
		return 4
	}
}

// ---------- 原因码（机器可读；reason 是给人看的中文） ----------

const (
	hcCodeOK             = "ok"                 // 正常
	hcCodeManualDisabled = "manual_disabled"    // 运维手动停用（跳过）
	hcCodeAuthMissing    = "auth_missing"       // 本地凭证不可用
	hcCodeSessionDead    = "session_dead"       // 12153 登录失效
	hcCodeRequestIllegal = "request_illegal"    // 11140 request illegal（请求非法；**不是**账号封禁，v0.24.3）
	hcCodeChannelDenied  = "channel_denied"     // 11128 渠道/客户端指纹拒绝（请求级，与账号无关，v0.24.8）
	hcCodeRestricted     = "account_restricted" // 上游明说「账号存在异常行为，已被限制使用」（账号级终态，v0.24.6）
	hcCodeAccountFault   = "account_fault"      // 14017 账号故障 / 试用未开通
	hcCodeNoCredits      = "no_credits"         // 余额耗尽
	hcCodeWafBlock       = "waf_block"          // 403 WAF 拦截
	hcCodeRateLimited    = "rate_limited"       // 429 限流
	hcCodeUpstream5xx    = "upstream_error"     // 5xx 上游故障
	hcCodeUpstream404    = "upstream_not_found" // 404 上游偶发
	hcCodeTransport      = "transport_error"    // 网络/超时（传输层）
	hcCodeUnclassified   = "unclassified"       // 其他 4xx / 未分类
	hcCodeDegraded       = "degraded_local"     // 本地降权痕迹
	hcCodeZeroRemain     = "zero_credits"       // 探测成功但余额为 0
	hcCodeProbeThrottled = "probe_throttled"    // 未探活：被探活总闸限流（并发满 / 30s 内重复）
)

// ---------- 处置动作（机器可读） ----------

const (
	hcActNone         = "none"                    // 无动作
	hcActCleared      = "cleared_dead"            // 清了会话失效计数
	hcActClearedFault = "cleared_request_illegal" // 清了「请求非法」（11140）累计计数（v0.24.3）
	hcActNoteDead     = "noted_session_dead"      // 记一次会话失效（累计阈值）
	hcActNoteFault    = "noted_request_illegal"   // 记一次「请求非法」（11140，累计阈值，v0.24.3）
	// hcActSkipUnpooledHost 未入池的代挂号：**只报告、不处置**（v0.31.20）。
	hcActSkipUnpooledHost = "skipped_unpooled_host"
	hcActDisabled         = "disabled"       // 已禁用
	hcActDeleted          = "deleted"        // 已删除（v0.24.5：11140 达阈值的默认动作）
	hcActArchived         = "archived"       // 已归档到异常分组
	hcActCooled           = "cooled"         // 已冷却
	hcActCreditsSync      = "credits_synced" // 已同步余额
)

// ---------- 结果结构 ----------

// HealthCheckResult 单个账号的检测结果。
type HealthCheckResult struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname,omitempty"`
	Realm    string `json:"realm,omitempty"`
	// State 五态之一；StateLabel 是它的中文名（前端直接用）。
	State      string `json:"state"`
	StateLabel string `json:"state_label"`
	// ReasonCode 机器可读原因码；Reason 是给人看的中文原因。
	ReasonCode string `json:"reason_code"`
	Reason     string `json:"reason"`
	// Definite 本次结论是否「确定性异常」（= 允许自动处置）。
	// 透出它是为了让运维看懂「为什么这条没被自动处理」。
	Definite bool `json:"definite"`
	// Actions 本次实际执行的处置动作（空 = 没动账号任何状态）。
	Actions []string `json:"actions,omitempty"`
	// Action 运维可对**这一条**执行的一键动作（hcActionEnable / hcActionRevive / 空）。
	//
	// 与 Actions 的区别：Actions 是「系统刚刚做了什么」，Action 是「运维现在能做什么」。
	// 前端据此渲染按钮，不必匹配中文原因（见 hcActionFor）。
	Action string `json:"action,omitempty"`
	// Credits 探测得到的权威余额；CreditsKnown=false 表示本次没拿到（探测失败）。
	Credits      int64 `json:"credits"`
	CreditsKnown bool  `json:"credits_known"`
	LatencyMs    int64 `json:"latency_ms"`
	CheckedAt    int64 `json:"checked_at"`

	// —— 对话探活（v0.24.3）——
	// 余额接口只能证明「凭证 + billing 通道健康」，不能证明「chat 通道能干活」。
	// 这一组是**真实对话**探活的结果：ChatProbed=false 表示本次没做（被关掉或跳过）。
	ChatProbed bool   `json:"chat_probed"`
	ChatOK     bool   `json:"chat_ok"`
	ChatModel  string `json:"chat_model,omitempty"`
	ChatMs     int64  `json:"chat_ms,omitempty"`
	ChatNote   string `json:"chat_note,omitempty"`

	// Egress 本次探活**实际使用**的出口（v0.24.3）：
	//
	//	"direct"     显式直连（覆盖全局默认）
	//	""           未指定且无全局默认 → 走默认 client（实际也是直连）
	//	"http://..." 生效的代理 URL
	//
	// 为什么它是关键信息：判断 11140 的归属（出口 IP 问题 vs 账号问题）靠的就是对比 ——
	// 同一个号「直连被拦、换代理正常」= 出口 IP 被关联；「换代理仍被拦」= 账号级标记。
	// 没有这一栏，运维只能看到「请求非法」而不知道该去动出口还是动账号。
	//
	// 注：v0.24.3 起本包所有出站（含余额与对话探活）都按账号选出口，故这里的值
	// **就是**探活真实走的路径（此前探活一律直连，配了代理也不会生效）。
	Egress string `json:"egress,omitempty"`

	// UpstreamNote 上游响应体 displayMsg 的原文（zh 优先，见 upstream.UpstreamDisplayMsg）。
	//
	// 为什么单独一栏而不是塞进 reason：reason 是**我们**的判定话术，UpstreamNote 是
	// **上游自己**的话。两者要能分开读 —— 运维一眼就能看到「上游说这个号异常」还是
	// 只说了「请求非法」，这正是 11140 归属判断（账号级 vs 出口级）的分水岭。
	// 空 = 上游没给（非 JSON / 无该字段），不编造。
	UpstreamNote string `json:"upstream_note,omitempty"`
}

// HealthCheckSummary 一批检测的汇总。
type HealthCheckSummary struct {
	Total     int            `json:"total"`
	ByState   map[string]int `json:"by_state"`
	Definite  int            `json:"definite"`  // 确定性异常数
	Uncertain int            `json:"uncertain"` // 不确定数
	StartedAt int64          `json:"started_at"`
	EndedAt   int64          `json:"ended_at"`
	// Source 本次结果由谁触发：manual（手动检测）/ import（导入后自动）。
	// 透出它是为了不让运维把「导入时顺手探的一批」误读成「我点的那次检测」。
	Source string `json:"source,omitempty"`
	Note   string `json:"note,omitempty"`
}

// hcActionFor 该检测结果对应哪个「一键可解决」的运维动作（无则空串）。
//
// 为什么必须是纯函数 + 结构化输出：池内早已区分了两种停用
// （见 pool.Status 上 ManualDisabled 的注释：「两种可用操作不同：前者可 revive，
// 后者该 enable」），但这个契约此前只存在于注释里，调用方（webapp 的启用端点）
// 漏掉了 revive 那一半 —— 于是对被系统禁用的号点「启用」毫无效果。
// 把动作显式算出来、透给前端，契约就再也藏不住了。
//
// st 必须传**探测前**的池状态（探测过程会 ClearSessionDead / SetCredits，
// 用探测后的状态会丢掉「它曾经被禁用」这个关键事实）。
func hcActionFor(state, reasonCode string, st pool.Status) string {
	switch {
	case state == hcSkipped && reasonCode == hcCodeManualDisabled:
		return hcActionEnable
	case state == hcDegraded && st.Disabled:
		// 探测通过但池内仍挂着系统禁用位 = 典型的「误判受害者」，可复活。
		return hcActionRevive
	}
	return ""
}

// ---------- 判定核心（纯函数，全部可单测） ----------

// hcPrecheckVerdict 本地前置判定的结果。
type hcPrecheckVerdict struct {
	State      string
	ReasonCode string
	Reason     string
	// Done=true 表示已定论，不必出站探测。
	Done bool
}

// hcPrecheck 第一级：本地前置判定（**不出站**）。
//
// 只处理「本地就能定论」的两类；其余一律放行去探测（包括 Disabled —— 被误判的
// 号必须靠探测才能翻案，见文件头注释）。
func hcPrecheck(acc *Account, st pool.Status, found bool) hcPrecheckVerdict {
	if acc == nil || !found {
		return hcPrecheckVerdict{
			State: hcAbnormal, ReasonCode: hcCodeAuthMissing,
			Reason: "账号不在池内（已删除或未同步）", Done: true,
		}
	}
	// ① 运维手动停用：刻意摘除，探测它没有意义，也不该打扰运维。
	if st.ManualDisabled {
		reason := "运维手动停用，未参与检测"
		if st.ManualReason != "" {
			reason += "（" + st.ManualReason + "）"
		}
		return hcPrecheckVerdict{
			State: hcSkipped, ReasonCode: hcCodeManualDisabled, Reason: reason, Done: true,
		}
	}
	// ② 凭证不可用：本地即可定论，发出去也只会失败。
	if strings.TrimSpace(acc.AccessToken) == "" {
		return hcPrecheckVerdict{
			State: hcAbnormal, ReasonCode: hcCodeAuthMissing,
			Reason: "账号凭证缺失（无 AccessToken），无法登录上游", Done: true,
		}
	}
	return hcPrecheckVerdict{}
}

// hcErrVerdict 第二级：探测错误 → 判定。
type hcErrVerdict struct {
	State      string
	ReasonCode string
	Reason     string
	// Definite=true 表示「证据指向账号本身」，允许自动处置。
	Definite bool
}

// hcVerdictFromErr 把探测失败翻译成判定结论。
//
// 映射规则即文件头「自动处置矩阵」：只有账号自身的问题才算确定；出口 / 上游 /
// 网络一类一律 uncertain 且不处置。未分类错误的默认值是 uncertain —— 宁可漏报
// 也不误判（一次探测的信息量不足以给账号定罪）。
func hcVerdictFromErr(err error) hcErrVerdict {
	if err == nil {
		return hcErrVerdict{}
	}
	var ue *upstream.Error
	if !errors.As(err, &ue) {
		// 非上游信封错误 = 传输层（超时、DNS、连接被拒等）。与账号无关。
		return hcErrVerdict{
			State: hcUncertain, ReasonCode: hcCodeTransport,
			Reason: "探测请求未完成（网络 / 超时）：" + truncateForReason(err.Error()),
		}
	}
	msg := truncateForReason(ue.Msg)

	// —— 账号级限制信号（v0.24.6）：**优先于一切按码/按文案的细分** ——
	//
	// 上游已经在 displayMsg 里明说"这个账号被限制了"，此时再按 11140 / WAF / 限流
	// 去细分只会把结论说轻 —— 那些分支的处置都是**可逆**的（软冷却 / 累计），
	// 而账号级限制是不可逆的，必须走即时处置。判据是上游的原话，不是我们的猜测。
	if upstream.IsAccountRestricted(ue.Display) {
		return hcErrVerdict{
			State: hcAbnormal, ReasonCode: hcCodeRestricted, Definite: true,
			Reason: upstream.AccountRestrictedHint() + upstreamNoteSuffix(ue.Display) + suffixMsg(msg),
		}
	}

	switch ue.Kind {
	case upstream.ErrSessionDead:
		return hcErrVerdict{
			State: hcAbnormal, ReasonCode: hcCodeSessionDead, Definite: true,
			Reason: "登录已失效（12153 会话失效）" + suffixMsg(msg),
		}
	case upstream.ErrAccountFault:
		// 14017 与 11140 共用 ErrAccountFault，但**语义完全不同**，必须分开：
		//
		//	11140 = **request illegal**（这次请求不合法）—— 由出口 IP、请求频率、
		//	        头/UA 细节这类**与账号无关**的因素触发。它不是「账号被封」。
		//	        旧实现把它读成 banned 并一次即永久停用，2026-10-13 实测在一个风控
		//	        窗口内**批量误杀 30 个健康账号**（凭证与余额完好、单独重探即通过）。
		//	        详见 docs/封号事件诊断与账号隔离核对-20261013.md。
		//	14017 = 试用未开通 / 权限异常 —— 必须重登，属账号侧。
		if strings.Contains(ue.Msg, "11140") {
			return hcErrVerdict{
				State: hcUncertain, ReasonCode: hcCodeRequestIllegal,
				// Definite=false：证据**不指向账号本身**，不给定性。
				// 但调用方仍会做可逆处置（软冷却 + 累计），见 applyHealthDisposition。
				//
				// 措辞循证（2026-10-13 A 方案对照实验结论）：同一个号换 **3 个不同出口 IP**
				// （两条代理 + 显式直连）仍被拦，而**同一出口 IP 上的健康号正常** —— 故这
				// 一类 11140 是**账号级标记**，换出口不解除；「多为出口 IP 问题」的旧措辞
				// 会把运维引去反复换代理（无效动作）。是否成因为出口，看 UpstreamNote：
				// 上游若明说「异常行为」即是账号级。
				Reason: "上游返回 11140 request illegal。**它不等于账号被封**：凭证与余额仍可用，" +
					"但对话通道被上游风控拦截。若上游同时给了「异常行为」类提示，则为**账号级标记**" +
					"（换出口无法解除）。已软冷却并累计，连续超阈值才暂停" +
					upstreamNoteSuffix(ue.Display) + suffixMsg(msg),
			}
		}
		return hcErrVerdict{
			State: hcAbnormal, ReasonCode: hcCodeAccountFault, Definite: true,
			Reason: "账号授权 / 权限异常（14017），需重新登录激活" + suffixMsg(msg),
		}
	case upstream.ErrHardCredit:
		return hcErrVerdict{
			State: hcAbnormal, ReasonCode: hcCodeNoCredits, Definite: true,
			Reason: "账号积分不足 / 已耗尽（受限，无法消耗积分）" + suffixMsg(msg),
		}
	case upstream.ErrChannelDenied:
		// 与 ErrWafBlock 同一类「不是账号的问题」，但成因不同：WAF 多为出口 IP，
		// 渠道拒绝多为**客户端指纹 / 请求形态**。刻意分开成两个 reason code，
		// 否则运维会把「换代理」当成万能动作（对指纹问题无效）。
		return hcErrVerdict{
			State: hcUncertain, ReasonCode: hcCodeChannelDenied,
			Reason: "上游按渠道策略拒绝了本次请求（11128）——与**客户端指纹 / 请求形态**有关，" +
				"不是账号问题（凭证与余额正常）。换账号（每号独立的机器/会话指纹）可能直接通过" + suffixMsg(msg),
		}
	case upstream.ErrWafBlock:
		return hcErrVerdict{
			State: hcUncertain, ReasonCode: hcCodeWafBlock,
			Reason: "被上游 WAF 拦截（403）——通常是**出口 IP** 的问题而非账号本身，建议检查代理" + suffixMsg(msg),
		}
	case upstream.ErrSoftRate:
		return hcErrVerdict{
			State: hcUncertain, ReasonCode: hcCodeRateLimited,
			Reason: "账号正在限流（429）——临时状态，通常无需处理" + suffixMsg(msg),
		}
	case upstream.ErrNotFound:
		return hcErrVerdict{
			State: hcUncertain, ReasonCode: hcCodeUpstream404,
			Reason: "上游接口偶发 404（与账号无关，可稍后重试）" + suffixMsg(msg),
		}
	case upstream.ErrServer:
		return hcErrVerdict{
			State: hcUncertain, ReasonCode: hcCodeUpstream5xx,
			Reason: "上游服务故障（5xx）——全局问题，不归因账号" + suffixMsg(msg),
		}
	default:
		// ErrClient / ErrContentBlocked / ErrBadParams / ErrModelBlocked / ErrPromptTooLong /
		// ErrImageInvalid / ErrNone —— 对「账号体检」而言信息不足，一律不确定。
		return hcErrVerdict{
			State: hcUncertain, ReasonCode: hcCodeUnclassified,
			Reason: "探测返回未分类错误（不足以判定账号异常）" + suffixMsg(msg),
		}
	}
}

// hcVerdictFromProbeOK 第三级：探测成功后的本地补充判定。
//
// 顺序即优先级：余额为 0（受限）> 降权痕迹（可用但有伤）> 正常。
func hcVerdictFromProbeOK(st pool.Status, remain int64) hcErrVerdict {
	if remain <= 0 {
		return hcErrVerdict{
			State: hcAbnormal, ReasonCode: hcCodeZeroRemain, Definite: true,
			Reason: "凭证有效但账号积分为 0（无额度可服务）",
		}
	}
	if r := hcDegradeReason(st); r != "" {
		return hcErrVerdict{State: hcDegraded, ReasonCode: hcCodeDegraded, Reason: r}
	}
	return hcErrVerdict{State: hcHealthy, ReasonCode: hcCodeOK, Reason: "账号正常（凭证有效、余额充足、无降权）"}
}

// hcDegradeReason 汇总池内的降权 / 不可服务痕迹；无痕迹返回空串。
//
// 这些痕迹都表示「凭证是好的，但当前不参与选号」，故判 degraded 而非 healthy
// —— 否则运维会以为一个正在熔断的号在正常工作。
func hcDegradeReason(st pool.Status) string {
	var parts []string
	now := time.Now()
	if st.Disabled {
		r := strings.TrimSpace(st.DisabledReason)
		if r == "" {
			r = "原因未记录"
		}
		parts = append(parts, "曾被系统禁用（"+r+"）——本次探测通过，可考虑「复活」")
	}
	if st.Cooling && st.CoolRemaining > 0 {
		parts = append(parts, "冷却中（剩余 "+humanizeSeconds(st.CoolRemaining)+"）")
	}
	if !st.BreakerUntil.IsZero() && st.BreakerUntil.After(now) {
		parts = append(parts, "熔断中（恢复于 "+st.BreakerUntil.Format("15:04:05")+"）")
	}
	if !st.DegradeUntil.IsZero() && st.DegradeUntil.After(now) {
		parts = append(parts, "连败降权中（恢复于 "+st.DegradeUntil.Format("15:04:05")+"）")
	}
	if len(st.RateLimitedModels) > 0 {
		parts = append(parts, "存在模型级限流（"+itoa(len(st.RateLimitedModels))+" 个模型）")
	}
	if len(parts) == 0 {
		return ""
	}
	return "凭证有效，但当前不参与选号：" + strings.Join(parts, "；")
}

// ---------- 小工具 ----------

// truncateForReason 把上游消息裁剪到便于展示的长度（原因行不该被一大段 JSON 撑爆）。
func truncateForReason(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) > 120 {
		return string(r[:120]) + "…"
	}
	return s
}

// suffixMsg 把上游消息拼成「 — 原始信息」后缀（空消息返回空串）。
// shouldClearFault 决定本次探测是否该清 11140 累计计数（v0.24.3）。
//
// 口径：**以「能不能真的服务对话」为准**。
//   - 开了对话探活 → 只有对话通道通过（chatProbed && chatOK）才清；
//     余额通过不算 —— 实测存在「余额全通、对话全拦」的号，此时清零会把计数
//     钉死在 1/3（清一次、记一次），既不摘号又持续给用户报错。
//   - 没开对话探活 → 退化为「余额通过即清」（证据弱一档，但比一直挂着强）。
//
// 抽成纯函数是为了把这条**曾经写错过**的判定钉进单测（见 healthcheck_test.go）。
func shouldClearFault(chatProbeEnabled, chatProbed, chatOK bool) bool {
	if chatProbeEnabled {
		return chatProbed && chatOK
	}
	return true
}

// upstreamNoteOf 取上游错误信封里 displayMsg 的人话解释（无则空串）。
func upstreamNoteOf(err error) string {
	var ue *upstream.Error
	if err != nil && errors.As(err, &ue) {
		return ue.Display
	}
	return ""
}

// upstreamNoteSuffix 把上游提示拼成 reason 的从句（空串则该从句整体消失）。
func upstreamNoteSuffix(display string) string {
	if display == "" {
		return ""
	}
	return "；上游提示：" + display
}

func suffixMsg(msg string) string {
	if msg == "" {
		return ""
	}
	return "；上游返回：" + msg
}

// humanizeSeconds 秒 → 「1m20s」这类紧凑文本（原因行用，不引第三方库）。
func humanizeSeconds(sec int64) string {
	if sec < 60 {
		return itoa(int(sec)) + "s"
	}
	if sec < 3600 {
		return itoa(int(sec/60)) + "m" + itoa(int(sec%60)) + "s"
	}
	return itoa(int(sec/3600)) + "h" + itoa(int((sec%3600)/60)) + "m"
}

// ---------- 执行器 ----------

// checkAccountHealth 检测单个账号并（在确定性异常时）自动处置。
//
// 返回的结果**总是非 nil**（失败也带原因），便于批量场景逐条展示。
// allowDelete 一路下传给 applyHealthDisposition（见其注释）：false = 观测型调用，
// 只做可逆处置、绝不删除。
func (a *app) checkAccountHealth(uid string, allowDelete bool) *HealthCheckResult {
	start := time.Now()
	res := &HealthCheckResult{UID: uid, CheckedAt: start.Unix(), State: hcUncertain}
	defer func() { res.LatencyMs = time.Since(start).Milliseconds() }()

	// (nil, nil) = 库里没有该账号（GetAccount 的既有语义），故两个条件都要看。
	acc, err := a.store.GetAccount(uid)
	if err != nil || acc == nil {
		acc = nil
	}
	st, inPool := a.proxy.pool.Status(uid)
	if acc != nil {
		res.Nickname = acc.Nickname
		res.Realm = acc.Realm
	}
	// 本次探活会走的出口（v0.24.3）：v0.24.3 起 upstream 的所有出站都按账号选出口，
	// 所以这就是真实路径 —— 定位 11140 时「直连被拦 / 走代理正常」的对比靠它。
	res.Egress = a.proxy.ResolveEgress(uid)

	// —— 第一级：本地前置 ——
	if pre := hcPrecheck(acc, st, inPool); pre.Done {
		res.State, res.ReasonCode, res.Reason = pre.State, pre.ReasonCode, pre.Reason
		res.StateLabel = hcStateLabel(pre.State)
		res.Action = hcActionFor(pre.State, pre.ReasonCode, st)
		return res
	}

	// 探活总闸（v0.24.8 补齐）：**一切会真出站的探活**共用同一把闸。
	//
	// 此前只有「查积分」（writeCreditDetail）挂在闸上，健康检测 / 对话探活 / 批量探活
	// 全都是绕过去的 —— 而 healthchat.go 的注释明写「本闸只管探活类出站（查积分 /
	// 健康检测 / 对话探活）」。实现与自述不符的代价很具体：前端卡片（走闸）与批量
	// 体检（不走闸）同时进行时，全局并发就突破了 3 的上限，而这道闸存在的**全部理由**
	// 正是防 2026-10-13 那种「几秒内几十个并发打同一出口 → 11140 批量误判」。
	//
	// 位置在**本地前置判定之后**：`skipped`（手动停用 / 无凭证 / 不在池）根本不出站，
	// 让它占闸只会把真正需要探的号无谓拒掉。
	//
	// 被拦时返回「不确定 + 限流原因」而不是错误：批量体检里个别账号被跳过是可接受的
	// （30 秒内重复探同一个号本来就没有新增信息量），但**并发必须有界**。
	if release, gerr := a.probes.acquire(uid); gerr != nil {
		res.State, res.ReasonCode = hcUncertain, hcCodeProbeThrottled
		res.StateLabel = hcStateLabel(hcUncertain)
		res.Reason = "未探活（探活总闸限流）：" + gerr.Error()
		return res
	} else {
		defer release()
	}

	au := authFromAccount(acc)
	if au == nil {
		res.State, res.ReasonCode, res.StateLabel = hcAbnormal, hcCodeAuthMissing, hcStateLabel(hcAbnormal)
		res.Reason = "账号凭证无法构造（token 格式异常）"
		return res
	}

	// —— 第二级：真出站探测（拉余额 = 探活） ——
	// 只用 remain / expiringSoon：逐包明细（packs）是「积分面板」的用途，
	// 健康检测只需要权威总额。用 _ 丢弃而非 `_ = packs`，避免留下无意义语句。
	_, remain, _, _, expiringSoon, perr := a.proxy.uc.ResourcePackages(au)
	if perr != nil {
		v := hcVerdictFromErr(perr)
		res.State, res.ReasonCode, res.Reason = v.State, v.ReasonCode, v.Reason
		res.Definite = v.Definite
		res.UpstreamNote = upstreamNoteOf(perr)
		res.StateLabel = hcStateLabel(v.State)
		// 处置条件：确定性问题，或「11140 请求非法」（v0.24.3）。
		// 后者**不定罪**（State=uncertain、Definite=false），但仍要做**可逆**处置
		// （软冷却 + 累计计数）—— 否则一个持续被风控判非法的号会无限占用选号名额、
		// 每次都被选中再失败。可逆是关键：软冷却自动到期，累计到阈值才摘除。
		if v.Definite || v.ReasonCode == hcCodeRequestIllegal {
			// 已禁用的号**不再行使删除**（v0.24.8）：它早已被摘除出选号，删除只会把
			// 「上游标记自行解除后还能复活」这条路彻底断掉 —— 而被误判禁用的号恰恰
			// 是最需要保留这个可能性的那批（见文件头：它们正是最需要被检测的）。
			// 计数与软冷却照常，只是不做不可逆动作。
			res.Actions = a.applyHealthDisposition(uid, v.ReasonCode, perr, allowDelete && !st.Disabled)
		}
		return res
	}

	// —— 探测成功：后续流程（通过前后）——
	// ① 账号被证明未死 → 清会话失效计数（与既有纪律一致）。
	if a.proxy.pool.ClearSessionDead(uid) {
		res.Actions = append(res.Actions, hcActCleared)
	}
	// ①′ 11140 累计的清零**故意不放在这里**（v0.24.3 修正）。
	//
	// 曾经的写法是「余额通道通过 → 清零」，理由是"余额能通就说明没被持续判非法"。
	// **该理由已被实测证伪**：这 30 个号余额通道 100% 通过、对话通道 100% 11140。
	// 若在余额通过时就清零，计数会陷入「清一次 → 对话失败再记一次」的循环、永远钉在
	// 1/3：既到不了阈值、也不会被停用，用户请求却持续 500 —— 最糟的组合。
	//
	// 现在的口径：**只有对话通道（真正服务用户的那条）被证明可用才清零**，
	// 由下方对话探活分支执行（未开启探活时退化为余额通过即清，见 shouldClearFault）。
	// ② 同步权威余额（含快过期分桶）+ 按余额解冻 + 即时重算预备组
	//    —— 与 creditsync 的收尾口径逐字一致。
	a.proxy.pool.SetCreditsDetailed(uid, remain, expiringSoon)
	a.proxy.pool.ReenableIfCredits(uid, remain)
	a.reconcilePoolGroupOne(uid)
	res.Actions = append(res.Actions, hcActCreditsSync)
	res.Credits, res.CreditsKnown = remain, true

	// —— 第二级·续：**对话探活**（v0.24.3，默认开启，见 healthchat.go）——
	//
	// 余额通道通过 ≠ chat 通道可用。这一步发一次极小的真实对话（单轮 / max_tokens=1 /
	// 非流式），把「能不能干活」也纳入判定 —— 2026-10-13 事故的盲区正在这里：
	// 30 个号余额探活全通过，实际已被上游风控判为「请求非法」。
	if a.hcChatProbeEnabled() {
		cp := a.hcChatProbe(au)
		res.ChatProbed, res.ChatOK = true, cp.OK
		res.ChatModel, res.ChatMs, res.ChatNote = cp.Model, cp.LatencyMs, cp.Note
		if !cp.OK {
			// 复用同一套错误判定（不另写分类逻辑）——对话通道的证据比余额通道更贴近
			// 「能不能真的服务请求」，故由它定论。
			v := hcVerdictFromErr(cp.Err)
			if cp.Err == nil {
				// 防御：没有可分类的错误时按「不确定」处理。
				// **绝不因为信息不全就罚账号** —— 这是本模块的既有闸门。
				v = hcErrVerdict{State: hcUncertain, ReasonCode: hcCodeUnclassified,
					Reason: "对话探活失败（无分类错误）：" + cp.Note}
			}
			res.State, res.ReasonCode, res.Definite = v.State, v.ReasonCode, v.Definite
			res.Reason = "对话探活：" + v.Reason
			res.UpstreamNote = upstreamNoteOf(cp.Err)
			res.StateLabel = hcStateLabel(v.State)
			res.Action = hcActionFor(v.State, v.ReasonCode, st)
			if v.Definite || v.ReasonCode == hcCodeRequestIllegal {
				// 同余额通道：已禁用的号不做删除（理由见上方注释）。
				res.Actions = append(res.Actions, a.applyHealthDisposition(uid, v.ReasonCode, cp.Err, allowDelete && !st.Disabled)...)
			}
			return res
		}
		// 对话通道通过 → 这是**唯一**能证明「11140 不是持续状态」的证据，清零。
		if shouldClearFault(true, true, cp.OK) && a.proxy.pool.ClearAccountFault(uid) {
			res.Actions = append(res.Actions, hcActClearedFault)
		}
	} else if shouldClearFault(false, false, false) && a.proxy.pool.ClearAccountFault(uid) {
		res.Actions = append(res.Actions, hcActClearedFault)
	}

	// —— 第三级：本地补充判定（用**刷新前**的池状态看降权痕迹，避免刚写回的
	//     余额/解冻影响结论）——
	v := hcVerdictFromProbeOK(st, remain)
	res.State, res.ReasonCode, res.Reason, res.Definite = v.State, v.ReasonCode, v.Reason, v.Definite
	res.StateLabel = hcStateLabel(v.State)
	// 可执行动作（v0.23.4）：用**探测前**的 st —— 上面几步会 ClearSessionDead /
	// SetCreditsDetailed / ReenableIfCredits，用之后的状态就看不到「曾被禁用」了。
	res.Action = hcActionFor(res.State, res.ReasonCode, st)
	// 余额为 0 属确定性「受限」，但它不需要额外处置：余额归零已由 SetCreditsDetailed
	// 对齐，分组收敛由 reconcilePoolGroupOne 按预备组规则处理（阈值 100 < 0 即进预备组）。
	return res
}

// applyHealthDisposition 对确定性异常执行自动处置，返回执行的动作列表。
//
// 处置**全部复用既有原语**（与 proxy.go 的 reportUpstreamError 同一套语义），
// 不新造路径；reason 文案带上「健康检测」前缀，便于在账号卡上区分来源。
//
// allowDelete 控制**是否允许不可逆动作（删除）**（v0.24.8）：
//
//	true  —— 手动体检 / 批量检测：完整处置，包含按策略删除。
//	false —— **观测型调用**（refreshCredits / refreshCreditsAsync，即"刷新积分"
//	          与"导入后刷余额"）：只做**可逆**处置（软冷却 / 停用 / 归档 / 计数），
//	          **绝不删除**。
//
// 为什么需要这个区分：这两条路径复用了检测批（同一个出站请求既取回权威余额、又判定
// 账号可用），但它们的**语义是观测**。此前一次「刷新全部积分」或「导入账号」可能连带
// 产生一批不可逆删除，且删除是异步的、界面上没有任何确认 —— 用户点的是"刷新余额"，
// 得到的是"账号没了"。把不可逆动作限定在显式体检路径上。
func (a *app) applyHealthDisposition(uid, reasonCode string, cause error, allowDelete bool) []string {
	var acts []string
	// ⚠️ 未入池的代挂号：**只报告，不处置**（v0.31.20，审查发现的越权处置）。
	//
	// 代挂号只要凭证能解析就进池成员表，于是健康检测会连**没参与平台流量**的号
	// 一起探活并按 11140 / 12153 阈值停用、甚至在 `pool_delete_on_11140=1` 时
	// **不可逆删除**。这与两条既定语义直接冲突：
	//   ① 「未入池 = 不参与平台反代」（`allowsSelection` 把它们排除在外）；
	//   ② 「入池要号主同意」（v0.31.18 的 invited 态）—— 号主还没同意，号先被删了。
	// 平台没有为这些号提供任何流量，也就没有立场对它们做不可逆处置。
	// （用户自己的健康操作如「同步积分」是用户主动发起的，不经过这里。）
	if a.isUnpooledHosted(uid) {
		log.Printf("INFO: [health] %s 是未入池的代挂号，只报告不处置（code=%s）", uid, reasonCode)
		return []string{hcActSkipUnpooledHost}
	}
	var ue *upstream.Error
	_ = errors.As(cause, &ue)
	retryAfter := time.Duration(0)
	if ue != nil {
		retryAfter = ue.RetryAfter
	}
	_ = retryAfter
	switch reasonCode {
	case hcCodeSessionDead:
		// 走阈值累计（不一次杀号）：12153 会被网络抖动临时触发。
		if a.proxy.pool.NoteSessionDead(uid) {
			if a.proxy.pool.RetireAccount(uid, upstream.ErrSessionDead, "连续 12153（会话失效）超阈值，已禁用，需重登") {
				a.proxy.archiveAbnormal(uid, "登录失效（12153）连续超阈值，已禁用", false)
				acts = append(acts, hcActNoteDead, hcActDisabled, hcActArchived)
			} else {
				acts = append(acts, hcActNoteDead)
			}
		} else {
			acts = append(acts, hcActNoteDead)
		}
	case hcCodeRequestIllegal:
		// 11140 request illegal（v0.24.3）：**不**直接停用。
		//
		// 旧实现此处是 `pool.Disable` + `archiveAbnormal`，一次即永久摘除、
		// 并写 reason「上游封禁（11140），需重登」——2026-10-13 实测在一个风控
		// 窗口内批量误杀 30 个健康账号。现在与 12153 同纪律：
		// **软冷却（可逆、自动到期）+ 累计计数，连续超阈值才摘除**。
		fails, reached := a.proxy.pool.NoteAccountFault(uid)
		// 达阈值 → 按策略处置。
		//
		// 处置**收敛到 app.retireBy11140**（v0.24.8）：此前这里内联一份「读 kv 开关 →
		// 删除 / 停用」，而生产转发链路（proxy.reportUpstreamError）另有一份写死的
		// `RetireAccount`，于是同一个错误码在两条链路上给出不可逆程度不同的结果，
		// 且 `pool_delete_on_11140` 只覆盖其中一条。现在两条链路读同一个开关、
		// 走同一个函数（旧网关无 DB，等价于开关恒为 0，见该函数注释）。
		if reached {
			acts = append(acts, a.retireBy11140(uid, allowDelete)...)
		} else if st, ok := a.proxy.pool.Status(uid); ok && st.Disabled {
			// 已经因**别的原因**停用的号：只累计计数，**不**再写软冷却。
			//
			// Cooldown 会覆盖 e.reason，于是一个「已停用」的号会被写成
			// 「累计 1/3，未超阈值仅软冷却」——2026-10-13 实测到该误导：账号卡上
			// 显示"仅软冷却"却实际不在池内，运维会误判成"还在用、只是被降温"。
			// 停用位（Disabled）与冷却位（cool_kind/until）是两件事，别用一个文案描述两个。
			acts = append(acts, hcActNoteFault)
		} else {
			// ⚠️ 用 `accountFaultCooldown`（2h）而**不是** `softCooldown`（60s）：
			// 同一个 11140 在转发链（proxy.reportUpstreamError）用的就是 2h，且那里的
			// 注释写明了理由 —— 60s 时"一个风控窗口内 3 次白撞"就能凑满阈值摘号，
			// 2026-10-13 正是这么批量误杀 30 个健康号的。健康检测这条链路此前**没跟上**
			// 那次修复，于是同一个错误码在两条链路上有两套阈值（v0.31.20 收敛）。
			a.proxy.pool.Cooldown(uid, pool.CoolSoft, accountFaultCooldown,
				"11140 request illegal（累计 "+itoa(fails)+"/"+itoa(pool.AccountFaultThreshold())+"，未超阈值仅软冷却）")
			acts = append(acts, hcActNoteFault, hcActCooled)
		}
	case hcCodeRestricted:
		// 账号级限制：**即时上报 + 立即删除**，不等累计（v0.24.6，用户明确要求）。
		//
		// 等 3 次在这里没有意义：这一条是上游对**账号本身**的判定，不是"这次请求"，
		// 且已实测证实换出口 IP 不解除 —— 让它留在池里只会让选中它的请求持续报错。
		//
		// allowDelete=false（刷新积分 / 导入这类**观测型**调用）：照常上报，但只
		// **停用（可逆）**，不删除 —— 删除不可逆，不该由"刷一下积分"顺带完成，
		// 何况那是异步的、界面无确认。人工看到上报后可以再决定删。
		a.reportRisk(uid, "", "11140", upstreamNoteOf(cause), "health", "observed")
		if allowDelete {
			acts = append(acts, a.handleRestrictedAccount(uid, "health", upstreamNoteOf(cause))...)
		} else if a.proxy.pool.RetireAccount(uid, upstream.ErrAccountFault, upstream.AccountRestrictedHint()) {
			a.proxy.archiveAbnormal(uid, "账号级限制（观测到，仅停用待人工确认）", false)
			acts = append(acts, hcActDisabled, hcActArchived)
		}
	case hcCodeAccountFault:
		a.proxy.pool.Cooldown(uid, pool.CoolSoft, softCooldown, "健康检测：账号授权/权限异常")
		// autoRecover=false：14017 需重登才能解决，不能靠冷却到期自动回归（v0.23.1）。
		a.proxy.archiveAbnormal(uid, "账号故障（14017），检测确认，需重登", false)
		acts = append(acts, hcActCooled, hcActArchived)
	case hcCodeNoCredits:
		a.proxy.pool.CooldownUntilTomorrow4AM(uid, "健康检测：积分耗尽")
		a.proxy.archiveAbnormal(uid, "积分不足（无法消耗积分），检测确认", true)
		acts = append(acts, hcActCooled, hcActArchived)
	}
	// 同步选号范围：处置后内存映射需立即反映（否则本进程还会选到它）。
	if len(acts) > 0 {
		_ = a.proxy.SyncAccounts()
		log.Printf("INFO: [health] account %s 判定异常 %s → 处置 %v", uid, reasonCode, acts)
	}
	return acts
}

// checkHealthMany 逐个检测（账号间错峰，避免一串请求同时打同一出口）。
//
// 错峰的理由与 creditsync 相同：检测不是业务请求，没必要也不该在极短时间内
// 对同一出口打出一串请求（那本身就是个可识别特征）。
// allowDelete 同 checkAccountHealth：false = 观测型调用（不删除）。
func (a *app) checkHealthMany(uids []string, allowDelete bool) []*HealthCheckResult {
	out := make([]*HealthCheckResult, 0, len(uids))
	for i, uid := range uids {
		uid = strings.TrimSpace(uid)
		if uid == "" {
			continue
		}
		if i > 0 {
			time.Sleep(time.Duration(250+rand.Intn(350)) * time.Millisecond)
		}
		out = append(out, a.checkAccountHealth(uid, allowDelete))
	}
	// 排序：严重的在前（异常 > 不确定 > 降权 > 跳过 > 正常），同级按 uid 稳定。
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := hcStateRank(out[i].State), hcStateRank(out[j].State)
		if ri != rj {
			return ri < rj
		}
		return out[i].UID < out[j].UID
	})
	return out
}

// healthCheckCandidates 全量检测的默认目标集合。
//
// 包含**所有在池账号**（含禁用 / 冷却 / 降权），只排除运维手动停用的 —— 被系统
// 误判禁用的号正是最需要被检测的那批（见文件头）。上限防止一次点击打出上百请求。
func (a *app) healthCheckCandidates() []string {
	var out []string
	for _, st := range a.proxy.pool.List() {
		if st.ManualDisabled {
			continue
		}
		// 未入池的代挂号不进候选（v0.31.20）：它们不参与平台流量，探活是**白出站**
		// （每号一次上游调用），而处置又是对用户资产的越权。号主想看自己的号，
		// 用控制台上的主动操作（同步积分 / 查看全部）。
		if a.isUnpooledHosted(st.UID) {
			continue
		}
		if a.proxy.pool.AuthByUID(st.UID) == nil && !st.Disabled {
			// 凭证取不到且未被禁用 → 无意义（禁用号仍要探，见文件头）。
			continue
		}
		out = append(out, st.UID)
		if len(out) >= healthCheckMaxAccounts {
			break
		}
	}
	return out
}

// isUnpooledHosted 是否为「未入池的代挂号」。
//
// 判定只看两个库字段（不看池内状态）：`owner_user_id > 0` 且 `pool_state != "in"`。
// 包括未申请（空）/ 待审（pending）/ 已邀请（invited）/ 已驳回（rejected）——
// 它们的共同点是**此刻不参与平台反代**，因此平台不该对它们做不可逆处置。
func (a *app) isUnpooledHosted(uid string) bool {
	acc, err := a.store.GetAccount(uid)
	if err != nil || acc == nil {
		return false
	}
	return acc.OwnerUserID > 0 && acc.PoolState != "in"
}

// healthCheckMaxAccounts 单次批量检测的账号上限。
// 与 creditsRefreshMaxAccounts 同理：给「一键全量」兜底，避免一次点击跑上几分钟。
const healthCheckMaxAccounts = 60

// ---------- 批量结果的进程内快照 ----------

// healthSnapshot 最近一次批量检测的结果（内存缓存，进程内、重启即失）。
//
// 为什么放内存而不落库：检测结果是**时点观测**而非账号的持久属性 —— 落库会让
// 「上周的异常」和「今天的状态」混在一张表里，读的人必须先问「这是什么时候的」。
// 内存快照天然只有一份「最近一次」，语义无歧义；要历史就自己复制报告。
type healthSnapshot struct {
	mu        sync.RWMutex
	results   []*HealthCheckResult
	summary   HealthCheckSummary
	hasResult bool
}

var healthLast healthSnapshot

// set 写入一次批量结果。source 见 HealthCheckSummary.Source。
func (s *healthSnapshot) set(res []*HealthCheckResult, started time.Time, source string) {
	s.mu.Lock()
	s.results, s.hasResult = res, true
	s.summary = HealthCheckSummary{
		StartedAt: started.Unix(),
		EndedAt:   time.Now().Unix(),
		Source:    source,
	}
	s.recountLocked()
	s.mu.Unlock()
}

// recountLocked 由当前 results 重算**计数类**字段（Total / ByState / Definite /
// Uncertain），保留 StartedAt / EndedAt / Source 等「本次检测」的元信息。
//
// set 与 dropUIDs 共用同一段计数口径：两处各写一遍的话，将来改了判定规则漏改一处，
// 就会出现「明细说 3 个异常、汇总说 2 个」这种没人能解释的现象。
func (s *healthSnapshot) recountLocked() {
	sum := s.summary
	sum.Total = len(s.results)
	sum.ByState = map[string]int{}
	sum.Definite = 0
	sum.Uncertain = 0
	for _, r := range s.results {
		if r == nil {
			continue
		}
		sum.ByState[r.State]++
		if r.State == hcAbnormal && r.Definite {
			sum.Definite++
		}
		if r.State == hcUncertain {
			sum.Uncertain++
		}
	}
	s.summary = sum
}

// dropUIDs 把指定账号从「最近一次检测」快照里剔除（v0.23.4），返回实际剔除条数。
//
// 为什么需要它（这是一类**通用**问题，不止停用/启用）：
// 检测结果是**时点观测**，不是账号的持久属性。当账号状态被人工改变（启用 / 停用 /
// 改凭证）后，基于旧状态得出的结论立即失效，但快照不会自己知道。留着旧条目的后果：
//
//	· 留在 skipped「运维手动停用」→ 运维刚点了启用，页面上仍写「已跳过」，他会
//	  认为「启用没生效」，然后反复点（本缺陷的直接诱因之一）；
//	· 留在 degraded「曾被系统禁用…可考虑『复活』」→ 提示永远挂着，同样诱导重复操作。
//
// 选择「剔除」而不是「标记为过期」：留着一条已知错误的结论比没有结论更坏 ——
// 运维会照着它行动。剔除后前端重拉即见该条目消失、汇总计数同步下降，语义正确
// （这一条观测确实已不成立）。
//
// 全部剔除后回到 hasResult=false：此时确实没有任何仍然有效的观测了。
func (s *healthSnapshot) dropUIDs(uids []string) int {
	drop := make(map[string]bool, len(uids))
	for _, u := range uids {
		if u = strings.TrimSpace(u); u != "" {
			drop[u] = true
		}
	}
	if len(drop) == 0 {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := make([]*HealthCheckResult, 0, len(s.results))
	n := 0
	for _, r := range s.results {
		if r != nil && drop[r.UID] {
			n++
			continue
		}
		kept = append(kept, r)
	}
	if n == 0 {
		return 0
	}
	s.results = kept
	s.recountLocked()
	s.hasResult = len(kept) > 0
	return n
}

// get 读最近一次结果（无结果时 hasResult=false）。
func (s *healthSnapshot) get() ([]*HealthCheckResult, HealthCheckSummary, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cp := make([]*HealthCheckResult, len(s.results))
	copy(cp, s.results)
	return cp, s.summary, s.hasResult
}

// ---------- Handlers ----------

// handleCheckAccountHealth 单账号健康检测（**同步**，1 次上游请求）。
// POST /api/admin/accounts/{uid}/health
func (a *app) handleCheckAccountHealth(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimSpace(r.PathValue("uid"))
	if uid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少账号 uid"})
		return
	}
	// 注意 store.GetAccount 对不存在的 uid 返回 **(nil, nil)** 而不是 error
	// （见其实现），故必须同时判 acc == nil —— 只判 err 会让不存在的账号
	// 走到下面并返回 200 空结果，前端就无法区分「账号不存在」与「检测完了」。
	acc, err := a.store.GetAccount(uid)
	if err != nil || acc == nil {
		// 池内有但库里没有（同步竞态）也要能检测 —— 不因查库失败就拒绝。
		if a.proxy.pool.AuthByUID(uid) == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "账号不存在"})
			return
		}
	}
	// 管理员显式体检：允许完整处置（含按策略删除）——这是"我就是要查它"的入口。
	res := a.checkAccountHealth(uid, true)
	writeJSON(w, http.StatusOK, map[string]any{
		"result": res,
		"ok":     res.State == hcHealthy || res.State == hcDegraded,
	})
}

// handleCheckHealthBatch 批量健康检测（**异步**受理）。
// POST /api/admin/accounts/health   body: {"uids":["..."]}（省略/空 = 全量候选）
func (a *app) handleCheckHealthBatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UIDs []string `json:"uids"`
	}
	// body 可选（空 body = 全量），解析失败按「未提供」处理。
	_ = json.NewDecoder(r.Body).Decode(&req)
	targets := req.UIDs
	if len(targets) == 0 {
		targets = a.healthCheckCandidates()
	}
	if len(targets) > healthCheckMaxAccounts {
		targets = targets[:healthCheckMaxAccounts]
	}
	if len(targets) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "noop", "count": 0,
			"note": "没有需要检测的账号",
		})
		return
	}
	a.checkHealthAsync(targets)
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "started",
		"count":  len(targets),
		"note":   "已在后台开始检测（账号之间错开数百毫秒），数秒后刷新结果即可看到",
	})
}

// checkHealthAsync 后台批量检测（异步受理用）。
//
// allowDelete=true：这是管理员显式发起的「批量体检」，允许完整处置（含按策略删除）。
func (a *app) checkHealthAsync(uids []string) {
	go func() {
		started := time.Now()
		res := a.checkHealthMany(uids, true)
		healthLast.set(res, started, "manual")
		_, sum, _ := healthLast.get()
		log.Printf("INFO: [health] batch done: total=%d abnormal=%d uncertain=%d (异常优先已排序)",
			sum.Total, sum.Definite, sum.Uncertain)
	}()
}

// handleHealthLast 取最近一次批量检测结果与汇总。
// GET /api/admin/accounts/health/last
func (a *app) handleHealthLast(w http.ResponseWriter, r *http.Request) {
	res, sum, ok := healthLast.get()
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"has_result": false,
			"note":       "还没有检测记录，点「检测」或「全量检测」开始",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"has_result": true,
		"results":    res,
		"summary":    sum,
		"limits":     map[string]any{"max_accounts": healthCheckMaxAccounts},
	})
}
