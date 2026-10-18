// wb2api-egress-agent —— 本机出口代理 agent（配 WorkBuddy2API v0.31.18+ 使用）
//
// 职责单一：**把"本机的网络出口"拉起来，并让它一直活着。**
//
// 它做两件事：
//  1. 跑本地代理进程（wbproxy）—— 一个 HTTP/SOCKS 代理，出口 = 这台机器的宽带；
//  2. 建一条到云端的 **SSH 反向隧道** —— 让云端能通过它访问上面那个代理。
//
// 为什么需要它（而不是在控制台直接"拉起"）：
//
//	云端**无法主动连你的电脑**（你在家庭 NAT 后面）。所以"拉起"这件事只能由
//	本机主动完成。agent 用**轮询**解决：云端把你的点击变成一条指令，agent 取到
//	指令后就地执行 —— 全程**只出不进**，不需要在你的路由器上开任何端口，
//	也不需要在这台机器上运行 SSH 服务端。
//
// # 多机支持（v0.2.0 / 服务端 v0.31.18）
//
// 同一把 agent 令牌可以在**多台电脑**上使用（家里台式机 + 笔记本很常见）。
// 为此本 agent 会给自己生成一个**跨重启稳定**的 machine id（落在同目录的
// `machine.id`），并随每次 poll / report 上报。服务端据此：
//
//	· 给每台机器分配**不同的隧道端口**（11080、11081…）—— 否则两台都
//	  `ssh -R 11080`，后到的那台必然失败，而错误只出现在它自己的日志里；
//	· 把指令**定向**发给被点的那一台（而不是"谁最近说过话就命令谁"）；
//	· 把状态按机器分桶（否则两台每 5 秒互相覆盖，页面上的出口 IP 会乱跳）。
//
// 用法（在控制台「代理」页复制那行命令即可，不必手填任何参数）：
//
//	wb2api-egress-agent.exe -server http://<你的服务地址> -token <令牌>
//
// 可选参数：
//
//	-proxy   wbproxy 可执行文件路径（默认：与本程序同目录的 wbproxy.exe）
//	-key     SSH 私钥路径（默认：与本程序同目录的 ai.pem）
//	-user    SSH 用户名（默认 root）
//	-machine 覆盖机器标识（默认：同目录 machine.id；一般不用填）
package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const agentVersion = "0.3.3"

// pollInterval 轮询间隔。取 5s：
//   - 用户的体感是"点完几秒内就动起来"；
//   - 对云端是每分钟 12 次极轻请求（一个空 JSON），完全可忽略。
const pollInterval = 5 * time.Second

// keepAliveInterval 子进程保活的检查间隔。
const keepAliveInterval = 10 * time.Second

type cmdSpec struct {
	Action     string `json:"action"` // start / stop / none
	At         int64  `json:"at"`
	ProxyPort  int    `json:"proxy_port"`
	TunnelPort int    `json:"tunnel_port"`
}

type agentState struct {
	Version  string `json:"version"`
	Hostname string `json:"hostname"`
	ProxyOK  bool   `json:"proxy_ok"`
	TunnelOK bool   `json:"tunnel_ok"`
	// ExitIP 纯 IP（v0.3.1 起）。此前是「IP（地区 ISP）」复合串，云端无法按 IP 比较，
	// 于是"两条线路其实是同一个出口"这件事在后台完全看不出来。
	ExitIP string `json:"exit_ip,omitempty"`
	// ExitIPNote 归属地/运营商（原复合串括号里的部分），只给人看。
	ExitIPNote string `json:"exit_ip_note,omitempty"`
	// Fingerprint 机器指纹（v0.3.1）：hostname + 主网卡 MAC + 首选内网 IP 的哈希。
	// 用来识破「同一台机器跑了两份 agent」，以及「拷贝安装目录到另一台电脑导致
	// machine.id 重复」这两种会让后台误以为有两条独立线路的情况。
	Fingerprint string `json:"fingerprint,omitempty"`
	// LocalIP 本机首选内网地址（人眼核对"这是哪台电脑"）。
	LocalIP string `json:"local_ip,omitempty"`
	Message string `json:"message,omitempty"`
	// AuthCandidates 本机探测到的客户端登录态候选（v0.31.42）。
	// **只有元信息**：路径 / 修改时间 / 大小 / uid 前 8 位 / 结论。不含任何令牌。
	AuthCandidates []authFileCandidate `json:"auth_candidates,omitempty"`
}

type agent struct {
	server string
	token  string
	proxy  string
	key    string
	user   string
	// machine 这台机器的稳定标识（见 agentMachineID）。
	machine string

	mu         sync.Mutex
	proxyCmd   *exec.Cmd
	sshCmd     *exec.Cmd
	proxyPort  int
	tunnelPort int
	lastAt     int64 // 已执行过的指令时刻（幂等：同一时刻只执行一次）
	lastAction string
	msg        string
	exitIP     string
	exitNote   string
	// fp / localIP 启动时算一次即可：MAC 与内网地址不会在运行期变化，
	// 每 5 秒重算一遍只是白白遍历网卡。
	fp      string
	localIP string
	// noUpdate 关闭自动更新（--no-update）。
	noUpdate bool
	// authCand 最近一次扫到的登录态候选（只含元信息，见 authscan.go）。
	// authCandAt 上次扫描时刻：5 秒一次的 report 不该每次都去走目录。
	authCand   []authFileCandidate
	authCandAt time.Time
}

func (a *agent) setMsg(f string, args ...any) {
	a.mu.Lock()
	a.msg = fmt.Sprintf(f, args...)
	msg := a.msg
	a.mu.Unlock()
	// 用 Print 而不是 Printf：msg 是**已经格式化过**的文本，
	// 再走一遍 Printf 会把文本里偶然出现的 % 当成格式动词（且 go vet 会报
	// non-constant format string）。
	log.Print(msg)
}

// ---------- 与云端的两次调用 ----------

func (a *agent) post(path string, body any, out any) error {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, a.server+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("X-Agent-Token", a.token)
	req.Header.Set("Content-Type", "application/json")
	return a.do(req, out)
}

func (a *agent) get(path string, out any) error {
	req, err := http.NewRequest(http.MethodGet, a.server+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Agent-Token", a.token)
	return a.do(req, out)
}

func (a *agent) do(req *http.Request, out any) error {
	cl := &http.Client{Timeout: 15 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// poll 取指令。云端**每次都回当前指令**（不清空），幂等靠 At 比对。
//
// 带 machine / hostname：云端据此把指令定向到本机（多机支持）。
// 老版本服务端不认这两个参数也不受影响（多余的 query 会被忽略）。
func (a *agent) poll() (cmdSpec, error) {
	var spec cmdSpec
	// 带上指纹（v0.3.2）：云端在**第一次** poll 就能分清"这是哪台机器"。
	// 不带的话，复制安装目录的第二台电脑会先用同一个 machine.id 登记一次，
	// 直到下一次 report 才被指纹纠正 —— 那一小段时间里它拿的是别人的端口。
	err := a.get("/api/agent/poll?machine="+url.QueryEscape(a.machine)+
		"&hostname="+url.QueryEscape(hostname())+
		"&fingerprint="+url.QueryEscape(a.fp), &spec)
	return spec, err
}

func (a *agent) report() {
	st := agentState{Version: agentVersion, Hostname: hostname()}
	a.mu.Lock()
	st.ProxyOK = a.proxyCmd != nil
	st.TunnelOK = a.sshCmd != nil
	st.ExitIP = a.exitIP
	st.ExitIPNote = a.exitNote
	st.Message = a.msg
	a.mu.Unlock()
	st.Fingerprint = a.fp
	st.LocalIP = a.localIP
	a.mu.Lock()
	st.AuthCandidates = a.authCand
	a.mu.Unlock()
	_ = a.post("/api/agent/report?machine="+url.QueryEscape(a.machine), st, nil)
}

// ---------- 子进程管理 ----------

// running 是否有子进程在跑（用于"端口变了要先停"的判断）。
func (a *agent) running() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.proxyCmd != nil || a.sshCmd != nil
}

// startProxy 起本地代理（幂等：已在跑就返回）。
func (a *agent) startProxy(port int) error {
	a.mu.Lock()
	if a.proxyCmd != nil {
		a.mu.Unlock()
		return nil
	}
	a.mu.Unlock()

	bin := a.proxy
	if _, err := os.Stat(bin); err != nil {
		return fmt.Errorf("找不到代理程序 %s（请与 wbproxy.exe 放在同一目录，或用 -proxy 指定）", bin)
	}
	cmd := exec.Command(bin, "-addr", fmt.Sprintf("127.0.0.1:%d", port))
	cmd.Dir = filepath.Dir(bin)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动代理失败: %w", err)
	}
	a.mu.Lock()
	a.proxyCmd = cmd
	a.proxyPort = port
	a.mu.Unlock()
	go func() { // 退出即置空（主循环据此保活）
		_ = cmd.Wait()
		a.mu.Lock()
		if a.proxyCmd == cmd {
			a.proxyCmd = nil
		}
		a.mu.Unlock()
		log.Printf("[agent] 代理进程已退出，将在保活检查中重启")
	}()
	a.setMsg("本地代理已启动（127.0.0.1:%d）", port)
	return nil
}

// startTunnel 建 SSH 反向隧道（幂等）。
//
// `-R port:127.0.0.1:proxyPort` 的中间 host 写 **127.0.0.1**，表示由 **SSH 客户端
// （也就是这台机器）** 侧解析 —— 于是隧道出口落在这台机器的代理上，而不是云端。
// 这一点写反（写成云端地址）会静默形成一条"云端到自己"的空隧道。
//
// ⚠️ 多机时隧道端口由**云端分配**（11080 / 11081 …）：两台机器抢同一个端口时
// `ExitOnForwardFailure=yes` 会让后到的那条直接退出，日志里是
// `remote port forwarding failed for listen port N` —— 这正是"第二台电脑点了没反应"
// 的典型原因，故端口必须来自云端而不是写死。
func (a *agent) startTunnel(host string, tunnelPort, proxyPort int) error {
	a.mu.Lock()
	if a.sshCmd != nil {
		a.mu.Unlock()
		return nil
	}
	key := a.key
	a.mu.Unlock()

	// ⚠️ 监听地址**必须显式写 127.0.0.1**（v0.31.25）：服务端给用户密钥配了
	// `permitlisten="127.0.0.1:12xxx"` 白名单，而不带地址的 `-R 12010:...`
	// 请求的监听地址是"默认"（sshd 按 GatewayPorts 解析），与白名单里的显式
	// 127.0.0.1 不匹配 → **隧道会被 sshd 拒绝**，现象只是"连不通"。
	args := []string{
		"-i", key,
		"-N", // 不执行远程命令，只做转发
		"-R", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", tunnelPort, proxyPort),
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ExitOnForwardFailure=yes", // 转发建不起来就直接退出，别挂着一个空隧道
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=3",
		fmt.Sprintf("%s@%s", a.user, host),
	}
	cmd := exec.Command("ssh", args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("建立隧道失败: %w", err)
	}
	a.mu.Lock()
	a.sshCmd = cmd
	a.tunnelPort = tunnelPort
	a.mu.Unlock()
	go func() {
		_ = cmd.Wait()
		a.mu.Lock()
		if a.sshCmd == cmd {
			a.sshCmd = nil
		}
		a.mu.Unlock()
		log.Printf("[agent] 隧道已断开，将在保活检查中重连")
	}()
	a.setMsg("反向隧道已建立（云端 127.0.0.1:%d → 本机 127.0.0.1:%d）", tunnelPort, proxyPort)
	return nil
}

// stopAll 停掉两个子进程。
func (a *agent) stopAll() {
	a.mu.Lock()
	pc, sc := a.proxyCmd, a.sshCmd
	a.proxyCmd, a.sshCmd = nil, nil
	a.mu.Unlock()
	for _, c := range []*exec.Cmd{sc, pc} {
		if c != nil && c.Process != nil {
			_ = c.Process.Kill()
		}
	}
	a.setMsg("已停止（代理与隧道均关闭）")
}

// probeExitIP 通过本地代理查一次出口 IP。
//
// 这是**最有说服力的一行状态**：它证明"流量真的从这台机器出去了"，
// 而不只是"进程起来了"。失败不是错误（有些网络不允许访问查询接口），
// 留空即可 —— 但控制台会因此少一个可核对的证据。
func (a *agent) probeExitIP(proxyPort int) {
	pu, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", proxyPort))
	if err != nil {
		return
	}
	cl := &http.Client{
		Timeout:   12 * time.Second,
		Transport: &http.Transport{Proxy: http.ProxyURL(pu)},
	}
	resp, err := cl.Get("http://ip-api.com/json/?fields=query,country,regionName,city,isp")
	if err != nil {
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var d struct {
		Query      string `json:"query"`
		RegionName string `json:"regionName"`
		City       string `json:"city"`
		ISP        string `json:"isp"`
	}
	if json.Unmarshal(raw, &d) != nil || d.Query == "" {
		return
	}
	a.mu.Lock()
	a.exitIP = d.Query
	a.exitNote = strings.TrimSpace(strings.Join([]string{d.RegionName, d.City, d.ISP}, " "))
	a.mu.Unlock()
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

// agentMachineID 这台机器的稳定标识（v0.2.0 多机支持）。
//
// 为什么必须"稳定"：云端按它分桶存指令与状态。若每次启动都换一个 id，
// 后果是 ① 云端列表里堆出一串"历史机器"（每重启一次多一条）；
// ② 端口一路往后涨（11080 → 11081 → 11082 …）直到某天耗尽；
// ③ 用户看到的是"我明明只有两台电脑，为什么列了 9 台"。
//
// 所以：优先读同目录的 machine.id（首次运行时生成并落盘）。
// 落盘失败（只读目录）时退化为 hostname —— 同一台机器仍然稳定。
func agentMachineID(dir string) string {
	fp := filepath.Join(dir, "machine.id")
	if b, err := os.ReadFile(fp); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			return s
		}
	}
	id := "m-" + strings.ToLower(strings.TrimSpace(hostname()))
	if id == "m-" {
		id = "m-unknown"
	}
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err == nil {
		id = "m-" + hex.EncodeToString(buf)
	}
	if err := os.WriteFile(fp, []byte(id+"\n"), 0o600); err != nil {
		log.Printf("[agent] 无法写入 %s（%v）—— 本次使用 %s；重启后它会变（请把 agent 放在可写目录）", fp, err, id)
		return id
	}
	return id
}

// agentConfig 本地配置文件（`-config egress.json`）。
//
// 为什么要支持配置文件：一键安装脚本是**用 echo 拼**出来的，命令行长参数
// 在 cmd 里的转义（引号、`&`、空格）是个无底洞。把变量放进 JSON 文件，
// 脚本就只剩"下载 + 起进程"两件事。
type agentConfig struct {
	Server string `json:"server"`
	Token  string `json:"token"`
	// User SSH 登录名（默认 root）。
	User string `json:"user,omitempty"`
	// KeyFile 私钥路径（默认同目录 key.pem；平台机器请放部署时那把并写这里）。
	KeyFile string `json:"key_file,omitempty"`
}

// agentBootstrap 云端下发的自举配置（`GET /api/agent/config`）。
type agentBootstrap struct {
	Server     string `json:"server"`
	Token      string `json:"token"`
	PrivateKey string `json:"private_key,omitempty"`
	User       string `json:"user"`
	Note       string `json:"note,omitempty"`
}

// loadAgentConfig 读配置文件（不存在则返回零值，不算错误）。
func loadAgentConfig(path string) (agentConfig, error) {
	var c agentConfig
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("配置文件 %s 不是合法 JSON：%w", path, err)
	}
	return c, nil
}

// bootstrapKey 凭令牌向云端取回自己的受限私钥，落盘到 dir/key.pem。
//
// 这是"一键安装"能不含任何密钥材料的关键：安装脚本只带服务地址与令牌，
// 私钥由 agent 首次启动时自取。于是浏览器下载目录里不会留下一份私钥，
// 而真的要作废时也只需在服务端重新生成（下一次启动又是新的）。
//
// 返回 keyPath 为空表示"云端没给私钥"（平台机器：管理员本机沿用部署密钥），
// 这不是错误 —— 调用方据此决定要不要继续用 -key。
func bootstrapKey(server, token, dir string) (keyPath, user, note string, err error) {
	if strings.TrimSpace(token) == "" {
		return "", "", "", fmt.Errorf("缺少令牌，无法自举")
	}
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(server, "/")+"/api/agent/config", nil)
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("X-Agent-Token", token)
	cl := &http.Client{Timeout: 20 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return "", "", "", fmt.Errorf("向云端取配置失败：%w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("云端拒绝取配置（HTTP %d）：%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var bs agentBootstrap
	if err := json.Unmarshal(body, &bs); err != nil {
		return "", "", "", fmt.Errorf("云端返回的配置无法解析：%w", err)
	}
	user, note = bs.User, bs.Note
	if strings.TrimSpace(bs.PrivateKey) == "" {
		return "", user, note, nil // 平台机器：由 -key 提供
	}
	keyPath = filepath.Join(dir, "key.pem")
	// 0600：私钥不该被同机其他用户读到。
	if err := os.WriteFile(keyPath, []byte(bs.PrivateKey), 0o600); err != nil {
		return "", user, note, fmt.Errorf("写入私钥失败（%s）：%w", keyPath, err)
	}
	return keyPath, user, note, nil
}

// sshHost 从 server 地址里取主机名（隧道连的就是这个主机）。
func sshHost(server string) string {
	u, err := url.Parse(server)
	if err != nil || u.Host == "" {
		return ""
	}
	h := u.Hostname()
	return h
}

// portOr 端口缺省值。
func portOr(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func main() {
	var (
		server   = flag.String("server", "", "云端服务地址，如 http://1.2.3.4:8780（必填）")
		token    = flag.String("token", "", "控制台「代理」页给出的 agent 令牌（必填）")
		proxy    = flag.String("proxy", "", "wbproxy 可执行文件路径（默认同目录 wbproxy.exe）")
		key      = flag.String("key", "", "SSH 私钥路径（默认同目录 ai.pem）")
		user     = flag.String("user", "root", "SSH 用户名")
		machine  = flag.String("machine", "", "覆盖机器标识（默认同目录 machine.id）")
		once     = flag.Bool("once", false, "只跑一轮就退出（自检用：验证令牌与网络是否可用）")
		cfgPath  = flag.String("config", "", "配置文件路径（一键安装会写 egress.json，含 server/token）")
		noUpdate = flag.Bool("no-update", false, "关闭自动更新（排查/回滚时用）")
	)
	flag.Parse()

	// 配置文件里的值作为**缺省**被命令行覆盖（命令行优先，便于临时排查）。
	if *cfgPath != "" {
		c, err := loadAgentConfig(*cfgPath)
		if err != nil {
			fmt.Println("读取配置失败：" + err.Error())
			os.Exit(2)
		}
		if *server == "" {
			*server = c.Server
		}
		if *token == "" {
			*token = c.Token
		}
		if *user == "root" && c.User != "" {
			*user = c.User
		}
		if *key == "" && c.KeyFile != "" {
			*key = c.KeyFile
		}
	}

	if *server == "" || *token == "" {
		fmt.Println("用法：wb2api-egress-agent.exe -server http://<服务地址> -token <令牌>")
		fmt.Println("      或：wb2api-egress-agent.exe -config egress.json")
		fmt.Println("（控制台「代理」页的「一键安装」会把这些都准备好，不需要手填）")
		os.Exit(2)
	}
	base := strings.TrimRight(*server, "/")
	host := sshHost(base)
	if host == "" {
		fmt.Println("-server 需要形如 http://1.2.3.4:8780 的地址")
		os.Exit(2)
	}
	self, err := os.Executable()
	if err != nil {
		self = "."
	}
	dir := filepath.Dir(self)
	if *proxy == "" {
		*proxy = filepath.Join(dir, "wbproxy.exe")
	}
	if *key == "" {
		*key = filepath.Join(dir, "key.pem")
	}
	// 自举（v0.31.25）：私钥不存在就凭令牌向云端取一次，落盘后继续。
	// 这样"一键安装"的脚本里不需要带任何密钥材料。
	if _, statErr := os.Stat(*key); statErr != nil {
		kp, bu, note, err := bootstrapKey(base, *token, dir)
		switch {
		case err != nil:
			log.Printf("[agent] 自举失败：%v", err)
			log.Printf("[agent] 请确认令牌有效（控制台「代理」页可重新下载安装包）")
			os.Exit(3)
		case kp != "":
			*key = kp
			if bu != "" {
				*user = bu
			}
			log.Printf("[agent] 已自举隧道密钥：%s（%s）", kp, note)
		default:
			log.Printf("[agent] 云端未下发私钥（平台机器）——需要 -key 指向部署用的私钥文件")
			if note != "" {
				log.Printf("[agent] %s", note)
			}
		}
	}
	mid := strings.TrimSpace(*machine)
	if mid == "" {
		mid = agentMachineID(dir)
	}

	a := &agent{
		server: base, token: *token, proxy: *proxy, key: *key, user: *user, machine: mid,
		fp: machineFingerprint(), localIP: localIPv4(), noUpdate: *noUpdate,
	}
	log.Printf("wb2api-egress-agent %s 启动 | 云端=%s | 机器=%s(%s) | 指纹=%s | 内网=%s | 代理=%s | 私钥=%s",
		agentVersion, base, mid, hostname(), a.fp, a.localIP, *proxy, *key)

	// 自检模式：只验一次令牌与网络，然后退出。
	if *once {
		spec, err := a.poll()
		if err != nil {
			log.Printf("自检失败：%v", err)
			os.Exit(1)
		}
		log.Printf("自检通过：令牌有效，当前指令 action=%q（本机隧道端口应为 %d）", spec.Action, spec.TunnelPort)
		return
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		log.Printf("收到退出信号，正在停止子进程…")
		a.stopAll()
		os.Exit(0)
	}()

	// 自更新（v0.31.42）：启动先看一眼，之后每 6 小时一次。
	//
	// 放在启动时而不是后台 goroutine：换文件后本进程会退出，逻辑必须能在主循环里
	// 干净地"走到一半就不返回"，不必再操心并发退出。
	lastUpdate := time.Now().Add(updateCheckInterval) // 启动时立刻检查一次
	// auth 候选扫描（v0.31.42）：启动扫一次、之后每 5 分钟一次。
	// 只上报元信息（路径/mtime/uid 前缀），不含任何令牌 —— 见 authscan.go 的边界说明。
	scanAuth := func() {
		c := scanAuthFiles()
		a.mu.Lock()
		a.authCand, a.authCandAt = c, time.Now()
		a.mu.Unlock()
		if len(c) > 0 {
			log.Printf("[authscan] 探测到 %d 个候选登录态文件（只上报路径与元信息，不上传令牌）", len(c))
		}
	}
	scanAuth()

	lastKeepAlive := time.Now()
	for {
		if time.Since(lastUpdate) >= updateCheckInterval {
			lastUpdate = time.Now()
			if err := a.maybeSelfUpdate(); err != nil {
				// 更新失败**不影响主功能**：继续跑当前版本，6 小时后再试。
				log.Printf("[update] 自动更新未完成：%v", err)
			}
		}
		a.mu.Lock()
		needScan := time.Since(a.authCandAt) > 5*time.Minute
		a.mu.Unlock()
		if needScan {
			scanAuth()
		}
		spec, err := a.poll()
		if err != nil {
			a.setMsg("轮询云端失败（将自动重试）：%v", err)
		} else {
			switch spec.Action {
			case "start":
				wantPP, wantTP := portOr(spec.ProxyPort, 1080), portOr(spec.TunnelPort, 11080)
				a.mu.Lock()
				executed := spec.At == a.lastAt && a.lastAction == "start"
				curPP, curTP := a.proxyPort, a.tunnelPort
				a.mu.Unlock()
				// 端口变了（多机时云端会给每台分配不同端口）→ 先停再起。
				// 否则隧道还指着旧端口，表现是"状态显示已拉起，但云端永远连不通"。
				if (curPP != 0 && curPP != wantPP) || (curTP != 0 && curTP != wantTP) {
					if a.running() {
						log.Printf("[agent] 端口变化（%d→%d / %d→%d），重建代理与隧道",
							curPP, wantPP, curTP, wantTP)
						a.stopAll()
					}
					executed = false
				}
				if !executed {
					if err := a.startProxy(wantPP); err != nil {
						a.setMsg("拉起失败：%v", err)
					} else if err := a.startTunnel(host, wantTP, wantPP); err != nil {
						a.setMsg("拉起失败：%v", err)
					} else {
						a.mu.Lock()
						a.lastAt, a.lastAction = spec.At, "start"
						a.mu.Unlock()
						go a.probeExitIP(wantPP)
					}
				}
			case "stop":
				a.mu.Lock()
				executed := spec.At == a.lastAt && a.lastAction == "stop"
				a.mu.Unlock()
				if !executed {
					a.stopAll()
					a.mu.Lock()
					a.lastAt, a.lastAction = spec.At, "stop"
					a.mu.Unlock()
				}
			}
		}

		// 保活：子进程掉了就重拉（只在"当前应为运行态"时）。
		if time.Since(lastKeepAlive) > keepAliveInterval {
			lastKeepAlive = time.Now()
			a.mu.Lock()
			want := a.lastAction == "start"
			pp, tp := portOr(a.proxyPort, 1080), portOr(a.tunnelPort, 11080)
			proxyAlive, sshAlive := a.proxyCmd != nil, a.sshCmd != nil
			a.mu.Unlock()
			if want {
				if !proxyAlive {
					_ = a.startProxy(pp)
				}
				if !sshAlive {
					_ = a.startTunnel(host, tp, pp)
				}
			}
		}

		a.report()
		time.Sleep(pollInterval)
	}
}

// machineFingerprint 这台机器的物理指纹（v0.3.1）。
//
// 为什么不能只用 machine.id：那个 id 是 agent 自己生成、存在同目录 machine.id 里的。
// 用户把整个安装目录拷到另一台电脑（很常见：家里两台机都想接进来）→ 两份 agent
// 报同一个 id，云端按 owner+id 分桶就会把两台并成一台，第二台永远抢不到端口。
// 指纹取的是 hostname + 主网卡 MAC + 首选内网 IP，跨目录、跨版本都稳定，
// 且不是 agent 随便填的 —— 后台据此能直接说"这两条线路是同一台机器"。
//
// 取不到 MAC（容器/权限受限）时退化为 hostname+内网 IP：仍然是"同一台机器会相同"，
// 只是区分度低一些，并在日志里说明，不假装自己拿到了。
func machineFingerprint() string {
	parts := []string{strings.ToLower(strings.TrimSpace(hostname())), primaryMAC(), localIPv4()}
	seed := strings.Join(parts, "|")
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:8])
}

// primaryMAC 第一块"像真网卡"的 MAC（跳过回环、全零、虚拟网桥常见的空值）。
func primaryMAC() string {
	ifs, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, f := range ifs {
		if f.Flags&net.FlagLoopback != 0 || len(f.HardwareAddr) == 0 {
			continue
		}
		// 全零 MAC（部分虚拟网卡）没有区分度，跳过。
		zero := true
		for _, b := range f.HardwareAddr {
			if b != 0 {
				zero = false
				break
			}
		}
		if zero {
			continue
		}
		return strings.ToLower(f.HardwareAddr.String())
	}
	return ""
}

// localIPv4 本机首选内网地址（不发包：UDP "连接"只做路由选择）。
func localIPv4() string {
	c, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer c.Close()
	if a, ok := c.LocalAddr().(*net.UDPAddr); ok {
		return a.IP.String()
	}
	return ""
}
