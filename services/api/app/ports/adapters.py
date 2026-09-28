from __future__ import annotations

from collections.abc import AsyncIterator, Mapping
from dataclasses import dataclass, field
from typing import Any, Protocol

from app.domain.channel import ChannelManifest


class UpstreamAdapter(Protocol):
    """Port implemented by a platform client.

    The protocol deliberately keeps transport and persistence details out of
    the application layer.  Existing bridge adapters can satisfy the small
    ``list_models`` portion while the remaining methods are migrated.
    """

    manifest: ChannelManifest

    async def health(self, context: Any) -> Any: ...

    async def list_models(self, context: Any) -> list[Mapping[str, Any]]: ...

    async def invoke(self, request: Any, account: Any) -> Any: ...

    async def invoke_capability(
        self, capability: str, request: Any, account: Any
    ) -> Any: ...

    async def invoke_stream(self, request: Any, account: Any) -> AsyncIterator[Any]: ...

    async def chat(self, request: Any, account: Any) -> Any: ...

    async def chat_stream(self, request: Any, account: Any) -> AsyncIterator[Any]: ...

    def map_error(self, error: Exception) -> Any: ...


@dataclass(frozen=True)
class NormalizedRequest:
    """Channel-neutral request passed from the gateway to an adapter.

    Adapters receive an immutable value rather than a Starlette ``Request`` so
    they can be contract-tested without running the web application.  ``body``
    is kept for adapters that need the exact JSON bytes; ``payload`` contains
    the parsed form used by the built-in HTTP clients.
    """

    model: str
    payload: Mapping[str, Any] = field(default_factory=dict)
    headers: Mapping[str, str] = field(default_factory=dict)
    request_id: str = ""
    stream: bool = False
    body: bytes = b""


@dataclass(frozen=True)
class AdapterContext:
    """Non-secret context supplied by the application composition root."""

    base_url: str = ""
    request_id: str = ""
    metadata: Mapping[str, Any] = field(default_factory=dict)
