// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// 出口隧道的**信任根**：每用户一把受限 SSH 密钥 + 隧道端口分区（v0.31.25）。
//
// # 为什么必须有这个文件
//
// v0.31.24 把「拉起出口」的权限下沉给了普通用户，但那条路当时**实际上走不通**：
// agent 用 `ssh -R` 建反向隧道，需要一把能登录服务器的 SSH 私钥，而部署时用的
// 是服务器 root 的那把（ai.pem）。把 root 私钥分发给每个用户 = 每人一个 root
// —— 这显然不行；不分发，用户就拉不起来。
//
// # 做法
//
//  1. **每用户一把专属 ed25519 密钥**（服务端用 ssh-keygen 生成，存在 data 目录
//     与 kv 里，只在"该用户自己取回"时下发）；
//
//  2. 写进 `authorized_keys` 时**带限制**：
//
//     restrict,port-forwarding,permitlisten="127.0.0.1:12010",...,"127.0.0.1:12019"
//
//     `restrict` 关掉 pty / agent 转发 / X11 / 用户 rc，只放回端口转发；
//     `command="/bin/true"` **顶替掉任何 exec/shell 请求**；
//     `permitlisten` 把这个用户**锁在自己的端口段内**。
//
//     ⚠️ 实测教训（2026-10-17，在隔离的临时 sshd 实例上验的）：**`restrict` 并不禁止 exec**
//     —— 它只禁 pty / agent 转发 / X11 / user-rc /（默认的）端口转发，`ssh host 'cmd'`
//     照样能跑。所以光有 `restrict` 等于发出了一把能 root shell 的钥匙。
//     必须再加 `command=`：exec 请求被顶替成 `/bin/true`（连续两次实测输出为空），
//     而 `ssh -N -R`（不请求 shell）的转发**不受影响** —— 同一实例上两边都验过。
//
//  3. **端口分区**（见 egressPortSegOf）：平台 11080-11089；用户
//     `12000 + (uid%1000)*10` .. `+9`。
//
// # 这两条合起来挡住了什么（都很现实）
//
//   - **拿一把 root 的钥匙**：`restrict` + `command="/bin/true"` 把 exec 请求顶替掉，
//     登录后**只能**建白名单内的转发（实测：`ssh -i key host 'id'` 无输出且不报错）；
//   - **抢占别人的隧道端口**：`ssh -R 127.0.0.1:11080`（平台的端口）会被 sshd 直接
//     拒绝（不在 permitlisten 列表里）。这一步很关键 —— 若允许抢占，A 就能在自己
//     机器上"接住"本应发给 B（或平台）的流量，而那些请求里带着 access_token。
//   - **在自己机器上开任意入站端口**：`port-forwarding` 只放行转发，且 `-R` 的
//     监听地址被限制为 127.0.0.1（服务器侧 GatewayPorts 默认 no）。
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	// egressAuthKeysCommentFmt authorized_keys 里本服务写的条目注释（幂等替换用）。
	//
	// ⚠️ 只认自己的前缀：改写 authorized_keys 是高危操作（写坏了自己也登不进去），
	// 所以纪律是"**只删自己写过的行、只追加**"，别人的条目一个字节都不碰。
	egressAuthKeysCommentFmt = "wb2api-u%d"

	// egressSSHPrivKeyFmt / egressSSHPubKeyFmt 私钥与公钥在 kv 里的键。
	egressSSHPrivKeyFmt = "egress_ssh_priv_u%d"
	egressSSHPubKeyFmt  = "egress_ssh_pub_u%d"

	// 端口分区：平台（owner=0）10 个，用户每段 10 个。
	egressPlatformPortBase = 11080
	egressPlatformPortSpan = 10
	egressUserPortBase     = 12000
	egressUserPortSpan     = 10
	// egressUserPortMod 用户段取模（决定最多多少个不同段：1000 段 × 10 端口）。
	//
	// 用取模而不是 uid*10 直接算：uid 没有上界，直接乘会立刻超过 65535。
	// 取模带来的"uid 差 1000 的倍数会撞段"在实践中够用（用户数以百计），
	// 且真撞了也只是两台机器共用一个 10 端口段 —— 分配时会跳过已占用的。
	egressUserPortMod = 1000
)

// egressPortSegOf 某个 owner 的隧道端口段（起点 + 长度）。
func egressPortSegOf(owner int64) (base, span int) {
	if owner <= 0 {
		return egressPlatformPortBase, egressPlatformPortSpan
	}
	return egressUserPortBase + int(owner%egressUserPortMod)*egressUserPortSpan, egressUserPortSpan
}

// egressPortSegText 段的展示文本（错误信息里用）。
func egressPortSegText(owner int64) string {
	base, span := egressPortSegOf(owner)
	return fmt.Sprintf("%d-%d", base, base+span-1)
}

// egressSSHKeyEnsure 取（必要时生成并登记）某个用户的专属受限隧道密钥。
//
// 返回值：私钥 PEM（给该用户自己的 agent 用）。
//
// 幂等：已有密钥直接返回，不重新生成 —— 重新生成会让**已经在跑的 agent** 立刻
// 失去隧道（旧密钥被撤），而用户并没有要求换密钥。
func (a *app) egressSSHKeyEnsure(uid int64) (string, error) {
	if uid <= 0 {
		return "", fmt.Errorf("平台视角不使用用户密钥（管理员本机跑 agent 沿用部署时的那个密钥）")
	}
	privKey := fmt.Sprintf(egressSSHPrivKeyFmt, uid)
	if v, err := a.store.GetKV(privKey); err == nil && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v) + "\n", nil
	}
	pem, err := a.egressSSHKeyGenerate(uid)
	if err != nil {
		return "", err
	}
	if err := a.store.SetKV(privKey, pem); err != nil {
		return "", err
	}
	return pem + "\n", nil
}

// egressSSHKeyGenerate 生成密钥对并登记公钥到 authorized_keys。
//
// 用 `ssh-keygen` 而不是自己编码 OpenSSH 私钥格式：后者要手写 PEM 封装与
// 密钥派生参数（bcrypt kdf），出错的表现是"密钥看起来正常但登录被拒"，
// 极难排查。服务器上本就装着 OpenSSH。
func (a *app) egressSSHKeyGenerate(uid int64) (pem string, err error) {
	dir := filepath.Join(a.dataDir, "sshkeys", fmt.Sprintf("u%d", uid))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("创建密钥目录失败：%w", err)
	}
	keyPath := filepath.Join(dir, "id_ed25519")
	comment := fmt.Sprintf(egressAuthKeysCommentFmt, uid)

	// `-N ""` 空口令：agent 是非交互进程，有口令的密钥得配 ssh-agent，不合适。
	// `-C <comment>`：让它成为 authorized_keys 里可被精确识别的条目（幂等替换的锚）。
	cmd := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-C", comment, "-f", keyPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("生成密钥失败（ssh-keygen）：%v（输出：%s）",
			err, strings.TrimSpace(string(out)))
	}
	priv, err := os.ReadFile(keyPath)
	if err != nil {
		return "", fmt.Errorf("读取私钥失败：%w", err)
	}
	pub, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		return "", fmt.Errorf("读取公钥失败：%w", err)
	}
	// 公钥行只有一行 `<type> <b64> <comment>`；我们重写注释与选项。
	pubLine, err := a.egressAuthorizeKey(uid, strings.TrimSpace(string(pub)))
	if err != nil {
		return "", err
	}
	if err := a.store.SetKV(fmt.Sprintf(egressSSHPubKeyFmt, uid), pubLine); err != nil {
		return "", err
	}
	log.Printf("INFO: [egress] 已为用户 %d 生成受限隧道密钥（端口段 %s）", uid, egressPortSegText(uid))
	return string(priv), nil
}

// egressAuthorizeKey 把公钥（带限制）写进 authorized_keys，返回写入的那一行。
//
// ⚠️ 高危操作的三条纪律（写坏会导致**自己也登不进服务器**）：
//  1. **只追加、只删自己写过的行**（按 comment 精确匹配），其它条目原样保留；
//  2. 写前**备份**一次原文件（authorized_keys.wb2api.bak）；
//  3. **原子替换**（临时文件 + rename）+ 权限 0600。
func (a *app) egressAuthorizeKey(uid int64, pubKeyLine string) (string, error) {
	fields := strings.Fields(pubKeyLine)
	if len(fields) < 2 {
		return "", fmt.Errorf("公钥格式异常：%q", pubKeyLine)
	}
	keyType, keyBody := fields[0], fields[1]
	comment := fmt.Sprintf(egressAuthKeysCommentFmt, uid)

	// ⚠️ 顺序与内容都别动：`restrict` 在前（关掉一切），后面逐项放回。
	// `command="/bin/true"` 是**必需**的，不是保险 —— 见文件头「实测教训」。
	opts := []string{"restrict", "command=\"/bin/true\"", "port-forwarding"}
	base, span := egressPortSegOf(uid)
	for i := 0; i < span; i++ {
		opts = append(opts, fmt.Sprintf("permitlisten=\"127.0.0.1:%d\"", base+i))
	}
	line := strings.Join(opts, ",") + " " + keyType + " " + keyBody + " " + comment

	akPath, err := a.authorizedKeysPath()
	if err != nil {
		return "", err
	}
	var old string
	if b, err := os.ReadFile(akPath); err == nil {
		old = string(b)
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("读取 %s 失败：%w", akPath, err)
	}

	// 逐行过滤：丢掉**注释正是本用户**的旧行（幂等），其余一律保留。
	kept := make([]string, 0, 16)
	for _, l := range strings.Split(old, "\n") {
		s := strings.TrimSpace(l)
		if s == "" {
			continue
		}
		if strings.HasSuffix(s, " "+comment) || strings.HasSuffix(s, "\t"+comment) {
			continue
		}
		kept = append(kept, s)
	}
	// 保险：原有非本服务的条目数必须不变（防"过滤逻辑误伤"）。
	beforeOthers := 0
	for _, l := range strings.Split(old, "\n") {
		s := strings.TrimSpace(l)
		if s != "" && !strings.HasSuffix(s, " "+comment) && !strings.HasSuffix(s, "\t"+comment) {
			beforeOthers++
		}
	}
	if beforeOthers != len(kept) {
		return "", fmt.Errorf("authorized_keys 过滤校验失败（原 %d 条，保留 %d 条），已放弃写入", beforeOthers, len(kept))
	}
	kept = append(kept, line)
	content := strings.Join(kept, "\n") + "\n"

	if err := os.MkdirAll(filepath.Dir(akPath), 0o700); err != nil {
		return "", fmt.Errorf("创建 %s 目录失败：%w", filepath.Dir(akPath), err)
	}
	if old != "" {
		bak := akPath + ".wb2api.bak"
		if _, err := os.Stat(bak); os.IsNotExist(err) {
			if err := os.WriteFile(bak, []byte(old), 0o600); err != nil {
				log.Printf("WARN: [egress] 备份 authorized_keys 失败（继续写入）：%v", err)
			}
		}
	}
	tmp := akPath + fmt.Sprintf(".tmp%d", time.Now().UnixNano())
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return "", fmt.Errorf("写临时文件失败：%w", err)
	}
	if err := os.Rename(tmp, akPath); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("替换 %s 失败：%w", akPath, err)
	}
	return line, nil
}

// authorizedKeysPath 找到要写的 authorized_keys。
//
// 优先用注入的路径（测试/特殊部署）；否则：优先 root 的（webapp 以 root 运行时的
// 部署形态），找不到再试当前用户 HOME。
func (a *app) authorizedKeysPath() (string, error) {
	if p := strings.TrimSpace(a.authKeysPath); p != "" {
		return p, nil
	}
	return egressAuthorizedKeysPath()
}

// egressAuthorizedKeysPath 常规推导（见 authorizedKeysPath）。
func egressAuthorizedKeysPath() (string, error) {
	candidates := []string{"/root/.ssh/authorized_keys"}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates, filepath.Join(home, ".ssh", "authorized_keys"))
	}
	for _, c := range candidates {
		if _, err := os.Stat(filepath.Dir(c)); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("找不到可写的 .ssh 目录（尝试过：%s）——"+
		"本功能要求 webapp 以 root 运行（systemd 默认）", strings.Join(candidates, "、"))
}

// egressInstallScript 生成「一键安装」脚本（.bat）。
//
// 脚本里**只有服务地址与令牌**，没有任何密钥材料：
// 密钥由 agent 首次启动时凭令牌向 `/api/agent/config` 自举取回。
// 这样脚本短、不易被转义问题弄坏，而密钥也不会散落在浏览器下载历史里
// （下载目录里的 .bat 通常不会长期留存，但密钥文件会）。
func (a *app) egressInstallScript(r *http.Request, scope egressScope) (string, error) {
	origin := requestOrigin(r)
	if origin == "" {
		return "", fmt.Errorf("无法从请求推导服务地址")
	}
	tok, err := a.egressAgentTokenFor(scope.hostOwner())
	if err != nil {
		return "", err
	}
	// v0.31.53：只剩平台视角，安装包沿用**部署时那把密钥**（同目录 ai.pem），
	// 不再给用户生成专属隧道密钥 —— 那条路（用户自己接机器）已下线。
	if _, err := a.egressAgentTokenGet(); err != nil {
		return "", err
	}
	title := "WorkBuddy2API 本机出口代理(platform)"
	extraArgs := " -key ai.pem"
	hint := "echo NOTE: platform machine -- put the deployment key here as ai.pem\r\n"
	_ = scope // 保留参数：调用方仍传 scope，便于将来加"平台多机器"时区分

	// ⚠️ 全部用 ASCII：cmd 的代码页对中文注释极不友好（之前踩过 "中文 rem 被拆行"
	// 的坑）。窗口标题里的中文用 `title` 命令前先 chcp，且标题本身从简。
	script := "@echo off\r\n" +
		"chcp 65001 >nul\r\n" +
		"setlocal\r\n" +
		"title " + title + "\r\n" +
		"echo ============================================\r\n" +
		"echo  WorkBuddy2API local egress agent - one-click setup\r\n" +
		"echo  This window can be closed ONLY after it says READY.\r\n" +
		"echo  (after that the agent keeps running in background)\r\n" +
		"echo ============================================\r\n" +
		"set \"DIR=%LOCALAPPDATA%\\wb2api-egress\"\r\n" +
		"set \"SERVER=" + origin + "\"\r\n" +
		"set \"TOKEN=" + tok + "\"\r\n" +
		"if not exist \"%DIR%\" mkdir \"%DIR%\" >nul 2>nul\r\n" +
		"cd /d \"%DIR%\"\r\n" +
		"echo [1/4] downloading agent...\r\n" +
		"curl -fsSL -H \"X-Agent-Token: %TOKEN%\" -o agent.exe \"%SERVER%/api/egress/agent/bundle?part=agent\" || goto :fail\r\n" +
		"echo [2/4] downloading proxy...\r\n" +
		"curl -fsSL -H \"X-Agent-Token: %TOKEN%\" -o wbproxy.exe \"%SERVER%/api/egress/agent/bundle?part=proxy\" || goto :fail\r\n" +
		"echo [3/4] writing config...\r\n" +
		hint +
		"> egress.json echo {\"server\":\"%SERVER%\",\"token\":\"%TOKEN%\"}\r\n" +
		"echo [4/4] starting agent...\r\n" +
		"start \"wb2api-egress-agent\" /min agent.exe -config egress.json" + extraArgs + "\r\n" +
		"echo.\r\n" +
		"echo Done. The agent is starting now; keep this window if you want to watch logs.\r\n" +
		"echo You can now go back to the console and click [Pull up].\r\n" +
		"echo Installed at: %DIR%\r\n" +
		"pause\r\n" +
		"goto :eof\r\n" +
		":fail\r\n" +
		"echo.\r\n" +
		"echo [ERROR] download failed. Check your network, then run this file again.\r\n" +
		"echo Server: %SERVER%\r\n" +
		"pause\r\n" +
		"exit /b 1\r\n"
	return script, nil
}

// egressAgentBundlePath 服务器上 agent / wbproxy 可执行文件的位置。
//
// 部署时把它们放在 `/opt/wb2api/egress-agent/`（与 dist/egress-agent/ 同名文件）。
// 找不到时**明确报错**而不是回一个空文件：空文件被装到用户机上，表现是
// "agent 一闪而过"，用户完全看不出是服务端没带文件。
func egressAgentBundlePath(part string) (string, error) {
	base := ""
	// 允许用环境变量覆盖（开发/测试环境用）。
	if v := strings.TrimSpace(os.Getenv("WB2API_AGENT_DIR")); v != "" {
		base = v
	} else {
		exe, err := os.Executable()
		if err == nil {
			base = filepath.Join(filepath.Dir(exe), "egress-agent")
		}
	}
	if base == "" {
		return "", fmt.Errorf("无法定位 agent 交付目录")
	}
	switch part {
	case "", "agent":
		return filepath.Join(base, "wb2api-egress-agent.exe"), nil
	case "proxy":
		return filepath.Join(base, "wbproxy.exe"), nil
	default:
		return "", fmt.Errorf("未知的下载项：%q（可选 agent / proxy）", part)
	}
}

// egressAgentConfigPayload agent 自举用的配置（`GET /api/agent/config`）。
type egressAgentConfigPayload struct {
	Server string `json:"server"`
	Token  string `json:"token"`
	// PrivateKey 该用户专属的受限私钥（只有持有其令牌的人能取到自己的这一把）。
	PrivateKey string `json:"private_key,omitempty"`
	// User SSH 登录名（受限密钥仍用 root 登录，但被 restrict 关掉一切交互能力）。
	User string `json:"user"`
	// Note 给 agent 日志用的人话说明。
	Note string `json:"note,omitempty"`
}

// egressAgentConfigFor 组装某个 owner 的 agent 自举配置。
func (a *app) egressAgentConfigFor(r *http.Request, uid int64) (egressAgentConfigPayload, error) {
	out := egressAgentConfigPayload{Server: requestOrigin(r), User: "root"}
	tok, err := a.egressAgentTokenFor(uid)
	if err != nil {
		return out, err
	}
	out.Token = tok
	if uid > 0 {
		priv, err := a.egressSSHKeyEnsure(uid)
		if err != nil {
			return out, err
		}
		out.PrivateKey = priv
		out.Note = fmt.Sprintf("受限隧道密钥：只允许转发，且只能监听 127.0.0.1 的 %s 端口", egressPortSegText(uid))
	} else {
		out.Note = "平台机器：请沿用部署时配置的 SSH 私钥（-key 参数）"
	}
	return out, nil
}

// egressAgentConfigJSON 序列化（测试里也用它比对形状）。
func (p egressAgentConfigPayload) JSON() ([]byte, error) {
	return json.MarshalIndent(p, "", "  ")
}

// ---------------------------------------------------------------------------
// 一键安装 / 下载 / 自举（v0.31.25）
//
// 这一组接口的使命只有一句话：**让用户点一下就把 agent 装起来，不再复制粘贴
// 任何参数、任何密钥**。三者的分工：
//
//	GET /api/egress/home/install   → 下载一个 .bat（只含服务地址 + 自己的令牌）
//	GET /api/egress/agent/bundle   → 下发 agent.exe / wbproxy.exe（令牌鉴权）
//	GET /api/agent/config          → agent 首次启动时凭令牌**自举**取回自己的私钥
//
// 为什么密钥不放在 .bat 里：.bat 会进浏览器下载目录（往往长期留存），
// 而私钥一旦落在那儿就是一份散落的凭证。改成"agent 自己去取"之后，
// 私钥只出现在 agent 的工作目录里，且随时可以作废重发。
// ---------------------------------------------------------------------------

// handleEgressInstall GET /api/egress/home/install
//
// 返回一个可双击运行的 .bat。**响应是附件下载**（不是页面），浏览器会直接存盘。
func (a *app) handleEgressInstall(w http.ResponseWriter, r *http.Request) {
	// v0.31.53：安装包只面向平台机器（路由挂 adminGuard）。
	// 不再按登录用户生成 —— 用户端代理下线后，用户手里不该再有"带令牌的安装包"。
	scope := egressScope{Platform: true}
	if u := userFrom(r); u != nil {
		scope.UserID = u.ID
	}
	script, err := a.egressInstallScript(r, scope)
	if err != nil {
		// 生成失败必须**说清原因**：最常见的是 webapp 不是 root（写不了
		// authorized_keys）或 ssh-keygen 缺失 —— 这两条都需要管理员动手，
		// 笼统的"内部错误"会让用户反复重试。
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "生成安装脚本失败：" + err.Error(),
			"hint":  "该功能需要服务器具备 ssh-keygen，且 webapp 以 root 运行（systemd 默认）",
		})
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="wb2api-egress-setup.bat"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(script)))
	_, _ = w.Write([]byte(script))
}

// handleEgressAgentBundle GET /api/egress/agent/bundle?part=agent|proxy
//
// 下发 agent 可执行文件。**要令牌**：否则这个端点就是一个公开的 15MB 下载位
// （盗链 / 被扫到就白烧带宽）。令牌只放到"能启动 agent"的人手里，正好。
func (a *app) handleEgressAgentBundle(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.egressAgentAuth(r); !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid agent token"})
		return
	}
	path, err := egressAgentBundlePath(r.URL.Query().Get("part"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	f, err := os.Open(path)
	if err != nil {
		// ⚠️ 最常见的部署遗漏：只更新了 webapp，没把 egress-agent/ 目录传上去。
		// 明确点名"缺什么、放哪儿"，而不是回一个 404。
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "服务端未随部署提供该文件：" + path,
			"hint":  "请把 dist/egress-agent/ 下的 wb2api-egress-agent.exe 与 wbproxy.exe 放到 webapp 同级的 egress-agent/ 目录",
		})
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	name := filepath.Base(path)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	// ServeContent 处理 Range / Content-Length（下载器会用到）。
	http.ServeContent(w, r, name, st.ModTime(), f)
}

// handleEgressAgentConfig GET /api/agent/config
//
// agent 的**自举**入口：凭令牌取回自己的服务地址、令牌与受限私钥。
//
// 为什么让 agent 自举而不是把密钥塞进安装包：见文件头注释。安全上等同于
// "拿着令牌的人能拿到自己的私钥" —— 而令牌本来就是他自己的，没有放大。
func (a *app) handleEgressAgentConfig(w http.ResponseWriter, r *http.Request) {
	uid, ok := a.egressAgentAuth(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid agent token"})
		return
	}
	// v0.31.53：这个端点会下发**受限私钥**。用户端代理下线后，用户身份的
	// agent 不该再拿到任何凭证 —— 拒在这里比"下去之后再删"可靠得多。
	if uid > 0 {
		writeUserEgressGone(w)
		return
	}
	cfg, err := a.egressAgentConfigFor(r, uid)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}
