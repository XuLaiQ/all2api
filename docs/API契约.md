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

默认本地地址为 `http://localhost:8080/v1`。模型 ID 使用：

- `<channel>/<upstream_model>`：直接指定渠道，例如 `wb/cn:glm-5.2`；
- 管理面配置的 alias：展开为有序渠道/模型目标，并在每个目标上重新执行 Key scope 校验。

调用方不传渠道内部地址、上游管理令牌、Cookie、OAuth token 或浏览器 profile。`GET /v1/models` 只返回当前 Key 授权且 adapter 确认可用的模型。

当前数据面端点：

```text
GET  /v1/models
POST /v1/chat/completions
POST /v1/messages
POST /v1/responses
```

协议能力由渠道 manifest 声明；未声明的图像/视频/音频/文件能力必须返回明确的 `invalid_request_error` 或 `capability_not_supported`，不能静默丢弃字段。

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

数据面错误遵循对应 OpenAI/Anthropic error shape，不套管理 envelope。SSE 已开始后无法修改 HTTP 状态；发送协议错误帧并记录一次失败，不伪造 `[DONE]`。

## 5. 渠道目录和能力

```http
GET /admin/api/channels
GET /admin/api/channels/adapters
GET /admin/api/channels/{channel}/provision-schema
GET /admin/api/channels/{channel}/runtime
POST /admin/api/channels/{channel}/test
```

`channels` 返回 registry/manifest 的安全视图：slug、名称、adapter version、enabled、protocols、capabilities、账号流程摘要、配置状态和健康状态。不得返回 Secret 或内部路径。`/test` 只由用户显式触发，不在页面初次加载时自动探测。

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

### 6.2 迁移兼容路径

当前代码已有：

```text
POST /admin/api/accounts/{channel}/onboarding/start
GET  /admin/api/accounts/{channel}/onboarding/poll
POST /admin/api/accounts/{channel}/onboarding/finish
```

迁移期可以继续提供这些路径，但它们必须成为目标 provision API 的兼容别名，并最终由 registry dispatch；不能继续调用三个源项目的管理 HTTP 接口。字段和状态详见[账号新增流程](账号新增流程.md)。

### 6.3 账号列表/同步

```http
GET  /admin/api/accounts?page=1&page_size=50&channel=wb&status=ready&search=x
POST /admin/api/accounts/sync
```

列表只返回 canonical Account 安全字段和网关运行态。`sync` 是管理员显式触发的 adapter 内部同步，不读取源项目数据库或文件；同步结果不得回传凭据。

## 7. 网关 Key 管理

```http
GET    /admin/api/keys
POST   /admin/api/keys
PATCH  /admin/api/keys/{id}
POST   /admin/api/keys/{id}/rotate
DELETE /admin/api/keys/{id}
```

创建/编辑 body 至少包含：`name`、`channels`、`models`、`expires_at`、`limit_rpm`。`channels=[]` 表示全部当前已启用渠道；模型范围必须与渠道交叉校验。明文只在创建/轮换响应出现一次，响应 `Cache-Control: no-store`，数据库仅存 hash/prefix。

## 8. 路由、模型和日志

```http
GET   /admin/api/models
PATCH /admin/api/models/{model_id}
GET   /admin/api/routes
PUT   /admin/api/routes/{alias}
DELETE /admin/api/routes/{alias}
GET   /admin/api/logs
POST  /admin/api/logs/clear
```

alias target 为 `{channel, model}` 数组，顺序决定优先级。只对 429、连接失败、502/503/504 等可重试错误降级；流开始后不重放。日志至少记录 request_id、key_id、实际 channel/model、fallback_depth、status、error_kind、stream、usage 和 latency；不返回 token、Cookie、平台原始错误体或未脱敏账号标识。

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
| 账号池 | `/accounts`、`/accounts/sync`、`/channels/{channel}/accounts/provision/*` |
| 模型/路由 | `/models`、`/routes` |
| Key | `/keys`、`/keys/{id}/rotate` |
| 日志/审计/用量 | `/logs`、`/audit-logs`、`/stats/*` |

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
reasoning、自定义 text format 与图像/音频/文件输入返回 `invalid_request_error`。流式输出
`response.created`、`response.in_progress`、文本 delta 和完成/失败终态。上述能力需要随着
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

### 12.3 当前只读运维接口

| 接口 | 当前行为 | 迁移要求 |
|---|---|---|
| `GET /admin/api/accounts` | 读取本地账号快照，支持 `channel/status/search` | 改为内置 adapter 同步结果，不能读取源项目文件/DB |
| `POST /admin/api/accounts/sync` | 管理员显式同步账号 | 由内置 adapter 执行，不访问源项目管理 HTTP |
| `GET /admin/api/models` | 读取已观测模型缓存 | manifest/model mapper 持续维护缓存语义 |
| `GET /admin/api/channels` | 读取当前 registry/config，不自动探测 | 升级为 manifest 安全视图和 provision schema 入口 |
| `GET /admin/api/logs` | 服务端分页、请求/渠道/模型/状态/时间筛选 | 保留脱敏，不泄露 token、账号原值、IP/UA |
| `GET /admin/api/metrics` | 读取本地请求/账号运行态，不主动探测 | 区分 adapter/config/platform/worker 健康 |

管理页不能通过初次加载触发模型发现、账号同步或真实平台登录。所有兼容路径都必须在 M5
切换前迁移到目标 adapter/provisioner，或明确删除。
