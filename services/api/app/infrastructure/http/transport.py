"""Build outbound HTTP clients with explicit proxy semantics.

Provider clients must not inherit a developer workstation's system proxy by
accident.  A proxy is therefore opt-in and owned by the channel composition
root; tests can still inject a pre-built client through the adapter port.
"""

from __future__ import annotations

from typing import Any

import httpx


def build_client(
    *,
    proxy: str = "",
    timeout: float = 300.0,
    connect_timeout: float = 10.0,
    trust_env: bool = False,
    **kwargs: Any,
) -> httpx.AsyncClient:
    """Create an ``httpx`` client with a deliberate environment policy."""

    options: dict[str, Any] = {
        "timeout": httpx.Timeout(float(timeout), connect=float(connect_timeout)),
        "trust_env": bool(trust_env),
    }
    if str(proxy or "").strip():
        options["proxy"] = str(proxy).strip()
    options.update(kwargs)
    return httpx.AsyncClient(**options)


__all__ = ["build_client"]
