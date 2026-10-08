"""ChatGPT Web channel manifest and account flow declarations."""

from __future__ import annotations

from app.domain.channel import ChannelManifest, ProvisionFlowSpec

CHATGPT_WEB_BASE_URL = "https://chatgpt.com"

CHATGPT_MANIFEST = ChannelManifest(
    slug="chatgpt",
    display_name="ChatGPT",
    adapter_version="1.0.0",
    protocols=("openai", "anthropic", "responses"),
    # The concrete chat/image model records are supplied by the authenticated
    # provider catalogue at runtime; the manifest only declares capability
    # families and never invents model names.
    capabilities=("chat", "image"),
    health_checks=("backend-api/me", "backend-api/conversation/init", "backend-api/accounts/check"),
    config_schema={
        "type": "object",
        "properties": {
            "base_url": {"type": "string", "const": CHATGPT_WEB_BASE_URL},
        },
        "additionalProperties": False,
    },
    account_flows=(
        ProvisionFlowSpec(
            id="token-import",
            kind="token_import",
            schema={
                "type": "object",
                "properties": {
                    "tokens": {"type": "array"},
                    "accounts": {"type": "array"},
                },
                # Third-party exports carry envelope fields such as type,
                # version, exported_at and proxies. The provisioner only
                # consumes tokens/accounts and ignores those metadata fields.
                "additionalProperties": True,
            },
            supports={"import": True},
            timeout_seconds=300,
        ),
        ProvisionFlowSpec(
            id="oauth-pkce",
            kind="oauth",
            schema={
                "type": "object",
                "properties": {
                    "email_hint": {"type": "string", "maxLength": 256},
                    "callback": {"type": "string", "maxLength": 4096},
                },
                "additionalProperties": False,
            },
            supports={"start": True, "poll": True, "complete": True, "cancel": True},
            timeout_seconds=600,
        ),
    ),
)


def get_manifest() -> ChannelManifest:
    """Return the immutable manifest shared by registry and adapter tests."""

    return CHATGPT_MANIFEST


manifest = CHATGPT_MANIFEST
