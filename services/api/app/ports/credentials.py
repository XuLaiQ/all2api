from __future__ import annotations

from collections.abc import Mapping
from typing import Any, Protocol


class CredentialStore(Protocol):
    """Port for encrypted, atomic credential persistence."""

    async def atomic_write(
        self, channel: str, account_id: str, credentials: Mapping[str, Any]
    ) -> Any: ...

    async def read(self, channel: str, account_id: str) -> Mapping[str, Any] | None: ...


class InMemoryCredentialStore:
    """Small process-local implementation used by adapters and contract tests."""

    def __init__(self) -> None:
        self.records: dict[tuple[str, str], dict[str, Any]] = {}

    async def atomic_write(self, *args: Any, **kwargs: Any) -> str:
        channel = str(kwargs.pop("channel", ""))
        if len(args) == 3:
            channel, account_id, credentials = args
        elif len(args) == 2:
            account_id, credentials = args
        else:
            raise TypeError("atomic_write expects account id and credentials")
        key = (str(channel or "default"), str(account_id))
        self.records[key] = dict(credentials)
        return f"{key[0]}:{key[1]}"

    def get(self, channel: str, account_id: str) -> dict[str, Any] | None:
        value = self.records.get((str(channel), str(account_id)))
        return dict(value) if value is not None else None

    async def read(self, channel: str, account_id: str) -> dict[str, Any] | None:
        return self.get(channel, account_id)

    async def delete(self, credential_ref: str) -> None:
        """Remove one test credential using the same port shape as SQLite."""

        target = str(credential_ref)
        for key, _value in list(self.records.items()):
            if f"{key[0]}:{key[1]}" == target:
                self.records.pop(key, None)

    def snapshot(self) -> dict[tuple[str, str], dict[str, Any]]:
        return {key: dict(value) for key, value in self.records.items()}
