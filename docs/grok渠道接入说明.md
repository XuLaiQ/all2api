# Grok 渠道接入说明

当前 Go runtime 已接入 `grok` 渠道，运行时不依赖 `F:\token-p\grok\grok2api` 的代码、进程、数据库或账号目录。

## 当前范围

- 渠道 slug：`grok`。
- 凭据类型：Grok Web SSO token 或 Grok Build OAuth access/refresh token，保存于 All2API 加密 credential store。
- Web 调用协议：通过 Grok Web Gateway WebSocket 建立会话并转换为 OpenAI Chat Completions、Anthropic Messages 和 Responses。
- Build 调用协议：通过 `cli-chat-proxy.grok.com/v1` 的 `/models` 和 `/responses`，使用 CLI headers 与 OAuth Bearer。
- Web 模型：`grok-chat-fast`、`grok-chat-auto`、`grok-chat-expert`、`grok-chat-heavy`；Build 模型由 `/models` 动态返回。
- 账号 tier：`basic` 只暴露 fast；`super` 暴露 fast/auto/expert；`heavy` 暴露全部四个模型；未填写 tier 按 basic 处理。

参考项目中的 Grok Console、Imagine 图片/视频协议、Statsig 签名和浏览器清关属于独立能力，当前没有在 manifest 中声明，因此不会被网关误路由为已支持能力。

## 配置

```text
A2A_GROK_PLATFORM_BASE=https://grok.com
A2A_GROK_BUILD_BASE=https://cli-chat-proxy.grok.com/v1
# 访问 grok.com / auth.x.ai 需要代理时填写，例如 http://127.0.0.1:7890；留空使用进程的 HTTPS_PROXY
A2A_GROK_PROXY=
```

只在需要切换测试上游时修改该地址。token、Cloudflare cookie 和 user id 不应写入环境变量或渠道公共配置；它们只能通过管理员账号导入接口提交。

## 导入账号

管理员调用：

```http
POST /admin/api/channels/grok/accounts/provision/import
Idempotency-Key: grok-import-20261010-001
Content-Type: application/json
```

Web SSO 请求体示例：

```json
{
  "flow": "token-import",
  "idempotency_key": "grok-import-20261010-001",
  "payload": {
    "tokens": [
      {
        "sso_token": "<redacted>",
        "user_id": "<optional-grok-user-uuid>",
        "tier": "super",
        "cloudflare_cookies": "<optional-cookie-header>"
      }
    ]
  }
}
```

也可以把 token 作为 `tokens` 数组中的字符串传入。导入层会去重、脱敏生成账号 native id，并只把 token 及可选元数据加密写入 SQLite；账号列表、请求日志、审计日志和错误响应不会返回 token。

Build OAuth 文件使用 `accounts` 数组，字段包括 `provider: "grok_build"`、`access_token`、`refresh_token`、`client_id`、`user_id`、`email` 和过期信息。导入后账号 native id 使用 `build:<user_id>`，不会把 Build OAuth 账号发送到 Web SSO Gateway。

Build 令牌续期与重新登录：

- access token 到期前 2 分钟（或上游返回 401 时）自动用 `refresh_token` 向 `auth.x.ai` 续期，并立即把轮换后的 refresh token 写回加密存储；同一账号的并发请求共用一次续期。
- refresh token 被吊销或失效时，接口返回 `credential_rejected` 并提示重新登录。
- 账号管理中的“xAI 设备码登录”（flow `device-code`）会打开 xAI 授权页并显示确认代码，授权完成后自动新增账号；同一 xAI 用户再次登录会更新原账号的凭据。
- 重新导入同一账号的新凭据会覆盖旧凭据（导入结果中的 `updated`），不再被当作重复跳过。

## 调用

模型使用渠道前缀：

```json
{
  "model": "grok/grok-chat-fast",
  "messages": [{"role": "user", "content": "hello"}],
  "stream": true
}
```

Build OAuth 账号直接使用其动态目录中的模型，例如 `grok/grok-4.7`。Build 请求会发送到 `A2A_GROK_BUILD_BASE` 配置的 `/responses`，并使用 `x-grok-client-version=1.0.13`。

Gateway 会从 `grok` 账号池租约中选择账号，先发送 Grok Gateway `session.create`，收到会话绑定后发送用户消息和 `response.create`，再将 `response.chunk` 转换为统一流式输出。最终快照只补发尚未输出的文本，避免重复内容。

## 验证边界

脱敏 WebSocket fixture 和账号导入测试已覆盖本地协议链路：

```powershell
go test ./internal/adapters/grok ./internal/application/accounts
go test ./...
```

真实平台验收仍需要管理员提供有效的 Grok Web SSO token，并受 Grok 登录状态、Cloudflare cookie、出口 IP 和上游反机器人策略影响；本地测试通过不等于真实账号 E2E 已通过。
