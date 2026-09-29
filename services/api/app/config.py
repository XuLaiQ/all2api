from functools import lru_cache
from pathlib import Path

from pydantic import Field, SecretStr, model_validator
from pydantic_settings import BaseSettings, SettingsConfigDict

API_DIR = Path(__file__).resolve().parents[1]


class Settings(BaseSettings):
    model_config = SettingsConfigDict(
        env_prefix="A2A_",
        env_file=API_DIR / ".env",
        env_file_encoding="utf-8",
        extra="ignore",
    )

    host: str = "0.0.0.0"
    port: int = 8080
    base_path: str = ""
    db_path: str = "./data/all2api.db"
    # Fernet key (or arbitrary secret from which a development key is derived)
    # used exclusively for the local encrypted credential store.
    credential_master_key: SecretStr = SecretStr("")
    state_path: str = "./data/gateway/state.json"
    log_retention_days: int = Field(default=30, ge=1, le=36500)
    usage_retention_days: int = Field(default=365, ge=1, le=36500)
    session_secret: SecretStr = SecretStr("dev-only-change-me")
    admin_username: str = "admin"
    admin_password: SecretStr | None = None
    allow_weak_admin_password: bool = False
    bootstrap_api_key: SecretStr | None = None
    bootstrap_rpm: int = 60
    admin_token: SecretStr = SecretStr("")
    # Native channel endpoints.  These values describe the provider platform
    # contacted by in-process adapters and must never point at a source
    # project's management port.
    wb_platform_base: str = "https://copilot.tencent.com"
    wb_platform_data_key: SecretStr = SecretStr("")
    doubao_platform_base: str = "https://www.doubao.com"
    chatgpt_platform_base: str = "https://auth.openai.com"
    chatgpt_proxy: str = ""

    # Migration-only HTTP bridge.  The bridge is disabled by default and is
    # kept separate from the native platform configuration above.  The older
    # ``*_upstream_*`` fields remain as compatibility aliases for existing
    # deployments; new deployments should use the explicit legacy group.
    legacy_bridge_enabled: bool = False
    legacy_wb_upstream_base: str = ""
    legacy_wb_admin_token: SecretStr = SecretStr("")
    legacy_wb_data_key: SecretStr = SecretStr("")
    legacy_doubao_upstream_base: str = ""
    legacy_doubao_api_key: SecretStr = SecretStr("")
    legacy_chatgpt_upstream_base: str = ""
    legacy_chatgpt_auth_key: SecretStr = SecretStr("")

    # Deprecated bridge aliases.  They are deliberately not consumed by
    # native provisioners; remove them after the migration profile is retired.
    wb_upstream_base: str = ""
    wb_admin_token: SecretStr = SecretStr("")
    wb_data_key: SecretStr = SecretStr("")
    wb_trace_secret: SecretStr = SecretStr("")
    doubao_upstream_base: str = ""
    doubao_api_key: SecretStr = SecretStr("")
    doubao_public_data_plane: bool = False
    # Profile root owned by this service; it must not point at a source
    # project's data directory.
    doubao_profile_root: str = "./data/doubao/profiles"
    # Optional native Playwright worker. Chromium is never imported or started
    # unless this flag is explicitly enabled in the deployment environment.
    doubao_browser_enabled: bool = False
    doubao_browser_executable: str = ""
    doubao_browser_headless: bool = True
    doubao_browser_login_path: str = "/"
    doubao_browser_max_contexts: int = Field(default=4, ge=1, le=128)
    doubao_browser_max_pages_per_context: int = Field(default=2, ge=1, le=32)
    doubao_browser_operation_timeout_seconds: float = Field(default=30.0, gt=0, le=300)
    doubao_browser_launch_timeout_seconds: float = Field(default=30.0, gt=0, le=300)
    doubao_browser_session_ttl_seconds: float = Field(default=300.0, gt=0, le=86400)
    doubao_browser_qr_selector: str = 'img[src*="qr"], canvas[data-qr]'
    doubao_browser_qr_code_attribute: str = "data-code"
    doubao_browser_authenticated_selector: str = (
        '[data-testid="user-avatar"], [data-authenticated="true"]'
    )
    chatgpt_upstream_base: str = ""
    chatgpt_auth_key: SecretStr = SecretStr("")
    session_days: int = 1
    session_idle_hours: int = 12
    # Native account provision sessions (QR/OAuth) are durable in SQLite and
    # remain replayable until this provider-state TTL elapses.
    provision_session_ttl_seconds: int = Field(default=600, ge=1, le=86400)
    secure_cookie: str = "auto"
    trust_proxy: bool = True
    trusted_proxies: str = "127.0.0.1,::1"
    login_max_fails: int = 5
    cors_origins: str = "http://localhost:5173,http://127.0.0.1:5173"

    @model_validator(mode="after")
    def validate_retention_windows(self) -> "Settings":
        if self.usage_retention_days < self.log_retention_days:
            raise ValueError("A2A_USAGE_RETENTION_DAYS must be >= A2A_LOG_RETENTION_DAYS")
        return self

    @property
    def cors_origin_list(self) -> list[str]:
        return [origin.strip() for origin in self.cors_origins.split(",") if origin.strip()]


@lru_cache
def get_settings() -> Settings:
    return Settings()
