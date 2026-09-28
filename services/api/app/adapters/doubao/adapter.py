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
    NullBrowserWorker,
    build_browser_worker,
    maybe_await,
)
from .credentials import CredentialStore, MemoryCredentialStore
from .errors import safe_error
from .manifest import DOUBAO_MANIFEST
from .mapper import map_account
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
    ) -> None:
        self.manifest = manifest or DOUBAO_MANIFEST
        self.provisioner = provisioner or DoubaoProvisioner(
            profile_root=profile_root,
            browser_worker=browser_worker or NullBrowserWorker(),
            credential_store=credential_store or MemoryCredentialStore(),
            manifest=self.manifest,
        )
        self.runtime = runtime or (
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
        return {
            "status": (
                "ok"
                if worker_health.get("status") in {"ready", "not_configured"}
                else "degraded"
            ),
            "channel": self.manifest.slug,
            "worker": worker_health,
        }

    async def list_models(self, context: Any = None) -> list[Mapping[str, Any]]:
        """Return models supplied by an already migrated catalogue port.

        The native adapter deliberately performs no legacy HTTP request.  A
        future Doubao model client can populate ``context['models']`` here.
        """

        if isinstance(context, Mapping) and isinstance(context.get("models"), list):
            return [item for item in context["models"] if isinstance(item, Mapping)]
        if self.runtime is not None:
            return await self.runtime.list_models(context)
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
        """Close an injected runtime client when the composition root owns it."""

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
    platform_key = getattr(settings, "doubao_platform_data_key", None)
    if hasattr(platform_key, "get_secret_value"):
        platform_key = platform_key.get_secret_value()
    if not platform_key:
        platform_key = getattr(settings, "doubao_api_key", "")
        if hasattr(platform_key, "get_secret_value"):
            platform_key = platform_key.get_secret_value()
    return build_adapter(
        profile_root=getattr(settings, "doubao_profile_root", "./data/doubao/profiles"),
        browser_worker=browser_worker or browser_worker_from_settings(settings),
        credential_store=credential_store,
        base_url=str(getattr(settings, "doubao_platform_base", "") or ""),
        auth_key=str(platform_key or ""),
    )


def browser_worker_from_settings(settings: Any) -> BrowserWorker:
    """Build the optional browser worker without importing Playwright eagerly."""

    return build_browser_worker(settings)


__all__ = ["DoubaoAdapter", "build_adapter", "browser_worker_from_settings", "create_adapter"]
