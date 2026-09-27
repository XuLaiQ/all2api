"""Map ChatGPT token/account payloads to canonical, secret-free DTOs."""

from __future__ import annotations

import hashlib
from collections.abc import Mapping
from typing import Any

_SECRET_FIELDS = {
    "access_token",
    "accessToken",
    "refresh_token",
    "refreshToken",
    "id_token",
    "idToken",
    "token",
    "oauth_token",
    "client_secret",
}


def token_fingerprint(token: str) -> str:
    """Return a stable, non-reversible identifier for a token."""

    return hashlib.sha256(token.encode("utf-8")).hexdigest()[:24]


def credential_reference(token: str) -> str:
    return f"chatgpt:{token_fingerprint(token)}"


def redact_secret(value: Any) -> str:
    """Never return a complete secret; retain only a tiny diagnostic prefix."""

    text = str(value or "")
    if not text:
        return ""
    return f"{text[:3]}…" if len(text) > 3 else "***"


def _first(payload: Mapping[str, Any], *names: str) -> str:
    for name in names:
        value = payload.get(name)
        if isinstance(value, str) and value.strip():
            return value.strip()
    return ""


def token_record(item: Any) -> dict[str, Any]:
    """Normalize a token string or full OAuth token object.

    The returned ``credentials`` member is private and is only passed to the
    CredentialStore.  Callers should use :func:`canonical_account` for API DTOs.
    """

    if isinstance(item, str):
        payload: Mapping[str, Any] = {"access_token": item}
    elif isinstance(item, Mapping):
        payload = item
    else:
        raise ValueError("token item must be a string or object")
    access = _first(payload, "access_token", "accessToken", "token")
    refresh = _first(payload, "refresh_token", "refreshToken")
    identity = _first(payload, "id_token", "idToken")
    if not access:
        raise ValueError("access token is required")
    credentials = {
        "access_token": access,
        "refresh_token": refresh,
        "id_token": identity,
    }
    return {
        "fingerprint": token_fingerprint(access),
        "credential_ref": credential_reference(access),
        "credentials": credentials,
        "metadata": {
            "email": _first(payload, "email", "email_address"),
            "name": _first(payload, "name", "nickname"),
            "plan": _first(payload, "plan", "plan_type", "type"),
            "source_type": _first(payload, "source_type") or "oauth",
            "expires_at": payload.get("expires_at") or payload.get("expiresAt"),
        },
    }


def canonical_account(record: Mapping[str, Any]) -> dict[str, Any]:
    metadata = record.get("metadata") if isinstance(record.get("metadata"), Mapping) else {}
    fingerprint = str(record.get("fingerprint") or "")
    native_id = f"token:{fingerprint}" if fingerprint else "token:unknown"
    name = str(metadata.get("email") or metadata.get("name") or native_id)
    return {
        "id": f"chatgpt:{native_id}",
        "channel": "chatgpt",
        "native_id": native_id,
        "name": name,
        "kind": "oauth",
        "tier": metadata.get("plan") or None,
        "status": "ready",
        "enabled": True,
        "quota_used": 0,
        "quota_total": 0,
        "quota_unit": "none",
        "expires_at": metadata.get("expires_at"),
        "priority": 0,
        "success_count": 0,
        "fail_count": 0,
        "streak": 0,
        "cooldown_until": None,
        "last_error": None,
        "ext": {
            "source_type": metadata.get("source_type") or "oauth",
            "credential_ref": str(record.get("credential_ref") or ""),
        },
    }


def sanitize_metadata(payload: Mapping[str, Any]) -> dict[str, Any]:
    """Drop secret-shaped fields before persisting metadata or returning it."""

    return {
        str(key): value
        for key, value in payload.items()
        if str(key) not in _SECRET_FIELDS
    }
