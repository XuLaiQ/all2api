"""Pytest configuration for real-platform E2E tests.

The E2E suite may read credentials from explicitly configured source files so
local verification does not require copying secrets into a second account
store.  Values stay in process memory and are never included in test output.
"""

from __future__ import annotations

import json
import os
from pathlib import Path
from typing import Any

import pytest

REPO_ROOT = Path(__file__).resolve().parents[4]
DEFAULT_CHATGPT_EXPORT = REPO_ROOT / "sub2api-export-1790396257763.json"
DEFAULT_WORKBUDDY_AUTH_DIR = Path(r"F:\token-p\WorkBuddy\wb2api\data\auths")
DEFAULT_DOUBAO_PROFILE_ROOT = Path(r"F:\token-p\反代\doubao2api\data\accounts")


def _truthy(name: str, default: bool = False) -> bool:
    value = os.getenv(name)
    if value is None:
        return default
    return value.strip().lower() in {"1", "true", "yes", "on"}


def _read_json(path: Path) -> dict[str, Any] | None:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, json.JSONDecodeError):
        return None
    return value if isinstance(value, dict) else None


def _path_from_env(name: str, fallback: Path) -> Path:
    value = os.getenv(name, "").strip()
    return Path(value).expanduser() if value else fallback


def _first_text(*values: Any) -> str:
    for value in values:
        if isinstance(value, str) and value.strip():
            return value.strip()
    return ""


def _source_accounts_enabled() -> bool:
    return _truthy("E2E_USE_SOURCE_ACCOUNTS")


def _load_chatgpt_source() -> dict[str, str]:
    path = _path_from_env("CHATGPT_TEST_EXPORT_FILE", DEFAULT_CHATGPT_EXPORT)
    payload = _read_json(path)
    accounts = payload.get("accounts") if isinstance(payload, dict) else None
    if not isinstance(accounts, list) or not accounts:
        return {}
    try:
        index = max(0, int(os.getenv("CHATGPT_TEST_ACCOUNT_INDEX", "0")))
    except ValueError:
        index = 0
    item = accounts[index] if index < len(accounts) else accounts[0]
    if not isinstance(item, dict):
        return {}
    credentials = item.get("credentials")
    if not isinstance(credentials, dict):
        return {}
    return {
        "access_token": _first_text(credentials.get("access_token")),
        "refresh_token": _first_text(credentials.get("refresh_token")),
        "id_token": _first_text(credentials.get("id_token")),
        "email": _first_text(credentials.get("email"), item.get("name")),
        "chatgpt_account_id": _first_text(credentials.get("chatgpt_account_id")),
        "chatgpt_user_id": _first_text(credentials.get("chatgpt_user_id")),
        "client_id": _first_text(credentials.get("client_id")),
        "expires_at": _first_text(credentials.get("expires_at")),
    }


def _load_workbuddy_source() -> dict[str, str]:
    auth_dir = _path_from_env("WB_TEST_AUTH_DIR", DEFAULT_WORKBUDDY_AUTH_DIR)
    configured_file = os.getenv("WB_TEST_AUTH_FILE", "").strip()
    candidates = (
        [Path(configured_file)]
        if configured_file
        else sorted(auth_dir.glob("workbuddy*.json"))
    )
    if not candidates:
        return {}
    selected = next((path for path in candidates if path.is_file()), None)
    if selected is None:
        return {}
    payload = _read_json(selected)
    auth = payload.get("auth") if isinstance(payload, dict) else None
    account = payload.get("account") if isinstance(payload, dict) else None
    if not isinstance(auth, dict) or not isinstance(account, dict):
        return {}
    domain = _first_text(auth.get("domain"))
    realm = _first_text(auth.get("realm"), payload.get("realm"))
    if not realm:
        realm = "global" if domain.lower().endswith(".workbuddy.ai") else "cn"
    return {
        "access_token": _first_text(auth.get("accessToken"), auth.get("access_token")),
        "refresh_token": _first_text(auth.get("refreshToken"), auth.get("refresh_token")),
        "device_token": _first_text(auth.get("deviceToken"), auth.get("device_token")),
        "expires_at": _first_text(auth.get("expiresAt"), auth.get("expires_at")),
        "realm": realm.lower(),
        "uid": _first_text(account.get("uid"), account.get("userId")),
        "nickname": _first_text(account.get("nickname"), account.get("nick")),
        "domain": domain,
        "enterprise_id": _first_text(
            account.get("enterpriseId"), account.get("enterprise_id"), auth.get("enterpriseId")
        ),
    }


def _load_doubao_source() -> dict[str, Any]:
    root = _path_from_env("DOUBAO_TEST_PROFILE_ROOT", DEFAULT_DOUBAO_PROFILE_ROOT)
    account_id = os.getenv("DOUBAO_TEST_ACCOUNT_ID", "").strip()
    if not account_id:
        candidates = sorted(
            path.name
            for path in root.iterdir()
            if path.is_dir() and (path / "browser").is_dir()
        ) if root.is_dir() else []
        account_id = next(
            (value for value in candidates if value != "default"),
            candidates[0] if candidates else "",
        )
    profile_path = root / account_id / "browser" if account_id else Path()
    if not profile_path.is_dir():
        return {}
    meta = _read_json(root / account_id / "meta.json") or {}
    return {
        "account_id": account_id,
        "profile_root": str(root),
        "profile_path": str(profile_path),
        "meta": {
            key: value
            for key, value in meta.items()
            if key not in {"browser_data", "cookies"}
        },
    }


def pytest_configure(config):
    """Register custom markers."""
    config.addinivalue_line("markers", "e2e: End-to-end tests with real platforms")
    config.addinivalue_line("markers", "browser: Tests requiring Playwright browser")
    config.addinivalue_line("markers", "slow: Slow-running tests (>1 minute)")


def pytest_collection_modifyitems(config, items):
    """Skip E2E tests unless explicitly enabled."""
    # Check if E2E tests are enabled globally
    e2e_enabled = os.getenv("E2E_ENABLED", "false").lower() == "true"
    
    skip_e2e = pytest.mark.skip(reason="E2E tests disabled (set E2E_ENABLED=true)")
    skip_browser = pytest.mark.skip(
        reason="Browser tests disabled (set E2E_DOUBAO_ENABLED=true and install playwright)"
    )
    
    for item in items:
        # Skip all E2E tests if not enabled
        if "e2e" in item.keywords and not e2e_enabled:
            item.add_marker(skip_e2e)
        
        # Skip browser tests if not explicitly enabled
        if "browser" in item.keywords:
            doubao_enabled = os.getenv("E2E_DOUBAO_ENABLED", "false").lower() == "true"
            if not doubao_enabled:
                item.add_marker(skip_browser)


@pytest.fixture
def chatgpt_test_account():
    """
    Fixture providing ChatGPT test account credentials.
    
    Reads from environment variables:
    - CHATGPT_TEST_ACCESS_TOKEN
    - CHATGPT_TEST_REFRESH_TOKEN
    - CHATGPT_TEST_EMAIL
    """
    source = _load_chatgpt_source() if _source_accounts_enabled() else {}
    access_token = _first_text(
        source.get("access_token"), os.getenv("CHATGPT_TEST_ACCESS_TOKEN")
    )
    refresh_token = _first_text(
        source.get("refresh_token"), os.getenv("CHATGPT_TEST_REFRESH_TOKEN")
    )
    email = _first_text(source.get("email"), os.getenv("CHATGPT_TEST_EMAIL"))
    
    if not all([access_token, refresh_token, email]):
        pytest.skip("ChatGPT test account credentials not configured")
    
    return {
        "access_token": access_token,
        "refresh_token": refresh_token,
        "email": email,
        "id_token": _first_text(source.get("id_token"), os.getenv("CHATGPT_TEST_ID_TOKEN")),
        "chatgpt_account_id": _first_text(
            source.get("chatgpt_account_id"), os.getenv("CHATGPT_TEST_ACCOUNT_ID")
        ),
        "chatgpt_user_id": _first_text(
            source.get("chatgpt_user_id"), os.getenv("CHATGPT_TEST_USER_ID")
        ),
        "client_id": _first_text(source.get("client_id"), os.getenv("CHATGPT_TEST_CLIENT_ID")),
        "expires_at": _first_text(source.get("expires_at"), os.getenv("CHATGPT_TEST_EXPIRES_AT")),
    }


@pytest.fixture
def workbuddy_test_account():
    """
    Fixture providing WorkBuddy test account credentials.
    
    Reads from environment variables:
    - WB_TEST_ACCESS_TOKEN
    - WB_TEST_REFRESH_TOKEN
    - WB_TEST_REALM
    """
    source = _load_workbuddy_source() if _source_accounts_enabled() else {}
    access_token = _first_text(source.get("access_token"), os.getenv("WB_TEST_ACCESS_TOKEN"))
    refresh_token = _first_text(source.get("refresh_token"), os.getenv("WB_TEST_REFRESH_TOKEN"))
    realm = _first_text(source.get("realm"), os.getenv("WB_TEST_REALM"), "cn")
    
    if not all([access_token, refresh_token]):
        pytest.skip("WorkBuddy test account credentials not configured")
    
    return {
        "access_token": access_token,
        "refresh_token": refresh_token,
        "realm": realm,
        "device_token": _first_text(source.get("device_token"), os.getenv("WB_TEST_DEVICE_TOKEN")),
        "expires_at": _first_text(source.get("expires_at"), os.getenv("WB_TEST_EXPIRES_AT")),
        "uid": _first_text(source.get("uid"), os.getenv("WB_TEST_UID")),
        "nickname": _first_text(source.get("nickname"), os.getenv("WB_TEST_NICKNAME")),
        "domain": _first_text(source.get("domain"), os.getenv("WB_TEST_DOMAIN")),
        "enterprise_id": _first_text(
            source.get("enterprise_id"), os.getenv("WB_TEST_ENTERPRISE_ID")
        ),
    }


@pytest.fixture
def doubao_test_account():
    """
    Fixture providing Doubao test account credentials.
    
    Reads from environment variables:
    - DOUBAO_TEST_COOKIES (JSON string)
    - DOUBAO_TEST_ACCOUNT_ID
    """
    source = _load_doubao_source()
    if not source:
        pytest.skip("Doubao real browser profile is not configured")
    return source
