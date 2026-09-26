from __future__ import annotations

import re
import time
from typing import Annotated

from fastapi import APIRouter, Depends, HTTPException, Request, Response
from pydantic import BaseModel, ConfigDict, Field, field_validator, model_validator

from app.adapters.registry import get_registry
from app.config import get_settings
from app.db import database
from app.security import require_admin_request

router = APIRouter(
    prefix="/admin/api/routes",
    tags=["routes"],
    dependencies=[Depends(require_admin_request)],
)
_ALIAS_PATTERN = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$")
AdminContext = Annotated[dict, Depends(require_admin_request)]


def _require_route_admin(user: AdminContext) -> dict:
    if user.get("role") != "admin":
        raise HTTPException(status_code=403, detail="admin role required")
    return user


class RouteTargetInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    channel: str = Field(min_length=1, max_length=32)
    model: str = Field(min_length=1, max_length=256)

    @field_validator("model")
    @classmethod
    def validate_model(cls, value: str) -> str:
        value = value.strip()
        if not value:
            raise ValueError("model must not be blank")
        return value


class RouteInput(BaseModel):
    model_config = ConfigDict(extra="forbid")

    strategy: str = Field(default="priority", pattern="^priority$")
    enabled: bool = True
    targets: list[RouteTargetInput] = Field(min_length=1, max_length=8)

    @model_validator(mode="after")
    def validate_target_channels(self):
        channels = [target.channel for target in self.targets]
        if len(channels) != len(set(channels)):
            raise ValueError("route targets must use distinct channels")
        return self


def _read_route(conn, alias: str) -> dict | None:
    route = conn.execute(
        "SELECT alias, strategy, enabled, created_at FROM routes WHERE alias = ?",
        (alias,),
    ).fetchone()
    if route is None:
        return None
    targets = conn.execute(
        "SELECT position, channel, model, weight FROM route_targets "
        "WHERE alias = ? ORDER BY position",
        (alias,),
    ).fetchall()
    return {
        "alias": route["alias"],
        "strategy": route["strategy"],
        "enabled": bool(route["enabled"]),
        "created_at": route["created_at"],
        "targets": [dict(target) for target in targets],
    }


@router.get("")
def list_routes() -> dict:
    with database(get_settings().db_path) as conn:
        aliases = [row["alias"] for row in conn.execute("SELECT alias FROM routes ORDER BY alias")]
        routes = [_read_route(conn, alias) for alias in aliases]
    return {"data": routes, "total": len(routes)}


@router.put("/{alias}", dependencies=[Depends(_require_route_admin)])
def put_route(alias: str, body: RouteInput, request: Request) -> dict:
    if not _ALIAS_PATTERN.fullmatch(alias):
        raise HTTPException(status_code=422, detail="invalid route alias")
    adapters = get_registry()
    seen_channels: set[str] = set()
    for target in body.targets:
        if target.channel not in adapters:
            raise HTTPException(status_code=422, detail=f"unknown channel: {target.channel}")
        if "/" in target.model and not target.model.startswith(f"{target.channel}/"):
            raise HTTPException(
                status_code=422,
                detail="target model prefix must match its channel",
            )
        if target.channel in seen_channels:
            raise HTTPException(
                status_code=422,
                detail="route targets must use distinct channels",
            )
        seen_channels.add(target.channel)

    now = int(time.time())
    with database(get_settings().db_path) as conn:
        conn.execute(
            """INSERT INTO routes(alias, strategy, enabled, created_at)
            VALUES (?, ?, ?, ?)
            ON CONFLICT(alias) DO UPDATE SET
                strategy=excluded.strategy, enabled=excluded.enabled""",
            (alias, body.strategy, int(body.enabled), now),
        )
        conn.execute("DELETE FROM route_targets WHERE alias = ?", (alias,))
        conn.executemany(
            "INSERT INTO route_targets(alias, position, channel, model) VALUES (?, ?, ?, ?)",
            [
                (alias, position, target.channel, target.model)
                for position, target in enumerate(body.targets)
            ],
        )
        conn.execute(
            """INSERT INTO audit_logs(ts, actor, action, target, detail, ip)
            VALUES (?, ?, ?, ?, ?, ?)""",
            (
                now,
                "admin",
                "upsert_route",
                alias,
                f"targets={len(body.targets)}",
                str(request.client.host if request.client else "")[:64],
            ),
        )
        result = _read_route(conn, alias)
    return {"data": result}


@router.delete("/{alias}", status_code=204, dependencies=[Depends(_require_route_admin)])
def delete_route(alias: str, request: Request) -> Response:
    if not _ALIAS_PATTERN.fullmatch(alias):
        raise HTTPException(status_code=422, detail="invalid route alias")
    with database(get_settings().db_path) as conn:
        row = conn.execute("SELECT alias FROM routes WHERE alias = ?", (alias,)).fetchone()
        if row is None:
            raise HTTPException(status_code=404, detail="route not found")
        conn.execute("DELETE FROM route_targets WHERE alias = ?", (alias,))
        conn.execute("DELETE FROM routes WHERE alias = ?", (alias,))
        conn.execute(
            """INSERT INTO audit_logs(ts, actor, action, target, detail, ip)
            VALUES (?, ?, ?, ?, ?, ?)""",
            (
                int(time.time()),
                "admin",
                "delete_route",
                alias,
                "",
                str(request.client.host if request.client else "")[:64],
            ),
        )
    return Response(status_code=204)
