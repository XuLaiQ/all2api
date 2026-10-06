from __future__ import annotations

import asyncio
import codecs
import inspect
import json
import platform
import re
import shutil
import sqlite3
import time
import uuid
from collections.abc import Mapping
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Annotated, Any, Literal

import httpx
from fastapi import APIRouter, Depends, Header, HTTPException, Query, Request
from fastapi.responses import FileResponse, StreamingResponse
from pydantic import BaseModel, ConfigDict, Field, field_validator
from starlette.convertors import StringConvertor, register_url_convertor

from app.adapters.chatgpt.errors import ChatGPTError
from app.adapters.provisioner import ProvisioningUnsupportedError
from app.adapters.registry import AdapterSpec, data_plane_configured, get_registry
from app.adapters.workbuddy.errors import WorkBuddyError
from app.application.accounts.lifecycle import (
    AccountLifecycleError,
    AccountLifecycleService,
)
from app.application.accounts.service import (
    AccountProvisionService,
    ProvisionDispatchError,
    ProvisionSchemaError,
)
from app.application.channels.service import ChannelNotFoundError, ChannelService
from app.compat.legacy_bridge import provisioning
from app.config import get_settings
from app.infrastructure.credentials import AccountNotFoundError
from app.infrastructure.db import SCHEMA_VERSION, database, resolve_db_path
from app.infrastructure.security import require_admin_request, require_same_origin
from app.infrastructure.token_usage import estimate_usage
from app.scheduler.runtime import account_runtime_snapshot, channel_state, runtime_states

router = APIRouter(prefix="/admin/api", dependencies=[Depends(require_admin_request)])
AdminContext = Annotated[dict, Depends(require_admin_request)]


class _AccountIdConvertor(StringConvertor):
    # Canonical IDs always include their channel prefix.  Keeping the removed
    # legacy ``sync`` URL outside this route also preserves a real 404 response.
    regex = r"(?!sync$)[^/]+"


register_url_convertor("account_id", _AccountIdConvertor())


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


class ProvisionStartRequest(BaseModel):
    """Generic M1 provision envelope.

    Platform fields stay inside ``payload`` and are validated against the
    selected channel's manifest before the provisioner is called.
    """

    flow: str = Field(min_length=1, max_length=64)
    payload: dict[str, object] = Field(default_factory=dict)
    idempotency_key: str | None = Field(default=None, min_length=1, max_length=256)


class ProvisionPayloadRequest(BaseModel):
    payload: dict[str, object] = Field(default_factory=dict)
    idempotency_key: str | None = Field(default=None, min_length=1, max_length=256)


class AccountStatePatch(BaseModel):
    enabled: bool


class AccountBatchDeleteRequest(BaseModel):
    """Account ids selected in the console; one batch covers one page at most."""

    ids: list[str] = Field(default_factory=list, max_length=200)


class SettingsPatch(BaseModel):
    """Runtime-safe settings that can be changed from the admin console.

    Provider credentials, URLs, session secrets, and other process wiring stay
    environment-only.  Retention windows are the first persisted settings
    surface because they can be applied immediately without rebuilding the
    adapter registry.
    """

    model_config = ConfigDict(extra="forbid")

    log_retention_days: int | None = Field(default=None, ge=1, le=36500)
    usage_retention_days: int | None = Field(default=None, ge=1, le=36500)


_USERNAME_PATTERN = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._@-]{0,127}$")


class UserCreate(BaseModel):
    model_config = ConfigDict(extra="forbid")

    username: str = Field(min_length=1, max_length=128)
    role: Literal["admin", "viewer"] = "viewer"
    enabled: bool = True

    @field_validator("username")
    @classmethod
    def validate_username(cls, value: str) -> str:
        value = value.strip()
        if not _USERNAME_PATTERN.fullmatch(value):
            raise ValueError("username contains unsupported characters")
        return value


class UserPatch(BaseModel):
    model_config = ConfigDict(extra="forbid")

    role: Literal["admin", "viewer"] | None = None
    enabled: bool | None = None


class ChannelCreate(BaseModel):
    model_config = ConfigDict(extra="forbid")

    slug: str = Field(min_length=1, max_length=64, pattern=r"^[a-z0-9][a-z0-9-]{0,63}$")
    enabled: bool = True
    config: dict[str, object] = Field(default_factory=dict)


class ChannelPatch(BaseModel):
    model_config = ConfigDict(extra="forbid")

    enabled: bool | None = None
    config: dict[str, object] | None = None


class PlaygroundMessage(BaseModel):
    model_config = ConfigDict(extra="forbid")

    role: Literal["system", "user", "assistant"]
    content: str = Field(min_length=1, max_length=16_000)


class PlaygroundChatRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    channel: str = Field(min_length=1, max_length=64)
    model: str = Field(min_length=1, max_length=256)
    conversation_id: str | None = Field(default=None, min_length=1, max_length=128)
    messages: list[PlaygroundMessage] = Field(min_length=1, max_length=32)
    temperature: float | None = Field(default=None, ge=0, le=2)
    max_tokens: int | None = Field(default=None, ge=1, le=8192)
    stream: bool = False


class PlaygroundSearchRequest(BaseModel):
    model: str = Field(default="auto", min_length=1, max_length=256)
    channel: str = Field(default="chatgpt", min_length=1, max_length=64)
    prompt: str = Field(min_length=1, max_length=16_000)


class PlaygroundEditableFileRequest(BaseModel):
    model: str = Field(default="auto", min_length=1, max_length=256)
    channel: str = Field(default="chatgpt", min_length=1, max_length=64)
    kind: Literal["ppt", "psd"]
    prompt: str = Field(default="", max_length=16_000)
    base64_images: list[str] = Field(default_factory=list, max_length=4)


_RETENTION_SETTING_DEFAULTS = {
    "log_retention_days": 30,
    "usage_retention_days": 365,
}


def _retention_settings() -> tuple[dict[str, int], dict[str, str]]:
    """Resolve persisted retention values without exposing arbitrary settings."""

    settings = get_settings()
    values = {
        key: int(getattr(settings, key, fallback))
        for key, fallback in _RETENTION_SETTING_DEFAULTS.items()
    }
    sources = {key: "environment" for key in values}
    with database(settings.db_path) as conn:
        rows = conn.execute(
            "SELECT key, value FROM settings WHERE key IN (?, ?)",
            tuple(values),
        ).fetchall()
    for row in rows:
        key = str(row["key"])
        try:
            candidate = int(str(row["value"]).strip())
        except (TypeError, ValueError):
            continue
        if 1 <= candidate <= 36500:
            values[key] = candidate
            sources[key] = "database"
    # Do not let a manually corrupted row violate the cross-field invariant.
    if values["usage_retention_days"] < values["log_retention_days"]:
        values = {
            key: int(getattr(settings, key, fallback))
            for key, fallback in _RETENTION_SETTING_DEFAULTS.items()
        }
        sources = {key: "environment" for key in values}
    return values, sources


def _settings_payload() -> dict[str, object]:
    values, sources = _retention_settings()
    return {
        "values": values,
        "sources": sources,
        "mutable": list(_RETENTION_SETTING_DEFAULTS),
        "restart_required": False,
    }


def _require_admin_role(user: AdminContext) -> dict:
    if user.get("role") != "admin":
        raise HTTPException(status_code=403, detail="admin role required")
    return user


def _require_admin_write(request: Request, user: AdminContext) -> dict:
    """Require an admin session/token and an explicit same-origin write."""

    require_same_origin(request)
    return _require_admin_role(user)


def _idempotency_key(body_key: str | None, header_key: str | None) -> str:
    key = (body_key or header_key or "").strip()
    if not key:
        raise HTTPException(status_code=422, detail="idempotency_key is required")
    return key


@router.get("/settings", tags=["settings"])
def get_admin_settings() -> dict:
    """Return the safe, persisted settings surface for the console."""

    return {"data": _settings_payload()}


@router.post(
    "/settings",
    tags=["settings"],
    dependencies=[Depends(_require_admin_role)],
)
def update_admin_settings(
    body: SettingsPatch,
    request: Request,
    user: AdminContext,
) -> dict:
    current, _ = _retention_settings()
    requested = body.model_dump(exclude_unset=True)
    updates = {key: value for key, value in requested.items() if value is not None}
    if not updates:
        raise HTTPException(status_code=422, detail="at least one setting is required")
    candidate = {**current, **{key: int(value) for key, value in updates.items()}}
    if candidate["usage_retention_days"] < candidate["log_retention_days"]:
        raise HTTPException(
            status_code=422,
            detail="usage_retention_days must be greater than or equal to log_retention_days",
        )

    settings = get_settings()
    now = int(time.time())
    with database(settings.db_path) as conn:
        for key, value in updates.items():
            conn.execute(
                "INSERT INTO settings(key, value) VALUES (?, ?) "
                "ON CONFLICT(key) DO UPDATE SET value = excluded.value",
                (key, str(int(value))),
            )
        changed = ",".join(sorted(updates))
        conn.execute(
            """INSERT INTO audit_logs(ts, actor, action, target, detail, ip)
            VALUES (?, ?, 'update_settings', 'retention', ?, ?)""",
            (
                now,
                str(user.get("username") or "admin")[:128],
                f"changed={changed}"[:1000],
                str(request.client.host if request.client else "")[:64],
            ),
        )
    return {"data": _settings_payload()}


def _actor(user: Mapping[str, object] | dict) -> str:
    return str(user.get("username") or "admin")[:128]


def _audit_write(
    conn,
    *,
    user: Mapping[str, object] | dict,
    request: Request,
    action: str,
    target: str,
    detail: str = "",
) -> None:
    conn.execute(
        """INSERT INTO audit_logs(ts, actor, action, target, detail, ip)
        VALUES (?, ?, ?, ?, ?, ?)""",
        (
            int(time.time()),
            _actor(user),
            action[:128],
            target[:256],
            detail[:1000],
            str(request.client.host if request.client else "")[:64],
        ),
    )


def _configured_admin_username() -> str:
    return str(getattr(get_settings(), "admin_username", "admin") or "admin").strip()


def _ensure_configured_admin(conn) -> None:
    """Expose the environment-backed administrator without storing a password."""

    username = _configured_admin_username()
    if not _USERNAME_PATTERN.fullmatch(username):
        return
    now = int(time.time())
    conn.execute(
        """INSERT INTO users(username, role, enabled, created_at, updated_at)
        VALUES (?, 'admin', 1, ?, ?)
        ON CONFLICT(username) DO UPDATE SET role='admin', enabled=1, updated_at=?""",
        (username, now, now, now),
    )


def _user_payload(row) -> dict[str, object]:
    return {
        "username": str(row["username"]),
        "role": str(row["role"]),
        "enabled": bool(row["enabled"]),
        "created_at": int(row["created_at"]),
        "updated_at": int(row["updated_at"]),
    }


@router.get("/users", tags=["users"])
def list_users(
    page: int = Query(default=1, ge=1),
    page_size: int = Query(default=50, ge=1, le=200),
    search: str | None = Query(default=None, min_length=1, max_length=128),
    enabled: bool | None = None,
) -> dict:
    filters: list[str] = []
    values: list[object] = []
    if search:
        escaped = search.replace("\\", "\\\\").replace("%", "\\%").replace("_", "\\_")
        filters.append("username LIKE ? ESCAPE '\\'")
        values.append(f"%{escaped}%")
    if enabled is not None:
        filters.append("enabled = ?")
        values.append(int(enabled))
    where = f"WHERE {' AND '.join(filters)}" if filters else ""
    offset = (page - 1) * page_size
    with database(get_settings().db_path) as conn:
        _ensure_configured_admin(conn)
        total = int(conn.execute(f"SELECT COUNT(*) FROM users {where}", values).fetchone()[0])
        rows = conn.execute(
            f"""SELECT username, role, enabled, created_at, updated_at
            FROM users {where} ORDER BY username LIMIT ? OFFSET ?""",
            [*values, page_size, offset],
        ).fetchall()
    return {
        "data": [_user_payload(row) for row in rows],
        "pagination": {
            "page": page,
            "page_size": page_size,
            "total": total,
            "total_pages": (total + page_size - 1) // page_size,
        },
        "authentication": "environment_admin_only",
    }


@router.post(
    "/users",
    status_code=201,
    tags=["users"],
    dependencies=[Depends(_require_admin_role)],
)
def create_user(body: UserCreate, request: Request, user: AdminContext) -> dict:
    now = int(time.time())
    with database(get_settings().db_path) as conn:
        _ensure_configured_admin(conn)
        try:
            conn.execute(
                """INSERT INTO users(username, role, enabled, created_at, updated_at)
                VALUES (?, ?, ?, ?, ?)""",
                (body.username, body.role, int(body.enabled), now, now),
            )
        except sqlite3.IntegrityError as exc:
            raise HTTPException(status_code=409, detail="user already exists") from exc
        _audit_write(
            conn,
            user=user,
            request=request,
            action="create_user",
            target=body.username,
            detail=f"role={body.role};enabled={str(body.enabled).lower()}",
        )
        row = conn.execute(
            "SELECT username, role, enabled, created_at, updated_at FROM users WHERE username = ?",
            (body.username,),
        ).fetchone()
    return {"data": _user_payload(row)}


@router.patch(
    "/users/{username}",
    tags=["users"],
    dependencies=[Depends(_require_admin_role)],
)
def patch_user(
    username: str,
    body: UserPatch,
    request: Request,
    user: AdminContext,
) -> dict:
    if not _USERNAME_PATTERN.fullmatch(username):
        raise HTTPException(status_code=404, detail="user not found")
    if not body.model_fields_set:
        raise HTTPException(status_code=422, detail="at least one user field is required")
    configured_admin = _configured_admin_username().casefold()
    if username.casefold() == configured_admin and (
        body.enabled is False or body.role not in {None, "admin"}
    ):
        raise HTTPException(status_code=409, detail="configured administrator cannot be disabled")
    with database(get_settings().db_path) as conn:
        _ensure_configured_admin(conn)
        row = conn.execute(
            "SELECT username, role, enabled, created_at, updated_at FROM users WHERE username = ?",
            (username,),
        ).fetchone()
        if row is None:
            raise HTTPException(status_code=404, detail="user not found")
        role = body.role if body.role is not None else str(row["role"])
        enabled = body.enabled if body.enabled is not None else bool(row["enabled"])
        now = int(time.time())
        conn.execute(
            "UPDATE users SET role = ?, enabled = ?, updated_at = ? WHERE username = ?",
            (role, int(enabled), now, username),
        )
        _audit_write(
            conn,
            user=user,
            request=request,
            action="update_user",
            target=username,
            detail=f"role={role};enabled={str(enabled).lower()}",
        )
        result = conn.execute(
            "SELECT username, role, enabled, created_at, updated_at FROM users WHERE username = ?",
            (username,),
        ).fetchone()
    return {"data": _user_payload(result)}


@router.delete(
    "/users/{username}",
    tags=["users"],
    dependencies=[Depends(_require_admin_role)],
)
def delete_user(username: str, request: Request, user: AdminContext) -> dict:
    if not _USERNAME_PATTERN.fullmatch(username):
        raise HTTPException(status_code=404, detail="user not found")
    if username.casefold() == _configured_admin_username().casefold():
        raise HTTPException(status_code=409, detail="configured administrator cannot be deleted")
    with database(get_settings().db_path) as conn:
        row = conn.execute("SELECT username FROM users WHERE username = ?", (username,)).fetchone()
        if row is None:
            raise HTTPException(status_code=404, detail="user not found")
        conn.execute("DELETE FROM users WHERE username = ?", (username,))
        _audit_write(
            conn,
            user=user,
            request=request,
            action="delete_user",
            target=username,
        )
    return {"data": {"username": username, "deleted": True}}


def _provision_adapter(channel: str, *, legacy_route: bool = False) -> AdapterSpec:
    adapter = get_registry().get(channel)
    if adapter is None:
        raise HTTPException(status_code=404, detail="channel is not registered")
    if (
        legacy_route
        and hasattr(adapter, "provision_configured")
        and not bool(getattr(get_settings(), "legacy_bridge_enabled", False))
    ):
        return adapter
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


def _require_legacy_bridge() -> None:
    """Guard the migration-only HTTP onboarding endpoints.

    Native provision routes dispatch directly to the channel provisioner.  The
    older routes below are retained solely for compatibility and must never
    contact a source project's management port unless explicitly enabled.
    """

    settings = get_settings()
    # Older test/deployment settings objects may not expose the new switch;
    # preserve their legacy validation behavior while real Settings defaults
    # the bridge to disabled.
    if not hasattr(settings, "legacy_bridge_enabled"):
        return
    if not bool(settings.legacy_bridge_enabled):
        raise HTTPException(
            status_code=410,
            detail="legacy bridge is disabled; use the native channel provision API",
        )


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
            l.prompt_tokens, l.completion_tokens, l.usage_reported, l.usage_kind,
            l.ttft_ms, l.latency_ms
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
    retention, _ = _retention_settings()
    now = int(time.time())
    log_cutoff = now - retention["log_retention_days"] * 86400
    usage_cutoff_date = datetime.fromtimestamp(now, UTC).date() - timedelta(
        days=retention["usage_retention_days"] - 1
    )
    usage_cutoff_day = usage_cutoff_date.isoformat()
    log_cutoff_iso = datetime.fromtimestamp(log_cutoff, UTC).isoformat().replace("+00:00", "Z")
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
            "log_retention_days": retention["log_retention_days"],
            "usage_retention_days": retention["usage_retention_days"],
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
        total = int(conn.execute(f"SELECT COUNT(*) FROM audit_logs {where}", values).fetchone()[0])
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
                    "models_configured": data_plane_configured(adapter, get_settings().db_path),
                    "accounts_configured": adapter.accounts_configured,
                    "provision_configured": bool(getattr(adapter, "provision_configured", False)),
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
    _backfill_playground_usage()
    start, end = _usage_period(days)
    now = int(time.time())
    start_ts = int(datetime.fromisoformat(f"{start}T00:00:00+00:00").timestamp())
    end_ts = int(datetime.fromisoformat(f"{end}T00:00:00+00:00").timestamp()) + 86400
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
                "avg_latency_ms": (int(metric["latency_ms_total"]) / requests if requests else 0),
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
            "avg_latency_ms": (sum(latency_values) / request_count if request_count else 0),
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
    _backfill_playground_usage()
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
        columns = (
            "u.key_id, CASE WHEN u.key_id = 0 THEN '调试台' "
            "ELSE COALESCE(k.name, '') END AS key_name"
        )
        group_by = "u.key_id, k.name"
        key_join = "LEFT JOIN api_keys k ON k.id = u.key_id"
    with database(get_settings().db_path) as conn:
        rows = conn.execute(
            f"""SELECT {columns}, SUM(u.requests) AS requests,
            SUM(u.prompt_tokens) AS prompt_tokens,
            SUM(u.completion_tokens) AS completion_tokens,
            SUM(u.prompt_tokens + u.completion_tokens) AS tokens,
            SUM(u.usage_reported_requests) AS usage_reported_requests,
            SUM(u.usage_estimated_requests) AS usage_estimated_requests,
            SUM(u.requests - u.usage_reported_requests - u.usage_estimated_requests)
                AS usage_unknown_requests
            FROM usage_daily u {key_join}
            WHERE u.day BETWEEN ? AND ?
            GROUP BY {group_by} ORDER BY requests DESC""",
            (start, end),
        ).fetchall()
    data = [{**dict(row), "credits": None, "credits_available": False} for row in rows]
    return {"data": data, "from": start, "to": end}


@router.get("/stats/summary", tags=["stats"])
def get_usage_summary(days: int = Query(default=30, ge=1, le=366)) -> dict:
    _backfill_playground_usage()
    start, end = _usage_period(days)
    with database(get_settings().db_path) as conn:
        row = conn.execute(
            """SELECT COALESCE(SUM(requests), 0) AS requests,
            COALESCE(SUM(prompt_tokens), 0) AS prompt_tokens,
            COALESCE(SUM(completion_tokens), 0) AS completion_tokens,
            COALESCE(SUM(prompt_tokens + completion_tokens), 0) AS tokens,
            COALESCE(SUM(usage_reported_requests), 0) AS usage_reported_requests,
            COALESCE(SUM(usage_estimated_requests), 0) AS usage_estimated_requests,
            COALESCE(SUM(requests - usage_reported_requests - usage_estimated_requests), 0)
                AS usage_unknown_requests
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


def _recent_usage(hours: int = 48, top: int = 12) -> dict:
    """Return the real, timestamped usage series used by the overview chart.

    ``usage_daily`` intentionally has no timestamp, so the recent chart reads
    the request log directly.  Token usage is the chart metric; requests are
    returned alongside it for tooltip/detail consumers and for deterministic
    ranking when a provider did not report token counts.
    """
    _backfill_playground_usage()
    now_ts = int(time.time())
    start_ts = now_ts - hours * 3600
    start_dt = datetime.fromtimestamp(start_ts, UTC)
    end_dt = datetime.fromtimestamp(now_ts, UTC)

    with database(get_settings().db_path) as conn:
        top_rows = conn.execute(
            """SELECT l.key_id,
            COALESCE(k.name,
                CASE
                    WHEN l.key_id = 0 THEN '调试台'
                    WHEN l.key_id IS NULL THEN '匿名请求'
                    ELSE 'Key #' || l.key_id
                END
            ) AS key_name,
            SUM(CASE WHEN l.usage_reported = 1 OR l.usage_kind = 'estimated'
                THEN l.prompt_tokens ELSE 0 END)
                AS total_prompt_tokens,
            SUM(CASE WHEN l.usage_reported = 1 OR l.usage_kind = 'estimated'
                THEN l.completion_tokens ELSE 0 END)
                AS total_completion_tokens,
            SUM(CASE WHEN l.usage_reported = 1 OR l.usage_kind = 'estimated'
                THEN l.prompt_tokens + l.completion_tokens ELSE 0 END) AS total_tokens,
            SUM(l.usage_reported) AS usage_reported_requests,
            SUM(CASE WHEN l.usage_kind = 'estimated' THEN 1 ELSE 0 END)
                AS usage_estimated_requests,
            COUNT(*) - SUM(l.usage_reported)
                - SUM(CASE WHEN l.usage_kind = 'estimated' THEN 1 ELSE 0 END)
                AS usage_unknown_requests,
            COUNT(*) AS total_requests
            FROM request_logs l
            LEFT JOIN api_keys k ON k.id = l.key_id
            WHERE l.ts >= ? AND l.ts < ?
            GROUP BY l.key_id, k.name
            ORDER BY total_tokens DESC, usage_reported_requests DESC, total_requests DESC,
                CASE WHEN l.key_id IS NULL THEN 1 ELSE 0 END, l.key_id
            LIMIT ?""",
            (start_ts, now_ts, top),
        ).fetchall()

        if not top_rows:
            return {
                "metric": "tokens",
                "usage_semantics": "reported_or_estimated_tokens",
                "bucket": "hour",
                "from": start_dt.isoformat().replace("+00:00", "Z"),
                "to": end_dt.isoformat().replace("+00:00", "Z"),
                "series": [],
            }

        non_null_ids = [row["key_id"] for row in top_rows if row["key_id"] is not None]
        key_filters: list[str] = []
        key_values: list[object] = [start_ts, now_ts]
        if non_null_ids:
            placeholders = ", ".join("?" for _ in non_null_ids)
            key_filters.append(f"l.key_id IN ({placeholders})")
            key_values.extend(non_null_ids)
        if any(row["key_id"] is None for row in top_rows):
            key_filters.append("l.key_id IS NULL")
        point_rows = conn.execute(
            f"""SELECT CAST(l.ts / 3600 AS INTEGER) * 3600 AS bucket_ts,
            l.key_id,
            SUM(CASE WHEN l.usage_reported = 1 OR l.usage_kind = 'estimated'
                THEN l.prompt_tokens ELSE 0 END)
                AS prompt_tokens,
            SUM(CASE WHEN l.usage_reported = 1 OR l.usage_kind = 'estimated'
                THEN l.completion_tokens ELSE 0 END)
                AS completion_tokens,
            SUM(CASE WHEN l.usage_reported = 1 OR l.usage_kind = 'estimated'
                THEN l.prompt_tokens + l.completion_tokens ELSE 0 END) AS tokens,
            SUM(l.usage_reported) AS usage_reported_requests,
            SUM(CASE WHEN l.usage_kind = 'estimated' THEN 1 ELSE 0 END)
                AS usage_estimated_requests,
            COUNT(*) - SUM(l.usage_reported)
                - SUM(CASE WHEN l.usage_kind = 'estimated' THEN 1 ELSE 0 END)
                AS usage_unknown_requests,
            COUNT(*) AS requests
            FROM request_logs l
            WHERE l.ts >= ? AND l.ts < ?
              AND ({" OR ".join(key_filters)})
            GROUP BY bucket_ts, l.key_id
            ORDER BY bucket_ts, l.key_id""",
            key_values,
        ).fetchall()

    points_by_key: dict[int | None, dict[int, tuple[int, int, int, int, int, int, int]]] = {}
    for row in point_rows:
        points_by_key.setdefault(row["key_id"], {})[int(row["bucket_ts"])] = (
            int(row["prompt_tokens"] or 0),
            int(row["completion_tokens"] or 0),
            int(row["tokens"] or 0),
            int(row["usage_reported_requests"] or 0),
            int(row["usage_estimated_requests"] or 0),
            int(row["usage_unknown_requests"] or 0),
            int(row["requests"] or 0),
        )
    first_bucket_ts = (start_ts // 3600) * 3600
    last_bucket_ts = (now_ts // 3600) * 3600
    series = []
    zero_point = (0, 0, 0, 0, 0, 0, 0)
    for row in top_rows:
        key_points = points_by_key.get(row["key_id"], {})
        points = []
        for bucket_ts in range(first_bucket_ts, last_bucket_ts + 1, 3600):
            point = key_points.get(bucket_ts, zero_point)
            points.append(
                {
                    "ts": datetime.fromtimestamp(bucket_ts, UTC).isoformat().replace("+00:00", "Z"),
                    "prompt_tokens": point[0],
                    "completion_tokens": point[1],
                    "tokens": point[2],
                    "usage_reported_requests": point[3],
                    "usage_estimated_requests": point[4],
                    "usage_unknown_requests": point[5],
                    "requests": point[6],
                }
            )
        series.append(
            {
                "key_id": row["key_id"],
                "key_name": row["key_name"],
                "prompt_tokens": int(row["total_prompt_tokens"] or 0),
                "completion_tokens": int(row["total_completion_tokens"] or 0),
                "tokens": int(row["total_tokens"] or 0),
                "usage_reported_requests": int(row["usage_reported_requests"] or 0),
                "usage_estimated_requests": int(row["usage_estimated_requests"] or 0),
                "usage_unknown_requests": int(row["usage_unknown_requests"] or 0),
                "requests": int(row["total_requests"] or 0),
                "points": points,
            }
        )

    return {
        "metric": "tokens",
        "usage_semantics": "reported_or_estimated_tokens",
        "bucket": "hour",
        "from": start_dt.isoformat().replace("+00:00", "Z"),
        "to": end_dt.isoformat().replace("+00:00", "Z"),
        "series": series,
    }


@router.get("/overview", tags=["overview"])
def get_overview(days: int = Query(default=30, ge=1, le=366)) -> dict:
    _backfill_playground_usage()
    start, end = _usage_period(days)
    with database(get_settings().db_path) as conn:
        conn.execute("BEGIN")
        summary_row = conn.execute(
            """SELECT COALESCE(SUM(requests), 0) AS requests,
            COALESCE(SUM(prompt_tokens), 0) AS prompt_tokens,
            COALESCE(SUM(completion_tokens), 0) AS completion_tokens,
            COALESCE(SUM(prompt_tokens + completion_tokens), 0) AS tokens,
            COALESCE(SUM(usage_reported_requests), 0) AS usage_reported_requests,
            COALESCE(SUM(usage_estimated_requests), 0) AS usage_estimated_requests,
            COALESCE(SUM(requests - usage_reported_requests - usage_estimated_requests), 0)
                AS usage_unknown_requests
            FROM usage_daily WHERE day BETWEEN ? AND ?""",
            (start, end),
        ).fetchone()
        daily_rows = conn.execute(
            """SELECT day, SUM(requests) AS requests,
            SUM(prompt_tokens) AS prompt_tokens,
            SUM(completion_tokens) AS completion_tokens,
            SUM(prompt_tokens + completion_tokens) AS tokens,
            SUM(usage_reported_requests) AS usage_reported_requests,
            SUM(usage_estimated_requests) AS usage_estimated_requests,
            SUM(requests - usage_reported_requests - usage_estimated_requests)
                AS usage_unknown_requests
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
        "data": [{**dict(row), "credits": None, "credits_available": False} for row in daily_rows],
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
            "recent": _recent_usage(),
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
        if bool(getattr(adapter, "provision_configured", adapter.accounts_configured)):
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
        total = int(conn.execute(f"SELECT COUNT(*) FROM accounts a {where}", values).fetchone()[0])
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
    unconfigured_channels, unconfigured_channel_details = _unconfigured_account_channels()
    return {
        "data": [_account_snapshot(row) for row in rows],
        "pagination": {
            "page": page,
            "page_size": page_size,
            "total": total,
            "total_pages": (total + page_size - 1) // page_size,
        },
        "unconfigured_channels": unconfigured_channels,
        "unconfigured_channel_details": unconfigured_channel_details,
    }


def _account_lifecycle_service() -> AccountLifecycleService:
    settings = get_settings()
    return AccountLifecycleService(
        get_registry(),
        db_path=settings.db_path,
        master_key=getattr(settings, "credential_master_key", ""),
    )


@router.patch(
    "/accounts/{account_id:account_id}",
    tags=["accounts"],
    dependencies=[Depends(_require_admin_role)],
)
async def update_account_state(
    account_id: str,
    body: AccountStatePatch,
    request: Request,
    user: AdminContext,
) -> dict:
    """Enable or disable a local account and its provider profile."""

    try:
        result = await _account_lifecycle_service().set_enabled(
            account_id,
            body.enabled,
            actor=str(user.get("username") or "admin"),
            ip=request.client.host if request.client else "",
        )
    except AccountNotFoundError as exc:
        raise HTTPException(status_code=404, detail="account was not found") from exc
    except AccountLifecycleError as exc:
        raise HTTPException(status_code=502, detail="provider account state update failed") from exc
    return {"data": result}


@router.delete(
    "/accounts/{account_id:account_id}",
    tags=["accounts"],
    dependencies=[Depends(_require_admin_role)],
)
async def delete_account(
    account_id: str,
    request: Request,
    user: AdminContext,
) -> dict:
    """Delete local account data, encrypted credentials and provider profile."""

    try:
        result = await _account_lifecycle_service().delete(
            account_id,
            actor=str(user.get("username") or "admin"),
            ip=request.client.host if request.client else "",
        )
    except AccountNotFoundError as exc:
        raise HTTPException(status_code=404, detail="account was not found") from exc
    except AccountLifecycleError as exc:
        raise HTTPException(status_code=502, detail="provider account cleanup failed") from exc
    return {
        "data": {
            "id": result["id"],
            "channel": result["channel"],
            "deleted": True,
            "credentials_deleted": result["credentials_deleted"],
        }
    }


@router.post(
    "/accounts/batch-delete",
    tags=["accounts"],
    dependencies=[Depends(_require_admin_role)],
)
async def batch_delete_accounts(
    body: AccountBatchDeleteRequest,
    request: Request,
    user: AdminContext,
) -> dict:
    """Delete every selected account best-effort without returning secrets."""

    ids = [str(item).strip() for item in body.ids if str(item).strip()]
    ids = list(dict.fromkeys(ids))
    if not ids:
        raise HTTPException(status_code=400, detail="ids is required")
    result = await _account_lifecycle_service().delete_many(
        ids,
        actor=str(user.get("username") or "admin"),
        ip=request.client.host if request.client else "",
    )
    return {"data": result}


@router.post(
    "/accounts/{account_id:account_id}/refresh",
    tags=["accounts"],
    dependencies=[Depends(_require_admin_role)],
)
async def refresh_account_credential(
    account_id: str,
    request: Request,
    user: AdminContext,
) -> dict:
    """Refresh one provider credential without ever returning token material."""

    try:
        result = await _account_lifecycle_service().refresh(
            account_id,
            actor=str(user.get("username") or "admin"),
            ip=request.client.host if request.client else "",
        )
    except AccountNotFoundError as exc:
        raise HTTPException(status_code=404, detail="account was not found") from exc
    except AccountLifecycleError as exc:
        raise HTTPException(status_code=502, detail="provider credential refresh failed") from exc
    return {"data": result}


@router.post("/accounts/{channel}/onboarding/start", tags=["accounts"])
async def start_account_onboarding(channel: str, body: AccountOnboardingStart) -> dict:
    """Start the native account onboarding flow exposed by an upstream adapter."""
    adapter = _provision_adapter(channel, legacy_route=True)
    _require_legacy_bridge()
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
    adapter = _provision_adapter(channel, legacy_route=True)
    _require_legacy_bridge()
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
    adapter = _provision_adapter(channel, legacy_route=True)
    _require_legacy_bridge()
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


def _channel_management_state(slug: str) -> dict[str, object] | None:
    """Read the local, non-secret management override for a registry channel."""

    try:
        with database(get_settings().db_path) as conn:
            row = conn.execute(
                "SELECT enabled, config, created_at, updated_at FROM channels WHERE slug = ?",
                (slug,),
            ).fetchone()
    except sqlite3.Error:
        return None
    if row is None:
        return None
    try:
        config = json.loads(str(row["config"] or "{}"))
    except (TypeError, ValueError):
        config = {}
    return {
        "enabled": bool(row["enabled"]),
        "config": config if isinstance(config, dict) else {},
        "created_at": int(row["created_at"]),
        "updated_at": int(row["updated_at"]),
    }


def _channel_payload(adapter: AdapterSpec) -> dict:
    managed = _channel_management_state(adapter.slug)
    management_enabled = bool(managed["enabled"]) if managed is not None else True
    data_plane_ready = data_plane_configured(adapter, get_settings().db_path)
    enabled = management_enabled and data_plane_ready
    runtime = channel_state(adapter.slug)
    manifest = getattr(adapter, "manifest", None)
    if manifest is None:
        manifest = ChannelService({adapter.slug: adapter}).manifest(adapter.slug)
    return {
        "slug": adapter.slug,
        "name": adapter.name,
        "adapter": adapter.adapter,
        "display_name": manifest.display_name,
        "adapter_version": manifest.adapter_version,
        "platform_base": getattr(adapter, "platform_base_url", ""),
        "upstream_base": adapter.base_url,
        "legacy_bridge_enabled": bool(getattr(adapter, "legacy_bridge_enabled", False)),
        "enabled": enabled,
        "management_enabled": management_enabled,
        "data_plane_configured": data_plane_ready,
        "management": managed or {"source": "registry"},
        "state": runtime["state"] if enabled else "available",
        "protocols": list(adapter.protocols),
        "caps": list(adapter.caps),
        "capabilities": list(manifest.capabilities),
        "provision_flows": [flow.as_dict() for flow in manifest.account_flows],
        "accounts_configured": adapter.accounts_configured,
        "provision_configured": bool(getattr(adapter, "provision_configured", False)),
        "account_config": _adapter_account_config(adapter),
        "runtime": runtime,
    }


@router.get("/channels", tags=["channels"])
def list_channels() -> dict:
    channels = [_channel_payload(adapter) for adapter in get_registry().values()]
    return {"data": channels, "total": len(channels)}


@router.get("/channels/{slug}/provision-schema", tags=["channels", "accounts"])
def get_provision_schema(slug: str) -> dict:
    """Return the selected channel's declarative account flow schema.

    This endpoint is intentionally independent from the legacy onboarding
    bridge.  It only reads registry metadata, so selecting a channel never
    causes an upstream request or exposes credentials.
    """

    try:
        schema = ChannelService(get_registry()).provision_schema(slug)
    except ChannelNotFoundError as exc:
        raise HTTPException(status_code=404, detail="channel is not registered") from exc
    adapter = get_registry()[slug]
    return {
        "data": {
            **schema,
            "configured": bool(getattr(adapter, "accounts_configured", False)),
            "provision_configured": bool(getattr(adapter, "provision_configured", False)),
            "account_config": _adapter_account_config(adapter),
        }
    }


async def _dispatch_provision(
    channel: str,
    operation: str,
    *,
    flow: str | None = None,
    payload: dict[str, object] | None = None,
    session_id: str = "",
    idempotency_key: str = "",
) -> dict:
    adapter = get_registry().get(channel)
    if adapter is None:
        raise HTTPException(status_code=404, detail="channel is not registered")
    if not bool(getattr(adapter, "accounts_configured", True)) and not bool(
        getattr(adapter, "provision_configured", False)
    ):
        missing = list(getattr(adapter, "account_config_missing", ()))
        detail = "channel account provisioning is not configured"
        if missing:
            detail = f"channel account provisioning is not configured: {', '.join(missing)}"
        raise HTTPException(status_code=409, detail=detail)
    service = AccountProvisionService(get_registry())
    try:
        result = await service.dispatch(
            channel,
            operation,
            flow=flow,
            payload=payload or {},
            session_id=session_id,
            idempotency_key=idempotency_key,
        )
    except ChannelNotFoundError as exc:
        raise HTTPException(status_code=404, detail="channel is not registered") from exc
    except ProvisionSchemaError as exc:
        raise HTTPException(status_code=422, detail=str(exc)) from exc
    except ProvisionDispatchError as exc:
        raise HTTPException(status_code=409, detail=str(exc)) from exc
    except ProvisioningUnsupportedError as exc:
        # Explicitly signal the M1 migration boundary.  The legacy endpoint
        # remains available during migration, while the target API cannot
        # silently fall back to another project's service.
        raise HTTPException(status_code=501, detail=str(exc)) from exc
    except WorkBuddyError as exc:
        # WorkBuddy's native provisioner already classifies safe, actionable
        # protocol errors. Preserve that message instead of collapsing a realm
        # mismatch or invalid completion payload into a generic 502.
        status = 422 if exc.code in {"invalid_request", "realm_mismatch"} else 409
        raise HTTPException(status_code=status, detail=str(exc)) from exc
    except Exception as exc:
        # Native adapters expose a safe status/message pair for expected
        # account-flow failures.  Never forward arbitrary exception text or
        # upstream response bodies to the management client.
        status = getattr(exc, "status_code", 502)
        try:
            status = int(status)
        except (TypeError, ValueError):
            status = 502
        if status < 400 or status > 599:
            status = 502
        detail = getattr(exc, "message", None) or "channel account provisioning failed"
        raise HTTPException(status_code=status, detail=str(detail)) from exc
    return {"data": {"channel": channel, "operation": operation, **(dict(result or {}))}}


@router.post(
    "/channels/{slug}/accounts/provision/start",
    tags=["accounts"],
    dependencies=[Depends(_require_admin_role)],
)
async def provision_start(
    slug: str,
    body: ProvisionStartRequest,
    idempotency_header: str | None = Header(default=None, alias="Idempotency-Key"),
) -> dict:
    return await _dispatch_provision(
        slug,
        "start",
        flow=body.flow,
        payload=body.payload,
        idempotency_key=_idempotency_key(body.idempotency_key, idempotency_header),
    )


@router.get(
    "/channels/{slug}/accounts/provision/{session_id}",
    tags=["accounts"],
)
async def provision_poll(
    slug: str,
    session_id: str,
) -> dict:
    return await _dispatch_provision(
        slug,
        "poll",
        session_id=session_id,
    )


@router.post(
    "/channels/{slug}/accounts/provision/{session_id}/complete",
    tags=["accounts"],
    dependencies=[Depends(_require_admin_role)],
)
async def provision_complete(
    slug: str,
    session_id: str,
    body: ProvisionPayloadRequest,
    idempotency_header: str | None = Header(default=None, alias="Idempotency-Key"),
) -> dict:
    return await _dispatch_provision(
        slug,
        "complete",
        payload=body.payload,
        session_id=session_id,
        idempotency_key=_idempotency_key(body.idempotency_key, idempotency_header),
    )


@router.post(
    "/channels/{slug}/accounts/provision/import",
    tags=["accounts"],
    dependencies=[Depends(_require_admin_role)],
)
async def provision_import(
    slug: str,
    body: ProvisionStartRequest,
    idempotency_header: str | None = Header(default=None, alias="Idempotency-Key"),
) -> dict:
    return await _dispatch_provision(
        slug,
        "import_accounts",
        flow=body.flow,
        payload=body.payload,
        idempotency_key=_idempotency_key(body.idempotency_key, idempotency_header),
    )


@router.post(
    "/channels/{slug}/accounts/provision/{session_id}/cancel",
    tags=["accounts"],
    dependencies=[Depends(_require_admin_role)],
)
async def provision_cancel(
    slug: str,
    session_id: str,
    body: ProvisionPayloadRequest,
    idempotency_header: str | None = Header(default=None, alias="Idempotency-Key"),
) -> dict:
    return await _dispatch_provision(
        slug,
        "cancel",
        session_id=session_id,
        idempotency_key=_idempotency_key(body.idempotency_key, idempotency_header),
    )


@router.get("/channels/adapters", tags=["channels"])
def list_adapters() -> dict:
    adapters = [_channel_payload(adapter) for adapter in get_registry().values()]
    return {"data": adapters, "total": len(adapters)}


@router.post("/channels/{slug}/test", tags=["channels"])
async def test_channel(slug: str) -> dict:
    adapter = get_registry().get(slug)
    if adapter is None:
        raise HTTPException(status_code=404, detail="channel is not registered")
    if not data_plane_configured(adapter, get_settings().db_path):
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


_SENSITIVE_CONFIG_PARTS = (
    "secret",
    "token",
    "password",
    "cookie",
    "credential",
    "authorization",
    "api_key",
    "apikey",
    "access_key",
    "private_key",
)


def _validate_public_channel_config(value: object, *, path: str = "config", depth: int = 0) -> None:
    """Reject provider credentials before they can reach the local config table."""

    if depth > 4:
        raise HTTPException(status_code=422, detail="channel config nesting is too deep")
    if isinstance(value, Mapping):
        if len(value) > 64:
            raise HTTPException(status_code=422, detail="channel config has too many fields")
        for key, item in value.items():
            name = str(key).strip()
            if not name or len(name) > 64:
                raise HTTPException(status_code=422, detail="channel config key is invalid")
            normalized = name.casefold().replace("-", "_")
            if any(part in normalized for part in _SENSITIVE_CONFIG_PARTS):
                raise HTTPException(
                    status_code=422,
                    detail=f"provider secret field is not accepted: {path}.{name}",
                )
            _validate_public_channel_config(item, path=f"{path}.{name}", depth=depth + 1)
        return
    if isinstance(value, (list, tuple)):
        if len(value) > 64:
            raise HTTPException(status_code=422, detail="channel config list is too long")
        for item in value:
            _validate_public_channel_config(item, path=path, depth=depth + 1)
        return
    if value is None or isinstance(value, (bool, int, float)):
        return
    if isinstance(value, str):
        if len(value) > 4096:
            raise HTTPException(status_code=422, detail="channel config value is too long")
        return
    raise HTTPException(status_code=422, detail=f"unsupported channel config value at {path}")


def _channel_config_row(adapter: AdapterSpec, config: dict[str, object], enabled: bool) -> dict:
    now = int(time.time())
    return {
        "slug": adapter.slug,
        "name": adapter.name,
        "adapter": adapter.adapter,
        "upstream_base": str(
            getattr(adapter, "platform_base_url", "") or getattr(adapter, "base_url", "")
        )[:2048],
        "auth_kind": "adapter-managed",
        "enabled": int(enabled),
        "config": json.dumps(config, ensure_ascii=False, separators=(",", ":")),
        "created_at": now,
        "updated_at": now,
    }


def _managed_channel_payload(slug: str) -> dict:
    adapter = get_registry().get(slug)
    if adapter is None:
        raise HTTPException(status_code=404, detail="channel is not registered")
    return _channel_payload(adapter)


@router.post(
    "/channels",
    status_code=201,
    tags=["channels"],
    dependencies=[Depends(_require_admin_write)],
)
def create_channel_override(
    body: ChannelCreate,
    request: Request,
    user: AdminContext,
) -> dict:
    adapter = get_registry().get(body.slug)
    if adapter is None:
        raise HTTPException(
            status_code=501,
            detail="dynamic provider registration is not implemented; use a built-in channel",
        )
    _validate_public_channel_config(body.config)
    row = _channel_config_row(adapter, body.config, body.enabled)
    with database(get_settings().db_path) as conn:
        if conn.execute("SELECT 1 FROM channels WHERE slug = ?", (body.slug,)).fetchone():
            raise HTTPException(status_code=409, detail="channel configuration already exists")
        conn.execute(
            """INSERT INTO channels
            (slug, name, adapter, upstream_base, auth_kind, enabled, config, created_at, updated_at)
            VALUES (:slug, :name, :adapter, :upstream_base, :auth_kind, :enabled, :config,
                    :created_at, :updated_at)""",
            row,
        )
        _audit_write(
            conn,
            user=user,
            request=request,
            action="create_channel_config",
            target=body.slug,
            detail=f"enabled={str(body.enabled).lower()};config_fields={len(body.config)}",
        )
    return {"data": _managed_channel_payload(body.slug)}


@router.patch(
    "/channels/{slug}",
    tags=["channels"],
    dependencies=[Depends(_require_admin_write)],
)
def patch_channel_override(
    slug: str,
    body: ChannelPatch,
    request: Request,
    user: AdminContext,
) -> dict:
    adapter = get_registry().get(slug)
    if adapter is None:
        raise HTTPException(status_code=404, detail="channel is not registered")
    if not body.model_fields_set:
        raise HTTPException(status_code=422, detail="at least one channel field is required")
    if body.config is not None:
        _validate_public_channel_config(body.config)
    current = _channel_management_state(slug)
    current_config = dict(current["config"]) if current else {}
    enabled = (
        bool(current["enabled"])
        if current
        else data_plane_configured(adapter, get_settings().db_path)
    )
    if body.config is not None:
        current_config = body.config
    if body.enabled is not None:
        enabled = body.enabled
    row = _channel_config_row(adapter, current_config, enabled)
    with database(get_settings().db_path) as conn:
        existing = conn.execute(
            "SELECT created_at FROM channels WHERE slug = ?", (slug,)
        ).fetchone()
        if existing is not None:
            row["created_at"] = int(existing["created_at"])
        conn.execute(
            """INSERT INTO channels
            (slug, name, adapter, upstream_base, auth_kind, enabled, config, created_at, updated_at)
            VALUES (:slug, :name, :adapter, :upstream_base, :auth_kind, :enabled, :config,
                    :created_at, :updated_at)
            ON CONFLICT(slug) DO UPDATE SET
                name=excluded.name, adapter=excluded.adapter, upstream_base=excluded.upstream_base,
                auth_kind=excluded.auth_kind, enabled=excluded.enabled, config=excluded.config,
                updated_at=excluded.updated_at""",
            row,
        )
        _audit_write(
            conn,
            user=user,
            request=request,
            action="update_channel_config",
            target=slug,
            detail=f"enabled={str(enabled).lower()};config_fields={len(current_config)}",
        )
    return {"data": _managed_channel_payload(slug)}


@router.delete(
    "/channels/{slug}",
    tags=["channels"],
    dependencies=[Depends(_require_admin_write)],
)
def delete_channel_override(slug: str, request: Request, user: AdminContext) -> dict:
    if slug not in get_registry():
        raise HTTPException(status_code=404, detail="channel is not registered")
    with database(get_settings().db_path) as conn:
        if conn.execute("SELECT 1 FROM channels WHERE slug = ?", (slug,)).fetchone() is None:
            raise HTTPException(status_code=404, detail="channel configuration not found")
        conn.execute("DELETE FROM channels WHERE slug = ?", (slug,))
        _audit_write(
            conn,
            user=user,
            request=request,
            action="delete_channel_config",
            target=slug,
            detail="reset_to_registry_defaults",
        )
    return {"data": {"slug": slug, "deleted": True, "reset_to_registry_defaults": True}}


def _playground_account(channel: str) -> dict[str, str] | None:
    with database(get_settings().db_path) as conn:
        row = conn.execute(
            """SELECT id, native_id FROM accounts
            WHERE channel = ? AND enabled = 1
                AND COALESCE(status_override, status) IN (
                    'ready', 'busy', 'cooldown', 'limited'
                )
                AND EXISTS (
                    SELECT 1 FROM credentials c
                    WHERE c.channel = accounts.channel AND c.account_id = accounts.native_id
                )
            ORDER BY priority DESC, updated_at DESC, id LIMIT 1""",
            (channel,),
        ).fetchone()
    if row is None:
        return None
    return {"id": str(row["id"]), "account_id": str(row["id"]), "native_id": str(row["native_id"])}


def _playground_file_root() -> Path:
    root = Path(get_settings().db_path).expanduser().resolve().parent / "playground-files"
    root.mkdir(parents=True, exist_ok=True)
    return root


def _playground_safe_filename(value: object, fallback: str) -> str:
    name = Path(str(value or "")).name.replace("\x00", "").strip()
    name = re.sub(r"[^A-Za-z0-9._-]+", "-", name).strip(".-")
    return (name or fallback)[:160]


def _playground_run_payload(row) -> dict[str, object]:
    return {
        "id": str(row["id"]),
        "conversation_id": str(row["conversation_id"]) if row["conversation_id"] else None,
        "actor": str(row["actor"]),
        "channel": str(row["channel"]),
        "model": str(row["model"]),
        "status": str(row["status"]),
        "message_count": int(row["message_count"]),
        "request_bytes": int(row["request_bytes"]),
        "response_status": row["response_status"],
        "error_code": row["error_code"],
        "created_at": int(row["created_at"]),
        "completed_at": row["completed_at"],
    }


def _playground_conversation_payload(row) -> dict[str, object]:
    return {
        "id": str(row["id"]),
        "title": str(row["title"]),
        "channel": str(row["channel"]),
        "model": str(row["model"]),
        "message_count": int(row["message_count"]),
        "created_at": int(row["created_at"]),
        "updated_at": int(row["updated_at"]),
    }


def _playground_message_payload(row) -> dict[str, object]:
    raw = None
    if row["raw_response"]:
        try:
            raw = json.loads(str(row["raw_response"]))
        except (TypeError, ValueError):
            raw = str(row["raw_response"])
    payload: dict[str, object] = {
        "id": str(row["id"]),
        "role": str(row["role"]),
        "content": str(row["content"]),
        "created_at": int(row["created_at"]),
    }
    if str(row["model"] or ""):
        payload["model"] = str(row["model"])
    if raw is not None:
        payload["raw"] = raw
    return payload


def _playground_exception_message(exc: Exception) -> str:
    message = str(exc)
    if "Turnstile" in message or "Arkose" in message:
        return "ChatGPT 当前要求浏览器人机验证，请在 ChatGPT Web 完成验证后重新导入会话凭据。"
    return message or "调试请求失败"


def _playground_text(value: object) -> str:
    """Extract assistant text while ignoring provider metadata and tool payloads."""

    if isinstance(value, str):
        trimmed = value.strip()
        if not trimmed:
            return ""
        if any(line.strip().startswith("data:") for line in trimmed.splitlines()):
            chunks: list[str] = []
            for frame in trimmed.replace("\r\n", "\n").split("\n\n"):
                data = _playground_sse_data(frame)
                if not data or data == "[DONE]":
                    continue
                try:
                    parsed: object = json.loads(data)
                except (TypeError, ValueError, json.JSONDecodeError):
                    chunks.append(data)
                else:
                    chunks.append(_playground_text(parsed))
            return "".join(chunks)
        if trimmed.startswith(("{", "[")):
            try:
                return _playground_text(json.loads(trimmed))
            except (TypeError, ValueError, json.JSONDecodeError):
                pass
        return value
    if isinstance(value, list):
        return "".join(_playground_text(item) for item in value)
    if not isinstance(value, Mapping):
        return ""

    choices = value.get("choices")
    if isinstance(choices, list):
        text = "".join(_playground_text(item) for item in choices)
        if text:
            return text
    for key in ("message", "delta"):
        if key in value:
            text = _playground_text(value[key])
            if text:
                return text
    for key in ("output_text", "text", "content"):
        if key in value:
            text = _playground_text(value[key])
            if text:
                return text
    for key in ("output", "data"):
        nested = value.get(key)
        if isinstance(nested, str):
            return nested
        if isinstance(nested, (Mapping, list)):
            text = _playground_text(nested)
            if text:
                return text
    return ""


def _playground_error(value: object) -> tuple[str, str | None]:
    if not isinstance(value, Mapping):
        return "", None
    error = value.get("error")
    if isinstance(error, Mapping):
        message = str(error.get("message") or "").strip()
        code = str(error.get("code") or "upstream_error").strip() or "upstream_error"
        if message:
            return message[:16_000], code[:128]
    elif isinstance(error, str) and error.strip():
        return error.strip()[:16_000], "upstream_error"
    code = value.get("code")
    message = value.get("message") or value.get("msg")
    display = value.get("displayMsg")
    if isinstance(display, Mapping):
        message = display.get("zh") or display.get("en") or message
    if code not in (None, "", 0, "0") and str(message or "").strip():
        return str(message).strip()[:16_000], str(code)[:128]
    nested = value.get("data")
    if isinstance(nested, Mapping):
        return _playground_error(nested)
    return "", None


def _playground_sse_data(frame: str) -> str:
    return "\n".join(
        line[5:].lstrip(" ")
        for line in frame.replace("\r\n", "\n").split("\n")
        if line.startswith("data:")
    ).strip()


def _playground_event(payload: Mapping[str, object]) -> bytes:
    serialized = json.dumps(dict(payload), ensure_ascii=False, separators=(",", ":"))
    return f"data: {serialized}\n\n".encode()


def _record_playground_usage(
    conn,
    *,
    run_id: str,
    channel: str,
    model: str,
    status: int,
    started: int,
    messages: list[Mapping[str, Any]],
    completion: str,
    stream: bool,
    error: str | None = None,
    recorded_at: int | None = None,
) -> None:
    """Record debug-console traffic in the same live usage tables as gateway traffic."""

    usage_kind = "unknown"
    prompt_tokens = completion_tokens = 0
    if status < 400:
        prompt_tokens, completion_tokens = estimate_usage(messages, completion, model)
        usage_kind = "estimated"
    now = int(recorded_at or time.time())
    day = datetime.fromtimestamp(now, UTC).date().isoformat()
    conn.execute(
        """INSERT INTO request_logs
        (ts, request_id, channel, key_id, model, upstream_model, status, error,
         stream, prompt_tokens, completion_tokens, usage_reported, usage_kind, latency_ms)
        VALUES (?, ?, ?, 0, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?)""",
        (
            now,
            run_id,
            channel,
            model,
            model,
            status,
            error,
            int(stream),
            prompt_tokens,
            completion_tokens,
            usage_kind,
            max(0, (now - started) * 1000),
        ),
    )
    conn.execute(
        """INSERT INTO usage_daily
        (day, channel, key_id, model, requests, prompt_tokens, completion_tokens,
         usage_reported_requests, usage_estimated_requests)
        VALUES (?, ?, 0, ?, 1, ?, ?, 0, ?)
        ON CONFLICT(day, channel, key_id, model) DO UPDATE SET
            requests=usage_daily.requests + 1,
            prompt_tokens=usage_daily.prompt_tokens + excluded.prompt_tokens,
            completion_tokens=usage_daily.completion_tokens + excluded.completion_tokens,
            usage_estimated_requests=usage_daily.usage_estimated_requests +
                excluded.usage_estimated_requests""",
        (
            day,
            channel,
            model,
            prompt_tokens,
            completion_tokens,
            int(usage_kind == "estimated"),
        ),
    )


def _backfill_playground_usage() -> None:
    """Bring historical debug runs into the live usage tables once."""

    with database(get_settings().db_path) as conn:
        runs = conn.execute(
            """SELECT p.id, p.conversation_id, p.channel, p.model, p.status,
            p.created_at, p.completed_at
            FROM playground_runs p
            LEFT JOIN request_logs l ON l.request_id = p.id
            WHERE p.completed_at IS NOT NULL AND l.id IS NULL
            ORDER BY p.created_at ASC"""
        ).fetchall()
        for run in runs:
            messages = conn.execute(
                """SELECT role, content FROM playground_messages
                WHERE conversation_id = ? ORDER BY created_at ASC, id ASC""",
                (str(run["conversation_id"] or ""),),
            ).fetchall()
            completion = next(
                (
                    str(item["content"] or "")
                    for item in reversed(messages)
                    if item["role"] == "assistant"
                ),
                "",
            )
            _record_playground_usage(
                conn,
                run_id=str(run["id"]),
                channel=str(run["channel"]),
                model=str(run["model"]),
                status=200 if str(run["status"]) == "ok" else 502,
                started=int(run["created_at"]),
                messages=[
                    {"role": str(item["role"]), "content": str(item["content"] or "")}
                    for item in messages
                ],
                completion=completion,
                stream=False,
                error=None if str(run["status"]) == "ok" else "调试请求失败",
                recorded_at=int(run["created_at"]),
            )
@router.get("/playground/conversations", tags=["playground"])
def list_playground_conversations(
    user: AdminContext,
    page: int = Query(default=1, ge=1),
    page_size: int = Query(default=50, ge=1, le=200),
) -> dict:
    actor = _actor(user)
    offset = (page - 1) * page_size
    with database(get_settings().db_path) as conn:
        total = int(
            conn.execute(
                "SELECT COUNT(*) FROM playground_conversations WHERE actor = ?",
                (actor,),
            ).fetchone()[0]
        )
        rows = conn.execute(
            """SELECT c.id, c.title, c.channel, c.model, c.created_at, c.updated_at,
            (SELECT COUNT(*) FROM playground_messages m
             WHERE m.conversation_id = c.id) AS message_count
            FROM playground_conversations c
            WHERE c.actor = ?
            ORDER BY c.updated_at DESC, c.id DESC LIMIT ? OFFSET ?""",
            (actor, page_size, offset),
        ).fetchall()
    return {
        "data": [_playground_conversation_payload(row) for row in rows],
        "pagination": {
            "page": page,
            "page_size": page_size,
            "total": total,
            "total_pages": (total + page_size - 1) // page_size,
        },
    }


@router.get("/playground/conversations/{conversation_id}", tags=["playground"])
def get_playground_conversation(
    user: AdminContext,
    conversation_id: str,
) -> dict:
    actor = _actor(user)
    with database(get_settings().db_path) as conn:
        conversation = conn.execute(
            """SELECT c.id, c.title, c.channel, c.model, c.created_at, c.updated_at,
            (SELECT COUNT(*) FROM playground_messages m
             WHERE m.conversation_id = c.id) AS message_count
            FROM playground_conversations c WHERE c.id = ? AND c.actor = ?""",
            (conversation_id, actor),
        ).fetchone()
        if conversation is None:
            raise HTTPException(status_code=404, detail="conversation not found")
        messages = conn.execute(
            """SELECT id, role, content,
            COALESCE(
                NULLIF(model, ''),
                (
                    SELECT r.model FROM playground_runs r
                    WHERE r.conversation_id = playground_messages.conversation_id
                    ORDER BY ABS(
                        r.created_at - CAST(playground_messages.created_at / 1000000000 AS INTEGER)
                    ), r.created_at DESC
                    LIMIT 1
                ),
                ''
            ) AS model,
            raw_response, created_at
            FROM playground_messages WHERE conversation_id = ? ORDER BY created_at ASC, id ASC""",
            (conversation_id,),
        ).fetchall()
    return {
        "data": {
            **_playground_conversation_payload(conversation),
            "messages": [_playground_message_payload(row) for row in messages],
        }
    }


@router.delete(
    "/playground/conversations/{conversation_id}",
    tags=["playground"],
    dependencies=[Depends(_require_admin_write)],
)
def delete_playground_conversation(
    conversation_id: str,
    request: Request,
    user: AdminContext,
) -> dict:
    actor = _actor(user)
    with database(get_settings().db_path) as conn:
        conversation = conn.execute(
            "SELECT id FROM playground_conversations WHERE id = ? AND actor = ?",
            (conversation_id, actor),
        ).fetchone()
        if conversation is None:
            raise HTTPException(status_code=404, detail="conversation not found")
        running = conn.execute(
            """SELECT 1 FROM playground_runs
            WHERE conversation_id = ? AND status = 'running' LIMIT 1""",
            (conversation_id,),
        ).fetchone()
        if running is not None:
            raise HTTPException(status_code=409, detail="conversation has a running request")
        deleted_runs = int(
            conn.execute(
                "SELECT COUNT(*) FROM playground_runs WHERE conversation_id = ?",
                (conversation_id,),
            ).fetchone()[0]
        )
        conn.execute("DELETE FROM playground_runs WHERE conversation_id = ?", (conversation_id,))
        conn.execute("DELETE FROM playground_conversations WHERE id = ?", (conversation_id,))
        _audit_write(
            conn,
            user=user,
            request=request,
            action="delete_playground_conversation",
            target=conversation_id,
            detail=f"request_records_deleted={deleted_runs}",
        )
    return {
        "data": {
            "id": conversation_id,
            "deleted": True,
            "request_records_deleted": deleted_runs,
        }
    }


@router.get("/playground/runs", tags=["playground"])
def list_playground_runs(
    page: int = Query(default=1, ge=1),
    page_size: int = Query(default=50, ge=1, le=200),
) -> dict:
    offset = (page - 1) * page_size
    with database(get_settings().db_path) as conn:
        total = int(conn.execute("SELECT COUNT(*) FROM playground_runs").fetchone()[0])
        rows = conn.execute(
            """SELECT id, conversation_id, actor, channel, model, status,
            message_count, request_bytes,
            response_status, error_code, created_at, completed_at
            FROM playground_runs ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?""",
            (page_size, offset),
        ).fetchall()
    return {
        "data": [_playground_run_payload(row) for row in rows],
        "pagination": {
            "page": page,
            "page_size": page_size,
            "total": total,
            "total_pages": (total + page_size - 1) // page_size,
        },
    }


@router.post(
    "/playground/chat",
    tags=["playground"],
    response_model=None,
    dependencies=[Depends(_require_admin_write)],
)
async def playground_chat(
    body: PlaygroundChatRequest,
    request: Request,
    user: AdminContext,
) -> dict | StreamingResponse:
    adapter = get_registry().get(body.channel)
    if adapter is None:
        raise HTTPException(status_code=404, detail="channel is not registered")
    managed = _channel_management_state(body.channel)
    if managed is not None and not bool(managed["enabled"]):
        raise HTTPException(status_code=503, detail="channel_disabled")
    runtime = getattr(adapter, "runtime", None)
    invoke = getattr(runtime, "invoke", None)
    invoke_stream = getattr(runtime, "invoke_stream", None)
    if not callable(invoke):
        raise HTTPException(
            status_code=501,
            detail="playground requires a native channel runtime; no upstream call was made",
        )
    if body.stream and not callable(invoke_stream):
        raise HTTPException(
            status_code=501,
            detail=(
                "playground streaming requires a native streaming runtime; "
                "no upstream call was made"
            ),
        )
    model = body.model
    prefix = f"{body.channel}/"
    if model.startswith(prefix):
        model = model[len(prefix) :]
    if "/" in model:
        raise HTTPException(status_code=422, detail="model must belong to the selected channel")
    actor = _actor(user)
    conversation_id = (body.conversation_id or "").strip()
    now = int(time.time())
    with database(get_settings().db_path) as conn:
        if conversation_id:
            conversation = conn.execute(
                "SELECT id FROM playground_conversations WHERE id = ? AND actor = ?",
                (conversation_id, actor),
            ).fetchone()
            if conversation is None:
                raise HTTPException(status_code=404, detail="conversation not found")
            conn.execute(
                "UPDATE playground_conversations SET channel=?, model=?, updated_at=? WHERE id=?",
                (body.channel, model, now, conversation_id),
            )
        else:
            conversation_id = f"conv_{uuid.uuid4().hex}"
            title = body.messages[0].content.strip()[:80] or "新对话"
            conn.execute(
                """INSERT INTO playground_conversations
                (id, actor, title, channel, model, created_at, updated_at)
                VALUES (?, ?, ?, ?, ?, ?, ?)""",
                (conversation_id, actor, title, body.channel, model, now, now),
            )
        last_message = body.messages[-1]
        if last_message.role == "user":
            conn.execute(
                """INSERT INTO playground_messages
                (id, conversation_id, role, content, model, created_at)
                VALUES (?, ?, 'user', ?, ?, ?)""",
                (
                    f"msg_{uuid.uuid4().hex}",
                    conversation_id,
                    last_message.content,
                    model,
                    time.time_ns(),
                ),
            )
        conn.execute(
            "UPDATE playground_conversations SET updated_at=? WHERE id=?",
            (now, conversation_id),
        )
    payload: dict[str, object] = {
        "model": model,
        "messages": [item.model_dump() for item in body.messages],
        "stream": body.stream,
    }
    if body.temperature is not None:
        payload["temperature"] = body.temperature
    if body.max_tokens is not None:
        payload["max_tokens"] = body.max_tokens
    run_id = f"pg_{uuid.uuid4().hex}"
    request_bytes = len(json.dumps(payload, ensure_ascii=False, separators=(",", ":")).encode())
    account = _playground_account(body.channel)
    with database(get_settings().db_path) as conn:
        conn.execute(
            """INSERT INTO playground_runs
            (id, conversation_id, actor, channel, model, status, message_count,
             request_bytes, created_at)
            VALUES (?, ?, ?, ?, ?, 'running', ?, ?, ?)""",
            (
                run_id,
                conversation_id,
                actor,
                body.channel,
                model,
                len(body.messages),
                request_bytes,
                now,
            ),
        )

    if body.stream:

        async def stream_body():
            status_code = 200
            status = "ok"
            error_code: str | None = None
            error_detail = ""
            assistant_content = ""
            pending = ""
            cancelled = False
            utf8_decoder = codecs.getincrementaldecoder("utf-8")()

            def consume_data(data: str) -> bytes | None:
                nonlocal assistant_content, error_code, error_detail
                if not data or data == "[DONE]":
                    return None
                try:
                    parsed: object = json.loads(data)
                except (TypeError, ValueError, json.JSONDecodeError):
                    text = data if not data.lstrip().startswith("{") else ""
                else:
                    detail, code = _playground_error(parsed)
                    if detail and not error_detail:
                        error_detail = detail
                        error_code = code
                    text = _playground_text(parsed)
                if not text:
                    return None
                remaining = 16_000 - len(assistant_content)
                if remaining <= 0:
                    return None
                text = text[:remaining]
                assistant_content += text
                return _playground_event({"type": "delta", "content": text})

            try:
                stream = invoke_stream(
                    {"model": model, "payload": payload, "stream": True},
                    account,
                )
                if inspect.isawaitable(stream):
                    stream = await stream
                async for chunk in stream:
                    raw_bytes = chunk if isinstance(chunk, bytes) else str(chunk).encode()
                    pending += utf8_decoder.decode(raw_bytes)
                    while True:
                        boundaries = [
                            (pending.find(separator), len(separator))
                            for separator in ("\r\n\r\n", "\n\n")
                            if pending.find(separator) >= 0
                        ]
                        if not boundaries:
                            break
                        index, separator_length = min(boundaries, key=lambda item: item[0])
                        frame, pending = pending[:index], pending[index + separator_length :]
                        data = _playground_sse_data(frame)
                        if not data and frame.strip() and not frame.lstrip().startswith("data:"):
                            data = frame.strip()
                        event = consume_data(data)
                        if event:
                            yield event
                pending += utf8_decoder.decode(b"", final=True)
                if pending.strip():
                    data = _playground_sse_data(pending) or pending.strip()
                    event = consume_data(data)
                    if event:
                        yield event
            except asyncio.CancelledError:
                status = "failed"
                status_code = 499
                error_code = "client_disconnected"
                error_detail = "客户端已断开连接"
                cancelled = True
            except ChatGPTError as exc:
                status = "failed"
                status_code = int(getattr(exc, "status_code", 502))
                error_code = str(getattr(exc, "code", "chatgpt_error"))[:128]
                error_detail = _playground_exception_message(exc)
            except Exception:
                status = "failed"
                status_code = 502
                error_code = "upstream_error"
                error_detail = "调试请求失败"

            if error_detail and not cancelled:
                status = "failed"
                status_code = status_code if status_code >= 400 else 502
                yield _playground_event(
                    {
                        "type": "error",
                        "message": error_detail,
                        "response_status": status_code,
                        "error_code": error_code or "upstream_error",
                    }
                )

            if status == "ok" and not assistant_content and not cancelled:
                status = "failed"
                status_code = 502
                error_code = "empty_response"
                error_detail = "上游返回了空内容"
                yield _playground_event(
                    {
                        "type": "error",
                        "message": error_detail,
                        "response_status": status_code,
                        "error_code": error_code,
                    }
                )

            response_payload: dict[str, object]
            if status == "ok":
                response_payload = {"content": assistant_content}
            else:
                response_payload = {
                    "error": {
                        "message": error_detail or "调试请求失败",
                        "code": error_code or "upstream_error",
                    }
                }
            raw_response = json.dumps(response_payload, ensure_ascii=False)[:128_000]
            finished = int(time.time())
            with database(get_settings().db_path) as conn:
                conn.execute(
                    """UPDATE playground_runs
                    SET status=?, response_status=?, error_code=?, completed_at=?
                    WHERE id=?""",
                    (status, status_code, error_code, finished, run_id),
                )
                if status == "ok":
                    conn.execute(
                        """INSERT INTO playground_messages
                        (id, conversation_id, role, content, model, raw_response, created_at)
                        VALUES (?, ?, 'assistant', ?, ?, ?, ?)""",
                        (
                            f"msg_{uuid.uuid4().hex}",
                            conversation_id,
                            assistant_content,
                            model,
                            raw_response,
                            time.time_ns(),
                        ),
                    )
                else:
                    conn.execute(
                        """INSERT INTO playground_messages
                        (id, conversation_id, role, content, model, created_at)
                        VALUES (?, ?, 'error', ?, ?, ?)""",
                        (
                            f"msg_{uuid.uuid4().hex}",
                            conversation_id,
                            error_detail or "调试请求失败",
                            model,
                            time.time_ns(),
                        ),
                    )
                _record_playground_usage(
                    conn,
                    run_id=run_id,
                    channel=body.channel,
                    model=model,
                    status=status_code,
                    started=now,
                    messages=[item.model_dump() for item in body.messages],
                    completion=assistant_content,
                    stream=True,
                    error=error_detail or None,
                )
                conn.execute(
                    "UPDATE playground_conversations SET updated_at=? WHERE id=?",
                    (finished, conversation_id),
                )
                _audit_write(
                    conn,
                    user=user,
                    request=request,
                    action="playground_chat",
                    target=run_id,
                    detail=f"channel={body.channel};model={model};status={status_code}",
                )
            if not cancelled:
                yield _playground_event(
                    {
                        "type": "done",
                        "run_id": run_id,
                        "conversation_id": conversation_id,
                        "channel": body.channel,
                        "model": model,
                        "status": status,
                        "response_status": status_code,
                        "response": response_payload,
                    }
                )

        return StreamingResponse(
            stream_body(),
            media_type="text/event-stream",
            headers={"Cache-Control": "no-cache", "X-Accel-Buffering": "no"},
        )

    try:
        response = invoke({"model": model, "payload": payload, "stream": False}, account)
        if inspect.isawaitable(response):
            response = await response
        status_code = int(getattr(response, "status_code", 200))
        try:
            result_body = response.json()
        except (ValueError, TypeError):
            result_body = {"content": str(getattr(response, "text", ""))[:64_000]}
        error_code = None
        error_detail, parsed_error_code = _playground_error(result_body)
        if status_code >= 400:
            error_code = (parsed_error_code or "upstream_error")[:128]
        finished = int(time.time())
        status = "ok" if status_code < 400 else "failed"
        raw_response = json.dumps(result_body, ensure_ascii=False)[:128_000]
        assistant_content = _playground_text(result_body)[:16_000]
        if not assistant_content and status_code >= 400:
            assistant_content = (error_detail or error_code or "调试请求失败")[:16_000]
        with database(get_settings().db_path) as conn:
            conn.execute(
                """UPDATE playground_runs
                SET status=?, response_status=?, error_code=?, completed_at=?
                WHERE id=?""",
                (status, status_code, error_code, finished, run_id),
            )
            conn.execute(
                """INSERT INTO playground_messages
                (id, conversation_id, role, content, model, raw_response, created_at)
                VALUES (?, ?, 'assistant', ?, ?, ?, ?)""",
                (
                    f"msg_{uuid.uuid4().hex}",
                    conversation_id,
                    assistant_content,
                    model,
                    raw_response,
                    time.time_ns(),
                ),
            )
            _record_playground_usage(
                conn,
                run_id=run_id,
                channel=body.channel,
                model=model,
                status=status_code,
                started=now,
                messages=[item.model_dump() for item in body.messages],
                completion=assistant_content,
                stream=False,
                error=error_detail or None,
            )
            conn.execute(
                "UPDATE playground_conversations SET updated_at=? WHERE id=?",
                (finished, conversation_id),
            )
            _audit_write(
                conn,
                user=user,
                request=request,
                action="playground_chat",
                target=run_id,
                detail=f"channel={body.channel};model={model};status={status_code}",
            )
        return {
            "data": {
                "run_id": run_id,
                "conversation_id": conversation_id,
                "channel": body.channel,
                "model": model,
                "status": status,
                "response_status": status_code,
                "response": result_body,
            }
        }
    except ChatGPTError as exc:
        failed_at = int(time.time())
        detail = _playground_exception_message(exc)
        with database(get_settings().db_path) as conn:
            conn.execute(
                """UPDATE playground_runs
                SET status='failed', response_status=?, error_code=?, completed_at=?
                WHERE id=?""",
                (
                    int(getattr(exc, "status_code", 502)),
                    str(getattr(exc, "code", "chatgpt_error"))[:128],
                    failed_at,
                    run_id,
                ),
            )
            conn.execute(
                """INSERT INTO playground_messages
                (id, conversation_id, role, content, model, created_at)
                VALUES (?, ?, 'error', ?, ?, ?)""",
                (f"msg_{uuid.uuid4().hex}", conversation_id, detail, model, time.time_ns()),
            )
            _record_playground_usage(
                conn,
                run_id=run_id,
                channel=body.channel,
                model=model,
                status=int(getattr(exc, "status_code", 502)),
                started=now,
                messages=[item.model_dump() for item in body.messages],
                completion="",
                stream=False,
                error=detail,
            )
            conn.execute(
                "UPDATE playground_conversations SET updated_at=? WHERE id=?",
                (failed_at, conversation_id),
            )
            _audit_write(
                conn,
                user=user,
                request=request,
                action="playground_chat",
                target=run_id,
                detail=f"error_code={getattr(exc, 'code', 'chatgpt_error')}",
            )
        headers = {}
        retry_after = getattr(exc, "retry_after", None)
        if retry_after is not None:
            headers["Retry-After"] = str(retry_after)
        raise HTTPException(
            status_code=int(getattr(exc, "status_code", 502)),
            detail=detail,
            headers=headers or None,
        ) from exc
    except HTTPException:
        raise
    except Exception as exc:
        with database(get_settings().db_path) as conn:
            conn.execute(
                """UPDATE playground_runs
                SET status='failed', error_code='upstream_error', completed_at=?
                WHERE id=?""",
                (int(time.time()), run_id),
            )
            failed_at = int(time.time())
            conn.execute(
                """INSERT INTO playground_messages
                (id, conversation_id, role, content, model, created_at)
                VALUES (?, ?, 'error', ?, ?, ?)""",
                (f"msg_{uuid.uuid4().hex}", conversation_id, "调试请求失败", model, time.time_ns()),
            )
            _record_playground_usage(
                conn,
                run_id=run_id,
                channel=body.channel,
                model=model,
                status=502,
                started=now,
                messages=[item.model_dump() for item in body.messages],
                completion="",
                stream=False,
                error="调试请求失败",
            )
            conn.execute(
                "UPDATE playground_conversations SET updated_at=? WHERE id=?",
                (failed_at, conversation_id),
            )
            _audit_write(
                conn,
                user=user,
                request=request,
                action="playground_chat",
                target=run_id,
                detail="status=exception",
            )
        raise HTTPException(status_code=502, detail="playground upstream request failed") from exc


@router.post(
    "/playground/search",
    tags=["playground"],
    response_model=None,
    dependencies=[Depends(_require_admin_write)],
)
async def playground_search(
    body: PlaygroundSearchRequest,
    request: Request,
    user: AdminContext,
) -> dict:
    adapter = get_registry().get(body.channel)
    if adapter is None:
        raise HTTPException(status_code=404, detail="channel is not registered")
    managed = _channel_management_state(body.channel)
    if managed is not None and not bool(managed["enabled"]):
        raise HTTPException(status_code=503, detail="channel_disabled")
    runtime = getattr(adapter, "runtime", None)
    search = getattr(runtime, "search", None)
    if not callable(search):
        raise HTTPException(
            status_code=501, detail="selected channel does not implement native search"
        )
    model = body.model
    prefix = f"{body.channel}/"
    if model.startswith(prefix):
        model = model[len(prefix) :]
    account = _playground_account(body.channel)
    try:
        result = search(body.prompt, model, account)
        if inspect.isawaitable(result):
            result = await result
    except ChatGPTError as exc:
        raise HTTPException(
            status_code=int(getattr(exc, "status_code", 502)),
            detail=_playground_exception_message(exc),
        ) from exc
    if not isinstance(result, Mapping):
        raise HTTPException(status_code=502, detail="search returned an invalid response")
    with database(get_settings().db_path) as conn:
        _audit_write(
            conn,
            user=user,
            request=request,
            action="playground_search",
            target=body.channel,
            detail=f"model={model}",
        )
    return {"data": dict(result)}


@router.post(
    "/playground/editable-file",
    tags=["playground"],
    response_model=None,
    dependencies=[Depends(_require_admin_write)],
)
async def playground_editable_file(
    body: PlaygroundEditableFileRequest,
    request: Request,
    user: AdminContext,
) -> dict:
    adapter = get_registry().get(body.channel)
    if adapter is None:
        raise HTTPException(status_code=404, detail="channel is not registered")
    runtime = getattr(adapter, "runtime", None)
    generate = getattr(runtime, "generate_editable", None)
    if not callable(generate):
        raise HTTPException(
            status_code=501, detail="selected channel does not implement editable file generation"
        )
    model = body.model
    prefix = f"{body.channel}/"
    if model.startswith(prefix):
        model = model[len(prefix) :]
    account = _playground_account(body.channel)
    try:
        result = generate(body.kind, body.prompt, body.base64_images, model, account)
        if inspect.isawaitable(result):
            result = await result
    except ChatGPTError as exc:
        raise HTTPException(
            status_code=int(getattr(exc, "status_code", 502)),
            detail=_playground_exception_message(exc),
        ) from exc
    except Exception as exc:
        raise HTTPException(status_code=502, detail="editable file generation failed") from exc
    if not isinstance(result, Mapping):
        raise HTTPException(
            status_code=502, detail="editable file generation returned an invalid response"
        )
    raw_files = result.get("files")
    if not isinstance(raw_files, list):
        raise HTTPException(status_code=502, detail="editable file generation returned no files")
    task_id = f"file_{uuid.uuid4().hex}"
    task_root = _playground_file_root() / task_id
    task_root.mkdir(parents=True, exist_ok=False)
    urls: dict[str, str] = {}
    for index, item in enumerate(raw_files, start=1):
        if not isinstance(item, Mapping) or not isinstance(item.get("content"), (bytes, bytearray)):
            continue
        filename = _playground_safe_filename(item.get("name"), f"{body.kind}-{index}")
        path = task_root / filename
        path.write_bytes(bytes(item["content"]))
        key = "zip_url" if filename.lower().endswith(".zip") else "primary_url"
        if key == "primary_url" and "primary_url" in urls:
            continue
        urls[key] = f"/admin/api/playground/files/{task_id}/{filename}"
    if "primary_url" not in urls or "zip_url" not in urls:
        raise HTTPException(
            status_code=502, detail="editable file generation did not return both files"
        )
    with database(get_settings().db_path) as conn:
        _audit_write(
            conn,
            user=user,
            request=request,
            action="playground_editable_file",
            target=body.channel,
            detail=f"kind={body.kind};model={model}",
        )
    return {"data": {"task_id": task_id, "kind": body.kind, "status": "success", **urls}}


@router.get("/playground/files/{task_id}/{filename:path}", tags=["playground"])
def download_playground_file(task_id: str, filename: str) -> FileResponse:
    if not re.fullmatch(r"file_[a-f0-9]{32}", task_id):
        raise HTTPException(status_code=404, detail="file not found")
    root = _playground_file_root() / task_id
    path = (root / Path(filename).name).resolve()
    if root.resolve() not in path.parents or not path.is_file():
        raise HTTPException(status_code=404, detail="file not found")
    return FileResponse(path, filename=path.name)
