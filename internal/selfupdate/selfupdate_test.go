// internal/selfupdate 的自测网（v0.31.7，P2-7 拆包第一刀）。
//
// 本包从 cmd/webapp 的 updatecheck.go 里搬出来，**行为一字未改** —— 所以这里的用例
// 刻意只钉两类东西：
//  1. 边界（拆分时最容易在搬运中走样的地方：版本比较、平台匹配、候选顺序、响应体两种形态）；
//  2. **对外键名契约**（JSON tag）—— 前端与它逐字耦合，改名等于改协议，
//     而"内嵌无 tag 结构体导致键名变成 Go 字段名"在本项目真实发生过（v0.29.0 提现按钮点不动）。
package selfupdate

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func mkSemver(t *testing.T, s string) Semver {
	t.Helper()
	v := ParseSemver(s)
	if !v.OK {
		t.Fatalf("ParseSemver(%q) 解析失败", s)
	}
	return v
}

func TestParseSemverBoundaries(t *testing.T) {
	// 宽松接受：三段、两段、一段、v 前缀、预发布 + build 元数据
	for _, in := range []string{"1.2.3", "v1.2.3", "V1.2.3", " 1.2.3 ", "1.2", "1", "1.2.3-rc.1", "1.2.3-rc.1+build.9"} {
		if !ParseSemver(in).OK {
			t.Errorf("ParseSemver(%q) 应解析成功（宽松口径）", in)
		}
	}
	// 非数字段一律失败：**宽松不等于乱认**
	for _, in := range []string{"", "x.y.z", "1.2.x", "abc", "1.2.3.4.5.6"} {
		if ParseSemver(in).OK {
			t.Errorf("ParseSemver(%q) 应解析失败", in)
		}
	}
	// 预发布标识符要真的被切出来（比较时用它判断"正式版 > 预发布"）
	if got := mkSemver(t, "1.2.3-rc.1+build.9"); len(got.Pre) == 0 || got.Pre[0] != "rc" {
		t.Errorf("Pre 应含 rc，got %+v", got.Pre)
	}
}

func TestCompareSemverOrdering(t *testing.T) {
	cases := []struct{ a, b string }{
		{"1.2.3", "1.2.4"},
		{"1.2.3", "1.3.0"},
		{"1.2.3", "2.0.0"},
		{"1.2.3-rc.1", "1.2.3"}, // 预发布 < 正式版
	}
	for _, c := range cases {
		if got := CompareSemver(mkSemver(t, c.a), mkSemver(t, c.b)); got >= 0 {
			t.Errorf("Compare(%s,%s)=%d，应 <0", c.a, c.b, got)
		}
		if got := CompareSemver(mkSemver(t, c.b), mkSemver(t, c.a)); got <= 0 {
			t.Errorf("Compare(%s,%s)=%d，应 >0", c.b, c.a, got)
		}
	}
	if got := CompareSemver(mkSemver(t, "1.2.3"), mkSemver(t, "v1.2.3")); got != 0 {
		t.Errorf("同一版本的不同写法应相等，got %d", got)
	}
}

func TestDecideAssetMatchByPlatform(t *testing.T) {
	m := &Manifest{
		Version: "9.9.9",
		Notes:   "x",
		Assets: []Asset{
			{OS: "linux", Arch: "amd64", URL: "https://e/l", SHA256: strings.Repeat("a", 64)},
			{OS: "windows", Arch: "amd64", URL: "https://e/w", SHA256: strings.Repeat("b", 64)},
		},
	}
	st := Decide("1.0.0", m, "", 0)
	if !st.HasUpdate || st.Latest != "9.9.9" {
		t.Fatalf("应判定有新版本，got %+v", st)
	}
	if st.Asset == nil || st.Asset.URL != "https://e/w" {
		t.Errorf("平台匹配应挑 windows/amd64 那一条，got %+v", st.Asset)
	}
	if st.Asset.OS != "windows" || st.Asset.SHA256 == "" {
		t.Errorf("产物应带上平台与摘要，got %+v", st.Asset)
	}

	// 没有当前平台产物 → Asset 缺席（不是"随便挑一个"）—— 装错平台比不提醒更糟
	m.Assets = m.Assets[:1]
	if got := Decide("1.0.0", m, "", 0); got.Asset != nil {
		t.Errorf("无平台产物时不该硬塞一个，got %+v", got.Asset)
	}

	// 已是最新：HasUpdate=false
	if got := Decide("9.9.9", m, "", 0); got.HasUpdate {
		t.Error("同版本不该报有新版本")
	}
	// 低于最低支持版本 → TooOld
	if got := Decide("0.0.1", &Manifest{Version: "0.0.1", MinSupported: "1.0.0"}, "", 0); !got.TooOld {
		t.Error("低于 min_supported 应标 TooOld")
	}
}

func TestCandidateDerivationAndOrdering(t *testing.T) {
	raw := "https://raw.githubusercontent.com/o/r/main/version.json"
	cands := Candidates(raw)
	if len(cands) < 2 {
		t.Fatalf("GitHub raw 地址应推出多个等价候选（回退用），got %v", cands)
	}
	if cands[0] != raw {
		t.Errorf("主地址必须排第一，got %v", cands)
	}
	for _, c := range cands {
		if !strings.HasPrefix(c, "https://") {
			t.Errorf("候选只应是 https，got %q", c)
		}
	}
	// 非 raw 形态（自建 CDN / 内网）**不做推导**：多试几个第三方站点没有依据
	plain := "https://example.com/v.json"
	if got := Candidates(plain); len(got) != 1 || got[0] != plain {
		t.Errorf("非 raw 地址不该被推导，got %v", got)
	}
	// 候选顺序：上一次成功的那个优先（避免每次先在不可达地址上白等超时）
	ordered := OrderCandidates(cands, cands[len(cands)-1])
	if ordered[0] != cands[len(cands)-1] {
		t.Errorf("上次成功的候选应排第一，got %v", ordered)
	}
}

func TestParseBodyAcceptsBothShapes(t *testing.T) {
	// ① 裸清单 JSON（raw / 镜像返回的就是它）
	bare := `{"version":"1.2.3","notes":"n","assets":[{"os":"windows","arch":"amd64","url":"u","sha256":"h"}]}`
	m, err := ParseBody([]byte(bare))
	if err != nil || m == nil || m.Version != "1.2.3" {
		t.Fatalf("裸清单应解析成功，got %v %v", m, err)
	}
	// ② Contents API 信封（content 是 base64）—— 同一条链路的第二种形态
	inner := base64.StdEncoding.EncodeToString([]byte(`{"version":"2.0.0","notes":"n2"}`))
	env := `{"name":"version.json","content":"` + inner + `","encoding":"base64"}`
	m2, err := ParseBody([]byte(env))
	if err != nil || m2 == nil || m2.Version != "2.0.0" {
		t.Fatalf("Contents API 信封应解析成功，got %v %v", m2, err)
	}
	// ③ 噪声（HTML 错误页 / 空体）→ 明确报错，不静默返回空清单
	if m3, err := ParseBody([]byte("<html>404</html>")); err == nil || m3 != nil {
		t.Errorf("噪声响应应报错，got %v %v", m3, err)
	}
}

// TestJSONKeyContract 对外键名契约（前端逐字读取，改名即改协议）。
func TestJSONKeyContract(t *testing.T) {
	st := Status{
		Current: "1.0.0", Latest: "1.1.0", HasUpdate: true, Severity: "minor",
		Asset:    &Asset{OS: "windows", Arch: "amd64", URL: "u", SHA256: "h", Size: 7},
		Download: &Download{State: "none", Version: "1.1.0", ApplyHint: "run"},
	}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"current", "latest", "has_update", "severity", "asset", "download"} {
		if _, ok := got[k]; !ok {
			t.Errorf("下发 JSON 缺少键 %q（前端读它）", k)
		}
	}
	// Go 字段名**不得**出现在下发 JSON 里（v0.29.0 的提现按钮点不动就是这么来的）
	for _, k := range []string{"Current", "HasUpdate", "Download", "Asset"} {
		if _, ok := got[k]; ok {
			t.Errorf("下发 JSON 出现了 Go 字段名 %q —— 说明有字段缺 json tag", k)
		}
	}
	asset := got["asset"].(map[string]any)
	for _, k := range []string{"os", "arch", "url", "sha256", "size"} {
		if _, ok := asset[k]; !ok {
			t.Errorf("asset 缺少键 %q（下载校验读它）", k)
		}
	}
	dl := got["download"].(map[string]any)
	for _, k := range []string{"state", "version", "apply_hint"} {
		if _, ok := dl[k]; !ok {
			t.Errorf("download 缺少键 %q", k)
		}
	}
}

func TestCacheTTLIsSane(t *testing.T) {
	if CacheTTL <= 0 || CacheTTL > 24*time.Hour {
		t.Errorf("CacheTTL=%v 不合理（既不该每次都出站，也不该长到看不出新版本）", CacheTTL)
	}
	if DefaultManifestURL == "" {
		t.Error("DefaultManifestURL 不该为空：没默认地址 = 提醒功能形同不存在")
	}
}
