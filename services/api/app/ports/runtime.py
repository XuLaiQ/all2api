from __future__ import annotations

from collections.abc import Mapping
from typing import Any, Protocol


class HttpClient(Protocol):
    async def get(self, url: str, **kwargs: Any) -> Any: ...

    async def post(self, url: str, **kwargs: Any) -> Any: ...


class CredentialStore(Protocol):
    async def atomic_write(
        self, channel: str, account_id: str, credentials: Mapping[str, Any]
    ) -> Any: ...
