// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// fpsample.go 指纹被拦样本回流（R4，v0.26.1）。
//
// ## 解决什么问题
//
// 出站指纹黑名单（`internal/upstream/sanitize.go`）目前是**静态表**：每次发现新的
// 触发词，都要「人工察觉 → 改代码 → 跑测试 → 发版」。也就是说指纹对抗被绑在
// **发版节奏**上，而对手是随时在变的。同类项目（TraeWorkAssistant）的做法是
// **把被拦时的样本落盘**（system 原文前若干字符 + 工具清单），运维据此扩表 ——
// 对抗节奏从「发版」变成「运营」。
//
// ## ⚠️ 为什么默认**关闭**
//
// 这是本仓少有的、会**把请求内容写到磁盘**的功能：样本里含 system 提示词原文。
// 本仓此前对外承诺过「请求体 / 响应体内容不留存」（见 README 与使用说明），
// 因此本功能做成**显式开关**（`fingerprint_samples`，默认 off）：
//
//   - 关（默认）—— 行为与之前完全一致，一个字都不落盘；
//   - 开 —— 只落 **system 提示词前 300 字符 + 工具名**（不含用户消息、不含助手回复、
//     不落整个请求体），用于排查"是哪句话/哪个工具触发了上游内容策略"。
//
// 只做选择性落盘而不是"把整个请求存下来"，是因为前者足以支撑扩黑名单，
// 而后者会把下游用户的提问内容长期留在服务器上 —— 那是另一类问题。
//
// ## 为什么挂在唯一出口上
//
// 采样点在 `reportUpstreamError` 的**唯一返回点**：那里已经拿到最终分类（kind），
// 不必在每个 case 分支里各写一遍（写多份必然漏一份）。
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/upstream"
)

const (
	// kvFingerprintSamples 设置键：on / off（默认 off）。见文件头「为什么默认关闭」。
	kvFingerprintSamples = "fingerprint_samples"

	// fpSampleSystemChars system 原文保留字符数。
	//
	// 300 是权衡：指纹一般是模板句 / 身份句，出现在 system 开头的概率最高；
	// 再长收益递减，而隐私暴露面线性增长。
	fpSampleSystemChars = 300
	// fpSampleMaxTools 工具名最多保留多少个（工具清单本身也是指纹载体）。
	fpSampleMaxTools = 40
	// fpSampleFileMaxBytes 样本文件上限，超过即停止追加（**不轮转**：样本是运维资产，
	// 静默滚掉比不写更糟；到顶时接口会明确提示"已满，请先清理"）。
	fpSampleFileMaxBytes = 2 << 20
	// fpSampleMaxLines 接口一次最多返回多少条（去重后的最近样本）。
	fpSampleMaxLines = 50
	// fpSampleDedupMax 内存去重表上限（进程内去重，避免同一句话把文件刷满）。
	fpSampleDedupMax = 500
)

// fpSampleFile 样本文件路径。由 main.go 按 `-db` 所在目录设置（与库、密钥同目录），
// 单测直接改这个变量即可，无需构造 app。
var fpSampleFile = filepath.Join("data", "fingerprint_samples.jsonl")

// fpSampleDedup 进程内去重表（key = 样本指纹 → 已写过）。
var fpSampleDedup = struct {
	sync.Mutex
	seen map[string]bool
}{seen: map[string]bool{}}

// fingerprintSamplesEnabled 是否开启采样（默认关闭）。
func (p *Proxy) fingerprintSamplesEnabled() bool {
	if p == nil || p.store == nil {
		return false
	}
	v, _ := p.store.GetKV(kvFingerprintSamples)
	return strings.EqualFold(strings.TrimSpace(v), "on")
}

// fingerprintSample 一条被拦样本（字段刻意少而稳，便于外部脚本消费）。
type fingerprintSample struct {
	At       int64    `json:"at"`
	Kind     string   `json:"kind"` // channel_denied / content_blocked / account_fault
	Model    string   `json:"model"`
	UID      string   `json:"uid,omitempty"`
	Key      string   `json:"key"` // 去重键（sha256 前 12）
	System   string   `json:"system"`
	Tools    []string `json:"tools,omitempty"`
	Upstream string   `json:"upstream,omitempty"` // 上游错误摘要（前 200 字）
}

// extractFingerprintSample 从**请求体**里取出可用于扩黑名单的最小样本。
//
// 只取三样：system 提示词前 N 字、工具名清单、以及（可选）上游错误摘要。
// 刻意不取 user / assistant 消息 —— 那些是下游用户的内容，与指纹无关。
func extractFingerprintSample(body []byte) (string, []string) {
	if len(body) == 0 {
		return "", nil
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return "", nil
	}
	var sys string
	if msgs, ok := obj["messages"].([]any); ok {
		for _, m := range msgs {
			mm, ok := m.(map[string]any)
			if !ok {
				continue
			}
			if s, _ := mm["role"].(string); s != "system" {
				continue
			}
			switch c := mm["content"].(type) {
			case string:
				sys = c
			case []any:
				// 多段形态：[{type:"text", text:"..."}]；只拼接 text 段。
				var b strings.Builder
				for _, seg := range c {
					if sm, ok := seg.(map[string]any); ok {
						if t, ok := sm["text"].(string); ok {
							b.WriteString(t)
						}
					}
				}
				sys = b.String()
			}
			break // 只看第一条 system
		}
	}
	var tools []string
	if ts, ok := obj["tools"].([]any); ok {
		for _, t := range ts {
			if len(tools) >= fpSampleMaxTools {
				break
			}
			tm, ok := t.(map[string]any)
			if !ok {
				continue
			}
			name, _ := tm["name"].(string) // 扁平形态
			if name == "" {
				if fn, ok := tm["function"].(map[string]any); ok { // OpenAI 嵌套形态
					name, _ = fn["name"].(string)
				}
			}
			if name != "" {
				tools = append(tools, name)
			}
		}
	}
	return truncateRunes(strings.TrimSpace(sys), fpSampleSystemChars), tools
}

// sha256Hex 十六进制摘要（样本去重键与设备凭证指纹共用同一口径）。
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// sampleKey 样本去重键：只看 (kind, system 前缀, 工具清单) —— 同一句话反复被拦
// 只需要留一条，否则文件会被同一模式刷满、反而掩盖新样本。
func sampleKey(kind, system string, tools []string) string {
	h := sha256Hex(kind + "\x1f" + system + "\x1f" + strings.Join(tools, ","))
	return h[:12]
}

// maybeSampleFingerprint 在**唯一出口**按分类决定是否落样本（默认关闭时零成本返回）。
func (p *Proxy) maybeSampleFingerprint(kind upstream.ErrKind, uid, model string, reqBody []byte, upstreamBody []byte) {
	var k string
	switch kind {
	case upstream.ErrChannelDenied:
		k = "channel_denied" // 11128：渠道 / 客户端指纹被拒
	case upstream.ErrContentBlocked:
		k = "content_blocked" // 内容策略拦截（常由 system 指纹误触发）
	case upstream.ErrAccountFault:
		k = "account_fault" // 11140 request illegal
	default:
		return
	}
	if !p.fingerprintSamplesEnabled() {
		return // 关：一个字都不落盘
	}
	sys, tools := extractFingerprintSample(reqBody)
	if sys == "" && len(tools) == 0 {
		return // 没取到可用样本（如非 chat 请求），不写空行污染文件
	}
	key := sampleKey(k, sys, tools)
	fpSampleDedup.Lock()
	if fpSampleDedup.seen[key] {
		fpSampleDedup.Unlock()
		return
	}
	if len(fpSampleDedup.seen) >= fpSampleDedupMax {
		fpSampleDedup.seen = map[string]bool{} // 表满清空重来：宁可有重复，不可无限增长
	}
	fpSampleDedup.seen[key] = true
	fpSampleDedup.Unlock()

	if err := appendFingerprintSample(fingerprintSample{
		At: time.Now().Unix(), Kind: k, Model: model, UID: uid, Key: key,
		System: sys, Tools: tools,
		Upstream: truncateRunes(strings.TrimSpace(string(upstreamBody)), 200),
	}); err != nil {
		// 采样失败绝不影响主流程（它是诊断增强，不是业务路径）。
		log.Printf("WARN: [fpsample] 写样本失败：%v", err)
	}
}

// appendFingerprintSample 追加一行 JSONL。文件超上限时**拒绝写入**（见常量注释）。
func appendFingerprintSample(s fingerprintSample) error {
	if fpSampleFile == "" {
		return fmt.Errorf("样本文件路径为空")
	}
	if st, err := os.Stat(fpSampleFile); err == nil && st.Size() >= fpSampleFileMaxBytes {
		return fmt.Errorf("样本文件已达上限 %d 字节，请先清理 %s", fpSampleFileMaxBytes, fpSampleFile)
	}
	if dir := filepath.Dir(fpSampleFile); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(fpSampleFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

// fingerprintSamplesView GET /api/admin/fingerprint-samples 的响应。
type fingerprintSamplesView struct {
	Enabled bool                `json:"enabled"`
	File    string              `json:"file"`
	Count   int                 `json:"count"` // 去重后的样本数
	Full    bool                `json:"full"`  // 文件是否已到上限
	Kinds   map[string]int      `json:"kinds"`
	Samples []fingerprintSample `json:"samples"`
	// Size / Max / Lines 供界面画占用条与"还能装多少"的判断。
	//
	// 为什么要透出体积而不是只给一个 full 布尔：文件到上限时写入会被**拒绝**
	// （不轮转，样本是运维资产），而"还能装多少"决定了运维是"顺手看一眼"
	// 还是"必须现在处理"。只给布尔的话，90% 与 100% 看起来一样。
	Size  int64 `json:"size"`
	Max   int64 `json:"max"`
	Lines int   `json:"lines"` // 文件里的物理行数（含被去重合并掉的重复样本）
	// Models 各模型的样本数（Top 8）：定位"是哪个模型在触发内容策略"的第一手线索。
	Models []fpCount `json:"models"`
	// LastAt 最近一条样本的时刻（0 = 无）。
	LastAt int64 `json:"last_at"`
}

// fpCount 一个分组计数（类型 / 模型通用）。
type fpCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// topFpCounts 取计数最大的前 n 项，按「数量降序，同数量按名字升序」排序。
//
// 名字升序是刻意的：数量相同时 map 遍历顺序随机，不排序会让同一份数据每次
// 刷新出来的顺序都不一样 —— 表现是"这个界面在跳"，很难查。
func topFpCounts(m map[string]int, n int) []fpCount {
	out := make([]fpCount, 0, len(m))
	for k, c := range m {
		out = append(out, fpCount{Name: k, Count: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// clearFingerprintSamples 清空样本文件并重置进程内去重表。
//
// 为什么必须提供这个入口：文件到上限后 appendFingerprintSample 会**拒绝写入**，
// 接口只回一句"请先清理 <路径>" —— 而此前没有任何界面或接口能做这件事，
// 等于把"采样到顶"变成必须登录服务器手删文件。运维手册里那句"请先清理"
// 于是变成了空话（这正是用户反馈"缺少可视化面板"背后的实际问题之一）。
//
// 返回被清掉的字节数与行数，供界面回显"清掉了多少"。
func clearFingerprintSamples() (int64, error) {
	var size int64
	if st, err := os.Stat(fpSampleFile); err == nil {
		size = st.Size()
	} else if !os.IsNotExist(err) {
		return 0, err
	}
	// 直接删除而不是截断：少一个"文件存在但为空"的中间态。
	// 不存在（还没采样过）不算错误 —— 清空一个空文件应当是幂等的成功。
	if err := os.Remove(fpSampleFile); err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	// 去重表也要清：只删文件的话，同样的样本在本进程余下的生命周期里
	// 依然被内存表挡着、再也写不回来 —— 表现是"清空之后文件一直是空的"。
	fpSampleDedup.Lock()
	fpSampleDedup.seen = map[string]bool{}
	fpSampleDedup.Unlock()
	return size, nil
}

// handleFingerprintSamplesClear DELETE /api/admin/fingerprint-samples
//
// 只清本功能自己的样本文件 —— 路径是程序启动时按 -db 目录定的包级变量，
// **不接受任何来自请求的参数**（否则就成了一个任意文件删除接口）。
func (a *app) handleFingerprintSamplesClear(w http.ResponseWriter, r *http.Request) {
	size, err := clearFingerprintSamples()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	log.Printf("INFO: [fpsample] 管理侧清空样本文件 %s（释放 %d 字节）", fpSampleFile, size)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed_bytes": size, "file": fpSampleFile})
}

// handleFingerprintSamples GET /api/admin/fingerprint-samples
//
// 读样本文件并**按去重键合并**（同一模式只留最新一条），最多返回 fpSampleMaxLines 条。
func (a *app) handleFingerprintSamples(w http.ResponseWriter, r *http.Request) {
	v := fingerprintSamplesView{Enabled: a.proxy.fingerprintSamplesEnabled(),
		File: fpSampleFile, Kinds: map[string]int{}, Samples: []fingerprintSample{},
		Max: fpSampleFileMaxBytes, Models: []fpCount{}}
	raw, err := os.ReadFile(fpSampleFile)
	if err != nil {
		if !os.IsNotExist(err) {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, v) // 还没写过 = 空清单，不是错误
		return
	}
	v.Size = int64(len(raw))
	if st, err := os.Stat(fpSampleFile); err == nil {
		v.Size = st.Size()
		if st.Size() >= fpSampleFileMaxBytes {
			v.Full = true
		}
	}
	byKey := map[string]fingerprintSample{}
	order := []string{}
	modelCount := map[string]int{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		v.Lines++
		var s fingerprintSample
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			continue // 半行 / 坏行直接跳过（文件是追加写的，崩溃可能留半行）
		}
		if _, dup := byKey[s.Key]; !dup {
			order = append(order, s.Key)
		}
		byKey[s.Key] = s // 同键取最新
		v.Kinds[s.Kind]++
		if s.Model != "" {
			modelCount[s.Model]++
		}
		if s.At > v.LastAt {
			v.LastAt = s.At
		}
	}
	v.Models = topFpCounts(modelCount, 8)
	// 只回最近的 N 条（order 是写入顺序，取尾部）。
	start := 0
	if len(order) > fpSampleMaxLines {
		start = len(order) - fpSampleMaxLines
	}
	for _, k := range order[start:] {
		v.Samples = append(v.Samples, byKey[k])
	}
	v.Count = len(order)
	writeJSON(w, http.StatusOK, v)
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
