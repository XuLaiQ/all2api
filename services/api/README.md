# services/api —— 网关后端

**当前状态：M2 进行中。** 已实现 SQLite schema、三渠道适配器、统一模型发现/聊天路由、账号聚合、别名跨渠道降级、WorkBuddy 可验证请求的账号级运行态、网关 Key 限流、SSE 代理和浏览器管理会话；网关账号租约调度与真实三渠道联调尚未完成。

## 职责

统一网关的后端：适配器层、调度引擎、存储层、对外协议层、管理面 API。
完整设计见 `docs/系统设计与实现文档.md`（架构 §1、ADR §2、域模型 §3、
适配器 §4、请求流水线 §5、调度 §6、存储 §7、协议 §8、管理面 §9、部署 §11）。

## 技术选型（已定）

| 项 | 选型 | 理由 |
|---|---|---|
| 语言 / 框架 | Python 3.13 + FastAPI + uvicorn | 三个源项目都是 FastAPI，协议转换代码可直接复用 |
| HTTP 客户端 | httpx（异步） | 原生支持 SSE 流式与超时分级 |
| 存储 | SQLite（WAL） | 单机运维场景够用；wb2api 已验证 13 张表 + 裸 SQL 可行 |
| 账号运行态 | `state.json` + 内存 | 高频读写、需原子落盘；放 DB 会引入无谓事务开销 |
| 迁移 | 自研 `_MIGRATIONS` 元组 | 不引 Alembic（单机工具，迁移项个位数） |
| 包管理 | uv + `pyproject.toml` | 与 JS 侧 pnpm 并列，各自管各自的依赖 |

**不引入**：Redis / 消息队列 / ORM / 微服务框架。请求量级是「每秒几十」而非「每秒几万」。

## 计划目录结构（M1 建立）

```
services/api/
├─ Dockerfile
├─ pyproject.toml
├─ uv.lock
├─ .python-version
├─ .env.example
├─ app/
│  ├─ main.py            # FastAPI 入口，挂载 /admin/api 与 /v1
│  ├─ config.py          # 读 A2A_* 环境变量
│  ├─ db.py              # SQLite + WAL + _MIGRATIONS
│  ├─ security.py        # 会话 Cookie 签名、密钥哈希、CSRF、防爆破
│  ├─ deps.py            # 依赖注入：KeyContext、DB 连接
│  ├─ models.py          # 后端域模型；HTTP DTO 以本服务 OpenAPI 为准
│  ├─ schemas.py         # 请求/响应模型
│  ├─ routers/
│  │  ├─ admin.py        # /admin/api/*（§9.2）
│  │  └─ gateway.py      # /v1/*（§8.1）
│  ├─ adapters/
│  │  ├─ base.py         # UpstreamAdapter / AdminAdapter 协议
│  │  ├─ registry.py     # ADAPTERS 注册表
│  │  ├─ wb/             # M1
│  │  ├─ doubao/         # M2
│  │  └─ chatgpt/        # M2
│  ├─ scheduler/
│  │  ├─ pool.py         # 账号池 + 租约 + 冷却/熔断
│  │  ├─ pick.py         # 四段式选号
│  │  └─ state.py        # state.json 原子落盘 + 择新恢复
│  └─ errors.py          # ErrKind 与错误响应构造
└─ tests/
```

## 本地启动

```bash
cd services/api
uv sync --extra dev
cp .env.example .env
uv run uvicorn app.main:app --reload --port 8080
```

- API 文档：http://localhost:8080/docs
- 健康检查：http://localhost:8080/admin/api/healthz（检查进程与 SQLite）
- 浏览器管理认证使用 `A2A_ADMIN_USERNAME` / `A2A_ADMIN_PASSWORD` 登录并获得 HttpOnly 会话 Cookie；脚本可继续使用 `Authorization: Bearer <A2A_ADMIN_TOKEN>`。默认要求配置至少 12 字符密码和至少 32 字符的非默认 `A2A_SESSION_SECRET`；仅本地开发可显式设置 `A2A_ALLOW_WEAK_ADMIN_PASSWORD=true` 放宽密码长度校验。
- 反向代理部署时将代理 IP/CIDR 配入 `A2A_TRUSTED_PROXIES`；不可信连接提交的 `X-Forwarded-*` 头会被忽略。
- WorkBuddy 定向选号与账号归因要求 `A2A_WB_TRACE_SECRET` 和 WorkBuddy `WB2A_TRACE_SECRET` 配置相同的随机值（至少 32 字节）。有共享密钥且存在同步快照时，网关按快照定向选号；无快照时保留上游选号，未配置共享密钥时仍可转发但网关不能记录账号归因。
- Doubao 使用 `A2A_DOUBAO_UPSTREAM_BASE` / `A2A_DOUBAO_API_KEY`；ChatGPT 使用 `A2A_CHATGPT_UPSTREAM_BASE` / `A2A_CHATGPT_AUTH_KEY`。
- 数据面从 `A2A_BOOTSTRAP_API_KEY` 初始化一把只存哈希、默认 60 RPM 的 Key（可用 `A2A_BOOTSTRAP_RPM` 调整）。Bootstrap Key 需 `sk-a2a-` 前缀、至少 40 字符；脚本管理令牌需 `wbt_` 前缀、至少 36 字符。

前端联调：按 `docs/前端开发准备.md` 创建 `web/.env.local`；
`VITE_PROXY_TARGET` 默认指向 `http://127.0.0.1:8080`，Vite 会代理 `/admin/api` 与 `/v1`。

## M1 的验收标准

- [x] `GET /admin/api/healthz` 检查进程与 SQLite
- [x] 管理员 Cookie 登录、会话查询与登出；失败登录锁定、CSRF Origin 校验和前端 401 过期回登录已实现（需配置登录凭据后完成环境联调）
- [x] WorkBuddy 模型列表通过 `GET /v1/models` 暴露
- [x] `/admin/api/accounts` 返回归一化账号（含 `ext{}`）
- [x] 网关 Key 可校验；非流式与 SSE 聊天均转发并写入 `request_logs`
- [x] 使用真实 WorkBuddy 实例完成端到端调用（`wb/cn:glm-5.2` 返回 200）
- [x] 用共享追踪密钥从 WorkBuddy 账号响应头映射并记录 `account_id`（响应账号头、Request ID 和本地映射均已核验）
- [x] `uv run pytest` 通过（49 项）

## M2 当前进展

- [x] `GET /v1/models` 聚合已配置渠道的模型并加渠道前缀
- [x] `POST /v1/chat/completions` 按模型前缀代理 WorkBuddy、Doubao、ChatGPT
- [x] `GET /admin/api/channels`、`GET /admin/api/channels/adapters` 提供通用适配器目录
- [x] `GET /admin/api/channels/{slug}/runtime` 查看渠道/模型级冷却与熔断状态
- [x] `GET /admin/api/accounts` 聚合各已配置渠道的归一化账号
- [x] 对匹配 request ID 的 WorkBuddy 账号记录成功、限流、服务错误与流中断运行态；独立 SQLite 持久化并在账号快照读取，用于运维观察
- [x] 路由别名 CRUD 与按顺序跨渠道重试；SSE 已开始后不重放请求
- [x] SQLite 持久化渠道/模型软冷却与渠道熔断，别名路由跳过冷却目标
- [x] SSE 中途断流后发送 OpenAI 错误帧并正常结束；只记录一次 502 熔断失败
- [x] WorkBuddy 按已同步账号快照挑选最多 3 个候选；每 API 进程每账号至多 1 个在途租约，上游收到受共享追踪密钥保护的 `X-A2A-Account-ID` 后严格校验并实际占用自己的账号池名额
- [ ] Doubao/ChatGPT 账号租约与定向选号；两家还缺少可信逐请求账号选择/身份回报契约
- [ ] 使用三套真实上游凭据完成端到端验收

真实联调记录：WorkBuddy 账号同步 2 个、模型发现 70 个，`wb/cn:glm-5.2` 调用成功且请求日志记录了匹配的账号。WorkBuddy 定向选号契约与网关租约现已实现，但本次未进行运行中的双端联调。Doubao 账号同步 3 个、模型发现 23 个，但聊天被上游风控拒绝（HTTP 429，错误码 `710022002`）；ChatGPT 账号同步 1 个，文本模型无可用账号，图片模型调用因上游额度不足返回 HTTP 429。后两项未验证成功路径；Doubao/ChatGPT 上游没有可信的逐请求账号标识，因此不把其渠道级失败归因到具体账号，也不声称网关已能指定上游账号。

## M3 部分进展

- [x] Anthropic `/v1/messages` 文本与 function tool 非流/SSE 转换已实现；图像及未知字段明确拒绝，并接受 `x-api-key` 或 Bearer Key。
- [x] OpenAI Responses `/v1/responses` 纯文本非流与 SSE 转换已实现；tools/续接/多模态及未知字段明确拒绝。
- [x] 请求明细与 `usage_daily` 在同一 SQLite 事务写入，按 UTC 记账日期、渠道、Key、模型累加请求与已报告 Token；未知用量单独计数。
- [x] 提供 `/admin/api/stats/summary`、`daily`、`by-channel`、`by-model`、`by-key` 查询，`days` 范围为 1-366，默认最近 30 天。
- [x] 被动解析完整 SSE usage 帧；原始字节不变地转发，不向上游注入未验证的 `stream_options`。
- [ ] 上游未报告 usage 的请求仍有未知 Token 数；不以文本长度估算。当前没有可信 credits/价格来源，统计 API 返回 `credits: null`。

## M4 部分进展

- [x] `/admin/api/overview` 一次返回近期待办、用量汇总/趋势与渠道运行态；总览页面以单请求加载
- [x] `GET /admin/api/audit-logs` 提供脱敏审计摘要、服务端分页和 actor/action/target/时间筛选；viewer 与 admin 均可读
- [x] `GET /admin/api/sysinfo` 与 `GET /admin/api/storage/health` 提供脱敏运行时、SQLite、运行态文件和磁盘健康信息
- [x] `GET /admin/api/metrics` 提供本地请求数、错误率、延迟、流式比例、渠道和账号池聚合；不触发上游探测
- [x] `POST /admin/api/logs/clear` 按配置保留期事务清理请求明细/旧用量聚合并写审计；仅 admin 可执行，重复调用幂等
- [x] 请求日志页提供 admin-only 过期日志清理确认、执行状态、删除结果和失败状态
- [x] Web 审计日志页支持 actor/action/target/时间筛选及分页，读取 admin/viewer 可见的脱敏审计摘要
- [x] `POST /admin/api/channels/{slug}/test` 显式测试一次渠道模型接口；不自动探测、不写缓存，错误按 404/409/502/504 归一
- [x] `GET /admin/api/logs` 提供服务端分页与 request/channel/model/status/error/stream/time 筛选；响应隐藏原始上游错误体、账号标识、IP 与 User-Agent。
- [x] 网关密钥列表、创建、编辑、轮换和软撤销已接入；创建/轮换仅返回一次明文，bootstrap Key 由配置管理。
- [x] 路由别名管理页面支持目标顺序、启停和删除；PUT/DELETE 强制 admin 角色，viewer 只读。
- [x] 渠道状态页读取本地配置与运行态；不自动触发账号同步或模型探测。
- [x] 账号池页面读取本地快照；管理员通过显式确认的 `POST /admin/api/accounts/sync` 才会访问上游并同步。
- [x] 模型目录读取已观测缓存；admin 启停会应用到公开模型列表、直连调用和别名目标。

阶段、契约和验收见 `docs/项目开发基线.md`、`docs/API契约.md` 与 `docs/系统设计与实现文档.md` §12。
