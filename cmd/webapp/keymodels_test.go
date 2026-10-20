// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ======================================================================
// 密钥可用模型清单的回归网（v0.31.74）
//
// 这个接口只有三个判断，但每一个错了都会把用户引到错误方向：
//  1. 归属：拿别人的 key id 必须 404（不能 403 —— 403 等于确认了"这把 key 存在"）；
//  2. 状态：停用的 key 403（前端据此把下拉置空并提示"已停用"，而不是显示一列灰模型）；
//  3. 代挂 Key 绑定号未入池：返回空 + reason，**不能**回落到池里别人的模型 ——
//     那会让用户选一个自己根本调不通的模型，比不给清单更糟。
// ======================================================================

// keyModelsCall 直接调 handler（绕过中间件），返回解析后的响应。
func keyModelsCall(t *testing.T, a *app, u *User, keyID string) (int, keyModelsResp) {
	t.Helper()
	if u == nil {
		u = &User{ID: 1}
	}
	req := httptest.NewRequest(http.MethodGet, "/api/keys/"+keyID+"/models", nil)
	req.SetPathValue("id", keyID)
	req = withUser(req, u)
	rec := httptest.NewRecorder()
	a.handleKeyModels(rec, req)
	var out keyModelsResp
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("响应不是合法 JSON：%v（%s）", err, rec.Body.String())
		}
	}
	return rec.Code, out
}

// TestKeyModelsOwnershipAndStatus 归属与状态：跨用户 404、停用 403。
func TestKeyModelsOwnershipAndStatus(t *testing.T) {
	store, a, _, _ := newAuthzApp(t)
	defer store.Close()
	_ = a.routes() // 确保路由表可构建（不会因本接口的注册破坏整体）

	owner, err := store.CreateUser("km-owner", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	other, err := store.CreateUser("km-other", "x")
	if err != nil {
		t.Fatalf("CreateUser(other): %v", err)
	}
	key, err := store.CreateAPIKey(owner.ID, "k1", "hash-k1", "", "sk-wb-km", "", "", "")
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	// ① 本人：200
	if code, resp := keyModelsCall(t, a, owner, itoa(int(key.ID))); code != http.StatusOK {
		t.Fatalf("本人取模型应为 200，得到 %d", code)
	} else if resp.Kind != "pool" {
		t.Errorf("池计费 Key 的 kind 应为 pool，得到 %q", resp.Kind)
	}

	// ② 别人：404（不是 403 —— 403 会泄露"这把 key 存在"）
	if code, _ := keyModelsCall(t, a, other, itoa(int(key.ID))); code != http.StatusNotFound {
		t.Errorf("跨用户应为 404，得到 %d", code)
	}

	// ③ 停用：403
	if err := store.SetAPIKeyStatus(owner.ID, key.ID, StatusBanned); err != nil {
		t.Fatalf("SetAPIKeyStatus: %v", err)
	}
	if code, _ := keyModelsCall(t, a, owner, itoa(int(key.ID))); code != http.StatusForbidden {
		t.Errorf("停用的密钥应为 403，得到 %d", code)
	}
}

// TestKeyModelsHostKeyNotInPool 代挂 Key 绑定的号不在池 → 空清单 + reason，且允许手填。
//
// 这条是"不撒谎"的底线：账号级模型清单在本项目里不存在（只有运行期观测），
// 号不在池就等于"我们不知道它能跑什么"。此时给池目录会让用户选到调不通的模型。
func TestKeyModelsHostKeyNotInPool(t *testing.T) {
	store, a, _, _ := newAuthzApp(t)
	defer store.Close()

	owner, err := store.CreateUser("km-host", "x")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	// 绑一个**不存在于池里**的 uid（newAuthzApp 只塞了 az-acct）。
	key, err := store.CreateAPIKey(owner.ID, "host-key", "hash-h", "", "sk-wb-h",
		KeyKindHost, "", "not-in-pool-uid")
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	code, resp := keyModelsCall(t, a, owner, itoa(int(key.ID)))
	if code != http.StatusOK {
		t.Fatalf("应为 200（清单为空不是错误），得到 %d", code)
	}
	if len(resp.Models) != 0 {
		t.Errorf("绑定的号不在池时不该回落到池目录，得到 %v", resp.Models)
	}
	if resp.Reason != "host_accounts_not_in_pool" {
		t.Errorf("reason 应为 host_accounts_not_in_pool，得到 %q", resp.Reason)
	}
	if !resp.ManualAllowed {
		t.Error("清单不可知时必须允许手填模型名，否则测试功能不可用")
	}
}
