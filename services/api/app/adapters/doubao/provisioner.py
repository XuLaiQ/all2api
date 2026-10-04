"""Native Doubao profile and QR-login provisioner.

This module contains the channel state machine only.  Browser automation and
credential persistence enter through ports, so the unified service has no
dependency on a legacy upstream service, its HTTP endpoints, or its profile
directory.
"""

from __future__ import annotations

import base64
import hashlib
import inspect
import json
import re
import shutil
import time
import uuid
from collections.abc import Callable, Mapping
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from app.domain.channel import ChannelManifest
from app.infrastructure.credentials import record_account
from app.infrastructure.provision_state import ProvisionStateStore

from .browser import BrowserWorker, maybe_await
from .credentials import (
    CredentialStore,
    MemoryCredentialStore,
    delete_credentials,
    store_credentials,
)
from .errors import (
    BrowserWorkerError,
    CredentialStoreError,
    DoubaoProvisionError,
    InvalidAccountIdError,
    ProfileAlreadyExistsError,
    ProfileNotFoundError,
    ProvisionSessionExpiredError,
    ProvisionSessionNotFoundError,
)
from .manifest import DOUBAO_MANIFEST
from .mapper import map_browser_event, map_profile
from .native_qr import DOUBAO_BASE_URL, DOUBAO_USER_AGENT, NativeDoubaoQrWorker

ACCOUNT_ID_PATTERN = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$")

# Doubao web cookies that prove an authenticated session.  The reference
# client refuses to run without ``sessionid``; the import flow keeps
# the same contract so unusable cookies fail fast at the management API.
REQUIRED_SESSION_COOKIES = ("sessionid", "sessionid_ss")

# Device params produced by QR login / compatible session exports.  They are
# merged into stored credentials so imported accounts behave exactly like
# QR-provisioned ones at the data plane.
_DEVICE_PARAM_KEYS = ("device_id", "web_id", "fp", "msToken")


def _new_account_id() -> str:
    """Create an internal profile id; it is never an operator input."""

    return f"doubao-{uuid.uuid4().hex[:16]}"


def _qr_image_value(value: Any) -> str | None:
    """Extract a renderable QR image from a worker challenge or event.

    Browser workers may return raw bytes, a data URI, or either of the two
    supported field names. The API stores only the normalized base64 payload.
    """

    if isinstance(value, Mapping):
        raw = value.get("qr_image_base64") or value.get("qr_image")
    else:
        raw = getattr(value, "qr_image_base64", None) or getattr(value, "qr_image", None)
    if raw is None:
        return None
    if isinstance(raw, bytes):
        return base64.b64encode(raw).decode("ascii")
    text = str(raw).strip()
    if text.startswith("data:") and "," in text:
        text = text.split(",", 1)[1]
    return text or None


@dataclass(frozen=True)
class DoubaoProfile:
    account_id: str
    name: str
    profile_path: str
    enabled: bool = True
    priority: int = 0
    status: str = "needLogin"


@dataclass
class _QrSession:
    session_id: str
    account_id: str
    flow: str
    created_at: float
    expires_at: float
    status: str = "created"
    qr_code: str | None = None
    qr_image_base64: str | None = None
    credential_ref: str | None = None
    last_result: dict[str, Any] | None = None
    created_profile: bool = False
    # The worker/browser context is intentionally not persisted.  A freshly
    # loaded session must be restored once, while a live session must never be
    # restored during ordinary polling because restoration may issue a new QR.
    worker_session_ready: bool = False


class DoubaoProfileStore:
    """Filesystem profile metadata store owned by this project.

    Only a safe ``meta.json`` and an empty browser directory are created.  No
    source-project profile, cookie, or token is copied.
    """

    def __init__(self, root: str | Path) -> None:
        self.root = Path(root).expanduser()

    def _validate_id(self, account_id: str) -> str:
        account_id = account_id.strip()
        if not ACCOUNT_ID_PATTERN.fullmatch(account_id):
            raise InvalidAccountIdError(account_id)
        return account_id

    def path_for(self, account_id: str) -> Path:
        account_id = self._validate_id(account_id)
        return (self.root / account_id).resolve()

    def exists(self, account_id: str) -> bool:
        return self.path_for(account_id).is_dir()

    def get(self, account_id: str) -> DoubaoProfile:
        path = self.path_for(account_id)
        metadata_path = path / "meta.json"
        if not metadata_path.exists():
            raise ProfileNotFoundError(account_id)
        try:
            metadata = json.loads(metadata_path.read_text(encoding="utf-8"))
        except (OSError, ValueError) as exc:
            raise ProfileNotFoundError(account_id) from exc
        return DoubaoProfile(
            account_id=account_id,
            name=str(metadata.get("name") or account_id),
            profile_path=str(path / "browser"),
            enabled=bool(metadata.get("enabled", True)),
            priority=int(metadata.get("priority", 0) or 0),
            status=str(metadata.get("status") or "needLogin"),
        )

    def create(
        self,
        account_id: str,
        *,
        name: str = "",
        priority: int = 0,
        enabled: bool = True,
    ) -> DoubaoProfile:
        account_id = self._validate_id(account_id)
        path = self.path_for(account_id)
        if path.exists():
            raise ProfileAlreadyExistsError(account_id)
        path.mkdir(parents=True, exist_ok=False)
        browser_path = path / "browser"
        browser_path.mkdir()
        profile = DoubaoProfile(
            account_id=account_id,
            name=name.strip() or account_id,
            profile_path=str(browser_path),
            enabled=bool(enabled),
            priority=int(priority),
        )
        metadata = {
            "id": profile.account_id,
            "name": profile.name,
            "enabled": profile.enabled,
            "priority": profile.priority,
            "status": profile.status,
        }
        temporary = path / "meta.json.tmp"
        try:
            temporary.write_text(
                json.dumps(metadata, ensure_ascii=False, indent=2) + "\n",
                encoding="utf-8",
            )
            temporary.replace(path / "meta.json")
        except OSError:
            # Keep profile creation atomic from the caller's perspective.
            try:
                temporary.unlink(missing_ok=True)
                (path / "meta.json").unlink(missing_ok=True)
                browser_path.rmdir()
                path.rmdir()
            except OSError:
                pass
            raise
        return profile

    def set_enabled(self, account_id: str, enabled: bool) -> DoubaoProfile:
        """Persist the operator enabled flag without exposing browser state."""

        self.get(account_id)
        path = self.path_for(account_id)
        metadata_path = path / "meta.json"
        try:
            metadata = json.loads(metadata_path.read_text(encoding="utf-8"))
            if not isinstance(metadata, dict):
                raise ValueError("profile metadata must be an object")
            metadata["enabled"] = bool(enabled)
            temporary = path / "meta.json.tmp"
            temporary.write_text(
                json.dumps(metadata, ensure_ascii=False, indent=2) + "\n",
                encoding="utf-8",
            )
            temporary.replace(metadata_path)
        except (OSError, ValueError) as exc:
            raise ProfileNotFoundError(account_id) from exc
        return self.get(account_id)

    def set_status(self, account_id: str, status: str) -> DoubaoProfile:
        """Persist the provider status used by the account mapper."""

        current = self.get(account_id)
        path = self.path_for(account_id)
        metadata_path = path / "meta.json"
        try:
            metadata = json.loads(metadata_path.read_text(encoding="utf-8"))
            if not isinstance(metadata, dict):
                raise ValueError("profile metadata must be an object")
            metadata["status"] = str(status or "unknown")
            temporary = path / "meta.json.tmp"
            temporary.write_text(
                json.dumps(metadata, ensure_ascii=False, indent=2) + "\n",
                encoding="utf-8",
            )
            temporary.replace(metadata_path)
        except (OSError, ValueError) as exc:
            raise ProfileNotFoundError(account_id) from exc
        return self.get(current.account_id)

    def delete(self, account_id: str) -> None:
        """Remove a provider-owned profile directory after browser cleanup."""

        path = self.path_for(account_id)
        if path.exists():
            if not path.is_dir():
                raise ProfileNotFoundError(account_id)
            shutil.rmtree(path)


def _value(source: Any, key: str, default: Any = None) -> Any:
    if isinstance(source, Mapping):
        return source.get(key, default)
    return getattr(source, key, default)


def parse_cookie_header(value: str) -> dict[str, str]:
    """Parse a ``Cookie`` request header into a name→value mapping."""

    cookies: dict[str, str] = {}
    for part in str(value or "").split(";"):
        part = part.strip()
        if not part or "=" not in part:
            continue
        name, cookie_value = part.split("=", 1)
        name = name.strip()
        if not name:
            continue
        cookies[name] = cookie_value.strip()
    return cookies


def _normalize_cookie_list(value: Any) -> dict[str, str]:
    """Accept browser-style cookie exports (``[{name, value}, ...]``)."""

    cookies: dict[str, str] = {}
    if not isinstance(value, list):
        return cookies
    for item in value:
        if not isinstance(item, Mapping):
            continue
        name = str(item.get("name") or "").strip()
        if not name:
            continue
        cookies[name] = str(item.get("value") or "").strip()
    return cookies


def _cookie_fingerprint(cookies: Mapping[str, str]) -> str:
    session_value = next(
        (cookies[key] for key in REQUIRED_SESSION_COOKIES if cookies.get(key)),
        "",
    )
    digest = hashlib.sha256(session_value.encode("utf-8")).hexdigest()
    return digest[:12]


class DoubaoProvisioner:
    """AccountProvisioner implementation for profile + QR login flows."""

    def __init__(
        self,
        *,
        profile_root: str | Path = "./data/doubao/profiles",
        browser_worker: BrowserWorker | None = None,
        credential_store: CredentialStore | None = None,
        manifest: ChannelManifest | None = None,
        session_ttl_seconds: float = 300,
        clock: Callable[[], float] = time.time,
        state_store: ProvisionStateStore | None = None,
    ) -> None:
        self.profile_store = DoubaoProfileStore(profile_root)
        self.browser_worker = browser_worker or NativeDoubaoQrWorker(
            session_ttl_seconds=session_ttl_seconds,
            clock=clock,
        )
        self.credential_store = credential_store or MemoryCredentialStore()
        self.manifest = manifest or DOUBAO_MANIFEST
        self.flows = tuple(self.manifest.account_flows)
        self.session_ttl_seconds = max(1.0, float(session_ttl_seconds))
        self._clock = clock
        self.state_store = state_store
        self._sessions: dict[str, _QrSession] = {}
        self._idempotency: dict[tuple[str, str], dict[str, Any]] = {}

    async def startup(self) -> None:
        """Start an explicitly configured worker during application startup."""

        start = getattr(self.browser_worker, "start", None)
        if callable(start):
            try:
                await maybe_await(start())
            except BrowserWorkerError:
                # Keep the gateway available while the channel reports a
                # degraded worker. Provision attempts still fail explicitly;
                # one optional browser must not block other channels at boot.
                return

    async def shutdown(self) -> None:
        """Close browser contexts/processes owned by this provisioner."""

        stop = getattr(self.browser_worker, "stop", None)
        if callable(stop):
            await maybe_await(stop())

    async def worker_health(self) -> Mapping[str, Any]:
        health = getattr(self.browser_worker, "health", None)
        if callable(health):
            value = await maybe_await(health())
            if isinstance(value, Mapping):
                return dict(value)
        return {"status": "unknown", "worker": type(self.browser_worker).__name__}

    async def set_account_enabled(self, account_id: str, enabled: bool) -> None:
        """Keep the local browser profile aligned with admin account state."""

        self.profile_store.set_enabled(account_id, bool(enabled))

    async def delete_account(self, account_id: str) -> None:
        """Cancel active QR work and remove all provider-owned profile files."""

        account_id = self.profile_store._validate_id(account_id)
        for session_id, session in list(self._sessions.items()):
            if session.account_id != account_id:
                continue
            try:
                await maybe_await(self.browser_worker.cancel_qr_login(session_id))
            finally:
                self._sessions.pop(session_id, None)
                if self.state_store is not None:
                    self.state_store.delete_session("doubao", session_id)
        cleanup = getattr(self.browser_worker, "delete_account", None)
        if callable(cleanup):
            await maybe_await(cleanup(account_id))
        self.profile_store.delete(account_id)

    def describe(self):
        return self.flows

    def _idempotent(self, operation: str, key: str) -> dict[str, Any] | None:
        if not key:
            return None
        if self.state_store is not None:
            durable = self.state_store.get_idempotency(
                "doubao", operation, key, now=self._clock()
            )
            if durable is not None:
                return dict(durable.response)
            # A durable TTL has expired; do not let the hot cache revive it.
            self._idempotency.pop((operation, key), None)
            return None
        cached = self._idempotency.get((operation, key))
        return dict(cached) if cached is not None else None

    def _remember(
        self,
        operation: str,
        key: str,
        result: dict[str, Any],
        *,
        session_id: str = "",
        expires_at: float | None = None,
    ) -> dict[str, Any]:
        if key:
            self._idempotency[(operation, key)] = dict(result)
            if self.state_store is not None:
                self.state_store.remember_idempotency(
                    "doubao",
                    operation,
                    key,
                    result,
                    session_id=session_id,
                    expires_at=expires_at,
                )
        return result

    def _new_session(
        self,
        account_id: str,
        flow: str,
        *,
        created_profile: bool = False,
    ) -> _QrSession:
        now = self._clock()
        session = _QrSession(
            session_id=f"doubao-{uuid.uuid4().hex}",
            account_id=account_id,
            flow=flow,
            created_at=now,
            expires_at=now + self.session_ttl_seconds,
            created_profile=created_profile,
        )
        self._sessions[session.session_id] = session
        self._persist(session)
        return session

    @staticmethod
    def _from_state(state: Mapping[str, Any]) -> _QrSession:
        return _QrSession(
            session_id=str(state.get("session_id") or state.get("id") or ""),
            account_id=str(state.get("account_id") or ""),
            flow=str(state.get("flow") or "qr-login"),
            created_at=float(state.get("created_at") or 0),
            expires_at=float(state.get("expires_at") or 0),
            status=str(state.get("status") or "created"),
            qr_code=str(state.get("qr_code")) if state.get("qr_code") is not None else None,
            qr_image_base64=(
                str(state.get("qr_image_base64"))
                if state.get("qr_image_base64") is not None
                else None
            ),
            credential_ref=(
                str(state.get("credential_ref"))
                if state.get("credential_ref") is not None
                else None
            ),
            last_result=(
                dict(state["last_result"])
                if isinstance(state.get("last_result"), Mapping)
                else None
            ),
            created_profile=bool(state.get("created_profile", False)),
        )

    def _persist(self, session: _QrSession) -> None:
        if self.state_store is None:
            return
        self.state_store.save_session(
            "doubao",
            session.session_id,
            flow=session.flow,
            status=session.status,
            created_at=session.created_at,
            expires_at=session.expires_at,
            state={
                "session_id": session.session_id,
                "account_id": session.account_id,
                "flow": session.flow,
                "created_at": session.created_at,
                "expires_at": session.expires_at,
                "status": session.status,
                "qr_code": session.qr_code,
                "qr_image_base64": session.qr_image_base64,
                "credential_ref": session.credential_ref,
                "last_result": session.last_result,
                "created_profile": session.created_profile,
            },
        )

    def _get_session(self, session_id: str) -> _QrSession:
        session = self._sessions.get(session_id)
        if session is None and self.state_store is not None:
            state = self.state_store.load_session("doubao", session_id)
            if state is not None:
                session = self._from_state(state)
                self._sessions[session_id] = session
        if session is None:
            raise ProvisionSessionNotFoundError(session_id)
        if (
            session.status
            not in {"succeeded", "cancelled", "expired", "failed", "captcha"}
            and self._clock() >= session.expires_at
        ):
            session.status = "expired"
            self._persist(session)
            raise ProvisionSessionExpiredError()
        return session

    async def _remove_account_catalog(self, account_id: str) -> None:
        """Best-effort removal of a newly-created local account row."""

        destroy = getattr(self.credential_store, "destroy_account", None)
        if not callable(destroy):
            return
        try:
            result = destroy(f"doubao:{account_id}")
            if inspect.isawaitable(result):
                await result
        except Exception:
            # The row may not have been written yet. Cleanup must not hide the
            # original provision failure or turn it into an opaque 500.
            return

    async def _cleanup_failed_session(self, session: _QrSession) -> None:
        """Remove provider-owned state created by an unsuccessful QR flow."""

        if session.credential_ref:
            try:
                await delete_credentials(self.credential_store, session.credential_ref)
            except Exception:
                pass
        try:
            cleanup = getattr(self.browser_worker, "cancel_qr_login", None)
            if callable(cleanup):
                await maybe_await(cleanup(session.session_id))
        except Exception:
            pass
        if not session.created_profile:
            return
        await self._remove_account_catalog(session.account_id)
        try:
            self.profile_store.delete(session.account_id)
        except Exception:
            pass
        session.created_profile = False

    async def _restore_worker_session(self, session: _QrSession) -> None:
        """Recreate the non-durable browser context after a process restart."""

        has_session = getattr(self.browser_worker, "has_qr_session", None)
        if callable(has_session):
            try:
                if bool(await maybe_await(has_session(session.session_id))):
                    session.worker_session_ready = True
                    return
            except Exception:
                # Let the restore call below produce the worker-specific
                # error, rather than treating an unreliable probe as proof
                # that the existing session is still usable.
                pass
        elif session.worker_session_ready:
            return

        restore = getattr(self.browser_worker, "restore_qr_login", None)
        if not callable(restore):
            session.worker_session_ready = True
            return
        profile = self.profile_store.get(session.account_id)
        challenge = await maybe_await(
            restore(
                session.session_id,
                session.account_id,
                profile.profile_path,
                session.created_at,
            )
        )
        if challenge is None:
            session.worker_session_ready = True
            return
        qr_code = _value(challenge, "qr_code")
        if qr_code:
            session.qr_code = str(qr_code)
        qr_image = _qr_image_value(challenge)
        if qr_image:
            session.qr_image_base64 = qr_image
        status = str(_value(challenge, "status", "waiting_scan") or "waiting_scan")
        if session.status not in {"succeeded", "cancelled", "expired", "failed", "captcha"}:
            session.status = status
        session.worker_session_ready = True
        self._session_result(session)
        self._persist(session)

    async def start(
        self,
        flow: str,
        payload: Mapping[str, Any],
        idempotency_key: str,
    ) -> Mapping[str, Any]:
        if not str(idempotency_key or "").strip():
            raise DoubaoProvisionError(
                "idempotency_key is required",
                "idempotency_key_required",
                422,
            )
        if flow == "create-profile":
            cached = self._idempotent("create-profile", idempotency_key)
            if cached is not None:
                return cached
            account_id = str(payload.get("account_id") or "").strip() or _new_account_id()
            profile = self.profile_store.create(
                account_id,
                name=str(payload.get("name") or ""),
                priority=int(payload.get("priority") or 0),
                enabled=bool(payload.get("enabled", True)),
            )
            result = {
                "status": "succeeded",
                "flow": "create-profile",
                "account": map_profile(profile),
            }
            # Profile creation is itself a successful native onboarding step;
            # catalog it even though credentials are populated by qr-login.
            try:
                await record_account(self.credential_store, "doubao", result["account"], "")
            except Exception:
                try:
                    self.profile_store.delete(account_id)
                except Exception:
                    pass
                await self._remove_account_catalog(account_id)
                raise CredentialStoreError() from None
            return self._remember("create-profile", idempotency_key, result)

        if flow != "qr-login":
            raise DoubaoProvisionError(
                f"Doubao flow {flow!r} is not supported",
                "flow_not_supported",
                422,
            )
        cached = self._idempotent("qr-login", idempotency_key)
        if cached is not None:
            return cached
        account_id = str(payload.get("account_id") or "").strip() or _new_account_id()
        created_profile = False
        try:
            profile = self.profile_store.get(account_id)
        except ProfileNotFoundError:
            # The public account flow is QR-first.  Keep ``create-profile`` as
            # a compatible low-level operation, but make selecting ``qr-login``
            # sufficient for a new account so the UI does not require a hidden
            # two-step setup or a second request from the operator.
            profile = self.profile_store.create(
                account_id,
                name=str(payload.get("name") or account_id),
                priority=int(payload.get("priority") or 0),
                enabled=bool(payload.get("enabled", True)),
            )
            created_profile = True
        session = self._new_session(account_id, flow, created_profile=created_profile)
        try:
            challenge = await maybe_await(
                self.browser_worker.start_qr_login(account_id, profile.profile_path)
            )
        except DoubaoProvisionError:
            await self._cleanup_failed_session(session)
            self._sessions.pop(session.session_id, None)
            if self.state_store is not None:
                self.state_store.delete_session("doubao", session.session_id)
            raise
        except Exception as exc:
            await self._cleanup_failed_session(session)
            self._sessions.pop(session.session_id, None)
            if self.state_store is not None:
                self.state_store.delete_session("doubao", session.session_id)
            raise BrowserWorkerError() from exc
        challenge_id = str(_value(challenge, "session_id", session.session_id))
        session.qr_code = str(_value(challenge, "qr_code", "") or "")
        session.qr_image_base64 = _qr_image_value(challenge)
        session.status = str(_value(challenge, "status", "waiting_scan") or "waiting_scan")
        session.worker_session_ready = True
        if challenge_id != session.session_id:
            old_session_id = session.session_id
            self._sessions[challenge_id] = session
            self._sessions.pop(old_session_id, None)
            if self.state_store is not None:
                self.state_store.delete_session("doubao", old_session_id)
            session.session_id = challenge_id
        result = self._session_result(session)
        self._persist(session)
        return self._remember(
            "qr-login",
            idempotency_key,
            result,
            session_id=session.session_id,
            expires_at=session.expires_at,
        )

    def _session_result(self, session: _QrSession, event: Any | None = None) -> dict[str, Any]:
        result = {
            "session_id": session.session_id,
            "flow": session.flow,
            "account_id": session.account_id,
            "status": session.status,
            "expires_at": session.expires_at,
            # Keep QR fields stable across waiting/poll responses.  A client
            # can render ``qr_code`` immediately when no image is available.
            "qr_code": session.qr_code or None,
            "qr_image_base64": session.qr_image_base64 or None,
        }
        if session.credential_ref:
            result["credential_ref"] = session.credential_ref
        if event is not None:
            result.update(map_browser_event(event, credential_ref=session.credential_ref))
            result["session_id"] = session.session_id
            result["account_id"] = session.account_id
            result["expires_at"] = session.expires_at
            # Worker statuses (for example ``confirmed``) are normalized to
            # the unified state machine before being exposed to callers.
            result["status"] = session.status
        session.last_result = dict(result)
        return result

    async def _apply_event(self, session: _QrSession, event: Any) -> dict[str, Any]:
        status = str(_value(event, "status", "waiting_scan") or "waiting_scan").lower()
        status_map = {
            "new": "waiting_scan",
            "waiting": "waiting_scan",
            "waiting_scan": "waiting_scan",
            "scanned": "scanned",
            "confirmed": "succeeded",
            "success": "succeeded",
            "succeeded": "succeeded",
            "expired": "expired",
            "cancelled": "cancelled",
            "canceled": "cancelled",
            "captcha": "captcha",
            "error": "failed",
            "failed": "failed",
        }
        session.status = status_map.get(status, "failed")
        qr_code = _value(event, "qr_code")
        if qr_code:
            session.qr_code = str(qr_code)
        qr_image_base64 = _qr_image_value(event)
        if qr_image_base64:
            session.qr_image_base64 = qr_image_base64
        credentials = _value(event, "credentials")
        if session.status == "succeeded" and isinstance(credentials, Mapping):
            try:
                session.credential_ref = await store_credentials(
                    self.credential_store, session.account_id, credentials
                )
                self.profile_store.set_status(session.account_id, "ready")
                account = map_profile(self.profile_store.get(session.account_id))
                await record_account(
                    self.credential_store,
                    "doubao",
                    account,
                    session.credential_ref,
                )
            except Exception as exc:
                session.status = "failed"
                await self._cleanup_failed_session(session)
                raise CredentialStoreError() from exc
        elif session.status in {"expired", "cancelled", "failed", "captcha"}:
            await self._cleanup_failed_session(session)
        result = self._session_result(session, event)
        self._persist(session)
        return result

    async def poll(self, session_id: str, idempotency_key: str = "") -> Mapping[str, Any]:
        try:
            session = self._get_session(session_id)
        except ProvisionSessionExpiredError:
            expired = self._sessions.get(session_id)
            if expired is not None:
                await self._cleanup_failed_session(expired)
            raise
        if (
            session.status in {"succeeded", "cancelled", "expired", "failed", "captcha"}
            and session.last_result
        ):
            return dict(session.last_result)
        try:
            await self._restore_worker_session(session)
        except DoubaoProvisionError:
            raise
        except Exception as exc:
            raise BrowserWorkerError() from exc
        try:
            event = await maybe_await(self.browser_worker.poll_qr_login(session.session_id))
        except DoubaoProvisionError:
            raise
        except Exception as exc:
            raise BrowserWorkerError() from exc
        return await self._apply_event(session, event)

    async def complete(
        self,
        session_id: str,
        payload: Mapping[str, Any],
        idempotency_key: str,
    ) -> Mapping[str, Any]:
        if not str(idempotency_key or "").strip():
            raise DoubaoProvisionError(
                "idempotency_key is required",
                "idempotency_key_required",
                422,
            )
        if self.state_store is not None:
            cached = self.state_store.get_idempotency(
                "doubao", "complete", idempotency_key, now=self._clock()
            )
            if cached is not None and (
                not cached.session_id or cached.session_id == session_id
            ):
                return dict(cached.response)
        try:
            session = self._get_session(session_id)
        except ProvisionSessionExpiredError:
            expired = self._sessions.get(session_id)
            if expired is not None:
                await self._cleanup_failed_session(expired)
            raise
        if session.status in {"succeeded", "cancelled"} and session.last_result:
            return dict(session.last_result)
        try:
            await self._restore_worker_session(session)
        except DoubaoProvisionError:
            raise
        except Exception as exc:
            raise BrowserWorkerError() from exc
        try:
            event = await maybe_await(self.browser_worker.complete_qr_login(session.session_id))
        except DoubaoProvisionError:
            raise
        except Exception as exc:
            raise BrowserWorkerError() from exc
        result = await self._apply_event(session, event)
        return self._remember(
            "complete",
            idempotency_key,
            result,
            session_id=session.session_id,
            expires_at=session.expires_at,
        )

    async def cancel(self, session_id: str, idempotency_key: str = "") -> None:
        try:
            session = self._get_session(session_id)
        except ProvisionSessionExpiredError:
            expired = self._sessions.get(session_id)
            if expired is not None:
                await self._cleanup_failed_session(expired)
            raise
        if session.status in {"cancelled", "succeeded"}:
            return
        try:
            await self._restore_worker_session(session)
            await maybe_await(self.browser_worker.cancel_qr_login(session.session_id))
        except Exception as exc:
            raise BrowserWorkerError() from exc
        session.status = "cancelled"
        await self._cleanup_failed_session(session)
        session.last_result = self._session_result(session)
        self._persist(session)

    async def _credentials_exist(self, account_id: str) -> bool:
        """Return whether the store already holds credentials for this account."""

        reader = getattr(self.credential_store, "read", None)
        if not callable(reader):
            return False
        try:
            value = reader("doubao", account_id)
            if inspect.isawaitable(value):
                value = await value
        except Exception:
            return False
        return isinstance(value, Mapping)

    def _platform_base(self) -> str:
        base = str(getattr(self.browser_worker, "base_url", "") or "").strip()
        return (base or DOUBAO_BASE_URL).rstrip("/")

    def _import_item(self, item: Any, defaults: Mapping[str, Any]) -> dict[str, Any]:
        """Normalize one cookie import entry into profile + credential parts.

        Accepted shapes (mirroring the provider's session export format):
        - a raw ``Cookie`` header string;
        - ``{"cookie": "..."}`` / ``{"Cookie": "..."}``;
        - ``{"cookies": {...}}`` or a browser-style ``{"cookies": [...]}``;
        - a session file ``{"cookies": {...}, "params": {...}}``.
        """

        overrides: Mapping[str, Any] = {}
        cookies: dict[str, str] = {}
        params: dict[str, str] = {}
        if isinstance(item, str):
            cookies = parse_cookie_header(item)
        elif isinstance(item, Mapping):
            overrides = item
            header = item.get("cookie") or item.get("Cookie") or item.get("cookie_header")
            if isinstance(header, str) and header.strip():
                cookies = parse_cookie_header(header)
            raw_cookies = item.get("cookies")
            if isinstance(raw_cookies, Mapping):
                cookies.update(
                    {
                        str(key).strip(): str(value).strip()
                        for key, value in raw_cookies.items()
                        if str(key).strip()
                    }
                )
            elif isinstance(raw_cookies, list):
                cookies.update(_normalize_cookie_list(raw_cookies))
            raw_params = item.get("params")
            if isinstance(raw_params, Mapping):
                params = {
                    str(key): str(value)
                    for key, value in raw_params.items()
                    if value is not None
                }
        else:
            raise ValueError("账号条目必须是 Cookie 字符串或 JSON 对象")
        if not cookies:
            raise ValueError("未解析到任何 Cookie")
        if not any(cookies.get(key) for key in REQUIRED_SESSION_COOKIES):
            raise ValueError("Cookie 缺少 sessionid，请确认已从登录后的 doubao.com 复制")

        account_id = str(
            overrides.get("account_id") or defaults.get("account_id") or ""
        ).strip()
        if not account_id:
            account_id = f"cookie-{_cookie_fingerprint(cookies)}"
        # Reuse the profile store's validation so import and QR login accept
        # exactly the same account id alphabet.
        account_id = self.profile_store._validate_id(account_id)
        name = str(overrides.get("name") or defaults.get("name") or "").strip()
        priority_raw = overrides.get("priority", defaults.get("priority", 0))
        try:
            priority = int(priority_raw or 0)
        except (TypeError, ValueError):
            priority = 0
        enabled_raw = overrides.get("enabled", defaults.get("enabled", True))
        enabled = bool(enabled_raw)

        base = self._platform_base()
        cookie_header = "; ".join(f"{key}={value}" for key, value in cookies.items())
        credentials: dict[str, Any] = {
            "Cookie": cookie_header,
            "cookies": dict(cookies),
            "msToken": str(cookies.get("msToken") or params.get("msToken") or ""),
            "User-Agent": str(params.get("User-Agent") or DOUBAO_USER_AGENT),
            "Origin": base,
            "Referer": f"{base}/chat/login",
        }
        for key in _DEVICE_PARAM_KEYS:
            value = params.get(key) or cookies.get(key)
            if value:
                credentials[key] = str(value)
        return {
            "account_id": account_id,
            "name": name or account_id,
            "priority": priority,
            "enabled": enabled,
            "fingerprint": _cookie_fingerprint(cookies),
            "credentials": credentials,
        }

    @staticmethod
    def _import_result(
        *,
        added: int,
        skipped: int,
        refreshed: int,
        errors: list[str],
        accounts: list[dict[str, Any]],
    ) -> dict[str, Any]:
        # Whitelist response: credential material never reaches the DTO.
        return {
            "status": (
                "success"
                if not errors
                else ("partial" if added or refreshed else "error")
            ),
            "added": int(added),
            "skipped": int(skipped),
            "refreshed": int(refreshed),
            "errors": list(errors),
            "accounts": accounts,
        }

    async def import_accounts(
        self,
        payload: Mapping[str, Any],
        idempotency_key: str,
    ) -> Mapping[str, Any]:
        if not str(idempotency_key or "").strip():
            raise DoubaoProvisionError(
                "idempotency_key is required",
                "idempotency_key_required",
                422,
            )
        cached = self._idempotent("cookie-import", idempotency_key)
        if cached is not None:
            return cached

        if not isinstance(payload, Mapping):
            raise DoubaoProvisionError(
                "导入数据必须是 JSON 对象",
                code="invalid_request",
                status_code=422,
            )
        items: list[Any] = []
        defaults: Mapping[str, Any] = payload
        raw_accounts = payload.get("accounts")
        if isinstance(raw_accounts, list):
            items.extend(raw_accounts)
        if str(payload.get("cookie") or "").strip():
            items.append(str(payload["cookie"]).strip())
        if isinstance(payload.get("cookies"), (Mapping, list)):
            items.append(
                {"cookies": payload["cookies"], "params": payload.get("params")}
            )
        if not items:
            raise DoubaoProvisionError(
                "cookie 或 accounts 至少提供一项",
                code="invalid_request",
                status_code=422,
            )

        added = skipped = refreshed = 0
        errors: list[str] = []
        accounts: list[dict[str, Any]] = []
        seen: set[str] = set()
        for index, item in enumerate(items, start=1):
            try:
                parsed = self._import_item(item, defaults)
            except InvalidAccountIdError:
                errors.append(f"第 {index} 条账号 ID 只能包含字母、数字、下划线和短横线")
                continue
            except ValueError as exc:
                errors.append(f"第 {index} 条账号数据无效：{exc}")
                continue
            fingerprint = str(parsed["fingerprint"])
            if fingerprint in seen:
                skipped += 1
                continue
            seen.add(fingerprint)
            account_id = str(parsed["account_id"])
            # Re-importing a known session rotates credentials instead of
            # resetting the operator-managed account state.
            existing = self.profile_store.exists(account_id) and (
                await self._credentials_exist(account_id)
            )
            profile_exists = self.profile_store.exists(account_id)
            created_profile = False
            try:
                if profile_exists:
                    self.profile_store.get(account_id)
                else:
                    self.profile_store.create(
                        account_id,
                        name=str(parsed["name"]),
                        priority=int(parsed["priority"]),
                        enabled=bool(parsed["enabled"]),
                    )
                created_profile = not profile_exists
            except (ProfileAlreadyExistsError, ProfileNotFoundError, InvalidAccountIdError):
                errors.append(f"第 {index} 条账号 profile 创建失败")
                continue
            try:
                credential_ref = await store_credentials(
                    self.credential_store, account_id, parsed["credentials"]
                )
            except Exception:
                if created_profile:
                    try:
                        self.profile_store.delete(account_id)
                    except Exception:
                        pass
                errors.append(f"第 {index} 条账号凭据保存失败")
                continue
            try:
                self.profile_store.set_status(account_id, "ready")
                account = map_profile(self.profile_store.get(account_id))
                if not existing:
                    await record_account(
                        self.credential_store,
                        "doubao",
                        account,
                        credential_ref,
                    )
            except Exception:
                if not existing:
                    try:
                        await delete_credentials(self.credential_store, credential_ref)
                    except Exception:
                        pass
                if created_profile:
                    try:
                        self.profile_store.delete(account_id)
                    except Exception:
                        pass
                    await self._remove_account_catalog(account_id)
                errors.append(f"第 {index} 条账号目录保存失败")
                continue
            if existing:
                refreshed += 1
            else:
                added += 1
            accounts.append(account)
        result = self._import_result(
            added=added,
            skipped=skipped,
            refreshed=refreshed,
            errors=errors,
            accounts=accounts,
        )
        return self._remember("cookie-import", idempotency_key, result)


__all__ = ["ACCOUNT_ID_PATTERN", "DoubaoProfile", "DoubaoProfileStore", "DoubaoProvisioner"]
