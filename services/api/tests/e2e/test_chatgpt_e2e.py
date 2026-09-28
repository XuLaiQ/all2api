"""Real ChatGPT Web E2E tests.

These tests require an explicit E2E flag and real OAuth credentials. They do
not use the OpenAI API-key endpoint because the supplied credentials are
ChatGPT Web OAuth tokens.
"""

from __future__ import annotations

import json
import os
from datetime import UTC, datetime

import pytest

pytestmark = [
    pytest.mark.e2e,
    pytest.mark.skipif(
        os.getenv("E2E_CHATGPT_ENABLED", "false").lower() != "true",
        reason="ChatGPT E2E tests disabled",
    ),
]


def _web_client(account: dict[str, str]):
    from app.adapters.chatgpt.client import ChatGPTWebClient

    return ChatGPTWebClient(account)


class TestChatGPTRealPlatform:
    """Exercise the actual ChatGPT Web endpoints with the supplied account."""

    @pytest.mark.asyncio
    async def test_token_import_and_validation(self, chatgpt_test_account):
        import jwt

        claims = jwt.decode(
            chatgpt_test_account["access_token"],
            options={"verify_signature": False},
        )
        assert claims.get("sub")
        assert claims.get("exp")
        profile = claims.get("https://api.openai.com/profile")
        assert isinstance(profile, dict)
        assert profile.get("email") == chatgpt_test_account["email"]
        assert datetime.fromtimestamp(int(claims["exp"]), UTC) > datetime.now(UTC)

    @pytest.mark.asyncio
    async def test_list_models_real(self, chatgpt_test_account):
        models = await _web_client(chatgpt_test_account).list_models()
        assert models
        assert all(item.get("id") for item in models)

    @pytest.mark.asyncio
    async def test_chat_completion_real(self, chatgpt_test_account):
        client = _web_client(chatgpt_test_account)
        models = await client.list_models()
        response = await client.chat(
            {
                "model": models[0]["id"],
                "messages": [{"role": "user", "content": "Reply with one word: ready"}],
                "stream": False,
                "max_tokens": 8,
            }
        )
        assert response.status_code == 200
        payload = response.json()
        assert payload.get("choices")
        assert payload["choices"][0]["message"]["content"]

    @pytest.mark.asyncio
    async def test_token_refresh_real(self, chatgpt_test_account):
        if os.getenv("E2E_TEST_TOKEN_REFRESH", "false").lower() != "true":
            pytest.skip("Token refresh changes the real account; set E2E_TEST_TOKEN_REFRESH=true")

        from app.adapters.chatgpt.oauth_client import OAuthClient, OAuthConfig

        client_id = chatgpt_test_account.get("client_id") or "all2api"
        refreshed = await OAuthClient(OAuthConfig(client_id=client_id)).refresh_token(
            chatgpt_test_account["refresh_token"]
        )
        assert refreshed.get("access_token")


class TestChatGPTAdapterE2E:
    """Exercise All2API account provisioning and the native Web client."""

    @pytest.mark.asyncio
    async def test_adapter_import_account(self, chatgpt_test_account):
        from app.adapters.chatgpt.provisioner import ChatGPTProvisioner
        from app.ports.credentials import InMemoryCredentialStore

        store = InMemoryCredentialStore()
        result = await ChatGPTProvisioner(store).token_import(
            {
                "accounts": [
                    {
                        "access_token": chatgpt_test_account["access_token"],
                        "refresh_token": chatgpt_test_account["refresh_token"],
                        "id_token": chatgpt_test_account.get("id_token", ""),
                        "email": chatgpt_test_account["email"],
                        "chatgpt_user_id": chatgpt_test_account.get("chatgpt_user_id", ""),
                    }
                ]
            },
            "test_import_001",
        )
        assert result.get("status") == "success", result.get("errors")
        assert result.get("added") == 1
        assert chatgpt_test_account["access_token"] not in json.dumps(result)
        assert chatgpt_test_account["refresh_token"] not in json.dumps(result)

    @pytest.mark.asyncio
    async def test_adapter_list_models(self, chatgpt_test_account):
        models = await _web_client(chatgpt_test_account).list_models()
        assert models

    @pytest.mark.asyncio
    async def test_adapter_chat_completion(self, chatgpt_test_account):
        client = _web_client(chatgpt_test_account)
        models = await client.list_models()
        response = await client.chat(
            {
                "model": models[0]["id"],
                "messages": [{"role": "user", "content": "Reply with one word: ready"}],
                "stream": True,
                "max_tokens": 8,
            }
        )
        assert response.status_code == 200
        assert "data:" in response.text
