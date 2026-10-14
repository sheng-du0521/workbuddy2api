<div align="center">

# WorkBuddy2API

**把 CodeBuddy 账号变成 OpenAI 兼容 API 的自托管网关**

OAuth 登录 · 账号池轮转 · 熔断与冷却 · 会话粘性 · 定时积分任务 · 中文可视化面板

![Go](https://img.shields.io/badge/Go-1.26.0-00ADD8?logo=go&logoColor=white&style=flat-square)
![API](https://img.shields.io/badge/API-OpenAI_Compatible-412991?style=flat-square)
![Deploy](https://img.shields.io/badge/Deploy-Docker_/_Windows-2496ED?style=flat-square)
![Transport](https://img.shields.io/badge/Transport-SSE_/_Streaming-0DBD8B?style=flat-square)
![License](https://img.shields.io/badge/License-MIT-22c55e?style=flat-square)

**一个可执行文件 = 网站 + 管理后台 + OpenAI 兼容网关**

</div>

---

> [!TIP]
> ### 🤝 寻求 IP 代理商赞助（欢迎代理 IP / 住宅 IP 服务商）
>
> 本项目的防关联能力依赖「**一个账号一条独立出口 IP**」，住宅 / 机房代理是它唯一的现实
> 来源（本仓已把出口线路池做完整：线路池 · 账号单绑定 · 真实出口 IP 探活 ·
> **失效不静默换出口**）。
>
> **欢迎赞助本项目**：提供几十 MB 或几天的**测试额度**即可 —— 用于验证线路可用性与
> 真实出口 IP。需要的形态：**HTTP / HTTPS / SOCKS5** 任一，要求**一条线路一个稳定源 IP**、
> 支持用户名密码认证。作为回报，我们会在此处与后台「出口代理池」页注明线路来源。
>
> **联系方式：抖音 `tgy25416`**（作者唯一联系方式）。

> [!WARNING]
> ### 禁止商用倒卖
>
> **禁止将本项目源代码（含衍生版本、修改版本及任何实质性相同的副本）用于商业转售、
> 倒卖或付费分发** —— 包括出售源码 / "源码授权"、以"付费进群 / 教程 / 加盟费 / 代理费 /
> 技术支持费"等名义把源码作为对价交付、以及声称已获作者授权。
>
> **允许**：个人学习、研究、测试；自行部署使用；非营利性的分享、fork 与二次开发。
>
> **法律边界的诚实说明**：本项目采用 MIT License，该授权**不可撤销** —— 已获得副本的人
> 本就拥有使用、复制、修改、分发等权利，本声明**无法收回**。它的准确定位是
> **作者意愿的明确表达** + **"从未作出商业倒卖授权"的事实记录**，并在**商标与品牌层面**
> 具备实际效力。完整说明见 [NOTICE](NOTICE)。

---

## 这是什么

`WorkBuddy2API` 把腾讯 CodeBuddy（`copilot.tencent.com`）的账号包装成**统一的 OpenAI
兼容接口**。官方不提供 OpenAI 形态的开放 API，本项目通过 OAuth 设备授权取得凭证，
在网关侧做 token 自动刷新、**账号池调度**与**流量治理**。

面向**个人多账号**场景。对客户端只暴露 OpenAI 兼容接口，现有 SDK / 前端 / 工具零改造接入。

| 你原本要做的 | 本项目的做法 |
|---|---|
| 一个账号用完就没了 | **账号池轮转** —— 多号共享、单号故障自动换号 |
| 上游 429 / 挂掉就雪崩 | **熔断 + 分级冷却**（软限流指数退避 · 402 硬冷却至次日 · 5xx 熔断） |
| 多轮对话中途莫名换号 | **会话粘性** —— 同一对话尽量绑同一账号，跨重启不丢 |
| 不知道怎么管 27 个号 | **中文 Web 面板** —— 账号池视图、积分明细、签到/任务、出口线路、诊断接口 |
| 手动跑签到脚本 | **定时任务** —— 签到 / 活跃 / 旅行 / 保活四类独立开关 |
| 加号怕被关联 | **导入防关联门禁**（换 IP + 单批上限）+ 每号独立设备标识 + 出口单绑定 |
| 不知道谁用了多少 | 每次调用的模型、Token、上游账号、扣费、会话键全量留痕 |

---

## 功能亮点

### 账号池治理

- **加权轮转** —— 三因子加权随机（额度 / 快过期积分 / 闲置补偿）+ Top5 短名单 + 防撞号窗口
- **分级冷却** —— 429 软冷却指数退避 · 6004 模型级冷却对齐上游重置墙钟 · 402 硬冷却至次日 04:00 · 5xx 熔断
- **错误分类处置** —— 上游业务码逐个定级（11101 不罚号 / 11128 渠道级不罚号 / 11140 累计阈值才摘 / 12153 连续计数）
- **运行态九态** —— `online` / `cooling` / `rate_limited` / `risk_cooling` / `exhausted` / `relogin` / `egress_error` / `disabled` / `manual_disabled`
- **两个独立停用位** —— 运维手动停用与系统判定禁用各自独立，恢复时各清各的

### 防风控

- **出口线路池** —— 多线路 + 权重 + 停用 + **账号单绑定** + 全局兜底；真实出口 IP 探活；坏线路自动摘除
- **不静默换出口**（默认）—— 线路失效时账号留在原线路并排除出选号，**绝不回落直连**（换出口在上游眼里就是特征变化）
- **本机 / 内网目标不经代理** —— loopback / 私网 / CGNAT / 容器别名一律直连
- **每号独立设备标识** —— `X-Device-Token`（每号 > 全局 > 文件兜底）+ 按 uid 稳定派生的机器/会话 ID
- **设备凭证共用检测** —— 全池共用一个凭证等于声称自己是同一台设备，审计会直接点名
- **指纹脱敏** —— 出站前剥离上游内容审核黑名单指纹（键值段 / header 段 / 模板句最小改写）
- **5xx 降档重试**（可选，默认关）—— 上游 5xx 时下沉到下一成本层继续救；因为会花更贵的额度，留给站长自己开

### 可观测

- **选号可观测** —— 均衡度指标（最高占比 / 覆盖账号数 / 偏斜倍数）+ **候选队列预览**（每个号为什么排这 / 为什么跳过）
- **选号轨迹** —— 同一会话是否换过号、换了几次，可复现查询
- **积分明细** —— 账号卡片直接展示积分包与到期进度条（走本地缓存，零额外出站）
- **健康检测** —— 快照式检测 + 时点观测失效机制，避免旧结论诱导重复操作
- **被拦样本回流**（可选）—— 被上游按内容策略拦下时，留下 system 片段 + 工具名，供扩指纹黑名单

### 请求链路

- **流式 / 非流式** —— SSE 帧按规范重建；出站强制 `stream:true`
- **统一出站改写** —— 角色归一 · `tool_choice` 归一 · `reasoning_effort` 档位降级 · 思维链回填 · 指纹脱敏（**单一入口，勿绕开**）
- **内容误拦自愈** —— 被上游内容策略拦截时同请求内换中性提示词重试一次，二次仍拦则如实返回
- **错误码映射** —— 按错误种类映射 400 / 404 / 429 / 503；**请求级错误不罚账号**

### 定时积分任务

- **四类独立排程** —— 签到（09/21 点，末尾跑连登管家）· 活跃上报（10 点）· 猫猫旅行（09/21 点）· token 保活（22 点）
- **双域适配** —— 国内版与国际版各自独立路由与任务口径
- **任务台账** —— 执行结果、积分流水、失败原因分页可查

---

## 快速开始

> ⚠️ **先确认你要跑哪个产品** —— 本仓有**两个可执行文件**，形态完全不同：

| 产品 | 端口 | 形态 | 入口 |
|---|---|---|---|
| **`webapp`（主产品）** | 8780 | 网站 + 管理后台 + OpenAI 网关，**有 Web 界面** | `./webapp -addr 127.0.0.1:8780 -db ./data/webapp.db` |
| `wb2api`（旧版） | 7863 | 纯命令行网关，**无 Web 界面**，读 `config.json` | `./wb2api -config ./config.json` |

**方式一：下载发布包（推荐，Web 版）**

1. 从 [Releases](https://github.com/sheng-du0521/workbuddy2api/releases) 下载对应平台的压缩包（附 `SHA256SUMS.txt` 可核对）；
2. 解压后双击 `start-webapp.bat`（Windows）或执行 `./webapp`；
3. 浏览器打开 `http://127.0.0.1:8780`，用**管理密钥**登录。密钥按
   **环境变量 `WB2API_ADMINKEY` → `data/admin.key` → 首次启动随机生成并落盘** 的顺序取，
   启动日志里会打印一行提示（**取值为脱敏显示**）；
4. 面板「账号管理 → 添加账号」扫码授权，账号自动入池。

**方式二：源码构建**

```bash
git clone https://github.com/sheng-du0521/workbuddy2api.git
cd workbuddy2api
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o webapp ./cmd/webapp
./webapp -addr 127.0.0.1:8780 -db ./data/webapp.db
```

> 国内构建建议 `--build-arg GOPROXY=https://goproxy.cn,direct`（官方 proxy 在大陆不可达）。
> 要求 Go **1.26+**（`go.mod` 已声明，缺工具链时 `go` 会自动下载）。

**方式三：Docker Compose（**起的是旧版命令行网关**）**

```bash
docker compose up -d        # 7863 端口，纯命令行网关
```

> ⚠️ 当前的 `docker-compose.yml` / `Dockerfile` 起的是**旧版 `wb2api`（7863，无 Web 界面）**；
> 镜像里虽然也编译了 `webapp`，但默认 `ENTRYPOINT` 不是它。要用 Web 版请走方式一或方式二。

---

## 使用指南

日常三步：**纳管账号 → 分发 API Key → 把下游指到 `/v1`**。

1. **纳管账号** —— 面板「账号管理 → 添加账号」扫码；或把桌面端导出的 auth 文件批量导入
   （导入自带防关联门禁：换 IP + 单批上限）；
2. **分发 Key** —— 面板「API 密钥」为每个下游各建一把，各自独立计费与额度；
3. **接入下游** —— 任何支持自定义 Base URL 的 OpenAI 客户端：

```bash
curl http://127.0.0.1:8780/v1/chat/completions \
  -H "Authorization: Bearer <你的 API Key>" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"你好"}]}'
```

> 「代理 → 设备与选号诊断」里有三块只读诊断：**设备凭证共用检测**、**选号均衡度**、
> **候选队列预览**（每个号为什么排这 / 为什么跳过）。
>
> 详细操作（签到任务、出口线路、积分补充、故障排查）随包分发在发布包的
> `Web应用使用说明.md` 里；本仓库只放源码，不含该文档。

---

## 接口说明

**数据面**（用面板分发的 API Key 鉴权）

| 方法 | 路径 | 说明 |
|---|---|---|
| `POST` | `/v1/chat/completions` | OpenAI 兼容对话（流式 / 非流式） |
| `GET` | `/v1/models` | 模型列表 |
| `GET` | `/api/models` | 同上（按当前用户可见范围过滤） |
| `GET` | `/healthz` | 存活探测（无鉴权；无可用账号时返回 503） |

**管理面**（用管理密钥，请求头 `X-Admin-Key`）

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/api/admin/accounts` | 账号池视图（含 `run_state` / `credits` / 冷却 / 出口） |
| `GET` | `/api/admin/egress/overview` | 出口聚合看板 |
| `POST` | `/api/admin/egress/proxies` | 线路池增删改（`GET` 列表 / `DELETE` 删除） |
| `POST` | `/api/admin/egress/assign` | 按策略批量分配线路 |
| `POST` | `/api/admin/egress/recover` | 坏线路处置（`mode` / `moved` / `kept` / `blocked`） |
| `GET` | `/api/admin/egress/accounts` | 逐账号的配置出口 vs 实际出口 |
| `POST` | `/api/admin/accounts/{uid}/device-token` | 写入该号设备凭证（空串 = 清除） |
| `POST` | `/api/admin/device-tokens/bulk` | 批量粘贴导入（每行 `账号标识 凭证`） |
| `GET` | `/api/admin/device-tokens` | 设备凭证审计（三态 + **共用检测**） |
| `GET` | `/api/admin/selection-balance` | 选号均衡度（最高占比 / 覆盖 / 偏斜倍数） |
| `GET` | `/api/admin/selection-candidates` | 候选队列预览（为什么排这 / 为什么跳过） |
| `GET` | `/api/admin/selection-trace` | 会话选号轨迹（是否中途换号） |
| `GET` | `/api/admin/fingerprint-samples` | 被拦样本（R4；默认关闭，开关在设置里） |
| `GET` | `/api/admin/settings` | 全部设置（含 `egress_on_line_down` / 两个开关等） |

> 命令行工具（`login` / `signin` / `credit` / `trial` / `activity` / `stats` / `acct`）
> 在发布包与 Docker 镜像里都随附。

---

## 安全与合规

- **管理密钥**：`WB2API_ADMINKEY` 环境变量 > `data/admin.key` > 首次启动随机生成；
  比较用固定时间实现；**日志里只打脱敏值**；
- **凭证不回显**：设备凭证只回指纹与长度；账号 token 从不出现在任何响应里；
- **面管理加固**：CSRF（Origin / Referer 与 Host 比对）· Cookie `Secure` 自适应 ·
  按**真实客户端 IP** 限流（含桶回收）· 内部错误脱敏 · 请求体限 16 MB；
- **出口隔离**：账号可单绑线路；线路失效**默认不静默换出口**；任何情况下**不回落直连**；
- **发布包校验**：随包 `SHA256SUMS.txt` 可核对；发布脚本会断言包内不含密钥 / 数据库 / 明文 key。

**合规边界**：本项目是**非官方**网关，使用你自己的 CodeBuddy 账号作为上游，
**仅限本人授权账号、本机 / 私有环境**使用。使用者需自行遵守上游服务条款。

---

## 已知限制

- **Docker 起的是旧版产品** —— `docker-compose.yml` 目前只跑 `wb2api`（7863，无 Web 界面），
  Web 版请用发布包或源码构建；
- **出口线路需自备** —— 线路池能力完整，但**池里默认没有线路**，需要你填入自己的代理；
- **设备凭证需自备** —— `X-Device-Token` 由上游 SDK 签发，必须从桌面端抓包取得后逐号录入
  （本项目**不会**伪造该值，自编值比不带更容易被判异常）；
- **部分新接口暂未接入面板** —— 选号均衡度 / 候选预览 / 设备凭证审计目前需用管理密钥直接调用，
  面板接线在后续版本；
- **请求体 / 响应体内容默认不留存** —— 只记录元数据（模型 / Token / 上游账号 / 扣费 / 会话键）。
  唯一例外是**可选开启**的「被拦样本回流」：开启后只落 **system 提示词前 300 字 + 工具名**
  （不落用户消息、不落整个请求体），用途是把指纹黑名单的更新从"发版"变成"运营"。默认关闭。
- **本仓库只放源码** —— 使用说明、发布包、调研与事件记录不在仓库内（见 `.gitignore`）。

---

## 更多文档

| 文档 | 位置 |
|---|---|
| 法律声明与边界 | [`NOTICE`](NOTICE) |
| 开源协议 | [`LICENSE`](LICENSE) |
| 配置样例 | [`config.example.json`](config.example.json) |
| Web 版使用说明 | 随发布包分发（`Web应用使用说明.md`） |
| 版本清单（供更新提醒） | [`version.json`](version.json) |

---

## 相关项目

- [**Sliverkiss/workbuddy2api**](https://github.com/Sliverkiss/workbuddy2api) —— 本项目的源头（MIT）
- [**ithtelab/workbuddy-manager**](https://github.com/ithtelab/workbuddy-manager) —— 同生态的 Web 管理端（Python + Next.js），
  出口线路池与「代理池 + 账号单绑定」的形态参考
- [**linguo2625469/workbuddy2api-panel**](https://github.com/linguo2625469/workbuddy2api-panel) —— 生态内活跃的增强分支（Go）

> 以上为独立项目，**与本仓库无代码依赖**；使用中遇到问题请到各自仓库反馈。

---

## 更新日志与反馈

- 每个版本的变更见发布包内的 `Web应用使用说明.md`（按版本倒序，含"验证"与"诚实边界"两节）
- 反馈请到 [Issues](https://github.com/sheng-du0521/workbuddy2api/issues)；提交前请**先移除日志里的密钥、Token 等敏感信息**

---

## 联系方式

- **抖音：`tgy25416`**（作者唯一联系方式；本项目**没有任何交流群 / 频道 / 付费社群**）

---

## License

[MIT](LICENSE) —— 附加「禁止商用倒卖」意愿声明，详见 [NOTICE](NOTICE)。
