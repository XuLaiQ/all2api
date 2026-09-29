from __future__ import annotations

from types import SimpleNamespace

import httpx
import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from app.adapters.chatgpt.provisioner import ChatGPTProvisioner
from app.adapters.native_runtime import NativeHttpAdapter
from app.adapters.workbuddy.client import WorkBuddyClient
from app.adapters.workbuddy.provisioner import MemoryCredentialStore as WorkBuddyStore
from app.adapters.workbuddy.provisioner import WorkBuddyProvisioner
from app.infrastructure import db, security
from app.ports.credentials import InMemoryCredentialStore
from app.routers import admin, gateway
from app.scheduler import pool


class _WorkBuddyRefreshClient:
    async def refresh_token(self, credentials, *, account_id: str):
        assert account_id == "cn:user-1"
        assert credentials["refresh_token"] == "old-refresh"
        return {
            "access_token": "new-access",
            "refresh_token": "new-refresh",
            "expires_at": 1234,
            "realm": "cn",
        }


class _ChatGPTRefreshClient:
    async def refresh_token(self, refresh_token: str):
        assert refresh_token == "old-refresh"
        return {
            "access_token": "new-access",
            "refresh_token": "new-refresh",
            "id_token": "new-id",
        }


@pytest.mark.asyncio
async def test_workbuddy_refresh_and_provider_hooks_keep_secrets_private():
    store = WorkBuddyStore()
    await store.atomic_write(
        "wb",
        "cn:user-1",
        {"access_token": "old-access", "refresh_token": "old-refresh"},
    )
    provisioner = WorkBuddyProvisioner(_WorkBuddyRefreshClient(), credential_store=store)

    refreshed = await provisioner.refresh_credential("wb:cn:user-1")
    assert refreshed["status"] == "refreshed"
    assert "new-access" not in str(refreshed)
    assert "new-refresh" not in str(refreshed)
    assert (await store.read("wb", "cn:user-1"))["access_token"] == "new-access"

    await provisioner.set_account_enabled("cn:user-1", False)
    assert "cn:user-1" in provisioner._disabled_accounts
    await provisioner.set_account_enabled("cn:user-1", True)
    await provisioner.delete_account("cn:user-1")
    assert "cn:user-1" in provisioner._deleted_accounts


@pytest.mark.asyncio
async def test_chatgpt_refresh_rotates_store_record_without_secret_dto():
    store = InMemoryCredentialStore()
    await store.atomic_write(
        "chatgpt",
        "token:account-1",
        {"access_token": "old-access", "refresh_token": "old-refresh"},
    )
    provisioner = ChatGPTProvisioner(store, _ChatGPTRefreshClient())

    refreshed = await provisioner.refresh_credential("chatgpt:token:account-1")
    assert refreshed["status"] == "refreshed"
    assert "new-access" not in str(refreshed)
    assert (await store.read("chatgpt", "token:account-1"))["access_token"] == "new-access"


def test_account_candidates_share_runtime_state_for_native_channels(tmp_path, monkeypatch):
    db_path = tmp_path / "pool.db"
    db.migrate(str(db_path))
    monkeypatch.setattr(pool, "get_settings", lambda: SimpleNamespace(db_path=str(db_path)))
    with db.database(str(db_path)) as conn:
        conn.executemany(
            """INSERT INTO accounts
            (id, channel, native_id, name, kind, status, enabled, created_at, updated_at)
            VALUES (?, ?, ?, ?, 'oauth', 'ready', ?, 1, 1)""",
            [
                ("doubao:demo-1", "doubao", "demo-1", "Doubao one", 1),
                ("chatgpt:token:demo-1", "chatgpt", "token:demo-1", "ChatGPT one", 1),
                ("chatgpt:token:disabled", "chatgpt", "token:disabled", "Disabled", 0),
            ],
        )

    has_doubao, doubao = pool.account_candidates("doubao", "model-a")
    has_chatgpt, chatgpt = pool.account_candidates("chatgpt", "model-a")
    assert has_doubao and [item.native_id for item in doubao] == ["demo-1"]
    assert has_chatgpt and [item.native_id for item in chatgpt] == ["token:demo-1"]
    lease = pool.acquire_account_lease(chatgpt[0])
    assert lease is not None
    lease.release()


def test_workbuddy_unqualified_model_uses_global_account_realm(tmp_path, monkeypatch):
    db_path = tmp_path / "workbuddy-realm-pool.db"
    db.migrate(str(db_path))
    monkeypatch.setattr(pool, "get_settings", lambda: SimpleNamespace(db_path=str(db_path)))
    with db.database(str(db_path)) as conn:
        conn.execute(
            """INSERT INTO accounts
            (id, channel, native_id, name, kind, status, enabled, created_at, updated_at)
            VALUES ('wb:global:user-1', 'wb', 'global:user-1', 'Global account',
                    'oauth', 'ready', 1, 1, 1)"""
        )

    has_snapshot, candidates = pool.workbuddy_candidates("default-model")
    assert has_snapshot is True
    assert [item.native_id for item in candidates] == ["global:user-1"]

    has_snapshot, candidates = pool.workbuddy_candidates("cn:default-model")
    assert has_snapshot is True
    assert candidates == []


@pytest.mark.asyncio
async def test_native_runtime_injects_selected_doubao_cookie_credentials():
    seen: list[httpx.Request] = []

    def respond(request: httpx.Request) -> httpx.Response:
        seen.append(request)
        return httpx.Response(200, json={"id": "native", "choices": []})

    store = InMemoryCredentialStore()
    await store.atomic_write(
        "doubao",
        "demo-1",
        {"Cookie": "session=cookie-value", "msToken": "ms-token-value"},
    )
    client = httpx.AsyncClient(transport=httpx.MockTransport(respond))
    adapter = NativeHttpAdapter(
        SimpleNamespace(slug="doubao"),
        "https://doubao.example",
        http_client=client,
        credential_store=store,
        channel="doubao",
    )
    response = await adapter.invoke(
        {"model": "model-a", "payload": {"model": "model-a"}},
        {"account_id": "demo-1", "lease_account_id": "doubao:demo-1"},
    )
    await client.aclose()
    assert response.status_code == 200
    assert seen[0].headers["cookie"] == "session=cookie-value"
    assert seen[0].headers["x-ms-token"] == "ms-token-value"


@pytest.mark.asyncio
async def test_workbuddy_runtime_routes_global_credentials_to_global_platform():
    seen: list[httpx.Request] = []

    def respond(request: httpx.Request) -> httpx.Response:
        seen.append(request)
        return httpx.Response(200, json={"id": "native", "choices": []})

    store = WorkBuddyStore()
    await store.atomic_write(
        "wb",
        "global:user-1",
        {
            "access_token": "access-token",
            "realm": "global",
            "domain": "www.workbuddy.ai",
            "uid": "user-1",
        },
    )
    client = httpx.AsyncClient(transport=httpx.MockTransport(respond))
    workbuddy = WorkBuddyClient("https://copilot.tencent.com", http_client=client)
    adapter = NativeHttpAdapter(
        SimpleNamespace(slug="wb"),
        workbuddy.base_url,
        http_client=client,
        credential_store=store,
        channel="wb",
        chat_path=WorkBuddyClient.CHAT_PATH,
        base_url_resolver=workbuddy.base_url_for_credentials,
        credential_headers_resolver=workbuddy.runtime_headers,
    )
    response = await adapter.invoke(
        {
            "model": "model-a",
            "payload": {
                "model": "model-a",
                "messages": [{"role": "user", "content": "hello"}],
            },
        },
        {"account_id": "global:user-1"},
    )
    await client.aclose()
    assert response.status_code == 200
    assert str(seen[0].url) == "https://www.workbuddy.ai/v2/chat/completions"
    assert seen[0].headers["x-machine-id"]
    assert seen[0].headers["x-session-id"]


def test_admin_refresh_is_admin_only_and_audited(tmp_path, monkeypatch):
    db_path = tmp_path / "refresh-api.db"
    db.migrate(str(db_path))
    with db.database(str(db_path)) as conn:
        conn.execute(
            """INSERT INTO accounts
            (id, channel, native_id, name, kind, status, enabled, created_at, updated_at)
            VALUES ('chatgpt:token:demo', 'chatgpt', 'token:demo', 'Demo', 'oauth',
                    'ready', 1, 1, 1)"""
        )

    class Provisioner:
        seen: list[str] = []

        async def refresh_credential(self, account_id: str):
            self.seen.append(account_id)
            return {"status": "refreshed", "account": {"id": "chatgpt:token:demo"}}

    provisioner = Provisioner()
    settings = SimpleNamespace(db_path=str(db_path), credential_master_key="test-master")
    monkeypatch.setattr(admin, "get_settings", lambda: settings)
    monkeypatch.setattr(
        admin,
        "get_registry",
        lambda: {"chatgpt": SimpleNamespace(provisioner=provisioner)},
    )
    app = FastAPI()
    app.dependency_overrides[security.require_admin_request] = lambda: {
        "role": "admin",
        "username": "tester",
    }
    app.include_router(admin.router)
    with TestClient(app) as client:
        response = client.post("/admin/api/accounts/chatgpt:token:demo/refresh")
    assert response.status_code == 200
    assert provisioner.seen == ["token:demo"]
    assert response.json()["data"]["refreshed"] is True
    with db.database(str(db_path)) as conn:
        assert conn.execute(
            "SELECT COUNT(*) FROM audit_logs WHERE action = 'refresh_account'"
        ).fetchone()[0] == 1


def test_gateway_selected_native_account_falls_back_to_lease_id(tmp_path, monkeypatch):
    db_path = tmp_path / "gateway-selection.db"
    db.migrate(str(db_path))
    monkeypatch.setattr(gateway, "get_settings", lambda: SimpleNamespace(db_path=str(db_path)))
    headers = httpx.Headers()
    assert gateway._selected_account_id(
        "request-1",
        headers,
        {
            "channel": "chatgpt",
            "account_id": "token:demo",
            "lease_account_id": "chatgpt:token:demo",
        },
    ) == "chatgpt:token:demo"


def test_native_local_account_can_route_without_public_channel_key(tmp_path, monkeypatch):
    db_path = tmp_path / "native-local-account.db"
    db.migrate(str(db_path))
    monkeypatch.setattr(gateway, "get_settings", lambda: SimpleNamespace(db_path=str(db_path)))
    with db.database(str(db_path)) as conn:
        conn.execute(
            """INSERT INTO accounts
            (id, channel, native_id, name, kind, status, enabled, created_at, updated_at)
            VALUES ('doubao:demo', 'doubao', 'demo', 'Demo', 'browser', 'ready', 1, 1, 1)"""
        )
    adapter = SimpleNamespace(
        slug="doubao",
        name="Doubao",
        models_configured=False,
        runtime=object(),
    )
    _, targets = gateway._resolve_targets(
        "doubao/model-a",
        {"channels": "[]", "models": '["doubao/*"]'},
        {"doubao": adapter},
    )
    assert targets == [{"channel": "doubao", "upstream_model": "model-a"}]
