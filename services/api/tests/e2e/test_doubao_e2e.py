"""Doubao E2E tests backed by a real, operator-provided browser profile.

The source profile is never opened by Chromium. The fixture copies its
Chromium user-data directory to a temporary test directory, and all browser
cleanup is performed before that directory is removed.
"""

from __future__ import annotations

import asyncio
import json
import os
import re
import shutil
import stat
import time
from collections.abc import Iterator
from pathlib import Path
from typing import Any

import pytest

from app.adapters.doubao.adapter import DoubaoAdapter
from app.adapters.doubao.browser import BrowserWorkerConfig, PlaywrightBrowserWorker
from app.adapters.doubao.credentials import NonRetainingCredentialStore
from app.adapters.doubao.errors import BrowserWorkerError, BrowserWorkerUnavailableError

_ACCOUNT_ID_PATTERN = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$")
_DEFAULT_AUTH_SELECTOR = '[data-testid="user-avatar"], [data-authenticated="true"]'
_LOCK_NAMES = {"SingletonCookie", "SingletonLock", "SingletonSocket"}


def _env_flag(name: str, default: bool = False) -> bool:
    value = os.getenv(name)
    if value is None:
        return default
    return value.strip().lower() in {"1", "true", "yes", "on"}


def _remove_tree(path: Path) -> None:
    """Remove a copied Chromium profile, including read-only Windows files."""

    def onerror(function: Any, target: str, _exc_info: Any) -> None:
        try:
            os.chmod(target, stat.S_IWRITE)
            function(target)
        except OSError:
            return

    shutil.rmtree(path, ignore_errors=False, onerror=onerror)


def _looks_like_chromium_user_data(path: Path) -> bool:
    """Inspect only directory names; never open profile files."""

    markers = ("Local State", "Default", "Profile 1", "Network")
    if any((path / marker).exists() for marker in markers):
        return True
    if path.name.lower() == "browser":
        try:
            return any(child.is_dir() for child in path.iterdir())
        except OSError:
            return False
    return False


def _find_source_profile(profile_root: Path, account_id: str) -> Path | None:
    root = profile_root.expanduser().resolve()
    account_root = (root / account_id).resolve()
    try:
        account_root.relative_to(root)
    except ValueError:
        return None

    for candidate in (account_root / "browser", account_root):
        if candidate.is_dir() and _looks_like_chromium_user_data(candidate):
            return candidate
    return None


def _copy_profile_without_lock_files(source: Path, target: Path) -> None:
    def ignore(_directory: str, names: list[str]) -> set[str]:
        return {name for name in names if name in _LOCK_NAMES}

    shutil.copytree(source, target, ignore=ignore)


@pytest.fixture
def real_doubao_profile(tmp_path: Path) -> Iterator[dict[str, Any]]:
    """Copy the configured real account profile into an isolated temp root."""

    profile_root_value = os.getenv("DOUBAO_TEST_PROFILE_ROOT", "").strip()
    account_id = os.getenv("DOUBAO_TEST_ACCOUNT_ID", "").strip()
    if not profile_root_value or not account_id:
        pytest.skip(
            "DOUBAO_TEST_PROFILE_ROOT and DOUBAO_TEST_ACCOUNT_ID are required "
            "for real Doubao E2E"
        )
    if not _ACCOUNT_ID_PATTERN.fullmatch(account_id):
        pytest.skip("DOUBAO_TEST_ACCOUNT_ID must be a safe account directory name")

    profile_root = Path(profile_root_value).expanduser()
    if not profile_root.is_dir():
        pytest.skip("DOUBAO_TEST_PROFILE_ROOT does not point to an existing directory")
    source = _find_source_profile(profile_root, account_id)
    if source is None:
        pytest.skip(
            "DOUBAO_TEST_PROFILE_ROOT/DOUBAO_TEST_ACCOUNT_ID does not contain "
            "a Chromium profile"
        )

    copied_root = tmp_path / "doubao-profile"
    copied_account = copied_root / account_id
    copied_browser = copied_account / "browser"
    copied_account.mkdir(parents=True)
    try:
        _copy_profile_without_lock_files(source, copied_browser)
        (copied_account / "meta.json").write_text(
            json.dumps(
                {
                    "id": account_id,
                    "name": account_id,
                    "enabled": True,
                    "priority": 0,
                    "status": "ready",
                },
                separators=(",", ":"),
            )
            + "\n",
            encoding="utf-8",
        )
    except OSError as exc:
        _remove_tree(copied_root)
        pytest.skip(
            "real Doubao profile could not be copied to a temporary directory: "
            f"{type(exc).__name__}"
        )

    try:
        yield {
            "account_id": account_id,
            "source_path": source,
            "profile_root": copied_root,
            "profile_path": copied_browser,
        }
    finally:
        if copied_root.exists():
            _remove_tree(copied_root)


def _build_real_worker() -> PlaywrightBrowserWorker:
    return PlaywrightBrowserWorker(
        BrowserWorkerConfig(
            enabled=True,
            platform_base_url="https://www.doubao.com",
            login_path=os.getenv("DOUBAO_TEST_LOGIN_PATH", "/").strip() or "/",
            executable_path=os.getenv("DOUBAO_TEST_BROWSER_EXECUTABLE", "").strip(),
            headless=_env_flag("DOUBAO_TEST_HEADLESS", True),
            persistent_profile=True,
            authenticated_selector=(
                os.getenv("DOUBAO_TEST_AUTH_SELECTOR", "").strip()
                or _DEFAULT_AUTH_SELECTOR
            ),
        )
    )


async def _start_real_worker_or_skip(worker: PlaywrightBrowserWorker) -> None:
    try:
        await worker.start()
    except BrowserWorkerUnavailableError:
        await worker.stop()
        pytest.skip("Playwright or the Chromium browser executable is not available")
    except BrowserWorkerError as exc:
        await worker.stop()
        pytest.skip(f"real Chromium could not start: {type(exc).__name__}")


def _assert_authenticated_snapshot(
    snapshot: dict[str, Any] | Any,
    *,
    expected_session_id: str,
) -> None:
    assert snapshot["session_id"] == expected_session_id
    assert str(snapshot["url"]).startswith("https://www.doubao.com")
    assert str(snapshot["title"]).strip(), "Doubao page did not expose a document title"
    assert snapshot["authenticated"] is True, (
        "the configured Doubao profile is not authenticated; provide a logged-in "
        "profile or set DOUBAO_TEST_AUTH_SELECTOR"
    )


pytestmark = [
    pytest.mark.e2e,
    pytest.mark.browser,
    pytest.mark.skipif(
        not _env_flag("E2E_DOUBAO_ENABLED"),
        reason="Doubao E2E disabled; set E2E_DOUBAO_ENABLED=true",
    ),
]


class TestDoubaoRealPlatform:
    """Smoke tests for a real copied Doubao Chromium profile."""

    @pytest.mark.asyncio
    async def test_browser_profile_exists(self, real_doubao_profile: dict[str, Any]) -> None:
        source = Path(real_doubao_profile["source_path"])
        copied = Path(real_doubao_profile["profile_path"])
        assert source.is_dir()
        assert copied.is_dir()
        assert source.resolve() != copied.resolve()
        meta_path = (
            Path(real_doubao_profile["profile_root"])
            / real_doubao_profile["account_id"]
            / "meta.json"
        )
        assert meta_path.is_file()

    @pytest.mark.asyncio
    async def test_playwright_installation(
        self, real_doubao_profile: dict[str, Any]
    ) -> None:
        del real_doubao_profile
        try:
            from playwright.async_api import async_playwright
        except ImportError:
            pytest.skip("Playwright is not installed; install the browser optional dependency")

        async with async_playwright() as playwright:
            try:
                browser = await playwright.chromium.launch(headless=True)
            except Exception as exc:
                pytest.skip(f"Chromium is not installed or cannot launch: {type(exc).__name__}")
            try:
                assert browser.is_connected()
            finally:
                await browser.close()

    @pytest.mark.asyncio
    async def test_browser_session_creation(
        self, real_doubao_profile: dict[str, Any]
    ) -> None:
        worker = _build_real_worker()
        await _start_real_worker_or_skip(worker)
        session_id = ""
        try:
            challenge = await worker.start_qr_login(
                real_doubao_profile["account_id"],
                str(real_doubao_profile["profile_path"]),
            )
            session_id = challenge.session_id
            snapshot = await worker.session_snapshot(session_id)
            _assert_authenticated_snapshot(snapshot, expected_session_id=session_id)
        finally:
            if session_id:
                await worker.cancel_qr_login(session_id)
            await worker.stop()


class TestDoubaoAdapterE2E:
    """Adapter and provisioner calls using the real copied profile."""

    @pytest.mark.asyncio
    async def test_adapter_browser_worker_calls(
        self, real_doubao_profile: dict[str, Any]
    ) -> None:
        worker = _build_real_worker()
        adapter = DoubaoAdapter(
            profile_root=real_doubao_profile["profile_root"],
            browser_worker=worker,
            credential_store=NonRetainingCredentialStore(),
        )
        await _start_real_worker_or_skip(worker)
        session_id = ""
        try:
            health = await adapter.health()
            assert health["status"] == "ok"
            assert health["worker"]["worker"] == "playwright"
            normalized = adapter.normalize_account(
                {"account_id": real_doubao_profile["account_id"]}
            )
            assert normalized["native_id"] == real_doubao_profile["account_id"]

            result = await adapter.provisioner.start(
                "qr-login",
                {"account_id": real_doubao_profile["account_id"]},
                "doubao-real-profile-smoke",
            )
            session_id = str(result["session_id"])
            snapshot = await worker.session_snapshot(session_id)
            _assert_authenticated_snapshot(snapshot, expected_session_id=session_id)

            completed = await adapter.provisioner.poll(session_id)
            assert completed["account_id"] == real_doubao_profile["account_id"]
            assert completed["status"] == "succeeded"
            assert completed.get("credential_ref")
            assert "credentials" not in completed
            assert "storage_state" not in completed
        finally:
            if session_id:
                await worker.cancel_qr_login(session_id)
            await adapter.shutdown()

    @pytest.mark.asyncio
    async def test_browser_crash_recovery(
        self, real_doubao_profile: dict[str, Any]
    ) -> None:
        worker = _build_real_worker()
        adapter = DoubaoAdapter(
            profile_root=real_doubao_profile["profile_root"],
            browser_worker=worker,
            credential_store=NonRetainingCredentialStore(),
        )
        await _start_real_worker_or_skip(worker)
        session_id = ""
        try:
            started = await adapter.provisioner.start(
                "qr-login",
                {"account_id": real_doubao_profile["account_id"]},
                "doubao-recovery-smoke",
            )
            session_id = str(started["session_id"])
            snapshot = await worker.session_snapshot(session_id)
            _assert_authenticated_snapshot(snapshot, expected_session_id=session_id)

            await worker.restart()
            recovered = await adapter.provisioner.poll(session_id)
            assert recovered["status"] == "succeeded"
            assert recovered["account_id"] == real_doubao_profile["account_id"]
        finally:
            if session_id:
                await worker.cancel_qr_login(session_id)
            await adapter.shutdown()

    @pytest.mark.asyncio
    @pytest.mark.slow
    @pytest.mark.skipif(
        not _env_flag("DOUBAO_RUN_LONG_STABILITY"),
        reason="24-hour Doubao stability test requires DOUBAO_RUN_LONG_STABILITY=true",
    )
    async def test_browser_stability_24h(
        self, real_doubao_profile: dict[str, Any]
    ) -> None:
        worker = _build_real_worker()
        await _start_real_worker_or_skip(worker)
        session_id = ""
        duration = max(1.0, float(os.getenv("DOUBAO_STABILITY_SECONDS", "86400")))
        interval = max(1.0, float(os.getenv("DOUBAO_STABILITY_INTERVAL_SECONDS", "60")))
        deadline = time.monotonic() + duration
        try:
            challenge = await worker.start_qr_login(
                real_doubao_profile["account_id"],
                str(real_doubao_profile["profile_path"]),
            )
            session_id = challenge.session_id
            while time.monotonic() < deadline:
                health = await worker.health()
                assert health["status"] == "ready"
                snapshot = await worker.session_snapshot(session_id)
                _assert_authenticated_snapshot(snapshot, expected_session_id=session_id)
                await asyncio.sleep(min(interval, max(0.1, deadline - time.monotonic())))
        finally:
            if session_id:
                await worker.cancel_qr_login(session_id)
            await worker.stop()
