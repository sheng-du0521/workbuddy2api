// 模型名归一化的测试（v0.31.3 从 cmd/webapp 迁入；两个产品共用同一套实现与用例）。
//
// 背景（本机实测）：`GLM-5.3` / `" glm-5.3 "` / `glm-5.3\n` / `[global:]glm-5.3`
// 全部被上游回 11102（"model [...] service info not found"），而 `glm-5.3` 正常 ——
// 上游对模型名大小写敏感且不容忍空白与前缀。
package modelname

import (
	"fmt"
	"strings"
	"testing"
)

// TestNormalize 逐层验证：去空白 → 剥 realm 前缀 → 大小写归一 → 分隔符归一。
func TestNormalize(t *testing.T) {
	known := []string{"glm-5.3", "kimi-k3-1", "deepseek-v4-flash"}
	cases := []struct {
		in, want string
		anyHow   string // 期望 how 里包含的关键字（空 = 必须完全没有改动）
	}{
		// 第 1 层：空白与零宽字符（复制粘贴的典型产物）
		{"glm-5.3", "glm-5.3", ""},
		{" glm-5.3 ", "glm-5.3", "去首尾空白"},
		{"glm-5.3\n", "glm-5.3", "去首尾空白"},
		{"\tglm-5.3\r\n", "glm-5.3", "去首尾空白"},
		{"\u00a0glm-5.3\u200b", "glm-5.3", "去首尾空白"}, // NBSP + 零宽空格（TrimSpace 不处理）
		{"\ufeffglm-5.3", "glm-5.3", "去首尾空白"},       // BOM
		// 第 2 层：realm 前缀（本仓路由协议，上游只认裸名；旧网关一直在剥）
		{"global:glm-5.3", "glm-5.3", "剥 realm 前缀"},
		{"cn:glm-5.3", "glm-5.3", "剥 realm 前缀"},
		{"[global:]glm-5.3", "glm-5.3", "剥 realm 前缀"},
		{"[cn:]glm-5.3", "glm-5.3", "剥 realm 前缀"},
		{" global: glm-5.3 ", "glm-5.3", "剥 realm 前缀"},
		// 非 realm 前缀不剥（与旧网关 resolveModel 口径一致：只有 cn/global 两个词）
		{"foo:glm-5.3", "foo:glm-5.3", ""},
		{"global:", "global:", ""}, // 前缀后为空：不剥（否则会把有效信息剥成空）
		{"global", "global", ""},   // 没有冒号
		// 第 3 层：大小写归一（与已知清单精确匹配）
		{"GLM-5.3", "glm-5.3", "大小写归一"},
		{"Kimi-K3-1", "kimi-k3-1", "大小写归一"},
		// 第 4 层：分隔符归一（与已知清单"忽略分隔符"匹配且唯一）
		{"glm-5-3", "glm-5.3", "分隔符归一"},
		{"GLM_5.3", "glm-5.3", "分隔符归一"},
		{"glm5.3", "glm-5.3", "分隔符归一"},
		// 认不出来就**原样**送出去（不猜模型）
		{"gpt-4o", "gpt-4o", ""},
		{"", "", ""},
		{"   ", "", "去首尾空白"}, // 全空白 → 空串（让上游照常报 model [] 不存在）
	}
	for _, c := range cases {
		got, how := Normalize(c.in, known)
		if got != c.want {
			t.Errorf("Normalize(%q) = %q，期望 %q（how=%q）", c.in, got, c.want, how)
		}
		if c.anyHow == "" && how != "" {
			t.Errorf("Normalize(%q) 不应改动，却报告改动了：%q", c.in, how)
		}
		if c.anyHow != "" && !strings.Contains(how, c.anyHow) {
			t.Errorf("Normalize(%q) 的说明 %q 应包含 %q", c.in, how, c.anyHow)
		}
	}
}

// TestNormalizeKnownListEmptyOrNil 拿不到清单时只做前两层（不凭猜改写）。
func TestNormalizeKnownListEmptyOrNil(t *testing.T) {
	for _, known := range [][]string{nil, {}} {
		if got, _ := Normalize(" GLM-5.3 ", known); got != "GLM-5.3" {
			t.Errorf("无清单时应只去空白（大小写保留），got %q", got)
		}
		if got, how := Normalize("glm-5.3", known); got != "glm-5.3" || how != "" {
			t.Errorf("无清单时正常名不应改动，got %q how=%q", got, how)
		}
	}
}

// TestNormalizeAmbiguousNeverGuesses 清单里存在多个等价项时放弃改写。
//
// 猜错的代价是"不可诊断的失败"（把模型名改成了另一个真实存在的模型，
// 上游会正常服务 —— 于是用户永远不知道自己的写法被改过）。
func TestNormalizeAmbiguousNeverGuesses(t *testing.T) {
	known := []string{"abc", "ABC"}
	if got, _ := Normalize("Abc", known); got != "Abc" {
		t.Errorf("命中不唯一时必须原样返回，got %q", got)
	}
	// 分隔符层同样要唯一：清单里同时有 glm-5.3 与 glm5-3 时，"GLM_5.3" 不该被改写。
	known2 := []string{"glm-5.3", "glm5-3"}
	if got, _ := Normalize("GLM_5.3", known2); got != "GLM_5.3" {
		t.Errorf("宽匹配命中不唯一时必须原样返回，got %q", got)
	}
}

// TestIsKnownSameScopeAsNormalize 判定"未知"的口径必须与归一化同口径。
//
// 否则会出现自相矛盾的日志：一条"模型名 X 不在已知清单内"的 WARN，紧跟着
// 归一化把 X 成功折回了规范名（说明它其实在清单里）。
func TestIsKnownSameScopeAsNormalize(t *testing.T) {
	known := []string{"glm-5.3", "kimi-k3-1"}
	for _, m := range []string{"glm-5.3", "GLM-5.3", "GLM_5.3", "glm5.3"} {
		if !IsKnown(known, m) {
			t.Errorf("IsKnown(%q) 应为 true —— 它能被归一化匹配上", m)
		}
	}
	if IsKnown(known, "gpt-4o") {
		t.Error("清单里没有的名字不该被判为已知")
	}
	if !IsKnown(known, "") {
		t.Error("空模型名不该报「未知」——它是「没给模型」而不是「给了错模型」")
	}
}

// TestFallbackModelsWellFormed 兜底清单是"已知集合的最小形态"，必须非空且无空白/重复。
func TestFallbackModelsWellFormed(t *testing.T) {
	if len(FallbackModels) == 0 {
		t.Fatal("兜底清单不能为空（否则冷启动时归一化只剩前两层）")
	}
	seen := map[string]bool{}
	for _, m := range FallbackModels {
		if m == "" || m != strings.TrimSpace(m) {
			t.Errorf("兜底清单条目 %q 含空白或为空", m)
		}
		if seen[m] {
			t.Errorf("兜底清单条目 %q 重复", m)
		}
		seen[m] = true
	}
}

// TestNoteUnknownDedupAndBound WARN 去重（同名一次）且有上限（外部输入不能撑大状态）。
func TestNoteUnknownDedupAndBound(t *testing.T) {
	unknown.mu.Lock()
	old := unknown.seen
	unknown.seen = nil
	unknown.mu.Unlock()
	defer func() {
		unknown.mu.Lock()
		unknown.seen = old
		unknown.mu.Unlock()
	}()

	known := []string{"glm-5.3"}
	NoteUnknown("nope-1", known, "test")
	NoteUnknown("nope-1", known, "test")
	if n := len(unknown.seen); n != 1 {
		t.Errorf("同名应只记一次，got %d", n)
	}
	for i := 0; i < 200; i++ {
		NoteUnknown(fmt.Sprintf("nope-%d", i), known, "test")
	}
	if n := len(unknown.seen); n > 64 {
		t.Errorf("应有上限 64，got %d", n)
	}
}
