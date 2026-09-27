"""Account enable/disable and destruction use cases.

The service coordinates provider-owned cleanup with the local encrypted store.
Provider hooks receive only the channel-native account identifier; credentials
remain inside the credential store and never cross this boundary.
"""

from __future__ import annotations

import inspect
from typing import Any

from app.credentials import AccountNotFoundError, DatabaseCredentialStore


class AccountLifecycleError(RuntimeError):
    """Raised when provider cleanup cannot be completed safely."""


class AccountLifecycleService:
    def __init__(self, registry: dict[str, Any], *, db_path: str, master_key: Any = "") -> None:
        self.registry = registry
        self.store = DatabaseCredentialStore(db_path, master_key)

    async def _metadata(self, account_id: str) -> dict[str, Any]:
        return await self.store.account_metadata(account_id)

    async def _provider_hook(
        self,
        metadata: dict[str, Any],
        method_name: str,
        *args: Any,
    ) -> bool:
        adapter = self.registry.get(str(metadata["channel"]))
        provisioner = getattr(adapter, "provisioner", None) if adapter is not None else None
        method = getattr(provisioner, method_name, None)
        if not callable(method):
            return False
        try:
            result = method(str(metadata["native_id"]), *args)
            if inspect.isawaitable(result):
                await result
        except Exception as exc:
            raise AccountLifecycleError("provider account cleanup failed") from exc
        return True

    async def set_enabled(
        self,
        account_id: str,
        enabled: bool,
        *,
        actor: str = "system",
        ip: str = "",
    ) -> dict[str, Any]:
        metadata = await self._metadata(account_id)
        await self._provider_hook(metadata, "set_account_enabled", bool(enabled))
        return await self.store.set_account_enabled(
            account_id,
            bool(enabled),
            actor=actor,
            ip=ip,
        )

    async def delete(
        self,
        account_id: str,
        *,
        actor: str = "system",
        ip: str = "",
    ) -> dict[str, Any]:
        metadata = await self._metadata(account_id)
        await self._provider_hook(metadata, "delete_account")
        return await self.store.destroy_account(account_id, actor=actor, ip=ip)


__all__ = ["AccountLifecycleError", "AccountLifecycleService", "AccountNotFoundError"]
