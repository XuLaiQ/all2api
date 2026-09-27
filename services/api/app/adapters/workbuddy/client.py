from __future__ import annotations

from collections.abc import Mapping
from typing import Any, Protocol

import httpx

from app.adapters.workbuddy.errors import (
    WorkBuddyProtocolError,
    WorkBuddyTimeoutError,
    WorkBuddyTransportError,
)
from app.adapters.workbuddy.mapper import normalize_realm


class AsyncHttpClient(Protocol):
    async def post(self, url: str, **kwargs: Any) -> Any: ...

    async def get(self, url: str, **kwargs: Any) -> Any: ...


class WorkBuddyClient:
    """Small platform HTTP client used by the native WorkBuddy adapter.

    The client knows only the public WorkBuddy authentication protocol.  It accepts
    an injected ``httpx.AsyncClient`` (or compatible fake) so contract tests never
    need a running upstream service.
    """

    def __init__(
        self,
        base_url: str,
        *,
        data_key: str = "",
        http_client: AsyncHttpClient | None = None,
        timeout: float = 15.0,
        connect_timeout: float = 3.0,
    ) -> None:
        self.base_url = base_url.rstrip("/")
        self.data_key = data_key
        self._http_client = http_client
        self.timeout = timeout
        self.connect_timeout = connect_timeout

    def _headers(self, realm: str, *, access_token: str = "") -> dict[str, str]:
        resolved = normalize_realm(realm)
        headers = {
            "Accept": "application/json",
            "Content-Type": "application/json",
            "User-Agent": "all2api-workbuddy/1.0",
            "Origin": "https://copilot.tencent.com"
            if resolved == "cn"
            else "https://www.workbuddy.ai",
        }
        if access_token:
            headers["Authorization"] = f"Bearer {access_token}"
        elif self.data_key:
            headers["Authorization"] = f"Bearer {self.data_key}"
        return headers

    async def _request(self, method: str, path: str, *, realm: str, **kwargs: Any) -> Any:
        client = self._http_client
        owned = False
        if client is None:
            client = httpx.AsyncClient(
                timeout=httpx.Timeout(self.timeout, connect=self.connect_timeout)
            )
            owned = True
        try:
            request = getattr(client, method)
            try:
                response = await request(f"{self.base_url}{path}", **kwargs)
            except httpx.TimeoutException as exc:
                raise WorkBuddyTimeoutError("WorkBuddy authentication request timed out") from exc
            except httpx.HTTPError as exc:
                raise WorkBuddyTransportError("WorkBuddy authentication request failed") from exc
            status = getattr(response, "status_code", 200)
            if status >= 500:
                raise WorkBuddyTransportError(f"WorkBuddy authentication returned HTTP {status}")
            if status >= 400:
                raise WorkBuddyProtocolError(f"WorkBuddy authentication returned HTTP {status}")
            try:
                return response.json()
            except (TypeError, ValueError) as exc:
                raise WorkBuddyProtocolError("WorkBuddy returned invalid JSON") from exc
        finally:
            if owned:
                await client.aclose()

    @staticmethod
    def _envelope(payload: Any) -> tuple[int, Mapping[str, Any]]:
        if not isinstance(payload, Mapping):
            raise WorkBuddyProtocolError("WorkBuddy response must be a JSON object")
        code = payload.get("code", 0)
        try:
            code = int(code or 0)
        except (TypeError, ValueError) as exc:
            raise WorkBuddyProtocolError("WorkBuddy response code is invalid") from exc
        data = payload.get("data", payload)
        if data is None:
            data = {}
        if not isinstance(data, Mapping):
            raise WorkBuddyProtocolError("WorkBuddy response data must be an object")
        return code, data

    async def start_qr(self, realm: str = "cn") -> dict[str, Any]:
        resolved = normalize_realm(realm)
        payload = await self._request(
            "post",
            "/v2/plugin/auth/state",
            realm=resolved,
            params={"platform": "CLI"},
            json={},
            headers=self._headers(resolved),
        )
        code, data = self._envelope(payload)
        if code != 0:
            raise WorkBuddyProtocolError(f"WorkBuddy rejected QR start (code={code})")
        state = str(data.get("state") or "").strip()
        auth_url = str(data.get("authUrl") or data.get("auth_url") or "").strip()
        if not state or not auth_url:
            raise WorkBuddyProtocolError("WorkBuddy QR start response is missing state or auth URL")
        return {"state": state, "auth_url": auth_url, "realm": resolved}

    async def poll_qr(self, state: str, realm: str = "cn") -> dict[str, Any]:
        if not state:
            raise WorkBuddyProtocolError("WorkBuddy QR state is required")
        resolved = normalize_realm(realm)
        token_payload = await self._request(
            "get",
            "/v2/plugin/auth/token",
            realm=resolved,
            params={"state": state},
            headers=self._headers(resolved),
        )
        code, data = self._envelope(token_payload)
        if code != 0 or not data.get("accessToken") and not data.get("access_token"):
            return {"status": "waiting", "realm": resolved}
        access_token = str(data.get("accessToken") or data.get("access_token") or "")
        account_payload = await self._request(
            "get",
            "/v2/plugin/login/account",
            realm=resolved,
            params={"state": state},
            headers=self._headers(resolved, access_token=access_token),
        )
        account_code, account = self._envelope(account_payload)
        if account_code != 0:
            return {"status": "waiting", "realm": resolved}
        uid = account.get("uid") or account.get("userId") or account.get("user_id")
        if not uid:
            return {"status": "waiting", "realm": resolved}
        # The account response is authoritative for the tenant/realm.  Keep it
        # in the native result so the provisioner can reject a callback that
        # belongs to a different realm than the one selected at start time.
        account_realm = resolved
        if account.get("realm"):
            account_realm = normalize_realm(str(account.get("realm")))
        return {
            "status": "ready",
            "realm": account_realm,
            "uid": str(uid),
            "nickname": str(account.get("nickname") or account.get("nick") or ""),
            "enterprise_id": str(account.get("enterpriseId") or account.get("enterprise_id") or ""),
            "domain": str(data.get("domain") or account.get("domain") or ""),
            "access_token": access_token,
            "refresh_token": str(data.get("refreshToken") or data.get("refresh_token") or ""),
            "device_token": str(data.get("deviceToken") or data.get("device_token") or ""),
            "expires_at": int(data.get("expiresAt") or data.get("expires_at") or 0),
        }

    async def list_models(self) -> list[dict[str, Any]]:
        payload = await self._request(
            "get",
            "/v1/models",
            realm="cn",
            headers=self._headers("cn"),
        )
        if isinstance(payload, list):
            items = payload
        elif isinstance(payload, Mapping) and isinstance(payload.get("data"), list):
            items = payload["data"]
        else:
            return []
        return [
            dict(item)
            for item in items
            if isinstance(item, Mapping) and isinstance(item.get("id"), str)
        ]

    async def health(self) -> dict[str, Any]:
        try:
            await self.list_models()
        except WorkBuddyTransportError:
            return {"status": "platform_unavailable"}
        except WorkBuddyProtocolError:
            return {"status": "protocol_error"}
        return {"status": "ok"}
