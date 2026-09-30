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

_IMPORT_CREDENTIAL_FIELDS = {
    "access_token",
    "accessToken",
    "refresh_token",
    "refreshToken",
    "id_token",
    "idToken",
    "token",
    "oauth_token",
    "client_id",
    "chatgpt_account_id",
    "chatgpt_user_id",
    "email",
    "expires_at",
    "expiresAt",
    "organization_id",
    "plan_type",
    "proxy",
    "user_agent",
    "oai_device_id",
    "oai_session_id",
    "impersonate",
    "sec-ch-ua",
    "sec-ch-ua-mobile",
    "sec-ch-ua-platform",
    "fp",
    "sentinel_p",
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


def _merged_import_payload(item: Mapping[str, Any]) -> tuple[dict[str, Any], Mapping[str, Any]]:
    """Flatten the nested account shape used by sub2api exports.

    Export metadata remains available for the canonical account view, while
    the nested ``credentials`` object remains the only source for credential
    material written to the encrypted store.
    """

    extra = item.get("extra")
    credentials = item.get("credentials")
    merged: dict[str, Any] = {}
    if isinstance(extra, Mapping):
        merged.update(extra)
    merged.update(item)
    if isinstance(credentials, Mapping):
        merged.update(credentials)
        return merged, merged
    return merged, item


def token_record(item: Any) -> dict[str, Any]:
    """Normalize a token string or full OAuth token object.

    The returned ``credentials`` member is private and is only passed to the
    CredentialStore.  Callers should use :func:`canonical_account` for API DTOs.
    """

    if isinstance(item, str):
        payload: Mapping[str, Any] = {"access_token": item}
        credential_payload: Mapping[str, Any] = payload
    elif isinstance(item, Mapping):
        payload, credential_payload = _merged_import_payload(item)
    else:
        raise ValueError("token item must be a string or object")
    access = _first(payload, "access_token", "accessToken", "token")
    refresh = _first(payload, "refresh_token", "refreshToken")
    identity = _first(payload, "id_token", "idToken")
    if not access:
        raise ValueError("access token is required")
    credentials = {
        str(key): value
        for key, value in credential_payload.items()
        if str(key) in _IMPORT_CREDENTIAL_FIELDS and value is not None
    }
    credentials["access_token"] = access
    if refresh:
        credentials["refresh_token"] = refresh
    if identity:
        credentials["id_token"] = identity
    credentials["auth_mode"] = (
        "codex"
        if any(
            credentials.get(key)
            for key in ("client_id", "organization_id", "id_token", "chatgpt_account_id")
        )
        else "web"
    )
    return {
        "fingerprint": token_fingerprint(access),
        "credential_ref": credential_reference(access),
        "credentials": credentials,
        "metadata": {
            "email": _first(payload, "email", "email_address", "mailbox_email"),
            "name": _first(payload, "name", "nickname", "display_name"),
            "plan": _first(payload, "plan", "plan_type", "type"),
            "source_type": _first(payload, "source_type", "source") or "oauth",
            "expires_at": payload.get("expires_at") or payload.get("expiresAt"),
            "chatgpt_account_id": _first(payload, "chatgpt_account_id"),
            "chatgpt_user_id": _first(payload, "chatgpt_user_id", "user_id"),
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
            **{
                key: metadata[key]
                for key in ("chatgpt_account_id", "chatgpt_user_id")
                if metadata.get(key)
            },
        },
    }


def sanitize_metadata(payload: Mapping[str, Any]) -> dict[str, Any]:
    """Drop secret-shaped fields before persisting metadata or returning it."""

    return {
        str(key): value
        for key, value in payload.items()
        if str(key) not in _SECRET_FIELDS
    }
