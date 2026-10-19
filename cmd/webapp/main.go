// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// Command webapp 把 workbuddy2api 的多账号网关做成单进程 Web 应用：
// 自带管理 UI + OpenAI 兼容接口，账号存 SQLite（替换 auths/ 目录与 state.json）。
//
// 历史沿革（Web 化时与旧网关 cmd/server 的差异清单；**旧网关已于 v0.31.5 整体删除**，
// 本仓现在只有这一个产品）：
//  1. 存储：auths/*.json → SQLite；state.json → pool_state.json（由 pool 包自己落盘）
//  2. OAuth：临时 state 文件 → 进程内会话表（多并发不互相覆盖）
//  3. 定时任务：四类纯 Go 任务（签到/旅行/活跃/保活）直接复用 internal/scheduler；
//     开学季与夜猫子依赖 exec 拉 python 脚本，未打包故禁用
//  4. 路径：无硬编码 /app/data，全部走 flag
//
// v0.2.0：调度内核由自研游标轮转替换为复用 internal/pool + internal/session，
// 继承原网关的三因子加权选号、软/硬冷却分级熔断、6004 模型级限额、
// 会话粘性（30m 滚动续期）、token 临近过期自动保活。
//
// v0.3.0：多租户中转站——用户体系、API Key、按上游 credit 计费、兑换码充值。
//
// v0.4.0：定时任务接线（账号自动签到/做任务维持额度），凭证经 auth.SaveHook
// 即时回写 SQLite（不再因无 FilePath 而静默丢刷新结果）。
//
// v0.5.0：计费硬化 —— 预扣-结算（按 max_tokens 上界冻结，多退少补，崩溃可对账）
// 与用户级日/月消费闸。此前是纯事后结算，单笔长请求能把余额打成负数。
package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"workbuddy2api/internal/api/fingerprint"
	"workbuddy2api/internal/httpx"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/modelname"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

//go:embed ui
var uiFS embed.FS

// version Web 应用版本标记。
//
// ⚠️ **发布时务必与仓库根的 `version.json` 同步**（v0.26.4 起由测试强制：
// TestVersionMatchesManifest 会红）。这个常量从 v0.26.0 起被漏改过三次
// （0.26.1/0.26.2/0.26.3 都没动），后果不只是"banner 显示旧版本"：
//
//   - **版本更新提醒的 `current` 取的就是它**（updatecheck.go 的 decideUpdate(version, …)）→
//     current 恒为 0.26.0 而清单是 0.26.4 → 页面**误报"有新版本"**，诱导用户去"升级"到
//     自己已经在跑的版本；反向错（清单也停旧）则会**漏报真实更新**。
//   - /healthz（已鉴权）、/api/admin/live、/metrics 的 wb_webapp_info 都回显它 →
//     运维看到的版本与真实二进制不符，排障时会怀疑错方向。
//
// 之所以要测试而不是靠自觉：这四处都是**间接**消费（通过常量），改代码时不会有人
// 顺手想起它；只有让测试在"两处版本号不一致"时直接失败，才守得住。
const version = "0.31.56"

type app struct {
	store  *Store
	proxy  *Proxy
	tasks  *taskRunner // 定时任务（签到/旅行/活跃/保活），-no-tasks 时为 nil
	key    string      // 管理密钥（命令行/脚本运维通道）
	secret *SecretBox
	// 开放注册下的两道闸：注册与登录各自按 IP 限速。
	registerLimiter *rateLimiter
	loginLimiter    *rateLimiter
	// secureCookies 强制给会话 Cookie 加 Secure（-secure-cookies）。
	// 默认 false = 自动判定（TLS 或可信代理声明 https 时才加），见 cookieSecure：
	// 本项目默认跑纯 HTTP 本机，无条件加会让本地登录失效。
	secureCookies bool
	// trustedProxies 采信其转发头的额外网段（-trusted-proxies，CIDR 逗号分隔）。
	// nil = 只信 loopback（隧道与同机反代的默认形态），见 clientIP。
	trustedProxies *ipSet
	// metricsPublic 是否**公开** /metrics（-metrics-public，默认 false）。
	// 默认需要管理员权限：该端点聚合了用户数/用量/号池规模，经公网隧道暴露等于
	// 免费给扫描器一份「这台机器有多少用户和多少可用账号」的清单（P3-11）。
	metricsPublic bool
	// dataDir 数据目录（webapp.db / secret.key / admin.key 所在）。
	// 导入白名单**永远**不允许覆盖到这里（v0.24.2，P3-8，见 resolveImportPath）。
	dataDir string
	// egressVerify 线路准入验证的可替换实现（**仅测试**，v0.31.47）。
	//
	// 生产为 nil → 用 verifyLineUsable（真实发一次请求经该代理出去）。
	// 测试里替换成桩，是因为"绑号前必须验证线路"这条纪律本身要保留，
	// 而单测环境没有可用代理 —— 用桩保留结构、绕开网络。
	egressVerify func(url string) (string, error)
	// modelRatioFetch 模型倍率同步的目录读取（**仅测试**，v0.31.51）。
	modelRatioFetch func() ([]upstream.ModelInfo, error)
	// agentDir agent 产物目录（-agent-dir，v0.31.42）。空 = 可执行文件同级 egress-agent/。
	// 服务端从这里读 agent-version.json 与二进制，供 agent 自更新下载（见 agent_dist.go）。
	agentDir string
	// authKeysPath 覆盖 authorized_keys 的写入位置（v0.31.25，仅测试与特殊部署用）。
	//
	// 空 = 走 egressAuthorizedKeysPath() 的常规推导（/root/.ssh/authorized_keys）。
	// 之所以做成可注入：写入 authorized_keys 是**高危操作**，而测试绝不能碰
	// 开发者本机真实的 ~/.ssh/authorized_keys —— 那是在别人机器上留下后门。
	authKeysPath string
	// importRoots 「服务端路径导入」允许读取的目录白名单（-import-roots 追加，
	// 默认含账号文件的规范目录 %USERPROFILE%\.wb-switch）。见 resolveImportPath。
	importRoots []string
	// probes 探活类出站的全局闸（v0.24.3）：限全局并发 + 单账号最小间隔。
	// 见 healthchat.go —— 2026-10-13 事故的产物（前端逐卡出站 → 59 并发同出口 → 上游风控）。
	probes *probeGate
	// pluginGate 插件执行类出站的闸（v0.31.16）：与 probes **同类型、独立实例**。
	//
	// 为什么不共用 probes：领取补偿是**写类**出站，可能比探活慢得多；共用一道闸
	// 会出现"跑一次批量领取把并发额度占满 → 页面上所有积分探活都被拒"的互相干扰。
	// 类型复用（同一套语义：全局在途上限 + 同号最小间隔）而实例独立 —— 语义一处、
	// 额度各管各的。
	pluginGate probeGate
	// /v1/* 客户端维度限流（按 API Key / IP），RPM 由管理员设置，0 = 不限。
	limiter *clientLimiter
	// 告警状态（/metrics 暴露 + webhook 跃迁通知）。
	alerts *alertState
	// 以下三个字段是**懒初始化**的内部缓存/节流器（v0.24.0 消除数据竞争）。
	//
	// 为什么改（原先的写法有真实数据竞争）：此前是裸的
	//
	//	if a.taskCache == nil { a.taskCache = &taskStatusCache{} }
	//
	// 多个请求 goroutine 首次并发访问会**同时写同一个指针字段**；还可能各建一份、
	// 后写覆盖先写（缓存丢失）。
	//
	// 现在统一走 `cachesOnce`：一次加锁，把三个字段一起初始化。
	// 为什么不用三个 sync.Once —— ~ 因为 Once 执行过就再也不会执行，
	// 于是**字段一旦被置回 nil 就再也回不来**（测试需要重置节流器，见
	// poolgroup_test.go 的 `app.poolGroups = nil`）。用 mutex + 惰性检查既消除
	// 竞争，又保留「可重置」这一测试与运维都需要的能力（app.initCaches 显式重置）。
	cachesOnce sync.Mutex
	// taskCache 账号「任务完成情况」查询的短 TTL 缓存（见 taskstatus.go）。
	taskCache *taskStatusCache
	// growthCache 成长任务「完成进度」查询的短 TTL 缓存（见 growthprogress.go）。
	growthCache *growthProgressCache
	// poolGroups 号池分组自动维护的单账号节流器（v0.20.0，见 poolgroup.go）。
	poolGroups *poolGroupGuard
}

// initCaches 惰性初始化内部缓存/节流器；并发安全，且**可在置 nil 后重新初始化**
// （与 sync.Once 的关键差别：Once 只跑一次，字段置 nil 后就再也建不起来）。
func (a *app) initCaches() {
	a.cachesOnce.Lock()
	defer a.cachesOnce.Unlock()
	if a.taskCache == nil {
		a.taskCache = &taskStatusCache{}
	}
	if a.growthCache == nil {
		a.growthCache = &growthProgressCache{}
	}
	if a.poolGroups == nil {
		a.poolGroups = &poolGroupGuard{}
	}
}

// startedAt 进程启动时间（/metrics 的 uptime 基准）。
var startedAt = time.Now()

// reconcileReservationsOnStartup 启动期的预扣对账；**诊断模式必须跳过**。
//
// 抽成独立函数是为了让它**可测** —— 它承载一条靠肉眼守不住、又涉及钱的契约：
// 只有「本进程将成为服务进程」时才能对账。诊断进程（`-check-update`）没有自己的在途
// 请求，表里残留的记录只可能属于**正在服务的那个进程**，退了就是替别人退款。
//
// 回归测试见 TestReconcileReservationsSkippedInDiagnosticMode。
func reconcileReservationsOnStartup(store *Store, diagnosticOnly bool) {
	if diagnosticOnly {
		// 刻意静默：诊断模式的输出要保持「只有一个版本检查结果」，不掺启动噪音。
		return
	}
	if n, refunded, err := store.ReconcileReservations(); err != nil {
		log.Printf("预扣对账失败: %v", err)
	} else if n > 0 {
		log.Printf("预扣对账：退还 %d 笔在途冻结，合计 %s 点", n, formatPoints(refunded))
	}
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8780", "监听地址（默认仅本机）")
	dbPath := flag.String("db", "./data/webapp.db", "SQLite 数据库路径")
	apiKey := flag.String("key", "", "管理密钥（脚本/运维通道）；留空则启动自动生成并打印")
	secretPath := flag.String("secret", "", "主密钥文件（加密用户 API Key）；留空=数据目录/secret.key，传 none 禁用加密")
	noTasks := flag.Bool("no-tasks", false, "禁用定时任务（签到/旅行/活跃/保活）与池凭证回写")
	secureCookies := flag.Bool("secure-cookies", false,
		"强制会话 Cookie 带 Secure（全链路 https 时开启；默认按请求自动判定）")
	trustedProxies := flag.String("trusted-proxies", "",
		"额外可信代理的 CIDR 列表（逗号分隔，如 10.0.0.0/8）。默认只信 loopback；\n"+
			"仅当前置代理不在本机时才需要配置（配置后才会采信其 X-Forwarded-For）")
	metricsPublic := flag.Bool("metrics-public", false,
		"公开 /metrics（默认需要管理员权限或 X-Admin-Key）。\n"+
			"仅在「抓取端就在本机且不打算给密钥」时开启；公网/隧道部署**不要**开")
	importRoots := flag.String("import-roots", "",
		"「服务端路径导入」额外允许读取的目录（逗号分隔，绝对路径）。\n"+
			"默认只允许账号文件的规范目录（%USERPROFILE%\\.wb-switch）；数据目录恒不可读")
	modelProbeDelay := flag.Duration("model-probe-delay", 20*time.Minute,
		"模型级冷却的半开探测：进入冷却后先等多久才允许首次探测（<=0 = 用默认 20m）。\n"+
			"探测是唯一会为'已被判定不可用'的 (账号, 模型) 主动发上游请求的路径，见 pool.SetModelProbe")
	modelProbeInterval := flag.Duration("model-probe-interval", time.Hour,
		"模型级冷却的半开探测间隔（同一账号+模型两次探测的最小间隔；\n"+
			"0/负 = 关停探测，回到'严格等上游墙钟'）")
	modelProbeGlobal := flag.Duration("model-probe-global", 5*time.Minute,
		"模型级冷却的半开探测**全池**间隔（同一模型两次探测之间的全局最小间隔）。\n"+
			"这是出站强度的主闸门：没有它，池里 N 个冷却号会被客户端连续请求逐个探一遍")
	agentDir := flag.String("agent-dir", "",
		"agent 产物目录（含 agent-version.json 与 wb2api-egress-agent.exe），供 agent 自更新下载。"+
			"留空 = 可执行文件同级 egress-agent/；目录不存在时自动更新对 agent 呈 disabled")
	checkUpdate := flag.Bool("check-update", false,
		"只做一次版本更新检查并打印结果，然后退出（不启动服务）。\n"+
			"供运维脚本/定时巡检使用；退出码 0=已最新 1=有新版 2=检查失败")
	flag.Parse()

	// diagnosticOnly 纯诊断模式：本进程**不会承接任何流量**，因此绝不允许做
	// 「会改变业务状态」的启动副作用。
	//
	// 为什么必须有这个区分（2026-10-14 实测踩到）：`-check-update` 复用的是**完整启动
	// 路径**，于是它也会执行 ReconcileReservations() —— 把 reservations 表里的在途预扣
	// **全额退还给用户**。那个设计本意是回收「上个进程被强杀」留下的孤儿预扣，但它分不清
	// 孤儿和**别的进程正在服务中的**预扣：真服务进程在服务某请求时，预扣记录同样在表里，
	// 于是诊断进程把这笔钱退掉 → 服务随后结算时 `SettleReservation` 找不到记录 →
	// 记 `[settle_failed:not found]` → 该笔**实际未被扣费**（方向对用户有利、不会重复
	// 计费，但属于非预期变更，且诊断命令不该动钱）。
	//
	// 教训：诊断命令「只读」是错觉 —— 只要复用完整启动路径，就会带上所有启动副作用。
	// **以后新增任何不接流量的诊断开关，都要并进这个变量。**
	diagnosticOnly := *checkUpdate

	// 可信代理集合（v0.24.1，P3-5）：不配 = 只信 loopback。
	proxySet, err := parseIPSet(*trustedProxies)
	if err != nil {
		log.Fatalf("解析 -trusted-proxies 失败：%v", err)
	}

	if *apiKey == "" {
		// 管理密钥的**默认安全路径**（v0.24.0 加固）：
		//
		// 此前启动脚本把密钥硬编码成固定值传给 -key，于是所有部署共用同一个密钥，
		// 且脚本本身会随分发包外发 —— 等于把后台交出去（见 docs/项目全面审查报告）。
		// 现在改为：**不传 -key 时由本程序生成并持久化到数据目录**。
		//
		// 为什么落盘而不是每次随机：密钥每次重启都变，运维脚本/隧道探活就全废了。
		// 放在 <数据目录>/admin.key（与 webapp.db、secret.key 同处，不进分发包）。
		//
		// 优先级：-key 参数 > 环境变量（脚本侧读）> admin.key 文件 > 新生成。
		// 环境变量由启动脚本处理（见 start-webapp.bat），这里只管文件与生成。
		// ⚠️ 数据目录必须**先建**（v0.27.0 修）：下面要把 admin.key 写进去，
		// 而首次部署时该目录还不存在，MkdirAll 却排在密钥处理之后 —— 结果是
		// 全新目录首次启动**密钥落盘失败**，退化成"临时密钥、重启就变"；
		// 而启动脚本 / 隧道探活引用的是上一次的密钥，表现为"刚配好就又失效"。
		if dir := filepath.Dir(*dbPath); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				log.Fatalf("创建数据目录失败: %v", err)
			}
		}
		keyPath := filepath.Join(filepath.Dir(*dbPath), "admin.key")
		// R4：被拦样本文件与库 / 密钥同目录（-db 决定），避免依赖进程 CWD。
		fpSampleFile = filepath.Join(filepath.Dir(*dbPath), "fingerprint_samples.jsonl")
		if v, err := os.ReadFile(keyPath); err == nil {
			if s := strings.TrimSpace(string(v)); s != "" {
				*apiKey = s
				log.Printf("已从 %s 加载管理密钥（脚本通道用；后台仍可正常登录）", keyPath)
			}
		}
		if *apiKey == "" {
			*apiKey = "wb-" + newRandomID(24)
			if err := os.WriteFile(keyPath, []byte(*apiKey+"\n"), 0o600); err != nil {
				// 落盘失败不阻断启动，但要明确告知「重启会变」，否则脚本会莫名其妙失效。
				log.Printf("警告：管理密钥落盘失败（%v）——本次为临时密钥，重启后会改变", err)
				log.Printf("本次管理密钥：%s", *apiKey)
			} else {
				// 首次生成必须打印一次：这是运维唯一的 bootstrap 途径（脚本通道要用）。
				// 之后的重启只打印路径，密钥不再进日志。
				log.Printf("已生成管理密钥并保存到 %s（首次生成，请抄走；该文件不随分发包外发）", keyPath)
				log.Printf("本次管理密钥：%s", *apiKey)
				if runtime.GOOS == "windows" {
					// Windows 忽略 0o600 权限位，文件实际继承目录 ACL。
					log.Printf("提示：Windows 下请确认 %s 仅当前用户可读（icacls 收紧）", keyPath)
				}
			}
		}
	}

	store, err := OpenStore(*dbPath)
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	defer store.Close()

	// 模型名清单种子化（v0.31.2）：把历史**成功**用过的模型名装进内存，
	// 供模型名写法归一使用（见 modelname_seed.go 的"为什么必须做"）。
	// 放在服务开始接流量之前：否则重启后的前若干请求会因为清单空而漏掉归一化
	// （实测过：`GLM-5.3` 仍 404，而 `" glm-5.3 "` 已 200 —— 差别就在清单）。
	seedRecentModels(store)

	// 主密钥：用于加密入库的用户 API Key（可回显给本人，但不明文落库）。
	secretFile := *secretPath
	if secretFile == "" {
		secretFile = filepath.Join(filepath.Dir(*dbPath), "secret.key")
	}
	var box *SecretBox
	if secretFile != "none" {
		box, err = LoadOrCreateSecret(secretFile)
		if err != nil {
			log.Fatalf("加载主密钥失败: %v", err)
		}
		log.Printf("主密钥已加载（%s）", secretFile)
	} else {
		log.Printf("警告：已禁用加密（-secret none），用户 API Key 不可回显，账号 token 亦明文落库")
	}
	// 账号 AccessToken/RefreshToken 静态加密：写在读两端的边界内透明加解密，
	// 老明文行读时原样透传、下次写回自动转密文，无需一次性迁移。
	store.SetSecret(box)

	// 池状态落盘到数据目录（与 SQLite 同目录），重启后冷却/成本台账不丢。
	dataDir := filepath.Dir(*dbPath)
	stateFp := filepath.Join(dataDir, "pool_state.json")
	px := NewProxy(store, stateFp)
	// 模型级半开探测的三档节奏（v0.31.6）：6004 的恢复时刻来自上游墙钟（封顶 24h），
	// 上游提前解除时只能靠主动试出来。三档分别管「进冷却后多久才可探」「同号多久可再探」
	// 「全池同模型多久可再探」。出站强度可按需下调或关停（interval=0）。
	px.pool.SetModelProbe(*modelProbeDelay, *modelProbeInterval, *modelProbeGlobal)

	// 凭证落盘接到 SQLite：必须在任何 token 刷新之前装好，否则刷新成功的凭证
	// 会因「无 FilePath」而丢失（详见 tasks.go installCredentialHook）。
	installCredentialHook(store)

	// 预扣对账：上次进程被强杀时残留在途预扣要把钱退回去。
	// 那些请求的结果已无从得知（响应可能只发了一半），退还方向对用户有利；
	// 也不会重复计费 —— 正常结算路径已 delete 记录，能留下的只有中断的。
	//
	// ⚠️ **只在服务进程里做**（diagnosticOnly == false）：诊断进程没有自己的在途请求，
	// 表里的记录只可能属于**别人**。见上方 diagnosticOnly 的注释。
	reconcileReservationsOnStartup(store, diagnosticOnly)

	a := &app{
		store:           store,
		proxy:           px,
		key:             *apiKey,
		secret:          box,
		secureCookies:   *secureCookies,
		trustedProxies:  proxySet,
		metricsPublic:   *metricsPublic,
		dataDir:         dataDir,
		agentDir:        *agentDir,
		importRoots:     importRootList(*importRoots, defaultDesktopAccountsPath()),
		probes:          &probeGate{},
		registerLimiter: newRateLimiter(10, time.Hour),
		loginLimiter:    newRateLimiter(30, 15*time.Minute),
		limiter:         newClientLimiter(),
		alerts:          newAlertState(),
	}

	// 自定义出站指纹规则（R4，v0.28.0）：从 kv 读回并安装到 upstream 的全局规则集上。
	// **必须在服务开始接流量之前**（否则最初一段文本走的是无规则路径，而且没有任何
	// 痕迹能看出"前 N 个请求没脱敏"）。
	//
	// 失败**不阻断启动**（详见 loadFingerprintRules 的注释：规则是增强，不是前置条件），
	// 但一定会留一条 WARN 日志，避免运维"以为规则还在生效"。
	if n, err := a.loadFingerprintRules(); err != nil {
		log.Printf("WARN: [fprules] 启动加载自定义指纹规则失败（已按无自定义规则运行）：%v", err)
	} else if n > 0 {
		log.Printf("INFO: [fprules] 已加载 %d 条自定义出站指纹规则", n)
	}

	// -check-update：只跑一次版本检查就退出，不启动 HTTP 服务与定时任务。
	// 显式清理而不是依赖 defer —— 下面会 os.Exit，defer 一个都不会执行
	// （main.go 里优雅停机那段注释专门讲过这个坑）。
	if *checkUpdate {
		st := a.checkUpdate(context.Background(), true)
		printUpdateStatus(st)
		a.proxy.Close()
		_ = store.Close()
		os.Exit(updateExitCode(st))
	}

	// 账号出口 ↔ 线路池对齐（v0.31.33）：把历史数据里"账号上有、池里没有"的出口
	// 补登记，并把同一代理的不同写法合并。**不探活** —— 启动阶段做网络请求会拖慢
	// 首屏，且失败不好处理；出口 IP 由定时探活（5 分钟一轮）补齐。
	// 失败只记日志：对齐是"数据卫生"，不该阻止服务启动。
	if res, err := a.convergeAccountEgress(false); err != nil {
		log.Printf("WARN: [egress] 启动对齐账号出口失败（不影响启动）：%v", err)
	} else if res.MergedLines+res.RegisteredLines+res.RewrittenAccounts > 0 {
		log.Printf("INFO: [egress] 启动对齐：合并重复线路 %d 条、补登记 %d 条、改写账号 %d 个",
			res.MergedLines, res.RegisteredLines, res.RewrittenAccounts)
	}

	defer a.proxy.Close()
	a.startGC()
	// 号池积分实时扣减后触发自动分组判定（v0.20.0）。
	a.proxy.onCreditChanged = a.reconcilePoolGroupOne
	// 上游判定「账号级限制」时的即时处置（v0.24.6）：上报 + 立即删除。
	// 与 onCreditChanged 同一处注入 —— Proxy 只识别与转达，善后（DB + 池 + 健康快照）
	// 属 app 层职责。未注入时 Proxy 退化为「停用」，不会静默放过。
	a.proxy.onAccountRestricted = a.handleRestrictedAccount
	// 「11140 连续超阈值」的最终处置（v0.24.8）：与健康检测共用同一个函数，
	// 让两条链路读**同一个** kv 开关（pool_delete_on_11140）决定删除还是停用。
	// 生产转发链路允许完整处置（含按策略删除）——它的语义就是"这个号现在服务不了请求"；
	// 观测型路径（刷新积分 / 导入）另走 allowDelete=false，不经过本回调。
	a.proxy.onRetire11140 = func(uid string) []string { return a.retireBy11140(uid, true) }

	// 定时任务：账号在池中自动签到/做任务维持额度。
	// 失效上报（v0.31.29）必须在构造 runner **之前**赋值：schedulerConfig 在
	// 构造时就被消费，晚一步设置就永远不生效。
	taskRunnerOnTokenDead = func(uid, nick, detail string) {
		a.reportRisk(uid, nick, "12153", detail, "token", "observed")
	}
	if !*noTasks {
		a.tasks = newTaskRunner(px, store, dataDir)
		a.tasks.Start()
		defer a.tasks.Stop()
		// 新入池账号补跑（v0.23.3）：账号在当天触发点之后入池时，当天不会再被排到
		// （最长等 ~21 小时），补跑一次签到让新号当场拿到额度与权威余额。
		// 钩子在 Proxy 同步发现「新增」账号时触发，覆盖导入 / 扫码 / 代挂审批等全部入池路径。
		px.onAccountsAdded = a.tasks.CatchupNewAccounts
		// 显式记一行：补跑只在真的有新账号入池时才产生日志（日志里没有 = 没新号，
		// 而不是"机制没生效"）。没有这行的话，运营无从区分这两种情况。
		log.Printf("新入池账号补跑已启用：新号入池后延迟 45~120s 补跑一轮签到（尊重定时任务总开关与自动签到开关）")
	} else {
		log.Printf("定时任务已禁用（-no-tasks）")
	}

	// 出口代理池的周期探活（v0.14.0 防封）：独立于账号任务，仅检查线路健康。
	probeCtx, stopProbe := context.WithCancel(context.Background())
	defer stopProbe()
	StartEgressProber(probeCtx, a)

	// 号池分组自动维护（v0.20.0）：预备组进出的实时判定由积分变更点触发，
	// 这里的循环只做「全量兜底」（重启对齐 / 漏网纠偏）。
	a.startPoolGroupReconciler(probeCtx)

	// 版本更新提醒（v0.26.0）：启动后延迟一次 + 之后每 12 小时一次。
	// 失败静默、只读缓存，绝不碰账号与出站主链路（详见 updatecheck.go）。
	go a.updateLoop(probeCtx)
	// 模型倍率自动同步（v0.31.51，需求一）：启动后补跑 + 之后每小时检查是否满 20 小时。
	go a.modelRatioLoop(probeCtx)

	mux := a.routes()

	srv := &http.Server{
		Addr: *addr,
		// 中间件顺序：安全头最外（错误响应也要带）→ 请求体上限 → CSRF。
		// CSRF 放在最内层：它需要读 Origin/Host，且拦截时希望已带上安全头。
		Handler:           recoverer(securityHeaders(limitRequestBody(a.csrfGuard(routeDiagnostics(mux)), maxRequestBody))),
		ReadHeaderTimeout: 30 * time.Second,
		// IdleTimeout / ReadTimeout 此前缺失（v0.24.0 补）：只有 ReadHeaderTimeout
		// 挡不住「连上但不发完请求」的慢速连接占资源。写超时不给 —— 流式回答
		// 可能长达数分钟，设 WriteTimeout 会把正常长回复掐断（这是刻意的取舍）。
		IdleTimeout: 120 * time.Second,
	}
	log.Printf("workbuddy2api webapp %s 启动于 http://%s", version, *addr)
	// 提示语必须跟着 P3-12 一起改：还写「首个用户自动成为管理员」会直接把运维
	// 引到一个已经不存在的功能上（他会以为注册完就是管理员，然后发现进不去后台）。
	log.Printf("管理密钥 %s（首次部署：注册时填入该密钥即成为管理员；不再有「首个用户自动提权」）",
		strings.Repeat("*", len(*apiKey)))

	// 优雅停机（v0.24.0 补）。
	//
	// 为什么必须做：此前是 `log.Fatal(srv.ListenAndServe())` —— log.Fatal 内部
	// os.Exit(1)，**main 里所有 defer 都不会执行**。后果：粘性绑定异步队列
	// (proxy.Close) 不排空、池状态可能少一次落盘 (store.Close)、正在跑的定时任务
	// 被硬杀 (tasks.Stop)、出口探活协程不退出 (stopProbe)。进程"能重启"掩盖了
	// 这些丢失，只有事后发现状态不对时才会怀疑，极难排查。
	//
	// 现在收到 SIGINT/SIGTERM 后：停止接收新连接 → 等在途请求收尾（上限 20s）→
	// 正常返回 main → 逐个执行 defer 清理。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		// 端口被占用之类的启动失败：这是致命错误，但仍要先走清理再退出，
		// 否则会留下半初始化的状态（如已打开的 SQLite 连接）。
		log.Printf("HTTP 服务异常退出：%v", err)
	case <-ctx.Done():
		log.Printf("收到退出信号，开始优雅停机…")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("优雅停机超时/失败（在途请求可能被中断）：%v", err)
	}
	log.Printf("已停机，正在执行清理（凭证回写 / 池状态落盘 / 任务停止）")
}

// authorized 校验管理密钥（X-Admin-Key 或 Bearer）——仅用于运维/脚本通道，
// 管理后台的常规入口走 adminGuard（会话 + 管理员角色）。
//
// 比较用 subtle.ConstantTimeCompare（v0.24.0）：原先用 `==`，理论上可按字节
// 逐位试探出密钥（时间侧信道）。apiKeyGuard 早已用常量时间比较，此处对齐口径 ——
// 同一件事在两处用不同实现，正是「改了 A 忘了 B」的温床。
func (a *app) authorized(r *http.Request) bool {
	if a.key == "" {
		return true
	}
	got := r.Header.Get("X-Admin-Key")
	if got == "" {
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			got = strings.TrimPrefix(auth, "Bearer ")
		}
	}
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(a.key)) == 1
}

// handleHealth 存活探针（v0.24.2，P3-11 收敛了返回内容）。
//
// 为什么保持公开：隧道与监控的探活请求不带凭据，一旦挂鉴权，探活会恒判失败，
// 于是「服务好好的，监控却说它死了」。所以这里选择公开，而不是选择安全 ——
// 代价是**必须只回最小信息**：
//   - 未鉴权：只有 status / service。这两个字段不含任何部署细节。
//   - 已鉴权（管理密钥或管理员会话）：附带 version / time，方便运维对版本与时钟。
//
// 为什么 version 不在公开部分：版本号告诉扫描器「这台机器在用哪个已知有洞的版本」，
// 而探活本身完全不需要它。这是典型的「泄漏了但没人觉得是泄漏」的信息。
func (a *app) handleHealth(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{
		"status":  "ok",
		"service": "workbuddy2api-webapp",
	}
	if a.authorized(r) || a.isAdminSession(r) {
		body["version"] = version
		body["time"] = time.Now().Unix()
	}
	writeJSON(w, http.StatusOK, body)
}

// maxRequestBody 单个请求体的上限（v0.24.0，P2-4）。
//
// 为什么必须有：本项目此前**一处都没用 http.MaxBytesReader**，约 40 处
// json.NewDecoder(r.Body).Decode(...) 全是无上限读取 —— 其中还包括**未鉴权**的
// /api/auth/register、/api/auth/login，以及「把整个账号文件塞进 JSON」的导入接口。
// 任何人发一个超大 body 就能持续吃内存。
//
// 取 16MB：导入接口要把整份账号 JSON 放在 body 里（上百个账号约 1-2MB），
// 留一个数量级余量；正常 API 请求都远小于它。真正的细分限制（如 chat 的 8MB）
// 仍由各 handler 自己加，这里只是**兜底**。
const maxRequestBody = 16 << 20

// limitRequestBody 给请求体套一个硬上限。
//
// 超限时 MaxBytesReader 让后续 Read 返回错误，handler 的既有错误分支
// 会把它转成 400/413 —— 不需要每个 handler 各写一遍。
func limitRequestBody(next http.Handler, max int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, max)
		}
		next.ServeHTTP(w, r)
	})
}

// securityHeaders 给所有响应补安全响应头（v0.18.x 加固）。
//
// 三条头的取舍：
//   - X-Content-Type-Options: nosniff —— 禁止浏览器 MIME 嗅探（防把 JSON/文本当 HTML 执行）。
//   - X-Frame-Options: DENY —— 禁止被 iframe 嵌套（防管理台/控制台被点击劫持）。
//   - Referrer-Policy: no-referrer —— 跨站跳转不带 Referer（防带 key 的 URL 外泄来源）。
//
// 为什么不上 Content-Security-Policy：本应用前端是「单 exe 内联 admin.html/dashboard.html」
// （内联 <script> + 内联 style），上严格 CSP 必须给每处内联脚本配 nonce 并改造模板，
// 收益（本场景是内网自用后台）与改造成本不匹配，留作后续独立事项。
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// recoverer 兜住 handler 里的 panic，把它变成一条**可读的 500**（v0.26.5）。
//
// ## 为什么必须有
//
// net/http 对 handler panic 的默认处置是：打一行日志 + **关闭连接**、**不发任何响应**。
// 对浏览器来说这不是"服务器错误"，而是一次网络失败 —— fetch 抛 TypeError，
// 前端 catch 到的是 "Failed to fetch" 之类无从查起的文案。
//
// 这不是假设：v0.26.4 之前「反代选号 · 防封」页的保存按钮正是踩在这上面
// （可选字段守卫被误嵌进外层块 → nil 解引用），运维看到的全部信息就是
// 「保存报错」四个字，既不知道是哪个接口、也不知道是服务端炸了还是网络不通。
// 一次 nil 解引用因此拖了很久才定位。
//
// 处置口径与 errsafe.go 一致：**细节写日志、对外只回稳定文案** ——
// panic 栈里常有内部路径与变量值，不能直接吐给调用方。
//
// 位置：放在最外层（securityHeaders 之外），这样连安全头中间件自己的 panic 也兜得住。
func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// 日志里带完整栈：panic 是"程序性缺陷"，唯一有效的排障手段就是栈。
			// 对外只回稳定文案（别把内部路径/变量值吐给调用方）。
			log.Printf("ERROR: [panic] %s %s: %v\n%s", r.Method, r.URL.Path, rec, debug.Stack())
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error": "服务器内部错误（已记录日志，请把发生时间告知管理员）",
			})
		}()
		next.ServeHTTP(w, r)
	})
}

// routeDiagnostics 路径归一 + 404 留痕（v0.31.51）。
//
// 现场（2026-10-19 20:0x）：用户报"控制台调用模型请求 Internal error 404"，
// 但服务端**没有任何日志** —— 因为 404 是在路由层产生的，压根没进任何 handler。
// 结果是只能靠猜客户端到底请求了哪条路径，猜错的代价是改错地方。
//
// 这里做两件事：
//  1. **尾部斜杠归一**：`/v1/chat/completions/` 与不带斜杠视为同一条路由。
//     不少客户端（含手写 base_url + 自己拼路径）会多带一个斜杠，Go 的 ServeMux
//     对它只回 404，而用户看到的就是一句"内部错误"。
//  2. **未匹配路径留痕**：打一行 WARN（方法/路径/UA/IP），下次再报"404"时，
//     我们能直接说出是哪个客户端、请求了哪条路径，而不是再猜一轮。
func routeDiagnostics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := r.URL.Path; len(p) > 1 && strings.HasSuffix(p, "/") {
			r.URL.Path = strings.TrimRight(p, "/")
		}
		rec := &routeStatusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if rec.status == http.StatusNotFound {
			ua := r.Header.Get("User-Agent")
			if len(ua) > 80 {
				ua = ua[:80]
			}
			log.Printf("WARN: [route] 404 %s %s ua=%q ip=%s —— 客户端请求了未实现的路径；"+
				"OpenAI 兼容接口是 POST /v1/chat/completions 与 GET /v1/models",
				r.Method, r.URL.Path, ua, clientIPOf(r))
		}
	})
}

// routeStatusRecorder 只为读状态码而存在的包装。
//
// Unwrap 必须实现：流式回答依赖 http.ResponseController 拿到底层 Writer 做 Flush，
// 少了它 SSE 会被缓冲住（表现为"回答很久才一次性出现"，比 404 更难查）。
type routeStatusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *routeStatusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status, r.wroteHeader = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *routeStatusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.status, r.wroteHeader = http.StatusOK, true
	}
	return r.ResponseWriter.Write(b)
}

func (r *routeStatusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *routeStatusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// rootHandler 根路径与"未匹配路径"的统一落点（v0.31.49）。
//
// 为什么不再直接 `mux.Handle("GET /", ui)`：Go 1.22 的 ServeMux 在**方法不匹配**时
// 回 405，而 `GET /` 会吃掉所有未注册路径 —— 于是"POST 到 /chat/completions"
// （客户端把 base_url 填成裸域名，SDK 自动补 /chat/completions）得到的是
// `405 Method Not Allowed`，看的人只会以为网关不支持这个接口，实际是**路径少了 /v1**。
// 现在：GET/HEAD 仍走静态页；其它方法一律回 404 + 明确的接入提示。
// 405 只该出现在"路径对、方法错"的地方，不该替 404 背锅。
func (a *app) rootHandler() http.Handler {
	ui := a.uiHandler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			ui.ServeHTTP(w, r)
			return
		case http.MethodOptions:
			// 预检：不放置任何跨域头（本应用同源使用），但仍要给出可读的答复。
			w.Header().Set("Allow", "GET, HEAD, POST, OPTIONS")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "no such endpoint: " + r.Method + " " + r.URL.Path,
			"hint": "OpenAI 兼容接口是 POST /v1/chat/completions 与 GET /v1/models" +
				"（也接受不带 /v1 的 /chat/completions 与 /models）；" +
				"客户端 base_url 建议填 http://<host>:<port>/v1",
		})
	})
}

// uiHandler 静态前端资源。
func (a *app) uiHandler() http.Handler {
	sub, err := fs.Sub(uiFS, "ui")
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "ui not bundled", http.StatusInternalServerError)
		})
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 前端资源一律禁止缓存。
		//
		// 为什么必须这样：本应用是「单 exe 自带前端」——升级只替换 exe，页面 URL 不变。
		// embed.FS 的文件没有 ModTime（零值），FileServer 既不发 Last-Modified 也不发
		// ETag，浏览器便按启发式规则自行缓存；于是升级后用户很可能仍加载**旧版
		// admin.html / app.js**，而它请求的是新版后端：接口改名或未实现时就会报出
		// 一句没有信息量的错误（典型如「重同步」点下去失败），**刷新也未必恢复**
		//（启发式缓存在同一会话内持续生效），只能靠强制刷新。
		//
		// 代价是每次访问重新下载几十 KB 的静态资源；对本场景（内网/自用后台，
		// 升级远比流量频繁）可以接受，换来「升级后刷新即生效」这个确定性。
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		fileServer.ServeHTTP(w, r)
	})
}

// accountView 脱敏后的账号视图（绝不下发 token）。
type accountView struct {
	UID         string `json:"uid"`
	Nickname    string `json:"nickname"`
	Realm       string `json:"realm"`
	Credits     int64  `json:"credits"`
	Disabled    bool   `json:"disabled"`
	ManualOff   bool   `json:"manual_off"`
	Cooling     bool   `json:"cooling"`
	CoolKind    string `json:"cool_kind,omitempty"`
	CoolingFor  int64  `json:"cooling_for"`
	Reason      string `json:"reason,omitempty"`
	SoftStreak  int    `json:"soft_streak,omitempty"`
	LimitedMods int    `json:"rate_limited_models,omitempty"`
	// LimitedDetail 被限的**具体模型**及其截止时刻（v0.26.4）。
	//
	// 为什么光有计数不够：6004 是**模型级**限额，账号对其它模型仍然可用。
	// 只给一个「限模×1」运维没法判断"到底哪个模型被限、什么时候恢复"——
	// 也就无法回答"这个号现在能不能接某个模型"。这是本轮用户明确提出的口径：
	// 「6004 的冷却是针对对应模型的，其他模型调用正常接」——既然行为如此，
	// 界面就必须能**看出来**它如此（否则行为正确也没人能确认）。
	LimitedDetail []limitedModelView `json:"limited_models,omitempty"`
	ModelCosts    int                `json:"model_costs,omitempty"`
	FailCount     int                `json:"fail_count"`
	ExpiresAt     int64              `json:"expires_at"`
	// Group 账号分组；Egress 该账号实际生效的出站代理（专用优先，其次全局默认；空=直连）。
	Group  string `json:"group"`
	Egress string `json:"egress"`
	// EgressError 出站代理不可用的原因（空 = 正常）。非空时该账号已被排除出平台选号，
	// 目的是不让它从本机 IP 裸奔出去（见 Proxy.egressBad 的说明）。
	EgressError string `json:"egress_error,omitempty"`
	// TokenDead 登录已失效（refresh 12153）——v0.31.30。
	//
	// 前端据此把整张卡标红并给「重新登录 / 导入 JSON」入口。**必须与"冷却"分开**：
	// 冷却等下去会好，登录失效等多久都不会好，只能重新导入。
	TokenDead bool `json:"token_dead"`
	// TokenDeadAt 失效确认时刻（unix 秒，0 = 正常）。
	TokenDeadAt int64 `json:"token_dead_at,omitempty"`
	// Source 账号来源（v0.31.37）：qr / import / 空（历史）。
	//
	// 存在的理由只有一个：让"扫码登录的号是不是更容易掉线"变成**可计算**的问题。
	// 此前来源既没落库也没下发，只能靠运维印象；而印象与数据经常相反。
	Source string `json:"source"`
	// RefreshLost refresh token 已失效但 access 仍可用（v0.31.37）。
	//
	// 与 TokenDead 的分工：TokenDead = 现在就不能服务了（必须重登）；
	// RefreshLost = 还能服务到 access 过期，但**不会再自动续期**，得提前去重登。
	// 只给一个布尔会把"还有一个月可用"的号渲染成红牌，那正是"莫名掉线"的来源。
	RefreshLost   bool  `json:"refresh_lost"`
	RefreshLostAt int64 `json:"refresh_lost_at,omitempty"`
	// RefreshMode 刷新模式（v0.31.40）：auto / passive。
	//
	// passive = 只消费 access、不主动续期。它与 TokenDead/RefreshLost 是三个独立维度：
	// 能不能服务（TokenDead）、续期票还在不在（RefreshLost）、**我们还要不要主动去续**（RefreshMode）。
	// 合成两个会让"只消费但续期票还好"这种状态无法表达。
	RefreshMode string `json:"refresh_mode"`
	// RunState 运行态枚举（R9）：把布尔位与自由文案收敛成九态，见 runstate.go。
	// 存在的理由：区分「风控冷却」与「限流冷却」—— 两者处置方向相反，此前都显示"冷却中"。
	RunState string `json:"run_state"`
	// ---- 用户代挂（hosting）----
	// OwnerID：0 = 平台自持；>0 = 该用户代挂。OwnerName 供管理后台直接展示。
	OwnerID   int64  `json:"owner_id"`
	OwnerName string `json:"owner_name,omitempty"`
	// HostGroup 号主自定的代挂分组（管理后台统一展示）。
	HostGroup string `json:"host_group"`
	// PoolState ""/"pending"/"in"/"rejected"；平台自持账号恒为 "in"。
	PoolState string `json:"pool_state"`
	HostNote  string `json:"host_note,omitempty"`
	// Earned 该账号累计为号主产生的收益（点）；EarnedCNY 为其金额形态（元，v0.17.7）。
	// 收益属「返点」→ 界面一律以金额为主展示，点数进 title 说明。
	Earned    float64 `json:"earned"`
	EarnedCNY float64 `json:"earned_cny"`
	// ---- 已挂载时长（v0.26.7）----
	// 从账号**首次接入本系统**（入库时刻）起算的自然日数，口径见 mounted.go。
	// MountedKnown=false 表示没有可信的挂载时刻（历史行/异常导入），
	// 前端应显示"—"而不是 0 天 —— "不知道挂了多久"与"刚挂上"是两件事。
	MountedAt    int64  `json:"mounted_at"`
	MountedDays  int    `json:"mounted_days"`
	MountedText  string `json:"mounted_text"`
	MountedKnown bool   `json:"mounted_known"`
	// Task 该账号的定时任务开关（v0.11.0）。
	Task accountTaskState `json:"task"`
	// ---- 计费口径（v0.12.0，需求 7）----
	// ConsumedCredit 该账号作为 serving 账号被消耗的号池积分合计；
	// ChargedPoints 据此按倍率向调用方收取的点数合计（= ConsumedCredit × 倍率）。
	ConsumedCredit float64 `json:"consumed_credit"`
	ChargedPoints  float64 `json:"charged_points"`
	ServedRequests int64   `json:"served_requests"`
	// ---- 消耗分组（v0.13.0）----
	// ConsumeGroup 管理员的显式设置（空串 = 按来源推断）；
	// ConsumeGroupEffective 生效分组（显式优先，否则推断）；
	// ConsumeGroupAuto 是否为系统自动归档到异常（可自动回归）。
	ConsumeGroup          string `json:"consume_group"`
	ConsumeGroupEffective string `json:"consume_group_effective"`
	ConsumeGroupText      string `json:"consume_group_text"`
	ConsumeGroupAuto      bool   `json:"consume_group_auto"`
	ConsumeGroupNote      string `json:"consume_group_note,omitempty"`
	// GrowthDoneAt 成长任务首次完成时间（0 = 尚未跑过，仍在新号待办里）。
	GrowthDoneAt int64 `json:"growth_done_at"`
	// CreditsKnown 池内是否已有该账号的**权威积分读数**（v0.23.3）。
	//
	// 为什么需要：Credits == 0 有两种完全不同的含义 ——「余额真的花光了」与
	// 「还没拉过」。运营在账号池页要区分它们（前者是异常、后者只是新号未初始化），
	// 而 card 上两者都显示 0。这里把判据透出去，供卡片标注与筛选使用。
	// 与 hostView.CreditsKnown（v0.23.1）同口径：都源自池内 CreditsSeenAt。
	CreditsKnown bool `json:"credits_known"`
	// CreditPacks / CreditPacksAt 上次**成功**查询到的逐套餐明细与时刻（v0.24.9）。
	//
	// 本地缓存，读它**不产生任何出站**：卡片因此能直接列出「哪些包、哪天到期」，
	// 而不必像 v0.24.3~v0.24.8 那样只显示一个总额快照。点「刷新明细」才真实出站。
	// 详见 creditcache.go 的说明（勿改回渲染即出站）。
	CreditPacks   []upstream.CreditPackage `json:"credit_packs,omitempty"`
	CreditPacksAt int64                    `json:"credit_packs_at,omitempty"`
	// CreditCachedRemain 缓存明细的可用合计（v0.25.0，排除已过期包）。
	// 卡片上那个大数字用它（而非会漂移的池内快照 Credits），使「总额」与下面的明细行自洽。
	// CreditPacksAt > 0 才有效（= 有缓存）；前端据此判断该用哪个数。
	CreditCachedRemain int64 `json:"credit_cached_remain"`
	// DeviceTokenState 该账号出站时 X-Device-Token 的来源（v0.25.1）：
	// "account"（账号自带，隔离性最好）/ "global"（全局兜底，多号共用同一凭证）/
	// "none"（该头不会被注入 —— 上游看到的是一台「无设备凭证的客户端」）。
	// 多态而非 bool：审计结论天差地别，处置也不同（现为五态）。见 devicecred.go。
	DeviceTokenState string `json:"device_token_state"`
}

// limitedModelsOf 池状态 → 视图行（按模型名排序，输出**确定**：map 遍历无序，
// 不排序会让同一份状态在界面上每次刷新顺序都变）。
//
// 上限 8 个：一个账号同时被限的模型数现实中极少超过 2–3 个，截断只是防病态数据
// 把卡片撑爆；截断时会带上 "…共 N 个" 的说明（见前端），不静默丢。
func limitedModelsOf(rows []pool.RateLimitedModel) []limitedModelView {
	if len(rows) == 0 {
		return nil
	}
	out := make([]limitedModelView, 0, len(rows))
	for _, r := range rows {
		v := limitedModelView{Model: r.Model, Reason: r.Reason}
		if !r.Until.IsZero() {
			v.Until = r.Until.Unix()
		}
		if !r.ResetAt.IsZero() {
			v.ResetAt = r.ResetAt.Unix()
		}
		if r.Probeable {
			v.Probeable = true
		}
		if !r.ProbedAt.IsZero() {
			v.ProbedAt = r.ProbedAt.Unix()
		}
		if !r.NextProbeAt.IsZero() {
			v.NextProbeAt = r.NextProbeAt.Unix()
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	const maxShown = 8
	if len(out) > maxShown {
		out = out[:maxShown]
	}
	return out
}

// limitedModelView 单个「被限模型」的视图行（账号卡片用）。
type limitedModelView struct {
	Model string `json:"model"`
	// Until 我方冷却截止（可能被 soft_rate_max 截断）。0 = 未知。
	Until int64 `json:"until,omitempty"`
	// ResetAt 上游明示的重置墙钟（未经截断）。0 = 上游没给时间信息。
	ResetAt int64 `json:"reset_at,omitempty"`
	// Reason 触发原因（含 reset 来源后缀，可看出时长是上游给的还是本地估的）。
	Reason string `json:"reason,omitempty"`
	// Probeable / NextProbeAt / ProbedAt 半开探测可见性（v0.31.6）。
	//
	// 为什么必须透出：6004 的冷却上限是 24h，运维看到「至 明天 00:00」会以为
	// 这个号今天彻底没救；而实际系统会在 delay 之后自动试探。**机制存在却看不见**，
	// 排障时只能翻代码 —— 这是本项目反复出现的一类问题，故在此显式暴露。
	// NextProbeAt 缺席 = 本次冷却窗口内不会再探（剩余时间不足 delay）。
	Probeable   bool  `json:"probeable,omitempty"`
	ProbedAt    int64 `json:"probed_at,omitempty"`
	NextProbeAt int64 `json:"next_probe_at,omitempty"`
}

// toView 由池状态生成视图；expires/分组等仅存于库的字段从 acc 补齐，
// 出站代理取实际生效值（专用优先，其次全局默认）。
// toView 单账号视图（积分明细缓存按主键现查一次）。
func (a *app) toView(st pool.Status, acc *Account) accountView {
	return a.toViewP(st, acc, a.store.AccountCreditPacks(st.UID))
}

// toViewP 带明细缓存的账号视图（列表路径用：缓存已批量载入，避免 N+1 查询）。
func (a *app) toViewP(st pool.Status, acc *Account, cp cachedCreditPacks) accountView {
	name := st.Nickname
	if name == "" {
		name = st.UID
	}
	v := accountView{
		UID:           st.UID,
		Nickname:      name,
		Realm:         st.Realm,
		Credits:       st.Credits,
		Disabled:      st.Disabled,
		ManualOff:     st.ManualDisabled,
		Cooling:       st.Cooling,
		CoolKind:      st.CoolKind,
		CoolingFor:    st.CoolRemaining,
		Reason:        st.Reason,
		SoftStreak:    st.SoftStreak,
		LimitedMods:   len(st.RateLimitedModels),
		LimitedDetail: limitedModelsOf(st.RateLimitedModels),
		ModelCosts:    len(st.ModelCosts),
		// CreditsSeenAt == 0 = 从未拉取过权威余额（新导入的号在首次签到/检测前都是它）。
		CreditsKnown: st.CreditsSeenAt != 0,
	}
	if v.CoolingFor < 0 {
		v.CoolingFor = 0
	}
	if acc != nil {
		v.FailCount = acc.FailCount
		v.ExpiresAt = acc.ExpiresAt
		v.Group = acc.Group
		v.OwnerID = acc.OwnerUserID
		v.HostGroup = acc.HostGroup
		v.PoolState = acc.PoolState
		v.HostNote = acc.HostNote
		v.Earned = MicroToPoint(acc.EarnedMicro)
		v.EarnedCNY = microToCNY(acc.EarnedMicro, a.moneyRate())
		v.Task = accountTaskStateOf(parseAccountTaskCfg(acc.TaskCfg))
		v.ConsumeGroup = acc.ConsumeGroup
		v.ConsumeGroupEffective = effectiveConsumeGroup(acc)
		v.ConsumeGroupText = consumeGroupLabel(v.ConsumeGroupEffective)
		v.ConsumeGroupAuto = isAutoArchived(acc)
		v.ConsumeGroupNote = acc.ConsumeGroupNote
		// 成长任务：0 = 尚未跑过首次（待办）；>0 = 已跑过（时间戳）。
		v.GrowthDoneAt = acc.GrowthDoneAt
		// 已挂载时长：口径集中在 mounted.go，此处只取值。
		mi := mountedOf(acc.CreatedAt, time.Now())
		v.MountedAt, v.MountedDays, v.MountedText, v.MountedKnown = mi.At, mi.Days, mi.Text, mi.Known
		if acc.OwnerUserID == 0 {
			v.PoolState = "in" // 平台自持账号隐含在池
		}
		if acc.Nickname != "" && (st.Nickname == "" || st.Nickname == st.UID) {
			v.Nickname = acc.Nickname
		}
	} else {
		// 池里有、库里没有对应行（异常态）：任务开关按缺省（全参与）展示。
		v.Task = accountTaskStateOf(AccountTaskConfig{Enabled: true})
	}
	v.Egress = a.proxy.EgressForUID(st.UID)
	v.EgressError = a.proxy.EgressErrorForUID(st.UID)
	v.CreditPacks, v.CreditPacksAt = cp.Packs, cp.At
	if cp.At > 0 {
		v.CreditCachedRemain = cachedRemain(cp, time.Now().Unix())
	}
	v.DeviceTokenState = a.deviceTokenState(acc)
	v.RunState = accountRunStateOf(v.Disabled, v.ManualOff, v.Cooling, v.Reason, v.CoolKind, v.EgressError)
	return v
}

// userNameMap 返回 user_id → 用户名（管理后台展示号主用）。
func (a *app) userNameMap() map[int64]string {
	users, err := a.store.ListUsers()
	if err != nil {
		return nil
	}
	m := make(map[int64]string, len(users))
	for _, u := range users {
		m[u.ID] = u.Username
	}
	return m
}

// accountMap 返回 uid → 库记录，供视图补齐仅存于库的字段。
func (a *app) accountMap() map[string]*Account {
	all, err := a.store.ListAccounts()
	if err != nil {
		return nil
	}
	m := make(map[string]*Account, len(all))
	for _, acc := range all {
		m[acc.UID] = acc
	}
	return m
}

func (a *app) handleStatus(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled, inFlightFull := a.proxy.pool.CountsDetailed()
	sticky := a.proxy.sess.Count()
	events, last := a.proxy.pool.CostExploreStatus()
	writeJSON(w, http.StatusOK, map[string]any{
		"total":         total,
		"healthy":       healthy,
		"disabled":      disabled,
		"cooling":       cooling,
		"inflight_full": inFlightFull,
		"sticky":        sticky,
		"cost_explore":  events,
		"explore_last":  len(last),
		"servable":      a.proxy.pool.ServableNow(),
		"alerts":        a.alertView(),
		"version":       version,
	})
}

// handlePool 透出池内部台账（模型限额、成本探索明细），供运维排查。
func (a *app) handlePool(w http.ResponseWriter, r *http.Request) {
	list := a.proxy.pool.List()
	events, last := a.proxy.pool.CostExploreStatus()
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":       list,
		"explore_events": events,
		"explore_last":   last,
	})
}

func (a *app) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	out, err := a.allAccountViewsErr()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

// allAccountViews 账号池卡片视图（忽略错误：聚合类调用方拿不到就退化为空集）。
func (a *app) allAccountViews() []accountView {
	out, _ := a.allAccountViewsErr()
	return out
}

// allAccountViewsErr 唯一的账号视图装配入口（v0.17.3 抽出）。
//
// 抽出来的原因：账号池卡片与「实时概览 · 号池仪表盘」必须**完全同源**——
// 两处各自实现一遍，迟早会出现「仪表盘说 18 个、卡片只列 17 个」这类不一致，
// 而排查这种不一致的成本远高于抽一个函数。
func (a *app) allAccountViewsErr() ([]accountView, error) {
	// 以库为准枚举（含「未申请入池」的代挂账号——它们不在池里但必须展示），
	// 再按 uid 挂上池状态（冷却/额度等运行时字段）。
	dbAccounts, err := a.store.ListAccounts()
	if err != nil {
		return nil, err
	}
	statusByUID := map[string]pool.Status{}
	for _, st := range a.proxy.pool.List() {
		statusByUID[st.UID] = st
	}
	names := a.userNameMap()
	// 各账号被消耗的号池积分 / 收取点数（v0.12.0）：按 serving 账号聚合，供卡片展示与对账。
	usageByUID, _ := a.store.UsageByAccount()
	// 积分明细缓存（v0.24.9）：**一次查询**批量载入，卡片直接展示（零出站），
	// 点「刷新明细」才出站。逐个账号查会是 N+1，故走 ByUID 批量接口。
	creditByUID := a.store.CreditPacksByUID()
	// 登录失效标记（一次轻查询；不塞进 Account 的 SELECT 列清单，避免 scan 顺序错位）。
	deadMap := a.store.TokenDeadMap()
	// 仅剩 access 标记 / 来源（同为轻查询，理由同上）。
	lostMap := a.store.RefreshLostMap()
	srcMap := a.store.SourceMap()
	passiveMap := a.store.RefreshModeMap()
	out := make([]accountView, 0, len(dbAccounts))
	for _, acc := range dbAccounts {
		st, ok := statusByUID[acc.UID]
		if !ok {
			st = pool.Status{UID: acc.UID, Nickname: acc.Nickname, Realm: acc.Realm}
		}
		v := a.toViewP(st, acc, creditByUID[acc.UID])
		if at, ok := deadMap[acc.UID]; ok {
			v.TokenDead = true
			v.TokenDeadAt = at
		}
		if at, ok := lostMap[acc.UID]; ok && !v.TokenDead {
			v.RefreshLost = true
			v.RefreshLostAt = at
		}
		v.Source = srcMap[acc.UID]
		v.RefreshMode = refreshModeAuto
		if passiveMap[acc.UID] {
			v.RefreshMode = refreshModePassive
		}
		if acc.OwnerUserID > 0 {
			v.OwnerName = names[acc.OwnerUserID]
		}
		if us, ok := usageByUID[acc.UID]; ok {
			v.ConsumedCredit = us.Credit
			v.ChargedPoints = MicroToPoint(us.Charged)
			v.ServedRequests = us.Requests
		}
		out = append(out, v)
	}
	return out, nil
}

func (a *app) handleGetKey(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"key": a.key})
}

func (a *app) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if uid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing uid"})
		return
	}
	// 走统一善后（DB → 池 → 健康快照）。此前只做前两步，删掉的号会继续留在
	// 「最近一次检测结果」里（健康快照是时点观测，不会自己发现账号已消失）。
	if _, err := a.deleteAccountsHard([]string{uid}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": uid})
}

// handleSync 手动触发账号重同步（调试用：改库后对齐池）。
func (a *app) handleSync(w http.ResponseWriter, r *http.Request) {
	rep, err := a.proxy.SyncAccountsReport()
	if err != nil {
		// 失败原因必须原样回传：前端只显示 error 字段，这里吞掉等于让用户面对
		// 一句没有信息量的「重同步失败」，也正是这类问题最难远程排。
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "重同步失败：" + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"synced":  true,
		"total":   rep.Total,
		"in_pool": rep.InPool,
		"added":   rep.Added,
		"removed": rep.Removed,
	})
}

// handleDisable 运营停用 / 恢复单个账号（管理后台账号卡的「停用 / 启用」）。
//
// v0.23.1 补上**分组联动**：停用即归档到「异常」消耗分组，恢复时自动回归原分组。
//
// 为什么必须联动（历史缺陷）：停用此前只改 `accounts.disabled` 与池内 manualDisabled，
// **完全不碰 consume_group**。后果是「一个已停用的号仍显示在『反代 / 用户申请』组里」——
// 分组语义失真（管理员在该组里看到一堆其实不参与消耗的号），且停用号在界面上与正常号
// 无法按分组区分。而「异常」组的定义正是「不参与消耗」，停用号天然属于它。
//
// 与既有机制的关系（不新造路径）：
//
//	· 复用 archiveAbnormal 写 cgAbnormal + 记 prev（生效分组）+ 打 autoTagPrefix 标记；
//	· 标记使该归档进入 reconcileAbnormal 的**自动回归**范围 —— 解除停用后，
//	  下一次复查（≤60s，本函数恢复分支会立即触发一次）即把账号放回原分组。
//	· 已在异常组的账号不会被覆盖（archiveAbnormal 内部保留首次原因）。
//
// 恢复分支**不**直接改分组：回归目标以 archiveAbnormal 记下的 prev 为准，
// 仍走同一条回归路径，避免两处各写一遍「恢复成哪个组」的规则。
func (a *app) handleDisable(disabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := r.PathValue("uid")
		if uid == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing uid"})
			return
		}
		if err := a.store.SetDisabled(uid, disabled); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		// 池侧手动停用（issue #138 语义）：与系统熔断区分开，/api/status 可分辨。
		reason := ""
		if disabled {
			reason = "web ui"
		}
		a.proxy.pool.SetManualDisabled(uid, disabled, reason)

		// 恢复时**额外**清「系统自动禁用」位（v0.23.4 修复）。
		//
		// 池内有**两个互相独立**的停用位，恢复必须各清各的（见 pool.Status 上
		// ManualDisabled 的注释：「两种可用操作不同：前者可 revive，后者该 enable」）：
		//
		//	· manualDisabled —— 运维的意图，由上面的 SetManualDisabled 清；
		//	· disabled       —— 系统的判定（12153 连续失效 / 11140 封禁），
		//	                    只有 ReviveDisabled 能清（它同时清 reason 与误判计数）。
		//
		// 缺陷现场：本函数此前只清前者（外加库里的 disabled 列），**从未调用
		// ReviveDisabled** —— 旧网关 internal/server/admin.go 是有这一步的，重写
		// webapp 时漏掉了。后果是：对「被系统禁用」的号点启用，账号在池内仍被判
		// 不可服务；而健康检测对这类号照常探测、通过后报「曾被系统禁用（12153
		// session dead）——可考虑『复活』」—— 运维反复点启用都没用，那条提示永远挂着。
		//
		// 幂等：账号本就没被系统禁用时 ReviveDisabled 返回 false 且无副作用，
		// 可安全重复调用（防「重复处理」：按钮连点 / 脚本重放）。
		revived := false
		if !disabled {
			revived = a.proxy.pool.ReviveDisabled(uid)
		}

		group := ""
		if disabled {
			// 停用 → 归档异常组（autoRecover=true：解除停用后应能自动回归）。
			a.proxy.archiveAbnormal(uid, "运营停用", true)
		} else {
			// 恢复 → 立即触发一次回归复查（否则要等下一个 60s 周期）。
			// 只处理带 auto 标记且池内已恢复可用的号；手动设的异常不在此列（既有语义）。
			a.proxy.reconcileAbnormalOnce()
		}
		// 该账号的旧检测结论立即失效（v0.23.4）：检测结果是**时点观测**，账号状态
		// 刚被本端点改变，旧结论（「已跳过（运维手动停用）」/「曾被系统禁用…可考虑
		// 复活」）已经不成立。留着它会让运维以为「启用没生效」并反复点击 —— 这正是
		// 本次 bug 的另一半成因。剔除而非标记：留着一条已知错误的结论比没有更坏。
		invalidated := healthLast.dropUIDs([]string{uid})
		// 分组与停用状态都直接影响选号范围，重算一次。
		_ = a.proxy.SyncAccounts()
		if acc, err := a.store.GetAccount(uid); err == nil && acc != nil {
			group = consumeGroupLabel(effectiveConsumeGroup(acc))
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"uid": uid, "disabled": disabled,
			// revived 表示本次是否**确实**清除了系统禁用位（false 可能是原本就没被
			// 系统禁用，或账号已不在池内 —— 用于运维诊断「为什么点了没变化」）。
			"revived": revived,
			// health_result_invalidated 表示有多少条旧的检测结论因此失效（一般 0 或 1）。
			"health_result_invalidated": invalidated,
			// 回显当前生效分组，便于前端即时反映联动结果（停用后应为「异常」）。
			"consume_group": func() string {
				if acc, err := a.store.GetAccount(uid); err == nil && acc != nil {
					return acc.ConsumeGroup
				}
				return ""
			}(),
			"consume_group_text": group,
		})
	}
}

func (a *app) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Realm string `json:"realm"`
		// UID 可选（v0.31.40）：管理员针对某个号重新登录时带上，沿用该号的设备身份。
		UID string `json:"uid"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	sess, err := StartOAuth(req.Realm, strings.TrimSpace(req.UID))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": sess.ID,
		"auth_url":   sess.AuthURL,
		"realm":      sess.Realm,
	})
}

func (a *app) handleOAuthPoll(w http.ResponseWriter, r *http.Request) {
	a.pollOAuth(w, r.URL.Query().Get("sid"), 0)
}

// pollOAuth 轮询 OAuth 完成情况并落库（ownerUserID=0 为管理员通道）。
// 抽成方法供管理端与用户代挂端共用，避免两份逻辑漂移。
func (a *app) pollOAuth(w http.ResponseWriter, sid string, ownerUserID int64) {
	if sid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing sid"})
		return
	}
	res, err := PollOAuth(sid, a.store, ownerUserID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if res.Status == "pending" {
		writeJSON(w, http.StatusOK, map[string]any{"status": "pending", "message": res.Message})
		return
	}
	// 扫码成功 = 规范化落库（v0.31.37，见 oauth_pool.go）：
	// 补 device_token / 记来源 / 按开关自动入池，并生成与导出同构的 auth JSON。
	// **必须在 SyncAccounts 之前**：自动入池改的是库里的 pool_state，而 SyncAccounts
	// 按库重建池内 `inPool` 映射 —— 顺序反了会表现为"界面上显示已入池，但选号仍跳过"。
	authJSON, errMat := a.materializeQRLogin(res.Account)
	if errMat != nil {
		log.Printf("WARN: [qr] 规范化落库失败 uid=%s: %v", res.Account.UID, errMat)
	}
	// 新号入库后立即对齐池，免去下一次请求时才发现。
	_ = a.proxy.SyncAccounts()
	acc, _ := a.store.GetAccount(res.Account.UID)
	if st, ok := a.proxy.pool.Status(res.Account.UID); ok {
		out := map[string]any{"status": "done", "account": a.toView(st, acc)}
		if authJSON != "" {
			// 前端用它做「下载 auth JSON」：扫码之后用户手里就有一份可直接再导入的凭据，
			// 不必再等导出接口（那是"事后补救"，这里是"当场交付"）。
			out["auth_json"] = authJSON
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "done", "auth_json": authJSON})
}

// modelsCache 模型列表缓存（避免每次 /v1/models 都打上游）。
//
// 并发保护（v0.24.0 修复）：此前是一个**无锁的包级 struct**，而 cachedModelIDs
// 读、refreshModelsCache 写会被多个请求 goroutine 同时碰到 —— 这是一处真实数据竞争
// （`-race` 可复现；slice header 撕裂读理论上还会 panic）。/v1/models 恰恰是客户端
// 接入时的**第一个**探测请求，多客户端同时接入就会撞上。
var modelsCache struct {
	mu      sync.RWMutex
	at      time.Time
	payload []byte
	ids     []string
}

// modelsCacheTTL 模型列表缓存有效期。
const modelsCacheTTL = 5 * time.Minute

// cachedModelIDs 返回缓存中的模型 id 列表（供 /api/models 轻量返回）。
//
// 返回副本：调用方可能持有它，不能把内部切片暴露出去（否则又变成无锁共享）。
func cachedModelIDs() []string {
	modelsCache.mu.RLock()
	defer modelsCache.mu.RUnlock()
	return copyIDs(modelsCache.ids)
}

// copyIDs 返回切片副本（nil 安全）。
func copyIDs(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

// modelsCacheFreshLocked 缓存是否仍然新鲜（调用方需持锁）。
func modelsCacheFreshLocked(now time.Time) bool {
	return now.Sub(modelsCache.at) < modelsCacheTTL && len(modelsCache.payload) > 0
}

// modelsCacheSnapshotIfFresh 在**持读锁**的前提下判断新鲜度并取走 id 副本。
//
// v0.26.1 修的真实数据竞争：此前 refreshModelsCache 的快路径直接调用
// modelsCacheFreshLocked（其注释已写明「调用方需持锁」）却**没持锁**，
// 而慢路径会在写锁下改 at/payload —— 8 个并发调用时 `go test -race`
// 稳定报 DATA RACE（TestModelsCacheSingleFlight，CI 上抓到）。
//
// 顺带把「判断 + 取值」放进同一个临界区，避免两次加锁之间的 TOCTOU：
// 若先判断再单独取副本，中间可能被刷新线程插进来，虽然只影响一次返回值，
// 但没有理由留下这个窗口。
func modelsCacheSnapshotIfFresh() ([]string, bool) {
	modelsCache.mu.RLock()
	defer modelsCache.mu.RUnlock()
	if !modelsCacheFreshLocked(time.Now()) {
		return nil, false
	}
	return copyIDs(modelsCache.ids), true
}

// modelsCacheFresh 缓存是否仍然新鲜（自带读锁，供缓存外部调用者使用）。
//
// 存在的意义：modelsCacheFreshLocked 要求调用方持锁，而「只想知道新不新鲜」的
// 调用方（如控制台模型列表入口）很容易忘记加锁 —— insights.go 就漏了。
// 提供一个自带锁的版本，让外部调用方没有写错的余地。
func modelsCacheFresh() bool {
	modelsCache.mu.RLock()
	defer modelsCache.mu.RUnlock()
	return modelsCacheFreshLocked(time.Now())
}

// modelsPayload 返回缓存载荷副本（无缓存时 nil）。
func modelsPayload() []byte {
	modelsCache.mu.RLock()
	defer modelsCache.mu.RUnlock()
	if len(modelsCache.payload) == 0 {
		return nil
	}
	out := make([]byte, len(modelsCache.payload))
	copy(out, modelsCache.payload)
	return out
}

// refreshModelsCache 拉取/复用模型列表缓存，返回 id 列表。
// 上游取不到时回落到内置清单（真实可用名以账号实际权限为准）。
//
// **单飞（single-flight）**：并发请求只放一个去打上游，其余等它 ——
// 此前每个并发请求各自 fetch，N 个客户端同时接入 = N 次上游请求（惊群）。
// 用「写锁 + 双检」实现：先读锁快路径，未命中再取写锁重查一次，命中就直接返回。
// 代价是刷新期间其他请求阻塞（最长一次上游超时），但这比"每个请求都打一次上游"好得多，
// 且 5 分钟才发生一次。不引 golang.org/x/sync/singleflight 是为了守住 3 个直接依赖。
func (a *app) refreshModelsCache() []string {
	// 快路径必须**持读锁**：modelsCacheFreshLocked 读 at/payload，
	// 而下面的慢路径会在写锁下写它们。此处曾无锁（见 modelsCacheSnapshotIfFresh 注释）。
	if ids, ok := modelsCacheSnapshotIfFresh(); ok {
		return ids
	}

	modelsCache.mu.Lock()
	defer modelsCache.mu.Unlock()
	// 双检：等锁期间可能已有别的 goroutine 刷好了。
	if modelsCacheFreshLocked(time.Now()) {
		return copyIDs(modelsCache.ids)
	}

	var ids []string
	if au := a.proxy.pool.Pick(""); au != nil {
		if list, err := fetchModelIDs(a.proxy.uc, au); err == nil {
			ids = list
		}
	}
	if len(ids) == 0 {
		ids = fallbackModels
	}
	data := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		data = append(data, map[string]any{
			"id":       id,
			"object":   "model",
			"created":  time.Now().Unix(),
			"owned_by": "workbuddy",
		})
	}
	payload, _ := json.Marshal(map[string]any{"object": "list", "data": data})
	modelsCache.at = time.Now()
	modelsCache.payload = payload
	modelsCache.ids = ids
	return copyIDs(ids)
}

func (a *app) handleModels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// 命中缓存直接写副本；未命中走 refresh（内部单飞），再取一次。
	if p := modelsPayload(); p != nil {
		_, _ = w.Write(p)
		return
	}
	a.refreshModelsCache()
	if p := modelsPayload(); p != nil {
		_, _ = w.Write(p)
		return
	}
	_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
}

// fetchModelIDs 调用上游取真实模型列表，带 panic 保护（上游异常不应拖垮进程）。
func fetchModelIDs(uc *upstream.Client, au *auth.Auth) (ids []string, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("fetch models panic: %v", rec)
		}
	}()
	list, err := uc.FetchModels(au)
	if err != nil {
		return nil, err
	}
	for _, m := range list {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}

// fallbackModels 上游模型列表取不到时兜底（真实可用名以账号实际权限为准）。
//
// 清单本体在 internal/modelname（v0.31.3）：两个产品（主产品 / 旧网关）对「最小已知
// 模型集合」必须一致，否则同一个写法只在一个产品上能被折回规范名。此处只是别名。
var fallbackModels = modelname.FallbackModels

// writeJSON 所有 JSON 响应的**唯一出口**（v0.24.1 起兼做内部错误脱敏）。
//
// 为什么在这里脱敏而不是改 150+ 个调用点：见 errsafe.go 的说明 ——
// 单点收敛、可单测、与业务判断（状态码）解耦。业务文案不受影响，
// 只有「看起来像 SQLite/栈信息」的文本会被替换，且原文仍进日志。
// writeJSON 统一的 JSON 响应出口 —— **实现已下沉 `internal/httpx.WriteJSON`**
// （v0.31.12，P2-7 第四刀起手：脱敏 + 写出是所有 handler 的横切关注点，不该由
// 每个 `internal/api/*` 包各复制一份）。
//
// 保留本包同名薄包装：它的调用点有 **552 处**，改引用点不会带来任何理解收益，
// 只会让这一刀的面积虚增（与前三刀的接线纪律一致）。
func writeJSON(w http.ResponseWriter, status int, v any) { httpx.WriteJSON(w, status, v) }

// routes 注册全部 HTTP 路由（v0.24.0 从 main() 抽出）。
//
// 为什么必须抽出来：此前这 ~110 条路由写死在 main() 里，而 main() 在测试中不可调用 ——
// 于是**没有任何测试能构造出真实 mux**，adminGuard / userGuard 这些鉴权中间件从未被
// 真正穿过（既有测试只能直接调 a.handleX，把中间件整段绕过）。鉴权是安全边界，
// 必须有测试覆盖；抽成方法正是那一步的前置条件（见 authz_test.go）。
func (a *app) routes() *http.ServeMux {
	mux := http.NewServeMux()
	// /healthz：**存活探针**，刻意保持公开 —— 隧道 / 负载均衡 / 监控要先能问一句
	// 「进程活着吗」，给它挂鉴权等于把探活也一起挡掉。代价是它必须**只回最小信息**，
	// 于是 version/time 只在已鉴权时附带（v0.24.2，P3-11，见 handleHealth）。
	mux.HandleFunc("GET /healthz", a.handleHealth)
	// Prometheus 抓取端点：仅指标，不含凭证/uid/会话键 —— 但**含**用户数、用量与
	// 号池规模聚合，且默认需要管理员权限（v0.24.2，P3-11；-metrics-public 可放开）。
	mux.HandleFunc("GET /metrics", a.metricsGuard(a.handleMetrics))
	mux.Handle("/", a.rootHandler())

	// 认证
	mux.HandleFunc("POST /api/auth/register", a.handleRegister)
	mux.HandleFunc("POST /api/auth/login", a.handleLogin)
	mux.HandleFunc("POST /api/auth/admin/login", a.handleAdminLogin)
	mux.HandleFunc("POST /api/auth/logout", a.handleLogout)
	mux.HandleFunc("GET /api/auth/me", a.handleMe)
	// 改密码（v0.17.8）：登录用户改自己的密码（校验原密码 + 强度，注销其它会话）。
	mux.HandleFunc("POST /api/auth/password", a.userGuard(a.handleChangePassword))

	// 用户自助：Key / 账单 / 兑换
	mux.HandleFunc("GET /api/keys", a.userGuard(a.handleListKeys))
	mux.HandleFunc("POST /api/keys", a.userGuard(a.handleCreateKey))
	// 显式取回完整密钥（v0.17.8）：列表只回掩码，明文单独一次、本人限定。
	mux.HandleFunc("GET /api/keys/{id}/reveal", a.userGuard(a.handleRevealKey))
	mux.HandleFunc("DELETE /api/keys/{id}", a.userGuard(a.handleDeleteKey))
	mux.HandleFunc("POST /api/keys/{id}/disable", a.userGuard(a.handleToggleKey(false)))
	mux.HandleFunc("POST /api/keys/{id}/enable", a.userGuard(a.handleToggleKey(true)))

	// 本地出口代理「一键拉起」（v0.31.17）。
	//
	// agent 两个端点用 **专用令牌**（X-Agent-Token）而不是 userGuard/adminGuard：
	// agent 跑在用户家里的电脑上、没有浏览器会话；给它一把只够 poll/report 的
	// 钥匙，比把管理密钥抄到本机安全得多（它能做的只有"拉代理"，读不到任何号）。
	mux.HandleFunc("GET /api/agent/poll", a.handleEgressAgentPoll)
	mux.HandleFunc("POST /api/agent/report", a.handleEgressAgentReport)
	// agent 自举（v0.31.25）：首次启动时凭令牌取回服务地址与自己的受限私钥，
	// 于是"一键安装"的脚本里**不需要包含任何密钥材料**。
	mux.HandleFunc("GET /api/agent/config", a.handleEgressAgentConfig)
	// agent 自更新（v0.31.42）：版本清单 + 产物下载。同样只认 agent 令牌 ——
	// 不让公网探测版本，也不让 agent 从第三方地址装可执行文件。
	mux.HandleFunc("GET /api/agent/version", a.handleAgentVersion)
	mux.HandleFunc("GET /api/agent/download", a.handleAgentDownload)
	// 出口控制台（v0.31.53）：**仅平台直属**（管理员会话 / 运维密钥）。
	//
	// 用户端自助代理已整体下线：普通用户既没有入口，也没有接口权限 ——
	// 五个端点全部在守卫层拒绝（403 platform_only），而不是靠前端藏按钮。
	// 账号出口从此**统一由管理后台分配**（/api/admin/egress/*），
	// 业务范围见 docs/用户端代理下线-20261019.md。
	mux.HandleFunc("GET /api/egress/home", a.adminGuard(a.handleEgressHome))
	mux.HandleFunc("POST /api/egress/home/pull", a.egressPlatformGuard(a.handleEgressHomePull))
	mux.HandleFunc("POST /api/egress/home/release", a.egressPlatformGuard(a.handleEgressHomeRelease))
	mux.HandleFunc("POST /api/egress/home/apply", a.egressPlatformGuard(a.handleEgressHomeApply))
	// 出口体检与纠偏（v0.31.31）：端口冲突 / 隧道没落到本机 → 换端口重建。
	// 只碰 ports 与 cmd 两个字段，不动账号、线路与全局默认。
	mux.HandleFunc("POST /api/egress/home/repair", a.egressPlatformGuard(a.handleEgressRepair))
	// 一键安装（v0.31.53）：安装包是**平台物料**（里面是平台 agent 令牌），
	// 只在管理端生成，普通用户不再拥有这条链路。
	mux.HandleFunc("GET /api/egress/home/install", a.adminGuard(a.handleEgressInstall))
	// agent / wbproxy 可执行文件（令牌鉴权，避免变成公开的 15MB 下载位）。
	mux.HandleFunc("GET /api/egress/agent/bundle", a.handleEgressAgentBundle)

	// 插件中心（v0.31.16，用户侧）：只下发 audience 含 user 的插件。
	// 执行接口内部按 owner_user_id 收窄 —— 用户只能领**自己的**代挂账号。
	mux.HandleFunc("GET /api/plugins", a.userGuard(a.handleUserPluginList))
	// 用户级启停（v0.31.22）：只写"我这个用户"的偏好，动不了平台开关。
	mux.HandleFunc("POST /api/plugins/{id}/enabled", a.userGuard(a.handlePluginUserToggle))
	mux.HandleFunc("POST /api/plugins/{id}/claim",
		a.userGuard(func(w http.ResponseWriter, r *http.Request) { a.handlePluginClaim(w, r, false) }))
	mux.HandleFunc("POST /api/keys/{id}/accounts", a.userGuard(a.handleSetKeyAccounts))
	mux.HandleFunc("GET /api/usage", a.userGuard(a.handleUsage))
	mux.HandleFunc("GET /api/usage/report", a.userGuard(a.handleUsageReport))
	mux.HandleFunc("GET /api/usage/breakdown", a.userGuard(a.handleUsageBreakdown))
	mux.HandleFunc("GET /api/me/usage-split", a.userGuard(a.handleMyUsageSplit))
	// 消耗排行（v0.17.8）：控制台概览高频刷新榜单，故与 /api/usage 分开。
	mux.HandleFunc("GET /api/usage/ranking", a.userGuard(a.handleUserUsageRanking))
	mux.HandleFunc("GET /api/models", a.userGuard(a.handleUserModels))
	// 模型倍率（v0.31.51，需求一）：用户侧只读，管理端含变化台账与手动同步。
	mux.HandleFunc("GET /api/models/ratios", a.userGuard(a.handleModelRatios))
	// 模型冷却（v0.31.55）：用户端只给**模型级结论**（能不能用 / 为什么 / 多久恢复），
	// 不含账号数也不含 uid —— 号池结构不出这个接口。
	mux.HandleFunc("GET /api/models/cooling", a.userGuard(a.handleModelCooling))
	mux.HandleFunc("GET /api/admin/model-ratios", a.adminGuard(a.handleAdminModelRatios))
	mux.HandleFunc("POST /api/admin/model-ratios/sync", a.adminGuard(a.handleAdminModelRatioSync))
	mux.HandleFunc("GET /api/referral", a.userGuard(a.handleReferral))
	// 站内公告（v0.17.8）：用户侧只读启用中的公告，用于弹窗与概览卡片。
	mux.HandleFunc("GET /api/announcements", a.userGuard(a.handleUserAnnouncements))
	mux.HandleFunc("POST /api/redeem", a.userGuard(a.handleRedeem))
	mux.HandleFunc("GET /api/wallet", a.userGuard(a.handleWallet))
	mux.HandleFunc("POST /api/wallet/withdraw", a.userGuard(a.handleWithdrawApply))

	// 用户代挂（hosting）：授权添加本人账号 / 代挂分组 / 申请入池 / 积分明细 / 收益
	mux.HandleFunc("POST /api/host/oauth/start", a.userGuard(a.handleHostOAuthStart))
	mux.HandleFunc("GET /api/host/oauth/poll", a.userGuard(a.handleHostOAuthPoll))
	mux.HandleFunc("GET /api/host/accounts", a.userGuard(a.handleHostAccounts))
	// 导出（v0.31.29）：只导出自己名下的号；文件里是凭证明文，前端须二次确认。
	mux.HandleFunc("GET /api/host/accounts/export", a.userGuard(a.handleExportHostAccounts))
	// 健康检测（v0.31.31，插件 health-check）：只查本人号，且 allowDelete=false。
	// 路由放在 /accounts/{uid}/... 之前无冲突（静态段与通配段分开注册）。
	// 用户端导入账号 JSON（v0.31.32）：重新登录的另一条路（不必扫码）。
	// 只写自己名下的号：别人的号跳过并说明（见 host_import.go 文件头）。
	mux.HandleFunc("POST /api/host/accounts/import", a.userGuard(a.handleHostImportAccounts))
	mux.HandleFunc("POST /api/host/accounts/health", a.userGuard(a.handleHostHealthCheck))
	mux.HandleFunc("GET /api/host/accounts/health/last", a.userGuard(a.handleHostHealthLast))
	mux.HandleFunc("POST /api/host/accounts/{uid}/group", a.userGuard(a.handleHostSetGroup))
	mux.HandleFunc("POST /api/host/accounts/{uid}/pool", a.userGuard(a.handleHostPool))
	mux.HandleFunc("DELETE /api/host/accounts/{uid}", a.userGuard(a.handleHostDeleteAccount))
	mux.HandleFunc("GET /api/host/accounts/{uid}/resources", a.userGuard(a.handleHostResources))
	mux.HandleFunc("GET /api/host/accounts/{uid}/task-status", a.userGuard(a.handleHostAccountTaskStatus))
	// 用户端代理（v0.31.53 整体下线）：这四个端点保留注册，但**一律 410**。
	//
	// 为什么不直接删路由：直接删会回 404，而 404 与"路径写错"完全无法区分 ——
	// 还在运行的老前端、用户自己的脚本会以为是地址问题，反复重试、报"接口不存在"。
	// 410 附带 code 与明确出口，一眼就知道"这个能力没有了、该找谁"。
	// 业务实现已经删干净（host_egress_lines.go 不再存在），这里只是**停止位**。
	for _, p := range []string{
		"POST /api/host/egress",
		"GET /api/host/egress/lines",
		"POST /api/host/egress/assign",
		"POST /api/host/egress/rebalance",
	} {
		mux.HandleFunc(p, a.userGuard(a.handleUserEgressRetired))
	}
	mux.HandleFunc("GET /api/system/info", a.userGuard(a.handleSystemInfo))
	mux.HandleFunc("GET /api/host/earnings", a.userGuard(a.handleHostEarnings))
	mux.HandleFunc("POST /api/host/accounts/{uid}/task", a.userGuard(a.handleHostSetTask))
	mux.HandleFunc("POST /api/host/task-all", a.userGuard(a.handleHostSetTaskAll))

	// 账号市场（用户侧，v0.19.0）：在售列表 + 买断。
	mux.HandleFunc("GET /api/sale/accounts", a.userGuard(a.handleSaleList))
	mux.HandleFunc("POST /api/sale/accounts/{uid}/buy", a.userGuard(a.handleBuyAccount))
	// 用户上架 / 下架自己的账号（v0.20.0）。
	mux.HandleFunc("POST /api/sale/accounts/{uid}/list", a.userGuard(a.handleUserAccountSale))
	// 成长任务（v0.17.1）：仅限本人代挂账号 —— 看进度 + 选号手动执行。
	mux.HandleFunc("GET /api/host/growth/overview", a.userGuard(a.handleHostGrowthOverview))
	mux.HandleFunc("POST /api/host/growth/progress", a.userGuard(a.handleHostGrowthProgress))
	mux.HandleFunc("POST /api/host/growth/run", a.userGuard(a.handleHostGrowthRun))

	// 管理后台：账号池 / 用户 / 兑换码 / 设置
	mux.HandleFunc("GET /api/admin/status", a.adminGuard(a.handleStatus))
	// 账号来源健康度（v0.31.37）：把"扫码登录是不是更容易掉线"变成可计算的数字。
	// 只读聚合，零出站（见 source_stats.go 的口径说明）。
	mux.HandleFunc("GET /api/admin/accounts/source-stats", a.adminGuard(a.handleAccountSourceStats))
	// 选号轨迹诊断（v0.17.5）：按会话聚合流水，列出「被多个账号服务过」的会话 ——
	// 直接回答「同一场对话是否在调用过程中换号」。只读，不参与运行时决策。
	mux.HandleFunc("GET /api/admin/selection-trace", a.adminGuard(a.handleSelectionTrace))
	// 选号可观测性（R7/R8，v0.26.1）：均衡度指标 + 候选队列预览（为什么排这 / 为什么跳过）。
	mux.HandleFunc("GET /api/admin/selection-balance", a.adminGuard(a.handleSelectionBalance))
	mux.HandleFunc("GET /api/admin/selection-candidates", a.adminGuard(a.handleSelectionCandidates))
	mux.HandleFunc("GET /api/admin/accounts", a.adminGuard(a.handleListAccounts))
	// 账号导出（v0.31.29）：`?uids=` 只导勾选的，不带则全量。格式与导入闭环。
	mux.HandleFunc("GET /api/admin/accounts/export", a.adminGuard(a.handleExportAccounts))
	mux.HandleFunc("GET /api/admin/pool", a.adminGuard(a.handlePool))
	mux.HandleFunc("POST /api/admin/accounts/sync", a.adminGuard(a.handleSync))
	// 桌面端账号导入（v0.12.0，需求 4）：把 ~/.wb-switch/accounts.json 里的已授权账号合并进号池。
	mux.HandleFunc("POST /api/admin/accounts/import-desktop", a.adminGuard(a.handleImportDesktopAccounts))
	// 导入防关联门禁（v0.25.0）：查当前出口 IP / 换 IP。见 import_guard.go。
	mux.HandleFunc("POST /api/admin/import/ip-status", a.adminGuard(a.handleImportIPStatus))
	mux.HandleFunc("POST /api/admin/import/ip-reset", a.adminGuard(a.handleImportIPReset))
	// 号池余额拉取（v0.17.8）：导入后自动跑，也可手动触发（候选 = 池内余额为 0 的号）。
	mux.HandleFunc("POST /api/admin/credits/refresh", a.adminGuard(a.handleRefreshCredits))
	mux.HandleFunc("DELETE /api/admin/accounts/{uid}", a.adminGuard(a.handleDeleteAccount))
	// 批量删除（v0.24.5）：管理后台「删除选中」与健康页「删除对话探活失败的全部」共用。
	mux.HandleFunc("POST /api/admin/accounts/batch-delete", a.adminGuard(a.handleBatchDeleteAccounts))
	// 风控上报台账（v0.24.6）：上游判定账号受限时的记录，健康检测页展示。
	mux.HandleFunc("GET /api/admin/risk/reports", a.adminGuard(a.handleRiskReports))
	mux.HandleFunc("POST /api/admin/accounts/{uid}/disable", a.adminGuard(a.handleDisable(true)))
	mux.HandleFunc("POST /api/admin/accounts/{uid}/enable", a.adminGuard(a.handleDisable(false)))
	mux.HandleFunc("POST /api/admin/oauth/start", a.adminGuard(a.handleOAuthStart))
	mux.HandleFunc("GET /api/admin/oauth/poll", a.adminGuard(a.handleOAuthPoll))
	mux.HandleFunc("GET /api/admin/key", a.adminGuard(a.handleGetKey))
	mux.HandleFunc("GET /api/admin/users", a.adminGuard(a.handleAdminUsers))
	mux.HandleFunc("GET /api/admin/usage", a.adminGuard(a.handleAdminUsage))
	// 用户消耗排行（v0.17.8）：报表页可单独高频刷新榜单，不必重算整份报表。
	mux.HandleFunc("GET /api/admin/usage/ranking", a.adminGuard(a.handleAdminUsageRanking))
	// 只读排查出口：某账号的 growth 任务原始形状（不解析，直接透出上游 data 段）。
	mux.HandleFunc("GET /api/admin/accounts/{uid}/growth-tasks", a.adminGuard(a.handleGrowthTasks))
	mux.HandleFunc("POST /api/admin/accounts/{uid}/growth-tasks/accept", a.adminGuard(a.handleGrowthTaskAccept))
	mux.HandleFunc("POST /api/admin/users/{id}/balance", a.adminGuard(a.handleAdminSetBalance))
	// 余额变动审计（v0.27.0）：谁在什么时候把谁的钱改了多少、结果如何。
	mux.HandleFunc("GET /api/admin/balance-audits", a.adminGuard(a.handleAdminBalanceAudits))
	// 计费倍率：变更历史（后台，含调整人）与用户侧只读视图（不含管理员信息）。
	mux.HandleFunc("GET /api/admin/multiplier-history", a.adminGuard(a.handleAdminMultiplierHistory))
	mux.HandleFunc("GET /api/multiplier", a.userGuard(a.handleMultiplier))
	mux.HandleFunc("POST /api/admin/users/{id}/status", a.adminGuard(a.handleAdminSetUserStatus))
	mux.HandleFunc("POST /api/admin/users/{id}/limits", a.adminGuard(a.handleAdminSetLimits))
	// 逐用户返点比例（v0.17.7）：查看 / 编辑该用户的号池分成与推广返点比例。
	// 覆盖值优先于全局配置；-1 或 null 表示清除覆盖回到跟随全局。改动只影响之后的入账。
	mux.HandleFunc("GET /api/admin/users/{id}/ratios", a.adminGuard(a.handleAdminGetUserRatios))
	mux.HandleFunc("POST /api/admin/users/{id}/ratios", a.adminGuard(a.handleAdminSetUserRatios))
	mux.HandleFunc("GET /api/admin/codes", a.adminGuard(a.handleAdminListCodes))
	mux.HandleFunc("POST /api/admin/codes", a.adminGuard(a.handleAdminGenCodes))
	mux.HandleFunc("GET /api/admin/settings", a.adminGuard(a.handleAdminSettings))
	mux.HandleFunc("POST /api/admin/settings", a.adminGuard(a.handleAdminSettings))

	// 插件中心（v0.31.16）：注册表驱动的可启停功能单元。
	// 两端各有入口（admin / user），**鉴权与数据范围各自独立** —— 插件不是权限模型。
	mux.HandleFunc("GET /api/admin/plugins", a.adminGuard(a.handleAdminPluginList))
	mux.HandleFunc("POST /api/admin/plugins/{id}/enabled",
		a.adminGuard(func(w http.ResponseWriter, r *http.Request) { a.handlePluginToggle(w, r) }))
	mux.HandleFunc("POST /api/admin/plugins/{id}/claim",
		a.adminGuard(func(w http.ResponseWriter, r *http.Request) { a.handlePluginClaim(w, r, true) }))

	// 版本更新提醒（v0.26.0）：读状态 + 跳过某版本。**只提醒，不自动更新**。
	mux.HandleFunc("GET /api/admin/update-status", a.adminGuard(a.handleAdminUpdateStatus))
	mux.HandleFunc("POST /api/admin/update-skip", a.adminGuard(a.handleAdminUpdateSkip))
	// 「应用更新」（v0.26.7）：生成替换脚本并回操作指引。**不自动替换、不自动重启**
	// —— 正在运行的 exe 无法被覆盖，任何自动替换都要靠外部进程在退出后动手，
	// 等于一次无人值守的重启；且清单与二进制同源、无法自证，最后一步必须由人确认。
	mux.HandleFunc("POST /api/admin/update-apply", a.adminGuard(a.handleAdminUpdateApply))

	// 站内公告管理（v0.17.8）：创建 / 编辑 / 启停 / 删除。
	mux.HandleFunc("GET /api/admin/announcements", a.adminGuard(a.handleAdminListAnnouncements))
	mux.HandleFunc("POST /api/admin/announcements", a.adminGuard(a.handleAdminCreateAnnouncement))
	mux.HandleFunc("POST /api/admin/announcements/{id}", a.adminGuard(a.handleAdminUpdateAnnouncement))
	mux.HandleFunc("POST /api/admin/announcements/{id}/toggle", a.adminGuard(a.handleAdminToggleAnnouncement))
	mux.HandleFunc("DELETE /api/admin/announcements/{id}", a.adminGuard(a.handleAdminDeleteAnnouncement))

	// 定时任务：台账查看 + 手动触发
	mux.HandleFunc("GET /api/admin/tasks", a.adminGuard(a.handleTasks))
	mux.HandleFunc("POST /api/admin/tasks/{kind}/run", a.adminGuard(a.handleTaskRun))

	// 实时运行态势：逐分钟序列 + 号池快照 + 告警 + 最近事件（实时仪表盘用）。
	mux.HandleFunc("GET /api/admin/live", a.adminGuard(a.handleLive))
	mux.HandleFunc("GET /api/admin/usage-split", a.adminGuard(a.handleAdminUsageSplit))
	// 号池仪表盘聚合（v0.17.3）：与 /live 同频轮询；服务端算好，零额外上游出站。
	mux.HandleFunc("GET /api/admin/pool-dashboard", a.adminGuard(a.handlePoolDashboard))

	// 定时任务配置（可改触发时刻/开关，保存后热重建调度器即时生效）。
	mux.HandleFunc("GET /api/admin/task-config", a.adminGuard(a.handleTaskConfig))
	mux.HandleFunc("POST /api/admin/task-config", a.adminGuard(a.handleTaskConfig))
	// 账号分组与专用出站代理（反代选号分类 + 防封）。
	mux.HandleFunc("POST /api/admin/accounts/{uid}/group", a.adminGuard(a.handleAccountGroup))
	mux.HandleFunc("POST /api/admin/accounts/{uid}/consume-group", a.adminGuard(a.handleSetConsumeGroup))
	// 成长任务（不排程，手动选号执行 + 进度查看，v0.17.1）。
	// overview 只回本地信息与缓存进度（不打上游）；progress 才真去查。
	mux.HandleFunc("GET /api/admin/growth/overview", a.adminGuard(a.handleAdminGrowthOverview))
	mux.HandleFunc("POST /api/admin/growth/progress", a.adminGuard(a.handleAdminGrowthProgress))
	mux.HandleFunc("POST /api/admin/growth/run", a.adminGuard(a.handleGrowthRun))
	mux.HandleFunc("POST /api/admin/accounts/consume-group/batch", a.adminGuard(a.handleBatchConsumeGroup))
	mux.HandleFunc("POST /api/admin/accounts/{uid}/egress", a.adminGuard(a.handleAccountEgress))

	// ---- 设备风控凭证（X-Device-Token，R2 / v0.26.1）----
	// 每号写入通道 + 「共用一个即关联」审计 + 批量粘贴导入。
	// 为什么值得单开一组：这是本仓库**唯一有逆向证据**的风控头，而此前它没有任何写入口，
	// 全池长期为空（上游看到的是一台「无设备凭证的客户端」）。
	mux.HandleFunc("POST /api/admin/accounts/{uid}/device-token", a.adminGuard(a.handleAccountDeviceToken))
	mux.HandleFunc("GET /api/admin/device-tokens", a.adminGuard(a.handleDeviceTokenAudit))

	// 指纹被拦样本回流（R4 / v0.26.1）：默认关闭，开启后落 system 原文前 300 字 + 工具清单。
	mux.HandleFunc("GET /api/admin/fingerprint-samples", a.adminGuard(a.handleFingerprintSamples))
	mux.HandleFunc("DELETE /api/admin/fingerprint-samples", a.adminGuard(a.handleFingerprintSamplesClear))

	// 出站指纹规则（R4，v0.28.0）：把"被拦样本"变成"可上线规则"，无需发版。
	// 与上面采样面板配套 —— 采集（样本）+ 处置（规则）合起来才是闭环。
	//
	// 实现已于 v0.31.12 下沉 `internal/api/fingerprint`（P2-7 第四刀第一刀）：
	// 路由**注册**留在这里（应用级装配），handler 的**实现**在新包。
	// 样本读取（文件 IO、路径可被 flag 改）作为依赖注入 —— 新包不认识文件系统。
	fpRules := fingerprint.New(a.store, fingerprintSampleTexts)
	mux.HandleFunc("GET /api/admin/fingerprint-rules", a.adminGuard(fpRules.HandleRules))
	mux.HandleFunc("POST /api/admin/fingerprint-rules", a.adminGuard(fpRules.HandleRuleUpsert))
	mux.HandleFunc("DELETE /api/admin/fingerprint-rules", a.adminGuard(fpRules.HandleRuleDelete))
	// 试跑（保存前预演，不动全局规则）与样本回放（用真实被拦样本验证规则有效性）。
	mux.HandleFunc("POST /api/admin/fingerprint-rules/test", a.adminGuard(fpRules.HandleRuleTest))
	mux.HandleFunc("POST /api/admin/fingerprint-rules/replay", a.adminGuard(fpRules.HandleRuleReplay))

	// 消耗失败台账（v0.26.3，见 failures.go）：统计 + 明细 + 错误码处置策略表。
	mux.HandleFunc("GET /api/admin/consume-failures", a.adminGuard(a.handleConsumeFailures))
	mux.HandleFunc("POST /api/admin/device-tokens/bulk", a.adminGuard(a.handleDeviceTokenBulk))

	// ---- 出口代理池（v0.14.0 防封）----
	// 代理清单 + 批量分配 + 出口聚合看板 + 探活。
	mux.HandleFunc("GET /api/admin/egress/proxies", a.adminGuard(a.handleEgressProxies))
	mux.HandleFunc("POST /api/admin/egress/proxies", a.adminGuard(a.handleEgressProxies))
	mux.HandleFunc("DELETE /api/admin/egress/proxies", a.adminGuard(a.handleEgressProxies))
	mux.HandleFunc("POST /api/admin/egress/check", a.adminGuard(a.handleEgressCheck))
	// 出口运维（v0.24.6）：坏线路上的账号自动迁移 + 候选地址批量探活导入。
	mux.HandleFunc("POST /api/admin/egress/recover", a.adminGuard(a.handleEgressRecover))
	mux.HandleFunc("POST /api/admin/egress/candidates", a.adminGuard(a.handleEgressCandidates))
	mux.HandleFunc("POST /api/admin/egress/assign", a.adminGuard(a.handleEgressAssign))
	// 出口范围（v0.31.50，需求二）：人工停用 / 越界存量 / 强制越界审计。
	// 三个都是**只读或单点写**，不加额外权限层：adminGuard 已是最高权威。
	mux.HandleFunc("POST /api/admin/egress/proxies/manual-disable", a.adminGuard(a.handleEgressLineManualDisable))
	mux.HandleFunc("GET /api/admin/egress/compliance", a.adminGuard(a.handleEgressCompliance))
	mux.HandleFunc("GET /api/admin/egress/force-audits", a.adminGuard(a.handleEgressForceAudits))
	// 全局默认出口修复（v0.31.26）：护栏管不了历史配置，历史配置需要一键改回。
	mux.HandleFunc("POST /api/admin/egress/default/repair", a.adminGuard(a.handleEgressDefaultRepair))
	// 账号出口 ↔ 线路池对齐（v0.31.33）：补登记池外的账号出口、合并同 URL 不同写法。
	mux.HandleFunc("POST /api/admin/egress/converge", a.adminGuard(a.handleEgressConverge))
	// 出站口径（v0.31.35）：统一走服务器 / 撤回。GET 看快照，POST 切换（带 dry_run 与备份）。
	mux.HandleFunc("GET /api/admin/egress/policy", a.adminGuard(a.handleEgressPolicy))
	mux.HandleFunc("POST /api/admin/egress/policy", a.adminGuard(a.handleEgressPolicy))
	mux.HandleFunc("GET /api/admin/egress/overview", a.adminGuard(a.handleEgressOverview))
	// 账号代理视图（v0.17.9）：逐账号的出口配置与实际出口（只读本地，不出站）。
	mux.HandleFunc("GET /api/admin/egress/accounts", a.adminGuard(a.handleEgressAccounts))
	// 逐套餐积分明细（账号卡片的「N 个积分包 + 到期进度条」用）。
	mux.HandleFunc("GET /api/admin/accounts/{uid}/resources", a.adminGuard(a.handleAccountResources))
	// 明细缓存预热（v0.24.9）：串行补齐整页卡片的积分明细，见 creditcache.go。
	// 路径是字面量而非 {uid}，与 consume-group/batch 同一做法（更具体的模式优先）。
	mux.HandleFunc("POST /api/admin/accounts/credit-warmup", a.adminGuard(a.handleAdminCreditWarmup))
	// 单号刷新（v0.25.0）：前端在页面可见时按间隔逐个调用，实现"卡片实时"补新。
	mux.HandleFunc("POST /api/admin/accounts/credit-refresh", a.adminGuard(a.handleCreditRefreshOne))
	// 号池账号健康检测（v0.23.0）：单个同步 + 批量异步 + 取最近一次批量结果。
	// 路由顺序注意：`/accounts/health` 是**静态段**，必须注册在 `/accounts/{uid}/...`
	// 之前吗？—— 不必：Go 1.22+ 的 ServeMux 按「更具体的模式优先」匹配，静态段
	// 天然优先于通配段，故顺序无关（此处仍聚在一起便于阅读）。
	mux.HandleFunc("POST /api/admin/accounts/health", a.adminGuard(a.handleCheckHealthBatch))
	// 探活复活（v0.31.39）：给被判失效的号一条可验证的复活路。
	// 批量端点默认 dry_run（只列候选、零出站），真正执行才逐号做对话探活。
	// 路径注意：`/accounts/revive-dead` 是静态段，必须注册在 `/accounts/{uid}/...` 之前。
	mux.HandleFunc("POST /api/admin/accounts/revive-dead", a.adminGuard(a.handleReviveDead))
	// 最近被拒的请求（v0.31.45）：解决"用户报 403、服务端查不到"的最后一环。
	mux.HandleFunc("GET /api/admin/auth-rejects", a.adminGuard(a.handleAuthRejects))
	mux.HandleFunc("POST /api/admin/accounts/{uid}/revive", a.adminGuard(a.handleReviveAccount))
	// 刷新模式切换（v0.31.40）：冲突自动降级后的**反向开关**。
	mux.HandleFunc("POST /api/admin/accounts/{uid}/refresh-mode", a.adminGuard(a.handleSetRefreshMode))
	mux.HandleFunc("GET /api/admin/accounts/health/last", a.adminGuard(a.handleHealthLast))
	mux.HandleFunc("POST /api/admin/accounts/{uid}/health", a.adminGuard(a.handleCheckAccountHealth))
	// 账号卡片「任务完成情况」（只读：今日签到 + 猫猫旅行状态）。
	mux.HandleFunc("GET /api/admin/accounts/{uid}/task-status", a.adminGuard(a.handleAccountTaskStatus))
	// 用户代挂：全部代挂账号 + 待审队列 + 审批（管理后台统一展示代挂分组）。
	mux.HandleFunc("GET /api/admin/host/accounts", a.adminGuard(a.handleAdminHostAccounts))
	mux.HandleFunc("POST /api/admin/host/accounts/{uid}/review", a.adminGuard(a.handleAdminHostReview))

	// 用户详情（v0.21.0）：管理后台用户面板的「查看详情」。
	mux.HandleFunc("GET /api/admin/users/{id}", a.adminGuard(a.handleAdminUserDetail))

	// 账号上架 / 买断（v0.19.0）
	// 管理端：上架 / 下架指定账号 + 市场台账（在售、可上架候选、买断记录）。
	mux.HandleFunc("POST /api/admin/accounts/{uid}/sale", a.adminGuard(a.handleAdminAccountSale))
	mux.HandleFunc("GET /api/admin/sales", a.adminGuard(a.handleAdminSales))

	// 提现审核（v0.11.0）。
	mux.HandleFunc("GET /api/admin/withdrawals", a.adminGuard(a.handleAdminWithdrawals))
	mux.HandleFunc("POST /api/admin/withdrawals/{id}/review", a.adminGuard(a.handleAdminWithdrawReview))
	// 账号池：逐账号定时任务开关（v0.11.0）。
	mux.HandleFunc("POST /api/admin/accounts/{uid}/task", a.adminGuard(a.handleAdminSetTask))

	// OpenAI 兼容层：用户 API Key 鉴权（多租户核心）
	// liveCapture 必须在**最外层**（v0.30.0）：它负责把"被前置环节拒绝"的请求
	// （鉴权失败 / 密钥停用 / 账号封禁 / 余额不足 / 配额超限 / 限流 / 请求体非法）
	// 也补记进实时面板 —— 否则曲线与事件流只反映走到上游的请求，
	// 呈现"只有成功、没有失败"的假象（详见 live.go 的说明）。
	// ── 旧网关对外面的兼容层（v0.31.4 落地；旧网关实现已于 v0.31.5 删除，契约保留）──
	// 两个产品合并的第一步：让老命令与老脚本**一行不改**也能打 webapp。
	// 形状、键名、状态码一律照旧网关（见 compat.go 文件头），鉴权用同一个管理密钥。
	mux.HandleFunc("GET /status", a.compatGuard(a.handleCompatStatus))
	mux.HandleFunc("POST /v1/stats/reset", a.compatGuard(a.handleCompatStatsReset))
	mux.HandleFunc("POST /admin/accounts/{uid}/disable", a.compatGuard(a.handleCompatAccountDisable))
	mux.HandleFunc("POST /admin/accounts/{uid}/enable", a.compatGuard(a.handleCompatAccountEnable))
	mux.HandleFunc("POST /admin/accounts/{uid}/revive", a.compatGuard(a.handleCompatAccountRevive))
	mux.HandleFunc("POST /admin/tasks/{name}/run", a.compatGuard(a.handleCompatTaskRun))
	mux.HandleFunc("POST /v1/chat/completions", a.liveCapture(a.apiKeyGuard(a.proxy.ServeChat)))
	// /v1/models 是客户端接入时的第一个探测请求，且不产生消费 —— 故豁免余额校验，
	// 否则余额为 0 的用户在客户端里只会看到「连接失败」（详见 apiKeyGuardNoBalance）。
	mux.HandleFunc("GET /v1/models", a.apiKeyGuardNoBalance(a.handleModels))
	// 不带 /v1 的别名（v0.31.49）：不少客户端把 base_url 填成裸域名
	// （`http://host:8780`），SDK 会自己补 /chat/completions 与 /models。
	// 此前这两条路径没有注册，落到 `GET /` 兜底上 → 用户看到的是
	// `405 Method Not Allowed (target: http://host:8780)`，一句都指不到真正原因。
	// 同一个处理器、同一套鉴权，只是多认一个入口。
	mux.HandleFunc("POST /chat/completions", a.liveCapture(a.apiKeyGuard(a.proxy.ServeChat)))
	mux.HandleFunc("GET /models", a.apiKeyGuardNoBalance(a.handleModels))

	return mux
}
