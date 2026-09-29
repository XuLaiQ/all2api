from __future__ import annotations

import hashlib
from collections.abc import Mapping
from typing import Any, Protocol

import httpx

from app.adapters.workbuddy.errors import (
    WorkBuddyProtocolError,
    WorkBuddyTimeoutError,
    WorkBuddyTransportError,
)
from app.adapters.workbuddy.mapper import (
    map_model_payload,
    merge_model_catalog,
    normalize_realm,
    resolve_realm,
)


class AsyncHttpClient(Protocol):
    async def post(self, url: str, **kwargs: Any) -> Any: ...

    async def get(self, url: str, **kwargs: Any) -> Any: ...


class WorkBuddyClient:
    """HTTP client for the public WorkBuddy plugin protocol.

    The client owns provider-specific paths and headers.  Credential material is
    accepted only from the caller's local CredentialStore boundary; this class
    never reads source-project files or management APIs.
    """

    CHAT_PATH = "/v2/chat/completions"
    CN_MODELS_PATH = "/console/enterprises/personal/models"
    GLOBAL_MODELS_PATH = "/v2/enterprises/personal/models"
    V3_CONFIG_PATH = "/v3/config"

    def __init__(
        self,
        base_url: str = "https://copilot.tencent.com",
        *,
        global_base_url: str = "https://www.workbuddy.ai",
        data_key: str = "",
        http_client: AsyncHttpClient | None = None,
        timeout: float = 30.0,
        connect_timeout: float = 5.0,
        user_agent: str = "CLI/2.63.2 CodeBuddy/2.63.2",
    ) -> None:
        self.base_url = str(base_url or "https://copilot.tencent.com").rstrip("/")
        self.global_base_url = str(global_base_url or "https://www.workbuddy.ai").rstrip("/")
        self.data_key = str(data_key or "")
        self._http_client = http_client
        self.timeout = float(timeout)
        self.connect_timeout = float(connect_timeout)
        self.user_agent = str(user_agent or "CLI/2.63.2 CodeBuddy/2.63.2")

    def _base_url(self, realm: str) -> str:
        return self.global_base_url if normalize_realm(realm) == "global" else self.base_url

    def base_url_for_credentials(self, credentials: Mapping[str, Any] | None = None) -> str:
        values = credentials if isinstance(credentials, Mapping) else {}
        return self._base_url(resolve_realm(values.get("realm"), values.get("domain")))

    def runtime_headers(self, credentials: Mapping[str, Any]) -> Mapping[str, str]:
        values = credentials if isinstance(credentials, Mapping) else {}
        realm = resolve_realm(values.get("realm"), values.get("domain"))
        return self._headers(realm, credentials=values)

    @staticmethod
    def _stable_id(prefix: str, uid: str) -> str:
        digest = hashlib.sha256(f"{prefix}:{uid}".encode()).hexdigest()
        return digest[:32]

    def _headers(
        self,
        realm: str,
        *,
        access_token: str = "",
        credentials: Mapping[str, Any] | None = None,
        accept: str = "application/json",
    ) -> dict[str, str]:
        resolved = normalize_realm(realm)
        values = credentials if isinstance(credentials, Mapping) else {}
        domain = str(values.get("domain") or "").strip()
        uid = str(values.get("uid") or values.get("user_id") or values.get("userId") or "").strip()
        enterprise_id = str(
            values.get("enterprise_id")
            or values.get("enterpriseId")
            or values.get("tenant_id")
            or values.get("tenantId")
            or ""
        ).strip()
        origin = "https://www.workbuddy.ai" if resolved == "global" else "https://copilot.tencent.com"
        headers = {
            "Accept": accept,
            "Content-Type": "application/json",
            "User-Agent": self.user_agent,
            "Origin": origin,
            "Referer": f"{origin}/",
            "X-Requested-With": "XMLHttpRequest",
            "X-CodeBuddy-Request": "1",
            "Accept-Language": "en-US" if resolved == "global" else "zh-CN",
        }
        token = str(
            access_token or values.get("access_token") or values.get("accessToken") or ""
        ).strip()
        if token:
            headers["Authorization"] = f"Bearer {token}"
        elif self.data_key:
            headers["Authorization"] = f"Bearer {self.data_key}"
        if uid:
            headers["X-User-Id"] = uid
            headers["X-Machine-ID"] = self._stable_id("machine", uid)
            headers["X-Session-ID"] = self._stable_id("session", uid)
        if domain:
            headers["X-Domain"] = domain
        elif resolved == "global":
            headers["X-Domain"] = "www.workbuddy.ai"
        if enterprise_id:
            headers["X-Enterprise-Id"] = enterprise_id
            headers["X-Tenant-Id"] = enterprise_id
        elif resolved == "global":
            headers["X-No-Enterprise-Id"] = "1"
        device_token = str(
            values.get("device_token") or values.get("deviceToken") or ""
        ).strip()
        if device_token:
            headers["X-Device-Token"] = device_token
        return headers

    async def _request(
        self,
        method: str,
        path: str,
        *,
        realm: str,
        credentials: Mapping[str, Any] | None = None,
        access_token: str = "",
        **kwargs: Any,
    ) -> Any:
        resolved = normalize_realm(realm)
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
                response = await request(
                    f"{self._base_url(resolved)}{path}",
                    headers=self._headers(
                        resolved,
                        access_token=access_token,
                        credentials=credentials,
                    ) | dict(kwargs.pop("headers", {}) or {}),
                    **kwargs,
                )
            except httpx.TimeoutException as exc:
                raise WorkBuddyTimeoutError("WorkBuddy request timed out") from exc
            except httpx.HTTPError as exc:
                raise WorkBuddyTransportError("WorkBuddy request failed") from exc
            status = int(getattr(response, "status_code", 200))
            if status == 401:
                raise WorkBuddyProtocolError("WorkBuddy credential was rejected")
            if status >= 500:
                raise WorkBuddyTransportError(f"WorkBuddy returned HTTP {status}")
            if status >= 400:
                raise WorkBuddyProtocolError(f"WorkBuddy returned HTTP {status}")
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
        raw_code = payload.get("code", 0)
        try:
            code = int(raw_code or 0)
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
        )
        code, data = self._envelope(payload)
        if code != 0:
            raise WorkBuddyProtocolError("WorkBuddy rejected QR start")
        state = str(data.get("state") or "").strip()
        auth_url = str(data.get("authUrl") or data.get("auth_url") or "").strip()
        if not state or not auth_url:
            raise WorkBuddyProtocolError("WorkBuddy QR start response is missing state or auth URL")
        return {"state": state, "auth_url": auth_url, "realm": resolved}

    async def poll_qr(self, state: str, realm: str = "cn") -> dict[str, Any]:
        if not str(state or "").strip():
            raise WorkBuddyProtocolError("WorkBuddy QR state is required")
        resolved = normalize_realm(realm)
        token_payload = await self._request(
            "get",
            "/v2/plugin/auth/token",
            realm=resolved,
            params={"state": state},
        )
        code, data = self._envelope(token_payload)
        access_token = str(data.get("accessToken") or data.get("access_token") or "").strip()
        if code != 0 or not access_token:
            return {"status": "waiting", "realm": resolved}
        account_payload = await self._request(
            "get",
            "/v2/plugin/login/account",
            realm=resolved,
            access_token=access_token,
            params={"state": state},
        )
        account_code, account = self._envelope(account_payload)
        if account_code != 0:
            return {"status": "waiting", "realm": resolved}
        uid = account.get("uid") or account.get("userId") or account.get("user_id")
        if not uid:
            return {"status": "waiting", "realm": resolved}
        # The selected login realm is authoritative.  The account endpoint
        # often omits ``realm`` and ``domain`` after a successful global login;
        # resolving an empty response would incorrectly fall back to ``cn``
        # and make a valid global session fail the provisioner mismatch check.
        account_realm = resolved
        if account.get("realm") or account.get("domain"):
            account_realm = resolve_realm(account.get("realm"), account.get("domain"))
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

    async def refresh_token(
        self,
        credentials: Mapping[str, Any],
        *,
        account_id: str = "",
    ) -> dict[str, Any]:
        if not isinstance(credentials, Mapping):
            raise WorkBuddyProtocolError("WorkBuddy credentials are invalid")
        refresh = str(
            credentials.get("refresh_token") or credentials.get("refreshToken") or ""
        ).strip()
        if not refresh:
            raise WorkBuddyProtocolError("WorkBuddy credentials have no refresh token")
        realm = resolve_realm(credentials.get("realm"), credentials.get("domain"))
        headers = {"X-Refresh-Token": refresh, "X-Auth-Refresh-Source": "plugin"}
        payload = await self._request(
            "post",
            "/v2/plugin/auth/token/refresh",
            realm=realm,
            credentials=credentials,
            access_token=str(
                credentials.get("access_token") or credentials.get("accessToken") or ""
            ),
            headers=headers,
        )
        code, data = self._envelope(payload)
        if code != 0:
            raise WorkBuddyProtocolError("WorkBuddy rejected token refresh")
        access = str(data.get("accessToken") or data.get("access_token") or "").strip()
        if not access:
            raise WorkBuddyProtocolError("WorkBuddy refresh response has no access token")
        return {
            "access_token": access,
            "refresh_token": str(data.get("refreshToken") or data.get("refresh_token") or refresh),
            "device_token": str(
                data.get("deviceToken")
                or data.get("device_token")
                or credentials.get("device_token")
                or ""
            ),
            "expires_at": int(data.get("expiresAt") or data.get("expires_at") or 0),
            "realm": realm,
            "domain": str(data.get("domain") or credentials.get("domain") or ""),
        }

    async def list_models(
        self,
        realm: str = "cn",
        credentials: Mapping[str, Any] | None = None,
    ) -> list[dict[str, Any]]:
        resolved = resolve_realm(
            realm,
            credentials.get("domain") if isinstance(credentials, Mapping) else "",
        )
        failures: list[Exception] = []
        primary: list[dict[str, Any]] = []
        secondary: list[dict[str, Any]] = []
        try:
            v3 = await self._request(
                "get",
                self.V3_CONFIG_PATH,
                realm=resolved,
                credentials=credentials,
            )
            primary = map_model_payload(v3, realm=resolved)
        except Exception as exc:
            failures.append(exc)
        try:
            path = self.GLOBAL_MODELS_PATH if resolved == "global" else self.CN_MODELS_PATH
            enterprise = await self._request(
                "get",
                path,
                realm=resolved,
                credentials=credentials,
            )
            secondary = map_model_payload(enterprise, realm=resolved, enterprise=True)
        except Exception as exc:
            failures.append(exc)
        merged = merge_model_catalog(primary, secondary)
        if merged:
            return merged
        if failures and all(isinstance(item, WorkBuddyTimeoutError) for item in failures):
            raise WorkBuddyTimeoutError("WorkBuddy model request timed out") from failures[-1]
        if failures:
            raise WorkBuddyProtocolError(
                "WorkBuddy model catalogue is unavailable"
            ) from failures[-1]
        return []

    async def health(self) -> dict[str, Any]:
        try:
            await self.list_models()
        except WorkBuddyTimeoutError:
            return {"status": "timeout"}
        except WorkBuddyTransportError:
            return {"status": "platform_unavailable"}
        except WorkBuddyProtocolError:
            return {"status": "protocol_error"}
        return {"status": "ok"}


__all__ = ["AsyncHttpClient", "WorkBuddyClient"]
