// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

// SecretBox 静态加密箱（AES-256-GCM）。
//
// 用途：用户 API Key 明文需要「能回显给本人」，但又不能明文落库，
// 所以存密文（KeyEnc）+ 哈希（KeyHash）。哈希负责鉴权查找，密文负责回显。
// 主密钥由 -secret 指定，或从数据目录的 secret.key 读取 / 首次自动生成。
type SecretBox struct {
	aead cipher.AEAD
}

// LoadOrCreateSecret 读取主密钥；文件不存在则生成并落盘（0600）。
// path 传空串表示禁用加密（此时 KeyEnc 留空、不可回显，仅哈希可比对）。
func LoadOrCreateSecret(path string) (*SecretBox, error) {
	if path == "" {
		return nil, nil
	}
	key, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("生成主密钥失败: %w", err)
		}
		if err := os.WriteFile(path, key, 0o600); err != nil {
			return nil, fmt.Errorf("写入主密钥失败: %w", err)
		}
	}
	if len(key) < 32 {
		// 兼容用户手填短密钥：用 sha256 拉齐到 32 字节。
		sum := sha256.Sum256(key)
		key = sum[:]
	}
	block, err := aes.NewCipher(key[:32])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &SecretBox{aead: aead}, nil
}

// Encrypt 加密并返回 base64 串。
func (s *SecretBox) Encrypt(plain string) (string, error) {
	if s == nil {
		return "", nil
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := s.aead.Seal(nonce, nonce, []byte(plain), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt 解密 base64 串。
func (s *SecretBox) Decrypt(enc string) (string, error) {
	if s == nil {
		return "", errors.New("未启用加密（无主密钥）")
	}
	if enc == "" {
		return "", errors.New("密文为空")
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", err
	}
	ns := s.aead.NonceSize()
	if len(raw) < ns {
		return "", errors.New("密文长度非法")
	}
	plain, err := s.aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", errors.New("解密失败（主密钥可能已更换）")
	}
	return string(plain), nil
}

// randRead 熵源。抽成包级变量是为了**可注入**：测试能模拟 crypto/rand 故障，
// 从而把「失败即拒绝、绝不降级」这条契约钉住（不可注入的契约等于没有契约）。
var randRead = rand.Read

// secureBytes 取 n 字节密码学随机数；**失败即拒绝，刻意不降级**。
//
// 为什么必须这样（v0.24.1 修，审查报告 P3-3）：此前这几处（会话 token / API Key /
// 兑换码 / 邀请码 / 消息 ID）在 crypto/rand.Read 出错时**回落成时间戳派生**：
//
//	会话 token ← "%x" % UnixNano()      → 攻击者可枚举
//	API Key    ← 同上                    → 可被猜出后直接白嫖
//	兑换码     ← 同上                    → 可直接伪造面值
//
// 这类「降级」是最糟糕的一种失败模式：**不报错、不告警，静默产出弱凭据**，
// 而调用方无从分辨。crypto/rand 失败在这里不是「可预期的业务失败」
// （不像网络或数据库），它意味着操作系统的 CSPRNG 不可用 —— 进程已无法履行
// 它的安全契约，继续跑只会产出可预测的凭据。
//
// 所以选择 **panic**：net/http 会按连接 recover（记日志 + 断开该连接，进程不死），
// 单个请求得到 500，而**绝不会**有弱凭据流出去。启动期（生成管理密钥）panic
// 则等价于 fail-fast，正是想要的。
func secureBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := randRead(b); err != nil {
		panic("crypto/rand 不可用（" + err.Error() + "）：拒绝生成可预测的凭据")
	}
	return b
}

// HashToken 通用令牌哈希（sha256 十六进制），用于查找索引。
func HashToken(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// HashAPIKey API Key 的查找哈希。
func HashAPIKey(k string) string { return HashToken(k) }

// GenerateAPIKey 生成一把调用密钥，形如 sk-wb-<43字符>。
//
// 熵源失败即 panic（不降级成时间戳派生）—— 见 secureBytes 的说明。
func GenerateAPIKey() string {
	return "sk-wb-" + base64.RawURLEncoding.EncodeToString(secureBytes(24))
}

// keyPrefix 展示用前缀（前 12 位 + 末 4 位，中间打码）。
func keyPrefix(k string) string {
	if len(k) <= 18 {
		return k
	}
	return k[:12] + "..." + k[len(k)-4:]
}

// maskKey 列表展示用的掩码（不泄露完整密钥）。
func maskKey(prefix string) string {
	if prefix == "" {
		return "sk-wb-****"
	}
	return prefix
}

// maskSecret 把完整密钥渲染成「首尾保留 + 中间掩码」的展示形态。
//
// 需求（v0.17.8）：控制台 Key 卡片**默认脱敏**，完整值只在用户显式点
// 「查看 / 复制」时才取一次。本函数负责默认展示那一版。
//
// 规则（刻意做成固定形态，可预期也便于断言）：
//   - 完整值保留前缀（首个 '-' 之前，如 "sk-wb-"）与**最后 4 位**，中间一律 8 个 '•'；
//   - 掩码长度**不随真实密钥长度变化** —— 否则掩码位数本身就成了长度泄露；
//   - 值过短（不足以同时露出前缀与尾 4 位）时整体掩码，不露出任何真实字符。
func maskSecret(k string) string {
	const dots = "••••••••"
	if k == "" {
		return "sk-wb-" + dots
	}
	head := ""
	// 前缀取「前 8 个字符里的最后一个 '-'」及其之前的部分：形如 "sk-wb-"。
	// 用「最后一个」而不是「第一个」，是为了兼容多段前缀（sk-wb- / sk-proj- 之类），
	// 同时把扫描范围卡在前 8 字符内，避免密钥里偶然出现的 '-' 把大半截原文带出来。
	scan := k
	if len(scan) > 8 {
		scan = scan[:8]
	}
	if i := strings.LastIndexByte(scan, '-'); i >= 0 {
		head = k[:i+1]
	}
	tail := ""
	if len(k) >= len(head)+8 {
		tail = k[len(k)-4:]
	}
	if head == "" && tail == "" {
		return dots
	}
	return head + dots + tail
}

// generateRedeemCode 生成一张兑换码（无易混淆字符，形如 WB-XXXX-XXXX-XXXX）。
//
// 熵源失败即 panic（不降级成时间戳派生）—— 见 secureBytes 的说明。
// 兑换码是有面值的凭据，可预测等于可伪造。
func generateRedeemCode() string {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // 去掉 I O 0 1
	b := secureBytes(12)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return fmt.Sprintf("WB-%s-%s-%s", b[0:4], b[4:8], b[8:12])
}
