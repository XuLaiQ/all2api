#!/usr/bin/env python3
"""Audit locked Go modules without downloading an external scanner.

The check validates module content with ``go mod verify`` and inspects the
local module cache for common license/notice files. It is intentionally not a
legal conclusion and does not replace govulncheck or go-licenses; missing
cache entries are reported as unverified rather than treated as passing.
"""

from __future__ import annotations

import argparse
import json
import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
LICENSE_NAMES = re.compile(r"^(license|copying|notice)(\.[^/]*)?$", re.IGNORECASE)
MODULE_LINE = re.compile(r"^\s*([^\s()]+)\s+(v[^\s]+)\s*(?://.*)?$")


def escape_module_path(value: str) -> str:
    result: list[str] = []
    for char in value:
        if "A" <= char <= "Z":
            result.extend(("!", char.lower()))
        else:
            result.append(char)
    return "".join(result)


def locked_modules() -> list[tuple[str, str]]:
    modules: list[tuple[str, str]] = []
    in_require_block = False
    for raw in (ROOT / "go.mod").read_text(encoding="utf-8").splitlines():
        line = raw.strip()
        if line == "require (":
            in_require_block = True
            continue
        if in_require_block and line == ")":
            in_require_block = False
            continue
        candidate = raw if in_require_block else raw.removeprefix("require ")
        match = MODULE_LINE.match(candidate)
        if match:
            modules.append((match.group(1), match.group(2)))
    return modules


def find_license_files(module_dir: Path) -> list[str]:
    found: list[str] = []
    for path in module_dir.rglob("*"):
        if path.is_file() and LICENSE_NAMES.match(path.name):
            found.append(path.relative_to(module_dir).as_posix())
    return sorted(found)[:20]


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--json", action="store_true", help="emit machine-readable output")
    args = parser.parse_args()

    verify = subprocess.run(
        ["go", "mod", "verify"], cwd=ROOT, capture_output=True, text=True, check=False
    )
    try:
        cache = Path(subprocess.check_output(["go", "env", "GOMODCACHE"], cwd=ROOT, text=True).strip())
    except (OSError, subprocess.CalledProcessError) as exc:
        cache = Path()
        cache_error = str(exc)
    else:
        cache_error = ""

    dependencies: list[dict[str, object]] = []
    for module, version in locked_modules():
        # Go's module cache keeps the module path hierarchy and appends the
        # version to the final path component, e.g. golang.org/x/sys@vX.
        module_dir = cache / (escape_module_path(module) + "@" + version) if cache else Path()
        present = module_dir.is_dir()
        dependencies.append(
            {
                "module": module,
                "version": version,
                "cache_present": present,
                "license_files": find_license_files(module_dir) if present else [],
            }
        )

    cache_complete = bool(dependencies) and all(item["cache_present"] for item in dependencies)
    license_complete = bool(dependencies) and all(item["license_files"] for item in dependencies)
    result = {
        "format": "all2api-go-supply-chain-local-v1",
        "module_verify": {"status": "ok" if verify.returncode == 0 else "failed", "output": verify.stdout.strip()},
        "module_cache": str(cache) if cache else "",
        "cache_error": cache_error,
        "cache_complete": cache_complete,
        "license_files_complete": license_complete,
        "dependencies": dependencies,
        "root_license_declared": any((ROOT / name).is_file() for name in ("LICENSE", "LICENSE.md", "COPYING")),
    }
    if args.json:
        print(json.dumps(result, ensure_ascii=False, indent=2))
    else:
        print(f"module_verify={result['module_verify']['status']}")
        print(f"module_cache_complete={cache_complete}")
        print(f"dependency_license_files_complete={license_complete}")
        print(f"root_license_declared={result['root_license_declared']}")
        for item in dependencies:
            state = "ok" if item["cache_present"] and item["license_files"] else "unverified"
            print(f"{state} {item['module']}@{item['version']}")

    if verify.returncode != 0:
        return 1
    if not cache_complete or not license_complete or not result["root_license_declared"]:
        return 2
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
