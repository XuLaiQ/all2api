from __future__ import annotations

import asyncio
import secrets
import time
from collections.abc import Callable, Mapping
from dataclasses import dataclass
from typing import Any

from app.adapters.workbuddy.client import WorkBuddyClient
from app.adapters.workbuddy.errors import (
    WorkBuddyCredentialStoreError,
    WorkBuddyInvalidRequestError,
    WorkBuddyRealmMismatchError,
    WorkBuddySessionExpiredError,
    WorkBuddySessionNotFoundError,
    WorkBuddyStateError,
)
from app.adapters.workbuddy.manifest import WORKBUDDY_FLOW, WORKBUDDY_MANIFEST
from app.adapters.workbuddy.mapper import (
    credential_record,
    map_account,
    normalize_realm,
    session_view,
)
from app.credentials import record_account
from app.domain.channel import ProvisionFlowSpec


@dataclass
class _Session:
    id: str
    flow: str
    realm: str
    provider_state: str
    auth_url: str
    created_at: float
    expires_at: float
    idempotency_key: str
    status: str = "waiting_user"
    ready: dict[str, Any] | None = None
    result: dict[str, Any] | None = None
    last_poll_at: float = 0.0
    complete_idempotency_key: str = ""


class MemoryCredentialStore:
    """Safe test/development store implementing the credential port.

    Production wiring should provide the encrypted store.  The provisioner never
    serializes this object's values into its API result.
    """

    def __init__(self) -> None:
        self.records: dict[tuple[str, str], dict[str, Any]] = {}

    async def atomic_write(
        self, channel: str, account_id: str, credentials: Mapping[str, Any]
    ) -> str:
        self.records[(channel, account_id)] = dict(credentials)
        return f"{channel}:{account_id}"


class WorkBuddyProvisioner:
    """Native WorkBuddy QR OAuth state machine.

    Sessions are intentionally short-lived and in-memory.  Credential persistence is
    delegated to ``CredentialStore.atomic_write``; no source project storage layout or
    management endpoint is consulted.
    """

    flows: tuple[ProvisionFlowSpec, ...] = WORKBUDDY_MANIFEST.account_flows

    def __init__(
        self,
        client: WorkBuddyClient,
        *,
        credential_store: Any | None = None,
        clock: Callable[[], float] | None = None,
        ttl_seconds: int = 300,
        min_poll_interval: float = 0.0,
    ) -> None:
        self.client = client
        self.credential_store = credential_store or MemoryCredentialStore()
        self.clock = clock or time.time
        self.ttl_seconds = max(1, int(ttl_seconds))
        self.min_poll_interval = max(0.0, float(min_poll_interval))
        self._sessions: dict[str, _Session] = {}
        self._start_idempotency: dict[str, str] = {}
        self._lock = asyncio.Lock()

    @classmethod
    def from_settings(
        cls, settings: Any, *, credential_store: Any | None = None
    ) -> WorkBuddyProvisioner:
        platform_key = getattr(settings, "wb_platform_data_key", None)
        key = (
            platform_key.get_secret_value()
            if hasattr(platform_key, "get_secret_value")
            else str(platform_key or "")
        )
        if not key:
            legacy_key = getattr(settings, "wb_data_key", "")
            key = (
                legacy_key.get_secret_value()
                if hasattr(legacy_key, "get_secret_value")
                else str(legacy_key or "")
            )
        # Native onboarding talks to WorkBuddy's public platform.  The
        # migration-era ``wb_upstream_base`` points at the old project's
        # management service and must never be used by this provisioner.
        platform_base = str(
            getattr(settings, "wb_platform_base", "https://copilot.tencent.com")
            or "https://copilot.tencent.com"
        )
        return cls(
            WorkBuddyClient(platform_base, data_key=key),
            credential_store=credential_store,
        )

    def describe(self) -> tuple[ProvisionFlowSpec, ...]:
        return self.flows

    def _now(self) -> float:
        return float(self.clock())

    def _get(self, session_id: str) -> _Session:
        session = self._sessions.get(session_id)
        if session is None:
            raise WorkBuddySessionNotFoundError("provision session was not found")
        if (
            session.status not in {"succeeded", "cancelled", "expired", "failed_terminal"}
            and self._now() >= session.expires_at
        ):
            session.status = "expired"
            raise WorkBuddySessionExpiredError("provision session has expired")
        return session

    @staticmethod
    def _validate_start(flow: str, payload: Mapping[str, Any]) -> tuple[str, str]:
        if flow != WORKBUDDY_FLOW:
            raise WorkBuddyInvalidRequestError(f"unsupported WorkBuddy flow: {flow}")
        unknown = set(payload) - {"realm", "region"}
        if unknown:
            raise WorkBuddyInvalidRequestError(f"unknown payload field: {sorted(unknown)[0]}")
        realm = normalize_realm(payload.get("realm", "cn"))
        region = str(payload.get("region", "") or "").strip()
        if len(region) > 16:
            raise WorkBuddyInvalidRequestError("region exceeds 16 characters")
        if realm == "cn" and region:
            raise WorkBuddyInvalidRequestError("region is only valid for the global realm")
        return realm, region

    async def start(
        self,
        flow: str,
        payload: Mapping[str, Any],
        idempotency_key: str,
    ) -> Mapping[str, Any]:
        if not idempotency_key or not str(idempotency_key).strip():
            raise WorkBuddyInvalidRequestError("idempotency_key is required")
        realm, _region = self._validate_start(flow, payload)
        async with self._lock:
            existing_id = self._start_idempotency.get(idempotency_key)
            if existing_id:
                return self._view(self._get(existing_id))
            started = await self.client.start_qr(realm)
            now = self._now()
            session_id = f"wb_{secrets.token_urlsafe(18)}"
            session = _Session(
                id=session_id,
                flow=flow,
                realm=realm,
                provider_state=str(started["state"]),
                auth_url=str(started["auth_url"]),
                created_at=now,
                expires_at=now + self.ttl_seconds,
                idempotency_key=idempotency_key,
            )
            self._sessions[session_id] = session
            self._start_idempotency[idempotency_key] = session_id
            return self._view(session)

    async def poll(self, session_id: str, idempotency_key: str = "") -> Mapping[str, Any]:
        session = self._get(session_id)
        if session.status == "succeeded":
            return dict(session.result or self._view(session))
        if session.status in {"cancelled", "expired", "failed_terminal"}:
            return self._view(session)
        if session.status == "validating" and session.ready:
            return self._view(session)
        now = self._now()
        if self.min_poll_interval and now - session.last_poll_at < self.min_poll_interval:
            return self._view(
                session,
                retry_after=max(1, int(self.min_poll_interval - (now - session.last_poll_at))),
            )
        session.last_poll_at = now
        result = await self.client.poll_qr(session.provider_state, session.realm)
        status = str(result.get("status") or "waiting")
        if status in {"waiting", "pending"}:
            session.status = "waiting_callback"
            return self._view(session, retry_after=2)
        if status == "expired":
            session.status = "expired"
            return self._view(session)
        if status != "ready":
            session.status = "failed_retryable"
            return self._view(session)
        result_realm = normalize_realm(result.get("realm", session.realm))
        if result_realm != session.realm:
            raise WorkBuddyRealmMismatchError("WorkBuddy response realm does not match the session")
        session.ready = dict(result)
        session.status = "validating"
        account = map_account(result, realm=session.realm)
        return self._view(session, account=account, status="ready", next_step="complete")

    async def complete(
        self,
        session_id: str,
        payload: Mapping[str, Any],
        idempotency_key: str,
    ) -> Mapping[str, Any]:
        if not idempotency_key or not str(idempotency_key).strip():
            raise WorkBuddyInvalidRequestError("idempotency_key is required")
        if payload:
            unknown = set(payload) - {"region"}
            if unknown:
                raise WorkBuddyInvalidRequestError(f"unknown payload field: {sorted(unknown)[0]}")
        session = self._get(session_id)
        if session.status == "succeeded":
            return dict(session.result or self._view(session))
        if session.status != "validating" or not session.ready:
            raise WorkBuddyStateError("WorkBuddy QR session is not ready to complete")
        account = map_account(session.ready, realm=session.realm)
        credentials = credential_record(session.ready, realm=session.realm)
        try:
            credential_ref = await self.credential_store.atomic_write(
                "wb", account["native_id"], credentials
            )
            await record_account(
                self.credential_store,
                "wb",
                account,
                str(credential_ref),
            )
        except Exception as exc:
            raise WorkBuddyCredentialStoreError("could not persist WorkBuddy credentials") from exc
        session.status = "succeeded"
        session.complete_idempotency_key = idempotency_key
        session.result = self._view(session, account=account, status="succeeded", next_step="done")
        session.ready = None
        return dict(session.result)

    async def cancel(self, session_id: str, idempotency_key: str = "") -> None:
        session = self._get(session_id)
        if session.status in {"succeeded", "expired"}:
            return
        session.status = "cancelled"
        session.ready = None

    def _view(
        self,
        session: _Session,
        *,
        account: Mapping[str, Any] | None = None,
        status: str | None = None,
        next_step: str | None = None,
        retry_after: int | None = None,
    ) -> dict[str, Any]:
        return session_view(
            session_id=session.id,
            status=status or session.status,
            realm=session.realm,
            expires_at=session.expires_at,
            auth_url=session.auth_url
            if session.status in {"waiting_user", "waiting_callback"}
            else "",
            account=account,
            next_step=next_step,
            retry_after=retry_after,
        )
