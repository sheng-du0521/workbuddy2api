// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package upstream

import (
	"strings"
	"testing"
)

// 本文件是 v0.28.0（R4：指纹规则在线扩表）的回归测试。
//
// ⚠️ 必须放在**包内**（package upstream）：被测的 sanitizeText / hasFingerprint 是包私有，
// 而它们正是"规则真的接进管线了"的唯一观察面 —— 只测 SetFingerprintRules 的返回值
// 会漏掉"规则装上了但热路径没读它"这一个最关键的缺陷形态。
//
// ⚠️ 全局状态：自定义规则集是**包级指针**。每个改动它的用例都必须
// `defer resetFpRules(t)`，否则会污染同包其它文件的用例（Go 同包默认串行执行，
// 但串行不等于互不影响 —— 上一条用例留下的规则会改写下一条的输入）。
func resetFpRules(t *testing.T) {
	t.Helper()
	if err := SetFingerprintRules(nil); err != nil {
		t.Fatalf("重置自定义规则失败：%v", err)
	}
	t.Cleanup(func() { _ = SetFingerprintRules(nil) })
}

// litRule 构造一条 literal 规则（免去到处写 Kind）。
func litRule(match, replace string) FingerprintRule {
	return FingerprintRule{Kind: FingerprintRuleLiteral, Match: match, Replace: replace}
}

// TestFingerprintRuleIDDeterministic 规则身份由 (kind, match) 决定。
//
// 这条性质支撑了两件事：改 Replace 不产生新规则；同一匹配串不会存两条。
// 若哪天换成随机 ID，upsert 会静默变成"每次保存都新增一条"，列表迅速堆满。
func TestFingerprintRuleIDDeterministic(t *testing.T) {
	a := litRule("aaaa-bbbb-cccc", "aaaa-bbbb-cc-cc")
	b := litRule("aaaa-bbbb-cccc", "改写成了完全不同的另一串")
	if idOf(t, a) != idOf(t, b) {
		t.Errorf("同一 (kind, match) 必须得到同一 ID：%s != %s", idOf(t, a), idOf(t, b))
	}
	// 形态不同即不同规则（同一串在 literal 与 regex 下语义不同）。
	c := FingerprintRule{Kind: FingerprintRuleRegex, Match: "aaaa-bbbb-cccc", Replace: "x"}
	if idOf(t, a) == idOf(t, c) {
		t.Errorf("literal 与 regex 的同一个匹配串不应共用 ID")
	}
	// 匹配串变了就是另一条。
	d := litRule("aaaa-bbbb-cccd", "x")
	if idOf(t, a) == idOf(t, d) {
		t.Errorf("匹配串不同必须是不同规则")
	}
}

// idOf 借由 SetFingerprintRules 的归一化取回确定性 ID。
func idOf(t *testing.T, r FingerprintRule) string {
	t.Helper()
	defer resetFpRules(t)
	if err := SetFingerprintRules([]FingerprintRule{r}); err != nil {
		t.Fatalf("SetFingerprintRules: %v", err)
	}
	got := FingerprintRules()
	if len(got) != 1 {
		t.Fatalf("期望 1 条规则，得到 %d", len(got))
	}
	return got[0].ID
}

// TestFingerprintRuleValidation 校验是保存前的唯一闸门 —— 每一条都必须给出具体原因。
//
// 为什么这些边界值得逐条钉住：坏规则不会立刻报错，它表现为"某些请求开始失败"，
// 而且**只有出站时才发现**。所以"能存进去"这件事必须等价于"能用"。
func TestFingerprintRuleValidation(t *testing.T) {
	cases := []struct {
		name     string
		rule     FingerprintRule
		wantFail bool
		// wantErr 错误文案必须包含的子串（断言"因为什么失败"，而不只是"失败了"）。
		wantErr string
	}{
		{name: "literal 正常", rule: litRule("some-fingerprint-sentence", "some-fingerprint sent-ence")},
		{name: "regex 正常", rule: FingerprintRule{Kind: FingerprintRuleRegex, Match: `secret_[a-z]+=x`, Replace: ""}},
		{name: "regex 支持反向引用", rule: FingerprintRule{Kind: FingerprintRuleRegex, Match: `(tok)_v(\d+)`, Replace: "$1-v$2"}},
		{name: "未知形态", rule: FingerprintRule{Kind: "sql", Match: "abcdef", Replace: "x"}, wantFail: true, wantErr: "未知规则形态"},
		{name: "空匹配串", rule: FingerprintRule{Kind: FingerprintRuleLiteral, Match: "", Replace: "x"}, wantFail: true, wantErr: "不能为空"},
		{name: "literal 过短", rule: litRule("the", "t-he"), wantFail: true, wantErr: "至少 4 个字符"},
		{name: "替换串等于匹配串", rule: litRule("abcdefgh", "abcdefgh"), wantFail: true, wantErr: "不会产生任何变化"},
		{name: "正则编译失败", rule: FingerprintRule{Kind: FingerprintRuleRegex, Match: `([unclosed`, Replace: "x"}, wantFail: true, wantErr: "无法编译"},
		// 空匹配是唯一能"自伤"的形态：ReplaceAllString 会把替换串插到每个字符位置。
		{name: "正则匹配空串", rule: FingerprintRule{Kind: FingerprintRuleRegex, Match: `x*`, Replace: "-"}, wantFail: true, wantErr: "匹配空串"},
		{name: "正则匹配空串2", rule: FingerprintRule{Kind: FingerprintRuleRegex, Match: `(a|)`, Replace: "-"}, wantFail: true, wantErr: "匹配空串"},
		{name: "匹配串过长", rule: litRule(strings.Repeat("x", maxFingerprintPatternBytes+1), "y"), wantFail: true, wantErr: "过长"},
		{name: "替换串过长", rule: litRule("abcdefgh", strings.Repeat("y", maxFingerprintReplaceBytes+1)), wantFail: true, wantErr: "过长"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer resetFpRules(t)
			err := SetFingerprintRules([]FingerprintRule{c.rule})
			if c.wantFail {
				if err == nil {
					t.Fatalf("应当被拒绝，却通过了")
				}
				if !strings.Contains(err.Error(), c.wantErr) {
					t.Errorf("错误文案应包含 %q，实际 %q", c.wantErr, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("应当通过，却失败：%v", err)
			}
		})
	}
}

// TestFingerprintRulesCountLimit 条数上限（防脚本批量灌规则把热路径拖慢）。
func TestFingerprintRulesCountLimit(t *testing.T) {
	defer resetFpRules(t)
	rules := make([]FingerprintRule, 0, MaxFingerprintRules+1)
	for i := 0; i <= MaxFingerprintRules; i++ {
		rules = append(rules, litRule("fingerprint-"+strings.Repeat("a", i%5)+string(rune('a'+i%26))+itoaTest(i), "x"))
	}
	if err := SetFingerprintRules(rules); err == nil {
		t.Fatalf("超过 %d 条应当被拒绝（实际条数 %d）", MaxFingerprintRules, len(rules))
	}
}

// itoaTest 避免为此引入 strconv（包内已有同名测试辅助时也不冲突）。
func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestFingerprintRuleLiteralApplies 核心：装了 literal 规则后，热路径真的改了文本。
//
// 这里刻意断言**两个层面**：预检（hasFingerprint）与应用（sanitizeText）。
// 只测后者会漏掉"预检没进自定义规则 → 整条净化被跳过"这个缺陷
// （它的表现正是最迷惑人的那种："规则列表里有它，但没有效果"）。
func TestFingerprintRuleLiteralApplies(t *testing.T) {
	defer resetFpRules(t)
	const in = "xx NEWFINGERPRINT yy"
	if err := SetFingerprintRules([]FingerprintRule{litRule("NEWFINGERPRINT", "NEW-FINGERPRINT")}); err != nil {
		t.Fatalf("SetFingerprintRules: %v", err)
	}
	if !hasFingerprint(in) {
		t.Fatalf("预检必须命中自定义规则（否则整条净化会被跳过）")
	}
	if got := sanitizeText(in); got != "xx NEW-FINGERPRINT yy" {
		t.Errorf("sanitizeText = %q，期望 %q", got, "xx NEW-FINGERPRINT yy")
	}
	// 卸掉规则后必须**立刻**恢复原状（不存在"卸不掉"的规则）。
	if err := SetFingerprintRules(nil); err != nil {
		t.Fatalf("清空失败：%v", err)
	}
	if got := sanitizeText(in); got != in {
		t.Errorf("清空后不应再改写，得到 %q", got)
	}
	if hasFingerprint(in) {
		t.Errorf("清空后预检不应命中")
	}
}

// TestFingerprintRuleRegexStrip 正则形态：整段删除（Replace 为空）。
func TestFingerprintRuleRegexStrip(t *testing.T) {
	defer resetFpRules(t)
	const in = "prefix wb_trace=9f3a; suffix"
	if err := SetFingerprintRules([]FingerprintRule{
		{Kind: FingerprintRuleRegex, Match: `wb_trace=[0-9a-f]+;?\s*`, Replace: ""},
	}); err != nil {
		t.Fatalf("SetFingerprintRules: %v", err)
	}
	if got := sanitizeText(in); got != "prefix suffix" {
		t.Errorf("sanitizeText = %q，期望 %q", got, "prefix suffix")
	}
}

// TestFingerprintRuleLiteralBeforeRegex 应用顺序：literal 先于 regex。
//
// 顺序有语义（literal 是最小改动，先做最小改动后面的 regex 才有机会处理残段）。
// 这条用一对**互相可见**的规则把顺序钉死：若顺序反了，结果会不同。
func TestFingerprintRuleLiteralBeforeRegex(t *testing.T) {
	defer resetFpRules(t)
	// literal 把 AAAA 变成 BBBB；regex 把 BBBB 删掉。
	// 若 literal 先 → 结果不含 BBBB；若 regex 先 → BBBB 残留。
	const in = "x AAAA y"
	rules := []FingerprintRule{
		{Kind: FingerprintRuleRegex, Match: `BBBB`, Replace: ""},
		litRule("AAAA", "BBBB"),
	}
	if err := SetFingerprintRules(rules); err != nil {
		t.Fatalf("SetFingerprintRules: %v", err)
	}
	got := strings.TrimSpace(sanitizeText(in))
	if strings.Contains(got, "BBBB") {
		t.Errorf("literal 必须先于 regex 应用，得到 %q", got)
	}
}

// TestFingerprintRulesRejectOverBroad 过宽自检：会命中普通文本的规则必须在保存时被拒。
//
// 这是本功能最重要的一道安全网。没有它，一条 `e` → `3` 之类的规则会让**所有**
// 提示词被改写，表现是"加了规则之后模型开始答非所问"，而没人会想到是脱敏层干的。
func TestFingerprintRulesRejectOverBroad(t *testing.T) {
	defer resetFpRules(t)
	// 语料里含 "func main"（见 fingerprintSafetyCorpus）。
	err := SetFingerprintRules([]FingerprintRule{litRule("func main", "func-main")})
	if err == nil {
		t.Fatalf("命中安全语料的规则必须被拒绝")
	}
	if !strings.Contains(err.Error(), "普通文本") {
		t.Errorf("错误文案应说明会命中普通文本，实际 %q", err.Error())
	}
	// 被拒后**不得**留下任何规则（否则"保存失败"其实是"保存了一半"）。
	if n := len(FingerprintRules()); n != 0 {
		t.Errorf("校验失败后不应留下规则，实际 %d 条", n)
	}
	// 关键对照：同一条规则若不是命中语料就应当通过 —— 证明拒绝的原因是"过宽"，
	// 而不是"contains 这个词一律拒绝"。
	if err := SetFingerprintRules([]FingerprintRule{litRule("func mainX", "func main-X")}); err != nil {
		t.Errorf("不命中语料的规则应当通过：%v", err)
	}
}

// TestFingerprintSafetyCorpusUntouchedByBuiltin 安全语料在**只有内置规则**时必须逐字不变。
//
// 这是过宽自检的基准。若哪天内置规则开始命中这些文本，自检会把每条自定义规则
// 都误判为"过宽"——那时错误会指向运维，而根因在内置表。
func TestFingerprintSafetyCorpusUntouchedByBuiltin(t *testing.T) {
	for _, text := range fingerprintSafetyCorpus {
		if got := sanitizeTextWith(nil, text); got != text {
			t.Errorf("内置规则不该改动安全语料。\n原文：%q\n结果：%q", text, got)
		}
		if hasFingerprintWith(nil, text) {
			t.Errorf("内置预检不该命中安全语料：%q", text)
		}
	}
}

// TestFingerprintRulesEmptyByDefault 默认无自定义规则时，管线行为与从前逐字一致。
//
// 这条是"本功能对既有生产零影响"的证据：新增的是叠加层，未配置时指针为 nil。
func TestFingerprintRulesEmptyByDefault(t *testing.T) {
	resetFpRules(t)
	if n := len(FingerprintRules()); n != 0 {
		t.Fatalf("默认不应有自定义规则，实际 %d 条", n)
	}
	if cr := activeFingerprintRules.Load(); cr != nil {
		t.Fatalf("默认指针应为 nil（热路径只做一次 nil 判断）")
	}
	// 内置规则照常生效（叠加层没有把内置路径挡住）。
	if got := sanitizeText("... 11128 ..."); strings.Contains(got, "11128") {
		t.Errorf("内置规则应仍生效，得到 %q", got)
	}
}

// TestFingerprintRuleDedupLastWins 同一 (kind, match) 只保留最后一条（upsert 语义）。
func TestFingerprintRuleDedupLastWins(t *testing.T) {
	defer resetFpRules(t)
	if err := SetFingerprintRules([]FingerprintRule{
		litRule("DUPLICATE-MATCH", "one"),
		litRule("DUPLICATE-MATCH", "two"),
	}); err != nil {
		t.Fatalf("SetFingerprintRules: %v", err)
	}
	got := FingerprintRules()
	if len(got) != 1 {
		t.Fatalf("同匹配串应合并为 1 条，实际 %d", len(got))
	}
	if got[0].Replace != "two" {
		t.Errorf("后者应覆盖前者，实际 replace=%q", got[0].Replace)
	}
}

// TestFingerprintRuleHits 回放：把"这条规则有没有用"变成数字。
func TestFingerprintRuleHits(t *testing.T) {
	rules := []FingerprintRule{
		litRule("HIT-A", "a"),
		litRule("HIT-B", "b"),
	}
	texts := []string{"has HIT-A here", "has HIT-A and HIT-B", "neither", ""}
	hits, err := FingerprintRuleHits(rules, texts)
	if err != nil {
		t.Fatalf("FingerprintRuleHits: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("应返回每条规则的计数（含 0），实际 %d 项", len(hits))
	}
	var aID, bID string
	for _, r := range rules {
		// 用与实现相同的派生方式取 ID（这里通过 SetFingerprintRules 归一化更稳妥，
		// 但那条路径会改全局状态 —— 回放本身不应有副作用，所以直接算）。
		n, err := validateFingerprintRule(r)
		if err != nil {
			t.Fatalf("validate: %v", err)
		}
		if r.Match == "HIT-A" {
			aID = n.ID
		} else {
			bID = n.ID
		}
	}
	if hits[aID] != 2 {
		t.Errorf("HIT-A 应命中 2 条，实际 %d", hits[aID])
	}
	if hits[bID] != 1 {
		t.Errorf("HIT-B 应命中 1 条，实际 %d", hits[bID])
	}
	// 回放**不得**改动全局规则集（它是"保存前预演"的基础）。
	if n := len(FingerprintRules()); n != 0 {
		t.Errorf("回放不应安装规则，实际 %d 条", n)
	}
	// 非法规则集应报错而不是静默返回空计数（否则界面会显示"一条都没命中"，
	// 把"规则写错了"伪装成"规则没用"）。
	if _, err := FingerprintRuleHits([]FingerprintRule{{Kind: "bad", Match: "x", Replace: "y"}}, texts); err == nil {
		t.Errorf("非法规则集应报错")
	}
}

// TestSanitizeTextWithDoesNotInstall 候选集评估不得影响已安装的规则集。
func TestSanitizeTextWithDoesNotInstall(t *testing.T) {
	defer resetFpRules(t)
	if err := SetFingerprintRules([]FingerprintRule{litRule("INSTALLED-ONE", "i")}); err != nil {
		t.Fatalf("SetFingerprintRules: %v", err)
	}
	out, changed, err := SanitizeTextWith([]FingerprintRule{litRule("CANDIDATE-TWO", "c")}, "CANDIDATE-TWO")
	if err != nil {
		t.Fatalf("SanitizeTextWith: %v", err)
	}
	if !changed || out == "CANDIDATE-TWO" {
		t.Errorf("候选集应生效于本次调用，得到 %q", out)
	}
	// 全局仍只有已安装的那条，且候选规则**不**生效。
	got := FingerprintRules()
	if len(got) != 1 || got[0].Match != "INSTALLED-ONE" {
		t.Fatalf("候选评估改动了已安装规则集：%+v", got)
	}
	if s := sanitizeText("CANDIDATE-TWO"); s != "CANDIDATE-TWO" {
		t.Errorf("候选规则不应影响后续调用，得到 %q", s)
	}
	// 非法候选集必须报错（而不是悄悄按"没有规则"跑）。
	if _, _, err := SanitizeTextWith([]FingerprintRule{{Kind: FingerprintRuleRegex, Match: `a*`, Replace: "-"}}, "aaa"); err == nil {
		t.Errorf("非法候选集应报错")
	}
}

// TestBuiltinFingerprintRulesView 内置表的只读视图。
//
// 它存在的理由是"让运维分清哪些是本来就有的"——所以视图必须是内置表的**超集**
// 与**忠实映射**，且全部标 Builtin=true（前端据此禁用删除按钮）。
func TestBuiltinFingerprintRulesView(t *testing.T) {
	view := BuiltinFingerprintRules()
	if len(view) < len(sanitizeRewrites)+3 {
		t.Fatalf("内置视图至少应含 %d 条（5 改 + 3 正则），实际 %d", len(sanitizeRewrites)+3, len(view))
	}
	foundIdentity := false
	for _, r := range view {
		if !r.Builtin {
			t.Errorf("内置规则必须标 Builtin：%+v", r)
		}
		if r.ID == "" {
			t.Errorf("内置规则也要有 ID（前端用它做 key）：%+v", r)
		}
		if r.Kind != FingerprintRuleLiteral && r.Kind != FingerprintRuleRegex {
			t.Errorf("内置规则形态必须是两种之一：%+v", r)
		}
		if strings.Contains(r.Match, "Anthropic's official CLI for Claude") {
			foundIdentity = true
		}
	}
	if !foundIdentity {
		t.Errorf("内置视图应含身份句改写规则（否则它不是内置表的忠实映射）")
	}
	// 视图必须是**副本**：改动它不得影响内置表本身。
	orig := sanitizeRewrites[0][1]
	view[0].Replace = "被外部改坏了"
	if sanitizeRewrites[0][1] != orig {
		t.Fatalf("内置视图不是副本，外部改动污染了内置表")
	}
}
