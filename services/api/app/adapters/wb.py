from __future__ import annotations

from typing import Any

import httpx

from app.config import get_settings


def _base_url() -> str:
    return get_settings().wb_upstream_base.rstrip("/")


def _data_headers() -> dict[str, str]:
    key = get_settings().wb_data_key.get_secret_value()
    return {"Authorization": f"Bearer {key}"} if key else {}


def _admin_headers() -> dict[str, str]:
    token = get_settings().wb_admin_token.get_secret_value()
    return {"Authorization": f"Bearer {token}"} if token else {}


def _model_items(payload: Any) -> list[dict[str, Any]]:
    if isinstance(payload, list):
        items = payload
    elif isinstance(payload, dict) and isinstance(payload.get("data"), list):
        items = payload["data"]
    else:
        return []
    return [item for item in items if isinstance(item, dict) and isinstance(item.get("id"), str)]


async def list_models() -> list[dict[str, Any]]:
    async with httpx.AsyncClient(timeout=httpx.Timeout(15, connect=3)) as client:
        response = await client.get(f"{_base_url()}/v1/models", headers=_data_headers())
        response.raise_for_status()
        return _model_items(response.json())


async def list_accounts() -> list[dict[str, Any]]:
    async with httpx.AsyncClient(timeout=httpx.Timeout(15, connect=3)) as client:
        response = await client.get(f"{_base_url()}/api/accounts", headers=_admin_headers())
        response.raise_for_status()
        payload = response.json()
    raw_items = payload.get("accounts", []) if isinstance(payload, dict) else []
    if not isinstance(raw_items, list):
        return []
    safe_fields = {
        "file", "uid", "enterprise_id", "realm", "domain", "credits", "expires_at",
        "is_expired", "remain_seconds", "ttl_seconds", "issued_at", "source",
        "invalid_reason", "disabled_by_panel", "in_pool", "cooling",
        "cool_remaining_sec", "rate_limited_models", "disabled_reason",
        "success_count", "in_flight", "breaker_fails", "degrade_until",
        "consecutive_fails", "err_total", "last_err", "creditsExpiring",
        "credits_expiring",
    }
    result: list[dict[str, Any]] = []
    for index, item in enumerate(raw_items):
        if not isinstance(item, dict):
            continue
        uid = str(item.get("uid") or "").strip()
        realm = str(item.get("realm") or "cn").strip().lower()
        native_id = f"{realm}:{uid}" if uid else str(item.get("file") or index)
        ext = {key: value for key, value in item.items() if key in safe_fields}
        result.append(
            {
                "id": f"wb:{native_id}",
                "channel": "wb",
                "native_id": native_id,
                "name": str(item.get("nickname") or item.get("name") or uid or native_id),
                "kind": str(item.get("kind") or "account"),
                "tier": item.get("tier"),
                "status": _status(item),
                "enabled": not bool(item.get("disabled_by_panel") or item.get("disabled")),
                "quota_used": 0,
                "quota_total": float(item.get("credits") or 0),
                "quota_unit": "credits_remaining",
                "expires_at": item.get("expires_at"),
                "priority": int(item.get("priority") or 0),
                "ext": ext,
            }
        )
    return result


def _status(item: dict[str, Any]) -> str:
    if item.get("disabled_by_panel") or item.get("disabled"):
        return "disabled"
    if item.get("disabled_reason"):
        return "needLogin"
    if item.get("invalid_reason") or item.get("in_pool") is False:
        return "error"
    if item.get("cooling") or item.get("degrade_until"):
        return "cooldown"
    if item.get("rate_limited_models"):
        return "limited"
    if item.get("status") in {"needLogin", "expired", "error", "busy", "ready"}:
        return str(item["status"])
    return "ready"
