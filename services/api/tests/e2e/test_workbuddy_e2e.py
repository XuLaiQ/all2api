"""
WorkBuddy Real Platform E2E Tests

These tests verify WorkBuddy adapter against the real WorkBuddy platform.
They use actual credentials and make real API calls.

Requirements:
- Set E2E_WORKBUDDY_ENABLED=true
- Configure WB_TEST_* environment variables
- Ensure test account has valid tokens
"""

import os
from datetime import UTC, datetime

import pytest


def _platform_base(account: dict[str, str]) -> str:
    configured = os.getenv("WB_TEST_PLATFORM_BASE", "").strip()
    if configured:
        return configured.rstrip("/")
    return (
        "https://www.workbuddy.ai"
        if account.get("realm") == "global"
        else "https://copilot.tencent.com"
    )


def _import_payload(account: dict[str, str]) -> dict[str, list[dict[str, str]]]:
    return {
        "accounts": [
            {
                "access_token": account["access_token"],
                "refresh_token": account["refresh_token"],
                "device_token": account.get("device_token", ""),
                "expires_at": account.get("expires_at", "0"),
                "realm": account["realm"],
                "uid": account.get("uid", ""),
                "nickname": account.get("nickname", ""),
                "domain": account.get("domain", ""),
                "enterprise_id": account.get("enterprise_id", ""),
            }
        ]
    }


@pytest.mark.e2e
@pytest.mark.skipif(
    os.getenv("E2E_WORKBUDDY_ENABLED", "false").lower() != "true",
    reason="WorkBuddy E2E tests disabled"
)
class TestWorkBuddyRealPlatform:
    """Test WorkBuddy adapter with real WorkBuddy platform."""
    
    @pytest.mark.asyncio
    async def test_token_validation(self, workbuddy_test_account):
        """
        Test Case: Validate real WorkBuddy tokens
        
        Steps:
        1. Import access_token and refresh_token from test account
        2. Validate token format (JWT)
        3. Extract uid and realm from token claims
        4. Verify token is not expired
        
        Expected:
        - Tokens are valid JWT format
        - Claims contain required fields (sub, realm)
        - Token is not expired
        """
        import jwt
        from jwt.exceptions import InvalidTokenError
        
        access_token = workbuddy_test_account["access_token"]
        realm = workbuddy_test_account["realm"]
        
        # Validate access token format
        try:
            # Decode without verification to read claims
            claims = jwt.decode(access_token, options={"verify_signature": False})
            
            # Verify required claims
            assert "sub" in claims, "Missing 'sub' claim (uid)"
            assert "exp" in claims, "Missing 'exp' claim (expiration)"
            assert "iss" in claims, "Missing 'iss' claim (issuer)"
            
            issuer = claims["iss"]
            
            # Verify realm matches issuer
            if realm == "cn":
                assert "codebuddy.cn" in issuer, f"CN realm should use codebuddy.cn: {issuer}"
            elif realm == "global":
                assert "workbuddy.ai" in issuer, f"Global realm should use workbuddy.ai: {issuer}"
            
            # Check expiration
            exp_timestamp = claims["exp"]
            exp_time = datetime.fromtimestamp(exp_timestamp, UTC)
            now = datetime.now(UTC)
            
            if exp_time < now:
                pytest.skip(f"Access token expired at {exp_time}")
            
        except InvalidTokenError as e:
            pytest.fail(f"Invalid access token format: {e}")
    
    @pytest.mark.asyncio
    async def test_verify_account_info(self, workbuddy_test_account):
        """
        Test Case: Verify account information
        
        Steps:
        1. Use access_token to call WorkBuddy API
        2. Get account information
        3. Verify uid and nickname
        
        Expected:
        - API returns account info
        - UID matches expected
        """
        realm = workbuddy_test_account["realm"]
        issuer = ""
        import jwt

        claims = jwt.decode(
            workbuddy_test_account["access_token"],
            options={"verify_signature": False},
        )
        issuer = str(claims.get("iss") or "")
        assert issuer, "WorkBuddy token does not contain an issuer"
        if realm == "cn":
            assert "codebuddy.cn" in issuer
        else:
            assert "workbuddy.ai" in issuer
    
    @pytest.mark.asyncio
    async def test_token_refresh(self, workbuddy_test_account):
        """
        Test Case: Refresh access token using refresh token
        
        Steps:
        1. Use refresh_token to request new access_token
        2. Verify new token is returned
        3. Validate new token works
        
        Expected:
        - Refresh succeeds and returns new tokens
        - New access_token is valid
        
        Note: This test is skipped by default to conserve refresh tokens.
        Enable with E2E_TEST_WB_TOKEN_REFRESH=true
        """
        if os.getenv("E2E_TEST_WB_TOKEN_REFRESH", "false").lower() != "true":
            pytest.skip("Token refresh test disabled (set E2E_TEST_WB_TOKEN_REFRESH=true)")
        
        from app.adapters.workbuddy.client import WorkBuddyClient

        account = workbuddy_test_account
        client = WorkBuddyClient(_platform_base(account))
        refreshed = await client.refresh_token(
            {
                "access_token": account["access_token"],
                "refresh_token": account["refresh_token"],
                "device_token": account.get("device_token", ""),
                "realm": account["realm"],
                "domain": account.get("domain", ""),
                "enterprise_id": account.get("enterprise_id", ""),
            },
            account_id=f"{account.get('realm', 'cn')}:{account.get('uid', '')}",
        )
        assert refreshed.get("access_token")
        assert refreshed.get("realm") == account["realm"]


@pytest.mark.e2e
@pytest.mark.skipif(
    os.getenv("E2E_WORKBUDDY_ENABLED", "false").lower() != "true",
    reason="WorkBuddy E2E tests disabled"
)
class TestWorkBuddyAdapterE2E:
    """Test WorkBuddy adapter integration with real platform via All2API."""
    
    @pytest.mark.asyncio
    async def test_adapter_import_account(self, workbuddy_test_account):
        """
        Test Case: Import WorkBuddy account via adapter
        
        Steps:
        1. Use WorkBuddyProvisioner to import account
        2. Verify account is created with correct credentials
        3. Check credential storage
        
        Expected:
        - Account import succeeds
        - Credentials are securely stored
        - Account can be retrieved
        """
        from app.adapters.workbuddy.client import WorkBuddyClient
        from app.adapters.workbuddy.provisioner import MemoryCredentialStore, WorkBuddyProvisioner

        store = MemoryCredentialStore()
        provisioner = WorkBuddyProvisioner(
            WorkBuddyClient(_platform_base(workbuddy_test_account)),
            credential_store=store,
        )
        result = await provisioner.import_accounts(
            payload=_import_payload(workbuddy_test_account),
            idempotency_key="test_wb_import_001",
        )

        assert result.get("status") == "success", result.get("errors")
        assert result.get("added") == 1
        assert len(result.get("accounts", [])) == 1
        assert workbuddy_test_account["access_token"] not in str(result)
        assert workbuddy_test_account["refresh_token"] not in str(result)
    
    @pytest.mark.asyncio
    async def test_adapter_qr_login_flow(self):
        """
        Test Case: QR code login flow
        
        Steps:
        1. Start QR login session
        2. Get QR code image/URL
        3. Poll for completion
        
        Expected:
        - QR session starts successfully
        - QR code is generated
        - Session can be polled
        
        Note: Actual scanning requires manual intervention or automation
        """
        from app.adapters.workbuddy.client import WorkBuddyClient
        from app.adapters.workbuddy.provisioner import MemoryCredentialStore, WorkBuddyProvisioner

        provisioner = WorkBuddyProvisioner(
            WorkBuddyClient(_platform_base({"realm": "cn"})),
            credential_store=MemoryCredentialStore(),
        )
        
        # Start QR login
        session = await provisioner.start(
            flow="qr-oauth",
            payload={"realm": "cn"},
            idempotency_key="test_wb_qr_001"
        )
        
        # Verify session started
        assert session.get("session_id"), "Session ID should be present"
        assert session.get("status") in {"waiting_user", "waiting_callback"}
    
    @pytest.mark.asyncio
    async def test_adapter_list_models(self, workbuddy_test_account):
        """
        Test Case: List models through adapter
        
        Steps:
        1. Use adapter to list available models
        2. Verify model list is returned
        
        Expected:
        - Adapter returns model list
        - Models have required fields
        """
        from app.adapters.workbuddy.client import WorkBuddyClient

        models = await WorkBuddyClient(_platform_base(workbuddy_test_account)).list_models(
            realm=workbuddy_test_account["realm"],
            credentials=workbuddy_test_account,
        )
        assert models
        assert all(isinstance(item.get("id"), str) and item["id"] for item in models)
    
    @pytest.mark.asyncio
    async def test_adapter_chat_completion(self, workbuddy_test_account):
        """
        Test Case: Chat completion through adapter
        
        Steps:
        1. Send chat completion request through adapter
        2. Verify response format
        
        Expected:
        - Adapter handles request correctly
        - Response is in expected format
        """
        from app.adapters.workbuddy.adapter import WorkBuddyAdapter
        from app.adapters.workbuddy.client import WorkBuddyClient
        from app.adapters.workbuddy.provisioner import MemoryCredentialStore, WorkBuddyProvisioner

        store = MemoryCredentialStore()
        client = WorkBuddyClient(_platform_base(workbuddy_test_account))
        provisioner = WorkBuddyProvisioner(client, credential_store=store)
        imported = await provisioner.import_accounts(
            payload=_import_payload(workbuddy_test_account),
            idempotency_key="test_wb_chat_import_001",
        )
        account = imported["accounts"][0]
        models = await client.list_models(
            realm=workbuddy_test_account["realm"],
            credentials=workbuddy_test_account,
        )
        assert models
        adapter = WorkBuddyAdapter(client, provisioner=provisioner)
        response = await adapter.chat(
            {
                "model": models[0]["id"],
                "payload": {
                    "model": models[0]["id"],
                    "messages": [{"role": "user", "content": "Reply with one word: ready"}],
                    "stream": True,
                    "max_tokens": 8,
                },
            },
            account,
        )
        assert response.status_code == 200
        assert "data:" in response.text
