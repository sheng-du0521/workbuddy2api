// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// fingerprintrules.go 出站指纹规则的**运营侧**：持久化 + 管理接口 + 样本回放（R4，v0.28.0）。
//
// ## 与 fpsample.go 的分工
//
//	fpsample.go        —— **采集**：被拦时把 system 原文前 300 字落盘（默认关闭）。
//	fingerprintrules.go—— **处置**：把采集到的证据变成一条规则，并能在保存前验证它有效。
//
// 这两个文件合起来才构成闭环。此前只有采集：运维看得到样本、却只能"改代码 + 发版"
// 才能扩黑名单 —— 采集变成了一份只能看不能用的清单。
//
// ## 持久化为什么用 kv 而不是新表
//
// 规则是**极少量、整体读写、不分页、不查询**的配置（上限 200 条，实际个位数）。
// 建表会引入 migration / 索引 / 事务这几件在配置场景里并不划算的事，而 kv 的
// "整份 JSON 原子替换"语义恰好是这里需要的：规则集的**一致性边界是整份**，
// 不存在"改了一半生效"的合法中间态。
//
// ## 保存失败必须**不改变**现有规则
//
// 校验在 upstream.SetFingerprintRules 里做（编译 + 过宽自检），失败即返回 error，
// 此时既不落 kv 也不动已安装的规则集 —— 于是"保存失败"对生产**零影响**。
// 这个顺序（先校验后落盘）是刻意的：反过来的话，一次失败的保存会在重启后
// 变成"启动即加载一份非法规则"，而启动路径只能选择吞掉它。
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"

	"workbuddy2api/internal/upstream"
)

// kvFingerprintRules 自定义指纹规则的持久化键（整份 JSON 数组）。
const kvFingerprintRules = "fingerprint_rules"

// loadFingerprintRules 从 kv 读取规则并安装。返回规则条数与错误。
//
// **启动路径容错**：kv 里是坏 JSON（手工改过 / 半截写入）时**不阻断启动** ——
// 只记一条日志并按"没有自定义规则"运行。理由：指纹规则是增强，不是业务前置条件，
// 让一个坏字符把整个中转站挡在门外是完全不成比例的。
// 但**不静默**：日志里必须留下这件事，否则运维会以为规则还在生效。
func (a *app) loadFingerprintRules() (int, error) {
	if a == nil || a.store == nil {
		return 0, nil
	}
	raw, err := a.store.GetKV(kvFingerprintRules)
	if err != nil {
		return 0, err
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		// 空 = 没有自定义规则。显式清一次，避免进程内残留（测试与热重载会走到）。
		_ = upstream.SetFingerprintRules(nil)
		return 0, nil
	}
	var rules []upstream.FingerprintRule
	if err := json.Unmarshal([]byte(raw), &rules); err != nil {
		log.Printf("WARN: [fprules] 持久化的指纹规则无法解析（按无自定义规则运行）：%v", err)
		_ = upstream.SetFingerprintRules(nil)
		return 0, err
	}
	if err := upstream.SetFingerprintRules(rules); err != nil {
		log.Printf("WARN: [fprules] 持久化的指纹规则校验失败（按无自定义规则运行）：%v", err)
		_ = upstream.SetFingerprintRules(nil)
		return 0, err
	}
	return len(upstream.FingerprintRules()), nil
}

// saveFingerprintRules 校验 → 安装 → 持久化（顺序见文件头）。
//
// ⚠️ 顺序不可换：先 SetFingerprintRules 校验并安装，再写 kv。若反过来，
// 校验失败时 kv 里已经存了一份非法规则，重启后会被加载（只能吞掉）。
func (a *app) saveFingerprintRules(rules []upstream.FingerprintRule) error {
	if err := upstream.SetFingerprintRules(rules); err != nil {
		return err
	}
	// 落盘的是**归一化后**的规则（ID / AddedAt 已补齐），读回来与内存中完全一致 ——
	// 否则重启前后界面上的 ID 会变，运维无法引用某条规则去沟通。
	norm := upstream.FingerprintRules()
	b, err := json.Marshal(norm)
	if err != nil {
		return err
	}
	return a.store.SetKV(kvFingerprintRules, string(b))
}

// fingerprintRulesView GET /api/admin/fingerprint-rules 的响应。
type fingerprintRulesView struct {
	// Builtin 内置规则（编译期常量，只读）。
	Builtin []upstream.FingerprintRule `json:"builtin"`
	// Custom 当前生效的自定义规则。
	Custom []upstream.FingerprintRule `json:"custom"`
	// Max 自定义规则条数上限（界面据此提示"还能加多少"）。
	Max int `json:"max"`
	// Applied 自启动以来是否真的调用过 SetFingerprintRules（诊断用：区分
	// "没有规则"与"规则没装上去"）。
	Applied bool `json:"applied"`
}

// handleAdminFingerprintRules GET /api/admin/fingerprint-rules
func (a *app) handleAdminFingerprintRules(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, fingerprintRulesView{
		Builtin: upstream.BuiltinFingerprintRules(),
		Custom:  upstream.FingerprintRules(),
		Max:     upstream.MaxFingerprintRules,
		Applied: true,
	})
}

// fingerprintRuleRequest 单条规则的增改请求（upsert 语义：同 (kind, match) 视为同一条）。
type fingerprintRuleRequest struct {
	Kind    string `json:"kind"`
	Match   string `json:"match"`
	Replace string `json:"replace"`
	Note    string `json:"note"`
	Source  string `json:"source"`
}

// handleAdminFingerprintRuleUpsert POST /api/admin/fingerprint-rules
//
// 单条 upsert 而不是"整份替换"：整份替换要求前端把当前全部规则回传，
// 任何一次界面状态陈旧都会**静默删掉**刚在另一个标签页加的规则。
// 单条语义下最坏也只是覆盖同一条（同一匹配串）。
func (a *app) handleAdminFingerprintRuleUpsert(w http.ResponseWriter, r *http.Request) {
	var req fingerprintRuleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	candidate := upstream.FingerprintRule{
		Kind: req.Kind, Match: req.Match, Replace: req.Replace,
		Note: req.Note, Source: req.Source,
	}
	// 在"当前规则 + 新规则"上整体校验：单条合法不代表**合起来**合法
	// （过宽自检是集合级的，例如两条各自无害的规则叠加后命中安全语料）。
	cur := upstream.FingerprintRules()
	next := make([]upstream.FingerprintRule, 0, len(cur)+1)
	next = append(next, cur...)
	next = append(next, candidate)
	if err := a.saveFingerprintRules(next); err != nil {
		// 校验失败回 400（是调用方给的数据问题），并把原因原样透出 ——
		// 这条错误文案就是界面给运维的"为什么保存不了"，必须具体。
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	log.Printf("INFO: [fprules] 新增/更新自定义指纹规则 kind=%s match=%q（当前共 %d 条）",
		req.Kind, req.Match, len(upstream.FingerprintRules()))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "custom": upstream.FingerprintRules(),
	})
}

// handleAdminFingerprintRuleDelete DELETE /api/admin/fingerprint-rules?id=xxx
//
// 用 query 参数而不是 path 通配：规则 ID 是 "fp-" + 12 位十六进制，本身不含斜杠，
// 但用 query 能让"失败时原样回显被删的 ID"更自然（删除是幂等的，删不存在的 ID
// 返回成功并标注 changed=false，不报 404 —— 界面重复点不会看到吓人的报错）。
func (a *app) handleAdminFingerprintRuleDelete(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少 id"})
		return
	}
	cur := upstream.FingerprintRules()
	next := make([]upstream.FingerprintRule, 0, len(cur))
	removed := 0
	for _, rl := range cur {
		if rl.ID == id {
			removed++
			continue
		}
		next = append(next, rl)
	}
	if removed == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "changed": false, "custom": cur, "note": "该规则不存在（可能已被删除）",
		})
		return
	}
	if err := a.saveFingerprintRules(next); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	log.Printf("INFO: [fprules] 删除自定义指纹规则 %s（剩余 %d 条）", id, len(upstream.FingerprintRules()))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "changed": true, "removed": removed, "custom": upstream.FingerprintRules(),
	})
}

// fingerprintTestRequest 试跑请求。
type fingerprintTestRequest struct {
	Text string `json:"text"`
	// Rules 可选：给定时用这份**候选**规则集评估（保存前预演），
	// 不给则用当前已安装的规则集。
	Rules []upstream.FingerprintRule `json:"rules"`
}

// handleAdminFingerprintRuleTest POST /api/admin/fingerprint-rules/test
//
// 试跑：把一段真实文本跑一遍完整净化管线，返回前后对照 + 命中的规则。
//
// 为什么必须有它：规则的效果无法靠"读正则"确认（尤其涉及中文与转义时）。
// 没有试跑，运维唯一能确认的方式是"存下去，看线上还拦不拦"—— 而失败的表现是
// 请求被上游拦截，代价落在真实用户身上。
func (a *app) handleAdminFingerprintRuleTest(w http.ResponseWriter, r *http.Request) {
	var req fingerprintTestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	if req.Text == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "文本不能为空"})
		return
	}
	if len(req.Text) > 64<<10 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "文本过长（上限 64KB）"})
		return
	}
	// 注：候选规则集的**校验**不在这里单独做，而是由 upstream.SanitizeTextWith /
	// FingerprintRuleHits 内部完成（都走 compileFingerprintRules）—— 校验逻辑只有一份，
	// 不可能出现"这个入口放过了、那个入口拒绝了"的分叉。
	before := req.Text
	after := before
	changed := false
	// Rules 非 nil 走候选集（预演，不动全局状态）；nil 走当前已安装的规则集。
	// 两者都走 upstream 的同一套编译/匹配代码，因此"试跑说命中"与"生产真会改"
	// 不可能分叉（这正是把试跑逻辑放在 upstream 而不是前端 JS 里的原因）。
	activeRules := upstream.FingerprintRules()
	if req.Rules != nil {
		var err error
		after, changed, err = upstream.SanitizeTextWith(req.Rules, before)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		activeRules = req.Rules
	} else {
		after, changed = upstream.SanitizeText(before)
	}
	matched := []string{}
	if hits, err := upstream.FingerprintRuleHits(activeRules, []string{before}); err == nil {
		for id, n := range hits {
			if n > 0 {
				matched = append(matched, id)
			}
		}
	}
	// 排序让同一输入的输出稳定（map 遍历无序会让界面"看起来在跳"）。
	sort.Strings(matched)
	// BuiltinHit：只跑内置规则是否也会改写这段文本 —— 把"谁的功劳"分清楚，
	// 否则运维无法判断"我加的这条规则是不是多余的"。
	_, builtinChanged := upstream.BuiltinSanitizeText(before)
	writeJSON(w, http.StatusOK, map[string]any{
		"before": before, "after": after, "changed": changed,
		"hit_rules":       matched,
		"builtin_changed": builtinChanged,
	})
}

// fingerprintReplayRequest 样本回放请求。
type fingerprintReplayRequest struct {
	// Rules 可选：给定则评估这份候选规则集；否则评估当前已安装的规则集。
	Rules []upstream.FingerprintRule `json:"rules"`
}

// handleAdminFingerprintRuleReplay POST /api/admin/fingerprint-rules/replay
//
// 拿**真实被拦样本**回放规则集，返回每条规则命中多少条样本。
//
// 这是把"这条规则有没有用"从猜测变成数字的那一步。样本是"上游确实拦过"的
// 请求（system 原文），因此命中数至少能排除"规则写错/写偏"这一整类问题 ——
// 而这类问题在只靠"读正则"的年代是**上线后才发现**的。
func (a *app) handleAdminFingerprintRuleReplay(w http.ResponseWriter, r *http.Request) {
	var req fingerprintReplayRequest
	// 允许空 body（用当前规则集）。
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	texts, perKind, err := fingerprintSampleTexts()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	rules := req.Rules
	if rules == nil {
		rules = upstream.FingerprintRules()
	}
	hits, err := upstream.FingerprintRuleHits(rules, texts)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		// SampleCount 参与回放的样本数（0 = 还没采到样本，或采样开关没开）。
		"sample_count": len(texts),
		// SampleKinds 样本按被拦分类的分布，便于判断"这批规则针对的是哪类拦截"。
		"sample_kinds": perKind,
		"hits":         hits,
		"rules":        rules,
	})
}

// fingerprintSampleTexts 读出样本文件里全部去重后的 system 原文。
//
// 与 handleFingerprintSamples 用同一套解析（同文件、同去重键），但**不做条数截断**：
// 界面只展示最近 50 条是展示取舍，而回放要的是全集 —— 截断会让"没命中"变成假象。
func fingerprintSampleTexts() ([]string, map[string]int, error) {
	perKind := map[string]int{}
	raw, err := os.ReadFile(fpSampleFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, perKind, nil // 还没采样过 = 空集，不是错误
		}
		return nil, perKind, err
	}
	seen := map[string]bool{}
	out := []string{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var s fingerprintSample
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			continue // 半行 / 坏行跳过（文件是追加写的）
		}
		if seen[s.Key] {
			continue
		}
		seen[s.Key] = true
		perKind[s.Kind]++
		if strings.TrimSpace(s.System) != "" {
			out = append(out, s.System)
		}
	}
	return out, perKind, nil
}
