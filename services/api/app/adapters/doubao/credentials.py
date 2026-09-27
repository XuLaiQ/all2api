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

    async def atomic_write(
        self,
        channel: str,
        account_id: str,
        materials: Mapping[str, Any],
    ) -> str:
        return await self.put(account_id, materials)

    async def delete(self, credential_ref: str) -> None:
        self.records.pop(credential_ref, None)


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


__all__ = ["CredentialStore", "MemoryCredentialStore", "delete_credentials", "store_credentials"]
