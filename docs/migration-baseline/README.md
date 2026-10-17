# Contract baseline

Frozen snapshots exported from the original Python implementation before it was
removed. They are now the contract inputs for the Go backend and are not
regenerated (the exporter was deleted together with the Python service).

| File | Used by |
| --- | --- |
| `routes.json` | `internal/transport/http/route_coverage_test.go` — every listed method + path must be registered in the Go `ServeMux`. Add new public routes here when you add them. |
| `database-schema.sql`, `database-snapshot.json` | Reference for `internal/infrastructure/persistence/migrations/0011_baseline.sql` (schema version 11). The snapshot holds only DDL, table names and row counts — no credentials or account payloads. |
| `security-golden-vectors.json` | Public fixture values for the Fernet reader, API key hash and session-cookie signing shape. |
| `config-map.json`, `openapi.json`, `metadata.json` | Historical reference for environment variables and the original OpenAPI surface. |

Go build gates:

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\verify\verify-go-build.ps1
powershell -ExecutionPolicy Bypass -File .\scripts\verify\verify-go-clean-build.ps1
```

The production image is defined by [Dockerfile](../../Dockerfile) and the local
topology by [docker-compose.yml](../../docker-compose.yml).
