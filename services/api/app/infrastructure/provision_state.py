"""Durable state for native account provisioning flows.

Provisioners keep their state-machine objects in memory for fast access, but a
process restart must not make a QR/OAuth session or an idempotent response
unrecoverable.  This store deliberately lives beside the credential store and
encrypts the complete state blob before it reaches SQLite.  The database only
contains routing metadata (channel, operation and expiry) plus ciphertext.
"""

from __future__ import annotations

import base64
import hashlib
import json
import time
from collections.abc import Callable, Mapping
from dataclasses import dataclass
from typing import Any

from cryptography.fernet import Fernet, InvalidToken

from app.infrastructure.db import database, migrate, resolve_db_path


class ProvisionStateError(RuntimeError):
    """Raised when durable provision state cannot be decoded or written."""


def _fernet_key(value: Any) -> bytes:
    """Derive a valid Fernet key without persisting the source secret."""

    raw = value.get_secret_value() if hasattr(value, "get_secret_value") else str(value or "")
    raw = raw.strip()
    if raw:
        try:
            key = raw.encode("ascii")
            Fernet(key)
            return key
        except (ValueError, TypeError):
            pass
    digest = hashlib.sha256((raw or "all2api-development-provision-state-key").encode()).digest()
    return base64.urlsafe_b64encode(digest)


@dataclass(frozen=True)
class ProvisionIdempotency:
    """A replayable operation result and its optional owning session."""

    response: dict[str, Any]
    session_id: str = ""
    expires_at: float | None = None


class ProvisionStateStore:
    """SQLite-backed, encrypted session and idempotency state."""

    def __init__(
        self,
        db_path: str,
        master_key: Any = "",
        *,
        clock: Callable[[], float] | None = None,
    ) -> None:
        self.db_path = str(resolve_db_path(db_path))
        self._fernet = Fernet(_fernet_key(master_key))
        self._clock = clock or time.time
        # The store can be constructed while the registry is being assembled,
        # before the FastAPI lifespan has executed its normal migration.
        migrate(self.db_path)

    def _encrypt(self, value: Mapping[str, Any]) -> str:
        try:
            payload = json.dumps(dict(value), ensure_ascii=False, separators=(",", ":"))
            return self._fernet.encrypt(payload.encode("utf-8")).decode("ascii")
        except (TypeError, ValueError) as exc:
            raise ProvisionStateError("provision state is not JSON serializable") from exc

    def _decrypt(self, value: str) -> dict[str, Any]:
        try:
            payload = self._fernet.decrypt(str(value).encode("ascii"))
            decoded = json.loads(payload.decode("utf-8"))
        except (InvalidToken, UnicodeDecodeError, ValueError, TypeError) as exc:
            raise ProvisionStateError("stored provision state could not be decrypted") from exc
        if not isinstance(decoded, dict):
            raise ProvisionStateError("stored provision state is invalid")
        return decoded

    def save_session(
        self,
        channel: str,
        session_id: str,
        *,
        flow: str,
        status: str,
        created_at: float,
        expires_at: float,
        idempotency_key: str = "",
        state: Mapping[str, Any],
    ) -> None:
        now = self._clock()
        encrypted = self._encrypt(state)
        with database(self.db_path) as conn:
            conn.execute(
                """INSERT INTO provision_sessions
                (channel, session_id, flow, status, idempotency_key, created_at,
                 expires_at, state, updated_at)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
                ON CONFLICT(channel, session_id) DO UPDATE SET
                  flow=excluded.flow, status=excluded.status,
                  idempotency_key=excluded.idempotency_key,
                  created_at=excluded.created_at, expires_at=excluded.expires_at,
                  state=excluded.state, updated_at=excluded.updated_at""",
                (
                    str(channel),
                    str(session_id),
                    str(flow),
                    str(status),
                    str(idempotency_key or ""),
                    float(created_at),
                    float(expires_at),
                    encrypted,
                    now,
                ),
            )

    def load_session(self, channel: str, session_id: str) -> dict[str, Any] | None:
        with database(self.db_path) as conn:
            row = conn.execute(
                """SELECT flow, status, idempotency_key, created_at, expires_at, state
                FROM provision_sessions WHERE channel = ? AND session_id = ?""",
                (str(channel), str(session_id)),
            ).fetchone()
        if row is None:
            return None
        state = self._decrypt(str(row["state"]))
        # The columns are authoritative for expiry/status and the encrypted
        # blob is authoritative for provider-specific private state.
        state.setdefault("id", str(session_id))
        state.setdefault("flow", str(row["flow"]))
        state["status"] = str(row["status"])
        state.setdefault("idempotency_key", str(row["idempotency_key"] or ""))
        state.setdefault("created_at", float(row["created_at"]))
        state["expires_at"] = float(row["expires_at"])
        return state

    def delete_session(self, channel: str, session_id: str) -> None:
        with database(self.db_path) as conn:
            conn.execute(
                "DELETE FROM provision_sessions WHERE channel = ? AND session_id = ?",
                (str(channel), str(session_id)),
            )

    def remember_idempotency(
        self,
        channel: str,
        operation: str,
        key: str,
        response: Mapping[str, Any],
        *,
        session_id: str = "",
        expires_at: float | None = None,
    ) -> None:
        if not str(key or "").strip():
            return
        now = time.time()
        encrypted = self._encrypt(response)
        with database(self.db_path) as conn:
            conn.execute(
                """INSERT INTO provision_idempotency
                (channel, operation, idempotency_key, session_id, expires_at,
                 response, created_at, updated_at)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?)
                ON CONFLICT(channel, operation, idempotency_key) DO UPDATE SET
                  session_id=excluded.session_id, expires_at=excluded.expires_at,
                  response=excluded.response, updated_at=excluded.updated_at""",
                (
                    str(channel),
                    str(operation),
                    str(key),
                    str(session_id or ""),
                    float(expires_at) if expires_at is not None else None,
                    encrypted,
                    now,
                    now,
                ),
            )

    def get_idempotency(
        self,
        channel: str,
        operation: str,
        key: str,
        *,
        now: float | None = None,
    ) -> ProvisionIdempotency | None:
        if not str(key or "").strip():
            return None
        with database(self.db_path) as conn:
            row = conn.execute(
                """SELECT session_id, expires_at, response
                FROM provision_idempotency
                WHERE channel = ? AND operation = ? AND idempotency_key = ?""",
                (str(channel), str(operation), str(key)),
            ).fetchone()
            if row is not None and row["expires_at"] is not None:
                if float(row["expires_at"]) <= (self._clock() if now is None else float(now)):
                    conn.execute(
                        """DELETE FROM provision_idempotency
                        WHERE channel = ? AND operation = ? AND idempotency_key = ?""",
                        (str(channel), str(operation), str(key)),
                    )
                    row = None
        if row is None:
            return None
        return ProvisionIdempotency(
            response=self._decrypt(str(row["response"])),
            session_id=str(row["session_id"] or ""),
            expires_at=float(row["expires_at"]) if row["expires_at"] is not None else None,
        )

    def purge_expired(self, *, before: float | None = None) -> int:
        """Remove old terminal rows; active sessions are never purged here."""

        cutoff = self._clock() if before is None else float(before)
        with database(self.db_path) as conn:
            first = conn.execute(
                "DELETE FROM provision_idempotency "
                "WHERE expires_at IS NOT NULL AND expires_at <= ?",
                (cutoff,),
            ).rowcount
            second = conn.execute(
                """DELETE FROM provision_sessions
                WHERE expires_at <= ? AND status IN
                  ('succeeded', 'cancelled', 'expired', 'failed', 'failed_terminal')""",
                (cutoff,),
            ).rowcount
        return int(first or 0) + int(second or 0)


__all__ = ["ProvisionIdempotency", "ProvisionStateError", "ProvisionStateStore"]
