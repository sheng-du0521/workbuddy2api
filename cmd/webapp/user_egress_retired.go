package main

import (
	"log"
	"net/http"
	"sync"
	"time"
)

// handleUserEgressRetired 用户端代理能力的**统一停止位**（v0.31.53）。
//
// 背景：产品决定把出口代理收敛为"平台能力" —— 普通用户不再自行获取、
// 配置或分配出口，所有账号统一由管理后台分配。前端入口、界面、业务实现
// 都已删除；这个 handler 只负责回答**还在发请求的客户端**。
//
// 为什么用 410 而不是 404：
//   - 404 与"路径写错了"无法区分，老前端会以为是地址问题，反复重试；
//   - 410 是"这里曾经有、现在永久没有了"，配 code 与下一步，一次就能看懂。
//
// 为什么打日志：这四个端点的调用量就是"旧逻辑残留"的度量。
// 上线后如果还在被刷，说明还有某个客户端（或某个缓存页）没更新 ——
// 没有这行日志，我们只能靠猜。
func (a *app) handleUserEgressRetired(w http.ResponseWriter, r *http.Request) {
	var uid int64
	if u := userFrom(r); u != nil {
		uid = u.ID
	}
	log.Printf("WARN: [egress] 用户端代理端点被调用 %s %s user=%d ip=%s —— 该能力已于 v0.31.53 下线",
		r.Method, r.URL.Path, uid, clientIPOf(r))
	writeJSON(w, http.StatusGone, map[string]any{
		"error": "用户端代理已下线：账号出口统一由管理后台分配。" +
			"如需调整你自己账号的出口，请联系平台管理员。",
		"code":  "user_egress_removed",
		"since": "v0.31.53",
		"hint":  "用户控制台不再提供任何代理入口；管理员在「管理后台 → 代理」按账号分配出口",
	})
}

// writeUserEgressGone agent 侧的统一下线响应（v0.31.53）。
//
// 为什么连 agent 也要拒：已经装过 agent 的机器仍在每 5 秒 poll 一次 ——
// 它会持续往机器表里写记录（页面上就成了"看得见但没有线路"的幽灵机器），
// 而且 `/api/agent/config` 会把它的受限私钥发下来。用户端代理下线后，
// 这台机器不再有任何合法用途，所以直接 410 + action=stop，让 agent 自己收摊。
//
// 为什么带 action=stop：agent 的旧版本不认 410，但认 `action` 字段 ——
// 让它停总比让它继续每 5 秒重试要好（否则日志与机器表都会被刷）。
func writeUserEgressGone(w http.ResponseWriter) {
	writeJSON(w, http.StatusGone, map[string]any{
		"action": "stop",
		"code":   "user_egress_removed",
		"since":  "v0.31.53",
		"error":  "用户端代理已下线：这台机器不再接入平台。请关闭本机 agent 并删除安装目录。",
	})
}

// egressUserAgentWarn 用户 agent 被拒日志的节流（v0.31.53）。
//
// 为什么必须节流：旧 agent 每 5 秒 poll 一次，且**不会因为 410 就退出**
// （用户得手动删目录）。不节流的话，一台没删的机器就能刷出 1.7 万条/天，
// 把真正的排障日志淹掉 —— 这正是本版要避免的那种"自己制造噪音"。
//
// 口径：同一 owner 每 5 分钟记一条。首条一定会记（那是"还有残留"的证据）。
var (
	egressUserAgentWarnMu sync.Mutex
	egressUserAgentWarnAt = map[int64]int64{}
)

func shouldLogUserAgentGone(uid int64) bool {
	now := time.Now().Unix()
	egressUserAgentWarnMu.Lock()
	defer egressUserAgentWarnMu.Unlock()
	if t := egressUserAgentWarnAt[uid]; now-t < 300 {
		return false
	}
	egressUserAgentWarnAt[uid] = now
	return true
}
