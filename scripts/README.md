# scripts

仓库级 PowerShell 脚本，按用途分目录。所有脚本都按自身位置定位仓库根目录，可以在任意当前目录下执行：

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\<目录>\<脚本>.ps1
```

不需要 PowerShell 的检查用 Go 实现，放在 `tools/`：

| 命令 | 用途 |
| --- | --- |
| `go run ./tools/doclinks` | 检查 Markdown 本地链接；默认跳过 `docs/archive/`，`-include-archive` 可一并检查 |
| `go run ./tools/supplychain` | `go mod verify`，以及实际编译进 `cmd/` 的模块是否在缓存中、是否带许可证文件（`-json` 输出机器可读结果） |

公开路由覆盖由 `go test ./internal/transport/http` 中的 `TestEveryContractRouteIsRegistered` 校验。

## verify/ —— 确定性门禁（不访问真实平台）

| 脚本 | 用途 |
| --- | --- |
| `verify-go-build.ps1` | `go test`、`go vet` 和三个 `cmd/` 二进制的构建 |
| `verify-go-clean-build.ps1` | 用临时 SQLite 和随机端口启动服务并检查 healthz；扫描运行时代码中的源项目路径/端口 |
| `verify-go-image.ps1` | 构建 `Dockerfile` 并确认镜像包含三个 Go 二进制且不含 Python（Docker 不可用时退出码 2） |
| `verify-go-backup-restore.ps1` | 从备份目录恢复到隔离位置并启动验证 |
| `verify-go-rollback.ps1` | 用归档的上一版 Go binary 做回滚演练 |
| `check-release-readiness.ps1` | 汇总发布前置条件（Compose、CI、供应链、LICENSE、Go-only、备份、E2E 证据），输出 JSON blocker 列表 |

## probe/ —— 真实平台探针（需要账号，输出脱敏）

| 脚本 | 用途 |
| --- | --- |
| `probe-go-platforms.ps1` | 在 SQLite 副本（默认 `.\data\all2api.db`）上对各渠道执行 models/chat 探针 |
| `probe-go-browser.ps1` | 用单个 Doubao 浏览器 profile 做 CDP 探针 |
| `probe-go-browser-profiles.ps1` | 对 profile 根目录下所有 profile 批量执行 `probe-go-browser.ps1` |
| `verify-chatgpt-web-import.ps1` | 在隔离数据库中验证 ChatGPT Web 账号导入与调用 |

## ops/ —— 运维

| 脚本 | 用途 |
| --- | --- |
| `backup-go-state.ps1` | 停服状态下备份 SQLite（含 WAL）和媒体文件，生成 SHA-256 manifest |
| `generate-go-sbom.ps1` | 构建 release binary 并生成依赖清单和 SHA-256 manifest |

Windows PowerShell 5.1 会按系统代码页读取无 BOM 的脚本，因此 `.ps1` 文件只使用 ASCII 字符。
