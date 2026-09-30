"""Native Doubao chat transport.

Doubao does not expose an OpenAI-compatible ``/v1`` data endpoint.  The web
client's ``/chat/completion`` endpoint accepts the same authenticated cookie
session used by QR login and returns SSE events.  This module owns that private
protocol and converts it to the gateway's OpenAI response contract.
"""

from __future__ import annotations

import json
import time
import uuid
from collections.abc import AsyncIterator, Mapping
from typing import Any

import httpx

from app.adapters.native_runtime import NativeHttpAdapter, NativeStream
from app.infrastructure.http import build_client

from .manifest import DOUBAO_MANIFEST

DOUBAO_COMPLETION_PATH = "/chat/completion"
DOUBAO_MODEL_ID = "doubao"
DOUBAO_DEFAULT_BOT_ID = "7338286299411103781"
DOUBAO_AID = "582478"
DEFAULT_DEVICE_ID = "714003710229497"
DEFAULT_WEB_ID = "7604137868021548590"
DEFAULT_FP = "verify_mlcfw5f7_TPq0YmFD_NrsC_4RuQ_BJPg_M5W7i58I7wV0"


class DoubaoUpstreamError(RuntimeError):
    """Safe, structured failure returned by the Doubao data plane."""

    def __init__(
        self,
        message: str,
        *,
        status_code: int = 502,
        code: str = "doubao_upstream_error",
        retryable: bool = False,
    ) -> None:
        super().__init__(message)
        self.message = message
        self.status_code = int(status_code)
        self.code = code
        self.retryable = bool(retryable)


def _text(value: Any) -> str:
    return str(value or "").strip()


def _credential_value(credentials: Mapping[str, Any], *names: str) -> str:
    normalized = {str(key).lower(): value for key, value in credentials.items()}
    for name in names:
        value = normalized.get(name.lower())
        if value not in (None, ""):
            return _text(value)
    return ""


def _cookie_header(credentials: Mapping[str, Any]) -> str:
    raw = credentials.get("Cookie") or credentials.get("cookie")
    if isinstance(raw, str) and raw.strip():
        return raw.strip()
    raw = credentials.get("cookies") or credentials.get("Cookies")
    if isinstance(raw, Mapping):
        return "; ".join(
            f"{key}={value}" for key, value in raw.items() if value not in (None, "")
        )
    if isinstance(raw, list):
        return "; ".join(
            f"{item.get('name')}={item.get('value')}"
            for item in raw
            if isinstance(item, Mapping) and item.get("name") and item.get("value") is not None
        )
    return ""


def _json_mapping(value: Any) -> Mapping[str, Any] | None:
    if isinstance(value, Mapping):
        return value
    if isinstance(value, str):
        try:
            parsed = json.loads(value)
        except (TypeError, ValueError, json.JSONDecodeError):
            return None
        return parsed if isinstance(parsed, Mapping) else None
    return None


def _message_text(value: Any) -> str:
    if isinstance(value, str):
        return value.strip()
    if isinstance(value, list):
        parts: list[str] = []
        for item in value:
            if isinstance(item, str):
                parts.append(item)
            elif isinstance(item, Mapping) and isinstance(item.get("text"), str):
                parts.append(str(item["text"]))
        return "".join(parts).strip()
    if isinstance(value, Mapping):
        for key in ("text", "content", "value"):
            if key in value:
                result = _message_text(value[key])
                if result:
                    return result
    return ""


def _prompt_from_messages(messages: Any) -> str:
    if not isinstance(messages, list) or not messages:
        raise DoubaoUpstreamError(
            "豆包请求缺少 messages，至少需要一条用户消息",
            status_code=400,
            code="invalid_request",
        )
    parts: list[str] = []
    for item in messages:
        if not isinstance(item, Mapping):
            raise DoubaoUpstreamError(
                "豆包 messages 中存在无效消息",
                status_code=400,
                code="invalid_request",
            )
        content = _message_text(item.get("content"))
        if not content:
            continue
        role = _text(item.get("role")) or "user"
        parts.append(f"[{role}]\n{content}")
    if not parts:
        raise DoubaoUpstreamError(
            "豆包请求中的 messages 没有可发送文本",
            status_code=400,
            code="invalid_request",
        )
    return "\n\n".join(parts)[-120_000:]


def _content_block_text(block: Any) -> str:
    if not isinstance(block, Mapping):
        return ""
    content = block.get("content")
    if not isinstance(content, Mapping):
        return ""
    text_block = content.get("text_block")
    if isinstance(text_block, Mapping):
        return _text(text_block.get("text"))
    return ""


def _event_text(value: Mapping[str, Any], event_name: str = "") -> str:
    if value.get("reply_id") == "0":
        return ""
    if event_name == "CHUNK_DELTA" and isinstance(value.get("text"), str):
        return str(value["text"])
    if isinstance(value.get("text"), str) and value.get("text"):
        return str(value["text"])
    for patch in value.get("patch_op", []):
        if isinstance(patch, Mapping):
            patch_value = patch.get("patch_value")
            if isinstance(patch_value, Mapping):
                for block in patch_value.get("content_block", []):
                    text = _content_block_text(block)
                    if text:
                        return text
    content = value.get("content")
    if isinstance(content, Mapping):
        for block in content.get("content_block", []):
            text = _content_block_text(block)
            if text:
                return text
        text = _message_text(content)
        if text:
            return text
    event_data = _json_mapping(value.get("event_data"))
    if event_data is not None and event_data is not value:
        return _event_text(event_data, event_name)
    message = value.get("message")
    if isinstance(message, Mapping):
        if message.get("reply_id") == "0":
            return ""
        message_content = _json_mapping(message.get("content")) or message.get("content")
        if isinstance(message_content, Mapping):
            return _message_text(message_content.get("text"))
    return ""


def _conversation_id(value: Mapping[str, Any]) -> str:
    for key in ("ack_client_meta", "meta"):
        nested = value.get(key)
        if isinstance(nested, Mapping):
            result = _text(nested.get("conversation_id"))
            if result and result != "0":
                return result
    event_data = _json_mapping(value.get("event_data"))
    if event_data is not None:
        return _conversation_id(event_data)
    return ""


def _error_from_event(value: Mapping[str, Any], event_name: str) -> tuple[str, str] | None:
    normalized_event = event_name.upper()
    if normalized_event in {"GATEWAY-ERROR", "STREAM_ERROR", "ERROR"}:
        code = _text(value.get("code") or value.get("error_code")) or "upstream_error"
        message = _text(value.get("message") or value.get("error_msg") or value.get("msg"))
        nested = value.get("error")
        if isinstance(nested, Mapping):
            code = _text(nested.get("code")) or code
            message = _text(nested.get("message")) or message
        return code, message or "豆包上游返回了错误"
    error = value.get("error")
    if isinstance(error, Mapping):
        return (
            _text(error.get("code")) or "upstream_error",
            _text(error.get("message")) or "豆包上游返回了错误",
        )
    if "error_code" in value and value.get("error_code") not in (None, "", 0, "0"):
        return (
            _text(value.get("error_code")) or "upstream_error",
            _text(value.get("error_msg")) or "豆包上游返回了错误",
        )
    event_data = _json_mapping(value.get("event_data"))
    if event_data is not None:
        return _error_from_event(event_data, normalized_event)
    return None


def _status_for_error(code: str, status_code: int = 502) -> tuple[int, str, bool]:
    value = code.lower()
    if value in {"401", "403", "710012001", "auth_required", "credential_expired"}:
        return 401, "credential_expired", False
    if value in {"429", "710022002", "710022004", "rate_limited"}:
        return 429, "rate_limited", True
    return status_code, "doubao_upstream_error", status_code >= 500


class DoubaoHttpTransport(NativeHttpAdapter):
    """Cookie-authenticated native transport for Doubao Web chat."""

    def __init__(
        self,
        base_url: str,
        *,
        credential_store: Any | None = None,
        http_client: httpx.AsyncClient | None = None,
        timeout: float = 180.0,
        connect_timeout: float = 10.0,
        bot_id: str = DOUBAO_DEFAULT_BOT_ID,
    ) -> None:
        super().__init__(
            DOUBAO_MANIFEST,
            base_url,
            http_client=http_client,
            timeout=timeout,
            connect_timeout=connect_timeout,
            credential_store=credential_store,
            channel="doubao",
        )
        self.bot_id = _text(bot_id) or DOUBAO_DEFAULT_BOT_ID

    def _client(self) -> tuple[httpx.AsyncClient, bool]:
        if self._http_client is not None:
            return self._http_client, False
        return (
            build_client(timeout=self.timeout, connect_timeout=self.connect_timeout),
            True,
        )

    def _query(self, credentials: Mapping[str, Any]) -> dict[str, str]:
        device_id = _credential_value(credentials, "device_id") or DEFAULT_DEVICE_ID
        web_id = _credential_value(credentials, "web_id") or DEFAULT_WEB_ID
        fp = _credential_value(credentials, "fp") or DEFAULT_FP
        params = {
            "aid": DOUBAO_AID,
            "real_aid": DOUBAO_AID,
            "device_id": device_id,
            "tea_uuid": device_id,
            "web_id": web_id,
            "device_platform": "web",
            "language": "zh",
            "region": "CN",
            "sys_region": "CN",
            "pkg_type": "release_version",
            "version_code": "20800",
            "pc_version": "2.1.7",
            "chromium_version": "131.0.0.0",
            "client_platform": "pc_client",
            "runtime": "web",
            "runtime_version": "3.5.4",
            "samantha_web": "1",
            "use-olympus-account": "1",
            "fp": fp,
            "web_tab_id": uuid.uuid4().hex,
        }
        ms_token = _credential_value(credentials, "mstoken", "msToken")
        if ms_token:
            params["msToken"] = ms_token
        return params

    def _headers(self, credentials: Mapping[str, Any]) -> dict[str, str]:
        cookie = _cookie_header(credentials)
        if not cookie:
            raise DoubaoUpstreamError(
                "豆包账号缺少 Cookie，请重新导入包含 sessionid 的凭据",
                status_code=401,
                code="credential_missing",
            )
        base_url = self.base_url.rstrip("/")
        headers = {
            "Accept": "text/event-stream",
            "Content-Type": "application/json",
            "Cookie": cookie,
            "Origin": _credential_value(credentials, "origin") or base_url,
            "Referer": _credential_value(credentials, "referer") or f"{base_url}/chat/",
            "User-Agent": _credential_value(credentials, "user-agent")
            or (
                "Mozilla/5.0 (Windows NT 10.0; Win64; x64) "
                "AppleWebKit/537.36 Chrome/131.0.0.0 Safari/537.36"
            ),
            "agw-js-conv": "str",
        }
        csrf = ""
        for item in cookie.split(";"):
            name, separator, value = item.strip().partition("=")
            if separator and name in {"passport_csrf_token", "passport_csrf_token_default"}:
                csrf = value
                break
        if csrf:
            headers["x-tt-passport-csrf-token"] = csrf
        return headers

    @staticmethod
    def _completion_payload(
        prompt: str,
        credentials: Mapping[str, Any],
        payload: Mapping[str, Any],
    ) -> dict[str, Any]:
        conversation_id = _text(payload.get("conversation_id"))
        need_create = not conversation_id
        now_ms = int(time.time() * 1000)
        now_sec = int(time.time())
        bot_id = _credential_value(credentials, "bot_id") or DOUBAO_DEFAULT_BOT_ID
        return {
            "client_meta": {
                "local_conversation_id": f"local_{uuid.uuid4().hex[:16]}" if need_create else "",
                "conversation_id": conversation_id,
                "bot_id": bot_id,
                "last_section_id": "",
                "last_message_index": None,
            },
            "messages": [
                {
                    "local_message_id": str(uuid.uuid4()),
                    "content_block": [
                        {
                            "block_type": 10000,
                            "content": {
                                "text_block": {
                                    "text": prompt,
                                    "icon_url": "",
                                    "icon_url_dark": "",
                                    "summary": "",
                                },
                                "pc_event_block": "",
                            },
                            "block_id": str(uuid.uuid4()),
                            "parent_id": "",
                            "append_fields": [],
                        }
                    ],
                    "message_status": 0,
                }
            ],
            "option": {
                "send_message_scene": "",
                "create_time_ms": now_ms,
                "collect_id": "",
                "is_audio": False,
                "answer_with_suggest": False,
                "tts_switch": False,
                "need_deep_think": 0,
                "click_clear_context": False,
                "from_suggest": False,
                "is_regen": False,
                "is_replace": False,
                "disable_sse_cache": False,
                "select_text_action": "",
                "resend_for_regen": False,
                "scene_type": 0,
                "unique_key": str(uuid.uuid4()),
                "start_seq": 0,
                "need_create_conversation": need_create,
                "regen_query_id": [],
                "edit_query_id": [],
                "regen_instruction": "",
                "no_replace_for_regen": False,
                "message_from": 0,
                "shared_app_name": "",
                "shared_app_id": "",
                "sse_recv_event_options": {"support_chunk_delta": True},
                "is_ai_playground": False,
                "recovery_option": {
                    "is_recovery": False,
                    "req_create_time_sec": now_sec,
                    "append_sse_event_scene": 0,
                },
                "message_storage_type": 0,
            },
            "ext": {
                "use_deep_think": "0",
                "fp": _credential_value(credentials, "fp") or DEFAULT_FP,
                "collection_id": "",
                "commerce_credit_config_enable": "0",
                "sub_conv_firstmet_type": "1" if need_create else "0",
            },
        }

    async def _upstream(self, payload: Mapping[str, Any], credentials: Mapping[str, Any]) -> bytes:
        prompt = _prompt_from_messages(payload.get("messages"))
        body = self._completion_payload(prompt, credentials, payload)
        client, owned = self._client()
        url = f"{self.base_url.rstrip('/')}{DOUBAO_COMPLETION_PATH}"
        try:
            try:
                response = await client.post(
                    url,
                    params=self._query(credentials),
                    headers=self._headers(credentials),
                    json=body,
                )
            except httpx.TimeoutException as exc:
                raise DoubaoUpstreamError(
                    "豆包上游请求超时，请稍后重试",
                    status_code=504,
                    code="upstream_timeout",
                    retryable=True,
                ) from exc
            except httpx.RequestError as exc:
                raise DoubaoUpstreamError(
                    "豆包上游连接失败，请检查网络或稍后重试",
                    status_code=502,
                    code="upstream_unavailable",
                    retryable=True,
                ) from exc
            raw = bytes(response.content)
            if response.status_code >= 400:
                self._raise_upstream_error(response.status_code, raw)
            content_type = _text(response.headers.get("content-type")).lower()
            if "text/event-stream" not in content_type:
                self._raise_unexpected_body(content_type, raw)
            return raw
        finally:
            if owned:
                await client.aclose()

    @staticmethod
    def _raise_upstream_error(status_code: int, raw: bytes) -> None:
        message = ""
        code = str(status_code)
        try:
            value = json.loads(raw.decode("utf-8", "replace"))
        except (TypeError, ValueError, json.JSONDecodeError):
            value = None
        if isinstance(value, Mapping):
            nested = value.get("error")
            if isinstance(nested, Mapping):
                code = _text(nested.get("code")) or code
                message = _text(nested.get("message"))
            else:
                code = _text(value.get("code")) or code
                message = _text(value.get("msg") or value.get("message"))
        status, mapped_code, retryable = _status_for_error(code, status_code)
        if not message:
            message = (
                "豆包账号凭据已失效，请重新导入 Cookie"
                if mapped_code == "credential_expired"
                else "豆包上游暂时拒绝了请求，请稍后重试"
                if mapped_code == "rate_limited"
                else f"豆包上游请求失败（HTTP {status_code}）"
            )
        raise DoubaoUpstreamError(
            message[:500],
            status_code=status,
            code=mapped_code,
            retryable=retryable,
        )

    @staticmethod
    def _raise_unexpected_body(content_type: str, raw: bytes) -> None:
        try:
            value = json.loads(raw.decode("utf-8", "replace"))
        except (TypeError, ValueError, json.JSONDecodeError):
            value = None
        if isinstance(value, Mapping):
            code = _text(value.get("code")) or "upstream_error"
            message = _text(value.get("msg") or value.get("message")) or "豆包上游返回了非流式错误"
            status, mapped, retryable = _status_for_error(code)
            raise DoubaoUpstreamError(
                message[:500],
                status_code=status,
                code=mapped,
                retryable=retryable,
            )
        raise DoubaoUpstreamError(
            f"豆包上游返回了无法识别的响应（content-type={content_type or 'unknown'}）",
            code="protocol_error",
        )

    @staticmethod
    def _parse(raw: bytes) -> tuple[list[str], str]:
        chunks: list[str] = []
        conversation_id = ""
        normalized = raw.decode("utf-8", "replace").replace("\r\n", "\n")
        for frame in normalized.split("\n\n"):
            if not frame.strip():
                continue
            event_name = ""
            data_lines: list[str] = []
            for line in frame.split("\n"):
                if line.startswith("event:"):
                    event_name = line[6:].strip()
                elif line.startswith("data:"):
                    data_lines.append(line[5:].lstrip(" "))
            data = "\n".join(data_lines).strip()
            if not data or data == "[DONE]":
                continue
            try:
                value = json.loads(data)
            except (TypeError, ValueError, json.JSONDecodeError):
                continue
            if not isinstance(value, Mapping):
                continue
            error = _error_from_event(value, event_name)
            if error is not None:
                code, message = error
                status, mapped_code, retryable = _status_for_error(code)
                raise DoubaoUpstreamError(
                    message[:500], status_code=status, code=mapped_code, retryable=retryable
                )
            conversation_id = conversation_id or _conversation_id(value)
            text = _event_text(value, event_name)
            if text:
                chunks.append(text)
        if not chunks:
            raise DoubaoUpstreamError(
                "豆包上游返回了空内容，可能是登录失效或平台协议已变化",
                code="empty_response",
            )
        return chunks, conversation_id

    @staticmethod
    def _openai_response(model: str, chunks: list[str], conversation_id: str) -> dict[str, Any]:
        content = "".join(chunks)
        response: dict[str, Any] = {
            "id": f"chatcmpl-{uuid.uuid4().hex[:24]}",
            "object": "chat.completion",
            "created": int(time.time()),
            "model": model or DOUBAO_MODEL_ID,
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
        }
        if conversation_id:
            response["conversation_id"] = conversation_id
        return response

    @staticmethod
    def _openai_stream(model: str, chunks: list[str], conversation_id: str) -> bytes:
        response_id = f"chatcmpl-{uuid.uuid4().hex[:24]}"
        created = int(time.time())
        frames = [
            {
                "id": response_id,
                "object": "chat.completion.chunk",
                "created": created,
                "model": model or DOUBAO_MODEL_ID,
                "choices": [{"index": 0, "delta": {"role": "assistant"}, "finish_reason": None}],
            }
        ]
        frames.extend(
            {
                "id": response_id,
                "object": "chat.completion.chunk",
                "created": created,
                "model": model or DOUBAO_MODEL_ID,
                "choices": [{"index": 0, "delta": {"content": chunk}, "finish_reason": None}],
            }
            for chunk in chunks
            if chunk
        )
        final: dict[str, Any] = {
            "id": response_id,
            "object": "chat.completion.chunk",
            "created": created,
            "model": model or DOUBAO_MODEL_ID,
            "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
        }
        if conversation_id:
            final["conversation_id"] = conversation_id
        frames.append(final)
        return b"".join(
            f"data: {json.dumps(frame, ensure_ascii=False, separators=(',', ':'))}\n\n".encode()
            for frame in frames
        ) + b"data: [DONE]\n\n"

    async def list_models(self, context: Any = None) -> list[Mapping[str, Any]]:
        credentials = await self._account_credentials(context)
        if not _cookie_header(credentials):
            return []
        return [
            {
                "id": DOUBAO_MODEL_ID,
                "object": "model",
                "created": 0,
                "owned_by": "doubao",
                "caps": ["chat"],
            }
        ]

    async def health(self, context: Any = None) -> Mapping[str, Any]:
        credentials = await self._account_credentials(context)
        if not _cookie_header(credentials):
            return {
                "status": "no_credentials",
                "message": "豆包账号缺少 Cookie，请先导入或完成扫码登录",
            }
        return {"status": "configured", "transport": "http"}

    async def invoke(self, request: Any, account: Any = None) -> httpx.Response:
        _model, payload, _headers, _stream = self._payload(request)
        credentials = await self._account_credentials(account)
        try:
            raw = await self._upstream(payload, credentials)
            chunks, conversation_id = self._parse(raw)
            model = _text(payload.get("model")) or DOUBAO_MODEL_ID
            if bool(payload.get("stream")):
                body = self._openai_stream(model, chunks, conversation_id)
                return httpx.Response(
                    200,
                    headers={"content-type": "text/event-stream"},
                    content=body,
                    request=httpx.Request("POST", f"{self.base_url}{DOUBAO_COMPLETION_PATH}"),
                )
            return httpx.Response(
                200,
                headers={"content-type": "application/json"},
                json=self._openai_response(model, chunks, conversation_id),
                request=httpx.Request("POST", f"{self.base_url}{DOUBAO_COMPLETION_PATH}"),
            )
        except DoubaoUpstreamError as exc:
            return httpx.Response(
                exc.status_code,
                headers={"content-type": "application/json"},
                json={"error": {"message": exc.message, "code": exc.code}},
                request=httpx.Request("POST", f"{self.base_url}{DOUBAO_COMPLETION_PATH}"),
            )

    async def open_stream(self, request: Any, account: Any = None) -> NativeStream:
        response = await self.invoke({**dict(request), "stream": True}, account)
        client = build_client(timeout=self.timeout, connect_timeout=self.connect_timeout)
        return NativeStream(client=client, response=response)

    async def invoke_stream(self, request: Any, account: Any = None) -> AsyncIterator[bytes]:
        handle = await self.open_stream(request, account)
        try:
            async for chunk in handle.response.aiter_bytes():
                yield chunk
        finally:
            await handle.response.aclose()
            await handle.client.aclose()

    async def chat(self, request: Any, account: Any = None) -> httpx.Response:
        return await self.invoke(request, account)

    async def chat_stream(self, request: Any, account: Any = None) -> AsyncIterator[bytes]:
        async for chunk in self.invoke_stream(request, account):
            yield chunk

    def map_error(self, error: Exception) -> Mapping[str, Any]:
        if isinstance(error, DoubaoUpstreamError):
            return {
                "code": error.code,
                "message": error.message,
                "retryable": error.retryable,
                "status_code": error.status_code,
            }
        if isinstance(error, httpx.TimeoutException):
            return {
                "code": "upstream_timeout",
                "message": "豆包上游请求超时，请稍后重试",
                "retryable": True,
                "status_code": 504,
            }
        return {
            "code": "upstream_unavailable",
            "message": "豆包上游连接失败，请检查网络或稍后重试",
            "retryable": True,
            "status_code": 502,
        }


__all__ = [
    "DOUBAO_COMPLETION_PATH",
    "DOUBAO_DEFAULT_BOT_ID",
    "DOUBAO_MODEL_ID",
    "DoubaoHttpTransport",
    "DoubaoUpstreamError",
]
