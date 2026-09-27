#!/usr/bin/env python3
"""Check local Markdown links without accessing the network."""

from __future__ import annotations

import argparse
import re
import sys
from pathlib import Path
from urllib.parse import unquote


ROOT = Path(__file__).resolve().parents[1]
LINK_RE = re.compile(
    r"(?<!!)\[[^\]]*\]\((?P<target><[^>]+>|[^)\s]+)(?:\s+[^)]*)?\)"
)
SKIP_SCHEMES = ("http://", "https://", "mailto:", "tel:", "data:")


def _markdown_files(inputs: list[str]) -> list[Path]:
    if not inputs:
        inputs = ["README.md", "docs", "services/api/README.md"]
    files: list[Path] = []
    for item in inputs:
        path = (ROOT / item).resolve()
        if path.is_dir():
            files.extend(sorted(path.rglob("*.md")))
        elif path.is_file() and path.suffix.lower() == ".md":
            files.append(path)
        else:
            raise ValueError(f"Markdown path does not exist: {item}")
    return files


def _target_path(source: Path, target: str) -> Path | None:
    target = target.strip()
    if target.startswith("<") and target.endswith(">"):
        target = target[1:-1]
    if not target or target.startswith("#") or target.lower().startswith(SKIP_SCHEMES):
        return None
    target = unquote(target.split("#", 1)[0])
    if not target:
        return None
    if target.startswith("/"):
        return (ROOT / target.lstrip("/"))
    return (source.parent / target).resolve()


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("paths", nargs="*", help="Markdown files or directories")
    args = parser.parse_args()
    try:
        files = _markdown_files(args.paths)
    except ValueError as exc:
        print(f"FAIL {exc}", file=sys.stderr)
        return 1

    failures: list[str] = []
    checked = 0
    for source in files:
        text = source.read_text(encoding="utf-8")
        for match in LINK_RE.finditer(text):
            target = _target_path(source, match.group("target"))
            if target is None:
                continue
            checked += 1
            if not target.exists():
                line = text.count("\n", 0, match.start()) + 1
                failures.append(
                    f"{source.relative_to(ROOT)}:{line} -> {match.group('target')}"
                )

    if failures:
        print("Broken local Markdown links:", file=sys.stderr)
        print("\n".join(f"- {failure}" for failure in failures), file=sys.stderr)
        return 1
    print(f"Checked {len(files)} Markdown files and {checked} local links: PASS")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
