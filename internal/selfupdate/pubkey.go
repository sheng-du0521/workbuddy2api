// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 更新清单验签用的**内置公钥**（v0.31.60，审查 F07）。
//
// ## 为什么是"内置"而不是"从服务器取"
//
// 验签的全部意义是"不信任发布通道"。公钥如果也从同一条通道取，攻击者把公钥和清单
// 一起换掉，签名就退化成一句装饰。内置（编译进二进制）之后，换公钥必须换二进制 ——
// 而换二进制这件事本身就是最强的攻击面，签名已无能为力，边界清楚。
//
// ## 怎么轮换（v0.31.61 起支持多公钥）
//
//  1. 生成新对：`go run ./cmd/relsign keygen -out <新私钥文件>`
//  2. 把**新公钥追加**到下面的列表里（**旧公钥先留着**），发版；
//  3. 用新私钥签清单并发布。老客户端（只认识旧公钥）此时会拒绝新清单 ——
//     这是**签名机制的正常代价**：它保护的是"没人能偷偷换清单"，代价就是换钥匙要
//     靠一次"无法被验签的窗口"过渡。所以实践上：
//     · 优先在"本来就要发新版"的时候换，让用户先升级客户端；
//     · 或保留旧私钥继续签名一段时间，直到足够多的客户端内置了新公钥；
//  4. 确认无人依赖旧钥匙后，再把旧公钥从列表里删掉。
//
// 列表里**任何一把**验签通过即可（验的是同一份清单，不是"多把钥匙签多份"）。
//
// ## 私钥在哪
//
// **不在仓库里，也不要放进仓库**。当前放在发布机仓库外的
// `D:/_ro/update_signing.key`（仅本机，权限 600 等价），泄露即等于把发布权交出去。
// 签名用 `cmd/relsign`，不参与 CI 之外任何流程。
package selfupdate

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
)

// ErrBadPublicKey 内置公钥写错（不是合法 hex 或长度不对）。
var ErrBadPublicKey = errors.New("内置公钥长度非法")

// ManifestPubKeyHex 发布密钥的**公钥列表**（hex，每个 64 字符）。
//
// 空列表 = 本次构建没有内置公钥：此时 Trust.Check 会明确报"内置公钥不可用"，
// 而不是"验签通过" —— 静默放行是这类功能最危险的失败方向。
//
// 轮换时**追加**新公钥、保留旧公钥，见上方「怎么轮换」。
var ManifestPubKeyHex = []string{
	"b899cc8d4d73916838931d10df2cfcdda420b703addac85ef139d102c2841cdb", // 2026-10-20 首版发布密钥
}

var (
	pubOnce sync.Once
	pubKeys []ed25519.PublicKey
	pubErr  error
)

// DefaultPublicKeys 解析内置公钥列表（只解析一次）。空 = 本次构建没内置公钥。
//
// 单条非法不致命：记下第一个错误、跳过这一条，其余照常可用 —— 否则"轮换时多写了一个
// 错字符"会让整个更新检查失效，而那种错误在发布前根本看不出来。
func DefaultPublicKeys() []ed25519.PublicKey {
	pubOnce.Do(func() {
		for _, s := range ManifestPubKeyHex {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			raw, err := hex.DecodeString(s)
			if err != nil {
				if pubErr == nil {
					pubErr = err
				}
				continue
			}
			if len(raw) != ed25519.PublicKeySize {
				if pubErr == nil {
					pubErr = ErrBadPublicKey
				}
				continue
			}
			pubKeys = append(pubKeys, ed25519.PublicKey(raw))
		}
	})
	return pubKeys
}

// DefaultPublicKey 第一把内置公钥（兼容单钥调用方，如 cmd/relsign 的默认校验）。
func DefaultPublicKey() ed25519.PublicKey {
	if ks := DefaultPublicKeys(); len(ks) > 0 {
		return ks[0]
	}
	return nil
}

// PublicKeyErr 内置公钥的解析错误（nil = 正常）。
func PublicKeyErr() error { return pubErr }
