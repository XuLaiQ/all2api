"""Native WorkBuddy channel implementation.

All protocol and provisioning behavior lives in this package.  It does not import,
spawn, or call the legacy WorkBuddy project.
"""

from app.adapters.workbuddy.adapter import WorkBuddyAdapter, create_adapter
from app.adapters.workbuddy.client import WorkBuddyClient
from app.adapters.workbuddy.manifest import WORKBUDDY_MANIFEST
from app.adapters.workbuddy.provisioner import MemoryCredentialStore, WorkBuddyProvisioner

__all__ = [
    "MemoryCredentialStore",
    "WORKBUDDY_MANIFEST",
    "WorkBuddyAdapter",
    "WorkBuddyClient",
    "WorkBuddyProvisioner",
    "create_adapter",
]
