// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 插件中心（v0.31.16）。
//
// 存在的理由：功能会越加越多，而**侧导航不能无限膨胀**。把「可独立启停的功能单元」
// 抽成插件之后：
//   - 导航结构固定（两端各一个「插件」入口），加插件只改下面的注册表，
//     不必再动 admin.html / dashboard.html 的 nav 与 pane；
//   - 运维可以**停用**某个插件（存 kv；停用 = 列表里标注 + 执行接口直接拒绝）；
//   - 同一个插件能同时出现在**管理后台**与**用户控制台**（Audience 决定），
//     但两端各自走自己的鉴权与数据范围。
//
// ⚠️ 边界（刻意的，别越）：
//   - 插件是**功能的组织方式，不是权限模型**。平台侧一律过 adminGuard，
//     用户侧一律过 userGuard 且只能碰自己的号。插件不允许绕过鉴权。
//   - 停用是「拒绝执行 + 前端不渲染入口」，**不是**数据迁移或状态清理 ——
//     停用一个只读/写类插件不该改动任何业务状态。
package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/upstream"
)

// pluginDef 一个插件的定义（注册表条目）。
//
// 注册表是**编译期只读**的：插件对应真实代码（一个执行分支），不是动态加载的
// 沙箱脚本 —— 「能装第三方插件」意味着「能执行任意代码」，那是另一个量级的
// 安全承诺，本仓不做（真要做得先解决沙箱与签名，不是加个列表就完事）。
type pluginDef struct {
	ID   string // 稳定标识（前端以此调用执行接口；改它等于破坏契约）
	Name string
	Desc string
	// Audience 可见范围：admin（仅管理后台）/ user（仅控制台）/ both（两端）。
	Audience string
	// Builtin 内置插件（随二进制分发）。保留该字段是为了列表里的分组展示，
	// 目前恒 true。
	Builtin bool
	// Kind 插件的**面板形态**（v0.31.31）：决定前端展开后渲染哪一种面板。
	//
	//	"claim"   选号 → 逐个串行执行 → 逐行结果（领取类插件）
	//	"health"  选号 → 发起检测 → 展示每号的检测结论（健康检测）
	//	"egress"  打开即进入**独立界面**（代理 / 本地出口）：它没有"一次执行"，
	//	          而是一个持续操作的控制台。前端据此把这类的「打开」渲染成整页视图，
	//	          而不是塞进卡片里的小面板 —— 隧道状态、逐号出口、机器列表这些
	//	          放在 400px 宽的卡片里根本没法用（v0.31.34 的现场反馈）。
	//
	// 为什么要显式区分而不是让前端按 id 猜：面板的交互与文案完全不同
	// （领取的语义是"执行一次写操作"，检测的语义是"读一次状态"）。
	// 让前端按 id 硬编码分支，加第二个同类插件时就得改前端 —— 那就白抽插件层了。
	Kind string
}

// pluginRegistry 插件注册表。加插件 = 在这里加一条 + 在 handlePluginClaim 里
// 加一个执行分支（外加路由注册，若需要新 path 就仍走 {id} 通配）。
var pluginRegistry = []pluginDef{
	{
		ID:       pluginIDClaimCompensation,
		Name:     "多账号领取积分补偿",
		Desc:     "批量向上游领取「活动补偿」（有则领、无则静默）。补偿是幂等写：同一个号重复调用不会重复到账。",
		Audience: "both",
		Builtin:  true,
		// 显式声明形态：前端靠它选面板（空值会落到 default，行为一样，
		// 但"看起来像忘了填"的字段迟早被人改成别的默认值）。
		Kind: "claim",
	},
	{
		ID:   pluginIDHealthCheck,
		Name: "账号健康检测",
		Desc: "逐个检查你名下的代挂账号：凭证是否还有效、余额是否读得到、对话通道是否通、" +
			"出口是否正常。只读检查，**不会**停用/删除任何账号。",
		Audience: "both",
		Builtin:  true,
		Kind:     "health",
	},
	{
		ID:   pluginIDHostEgress,
		Name: "平台直属出口",
		Desc: "平台自己的出口机器：一键拉起本机代理 + SSH 反向隧道，查看隧道与出口状态、" +
			"体检纠偏、释放。**这是平台能力，仅管理员可见**。" +
			"用户端自助代理已下线 —— 账号出口一律在管理后台「代理」页分配。",
		// v0.31.53：从 user 改为 admin。
		//
		// 产品决定：出口代理收敛为平台能力，用户端不再具备任何代理使用能力。
		// 改 Audience 而不是删条目，是因为**管理端仍需要这个入口**去操作平台
		// 直属机器（拉起/纠偏/释放）—— 删掉它，管理员就只能靠 curl。
		// 注意：插件列表接口按 audience 过滤，普通用户拿到的列表里没有这一条。
		Audience: "admin",
		Builtin:  true,
		Kind:     "egress",
	},
}

// pluginIDHostEgress 「本地出口代理」插件的 id（v0.31.34）。
//
// 为什么把它从**侧导航**挪进插件：
//
//	· 它不是一个"页面功能"，而是一套**持续运行的基础设施**（下载安装包、拉起隧道、
//	  维持心跳、逐号分配出口）。放在一级导航里，用户会以为它是"用一次就走"的功能；
//	· 侧导航已有 13 项，再往上加只会让"常用功能"被稀释；
//	· 插件化之后它可以被用户自己停用（不想要这条链路的人不必一直看到它），
//	  也能被平台统一停用（例如某次上游风控期间暂时关闭自助接入）。
//
// 与其他插件的差别：它是**独立界面**（Kind=egress），没有"执行一次"的语义 ——
// 因此 `handlePluginClaim` 里对它返回 400（明确说清"没有执行入口"），
// 而不是让它掉进 default 分支报"未实现"。
const pluginIDHostEgress = "host-egress"

// pluginIDHealthCheck 健康检测插件的 id。
//
// 检测逻辑早就有了（管理后台「健康检测」页），这一版是把它**插件化**：
// 用户也能在控制台里对自己的号做体检，而不必请管理员代跑。
// 与后台的差别只有两条（都在 host_health.go 里写死）：
//   - 只检测本人名下的号；
//   - **不允许任何不可逆处置**（allowDelete=false）—— 用户的号是用户的资产。
const pluginIDHealthCheck = "health-check"

const pluginIDClaimCompensation = "claim-compensation"

// pluginEnabledKV 插件启停的存储前缀（kv 表）。**缺省即启用** ——
// 这样新增插件不需要迁移，也不会因为忘了写 kv 而"装上了却不生效"。
const pluginEnabledKV = "plugin_enabled_"

// pluginEnabled 插件当前是否启用（kv 无值 = 启用）。
func (a *app) pluginEnabled(id string) bool {
	v, err := a.store.GetKV(pluginEnabledKV + id)
	if err != nil {
		return true // 没配过 = 启用
	}
	return strings.TrimSpace(v) != "0"
}

// pluginUserOffKV 用户级「我不启用这个插件」的存储前缀（v0.31.22）。
//
// 为什么必须与全局开关分开（`plugin_enabled_<id>`）：
//
//	· 全局那位是**平台级决定** —— 这个能力该不该提供给用户，只有管理员能动；
//	· 「我自己的号要不要用这个插件」是**用户自己的事**。
//
// 合成一个键意味着任何一个用户点一下停用，全平台都跟着停 —— 权限与语义都不对。
//
// 键形如 `plugin_user_off_<userID>_<pluginID>`，值 "1" = 该用户停用。
// **缺省启用**，与全局开关同一口径（新增插件不需要迁移，也不会因为忘了写 kv
// 而"装上了却不生效"）。
const pluginUserOffKV = "plugin_user_off_"

func pluginUserOffKey(userID int64, id string) string {
	return pluginUserOffKV + strconv.FormatInt(userID, 10) + "_" + id
}

// pluginUserEnabled 该用户是否启用此插件（无记录 = 启用）。
func (a *app) pluginUserEnabled(userID int64, id string) bool {
	if userID <= 0 {
		return true
	}
	v, err := a.store.GetKV(pluginUserOffKey(userID, id))
	if err != nil {
		return true
	}
	return strings.TrimSpace(v) != "1"
}

// setPluginUserEnabled 写该用户的插件偏好。**只写 kv**，与平台级开关同纪律。
func (a *app) setPluginUserEnabled(userID int64, id string, on bool) error {
	v := "0"
	if !on {
		v = "1"
	}
	return a.store.SetKV(pluginUserOffKey(userID, id), v)
}

// pluginByID 按 ID 取注册项。
func pluginByID(id string) (pluginDef, bool) {
	for _, p := range pluginRegistry {
		if p.ID == id {
			return p, true
		}
	}
	return pluginDef{}, false
}

// pluginView 列表下发的一条（对外形状固定：前端只渲染，不判断）。
type pluginView struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Desc     string `json:"desc"`
	Audience string `json:"audience"`
	Builtin  bool   `json:"builtin"`
	// Kind 面板形态（见 pluginDef.Kind）。后端下发，前端只认它，不认 id。
	Kind string `json:"kind"`
	// Enabled **平台级**开关：该插件是否由管理员提供给本端使用。
	Enabled bool `json:"enabled"`
	// UserEnabled **当前用户**是否启用（v0.31.22）。管理端看的是平台级，
	// 故恒为 true（管理端没有"某个用户"这个上下文）。
	UserEnabled bool `json:"user_enabled"`
	// Active 实际生效 = 平台启用 && 用户启用。
	//
	// 为什么由服务端算好而不是让前端 `a && b`：这是**唯一**决定"能不能用"的口径，
	// 散在两个前端文件里各拼一遍，迟早有一边忘了 && 另一边。服务端算一次，
	// 前端只渲染。
	Active bool `json:"active"`
}

// audienceVisible 该插件对某个端是否可见。
func audienceVisible(def pluginDef, want string) bool {
	return def.Audience == "both" || def.Audience == want
}

// handlePluginList 插件列表。
//
// 管理端（wantAdmin=true）拿到 admin/both 的插件与**启停**信息；
// 用户端只拿到 user/both 的插件，且**不下发 audience/builtin/enabled 以外的
// 平台运维信息**（用户不需要知道平台还有哪些插件）。
func (a *app) pluginListView(wantAdmin bool, userID int64) []pluginView {
	out := make([]pluginView, 0, len(pluginRegistry))
	for _, def := range pluginRegistry {
		if wantAdmin {
			if !audienceVisible(def, "admin") {
				continue
			}
		} else if !audienceVisible(def, "user") {
			continue
		}
		globalOn := a.pluginEnabled(def.ID)
		// 管理端没有"某个用户"这个上下文 → user_enabled 恒 true（见 pluginView 注释）。
		userOn := true
		if !wantAdmin {
			userOn = a.pluginUserEnabled(userID, def.ID)
		}
		out = append(out, pluginView{
			ID: def.ID, Name: def.Name, Desc: def.Desc,
			Audience: def.Audience, Builtin: def.Builtin, Kind: def.Kind,
			Enabled: globalOn, UserEnabled: userOn, Active: globalOn && userOn,
		})
	}
	return out
}

// handleAdminPluginList GET /api/admin/plugins
func (a *app) handleAdminPluginList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"plugins": a.pluginListView(true, 0)})
}

// handleUserPluginList GET /api/plugins （控制台）
func (a *app) handleUserPluginList(w http.ResponseWriter, r *http.Request) {
	var uid int64
	if u := userFrom(r); u != nil {
		uid = u.ID
	}
	writeJSON(w, http.StatusOK, map[string]any{"plugins": a.pluginListView(false, uid)})
}

// handlePluginToggle POST /api/admin/plugins/{id}/enabled  body {"enabled":bool}
//
// 只写 kv，不碰任何业务状态：停用 = 「列表里不建议用 + 执行接口拒绝」。
// 这样停用一个插件绝不会顺手改坏别的东西（对比：把"停用"做成"清理数据"，
// 运维就再也不敢点了）。
func (a *app) handlePluginToggle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := pluginByID(id); !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "插件不存在"})
		return
	}
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Enabled == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误（需要 enabled 布尔）"})
		return
	}
	v := "0"
	if *req.Enabled {
		v = "1"
	}
	if err := a.store.SetKV(pluginEnabledKV+id, v); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	log.Printf("INFO: [plugins] %s 已%s（管理端）", id, map[bool]string{true: "启用", false: "停用"}[*req.Enabled])
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "enabled": *req.Enabled})
}

// handlePluginUserToggle POST /api/plugins/{id}/enabled  body {"enabled":bool}
//
// 用户级「我这边启用 / 停用这个插件」（v0.31.22）。
//
// 为什么要有它：插件中心给两端各一个入口，但此前**只有管理端有开关** ——
// 用户端卡片只能看不能动，"可安装 / 可不安装"这件事在用户侧根本不存在。
// 而平台级开关不能让用户动（一个用户点一下会把所有人的功能关掉），
// 所以这里落的是**按用户隔离**的偏好，与平台开关互不影响：
//
//	平台关 + 用户开 → 不可用（以平台为准）
//	平台关 + 用户关 → 不可用
//	平台开 + 用户关 → **仅该用户**不可用
//	平台开 + 用户开 → 可用
func (a *app) handlePluginUserToggle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	def, ok := pluginByID(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "插件不存在"})
		return
	}
	if !audienceVisible(def, "user") {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "该插件对本端不可用"})
		return
	}
	u := userFrom(r)
	if u == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "未登录"})
		return
	}
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Enabled == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误（需要 enabled 布尔）"})
		return
	}
	// 平台已停用时，用户点"启用"没有意义 —— 明确拒绝，而不是静默写一条永远不生效的
	// 偏好（那会让用户以为"我点了启用"，下次仍然用不了，且没有任何提示）。
	if *req.Enabled && !a.pluginEnabled(def.ID) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "该插件已被管理员停用，请先联系管理员"})
		return
	}
	if err := a.setPluginUserEnabled(u.ID, def.ID, *req.Enabled); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	log.Printf("INFO: [plugins] 用户 %s 已%s插件 %s", u.Username,
		map[bool]string{true: "启用", false: "停用"}[*req.Enabled], def.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "enabled": *req.Enabled, "user_enabled": *req.Enabled,
	})
}

// ---------------------------------------------------------------------------
// 执行：多账号领取积分补偿
// ---------------------------------------------------------------------------

// claimStatus 单号领取的结果分类（前端据此分色展示，不猜）。
const (
	claimOK      = "ok"      // 到账
	claimAlready = "already" // **已领取过**（上游明确答复"限领一次 / 已领取"）
	claimNone    = "none"    // 无可领（当前没有这个活动 / 上游没给可领的东西）
	claimSkip    = "skip"    // 不适用（国际版账号）
	claimError   = "error"   // 真失败（网络 / 鉴权 / 5xx）
)

// claimResult 单号结果。逐号如实回报 —— 这个插件的价值全在"哪几个号失败了"上。
type claimResult struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Status   string `json:"status"`
	Credit   int64  `json:"credit"`
	Message  string `json:"message,omitempty"`
	// Detail 上游 data 原文（截断）。**只在 credit==0 时填** ——
	// 这时"真的无可领"与"我们字段名猜错了"从 credit 上看一模一样，
	// 原文是唯一能区分的事实。
	Detail string `json:"detail,omitempty"`
	// Code 上游业务码（能从答复里解析出来时填，如 10001）。
	//
	// 为什么要把它单独拎出来（v0.31.19，用户要求"按补偿领取反馈区分"）：
	// "已领取过"与"当前无活动"此前共用一个 none 状态、一句笼统文案
	// （"已领过或当前无活动"）—— 于是用户看到"无可领"就来问"到底是哪个"，
	// 而这两个结论的下一步动作完全不同（前者不用再试，后者等有活动再试）。
	Code int `json:"code,omitempty"`
	// UpstreamMsg 上游 msg 的**原文**（未加工）。message 是我们对它的人话转述，
	// 转述可能错，原文不会 —— 这也是排障时唯一能确认"上游到底说了什么"的依据。
	UpstreamMsg string `json:"upstream_msg,omitempty"`
}

// claimCompensationOne 领取单个账号的活动补偿。
//
// 出站纪律（本仓最贵的一类教训）：
//   - 过 **pluginGate**：全局在途 ≤ probeMaxInflight、同号 30s 一次 ——
//     禁止"一键全并发"（59 号并发出站 → 上游风控，2026-10-13 事故形态）；
//   - 走 `uc.ClaimCompensation` → `billingJSON` → `doJSON` → `doAuth`，
//     即**走该账号自己的出口线路**（与 chat 同一条出口，不裸连）；
//   - **失败一律不处置账号**：不冷却、不熔断、不喂连败。领取失败说明不了
//     "这个号坏了"（补偿无了、上游抖动都会失败）—— 与项目纪律
//     「不可逆处置只给指向账号本身的证据」一致。
func (a *app) claimCompensationOne(uid string) claimResult {
	res := claimResult{UID: uid}
	au := a.proxy.pool.AuthByUID(uid)
	if au == nil {
		res.Status = claimError
		res.Message = "账号不在号池（已删除或未同步）"
		return res
	}
	res.Nickname = au.Nickname
	if au.IsGlobal() {
		// 国际版没有这条链路（凭据：scheduler.claimGrowthRewards 对 global 直接 return，
		// 注释记录了实测 global 的 /activity/growth/streak 返回 500）。盲试只会
		// 让运维看到一片红色错误，误以为插件坏了。
		res.Status = claimSkip
		res.Message = "国际版账号不适用"
		return res
	}
	release, err := a.pluginGate.acquire(uid)
	if err != nil {
		res.Status = claimError
		res.Message = err.Error()
		return res
	}
	defer release()

	credit, detail, err := a.proxy.uc.ClaimCompensationRaw(au)
	switch {
	case err == nil && credit > 0:
		res.Status = claimOK
		res.Credit = credit
	case err == nil:
		// 上游 200 + code=0，但**没有 credit**：实测这是"已领过"的另一种答复形态
		// （2026-10-16 真机：同一个池里，多数号回业务错误、个别号回 code=0 且 data 为空）。
		// 它与"真的无可领"是同一件事，故同样归 none —— 但原文照旧透出，
		// 万一将来上游改成 `{"data":{"points":N}}` 这种字段，这里能一眼看出来。
		res.Status = claimNone
		res.Message = "无到账积分（多为已领过）"
		res.Detail = detail
	default:
		text := claimErrText(err)
		res.Code, res.UpstreamMsg = claimBizReply(text)
		if isClaimNormalState(err) {
			// **再分一层**（v0.31.19）：把"已领取过"从"当前无活动"里摘出来。
			// 两者都是正常态、都不该报红，但给用户的结论不一样。
			if claimAlreadyReply(res.Code, res.UpstreamMsg) {
				res.Status = claimAlready
				res.Message = "已领取过"
				if res.UpstreamMsg != "" {
					res.Message = "已领取过（上游：" + res.UpstreamMsg + "）"
				}
			} else {
				res.Status = claimNone
				res.Message = "无可领补偿（当前无此活动）"
			}
			// 上游错答原文（含 code / msg）：排障时这是唯一能确认"上游到底说了什么"的
			// 依据 —— 上面的 message 是我对它的人话转述，转述可能错，原文不会。
			res.Detail = text
		} else {
			res.Status = claimError
			res.Message = text
		}
	}
	return res
}

// claimBizReply 从上游错误文本里抽出业务信封 `{"code":N,"msg":"…"}`。
//
// 文本形态是 `upstream client (http 400): {"code":10001,"msg":"…","requestId":"…"}`，
// 所以先定位第一个 `{`，再用 Decoder 只解**第一个** JSON 值（尾部还跟着
// requestId 等字段，`json.Unmarshal` 会因"多余内容"直接失败）。
// 解不出来就返回 0/""，调用方据此退回笼统文案 —— 绝不猜。
func claimBizReply(raw string) (int, string) {
	i := strings.Index(raw, "{")
	if i < 0 {
		return 0, ""
	}
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if json.NewDecoder(strings.NewReader(raw[i:])).Decode(&env) != nil {
		return 0, ""
	}
	return env.Code, strings.TrimSpace(env.Msg)
}

// claimAlreadyCodes 上游"已经领过"的业务码。**只放实测确认过的**。
//
// 10001 = 2026-10-16 真机：`{"code":10001,"msg":"每人限领一次，您已领取过无法重复领取"}`。
var claimAlreadyCodes = map[int]bool{10001: true}

// claimAlreadyWords 上游"已领过"的措辞（中文 / 英文双通道）。
//
// ⚠️ 这里按文案判定是**可接受的**，因为结论只用于**展示分类**（把"已领过"和
// "当前无活动"分开说清），不产生任何账号处置 —— 与「按文案给账号定罪」那条
// 红线不冲突。反过来说：**将来若要用它触发任何不可逆动作，必须改成只看业务码。**
var claimAlreadyWords = []string{
	"已领取", "已领过", "已经领取", "限领一次", "重复领取", "不可重复",
	"already claimed", "already received", "already got", "only once",
}

// claimAlreadyReply 上游答复是否表示"这个号已经领过了"。
func claimAlreadyReply(code int, msg string) bool {
	if claimAlreadyCodes[code] {
		return true
	}
	low := strings.ToLower(strings.TrimSpace(msg))
	for _, w := range claimAlreadyWords {
		if strings.Contains(low, strings.ToLower(w)) {
			return true
		}
	}
	return false
}

// isClaimNormalState 上游的**业务拒绝**（"没有可领的"）属于正常态，不该报红。
//
// 补偿是"有则领"的幂等写，「没有补偿」在正常情况下就是一个业务错误 —— 与真失败
// （网络 / 5xx / 401）必须分开，否则运维每次点都会看到一片"失败"，进而学会忽略它。
//
// ⚠️ 判据的关键细节（单测抓出来的）：上游的业务拒绝是 **HTTP 200 + `code != 0`**，
// 而 `doJSON` 在 `env.Code != 0` 分支里构造的 `Error.Status` 仍是**响应状态码 200**
// —— 所以"看 Status 是不是 4xx"这条判据会漏掉最常见的那种形态，把它误报成真失败。
// 正确的判据是看 `Kind`：`ErrClient` 就是"上游答复了、只是这个请求不能成功"。
//
// 401 刻意**不算**正常态：会话失效能被识别出来（Kind 是 ErrSessionDead 而非
// ErrClient），那是真问题，必须报出来让人去处理。
func isClaimNormalState(err error) bool {
	var ue *upstream.Error
	if !errors.As(err, &ue) {
		return false // 非上游错误（网络 / 超时 / 出口不可用）→ 真失败
	}
	if ue.Kind == upstream.ErrClient {
		return true // 业务拒绝（多为 HTTP 200 + code!=0）
	}
	switch ue.Status {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusConflict,
		http.StatusForbidden, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

// claimErrText 错误文案（截断，避免把整个 HTML 错误页塞进 JSON）。
func claimErrText(err error) string {
	s := strings.TrimSpace(err.Error())
	if s == "" {
		return "上游调用失败"
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// claimRequest 执行请求。UIDs 与 All 二选一（显式声明 All，避免空筛选被误当全选）。
type claimRequest struct {
	UIDs []string `json:"uids"`
	All  bool     `json:"all"`
	// DryRun 只列出将要处理的账号，不出站（给前端"先看看有多少"用）。
	DryRun bool `json:"dry_run"`
}

// claimTarget 一个待处理账号（管理端带号主信息，便于按号主判断隔离）。
type claimTarget struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	OwnerID  int64  `json:"owner_user_id"`
}

// resolveClaimTargets 解析目标账号集合。
//
// ownerScope < 0 表示不限（管理端）；>= 0 表示只允许该号主（用户控制台）。
// 用户侧**必须先过滤再判断空**：越权的目标要静默剔除而不是报错，
// 否则接口就成了"探测别人 uid 是否存在"的探针。
func (a *app) resolveClaimTargets(req claimRequest, ownerScope int64) ([]claimTarget, error) {
	accs, err := a.store.ListAccounts()
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, u := range req.UIDs {
		want[strings.TrimSpace(u)] = true
	}
	out := make([]claimTarget, 0, len(accs))
	for _, acc := range accs {
		if ownerScope >= 0 && acc.OwnerUserID != ownerScope {
			continue
		}
		if !req.All && !want[acc.UID] {
			continue
		}
		out = append(out, claimTarget{UID: acc.UID, Nickname: acc.Nickname, OwnerID: acc.OwnerUserID})
	}
	return out, nil
}

// handlePluginClaim POST /api/{admin/}plugins/{id}/claim
//
// 每次调用**只处理一个账号**（uid 必填）。批量由前端**串行**循环完成 ——
// 这是刻意的设计：
//   - 服务端不持有长任务状态（无超时、无重启丢失、无进度对齐问题）；
//   - 前端能实时显示逐号进度、失败可单独重试、可随时中断；
//   - 出站节奏天然是"一次一个"，与 pluginGate 同向，不会出现"一个 HTTP 请求
//     里塞 33 次出站"那种无法限流的形态。
//
// 若将来要服务端批量化，**必须**先有任务队列与取消语义，不能直接在这里加循环。
func (a *app) handlePluginClaim(w http.ResponseWriter, r *http.Request, wantAdmin bool) {
	id := r.PathValue("id")
	def, ok := pluginByID(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "插件不存在"})
		return
	}
	if wantAdmin && !audienceVisible(def, "admin") || !wantAdmin && !audienceVisible(def, "user") {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "该插件对本端不可用"})
		return
	}
	if !a.pluginEnabled(def.ID) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "插件已停用，请先在「插件」页启用"})
		return
	}
	// 用户级停用同样要拒绝执行（v0.31.22）—— 停用不是纯前端效果：
	// 前端只是"不渲染入口"，真正的门槛必须在这里，否则一个直接调接口的客户端
	// 就能绕过用户的停用意愿。
	if !wantAdmin {
		if u := userFrom(r); u != nil && !a.pluginUserEnabled(u.ID, def.ID) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "你已停用该插件，请先在「插件」页启用"})
			return
		}
	}
	// 独立界面型插件（Kind=egress）没有"执行一次"的语义 —— 它是持续运行的控制台。
	//
	// 这条判断放在**解析参数之前**：否则 `dry_run:true` 会先返回一份目标列表，
	// 调用方会以为"接口能用，只是还没执行"。语义不对的接口应该在门口就拒绝，
	// 而不是先陪它走完一遍流程。
	if def.Kind == "egress" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "这个插件是独立界面（把本机接成出口代理），不通过批量执行接口运行；" +
				"请在插件页点「打开」进入它的面板",
		})
		return
	}

	var req claimRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}

	ownerScope := int64(-1) // 管理端不限
	if !wantAdmin {
		u := userFrom(r)
		if u == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "未登录"})
			return
		}
		ownerScope = u.ID
	}

	// 目标列表（DryRun 与执行共用同一份解析，避免"预览的和执行的不是一批"）。
	targets, err := a.resolveClaimTargets(req, ownerScope)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if req.DryRun {
		writeJSON(w, http.StatusOK, map[string]any{
			"total": len(targets), "targets": targets, "dry_run": true,
		})
		return
	}

	// 单号执行（本接口不批量，见函数头注释）。
	if len(targets) != 1 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "本接口一次只处理一个账号（请传 uids 恰好一个），批量请由前端串行循环",
		})
		return
	}
	switch id {
	case pluginIDClaimCompensation:
		res := a.claimCompensationOne(targets[0].UID)
		if res.Nickname == "" {
			res.Nickname = targets[0].Nickname
		}
		if res.Status == claimOK {
			log.Printf("INFO: [plugins] 领取补偿 ok acct=%s +%d credit",
				logfmt.Label(res.UID, res.Nickname), res.Credit)
		}
		writeJSON(w, http.StatusOK, res)
	default:
		writeJSON(w, http.StatusNotImplemented, map[string]any{"error": "该插件未实现执行入口"})
	}
}

// claimIntOr 把 kv 里的字符串读成 int（带默认值）。
// 留着给后续插件用（现有插件不需要数值参数）；不导出，避免误用。
func claimIntOr(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return def
}
