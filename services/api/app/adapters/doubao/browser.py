"""Browser worker ports used by the Doubao QR login state machine.

The port keeps Playwright and platform details out of the application service.
The fake worker is deliberately deterministic and emits synthetic QR values;
it never contains a real Cookie, profile or token.
"""

from __future__ import annotations

import asyncio
import base64
import inspect
import time
import uuid
from collections.abc import Mapping, Sequence
from dataclasses import dataclass, field
from typing import Any, Protocol

from .errors import BrowserWorkerError, BrowserWorkerUnavailableError


@dataclass(frozen=True)
class BrowserChallenge:
    session_id: str
    account_id: str
    qr_code: str
    qr_image_base64: str | None = None
    status: str = "waiting_scan"
    expires_at: float | None = None


@dataclass(frozen=True)
class BrowserEvent:
    """A redacted worker event.

    ``credentials`` is consumed by the provisioner and never returned to the
    API.  Production workers should provide a short lived mapping here and
    immediately erase their local copy after the CredentialStore write.
    """

    session_id: str
    account_id: str
    status: str
    qr_code: str | None = None
    qr_image_base64: str | None = None
    message: str = ""
    credentials: Mapping[str, Any] | None = None
    error_code: str | None = None


class BrowserWorker(Protocol):
    async def start(self) -> None: ...

    async def stop(self) -> None: ...

    async def restart(self) -> None: ...

    async def health(self) -> Mapping[str, Any]: ...

    def is_alive(self) -> bool: ...

    async def start_qr_login(self, account_id: str, profile_path: str) -> BrowserChallenge: ...

    async def restore_qr_login(
        self,
        session_id: str,
        account_id: str,
        profile_path: str,
        created_at: float,
    ) -> BrowserChallenge | None: ...

    async def poll_qr_login(self, session_id: str) -> BrowserEvent: ...

    async def complete_qr_login(self, session_id: str) -> BrowserEvent: ...

    async def cancel_qr_login(self, session_id: str) -> None: ...

    async def delete_account(self, account_id: str) -> None: ...


class NullBrowserWorker:
    """Production-safe default until a Playwright worker is wired in."""

    async def start(self) -> None:
        return None

    async def stop(self) -> None:
        return None

    async def restart(self) -> None:
        return None

    async def health(self) -> Mapping[str, Any]:
        return {
            "status": "not_configured",
            "worker": "null",
            "active_sessions": 0,
        }

    def is_alive(self) -> bool:
        return False

    async def start_qr_login(self, account_id: str, profile_path: str) -> BrowserChallenge:
        raise BrowserWorkerUnavailableError()

    async def restore_qr_login(
        self,
        session_id: str,
        account_id: str,
        profile_path: str,
        created_at: float,
    ) -> BrowserChallenge | None:
        raise BrowserWorkerUnavailableError()

    async def poll_qr_login(self, session_id: str) -> BrowserEvent:
        raise BrowserWorkerUnavailableError()

    async def complete_qr_login(self, session_id: str) -> BrowserEvent:
        raise BrowserWorkerUnavailableError()

    async def cancel_qr_login(self, session_id: str) -> None:
        return None

    async def delete_account(self, account_id: str) -> None:
        return None


class FakeBrowserWorker:
    """Deterministic worker for adapter contract tests.

    ``events`` maps a session to a sequence of statuses.  A synthetic QR
    challenge is returned by ``start_qr_login``.  Optional ``credentials`` are
    test fixtures only and should contain fake values.
    """

    def __init__(
        self,
        events: Sequence[str] = ("waiting_scan", "scanned", "confirmed"),
        *,
        credentials: Mapping[str, Any] | None = None,
    ) -> None:
        self.events = tuple(events)
        self.credentials = dict(credentials or {"session": "fake-session"})
        self.sessions: dict[str, dict[str, Any]] = {}
        self.cancelled: set[str] = set()

    async def start(self) -> None:
        return None

    async def stop(self) -> None:
        self.sessions.clear()

    async def restart(self) -> None:
        await self.stop()

    async def health(self) -> Mapping[str, Any]:
        return {
            "status": "ready",
            "worker": "fake",
            "active_sessions": len(self.sessions),
        }

    def is_alive(self) -> bool:
        return True

    async def start_qr_login(self, account_id: str, profile_path: str) -> BrowserChallenge:
        session_id = f"fake-{uuid.uuid4().hex}"
        self.sessions[session_id] = {
            "account_id": account_id,
            "profile_path": profile_path,
            "index": 0,
        }
        return BrowserChallenge(
            session_id=session_id,
            account_id=account_id,
            qr_code=f"fake://doubao/qr/{session_id}",
        )

    async def restore_qr_login(
        self,
        session_id: str,
        account_id: str,
        profile_path: str,
        created_at: float,
    ) -> BrowserChallenge | None:
        """Recreate a deterministic worker session after a process restart."""

        if session_id in self.sessions:
            return None
        self.sessions[session_id] = {
            "account_id": account_id,
            "profile_path": profile_path,
            "index": 0,
            "created_at": created_at,
        }
        return BrowserChallenge(
            session_id=session_id,
            account_id=account_id,
            qr_code=f"fake://doubao/qr/{session_id}",
        )

    async def poll_qr_login(self, session_id: str) -> BrowserEvent:
        current = self.sessions[session_id]
        index = min(int(current["index"]), max(0, len(self.events) - 1))
        status = self.events[index] if self.events else "waiting_scan"
        current["index"] = int(current["index"]) + 1
        return BrowserEvent(
            session_id=session_id,
            account_id=str(current["account_id"]),
            status=status,
            qr_code=f"fake://doubao/qr/{session_id}",
            credentials=self.credentials if status in {"confirmed", "succeeded"} else None,
        )

    async def complete_qr_login(self, session_id: str) -> BrowserEvent:
        current = self.sessions[session_id]
        return BrowserEvent(
            session_id=session_id,
            account_id=str(current["account_id"]),
            status="confirmed",
            credentials=self.credentials,
        )

    async def cancel_qr_login(self, session_id: str) -> None:
        self.cancelled.add(session_id)

    async def delete_account(self, account_id: str) -> None:
        for session_id, session in list(self.sessions.items()):
            if str(session.get("account_id")) == str(account_id):
                self.cancelled.add(session_id)
                self.sessions.pop(session_id, None)


async def maybe_await(value: Any) -> Any:
    """Accept sync fakes as well as async browser implementations."""

    if inspect.isawaitable(value):
        return await value
    return value


@dataclass(frozen=True)
class BrowserWorkerConfig:
    """Non-secret settings for the optional Playwright worker.

    The worker is intentionally opt-in.  Constructing this value never imports
    Playwright or starts Chromium.  ``from_settings`` is kept here so adapter
    composition remains the only place that translates application settings
    into browser infrastructure.
    """

    enabled: bool = False
    platform_base_url: str = "https://www.doubao.com"
    login_path: str = "/"
    executable_path: str = ""
    headless: bool = True
    max_contexts: int = 4
    max_pages_per_context: int = 2
    operation_timeout_seconds: float = 30.0
    launch_timeout_seconds: float = 30.0
    session_ttl_seconds: float = 300.0
    qr_selector: str = 'img[src*="qr"], canvas[data-qr]'
    qr_code_attribute: str = "data-code"
    authenticated_selector: str = '[data-testid="user-avatar"], [data-authenticated="true"]'
    launch_args: tuple[str, ...] = field(default_factory=lambda: ("--disable-dev-shm-usage",))

    def __post_init__(self) -> None:
        if self.max_contexts < 1:
            raise ValueError("max_contexts must be positive")
        if self.max_pages_per_context < 1:
            raise ValueError("max_pages_per_context must be positive")
        if (
            self.operation_timeout_seconds <= 0
            or self.launch_timeout_seconds <= 0
            or self.session_ttl_seconds <= 0
        ):
            raise ValueError("browser timeouts must be positive")
        if not str(self.platform_base_url).strip():
            raise ValueError("platform_base_url must not be blank")

    @classmethod
    def from_settings(cls, settings: Any) -> BrowserWorkerConfig:
        def text(name: str, default: str = "") -> str:
            return str(getattr(settings, name, default) or default).strip()

        return cls(
            enabled=bool(getattr(settings, "doubao_browser_enabled", False)),
            platform_base_url=text(
                "doubao_platform_base", "https://www.doubao.com"
            ).rstrip("/"),
            login_path=text("doubao_browser_login_path", "/") or "/",
            executable_path=text("doubao_browser_executable"),
            headless=bool(getattr(settings, "doubao_browser_headless", True)),
            max_contexts=int(getattr(settings, "doubao_browser_max_contexts", 4)),
            max_pages_per_context=int(
                getattr(settings, "doubao_browser_max_pages_per_context", 2)
            ),
            operation_timeout_seconds=float(
                getattr(settings, "doubao_browser_operation_timeout_seconds", 30.0)
            ),
            launch_timeout_seconds=float(
                getattr(settings, "doubao_browser_launch_timeout_seconds", 30.0)
            ),
            session_ttl_seconds=float(
                getattr(settings, "doubao_browser_session_ttl_seconds", 300.0)
            ),
            qr_selector=text("doubao_browser_qr_selector", cls.qr_selector),
            qr_code_attribute=text("doubao_browser_qr_code_attribute", cls.qr_code_attribute),
            authenticated_selector=text(
                "doubao_browser_authenticated_selector", cls.authenticated_selector
            ),
        )

    def as_public_dict(self) -> dict[str, Any]:
        """Return operational metadata safe to expose in health/config views."""

        return {
            "enabled": self.enabled,
            "platform_base_url": self.platform_base_url,
            "login_path": self.login_path,
            "headless": self.headless,
            "max_contexts": self.max_contexts,
            "max_pages_per_context": self.max_pages_per_context,
            "operation_timeout_seconds": self.operation_timeout_seconds,
            "launch_timeout_seconds": self.launch_timeout_seconds,
            "session_ttl_seconds": self.session_ttl_seconds,
        }


@dataclass
class _PlaywrightSession:
    account_id: str
    profile_path: str
    context: Any
    page: Any
    created_at: float


class PlaywrightBrowserWorker:
    """Optional in-process Playwright worker for native Doubao QR login.

    Playwright is imported lazily from :meth:`start`; the default registry uses
    :class:`NullBrowserWorker`, so normal development and API tests never need
    Chromium or the Playwright package installed.  A small factory injection
    keeps lifecycle and crash recovery contract-testable without a browser.
    """

    def __init__(
        self,
        config: BrowserWorkerConfig | None = None,
        *,
        playwright_factory: Any | None = None,
        clock: Any = time.monotonic,
    ) -> None:
        self.config = config or BrowserWorkerConfig(enabled=True)
        self._playwright_factory = playwright_factory
        self._clock = clock
        self._playwright: Any | None = None
        self._browser: Any | None = None
        self._sessions: dict[str, _PlaywrightSession] = {}
        self._lock = asyncio.Lock()
        self._started = False
        self._failure_count = 0
        self._last_error = ""

    async def start(self) -> None:
        async with self._lock:
            if not self.config.enabled:
                raise BrowserWorkerUnavailableError()
            if self._started and self._is_connected():
                return
            if self._started:
                await self._stop_unlocked()
            factory = self._playwright_factory
            if factory is None:
                try:
                    from playwright.async_api import async_playwright
                except ImportError as exc:
                    self._last_error = "playwright_not_installed"
                    raise BrowserWorkerUnavailableError() from exc
                factory = async_playwright
            try:
                manager = factory()
                self._playwright = await maybe_await(manager.start())
                chromium = self._playwright.chromium
                launch_kwargs: dict[str, Any] = {
                    "headless": self.config.headless,
                    "timeout": int(self.config.launch_timeout_seconds * 1000),
                }
                if self.config.executable_path:
                    launch_kwargs["executable_path"] = self.config.executable_path
                if self.config.launch_args:
                    launch_kwargs["args"] = list(self.config.launch_args)
                self._browser = await maybe_await(chromium.launch(**launch_kwargs))
                self._started = True
                self._last_error = ""
            except BrowserWorkerUnavailableError:
                raise
            except Exception as exc:
                self._failure_count += 1
                self._last_error = type(exc).__name__
                await self._stop_unlocked()
                raise BrowserWorkerError() from exc

    async def stop(self) -> None:
        async with self._lock:
            await self._stop_unlocked()

    async def _stop_unlocked(self) -> None:
        sessions = list(self._sessions.values())
        self._sessions.clear()
        for session in sessions:
            try:
                await maybe_await(session.context.close())
            except Exception:
                self._failure_count += 1
        if self._browser is not None:
            try:
                await maybe_await(self._browser.close())
            except Exception:
                self._failure_count += 1
        if self._playwright is not None:
            try:
                await maybe_await(self._playwright.stop())
            except Exception:
                self._failure_count += 1
        self._playwright = None
        self._browser = None
        self._started = False

    async def restart(self) -> None:
        await self.stop()
        await self.start()

    def _is_connected(self) -> bool:
        if self._browser is None:
            return False
        connected = getattr(self._browser, "is_connected", None)
        if callable(connected):
            try:
                value = connected()
                return bool(value)
            except Exception:
                return False
        return True

    async def _ensure_running(self) -> None:
        if not self._started or not self._is_connected():
            if self._started:
                # A crashed browser invalidates every existing context.  Drop
                # them before relaunching so stale pages cannot leak cookies.
                await self.restart()
            else:
                await self.start()

    async def health(self) -> Mapping[str, Any]:
        connected = self._is_connected()
        if not self.config.enabled:
            status = "not_configured"
        elif connected:
            status = "ready"
        elif self._last_error:
            status = "degraded"
        elif self._started:
            status = "degraded"
        else:
            status = "stopped"
        return {
            "status": status,
            "worker": "playwright",
            "browser_connected": connected,
            "active_sessions": len(self._sessions),
            "max_contexts": self.config.max_contexts,
            "failure_count": self._failure_count,
            "last_error": self._last_error,
        }

    def is_alive(self) -> bool:
        """Synchronous watchdog hook matching common process supervisors."""

        return bool(self._started and self._is_connected())

    async def stats(self) -> Mapping[str, Any]:
        """Alias for health used by operational metrics collectors."""

        return await self.health()

    async def _new_context(self, profile_path: str) -> Any:
        await self._ensure_running()
        if len(self._sessions) >= self.config.max_contexts:
            raise BrowserWorkerError("Doubao 浏览器 worker 已达到上下文上限")
        browser = self._browser
        if browser is None:
            raise BrowserWorkerUnavailableError()
        kwargs: dict[str, Any] = {
            "accept_downloads": False,
            "java_script_enabled": True,
        }
        return await maybe_await(browser.new_context(**kwargs))

    async def _new_page(self, context: Any) -> Any:
        pages = getattr(context, "pages", ())
        if callable(pages):
            pages = pages()
        if len(pages or ()) >= self.config.max_pages_per_context:
            raise BrowserWorkerError("Doubao 浏览器 worker 已达到页面上限")
        return await maybe_await(context.new_page())

    async def _goto(self, page: Any) -> None:
        url = f"{self.config.platform_base_url.rstrip('/')}/{self.config.login_path.lstrip('/')}"
        await maybe_await(
            page.goto(
                url,
                wait_until="domcontentloaded",
                timeout=int(self.config.operation_timeout_seconds * 1000),
            )
        )

    async def _selector_exists(self, page: Any, selector: str) -> bool:
        if not selector:
            return False
        try:
            result = await maybe_await(page.query_selector(selector))
            return result is not None
        except Exception:
            return False

    async def _qr_value(self, page: Any) -> tuple[str | None, str | None]:
        try:
            element = await maybe_await(page.query_selector(self.config.qr_selector))
            if element is None:
                return None, None
            code = await maybe_await(element.get_attribute(self.config.qr_code_attribute))
            src = await maybe_await(element.get_attribute("src"))
            if code:
                return str(code), None
            if src:
                # Capture an image element in the browser context.  Returning
                # its URL would make the UI encode that URL as a new QR code,
                # and a blob/relative URL may not be reachable from the admin
                # origin.  Fall back to the URL only when screenshot capture
                # is unavailable in a custom worker implementation.
                try:
                    screenshot = await maybe_await(element.screenshot(type="png"))
                except Exception:
                    screenshot = None
                if screenshot:
                    return None, base64.b64encode(screenshot).decode("ascii")
                return None, str(src)
            screenshot = await maybe_await(element.screenshot(type="png"))
            if screenshot:
                return None, base64.b64encode(screenshot).decode("ascii")
        except Exception:
            return None, None
        return None, None

    async def start_qr_login(self, account_id: str, profile_path: str) -> BrowserChallenge:
        context = await self._new_context(profile_path)
        try:
            page = await self._new_page(context)
            await self._goto(page)
            session_id = f"pw-{uuid.uuid4().hex}"
            qr_code, qr_image = await self._qr_value(page)
            self._sessions[session_id] = _PlaywrightSession(
                account_id=account_id,
                profile_path=profile_path,
                context=context,
                page=page,
                created_at=self._clock(),
            )
            return BrowserChallenge(
                session_id=session_id,
                account_id=account_id,
                qr_code=qr_code or f"doubao://qr/{session_id}",
                qr_image_base64=qr_image,
            )
        except Exception as exc:
            try:
                await maybe_await(context.close())
            except Exception:
                pass
            if isinstance(exc, BrowserWorkerError):
                raise
            self._failure_count += 1
            self._last_error = type(exc).__name__
            raise BrowserWorkerError() from exc

    async def restore_qr_login(
        self,
        session_id: str,
        account_id: str,
        profile_path: str,
        created_at: float,
    ) -> BrowserChallenge | None:
        """Reopen a durable session with a fresh page after worker restart.

        Browser contexts are deliberately not serialized.  If Chromium was
        restarted, the provider may issue a new QR challenge; the durable
        provision session id stays stable and the challenge is returned to the
        caller on its next poll.
        """

        if session_id in self._sessions:
            return None
        context = await self._new_context(profile_path)
        try:
            page = await self._new_page(context)
            await self._goto(page)
            qr_code, qr_image = await self._qr_value(page)
            self._sessions[session_id] = _PlaywrightSession(
                account_id=account_id,
                profile_path=profile_path,
                context=context,
                page=page,
                created_at=created_at,
            )
            return BrowserChallenge(
                session_id=session_id,
                account_id=account_id,
                qr_code=qr_code or f"doubao://qr/{session_id}",
                qr_image_base64=qr_image,
            )
        except Exception as exc:
            try:
                await maybe_await(context.close())
            except Exception:
                pass
            if isinstance(exc, BrowserWorkerError):
                raise
            self._failure_count += 1
            self._last_error = type(exc).__name__
            raise BrowserWorkerError() from exc

    def _session(self, session_id: str) -> _PlaywrightSession:
        session = self._sessions.get(session_id)
        if session is None:
            raise BrowserWorkerError("Doubao 浏览器登录会话不存在")
        return session

    async def poll_qr_login(self, session_id: str) -> BrowserEvent:
        await self._ensure_running()
        session = self._session(session_id)
        if self._clock() - session.created_at >= self.config.session_ttl_seconds:
            await self._close_session(session_id, session)
            return BrowserEvent(
                session_id=session_id,
                account_id=session.account_id,
                status="expired",
            )
        try:
            authenticated = await self._selector_exists(
                session.page, self.config.authenticated_selector
            )
            qr_code, qr_image = await self._qr_value(session.page)
            if authenticated:
                return await self._complete_session(session_id, session, "confirmed")
            return BrowserEvent(
                session_id=session_id,
                account_id=session.account_id,
                status="waiting_scan" if qr_code else "scanned",
                qr_code=qr_code,
                qr_image_base64=qr_image,
            )
        except BrowserWorkerError:
            raise
        except Exception as exc:
            self._failure_count += 1
            self._last_error = type(exc).__name__
            raise BrowserWorkerError() from exc

    async def complete_qr_login(self, session_id: str) -> BrowserEvent:
        await self._ensure_running()
        session = self._session(session_id)
        if self._clock() - session.created_at >= self.config.session_ttl_seconds:
            await self._close_session(session_id, session)
            return BrowserEvent(
                session_id=session_id,
                account_id=session.account_id,
                status="expired",
            )
        try:
            authenticated = await self._selector_exists(
                session.page, self.config.authenticated_selector
            )
            if not authenticated:
                return BrowserEvent(
                    session_id=session_id,
                    account_id=session.account_id,
                    status="scanned",
                )
            return await self._complete_session(session_id, session, "confirmed")
        except BrowserWorkerError:
            raise
        except Exception as exc:
            self._failure_count += 1
            self._last_error = type(exc).__name__
            raise BrowserWorkerError() from exc

    async def _complete_session(
        self, session_id: str, session: _PlaywrightSession, status: str
    ) -> BrowserEvent:
        credentials: Mapping[str, Any] = {}
        storage_state = getattr(session.context, "storage_state", None)
        if callable(storage_state):
            try:
                # Keep the state in memory only.  The provisioner immediately
                # passes it to the encrypted CredentialStore; writing a
                # plaintext state.json would bypass that security boundary.
                state = await maybe_await(storage_state())
                if isinstance(state, Mapping):
                    credentials = {"storage_state": dict(state)}
            except Exception:
                # Persisting credentials is owned by CredentialStore.  Returning
                # a confirmed event without a state would create an unusable
                # account, so surface a worker error to the provisioner.
                raise BrowserWorkerError() from None
        await self._close_session(session_id, session)
        return BrowserEvent(
            session_id=session_id,
            account_id=session.account_id,
            status=status,
            credentials=credentials,
        )

    async def _close_session(self, session_id: str, session: _PlaywrightSession) -> None:
        self._sessions.pop(session_id, None)
        try:
            await maybe_await(session.context.close())
        except Exception:
            self._failure_count += 1

    async def cancel_qr_login(self, session_id: str) -> None:
        session = self._sessions.get(session_id)
        if session is not None:
            await self._close_session(session_id, session)

    async def delete_account(self, account_id: str) -> None:
        """Close every live context belonging to an account before deletion."""

        for session_id, session in list(self._sessions.items()):
            if str(session.account_id) == str(account_id):
                await self._close_session(session_id, session)


def build_browser_worker(
    settings_or_config: Any | None = None,
    *,
    playwright_factory: Any | None = None,
) -> BrowserWorker:
    """Create Null or Playwright worker from settings/config without side effects."""

    if isinstance(settings_or_config, BrowserWorkerConfig):
        config = settings_or_config
    else:
        config = BrowserWorkerConfig.from_settings(settings_or_config or object())
    if not config.enabled:
        return NullBrowserWorker()
    return PlaywrightBrowserWorker(config, playwright_factory=playwright_factory)


__all__ = [
    "BrowserChallenge",
    "BrowserEvent",
    "BrowserWorker",
    "BrowserWorkerConfig",
    "build_browser_worker",
    "FakeBrowserWorker",
    "NullBrowserWorker",
    "PlaywrightBrowserWorker",
    "maybe_await",
]
