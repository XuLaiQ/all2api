"""Native Doubao chat transport.

Doubao does not expose an OpenAI-compatible ``/v1`` data endpoint. The stable
web client path used by the reference implementation is
``/alice/message/stream_call_bot`` with a base64-encoded event payload. This
module owns that private protocol and converts it to the gateway contract.
"""

from __future__ import annotations

import base64
import binascii
import hashlib
import json
import re
import time
import uuid
from collections.abc import AsyncIterator, Mapping
from typing import Any

import httpx

from app.adapters.native_runtime import NativeHttpAdapter, NativeStream
from app.infrastructure.http import build_client

from .manifest import DOUBAO_MANIFEST

DOUBAO_ALICE_COMPLETION_PATH = "/alice/message/stream_call_bot"
DOUBAO_SAMANTHA_COMPLETION_PATH = "/samantha/chat/completion"
DOUBAO_MODEL_CATALOGUE_PATH = "/alice/slot/action_bar_v3/brief_list"
DOUBAO_ACTION_BAR_ITEM_CONFIG_PATH = "/alice/slot/action_bar_v3/get_item_conf"
DOUBAO_CHAT_PAGE_PATH = "/chat/"
DOUBAO_DEFAULT_BOT_ID = "7338286299411103781"
DOUBAO_AID = "582478"
DEFAULT_DEVICE_ID = "714003710229497"
DEFAULT_WEB_ID = "7604137868021548590"
DEFAULT_FP = "verify_mlcfw5f7_TPq0YmFD_NrsC_4RuQ_BJPg_M5W7i58I7wV0"
_THINK_BLOCK_RE = re.compile(r"<think\b[^>]*>.*?</think\s*>", re.IGNORECASE | re.DOTALL)
_THINK_TAG_RE = re.compile(r"</?think\b[^>]*>", re.IGNORECASE)


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


def _model_item_id(value: Any) -> str:
    if value is None:
        return ""
    return str(value).strip()


def _credential_scope(credentials: Mapping[str, Any]) -> str:
    cookie = _cookie_header(credentials)
    return hashlib.sha256(cookie.encode("utf-8")).hexdigest()[:24] if cookie else ""


def _strip_think_markup(value: str) -> str:
    """Remove provider reasoning markup from the user-visible answer."""

    return _THINK_TAG_RE.sub("", _THINK_BLOCK_RE.sub("", value))


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


def _router_data_from_html(value: str) -> Mapping[str, Any]:
    """Decode the JSON state embedded in Doubao's server-rendered chat page."""

    marker = "window._ROUTER_DATA ="
    marker_index = value.find(marker)
    if marker_index < 0:
        raise DoubaoUpstreamError("豆包模型接口未返回路由配置", code="catalogue_invalid")
    start = value.find("{", marker_index + len(marker))
    if start < 0:
        raise DoubaoUpstreamError("豆包模型接口返回格式不正确", code="catalogue_invalid")
    try:
        parsed, _ = json.JSONDecoder().raw_decode(value[start:])
    except (TypeError, ValueError, json.JSONDecodeError) as exc:
        raise DoubaoUpstreamError("豆包模型接口返回了无效目录", code="catalogue_invalid") from exc
    if not isinstance(parsed, Mapping):
        raise DoubaoUpstreamError("豆包模型接口返回格式不正确", code="catalogue_invalid")
    return parsed


def _catalogue_root(value: Any) -> Mapping[str, Any] | None:
    current = _json_mapping(value)
    for _ in range(4):
        if current is None:
            return None
        if any(
            key in current
            for key in ("modeSelectData", "mode_select_data", "menuConfV2", "mode_list")
        ):
            return current
        nested = next(
            (
                current.get(key)
                for key in (
                    "data",
                    "loaderData",
                    "chat_layout",
                    "actionBarBriefList",
                    "modeSelectData",
                    "menuConfV2",
                )
                if isinstance(current.get(key), Mapping)
            ),
            None,
        )
        if nested is None:
            return current
        current = nested
    return current


def _iter_model_items(
    value: Any,
    *,
    mode_id: str = "",
) -> list[tuple[Mapping[str, Any], str]]:
    """Collect model entries from the versioned mode-select response shape."""

    if isinstance(value, list):
        result: list[tuple[Mapping[str, Any], str]] = []
        for item in value:
            result.extend(_iter_model_items(item, mode_id=mode_id))
        return result
    if not isinstance(value, Mapping):
        return []

    result = []
    for key in ("mode_list", "modeList"):
        mode_list = value.get(key)
        if not isinstance(mode_list, Mapping):
            continue
        for mode in mode_list.get("item_list", []):
            if not isinstance(mode, Mapping):
                continue
            next_mode_id = _model_item_id(mode.get("mode_id") or mode.get("modeId")) or mode_id
            result.extend(_iter_model_items(mode, mode_id=next_mode_id))
    for key in ("model_list", "modelList"):
        model_list = value.get(key)
        if not isinstance(model_list, Mapping):
            continue
        for item in model_list.get("item_list", []):
            if isinstance(item, Mapping):
                result.append((item, mode_id))

    for key, child in value.items():
        if key in {"mode_list", "modeList", "model_list", "modelList"}:
            continue
        result.extend(_iter_model_items(child, mode_id=mode_id))
    return result


def _catalogue_models(value: Any) -> list[dict[str, Any]]:
    root = _catalogue_root(value)
    if root is None:
        return []
    candidates: list[Any] = [root]
    for key in ("modeSelectData", "mode_select_data", "menuConfV2", "menu_conf_v2"):
        nested = root.get(key)
        if isinstance(nested, Mapping):
            candidates.append(nested)

    models: list[dict[str, Any]] = []
    seen: set[str] = set()
    for candidate in candidates:
        for item, mode_id in _iter_model_items(candidate):
            model_id = next(
                (
                    _model_item_id(item.get(key))
                    for key in ("model_item_key", "modelItemKey", "model_id", "id", "item_id")
                    if _model_item_id(item.get(key))
                ),
                "",
            )
            if not model_id or model_id in seen:
                continue
            subscription = item.get("subscribe_config")
            if item.get("need_login") is True or (
                isinstance(subscription, Mapping) and subscription.get("need_upgrade") is True
            ):
                continue
            seen.add(model_id)
            extra = item.get("model_extra_params")
            model = {
                "id": model_id,
                "name": _text(item.get("name") or item.get("display_name") or model_id),
                "object": "model",
                "created": 0,
                "owned_by": "doubao",
                "kind": "chat",
                "caps": ["chat"],
                "model_item_key": model_id,
                "mode_id": mode_id,
            }
            if isinstance(extra, Mapping):
                model["model_extra_params"] = dict(extra)
                window = extra.get("total_window_size") or extra.get("context_window")
                try:
                    if window is not None:
                        model["context_window"] = int(window)
                except (TypeError, ValueError):
                    pass
            if item.get("agent_mode") is not None:
                model["agent_mode"] = item["agent_mode"]
            models.append(model)
    return models


def _samantha_events(raw: bytes) -> list[Mapping[str, Any]]:
    """Decode Samantha's event-data envelopes without exposing raw payloads."""

    events: list[Mapping[str, Any]] = []
    normalized = raw.decode("utf-8", "replace").replace("\r\n", "\n")
    for frame in normalized.split("\n\n"):
        data = "\n".join(
            line[5:].lstrip(" ") for line in frame.split("\n") if line.startswith("data:")
        ).strip()
        if not data or data == "[DONE]":
            continue
        try:
            value = json.loads(data)
        except (TypeError, ValueError, json.JSONDecodeError):
            continue
        if isinstance(value, Mapping):
            events.append(value)
    return events


def _samantha_event_data(event: Mapping[str, Any]) -> Mapping[str, Any]:
    value = _decode_json_layers(event.get("event_data"))
    return value if isinstance(value, Mapping) else {}


def _samantha_message(event: Mapping[str, Any]) -> Mapping[str, Any]:
    value = _decode_json_layers(_samantha_event_data(event).get("message"))
    return value if isinstance(value, Mapping) else {}


def _samantha_content(message: Mapping[str, Any]) -> Any:
    value = _decode_json_layers(message.get("content"))
    return value


def _decode_json_layers(value: Any, *, limit: int = 5) -> Any:
    for _ in range(limit):
        if not isinstance(value, str):
            return value
        text = value.strip()
        if not text or text[0] not in "[{":
            return value
        try:
            decoded = json.loads(text)
        except (TypeError, ValueError, json.JSONDecodeError):
            return value
        if decoded == value:
            return value
        value = decoded
    return value


def _first_media_url(value: Any, keys: tuple[str, ...]) -> str:
    value = _decode_json_layers(value)
    if isinstance(value, Mapping):
        for key in keys:
            candidate = value.get(key)
            if isinstance(candidate, str):
                if candidate.startswith(("http://", "https://")):
                    return candidate
                if key == "main_url" or key.endswith("_raw"):
                    try:
                        decoded = base64.b64decode(candidate).decode("utf-8", "replace").strip()
                    except (binascii.Error, ValueError, UnicodeDecodeError):
                        decoded = ""
                    if decoded.startswith(("http://", "https://")):
                        return decoded
            elif isinstance(candidate, (Mapping, list)):
                found = _first_media_url(candidate, keys)
                if found:
                    return found
        for child in value.values():
            found = _first_media_url(child, keys)
            if found:
                return found
    elif isinstance(value, list):
        for child in value:
            found = _first_media_url(child, keys)
            if found:
                return found
    return ""


def _json_value(value: Any) -> Any:
    if not isinstance(value, str):
        return value
    try:
        return json.loads(value)
    except (TypeError, ValueError, json.JSONDecodeError):
        return value


def _generation_option_models(
    payload: Any,
    entry_types: Mapping[str, int],
) -> list[dict[str, Any]]:
    """Map Doubao's per-skill selector config to model catalogue records."""

    if not isinstance(payload, Mapping):
        return []
    data = payload.get("data")
    item_list = data.get("item_list") if isinstance(data, Mapping) else None
    if not isinstance(item_list, Mapping):
        return []
    results: list[dict[str, Any]] = []
    seen: set[tuple[str, str]] = set()
    for item_id, item in item_list.items():
        if not isinstance(item, Mapping):
            continue
        instruction_type = int(entry_types.get(str(item_id), 0) or 0)
        kind = "image" if instruction_type == 4 else "video" if instruction_type == 18 else ""
        instruction_conf = item.get("instruction_conf")
        if not isinstance(instruction_conf, Mapping):
            continue
        instruction_items = instruction_conf.get("instruction_item_list")
        if not isinstance(instruction_items, list):
            continue
        for instruction in instruction_items:
            if not isinstance(instruction, Mapping):
                continue
            template = _json_value(instruction.get("template"))
            if not isinstance(template, Mapping):
                continue
            selectors = template.get("selector_list")
            if not isinstance(selectors, list):
                continue
            for selector in selectors:
                if not isinstance(selector, Mapping):
                    continue
                config = _json_value(selector.get("template"))
                if not isinstance(config, Mapping):
                    continue
                key = _text(config.get("key"))
                selector_kind = ""
                if key == "model" and kind == "image":
                    selector_kind = "image"
                elif key == "video-model" and kind == "video":
                    selector_kind = "video"
                if selector_kind not in {"image", "video"}:
                    continue
                options = config.get("option_list")
                if not isinstance(options, list):
                    continue
                for option in options:
                    if not isinstance(option, Mapping):
                        continue
                    option_key = _text(
                        option.get("option_key")
                        or option.get("value")
                        or option.get("display_text")
                    )
                    display_name = _text(option.get("display_text") or option_key)
                    if not option_key or not display_name:
                        continue
                    dedupe_key = (selector_kind, option_key)
                    if dedupe_key in seen:
                        continue
                    seen.add(dedupe_key)
                    extra = option.get("extra") if isinstance(option.get("extra"), Mapping) else {}
                    results.append(
                        {
                            "id": option_key,
                            "name": display_name,
                            "kind": selector_kind,
                            "caps": [selector_kind],
                            "owned_by": "doubao",
                            "option_key": option_key,
                            "description": _text(extra.get("sub_display")),
                            "skill_type": 3 if selector_kind == "image" else 17,
                        }
                    )
    return results


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
        self._model_specs: dict[str, dict[str, dict[str, Any]]] = {}

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

    async def _samantha_request(
        self,
        credentials: Mapping[str, Any],
        payload: Mapping[str, Any],
        *,
        timeout: float,
    ) -> bytes:
        """Call the authenticated Doubao multimedia endpoint."""

        client, owned = self._client()
        try:
            response = await client.post(
                self._url(DOUBAO_SAMANTHA_COMPLETION_PATH, credentials),
                params=self._query(credentials),
                headers=self._headers(credentials),
                json=dict(payload),
                timeout=timeout,
            )
            raw = bytes(response.content)
            if response.status_code >= 400:
                self._raise_upstream_error(response.status_code, raw)
            return raw
        except httpx.TimeoutException as exc:
            raise DoubaoUpstreamError(
                "豆包多媒体请求超时，请稍后重试",
                status_code=504,
                code="upstream_timeout",
                retryable=True,
            ) from exc
        except httpx.RequestError as exc:
            raise DoubaoUpstreamError(
                "豆包多媒体连接失败，请检查网络或稍后重试",
                status_code=502,
                code="upstream_unavailable",
                retryable=True,
            ) from exc
        finally:
            if owned:
                await client.aclose()

    @staticmethod
    def _samantha_payload(
        prompt: str,
        *,
        skill_id: int,
        content_type: int,
        ratio: str = "",
        model: str = "",
        duration: int | None = None,
    ) -> dict[str, Any]:
        content: dict[str, Any] = {"text": prompt}
        if ratio:
            content["ratio"] = ratio
        if model:
            content["model"] = model
        if duration is not None:
            content["duration"] = int(duration)
        variables: dict[str, Any] = {
            "style": "",
            "ratio": ratio,
            "model": model,
            "template_type": "placeholder",
        }
        if duration is not None:
            variables["duration"] = str(duration)
        input_skill = {
            "skill_id": str(skill_id),
            "skill_type": skill_id,
            "variables": variables,
        }
        samantha_context = json.dumps(
            {"query_context": variables}, ensure_ascii=False, separators=(",", ":")
        )
        extra_ext = {
            "input_skill": json.dumps(input_skill, ensure_ascii=False),
            "answer_with_suggest": "0",
            "samantha_context": samantha_context,
        }
        message = {
            "content": json.dumps(content, ensure_ascii=False),
            "content_type": content_type,
            "attachments": [],
            "references": [],
            "skill": {
                "skill_type": skill_id,
                "skill_type_no_default": skill_id,
                "skill_id": str(skill_id),
                "skill_id_no_default": str(skill_id),
            },
            # These fields mirror the Web send-message envelope. Older
            # Samantha deployments ignore the extra metadata, while newer
            # deployments use it to select Seedream/Seedance variants.
            "ext": {"samantha_context": samantha_context},
            "extra_ext": extra_ext,
            "extraExt": extra_ext,
        }
        return {
            "messages": [message],
            "completion_option": {
                "is_regen": False,
                "with_suggest": True,
                "need_create_conversation": True,
                "launch_stage": 1,
                "is_replace": False,
                "is_delete": False,
                "is_ai_playground": False,
                "memory_type": 2,
                "message_from": 0,
                "use_deep_think": False,
                "use_auto_cot": False,
                "resend_for_regen": False,
                "enable_commerce_credit": False,
                "action_bar_skill_id": skill_id,
            },
            "evaluate_option": {"web_ab_params": ""},
            "local_conversation_id": str(uuid.uuid4()),
            "local_message_id": str(uuid.uuid4()),
        }

    @staticmethod
    def _samantha_error(raw: bytes, operation: str) -> None:
        for event in _samantha_events(raw):
            event_data = _samantha_event_data(event)
            code = event_data.get("code") or event_data.get("error_code") or event.get("code")
            message = (
                event_data.get("message")
                or event_data.get("msg")
                or event_data.get("error_msg")
                or event.get("message")
            )
            if code not in (None, "", 0, "0"):
                status, mapped, retryable = _status_for_error(str(code))
                raise DoubaoUpstreamError(
                    _text(message) or f"豆包{operation}失败",
                    status_code=status,
                    code=mapped,
                    retryable=retryable,
                )

    async def generate_image(
        self,
        prompt: str,
        *,
        ratio: str = "",
        model: str = "",
        account: Any = None,
    ) -> dict[str, Any]:
        credentials = await self._account_credentials(account)
        if not _cookie_header(credentials):
            raise DoubaoUpstreamError(
                "豆包账号缺少 Cookie，请先导入或完成扫码登录",
                status_code=401,
                code="credential_missing",
            )
        raw = await self._samantha_request(
            credentials,
            self._samantha_payload(
                prompt,
                skill_id=3,
                content_type=2009,
                ratio=ratio,
                model=model,
            ),
            timeout=180,
        )
        self._samantha_error(raw, "生图")
        raw_images: list[dict[str, Any]] = []
        fallback_images: list[dict[str, Any]] = []
        for event in _samantha_events(raw):
            message = _samantha_message(event)
            if int(message.get("content_type") or 0) != 2010:
                continue
            content = _samantha_content(message)
            if isinstance(content, Mapping) and isinstance(content.get("data"), list):
                values = content["data"]
            elif isinstance(content, list):
                values = content
            else:
                values = [content]
            for item in values:
                url = _first_media_url(
                    item, ("image_ori_raw", "image_raw", "image_ori", "image_thumb", "url")
                )
                if url:
                    has_raw_variant = (
                        _first_media_url(item, ("image_ori_raw", "image_raw", "url")) == url
                    )
                    target = raw_images if has_raw_variant else fallback_images
                    target.append(
                        {
                            "url": url,
                            "watermark_free": has_raw_variant,
                            "source_variant": "image_ori_raw" if has_raw_variant else "fallback",
                        }
                    )
        images = raw_images or fallback_images
        if not images:
            raise DoubaoUpstreamError(
                "豆包没有返回生成图片，可能是账号风控或上游协议变化",
                code="empty_media_response",
            )
        return {"created": int(time.time()), "data": images}

    async def generate_video(
        self,
        prompt: str,
        *,
        ratio: str = "",
        model: str = "",
        duration: int | None = None,
        account: Any = None,
    ) -> dict[str, Any]:
        credentials = await self._account_credentials(account)
        if not _cookie_header(credentials):
            raise DoubaoUpstreamError(
                "豆包账号缺少 Cookie，请先导入或完成扫码登录",
                status_code=401,
                code="credential_missing",
            )
        raw = await self._samantha_request(
            credentials,
            self._samantha_payload(
                prompt,
                skill_id=17,
                content_type=2020,
                ratio=ratio,
                model=model,
                duration=duration,
            ),
            timeout=120,
        )
        self._samantha_error(raw, "视频生成")
        task_id = ""
        for event in _samantha_events(raw):
            event_data = _samantha_event_data(event)
            finish_reason = event_data.get("fin_reason")
            if isinstance(finish_reason, Mapping):
                task = finish_reason.get("async_task")
                if isinstance(task, Mapping):
                    task_id = _text(task.get("id")) or task_id
        if task_id:
            raw = await self._samantha_request(
                credentials,
                {"task_id": task_id, "event_id": 0},
                timeout=360,
            )
            self._samantha_error(raw, "视频生成")
        videos: list[dict[str, Any]] = []
        for event in _samantha_events(raw):
            message = _samantha_message(event)
            if int(message.get("content_type") or 0) != 2021:
                continue
            content = _samantha_content(message)
            if isinstance(content, Mapping) and isinstance(content.get("data"), list):
                values = content["data"]
            elif isinstance(content, list):
                values = content
            else:
                values = [content]
            for item in values:
                url = _first_media_url(item, ("video_url", "main_url", "url"))
                if url:
                    videos.append({"url": url, "video_url": url})
        if not videos:
            raise DoubaoUpstreamError(
                "豆包没有返回生成视频，可能是任务仍在排队或上游协议变化",
                code="empty_media_response",
            )
        return {"created": int(time.time()), "data": videos}

    def _completion_payload(
        self,
        prompt: str,
        credentials: Mapping[str, Any],
        payload: Mapping[str, Any],
    ) -> dict[str, Any]:
        model = _model_item_id(payload.get("model"))
        if "/" in model:
            model = model.rsplit("/", 1)[-1]
        scope = _credential_scope(credentials)
        model_spec = self._model_specs.get(scope, {}).get(model, {})
        raw_deep_think = model_spec.get("need_deep_think")
        try:
            deep_think = int(raw_deep_think) if raw_deep_think is not None else 0
        except (TypeError, ValueError):
            deep_think = 0
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
                "need_deep_think": deep_think,
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
                "use_deep_think": str(deep_think),
                "fp": _credential_value(credentials, "fp") or DEFAULT_FP,
                "collection_id": "",
                "commerce_credit_config_enable": "0",
                "sub_conv_firstmet_type": "1" if need_create else "0",
            },
        }

    async def _upstream(self, payload: Mapping[str, Any], credentials: Mapping[str, Any]) -> bytes:
        prompt = _prompt_from_messages(payload.get("messages"))
        message = {
            "conversation_id": _text(payload.get("conversation_id")) or "0",
            "section_id": _text(payload.get("section_id")) or "0",
            "local_message_id": str(uuid.uuid4()),
            "content_type": 1,
            "content": json.dumps({"text": prompt}, ensure_ascii=False),
            "reply_id": "",
            "ext": {
                "origin": self.base_url.rstrip("/"),
                "stream": "1",
                "answer_with_suggest": "1",
                "browser_language": "zh-CN",
            },
            "local_conversation_id": "0",
            "bot_id": _credential_value(credentials, "bot_id") or self.bot_id,
            "meta_infos": [],
        }
        alice_payload = {
            "event_type": 1,
            "message": message,
        }
        encoded_payload = base64.b64encode(
            json.dumps(alice_payload, ensure_ascii=False).encode("utf-8")
        ).decode("ascii")
        client, owned = self._client()
        url = f"{self.base_url.rstrip('/')}{DOUBAO_ALICE_COMPLETION_PATH}"
        try:
            try:
                response = await client.post(
                    url,
                    params=self._query(credentials),
                    headers=self._headers(credentials),
                    json={"payload": encoded_payload},
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
    def _parse_alice(raw: bytes) -> tuple[list[str], str]:
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
            if not data or data == "[DONE]" or event_name == "done":
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
            message = value.get("message")
            if isinstance(message, str):
                try:
                    message = json.loads(message)
                except (TypeError, ValueError, json.JSONDecodeError):
                    message = None
            if not isinstance(message, Mapping):
                continue
            conversation_id = conversation_id or _text(
                message.get("conversation_id") or value.get("conversation_id")
            )
            if _text(message.get("reply_id")) == "0":
                continue
            content = message.get("content")
            if isinstance(content, str):
                try:
                    content = json.loads(content)
                except (TypeError, ValueError, json.JSONDecodeError):
                    content = {}
            if not isinstance(content, Mapping):
                continue
            content_type = message.get("content_type")
            if content_type in {1, 2001, 2003, 10000, 2008}:
                text = _text(content.get("text"))
                if text:
                    chunks.append(text)
        if not chunks:
            raise DoubaoUpstreamError(
                "豆包上游返回了空内容，可能是登录失效、风控拦截或平台协议已变化",
                code="empty_response",
            )
        text = _strip_think_markup("".join(chunks))
        if not text:
            raise DoubaoUpstreamError(
                "豆包上游只返回了推理标记，没有可显示的回答",
                code="empty_response",
            )
        return [text], conversation_id

    @staticmethod
    def _openai_response(model: str, chunks: list[str], conversation_id: str) -> dict[str, Any]:
        content = "".join(chunks)
        response: dict[str, Any] = {
            "id": f"chatcmpl-{uuid.uuid4().hex[:24]}",
            "object": "chat.completion",
            "created": int(time.time()),
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
                "model": model,
                "choices": [{"index": 0, "delta": {"role": "assistant"}, "finish_reason": None}],
            }
        ]
        frames.extend(
            {
                "id": response_id,
                "object": "chat.completion.chunk",
                "created": created,
                "model": model,
                "choices": [{"index": 0, "delta": {"content": chunk}, "finish_reason": None}],
            }
            for chunk in chunks
            if chunk
        )
        final: dict[str, Any] = {
            "id": response_id,
            "object": "chat.completion.chunk",
            "created": created,
            "model": model,
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
        client, owned = self._client()
        try:
            headers = self._headers(credentials)
            headers["Accept"] = "application/json"
            response = await client.post(
                self._url(DOUBAO_MODEL_CATALOGUE_PATH, credentials),
                headers=headers,
                json={
                    "bot_id": _credential_value(credentials, "bot_id") or self.bot_id,
                    "language_code": "zh",
                },
            )
            raw = bytes(response.content)
            if response.status_code >= 400:
                self._raise_upstream_error(response.status_code, raw)
            try:
                payload = response.json()
            except (TypeError, ValueError, json.JSONDecodeError):
                payload = _router_data_from_html(raw.decode("utf-8", "replace"))
        except httpx.TimeoutException as exc:
            raise DoubaoUpstreamError(
                "豆包模型目录请求超时，请稍后重试",
                status_code=504,
                code="upstream_timeout",
                retryable=True,
            ) from exc
        except httpx.RequestError as exc:
            raise DoubaoUpstreamError(
                "豆包模型目录连接失败，请检查网络或稍后重试",
                status_code=502,
                code="upstream_unavailable",
                retryable=True,
            ) from exc
        finally:
            if owned:
                await client.aclose()

        models = _catalogue_models(payload)
        if not models:
            client, owned = self._client()
            try:
                page_headers = self._headers(credentials)
                page_headers["Accept"] = "text/html,application/xhtml+xml"
                page = await client.get(
                    self._url(DOUBAO_CHAT_PAGE_PATH, credentials),
                    headers=page_headers,
                )
                if page.status_code >= 400:
                    self._raise_upstream_error(page.status_code, bytes(page.content))
                models = _catalogue_models(_router_data_from_html(page.text))
            finally:
                if owned:
                    await client.aclose()
        scope = _credential_scope(credentials)
        self._model_specs[scope] = {str(item["id"]): dict(item) for item in models}
        return models

    async def list_generation_options(self, context: Any = None) -> list[Mapping[str, Any]]:
        """Read the authenticated image/video selector config used by Doubao Web.

        The normal brief-list endpoint only contains chat models. Creation
        skills expose their model choices through a second per-item config API.
        Keeping this lookup in the transport makes the admin model catalogue
        match the account's actual Web entitlements.
        """

        credentials = await self._account_credentials(context)
        if not _cookie_header(credentials):
            return []
        client, owned = self._client()
        try:
            headers = self._headers(credentials)
            headers["Accept"] = "application/json"
            brief = await client.post(
                self._url(DOUBAO_MODEL_CATALOGUE_PATH, credentials),
                params=self._query(credentials),
                headers=headers,
                json={
                    "bot_id": _credential_value(credentials, "bot_id") or self.bot_id,
                    "language_code": "zh",
                },
            )
            if brief.status_code >= 400:
                self._raise_upstream_error(brief.status_code, bytes(brief.content))
            brief_payload = brief.json()
            entries = (
                brief_payload.get("data", {}).get("entry_list", [])
                if isinstance(brief_payload, Mapping)
                else []
            )
            if not isinstance(entries, list):
                return []
            entry_types = {
                str(item.get("item_id")): int(item.get("instruction_type") or 0)
                for item in entries
                if isinstance(item, Mapping) and item.get("item_id") is not None
            }
            item_ids = [item_id for item_id in entry_types if item_id]
            if not item_ids:
                return []
            config = await client.post(
                self._url(DOUBAO_ACTION_BAR_ITEM_CONFIG_PATH, credentials),
                params=self._query(credentials),
                headers=headers,
                json={
                    "bot_id": _credential_value(credentials, "bot_id") or self.bot_id,
                    "language_code": "zh",
                    "item_ids": item_ids,
                },
            )
            if config.status_code >= 400:
                self._raise_upstream_error(config.status_code, bytes(config.content))
            return _generation_option_models(config.json(), entry_types)
        except (httpx.TimeoutException, httpx.RequestError) as exc:
            raise DoubaoUpstreamError(
                "豆包生图/视频模型目录请求失败，请稍后重试",
                status_code=502,
                code="catalogue_unavailable",
                retryable=True,
            ) from exc
        finally:
            if owned:
                await client.aclose()

    async def health(self, context: Any = None) -> Mapping[str, Any]:
        credentials = await self._account_credentials(context)
        if not _cookie_header(credentials):
            return {
                "status": "no_credentials",
                "message": "豆包账号缺少 Cookie，请先导入或完成扫码登录",
            }
        return {"status": "configured", "transport": "http"}

    async def invoke(self, request: Any, account: Any = None) -> httpx.Response:
        model, payload, _headers, _stream = self._payload(request)
        credentials = await self._account_credentials(account)
        try:
            raw = await self._upstream(payload, credentials)
            chunks, conversation_id = self._parse_alice(raw)
            model = _text(payload.get("model"))
            if bool(payload.get("stream")):
                body = self._openai_stream(model, chunks, conversation_id)
                return httpx.Response(
                    200,
                    headers={"content-type": "text/event-stream"},
                    content=body,
                    request=httpx.Request("POST", f"{self.base_url}{DOUBAO_ALICE_COMPLETION_PATH}"),
                )
            return httpx.Response(
                200,
                headers={"content-type": "application/json"},
                json=self._openai_response(model, chunks, conversation_id),
                request=httpx.Request("POST", f"{self.base_url}{DOUBAO_ALICE_COMPLETION_PATH}"),
            )
        except DoubaoUpstreamError as exc:
            return httpx.Response(
                exc.status_code,
                headers={"content-type": "application/json"},
                json={"error": {"message": exc.message, "code": exc.code}},
                request=httpx.Request("POST", f"{self.base_url}{DOUBAO_ALICE_COMPLETION_PATH}"),
            )

    async def invoke_capability(
        self,
        capability: str,
        request: Any,
        account: Any = None,
    ) -> httpx.Response:
        normalized = str(capability or "").strip().lower()
        if normalized not in {"image", "video"}:
            return await super().invoke_capability(normalized, request, account)
        model, payload, _headers, _stream = self._payload(request)
        prompt = _text(payload.get("prompt"))
        if not prompt:
            return httpx.Response(
                400,
                json={"error": {"message": "prompt is required", "code": "invalid_request_error"}},
            )
        ratio = _text(payload.get("ratio"))
        selected_model = _text(model or payload.get("model"))
        duration_value = payload.get("duration")
        try:
            duration = int(duration_value) if duration_value is not None else None
        except (TypeError, ValueError):
            duration = None
        try:
            result = (
                await self.generate_image(
                    prompt,
                    ratio=ratio,
                    model=selected_model,
                    account=account,
                )
                if normalized == "image"
                else await self.generate_video(
                    prompt,
                    ratio=ratio,
                    model=selected_model,
                    duration=duration,
                    account=account,
                )
            )
            return httpx.Response(200, headers={"content-type": "application/json"}, json=result)
        except DoubaoUpstreamError as exc:
            return httpx.Response(
                exc.status_code,
                headers={"content-type": "application/json"},
                json={"error": {"message": exc.message, "code": exc.code}},
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
    "DOUBAO_CHAT_PAGE_PATH",
    "DOUBAO_ALICE_COMPLETION_PATH",
    "DOUBAO_DEFAULT_BOT_ID",
    "DOUBAO_MODEL_CATALOGUE_PATH",
    "DoubaoHttpTransport",
    "DoubaoUpstreamError",
]
