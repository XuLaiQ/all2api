from __future__ import annotations

import asyncio
from collections.abc import Awaitable, Callable
from dataclasses import dataclass, field
from typing import Any

from app.adapters import doubao
from app.adapters.chatgpt.adapter import ChatGPTAdapter
from app.adapters.chatgpt.manifest import CHATGPT_MANIFEST
from app.adapters.chatgpt.oauth_client import OAuthClient, OAuthConfig
from app.adapters.chatgpt.provisioner import ChatGPTProvisioner
from app.adapters.doubao.native_qr import NativeDoubaoQrWorker
from app.adapters.native_runtime import NativeHttpAdapter
from app.adapters.provisioner import DeclarativeProvisioner
from app.adapters.workbuddy.client import WorkBuddyClient
from app.adapters.workbuddy.manifest import WORKBUDDY_MANIFEST
from app.adapters.workbuddy.provisioner import WorkBuddyProvisioner
from app.config import Settings, get_settings
from app.domain.channel import ChannelManifest
from app.infrastructure.credentials import DatabaseCredentialStore
from app.infrastructure.db import database
from app.infrastructure.provision_state import ProvisionStateStore

ModelReader = Callable[[str, str], Awaitable[list[dict[str, Any]]]]
AccountReader = Callable[[str, str], Awaitable[list[dict[str, Any]]]]


def _merge_models(groups: list[list[dict[str, Any]]]) -> list[dict[str, Any]]:
    merged: dict[str, dict[str, Any]] = {}
    for group in groups:
        for item in group:
            model_id = str(item.get("id") or "").strip()
            if model_id and model_id not in merged:
                merged[model_id] = dict(item)
    return list(merged.values())


async def _empty_accounts(_base_url: str, _key: str) -> list[dict[str, Any]]:
    """Native account catalogue is owned by the local credential store."""

    return []


@dataclass(frozen=True)
class AdapterSpec:
    slug: str
    name: str
    adapter: str
    base_url: str
    model_key: str = field(repr=False)
    account_key: str = field(repr=False)
    model_reader: ModelReader
    account_reader: AccountReader
    # ``base_url`` is retained as the compatibility/legacy data-plane base.
    # Native provisioners receive their provider ``platform_base_url``.
    platform_base_url: str = ""
    legacy_bridge_enabled: bool = False
    base_env: str = field(default="", repr=False)
    model_env: str = field(default="", repr=False)
    account_env: str = field(default="", repr=False)
    allow_empty_key: bool = False
    protocols: tuple[str, ...] = ("openai",)
    caps: tuple[str, ...] = ("chat",)
    manifest: ChannelManifest | None = None
    provisioner: Any | None = field(default=None, repr=False)
    upstream_adapter: Any | None = field(default=None, repr=False)
    native_model_configured: bool = False

    def __post_init__(self) -> None:
        if self.manifest is None:
            object.__setattr__(
                self,
                "manifest",
                ChannelManifest(
                    slug=self.slug,
                    display_name=self.name,
                    adapter_version="0.1.0",
                    protocols=tuple(self.protocols),
                    capabilities=tuple(self.caps),
                ),
            )
        if self.provisioner is None:
            object.__setattr__(
                self,
                "provisioner",
                DeclarativeProvisioner(self.manifest.account_flows),
            )

    @property
    def models_configured(self) -> bool:
        if self.upstream_adapter is not None:
            return bool(self.native_model_configured and self.platform_base_url)
        return bool(
            (self.legacy_bridge_enabled or not self.platform_base_url)
            and self.base_url
            and (self.model_key or self.allow_empty_key)
        )

    @property
    def accounts_configured(self) -> bool:
        return not self.account_config_missing

    @property
    def provision_configured(self) -> bool:
        """Whether the native account provisioner is available.

        ``accounts_configured`` remains the legacy upstream-management
        configuration used by compatibility sync paths. Native provisioners
        expose target flows independently of those bridge credentials.
        """

        return not isinstance(self.provisioner, DeclarativeProvisioner)

    @property
    def account_config_missing(self) -> tuple[str, ...]:
        """Return the environment variables required by account management.

        The management UI needs to distinguish an unreachable upstream from an
        adapter that cannot even attempt onboarding.  Keep this derived from the
        same values used by ``accounts_configured`` so the two cannot drift.
        """
        missing: list[str] = []
        if not self.base_url:
            missing.append(self.base_env or "upstream_base")
        if not self.account_key and not self.allow_empty_key:
            missing.append(self.account_env or "account_key")
        return tuple(missing)

    @property
    def account_config(self) -> dict[str, object]:
        required = [value for value in (self.base_env, self.account_env) if value]
        return {
            "configured": self.accounts_configured,
            "required_env": required,
            "missing_env": list(self.account_config_missing),
        }

    async def list_models(self) -> list[dict[str, Any]]:
        return await self.model_reader(self.base_url, self.model_key)

    @property
    def runtime(self) -> Any | None:
        """Native data-plane adapter; absent on compatibility test fakes."""

        return self.upstream_adapter

    async def list_accounts(self) -> list[dict[str, Any]]:
        return await self.account_reader(self.base_url, self.account_key)

    @property
    def legacy_configured(self) -> bool:
        """Whether compatibility readers are explicitly allowed to run."""

        return bool(self.legacy_bridge_enabled and self.accounts_configured)


# M1 name used by the target architecture.  AdapterSpec remains the concrete
# compatibility type consumed by the existing gateway and admin code.
RegisteredChannel = AdapterSpec


_REGISTRY_CACHE: dict[tuple[str, ...], dict[str, AdapterSpec]] = {}


def _registry_key(settings: Settings) -> tuple[str, ...]:
    """Return the configuration identity used to keep provision sessions stable."""

    def secret(value: Any) -> str:
        return value.get_secret_value() if hasattr(value, "get_secret_value") else str(value)

    return (
        settings.db_path,
        secret(getattr(settings, "credential_master_key", "")),
        str(bool(getattr(settings, "legacy_bridge_enabled", False))),
        str(getattr(settings, "wb_platform_base", "https://copilot.tencent.com")),
        secret(getattr(settings, "wb_platform_data_key", "")),
        str(getattr(settings, "doubao_platform_base", "https://www.doubao.com")),
        str(getattr(settings, "chatgpt_platform_base", "https://auth.openai.com")),
        str(getattr(settings, "chatgpt_proxy", "")),
        str(getattr(settings, "legacy_wb_upstream_base", "")),
        secret(getattr(settings, "legacy_wb_data_key", "")),
        secret(getattr(settings, "legacy_wb_admin_token", "")),
        str(getattr(settings, "legacy_doubao_upstream_base", "")),
        secret(getattr(settings, "legacy_doubao_api_key", "")),
        str(getattr(settings, "legacy_chatgpt_upstream_base", "")),
        secret(getattr(settings, "legacy_chatgpt_auth_key", "")),
        settings.wb_upstream_base,
        secret(settings.wb_data_key),
        secret(settings.wb_admin_token),
        settings.doubao_upstream_base,
        secret(settings.doubao_api_key),
        str(bool(getattr(settings, "doubao_public_data_plane", False))),
        str(getattr(settings, "doubao_profile_root", "./data/doubao/profiles")),
        str(bool(getattr(settings, "doubao_browser_enabled", False))),
        str(getattr(settings, "doubao_browser_executable", "")),
        str(bool(getattr(settings, "doubao_browser_headless", True))),
        str(getattr(settings, "doubao_browser_login_path", "/")),
        str(getattr(settings, "doubao_browser_max_contexts", 4)),
        str(getattr(settings, "doubao_browser_max_pages_per_context", 2)),
        str(getattr(settings, "doubao_browser_operation_timeout_seconds", 30.0)),
        str(getattr(settings, "doubao_browser_launch_timeout_seconds", 30.0)),
        str(getattr(settings, "doubao_browser_session_ttl_seconds", 300.0)),
        str(getattr(settings, "doubao_browser_qr_selector", "")),
        str(getattr(settings, "doubao_browser_qr_code_attribute", "")),
        str(getattr(settings, "doubao_browser_authenticated_selector", "")),
        str(getattr(settings, "provision_session_ttl_seconds", 600)),
        settings.chatgpt_upstream_base,
        secret(settings.chatgpt_auth_key),
    )


def get_registry(settings: Settings | None = None) -> dict[str, AdapterSpec]:
    """Return a stable registry for the current configuration.

    Native provisioners own in-memory OAuth/QR sessions, so rebuilding them for
    every request would make poll/complete unable to find the session created by
    start.  Cache by non-secret configuration identity; tests may still pass an
    alternate Settings instance and receive an isolated registry.
    """

    settings = settings or get_settings()
    cache_key = _registry_key(settings)
    cached = _REGISTRY_CACHE.get(cache_key)
    if cached is not None:
        return cached
    registry = _build_registry(settings)
    _REGISTRY_CACHE[cache_key] = registry
    return registry


def _build_registry(settings: Settings) -> dict[str, AdapterSpec]:
    credential_store = DatabaseCredentialStore(
        settings.db_path,
        getattr(settings, "credential_master_key", ""),
    )
    provision_state_store = ProvisionStateStore(
        settings.db_path,
        getattr(settings, "credential_master_key", ""),
    )

    def secret_value(primary: str, fallback: str) -> str:
        value = getattr(settings, primary, None)
        if value is not None:
            resolved = (
                value.get_secret_value()
                if hasattr(value, "get_secret_value")
                else str(value)
            )
            if resolved:
                return resolved
        value = getattr(settings, fallback, "")
        return (
            value.get_secret_value()
            if hasattr(value, "get_secret_value")
            else str(value or "")
        )

    def text_value(primary: str, fallback: str) -> str:
        value = str(getattr(settings, primary, "") or "").strip()
        return value or str(getattr(settings, fallback, "") or "").strip()

    legacy_enabled = bool(getattr(settings, "legacy_bridge_enabled", False))
    wb_legacy_base = text_value("legacy_wb_upstream_base", "wb_upstream_base")
    doubao_legacy_base = text_value("legacy_doubao_upstream_base", "doubao_upstream_base")
    chatgpt_legacy_base = text_value("legacy_chatgpt_upstream_base", "chatgpt_upstream_base")
    wb_legacy_data_key = secret_value("legacy_wb_data_key", "wb_data_key")
    wb_legacy_admin_token = secret_value("legacy_wb_admin_token", "wb_admin_token")
    doubao_legacy_key = secret_value("legacy_doubao_api_key", "doubao_api_key")
    chatgpt_legacy_key = secret_value("legacy_chatgpt_auth_key", "chatgpt_auth_key")

    wb_platform_base = str(
        getattr(settings, "wb_platform_base", "https://copilot.tencent.com")
        or "https://copilot.tencent.com"
    ).rstrip("/")
    wb_platform_key = secret_value("wb_platform_data_key", "wb_data_key")
    chatgpt_platform_base = str(
        getattr(settings, "chatgpt_platform_base", "https://auth.openai.com")
        or "https://auth.openai.com"
    ).rstrip("/")
    chatgpt_platform_key = chatgpt_legacy_key

    # Native model readers are closures over the in-process clients.  They do
    # not import the old wb.py/chatgpt.py bridge modules and therefore cannot
    # accidentally read a source project's environment or storage.
    wb_client = WorkBuddyClient(wb_platform_base, data_key=wb_platform_key)
    chatgpt_oauth_client = OAuthClient(
        OAuthConfig(
            authorize_endpoint=(
                str(getattr(settings, "chatgpt_platform_base", "https://auth.openai.com") or "https://auth.openai.com").rstrip("/")
                + "/oauth/authorize"
            ),
            token_endpoint=(
                str(getattr(settings, "chatgpt_platform_base", "https://auth.openai.com") or "https://auth.openai.com").rstrip("/")
                + "/oauth/token"
            ),
        )
    )
    chatgpt_provisioner = ChatGPTProvisioner(
        credential_store=credential_store,
        session_ttl=int(getattr(settings, "provision_session_ttl_seconds", 600)),
        state_store=provision_state_store,
        oauth_client=chatgpt_oauth_client,
    )
    chatgpt_client = ChatGPTAdapter(
        credential_store=credential_store,
        web_base_url="https://chatgpt.com",
        proxy_url=str(getattr(settings, "chatgpt_proxy", "") or ""),
        refresh_callback=chatgpt_provisioner.refresh_credential,
    )

    async def native_wb_models(_base_url: str, _key: str) -> list[dict[str, Any]]:
        groups: list[list[dict[str, Any]]] = []
        with database(settings.db_path) as conn:
            rows = conn.execute(
                "SELECT native_id FROM accounts WHERE channel = 'wb' AND enabled = 1 "
                "ORDER BY priority DESC, updated_at DESC"
            ).fetchall()
        async def read_one(row: Any) -> list[dict[str, Any]]:
            native_id = str(row["native_id"])
            realm = native_id.split(":", 1)[0] if ":" in native_id else "cn"
            credentials = await credential_store.read("wb", native_id)
            if not credentials:
                return []
            try:
                values = await wb_client.list_models(realm=realm, credentials=credentials)
            except Exception:
                return []
            return [dict(item) for item in values]

        groups = [
            group
            for group in await asyncio.gather(*(read_one(row) for row in rows))
            if group
        ]
        if not groups:
            if rows and not wb_platform_key:
                raise RuntimeError("no WorkBuddy account returned a model catalogue")
            values = await wb_client.list_models()
            groups.append([dict(item) for item in values])
        return _merge_models(groups)

    async def native_chatgpt_models(_base_url: str, _key: str) -> list[dict[str, Any]]:
        groups: list[list[dict[str, Any]]] = []
        with database(settings.db_path) as conn:
            rows = conn.execute(
                "SELECT native_id FROM accounts WHERE channel = 'chatgpt' AND enabled = 1 "
                "ORDER BY priority DESC, updated_at DESC"
            ).fetchall()
        async def read_one(row: Any) -> list[dict[str, Any]]:
            credentials = await credential_store.read("chatgpt", str(row["native_id"]))
            if not credentials:
                return []
            try:
                values = await chatgpt_client.list_models({"credentials": credentials})
            except Exception:
                return []
            return [dict(item) for item in values]

        groups = [
            group
            for group in await asyncio.gather(*(read_one(row) for row in rows))
            if group
        ]
        if not groups and chatgpt_platform_key:
            values = await chatgpt_client.list_models(
                {"base_url": chatgpt_platform_base, "auth_key": chatgpt_platform_key}
            )
            groups.append([dict(item) for item in values])
        if rows and not groups:
            raise RuntimeError("no ChatGPT account returned a model catalogue")
        return _merge_models(groups)

    wb_runtime = NativeHttpAdapter(
        WORKBUDDY_MANIFEST,
        wb_platform_base,
        auth_key=wb_platform_key,
        chat_path=WorkBuddyClient.CHAT_PATH,
        credential_store=credential_store,
        channel="wb",
        base_url_resolver=wb_client.base_url_for_credentials,
        credential_headers_resolver=wb_client.runtime_headers,
    )
    chatgpt_runtime = chatgpt_client
    doubao_platform_base = str(
        getattr(settings, "doubao_platform_base", "https://www.doubao.com")
        or "https://www.doubao.com"
    ).rstrip("/")
    doubao_platform_key = secret_value("doubao_platform_data_key", "doubao_api_key")
    doubao_native_enabled = bool(
        getattr(settings, "doubao_public_data_plane", False) or doubao_platform_key
    )
    doubao_provisioner = doubao.DoubaoProvisioner(
        profile_root=getattr(settings, "doubao_profile_root", "./data/doubao/profiles"),
        # Direct HTTP QR login is the default. Playwright remains an optional
        # compatibility worker when explicitly enabled for browser-only flows.
        browser_worker=(
            doubao.browser_worker_from_settings(settings)
            if bool(getattr(settings, "doubao_browser_enabled", False))
            else NativeDoubaoQrWorker.from_settings(settings)
        ),
        credential_store=credential_store,
        session_ttl_seconds=float(
            getattr(settings, "doubao_browser_session_ttl_seconds", 300.0)
        ),
        state_store=provision_state_store,
    )
    doubao_adapter = doubao.DoubaoAdapter(
        provisioner=doubao_provisioner,
        # The native runtime must remain available for accounts provisioned
        # through QR login even when no public/platform key is configured.
        # ``native_model_configured`` below still controls public catalogue
        # discovery and channel configuration reporting.
        base_url=doubao_platform_base,
        auth_key=doubao_platform_key,
        credential_store=credential_store,
    )

    async def native_doubao_models(_base_url: str, _key: str) -> list[dict[str, Any]]:
        with database(settings.db_path) as conn:
            rows = conn.execute(
                "SELECT native_id FROM accounts WHERE channel = 'doubao' "
                "AND enabled = 1 ORDER BY priority DESC, updated_at DESC"
            ).fetchall()
        groups: list[list[dict[str, Any]]] = []
        async def read_one(row: Any) -> list[dict[str, Any]]:
            try:
                values = await doubao_adapter.list_models({"native_id": str(row["native_id"])})
            except Exception:
                return []
            return [dict(item) for item in values]

        groups = [
            group
            for group in await asyncio.gather(*(read_one(row) for row in rows))
            if group
        ]
        if not groups:
            if rows:
                raise RuntimeError("no Doubao account returned a model catalogue")
            groups.append([dict(item) for item in await doubao_adapter.list_models()])
        return _merge_models(groups)

    return {
        "wb": AdapterSpec(
            slug="wb",
            name="WorkBuddy",
            adapter="wb",
            base_url=wb_legacy_base.rstrip("/"),
            model_key=wb_legacy_data_key,
            account_key=wb_legacy_admin_token,
            model_reader=native_wb_models,
            account_reader=_empty_accounts,
            platform_base_url=wb_platform_base,
            legacy_bridge_enabled=legacy_enabled,
            base_env="A2A_WB_UPSTREAM_BASE",
            model_env="A2A_WB_DATA_KEY",
            account_env="A2A_WB_ADMIN_TOKEN",
            protocols=("openai", "anthropic", "responses"),
            caps=("chat",),
            manifest=WORKBUDDY_MANIFEST,
            upstream_adapter=wb_runtime,
            native_model_configured=bool(wb_platform_key),
            provisioner=WorkBuddyProvisioner.from_settings(
                settings,
                credential_store=credential_store,
                state_store=provision_state_store,
                ttl_seconds=int(getattr(settings, "provision_session_ttl_seconds", 600)),
            ),
        ),
        "doubao": AdapterSpec(
            slug="doubao",
            name="Doubao",
            adapter="doubao",
            base_url=doubao_legacy_base.rstrip("/"),
            model_key=doubao_legacy_key,
            account_key=doubao_legacy_key,
            model_reader=native_doubao_models,
            account_reader=_empty_accounts,
            platform_base_url=doubao_platform_base,
            legacy_bridge_enabled=legacy_enabled,
            base_env="A2A_DOUBAO_UPSTREAM_BASE",
            model_env="A2A_DOUBAO_API_KEY",
            account_env="A2A_DOUBAO_API_KEY",
            allow_empty_key=settings.doubao_public_data_plane,
            protocols=("openai", "anthropic", "responses"),
            caps=("chat", "image", "video", "audio", "file"),
            manifest=doubao.build_manifest(),
            provisioner=doubao_provisioner,
            upstream_adapter=doubao_adapter,
            native_model_configured=doubao_native_enabled,
        ),
        "chatgpt": AdapterSpec(
            slug="chatgpt",
            name="ChatGPT",
            adapter="chatgpt",
            base_url=chatgpt_legacy_base.rstrip("/"),
            model_key=chatgpt_legacy_key,
            account_key=chatgpt_legacy_key,
            model_reader=native_chatgpt_models,
            account_reader=_empty_accounts,
            platform_base_url=chatgpt_platform_base,
            legacy_bridge_enabled=legacy_enabled,
            base_env="A2A_CHATGPT_UPSTREAM_BASE",
            model_env="A2A_CHATGPT_AUTH_KEY",
            account_env="A2A_CHATGPT_AUTH_KEY",
            protocols=("openai", "anthropic", "responses"),
            caps=("chat", "image", "search", "ppt", "psd"),
            # ChatGPT onboarding is implemented in-process.  Keep the legacy
            # manifest fields below during the compatibility migration, but
            # use the canonical package manifest and CredentialStore port for
            # all target provision endpoints.
            manifest=CHATGPT_MANIFEST,
            upstream_adapter=chatgpt_runtime,
            native_model_configured=bool(chatgpt_platform_key),
            provisioner=chatgpt_provisioner,
        ),
    }
