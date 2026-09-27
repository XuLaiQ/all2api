from __future__ import annotations

import hashlib
from collections.abc import Mapping
from typing import Any

import httpx

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
    """Fetch the compatibility upstream's advertised models through its authenticated data API."""
    async with httpx.AsyncClient(timeout=_TIMEOUT) as client:
        response = await client.get(_url(base_url, "/v1/models"), headers=_headers(auth_key))
        response.raise_for_status()
        payload = response.json()
    return [item for item in _items(payload, "data") if isinstance(item.get("id"), str)]


async def list_accounts(base_url: str, auth_key: str) -> list[dict[str, Any]]:
    """Fetch and normalize accounts from the compatibility upstream's admin API.

    OAuth tokens are deliberately excluded from the normalized record. The upstream
    uses access_token as its dictionary key, so a one-way digest provides a stable ID.
    """
    async with httpx.AsyncClient(timeout=_TIMEOUT) as client:
        response = await client.get(_url(base_url, "/api/accounts"), headers=_headers(auth_key))
        response.raise_for_status()
        payload = response.json()

    raw_items = _items(payload, "items", "accounts")
    result: list[dict[str, Any]] = []
    for item in raw_items:
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
        name = str(item.get("email") or item.get("name") or user_id or native_id)
        ext = {key: item[key] for key in _ACCOUNT_EXT_FIELDS if key in item}
        if quota is not None:
            ext["quota_remaining"] = quota

        result.append(
            {
                "id": f"chatgpt:{native_id}",
                "channel": "chatgpt",
                "native_id": native_id,
                "name": name,
                "kind": "oauth",
                "tier": item.get("type"),
                "status": status,
                "enabled": status != "disabled",
                # The upstream account endpoint exposes remaining image quota, but
                # not the corresponding image_gen.limit, so total usage is unknown.
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
