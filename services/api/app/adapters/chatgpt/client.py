from __future__ import annotations

import asyncio
import base64
import hashlib
import inspect
import json
import mimetypes
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
    ChatGPTError,
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
            self.credentials.get("fp") if isinstance(self.credentials.get("fp"), Mapping) else {}
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
            self.credentials.get("access_token") or self.credentials.get("accessToken") or ""
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
        sources = self._script_sources or ["https://chatgpt.com/backend-api/sentinel/sdk.js"]
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
            "0",
            "window",
            "self",
            "document",
            "name",
            "location",
            "customElements",
            "history",
            "navigation",
            "innerWidth",
            "innerHeight",
            "scrollX",
            "scrollY",
            "visualViewport",
            "screenX",
            "screenY",
            "outerWidth",
            "outerHeight",
            "devicePixelRatio",
            "screen",
            "chrome",
            "navigator",
            "onresize",
            "performance",
            "crypto",
            "indexedDB",
            "sessionStorage",
            "localStorage",
            "scheduler",
            "alert",
            "atob",
            "btoa",
            "fetch",
            "matchMedia",
            "postMessage",
            "queueMicrotask",
            "requestAnimationFrame",
            "setInterval",
            "setTimeout",
            "caches",
            "__NEXT_DATA__",
            "__BUILD_MANIFEST",
            "__NEXT_PRELOADREADY",
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
            0,
            0,
            0,
            0,
            0,
            0,
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
        config = json.loads(base64.b64decode(self._requirements_token()[7:]).decode("utf-8"))
        static_1 = (json.dumps(config[:3], separators=(",", ":"))[:-1] + ",").encode()
        static_2 = ("," + json.dumps(config[4:9], separators=(",", ":"))[1:-1] + ",").encode()
        static_3 = ("," + json.dumps(config[10:], separators=(",", ":"))[1:]).encode()
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
        usage: Mapping[str, Any] | None = None,
    ) -> bytes | dict[str, Any]:
        request_id = f"chatcmpl-{uuid.uuid4().hex}"
        created = int(time.time())
        usage_payload = {"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
        if isinstance(usage, Mapping):
            prompt = int(usage.get("input_tokens") or usage.get("prompt_tokens") or 0)
            completion = int(usage.get("output_tokens") or usage.get("completion_tokens") or 0)
            usage_payload = {
                "prompt_tokens": prompt,
                "completion_tokens": completion,
                "total_tokens": int(usage.get("total_tokens") or prompt + completion),
            }
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
                "usage": usage_payload,
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
        return (f"data: {json.dumps(event, ensure_ascii=False)}\n\ndata: [DONE]\n\n").encode()

    @staticmethod
    def _walk_mappings(value: Any):
        if isinstance(value, Mapping):
            yield value
            for nested in value.values():
                yield from ChatGPTWebClient._walk_mappings(nested)
        elif isinstance(value, list):
            for nested in value:
                yield from ChatGPTWebClient._walk_mappings(nested)

    @staticmethod
    def _search_message_text(message: Mapping[str, Any]) -> str:
        content = message.get("content")
        parts: list[str] = []
        if isinstance(content, Mapping):
            text = content.get("text")
            if isinstance(text, str):
                parts.append(text)
            raw_parts = content.get("parts")
            if isinstance(raw_parts, list):
                parts.extend(str(item) for item in raw_parts if isinstance(item, str))
        if isinstance(content, str):
            parts.append(content)
        return "\n".join(item for item in parts if item).strip()

    @classmethod
    def _search_result(
        cls, conversation_id: str, conversation: Mapping[str, Any]
    ) -> dict[str, Any]:
        messages: list[Mapping[str, Any]] = []
        mapping = conversation.get("mapping")
        if isinstance(mapping, Mapping):
            for node in mapping.values():
                if not isinstance(node, Mapping):
                    continue
                message = node.get("message")
                if not isinstance(message, Mapping):
                    continue
                author = message.get("author")
                if isinstance(author, Mapping) and str(author.get("role") or "") == "assistant":
                    messages.append(message)
        message = (
            max(messages, key=lambda item: float(item.get("create_time") or 0)) if messages else {}
        )
        answer = cls._search_message_text(message)
        sources: list[dict[str, str]] = []
        seen: set[str] = set()
        for item in cls._walk_mappings(message):
            url = str(item.get("url") or item.get("link") or item.get("source_url") or "").strip()
            if not url or not url.startswith(("http://", "https://")) or url in seen:
                continue
            seen.add(url)
            sources.append(
                {
                    "title": str(item.get("title") or item.get("name") or "").strip(),
                    "url": url,
                    "snippet": str(item.get("snippet") or item.get("description") or "").strip(),
                }
            )
        metadata = message.get("metadata") if isinstance(message.get("metadata"), Mapping) else {}
        finish = (
            metadata.get("finish_details")
            if isinstance(metadata.get("finish_details"), Mapping)
            else {}
        )
        return {
            "conversation_id": conversation_id,
            "status": str(finish.get("type") or metadata.get("status") or "").strip(),
            "answer": answer,
            "sources": sources,
            "assistant_message_id": str(message.get("id") or ""),
        }

    async def search(self, prompt: str, model: str = "auto") -> dict[str, Any]:
        """Run the ChatGPT Web search composer and return a redacted result DTO."""

        query = str(prompt or "").strip()
        if not query:
            raise ChatGPTInvalidRequestError("search prompt is required")
        await self._bootstrap()
        prepare_path = "/backend-api/f/conversation/prepare"
        prepared = await self._json_request(
            "POST",
            prepare_path,
            json_body={
                "action": "next",
                "fork_from_shared_post": False,
                "parent_message_id": "client-created-root",
                "model": str(model or "auto"),
                "client_prepare_state": "success",
                "timezone_offset_min": -480,
                "timezone": "Asia/Shanghai",
                "conversation_mode": {"kind": "primary_assistant"},
                "system_hints": ["search"],
                "partial_query": {
                    "id": str(uuid.uuid4()),
                    "author": {"role": "user"},
                    "content": {"content_type": "text", "parts": [query]},
                },
                "supports_buffering": True,
                "supported_encodings": ["v1"],
                "client_contextual_info": {"app_name": "chatgpt.com"},
            },
            timeout=60,
        )
        conduit_token = str(prepared.get("conduit_token") or "").strip()
        if not conduit_token:
            raise ChatGPTProtocolError("ChatGPT search did not return a conduit token")
        requirements = await self._requirements()
        path = "/backend-api/f/conversation"
        extra = {
            "X-Conduit-Token": conduit_token,
            "OpenAI-Sentinel-Chat-Requirements-Token": requirements["token"],
            **(
                {"OpenAI-Sentinel-Turnstile-Token": requirements["turnstile"]}
                if requirements.get("turnstile")
                else {}
            ),
            **(
                {"OpenAI-Sentinel-Proof-Token": requirements["proof"]}
                if requirements.get("proof")
                else {}
            ),
            **(
                {"OpenAI-Sentinel-SO-Token": requirements["so_token"]}
                if requirements.get("so_token")
                else {}
            ),
        }
        body = {
            "action": "next",
            "messages": [
                {
                    "id": str(uuid.uuid4()),
                    "author": {"role": "user"},
                    "create_time": time.time(),
                    "content": {"content_type": "text", "parts": [query]},
                    "metadata": {
                        "system_hints": ["search"],
                        "serialization_metadata": {"custom_symbol_offsets": []},
                    },
                }
            ],
            "parent_message_id": "client-created-root",
            "model": str(model or "auto"),
            "client_prepare_state": "success",
            "timezone_offset_min": -480,
            "timezone": "Asia/Shanghai",
            "conversation_mode": {"kind": "primary_assistant"},
            "enable_message_followups": True,
            "system_hints": [],
            "supports_buffering": True,
            "supported_encodings": ["v1"],
            "force_use_search": True,
            "client_reported_search_source": "conversation_composer_web_icon",
            "client_contextual_info": {"app_name": "chatgpt.com"},
        }
        client, owned = self._client()
        response: Any = None
        try:
            response = await client.post(
                f"{self.base_url}{path}",
                headers=self._headers(path, accept="text/event-stream", extra=extra),
                json=body,
                timeout=self.timeout,
            )
            self._raise_for_status(response)
            _, conversation_id = self._sse_text(response.content)
        except httpx.TimeoutException as exc:
            raise ChatGPTTimeoutError() from exc
        except httpx.RequestError as exc:
            raise ChatGPTUpstreamUnavailableError() from exc
        finally:
            if response is not None:
                await self._close_resource(response)
            if owned:
                await self._close_resource(client)
        if not conversation_id:
            raise ChatGPTProtocolError("ChatGPT search did not return a conversation id")
        # The completion stream can contain intermediate tool events. Reading the
        # conversation document gives the stable answer and citation metadata.
        await asyncio.sleep(0.5)
        for _ in range(8):
            try:
                conversation = await self._json_request(
                    "GET",
                    f"/backend-api/conversation/{conversation_id}",
                    timeout=60,
                )
            except ChatGPTError as exc:
                if not isinstance(
                    exc,
                    (ChatGPTProtocolError, ChatGPTTimeoutError, ChatGPTUpstreamUnavailableError),
                ):
                    raise
                conversation = {}
            result = self._search_result(conversation_id, conversation)
            if result["answer"]:
                return result
            await asyncio.sleep(0.75)
        return self._search_result(
            conversation_id, conversation if isinstance(conversation, Mapping) else {}
        )

    @staticmethod
    def _editable_image_size(data: bytes, mime_type: str) -> tuple[int, int]:
        if mime_type == "image/png" and len(data) >= 24 and data[:8] == b"\x89PNG\r\n\x1a\n":
            return int.from_bytes(data[16:20], "big"), int.from_bytes(data[20:24], "big")
        if mime_type in {"image/jpeg", "image/jpg"}:
            index = 2
            while index + 9 < len(data) and data[index] == 0xFF:
                marker = data[index + 1]
                length = int.from_bytes(data[index + 2 : index + 4], "big")
                if marker in {
                    0xC0,
                    0xC1,
                    0xC2,
                    0xC3,
                    0xC5,
                    0xC6,
                    0xC7,
                    0xC9,
                    0xCA,
                    0xCB,
                    0xCD,
                    0xCE,
                    0xCF,
                }:
                    return int.from_bytes(data[index + 5 : index + 7], "big"), int.from_bytes(
                        data[index + 7 : index + 9], "big"
                    )
                index += max(2, length + 2)
        return 1, 1

    async def _upload_editable_image(self, value: str, index: int) -> dict[str, Any]:
        raw = str(value or "").strip()
        match = re.match(r"^data:([^;]+);base64,(.*)$", raw, flags=re.IGNORECASE | re.DOTALL)
        mime_type = str(match.group(1) if match else "image/png").lower()
        encoded = str(match.group(2) if match else raw).strip()
        try:
            data = base64.b64decode(encoded, validate=True)
        except (ValueError, TypeError) as exc:
            raise ChatGPTInvalidRequestError("editable image is not valid base64") from exc
        width, height = self._editable_image_size(data, mime_type)
        extension = mimetypes.guess_extension(mime_type) or ".png"
        name = f"image_{index}{extension}"
        metadata_path = "/backend-api/files"
        client, owned = self._client()
        response: Any = None
        try:
            response = await client.post(
                f"{self.base_url}{metadata_path}",
                headers=self._headers(metadata_path, accept="*/*"),
                json={
                    "file_name": name,
                    "file_size": len(data),
                    "use_case": "multimodal",
                    "timezone_offset_min": -480,
                    "reset_rate_limits": False,
                    "store_in_library": True,
                    "library_persistence_mode": "opportunistic",
                },
                timeout=60,
            )
            self._raise_for_status(response)
            created = response.json()
            if not isinstance(created, Mapping):
                raise ChatGPTProtocolError("ChatGPT image upload metadata is invalid")
            upload_url = str(created.get("upload_url") or "")
            file_id = str(created.get("file_id") or "")
            if not upload_url or not file_id:
                raise ChatGPTProtocolError("ChatGPT image upload metadata is incomplete")
            uploaded = await client.put(
                upload_url,
                headers={
                    "Content-Type": mime_type,
                    "x-ms-blob-type": "BlockBlob",
                    "x-ms-version": "2020-04-08",
                    "Origin": self.base_url,
                    "Referer": f"{self.base_url}/",
                    "User-Agent": self.user_agent,
                },
                content=data,
                timeout=120,
            )
            self._raise_for_status(uploaded)
            completed_path = f"/backend-api/files/{file_id}/uploaded"
            completed = await client.post(
                f"{self.base_url}{completed_path}",
                headers=self._headers(completed_path, accept="*/*"),
                json={},
                timeout=60,
            )
            self._raise_for_status(completed)
            return {
                "file_id": file_id,
                "library_file_id": str(created.get("library_file_id") or ""),
                "file_name": name,
                "file_size": len(data),
                "mime_type": mime_type,
                "width": width,
                "height": height,
            }
        except httpx.TimeoutException as exc:
            raise ChatGPTTimeoutError() from exc
        except httpx.RequestError as exc:
            raise ChatGPTUpstreamUnavailableError() from exc
        finally:
            if response is not None:
                await self._close_resource(response)
            if owned:
                await self._close_resource(client)

    async def generate_editable(
        self,
        kind: str,
        prompt: str,
        images: list[str] | None = None,
        model: str = "auto",
    ) -> dict[str, Any]:
        """Request a PPT/PSD artifact and return attachment descriptors.

        The file bytes remain inside the API service; callers receive only the
        sanitized names and bytes needed by the management router to persist
        local download artifacts.
        """

        normalized_kind = str(kind or "").strip().lower()
        if normalized_kind not in {"ppt", "psd"}:
            raise ChatGPTInvalidRequestError("editable file kind is invalid")
        image_values = list(images or [])
        if normalized_kind == "psd" and not image_values:
            raise ChatGPTInvalidRequestError("PSD generation requires an image")
        fixed_prompt = (
            "我需要你根据用户的需求制作一个可以编辑的 PPT，直接执行并输出 PPT 文件和素材 zip。"
            if normalized_kind == "ppt"
            else "请把用户提供的海报拆分为可编辑图层，直接输出 PSD 文件和图层素材 zip。"
        )
        user_prompt = str(prompt or "").strip()
        full_prompt = fixed_prompt + (f"\n\n用户补充需求：{user_prompt}" if user_prompt else "")
        uploaded = [
            await self._upload_editable_image(value, index)
            for index, value in enumerate(image_values, start=1)
        ]
        prepare_path = "/backend-api/f/conversation/prepare"
        prepared = await self._json_request(
            "POST",
            prepare_path,
            json_body={
                "action": "next",
                "fork_from_shared_post": False,
                "parent_message_id": "client-created-root",
                "model": str(model or "auto"),
                "client_prepare_state": "success",
                "timezone_offset_min": -480,
                "timezone": "Asia/Shanghai",
                "conversation_mode": {"kind": "primary_assistant"},
                "partial_query": {
                    "id": str(uuid.uuid4()),
                    "author": {"role": "user"},
                    "content": {"content_type": "text", "parts": [full_prompt]},
                },
                "supports_buffering": True,
                "supported_encodings": ["v1"],
                "attachment_mime_types": [str(item["mime_type"]) for item in uploaded],
                "thinking_effort": "high",
            },
            timeout=60,
        )
        conduit_token = str(prepared.get("conduit_token") or "").strip()
        if not conduit_token:
            raise ChatGPTProtocolError("ChatGPT editable task did not return a conduit token")
        requirements = await self._requirements()
        message_parts: list[dict[str, Any] | str] = [
            {
                "content_type": "image_asset_pointer",
                "asset_pointer": f"sediment://{item['file_id']}",
                "size_bytes": item["file_size"],
                "width": item["width"],
                "height": item["height"],
            }
            for item in uploaded
        ]
        message_parts.append(full_prompt)
        message: dict[str, Any] = {
            "id": str(uuid.uuid4()),
            "author": {"role": "user"},
            "create_time": time.time(),
            "content": {
                "content_type": "multimodal_text" if uploaded else "text",
                "parts": message_parts,
            },
            "metadata": {
                "attachments": [
                    {
                        "id": item["file_id"],
                        "size": item["file_size"],
                        "name": item["file_name"],
                        "mime_type": item["mime_type"],
                        "width": item["width"],
                        "height": item["height"],
                        "source": "library",
                        "library_file_id": item["library_file_id"],
                    }
                    for item in uploaded
                ],
            },
        }
        path = "/backend-api/f/conversation"
        extra = {
            "X-Conduit-Token": conduit_token,
            "OpenAI-Sentinel-Chat-Requirements-Token": requirements["token"],
            **(
                {"OpenAI-Sentinel-Turnstile-Token": requirements["turnstile"]}
                if requirements.get("turnstile")
                else {}
            ),
            **(
                {"OpenAI-Sentinel-Proof-Token": requirements["proof"]}
                if requirements.get("proof")
                else {}
            ),
            **(
                {"OpenAI-Sentinel-SO-Token": requirements["so_token"]}
                if requirements.get("so_token")
                else {}
            ),
        }
        client, owned = self._client()
        response: Any = None
        try:
            response = await client.post(
                f"{self.base_url}{path}",
                headers=self._headers(path, accept="text/event-stream", extra=extra),
                json={
                    "action": "next",
                    "messages": [message],
                    "parent_message_id": "client-created-root",
                    "model": str(model or "auto"),
                    "client_prepare_state": "sent",
                    "timezone_offset_min": -480,
                    "timezone": "Asia/Shanghai",
                    "conversation_mode": {"kind": "primary_assistant"},
                    "enable_message_followups": True,
                    "supports_buffering": True,
                    "supported_encodings": ["v1"],
                    "client_contextual_info": {"app_name": "chatgpt.com"},
                    "thinking_effort": "high",
                },
                timeout=self.timeout,
            )
            self._raise_for_status(response)
            _, conversation_id = self._sse_text(response.content)
        except httpx.TimeoutException as exc:
            raise ChatGPTTimeoutError() from exc
        except httpx.RequestError as exc:
            raise ChatGPTUpstreamUnavailableError() from exc
        finally:
            if response is not None:
                await self._close_resource(response)
            if owned:
                await self._close_resource(client)
        if not conversation_id:
            raise ChatGPTProtocolError("ChatGPT editable task did not return a conversation id")
        artifacts: list[dict[str, str]] = []
        for _ in range(60):
            document = await self._json_request(
                "GET", f"/backend-api/conversation/{conversation_id}", timeout=60
            )
            seen: set[str] = set()
            for item in self._walk_mappings(document):
                file_id = str(
                    item.get("file_id") or item.get("attachment_id") or item.get("id") or ""
                ).strip()
                name = str(
                    item.get("name")
                    or item.get("file_name")
                    or item.get("filename")
                    or item.get("title")
                    or ""
                ).strip()
                mime_type = str(item.get("mime_type") or item.get("mimeType") or "").lower()
                if not file_id or not name or file_id in seen:
                    continue
                if (
                    not any(suffix in name.lower() for suffix in (".ppt", ".pptx", ".psd", ".zip"))
                    and "powerpoint" not in mime_type
                    and "photoshop" not in mime_type
                ):
                    continue
                seen.add(file_id)
                artifacts.append({"file_id": file_id, "name": name, "mime_type": mime_type})
            primary_ext = (".ppt", ".pptx") if normalized_kind == "ppt" else (".psd",)
            has_primary = any(item["name"].lower().endswith(primary_ext) for item in artifacts)
            has_zip = any(item["name"].lower().endswith(".zip") for item in artifacts)
            if has_primary and has_zip:
                break
            await asyncio.sleep(2)
        if not artifacts:
            raise ChatGPTProtocolError(f"ChatGPT did not return {normalized_kind} artifacts")
        files: list[dict[str, Any]] = []
        for artifact in artifacts:
            content = await self._download_editable_artifact(conversation_id, artifact["file_id"])
            if content is not None:
                files.append({"name": artifact["name"], "content": content})
        return {"conversation_id": conversation_id, "files": files}

    async def _download_editable_artifact(self, conversation_id: str, file_id: str) -> bytes | None:
        del conversation_id
        paths = [f"/backend-api/files/download/{file_id}", f"/backend-api/files/{file_id}/download"]
        for path in paths:
            client, owned = self._client()
            response: Any = None
            try:
                response = await client.get(
                    f"{self.base_url}{path}", headers=self._headers(path, accept="*/*"), timeout=300
                )
                if response.status_code < 400:
                    content_type = str(response.headers.get("content-type") or "").lower()
                    if "json" not in content_type:
                        return bytes(response.content)
                    try:
                        value = response.json()
                    except (TypeError, ValueError):
                        value = {}
                    url = (
                        str(value.get("download_url") or value.get("url") or "")
                        if isinstance(value, Mapping)
                        else ""
                    )
                    if url:
                        download = await client.get(url, timeout=300)
                        if download.status_code < 400:
                            return bytes(download.content)
            except (httpx.TimeoutException, httpx.RequestError):
                continue
            finally:
                if response is not None:
                    await self._close_resource(response)
                if owned:
                    await self._close_resource(client)
        return None

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
