"""Persistence contract tests for native account provision sessions."""

from __future__ import annotations

import sqlite3
import time
from pathlib import Path
from urllib.parse import parse_qs, urlparse

import pytest

from app.adapters.chatgpt.oauth_client import PKCERequest
from app.adapters.chatgpt.provisioner import ChatGPTProvisioner
from app.adapters.doubao.browser import FakeBrowserWorker
from app.adapters.doubao.provisioner import DoubaoProvisioner
from app.adapters.workbuddy.provisioner import WorkBuddyProvisioner
from app.ports.credentials import InMemoryCredentialStore
from app.infrastructure.provision_state import ProvisionStateStore


class _OAuthFake:
    def __init__(self) -> None:
        self.request = PKCERequest(
            state="state-1",
            verifier="private-verifier",
            challenge="challenge-1",
            authorize_url="https://auth.example/authorize?state=state-1",
        )

    def begin(self, *, email_hint: str = "") -> PKCERequest:
        return self.request

    def parse_callback(self, callback: str) -> dict[str, str]:
        parsed = urlparse(callback)
        return {key: values[0] for key, values in parse_qs(parsed.query).items()}

    async def exchange_code(self, *, code: str, verifier: str) -> dict[str, str]:
        assert code == "code-1"
        assert verifier == "private-verifier"
        return {"access_token": "secret-token", "email": "state@example.com"}


class _WorkBuddyClient:
    async def start_qr(self, realm: str) -> dict[str, str]:
        return {
            "state": "private-provider-state",
            "auth_url": "https://login.example/qr",
            "realm": realm,
        }

    async def poll_qr(self, state: str, realm: str) -> dict[str, str]:
        assert state == "private-provider-state"
        return {
            "status": "ready",
            "realm": realm,
            "uid": "restart-user",
            "nickname": "Restart User",
            "access_token": "secret-access",
        }


@pytest.mark.asyncio
async def test_state_store_encrypts_private_state_and_replays_idempotency(tmp_path: Path):
    path = tmp_path / "state.db"
    store = ProvisionStateStore(str(path), "test-master-key")
    now = time.time()
    store.save_session(
        "chatgpt",
        "session-1",
        flow="oauth-pkce",
        status="waiting_callback",
        created_at=now,
        expires_at=now + 100,
        idempotency_key="start-1",
        state={"verifier": "secret-verifier", "state": "state-1"},
    )
    store.remember_idempotency(
        "chatgpt",
        "start:oauth-pkce",
        "start-1",
        {"session_id": "session-1", "status": "waiting_callback"},
        session_id="session-1",
        expires_at=now + 100,
    )

    conn = sqlite3.connect(path)
    try:
        raw_state = conn.execute(
            "SELECT state FROM provision_sessions WHERE session_id = 'session-1'"
        ).fetchone()[0]
    finally:
        conn.close()
    assert "secret-verifier" not in raw_state

    restarted = ProvisionStateStore(str(path), "test-master-key")
    assert restarted.load_session("chatgpt", "session-1")["verifier"] == "secret-verifier"
    replay = restarted.get_idempotency("chatgpt", "start:oauth-pkce", "start-1")
    assert replay is not None
    assert replay.session_id == "session-1"


@pytest.mark.asyncio
async def test_chatgpt_oauth_session_survives_provisioner_restart(tmp_path: Path):
    state = ProvisionStateStore(str(tmp_path / "chatgpt.db"), "test-master-key")
    credentials = InMemoryCredentialStore()
    first = ChatGPTProvisioner(credentials, _OAuthFake(), state_store=state)
    started = await first.start("oauth-pkce", {}, "oauth-start")

    restarted = ChatGPTProvisioner(credentials, _OAuthFake(), state_store=state)
    assert await restarted.start("oauth-pkce", {}, "oauth-start") == started
    assert (await restarted.poll(started["session_id"]))["status"] == "waiting_callback"
    completed = await restarted.complete(
        started["session_id"],
        {"callback": "http://127.0.0.1/callback?code=code-1&state=state-1"},
        "oauth-complete",
    )
    replay = ChatGPTProvisioner(credentials, _OAuthFake(), state_store=state)
    assert await replay.complete(started["session_id"], {}, "oauth-complete") == completed


@pytest.mark.asyncio
async def test_workbuddy_qr_session_survives_provisioner_restart(tmp_path: Path):
    state = ProvisionStateStore(str(tmp_path / "workbuddy.db"), "test-master-key")
    credentials = InMemoryCredentialStore()
    first = WorkBuddyProvisioner(
        _WorkBuddyClient(),
        credential_store=credentials,
        state_store=state,
    )
    started = await first.start("qr-oauth", {"realm": "cn"}, "wb-start")
    restarted = WorkBuddyProvisioner(
        _WorkBuddyClient(),
        credential_store=credentials,
        state_store=state,
    )
    assert await restarted.start("qr-oauth", {"realm": "cn"}, "wb-start") == started
    ready = await restarted.poll(started["session_id"])
    assert ready["status"] == "ready"
    completed = await restarted.complete(started["session_id"], {}, "wb-complete")
    replay = WorkBuddyProvisioner(
        _WorkBuddyClient(),
        credential_store=credentials,
        state_store=state,
    )
    assert await replay.complete(started["session_id"], {}, "wb-complete") == completed


@pytest.mark.asyncio
async def test_doubao_qr_session_state_survives_provisioner_restart(tmp_path: Path):
    state = ProvisionStateStore(str(tmp_path / "doubao.db"), "test-master-key")
    worker = FakeBrowserWorker(events=("waiting_scan", "scanned", "confirmed"))
    first = DoubaoProvisioner(
        profile_root=tmp_path / "profiles",
        browser_worker=worker,
        state_store=state,
    )
    started = await first.start("qr-login", {"account_id": "restart"}, "doubao-start")
    restarted = DoubaoProvisioner(
        profile_root=tmp_path / "profiles",
        browser_worker=FakeBrowserWorker(events=("waiting_scan", "scanned", "confirmed")),
        state_store=state,
    )
    assert await restarted.start("qr-login", {"account_id": "restart"}, "doubao-start") == started
    assert (await restarted.poll(started["session_id"]))["status"] == "waiting_scan"
    assert (await restarted.poll(started["session_id"]))["status"] == "scanned"
