// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// growth_provider.go 把 Store 里的账号适配成 scheduler.GrowthProvider（v0.17.0）。
//
// 这层适配的意义：scheduler 包不该知道 webapp 的 Store / 加密字段 / Account 结构，
// 只需要「按 uid 给我明文凭证」与「记录这个号的执行时间」两个能力。
package main

import (
	"log"

	"workbuddy2api/internal/scheduler"
)

// growthProvider scheduler.GrowthProvider 的 webapp 实现。
type growthProvider struct{ store *Store }

// toGrowthCreds 把账号转成凭证，跳过没有 access token 的账号。
//
// 为什么跳过而不是让脚本去报错：成长任务全部是带 Bearer 的写请求，没有 access
// token 就一定失败；先在这里滤掉，可以避免往台账里刷一条无意义的失败，
// 把真正的故障淹掉。
func (g growthProvider) toGrowthCreds(list []*Account) []scheduler.GrowthCred {
	out := make([]scheduler.GrowthCred, 0, len(list))
	for _, a := range list {
		// scanAccount 已在读取边界解密，AccessToken 此处是明文。
		if a.AccessToken == "" {
			log.Printf("growth: 跳过 %s（无 access token）", shortUIDForLog(a.UID))
			continue
		}
		out = append(out, scheduler.GrowthCred{
			UID:         a.UID,
			Nickname:    a.Nickname,
			Realm:       a.Realm,
			Domain:      a.Domain,
			AccessToken: a.AccessToken,
		})
	}
	return out
}

// GrowthCredsByUID 按 uid 取凭证（顺序与入参一致，缺失 / 无 token 的跳过）。
func (g growthProvider) GrowthCredsByUID(uids []string) ([]scheduler.GrowthCred, error) {
	if g.store == nil || len(uids) == 0 {
		return nil, nil
	}
	list, err := g.store.AccountsByUIDs(uids)
	if err != nil {
		return nil, err
	}
	return g.toGrowthCreds(list), nil
}

// MarkGrowthDone 记录最近一次执行时间。
func (g growthProvider) MarkGrowthDone(uid string) error {
	if g.store == nil {
		return nil
	}
	return g.store.MarkGrowthDone(uid)
}

// shortUIDForLog 日志标识（8 位前缀）。
func shortUIDForLog(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}
