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
// ## 怎么轮换
//
//  1. 生成新对：`go run ./cmd/relsign keygen -out <新私钥文件>`
//  2. 把新公钥 hex 填进下面的常量，发版；
//  3. 至少要有一个版本同时接受新旧两把钥匙，之后才能停用旧钥匙 ——
//     ⚠️ 当前实现是**单公钥**：轮换窗口内老客户端会用旧公钥验新清单而失败。
//     真轮换时需要把这里改成 `[]ed25519.PublicKey` 并全部试一遍（已记入技术债）。
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

// ManifestPubKeyHex 发布密钥的**公钥**（hex，64 字符）。
//
// 空串 = 本次构建没有内置公钥：此时 Trust.Check 会明确报"内置公钥不可用"，
// 而不是"验签通过" —— 静默放行是这类功能最危险的失败方向。
//
// 可用 `-ldflags "-X workbuddy2api/internal/selfupdate.ManifestPubKeyHex=<hex>"`
// 在构建时覆盖（私有化部署自带发布链时有用）。
var ManifestPubKeyHex = "b899cc8d4d73916838931d10df2cfcdda420b703addac85ef139d102c2841cdb"

var (
	pubOnce sync.Once
	pubKey  ed25519.PublicKey
	pubErr  error
)

// DefaultPublicKey 解析内置公钥（只解析一次）。失败返回 nil，由调用方明确报错。
func DefaultPublicKey() ed25519.PublicKey {
	pubOnce.Do(func() {
		s := strings.TrimSpace(ManifestPubKeyHex)
		if s == "" {
			return
		}
		raw, err := hex.DecodeString(s)
		if err != nil {
			pubErr = err
			return
		}
		if len(raw) != ed25519.PublicKeySize {
			pubErr = ErrBadPublicKey
			return
		}
		pubKey = ed25519.PublicKey(raw)
	})
	return pubKey
}

// PublicKeyErr 内置公钥的解析错误（nil = 正常）。
func PublicKeyErr() error { return pubErr }
