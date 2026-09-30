from __future__ import annotations

import json
import re
import threading
import time
from dataclasses import dataclass
from datetime import datetime

from app.config import get_settings
from app.infrastructure.db import database

WORKBUDDY_ACCOUNT_ATTEMPTS = 3
WORKBUDDY_LOCAL_INFLIGHT_LIMIT = 1
_ACCOUNT_ID = re.compile(r"(?:cn|global):[A-Za-z0-9@._-]{1,128}")
_lease_lock = threading.Lock()
_inflight: dict[str, int] = {}
_last_selected: dict[str, int] = {}
_selection_seq = 0


@dataclass(frozen=True)
class AccountCandidate:
    account_id: str
    native_id: str
    channel: str = ""


@dataclass(frozen=True)
class WorkBuddyCandidate(AccountCandidate):
    channel: str = "wb"


@dataclass
class WorkBuddyLease:
    account_id: str
    native_id: str
    _released: bool = False

    def release(self) -> None:
        with _lease_lock:
            if self._released:
                return
            self._released = True
            remaining = _inflight.get(self.account_id, 0) - 1
            if remaining > 0:
                _inflight[self.account_id] = remaining
            else:
                _inflight.pop(self.account_id, None)


def workbuddy_candidates(
    model: str,
    *,
    db_path: str | None = None,
) -> tuple[bool, list[WorkBuddyCandidate]]:
    now = int(time.time())
    prefix, separator, bare_model = model.partition(":")
    requested_realm = prefix if separator and prefix in {"cn", "global"} else None
    model_name = bare_model if requested_realm else model
    with database(db_path or get_settings().db_path) as conn:
        snapshot_exists = (
            conn.execute("SELECT 1 FROM accounts WHERE channel = 'wb' LIMIT 1").fetchone()
            is not None
        )
        rows = conn.execute(
            """SELECT a.id, a.native_id, a.ext
            FROM accounts a LEFT JOIN account_runtime_state r ON r.account_id = a.id
            WHERE a.channel = 'wb' AND a.enabled = 1
                AND COALESCE(a.status_override, a.status) IN (
                    'ready', 'busy', 'cooldown', 'limited'
                )
                AND (r.cooldown_until IS NULL OR r.cooldown_until <= ?)
                AND (r.breaker_until IS NULL OR r.breaker_until <= ?)
            ORDER BY COALESCE(r.updated_at, 0), a.priority DESC, a.name, a.id""",
            (now, now),
        ).fetchall()

    ranked_candidates = []
    with _lease_lock:
        active_ids = {str(row["id"]) for row in rows}
        for account_id in _last_selected.keys() - active_ids:
            _last_selected.pop(account_id, None)
        for order, row in enumerate(rows):
            account_id = str(row["id"])
            native_id = str(row["native_id"])
            if not _ACCOUNT_ID.fullmatch(native_id):
                continue
            # An unqualified public model is realm-neutral.  Prefer the
            # account's stored realm at runtime so a global-only pool can
            # serve the same model IDs as a cn-only pool.  Explicit realm
            # prefixes remain strict and never cross between platforms.
            if requested_realm and not native_id.startswith(f"{requested_realm}:"):
                continue
            if _model_is_limited(str(row["ext"]), model_name, now):
                continue
            if _inflight.get(account_id, 0) >= WORKBUDDY_LOCAL_INFLIGHT_LIMIT:
                continue
            ranked_candidates.append(
                (
                    _last_selected.get(account_id, 0),
                    order,
                    WorkBuddyCandidate(account_id, native_id),
                )
            )
        ranked_candidates.sort(key=lambda item: (item[0], item[1]))
    candidates = [item[2] for item in ranked_candidates[:WORKBUDDY_ACCOUNT_ATTEMPTS]]
    return snapshot_exists, candidates


def account_candidates(
    channel: str,
    model: str,
    *,
    max_candidates: int = WORKBUDDY_ACCOUNT_ATTEMPTS,
    db_path: str | None = None,
    require_credentials: bool = False,
) -> tuple[bool, list[AccountCandidate]]:
    """Select enabled local accounts for any native channel.

    WorkBuddy retains its realm/model filtering in ``workbuddy_candidates``;
    Doubao and ChatGPT use the same durable account/runtime state and lease
    ordering but do not expose WorkBuddy's realm-specific model metadata.
    """

    channel = str(channel or "").strip()
    if not channel:
        return False, []
    now = int(time.time())
    with database(db_path or get_settings().db_path) as conn:
        snapshot_exists = (
            conn.execute("SELECT 1 FROM accounts WHERE channel = ? LIMIT 1", (channel,)).fetchone()
            is not None
        )
        credential_filter = (
            "AND EXISTS (SELECT 1 FROM credentials c "
            "WHERE c.channel = a.channel AND c.account_id = a.native_id)"
            if require_credentials
            else ""
        )
        rows = conn.execute(
            f"""SELECT a.id, a.native_id, a.ext
            FROM accounts a LEFT JOIN account_runtime_state r ON r.account_id = a.id
            WHERE a.channel = ? AND a.enabled = 1
                AND COALESCE(a.status_override, a.status) IN (
                    'ready', 'busy', 'cooldown', 'limited'
                )
                {credential_filter}
                AND (r.cooldown_until IS NULL OR r.cooldown_until <= ?)
                AND (r.breaker_until IS NULL OR r.breaker_until <= ?)
            ORDER BY COALESCE(r.updated_at, 0), a.priority DESC, a.name, a.id""",
            (channel, now, now),
        ).fetchall()

    ranked: list[tuple[int, int, AccountCandidate]] = []
    with _lease_lock:
        active_ids = {str(row["id"]) for row in rows}
        for account_id in _last_selected.keys() - active_ids:
            _last_selected.pop(account_id, None)
        for order, row in enumerate(rows):
            account_id = str(row["id"])
            if _inflight.get(account_id, 0) >= WORKBUDDY_LOCAL_INFLIGHT_LIMIT:
                continue
            ranked.append(
                (
                    _last_selected.get(account_id, 0),
                    order,
                    AccountCandidate(account_id, str(row["native_id"]), channel),
                )
            )
        ranked.sort(key=lambda item: (item[0], item[1]))
    return snapshot_exists, [item[2] for item in ranked[: max(1, int(max_candidates))]]


def _model_is_limited(ext_json: str, model: str, now: int) -> bool:
    try:
        ext = json.loads(ext_json)
    except (TypeError, json.JSONDecodeError):
        return False
    rows = ext.get("rate_limited_models") if isinstance(ext, dict) else None
    if not isinstance(rows, list):
        return False
    for row in rows:
        if not isinstance(row, dict) or row.get("model") != model:
            continue
        until = row.get("until") or row.get("reset_at")
        if not isinstance(until, str):
            return True
        try:
            deadline = datetime.fromisoformat(until.replace("Z", "+00:00")).timestamp()
        except (TypeError, ValueError, OverflowError):
            return True
        if deadline > now:
            return True
    return False


def acquire_workbuddy_lease(candidate: WorkBuddyCandidate) -> WorkBuddyLease | None:
    return acquire_account_lease(candidate)


def acquire_account_lease(candidate: AccountCandidate) -> WorkBuddyLease | None:
    global _selection_seq
    with _lease_lock:
        inflight = _inflight.get(candidate.account_id, 0)
        if inflight >= WORKBUDDY_LOCAL_INFLIGHT_LIMIT:
            return None
        _inflight[candidate.account_id] = inflight + 1
        _selection_seq += 1
        _last_selected[candidate.account_id] = _selection_seq
    return WorkBuddyLease(candidate.account_id, candidate.native_id)
