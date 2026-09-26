from __future__ import annotations

import asyncio
import json
import platform
import re
import shutil
import threading
import time
import uuid
from datetime import UTC, datetime, timedelta
from typing import Annotated

import httpx
from fastapi import APIRouter, Depends, HTTPException, Query, Request
from pydantic import BaseModel, Field

from app.adapters import provisioning
from app.adapters.registry import AdapterSpec, get_registry
from app.config import get_settings
from app.db import SCHEMA_VERSION, database, resolve_db_path
from app.scheduler.runtime import account_runtime_snapshot, channel_state, runtime_states
from app.security import require_admin_request

router = APIRouter(prefix="/admin/api", dependencies=[Depends(require_admin_request)])
AdminContext = Annotated[dict, Depends(require_admin_request)]
_account_sync_lock = threading.Lock()


class AccountOnboardingStart(BaseModel):
    realm: str = "cn"
    account_id: str = ""
    name: str = ""
    priority: int = Field(default=0, ge=-1000, le=1000)
    email_hint: str = ""


class AccountOnboardingFinish(BaseModel):
    session_id: str = ""
    callback: str = ""
    tokens: list[str] = Field(default_factory=list)
    accounts: list[dict[str, object]] = Field(default_factory=list)


def _provision_adapter(channel: str) -> AdapterSpec:
    adapter = get_registry().get(channel)
    if adapter is None:
        raise HTTPException(status_code=404, detail="channel is not registered")
    if not adapter.accounts_configured:
        missing = list(getattr(adapter, "account_config_missing", ()))
        if missing:
            detail = (
                f"{adapter.name} 账号管理接口未配置，请在 services/api/.env 设置："
                f"{', '.join(missing)}"
            )
        else:
            detail = f"{adapter.name} 账号管理接口未配置"
        raise HTTPException(status_code=409, detail=detail)
    return adapter


def _raise_provision_error(cause: Exception) -> None:
    status, message = provisioning.upstream_error(cause)
    raise HTTPException(status_code=status, detail=message) from cause


def _runtime_state_info(path) -> dict[str, object]:
    if not path.exists():
        return {"present": False, "status": "not_initialized"}
    if not path.is_file():
        return {"present": True, "status": "invalid"}
    try:
        with path.open("rb") as stream:
            stream.read(1)
    except OSError as exc:
        return {"present": True, "status": "unreadable", "error": type(exc).__name__}
    return {"present": True, "status": "ok"}


@router.get("/logs", tags=["logs"])
def list_request_logs(
    page: int = Query(default=1, ge=1),
    page_size: int = Query(default=50, ge=1, le=200),
    request_id: str | None = Query(default=None, min_length=1, max_length=128),
    channel: str | None = Query(default=None, min_length=1, max_length=64),
    model: str | None = Query(default=None, min_length=1, max_length=256),
    status: int | None = Query(default=None, ge=100, le=599),
    error_kind: str | None = Query(default=None, min_length=1, max_length=64),
    key_id: int | None = Query(default=None, ge=1),
    stream: bool | None = None,
    from_time: str | None = Query(default=None, alias="from", max_length=40),
    to_time: str | None = Query(default=None, alias="to", max_length=40),
) -> dict:
    def parse_time(value: str | None, label: str) -> int | None:
        if value is None:
            return None
        try:
            parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
        except ValueError:
            raise HTTPException(status_code=400, detail=f"{label} must be RFC 3339") from None
        if parsed.tzinfo is None:
            raise HTTPException(status_code=400, detail=f"{label} must include a timezone")
        return int(parsed.astimezone(UTC).timestamp())

    from_ts = parse_time(from_time, "from")
    to_ts = parse_time(to_time, "to")
    if from_ts is not None and to_ts is not None and from_ts >= to_ts:
        raise HTTPException(status_code=400, detail="from must be earlier than to")
    filters = []
    values: list[object] = []
    for column, value in (
        ("l.request_id", request_id),
        ("l.channel", channel),
        ("l.status", status),
        ("l.error_kind", error_kind),
        ("l.key_id", key_id),
    ):
        if value is not None:
            filters.append(f"{column} = ?")
            values.append(value)
    if model is not None:
        filters.append("l.model LIKE ? ESCAPE '\\'")
        escaped_model = model.replace("\\", "\\\\").replace("%", "\\%").replace("_", "\\_")
        values.append(f"%{escaped_model}%")
    if stream is not None:
        filters.append("l.stream = ?")
        values.append(int(stream))
    if from_ts is not None:
        filters.append("l.ts >= ?")
        values.append(from_ts)
    if to_ts is not None:
        filters.append("l.ts < ?")
        values.append(to_ts)
    where = f"WHERE {' AND '.join(filters)}" if filters else ""
    offset = (page - 1) * page_size
    with database(get_settings().db_path) as conn:
        total = int(
            conn.execute(
                f"SELECT COUNT(*) FROM request_logs l {where}",
                values,
            ).fetchone()[0]
        )
        rows = conn.execute(
            f"""SELECT l.id, l.ts, l.request_id, l.channel, l.key_id,
            l.model, l.upstream_model,
            l.route_alias, l.fallback_depth, l.status, l.error_kind, l.stream,
            l.prompt_tokens, l.completion_tokens, l.usage_reported, l.ttft_ms, l.latency_ms
            FROM request_logs l {where}
            ORDER BY l.ts DESC, l.id DESC LIMIT ? OFFSET ?""",
            [*values, page_size, offset],
        ).fetchall()
    return {
        "data": [
            {
                **dict(row),
                "ts": datetime.fromtimestamp(int(row["ts"]), UTC)
                .isoformat()
                .replace("+00:00", "Z"),
            }
            for row in rows
        ],
        "pagination": {
            "page": page,
            "page_size": page_size,
            "total": total,
            "total_pages": (total + page_size - 1) // page_size,
        },
    }


@router.post("/logs/clear", tags=["logs"])
def clear_request_logs(request: Request, user: AdminContext) -> dict:
    if user.get("role") != "admin":
        raise HTTPException(status_code=403, detail="admin role required")
    settings = get_settings()
    now = int(time.time())
    log_cutoff = now - settings.log_retention_days * 86400
    usage_cutoff_date = (
        datetime.fromtimestamp(now, UTC).date()
        - timedelta(days=settings.usage_retention_days - 1)
    )
    usage_cutoff_day = usage_cutoff_date.isoformat()
    log_cutoff_iso = datetime.fromtimestamp(log_cutoff, UTC).isoformat().replace(
        "+00:00", "Z"
    )
    with database(settings.db_path) as conn:
        conn.execute("BEGIN IMMEDIATE")
        request_logs_deleted = int(
            conn.execute(
                "SELECT COUNT(*) FROM request_logs WHERE ts < ?", (log_cutoff,)
            ).fetchone()[0]
        )
        usage_daily_deleted = int(
            conn.execute(
                "SELECT COUNT(*) FROM usage_daily WHERE day < ?", (usage_cutoff_day,)
            ).fetchone()[0]
        )
        conn.execute("DELETE FROM request_logs WHERE ts < ?", (log_cutoff,))
        conn.execute("DELETE FROM usage_daily WHERE day < ?", (usage_cutoff_day,))
        detail = (
            f"request_logs={request_logs_deleted} "
            f"usage_daily={usage_daily_deleted} "
            f"log_cutoff={log_cutoff_iso} usage_cutoff_day={usage_cutoff_day}"
        )
        conn.execute(
            """INSERT INTO audit_logs(ts, actor, action, target, detail, ip)
            VALUES (?, ?, 'clear_logs', 'retention', ?, ?)""",
            (
                now,
                str(user.get("username") or "admin")[:128],
                detail[:1000],
                str(request.client.host if request.client else "")[:64],
            ),
        )
    return {
        "data": {
            "request_logs_deleted": request_logs_deleted,
            "usage_daily_deleted": usage_daily_deleted,
            "log_cutoff": log_cutoff_iso,
            "usage_cutoff_day": usage_cutoff_day,
            "log_retention_days": settings.log_retention_days,
            "usage_retention_days": settings.usage_retention_days,
        }
    }


@router.get("/audit-logs", tags=["audit"])
def list_audit_logs(
    page: int = Query(default=1, ge=1),
    page_size: int = Query(default=50, ge=1, le=200),
    actor: str | None = Query(default=None, min_length=1, max_length=128),
    action: str | None = Query(default=None, min_length=1, max_length=128),
    target: str | None = Query(default=None, min_length=1, max_length=256),
    from_time: str | None = Query(default=None, alias="from", max_length=40),
    to_time: str | None = Query(default=None, alias="to", max_length=40),
) -> dict:
    def parse_time(value: str | None, label: str) -> int | None:
        if value is None:
            return None
        try:
            parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
        except ValueError:
            raise HTTPException(status_code=400, detail=f"{label} must be RFC 3339") from None
        if parsed.tzinfo is None:
            raise HTTPException(status_code=400, detail=f"{label} must include a timezone")
        return int(parsed.astimezone(UTC).timestamp())

    from_ts = parse_time(from_time, "from")
    to_ts = parse_time(to_time, "to")
    if from_ts is not None and to_ts is not None and from_ts >= to_ts:
        raise HTTPException(status_code=400, detail="from must be earlier than to")
    filters = []
    values: list[object] = []
    for column, value in (
        ("actor", actor),
        ("action", action),
        ("target", target),
    ):
        if value is not None:
            filters.append(f"{column} = ?")
            values.append(value)
    if from_ts is not None:
        filters.append("ts >= ?")
        values.append(from_ts)
    if to_ts is not None:
        filters.append("ts < ?")
        values.append(to_ts)
    where = f"WHERE {' AND '.join(filters)}" if filters else ""
    offset = (page - 1) * page_size
    with database(get_settings().db_path) as conn:
        total = int(
            conn.execute(f"SELECT COUNT(*) FROM audit_logs {where}", values).fetchone()[0]
        )
        rows = conn.execute(
            f"""SELECT id, ts, actor, action, target, detail
            FROM audit_logs {where}
            ORDER BY ts DESC, id DESC LIMIT ? OFFSET ?""",
            [*values, page_size, offset],
        ).fetchall()
    return {
        "data": [
            {
                "id": int(row["id"]),
                "ts": datetime.fromtimestamp(int(row["ts"]), UTC)
                .isoformat()
                .replace("+00:00", "Z"),
                "actor": row["actor"],
                "action": row["action"],
                "target": row["target"],
                "detail": row["detail"],
            }
            for row in rows
        ],
        "pagination": {
            "page": page,
            "page_size": page_size,
            "total": total,
            "total_pages": (total + page_size - 1) // page_size,
        },
    }


@router.get("/sysinfo", tags=["system"])
def get_sysinfo() -> dict:
    settings = get_settings()
    adapters = get_registry()
    db_path = resolve_db_path(settings.db_path)
    state_path = resolve_db_path(settings.state_path)
    runtime_state = _runtime_state_info(state_path)
    try:
        database_size = db_path.stat().st_size if db_path.exists() else 0
    except OSError:
        database_size = 0
    return {
        "data": {
            "service": "all2api-api",
            "version": "0.1.0",
            "python_version": platform.python_version(),
            "platform": platform.system().lower(),
            "python_implementation": platform.python_implementation(),
            "schema_version": SCHEMA_VERSION,
            "database": {
                "present": db_path.exists(),
                "bytes": database_size,
            },
            "runtime_state": {
                **runtime_state,
            },
            "channels": [
                {
                    "slug": adapter.slug,
                    "models_configured": adapter.models_configured,
                    "accounts_configured": adapter.accounts_configured,
                }
                for adapter in adapters.values()
            ],
        }
    }


@router.get("/storage/health", tags=["system"])
def get_storage_health() -> dict:
    settings = get_settings()
    db_path = resolve_db_path(settings.db_path)
    state_path = resolve_db_path(settings.state_path)
    runtime_state = _runtime_state_info(state_path)
    database_status = "missing"
    database_bytes = 0
    database_error = None
    if db_path.exists():
        try:
            with database(settings.db_path) as conn:
                conn.execute("SELECT 1").fetchone()
            database_status = "ok"
            database_bytes = db_path.stat().st_size
        except Exception as exc:
            database_status = "error"
            database_error = type(exc).__name__

    disk_status = "ok"
    disk: dict[str, int | str] = {}
    try:
        usage = shutil.disk_usage(db_path.parent)
        disk = {
            "total_bytes": usage.total,
            "free_bytes": usage.free,
            "used_bytes": usage.used,
        }
    except OSError as exc:
        disk_status = "error"
        disk["error"] = type(exc).__name__

    overall = (
        "ok"
        if database_status == "ok"
        and disk_status == "ok"
        and runtime_state["status"] not in {"invalid", "unreadable"}
        else "degraded"
    )
    data: dict[str, object] = {
        "status": overall,
        "database": {
            "status": database_status,
            "bytes": database_bytes,
        },
        "runtime_state": {
            **runtime_state,
        },
        "disk": {"status": disk_status, **disk},
    }
    if database_error is not None:
        data["database"] = {
            "status": database_status,
            "bytes": database_bytes,
            "error": database_error,
        }
    return {"data": data}


@router.get("/metrics", tags=["system"])
def get_metrics(days: int = Query(default=1, ge=1, le=366)) -> dict:
    start, end = _usage_period(days)
    now = int(time.time())
    start_ts = int(datetime.fromisoformat(f"{start}T00:00:00+00:00").timestamp())
    end_ts = int(
        datetime.fromisoformat(f"{end}T00:00:00+00:00").timestamp()
    ) + 86400
    with database(get_settings().db_path) as conn:
        rows = conn.execute(
            """SELECT channel, status, latency_ms, stream
            FROM request_logs WHERE ts >= ? AND ts < ?""",
            (start_ts, end_ts),
        ).fetchall()
        account_row = conn.execute(
            """SELECT COUNT(*) AS total,
            SUM(CASE WHEN enabled = 1 THEN 1 ELSE 0 END) AS enabled,
            SUM(CASE WHEN a.enabled = 1
                AND COALESCE(a.status_override, a.status) IN ('ready', 'busy')
                AND (a.expires_at IS NULL OR a.expires_at > ?)
                AND (r.cooldown_until IS NULL OR r.cooldown_until <= ?)
                AND (r.breaker_until IS NULL OR r.breaker_until <= ?)
                THEN 1 ELSE 0 END) AS available
            FROM accounts a
            LEFT JOIN account_runtime_state r ON r.account_id = a.id""",
            (now, now, now),
        ).fetchone()
        observed_accounts = int(
            conn.execute("SELECT COUNT(*) FROM account_runtime_state").fetchone()[0]
        )

    latency_values = [max(0, int(row["latency_ms"] or 0)) for row in rows]
    latency_values.sort()
    request_count = len(rows)
    error_count = sum(1 for row in rows if int(row["status"] or 0) >= 400)
    stream_count = sum(1 for row in rows if bool(row["stream"]))
    channel_metrics: dict[str, dict[str, object]] = {}
    for row in rows:
        channel = str(row["channel"] or "unknown")
        metric = channel_metrics.setdefault(
            channel,
            {"requests": 0, "errors": 0, "latency_ms_total": 0},
        )
        metric["requests"] = int(metric["requests"]) + 1
        metric["errors"] = int(metric["errors"]) + int(int(row["status"] or 0) >= 400)
        metric["latency_ms_total"] = int(metric["latency_ms_total"]) + max(
            0, int(row["latency_ms"] or 0)
        )
    channels = []
    for channel, metric in sorted(channel_metrics.items()):
        requests = int(metric["requests"])
        channels.append(
            {
                "channel": channel,
                "requests": requests,
                "errors": int(metric["errors"]),
                "error_rate": (int(metric["errors"]) / requests) if requests else 0,
                "avg_latency_ms": (
                    int(metric["latency_ms_total"]) / requests if requests else 0
                ),
            }
        )
    p95_index = max(0, min(len(latency_values) - 1, int(len(latency_values) * 0.95) - 1))
    return {
        "data": {
            "from": start,
            "to": end,
            "requests": request_count,
            "errors": error_count,
            "error_rate": (error_count / request_count) if request_count else 0,
            "streams": stream_count,
            "avg_latency_ms": (
                sum(latency_values) / request_count if request_count else 0
            ),
            "p95_latency_ms": latency_values[p95_index] if latency_values else 0,
            "channels": channels,
            "accounts": {
                "total": int(account_row["total"] or 0),
                "enabled": int(account_row["enabled"] or 0),
                "available": int(account_row["available"] or 0),
                "runtime_observed": observed_accounts,
            },
        }
    }


def _usage_period(days: int) -> tuple[str, str]:
    today = datetime.now(UTC).date()
    start = today - timedelta(days=days - 1)
    return start.isoformat(), today.isoformat()


def _usage_grouped(days: int, dimension: str) -> dict:
    start, end = _usage_period(days)
    groupings = {
        "daily": ("day", "day"),
        "channel": ("channel", "channel"),
        "model": ("channel, model", "channel, model"),
        "key": ("key_id", "key_id"),
    }
    columns, group_by = groupings[dimension]
    key_join = ""
    if dimension == "key":
        columns = "u.key_id, COALESCE(k.name, '') AS key_name"
        group_by = "u.key_id, k.name"
        key_join = "LEFT JOIN api_keys k ON k.id = u.key_id"
    with database(get_settings().db_path) as conn:
        rows = conn.execute(
            f"""SELECT {columns}, SUM(u.requests) AS requests,
            SUM(u.prompt_tokens) AS prompt_tokens,
            SUM(u.completion_tokens) AS completion_tokens,
            SUM(u.prompt_tokens + u.completion_tokens) AS tokens,
            SUM(u.usage_reported_requests) AS usage_reported_requests,
            SUM(u.requests - u.usage_reported_requests) AS usage_unknown_requests
            FROM usage_daily u {key_join}
            WHERE u.day BETWEEN ? AND ?
            GROUP BY {group_by} ORDER BY requests DESC""",
            (start, end),
        ).fetchall()
    data = [
        {**dict(row), "credits": None, "credits_available": False}
        for row in rows
    ]
    return {"data": data, "from": start, "to": end}


@router.get("/stats/summary", tags=["stats"])
def get_usage_summary(days: int = Query(default=30, ge=1, le=366)) -> dict:
    start, end = _usage_period(days)
    with database(get_settings().db_path) as conn:
        row = conn.execute(
            """SELECT COALESCE(SUM(requests), 0) AS requests,
            COALESCE(SUM(prompt_tokens), 0) AS prompt_tokens,
            COALESCE(SUM(completion_tokens), 0) AS completion_tokens,
            COALESCE(SUM(prompt_tokens + completion_tokens), 0) AS tokens,
            COALESCE(SUM(usage_reported_requests), 0) AS usage_reported_requests,
            COALESCE(SUM(requests - usage_reported_requests), 0) AS usage_unknown_requests
            FROM usage_daily WHERE day BETWEEN ? AND ?""",
            (start, end),
        ).fetchone()
    return {
        "data": {
            **dict(row),
            "credits": None,
            "credits_available": False,
            "from": start,
            "to": end,
        }
    }


@router.get("/stats/daily", tags=["stats"])
def get_usage_daily(days: int = Query(default=30, ge=1, le=366)) -> dict:
    return _usage_grouped(days, "daily")


@router.get("/stats/by-channel", tags=["stats"])
def get_usage_by_channel(days: int = Query(default=30, ge=1, le=366)) -> dict:
    return _usage_grouped(days, "channel")


@router.get("/stats/by-model", tags=["stats"])
def get_usage_by_model(days: int = Query(default=30, ge=1, le=366)) -> dict:
    return _usage_grouped(days, "model")


@router.get("/stats/by-key", tags=["stats"])
def get_usage_by_key(days: int = Query(default=30, ge=1, le=366)) -> dict:
    return _usage_grouped(days, "key")


@router.get("/overview", tags=["overview"])
def get_overview(days: int = Query(default=30, ge=1, le=366)) -> dict:
    start, end = _usage_period(days)
    with database(get_settings().db_path) as conn:
        conn.execute("BEGIN")
        summary_row = conn.execute(
            """SELECT COALESCE(SUM(requests), 0) AS requests,
            COALESCE(SUM(prompt_tokens), 0) AS prompt_tokens,
            COALESCE(SUM(completion_tokens), 0) AS completion_tokens,
            COALESCE(SUM(prompt_tokens + completion_tokens), 0) AS tokens,
            COALESCE(SUM(usage_reported_requests), 0) AS usage_reported_requests,
            COALESCE(SUM(requests - usage_reported_requests), 0) AS usage_unknown_requests
            FROM usage_daily WHERE day BETWEEN ? AND ?""",
            (start, end),
        ).fetchone()
        daily_rows = conn.execute(
            """SELECT day, SUM(requests) AS requests,
            SUM(prompt_tokens) AS prompt_tokens,
            SUM(completion_tokens) AS completion_tokens,
            SUM(prompt_tokens + completion_tokens) AS tokens,
            SUM(usage_reported_requests) AS usage_reported_requests,
            SUM(requests - usage_reported_requests) AS usage_unknown_requests
            FROM usage_daily WHERE day BETWEEN ? AND ?
            GROUP BY day ORDER BY day""",
            (start, end),
        ).fetchall()
    summary = {
        **dict(summary_row),
        "credits": None,
        "credits_available": False,
        "from": start,
        "to": end,
    }
    daily = {
        "data": [
            {**dict(row), "credits": None, "credits_available": False}
            for row in daily_rows
        ],
        "from": start,
        "to": end,
    }
    channels = [_channel_payload(adapter) for adapter in get_registry().values()]
    todos = []
    for channel in channels:
        if not channel["enabled"]:
            todos.append(
                {
                    "id": f"{channel['slug']}:unconfigured",
                    "code": "channel_unconfigured",
                    "severity": "warning",
                    "channel": channel["slug"],
                    "title": f"配置 {channel['name']}",
                    "description": "该渠道尚未完成数据面配置。",
                    "href": "/channels",
                }
            )
            continue
        runtime = channel["runtime"]
        if runtime["state"] in {"breaker_open", "cooldown"}:
            is_breaker = runtime["state"] == "breaker_open"
            todos.append(
                {
                    "id": f"{channel['slug']}:{runtime['state']}",
                    "code": f"channel_{runtime['state']}",
                    "severity": "critical" if is_breaker else "warning",
                    "channel": channel["slug"],
                    "title": f"{channel['name']}渠道{'熔断中' if is_breaker else '冷却中'}",
                    "description": (
                        f"预计 {runtime['retry_after']} 秒后恢复。"
                        if runtime["retry_after"] is not None
                        else "请检查渠道运行状态。"
                    ),
                    "href": "/channels",
                }
            )
    return {
        "data": {
            "summary": summary,
            "daily": daily,
            "channels": channels,
            "todos": todos,
        }
    }


def _account_snapshot(row) -> dict:
    return {
        "id": row["id"],
        "channel": row["channel"],
        "name": row["name"],
        "kind": row["kind"],
        "tier": row["tier"],
        "status": row["status_override"] or row["status"],
        "enabled": bool(row["enabled"]),
        "quota_used": float(row["quota_used"]),
        "quota_total": float(row["quota_total"]),
        "quota_unit": row["quota_unit"],
        "expires_at": row["expires_at"],
        "cooldown_until": row["cooldown_until"],
        "gateway_runtime": account_runtime_snapshot(row),
        "updated_at": int(row["updated_at"]),
    }


def _adapter_account_config(adapter: AdapterSpec) -> dict[str, object]:
    config = getattr(adapter, "account_config", None)
    if isinstance(config, dict):
        return config
    return {
        "configured": bool(adapter.accounts_configured),
        "required_env": [],
        "missing_env": list(getattr(adapter, "account_config_missing", ())),
    }


def _unconfigured_account_channels() -> tuple[list[str], dict[str, dict[str, object]]]:
    channels: list[str] = []
    details: dict[str, dict[str, object]] = {}
    for adapter in get_registry().values():
        if adapter.accounts_configured:
            continue
        channels.append(adapter.slug)
        details[adapter.slug] = _adapter_account_config(adapter)
    return channels, details


@router.get("/accounts", tags=["accounts"])
def list_accounts(
    page: int = Query(default=1, ge=1),
    page_size: int = Query(default=50, ge=1, le=200),
    channel: str | None = Query(default=None, min_length=1, max_length=32),
    status: str | None = Query(default=None, min_length=1, max_length=32),
    search: str | None = Query(default=None, min_length=1, max_length=128),
) -> dict:
    filters = []
    values: list[object] = []
    if channel:
        filters.append("a.channel = ?")
        values.append(channel)
    if status:
        filters.append("COALESCE(a.status_override, a.status) = ?")
        values.append(status)
    if search:
        escaped = search.replace("\\", "\\\\").replace("%", "\\%").replace("_", "\\_")
        filters.append("a.name LIKE ? ESCAPE '\\'")
        values.append(f"%{escaped}%")
    where = f"WHERE {' AND '.join(filters)}" if filters else ""
    offset = (page - 1) * page_size
    with database(get_settings().db_path) as conn:
        total = int(
            conn.execute(f"SELECT COUNT(*) FROM accounts a {where}", values).fetchone()[0]
        )
        rows = conn.execute(
            f"""SELECT a.id, a.channel, a.name, a.kind, a.tier, a.status,
            a.status_override, a.enabled, a.quota_used, a.quota_total, a.quota_unit,
            a.expires_at, a.success_count, a.fail_count, a.streak, a.cooldown_until,
            a.priority, a.updated_at,
            r.success_count AS runtime_success_count,
            r.fail_count AS runtime_fail_count,
            r.consecutive_failures AS runtime_consecutive_failures,
            r.cooldown_until AS runtime_cooldown_until,
            r.breaker_until AS runtime_breaker_until,
            r.last_status AS runtime_last_status,
            r.last_error_kind AS runtime_last_error_kind,
            r.updated_at AS runtime_updated_at
            FROM accounts a LEFT JOIN account_runtime_state r ON r.account_id = a.id
            {where}
            ORDER BY a.channel, a.name, a.id LIMIT ? OFFSET ?""",
            [*values, page_size, offset],
        ).fetchall()
        last_synced = conn.execute(
            "SELECT channel, MAX(updated_at) AS synced_at FROM accounts GROUP BY channel"
        ).fetchall()
    unconfigured_channels, unconfigured_channel_details = _unconfigured_account_channels()
    return {
        "data": [_account_snapshot(row) for row in rows],
        "pagination": {
            "page": page,
            "page_size": page_size,
            "total": total,
            "total_pages": (total + page_size - 1) // page_size,
        },
        "last_synced": {row["channel"]: row["synced_at"] for row in last_synced},
        "unconfigured_channels": unconfigured_channels,
        "unconfigured_channel_details": unconfigured_channel_details,
    }


@router.post("/accounts/sync", tags=["accounts"])
async def sync_accounts(request: Request, user: AdminContext) -> dict:
    if user.get("role") != "admin":
        raise HTTPException(status_code=403, detail="admin role required")
    if not _account_sync_lock.acquire(blocking=False):
        raise HTTPException(status_code=409, detail="account sync is already running")
    adapters = get_registry()
    configured = [adapter for adapter in adapters.values() if adapter.accounts_configured]
    unconfigured = [
        adapter.slug for adapter in adapters.values() if not adapter.accounts_configured
    ]
    unconfigured_details = {
        adapter.slug: _adapter_account_config(adapter)
        for adapter in adapters.values()
        if not adapter.accounts_configured
    }
    now = int(time.time())
    audit_id = None
    try:
        with database(get_settings().db_path) as conn:
            cursor = conn.execute(
                "INSERT INTO audit_logs(ts, actor, action, target, detail, ip) "
                "VALUES (?, ?, 'sync_accounts', 'all', 'started', ?)",
                (
                    now,
                    str(user.get("username") or "admin")[:128],
                    str(request.client.host if request.client else "")[:64],
                ),
            )
            audit_id = cursor.lastrowid
        results = await asyncio.gather(
            *(adapter.list_accounts() for adapter in configured),
            return_exceptions=True,
        )
        accounts: list[dict] = []
        unavailable = [
            adapter.slug
            for adapter, result in zip(configured, results, strict=True)
            if isinstance(result, Exception)
        ]
        for adapter, result in zip(configured, results, strict=True):
            if isinstance(result, Exception):
                continue
            for upstream_account in result:
                native_id = str(upstream_account.get("native_id") or "").strip()
                if not native_id:
                    continue
                account = dict(upstream_account)
                account["channel"] = adapter.slug
                account["native_id"] = native_id
                account["id"] = f"{adapter.slug}:{native_id}"
                accounts.append(account)
        if not accounts and unavailable and not unconfigured:
            with database(get_settings().db_path) as conn:
                conn.execute(
                    "UPDATE audit_logs SET detail = ? WHERE id = ?",
                    (f"failed channels={','.join(unavailable)}", audit_id),
                )
            raise HTTPException(
                status_code=502,
                detail="all configured account services are unavailable",
            )
        with database(get_settings().db_path) as conn:
            conn.execute("BEGIN IMMEDIATE")
            for account in accounts:
                ext = account.get("ext") if isinstance(account.get("ext"), dict) else {}
                quota_used = account.get("quota_used")
                quota_total = account.get("quota_total")
                conn.execute(
                    """INSERT INTO accounts
                    (id, channel, native_id, name, kind, tier, status, enabled, quota_used,
                     quota_total, quota_unit, expires_at, success_count, fail_count, streak,
                     cooldown_until, last_error, priority, ext, created_at, updated_at)
                    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                    ON CONFLICT(id) DO UPDATE SET name=excluded.name, kind=excluded.kind,
                    tier=excluded.tier, status=excluded.status, enabled=excluded.enabled,
                    quota_used=excluded.quota_used, quota_total=excluded.quota_total,
                    quota_unit=excluded.quota_unit, success_count=excluded.success_count,
                    fail_count=excluded.fail_count, streak=excluded.streak,
                    cooldown_until=excluded.cooldown_until, last_error=excluded.last_error,
                    expires_at=excluded.expires_at, priority=excluded.priority,
                    ext=excluded.ext, updated_at=excluded.updated_at""",
                    (
                        account["id"], account["channel"], account["native_id"],
                        str(account.get("name") or account["native_id"]),
                        str(account.get("kind") or "account"), account.get("tier"),
                        str(account.get("status") or "unknown"),
                        int(bool(account.get("enabled", True))),
                        float(quota_used) if quota_used is not None else 0,
                        float(quota_total) if quota_total is not None else 0,
                        str(account.get("quota_unit") or "none"), account.get("expires_at"),
                        int(account.get("success_count") or 0), int(account.get("fail_count") or 0),
                        int(account.get("streak") or 0), account.get("cooldown_until"),
                        str(account.get("last_error") or "")[:1000],
                        int(account.get("priority") or 0), json.dumps(ext, ensure_ascii=False),
                        now, now,
                    ),
                )
            detail = f"synced={len(accounts)} unavailable={','.join(unavailable)}"
            conn.execute("UPDATE audit_logs SET detail = ? WHERE id = ?", (detail[:128], audit_id))
        return {
            "data": {
                "synced": len(accounts),
                "unavailable_channels": unavailable,
                "unconfigured_channels": unconfigured,
                "unconfigured_channel_details": unconfigured_details,
            },
            "unconfigured_channels": unconfigured,
            "unconfigured_channel_details": unconfigured_details,
            "unavailable_channels": unavailable,
            "last_synced_at": now,
        }
    except Exception as exc:
        if audit_id is not None and not isinstance(exc, HTTPException):
            with database(get_settings().db_path) as conn:
                conn.execute(
                    "UPDATE audit_logs SET detail = 'failed: sync exception' WHERE id = ?",
                    (audit_id,),
                )
        raise
    finally:
        _account_sync_lock.release()


@router.post("/accounts/{channel}/onboarding/start", tags=["accounts"])
async def start_account_onboarding(channel: str, body: AccountOnboardingStart) -> dict:
    """Start the native account onboarding flow exposed by an upstream adapter."""
    adapter = _provision_adapter(channel)
    try:
        if channel == "wb":
            result = await provisioning.workbuddy_start(
                adapter.base_url,
                adapter.account_key,
                body.realm,
            )
            return {"data": {"channel": channel, "flow": "qr", **result}}

        if channel == "doubao":
            account_id = body.account_id.strip() or f"doubao_{uuid.uuid4().hex[:8]}"
            if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_-]{0,63}", account_id):
                raise HTTPException(
                    status_code=400,
                    detail="账号 ID 只能包含字母、数字、下划线和短横线",
                )
            created = await provisioning.doubao_create(
                adapter.base_url,
                adapter.account_key,
                account_id,
                body.name.strip() or account_id,
                body.priority,
            )
            qr = await provisioning.doubao_qr_start(
                adapter.base_url,
                adapter.account_key,
                account_id,
            )
            return {"data": {"channel": channel, "flow": "qr", **created, **qr}}

        if channel == "chatgpt":
            oauth = await provisioning.chatgpt_oauth_start(
                adapter.base_url,
                adapter.account_key,
                body.email_hint.strip(),
            )
            return {"data": {"channel": channel, "flow": "oauth", **oauth}}

        raise HTTPException(status_code=400, detail="该渠道暂不支持账号新增")
    except HTTPException:
        raise
    except Exception as cause:
        _raise_provision_error(cause)


@router.get("/accounts/{channel}/onboarding/poll", tags=["accounts"])
async def poll_account_onboarding(
    channel: str,
    state: str | None = Query(default=None, min_length=1, max_length=256),
    realm: str = Query(default="cn"),
    region: str | None = Query(default=None, max_length=16),
    account_id: str | None = Query(default=None, max_length=128),
) -> dict:
    """Poll a WorkBuddy or Doubao QR onboarding flow."""
    adapter = _provision_adapter(channel)
    try:
        if channel == "wb":
            if not state:
                raise HTTPException(status_code=400, detail="state is required")
            result = await provisioning.workbuddy_poll(
                adapter.base_url,
                adapter.account_key,
                state,
                realm,
                region,
            )
            return {"data": {"channel": channel, "flow": "qr", **result}}
        if channel == "doubao":
            if not account_id:
                raise HTTPException(status_code=400, detail="account_id is required")
            result = await provisioning.doubao_qr_poll(
                adapter.base_url,
                adapter.account_key,
                account_id,
            )
            return {"data": {"channel": channel, "flow": "qr", **result}}
        raise HTTPException(status_code=400, detail="该渠道不支持扫码轮询")
    except HTTPException:
        raise
    except Exception as cause:
        _raise_provision_error(cause)


@router.post("/accounts/{channel}/onboarding/finish", tags=["accounts"])
async def finish_account_onboarding(channel: str, body: AccountOnboardingFinish) -> dict:
    """Finish ChatGPT OAuth or token import without returning upstream credentials."""
    adapter = _provision_adapter(channel)
    if channel != "chatgpt":
        raise HTTPException(status_code=400, detail="该渠道不支持此完成方式")
    try:
        if body.callback.strip():
            if not body.session_id.strip():
                raise HTTPException(status_code=400, detail="session_id is required")
            result = await provisioning.chatgpt_oauth_finish(
                adapter.base_url,
                adapter.account_key,
                body.session_id.strip(),
                body.callback.strip(),
            )
        else:
            tokens = [token.strip() for token in body.tokens if token.strip()]
            if not tokens and not body.accounts:
                raise HTTPException(
                    status_code=400,
                    detail="callback、tokens 或 accounts 至少提供一项",
                )
            result = await provisioning.chatgpt_import(
                adapter.base_url,
                adapter.account_key,
                tokens,
                body.accounts,
            )
        return {"data": {"channel": channel, "flow": "oauth", **result}}
    except HTTPException:
        raise
    except Exception as cause:
        _raise_provision_error(cause)


def _channel_payload(adapter: AdapterSpec) -> dict:
    enabled = adapter.models_configured
    runtime = channel_state(adapter.slug)
    return {
        "slug": adapter.slug,
        "name": adapter.name,
        "adapter": adapter.adapter,
        "upstream_base": adapter.base_url,
        "enabled": enabled,
        "state": runtime["state"] if enabled else "available",
        "protocols": list(adapter.protocols),
        "caps": list(adapter.caps),
        "accounts_configured": adapter.accounts_configured,
        "account_config": _adapter_account_config(adapter),
        "runtime": runtime,
    }


@router.get("/channels", tags=["channels"])
def list_channels() -> dict:
    channels = [_channel_payload(adapter) for adapter in get_registry().values()]
    return {"data": channels, "total": len(channels)}


@router.get("/channels/adapters", tags=["channels"])
def list_adapters() -> dict:
    adapters = [_channel_payload(adapter) for adapter in get_registry().values()]
    return {"data": adapters, "total": len(adapters)}


@router.post("/channels/{slug}/test", tags=["channels"])
async def test_channel(slug: str) -> dict:
    adapter = get_registry().get(slug)
    if adapter is None:
        raise HTTPException(status_code=404, detail="channel is not registered")
    if not adapter.models_configured:
        raise HTTPException(status_code=409, detail="channel is not configured")
    started = time.perf_counter()
    try:
        models = await adapter.list_models()
    except httpx.TimeoutException as exc:
        raise HTTPException(status_code=504, detail="channel test timed out") from exc
    except httpx.HTTPError as exc:
        raise HTTPException(status_code=502, detail="channel test failed") from exc
    except Exception as exc:
        raise HTTPException(status_code=502, detail="channel test failed") from exc
    return {
        "data": {
            "channel": slug,
            "status": "ok",
            "latency_ms": max(0, int((time.perf_counter() - started) * 1000)),
            "model_count": len(models),
            "tested_at": datetime.now(UTC).isoformat().replace("+00:00", "Z"),
        }
    }


@router.get("/channels/{slug}/runtime", tags=["channels"])
def get_channel_runtime(slug: str) -> dict:
    if slug not in get_registry():
        raise HTTPException(status_code=404, detail="channel is not registered")
    states = runtime_states(slug)
    return {"data": {"channel": slug, "states": states}, "total": len(states)}
