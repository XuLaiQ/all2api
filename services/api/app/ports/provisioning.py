from __future__ import annotations

from collections.abc import Mapping
from typing import Any, Protocol

from app.domain.channel import ProvisionFlowSpec


class AccountProvisioner(Protocol):
    """Port for channel-owned account onboarding.

    Implementations must return redacted session/result DTOs.  They must not
    expose credentials or delegate to another project's management API.
    """

    flows: tuple[ProvisionFlowSpec, ...]

    def describe(self) -> tuple[ProvisionFlowSpec, ...]: ...

    async def start(
        self,
        flow: str,
        payload: Mapping[str, Any],
        idempotency_key: str,
    ) -> Mapping[str, Any]: ...

    async def poll(self, session_id: str, idempotency_key: str = "") -> Mapping[str, Any]: ...

    async def complete(
        self,
        session_id: str,
        payload: Mapping[str, Any],
        idempotency_key: str,
    ) -> Mapping[str, Any]: ...

    async def import_accounts(
        self,
        payload: Mapping[str, Any],
        idempotency_key: str,
    ) -> Mapping[str, Any]: ...

    async def cancel(self, session_id: str, idempotency_key: str = "") -> None: ...
