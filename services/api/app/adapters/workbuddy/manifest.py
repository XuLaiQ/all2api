from __future__ import annotations

from app.domain.channel import ChannelManifest, ProvisionFlowSpec

WORKBUDDY_CHANNEL = "wb"
WORKBUDDY_FLOW = "qr-oauth"
WORKBUDDY_IMPORT_FLOW = "token-import"

WORKBUDDY_MANIFEST = ChannelManifest(
    slug=WORKBUDDY_CHANNEL,
    display_name="WorkBuddy",
    adapter_version="1.0.0",
    protocols=("openai", "anthropic", "responses"),
    capabilities=("chat",),
    account_flows=(
        ProvisionFlowSpec(
            id=WORKBUDDY_IMPORT_FLOW,
            kind="token_import",
            schema={
                "type": "object",
                "properties": {"accounts": {"type": "array"}},
                "required": ["accounts"],
                "additionalProperties": False,
            },
            supports={"import": True},
            timeout_seconds=300,
        ),
        ProvisionFlowSpec(
            id=WORKBUDDY_FLOW,
            kind="qr",
            schema={
                "type": "object",
                "properties": {
                    "realm": {"type": "string", "enum": ["cn", "global"]},
                    "region": {"type": "string", "maxLength": 16},
                },
                "additionalProperties": False,
            },
            supports={"start": True, "poll": True, "complete": True, "cancel": True},
            timeout_seconds=300,
        ),
    ),
)


def manifest() -> ChannelManifest:
    """Return the immutable public manifest for the WorkBuddy channel."""

    return WORKBUDDY_MANIFEST
