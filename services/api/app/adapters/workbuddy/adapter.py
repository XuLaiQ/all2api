from __future__ import annotations

from collections.abc import AsyncIterator, Mapping
from typing import Any

from app.adapters.native_runtime import NativeHttpAdapter
from app.adapters.workbuddy.client import WorkBuddyClient
from app.adapters.workbuddy.errors import WorkBuddyError, WorkBuddyProtocolError
from app.adapters.workbuddy.manifest import WORKBUDDY_MANIFEST
from app.adapters.workbuddy.provisioner import WorkBuddyProvisioner


class WorkBuddyAdapter:
    """Native WorkBuddy channel adapter assembled from ports."""

    manifest = WORKBUDDY_MANIFEST

    def __init__(
        self, client: WorkBuddyClient, *, provisioner: WorkBuddyProvisioner | None = None
    ) -> None:
        self.client = client
        self._runtime = NativeHttpAdapter(
            self.manifest,
            client.base_url,
            auth_key=client.data_key,
        )
        self.provisioner = provisioner or WorkBuddyProvisioner(client)

    async def health(self, context: Any = None) -> Mapping[str, Any]:
        return await self.client.health()

    async def list_models(self, context: Any = None) -> list[Mapping[str, Any]]:
        return await self.client.list_models()

    async def invoke(self, request: Any, account: Any) -> Any:
        return await self._runtime.invoke(request, account)

    async def invoke_stream(self, request: Any, account: Any) -> AsyncIterator[Any]:
        async for chunk in self._runtime.invoke_stream(request, account):
            yield chunk

    async def chat(self, request: Any, account: Any = None) -> Any:
        return await self.invoke(request, account)

    async def chat_stream(self, request: Any, account: Any = None) -> AsyncIterator[Any]:
        async for chunk in self.invoke_stream(request, account):
            yield chunk

    def map_error(self, error: Exception) -> Mapping[str, Any]:
        if isinstance(error, WorkBuddyError):
            return error.as_dict()
        if isinstance(error, (ValueError, TypeError)):
            return {"code": "invalid_request", "message": str(error), "retryable": False}
        return WorkBuddyProtocolError(str(error)).as_dict()


def create_adapter(settings: Any, *, credential_store: Any | None = None) -> WorkBuddyAdapter:
    platform_key = getattr(settings, "wb_platform_data_key", None)
    data_key = (
        platform_key.get_secret_value()
        if hasattr(platform_key, "get_secret_value")
        else str(platform_key or "")
    )
    if not data_key:
        legacy_key = getattr(settings, "wb_data_key", "")
        data_key = (
            legacy_key.get_secret_value()
            if hasattr(legacy_key, "get_secret_value")
            else str(legacy_key or "")
        )
    # Keep the native client independent from the legacy HTTP bridge.  The
    # latter's management URL is only used by compatibility readers.
    platform_base = str(
        getattr(settings, "wb_platform_base", "https://copilot.tencent.com")
        or "https://copilot.tencent.com"
    )
    client = WorkBuddyClient(platform_base, data_key=data_key)
    provisioner = WorkBuddyProvisioner(client, credential_store=credential_store)
    return WorkBuddyAdapter(client, provisioner=provisioner)
