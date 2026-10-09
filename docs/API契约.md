# API 契约

本文是前后端共同遵循的 HTTP 契约。目标是让 All2API 成为一个独立交付物：调用方只面对 All2API 的 URL 和 Key；渠道 adapter/provisioner 在服务端执行平台私有逻辑。三个源项目不出现在目标态 API 的服务边界中。

## 1. 服务边界和认证

| 前缀 | 用途 | 调用方 | 认证 |
|---|---|---|---|
| `/admin/api/*` | 管理控制台、运维和账号流程 | `web/`、管理脚本 | HttpOnly Session Cookie 或管理令牌 |
| `/v1/*` | OpenAI/Anthropic/Responses 兼容数据面 | 外部客户端 | `Authorization: Bearer sk-a2a-*` 或协议允许的 `x-api-key` |

管理 Cookie 不能调用数据面；Gateway Key 不能调用管理面。管理面写操作按角色校验：`admin` 可写，`viewer` 只读（显式上游探测、账号授权、Key 管理、清日志等均视为写/副作用操作）。

## 2. 统一调用地址和模型

```http
GET  https://gateway.example/v1/models
POST https://gateway.example/v1/chat/completions
Authorization: Bearer sk-a2a-<gateway-issued-key>
```

默认本地地址为 `http://localhost:8888/v1`。模型 ID 使用：

- `<channel>/<upstream_model>`：直接指定渠道，例如 `wb/cn:glm-5.2`；
- 管理面配置的 alias：展开为有序渠道/模型目标，并在每个目标上重新执行 Key scope 校验。

调用方不传渠道内部地址、上游管理令牌、Cookie、OAuth token 或浏览器 profile。`GET /v1/models` 只返回当前 Key 授权且 adapter 确认可用的模型。

同一地址和 Key 的最小调用示例（模型前缀决定渠道）：

```powershell
curl "$BASE_URL/v1/chat/completions" -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" `
  -d '{"model":"wb/cn:glm-5.2","messages":[{"role":"user","content":"hello"}]}'
curl "$BASE_URL/v1/chat/completions" -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" `
  -d '{"model":"doubao/doubao-pro","messages":[{"role":"user","content":"hello"}]}'
curl "$BASE_URL/v1/chat/completions" -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" `
  -d '{"model":"chatgpt/gpt-4o-mini","messages":[{"role":"user","content":"hello"}]}'
```

当前数据面端点：

```text
GET  /v1/models
POST /v1/chat/completions
POST /v1/messages
POST /v1/responses
POST /v1/images/generations
POST /v1/video/generations
POST /v1/audio/generations
POST /v1/search
```

协议能力由渠道 manifest 声明；未声明的图像/视频/音频/文件能力必须返回明确的 `invalid_request_error` 或 `capability_not_supported`，不能静默丢弃字段。

多媒体和搜索请求统一使用 JSON body，并要求 `model`（搜索可省略，由渠道使用
`chatgpt/auto` 默认选择）。`model` 的渠道前缀仍参与 Key scope、渠道启用状态、账号租约、
fallback、请求日志和用量统计。路由层只把请求发送到同时声明对应 capability 的渠道：

- `image` -> `/v1/images/generations`；
- `video` -> `/v1/video/generations`；
- `audio` -> `/v1/audio/generations`；
- `search` -> `/v1/search`。

这些通用路由当前为非流式 JSON dispatch；请求 `stream=true` 返回
`invalid_request_error`。图片/视频成功结果会由 Go media store 归一化为本地 asset，
并在网关响应中返回 `/v1/files/{file_id}/content`；无法下载的签名 URL 仍以 remote
asset metadata 保留。图像编辑、PPT/PSD 和可编辑文件任务仍受 manifest gate，未声明
时返回 `capability_not_supported`，不会把 provider-specific 协议伪装成通用结果。

`POST /v1/files`、`GET /v1/files`、`GET/DELETE /v1/files/{file_id}` 和
`GET /v1/files/{file_id}/content` 由 Go 本地 media store 提供。上传使用 multipart
字段 `file`，可选 `purpose`；文件按网关 Key 隔离，响应不返回服务器路径，内容下载
仍需同一 Key。单文件上限为 100 MB，provider 文件处理和多模态输入不由这些本地接口
自动推断。

Go media worker 的 `POST /v1/watermark/remove` 接受 `{ "data_base64": "..." }`，返回
PNG base64、尺寸和 `method=go_region_repair`。该接口是本地确定性区域修复，不代表
已通过任一 provider 的真实去水印质量验收。

## 3. 通用格式

- JSON 使用 UTF-8 和 `snake_case`；时间为 UTC Unix 秒或 RFC 3339 UTC；
- 每个响应带 `X-Request-ID`，客户端可提交合法 request id；
- 管理成功响应为 `{ "data": ... }`；分页为 `{data, pagination}`；
- 创建返回 `201`，无响应体成功返回 `204`；
- 管理错误固定为：

```json
{
  "error": {
    "code": "validation_error",
    "message": "请求参数不合法",
    "details": {},
    "request_id": "req_01..."
  }
}
```

`details` 不得包含 traceback、Secret、token、Cookie、绝对路径或平台原始错误体。

## 4. 数据面授权和错误

请求执行顺序：解析 Key → 校验 enabled/expiry/RPM → 规范化 model/alias → 校验 channel/model scope → scheduler → adapter。Key scope 规则见[密钥与渠道授权规范](密钥与渠道授权规范.md)。

统一错误建议：

| HTTP | code | 语义 |
|---|---|---|
| 400 | `invalid_request` | 请求结构或参数错误 |
| 401 | `invalid_api_key` | Key 缺失/无效 |
| 403 | `channel_not_allowed` / `model_not_allowed` | Key 未授权，不尝试目标 |
| 404 | `model_not_found` / `channel_not_found` | 目标不存在 |
| 409 | `account_unavailable` / `state_conflict` | 账号或流程状态冲突 |
| 429 | `rate_limited` | Key、渠道或平台限流 |
| 502 | `adapter_error` / `protocol_error` | adapter 或平台响应不可解析 |
| 503 | `channel_disabled` / `adapter_unavailable` | 本地配置/worker 不可用 |
| 504 | `upstream_timeout` | 平台请求超时 |

数据面错误遵循对应 OpenAI/Anthropic error shape，不套管理 envelope。SSE 已开始后无法修改 HTTP 状态；公共 Chat Completions、Anthropic Messages 和 Responses 均逐步输出 delta，发送协议错误帧并记录一次失败，不伪造 `[DONE]`。

## 5. 渠道目录和能力

```http
GET /admin/api/channels
GET /admin/api/channels/adapters
GET /admin/api/channels/{channel}/provision-schema
GET /admin/api/channels/{channel}/runtime
POST /admin/api/channels/{channel}/test
POST /admin/api/channels
PATCH /admin/api/channels/{channel}
DELETE /admin/api/channels/{channel}
```

`channels` 返回 registry/manifest 的安全视图：slug、名称、adapter version、enabled、protocols、capabilities、账号流程摘要、legacy `accounts_configured`、native `provision_configured` 和健康状态。不得返回 Secret 或内部路径。`/test` 只由用户显式触发，不在页面初次加载时自动探测。

渠道写接口只允许已注册的内置渠道，保存 SQLite 中的本地启用覆盖和非敏感配置；不动态加载 provider、不写入环境配置，也不接受 token、Cookie、password、authorization、api_key 等字段。`DELETE` 删除本地覆盖并恢复 registry 默认值。写入要求 `admin` 角色和 `Origin` 与 `Host` 同源校验，并记录审计。当前启用覆盖不会重建进程内 adapter；数据面强制停用仍需后续 runtime integration 完成，接口会明确返回本地管理状态。

## 6. 账号新增和渠道 dispatch

### 6.1 目标 API

```http
POST /admin/api/channels/{channel}/accounts/provision/start
GET  /admin/api/channels/{channel}/accounts/provision/{session_id}
POST /admin/api/channels/{channel}/accounts/provision/{session_id}/complete
POST /admin/api/channels/{channel}/accounts/provision/import
POST /admin/api/channels/{channel}/accounts/provision/{session_id}/cancel
```

body：

```json
{
  "flow": "oauth-pkce",
  "payload": {},
  "idempotency_key": "acc_01J..."
}
```

接口只做：校验 channel/flow/schema/权限/幂等、调用 registry 中的 `account_provisioner`、持久化脱敏 session、写审计和归一化账号。禁止在 router 里按平台写 `if/elif`。

`provision/import` 只接收管理员在本次新增流程中主动提交的账号材料（例如 ChatGPT token
三件套）；它不从任何外部项目读取或同步账号列表。旧的 `POST /admin/api/accounts/sync` 已
删除，客户端不得依赖该路径。

### 6.2 迁移兼容路径

当前代码已有：

```text
POST /admin/api/accounts/{channel}/onboarding/start
GET  /admin/api/accounts/{channel}/onboarding/poll
POST /admin/api/accounts/{channel}/onboarding/finish
```

迁移期可以继续提供这些路径，但它们必须成为目标 provision API 的兼容别名，并最终由 registry dispatch；不能继续调用三个源项目的管理 HTTP 接口。字段和状态详见[账号新增流程](账号新增流程.md)。

### 6.3 账号列表

```http
GET  /admin/api/accounts?page=1&page_size=50&channel=wb&status=ready&search=x
POST /admin/api/accounts/{account_id}/refresh
```

列表只返回 canonical Account 安全字段和网关运行态。账号新增由渠道 provision API 完成并直接写入本地账号表；该接口不会读取源项目数据库、文件或管理端口，也不会触发外部账号同步。
`POST /admin/api/accounts/{account_id}/refresh` 仅允许管理员调用。支持 token 轮换的渠道调用
对应 provisioner 的 `refresh_credential`，只返回脱敏账号摘要并写入 `refresh_account` 审计事件。
Doubao 的 Cookie/browser session 没有独立 token refresh endpoint；该渠道返回明确的
`capability_not_supported`，管理员应重新导入 Cookie 或重新执行 QR/profile 授权，不得伪造刷新成功。

## 7. 网关 Key 管理

```http
GET    /admin/api/keys
POST   /admin/api/keys
PATCH  /admin/api/keys/{id}
POST   /admin/api/keys/{id}/rotate
DELETE /admin/api/keys/{id}
```

创建/编辑 body 至少包含：`name`、`channels`、`models`、`expires_at`、`limit_rpm`。`channels=[]` 表示全部当前已启用且已配置渠道；模型范围必须与渠道交叉校验。创建/轮换响应和管理员 Key 列表均使用 `Cache-Control: no-store`；数据库使用 `key_hash` 鉴权，并以服务主密钥加密保存当前 Key 以支持管理员列表脱敏展示和复制，禁止保存未加密明文。viewer 列表只返回 prefix 和安全元数据，历史上没有加密密文的旧 Key 需要轮换后才能复制。

创建 Key 的最小请求：

```json
{
  "name": "team-a",
  "channels": ["wb", "chatgpt"],
  "models": ["wb/*", "chatgpt/gpt-4o-mini"],
  "limit_rpm": 60,
  "expires_at": null
}
```

验收要求：Key 只授权 `wb` 时调用 `doubao/*` 必须返回 `403 channel_not_allowed`；渠道已注册但被禁用时返回 `503 channel_disabled`；alias 展开后不能绕过这两项检查。上面的通道前缀和通配符属于目标契约；迁移期间若当前 validator 只接受 `models:["*"]`，必须先更新 OpenAPI、后端校验和前端 DTO，再启用按渠道模型列表。

## 8. 路由、模型和日志

```http
GET   /admin/api/models
PATCH /admin/api/models/{model_id}
GET   /admin/api/routes
PUT   /admin/api/routes/{alias}
DELETE /admin/api/routes/{alias}
GET   /admin/api/logs
POST  /admin/api/logs/clear
GET   /admin/api/settings
POST  /admin/api/settings
GET   /admin/api/users
POST  /admin/api/users
PATCH /admin/api/users/{name}
DELETE /admin/api/users/{name}
GET   /admin/api/playground/conversations
GET   /admin/api/playground/conversations/{id}
DELETE /admin/api/playground/conversations/{id}
POST  /admin/api/playground/chat
GET   /admin/api/playground/runs
POST  /admin/api/channels
PATCH /admin/api/channels/{slug}
DELETE /admin/api/channels/{slug}
```

alias target 为 `{channel, model}` 数组，顺序决定优先级。只对 429、连接失败、502/503/504 等可重试错误降级；流开始后不重放。日志至少记录 request_id、key_id、实际 channel/model、fallback_depth、status、error_kind、stream、usage 和 latency；不返回 token、Cookie、平台原始错误体或未脱敏账号标识。

Go gateway 已支持使用不带 `/` 的 route alias 作为 `model`：启用 alias 按 priority target
顺序解析；公共 `/v1` 和 Playground 流式请求仅在首个 target 输出前遇到可重试错误时回退，
输出开始后不重放，并将 alias/upstream model/fallback depth 写入请求日志。真实渠道 E2E
前不会扩大 manifest 能力。

`GET /admin/api/settings` 返回可安全展示的运行时设置及其来源；当前支持
`log_retention_days` 与 `usage_retention_days`。`POST` 只允许 `admin` 角色写入，值限制为
1-36500 天，且用量保留期不得短于日志保留期。配置持久化在本地 SQLite `settings` 表，
更新立即用于日志清理，并写入 `audit_logs`；环境变量、凭据、URL 和会话密钥不通过此接口
读取或修改。

用户 API 只维护本地管理目录和角色/启停状态，不存储密码；当前认证仍由环境管理员账号和
会话系统提供。渠道写 API 只允许内置 registry 中已存在的渠道，配置内容仅接受非 Secret
的 JSON 标量/对象，并写入审计；动态 provider 注册不在当前范围内。

Playground 支持管理员发起的文本流式/非流式请求，以及受 manifest gate 保护的图片、视频、
搜索和 editable-file 路由；所有请求调用 native runtime，不调用 legacy bridge。文本回答、
生成响应和运行元数据分别持久化到会话消息、media_assets 和 `playground_runs`。
editable-file 生成结果使用 Go media asset content URL；旧的
`/admin/api/playground/files/{task_id}/{filename}` 下载路径明确返回
`capability_not_supported`，不会恢复 Python 文件目录依赖。

## 9. 会话、幂等和安全

- `POST /admin/api/auth/login`、`GET /admin/api/auth/session`、`POST /admin/api/auth/logout` 管理会话；Cookie 使用 HttpOnly/SameSite/Secure 策略；
- Cookie 写请求校验 Origin/Host 和 CSRF；脚本管理令牌只供服务端；
- GET 可有限重试；写操作默认不自动重试，必须依赖幂等键或资源版本；
- provision start/import/complete/cancel 必须有幂等键和 TTL；重复请求返回同一结果或明确 `409`；
- 所有写操作审计 actor/action/target/result，审计摘要不含 Secret。

## 10. 页面 API 映射

| 页面 | 主要 API |
|---|---|
| 登录 | `/auth/login`、`/auth/session`、`/auth/logout` |
| 渠道 | `/channels`、`/channels/adapters`、`/channels/{channel}/provision-schema`、`/channels/{channel}/test` |
| 账号池 | `/accounts`、`/channels/{channel}/accounts/provision/*` |
| 模型/路由 | `/models`、`/routes` |
| Key | `/keys`、`/keys/{id}/rotate` |
| 日志/审计/用量 | `/logs`、`/audit-logs`、`/stats/*` |
| 系统设置 | `/settings` |
| 用户管理 | `/users` |
| 调试台 | `/playground/conversations`、`/playground/chat`、`/playground/runs` |

前端类型必须从 OpenAPI 或同一变更中的 DTO 同步，不在组件中复制渠道枚举、授权规则或调度逻辑。

## 11. 契约交付和兼容性

FastAPI OpenAPI 是机器可读来源。改变字段、状态、错误码、scope 语义或 onboarding 路径时，必须同时更新：OpenAPI、本文、前端类型、适配器契约测试、迁移说明和兼容性窗口。新平台不得通过修改通用 API 增加平台专用端点，除非先记录 ADR 并证明无法用 manifest/provision schema 表达。

## 12. 当前 bridge 兼容契约（迁移期间）

本节只记录当前代码已经提供的兼容行为，方便迁移时做回归测试；它不表示这些实现满足
内置渠道目标。接口/字段以运行中的 FastAPI OpenAPI 为准。

### 12.1 Messages 与 Responses

`POST /v1/messages` 接受 Bearer 或 `x-api-key`，要求 `model`、正整数 `max_tokens` 和非空
`messages`。当前支持文本 `system`、`temperature`、`top_p`、`stop_sequences`、`stream`、
`metadata.user_id`、function `tools` 和 `tool_choice`；assistant `tool_use` 与 user
`tool_result` 映射为 OpenAI function calls。图像/多模态内容、错误 `tool_result` 与未知字段
返回 Anthropic `invalid_request_error`，不会静默丢弃。SSE 输出 Anthropic message/content
block 事件；断流输出 `error` 后以 `message_stop` 收尾。未报告 token usage 时 wire 值为 0，
但管理统计必须标记为 usage unknown。

`POST /v1/responses` 当前支持纯文本 `input`（字符串或 system/developer/user/assistant
消息数组）、`instructions`、`max_output_tokens`、`temperature`、`top_p`、`stream`、
string-valued `metadata` 和 `store:false`。tools、previous response continuation、background、
reasoning、自定义 text format 与图像/音频/文件输入返回 `invalid_request_error`。`stream=true`
通过 gateway stream port 逐步输出文本 delta；上游在首个 delta 后中断时输出协议 error
终态，不伪造成功完成。流式输出包含 `response.created`、`response.in_progress`、文本 delta
和完成/失败终态。上述能力需要随着
内置 adapter 的 manifest 重新验证，而不是由 bridge 自动继承。

### 12.2 管理会话与安全

当前会话端点为：

| 方法 | 路径 | 行为 |
|---|---|---|
| `POST` | `/admin/api/auth/login` | 校验用户名/密码，设置 HttpOnly Session Cookie |
| `GET` | `/admin/api/auth/session` | 返回当前用户、角色和会话到期时间 |
| `POST` | `/admin/api/auth/logout` | 撤销当前会话并清 Cookie，返回 `204` |

Cookie 使用 `HttpOnly`、`SameSite=Lax`，HTTPS 按配置启用 `Secure`，作用路径为 `/admin/api`。
Cookie 写操作校验 `Origin` 与 `Host`；脚本 Bearer 管理令牌不进入浏览器且不使用 Cookie CSRF
逻辑。`A2A_TRUSTED_PROXIES` 仅允许直接连接 API 的代理 IP/CIDR；不可信连接提交的
`X-Forwarded-*` 必须忽略。管理员密码至少 12 字符，`A2A_SESSION_SECRET` 至少 32 字符且
生产不得使用默认值。

### 12.3 当前运维与管理接口

| 接口 | 当前行为 | 迁移要求 |
|---|---|---|
| `GET /admin/api/accounts` | 读取本地账号快照，支持 `channel/status/search` | 账号仅由本项目的 provision 流程写入，不能读取源项目文件/DB |
| `GET /admin/api/models` | 读取已观测模型缓存 | manifest/model mapper 持续维护缓存语义 |
| `GET /admin/api/channels` | 读取当前 registry/config，不自动探测 | 升级为 manifest 安全视图和 provision schema 入口 |
| `GET /admin/api/logs` | 服务端分页、request id/渠道/模型/状态/error kind/stream/RFC3339 时间筛选，返回 upstream model、route alias、fallback depth、TTFT | 保留脱敏，不泄露 token、账号原值、IP/UA |
| `GET /admin/api/metrics` | 读取本地请求/账号运行态，不主动探测 | 区分 adapter/config/platform/worker 健康 |
| `GET /admin/api/settings` | 读取 retention 设置及来源 | 仅暴露白名单设置，不读取 Secret |
| `POST /admin/api/settings` | 管理员更新 retention 设置并写审计 | 写操作要求 `admin` 和同源校验 |
| `GET /admin/api/users` | 读取本地用户目录 | 用户目录不保存密码；实际登录仍使用环境管理员配置 |
| `POST /admin/api/users` | 创建 viewer/admin 目录项 | 仅 admin + 同源；不创建可登录密码 |
| `PATCH/DELETE /admin/api/users/{name}` | 修改启用/角色或删除目录项 | 环境配置管理员不可删除或停用；写审计 |
| `GET/DELETE /admin/api/playground/conversations/{id}` | 读取或删除对话及其消息、请求记录 | 仅 admin 可删除；删除在同一事务内清理关联记录 |
| `GET /admin/api/playground/runs` | 读取 Playground 运行元数据 | 不保存消息正文、凭据或上游原始错误体 |
| `POST /admin/api/playground/chat` | 对已配置 native runtime 执行一次流式或非流式调试调用 | 仅 admin + 同源；未配置 runtime 返回 `501`，不会回退到源项目 bridge |

管理页不能通过初次加载触发模型发现或真实平台登录。所有兼容路径都必须在 M5
切换前迁移到目标 adapter/provisioner，或明确删除。
