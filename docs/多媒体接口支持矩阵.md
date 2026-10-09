# All2API 多媒体接口支持矩阵

> 文档类型：技术规格说明  
> 创建时间：2026-09-28  
> 状态：Go contract 已迁移，真实 provider E2E 待验收
> 关联任务：P0-4 多媒体接口实现

## 概述

本文档明确 All2API 当前对多媒体接口的支持状态，区分"已声明"和"已实现"，并提供清晰的实现路线图。

> 2026-10-09 Go 迁移状态：三渠道生产 manifest 仍只声明 `chat`。Go 已完成 provider
> capability contract、生成结果归一化、100 MB 本地 asset store、远程 URL fallback、
> `/v1/files/{id}/content` 和 Go watermark transformer；真实平台调用、CDN 下载和视觉
> 质量未通过前不得把 image/video/search/editable 加入 manifest。

## 当前状态

Go 迁移期间以 `internal/adapters/registry` 的 manifest 为准：WorkBuddy、Doubao、
ChatGPT 当前都只声明 `chat`，未通过真实平台 E2E 的 provider capability 不会对外开放。
下方关于 `gateway.py`/Python manifest 的内容保留为迁移来源和行为对照，不是 Go 生产能力声明。

### 1. 框架层（✅ 已完成）

**gateway.py 路由**：
- ✅ `POST /v1/images/generations` → `image_generations`
- ✅ `POST /v1/video/generations` → `video_generations`
- ✅ `POST /v1/audio/generations` → `audio_generations`
- ✅ `POST /v1/search` → `search`
- ✅ `POST /v1/images/edits` → 返回 404 capability_not_supported
- ✅ `POST /v1/files` → 返回 404 capability_not_supported
- ✅ `POST /v1/ppt/generations` → 返回 404 capability_not_supported
- ✅ `POST /v1/psd/generations` → 返回 404 capability_not_supported

**native_runtime.py 能力支持**：
```python
CAPABILITY_PATHS: dict[str, str] = {
    "chat": "/v1/chat/completions",
    "image": "/v1/images/generations",
    "video": "/v1/video/generations",
    "audio": "/v1/audio/generations",
    "search": "/v1/search",
}
```

**能力调度流程**：
```
客户端请求
  ↓
gateway._capability_request()
  ↓
_dispatch_chat(capability="image")
  ↓
_resolve_capability_targets() - 过滤支持该能力的 channel
  ↓
adapter.invoke_capability()
  ↓
native_runtime.invoke_capability() - 发送到上游
```

### 2. Manifest 声明状态

#### 2.1 当前 Go 生产 manifest

| 渠道 | 当前 Go 声明的能力 | 当前门禁状态 |
|------|-------------------|--------------|
| WorkBuddy | `chat` | 真实聊天探针已通过 |
| Doubao | `chat` | 真实聊天探针已通过；媒体 E2E 未通过前保持 chat-only |
| ChatGPT | `chat` | 真实平台探针未通过，保持 chat-only |

下表是迁移来源中的 Python manifest，仅用于审计输入和行为对照，不代表 Go
生产运行时已经声明这些能力。

#### 2.2 迁移来源 Python manifest（历史对照）

| 渠道 | 声明的能力 | 真实支持状态 |
|------|-----------|-------------|
| **WorkBuddy** | `chat` | ✅ 完全支持 |
| **ChatGPT** | `chat`, `image`, `search`, `ppt`, `psd` | ⚠️ 仅 chat 实现；其他未验证 |
| **Doubao** | `chat`, `image`, `video`, `audio`, `file` | ⚠️ 仅 chat 实现；其他未验证 |

### 3. 具体能力实现状态

#### 3.1 Chat（聊天补全）
**状态**：✅ 全面支持

- ✅ WorkBuddy: 已实现，已测试
- ✅ ChatGPT: 已实现，已测试
- ✅ Doubao: 已实现，已测试
- ✅ 支持流式和非流式
- ✅ 支持工具调用

#### 3.2 Image（图像生成）
**状态**：⚠️ 框架存在，未实现真实调用

**声明渠道**：
- ChatGPT (`capabilities=("chat", "image", "search", "ppt", "psd")`)
- Doubao (`capabilities=("chat", "image", "video", "audio", "file")`)

**当前行为**：
```python
# 请求示例
POST /v1/images/generations
{
  "model": "chatgpt/dall-e-3",
  "prompt": "A beautiful sunset",
  "n": 1,
  "size": "1024x1024"
}

# 实际发生：
# 1. gateway 识别 capability="image"
# 2. _resolve_capability_targets 过滤出支持 image 的 channel (chatgpt, doubao)
# 3. adapter.invoke_capability("image", ...) 
# 4. native_runtime 发送 POST {base_url}/v1/images/generations
# 5. 上游可能：
#    - 返回 404（不支持该路径）
#    - 返回 501（能力未实现）
#    - 返回真实结果（如果上游支持）
```

**需要验证**：
- [ ] ChatGPT 官方平台是否支持 DALL-E API
- [ ] Doubao 平台是否有图像生成接口
- [ ] 上游接口的请求/响应格式
- [ ] 凭据注入方式（Authorization vs Cookie）

#### 3.3 Search（网络搜索）
**状态**：⚠️ 框架存在，未实现真实调用

**声明渠道**：
- ChatGPT (`capabilities` 包含 `search`)

**当前行为**：
```python
POST /v1/search
{
  "model": "chatgpt/auto",  # 使用默认模型
  "query": "latest AI news",
  "max_results": 10
}

# 发送到 {chatgpt_base_url}/v1/search
```

**需要验证**：
- [ ] ChatGPT 平台搜索接口的实际路径和格式
- [ ] 搜索结果的归一化格式

#### 3.4 Video（视频生成）
**状态**：⚠️ 框架存在，未实现真实调用

**声明渠道**：
- Doubao (`capabilities` 包含 `video`)

**需要验证**：
- [ ] Doubao 是否有视频生成能力
- [ ] 接口格式和异步轮询机制

#### 3.5 Audio（音频生成）
**状态**：⚠️ 框架存在，未实现真实调用

**声明渠道**：
- Doubao (`capabilities` 包含 `audio`)

**需要验证**：
- [ ] Doubao 音频生成接口

#### 3.6 File（文件上传）
**状态**：❌ 明确不支持（返回 404）

**声明渠道**：
- Doubao (`capabilities` 包含 `file`)

**问题**：
- Manifest 声明了 `file` 但 CAPABILITY_PATHS 没有映射
- Gateway 有路由但返回 `_unsupported_capability`

#### 3.7 PPT/PSD 生成
**状态**：❌ 明确不支持（返回 404）

**声明渠道**：
- ChatGPT (`capabilities` 包含 `ppt`, `psd`)

**问题**：
- Manifest 声明但未实现

## 问题分析

### 核心问题：虚假承诺

Manifest 声明了能力，但没有真实实现：

```python
# chatgpt/manifest.py
capabilities=("chat", "image", "search", "ppt", "psd")
# ❌ 只有 chat 被验证，其他能力未实现
```

**风险**：
1. 用户调用 `/v1/images/generations` 时可能：
   - 得到 404（上游不支持）
   - 得到 500（格式不匹配）
   - 超时（轮询机制不存在）
2. 与 manifest 承诺不符
3. 无法通过 contract tests

## 解决方案

### 方案 A：保守方案（推荐用于 P0）

**目标**：确保 manifest 声明与实际能力一致

**步骤**：

1. **缩减 manifest 声明**（仅保留已验证能力）：

```python
# chatgpt/manifest.py
CHATGPT_MANIFEST = ChannelManifest(
    slug="chatgpt",
    display_name="ChatGPT",
    adapter_version="0.2.0",
    protocols=("openai", "anthropic", "responses"),
    capabilities=("chat",),  # ← 移除未实现的 image, search, ppt, psd
    # ...
)
```

```python
# doubao/manifest.py
capabilities=("chat",),  # ← 移除未实现的 image, video, audio, file
```

```python
# workbuddy/manifest.py  
capabilities=("chat",),  # ← 保持不变
```

2. **添加能力发展路线图文档**：

创建 `docs/能力发展路线图.md`：
```markdown
## 计划支持的能力

### P1-1: ChatGPT 图像生成（计划中）
- 调研 ChatGPT DALL-E 接口
- 实现请求映射
- 添加 E2E 测试
- 预计工期：3-5 天

### P1-2: ChatGPT 搜索（计划中）
- 调研 ChatGPT 搜索 API
- 实现结果归一化
- 预计工期：2-3 天
```

3. **更新文档明确当前支持范围**：

```markdown
# API 参考

## 当前支持的接口

### ✅ 聊天补全
- `POST /v1/chat/completions`
- `POST /v1/messages` (Anthropic 协议)
- `POST /v1/responses` (Responses 协议)
- 所有渠道（WorkBuddy, ChatGPT, Doubao）

### 🚧 计划中
- 图像生成（ChatGPT, Doubao）
- 网络搜索（ChatGPT）
- 视频/音频生成（Doubao）

### ❌ 不支持
- 图像编辑
- 文件上传
- PPT/PSD 生成
```

**优点**：
- 立即可完成（1-2 天）
- 诚实，无虚假承诺
- 为后续实现留下清晰路径

**缺点**：
- 功能范围缩小

### 方案 B：积极方案（适合 P1）

**目标**：实现已声明的核心多媒体能力

**步骤**：

1. **ChatGPT 图像生成实现**（5-7 天）：
   - 调研 ChatGPT 平台的 DALL-E 接口
   - 实现真实调用
   - 添加 E2E 测试
   - 支持 `dall-e-3`, `dall-e-2` 模型

2. **ChatGPT 搜索实现**（2-3 天）：
   - 调研搜索接口
   - 实现结果归一化
   - 添加测试

3. **Doubao 图像生成实现**（5-7 天）：
   - 调研 Doubao 图像生成接口
   - 浏览器集成（如需要）
   - 异步轮询机制

4. **移除不支持的能力声明**：
   - `ppt`, `psd` 从 ChatGPT manifest 移除
   - `video`, `audio`, `file` 从 Doubao manifest 移除（或标记为 alpha）

**预计工期**：10-14 天

### 方案 C：混合方案（推荐）

**阶段 1（P0，1-2 天）**：
- ✅ 缩减 manifest 到已验证能力
- ✅ 移除虚假能力声明
- ✅ 更新文档说明支持范围

**阶段 2（P1，3-5 天）**：
- 🔄 实现 ChatGPT 图像生成
- 📝 manifest 添加 `image` capability
- ✅ E2E 测试验证

**阶段 3（P1，2-3 天）**：
- 🔄 实现 ChatGPT 搜索
- 📝 manifest 添加 `search` capability
- ✅ E2E 测试验证

**阶段 4（P2，未来）**：
- Doubao 多媒体能力
- 视频/音频生成

## P0-4 验收标准（方案 C 阶段 1）

- [x] 所有 manifest.capabilities 声明的能力都有对应实现或明确标记为未实现
- [x] 未实现的接口返回 404 和清晰的错误说明
- [x] 文档明确列出支持矩阵和版本计划
- [ ] ~~图像生成在 Doubao 和 ChatGPT 可用~~ → 移至 P1
- [ ] ~~搜索在 ChatGPT 可用~~ → 移至 P1
- [x] Go contract tests 覆盖已实现的多媒体 adapter、media store、watermark 和 manifest gate；真实 provider E2E 仍单独验收

## 历史实施计划

以下计划保留为迁移来源记录。当前生产事实以 `internal/adapters/registry` 的 Go manifest
和 `docs/migration-evidence/` 证据为准，不能按下方 Python 路径执行。

### 立即执行（P0-4 阶段 1）

1. 更新 Go registry manifest：
   - `internal/adapters/registry/registry.go`: 三渠道只声明已验证的 `chat`

2. 创建能力发展路线图文档

3. 更新 API 文档说明支持范围

4. 验收：
   - 运行 Go contract tests 确保 manifest gate 一致
   - 确认 `/v1/images/generations` 对不支持渠道返回 404
   - 文档审查

### 后续计划（P1）

按需实现具体多媒体能力，每个能力作为独立任务：
- P1-X: ChatGPT 图像生成
- P1-Y: ChatGPT 搜索
- P1-Z: Doubao 图像生成

## 附录：Contract Test 模板

```python
# tests/test_image_generation_contract.py (未来实现)

@pytest.mark.asyncio
async def test_chatgpt_image_generation():
    """Contract: ChatGPT adapter supports image generation."""
    adapter = ChatGPTAdapter(...)
    
    request = {
        "model": "dall-e-3",
        "prompt": "A sunset over mountains",
        "n": 1,
        "size": "1024x1024"
    }
    
    response = await adapter.invoke_capability("image", request)
    
    assert response.status_code == 200
    body = response.json()
    assert "data" in body
    assert len(body["data"]) == 1
    assert "url" in body["data"][0]
```
