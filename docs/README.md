# All2API 文档索引

后端只有 Go 实现（`cmd/` + `internal/`）。各渠道真实平台 E2E 的状态以
[`migration-evidence/`](migration-evidence/) 中的脱敏证据为准。

## 需求与能力

| 文档 | 内容 |
| --- | --- |
| [统一平台整合需求说明](统一平台整合需求说明.md) | 项目目标、范围和边界 |
| [项目开发基线](项目开发基线.md) | 需求追踪、阶段 DoD 和测试分层 |
| [能力发展路线图](能力发展路线图.md) | 未实现能力和决策记录 |

## API 与安全

| 文档 | 内容 |
| --- | --- |
| [API 契约](API契约.md) | 数据面、管理面、认证、错误和分页约定 |
| [多媒体接口支持矩阵](多媒体接口支持矩阵.md) | 图片、视频等能力的实际支持情况 |
| [密钥与渠道授权规范](密钥与渠道授权规范.md) | Key scope、渠道/模型授权和审计要求 |

## 渠道

| 文档 | 内容 |
| --- | --- |
| [渠道适配器开发规范](渠道适配器开发规范.md) | 新增渠道 adapter 的规范 |
| [账号新增流程](账号新增流程.md) | provision flow、状态机和凭据边界 |
| [grok 渠道接入说明](grok渠道接入说明.md) | Grok Web SSO、模型 tier 和验收边界 |

## 开发与运维

| 文档 | 内容 |
| --- | --- |
| [企业开发规范](企业开发规范.md) | 目录、依赖、测试和安全门禁 |
| [前端开发准备](前端开发准备.md) | Web 工程约定 |
| [页面高度与滚动条开发规则](页面高度与滚动条开发规则.md) | 控制台布局规则 |
| [部署运维与验收](部署运维与验收.md) | 配置、备份、健康检查和发布门槛 |
| [脚本说明](../scripts/README.md) | 构建、验证、探针和运维脚本 |

## 数据目录

| 目录 | 内容 | 使用方 |
| --- | --- | --- |
| [`migration-baseline/`](migration-baseline/) | 冻结的公开路由、schema、配置和安全向量 | `internal/transport/http/route_coverage_test.go` |
| [`migration-evidence/`](migration-evidence/) | 脱敏的平台探针和门禁证据 | `scripts/verify/check-release-readiness.ps1` |
| [`archive/`](archive/) | 过期的计划、会话记录和 Python 时期的设计与迁移文档，仅供追溯 | — |

`migration-baseline/` 和 `migration-evidence/` 被测试和脚本按固定路径读取，移动前需同步修改。
