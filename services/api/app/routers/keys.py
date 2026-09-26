from __future__ import annotations

import json
import re
import time
from typing import Annotated

from fastapi import APIRouter, Depends, HTTPException, Query, Request, Response
from pydantic import BaseModel, ConfigDict, Field, field_validator, model_validator

from app.adapters.registry import get_registry
from app.config import get_settings
from app.db import database
from app.security import hash_api_key, issue_api_key, require_admin_request

router = APIRouter(
    prefix="/admin/api/keys",
    tags=["keys"],
    dependencies=[Depends(require_admin_request)],
)
_NAME_PATTERN = re.compile(r"^[^\x00-\x1f\x7f]{1,128}$")
AdminUser = Annotated[dict, Depends(require_admin_request)]


class ApiKeyCreate(BaseModel):
    model_config = ConfigDict(extra="forbid")

    name: str = Field(min_length=1, max_length=128)
    channels: list[str] = Field(default_factory=list, max_length=16)
    models: list[str] = Field(default_factory=lambda: ["*"], max_length=128)
    expires_at: int | None = Field(default=None, ge=1)
    limit_rpm: int = Field(default=0, ge=0, le=1_000_000)

    @field_validator("name")
    @classmethod
    def validate_name(cls, value: str) -> str:
        value = value.strip()
        if not _NAME_PATTERN.fullmatch(value):
            raise ValueError("name contains unsupported characters")
        return value

    @field_validator("channels")
    @classmethod
    def validate_channels(cls, value: list[str]) -> list[str]:
        if len(value) != len(set(value)):
            raise ValueError("channels must be unique")
        known = set(get_registry())
        if any(channel not in known for channel in value):
            raise ValueError("channels contains an unknown channel")
        return value

    @field_validator("models")
    @classmethod
    def validate_models(cls, value: list[str]) -> list[str]:
        if not value or len(value) != len(set(value)):
            raise ValueError("models must be a non-empty unique list")
        if any(not model.strip() or len(model) > 320 for model in value):
            raise ValueError("models contains an invalid model id")
        if "*" in value and len(value) != 1:
            raise ValueError("wildcard model scope cannot be combined with model ids")
        return value

    @model_validator(mode="after")
    def validate_expiration(self):
        if self.expires_at is not None and self.expires_at <= int(time.time()):
            raise ValueError("expires_at must be in the future")
        return self


class ApiKeyPatch(BaseModel):
    model_config = ConfigDict(extra="forbid")

    name: str | None = Field(default=None, min_length=1, max_length=128)
    enabled: bool | None = None
    channels: list[str] | None = Field(default=None, max_length=16)
    models: list[str] | None = Field(default=None, max_length=128)
    expires_at: int | None = Field(default=None, ge=1)
    limit_rpm: int | None = Field(default=None, ge=0, le=1_000_000)

    @field_validator("name")
    @classmethod
    def validate_name(cls, value: str | None) -> str | None:
        if value is None:
            return None
        value = value.strip()
        if not _NAME_PATTERN.fullmatch(value):
            raise ValueError("name contains unsupported characters")
        return value

    @field_validator("channels")
    @classmethod
    def validate_channels(cls, value: list[str] | None) -> list[str] | None:
        if value is None:
            raise ValueError("channels cannot be null")
        if len(value) != len(set(value)) or any(channel not in get_registry() for channel in value):
            raise ValueError("channels contains an invalid channel")
        return value

    @field_validator("models")
    @classmethod
    def validate_models(cls, value: list[str] | None) -> list[str] | None:
        if value is None:
            raise ValueError("models cannot be null")
        if (
            not value
            or len(value) != len(set(value))
            or any(not model.strip() or len(model) > 320 for model in value)
            or ("*" in value and len(value) != 1)
        ):
            raise ValueError("models contains an invalid model scope")
        return value

    @model_validator(mode="after")
    def validate_expiration(self):
        if self.expires_at is not None and self.expires_at <= int(time.time()):
            raise ValueError("expires_at must be in the future")
        return self

    @model_validator(mode="after")
    def validate_non_nullable_fields(self):
        for field in ("name", "enabled", "channels", "models", "limit_rpm"):
            if field in self.model_fields_set and getattr(self, field) is None:
                raise ValueError(f"{field} cannot be null")
        return self


def _safe_key(row) -> dict:
    return {
        "id": int(row["id"]),
        "name": row["name"],
        "prefix": row["prefix"],
        "enabled": bool(row["enabled"]),
        "expires_at": row["expires_at"],
        "channels": json.loads(row["channels"]),
        "models": json.loads(row["models"]),
        "limit_rpm": int(row["limit_rpm"]),
        "created_at": int(row["created_at"]),
        "last_used_at": row["last_used_at"],
    }


def _require_key_admin(user: AdminUser) -> dict:
    if user.get("role") != "admin":
        raise HTTPException(status_code=403, detail="admin role required")
    return user


def _audit_key(conn, request: Request, action: str, key_id: int, detail: str = "") -> None:
    conn.execute(
        "INSERT INTO audit_logs(ts, actor, action, target, detail, ip) "
        "VALUES (?, 'admin', ?, ?, ?, ?)",
        (
            int(time.time()),
            action,
            str(key_id),
            detail[:128],
            str(request.client.host if request.client else "")[:64],
        ),
    )


@router.get("")
def list_api_keys(
    page: int = Query(default=1, ge=1),
    page_size: int = Query(default=50, ge=1, le=200),
    search: str | None = Query(default=None, min_length=1, max_length=128),
    enabled: bool | None = None,
) -> dict:
    filters = []
    values: list[object] = []
    if search:
        filters.append("(name LIKE ? ESCAPE '\\' OR prefix LIKE ? ESCAPE '\\')")
        escaped = search.replace("\\", "\\\\").replace("%", "\\%").replace("_", "\\_")
        values.extend((f"%{escaped}%", f"%{escaped}%"))
    if enabled is not None:
        filters.append("enabled = ?")
        values.append(int(enabled))
    where = f"WHERE {' AND '.join(filters)}" if filters else ""
    offset = (page - 1) * page_size
    with database(get_settings().db_path) as conn:
        total = int(
            conn.execute(f"SELECT COUNT(*) FROM api_keys {where}", values).fetchone()[0]
        )
        rows = conn.execute(
            f"""SELECT id, name, prefix, enabled, expires_at, channels, models,
            limit_rpm, created_at, last_used_at
            FROM api_keys {where} ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?""",
            [*values, page_size, offset],
        ).fetchall()
    return {
        "data": [_safe_key(row) for row in rows],
        "pagination": {
            "page": page,
            "page_size": page_size,
            "total": total,
            "total_pages": (total + page_size - 1) // page_size,
        },
    }


@router.post("", status_code=201, dependencies=[Depends(_require_key_admin)])
def create_api_key(body: ApiKeyCreate, request: Request, response: Response) -> dict:
    if body.name.casefold() == "bootstrap":
        raise HTTPException(status_code=409, detail="bootstrap key name is reserved")
    raw_key = issue_api_key()
    now = int(time.time())
    with database(get_settings().db_path) as conn:
        conn.execute("BEGIN IMMEDIATE")
        if conn.execute(
            "SELECT 1 FROM api_keys WHERE name = ? COLLATE NOCASE", (body.name,)
        ).fetchone():
            raise HTTPException(status_code=409, detail="key name already exists")
        cursor = conn.execute(
            """INSERT INTO api_keys
            (name, key_hash, prefix, enabled, expires_at, channels, models, limit_rpm,
             created_at)
            VALUES (?, ?, ?, 1, ?, ?, ?, ?, ?)""",
            (
                body.name,
                hash_api_key(raw_key),
                raw_key[:14],
                body.expires_at,
                json.dumps(body.channels),
                json.dumps(body.models),
                body.limit_rpm,
                now,
            ),
        )
        key_id = int(cursor.lastrowid)
        _audit_key(conn, request, "create_api_key", key_id)
        row = conn.execute(
            """SELECT id, name, prefix, enabled, expires_at, channels, models, limit_rpm,
            created_at, last_used_at FROM api_keys WHERE id = ?""",
            (key_id,),
        ).fetchone()
    response.headers["Cache-Control"] = "no-store"
    return {"data": _safe_key(row), "key": raw_key}


@router.patch("/{key_id}", dependencies=[Depends(_require_key_admin)])
def patch_api_key(key_id: int, body: ApiKeyPatch, request: Request) -> dict:
    changes = body.model_dump(exclude_unset=True)
    if not changes:
        raise HTTPException(status_code=400, detail="no key fields were provided")
    with database(get_settings().db_path) as conn:
        conn.execute("BEGIN IMMEDIATE")
        row = conn.execute("SELECT name FROM api_keys WHERE id = ?", (key_id,)).fetchone()
        if row is None:
            raise HTTPException(status_code=404, detail="API key not found")
        if row["name"].casefold() == "bootstrap":
            raise HTTPException(status_code=409, detail="bootstrap key is managed by configuration")
        if "name" in changes:
            if changes["name"].casefold() == "bootstrap":
                raise HTTPException(status_code=409, detail="bootstrap key name is reserved")
            duplicate = conn.execute(
                "SELECT 1 FROM api_keys WHERE name = ? COLLATE NOCASE AND id != ?",
                (changes["name"], key_id),
            ).fetchone()
            if duplicate:
                raise HTTPException(status_code=409, detail="key name already exists")
        columns = {
            "name": "name",
            "enabled": "enabled",
            "channels": "channels",
            "models": "models",
            "expires_at": "expires_at",
            "limit_rpm": "limit_rpm",
        }
        sets = []
        values: list[object] = []
        for field, value in changes.items():
            column = columns[field]
            sets.append(f"{column} = ?")
            values.append(
                json.dumps(value)
                if field in {"channels", "models"}
                else int(value)
                if field == "enabled"
                else value
            )
        values.append(key_id)
        conn.execute(f"UPDATE api_keys SET {', '.join(sets)} WHERE id = ?", values)
        _audit_key(conn, request, "update_api_key", key_id, ",".join(sorted(changes)))
        updated = conn.execute(
            """SELECT id, name, prefix, enabled, expires_at, channels, models, limit_rpm,
            created_at, last_used_at FROM api_keys WHERE id = ?""",
            (key_id,),
        ).fetchone()
    return {"data": _safe_key(updated)}


@router.post("/{key_id}/rotate", dependencies=[Depends(_require_key_admin)])
def rotate_api_key(key_id: int, request: Request, response: Response) -> dict:
    raw_key = issue_api_key()
    with database(get_settings().db_path) as conn:
        conn.execute("BEGIN IMMEDIATE")
        row = conn.execute("SELECT name FROM api_keys WHERE id = ?", (key_id,)).fetchone()
        if row is None:
            raise HTTPException(status_code=404, detail="API key not found")
        if row["name"].casefold() == "bootstrap":
            raise HTTPException(status_code=409, detail="bootstrap key is managed by configuration")
        conn.execute(
            "UPDATE api_keys SET key_hash = ?, prefix = ?, enabled = 1 WHERE id = ?",
            (hash_api_key(raw_key), raw_key[:14], key_id),
        )
        conn.execute("DELETE FROM api_key_rate_events WHERE key_id = ?", (key_id,))
        _audit_key(conn, request, "rotate_api_key", key_id)
        updated = conn.execute(
            """SELECT id, name, prefix, enabled, expires_at, channels, models, limit_rpm,
            created_at, last_used_at FROM api_keys WHERE id = ?""",
            (key_id,),
        ).fetchone()
    response.headers["Cache-Control"] = "no-store"
    return {"data": _safe_key(updated), "key": raw_key}


@router.delete("/{key_id}", status_code=204, dependencies=[Depends(_require_key_admin)])
def revoke_api_key(key_id: int, request: Request) -> Response:
    with database(get_settings().db_path) as conn:
        row = conn.execute("SELECT name FROM api_keys WHERE id = ?", (key_id,)).fetchone()
        if row is None:
            raise HTTPException(status_code=404, detail="API key not found")
        if row["name"].casefold() == "bootstrap":
            raise HTTPException(status_code=409, detail="bootstrap key is managed by configuration")
        conn.execute("UPDATE api_keys SET enabled = 0 WHERE id = ?", (key_id,))
        conn.execute("DELETE FROM api_key_rate_events WHERE key_id = ?", (key_id,))
        _audit_key(conn, request, "revoke_api_key", key_id)
    return Response(status_code=204)
