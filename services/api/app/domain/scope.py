"""API key channel/model scope normalization and authorization.

Scope matching deliberately lives outside the routers so every data-plane
entry point uses the same canonical rules.  A model scope can be ``*``, a
channel wildcard (``channel/*``), a canonical model id (``channel/model``),
or a bare upstream model name retained for backwards compatibility.
"""

from __future__ import annotations

import json
from collections.abc import Iterable, Mapping
from typing import Any


class ScopeValidationError(ValueError):
    """Raised when channel and model scopes cannot be safely combined."""


def _as_list(value: Any) -> list[str] | None:
    if isinstance(value, str):
        try:
            value = json.loads(value)
        except (TypeError, json.JSONDecodeError):
            return None
    if not isinstance(value, list) or any(not isinstance(item, str) for item in value):
        return None
    return value


def normalize_scope_models(models: Iterable[str] | None) -> list[str]:
    """Normalize an API payload's model scopes.

    The contract treats an empty list as the global wildcard.  Keep ordering
    stable while deduplicating only as a defensive measure for legacy rows.
    """

    if models is None:
        return ["*"]
    values = [str(item).strip() for item in models]
    if not values:
        return ["*"]
    return list(dict.fromkeys(values))


def validate_scope(
    channels: Iterable[str],
    models: Iterable[str] | None,
    known_channels: Iterable[str],
) -> tuple[list[str], list[str]]:
    """Return normalized scopes or raise :class:`ScopeValidationError`.

    Channels are registry slugs.  A non-global model scope carrying a channel
    prefix must refer to a registered channel and, when the channel scope is
    explicit, belong to that channel set.  This prevents a Key with
    ``channels=["wb"]`` from persisting ``chatgpt/...`` authorization.
    """

    channel_values = [str(item).strip() for item in channels]
    model_values = normalize_scope_models(models)
    known = {str(item) for item in known_channels}
    if len(channel_values) != len(set(channel_values)):
        raise ScopeValidationError("channels must be unique")
    if any(not value or value not in known for value in channel_values):
        raise ScopeValidationError("channels contains an unknown channel")
    if len(model_values) != len(set(model_values)):
        raise ScopeValidationError("models must be a non-empty unique list")
    if any(not value or len(value) > 320 for value in model_values):
        raise ScopeValidationError("models contains an invalid model scope")
    if "*" in model_values and len(model_values) != 1:
        raise ScopeValidationError("wildcard model scope cannot be combined with model ids")

    for scope in model_values:
        if scope == "*":
            continue
        prefix, separator, model = scope.partition("/")
        if not separator:
            # Legacy rows may contain a bare upstream model name.  It is
            # matched against the upstream portion at request time.
            continue
        if not prefix or not model:
            raise ScopeValidationError("models contains an invalid model scope")
        if prefix not in known:
            raise ScopeValidationError(f"models contains an unknown channel: {prefix}")
        if channel_values and prefix not in channel_values:
            raise ScopeValidationError(
                f"model scope channel {prefix!r} is not included in channels"
            )
    return channel_values, model_values


def decode_key_scope(key: Mapping[str, Any]) -> tuple[list[str], list[str]] | None:
    """Decode the scope columns from a DB row or in-memory key context."""

    channels = _as_list(key.get("channels"))
    models = _as_list(key.get("models"))
    if channels is None or models is None:
        return None
    normalized_models = normalize_scope_models(models)
    if any(not isinstance(item, str) or not item.strip() for item in channels):
        return None
    if (
        not normalized_models
        or any(not item or len(item) > 320 for item in normalized_models)
        or ("*" in normalized_models and len(normalized_models) != 1)
    ):
        return None
    return channels, normalized_models


def channel_allowed(channels: Iterable[str], channel: str) -> bool:
    values = list(channels)
    return not values or channel in values


def model_allowed(models: Iterable[str], channel: str, upstream_model: str) -> bool:
    """Check a canonical target against model scopes.

    Matching is exact after the gateway has split ``channel/upstream_model``;
    no suffix or display-name matching is performed.
    """

    full_model = f"{channel}/{upstream_model}"
    for scope in models:
        if scope == "*" or scope == full_model or scope == upstream_model:
            return True
        prefix, separator, model = scope.partition("/")
        if separator and model == "*" and prefix == channel:
            return True
    return False


def scope_decision(
    key: Mapping[str, Any], channel: str, upstream_model: str
) -> str:
    """Return ``allowed`` or the stable authorization failure code."""

    decoded = decode_key_scope(key)
    if decoded is None:
        return "invalid_scope"
    channels, models = decoded
    if not channel_allowed(channels, channel):
        return "channel_not_allowed"
    if not model_allowed(models, channel, upstream_model):
        return "model_not_allowed"
    return "allowed"

