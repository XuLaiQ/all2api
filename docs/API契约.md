# API 契约

本文规定前后端共同遵循的 HTTP 约定。业务端点清单、领域字段和数据关系以[系统设计与实现文档](系统设计与实现文档.md)为准；实现中如需改变契约，先更新本文和 OpenAPI，再改调用方。

## 服务边界

| 前缀 | 用途 | 调用方 | 认证 |
|---|---|---|---|
| `/admin/api/*` | 管理控制台与管理自动化 | `web/`、管理脚本 | Session Cookie 或管理令牌 `wbt_` |
| `/v1/*` | OpenAI/Anthropic 兼容数据面 | 外部 API 客户端 | `Authorization: Bearer sk-a2a-*` |

两类密钥不可互换。浏览器只使用管理会话 Cookie；数据面密钥只在创建时返回明文一次，前端不得持久化。

## 统一调用地址与 API Key

所有调用方配置同一个 Gateway Base URL，默认 `http://localhost:8080/v1`，并使用由 All2API 管理面创建的 `sk-a2a-*` Key：

```http
GET  http://localhost:8080/v1/models
POST http://localhost:8080/v1/chat/completions
Authorization: Bearer sk-a2a-<gateway-issued-key>
```

模型 ID 使用 `渠道 slug/上游模型 ID` 直达单一渠道，或使用管理面配置的别名由路由规则选渠道。调用方不传 WorkBuddy/doubao/chatgpt 的上游地址、Cookie 或上游 Key；适配器在服务端访问这三个既有反代项目。初版验收要求同一个 Base URL 和网关 Key 能按授权访问三家渠道的可用模型。

当前可调用的数据面包括 `GET /v1/models`、`POST /v1/chat/completions`、`POST /v1/messages` 和文本切片 `POST /v1/responses`（非流式与 SSE 流式）。Messages 使用 `x-api-key` 或兼容的 Bearer Key，要求 `model`、正整数 `max_tokens` 与非空 `messages`，可带文本 `system`、`temperature`、`top_p`、`stop_sequences`、`stream`、`metadata.user_id`、function `tools` 与 `tool_choice`。assistant `tool_use` 和 user `tool_result` 会映射到 OpenAI function calls；图像/多模态内容、错误 tool_result、未知字段返回 Anthropic `invalid_request_error`，不会静默丢弃。SSE 输出 Anthropic message 与 content block 事件；流中断输出 `error` 后以 `message_stop` 收尾。若上游不提供 Token 用量，Messages 响应字段按协议以 0 填充；管理统计会将该请求标为用量未知，0 不是估值。

Responses 首版支持 `model` 与纯文本 `input`（字符串，或 system/developer/user/assistant 消息数组），可带 `instructions`、`max_output_tokens`、`temperature`、`top_p`、`stream`、string-valued `metadata` 和 `store:false`。tools、previous response continuation、background、reasoning、自定义 text format 与图像/音频/文件输入返回标准 OpenAI `invalid_request_error`。非流式返回 `response` 对象；流式输出 `response.created` / `response.in_progress`、文本 delta 与完成或失败终态。上游未报告 Token 时 wire usage 以 0 填充，管理统计保留未知标记。图像、视频、音频、检索和文件生成端点仍未实现。

首版统一入口验收方式：

1. 管理面分别配置三家现有反代的 upstream URL 与各自管理凭据；调用侧不得取得这些配置。
2. `GET /v1/models` 返回密钥授权范围内、由适配器确认可用的模型。
3. 同一个 Gateway Base URL 与同一把被授权的网关 Key，使用 `wb/<model>`、`doubao/<model>`、`chatgpt/<model>` 或配置别名分别调用成功。
4. 每个请求日志记录实际命中的渠道、模型、状态和 request ID，便于确认路由结果。

## 通用格式

- UTF-8 JSON；字段使用 `snake_case`。
- 数据库时间戳统一使用 UTC Unix 秒；HTTP 时间统一序列化为 RFC 3339 UTC，例如 `2026-09-25T08:00:00Z`。时长字段明确使用 `*_ms` 或 `*_seconds` 后缀。
- 每个响应带 `X-Request-ID`。客户端可提交该 header；服务端校验格式并回传，用于日志排查。
- JSON 列表使用 `page`（默认 `1`）与 `page_size`（默认 `50`，最大 `200`），返回稳定排序后的结果。
- 管理面成功响应统一为 `{ "data": ... }`。分页列表形态为 `{ "data": [...], "pagination": { "page": 1, "page_size": 50, "total": 0, "total_pages": 0 } }`。
- 创建资源返回 `201`；成功但无响应体返回 `204`。资源缺失返回 `404`，版本/状态冲突返回 `409`。

## 管理面错误

错误体固定为：

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

`details` 可为空对象；不得把 traceback、Secret、上游凭据或内部文件路径放入响应。

| HTTP | 含义 | 前端行为 |
|---|---|---|
| `400` | 业务参数错误 | 显示字段/操作错误，不自动重试 |
| `401` | 会话缺失或过期 | 清理内存身份状态并转登录页 |
| `403` | 角色或资源权限不足 | 显示无权限状态，不重试 |
| `404` | 资源不存在 | 更新列表或展示资源已移除 |
| `409` | 重复创建、状态冲突、版本冲突 | 提示刷新并重新确认 |
| `422` | 请求 schema 校验失败 | 映射 `details` 到对应字段 |
| `429` | 限流 | 尊重 `Retry-After`，用户可见倒计时 |
| `500` | 未处理服务错误 | 显示 request ID，允许人工重试 |
| `502` / `503` / `504` | 上游错误、服务不可用或超时 | 保留输入，显示恢复操作；写请求不自动重试 |

数据面 `/v1` 错误遵循系统设计 §8.3 的 OpenAI `error` 结构，不套用管理面 envelope。
若 SSE 响应头已发送后上游断流，HTTP 状态无法再改写；网关以 `data: {"error": ...}` 终止流，不发送伪造的 `[DONE]`，并在请求日志中记录 502。

## 管理会话

系统设计 §9.1 的认证方案需要以下端点，作为管理面端点清单的补充：

| 方法 | 路径 | 行为 |
|---|---|---|
| `POST` | `/admin/api/auth/login` | 校验用户名/密码；成功设置 HttpOnly Session Cookie |
| `GET` | `/admin/api/auth/session` | 返回当前用户、角色和会话到期时间 |
| `POST` | `/admin/api/auth/logout` | 撤销当前会话并清除 Cookie，返回 `204` |

Cookie 使用 `HttpOnly`、`SameSite=Lax`、按 HTTPS 配置 `Secure`，作用路径限定为 `/admin/api`。Cookie 是签名会话，不是业务数据。登录与 Cookie 会话认证的非安全方法校验 `Origin` 与 `Host`；不使用 Cookie 的脚本 Bearer 请求不执行 CSRF 检查。CORS 仅允许显式列出的源并启用 credentials。登录按 IP 和用户名双维度限速。管理令牌只供服务端脚本/CI 使用，不进入浏览器。

经反向代理部署时，应设置 `A2A_TRUSTED_PROXIES` 为直接连接 API 的代理 IP/CIDR，并确保代理覆盖客户端提交的 `X-Forwarded-For` 与 `X-Forwarded-Proto`。只有 TCP 对端匹配该列表时才采信这些头；`A2A_TRUST_PROXY=false` 时完全忽略转发头。代理应保留原始外部 `Host`，使其与浏览器 `Origin` authority 一致。不要把公网地址或任意来源加入可信代理列表。

首个管理员由 API 环境变量 `A2A_ADMIN_USERNAME` 与 `A2A_ADMIN_PASSWORD` 配置；密码至少 12 字符，签名密钥 `A2A_SESSION_SECRET` 至少 32 字符且不得使用默认值。会话同时受总寿命与空闲时限约束，登出会撤销当前服务进程内的会话；API 进程重启后旧会话失效。

角色固定为 `admin` 与 `viewer`。`viewer` 只能读；任何更改、探测、登录上游、重置用量和清日志等有副作用的操作均要求 `admin`。后端每次请求执行授权，前端隐藏控件只是辅助体验。

## 幂等与重试

- GET/HEAD 可由客户端按网络错误策略有限重试。
- POST/PATCH/DELETE 默认不自动重试。用户确认重试前，前端必须能说明操作结果未知的风险。
- 账号批量探测按 `ids[]` 去重，并对每个 ID 返回独立结果；不得把一次批量动作实现成单账号探测。
- 创建密钥等只返回一次明文的操作必须明确展示结果，关闭后不再提供明文读取。
- 状态型写操作应验证当前资源状态；发生并发冲突返回 `409`，不静默覆盖。

## 页面与 API 映射

页面字段以响应 DTO 为准；不在前端复制调度、额度或权限判定。

| 页面/区域 | 首要 API |
|---|---|
| 登录 / 会话 | `/auth/login`、`/auth/session`、`/auth/logout` |
| 运行总览 | `/overview`、`/stats/summary`、`/logs` |
| 渠道 | `/channels`、`/channels/adapters`、`POST /channels/{slug}/test` |
| 账号池 | `/accounts`（本地快照）、`POST /accounts/sync`（管理员显式上游同步） |
| 模型 / 路由 | `/models`、`PATCH /models/{model_id}`、`/routes` |
| 密钥 | `/keys`、`/keys/{id}/rotate` |
| 请求日志 / 用量 | `/logs`、`POST /logs/clear`、`/stats/*` |
| 调试台 | `/playground/chat` |
| 系统设置 | `/settings`、`/users`、`/audit-logs`、`/sysinfo`、`/storage/health`、`/metrics` |

审计日志：`GET /admin/api/audit-logs` 供 admin/viewer 读取审计摘要，支持 `page`、`page_size`（最大 200）、`actor`、`action`、`target` 以及 RFC 3339 UTC 半开时间筛选。结果按 `ts DESC, id DESC` 稳定排序，返回 `id`、时间、actor、action、target、detail 和统一分页 envelope；不返回来源 IP。该端点只读，不改变任何写操作权限。

系统信息：`GET /admin/api/sysinfo` 返回服务版本、Python 运行时、SQLite schema 版本、数据库是否存在/大小、运行态文件是否初始化及各渠道是否完成模型/账号配置；不返回绝对路径、环境变量或 Secret。存储健康：`GET /admin/api/storage/health` 检查 SQLite 可读性、运行态文件状态和数据库所在磁盘容量，返回 `status` 为 `ok` 或 `degraded`；运行态文件状态可为 `not_initialized`、`ok`、`invalid` 或 `unreadable`，未初始化不单独导致失败。

指标：`GET /admin/api/metrics` 接受 `days`（1-366，默认 1），只读取网关本地 `request_logs`、账号快照和运行态表，返回请求数、错误数/比例、流式请求数、平均/P95 延迟、按渠道聚合，以及账号总数/启用数/可用数/已观测运行态数。该端点不探测上游、不返回请求明细、账号标识或 IP；空数据返回零值和空渠道列表。

渠道测试：`POST /admin/api/channels/{slug}/test` 由 admin/viewer 显式触发一次模型目录请求，不写模型缓存、运行态或审计日志。未知渠道返回 `404`，未配置渠道返回 `409`，上游超时返回 `504`，其他上游/适配器错误返回 `502` 且不透传原始错误体；成功响应包含 `channel`、`status`、`latency_ms`、`model_count` 和 UTC `tested_at`。

运行总览：`GET /admin/api/overview` 接受 `days`（1-366，默认 30），单次返回 `summary`（同 `/stats/summary`）、`daily`（同 `/stats/daily`）、`channels`（同 `/channels`）与 `todos`。待办只由本地已配置状态和渠道熔断/冷却状态生成，包含 `id`、`code`、`severity`、`channel`、`title`、`description`、`href`；此接口不探测上游。空数据库时统计为零、每日记录为空，渠道与待办仍按本地注册表和配置返回。

账号池：`GET /admin/api/accounts` 只读已同步的本地账号快照，支持分页及 channel/status/search 筛选；不请求上游，不返回 native ID、错误原文或 ext。`gateway_runtime` 是网关独立计数，仅在请求 ID 与 WorkBuddy 回传账号 ID 均通过校验后更新，包含 `state`、成功/失败数、连续失败数、冷却/熔断截止、最近状态与更新时间；未观测账号为 `unobserved`。该运行态保存在 SQLite，重启后保留，并参与 WorkBuddy 的候选过滤。配置共享追踪密钥且已有账号同步快照时，网关最多展开 3 个最近较少使用的 WB 候选，每个 API 进程每账号最多 1 个在途租约；上游只接受经 `X-A2A-Trace-Secret` 与 `X-A2A-Request-ID` 校验的 `X-A2A-Account-ID: <realm>:<uid>`，对不健康、模型受限或满载账号返回 `409 account_unavailable`，不会静默换号。成功或失败响应的可信账号 ID 必须与目标一致，否则网关返回 502。没有本地 WB 快照时为兼容旧部署保留上游自行选号。管理员显式调用 `POST /admin/api/accounts/sync` 时，网关才并发请求已配置上游账号接口、更新上游快照并记录审计摘要；viewer 返回 403。Doubao/ChatGPT 尚无可信逐请求账号选择/身份契约，其 `gateway_runtime` 保持 `unobserved` 且不参与选号。额度展示必须按 `quota_unit` 解释，例如 `credits_remaining` 是剩余 credits，不是已用额度；`none` 表示不适用/不可比较。

模型目录：`GET /admin/api/models` 只读网关已观测模型缓存，不请求上游；数据可能不完整或陈旧。`PATCH /admin/api/models/{model_id}`（model_id 可含 `/`）仅允许 admin 修改 `enabled` 并写审计日志。禁用状态由模型发现刷新保留，同时从 `GET /v1/models` 隐藏，直连聊天返回 404，别名路由跳过该目标。缺失模型表示尚未观测，不表示上游不可用。模型刷新只会在调用方显式请求 `GET /v1/models` 时发生，管理页挂载不触发刷新。

渠道目录：`GET /admin/api/channels` 与 `/channels/adapters` 读取本地注册表和配置，不会探测上游；`enabled` 表示数据面配置齐全，不等同于上游在线。`GET /admin/api/channels/{slug}/runtime` 只读本地 SQLite，返回该渠道与模型级冷却/熔断状态、截止时间、最近 HTTP 状态与失败分类；无记录表示暂无网关运行数据，不代表上游探活成功。模型发现 `/v1/models` 会请求上游，管理页面不得在渠道目录初次加载时自动调用。

用量统计端点为 `/admin/api/stats/summary`、`/daily`、`/by-channel`、`/by-model` 和 `/by-key`；可传 `days`（1-366，默认 30），日期按 UTC 记账日期计算，汇总包含 requests、已报告 Token 数和 `usage_unknown_requests`。网关被动解析完整的 OpenAI SSE usage 帧，不改写转发字节；上游未报告 usage 的流式或非流式请求会计入未知用量。当前没有可信 credits/价格来源，响应中的 `credits` 为 `null` 且 `credits_available` 为 `false`。

请求日志：`GET /admin/api/logs` 支持 `page` / `page_size`（最大 200）、`request_id`、`channel`、`model`（包含匹配）、`status`、`error_kind`、`key_id`、`stream`、`from` / `to`（RFC 3339 UTC 半开区间）。结果按 `ts DESC, id DESC` 稳定排序，返回统一分页 envelope。日志列表只暴露排障必要字段；不返回上游原始错误体、账号 ID、IP 或 User-Agent。

日志清理：`POST /admin/api/logs/clear` 仅 admin 可调用，不接受自定义 cutoff。它使用 `A2A_LOG_RETENTION_DAYS`（默认 30）删除早于当前时间减去 N×24 小时的 `request_logs`，使用 `A2A_USAGE_RETENTION_DAYS`（默认 365）删除早于最近 N 个 UTC 日（含今天）的 `usage_daily`；两个删除和一条 `clear_logs` 审计记录在同一事务中完成。清理依赖请求写入时已完成的 `usage_daily` 聚合，不从旧明细回填，避免重复计量；重复调用是幂等的。响应返回删除行数和两个 cutoff，不返回明细、IP 或 Secret。

网关密钥：`GET /admin/api/keys` 分页返回安全元数据；`POST /admin/api/keys` 创建后在响应中一次返回明文并设置 `Cache-Control: no-store`；`PATCH /admin/api/keys/{id}` 更新名称、渠道/模型范围、RPM、过期时间和启用状态；`POST /admin/api/keys/{id}/rotate` 立即替换旧密钥并一次返回新值；`DELETE /admin/api/keys/{id}` 软撤销。明文只在创建/轮换响应出现，数据库仅存哈希；不提供旧密钥 reveal。写操作只允许 admin，bootstrap Key 由环境配置管理，不能经 API 编辑、轮换或撤销。

路由别名管理：`GET /admin/api/routes` 列出路由；`PUT /admin/api/routes/{alias}` 创建或替换路由；`DELETE /admin/api/routes/{alias}` 删除路由并返回 `204`。PUT body 为 `{ "strategy": "priority", "enabled": true, "targets": [{ "channel": "wb", "model": "glm-5.2" }] }`，支持 1-8 个不同渠道目标，顺序决定优先级。PUT/DELETE 仅允许 admin，viewer 只读。别名作为模型 ID 暴露于 `GET /v1/models`；聊天请求按目标顺序尝试，只有 429、500、502、503、504 或建立上游连接失败时才回退到下一目标。流式响应开始后不重放请求。

管理面完整 method/path 清单见系统设计 §9.2。流式请求必须保留 SSE 事件顺序，支持浏览器取消；代理层不得缓存或缓冲流。

## 契约交付

FastAPI 的 OpenAPI schema 是机器可读契约源。每个后端路由用 Pydantic request/response schema；前端类型从稳定的 OpenAPI 产物生成或在同一变更中同步维护。变更需附兼容性说明和前后端联调结果。
