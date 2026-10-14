// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"workbuddy2api/internal/upstream"
)

// ======================================================================
// 积分明细的本地缓存（v0.24.9）
// ======================================================================
//
// 背景（必须记住，否则很容易又把它改回「渲染即出站」）：
//
//	v0.24.3 之前的账号卡是 creditPanel(url)：**一渲染就发一次真实出站**查上游积分。
//	账号池页有几十张卡片 → 几十个并发出站、同一出口 IP、几秒内打完 →
//	上游风控当场判「请求非法(11140)」→ 30 个健康账号被误判停用（2026-10-13 事故）。
//	所以 v0.24.3 改成了「点开才查」，并把出站统一挂进探活总闸（probeGate）。
//
// 代价是**可用性倒退**：卡片上只剩一个总额快照，看不到「哪些包、什么时候到期」——
// 而那恰恰是运维判断「这个号还能顶多久」的依据。
//
// 本文件把上一次**成功**查询到的逐套餐明细落库：
//   - 渲染卡片时直接读缓存 → **零出站**，拿回明细展示能力；
//   - 点「刷新明细」才真实出站（仍受 probeGate 约束），成功后覆盖缓存。
//
// 存储形态：`accounts.credit_packs` = JSON 数组（upstream.CreditPackage 原样序列化，
// 字段名与 /resources 接口的 resources 行完全一致，前端可共用同一套渲染）。
// 刻意**不**进 accountCols / UpsertAccount：这是展示缓存，不是账号属性。

const (
	// creditPacksCacheMax 单账号缓存的最大行数。
	//
	// 上限的意义是**给存储上界**：上游的套餐数量理论上无界（活动包会不断叠加，
	// 实测单号已见 27 个），而缓存只用于展示，截断不会影响任何业务判断。
	// 截断按**到期时间升序**——先丢「最晚到期」的，因为它们最不可能是运维要找的。
	creditPacksCacheMax = 80
	// creditPackNameMax 套餐名单行长度上限（防上游塞超长字符串进库）。
	//
	// 单位是**字符（rune）不是字节**：套餐名是中文，按字节截断会把一个汉字切成
	// 半个（非法 UTF-8），JSON 序列化时又被补成 U+FFFD —— 于是「截断到 64」的实际
	// 结果是 66 字节，且末尾是个乱码方块。单测（TestCreditPacksCaps）就是被这个抓出来的。
	creditPackNameMax = 64
)

// cachedCreditPacks 一个账号的明细缓存。
type cachedCreditPacks struct {
	Packs []upstream.CreditPackage
	At    int64
}

// encodeCreditPacks 序列化待入库的缓存（含截断与字段清洗）。
//
// 返回空串表示「没有可缓存的内容」——调用方据此**不覆盖**已有缓存，
// 避免一次返回空列表的异常响应把好数据抹掉。
func encodeCreditPacks(packs []upstream.CreditPackage, at int64) string {
	if len(packs) == 0 {
		return ""
	}
	rows := make([]upstream.CreditPackage, 0, len(packs))
	for _, p := range packs {
		p.Name = truncateRunes(strings.TrimSpace(p.Name), creditPackNameMax)
		rows = append(rows, p)
	}
	// 按到期升序（0 = 无到期，排最后）——与 /resources 接口的既有排序同口径，
	// 也让下面的截断丢掉「最晚到期」的那些。
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i].ExpireAt, rows[j].ExpireAt
		if a == 0 {
			return false
		}
		if b == 0 {
			return true
		}
		return a < b
	})
	if len(rows) > creditPacksCacheMax {
		rows = rows[:creditPacksCacheMax]
	}
	blob, err := json.Marshal(rows)
	if err != nil {
		return ""
	}
	// 缓存整体体积兜底：极端情况下（上游返回上千个包）也不该把一行撑爆。
	if len(blob) > 64*1024 {
		return ""
	}
	// At 单独一列存放，不进 JSON —— 便于 SQL 侧直接读时间戳而不必解析。
	_ = at
	return string(blob)
}

// cachedRemain 缓存明细里的**可用合计**（排除已过期的包）。
//
// 为什么卡片上的总额要用它而不是池内快照（v0.25.0 起）：
//   - 池内 credits 是「逐次请求扣减出来的内存快照」，会漂移；
//   - 缓存明细是**上一次真实查询**的逐包结果，与卡片下面列出的行**同一份数据** ——
//     用它的合计，卡片就永远不会出现「大字说 4437、明细加起来是 3800」这种自相矛盾。
//
// 排除已过期包：上游偶尔会留下「已过到期时间但 remain 仍 > 0」的行，
// 把它们算进合计会让总额偏大（看起来比实际能用的多）。
func cachedRemain(cp cachedCreditPacks, now int64) int64 {
	var sum int64
	for _, p := range cp.Packs {
		if p.ExpireAt > 0 && p.ExpireAt < now {
			continue
		}
		if p.Remain > 0 {
			sum += p.Remain
		}
	}
	return sum
}

// truncateRunes 按**字符**截断（不是按字节）：中文名按字节切会把汉字劈成半个，
// 得到非法 UTF-8，再经 JSON 编码补成 U+FFFD，长度与观感双双失真。
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max])
}

// decodeCreditPacks 反序列化缓存。任何解析失败都退化为「无缓存」——
// 缓存是纯展示增强，绝不能因为它坏了而让账号卡渲染不出来。
func decodeCreditPacks(raw string, at int64) cachedCreditPacks {
	out := cachedCreditPacks{At: at}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return out
	}
	var rows []upstream.CreditPackage
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		return cachedCreditPacks{At: at}
	}
	out.Packs = rows
	return out
}

// cacheCreditPacks 把一次成功查询的明细写入本地缓存（唯一写入口）。
//
// 空输入**什么都不做**：上游偶发返回空列表时，卡片不能因此从「有明细」退化成
// 「没有明细」（旧数据比空数据有用）。写失败只记日志、不影响调用方。
func (a *app) cacheCreditPacks(uid string, packs []upstream.CreditPackage) {
	if uid == "" {
		return
	}
	at := time.Now().Unix()
	blob := encodeCreditPacks(packs, at)
	if blob == "" {
		return
	}
	if err := a.store.SetAccountCreditPacks(uid, blob, at); err != nil {
		log.Printf("WARN: [credit] 明细缓存写入失败 uid=%s: %v", uid, err)
	}
}

// SetAccountCreditPacks 写回明细缓存（仅在上游查询成功时调用）。
//
// 刻意容忍 uid 不存在（返回的 err 由调用方决定是否记日志）：缓存写失败不该影响
// 用户正在进行的查询结果展示。
func (s *Store) SetAccountCreditPacks(uid, raw string, at int64) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	_, err := s.db.Exec(`UPDATE accounts SET credit_packs = ?, credit_packs_at = ? WHERE uid = ?`,
		raw, at, uid)
	return err
}

// AccountCreditPacks 读单个账号的明细缓存（按主键，单次查询）。
func (s *Store) AccountCreditPacks(uid string) cachedCreditPacks {
	var raw string
	var at int64
	err := s.db.QueryRow(`SELECT credit_packs, credit_packs_at FROM accounts WHERE uid = ?`, uid).
		Scan(&raw, &at)
	if err != nil {
		return cachedCreditPacks{}
	}
	return decodeCreditPacks(raw, at)
}

// ======================================================================
// 明细缓存预热（v0.24.9）
// ======================================================================
//
// 为什么需要：明细缓存只有「被人查过一次」的账号才有。而卡片的正确观感是
// **整页都有明细** —— 否则运维每次都要先逐张点一遍，等于把 v0.24.3 的
// 「点开才查」体验原样保留，只是换成一次性补齐。
//
// 为什么敢做（这与封号事故的界线在哪）：
//
//	事故形态是「几十个**并发**出站、几秒内打完同一个出口」。本预热：
//	  - **串行**：一个查完才查下一个（并发恒为 1，比健康检测更保守）；
//	  - 每个账号之间**间隔** creditWarmInterval；
//	  - 每次都过探活总闸（probeGate），失败即跳过、不重试。
//	也就是说它的出站节奏与既有的「检测全部」同强度或更弱，不是新的风险形态。
//
// 刻意**只写明细缓存**，不写池内余额、不解冻、不重算分组 —— 那是健康检测与
// 签到这类"有业务含义的观测"的职责。预热是纯展示补齐，不该顺带改动业务状态。
const creditWarmInterval = 700 * time.Millisecond

// creditWarmReport 预热结果（逐条如实回报，不吞失败）。
type creditWarmReport struct {
	Total   int             `json:"total"`
	OK      int             `json:"ok"`
	Failed  int             `json:"failed"`
	Skipped int             `json:"skipped"`
	Rows    []creditWarmRow `json:"rows"`
	Elapsed float64         `json:"elapsed_sec"`
}

type creditWarmRow struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	OK       bool   `json:"ok"`
	Packs    int    `json:"packs"`
	Error    string `json:"error,omitempty"`
}

// warmupCreditCache 串行预热给定账号（或全部）的明细缓存，返回逐条结果。
func (a *app) warmupCreditCache(uids []string) creditWarmReport {
	started := time.Now()
	rep := creditWarmReport{Rows: make([]creditWarmRow, 0, len(uids))}
	for _, uid := range uids {
		row := creditWarmRow{UID: uid}
		acc, err := a.store.GetAccount(uid)
		if err != nil || acc == nil {
			row.Error = "账号不存在"
			rep.Skipped++
			rep.Rows = append(rep.Rows, row)
			continue
		}
		row.Nickname = acc.Nickname
		au := authFromAccount(acc)
		if au == nil {
			row.Error = "凭证不可用"
			rep.Skipped++
			rep.Rows = append(rep.Rows, row)
			continue
		}
		// 与其它探活类出站共用同一把闸：被拒（30s 内刚查过 / 全局并发已满）
		// 就跳过这个号，**不排队、不重试** —— 跳过是安全的，它的卡上还有旧明细。
		release, gerr := a.probes.acquire(uid)
		if gerr != nil {
			row.Error = gerr.Error()
			rep.Skipped++
			rep.Rows = append(rep.Rows, row)
			continue
		}
		packs, _, _, _, _, perr := a.proxy.uc.ResourcePackages(au)
		release()
		if perr != nil {
			row.Error = perr.Error()
			rep.Failed++
			rep.Rows = append(rep.Rows, row)
		} else {
			a.cacheCreditPacks(uid, packs)
			row.OK = true
			row.Packs = len(packs)
			rep.OK++
			rep.Rows = append(rep.Rows, row)
		}
		if time.Since(started) > 0 && len(rep.Rows) < len(uids) {
			time.Sleep(creditWarmInterval)
		}
	}
	rep.Total = len(uids)
	rep.Elapsed = time.Since(started).Seconds()
	return rep
}

// ======================================================================
// 单号刷新（v0.25.0）：给「账号卡实时显示积分」用
// ======================================================================
//
// 用户的原始要求是「账号卡要实时显示积分情况」。**但不能靠"渲染即出站"实现** ——
// 那正是 2026-10-13 封号事故的根因（几十张卡片同时出站 → 上游判 11140 → 30 个号被误停）。
//
// 所以改成两段式：
//
//	① 卡片渲染：读**缓存**（零出站，v0.24.9）—— 打开页面立刻有数，不会空白；
//	② 补新：**页面可见时**由前端每隔 N 秒调本接口刷**一个**号（串行，等上一个返回再发下一个）。
//
// 与事故形态的关键区别（改这里之前请逐条对照）：
//
//	事故：几十个**并发**、**几秒内**打完、**同一出口**、**无人察觉**；
//	现在：**一次一个**、**间隔 N 秒**、每个号 30 秒内不重复、**受探活总闸**（全局并发 ≤3）、
//	     **只在页面可见时**推进（切走即停），且节奏由人打开页面这一动作驱动。
//
// kv `credit_auto_refresh_sec`：默认 5 秒/号；0 = 关闭自动补新（只留手动按钮）；
// 上限 300（给一个上界，避免误配成 0.001 秒把闸当摆设）。
const (
	kvCreditAutoRefreshSec = "credit_auto_refresh_sec"
	creditRefreshSecDef    = 5
	creditRefreshSecMax    = 300
)

// creditAutoRefreshInterval 解析自动补新间隔（秒）。0 表示关闭。
//
// 抽成纯函数是为了可测：这段"默认值/上限/垃圾输入"的逻辑最容易被后续改动悄悄改坏，
// 而它直接决定出站频率 —— 配错了就是风控事故。
func creditAutoRefreshInterval(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return creditRefreshSecDef
	}
	n := 0
	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil {
		return creditRefreshSecDef
	}
	if n <= 0 {
		return 0
	}
	if n > creditRefreshSecMax {
		return creditRefreshSecMax
	}
	return n
}

func (a *app) creditAutoRefreshSec() int {
	v, _ := a.store.GetKV(kvCreditAutoRefreshSec)
	return creditAutoRefreshInterval(v)
}

// handleCreditRefreshOne 刷新单个账号的明细缓存（POST /api/admin/accounts/credit-refresh）。
//
// 一次只做一个号，**立即返回**：节奏由调用方（前端定时器）掌握，服务端不排队、不堆积。
// 被探活总闸拒（同号 30s 内刚查过 / 全局并发已满）时回 200 + throttled，
// 让前端"跳过这个号、继续下一个"而不是把整轮刷新中断。
func (a *app) handleCreditRefreshOne(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID string `json:"uid"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	uid := strings.TrimSpace(req.UID)
	if uid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少 uid"})
		return
	}
	acc, err := a.store.GetAccount(uid)
	if err != nil || acc == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "账号不存在"})
		return
	}
	au := authFromAccount(acc)
	if au == nil {
		writeJSON(w, http.StatusOK, map[string]any{"uid": uid, "ok": false, "error": "凭证不可用"})
		return
	}
	release, gerr := a.probes.acquire(uid)
	if gerr != nil {
		writeJSON(w, http.StatusOK, map[string]any{"uid": uid, "ok": false, "throttled": true, "error": gerr.Error()})
		return
	}
	defer release()
	packs, _, _, _, _, perr := a.proxy.uc.ResourcePackages(au)
	if perr != nil {
		writeJSON(w, http.StatusOK, map[string]any{"uid": uid, "ok": false, "error": perr.Error()})
		return
	}
	// 只写明细缓存，**不写池内余额**：这是展示层补新，不该改动选号/解冻/分组这些业务状态。
	a.cacheCreditPacks(uid, packs)
	now := time.Now().Unix()
	// 连**行数据**一起回传：前端要就地重画那一张卡（总额 + 明细），
	// 只回一个总数的话它还得再发一次请求 —— 那等于把串行补新变成两倍出站。
	rows := make([]map[string]any, 0, len(packs))
	for _, p := range packs {
		rows = append(rows, map[string]any{
			"name": p.Name, "remain": p.Remain, "used": p.Used,
			"size": p.Size, "expire_at": p.ExpireAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"uid": uid, "ok": true, "packs": len(packs),
		"remain":     cachedRemain(cachedCreditPacks{Packs: packs}, now),
		"resources":  rows,
		"updated_at": now,
	})
}

// handleAdminCreditWarmup 管理侧：串行预热明细缓存（POST /api/admin/accounts/credit-warmup）。
//
// 请求体：`{"uids":["..."]}`；**uids 缺省 = 全部账号**。
//
// 与「批量删除空输入绝不默认全删」那条纪律的关系（两者看似矛盾，其实不同类）：
// 删除**不可逆**，所以空输入必须报错；本接口是**只读查询**，幂等、可回退、
// 不改变任何账号状态，而它的设计目的恰恰是「把整页卡片补齐」——
// 让运维还得先全选一遍纯属多余。所以这里默认全部是刻意的。
func (a *app) handleAdminCreditWarmup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UIDs []string `json:"uids"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	uids := make([]string, 0, len(req.UIDs))
	seen := map[string]bool{}
	for _, u := range req.UIDs {
		u = strings.TrimSpace(u)
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		uids = append(uids, u)
	}
	if len(uids) == 0 {
		accs, err := a.store.ListAccounts()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		for _, acc := range accs {
			uids = append(uids, acc.UID)
		}
	}
	// 上限保护：单次最多 creditWarmMaxUIDs 个。即便被脚本误调用，也不会变成
	// 无界出站（按 700ms 间隔，200 个约 2 分半）。
	const creditWarmMaxUIDs = 200
	if len(uids) > creditWarmMaxUIDs {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "单次最多预热 " + itoa(creditWarmMaxUIDs) + " 个账号",
		})
		return
	}
	if len(uids) == 0 {
		writeJSON(w, http.StatusOK, creditWarmReport{Rows: []creditWarmRow{}})
		return
	}

	rep := a.warmupCreditCache(uids)
	log.Printf("INFO: [credit] 明细预热完成：共 %d，成功 %d，失败 %d，跳过 %d，耗时 %.1fs",
		rep.Total, rep.OK, rep.Failed, rep.Skipped, rep.Elapsed)
	writeJSON(w, http.StatusOK, rep)
}

// CreditPacksByUID 一次性读全部账号的明细缓存（列表页用，避免 N+1 查询）。
func (s *Store) CreditPacksByUID() map[string]cachedCreditPacks {
	out := map[string]cachedCreditPacks{}
	rows, err := s.db.Query(`SELECT uid, credit_packs, credit_packs_at FROM accounts
		WHERE credit_packs <> ''`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var uid, raw string
		var at int64
		if err := rows.Scan(&uid, &raw, &at); err != nil {
			continue
		}
		out[uid] = decodeCreditPacks(raw, at)
	}
	return out
}
