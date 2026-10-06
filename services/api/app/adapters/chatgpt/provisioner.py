"""In-process ChatGPT token import and OAuth PKCE account provisioner.

The provisioner owns only protocol state and canonical account metadata.  Token
material is passed directly to the application's CredentialStore and is never
included in a session or result DTO.
"""

from __future__ import annotations

import inspect
import secrets
import time
from collections.abc import Callable, Mapping
from dataclasses import dataclass
from typing import Any

from app.infrastructure.credentials import record_account
from app.infrastructure.provision_state import ProvisionStateStore
from app.ports.credentials import CredentialStore, InMemoryCredentialStore

from .errors import OAuthExpiredError, OAuthStateError, public_error
from .mapper import canonical_account, token_record
from .oauth_client import OAuthClient, credential_client_id


@dataclass
class _Session:
    id: str
    flow: str
    state: str
    verifier: str
    created_at: float
    expires_at: float
    idempotency_key: str
    email_hint: str = ""
    status: str = "waiting_callback"
    consumed: bool = False
    result: dict[str, Any] | None = None


class ChatGPTProvisioner:
    """AccountProvisioner implementation for the ChatGPT channel."""

    def __init__(
        self,
        credential_store: CredentialStore | Any | None = None,
        oauth_client: OAuthClient | Any | None = None,
        *,
        session_ttl: int = 600,
        clock: Callable[[], float] | None = None,
        state_store: ProvisionStateStore | None = None,
    ) -> None:
        self.credential_store = credential_store or InMemoryCredentialStore()
        self.oauth_client = oauth_client or OAuthClient()
        self.session_ttl = max(1, int(session_ttl))
        self._clock = clock or time.time
        self.state_store = state_store
        self._sessions: dict[str, _Session] = {}
        self._idempotent: dict[tuple[str, str], dict[str, Any]] = {}
        self._disabled_accounts: set[str] = set()
        self._deleted_accounts: set[str] = set()

    def describe(self):
        from .manifest import CHATGPT_MANIFEST

        return CHATGPT_MANIFEST.account_flows

    @staticmethod
    def _from_state(state: Mapping[str, Any]) -> _Session:
        return _Session(
            id=str(state.get("id") or ""),
            flow=str(state.get("flow") or "oauth-pkce"),
            state=str(state.get("state") or ""),
            verifier=str(state.get("verifier") or ""),
            created_at=float(state.get("created_at") or 0),
            expires_at=float(state.get("expires_at") or 0),
            idempotency_key=str(state.get("idempotency_key") or ""),
            email_hint=str(state.get("email_hint") or ""),
            status=str(state.get("status") or "waiting_callback"),
            consumed=bool(state.get("consumed", False)),
            result=dict(state["result"]) if isinstance(state.get("result"), Mapping) else None,
        )

    def _persist(self, session: _Session) -> None:
        if self.state_store is None:
            return
        self.state_store.save_session(
            "chatgpt",
            session.id,
            flow=session.flow,
            status=session.status,
            created_at=session.created_at,
            expires_at=session.expires_at,
            idempotency_key=session.idempotency_key,
            state={
                "id": session.id,
                "flow": session.flow,
                "state": session.state,
                "verifier": session.verifier,
                "created_at": session.created_at,
                "expires_at": session.expires_at,
                "idempotency_key": session.idempotency_key,
                "email_hint": session.email_hint,
                "status": session.status,
                "consumed": session.consumed,
                "result": session.result,
            },
        )

    def _get_session(self, session_id: str) -> _Session:
        session = self._sessions.get(session_id)
        if session is None and self.state_store is not None:
            state = self.state_store.load_session("chatgpt", session_id)
            if state is not None:
                session = self._from_state(state)
                self._sessions[session_id] = session
        if session is None:
            raise OAuthStateError("OAuth session not found")
        return session

    def _cached(self, operation: str, key: str) -> dict[str, Any] | None:
        if not key:
            return None
        if self.state_store is not None:
            durable = self.state_store.get_idempotency(
                "chatgpt", operation, key, now=self._clock()
            )
            if durable is not None:
                return dict(durable.response)
            # A durable TTL has expired; do not let the hot cache revive it.
            self._idempotent.pop((operation, key), None)
            return None
        cached = self._idempotent.get((operation, key))
        return dict(cached) if cached is not None else None

    def _remember(
        self,
        operation: str,
        key: str,
        result: Mapping[str, Any],
        *,
        session_id: str = "",
        expires_at: float | None = None,
    ) -> dict[str, Any]:
        value = dict(result)
        if key:
            self._idempotent[(operation, key)] = value
            if self.state_store is not None:
                self.state_store.remember_idempotency(
                    "chatgpt",
                    operation,
                    key,
                    value,
                    session_id=session_id,
                    expires_at=expires_at,
                )
        return value

    async def _call_store(
        self,
        reference: str,
        credentials: Mapping[str, Any],
        *,
        account_id: str | None = None,
    ) -> str:
        """Write one record while accepting common encrypted store method names."""

        store = self.credential_store
        method_name = next(
            (
                name
                for name in ("atomic_write", "put", "save", "write")
                if callable(getattr(store, name, None))
            ),
            None,
        )
        method = getattr(store, method_name, None) if method_name else None
        if method is None:
            raise TypeError("credential store does not implement a write operation")
        stored_ref: Any = reference
        storage_id = str(account_id or reference)
        try:
            if method_name == "atomic_write":
                result = method("chatgpt", storage_id, dict(credentials))
            else:
                result = method(storage_id, dict(credentials), channel="chatgpt")
        except TypeError:
            result = method(storage_id, dict(credentials))
        if inspect.isawaitable(result):
            result = await result
        if result is not None:
            stored_ref = result
        return str(stored_ref)

    @staticmethod
    def _result(
        *,
        added: int,
        skipped: int,
        errors: list[str],
        accounts: list[dict[str, Any]],
        refreshed: int = 0,
    ) -> dict[str, Any]:
        # Deliberately construct a whitelist response.  A future mapper field
        # cannot accidentally make credentials part of the public DTO.
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

    async def _credentials_exist(self, account_id: str) -> bool:
        """Return whether the store already holds credentials for this account."""

        reader = getattr(self.credential_store, "read", None)
        if not callable(reader):
            return False
        try:
            value = reader("chatgpt", account_id)
            if inspect.isawaitable(value):
                value = await value
        except Exception:
            return False
        return isinstance(value, Mapping)

    async def token_import(
        self,
        payload: Mapping[str, Any],
        idempotency_key: str = "",
    ) -> Mapping[str, Any]:
        idem = idempotency_key.strip()
        cached = self._cached("token-import", idem)
        if cached is not None:
            return cached
        values: list[Any] = []
        sources: list[Mapping[str, Any]] = [payload]
        exported = payload.get("export")
        if isinstance(exported, Mapping):
            sources.append(exported)
        for source in sources:
            tokens = source.get("tokens", [])
            accounts = source.get("accounts", [])
            if isinstance(tokens, list):
                values.extend(tokens)
            if isinstance(accounts, list):
                values.extend(accounts)
        added = skipped = refreshed = 0
        errors: list[str] = []
        result_accounts: list[dict[str, Any]] = []
        seen: set[str] = set()
        for index, item in enumerate(values, start=1):
            try:
                record = token_record(item)
            except ValueError:
                errors.append(f"第 {index} 条账号数据无效（缺少 access_token）")
                continue
            fingerprint = str(record["fingerprint"])
            if fingerprint in seen:
                skipped += 1
                continue
            seen.add(fingerprint)
            account_id = f"token:{fingerprint}"
            # A re-import of an already known token rotates its credentials
            # instead of resetting the operator-managed account state (enabled
            # flag, status, counters) back to a fresh "ready" row.
            existing = await self._credentials_exist(account_id)
            try:
                stored_ref = await self._call_store(
                    record["credential_ref"],
                    record["credentials"],
                    account_id=account_id,
                )
            except Exception:
                # Store implementation details may contain paths or secret
                # values, so expose only a stable action message.
                errors.append(f"第 {index} 条账号凭据保存失败")
                continue
            account = canonical_account(record)
            if existing:
                refreshed += 1
            else:
                try:
                    await record_account(self.credential_store, "chatgpt", account, stored_ref)
                except Exception:
                    # A catalog write failure must not leave an orphaned token
                    # that can later be mistaken for a provisioned account.
                    delete = getattr(self.credential_store, "delete", None)
                    if callable(delete):
                        cleanup = delete(stored_ref)
                        if inspect.isawaitable(cleanup):
                            await cleanup
                    errors.append(f"第 {index} 条账号目录保存失败")
                    continue
                added += 1
            result_accounts.append(account)
        result = self._result(
            added=added,
            skipped=skipped,
            refreshed=refreshed,
            errors=errors,
            accounts=result_accounts,
        )
        return self._remember("token-import", idem, result)

    async def import_accounts(
        self,
        payload: Mapping[str, Any],
        idempotency_key: str,
    ) -> Mapping[str, Any]:
        return await self.token_import(payload, idempotency_key)

    async def start(
        self,
        flow: str,
        payload: Mapping[str, Any],
        idempotency_key: str,
    ) -> Mapping[str, Any]:
        if flow != "oauth-pkce":
            raise OAuthStateError("unsupported ChatGPT provision flow")
        idem = idempotency_key.strip()
        cached = self._cached("start:oauth-pkce", idem)
        if cached is not None:
            return cached
        email_hint = str(payload.get("email_hint") or "").strip()
        request = self.oauth_client.begin(email_hint=email_hint)
        session_id = secrets.token_urlsafe(24)
        now = self._clock()
        session = _Session(
            id=session_id,
            flow=flow,
            state=request.state,
            verifier=request.verifier,
            created_at=now,
            expires_at=now + self.session_ttl,
            idempotency_key=idem,
            email_hint=email_hint,
        )
        self._sessions[session_id] = session
        self._persist(session)
        result = {
            "session_id": session_id,
            "status": "waiting_callback",
            "authorize_url": request.authorize_url,
            "expires_in": self.session_ttl,
            "redirect_uri": getattr(getattr(self.oauth_client, "config", None), "redirect_uri", ""),
        }
        return self._remember(
            "start:oauth-pkce",
            idem,
            result,
            session_id=session.id,
            expires_at=session.expires_at,
        )

    async def poll(self, session_id: str, idempotency_key: str = "") -> Mapping[str, Any]:
        session = self._get_session(session_id)
        if session.status == "success" and session.result:
            return {"session_id": session_id, "status": "success", "result": dict(session.result)}
        if session.status == "cancelled":
            return {
                "session_id": session_id,
                "status": "success",
                "result": dict(session.result or {}),
            }
        remaining = max(0, int(session.expires_at - self._clock()))
        if session.consumed and session.result:
            return {"session_id": session_id, "status": "success", "result": dict(session.result)}
        if remaining <= 0:
            session.status = "expired"
            self._persist(session)
            return {"session_id": session_id, "status": "expired", "expires_in": 0}
        return {"session_id": session_id, "status": "waiting_callback", "expires_in": remaining}

    async def complete(
        self,
        session_id: str,
        payload: Mapping[str, Any],
        idempotency_key: str,
    ) -> Mapping[str, Any]:
        if self.state_store is not None:
            cached = self.state_store.get_idempotency(
                "chatgpt", "complete", idempotency_key, now=self._clock()
            )
            if cached is not None and (
                not cached.session_id or cached.session_id == session_id
            ):
                return dict(cached.response)
        session = self._get_session(session_id)
        if session.consumed:
            if session.result is not None:
                return dict(session.result)
            raise OAuthExpiredError("OAuth session was already completed")
        if session.expires_at <= self._clock():
            session.status = "expired"
            self._persist(session)
            raise OAuthExpiredError("OAuth session expired")
        callback = str(payload.get("callback") or "").strip()
        values = self.oauth_client.parse_callback(callback) if callback else {
            key: str(value) for key, value in payload.items() if key in {"code", "state", "error"}
        }
        if values.get("error"):
            raise OAuthStateError("OAuth 授权被拒绝")
        if values.get("state") != session.state:
            raise OAuthStateError("OAuth state 校验失败")
        code = values.get("code", "")
        try:
            token_payload = await self.oauth_client.exchange_code(
                code=code,
                verifier=session.verifier,
            )
        except Exception as exc:
            # Marking consumed prevents callback replay after an exchange
            # attempt.  The caller can start a fresh flow when the provider
            # rejects a code.
            session.consumed = True
            session.status = "failed"
            self._persist(session)
            raise OAuthStateError(public_error(exc)) from exc
        result = await self.token_import(
            {"accounts": [token_payload]},
            idempotency_key or session.idempotency_key,
        )
        session.consumed = True
        session.result = dict(result)
        session.status = "success"
        self._persist(session)
        return self._remember(
            "complete",
            idempotency_key or session.idempotency_key,
            result,
            session_id=session.id,
            expires_at=session.expires_at,
        )

    async def cancel(self, session_id: str, idempotency_key: str = "") -> None:
        session = self._sessions.get(session_id)
        if session is None and self.state_store is not None:
            session = self._get_session(session_id)
        if session is not None:
            session.consumed = True
            session.status = "cancelled"
            session.result = {"status": "cancelled", "session_id": session_id}
            self._persist(session)

    @staticmethod
    def _native_account_id(account_id: str) -> str:
        value = str(account_id or "").strip()
        return value[8:] if value.startswith("chatgpt:") else value

    async def _read_credentials(self, account_id: str) -> dict[str, Any]:
        reader = getattr(self.credential_store, "read", None)
        if not callable(reader):
            raise OAuthStateError("credential store cannot read credentials")
        value = reader("chatgpt", account_id)
        if inspect.isawaitable(value):
            value = await value
        if not isinstance(value, Mapping):
            raise OAuthStateError("ChatGPT credentials were not found")
        return dict(value)

    async def _account_view(
        self,
        account_id: str,
        credentials: Mapping[str, Any],
        credential_ref: str,
        *,
        enabled: bool = True,
    ) -> dict[str, Any]:
        native_id = self._native_account_id(account_id)
        metadata: Mapping[str, Any] = {}
        reader = getattr(self.credential_store, "account_metadata", None)
        if callable(reader):
            try:
                value = reader(f"chatgpt:{native_id}")
                if inspect.isawaitable(value):
                    value = await value
                if isinstance(value, Mapping):
                    metadata = value
            except Exception:
                metadata = {}
        name = str(
            metadata.get("name")
            or credentials.get("email")
            or credentials.get("email_address")
            or native_id
        )
        return {
            "id": str(metadata.get("id") or f"chatgpt:{native_id}"),
            "channel": "chatgpt",
            "native_id": native_id,
            "name": name,
            "kind": "oauth",
            "tier": metadata.get("tier"),
            "status": "ready",
            "enabled": bool(metadata.get("enabled", enabled)),
            "quota_used": 0,
            "quota_total": 0,
            "quota_unit": "none",
            "expires_at": credentials.get("expires_at") or credentials.get("expiresAt"),
            "priority": 0,
            "ext": {"credential_ref": str(credential_ref or "")},
        }

    async def set_account_enabled(self, account_id: str, enabled: bool) -> None:
        native_id = self._native_account_id(account_id)
        if not native_id:
            raise OAuthStateError("account_id is required")
        if bool(enabled):
            self._disabled_accounts.discard(native_id)
        else:
            self._disabled_accounts.add(native_id)

    async def delete_account(self, account_id: str) -> None:
        native_id = self._native_account_id(account_id)
        if not native_id:
            raise OAuthStateError("account_id is required")
        for session_id, session in list(self._sessions.items()):
            result = session.result or {}
            accounts = result.get("accounts", []) if isinstance(result, Mapping) else []
            matches = any(
                isinstance(item, Mapping)
                and self._native_account_id(str(item.get("native_id") or "")) == native_id
                for item in accounts
            )
            if matches:
                self._sessions.pop(session_id, None)
                if self.state_store is not None:
                    self.state_store.delete_session("chatgpt", session_id)
        self._disabled_accounts.discard(native_id)
        self._deleted_accounts.add(native_id)

    async def refresh_credential(self, account_id: str) -> Mapping[str, Any]:
        """Refresh one account through OAuth and atomically rotate its tokens."""

        native_id = self._native_account_id(account_id)
        if not native_id:
            raise OAuthStateError("account_id is required")
        credentials = await self._read_credentials(native_id)
        refresh = str(
            credentials.get("refresh_token") or credentials.get("refreshToken") or ""
        ).strip()
        if not refresh:
            raise OAuthStateError("ChatGPT credentials have no refresh token")
        refresher = getattr(self.oauth_client, "refresh_token", None)
        if not callable(refresher):
            raise OAuthStateError("ChatGPT OAuth refresh is unavailable")
        client_id = credential_client_id(credentials)
        try:
            if client_id:
                refreshed = refresher(refresh, client_id=client_id)
            else:
                refreshed = refresher(refresh)
            if inspect.isawaitable(refreshed):
                refreshed = await refreshed
        except Exception as exc:
            raise OAuthStateError("ChatGPT credential refresh failed") from exc
        if not isinstance(refreshed, Mapping) or not str(
            refreshed.get("access_token") or refreshed.get("accessToken") or ""
        ).strip():
            raise OAuthStateError("ChatGPT refresh response is invalid")
        updated = dict(credentials)
        updated.update(dict(refreshed))
        updated.setdefault("refresh_token", refresh)
        try:
            expires_in = float(refreshed.get("expires_in"))
        except (TypeError, ValueError):
            expires_in = 0.0
        if expires_in > 0:
            updated["expires_at"] = int(self._clock() + expires_in)
        stored_ref = await self._call_store("", updated, account_id=native_id)
        account = await self._account_view(native_id, updated, stored_ref)
        await record_account(self.credential_store, "chatgpt", account, stored_ref)
        return {
            "status": "refreshed",
            "account": {key: value for key, value in account.items() if key != "ext"},
            "credential_ref": stored_ref,
        }

    async def oauth_pkce(self, *args: Any, **kwargs: Any) -> Mapping[str, Any]:
        """Compatibility helper for direct adapter callers.

        ``start`` and ``complete`` are the port operations; this method keeps
        the protocol-specific name available to unit tests and extensions.
        """

        if kwargs.get("session_id") or (
            args and isinstance(args[0], str) and args[0] in self._sessions
        ):
            return await self.complete(*args, **kwargs)
        return await self.start("oauth-pkce", *args, **kwargs)


Provisioner = ChatGPTProvisioner
