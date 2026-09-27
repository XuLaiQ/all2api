from __future__ import annotations

import re
from collections.abc import Mapping
from dataclasses import dataclass, field
from typing import Any

_SLUG_PATTERN = re.compile(r"^[a-z0-9][a-z0-9-]{0,63}$")


@dataclass(frozen=True)
class ProvisionFlowSpec:
    """Declarative description of one account onboarding flow.

    ``schema`` uses the small JSON-schema subset understood by the generic
    application service.  Platform code owns the actual protocol and secrets;
    the schema is safe to expose to an administrator or a form renderer.
    """

    id: str
    kind: str
    schema: Mapping[str, Any] = field(default_factory=dict)
    supports: Mapping[str, bool] = field(default_factory=dict)
    timeout_seconds: int = 300
    idempotency: str = "required"
    requires_admin: bool = True

    def __post_init__(self) -> None:
        flow_id = self.id.strip()
        if not flow_id or len(flow_id) > 64:
            raise ValueError("provision flow id must be between 1 and 64 characters")
        if not self.kind.strip():
            raise ValueError("provision flow kind must not be blank")
        if self.timeout_seconds < 1:
            raise ValueError("provision flow timeout must be positive")
        if self.idempotency not in {"required", "optional", "none"}:
            raise ValueError("unsupported provision flow idempotency policy")

    def as_dict(self) -> dict[str, Any]:
        """Return a JSON-safe copy for API responses."""

        return {
            "id": self.id,
            "kind": self.kind,
            "schema": dict(self.schema),
            "supports": dict(self.supports),
            "timeout_seconds": self.timeout_seconds,
            "idempotency": self.idempotency,
            "requires_admin": self.requires_admin,
        }


@dataclass(frozen=True)
class ChannelManifest:
    """Stable identity and capabilities advertised by a channel adapter."""

    slug: str
    display_name: str
    adapter_version: str = "0.1.0"
    protocols: tuple[str, ...] = ()
    capabilities: tuple[str, ...] = ()
    account_flows: tuple[ProvisionFlowSpec, ...] = ()
    config_schema_version: int = 1
    config_schema: Mapping[str, Any] = field(default_factory=dict)
    health_checks: tuple[str, ...] = ()
    limits: Mapping[str, Any] = field(default_factory=dict)

    def __post_init__(self) -> None:
        slug = self.slug.strip()
        if not _SLUG_PATTERN.fullmatch(slug):
            raise ValueError("channel slug must match [a-z0-9][a-z0-9-]{0,63}")
        if not self.display_name.strip():
            raise ValueError("channel display name must not be blank")
        if self.config_schema_version < 1:
            raise ValueError("config schema version must be positive")
        flow_ids = [flow.id for flow in self.account_flows]
        if len(flow_ids) != len(set(flow_ids)):
            raise ValueError("channel provision flow ids must be unique")

    @property
    def flows(self) -> tuple[ProvisionFlowSpec, ...]:
        """Compatibility alias used by schema renderers."""

        return self.account_flows

    def as_dict(self) -> dict[str, Any]:
        """Return a serialisable manifest payload without secret values."""

        return {
            "slug": self.slug,
            "display_name": self.display_name,
            "adapter_version": self.adapter_version,
            "protocols": list(self.protocols),
            "capabilities": list(self.capabilities),
            "config_schema_version": self.config_schema_version,
            "config_schema": dict(self.config_schema),
            "health_checks": list(self.health_checks),
            "limits": dict(self.limits),
            "flows": [flow.as_dict() for flow in self.account_flows],
        }

    def provision_schema(self) -> dict[str, Any]:
        """Return the stable account provision schema envelope."""

        return {
            "channel": self.slug,
            "adapter_version": self.adapter_version,
            "config_schema_version": self.config_schema_version,
            "flows": [flow.as_dict() for flow in self.account_flows],
        }
