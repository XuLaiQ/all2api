from __future__ import annotations

from collections.abc import Mapping
from typing import Any

import httpx

_ADMIN_TIMEOUT = httpx.Timeout(15, connect=3)


def _headers(api_key: str) -> dict[str, str]:
    headers = {"Accept": "application/json"}
    if api_key:
        headers["Authorization"] = f"Bearer {api_key}"
    return headers


def _normalize_models(payload: Any) -> list[dict[str, Any]]:
    if not isinstance(payload, Mapping):
        return []
    catalogue = payload.get("models")
    if not isinstance(catalogue, Mapping):
        return []

    kind_caps = {
        "chat": ["chat"],
        "qianwen": ["chat"],
        "image": ["image"],
        "video": ["video"],
        "audio": ["audio"],
    }
    models: list[dict[str, Any]] = []
    seen: set[str] = set()
    for kind, ids in catalogue.items():
        if not isinstance(ids, list):
            continue
        for model_id in ids:
            if not isinstance(model_id, str) or not model_id or model_id in seen:
                continue
            seen.add(model_id)
            models.append(
                {
                    "id": model_id,
                    "object": "model",
                    "owned_by": "doubao" if kind != "qianwen" else "qianwen",
                    "kind": kind if kind in kind_caps else "chat",
                    "caps": kind_caps.get(kind, ["chat"]),
                }
            )
    return models


async def list_models(
    base_url: str,
    api_key: str,
) -> list[dict[str, Any]]:
    """Read the upstream model catalogue through its authenticated admin API."""
    async with httpx.AsyncClient(timeout=_ADMIN_TIMEOUT) as client:
        response = await client.get(
            f"{base_url.rstrip('/')}/admin/api/system",
            headers=_headers(api_key),
        )
        response.raise_for_status()
        return _normalize_models(response.json())


def _account_status(item: Mapping[str, Any]) -> str:
    status = str(item.get("status") or "unknown").lower()
    if item.get("needs_captcha") or status == "captcha":
        return "captcha"
    if status in {"need_login", "needlogin", "expired"}:
        return "needLogin"
    if status in {"cooldown", "cooling"}:
        return "cooldown"
    if status in {"busy", "ready", "error", "unknown"}:
        return status
    return "error"


def _normalize_accounts(payload: Any) -> list[dict[str, Any]]:
    raw_items = payload.get("accounts", []) if isinstance(payload, Mapping) else []
    if not isinstance(raw_items, list):
        return []

    # Include operational metadata only; browser cookies, tokens and profile contents
    # must never be copied into the unified account response.
    safe_ext_fields = {
        "browser_data",
        "max_concurrent",
        "active_requests",
        "active_tasks",
        "total_requests",
        "success_count",
        "failure_count",
        "consecutive_failures",
        "cooldown_until",
        "browser_initialized",
        "browser_ready",
        "needs_captcha",
        "last_error_code",
    }
    result: list[dict[str, Any]] = []
    for item in raw_items:
        if not isinstance(item, Mapping):
            continue
        native_id = str(item.get("id") or "").strip()
        if not native_id:
            continue
        enabled = bool(item.get("enabled", True))
        ext = {key: item[key] for key in safe_ext_fields if key in item}
        if "max_concurrent_per_account" in payload:
            ext["max_concurrent"] = payload["max_concurrent_per_account"]
        result.append(
            {
                "id": f"doubao:{native_id}",
                "channel": "doubao",
                "native_id": native_id,
                "name": str(item.get("name") or native_id),
                "kind": "account",
                "tier": None,
                "status": "disabled" if not enabled else _account_status(item),
                "enabled": enabled,
                "quota_used": 0,
                "quota_total": 0,
                "quota_unit": "none",
                "expires_at": None,
                "priority": 0,
                "ext": ext,
            }
        )
    return result


async def list_accounts(
    base_url: str,
    api_key: str,
) -> list[dict[str, Any]]:
    """Read and normalize account status via the upstream admin API."""
    async with httpx.AsyncClient(timeout=_ADMIN_TIMEOUT) as client:
        response = await client.get(
            f"{base_url.rstrip('/')}/admin/api/accounts",
            headers=_headers(api_key),
        )
        response.raise_for_status()
        return _normalize_accounts(response.json())
