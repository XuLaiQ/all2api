# services/api —— All2API 网关后端

## 定位

`services/api` 是 All2API 的后端交付单元，负责统一数据面、管理面、渠道 registry、账号 provision、路由调度、持久化、审计和计量。目标态由本仓库内置 WorkBuddy、Doubao、ChatGPT adapter/client/worker；不启动、导入、读取或请求三个源项目。

当前 checkout 仍处于迁移过渡：`app/adapters/{wb,chatgpt,provisioning}.py`、
`app/adapters/doubao_transport.py` 和 `routers/admin.py` 的兼容分支仍保留，但 bridge 默认关闭。
三渠道 native provisioner、二维码状态机、加密凭据和本地账号目录已接入；兼容代码不是新增渠道
的模板。账号列表不再从外部项目同步，旧 `/admin/api/accounts/sync` 路由已删除，`provision/import`
只处理管理员主动提交的账号材料。后端开发先读根目录 `docs/统一平台整合需求说明.md` 和
`docs/目录重构与迁移映射.md`，再读 `docs/目标架构与迁移方案.md`、`docs/渠道适配器开发规范.md`
和 `docs/账号新增流程.md`。

## 当前技术栈

Python 3.13、FastAPI、uvicorn、httpx、Pydantic Settings、SQLite/WAL、uv。前端和后端分别管理依赖；平台专用的 Playwright、curl/fingerprint 或 worker 依赖必须按 adapter extra 锁定，不得隐式依赖源项目环境。

## 当前实际目录

```text
app/
├─ main.py                 FastAPI 装配
├─ config.py               Settings
├─ db.py / credentials.py  SQLite schema、迁移和加密凭据
├─ security.py             管理会话、Key hash、CSRF
├─ domain/ application/ ports/
├─ routers/                admin、gateway、keys、models、routes、auth
├─ adapters/               渠道包、registry、native runtime、迁移兼容层
├─ scheduler/              pool.py、runtime.py
└─ protocols/              Anthropic/Responses 转换
tests/                     当前扁平测试集；目标按 unit/integration/contract/security/e2e 分层
```

目标目录分层和依赖方向见 `docs/企业开发规范.md`。新增功能不要继续创建平台专用 router 或重复表。

## 本地启动

```powershell
uv sync --extra dev
Copy-Item .env.example .env
uv run uvicorn app.main:app --host 127.0.0.1 --port 8080 --reload
```

健康检查：`GET http://127.0.0.1:8080/admin/api/healthz`；OpenAPI：`http://127.0.0.1:8080/docs`。管理员会话需配置强密码和随机 session secret；生产不得使用默认值。

### Doubao browser worker（可选）

Doubao 二维码登录的 Playwright worker 是本项目内置的可插拔组件，默认使用
`NullBrowserWorker`，因此 `uv sync --extra dev` 不会安装或启动 Chromium。需要启用
真实浏览器时执行 `uv sync --extra browser`，准备受控的 Chromium，然后设置
`A2A_DOUBAO_BROWSER_ENABLED=true` 和 `A2A_DOUBAO_BROWSER_EXECUTABLE`（可选）。
worker 在应用 lifespan 中启动和关闭，按 `A2A_DOUBAO_BROWSER_MAX_CONTEXTS`、
`A2A_DOUBAO_BROWSER_MAX_PAGES_PER_CONTEXT` 限制资源；断开后会清理旧上下文并尝试重启。
二维码登录完成后只将内存中的 storage state 交给加密 CredentialStore，不写入明文
`state.json`。Fake/Null worker 可用于单测和未配置环境。

## 目标开发规则

- router 只调用 application service；不得直接调用具体 adapter 或读取环境变量；
- 账号新增统一走 registry → `AccountProvisioner`，先选 channel 再按 manifest schema 处理；
- Key 授权统一走 `channels/models` scope，adapter 不决定权限；
- 平台请求、浏览器生命周期、凭据读写分别封装在 ports/infrastructure/adapter 内；
- 所有平台差异通过 manifest、mapper、错误映射和 `ext` 表达，禁止在通用核心增加平台 `if/elif`；
- 任何源代码迁移必须登记来源版本、许可证、目标模块和契约 fixture。

## 检查命令

```powershell
uv run ruff check .
uv run pytest
# 在仓库根目录执行无源项目 clean build/run 门禁
Set-Location ..\..
python scripts/check-doc-links.py
python scripts/verify-clean-build.py
```

合并前同时执行根目录前端门禁，并执行源项目隔离扫描：

```powershell
rg -n "F:\\token-p|wb2api|doubao2api|chatgpt2api|:7863|:7864|:9090|:8000" app tests Dockerfile pyproject.toml
```

命中时必须说明是迁移文档/fixture/兼容 bridge，运行时代码命中应阻断合并。

## 迁移验收状态

| 能力 | 当前状态 |
|---|---|
| 统一 `/v1`、管理会话、SQLite、基础 Key | 已有过渡实现 |
| registry/manifest/provision schema/Key scope | 已实现，native provisioner 已接入 |
| 模型/路由/日志/统计 | 部分已实现，需继续按目标 ports 重构 |
| WorkBuddy native client + QR provisioner | 已接入；本地 session、账号池和 refresh 已接入，真实平台账号和数据面 E2E 待验收 |
| Doubao native browser/profile + QR provisioner | 已接入；本地 session、账号池和 profile 生命周期已接入，真实 Chromium/browser worker 和平台调用待验收 |
| ChatGPT native OAuth/token provisioner | 已接入；本地 session、账号池和 OAuth refresh 已接入，真实平台 E2E 待验收 |
| 管理面 Settings/Users/Playground/渠道覆盖 | 已接入；Playground 流式、真实用户凭据和动态 provider 注册仍未实现 |
| 本地账号目录和同步接口边界 | provision 成功后写入本地 `accounts`；旧 `/accounts/sync` 已删除 |
| 源项目断开后 clean build/run | clean gate 已具备；正式发布仍需在隔离 checkout 执行并留存证据 |

完整阶段门槛、部署和发布验收见 `docs/项目开发基线.md` 与 `docs/部署运维与验收.md`。
