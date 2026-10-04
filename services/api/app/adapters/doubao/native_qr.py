"""Direct HTTP QR login for Doubao.

The worker mirrors the public web login protocol used by Doubao's client:
obtain a passport CSRF token, request a QR image, poll the QR state and follow
the provider redirect after confirmation. It never starts a browser and the
cookie material is returned only to the provisioner's credential-store port.
"""

from __future__ import annotations

import base64
import binascii
import time
import uuid
from collections.abc import Mapping
from dataclasses import dataclass
from typing import Any
from urllib.parse import urljoin

import httpx

from app.infrastructure.http import build_client

from .browser import BrowserChallenge, BrowserEvent
from .errors import BrowserWorkerError, ProvisionSessionExpiredError

DOUBAO_BASE_URL = "https://www.doubao.com"
DOUBAO_AID = 497858
DOUBAO_USER_AGENT = (
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36"
)


@dataclass
class _NativeQrSession:
    session_id: str
    account_id: str
    created_at: float
    expires_at: float
    csrf_token: str
    qr_token: str
    cookies: dict[str, str]
    device_params: dict[str, str]
    qr_code: str | None = None
    qr_image_base64: str | None = None
    status: str = "waiting_scan"
    credentials: dict[str, Any] | None = None


class NativeDoubaoQrWorker:
    """BrowserWorker-compatible QR flow implemented entirely over HTTP."""

    def __init__(
        self,
        base_url: str = DOUBAO_BASE_URL,
        *,
        http_client: httpx.AsyncClient | None = None,
        timeout: float = 30.0,
        session_ttl_seconds: float = 600.0,
        clock: Any = time.time,
    ) -> None:
        self.base_url = str(base_url or DOUBAO_BASE_URL).rstrip("/")
        self._http_client = http_client
        self.timeout = max(1.0, float(timeout))
        self.session_ttl_seconds = max(30.0, float(session_ttl_seconds))
        self._clock = clock
        self._sessions: dict[str, _NativeQrSession] = {}

    @classmethod
    def from_settings(cls, settings: Any) -> NativeDoubaoQrWorker:
        base_url = str(
            getattr(settings, "doubao_platform_base", DOUBAO_BASE_URL)
            or DOUBAO_BASE_URL
        ).rstrip("/")
        return cls(
            base_url,
            timeout=float(getattr(settings, "doubao_browser_operation_timeout_seconds", 30.0)),
            session_ttl_seconds=float(
                getattr(settings, "provision_session_ttl_seconds", 600.0)
            ),
        )

    async def start(self) -> None:
        return None

    async def stop(self) -> None:
        self._sessions.clear()

    async def restart(self) -> None:
        await self.stop()

    def is_alive(self) -> bool:
        return True

    def has_qr_session(self, session_id: str) -> bool:
        return session_id in self._sessions

    async def health(self) -> Mapping[str, Any]:
        return {
            "status": "ready",
            "worker": "native-http-qr",
            "browser_connected": False,
            "active_sessions": len(self._sessions),
        }

    def _headers(self, csrf_token: str = "") -> dict[str, str]:
        origin = self.base_url
        headers = {
            "User-Agent": DOUBAO_USER_AGENT,
            "Accept": "application/json, text/plain, */*",
            "Referer": f"{origin}/chat/login",
            "Origin": origin,
        }
        if csrf_token:
            headers["x-tt-passport-csrf-token"] = csrf_token
        return headers

    @staticmethod
    def _cookie_header(cookies: Mapping[str, str]) -> str:
        return "; ".join(f"{key}={value}" for key, value in cookies.items())

    @staticmethod
    def _merge_set_cookies(target: dict[str, str], response: httpx.Response) -> None:
        for item in [*response.history, response]:
            for raw in item.headers.get_list("set-cookie"):
                part = raw.split(";", 1)[0].strip()
                if "=" not in part:
                    continue
                name, value = part.split("=", 1)
                target[name.strip()] = value.strip()

    async def _request(
        self,
        method: str,
        path_or_url: str,
        *,
        cookies: dict[str, str],
        csrf_token: str = "",
        params: Mapping[str, str] | None = None,
        follow_redirects: bool = False,
    ) -> httpx.Response:
        url = path_or_url if path_or_url.startswith("http") else f"{self.base_url}{path_or_url}"
        client = self._http_client
        owned = client is None
        if owned:
            client = build_client(
                timeout=self.timeout,
                connect_timeout=min(10.0, self.timeout),
            )
        try:
            current_url = url
            current_params = params
            for _ in range(10):
                headers = self._headers(csrf_token)
                cookie_header = self._cookie_header(cookies)
                if cookie_header:
                    headers["Cookie"] = cookie_header
                response = await getattr(client, method.lower())(
                    current_url,
                    headers=headers,
                    params=current_params,
                    follow_redirects=False,
                )
                self._merge_set_cookies(cookies, response)
                if not follow_redirects or response.status_code not in {301, 302, 303, 307, 308}:
                    if response.status_code >= 400:
                        raise BrowserWorkerError("Doubao 登录接口暂时不可用")
                    return response
                location = response.headers.get("location")
                if not location:
                    raise BrowserWorkerError("Doubao 登录重定向缺少目标地址")
                current_url = urljoin(current_url, location)
                current_params = None
            raise BrowserWorkerError("Doubao 登录重定向次数过多")
        except httpx.TimeoutException as exc:
            raise BrowserWorkerError("Doubao 登录接口请求超时") from exc
        except httpx.HTTPError as exc:
            raise BrowserWorkerError("Doubao 登录接口请求失败") from exc
        finally:
            if owned:
                await client.aclose()

    @staticmethod
    def _json(response: httpx.Response) -> Mapping[str, Any]:
        try:
            value = response.json()
        except (TypeError, ValueError) as exc:
            raise BrowserWorkerError("Doubao 登录接口返回了无效数据") from exc
        if not isinstance(value, Mapping):
            raise BrowserWorkerError("Doubao 登录接口返回格式不正确")
        return value

    async def _csrf_token(self, cookies: dict[str, str]) -> str:
        for name in ("passport_csrf_token", "passport_csrf_token_default"):
            if cookies.get(name):
                return cookies[name]
        response = await self._request(
            "GET",
            "/passport/safe/csrf_token/",
            cookies=cookies,
            params={"aid": str(DOUBAO_AID)},
        )
        payload = self._json(response)
        data = payload.get("data")
        if isinstance(data, Mapping) and data.get("passport_csrf_token"):
            return str(data["passport_csrf_token"])
        return str(cookies.get("passport_csrf_token") or "")

    async def _qr_image(
        self,
        raw: Any,
        *,
        cookies: dict[str, str],
        csrf_token: str,
    ) -> tuple[str | None, str | None]:
        value = str(raw or "").strip()
        if not value:
            return None, None
        if value.startswith("http"):
            response = await self._request(
                "GET",
                value,
                cookies=cookies,
                csrf_token=csrf_token,
            )
            return None, base64.b64encode(response.content).decode("ascii")
        if value.startswith("data:") and "," in value:
            value = value.split(",", 1)[1]
        try:
            base64.b64decode(value, validate=False)
        except (binascii.Error, ValueError):
            return value, None
        return None, value

    async def _start_session(
        self,
        account_id: str,
        *,
        session_id: str | None = None,
        created_at: float | None = None,
    ) -> BrowserChallenge:
        cookies: dict[str, str] = {}
        await self._request("GET", "/", cookies=cookies)
        csrf_token = await self._csrf_token(cookies)
        response = await self._request(
            "GET",
            "/passport/web/get_qrcode/",
            cookies=cookies,
            csrf_token=csrf_token,
            params={"next": self.base_url, "aid": str(DOUBAO_AID)},
        )
        payload = self._json(response)
        data = payload.get("data")
        if not isinstance(data, Mapping):
            raise BrowserWorkerError("Doubao 二维码响应格式不正确")
        try:
            error_code = int(data.get("error_code", payload.get("error_code", -1)))
        except (TypeError, ValueError):
            error_code = -1
        if error_code != 0:
            raise BrowserWorkerError("Doubao 二维码生成失败")
        qr_token = str(data.get("token") or "").strip()
        if not qr_token:
            raise BrowserWorkerError("Doubao 二维码响应缺少 token")
        qr_code, qr_image = await self._qr_image(
            data.get("qrcode") or data.get("qrcode_url"),
            cookies=cookies,
            csrf_token=csrf_token,
        )
        now = float(self._clock() if created_at is None else created_at)
        actual_session_id = session_id or f"doubao-http-{uuid.uuid4().hex}"
        self._sessions[actual_session_id] = _NativeQrSession(
            session_id=actual_session_id,
            account_id=account_id,
            created_at=now,
            expires_at=now + self.session_ttl_seconds,
            csrf_token=csrf_token,
            qr_token=qr_token,
            cookies=cookies,
            device_params={
                "device_id": str(uuid.uuid4().int % 10**16),
                "web_id": str(uuid.uuid4().int % 10**19),
                "fp": f"verify_{uuid.uuid4().hex}",
            },
            qr_code=qr_code,
            qr_image_base64=qr_image,
        )
        return BrowserChallenge(
            session_id=actual_session_id,
            account_id=account_id,
            qr_code=qr_code or "",
            qr_image_base64=qr_image,
            status="waiting_scan",
            expires_at=now + self.session_ttl_seconds,
        )

    async def start_qr_login(self, account_id: str, profile_path: str) -> BrowserChallenge:
        del profile_path
        return await self._start_session(account_id)

    async def restore_qr_login(
        self,
        session_id: str,
        account_id: str,
        profile_path: str,
        created_at: float,
    ) -> BrowserChallenge | None:
        del profile_path
        return await self._start_session(
            account_id,
            session_id=session_id,
            created_at=created_at,
        )

    def _session(self, session_id: str) -> _NativeQrSession:
        session = self._sessions.get(session_id)
        if session is None:
            raise BrowserWorkerError("Doubao 原生二维码会话不存在，请重新生成二维码")
        if float(self._clock()) >= session.expires_at:
            session.status = "expired"
            raise ProvisionSessionExpiredError()
        return session

    @staticmethod
    def _event(
        session: _NativeQrSession,
        status: str,
        *,
        message: str = "",
        credentials: Mapping[str, Any] | None = None,
    ) -> BrowserEvent:
        return BrowserEvent(
            session_id=session.session_id,
            account_id=session.account_id,
            status=status,
            qr_code=session.qr_code,
            qr_image_base64=session.qr_image_base64,
            message=message,
            credentials=credentials,
        )

    async def poll_qr_login(self, session_id: str) -> BrowserEvent:
        session = self._session(session_id)
        response = await self._request(
            "GET",
            "/passport/web/check_qrconnect/",
            cookies=session.cookies,
            csrf_token=session.csrf_token,
            params={
                "next": self.base_url,
                "token": session.qr_token,
                "aid": str(DOUBAO_AID),
            },
        )
        payload = self._json(response)
        data = payload.get("data")
        if not isinstance(data, Mapping):
            return self._event(session, "waiting_scan", message="等待扫码")
        try:
            error_code = int(data.get("error_code", payload.get("error_code", -1)))
        except (TypeError, ValueError):
            error_code = -1
        if error_code != 0:
            description = str(data.get("description") or "")
            if "expired" in description.lower() or "过期" in description:
                session.status = "expired"
                return self._event(session, "expired", message="二维码已过期")
            return self._event(session, session.status, message="等待扫码")
        status = str(data.get("status") or "new").lower()
        if status == "new":
            session.status = "waiting_scan"
            return self._event(session, "waiting_scan", message="等待扫码")
        if status == "scanned":
            session.status = "scanned"
            return self._event(session, "scanned", message="已扫码，请在手机上确认")
        if status == "expired":
            session.status = "expired"
            return self._event(session, "expired", message="二维码已过期")
        if status != "confirmed":
            return self._event(session, session.status, message="等待扫码")

        redirect_url = str(data.get("redirect_url") or "").strip()
        if redirect_url:
            await self._request(
                "GET",
                urljoin(f"{self.base_url}/", redirect_url),
                cookies=session.cookies,
                csrf_token=session.csrf_token,
                follow_redirects=True,
            )
        cookie_header = self._cookie_header(session.cookies)
        session.credentials = {
            "Cookie": cookie_header,
            "cookies": dict(session.cookies),
            "msToken": session.cookies.get("msToken", ""),
            "User-Agent": DOUBAO_USER_AGENT,
            "Origin": self.base_url,
            "Referer": f"{self.base_url}/chat/login",
            **session.device_params,
        }
        session.status = "succeeded"
        return self._event(
            session,
            "succeeded",
            message="登录成功",
            credentials=session.credentials,
        )

    async def complete_qr_login(self, session_id: str) -> BrowserEvent:
        session = self._session(session_id)
        if session.status == "succeeded" and session.credentials:
            return self._event(
                session,
                "succeeded",
                message="登录成功",
                credentials=session.credentials,
            )
        return await self.poll_qr_login(session_id)

    async def cancel_qr_login(self, session_id: str) -> None:
        self._sessions.pop(session_id, None)

    async def delete_account(self, account_id: str) -> None:
        for session_id, session in list(self._sessions.items()):
            if session.account_id == account_id:
                self._sessions.pop(session_id, None)


__all__ = ["NativeDoubaoQrWorker"]
