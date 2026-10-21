// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"strings"
	"testing"
)

// ======================================================================
// 模型名形态校验（v0.31.79）
//
// 线上真实出现过：客户端把 /v1/models 的整个清单用空格拼成一个字符串塞进 model
// （54 个名字、1KB 长）。它会一路打进上游、被 11102 拒、触发 rotateguard，
// 最后回客户端 404/503 —— 每一步都"看起来正常"。
//
// 判据的**误伤方向**很重要：宁可漏判（放过去，让上游 11102 说"不存在"），
// 也不能误判（把真实模型名拦成"这是清单"）—— 那会让正常用户突然不能调用。
// 所以阈值取得很宽：>128 字符 或 ≥2 个空格。
// ======================================================================

func TestModelNameLooksLikeList(t *testing.T) {
	normal := []string{
		"glm-5.3",
		"deepseek-v4-flash",
		"claude-sonnet-4-5-20250929",
		"wb-group-work",
		"wb-group-ch3",
		"gpt-5.5-sub2",
		"默认模型",                   // 中文名也不算清单（没有空格）
		"glm 5.3",                // 只有两个词：可能只是写错，不按"清单"处理
		" [global:]GLM-5.3 ",     // 复制粘贴带的首尾空白：Fields 只有一个词（真实踩过的误伤）
		"glm  5.3",               // 连续空白也只算两个词
		strings.Repeat("a", 128), // 正好 128：边界内
	}
	for _, m := range normal {
		if modelNameLooksLikeList(m) {
			t.Errorf("%q 不该被判成清单", m)
		}
	}

	list := []string{
		// 线上那条的原样复刻（截前几个）：两个以上空格
		"auto fast-model balanced-model deep-model",
		// 超长但只有换行分隔
		strings.Repeat("a", 129),
		// 54 个名字用空格 join（真实事件形态）
		strings.Repeat("glm-5.3 ", 54),
	}
	for _, m := range list {
		if !modelNameLooksLikeList(m) {
			t.Errorf("%q 应被判成清单", m)
		}
	}
}
