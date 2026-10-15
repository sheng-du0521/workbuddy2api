// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 清单的**拉取与候选推导**（version.json 的多地址回退）。
//
// 与 manifest.go 的分工：那边是"清单说了什么"，这边是"从哪拿到清单"。
// 网络客户端由调用方通过 HTTPClient 注入（测试可替换），本包自己不建连接池。
package selfupdate

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Candidates 列出一个地址的**等价候选**，按建议尝试顺序返回。
//
// ## 为什么需要回退（v0.26.5）
//
// `raw.githubusercontent.com` 在部分网络下**直接不可达**（本项目实测：本机连不通，
// 12 秒超时；而 `api.github.com` 0.6 秒返回）。于是"检查更新"在这类环境里恒失败，
// 提醒功能等于不存在 —— 而它恰恰是私有化部署最需要的那个功能（用户不会天天来翻仓库）。
//
// 候选的来源是**从 raw 地址推导**，不是硬编码一个第三方域名列表：
//
//	https://raw.githubusercontent.com/OWNER/REPO/BRANCH/PATH
//	  → https://api.github.com/repos/OWNER/REPO/contents/PATH?ref=BRANCH   （GitHub 官方 API，可解析 base64）
//	  → https://cdn.jsdelivr.net/gh/OWNER/REPO@BRANCH/PATH                （公共 CDN 镜像）
//	  → https://ghproxy.net/https://raw.githubusercontent.com/...          （反代）
//
// 非 raw 形态的地址（自建 CDN、内网文件服务）**不做推导**：那种情况下"多试几个
// 第三方站点"没有任何依据，只会把出站面扩大。此时候选就是它自己。
//
// ⚠️ 安全边界：回退只改**传输路径**，不改**校验口径** —— 清单里给出的下载地址与
// SHA-256 照旧原样展示给运维，程序从不自动下载替换 exe（见文件头"为什么不做自动更新"）。
// 也就是说镜像被投毒的影响面是"提示里的说明文字"，不是"被自动装上的二进制"。
func Candidates(primary string) []string {
	p := strings.TrimSpace(primary)
	if p == "" {
		return nil
	}
	out := []string{p}
	owner, repo, branch, path, ok := SplitRawGitHubURL(p)
	if !ok {
		return out
	}
	out = append(out,
		fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s?ref=%s", owner, repo, path, branch),
		fmt.Sprintf("https://cdn.jsdelivr.net/gh/%s/%s@%s/%s", owner, repo, branch, path),
		"https://ghproxy.net/https://raw.githubusercontent.com/"+owner+"/"+repo+"/"+branch+"/"+path,
	)
	return out
}

// SplitRawGitHubURL 解析 raw.githubusercontent.com/OWNER/REPO/BRANCH/PATH... 形态。
// 路径可以有多段（如 dist/version.json），故只切前三段、其余整体作为 path。
func SplitRawGitHubURL(u string) (owner, repo, branch, path string, ok bool) {
	const prefix = "https://raw.githubusercontent.com/"
	rest, found := strings.CutPrefix(u, prefix)
	if !found {
		// 也接受 http:// 形态（老配置里可能有）。
		rest, found = strings.CutPrefix(u, "http://raw.githubusercontent.com/")
		if !found {
			return "", "", "", "", false
		}
	}
	rest = strings.TrimPrefix(rest, "/")
	parts := strings.SplitN(rest, "/", 4)
	if len(parts) < 4 {
		return "", "", "", "", false
	}
	owner, repo, branch, path = parts[0], parts[1], parts[2], parts[3]
	if owner == "" || repo == "" || branch == "" || path == "" {
		return "", "", "", "", false
	}
	return owner, repo, branch, path, true
}

// ParseBody 把响应体解析成清单，**同时接受两种封装**：
//
//  1. 裸清单 JSON（version.json 本体，raw / CDN 镜像返回的就是它）；
//  2. GitHub Contents API 信封：{name,path,size,content:"<base64>",encoding:"base64",...}。
//
// 第 2 种必须支持 —— 它正是 raw 不可达时的那条回退路径。不支持的话"回退"
// 只会拿到一个没有 version 字段的 JSON 然后报"清单缺少 version 字段"，
// 看起来像清单写错了，实际是解析口径没跟上。
func ParseBody(body []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(body, &m); err == nil && strings.TrimSpace(m.Version) != "" {
		return &m, nil
	}
	var env struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("清单解析失败: %w", err)
	}
	if strings.EqualFold(strings.TrimSpace(env.Encoding), "base64") && strings.TrimSpace(env.Content) != "" {
		// Contents API 的 content 是**折行**的 base64（每 60 字符一个 \n）。
		raw, err := base64.StdEncoding.DecodeString(stripSpace(env.Content))
		if err != nil {
			return nil, fmt.Errorf("清单 base64 解码失败: %w", err)
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("清单解析失败（Contents API 内容）: %w", err)
		}
	}
	if strings.TrimSpace(m.Version) == "" {
		return nil, fmt.Errorf("清单缺少 version 字段")
	}
	return &m, nil
}

// stripSpace 去掉字符串里所有空白（base64 折行、缩进、\r）。
func stripSpace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r':
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// fetchOnce 在**一个**候选地址上拉一次清单。失败返回 error 由调用方决定是否换地址。
func fetchOnce(ctx context.Context, url, curVersion string) (*Manifest, error) {
	if url == "" {
		return nil, fmt.Errorf("未配置更新清单地址")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// 带 UA 是礼貌也是实践：GitHub raw / CDN / 反代都可能拒绝空 UA 的客户端。
	req.Header.Set("User-Agent", "workbuddy2api/"+curVersion)
	req.Header.Set("Accept", "application/json")

	resp, err := HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 403/429 常见于被限流；这里不做重试（下次周期再试即可），
		// 把状态码如实带回，便于运维判断是不是被 CDN 拦了。
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, ManifestMaxBytes))
	if err != nil {
		return nil, err
	}
	return ParseBody(body)
}

// Fetch 依候选顺序尝试，返回第一个成功的清单与其实际地址。
//
// 每个候选各自受 ctx 约束（ctx 的预算由调用方给足，见 handleAdminUpdateStatus）；
// **逐个试**而不是并发打，是因为候选之间有明确的主次（canonical 优先），
// 并发只会让我们对外多打几个包。
//
// 全部失败时 error 汇总每个候选的失败原因 —— 排障时一眼能看出"是全都超时
// （网络不通）"还是"只有 raw 超时而 API 报 404（路径写错了）"。
func Fetch(ctx context.Context, primary, curVersion, lastUsed string) (*Manifest, string, error) {
	cands := Candidates(primary)
	if len(cands) == 0 {
		return nil, "", fmt.Errorf("未配置更新清单地址")
	}
	// 上次成功的候选提到最前：避免每次都在已知不可达的地址上白等一个超时。
	cands = OrderCandidates(cands, lastUsed)
	errs := make([]string, 0, len(cands))
	for _, c := range cands {
		m, err := fetchOnce(ctx, c, curVersion)
		if err == nil {
			return m, c, nil
		}
		errs = append(errs, ShortHost(c)+": "+err.Error())
		if ctx.Err() != nil {
			break // 预算用尽：剩下的候选不必再试（免得把"超时"记成"全部不可达"）
		}
	}
	return nil, "", fmt.Errorf("所有候选地址均失败（%s）", strings.Join(errs, "；"))
}

// OrderCandidates 把上次命中的候选提到最前（抽成纯函数以便钉进测试：
// "优先试上次成功的地址"这条规则看不见摸不着，只靠端到端很难断言）。
//
// lastUsed 不在候选里（运维改了清单地址 / 候选推导规则变了）时**原样返回** ——
// 不把外部输入塞进候选列表，否则一个陈旧的 kv 值就能让程序去访问任意 URL。
func OrderCandidates(cands []string, lastUsed string) []string {
	if lastUsed == "" || len(cands) < 2 {
		return cands
	}
	for i, c := range cands {
		if c == lastUsed {
			if i == 0 {
				return cands
			}
			out := make([]string, 0, len(cands))
			out = append(out, lastUsed)
			out = append(out, cands[:i]...)
			out = append(out, cands[i+1:]...)
			return out
		}
	}
	return cands
}

// ShortHost 把地址压成 host+路径，用于错误汇总（完整 URL 太长，且反代形态会嵌套）。
func ShortHost(u string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	if i := strings.IndexByte(s, '/'); i > 0 {
		return s[:i]
	}
	return s
}

// checkUpdate 执行一次检查（带缓存与单飞），返回给前端的状态。
//
// force=true 时忽略缓存（用户手动点了"立即检查"）。
