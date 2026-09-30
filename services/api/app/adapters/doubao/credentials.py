"""CredentialStore port and an in-memory fake for contract tests."""

from __future__ import annotations

import hashlib
import inspect
from collections.abc import Mapping
from typing import Any

from app.ports.credentials import CredentialStore


class MemoryCredentialStore:
    """Small fake store that keeps test credentials out of API responses."""

    def __init__(self) -> None:
        self.records: dict[str, tuple[str, dict[str, Any]]] = {}

    async def put(self, account_id: str, materials: Mapping[str, Any]) -> str:
        digest = hashlib.sha256(
            f"{account_id}:{len(self.records)}".encode()
        ).hexdigest()[:24]
        ref = f"cred_doubao_{digest}"
        self.records[ref] = (account_id, dict(materials))
        return ref

    async def read(self, channel: str, account_id: str) -> dict[str, Any] | None:
        """Look up materials by account id so re-imports can be detected."""

        for _ref, (stored_id, materials) in self.records.items():
            if stored_id == str(account_id):
                return dict(materials)
        return None

    async def atomic_write(
        self,
        channel: str,
        account_id: str,
        materials: Mapping[str, Any],
    ) -> str:
        # Match the durable store contract: one record per account, rewritten
        # in place on conflict so ``read`` always returns the latest materials.
        for ref, (stored_id, _materials) in self.records.items():
            if stored_id == str(account_id):
                self.records[ref] = (str(account_id), dict(materials))
                return ref
        return await self.put(account_id, materials)

    async def delete(self, credential_ref: str) -> None:
        self.records.pop(credential_ref, None)


class NonRetainingCredentialStore:
    """Credential port sink for browser smoke tests.

    The real browser may produce storage state after an authenticated profile
    is opened.  This store accepts the mapping only to complete the provision
    contract, creates a safe reference, and never inspects or retains it.
    """

    def __init__(self) -> None:
        self._writes = 0

    async def atomic_write(
        self,
        channel: str,
        account_id: str,
        _credentials: Mapping[str, Any],
    ) -> str:
        channel = str(channel or "").strip()
        account_id = str(account_id or "").strip()
        if not channel or not account_id:
            raise ValueError("channel and account id are required")
        self._writes += 1
        return f"cred_{channel}_{self._writes}"

    async def read(self, channel: str, account_id: str) -> None:
        return None

    async def delete(self, credential_ref: str) -> None:
        return None


async def store_credentials(
    store: CredentialStore,
    account_id: str,
    materials: Mapping[str, Any],
) -> str:
    """Call a sync or async fake while keeping the port async."""

    atomic_write = getattr(store, "atomic_write", None)
    if callable(atomic_write):
        result = atomic_write("doubao", account_id, materials)
    else:
        put = getattr(store, "put", None)
        if not callable(put):
            raise TypeError("credential store does not implement atomic_write or put")
        result = put(account_id, materials)
    if inspect.isawaitable(result):
        result = await result
    return str(result)


async def delete_credentials(store: CredentialStore, credential_ref: str) -> None:
    result = store.delete(credential_ref)
    if inspect.isawaitable(result):
        await result


__all__ = [
    "CredentialStore",
    "MemoryCredentialStore",
    "NonRetainingCredentialStore",
    "delete_credentials",
    "store_credentials",
]
