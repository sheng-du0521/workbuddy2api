// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/egress"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

func TestSplitKVSet(t *testing.T) {
	if got := splitKVSet(""); got != nil {
		t.Fatalf("空串应为 nil（不过滤），得 %#v", got)
	}
	if got := splitKVSet(" , , "); got != nil {
		t.Fatalf("全空白应为 nil，得 %#v", got)
	}
	got := splitKVSet("a, b ,c,,a")
	if len(got) != 3 || !got["a"] || !got["b"] || !got["c"] {
		t.Fatalf("应去重去空得 {a,b,c}，得 %#v", got)
	}
}

func TestSanitizeCSV(t *testing.T) {
	if got := sanitizeCSV(" a , b ,, a , c "); got != "a,b,c" {
		t.Fatalf("sanitizeCSV = %q，期望 a,b,c", got)
	}
	if got := sanitizeCSV("   "); got != "" {
		t.Fatalf("全空白应为空串，得 %q", got)
	}
}

// TestValidateProxyURL 代理校验自 v0.14.0 起统一走 internal/egress。
//
// 与旧本地实现的两点差异（正是本次要修的「保存通过、运行期静默失效」）：
//   - socks5h 不再放行：Go 的 http.Transport 不认这个 scheme，旧实现放过它，
//     结果保存成功、使用时静默回落直连；
//   - 端口必填：旧实现只查 Host 非空，"http://1.2.3.4" 也能存进去。
func TestValidateProxyURL(t *testing.T) {
	ok := []string{"http://1.2.3.4:8080", "https://user:pass@host:443", "socks5://1.2.3.4:1080", ""}
	for _, u := range ok {
		if _, err := egress.Validate(u); err != nil {
			t.Fatalf("应通过：%s（%v）", u, err)
		}
	}
	bad := []string{"1.2.3.4:8080", "ftp://h:1", "http://", "://x", "socks5h://h:1", "http://1.2.3.4"}
	for _, u := range bad {
		if _, err := egress.Validate(u); err == nil {
			t.Fatalf("应拒绝：%s", u)
		}
	}
}

func TestNormHours(t *testing.T) {
	if got := normHours(nil, []int{9, 21}); len(got) != 2 || got[0] != 9 || got[1] != 21 {
		t.Fatalf("空输入应回落默认，得 %v", got)
	}
	if got := normHours([]int{21, 9, 9, -1, 25}, []int{}); len(got) != 2 || got[0] != 9 || got[1] != 21 {
		t.Fatalf("应去重/过滤/升序得 [9 21]，得 %v", got)
	}
	if got := normHours([]int{5}, []int{}); len(got) != 1 || got[0] != 5 {
		t.Fatalf("单值应保留，得 %v", got)
	}
}

func TestTaskConfigRoundTrip(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "tc.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	// 无记录 → 缺省。
	d := store.LoadTaskConfig()
	if !d.Enabled || d.JitterMinutes != 5 || len(d.CheckinHours) != 2 {
		t.Fatalf("缺省配置不符：%+v", d)
	}

	// 存→读，且非法值被规整。
	in := DefaultTaskConfig()
	in.Enabled = false
	in.CheckinHours = []int{7, 7, 30}
	in.KeepaliveDays = -3 // 应被收敛到 0
	in.LazyRefreshHours = 12
	if err := store.SaveTaskConfig(in); err != nil {
		t.Fatalf("SaveTaskConfig: %v", err)
	}
	out := store.LoadTaskConfig()
	if out.Enabled {
		t.Fatal("Enabled 应为 false")
	}
	if len(out.CheckinHours) != 1 || out.CheckinHours[0] != 7 {
		t.Fatalf("CheckinHours 应规整为 [7]，得 %v", out.CheckinHours)
	}
	if out.KeepaliveDays != 0 {
		t.Fatalf("KeepaliveDays 负值应收敛为 0，得 %d", out.KeepaliveDays)
	}
	if out.LazyRefreshHours != 12 {
		t.Fatalf("LazyRefreshHours 应保留 12，得 %d", out.LazyRefreshHours)
	}
}

func TestAccountGroupEgressPersist(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "acc.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	if err := store.UpsertAccount(&Account{UID: "u1", Realm: "cn", AccessToken: "a"}); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	if err := store.SetAccountGroup("u1", "反代"); err != nil {
		t.Fatalf("SetAccountGroup: %v", err)
	}
	if err := store.SetAccountEgress("u1", "socks5://1.2.3.4:1080"); err != nil {
		t.Fatalf("SetAccountEgress: %v", err)
	}
	// 再 Upsert（模拟 OAuth/token 回写等路径）不得把用户设置的分组/代理冲掉。
	if err := store.UpsertAccount(&Account{UID: "u1", Realm: "cn", AccessToken: "b", Nickname: "n"}); err != nil {
		t.Fatalf("re-Upsert: %v", err)
	}
	acc, err := store.GetAccount("u1")
	if err != nil || acc == nil {
		t.Fatalf("GetAccount: %v %v", acc, err)
	}
	if acc.Group != "反代" {
		t.Fatalf("分组被覆盖：%q", acc.Group)
	}
	if acc.EgressProxy != "socks5://1.2.3.4:1080" {
		t.Fatalf("出站代理被覆盖：%q", acc.EgressProxy)
	}
}

// newSelectionProxy 造一个只装了账号池的最小 Proxy（选号过滤/节流/出站测试用）。
// 账号统一按「反代」消耗分组登记（等价于平台自持账号的推断结果，v0.13.0），
// 使这些用例聚焦白名单 / 节流逻辑本身，而不受消耗分组影响。
func newSelectionProxy(auths ...*auth.Auth) *Proxy {
	p := pool.New("")
	p.SetMaxInFlight(0)
	for _, a := range auths {
		p.Add(a)
	}
	px := &Proxy{
		uc:                upstream.New(),
		pool:              p,
		lastHit:           map[string]int64{},
		clients:           map[string]*http.Client{},
		groupByUID:        map[string]string{},
		egressByUID:       map[string]string{},
		consumeGroupByUID: map[string]string{},
	}
	for _, a := range auths {
		px.consumeGroupByUID[a.UID] = cgProxy
	}
	return px
}

func TestSelectionGroupFilter(t *testing.T) {
	px := newSelectionProxy(
		&auth.Auth{UID: "alpha", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999},
		&auth.Auth{UID: "beta", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999},
	)
	px.groupByUID["alpha"] = "反代"
	px.groupByUID["beta"] = "闲置"
	// 白名单自 v0.13.1 起同时认「消耗分组」名（账号卡片已只暴露消耗分组）。
	// beta 显式设为「用户申请」：它参与消耗，但既不是传统分组「反代」也不是
	// 消耗分组「反代」，因此仍应被白名单排除——比用「闲置」更能证明是**白名单**
	// 而非「不参与消耗」把它挡掉的。
	px.consumeGroupByUID["alpha"] = cgProxy
	px.consumeGroupByUID["beta"] = cgApply

	// 未配置过滤 → 全放行。
	if !px.allowsSelection("alpha", pickScope{}) || !px.allowsSelection("beta", pickScope{}) {
		t.Fatal("未配置过滤时应全放行")
	}

	// 只允许「反代」分组 → beta 被排除。
	px.allowGroups = map[string]bool{"反代": true}
	if !px.allowsSelection("alpha", pickScope{}) {
		t.Fatal("alpha 属「反代」应放行")
	}
	if px.allowsSelection("beta", pickScope{}) {
		t.Fatal("beta 不属「反代」应被排除")
	}
	for i := 0; i < 50; i++ {
		au := px.pickExcluding(map[string]bool{}, "", pickScope{})
		if au == nil || au.UID != "alpha" {
			t.Fatalf("应始终选到 alpha，得 %v", au)
		}
	}

	// 白名单也认消耗分组中文名：只放行「用户申请」时 alpha（反代）应被排除。
	px.allowGroups = map[string]bool{"用户申请": true}
	if px.allowsSelection("alpha", pickScope{}) {
		t.Fatal("白名单「用户申请」时 alpha（消耗分组=反代）应被排除")
	}
	if !px.allowsSelection("beta", pickScope{}) {
		t.Fatal("白名单「用户申请」时 beta（消耗分组=用户申请）应放行")
	}

	// 追加 uid 白名单：组内但不在白名单 → 仍排除。
	px.allowGroups = map[string]bool{"反代": true}
	px.allowUIDs = map[string]bool{"gamma": true}
	if px.allowsSelection("alpha", pickScope{}) {
		t.Fatal("uid 白名单不含 alpha 时应排除")
	}
}

func TestSelectionPacingSkipsRecent(t *testing.T) {
	px := newSelectionProxy(
		&auth.Auth{UID: "alpha", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999},
		&auth.Auth{UID: "beta", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999},
	)
	// 无间隔 → 不节流。
	if px.recentlyHit("alpha") {
		t.Fatal("minGap=0 时不应判为 recently hit")
	}
	// 设 1 小时间隔并记账 alpha 刚被用过 → 选号应避开 alpha，落到 beta。
	px.minGap = time.Hour
	px.noteHit("alpha")
	if !px.recentlyHit("alpha") {
		t.Fatal("noteHit 后应判为 recently hit")
	}
	for i := 0; i < 50; i++ {
		au := px.pickExcluding(map[string]bool{}, "", pickScope{})
		if au == nil || au.UID != "beta" {
			t.Fatalf("应避开 alpha 选到 beta，得 %v", au)
		}
	}
	// 两个都刚用过 → 允许回落（仍返回某个可用账号，不死循环）。
	px.noteHit("beta")
	if au := px.pickExcluding(map[string]bool{}, "", pickScope{}); au == nil {
		t.Fatal("所有账号都在间隔内时应回落返回可用账号，而不是 nil")
	}
}

func TestEgressClientCache(t *testing.T) {
	px := newSelectionProxy()
	// 无代理 → 用共享直连客户端。
	c0, err := px.clientFor("any")
	if err != nil {
		t.Fatalf("未配代理不应报错: %v", err)
	}
	if c0 != px.uc.HTTP {
		t.Fatal("未配代理应回落共享客户端")
	}
	// 相同代理 → 缓存复用；不同代理 → 不同客户端。
	c1, err := px.egressClient("http://1.2.3.4:8080")
	if err != nil {
		t.Fatalf("合法代理不应报错: %v", err)
	}
	c2, _ := px.egressClient("http://1.2.3.4:8080")
	c3, _ := px.egressClient("socks5://5.6.7.8:1080")
	if c1 != c2 {
		t.Fatal("相同代理应复用同一 client")
	}
	if c1 == c3 {
		t.Fatal("不同代理应是不同 client")
	}
	// v0.14.0：代理非法时必须返回 error，**绝不**回落到直连客户端。
	// 这正是「配了代理却从本机 IP 出去」那个静默失效的回归护栏。
	for _, bad := range []string{"socks5h://1.2.3.4:1080", "1.2.3.4:8080", "http://1.2.3.4:99999", "http://:8080"} {
		c, err := px.egressClient(bad)
		if err == nil {
			t.Fatalf("非法代理 %q 应返回 error", bad)
		}
		if c != nil {
			t.Fatalf("非法代理 %q 不应返回客户端（否则就是静默直连）", bad)
		}
		if c == px.uc.HTTP {
			t.Fatalf("非法代理 %q 绝不能回落到共享直连客户端", bad)
		}
	}
	// 账号级出站优先于全局默认。
	px.egressByUID["u"] = "socks5://5.6.7.8:1080"
	px.defaultEgress = "http://1.2.3.4:8080"
	if px.EgressForUID("u") != "socks5://5.6.7.8:1080" {
		t.Fatalf("账号专用代理应优先，得 %q", px.EgressForUID("u"))
	}
	if px.EgressForUID("unknown") != "http://1.2.3.4:8080" {
		t.Fatalf("未配账号应回落默认，得 %q", px.EgressForUID("unknown"))
	}
}
