#!/usr/bin/env python3
"""Verify that every frozen migration route has a Go HTTP path registration.

This is a path-coverage gate, not a method/behavior equivalence test. The Go
HTTP contract tests remain authoritative for methods, authorization and status
codes. The script intentionally reads only the frozen route fixture and the Go
ServeMux source; it does not start a service or read credentials.
"""

from __future__ import annotations

import json
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
BASELINE = ROOT / "docs" / "migration-baseline" / "routes.json"
SERVER = ROOT / "internal" / "transport" / "http" / "server.go"

# These paths are registered through a Go slice loop rather than an individual
# HandleFunc call, so include the loop's explicit members in the source gate.
DYNAMIC_PATHS = {
    "/v1/images/edits",
    "/v1/audio/generations",
    "/v1/files/download",
    "/v1/ppt/generations",
    "/v1/psd/generations",
    "/v1/editable-file-tasks",
    "/v1/messages/count_tokens",
}


def normalize(path: str) -> str:
    return re.sub(r"\{[^}]+\}", "{param}", path)


def main() -> int:
    baseline = json.loads(BASELINE.read_text(encoding="utf-8"))
    source = SERVER.read_text(encoding="utf-8")
    registered = set(re.findall(r'HandleFunc\("([^"]+)"', source))
    registered.update(DYNAMIC_PATHS)
    registered_normalized = {normalize(path) for path in registered}
    baseline_paths = {str(item["path"]) for item in baseline}
    missing = sorted(path for path in baseline_paths if normalize(path) not in registered_normalized)
    if missing:
        print("FAIL: frozen routes are missing from the Go ServeMux:")
        print("\n".join(f"- {path}" for path in missing))
        return 1
    print(
        "PASS: Go route path coverage "
        f"({len(baseline_paths)} baseline paths, {len(registered)} registered patterns)"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
