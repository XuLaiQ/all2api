# All2API

All2API 是一个统一的多渠道模型网关与管理控制台。目标是把 WorkBuddy、Doubao、ChatGPT 三类渠道的协议、账号池、账号新增、模型目录、密钥授权和调用入口收敛到**一个可构建、可部署、可测试的项目**中。

> 当前状态：三渠道 native 账号新增、二维码会话、加密凭据落库和本地账号目录已经接入；
> 数据面仍有迁移期兼容边界，真实平台账号 E2E、完整账号生命周期和最终 bridge 清理仍是发布门槛。
> 先阅读[统一平台整合需求说明](docs/统一平台整合需求说明.md)，再开始改代码。

## 先看清边界

`F:\token-p\WorkBuddy\wb2api`、`F:\token-p\反代\doubao2api`、`F:\token-p\chatgpt2api` 只作为一次性代码审计和迁移输入，不是 All2API 的运行时依赖。目标版本必须满足：

- 不导入三个源项目的 Python/Go 包；
- 不读取它们的数据库、账号文件或浏览器 profile；
- 不启动或请求它们的端口；
- 源目录被删除、离线或改名后，All2API 仍能安装、构建、测试和启动；
- 迁移后的代码、依赖、配置、测试和启动脚本全部归本仓库管理。

这里的“单一项目”指单一代码仓库、交付物、配置和生命周期；Doubao 的浏览器 worker 或 WorkBuddy 的内部 worker 可以保留进程隔离，但 worker 的代码和镜像必须在本仓库内。

## 用户能力

1. 在同一个管理面分别管理 `wb`、`doubao`、`chatgpt` 渠道、模型和账号。
2. 对外只提供一个 Gateway Base URL（默认 `http://localhost:8080/v1`）和网关签发的 `sk-a2a-*` API Key。
3. 创建/编辑 Key 时选择一个或多个允许渠道，并可进一步限制模型；空渠道列表的语义是“全部已启用且已配置渠道”。
4. 新增账号必须先选择渠道，再由该渠道的 `AccountProvisioner` 返回动态表单和 OAuth/二维码/导入流程。
5. 后续新增平台只增加适配器、凭据流程、配置和契约测试，不复制网关核心、数据库表或平台专用页面。

账号列表只维护本项目 provision 流程写入的本地目录；旧的“同步账号”接口已经删除，
`provision/import` 仅用于管理员主动提交本次新增账号的凭据材料。

四条开发主线：**按渠道管理** → **统一 URL + Key scope** → **先选渠道再新增账号** → **通过代码新增 adapter 扩展平台**。

## 仓库结构（目标结构，当前迁移中）

```text
web/                         React + TypeScript 管理控制台
services/api/app/            FastAPI 入口、应用服务、领域、端口和基础设施
services/api/app/adapters/   内置渠道适配器（目标：每渠道独立包）
services/api/app/scheduler/  跨渠道路由、账号池、冷却和熔断
services/api/tests/          单元、集成、适配器契约和安全测试
docs/                        架构、API、迁移、账号、密钥和工程规范
design/                      UI 原型，不是业务事实来源
```

当前 checkout 仍保留迁移期兼容实现：`app/adapters/{wb,chatgpt,provisioning}.py`、
`app/adapters/doubao_transport.py` 和 `routers/admin.py` 的兼容分支仍读取
`A2A_LEGACY_*` 配置，但 bridge 默认关闭；三渠道 native provisioner 和二维码状态机已在
本项目内运行。这些兼容模块只能用于迁移对照，不能作为新增渠道模板，最终发布仍需完成真实平台
E2E 和 bridge 清理。迁移边界和验收以以下文档为准。

## 文档导航

- [统一平台整合需求说明](docs/统一平台整合需求说明.md)：需求、边界、账号方法迁移、统一调用示例和完成定义，后续开发首先阅读。
- [目录重构与迁移映射](docs/目录重构与迁移映射.md)：当前过渡代码到企业级目标目录的逐文件映射和合并停止线。
- [目标架构与迁移方案](docs/目标架构与迁移方案.md)：目标态、迁移阶段、源项目隔离和回滚。
- [渠道适配器开发规范](docs/渠道适配器开发规范.md)：新增平台的 SPI、目录模板、注册和契约测试。
- [账号新增流程](docs/账号新增流程.md)：先选渠道、动态表单、三种账号流程和凭据安全。
- [密钥与渠道授权规范](docs/密钥与渠道授权规范.md)：统一 URL、Key scope、模型授权和审计。
- [企业开发规范](docs/企业开发规范.md)：目录、依赖、迁移、测试、CI、评审和安全门禁。
- [部署运维与验收](docs/部署运维与验收.md)：单项目部署、环境、备份、观测和验收矩阵。
- [API 契约](docs/API契约.md)：前后端及统一数据面 HTTP 契约。
- [项目开发基线](docs/项目开发基线.md)：需求追踪、阶段 DoD 和当前实现状态。
- [系统设计与实现文档](docs/系统设计与实现文档.md)：目标架构的技术细节。
- [前端开发准备](docs/前端开发准备.md)：Web 工具链、动态渠道表单和页面实施规则。
- [后端说明](services/api/README.md)：后端当前目录、启动和迁移状态。

## 本地开发（当前仓库）

要求：Node.js 20.19+、pnpm 10、Python 3.13.x、uv。

```powershell
Set-Location web
pnpm install
Copy-Item .env.example .env.local
pnpm exec vite --host 127.0.0.1 --port 5173
```

另开终端：

```powershell
Set-Location services/api
uv sync --extra dev
Copy-Item .env.example .env
uv run uvicorn app.main:app --host 127.0.0.1 --port 8080 --reload
```

打开 `http://localhost:5173`，API 文档为 `http://localhost:8080/docs`。

## 质量门禁

```powershell
Set-Location web
pnpm lint
pnpm typecheck
pnpm build
Set-Location ..\services\api
uv run ruff check .
uv run pytest

# 无源项目 clean build/run 门禁（随机端口和临时数据库）
Set-Location ..\..
python scripts/check-doc-links.py
python scripts/verify-clean-build.py
```

## Docker Compose（当前本地过渡拓扑）

```powershell
Copy-Item .env.example .env
Copy-Item services/api/.env.example services/api/.env
docker compose up --build
```

Compose 目前仍保留显式的 legacy bridge 变量名以支持迁移期联调，但默认关闭且模板值为空；它不会改变目标态“源项目零运行依赖”的验收要求。

CI 还会执行 `python scripts/verify-clean-build.py --docker` 和 API 镜像构建。

迁移阶段还必须执行“无源项目验证”：移除/改名三个 `F:\token-p` 目录、停止其端口，并在干净 checkout 中重复安装、构建、测试和启动。未通过该验证，不得把渠道标记为“已内置”。

根 `.env.example` 与 Compose 仍保留 bridge 过渡变量名，但不会填入 upstream 地址；详见[部署运维与验收](docs/部署运维与验收.md)。目标态配置不得要求 `A2A_*_UPSTREAM_BASE`、外部管理令牌或源项目数据库路径。
