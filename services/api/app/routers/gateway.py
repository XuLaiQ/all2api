from __future__ import annotations

import asyncio
import json
import re
import time
from collections.abc import Mapping
from datetime import UTC, datetime
from typing import Annotated, Any

import httpx
from fastapi import APIRouter, Depends, HTTPException, Request
from fastapi.responses import JSONResponse, Response, StreamingResponse
from starlette.background import BackgroundTask

from app.adapters.native_runtime import CAPABILITY_PATHS, normalize_capability
from app.adapters.registry import AdapterSpec, data_plane_configured, get_registry
from app.config import get_settings
from app.domain.scope import decode_key_scope, model_allowed, scope_decision
from app.infrastructure.db import database
from app.infrastructure.security import require_api_key
from app.infrastructure.token_usage import estimate_usage
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
from app.routers.models import adapter_catalogue_enabled, fetch_adapter_models, upsert_model_cache
from app.scheduler.pool import (
    AccountCandidate,
    account_candidates,
    acquire_account_lease,
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

router = APIRouter(prefix="/v1")
KeyContext = Annotated[dict, Depends(require_api_key)]
_upstream_slots = asyncio.Semaphore(64)
PUBLIC_MODEL_DISCOVERY_TIMEOUT_SECONDS = 5


def _allowed(key: dict[str, Any], channel: str, model_id: str) -> bool:
    """Backward-compatible boolean scope check for model listings."""

    upstream_model = model_id
    prefix, separator, suffix = model_id.partition("/")
    if separator and prefix == channel:
        upstream_model = suffix
    return scope_decision(key, channel, upstream_model) == "allowed"


def _model_disabled(channel: str, upstream_model: str) -> bool:
    with database(get_settings().db_path) as conn:
        row = conn.execute(
            "SELECT enabled FROM models WHERE id = ?",
            (f"{channel}/{upstream_model}",),
        ).fetchone()
    return row is not None and not bool(row["enabled"])


def _channel_management_enabled(channel: str) -> bool:
    """Return the persisted local enablement override for a channel.

    The registry describes what is installed; the channels table describes the
    administrator's current runtime decision. Keep those concerns separate so
    disabling a channel actually gates both direct models and aliases.
    """

    with database(get_settings().db_path) as conn:
        row = conn.execute(
            "SELECT enabled FROM channels WHERE slug = ?",
            (str(channel),),
        ).fetchone()
    return row is None or bool(row["enabled"])


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
    usage_kind: str = "unknown",
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
             completion_tokens, usage_reported, usage_kind, latency_ms)
             VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)""",
            (
                ts,
                request_id,
                channel,
                key_id,
                account_id,
                model,
                upstream_model if upstream_model is not None else model.partition("/")[2],
                route_alias,
                fallback_depth,
                status,
                error_kind,
                error,
                int(stream),
                prompt_tokens,
                completion_tokens,
                int(usage_reported),
                usage_kind,
                int((time.monotonic() - started) * 1000),
            ),
        )
        conn.execute(
            """INSERT INTO usage_daily
             (day, channel, key_id, model, requests, prompt_tokens, completion_tokens,
             usage_reported_requests, usage_estimated_requests)
             VALUES (?, ?, ?, ?, 1, ?, ?, ?, ?)
            ON CONFLICT(day, channel, key_id, model) DO UPDATE SET
                requests=usage_daily.requests + excluded.requests,
                prompt_tokens=usage_daily.prompt_tokens + excluded.prompt_tokens,
                completion_tokens=usage_daily.completion_tokens + excluded.completion_tokens,
                 usage_reported_requests=usage_daily.usage_reported_requests +
                     excluded.usage_reported_requests,
                 usage_estimated_requests=usage_daily.usage_estimated_requests +
                     excluded.usage_estimated_requests""",
            (
                day,
                channel,
                key_id,
                model,
                prompt_tokens,
                completion_tokens,
                int(usage_reported),
                int(usage_kind == "estimated"),
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


def _selected_account_id(
    request_id: str,
    headers: httpx.Headers,
    target: dict[str, str],
) -> str | None:
    """Resolve a trusted canonical account id for request logging/runtime state."""

    upstream = _account_id_from_upstream(request_id, headers, target.get("channel", ""))
    if upstream is not None:
        return upstream
    selected = target.get("lease_account_id")
    return str(selected) if selected else None


def _is_account_unavailable(response: httpx.Response) -> bool:
    return response.status_code == 409 and _upstream_error_code(response) == "account_unavailable"


def _account_selection_mismatch(
    request_id: str,
    response: httpx.Response,
    target: dict[str, str],
) -> bool:
    # The X-WB headers are emitted by the legacy bridge only.  Native
    # WorkBuddy requests are bound to the selected encrypted credential and
    # the public platform does not echo these internal routing headers.
    if target.get("verify_account_selection") != "true":
        return False
    if target.get("channel") != "wb":
        return False
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
            raise HTTPException(status_code=404, detail="channel_not_found")
        if not upstream_model:
            raise HTTPException(status_code=400, detail="model name after channel prefix is empty")
        decision = scope_decision(key, channel, upstream_model)
        if decision == "invalid_scope":
            raise HTTPException(status_code=401, detail="invalid_api_key_scope")
        if decision != "allowed":
            raise HTTPException(status_code=403, detail=decision)
        if not _channel_management_enabled(channel):
            raise HTTPException(status_code=503, detail="channel_disabled")
        if not data_plane_configured(adapter, get_settings().db_path):
            raise HTTPException(status_code=503, detail="channel_disabled")
        if _model_disabled(channel, upstream_model):
            raise HTTPException(status_code=404, detail="model_not_found")
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

    decoded_scope = decode_key_scope(key)
    if decoded_scope is None:
        raise HTTPException(status_code=401, detail="invalid_api_key_scope")
    channels, allowed_models = decoded_scope
    targets: list[dict[str, str]] = []
    has_authorized_target = False
    blocked_deadlines: list[int] = []
    disabled_channels: set[str] = set()
    for row in rows:
        target_channel = str(row["channel"])
        adapter = adapters.get(target_channel)
        target_model = str(row["model"])
        if target_model.startswith(f"{target_channel}/"):
            target_model = target_model[len(target_channel) + 1 :]
        channel_allowed = not channels or target_channel in channels
        target_allowed = model_allowed(allowed_models, target_channel, target_model)
        # Alias names never grant access by themselves.  Every expanded target
        # must independently satisfy both channel and model scopes.
        if not channel_allowed or not target_allowed:
            continue
        has_authorized_target = True
        if not _channel_management_enabled(target_channel):
            disabled_channels.add(target_channel)
            continue
        if adapter and not data_plane_configured(adapter, get_settings().db_path):
            disabled_channels.add(target_channel)
            continue
        if adapter and target_model:
            if _model_disabled(target_channel, target_model):
                continue
            blocked_until = block_until(target_channel, target_model) if respect_runtime else None
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
        if disabled_channels:
            raise HTTPException(status_code=503, detail="channel_disabled")
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


def _capability_name(value: str) -> str:
    """Normalize manifest capability names to the public dispatch vocabulary."""

    return normalize_capability(value)


def _adapter_capabilities(adapter: Any) -> set[str]:
    manifest = getattr(adapter, "manifest", None)
    values = getattr(manifest, "capabilities", None)
    if values is None:
        values = getattr(adapter, "caps", ())
    return {_capability_name(value) for value in values or ()}


def _supports_capability(adapter: Any, capability: str) -> bool:
    return _capability_name(capability) in _adapter_capabilities(adapter)


def _model_supports_capability(channel: str, model: str, capability: str) -> bool:
    """Require a live/cached model declaration for non-chat capabilities."""

    if capability == "chat":
        return True
    with database(get_settings().db_path) as conn:
        row = conn.execute(
            "SELECT caps FROM models WHERE channel = ? AND upstream_id = ? AND enabled = 1",
            (channel, model),
        ).fetchone()
    if row is None:
        return False
    try:
        values = json.loads(row["caps"] or "[]")
    except (TypeError, ValueError, json.JSONDecodeError):
        return False
    return capability in {
        _capability_name(value) for value in values if isinstance(value, str)
    }


def _resolve_capability_targets(
    model_id: str,
    key: dict[str, Any],
    adapters: dict[str, AdapterSpec],
    capability: str,
) -> tuple[str | None, list[dict[str, str]]]:
    """Resolve model/alias targets and remove channels without the capability."""

    capability = _capability_name(capability)
    if capability not in CAPABILITY_PATHS or capability == "chat":
        raise HTTPException(status_code=404, detail="capability_not_supported")

    channel, separator, _ = model_id.partition("/")
    if separator:
        adapter = adapters.get(channel)
        upstream_model = model_id[len(channel) + 1 :]
        if not _model_supports_capability(channel, upstream_model, capability):
            raise HTTPException(status_code=404, detail="capability_not_supported")
        if adapter is not None and not _supports_capability(adapter, capability):
            raise HTTPException(status_code=404, detail="capability_not_supported")

    route_alias, targets = _resolve_targets(model_id, key, adapters)
    supported = [
        target
        for target in targets
        if _supports_capability(adapters.get(target["channel"]), capability)
        and _model_supports_capability(
            target["channel"], target["upstream_model"], capability
        )
    ]
    if not supported:
        raise HTTPException(status_code=404, detail="capability_not_supported")
    return route_alias, supported


def _capability_error(
    request_id: str,
    status_code: int,
    code: str,
    message: str,
    headers: dict[str, str] | None = None,
) -> JSONResponse:
    return JSONResponse(
        status_code=status_code,
        content={
            "error": {
                "message": message,
                "type": "invalid_request_error" if status_code < 500 else "api_error",
                "param": None,
                "code": code,
            }
        },
        headers={"X-Request-ID": request_id, **(headers or {})},
    )


def _capability_error_code(detail: Any, status_code: int) -> str:
    value = str(detail or "").strip()
    stable = {
        "capability_not_supported",
        "channel_not_found",
        "channel_disabled",
        "model_not_found",
        "channel_not_allowed",
        "model_not_allowed",
        "invalid_api_key_scope",
        "account_unavailable",
    }
    if value in stable:
        return value
    if status_code == 404:
        return "model_not_found"
    if status_code == 403:
        return "model_not_allowed"
    if status_code == 503:
        return "channel_unavailable"
    return "invalid_request_error" if status_code < 500 else "adapter_error"


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
    if type(prompt) is not int or prompt < 0 or type(completion) is not int or completion < 0:
        return None
    return prompt, completion


def _response_text(value: Any) -> str:
    if isinstance(value, str):
        return value
    if isinstance(value, list):
        return "".join(_response_text(item) for item in value)
    if not isinstance(value, Mapping):
        return ""
    choices = value.get("choices")
    if isinstance(choices, list):
        text = "".join(_response_text(item) for item in choices)
        if text:
            return text
    for key in ("message", "delta", "content", "text", "output_text"):
        if key in value:
            text = _response_text(value[key])
            if text:
                return text
    return ""


def _extract_sse_usage(
    buffer: bytes,
) -> tuple[bytes, tuple[int, int] | None, str]:
    latest_usage = None
    text_parts: list[str] = []
    while True:
        boundaries = [
            (index, separator)
            for separator in (b"\r\n\r\n", b"\n\n")
            if (index := buffer.find(separator)) >= 0
        ]
        if not boundaries:
            return buffer, latest_usage, "".join(text_parts)
        index, separator = min(boundaries, key=lambda item: item[0])
        frame, buffer = buffer[:index], buffer[index + len(separator) :]
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
        if isinstance(payload, Mapping):
            text_parts.append(_response_text(payload))
        if len(buffer) > 1_048_576:
            buffer = b""


def _sse_to_chat_response(body: bytes, model: str) -> dict[str, Any]:
    """Aggregate a provider SSE body for a non-streaming OpenAI request."""

    choices: dict[int, dict[str, Any]] = {}
    response_id = ""
    created = int(time.time())
    response_model = model
    usage: dict[str, Any] | None = None
    saw_chunk = False
    normalized = body.replace(b"\r\n", b"\n")
    for frame in normalized.split(b"\n\n"):
        data_lines = [line[5:].lstrip() for line in frame.split(b"\n") if line.startswith(b"data:")]
        if not data_lines:
            continue
        raw = b"\n".join(data_lines)
        if raw.strip() == b"[DONE]":
            continue
        try:
            payload = json.loads(raw)
        except (TypeError, ValueError, UnicodeDecodeError) as exc:
            raise ValueError("upstream returned an invalid SSE frame") from exc
        if not isinstance(payload, dict):
            continue
        if isinstance(payload.get("error"), dict):
            raise ValueError(str(payload["error"].get("message") or "upstream returned an error"))
        saw_chunk = True
        response_id = str(payload.get("id") or response_id)
        response_model = str(payload.get("model") or response_model)
        if isinstance(payload.get("created"), int):
            created = payload["created"]
        if isinstance(payload.get("usage"), dict):
            usage = payload["usage"]
        for raw_choice in payload.get("choices", []):
            if not isinstance(raw_choice, dict):
                continue
            index = raw_choice.get("index", 0)
            if type(index) is not int or index < 0:
                index = 0
            choice = choices.setdefault(
                index,
                {
                    "index": index,
                    "message": {"role": "assistant", "content": ""},
                    "finish_reason": None,
                },
            )
            delta = raw_choice.get("delta")
            if isinstance(delta, dict):
                role = delta.get("role")
                if isinstance(role, str) and role:
                    choice["message"]["role"] = role
                content = delta.get("content")
                if isinstance(content, str):
                    choice["message"]["content"] += content
                reasoning = delta.get("reasoning_content")
                if isinstance(reasoning, str):
                    choice["message"]["reasoning_content"] = (
                        choice["message"].get("reasoning_content", "") + reasoning
                    )
            finish_reason = raw_choice.get("finish_reason")
            if finish_reason is not None:
                choice["finish_reason"] = finish_reason

    if not saw_chunk or not choices:
        raise ValueError("upstream returned no chat completion choices")
    return {
        "id": response_id or f"chatcmpl-{int(time.time() * 1000)}",
        "object": "chat.completion",
        "created": created,
        "model": response_model,
        "choices": [choices[index] for index in sorted(choices)],
        **({"usage": usage} if usage is not None else {}),
    }


@router.get("/models", tags=["gateway"], response_model=None)
async def list_models(key: KeyContext) -> dict:
    adapters = get_registry()
    db_path = get_settings().db_path
    configured = [
        adapter for adapter in adapters.values() if adapter_catalogue_enabled(adapter, db_path)
    ]
    results = await asyncio.gather(
        *(
            asyncio.wait_for(
                fetch_adapter_models(adapter),
                timeout=PUBLIC_MODEL_DISCOVERY_TIMEOUT_SECONDS,
            )
            for adapter in configured
        ),
        return_exceptions=True,
    )
    unavailable_channels: set[str] = set()
    models: list[dict[str, Any]] = []
    model_ids: set[str] = set()
    now = int(time.time())
    for adapter, result in zip(configured, results, strict=True):
        if isinstance(result, Exception):
            unavailable_channels.add(adapter.slug)
            continue
        items = [dict(item) for item in result if isinstance(item, Mapping) and item.get("id")]
        if not items:
            unavailable_channels.add(adapter.slug)
            continue
        upsert_model_cache(adapter.slug, items, db_path=db_path)
        with database(db_path) as conn:
            enabled_ids = {
                row["id"]
                for row in conn.execute(
                    "SELECT id FROM models WHERE channel = ? AND enabled = 1",
                    (adapter.slug,),
                )
            }
        for item in items:
            upstream_id = item.get("id")
            if not isinstance(upstream_id, str) or not upstream_id:
                continue
            model_id = f"{adapter.slug}/{upstream_id}"
            if model_id in model_ids or model_id not in enabled_ids:
                continue
            if not _allowed(key, adapter.slug, model_id):
                continue
            display_name = str(item.get("name") or item.get("display_name") or upstream_id)
            models.append(
                {
                    "id": model_id,
                    "object": "model",
                    "created": now,
                    "owned_by": adapter.slug,
                    "name": display_name,
                    "kind": str(item.get("kind") or "chat"),
                    "caps": list(item.get("caps") or item.get("capabilities") or ["chat"]),
                }
            )
            model_ids.add(model_id)

    if unavailable_channels:
        with database(db_path) as conn:
            cached_rows = conn.execute(
                """SELECT id, channel, upstream_id, display_name, kind, caps
                FROM models WHERE enabled = 1 ORDER BY channel, display_name, id"""
            ).fetchall()
        for row in cached_rows:
            channel = str(row["channel"])
            model_id = str(row["id"])
            if channel not in unavailable_channels or model_id in model_ids:
                continue
            if not _allowed(key, channel, model_id):
                continue
            models.append(
                {
                    "id": model_id,
                    "object": "model",
                    "created": now,
                    "owned_by": channel,
                    "name": str(row["display_name"] or row["upstream_id"]),
                    "kind": str(row["kind"] or "chat"),
                    "caps": json.loads(row["caps"] or "[]"),
                }
            )
            model_ids.add(model_id)

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
    if configured and len(unavailable_channels) == len(configured) and not models:
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


async def _capability_request(
    request: Request,
    key: dict[str, Any],
    capability: str,
) -> Response:
    request_id = request.state.request_id
    try:
        payload = await request.json()
    except Exception:
        return _capability_error(
            request_id,
            400,
            "invalid_request_error",
            "request body must be valid JSON",
        )
    try:
        return await _dispatch_chat(request, key, payload, capability=capability)
    except HTTPException as exc:
        headers = dict(exc.headers or {})
        return _capability_error(
            request_id,
            exc.status_code,
            _capability_error_code(exc.detail, exc.status_code),
            str(exc.detail),
            headers,
        )


@router.post("/images/generations", tags=["gateway"])
async def image_generations(request: Request, key: KeyContext) -> Response:
    return await _capability_request(request, key, "image")


@router.post("/video/generations", tags=["gateway"])
async def video_generations(request: Request, key: KeyContext) -> Response:
    return await _capability_request(request, key, "video")


@router.post("/audio/generations", tags=["gateway"])
async def audio_generations(request: Request, key: KeyContext) -> Response:
    return await _capability_request(request, key, "audio")


@router.post("/search", tags=["gateway"])
async def search(request: Request, key: KeyContext) -> Response:
    return await _capability_request(request, key, "search")


async def _unsupported_capability(request: Request, capability: str) -> Response:
    return _capability_error(
        request.state.request_id,
        404,
        "capability_not_supported",
        f"capability {capability!r} is not implemented by the native data plane",
    )


@router.post("/images/edits", tags=["gateway"])
async def image_edits(request: Request, key: KeyContext) -> Response:
    return await _unsupported_capability(request, "image_edits")


@router.post("/files", tags=["gateway"])
async def file_upload(request: Request, key: KeyContext) -> Response:
    return await _unsupported_capability(request, "file")


@router.get("/files/download", tags=["gateway"])
async def file_download(request: Request, key: KeyContext) -> Response:
    return await _unsupported_capability(request, "file")


@router.post("/ppt/generations", tags=["gateway"])
async def ppt_generations(request: Request, key: KeyContext) -> Response:
    return await _unsupported_capability(request, "ppt")


@router.post("/psd/generations", tags=["gateway"])
async def psd_generations(request: Request, key: KeyContext) -> Response:
    return await _unsupported_capability(request, "psd")


@router.get("/editable-file-tasks", tags=["gateway"])
async def editable_file_tasks(request: Request, key: KeyContext) -> Response:
    return await _unsupported_capability(request, "editable_file_tasks")


@router.post("/messages/count_tokens", tags=["gateway"])
async def count_tokens(request: Request, key: KeyContext) -> Response:
    return await _unsupported_capability(request, "count_tokens")


async def _dispatch_chat(
    request: Request,
    key: dict,
    payload: Any,
    *,
    capability: str = "chat",
) -> Response:
    capability = _capability_name(capability)
    if not isinstance(payload, dict):
        raise HTTPException(status_code=422, detail="request body must be an object")
    model_value = payload.get("model")
    if not isinstance(model_value, str) or not model_value.strip():
        raise HTTPException(status_code=422, detail="model is required")
    model_id = model_value.strip()
    if capability != "chat" and payload.get("stream") is True:
        raise HTTPException(
            status_code=400, detail="streaming is not supported for this capability"
        )
    adapters = get_registry()
    if capability == "chat":
        route_alias, targets = _resolve_targets(model_id, key, adapters)
    else:
        route_alias, targets = _resolve_capability_targets(model_id, key, adapters, capability)

    def log_request(**values: Any) -> None:
        values["model"] = model_id
        values["route_alias"] = route_alias
        values.setdefault("upstream_model", targets[0]["upstream_model"])
        _log_request(**values)

    request_id = request.state.request_id
    started = time.monotonic()
    trace_secret = get_settings().wb_trace_secret.get_secret_value()
    trace_adapters = [
        adapters[target["channel"]]
        for target in targets
        if target["channel"] in adapters
        and callable(getattr(adapters[target["channel"]], "request_headers_resolver", None))
    ]
    if (
        trace_adapters
        and trace_secret
        and len(trace_secret.encode("utf-8")) < 32
    ):
        trace_adapter = trace_adapters[0]
        log_request(
            request_id=request_id,
            key_id=key["id"],
            channel=trace_adapter.slug,
            status=503,
            started=started,
            error=f"invalid {trace_adapter.name} trace secret configuration",
            stream=payload.get("stream") is True,
        )
        raise HTTPException(
            status_code=503,
            detail="A2A_WB_TRACE_SECRET must contain at least 32 bytes",
        )
    expanded_targets: list[dict[str, str]] = []
    for route_depth, target in enumerate(targets):
        target_channel = target["channel"]
        adapter = adapters.get(target_channel)
        base_target = {
            **target,
            "route_depth": str(route_depth),
            "verify_account_selection": str(
                adapter is not None and getattr(adapter, "runtime", None) is None
            ).lower(),
        }
        if adapter is None:
            expanded_targets.append(base_target)
            continue
        candidate_reader = getattr(adapter, "candidate_reader", None)
        if callable(candidate_reader):
            has_snapshot, candidates = candidate_reader(
                target["upstream_model"], get_settings().db_path
            )
        elif getattr(adapter, "runtime", None) is not None:
            has_snapshot, candidates = account_candidates(
                target_channel,
                target["upstream_model"],
                db_path=get_settings().db_path,
                require_credentials=True,
            )
        else:
            has_snapshot, candidates = False, []
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
            error="no eligible local accounts in the account snapshot",
            error_kind="account_unavailable",
            stream=payload.get("stream") is True,
        )
        raise HTTPException(
            status_code=503,
            detail="no eligible local accounts are available",
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
        header_resolver = getattr(adapter, "request_headers_resolver", None)
        if callable(header_resolver):
            headers.update(header_resolver(target, trace_secret, request_id))
        if request.headers.get("accept"):
            headers["Accept"] = request.headers["accept"]
        path = CAPABILITY_PATHS[capability]
        url = f"{adapter.base_url.rstrip('/')}{path}"
        return adapter, body, headers, url

    if payload.get("stream") is True:
        selected: (
            tuple[int, httpx.AsyncClient, httpx.Response, AdapterSpec, dict[str, str], Any] | None
        ) = None
        last_status = 502
        for attempt_index, target in enumerate(targets):
            depth = int(target["route_depth"])
            account_lease = None
            if target.get("lease_account_id"):
                candidate = AccountCandidate(
                    target["lease_account_id"],
                    target["account_id"],
                    target.get("channel", ""),
                )
                account_lease = acquire_account_lease(candidate)
                if account_lease is None:
                    last_status = 503
                    continue
            adapter, body, headers, url = prepare_attempt(target)
            runtime_adapter = getattr(adapter, "runtime", None)
            client = httpx.AsyncClient(timeout=httpx.Timeout(300, connect=10))
            try:
                if runtime_adapter is not None:
                    if capability != "chat":
                        raise ValueError(f"streaming is not supported for capability {capability}")
                    upstream_handle = await runtime_adapter.open_stream(
                        {
                            "model": target["upstream_model"],
                            "payload": json.loads(body),
                            "headers": headers,
                            "stream": True,
                        },
                        target,
                    )
                    await client.aclose()
                    client = upstream_handle.client
                    upstream = upstream_handle.response
                else:
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
                mapped: Mapping[str, Any] = {}
                mapper = getattr(runtime_adapter, "map_error", None)
                if callable(mapper):
                    try:
                        value = mapper(exc)
                        if isinstance(value, Mapping):
                            mapped = value
                    except Exception:
                        mapped = {}
                try:
                    mapped_status = int(mapped.get("status_code") or 502)
                except (TypeError, ValueError):
                    mapped_status = 502
                if mapped_status < 400 or mapped_status > 599:
                    mapped_status = 502
                mapped_message = str(mapped.get("message") or "upstream request failed")
                mapped_code = str(mapped.get("code") or "upstream_error")
                last_status = mapped_status
                if isinstance(exc, httpx.HTTPError):
                    record_transient_failure(adapter.slug, mapped_status, "transport_error")
                if attempt_index + 1 < len(targets):
                    continue
                _upstream_slots.release()
                log_request(
                    request_id=request_id,
                    key_id=key["id"],
                    channel=adapter.slug,
                    upstream_model=target["upstream_model"],
                    status=mapped_status,
                    started=started,
                    error=mapped_message,
                    error_kind=mapped_code,
                    stream=True,
                    fallback_depth=depth,
                )
                raise HTTPException(
                    status_code=mapped_status,
                    detail=mapped_message,
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
                error_account_id = _selected_account_id(request_id, upstream.headers, target)
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
        account_id = _selected_account_id(request_id, upstream.headers, target)
        prompt_tokens = completion_tokens = 0
        usage_reported = False
        usage_kind = "unknown"
        completion_text = ""

        async def chunks():
            nonlocal prompt_tokens, completion_tokens, usage_reported, usage_kind, completion_text
            status = upstream.status_code
            stream_error = "upstream request failed" if status >= 400 else None
            stream_error_kind = None
            completed = False
            pending = b""
            try:
                async for chunk in upstream.aiter_bytes():
                    pending += chunk
                    pending, usage, text = _extract_sse_usage(pending)
                    completion_text += text
                    if usage is not None:
                        prompt_tokens, completion_tokens = usage
                        usage_reported = True
                        usage_kind = "reported"
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
                    record_account_transient_failure(account_id, status, "stream_interrupted")
                yield _stream_error_frame(stream_error)
            except Exception:
                status = 502
                stream_error = "stream interrupted"
                stream_error_kind = "stream_interrupted"
                record_transient_failure(adapter.slug, status, "stream_interrupted")
                if account_id is not None:
                    record_account_transient_failure(account_id, status, "stream_interrupted")
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
                        if status < 400 and not usage_reported:
                            prompt_tokens, completion_tokens = estimate_usage(
                                payload.get("messages"),
                                completion_text,
                                target["upstream_model"],
                            )
                            usage_kind = "estimated"
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
                            usage_kind=usage_kind,
                            fallback_depth=fallback_depth,
                        )

        return StreamingResponse(
            chunks(),
            status_code=upstream.status_code,
            background=(
                BackgroundTask(account_lease.release) if account_lease is not None else None
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
            candidate = AccountCandidate(
                target["lease_account_id"],
                target["account_id"],
                target.get("channel", ""),
            )
            account_lease = acquire_account_lease(candidate)
            if account_lease is None:
                last_status = 503
                continue
        adapter, body, headers, url = prepare_attempt(target)
        try:
            runtime_adapter = getattr(adapter, "runtime", None)
            if runtime_adapter is not None:
                invoke_capability = getattr(runtime_adapter, "invoke_capability", None)
                if capability != "chat" and not callable(invoke_capability):
                    raise ValueError(f"native adapter does not implement capability {capability}")
                if capability == "chat":
                    upstream = await runtime_adapter.invoke(
                        {
                            "model": target["upstream_model"],
                            "payload": json.loads(body),
                            "headers": headers,
                        },
                        target,
                    )
                else:
                    upstream = await invoke_capability(
                        capability,
                        {
                            "model": target["upstream_model"],
                            "payload": json.loads(body),
                            "headers": headers,
                        },
                        target,
                    )
            else:
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
            mapped: Mapping[str, Any] = {}
            mapper = getattr(runtime_adapter, "map_error", None)
            if callable(mapper):
                try:
                    value = mapper(exc)
                    if isinstance(value, Mapping):
                        mapped = value
                except Exception:
                    mapped = {}
            try:
                mapped_status = int(mapped.get("status_code") or 502)
            except (TypeError, ValueError):
                mapped_status = 502
            if mapped_status < 400 or mapped_status > 599:
                mapped_status = 502
            mapped_message = str(mapped.get("message") or "upstream request failed")
            mapped_code = str(mapped.get("code") or "upstream_error")
            last_status = mapped_status
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
                status=mapped_status,
                started=started,
                error=mapped_message,
                error_kind=mapped_code,
                fallback_depth=depth,
            )
            raise HTTPException(status_code=mapped_status, detail=mapped_message) from exc
        if _account_selection_mismatch(request_id, upstream, target):
            await upstream.aclose()
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
        response_account_id = _selected_account_id(request_id, upstream.headers, target)
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
            await upstream.aclose()
            last_status = 503 if account_unavailable else upstream.status_code
            fallback_depth = depth
            if account_lease is not None:
                account_lease.release()
            continue
        if account_lease is not None:
            account_lease.release()
        if account_unavailable:
            await upstream.aclose()
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
    account_id = _selected_account_id(request_id, upstream.headers, selected_target)
    prompt_tokens = completion_tokens = 0
    usage_reported = False
    usage_kind = "unknown"
    content_type = upstream.headers.get("content-type", "application/json")
    try:
        if "text/event-stream" in content_type.lower():
            result = _sse_to_chat_response(upstream.content, selected_target["upstream_model"])
            content_type = "application/json"
        else:
            result = upstream.json()
        usage = result.get("usage", {}) if isinstance(result, dict) else {}
        token_usage = _token_usage(usage)
        if token_usage is not None:
            prompt_tokens, completion_tokens = token_usage
            usage_reported = True
            usage_kind = "reported"
        elif upstream.status_code < 400:
            prompt_tokens, completion_tokens = estimate_usage(
                payload.get("messages"),
                _response_text(result),
                selected_target["upstream_model"],
            )
            usage_kind = "estimated"
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
        started=started,
        error="upstream request failed" if upstream.status_code >= 400 else None,
        error_kind=_error_kind_for_status(upstream.status_code),
        prompt_tokens=prompt_tokens,
        completion_tokens=completion_tokens,
        usage_reported=usage_reported,
        usage_kind=usage_kind,
        account_id=account_id,
        fallback_depth=fallback_depth,
    )
    await upstream.aclose()
    return Response(
        content=response_body,
        status_code=upstream.status_code,
        media_type=content_type,
        headers=_client_response_headers(request_id, upstream),
    )
