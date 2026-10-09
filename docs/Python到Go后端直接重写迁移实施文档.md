# Python 到 Go 后端直接重写迁移实施文档

> 状态：实施基线
>
> 迁移方式：全量直接重写
>
> 最终目标：All2API 后端生产运行时、适配器、worker、测试和构建链全部使用 Go；Python 不再进入生产镜像、启动流程或运行时依赖。

## 1. 迁移结论

本项目不采用 Python/Go 双栈长期并行，不保留 Python 兼容后端，不把旧 Python 服务包装成 Go 的 HTTP 代理。

旧 Python 代码只允许在重写期间作为以下用途使用：

- 阅读现有行为和业务规则；
- 生成脱敏协议 fixture、数据库样本和回归样本；
- 对照请求、响应、错误、SSE 和账号状态机；
- 在隔离环境中临时启动，用于离线行为比对。

旧 Python 服务不得：

- 出现在 Go 生产镜像或 Compose 服务中；
- 被 Go 通过 HTTP、IPC、子进程或动态 import 调用；
- 与 Go 同时写入生产数据库；
- 继续作为渠道、浏览器、媒体或凭据能力的 fallback；
- 成为新功能的实现模板。

外部调用契约保持稳定：前端继续使用 `/admin/api`，外部客户端继续使用 `/v1` 和网关 Key。内部 Python 类型、FastAPI 依赖和模块结构不作为兼容目标。

## 2. 当前基线

当前后端位于 `services/api`，入口为 `app/main.py`，主要运行时为 Python 3.13、FastAPI、httpx、SQLite/WAL、Fernet 凭据存储，以及可选的 Playwright、指纹 HTTP 和 OpenCV 能力。

当前后端包含以下业务面：

| 业务面 | 当前能力 | Go 重写目标 |
|---|---|---|
| 数据面 | `/v1/models`、聊天、Responses、Anthropic Messages、图像、视频、音频、搜索和文件接口 | 保持路径、请求字段、响应字段、错误码、Header、SSE 和流中断语义 |
| 管理面 | 登录、会话、用户、Key、模型、路由、渠道、账号、日志、审计、统计和设置 | 按 application service 拆分，HTTP handler 不直接包含业务规则 |
| 渠道 | WorkBuddy、Doubao、ChatGPT adapter/provisioner | 全部重写为 Go adapter，禁止调用原项目端口和管理接口 |
| 存储 | SQLite schema、WAL、账号、Key、模型、路由、日志、usage、媒体和 Playground | 保持数据可读，采用 Go 版本化 migration 和 repository |
| 安全 | API Key hash、管理会话、CSRF/same-origin、Fernet 凭据加密 | 先实现兼容读取和 golden vector，再决定是否升级格式 |
| worker | Doubao QR/profile/browser、媒体下载和去水印 | Go worker 或 Go 调度的本仓库二进制，不得保留 Python worker |

仓库当前没有 `go.mod` 或 Go 后端源码，因此本任务是完整重写，不是 Python 代码的机械翻译。

## 3. 目标架构

```text
web/ 管理控制台
        │ HTTP
        ▼
cmd/all2api/main.go
        ├─ HTTP transport / middleware / error mapping
        ├─ application services
        ├─ domain / ports
        ├─ scheduler / runtime state
        ├─ adapters/workbuddy
        ├─ adapters/doubao
        ├─ adapters/chatgpt
        ├─ browser worker（Go）
        ├─ media worker（Go）
        └─ SQLite + encrypted credential store
```

目标目录：

```text
cmd/
├─ all2api/
├─ doubao-browser-worker/
└─ media-worker/
internal/
├─ config/
├─ bootstrap/
├─ transport/http/
├─ middleware/
├─ domain/
├─ application/
│  ├─ accounts/
│  ├─ channels/
│  ├─ gateway/
│  ├─ keys/
│  ├─ models/
│  ├─ routes/
│  ├─ usage/
│  └─ admin/
├─ ports/
├─ adapters/
│  ├─ registry/
│  ├─ workbuddy/
│  ├─ doubao/
│  └─ chatgpt/
├─ infrastructure/
│  ├─ persistence/
│  ├─ crypto/
│  ├─ httpclient/
│  ├─ browser/
│  ├─ media/
│  └─ observability/
├─ protocols/
└─ scheduler/
migrations/
tests/
├─ contract/
├─ integration/
├─ security/
└─ e2e/
```

`web/` 保持 TypeScript 实现。页面只依赖管理 API，不把 Python 或 Go 的内部结构暴露给前端。

## 4. 重写原则

### 4.1 只保持外部兼容

必须保持：

- `/v1` 和 `/admin/api` 路径；
- Bearer、`x-api-key`、管理会话 Cookie 和权限语义；
- OpenAI、Anthropic、Responses 的公共 DTO；
- 错误结构、状态码、`X-Request-ID`、`Retry-After` 和流式事件；
- 现有 SQLite 数据、Key hash、账号状态和审计记录的可用性；
- 前端现有接口调用方式。

不需要保持：

- FastAPI/Pydantic 类型；
- Python 包名、异常类型和内部模块路径；
- Python 环境变量实现细节；
- 旧 bridge 的兼容路由和旧服务管理 API。

### 4.2 行为重写，不逐行翻译

先为每个行为建立脱敏输入、输出、错误和状态 fixture，再用 Go 重新设计领域对象、ports、repository 和 adapter。禁止把 `admin.py` 或 `gateway.py` 直接翻译成新的单体大文件。

### 4.3 单一生产运行时

最终 Compose、Dockerfile、启动脚本和健康检查只能启动 Go。浏览器和媒体如果需要独立进程，也必须是本仓库源码、Go 构建、Go 配置和 Go 健康检查管理的 worker。

### 4.4 数据库单写入者

开发期使用 Python 只读副本或脱敏 fixture 做比对。生产切换后只允许 Go 写入数据库，禁止 Python/Go 双写、共享连接池或共享运行态文件。

## 5. Python 到 Go 的模块映射

| Python 当前模块 | Go 目标模块 | 重写要求 |
|---|---|---|
| `app/main.py`、`config.py` | `cmd/all2api`、`internal/bootstrap`、`internal/config` | 生命周期、配置解析、依赖装配和优雅退出分离 |
| `routers/gateway.py` | `transport/http/gateway`、`application/gateway` | handler 只解析请求和写响应；路由、授权、调度进入 application |
| `routers/admin.py` | `transport/http/admin/*`、`application/*` | 拆成 settings/users/logs/accounts/channels/playground/media |
| `routers/auth.py`、`keys.py` | `transport/http/auth`、`application/auth`、`application/keys` | 保留 Cookie、Key hash、轮换、撤销和审计语义 |
| `routers/models.py`、`routes.py` | `application/models`、`application/routes` | 模型缓存、禁用、alias 展开和 scope 校验独立实现 |
| `domain/`、`ports/` | `internal/domain`、`internal/ports` | 不依赖 HTTP、SQLite、具体 adapter 或平台 SDK |
| `infrastructure/db.py` | `internal/infrastructure/persistence` | embedded migrations、事务、WAL、repository 和备份检查 |
| `infrastructure/security.py` | `internal/infrastructure/crypto`、`application/auth` | 区分加密、认证、会话、CSRF 和权限用例 |
| `infrastructure/credentials.py` | `internal/infrastructure/crypto`、`ports.CredentialStore` | 先兼容既有 Fernet 密文，禁止明文中间文件 |
| `scheduler/` | `internal/scheduler` | 租约、冷却、熔断、重试和跨渠道策略通用化 |
| `protocols/` | `internal/protocols` | canonical request/response、SSE 和 usage 转换 |
| `adapters/workbuddy/*` | `internal/adapters/workbuddy` | Go HTTP client、OAuth/realm、JWT/device token、账号池和 SSE |
| `adapters/doubao/*` | `internal/adapters/doubao`、`internal/browser`、`cmd/doubao-browser-worker` | QR、Cookie/profile、浏览器生命周期、聊天和多媒体全部 Go 化 |
| `adapters/chatgpt/*` | `internal/adapters/chatgpt` | OAuth PKCE、token refresh、Web 模型目录、Web conversation/SSE、指纹和错误映射 |
| `infrastructure/media_store.py` | `internal/infrastructure/media`、`cmd/media-worker` | 下载、大小限制、媒体元数据、去水印和文件生命周期重写 |
| `tests/**/*.py` | `tests/**/*.go` | 单元、集成、契约、安全和 E2E 测试改用 Go；前端测试保持现有工具链 |

## 6. 直接重写阶段

### G0：基线冻结

交付：

- 当前 OpenAPI 和全部路由清单；
- 每个接口的请求、响应、状态码、Header 和错误 fixture；
- SQLite 表结构、索引、触发器、迁移版本和数据量快照；
- Fernet、API Key hash、会话 Cookie 的兼容性 golden vector；
- 三渠道的 mock 上游响应、SSE、账号状态和错误矩阵；
- 配置项从 Python 名称到 Go 配置结构的映射表。

完成条件：基线提交固定，后续 Go 实现只能通过 fixture 和契约测试证明行为等价。

### G1：Go 运行时和存储

实现 `go.mod`、`cmd/all2api`、配置加载、日志、请求 ID、CORS、健康检查、信号处理、SQLite 连接、embedded migrations 和 repository 基础接口。

数据库要求：

- 生产数据库文件路径保持兼容；
- 启动前检查 schema version，禁止静默建表覆盖已有数据；
- 所有迁移可重复执行，不使用无条件 `DROP TABLE`；
- 继续支持 WAL、事务、唯一约束和审计写入；
- 迁移前自动或人工完成备份，并支持在副本上演练。

### G2：安全、领域和数据面

依次实现：

1. 管理会话、角色、same-origin/CSRF 和登录限流；
2. API Key hash、一次性明文返回、scope、过期、轮换和撤销；
3. Channel、Account、Model、Route、RequestLog、Usage 和 Audit domain；
4. registry、manifest、capability、alias、渠道/模型授权和调度；
5. OpenAI、Anthropic、Responses 的 canonical protocol；
6. SSE 转换、客户端断开、上游断流、usage 和重试边界；
7. request logs、usage_daily、account runtime state 和审计事务。

完成条件：fake adapter 可以在不修改核心代码的情况下完成模型发现、账号选择和请求调用。

### G3：管理面

按以下边界实现 Go handler 和 application service：

- auth/session；
- users/settings；
- keys/models/routes；
- channels/adapter manifest/provision schema；
- accounts、start/poll/finish/import/cancel/refresh；
- logs、audit、metrics、usage、overview；
- Playground 会话、SSE、文件和媒体资源。

管理 handler 不得出现平台分支、平台 URL、凭据读取或直接 SQL 业务编排。平台差异只能经 registry、manifest、provisioner 和 adapter ports 表达。

### G4：三渠道 Go adapter

每个渠道必须按同一顺序完成：

1. 脱敏 fixture 和 mapper；
2. 错误类型、状态机和能力声明；
3. Go HTTP client/浏览器 client；
4. 账号新增、刷新、禁用、删除和凭据轮换；
5. 模型发现、普通响应和 SSE；
6. 多媒体或搜索等 capability；
7. adapter contract、SQLite 集成和真实平台 E2E。

迁移顺序固定为 WorkBuddy、Doubao、ChatGPT：

| 渠道 | 首要 Go 工作 | 高风险项 |
|---|---|---|
| WorkBuddy | OAuth/realm、JWT/device token、账号池、租约、SSE | 账号选择可信度、错误重试和凭据轮换 |
| Doubao | QR、Cookie、profile、浏览器 worker、聊天和多媒体 | 浏览器生命周期、验证码、Cookie 隔离和媒体处理 |
| ChatGPT | OAuth PKCE、token refresh、Web 模型目录、Web conversation/SSE | TLS/指纹、Turnstile/PoW、上游限流和会话刷新 |

不得把原项目的数据库、Cookie、OAuth token、浏览器 profile、管理 API 或 HTTP wrapper 带入 Go 运行时。

### G5：浏览器和媒体能力

Python Playwright、Python OpenCV 和 Python 媒体逻辑必须移除。默认方案：

- 浏览器使用 Go Playwright binding 或本仓库内的 Go browser worker；
- 图像/视频处理使用 Go 原生库、GoCV 或本仓库构建的 Go media worker；
- FFmpeg 只能作为受控外部工具，不得依赖旧 Python 包装脚本；
- worker 通过明确的内部 Go interface 或本地协议通信；
- worker 具有版本、超时、并发限制、健康检查和资源回收；
- worker 异常不得泄露 Cookie、token、profile 路径或本地绝对路径。

如果某项能力暂时无法纯 Go 实现，必须在 G0 建立 ADR 和替代方案；不得以“暂时保留 Python worker”作为完成状态。

### G6：切换和删除 Python

切换顺序：

1. 停止 Python 服务和所有旧 worker；
2. 备份 SQLite、媒体目录和 Go 配置；
3. 在相同端口启动 Go 服务；
4. 执行健康、登录、渠道、模型、Key、账号、文本、SSE 和媒体 smoke test；
5. 执行前端关键页面回归；
6. 保留切换前备份和可回滚 Go/Python 镜像，但不恢复双栈运行；
7. 删除 Python 生产目录、`pyproject.toml`、`uv.lock`、Python Docker 构建和 Python CI 命令；
8. 更新 README、Compose、环境变量、部署、验收和迁移矩阵。

## 7. Go 依赖和实现约束

首版优先使用 Go 标准库：`net/http`、`encoding/json`、`context`、`crypto`、`embed`、`database/sql`、`io` 和 `image`。

依赖选择必须满足：

- Go 版本在 `go.mod` 中明确锁定，并在 CI、Docker 和开发机一致；
- SQLite 驱动经过 WAL、事务、并发和 Windows/Linux/Docker 验证；
- Fernet 兼容库或实现经过现有密文 golden vector 验证；
- ChatGPT 指纹能力优先使用 Go client；只有真实 E2E 证明标准 TLS 不足时，才引入 uTLS 类依赖；
- 浏览器和媒体依赖固定版本并纳入镜像、SBOM 和许可证检查；
- 不引入任何源项目的 Go/Python module、Git submodule 或动态路径。

## 8. 测试方案

每个 Go package 至少配套以下测试：

| 测试层 | 内容 |
|---|---|
| unit | domain、scope、错误映射、状态机、调度、token/usage 估算 |
| integration | SQLite migration、事务、WAL、repository、CredentialStore、文件存储 |
| contract | 三渠道 mapper/client/provisioner、模型、普通响应、SSE、错误和取消 |
| security | Key hash、一次性明文、Cookie、角色、CSRF、日志脱敏、路径穿越 |
| api | `/v1`、`/admin/api` 路由、响应、Header、错误和 OpenAPI parity |
| e2e | 登录、账号新增、Key、模型、路由、三渠道调用、Playground 和媒体 |
| clean | 删除源目录、Python、旧端口和旧配置后重新构建、启动和健康检查 |

执行门禁：

```powershell
go test ./...
go vet ./...
go build ./cmd/all2api
go build ./cmd/doubao-browser-worker
go build ./cmd/media-worker
pnpm lint
pnpm typecheck
pnpm build
```

CI 中不得再要求 `uv`、Python、FastAPI 或 `pytest` 才能构建和启动后端。

## 9. 数据和凭据迁移

### 9.1 普通数据

Go 直接读取现有 SQLite 文件，先在副本上执行 schema 检查和迁移。必须核对：

- 表、索引、唯一约束和迁移版本；
- accounts、channels、models、routes、api_keys 的数量；
- request_logs、usage_daily、audit_logs 的可查询性；
- Playground 和 media metadata 与实际文件的对应关系。

检查工具只能输出数量、hash、状态和类别，不得打印 token、Cookie、密码或密文原文。

### 9.2 加密凭据

现有加密字段必须先由 Go 兼容读取。迁移期间：

- 不把凭据解密到临时文本文件；
- 不复制源项目账号目录、Cookie、OAuth token 或浏览器 profile；
- 不在日志、错误、测试输出和 API 响应中输出 Secret；
- 若必须升级算法，使用一次性 Go 离线迁移工具，在内存中完成解密/重加密并原子替换；
- 迁移失败时保留原数据库和凭据备份，不覆盖原文件。

管理会话 Cookie 可以在切换时全部失效并要求重新登录；API Key hash、渠道凭据和账号数据不得因切换失效。

## 10. 发布、回滚和删除条件

### 发布前

- Go 二进制、worker、Docker 镜像和 SBOM 已生成；
- SQLite 和媒体目录备份已恢复演练；
- 三渠道 contract 和真实平台 E2E 已记录结果；
- 前端、OpenAPI、配置模板和运维文档已同步；
- `rg` 扫描运行时代码不再命中 Python、旧端口、源路径、旧 bridge 和源项目 import。

### 回滚

回滚对象只有：Go 镜像、worker 镜像和切换前数据库/媒体备份。回滚前必须停止 Go，禁止让旧 Python 和 Go 同时写同一份数据库。任何数据库前向迁移都必须先在副本验证旧版本是否可读取；不能验证时只能使用切换前完整备份回滚。

### Python 删除门槛

只有以下条件全部满足才能删除 Python：

- Go 数据面和管理面全部上线；
- 三个渠道账号流程和调用能力完成；
- 浏览器、媒体和文件能力没有 Python fallback；
- Go 可以直接读取需要保留的现有数据和凭据；
- clean build/run 不需要 Python 或源项目目录；
- Go 版本回滚和数据库恢复已演练；
- 文档、Compose、CI、脚本和环境变量均已更新。

## 11. Definition of Done

本迁移只有在以下条件同时满足时才算完成：

1. 生产镜像只包含 Go 服务和本仓库构建的 Go worker/必要运行库；
2. 不存在 Python 启动入口、Python subprocess、旧 bridge 或源项目 HTTP 依赖；
3. `/v1` 和 `/admin/api` 的外部契约通过回归测试；
4. 现有数据库、Key、账号、凭据、审计、usage 和媒体资产可继续使用；
5. WorkBuddy、Doubao、ChatGPT 均通过 adapter contract、集成、安全和 E2E 验收；
6. Web 控制台可完成登录、配置、账号管理、调用、日志、统计和媒体操作；
7. Go 单元、集成、API、E2E、clean build、漏洞和许可证门禁通过；
8. 运行时扫描不再出现 Python、源项目路径、旧端口、明文 Secret 或不受控外部服务；
9. 迁移来源、数据库变更、风险、回滚和实际测试证据已归档。

## 12. 执行清单

### 本轮实施记录（2026-10-08）

已开始 G0/G1 的首个可验收纵切面：

- 已建立 Go module、`cmd/all2api`、`internal/config`、`internal/application`、`internal/ports`、`internal/infrastructure/persistence` 和 `internal/transport/http`；
- 已将当前 Python schema 11 固化为 Go `embed` 的幂等 baseline migration，支持 WAL、事务、现有数据保留、缺列兼容补齐和未来 schema 拒绝启动；
- 已实现环境配置校验、请求 ID、CORS、优雅退出、`/admin/api/healthz` 和存储快照；
- Go 配置已增加 `A2A_ENV=production` 安全门禁：生产拒绝默认/弱 session secret、空/弱 credential master key、弱管理员密码、legacy bridge/upstream 配置和无 token 的 browser worker；development/test 仍支持 clean-run 默认配置。
- 已加入 G0 基线冻结工具，生成 OpenAPI、82 条 method/path 路由清单、56 个配置字段映射，以及显式指定 SQLite 后的只读 schema/数量快照；
- 已加入脱敏安全 golden vector：API Key SHA-256、Python Fernet 密文读取和签名 session cookie 字段/算法约束；
- 已为配置、迁移幂等性、数据保留、未来 schema 拒绝和健康接口建立 Go 测试。

G2 本轮已完成首个安全/Key 纵切面：

- 管理 session、角色检查、same-origin 写保护、Secure/HttpOnly/SameSite Cookie、登录限流和审计；
- API Key 创建、hash 鉴权、Fernet 密文、scope 交叉校验、过期、RPM、一次性创建/轮换明文、软撤销和 bootstrap 保护；
- `/admin/api/auth/*` 与 `/admin/api/keys*` 已有 Go application/repository/HTTP 分层和 SQLite 集成测试；列表和编辑响应不返回完整 Key。

G2 数据面继续完成了第一条 fake adapter 验证链：

- `ports.Adapter`、canonical model/chat DTO、gateway application service 和 fake adapter 已建立；
- `/v1/models`、`/v1/chat/completions` 已接入 Go transport，验证 Bearer/x-api-key、Key RPM、channel/model scope、model discovery、OpenAI JSON、基础 SSE 和错误映射；
- 生产 `cmd/all2api` 默认使用空 adapter registry，fake adapter 只在测试注入，当前不代表任何真实渠道已迁移。
- Anthropic Messages 与 OpenAI Responses 的文本规范化、工具调用输入校验、响应转换和基础 SSE 事件已接入同一 gateway；
- 公共 Chat Completions、Anthropic Messages 和 Responses 已补齐增量 SSE：首 chunk 前失败保留正常 HTTP 错误，首 chunk 后中断发送协议 error 终态且不伪造 `[DONE]`；对应 HTTP/protocol contract 已通过。
- `request_logs` 与 `usage_daily` 已有 Go `RequestRecorder`，在一个 SQLite 事务中写入并通过聚合测试。

G4 已开始 WorkBuddy：

- Go WorkBuddy client/adapter 已实现原生模型目录、`/v2/chat/completions` 请求、账号 credential resolver、错误边界和 mock HTTP contract；
- WorkBuddy Go adapter 已补充上游 OpenAI SSE 解析和 gateway account-aware stream port；`stream=true` 通过同一账号租约转发增量并记录最终 usage。
- gateway 的 account-aware adapter 会通过 Go scheduler 获取可用账号租约，凭据由本项目 Fernet CredentialStore 解密，adapter 不直接访问源项目数据库或管理端口；
- `cmd/all2api` 已注册 WorkBuddy Go adapter；没有本地账号或上游不可用时返回本地 adapter/account unavailable，不回退 Python。
- ChatGPT Go adapter 已接入 `chatgpt.com/backend-api/models` 与 conversation SSE；Doubao Go adapter 已接入 Cookie credential、model catalogue 和 Alice chat SSE；两者均通过本项目账号租约和 CredentialStore。
- 三渠道 model discovery 已改为 account-aware：先获取本地账号租约，再用对应 credential 查询目录；某一渠道不可用时不会阻断其它已配置渠道，全部不可用才返回 `adapter_unavailable`。

G3/G5 基础边界已继续建立：

- registry/manifest 已统一列出三渠道及已验证能力；三渠道当前只声明 `chat`，未实现的图片/视频能力不会伪造为可用；未装配的渠道显示 `not_configured`，不会伪造健康；
- Go `doubao-browser-worker` 和 `media-worker` 已有独立二进制、`/healthz` 和 Go build gate；Doubao worker 已实现标准库 CDP/WebSocket、Chromium 启动、profile 路径边界、QR/认证状态读取、受 token 保护的 session complete 和资源回收；本机 Go CDP 真实启动/导航已通过，但复制的 profile 状态为 `scanned` 且未认证，因此平台登录验收仍未通过。
- media-worker 已升级为可独立运行的安全资产 worker：base64 资产写入、读取、删除、文件名净化、路径边界、worker token 和 100 MB 限制均由 Go 实现；另有 Go 原生 `/v1/watermark/remove` 区域修复能力、核心和 HTTP contract test，provider-specific 生成及视觉质量仍需真实 E2E。
- 已提供不含 Python 服务的 `Dockerfile.golang` 与 `docker-compose.go.yml`，默认旧 Compose 仍保留到 G6 切换验证完成。
- Go-only Compose 已把 gateway、browser-worker、media-worker 和 web 纳入同一健康检查/依赖拓扑；browser/media worker 默认关闭资产操作且要求显式 token，media provider-specific 操作仍明确返回 pending。
- browser/media worker 已完成本机无 Chromium/无 token 条件下的协议 smoke：health 返回 `not_configured`，未带 token 返回 `unauthorized`/`worker_token_not_configured`，启用但未配置浏览器时返回 `worker_capability_pending`；CDP handshake、命令响应、profile 边界和 Doubao worker client 均有 contract test。
- Go clean build/run gate 已通过：临时 SQLite、临时端口和构建产物启动后，详细 healthz 返回 schema 11；gate 使用 curl 绕过本机代理并不污染仓库根目录。
- 已加入 `scripts/backup-go-state.ps1` 和 `scripts/verify-go-backup-restore.ps1`；在停止写入的临时演练中完成 SQLite/WAL sidecar、媒体文件 SHA-256 校验、Go clean build/restore 启动和 schema 11 health 验收。备份 manifest 不包含配置文件或加密主密钥，主密钥必须由外部 Secret Manager 单独恢复。
- 已加入 `scripts/verify-go-rollback.ps1`；另从已构建的 Go-only API 镜像归档了 Linux/amd64 上一版 binary，并完成挂载备份副本的 Docker 回滚演练，healthz 返回 schema 11。Windows `Start-Process` 不能直接执行 Linux binary，生产镜像回滚应使用同平台容器演练；artifact/hash 和结果已归档到 `docs/migration-evidence/`。
- 已加入 `scripts/check-go-cutover.ps1`；该脚本逐项检查 Docker daemon、Go-only 默认 Compose、`A2A_ENV=production`、CI image/artifact gate、Python/legacy runtime 删除、上一版 Go binary、备份脚本和脱敏真实 E2E JSON 证据，缺项时以非零状态和 JSON blocker 列表拒绝切换。
- 已加入 `scripts/probe-go-platforms.ps1`，按隔离 SQLite 副本执行脱敏真实探针并自动写 JSON，只输出 health、状态码、错误 code、模型数量和文本长度；最新代理重试结果为 WorkBuddy `models=200/22`、chat `200/text_length=3`，Doubao `models=200/8`、chat `200/text_length=114`，ChatGPT `401/credential_rejected`。代理已打通 Go 进程到 ChatGPT Web 的网络路径，但隔离数据库中的现有凭据被上游拒绝；此前曾观察到 Doubao 的短暂 404/502，状态以最新归档 JSON 为准。
- 脱敏探针 JSON 已归档至 `docs/migration-evidence/platform-probe-2026-10-09.json`；browser worker container smoke 和本机 Go CDP 浏览器 E2E 已分别归档至 `docs/migration-evidence/browser-worker-smoke-2026-10-09.json`、`docs/migration-evidence/browser-worker-e2e-2026-10-09.json`。最新重跑发现当前可用 profile 数为 1，真实打开 `https://www.doubao.com/chat/` 并取得页面标题，但 `authenticated=false`；证据明确标记真实三渠道/browser E2E 未通过。
- 额外无凭据浏览器检查已归档至 `docs/migration-evidence/doubao-browser-region-2026-10-09.json`：公开访问被重定向到 `security/doubao-region-ban`，页面提示区域限制并要求登录；未点击登录、未输入凭据、未读取 Cookie。
- 已加入 `scripts/generate-go-sbom.ps1`；在临时目录生成 gateway、browser-worker、media-worker 的 CGO=0 release binary、依赖模块清单和 SHA-256 manifest，避免把未执行 Docker image build 误记为完整发布证据。
- 已加入 `.github/workflows/go-migration.yml`，在 CI 中分离执行 Go race/vet/build、Python regression、Web lint/typecheck/build 和 Go-only Compose config gate。
- CI 已增加 Linux Dockerfile.golang 实构建 job 和 Windows Go release/SBOM artifact job；本机 Docker daemon 不可用时仍只记录为本地阻断，不降低 CI 门禁。
- `verify_no_source_deps.py` 已修正为检查 Go-only Dockerfile；Docker daemon 缺失时输出 `UNVERIFIED` 并退出码 2，不再把跳过的镜像构建打印为 PASS。
- `verify_no_source_deps.py` 的 clean-build 分支已改为 Go test/vet 和三枚 Go binary 构建，不再通过 `uv`/Python 编译作为 Go 运行时证据；Python regression 仍由独立 CI job 负责。
- `verify_no_legacy_in_prod.py` 已改为构建/检查 `Dockerfile.golang`，验证三枚 Go binary 且拒绝 `/app/app` Python runtime；Docker daemon 不可用时返回 `UNVERIFIED`/退出码 2。
- Go release artifact/SBOM smoke 已通过：`generate-go-sbom.ps1` 在临时目录生成 3 个 CGO=0 binary、11 条 go.sum 依赖记录和 SHA-256 manifest；该结果不替代 Docker image build。
- Responses 增量 SSE 的终止阶段已改为显式生成四个 terminal event（`response.output_text.done`、`response.content_part.done`、`response.output_item.done`、`response.completed`），协议测试覆盖非空/空文本并禁止重复 delta 或 setup event；真实 HTTP stream gate 继续通过。
- Doubao Go client 已对齐已审计原生 transport：模型目录使用带设备参数的 POST，补齐 Cookie/CSRF/Origin/Referer headers，并在目录接口 404/空结果时解析 `/chat/` 路由目录；新增 fixture 后真实隔离探针恢复为 models/chat HTTP 200。
- ChatGPT Go client 统一使用 ChatGPT Web `/backend-api/models`、Sentinel requirements 和 `/backend-api/conversation`；OAuth 字段只用于 Web 凭据认证，不分流到 Codex 或其他 ChatGPT 协议；Web SSE contract test 已覆盖，真实 ChatGPT probe 因 `401/credential_rejected` 仍未通过。
- 已使用 `sub2api-account-20261009203027.json` 在隔离 SQLite 副本执行 ChatGPT Web 账号导入：导入 HTTP 200、新增 1 个账号、模型目录 HTTP 200/13 个模型；refresh 为 `502/credential_refresh_failed`，聊天为 `503/adapter_unavailable`，同一代理直连 ChatGPT Web 返回 Cloudflare `403`。证据见 `docs/migration-evidence/chatgpt-web-import-2026-10-09.json`，未使用或调用 Codex endpoint。
- 针对上述失败已完成修复：Go ChatGPT client 改用 `tls-client` Chrome 110 TLS/HTTP2 指纹和精确 header order；bootstrap、Web conversation payload、Sentinel PoW 配置和 proof JSON 组装已对齐参考项目；gateway 对成功模型目录增加 5 分钟缓存，避免每次聊天重复触发 Cloudflare 目录检查。Go direct adapter 使用同一账号已观察到 conversation 成功，完整门禁通过；最终 gateway 隔离复测需等待 `127.0.0.1:7892` 代理重新监听。详见 `docs/migration-evidence/chatgpt-web-transport-fix-2026-10-09.json`。
- Go-only Compose 与 `probe-go-platforms.ps1 -ChatGPTProxy` 已显式传递 ChatGPT proxy 配置；2026-10-09 代理重试确认网络可达，失败原因已从 `upstream_unavailable` 收敛为现有凭据被拒绝，不能把它误判为平台验收通过。
- ChatGPT 连通性证据已归档至 `docs/migration-evidence/chatgpt-connectivity-2026-10-09.json`：ChatGPT Web 页面、`/backend-api/models`、`/backend-api/conversation` 和 `chat.openai.com` 在当前 Go 环境不可达，`auth.openai.com` 可达并返回无凭据时的 403；未记录凭据或上游响应体。
- 用户报告代理已连接后，in-app browser 可打开公开 `https://chatgpt.com/` 页面，但当前未登录且 Web API endpoint 直接导航被浏览器客户端拦截；证据见 `docs/migration-evidence/chatgpt-browser-proxy-2026-10-09.json`。Go 进程已支持显式 `A2A_CHATGPT_PROXY`，但未提供有效 ChatGPT Web 凭据，因此真实 ChatGPT Web E2E 尚未证明。
- 本轮最终 deterministic gate 复核通过：Go test/vet/build/clean-run、Python ruff/pytest、Web lint/typecheck/build、Go-only Compose config；2026-10-09 Docker Desktop 已启动，Go-only api/browser-worker/media-worker 三镜像真实构建并启动健康检查通过，镜像摘要为 api `sha256:1e8bd64e...`、browser-worker `sha256:fa4742d...`、media-worker `sha256:6be88b3...`。基础镜像通过本机 Docker mirror 拉取，Dockerfile.golang 本身未使用 Python。
- Go 管理查询已接入 request logs、usage summary/daily/channel/model/key，读取由 repository/application query service 负责，未在 handler 中拼 SQL；`POST /admin/api/logs/clear` 已按 retention 在 SQLite 事务内清理 request_logs/usage_daily 并写审计。
- Go request-log 查询已补齐前端过滤契约：request id、channel、model contains、HTTP status、error kind、stream 和 RFC3339 时间范围，并返回 upstream model、route alias、fallback depth、TTFT 字段。
- Go 管理面已接入模型目录启用状态、priority route CRUD 和 retention settings 的 repository/application/query boundary；写操作带角色、同源保护和审计。
- Go gateway 已消费 SQLite priority route：无渠道前缀的模型请求按启用 alias 顺序尝试 targets，429/502/503/504、上游不可用、账号不可用和本地禁用会尝试后续 target；成功结果写入 alias/upstream model/fallback depth 元数据。公共 `/v1` 与 Playground 流式请求只在首个 target 输出 chunk 前允许回退，输出开始后不重放；未配置或停用 alias 返回明确 `model_not_found`。
- Go 路由覆盖审计已确认：冻结基线中剩余未保留的三条 `/admin/api/accounts/{channel}/onboarding/*` 是迁移期 legacy onboarding，目标接口为 `/admin/api/channels/{slug}/accounts/provision/*`；editable-file 旧下载路径已显式返回 `capability_not_supported`，不再返回 404 或依赖 Python 文件目录。
- Go 管理面已补齐本地 channel override CRUD、`logs/clear` 和 accounts batch-delete；channel config 只允许内置 registry 渠道和非敏感 JSON，账号批删按 native credential id 事务清理并逐项返回结果。
- channel override 的 `enabled` 已接入 Go gateway 数据面和 Playground 管理调用；禁用渠道统一返回 `channel_disabled`，不再只是前端展示状态。
- Go 管理面已接入 `POST /admin/api/models/refresh`：按账号租约读取实时目录、原子 upsert `models` 缓存、保留管理员禁用状态，并返回逐渠道成功/失败摘要。
- Go 管理面已接入 users 列表、创建、角色/启用状态更新和删除保护；同样通过 UserService/UserRepository 和审计边界。
- Go 管理面已补齐 audit-logs、channel runtime/test 查询，测试操作要求本地账号和 credential 可用，不会在未配置时探测上游。
- Go 系统页和账号页基础 API 已接入：`sysinfo`、`storage/health`、`metrics`、`overview`、账号安全列表、启停、单删和批量删除；批量删除会在事务内按 native credential id 清理凭据并返回逐项结果；系统信息已使用 `go_version`/`implementation`，不再暴露 Python runtime 字段；未实现的账号 provision 能力会显式返回 `capability_not_supported`。
- 已补齐前端使用的 `POST /admin/api/accounts/{account_id}/refresh` Go 路由；WorkBuddy/ChatGPT 走 token refresh，Doubao Cookie/browser session 返回明确 `capability_not_supported`，并由 HTTP 回归测试锁定，避免 404 或伪造刷新成功。
- 已为 Go 运行时注册迁移期旧 onboarding 三条路径并统一返回 `410 legacy_bridge_disabled`，同时保留 native provision API；HTTP 回归测试确认旧客户端不会收到含糊的 404。
- 已加入 `scripts/verify-go-route-coverage.py` 和 CI gate，读取冻结 `routes.json` 与 Go `ServeMux` 注册表，当前覆盖 72 条 baseline path，防止新增接口再次出现文档/路由遗漏。
- CI 已补充独立 `go-vulnerability-scan` job，使用固定版本 `govulncheck@v1.1.4` 扫描 Go runtime，以及 `go-license-scan` job，使用固定版本 `go-licenses@v1.3.0` 扫描 Go 依赖许可证；本机执行因 `proxy.golang.org` 网络不可达而记为未验证，不把它误报为通过。仓库根 `LICENSE` 仍未声明，公开发布前需要明确许可证决策，当前不能把许可证门禁标为通过。
- 供应链实测摘要已归档至 `docs/migration-evidence/supply-chain-2026-10-09.json`：`go mod verify` 通过，漏洞/依赖许可证工具因外部 Go 网络不可达未验证，根许可证仍缺失。
- 已加入 `scripts/verify-go-supply-chain.py` 与 `go-supply-chain-local` CI gate：在模块已下载时验证 `go mod verify`、模块 cache 完整性、常见依赖许可证文件和根许可证存在；当前根许可证缺失时返回未验证退出码。该审计仍不是法务结论，也不能替代 govulncheck/go-licenses。
- `check-go-cutover.ps1` 已修复为在 Docker daemon 不可用时输出结构化 `docker_daemon=false` blocker，而不是因 PowerShell 外部命令异常直接中止；当前实测 readiness 能完整保留 Docker、供应链、许可证和真实平台阻断项。
- Go 渠道管理响应已根据本地 provisioner/账号池真实状态填充 `provision_configured`、`accounts_configured`；WB `qr-oauth`、Doubao `qr-login`、ChatGPT `oauth-pkce` 均通过动态 schema 暴露，前端会在 `next_step=complete` 时自动完成 QR 流程。
- Go media store 已接入素材安全文件落库、100 MB 大小限制、路径穿越保护、列表、内容读取和删除；生成结果已支持 base64/远程 URL 归一化、失败时 remote metadata fallback、`/v1/files/{id}/content` 本地 URL 重写，以及 Doubao 图片的 Go watermark transformer。真实 provider 生成、CDN 下载成功率和视觉质量仍需平台 E2E。
- Go 数据面已接入本地 `/v1/files` 生命周期：multipart 上传、Key actor 隔离、列表、元数据、删除和内容下载均复用 media store，provider 文件/多模态输入仍单独受能力矩阵约束。
- Doubao Samantha 图片/视频请求已迁移到 Go adapter contract：包含设备查询参数、Cookie headers、skill/content_type payload、SSE event 解码、异步视频 task_id 轮询和安全错误映射；gateway 已有 manifest gate，当前 manifest 仍只声明已验证的 `chat`，真实媒体 E2E 通过后才开放 `/v1/images/generations`、`/v1/video/generations`。
- Go Playground 已接入会话/消息/运行记录持久化和文本 chat 调用，复用 gateway 的 account-aware adapter；图片/视频 generation、search 和 editable-file 的管理路由、capability dispatch 与本地资产落库/下载 URL 边界已迁移，但受 manifest gate 保护，真实 E2E 通过前不会开放。
- 未迁移的 Go 数据面和管理面能力已注册显式 `capability_not_supported` 响应入口，避免 frontend/API contract 以 404 掩盖迁移状态。
- WorkBuddy token import 及 QR/OAuth、ChatGPT OAuth-PKCE、Doubao browser QR/profile 的 Go provision 已接入本地加密 credential 写入、持久化 session 和幂等边界；Go ChatGPT 已迁移 requirements prepare/finalize、Proof-of-Work 和纯计算 Turnstile 指令解释器，并对 Arkose/无法解析 challenge 返回安全 protocol error；真实 challenge 和平台 refresh 仍待 E2E。
- Doubao Cookie import 和 ChatGPT token import 也已接入同一 Go 加密 credential/idempotency pipeline；三渠道显式材料导入不依赖旧项目文件、数据库或管理端口。
- Doubao native HTTP QR worker 已按现有协议重写：CSRF、二维码生成、轮询扫码状态、redirect Cookie 收集和凭据状态均有 Go mock contract；Chromium profile worker 已完成 Go/CDP 协议边界，但真实浏览器启动与平台登录仍待环境验收。
- WorkBuddy QR/OAuth 的 Go start/poll/complete/cancel 已接入原生 WorkBuddy auth state/token/account API，并将 session/idempotency state 加密持久化到 SQLite；mock contract 已验证重启读取和凭据落库。
- WorkBuddy 与 ChatGPT 的管理员 credential refresh 已接入 Go：refresh token 只在服务端解密、通过原生 token endpoint 轮换并事务加密更新，`/admin/api/accounts/{id}/refresh` 不返回凭据；两者的真实平台轮换仍需 E2E 验收。
- ChatGPT OAuth PKCE 的 Go start/poll/callback-complete/cancel 已完成 mock contract；state/verifier 只进入加密 provision session，token exchange 失败不会回显上游响应。
- ChatGPT Web search 和 image 的 Go adapter contract 已完成：search 覆盖 prepare/conduit/Sentinel/SSE/document/source 脱敏；image 覆盖 file metadata/upload/complete、picture_v2、Sentinel、file-service 引用和下载。两者仍受 chat-only manifest gate 和真实平台 E2E 约束。

以下仍未宣称完成：三渠道真实平台 E2E、ChatGPT Turnstile/Arkose 真实 challenge 验收、Doubao browser 的真实 Chromium/平台登录验收、provider 真实生成和视觉质量、Playground editable-file 的真实调用、前端切换和 Python 删除门槛。Go media provider result storage/watermark/file URL contract、Go ChatGPT image 的 upload/picture_v2/file-download contract 和纯计算 Turnstile contract 已完成，但仍受 manifest gate 和真实平台验收约束。当前 Compose 仍由 Python 启动，直到 G6 切换条件全部满足前不得改成生产 Go 入口。

真实隔离探测证据（2026-10-09）：将当前本地 SQLite 复制到临时目录后，Go 服务详细 healthz 返回 200/schema 11；Go Fernet 兼容读取已成功解密副本中的 wb/doubao/chatgpt credential 字段。最新代理重试使用该副本和临时管理/网关 Key 按渠道探测，WorkBuddy `/v1/models` 返回 HTTP 200、22 个账号可见模型，文本调用返回 HTTP 200 且 assistant 文本长度为 3；Doubao `/v1/models` 返回 HTTP 200、8 个模型，文本调用返回 HTTP 200 且 assistant 文本长度为 114；ChatGPT Web 目录返回 HTTP 401、错误 code 为 `credential_rejected`。上游错误由 Go typed transport error 安全归类，不回显凭据或上游 body；状态以归档 JSON 为准。该结果证明代理下 Go 运行链、WorkBuddy 和 Doubao 上游聊天链已打通，但 ChatGPT 有效凭据未通过验收，不能计为三渠道真实成功 E2E。

Docker 实构建证据：2026-10-09 `docker info` 返回 server `29.4.0`；首次 Go-only build 暴露 `CGO_ENABLED=0` 缺少 `github.com/google/uuid` go.sum 锁定项，补齐后 `docker compose -f docker-compose.go.yml build api browser-worker media-worker` 成功。三容器分别返回 API database health `ok`、browser/media worker `not_configured`（无 token 时的正确安全状态），browser image 内 `/usr/bin/chromium --version` 返回 Chromium `142.0.7444.59`，随后已清理本轮验证容器、网络和临时卷。Go-only source-deps/legacy-image gate 也返回 PASS。另已完成 SQLite/媒体备份恢复和同平台容器回滚 health smoke。Docker Hub 直连曾失败，实际基础镜像由本机已配置 mirror 拉取；Chromium profile 的平台登录 E2E 仍未通过。

2026-10-09 media contract 增量已验证：生成响应的 base64/远程 URL 归一化、下载失败 remote metadata fallback、Doubao Go watermark transformer、生成结果到 `/v1/files/{id}/content` 的 Key 隔离访问、Playground asset metadata、Go race/vet/clean build 和新 Compose smoke 均通过；该证据仅覆盖本地 deterministic contract，不替代真实 provider 生成和视觉质量 E2E，详见 `docs/migration-evidence/media-contract-2026-10-09.json`。

本次 readiness 最终仍为 `ready=false`：Go/CI/Compose/备份恢复/回滚门禁均通过；WorkBuddy 与 Doubao 聊天探针已通过，剩余阻断为 ChatGPT `credential_rejected`、Doubao profile `8a899...` 未认证，以及默认 Compose 和 Python/compat 删除门槛。继续切换前必须提供未过期的 ChatGPT Web 凭据和至少一个已认证 Doubao Chromium profile；重新取得三渠道全 `status=ok` 与 browser `status=ok` JSON 后，才能执行 G6 默认入口切换和 Python 删除。

- [x] 冻结 API、数据库、配置和安全基线
- [x] 建立 Go module、目录和 CI 工具链
- [x] 完成 SQLite、Fernet、Key 和会话兼容实现
- [x] 完成 domain、ports、registry、scheduler 和 protocols
- [x] 完成 `/v1` 数据面实现和 deterministic contract
- [x] 完成 `/admin/api` 管理面实现和 deterministic contract
- [x] 完成 WorkBuddy Go adapter、provision 和 refresh 实现
- [x] 完成 Doubao Go adapter、native QR 和 Go CDP browser worker 实现
- [x] 完成 ChatGPT Go adapter、token import、OAuth PKCE 和 refresh 实现
- [x] 完成 Go media provider result storage/去水印/file URL contract 和 Playground 非文本的本地资产边界（真实 provider 生成与视觉质量 E2E 仍未完成）
- [x] 完成 Go 测试、前端回归、race、clean build/run 和 deterministic gates
- [x] 完成 Go-only Docker image build 和容器 health smoke
- [ ] 完成三渠道真实平台 E2E
- [x] 完成停服备份恢复演练
- [x] 完成使用归档上一版 Go binary 的回滚演练（Linux/amd64 镜像 binary、备份恢复和 Docker health smoke 已归档；Windows 脚本仅适用于 Windows binary）
- [ ] 删除 Python、uv、旧 bridge 和源项目运行时依赖
- [x] 更新 README、Compose、配置模板、CI 和迁移文档
