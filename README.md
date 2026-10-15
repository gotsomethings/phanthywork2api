# phanthycode2api

> 将 PhanthyCode CLI 的 Anthropic Messages API 转换为 OpenAI 兼容 API，Go 实现。

本项目基于 [lwjlwjlwjlwj/phanthycode2api](https://github.com/lwjlwjlwjlwj/phanthycode2api)（MIT）改进，
在此致谢原作者的协议转换、多账号池与 OAuth 登录框架。主要新增：账号安全加固、按账号代理、
每日签到领积分、内置控制面板与配置热重载，详见下方各章节。

多账号池管理 + 冷却与错误处理 + OpenAI ↔ Anthropic 协议转换，把 PhanthyCode 上游封装成
任意 OpenAI 兼容客户端（NextChat、ChatBox、OpenCat 等）可直接接入的标准接口。

## 功能特性

- 🔑 **可选鉴权** — 配置 `api_key` 后需 Bearer token 访问（空则无鉴权，便于本地调试）
- 🔄 **OpenAI 兼容** — 无痛接入任何支持 OpenAI 格式的客户端（NextChat、ChatBox、OpenCat 等）
- 👥 **多账号池管理** — 自动轮转账号、错误计数阈值冷却、禁用、持久化状态
- 🔁 **协议转换** — OpenAI 请求/响应 ↔ Anthropic Messages API 双向转换，流式 SSE 实时透传
- 🔓 **OAuth 登录** — 半自动 PKCE 授权码流程，一键获取凭证
- ⏰ **定时 keepalive** — 保持 token 活跃，接近过期时自动刷新
- 🗺 **模型映射** — 客户端使用商业名（DeepSeek-V4、gpt-5.6-sol 等），自动映射到上游真实模型
- 🛡 **账号安全加固** — 一号一 IP 代理、风控熔断（命中即禁用且不换号重试）、每账号并发/日限额、指数退避冷却、请求抖动
- 🎛 **内置控制面板** — `/admin/` 单页管理台：概览、接入信息、对话测试台、账号池、代理、安全加固、每日签到、配置编辑；改配置**热生效不重启**
- 🌐 **按账号代理** — 支持 http/https/socks5，兼容 Resin 等代理池的 sticky session（`Platform.Account:token`）
- 🎁 **每日签到领积分** — 定时调用官方签到端点自动领积分，结果持久化，支持手动补签
- 🧩 **官方请求头对齐** — 补齐 `anthropic-version` / `anthropic-beta` / UA / 自定义头，均可用环境变量覆盖
- 🏗 **Go 单二进制** — 无第三方依赖，`go build` 即得

## 快速开始

### 1. 构建 & 配置

```bash
git clone https://github.com/lwjlwjlwjlwj/phanthycode2api.git
cd phanthycode2api
go build -o phanthycode2api ./cmd/server
cp config.example.json config.json
# 编辑 config.json，设置 api_key（可留空 = 不鉴权）
```

### 2. 登录获取凭证

```bash
go run ./cmd/login
```

浏览器打开授权链接 → 登录 PhanthyCode 账号 → 授权后粘贴 code 到终端，凭证自动保存到 `auths/` 目录。

### 3. 启动服务

```bash
./phanthycode2api -config config.json
```

或直接用环境变量（无需配置文件）：

```bash
P2A_LISTEN=:7864 P2A_API_KEY=your-secret-key ./phanthycode2api
```

默认监听 `:7864`，可通过 `P2A_LISTEN` 环境变量覆盖。

### 4. 验证

```bash
# 健康检查（无需鉴权）
curl -s http://localhost:7864/healthz

# 模型列表
curl -s http://localhost:7864/v1/models \
  -H "Authorization: Bearer your-secret-key"

# 聊天（非流式）
curl -s http://localhost:7864/v1/chat/completions \
  -H "Authorization: Bearer your-secret-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"DeepSeek-V4","messages":[{"role":"user","content":"你好"}]}'

# 聊天（流式）
curl -N http://localhost:7864/v1/chat/completions \
  -H "Authorization: Bearer your-secret-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"DeepSeek-V4","stream":true,"messages":[{"role":"user","content":"数到3"}]}'

# 账号池状态
curl -s http://localhost:7864/status \
  -H "Authorization: Bearer your-secret-key"
```

## 控制面板

服务内置管理面板，与 API 同端口：启动后浏览器打开

```
http://<host>:7864/admin/
```

页面为纯静态（不含任何密钥），可直接访问；所有 `/admin/api/*` 在配置了 `api_key` 时
必须带 `Authorization: Bearer <key>`（与 OpenAI 兼容接口同一把钥匙）。在页面右上角
输入一次密钥即可（仅存浏览器 localStorage）。

| 板块 | 能做什么 |
|---|---|
| 概览 | 账号池健康统计（健康/冷却/禁用/今日派发）、运行参数一览、快捷操作 |
| 接入信息 | Base URL、模型清单、JSON 配置、curl 示例、各客户端填写对照（一键复制） |
| 对话测试台 | 直接打 `/v1/chat/completions`，支持流式，实时看回复 |
| 账号池 | 逐账号 解禁 / 禁用 / 清冷却 / 一键签到 / 查积分；批量全部解禁 |
| 代理 | 全局默认代理、账号级代理（一号一 IP）、**代理连通性测试**（显示出口 IP 与延迟） |
| 安全加固 | 并发上限、每日限额、抖动、等槽位、最多换号、上游超时、风控阻断开关、冷却策略 |
| 每日签到 | 签到时间设置（支持多个整点）、立即签到、今日结果、最近 7 天记录 |
| 设置 | 直接编辑完整 config.json，保存即写回磁盘并热生效；也可从磁盘重载 |

### 热生效范围

面板改动**不需要重启服务**，对后续新请求立即生效：

- 代理（全局 / 账号级）— 重建该代理的连接池，在途请求不受影响
- 安全参数（并发、日限额、抖动、超时、风控开关、换号次数、冷却时长）
- 签到时间（keepalive / claim_hours）
- 上游请求头（UA、anthropic-version、anthropic-beta、自定义头）

以下项改动需要重启：`listen`、`auth_dir`、`state_file`、`claims_file`、`api_key`
（`api_key` 改动会立即影响后续请求鉴权，但页面提示需重新保存密钥）。

### 管理 API 一览

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/admin/api/overview` | 概览（账号统计 + 运行参数 + 签到摘要） |
| GET/PUT | `/admin/api/config` | 读取 / 局部更新配置（`{"patch":{...},"persist":true}`） |
| POST | `/admin/api/reload` | 从磁盘重新加载配置 |
| GET | `/admin/api/accounts` | 账号列表 |
| POST | `/admin/api/accounts/{uid}/enable\|disable\|clear-cooldown\|claim` | 账号操作 |
| PUT | `/admin/api/accounts/{uid}/proxy` | 设置/清除账号代理（`{"proxy":"..."}`） |
| GET | `/admin/api/rewards/{uid}` | 查询该账号积分 |
| POST | `/admin/api/proxy/test` | 代理连通性测试（`{"proxy":"...","echo_url":"..."}`） |
| GET | `/admin/api/claims` | 签到状态与最近记录 |
| POST | `/admin/api/claims/run` | 立即对全部账号签到 |

示例：

```bash
# 改并发与日限额并落盘
curl -X PUT http://localhost:7864/admin/api/config \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{"persist":true,"patch":{"safety":{"concurrency":1,"daily_limit":200}}}'

# 给某账号绑代理（Resin sticky 格式）
curl -X PUT http://localhost:7864/admin/api/accounts/acc1/proxy \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{"proxy":"http://Default.acc1:token@resin-host:8080","persist":true}'
```

## 配置说明

```json
{
  "listen": ":7864",
  "api_key": "",
  "auth_dir": "./auths",
  "state_file": "./data/state.json",
  "claims_file": "./data/claims.json",
  "base_url": "https://code.phanthy.com",
  "cooldown": {
    "hard_credit": "12h",
    "soft_rate": "60s",
    "err_threshold": 3,
    "err_cooldown": "10m"
  },
  "schedule": {
    "keepalive_hours": [22],
    "claim_hours": [8]
  },
  "proxy": {
    "default": "",
    "by_account": {
      "acc1": "http://Default.acc1:token@127.0.0.1:8080"
    }
  },
  "safety": {
    "daily_limit": 0,
    "concurrency": 1,
    "slot_wait": "0s",
    "jitter_max": "300ms",
    "refresh_skew": "10m",
    "max_rotate": 3,
    "block_on_risk_control": true
  },
  "upstream": {
    "timeout_seconds": 600,
    "user_agent": "",
    "anthropic_version": "2023-06-01",
    "beta": "oauth-2025-04-20",
    "extra_headers": {}
  }
}
```

| 配置项 | 环境变量 | 默认值 | 说明 |
|---|---|---|---|
| `listen` | `P2A_LISTEN` | `:7864` | 监听地址 |
| `api_key` | `P2A_API_KEY` | `""` | 服务端 API 密钥（空则无鉴权） |
| `auth_dir` | `P2A_AUTH_DIR` | `./auths` | 账号凭证目录 |
| `state_file` | `P2A_STATE_FILE` | `./data/state.json` | 账号状态持久化路径 |
| `claims_file` | `P2A_CLAIMS_FILE` | `./data/claims.json` | 签到记录持久化路径 |
| `base_url` | `P2A_BASE_URL` | `https://code.phanthy.com` | 上游 API 地址 |
| `cooldown.hard_credit` | `P2A_HARD_CREDIT` | `12h` | 积分不足冷却时长 |
| `cooldown.soft_rate` | `P2A_SOFT_RATE` | `60s` | 限流冷却时长 |
| `cooldown.err_threshold` | `P2A_ERR_THRESHOLD` | `3` | 连续错误阈值 |
| `cooldown.err_cooldown` | `P2A_ERR_COOLDOWN` | `10m` | 错误冷却时长 |
| `schedule.keepalive_hours` | — | `[22]` | 定时 keepalive 小时 |
| `schedule.claim_hours` | `P2A_CLAIM_HOURS` | `[8]` | 每日签到小时（`off` 关闭） |
| `proxy.default` | `P2A_PROXY_URL` | `""` | 全局默认代理 |
| `proxy.by_account` | `P2A_PROXY_BY_ACCOUNT` | `{}` | 账号级代理（覆盖全局） |
| `safety.daily_limit` | `P2A_DAILY_LIMIT` | `0` | 每账号每日请求上限（0=不限） |
| `safety.concurrency` | `P2A_CONCURRENCY` | `1` | 每账号并发上限 |
| `safety.slot_wait` | `P2A_SLOT_WAIT` | `0s` | 等并发槽位最长时间 |
| `safety.jitter_max` | `P2A_JITTER_MAX` | `300ms` | 派发前随机抖动上限 |
| `safety.refresh_skew` | `P2A_REFRESH_SKEW` | `10m` | token 提前刷新窗口 |
| `safety.max_rotate` | `P2A_MAX_ROTATE` | `3` | 单请求最多换号次数 |
| `safety.block_on_risk_control` | — | `true` | 命中风控是否阻断整个请求 |
| `upstream.timeout_seconds` | `P2A_TIMEOUT_SECONDS` | `600` | 上游请求超时（含流式读取总时长） |
| `upstream.user_agent` | `P2A_UA` | `phanthycode2api/1.0` | 上游请求 UA |
| `upstream.anthropic_version` | `P2A_ANTHROPIC_VERSION` | `2023-06-01` | Anthropic 版本头（官方 CLI 必发） |
| `upstream.beta` | `P2A_BETA` | `oauth-2025-04-20` | anthropic-beta 头 |
| `upstream.extra_headers` | `P2A_EXTRA_HEADERS` | `{}` | 额外注入头（`k1=v1,k2=v2`） |

### 代理配置（一号一 IP）

代理对客户端暴露的是**标准正向代理**，所以格式与普通 HTTP/SOCKS5 代理完全一致：

```bash
# 全局
P2A_PROXY_URL="http://127.0.0.1:8080"
# 账号级（逗号分隔）
P2A_PROXY_BY_ACCOUNT="acc1=http://Default.acc1:tok@127.0.0.1:8080,acc2=socks5://127.0.0.1:1081"
# 凭证文件里也可以直接写 proxy 字段（三种凭证形态均支持）
# {"auth":{...},"account":{"uid":"acc1"},"proxy":"http://Default.acc1:tok@host:8080"}
```

配合 [Resin](https://github.com/Resinat/Resin) 这类代理池时，把代理认证用户名写成
`Platform.Account:token`（如 `Default.phanthy_acc1:my-token`），每个 Account 会绑定固定出口 IP，
即实现「一个账号永远一个 IP」，这是账号安全的第一道防线。同一账号的
chat / OAuth 刷新 / keepalive / 签到请求全部复用同一个代理连接池，不会中途换 IP。

### 账号安全加固说明

| 机制 | 行为 |
|---|---|
| 风控熔断 | 响应命中风控/封禁关键词（中英文）→ 立即禁用该账号，并**不换号重试**，直接返回 403，避免把风险扩散到其他账号 |
| 弱关键词保护 | `forbidden`/`abnormal` 等弱词仅在 403/429 下判风控，普通业务响应不会误杀账号 |
| 并发闸门 | 每账号并发上限（默认 1），占满即跳过换下一个账号，不排队堆积 |
| 每日限额 | 每账号每日请求上限，超限当天不再派发 |
| 指数退避 | 软限流/连续错误冷却按 `base × 2^n` 递增（软限流封顶 1h，错误封顶 8h），成功后重置 |
| 拟人抖动 | 每次派发前随机 sleep（默认 0~300ms），打散固定请求节奏 |
| 凭证保护 | 凭证文件以 0600 原子写回，日志只打 UID 不打 token |

### 每日签到领积分

- 端点（取自官方桌面端 sidecar 路由表）：
  `POST /api/oauth/activities/daily-login/claim`、查询 `GET /api/oauth/rewards`
- 默认每天 `claim_hours`（默认 `[8]`）触发，整点基础上加 **0~15 分钟随机抖动**，避免所有账号扎堆
- 启动即补签一次；`data/claims.json` 记录每天每账号结果，已成功签到的当天不重复请求
- 签到失败**不冷却账号**（与 chat 错误分开），只记录原因，下次调度重试
- 手动补签：`curl -X POST http://localhost:7864/claims/run -H "Authorization: Bearer <key>"`
- 查看结果：`curl http://localhost:7864/claims -H "Authorization: Bearer <key>"`
- 关闭签到：`P2A_CLAIM_HOURS=off`

> 提示：签到与多账号轮转本身存在封号风险，加固只能降低概率。建议主力账号只跑 chat，
> 小号再开启签到。

### 修改监听端口

端口**可以随时改**，默认 `:7864`。三种改法：

| 方式 | 操作 | 生效时机 |
|---|---|---|
| 配置文件 | `config.json` 的 `"listen": ":9000"` | 重启后 |
| 环境变量 | `P2A_LISTEN=":9000"` | 重启后 |
| Docker | 新建 `.env`，写 `P2A_PORT=9000`，然后 `docker compose up -d` | 重建容器后 |

`docker-compose.yml` 已把端口参数化：`ports`、`P2A_LISTEN`、`healthcheck` 三处都跟随
`${P2A_PORT:-7864}`，所以只改一个 `P2A_PORT` 即可，不会出现「端口改了但健康检查还在探 7864」
导致容器被标成 unhealthy 的情况。

注意：`listen` 属于启动期参数，在面板「设置」里改会写入配置文件，但**仍需重启才生效**。

### 可用模型

| 客户端名 | 上游模型 |
|---|---|
| DeepSeek-V4 | Iris-1.0 |
| gpt-5.6-sol | Zeus-1.1-pro |
| gpt-5.6-terra | Zeus-1.1 |
| gpt-5.6-luna | Zeus-1.1-fast |
| gpt-5.5 | Zeus-1.0-pro |
| Claude Opus 4.8 | Gaia-1.2 |
| Claude Opus 4.7 | Gaia-1.1 |
| Claude Sonnet 4.6 | Gaia-1.0 |
| Kimi K3 | Apollo-2.0 |
| Kimi-k2.7-code | Apollo-1.1 |
| Kimi K2.6 | Apollo-1.0 |
| GLM 5.2 | Metis-1.1 |
| GLM-5.1 | Metis-1.0 |

## API

### `POST /v1/chat/completions`

OpenAI 兼容。支持 `stream`（SSE）、`max_tokens`、`temperature`、`top_p`。

### `GET /v1/models`

返回配置的模型列表。

### `GET /status`

账号池状态概览（每个账号的 UID、状态、冷却、错误计数、今日请求数）。

### `GET /claims`

最近 7 天签到记录（每账号每天的成功/失败与摘要）。未启用签到返回 `{"enabled":false}`。

### `POST /claims/run`

立即触发一次签到（手动补签）。

### `GET /healthz`

健康检查（无需鉴权）。

## 鉴权机制

启用方式：在 `config.json` 设置 `api_key`（或环境变量 `P2A_API_KEY`），服务端即开启 Bearer 鉴权。

- 受保护端点（`/v1/chat/completions`、`/v1/models`、`/status`、`/claims` 及全部 `/admin/api/*`）需携带请求头：
  ```
  Authorization: Bearer <your_api_key>
  ```
- 缺失或错误的 token → `401 {"error":{"code":"invalid_api_key",...}}`
- `/healthz` **不**参与鉴权（方便健康检查与容器探针）
- `api_key` 留空（默认）→ 不鉴权，任意请求可直接访问（适合本地开发 / 内网部署）

鉴权在 `internal/server/handler.go` 的 `withAuth` 中间件中实现：根据配置的 `APIKey`
统一拦截，匹配失败直接返回 401，零外部依赖。

## Docker 部署

### 1. 构建镜像 & 启动

```bash
# 可选：编辑 docker-compose.yml 设置 P2A_API_KEY 开启鉴权
docker compose up -d --build
```

服务默认监听 `7864`，通过 `docker-compose.yml` 的 `ports` 映射到宿主机。

### 2. 凭证与状态持久化

账号凭证（`auths/`）与运行状态（`data/`）已通过卷挂载持久化，容器重建不会丢失。
**注意**：这两目录含真实 token，已在 `.gitignore` / `.dockerignore` 中排除，切勿提交。

首次使用仍需在宿主机生成凭证后再挂载：

```bash
# 宿主机本地生成凭证（go run ./cmd/login），写入 ./auths
go run ./cmd/login
# 凭证就绪后再 docker compose up，容器直接复用 ./auths
```

### 3. 自定义配置

如需完全自定义，挂载本地 `config.json`（镜像内不再内置 `config.json`，未挂载时走代码默认值 + `P2A_*` 环境变量）：

```yaml
# docker-compose.yml（取消注释）
volumes:
  - ./config.json:/app/config.json:ro
```

### 4. 验证

```bash
# 健康检查
curl -s http://localhost:7864/healthz

# 聊天（若已开启鉴权，加 -H "Authorization: Bearer <key>"）
curl -s http://localhost:7864/v1/chat/completions \
  -H "Authorization: Bearer your-secret-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"DeepSeek-V4","messages":[{"role":"user","content":"你好"}]}'
```

### 服务端口

| 服务 | 端口 | 说明 |
|------|------|------|
| phanthycode2api | `7864` | OpenAI 兼容 API 入口 |

> 国内网络环境：Dockerfile 使用官方 `golang:1.26-alpine` 基础镜像；若拉取缓慢，可改用国内镜像源（如阿里云 `registry.cn-hangzhou.aliyuncs.com`）或配置 Docker daemon 镜像加速。

## 目录结构

```
phanthycode2api/
├── cmd/
│   ├── server/          # 服务入口：配置 → auth → pool → upstream → scheduler → server
│   │   ├── main.go
│   │   └── config.go
│   └── login/           # 半自动 OAuth 登录工具
│       └── main.go
├── internal/
│   ├── admin/           # 控制面板：管理 API + 单文件页面（admin.html，embed 托管）
│   ├── conf/            # 配置定义 + 热重载管理器（Manager/Runtime，atomic 替换）
│   ├── auth/            # 账号解析（三种形态）+ 原子写入 + 账号级代理字段
│   ├── pool/            # 账号池状态机（冷却、禁用、并发/日限额闸门、指数退避）
│   ├── upstream/        # 核心：OpenAI ↔ Anthropic 协议转换 + SSE 流式转换
│   │                    #   client.go（请求头/错误分类/风控识别）proxy.go（代理池）claim.go（签到）
│   ├── server/          # OpenAI 兼容 HTTP 服务器，带轮转、风控熔断 + 鉴权中间件
│   ├── claims/          # 签到结果持久化（data/claims.json）
│   └── scheduler/       # 定时 keepalive + 每日签到任务
├── config.example.json  # 配置模板
├── Dockerfile           # 多阶段构建
├── docker-compose.yml   # 容器编排（含 healthcheck）
├── .dockerignore
├── .gitignore
└── go.mod
```

## 免责声明

本项目仅供学习和研究使用。请遵守 PhanthyCode 平台服务条款，自行承担使用风险。
作者不对任何因使用本项目产生的直接或间接损失负责。

## License

MIT
