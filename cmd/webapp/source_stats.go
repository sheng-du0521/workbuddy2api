// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// source_stats.go 账号来源健康度（v0.31.37）。
//
// ## 这个端点存在的唯一理由
//
// 运营反复提出一个判断：「掉线的大多数是扫码登录的号」。在此之前这个判断
// **无法被验证** —— 库里没有来源列，池里没有来源信息，界面上也没有任何地方
// 把"怎么进来的"和"现在还好吗"放在一起。于是它只能停留在印象层面：
// 事后复盘时谁也拿不出数字，改版时也就只能靠感觉决定"该修扫码还是该修保活"。
//
// 现在给一个纯读数端点：按来源聚合 号数 / 在池 / 登录失效 / 仅剩 access，
// 外加最关键的 `dead_rate`。它不产出任何出站请求，也不改任何状态。
//
// ## 口径（必须写死，否则同样的名字会得出不同结论）
//
//   - 分母 = 该来源的**全部**账号（含已停用、未入池）——不是"在池账号"。
//     用池内做分母会让"未入池但已失效"的号凭空消失，失效占比被系统性低估。
//   - `dead` 只算 token_dead（真死）；refresh_lost 单独一列，**不并入** dead。
//     把"仅剩 access"算成死号，会让扫码来源的失效占比虚高，正好把要验证的
//     那个判断做假。这是本文件最容易写错的一行。
//   - 历史行（source 为空）单列成 "unknown"，不丢弃：它是老数据的真相，
//     且"unknown 占多少"本身说明来源标记从哪一天开始生效。
package main

import (
	"net/http"
)

// sourceStatRow 单个来源的统计行（对外下发，字段名必须显式带 json tag）。
type sourceStatRow struct {
	Source string `json:"source"`
	Label  string `json:"label"`
	Total  int    `json:"total"`
	InPool int    `json:"in_pool"`
	Dead   int    `json:"dead"`
	Lost   int    `json:"lost"`
	// DeadRate 登录失效占该来源全部账号的比例（0~1，保留三位有效小数由前端处理）。
	DeadRate float64 `json:"dead_rate"`
}

// sourceLabel 来源的中文名（前端不再自己映射，避免两边口径漂移）。
func sourceLabel(src string) string {
	switch src {
	case sourceQR:
		return "扫码登录"
	case sourceImport:
		return "JSON 导入"
	}
	return "历史/未知"
}

// buildSourceStats 聚合来源健康度（纯函数：入参全是普通值，便于单测钉住口径）。
//
// dead/lost 用 map 传入而不是从 store 现查，是为了让这个函数可以在测试里
// 构造"某个来源里有一半失效"这种极端分布 —— 而不用起数据库。
func buildSourceStats(accs []*Account, dead, lost map[string]int64, src map[string]string) []sourceStatRow {
	// 固定顺序输出：unknown → qr → import。
	// 顺序固定不是为了好看：前端表格按返回顺序渲染，若按 map 迭代就会每次刷新
	// 换一个行序，运维在做"前后对比"时会误以为数据错位。
	order := []string{"", sourceQR, sourceImport}
	idx := map[string]*sourceStatRow{}
	out := make([]sourceStatRow, 0, len(order))
	for _, k := range order {
		out = append(out, sourceStatRow{Source: k, Label: sourceLabel(k)})
		idx[k] = &out[len(out)-1]
	}
	for _, a := range accs {
		if a == nil || a.UID == "" {
			continue
		}
		k := normSource(src[a.UID])
		row, ok := idx[k]
		if !ok { // 理论上不可达（normSource 已封闭取值），防御性归到 unknown
			row = idx[""]
		}
		row.Total++
		// 在池口径与 effectiveConsumeGroup 的判定保持一致：平台号恒在池，
		// 代挂号看 pool_state == "in"。
		if a.OwnerUserID == 0 || a.PoolState == "in" {
			row.InPool++
		}
		if _, ok := dead[a.UID]; ok {
			row.Dead++
		} else if _, ok := lost[a.UID]; ok {
			// 真死优先：一个号同时有两位时（理论上 MarkTokenDead 会清 lost，
			// 但历史行可能同时存在）只算 dead，避免同一个号被数两次。
			row.Lost++
		}
	}
	for i := range out {
		if out[i].Total > 0 {
			out[i].DeadRate = float64(out[i].Dead) / float64(out[i].Total)
		}
	}
	return out
}

// handleAccountSourceStats GET /api/admin/accounts/source-stats
//
// 只读：不刷新、不探测、不改状态。放在 adminGuard 之后（含池内账号规模等运营信息）。
func (a *app) handleAccountSourceStats(w http.ResponseWriter, r *http.Request) {
	accs, err := a.store.ListAccounts()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	rows := buildSourceStats(accs, a.store.TokenDeadMap(), a.store.RefreshLostMap(), a.store.SourceMap())
	writeJSON(w, http.StatusOK, map[string]any{
		"rows": rows,
		// 口径随数据一起下发：前端不需要（也不应该）自己再算一遍。
		"note": "分母为该来源全部账号；dead 仅计 token_dead，refresh_lost 单列",
	})
}
