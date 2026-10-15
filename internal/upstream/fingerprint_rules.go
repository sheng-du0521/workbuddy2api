// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// fingerprint_rules.go 出站指纹规则的**线上可扩展层**（R4，v0.28.0）。
//
// ## 为什么需要这一层
//
// sanitize.go 里的黑名单是**编译期常量**：发现新触发词 → 人工察觉 → 改代码 →
// 跑测试 → 发版。而指纹由上游随时更新，于是这条链路是**唯一还绑在发版节奏上的
// 风控能力**——运维看见了样本却只能等下一个版本。
//
// 本层把内置表**原样保留**（快照测试仍逐字节锁死，不因本功能而漂移），在其上
// 叠加一层运行期规则：
//
//	生效规则 = 内置表（编译期，只读） + 自定义规则（运行期，可增删）
//
// 两者**只叠加、不覆盖**：内置规则是实测验证过的最小改写，"用自定义规则关掉某条
// 内置规则"这样的能力等于给运维一个不知道该不该按的开关 —— 故不提供。
//
// ## 两种规则形态（刻意只做两种）
//
//	literal —— 逐字替换（等价内置 sanitizeRewrites 的形态："只改一个词"）。
//	           最安全：不会误伤预期外的文本，**推荐优先使用**。
//	regex   —— 正则替换，Replace 为空即整段删除（等价内置 sanitizeHdrRe 的形态：
//	           处理"键值段"这类长度不定的指纹）。
//
// 不做"任意正则随便挂"的通用能力，而是给足约束（见 validateFingerprintRule）：
// 长度上限、条数上限、拒绝**匹配空串**的正则（空匹配会在每个字符位置插入替换串，
// 能把一个请求体撑大几个数量级）。
//
// ## 三道安全网（因为一条坏规则会污染**所有**出站请求）
//
//  1. 保存即校验：非法正则 / 空匹配 / 过短 literal 直接在写入前被拒（400），
//     不会出现"存进去了但没生效"这种需要翻日志才发现的状态；
//  2. 过宽自检：新规则集必须让 `fingerprintSafetyCorpus` 的**普通文本原样通过**
//     （与内置表单独跑的结果逐字一致）。否则一条 `the` → `t-he` 之类的规则会让
//     所有提示词被改写，表现为"加了规则之后模型开始答非所问"，而没人会想到是
//     脱敏层干的；
//  3. 可回放：`FingerprintRuleHits` 对**真实被拦样本**统计每条规则的命中数 ——
//     "这条规则到底有没有用"从猜测变成可测量的数字（见 webapp 的 replay 接口）。
//
// ## 并发与热路径成本
//
// 规则集是不可变值，整体原子替换（atomic.Pointer）。热路径（hasFingerprint /
// sanitizeText）只做一次原子读；**没有自定义规则时该指针为 nil**，成本是一次
// nil 判断 —— 对每个消息片段都会走的预检来说，这个量级必须守住。
package upstream

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// 规则形态与上限。
const (
	// FingerprintRuleLiteral 逐字替换：Match 按字面量匹配，Replace 替换之。
	FingerprintRuleLiteral = "literal"
	// FingerprintRuleRegex 正则替换：Match 是 RE2 正则，Replace 支持 $1 反向引用，
	// Replace 为空即整段删除。
	//
	// 用 Go 的 regexp（RE2）而非 PCRE：RE2 无回溯，最坏也是线性时间，
	// 因此**不存在正则回溯打挂进程**这一风险 —— 这是敢让运维自己写正则的前提。
	FingerprintRuleRegex = "regex"

	// MaxFingerprintRules 自定义规则条数上限。
	//
	// 200 是"远超实际需要"的量级：真实指纹是**有限且少**的（内置表 5 改 + 3 正则
	// 覆盖了长期积累的全部实测触发词）。设上限只是防住"脚本批量灌规则"把热路径拖慢。
	MaxFingerprintRules = 200
	// maxFingerprintPatternBytes Match 串的字节上限（512 字节约 170 个汉字）。
	maxFingerprintPatternBytes = 512
	// maxFingerprintReplaceBytes Replace 串的字节上限。
	maxFingerprintReplaceBytes = 512
	// minFingerprintLiteralRunes literal 形态 Match 的**最少字符数**。
	//
	// 4 是刻意留的下限：更短的串（"the" / "1" / "cc"）几乎必然出现在普通文本里，
	// 一旦命中就是全量改写。真需要处理短串的场合应当用 regex 写足上下文。
	minFingerprintLiteralRunes = 4
	// minFingerprintRegexBytes regex 形态 Match 的最少字节数。
	//
	// 比 literal 更松（3）：正则本身可以很紧凑（`cc_\w+=`），它的精确性来自模式
	// 而非长度；空匹配另有专门检查（那才是真正的危险形态）。
	minFingerprintRegexBytes = 3
)

// FingerprintRule 一条运维自定义的指纹规则。
//
// ID 由 (Kind, Match) 确定性派生（见 fingerprintRuleID），**不接受调用方指定**：
// 规则的身份就是"匹配什么"，这样 upsert 天然幂等，也不会出现"同一个匹配串存了两条"。
type FingerprintRule struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Match   string `json:"match"`
	Replace string `json:"replace"`
	Note    string `json:"note,omitempty"`
	// Source 规则来源："manual"（人工新增）/ "sample"（从被拦样本采纳）。
	// 只用于界面标注 —— 它回答"这条规则当初是因为什么加的"，是几个月后唯一的线索。
	Source string `json:"source,omitempty"`
	// AddedAt 入库时刻（unix 秒）。0 表示未知（历史数据）。
	AddedAt int64 `json:"added_at,omitempty"`
	// Builtin 标记该条来自内置表（只读视图用；自定义规则恒 false）。
	Builtin bool `json:"builtin,omitempty"`
}

// fingerprintRuleID 规则身份：(形态, 匹配串) 的确定性摘要。
//
// 为什么不用随机 ID / 自增：规则要跨进程持久化，也要跨版本对得上。
// 确定性 ID 让"改 Replace 不产生新规则"、"同一匹配串不会存两条"这两件事
// 由数据结构本身保证，而不是靠调用方记得去重。
func fingerprintRuleID(kind, match string) string {
	sum := sha256.Sum256([]byte(kind + "\x1f" + match))
	return "fp-" + hex.EncodeToString(sum[:])[:12]
}

// validateFingerprintRule 校验并归一化一条规则。返回的规则 ID 一定已填好。
func validateFingerprintRule(r FingerprintRule) (FingerprintRule, error) {
	r.Kind = strings.ToLower(strings.TrimSpace(r.Kind))
	switch r.Kind {
	case FingerprintRuleLiteral, FingerprintRuleRegex:
	default:
		return r, fmt.Errorf("未知规则形态 %q（仅支持 %s / %s）",
			r.Kind, FingerprintRuleLiteral, FingerprintRuleRegex)
	}
	if r.Match == "" {
		return r, fmt.Errorf("匹配串不能为空")
	}
	if len(r.Match) > maxFingerprintPatternBytes {
		return r, fmt.Errorf("匹配串过长（%d 字节，上限 %d）", len(r.Match), maxFingerprintPatternBytes)
	}
	if len(r.Replace) > maxFingerprintReplaceBytes {
		return r, fmt.Errorf("替换串过长（%d 字节，上限 %d）", len(r.Replace), maxFingerprintReplaceBytes)
	}
	if r.Replace == r.Match {
		// 不进不出等于没有规则，但会常驻在列表里让人以为"已经处理过了"。
		return r, fmt.Errorf("替换串与匹配串相同（该规则不会产生任何变化）")
	}
	switch r.Kind {
	case FingerprintRuleLiteral:
		if n := len([]rune(r.Match)); n < minFingerprintLiteralRunes {
			return r, fmt.Errorf("逐字规则的匹配串至少 %d 个字符（当前 %d）——"+
				"更短的串几乎必然出现在普通文本里，会改写**所有**请求", minFingerprintLiteralRunes, n)
		}
	case FingerprintRuleRegex:
		// 检查顺序刻意是「编译 → 空匹配 → 长度」：空匹配是最危险也最难自查的形态，
		// 让它优先于长度报错，运维拿到的才是**真正要改的那件事**的错误文案
		// （否则一条 `x*` 会先被告知"太短"，改长了反而变成更隐蔽的祸）。
		re, err := regexp.Compile(r.Match)
		if err != nil {
			return r, fmt.Errorf("正则无法编译：%v", err)
		}
		// ⚠️ 空匹配是本层唯一能"自伤"的形态：ReplaceAllString 会把替换串插到
		// 每一个字符位置（`a` → `XaX`），请求体可被放大几个数量级。
		if re.MatchString("") {
			return r, fmt.Errorf("该正则能匹配空串（如 `.*` / `x?` / `(a|)`）：" +
				"空匹配会在每个字符位置插入替换串，把请求体放大数倍。请补足必匹配的内容")
		}
		if len(r.Match) < minFingerprintRegexBytes {
			return r, fmt.Errorf("正则规则的匹配串至少 %d 个字节（当前 %d）", minFingerprintRegexBytes, len(r.Match))
		}
	}
	// Note / Source 只做长度收敛（它们是给人看的，不参与匹配）。
	if len(r.Note) > 300 {
		r.Note = string([]rune(r.Note)[:300])
	}
	if r.Source == "" {
		r.Source = "manual"
	}
	if r.AddedAt <= 0 {
		r.AddedAt = time.Now().Unix()
	}
	r.ID = fingerprintRuleID(r.Kind, r.Match)
	return r, nil
}

// compiledFpRule 一条编译好的规则（match/apply 共用，保证"命中判定"与"实际改写"
// 用的是同一个模式 —— 两者分叉会出现"预检说命中、实际什么也没改"的静默缺陷）。
type compiledFpRule struct {
	rule FingerprintRule
	lit  string // literal 形态的匹配串
	re   *regexp.Regexp
}

func (c compiledFpRule) match(text string) bool {
	if c.re != nil {
		return c.re.MatchString(text)
	}
	return strings.Contains(text, c.lit)
}

func (c compiledFpRule) apply(text string) string {
	if c.re != nil {
		return c.re.ReplaceAllString(text, c.rule.Replace)
	}
	return strings.ReplaceAll(text, c.lit, c.rule.Replace)
}

// compiledFingerprintRules 不可变的规则集（原子替换的单位）。
type compiledFingerprintRules struct {
	// ordered 按**应用顺序**排列：literal 全部在前、regex 在后。
	//
	// 顺序有语义：literal 是"只改一个词"的最小改动，先做最小改动，后面的 regex
	// 才有机会处理剩余的键值段（与内置 rewrites → strips 同序）。
	// 同为 literal 时按入库顺序（稳定），保证同一规则集每次行为一致。
	ordered []compiledFpRule
	// list 归一化后的原始规则（读回给管理接口用；顺序同 ordered）。
	list []FingerprintRule
}

// activeFingerprintRules 当前生效的自定义规则集。nil = 没有自定义规则（默认）。
var activeFingerprintRules atomic.Pointer[compiledFingerprintRules]

// compileFingerprintRules 校验 + 编译一批规则（同 ID 后者覆盖前者；literal 前置）。
func compileFingerprintRules(rules []FingerprintRule) (*compiledFingerprintRules, error) {
	if len(rules) == 0 {
		return nil, nil
	}
	if len(rules) > MaxFingerprintRules {
		return nil, fmt.Errorf("规则条数超上限（%d > %d）", len(rules), MaxFingerprintRules)
	}
	// 先按 ID 去重（后者覆盖前者），再按 kind 稳定分组。
	byID := make(map[string]FingerprintRule, len(rules))
	order := make([]string, 0, len(rules))
	for _, r := range rules {
		n, err := validateFingerprintRule(r)
		if err != nil {
			return nil, err
		}
		if _, ok := byID[n.ID]; !ok {
			order = append(order, n.ID)
		}
		byID[n.ID] = n
	}
	norm := make([]FingerprintRule, 0, len(order))
	for _, id := range order {
		norm = append(norm, byID[id])
	}
	// 稳定排序：literal 在前。sort.SliceStable 保住同类之间的入库顺序（可预期）。
	sort.SliceStable(norm, func(i, j int) bool {
		return norm[i].Kind == FingerprintRuleLiteral && norm[j].Kind != FingerprintRuleLiteral
	})

	out := &compiledFingerprintRules{list: norm, ordered: make([]compiledFpRule, 0, len(norm))}
	for _, r := range norm {
		c := compiledFpRule{rule: r}
		if r.Kind == FingerprintRuleRegex {
			re, err := regexp.Compile(r.Match)
			if err != nil { // 理论上不可达（上面已校验），但编译失败必须显式返回而不是留一条空规则
				return nil, fmt.Errorf("正则编译失败：%v", err)
			}
			c.re = re
		} else {
			c.lit = r.Match
		}
		out.ordered = append(out.ordered, c)
	}
	return out, nil
}

// fingerprintSafetyCorpus 必须**原样通过**的普通文本集（过宽自检用）。
//
// 选样标准：覆盖中英文、代码、SQL、JSON —— 也就是真实请求里最常见的几种形态。
// 一条规则若能改动其中任何一条，它在生产里就一定会改动大量正常请求。
var fingerprintSafetyCorpus = []string{
	"你好，帮我写一个 Go 的快速排序，要求支持泛型。",
	"Explain how TCP/IP works in three paragraphs, then show me a curl example.",
	"请把这段代码重构一下：\nfunc main() {\n\tprintln(\"hello\")\n}",
	"SELECT id, name FROM users WHERE created_at > '2026-01-01' ORDER BY id DESC;",
	"我们下周一开评审会，请准备三页材料：背景、方案、风险。",
	`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`,
}

// checkRulesNotOverBroad 过宽自检：加入自定义规则后，安全语料必须**逐字不变**
// （基准是"只有内置规则时"的输出，因此这里只检验自定义规则带来的增量改动）。
func checkRulesNotOverBroad(cr *compiledFingerprintRules) error {
	if cr == nil || len(cr.ordered) == 0 {
		return nil
	}
	for _, text := range fingerprintSafetyCorpus {
		base := sanitizeTextWith(nil, text)
		got := sanitizeTextWith(cr, text)
		if got == base {
			continue
		}
		for _, c := range cr.ordered {
			if c.match(text) {
				return fmt.Errorf("规则 %s（%s %q）会命中普通文本，请改用更精确的匹配串",
					c.rule.ID, c.rule.Kind, c.rule.Match)
			}
		}
		// 逐条都不命中却结果不同 —— 分类上不可能发生，留一条兜底错误避免静默放行。
		return fmt.Errorf("自定义规则改动了普通文本，但未定位到具体规则（请人工复核）")
	}
	return nil
}

// SetFingerprintRules 安装自定义指纹规则集（传入 nil / 空切片即清空）。
//
// 这是**唯一的写入口**：校验 → 编译 → 过宽自检 → 原子替换。任一步失败都保持
// 原有规则集不变（不会出现"校验失败但已经半装上去"的中间态）。
func SetFingerprintRules(rules []FingerprintRule) error {
	cr, err := compileFingerprintRules(rules)
	if err != nil {
		return err
	}
	if err := checkRulesNotOverBroad(cr); err != nil {
		return err
	}
	activeFingerprintRules.Store(cr) // cr 可能为 nil —— 正是"清空"的语义
	return nil
}

// FingerprintRules 返回当前生效的自定义规则（副本，调用方可随意改）。
func FingerprintRules() []FingerprintRule {
	cr := activeFingerprintRules.Load()
	if cr == nil {
		return []FingerprintRule{}
	}
	out := make([]FingerprintRule, len(cr.list))
	copy(out, cr.list)
	return out
}

// BuiltinFingerprintRules 内置表的管理侧只读视图（把编译期常量摊平成同一形状）。
//
// ⚠️ 这里**只读**：内置规则的增删仍然只走"改代码 + 快照测试 + 发版"。
// 之所以要暴露给界面，是因为运维需要知道"哪些是本来就有的、哪些是我加的"——
// 否则会把内置规则也当成自己的规则去删（然后发现删不掉，或者更糟：以为删掉了）。
func BuiltinFingerprintRules() []FingerprintRule {
	out := make([]FingerprintRule, 0, len(sanitizeRewrites)+4)
	for _, rw := range sanitizeRewrites {
		out = append(out, FingerprintRule{
			ID: fingerprintRuleID(FingerprintRuleLiteral, rw[0]),
			// 用 literal 形态呈现（尽管内置表内部是"逐字替换"的另一种写法）：形状对齐
			// 是为了让运维一眼看出"我该用哪种形态写自己的规则"。
			Kind: FingerprintRuleLiteral, Match: rw[0], Replace: rw[1],
			Note: "内置（编译期常量，只读）", Source: "builtin", Builtin: true,
		})
	}
	for _, p := range []struct{ re, note string }{
		{sanitizeHdrRe.String(), "内置：整段删除 x-anthropic-billing-header 键值"},
		{sanitizeKvRe.String(), "内置：循环清理尾随 cc_xxx=...; 键值"},
		{sanitizeBareHdrRe.String(), "内置：缩写残留的裸键名（无冒号形态）"},
	} {
		out = append(out, FingerprintRule{
			ID: fingerprintRuleID(FingerprintRuleRegex, p.re),
			// 空替换串 = 删除；对 bareHeader 那条真实语义是缩写，但它的替换发生在本表
			// 之外的固定流程里，故这里如实标为"删除形态"，不做与实现不一致的美化。
			Kind: FingerprintRuleRegex, Match: p.re, Replace: "",
			Note: p.note, Source: "builtin", Builtin: true,
		})
	}
	return out
}

// SanitizeText 净化一段文本，返回（结果, 是否发生变化）。
//
// 供管理侧的"试跑"使用：让运维在**保存前**看到这条规则对一段真实文本做了什么。
// 走的是与生产完全相同的管线（内置规则 + 已安装的自定义规则）。
func SanitizeText(text string) (string, bool) {
	out := sanitizeText(text)
	return out, out != text
}

// HasFingerprint 预检结果（试跑界面用它区分"命中但改写后恰好相同"与"根本没命中"）。
func HasFingerprint(text string) bool { return hasFingerprint(text) }

// SanitizeTextWith 用一份**候选**规则集净化，返回（结果, 是否变化, 错误）。
//
// 与 SetFingerprintRules 的区别是它**不改动全局状态**：用于"保存前预演"。
// 之所以需要这个能力，是因为试跑必须在规则真正生效之前完成 —— 否则一次失败的实验
// 会被真实流量承担（生产上的表现是"上游开始拦我的请求"，而不是"保存报错"）。
func SanitizeTextWith(rules []FingerprintRule, text string) (string, bool, error) {
	cr, err := compileFingerprintRules(rules)
	if err != nil {
		return text, false, err
	}
	out := sanitizeTextWith(cr, text)
	return out, out != text, nil
}

// BuiltinSanitizeText 只用**内置**规则净化（不含任何自定义规则）。
//
// 用途是把功劳分清楚：一段文本被改写，到底是因为内置表还是因为运维新加的规则。
// 没有这个对照，运维无法判断"我加的规则是不是多余的"。
func BuiltinSanitizeText(text string) (string, bool) {
	out := sanitizeTextWith(nil, text)
	return out, out != text
}

// FingerprintRuleHits 对一批文本回放给定规则集，返回「每条规则命中了多少条文本」。
//
// 用**候选**规则集（而不是已安装的那套）评估，是因为它的用途是"保存前先看效果"：
// 若先装再评，评估期间生产流量已经在用一条没验证过的规则了。
//
// 返回的 map 以规则 ID 为键；未命中的规则也会出现在 map 里（值为 0）——
// "这条规则一条样本都没命中"与"这条规则没被评估"是两件事，界面必须能区分。
func FingerprintRuleHits(rules []FingerprintRule, texts []string) (map[string]int, error) {
	cr, err := compileFingerprintRules(rules)
	if err != nil {
		return nil, err
	}
	hits := map[string]int{}
	if cr == nil {
		return hits, nil
	}
	for _, c := range cr.ordered {
		hits[c.rule.ID] = 0
	}
	for _, t := range texts {
		if t == "" {
			continue
		}
		for _, c := range cr.ordered {
			if c.match(t) {
				hits[c.rule.ID]++
			}
		}
	}
	return hits, nil
}
