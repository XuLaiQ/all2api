from __future__ import annotations

import base64
import hashlib
import json
import random
import re
import time
import uuid
from collections.abc import Mapping
from typing import Any

import httpx

from .errors import (
    ChatGPTAuthError,
    ChatGPTInvalidRequestError,
    ChatGPTProtocolError,
    ChatGPTRateLimitError,
    ChatGPTTimeoutError,
    ChatGPTUpstreamUnavailableError,
)
from .manifest import CHATGPT_WEB_BASE_URL


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
        self.device_id = str(self.credentials.get("oai_device_id") or uuid.uuid4())
        self.session_id = str(self.credentials.get("oai_session_id") or uuid.uuid4())
        self.user_agent = str(
            self.credentials.get("user_agent")
            or "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
            "(KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36"
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
            "OAI-Device-Id": self.device_id,
            "OAI-Session-Id": self.session_id,
            "OAI-Language": "zh-CN",
            "OAI-Client-Version": "all2api-native",
            "X-OpenAI-Target-Path": path,
            "X-OpenAI-Target-Route": path.split("?", 1)[0],
        }
        if extra:
            headers.update({str(key): str(value) for key, value in extra.items()})
        return headers

    def _client(self) -> tuple[httpx.AsyncClient, bool]:
        if self._http_client is not None:
            return self._http_client, False
        return (
            httpx.AsyncClient(
                timeout=httpx.Timeout(self.timeout, connect=self.connect_timeout)
            ),
            True,
        )

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
                await client.aclose()

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
        finally:
            if owned:
                await client.aclose()

    def _requirements_token(self) -> str:
        sources = self._script_sources or [
            "https://chatgpt.com/backend-api/sentinel/sdk.js"
        ]
        navigator_keys = (
            "webdriver-false",
            "vendor-Google Inc.",
            "language-zh-CN",
            "hardwareConcurrency-32",
        )
        window_keys = ("location", "document", "navigator", "performance", "crypto")
        resolutions = ((1920, 1080), (1440, 900), (2560, 1440), (3840, 2160))
        width, height = random.choice(resolutions)
        config = [
            f"{width}x{height}",
            time.strftime(
                "%a %b %d %Y %H:%M:%S GMT-0500 (Eastern Standard Time)",
                time.localtime(),
            ),
            4294705152,
            1,
            self.user_agent,
            random.choice(sources),
            self._data_build,
            "en-US",
            "en-US,es-US,en,es",
            random.random(),
            random.choice(navigator_keys),
            "location",
            random.choice(window_keys),
            time.perf_counter() * 1000,
            str(uuid.uuid4()),
            "",
            random.choice((8, 16, 24, 32)),
            time.time() * 1000,
            0,
            0,
            0,
            0,
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
        prepared = await self._json_request(
            "POST",
            f"{base}/prepare",
            json_body={"p": str(self.credentials.get("sentinel_p") or self._requirements_token())},
            timeout=30,
        )
        if (prepared.get("arkose") or {}).get("required"):
            raise ChatGPTProtocolError("ChatGPT requires an unsupported Arkose challenge")
        if (prepared.get("turnstile") or {}).get("required"):
            raise ChatGPTProtocolError("ChatGPT requires an unsupported Turnstile challenge")
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
                "turnstile_token": "",
            },
            timeout=30,
        )
        token = str(finalized.get("token") or "").strip()
        if not token:
            raise ChatGPTProtocolError("ChatGPT did not return a conversation requirement token")
        return {
            "token": token,
            "proof": proof,
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
                await response.aclose()
            if owned:
                await client.aclose()

    async def invoke(self, request: Mapping[str, Any]) -> httpx.Response:
        return await self.chat(request)


__all__ = ["ChatGPTWebClient"]
