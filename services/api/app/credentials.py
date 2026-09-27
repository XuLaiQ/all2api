"""Encrypted credential persistence owned by All2API.

Credentials are deliberately kept behind this module.  The ``credentials`` table
contains only a Fernet ciphertext; account catalogue fields and audit records are
derived from the redacted canonical account DTO supplied by a provisioner.
"""

from __future__ import annotations

import base64
import hashlib
import json
import secrets
import time
from collections.abc import Mapping
from typing import Any

from cryptography.fernet import Fernet, InvalidToken

from app.db import database, migrate, resolve_db_path

_SECRET_NAMES = {
    "access_token",
    "accessToken",
    "refresh_token",
    "refreshToken",
    "id_token",
    "idToken",
    "device_token",
    "deviceToken",
    "token",
    "oauth_token",
    "client_secret",
    "cookie",
    "cookies",
    "authorization",
    "password",
    "secret",
}


def _fernet_key(value: Any) -> bytes:
    """Return a valid Fernet key without ever persisting the source secret."""

    raw = value.get_secret_value() if hasattr(value, "get_secret_value") else str(value or "")
    raw = raw.strip()
    if raw:
        try:
            key = raw.encode("ascii")
            Fernet(key)
            return key
        except (ValueError, TypeError):
            pass
    # A deterministic development key keeps local tests usable while production
    # deployments can inject a random A2A_CREDENTIAL_MASTER_KEY.
    digest = hashlib.sha256((raw or "all2api-development-credential-key").encode()).digest()
    return base64.urlsafe_b64encode(digest)


def _public_value(value: Any, *, key: str = "") -> Any:
    """Drop secret-shaped values before writing canonical account metadata."""

    if key in _SECRET_NAMES or key.lower() in {item.lower() for item in _SECRET_NAMES}:
        return None
    if isinstance(value, Mapping):
        result: dict[str, Any] = {}
        for item_key, item_value in value.items():
            clean = _public_value(item_value, key=str(item_key))
            if clean is not None:
                result[str(item_key)] = clean
        return result
    if isinstance(value, (list, tuple)):
        return [_public_value(item) for item in value]
    if isinstance(value, (str, int, float, bool)) or value is None:
        return value
    return str(value)


class CredentialStoreError(RuntimeError):
    """Raised when encrypted credential persistence fails."""


class AccountNotFoundError(CredentialStoreError):
    """Raised when an account lifecycle operation targets an unknown account."""


class DatabaseCredentialStore:
    """SQLite-backed, Fernet-encrypted implementation of the credential port."""

    def __init__(self, db_path: str, master_key: Any = "") -> None:
        self.db_path = str(resolve_db_path(db_path))
        self._fernet = Fernet(_fernet_key(master_key))
        # CredentialStore can be constructed by an adapter in tests or by a
        # startup hook before the application lifespan has run. Ensure the
        # credentials table exists without relying on call-site ordering.
        migrate(self.db_path)

    @staticmethod
    def _reference(channel: str) -> str:
        return f"cred_{channel}_{secrets.token_urlsafe(18)}"

    async def atomic_write(
        self,
        channel: str,
        account_id: str,
        credentials: Mapping[str, Any],
    ) -> str:
        channel = str(channel or "").strip()
        account_id = str(account_id or "").strip()
        if not channel or not account_id or not isinstance(credentials, Mapping):
            raise CredentialStoreError("channel, account id and credentials are required")
        try:
            payload = json.dumps(dict(credentials), ensure_ascii=False, separators=(",", ":"))
            encrypted = self._fernet.encrypt(payload.encode("utf-8")).decode("ascii")
            with database(self.db_path) as conn:
                now = int(time.time())
                existing = conn.execute(
                    "SELECT credential_ref FROM credentials WHERE channel = ? AND account_id = ?",
                    (channel, account_id),
                ).fetchone()
                reference = (
                    str(existing["credential_ref"])
                    if existing
                    else self._reference(channel)
                )
                conn.execute(
                    """INSERT INTO credentials
                    (credential_ref, channel, account_id, encrypted_payload, created_at, updated_at)
                    VALUES (?, ?, ?, ?, ?, ?)
                    ON CONFLICT(channel, account_id) DO UPDATE SET
                    credential_ref=excluded.credential_ref,
                    encrypted_payload=excluded.encrypted_payload,
                    updated_at=excluded.updated_at""",
                    (reference, channel, account_id, encrypted, now, now),
                )
            return reference
        except CredentialStoreError:
            raise
        except Exception as exc:
            raise CredentialStoreError("could not persist encrypted credentials") from exc

    async def read(self, channel: str, account_id: str) -> dict[str, Any] | None:
        """Read credentials for internal adapter use; never used by API DTOs."""

        with database(self.db_path) as conn:
            row = conn.execute(
                "SELECT encrypted_payload FROM credentials WHERE channel = ? AND account_id = ?",
                (str(channel), str(account_id)),
            ).fetchone()
        if row is None:
            return None
        try:
            value = self._fernet.decrypt(str(row["encrypted_payload"]).encode("ascii"))
            result = json.loads(value.decode("utf-8"))
        except (InvalidToken, UnicodeDecodeError, ValueError, TypeError) as exc:
            raise CredentialStoreError("stored credential could not be decrypted") from exc
        if not isinstance(result, dict):
            raise CredentialStoreError("stored credential payload is invalid")
        return result

    async def delete(self, credential_ref: str) -> None:
        with database(self.db_path) as conn:
            conn.execute("DELETE FROM credentials WHERE credential_ref = ?", (str(credential_ref),))

    def _account_row(self, account_id: str):
        with database(self.db_path) as conn:
            row = conn.execute(
                "SELECT id, channel, native_id, name, status, status_override, enabled, ext "
                "FROM accounts WHERE id = ?",
                (str(account_id),),
            ).fetchone()
        if row is None:
            raise AccountNotFoundError("account was not found")
        return row

    async def account_metadata(self, account_id: str) -> dict[str, Any]:
        """Return non-secret metadata needed by lifecycle orchestration."""

        row = self._account_row(account_id)
        return {
            "id": str(row["id"]),
            "channel": str(row["channel"]),
            "native_id": str(row["native_id"]),
            "name": str(row["name"]),
            "enabled": bool(row["enabled"]),
        }

    async def set_account_enabled(
        self,
        account_id: str,
        enabled: bool,
        *,
        actor: str = "system",
        ip: str = "",
    ) -> dict[str, Any]:
        """Toggle an account and keep the operator decision auditable.

        ``status_override`` records an administrative disable without destroying
        the provider reported status.  Clearing the override on re-enable lets
        the next runtime observation become authoritative again.
        """

        row = self._account_row(account_id)
        enabled_value = bool(enabled)
        override = None if enabled_value else "disabled"
        now = int(time.time())
        with database(self.db_path) as conn:
            conn.execute(
                "UPDATE accounts SET enabled = ?, status_override = ?, updated_at = ? "
                "WHERE id = ?",
                (int(enabled_value), override, now, str(account_id)),
            )
            conn.execute(
                "INSERT INTO audit_logs(ts, actor, action, target, detail, ip) "
                "VALUES (?, ?, ?, ?, ?, ?)",
                (
                    now,
                    str(actor or "system")[:128],
                    "enable_account" if enabled_value else "disable_account",
                    str(account_id)[:256],
                    f"channel={str(row['channel'])[:64]};enabled={str(enabled_value).lower()}",
                    str(ip or "")[:64],
                ),
            )
        return {
            "id": str(row["id"]),
            "channel": str(row["channel"]),
            "name": str(row["name"]),
            "status": str(override or row["status"]),
            "enabled": enabled_value,
            "updated_at": now,
        }

    async def destroy_account(
        self,
        account_id: str,
        *,
        actor: str = "system",
        ip: str = "",
    ) -> dict[str, Any]:
        """Destroy an account, its encrypted credentials and runtime state.

        The returned metadata is for internal lifecycle orchestration only.  It
        intentionally contains no credential payload and is never an API DTO.
        """

        row = self._account_row(account_id)
        ext: dict[str, Any] = {}
        try:
            value = json.loads(str(row["ext"] or "{}"))
            if isinstance(value, dict):
                ext = value
        except (TypeError, ValueError):
            ext = {}
        credential_ref = str(ext.get("credential_ref") or "").strip()
        now = int(time.time())
        with database(self.db_path) as conn:
            # Match both the canonical reference and the channel/native key.
            # Profile-only Doubao accounts have no credential_ref yet, while a
            # rotated credential may have a reference that is still present.
            deleted = conn.execute(
                "DELETE FROM credentials WHERE credential_ref = ? "
                "OR (channel = ? AND account_id = ?)",
                (credential_ref, str(row["channel"]), str(row["native_id"])),
            ).rowcount
            conn.execute("DELETE FROM accounts WHERE id = ?", (str(account_id),))
            conn.execute(
                "INSERT INTO audit_logs(ts, actor, action, target, detail, ip) "
                "VALUES (?, ?, ?, ?, ?, ?)",
                (
                    now,
                    str(actor or "system")[:128],
                    "delete_account",
                    str(account_id)[:256],
                    f"channel={str(row['channel'])[:64]};credentials_deleted={int(deleted or 0)}",
                    str(ip or "")[:64],
                ),
            )
        return {
            "id": str(row["id"]),
            "channel": str(row["channel"]),
            "native_id": str(row["native_id"]),
            "credential_ref": credential_ref,
            "credentials_deleted": int(deleted or 0),
        }

    async def upsert_account(
        self,
        channel: str,
        account: Mapping[str, Any],
        credential_ref: str,
        *,
        actor: str = "system",
        ip: str = "",
    ) -> None:
        """Persist a canonical account and emit a redacted provisioning audit."""

        channel = str(channel or account.get("channel") or "").strip()
        native_id = str(account.get("native_id") or "").strip()
        if not channel or not native_id:
            raise CredentialStoreError("canonical account is missing channel or native_id")
        account_id = str(account.get("id") or f"{channel}:{native_id}")
        ext = _public_value(account.get("ext") if isinstance(account.get("ext"), Mapping) else {})
        if not isinstance(ext, dict):
            ext = {}
        ext["credential_ref"] = str(credential_ref)
        now = int(time.time())
        with database(self.db_path) as conn:
            conn.execute(
                """INSERT INTO accounts
                (id, channel, native_id, name, kind, tier, status, enabled, quota_used,
                 quota_total, quota_unit, expires_at, success_count, fail_count, streak,
                 cooldown_until, last_error, priority, ext, created_at, updated_at)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                ON CONFLICT(id) DO UPDATE SET name=excluded.name, kind=excluded.kind,
                tier=excluded.tier, status=excluded.status, enabled=excluded.enabled,
                quota_used=excluded.quota_used, quota_total=excluded.quota_total,
                quota_unit=excluded.quota_unit, expires_at=excluded.expires_at,
                priority=excluded.priority, ext=excluded.ext, updated_at=excluded.updated_at""",
                (
                    account_id,
                    channel,
                    native_id,
                    str(account.get("name") or native_id)[:256],
                    str(account.get("kind") or "account")[:64],
                    account.get("tier"),
                    str(account.get("status") or "ready")[:64],
                    int(bool(account.get("enabled", True))),
                    float(account.get("quota_used") or 0),
                    float(account.get("quota_total") or 0),
                    str(account.get("quota_unit") or "none")[:32],
                    account.get("expires_at"),
                    int(account.get("success_count") or 0),
                    int(account.get("fail_count") or 0),
                    int(account.get("streak") or 0),
                    account.get("cooldown_until"),
                    str(account.get("last_error") or "")[:1000],
                    int(account.get("priority") or 0),
                    json.dumps(ext, ensure_ascii=False, separators=(",", ":")),
                    now,
                    now,
                ),
            )
            conn.execute(
                """INSERT INTO audit_logs
                (ts, actor, action, target, detail, ip) VALUES (?, ?, ?, ?, ?, ?)""",
                (
                    now,
                    str(actor or "system")[:128],
                    "provision_account",
                    account_id[:256],
                    f"channel={channel};credential_ref={str(credential_ref)[:128]}",
                    str(ip or "")[:64],
                ),
            )


async def record_account(
    store: Any,
    channel: str,
    account: Mapping[str, Any],
    credential_ref: str,
    *,
    actor: str = "system",
    ip: str = "",
) -> None:
    """Invoke optional account catalog persistence while preserving test fakes."""

    method = getattr(store, "upsert_account", None)
    if callable(method):
        result = method(channel, account, credential_ref, actor=actor, ip=ip)
        if hasattr(result, "__await__"):
            await result


__all__ = [
    "AccountNotFoundError",
    "CredentialStoreError",
    "DatabaseCredentialStore",
    "record_account",
]
