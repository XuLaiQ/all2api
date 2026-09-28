"""ChatGPT model/account reader and adapter shell.

Transport used by the gateway is intentionally kept small here.  Account
onboarding lives in :mod:`provisioner` and never calls a legacy management API.
"""

from __future__ import annotations

import hashlib
from collections.abc import Mapping
from typing import Any

import httpx

from app.adapters.native_runtime import NativeHttpAdapter

from .manifest import CHATGPT_MANIFEST

_TIMEOUT = httpx.Timeout(15, connect=3)
_ACCOUNT_EXT_FIELDS = {
    "default_model_slug",
    "image_inflight",
    "last_invalid_at",
    "last_token_refresh_at",
    "last_token_refresh_error",
    "last_token_refresh_error_at",
    "limits_progress",
    "proxy",
    "restore_at",
    "source_type",
}


def _url(base_url: str, path: str) -> str:
    root = base_url.strip().rstrip("/")
    if root.endswith("/v1") and path.startswith("/v1/"):
        root = root[:-3]
    return f"{root}{path}"


def _headers(auth_key: str) -> dict[str, str]:
    key = auth_key.strip()
    return {"Authorization": f"Bearer {key}"} if key else {}


def _items(payload: Any, *keys: str) -> list[dict[str, Any]]:
    if isinstance(payload, list):
        values = payload
    elif isinstance(payload, Mapping):
        values = next((payload.get(key) for key in keys if isinstance(payload.get(key), list)), [])
    else:
        return []
    return [item for item in values if isinstance(item, dict)]


async def list_models(base_url: str, auth_key: str) -> list[dict[str, Any]]:
    async with httpx.AsyncClient(timeout=_TIMEOUT) as client:
        response = await client.get(_url(base_url, "/v1/models"), headers=_headers(auth_key))
        response.raise_for_status()
        payload = response.json()
    return [item for item in _items(payload, "data") if isinstance(item.get("id"), str)]


async def list_accounts(base_url: str, auth_key: str) -> list[dict[str, Any]]:
    """Read canonical account metadata from the configured model service.

    This reader is retained for the compatibility sync endpoint.  The new
    provisioner stores credentials locally and does not call this endpoint.
    """

    async with httpx.AsyncClient(timeout=_TIMEOUT) as client:
        response = await client.get(_url(base_url, "/api/accounts"), headers=_headers(auth_key))
        response.raise_for_status()
        payload = response.json()

    result: list[dict[str, Any]] = []
    for item in _items(payload, "items", "accounts"):
        access_token = str(item.get("access_token") or item.get("accessToken") or "").strip()
        user_id = str(item.get("user_id") or "").strip()
        if not access_token and not user_id:
            continue
        native_id = (
            f"user:{user_id}"
            if user_id
            else f"token:{hashlib.sha256(access_token.encode('utf-8')).hexdigest()[:20]}"
        )
        status = _status(item)
        quota = _nonnegative_int(item.get("quota"))
        ext = {key: item[key] for key in _ACCOUNT_EXT_FIELDS if key in item}
        if quota is not None:
            ext["quota_remaining"] = quota
        result.append(
            {
                "id": f"chatgpt:{native_id}",
                "channel": "chatgpt",
                "native_id": native_id,
                "name": str(item.get("email") or item.get("name") or user_id or native_id),
                "kind": "oauth",
                "tier": item.get("type"),
                "status": status,
                "enabled": status != "disabled",
                "quota_used": 0,
                "quota_total": 0,
                "quota_unit": "none",
                "expires_at": item.get("expires_at"),
                "priority": 0,
                "success_count": _nonnegative_int(item.get("success")) or 0,
                "fail_count": _nonnegative_int(item.get("fail")) or 0,
                "streak": _nonnegative_int(item.get("invalid_count")) or 0,
                "cooldown_until": None,
                "last_error": item.get("last_refresh_error"),
                "ext": ext,
            }
        )
    return result


def _nonnegative_int(value: Any) -> int | None:
    try:
        return max(0, int(value)) if value is not None else None
    except (TypeError, ValueError, OverflowError):
        return None


def _status(item: Mapping[str, Any]) -> str:
    status = str(item.get("status") or "").strip()
    if status == "禁用":
        return "disabled"
    if status == "异常":
        return "needLogin" if item.get("last_refresh_error") else "error"
    if status == "限流" or _nonnegative_int(item.get("quota")) == 0:
        return "limited"
    if status == "正常":
        return "busy" if _nonnegative_int(item.get("image_inflight")) else "ready"
    return "error"


class ChatGPTAdapter:
    manifest = CHATGPT_MANIFEST

    def __init__(
        self,
        base_url: str = "",
        *,
        auth_key: str = "",
        http_client: httpx.AsyncClient | Any | None = None,
    ) -> None:
        self.base_url = str(base_url or "").rstrip("/")
        self.auth_key = str(auth_key or "")
        self._runtime = NativeHttpAdapter(
            self.manifest,
            self.base_url,
            auth_key=self.auth_key,
            http_client=http_client,
        )

    async def health(self, context: Any = None) -> Mapping[str, Any]:
        if isinstance(context, Mapping) and context.get("base_url"):
            runtime = NativeHttpAdapter(
                self.manifest,
                str(context.get("base_url")),
                auth_key=str(context.get("auth_key") or ""),
            )
            return await runtime.health(context)
        return await self._runtime.health(context)

    async def list_models(self, context: Any) -> list[dict[str, Any]]:
        return await list_models(str(context.get("base_url", "")), str(context.get("auth_key", "")))

    async def list_accounts(self, context: Any) -> list[dict[str, Any]]:
        return await list_accounts(
            str(context.get("base_url", "")), str(context.get("auth_key", ""))
        )

    async def invoke(self, request: Any, account: Any = None) -> Any:
        return await self._runtime.invoke(request, account)

    async def invoke_capability(self, capability: str, request: Any, account: Any = None) -> Any:
        return await self._runtime.invoke_capability(capability, request, account)

    async def invoke_stream(self, request: Any, account: Any = None):
        async for chunk in self._runtime.invoke_stream(request, account):
            yield chunk

    async def chat(self, request: Any, account: Any = None) -> Any:
        return await self.invoke(request, account)

    async def chat_stream(self, request: Any, account: Any = None):
        async for chunk in self.invoke_stream(request, account):
            yield chunk

    def map_error(self, error: Exception) -> Mapping[str, Any]:
        if isinstance(error, httpx.TimeoutException):
            return {
                "code": "upstream_timeout",
                "message": "upstream request timed out",
                "retryable": True,
            }
        if isinstance(error, httpx.HTTPError):
            return {
                "code": "upstream_unavailable",
                "message": "upstream request failed",
                "retryable": True,
            }
        return {"code": "internal", "message": "upstream adapter failed", "retryable": False}
