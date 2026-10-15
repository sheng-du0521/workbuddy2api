// 模型名归一化的**证据源**（算法本身已下沉到 internal/modelname，v0.31.3）。
//
// 为什么算法要下沉：旧网关（internal/server）此前只剥 realm 前缀，没有任何大小写/
// 空白归一 —— 同一个 `GLM-5.3` 在主产品上能被救回、在旧网关上必然 404；而且
// (账号,模型) 冷却键的写法不同源，一个产品写下的 6004 冷却另一个看不见。
// 现在两个产品共用 internal/modelname 的同一份实现与同一份口径。
//
// 本文件只剩**本产品特有的证据源装配**：已知模型清单从哪来（三源合并，全部零出站）。
package main

import (
	"sort"
	"strings"
	"sync"

	"workbuddy2api/internal/modelname"
)

// normalizeModelName 归一化（薄封装，保留本包既有调用点的可读性）。
func normalizeModelName(raw string, known []string) (string, string) {
	return modelname.Normalize(raw, known)
}

// knownModelSet 该名字是否在已知清单里（与归一化同口径）。
func knownModelSet(known []string, m string) bool { return modelname.IsKnown(known, m) }

// noteUnknownModel 记一次"未知模型名"（同名只记一次）。
func noteUnknownModel(m string, known []string) {
	modelname.NoteUnknown(m, known, "proxy")
}

// knownModelIDs 归一化时使用的已知模型清单（三源合并，**全部零出站**）：
//
//  1. /v1/models 的新鲜缓存（5 分钟 TTL）—— 最全；
//  2. 池内**实际服务过**的模型（observed，来自 (账号,模型) 成本台账与模型级冷却键）
//     —— 进程刚启动、缓存还没热时它就有值，且名字一定被上游认过；
//  3. 内置兜底清单 —— 池空/新部署时的最小集合。
//
// 为什么"绝不出站"是硬要求：ServeChat 是热路径，而权威清单要一次上游往返。
// 拿不到就退化成只做去空白/剥前缀（第 1、2 层），归一化的正确性不受影响
// —— 少的是"把大小写折回规范名"这一层便利。
//
// 上限 256 条：清单只用于字符串比较，但**不该无界**（模型名来自请求与服务端观测，
// 属于外部输入；一个乱打模型的客户端能把池内观测表撑大）。
// 兜底清单与 internal/modelname 同源（`modelname.FallbackModels`），不在此重复定义。
func knownModelIDs(observed []string) []string {
	const maxKnown = 256
	out := make([]string, 0, 32)
	seen := map[string]struct{}{}
	add := func(m string) {
		if m == "" || len(out) >= maxKnown {
			return
		}
		if _, ok := seen[m]; ok {
			return
		}
		seen[m] = struct{}{}
		out = append(out, m)
	}
	if ids, ok := modelsCacheSnapshotIfFresh(); ok {
		for _, m := range ids {
			add(m)
		}
	}
	for _, m := range observed {
		add(m)
	}
	for _, m := range recentModelList() {
		add(m)
	}
	for _, m := range modelname.FallbackModels {
		add(m)
	}
	return out
}

// ---------------------------------------------------------------------------
// 「本进程/本库见过且**上游认过**的模型名」——归一化的第三类证据源
// ---------------------------------------------------------------------------

// recentModels 近期成功服务过的模型名（进程内 + 启动时从 usage_logs 种子化）。
//
// 为什么需要它（本机实测的教训）：模型清单缓存（/v1/models，5 分钟 TTL）在
// **进程刚启动**时是空的，而池内成本台账（pool.ObservedModels）只覆盖最近 6 小时
// 且未必落盘 —— 两者都空时，"大小写归一 / 分隔符归一"这两层会静默失效：
// 实测重启后 `GLM-5.3` 仍然 404，而 `" glm-5.3 "`（去空白层不依赖清单）已经 200。
// 于是加一条**零出站且跨重启可靠**的来源：usage_logs 里 status=ok 的模型名。
//
// 只记**成功**过的名字：失败的那次可能是写法错的（把错的写进清单，
// 下次就会把正确写法"归一"成错的 —— 恰好是最危险的方向）。
var recentModels struct {
	mu   sync.RWMutex
	list []string
	seen map[string]struct{}
}

// rememberModel 记录一个**上游认过**的模型名（幂等；上限 256，超出丢弃新值）。
func rememberModel(m string) {
	m = strings.TrimSpace(m)
	if m == "" {
		return
	}
	recentModels.mu.Lock()
	defer recentModels.mu.Unlock()
	if recentModels.seen == nil {
		recentModels.seen = map[string]struct{}{}
	}
	if _, ok := recentModels.seen[m]; ok {
		return
	}
	if len(recentModels.list) >= 256 {
		return // 只增不替换：清单是外部输入的派生集，宁可保留已确认的旧值
	}
	recentModels.seen[m] = struct{}{}
	recentModels.list = append(recentModels.list, m)
}

// recentModelList 返回近期成功模型名的副本（稳定排序，便于测试与日志）。
func recentModelList() []string {
	recentModels.mu.RLock()
	defer recentModels.mu.RUnlock()
	out := make([]string, len(recentModels.list))
	copy(out, recentModels.list)
	sort.Strings(out)
	return out
}
