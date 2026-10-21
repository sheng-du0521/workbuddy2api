// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ======================================================================
// 通道接入：探活识别到的模型「一键入库」（v0.31.88）
//
// 需求：接入第三方通道后，自动识别它支持的模型，把它们加入系统模型列表，
// 并能给每个模型单独配「每 1k 积分」与「计费倍率」。
//
// 两个「列表」别混（这是本文件最重要的注释）：
//   - upstream_models：**这条通道能服务哪些模型**。没有它，通道存在但一个模型都路由不到。
//   - model_ratios   ：**系统模型列表**（用户可见目录 + 计费倍率）。没有它，
//     运营在「模型倍率」页看不到这个模型，也就没法给它单独定价。
//
// 所以"自动加入系统模型列表"必须两张表都写。只写前者 = 能用但看不见；
// 只写后者 = 看得见但跑不了（更糟）。
//
// 幂等语义（决定了「再点一次探活」是否安全）：
//   - 已有通道映射 → **跳过，不覆盖单价**。人工调过的价不能被一次探活冲掉；
//   - 已在 model_ratios 里 → 只补缺，不动官方倍率/实测值（那是同步任务的地盘）；
//   - 只有本次显式给了 billing_ratio>0 的模型才写倍率（0 = 跟随全局，不写）。
// ======================================================================

// relayImportItem 探活识别到的一条候选模型。
type relayImportItem struct {
	Model       string `json:"model"`
	RemoteModel string `json:"remote_model"`
	// CreditPer1k 每 1000 token 折算多少积分。0 = 暂不计费（可后补）。
	CreditPer1k float64 `json:"credit_per_1k"`
	// Enabled 缺省 true（探活识别到就是想让它能用）；显式 false 才不入路由。
	Enabled *bool `json:"enabled"`
	// BillingRatio 单模型计费倍率：0 = 不参与、跟随全局（**不是免费**）。
	BillingRatio float64 `json:"billing_ratio"`
}

// relayImportResult 一次导入的逐类结果。
//
// 分四类而不是一个数字：运营需要知道"这 20 个里哪些是新增、哪些本来就有、
// 哪些被拒了以及为什么" —— 只回「成功 18」等于让人自己去列表里数。
type relayImportResult struct {
	Added       []string `json:"added"`        // 新加入通道映射
	Skipped     []string `json:"skipped"`      // 已有映射，跳过（不覆盖单价）
	RatiosAdded []string `json:"ratios_added"` // 本次新进入系统模型列表
	RatiosSet   []string `json:"ratios_set"`   // 本次写入了单模型倍率
	Invalid     []string `json:"invalid"`      // 被拒绝的（带原因）
}

// Empty 本次什么都没做成（用于"别假装成功"）。
func (r relayImportResult) Empty() bool {
	return len(r.Added) == 0 && len(r.Skipped) == 0 && len(r.RatiosAdded) == 0
}

// ImportUpstreamModels 把一批模型写进「通道映射 + 系统模型列表」。
//
// **全程一个事务**：中途失败不能留下"映射写了一半、模型列表没写"的状态 ——
// 那种半成品最难查（通道列表里有、倍率页没有）。
func (s *Store) ImportUpstreamModels(upstreamID int64, items []relayImportItem) (relayImportResult, error) {
	var res relayImportResult
	if upstreamID <= 0 {
		return res, errors.New("必须先选一条通道")
	}
	if len(items) == 0 {
		return res, errors.New("没有要导入的模型")
	}
	if len(items) > 200 {
		return res, errors.New("一次最多导入 200 个模型（分几次导，别一次把整站目录塞进来）")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	for _, it := range items {
		model := strings.TrimSpace(it.Model)
		// 与 SaveUpstreamModel 同一套校验：两条入口对同一个字段不能有两种标准。
		if model == "" || len(model) > 120 {
			res.Invalid = append(res.Invalid, strings.TrimSpace(it.Model)+"（模型名为空或超过 120 字符）")
			continue
		}
		if it.CreditPer1k < 0 || it.CreditPer1k > 100000 {
			res.Invalid = append(res.Invalid, model+"（每 1k 积分必须在 0–100000 之间）")
			continue
		}
		if it.BillingRatio < 0 || it.BillingRatio > billingRatioMax {
			res.Invalid = append(res.Invalid, model+"（倍率必须在 0–"+
				strconv.FormatFloat(billingRatioMax, 'f', 0, 64)+" 之间，0 = 跟随全局）")
			continue
		}
		en := 1
		if it.Enabled != nil && !*it.Enabled {
			en = 0
		}

		// ① 通道映射：已存在就跳过（不覆盖人工调过的单价）。
		r, err := tx.Exec(`INSERT INTO upstream_models
			(upstream_id, model, remote_model, credit_per_1k, enabled, verified_at, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?)
			ON CONFLICT(upstream_id, model) DO NOTHING`,
			upstreamID, model, strings.TrimSpace(it.RemoteModel), it.CreditPer1k, en, now, now, now)
		if err != nil {
			return res, err
		}
		if n, _ := r.RowsAffected(); n == 0 {
			res.Skipped = append(res.Skipped, model)
		} else {
			res.Added = append(res.Added, model)
		}

		// ② 系统模型列表：只补缺。
		// 用 DO NOTHING 而不是 PutModelRatio 的 upsert —— 后者会把 official_credits /
		// measured_cost_1k 一起改成本次传的值（这里是空），等于把官方同步的成果冲掉。
		rr, err := tx.Exec(`INSERT INTO model_ratios
			(model, name, official_credits, official_badge, official_free,
			 measured_cost_1k, measured_known, updated_at)
			VALUES (?,?,'','',0,0,0,?)
			ON CONFLICT(model) DO NOTHING`, model, model, now)
		if err != nil {
			return res, err
		}
		if n, _ := rr.RowsAffected(); n > 0 {
			res.RatiosAdded = append(res.RatiosAdded, model)
		}

		// ③ 单模型倍率：只有显式给了 >0 才写。
		// scope 固定 relay：这是通道模型的价，写成 pool 会把号池同名模型一起调价。
		if it.BillingRatio > 0 {
			if _, err := tx.Exec(`UPDATE model_ratios
				SET billing_ratio = ?, billing_scope = 'relay', updated_at = ?
				WHERE model = ?`, it.BillingRatio, now, model); err != nil {
				return res, err
			}
			res.RatiosSet = append(res.RatiosSet, model)
		}
	}
	if err := tx.Commit(); err != nil {
		return res, err
	}
	return res, nil
}

// handleAdminUpstreamImport POST /api/admin/upstreams/models/import
//
//	{ "id": 3, "models": [ {"model":"gpt-5.5","credit_per_1k":12,"billing_ratio":1.5} ] }
//
// 探活识别到的模型从这里一次入库。**不做探活**：探活是另一次请求（运营要先看到清单
// 才决定导哪些），这里只负责"把已经确认的写进库"。
func (a *app) handleAdminUpstreamImport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     int64             `json:"id"`
		Models []relayImportItem `json:"models"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	if req.ID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id 不能为空"})
		return
	}
	// 通道必须存在：挂到不存在的通道上会变成一批永远路由不到的孤儿映射。
	list, err := a.store.ListUpstreams()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	found := false
	for i := range list {
		if list[i].ID == req.ID {
			found = true
			break
		}
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "通道不存在"})
		return
	}

	res, err := a.store.ImportUpstreamModels(req.ID, req.Models)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	// 入库后两件事必须一起做（与单条保存同一收尾）：作废模型缓存 + 刷新单价/倍率快照。
	// 漏掉后者会让"刚配的倍率"在下一笔请求里不生效。
	a.channelConfigChanged()
	a.proxy.ReloadBilling()

	log.Printf("INFO: [relay] 通道 id=%d 导入模型：新增 %d、跳过 %d、入目录 %d、写倍率 %d、非法 %d",
		req.ID, len(res.Added), len(res.Skipped), len(res.RatiosAdded), len(res.RatiosSet), len(res.Invalid))

	if res.Empty() && len(res.Invalid) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "没有一条能导入：" + strings.Join(res.Invalid, "；"),
			"code":  "nothing_imported",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "added": res.Added, "skipped": res.Skipped,
		"ratios_added": res.RatiosAdded, "ratios_set": res.RatiosSet, "invalid": res.Invalid,
	})
}
