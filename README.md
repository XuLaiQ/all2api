# All2API

All2API 是一个面向多渠道大模型服务的统一 API 网关和管理控制台。它把 WorkBuddy、Doubao、ChatGPT、Grok 的渠道适配、账号池、模型目录、访问密钥、路由、日志、审计和用量统计集中到一个项目中，对外提供一个统一的 `/v1` 地址。

后端完全由 Go 实现（`cmd/` + `internal/`），前端是 React + TypeScript 控制台（`web/`）。

> **当前状态**：核心网关、管理控制台、四渠道 native adapter/provision、SQLite 持久化和安全边界已经具备，项目仍处于真实平台验收阶段。生产部署前必须使用目标平台账号完成 E2E 验证，并通过 `scripts/verify/check-release-readiness.ps1` 的发布检查。

## 目录

- [功能概览](#功能概览)
- [支持范围](#支持范围)
- [系统架构](#系统架构)
- [快速开始](#快速开始)
- [配置](#配置)
- [使用统一网关](#使用统一网关)
- [管理控制台](#管理控制台)
- [账号新增流程](#账号新增流程)
- [数据与安全](#数据与安全)
- [开发与测试](#开发与测试)
- [仓库结构](#仓库结构)
- [文档](#文档)
- [当前限制](#当前限制)
- [许可证](#许可证)

## 功能概览

### 统一数据面

- 一个网关地址：默认 `http://localhost:8888/v1`。
- 一个网关 API Key 可按渠道和模型授权，调用不同渠道的模型。
- 支持 OpenAI Chat Completions、Anthropic Messages 和 OpenAI Responses 三种文本协议。
- 支持非流式和流式文本对话；流式响应会转换为对应协议的 SSE 格式。
- 模型 ID 使用 `渠道/上游模型 ID` 形式，例如 `wb/example-model`、`doubao/example-model`、`chatgpt/example-model`、`grok/grok-chat-fast`。
- 支持按模型或路由别名选择目标，并记录实际渠道、账号、回退层级、延迟和错误类型。
- 上游未返回 usage 时，网关用内置的 tiktoken 词表在本地估算 Token，并在用量中标记为“估算”。
- 统一返回请求 ID 和协议对应的错误结构，便于排查请求日志。

### 渠道与账号池

- 内置 `wb`（WorkBuddy）、`doubao`（Doubao）、`chatgpt`（ChatGPT）、`grok`（Grok Web/Build）四个渠道。
- 通过 registry 和 manifest 描述渠道协议、能力、配置和账号新增流程。
- 账号只写入本项目自己的 SQLite 数据库，不读取任何外部项目的数据库、账号文件或管理端口。
- 支持账号启用/停用、刷新凭据（WorkBuddy、ChatGPT）、批量删除。
- 数据面使用本地账号池和租约；渠道/账号支持冷却、熔断和失败状态记录。
- 账号新增使用持久化 session、幂等键和统一的 start/poll/complete/cancel/import 生命周期。

### 管理控制台

- 运行总览：请求量、Token、渠道健康、账号池和路由概况。
- 渠道：查看 manifest、配置渠道启用状态、显式测试平台连接和查看 runtime 状态。
- 账号池：查看账号状态、筛选、启停、刷新凭据、删除和按渠道动态新增账号。
- 模型目录：实时刷新渠道模型，持久化 observed cache，搜索/筛选并启用或停用模型。
- 路由规则：创建跨渠道的 priority 回退链，最多配置 8 个不同渠道目标。
- 网关密钥：创建、编辑、轮换和撤销 `sk-a2a-*` Key，设置渠道、模型、过期时间和 RPM 限制。
- 请求日志：按请求 ID、渠道、模型、状态和时间筛选，分页查看并清理过期数据。
- 审计日志：记录登录、Key、账号、渠道、路由、模型、用户和保留策略等管理写操作。
- 用量统计：按天、渠道、模型或 Key 汇总请求和 Token，区分上游报告、本地估算和未知用量；调试台请求也计入统计（`key_id=0`）。
- 素材库：保存生成的图片、视频等素材，支持筛选、预览、下载和删除；带水印的图片可用 Telea 修补算法去除右下角水印，并另存为 `_clean` 副本。
- 系统健康：查看服务信息、存储检查、指标、保留策略和渠道运行状态。
- 调试台：管理员直接对已配置渠道发起聊天请求，保存会话历史和调用记录。
- 用户管理：维护 viewer/admin 角色和启用状态；登录凭据仍由环境变量提供。

## 支持范围

### 内置渠道

| 渠道 | 标识 | 协议 | 当前能力 | 账号新增方式 |
| --- | --- | --- | --- | --- |
| WorkBuddy | `wb` | OpenAI、Anthropic、Responses | 文本聊天 | Token 导入、二维码 OAuth（`cn`/`global`） |
| Doubao | `doubao` | OpenAI、Anthropic、Responses | 文本聊天 | Cookie 导入、二维码登录（默认 HTTP QR；可选 Chromium browser worker） |
| ChatGPT | `chatgpt` | OpenAI、Anthropic、Responses | 文本聊天 | Token 导入、OAuth PKCE；真实 refresh/platform E2E 仍待验收 |
| Grok Web/Build | `grok` | OpenAI、Anthropic、Responses | 文本聊天 | Web SSO 或 Build OAuth Token 导入；真实平台 E2E 仍待验收 |

### 协议接口

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `GET` | `/v1/models` | 返回当前 Key 可见且已缓存/可用的模型 |
| `POST` | `/v1/chat/completions` | OpenAI Chat Completions，支持流式 |
| `POST` | `/v1/messages` | Anthropic Messages，支持 `x-api-key` 或 Bearer 认证，含 tool use/tool result 映射 |
| `POST` | `/v1/responses` | OpenAI Responses，接受文本或文本消息输入，支持流式 |
| `GET/POST/DELETE` | `/v1/files`、`/v1/files/{id}`、`/v1/files/{id}/content` | 本地文件上传、列表、元数据、删除和下载；按网关 Key 隔离 |

四个内置渠道当前只声明已验证的 `chat` 能力。以下路径已路由，但由 manifest 能力门禁保护：

| 路径 | 当前行为 |
| --- | --- |
| `/v1/images/generations`、`/v1/video/generations`、`/v1/search` | 渠道 manifest 声明对应能力并通过真实 E2E 后才接受请求；结果写入媒体存储 |
| `/v1/images/edits`、`/v1/audio/generations`、`/v1/files/download` | 返回 `capability_not_supported` |
| `/v1/ppt/generations`、`/v1/psd/generations`、`/v1/editable-file-tasks` | 返回 `capability_not_supported` |
| `/v1/messages/count_tokens` | 返回 `capability_not_supported` |

## 系统架构

```text
浏览器
  │
  ├── React + TypeScript 控制台
  │       └── /admin/api/*  管理面会话、角色和审计
  │
  └── OpenAI / Anthropic / Responses 客户端
          └── /v1/*         API Key、scope 和协议转换
                                  │
                       all2api（Go，cmd/all2api）
                                  │
                 registry → 路由/账号池 → native adapter
                                  │
             WorkBuddy / Doubao / ChatGPT / Grok 官方平台

   可选 worker：doubao-browser-worker（Chromium/CDP）、media-worker（去水印）

                         SQLite + WAL（data/all2api.db）
              账号、模型、Key、日志、用量、审计、session、素材元数据
                         │
                 data/media-assets/ 素材文件
                         │
                 Fernet 加密的渠道凭据
```

## 快速开始

### 环境要求

- Go `1.27+`
- Node.js `20.19+`、pnpm `10`（控制台）
- Docker Desktop（可选，使用 Compose 时）
- 真实账号新增和调用还需要目标平台可访问。

### 准备配置

```powershell
Copy-Item .env.example .env
```

编辑 `.env`，至少设置：

- `A2A_SESSION_SECRET`：随机高熵会话密钥。
- `A2A_ADMIN_PASSWORD`：长度至少 12 个字符的管理员密码。未设置时登录接口返回未配置错误，不会创建默认密码。
- `A2A_CREDENTIAL_MASTER_KEY`：Fernet key 或高熵密钥。
- `A2A_ENV`：开发用 `development`；生产必须为 `production`，启用生产安全硬门禁。

### 方式一：本地运行

```powershell
# 后端：自动读取 ./.env，数据默认写入 ./data/
go run ./cmd/all2api

# 另开终端启动控制台
pnpm --dir web install --frozen-lockfile
pnpm --dir web dev
```

打开 <http://localhost:5555>。Vite 会把 `/admin/api` 和 `/v1` 代理到 `VITE_PROXY_TARGET`（默认 `http://127.0.0.1:8888`）。

### 方式二：Docker Compose

```powershell
docker compose up --build
```

Compose 启动 `api`、`browser-worker`、`media-worker` 和 `web` 四个服务，运行数据保存在 `api_data` 等命名卷中。不要使用 `docker compose down -v`，除非确认要删除数据库、账号和加密凭据。

服务地址：

- 控制台：<http://localhost:5555>
- API：<http://localhost:8888>
- 健康检查：<http://localhost:8888/admin/api/healthz>

## 配置

所有配置使用 `A2A_` 前缀的环境变量，完整模板和说明见 [`.env.example`](.env.example)。`all2api` 启动时读取 `./.env`（或 `A2A_ENV_FILE` 指定的文件），进程中已存在的环境变量优先。

| 配置组 | 主要变量 | 作用 |
| --- | --- | --- |
| 服务与存储 | `A2A_HOST`、`A2A_PORT`、`A2A_DB_PATH`、`A2A_STATE_PATH` | 监听地址、SQLite 路径和运行状态路径 |
| 管理安全 | `A2A_SESSION_SECRET`、`A2A_ADMIN_USERNAME`、`A2A_ADMIN_PASSWORD`、`A2A_ADMIN_TOKEN` | 管理会话、管理员登录和自动化管理认证 |
| 凭据安全 | `A2A_CREDENTIAL_MASTER_KEY` | 加密本地账号凭据；恢复数据库必须同时恢复此密钥 |
| 网关引导 | `A2A_BOOTSTRAP_API_KEY`、`A2A_BOOTSTRAP_RPM` | 启动时同步名为 `bootstrap` 的网关 Key；清空变量即禁用 |
| 渠道端点 | `A2A_WB_PLATFORM_BASE`、`A2A_DOUBAO_PLATFORM_BASE`、`A2A_CHATGPT_*`、`A2A_GROK_*` | 官方平台端点和 ChatGPT 网络代理 |
| 账号 session | `A2A_PROVISION_SESSION_TTL_SECONDS` | QR/OAuth provision session TTL；启动时清理过期的终态 session |
| Doubao worker | `A2A_DOUBAO_PROFILE_ROOT`、`A2A_DOUBAO_BROWSER_*` | 可选的 Chromium/CDP worker；启用时必须配置 worker token |
| 保留与跨域 | `A2A_LOG_RETENTION_DAYS`、`A2A_USAGE_RETENTION_DAYS`、`A2A_CORS_ORIGINS` | 日志/用量保留周期和浏览器来源 |

生产环境（`A2A_ENV=production`）会拒绝默认 session secret、弱 credential master key、弱管理员密码、启用但无 token 的 browser worker，以及仍然设置的已删除 legacy bridge 变量（`A2A_LEGACY_*`、`A2A_*_UPSTREAM_BASE` 等）。同时应按反向代理拓扑设置 `A2A_SECURE_COOKIE`、`A2A_TRUST_PROXY`、`A2A_TRUSTED_PROXIES`，并通过 Secret Manager 注入密钥。

## 使用统一网关

### 1. 创建网关 Key

登录控制台，在“网关密钥”页面创建 Key。创建和轮换时明文 Key 只返回一次，数据库只保存 hash。Key 可以配置：

- `channels`：允许访问的渠道；空列表表示所有已注册且可用渠道。
- `models`：允许访问的模型 ID；默认 `[*]`。
- `expires_at`：过期时间。
- `limit_rpm`：每分钟请求上限，`0` 表示不限制。

### 2. 查看模型

```bash
export A2A_BASE_URL="http://localhost:8888/v1"
export A2A_API_KEY="sk-a2a-your-key"

curl "$A2A_BASE_URL/models" \
  -H "Authorization: Bearer $A2A_API_KEY"
```

`/v1/models` 只返回当前 Key 有权访问且网关已观察到的模型。

### 3. OpenAI Chat Completions

```bash
curl "$A2A_BASE_URL/chat/completions" \
  -H "Authorization: Bearer $A2A_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "wb/example-model",
    "messages": [{"role": "user", "content": "你好"}],
    "stream": false
  }'
```

流式请求只需将 `stream` 改为 `true`。也可以使用路由别名作为模型值，让网关按照管理面配置的 priority 目标链回退。

### 4. Anthropic Messages

```bash
curl "$A2A_BASE_URL/messages" \
  -H "x-api-key: $A2A_API_KEY" \
  -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "chatgpt/example-model",
    "max_tokens": 512,
    "messages": [{"role": "user", "content": "你好"}],
    "stream": true
  }'
```

### 5. OpenAI Responses

```bash
curl "$A2A_BASE_URL/responses" \
  -H "Authorization: Bearer $A2A_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "doubao/example-model",
    "input": "请用一句话介绍 All2API"
  }'
```

调用方不需要知道渠道内部地址、Cookie、OAuth token 或浏览器 profile。请求日志只保留脱敏后的请求元数据和运行结果。

## 管理控制台

控制台使用管理会话 Cookie，API 路径以 `/admin/api` 为前缀：登录 `POST /admin/api/auth/login`、会话 `GET /admin/api/auth/session`、退出 `POST /admin/api/auth/logout`。

自动化客户端可以配置 `wbt_*` 格式的 `A2A_ADMIN_TOKEN`，用 `Authorization: Bearer <management-token>` 访问管理 API；该方式直接使用 `admin` 角色，不创建浏览器 session。

权限规则：

- `admin`：可以写入 Key、模型、路由、渠道、账号、设置和用户，并可以使用调试台。
- `viewer`：只能读取管理数据。
- 浏览器 session 的管理写请求要求同源；登录失败会记录审计并触发限流。
- `GET /admin/api/healthz` 不要求登录，可用于容器探针；`?detail=true` 返回存储和渠道配置状态，不会请求上游平台。

主要管理 API：

| 资源 | API |
| --- | --- |
| 总览、系统和用量 | `/admin/api/overview`、`/admin/api/sysinfo`、`/admin/api/storage/health`、`/admin/api/metrics`、`/admin/api/stats/*` |
| 渠道 | `/admin/api/channels`、`/admin/api/channels/{slug}/test`、`/admin/api/channels/{slug}/runtime` |
| 账号 | `/admin/api/accounts`、`/admin/api/channels/{channel}/provision-schema`、`/admin/api/channels/{channel}/accounts/provision/*` |
| 模型 | `/admin/api/models`、`/admin/api/models/refresh` |
| Key | `/admin/api/keys` |
| 路由 | `/admin/api/routes` |
| 日志与审计 | `/admin/api/logs`、`/admin/api/audit-logs` |
| 设置与用户 | `/admin/api/settings`、`/admin/api/users` |
| 素材 | `/admin/api/media/assets`、`/admin/api/media/assets/{id}/remove-watermark` |
| 调试台 | `/admin/api/playground/conversations`、`/admin/api/playground/runs`、`/admin/api/playground/chat` |

完整请求字段、状态码和错误契约见 [API 契约](docs/API契约.md)。所有公开路由登记在 [`docs/migration-baseline/routes.json`](docs/migration-baseline/routes.json)，`go test` 会校验每一条都已注册。

## 账号新增流程

统一流程是“先选择渠道，再读取该渠道 manifest 的动态 schema”：

```text
选择渠道
  → 获取 provision-schema
  → 选择该渠道的 flow
  → start/import
  → poll（二维码或 OAuth 状态）
  → complete / cancel
  → 凭据加密落库 + 账号安全快照 + 审计记录
```

| 渠道 | flow | 说明 |
| --- | --- | --- |
| WorkBuddy | `token-import` | 管理员提交 token bundle；只保存加密凭据和脱敏账号信息 |
| WorkBuddy | `qr-oauth` | 生成二维码并轮询授权，支持 `cn`/`global` realm |
| Doubao | `token-import` | 导入 Cookie |
| Doubao | `qr-login` | 默认使用 HTTP QR 登录；启用 browser worker 后由 Chromium profile 完成 |
| ChatGPT | `token-import` | 导入 access/refresh/id token 等 token bundle |
| ChatGPT | `oauth-pkce` | 发起 OAuth PKCE，完成 callback 后交换并加密保存 token |
| Grok | `token-import` | 导入 Web SSO token 或 Build OAuth token |

provision 写接口要求管理员角色和幂等键（`idempotency_key`）。凭据不会出现在账号列表、错误响应、审计日志或前端 DTO 中。

## 数据与安全

- SQLite 开启 WAL，保存渠道覆盖、账号安全快照、加密凭据、模型缓存、路由、Key、请求日志、用量、审计和 provision session。
- `A2A_CREDENTIAL_MASTER_KEY` 用于 Fernet 加密账号凭据；数据库不保存明文 token、Cookie 或 OAuth secret。
- API Key 只以 hash 保存；创建和轮换后的明文只返回一次。
- 管理面使用签名 session Cookie，支持 idle/absolute 过期、登录失败限流、Secure/HttpOnly/SameSite Cookie 和同源写保护。
- 渠道配置写接口拒绝 token、password、cookie、authorization、api key 等敏感字段。
- 请求日志和用量支持独立保留周期，清理操作会写入审计日志。
- `data/` 和 `.env` 已被 `.gitignore` 和 `.dockerignore` 排除，不会进入仓库或镜像构建上下文。

备份时必须同时保护 SQLite 数据和同一份加密主密钥；丢失主密钥后，数据库中的账号凭据无法恢复。

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\ops\backup-go-state.ps1 `
  -DatabasePath .\data\all2api.db `
  -MediaRoot .\data\media-assets `
  -OutputRoot C:\secure-backups\all2api\latest `
  -ServiceStopped
powershell -ExecutionPolicy Bypass -File .\scripts\verify\verify-go-backup-restore.ps1 `
  -BackupRoot C:\secure-backups\all2api\latest
```

备份 manifest 不包含 `.env` 或 `A2A_CREDENTIAL_MASTER_KEY`；恢复时必须从外部 Secret Manager 注入同一主密钥。

## 开发与测试

```powershell
# Go
gofmt -l cmd internal tools        # 无输出表示格式正确
go vet ./...
go test ./...                       # 含路由契约覆盖检查

# 控制台
pnpm --dir web lint
pnpm --dir web typecheck
pnpm --dir web build

# 仓库门禁
go run ./tools/doclinks             # Markdown 本地链接
go run ./tools/supplychain          # 依赖完整性与许可证文件
powershell -ExecutionPolicy Bypass -File .\scripts\verify\verify-go-build.ps1
powershell -ExecutionPolicy Bypass -File .\scripts\verify\verify-go-clean-build.ps1
powershell -ExecutionPolicy Bypass -File .\scripts\verify\verify-go-image.ps1   # 需要 Docker
```

仓库当前没有配置 CI，以上检查需要本地执行。真实平台探针需要单独准备账号，见 [scripts/README.md](scripts/README.md) 的 `probe/` 部分。

## 仓库结构

```text
.
├── cmd/                         可执行入口
│   ├── all2api/                 网关 + 管理面主服务
│   ├── doubao-browser-worker/   Doubao Chromium/CDP worker
│   └── media-worker/            媒体处理 worker
├── internal/                    Go 实现
│   ├── adapters/                WorkBuddy、Doubao、ChatGPT、Grok 和 registry
│   ├── application/             账号、网关、Key、媒体、调试台等用例服务
│   ├── browser/                 浏览器 worker 与 CDP 客户端
│   ├── config/                  A2A_* 环境变量与 .env 加载、校验
│   ├── domain/                  scope 等领域规则
│   ├── infrastructure/          SQLite、加密、媒体存储/去水印、Token 估算和安全实现
│   ├── ports/                   应用层与基础设施之间的接口
│   ├── protocols/               Anthropic/Responses 协议转换
│   ├── scheduler/               账号池和租约
│   └── transport/http/          HTTP 路由与 handler（按领域拆分）
├── tools/                       仓库维护工具（doclinks、supplychain）
├── web/                         React + TypeScript + Vite 管理控制台（pnpm）
├── scripts/                     verify/、probe/、ops/ 脚本
├── docs/                        需求、架构、API、运维文档；archive/ 为历史留存
├── design/                      UI 参考稿，不是业务事实来源
├── data/                        本地运行数据（不入库）
├── Dockerfile                   三个 Go 二进制的镜像
├── docker-compose.yml           本地开发拓扑
└── go.mod / go.sum
```

## 文档

完整索引见 [docs/README.md](docs/README.md)，脚本说明见 [scripts/README.md](scripts/README.md)。

| 文档 | 用途 |
| --- | --- |
| [API 契约](docs/API契约.md) | 数据面、管理面、认证、错误和分页约定 |
| [账号新增流程](docs/账号新增流程.md) | provision flow、状态机和凭据边界 |
| [Grok 渠道接入说明](docs/grok渠道接入说明.md) | Grok Web SSO 导入、模型 tier 和验收边界 |
| [密钥与渠道授权规范](docs/密钥与渠道授权规范.md) | Key scope、渠道/模型授权和审计要求 |
| [部署运维与验收](docs/部署运维与验收.md) | 配置、备份、健康检查和发布门槛 |
| [企业开发规范](docs/企业开发规范.md) | 目录、依赖、测试和安全门禁 |

## 当前限制

1. 各渠道的 native 代码、契约测试和本地状态机已存在，但真实平台网络、账号授权、凭据轮换和平台行为会受官方服务、地区、风控和账号状态影响；本地健康检查不会替代真实平台探测。
2. 当前内置渠道只承诺文本聊天。多媒体路由存在不等于多媒体功能可用，详见[支持范围](#支持范围)。
3. 凭据刷新目前只支持 WorkBuddy 和 ChatGPT 账号。
4. 用户管理只维护角色和启用状态，登录凭据仍来自 `A2A_ADMIN_USERNAME` / `A2A_ADMIN_PASSWORD`。
5. 仓库根目录未包含 `LICENSE`，公开发布前需要补充许可证。

## 许可证

当前仓库尚未声明开源许可证。除非后续补充 `LICENSE` 并明确授权，否则不要假定代码可以按任意开源许可证使用、修改或再分发。
