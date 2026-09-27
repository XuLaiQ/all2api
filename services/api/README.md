# services/api —— All2API 网关后端

## 定位

`services/api` 是 All2API 的后端交付单元，负责统一数据面、管理面、渠道 registry、账号 provision、路由调度、持久化、审计和计量。目标态由本仓库内置 WorkBuddy、Doubao、ChatGPT adapter/client/worker；不启动、导入、读取或请求三个源项目。

当前 checkout 仍处于迁移过渡：`app/adapters/{wb,doubao,chatgpt,provisioning}.py` 是外部 HTTP bridge，`routers/admin.py` 仍包含渠道分支。它们是待迁移代码，不是新增渠道的模板。目标边界见 `docs/目标架构与迁移方案.md`、`docs/渠道适配器开发规范.md` 和 `docs/账号新增流程.md`。

## 当前技术栈

Python 3.13、FastAPI、uvicorn、httpx、Pydantic Settings、SQLite/WAL、uv。前端和后端分别管理依赖；平台专用的 Playwright、curl/fingerprint 或 worker 依赖必须按 adapter extra 锁定，不得隐式依赖源项目环境。

## 当前实际目录

```text
app/
├─ main.py                 FastAPI 装配
├─ config.py               Settings
├─ db.py                   SQLite schema/迁移（当前过渡实现）
├─ security.py             管理会话、Key hash、CSRF
├─ routers/                admin、gateway、keys、models、routes、auth
├─ adapters/               wb.py、doubao.py、chatgpt.py、registry.py、provisioning.py
├─ scheduler/              pool.py、runtime.py
└─ protocols/              Anthropic/Responses 转换
tests/                     随代码补充 unit/integration/contract/security/e2e
```

目标目录分层和依赖方向见 `docs/企业开发规范.md`。新增功能不要继续创建平台专用 router 或重复表。

## 本地启动

```powershell
uv sync --extra dev
Copy-Item .env.example .env
uv run uvicorn app.main:app --host 127.0.0.1 --port 8080 --reload
```

健康检查：`GET http://127.0.0.1:8080/admin/api/healthz`；OpenAPI：`http://127.0.0.1:8080/docs`。管理员会话需配置强密码和随机 session secret；生产不得使用默认值。

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
| registry/模型/路由/日志/统计 | 部分已实现，需按目标 ports 重构 |
| WorkBuddy 内置 client + provisioner | 待迁移，当前为 bridge |
| Doubao 内置 browser/client + QR provisioner | 待迁移，当前为 bridge |
| ChatGPT 内置 OAuth/token/client + provisioner | 待迁移，当前为 bridge |
| 源项目断开后 clean build/run | 未通过前不得宣称完成 |

完整阶段门槛、部署和发布验收见 `docs/项目开发基线.md` 与 `docs/部署运维与验收.md`。
