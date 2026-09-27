"""Safe, channel-owned errors for Doubao provisioning."""

from __future__ import annotations

from dataclasses import dataclass


@dataclass
class DoubaoProvisionError(Exception):
    """An expected provisioning failure with a safe public message."""

    message: str
    code: str = "doubao_provision_error"
    status_code: int = 409

    def __post_init__(self) -> None:
        super().__init__(self.message)


class InvalidAccountIdError(DoubaoProvisionError):
    def __init__(self, account_id: str = "") -> None:
        super().__init__(
            "账号 ID 只能包含字母、数字、下划线和短横线",
            code="invalid_account_id",
            status_code=422,
        )


class ProfileAlreadyExistsError(DoubaoProvisionError):
    def __init__(self, account_id: str) -> None:
        super().__init__(
            "Doubao 账号 profile 已存在",
            code="profile_exists",
            status_code=409,
        )


class ProfileNotFoundError(DoubaoProvisionError):
    def __init__(self, account_id: str) -> None:
        super().__init__(
            "Doubao 账号 profile 不存在",
            code="profile_not_found",
            status_code=404,
        )


class ProvisionSessionNotFoundError(DoubaoProvisionError):
    def __init__(self, session_id: str) -> None:
        super().__init__(
            "Doubao 账号新增会话不存在或已过期",
            code="session_not_found",
            status_code=404,
        )


class ProvisionSessionExpiredError(DoubaoProvisionError):
    def __init__(self) -> None:
        super().__init__(
            "Doubao 二维码登录会话已过期",
            code="session_expired",
            status_code=410,
        )


class ProvisionSessionCancelledError(DoubaoProvisionError):
    def __init__(self) -> None:
        super().__init__(
            "Doubao 二维码登录会话已取消",
            code="session_cancelled",
            status_code=409,
        )


class BrowserWorkerError(DoubaoProvisionError):
    def __init__(self, message: str = "Doubao 浏览器 worker 操作失败") -> None:
        super().__init__(message, code="browser_worker_error", status_code=502)


class BrowserWorkerUnavailableError(BrowserWorkerError):
    def __init__(self) -> None:
        # Keep this distinct from a transient worker failure.  The admin API
        # uses the code/status to tell operators why no QR challenge can be
        # returned, while the message stays safe for the management UI.
        DoubaoProvisionError.__init__(
            self,
            "Doubao 浏览器 worker 尚未配置，请设置 A2A_DOUBAO_BROWSER_ENABLED=true 并安装 Chromium",
            code="browser_worker_unavailable",
            status_code=503,
        )


class CredentialStoreError(DoubaoProvisionError):
    def __init__(self) -> None:
        super().__init__(
            "Doubao 凭据安全存储失败",
            code="credential_store_error",
            status_code=503,
        )


def safe_error(exc: Exception) -> tuple[int, str, str]:
    """Map an expected exception to ``(status, code, message)``."""

    if isinstance(exc, DoubaoProvisionError):
        return exc.status_code, exc.code, exc.message
    return 502, "doubao_provision_error", "Doubao 账号新增失败"

