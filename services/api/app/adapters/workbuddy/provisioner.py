from __future__ import annotations

import asyncio
import inspect
import secrets
import time
from collections.abc import Callable, Mapping
from dataclasses import dataclass
from typing import Any

from app.adapters.workbuddy.client import WorkBuddyClient
from app.adapters.workbuddy.errors import (
    WorkBuddyCredentialStoreError,
    WorkBuddyInvalidRequestError,
    WorkBuddyProtocolError,
    WorkBuddyRealmMismatchError,
    WorkBuddySessionExpiredError,
    WorkBuddySessionNotFoundError,
    WorkBuddyStateError,
)
from app.adapters.workbuddy.manifest import WORKBUDDY_FLOW, WORKBUDDY_MANIFEST
from app.adapters.workbuddy.mapper import (
    credential_record,
    import_record,
    map_account,
    normalize_realm,
    session_view,
)
from app.domain.channel import ProvisionFlowSpec
from app.infrastructure.credentials import record_account
from app.infrastructure.provision_state import ProvisionStateStore


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

    async def read(self, channel: str, account_id: str) -> dict[str, Any] | None:
        value = self.records.get((str(channel), str(account_id)))
        return dict(value) if value is not None else None


class WorkBuddyProvisioner:
    """Native WorkBuddy QR OAuth state machine.

    The hot object cache is process-local, while ``ProvisionStateStore`` keeps the
    encrypted protocol state and replayable responses durable across restarts.
    Credential persistence is delegated to ``CredentialStore.atomic_write``; no
    source project storage layout or management endpoint is consulted.
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
        state_store: ProvisionStateStore | None = None,
    ) -> None:
        self.client = client
        self.credential_store = credential_store or MemoryCredentialStore()
        self.clock = clock or time.time
        self.ttl_seconds = max(1, int(ttl_seconds))
        self.min_poll_interval = max(0.0, float(min_poll_interval))
        self.state_store = state_store
        self._sessions: dict[str, _Session] = {}
        self._start_idempotency: dict[str, str] = {}
        self._import_idempotency: dict[str, dict[str, Any]] = {}
        self._disabled_accounts: set[str] = set()
        self._deleted_accounts: set[str] = set()
        self._lock = asyncio.Lock()

    @classmethod
    def from_settings(
        cls,
        settings: Any,
        *,
        credential_store: Any | None = None,
        state_store: ProvisionStateStore | None = None,
        ttl_seconds: int | None = None,
    ) -> WorkBuddyProvisioner:
        # Native onboarding talks to WorkBuddy's public platform.  The
        # migration-era ``wb_upstream_base`` points at the old project's
        # management service and must never be used by this provisioner.
        platform_base = str(
            getattr(settings, "wb_platform_base", "https://copilot.tencent.com")
            or "https://copilot.tencent.com"
        )
        return cls(
            WorkBuddyClient(platform_base),
            credential_store=credential_store,
            state_store=state_store,
            ttl_seconds=(
                int(ttl_seconds)
                if ttl_seconds is not None
                else int(getattr(settings, "provision_session_ttl_seconds", 300))
            ),
        )

    def describe(self) -> tuple[ProvisionFlowSpec, ...]:
        return self.flows

    async def import_accounts(
        self,
        payload: Mapping[str, Any],
        idempotency_key: str,
    ) -> Mapping[str, Any]:
        """Import administrator-supplied token bundles into local storage.

        This path deliberately accepts explicit payload material only.  It does
        not scan WorkBuddy auth files or call a source project's management API.
        """

        key = str(idempotency_key or "").strip()
        if not key:
            raise WorkBuddyInvalidRequestError("idempotency_key is required")
        if not isinstance(payload, Mapping):
            raise WorkBuddyInvalidRequestError("import payload must be an object")
        unknown = set(payload) - {"accounts"}
        if unknown:
            raise WorkBuddyInvalidRequestError(f"unknown payload field: {sorted(unknown)[0]}")
        cached = self._import_idempotency.get(key)
        if cached is not None:
            return dict(cached)
        if self.state_store is not None:
            durable = self.state_store.get_idempotency("wb", "import", key, now=self._now())
            if durable is not None:
                result = dict(durable.response)
                self._import_idempotency[key] = result
                return result
        items = payload.get("accounts")
        if not isinstance(items, list):
            raise WorkBuddyInvalidRequestError("accounts must be an array")
        added = 0
        skipped = 0
        errors: list[str] = []
        accounts: list[dict[str, Any]] = []
        seen: set[str] = set()
        for item in items:
            try:
                record = import_record(item, now=self._now())
                account = dict(record["account"])
                native_id = str(account["native_id"])
                if native_id in seen:
                    skipped += 1
                    continue
                seen.add(native_id)
                credential_ref = await self.credential_store.atomic_write(
                    "wb", native_id, record["credentials"]
                )
                await record_account(self.credential_store, "wb", account, str(credential_ref))
                account["ext"] = {"credential_ref": str(credential_ref)}
                accounts.append(account)
                added += 1
            except (ValueError, WorkBuddyProtocolError, WorkBuddyInvalidRequestError):
                errors.append("account item is invalid")
            except Exception as exc:
                raise WorkBuddyCredentialStoreError(
                    "could not persist WorkBuddy credentials"
                ) from exc
        result: dict[str, Any] = {
            "status": "success" if not errors else ("partial" if added else "error"),
            "added": added,
            "skipped": skipped,
            "refreshed": 0,
            "errors": errors,
            "accounts": accounts,
        }
        self._import_idempotency[key] = dict(result)
        if self.state_store is not None:
            self.state_store.remember_idempotency(
                "wb",
                "import",
                key,
                result,
                expires_at=self._now() + self.ttl_seconds,
            )
        return result

    def _now(self) -> float:
        return float(self.clock())

    @staticmethod
    def _from_state(state: Mapping[str, Any]) -> _Session:
        return _Session(
            id=str(state.get("id") or ""),
            flow=str(state.get("flow") or WORKBUDDY_FLOW),
            realm=normalize_realm(state.get("realm", "cn")),
            provider_state=str(state.get("provider_state") or ""),
            auth_url=str(state.get("auth_url") or ""),
            created_at=float(state.get("created_at") or 0),
            expires_at=float(state.get("expires_at") or 0),
            idempotency_key=str(state.get("idempotency_key") or ""),
            status=str(state.get("status") or "waiting_user"),
            ready=dict(state["ready"]) if isinstance(state.get("ready"), Mapping) else None,
            result=dict(state["result"]) if isinstance(state.get("result"), Mapping) else None,
            last_poll_at=float(state.get("last_poll_at") or 0),
            complete_idempotency_key=str(state.get("complete_idempotency_key") or ""),
        )

    def _persist(self, session: _Session) -> None:
        if self.state_store is None:
            return
        self.state_store.save_session(
            "wb",
            session.id,
            flow=session.flow,
            status=session.status,
            created_at=session.created_at,
            expires_at=session.expires_at,
            idempotency_key=session.idempotency_key,
            state={
                "id": session.id,
                "flow": session.flow,
                "realm": session.realm,
                "provider_state": session.provider_state,
                "auth_url": session.auth_url,
                "created_at": session.created_at,
                "expires_at": session.expires_at,
                "idempotency_key": session.idempotency_key,
                "status": session.status,
                "ready": session.ready,
                "result": session.result,
                "last_poll_at": session.last_poll_at,
                "complete_idempotency_key": session.complete_idempotency_key,
            },
        )

    def _get(self, session_id: str) -> _Session:
        session = self._sessions.get(session_id)
        if session is None and self.state_store is not None:
            state = self.state_store.load_session("wb", session_id)
            if state is not None:
                session = self._from_state(state)
                self._sessions[session_id] = session
        if session is None:
            raise WorkBuddySessionNotFoundError("provision session was not found")
        if (
            session.status not in {"succeeded", "cancelled", "expired", "failed_terminal"}
            and self._now() >= session.expires_at
        ):
            session.status = "expired"
            self._persist(session)
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
            if self.state_store is not None:
                cached = self.state_store.get_idempotency(
                    "wb", f"start:{flow}", idempotency_key, now=self._now()
                )
                if cached is not None:
                    return dict(cached.response)
            existing_id = self._start_idempotency.get(idempotency_key)
            if existing_id:
                try:
                    return self._view(self._get(existing_id))
                except WorkBuddySessionExpiredError:
                    self._start_idempotency.pop(idempotency_key, None)
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
            self._persist(session)
            if self.state_store is not None:
                self.state_store.remember_idempotency(
                    "wb",
                    f"start:{flow}",
                    idempotency_key,
                    self._view(session),
                    session_id=session.id,
                    expires_at=session.expires_at,
                )
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
        self._persist(session)
        result = await self.client.poll_qr(session.provider_state, session.realm)
        status = str(result.get("status") or "waiting")
        if status in {"waiting", "pending"}:
            session.status = "waiting_callback"
            self._persist(session)
            return self._view(session, retry_after=2)
        if status == "expired":
            session.status = "expired"
            self._persist(session)
            return self._view(session)
        if status != "ready":
            session.status = "failed_retryable"
            self._persist(session)
            return self._view(session)
        result_realm = normalize_realm(result.get("realm", session.realm))
        if result_realm != session.realm:
            raise WorkBuddyRealmMismatchError("WorkBuddy response realm does not match the session")
        session.ready = dict(result)
        session.status = "validating"
        self._persist(session)
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
        if self.state_store is not None:
            cached = self.state_store.get_idempotency(
                "wb", "complete", idempotency_key, now=self._now()
            )
            if cached is not None and (
                not cached.session_id or cached.session_id == session_id
            ):
                return dict(cached.response)
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
        self._persist(session)
        if self.state_store is not None:
            self.state_store.remember_idempotency(
                "wb",
                "complete",
                idempotency_key,
                session.result,
                session_id=session.id,
                expires_at=session.expires_at,
            )
        return dict(session.result)

    async def cancel(self, session_id: str, idempotency_key: str = "") -> None:
        session = self._get(session_id)
        if session.status in {"succeeded", "expired"}:
            return
        session.status = "cancelled"
        session.ready = None
        session.result = self._view(session)
        self._persist(session)

    @staticmethod
    def _native_account_id(account_id: str) -> str:
        value = str(account_id or "").strip()
        return value[3:] if value.startswith("wb:") else value

    async def _read_credentials(self, account_id: str) -> dict[str, Any]:
        reader = getattr(self.credential_store, "read", None)
        if not callable(reader):
            raise WorkBuddyCredentialStoreError("credential store cannot read credentials")
        value = reader("wb", account_id)
        if inspect.isawaitable(value):
            value = await value
        if not isinstance(value, Mapping):
            raise WorkBuddyCredentialStoreError("WorkBuddy credentials were not found")
        return dict(value)

    async def _account_view(
        self,
        account_id: str,
        *,
        credential_ref: str = "",
        enabled: bool = True,
    ) -> dict[str, Any]:
        native_id = self._native_account_id(account_id)
        metadata: Mapping[str, Any] = {}
        reader = getattr(self.credential_store, "account_metadata", None)
        if callable(reader):
            try:
                value = reader(f"wb:{native_id}")
                if inspect.isawaitable(value):
                    value = await value
                if isinstance(value, Mapping):
                    metadata = value
            except Exception:
                # Refresh must remain usable with lightweight test stores and
                # with a catalog row that is being rebuilt concurrently.
                metadata = {}
        realm, separator, uid = native_id.partition(":")
        if not separator:
            realm, uid = "cn", native_id
        return {
            "id": str(metadata.get("id") or f"wb:{native_id}"),
            "channel": "wb",
            "native_id": native_id,
            "name": str(metadata.get("name") or uid or native_id),
            "kind": "oauth",
            "status": "ready",
            "enabled": bool(metadata.get("enabled", enabled)),
            "realm": realm,
            "uid": uid,
            "ext": {"credential_ref": credential_ref},
        }

    async def set_account_enabled(self, account_id: str, enabled: bool) -> None:
        """Track provider-side enablement while the catalog owns the source of truth.

        WorkBuddy has no separate profile file in the native adapter.  Keeping
        this small set lets callers and tests observe the provider hook without
        writing credentials or contacting a management bridge.
        """

        native_id = self._native_account_id(account_id)
        if bool(enabled):
            self._disabled_accounts.discard(native_id)
        else:
            self._disabled_accounts.add(native_id)

    async def delete_account(self, account_id: str) -> None:
        """Cancel account-owned sessions before local catalog destruction."""

        native_id = self._native_account_id(account_id)
        for session_id, session in list(self._sessions.items()):
            account = session.ready or {}
            session_account = ""
            if isinstance(account, Mapping):
                try:
                    session_account = map_account(account, realm=session.realm)["native_id"]
                except Exception:
                    session_account = ""
            if session_account == native_id:
                session.status = "cancelled"
                session.ready = None
                self._sessions.pop(session_id, None)
                if self.state_store is not None:
                    self.state_store.delete_session("wb", session_id)
        self._disabled_accounts.discard(native_id)
        self._deleted_accounts.add(native_id)

    async def refresh_credential(self, account_id: str) -> Mapping[str, Any]:
        """Refresh and atomically replace one account's encrypted token set."""

        native_id = self._native_account_id(account_id)
        if not native_id:
            raise WorkBuddyInvalidRequestError("account_id is required")
        credentials = await self._read_credentials(native_id)
        try:
            refreshed = self.client.refresh_token(credentials, account_id=native_id)
            if inspect.isawaitable(refreshed):
                refreshed = await refreshed
        except WorkBuddyCredentialStoreError:
            raise
        except Exception as exc:
            raise WorkBuddyStateError("WorkBuddy credential refresh failed") from exc
        if not isinstance(refreshed, Mapping):
            raise WorkBuddyStateError("WorkBuddy refresh response is invalid")
        updated = dict(credentials)
        updated.update(dict(refreshed))
        writer = getattr(self.credential_store, "atomic_write", None)
        if not callable(writer):
            raise WorkBuddyCredentialStoreError("credential store cannot write credentials")
        try:
            stored = writer("wb", native_id, updated)
            if inspect.isawaitable(stored):
                stored = await stored
            account = await self._account_view(
                native_id,
                credential_ref=str(stored or ""),
            )
            await record_account(self.credential_store, "wb", account, str(stored or ""))
        except WorkBuddyCredentialStoreError:
            raise
        except Exception as exc:
            raise WorkBuddyCredentialStoreError("could not persist refreshed credentials") from exc
        return {
            "status": "refreshed",
            "account": {key: value for key, value in account.items() if key != "ext"},
            "credential_ref": str(stored or ""),
        }

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
