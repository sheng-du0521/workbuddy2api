// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package upstream

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy2api/internal/auth"
)

// newCheckinClient 造一个只指向 httptest 的 Client（billing 域）。
func newCheckinClient(t *testing.T, h http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := &Client{BillingBaseCN: srv.URL, HTTP: srv.Client()}
	return c, srv
}

// TestCheckinStatusActivityPath 新接口优先：命中 checkin-activity-status 即返回，
// 不应再打旧接口（避免双倍出站 + 状态被旧接口覆盖）。
func TestCheckinStatusActivityPath(t *testing.T) {
	paths := []string{}
	c, _ := newCheckinClient(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path != checkinActivityStatusPath {
			t.Errorf("不该访问 %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"today_checked_in":true}}`))
	})
	st, err := c.CheckinStatus(&auth.Auth{UID: "u1", AccessToken: "at"})
	if err != nil {
		t.Fatalf("CheckinStatus: %v", err)
	}
	if !st.Supported || !st.CheckedIn {
		t.Fatalf("应支持且已签到，得 %+v", st)
	}
	if len(paths) != 1 || paths[0] != checkinActivityStatusPath {
		t.Fatalf("应只请求新接口一次，实际 %v", paths)
	}
}

// TestCheckinStatusFallback 新接口失败 → 回退旧接口，并用 camelCase 字段名。
func TestCheckinStatusFallback(t *testing.T) {
	paths := []string{}
	c, _ := newCheckinClient(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == checkinActivityStatusPath {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":404,"msg":"not found"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"todayCheckedIn":true}}`))
	})
	st, err := c.CheckinStatus(&auth.Auth{UID: "u1", AccessToken: "at"})
	if err != nil {
		t.Fatalf("CheckinStatus: %v", err)
	}
	if !st.CheckedIn {
		t.Fatalf("回退路径应解析出已签到，得 %+v", st)
	}
	if len(paths) != 2 {
		t.Fatalf("应依次请求两条路径，实际 %v", paths)
	}
}

// TestCheckinStatusFalse 明确返回 false 时不得被当成「已签到」。
func TestCheckinStatusFalse(t *testing.T) {
	c, _ := newCheckinClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"today_checked_in":false}}`))
	})
	st, err := c.CheckinStatus(&auth.Auth{UID: "u1", AccessToken: "at"})
	if err != nil {
		t.Fatalf("CheckinStatus: %v", err)
	}
	if st.CheckedIn {
		t.Fatalf("应为未签到，得 %+v", st)
	}
}

// TestCheckinStatusBothFail 两条路径都失败 → 返回 error（调用方按「查询失败」展示），
// 绝不能把网络故障伪装成「未签到」——那会诱导用户重复补签。
func TestCheckinStatusBothFail(t *testing.T) {
	n := 0
	c, _ := newCheckinClient(t, func(w http.ResponseWriter, r *http.Request) {
		n++
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":500,"msg":"boom"}`))
	})
	st, err := c.CheckinStatus(&auth.Auth{UID: "u1", AccessToken: "at"})
	if err == nil {
		t.Fatalf("两条路径都失败应返回 error，得 %+v", st)
	}
	if n != 2 {
		t.Fatalf("应两条路径各试一次，实际 %d", n)
	}
}

// TestCheckinStatusGlobalUnsupported 国际版不发请求，直接回报「不支持」。
func TestCheckinStatusGlobalUnsupported(t *testing.T) {
	called := 0
	c, _ := newCheckinClient(t, func(w http.ResponseWriter, r *http.Request) {
		called++
	})
	c.GlobalEnabled = true
	st, err := c.CheckinStatus(&auth.Auth{UID: "u1", AccessToken: "at", Domain: "workbuddy.ai"})
	if err != nil {
		t.Fatalf("CheckinStatus: %v", err)
	}
	if st.Supported {
		t.Fatalf("国际版应报不支持，得 %+v", st)
	}
	if called != 0 {
		t.Fatalf("国际版不该发请求，实际 %d 次", called)
	}
}
