"""Declarative metadata for the built-in Doubao channel."""

from __future__ import annotations

from app.domain.channel import ChannelManifest, ProvisionFlowSpec


def build_manifest(adapter_version: str = "0.2.0") -> ChannelManifest:
    """Return the public manifest without runtime settings or secrets."""

    return ChannelManifest(
        slug="doubao",
        display_name="Doubao",
        adapter_version=adapter_version,
        protocols=("openai", "anthropic", "responses"),
        capabilities=("chat",),  # Only verified capabilities; multimedia planned for P1
        account_flows=(
            ProvisionFlowSpec(
                id="create-profile",
                kind="manual",
                schema={
                    "type": "object",
                    "properties": {
                        "account_id": {
                            "type": "string",
                            "minLength": 1,
                            "maxLength": 64,
                            "pattern": r"^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$",
                        },
                        "name": {"type": "string", "maxLength": 128},
                        "priority": {"type": "integer"},
                        "enabled": {"type": "boolean"},
                    },
                    "required": ["account_id"],
                    "additionalProperties": False,
                },
                supports={"start": True, "cancel": True},
                timeout_seconds=300,
            ),
            ProvisionFlowSpec(
                id="qr-login",
                kind="qr",
                schema={
                    "type": "object",
                    "properties": {
                        "account_id": {
                            "type": "string",
                            "minLength": 1,
                            "maxLength": 64,
                            "pattern": r"^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$",
                        },
                        "name": {"type": "string", "maxLength": 128},
                        "priority": {"type": "integer"},
                        "enabled": {"type": "boolean", "default": True},
                    },
                    "required": ["account_id"],
                    "additionalProperties": False,
                },
                supports={"start": True, "poll": True, "complete": True, "cancel": True},
                timeout_seconds=300,
            ),
        ),
        config_schema_version=2,
        config_schema={
            "type": "object",
            "properties": {
                "platform_base": {"type": "string", "format": "uri"},
                "browser": {
                    "type": "object",
                    "properties": {
                        "enabled": {"type": "boolean", "default": False},
                        "executable": {"type": "string"},
                        "headless": {"type": "boolean", "default": True},
                        "login_path": {"type": "string", "default": "/"},
                        "max_contexts": {"type": "integer", "minimum": 1, "default": 4},
                        "max_pages_per_context": {
                            "type": "integer",
                            "minimum": 1,
                            "default": 2,
                        },
                        "operation_timeout_seconds": {
                            "type": "number",
                            "exclusiveMinimum": 0,
                            "default": 30,
                        },
                        "launch_timeout_seconds": {
                            "type": "number",
                            "exclusiveMinimum": 0,
                            "default": 30,
                        },
                        "session_ttl_seconds": {
                            "type": "number",
                            "exclusiveMinimum": 0,
                            "default": 300,
                        },
                    },
                    "additionalProperties": False,
                },
            },
            "additionalProperties": False,
        },
        health_checks=("browser_worker", "credential_store"),
        limits={
            "browser": {
                "max_contexts": 4,
                "max_pages_per_context": 2,
                "restart_on_disconnect": True,
            }
        },
    )


DOUBAO_MANIFEST = build_manifest()

