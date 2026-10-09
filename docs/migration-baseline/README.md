# Go migration baseline

This directory contains migration-time snapshots generated from the current
Python implementation. The snapshots are inputs to Go contract tests; they do
not make the Python service part of the Go runtime.

Generate the contract and configuration snapshots from the repository root:

```powershell
uv run --directory services/api --no-sync python ../../scripts/freeze_go_baseline.py
```

To collect a database snapshot, pass a disposable copy explicitly:

```powershell
uv run --directory services/api --no-sync python ../../scripts/freeze_go_baseline.py --database .\data\all2api-copy.db
```

The collector opens the database in read-only mode and writes only schema DDL,
schema version, table names, and row counts. It never writes credentials or
account payloads. `security-golden-vectors.json` contains public fixture values
for the Go Fernet reader, API Key hash, and session-cookie signing shape. The
three channel adapter fixtures remain pending.

Go build gate:

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\verify-go-build.ps1
powershell -ExecutionPolicy Bypass -File .\scripts\verify-go-clean-build.ps1
```

The current Python Compose deployment is intentionally unchanged until the G6
switch criteria are met.

The Go production image definition is [Dockerfile.golang](../../Dockerfile.golang);
it builds the gateway and both worker binaries without copying Python sources.
The Go-only Compose topology is [docker-compose.go.yml](../../docker-compose.go.yml).
