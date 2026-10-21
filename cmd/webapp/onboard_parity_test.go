// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"unicode/utf16"
)

// ======================================================================
// 「work官组」两类一致的回归网（v0.31.83）
//
// 用户的要求是"两种方式在默认配置、鉴权、调用参数、可用性判断、异常处理上一致"。
// "一致"不能靠两段代码各自小心 —— 必须由**同一段计算 + 同一把尺子**保证。
// 所以这里按 5 个维度逐条对拍：每一条都同时问两类 Key，任何一边单独走样都会红。
//
//	① 默认配置：两类都有非空默认值，且默认就是自己的组名（开箱即用）
//	② 候选构成：组名固定排第一，随后是具体模型
//	③ 鉴权：跨用户 404、停用 403 —— 两类完全一样（同一个 userGuard + 同一条 SQL 条件）
//	④ 可用性判断：两类都走 expandGroupModel；成员为空时**都不展开**（ok=false）
//	⑤ 异常处理：串用组名时 400，且 serve 与 onboard 两处给出**同一个 code**
// ======================================================================

// parityFixture 一对"同构"的 Key：池计费 + 代挂自用。
type parityFixture struct {
	store *Store
	a     *app
	owner *User
	pool  *APIKey
	host  *APIKey
}

func newParityFixture(t *testing.T) *parityFixture {
	t.Helper()
	store, a, _, _ := newAuthzApp(t)
	t.Cleanup(func() { store.Close() })
	withModelsCache(t, []string{"m-one", "m-two"})

	owner, err := store.CreateUser("parity-owner", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	pool, err := store.CreateAPIKey(owner.ID, "pool", "ph1", "", "sk-ph1", "", "", "")
	if err != nil {
		t.Fatalf("CreateAPIKey(pool): %v", err)
	}
	// 绑一个池里存在的账号（az-acct 由 newAuthzApp 塞进号池）+ 一个不存在的，
	// 只为验证"绑定账号集合"这条路走得通；是否真有可用模型不影响本节断言。
	host, err := store.CreateAPIKey(owner.ID, "host", "ph2", "", "sk-ph2",
		KeyKindHost, "", "az-acct,nope")
	if err != nil {
		t.Fatalf("CreateAPIKey(host): %v", err)
	}
	return &parityFixture{store: store, a: a, owner: owner, pool: pool, host: host}
}

// options 调一次 /api/onboard/options，返回 (状态码, 响应)。
func (f *parityFixture) options(t *testing.T, who *User, keyID int64) (int, onboardOptionsResp) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/onboard/options?key_id="+itoa(int(keyID)), nil)
	req = withUser(req, who)
	rec := httptest.NewRecorder()
	f.a.handleOnboardOptions(rec, req)
	var out onboardOptionsResp
	if rec.Code == http.StatusOK {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out
}

// TestOnboardParity 两类 Key 在五个维度上必须同构。
func TestOnboardParity(t *testing.T) {
	f := newParityFixture(t)
	cases := []struct {
		name      string
		key       *APIKey
		wantKind  string
		wantGroup string
		wantLabel string
	}{
		{"池计费 Key", f.pool, "", "wb-group-work", "号池官组"},
		{"代挂自用 Key", f.host, KeyKindHost, "wb-group-host", "自挂官组"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// ---- ③ 鉴权：本人 200，跨用户 404 ----
			code, opt := f.options(t, f.owner, c.key.ID)
			if code != http.StatusOK {
				t.Fatalf("本人取接入计划应 200，得到 %d", code)
			}
			other, err := f.store.CreateUser("parity-other-"+c.key.Name, "x")
			if err != nil {
				t.Fatalf("CreateUser(other): %v", err)
			}
			if code, _ := f.options(t, other, c.key.ID); code != http.StatusNotFound {
				t.Errorf("跨用户应 404（不泄露存在性），得到 %d", code)
			}
			// 停用 → 403（两类一致，不能一个 403 一个 400）
			if err := f.store.SetAPIKeyStatus(f.owner.ID, c.key.ID, StatusBanned); err != nil {
				t.Fatalf("SetAPIKeyStatus: %v", err)
			}
			if code, _ := f.options(t, f.owner, c.key.ID); code != http.StatusForbidden {
				t.Errorf("停用后应 403，得到 %d", code)
			}
			if err := f.store.SetAPIKeyStatus(f.owner.ID, c.key.ID, StatusActive); err != nil {
				t.Fatalf("恢复状态: %v", err)
			}
			code, opt = f.options(t, f.owner, c.key.ID)

			// ---- ① 默认配置：非空，且默认就是自己的组名（开箱即用）----
			if opt.Default == "" {
				t.Error("默认模型不能为空 —— 那等于要求用户自己配")
			}
			if opt.Default != c.wantGroup {
				t.Errorf("默认模型 = %q，期望自己的组名 %q（开箱即用）", opt.Default, c.wantGroup)
			}
			if opt.GroupName != c.wantGroup || opt.GroupLabel != c.wantLabel {
				t.Errorf("组名/标签 = %q/%q，期望 %q/%q", opt.GroupName, opt.GroupLabel, c.wantGroup, c.wantLabel)
			}
			if opt.Kind != c.wantKind {
				t.Errorf("kind = %q，期望 %q", opt.Kind, c.wantKind)
			}

			// ---- ② 候选构成：组名固定排第一 ----
			if len(opt.Models) == 0 || opt.Models[0] != c.wantGroup {
				t.Errorf("候选 = %v，第一项必须是组名 %q", opt.Models, c.wantGroup)
			}
			for i, m := range opt.Models {
				if i > 0 && m == c.wantGroup {
					t.Errorf("组名在候选里出现了两次（位置 %d）", i)
				}
			}

			// ---- ④ 可用性判断：两类都走 expandGroupModel，且成员为空都不展开 ----
			// 让"本组成员"为空（把选配配成一个都不存在的模型名 → 交集为空），
			// 此时 expandGroupModel 必须对两类都给 ok=false（都不假装能展开）。
			if err := f.store.SetKV(kvOnboardModels, ""); err != nil {
				t.Fatal(err)
			}
			if err := f.store.SetKV(kvOnboardModelsHost, ""); err != nil {
				t.Fatal(err)
			}
			px := f.a.proxy
			if px == nil {
				t.Skip("无 proxy，跳过可用性判断")
			}
			// "有成员 ⇔ 能展开"：两类都走 expandGroupModel 同一实现，
			// 不存在"一种有成员也不展开、另一种没成员却硬展开"。
			hostMembers := px.hostGroupMembers(f.host)
			_, _, okHost := px.expandGroupModel(groupVirtualModel(groupHostID), f.host)
			if (len(hostMembers) > 0) != okHost {
				t.Errorf("自挂官组：成员 %d 个，展开=%v —— 两者必须一致（有成员才能展开）",
					len(hostMembers), okHost)
			}
			poolMembers, _, _ := px.groupMembersFor(groupWorkID, nil)
			_, _, okWork := px.expandGroupModel(groupVirtualModel(groupWorkID), &APIKey{})
			if (len(poolMembers) > 0) != okWork {
				t.Errorf("号池官组：成员 %d 个，展开=%v —— 两者必须一致", len(poolMembers), okWork)
			}
		})
	}
}

// TestGroupKindMismatchSymmetric 串用组名：两个方向都拒，且 serve 与 onboard 同一个 code。
func TestGroupKindMismatchSymmetric(t *testing.T) {
	// 纯函数层：两个方向对称
	code, _, hint := groupKindMismatch(KeyKindHost, groupWorkID)
	if code != "host_key_cannot_use_pool_group" || hint == "" {
		t.Errorf("代挂 Key + 号池官组 = (%q, %q)，期望 host_key_cannot_use_pool_group + hint", code, hint)
	}
	code, _, hint = groupKindMismatch("", groupHostID)
	if code != "pool_key_cannot_use_host_group" || hint == "" {
		t.Errorf("池计费 Key + 自挂官组 = (%q, %q)，期望 pool_key_cannot_use_host_group + hint", code, hint)
	}
	// 正确搭配与历史组名都不该被拦
	for _, c := range []struct{ kind, gid string }{
		{"", groupWorkID}, {KeyKindHost, groupHostID}, {KeyKindHost, "ch3"}, {"", "ch3"},
	} {
		if code, _, _ := groupKindMismatch(c.kind, c.gid); code != "" {
			t.Errorf("(%q, %q) 不该被判不匹配，得到 %q", c.kind, c.gid, code)
		}
	}

	// handler 层：onboard 用的也是这同一个判定（码必须一致）
	f := newParityFixture(t)
	body := `{"target":"workbuddy","model":"wb-group-work","os":"unix"}`
	req := httptest.NewRequest(http.MethodPost, "/api/keys/1/onboard", strings.NewReader(body))
	req.SetPathValue("id", itoa(int(f.host.ID)))
	req = withUser(req, f.owner)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.a.handleKeyOnboard(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("代挂 Key 接号池官组应 400，得到 %d（%s）", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["code"] != "host_key_cannot_use_pool_group" {
		t.Errorf("onboard 的 code = %v，必须与 groupKindMismatch 一致", out["code"])
	}
}

// TestOnboardScriptParity 调用参数一致：**不传 model** 时，两类 Key 各自拿到自己的组名，
// 且生成的脚本除 model / key 值外结构完全相同。
//
// 这条覆盖的是"系统自动按默认会配置的模型来完成接入"—— 不只是弹窗替你填好，
// 接口层也认这个默认：调用方（脚本 / 第三方）不传 model 也能接出正确配置。
func TestOnboardScriptParity(t *testing.T) {
	f := newParityFixture(t)
	mk := func(name, kind string) *APIKey {
		plain := GenerateAPIKey()
		enc, err := f.a.secret.Encrypt(plain)
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
		k, err := f.store.CreateAPIKey(f.owner.ID, name, HashAPIKey(plain), enc,
			keyPrefix(plain), kind, "", "az-acct")
		if err != nil {
			t.Fatalf("CreateAPIKey(%s): %v", name, err)
		}
		return k
	}
	pool := mk("script-pool", "")
	host := mk("script-host", KeyKindHost)

	gen := func(k *APIKey, body string) (int, map[string]any) {
		req := httptest.NewRequest(http.MethodPost, "/api/keys/1/onboard", strings.NewReader(body))
		req.SetPathValue("id", itoa(int(k.ID)))
		req = withUser(req, f.owner)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		f.a.handleKeyOnboard(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	// ① 不传 model → 各自用默认组名。
	codeP, outP := gen(pool, `{"target":"workbuddy","os":"windows"}`)
	codeH, outH := gen(host, `{"target":"workbuddy","os":"windows"}`)
	if codeP != http.StatusOK || codeH != http.StatusOK {
		t.Fatalf("不传 model 也应成功：pool=%d host=%d（%v / %v）", codeP, codeH, outP, outH)
	}
	if outP["model"] != "wb-group-work" {
		t.Errorf("池 Key 的默认 model = %v，期望 wb-group-work", outP["model"])
	}
	if outH["model"] != "wb-group-host" {
		t.Errorf("代挂 Key 的默认 model = %v，期望 wb-group-host", outH["model"])
	}

	// ② 脚本结构一致：行数相同、关键字段都在、没有串用对方的组名。
	sp, _ := outP["script"].(string)
	sh, _ := outH["script"].(string)
	if sp == "" || sh == "" {
		t.Fatal("脚本不能为空")
	}
	if lp, lh := strings.Count(sp, "\n"), strings.Count(sh, "\n"); lp != lh {
		t.Errorf("两份脚本行数不同（%d vs %d）—— 结构应当完全一致", lp, lh)
	}
	// .bat 的正文是**base64 编码的 PowerShell**：明文字段（apiKey / model）必须解码后搜，
	// 直接在 .bat 原文里找是找不到的（第一版就这么写错过）。
	psP := decodePSFromBat(t, sp)
	psH := decodePSFromBat(t, sh)
	for _, kw := range []string{"apiKey", "useCustomProtocol", "vendor"} {
		if !strings.Contains(psP, kw) || !strings.Contains(psH, kw) {
			t.Errorf("两份脚本都应包含 %q（pool=%v host=%v）", kw,
				strings.Contains(psP, kw), strings.Contains(psH, kw))
		}
	}
	if !strings.Contains(psP, `"id": "wb-group-work"`) {
		t.Error("池 Key 的脚本里 model 不是 wb-group-work")
	}
	if !strings.Contains(psH, `"id": "wb-group-host"`) {
		t.Error("代挂 Key 的脚本里 model 不是 wb-group-host")
	}
	// url 必须是**完整端点**（v0.31.84）：useCustomProtocol=true 时客户端原样发送，
	// 写成 base 会让 WorkBuddy 直接 POST 到 /v1 → internal error 404。
	for name, ps := range map[string]string{"池 Key": psP, "代挂 Key": psH} {
		if !strings.Contains(ps, "/v1/chat/completions") {
			t.Errorf("%s 的脚本 url 不是完整端点（缺 /v1/chat/completions）", name)
		}
	}
	if strings.Contains(psP, "wb-group-host") || strings.Contains(psH, "wb-group-work") {
		t.Error("脚本里串了对方的组名 —— 默认值没有按 Key 类型分流")
	}
}

// TestPostV1AliasRegistered POST /v1 必须与 /v1/chat/completions 同一条链路。
//
// 为什么需要这条别名（v0.31.84）：WorkBuddy 的 useCustomProtocol 把 models.json 里的
// url 当完整端点原样发送；老接入脚本写的是 base（`…/v1`），于是它 POST 到 `/v1`，
// 表现为 `internal error 404`。别名让"老脚本"不用重跑也能立刻恢复。
func TestPostV1AliasRegistered(t *testing.T) {
	store, _, mux, _ := newAuthzApp(t)
	defer store.Close()

	for _, path := range []string{"/v1", "/v1/chat/completions"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"x"}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code == http.StatusNotFound {
			t.Errorf("%s 未注册（客户端会看到 internal error 404）", path)
			continue
		}
		// 两条路由必须**同样要求鉴权**：别名不是放宽权限的后门。
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s 无 Key 时应 401，得到 %d —— 别名与正式路由的鉴权不一致", path, rec.Code)
		}
	}
}

// decodePSFromBat 从 .bat 的 -EncodedCommand 里解出 PowerShell 原文（UTF-16LE）。
func decodePSFromBat(t *testing.T, bat string) string {
	t.Helper()
	re := regexp.MustCompile(`-EncodedCommand\s+([A-Za-z0-9+/=]+)`)
	m := re.FindStringSubmatch(bat)
	if len(m) != 2 {
		t.Fatalf("脚本里没有 -EncodedCommand：%s", bat[:min(len(bat), 200)])
	}
	raw, err := base64.StdEncoding.DecodeString(m[1])
	if err != nil {
		t.Fatalf("base64 解码失败：%v", err)
	}
	u := make([]uint16, 0, len(raw)/2)
	for i := 0; i+1 < len(raw); i += 2 {
		u = append(u, uint16(raw[i])|uint16(raw[i+1])<<8)
	}
	return string(utf16.Decode(u))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
