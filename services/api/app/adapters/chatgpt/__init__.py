"""Built-in ChatGPT adapter package.

The reader functions are re-exported for compatibility with the existing
registry while provisioning is handled entirely by :class:`ChatGPTProvisioner`.
"""

from .adapter import ChatGPTAdapter, list_accounts, list_models
from .errors import ChatGPTError, ErrorKind, map_error
from .manifest import CHATGPT_MANIFEST, get_manifest, manifest
from .provisioner import ChatGPTProvisioner, Provisioner

__all__ = [
    "CHATGPT_MANIFEST",
    "ChatGPTAdapter",
    "ChatGPTError",
    "ChatGPTProvisioner",
    "ErrorKind",
    "Provisioner",
    "get_manifest",
    "list_accounts",
    "list_models",
    "manifest",
    "map_error",
]
