#!/usr/bin/env python3
"""Verify the Go-only production image boundary.

Exit codes: 0 means verified, 1 means a check failed, and 2 means Docker was
unavailable so the image boundary remains unverified.
"""

from __future__ import annotations

import shutil
import subprocess
import sys


def docker_available() -> bool | None:
    if not shutil.which("docker"):
        print("UNVERIFIED: Docker command is unavailable")
        return None
    try:
        result = subprocess.run(["docker", "info"], capture_output=True, text=True, timeout=5, check=False)
    except (FileNotFoundError, subprocess.TimeoutExpired):
        print("UNVERIFIED: Docker daemon is unavailable")
        return None
    if result.returncode != 0:
        print("UNVERIFIED: Docker daemon is unavailable")
        return None
    return True


def main() -> int:
    print("=" * 60)
    print("Verify Go-only production image boundary")
    print("=" * 60)
    availability = docker_available()
    if availability is None:
        return 2

    image_tag = "all2api-go-legacy-check:test"
    try:
        build = subprocess.run(
            ["docker", "build", "-t", image_tag, "-f", "Dockerfile.golang", "."],
            capture_output=True,
            text=True,
            timeout=600,
            check=False,
        )
        if build.returncode != 0:
            print(f"FAIL: Go-only Docker build failed: {build.stderr[-4000:]}")
            return 1
        verify = subprocess.run(
            [
                "docker", "run", "--rm", "--entrypoint", "/bin/sh", image_tag,
                "-c", "test -x /usr/local/bin/all2api && test -x /usr/local/bin/doubao-browser-worker && test -x /usr/local/bin/media-worker && test ! -e /app/app",
            ],
            capture_output=True,
            text=True,
            timeout=30,
            check=False,
        )
        if verify.returncode != 0:
            print(f"FAIL: Go-only image contents are invalid: {verify.stderr[-4000:]}")
            return 1
        print("PASS: Go-only image contains the expected runtime boundary")
        return 0
    finally:
        subprocess.run(["docker", "rmi", image_tag], capture_output=True, text=True, timeout=60, check=False)


if __name__ == "__main__":
    sys.exit(main())
