"""Native Codex Responses client for ChatGPT OAuth accounts.

OAuth credentials issued to the Codex CLI client (the ``access_token`` /
``refresh_token`` / ``id_token`` triple found in sub2api exports) authenticate
against ``/backend-api/codex/responses`` with a plain bearer token.  That
endpoint does not run the browser sentinel (Turnstile / proof-of-work)
challenges required by the Web conversation endpoint, so imported OAuth
accounts can chat without a browser verification round-trip.
"""

from __future__ import annotations

import base64
import json
from collections.abc import Iterator, Mapping
from typing import Any

import httpx

from app.infrastructure.http import build_client

from .client import ChatGPTWebClient
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

CODEX_DEFAULT_MODEL = "gpt-5.5"
CODEX_RESPONSES_PATH = "/backend-api/codex/responses"
CODEX_MODELS_PATH = "/backend-api/codex/models?client_version=0.50.0"


def _jwt_payload(token: str) -> dict[str, Any]:
    """Decode the unsigned payload of a JWT-shaped token without verification."""

    parts = str(token or "").split(".")
    if len(parts) != 3:
        return {}
    padded = parts[1] + "=" * (-len(parts[1]) % 4)
    try:
        value = json.loads(base64.urlsafe_b64decode(padded.encode("ascii")))
    except (ValueError, TypeError):
        return {}
    return value if isinstance(value, dict) else {}


def credential_client_id(credentials: Mapping[str, Any]) -> str:
    """Best-effort OAuth client id for refresh calls.

    The refresh token must be exchanged with the client it was issued to, so
    the stored ``client_id`` wins; otherwise the claim embedded in the token
    JWT is used as a fallback.
    """

    stored = str(credentials.get("client_id") or "").strip()
    if stored:
        return stored
    for key in ("access_token", "accessToken", "id_token", "idToken"):
        claim = str(_jwt_payload(str(credentials.get(key) or "")).get("client_id") or "").strip()
        if claim:
            return claim
    return ""


def is_codex_credentials(credentials: Mapping[str, Any]) -> bool:
    """Decide whether credentials belong to the Codex/OAuth backend flow.

    An explicit ``auth_mode`` marker wins.  Import mappers store the OAuth
    markers (``client_id`` / ``organization_id`` / ``id_token``) alongside the
    tokens, and plain Web session imports never carry them, so marker presence
    is the practical discriminator.
    """

    mode = str(credentials.get("auth_mode") or "").strip().lower()
    if mode in {"codex", "oauth"}:
        return True
    if mode in {"web", "session"}:
        return False
    return any(
        credentials.get(key)
        for key in ("client_id", "organization_id", "id_token", "idToken")
    )


class ChatGPTCodexClient:
    """Bearer-token client for the ChatGPT Codex Responses backend."""

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

    @property
    def access_token(self) -> str:
        return str(
            self.credentials.get("access_token") or self.credentials.get("accessToken") or ""
        ).strip()

    def _client(self) -> httpx.AsyncClient:
        if self._http_client is not None:
            return self._http_client
        proxy = str(self.credentials.get("proxy") or "").strip()
        return build_client(
            proxy=proxy,
            timeout=self.timeout,
            connect_timeout=self.connect_timeout,
        )

    @staticmethod
    async def _close_resource(resource: Any) -> None:
        closer = getattr(resource, "aclose", None) or getattr(resource, "close", None)
        if not callable(closer):
            return
        result = closer()
        if hasattr(result, "__await__"):
            await result

    def _headers(self, *, accept: str = "application/json") -> dict[str, str]:
        headers = {
            "Accept": accept,
            "Authorization": f"Bearer {self.access_token}",
        }
        account_id = str(
            self.credentials.get("chatgpt_account_id")
            or self.credentials.get("chatgpt-account-id")
            or ""
        ).strip()
        if account_id:
            headers["chatgpt-account-id"] = account_id
        return headers

    @staticmethod
    def _raise_for_status(response: httpx.Response) -> None:
        if response.status_code < 400:
            return
        detail = ""
        try:
            payload = response.json()
        except (TypeError, ValueError):
            payload = None
        if isinstance(payload, Mapping):
            error = payload.get("error")
            if isinstance(error, Mapping):
                detail = str(error.get("message") or "")
            elif isinstance(error, str):
                detail = error
            if not detail:
                detail = str(payload.get("message") or payload.get("detail") or "")
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
        raise ChatGPTProtocolError(
            detail or f"ChatGPT Codex request failed (HTTP {response.status_code})"
        )

    async def _request(
        self,
        method: str,
        path: str,
        *,
        json_body: Mapping[str, Any] | None = None,
        timeout: float | None = None,
    ) -> httpx.Response:
        client = self._client()
        owned = self._http_client is None
        try:
            try:
                response = await client.request(
                    method,
                    f"{self.base_url}{path}",
                    headers=self._headers(),
                    json=dict(json_body) if json_body is not None else None,
                    timeout=timeout or self.timeout,
                )
            except httpx.TimeoutException as exc:
                raise ChatGPTTimeoutError() from exc
            except httpx.RequestError as exc:
                raise ChatGPTUpstreamUnavailableError() from exc
            self._raise_for_status(response)
            return response
        finally:
            if owned:
                await self._close_resource(client)

    async def account_info(self) -> Mapping[str, Any]:
        response = await self._request("GET", "/backend-api/me", timeout=30)
        try:
            value = response.json()
        except (TypeError, ValueError) as exc:
            raise ChatGPTProtocolError() from exc
        if not isinstance(value, Mapping):
            raise ChatGPTProtocolError()
        return value

    async def catalogue_status(self) -> dict[str, Any]:
        """Read the Codex catalogue while preserving empty vs failed states."""

        try:
            response = await self._request(
                "GET",
                CODEX_MODELS_PATH,
                timeout=30,
            )
            payload = response.json()
        except ChatGPTAuthError as exc:
            return {"status": "auth_failed", "models": [], "message": str(exc)}
        except ChatGPTError as exc:
            return {"status": "failed", "models": [], "message": str(exc)}
        except (TypeError, ValueError, json.JSONDecodeError) as exc:
            return {"status": "failed", "models": [], "message": str(exc)}

        values = payload.get("models") if isinstance(payload, Mapping) else None
        if not isinstance(values, list):
            values = payload.get("data") if isinstance(payload, Mapping) else None
        result: list[dict[str, Any]] = []
        seen: set[str] = set()
        for value in values if isinstance(values, list) else []:
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
        return {
            "status": "ok" if result else "empty",
            "models": result,
            "message": "" if result else "该 ChatGPT 账号没有可用的 Codex 模型权益",
        }

    async def list_models(self) -> list[dict[str, Any]]:
        # An empty catalogue is meaningful: it means this account has no
        # available Codex entitlement. Never invent a model id that the
        # provider has not advertised.
        status = await self.catalogue_status()
        return list(status.get("models") or [])

    @staticmethod
    def _input_items(messages: Any) -> tuple[str, list[dict[str, Any]]]:
        if not isinstance(messages, list) or not messages:
            raise ChatGPTInvalidRequestError("messages must be a non-empty array")
        instructions: list[str] = []
        items: list[dict[str, Any]] = []
        for message in messages:
            if not isinstance(message, Mapping):
                raise ChatGPTInvalidRequestError("message must be an object")
            role = str(message.get("role") or "user").strip() or "user"
            content = message.get("content", "")
            if not isinstance(content, str):
                raise ChatGPTInvalidRequestError("ChatGPT Codex currently accepts text only")
            if role in {"system", "developer"}:
                instructions.append(content)
                continue
            content_type = "output_text" if role == "assistant" else "input_text"
            items.append({"role": role, "content": [{"type": content_type, "text": content}]})
        if not items:
            raise ChatGPTInvalidRequestError("messages must include a user turn")
        return "\n\n".join(instructions), items

    @classmethod
    def _responses_body(cls, payload: Mapping[str, Any]) -> dict[str, Any]:
        instructions, items = cls._input_items(payload.get("messages"))
        model = str(payload.get("model") or "").strip()
        if not model or model == "auto":
            model = CODEX_DEFAULT_MODEL
        body: dict[str, Any] = {
            "model": model,
            "store": False,
            "stream": True,
            "input": items,
        }
        if instructions:
            body["instructions"] = instructions
        return body

    @staticmethod
    def _sse_events(raw: bytes) -> Iterator[Mapping[str, Any]]:
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
            if isinstance(event, Mapping):
                yield event

    @classmethod
    def _collect_response(cls, raw: bytes) -> tuple[str, str, Mapping[str, Any] | None]:
        text_parts: list[str] = []
        response_id = ""
        usage: Mapping[str, Any] | None = None
        for event in cls._sse_events(raw):
            event_type = str(event.get("type") or "")
            if event_type == "response.output_text.delta":
                delta = event.get("delta")
                if isinstance(delta, str):
                    text_parts.append(delta)
            elif event_type == "response.created":
                response = event.get("response")
                if isinstance(response, Mapping):
                    response_id = response_id or str(response.get("id") or "")
            elif event_type in {"response.completed", "response.incomplete"}:
                response = event.get("response")
                if isinstance(response, Mapping):
                    response_id = response_id or str(response.get("id") or "")
                    value = response.get("usage")
                    if isinstance(value, Mapping):
                        usage = value
            elif event_type in {"response.failed", "response.error", "error"}:
                error: Any = event.get("error")
                if error is None and isinstance(event.get("response"), Mapping):
                    error = event["response"].get("error")
                if isinstance(error, Mapping):
                    message = str(error.get("message") or "")
                elif isinstance(error, str):
                    message = error
                else:
                    message = str(event.get("message") or "")
                raise ChatGPTProtocolError(
                    message or "ChatGPT Codex response stream reported a failure"
                )
        return "".join(text_parts), response_id, usage

    async def chat(self, payload: Mapping[str, Any]) -> httpx.Response:
        if not self.access_token:
            raise ChatGPTAuthError()
        body = self._responses_body(payload)
        response = await self._request(
            "POST", CODEX_RESPONSES_PATH, json_body=body, timeout=self.timeout
        )
        content, response_id, usage = self._collect_response(response.content)
        model = str(payload.get("model") or body["model"])
        stream = bool(payload.get("stream"))
        converted = ChatGPTWebClient._openai_response(
            model=model,
            content=content,
            stream=stream,
            conversation_id=response_id,
            usage=usage,
        )
        if stream:
            return httpx.Response(
                200,
                headers={"content-type": "text/event-stream"},
                content=converted if isinstance(converted, bytes) else b"",
            )
        return httpx.Response(
            200,
            headers={"content-type": "application/json"},
            json=converted if isinstance(converted, dict) else {},
        )

    async def invoke(self, request: Mapping[str, Any]) -> httpx.Response:
        return await self.chat(request)


__all__ = [
    "CODEX_DEFAULT_MODEL",
    "CODEX_MODELS_PATH",
    "CODEX_RESPONSES_PATH",
    "ChatGPTCodexClient",
    "ChatGPTError",
    "credential_client_id",
    "is_codex_credentials",
]
