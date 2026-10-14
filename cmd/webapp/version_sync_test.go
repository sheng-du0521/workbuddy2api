// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// 本文件把「版本号三处同步」从"靠自觉"变成"靠测试"。
//
// 背景（v0.26.4 的实测发现）：`main.go` 的 `const version` 从 v0.26.0 起被漏改过三次
// （0.26.1 / 0.26.2 / 0.26.3 都没动）。它的消费点全是**间接**的 ——
// 启动 banner、/healthz（已鉴权）、/api/admin/live、/metrics，以及最要紧的
// **版本更新提醒的 current**。后果：current 恒为 0.26.0 而清单是 0.26.4 →
// 页面误报"有新版本"，诱导用户升级到自己已经在跑的版本；清单若也停旧则漏报真实更新。
//
// 为什么必须机制化：改版本号时没人会想起这个常量（它离改动点很远、又不影响测试），
// 只有让"两处不一致"直接红，才守得住。

// TestVersionMatchesManifest 程序内版本常量必须与仓库根 version.json 的 version 一致。
//
// 语义说明：`version.json` 是**发布清单**（分发给用户、供更新提醒比对），
// `const version` 是**这个二进制是什么版本**。发布时二者相等。
// 若某次确实需要短暂不一致（例如先提交代码、清单稍后单独发），
// 请**同时**评估影响：不一致期间更新提醒会误报或漏报 —— 这通常说明该一起改。
func TestVersionMatchesManifest(t *testing.T) {
	// 测试的 CWD 是包目录（cmd/webapp），仓库根在两级之上。
	p := filepath.Join("..", "..", "version.json")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读取 %s 失败：%v", p, err)
	}
	var m struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("解析 version.json 失败：%v", err)
	}
	if m.Version == "" {
		t.Fatal("version.json 缺 version 字段")
	}
	if version != m.Version {
		t.Errorf("版本号不同步：cmd/webapp 的 version=%q，version.json=%q\n"+
			"（发版时两处必须一起改；不一致会让「版本更新提醒」误报/漏报）",
			version, m.Version)
	}
}

// TestVersionLooksLikeSemver 版本号必须是 x.y.z（更新提醒的比较器只认这个形态）。
//
// 为什么单独断言：decideUpdate 用 parseSemver 解析，非法形态会被判成"无法比较"并
// 静默降级（不报错、也不提醒）—— 又是一处"看起来正常"的失败。
func TestVersionLooksLikeSemver(t *testing.T) {
	re := regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	if !re.MatchString(version) {
		t.Errorf("version = %q，want x.y.z 形态（更新提醒比较器的要求）", version)
	}
}

// TestVersionIsRenderedInHealth 版本号确实到达了对外接口（常量不是死代码）。
//
// 这一条防的是反向错误：有人重构时把 version 从出口摘掉，更新提醒仍能工作
// （它直接用常量），但运维再也看不到版本 —— "二进制版本与界面不符"这类问题只能靠猜。
//
// **只测 /healthz**（v0.26.4 的选择）：live / metrics 也消费同一个常量
// （live.go 的 handleLive、metrics.go 的 wb_webapp_info），但它们依赖完整装配
// （限流器、出口池等），在最小测试 app 上会 nil panic —— 为断言一个字段去搭整套
// 装配不划算。选 /healthz 是因为它**自带一层语义**值得锁：**未鉴权时不得带版本**
// （版本号会告诉扫描器"这台机器在跑哪个已知有洞的版本"，见 handleHealth 说明）。
func TestVersionIsRenderedInHealth(t *testing.T) {
	store, px, a := newTestApp(t)
	defer store.Close()
	defer px.Close()

	// ① 未鉴权：不带 version。
	w := httptest.NewRecorder()
	a.handleHealth(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("/healthz HTTP %d", w.Code)
	}
	var anon map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &anon); err != nil {
		t.Fatalf("解析 healthz 响应失败：%v", err)
	}
	if _, ok := anon["version"]; ok {
		t.Errorf("未鉴权的 /healthz 不得带 version：%v", anon)
	}
	if anon["status"] != "ok" {
		t.Errorf("未鉴权仍应返回 status=ok（探活不能被鉴权挡住）：%v", anon)
	}

	// ② 已鉴权：带 version，且等于当前版本常量。
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req2.Header.Set("X-Admin-Key", "v012-admin-key") // newTestApp 装配的 app.key
	a.handleHealth(w2, req2)
	var auth map[string]any
	if err := json.Unmarshal(w2.Body.Bytes(), &auth); err != nil {
		t.Fatalf("解析 healthz 响应失败：%v", err)
	}
	if auth["version"] != version {
		t.Errorf("已鉴权的 /healthz 的 version = %v, want %q", auth["version"], version)
	}
}
