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

from app.credentials import record_account
from app.ports.credentials import CredentialStore, InMemoryCredentialStore

from .errors import OAuthExpiredError, OAuthStateError, public_error
from .mapper import canonical_account, token_record
from .oauth_client import OAuthClient


@dataclass
class _Session:
    id: str
    flow: str
    state: str
    verifier: str
    expires_at: float
    idempotency_key: str
    email_hint: str = ""
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
    ) -> None:
        self.credential_store = credential_store or InMemoryCredentialStore()
        self.oauth_client = oauth_client or OAuthClient()
        self.session_ttl = max(1, int(session_ttl))
        self._clock = clock or time.time
        self._sessions: dict[str, _Session] = {}
        self._idempotent: dict[str, dict[str, Any]] = {}

    def describe(self):
        from .manifest import CHATGPT_MANIFEST

        return CHATGPT_MANIFEST.account_flows

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
        *, added: int, skipped: int, errors: list[str], accounts: list[dict[str, Any]]
    ) -> dict[str, Any]:
        # Deliberately construct a whitelist response.  A future mapper field
        # cannot accidentally make credentials part of the public DTO.
        return {
            "status": "success" if not errors else ("partial" if added else "error"),
            "added": int(added),
            "skipped": int(skipped),
            "refreshed": 0,
            "errors": list(errors),
            "accounts": accounts,
        }

    async def token_import(
        self,
        payload: Mapping[str, Any],
        idempotency_key: str = "",
    ) -> Mapping[str, Any]:
        idem = idempotency_key.strip()
        if idem and idem in self._idempotent:
            return dict(self._idempotent[idem])
        values: list[Any] = []
        tokens = payload.get("tokens", [])
        accounts = payload.get("accounts", [])
        if isinstance(tokens, list):
            values.extend(tokens)
        if isinstance(accounts, list):
            values.extend(accounts)
        added = skipped = 0
        errors: list[str] = []
        result_accounts: list[dict[str, Any]] = []
        seen: set[str] = set()
        for item in values:
            try:
                record = token_record(item)
            except ValueError:
                errors.append("token item is invalid")
                continue
            fingerprint = str(record["fingerprint"])
            if fingerprint in seen:
                skipped += 1
                continue
            seen.add(fingerprint)
            try:
                account_id = f"token:{fingerprint}"
                stored_ref = await self._call_store(
                    record["credential_ref"],
                    record["credentials"],
                    account_id=account_id,
                )
            except Exception:
                # Store implementation details may contain paths or secret
                # values, so expose only a stable action message.
                errors.append("credential could not be stored")
                continue
            account = canonical_account(record)
            await record_account(self.credential_store, "chatgpt", account, stored_ref)
            result_accounts.append(account)
            added += 1
        result = self._result(added=added, skipped=skipped, errors=errors, accounts=result_accounts)
        if idem:
            self._idempotent[idem] = dict(result)
        return result

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
        if idem and idem in self._idempotent:
            return dict(self._idempotent[idem])
        email_hint = str(payload.get("email_hint") or "").strip()
        request = self.oauth_client.begin(email_hint=email_hint)
        session_id = secrets.token_urlsafe(24)
        session = _Session(
            id=session_id,
            flow=flow,
            state=request.state,
            verifier=request.verifier,
            expires_at=self._clock() + self.session_ttl,
            idempotency_key=idem,
            email_hint=email_hint,
        )
        self._sessions[session_id] = session
        result = {
            "session_id": session_id,
            "status": "waiting_callback",
            "authorize_url": request.authorize_url,
            "expires_in": self.session_ttl,
            "redirect_uri": getattr(getattr(self.oauth_client, "config", None), "redirect_uri", ""),
        }
        if idem:
            self._idempotent[idem] = dict(result)
        return result

    async def poll(self, session_id: str, idempotency_key: str = "") -> Mapping[str, Any]:
        session = self._sessions.get(session_id)
        if session is None:
            raise OAuthStateError("OAuth session not found")
        remaining = max(0, int(session.expires_at - self._clock()))
        if session.consumed and session.result:
            return {"session_id": session_id, "status": "success", "result": dict(session.result)}
        if remaining <= 0:
            return {"session_id": session_id, "status": "expired", "expires_in": 0}
        return {"session_id": session_id, "status": "waiting_callback", "expires_in": remaining}

    async def complete(
        self,
        session_id: str,
        payload: Mapping[str, Any],
        idempotency_key: str,
    ) -> Mapping[str, Any]:
        session = self._sessions.get(session_id)
        if session is None:
            raise OAuthStateError("OAuth session not found")
        if session.consumed:
            if session.result is not None:
                return dict(session.result)
            raise OAuthExpiredError("OAuth session was already completed")
        if session.expires_at <= self._clock():
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
            raise OAuthStateError(public_error(exc)) from exc
        result = await self.token_import(
            {"accounts": [token_payload]},
            idempotency_key or session.idempotency_key,
        )
        session.consumed = True
        session.result = dict(result)
        return result

    async def cancel(self, session_id: str, idempotency_key: str = "") -> None:
        session = self._sessions.get(session_id)
        if session is not None:
            session.consumed = True
            session.result = {"status": "cancelled", "session_id": session_id}

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
