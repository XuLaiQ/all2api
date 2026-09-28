"""Safe, channel-owned errors for the ChatGPT Web native client."""

from __future__ import annotations

from enum import StrEnum
from typing import Any

import httpx


class ErrorKind(StrEnum):
    INVALID_REQUEST = "invalid_request"
    AUTH_REQUIRED = "auth_required"
    CREDENTIAL_EXPIRED = "credential_expired"
    RATE_LIMITED = "rate_limited"
    UPSTREAM_TIMEOUT = "upstream_timeout"
    UPSTREAM_UNAVAILABLE = "upstream_unavailable"
    PROTOCOL_ERROR = "protocol_error"
    INTERNAL = "internal"


class ChatGPTError(Exception):
    """An exception with a message that is safe to expose to callers."""

    kind = ErrorKind.INTERNAL
    status_code = 502
    retryable = False
    code = "chatgpt_error"

    def __init__(
        self,
        message: str = "ChatGPT request failed",
        *,
        status_code: int | None = None,
        retry_after: int | None = None,
        code: str | None = None,
    ) -> None:
        super().__init__(message)
        if status_code is not None:
            self.status_code = int(status_code)
        self.retry_after = retry_after
        if code is not None:
            self.code = code

    def as_dict(self) -> dict[str, Any]:
        result: dict[str, Any] = {
            "code": self.code,
            "kind": str(self.kind),
            "message": str(self),
            "retryable": self.retryable,
            "status_code": self.status_code,
        }
        if self.retry_after is not None:
            result["retry_after"] = self.retry_after
        return result


class ChatGPTInvalidRequestError(ChatGPTError):
    kind = ErrorKind.INVALID_REQUEST
    status_code = 400
    code = "invalid_request"


class InvalidTokenError(ChatGPTError):
    kind = ErrorKind.AUTH_REQUIRED
    status_code = 401
    code = "authentication_error"


class ChatGPTAuthError(InvalidTokenError):
    """The provider rejected the supplied browser credential."""

    def __init__(self) -> None:
        super().__init__("ChatGPT credential is invalid or expired")


class ChatGPTRateLimitError(ChatGPTError):
    kind = ErrorKind.RATE_LIMITED
    status_code = 429
    retryable = True
    code = "rate_limited"

    def __init__(self, retry_after: int | None = None) -> None:
        super().__init__(
            "ChatGPT is rate limited; retry later",
            status_code=429,
            retry_after=retry_after,
        )


class ChatGPTTimeoutError(ChatGPTError):
    kind = ErrorKind.UPSTREAM_TIMEOUT
    status_code = 504
    retryable = True
    code = "upstream_timeout"

    def __init__(self) -> None:
        super().__init__("ChatGPT request timed out", status_code=504)


class ChatGPTUpstreamUnavailableError(ChatGPTError):
    kind = ErrorKind.UPSTREAM_UNAVAILABLE
    status_code = 502
    retryable = True
    code = "upstream_unavailable"

    def __init__(self) -> None:
        super().__init__("ChatGPT is temporarily unavailable", status_code=502)


class ChatGPTProtocolError(ChatGPTError):
    kind = ErrorKind.PROTOCOL_ERROR
    status_code = 502
    retryable = True
    code = "upstream_protocol_error"

    def __init__(self, message: str = "ChatGPT returned an invalid response") -> None:
        super().__init__(message, status_code=502)


class OAuthStateError(ChatGPTError):
    kind = ErrorKind.INVALID_REQUEST
    status_code = 400
    code = "oauth_state_error"


class OAuthExpiredError(OAuthStateError):
    """OAuth session state has passed its TTL or was already consumed."""


class OAuthProtocolError(ChatGPTError):
    kind = ErrorKind.PROTOCOL_ERROR
    status_code = 502
    code = "oauth_protocol_error"


def _safe_http_error(error: httpx.HTTPError) -> ChatGPTError:
    if isinstance(error, httpx.TimeoutException):
        return ChatGPTTimeoutError()
    if isinstance(error, httpx.RequestError):
        return ChatGPTUpstreamUnavailableError()
    return ChatGPTUpstreamUnavailableError()


def map_error(error: Exception) -> dict[str, Any]:
    """Map an internal exception to a stable, secret-free error envelope."""

    if isinstance(error, ChatGPTError):
        return error.as_dict()
    if isinstance(error, httpx.HTTPError):
        return _safe_http_error(error).as_dict()
    if isinstance(error, (ValueError, TypeError)):
        return ChatGPTInvalidRequestError("invalid ChatGPT request").as_dict()
    return {
        "code": "internal",
        "kind": str(ErrorKind.INTERNAL),
        "message": "ChatGPT request failed",
        "retryable": False,
        "status_code": 500,
    }


def public_error(error: Exception) -> str:
    return str(map_error(error)["message"])


__all__ = [
    "ChatGPTAuthError",
    "ChatGPTError",
    "ChatGPTInvalidRequestError",
    "ChatGPTProtocolError",
    "ChatGPTRateLimitError",
    "ChatGPTTimeoutError",
    "ChatGPTUpstreamUnavailableError",
    "ErrorKind",
    "InvalidTokenError",
    "OAuthExpiredError",
    "OAuthProtocolError",
    "OAuthStateError",
    "map_error",
    "public_error",
]
