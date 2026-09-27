from __future__ import annotations

from collections.abc import Mapping
from typing import Any

from app.adapters.workbuddy.errors import WorkBuddyInvalidRequestError, WorkBuddyProtocolError

_SECRET_KEYS = {
    "access_token",
    "accessToken",
    "refresh_token",
    "refreshToken",
    "id_token",
    "idToken",
    "device_token",
    "deviceToken",
    "token",
    "cookie",
    "cookies",
    "authorization",
}


def normalize_realm(value: Any) -> str:
    realm = str(value or "cn").strip().lower()
    if realm not in {"cn", "global"}:
        raise WorkBuddyInvalidRequestError("realm must be either 'cn' or 'global'")
    return realm


def _first(payload: Mapping[str, Any], *keys: str, default: Any = "") -> Any:
    for key in keys:
        if key in payload and payload[key] not in (None, ""):
            return payload[key]
    return default


def credential_record(raw: Mapping[str, Any], *, realm: str | None = None) -> dict[str, Any]:
    """Build the private credential record written through CredentialStore.

    This function intentionally returns secrets only to the storage boundary; callers
    must pass its output directly to a credential store and never serialize it in an
    API response.
    """

    access = _first(raw, "access_token", "accessToken")
    if not isinstance(access, str) or not access:
        raise WorkBuddyProtocolError("successful login response did not contain an access token")
    return {
        "access_token": access,
        "refresh_token": str(_first(raw, "refresh_token", "refreshToken", default="")),
        "device_token": str(_first(raw, "device_token", "deviceToken", default="")),
        "expires_at": int(_first(raw, "expires_at", "expiresAt", default=0) or 0),
        "realm": normalize_realm(realm or _first(raw, "realm", default="cn")),
        "domain": str(_first(raw, "domain", default="")),
    }


def map_account(raw: Mapping[str, Any], *, realm: str | None = None) -> dict[str, Any]:
    """Map platform account data into the canonical, safe account view."""

    resolved_realm = normalize_realm(realm or _first(raw, "realm", default="cn"))
    uid = str(_first(raw, "uid", "user_id", "userId", default="")).strip()
    if not uid:
        raise WorkBuddyProtocolError("successful login response did not contain an account id")
    nickname = str(_first(raw, "nickname", "nick", "name", default=uid)).strip() or uid
    native_id = f"{resolved_realm}:{uid}"
    return {
        "id": f"wb:{native_id}",
        "channel": "wb",
        "native_id": native_id,
        "name": nickname,
        "kind": "oauth",
        "status": "ready",
        "enabled": True,
        "realm": resolved_realm,
        "uid": uid,
        "enterprise_id": str(_first(raw, "enterprise_id", "enterpriseId", default="")),
        "domain": str(_first(raw, "domain", default="")),
    }


def redact(raw: Mapping[str, Any] | None, *, realm: str | None = None) -> dict[str, Any]:
    """Return a JSON-safe copy with token-like fields removed."""

    if not isinstance(raw, Mapping):
        return {}
    result: dict[str, Any] = {}
    for key, value in raw.items():
        if key in _SECRET_KEYS or key.lower() in {item.lower() for item in _SECRET_KEYS}:
            continue
        if isinstance(value, Mapping):
            result[str(key)] = redact(value)
        elif isinstance(value, list):
            result[str(key)] = [
                redact(item) if isinstance(item, Mapping) else item for item in value
            ]
        else:
            result[str(key)] = value
    if realm:
        result["realm"] = normalize_realm(realm)
    return result


def session_view(
    *,
    session_id: str,
    status: str,
    realm: str,
    expires_at: float,
    auth_url: str = "",
    account: Mapping[str, Any] | None = None,
    next_step: str | None = None,
    retry_after: int | None = None,
) -> dict[str, Any]:
    result: dict[str, Any] = {
        "session_id": session_id,
        "status": status,
        "realm": normalize_realm(realm),
        "expires_at": expires_at,
    }
    if auth_url:
        result["auth_url"] = auth_url
        # ``qr_code`` is the canonical renderable value shared by all QR
        # provisioners.  WorkBuddy returns a hosted authorization URL rather
        # than an image, so the web client can render this value locally with
        # a QR encoder without exposing the provider state.
        result["qr_code"] = auth_url
    if account:
        result["account"] = dict(account)
    if next_step:
        result["next_step"] = next_step
    if retry_after is not None:
        result["retry_after"] = retry_after
    return result
