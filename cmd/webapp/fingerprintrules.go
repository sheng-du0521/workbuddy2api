// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 出站指纹规则的**运营侧**：实现已下沉 `internal/api/fingerprint`
// （v0.31.12，P2-7 第四刀第一刀 —— 它是全仓依赖面最窄的一簇 admin 接口）。
//
// 本文件只剩两件**本包才有资格做**的事：
//
//  1. `kvFingerprintRules` 的常量别名 —— 既有测试按这个名字写 kv；
//  2. `loadFingerprintRules` 的转发 —— "启动时加载"是**应用生命周期**，
//     不是领域逻辑（谁来调、什么时候调，只有 main 知道）；
//  3. 顺带把路由此刻需要的 `*fingerprint.API` 组装出来（见 routes()）。
//
// ⚠️ 与 `internal/httpx` 的同一纪律：桥接层**只转发不判断**。任何"该不该保存、
// 校验顺序、失败回什么码"的逻辑都必须在新包里 —— 否则同一件事又变成两处实现。
package main

import "workbuddy2api/internal/api/fingerprint"

// kvFingerprintRules 持久化键（值本体在新包；测试仍按本名读写 kv）。
const kvFingerprintRules = fingerprint.KVKey

// loadFingerprintRules 启动时加载并安装自定义规则。返回条数与错误。
//
// nil 守卫留在这里（而不是新包）：它判的是 `app` 有没有装配好，属于本包的知识。
// 新包的 `API.Load` 另有一道 `store == nil` 守卫覆盖"接口值为 nil"的情形。
func (a *app) loadFingerprintRules() (int, error) {
	if a == nil || a.store == nil {
		return 0, nil
	}
	return fingerprint.New(a.store, fingerprintSampleTexts).Load()
}
