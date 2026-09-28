"""Errors and redacted error mapping for the ChatGPT adapter."""

from __future__ import annotations

from enum import Enum


class ErrorKind(str, Enum):
    INVALID_REQUEST = "invalid_request"
    AUTH_REQUIRED = "auth_required"
    CREDENTIAL_EXPIRED = "credential_expired"
    RATE_LIMITED = "rate_limited"
    UPSTREAM_TIMEOUT = "upstream_timeout"
    UPSTREAM_UNAVAILABLE = "upstream_unavailable"
    PROTOCOL_ERROR = "protocol_error"
    INTERNAL = "internal"


class ChatGPTError(Exception):
    """Base error whose string is safe to expose to an administrator."""

    kind = ErrorKind.INTERNAL


class InvalidTokenError(ChatGPTError):
    kind = ErrorKind.AUTH_REQUIRED


class OAuthStateError(ChatGPTError):
    kind = ErrorKind.INVALID_REQUEST


class OAuthExpiredError(OAuthStateError):
    """OAuth session state has passed its TTL or was already consumed."""


class OAuthProtocolError(ChatGPTError):
    kind = ErrorKind.PROTOCOL_ERROR


def map_error(error: Exception) -> dict[str, str]:
    """Map an internal exception to a stable, secret-free error envelope."""

    if isinstance(error, ChatGPTError):
        return {"kind": str(error.kind), "message": str(error)}
    # Do not include exception text: HTTP clients frequently put response bodies
    # (which may contain tokens) in it.
    name = type(error).__name__.lower()
    if "timeout" in name:
        return {"kind": ErrorKind.UPSTREAM_TIMEOUT, "message": "ChatGPT OAuth 请求超时"}
    return {"kind": ErrorKind.INTERNAL, "message": "ChatGPT 账号操作失败"}


def public_error(error: Exception) -> str:
    return map_error(error)["message"]
