"""Small HTTP runtime shared by native channel adapters.

The runtime is intentionally channel agnostic.  It only knows how to send a
normalised JSON request to the provider endpoint and how to expose the
response/stream lifecycle to the gateway.  Channel packages own model
catalogue mapping, credentials and error classification; this module never
imports a source project's bridge or management client.
"""

from __future__ import annotations

import inspect
import json
from collections.abc import AsyncIterator, Mapping
from dataclasses import dataclass
from typing import Any

import httpx

from app.domain.channel import ChannelManifest
from app.ports.adapters import NormalizedRequest


@dataclass
class NativeStream:
    """An open provider response and its owning HTTP client."""

    client: httpx.AsyncClient
    response: httpx.Response


class NativeHttpAdapter:
    """Provider HTTP transport implementing the common upstream adapter port.

    ``http_client`` can be injected with an ``httpx.MockTransport`` client in
    adapter contract tests.  In production a short-lived client is created for
    each request; stream callers receive the client in :class:`NativeStream`
    and must close it after consuming the stream.
    """

    def __init__(
        self,
        manifest: ChannelManifest,
        base_url: str,
        *,
        auth_key: str = "",
        http_client: httpx.AsyncClient | None = None,
        timeout: float = 300.0,
        connect_timeout: float = 10.0,
        chat_path: str = "/v1/chat/completions",
        models_path: str = "/v1/models",
        credential_store: Any | None = None,
        channel: str = "",
    ) -> None:
        self.manifest = manifest
        self.base_url = str(base_url or "").rstrip("/")
        self.auth_key = str(auth_key or "")
        self._http_client = http_client
        self.timeout = timeout
        self.connect_timeout = connect_timeout
        self.chat_path = chat_path
        self.models_path = models_path
        self.credential_store = credential_store
        self.channel = str(channel or manifest.slug)

    @staticmethod
    def _credential_headers(credentials: Mapping[str, Any]) -> dict[str, str]:
        result: dict[str, str] = {}
        authorization = credentials.get("authorization")
        access_token = (
            credentials.get("access_token")
            or credentials.get("accessToken")
            or credentials.get("token")
        )
        if authorization:
            result["Authorization"] = str(authorization)
        elif access_token:
            result["Authorization"] = f"Bearer {access_token}"

        cookie = credentials.get("cookie") or credentials.get("cookies")
        if isinstance(cookie, Mapping):
            cookie = "; ".join(
                f"{key}={value}" for key, value in cookie.items() if value is not None
            )
        if cookie:
            result["Cookie"] = str(cookie)
        return result

    async def _account_credentials(self, account: Any) -> Mapping[str, Any]:
        """Resolve private credentials for a selected local account.

        Account DTOs expose both the database id and provider-native id in
        different call paths. Try both forms while keeping all secret material
        inside the credential store boundary.
        """

        if self.credential_store is None or not isinstance(account, Mapping):
            return {}
        reader = getattr(self.credential_store, "read", None)
        if not callable(reader):
            return {}
        candidates: list[str] = []
        for key in ("native_id", "account_id", "lease_account_id", "id"):
            value = str(account.get(key) or "").strip()
            if not value:
                continue
            candidates.append(value)
            prefix = f"{self.channel}:"
            if value.startswith(prefix):
                candidates.append(value[len(prefix) :])
        seen: set[str] = set()
        for account_id in candidates:
            if account_id in seen:
                continue
            seen.add(account_id)
            value = reader(self.channel, account_id)
            if inspect.isawaitable(value):
                value = await value
            if isinstance(value, Mapping):
                return value
        return {}

    def _headers(
        self,
        headers: Mapping[str, str] | None = None,
        credentials: Mapping[str, Any] | None = None,
    ) -> dict[str, str]:
        result = {"Accept": "application/json", "Content-Type": "application/json"}
        if self.auth_key:
            result["Authorization"] = f"Bearer {self.auth_key}"
        if headers:
            result.update({str(key): str(value) for key, value in headers.items()})
        if credentials:
            result.update(self._credential_headers(credentials))
        return result

    def _url(self, path: str) -> str:
        root = self.base_url
        if root.endswith("/v1") and path.startswith("/v1/"):
            root = root[:-3]
        return f"{root}{path}"

    def _client(self) -> tuple[httpx.AsyncClient, bool]:
        if self._http_client is not None:
            return self._http_client, False
        return (
            httpx.AsyncClient(
                timeout=httpx.Timeout(self.timeout, connect=self.connect_timeout)
            ),
            True,
        )

    @staticmethod
    def _payload(
        request: NormalizedRequest | Mapping[str, Any],
    ) -> tuple[str, Mapping[str, Any], Mapping[str, str], bool]:
        if isinstance(request, NormalizedRequest):
            return request.model, request.payload, request.headers, request.stream
        model = str(request.get("model") or "")
        payload = request.get("payload")
        if not isinstance(payload, Mapping):
            payload = {
                key: value
                for key, value in request.items()
                if key not in {"headers", "stream"}
            }
        headers = request.get("headers")
        if not isinstance(headers, Mapping):
            headers = {}
        return model, payload, headers, bool(request.get("stream"))

    async def list_models(self, context: Any = None) -> list[Mapping[str, Any]]:
        """Read the provider catalogue and return only object-shaped entries."""

        client, owned = self._client()
        try:
            response = await client.get(self._url(self.models_path), headers=self._headers())
            response.raise_for_status()
            payload = response.json()
        finally:
            if owned:
                await client.aclose()
        if isinstance(payload, list):
            values = payload
        elif isinstance(payload, Mapping) and isinstance(payload.get("data"), list):
            values = payload["data"]
        else:
            return []
        return [item for item in values if isinstance(item, Mapping) and item.get("id")]

    async def health(self, context: Any = None) -> Mapping[str, Any]:
        try:
            await self.list_models(context)
        except httpx.TimeoutException:
            return {"status": "timeout"}
        except httpx.HTTPError:
            return {"status": "platform_unavailable"}
        except (TypeError, ValueError):
            return {"status": "protocol_error"}
        return {"status": "ok"}

    async def invoke(self, request: Any, account: Any = None) -> httpx.Response:
        """Perform one non-streaming chat request and return a buffered response."""

        _model, payload, headers, _stream = self._payload(request)
        credentials = await self._account_credentials(account)
        client, owned = self._client()
        try:
            response = await client.post(
                self._url(self.chat_path),
                content=json.dumps(
                    dict(payload), ensure_ascii=False, separators=(",", ":")
                ).encode(),
                headers=self._headers(headers, credentials),
            )
            # ``post`` already buffers the body.  Close the owned client before
            # returning so callers do not need to know the transport internals.
            if owned:
                await client.aclose()
            return response
        except Exception:
            if owned:
                await client.aclose()
            raise

    async def open_stream(self, request: Any, account: Any = None) -> NativeStream:
        """Open a streaming response for the gateway's SSE lifecycle."""

        _model, payload, headers, _stream = self._payload(request)
        credentials = await self._account_credentials(account)
        client, owned = self._client()
        if not owned:
            # Injected clients belong to the test/application composition root;
            # the gateway still receives a closeable handle for symmetry.
            owned = False
        try:
            request_obj = client.build_request(
                "POST",
                self._url(self.chat_path),
                content=json.dumps(
                    dict(payload), ensure_ascii=False, separators=(",", ":")
                ).encode(),
                headers=self._headers(headers, credentials),
            )
            response = await client.send(request_obj, stream=True)
            return NativeStream(client=client, response=response)
        except Exception:
            if owned:
                await client.aclose()
            raise

    async def invoke_stream(self, request: Any, account: Any = None) -> AsyncIterator[bytes]:
        """Yield raw provider bytes and close the response on completion."""

        handle = await self.open_stream(request, account)
        try:
            async for chunk in handle.response.aiter_bytes():
                yield chunk
        finally:
            await handle.response.aclose()
            if self._http_client is None:
                await handle.client.aclose()

    async def chat(self, request: Any, account: Any = None) -> httpx.Response:
        return await self.invoke(request, account)

    async def chat_stream(self, request: Any, account: Any = None) -> AsyncIterator[bytes]:
        async for chunk in self.invoke_stream(request, account):
            yield chunk

    def map_error(self, error: Exception) -> Mapping[str, Any]:
        if isinstance(error, httpx.TimeoutException):
            return {
                "code": "upstream_timeout",
                "message": "upstream request timed out",
                "retryable": True,
            }
        if isinstance(error, httpx.HTTPError):
            return {
                "code": "upstream_unavailable",
                "message": "upstream request failed",
                "retryable": True,
            }
        return {"code": "internal", "message": "upstream adapter failed", "retryable": False}


__all__ = ["NativeHttpAdapter", "NativeStream"]
