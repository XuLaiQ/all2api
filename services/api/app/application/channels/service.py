from __future__ import annotations

from collections.abc import Mapping
from typing import Any

from app.domain.channel import ChannelManifest


class ChannelNotFoundError(LookupError):
    """Raised when a requested channel slug is absent from the registry."""


def _legacy_manifest(registration: Any) -> ChannelManifest:
    """Build a manifest for a pre-M1 AdapterSpec or a small test fake."""

    slug = str(getattr(registration, "slug", "")).strip()
    flows = tuple(getattr(registration, "account_flows", ()) or ())
    return ChannelManifest(
        slug=slug,
        display_name=str(getattr(registration, "name", slug)),
        adapter_version="0.1.0",
        protocols=tuple(getattr(registration, "protocols", ()) or ()),
        capabilities=tuple(
            getattr(registration, "caps", getattr(registration, "capabilities", ())) or ()
        ),
        account_flows=flows,
    )


class ChannelService:
    """Read-only channel capability and provision-schema use case."""

    def __init__(self, registry: Mapping[str, Any]):
        self._registry = registry

    def registration(self, slug: str) -> Any:
        registration = self._registry.get(slug)
        if registration is None:
            raise ChannelNotFoundError(slug)
        return registration

    def manifest(self, slug: str) -> ChannelManifest:
        registration = self.registration(slug)
        manifest = getattr(registration, "manifest", None)
        if isinstance(manifest, ChannelManifest):
            return manifest
        return _legacy_manifest(registration)

    def list_manifests(self) -> list[ChannelManifest]:
        return [self.manifest(slug) for slug in self._registry]

    def capability(self, slug: str) -> dict[str, Any]:
        return self.manifest(slug).as_dict()

    def provision_schema(self, slug: str) -> dict[str, Any]:
        registration = self.registration(slug)
        manifest = self.manifest(slug)
        provisioner = getattr(registration, "provisioner", None)
        describe = getattr(provisioner, "describe", None)
        if callable(describe):
            flows = describe()
            # M1 provisioners are synchronous metadata providers.  Keep the
            # fallback for adapter fakes that only expose manifest flows.
            if not hasattr(flows, "__await__"):
                manifest = ChannelManifest(
                    slug=manifest.slug,
                    display_name=manifest.display_name,
                    adapter_version=manifest.adapter_version,
                    protocols=manifest.protocols,
                    capabilities=manifest.capabilities,
                    account_flows=tuple(flows),
                    config_schema_version=manifest.config_schema_version,
                    config_schema=manifest.config_schema,
                    health_checks=manifest.health_checks,
                    limits=manifest.limits,
                )
        return manifest.provision_schema()

