#!/usr/bin/env python3
"""Freeze migration inputs without starting the Python HTTP server.

The generated files are migration fixtures, not production configuration. A
database path is optional and, when supplied, is opened read-only through
SQLite's URI mode. Only schema DDL, schema version, and row counts are saved.
"""

from __future__ import annotations

import argparse
import json
import sqlite3
import sys
from datetime import UTC, datetime
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
API_DIR = ROOT / "services" / "api"
DEFAULT_OUTPUT = ROOT / "docs" / "migration-baseline"


def load_app():
    sys.path.insert(0, str(API_DIR))
    from app.main import app  # noqa: PLC0415 - migration-time source import

    return app


def freeze_openapi(output: Path) -> dict[str, object]:
    document = load_app().openapi()
    (output / "openapi.json").write_text(
        json.dumps(document, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )

    routes: list[dict[str, object]] = []
    for path, path_item in sorted(document.get("paths", {}).items()):
        for method, operation in sorted(path_item.items()):
            if method not in {"get", "post", "put", "patch", "delete", "head", "options"}:
                continue
            routes.append(
                {
                    "method": method.upper(),
                    "path": path,
                    "operation_id": operation.get("operationId", ""),
                    "tags": operation.get("tags", []),
                }
            )
    (output / "routes.json").write_text(
        json.dumps(routes, ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )
    return {"openapi_paths": len(document.get("paths", {})), "routes": len(routes)}


def freeze_config(output: Path) -> int:
    settings_module = __import__("app.config", fromlist=["Settings"])
    fields = settings_module.Settings.model_fields
    rows = []
    for name, field in sorted(fields.items()):
        annotation = str(field.annotation).replace("<class '", "").replace("'>", "")
        rows.append(
            {
                "python_field": name,
                "environment": f"A2A_{name.upper()}",
                "type": annotation,
                "required": field.is_required(),
                "secret": "SecretStr" in annotation,
            }
        )
    (output / "config-map.json").write_text(
        json.dumps(rows, ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )
    return len(rows)


def freeze_database(output: Path, database_path: Path | None) -> dict[str, object]:
    if database_path is None:
        return {
            "status": "not_collected",
            "reason": "No database path was supplied; production data was not read.",
        }

    resolved = database_path.expanduser().resolve()
    if not resolved.exists():
        raise SystemExit(f"database does not exist: {resolved}")

    uri = f"file:{resolved.as_posix()}?mode=ro"
    with sqlite3.connect(uri, uri=True) as connection:
        schema_rows = connection.execute(
            """
            SELECT type, name, tbl_name, sql
            FROM sqlite_master
            WHERE name NOT LIKE 'sqlite_%'
            ORDER BY type, name
            """
        ).fetchall()
        schema_version = connection.execute(
            "SELECT COALESCE(MAX(version), 0) FROM schema_migrations"
        ).fetchone()[0] if any(row[1] == "schema_migrations" for row in schema_rows) else 0
        tables = {
            table: connection.execute(f"SELECT COUNT(*) FROM {table}").fetchone()[0]
            for table in sorted(row[1] for row in schema_rows if row[0] == "table")
        }

    ddl = "\n\n".join(
        row[3] for row in schema_rows if row[3]
    ) + "\n"
    (output / "database-schema.sql").write_text(ddl, encoding="utf-8")
    snapshot = {
        "status": "collected_read_only",
        "schema_version": schema_version,
        "tables": tables,
        "source_path": "omitted",
    }
    (output / "database-snapshot.json").write_text(
        json.dumps(snapshot, ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )
    return snapshot


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, default=DEFAULT_OUTPUT)
    parser.add_argument(
        "--database",
        type=Path,
        help="optional SQLite copy; it is opened read-only and never migrated",
    )
    args = parser.parse_args()

    args.output.mkdir(parents=True, exist_ok=True)
    contract = freeze_openapi(args.output)
    config_fields = freeze_config(args.output)
    database = freeze_database(args.output, args.database)
    metadata = {
        "generated_at": datetime.now(UTC).isoformat(),
        "source": "services/api/app (migration-time read-only inspection)",
        "contract": contract,
        "config_fields": config_fields,
        "database": database,
        "secrets_written": False,
    }
    (args.output / "metadata.json").write_text(
        json.dumps(metadata, ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )
    print(json.dumps(metadata, ensure_ascii=False, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
