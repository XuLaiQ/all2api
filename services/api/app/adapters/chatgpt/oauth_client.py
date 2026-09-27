"""Small OAuth PKCE client used by the built-in ChatGPT provisioner."""

from __future__ import annotations

import base64
import hashlib
import secrets
import urllib.parse
from dataclasses import dataclass
from typing import Any

import httpx

from .errors import OAuthProtocolError


def _b64(value: bytes) -> str:
    return base64.urlsafe_b64encode(value).rstrip(b"=").decode("ascii")


@dataclass(frozen=True)
class OAuthConfig:
    authorize_endpoint: str = "https://auth.openai.com/oauth/authorize"
    token_endpoint: str = "https://auth.openai.com/oauth/token"
    client_id: str = "all2api"
    redirect_uri: str = "http://127.0.0.1:1455/auth/callback"
    scopes: tuple[str, ...] = ("openid", "profile", "email", "offline_access")


@dataclass(frozen=True)
class PKCERequest:
    state: str
    verifier: str
    challenge: str
    authorize_url: str


class OAuthClient:
    """OAuth transport with an injectable HTTP client for contract tests."""

    def __init__(
        self,
        config: OAuthConfig | None = None,
        http_client: httpx.AsyncClient | Any | None = None,
    ) -> None:
        self.config = config or OAuthConfig()
        self.http_client = http_client

    def begin(self, *, email_hint: str = "") -> PKCERequest:
        state = secrets.token_urlsafe(32)
        verifier = secrets.token_urlsafe(64)
        challenge = _b64(hashlib.sha256(verifier.encode("ascii")).digest())
        params = {
            "client_id": self.config.client_id,
            "redirect_uri": self.config.redirect_uri,
            "response_type": "code",
            "scope": " ".join(self.config.scopes),
            "state": state,
            "code_challenge": challenge,
            "code_challenge_method": "S256",
        }
        if email_hint.strip():
            params["login_hint"] = email_hint.strip()
        url = f"{self.config.authorize_endpoint}?{urllib.parse.urlencode(params)}"
        return PKCERequest(state=state, verifier=verifier, challenge=challenge, authorize_url=url)

    async def exchange_code(self, *, code: str, verifier: str) -> dict[str, Any]:
        if not code.strip() or not verifier.strip():
            raise OAuthProtocolError("OAuth 回调缺少授权码")
        payload = {
            "grant_type": "authorization_code",
            "client_id": self.config.client_id,
            "redirect_uri": self.config.redirect_uri,
            "code": code,
            "code_verifier": verifier,
        }
        if self.http_client is None:
            async with httpx.AsyncClient(timeout=httpx.Timeout(20, connect=5)) as client:
                response = await client.post(self.config.token_endpoint, data=payload)
        else:
            response = await self.http_client.post(self.config.token_endpoint, data=payload)
        try:
            response.raise_for_status()
            result = response.json()
        except (httpx.HTTPError, ValueError) as exc:
            raise OAuthProtocolError("OAuth token exchange failed") from exc
        if not isinstance(result, dict) or not isinstance(result.get("access_token"), str):
            raise OAuthProtocolError("OAuth token response is invalid")
        return result

    @staticmethod
    def parse_callback(callback: str) -> dict[str, str]:
        parsed = urllib.parse.urlparse(callback)
        query = urllib.parse.parse_qs(parsed.query, keep_blank_values=True)
        values = {key: items[0] for key, items in query.items() if items}
        if values.get("error"):
            raise OAuthProtocolError("OAuth 授权被拒绝")
        return values
