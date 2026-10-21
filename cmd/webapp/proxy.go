// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"workbuddy2api/internal/billing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/egress"
	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/prompt"
	"workbuddy2api/internal/rotateguard"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

const (
	// maxAttempts 单次请求最多尝试的账号数（含首次）。
	maxAttempts = 3
	// chatPath 上游 chat 路径（与 cmd/server 同源）。
	chatPath = "/v2/chat/completions"
	// softCooldown 429/WAF 等软冷却基准时长（指数退避由 pool 内部按 streak 计算）。
	softCooldown = 60 * time.Second

	// accountFaultCooldown 11140（request illegal）的专用冷却时长 —— **刻意远长于
	// softCooldown**（R5，v0.26.1，对齐同类项目 AgentHub 实测后的 2h 口径）。
	//
	// 为什么不能沿用 60s：11140 的处置是「软冷却 + 连续计数，满 accountFaultThreshold
	// 才摘除」。冷却只有 60s 时，一个风控窗口内 3 次白撞就能在 3 分钟内凑满阈值，
	// 把一个凭证与余额完好的健康号摘掉 —— 2026-10-13 就是这么批量误杀 30 个号的。
	// 阈值本身要表达的是「**持续性**被风控拦」，而不是「同一分钟内撞了三次」。
	// 冷到 2h 后，凑满 3 次至少横跨 6 小时，才真正说明这个号在当前出口/形态下长期不可用。
	//
	// 与 14017（trial not activated）刻意不同：那是"补完 register 就能自愈"，
	// 沿用短软冷却即可；11140 是出口 IP / 频率 / 请求形态问题，风控不会自己好，
	// 1 分钟后被重新选中又是一次白撞（白撞本身也是可聚类的异常信号）。
	accountFaultCooldown = 2 * time.Hour
	// notFoundCooldown 上游 404 的固定短冷却时长（对齐 cmd/server 的 notFoundCooldown）。
	// 偶发路径缺失不是限流信号，不该按 soft_rate 指数升级，更不该喂熔断——固定短冷却兜雪崩。
	notFoundCooldown = 60 * time.Second
	// refreshWithin token 剩余有效期低于此阈值时提前刷新。
	refreshWithin = 5 * time.Minute
	// maxAggregateContentBytes 非流式聚合的内容上限（v0.24.0，P2-4）。
	// 32MB 远大于任何正常回答，只用来挡住"异常上游无限吐内容"把进程打爆。
	// 流式路径不设这个上限：它逐行写出、受 TCP 背压约束，内存天然有界。
	maxAggregateContentBytes = 32 << 20
	// stickyTTL 会话粘性有效期（滚动续期）。
	stickyTTL = 30 * time.Minute
)

// 反代选号 / 防封 配置的 kv 键。
const (
	// kvProxyAccounts 反代选号白名单（uid，逗号分隔）。空 = 不按 uid 过滤。
	kvProxyAccounts = "proxy_accounts"
	// kvProxyGroups 反代选号分组白名单（分组名，逗号分隔）。空 = 不按分组过滤。
	kvProxyGroups = "proxy_groups"
	// kvProxyMinGap 同账号最小复用间隔（秒）。0 = 不限。防封节流用。
	kvProxyMinGap = "proxy_min_gap_seconds"
	// kvPickMode 选号模式：weighted（缺省，三因子加权随机）| roundrobin（顺序游标轮转）。
	// 空/非法一律按 weight 处理（升级后行为与现状一致，新机制须显式开启）。
	kvPickMode = "proxy_pick_mode"
	// kvEgressDefault 全局默认出站代理（http/https/socks5 URL）。账号未单独配置时生效。
	kvEgressDefault = "egress_proxy_default"
	// kvEgressJitterMs 请求前出口抖动上限（毫秒，0/缺省 = 关闭）。
	// 见 Proxy.egressJitter：避免共用同一出口的多个账号在同一毫秒齐发。
	kvEgressJitterMs = "egress_jitter_ms"
	// kvHostShareRatio 代挂分成比例（0~1）。代挂账号在池中服务平台流量后，
	// 按「该次消耗的号池积分 × 该比例」折入号主余额（以平台实收封顶，见 creditHostOwner）。
	// 0 = 不分（关闭分成）。
	kvHostShareRatio = "host_share_ratio"
)

// pickScope 一次选号的可用范围。
//
//   - HostUser == 0 → 平台通用范围：按**消耗分组**选号（v0.13.0）——
//     只取「用户申请 / 反代」两组，并按其优先级逐组挑选；闲置 / 异常不参与。
//     另受全局反代白名单（分组 / uid）约束。平台计费 Key 与管理员密钥走这里。
//   - HostUser  > 0 → 代挂自用范围：仅该用户自己的代挂账号（无视平台白名单与
//     消耗分组——号主用自用 Key 打自己的号，不该被管理员的分组设置挡住），
//     可按 HostGroup 再过滤，并可再按 AccountUIDs（Key 绑定的账号集合）收窄。
type pickScope struct {
	HostUser  int64
	HostGroup string
	// AccountUIDs 该 Key 显式绑定的账号集合（nil = 不按账号过滤）。
	// 生成 Key 时勾选「账号范围」即写入，用于把一把 Key 限定在若干账号上。
	AccountUIDs map[string]bool
}

// isHostSelf 是否代挂自用范围。
func (s pickScope) isHostSelf() bool { return s.HostUser > 0 }

// isPlatform 是否平台通用范围（按消耗分组优先级选号）。
func (s pickScope) isPlatform() bool { return s.HostUser == 0 }

// Proxy 持有账号池、粘性路由器与上游客户端。
//
// 与 cmd/server 的关系：选号/冷却/熔断/粘性/成本分层全部复用 internal/pool 与
// internal/session（即原网关真正的资产），webapp 只负责 Web 侧的入口与持久化。
type Proxy struct {
	// billMu / billView 单模型计费倍率的**不可变快照**（v0.31.67）。
	//
	// 为什么不直接每次读库：结算在请求末尾，每笔都查一次 model_ratios 是纯浪费；
	// 更重要的是"一次请求里读两次配置"会读到两个版本 —— 扣费用的倍率与流水里
	// 记录的倍率可能不是同一个。快照把"这一笔用哪一版"钉住。
	billMu   sync.RWMutex
	billView *billingSnapshot
	store    *Store
	uc       *upstream.Client
	pool     *pool.Pool
	sess     *session.Router
	// sessStore 粘性绑定的持久化后端（v0.17.5）。独立持有引用是为了在 Close 时
	// 排空它内部的异步写队列 —— Router 只按接口持有，关不到它。
	sessStore *stickyStore
	// wafGate WAF IP 级拦截状态机（fail-fast，v0.18.x）。窗内多号接连 WAF 403 →
	// 激活期内轮转遇 WAF 403 直接终止，不把一次客户端请求放大 N 倍打同一出口 IP。
	// 进程内状态、重启清零（窗 60s，重建成本极低）。语义见 internal/rotateguard。
	wafGate rotateguard.WAFGate
	// modelGate 「模型名在上游不存在」（11102）的跨账号 fail-fast 状态机（v0.31.16）。
	//
	// 11102 指向**模型名**、不指向账号：一个不存在的模型名对全池每个号都会得到
	// 同一个答案，换号重试纯属把一次请求放大 N 倍出站、并给每个号白记一条 6h
	// 负缓存（实测：一次假模型名的测速请求犁掉了 32/33 个账号）。闸门按模型名
	// 分桶，激活期内该模型的请求直接终止轮转，别的模型不受影响。
	// 进程内状态、重启清零（窗 60s，重建成本极低）。语义见 internal/rotateguard。
	modelGate rotateguard.ModelBlockGate
	// degrade 降级提示词状态机（v0.31.4，从旧网关移植）。
	//
	// 请求被上游内容策略拦截（400 审核文案 / 11128 渠道拒绝）时，判定多为 system 来源的
	// 指纹误报 → 换极简中性提示词 prompt.Degraded **同请求内重试一次**，并把降级状态
	// 保持到次日 00:00 CST（否则后续同一客户端的每个请求都要先撞一次 400）。
	// 第二次仍被拦 → 说明是用户内容本身触发审核，回内容防火墙错误、不再重试。
	// 进程内状态、重启清零；语义见 internal/prompt.Gate。
	degrade prompt.Gate
	// onCreditChanged 号池积分发生**实时扣减**后的回调（v0.20.0）。
	//
	// 用回调而非反向持有 *app：Proxy 是底层转发组件，反向依赖 app 会形成环，
	// 也让 Proxy 的单元测试必须构造整个 app。回调为 nil 时静默跳过（测试友好）。
	// 生产由 main.go 接到 app.reconcilePoolGroupOne（预备组自动分组）。
	onCreditChanged func(uid string)
	// onAccountsAdded 同步时发现**新增**账号后的回调（v0.23.3），参数为新增 uid。
	//
	// 用途：新入池账号当天往往已过签到触发点（要等到次日才被排到），这里让上层
	// 补跑一次，避免「新加入的账号没有任何定时任务执行记录」。生产由 main.go 接到
	// taskRunner.CatchupNewAccounts；nil 时静默跳过（测试与 -no-tasks 启动路径）。
	//
	// 与 onCreditChanged 同口径：用回调而非反向持有 *app，避免形成依赖环。
	// 只在「首次同步建基线之后」触发（见 SyncAccountsReport 的 prev 判断）。
	onAccountsAdded func(uids []string)
	// relayMu / relayPrice 通道模型的配置单价（model → credit/1k），用于预扣估算。
	//
	// 为什么不直接查库：预扣在**每个请求**的入口，SQLite 只有一条连接，
	// 每请求一次 SELECT 会和结算/心跳抢同一把锁。通道配置变更频率极低，
	// 变更时刷新一次快照即可（见 ReloadRelayPrices）。
	relayMu    sync.RWMutex
	relayPrice map[string]float64

	// costPer1k 观测到的「模型 → credit/1k token」实测单价，用于预扣估算。
	// 见 observeCost：取历史最大值并持久化到 kv，重启后由 loadObservedCosts 读回。
	costMu    sync.RWMutex
	costPer1k map[string]float64

	// cfgMu 串行化「配置重载 + 映射重建」（v0.31.58）。
	//
	// 为什么必须是一把独立的锁：`ReloadSelection` 写的是**内存里的默认出口**，
	// 而 `SyncAccountsReport` 用这个内存值去算 `egressBad`。两者若并发/乱序：
	//   · apply 先写库、再 Sync（读到的还是旧默认）→ 切换"看起来成功、其实没生效"；
	//   · 两个并发切换 → em 是新的、bad 是按旧默认算的，出现"配置串用"。
	// 这把锁覆盖"读库 → 算 bad → 替换映射"整段，使对外可见的永远是一个**自洽快照**。
	//
	// ⚠️ 持锁期间不得回调本类型的公开方法（会自死锁）；`onAccountsAdded` 是异步的，
	// 目前安全，改动它时要重新确认这一点。
	cfgMu sync.Mutex

	// selMu 保护反代选号过滤（分组/白名单）与出站路由（每账号代理/默认代理/节流间隔）。
	selMu         sync.RWMutex
	allowUIDs     map[string]bool   // nil = 全部账号
	allowGroups   map[string]bool   // nil = 全部分组
	groupByUID    map[string]string // uid → 分组名
	egressByUID   map[string]string // uid → 专用出站代理
	defaultEgress string
	// egressJitterMs 请求前抖动上限（毫秒，0 = 关闭）。
	// 目的：避免共用同一出口的多个账号在同一毫秒齐发（见 egressJitter）。
	egressJitterMs int
	minGap         time.Duration

	// ---- 用户代挂（hosting）选号与分成 ----
	ownerByUID     map[string]int64  // uid → 号主 user_id（0 = 平台自持）
	hostGroupByUID map[string]string // uid → 号主自定的代挂分组
	poolInByUID    map[string]bool   // uid → 是否为「已入池」的代挂账号（参与平台反代）
	// consumeGroupByUID uid → 生效的消耗分组（v0.13.0，见 effectiveConsumeGroup）。
	// 平台选号据此按 platformConsumeGroups 的优先级逐组挑选；不在其中的分组不参与。
	consumeGroupByUID map[string]string
	// lastReconcile 上次「异常自动回归」复查的 Unix 秒（atomic 节流，见 reconcileAbnormal）。
	lastReconcile int64
	// lastSyncedUIDs 上一次重同步时的进池账号集合（nil = 尚未同步过）。
	// 仅用于 SyncAccountsReport 的新增/移出计数与首次建基线，不参与选号。
	lastSyncedUIDs map[string]bool
	// egressBad uid → 出站代理不可用的原因（格式非法，或探活判定不可用）。
	//
	// 存在的意义是消灭「静默直连」：代理配错时旧实现只写一行日志、账号照走本机 IP，
	// 管理员以为在做 IP 隔离，实际防封已失效且无感知。现在这类账号会被排除出平台选号
	// （见 allowsSelection），并在界面上标明原因，必须由管理员修复。
	//
	// 注意：空代理是**有意直连**的合法配置（Validate 允许），不进这个表。
	egressBad      map[string]string
	hostShareRatio float64 // 代挂分成比例（0~1）
	referralRatio  float64 // 推广返点比例（0~1）：下级充值 / 下级号池收益时给上级的返点

	// mifMu/modelInFlight 记 **(账号, 模型) 维度的在途请求数**（v0.31.7）。
	//
	// 要解决的问题：模型级冷却（6004）是**响应到达之后**才写入的，于是同一个
	// (账号, 模型) 的并发请求会在冷却写下之前各自选到同一个号 —— 一次上游限额
	// 被放大成 N 次必然失败的请求（而且都从同一个出口 IP 出去）。
	//
	// 处置口径刻意是**软偏好、不是硬互斥**：
	//   - 有别的候选 → 换一个（把重复调度消掉）；
	//   - 该模型只剩这个号 → **照用**（可用性优先，绝不因为"忙"而回 503）。
	// 为什么不硬互斥：实测近 7 天 4086 次请求里，同 (账号,模型) ≤10s 的重复调度有
	// 398 对、其中含失败的 **0** 对（67 次 6004 里也没有一次是它造成的）——
	// 用一个未观测到的问题去换"并发能力下降"不划算；软偏好零成本且不会更差。
	// 计数 modelBusySkips 留证据：将来它涨起来就说明该升级为硬互斥。
	mifMu         sync.Mutex
	modelInFlight map[string]int
	// modelBusySkips 因"该 (账号,模型) 已有在途请求"而改选别的号的次数（/api/admin/live 透出）。
	modelBusySkips atomic.Int64
	// hitMu/lastHit 同账号最近一次被选中的时间戳（Unix 秒），用于最小复用间隔节流。
	hitMu   sync.Mutex
	lastHit map[string]int64

	// cliMu/clients 按出口代理 URL 缓存的 http.Client（每账号代理 = 每账号出口 IP）。
	cliMu   sync.Mutex
	clients map[string]*http.Client

	// 实时请求计数（/metrics 用）：与 DB 流水互补——流水记录成功结算的请求，
	// 这里的 total/ok/fail 含「未走到结算就失败」的（无可用号、上游全挂）。
	reqTotal atomic.Uint64
	reqOK    atomic.Uint64
	reqFail  atomic.Uint64

	// live 实时态势（/api/admin/live 的实时仪表盘）：逐分钟分桶 + 事件流 + 在途。
	live *liveStats

	// onAccountRestricted 上游判定「账号级限制」时的回调（uid, source, display）。
	//
	// 为什么用回调而不是在 Proxy 里直接处置：这类事件要动 **DB + 池 + 健康快照**
	// 三处（见 app.deleteAccountsHard），那是 app 层的职责。Proxy 只做识别与转达，
	// 免得两层边界被打穿（也便于单测只验识别、不拉起整套存储）。
	// 未注入时退化为「停用」，绝不静默放过。
	onAccountRestricted func(uid, source, display string) []string

	// onRetire11140 「11140 连续超阈值」的最终处置回调（v0.24.8）。
	//
	// 为什么不在这里直接 RetireAccount：处置强度由 kv `pool_delete_on_11140` 决定
	// （删除 or 停用 + 归档），而删除要动 DB + 池 + 健康快照三处（deleteAccountsHard）
	// —— 那是 app 层职责，与 onAccountRestricted 同一理由。
	//
	// **未注入时退化为「停用 + 归档」**，等价于开关关闭：绝不静默放过，
	// 但也绝不越权做不可逆的删除。
	onRetire11140 func(uid string) []string
}

// NewProxy 构造转发器并做首次账号同步。
// stateFp 传空串则不落盘池状态（纯内存）；生产建议给 data/pool_state.json。
func NewProxy(store *Store, stateFp string) *Proxy {
	// 从默认 Transport 克隆，保留 ProxyFromEnvironment：
	// 本机出网走 WORKBUDDY 代理（http_proxy），自行构造 Transport 会丢掉代理导致 Dial 失败。
	//
	// R3（v0.26.1）：外层再包一道「本机 / 内网目标直连」过滤。
	// 环境代理同样会接管私网目标（Go 的 httpproxy 只特判 `localhost` 一词，
	// 对 `10.x` / `192.168.x` / `172.16-31.x` 一律照发），后果与 egressClient 那处相同。
	envProxy := http.ProxyFromEnvironment
	shared := http.DefaultTransport.(*http.Transport).Clone()
	shared.Proxy = func(req *http.Request) (*url.URL, error) {
		if req != nil && req.URL != nil && egress.Bypass(req.URL.Hostname()) {
			return nil, nil
		}
		return envProxy(req)
	}
	shared.MaxIdleConns = 100
	shared.MaxIdleConnsPerHost = 10
	shared.IdleConnTimeout = 90 * time.Second
	shared.ResponseHeaderTimeout = 30 * time.Second
	httpClient := &http.Client{Transport: shared, Timeout: 180 * time.Second}
	streamClient := &http.Client{Transport: shared}

	p := pool.New(stateFp)
	p.SetMaxInFlight(0) // 0 = 不限在途（原型单用户场景）

	// 粘性路由：Available* 闭包直接挂到 pool，使绑定号仍可用时才命中。
	// Store 用 SQLite 持久化（v0.17.5，替代原 Noop）：重启不再丢粘性绑定，
	// 直接消除「进程重启 → 同一场对话换号」这一可复现的切换来源。
	sessStore := newStickyStore(store.db)
	sess := session.New(session.Config{
		TTL:               stickyTTL,
		GCInterval:        5 * time.Minute,
		Store:             sessStore,
		Available:         func() []string { return p.AvailableUIDs() },
		AvailableForModel: func(m string) []string { return p.AvailableUIDsForModel(m) },
	})
	sess.StartGC()
	// 冷启动恢复：把上次进程留下的绑定读回内存。**必须在 StartGC 之后、放行请求之前** ——
	// 此前这一句缺失（配的是 Noop，调了也没数据），现在 Store 有真实数据，漏调就等于
	// 持久化白做：重启后内存仍空，下一个请求照样重新分配账号。
	sess.LoadFromStore()

	// 必须以 upstream.New() 为基底：字面量构造会丢掉 ChatBaseCN/BillingBaseCN 默认值，
	// 导致签到/查余额等 billing 域请求 base 为空（unsupported protocol scheme ""）。
	// 仅覆盖需要定制的字段（HTTP 客户端/超时/脱敏/global 路由开关）。
	uc := upstream.New()
	uc.HTTP = httpClient
	uc.ChatHTTP = streamClient
	uc.HeaderTimeout = 30 * time.Second
	uc.IdleTimeout = 300 * time.Second
	uc.SanitizeFingerprints = true
	uc.GlobalEnabled = true // 与 auth 包默认开关一致：允许按 realm 路由 cn/global
	// 设备风控头兜底（v0.25.1）：cmd/server 一直有、webapp 漏装 —— 见 devicecred.go。
	// 不装的话 X-Device-Token 的唯一来源就只剩「每个账号的 device_token」。
	store.installDeviceToken(uc)

	px := &Proxy{
		store:             store,
		uc:                uc,
		pool:              p,
		sess:              sess,
		sessStore:         sessStore,
		costPer1k:         map[string]float64{},
		relayPrice:        map[string]float64{},
		lastHit:           map[string]int64{},
		clients:           map[string]*http.Client{},
		groupByUID:        map[string]string{},
		egressByUID:       map[string]string{},
		ownerByUID:        map[string]int64{},
		hostGroupByUID:    map[string]string{},
		poolInByUID:       map[string]bool{},
		consumeGroupByUID: map[string]string{},
		live:              newLiveStats(),
	}
	// 让 **upstream 自身**的所有出站也按账号选出口（v0.24.3）。
	//
	// 此前只有本文件的转发链路（自建请求 + chatClientFor(uid)）走账号代理，而
	// upstream 包内部的出站**一律走固定 transport（即直连）**：
	//   · 余额 / 积分查询（get-user-resource）—— 签到、健康检测、导入后刷余额全用它；
	//   · 模型列表（FetchModels / fetchV3Models / fetchEnterpriseModels）；
	//   · 活跃上报（report.go）、成长任务（travel.go）；
	//   · 健康检测的对话探活。
	// 后果：**出口代理池配了，但只有 chat 一条路真的走代理** —— 防封成了半成品；
	// 且健康检测的结论不反映账号真实出站路径（配了代理的号，探活仍从本机出去）。
	//
	// 注入而不是让 upstream 认识代理配置：本包才持有 uid → 出口映射与 client 缓存，
	// upstream 只需问一句「这个账号该用哪个 client」。
	uc.ClientForUID = func(uid string) (*http.Client, error) {
		c, err := px.clientFor(uid)
		if err != nil {
			// 配了不可用的代理：**显式报错，绝不回落直连**（v0.24.8）。
			//
			// 旧行为是返回 nil → upstream 回落默认 client（直连）。选号侧确实有
			// EgressError 拦截，但那只覆盖**平台选号（chat）**；本包自己的出站
			// （探活 / 签到 / 积分查询 / 模型列表 / 活跃上报）不看那个拦截，
			// 于是同一个号会「chat 被挡、其它链路照常从本机 IP 出去」。
			return nil, err
		}
		return c, nil
	}
	_ = px.SyncAccounts()
	px.ReloadSelection()
	px.loadObservedCosts()
	// 单模型计费倍率快照（v0.31.67）：必须在返回前加载一次，
	// 否则第一批请求会走"快照为空 → 回落全局"的分支（数值没错，但版本号缺失）。
	px.ReloadBilling()
	// 通道单价快照（v0.31.71）：启动时先加载一次，否则前几个通道请求会按
	// "没有单价 → 全局兜底" 估预扣（数额可能差一个数量级）。
	px.ReloadRelayPrices()
	return px
}

// Close 关停池与粘性 GC（进程退出前调用）。
func (p *Proxy) Close() {
	p.sess.StopGC()
	p.pool.Close()
	// 粘性持久化队列最后收尾（排空已入队的绑定变更）。
	if p.sessStore != nil {
		_ = p.sessStore.Close()
	}
}

// SyncReport 一次「重同步」的结果计数（供管理后台展示，便于确认同步是否真的生效）。
type SyncReport struct {
	// Total 这次从数据库读到的账号总数。
	Total int
	// InPool 其中真正进入内存号池的账号数（凭证不可用的会被跳过，故可能小于 Total）。
	InPool int
	// Added 相比上一次同步新增进池的账号数。
	Added int
	// Removed 相比上一次同步移出号池的账号数。
	Removed int
}

// SyncAccounts 重同步账号（等价于 SyncAccountsReport 丢弃计数）。
func (p *Proxy) SyncAccounts() error {
	_, err := p.SyncAccountsReport()
	return err
}

// SyncAccountsReport 从数据库重建内存号池与选号映射。
//
// 「重同步」到底做了什么（管理后台按钮的语义，务必与文档一致）：
//  1. 重新读 accounts 表，把**新增**的账号加入内存号池、把**已删除**的移出；
//  2. 逐账号把「手动停用」标记同步到池（enabled/disabled）；
//  3. 重建 uid → 分组 / 出站代理 / 代挂归属 / 生效消耗分组 的选号映射。
//
// 它**不**做这些（避免误解）：
//   - 不刷新 token、不查询积分（token 刷新由上游 401 / 定时任务触发；积分由卡片异步拉）；
//   - 不重置池内运行态（冷却、熔断计数、失败计数、已学单价）——
//     pool.SyncToDir 只做增删，不入值，正是为了不让一次重同步抹掉运行期状态。
//
// Added/Removed 需要与「上一次」比较，故首次调用（lastSyncedUIDs 为空）不计新增，
// 只建立基线——否则第一次点会显示「新增 5」，看着像刚导入，其实是既有账号。
// lineState 线路在池里的**运行态**（v0.31.47）。
//
// 与 egress.Validate 的分工：那是"这个 URL 格式对不对"，这是"这条线现在能不能用"。
// 二者缺一不可 —— 只校验格式，就会出现"线路已被探活停用、账号仍被选中"的
// 持续失败（2026-10-19 现场：用户拉起代理后全部请求失败，而页面上线路已停用）。
type lineState struct {
	Name       string
	Enabled    bool
	FailStreak int64
}

// unusableReason 这条线路当前是否不可用；可用返回空串。
func (ls lineState) unusableReason() string {
	if !ls.Enabled {
		return "出口线路已停用"
	}
	if ls.FailStreak >= egressFailThreshold {
		return fmt.Sprintf("出口线路连续 %d 次探活失败", ls.FailStreak)
	}
	return ""
}

// loadLineStates 读线路池，按**规范化 URL** 建索引（与账号侧写入口径一致）。
func (p *Proxy) loadLineStates() map[string]lineState {
	out := map[string]lineState{}
	list, err := p.store.ListEgressProxies()
	if err != nil {
		return out
	}
	for _, lp := range list {
		key := strings.TrimSpace(lp.URL)
		if norm, err := egress.Validate(lp.URL); err == nil && norm != "" {
			key = norm
		}
		out[key] = lineState{Name: lp.Name, Enabled: lp.Enabled, FailStreak: lp.FailStreak}
	}
	return out
}

// computeEgressBad 计算 uid → 出口不可用原因（纯函数，便于单测）。
//
// 判据分两层（v0.31.47 补齐第二层）：
//  1. **配置层**：URL 格式非法 → 出口异常；
//  2. **运行层**：线路在池里，但已停用 / 连续探活失败 → 出口异常。
//
// 第二层此前**完全缺失**，而 egress_ops.go 的注释却写着"账号留在坏线路上 →
// 判 egressBad → 排除出选号"。注释与实现不一致的代价在 2026-10-19 兑现了：
// 线路被自动停用后，绑在它上面的账号仍被正常选中，于是每次请求都连不上 ——
// 用户看到的就是"刚拉起代理，本地和号池的反代请求全部失败"。
//
// 语义边界：**不在池里的 URL 不判坏**。手填的线路本来就可以不在池里，
// 我们没有任何依据说它不能用（探活只覆盖池内线路）。
func computeEgressBad(all []*Account, defEgress string, lines map[string]lineState) map[string]string {
	bad := make(map[string]string, len(all))
	// 默认出口自身的问题（格式错 / 运行态不可用）会波及所有"未指定出口"的账号，
	// 影响面比单个账号大得多，必须一并标出。
	defBad := ""
	if defEgress != "" && !egress.IsDirect(defEgress) {
		// 必须用**规范化后**的 URL 去查线路状态（v0.31.58）。
		//
		// 原来的写法是 `lines[strings.TrimSpace(defEgress)]`，而 `loadLineStates`
		// 的键是 `egress.Validate` 规范化过的 URL（scheme 小写、去尾斜杠、补默认端口）。
		// 只要管理员把默认出口写成 `HTTP://host:port/` 这类等价形式，就查不到那条线路，
		// 于是"默认线路已被停用/连续探活失败"**不会**传播到跟随它的账号 ——
		// 这些号照常被选中、从一个坏出口发出请求，表现就是"切换代理后号池全挂"，
		// 而且按默认出口排障时看不出任何异常。
		if norm, err := egress.Validate(defEgress); err != nil {
			defBad = "全局默认出口代理配置有误：" + err.Error()
		} else if st, ok := lines[norm]; ok {
			if reason := st.unusableReason(); reason != "" {
				defBad = "全局默认" + reason + "（" + st.Name + "）"
			}
		}
	}
	for _, acc := range all {
		// 显式直连：无需校验，且**不**继承全局默认代理（这正是 direct 的语义）。
		if egress.IsDirect(acc.EgressProxy) {
			continue
		}
		cur := strings.TrimSpace(acc.EgressProxy)
		if cur != "" {
			norm, err := egress.Validate(cur)
			if err != nil {
				bad[acc.UID] = err.Error()
				continue
			}
			if st, ok := lines[norm]; ok {
				if reason := st.unusableReason(); reason != "" {
					bad[acc.UID] = reason + "（" + st.Name + "）：请重新拉起本机代理，或把该号改绑到可用线路"
				}
			}
			continue
		}
		// 留空 = 未指定 → 继承默认（默认有问题则全部继承者不可用）。
		if defBad != "" {
			bad[acc.UID] = defBad
		}
	}
	return bad
}

// RefreshEgressBad 只重算「出口异常」映射，不重载池（v0.31.47）。
//
// 为什么不能直接调 SyncAccounts：那是重载整个池（SyncToDir），
// 而线路启停是每 5 分钟一次的探活结果 —— 为了一次健康判定去重载号池，
// 会把在途请求与冷却状态都搅一遍。这里只需要替换一个 map。
func (p *Proxy) RefreshEgressBad() {
	// 与切换共用同一把锁（v0.31.58）：否则"切到一半"时可能按旧默认重算出一份
	// 与 em 不匹配的 bad 表，随后被探活循环固化下来。
	p.cfgMu.Lock()
	defer p.cfgMu.Unlock()
	all, err := p.store.ListAccounts()
	if err != nil {
		return
	}
	p.selMu.RLock()
	def := p.defaultEgress
	p.selMu.RUnlock()
	bad := computeEgressBad(all, def, p.loadLineStates())
	p.selMu.Lock()
	prev := len(p.egressBad)
	p.egressBad = bad
	p.selMu.Unlock()
	if len(bad) != prev {
		log.Printf("INFO: [egress] 出口异常账号数 %d → %d（线路启停后重算）", prev, len(bad))
	}
}

func (p *Proxy) SyncAccountsReport() (SyncReport, error) {
	p.cfgMu.Lock()
	defer p.cfgMu.Unlock()
	return p.syncAccountsReport()
}

// syncAccountsReport 真正的重建逻辑；调用方必须持有 cfgMu。
func (p *Proxy) syncAccountsReport() (SyncReport, error) {
	all, err := p.store.ListAccounts()
	if err != nil {
		return SyncReport{}, err
	}
	auths := make([]*auth.Auth, 0, len(all))
	for _, acc := range all {
		au := authFromAccount(acc)
		if au == nil {
			continue
		}
		auths = append(auths, au)
	}
	p.pool.SyncToDir(auths)
	// 手动停用在 SyncToDir 之后同步：确保账号已入池，否则 SetManualDisabled 找不到。
	for _, au := range auths {
		var disabled bool
		for _, acc := range all {
			if acc.UID == au.UID {
				disabled = acc.Disabled
				break
			}
		}
		p.pool.SetManualDisabled(au.UID, disabled, "web ui")
	}
	// 「只消费」标记（v0.31.40）：以库为准下发到池。
	//
	// 为什么放在 SyncAccounts 而不是每处刷新点现查库：刷新入口分布在 scheduler / proxy / checkin
	// 三处（scheduler 只拿得到 *auth.Auth 与 pool.Status，拿不到 store），
	// 放进池状态才能让"三处纪律一致"成为结构性事实，而不是靠每处记得查一次。
	passive := p.store.RefreshModeMap()
	for _, au := range auths {
		if _, changed := p.pool.SetPassiveRefresh(au.UID, passive[au.UID]); changed && passive[au.UID] {
			log.Printf("INFO: [refresh] %s 进入「只消费」模式（不再主动续期）", au.UID)
		}
	}
	// 重建 uid → 分组 / 出站代理 / 代挂归属 映射（反代选号过滤、出站路由、分成用）。
	gm := make(map[string]string, len(all))
	em := make(map[string]string, len(all))
	om := make(map[string]int64, len(all))
	hg := make(map[string]string, len(all))
	inPool := make(map[string]bool, len(all))
	cgm := make(map[string]string, len(all))
	for _, acc := range all {
		gm[acc.UID] = acc.Group
		em[acc.UID] = acc.EgressProxy
		om[acc.UID] = acc.OwnerUserID
		hg[acc.UID] = acc.HostGroup
		// 「已入池」只对代挂账号有意义；平台自持账号不靠该位（allowsSelection 另行放行）。
		inPool[acc.UID] = acc.OwnerUserID > 0 && acc.PoolState == "in"
		// 生效消耗分组：显式设置优先，否则按来源推断（v0.13.0）。
		cgm[acc.UID] = effectiveConsumeGroup(acc)
	}
	// 出口范围兜底（R12，v0.31.50）：写入口已经统一收口，但数据会被迁移脚本、
	// 手工 SQL、历史版本改。规则如果只在写入口生效，一次脏数据就绕过全部纪律 ——
	// 而这里的后果是"某个人的 access_token 从别人的家庭宽带出去"。
	// 违规映射**降级为跟随默认出口**并留 WARN 台账；不自动改库（避免批量断流）。
	if p.store != nil {
		if proxies, err := p.store.ListEgressProxies(); err == nil {
			if _, dropped := egressScopeFilter(em, proxies, all); dropped > 0 {
				log.Printf("WARN: [egress] 选号映射中发现 %d 个越界出口，已降级为跟随默认（详见上行 scope violation 日志）", dropped)
			}
		}
	}
	// 出口异常判定（v0.31.47）：**配置层 + 运行层**，见 computeEgressBad。
	//
	// 此前这里只做格式校验，于是「线路已被探活停用」不会被反映到选号上 ——
	// 那条线路上的账号照常被选中、每次请求都失败。
	p.selMu.RLock()
	defEgress := p.defaultEgress
	p.selMu.RUnlock()
	bad := computeEgressBad(all, defEgress, p.loadLineStates())
	// 进池集合（用于计数）：以真正进池的凭证为准。
	now := make(map[string]bool, len(auths))
	for _, au := range auths {
		now[au.UID] = true
	}
	rep := SyncReport{Total: len(all), InPool: len(auths)}
	p.selMu.Lock()
	prev := p.lastSyncedUIDs
	p.lastSyncedUIDs = now
	p.groupByUID = gm
	p.egressByUID = em
	p.ownerByUID = om
	p.hostGroupByUID = hg
	p.poolInByUID = inPool
	p.consumeGroupByUID = cgm
	p.egressBad = bad
	p.selMu.Unlock()
	if prev != nil { // 首次同步只建基线，不报「新增」
		added := make([]string, 0, 4)
		for uid := range now {
			if !prev[uid] {
				rep.Added++
				added = append(added, uid)
			}
		}
		for uid := range prev {
			if !now[uid] {
				rep.Removed++
			}
		}
		// 新增账号通知（v0.23.3）：让上层为新号补跑一轮签到。
		//
		// 为什么放在 selMu 解锁之后：钩子会起 goroutine 打上游，持锁调用会阻塞
		// 全部选号路径（sync 被 20 多处请求处理调用）。这里只读一次函数指针。
		if len(added) > 0 && p.onAccountsAdded != nil {
			p.onAccountsAdded(added)
		}
	}
	return rep, nil
}

// splitKVSet 把逗号分隔的配置串拆成集合；全空则返回 nil（语义 = 不过滤）。
func splitKVSet(csv string) map[string]bool {
	var out map[string]bool
	for _, part := range strings.Split(csv, ",") {
		v := strings.TrimSpace(part)
		if v == "" {
			continue
		}
		if out == nil {
			out = map[string]bool{}
		}
		out[v] = true
	}
	return out
}

// ApplyEgressChange 一次性完成「重载默认出口 + 重建选号映射」（v0.31.58）。
//
// 这是**切换出口**唯一应该调用的入口。分开调用两个方法会留下一个窗口：
// 重载完成、重建未完成时，新请求读到旧映射；反过来则是 `egressBad` 按旧默认算。
// 这个窗口正是"管理员一换代理，普通用户就开始报错"的成因之一。
//
// 顺序固定为 reload → sync：先让内存里的默认出口追上库，再用它评估每条线路可用性。
func (p *Proxy) ApplyEgressChange() error {
	p.cfgMu.Lock()
	defer p.cfgMu.Unlock()
	p.reloadSelection()
	_, err := p.syncAccountsReport()
	return err
}

// ReloadSelection 从 kv 读回反代选号（白名单/分组）+ 防封（最小复用间隔/默认出站代理）配置。
// 管理员改设置或账号分组后调用；只改过滤/路由参数，不触碰池状态。
func (p *Proxy) ReloadSelection() {
	p.cfgMu.Lock()
	defer p.cfgMu.Unlock()
	p.reloadSelection()
}

// SelectableUIDsForModel 平台视角下**此刻能服务该模型**的账号集合（v0.31.59）。
//
// 与 `allowsSelection` 使用同一份规则（白名单 / 消耗分组 / 出口健康），
// 因此冷却卡片的 full/partial 与真实选号不会分叉。代价是遍历一次账号表，
// 但只对有冷却记录的模型调用（通常 0~3 个），且外层有 5 秒缓存。
func (p *Proxy) SelectableUIDsForModel(model string) []string {
	cands := p.pool.AvailableUIDsForModel(model)
	if len(cands) == 0 {
		return nil
	}
	p.selMu.RLock()
	defer p.selMu.RUnlock()
	out := make([]string, 0, len(cands))
	for _, uid := range cands {
		if p.allowsSelectionLocked(uid, pickScope{}) {
			out = append(out, uid)
		}
	}
	return out
}

// reloadSelection 真正的重载逻辑；调用方必须持有 cfgMu。
func (p *Proxy) reloadSelection() {
	accts, _ := p.store.GetKV(kvProxyAccounts)
	groups, _ := p.store.GetKV(kvProxyGroups)
	gap, _ := p.store.GetKV(kvProxyMinGap)
	def, _ := p.store.GetKV(kvEgressDefault)
	jitter, _ := p.store.GetKV(kvEgressJitterMs)
	share, _ := p.store.GetKV(kvHostShareRatio)
	refShare, _ := p.store.GetKV(kvReferralRatio)
	pickMode, _ := p.store.GetKV(kvPickMode)

	var gapDur time.Duration
	if n, err := strconv.Atoi(strings.TrimSpace(gap)); err == nil && n > 0 {
		gapDur = time.Duration(n) * time.Second
	}
	// 抖动上限：0/非法 = 关闭（默认关闭，不给正常路径凭空加延迟）。
	jitterMs := 0
	if n, err := strconv.Atoi(strings.TrimSpace(jitter)); err == nil && n > 0 {
		if n > 5000 {
			n = 5000 // 防御：上限 5 秒，再大就不是「抖动」而是故障了
		}
		jitterMs = n
	}
	ratio := 0.0
	if f, err := strconv.ParseFloat(strings.TrimSpace(share), 64); err == nil && f > 0 {
		if f > 1 {
			f = 1
		}
		ratio = f
	}
	refRatio := 0.0
	if f, err := strconv.ParseFloat(strings.TrimSpace(refShare), 64); err == nil && f > 0 {
		if f > 1 {
			f = 1
		}
		refRatio = f
	}
	p.selMu.Lock()
	p.allowUIDs = splitKVSet(accts)
	p.allowGroups = splitKVSet(groups)
	p.defaultEgress = strings.TrimSpace(def)
	p.minGap = gapDur
	p.egressJitterMs = jitterMs
	p.hostShareRatio = ratio
	p.referralRatio = refRatio
	p.selMu.Unlock()
	// 选号模式注入池（pool 自带锁，与 selMu 无嵌套，故放在解锁之后调用）。
	// 归一化交给 pool.SetPickMode（只有 roundrobin 生效，其余回落 weighted），
	// 这里不重复判断，避免两处口径分裂。
	p.pool.SetPickMode(strings.TrimSpace(pickMode))
}

// allowsSelection 判定某账号是否落在本次选号的可用范围内。
//
// 平台通用（sc.HostUser==0）：只接受**参与消耗**的消耗分组（用户申请 / 反代）；
// 闲置与异常一律不选中，再叠加全局白名单（uid / 分组）。
// 代挂自用（sc.HostUser>0）：仅号主本人的账号，可按代挂分组过滤，不看消耗分组。
func (p *Proxy) allowsSelection(uid string, sc pickScope) bool {
	p.selMu.RLock()
	defer p.selMu.RUnlock()
	return p.allowsSelectionLocked(uid, sc)
}

// allowsSelectionLocked 规则本体；调用方必须持有 selMu 读锁。
//
// 抽出来是为了让"状态展示"与"真实选号"共用同一份规则（v0.31.59，审查 F11）：
// 冷却卡片此前用 pool.AvailableUIDsForModel 判 full/partial，而真实选号还叠加了
// 白名单、消耗分组、出口健康等条件 —— 于是"全池有号可用"被展示成 partial，
// 而当前请求实际上一个号都选不出来。
func (p *Proxy) allowsSelectionLocked(uid string, sc pickScope) bool {
	if sc.HostUser > 0 {
		if p.ownerByUID[uid] != sc.HostUser {
			return false
		}
		if sc.HostGroup != "" && p.hostGroupByUID[uid] != sc.HostGroup {
			return false
		}
		// Key 绑定账号集合：勾过就只在集合内选号（空集合 = 未绑定，不过滤）。
		if sc.AccountUIDs != nil && !sc.AccountUIDs[uid] {
			return false
		}
		return true
	}
	// 平台范围（v0.13.0）：消耗分组决定是否参与。uid 不在映射中 = 未知账号，一律不选。
	if !consumeGroupParticipates(p.consumeGroupByUID[uid]) {
		return false
	}
	// 出口异常（v0.14.0）：代理配置有误的账号不参与平台流量。
	// 宁可这个请求失败（返回无可用账号），也不让账号从本机 IP 裸奔出去 ——
	// 后者会让管理员在毫不知情的情况下失去全部出口隔离。
	if _, isBad := p.egressBad[uid]; isBad {
		return false
	}
	if p.allowUIDs != nil && !p.allowUIDs[uid] {
		return false
	}
	// 白名单同时认两种分组名：传统分组（account_group，如「桌面端」）与消耗分组
	// 中文名（如「反代」「用户申请」）。账号卡片自 v0.13.0 起只暴露消耗分组，
	// 若这里只认旧字段，「反代分组白名单」就会变成一个填了却永远筛不到人的陷阱。
	if p.allowGroups != nil {
		g, cg := p.groupByUID[uid], consumeGroupLabels[p.consumeGroupByUID[uid]]
		if !p.allowGroups[g] && (cg == "" || !p.allowGroups[cg]) {
			return false
		}
	}
	return true
}

// hostShareRatioNow 读取代挂分成比例（0 = 关闭分成）。
func (p *Proxy) hostShareRatioNow() float64 {
	p.selMu.RLock()
	defer p.selMu.RUnlock()
	return p.hostShareRatio
}

// referralRatioNow 读取推广返点比例（0 = 关闭推广返点）。
func (p *Proxy) referralRatioNow() float64 {
	p.selMu.RLock()
	defer p.selMu.RUnlock()
	return p.referralRatio
}

// recentlyHit 该账号是否在最小复用间隔内刚被用过（防封节流；间隔为 0 时恒 false）。
func (p *Proxy) recentlyHit(uid string) bool {
	p.selMu.RLock()
	gap := p.minGap
	p.selMu.RUnlock()
	if gap <= 0 {
		return false
	}
	p.hitMu.Lock()
	last := p.lastHit[uid]
	p.hitMu.Unlock()
	return last > 0 && time.Since(time.Unix(last, 0)) < gap
}

// noteChatSuccess 一次「账号真的服务成功」的统一记账（v0.31.0）。
//
// 两件事必须同进同出，故收在一个入口里：
//   - NoteSuccess：清失败/熔断等运行态阻塞位；
//   - BlockModelClear：清该 (账号, 模型) 的 11102「该后端无此模型」负缓存 ——
//     **这一步此前只在旧网关有**（internal/server/handler.go），webapp 侧一直缺，
//     于是 webapp 记下的 11102 条目只能等 TTL（6h 起、封顶 24h）自然到期。
//     过去它被"签到解冻会清空整张模型冷却表"这个副作用掩盖着；v0.31.0 修掉那个
//     副作用之后，这条**设计好的**恢复路径必须真的存在（否则 11102 条目再没有
//     提前解除的手段 —— 见 transition.go clearAccountCoolingLocked）。
//
// 只清 11102 条目（按 reason 前缀识别），6004 的独立冷却不受影响：6004 有上游给的
// 重置墙钟，一次成功不该把它抹掉（见 pool.NoteSuccess 注释）。
//
// 触发场景是**并发窗口**：11102 条目可能由另一个请求在本次选号之后写入，而本次
// 请求其实成功了 —— 不清的话一个陈旧的"后端无此模型"会把该模型挡满整个 TTL。
func (p *Proxy) noteChatSuccess(uid, model string) {
	p.pool.NoteSuccess(uid)
	p.pool.BlockModelClear(uid, model)
}

// modelKey (账号, 模型) 维度的在途计数键。用 \x1f 分隔：它不可能出现在 uid 或
// 模型名里，避免 "a|b" + "c" 与 "a" + "b|c" 撞键（本项目既有口径，见 costExplore 的键）。
func modelBusyKey(uid, model string) string { return uid + "\x1f" + model }

// noteModelInFlight 记一次「该 (账号, 模型) 开始了在途请求」。model 为空时不记
// （没有模型维度就没有这个问题，见 pool 侧"空模型退化账号级"的同一纪律）。
func (p *Proxy) noteModelInFlight(uid, model string) {
	if uid == "" || model == "" {
		return
	}
	p.mifMu.Lock()
	if p.modelInFlight == nil {
		p.modelInFlight = map[string]int{}
	}
	p.modelInFlight[modelBusyKey(uid, model)]++
	p.mifMu.Unlock()
}

// clearModelInFlight 在途请求结束时递减。**必须与 noteModelInFlight 严格配对** ——
// 计数泄漏会让这个号被永久"偏爱跳过"，是比原问题更糟的形态（所以所有释放路径
// 都收敛到 releaseHeld 这一个入口）。
func (p *Proxy) clearModelInFlight(uid, model string) {
	if uid == "" || model == "" {
		return
	}
	p.mifMu.Lock()
	k := modelBusyKey(uid, model)
	if n := p.modelInFlight[k]; n <= 1 {
		delete(p.modelInFlight, k) // 归零即删键：map 不无限膨胀
	} else {
		p.modelInFlight[k] = n - 1
	}
	p.mifMu.Unlock()
}

// modelBusy 该 (账号, 模型) 是否已有在途请求。供选号做"优先换一个"的软偏好。
func (p *Proxy) modelBusy(uid, model string) bool {
	if uid == "" || model == "" {
		return false
	}
	p.mifMu.Lock()
	n := p.modelInFlight[modelBusyKey(uid, model)]
	p.mifMu.Unlock()
	return n > 0
}

// noteHit 记账「该账号刚被用于一次请求」，供节流判定。
func (p *Proxy) noteHit(uid string) {
	p.hitMu.Lock()
	if p.lastHit == nil {
		p.lastHit = map[string]int64{}
	}
	p.lastHit[uid] = time.Now().Unix()
	p.hitMu.Unlock()
}

// pickExcluding 在「未被 tried 排除」的账号里选一个，且必须通过范围过滤；
// 若存在未在最小复用间隔内的账号则优先返回它，否则回落到任一可用账号。
//
// v0.13.0（消耗分组）：平台通用范围按 platformConsumeGroups 的顺序逐组选号 ——
// 先「用户申请」（号主申请入池的账号，优先消耗其积分），选不到才轮到「反代」；
// 闲置 / 异常不参与，**不设兜底**（宁缺勿滥，避免偷用未授权账号）。
//
// 每组用 tried 的**局部副本**：组内选号会把落选账号标记进 tried 以免死循环，
// 若直接写共享的 tried，前一组的落选账号会被后一组误当作「本次已试过」。
func (p *Proxy) pickExcluding(tried map[string]bool, model string, sc pickScope) *auth.Auth {
	return p.pickExcludingWith(tried, model, sc, func(t map[string]bool, m string) *auth.Auth {
		return p.pool.PickExcludingForRealm(t, m, "")
	})
}

// pickProbeExcluding 模型级**半开探测**取号（v0.31.6）：与 pickExcluding 同构，
// 只把取号原语换成"对该模型正处可探测冷却、且账号级完全健康"的号。
//
// 为什么复用同一套范围/分组/节流逻辑而不是另写一个简化版：探测发出的是**真实请求**，
// 它必须和正常请求受同样的约束 —— 代挂自用不能拿平台号去探测（越权）、平台范围不能
// 用别人分组的号、节流（recentlyHit）同样要生效。任何"探测路径放宽一点"都是权限漏洞。
func (p *Proxy) pickProbeExcluding(tried map[string]bool, model string, sc pickScope) *auth.Auth {
	return p.pickExcludingWith(tried, model, sc, func(t map[string]bool, m string) *auth.Auth {
		return p.pool.PickProbeForModelRealm(t, m, "")
	})
}

// pickOneFn 单次取号原语：常规轮换与半开探测共用同一套「分组优先级 + 范围过滤 + 节流」
// 外壳，只有"从池里怎么挑一个"这一步不同。抽出来是为了让两条路径的**约束**不可能漂移
// —— 本项目最贵的一类教训就是"同一件事两套实现"。
type pickOneFn func(tried map[string]bool, model string) *auth.Auth

func (p *Proxy) pickExcludingWith(tried map[string]bool, model string, sc pickScope, one pickOneFn) *auth.Auth {
	// 顺手复查「临时异常」账号是否已恢复（节流 + 异步，见 reconcileAbnormal）。
	p.reconcileAbnormal()
	if sc.isPlatform() {
		for _, g := range platformConsumeGroups {
			if au := p.pickScopedWith(copyTried(tried), model, sc, g, one); au != nil {
				return au
			}
		}
		return nil
	}
	// 代挂自用：不限消耗分组（号主自己的号，按 HostGroup / AccountUIDs 收窄）。
	return p.pickScopedWith(tried, model, sc, "", one)
}

// pickPriorityGroupForModel 返回当前**有可用账号的最高优先级**消耗分组（无则空串）。
//
// 判定口径与 pickExcluding 的分组优先级一致：按 platformConsumeGroups 顺序找第一个
// 「存在既通过 allowsSelection、又属于该组」的可用账号。model 非空时按模型过滤
// （6004 模型级冷却生效）；池内无模型级可用时退回账号级可用集。
//
// 用于粘性会话的优先级守卫：粘性绑定不得绕过「用户申请 > 反代」。
func (p *Proxy) pickPriorityGroupForModel(model string, sc pickScope) string {
	avail := p.pool.AvailableUIDsForModel(model)
	if len(avail) == 0 {
		avail = p.pool.AvailableUIDs()
	}
	for _, g := range platformConsumeGroups {
		for _, uid := range avail {
			// 两个调用各自持锁，顺序调用不嵌套（RWMutex 不可重入读锁）。
			if p.allowsSelection(uid, sc) && p.inConsumeGroup(uid, g) {
				return g
			}
		}
	}
	return ""
}

// stickyPriorityOK 粘性绑定号的消耗分组是否满足当前优先级。
//
// 背景（实测缺陷）：session.Router 在未命中时会按 key 哈希从「池内可用集」直接分配
// 一个账号，而该可用集**不含消耗分组过滤** —— 于是新会话可能被绑到「反代」组账号，
// 而 handleChat 会直接复用该绑定、不经过 pickExcluding，导致「用户申请优先」被绕过。
// 实测：apply 组有 2 个可用账号时，8 次请求仍有 3 次落到 proxy 组（全部 status=ok，
// 不是失败回退，即 proxy 被当成首选）。
//
// 这里做守卫：平台范围下要求绑定号属于「当前最高优先级的可用分组」，否则视为未命中
// （上层解绑 → 回落 pickExcluding 的正确分组选号）。代挂自用范围不受分组管辖，恒放行
// ——避免把号主自己的粘性打散。
func (p *Proxy) stickyPriorityOK(uid, model string, sc pickScope) bool {
	if !sc.isPlatform() {
		return true
	}
	want := p.pickPriorityGroupForModel(model, sc)
	if want == "" {
		return true // 无任何可用分组（理论上不会走到）：不额外限制，交给常规校验兜底
	}
	return p.inConsumeGroup(uid, want)
}

// copyTried 复制 tried 集合，供分组轮次隔离使用。
func copyTried(tried map[string]bool) map[string]bool {
	local := make(map[string]bool, len(tried))
	for k := range tried {
		local[k] = true
	}
	return local
}

// pickScoped 单轮选号：从池里反复取候选，跳过范围外的账号。
// group 非空时额外要求候选的生效消耗分组等于 group（平台范围按组逐轮选号用）。
//
// 上限 512：代挂自用场景下，个人账号可能只是池中很小一部分，需要多轮跳过平台账号
// 才能命中自己的号；512 远超实际账号规模，又不会成为死循环。
func (p *Proxy) pickScopedWith(tried map[string]bool, model string, sc pickScope, group string, one pickOneFn) *auth.Auth {
	var fallback, busyFallback *auth.Auth
	for i := 0; i < 512; i++ {
		cand := one(tried, model)
		if cand == nil {
			break
		}
		if !p.allowsSelection(cand.UID, sc) || (group != "" && !p.inConsumeGroup(cand.UID, group)) {
			tried[cand.UID] = true
			continue
		}
		// 该 (账号, 模型) 已有在途请求 → **优先换一个**（把重复调度消灭在冷却写入之前），
		// 但不排除它：候选只剩它时下面的 busyFallback 仍会把请求交出去。
		// 标记 tried 是有意的：同一客户端请求后续轮次也不该再选它（省一次无谓比较）。
		if p.modelBusy(cand.UID, model) {
			if busyFallback == nil {
				busyFallback = cand
			}
			p.modelBusySkips.Add(1)
			tried[cand.UID] = true
			continue
		}
		if fallback == nil {
			fallback = cand
		}
		if !p.recentlyHit(cand.UID) {
			return cand
		}
		tried[cand.UID] = true
	}
	if fallback != nil {
		return fallback
	}
	return busyFallback
}

// inConsumeGroup 该账号生效的消耗分组是否等于 group。
func (p *Proxy) inConsumeGroup(uid, group string) bool {
	p.selMu.RLock()
	defer p.selMu.RUnlock()
	return p.consumeGroupByUID[uid] == group
}

// ConsumeGroupForUID 返回账号生效的消耗分组（空串 = 未知账号）。供运维接口使用。
func (p *Proxy) ConsumeGroupForUID(uid string) string {
	p.selMu.RLock()
	defer p.selMu.RUnlock()
	return p.consumeGroupByUID[uid]
}

// SetConsumeGroupInMemory 只更新「uid → 生效消耗分组」这一条内存映射（v0.20.0）。
//
// 为什么需要：自动分组（预备组进出）在积分变更后高频触发，走不了整表
// SyncAccounts —— 那要重读全部账号并重建全部映射，代价与账号数成正比。
// 本方法 O(1)，只改一条，语义与 SyncAccountsReport 里对该账号的赋值一致。
func (p *Proxy) SetConsumeGroupInMemory(uid, group string) {
	p.selMu.Lock()
	defer p.selMu.Unlock()
	if p.consumeGroupByUID == nil {
		p.consumeGroupByUID = map[string]string{}
	}
	p.consumeGroupByUID[uid] = group
}

// isPoolIn 该账号是否为「已入池」的代挂账号（保留供运维与测试引用）。
func (p *Proxy) isPoolIn(uid string) bool {
	p.selMu.RLock()
	defer p.selMu.RUnlock()
	return p.poolInByUID[uid]
}

// clientFor 返回该账号应使用的 HTTP 客户端。
//
// 出口解析顺序（v0.14.1 起，见 ResolveEgress）：
//  1. 账号显式设为 direct → **真·直连**（Proxy=nil，绕过环境代理）
//  2. 账号配了代理 URL → 该代理的专用客户端
//  3. 账号未指定（空）→ 继承全局默认代理；默认也为空 → 共享客户端
//
// 严格语义（v0.14.0，消灭静默直连）：配了代理但校验不通过 → 返回 error，
// **绝不回落到直连**。旧实现在解析失败时只打一行日志就返回直连客户端，于是
// 「代理填错一个字符」会让账号悄悄从本机 IP 出去，而界面仍显示「已配代理」，
// 防封静默失效。返回 error 让调用方把这个号判失败并记录明确原因。
func (p *Proxy) clientFor(uid string) (*http.Client, error) {
	switch eff := p.ResolveEgress(uid); {
	case egress.IsDirect(eff):
		// 显式直连：用不带代理的客户端，**不**走环境代理。
		// 与「未指定」的区别在此：未指定保留环境代理行为（兼容既有部署），
		// 显式直连则给出「一定不加代理」的强保证。
		return p.directClient()
	case eff == "":
		return p.uc.HTTP, nil
	default:
		return p.egressClient(eff)
	}
}

// chatClientFor 返回本次 chat 转发应使用的 HTTP 客户端。
//
// **为什么流式必须换掉 180s 总超时**（v0.17.5 修复）：
// http.Client.Timeout 的官方语义是「含连接、重定向**以及读取响应体**的总时限」。
// clientFor 返回的三个客户端（uc.HTTP / directClient / egressClient）都带
// `Timeout: 180s` —— 用在流式上等于「上游生成超过 3 分钟就掐断连接」。
// 现实里长回答 / Agent 多轮工具调用 / reasoning 深度思考轻易超过 3 分钟，于是
// 连接被**网关自己**切断，客户端表现为「执行到一半会话中断」，且因为
// streamSSE 此前不检查 scanner.Err()、不补 [DONE]，这个失败是完全静默的。
//
// 修法不是「把 180s 调大」（那只是把悬崖挪远一点），而是让流式请求**不受总时限约束**：
// 只保留 Transport 层的 ResponseHeaderTimeout(30s) 守住「连上了但迟迟不给响应头」，
// 响应体读取交给客户端断连（r.Context() 取消）与上游自然收尾来终止 ——
// 这正是 NewProxy 里 streamClient 的既有设计（uc.ChatHTTP），只是主转发路径没用上。
//
// 非流式（服务端聚合）保留 180s：那时确实需要一个上界，否则单个卡死的上游会长期占住请求。
func (p *Proxy) chatClientFor(uid string, stream bool) (*http.Client, error) {
	c, err := p.clientFor(uid)
	if err != nil || !stream || c == nil || c.Timeout == 0 {
		return c, err
	}
	// 复用同一个 Transport（连接池/代理设置全部保留），只脱掉 Client 级总超时。
	// 不缓存：http.Client 本身极轻，Transport 才是重的，而 Transport 是共享的。
	return &http.Client{Transport: c.Transport}, nil
}

// ResolveEgress 解析某账号**最终生效**的出口配置。
//
// 返回值语义（三态，务必按此理解，否则会误判「裸奔」）：
//
//	"direct"       显式直连（覆盖全局默认）
//	""             未指定且全局默认也为空 → 实际是直连（但语义上是「未指定」）
//	"http://..."   生效的代理 URL（账号自配，或继承的全局默认）
//
// 需要「是否真的从本机 IP 出去」的判断时，应同时把 "direct" 与 "" 视为直连 ——
// 见 egressOverview 与 isDirectEgress。
func (p *Proxy) ResolveEgress(uid string) string {
	p.selMu.RLock()
	own := p.egressByUID[uid]
	def := p.defaultEgress
	p.selMu.RUnlock()
	if egress.IsDirect(own) {
		return egress.Direct
	}
	if own != "" {
		return own
	}
	return def
}

// directClient 返回「真·直连」客户端（Proxy=nil，不复用环境代理）。
//
// 与 uc.HTTP 的区别：uc.HTTP 源自 http.DefaultTransport.Clone()，保留
// ProxyFromEnvironment —— 在后端进程若存在 http_proxy 环境变量时，它其实会走该代理。
// 显式直连需要的是「无论如何都不加代理」，故单独建一个并缓存。
func (p *Proxy) directClient() (*http.Client, error) {
	p.cliMu.Lock()
	defer p.cliMu.Unlock()
	const key = "\x00direct"
	if p.clients == nil {
		p.clients = map[string]*http.Client{}
	}
	if c, ok := p.clients[key]; ok {
		return c, nil
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil // 显式清空：既不走环境代理，也不走任何 URL 代理
	tr.MaxIdleConnsPerHost = 10
	tr.ResponseHeaderTimeout = 30 * time.Second
	tr.IdleConnTimeout = 90 * time.Second
	c := &http.Client{Transport: tr, Timeout: 180 * time.Second}
	p.clients[key] = c
	return c, nil
}

// DefaultEgress 返回当前全局默认出口代理（空 = 未配置）。
func (p *Proxy) DefaultEgress() string {
	p.selMu.RLock()
	defer p.selMu.RUnlock()
	return p.defaultEgress
}

// isDirectEgress 判断解析结果是否**实际**从本机 IP 出去（显式直连，或未指定且无默认）。
func isDirectEgress(resolved string) bool {
	return resolved == "" || egress.IsDirect(resolved)
}

// egressClient 按代理 URL 缓存一个带该代理的 http.Client（每账号代理 = 每账号出口 IP）。
//
// 代理 URL 非法时返回 error，**不**构造直连客户端（见 clientFor 的说明）。
// 用 egress.Validate 而非裸 url.Parse：后者会放过 socks5h 这类 Go 不支持协商的
// scheme，问题会推迟到真正发请求时才暴露，报错也更难懂。
func (p *Proxy) egressClient(proxyURL string) (*http.Client, error) {
	p.cliMu.Lock()
	defer p.cliMu.Unlock()
	if p.clients == nil {
		p.clients = map[string]*http.Client{}
	}
	if c, ok := p.clients[proxyURL]; ok {
		return c, nil
	}
	norm, err := egress.Validate(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("出站代理不可用：%v", err)
	}
	if norm == "" {
		return nil, errors.New("出站代理不可用：地址为空（清空该账号的代理配置可恢复直连）")
	}
	u, err := url.Parse(norm)
	if err != nil {
		return nil, fmt.Errorf("出站代理不可用：%v", err)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	// R3（v0.26.1）：**本机 / 内网目标不经代理**。
	//
	// 不能直接用 http.ProxyURL(u)：那会让代理接管**所有**出站请求，包括健康探测与
	// 局域网端点 —— 内网目标被送进代理必然连不上，而界面上线路仍显示「正常」，
	// 于是故障表现为「本地端点不可达」，与出口配置的因果关系完全看不出来。
	// （同类项目 2026-10-12 修过同一类 bug：「配了 WB_HTTP_PROXY 也能连上上游」。）
	tr.Proxy = func(req *http.Request) (*url.URL, error) {
		if req == nil || req.URL == nil {
			return u, nil
		}
		if egress.Bypass(req.URL.Hostname()) {
			return nil, nil // 直连
		}
		return u, nil
	}
	tr.MaxIdleConnsPerHost = 10
	tr.ResponseHeaderTimeout = 30 * time.Second
	tr.IdleConnTimeout = 90 * time.Second
	c := &http.Client{Transport: tr, Timeout: 180 * time.Second}
	p.clients[proxyURL] = c
	return c, nil
}

// EgressForUID 暴露某账号当前生效的出站代理（专用优先，其次全局默认）；供 UI 展示与排障。
func (p *Proxy) EgressForUID(uid string) string {
	p.selMu.RLock()
	defer p.selMu.RUnlock()
	if e := p.egressByUID[uid]; e != "" {
		return e
	}
	return p.defaultEgress
}

// EgressErrorForUID 返回该账号出站代理不可用的原因（空串 = 正常/有意直连）。
// 非空表示该账号已因出口配置问题被排除出平台选号，需要管理员修复。
func (p *Proxy) EgressErrorForUID(uid string) string {
	p.selMu.RLock()
	defer p.selMu.RUnlock()
	return p.egressBad[uid]
}

// EgressBadUIDs 返回所有出口异常账号及其原因（供看板与告警聚合）。
func (p *Proxy) EgressBadUIDs() map[string]string {
	p.selMu.RLock()
	defer p.selMu.RUnlock()
	out := make(map[string]string, len(p.egressBad))
	for k, v := range p.egressBad {
		out[k] = v
	}
	return out
}

// SelectionSummary 返回当前选号过滤的只读摘要（供 /status、设置页展示）。
func (p *Proxy) SelectionSummary() map[string]any {
	p.selMu.RLock()
	uids := make([]string, 0, len(p.allowUIDs))
	for u := range p.allowUIDs {
		uids = append(uids, u)
	}
	groups := make([]string, 0, len(p.allowGroups))
	for g := range p.allowGroups {
		groups = append(groups, g)
	}
	out := map[string]any{
		"proxy_accounts":        strings.Join(uids, ","),
		"proxy_groups":          strings.Join(groups, ","),
		"proxy_min_gap_seconds": int(p.minGap / time.Second),
		"egress_proxy_default":  p.defaultEgress,
	}
	p.selMu.RUnlock()
	// 选号模式存在 pool（读取走 pool 自己的锁）：放在 selMu 释放之后，避免锁嵌套。
	out["pick_mode"] = p.pool.PickMode()
	return out
}

// 上游 chat base 一律走 upstream.Client.ChatBaseFor（v0.28.0）。
//
// 此前这里有一个本地的 `chatBaseFor(realm)`，把两个官方域名**硬编码**在本文件里 ——
// 于是 upstream 的 `ChatBaseCN` / `ChatBaseGlobal`（可配置）只对包内出站
// （签到 / 查余额 / 模型列表）生效，而**占比最大的转发链路**永远走硬编码域名：
// 运维把 base 指向自建镜像时会看到"配了没用"，且没有任何报错可查。
// 现在删掉本地副本，转发链路与包内出站共用同一个判定（realm 路由 + 可配置覆盖）。

// authFromAccount 把库里的账号记录转成 upstream 需要的 *auth.Auth。
// realm 字段在包内未导出，唯一可靠的构造途径是走 auth.Parse 解析嵌套 JSON。
func authFromAccount(a *Account) *auth.Auth {
	if a == nil {
		return nil
	}
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  a.AccessToken,
			"refreshToken": a.RefreshToken,
			"expiresAt":    a.ExpiresAt,
			"domain":       a.Domain,
			"realm":        a.Realm,
		},
		"account": map[string]any{
			"uid":          a.UID,
			"enterpriseId": a.EnterpriseID,
			"nickname":     a.Nickname,
		},
	}
	if a.DeviceToken != "" {
		doc["device_token"] = a.DeviceToken
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil
	}
	au, err := auth.Parse(raw)
	if err != nil {
		return nil
	}
	return au
}

// refreshIfNeeded 临近过期时刷新 token，并把新 token 写回数据库。
func (p *Proxy) refreshIfNeeded(au *auth.Auth) error {
	if !au.NeedsRefresh(refreshWithin) {
		return nil
	}
	if err := p.uc.RefreshToken(au); err != nil {
		return err
	}
	_ = p.store.UpdateTokens(au.UID, au.AccessTokenValue(), au.RefreshTokenValue(), au.ExpiresAt)
	return nil
}

// chatBodyMaxDefault 推理请求体的默认上限（32MB，v0.31.73）。
//
// **为什么从 8MB 提到 32MB**（线上真实故障，2026-10-20/21）：客户端
// `deepseek-harness/0.2.1-rc.2` 发来的请求体 content_length 在
// **8.4MB ~ 23.4MB** 之间（长上下文 + 附件），全部卡在 8MB 闸门上 ——
// 一次都没触达上游，客户端只看到 413。8MB 当初是"够用就好"的估计，
// 而 agent 类客户端的 body 大小与上下文长度成正比，估计必然被追上。
//
// 取 32MB 的两个依据：
//  1. 覆盖实测最大值（23.4MB）的 1.37 倍，留出增长余量；
//  2. 单请求内存峰值可预算：body 在实现里会同时存在三份（原始 buf →
//     解开字段的 map → 重组后的出站 JSON），32MB × 3 ≈ 100MB/请求。
//     生产 1.6GB / 2 核，按 3 并发算是 ~300MB —— 到顶但仍有余量。
//
// **再往上加必须同时改内存预算**，不是改一个常量的事。
// 需要临时放宽时用环境变量 WB_BODY_MAX_MB（见 applyBodyLimitEnv），不要改代码重发。
const chatBodyMaxDefault = 32 << 20

// chatBodyMax 生效的推理请求体上限（启动时可由 WB_BODY_MAX_MB 覆盖）。
//
// 用包级 var 而不是 const：环境变量要在启动时读一次，之后只读不写（并发安全）。
var chatBodyMax int64 = chatBodyMaxDefault

const (
	// bodyLimitEnvKey 覆盖推理请求体上限的环境变量（单位 MB）。
	bodyLimitEnvKey = "WB_BODY_MAX_MB"
	// bodyLimitMin / bodyLimitMax 允许的运行期范围。
	//
	// 下限 4MB 是"别再往小里配"（小于常见 agent 请求就没意义了）；
	// 上限 128MB 是内存红线：128MB × 3 份副本 = 384MB/请求，再高就不是"调参"而是"赌机器"。
	bodyLimitMin = 4 << 20
	bodyLimitMax = 128 << 20
)

// applyBodyLimitEnv 启动时读一次环境变量覆盖 chatBodyMax。
//
// 非法值**退回默认并 WARN**，不 panic：配置写错不该让服务起不来
// （与保留天数 retention 的处理同口径）。生效值会打进启动日志 ——
// "当前到底放行多大"必须能从日志回答，否则下次撞限又要猜。
func applyBodyLimitEnv() {
	raw := strings.TrimSpace(os.Getenv(bodyLimitEnvKey))
	if raw == "" {
		return
	}
	mb, err := strconv.Atoi(raw)
	if err != nil {
		log.Printf("WARN: %s=%q 不是整数，按默认 %d MB", bodyLimitEnvKey, raw, chatBodyMaxDefault>>20)
		return
	}
	v := int64(mb) << 20
	if v < bodyLimitMin || v > bodyLimitMax {
		log.Printf("WARN: %s=%d MB 超出 %d~%d MB 范围，按默认 %d MB",
			bodyLimitEnvKey, mb, bodyLimitMin>>20, bodyLimitMax>>20, chatBodyMaxDefault>>20)
		return
	}
	chatBodyMax = v
	log.Printf("INFO: 推理请求体上限按 %s 设为 %d MB", bodyLimitEnvKey, mb)
}

// readCappedBody 读入站请求体（硬上限 max）。
//
// 用 io.Copy 到 buffer 而不是 io.ReadAll：**出错时也要拿到已读字节数**。
// 「收到 0 字节就断」与「收到 7.9MB 才断」是两种完全不同的故障（前者是连接根本没
// 传到/立刻被掐，后者是传输尾部被截断），而 io.ReadAll 在报错时把这个信息丢掉了 ——
// 它只回一个 error，调用方无从判断请求体到底来了多少。
//
// 上限必须走 http.MaxBytesReader 而不是 io.LimitReader（v0.31.14 修的真实缺陷）：
// LimitReader 到上限后返回的是 **EOF 而不是错误**，超限的请求体会被**静默截断**，
// 再交给 json.Unmarshal 就变成 `invalid json: unexpected end of JSON input` ——
// 一条把「请求太大」说成「JSON 写坏了」的消息。MaxBytesReader 超限返回
// *http.MaxBytesError，可以据此回 413 并把上限写清楚。
func readCappedBody(w http.ResponseWriter, r *http.Request, max int64) ([]byte, error) {
	var buf bytes.Buffer
	_, err := io.Copy(&buf, http.MaxBytesReader(w, r.Body, max))
	return buf.Bytes(), err
}

// bodyReadEvidence 组装读体失败的取证片段（纯函数，字段口径可被单测钉住）。
//
// 为什么这些字段缺一不可：这条 400 的成因**不在网关内部**，而在发送侧
// （客户端库 / 反向代理 / 隧道 / 本机 http_proxy 都可能截断），
// 网关既无法代客户端重发、也无法替它猜是哪一段网络。所以网关的责任只有两件：
// 如实回报 + 把现场写下来。
//
//	· peer / xff —— 谁发来的（隧道场景 RemoteAddr 恒是回环，xff 才是线索）；
//	· content_length / 已读 —— **声明了多少 vs 实际到了多少**，这一对是定性关键；
//	· elapsed —— 立刻断（0ms）还是传了一会儿才断；
//	· ua —— 哪个客户端库（不同客户端的截断 bug 形态不同）。
func bodyReadEvidence(r *http.Request, n int, elapsed time.Duration) string {
	xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For"))
	if xff == "" {
		xff = "-"
	}
	ua := strings.TrimSpace(r.UserAgent())
	if ua == "" {
		ua = "-"
	}
	return fmt.Sprintf("peer=%s xff=%s content_length=%d 已读=%d 耗时=%s ua=%q",
		r.RemoteAddr, xff, r.ContentLength, n, elapsed.Round(time.Millisecond), ua)
}

// reportBodyReadFailure 读入站请求体失败时的统一处置（v0.31.14）。
//
// 两类分开处置，因为**责任在谁、客户端该做什么**完全不同：
//
//	· 超限（*http.MaxBytesError）→ 413。这是"请求太大"，既不是客户端的 JSON 写错，
//	  更不是账号/上游的问题；给 400 会让客户端以为请求格式非法。
//	· 其余读错（unexpected EOF / connection reset / i/o timeout）→ **408**（不是 400，见下）。
//	  Go 的 `unexpected EOF` 只有一种成因：**声明的长度与实际收到的字节不一致**
//	  （Content-Length 写大了、chunked 尾块没到、连接中途断）。根因在发送侧，
//	  但网关无从判断是哪一段网络 —— 所以只如实回报 + 落日志。
//
// ⚠️ **为什么是 408 而不是 400**（v0.31.15，真机案例驱动）：
// 400 的语义是"你的请求不合法"，而且**OpenAI 兼容 SDK 一律不对 400 重试**
// （标准重试名单是 408 / 409 / 429 / 5xx）。但"上传被截断"是**瞬时传输故障**：
// 同一条隧道上 45 万 token 的大请求平时都过（实测），重发通常就好了。
// 回 400 等于明确告诉客户端"别重试" —— 本该自愈的故障就变成了用户看到的报错，
// 每次都得手动重发。408（Request Timeout）语义正对「服务端没收到完整请求」，
// 且在标准 SDK 的重试名单里，于是客户端会自动重发一次。
// 413（超限）**刻意不用 408**：同一个超限请求重发多少次都还是超限，重试无意义。
//
// 为什么必须打这条日志：v0.31.13 及以前，这条分支**一个字节的痕迹都不留** ——
// /api/admin/live 只能看到"有一次 400 失败"（兜底层按响应码补记），
// 台账里没有它（没有用量）、日志里没有它（没打），于是
// 「客户端自己截断」与「中间层吃掉请求体」这两种根因**在本机无法区分**。
//
// ⚠️ 刻意**不写** consume_failures：那张表是"针对某个账号的上游消费失败"
// （有 uid / attempt / stage 语义），把读体失败塞进去会污染账号维度的口径。
func (p *Proxy) reportBodyReadFailure(w http.ResponseWriter, r *http.Request, err error, n int, elapsed time.Duration) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		log.Printf("WARN: [proxy] 请求体超限（上限 %d 字节）：%s", tooLarge.Limit, bodyReadEvidence(r, n, elapsed))
		writeOpenAIErrorHint(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("read body: request body too large (limit %d bytes)", tooLarge.Limit),
			fmt.Sprintf("request body exceeds the gateway limit of %d bytes; split or shrink the payload "+
				"(if this is a legitimate long-context request, raise it server-side with %s=<MB>, allowed %d-%d MB; "+
				"no upstream account was used and retrying the same body will fail the same way)",
				tooLarge.Limit, bodyLimitEnvKey, bodyLimitMin>>20, bodyLimitMax>>20))
		return
	}
	log.Printf("WARN: [proxy] 请求体读取失败：%v —— %s（成因在发送侧：声明的长度与实际到达的字节不一致，或被中间层截断；该请求未触达任何上游账号）",
		err, bodyReadEvidence(r, n, elapsed))
	// 408 而不是 400：这是**可重试**的传输故障，标准 SDK 会对 408 自动重发（见函数注释）。
	writeOpenAIErrorHint(w, http.StatusRequestTimeout, "read body: "+err.Error(),
		"transient transport failure: the connection ended before Content-Length bytes arrived (client aborted, or an intermediary/proxy/tunnel truncated the upload); no upstream account was used and re-sending the same request usually succeeds")
}

// ServeChat 处理 OpenAI 兼容的 /v1/chat/completions。
// 出站一律强制 stream=true，再按客户端要求原样转发 SSE 或聚合为单次响应。
func (p *Proxy) ServeChat(w http.ResponseWriter, r *http.Request) {
	// 注意：请求计数（reqTotal/reqOK/reqFail）**不在这里**加了（v0.30.0）。
	// 它们统一由 liveCapture（/v1 链路的最外层）在请求结束时记一次 —— 否则
	// "被前置环节拒绝的请求"只让 total +1、ok/fail 都不进，出现
	// `total != ok + fail` 的自相矛盾（这正是实时面板此前计数对不上的原因之一）。
	start := time.Now()
	p.live.begin()
	defer p.live.done()
	raw, err := readCappedBody(w, r, chatBodyMax)
	if err != nil {
		p.reportBodyReadFailure(w, r, err, len(raw), time.Since(start))
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if payload == nil {
		payload = map[string]any{}
	}
	model, _ := payload["model"].(string)
	// 未指定模型（需求 7）：400，零出站、零扣费。
	//
	// 此前空 model 会一路走到选号（按空名字选），最后给出 503/404 —— 客户端看到的是
	// "服务不可用"，而真正该做的是"把 model 填上"。空模型是**请求的问题**，
	// 用 5xx 表达会让 SDK 去重试一个永远不会成功的请求。
	if strings.TrimSpace(model) == "" {
		writeOpenAIErrorHint(w, http.StatusBadRequest, "model is required",
			"request body has no model; GET /v1/models lists the models available to this key "+
				"(or use wb-group-work to let the server pick one from the pool); "+
				"no upstream account was used")
		return
	}
	// 模型组（v0.31.76）：`wb-group-xxx` 先展开成组内一个**真实模型名**，之后一切照旧
	// （归一化 / 选号 / 计费倍率 / 台账 / 冷却全按真名走）。
	//
	// 展开必须放在归一化之前：归一化会把"不在已知清单里的名字"当作拼写差异处理，
	// 而 `wb-group-work` 本来就不该出现在上游模型清单里。
	//
	// v0.31.82 起「work官组」只有两类，且与 Key 类型必须匹配：
	//   池计费 Key → wb-group-work（号池官组）
	//   代挂 Key   → wb-group-host（自挂官组）
	// 判定走 groupKindMismatch，与「生成接入包」那一处**共用同一个函数**（v0.31.83）：
	// 两处各写一套迟早会出现"生成时说不行、发请求却说行"。
	if strings.HasPrefix(strings.TrimSpace(model), groupModelPrefix) {
		gid := strings.TrimPrefix(strings.TrimSpace(model), groupModelPrefix)
		if k := apiKeyFrom(r); k != nil {
			if code, _, hint := groupKindMismatch(k.Kind, gid); code != "" {
				writeOpenAIErrorHint(w, http.StatusBadRequest, strings.ReplaceAll(code, "_", " "), hint)
				return
			}
		}
	}
	if real, groupModels, ok := p.applyGroupExpansion(payload, model, apiKeyFrom(r)); ok {
		model = real
		// 同组备选（v0.31.96）：挂到请求上，relay 在首个成员失败时逐个重试。
		// 组=「这一组里随便挑一个能跑的」，只挑一个就退化成了"别名"（见 expandGroupModels 注释）。
		r = withGroupModels(r, groupModels)
	}
	// 模型名归一化（v0.31.2）：去空白 / 剥 realm 前缀 / 按已知清单折回规范写法。
	//
	// **必须在 Marshal 之前**改 payload：出站请求体是从 payload 重新编码的，
	// 只改本地变量上游照样收到旧写法（照样 11102）。
	// 归一化后 model 变量同时用于选号 / 冷却键 / 负缓存键 / 台账，
	// 因此"同一模型的不同写法"不会再各占一个冷却槽（见 modelname.go 注释）。
	//
	// 失败（拿不到已知清单）时只做去空白与剥前缀，不影响别处；
	// 归一化说明只在真的改动时打一条 INFO —— 静默改写客户端的请求是坏事，
	// 但也不该为每个正常请求写日志。
	known := knownModelIDs(p.pool.ObservedModels())
	if norm, how := normalizeModelName(model, known); norm != model {
		log.Printf("INFO: [proxy] 模型名归一化 %q → %q（%s）", model, norm, how)
		model = norm
		payload["model"] = norm
	}
	// 模型名形态校验（v0.31.79）：线上真实出现过把**整个模型清单**用空格拼成一个
	// model 发过来的请求（54 个名字、1KB 长）。它会被当成"某个不存在的模型"打进上游，
	// 被 11102 拒、触发 rotateguard、浪费账号配额，最后回给客户端 404/503 ——
	// 每个环节都"看起来正常"，只有那条 1KB 的 WARN 能看出真相。
	//
	// ⚠️ **必须放在归一化之后**：归一化会去掉首尾空白，而 `" [global:]GLM-5.3 "`
	// 这种输入（复制粘贴常见）首尾各一个空格 —— 在归一化前判会把它当"拼串"回 400，
	// 等于把正常用户挡在门外（归一化用例当场就红了）。漏判一条垃圾请求 < 误伤一个真实用户。
	if modelNameLooksLikeList(model) {
		writeOpenAIErrorHint(w, http.StatusBadRequest, "model is not a single model id",
			"the model field looks like a list of names (too many words or too long); "+
				"GET /v1/models returns {\"data\":[{\"id\":\"...\"}]} — take ONE id and send it as a plain string, "+
				"do not join the list; no upstream account was used")
		log.Printf("WARN: [proxy] 拒绝拼串模型名（%d 字符，%d 个词）：%s",
			len(model), len(strings.Fields(model)), truncateStr(model, 160))
		return
	}
	// 生效分组（v0.31.88）：非空时组外模型直接 404。
	//
	// 这是"切换分组 = 切换计费口径"的硬约束：不拦的话，用户切到 A 组却继续用
	// B 组的模型，账单里就会出现"按 A 组口径收 B 组的量"，事后根本无法解释。
	// 放在归一化与组展开之后 —— 校验的必须是**最终真名**，不是客户端的写法。
	if k := apiKeyFrom(r); k != nil {
		if ms := p.activeGroupModels(k); ms != nil && !modelInGroup(ms, model) {
			writeOpenAIErrorHint(w, http.StatusNotFound, "model not available in the key's active group",
				fmt.Sprintf("this key is pinned to group %q (%d models); GET /v1/models lists exactly "+
					"what it can use, or switch the group in the console; no upstream account was used",
					k.ActiveGroup, len(ms)))
			return
		}
	}
	// 生图模型不能走对话端点（v0.31.98）：**零出站、零扣费**，并说清这是能力边界。
	//
	// 2026-10-21 现场：通道里挂着 gpt-image-2.5 系列（生图模型），有人拿它发对话，
	// 上游回 400，网关回 relay_rejected —— 文案里那句"可能是图像模型"埋在一长串英文里，
	// 用户读到的是"网关/配置有问题"。这里在**我们自己这一层**就拦下来说清楚：
	// 该用哪个端点、这次有没有花钱。
	//
	// 位置：归一化与生效分组校验之后（此时 model 已是最终真名；组名也不会走到这里 ——
	// 组展开已把生图成员剔除，见 chatCapableModels）。
	if IsImageModel(model) {
		w.Header().Set("X-Wb-Fail-Kind", imageModelCode)
		log.Printf("INFO: [proxy] 拒绝生图模型走对话端点 model=%s（零出站、零扣费）", model)
		writeOpenAIErrorHint(w, http.StatusBadRequest, imageModelMsg, imageModelHint)
		return
	}
	// 归一化之后仍不在已知清单里 → 记一条 WARN（同名一次）。
	// 这正是"客户端模型名配置错"的直接症状：每次调用都会拿到上游 400/11102，
	// 而我们回 404 —— 没有这条日志时，运维要在台账、上游原文与客户端配置之间来回对账。
	if !knownModelSet(known, model) {
		noteUnknownModel(model, known)
	}
	// 「该模型名上游不存在」的快速失败（v0.31.16）：闸门激活期内的请求**零出站**
	// 直接拒绝，不再去撞任何账号。见 rotateguard.ModelBlockGate 的"为什么"。
	//
	// 位置刻意放在归一化与未知模型告警**之后**：一是必须用**归一化后**的名字查
	// 闸门（否则 `x` 与 ` X ` 各占一桶，等于绕过判定）；二是让"客户端模型名配错"
	// 的线索（noteUnknownModel 的 WARN）照常留下。
	//
	// 回 404 而不是 5xx：这是**确定性**结论（上游明说没有这个模型），重发同样的
	// 请求只会得到同样答案 —— 与 413 同理，"重发没用"的错误不许用可重试状态码。
	if p.modelGate.Active(model) {
		log.Printf("WARN: [proxy] model %q 已被判定为上游不存在（fail-fast 期内）→ 直接拒绝、零出站", model)
		writeOpenAIErrorHint(w, http.StatusNotFound,
			"model not available: upstream reports this model does not exist",
			"the gateway already confirmed this model name is unavailable with multiple accounts in a row; check the model name (a typo or a stale model id is the most common cause) — re-sending will not help")
		return
	}
	// 把 model 回填给实时面板的兜底层（liveCapture）：它在 ServeChat 返回**之后**
	// 才知道结果，那时请求体早已读完，无法再解析一次。
	setLiveModel(r, model)
	// 租户上下文由 apiKeyGuard 注入；取不到则说明该路由未挂鉴权中间件。
	u := userFrom(r)
	key := apiKeyFrom(r)
	// 代挂自用 Key（kind=host）：只路由到号主自己的代挂账号，免预扣、免扣费，
	// 也不占用平台余额/配额。平台计费 Key 与管理员密钥走默认（charge=true）。
	scope := pickScope{}
	charge := true
	if key != nil && key.Kind == KeyKindHost {
		charge = false
		if u != nil {
			scope = pickScope{
				HostUser:    u.ID,
				HostGroup:   key.HostGroup,
				AccountUIDs: splitKVSet(key.AccountUIDs),
			}
		}
	}
	wantStream, _ := payload["stream"].(bool)
	payload["stream"] = true
	outBody, err := json.Marshal(payload)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "re-encode body: "+err.Error())
		return
	}

	// 降级期内的请求**直达** Degraded 中性提示词（v0.31.4，从旧网关移植）：
	// 降级是"这个客户端的 system 模板正在被上游误杀"的状态，同一窗口内每个请求都先撞
	// 一次 400 再降级纯属浪费（且每次失败都会给上游多打一次必然失败的请求）。
	// 起点作用在 outBody 上：PrepareOutbound 每轮从它重算，故改写一次即对全部轮次生效。
	degradedApplied := false
	if p.degrade.Active() {
		outBody = prompt.Rewrite(outBody, prompt.Degraded)
		degradedApplied = true
		log.Printf("INFO: [proxy] 提示词降级生效中（直到 %s）：本次请求直接使用中性 system",
			p.degrade.Until().Format(time.RFC3339))
	}

	// 会话粘性键：优先客户端会话 ID，退化为首条 user 消息派生。
	stickyKey := session.ExtractKey(raw)
	if stickyKey == "" {
		stickyKey = session.StickyFallbackKey(raw)
	}
	stickyUID := ""
	if stickyKey != "" {
		if uid, ok := p.sess.ResolveForModel(stickyKey, model); ok {
			stickyUID = uid
		}
	}
	// 选号轨迹（v0.17.5）：粘性键的短哈希随流水落库 —— 不落明文（键可能是客户端的
	// conversation_id），只留可用于分组关联的指纹。用途是把「同一场对话的多轮」在
	// usage_logs 里串起来：同一 sticky_key_hash 的多行若 upstream_uid 不同，
	// 就是「调用过程中换号（消耗不同账号的积分）」的硬证据。
	// 此前流水只有最终账号，这个问题只能靠时间聚类去猜 —— 这正是本次排查的盲区。
	stickyHash := ""
	if stickyKey != "" {
		sum := sha256.Sum256([]byte(stickyKey))
		stickyHash = hex.EncodeToString(sum[:6]) // 12 hex
	}

	// 预扣：按 max_tokens 上界冻结额度，钱不够当场 402 拒绝。
	// 不做预扣的话，「余额 10 点」也能发起一次实际消耗 500 点的长请求（max_tokens 拉满），
	// 结算后余额直接变 -490 —— 这就是刷爆。预扣把风险挡在请求发出之前。
	//
	// 返回 0 表示池内尚无该模型实测单价（新部署），此时退化为事后结算，不做预扣。
	// 失败收尾用的分类跟踪（v0.24.0，P2-1）：把上游错误归因映射成对客户端有意义的状态码。
	// 此前无论什么失败都回 503，而 503 是**可重试**语义 —— OpenAI 兼容客户端会照着
	// 自动重试，一个注定失败的坏请求（模型不存在 / 上下文超长 / 内容拦截）于是被反复
	// 放大（每次再试 3 个账号）。语义上也错：4xx 被伪装成 5xx，排障时看不出真正原因。
	var lastErr string
	var lastKind, clientKind upstream.ErrKind
	var lastRetryAfter string

	var resv *Reservation
	settled := false
	if u != nil && charge {
		if reserve := p.estimateReserveMicro(payload, model); reserve > 0 {
			var keyID int64
			if key != nil {
				keyID = key.ID
			}
			rv, rerr := p.store.ReserveCharge(u.ID, keyID, reserve, model)
			switch {
			case errors.Is(rerr, ErrInsufficientBalance):
				writeOpenAIError(w, http.StatusPaymentRequired, fmt.Sprintf(
					"insufficient balance: this request reserves up to %s points, current balance %s",
					formatPoints(reserve), formatPoints(u.BalanceMicro)))
				p.noteLive(r, liveEvent{
					At: time.Now().Unix(), User: liveUserName(u), Model: model, OK: false,
					Err: "余额不足（预扣被拒）", Status: http.StatusPaymentRequired,
					ElapsedMS: time.Since(start).Milliseconds(),
				})
				// 消耗失败台账（failures.go）：预扣阶段被拒是**用户侧**失败，
				// 与上游无关 —— 记下来是为了让"用户看到余额不足的次数"可查，
				// 而不是让学生在 error_msg 里翻。
				p.store.recordConsumeFailure(ConsumeFailure{
					Stage: FailStageReserve, Status: http.StatusPaymentRequired,
					Model: model, UserID: u.ID, KeyID: keyID,
					Source: "reserve", Detail: "预扣被拒：余额不足（需 " + formatPoints(reserve) + " 点）",
				})
				return
			case rerr != nil:
				// 预扣失败（存储抖动等）不阻断：退化为事后结算，不误杀用户请求。
				log.Printf("reserve failed, fallback to post-paid: %v", rerr)
				p.store.recordConsumeFailure(ConsumeFailure{
					Stage: FailStageReserve, Model: model, UserID: u.ID, KeyID: keyID,
					Source: "reserve", Detail: "预扣失败，退化为事后结算：" + rerr.Error(),
				})
			default:
				resv = rv
			}
		}
	}
	if resv != nil {
		defer func() {
			if !settled {
				// 走到这里说明请求失败/中断/结算异常，把预扣原样退回。
				if _, err := p.store.ReleaseReservation(resv.ID); err != nil {
					log.Printf("release reservation %d failed: %v", resv.ID, err)
				}
			}
		}()
	}

	// ConversationID 与网关同源解析（用于 prompt_cache_key 的会话段）：
	// 此前 webapp 只填了请求级随机 ID，会话段恒空 → prompt_cache_key 退化为
	// 账号级键，命中率低于网关链路（见 PrepareOutbound 注释）。
	meta := upstream.ChatMeta{
		ConversationID:        session.ResolveConversationID(raw),
		ConversationRequestID: newRandomID(16),
	}

	// heldUID/heldModel 本次请求**当前持有**的租约（成功路径会一直持有到函数返回）。
	// 两者必须一起维护：释放时按 (uid, model) 递减在途计数（见 releaseHeld）。
	var heldUID, heldModel string
	defer func() { p.releaseHeld(&heldUID, &heldModel, model) }()
	// 通道分发（v0.31.68，需求 P2）：**号池没有这个模型的可用号时**才落第三方 / 中转站。
	//
	// 位置刻意在轮转之前、预扣之后：预扣是为"这一笔要花钱"准备的，与走哪条供给无关；
	// 而放到轮转里（au==nil 时降级）会把"上游临时故障"变成"刷第三方账单"——
	// 那不是一个可以默认开启的行为。
	//
	// handled=false 表示"不该走通道"（号池有号 / 代挂 Key / 没配通道），
	// 此时 serveRelay 没有写过响应，继续走下面的号池路径。
	if handled, relaySettled := p.serveRelay(w, r, payload, model, u, key, resv, stickyHash, start, wantStream); handled {
		if relaySettled {
			settled = true
		}
		return
	}

	tried := map[string]bool{}
	// probeAttempt 本次轮转是否由**半开探测**拿到号（v0.31.6）：决定成功时是否
	// 提前解除该 (账号, 模型) 的 6004 冷却，以及失败时是否补一条可读日志。
	// probedThisRequest 本请求是否**已经**做过一次探测（一次请求最多一次，见下方注释）。
	var probeAttempt, probedThisRequest bool

	for attempt := 0; attempt < maxAttempts; attempt++ {
		// 换号退避（v0.18.x）：attempt>0 时先等一段指数退避 + ±25% 抖动再换下一号。
		// 对齐 cmd/server 的 rotateBackoff（WAF 403 修复 P0-2）——此前 webapp 换号
		// 零间隔，多号齐步重试会以固定周期聚团打上游（WAF 按请求密度判罚），且被
		// IP 级拦截时会迅速把一次客户端请求放大成 N 倍打同一出口 IP。
		if attempt > 0 {
			if !rotateguard.SleepCtx(r.Context(), rotateguard.BackoffAfter(attempt-1)) {
				return // 客户端断连：终止轮转（defer 释放租约与预扣）
			}
		}
		var au *auth.Auth
		if stickyUID != "" {
			au = p.pool.PickByUIDForModel(stickyUID, model)
			// 三重放行条件：账号在池且该模型可用 + 落在本次范围 + **满足消耗分组优先级**。
			// 最后一条是 v0.15.2 补的（见 stickyPriorityOK 注释）：粘性绑定不得绕过
			// 「用户申请 > 反代」，否则新会话可能被绑到反代组账号并直接被使用。
			if au == nil || !p.allowsSelection(au.UID, scope) || !p.stickyPriorityOK(au.UID, model, scope) {
				// 粘性号不可用（冷却/模型限额/范围外/分组优先级不满足）
				// → 解绑回落普通轮换。代挂自用范围下不动别人的绑定
				//（sticky key 由请求内容派生、可能与他租户碰撞），仅本次忽略。
				if au == nil || !scope.isHostSelf() {
					p.sess.Unbind(stickyKey)
				}
				stickyUID = ""
				au = nil
			}
		}
		probeAttempt = false
		if au == nil {
			au = p.pickExcluding(tried, model, scope)
		}
		if au == nil && !probedThisRequest {
			// 常规候选耗尽 → **模型级半开探测**（v0.31.6）。
			//
			// 场景：池里每个号对该模型都还在 6004 冷却里（≤24h），但上游可能已经提前
			// 解除 —— 严格等墙钟意味着"其实早就恢复了"的容量被白放到最晚一天。
			// 这里放行**一次**"带着答案去问上游"的请求，把提前恢复的容量拿回来。
			//
			// ## 两层强度闸门（缺一层就变成出站脉冲）
			//
			//  1. 池侧：每 (账号, 模型) 每 interval（默认 1h）一次，且要过了 delay（20m）；
			//  2. **本处**：一次客户端请求最多探测一次。
			//
			// 第 2 条不是冗余：池侧闸门只管"同一个号"，而轮转会把 `tried` 里已试过的号
			// 排除后**继续挑下一个可探号** —— 没有这一条，一个客户端请求就会把池内所有
			// 冷却号依次探一遍（33 个号 = 33 次上游调用、同一出口 IP、几秒内打完），
			// 正是本项目最贵的一类事故。一次请求做一次实验，足够了。
			au = p.pickProbeExcluding(tried, model, scope)
			if au != nil {
				probeAttempt = true
				probedThisRequest = true
			}
		}
		if au == nil {
			break
		}
		tried[au.UID] = true
		if !p.pool.Acquire(au.UID) {
			continue // 在途名额被抢，换下一个
		}
		heldUID, heldModel = au.UID, model
		p.noteModelInFlight(au.UID, model)

		if err := p.refreshIfNeeded(au); err != nil {
			lastErr = "refresh token: " + err.Error()
			p.releaseAndUnbind(&heldUID, &heldModel, model, stickyKey, au.UID, &stickyUID)
			// 按 ErrKind 分流（v0.24.8）：12153 走会话失效的累计纪律，
			// 其余保持降权口径。此前一律 NoteFailures，导致 12153 既不被摘除、
			// 又永远重复被选中（见 applyRefreshFailure 注释）。
			p.applyRefreshFailure(au.UID, err)
			continue
		}

		url := p.uc.ChatBaseFor(au) + chatPath
		// 出站改写（v0.24.0 P1-1）：复用 upstream 的同一套管线和 webapp 之前**完全绕过**它，
		// 导致指纹脱敏 / 角色归一 / efforts 降级 / prompt_cache_key 全部失效
		// （而 p.uc.SanitizeFingerprints 明明设了 true —— 静默缺陷）。
		// 放在循环内、按**当前账号**改写：realm/uid 决定指纹与降级桶，换号必须重算。
		attemptBody := p.uc.PrepareOutbound(outBody, au, meta.ConversationID)
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(attemptBody))
		if err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "build request: "+err.Error())
			return
		}
		p.uc.ChatHeaders(req, au, "", meta)

		// 出口抖动（缺省关闭）：让共用同一出口的不同账号错开出发时刻，
		// 避免「同一 IP 在同一毫秒打出多个账号的请求」这一强关联信号。
		if d := p.egressJitter(au.UID); d > 0 {
			select {
			case <-time.After(d):
			case <-r.Context().Done():
				p.releaseAndUnbind(&heldUID, &heldModel, model, stickyKey, au.UID, &stickyUID)
				return
			}
		}

		// 出站客户端按账号选择：配了专用/默认出站代理的账号从各自出口 IP 打上游（防封）。
		// 代理配置有误时这里会返回 error —— 有意直连（未配代理）不受影响。
		// 流式请求走「无总超时」变体（见 chatClientFor）：180s 硬超时会掐断长流。
		client, cerr := p.chatClientFor(au.UID, wantStream)
		if cerr != nil {
			lastErr = cerr.Error()
			p.releaseAndUnbind(&heldUID, &heldModel, model, stickyKey, au.UID, &stickyUID)
			p.pool.NoteFailures(au.UID)
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err.Error()
			p.releaseAndUnbind(&heldUID, &heldModel, model, stickyKey, au.UID, &stickyUID)
			if errors.Is(err, contextDone(r)) {
				return
			}
			p.pool.NoteFailures(au.UID)
			continue
		}
		if resp.StatusCode >= 400 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			lastErr = fmt.Sprintf("upstream %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
			p.releaseAndUnbind(&heldUID, &heldModel, model, stickyKey, au.UID, &stickyUID)
			retryAfter := resp.Header.Get("Retry-After")
			// 上游明示的恢复时刻：三级来源解析（body 文案 > body 字段 > 响应头），
			// 见 internal/upstream/resetat.go。解析放在调用点而不是策略函数内部，
			// 是为了让「解析」与「据此罚谁」分开 —— 策略函数只需要一个时刻。
			resetAt, resetSrc := upstream.ResolveResetAt(resp.Header, body, time.Now())
			// 传 raw（客户端请求体原文）而不是 payload（改写后的 map）：raw 已是 []byte，
			// 且 system 提示词在两者中一致（改写只动角色/档位）。
			kind := p.reportUpstreamError(au.UID, model, resp.StatusCode, body, resetAt, resetSrc, raw)
			// 下游可用的等待提示：上游常常只把恢复时刻写在 body 里而不发 Retry-After 头，
			// 此时把解析出来的剩余秒数合成一个转发给客户端 —— 否则兼容客户端会立刻重试，
			// 把一次限流放大成一串连击（对上游是更明显的异常特征）。
			if retryAfter == "" && !resetAt.IsZero() {
				if sec := int(time.Until(resetAt).Seconds() + 0.999); sec > 0 {
					lastRetryAfter = itoa(sec)
				}
			}
			// R6（v0.26.1）：上游 5xx 说明**这一档整体不可用** → 把该成本层整层划掉，
			// 下次选号自然落到下一档（默认关闭；见 picksink.go 的"为什么默认关闭"）。
			if kind == upstream.ErrServer {
				p.sinkCostTier(au.UID, model, tried)
			}
			// 记录分类，供最终状态码映射（见 upstreamFailureStatus）。
			lastKind = kind
			// 探测失败：**只加日志**，处置一律走下面统一的分类分支（探测不是"另一种
			// 错误"，它只是"一次注定被拒也值得试的请求"）。冷却由 reportUpstreamError
			// 按既有纪律续期；池侧 ProbedAt 已在放行时写好，故不会立刻再探。
			if probeAttempt {
				log.Printf("WARN: [proxy] model_probe_failed model=%s acct=%s kind=%s status=%d",
					model, au.UID, kind, resp.StatusCode)
			}
			if retryAfter != "" {
				lastRetryAfter = retryAfter
			}
			// 内容拦截 / 渠道拒绝的**降级重试**（v0.31.4，从旧网关移植；此前 webapp
			// 完全没有这条能力，是合并两个产品时唯一的能力差集）。
			//
			// 判定依据：这类 4xx 多由客户端 system 模板的**逐字指纹**触发（issue #36/PR39），
			// 换中性提示词即大概率通过 —— 而换账号毫无帮助（全池 UA/身份声明一致）。
			// 只重试**一次**（degradedApplied 闸）：第二次仍被拦说明是用户内容本身触发审核，
			// 继续重试只是把同一份被拒内容反复打上游。
			// 该分支不改账号状态：内容问题不是账号问题（reportUpstreamError 已按此分类）。
			if (kind == upstream.ErrContentBlocked || kind == upstream.ErrChannelDenied) && !degradedApplied {
				p.degrade.Trigger()
				outBody = prompt.Rewrite(outBody, prompt.Degraded)
				degradedApplied = true
				// 单账号池也要能拿到重试机会：本次已试过的号重新可用（降级重试只占一次名额）。
				delete(tried, au.UID)
				log.Printf("WARN: [proxy] 内容拦截（%s，多为 system 指纹误报）→ 已触发提示词降级并同请求重试一次",
					kind)
				continue
			}
			// 客户端可修的错（请求本身的问题）单独记一份：即使后续账号报出别的
			// 错误，最终也该回 4xx —— 根因是请求，不是账号。
			if isClientFaultKind(kind) {
				clientKind = kind
			}
			// WAF IP 级 fail-fast（v0.18.x）：该次 WAF 403 喂入 IP 级状态机，若激活
			// （短窗内多号接连命中 = IP 被拦而非账号）则立即终止轮转——继续换号只会把
			// 请求放大 N 倍打同一出口 IP，加重风控。账号级软冷却已由 reportUpstreamError
			// 照常记账（单号偶发 403 仍冷却），IP 级状态只改变「是否继续轮转」。与
			// cmd/server 的 wafIP.noteWaf 同语义（逻辑见 internal/rotateguard）。
			if kind == upstream.ErrWafBlock && p.wafGate.NoteWaf(au.UID) {
				break
			}
			// 模型名不存在（11102）的跨账号 fail-fast（v0.31.16）：与账号无关的错误
			// 不该把全池犁一遍。判定口径与阈值见 rotateguard.ModelBlockGate ——
			// **两个不同号**在 60s 内接连报 11102 即判定"上游没有这个模型名"，
			// 激活期内直接终止轮转（本次请求不再换号，后续请求也不再出站）。
			// 注意：reportUpstreamError 已照常写入 (账号,模型) 负缓存 —— 那条
			// 仍是必要的（避免下一个请求再撞同一个号），本条只管"是否继续换号"。
			if kind == upstream.ErrModelBlocked && p.modelGate.NoteBlocked(model, au.UID) {
				break
			}
			continue
		}

		p.noteChatSuccess(au.UID, model)
		if probeAttempt && p.pool.ProbeSucceeded(au.UID, model) {
			// 半开探测**成功** = 直接证据：上游对该 (账号, 模型) 的限额已经解除。
			// 这是唯一允许"用成功清 6004 冷却"的路径（普通成功刻意不清，见
			// pool.ProbeSucceeded 与 noteChatSuccess 的注释）。
			log.Printf("INFO: [proxy] model_probe_recovered model=%s acct=%s（已提前解除该模型的 6004 冷却）",
				model, au.UID)
		}
		p.noteHit(au.UID) // 记账复用时刻，供最小复用间隔节流
		// 响应头透出实际服务的账号（对齐 cmd/server，v0.17.5 补齐）：
		//   - 排障时直接看到「这次是谁在答」；
		//   - 客户端侧验证「同一会话是否换号」的最便捷抓手 —— 连续两次响应比较该头即可，
		//     不必去翻服务端流水。
		// 必须在写响应体之前设置：streamSSE / aggregateSSE 内部会 WriteHeader（之后再改无效）。
		// 放在 NoteSuccess 之后：语义是「这个号成功服务了本次请求」，中途换号失败的号不冒领。
		w.Header().Set("X-Wb-Account", au.UID)
		// 代挂自用不写共享粘性表：粘性 key 由请求内容派生、跨租户可能碰撞，
		// 写入会污染其他租户的绑定。
		if stickyKey != "" && !scope.isHostSelf() {
			p.sess.Bind(stickyKey, au.UID)
		}
		var ui usageInfo
		var streamErr error
		var se streamModelErr
		// 流内 error 帧透传时附加 gateway_hint（v0.31.5，与旧网关 StreamHint 同口径）：
		// hintFn 惰性求值——正常流零开销，只有真撞到 error 帧才判一次。
		hintFn := func(payload string) string {
			if payload == "" || payload == "[DONE]" {
				return ""
			}
			return upstream.GatewayHint(upstream.FrameKind(payload), payload,
				upstream.HintContext{Model: model})
		}
		if wantStream {
			ui, se, streamErr = streamSSEHint(w, resp, hintFn)
		} else {
			// 非流式聚合路径不加 hint：聚合后回给客户端的是**合成**的响应体（不是上游帧），
			// 而旧网关那条路径同样不带 hint（它用 upstream.Aggregate）—— 保持一致，
			// 不为"看起来更完整"而偏离原口径。
			ui, se, streamErr = aggregateSSE(w, resp)
		}
		resp.Body.Close()
		// 流内模型级错误帧（HTTP 200 + error 帧）：补做 (账号, 模型) 冷却（v0.31.1）。
		//
		// 放在 streamErr 分支**之前**：流中途断了也要冷却（错误帧已到达，结论已定），
		// 何况断流本身就是"这个号这条路走不通"的旁证。
		if se.Code != 0 {
			p.reportInStreamModelError(au.UID, model, se.Raw, raw)
		}
		if streamErr != nil {
			// 上游流中断（读 body 出错：上游掐断 / 网络抖动 / 客户端断连传播）。
			// 流式路径此刻 HTTP 头已发出，改不了状态码 —— 但 [DONE] 已由 streamSSE
			// 兜底补写，客户端至少能正常收尾；非流式路径 aggregateSSE 已自行回 502。
			// 关键变化（v0.17.5）：从这里开始**不再静默** —— 有日志、有实时事件、有流水。
			log.Printf("WARN: [proxy] upstream stream interrupted acct=%s model=%s: %v",
				logfmt.Label(au.UID, au.Nickname), model, streamErr)
			p.noteLive(r, liveEvent{
				At: time.Now().Unix(), User: liveUserName(u), Model: model, OK: false,
				Err:       "上游流中断（" + streamErr.Error() + "）",
				ElapsedMS: time.Since(start).Milliseconds(),
			})
			// 记一条「中断」流水：0 费用（usage 为空、status≠ok → consumedCredit=0），
			// 预扣由 settleUsage 内部全额退还。这样「执行到一半断了」在账单里查得到。
			settled, _ = p.settleUsage(u, key, model, au.UID,
				usageInfo{LatencyMS: time.Since(start).Milliseconds()},
				resv, "interrupted",
				"upstream stream interrupted: "+streamErr.Error(), stickyHash, attempt+1)
			return
		}
		// 计费在响应发完之后结算：上游 credit × 倍率 → 与预扣对账（多退少补）→ 写流水。
		// 注意用 `=` 而非 `:=`：settled 是外层变量，`:=" 会在循环块里遮蔽它，
		// 导致下面 defer 的「未结算则释放预扣」判据永远为真（成功也退一次）。
		var chargedMicro int64
		// 端到端耗时（含排队 / 换号重试 / 回程）：在结算前填一次。
		// 走 usageInfo 而不是给 settleUsage 再加一个参数 —— 该函数已有 10 个入参，
		// 而这是一个纯粹的"本次请求观测值"，与 prompt/completion 同性质。
		ui.LatencyMS = time.Since(start).Milliseconds()
		settled, chargedMicro = p.settleUsage(u, key, model, au.UID, ui, resv, "ok", "", stickyHash, attempt+1)
		// 号池侧成本账簿 + 余额实时收敛（v0.17.6 修复）。
		//
		// **为什么必须在这里调**：cmd/webapp 不经过 internal/server，而 `pool.NoteModelCost`
		// 只在 internal/server/handler.go 里被调用过 —— 于是 webapp 链路上它**从未执行**。
		// 这不是「少了一个优化」，而是两个实测可见的后果：
		//   1. 池内 credits 只在签到（9 点 / 21 点）被覆盖，两次签到之间（最长 12 小时）
		//      完全不动 → 号池仪表盘的「积分合计 / 平均积分 / 积分 Top 8」整块冻结，
		//      看起来就是「实时数据不实时」（实测：18 个号 12 小时纹丝不动）；
		//   2. 池内 (账号, 模型) 成本观测恒空 → 选号的成本分层永远停在「无观测」，
		//      「优先消耗限免号」这套机制在 webapp 里实际失效（只有 handler.go 那条链路有）。
		//
		// 只在**上游真实回报 credit** 时记（与 handler.go 严格同口径）：credits 与成本
		// 账本都必须是实测值，拿估算值回灌会自我强化（估算 → 记成观测 → 下次估得更准），
		// 把账本变成回音壁。缺 credit 时 settleUsage 仍按既有口径做计费估算，
		// 但不污染池内观测。
		if ui.HasCredit && ui.Total() > 0 {
			p.pool.NoteModelCost(au.UID, model, ui.Credit, ui.Total())
			// 余额被实时扣减后立即做一次分组判定（v0.20.0）：长期消耗会跨过
			// 预备线（<100），需要及时把账号搬进预备组。回调内部有 1s/账号
			// 节流（见 poolgroup.go），故热路径代价只是一次 map 查询。
			if p.onCreditChanged != nil {
				p.onCreditChanged(au.UID)
			}
		} else if ui.Total() > 0 {
			// 有用量但无 credit（上游末帧缺字段）：只记日志，不写账本（缺失≠0）。
			log.Printf("WARN: [proxy] usage without credit acct=%s model=%s (no cost observation)",
				logfmt.Label(au.UID, au.Nickname), model)
		}
		p.noteLive(r, liveEvent{
			At: time.Now().Unix(), User: liveUserName(u), Model: model, OK: true,
			Tokens: ui.Total(), Charged: MicroToPoint(chargedMicro),
			Status:    http.StatusOK,
			ElapsedMS: time.Since(start).Milliseconds(),
		})
		return
	}
	if lastErr == "" {
		lastErr = "no available account (all cooling or disabled)"
	}
	// 状态码按**失败归因**映射，而不是一律 503（v0.24.0，P2-1）：
	// 请求本身的问题回 4xx（客户端不该重试），我方容量/上游故障才回 5xx。
	finalKind := lastKind
	if clientKind != upstream.ErrNone {
		finalKind = clientKind // 请求级错误优先：根因在请求，与账号/容量无关
	}
	finalStatus := upstreamFailureStatus(finalKind)
	// 状态码在**记录之前**算好（v0.30.0）：此前这一笔记在映射之前，
	// 于是事件流里所有"上游失败"都没有状态码，无法按 402/429/503 分类。
	p.noteLive(r, liveEvent{
		At: time.Now().Unix(), User: liveUserName(u), Model: model, OK: false,
		Err: lastErr, Status: finalStatus, ElapsedMS: time.Since(start).Milliseconds(),
	})
	if lastRetryAfter != "" {
		w.Header().Set("Retry-After", lastRetryAfter)
	}
	// gateway_hint（v0.31.5，从旧网关移植；此前 webapp 的错误响应没有这个字段）：
	// 失败原因的高层分类（finalKind）已经算好，这里只把它翻成一句"网关视角的补充说明"——
	// message 仍是原文，hint 是**并列**字段，绝不替换 message。
	// 没有 kind 可判（如池里压根没号）时退用本地调度事实的固定 hint（同为旧网关口径）。
	hint := upstream.GatewayHint(finalKind, lastErr, upstream.HintContext{Model: model})
	if hint == "" && finalKind == upstream.ErrNone {
		hint = upstream.NoHealthyAccountHint()
	}
	// 503 的取证（v0.31.79）：此前这条出口**只写 message**，于是"到底是哪一类失败"
	// 只能在 /api/admin/live 的内存事件里看，日志里查不到 —— 用户报 503 时我们只能靠猜。
	// 现在两件事一起做：
	//   1. 日志里打最终 kind + 状态码 + 模型（lastErr 已含上游原文，截断防止刷屏）；
	//   2. 响应头 X-Wb-Fail-Kind 带出分类，用户报障时能直接告诉我们属于哪一类。
	// kind 名来自 upstream.ErrKind.String()，与 Classify 同源，不另起一套词。
	w.Header().Set("X-Wb-Fail-Kind", finalKind.String())
	log.Printf("WARN: [proxy] 请求最终失败 kind=%s status=%d model=%q err=%s",
		finalKind.String(), finalStatus, model, truncateStr(lastErr, 300))
	writeOpenAIErrorHint(w, finalStatus, "upstream unavailable: "+lastErr, hint)
}

// isClientFaultKind 该错误是否由**请求本身**引起（换任何账号结果相同）。
//
// 判据与 internal/server 的 applyErrorPolicy「请求级错误零动作」同源：这些错误
// 既不罚账号，也不该让客户端重试 —— 必须改请求才有用。
//
// ErrChannelDenied（11128 渠道 / 客户端指纹拒绝）**在**此列（v0.24.8）：它的语义是
// 「这个客户端身份被上游渠道策略拒绝」，而本仓的 UA 与身份声明是**全池一致**的
// （只有 X-Machine-ID / X-Session-ID 按账号隔离），换号基本救不回来 ——
// 回 400 让调用方改请求，比回 503 诱导客户端把同一份指纹再打一遍更诚实。
//
// 注意两件事不要混淆：① 它仍然**不罚账号**；② 轮转照常发生 —— 轮转是重试循环的
// 默认行为，与本判定无关，本判定只决定「最终回给客户端什么状态码」。
func isClientFaultKind(k upstream.ErrKind) bool {
	switch k {
	case upstream.ErrBadParams, upstream.ErrContentBlocked, upstream.ErrPromptTooLong,
		upstream.ErrImageInvalid, upstream.ErrModelBlocked, upstream.ErrClient,
		upstream.ErrChannelDenied:
		return true
	}
	return false
}

// upstreamFailureStatus 把「最终失败原因」映射成对客户端有意义、且**重试语义正确**
// 的 HTTP 状态码。
//
// 为什么重要（v0.24.0，P2-1）：此前无论什么失败都回 503，而 503 在 OpenAI 兼容
// 客户端眼里是「服务暂时不可用，可重试」—— 于是一个注定失败的坏请求（模型名写错、
// 上下文超长、内容被拦）会被客户端反复自动重试，每次再打最多 3 个账号，
// 把一个客户端错误放大成持续的上游流量（也正是本仓库一直避免的风控特征）。
//
// 映射原则：
//
//	· 400 —— 请求本身的问题（参数/内容/长度/图片）。改请求才有用，重试无意义。
//
// modelNameLooksLikeList 判断 model 字段是不是"把整个模型清单拼成了一个字符串"。
//
// 判据用**词数**而不是空格数：`strings.Fields` 会忽略连续空白与首尾空白，
// 所以 `" glm-5.3 "`（复制粘贴常见）算 1 个词、不会被误伤。
// 真实模型 id 不含空格（最长的 `claude-sonnet-4-5-20250929` 也就 28 字符），
// 出现 3 个以上词基本只能是清单 join。
//
// 调用点必须在**归一化之后**（归一化会去首尾空白），见 ServeChat 里的注释。
func modelNameLooksLikeList(m string) bool {
	return len(m) > 128 || len(strings.Fields(m)) >= 3
}

// upstreamFailureStatus 失败归因 → 回给客户端的状态码。
//
//	· 400 —— 请求本身的问题（参数/内容/渠道策略/prompt 过长），换号也没用。
//	· 404 —— 模型不存在或无权限（ErrModelBlocked）。
//	· 429 —— 上游限流（ErrSoftRate），并透传 Retry-After。
//	· 503 —— 无可服务账号 / 账号级故障 / 上游 5xx —— 这是我方容量问题，可重试。
func upstreamFailureStatus(k upstream.ErrKind) int {
	switch k {
	case upstream.ErrBadParams, upstream.ErrContentBlocked, upstream.ErrChannelDenied,
		upstream.ErrPromptTooLong, upstream.ErrImageInvalid, upstream.ErrClient:
		return http.StatusBadRequest
	case upstream.ErrModelBlocked:
		return http.StatusNotFound
	case upstream.ErrSoftRate:
		return http.StatusTooManyRequests
	default:
		// ErrHardCredit / ErrSessionDead / ErrAccountFault / ErrWafBlock / ErrServer /
		// ErrNotFound / ErrNone(未分类) —— 对我方或上游而言都是「暂时没有可用通道」。
		return http.StatusServiceUnavailable
	}
}

// releaseAndUnbind 释放租约，并在失败号正是粘性号时解绑。
// releaseHeld 释放本次请求持有的账号租约，并同步递减 (账号, 模型) 在途计数。
//
// **所有释放路径都必须走这里**（成功、失败换号、客户端断连、defer 兜底）：
// 在途计数与租约必须同生同死 —— 计数泄漏会让该 (账号,模型) 被永久偏爱跳过，
// 比"重复调度"本身更糟。heldModel 与 held 一起清空，防止把旧模型记到新号上。
func (p *Proxy) releaseHeld(held *string, heldModel *string, model string) {
	if *held == "" {
		return
	}
	p.clearModelInFlight(*held, model)
	p.pool.Release(*held)
	*held, *heldModel = "", ""
}

func (p *Proxy) releaseAndUnbind(held *string, heldModel *string, model, stickyKey, uid string, stickyUID *string) {
	if *held != "" {
		p.clearModelInFlight(*held, model)
		p.pool.Release(*held)
		*held, *heldModel = "", ""
	}
	if *stickyUID != "" && uid == *stickyUID {
		p.sess.Unbind(stickyKey)
		*stickyUID = ""
	}
}

// contextDone 返回与请求上下文绑定的哨兵错误，用于识别客户端断开（不再重试换号）。
func contextDone(r *http.Request) error {
	if err := r.Context().Err(); err != nil {
		return err
	}
	return nil
}

// reportUpstreamError 按上游错误分类上报冷却/熔断，语义对齐 cmd/server 的 applyErrorPolicy。
//
// 参数 resetAt / resetSrc 来自 upstream.ResolveResetAt（上游明示的恢复时刻与来源）；
// 零值表示"上游没给可用时间信息"，此时各分支回落到**有界退避**（本地估算）。
// 把解析结果作为入参而不是在函数内部再解析一次：同一个响应只解析一次，
// 且"解析"与"处置强度"两件事的边界清晰（改解析不影响罚谁）。
//
// v0.18.x 修复（误熔断）：旧实现的 default 分支无条件 NoteError（喂熔断器）——
// 于是**请求级错误**（内容拦截 400 / 11101 请求体畸形 / 11102 模型不存在 /
// 11115 上下文超长 / 上游 404）也记在账号头上，连 3 次即熔断一个健康账号
// 30min~6h。这些全是「请求的问题不是账号的问题」（同一 body 换任何号结果相同），
// cmd/server 侧对此一律零动作或按 (账号,模型) 负缓存避让（见其 applyErrorPolicy
// 注释：「内容问题非账号问题，不罚账号」）。本次按共享的 upstream.Classify 分流。
//
// 与 cmd/server 的**有意差异**（勿按 server「修正」）：
//   - status >= 500 保留 NoteFailures（连败降权，10min 级），不改成 server 的
//     NoteError（熔断 30min 起、封顶 6h）。上游 5xx 多为全局故障，按熔断处理会
//     把整池账号批量摘除——hub issue #271 实测「4 小时 196 次失败熔断 13 个号、
//     池子几乎空掉」即此类尺度过激。本链路取更保守的降权口径。
func (p *Proxy) reportUpstreamError(uid, model string, status int, body []byte, resetAt time.Time, resetSrc upstream.ResetSource, reqBody []byte) upstream.ErrKind {
	display := upstream.UpstreamDisplayMsg(string(body))
	code := parseUpstreamCode(body)
	// kind 供本函数内部分流、台账记录、以及调用方（WAF IP 级 fail-fast 判定）共用，
	// 只分类一次。
	kind := upstream.Classify(status, string(body))

	// 消耗失败台账（failures.go）：**每一次**上游拒绝都落一条，且**放在所有处置
	// 分支之前** —— 下面的"账号受限"分支会提前 return，把它放在分类之后、
	// 处置之前的唯一目的就是让"无论走哪条分支，记录都在"。
	//
	// 为什么把请求**形状**（消息条数 / 是否流式 / 工具数）也算进上下文：
	// 排障时最先要回答的是"失败的请求长什么样"，这些不涉及内容的统计量足以
	// 把「长上下文超限」与「普通请求被拦」分开，且不落任何提示词原文。
	p.store.recordConsumeFailure(ConsumeFailure{
		Stage:       FailStageUpstream,
		Code:        code,
		Kind:        kind.String(),
		Status:      status,
		Model:       model,
		UID:         uid,
		Source:      "proxy",
		ResetSource: string(resetSrc),
		Detail: fmt.Sprintf("上游 %d：%s｜请求形状 %s", status,
			firstLine(display), requestShape(reqBody)),
	})

	// —— 账号级限制信号（v0.24.6）：先于按 code 的分流 ——
	//
	// 放在最前面是因为它的处置**强度最高且不可逆**（即时删除），而下面的
	// 6004 / 11140 / 12153 / 14017 分支全是可逆冷却。上游既然明说了账号被限制，
	// 就不该再按 11140 去累计（等 3 次只是白等两轮，期间用户请求持续失败）。
	//
	// 走回调而不是在这里直接删：删除要动 DB + 池 + 健康快照三处（deleteAccountsHard），
	// 那是 app 层的职责；Proxy 只负责识别与转达，避免把两层的边界打穿。
	if upstream.IsAccountRestricted(display) {
		if p.onAccountRestricted != nil {
			p.onAccountRestricted(uid, "proxy", display)
		} else {
			// 未注入回调（单元测试 / 极简装配）：退化为停用，绝不静默放过。
			p.pool.RetireAccount(uid, upstream.ErrAccountFault, upstream.AccountRestrictedHint())
		}
		return kind
	}
	switch {
	case code == 6004:
		// 模型级限额：只冷却该模型，账号对其他模型仍可用。
		//
		// 冷却时长来源（v0.26.3）：
		//   resetAt 非零 → 对齐上游明示的恢复墙钟（封顶 soft_rate_max，见 pool 侧）；
		//   resetAt 零值 → 模型级有界退避（soft_rate 基数起、按连续次数翻倍）。
		// 两者都**只挡该模型**；reason 里带上来源，排障时能一眼看出这个时长
		// 是上游给的还是本地估的（这两种情况的处置方向完全不同）。
		p.pool.CooldownSoftForModel(uid, softCooldown, resetAt, model, "6004 model rate limit"+resetSrcSuffix(resetSrc))
	case code == 11128 && (status == http.StatusBadRequest || status == http.StatusForbidden):
		// 渠道 / 客户端指纹级拒绝（v0.24.8）：**这次请求**被上游按渠道策略挡下，
		// 不是账号自身的问题 → 零动作（不冷却 / 不熔断 / 不计数），但**仍然轮转**：
		// 本仓每个账号有独立的 X-Machine-ID / X-Session-ID，换号确实可能直接通过。
		//
		// 为什么必须按 code 判、且必须排在 11140 之前：上游对 11128 与 11140
		// **复用同一句 msg**（"request illegal"）。只看文案会把渠道级问题读成账号级
		// 故障 —— 旧行为下 Classify 给出 ErrAccountFault，落进本函数 default 的
		// NoteError，直接喂熔断（30min 起、封顶 6h），把健康号错杀。
		//
		// status 限定 400/403：429 与 5xx 即便带 11128，语义仍是限流 / 上游故障，
		// 交给下面的 status 分支（对齐同类项目的 "does not override throttling"）。
	case code == 11140:
		// 11140 = **request illegal**（请求非法），不是账号封禁（v0.24.3）。
		//
		// 旧实现此处是 `Disable` + 归档，reason 写「account banned by upstream」——
		// 2026-10-13 实测：一个风控窗口内批量误杀 30 个健康账号（凭证与余额完好）。
		// 现在与健康检测同纪律：**软冷却（可逆）+ 累计计数，连续超阈值才摘除**
		// （见 pool.NoteAccountFault 与 docs/封号事件诊断与账号隔离核对-20261013.md）。
		if fails, disabled := p.pool.NoteAccountFault(uid); disabled {
			// 达阈值后的处置**必须与健康检测同源**（v0.24.8）：走 app 注入的回调，
			// 由 `pool_delete_on_11140` 统一决定「删除 or 停用+归档」。
			// 此前这里写死 RetireAccount，于是同一个错误码在两条链路上强度不同
			// （健康检测可能真删、转发路径只停用），而开关保护不了后者。
			const reason = "连续 11140（请求非法）超阈值，已暂停使用"
			if p.onRetire11140 != nil {
				p.onRetire11140(uid)
			} else if p.pool.RetireAccount(uid, upstream.ErrAccountFault, reason) {
				p.archiveAbnormal(uid, reason, false)
			}
		} else {
			// R5（v0.26.1）：改用专用的 accountFaultCooldown（2h），不再用 softCooldown（60s）——
			// 60s 会让「连续 3 次」在几分钟内凑满，把健康号摘掉；见常量处的说明。
			p.pool.Cooldown(uid, pool.CoolSoft, accountFaultCooldown,
				"11140 request illegal（累计 "+itoa(fails)+"/"+itoa(pool.AccountFaultThreshold())+"，未超阈值仅软冷却）")
		}
	case code == 12153:
		// 与健康检测**同纪律**（v0.24.7 对齐）：累计阈值后才摘除，不再一次即停。
		// 处置收敛到 handleSessionDead（v0.24.8）—— 本码的另一个触发源是
		// refresh 失败（见 applyRefreshFailure），两处必须共用同一份实现。
		p.handleSessionDead(uid)
	case code == 14017:
		p.pool.Cooldown(uid, pool.CoolSoft, softCooldown, "account fault (14017)")
		// autoRecover **false**（v0.23.1 修复）：14017 是「试用未开通 / 权限异常」，
		// 与余额类不同 —— 它不是「等一等就好」，而是**必须重新登录 / 重新激活**才能解决。
		// 此前标 true 导致：软冷却（60s）一到期，reconcileAbnormal 就把它放回原分组，
		// 下一次请求立刻再次 14017 → 归档 → 回归 → 归档 的循环，且每次都当作正常号
		// 参与选号、白耗一次请求。标记为需人工后，它会稳定留在异常组直到人工处置。
		p.archiveAbnormal(uid, "账号故障（14017），需重登", false)
	case code == 14020 || status == 402:
		p.pool.CooldownUntilTomorrow4AM(uid, "余额不足")
		p.archiveAbnormal(uid, "积分不足（无法消耗积分）", true)
	case status == 429:
		p.pool.CooldownSoftRate(uid, softCooldown, resetAt, "429 rate limit"+resetSrcSuffix(resetSrc))
	case status == 403:
		p.pool.CooldownSoftRate(uid, softCooldown, resetAt, "waf 403 block"+resetSrcSuffix(resetSrc))
	case status == 401:
		// 401 有两种完全不同的东西（v0.31.41 统一）：
		//   · body 里带 "12153" / "Offline user session not found" → 会话真失效，
		//     与 code==12153 是**同一件事**，必须走同一套处置（分档 + 标红 + 被动降级）。
		//     此前这里只做 NoteSessionDead：计数到 3 次就把号停掉，但卡片上**什么都没有**
		//     （不标红、不给重登按钮、不给探活复活）—— 运维只会看到"号突然不能用了"。
		//     同一个事件两条纪律，正是本仓反复复发的那个错。
		//   · 401 但没有会话失效证据（典型是 WAF / APISIX 的 HTML 页）→ 只能按"疑似"
		//     计数，不标红、不硬冷却：把风控页判成会话失效，代价比漏判更高。
		if kind == upstream.ErrSessionDead {
			p.handleSessionDead(uid)
		} else {
			p.pool.NoteSessionDead(uid)
		}
	case status >= 500:
		p.pool.NoteFailures(uid)
	default:
		// 请求级错误分流：按共享 Classify 识别，避免旧实现「一律 NoteError」把
		// 与账号无关的失败记到账号头上（见函数头注释）。
		switch kind {
		case upstream.ErrContentBlocked, upstream.ErrPromptTooLong,
			upstream.ErrImageInvalid, upstream.ErrBadParams:
			// 零动作：不冷却 / 不熔断 / 不喂连败（内容、上下文、图片、请求体是
			// 请求自身的问题）。调用方仍会轮转（见 ServeChat 的 continue）。
		case upstream.ErrModelBlocked:
			// 11102「该后端无此模型」：(账号, 模型) 负缓存避让，切模型即可用。
			p.pool.BlockModelBackoff(uid, model, upstream.ModelBlockReason)
		case upstream.ErrNotFound:
			// 404 偶发路径缺失：固定短冷却防雪崩，不随 soft_rate 退避、不喂熔断。
			p.pool.Cooldown(uid, pool.CoolSoft, notFoundCooldown, "upstream 404")
		default:
			// 请求级错误**不罚号**（v0.31.41）。
			//
			// 这里用 isClientFaultKind 作判据，而不是再列一份清单 —— 因为那个函数
			// 已经在别处决定「这类错误该不该回 4xx 给客户端」。此前两处各写一份：
			// 同一个 ErrClient 在**响应**上被判定为"客户端过错、回 400"，
			// 在**账号处置**上却落进 NoteError 喂熔断（30min 起、封顶 6h）。
			//
			// 后果不是理论问题：客户端发来一批参数不对的请求（工具 schema 缺字段、
			// 参数组合不被支持、客户端版本差异），每个请求轮转 3 个账号，
			// 于是**健康号被按次累计熔断**，小号池会直接进入"本地误判没有可用账号"
			// 的状态（同类项目 xiaofan6ya 1.0.7 修的就是这条）。
			//
			// 边界（有意保留的不对称）：
			//   · 仍**继续轮转** —— 小概率存在"这个号的渠道配置不同"，
			//     而轮转预算本来就有限，代价可控；
			//   · 证据不丢 —— recordConsumeFailure 已在函数开头逐条落账
			//     （code / kind / 请求形状），排障不依赖"账号失败计数"这一条线。
			if isClientFaultKind(kind) {
				break
			}
			p.pool.NoteError(uid)
		}
	}
	// R4（v0.26.1）：指纹被拦样本回流。挂在**唯一返回点**而不是每个 case 里 ——
	// 分类已经算好，各 case 各写一遍必然漏一份。默认关闭时这里是零成本返回。
	p.maybeSampleFingerprint(kind, uid, model, reqBody, body)
	return kind
}

// routeView 路由侧的可观测位（v0.31.7）。
//
// 用 map 而不是结构体：这是**对外下发**的视图，字段名必须显式（v0.29.0 的
// 「提现审核按钮点不动」就是内嵌无 json tag 结构体导致键名变成 Go 字段名）。
func (p *Proxy) routeView() map[string]any {
	p.mifMu.Lock()
	inflight := len(p.modelInFlight)
	p.mifMu.Unlock()
	return map[string]any{
		// model_inflight 当前有在途请求的 (账号, 模型) 键数（>0 说明正在起作用）。
		"model_inflight": inflight,
		// model_busy_skips 累计"因该 (账号,模型) 忙而改选别的号"的次数。
		"model_busy_skips": p.modelBusySkips.Load(),
	}
}

// promptDegradeView 提示词降级的可观测面（v0.31.4）。
//
// 用 map 而不是结构体：这是**对外下发**的视图，字段名必须显式（v0.29.0 的
// 「提现审核按钮点不动」就是内嵌无 json tag 结构体导致键名变成 Go 字段名）。
func (p *Proxy) promptDegradeView() map[string]any {
	until := p.degrade.Until()
	out := map[string]any{
		"degraded": !until.IsZero(),
		"triggers": p.degrade.Triggers(),
		"until":    int64(0),
	}
	if !until.IsZero() {
		out["until"] = until.Unix()
	}
	return out
}

// handleSessionDead 12153（会话失效）的**统一处置**（v0.24.8）。
//
// 为什么抽出来：这个码有两个触发源 —— chat 出站返回 12153（reportUpstreamError
// 的 code 分支），以及 **refresh token 失败**返回 12153（applyRefreshFailure）。
// 抽取前 webapp 的两条路径纪律不同：出站走「累计阈值 + 摘除闸门」，refresh 走
// 无条件 NoteFailures（只降权 10 分钟）。于是同一个错误码，一个号该不该被摘除
// 取决于它是"聊天时被发现"还是"刷新时被发现"—— 这正是本仓反复复发的
// 「同一错误码多套纪律」。
//
// 纪律：一次不摘（12153 会被网络抖动/刷新竞态临时触发），累计达阈值才走
// Pool.RetireAccount 闸门；未达阈值只做可逆软冷却。计数由任意一次成功请求
// （NoteSuccess）清零，故「偶发一次」不会累积。
func (p *Proxy) handleSessionDead(uid string) {
	// 首次 12153 即**硬冷却至次日 04:00**（v0.31.30，对齐同类项目 workbuddy2api-panel）：
	//
	// 12153 = 会话失效（refresh token 已废），继续把这个号排进候选只会白打一次上游，
	// 而 04:00 是上游额度/签到的重置墙钟，也是"重新登录后最有意义的重试点"。
	// 之前只给 10 分钟软冷却，于是"冷却一过又被选中 → 又失败"往复，既不服务也永远不摘。
	//
	// 与「3 次才摘号」并存：**冷却挡重试，阈值管要不要摘**。一次 12153 有可能是
	// 并发刷新的抖动，所以仍不因一次就摘；但它已经证明"这个号此刻不能服务"，
	// 把它留在候选里没有任何好处。
	// ---- 先分档：refresh 丢了 ≠ 这个号不能用了（v0.31.37）----
	//
	// 上游的 refresh 是**轮换制**：同一账号在号主本机客户端上也登着时，两边共用
	// 同一条轮换链，谁后刷谁把对方手里的 refresh 打掉（invalid_grant / 12153），
	// 但 **access token 往往还剩几十天**，照常能服务。
	//
	// 此前这种情况被直接判成"登录失效"：硬冷却到次日 04:00 + 整卡标红。于是运维
	// 看到的是一批"莫名其妙同时掉线"的号，而它们其实只是丢了续期权。处置方向错在
	// 把两件事当一件：**能不能服务**（看 access）与**能不能自动续期**（看 refresh）。
	//
	// 现在的分档：
	//   · access 仍有效（ExpiresAt 在未来）→ 记 refresh_lost + 软冷却，
	//     不写 token_dead_at、不进硬冷却，卡片给"仅剩 access"预警；
	//   · access 也过期/未知            → 才是真死，走原来的硬冷却 + 标红。
	acc, _ := p.store.GetAccount(uid)
	accessAlive := acc != nil && acc.ExpiresAt > time.Now().Unix()
	if accessAlive {
		p.pool.Cooldown(uid, pool.CoolSoft, softCooldown, "refresh 已失效，仅剩 access（需尽快重登）")
		if err := p.store.MarkRefreshLost(uid); err != nil {
			log.Printf("WARN: 标记 refresh 丢失失败 uid=%s: %v", uid, err)
		}
		// ---- 冲突自动降级（v0.31.40）----
		//
		// 这个分支的含义是确定的：**我们的 refresh 被上游作废了，但 access 还活着**。
		// 在"同一个号两边都在用"的场景里，这就是冲突的现场证据 —— 对方刚刷过，
		// 我们手里的票成了废票。
		//
		// 此时继续主动刷新只会做两件事：① 每次都去撞一次已经作废的票（可能触发
		// 上游的重放判定，把新签发的那份也一起作废）；② 一旦我们刷成功，
		// 反过来把对方的票打掉，形成"互相顶"的循环。
		//
		// 所以自动降到「只消费」：从此只用 access、不再主动刷。这样冲突的**我方这一半**
		// 就被消除了 —— 之后只有本机客户端在刷，我们只是读者。
		// 代价（必须让运维看见）：access 到期就断，需要在到期前重登一次。
		if err := p.store.SetRefreshMode(uid, refreshModePassive); err != nil {
			log.Printf("WARN: [refresh] %s 自动降级失败：%v", uid, err)
		} else {
			p.pool.SetPassiveRefresh(uid, true)
			log.Printf("WARN: [refresh] %s 检测到会话冲突（refresh 已废 / access 仍有效）→ 自动降级为「只消费」，不再主动续期；请号主退出本机客户端后手动切回", uid)
		}
		log.Printf("WARN: [token] %s refresh 12153 但 access 仍有效（exp=%d），按「仅剩 access」处理",
			logfmt.Label(uid, acc.Nickname), acc.ExpiresAt)
		// 不累计 sessionDeadFails：这不是"这个号不能服务"，累计会把可用号摘掉。
		return
	}
	p.pool.CooldownUntilTomorrow4AM(uid, "12153 会话失效（需重新登录）")
	// 落库标记（v0.31.30）：卡片要整卡标红 + 给「重新登录 / 导入 JSON」按钮。
	// store 写失败不影响处置（冷却已在内存生效），但会少一个界面提示 —— 记日志。
	if err := p.store.MarkTokenDead(uid); err != nil {
		log.Printf("WARN: 标记登录失效失败 uid=%s: %v", uid, err)
	}
	if p.pool.NoteSessionDead(uid) {
		if p.pool.RetireAccount(uid, upstream.ErrSessionDead, "连续 12153（会话失效）超阈值，已禁用，需重登") {
			p.archiveAbnormal(uid, "会话失效（12153）连续超阈值，已禁用", false)
		}
	}
}

// applyRefreshFailure refresh token 失败时的账号处置（v0.24.8 补齐分流）。
//
// 修复前这里**不分错误类型**一律 NoteFailures（连败降权，10 分钟级），于是：
//
//	· 12153（会话真的失效、必须重登）永远走不到 sessionDeadFails —— 号被降权 10 分钟，
//	  冷却一过又被选中、又 refresh 失败，如此往复：既服务不了请求，也永远不被摘除；
//	· 与 scheduler keepalive/checkin、旧网关 handler 三处纪律不一致。
//
// 现在：12153 → 走 handleSessionDead（与出站发现 12153 完全同源）；其余（网络抖动 /
// 5xx / 解析失败）保持既有降权口径 —— 与 reportUpstreamError 对 5xx 的处理一致，
// **有意**不改成旧网关的 NoteError 熔断（理由见 reportUpstreamError 函数头：上游 5xx
// 多为全局故障，按熔断处理会把整池批量摘除）。
func (p *Proxy) applyRefreshFailure(uid string, err error) {
	var ue *upstream.Error
	if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
		p.handleSessionDead(uid)
		return
	}
	p.pool.NoteFailures(uid)
}

// archiveAbnormal 把账号归档到「异常」消耗分组（v0.13.0）。
//
// autoRecover=true（积分不足 / 账号故障这类**临时**异常）时记下原分组，
// 冷却解除后由 reconcileAbnormal 自动放回；false（封号 / 会话失效）需管理员
// 重登后手动改回，不会被自动放回。
//
// 只做「库 + 内存映射」两处更新，不整体 SyncAccounts：本函数在失败重试链路上被调用，
// 保持轻量、避免与正在进行的池同步相互干扰；下次同步自然会以库为准。
func (p *Proxy) archiveAbnormal(uid, reason string, autoRecover bool) {
	if uid == "" || p.store == nil {
		return
	}
	acc, err := p.store.GetAccount(uid)
	if err != nil || acc == nil {
		return
	}
	if effectiveConsumeGroup(acc) == cgAbnormal {
		return // 已归档：保留首次原因与原分组，避免后续错误覆盖
	}
	// prev 记**原始显式值**（可能是空串），不记生效值（v0.23.1 修正）。
	//
	// 为什么必须记原始值：记生效值会把「未显式设置分组」**永久固化**成显式值。
	// 具体危害（真实路径）：
	//   代挂号 cg="" 未入池 → 生效推断为「闲置」→ 运营停用 → 记 prev="idle"
	//   → 运营启用 → 回归写 cg="idle"（**显式**）→ 之后用户申请入池并审批通过，
	//   账号本应生效为「用户申请」，却因 cg 被显式锁成 idle 而**仍然不参与消耗**。
	// 记原始空串后，回归落 cg=""，交由 inferConsumeGroup 按**当时**的
	// pool_state / owner 推断 —— 这正是 restoreGroupValue 已支持的语义
	// （其注释：有原分组则用它，否则回落「按来源推断」）。
	//
	// 与 v0.20.1 对「下架恢复分组」的同类修正保持同一原则：恢复原始语义，
	// 而不是恢复一个当时的快照值。
	prev := strings.TrimSpace(acc.ConsumeGroup)
	note := reason
	if autoRecover {
		note = autoTagPrefix + reason
	}
	if err := p.store.MarkAccountAbnormal(uid, prev, note); err != nil {
		return
	}
	p.selMu.Lock()
	if p.consumeGroupByUID == nil {
		p.consumeGroupByUID = map[string]string{}
	}
	p.consumeGroupByUID[uid] = cgAbnormal
	p.selMu.Unlock()
}

// reconcileIntervalSec 异常自动回归的复查间隔（秒）。每次选号前最多触发一次。
const reconcileIntervalSec = 60

// reconcileAbnormal 轻量节流地复查「自动归档」到异常组的账号，
// 把已恢复可用的放回原消耗分组。节流命中时异步执行，不阻塞请求。
func (p *Proxy) reconcileAbnormal() {
	if !p.reconcileDue() {
		return
	}
	go p.reconcileAbnormalOnce()
}

// reconcileDue 原子节流：距上次复查不足 reconcileIntervalSec 则返回 false。
func (p *Proxy) reconcileDue() bool {
	now := time.Now().Unix()
	last := atomic.LoadInt64(&p.lastReconcile)
	if now-last < reconcileIntervalSec {
		return false
	}
	return atomic.CompareAndSwapInt64(&p.lastReconcile, last, now)
}

// reconcileAbnormalOnce 执行一轮回归：临时异常（带 autoTagPrefix）且池中已恢复
// 可用（未停用、未手动停用、无冷却）的账号，恢复到归档前的分组。
//
// 「可自动回归」由**写入时的 autoRecover 标记**决定，回归侧不再重判根因 ——
// 这不是偷懒，而是刻意的单一来源：归档方（reportUpstreamError / 健康检测 / 停用）
// 最清楚这次失败的根因类别，把结论编码进标记即可；回归侧若再自行推断，
// 两处判据必然分叉。故：
//
//	余额耗尽（402/14020）→ autoRecover=true   余额恢复即可用，冷却到期即回归
//	运营停用            → autoRecover=true   解除停用即可用
//	登录失效（12153）    → autoRecover=false  需重登（且已在 Disable 路径直接禁用）
//	上游封禁（11140）    → autoRecover=false  需重登 / 换号
//	权限异常（14017）    → autoRecover=false  需重登激活（v0.23.1 从 true 修正为 false）
//	管理员手动设的异常    → 无标记             视为最终裁决，永不自动回归
//
// 边界：`prev` 为空时回落 inferConsumeGroup（按来源推断），故回归目标未必逐字等于
// 归档前那个值 —— 但一定是一个**合法的常规组**（RestoreAccountConsumeGroup 内部
// 防御性拒绝把目标写成异常组自身）。
func (p *Proxy) reconcileAbnormalOnce() {
	if p.store == nil {
		return // 轻量测试脚手架（无库）直接跳过
	}
	list, err := p.store.ListAbnormalAutoAccounts()
	if err != nil || len(list) == 0 {
		return
	}
	changed := false
	for _, acc := range list {
		st, ok := p.pool.Status(acc.UID)
		if !ok {
			continue // 不在池中（已删除 / 未同步）：等下一次同步后再判
		}
		if st.Disabled || st.ManualDisabled || st.Cooling || st.CoolRemaining > 0 {
			continue // 仍未恢复
		}
		if err := p.store.RestoreAccountConsumeGroup(acc.UID, acc.ConsumeGroupPrev); err != nil {
			continue
		}
		p.selMu.Lock()
		if p.consumeGroupByUID != nil {
			if g := restoreGroupValue(acc); g != "" {
				p.consumeGroupByUID[acc.UID] = g
			}
		}
		p.selMu.Unlock()
		changed = true
	}
	if changed {
		_ = p.SyncAccounts() // 以库为准整体重算（分组 / 白名单映射同步）
	}
}

// restoreGroupValue 计算回归后的生效分组：有原分组则用它，否则回落「按来源推断」。
func restoreGroupValue(acc *Account) string {
	if g := normalizeConsumeGroup(acc.ConsumeGroupPrev); g != "" && g != cgAbnormal {
		return g
	}
	return inferConsumeGroup(acc)
}

// parseUpstreamCode 从上游响应体里尽力解析业务错误码；解析不出返回 0。
//
// 实现收敛在 `upstream.ErrorCode`（v0.31.3）：旧网关也要判同一批码，而"同一个错误码
// 只能有一套纪律"是本项目最贵的一条教训 —— 解析口径抄两份就会出现"一个产品认得、
// 另一个不认得"的漂移。此处只是本包调用点的薄封装。
func parseUpstreamCode(body []byte) int { return upstream.ErrorCode(body) }

// streamSSE 把上游 SSE 原样转发给客户端，同时抽取 usage 供计费。
// 边转发边解析：上游通常在末帧才补全 usage.credit，但逐帧覆盖式采集能兼容
// 「首帧给 token、末帧给 credit」与「末帧一次性给全」两种形态。
// streamModelErr 流内（HTTP 200）出现的**模型级**错误帧。
//
// 为什么需要这个类型（v0.31.1）：上游有时把错误放在**已经 200 的流里**（error 帧），
// 而不是用 4xx 拒绝请求。旧实现把带 error 键的帧原样透传给客户端，
// **池侧完全不知道** —— 于是这个 (账号, 模型) 不会被冷却，下一个同模型请求照样被
// 路由到它，再次撞同一个错。这正是「冷却机制存在但没生效」的一类漏洞。
type streamModelErr struct {
	Code int
	Raw  []byte // 该帧原文（供台账记录与重置时刻解析）
}

// detectStreamModelErrorFrame 从一行 SSE 文本里提取模型级错误帧。
//
// 只认**模型级** code（6004 模型限额 / 11102 该后端无此模型）—— 这是它存在的唯一目的。
// 其他 code 一律返回零值，交给既有链路处置：流式热路径上不做重复分类，
// 也让本函数的语义边界与 (账号, 模型) 冷却严格对齐。
//
// 快速拒绝：绝大多数行不含 "error"，一次 Contains 即排除（热路径零分配、零解析）。
func detectStreamModelErrorFrame(line string) streamModelErr {
	if !strings.Contains(line, "error") {
		return streamModelErr{}
	}
	payload, ok := upstream.StripSSEDataPrefix(line)
	if !ok || !strings.Contains(payload, "error") {
		return streamModelErr{}
	}
	raw := []byte(payload)
	// 「模型级」的判据是共享单点（upstream.ModelLevelErrorCode）：6004 / 11102。
	if code := upstream.ErrorCode(raw); upstream.ModelLevelErrorCode(code) {
		return streamModelErr{Code: code, Raw: raw}
	}
	return streamModelErr{}
}

// reportInStreamModelError 对「HTTP 200 + 流内模型级错误帧」补做 (账号, 模型) 冷却。
//
// 与 4xx 路径**共用同一套冷却原语**（CooldownSoftForModel / BlockModelBackoff），
// 因此「生效范围、时长、恢复逻辑」与 6004 的 4xx 形态完全一致，不存在两套口径。
//
// 刻意**只**做模型级那一件事，不跑 reportUpstreamError 的全部分支：
// 这条路径上响应已经发出去一半，换号重试早已无意义；账号级处罚（熔断/归档/停用）
// 需要更强的证据，不该由一段流内文本触发。
//
// status 传真实值（200）而不是伪装成 429：台账要能区分「上游用 4xx 拒绝」与
// 「上游在流里报错」这两种形态，否则排障时会把后者读成前者。
func (p *Proxy) reportInStreamModelError(uid, model string, body, reqBody []byte) {
	code := parseUpstreamCode(body)
	if code == 0 || uid == "" {
		return
	}
	var src upstream.ResetSource
	switch code {
	case 6004:
		var resetAt time.Time
		resetAt, src = upstream.ResolveResetAt(nil, body, time.Now())
		p.pool.CooldownSoftForModel(uid, softCooldown, resetAt, model,
			"6004 model rate limit (in-stream)"+resetSrcSuffix(src))
	case 11102:
		p.pool.BlockModelBackoff(uid, model, upstream.ModelBlockReason)
	default:
		return
	}
	p.store.recordConsumeFailure(ConsumeFailure{
		Stage:       FailStageUpstream,
		Code:        code,
		Kind:        upstream.Classify(http.StatusOK, string(body)).String(),
		Status:      http.StatusOK,
		Model:       model,
		UID:         uid,
		Source:      "proxy-stream",
		ResetSource: string(src),
		Detail: fmt.Sprintf("上游 200 + 流内错误帧：%s｜请求形状 %s",
			firstLine(upstream.UpstreamDisplayMsg(string(body))), requestShape(reqBody)),
	})
	log.Printf("WARN: [proxy] 流内错误帧 acct=%s model=%s code=%d → 已按 (账号,模型) 写入冷却",
		logfmt.Label(uid, ""), model, code)
}

// streamSSE 逐行透传上游 SSE 到客户端，并返回本次用量。
//
// 返回值 err（v0.17.5 新增）：读上游 body 过程中的错误。**必须返回而不是吞掉** ——
// 此前调用方拿不到任何信号，上游断流被记成一次成功请求（流水 status=ok、ttfb 正常），
// 而客户端那侧是「执行到一半没下文了」。
//
// [DONE] 兜底（v0.17.5）：上游正常收尾会自带 `data: [DONE]`（透传时已写出），
// 但两种情况会漏 —— 上游流被中断，或上游实现漏发。漏发对 OpenAI 兼容客户端是致命的：
// 客户端以 `[DONE]`（或 finish_reason）判定「回答结束」，没有它就一直挂着等，
// 表现为「会话中断 / 一直转圈」。这里无条件兜底补一个，与 internal/upstream 的
// StreamHint 同口径（那条成熟链路一直有，本链路此前缺失）。
func streamSSE(w http.ResponseWriter, resp *http.Response) (usageInfo, streamModelErr, error) {
	return streamSSEHint(w, resp, nil)
}

// streamSSEHint 同 streamSSE，但在透传**上游 error 帧**前把 hintFn(payload) 附加为
// error.gateway_hint（v0.31.5）。hintFn 为 nil/返回空串 → 帧逐字节原样透传。
//
// 与 upstream.StreamHint 的分工：上游包那份是"库"（旧网关删除后已无产品调用方），
// 这份是 webapp 自己的流实现（额外带 usage 采集与模型级错误帧提取），两者共用
// 同一张判据表（upstream.GatewayHint / FrameKind），不存在两套口径。
func streamSSEHint(w http.ResponseWriter, resp *http.Response, hintFn func(string) string) (usageInfo, streamModelErr, error) {
	var ui usageInfo
	var me streamModelErr // 首个模型级错误帧（若有）：供调用方补做 (账号,模型) 冷却
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
	sawDone := false
	for scanner.Scan() {
		line := scanner.Text()
		parseUsageLine(line, &ui)
		// 只取**首个**模型级错误帧：一次流里同一 (账号,模型) 只会被冷却一次，
		// 重复取没有增量，反而让"首个原因"被后面的帧覆盖掉。
		if me.Code == 0 {
			me = detectStreamModelErrorFrame(line)
		}
		if isSSEDone(line) {
			sawDone = true
		}
		out := line
		if hintFn != nil {
			// 上游 error 帧：附加 error.gateway_hint 后透传（message 原文不动）。
			// 只在真的带 error 键时才尝试改写，正常数据帧零开销。
			if payload, ok := upstream.StripSSEDataPrefix(line); ok && strings.Contains(payload, "error") {
				if hint := hintFn(payload); hint != "" {
					if rewritten := upstream.AttachHintToErrorFrame(payload, hint); rewritten != payload {
						out = "data: " + rewritten
					}
				}
			}
		}
		fmt.Fprintf(w, "%s\n", out)
		if line == "" && flusher != nil {
			flusher.Flush()
		}
	}
	if flusher != nil {
		flusher.Flush()
	}
	err := scanner.Err()
	if !sawDone {
		// 补 [DONE] 收尾：写失败（客户端已断）不影响——对方人都走了，收尾没有意义。
		if _, werr := io.WriteString(w, "data: [DONE]\n\n"); werr == nil && flusher != nil {
			flusher.Flush()
		}
	}
	return ui, me, err
}

// isSSEDone 判定一行是否为 SSE 结束帧（容忍 "data: [DONE]" 与 "data:[DONE]" 两种写法）。
func isSSEDone(line string) bool {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "data:") {
		return false
	}
	return strings.TrimSpace(strings.TrimPrefix(t, "data:")) == "[DONE]"
}

// parseUsageLine 从一行 SSE 里抽取 usage。
//
// 解析口径**统一在 upstream.ParseUsageLine**（v0.26.7）—— 这里只负责"写进 ui"。
// 收敛前本函数与旧网关（internal/server/logging.go）各写一份，已经漂移出两处
// 真实差异（webapp 漏采缓存三字段、旧网关只认一种 data 前缀），
// 详见 internal/upstream/usage.go 顶部注释。
func parseUsageLine(line string, ui *usageInfo) {
	f, ok := upstream.ParseUsageLine(line)
	if !ok {
		return
	}
	ui.PromptTokens = f.PromptTokens
	ui.CompletionTokens = f.CompletionTokens
	ui.CacheHitTokens = f.CacheHitTokens
	ui.CacheMissTokens = f.CacheMissTokens
	ui.CacheWriteTokens = f.CacheWriteTokens
	if f.HasCredit {
		ui.Credit = f.Credit
		ui.HasCredit = true
	}
}

// sseChunk SSE data 帧的宽松解析结构（只取聚合所需字段）。
type sseChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Model   string `json:"model"`
	Created int64  `json:"created"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Content string `json:"content"`
			Role    string `json:"role"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *json.RawMessage `json:"usage"`
}

// AggregateResult 非流式聚合输出。
type AggregateResult struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index   int `json:"index"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage json.RawMessage `json:"usage,omitempty"`
}

// aggregateSSE 消费上游 SSE 并聚合为一次非流式响应。
// content 兼容两种上游形态：增量 delta（拼接）与累积快照（取最长）。
// aggregateSSE 消费上游 SSE 并聚合为一次非流式响应。
// content 兼容两种上游形态：增量 delta（拼接）与累积快照（取最长）。
//
// 返回值 err（v0.17.5 新增）：读上游 body 的错误。**聚合路径的静默截断比流式更危险** ——
// 它不逐帧发给客户端，而是读完整流再一口气写 JSON：上游中途断掉时，此前的实现会把
// 半截 content 当成**完整回答**以 200 发出，客户端完全无法察觉后半段丢了。
// 现在改为：中断即 502（此时尚未写任何响应字节，状态码还能改），让失败可诊断、可重试。
func aggregateSSE(w http.ResponseWriter, resp *http.Response) (usageInfo, streamModelErr, error) {
	var me streamModelErr // 同上：流内模型级错误帧（非流式客户端也会遇到）
	var (
		content strings.Builder
		id      string
		model   string
		created int64
		finish  string
		usage   json.RawMessage
	)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if me.Code == 0 {
			me = detectStreamModelErrorFrame(line)
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			if data == "[DONE]" {
				break
			}
			continue
		}
		var ck sseChunk
		if err := json.Unmarshal([]byte(data), &ck); err != nil {
			continue
		}
		if ck.ID != "" {
			id = ck.ID
		}
		if ck.Model != "" {
			model = ck.Model
		}
		if ck.Created != 0 {
			created = ck.Created
		}
		if ck.Usage != nil {
			usage = *ck.Usage
		}
		for _, c := range ck.Choices {
			cur := content.String()
			inc := c.Delta.Content
			if inc == "" {
				continue
			}
			if cur == "" || strings.HasPrefix(inc, cur) {
				content.Reset()
				content.WriteString(inc)
			} else {
				content.WriteString(inc)
			}
			// 聚合内容总长上限（v0.24.0，P2-4）。
			//
			// 为什么必须有：单行受 scanner 缓冲（4MB）约束，但**整段流此前无上限** ——
			// 聚合路径用 strings.Builder 把 content 全攒在内存里，异常/被劫持的上游
			// 可以持续吐超长内容把进程 OOM（流式路径不受影响：逐行写出，受 TCP 背压约束）。
			// 超限即报错（此时尚未写任何响应字节，状态码还能改），不静默截断 ——
			// 截断出来的"完整回答"比失败更危险（客户机会当成真的）。
			if content.Len() > maxAggregateContentBytes {
				writeOpenAIError(w, http.StatusBadGateway,
					fmt.Sprintf("upstream stream too large (> %d bytes)", maxAggregateContentBytes))
				return usageInfo{}, me, fmt.Errorf("aggregate content exceeds %d bytes", maxAggregateContentBytes)
			}
			if c.FinishReason != nil && *c.FinishReason != "" {
				finish = *c.FinishReason
			}
		}
	}
	if err := scanner.Err(); err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream stream interrupted: "+err.Error())
		return usageInfo{}, me, err
	}
	if id == "" {
		id = "chatcmpl-" + newRandomID(12)
	}
	if created == 0 {
		created = time.Now().Unix()
	}
	if finish == "" {
		finish = "stop"
	}
	out := AggregateResult{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   model,
	}
	out.Choices = make([]struct {
		Index   int `json:"index"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	}, 1)
	out.Choices[0].Index = 0
	out.Choices[0].Message.Role = "assistant"
	out.Choices[0].Message.Content = content.String()
	out.Choices[0].FinishReason = finish
	out.Usage = usage

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(out)
	return parseUsageJSON(usage), me, nil
}

// parseUsageJSON 从 usage 原始 JSON 抽取计费字段。
//
// 与流式路径**同一口径**（upstream.ParseUsageObject）：非流式是另一种客户端形态，
// 口径分叉会让"某类客户端的观测凭空消失"。
func parseUsageJSON(raw json.RawMessage) usageInfo {
	var ui usageInfo
	f, ok := upstream.ParseUsageObject(raw)
	if !ok {
		return ui
	}
	ui.PromptTokens = f.PromptTokens
	ui.CompletionTokens = f.CompletionTokens
	ui.CacheHitTokens = f.CacheHitTokens
	ui.CacheMissTokens = f.CacheMissTokens
	ui.CacheWriteTokens = f.CacheWriteTokens
	if f.HasCredit {
		ui.Credit = f.Credit
		ui.HasCredit = true
	}
	return ui
}

// settleUsage 结算一次请求：算钱 → 与预扣对账 → 写流水。
//
// **本方法是适配器**（v0.31.11，P2-7 第三刀下半）：真正的编排在
// `internal/billing.Settler.Settle` —— 那里能独立单测（造一个假 Ledger 即可），
// 不必拉起整个 app。本方法只负责三件**本包才有资格做的事**：
//
//  1. **模型名证据源**：成功请求把模型名写进"上游认过"清单（见下）；新包不认识 modelname；
//  2. **组装 Request**：把 *User / *APIKey / *Reservation 这些本包类型摊平成标量
//     （新包不认识它们，也不该认识）；
//  3. **触发分成**：`Result.HostCreditDue` 为真时调用 creditHostOwner ——
//     它需要"这个号归谁、是否已入池"这类**选号侧状态**，只有本包有。
//
// 计费口径（v0.12.0，需求 7 明确）：**唯一基准 = 本次被消耗的号池账号积分（consumedCredit）× 倍率**。
//
//	用户扣费（点）= 号池积分消耗 × 计费倍率（倍率由管理员在「设置 → 计费」配置，单位：点 / 1 积分）
//
// consumedCredit 取上游 usage.credit —— 即该次请求从 serving 账号里**实扣掉**的积分数
// （上游计费域的直接口径）。上游没回报 credit 时（老版本 / 异常响应）才用池里已学的
// 「账号+模型」实测单价 × token 数估算，仍估不出则按 0 计并标记 missing_credit 便于对账。
// 代挂分成 / 推广返点同样以**号池积分消耗**为基数折算（号主按自己的积分成本拿收益，
// 不受计费倍率缩放），分成本身再以本次平台实收封顶，见 creditHostOwner。
//
// 返回值：settled 表示预扣是否已被处理（false 说明结算异常，调用方应走释放路径把钱退回）；
// chargedMicro 是本次真正记账的扣费（微点），供实时态势（/api/admin/live）展示。
// stickyHash 与 attempts 是**选号轨迹**（仅供流水排查，不参与任何计费计算）：
//   - stickyHash 会话粘性键的 sha256 前 12 hex（不落明文：键可能是客户端 conversation_id）；
//   - attempts   本次实际尝试的账号数（1 = 首选即成功，>1 = 中途换过号）。
//
// 为什么要记：此前流水只有最终 upstream_uid，无法回答「同一场对话的多轮是否落到不同
// 账号」—— 那正是「调用过程中切换账号」这一问题的**观测盲区**（只能靠时间聚类猜）。
func (p *Proxy) settleUsage(u *User, key *APIKey, model, upstreamUID string, ui usageInfo, resv *Reservation, status, errMsg, stickyHash string, attempts int) (settled bool, chargedMicro int64) {
	return p.settleUsageScoped(u, key, model, upstreamUID, ui, resv, status, errMsg, stickyHash, attempts, "")
}

// settleUsageScoped 与 settleUsage 相同，但可以指定**消费归类**（v0.31.68，需求 P2）。
//
// scope 非空时只认 billing.ScopeRelay：pool / host 的归类依赖 KeyKind 等既有事实，
// 不该由调用方改写（改错就是"平台买单"或者"号主被白嫖"）。
func (p *Proxy) settleUsageScoped(u *User, key *APIKey, model, upstreamUID string, ui usageInfo, resv *Reservation, status, errMsg, stickyHash string, attempts int, scope string) (settled bool, chargedMicro int64) {
	// 成功的模型名进入"上游认过"清单（v0.31.2），供模型名写法归一使用。
	// 只记成功：失败记录里的名字可能正是写法错的，把它写进清单会让下一次
	// 把**正确**写法归一成错的（最危险的方向）。
	//
	// ⚠️ 这一段**刻意留在本包**（而不是搬进 billing.Settle）：它是**模型名的证据源**，
	// 属于 modelname 的职责；新包不认识它。注意它在管理通道分支**之前** —— 用管理密钥
	// 的成功请求同样是一次"上游认过这个名字"的证据。
	if status == "ok" {
		rememberModel(model)
	}

	req := billing.Request{
		HasUser:     u != nil,
		Model:       model,
		UpstreamUID: upstreamUID,
		Usage:       ui,
		Status:      status,
		ErrMsg:      errMsg,
		StickyHash:  stickyHash,
		Attempts:    attempts,
	}
	if u != nil {
		req.UserID = u.ID
	}
	if key != nil {
		req.KeyID = key.ID
		req.KeyKind = key.Kind
	}
	if resv != nil {
		req.HasReservation = true
		req.ReservationID = resv.ID
	}

	// 代挂分成的**资格**在本包解析（要查号主与入池状态），**金额与入账**交给
	// internal/billing —— 它把"扣费+流水+分成+返点"放进同一个事务（v0.31.60，审查 F02）。
	// 此前是先结算、再回来发分成，两次提交之间进程被杀就是"扣了钱没发分成"。
	if u != nil {
		req.Host = p.hostShareFor(upstreamUID)
	}
	// 单模型计费倍率（v0.31.67）：在结算前取一次快照值。
	// 取不到（未配置）时留 0，结算内部会回落全局 —— 这里**不要**自己兜底成 1，
	// 否则"全局倍率改成 2"会让所有未配置模型的请求都按 1 倍扣（静默少收一半）。
	//
	// 通道请求用另一份视图（v0.31.68）：同一个模型名在号池与通道上成本结构不同，
	// 共用一个倍率必然有一边亏。
	if scope == billing.ScopeRelay {
		req.Scope = billing.ScopeRelay
		req.Multiplier, _ = p.ratioForRelay(model)
	} else {
		req.Multiplier, _ = p.ratioFor(model)
	}

	res := p.billingSettler().Settle(req)
	return res.Settled, res.ChargedMicro
}

// hostShareFor 解析"这笔收益该给谁"（分成资格 + 上级推广资格）。
//
// 只回答三个事实：是不是代挂号、有没有入池、比例多少（以及号主的上级是谁）。
// **金额一律不算** —— 那是 billing.HostEarn / ReferralRebate 的职责，
// 两处各算一遍必然漂移（本项目最贵的一类缺陷）。
func (p *Proxy) hostShareFor(uid string) *billing.HostShare {
	if uid == "" {
		return nil
	}
	p.selMu.RLock()
	owner := p.ownerByUID[uid]
	inPool := p.poolInByUID[uid]
	p.selMu.RUnlock()
	if owner <= 0 || !inPool {
		return nil
	}
	// 比例解析放在拿到 owner 之后（v0.17.7）：逐用户覆盖优先于全局配置，
	// 所以必须先知道这笔收益归谁，才能取到正确的比例。
	ratio := p.hostRatioFor(owner)
	if ratio <= 0 {
		return nil
	}
	hs := &billing.HostShare{OwnerUserID: owner, Ratio: ratio}
	// 上级推广：比例按**收益人（上级）**解析 —— 这个比例描述的是「上级能从下级
	// 收益里抽多少」，属于上级的属性，不是下级号主的属性。
	up, err := p.store.GetUserByID(owner)
	if err != nil || up == nil || up.ReferredBy <= 0 || up.ReferredBy == owner {
		return hs
	}
	if r := p.referralRatioFor(up.ReferredBy); r > 0 {
		hs.ReferrerID = up.ReferredBy
		hs.ReferrerRatio = r
	}
	return hs
}

// logAdminChannelUsage 已随结算编排一起下沉 internal/billing（v0.31.11）：
// 它是 `Settler.Settle` 里 `HasUser=false` 那条分支（settleAdminChannel），
// 口径与计费路径共用一份实现。本包不再有这个函数 —— 如果你在找它，
// 去 internal/billing/settle.go 搜 `settleAdminChannel`。
// creditHostOwner / creditReferrerOfPool 已于 v0.31.60 **删除**（审查 F02 收尾）：
//
// 它们原本由 settleUsage 在结算之后调用，于是"扣费"与"发分成"是两次独立提交，
// 中间任何一步失败就是半截账（扣了钱没发分成 / 发了分成没扣到钱）。
// 现在改成：资格解析留在本包的 hostShareFor（见上），金额与入账由
// internal/billing.planHostCredit + Store.CommitSettlement 在**同一个事务**里落地。
//
// 如果你在找"号主分成到底怎么算"：去 internal/billing 搜 HostEarn；
// "怎么入账"：搜 CommitSettlement。**不要**在本包再写一遍入账逻辑。

// estimateCredit 用实测成本估算缺失的 credit：per1k × tokens / 1000。
//
// 单价来源两级回落（v0.12.0 修复）：
//  1. pool.ModelCost —— (账号, 模型) 粒度观测，由网关侧 pool.NoteModelCost 写入；
//  2. Proxy.costPer1k —— (模型) 粒度观测，由本进程 settleUsage 的 observeCost 写入。
//
// 为什么要两级：cmd/webapp 不经过 internal/server，pool.NoteModelCost 从不被调用，
// 故第 1 级在 webapp 运行时**永远为空**（实测：账号卡片 model_costs 恒 undefined）。
// 旧实现只读第 1 级 → 上游不回报 credit 时直接按 0 计费，纯漏计。
// 两级单位相同（均为「积分 / 1k token」），故可安全回落。
func (p *Proxy) estimateCredit(upstreamUID, model string, tokens int) float64 {
	if model == "" || tokens <= 0 {
		return 0
	}
	if upstreamUID != "" {
		if per1k, ok := p.pool.ModelCost(upstreamUID, model); ok && per1k > 0 {
			return billing.EstimateCredit(per1k, tokens)
		}
	}
	p.costMu.RLock()
	per1k, ok := p.costPer1k[model]
	p.costMu.RUnlock()
	if ok {
		return billing.EstimateCredit(per1k, tokens) // per1k <= 0 时自返 0（与旧行为一致）
	}
	return 0
}

// writeOpenAIError 输出 OpenAI 兼容的错误响应。
// writeOpenAIError 不带 gateway_hint 的错误响应（hint 未覆盖形态时走这里）。
func writeOpenAIError(w http.ResponseWriter, status int, msg string) {
	writeOpenAIErrorHint(w, status, msg, "")
}

// writeOpenAIErrorHint 带 gateway_hint 的错误响应（v0.31.5）。
//
// hint 为空串 → 字段**缺席**（与 writeOpenAIError 逐字节一致）：hint 表未覆盖的形态
// 宁可不写，也不编造一句"看起来像结论"的话。
//
// 为什么只**新增**一个键而不改既有形状：webapp 既有客户端（前端 api()、下游 OpenAI
// 兼容客户端）读的是 error.message / error.code，动它们就是破坏兼容；hint 是纯增量。
func writeOpenAIErrorHint(w http.ResponseWriter, status int, msg, hint string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	e := map[string]any{
		"message": msg,
		"type":    "upstream_error",
		"code":    status,
	}
	if hint != "" {
		e["gateway_hint"] = hint
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"error": e})
}
