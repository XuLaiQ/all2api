from __future__ import annotations

from collections.abc import Mapping
from typing import Any, Protocol

from app.domain.channel import ChannelManifest


class ChannelRegistration(Protocol):
    manifest: ChannelManifest
    provisioner: Any
    runtime: Any


class ChannelRegistry(Protocol):
    def get(self, slug: str) -> ChannelRegistration | None: ...

    def values(self) -> Any: ...

    def __iter__(self): ...


RegistryMapping = Mapping[str, ChannelRegistration]
