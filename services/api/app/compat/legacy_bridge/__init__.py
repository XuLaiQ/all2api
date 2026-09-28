"""Legacy bridge code - DO NOT USE FOR NEW DEVELOPMENT."""
import os
import warnings

_ENABLED = os.getenv("A2A_LEGACY_BRIDGE_ENABLED", "false").lower() == "true"

if _ENABLED:
    warnings.warn(
        "Legacy bridge is enabled. This code is deprecated and will be removed in v1.0.0",
        DeprecationWarning,
        stacklevel=2
    )
