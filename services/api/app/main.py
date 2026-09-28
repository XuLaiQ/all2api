import re
import uuid
from contextlib import asynccontextmanager

from fastapi import APIRouter, FastAPI, HTTPException, Request
from fastapi.exceptions import RequestValidationError
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import JSONResponse
from starlette.exceptions import HTTPException as StarletteHTTPException

from app.adapters.registry import get_registry
from app.config import get_settings
from app.infrastructure.db import database, migrate
from app.protocols.anthropic import anthropic_error_payload
from app.infrastructure.provision_state import ProvisionStateStore
from app.routers import admin, auth, gateway, keys, models, routes
from app.infrastructure.security import initialize_bootstrap_key, require_same_origin

settings = get_settings()


def _admin_error(
    status_code: int,
    code: str,
    message: str,
    request_id: str,
    details: dict | None = None,
    headers: dict[str, str] | None = None,
) -> JSONResponse:
    return JSONResponse(
        status_code=status_code,
        content={
            "error": {
                "code": code,
                "message": message,
                "details": details or {},
                "request_id": request_id,
            }
        },
        headers={"X-Request-ID": request_id, **(headers or {})},
    )


def _admin_error_code(status_code: int) -> str:
    return {
        400: "bad_request",
        401: "unauthorized",
        403: "forbidden",
        404: "not_found",
        409: "conflict",
        422: "validation_error",
        429: "rate_limited",
        502: "upstream_error",
        503: "service_unavailable",
        504: "upstream_timeout",
    }.get(status_code, "request_error")


@asynccontextmanager
async def lifespan(_: FastAPI):
    migrate(settings.db_path)
    initialize_bootstrap_key()
    ProvisionStateStore(
        settings.db_path,
        getattr(settings, "credential_master_key", ""),
    ).purge_expired()
    # Browser workers are opt-in; the default registry uses NullBrowserWorker.
    # Starting here gives explicitly enabled Playwright workers one owned
    # lifecycle and ensures shutdown closes contexts before process exit.
    registry = get_registry(settings)
    for adapter in registry.values():
        startup = getattr(getattr(adapter, "provisioner", None), "startup", None)
        if callable(startup):
            await startup()
    try:
        yield
    finally:
        for adapter in registry.values():
            shutdown = getattr(getattr(adapter, "provisioner", None), "shutdown", None)
            if callable(shutdown):
                await shutdown()


app = FastAPI(title="All2API Gateway", version="0.1.0", lifespan=lifespan)


@app.middleware("http")
async def request_id_middleware(request: Request, call_next):
    supplied = request.headers.get("X-Request-ID", "")
    request_id = (
        supplied
        if re.fullmatch(r"[A-Za-z0-9._:-]{1,128}", supplied)
        else f"req_{uuid.uuid4().hex}"
    )
    request.state.request_id = request_id
    protected_write = (
        request.url.path.startswith("/admin/api/")
        and request.method not in {"GET", "HEAD", "OPTIONS"}
    )
    has_session = bool(request.cookies.get("a2a_session"))
    is_login = request.url.path == "/admin/api/auth/login"
    if protected_write and (has_session or is_login):
        try:
            require_same_origin(request)
        except HTTPException as exc:
            return _admin_error(
                exc.status_code,
                _admin_error_code(exc.status_code),
                str(exc.detail),
                request_id,
            )
    response = await call_next(request)
    response.headers["X-Request-ID"] = request_id
    return response


def _openai_error(
    status_code: int,
    message: str,
    request_id: str,
    headers: dict[str, str] | None = None,
) -> JSONResponse:
    return JSONResponse(
        status_code=status_code,
        content={
            "error": {
                "message": message,
                "type": "authentication_error" if status_code == 401 else "api_error",
                "param": None,
                "code": "invalid_api_key" if status_code == 401 else "request_error",
            }
        },
        headers={"X-Request-ID": request_id, **(headers or {})},
    )


@app.exception_handler(StarletteHTTPException)
async def http_exception_handler(request: Request, exc: StarletteHTTPException):
    if request.url.path == "/v1/messages":
        detail = exc.detail if isinstance(exc.detail, str) else "request failed"
        request_id = request.state.request_id
        return JSONResponse(
            status_code=exc.status_code,
            content=anthropic_error_payload(detail, exc.status_code),
            headers={
                "X-Request-ID": request_id,
                "request-id": request_id,
                **(exc.headers or {}),
            },
        )
    if request.url.path.startswith("/v1/"):
        detail = exc.detail if isinstance(exc.detail, str) else "request failed"
        return _openai_error(
            exc.status_code,
            detail,
            request.state.request_id,
            headers=exc.headers,
        )
    if request.url.path.startswith("/admin/api/"):
        message = exc.detail if isinstance(exc.detail, str) else "request failed"
        return _admin_error(
            exc.status_code,
            _admin_error_code(exc.status_code),
            message,
            request.state.request_id,
            headers=exc.headers,
        )
    return JSONResponse(status_code=exc.status_code, content={"detail": exc.detail})


@app.exception_handler(RequestValidationError)
async def validation_exception_handler(request: Request, exc: RequestValidationError):
    if request.url.path == "/v1/messages":
        request_id = request.state.request_id
        return JSONResponse(
            status_code=422,
            content=anthropic_error_payload("request validation failed", 422),
            headers={"X-Request-ID": request_id, "request-id": request_id},
        )
    if request.url.path.startswith("/admin/api/"):
        details = [
            {
                "loc": error.get("loc", []),
                "message": error.get("msg", "invalid value"),
                "type": error.get("type", "value_error"),
            }
            for error in exc.errors()
        ]
        return _admin_error(
            422,
            "validation_error",
            "request validation failed",
            request.state.request_id,
            {"fields": details},
        )
    return JSONResponse(status_code=422, content={"detail": exc.errors()})


@app.exception_handler(Exception)
async def unhandled_exception_handler(request: Request, _: Exception):
    if request.url.path == "/v1/messages":
        request_id = request.state.request_id
        return JSONResponse(
            status_code=500,
            content=anthropic_error_payload("internal server error", 500),
            headers={"X-Request-ID": request_id, "request-id": request_id},
        )
    if request.url.path.startswith("/v1/"):
        return _openai_error(500, "internal server error", request.state.request_id)
    if request.url.path.startswith("/admin/api/"):
        return _admin_error(
            500,
            "internal_error",
            "internal server error",
            request.state.request_id,
        )
    return JSONResponse(status_code=500, content={"detail": "internal server error"})

if settings.cors_origin_list:
    app.add_middleware(
        CORSMiddleware,
        allow_origins=settings.cors_origin_list,
        allow_credentials=True,
        allow_methods=["GET", "POST", "PATCH", "DELETE", "OPTIONS"],
        allow_headers=[
            "Authorization",
            "Content-Type",
            "Idempotency-Key",
            "X-Api-Key",
            "X-Request-ID",
            "Anthropic-Version",
        ],
        expose_headers=["X-Request-ID"],
    )

admin_router = APIRouter(prefix="/admin/api")


@admin_router.get("/healthz", tags=["system"])
def healthz(detail: bool = False) -> dict[str, object]:
    try:
        with database(settings.db_path) as conn:
            conn.execute("SELECT 1").fetchone()
    except Exception as exc:
        raise HTTPException(status_code=503, detail="database unavailable") from exc
    basic = {"status": "ok", "service": "all2api-api", "database": "ok"}
    if not detail:
        # Keep the small compatibility response used by deployment probes.
        return basic

    channels: dict[str, dict[str, object]] = {}
    for adapter in get_registry(settings).values():
        data_configured = bool(getattr(adapter, "models_configured", False))
        provision_configured = bool(getattr(adapter, "provision_configured", False))
        channels[adapter.slug] = {
            "status": "ready" if data_configured else "not_configured",
            "data_plane_configured": data_configured,
            "provision_configured": provision_configured,
            # Healthz must not trigger an upstream request.  The channel test
            # endpoint is the explicit probe for platform reachability.
            "platform_probe": "not_run",
        }
    overall = "ready" if all(
        item["status"] == "ready" for item in channels.values()
    ) else "degraded"
    return {
        **basic,
        "status": overall,
        "checks": {
            "storage": {"status": "ready"},
            "channels": channels,
        },
    }


app.include_router(admin_router)
app.include_router(auth.router)
app.include_router(admin.router)
app.include_router(keys.router)
app.include_router(models.router)
app.include_router(routes.router)
app.include_router(gateway.router)
