// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// relsign —— 更新清单（version.json）的独立签名工具（v0.31.60，审查 F07）。
//
// 三个子命令：
//
//	relsign keygen -out <私钥文件>        生成一对密钥；私钥写文件、公钥打到 stdout
//	relsign sign   -key <私钥文件> -in <version.json>   就地补/换签名三字段
//	relsign verify -in <version.json> [-pub <hex>]      用内置或指定公钥验签
//
// ## 为什么是独立工具而不是塞进 webapp
//
// 签名需要**私钥**，而 webapp 是跑在公网服务器上的长驻进程：私钥一旦进了它的启动环境，
// 就多了一条"从线上主机拿到发布权"的路。发布机（离线/本地）单独跑一个一次性命令，
// 私钥从不出现在服务器上 —— 这是这类签名最基本的部署纪律。
//
// ## 为什么不签文件字节
//
// 见 internal/selfupdate/pubsig.go 的 CanonicalBytes：签的是**语义字段**，
// 格式化变化（缩进、字段顺序）不影响签名，语义变化（改 url / sha256 / 版本）必然验不过。
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"workbuddy2api/internal/selfupdate"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "keygen":
		cmdKeygen(os.Args[2:])
	case "sign":
		cmdSign(os.Args[2:])
	case "verify":
		cmdVerify(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "未知子命令：%s\n\n", os.Args[1])
		usage()
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `relsign —— 更新清单签名工具

用法：
  relsign keygen -out <私钥文件>
  relsign sign   -key <私钥文件> -in <version.json>
  relsign verify -in <version.json> [-pub <公钥hex>]

注意：私钥文件**不要**提交进仓库，也不要放到线上服务器。
`)
	os.Exit(2)
}

// cmdKeygen 生成密钥对。私钥落盘（0600），公钥打到 stdout 方便贴进 pubkey.go。
func cmdKeygen(args []string) {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	out := fs.String("out", "", "私钥输出路径（必需）")
	force := fs.Bool("force", false, "覆盖已存在的私钥文件（会作废旧签名）")
	_ = fs.Parse(args)
	if strings.TrimSpace(*out) == "" {
		fatal("必须用 -out 指定私钥输出路径")
	}
	if _, err := os.Stat(*out); err == nil && !*force {
		fatal("私钥文件已存在：" + *out + "（确实要覆盖请加 -force；覆盖后旧清单签名全部失效）")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fatal("生成密钥失败：" + err.Error())
	}
	// 只写 32 字节 seed：ed25519.PrivateKey 的后半段就是公钥，写全也没问题，
	// 但 seed 更短、且能从它重建完整私钥（避免"两段不一致"这种自造坑）。
	if err := os.WriteFile(*out, []byte(hex.EncodeToString(priv.Seed())+"\n"), 0o600); err != nil {
		fatal("写私钥失败：" + err.Error())
	}
	fmt.Printf("私钥已写入：%s（权限 0600，勿入库、勿上服务器）\n", *out)
	fmt.Printf("公钥（贴进 internal/selfupdate/pubkey.go 的 ManifestPubKeyHex）：\n%s\n", hex.EncodeToString(pub))
	fmt.Printf("key_id：%s\n", selfupdate.KeyID(pub))
}

// loadKey 读私钥（hex seed 或 hex 全量私钥都接受）。
func loadKey(path string) ed25519.PrivateKey {
	raw, err := os.ReadFile(path)
	if err != nil {
		fatal("读私钥失败：" + err.Error())
	}
	h := strings.TrimSpace(string(raw))
	b, err := hex.DecodeString(h)
	if err != nil {
		fatal("私钥不是合法 hex：" + err.Error())
	}
	switch len(b) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(b)
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(b)
	default:
		fatal(fmt.Sprintf("私钥长度非法：%d", len(b)))
	}
	return nil
}

// topLevelOrder 输出时的**首选字段顺序**：与原 version.json 的人读顺序一致。
// 未列出的字段排在后面（按名字排序）—— 清单将来加字段不会被这个工具吃掉。
var topLevelOrder = []string{
	"version", "min_supported", "notes", "released_at", "url", "assets",
	"sig_alg", "key_id", "signature", "_assets_hint",
}

// cmdSign 就地签名。**保留原有字段与顺序**（含 _assets_hint 这类帮助字段）——
// 用结构体整份重写会静默丢掉不认识的字，那正是"工具改了人的文件"最招人烦的地方。
func cmdSign(args []string) {
	fs := flag.NewFlagSet("sign", flag.ExitOnError)
	keyPath := fs.String("key", "", "私钥文件路径（必需）")
	in := fs.String("in", "version.json", "要签名的清单路径（就地改写）")
	_ = fs.Parse(args)
	if strings.TrimSpace(*keyPath) == "" {
		fatal("必须用 -key 指定私钥文件")
	}
	raw, err := os.ReadFile(*in)
	if err != nil {
		fatal("读清单失败：" + err.Error())
	}
	var m selfupdate.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		fatal("清单不是合法 JSON：" + err.Error())
	}
	if strings.TrimSpace(m.Version) == "" {
		fatal("清单缺少 version 字段")
	}
	if err := selfupdate.Sign(loadKey(*keyPath), &m); err != nil {
		fatal("签名失败：" + err.Error())
	}
	// 用 map 保留不认识的字段，再按固定顺序输出。
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		fatal("清单解析失败：" + err.Error())
	}
	set := func(k string, v any) {
		b, err := json.Marshal(v)
		if err != nil {
			fatal("序列化 " + k + " 失败：" + err.Error())
		}
		fields[k] = b
	}
	set("sig_alg", m.SigAlg)
	set("key_id", m.KeyID)
	set("signature", m.Signature)

	out := marshalOrdered(fields)
	if err := os.WriteFile(*in, out, 0o644); err != nil {
		fatal("写回清单失败：" + err.Error())
	}
	fmt.Printf("已签名：%s（version=%s key_id=%s）\n", *in, m.Version, m.KeyID)
}

// marshalOrdered 按 topLevelOrder 输出，其余字段按名字排在后面。
// 缩进 2 空格 + 结尾换行：与仓库里既有 version.json 的形态一致，diff 才不会整份翻红。
func marshalOrdered(fields map[string]json.RawMessage) []byte {
	var b strings.Builder
	b.WriteString("{\n")
	written := map[string]bool{}
	var emit func(k string, last bool)
	emit = func(k string, last bool) {
		v := fields[k]
		var buf bytes.Buffer
		if err := json.Indent(&buf, v, "  ", "  "); err != nil {
			fatal("格式化字段 " + k + " 失败：" + err.Error())
		}
		b.WriteString("  " + strconvQuote(k) + ": " + buf.String())
		if !last {
			b.WriteString(",")
		}
		b.WriteString("\n")
		written[k] = true
	}
	// 先挑出真正存在的首选字段。
	var order []string
	for _, k := range topLevelOrder {
		if _, ok := fields[k]; ok {
			order = append(order, k)
		}
	}
	var rest []string
	for k := range fields {
		if !written[k] && !contains(order, k) {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	order = append(order, rest...)
	for i, k := range order {
		emit(k, i == len(order)-1)
	}
	b.WriteString("}\n")
	return []byte(b.String())
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// strconvQuote 用 encoding/json 的字符串规则给键名加引号（键名里可能有中文/短横线）。
func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// cmdVerify 验签。默认用二进制内置公钥；-pub 可指定（私有化部署自签时用）。
func cmdVerify(args []string) {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	in := fs.String("in", "version.json", "要校验的清单路径")
	pubHex := fs.String("pub", "", "公钥 hex（省略则用内置公钥）")
	_ = fs.Parse(args)

	raw, err := os.ReadFile(*in)
	if err != nil {
		fatal("读清单失败：" + err.Error())
	}
	var m selfupdate.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		fatal("清单不是合法 JSON：" + err.Error())
	}
	var pub ed25519.PublicKey
	if strings.TrimSpace(*pubHex) == "" {
		pub = selfupdate.DefaultPublicKey()
		if len(pub) == 0 {
			fatal("本二进制没有内置公钥（ManifestPubKeyHex 为空），请用 -pub 指定")
		}
	} else {
		b, err := hex.DecodeString(strings.TrimSpace(*pubHex))
		if err != nil || len(b) != ed25519.PublicKeySize {
			fatal("公钥 hex 非法（需要 64 个 hex 字符）")
		}
		pub = ed25519.PublicKey(b)
	}
	if err := selfupdate.VerifyManifest(pub, &m); err != nil {
		fmt.Fprintf(os.Stderr, "验签失败：%v\n", err)
		os.Exit(1)
	}
	fmt.Printf("验签通过：version=%s key_id=%s\n", m.Version, m.KeyID)
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}
