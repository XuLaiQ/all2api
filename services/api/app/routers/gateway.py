from __future__ import annotations

import asyncio
import json
import re
import time
from datetime import UTC, datetime
from typing import Annotated, Any

import httpx
from fastapi import APIRouter, Depends, HTTPException, Request
from fastapi.responses import JSONResponse, Response, StreamingResponse
from starlette.background import BackgroundTask

from app.adapters.registry import AdapterSpec, get_registry
from app.config import get_settings
from app.db import database
from app.protocols.anthropic import (
    AnthropicRequestError,
    anthropic_error_payload,
    chat_response_to_messages,
    messages_sse,
    normalize_messages_request,
    openai_error_to_anthropic,
)
from app.protocols.responses import (
    ResponsesRequestError,
    chat_response_to_responses,
    normalize_responses_request,
    responses_sse,
)
from app.scheduler.pool import (
    WorkBuddyCandidate,
    acquire_workbuddy_lease,
    workbuddy_candidates,
)
from app.scheduler.runtime import (
    block_until,
    record_account_rate_limit,
    record_account_success,
    record_account_transient_failure,
    record_model_not_found,
    record_rate_limit,
    record_success,
    record_transient_failure,
    retry_after_seconds,
)
from app.security import require_api_key

router = APIRouter(prefix="/v1")
KeyContext = Annotated[dict, Depends(require_api_key)]
_upstream_slots = asyncio.Semaphore(64)


def _allowed(key: dict[str, Any], channel: str, model_id: str) -> bool:
    try:
        channels = json.loads(key["channels"])
        models = json.loads(key["models"])
    except (TypeError, json.JSONDecodeError):
        return False
    if not isinstance(channels, list) or not isinstance(models, list):
        return False
    return (not channels or channel in channels) and ("*" in models or model_id in models)


def _model_disabled(channel: str, upstream_model: str) -> bool:
    with database(get_settings().db_path) as conn:
        row = conn.execute(
            "SELECT enabled FROM models WHERE id = ?",
            (f"{channel}/{upstream_model}",),
        ).fetchone()
    return row is not None and not bool(row["enabled"])


def _log_request(
    *,
    request_id: str,
    key_id: int,
    model: str,
    status: int,
    started: float,
    upstream_model: str | None = None,
    error: str | None = None,
    error_kind: str | None = None,
    prompt_tokens: int = 0,
    completion_tokens: int = 0,
    usage_reported: bool = False,
    stream: bool = False,
    account_id: str | None = None,
    channel: str = "wb",
    route_alias: str | None = None,
    fallback_depth: int = 0,
) -> None:
    ts = int(time.time())
    day = datetime.fromtimestamp(ts, UTC).date().isoformat()
    with database(get_settings().db_path) as conn:
        conn.execute(
            """INSERT INTO request_logs
            (ts, request_id, channel, key_id, account_id, model, upstream_model,
             route_alias, fallback_depth, status, error_kind, error, stream, prompt_tokens,
             completion_tokens, usage_reported, latency_ms)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)""",
            (
                ts, request_id, channel, key_id, account_id, model,
                upstream_model if upstream_model is not None else model.partition("/")[2],
                route_alias, fallback_depth, status, error_kind, error, int(stream),
                prompt_tokens, completion_tokens,
                int(usage_reported),
                int((time.monotonic() - started) * 1000),
            ),
        )
        conn.execute(
            """INSERT INTO usage_daily
            (day, channel, key_id, model, requests, prompt_tokens, completion_tokens,
             usage_reported_requests)
            VALUES (?, ?, ?, ?, 1, ?, ?, ?)
            ON CONFLICT(day, channel, key_id, model) DO UPDATE SET
                requests=usage_daily.requests + excluded.requests,
                prompt_tokens=usage_daily.prompt_tokens + excluded.prompt_tokens,
                completion_tokens=usage_daily.completion_tokens + excluded.completion_tokens,
                usage_reported_requests=usage_daily.usage_reported_requests +
                    excluded.usage_reported_requests""",
            (
                day,
                channel,
                key_id,
                model,
                prompt_tokens,
                completion_tokens,
                int(usage_reported),
            ),
        )


def _account_id_from_upstream(
    request_id: str,
    headers: httpx.Headers,
    channel: str,
) -> str | None:
    if channel != "wb":
        return None
    if headers.get("X-WB-Request-ID") != request_id:
        return None
    native_id = headers.get("X-WB-Account-ID", "")
    if not re.fullmatch(r"(?:cn|global):[A-Za-z0-9@._-]{1,128}", native_id):
        return None
    with database(get_settings().db_path) as conn:
        row = conn.execute(
            "SELECT id FROM accounts WHERE channel = ? AND native_id = ?",
            (channel, native_id),
        ).fetchone()
    return str(row["id"]) if row else None


def _is_account_unavailable(response: httpx.Response) -> bool:
    return response.status_code == 409 and _upstream_error_code(response) == "account_unavailable"


def _account_selection_mismatch(
    request_id: str,
    response: httpx.Response,
    target: dict[str, str],
) -> bool:
    expected = target.get("account_id")
    if expected is None or _is_account_unavailable(response):
        return False
    if response.status_code in {400, 401, 403}:
        return False
    return (
        response.headers.get("X-WB-Request-ID") != request_id
        or response.headers.get("X-WB-Account-ID") != expected
    )


def _resolve_targets(
    model_id: str,
    key: dict[str, Any],
    adapters: dict[str, AdapterSpec],
    *,
    respect_runtime: bool = True,
) -> tuple[str | None, list[dict[str, str]]]:
    channel, separator, upstream_model = model_id.partition("/")
    if separator:
        adapter = adapters.get(channel)
        if adapter is None:
            raise HTTPException(status_code=404, detail="model channel is not registered")
        if not upstream_model:
            raise HTTPException(status_code=400, detail="model name after channel prefix is empty")
        if not _allowed(key, channel, model_id):
            raise HTTPException(status_code=403, detail="API key is not authorized for this model")
        if not adapter.models_configured:
            raise HTTPException(status_code=503, detail=f"{adapter.name} is not configured")
        if _model_disabled(channel, upstream_model):
            raise HTTPException(status_code=404, detail="model is disabled by an administrator")
        if respect_runtime:
            blocked_until = block_until(channel, upstream_model)
            if blocked_until:
                retry_after = max(1, blocked_until - int(time.time()))
                raise HTTPException(
                    status_code=503,
                    detail=f"{adapter.name} is cooling down",
                    headers={"Retry-After": str(retry_after)},
                )
        return None, [{"channel": channel, "upstream_model": upstream_model}]

    with database(get_settings().db_path) as conn:
        route = conn.execute(
            "SELECT alias, enabled FROM routes WHERE alias = ?",
            (model_id,),
        ).fetchone()
        if route is None or not route["enabled"]:
            raise HTTPException(status_code=404, detail="model alias is not registered")
        rows = conn.execute(
            "SELECT channel, model FROM route_targets WHERE alias = ? ORDER BY position",
            (model_id,),
        ).fetchall()

    try:
        channels = json.loads(key["channels"])
        allowed_models = json.loads(key["models"])
    except (TypeError, json.JSONDecodeError):
        raise HTTPException(status_code=401, detail="API key scope is invalid") from None
    if not isinstance(channels, list) or not isinstance(allowed_models, list):
        raise HTTPException(status_code=401, detail="API key scope is invalid")

    alias_allowed = "*" in allowed_models or model_id in allowed_models
    targets: list[dict[str, str]] = []
    has_authorized_target = False
    blocked_deadlines: list[int] = []
    for row in rows:
        target_channel = str(row["channel"])
        adapter = adapters.get(target_channel)
        target_model = str(row["model"])
        if target_model.startswith(f"{target_channel}/"):
            target_model = target_model[len(target_channel) + 1:]
        full_model = f"{target_channel}/{target_model}"
        channel_allowed = not channels or target_channel in channels
        model_allowed = alias_allowed or full_model in allowed_models
        if not channel_allowed or not model_allowed:
            continue
        has_authorized_target = True
        if adapter and adapter.models_configured and target_model:
            if _model_disabled(target_channel, target_model):
                continue
            blocked_until = (
                block_until(target_channel, target_model)
                if respect_runtime
                else None
            )
            if blocked_until:
                blocked_deadlines.append(blocked_until)
                continue
            targets.append({"channel": target_channel, "upstream_model": target_model})
    if not has_authorized_target:
        raise HTTPException(status_code=403, detail="API key is not authorized for this route")
    if not targets:
        if blocked_deadlines:
            retry_after = max(1, min(blocked_deadlines) - int(time.time()))
            raise HTTPException(
                status_code=503,
                detail="all route targets are cooling down",
                headers={"Retry-After": str(retry_after)},
            )
        raise HTTPException(
            status_code=503,
            detail="no configured targets are available for this route",
        )
    return model_id, targets


_RETRYABLE_UPSTREAM_STATUSES = {429, 500, 502, 503, 504}
_TRANSIENT_UPSTREAM_STATUSES = {500, 502, 503, 504}


def _upstream_error_code(response: httpx.Response) -> str:
    try:
        body = response.json()
    except (ValueError, TypeError):
        return ""
    if not isinstance(body, dict):
        return ""
    error = body.get("error")
    if isinstance(error, dict):
        code = error.get("code")
    else:
        code = body.get("code")
    return str(code or "").strip().lower()


def _is_model_not_found(response: httpx.Response) -> bool:
    return response.status_code == 404 and _upstream_error_code(response) in {
        "11102",
        "model_not_found",
        "model_not_available",
    }


def _record_upstream_status(channel: str, model: str, response: httpx.Response) -> None:
    status = response.status_code
    if status == 429:
        model_limited = _upstream_error_code(response) == "6004"
        record_rate_limit(
            channel,
            model,
            status,
            retry_after_seconds(response.headers.get("retry-after")),
            model_limited=model_limited,
        )
    elif status in _TRANSIENT_UPSTREAM_STATUSES:
        record_transient_failure(channel, status, "upstream_server_error")
    elif 200 <= status < 300:
        record_success(channel, model)


def _record_account_upstream_status(
    account_id: str | None,
    response: httpx.Response,
) -> None:
    if account_id is None:
        return
    status = response.status_code
    if status == 429:
        if _upstream_error_code(response) != "6004":
            record_account_rate_limit(
                account_id,
                status,
                retry_after_seconds(response.headers.get("retry-after")),
            )
    elif status in _TRANSIENT_UPSTREAM_STATUSES:
        record_account_transient_failure(account_id, status, "upstream_server_error")
    elif 200 <= status < 300:
        record_account_success(account_id)


def _error_kind_for_status(status: int) -> str | None:
    if status == 429:
        return "rate_limited"
    if status in _TRANSIENT_UPSTREAM_STATUSES:
        return "upstream_server_error"
    return None


def _client_response_headers(request_id: str, upstream: httpx.Response) -> dict[str, str]:
    headers = {"X-Request-ID": request_id}
    retry_after = retry_after_seconds(upstream.headers.get("retry-after"))
    if retry_after is not None:
        headers["Retry-After"] = str(retry_after)
    return headers


def _stream_error_frame(message: str) -> bytes:
    payload = {
        "error": {
            "message": message,
            "type": "api_error",
            "param": None,
            "code": "upstream_error",
        }
    }
    return f"data: {json.dumps(payload, separators=(',', ':'))}\n\n".encode()


def _token_usage(usage: Any) -> tuple[int, int] | None:
    if not isinstance(usage, dict):
        return None
    prompt = usage.get("prompt_tokens")
    completion = usage.get("completion_tokens")
    if (
        type(prompt) is not int
        or prompt < 0
        or type(completion) is not int
        or completion < 0
    ):
        return None
    return prompt, completion


def _extract_sse_usage(buffer: bytes) -> tuple[bytes, tuple[int, int] | None]:
    latest_usage = None
    while True:
        boundaries = [
            (index, separator)
            for separator in (b"\r\n\r\n", b"\n\n")
            if (index := buffer.find(separator)) >= 0
        ]
        if not boundaries:
            return buffer, latest_usage
        index, separator = min(boundaries, key=lambda item: item[0])
        frame, buffer = buffer[:index], buffer[index + len(separator):]
        data_lines = []
        for line in frame.replace(b"\r\n", b"\n").split(b"\n"):
            if line.startswith(b"data:"):
                value = line[5:]
                data_lines.append(value[1:] if value.startswith(b" ") else value)
        if not data_lines:
            continue
        try:
            payload = json.loads(b"\n".join(data_lines))
        except (TypeError, ValueError, UnicodeDecodeError):
            continue
        usage = _token_usage(payload.get("usage")) if isinstance(payload, dict) else None
        if usage is not None:
            latest_usage = usage
        if len(buffer) > 1_048_576:
            buffer = b""


@router.get("/models", tags=["gateway"], response_model=None)
async def list_models(key: KeyContext) -> dict:
    adapters = get_registry()
    configured = [adapter for adapter in adapters.values() if adapter.models_configured]
    results = await asyncio.gather(
        *(adapter.list_models() for adapter in configured),
        return_exceptions=True,
    )
    failures = [
        adapter.slug
        for adapter, result in zip(configured, results, strict=True)
        if isinstance(result, Exception)
    ]
    models: list[dict[str, Any]] = []
    rows: list[tuple[str, str, str, str, str, str]] = []
    now = int(time.time())
    for adapter, result in zip(configured, results, strict=True):
        if isinstance(result, Exception):
            continue
        for item in result:
            upstream_id = item.get("id")
            if not isinstance(upstream_id, str) or not upstream_id:
                continue
            model_id = f"{adapter.slug}/{upstream_id}"
            if not _allowed(key, adapter.slug, model_id):
                continue
            display_name = str(item.get("name") or item.get("display_name") or upstream_id)
            kind = str(item.get("kind") or "chat")
            caps = item.get("caps") if isinstance(item.get("caps"), list) else ["chat"]
            models.append(
                {
                    "id": model_id,
                    "object": "model",
                    "created": now,
                    "owned_by": adapter.slug,
                    "name": display_name,
                }
            )
            rows.append(
                (model_id, adapter.slug, upstream_id, display_name, kind, json.dumps(caps))
            )
    if rows:
        with database(get_settings().db_path) as conn:
            conn.executemany(
                """INSERT INTO models(id, channel, upstream_id, display_name, kind, caps, enabled)
                VALUES (?, ?, ?, ?, ?, ?, 1)
                ON CONFLICT(id) DO UPDATE SET display_name=excluded.display_name,
                kind=excluded.kind, caps=excluded.caps""",
                rows,
            )
        with database(get_settings().db_path) as conn:
            enabled_ids = {
                row["id"] for row in conn.execute("SELECT id FROM models WHERE enabled = 1")
            }
        models = [item for item in models if item["id"] in enabled_ids]
    with database(get_settings().db_path) as conn:
        aliases = [
            row["alias"]
            for row in conn.execute("SELECT alias FROM routes WHERE enabled = 1 ORDER BY alias")
        ]
    for alias in aliases:
        try:
            _resolve_targets(alias, key, adapters, respect_runtime=False)
        except HTTPException:
            continue
        models.append(
            {
                "id": alias,
                "object": "model",
                "created": now,
                "owned_by": "all2api",
            }
        )
    if configured and failures and len(failures) == len(configured) and not models:
        raise HTTPException(status_code=502, detail="all configured model services are unavailable")
    return {"object": "list", "data": models}


@router.post("/messages", tags=["gateway"])
async def anthropic_messages(request: Request, key: KeyContext) -> Response:
    request_id = request.state.request_id
    try:
        payload = await request.json()
    except Exception:
        return JSONResponse(
            status_code=400,
            content=anthropic_error_payload("request body must be valid JSON", 400),
            headers={"X-Request-ID": request_id, "request-id": request_id},
        )
    try:
        canonical = normalize_messages_request(payload)
    except AnthropicRequestError as exc:
        return JSONResponse(
            status_code=400,
            content=anthropic_error_payload(str(exc), 400),
            headers={"X-Request-ID": request_id, "request-id": request_id},
        )

    try:
        response = await _dispatch_chat(request, key, canonical)
    except HTTPException as exc:
        headers = {"X-Request-ID": request_id, "request-id": request_id}
        if exc.headers:
            headers.update(exc.headers)
        return JSONResponse(
            status_code=exc.status_code,
            content=anthropic_error_payload(str(exc.detail), exc.status_code),
            headers=headers,
        )

    if isinstance(response, StreamingResponse):
        return StreamingResponse(
            messages_sse(
                response.body_iterator,
                canonical["model"],
                allow_parallel_tools=canonical.get("parallel_tool_calls", True),
            ),
            status_code=response.status_code,
            headers={
                "X-Request-ID": request_id,
                "request-id": request_id,
                "Cache-Control": "no-cache",
                "Content-Type": "text/event-stream",
            },
        )

    if response.status_code >= 400:
        content = response.body if isinstance(response.body, bytes) else b""
        headers = {"X-Request-ID": request_id, "request-id": request_id}
        if response.headers.get("retry-after"):
            headers["Retry-After"] = response.headers["retry-after"]
        return JSONResponse(
            status_code=response.status_code,
            content=openai_error_to_anthropic(content, response.status_code),
            headers=headers,
        )

    try:
        body = json.loads(response.body)
        converted = chat_response_to_messages(body, canonical["model"])
    except (TypeError, ValueError, json.JSONDecodeError) as exc:
        return JSONResponse(
            status_code=502,
            content=anthropic_error_payload(str(exc), 502),
            headers={"X-Request-ID": request_id, "request-id": request_id},
        )
    return JSONResponse(
        status_code=response.status_code,
        content=converted,
        headers={"X-Request-ID": request_id, "request-id": request_id},
    )


@router.post("/responses", tags=["gateway"])
async def openai_responses(request: Request, key: KeyContext) -> Response:
    request_id = request.state.request_id
    try:
        payload = await request.json()
    except Exception:
        return JSONResponse(
            status_code=400,
            content={
                "error": {
                    "message": "request body must be valid JSON",
                    "type": "invalid_request_error",
                    "param": None,
                    "code": "invalid_json",
                }
            },
            headers={"X-Request-ID": request_id},
        )
    try:
        canonical = normalize_responses_request(payload)
    except ResponsesRequestError as exc:
        return JSONResponse(
            status_code=400,
            content={
                "error": {
                    "message": str(exc),
                    "type": "invalid_request_error",
                    "param": None,
                    "code": "invalid_request_error",
                }
            },
            headers={"X-Request-ID": request_id},
        )
    try:
        response = await _dispatch_chat(request, key, canonical)
    except HTTPException as exc:
        headers = {"X-Request-ID": request_id, **(exc.headers or {})}
        return JSONResponse(
            status_code=exc.status_code,
            content={
                "error": {
                    "message": str(exc.detail),
                    "type": "invalid_request_error" if exc.status_code == 400 else "api_error",
                    "param": None,
                    "code": "request_error",
                }
            },
            headers=headers,
        )
    if isinstance(response, StreamingResponse):
        return StreamingResponse(
            responses_sse(response.body_iterator, canonical["model"]),
            status_code=response.status_code,
            headers={
                "X-Request-ID": request_id,
                "Cache-Control": "no-cache",
                "Content-Type": "text/event-stream",
            },
        )
    if response.status_code >= 400:
        headers = {"X-Request-ID": request_id}
        if response.headers.get("retry-after"):
            headers["Retry-After"] = response.headers["retry-after"]
        return Response(
            content=response.body,
            status_code=response.status_code,
            media_type=response.headers.get("content-type", "application/json"),
            headers=headers,
        )
    try:
        result = json.loads(response.body)
        converted = chat_response_to_responses(result, canonical["model"])
    except (TypeError, ValueError, json.JSONDecodeError) as exc:
        return JSONResponse(
            status_code=502,
            content={
                "error": {
                    "message": str(exc),
                    "type": "api_error",
                    "param": None,
                    "code": "upstream_protocol_error",
                }
            },
            headers={"X-Request-ID": request_id},
        )
    return JSONResponse(
        status_code=response.status_code,
        content=converted,
        headers={"X-Request-ID": request_id},
    )


@router.post("/chat/completions", tags=["gateway"])
async def chat_completions(
    request: Request,
    key: KeyContext,
) -> Response:
    try:
        payload = await request.json()
    except Exception as exc:
        raise HTTPException(status_code=400, detail="request body must be valid JSON") from exc
    return await _dispatch_chat(request, key, payload)


async def _dispatch_chat(request: Request, key: dict, payload: Any) -> Response:
    if not isinstance(payload, dict) or not isinstance(payload.get("model"), str):
        raise HTTPException(status_code=422, detail="model is required")
    model_id = payload["model"]
    adapters = get_registry()
    route_alias, targets = _resolve_targets(model_id, key, adapters)

    def log_request(**values: Any) -> None:
        values["model"] = model_id
        values["route_alias"] = route_alias
        values.setdefault("upstream_model", targets[0]["upstream_model"])
        _log_request(**values)

    request_id = request.state.request_id
    started = time.monotonic()
    trace_secret = get_settings().wb_trace_secret.get_secret_value()
    if any(target["channel"] == "wb" for target in targets) and trace_secret and len(
        trace_secret.encode("utf-8")
    ) < 32:
        log_request(
            request_id=request_id,
            key_id=key["id"],
            channel="wb",
            status=503,
            started=started,
            error="invalid WorkBuddy trace secret configuration",
            stream=payload.get("stream") is True,
        )
        raise HTTPException(
            status_code=503,
            detail="A2A_WB_TRACE_SECRET must contain at least 32 bytes",
        )
    expanded_targets: list[dict[str, str]] = []
    for route_depth, target in enumerate(targets):
        base_target = {**target, "route_depth": str(route_depth)}
        if target["channel"] != "wb" or not trace_secret:
            expanded_targets.append(base_target)
            continue
        has_snapshot, candidates = workbuddy_candidates(target["upstream_model"])
        if not has_snapshot:
            expanded_targets.append(base_target)
            continue
        for candidate in candidates:
            expanded_targets.append(
                {
                    **base_target,
                    "account_id": candidate.native_id,
                    "lease_account_id": candidate.account_id,
                }
            )
    if not expanded_targets:
        first = targets[0]
        log_request(
            request_id=request_id,
            key_id=key["id"],
            channel=first["channel"],
            upstream_model=first["upstream_model"],
            status=503,
            started=started,
            error="no eligible WorkBuddy accounts in the synchronized snapshot",
            error_kind="account_unavailable",
            stream=payload.get("stream") is True,
        )
        raise HTTPException(
            status_code=503,
            detail="no eligible WorkBuddy accounts are available",
            headers={"Retry-After": "1"},
        )
    targets = expanded_targets
    try:
        await asyncio.wait_for(_upstream_slots.acquire(), timeout=1)
    except TimeoutError as exc:
        log_request(
            request_id=request_id,
            key_id=key["id"],
            channel=targets[0]["channel"],
            upstream_model=targets[0]["upstream_model"],
            status=503,
            started=started,
            error="gateway concurrency limit reached",
            stream=payload.get("stream") is True,
        )
        raise HTTPException(
            status_code=503,
            detail="gateway is at its upstream concurrency limit",
            headers={"Retry-After": "1"},
        ) from exc

    def prepare_attempt(target: dict[str, str]) -> tuple[AdapterSpec, bytes, dict[str, str], str]:
        adapter = adapters[target["channel"]]
        attempt_body = dict(payload)
        attempt_body["model"] = target["upstream_model"]
        body = json.dumps(attempt_body, separators=(",", ":")).encode()
        headers = {"Content-Type": "application/json"}
        if adapter.model_key:
            headers["Authorization"] = f"Bearer {adapter.model_key}"
        if adapter.slug == "wb" and trace_secret:
            headers["X-A2A-Trace-Secret"] = trace_secret
            headers["X-A2A-Request-ID"] = request_id
            if target.get("account_id"):
                headers["X-A2A-Account-ID"] = target["account_id"]
        if request.headers.get("accept"):
            headers["Accept"] = request.headers["accept"]
        url = f"{adapter.base_url.rstrip('/')}/v1/chat/completions"
        return adapter, body, headers, url

    if payload.get("stream") is True:
        selected: tuple[
            int, httpx.AsyncClient, httpx.Response, AdapterSpec, dict[str, str], Any
        ] | None = None
        last_status = 502
        for attempt_index, target in enumerate(targets):
            depth = int(target["route_depth"])
            account_lease = None
            if target.get("lease_account_id"):
                candidate = WorkBuddyCandidate(
                    target["lease_account_id"], target["account_id"]
                )
                account_lease = acquire_workbuddy_lease(candidate)
                if account_lease is None:
                    last_status = 503
                    continue
            adapter, body, headers, url = prepare_attempt(target)
            client = httpx.AsyncClient(timeout=httpx.Timeout(300, connect=10))
            try:
                upstream_request = client.build_request(
                    "POST", url, content=body, headers=headers
                )
                upstream = await client.send(upstream_request, stream=True)
            except asyncio.CancelledError:
                await client.aclose()
                _upstream_slots.release()
                if account_lease is not None:
                    account_lease.release()
                log_request(
                    request_id=request_id,
                    key_id=key["id"],
                    channel=adapter.slug,
                    upstream_model=target["upstream_model"],
                    status=499,
                    started=started,
                    error="client disconnected before upstream response",
                    stream=True,
                    fallback_depth=depth,
                )
                raise
            except Exception as exc:
                await client.aclose()
                if account_lease is not None:
                    account_lease.release()
                last_status = 502
                if isinstance(exc, httpx.HTTPError):
                    record_transient_failure(adapter.slug, 502, "transport_error")
                if attempt_index + 1 < len(targets):
                    continue
                _upstream_slots.release()
                log_request(
                    request_id=request_id,
                    key_id=key["id"],
                    channel=adapter.slug,
                    upstream_model=target["upstream_model"],
                    status=last_status,
                    started=started,
                    error="upstream unavailable",
                    error_kind=("transport_error" if isinstance(exc, httpx.HTTPError) else None),
                    stream=True,
                    fallback_depth=depth,
                )
                raise HTTPException(
                    status_code=502,
                    detail=f"{adapter.name} is unavailable",
                ) from exc
            if _account_selection_mismatch(request_id, upstream, target):
                await upstream.aclose()
                await client.aclose()
                if account_lease is not None:
                    account_lease.release()
                _upstream_slots.release()
                log_request(
                    request_id=request_id,
                    key_id=key["id"],
                    channel=adapter.slug,
                    upstream_model=target["upstream_model"],
                    status=502,
                    started=started,
                    error="WorkBuddy did not honor the requested account",
                    error_kind="account_selection_mismatch",
                    stream=True,
                    fallback_depth=depth,
                )
                return JSONResponse(
                    status_code=502,
                    content={
                        "error": {
                            "message": "WorkBuddy did not confirm the requested account",
                            "type": "api_error",
                            "param": None,
                            "code": "account_selection_mismatch",
                        }
                    },
                    headers={"X-Request-ID": request_id},
                )
            if upstream.status_code >= 400:
                error_account_id = _account_id_from_upstream(
                    request_id,
                    upstream.headers,
                    adapter.slug,
                )
                try:
                    error_body = await upstream.aread()
                except httpx.HTTPError as exc:
                    await upstream.aclose()
                    await client.aclose()
                    record_transient_failure(adapter.slug, 502, "error_body_read_failed")
                    if account_lease is not None:
                        account_lease.release()
                    if error_account_id is not None:
                        record_account_transient_failure(
                            error_account_id, 502, "error_body_read_failed"
                        )
                    last_status = 502
                    if attempt_index + 1 < len(targets):
                        continue
                    _upstream_slots.release()
                    log_request(
                        request_id=request_id,
                        key_id=key["id"],
                        channel=adapter.slug,
                        upstream_model=target["upstream_model"],
                        status=502,
                        started=started,
                        error="upstream error response could not be read",
                        error_kind="error_body_read_failed",
                        stream=True,
                        account_id=error_account_id,
                        fallback_depth=depth,
                    )
                    raise HTTPException(
                        status_code=502,
                        detail=f"{adapter.name} error response was incomplete",
                    ) from exc
                _record_account_upstream_status(error_account_id, upstream)
                model_not_found = _is_model_not_found(upstream)
                if model_not_found:
                    record_model_not_found(
                        adapter.slug,
                        target["upstream_model"],
                        upstream.status_code,
                    )
                if upstream.status_code in _RETRYABLE_UPSTREAM_STATUSES:
                    _record_upstream_status(adapter.slug, target["upstream_model"], upstream)
                account_unavailable = _is_account_unavailable(upstream)
                should_retry = (
                    upstream.status_code in _RETRYABLE_UPSTREAM_STATUSES
                    or model_not_found
                    or account_unavailable
                )
                if should_retry and attempt_index + 1 < len(targets):
                    last_status = 503 if account_unavailable else upstream.status_code
                    await upstream.aclose()
                    await client.aclose()
                    if account_lease is not None:
                        account_lease.release()
                    continue
                await upstream.aclose()
                await client.aclose()
                _upstream_slots.release()
                if account_lease is not None:
                    account_lease.release()
                if account_unavailable:
                    log_request(
                        request_id=request_id,
                        key_id=key["id"],
                        channel=adapter.slug,
                        upstream_model=target["upstream_model"],
                        status=503,
                        started=started,
                        error="WorkBuddy has no capacity for the selected account",
                        error_kind="account_unavailable",
                        stream=True,
                        fallback_depth=depth,
                    )
                    return JSONResponse(
                        status_code=503,
                        content={
                            "error": {
                                "message": "no eligible WorkBuddy account is available",
                                "type": "service_unavailable",
                                "param": None,
                                "code": "account_unavailable",
                            }
                        },
                        headers={"X-Request-ID": request_id, "Retry-After": "1"},
                    )
                log_request(
                    request_id=request_id,
                    key_id=key["id"],
                    channel=adapter.slug,
                    upstream_model=target["upstream_model"],
                    status=upstream.status_code,
                    started=started,
                    error=error_body[:1000].decode("utf-8", errors="replace"),
                    error_kind=_error_kind_for_status(upstream.status_code)
                    or ("model_not_found" if model_not_found else None),
                    stream=True,
                    account_id=error_account_id,
                    fallback_depth=depth,
                )
                return Response(
                    content=error_body,
                    status_code=upstream.status_code,
                    media_type=upstream.headers.get("content-type", "application/json"),
                    headers=_client_response_headers(request_id, upstream),
                )
            selected = (depth, client, upstream, adapter, target, account_lease)
            break
        if selected is None:
            _upstream_slots.release()
            first = targets[0]
            log_request(
                request_id=request_id,
                key_id=key["id"],
                channel=first["channel"],
                upstream_model=first["upstream_model"],
                status=last_status,
                started=started,
                error="no WorkBuddy account lease was available",
                error_kind="account_unavailable",
                stream=True,
                fallback_depth=int(first["route_depth"]),
            )
            raise HTTPException(status_code=last_status, detail="all route targets failed")
        fallback_depth, client, upstream, adapter, target, account_lease = selected
        account_id = _account_id_from_upstream(request_id, upstream.headers, adapter.slug)
        prompt_tokens = completion_tokens = 0
        usage_reported = False

        async def chunks():
            nonlocal prompt_tokens, completion_tokens, usage_reported
            status = upstream.status_code
            stream_error = "upstream request failed" if status >= 400 else None
            stream_error_kind = None
            completed = False
            pending = b""
            try:
                async for chunk in upstream.aiter_bytes():
                    pending += chunk
                    pending, usage = _extract_sse_usage(pending)
                    if usage is not None:
                        prompt_tokens, completion_tokens = usage
                        usage_reported = True
                    if len(pending) > 1_048_576:
                        pending = b""
                    yield chunk
                completed = True
            except asyncio.CancelledError:
                status = 499
                stream_error = "client disconnected"
                raise
            except GeneratorExit:
                status = 499
                stream_error = "client disconnected"
                raise
            except httpx.HTTPError:
                status = 502
                stream_error = "upstream stream interrupted"
                stream_error_kind = "stream_interrupted"
                record_transient_failure(adapter.slug, status, "stream_interrupted")
                if account_id is not None:
                    record_account_transient_failure(
                        account_id, status, "stream_interrupted"
                    )
                yield _stream_error_frame(stream_error)
            except Exception:
                status = 502
                stream_error = "stream interrupted"
                stream_error_kind = "stream_interrupted"
                record_transient_failure(adapter.slug, status, "stream_interrupted")
                if account_id is not None:
                    record_account_transient_failure(
                        account_id, status, "stream_interrupted"
                    )
                yield _stream_error_frame("upstream stream interrupted")
            finally:
                try:
                    await upstream.aclose()
                finally:
                    try:
                        await client.aclose()
                    finally:
                        _upstream_slots.release()
                        if account_lease is not None:
                            account_lease.release()
                        if completed and 200 <= upstream.status_code < 300:
                            record_success(adapter.slug, target["upstream_model"])
                            if account_id is not None:
                                record_account_success(account_id)
                        log_request(
                            request_id=request_id,
                            key_id=key["id"],
                            channel=adapter.slug,
                            upstream_model=target["upstream_model"],
                            status=status,
                            started=started,
                            error=stream_error,
                            error_kind=stream_error_kind or _error_kind_for_status(status),
                            stream=True,
                            account_id=account_id,
                            prompt_tokens=prompt_tokens,
                            completion_tokens=completion_tokens,
                            usage_reported=usage_reported,
                            fallback_depth=fallback_depth,
                        )

        return StreamingResponse(
            chunks(), status_code=upstream.status_code,
            background=(
                BackgroundTask(account_lease.release)
                if account_lease is not None
                else None
            ),
            headers={
                "X-Request-ID": request_id,
                "Cache-Control": "no-cache",
                "Content-Type": upstream.headers.get("content-type", "text/event-stream"),
            },
        )

    selected_response: httpx.Response | None = None
    selected_adapter: AdapterSpec | None = None
    selected_target: dict[str, str] | None = None
    fallback_depth = 0
    last_error: Exception | None = None
    last_status = 502
    for attempt_index, target in enumerate(targets):
        depth = int(target["route_depth"])
        account_lease = None
        if target.get("lease_account_id"):
            candidate = WorkBuddyCandidate(target["lease_account_id"], target["account_id"])
            account_lease = acquire_workbuddy_lease(candidate)
            if account_lease is None:
                last_status = 503
                continue
        adapter, body, headers, url = prepare_attempt(target)
        try:
            async with httpx.AsyncClient(timeout=httpx.Timeout(300, connect=10)) as client:
                upstream = await client.post(url, content=body, headers=headers)
        except asyncio.CancelledError:
            if account_lease is not None:
                account_lease.release()
            _upstream_slots.release()
            log_request(
                request_id=request_id,
                key_id=key["id"],
                channel=adapter.slug,
                upstream_model=target["upstream_model"],
                status=499,
                started=started,
                error="client disconnected",
                fallback_depth=depth,
            )
            raise
        except Exception as exc:
            if account_lease is not None:
                account_lease.release()
            last_error = exc
            last_status = 502
            if isinstance(exc, httpx.HTTPError):
                record_transient_failure(adapter.slug, 502, "transport_error")
            if attempt_index + 1 < len(targets):
                fallback_depth = depth + 1
                continue
            _upstream_slots.release()
            log_request(
                request_id=request_id,
                key_id=key["id"],
                channel=adapter.slug,
                upstream_model=target["upstream_model"],
                status=502,
                started=started,
                error="upstream unavailable",
                error_kind=("transport_error" if isinstance(exc, httpx.HTTPError) else None),
                fallback_depth=depth,
            )
            raise HTTPException(status_code=502, detail=f"{adapter.name} is unavailable") from exc
        if _account_selection_mismatch(request_id, upstream, target):
            upstream.close()
            if account_lease is not None:
                account_lease.release()
            _upstream_slots.release()
            log_request(
                request_id=request_id,
                key_id=key["id"],
                channel=adapter.slug,
                upstream_model=target["upstream_model"],
                status=502,
                started=started,
                error="WorkBuddy did not honor the requested account",
                error_kind="account_selection_mismatch",
                fallback_depth=depth,
            )
            return JSONResponse(
                status_code=502,
                content={
                    "error": {
                        "message": "WorkBuddy did not confirm the requested account",
                        "type": "api_error",
                        "code": "account_selection_mismatch",
                    }
                },
                headers={"X-Request-ID": request_id},
            )
        response_account_id = _account_id_from_upstream(
            request_id, upstream.headers, adapter.slug
        )
        _record_account_upstream_status(response_account_id, upstream)
        model_not_found = _is_model_not_found(upstream)
        if model_not_found:
            record_model_not_found(adapter.slug, target["upstream_model"], upstream.status_code)
        if upstream.status_code in _RETRYABLE_UPSTREAM_STATUSES:
            _record_upstream_status(adapter.slug, target["upstream_model"], upstream)
        elif 200 <= upstream.status_code < 300:
            record_success(adapter.slug, target["upstream_model"])
        account_unavailable = _is_account_unavailable(upstream)
        if (
            upstream.status_code in _RETRYABLE_UPSTREAM_STATUSES
            or model_not_found
            or account_unavailable
        ) and attempt_index + 1 < len(targets):
            upstream.close()
            last_status = 503 if account_unavailable else upstream.status_code
            fallback_depth = depth
            if account_lease is not None:
                account_lease.release()
            continue
        if account_lease is not None:
            account_lease.release()
        if account_unavailable:
            upstream.close()
            _upstream_slots.release()
            log_request(
                request_id=request_id,
                key_id=key["id"],
                channel=adapter.slug,
                upstream_model=target["upstream_model"],
                status=503,
                started=started,
                error="WorkBuddy has no capacity for the selected account",
                error_kind="account_unavailable",
                fallback_depth=depth,
            )
            return JSONResponse(
                status_code=503,
                content={
                    "error": {
                        "message": "no eligible WorkBuddy account is available",
                        "type": "service_unavailable",
                        "code": "account_unavailable",
                    }
                },
                headers={"X-Request-ID": request_id, "Retry-After": "1"},
            )
        selected_response = upstream
        selected_adapter = adapter
        selected_target = target
        fallback_depth = depth
        break
    _upstream_slots.release()
    if selected_response is None or selected_adapter is None or selected_target is None:
        first = targets[0]
        log_request(
            request_id=request_id,
            key_id=key["id"],
            channel=first["channel"],
            upstream_model=first["upstream_model"],
            status=last_status,
            started=started,
            error="no WorkBuddy account lease was available",
            error_kind="account_unavailable",
            fallback_depth=int(first["route_depth"]),
        )
        raise HTTPException(
            status_code=last_status,
            detail="all route targets failed",
        ) from last_error
    upstream = selected_response
    account_id = _account_id_from_upstream(request_id, upstream.headers, selected_adapter.slug)
    prompt_tokens = completion_tokens = 0
    usage_reported = False
    try:
        result = upstream.json()
        usage = result.get("usage", {}) if isinstance(result, dict) else {}
        token_usage = _token_usage(usage)
        if token_usage is not None:
            prompt_tokens, completion_tokens = token_usage
            usage_reported = True
        response_body = json.dumps(result, ensure_ascii=False).encode()
    except (ValueError, TypeError):
        response_body = upstream.content
    log_request(
        request_id=request_id,
        key_id=key["id"],
        channel=selected_adapter.slug,
        upstream_model=selected_target["upstream_model"],
        model=model_id,
        status=upstream.status_code,
        started=started, error="upstream request failed" if upstream.status_code >= 400 else None,
        error_kind=_error_kind_for_status(upstream.status_code),
        prompt_tokens=prompt_tokens, completion_tokens=completion_tokens,
        usage_reported=usage_reported,
        account_id=account_id,
        fallback_depth=fallback_depth,
    )
    content_type = upstream.headers.get("content-type", "application/json")
    return Response(
        content=response_body, status_code=upstream.status_code, media_type=content_type,
        headers=_client_response_headers(request_id, upstream),
    )
