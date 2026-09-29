from __future__ import annotations

import base64
import hashlib
import inspect
import json
import random
import re
import time
import uuid
from collections.abc import Mapping
from datetime import datetime, timedelta, timezone
from typing import Any

import httpx
from curl_cffi import requests as curl_requests

from .errors import (
    ChatGPTAuthError,
    ChatGPTInvalidRequestError,
    ChatGPTProtocolError,
    ChatGPTRateLimitError,
    ChatGPTTimeoutError,
    ChatGPTUpstreamUnavailableError,
)
from .manifest import CHATGPT_WEB_BASE_URL
from .turnstile import solve_turnstile_token


class ChatGPTWebClient:
    """Small native client for the authenticated ChatGPT Web backend."""

    def __init__(
        self,
        credentials: Mapping[str, Any] | None = None,
        *,
        base_url: str = CHATGPT_WEB_BASE_URL,
        http_client: httpx.AsyncClient | None = None,
        timeout: float = 300.0,
        connect_timeout: float = 10.0,
    ) -> None:
        self.credentials = dict(credentials or {})
        self.base_url = str(base_url or CHATGPT_WEB_BASE_URL).rstrip("/")
        self._http_client = http_client
        self.timeout = float(timeout)
        self.connect_timeout = float(connect_timeout)
        self._fingerprint = (
            self.credentials.get("fp")
            if isinstance(self.credentials.get("fp"), Mapping)
            else {}
        )
        self.device_id = str(
            self.credentials.get("oai_device_id")
            or self._fingerprint.get("oai-device-id")
            or uuid.uuid4()
        )
        self.session_id = str(
            self.credentials.get("oai_session_id")
            or self._fingerprint.get("oai-session-id")
            or uuid.uuid4()
        )
        self.user_agent = str(
            self.credentials.get("user_agent")
            or self._fingerprint.get("user-agent")
            or "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
            "(KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36 Edg/143.0.0.0"
        )
        self._script_sources: list[str] = []
        self._data_build = ""

    @property
    def access_token(self) -> str:
        return str(
            self.credentials.get("access_token")
            or self.credentials.get("accessToken")
            or ""
        ).strip()

    def _headers(
        self,
        path: str,
        *,
        accept: str = "application/json",
        extra: Mapping[str, str] | None = None,
    ) -> dict[str, str]:
        headers = {
            "Accept": accept,
            "Content-Type": "application/json",
            "Authorization": f"Bearer {self.access_token}",
            "Origin": self.base_url,
            "Referer": f"{self.base_url}/",
            "User-Agent": self.user_agent,
            "Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8",
            "Cache-Control": "no-cache",
            "Pragma": "no-cache",
            "Priority": "u=1, i",
            "Sec-Ch-Ua": '"Microsoft Edge";v="143", "Chromium";v="143", "Not A(Brand";v="24"',
            "Sec-Ch-Ua-Mobile": "?0",
            "Sec-Ch-Ua-Platform": '"Windows"',
            "Sec-Ch-Ua-Arch": '"x86"',
            "Sec-Ch-Ua-Bitness": '"64"',
            "Sec-Fetch-Dest": "empty",
            "Sec-Fetch-Mode": "cors",
            "Sec-Fetch-Site": "same-origin",
            "OAI-Device-Id": self.device_id,
            "OAI-Session-Id": self.session_id,
            "OAI-Language": "zh-CN",
            "OAI-Client-Version": "prod-a194cd50d4416d3c0b47c740f206b12ce60f5887",
            "OAI-Client-Build-Number": "6708908",
            "X-OpenAI-Target-Path": path,
            "X-OpenAI-Target-Route": path.split("?", 1)[0],
        }
        fingerprint_headers = {
            "Sec-Ch-Ua": "sec-ch-ua",
            "Sec-Ch-Ua-Mobile": "sec-ch-ua-mobile",
            "Sec-Ch-Ua-Platform": "sec-ch-ua-platform",
            "Sec-Ch-Ua-Arch": "sec-ch-ua-arch",
            "Sec-Ch-Ua-Bitness": "sec-ch-ua-bitness",
            "Sec-Ch-Ua-Full-Version": "sec-ch-ua-full-version",
            "Sec-Ch-Ua-Full-Version-List": "sec-ch-ua-full-version-list",
        }
        for header, key in fingerprint_headers.items():
            value = self.credentials.get(key) or self._fingerprint.get(key)
            if value:
                headers[header] = str(value)
        if extra:
            headers.update({str(key): str(value) for key, value in extra.items()})
        return headers

    def _client(self) -> tuple[Any, bool]:
        if self._http_client is not None:
            return self._http_client, False
        options: dict[str, Any] = {
            "impersonate": str(
                self.credentials.get("impersonate")
                or self._fingerprint.get("impersonate")
                or "chrome110"
            ),
            "verify": True,
        }
        proxy = str(self.credentials.get("proxy") or "").strip()
        if proxy:
            options["proxy"] = proxy
        return curl_requests.AsyncSession(**options), True

    @staticmethod
    async def _close_resource(resource: Any) -> None:
        closer = getattr(resource, "aclose", None) or getattr(resource, "close", None)
        if not callable(closer):
            return
        result = closer()
        if inspect.isawaitable(result):
            await result

    async def _json_request(
        self,
        method: str,
        path: str,
        *,
        json_body: Mapping[str, Any] | None = None,
        timeout: float | None = None,
    ) -> Mapping[str, Any]:
        client, owned = self._client()
        try:
            try:
                response = await client.request(
                    method,
                    f"{self.base_url}{path}",
                    headers=self._headers(path),
                    json=dict(json_body) if json_body is not None else None,
                    timeout=timeout,
                )
            except httpx.TimeoutException as exc:
                raise ChatGPTTimeoutError() from exc
            except httpx.RequestError as exc:
                raise ChatGPTUpstreamUnavailableError() from exc
            self._raise_for_status(response)
            try:
                value = response.json()
            except (TypeError, ValueError) as exc:
                raise ChatGPTProtocolError() from exc
            if not isinstance(value, Mapping):
                raise ChatGPTProtocolError()
            return value
        finally:
            if owned:
                await self._close_resource(client)

    @staticmethod
    def _raise_for_status(response: httpx.Response) -> None:
        if response.status_code == 401:
            raise ChatGPTAuthError()
        if response.status_code == 429:
            retry_after: int | None = None
            value = response.headers.get("retry-after")
            if value:
                try:
                    retry_after = max(0, int(float(value)))
                except ValueError:
                    retry_after = None
            raise ChatGPTRateLimitError(retry_after)
        if response.status_code >= 500:
            raise ChatGPTUpstreamUnavailableError()
        if response.status_code >= 400:
            raise ChatGPTProtocolError()

    async def account_info(self) -> Mapping[str, Any]:
        return await self._json_request("GET", "/backend-api/me")

    async def _bootstrap(self) -> None:
        client, owned = self._client()
        path = "/"
        try:
            try:
                response = await client.get(
                    f"{self.base_url}{path}",
                    headers=self._headers(path, accept="text/html"),
                    timeout=30,
                )
            except httpx.TimeoutException as exc:
                raise ChatGPTTimeoutError() from exc
            except httpx.RequestError as exc:
                raise ChatGPTUpstreamUnavailableError() from exc
            self._raise_for_status(response)
            html = response.text
            self._script_sources = re.findall(
                r"<script[^>]+src=[\"']([^\"']+)[\"']", html, flags=re.IGNORECASE
            )
            self._data_build = ""
            match = re.search(r"c/[^/]*/_", html)
            if match:
                self._data_build = match.group(0)
            if not self._data_build:
                build_match = re.search(
                    r'<html[^>]+data-build=["\']([^"\']+)["\']',
                    html,
                    flags=re.IGNORECASE,
                )
                if build_match:
                    self._data_build = build_match.group(1)
        finally:
            if owned:
                await self._close_resource(client)

    def _requirements_token(self) -> str:
        sources = self._script_sources or [
            "https://chatgpt.com/backend-api/sentinel/sdk.js"
        ]
        navigator_keys = (
            "registerProtocolHandler−function registerProtocolHandler() { [native code] }",
            "storage−[object StorageManager]",
            "locks−[object LockManager]",
            "appCodeName−Mozilla",
            "permissions−[object Permissions]",
            "share−function share() { [native code] }",
            "webdriver−false",
            "managed−[object NavigatorManagedData]",
            "canShare−function canShare() { [native code] }",
            "vendor−Google Inc.",
            "mediaDevices−[object MediaDevices]",
            "vibrate−function vibrate() { [native code] }",
            "storageBuckets−[object StorageBucketManager]",
            "mediaCapabilities−[object MediaCapabilities]",
            "cookieEnabled−true",
            "virtualKeyboard−[object VirtualKeyboard]",
            "product−Gecko",
            "presentation−[object Presentation]",
            "onLine−true",
            "mimeTypes−[object MimeTypeArray]",
            "credentials−[object CredentialsContainer]",
            "serviceWorker−[object ServiceWorkerContainer]",
            "keyboard−[object Keyboard]",
            "gpu−[object GPU]",
            "doNotTrack",
            "serial−[object Serial]",
            "pdfViewerEnabled−true",
            "language−zh-CN",
            "geolocation−[object Geolocation]",
            "userAgentData−[object NavigatorUAData]",
            "getUserMedia−function getUserMedia() { [native code] }",
            "sendBeacon−function sendBeacon() { [native code] }",
            "hardwareConcurrency−32",
            "windowControlsOverlay−[object WindowControlsOverlay]",
        )
        document_keys = ["__reactContainer$fzelfjyxej8", "_reactListening5dehydibo78", "location"]
        window_keys = (
            "0", "window", "self", "document", "name", "location", "customElements",
            "history", "navigation", "innerWidth", "innerHeight", "scrollX", "scrollY",
            "visualViewport", "screenX", "screenY", "outerWidth", "outerHeight",
            "devicePixelRatio", "screen", "chrome", "navigator", "onresize", "performance",
            "crypto", "indexedDB", "sessionStorage", "localStorage", "scheduler", "alert",
            "atob", "btoa", "fetch", "matchMedia", "postMessage", "queueMicrotask",
            "requestAnimationFrame", "setInterval", "setTimeout", "caches", "__NEXT_DATA__",
            "__BUILD_MANIFEST", "__NEXT_PRELOADREADY",
        )
        resolutions = ((1920, 1080), (1440, 900), (2560, 1440), (3840, 2160))
        width, height = random.choices(resolutions, k=1)[0]
        eastern = timezone(timedelta(hours=-5))
        config = [
            width + height,
            datetime.now(eastern).strftime("%a %b %d %Y %H:%M:%S GMT-0500 (Eastern Standard Time)"),
            4294705152,
            1,
            self.user_agent,
            random.choice(sources),
            self._data_build,
            "en-US",
            "en-US,es-US,en,es",
            random.random(),
            random.choice(navigator_keys),
            random.choice(document_keys),
            random.choice(window_keys),
            time.perf_counter() * 1000,
            str(uuid.uuid4()),
            "",
            random.choice((8, 16, 24, 32)),
            time.time() * 1000 - time.perf_counter() * 1000,
            0, 0, 0, 0, 0, 0,
            0,
        ]
        encoded = base64.b64encode(
            json.dumps(config, separators=(",", ":"), ensure_ascii=False).encode()
        ).decode()
        return f"gAAAAAC{encoded}"

    def _proof_token(self, seed: str, difficulty: str) -> str:
        try:
            target = bytes.fromhex(str(difficulty))
        except ValueError as exc:
            raise ChatGPTProtocolError("ChatGPT proof-of-work difficulty is invalid") from exc
        if not target:
            return ""
        config = json.loads(
            base64.b64decode(self._requirements_token()[7:]).decode("utf-8")
        )
        static_1 = (json.dumps(config[:3], separators=(",", ":"))[:-1] + ",").encode()
        static_2 = (
            "," + json.dumps(config[4:9], separators=(",", ":"))[1:-1] + ","
        ).encode()
        static_3 = (
            "," + json.dumps(config[10:], separators=(",", ":"))[1:]
        ).encode()
        for counter in range(500_000):
            body = static_1 + str(counter).encode() + static_2
            body += str(counter >> 1).encode() + static_3
            encoded = base64.b64encode(body)
            if hashlib.sha3_512(str(seed).encode() + encoded).digest()[: len(target)] <= target:
                return "gAAAAAB" + encoded.decode()
        raise ChatGPTProtocolError("ChatGPT proof-of-work could not be solved")

    async def list_models(self) -> list[dict[str, Any]]:
        await self._bootstrap()
        payload = await self._json_request(
            "GET", "/backend-api/models?history_and_training_disabled=false"
        )
        values = payload.get("models")
        if not isinstance(values, list):
            values = payload.get("data")
        if not isinstance(values, list):
            raise ChatGPTProtocolError("ChatGPT model response has no model list")
        result: list[dict[str, Any]] = []
        seen: set[str] = set()
        for value in values:
            if not isinstance(value, Mapping):
                continue
            model_id = str(value.get("slug") or value.get("id") or "").strip()
            if not model_id or model_id in seen:
                continue
            seen.add(model_id)
            result.append(
                {
                    "id": model_id,
                    "object": "model",
                    "created": int(value.get("created") or 0),
                    "owned_by": str(value.get("owned_by") or "chatgpt"),
                    "permission": [],
                    "root": model_id,
                    "parent": None,
                }
            )
        if not result:
            raise ChatGPTProtocolError("ChatGPT returned an empty model list")
        return result

    @staticmethod
    def _conversation_messages(messages: Any) -> list[dict[str, Any]]:
        if not isinstance(messages, list) or not messages:
            raise ChatGPTInvalidRequestError("messages must be a non-empty array")
        result: list[dict[str, Any]] = []
        for message in messages:
            if not isinstance(message, Mapping):
                raise ChatGPTInvalidRequestError("message must be an object")
            role = str(message.get("role") or "user").strip()
            content = message.get("content", "")
            if not isinstance(content, str):
                raise ChatGPTInvalidRequestError("ChatGPT Web currently accepts text messages only")
            result.append(
                {
                    "id": str(uuid.uuid4()),
                    "author": {"role": role},
                    "content": {"content_type": "text", "parts": [content]},
                }
            )
        return result

    async def _requirements(self) -> dict[str, str]:
        await self._bootstrap()
        base = "/backend-api/sentinel/chat-requirements"
        p_token = str(self.credentials.get("sentinel_p") or self._requirements_token())
        prepared = await self._json_request(
            "POST",
            f"{base}/prepare",
            json_body={"p": p_token},
            timeout=30,
        )
        if (prepared.get("arkose") or {}).get("required"):
            raise ChatGPTProtocolError("ChatGPT requires an unsupported Arkose challenge")
        turnstile_token = ""
        turnstile = prepared.get("turnstile") or {}
        if turnstile.get("required"):
            dx = str(turnstile.get("dx") or "")
            turnstile_token = solve_turnstile_token(dx, p_token) if dx else None
            if not turnstile_token:
                raise ChatGPTProtocolError("ChatGPT Turnstile challenge could not be solved")
        proof = ""
        proof_info = prepared.get("proofofwork")
        if isinstance(proof_info, Mapping) and proof_info.get("required"):
            proof = self._proof_token(
                str(proof_info.get("seed") or ""),
                str(proof_info.get("difficulty") or ""),
            )
        finalized = await self._json_request(
            "POST",
            f"{base}/finalize",
            json_body={
                "prepare_token": str(prepared.get("prepare_token") or ""),
                "proof_token": proof,
                "turnstile_token": turnstile_token,
            },
            timeout=30,
        )
        token = str(finalized.get("token") or "").strip()
        if not token:
            raise ChatGPTProtocolError("ChatGPT did not return a conversation requirement token")
        return {
            "token": token,
            "proof": proof,
            "turnstile": turnstile_token,
            "so_token": str(finalized.get("so_token") or ""),
        }

    @staticmethod
    def _conversation_body(payload: Mapping[str, Any]) -> dict[str, Any]:
        model = str(payload.get("model") or "auto").strip() or "auto"
        return {
            "action": "next",
            "messages": ChatGPTWebClient._conversation_messages(payload.get("messages")),
            "model": model,
            "parent_message_id": str(uuid.uuid4()),
            "conversation_mode": {"kind": "primary_assistant"},
            "conversation_origin": None,
            "force_paragen": False,
            "force_paragen_model_slug": "",
            "force_rate_limit": False,
            "force_use_sse": True,
            "history_and_training_disabled": True,
            "reset_rate_limits": False,
            "suggestions": [],
            "supported_encodings": [],
            "system_hints": [],
            "timezone": "Asia/Shanghai",
            "timezone_offset_min": -480,
            "variant_purpose": "comparison_implicit",
            "websocket_request_id": str(uuid.uuid4()),
        }

    @staticmethod
    def _sse_text(raw: bytes) -> tuple[str, str]:
        text_parts: list[str] = []
        conversation_id = ""
        for line in raw.decode("utf-8", "replace").splitlines():
            if not line.startswith("data:"):
                continue
            value = line[5:].strip()
            if not value or value == "[DONE]":
                continue
            try:
                event = json.loads(value)
            except json.JSONDecodeError:
                continue
            if not isinstance(event, Mapping):
                continue
            conversation_id = conversation_id or str(event.get("conversation_id") or "")
            message = event.get("message")
            if isinstance(message, Mapping):
                conversation_id = conversation_id or str(message.get("conversation_id") or "")
                content = message.get("content")
                if isinstance(content, Mapping):
                    parts = content.get("parts")
                    if isinstance(parts, list):
                        text_parts.append(
                            "".join(str(part) for part in parts if isinstance(part, str))
                        )
            delta = event.get("delta")
            if isinstance(delta, str):
                text_parts.append(delta)
        return "".join(text_parts), conversation_id

    @staticmethod
    def _openai_response(
        *,
        model: str,
        content: str,
        stream: bool,
        conversation_id: str = "",
    ) -> bytes | dict[str, Any]:
        request_id = f"chatcmpl-{uuid.uuid4().hex}"
        created = int(time.time())
        if not stream:
            return {
                "id": request_id,
                "object": "chat.completion",
                "created": created,
                "model": model,
                "choices": [
                    {
                        "index": 0,
                        "message": {"role": "assistant", "content": content},
                        "finish_reason": "stop",
                    }
                ],
                "usage": {
                    "prompt_tokens": 0,
                    "completion_tokens": 0,
                    "total_tokens": 0,
                },
                "_conversation_id": conversation_id,
            }
        event = {
            "id": request_id,
            "object": "chat.completion.chunk",
            "created": created,
            "model": model,
            "choices": [
                {
                    "index": 0,
                    "delta": {"role": "assistant", "content": content},
                    "finish_reason": None,
                }
            ],
        }
        return (
            f"data: {json.dumps(event, ensure_ascii=False)}\n\n"
            "data: [DONE]\n\n"
        ).encode()

    async def chat(self, payload: Mapping[str, Any]) -> httpx.Response:
        if not self.access_token:
            raise ChatGPTAuthError()
        body = self._conversation_body(payload)
        requirements = await self._requirements()
        path = "/backend-api/conversation"
        client, owned = self._client()
        response: httpx.Response | None = None
        try:
            try:
                response = await client.post(
                    f"{self.base_url}{path}",
                    headers=self._headers(
                        path,
                        accept="text/event-stream",
                        extra={
                            "OpenAI-Sentinel-Chat-Requirements-Token": requirements["token"],
                            **(
                                {"OpenAI-Sentinel-Turnstile-Token": requirements["turnstile"]}
                                if requirements.get("turnstile")
                                else {}
                            ),
                            **(
                                {"OpenAI-Sentinel-Proof-Token": requirements["proof"]}
                                if requirements["proof"]
                                else {}
                            ),
                            **(
                                {"OpenAI-Sentinel-SO-Token": requirements["so_token"]}
                                if requirements["so_token"]
                                else {}
                            ),
                        },
                    ),
                    json=body,
                    timeout=self.timeout,
                )
            except httpx.TimeoutException as exc:
                raise ChatGPTTimeoutError() from exc
            except httpx.RequestError as exc:
                raise ChatGPTUpstreamUnavailableError() from exc
            self._raise_for_status(response)
            raw = response.content
            content, conversation_id = self._sse_text(raw)
            model = str(payload.get("model") or body.get("model") or "auto")
            stream = bool(payload.get("stream"))
            converted = self._openai_response(
                model=model,
                content=content,
                stream=stream,
                conversation_id=conversation_id,
            )
            if stream:
                return httpx.Response(
                    200,
                    headers={"content-type": "text/event-stream"},
                    content=converted if isinstance(converted, bytes) else b"",
                    request=response.request,
                )
            return httpx.Response(
                200,
                headers={"content-type": "application/json"},
                json=converted if isinstance(converted, dict) else {},
                request=response.request,
            )
        finally:
            if response is not None:
                await self._close_resource(response)
            if owned:
                await self._close_resource(client)

    async def invoke(self, request: Mapping[str, Any]) -> httpx.Response:
        return await self.chat(request)


__all__ = ["ChatGPTWebClient"]
