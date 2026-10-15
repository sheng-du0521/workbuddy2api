// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// P2-7 拆包「第四刀第二刀」：用量报表域的**桥接层**（v0.31.13）。
//
// 本文件是 `cmd/webapp → internal/api/report` 的接线，只做两件事：
//  1. 把 app 的能力面（store + 会话身份）喂给 report.Service；
//  2. 把三条既有路由的处理原样转过去（路由本身仍注册在 main.go）。
//
// ⚠️ **桥接层只转发不判断** —— 这里不许出现任何 if/文案/写库/状态码。
// 一旦桥接层开始"顺手修一下"，同一个域就会出现两份会分叉的纪律
// （这正是 2026-10-13 批量误杀的成因类型）。
package main

import (
	"net/http"

	"workbuddy2api/internal/api/report"
)

// reportService 每次现构造。
//
// 为什么不缓存成 app 的字段：report.Service 无内部状态（只持两个不可变引用），
// 现构造既不会有可观测开销，又**结构性地杜绝**"某条构造路径忘了赋值 →
// 整个报表域 nil panic"这类缺陷（值接收者 + 现构造是本仓刻意保留的写法）。
func (a *app) reportService() report.Service {
	return report.New(a.store, a.reportUserID)
}

// reportUserID 会话 → 用户 ID，**显式布尔**表达"有没有登录"。
//
// 不用"ID == 0 就是匿名"：那是在靠"某个字段恰好是零值"推断事实，
// 真实用户 ID 从 1 起只是巧合而非契约。
func (a *app) reportUserID(r *http.Request) (int64, bool) {
	u := userFrom(r)
	if u == nil {
		return 0, false
	}
	return u.ID, true
}

// ---- 三条路由的处理（签名与迁移前逐字一致，main.go 的注册行未改动）----

func (a *app) handleUsageBreakdown(w http.ResponseWriter, r *http.Request) {
	a.reportService().HandleUsageBreakdown(w, r)
}

func (a *app) handleAdminUsageRanking(w http.ResponseWriter, r *http.Request) {
	a.reportService().HandleAdminRanking(w, r)
}

func (a *app) handleUserUsageRanking(w http.ResponseWriter, r *http.Request) {
	a.reportService().HandleUserRanking(w, r)
}
