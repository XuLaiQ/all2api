"""Admin account provisioning calls for the registered upstream adapters.

These helpers deliberately return only the fields needed by the management UI.
Upstream account payloads can contain credentials, so callers must not forward
their raw responses to the browser.
"""

from __future__ import annotations

from typing import Any

import httpx

_TIMEOUT = httpx.Timeout(30, connect=5)


def _headers(secret: str) -> dict[str, str]:
    headers = {"Accept": "application/json"}
    if secret:
        headers["Authorization"] = f"Bearer {secret}"
    return headers


def _url(base_url: str, path: str) -> str:
    return f"{base_url.rstrip('/')}{path}"


def _json(response: httpx.Response) -> dict[str, Any]:
    response.raise_for_status()
    payload = response.json()
    return payload if isinstance(payload, dict) else {"data": payload}


async def workbuddy_start(base_url: str, admin_token: str, realm: str = "cn") -> dict[str, Any]:
    async with httpx.AsyncClient(timeout=_TIMEOUT) as client:
        response = await client.post(
            _url(base_url, "/api/auth/start"),
            headers=_headers(admin_token),
            json={"realm": "global" if realm == "global" else "cn"},
        )
    payload = _json(response)
    return {
        "status": "waiting",
        "state": str(payload.get("state") or ""),
        "auth_url": str(payload.get("authUrl") or payload.get("auth_url") or ""),
        "realm": payload.get("realm") or realm,
    }


async def workbuddy_poll(
    base_url: str,
    admin_token: str,
    state: str,
    realm: str = "cn",
    region: str | None = None,
) -> dict[str, Any]:
    params = {"state": state, "realm": "global" if realm == "global" else "cn"}
    if region:
        params["region"] = region
    async with httpx.AsyncClient(timeout=_TIMEOUT) as client:
        response = await client.get(
            _url(base_url, "/api/auth/poll"),
            headers=_headers(admin_token),
            params=params,
        )
    payload = _json(response)
    status = str(payload.get("status") or "waiting")
    return {
        "status": status,
        "message": str(payload.get("message") or ""),
        "uid": str(payload.get("uid") or "") if payload.get("uid") else None,
        "nickname": str(payload.get("nickname") or "") if payload.get("nickname") else None,
        "realm": payload.get("realm"),
        "region_note": payload.get("region_note"),
    }


async def doubao_create(
    base_url: str,
    api_key: str,
    account_id: str,
    name: str,
    priority: int = 0,
) -> dict[str, Any]:
    async with httpx.AsyncClient(timeout=_TIMEOUT) as client:
        response = await client.post(
            _url(base_url, "/admin/api/accounts"),
            headers=_headers(api_key),
            json={
                "id": account_id,
                "name": name or account_id,
                "priority": priority,
                "enabled": True,
            },
        )
    payload = _json(response)
    return {
        "account_id": str(payload.get("id") or account_id),
        "name": str(payload.get("name") or name or account_id),
        "status": payload.get("status"),
    }


async def doubao_qr_start(base_url: str, api_key: str, account_id: str) -> dict[str, Any]:
    async with httpx.AsyncClient(timeout=_TIMEOUT) as client:
        response = await client.post(
            _url(base_url, "/v1/session/qr-login"),
            headers=_headers(api_key),
            params={"account_id": account_id},
        )
    payload = _json(response)
    return {
        "status": str(payload.get("status") or "waiting_scan"),
        "account_id": account_id,
        "qr_image_base64": payload.get("qr_image_base64"),
        "message": str(payload.get("message") or ""),
    }


async def doubao_qr_poll(base_url: str, api_key: str, account_id: str) -> dict[str, Any]:
    async with httpx.AsyncClient(timeout=_TIMEOUT) as client:
        response = await client.get(
            _url(base_url, "/v1/session/qr-login"),
            headers=_headers(api_key),
            params={"account_id": account_id},
        )
    payload = _json(response)
    return {
        "status": str(payload.get("status") or "waiting_scan"),
        "account_id": account_id,
        "qr_image_base64": payload.get("qr_image_base64"),
        "message": str(payload.get("message") or ""),
        "error": str(payload.get("error") or "") if payload.get("error") else None,
    }


async def chatgpt_oauth_start(base_url: str, auth_key: str, email_hint: str = "") -> dict[str, Any]:
    async with httpx.AsyncClient(timeout=_TIMEOUT) as client:
        response = await client.post(
            _url(base_url, "/api/accounts/oauth/start"),
            headers=_headers(auth_key),
            json={"email_hint": email_hint},
        )
    payload = _json(response)
    return {
        "session_id": str(payload.get("session_id") or ""),
        "authorize_url": str(payload.get("authorize_url") or ""),
        "expires_in": payload.get("expires_in"),
        "redirect_uri_prefix": payload.get("redirect_uri_prefix"),
    }


async def chatgpt_oauth_finish(
    base_url: str,
    auth_key: str,
    session_id: str,
    callback: str,
) -> dict[str, Any]:
    async with httpx.AsyncClient(timeout=_TIMEOUT) as client:
        response = await client.post(
            _url(base_url, "/api/accounts/oauth/finish"),
            headers=_headers(auth_key),
            json={"session_id": session_id, "callback": callback},
        )
    payload = _json(response)
    return {
        "status": "success",
        "added": int(payload.get("added") or 0),
        "skipped": int(payload.get("skipped") or 0),
        "refreshed": int(payload.get("refreshed") or 0),
        "errors": payload.get("errors") if isinstance(payload.get("errors"), list) else [],
    }


async def chatgpt_import(
    base_url: str,
    auth_key: str,
    tokens: list[str],
    accounts: list[dict[str, Any]] | None = None,
) -> dict[str, Any]:
    async with httpx.AsyncClient(timeout=_TIMEOUT) as client:
        response = await client.post(
            _url(base_url, "/api/accounts"),
            headers=_headers(auth_key),
            json={"tokens": tokens, "accounts": accounts or []},
        )
    payload = _json(response)
    return {
        "status": "success",
        "added": int(payload.get("added") or 0),
        "skipped": int(payload.get("skipped") or 0),
        "refreshed": int(payload.get("refreshed") or 0),
        "errors": payload.get("errors") if isinstance(payload.get("errors"), list) else [],
    }


def upstream_error(exc: Exception) -> tuple[int, str]:
    if isinstance(exc, httpx.TimeoutException):
        return 504, "上游账号服务请求超时"
    if isinstance(exc, httpx.HTTPStatusError):
        status = exc.response.status_code
        if status in {401, 403}:
            return 502, "上游账号服务鉴权失败"
        if status == 404:
            return 502, "上游账号管理接口不存在"
        if 400 <= status < 500:
            return 400, "上游账号服务拒绝了请求"
        return 502, "上游账号服务返回错误"
    if isinstance(exc, httpx.HTTPError):
        return 502, "无法连接上游账号服务"
    return 502, "上游账号操作失败"
