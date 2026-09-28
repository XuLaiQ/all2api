from __future__ import annotations

import asyncio
import json
import re
import time
from collections.abc import Mapping
from typing import Annotated, Any

from fastapi import APIRouter, Depends, HTTPException, Query, Request
from pydantic import BaseModel, ConfigDict

from app.adapters.registry import AdapterSpec, get_registry
from app.config import get_settings
from app.infrastructure.db import database
from app.infrastructure.security import require_admin_request

router = APIRouter(
    prefix="/admin/api/models",
    tags=["models"],
    dependencies=[Depends(require_admin_request)],
)
AdminUser = Annotated[dict, Depends(require_admin_request)]
_MODEL_ID = re.compile(r"^[A-Za-z0-9._-]+/.+$")
MODEL_DISCOVERY_TIMEOUT_SECONDS = 30


class ModelDiscoveryTimeoutError(RuntimeError):
    """A provider did not return its live model catalogue in time."""


async def fetch_adapter_models(adapter: AdapterSpec) -> list[Mapping[str, Any]]:
    try:
        result = await asyncio.wait_for(
            adapter.list_models(),
            timeout=MODEL_DISCOVERY_TIMEOUT_SECONDS,
        )
    except TimeoutError as exc:
        raise ModelDiscoveryTimeoutError("model discovery timed out") from exc
    return [item for item in result if isinstance(item, Mapping)]


class ModelPatch(BaseModel):
    model_config = ConfigDict(extra="forbid")

    enabled: bool


def _require_model_admin(user: AdminUser) -> dict:
    if user.get("role") != "admin":
        raise HTTPException(status_code=403, detail="admin role required")
    return user


def _channel_enabled(channel: str, db_path: str) -> bool:
    with database(db_path) as conn:
        row = conn.execute("SELECT enabled FROM channels WHERE slug = ?", (channel,)).fetchone()
    return row is None or bool(row["enabled"])


def _has_local_account(channel: str, db_path: str) -> bool:
    with database(db_path) as conn:
        return conn.execute(
            """SELECT 1 FROM accounts
            WHERE channel = ? AND enabled = 1
                AND COALESCE(status_override, status) IN ('ready', 'busy', 'cooldown', 'limited')
            LIMIT 1""",
            (channel,),
        ).fetchone() is not None


def adapter_catalogue_enabled(adapter: AdapterSpec, db_path: str) -> bool:
    """A public key or a stored native account can authenticate model reads."""

    if not _channel_enabled(adapter.slug, db_path):
        return False
    return bool(adapter.models_configured) or bool(
        getattr(adapter, "runtime", None) is not None
        and _has_local_account(adapter.slug, db_path)
    )


def _as_int(value: Any) -> int | None:
    if value is None or isinstance(value, bool):
        return None
    try:
        parsed = int(value)
    except (TypeError, ValueError, OverflowError):
        return None
    return parsed if parsed >= 0 else None


def _model_row(
    channel: str,
    item: Mapping[str, Any],
) -> tuple[str, str, str, str, str, int | None, int | None, float, str]:
    upstream_id = str(item.get("id") or item.get("model_id") or "").strip()
    model_id = f"{channel}/{upstream_id}"
    display_name = str(item.get("name") or item.get("display_name") or upstream_id).strip()
    kind = str(item.get("kind") or "chat").strip() or "chat"
    raw_caps = item.get("caps") or item.get("capabilities")
    caps = (
        [str(value).strip() for value in raw_caps if str(value).strip()]
        if isinstance(raw_caps, list)
        else ["chat"]
    )
    context_window = _as_int(
        item.get("context_window")
        or item.get("context_length")
        or item.get("maxInputTokens")
    )
    max_output = _as_int(
        item.get("max_output")
        or item.get("max_output_tokens")
        or item.get("maxOutputTokens")
    )
    multiplier = item.get("multiplier", 1)
    try:
        multiplier = float(multiplier)
    except (TypeError, ValueError):
        multiplier = 1.0
    return (
        model_id,
        channel,
        upstream_id,
        display_name,
        kind,
        context_window,
        max_output,
        multiplier,
        json.dumps(caps, ensure_ascii=False),
    )


def upsert_model_cache(
    channel: str,
    items: list[Mapping[str, Any]],
    *,
    db_path: str | None = None,
) -> int:
    path = db_path or get_settings().db_path
    rows = [
        _model_row(channel, item)
        for item in items
        if isinstance(item, Mapping) and item.get("id")
    ]
    with database(path) as conn:
        if rows:
            conn.executemany(
                """INSERT INTO models
                (
                    id, channel, upstream_id, display_name, kind, context_window,
                    max_output, multiplier, caps, enabled
                )
                VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1)
                ON CONFLICT(id) DO UPDATE SET
                    display_name=excluded.display_name,
                    kind=excluded.kind,
                    context_window=excluded.context_window,
                    max_output=excluded.max_output,
                    multiplier=excluded.multiplier,
                    caps=excluded.caps""",
                rows,
            )
        # A successful live refresh is authoritative for that channel. Keep
        # the operator's enabled flag for models still present, but remove
        # stale entries that the provider no longer advertises.
        current_ids = [row[0] for row in rows]
        if current_ids:
            placeholders = ",".join("?" for _ in current_ids)
            conn.execute(
                f"DELETE FROM models WHERE channel = ? AND id NOT IN ({placeholders})",
                [channel, *current_ids],
            )
        else:
            conn.execute("DELETE FROM models WHERE channel = ?", (channel,))
    return len(rows)


@router.post("/refresh", dependencies=[Depends(_require_model_admin)])
async def refresh_models(user: AdminUser) -> dict:
    del user
    db_path = get_settings().db_path
    adapters = [
        adapter
        for adapter in get_registry().values()
        if adapter_catalogue_enabled(adapter, db_path)
    ]
    results = await asyncio.gather(
        *(fetch_adapter_models(adapter) for adapter in adapters),
        return_exceptions=True,
    )
    channels: list[dict[str, object]] = []
    total = 0
    for adapter, result in zip(adapters, results, strict=True):
        if isinstance(result, Exception):
            channels.append(
                {
                    "channel": adapter.slug,
                    "status": "failed",
                    "error": (
                        "timeout"
                        if isinstance(result, ModelDiscoveryTimeoutError)
                        else "unavailable"
                    ),
                }
            )
            continue
        items = [dict(item) for item in result if isinstance(item, Mapping) and item.get("id")]
        count = upsert_model_cache(adapter.slug, items, db_path=db_path)
        total += count
        channels.append({"channel": adapter.slug, "status": "ok", "count": count})
    return {
        "data": {
            "source": "live",
            "refreshed_at": int(time.time()),
            "total": total,
            "channels": channels,
        }
    }


@router.get("")
def list_models(
    page: int = Query(default=1, ge=1),
    page_size: int = Query(default=50, ge=1, le=200),
    channel: str | None = Query(default=None, min_length=1, max_length=32),
    kind: str | None = Query(default=None, min_length=1, max_length=32),
    enabled: bool | None = None,
    search: str | None = Query(default=None, min_length=1, max_length=256),
) -> dict:
    filters = []
    values: list[object] = []
    if channel:
        filters.append("channel = ?")
        values.append(channel)
    if kind:
        filters.append("kind = ?")
        values.append(kind)
    if enabled is not None:
        filters.append("enabled = ?")
        values.append(int(enabled))
    if search:
        escaped = search.replace("\\", "\\\\").replace("%", "\\%").replace("_", "\\_")
        filters.append("(id LIKE ? ESCAPE '\\' OR display_name LIKE ? ESCAPE '\\')")
        values.extend((f"%{escaped}%", f"%{escaped}%"))
    where = f"WHERE {' AND '.join(filters)}" if filters else ""
    offset = (page - 1) * page_size
    with database(get_settings().db_path) as conn:
        total = int(
            conn.execute(f"SELECT COUNT(*) FROM models {where}", values).fetchone()[0]
        )
        rows = conn.execute(
            f"""SELECT id, channel, upstream_id, display_name, kind, caps,
            context_window, max_output, multiplier, enabled
            FROM models {where} ORDER BY channel, display_name, id LIMIT ? OFFSET ?""",
            [*values, page_size, offset],
        ).fetchall()
        kinds = [
            row["kind"]
            for row in conn.execute(
                "SELECT DISTINCT kind FROM models WHERE kind != '' ORDER BY kind"
            ).fetchall()
        ]
    return {
        "data": [
            {
                **{key: row[key] for key in row.keys() if key != "caps"},
                "enabled": bool(row["enabled"]),
                "caps": json.loads(row["caps"]),
            }
            for row in rows
        ],
        "pagination": {
            "page": page,
            "page_size": page_size,
            "total": total,
            "total_pages": (total + page_size - 1) // page_size,
        },
        "source": "observed_cache",
        "facets": {"kinds": kinds},
    }


@router.patch("/{model_id:path}", dependencies=[Depends(_require_model_admin)])
def patch_model(model_id: str, body: ModelPatch, request: Request, user: AdminUser) -> dict:
    if not _MODEL_ID.fullmatch(model_id):
        raise HTTPException(status_code=404, detail="model not found")
    with database(get_settings().db_path) as conn:
        conn.execute("BEGIN IMMEDIATE")
        row = conn.execute("SELECT enabled FROM models WHERE id = ?", (model_id,)).fetchone()
        if row is None:
            raise HTTPException(status_code=404, detail="model not found in observed cache")
        conn.execute("UPDATE models SET enabled = ? WHERE id = ?", (int(body.enabled), model_id))
        conn.execute(
            "INSERT INTO audit_logs(ts, actor, action, target, detail, ip) "
            "VALUES (?, ?, 'set_model_enabled', ?, ?, ?)",
            (
                int(time.time()),
                str(user.get("username", "admin"))[:128],
                model_id[:320],
                str(bool(body.enabled)).lower(),
                str(request.client.host if request.client else "")[:64],
            ),
        )
        result = conn.execute(
            """SELECT id, channel, upstream_id, display_name, kind, caps,
            context_window, max_output, multiplier, enabled FROM models WHERE id = ?""",
            (model_id,),
        ).fetchone()
    data = {key: result[key] for key in result.keys() if key != "caps"}
    data["enabled"] = bool(result["enabled"])
    data["caps"] = json.loads(result["caps"])
    return {"data": data}
