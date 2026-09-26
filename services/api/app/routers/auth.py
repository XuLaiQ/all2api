from __future__ import annotations

from datetime import UTC, datetime
from typing import Annotated

from fastapi import APIRouter, Depends, HTTPException, Request, Response
from pydantic import BaseModel, Field
from starlette.responses import JSONResponse

from app.config import get_settings
from app.security import (
    SESSION_COOKIE,
    admin_login_configured,
    begin_login_attempt,
    clear_login_failures,
    client_ip,
    create_session,
    current_admin_session,
    record_auth_audit,
    require_admin_request,
    revoke_session,
    session_cookie_options,
    verify_admin_credentials,
)

router = APIRouter(prefix="/admin/api/auth", tags=["auth"])
AdminContext = Annotated[dict, Depends(require_admin_request)]


class LoginRequest(BaseModel):
    username: str = Field(min_length=1, max_length=128)
    password: str = Field(min_length=1, max_length=1024)


def _utc_timestamp(value: int | None) -> str | None:
    if value is None:
        return None
    return datetime.fromtimestamp(value, tz=UTC).isoformat().replace("+00:00", "Z")


@router.post("/login")
def login(body: LoginRequest, request: Request) -> JSONResponse:
    if not admin_login_configured():
        raise HTTPException(status_code=503, detail="admin login is not configured")
    ip = client_ip(request)
    wait_seconds = begin_login_attempt(ip, body.username)
    if wait_seconds:
        raise HTTPException(
            status_code=429,
            detail="too many failed login attempts",
            headers={"Retry-After": str(wait_seconds)},
        )
    if not verify_admin_credentials(body.username, body.password):
        record_auth_audit("login_failed", body.username.strip()[:128], ip)
        raise HTTPException(status_code=401, detail="invalid username or password")

    username = get_settings().admin_username.strip()
    clear_login_failures(ip, username)
    cookie, expires_at = create_session(username)
    record_auth_audit("login", username, ip)
    response = JSONResponse(
        {
            "data": {
                "user": {"username": username, "role": "admin"},
                "expires_at": _utc_timestamp(expires_at),
            }
        },
        headers={"Cache-Control": "no-store"},
    )
    response.set_cookie(
        SESSION_COOKIE,
        cookie,
        max_age=get_settings().session_days * 86400,
        expires=datetime.fromtimestamp(expires_at, tz=UTC),
        **session_cookie_options(request),
    )
    return response


@router.get("/session")
def session(
    request: Request,
    user: AdminContext,
) -> JSONResponse:
    expires_at = None
    session_id = user.get("session_id")
    if session_id:
        current = current_admin_session(request.cookies.get(SESSION_COOKIE))
        if current:
            expires_at = _utc_timestamp(current[1].expires_at)
    return JSONResponse(
        {
            "data": {
                "user": {"username": user.get("username", "admin"), "role": user["role"]},
                "expires_at": expires_at,
            }
        },
        headers={"Cache-Control": "no-store"},
    )


@router.post("/logout", status_code=204)
def logout(request: Request) -> Response:
    current = current_admin_session(request.cookies.get(SESSION_COOKIE))
    if current:
        record_auth_audit("logout", current[1].username, client_ip(request))
    revoke_session(request.cookies.get(SESSION_COOKIE))
    response = Response(status_code=204)
    response.delete_cookie(SESSION_COOKIE, **session_cookie_options(request))
    return response
