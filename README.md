# All2API

All2API 将三个已完成反代的项目接入统一网关和管理控制台：WorkBuddy、doubao、chatgpt。调用方使用单一 Gateway Base URL 和网关签发的 API Key；后续平台通过适配器注册扩展。前后端在同一仓库维护，分别开发、分别管理依赖。

| 渠道 | 现有反代项目 |
|---|---|
| WorkBuddy | `F:\token-p\WorkBuddy\wb2api` |
| doubao | `F:\token-p\反代\doubao2api` |
| chatgpt | `F:\token-p\chatgpt2api` |

## 项目结构

```text
web/                 React + TypeScript 管理控制台
services/api/        FastAPI 网关服务
design/              UI 原型与设计说明
docs/                项目基线、API 契约、架构与开发规范
docker-compose.yml   本地前后端开发环境
```

对外调用默认使用 `http://localhost:8080/v1`，调用方携带网关发放的 `sk-a2a-*` Key；渠道选择使用 `wb/<model>`、`doubao/<model>` 或 `chatgpt/<model>`。前端使用 React、TypeScript、React Router、Vite 和 Axios；后端使用 Python 3.13、FastAPI、Pydantic Settings、httpx、SQLite 和 uv。M2 已接入三渠道适配器、统一模型/聊天入口、别名顺序降级和渠道/模型级持久化冷却。WorkBuddy 请求级账号归因和定向账号租约已实现，但本次未做双端真实联调；Doubao、ChatGPT 的成功路径仍受上游账号风控/额度阻塞，且缺少定向选择和可信逐请求账号回报契约，故三渠道首版验收尚未通过。

## 本地开发

要求：Node.js 20.19+、pnpm 10、Python 3.13.x、uv。前端和后端在不同终端启动。

启动前端：

```powershell
Set-Location web
pnpm install
Copy-Item .env.example .env.local
pnpm exec vite --host 127.0.0.1 --port 5173
```

打开 `http://localhost:5173`。Vite 将 `/admin/api` 与 `/v1` 代理到本机 `8080`。

启动后端：

```powershell
Set-Location services/api
uv sync --extra dev
Copy-Item .env.example .env
uv run uvicorn app.main:app --host 127.0.0.1 --port 8080 --reload
```

API 文档：`http://localhost:8080/docs`。`GET /admin/api/healthz` 检查进程与 SQLite。浏览器登录还需设置至少 12 字符的 `A2A_ADMIN_PASSWORD`，并将 `A2A_SESSION_SECRET` 换成至少 32 字符的随机值；静态 `A2A_ADMIN_TOKEN` 只用于脚本。

## Docker Compose

```powershell
Copy-Item .env.example .env
Copy-Item services/api/.env.example services/api/.env
docker compose up --build
```

Compose 同时启动前端与后端，API 健康后再启动 Web；数据库与 Python 环境分别保存在命名卷。打开 `http://localhost:5173`。默认 session secret 仅供本机开发，不能用于生产。

## 检查命令

```powershell
Set-Location web
pnpm lint
pnpm typecheck
pnpm build
Set-Location services/api
uv run ruff check .
uv run pytest
```

## 开发入口

先读[项目开发基线](docs/项目开发基线.md)和[API 契约](docs/API契约.md)，确认三渠道首版验收门槛及统一 URL/API Key 约定；再按[系统设计与实现文档](docs/系统设计与实现文档.md)的 M1-M5 阶段实施。前端约定见[前端开发准备](docs/前端开发准备.md)，后端运行方式见[后端说明](services/api/README.md)。

根 `.env.example` 用于 Compose；`web/.env.example` 用于 Vite；`services/api/.env.example` 用于后端。所有 `.env` 本地文件均不应提交。

M1 WorkBuddy 联调还需设置 `A2A_WB_UPSTREAM_BASE`、`A2A_WB_ADMIN_TOKEN`、`A2A_WB_DATA_KEY` 和 `A2A_WB_TRACE_SECRET`；同一个至少 32 字节的随机追踪密钥也要设置在 WorkBuddy 的 `WB2A_TRACE_SECRET`。账号关联头仅在共享密钥通过校验时生成，All2API 不会把它们转发给调用方。网关数据面 Key、WorkBuddy 凭据、管理密码和签名密钥默认留空或为开发占位值，需按两侧 `.env.example` 配置。

Doubao 和 ChatGPT 的统一调用与账号/模型读取分别通过 `A2A_DOUBAO_*`、`A2A_CHATGPT_*` 配置；对应上游服务必须可从 API 进程访问。Doubao 若未配置 API Key，需显式启用 `A2A_DOUBAO_PUBLIC_DATA_PLANE=true` 才会按上游公开数据面调用。

M2 Doubao / ChatGPT 上游分别通过 `A2A_DOUBAO_UPSTREAM_BASE`、`A2A_DOUBAO_API_KEY`、`A2A_CHATGPT_UPSTREAM_BASE` 和 `A2A_CHATGPT_AUTH_KEY` 配置；上游凭据只留在 API 环境中。
