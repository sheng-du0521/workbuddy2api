// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ======================================================================
// 代挂自用 Key 不许接入模型组（v0.31.81）
//
// 现场：用户一键接入后「自挂账号请求一直上游中断 + 404」。
// 根因不是账号坏了 —— 是接入弹窗把所有 Key 的默认模型都填成了 `wb-group-work`
// （v0.31.76 引入），而这个组名展开出来的是**平台号池**的模型；
// 代挂 Key 只在自己账号里选号，号主没有那个模型，上游必然 11102/404。
//
// 两道闸门都要有：
//  1. 生成脚本时拒绝（省用户一轮排查）；
//  2. 请求时拒绝（脚本可能是旧的、或者别人手填的）。
// 这里钉的是第一道；第二道在 ServeChat 里同一判据（同包同常量，不会漂移）。
// ======================================================================

func TestOnboardHostKeyRejectsGroup(t *testing.T) {
	store, a, _, _ := newAuthzApp(t)
	defer store.Close()

	owner, err := store.CreateUser("ob-host", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	host, err := store.CreateAPIKey(owner.ID, "host-key", "oh1", "", "sk-wb-oh",
		KeyKindHost, "", "u1")
	if err != nil {
		t.Fatalf("CreateAPIKey(host): %v", err)
	}
	pool, err := store.CreateAPIKey(owner.ID, "pool-key", "oh2", "", "sk-wb-op", "", "", "")
	if err != nil {
		t.Fatalf("CreateAPIKey(pool): %v", err)
	}

	call := func(keyID int64, model string) (int, map[string]any) {
		body := strings.NewReader(`{"target":"workbuddy","model":"` + model + `","os":"unix"}`)
		req := httptest.NewRequest(http.MethodPost, "/api/keys/1/onboard", body)
		req.SetPathValue("id", itoa(int(keyID)))
		req = withUser(req, owner)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		a.handleKeyOnboard(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	// ① 代挂 Key + **号池**官组 → 400，且给出可执行的下一步（指向 wb-group-host）。
	if code, out := call(host.ID, "wb-group-work"); code != http.StatusBadRequest {
		t.Errorf("代挂 Key 用号池官组应 400，得到 %d（%v）", code, out)
	} else if out["code"] != "host_key_cannot_use_pool_group" {
		t.Errorf("code = %v，期望 host_key_cannot_use_pool_group", out["code"])
	}

	// ①b 代挂 Key + **自挂**官组 → 放行（v0.31.82 起这是它的正当默认值）。
	// 加密不可用（测试库无 secret）时会走到 409/500，只要不是 400 + 上面那个 code 就算过。
	if code, out := call(host.ID, "wb-group-host"); code == http.StatusBadRequest &&
		out["code"] == "host_key_cannot_use_pool_group" {
		t.Errorf("代挂 Key 用自挂官组被误拦：%d %v", code, out)
	}

	// ①c 池计费 Key + 自挂官组 → 400（必须与 Key 类型匹配）。
	if code, out := call(pool.ID, "wb-group-host"); code != http.StatusBadRequest {
		t.Errorf("池计费 Key 用自挂官组应 400，得到 %d（%v）", code, out)
	} else if out["code"] != "pool_key_cannot_use_host_group" {
		t.Errorf("code = %v，期望 pool_key_cannot_use_host_group", out["code"])
	}

	// ② 代挂 Key + 具体模型名 → 放行（不能因为修 ① 把正常路径也挡了）。
	// 加密不可用（测试库无 secret）时会是 409/500，这不算本用例的失败：
	// 只要不是 400 + host_key_cannot_use_group 就说明闸门没有误伤。
	if code, out := call(host.ID, "glm-5.3"); code == http.StatusBadRequest &&
		(out["code"] == "host_key_cannot_use_pool_group" || out["code"] == "host_key_cannot_use_host_group") {
		t.Errorf("代挂 Key 用具体模型名被误拦：%d %v", code, out)
	}

	// ③ 池计费 Key + 组名 → 不该被这条规则拦（它是组名的合法使用者）。
	if code, out := call(pool.ID, "wb-group-work"); code == http.StatusBadRequest &&
		out["code"] == "host_key_cannot_use_pool_group" {
		t.Errorf("池计费 Key 被误拦：%d %v", code, out)
	}
}
