"""Declarative M1 provisioner boundary.

The bridge functions in :mod:`app.adapters.provisioning` are retained for the
legacy onboarding endpoints during migration.  New application code receives
one of these provisioner objects and can query its schema without importing or
calling another project's API.  Channel-specific implementations can replace
this object in the registry as they are migrated.
"""

from __future__ import annotations

from collections.abc import Mapping
from typing import Any

from app.domain.channel import ProvisionFlowSpec


class ProvisioningUnsupportedError(NotImplementedError):
    """Raised until a channel's native in-process flow is migrated."""


class DeclarativeProvisioner:
    """M1 provisioner that exposes schema and an explicit native boundary."""

    def __init__(self, flows: tuple[ProvisionFlowSpec, ...]):
        self.flows = tuple(flows)

    def describe(self) -> tuple[ProvisionFlowSpec, ...]:
        return self.flows

    async def start(
        self,
        flow: str,
        payload: Mapping[str, Any],
        idempotency_key: str,
    ) -> Mapping[str, Any]:
        raise ProvisioningUnsupportedError(
            "native account provisioning for this channel is not migrated yet"
        )

    async def poll(self, session_id: str, idempotency_key: str = "") -> Mapping[str, Any]:
        raise ProvisioningUnsupportedError(
            "native account provisioning for this channel is not migrated yet"
        )

    async def complete(
        self,
        session_id: str,
        payload: Mapping[str, Any],
        idempotency_key: str,
    ) -> Mapping[str, Any]:
        raise ProvisioningUnsupportedError(
            "native account provisioning for this channel is not migrated yet"
        )

    async def import_accounts(
        self,
        payload: Mapping[str, Any],
        idempotency_key: str,
    ) -> Mapping[str, Any]:
        raise ProvisioningUnsupportedError(
            "native account provisioning for this channel is not migrated yet"
        )

    async def cancel(self, session_id: str, idempotency_key: str = "") -> None:
        raise ProvisioningUnsupportedError(
            "native account provisioning for this channel is not migrated yet"
        )
