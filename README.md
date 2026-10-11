<h1 align="center">WorkBuddy 3-in-1 · 三合一 AI 网关</h1>

<p align="center">
  <b>把 CodeBuddy / Trae Code / ZCode 三种账号，同时变成一套 OpenAI 兼容 API</b><br>
  单二进制 · Web 管理面板 · 多账号池轮转 · 熔断与冷却 · 定时签到与保活 · 三条上游各自独立运维
</p>

<p align="center">
  <img alt="Go" src="https://img.shields.io/badge/Go-1.22.5-00ADD8?logo=go&logoColor=white&style=flat-square">
  <img alt="Upstreams" src="https://img.shields.io/badge/Upstreams-CodeBuddy%20%7C%20Trae%20Code%20%7C%20ZCode-412991?style=flat-square">
  <img alt="API" src="https://img.shields.io/badge/API-OpenAI_Compatible-0DBD8B?style=flat-square">
  <img alt="Deploy" src="https://img.shields.io/badge/Deploy-Single_Binary%20%7C%20Docker-2496ED?style=flat-square">
  <img alt="License" src="https://img.shields.io/badge/License-MIT-yellow?style=flat-square">
</p>

---

> ### 关于上游（务必先读）
>
> 本项目是 **[linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel)** 的分支，
> 而上游的根是 **[Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api)**（原仓库已删除）。
>
> - **上游做的事**：把腾讯 CodeBuddy 账号变成 OpenAI 兼容 API 的多账号网关，附带 Web 管理面板、账号池调度、
>   熔断冷却、会话粘性、签到 / 成长 / 旅行 / 保活定时任务、成长任务一键完成等。这些能力本分支**完整保留**。
> - **本分支做的事**：在**不改动 CodeBuddy 链路任何行为**的前提下，新增 **Trae Code** 与 **ZCode** 两条上游反代，
>   三条上游共用一个 `/v1/chat/completions` 入口，各自带账号池、签到 / 额度 / 活动运维与面板视图。
> - **MIT 协议与署名**：本分支基于 MIT 协议开源，`LICENSE` 中保留了 Sliverkiss 与 linguo2625469 的原始版权声明。
>   上游两位作者的账号池调度、错误分类、提示词体系等设计原样保留，在此致谢。

> ⚠️ **本项目仅限本人授权账号、本机 / 私有环境自用**：自动签到、保活、给自己的工具提供一个 OpenAI 兼容入口。
> **禁止**批量小号分发额度、二次打包售卖、加壳捆绑卡密收费。详见 [安全与合规](#安全与合规)。

---

## 目录

- [交流与支持](#交流与支持)
- [这个分支解决了什么问题](#这个分支解决了什么问题)
- [三上游能力对照](#三上游能力对照)
- [一个入口，三条上游：路由规则](#一个入口三条上游路由规则)
- [相对上游的增量](#相对上游的增量)
- [核心能力](#核心能力)
- [快速开始](#快速开始)
- [添加账号](#添加账号)
- [Web 管理面板](#web-管理面板)
- [API 端点](#api-端点)
- [配置说明](#配置说明)
- [定时任务](#定时任务)
- [命令行工具](#命令行工具)
- [安全与合规](#安全与合规)
- [已知问题与限制](#已知问题与限制)
- [常见问题](#常见问题)

---

## 交流与支持

欢迎加入交流群，讨论本项目的使用、报错与新版本适配：

- **QQ 群号：`1092215676`**
- [点此进群](https://qm.qq.com/cgi-bin/qm/qr?k=fAo0-5AHp6tCcG6-BozfatbLPcVw2Y9H&jump_from=webapi&authKey=nvoxE003gnKiqGPM1EJnkyAsO4DO0oKBfJCofWA0EcbZnoIBNZvv9ELpi2Yj21TS)

配套 **AI 中转站网关**（Agent 调用模型的 API 服务）：<https://www.pursueray.cn/>

> 用之前建议先读 [已知问题与限制](#已知问题与限制) —— 这篇文档把三条上游各自的短板、
> 逆向方案会随客户端升级失效的地方，以及本仓库当前的工程欠缺都写清楚了。

---

## 这个分支解决了什么问题

官方的 CodeBuddy、Trae、ZCode 都**不提供 OpenAI 形态的开放 API**，各自用各自的客户端协议与鉴权方式。
本分支把这三条完全不同的链路，收敛成**同一个 OpenAI 兼容端点**：

```
                  ┌─ CodeBuddy  (copilot.tencent.com)     ─┐
OpenAI 客户端 ──► │  一个 /v1/chat/completions            │──► 分别转成三家的
(SDK / IDE / CLI) │  按模型名前缀 / X-Provider 头分派      │    真实协议与鉴权
                  ├─ Trae Code  (trae-api-cn.mchost.guru) ─┤
                  └─ ZCode      (zcode.z.ai)             ─┘
```

**用一句话概括**：一条出口，三个账号池；谁有额度用谁，谁被限流换谁，登录、签到、保活、额度刷新全在面板上点。

## 三上游能力对照

| | **CodeBuddy**（上游原有） | **Trae Code**（本分支新增） | **ZCode**（本分支新增） |
|---|---|---|---|
| 上游主机 | `copilot.tencent.com`<br>`www.codebuddy.cn` | 对话 `trae-api-cn.mchost.guru`<br>业务 `api.trae.cn` | `zcode.z.ai`<br>回落 `open.bigmodel.cn` / `api.z.ai` |
| 对话协议 | 私有 HTTP + SSE | 私有 HTTP + SSE（`/api/ide/v1/chat`）<br>内置模型另走 SOLO 链（`/api/agent/v3/llm_utils_chat`） | **Anthropic Messages**<br>（`/api/v1/zcode-plan/anthropic/v1/messages`） |
| 鉴权 | OAuth 设备码 + token 刷新 | `Cloud-IDE-JWT` + ECDSA 签名的 refresh 换票 | JWT **+ 客户端请求签名**（Ed25519 + PoW） |
| 登录方式 | 面板 / `login.sh` | 面板一键 → 浏览器授权 → **常驻回环中继**自动接回调 | 面板一键 → CLI OAuth 设备流 |
| 额度单位 | 积分 credits | 积分 credits | **token**（与前两者不同口径，面板分开统计、不求和） |
| 签到 | ✅ 09 / 21 点（含连登兑换、抽奖） | ✅ 09 / 21 点 | — |
| 活动 / 领取 | ✅ 成长任务一键完成 17/18、开学季 5/5 | ✅ 活动巡检（裂变 / 权益领取） | ✅ 可领方案巡检 + 一键领取 |
| 保活 | ✅ 22 点 token 保活 | ✅ 22 点（pay_status + GetUserInfo） | ✅ 余额巡检 |
| 内置模型目录 | 实时拉取 + 内置表 | **35 条**（31 条客户端快照 + 4 条上游老平台） | **32 条**（可调白名单） |
| 模型 ID | `cn:glm-5.2` / `global:hy3` | `trae:<model>` | `zcode:<model>` |

## 一个入口，三条上游：路由规则

请求进来后，网关按下面的优先级判定走哪条上游（`internal/server/resolve_provider.go`）：

| 优先级 | 判据 | 示例 |
|---|---|---|
| 1 | 模型名带 provider 前缀（`/` 或 `:` 都认） | `trae:glm-5.3`、`zcode/glm-5.3-flash`、`workbuddy:cn:glm-5.2` |
| 2 | 请求头 `X-Provider` | `X-Provider: trae` |
| 3 | 兜底 | **CodeBuddy**（存量调用零回归） |

provider 别名：`zcode` / `zai` / `bigmodel` → ZCode；`trae` / `traecode` → Trae；`workbuddy` → CodeBuddy。

> **为什么不做「按模型名猜上游」**：CodeBuddy 链路本身就在用 `glm-5.2` / `glm-5.3` 这类模型名，
> 一旦按 `glm*` 猜成 ZCode，存量真实流量会被整批劫持到另一条上游。所以本分支**只认显式声明**，
> 宁可多打一个前缀，也不做有歧义的猜测（`resolve_provider.go` 内有完整注释与实测后果）。

`GET /v1/models` 会把三条上游的模型**合并成一份目录**，每条都带好前缀（`trae:` / `zcode:` / `cn:` `global:`），
客户端拿到什么 id 就直接填什么，不用记「哪种上游要用哪种前缀」。

## 相对上游的增量

### Trae Code 反代（`internal/trae/`）

| 能力 | 实现要点 |
|---|---|
| **对话** | `POST /api/ide/v1/chat`（SSE）。`buildTraePayload` 把 OpenAI messages 压成上游的 `chat_history` + `user_input` + `intent_name=general_qa_intent`；`stream` 恒 true |
| **内置模型（DLL）链** | 另一条独立链路 `/api/agent/v3/llm_utils_chat`，独立头矩阵（`X-Uid` / `X-Ide-Token` / `X-Ide-Version` / `X-Ide-Version-Code` / `Request-Traffic-Type` 等），缺头即 400 / 401 |
| **模型目录** | 内置 **35 条**（客户端 `state.vscdb` 的 7 个 section 合并 + 4 条上游仍认的老平台模型）；实时可用集来自 `model_list?type=chat` 与 `get_detail_param`，含倍率、折扣率、上下文、最大输出、多模态、工具能力、思考档位 |
| **工具调用** | 上游会**静默忽略** `tools` 字段，因此改为「提示词注入 + `<\|FunctionCallBegin\|>…<\|FunctionCallEnd\|>` 内联标记」，响应侧用跨帧 hold-back 状态机解析回标准 OpenAI `tool_calls` 增量帧 |
| **流式解析** | `event: output` → `data.response`（正文）/ `data.reasoning_content`（思考链）；`event: token_usage` → usage；`progress_notice` / `metadata` / `prompt` 控制帧丢弃（否则会把 `Building prompt: …` 当成正文吐给用户） |
| **排队自动换号** | 预读首段响应，命中 `queue` / `waiting` / `排队` 等字样立即换号重试 |
| **积分折算** | Trae 响应**不含积分字段**，按实测双系数 `a·prompt + b·completion` 估算，写入池与用量账本（面板显示 `≈`） |
| **token 刷新** | `POST /trae/api/v3/oauth/ExchangeToken`，请求体走 **ECDSA-P256 签名**（`DeviceProof`），过期时间为毫秒；请求前按 10 分钟 margin 预刷新并落盘 |
| **错误映射** | `model is unknown` → 404 `model_not_found`；`not entitled` → 403 `model_not_entitled`；其余 → 503 换号 |
| **登录** | 授权码 + PKCE。回调**被授权页硬校验为 `127.0.0.1:<port>/authorize`**（公网地址必被拒），本分支提供**常驻回环中继**（固定端口 `17654`，PowerShell / python3 两套脚本 + 一键安装），登录时把回调端口钉到中继上，浏览器跳回本机即可被自动接住并转交网关 |

### ZCode 反代（`internal/zcode/`）

| 能力 | 实现要点 |
|---|---|
| **对话** | 走 **Anthropic Messages** 协议：`POST https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages`；`convert_zcode.go` 负责 OpenAI ⇄ Anthropic 双向转换（system 抽取、`tool` → `tool_result`、`tool_calls` → `tool_use`、`stop` → `stop_sequences`…），对外仍是 OpenAI 响应 |
| **风控回落** | 命中 `3012`（非官方客户端判定）时，同一凭据改打 `open.bigmodel.cn` / `api.z.ai` 的 `/api/anthropic/v1/messages` |
| **签名凭据** | 上游要求客户端请求签名。凭据形如 `<32 位 hex keyId>.<secret>`：先用 HKDF + HMAC 握手（`POST /api/paas/c1f3a7e2/v2/client`）换回 **AES-256-GCM 包裹的 Ed25519 私钥**，再对 canonical 串 `<keyId>\n<ts>\n<版本>\n<sessionId>\n<nonce>` 签名，附 `X-Client-Ts` / `X-Client-Sig` / `X-Session-Id` / `X-Client-Nonce` / `X-App-Id` / `X-Client-Pow` 等头 |
| **凭据来源** | 自动发现本机 ZCode 客户端配置：`~/.zcode/v2/config.json`、`%APPDATA%\ZCode\config.json`、两份 `credentials.json`（支持 `enc:v1:` AES-256-GCM 解密）；也支持在面板**粘贴一次、池里所有 ZCode 号一起补满** |
| **自动刷新** | 每 5 分钟一轮：已有凭据则覆盖（secret 轮换）、缺失则补齐、**客户端上有而池里没有的账号自动建号** |
| **模型目录** | 内置 **32 条**可调模型（GLM-5.3 / 5.2 / 5 / 4.7 / 4.6 / 视觉系列…），叠加公共目录与账号自身权益（billing capabilities） |
| **思考档位** | 请求侧支持原生 `thinking`（enabled / disabled）与档位载体 `output_config.effort`（none / low / medium / high / xhigh）；面板区分「档位可调」「只有开关」「思考常开」三种形态 |
| **额度与积分** | 三层语义：① `monitor/usage/quota/limit` 拿真积分 → ② 「当前用户不存在 coding plan」按无套餐处理（非错误）→ ③ 回落 billing 权益桶。逐次积分按「当日真值 ÷ 当日经网关 token」折算，未标定回落 `features.zcode_credit_per_1k` |
| **方案领取** | `billing/preview` 列出可领方案 → `billing/claim` 领取；领取需要阿里云无痕验证参数，配合内置验证码求解器可自动完成 |

### 公共增量（三上游共用）

| 能力 | 实现要点 |
|---|---|
| **provider 隔离的账号池** | 选号、加权随机、余额分层**以及兜底路径**全部按 `provider` 硬过滤，三条上游的账号不会互相串号（`internal/pool/`） |
| **独立后台排程** | 新增 `upstream_schedule` 段，Trae / ZCode 的签到、活动巡检、保活、额度刷新与 CodeBuddy 的 `schedule` 段**完全分开**，互不干扰 |
| **面板三上游维度** | 账号卡按 provider 分组、额度分别统计（ZCode 的 token 量级单独走缩写渲染，不把积分图压成一条线）、模型目录按上游切换、请求日志可按上游筛选 |
| **验证码求解器** | `captcha_node/`（阿里云无痕验证，Chrome 求解）+ `internal/captcha` 装配；**默认关闭**，不部署时领取接口原样透出上游的验证码报错，零副作用 |
| **可配的换号上限** | `upstream.max_attempts`（默认 6，上限 32）—— 池里有一批僵尸号时可压到 2~3，号多时可提到 8~10 |
| **新增 CLI 工具** | `zcmint` / `zcsync` / `zctok` / `zdump` / `zcprobe` / `traeprobe` / `trefresh`，见[命令行工具](#命令行工具) |

## 核心能力

以下能力继承自上游（CodeBuddy 链路），本分支完整保留：

| 能力 | 说明 |
|---|---|
| 🔑 **OAuth 一键登录** | 面板「添加账号」按钮完成设备授权 → 凭证落盘 → **热加载进池（免重启）** |
| 🔄 **多账号池** | 最早到期优先 + 成本分层 + 加权随机选号，Top-5 候选 + 防惊群 |
| 🛡️ **熔断与冷却** | 429 软冷却 600s 起指数退避、404 固定 60s、402 硬冷却至次日 04:00、连败熔断、在途租约限流 |
| 🧲 **会话粘性** | 同一会话尽量绑定同一账号，TTL 滚动续期，失败自动解绑；可用 Upstash Redis 镜像防重启丢失 |
| ⏰ **定时任务** | 签到、成长任务、猫猫旅行、活跃上报、token 保活、夜猫子，各自独立开关（见[定时任务](#定时任务)） |
| ⚡ **流式 + 非流式** | 出站强制 `stream:true`；SSE 帧按规范重建；非流式由网关聚合为单响应 |
| 🧠 **推理模型兼容** | DeepSeek 思维链注入、`reasoning_content` 回填、effort 档位自动降级 |
| 💬 **系统提示词体系** | 网关自有提示词替换客户端 system，从源头消灭 system 来源的内容误报；遇拦截自动降级重试 |
| 🗑️ **指纹脱敏** | 出站请求体黑名单指纹字段清洗（可关） |
| 📊 **可观测** | 每请求一行表格日志（TTFB / token 速率 / 账号 / 上游）；`/healthz` 带身份标识可接探活 |
| 💾 **状态持久化** | 池状态本地原子落盘 + Upstash Redis 异步镜像（可选） |
| 🖥️ **Web 面板** | 内嵌单页（明暗主题）：账号运维 / 用量 / 套餐 / 任务中心 / 模型 / 配置热改 / 日志 |
| 🎯 **成长任务一键完成** | 官方成长计划 18 个任务中 **17 个可纯 API 一键完成**（含自动领奖），无需安装官方客户端 |

## 快速开始

### 环境要求

- **Docker + Docker Compose**（推荐），或 **Windows / macOS / Linux 直接跑单文件二进制**
- 一个或多个已注册的账号（CodeBuddy / Trae / ZCode 任选，可混用）
- 宿主机 Go ≥ 1.22（仅从源码构建时需要）

### 方式一：Docker Compose（推荐服务器部署）

```bash
# 1. 克隆
git clone https://github.com/jjjane233/workbuddy-3in1.git
cd workbuddy-3in1

# 2. 准备配置（compose 挂载此文件，缺失会导致容器启动失败）
cp config.example.json config.json
#    建议编辑 config.json 设置 api_key（或留空由程序自动生成随机密钥）

# 3. 启动（首次会构建镜像，约 1-2 分钟）
docker compose up -d --build

# 4. 健康检查（无可用账号时返回 503）
curl -s http://localhost:7863/healthz
# {"healthy":0,"total":0,"service":"workbuddy2api"}
```

启动后打开 **`http://localhost:7863/panel/`**，用面板「添加账号」完成登录（见下节）。

常用运维命令：

```bash
docker compose logs -f          # 跟踪日志
docker compose restart          # 重启
docker compose down             # 停止并移除容器（数据在 ./auths 与 ./data，不受影响）
```

### 方式二：Windows 单文件运行（无需 Docker）

```powershell
# 1) 从源码构建
go build -trimpath -ldflags="-s -w" -o wb2api.exe ./cmd/server

# 2) 直接运行：首次启动自动生成 config.json（含随机 api_key，日志打印一次）
.\wb2api.exe -config config.json

# 3) 浏览器打开面板添加账号
#    http://127.0.0.1:7863/panel/
```

exe 为**单文件自包含**（前端资源已 embed 进二进制），拷到任意 Windows 机器即可运行，
只需保证 `auths/`（凭证）与 `data/`（状态）目录可写。

### 方式三：源码运行（开发调试）

```bash
go build ./...
go vet ./...
go test ./...                      # 完整测试套件
go run ./cmd/server -config config.json
```

构建常用二进制：

```bash
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o wb2api ./cmd/server
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o signin_bin ./cmd/signin
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o login ./cmd/login
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o credit ./cmd/credit
```

## 添加账号

面板右上角「**添加账号**」，先在「上游」下拉里选平台 —— 三条链路的登录方式不同：

### CodeBuddy

选择上游 = CodeBuddy，点「开始登录」→ 浏览器完成设备授权 → 面板自动检测并落盘凭证 → **热加载进池（无需重启）**，
顺带完成首次签到。Linux / macOS 也可用命令行：`./login.sh`。

### Trae Code

Trae 授权页**硬校验回调地址必须是 `http://127.0.0.1:<port>/authorize`**，网关部署在公网时浏览器跳回本机没人接，
授权码就丢了。本分支的解法是**常驻回环中继**：

1. 面板「添加账号」→ 上游选 Trae，展开「常驻中继」区，复制面板给出的一行安装命令
   （Windows 用 PowerShell、Linux / macOS 用 python3），在你**自己的电脑上**执行一次 —— 之后它会开机自启；
2. 中继在线后（面板显示 `常驻中继在线（端口 17654）`）再点开始登录：授权页跳回的
   `127.0.0.1:17654/authorize?authCodeInfo=…` 会被中继接住并原样转交网关；
3. 没装中继也行：登录后把浏览器地址栏整行 URL 粘进面板的「手工提交回调」框，效果相同。

> 中继只做一件事：把 `127.0.0.1` 上的那一跳转发给网关。它用面板的 `api_key` 做心跳鉴权，
> **生成的脚本文件里包含你的面板 api_key**，请勿把安装命令或脚本外发。

### ZCode

选择上游 = ZCode，点「开始登录」走 CLI OAuth 设备流（可选 `zai` / `bigmodel` 两个 provider），
授权完成后面板自动落盘 token 并尝试换取签名凭据。

ZCode 的对话链路还需要**客户端签名凭据**（形如 `<keyId>.<secret>`）：

- **网关和 ZCode 客户端同一台机器**：面板会自动从 `~/.zcode/v2/config.json` 等位置发现并写好，无需操作；
- **纯服务器部署**：点面板的「**补一次凭据**」，把 ZCode 客户端 `config.json` 里的 `apiKey`
  （或整份 `config.json`，或直接 `<keyId>.<secret>`）粘进去 —— 一次写入会**给池里所有 ZCode 号补齐**；
- 之后每 5 分钟自动同步一轮，客户端上新增的账号会自动进池。

## Web 管理面板

内嵌单页（前端资源 go:embed 进二进制，零外部依赖），打开 `http://127.0.0.1:7863/panel/`：

| 视图 | 内容 |
|---|---|
| **账号** | 按上游分组的账号池：健康色条、额度量条、冷却倒计时；单号运维（签到 / 余额 / 保活 / 活动 / 领取）、批量操作、凭据写入；添加上游账号 |
| **用量** | 请求指标（完成成功率 / HTTP 成功率 / 平均耗时）、上游维度与账号维度对比、token 速率、积分与 token 分开统计 |
| **套餐** | 三条上游各自的权益包明细与到期时间；ZCode 可领方案列表 + 一键领取 |
| **任务中心** | 全账号任务扫描、执行队列（账号内串行、账号间可选并发）、开学季状态矩阵 |
| **模型** | 三上游合并目录，带倍率 / 档位 / 上下文 / 工具能力 / 思考形态；支持按上游切换与筛选 |
| **配置** | 在线改 `config.json` 并热生效（api_key / 冷却参数 / 池参数 / 排程 / 脱敏开关…），装配期字段提示需重启 |
| **运行日志** | 按「任务 / 对话 / 系统」分频道筛选，按 IP / UA / 模型 / 账号 / 请求 ID 检索 |

请求记录表带**调用来源**（客户端 IP / User-Agent，可用 `logging.request_client_info` 关闭）；
JSONL 归档只写请求元数据，**不写提示词、响应正文或凭证**。

## API 端点

| 端点 | 方法 | 说明 |
|---|---|---|
| `/v1/chat/completions` | POST | OpenAI 兼容对话，流式 / 非流式 |
| `/v1/models` | GET | 三上游合并模型目录（带 provider / realm 前缀与倍率、档位等字段） |
| `/status` | GET | 账号池汇总 + 每账号详情（含 provider、`disabled_reason`） |
| `/healthz` | GET | 探活，返回 `{"healthy":n,"total":m,"service":"workbuddy2api"}`，无可用账号返回 503 |
| `/panel/` | GET | Web 管理面板 |

```bash
# 模型列表
curl -s http://127.0.0.1:7863/v1/models -H "Authorization: Bearer $KEY"

# 走 CodeBuddy（无前缀时的兜底）
curl -s http://127.0.0.1:7863/v1/chat/completions \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{"model":"cn:glm-5.2","messages":[{"role":"user","content":"你好"}],"stream":true}'

# 走 Trae
curl -s http://127.0.0.1:7863/v1/chat/completions \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{"model":"trae:glm-5.3-flash","messages":[{"role":"user","content":"你好"}]}'

# 走 ZCode（等价写法：加 X-Provider: zcode）
curl -s http://127.0.0.1:7863/v1/chat/completions \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{"model":"zcode:glm-5.3","messages":[{"role":"user","content":"你好"}]}'
```

## 配置说明

完整样例见 [`config.example.json`](config.example.json)。三合一部分的重点字段：

### `upstream_schedule`（Trae / ZCode 独立排程）

| 字段 | 默认 | 说明 |
|---|---|---|
| `enabled` | `true` | 总开关；缺省开启（部署里没有 Trae / ZCode 账号时等于空转） |
| `trae_checkin_hours` | `[9,21]` | Trae 签到整点 |
| `trae_activity_hour` / `trae_activity_minutes` | `10` / `30` | Trae 活动定时全量巡检 / 滚动巡检周期 |
| `trae_keepalive_hour` | `22` | Trae 保活整点 |
| `trae_usage_minutes` | `30` | Trae 额度刷新周期 |
| `zcode_claim_hours` | `[3]` | ZCode 可领方案领取整点 |
| `zcode_preview_minutes` | `30` | 可领方案巡检周期 |
| `zcode_reward_minutes` | `60` | 奖励总览巡检周期 |
| `zcode_balance_minutes` | `5` | ZCode 余额刷新周期 |
| `tick_minutes` | `1` | 调度器 tick |

### 三合一部分的其他字段

| 字段 | 默认 | 说明 |
|---|---|---|
| `panel.public_base_url` | `""` | 面板对外可达基址（公网部署时填，用于生成授权回跳地址） |
| `panel.trae_relay_port` | `17654` | Trae 常驻回环中继端口（授权页只认 `127.0.0.1:<port>`） |
| `upstream.max_attempts` | `6` | 单请求最多换号次数（含第一次），上限钳 32 |
| `features.zcode_credit_per_1k` | `0.1` | ZCode 逐次积分的兜底单价（credits / 1K token），未标定时按它折算并标 `≈` |
| `captcha.enabled` | `false` | 是否装配验证码求解器（阿里云无痕验证，需要 `node` + Chrome） |
| `captcha.cmd_prefix` | `[]` | `node` 之前的命令前缀，无显示环境用 `xvfb-run` |
| `captcha.home` | `""` | 解算子进程的 HOME，留空自动用 state 同级的 `captcha-home`（必须可写） |

继承自上游的字段（`listen` / `api_key` / `schedule` / `pool` / `session_sticky` / `upstash` / `prompt` /
`upstream` 超时与出站身份等）语义不变，逐项注释见 [`cmd/server/config.go`](cmd/server/config.go)。

## 定时任务

| 任务 | 开关 | 时间 | 说明 |
|---|---|---|---|
| CodeBuddy 签到 | `schedule.checkin_enabled` | `[9,21]` | 含连登兑换（7 / 14 / 28 天档位）+ 抽奖次数自动抽完 |
| CodeBuddy 成长任务 | `schedule.growth_enabled` | `[1]` | 每日零点解锁的 Sequential 族任务自动扫描执行 |
| 猫猫旅行 | `schedule.travel_enabled` | `[9,21]` | 独立排程 |
| 活跃上报 | `schedule.activity_enabled` | `[10]` | 点亮连登 / 解锁领养 |
| token 保活 | `schedule.keepalive_enabled` | `[22]` | 保活 CodeBuddy token |
| 夜猫子 | `schedule.blackcat_enabled` | `[23]` | 先查任务进度再决定是否上报 |
| 余额刷新 | `schedule.balance_refresh_enabled` | 每 5 分钟 | 冷却账号余额恢复后自动解冻 |
| **Trae 签到 / 活动 / 保活 / 额度** | `upstream_schedule.enabled` | 见上表 | Trae 独立一条排程 |
| **ZCode 领方案 / 奖励 / 余额** | `upstream_schedule.enabled` | 见上表 | ZCode 独立一条排程 |

## 命令行工具

服务端以 `cmd/server` 为主，其余是运维 / 诊断用的小工具：

| 工具 | 用途 |
|---|---|
| `cmd/server` | 网关主服务（面板 + OpenAI 兼容 API） |
| `cmd/login` / `cmd/signin` / `cmd/credit` / `cmd/trial` | CodeBuddy：设备码登录 / 签到 / 查积分 / 试用探测 |
| `cmd/traeprobe` | Trae：对 auth 目录内每个 Trae 账号实测权益领取、裂变、活动与额度接口 |
| `cmd/trefresh` | Trae：Ping + 刷新 token + 拉模型列表（排查 token / 目录问题） |
| `cmd/zcmint` | ZCode：无客户端场景自助换钥（打印授权 URL → 轮询 → 换取凭据 → 写文件或推送网关） |
| `cmd/zcsync` | ZCode：把本机凭据单向推送到远端网关（支持 `-once` / `-interval` / `-dry-run`） |
| `cmd/zctok` | ZCode：打印本机凭据来源与预览 |
| `cmd/zdump` | ZCode：遍历客户端配置文件逐键打印（含 `enc:v1:` 解密） |
| `cmd/zcprobe` | ZCode：跑一遍凭据发现 + 用 `glm-5.3-flash` 校验凭据是否可用 |

## 安全与合规

### 凭据管理（`auths/`）

`auths/*.json` 中保存的是**明文 accessToken / refreshToken / 签名凭据**：

- 目录权限应为 `0700`、文件 `0600`（网关自身落盘即按此权限）；
- **绝不要**把 `auths/`、`data/`、`config.json` 提交到版本库或打进镜像 —— 本仓库 `.gitignore` 已默认忽略；
- 凭证等同于账号，泄露即等于把账号交出去。

### 网络暴露

- 默认监听 `:7863`，compose 暴露 `0.0.0.0:7863`，**无内置 TLS**；公网部署**必须**设置 `api_key`，建议前置反代 / 内网；
- 面板与 API 共用同一个 `api_key`；常量时间密钥比较、CSP 与安全响应头、UID 白名单防路径穿越已内置；
- Trae 回环中继的安装脚本**包含面板 api_key**（中继用它做心跳鉴权），不要外发脚本或安装命令；轮换 `api_key` 后需重装中继。

### 授权使用边界（重要）

1. 本项目是**非官方**网关，使用你自己**已授权账号**的凭证访问上游，仅限本人自用与私有环境测试；
2. 请遵守 CodeBuddy / Trae / 智谱各自的服务条款，**不要**批量注册账号、不要转售接口配额、不要对外提供付费中转；
3. 一切商用 / 售卖 / 加壳捆绑行为与本项目及作者无关，行为人自行承担后果（账号封禁、条款违约与法律风险）；
4. 本项目按「原样」提供，不承诺任何上游接口的长期可用性 —— 上游改协议就需要改代码。

### 内容与平台风控

- 出站请求体指纹脱敏、系统提示词替换是**减少误报**的手段，不是绕过平台政策的手段，请勿用于违规内容；
- 定时签到 / 保活 / 活动领取会**真实访问上游接口**，请自行评估账号风险。

## 已知问题与限制

这一节把本项目**当前真实存在的问题**写清楚 —— 包括逆向方案固有的脆弱点、三条上游各自的短板、
以及本仓库工程上的欠缺。用之前请先看一遍，别把它当成一个"开箱即用、长期稳定"的产品。

### 1. 与上游的版本差（最需要知道的一条）

- 本分支 = **上游某个快照 + 我们的三合一改造**，不是"跟随上游最新"。上游此后新增的
  `internal/pool/modelview.go`（模型维度视图）、`internal/upstream/dsml.go`（DSML 原生工具调用修复）
  以及若干企业版 / 超时测试，**没有包含在本仓库里**。
  影响面：CodeBuddy 链路缺这两项后期修复；三合一（Trae / ZCode）功能不受影响。
- 本仓库**不带上游的 git 历史**（只有一个初始 commit），没法用 `git log` 追溯上游演进；
  要对比请直接 clone [上游仓库](https://github.com/linguo2625469/workbuddy2api-panel)。
- 想把上游后续改动合并进来需要手工处理冲突：Go 模块路径已从
  `github.com/linguo2625469/workbuddy2api-panel` 改名为 `github.com/jjjane233/workbuddy-3in1`。

### 2. 逆向工程的本质风险（三条上游都有）

- 所有上游接口都是**逆向所得，非官方公开 API**：客户端升级、字段改名、加风控，都可能让某条链路
  **立刻失效**。本项目没有 SLA、不承诺兼容期，上游改协议就得改代码。
- **硬编码的客户端身份会过期**：
  - Trae：`AppID`、`DefaultIDEVersion = 3.3.96`、SOLO 链 `X-Ide-Version = 0.1.52` /
    `X-Ide-Version-Code = 20260811` / `X-Device-Brand = 83DG` / `x-os-version = 10.0.19045`；
  - ZCode：`DefaultAppVersion = 3.14.3`、签名握手路径 `/api/paas/c1f3a7e2/v2/client`、
    KDF salt 与 `X-App-Id`。
  官方客户端一发新版，这些值就可能不再被接受，需要重新抓包校准。
- **内置模型目录会过时**：Trae 那 35 条来自客户端 `state.vscdb` 的某个快照，ZCode 那 32 条是白名单，
  新模型 / 下线模型都要重新对齐。另外 `internal/trae/catalog.go` 注释写的快照版本（Trae CN 1.107.1）
  与 `trae.go` 里的出站版本（3.3.96）**口径不一致**，注释里的"31 条"也没跟着改成 35 条。

### 3. Trae 链路的具体短板

| 问题 | 说明 |
|---|---|
| **多账号共用同一设备指纹** | `x-app-id` / `x-device-brand` / `x-device-type` / `x-os-version` 是全局常量，只有 `x-device-id` 按账号取值（缺省回落 UID）。同池多号在风控侧可能被关联 |
| **积分是估算值，不是真值** | Trae 响应里**没有**积分字段，网关按 `a·prompt + b·completion` 双系数折算（面板标 `≈`）。系数表按模型档位维护，新模型落到兜底档位时会有偏差 |
| **流式没有 usage，思考链被丢弃** | SSE 收流没有 usage 帧，流式用量只能靠本地估算；`reasoning_content` **只在非流式响应**里保留（流式无标准槽位） |
| **工具调用不是原生 tools** | 上游会静默忽略 `tools`，只能用"提示词注入 + `<\|FunctionCallBegin\|>…<\|FunctionCallEnd\|>` 内联标记 + 跨帧状态机回译"。模型不按约定输出就丢工具调用；官方改提示词模板也会导致漂移 |
| **排队换号靠关键字匹配** | 预读首段命中 `queue` / `waiting` / `排队` 等字样才换号。上游改文案即失效（不会报错，只会继续等） |
| **登录必须自备回环中继** | 授权页硬校验 `127.0.0.1:<port>/authorize`，服务器部署无法直接登录。中继脚本**内嵌面板 api_key**、需要开机自启，轮换 key 后必须重装，且脚本不可外发 |
| **历史别名行为与字面不符** | `trae-auto` / `trae-solo` / `trae-agent` 等别名仍走 `/api/ide/v1/chat` 链，仅为兼容保留 |
| **`llm_raw_chat` 未使用** | 该端点只出现在常量与模型列表参数里，对话实际只走 `/api/ide/v1/chat` 与 SOLO 链 |

### 4. ZCode 链路的具体短板

| 问题 | 说明 |
|---|---|
| **签名凭据是硬门槛** | 对话必须带客户端签名（Ed25519 + PoW）。纯服务器部署要人工从客户端 `config.json` 粘一次 `<keyId>.<secret>`；客户端轮换 secret 后靠 5 分钟同步兜底，**窗口期内会 3012** |
| **PoW 难度实际写死** | 注释说难度取上游握手下发的 `powBits`，但 `Signer.powBits` 只有声明与读取、**没有任何赋值路径**，实际恒用 `signPoWBits = 8`。上游若提高难度，PoW 会算不对 |
| **思考链不输出** | 请求侧支持 `thinking` 与 `output_config.effort`，但响应侧 `thinking_delta` 被忽略，网关**不产出 `reasoning_content`** |
| **领取依赖验证码求解器** | ZCode 方案领取需要阿里云无痕验证；`captcha.enabled=false`（默认）时接口会把上游的验证码失败原样返回。开启求解器要真实 Chrome，服务器上还得 `xvfb-run`，CPU / 内存开销不小 |
| **口径与残留未清理** | `internal/captcha` 取配置用 `platform=win32`，ZCode 包内统一用 `win32-x64`；三个端点常量（`/api/v1/agent/configs`、`/zcode-api/incentive/referral/bind-code`、`/api/biz/subscription/list`）定义了但当前未调用 |
| **模型 ID 大小写不统一** | `GLM-5.3` 与 `glm-4.7` 混用（内部靠 `strings.ToLower` 索引兜住），调用方拼错大小写可能匹配不上 |

### 5. 工程与部署上的欠缺

- **只支持单实例部署**：账号池状态在内存 + 本地文件（`data/state.json`）。会话粘性可以镜像到 Upstash Redis，
  但选号与冷却不是分布式锁语义 —— **多副本同时跑会各选各的号，额度与在途限制形同虚设**。
- **无内置 TLS，面板与 API 共用一个 `api_key`**：公网必须自己加反代；key 一泄露，面板和接口一起暴露。
- **仓库内不再提供探测 / 压测脚本**：按发布要求已移除 `scripts/`（`probe_active.py`、`probe_max_tokens.py`、
  `task_*.py`）。面板的「模型输出上限」视图仍能读 `data/output_probes.json`，但**仓库不提供生成该文件的脚本**。
- **代码注释里有悬空引用**：多处指向 `docs/*.md`（如 `docs/TRAECODE_BUILTIN_MODELS_OPENAI_PROXY.md`、
  `docs/USAGE.md`、`docs/ZCODE_CREDENTIAL_AND_ACTIVITY.md`），这些设计文档不在仓库里。
- **面板 JS 测试在没装 node 的机器上会跳过**（`t.Skip`）：少一层前端校验，但不会报失败。
- **`.gitignore` 忽略除 `README.md` 外的所有 `*.md`**：想往仓库里加文档需要先改这条规则。
- **CI 的两个坑**：① GHCR 镜像首次发布后要**手动**把 Package 设为 Public，否则别人拉取需要 `docker login`；
  ② Release 打包依赖 `cmd/server/main.go` 里的 `appVersion`（当前 `1.11.11-panel`），打 tag 必须与之一致。
- **UI 与文案是中文单语**：面板没有 i18n，术语（上游 / 池 / 冷号）也没有多语言版本。

### 6. 合规与滥用风险

- 上游协议逆向、定时签到 / 保活 / 活动领取、验证码求解器，都可能触碰 CodeBuddy / Trae / 智谱的服务条款；
  仓库是公开的，这些能力任何人都能看到与复用。
- 作者不对可用性、稳定性、账号安全做任何承诺，也不为他人基于本项目的使用行为背书。请在自用范围内使用。

> 上面这些不是"以后会修"的承诺清单，而是**当前状态**的描述。欢迎在群里报问题或提 PR。

## 常见问题

<details>
<summary><b>ZCode 请求返回 405 / code 3012 怎么办？</b></summary>

这是上游对「非官方客户端」的判定。网关会先自动回落到 `open.bigmodel.cn` / `api.z.ai` 的 Anthropic 端点；
如果仍然 3012，通常是**签名凭据没写进去**：打开面板 → 账号 → ZCode 账号 → 「补一次凭据」，
粘贴客户端 `config.json` 里的 `apiKey`（`<keyId>.<secret>`），写入后网关会回读校验。
</details>

<details>
<summary><b>Trae 登录时浏览器报「ERR_CONNECTION_REFUSED」或提示回调地址非法？</b></summary>

Trae 授权页只接受 `http://127.0.0.1:<port>/authorize` 形式的回调，网关在公网时本机没有监听。
两种解法：① 按面板提示在你自己的电脑上安装一次**常驻回环中继**；② 登录后把浏览器地址栏整行 URL
粘进面板的「手工提交回调」框。
</details>

<details>
<summary><b>Trae 内置（DLL）模型不出现在 /v1/models 里？</b></summary>

DLL 段来自 `get_detail_param`，只有当前账号真实可用时才返回；账号没登录、token 过期或该账号无此权益时，
目录里只展示客户端内置目录（标记 `available=false`）。先确认账号健康度，再刷新模型目录。
</details>

<details>
<summary><b>面板里 ZCode 的额度是「token」，和 CodeBuddy / Trae 的「积分」不一样？</b></summary>

是的，两者口径不同：ZCode 的额度单位是 token，CodeBuddy / Trae 是 credits。
面板**分开统计、不做求和**（ZCode 走 M / B 缩写渲染），避免把积分图压成一条线或出现「0 / 0」的假象。
</details>

<details>
<summary><b>Docker 部署登录后报「写入 auths/…json.tmp 失败： permission denied」？</b></summary>

容器默认以 uid 10001 运行，宿主机挂载目录属主不同导致。三种解法任选：

```bash
# 方案 1（推荐，非 root）：让容器以你自己的 uid 运行
PUID=$(id -u) PGID=$(id -g) docker compose up -d
# 或写进 .env（已被 .gitignore 忽略）：echo "PUID=1000" > .env && echo "PGID=1000" >> .env

# 方案 2：把挂载目录属主交给容器默认用户
sudo chown -R 10001:10001 ./auths ./data ./config.json

# 方案 3：NAS / 群晖不便 chown 时，在 compose 里设 user: "0:0"
```
</details>

<details>
<summary><b>账号被自动 Disable 了怎么恢复？</b></summary>

Disable 通常来自明确的账号级判据（授权失效、硬限流、余额耗尽等）。面板账号卡会显示 `disabled_reason`；
重新登录该账号，或在确认账号已恢复后手动启用（之后会重新参与选号）。
</details>

## 致谢

- [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) —— 项目最初的作者：账号池调度、
  错误分类、提示词体系、任务链路的原始设计来源；
- [linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel) —— 本分支的直接上游：
  Web 管理面板、成长任务一键完成等能力的来源；
- 本分支新增的 Trae Code / ZCode 反代（`internal/trae`、`internal/zcode`、三合一请求分发与面板三上游运维）
  由本仓库维护。

## 免责声明

本项目为**非官方**第三方工具，与腾讯 CodeBuddy、字节跳动 Trae、智谱 ZCode 及其关联公司**无任何隶属或合作关系**。
项目按「原样」提供，不附带任何明示或暗示的担保；使用本项目产生的一切后果（包括账号封禁、服务条款违约、
数据或财产损失）由使用者自行承担。请仅在你拥有合法授权的账号与合规场景下使用。

## License

[MIT](LICENSE) © 2026 Sliverkiss · linguo2625469 · jjjane233
