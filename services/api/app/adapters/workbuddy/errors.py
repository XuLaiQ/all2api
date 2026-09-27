from __future__ import annotations

from typing import Any


class WorkBuddyError(Exception):
    """Base error for native WorkBuddy protocol operations."""

    code = "workbuddy_error"
    retryable = False

    def __init__(self, message: str = "WorkBuddy operation failed", *, code: str | None = None):
        super().__init__(message)
        if code is not None:
            self.code = code

    def as_dict(self) -> dict[str, Any]:
        return {"code": self.code, "message": str(self), "retryable": self.retryable}


class WorkBuddyInvalidRequestError(WorkBuddyError):
    code = "invalid_request"


class WorkBuddySessionNotFoundError(WorkBuddyError):
    code = "session_not_found"


class WorkBuddySessionExpiredError(WorkBuddyError):
    code = "session_expired"


class WorkBuddyStateError(WorkBuddyError):
    code = "invalid_state"


class WorkBuddyRealmMismatchError(WorkBuddyError):
    code = "realm_mismatch"


class WorkBuddyTransportError(WorkBuddyError):
    code = "upstream_unavailable"
    retryable = True


class WorkBuddyTimeoutError(WorkBuddyTransportError):
    code = "upstream_timeout"


class WorkBuddyProtocolError(WorkBuddyError):
    code = "protocol_error"
    retryable = True


class WorkBuddyCredentialError(WorkBuddyError):
    code = "credential_invalid"


class WorkBuddyCredentialStoreError(WorkBuddyError):
    code = "credential_store_error"
    retryable = True
