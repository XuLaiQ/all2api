from __future__ import annotations

import json
import re
import time
from typing import Annotated

from fastapi import APIRouter, Depends, HTTPException, Query, Request
from pydantic import BaseModel, ConfigDict

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


class ModelPatch(BaseModel):
    model_config = ConfigDict(extra="forbid")

    enabled: bool


def _require_model_admin(user: AdminUser) -> dict:
    if user.get("role") != "admin":
        raise HTTPException(status_code=403, detail="admin role required")
    return user


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
