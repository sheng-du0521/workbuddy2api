// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 账号状态机迁移的唯一权威实现。
//
// entry 的「可选择性」由五个正交维度决定：禁用(disabled)、手动停用(manualDisabled)、
// 账号级冷却(until/coolKind)、模型级冷却(modelCooldowns)、熔断(breakerUntil)。
// 维度之间以「迁移原语」收拢，禁止在其他文件散写这些字段——所有入口（applyErrorPolicy /
// refresh / keepalive / 签到 / 选号 / 运维端点）对状态的改动都必须经本文件的原语或经
// Cooldown/NoteError/NoteSuccess 等封装（它们在持锁下调用本文件原语）。
//
// 迁移矩阵（事件 → 动作 → 字段）：
//
//	disabled           ← disableLocked（Disable / NoteSessionDead 达阈）
//	manualDisabled     ← setManualDisabledLocked（运维端点 / CLI；只置位不清其他维度）
//	until/coolKind     ← Cooldown(CoolSoft/Hard，固定时长) / CooldownSoftRate / CooldownSoftForModel 无解析分支
//	modelCooldowns     ← CooldownSoftForModel 有解析分支；被 disableLocked/Cooldown/clearCoolingLocked 清
//	breakerUntil       ← recordBreakerFailureLocked（NoteError 喂入）；NoteSuccess 清
//	softStreak         ← CooldownSoftRate / CooldownSoftForModel 无解析分支；NoteSuccess/reviveCoolingLocked 清
//	sessionDeadFails   ← NoteSessionDead；ClearSessionDead/NoteSuccess/ReviveDisabled 清
//
// 关键正交性（疑点 4 修正）：
//   - 冷却域（until/coolKind/softStreak/modelCooldowns）与熔断器（fails/retryCount/
//     breakerUntil）正交：冷却管「近期被限流/余额耗尽」，熔断管「反复 5xx 失败」。
//     disableLocked 只清冷却域、不动熔断——禁用是授权/session 终态，不应覆盖熔断观测。
//   - clearCoolingLocked 是「冷却域归零」的单一来源，被 disableLocked 与
//     reviveCoolingLocked（签到解冻）共用，二者对冷却域的处置因此永远一致。
//   - manualDisabled 与 disabled 各自独立：前者是运维意图（只能由运维入口清除），
//     后者是系统判定（可被签到解冻/refresh 等路径自动撤销）。二者都不清对方，
//     并存时 /status 分别透出（见 entry.go Status.manual_disabled/disabled 注释）。
package pool

import "time"

// clearCoolingLocked 清**全部**冷却域：账号级（until/coolKind/reason/softStreak）
// 与模型级（modelCooldowns）一起归零。熔断器不属冷却域，不动。
//
// 只用于「账号退出选号」的终态迁移（disableLocked）与**人工**启用
// （ReviveDisabled）——这两种场景下"这个号完全不用了 / 运维宣布它可用"是明确的，
// 残留的模型级冷却只会造成"点了启用却没反应"。
//
// ⚠️ **签到的余额解冻（reviveCoolingLocked）不要走这里**，用
// clearAccountCoolingLocked —— 理由见其注释（v0.31.0 修正）。
// 调用方必须已持有 p.mu。
func (e *entry) clearCoolingLocked() {
	e.clearAccountCoolingLocked()
	e.modelCooldowns = nil // 账号退出选号 / 人工启用：模型级独立冷却一并作废
}

// clearAccountCoolingLocked 只清**账号级**冷却域（until/coolKind/reason/softStreak），
// **保留** modelCooldowns（6004/11102 的 (账号, 模型) 独立冷却）。
//
// 为什么必须拆出这一层（v0.31.0 实测缺陷）：reviveCoolingLocked 此前调
// clearCoolingLocked，而它的唯一生产调用者是 ReenableIfCredits —— 该函数被
// 「每日签到（每个在池账号、含冷却中的号）」「健康检测探测成功」「代挂刷新余额」
// **无条件**调用。于是任何一次余额刷新都会把整个模型的 6004 冷却表清空：
//
//	09:00 签到 → 清空 → 10:00 请求同一模型 → 重新调度到该号 → 又撞 6004
//
// 后果有两层：① 上游明说的重置墙钟被无视，冷却实际被截断到"距下次签到/探活的
// 间隔"（v0.26.5 把模型级上限提到 24h 的意义被抵消）；② 每次清空都放行一批
// 必然失败的请求（用户侧多一次失败、上游多一条风控痕迹）。
//
// 语义依据：签到只证明**余额通道**通了，既没有证明 chat 通道可用，更没有证明
// 「这个模型没被限流」。而连 NoteSuccess（强得多的证据：该号刚刚真的服务成功，
// 且**就是**在 chat 通道上）都刻意不碰 modelCooldowns —— 见其注释
// 「其他模型成功不得抹掉本模型的冷却截止」。更弱的证据没有理由清得更狠。
func (e *entry) clearAccountCoolingLocked() {
	e.until = time.Time{}
	e.coolKind = 0
	e.reason = ""
	e.softStreak = 0
}

// disableLocked 禁用迁移：置 disabled 并清冷却域（禁用是比冷却更强的不可用终态）。
//
// 疑点 4 修正：旧 Disable 只置 disabled+reason，不碰 until/modelCooldowns/softStreak，
// 会出现「disabled=true 但 cooling=true / 残留 modelCooldowns」的一致性问题——一个
// 先被硬冷却（到次日 04:00）再被禁用的账号会同时呈现两种状态。禁用后冷却无意义
// （账号已退出选号，冷却截止不再被读取），故一并清空。
//
// 熔断器保留：熔断是「连续 5xx 失败」信号（与授权/会话无关），禁用后再复活时
// 熔断观测仍有效，不应被禁用覆盖。
func (p *Pool) disableLocked(e *entry, reason string) {
	e.clearCoolingLocked()
	e.disabled = true
	e.reason = reason
	p.dirty.Store(true)
}

// reviveCoolingLocked 清**账号级**冷却域（until/coolKind/reason/softStreak）并更新
// credits，不动熔断器（fails/retryCount/breakerUntil），也不动 modelCooldowns（v0.31.0）。
// 签到解冻走这里：签到成功只证明余额恢复与 billing 通道健康，不证明 chat 通道健康，
// 熔断（连续 5xx 信号）不应被签到覆盖；同理，它也不构成「这个模型没被限流」的证据 ——
// 模型级冷却有上游给的重置墙钟，由自身 Until 到期（或人工启用）回收，见
// clearAccountCoolingLocked 的注释。
// softStreak 属冷却域（与 until/coolKind 同域），随冷却一并清零——与「解冻只清冷却
// 不清熔断」的既有 C5 语义一致；硬冷却（CoolHard）本就不参与 streak，这里清的是
// 历史软冷却累积。调用方必须已持有 p.mu。
func (p *Pool) reviveCoolingLocked(e *entry, credits int64) {
	e.credits = credits
	e.clearAccountCoolingLocked()
}

// setManualDisabledLocked 手动停用迁移（运维入口）：只置 manualDisabled + 原因，
// **不清冷却域、不动熔断器**。
//
// 与 disableLocked（自动禁用）的关键差异——手动停用是「对话流量摘除」，不是
// 「账号冻结」：签到、token 保活、排程任务照常执行，账号凭证与积分状态都是活的。
// 因此刻意不碰 until/coolKind/modelCooldowns/breakerUntil：停用期间这些维度继续
// 按各自规律演进（冷却自然到期、熔断计数继续累计），恢复时拿到的是「停用期间
// 真实发生过什么」的完整状态，而不是被清空的一刀切。
//
// 为什么用独立状态位而非复用 disabled：自动禁用会被签到解冻、refresh 成功等路径
// 自动撤销，手动停用若复用同一字段，运维意图会被这些路径意外解除。两位独立、
// 各自清除，都清空才回到选号池。
// 调用方必须已持有 p.mu。
func (p *Pool) setManualDisabledLocked(e *entry, disabled bool, reason string) {
	e.manualDisabled = disabled
	if disabled {
		e.manualReason = reason
	} else {
		e.manualReason = ""
	}
	p.dirty.Store(true)
}
