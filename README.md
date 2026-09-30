# All2API

[![CI](https://github.com/XuLaiQ/all2api/actions/workflows/ci.yml/badge.svg)](https://github.com/XuLaiQ/all2api/actions/workflows/ci.yml)

All2API 是一个面向多渠道大模型服务的统一 API 网关和管理控制台。它把 WorkBuddy、Doubao、ChatGPT 的渠道适配、账号池、模型目录、访问密钥、路由、日志、审计和用量统计集中到一个项目中，对外提供一个统一的 `/v1` 地址。

> 当前状态：核心网关、管理控制台、三渠道 native adapter/provisioner、SQLite 持久化和安全边界已经具备，项目仍处于真实平台验收阶段。生产部署前必须使用目标平台账号完成 E2E 验证，并完成隔离环境下的 clean build/run 验收。

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

- 一个网关地址：默认 `http://localhost:8080/v1`。
- 一个网关 API Key 可按渠道和模型授权，调用不同渠道的模型。
- 支持 OpenAI Chat Completions、Anthropic Messages 和 OpenAI Responses 三种文本协议。
- 支持非流式和流式文本对话；流式响应会转换为对应协议的 SSE 格式。
- 模型 ID 使用 `渠道/上游模型 ID` 形式，例如 `wb/example-model`、`doubao/example-model`、`chatgpt/example-model`。
- 支持按模型或路由别名选择目标，并记录实际渠道、账号、回退层级、延迟和错误类型。
- 统一返回请求 ID 和协议对应的错误结构，便于排查请求日志。

### 渠道与账号池

- 内置 `wb`（WorkBuddy）、`doubao`（Doubao）、`chatgpt`（ChatGPT）三个渠道。
- 通过 registry 和 manifest 描述渠道协议、能力、配置和账号新增流程。
- 账号只写入本项目自己的 SQLite 目录，不读取源项目数据库、账号文件或管理端口。
- 支持账号启用/停用、状态覆盖、优先级、过期时间、刷新和删除。
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
- 用量统计：按天、渠道、模型或 Key 汇总请求和 Token，并区分已报告/未知用量。
- 系统健康：查看服务信息、存储检查、指标、保留策略和渠道运行状态。
- 调试台：管理员对已配置 native runtime 发起流式文本调试请求，支持 Markdown 回答、会话历史和请求记录管理。
- 用户管理：维护 viewer/admin 角色和启用状态；当前登录凭据仍由环境变量提供。

## 支持范围

### 内置渠道

| 渠道 | 标识 | 协议 | 当前能力 | 账号新增方式 |
| --- | --- | --- | --- | --- |
| WorkBuddy | `wb` | OpenAI、Anthropic、Responses | 文本聊天 | Token 导入、二维码 OAuth（`cn`/`global`） |
| Doubao | `doubao` | OpenAI、Anthropic、Responses | 文本聊天 | 本地 profile、二维码登录；默认使用 native HTTP QR worker，可选 Playwright |
| ChatGPT | `chatgpt` | OpenAI、Anthropic、Responses | 文本聊天 | Token 导入、OAuth PKCE；支持凭据刷新 |

### 协议接口

| 方法 | 路径 | 状态 | 说明 |
| --- | --- | --- | --- |
| `GET` | `/v1/models` | 支持 | 返回当前 Key 可见且已缓存/可用的模型 |
| `POST` | `/v1/chat/completions` | 支持 | OpenAI Chat Completions，支持流式文本响应 |
| `POST` | `/v1/messages` | 支持 | Anthropic Messages 文本协议，支持 `x-api-key` 或 Bearer 认证 |
| `POST` | `/v1/responses` | 支持 | OpenAI Responses 文本协议，支持流式转换 |

三个内置渠道的 manifest 当前都只声明 `chat` 能力。以下接口虽然保留了统一路由入口，但不是当前产品能力：

协议转换以文本输入为主；Anthropic Messages 包含已实现的 tool use/tool result 映射，Responses 当前只接受文本或文本消息输入。

| 路径 | 当前行为 |
| --- | --- |
| `/v1/images/generations`、`/v1/video/generations`、`/v1/audio/generations`、`/v1/search` | 能力调度入口已存在；当前内置渠道未声明对应能力，不能按已支持接口使用 |
| `/v1/images/edits`、`/v1/files`、`/v1/files/download` | 返回 `capability_not_supported` |
| `/v1/ppt/generations`、`/v1/psd/generations`、`/v1/editable-file-tasks` | 返回 `capability_not_supported` |
| `/v1/messages/count_tokens` | 当前未实现，返回 `capability_not_supported` |

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
                          FastAPI 网关服务
                                  │
                 registry → 路由/账号池 → native adapter
                                  │
             WorkBuddy / Doubao / ChatGPT 官方平台

                         SQLite + WAL
              账号、模型、Key、日志、用量、审计、session
                         │
                 Fernet 加密的渠道凭据
```

三个源项目只作为一次性迁移和行为审计输入，不是运行时依赖。native adapter、provisioner、账号状态和数据存储都由本仓库管理。迁移期兼容 bridge 默认关闭，生产部署不需要它。

## 快速开始

### 环境要求

- Docker Desktop（使用 Compose 时）。
- 本地开发：Python `3.13.x`、[uv](https://docs.astral.sh/uv/)、Node.js `20.19+`、pnpm `10`。
- 真实账号新增和调用还需要目标平台可访问，并由管理员在控制台完成渠道配置和账号授权。

### 方式一：Docker Compose

在仓库根目录执行：

```powershell
Copy-Item .env.example .env
```

编辑 `.env`，至少设置以下值：

- `A2A_SESSION_SECRET`：随机高熵会话密钥。
- `A2A_ADMIN_PASSWORD`：长度至少 12 个字符的管理员密码。
- `A2A_CREDENTIAL_MASTER_KEY`：生产环境使用随机 Fernet key 或高熵密钥。

然后启动：

```powershell
docker compose up --build
```

服务地址：

- 控制台：<http://localhost:5173>
- API：<http://localhost:8080>
- OpenAPI：<http://localhost:8080/docs>
- 健康检查：<http://localhost:8080/admin/api/healthz>

停止服务：

```powershell
docker compose down
```

Compose 使用 `api_data` 等命名卷保存运行数据。不要使用 `docker compose down -v`，除非确认要删除本地数据库、账号元数据和加密凭据。

### 方式二：本地开发

启动后端：

```powershell
Copy-Item services/api/.env.example services/api/.env
Set-Location services/api
uv sync --extra dev
uv run uvicorn app.main:app --host 127.0.0.1 --port 8080 --reload
```

另开终端启动前端：

```powershell
Set-Location web
pnpm install --frozen-lockfile
pnpm exec vite --host 127.0.0.1 --port 5173
```

打开 <http://localhost:5173>。Vite 会将 `/admin/api` 和 `/v1` 代理到 `http://127.0.0.1:8080`；如需修改代理目标，复制 `web/.env.example` 为 `web/.env.local` 并调整 `VITE_PROXY_TARGET`。

首次登录使用 `services/api/.env` 中的 `A2A_ADMIN_USERNAME` 和 `A2A_ADMIN_PASSWORD`。未配置强密码时，管理登录接口会返回未配置错误，不会创建默认可用密码。

## 配置

后端使用 `A2A_` 前缀的环境变量，完整模板见 [`services/api/.env.example`](services/api/.env.example)。Docker Compose 的根模板见 [`.env.example`](.env.example)。

| 配置组 | 主要变量 | 作用 |
| --- | --- | --- |
| 服务与存储 | `A2A_HOST`、`A2A_PORT`、`A2A_DB_PATH`、`A2A_STATE_PATH` | 监听地址、SQLite 路径和运行状态路径 |
| 管理安全 | `A2A_SESSION_SECRET`、`A2A_ADMIN_USERNAME`、`A2A_ADMIN_PASSWORD`、`A2A_ADMIN_TOKEN` | 管理会话、初始管理员登录和自动化管理认证 |
| 凭据安全 | `A2A_CREDENTIAL_MASTER_KEY` | 加密本地账号凭据；数据库恢复必须同时恢复此密钥 |
| 网关引导 | `A2A_BOOTSTRAP_API_KEY`、`A2A_BOOTSTRAP_RPM` | 可选的配置注入型初始网关 Key 和速率限制 |
| 原生渠道 | `A2A_WB_PLATFORM_BASE`、`A2A_DOUBAO_PLATFORM_BASE`、`A2A_CHATGPT_PLATFORM_BASE`、`A2A_CHATGPT_PROXY` | native adapter 使用的官方平台端点和网络代理；账号凭据来自本地账号池 |
| 账号 session | `A2A_PROVISION_SESSION_TTL_SECONDS` | QR/OAuth provision session 的持久化 TTL |
| Doubao worker | `A2A_DOUBAO_PROFILE_ROOT`、`A2A_DOUBAO_BROWSER_ENABLED` 及 `A2A_DOUBAO_BROWSER_*` | 本项目管理的 profile 和可选 Playwright worker |
| 保留与跨域 | `A2A_LOG_RETENTION_DAYS`、`A2A_USAGE_RETENTION_DAYS`、`A2A_CORS_ORIGINS` | 日志/用量保留周期和浏览器来源 |

生产环境还应设置 `A2A_SECURE_COOKIE`、`A2A_TRUST_PROXY`、`A2A_TRUSTED_PROXIES`，并通过 Secret Manager 注入密码、session secret、加密主密钥和 bootstrap Key。`A2A_LEGACY_BRIDGE_ENABLED` 默认必须为 `false`；`A2A_LEGACY_*` 变量仅用于迁移对照，不是 native 运行所需配置。

## 使用统一网关

### 1. 创建网关 Key

登录控制台，在“网关密钥”页面创建 Key。创建和轮换时明文 Key 只返回一次，数据库只保存 hash。Key 可以配置：

- `channels`：允许访问的渠道；空列表表示所有已注册且可用渠道。
- `models`：允许访问的模型 ID；默认 `[*]`。
- `expires_at`：过期时间。
- `limit_rpm`：每分钟请求上限，`0` 表示不设置该 Key 的 RPM 限制。

### 2. 查看模型

```bash
export A2A_BASE_URL="http://localhost:8080/v1"
export A2A_API_KEY="sk-a2a-your-key"

curl "$A2A_BASE_URL/models" \
  -H "Authorization: Bearer $A2A_API_KEY"
```

`/v1/models` 只返回当前 Key 有权访问且网关已观察到的模型，不会返回全量模型再交给客户端过滤。

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

流式请求只需将 `stream` 改为 `true`。也可以使用 `route alias` 作为模型值，让网关按照管理面配置的 priority 目标链进行回退。

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

调用方不需要知道渠道内部地址、上游管理令牌、Cookie、OAuth token 或浏览器 profile。请求日志会保留脱敏后的请求元数据和运行结果。

## 管理控制台

控制台使用管理会话 Cookie，API 路径以 `/admin/api` 为前缀。管理登录接口为 `POST /admin/api/auth/login`，会话接口为 `GET /admin/api/auth/session`，退出接口为 `POST /admin/api/auth/logout`。

自动化客户端可以配置格式为 `wbt_*` 的 `A2A_ADMIN_TOKEN`，并使用 `Authorization: Bearer <management-token>` 访问管理 API；该方式直接使用 `admin` 角色，不会创建浏览器 session。

权限规则：

- `admin`：可以写入 Key、模型、路由、渠道、账号、设置和用户，并可以使用调试台。
- `viewer`：只能读取管理数据，不能执行管理写操作。
- 浏览器 session 的管理写请求要求同源校验；使用 `A2A_ADMIN_TOKEN` 的自动化调用通过 Bearer token 认证。登录失败会记录审计并触发限流。
- `GET /admin/api/healthz` 不要求管理登录，可用于容器探针；`?detail=true` 会返回渠道配置状态，但不会主动请求上游平台。

主要管理 API：

| 资源 | API |
| --- | --- |
| 总览、系统和用量 | `/admin/api/overview`、`/admin/api/sysinfo`、`/admin/api/storage/health`、`/admin/api/metrics`、`/admin/api/stats/*` |
| 渠道 | `/admin/api/channels`、`/admin/api/channels/{slug}/test`、`/admin/api/channels/{slug}/runtime` |
| 账号 | `/admin/api/accounts`、`/admin/api/channels/{slug}/provision/*` |
| 模型 | `/admin/api/models`、`/admin/api/models/refresh` |
| Key | `/admin/api/keys` |
| 路由 | `/admin/api/routes` |
| 日志与审计 | `/admin/api/logs`、`/admin/api/audit-logs` |
| 设置与用户 | `/admin/api/settings`、`/admin/api/users` |
| 调试台 | `/admin/api/playground/conversations`、`/admin/api/playground/runs`、`/admin/api/playground/chat` |

完整请求字段、状态码和错误契约以 [API 契约](docs/API契约.md) 及运行中的 OpenAPI 为准。

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
| Doubao | `create-profile` | 创建由 All2API 管理的本地 profile |
| Doubao | `qr-login` | 默认使用本项目的 HTTP QR worker 获取和轮询二维码；必要时可启用 Playwright |
| ChatGPT | `token-import` | 导入 access/refresh/id token 等 token bundle |
| ChatGPT | `oauth-pkce` | 发起 OAuth PKCE，完成 callback 后交换并加密保存 token |

账号 provision 写接口要求管理员角色和 `Idempotency-Key`。过期 session 会被清理，凭据不会出现在账号列表、错误响应、审计日志或前端 DTO 中。旧的 `/admin/api/accounts/sync` 不属于当前接口。

## 数据与安全

- SQLite 默认开启 WAL，并保存渠道覆盖、账号安全快照、加密凭据引用、模型缓存、路由、Key、请求日志、用量、审计和 provision session。
- `A2A_CREDENTIAL_MASTER_KEY` 用于 Fernet 加密账号凭据；数据库本身不保存明文 token、Cookie 或 OAuth secret。
- API Key 只以 hash 形式保存；创建和轮换后的明文只通过一次响应返回。
- 管理面使用签名 session Cookie，支持 idle/absolute session 过期、登录失败限流、Secure/HttpOnly/SameSite Cookie 和同源写保护。
- 渠道配置写接口拒绝 token、password、cookie、authorization、api key 等敏感字段。
- 请求日志和用量支持独立保留周期，清理操作会写入审计日志。
- CORS、可信代理和安全 Cookie 需按实际反向代理拓扑配置，不应直接复制开发环境默认值到公网。

备份时必须同时保护 SQLite 数据和同一份加密主密钥；丢失主密钥后，数据库中的账号凭据无法恢复。

## 开发与测试

### 前端检查

```powershell
pnpm --dir web install --frozen-lockfile
pnpm --dir web lint
pnpm --dir web typecheck
pnpm --dir web build
```

### 后端检查

```powershell
uv sync --project services/api --extra dev
Set-Location services/api
uv run ruff check app tests
uv run pytest -q
Set-Location ..\..
```

### 仓库门禁

```powershell
python scripts/check-doc-links.py
python scripts/verify-clean-build.py
```

`verify-clean-build.py` 会使用随机端口和临时 SQLite，检查运行时代码、配置模板和 Compose 默认值，确认项目不需要源项目目录、源项目端口或默认开启的 legacy bridge。安装 Docker 后可以额外运行：

```powershell
python scripts/verify-clean-build.py --docker
```

CI 还会执行 backend lint/test、frontend lint/typecheck/build、legacy 扫描、Docker Compose 验证和 API 镜像构建。真实平台 E2E 测试默认不会在普通单测中自动运行，需要单独准备平台账号和环境变量。

## 仓库结构

```text
.
├── services/api/              FastAPI 网关、native adapter、SQLite、测试
│   ├── app/adapters/          WorkBuddy、Doubao、ChatGPT 和 registry
│   ├── app/application/       账号和渠道用例服务
│   ├── app/domain/            渠道、scope 等领域规则
│   ├── app/infrastructure/    数据库、凭据、session 和安全实现
│   ├── app/protocols/         Anthropic/Responses 协议转换
│   ├── app/routers/            管理面、Key、模型、路由和网关 API
│   └── tests/                 单元、集成、契约和 E2E 测试
├── web/                       React + TypeScript + Vite 管理控制台
├── scripts/                   文档链接、clean build 和隔离检查
├── docs/                      架构、API、部署、账号和安全文档
├── design/                    UI 参考稿，不是业务事实来源
├── docker-compose.yml         本地 API + Web Compose 拓扑
└── .github/workflows/ci.yml  GitHub Actions 质量门禁
```

## 文档

| 文档 | 用途 |
| --- | --- |
| [API 契约](docs/API契约.md) | 数据面、管理面、认证、错误和分页约定 |
| [账号新增流程](docs/账号新增流程.md) | 三渠道 provision flow、状态机和凭据边界 |
| [密钥与渠道授权规范](docs/密钥与渠道授权规范.md) | Key scope、渠道/模型授权和审计要求 |
| [部署运维与验收](docs/部署运维与验收.md) | 配置、备份、健康检查、clean build 和发布门槛 |
| [系统设计与实现文档](docs/系统设计与实现文档.md) | 模块边界、数据结构和实现细节 |
| [项目开发基线](docs/项目开发基线.md) | 需求追踪、阶段 DoD、测试分层和当前状态 |
| [企业开发规范](docs/企业开发规范.md) | 目录、依赖、迁移、测试和安全门禁 |
| [后端说明](services/api/README.md) | 后端本地启动、native worker 和后端开发规则 |

## 当前限制

1. 三渠道的 native 代码、mock/契约测试和本地状态机已存在，但真实平台网络、账号授权、凭据轮换和平台行为会受官方服务、地区、风控和账号状态影响；本地健康检查不会替代真实平台探测。
2. 当前内置渠道只承诺文本聊天。多媒体路由骨架不等于多媒体功能已经可用，详见[支持范围](#支持范围)。
3. 调试台只允许管理员发起文本请求；未配置 native runtime 时不会回退到 legacy bridge。
4. 用户管理目前维护角色和启用状态，实际登录凭据仍来自 `A2A_ADMIN_USERNAME` / `A2A_ADMIN_PASSWORD`，不是独立的用户密码系统。
5. 项目根目录当前未包含 `LICENSE` 文件，公开发布前请补充明确的许可证和贡献指南。

## 许可证

当前仓库尚未声明开源许可证。除非仓库后续补充 `LICENSE` 并明确授权，否则不要假定代码可以按任意开源许可证使用、修改或再分发。
