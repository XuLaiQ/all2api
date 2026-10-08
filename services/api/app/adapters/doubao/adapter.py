"""Channel-owned Doubao adapter facade.

Only safe catalogue mapping belongs here.  Inbound protocol handling remains
in the shared gateway and account onboarding is delegated to
``DoubaoProvisioner`` through ports.
"""

from __future__ import annotations

from collections.abc import AsyncIterator, Mapping
from pathlib import Path
from typing import Any

from app.adapters.native_runtime import NativeHttpAdapter, NativeStream

from .browser import (
    BrowserWorker,
    build_browser_worker,
    maybe_await,
)
from .credentials import CredentialStore, MemoryCredentialStore
from .errors import safe_error
from .manifest import DOUBAO_MANIFEST
from .mapper import map_account
from .native_qr import NativeDoubaoQrWorker
from .provisioner import DoubaoProvisioner


class DoubaoAdapter:
    """Small composition root for the native Doubao channel."""

    def __init__(
        self,
        *,
        profile_root: str | Path = "./data/doubao/profiles",
        browser_worker: BrowserWorker | None = None,
        credential_store: CredentialStore | None = None,
        manifest=None,
        base_url: str = "",
        auth_key: str = "",
        http_client: Any | None = None,
        provisioner: DoubaoProvisioner | None = None,
        runtime: NativeHttpAdapter | None = None,
        transport: Any | None = None,
    ) -> None:
        self.manifest = manifest or DOUBAO_MANIFEST
        self.provisioner = provisioner or DoubaoProvisioner(
            profile_root=profile_root,
            browser_worker=browser_worker or NativeDoubaoQrWorker(),
            credential_store=credential_store or MemoryCredentialStore(),
            manifest=self.manifest,
        )
        self.runtime = runtime or transport or (
            NativeHttpAdapter(
                self.manifest,
                base_url,
                auth_key=auth_key,
                http_client=http_client,
                credential_store=credential_store,
                channel="doubao",
            )
            if str(base_url or "").strip()
            else None
        )

    def normalize_account(self, value: Mapping[str, Any] | Any) -> dict[str, Any]:
        return map_account(value)

    async def health(self, context: Any = None) -> Mapping[str, Any]:
        worker_health = await maybe_await(self.provisioner.worker_health())
        runtime_health: Mapping[str, Any] = {}
        if self.runtime is not None:
            health = getattr(self.runtime, "health", None)
            if callable(health):
                value = await maybe_await(health(context))
                if isinstance(value, Mapping):
                    runtime_health = dict(value)
        return {
            "status": (
                "ok"
                if worker_health.get("status") in {"ready", "not_configured"}
                and runtime_health.get("status") not in {"no_credentials", "error"}
                else "degraded"
            ),
            "channel": self.manifest.slug,
            "worker": worker_health,
            "transport": runtime_health,
        }

    async def startup(self) -> None:
        """Start channel-owned browser resources when the adapter is embedded."""

        await self.provisioner.startup()

    async def list_models(self, context: Any = None) -> list[Mapping[str, Any]]:
        """Return the authenticated chat and generation catalogue from Doubao Web."""

        if self.runtime is not None:
            values = await self.runtime.list_models(context)
            generation_options: list[Mapping[str, Any]] = []
            option_reader = getattr(self.runtime, "list_generation_options", None)
            if callable(option_reader):
                generation_options = [
                    item
                    for item in await option_reader(context)
                    if isinstance(item, Mapping)
                ]
            combined = [*values, *generation_options]
            return combined
        return []

    async def list_accounts(self, context: Any = None) -> list[Mapping[str, Any]]:
        if isinstance(context, Mapping) and isinstance(context.get("accounts"), list):
            return [item for item in context["accounts"] if isinstance(item, Mapping)]
        return []

    async def invoke(self, request: Any, account: Any) -> Any:
        if self.runtime is None:
            raise RuntimeError("Doubao native data plane is not configured")
        return await self.runtime.invoke(request, account)

    async def invoke_capability(self, capability: str, request: Any, account: Any = None) -> Any:
        if self.runtime is None:
            raise RuntimeError("Doubao native data plane is not configured")
        return await self.runtime.invoke_capability(capability, request, account)

    async def invoke_stream(self, request: Any, account: Any) -> AsyncIterator[Any]:
        if self.runtime is None:
            raise RuntimeError("Doubao native data plane is not configured")
        async for chunk in self.runtime.invoke_stream(request, account):
            yield chunk

    async def open_stream(self, request: Any, account: Any = None) -> NativeStream:
        """Expose the gateway stream handle while keeping transport channel-owned."""

        if self.runtime is None:
            raise RuntimeError("Doubao native data plane is not configured")
        return await self.runtime.open_stream(request, account)

    async def chat(self, request: Any, account: Any = None) -> Any:
        return await self.invoke(request, account)

    async def chat_stream(self, request: Any, account: Any = None) -> AsyncIterator[Any]:
        async for chunk in self.invoke_stream(request, account):
            yield chunk

    async def shutdown(self) -> None:
        """Close channel-owned browser resources and any injected runtime client."""

        await self.provisioner.shutdown()

        if self.runtime is not None and getattr(self.runtime, "_http_client", None) is None:
            # NativeHttpAdapter creates short-lived clients per request, so no
            # persistent client exists here. This method is kept as a lifecycle
            # hook for future channel transports.
            return

    def map_error(self, error: Exception) -> Mapping[str, Any]:
        status, code, message = safe_error(error)
        return {
            "status_code": status,
            "code": code,
            "message": message,
            "retryable": status >= 500,
        }


def build_adapter(
    *,
    profile_root: str | Path = "./data/doubao/profiles",
    browser_worker: BrowserWorker | None = None,
    credential_store: CredentialStore | None = None,
    base_url: str = "",
    auth_key: str = "",
    http_client: Any | None = None,
) -> DoubaoAdapter:
    return DoubaoAdapter(
        profile_root=profile_root,
        browser_worker=browser_worker,
        credential_store=credential_store,
        base_url=base_url,
        auth_key=auth_key,
        http_client=http_client,
    )


def create_adapter(
    settings: Any,
    *,
    browser_worker: BrowserWorker | None = None,
    credential_store: CredentialStore | None = None,
) -> DoubaoAdapter:
    configured_worker = (
        browser_worker_from_settings(settings)
        if bool(getattr(settings, "doubao_browser_enabled", False))
        else NativeDoubaoQrWorker.from_settings(settings)
    )
    return build_adapter(
        profile_root=getattr(settings, "doubao_profile_root", "./data/doubao/profiles"),
        browser_worker=browser_worker or configured_worker,
        credential_store=credential_store,
        base_url=str(getattr(settings, "doubao_platform_base", "") or ""),
    )


def browser_worker_from_settings(settings: Any) -> BrowserWorker:
    """Build the optional browser worker without importing Playwright eagerly."""

    return build_browser_worker(settings)


__all__ = ["DoubaoAdapter", "build_adapter", "browser_worker_from_settings", "create_adapter"]
