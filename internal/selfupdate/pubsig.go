// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 更新清单（version.json）的**独立签名**（v0.31.60，审查报告 F07）。
//
// ## 为什么需要它（审查怎么说的）
//
// 清单决定了两件事：要不要提示升级、以及**去下载哪个文件、它的 sha256 是多少**。
// 清单本身走的是 raw.githubusercontent / jsdelivr / ghproxy / 自建 CDN 多条路径
// （见 fetch.go 的多地址回退），而"sha256 从同一条通道取来"意味着：
// **能改通道的人就能同时改 url 与 sha256**，校验因此完全失效 —— 那只是"传输完整性"，
// 不是"来源真实性"。签名补的正是后者：私钥不在发布通道上，通道被投毒也伪造不出签名。
//
// ## 边界（刻意不做的事）
//
//   - 不引入第三方依赖：只用标准库 crypto/ed25519（无 CGO、无供应链）。
//   - 不自动安装：签名通过也不代表"该装"，安装始终是人点确认（见 cmd/webapp）。
//   - 不给清单加密：清单内容本来就是公开的，加密只会让部署更难排查。
//   - **私钥绝不入库**：本包只含公钥（见 pubkey.go），签名由 cmd/relsign 在发布机上做。
package selfupdate

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// SigAlg 当前唯一的签名算法标识。
//
// 写成显式字段而不是"有签名就是 ed25519"：算法升级时旧客户端必须能**明确拒绝**
// 自己不认识的新算法，而不是把一段未知字符串当成"验签失败"而含糊其辞。
const SigAlg = "ed25519"

// sigDomain 签名前缀（域分隔）。
//
// 作用：把"清单签名"与其他将来可能出现的签名（比如 agent 清单、发行说明）
// 在密码学上隔开 —— 同一把私钥签的两种东西不能互相顶替。
// 改这个前缀等于让所有旧客户端的验签失败，属于不兼容变更，必须随版本号走。
const sigDomain = "wb2api-version-json-v1\n"

// signedPayload 被签名的**规范化载荷**。
//
// 为什么不直接签文件字节：version.json 是人手写的，缩进、字段顺序、空格随时会变
// （换编辑器就变），签字节会让"只多了一个空格"变成"签名失效"。
// 这里把**语义字段**按固定顺序序列化后再签：格式化随便改，语义一改就验不过。
//
// 反过来，`_assets_hint` 这类给写清单的人看的帮助字段**不参与签名** ——
// 它不影响任何下载决策，改它不该让签名失效。
type signedPayload struct {
	V            int     `json:"v"`
	Version      string  `json:"version"`
	MinSupported string  `json:"min_supported"`
	Notes        string  `json:"notes"`
	ReleasedAt   int64   `json:"released_at"`
	URL          string  `json:"url"`
	Assets       []Asset `json:"assets"`
}

// CanonicalBytes 规范化载荷的字节形态（签名与验签共用**唯一**一份构造）。
//
// 必须是唯一实现：签名端和验签端各写一遍"怎么拼这些字段"，必然在某个字段上漂移，
// 而漂移的表现是"所有客户端都验签失败"或更糟——"某些字段根本没被签进去"。
func CanonicalBytes(m *Manifest) ([]byte, error) {
	if m == nil {
		return nil, errors.New("清单为空")
	}
	assets := m.Assets
	if assets == nil {
		assets = []Asset{}
	}
	p := signedPayload{
		V:            1,
		Version:      strings.TrimSpace(m.Version),
		MinSupported: strings.TrimSpace(m.MinSupported),
		Notes:        m.Notes,
		ReleasedAt:   m.ReleasedAt,
		URL:          strings.TrimSpace(m.URL),
		Assets:       assets,
	}
	body, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	return append([]byte(sigDomain), body...), nil
}

// KeyID 公钥的短标识（前 8 个 hex 字符）。
//
// 为什么要它：换密钥（轮换 / 泄露后作废）时，客户端能直接说出"清单用的是哪把钥匙"，
// 而不是只能说"验签失败" —— 后者在运维眼里与"清单被篡改"无法区分。
func KeyID(pub ed25519.PublicKey) string {
	if len(pub) == 0 {
		return ""
	}
	h := hex.EncodeToString(pub)
	if len(h) > 8 {
		return h[:8]
	}
	return h
}

// Sign 用私钥给清单签名，并就地填入 sig_alg / key_id / signature 三个字段。
func Sign(priv ed25519.PrivateKey, m *Manifest) error {
	if len(priv) != ed25519.PrivateKeySize {
		return fmt.Errorf("私钥长度不对：%d", len(priv))
	}
	payload, err := CanonicalBytes(m)
	if err != nil {
		return err
	}
	sig := ed25519.Sign(priv, payload)
	pub := priv.Public().(ed25519.PublicKey)
	m.SigAlg = SigAlg
	m.KeyID = KeyID(pub)
	m.Signature = base64.StdEncoding.EncodeToString(sig)
	return nil
}

// VerifyManifest 用公钥校验清单签名。
//
// 失败情形都返回**可读的原因**（运维要能区分"没签名""签名错""换了钥匙"）：
//   - 清单没有 signature → ErrUnsigned；
//   - sig_alg 不是 ed25519 → 明确说"算法不认识"（可能是未来版本签的）；
//   - key_id 与公钥不符 → 说"签名来自另一把钥匙"；
//   - 密码学校验失败 → ErrBadSignature（内容被改过）。
var (
	ErrUnsigned     = errors.New("清单未签名")
	ErrBadSignature = errors.New("清单签名无效（内容与签名不符）")
)

func VerifyManifest(pub ed25519.PublicKey, m *Manifest) error {
	if m == nil {
		return errors.New("清单为空")
	}
	if len(pub) != ed25519.PublicKeySize {
		return errors.New("内置公钥不可用（构建时未注入或长度非法）")
	}
	if strings.TrimSpace(m.Signature) == "" {
		return ErrUnsigned
	}
	if alg := strings.TrimSpace(m.SigAlg); alg != "" && !strings.EqualFold(alg, SigAlg) {
		return fmt.Errorf("清单签名算法不支持：%q（本机只认 %s）", alg, SigAlg)
	}
	if kid := strings.TrimSpace(m.KeyID); kid != "" && !strings.EqualFold(kid, KeyID(pub)) {
		return fmt.Errorf("清单由另一把密钥签名（清单 key_id=%s，本机=%s）", kid, KeyID(pub))
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(m.Signature))
	if err != nil {
		return fmt.Errorf("签名 base64 解码失败：%w", err)
	}
	payload, err := CanonicalBytes(m)
	if err != nil {
		return err
	}
	if !ed25519.Verify(pub, payload, sig) {
		return ErrBadSignature
	}
	return nil
}

// Trust 验签策略。
//
// Required=false 是给"用自己的清单、自己发版"的部署留的口子（kv 可关）。
// 默认必须是 true：默认不验签等于没有签名 —— 这正是审查 F07 指出的问题。
type Trust struct {
	Required bool
	Pub      ed25519.PublicKey
}

// Check 按策略校验。返回 nil 表示"可以信任这份清单"。
func (t Trust) Check(m *Manifest) error {
	if m == nil {
		return errors.New("清单为空")
	}
	if !t.Required {
		return nil
	}
	return VerifyManifest(t.Pub, m)
}
