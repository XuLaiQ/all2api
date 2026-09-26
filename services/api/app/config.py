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
    wb_upstream_base: str = "http://127.0.0.1:7864"
    wb_admin_token: SecretStr = SecretStr("")
    wb_data_key: SecretStr = SecretStr("")
    wb_trace_secret: SecretStr = SecretStr("")
    doubao_upstream_base: str = "http://127.0.0.1:9090"
    doubao_api_key: SecretStr = SecretStr("")
    doubao_public_data_plane: bool = False
    chatgpt_upstream_base: str = "http://127.0.0.1:8000"
    chatgpt_auth_key: SecretStr = SecretStr("")
    session_days: int = 1
    session_idle_hours: int = 12
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
