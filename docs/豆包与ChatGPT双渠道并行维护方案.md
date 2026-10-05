# 豆包与 ChatGPT 双渠道并行维护方案

> **更新日期**: 2026-09-29
> **文档版本**: v1.0
> **状态**: 设计完成，待评审后实施（本文不含代码变更）
> **目标**: 让豆包与 ChatGPT 作为两个**对等的一等渠道**长期并存于同一项目，各自可独立维护、独立失效、互不牵连
> **关联文档**:
> [渠道适配器开发规范](./渠道适配器开发规范.md) ·
> [三渠道代码迁移矩阵](./三渠道代码迁移矩阵.md) ·
> [目标架构与迁移方案](./目标架构与迁移方案.md) ·
> [能力发展路线图](./能力发展路线图.md) ·
> [下一步工作计划](./下一步工作计划.md) ·
> [账号新增流程](./账号新增流程.md)

---

## 0. 本文要解决的问题

### 0.1 结论先行

项目的多渠并存架构本身**不需要重构**：registry + manifest + provisioner + UpstreamAdapter 这套 SPI 已经能装下 N 个渠道。当前做不到"同时维护"的真正原因是两条：

1. **豆包被声明成完整渠道，实际数据面是个桩**。它在 manifest 里声明了 `capabilities=("chat",)`，但运行时把 OpenAI 格式的请求 POST 到豆包官网首页，拿回 HTML 当回答，并且在调试台里记成 `status=200` 成功。
2. **共享传输层已经混入渠道分支**。`NativeHttpAdapter._prepare_chat_payload()` 里写死了 `if self.channel == "wb"`，这是[开发规范 §9](./渠道适配器开发规范.md) 明令禁止的写法。只要它留着，"加/改一个渠道"就等于"改一次核心"，两个渠道不可能独立维护。

再叠加两个共享层缺陷（这次实测发现），使得两边同时不可用：

3. 豆包的 HTTP 客户端继承 Windows 系统代理，二维码登录随机超时失败。
4. ChatGPT 的模型目录靠硬编码兜底常量，而上游对所有兜底模型都返回"不存在/不支持"。

### 0.2 实测证据（可复现）

以下均为本次排查的真实观测，不是推断：

| # | 观测 | 命令/入口 | 结果 |
|---|---|---|---|
| E1 | 豆包数据面目标 | `POST https://www.doubao.com/v1/chat/completions` | **HTTP 200**，`content-type: text/html`，**268,955 字节** SPA 首页 HTML |
| E2 | 豆包登录侧协议 | `GET /passport/safe/csrf_token/` + `GET /passport/web/get_qrcode/`（`trust_env=False`） | **200**，`error_code:0`，返回真实 base64 二维码 → **登录协议是好的** |
| E3 | 系统代理被继承 | `urllib.request.getproxies()` | `{'http': 'http://127.0.0.1:7892', 'https': ...}`（注册表 `ProxyEnable=1`） |
| E4 | 代理导致的抖动 | `NativeDoubaoQrWorker.start_qr_login()` ×4（默认 `trust_env`） | **1 次 `ReadTimeout` 失败**，3 次成功；注入 `trust_env=False` 的 client 后 100% 成功 |
| E5 | `models` 表实际内容 | `SELECT channel, COUNT(*) FROM models` | `wb 23` / `chatgpt 2` / **`doubao 0`** |
| E6 | ChatGPT 兜底模型被拒 | `POST /backend-api/codex/responses`，`model=gpt-5.5` | **404** `The model \`gpt-5.5\` does not exist or you do not have access to it.` |
| E7 | 同上前缀变体 | `gpt-5.5-codex` / `gpt-5` / `gpt-5-codex` / `gpt-5.1-codex` / `gpt-5.2-codex` / `codex-mini-latest` | **400** `... is not supported when using Codex with a ChatGPT account.` |
| E8 | 真实目录接口 | `GET /backend-api/codex/models?client_version=0.50.0` | **200 `{"models":[]}`**（抽样 3 个账号一致） |
| E9 | 账号权益 | 凭据 `plan_type` | 10/10 账号为 **`free`** |
| E10 | 健康探针 | `GET /backend-api/me` | **403 HTML**（而 `/backend-api/codex/*` 鉴权是通过的） |
| E11 | 账号池现状 | `accounts` 表 | `chatgpt 10 ready` / `doubao 2 needLogin` / `wb 1 ready`；`credentials` 表 **doubao 0 条** |
| E12 | 僵尸账号 | `data/doubao/profiles/*/meta.json` | 2 个 profile，`"status": "needLogin"`，`ext.credential_ref=""` |

### 0.3 差距矩阵

按[开发规范](./渠道适配器开发规范.md)的 SPI 维度逐项对照（✅ 达标 / ⚠️ 部分 / ❌ 缺失）：

| SPI 维度 | WorkBuddy（参照样板） | 豆包 | ChatGPT |
|---|---|---|---|
| `manifest` 完整性与版本 | ✅ | ✅ | ✅ |
| `account_flows` 声明 | ✅ | ✅ | ✅ |
| `AccountProvisioner` 全流程 | ✅ | ⚠️ 能建 profile/导 Cookie，**状态不回写、失败留僵尸行** | ✅ 导入 + OAuth PKCE |
| `UpstreamAdapter.list_models` | ✅ | ❌ 0 条，refresh 必失败 | ⚠️ 返回兜底常量，上游不认 |
| `UpstreamAdapter.invoke/invoke_stream` | ✅ 原生 HTTP + realm 解析 | ❌ **桩**（指向官网首页） | ⚠️ 协议可用，目录/权益判错 |
| `health` | ✅ | ⚠️ 只报 worker | ❌ 探针 403 恒失败 |
| `AccountMapper` | ✅ | ⚠️ 状态单一来源未回写 | ✅ |
| `ErrorMapper` | ✅ | ⚠️ 错误笼统、不可操作 | ⚠️ 有分类，但 502 掩盖真因 |
| 传输策略（代理/超时/UA） | 未隔离 | ❌ 继承系统代理 | ✅ 有显式 proxy 配置 |
| `capabilities` 声明诚信 | ✅ | ❌ 声明 `chat` 但无实现 | ✅ |
| 契约测试 | 部分 | 有单测，无数据面 contract | 有单测，无目录/权益 contract |

---

## 1. 设计原则

后续所有改动都必须可回溯到这 4 条：

| # | 原则 | 反面教材（本次实测） |
|---|---|---|
| P1 | **核心代码里不出现渠道名**。渠道差异只能通过注入的钩子/port 表达 | `native_runtime.py` 的 `if self.channel == "wb"` |
| P2 | **共享的只放真正共享的**：传输策略、凭据存储、账号池调度、provision 会话/幂等、错误分类 | 代理继承策略没被抽象，导致豆包单独踩坑 |
| P3 | **"未实现"是一等状态**，必须可声明、可展示、可测试；禁止用"看起来成功"掩盖 | HTML 冒充回答、兜底模型 ID、`needLogin` 号占池、失败不报错 |
| P4 | **能力声明由实现推导**，不由人手写。`transport is None ⇒ capabilities == ()` | 豆包硬编码 `capabilities=("chat",)` 却无实现 |

---

## 2. 目标架构

### 2.1 分层与装配点

```text
共享层（两渠道复用，只维护一份）
├─ infrastructure/http/transport.py      ★新建：唯一的 httpx 客户端工厂（代理/超时/UA/连接池）
├─ adapters/native_runtime.py            传输骨架：请求、流式、错误；渠道差异全部走注入钩子
├─ infrastructure/credentials.py         加密凭据存储（已有）
├─ infrastructure/provision_state.py     provision 会话 + 幂等（已有）
├─ scheduler/pool.py                     账号池选号/租约/冷却（已有）
└─ application/accounts|channels/service.py

渠道层（各自独立，互不 import）
├─ adapters/doubao/
│  ├─ manifest.py            已有
│  ├─ provisioner.py         已有（需修状态机）
│  ├─ mapper.py / errors.py  已有
│  ├─ transport/             ★新建：A/B 双通道 + 路由器
│  └─ tests/contract/        ★新建
├─ adapters/chatgpt/
│  ├─ manifest.py / adapter.py / client.py / codex_client.py / oauth_client.py  已有
│  ├─ models.py              ★新建：按账号权益取真实目录
│  ├─ credential_kind.py     ★新建：显式凭据类型
│  └─ tests/contract/        ★新建
└─ adapters/workbuddy/       作为"完整原生渠道"的参照样板

唯一装配点（规范 §4）
└─ adapters/registry.py      只在这里决定"某渠道的数据面用什么实现"
```

### 2.2 共享层：统一传输工厂（解决代理继承）

**问题**：`httpx.AsyncClient` 默认 `trust_env=True`，会读取 Windows 注册表代理。实测 E3/E4 证明这会让豆包请求随机失败，而同一条链路对 ChatGPT 无害（因为它有显式 proxy 配置）—— 这种"某个渠道单独踩坑"正是共享策略缺失的症状。

```python
# app/infrastructure/http/transport.py  ★新建
def build_client(
    *,
    proxy: str = "",              # 渠道级显式代理；空字符串 = 直连
    timeout: float = 300.0,
    connect_timeout: float = 10.0,
    trust_env: bool = False,      # 默认不继承环境/系统代理
) -> httpx.AsyncClient: ...
```

**规则（写进规范）**：

1. 任何 adapter 建 HTTP 客户端**必须**经过本工厂，不得直接 `httpx.AsyncClient(...)`；
2. 默认 `trust_env=False`；需要代理时由渠道自己的配置项显式给出（如 `A2A_CHATGPT_PROXY`、`A2A_DOUBAO_PROXY`）；
3. 代理/超时/UA 属于渠道配置，走 `config_schema`，**不得从进程环境隐式读取**（规范 §55：adapter 不得直接读环境变量）。

**收益**：一次修复同时覆盖豆包登录、豆包数据面、ChatGPT、WorkBuddy；且以后新增渠道默认免疫。

### 2.3 共享传输骨架：把渠道分支换成注入钩子（ADR-1）

`NativeHttpAdapter` 现有的构造参数里已经有两个正确的钩子（`base_url_resolver`、`credential_headers_resolver`），说明设计方向本来是对的，只是没走完。补齐为 5 个：

```python
# app/adapters/native_runtime.py
BaseUrlResolver  : (credentials) -> str                       # ✅ 已有（wb 用它按 realm 选域名）
HeaderResolver   : (credentials) -> Mapping[str, str]         # ✅ 已有（wb 用它注入鉴权头）
RequestPreparer  : (payload, credentials) -> Mapping          # ★新增（替代 if channel == "wb"）
ResponseDecoder  : (raw_bytes) -> bytes                       # ★新增（平台 SSE → OpenAI 兼容 SSE）
ModelsReader     : (credentials) -> list[Mapping]             # ★新增（目录来源）
HealthProbe      : (credentials) -> Mapping                   # ★新增（探针）
```

改造后的装配示例：

```python
# wb（保持现有行为，只换写法）
wb_runtime = NativeHttpAdapter(
    WORKBUDDY_MANIFEST, wb_platform_base,
    chat_path=WorkBuddyClient.CHAT_PATH,
    credential_store=credential_store, channel="wb",
    base_url_resolver=wb_client.base_url_for_credentials,
    credential_headers_resolver=wb_client.runtime_headers,
    request_preparer=lambda p, c: prepare_chat_payload(p, realm=str(c.get("realm") or "cn")),
    models_reader=wb_client.list_models,
)

# doubao（transport 由 registry 决定，见第 3 节）
doubao_runtime = DoubaoTransportRouter(policy=..., browser=..., http=...)
```

**验收硬标准**：`grep -rnE '(channel|slug) == "' app/` 在 `adapters/` 之外**零命中**。这是 P1 阶段的唯一验收口径。

**当前基线：8 处**（已核对），处置方式各不相同，不能一刀切删掉：

| # | 位置 | 现状 | 处置 |
|---|---|---|---|
| 1 | [native_runtime.py:214](../services/api/app/adapters/native_runtime.py#L214) | `if self.channel == "wb"` 决定请求体加工 | **P1**：改为 `request_preparer` 注入（本节方案） |
| 2 | [gateway.py:1081](../services/api/app/routers/gateway.py#L1081) | wb 走 realm/模型过滤的候选选择，其他渠道走通用 `account_candidates` | **P1**：抽成 `channel_candidate_policy` port，由 wb 包提供实现；gateway 不再认识 wb |
| 3 | [gateway.py:1151](../services/api/app/routers/gateway.py#L1151) | wb 专用 `X-A2A-Trace-Secret` / `X-A2A-Account-ID` 请求头 | **P1**：抽成 `channel_request_headers` 钩子 |
| 4–8 | [admin.py:1487](../services/api/app/routers/admin.py#L1487)、[:1495](../services/api/app/routers/admin.py#L1495)、[:1516](../services/api/app/routers/admin.py#L1516)、[:1543](../services/api/app/routers/admin.py#L1543)、[:1554](../services/api/app/routers/admin.py#L1554) | legacy bridge 的 `/accounts/{channel}/onboarding/*` 分发 | **不属本方案**：归 P0-2 legacy 清理，随 bridge 退役整段删除；**明确不得作为新适配器模板**（规范 §3/§9 已有此要求） |

即 P1 需要真正消除的是第 1–3 处（3 类共享层分支），第 4–8 处等 P0-2 收尾。若 P1 结束时第 4–8 处仍在，验收口径按"`app/routers`、`app/application`、`app/scheduler` 中除 `compat/legacy_bridge` 调用点外零命中"执行，并在[迁移矩阵](./三渠道代码迁移矩阵.md)登记剩余豁免与删除期限。

### 2.4 能力声明诚信（ADR-2）

```python
# adapters/registry.py
doubao_transport = build_doubao_transport(settings)     # browser | http | auto | None
doubao_adapter = doubao.DoubaoAdapter(
    provisioner=doubao_provisioner,
    transport=doubao_transport,
    manifest=doubao.build_manifest(
        capabilities=doubao_transport.capabilities if doubao_transport else (),
    ),
)
```

由此自动获得三个正确行为：

1. `capabilities=()` 时前端渠道/模型下拉不给这个渠道，不再出现"能选但用不了"；
2. `/playground/chat` 对无 transport 的渠道直接返回 **501「no upstream call was made」**，而不是伪造 200；
3. 契约测试可以断言 `transport is None ⇒ capabilities == ()`，把"谎报能力"变成 CI 失败。

---

## 3. 豆包数据面：A + B 双通道设计

> 决策：**A（Playwright 浏览器桥）默认在线，B（纯 HTTP + 签名）作为加速通道**，两者实现同一个 `UpstreamAdapter` port，由路由器统一调度。

### 3.1 目标目录

```text
app/adapters/doubao/transport/
├─ __init__.py
├─ protocol.py      平台私有协议：请求体构造、device/签名参数、SSE 解析（★最易失效点，隔离在此）
├─ browser.py       通道 A：PlaywrightBrowserTransport（impl UpstreamAdapter）
├─ http.py          通道 B：HttpSignedTransport（impl UpstreamAdapter）
├─ router.py        DoubaoTransportRouter（impl UpstreamAdapter，对外唯一出口）
├─ models.py        目录：优先真实接口，空目录即"未实现"，禁止兜底常量
└─ errors.py        通道级错误分类（复用 adapters/doubao/errors.py 的 ErrorKind 体系）
```

### 3.2 通道 A：Playwright 浏览器桥

**原理**：在账号已登录的持久化 profile 里执行页面内 fetch，让页面自己计算 `sign`/`a_bogus` 等签名，直接消费豆包返回的 SSE。

**需要扩展的 port**（[doubao/browser.py:52](../services/api/app/adapters/doubao/browser.py#L52) 的 `BrowserWorker` 目前只有 QR 相关方法）：

```python
class BrowserWorker(Protocol):
    # ... 已有 start/stop/restart/health/is_alive/start_qr_login/restore_qr_login/
    #     poll_qr_login/complete_qr_login/cancel_qr_login/delete_account ...
    async def open_chat(
        self, account_id: str, profile_path: str, request: Mapping[str, Any]
    ) -> AsyncIterator[bytes]: ...                      # ★新增
    async def fetch_json(
        self, account_id: str, profile_path: str,
        path: str, *, method: str = "GET", body: Mapping[str, Any] | None = None,
    ) -> Mapping[str, Any]: ...                          # ★新增（目录/探针用）
```

**要点**：

| 项 | 设计 |
|---|---|
| 上下文复用 | 复用 `PlaywrightBrowserWorker._new_context(profile_path)` 的持久化 profile，避免每次冷启动 |
| 并发上限 | `browser.max_contexts`(默认 4) × `max_pages_per_context`(默认 2) = **8**；用 `asyncio.Semaphore` 排队，排队超时映射 `upstream_timeout`（可重试） |
| 崩溃恢复 | 复用现有 `restart()` + `_ensure_running()`；恢复失败直接抛 `BrowserWorkerUnavailableError`，由路由器降级到 B |
| 取消 | 客户端断开时关闭页面而非上下文，保持 profile 可用 |
| 安全 | 页面内 fetch 只允许白名单路径（同 ChatGPT 的做法），不得变成任意 URL 代理 |

### 3.3 通道 B：纯 HTTP + 签名

**原理**：对标 doubao2api 的 HTTP 路径，在 `protocol.py` 里实现聊天端点的请求体 + 签名 + SSE 解析。

| 项 | 设计 |
|---|---|
| 依赖凭据 | 加密凭据里的 `Cookie` + `device_id` / `web_id` / `fp` / `msToken`（`native_qr.py` 已收集，见 [doubao/provisioner.py:837](../services/api/app/adapters/doubao/provisioner.py#L837)） |
| 失效特征 | 豆包前端发版 → 签名算法漂移 → 上游返回 `protocol_error` / 空响应。**这是本通道的核心维护成本** |
| 漂移检测 | 每小时（可配）用池里最健康的账号做一次最小 chat 探针，失败即写审计 + 降权告警 |
| 并发上限 | `limits.concurrency` 配置（建议默认 4–8，受账号池规模约束） |
| 客户端 | 必须走 2.2 的 `build_client(...)`，`trust_env=False` |

### 3.4 双通道路由器（关键设计）

```python
# app/adapters/doubao/transport/router.py
class DoubaoTransportRouter:
    """对外唯一出口；A/B 的选择、降级与熔断都在这里，适配器之外无感知。"""

    manifest: ChannelManifest

    def __init__(self, *, browser, http, policy: TransportPolicy,
                 state: TransportStateStore, clock=time.time): ...

    async def health(self, ctx) -> Mapping:
        """同时返回两个通道的健康状态，供 /channels/doubao/runtime 展示。"""

    async def list_models(self, ctx) -> list[Mapping]:
        """目录优先取 A（页面内 fetch 最权威），失败回退 B；两者都空则返回空列表。"""

    async def invoke(self, request, account) -> Any: ...
    async def invoke_stream(self, request, account) -> AsyncIterator[bytes]: ...
    def map_error(self, error) -> Mapping: ...
```

**配置项**：`A2A_DOUBAO_TRANSPORT = auto | browser | http | off`（默认 `auto`）。

**`auto` 策略**（按错误语义决策，而非盲目重试）：

| 上游/通道反馈 | 判定 | 动作 |
|---|---|---|
| B 返回 `protocol_error` / 空响应 / 签名校验失败 | **结构性漂移**：算法跟不上了 | 提高 A 优先级 + 写审计 + 告警；B 熔断冷却 |
| A 返回 `BrowserWorkerUnavailableError` / contexts 占满 / 排队超时 | 资源性不可用 | 临时降级到 B，A 熔断冷却；资源释放后自动恢复 |
| 任一通道返回 `auth_required` / `credential_expired` | **账号问题，与通道无关** | **不切换**，把账号标记为需重新授权（切换只是把同一个错误换个路径再犯一次） |
| 任一通道返回 `rate_limited` | 上游限流 | 不切换；按 `retry_after` 冷却账号/模型 |
| 两通道都不可用 | 渠道级故障 | 渠道级熔断，网关选号时跳过豆包 |

**熔断状态存储**：新增 `transport_runtime_state(channel, transport, consecutive_failures, breaker_until, last_status, last_error_kind, updated_at, PRIMARY KEY(channel, transport))`。

> ⚠️ 不要复用 `channel_runtime_state`：它的主键是 `(channel, model)`，语义是"渠道×模型"，塞 transport 进去会让两套指标互相污染。

### 3.5 账号数据必须两通道通用（否则切换是空话）

A 需要 **profile 目录**（浏览器持久化上下文），B 需要 **加密 Cookie/device 参数**。要能自由切换，**同一批账号必须同时具备两者**：

```text
qr-login 成功路径     →  profile 落盘  +  加密凭据写入   （两条都要做）
cookie-import 成功路径 →  可选建 profile +  加密凭据写入
```

现状是 `native_qr.py` 的 `poll_qr_login` 已经采集了 Cookie + device 参数，Playwright worker 也能拿到 `storage_state`，缺的是**统一成同一个 canonical 凭据形状**并在 `ext` 里记录可用通道：

```json
// accounts.ext（示意，不含任何 Secret）
{ "credential_ref": "cred_doubao_xxx", "transports": ["browser", "http"], "protocol_version": 1 }
```

这样账号池选号时就能判断"这个号至少有一条通道可用"，UI 也能显示"浏览器可用 / 仅 HTTP 可用"。

### 3.6 并发、限额与 `limits` 声明

规范 §2 要求 `limits` 声明并发/超时/重试。双通道后需要按通道分别声明，并提供给调度器：

```python
limits={
    "transport": {
        "browser": {"concurrency": 8, "timeout_seconds": 300, "retryable": True,
                    "cost": "high",  "failure_mode": "resource"},
        "http":    {"concurrency": 6, "timeout_seconds": 120, "retryable": True,
                    "cost": "low",   "failure_mode": "algorithm_drift"},
    }
}
```

### 3.7 可观测性

| 位置 | 新增内容 |
|---|---|
| 请求日志 / 审计 `playground_chat` | `transport=browser\|http`、`transport_attempts=2` |
| `GET /admin/api/channels/doubao/runtime` | `transports: {browser: {...}, http: {...}}` 两通道独立健康度 |
| 账号池行 | 该账号可用通道（来自 `ext.transports`） |
| 渠道页 | 当前生效通道 + 上次漂移检测时间与结果 |

---

## 4. ChatGPT 侧对等能力补齐

### 4.1 模型目录以账号权益为准（ADR-3）

**现状缺陷**：[codex_client.py:34](../services/api/app/adapters/chatgpt/codex_client.py#L34) 在 `/backend-api/models` 返回 403 时**静默**退回 `_FALLBACK_MODELS = ("gpt-5.5", "gpt-5.5-codex")`，而实测 E6/E7 证明这两个 ID 上游全部拒绝。结果是"账号就绪、请求必败"。

**新增 `adapters/chatgpt/models.py`**：

```python
async def list_models(client, credentials) -> list[CanonicalModel]:
    if credential_kind(credentials) == "codex":
        values = await codex_models(client)     # GET /backend-api/codex/models?client_version=<ver>
    else:
        values = await web_models(client)       # GET /backend-api/models
    return values          # 空列表是合法结果，调用方不得替换为兜底常量

async def entitlement(client, credentials) -> Entitlement:
    """{'kind': 'codex'|'web'|'none', 'plan': str, 'reason': str}"""
```

**两条硬规则**（写进契约测试）：

1. **空目录不得写 `models` 表**。注意 [models.py:154](../services/api/app/routers/models.py#L154) 的 `upsert_model_cache()` 在 `rows` 为空时会执行 `DELETE FROM models WHERE channel = ?` —— 如果"空目录"直接走刷新，反而会把已有目录清空。刷新接口需区分 `status: "ok" | "empty" | "failed"`，`empty` 不写库。
2. **兜底常量必须删除**。目录为空时正确做法是给账号打 `no_entitlement` 标记并展示，而不是编造模型 ID 让每次请求都吃 502。

**账号状态**：`free` 号在池中显示"无 Codex 权益"；`account_candidates()` 选号时跳过，避免无效重试与账号冷却污染。

### 4.2 凭据类型显式化（ADR-4）

`is_codex_credentials()`（[codex_client.py:69](../services/api/app/adapters/chatgpt/codex_client.py#L69)）靠 `client_id / organization_id / id_token` 是否存在来猜。实测：文件导入（sub2api 导出）带这些标记 → 判对了；但 UI 的**「导入访问令牌」和「导入会话 JSON」只提取 `accessToken`** → 判定为 Web 令牌 → 走需要 sentinel/Turnstile 的网页通道 → 必然失败。

**改法**：

1. 导入时**显式确定类型**（让用户选，或按 JWT 的 `iss` / `client_id` / `chatgpt_account_id` 自动判定），写入凭据 `auth_mode ∈ {codex, web}`；`is_codex_credentials()` 已优先读该字段，只需补导入侧。
2. [chatgpt/mapper.py:117](../services/api/app/adapters/chatgpt/mapper.py#L117) 不再把缺失的 `refresh_token` / `id_token` 写成 `""`（空字符串会被当成"已设置"，导致 [provisioner.py:536](../services/api/app/adapters/chatgpt/provisioner.py#L536) 的刷新永远报 `no refresh token`）。

### 4.3 健康探针分流

实测 E10：`GET /backend-api/me` 对这些账号返回 **403 HTML**，而 `/backend-api/codex/*` 鉴权是通过的。当前 [chatgpt/manifest.py:15](../services/api/app/adapters/chatgpt/manifest.py#L15) 的 `health_checks` 用的正是 `backend-api/me` → 健康检查恒失败，把"无该接口权限"误判成"渠道不可用"。

**改法**：按 `credential_kind` 分流探针（codex → Codex 路径；web → `backend-api/me`），并把 403 归类为 `no_entitlement`（展示层语义）而非 `upstream_unavailable`（调度层语义）。

---

## 5. 豆包侧账号状态机修复

### 5.1 状态回写

[doubao/mapper.py:59](../services/api/app/adapters/doubao/mapper.py#L59) 的状态单一来源是 `meta.json`，而该字段被 [doubao/provisioner.py:176](../services/api/app/adapters/doubao/provisioner.py#L176) 写死 `needLogin` 之后**全项目无一处更新**。因此任何一次 re-upsert 都会把账号打回 `needLogin`，只有 `_apply_event()` / `import_accounts()` 里两处硬覆盖（`account["status"] = "ready"`）能救回来 —— 这是"两个来源打架"的结构性问题。

**改法**：`DoubaoProfileStore` 增加 `set_status(account_id, status)`，在扫码成功、Cookie 导入成功时落盘 `ready`，让 `meta.json` 成为唯一事实源，删除那两处硬覆盖。

### 5.2 失败不留僵尸账号

现状：[doubao/provisioner.py:549](../services/api/app/adapters/doubao/provisioner.py#L549) 的 `qr-login` 先 `profile_store.create()` + `record_account(..., credential_ref="")`，**然后**才去请求二维码。二维码请求一失败就抛异常，但账号行已落库 → 永久僵尸行（实测 E11/E12：2 个 `needLogin` 行 + 0 条凭据）。

**改法（二选一，推荐前者）**：

- **先成功再落库**：拿到二维码 challenge 之后再 `record_account`；
- **失败即回滚**：在 [provisioner.py:563](../services/api/app/adapters/doubao/provisioner.py#L563) 已有的异常清理分支里，补上 profile 目录 + `accounts` 行的回滚（现有代码只清了 session）。

**治理存量**：提供一次性清理入口（或手工删除那 2 行），并在账号池为 `needLogin` 行提供「继续授权 / 删除」动作，而不是让它无声占位。

### 5.3 状态枚举收拢（顺手补齐）

`app/domain/account.py` 目前不存在（只有 `domain/channel.py`、`domain/scope.py`），账号状态是散落的字符串（前端 `statusLabels` 里已有 `ready/busy/cooldown/limited/needLogin/captcha/disabled/expired/error/unknown`）。建议 P1 阶段补 `domain/account.py` 把状态收成枚举，避免"两个渠道各写各的字符串"导致调度器筛选条件漂移。

> 参考：这次还发现**调试台与网关的选号条件不一致**（[admin.py:2086](../services/api/app/routers/admin.py#L2086) 只排除 `disabled/expired`，而 [pool.py:138](../services/api/app/scheduler/pool.py#L138) 只接受 `ready/busy/cooldown/limited`），导致调试台会选中没有任何凭据的 `needLogin` 号。收拢枚举时应把筛选条件抽成一个共用函数。

---

## 6. 前端与可观测性

| # | 问题 | 位置 | 改法 |
|---|---|---|---|
| F1 | 新增账号失败时**不显示原因**：只看 `status===success`，`result.errors` 从未渲染 | [AccountOnboardingDialog.tsx:520](../web/src/features/accounts/AccountOnboardingDialog.tsx#L520) | 渲染 `added/skipped/refreshed/errors`；`status` 非成功时用红色告警逐条列出 |
| F2 | 调试台模型下拉为空时输入框被静默禁用，用户以为"坏了" | [PlaygroundPage.tsx:267](../web/src/features/management/PlaygroundPage.tsx#L267)、[:513](../web/src/features/management/PlaygroundPage.tsx#L513) | 下拉为空时给出明确文案（"该渠道暂无可用模型，请先刷新模型目录"）+ 一键跳转 |
| F3 | 渠道/账号页看不到"当前生效通道"与"账号可用通道" | AccountsPage / ChannelsPage | 展示 `ext.transports` 与 `transport_runtime_state` 摘要 |
| F4 | 前端产物可能比源码旧（`provision_idempotency` 里有 3 条 `doubao/create-profile` 的前端幂等键，但当前源码已隐藏该 flow） | `web/dist` | 发布流程固定"改前端必重新构建"，并在部署文档写明校验方式 |
| F5 | `create-profile` 已从 UI 隐藏，但串接 `qr-login` 的代码仍在 | [AccountOnboardingDialog.tsx:496](../web/src/features/accounts/AccountOnboardingDialog.tsx#L496) | 明确取舍：删除死代码，或把 `create-profile` 作为高级入口重新暴露 |

---

## 7. 契约测试与 CI 门禁

### 7.1 统一 contract suite（规范 §8 的落地）

```python
# tests/contract/conftest.py
@pytest.fixture(params=["wb", "doubao", "chatgpt"])
def channel(request, tmp_path):
    """从 registry 取注册项，注入 fake transport，禁止真实网络与真实凭据。"""
```

用例清单（每个渠道都要过同一套）：

| # | 用例 | 断言 |
|---|---|---|
| C1 | manifest 完整性 | 必填字段齐全；`capabilities` 与实际 transport 能力一致 |
| C2 | **能力诚信** | `transport is None ⇒ capabilities == ()` |
| C3 | 模型目录 | 成功 / **空** / 格式异常 / 超时 / 鉴权失败 五种分支都有确定行为 |
| C4 | chat | 非流 / SSE / 取消 / 上游断流 / usage 缺失 |
| C5 | 账号状态机 | start/poll/complete/import/cancel 的成功、重复、过期、取消、重试、凭据失效 |
| C6 | **失败不留僵尸账号** | provision 失败后 `accounts` 表无新增行、profile 目录已清理 |
| C7 | mapper 安全 | 不泄露 Secret；`ext` 可 JSON 序列化且版本兼容 |
| C8 | 错误分类 | 429 / 5xx / 404 / 连接错误 → 统一 `ErrorKind` |
| C9 | **传输不继承环境代理** | 设 `HTTP_PROXY`/`HTTPS_PROXY` 后断言请求未走代理（`trust_env is False`） |
| C10 | **禁止兜底常量** | 上游目录为空时，`models` 表不得出现任何该渠道新增行 |
| C11 | 通用性 | 新增 fake adapter 后，通用 API/UI 无平台专用改动 |

> C2 / C6 / C9 / C10 是这次实测发现的四个坑的直接回归门禁，必须先写测试再改代码。

### 7.2 能力矩阵自动生成

`scripts/gen_channel_matrix.py`：从 registry 的 manifest + `transport_runtime_state` 生成 `docs/渠道能力矩阵.md`，CI 校验"重新生成无 diff"。

作用：manifest 是唯一事实源，杜绝文档与实现漂移；任何一格从 ✅ 变 ❌ 都必须是一次有意识的提交（[迁移矩阵 §6](./三渠道代码迁移矩阵.md) 的"未完成 → 已内置"判定也据此自动推进）。

### 7.3 CI 门禁

1. `grep -rnE '(channel|slug) == "' app/` 在 `adapters/` 之外零命中（当前基线 8 处，处置见 §2.3；`compat/legacy_bridge` 调用点按登记的豁免与期限处理）；
2. 契约 suite 全绿；
3. 能力矩阵无 diff；
4. 无源项目验证脚本继续通过（已有，见[下一步工作计划](./下一步工作计划.md) P0-3）。

---

## 8. 实施阶段与任务清单

> 依赖顺序：P0 → P1 → (P2a ∥ P3) → P2b → P2c → P4。P3（ChatGPT）与 P2a（豆包通道 A）可并行。

### P0 止血（1–2 天，不改架构）

- [ ] 清理 2 个 `needLogin` 僵尸账号与对应 profile 目录
- [ ] 新建 `infrastructure/http/transport.py`，所有 adapter 改用；`trust_env=False`
- [ ] 豆包 `transport=None`，manifest `capabilities=()`（停止伪造 200；调试台改为明确 501）
- [ ] ChatGPT 删除 `_FALLBACK_MODELS`，目录为空时标记 `no_entitlement`，不写 `models` 表
- [ ] 模型刷新接口区分 `ok/empty/failed`，`empty` 不清库
- [ ] 前端渲染 `errors`/`added`（F1）

**验收**：调试台不再出现"成功但内容是 HTML"；ChatGPT 账号状态与实际权益一致；豆包请求得到明确 501 而非伪装成功。

### P1 对等基建（2–3 天）

- [ ] `NativeHttpAdapter` 增加 `request_preparer` / `response_decoder` / `models_reader` / `health_probe`，删除 `if channel == "wb"`（基线第 1 处）
- [ ] gateway 抽 `channel_candidate_policy` + `channel_request_headers` 钩子，wb 移出核心（基线第 2、3 处）
- [ ] `AdapterContext` 增加 `proxy` / `timeout`（渠道级，来自 `config_schema`）
- [ ] 补 `domain/account.py` 状态枚举 + 抽出共用选号筛选函数（修 F3 与选号不一致）
- [ ] 契约 suite 骨架 + C2/C6/C9/C10 四条门禁测试
- [ ] `tsconfig`/构建流程固定"改前端必重建"（F4）

**验收**：基线第 1–3 处分支消除（§2.3）；wb 行为不变（既有测试全绿）。

### P2a 豆包通道 A：Playwright 浏览器桥（5–8 天）

- [ ] `BrowserWorker` 协议扩展 `open_chat` / `fetch_json`；`PlaywrightBrowserWorker` 实现
- [ ] `transport/browser.py` 实现 `UpstreamAdapter`
- [ ] 信号量 + 排队 + 超时；崩溃恢复复用 `restart()`
- [ ] 页面内 fetch 路径白名单
- [ ] `transport/models.py` 真实目录

**验收**：豆包账号在调试台与网关真实返回内容；模型目录非空；24h 压测无内存/上下文泄漏。

### P2b 豆包通道 B：纯 HTTP + 签名（4–6 天）

- [ ] `transport/protocol.py`：请求体、签名、SSE 解析（★唯一易失效点）
- [ ] `transport/http.py` 实现 `UpstreamAdapter`
- [ ] 漂移检测探针（每小时，可配）+ 审计 + 告警
- [ ] 录制脱敏 fixture（签名输入/输出、SSE 片段）

**验收**：与通道 A 对同一 prompt 输出等价；签名漂移时能在 1 小时内被发现。

### P2c 双通道路由（2–3 天）

- [ ] `transport_runtime_state` 表 + 迁移
- [ ] `DoubaoTransportRouter` + `TransportPolicy` + `A2A_DOUBAO_TRANSPORT`
- [ ] 按 3.4 表的降级矩阵实现；账号级错误不切换
- [ ] `limits.transport` 声明 + 调度器读取
- [ ] 可观测性（3.7）

**验收**：故意让 A 不可用 → 请求自动走 B 且审计记录 `transport=http`；故意让 B 报 `protocol_error` → 自动切 A 并告警；两通道都停 → 渠道级熔断、网关跳过。

### P3 ChatGPT 权益与目录（3–5 天，可与 P2a 并行）

- [ ] `models.py`：codex / web 两类目录 + `entitlement()`
- [ ] `credential_kind.py` + 导入时显式 `auth_mode` + 不再写空字符串 token 字段
- [ ] 健康探针按类型分流；403 → `no_entitlement`
- [ ] `plan_type` 参与选号

**验收**：`free` 号在池中显示"无 Codex 权益"且不被网关选中；`plus/web` 号可正常对话。

### P4 门禁与文档（3–4 天）

- [ ] 契约 suite 补齐 C1–C11；CI 全绿
- [ ] `scripts/gen_channel_matrix.py` + CI diff 校验
- [ ] 更新[迁移矩阵 §6 进度表](./三渠道代码迁移矩阵.md)、[能力发展路线图](./能力发展路线图.md)、[部署运维与验收](./部署运维与验收.md)
- [ ] 把 2.2 的传输规则、2.3 的钩子契约写入[渠道适配器开发规范](./渠道适配器开发规范.md)

---

## 9. 验收标准（整体）

| # | 标准 | 判定方式 |
|---|---|---|
| A1 | 两个渠道在调试台与网关**都能真实对话** | 各发一条真实消息，返回真实内容 |
| A2 | 任一渠道故障**不影响**另一渠道 | 停掉豆包浏览器，ChatGPT 与 wb 请求全绿 |
| A3 | 核心代码零渠道分支 | 7.3 门禁 1 |
| A4 | 不再存在"声明了但没实现"的能力 | C2 门禁 + 能力矩阵无 ❌ 谎报 |
| A5 | 新增账号失败**必定**有可读原因 | 手工构造失败（缺 sessionid / 无效 token），前端逐条显示 |
| A6 | 失败不产生僵尸账号 | C6 门禁 |
| A7 | 任一通道失效可在 1 小时内被发现 | 漂移探针告警记录 |
| A8 | 新增第三个渠道不需改核心 | 用一个 fake adapter 走完 C1–C11 |

---

## 10. 风险与缓解

| # | 风险 | 概率 | 影响 | 缓解 |
|---|---|---|---|---|
| R1 | 豆包前端发版导致通道 B 签名失效 | 高 | 通道 B 不可用 | A 默认在线即兜底；`protocol.py` 隔离为唯一改动点；漂移探针 1 小时内告警 |
| R2 | 浏览器资源占用/崩溃（与既有 R2 同源） | 中 | 通道 A 抖动 | 信号量限流；复用 24h 压测与崩溃恢复；A 不可用时自动降级 B |
| R3 | ChatGPT free 号无 Codex 权益 | **已发生** | 每次请求 502 + 账号冷却 | P3 权益识别；选号阶段跳过；UI 明示 |
| R4 | 改造共享传输层引入回归 | 中 | 影响全部渠道 | 契约测试先行；wb 作为样板先迁移并保持行为不变 |
| R5 | 空目录刷新清空 `models` 表 | 中 | 已有目录丢失 | 刷新接口区分 `ok/empty/failed`；C10 门禁 |
| R6 | 浏览器桥引入任意 URL 代理风险 | 低 | 安全问题 | 路径白名单 + 审计 + 契约测试 |

---

## 11. 决策记录（ADR）

| ADR | 决策 | 理由 | 影响面 |
|---|---|---|---|
| **ADR-1** | 删除 `NativeHttpAdapter` 内的渠道分支，改为注入 5 个钩子 | 规范 §9 禁止核心写渠道 `if/elif`；否则渠道无法独立维护 | `native_runtime.py` + 三个渠道包 |
| **ADR-2** | `capabilities` 由 transport 推导，`transport is None ⇒ capabilities == ()` | 规范 §2「未实现不得声明」；根治"谎报能力" | manifest 构造 + registry + 前端渠道列表 |
| **ADR-3** | ChatGPT 模型目录以账号权益为准，**禁止兜底常量**；空目录不写库 | 实测 E6–E9：兜底 ID 上游全拒，导致"就绪但必败" | `chatgpt/models.py` + `models.py` 刷新接口 |
| **ADR-4** | 凭据类型显式化（`auth_mode`），不靠字段猜测 | 实测：UI 首个导入入口会把 OAuth 令牌误判为 Web 令牌 | 导入 UI + `mapper.py` + `codex_client.py` |
| **ADR-5** | 豆包采用 A+B 双通道，A 默认、B 加速；账号数据两通道通用 | A 不依赖签名逆向（抗发版），B 提供并发与延迟优势；失效点互不重叠 | `adapters/doubao/transport/` + 新状态表 + 调度器 |
| **ADR-6** | 通道熔断用独立表 `transport_runtime_state`，不复用 `channel_runtime_state` | 后者主键为 `(channel, model)`，混用会污染两套指标 | 迁移 + 调度器 + 运行时接口 |

---

## 12. 工作量与排序建议

| 阶段 | 内容 | 单人工作日 |
|---|---|---|
| P0 | 止血 | 1–2 |
| P1 | 对等基建 | 2–3 |
| P2a | 豆包通道 A | 5–8 |
| P2b | 豆包通道 B | 4–6 |
| P2c | 双通道路由 | 2–3 |
| P3 | ChatGPT 权益 | 3–5 |
| P4 | 门禁与文档 | 3–4 |
| **合计** | | **20–31 天** |

**两种排期**：

- **单人串行**：P0 → P1 → P2a → P2c → P2b → P3 → P4，约 4–6 周。**先让豆包真能用，再补加速通道**。
- **双人并行**：甲做 P0+P1+P2a+P2c，乙做 P3 → P2b，约 2.5–3.5 周。注意 P1 是共同前置，必须两人一起先完成。

**最小可用切片（MVP，约 1 周）**：P0 全部 + P2a 通道 A（暂不做路由器，`A2A_DOUBAO_TRANSPORT=browser` 固定）+ P3 的目录与权益识别。做完即可达成 A1/A2/A5/A6。

---

## 附：与既有文档的关系

| 文档 | 关系 |
|---|---|
| [渠道适配器开发规范](./渠道适配器开发规范.md) | 本文遵循其 SPI；P4 阶段需把 2.2 传输规则与 2.3 钩子契约回写进规范 |
| [三渠道代码迁移矩阵](./三渠道代码迁移矩阵.md) | 本文是其在「Doubao 数据面 / ChatGPT 目录」两行的具体实施设计；完成后需更新 §6 进度表 |
| [目标架构与迁移方案](./目标架构与迁移方案.md) | 本文落在其 M2–M4（逐渠道内置）阶段内，不改变目标拓扑 |
| [下一步工作计划](./下一步工作计划.md) | 本文把 P0-1（三渠道真实 E2E）中的 Doubao/ChatGPT 子任务细化为可执行任务清单 |
| [账号新增流程](./账号新增流程.md) | 第 5 节的状态机与回滚改动会影响该流程文档，实施后需同步 |
| [能力发展路线图](./能力发展路线图.md) | ADR-2 的"能力声明由实现推导"应与该路线图的多媒体能力标记规则统一 |

---

**变更记录**

| 版本 | 日期 | 变更 |
|---|---|---|
| v1.0 | 2026-09-29 | 首版：基于实测证据（E1–E12）确立双渠道对等设计、豆包 A+B 双通道、四阶段实施计划与 11 条契约门禁 |
