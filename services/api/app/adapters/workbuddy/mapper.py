from __future__ import annotations

import hashlib
import time
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
    "x-device-token",
    "x-refresh-token",
}


def normalize_realm(value: Any) -> str:
    realm = str(value or "cn").strip().lower()
    if realm not in {"cn", "global"}:
        raise WorkBuddyInvalidRequestError("realm must be either 'cn' or 'global'")
    return realm


def resolve_realm(explicit: Any = "", domain: Any = "") -> str:
    """Resolve the account realm using the native protocol precedence."""

    value = str(explicit or "").strip().lower()
    if value in {"cn", "global"}:
        return value
    host = str(domain or "").strip().lower()
    if host.startswith(("https://", "http://")):
        host = host.split("://", 1)[1].split("/", 1)[0]
    if host == "workbuddy.ai" or host.endswith(".workbuddy.ai"):
        return "global"
    return "cn"


def _first(payload: Mapping[str, Any], *keys: str, default: Any = "") -> Any:
    for key in keys:
        if key in payload and payload[key] not in (None, ""):
            return payload[key]
    return default


def _flatten_account(raw: Mapping[str, Any]) -> dict[str, Any]:
    """Flatten flat login data and the nested account export shape."""

    result: dict[str, Any] = {}
    for key in ("auth", "credentials", "account"):
        value = raw.get(key)
        if isinstance(value, Mapping):
            result.update(value)
    result.update(
        {
            str(key): value
            for key, value in raw.items()
            if key not in {"auth", "credentials", "account"}
        }
    )
    return result


def _as_int(value: Any, default: int = 0) -> int:
    if isinstance(value, bool):
        return default
    try:
        return int(value)
    except (TypeError, ValueError, OverflowError):
        return default


def _expires_at(payload: Mapping[str, Any], now: float | None = None) -> int:
    explicit = _as_int(_first(payload, "expires_at", "expiresAt", default=0))
    if explicit > 0:
        return explicit
    lifetime = _as_int(_first(payload, "expires_in", "expiresIn", default=0))
    if lifetime > 0:
        return int((time.time() if now is None else now) + lifetime)
    return 0


def credential_record(
    raw: Mapping[str, Any], *, realm: str | None = None, now: float | None = None
) -> dict[str, Any]:
    """Build the private credential record written through CredentialStore."""

    payload = _flatten_account(raw)
    access = _first(payload, "access_token", "accessToken", "token")
    if not isinstance(access, str) or not access.strip():
        raise WorkBuddyProtocolError("successful login response did not contain an access token")
    domain = str(_first(payload, "domain", default="")).strip()
    resolved_realm = resolve_realm(
        realm or _first(payload, "realm", default=""),
        domain,
    )
    uid = str(_first(payload, "uid", "user_id", "userId", default="")).strip()
    enterprise_id = str(
        _first(payload, "enterprise_id", "enterpriseId", "tenant_id", "tenantId", default="")
    ).strip()
    device_token = str(_first(payload, "device_token", "deviceToken", default="")).strip()
    record: dict[str, Any] = {
        "access_token": access.strip(),
        "refresh_token": str(_first(payload, "refresh_token", "refreshToken", default="")),
        "device_token": device_token,
        "expires_at": _expires_at(payload, now),
        "realm": resolved_realm,
        "domain": domain,
        "enterprise_id": enterprise_id,
    }
    if uid:
        record["uid"] = uid
        record["X-User-Id"] = uid
    if enterprise_id:
        record["X-Enterprise-Id"] = enterprise_id
        record["X-Tenant-Id"] = enterprise_id
    if domain:
        record["X-Domain"] = domain
    if device_token:
        record["X-Device-Token"] = device_token
    record["X-CodeBuddy-Request"] = "1"
    record["X-Requested-With"] = "XMLHttpRequest"
    record["Origin"] = (
        "https://www.workbuddy.ai" if resolved_realm == "global" else "https://copilot.tencent.com"
    )
    record["Referer"] = f"{record['Origin']}/"
    record["User-Agent"] = "CLI/2.63.2 CodeBuddy/2.63.2"
    record["Accept-Language"] = "en-US" if resolved_realm == "global" else "zh-CN"
    return record


def map_account(raw: Mapping[str, Any], *, realm: str | None = None) -> dict[str, Any]:
    """Map platform account data into the canonical, safe account view."""

    payload = _flatten_account(raw)
    domain = str(_first(payload, "domain", default="")).strip()
    resolved_realm = resolve_realm(
        realm or _first(payload, "realm", default=""),
        domain,
    )
    uid = str(_first(payload, "uid", "user_id", "userId", default="")).strip()
    if not uid:
        raise WorkBuddyProtocolError("successful login response did not contain an account id")
    nickname = str(_first(payload, "nickname", "nick", "name", default=uid)).strip() or uid
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
        "enterprise_id": str(
            _first(payload, "enterprise_id", "enterpriseId", "tenant_id", "tenantId", default="")
        ),
        "domain": domain,
        "expires_at": _expires_at(payload),
    }


def import_record(raw: Any, *, now: float | None = None) -> dict[str, Any]:
    """Normalize one administrator-supplied token bundle without network lookup."""

    if not isinstance(raw, Mapping):
        raise ValueError("account item is invalid")
    payload = _flatten_account(raw)
    uid = str(_first(payload, "uid", "user_id", "userId", default="")).strip()
    if not uid:
        raise ValueError("account uid is required")
    access = _first(payload, "access_token", "accessToken", "token")
    if not isinstance(access, str) or not access.strip():
        raise ValueError("access token is required")
    resolved_realm = resolve_realm(
        _first(payload, "realm", default=""),
        _first(payload, "domain", default=""),
    )
    return {
        "fingerprint": hashlib.sha256(access.strip().encode("utf-8")).hexdigest()[:24],
        "credentials": credential_record(payload, realm=resolved_realm, now=now),
        "account": map_account(payload, realm=resolved_realm),
    }


def _non_chat_model(model_id: str, max_output_tokens: int, tags: list[str]) -> bool:
    lowered = model_id.strip().lower()
    if lowered.startswith(("nes-", "completion-", "codewise-")):
        return True
    if 0 < max_output_tokens <= 256:
        return True
    return "text-to-image" in tags


def _model_entry(raw: Mapping[str, Any]) -> dict[str, Any] | None:
    model_id = str(raw.get("id") or raw.get("modelId") or "").strip()
    if not model_id:
        return None
    reasoning = raw.get("reasoning") if isinstance(raw.get("reasoning"), Mapping) else {}
    efforts = reasoning.get("supportedEfforts")
    tags = raw.get("tags") if isinstance(raw.get("tags"), list) else []
    clean_tags = [str(item) for item in tags if item]
    max_output = _as_int(raw.get("maxOutputTokens", raw.get("max_output_tokens", 0)))
    if _non_chat_model(model_id, max_output, clean_tags) or bool(raw.get("disabled")):
        return None
    return {
        "id": model_id,
        "name": str(raw.get("name") or model_id).strip(),
        "context_length": _as_int(raw.get("maxInputTokens", raw.get("context_length", 0))),
        "max_output_tokens": max_output,
        "efforts": [str(item) for item in efforts if item] if isinstance(efforts, list) else [],
        "default_effort": str(reasoning.get("defaultEffort") or "").strip(),
        "supports_images": bool(raw.get("supportsImages", raw.get("supports_images", False))),
        "description": str(raw.get("descriptionZh") or raw.get("description") or "").strip(),
        "credits": str(raw.get("credits") or "").strip(),
        "vendor": str(raw.get("vendor") or "").strip(),
        "tags": clean_tags,
        "is_default": bool(raw.get("isDefault", raw.get("is_default", False))),
        "supports_reasoning": bool(raw.get("supportsReasoning", False)),
        "supports_tool_call": bool(raw.get("supportsToolCall", False)),
        "only_reasoning": bool(raw.get("onlyReasoning", False)),
        "reasoning_summary": str(reasoning.get("summary") or "").strip(),
    }


def map_model_payload(
    payload: Any, *, realm: str = "cn", enterprise: bool = False
) -> list[dict[str, Any]]:
    """Map one real WorkBuddy model response into the gateway catalogue shape."""

    resolved_realm = normalize_realm(realm)
    data = payload
    if isinstance(data, Mapping) and "data" in data and "models" not in data:
        data = data.get("data")
    if isinstance(data, list):
        result: list[dict[str, Any]] = []
        for item in data:
            if isinstance(item, Mapping):
                mapped = _model_entry(item)
            else:
                model_id = str(item or "").strip()
                mapped = {"id": model_id, "name": model_id} if model_id else None
            if mapped is not None:
                result.append(mapped)
        return result
    if not isinstance(data, Mapping):
        return []
    raw_models = data.get("models") if isinstance(data.get("models"), list) else []
    cli_ids: list[str] = []
    agents = data.get("agents") if isinstance(data.get("agents"), list) else []
    if enterprise and resolved_realm == "cn":
        for agent in agents:
            if isinstance(agent, Mapping) and str(agent.get("name") or "") == "cli":
                values = agent.get("models")
                if isinstance(values, list):
                    cli_ids = [str(value).strip() for value in values if str(value).strip()]
                break
    by_id: dict[str, dict[str, Any]] = {}
    for raw in raw_models:
        if not isinstance(raw, Mapping):
            continue
        mapped = _model_entry(raw)
        if mapped is not None:
            by_id[str(mapped["id"])] = mapped
    ids = cli_ids or list(by_id)
    return [by_id[model_id] for model_id in ids if model_id in by_id]


def merge_model_catalog(
    primary: list[Mapping[str, Any]], secondary: list[Mapping[str, Any]]
) -> list[dict[str, Any]]:
    """Merge v3 as authoritative and append enterprise-only models."""

    result: list[dict[str, Any]] = []
    seen: set[str] = set()
    for item in [*primary, *secondary]:
        if not isinstance(item, Mapping):
            continue
        model_id = str(item.get("id") or "").strip()
        if not model_id or model_id in seen:
            continue
        seen.add(model_id)
        result.append(dict(item))
    return result


def prepare_chat_payload(payload: Mapping[str, Any], *, realm: str) -> dict[str, Any]:
    """Apply the native chat body requirements."""

    result = dict(payload)
    result["stream"] = True
    if "stream_options" not in result:
        result["stream_options"] = {"include_usage": True}
    if "max_completion_tokens" in result and "max_tokens" not in result:
        value = result.get("max_completion_tokens")
        if isinstance(value, int) and not isinstance(value, bool) and value > 0:
            result["max_tokens"] = value
        del result["max_completion_tokens"]
    messages = result.get("messages")
    if isinstance(messages, list):
        normalized: list[Any] = []
        for message in messages:
            if isinstance(message, Mapping):
                item = dict(message)
                if str(item.get("role") or "").strip().lower() == "developer":
                    item["role"] = "system"
                normalized.append(item)
            else:
                normalized.append(message)
        if normalize_realm(realm) == "global" and normalized:
            first = normalized[0]
            if not isinstance(first, Mapping) or str(first.get("role") or "").lower() != "system":
                normalized.insert(
                    0,
                    {"role": "system", "content": "You are a helpful assistant."},
                )
        result["messages"] = normalized
    return result


def redact(raw: Mapping[str, Any] | None, *, realm: str | None = None) -> dict[str, Any]:
    """Return a JSON-safe copy with token-like fields removed."""

    if not isinstance(raw, Mapping):
        return {}
    secret_names = {item.lower() for item in _SECRET_KEYS}
    result: dict[str, Any] = {}
    for key, value in raw.items():
        if str(key).lower() in secret_names:
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
        result["qr_code"] = auth_url
    if account:
        result["account"] = dict(account)
    if next_step:
        result["next_step"] = next_step
    if retry_after is not None:
        result["retry_after"] = retry_after
    return result
