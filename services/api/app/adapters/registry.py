from __future__ import annotations

from collections.abc import Awaitable, Callable
from dataclasses import dataclass, field
from typing import Any

from app.adapters import chatgpt, doubao, wb
from app.config import Settings, get_settings

ModelReader = Callable[[str, str], Awaitable[list[dict[str, Any]]]]
AccountReader = Callable[[str, str], Awaitable[list[dict[str, Any]]]]


async def _wb_models(_base_url: str, _key: str) -> list[dict[str, Any]]:
    return await wb.list_models()


async def _wb_accounts(_base_url: str, _key: str) -> list[dict[str, Any]]:
    return await wb.list_accounts()


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
    base_env: str = field(default="", repr=False)
    model_env: str = field(default="", repr=False)
    account_env: str = field(default="", repr=False)
    allow_empty_key: bool = False
    protocols: tuple[str, ...] = ("openai",)
    caps: tuple[str, ...] = ("chat",)

    @property
    def models_configured(self) -> bool:
        return bool(self.base_url and (self.model_key or self.allow_empty_key))

    @property
    def accounts_configured(self) -> bool:
        return not self.account_config_missing

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

    async def list_accounts(self) -> list[dict[str, Any]]:
        return await self.account_reader(self.base_url, self.account_key)


def get_registry(settings: Settings | None = None) -> dict[str, AdapterSpec]:
    settings = settings or get_settings()
    return {
        "wb": AdapterSpec(
            slug="wb",
            name="WorkBuddy",
            adapter="wb",
            base_url=settings.wb_upstream_base.rstrip("/"),
            model_key=settings.wb_data_key.get_secret_value(),
            account_key=settings.wb_admin_token.get_secret_value(),
            model_reader=_wb_models,
            account_reader=_wb_accounts,
            base_env="A2A_WB_UPSTREAM_BASE",
            model_env="A2A_WB_DATA_KEY",
            account_env="A2A_WB_ADMIN_TOKEN",
            protocols=("openai", "anthropic", "responses"),
            caps=("chat",),
        ),
        "doubao": AdapterSpec(
            slug="doubao",
            name="Doubao",
            adapter="doubao",
            base_url=settings.doubao_upstream_base.rstrip("/"),
            model_key=settings.doubao_api_key.get_secret_value(),
            account_key=settings.doubao_api_key.get_secret_value(),
            model_reader=doubao.list_models,
            account_reader=doubao.list_accounts,
            base_env="A2A_DOUBAO_UPSTREAM_BASE",
            model_env="A2A_DOUBAO_API_KEY",
            account_env="A2A_DOUBAO_API_KEY",
            allow_empty_key=settings.doubao_public_data_plane,
            protocols=("openai", "anthropic", "responses"),
            caps=("chat", "image", "video", "audio", "file"),
        ),
        "chatgpt": AdapterSpec(
            slug="chatgpt",
            name="ChatGPT",
            adapter="chatgpt",
            base_url=settings.chatgpt_upstream_base.rstrip("/"),
            model_key=settings.chatgpt_auth_key.get_secret_value(),
            account_key=settings.chatgpt_auth_key.get_secret_value(),
            model_reader=chatgpt.list_models,
            account_reader=chatgpt.list_accounts,
            base_env="A2A_CHATGPT_UPSTREAM_BASE",
            model_env="A2A_CHATGPT_AUTH_KEY",
            account_env="A2A_CHATGPT_AUTH_KEY",
            protocols=("openai", "anthropic", "responses"),
            caps=("chat", "image", "search", "ppt", "psd"),
        ),
    }
