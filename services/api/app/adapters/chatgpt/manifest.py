"""ChatGPT channel manifest and account flow declarations."""

from __future__ import annotations

from app.domain.channel import ChannelManifest, ProvisionFlowSpec

CHATGPT_MANIFEST = ChannelManifest(
    slug="chatgpt",
    display_name="ChatGPT",
    adapter_version="0.2.0",
    protocols=("openai", "anthropic", "responses"),
    capabilities=("chat",),  # Only verified capabilities; image/search planned for P1
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
