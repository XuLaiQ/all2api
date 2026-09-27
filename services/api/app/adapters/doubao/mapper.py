"""Mapping from internal Doubao records to safe canonical DTOs."""

from __future__ import annotations

from collections.abc import Mapping
from typing import Any

_SAFE_EVENT_KEYS = {
    "status",
    "message",
    "error_code",
    "qr_code",
    "qr_image_base64",
    "qr_image",
    "account_id",
    "session_id",
}


def _normalize_qr_image(value: Any) -> str | None:
    """Normalize worker QR image values to raw base64 for the web client.

    Browser implementations in the wild sometimes return a ``data:`` URI or
    use the legacy ``qr_image`` key.  The public DTO always carries the raw
    base64 payload under ``qr_image_base64`` so the UI can add the MIME prefix
    exactly once.
    """

    if value is None:
        return None
    if isinstance(value, bytes):
        import base64

        return base64.b64encode(value).decode("ascii")
    text = str(value).strip()
    if not text:
        return None
    if text.startswith("data:") and "," in text:
        return text.split(",", 1)[1]
    return text


def map_profile(profile: Any) -> dict[str, Any]:
    """Return a canonical account/profile payload with no session material."""

    if isinstance(profile, Mapping):
        get = profile.get
    else:
        def get(key: str, default: Any = None) -> Any:
            return getattr(profile, key, default)
    account_id = str(get("account_id", get("id", "")) or "")
    name = str(get("name", account_id) or account_id)
    return {
        "id": f"doubao:{account_id}",
        "channel": "doubao",
        "native_id": account_id,
        "name": name,
        "kind": "account",
        "status": str(get("status", "needLogin") or "needLogin"),
        "enabled": bool(get("enabled", True)),
        "priority": int(get("priority", 0) or 0),
        "quota_used": 0,
        "quota_total": 0,
        "quota_unit": "none",
        "expires_at": None,
        # Filesystem paths are internal worker details and must not be sent to
        # the management API.
        "ext": {},
    }


def map_account(profile: Any) -> dict[str, Any]:
    """Compatibility alias used by adapter callers."""

    return map_profile(profile)


def map_browser_event(event: Any, *, credential_ref: str | None = None) -> dict[str, Any]:
    """Map a worker event while dropping cookies/tokens/profile contents."""

    if isinstance(event, Mapping):
        values = {key: event.get(key) for key in _SAFE_EVENT_KEYS if key in event}
    else:
        values = {key: getattr(event, key) for key in _SAFE_EVENT_KEYS if hasattr(event, key)}
    result = {
        "status": str(values.get("status") or "waiting_scan"),
        "message": str(values.get("message") or ""),
    }
    for key in (
        "session_id",
        "account_id",
        "error_code",
        "qr_code",
    ):
        value = values.get(key)
        if value:
            result[key] = str(value)
    qr_image = values.get("qr_image_base64") or values.get("qr_image")
    normalized_image = _normalize_qr_image(qr_image)
    if normalized_image:
        result["qr_image_base64"] = normalized_image
    if credential_ref:
        result["credential_ref"] = credential_ref
    return result


__all__ = ["map_account", "map_browser_event", "map_profile"]
