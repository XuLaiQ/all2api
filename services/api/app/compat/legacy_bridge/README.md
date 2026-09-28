# Legacy Bridge 兼容代码

**状态**: ⚠️ 已弃用，仅用于迁移对照

**删除计划**: v1.0.0 正式版

## 使用限制

- ⚠️ 仅在显式设置 `A2A_LEGACY_BRIDGE_ENABLED=true` 时加载
- ❌ 不得用于新功能开发
- ❌ 不包含在生产镜像中

## 迁移完成标准

在以下条件全部满足后，可完全删除此目录：

- [ ] 三渠道 native E2E 全部通过（P0-1 完成）
- [ ] 源项目目录删除后 clean build 成功
- [ ] 至少 1 个月生产运行无 legacy 相关问题
- [ ] 所有依赖 legacy API 的客户端已迁移

## 文件清单

- `provisioning.py` - 旧 HTTP bridge 到源项目（仅被 admin.py 中的 legacy 路由使用）
- ~~`wb.py` - WorkBuddy 兼容 transport~~ (已删除 - 无引用)
- ~~`chatgpt.py` - ChatGPT 兼容 transport~~ (已删除 - 无引用)
- ~~`doubao_transport.py` - Doubao 兼容 transport~~ (已删除 - 无引用)

## 替代方案

请使用以下 native adapter：

- WorkBuddy: `app/adapters/workbuddy/adapter.py`
- Doubao: `app/adapters/doubao/adapter.py`
- ChatGPT: `app/adapters/chatgpt/adapter.py`

---

**创建时间**: 2026-09-28
**负责人**: 项目团队
**下次审查**: v1.0.0 发布前
