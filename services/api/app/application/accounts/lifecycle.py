"""Account enable/disable and destruction use cases.

The service coordinates provider-owned cleanup with the local encrypted store.
Provider hooks receive only the channel-native account identifier; credentials
remain inside the credential store and never cross this boundary.
"""

from __future__ import annotations

import inspect
from collections.abc import Mapping
from typing import Any

from app.infrastructure.credentials import AccountNotFoundError, DatabaseCredentialStore


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

    async def delete_many(
        self,
        account_ids: list[str],
        *,
        actor: str = "system",
        ip: str = "",
    ) -> dict[str, Any]:
        """Delete several accounts best-effort and report per-id outcomes.

        A missing or half-deleted account must never abort the remaining ids,
        so every failure is captured in ``failed`` with a stable reason.
        """

        deleted: list[dict[str, Any]] = []
        failed: list[dict[str, Any]] = []
        for account_id in account_ids:
            try:
                result = await self.delete(account_id, actor=actor, ip=ip)
            except AccountNotFoundError:
                failed.append({"id": str(account_id), "error": "not_found"})
            except AccountLifecycleError as exc:
                failed.append(
                    {"id": str(account_id), "error": str(exc) or "provider_cleanup_failed"}
                )
            else:
                deleted.append(
                    {
                        "id": str(result.get("id") or account_id),
                        "channel": str(result.get("channel") or ""),
                        "credentials_deleted": bool(result.get("credentials_deleted")),
                    }
                )
        return {
            "requested": len(account_ids),
            "deleted": deleted,
            "failed": failed,
        }

    async def refresh(
        self,
        account_id: str,
        *,
        actor: str = "system",
        ip: str = "",
    ) -> dict[str, Any]:
        """Refresh provider credentials without returning secret material."""

        metadata = await self._metadata(account_id)
        adapter = self.registry.get(str(metadata["channel"]))
        provisioner = getattr(adapter, "provisioner", None) if adapter is not None else None
        method = getattr(provisioner, "refresh_credential", None)
        if not callable(method):
            raise AccountLifecycleError("provider credential refresh is unavailable")
        try:
            result = method(str(metadata["native_id"]))
            if inspect.isawaitable(result):
                result = await result
        except Exception as exc:
            raise AccountLifecycleError("provider credential refresh failed") from exc
        if isinstance(result, Mapping):
            safe = dict(result)
            safe.pop("credentials", None)
            safe.pop("token", None)
            safe["id"] = str(account_id)
            safe["channel"] = str(metadata["channel"])
            safe["refreshed"] = True
            await self.store.record_account_refresh(account_id, actor=actor, ip=ip)
            return safe
        await self.store.record_account_refresh(account_id, actor=actor, ip=ip)
        return {
            "id": str(account_id),
            "channel": str(metadata["channel"]),
            "status": "refreshed",
            "refreshed": True,
        }


__all__ = ["AccountLifecycleError", "AccountLifecycleService", "AccountNotFoundError"]
