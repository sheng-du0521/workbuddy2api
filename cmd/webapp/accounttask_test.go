// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"path/filepath"
	"testing"
)

// TestAccountTaskCfgRoundTrip 校验任务配置的序列化 / 解析 / 收敛。
func TestAccountTaskCfgRoundTrip(t *testing.T) {
	// 缺省（全参与）→ 空串，保持库干净。
	if got := marshalAccountTaskCfg(AccountTaskConfig{Enabled: true}); got != "" {
		t.Fatalf("缺省配置应序列化为空串，得 %q", got)
	}
	// 关闭总开关 → 非空 JSON。
	off := AccountTaskConfig{Enabled: false}
	raw := marshalAccountTaskCfg(off)
	if raw == "" {
		t.Fatal("关闭总开关应序列化为非空 JSON")
	}
	if c := parseAccountTaskCfg(raw); c.Enabled {
		t.Fatalf("解析回应为 enabled=false，得 %+v", c)
	}
	// 非法任务名被过滤 + 去重 + 排序。
	c := normalizeAccountTaskCfg(AccountTaskConfig{Enabled: true, Disabled: []string{"travel", "bogus", "travel", "checkin"}})
	if len(c.Disabled) != 2 || c.Disabled[0] != "checkin" || c.Disabled[1] != "travel" {
		t.Fatalf("Disabled 应收敛为 [checkin travel]，得 %v", c.Disabled)
	}
	// 空串/非法 JSON 解析回落「全参与」。
	if pc := parseAccountTaskCfg(""); !pc.Enabled {
		t.Fatalf("空串应回落 enabled=true，得 %+v", pc)
	}
	if pc := parseAccountTaskCfg("{not json"); !pc.Enabled {
		t.Fatalf("非法 JSON 应回落 enabled=true，得 %+v", pc)
	}
}

// TestAccountTaskFilter 校验调度器过滤器的逐账号 / 逐任务语义。
func TestAccountTaskFilter(t *testing.T) {
	r := &taskRunner{taskCfg: map[string]AccountTaskConfig{
		"u_off":  {Enabled: false},
		"u_pick": {Enabled: true, Disabled: []string{"travel", "cat"}},
	}}
	// 未配置账号 = 全参与。
	for _, task := range accountTaskKinds {
		if !r.accountTaskFilter("u_new", task) {
			t.Fatalf("未配置账号应参与任务 %s", task)
		}
	}
	// 总开关关闭 = 全不参与。
	for _, task := range accountTaskKinds {
		if r.accountTaskFilter("u_off", task) {
			t.Fatalf("关闭总开关的账号不应参与任务 %s", task)
		}
	}
	// 逐任务关闭。
	if r.accountTaskFilter("u_pick", "travel") || r.accountTaskFilter("u_pick", "cat") {
		t.Fatal("被关闭的任务（travel/cat）不应参与")
	}
	if !r.accountTaskFilter("u_pick", "checkin") || !r.accountTaskFilter("u_pick", "activity") {
		t.Fatal("未关闭的任务（checkin/activity）应参与")
	}
	// nil 过滤器（调度器未接过滤）= 全参与。
	var nilRunner *taskRunner
	if !nilRunner.accountTaskFilter("any", "checkin") {
		t.Fatal("nil taskRunner 应全参与（零值安全）")
	}
}

// TestAccountTaskStateView 校验对外视图的 kinds 开关态。
func TestAccountTaskStateView(t *testing.T) {
	st := accountTaskStateOf(AccountTaskConfig{Enabled: true, Disabled: []string{"keepalive"}})
	if !st.Enabled || len(st.Kinds) != len(accountTaskKinds) {
		t.Fatalf("视图应启用且含全部任务类型，得 %+v", st)
	}
	for _, k := range st.Kinds {
		want := k.Name != "keepalive"
		if k.On != want {
			t.Fatalf("任务 %s 的 On 应为 %v，得 %v", k.Name, want, k.On)
		}
	}
	// 总开关关闭 → 所有任务 On=false。
	off := accountTaskStateOf(AccountTaskConfig{Enabled: false})
	for _, k := range off.Kinds {
		if k.On {
			t.Fatalf("总开关关闭时任务 %s 不应为 On", k.Name)
		}
	}
}

// TestKeyAccountBinding 校验 Key 绑定账号集合的落库与读回（req 7）。
func TestKeyAccountBinding(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "keybind.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()
	u, _ := store.CreateUser("binder", "x")

	k, err := store.CreateAPIKey(u.ID, "自用", "hash1", "enc1", "sk-wb", KeyKindHost, "主力", "uid-a,uid-b")
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	got := splitUIDs(k.AccountUIDs)
	if len(got) != 2 || got[0] != "uid-a" || got[1] != "uid-b" {
		t.Fatalf("绑定账号应为 [uid-a uid-b]，得 %v", got)
	}
	// 更新绑定。
	if err := store.SetAPIKeyAccounts(u.ID, k.ID, "uid-c"); err != nil {
		t.Fatalf("SetAPIKeyAccounts: %v", err)
	}
	list, _ := store.ListAPIKeys(u.ID)
	if len(list) != 1 || splitUIDs(list[0].AccountUIDs)[0] != "uid-c" {
		t.Fatalf("更新后绑定应为 uid-c，得 %+v", list)
	}
	// 清空绑定。
	if err := store.SetAPIKeyAccounts(u.ID, k.ID, ""); err != nil {
		t.Fatalf("SetAPIKeyAccounts clear: %v", err)
	}
	list, _ = store.ListAPIKeys(u.ID)
	if len(splitUIDs(list[0].AccountUIDs)) != 0 {
		t.Fatalf("清空后绑定应为空，得 %q", list[0].AccountUIDs)
	}
}

// TestStoreAccountTaskPersist 校验落库 + 读回（纯存储层）。
func TestStoreAccountTaskPersist(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "task.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()
	if err := store.UpsertAccount(&Account{UID: "acc-1", Nickname: "A", Realm: "cn"}); err != nil {
		t.Fatalf("UpsertAccount: %v", err)
	}
	// 初始：无配置 → 全参与。
	acc, _ := store.GetAccount("acc-1")
	if !parseAccountTaskCfg(acc.TaskCfg).Enabled {
		t.Fatalf("新账号应缺省参与，得 %q", acc.TaskCfg)
	}
	// 关闭 travel。
	if err := store.SetAccountTaskConfig("acc-1", marshalAccountTaskCfg(AccountTaskConfig{Enabled: true, Disabled: []string{"travel"}})); err != nil {
		t.Fatalf("SetAccountTaskConfig: %v", err)
	}
	m, err := store.TaskConfigMap()
	if err != nil {
		t.Fatalf("TaskConfigMap: %v", err)
	}
	if _, ok := m["acc-1"]; !ok {
		t.Fatalf("TaskConfigMap 应含 acc-1，得 %+v", m)
	}
	// Upsert（模拟重新登录）不应抹掉任务配置。
	if err := store.UpsertAccount(&Account{UID: "acc-1", Nickname: "A2", Realm: "cn"}); err != nil {
		t.Fatalf("UpsertAccount 2: %v", err)
	}
	acc2, _ := store.GetAccount("acc-1")
	if c := parseAccountTaskCfg(acc2.TaskCfg); len(c.Disabled) != 1 || c.Disabled[0] != "travel" {
		t.Fatalf("UpsertAccount 不应抹掉 task_cfg，得 %q", acc2.TaskCfg)
	}
}
