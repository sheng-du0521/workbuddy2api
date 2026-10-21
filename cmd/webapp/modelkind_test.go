// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"strings"
	"testing"
)

// ======================================================================
// 生图模型必须从"对话面"里消失（v0.31.98）
//
// 现场：生产有一条通道只挂了 `gpt-image-2.5` 系列（生图模型）。上游对
// `/chat/completions` 明确 400，但只要它在对话清单/通道组里，就一定有人选中它，
// 然后拿到 400 —— 用户读到"网关坏了"，运维被引去核对 base_url / key。
//
// 四条落点，一条用例一事：
//  ① 名字识别（宁漏判不误判：误判会把能对话的模型删掉）；
//  ② 通道组的对话成员剔除生图模型；
//  ③ 只有生图模型的通道**不再作为对话分组出现**（成员 0 = 不列）；
//  ④ 直接拿生图模型名打对话 → 400 + 明确指引，**零出站、零扣费**。
// ======================================================================

func TestIsImageModel(t *testing.T) {
	yes := []string{
		"gpt-image-2.5", "gpt-image-2.5-flare", "gpt-image-1", "dall-e-3", "dalle-3",
		"imagen-4", "flux-pro-1.1", "stable-diffusion-3.5", "sdxl-turbo",
		"midjourney-v7", "kolors-v2", "seedream-4", "recraft-v3", "ideogram-3",
		"cogview-4", "wanx-v2", "nano-banana", "grok-image-1", "qwen-image-edit",
		"GPT-IMAGE-2.5", "  gpt-image-2.5  ",
	}
	no := []string{
		"", "glm-5.3", "deepseek-v4-flash", "gpt-6-astra", "gpt-6-sol", "gpt-5.6",
		"claude-sonnet-4.5", "gpt-4o", "o3-mini", "hy3-x", "wb-group-work",
		"perplexity-sonar", "gemini-2.5-pro", "qwen3-max",
	}
	for _, m := range yes {
		if !IsImageModel(m) {
			t.Errorf("%q 应被识别为生图模型", m)
		}
	}
	for _, m := range no {
		if IsImageModel(m) {
			t.Errorf("%q 不该被识别为生图模型（误判会让用户\"有模型却选不到\"）", m)
		}
	}
}

// TestChannelGroupExcludesImageModels 通道组里混着生图模型时，对话成员只剩能对话的。
func TestChannelGroupExcludesImageModels(t *testing.T) {
	f := newGroupFixture(t)
	f.addChannelModel(t, "relay-chat-model")
	f.addChannelModel(t, "gpt-image-2.5")
	f.addChannelModel(t, "gpt-image-2.5-flare")
	ch := "ch" + itoa(int(f.chID))

	var members int
	for _, g := range f.px.userGroups(f.key) {
		if g.ID == ch {
			members = g.Members
		}
	}
	if members != 1 {
		t.Fatalf("通道组对话成员数 = %d，期望 1（3 个映射里只有 1 个能对话）", members)
	}

	models, _, ok := f.px.expandGroupModels("wb-group-"+ch, f.key)
	if !ok {
		t.Fatalf("组名应能展开，得到 models=%v ok=%v", models, ok)
	}
	for _, m := range models {
		if IsImageModel(m) {
			t.Fatalf("组内候选出现生图模型 %q（轮到它必被上游 400）：%v", m, models)
		}
	}
	if len(models) != 1 || models[0] != "relay-chat-model" {
		t.Fatalf("组内候选 = %v，期望只剩 relay-chat-model", models)
	}
}

// TestImageOnlyChannelIsNotAChatGroup 只挂生图模型的通道不再作为对话分组出现。
//
// 不列（而不是列出来但成员为空）的理由：分组是"用户选了一个就能用的东西"，
// 一个成员都用不了的分组一旦出现在弹窗里，点进去必然失败。
func TestImageOnlyChannelIsNotAChatGroup(t *testing.T) {
	f := newGroupFixture(t)
	f.addChannelModel(t, "gpt-image-2.5")
	uid := seedChannelWithModels(t, f.store, "img-only", "http://127.0.0.1:1", "gpt-image-2.5-flare")
	imgCh := "ch" + itoa(int(uid))

	for _, g := range f.px.userGroups(f.key) {
		if g.Kind == "channel" {
			t.Fatalf("生图模型构成的通道不该作为对话分组出现：%+v", g)
		}
	}
	if _, _, ok := f.px.expandGroupModels("wb-group-"+imgCh, f.key); ok {
		t.Fatalf("只有生图模型的组不该展开成功（展开出来必然 400）")
	}
}

// TestChatRejectsImageModel 生图模型打对话端点 → 400，且**没有任何出站**。
//
// 断言三件事，缺一件这条纪律就会退化成"能跑但白花钱"：
//
//	① 状态 400 + X-Wb-Fail-Kind=image_model_on_chat（不是 502，别让 SDK 重试）；
//	② 文案指向"这是生图模型 / 该用哪个端点 / 本次没花钱"；
//	③ 上游一次都没被打，且没有产生任何流水（零扣费）。
func TestChatRejectsImageModel(t *testing.T) {
	store, _, _, srv := newRelayChatApp(t, false)
	defer store.Close()
	defer srv.Close()

	up, hits := fakeRelayPerModel(t, map[string]int{})
	defer up.Close()
	// 通道里确实挂着这个生图模型（不是"名字不存在"，而是"存在但不能对话"）。
	seedRelay(t, store, up.URL, "gpt-image-2.5", 1.0, true)
	seedPoolModels([]string{"pool-known-model"})

	resp, body := chatOnce(t, srv, `{"model":"gpt-image-2.5","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 400 {
		t.Fatalf("生图模型走对话应回 400，得 %d：%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Wb-Fail-Kind"); got != imageModelCode {
		t.Fatalf("X-Wb-Fail-Kind = %q，期望 %q", got, imageModelCode)
	}
	for _, want := range []string{"image generation model", "/chat/completions", "nothing was charged"} {
		if !strings.Contains(body, want) {
			t.Errorf("错误文案缺 %q：%s", want, body)
		}
	}
	if n := *hits; n != 0 {
		t.Fatalf("不该有任何上游出站，实际打了 %d 次", n)
	}
	var cnt int
	if err := store.db.QueryRow(`SELECT count(*) FROM usage_logs WHERE model='gpt-image-2.5'`).Scan(&cnt); err != nil {
		t.Fatalf("查流水：%v", err)
	}
	if cnt != 0 {
		t.Fatalf("被本地拒绝的请求不该写流水，实际 %d 条", cnt)
	}
}
