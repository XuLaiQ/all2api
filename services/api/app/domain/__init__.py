"""Pure domain objects shared by the application and adapter layers.

The domain package intentionally has no dependency on FastAPI, httpx, SQLite,
or any platform specific client.  Adapter implementations may add richer
behaviour, but the objects exposed here are the stable contract used by the
management API and the future schema renderer.
"""

from app.domain.channel import ChannelManifest, ProvisionFlowSpec

__all__ = ["ChannelManifest", "ProvisionFlowSpec"]
