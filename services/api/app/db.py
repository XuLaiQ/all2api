from __future__ import annotations

import sqlite3
from collections.abc import Iterator
from contextlib import contextmanager
from pathlib import Path

from app.config import API_DIR

SCHEMA_VERSION = 4

_SCHEMA = """
CREATE TABLE IF NOT EXISTS channels (
    slug TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    adapter TEXT NOT NULL,
    upstream_base TEXT NOT NULL,
    auth_kind TEXT NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1,
    config TEXT NOT NULL DEFAULT '{}',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS accounts (
    id TEXT PRIMARY KEY,
    channel TEXT NOT NULL,
    native_id TEXT NOT NULL,
    name TEXT NOT NULL,
    kind TEXT NOT NULL,
    tier TEXT,
    status TEXT NOT NULL,
    status_override TEXT,
    enabled INTEGER NOT NULL DEFAULT 1,
    quota_used REAL NOT NULL DEFAULT 0,
    quota_total REAL NOT NULL DEFAULT 0,
    quota_unit TEXT NOT NULL DEFAULT 'none',
    expires_at INTEGER,
    success_count INTEGER NOT NULL DEFAULT 0,
    fail_count INTEGER NOT NULL DEFAULT 0,
    streak INTEGER NOT NULL DEFAULT 0,
    cooldown_until INTEGER,
    last_error TEXT,
    priority INTEGER NOT NULL DEFAULT 0,
    ext TEXT NOT NULL DEFAULT '{}',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE(channel, native_id)
);
CREATE INDEX IF NOT EXISTS idx_accounts_channel ON accounts(channel);
CREATE INDEX IF NOT EXISTS idx_accounts_status ON accounts(status);
CREATE TABLE IF NOT EXISTS channel_runtime_state (
    channel TEXT NOT NULL,
    model TEXT NOT NULL DEFAULT '',
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    breaker_streak INTEGER NOT NULL DEFAULT 0,
    soft_streak INTEGER NOT NULL DEFAULT 0,
    cooldown_until INTEGER,
    breaker_until INTEGER,
    last_status INTEGER,
    last_error_kind TEXT,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY(channel, model)
);
CREATE INDEX IF NOT EXISTS idx_channel_runtime_cooldown
    ON channel_runtime_state(channel, cooldown_until, breaker_until);
CREATE TABLE IF NOT EXISTS account_runtime_state (
    account_id TEXT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    success_count INTEGER NOT NULL DEFAULT 0,
    fail_count INTEGER NOT NULL DEFAULT 0,
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    breaker_streak INTEGER NOT NULL DEFAULT 0,
    soft_streak INTEGER NOT NULL DEFAULT 0,
    cooldown_until INTEGER,
    breaker_until INTEGER,
    last_status INTEGER,
    last_error_kind TEXT,
    updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_account_runtime_cooldown
    ON account_runtime_state(cooldown_until, breaker_until);
CREATE TABLE IF NOT EXISTS models (
    id TEXT PRIMARY KEY,
    channel TEXT NOT NULL,
    upstream_id TEXT NOT NULL,
    display_name TEXT NOT NULL,
    kind TEXT NOT NULL,
    caps TEXT NOT NULL DEFAULT '[]',
    context_window INTEGER,
    max_output INTEGER,
    multiplier REAL NOT NULL DEFAULT 1,
    enabled INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS idx_models_channel ON models(channel);
CREATE TABLE IF NOT EXISTS routes (
    alias TEXT PRIMARY KEY,
    strategy TEXT NOT NULL DEFAULT 'priority',
    enabled INTEGER NOT NULL DEFAULT 1,
    created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS route_targets (
    alias TEXT NOT NULL,
    position INTEGER NOT NULL,
    channel TEXT NOT NULL,
    model TEXT NOT NULL,
    weight INTEGER NOT NULL DEFAULT 1,
    PRIMARY KEY(alias, position),
    FOREIGN KEY(alias) REFERENCES routes(alias) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS api_keys (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    key_hash TEXT NOT NULL UNIQUE,
    prefix TEXT NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1,
    expires_at INTEGER,
    channels TEXT NOT NULL DEFAULT '[]',
    models TEXT NOT NULL DEFAULT '["*"]',
    limit_tokens INTEGER NOT NULL DEFAULT 0,
    limit_credits REAL NOT NULL DEFAULT 0,
    limit_rpm INTEGER NOT NULL DEFAULT 0,
    used_tokens INTEGER NOT NULL DEFAULT 0,
    used_credits REAL NOT NULL DEFAULT 0,
    ip_allowlist TEXT NOT NULL DEFAULT '[]',
    created_at INTEGER NOT NULL,
    last_used_at INTEGER
);
CREATE INDEX IF NOT EXISTS idx_keys_prefix ON api_keys(prefix);
CREATE UNIQUE INDEX IF NOT EXISTS idx_bootstrap_key_name
    ON api_keys(name) WHERE name = 'bootstrap';
CREATE TABLE IF NOT EXISTS api_key_rate_events (
    key_id INTEGER NOT NULL,
    ts INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_key_rate_events ON api_key_rate_events(key_id, ts);
CREATE TABLE IF NOT EXISTS request_logs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    ts INTEGER NOT NULL,
    request_id TEXT NOT NULL,
    channel TEXT,
    key_id INTEGER,
    account_id TEXT,
    model TEXT,
    upstream_model TEXT,
    route_alias TEXT,
    fallback_depth INTEGER NOT NULL DEFAULT 0,
    status INTEGER NOT NULL DEFAULT 0,
    error_kind TEXT,
    error TEXT,
    stream INTEGER NOT NULL DEFAULT 0,
    prompt_tokens INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    usage_reported INTEGER NOT NULL DEFAULT 0,
    ttft_ms INTEGER,
    latency_ms INTEGER NOT NULL DEFAULT 0,
    credits REAL,
    ip TEXT,
    ua TEXT
);
CREATE INDEX IF NOT EXISTS idx_logs_ts ON request_logs(ts);
CREATE INDEX IF NOT EXISTS idx_logs_channel ON request_logs(channel, ts);
CREATE INDEX IF NOT EXISTS idx_logs_key ON request_logs(key_id, ts);
CREATE TABLE IF NOT EXISTS usage_daily (
    day TEXT NOT NULL,
    channel TEXT NOT NULL,
    key_id INTEGER NOT NULL,
    model TEXT NOT NULL,
    requests INTEGER NOT NULL DEFAULT 0,
    prompt_tokens INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    usage_reported_requests INTEGER NOT NULL DEFAULT 0,
    credits REAL NOT NULL DEFAULT 0,
    PRIMARY KEY(day, channel, key_id, model)
);
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT);
CREATE TABLE IF NOT EXISTS audit_logs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    ts INTEGER NOT NULL,
    actor TEXT NOT NULL DEFAULT '',
    action TEXT NOT NULL DEFAULT '',
    target TEXT NOT NULL DEFAULT '',
    detail TEXT NOT NULL DEFAULT '',
    ip TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_audit_ts ON audit_logs(ts);
"""


def resolve_db_path(db_path: str) -> Path:
    path = Path(db_path).expanduser()
    return path if path.is_absolute() else API_DIR / path


def connect(db_path: str) -> sqlite3.Connection:
    path = resolve_db_path(db_path)
    path.parent.mkdir(parents=True, exist_ok=True)
    conn = sqlite3.connect(path, timeout=10)
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA foreign_keys=ON")
    conn.execute("PRAGMA busy_timeout=10000")
    return conn


def migrate(db_path: str) -> None:
    with connect(db_path) as conn:
        conn.execute("PRAGMA journal_mode=WAL")
        conn.execute("PRAGMA synchronous=NORMAL")
        conn.execute(
            "CREATE TABLE IF NOT EXISTS schema_migrations "
            "(version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)"
        )
        conn.executescript(_SCHEMA)
        for table, column, definition in (
            ("request_logs", "usage_reported", "INTEGER NOT NULL DEFAULT 0"),
            ("usage_daily", "usage_reported_requests", "INTEGER NOT NULL DEFAULT 0"),
        ):
            columns = {
                row["name"] for row in conn.execute(f"PRAGMA table_info({table})")
            }
            if column not in columns:
                conn.execute(f"ALTER TABLE {table} ADD COLUMN {column} {definition}")
        conn.execute(
            "INSERT OR IGNORE INTO schema_migrations(version, applied_at) "
            "VALUES (?, unixepoch())",
            (SCHEMA_VERSION,),
        )


@contextmanager
def database(db_path: str) -> Iterator[sqlite3.Connection]:
    conn = connect(db_path)
    try:
        yield conn
        conn.commit()
    except Exception:
        conn.rollback()
        raise
    finally:
        conn.close()
