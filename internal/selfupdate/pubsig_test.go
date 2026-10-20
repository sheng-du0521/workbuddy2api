// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package selfupdate

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// ======================================================================
// 清单签名的回归网（v0.31.60，审查 F07）
//
// 签名这种功能最容易"看起来在工作"：写进去、读出来、都是非空字符串，
// 而实际上签的和验的不是同一段字节，或者**关键字段根本没被签进去**。
// 下面每条都从"改一个字"出发断言它必须失败。
// ======================================================================

func testKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return pub, priv
}

func sampleManifest() *Manifest {
	return &Manifest{
		Version:      "0.31.60",
		MinSupported: "0.24.0",
		Notes:        "测试清单",
		ReleasedAt:   1792471214,
		URL:          "https://example.com/releases",
		Assets: []Asset{{
			OS: "linux", Arch: "amd64",
			URL:    "https://example.com/webapp",
			SHA256: strings.Repeat("a", 64),
			Size:   15639552,
		}},
	}
}

func mustSign(t *testing.T, priv ed25519.PrivateKey, m *Manifest) {
	t.Helper()
	if err := Sign(priv, m); err != nil {
		t.Fatalf("Sign: %v", err)
	}
}

// S1：签名 → 验签通过；且 key_id 是公钥的可识别短标识。
func TestSignVerifyRoundTrip(t *testing.T) {
	pub, priv := testKey(t)
	m := sampleManifest()
	mustSign(t, priv, m)

	if m.SigAlg != SigAlg || m.Signature == "" {
		t.Fatalf("签名字段没填：%+v", m)
	}
	if m.KeyID != KeyID(pub) || len(m.KeyID) != 8 {
		t.Fatalf("key_id 不对：%q，期望 %q", m.KeyID, KeyID(pub))
	}
	if err := VerifyManifest(pub, m); err != nil {
		t.Fatalf("自签自验应通过：%v", err)
	}
}

// S2：**每个会改变下载行为的字段**被改都必须验签失败。
//
// 这是整个功能的意义所在：清单同时决定"下哪个文件"和"它的 sha256 是多少"，
// 能改通道的人就能一起改 —— 只有签名能把这条路堵死。
func TestVerifyRejectsTamperedFields(t *testing.T) {
	pub, priv := testKey(t)
	cases := []struct {
		name string
		mut  func(*Manifest)
	}{
		{"版本号", func(m *Manifest) { m.Version = "9.9.9" }},
		{"最低支持版本", func(m *Manifest) { m.MinSupported = "0.0.1" }},
		{"下载地址", func(m *Manifest) { m.Assets[0].URL = "https://evil.example/x.exe" }},
		{"sha256", func(m *Manifest) { m.Assets[0].SHA256 = strings.Repeat("b", 64) }},
		{"体积", func(m *Manifest) { m.Assets[0].Size = 1 }},
		{"平台", func(m *Manifest) { m.Assets[0].OS = "windows" }},
		{"增补产物", func(m *Manifest) {
			m.Assets = append(m.Assets, Asset{OS: "windows", Arch: "amd64", URL: "https://evil.example/w.exe"})
		}},
		{"说明文字", func(m *Manifest) { m.Notes = "点这里下载：https://evil.example" }},
		{"发布时间", func(m *Manifest) { m.ReleasedAt++ }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := sampleManifest()
			mustSign(t, priv, m)
			c.mut(m)
			if err := VerifyManifest(pub, m); !errors.Is(err, ErrBadSignature) {
				t.Fatalf("改%s后应验签失败，得 %v", c.name, err)
			}
		})
	}
}

// S3：未签名 / 算法不认识 / 换了一把钥匙 —— 三种失败必须**可区分**。
//
// 不可区分的话，运维看到"验签失败"只能猜：是清单被改了？是我升级了客户端？
// 还是我换了密钥？三种情况的处置完全不同。
func TestVerifyDistinguishesFailureModes(t *testing.T) {
	pub, priv := testKey(t)
	otherPub, _ := testKey(t)

	m := sampleManifest()
	if err := VerifyManifest(pub, m); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("未签名应报 ErrUnsigned，得 %v", err)
	}

	mustSign(t, priv, m)
	if err := VerifyManifest(otherPub, m); err == nil || !strings.Contains(err.Error(), "另一把密钥") {
		t.Fatalf("换钥匙应提示密钥不匹配，得 %v", err)
	}

	m2 := sampleManifest()
	mustSign(t, priv, m2)
	m2.SigAlg = "rsa-pss" // 未来版本可能换算法：必须明确拒绝，不能当成"验签失败"
	if err := VerifyManifest(pub, m2); err == nil || !strings.Contains(err.Error(), "不支持") {
		t.Fatalf("未知算法应明确拒绝，得 %v", err)
	}

	// 公钥缺失是构建问题，不能与"篡改"混为一谈。
	if err := VerifyManifest(nil, m); err == nil {
		t.Fatal("没有公钥时必须失败（静默放行是最危险的失败方向）")
	}
}

// S4：再序列化一次（换缩进/字段顺序）不应影响验签。
//
// 这正是"签语义而不是签字节"的目的：清单是人手写的，编辑器换一下就重排缩进，
// 签字节会把这种无害改动判成篡改，最后大家会去关掉验签。
func TestVerifySurvivesReformatting(t *testing.T) {
	pub, priv := testKey(t)
	m := sampleManifest()
	mustSign(t, priv, m)

	blob, err := json.MarshalIndent(m, "", "\t") // 换成 tab 缩进
	if err != nil {
		t.Fatalf("MarshalIndent: %v", err)
	}
	var again Manifest
	if err := json.Unmarshal(blob, &again); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if err := VerifyManifest(pub, &again); err != nil {
		t.Fatalf("只是重排格式不该验签失败：%v", err)
	}
}

// S5：Trust 策略 —— 关闭验签时放行（自建发布链），开启时缺签名必须拒绝。
func TestTrustPolicy(t *testing.T) {
	pub, _ := testKey(t)
	m := sampleManifest() // 未签名

	if err := (Trust{Required: false}).Check(m); err != nil {
		t.Fatalf("关闭验签时应放行，得 %v", err)
	}
	if err := (Trust{Required: true, Pub: pub}).Check(m); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("开启验签时缺签名必须拒绝，得 %v", err)
	}
}
