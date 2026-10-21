package main

import "strings"

// 生图模型的识别（v0.31.98）。
//
// # 为什么要专门识别
//
// 2026-10-21 的真实现场：生产上有一条通道只挂了 `gpt-image-2.5` 系列。它们是
// **生图模型**，上游对 `/chat/completions` 明确 400（`This model is not supported on
// the Chat Completions endpoint`）。但只要它们出现在对话模型清单里，就会有人选中它、
// 然后拿到 400 —— 用户读到的是"网关坏了"，运维被引去核对 base_url / key，全是错方向。
//
// 所以生图模型必须**在对话面上被排除**，而不是靠运气不被选中：
//   - 通道组的对话成员剔除生图模型（`userGroups` / `expandGroupModels`）；
//   - 只挂生图模型的通道不再作为"对话分组"出现（成员数为 0 → 不列）；
//   - 直接拿生图模型名打对话接口 → 400 + 明确指引，零出站、零扣费。
//
// ⚠️ 这里只做**名字层面的**识别（保守、可解释），不去猜上游能力：
// 名字里带这些标记的当生图模型，宁可漏判（漏判的后果是沿用旧行为，一次 400 而已），
// 不要误判（误判会把能对话的模型从清单里删掉，用户"明明有模型却选不到"）。
var imageModelMarkers = []string{
	"image",  // gpt-image-2.5、qwen-image、gemini-*-image
	"dall-e", // dall-e-3
	"dalle",
	"imagen",           // google imagen
	"flux",             // black forest labs
	"stable-diffusion", // sd 全系
	"sdxl",
	"midjourney",
	"kolors",
	"seedream", // 即梦
	"seededit",
	"recraft",
	"ideogram",
	"cogview", // 智谱
	"wanx",    // 通义万相
	"nano-banana",
	"grok-image",
}

// IsImageModel 这个名字是不是生图模型（按名字判断，见 imageModelMarkers 的取舍说明）。
func IsImageModel(id string) bool {
	s := strings.ToLower(strings.TrimSpace(id))
	if s == "" {
		return false
	}
	for _, m := range imageModelMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// chatCapableModels 过滤出**能用于对话**的模型（去掉生图模型），保持原顺序。
//
// 所有"对话面"的成员清单都必须过这一道：通道组、模型清单、组名展开。
// 三处各写一遍 filter 就是上一版 `visibleGroups` 的翻版（两套口径必生 bug），
// 所以只留这一个函数。
func chatCapableModels(in []string) []string {
	if len(in) == 0 {
		return in
	}
	out := make([]string, 0, len(in))
	for _, m := range in {
		if IsImageModel(m) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// imageModelError 生图模型被拿去打对话接口时的统一说法（零出站、零扣费）。
//
// 文案要同时回答三个问题：这是模型能力边界（不是配置错）、该改什么、这次有没有花钱。
const (
	imageModelCode = "image_model_on_chat_endpoint"
	imageModelMsg  = "this model is an image generation model and cannot serve /chat/completions"
	imageModelHint = "the upstream rejects it on the chat endpoint (not a config problem); " +
		"pick a chat model for /chat/completions (GET /v1/models lists chat-capable models only), " +
		"or call the image endpoint for image generation; no upstream account was used, nothing was charged"
)
