"""Built-in Doubao channel adapter.

The package owns all native account provisioning and browser lifecycle code.
The deprecated ``doubao_transport`` bridge is intentionally not imported here;
catalogue and data-plane clients must be registered explicitly by the native
composition root.
"""

from .adapter import DoubaoAdapter, browser_worker_from_settings, build_adapter, create_adapter
from .browser import (
    BrowserChallenge,
    BrowserEvent,
    BrowserWorker,
    BrowserWorkerConfig,
    FakeBrowserWorker,
    NullBrowserWorker,
    PlaywrightBrowserWorker,
    build_browser_worker,
)
from .credentials import CredentialStore, MemoryCredentialStore
from .manifest import DOUBAO_MANIFEST, build_manifest
from .mapper import map_account, map_browser_event, map_profile
from .provisioner import DoubaoProfile, DoubaoProfileStore, DoubaoProvisioner

__all__ = [
    "BrowserChallenge",
    "BrowserEvent",
    "BrowserWorker",
    "BrowserWorkerConfig",
    "build_browser_worker",
    "CredentialStore",
    "DOUBAO_MANIFEST",
    "DoubaoAdapter",
    "DoubaoProfile",
    "DoubaoProfileStore",
    "DoubaoProvisioner",
    "FakeBrowserWorker",
    "MemoryCredentialStore",
    "NullBrowserWorker",
    "PlaywrightBrowserWorker",
    "browser_worker_from_settings",
    "build_adapter",
    "create_adapter",
    "build_manifest",
    "map_account",
    "map_browser_event",
    "map_profile",
]
